// Package auth is the authentication slice of implementation package 2:
// opaque application-credential generation, header parsing, the data-plane
// digest lookup, admin-token authentication, and the transaction-local
// application context helpers that own every database transaction the
// service runs.
//
// The package is the only place a raw credential is produced (at creation,
// exactly once) or compared in Go (the admin token, constant-time). Data-
// plane credential comparison is a keyed row fetch on the SHA-256 digest in
// the database; the raw secret is never compared in Go and never logged.
package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"vector-service/internal/dbctx"
)

// CredentialPrefix is the stable textual shape of every data-plane
// credential: the fixed "vsvc_" prefix followed by a base-encoded random
// payload of at least 256 bits.
const CredentialPrefix = "vsvc_"

// credentialEntropyBytes is the raw payload length in bytes (256 bits) of a
// generated credential. The payload is pure crypto/rand output: no
// timestamps or counters.
const credentialEntropyBytes = 32

// CredentialDigestLen is the length in bytes of the SHA-256 digest that is
// persisted in vector_control.application_credentials.credential_hash.
const CredentialDigestLen = 32

// ErrInvalidHeader is the parse error returned by ParseBearer for a missing
// or malformed Authorization header. It carries no input.
var ErrInvalidHeader = errors.New("auth: missing or malformed Authorization header")

// GenerateCredential returns a new opaque application credential: the
// "vsvc_" prefix plus 32 random bytes (256 bits) from crypto/rand,
// base64url-encoded without padding. The value is produced exactly once —
// the caller returns it at creation and persists only its digest (Digest).
func GenerateCredential() (string, error) {
	buf := make([]byte, credentialEntropyBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("auth: generate credential: %w", err)
	}
	return CredentialPrefix + base64.RawURLEncoding.EncodeToString(buf), nil
}

// Digest returns the SHA-256 digest of the raw credential. The 32 raw bytes
// are the only form of the credential ever persisted.
func Digest(credential string) [CredentialDigestLen]byte {
	return sha256.Sum256([]byte(credential))
}

// ParseBearer parses the Authorization header into its bearer credential.
// The grammar is exactly "Bearer <credential>": one header value, the
// "Bearer" scheme, exactly one ASCII space, and a non-empty credential
// containing no spaces. Missing header, wrong scheme, or empty token yield
// ErrInvalidHeader; the credential value never appears in the error.
func ParseBearer(headerValue string) (string, error) {
	const scheme = "Bearer"
	if headerValue == "" {
		return "", ErrInvalidHeader
	}
	schemePart, rest, found := strings.Cut(headerValue, " ")
	if !found || schemePart != scheme {
		return "", ErrInvalidHeader
	}
	if rest == "" || strings.Contains(rest, " ") {
		return "", ErrInvalidHeader
	}
	return rest, nil
}

// AuthResult is a successful data-plane authentication: the resolved
// application identity and the credential row it came from.
type AuthResult struct {
	// ApplicationID is the authenticated application's UUID. It is the only
	// source of application identity for data-plane handlers (overview
	// invariant 1).
	ApplicationID string

	// CredentialID is the credential row that authenticated. It exists only
	// so the best-effort last_used_at update can target the row in its own
	// short transaction; it is not part of the caller-facing identity.
	CredentialID string

	// LastUsedAt is the stored usage stamp read by the lookup (nil when the
	// row has never been stamped), for the throttle decision.
	LastUsedAt *time.Time
}

// LastUsedThrottleWindow bounds how often a credential row's last_used_at
// is rewritten: a stored value newer than the window is left untouched.
const LastUsedThrottleWindow = 60 * time.Second

// Lookup is the data-plane credential lookup: one keyed digest fetch in a
// short transaction. It fetches the credential row by credential_hash,
// joins vector_control.applications, and in the single predicate requires
// the credential enabled, the credential not expired
// (expires_at IS NULL OR expires_at > now()), and the application enabled.
// now() is evaluated server-side.
//
// Every failure shape — unknown digest, disabled credential, expired
// credential, disabled application — yields the single typed error
// ErrUnauthorized (uniform 401 unauthorized); nothing distinguishes which
// kind failed, and the error carries no token, digest, or identity. A
// database-infrastructure failure yields the helper outcome of the shared
// cause rule (dbctx): unavailable while the context is active, or the
// settled outcome when it is not.
//
// On success the function runs the throttled last_used_at update (if due)
// in its own short transaction, before returning.
func Lookup(ctx context.Context, pool *pgxpool.Pool, credential string) (AuthResult, error) {
	digest := Digest(credential)

	var result AuthResult
	if err := dbctx.WithShortTx(ctx, pool, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, lookupSQL, digest[:]).
			Scan(&result.ApplicationID, &result.CredentialID, &result.LastUsedAt)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				// All four failure shapes are indistinguishable to the
				// caller.
				return ErrUnauthorized
			}
			return err
		}
		return nil
	}); err != nil {
		// A uniform 401 (any failure shape) passes through unchanged.
		// Any other error is a database-infrastructure failure the
		// helpers have already settled by the shared cause rule (or the
		// abort sentinel); both pass through unchanged — never rendered
		// to a caller as anything other than the catalog outcome.
		return AuthResult{}, err
	}

	// Best-effort throttled usage stamp: failure never affects the request
	// (overview: warn-logged only, never fails, delays, or changes the
	// outcome).
	touchLastUsed(ctx, pool, result)
	return result, nil
}

// touchLastUsed runs the throttled last_used_at update in its own short
// transaction when the stored value is older than LastUsedThrottleWindow
// (a NULL stored value is always updated). Any failure — including the
// helper outcomes — is warn-logged with the stable safe allowlist fields
// only (event, operation, request ID when present, and the server-authored
// failure class) and dropped: never the failure's raw text, which is
// discarded at the log boundary.
func touchLastUsed(ctx context.Context, pool *pgxpool.Pool, result AuthResult) {
	if result.LastUsedAt != nil && time.Since(*result.LastUsedAt) < LastUsedThrottleWindow {
		return
	}
	if err := dbctx.WithShortTx(ctx, pool, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, lastUsedSQL, result.CredentialID)
		return err
	}); err != nil {
		logLastUsedFailure(ctx, err)
	}
}

// touchFailureClass reduces a touchLastUsed failure to its server-authored
// class. The result is one of the allowlisted class names: the class of a
// canceled or deadline context is "cancelled"; every other failure (including
// any raw driver error) is the class "unspecified". The raw error is never
// returned, rendered, or inspected for its text.
func touchFailureClass(err error) string {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return "cancelled"
	}
	return "unspecified"
}

// touchCauseAllowlist is the closed set of server-authored failure classes
// the touchLastUsed record may carry (mirroring the diagnostic-record safe
// allowlist). It is the single source of truth for the class text:
// logLastUsedFailure reads it. Adding an entry is a reviewed decision.
var touchCauseAllowlist = map[string]int{
	"cancelled":   9,  // len("cancelled")
	"unspecified": 11, // len("unspecified")
}

// logLastUsedFailure writes the single structured log record for a
// last_used_at update failure (slog.Warn). The record carries only the safe
// allowlist fields (event, operation, request ID when the context carries
// one, and the bounded server-authored failure class) — never the
// failure's raw text: not Error(), not any pgconn.PgError message, detail,
// hint, or caller line, and no credential material of any form.
func logLastUsedFailure(ctx context.Context, err error) {
	cause := touchFailureClass(err)
	if bound, ok := touchCauseAllowlist[cause]; !ok || len(cause) > bound {
		if !ok {
			// Not on the allowlist: dropped, not truncated.
			cause = ""
		} else {
			cause = cause[:bound]
		}
	}
	var attrs []any
	attrs = append(attrs,
		slog.String("event", "last_used_update_failure"),
		slog.String("operation", "authenticate"),
	)
	if rid := dbctx.RequestID(ctx); rid != "" {
		attrs = append(attrs, slog.String("request_id", rid))
	}
	if cause != "" {
		attrs = append(attrs, slog.String("failure_class", cause))
	}
	slog.Warn("last_used_at update failed; continuing", attrs...)
}

// lookupSQL is the normative one-keyed-fetch predicate of the data-plane
// lookup (package 2, section 2): digest equality AND credential enabled
// AND not expired (NULL expiry is non-expiring) AND application enabled.
// Only the digest is bound from caller input; now() is server-side.
const lookupSQL = `
SELECT a.id, c.id, c.last_used_at
FROM vector_control.application_credentials c
JOIN vector_control.applications a ON a.id = c.application_id
WHERE c.credential_hash = $1
  AND c.enabled
  AND (c.expires_at IS NULL OR c.expires_at > now())
  AND a.enabled`

// lastUsedSQL is the throttled usage stamp.
const lastUsedSQL = `
UPDATE vector_control.application_credentials
SET last_used_at = now()
WHERE id = $1`

// AdminPlane returns the admin-plane authentication check: a constant-time
// comparison of the presented bearer credential against the configured
// admin token. It performs no database access. Missing, malformed, or
// wrong credentials all yield the single typed error ErrUnauthorized; the
// error carries no credential material. The configured token must already
// be non-empty (config.Load rejects an empty or short token at startup),
// so an empty presented credential can never match.
func AdminPlane(adminToken, credential string) error {
	want := []byte(adminToken)
	got := []byte(credential)
	// crypto/subtle.ConstantTimeCompare compares both values to completion
	// (for differing lengths it still consumes the longer value's bytes
	// before reporting the mismatch), so a short prefix cannot
	// short-circuit the comparison early.
	if subtle.ConstantTimeCompare(want, got) != 1 {
		return ErrUnauthorized
	}
	return nil
}
