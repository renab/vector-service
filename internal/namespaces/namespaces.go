// Package namespaces is the namespace-resolution slice of implementation
// package 2: the namespace-key grammar and size validation, the
// application-scoped key lookup, and the result/context APIs the package-3
// operations and package-4 logging consume.
//
// The canonical path is the composed Resolve: it runs the
// dbctx.WithAppContext transaction for the authenticated application,
// resolves the namespace key inside it (after the transaction's first
// statement, the transaction-local application context), and returns the
// resolved context. Package 2's ordering note is settled: authenticate →
// WithAppContext (BEGIN + set_config) → resolve namespace → business SQL.
//
// The application identity is never accepted from the caller: it comes from
// the authenticated credential, carried on the context by dbctx.WithAppID.
// No code path accepts an arbitrary caller-supplied application ID
// (invariant 1).
//
// A missing key, a disabled namespace, or a foreign namespace (impossible
// to read under the forced RLS policy on vector_control.namespaces, but the
// predicate includes application_id regardless) all yield the single typed
// error ErrNotFound (the uniform 404 namespace_not_found): the API
// deliberately does not reveal whether a namespace exists under another
// application (docs/API.md, Namespace Isolation).
package namespaces

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"vector-service/internal/dbctx"
)

// CodeNotFound is the uniform namespace-resolution failure code (404):
// missing, disabled, or foreign namespace, indistinguishable to the caller.
const CodeNotFound = "namespace_not_found"

// MessageNotFound is the uniform namespace-resolution failure message.
const MessageNotFound = "namespace not found"

// MaxKeyLength is the maximum accepted length of a namespace key, matching
// the schema's key grammar (^[a-z][a-z0-9_-]{0,63}$: one leading
// lowercase letter plus at most 63 further characters).
const MaxKeyLength = 64

// KeyGrammar renders the accepted namespace-key grammar for diagnostic
// messages.
const KeyGrammar = "^[a-z][a-z0-9_-]{0,63}$"

// keyPattern is the namespace-key grammar, the same shape as the schema's
// application_key constraint (migrations 0001). Keys are lowercase
// letters, digits, dashes, and underscores, starting with a lowercase
// letter, at most 64 characters.
var keyPattern = regexp.MustCompile(KeyGrammar)

// ValidateKey checks a namespace key against the grammar and length bound.
// An empty key, a key above MaxKeyLength, or a key containing a character
// outside [a-z0-9_-] (or not starting with [a-z]) yields a typed error.
// The key is never logged on failure.
func ValidateKey(key string) error {
	if key == "" {
		return fmt.Errorf("namespaces: %w: key is empty", ErrInvalidKey)
	}
	if len(key) > MaxKeyLength {
		return fmt.Errorf("namespaces: %w: key length %d exceeds %d", ErrInvalidKey, len(key), MaxKeyLength)
	}
	if !keyPattern.MatchString(key) {
		return fmt.Errorf("namespaces: %w: key does not match %s", ErrInvalidKey, KeyGrammar)
	}
	return nil
}

// ErrInvalidKey is the typed namespace-key validation failure. The key
// itself is not part of the error value; the message describes the shape of
// the failure without echoing the input.
var ErrInvalidKey = errors.New("invalid namespace key")

// ErrNotFound is the typed, uniform namespace-resolution failure (404
// namespace_not_found): no row, a disabled row, or a foreign row (which
// cannot be read under the forced RLS policy anyway). It carries no
// namespace identity and distinguishes no failure kind: the response never
// reveals whether a namespace exists under another application.
type notFoundError struct{}

func (notFoundError) Error() string { return MessageNotFound }

// ErrNotFound is the single typed error for every namespace-resolution
// failure: missing, disabled, or foreign namespace.
var ErrNotFound = notFoundError{}

// ErrInvalidApp is the typed failure for a missing or malformed
// application identity in the context. It is a programming-error outcome:
// every data-plane request carries the authenticated identity, so a
// resolution without one means the caller skipped authentication.
var ErrInvalidApp = errors.New("invalid application identity")

// Result is a successful namespace resolution: the resolved namespace
// identity the operation (package 3) binds as a parameter and the logging
// state (package 4) records.
type Result struct {
	// NamespaceID is the resolved namespace's UUID.
	NamespaceID string

	// Key is the resolved namespace's key — the same key the request
	// presented, validated and matched against the row.
	Key string
}

// Resolve is the canonical namespace resolution: it runs the
// dbctx.WithAppContext transaction for the application carried on the
// context (dbctx.AppID — never a caller-supplied ID), and inside it
// resolves the key against vector_control.namespaces, requiring the row
// enabled. It returns the resolved context (carrying the result, for
// package-3 operations and package-4 logging) and the result.
//
// Failure semantics:
//
//   - invalid key (grammar or size) or a missing/malformed application
//     identity → the typed programming-error outcome (ErrInvalidKey /
//     ErrInvalidApp), without touching the database;
//   - no row, a disabled row, or a foreign row → the uniform ErrNotFound
//     (404 namespace_not_found);
//   - an infrastructure failure (acquisition, BEGIN, set_config, COMMIT,
//     ROLLBACK) → the typed outcome of the dbctx helper boundary,
//     unchanged.
//
// The transaction rolls back on any failure; on success it commits empty
// (resolution is read-only) and the request continues with the resolved
// context.
func Resolve(ctx context.Context, pool *pgxpool.Pool, key string) (context.Context, Result, error) {
	appID := dbctx.AppID(ctx)
	if !isUUID(appID) {
		return nil, Result{}, fmt.Errorf("namespaces: %w: no valid application identity in context", ErrInvalidApp)
	}
	if err := ValidateKey(key); err != nil {
		return nil, Result{}, err
	}
	var r Result
	err := dbctx.WithAppContext(ctx, pool, appID, func(tx pgx.Tx) error {
		resolved, err := ResolveInTx(ctx, tx, key)
		if err != nil {
			return err
		}
		r = resolved
		return nil
	})
	if err != nil {
		return nil, Result{}, err
	}
	return WithContext(ctx, r), r, nil
}

// ResolveInTx is the in-transaction lookup primitive: it resolves the
// namespace key against the application carried on the context inside an
// already-open application-scoped transaction.
//
// It is exported so the package-3 operations can compose namespace
// resolution, vector-space resolution, and their business SQL inside ONE
// dbctx.WithAppContext transaction (package 3, section 3: "all inside one
// WithAppContext transaction (namespace resolution first, then space
// resolution and its state gate)") instead of the composed Resolve's
// standalone read-only transaction. It is the same primitive Resolve calls
// inside its own transaction.
//
// Requirements (a caller that cannot satisfy them has a programming error,
// not a recoverable failure):
//
//   - the context carries the authenticated application identity
//     (dbctx.WithAppID); the identity is never caller-supplied
//     (invariant 1);
//   - the key passes ValidateKey (the handler validates it as request
//     material; an invalid key is rejected before any database work);
//   - the transaction was begun by dbctx.WithAppContext for that same
//     application, so the transaction-local application context (the
//     helper's first statement) is already established (invariant 3: no
//     business SQL as vector_api without an app-context transaction).
//
// No row, a disabled row, or a foreign row all return the uniform
// ErrNotFound. The predicate includes application_id regardless of RLS
// (defense in depth: the forced RLS policy enforces the same scope
// independently).
func ResolveInTx(ctx context.Context, tx pgx.Tx, key string) (Result, error) {
	appID := dbctx.AppID(ctx)
	if !isUUID(appID) {
		return Result{}, fmt.Errorf("namespaces: %w: no valid application identity in context", ErrInvalidApp)
	}
	if err := ValidateKey(key); err != nil {
		return Result{}, err
	}
	return resolveInTx(ctx, tx, appID, key)
}

// resolveInTx is the unexported core of Resolve and ResolveInTx.
func resolveInTx(ctx context.Context, tx pgx.Tx, appID, key string) (Result, error) {
	var r Result
	err := tx.QueryRow(ctx,
		`SELECT id, namespace_key
		 FROM vector_control.namespaces
		 WHERE application_id = $1
		   AND namespace_key = $2
		   AND enabled`,
		appID, key).Scan(&r.NamespaceID, &r.Key)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) || errors.Is(err, pgx.ErrNoRows) {
			return Result{}, ErrNotFound
		}
		return Result{}, err
	}
	return r, nil
}

// WithContext returns a context carrying the resolved namespace for the
// rest of the request. Package-3 operations read it (Namespace) to obtain
// the namespace UUID and key to bind as SQL parameters; package-4 logging
// reads the same value for the namespace_id and namespace log fields.
func WithContext(ctx context.Context, r Result) context.Context {
	return context.WithValue(ctx, key{}, r)
}

// Namespace returns the resolved namespace carried by the context, and
// false when none has been resolved. A data-plane operation that reaches
// business SQL without a resolved namespace is a programming error.
func Namespace(ctx context.Context) (Result, bool) {
	r, ok := ctx.Value(key{}).(Result)
	return r, ok
}

// isUUID reports whether s is a lowercase or uppercase hyphenated UUID
// (8-4-4-4-12 hexadecimal). It mirrors the form the schema and set_config
// accept; the authoritative check is the SQL cast, which runs only inside
// an app-context transaction.
func isUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i, c := range s {
		switch i {
		case 8, 13, 18, 23:
			if c != '-' {
				return false
			}
		default:
			if !isHex(c) {
				return false
			}
		}
	}
	return true
}

func isHex(c rune) bool {
	return (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
}

type key struct{}
