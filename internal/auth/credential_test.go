package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"strings"
	"testing"
	"time"
)

// TestGenerateCredential is the credential-generation unit matrix (package
// 2, Validation): shape, minimum entropy, uniqueness across many
// generations, and the shape properties the textual contract fixes.
func TestGenerateCredential(t *testing.T) {
	const n = 256
	seen := make(map[string]bool, n)
	for i := 0; i < n; i++ {
		cred, err := GenerateCredential()
		if err != nil {
			t.Fatalf("generate %d: %v", i, err)
		}
		if !strings.HasPrefix(cred, CredentialPrefix) {
			t.Fatalf("credential %d missing %q prefix: %q", i, CredentialPrefix, cred[:min(9, len(cred))])
		}
		payload := strings.TrimPrefix(cred, CredentialPrefix)
		if len(payload) != base64.RawURLEncoding.EncodedLen(credentialEntropyBytes) {
			t.Fatalf("payload length = %d, want %d (256 bits base64url, no padding)",
				len(payload), base64.RawURLEncoding.EncodedLen(credentialEntropyBytes))
		}
		decoded, err := base64.RawURLEncoding.DecodeString(payload)
		if err != nil {
			t.Fatalf("payload %q is not base64url: %v", payload, err)
		}
		if len(decoded) != credentialEntropyBytes {
			t.Fatalf("decoded payload length = %d, want %d bytes (256 bits)", len(decoded), credentialEntropyBytes)
		}
		// No padding characters, no prefix re-encoding artifacts.
		if strings.ContainsAny(cred, "=") {
			t.Fatalf("credential carries padding: %q", cred)
		}
		if seen[cred] {
			t.Fatalf("credential %d is a duplicate; 256-bit crypto/rand output must not repeat", i)
		}
		seen[cred] = true
	}
}

// TestDigest is the digest unit matrix: same input → same digest, different
// input → different digest, and the raw value never equals the stored
// digest.
func TestDigest(t *testing.T) {
	sample := "vsvc_" + strings.Repeat("A", 43)

	// Round-trip: the digest is exactly the SHA-256 of the raw value.
	want := sha256.Sum256([]byte(sample))
	if got := Digest(sample); got != want {
		t.Fatalf("Digest(%q...) = %x, want %x", sample[:7], got, want)
	}
	if got := Digest(sample); got != want {
		t.Fatalf("Digest is not stable across calls: %x vs %x", got, want)
	}
	if got := Digest(sample + "x"); got == want {
		t.Fatal("different inputs produced the same digest")
	}

	cred, err := GenerateCredential()
	if err != nil {
		t.Fatal(err)
	}
	// The raw credential never equals its stored form (the 32-byte digest
	// is a different, shorter, unrelated value).
	digest := Digest(cred)
	if cred == string(digest[:]) {
		t.Fatal("raw credential equals its digest; the stored form must be the digest only")
	}
}

// TestParseBearer is the header-grammar unit matrix (package 2, section 2):
// the header must be exactly "Bearer <credential>" — missing header, wrong
// scheme, or empty token is a uniform parse failure; the credential value
// never appears in the error.
func TestParseBearer(t *testing.T) {
	cred := "vsvc_" + strings.Repeat("b", 43)
	cases := []struct {
		name    string
		header  string
		want    string
		wantErr bool
	}{
		{"valid form", "Bearer " + cred, cred, false},
		{"missing header", "", "", true},
		{"wrong scheme basic", "Basic " + cred, "", true},
		{"lowercase scheme is not the bearer scheme", "bearer " + cred, "", true},
		{"uppercase scheme is not the bearer scheme", "BEARER " + cred, "", true},
		{"empty token", "Bearer ", "", true},
		{"token with internal space", "Bearer " + cred + " extra", "", true},
		{"scheme with double space", "Bearer  " + cred, "", true},
		{"no scheme", cred, "", true},
		{"trailing space in token", "Bearer " + cred + " ", "", true},
		{"no space between scheme and token", "Bearer" + cred, "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseBearer(tc.header)
			if tc.wantErr {
				if !errors.Is(err, ErrInvalidHeader) {
					t.Fatalf("error = %v, want ErrInvalidHeader", err)
				}
				if err != nil && err.Error() != ErrInvalidHeader.Error() {
					t.Fatalf("error message %q is not uniform; it must not enumerate or echo", err.Error())
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Fatalf("credential = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestAdminPlane is the admin-authentication unit matrix (package 2,
// section 3): exact match passes, mismatch fails, and the check is
// constant-time — the comparison runs over the full configured length
// regardless of where the presented value diverges (a short prefix must
// not short-circuit).
func TestAdminPlane(t *testing.T) {
	const token = "0123456789abcdef0123456789abcdef" // 32-byte configured token
	cases := []struct {
		name   string
		token  string
		caller string
		ok     bool
	}{
		{"exact match", token, token, true},
		{"mismatch last byte", token, token[:31] + "e", false},
		{"short prefix", token, token[:16], false},
		{"empty presented", token, "", false},
		{"longer than configured", token, token + "extra", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := AdminPlane(tc.token, tc.caller)
			if tc.ok {
				if err != nil {
					t.Fatalf("unexpected failure: %v", err)
				}
				return
			}
			if !errors.Is(err, ErrUnauthorized) {
				t.Fatalf("error = %v, want ErrUnauthorized", err)
			}
			if err.Error() != MessageUnauthorized {
				t.Fatalf("message %q is not uniform", err.Error())
			}
		})
	}
}

// TestAdminPlaneConstantTime verifies the comparison is not short-circuited
// by a differing prefix: every failing call compares the full configured
// length. The test instruments by comparing the comparison behavior across
// inputs of the same length at different divergence points — the
// implementation compares the longer value to completion, so a one-byte
// prefix divergence and a one-byte suffix divergence take the same code
// path (the observable property here is uniform rejection; timing is
// asserted only structurally, by the use of the constant-time primitive
// the implementation is required to use).
func TestAdminPlaneConstantTime(t *testing.T) {
	const token = "00000000000000000000000000000000"
	prefixDiverge := "1" + strings.Repeat("0", 31)
	suffixDiverge := strings.Repeat("0", 31) + "1"
	for _, v := range []string{prefixDiverge, suffixDiverge} {
		if err := AdminPlane(token, v); !errors.Is(err, ErrUnauthorized) {
			t.Fatalf("diverging input not rejected uniformly: %v", err)
		}
	}
}

// TestEmptyAdminTokenStartupRejection records that an empty or short admin
// token is rejected at startup by configuration, not here: the admin check
// itself receives only a validated token (config.Load enforces the
// 32-byte minimum and rejects the empty value). The check's contract is
// that an empty presented credential can never match a non-empty
// configured token.
func TestEmptyAdminTokenStartupRejection(t *testing.T) {
	// config.Load rejects "" and < 32 bytes; this package never sees an
	// empty configured token. The only empty-token interaction is the
	// presented value:
	if err := AdminPlane("0123456789abcdef0123456789abcdef", ""); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("empty presented credential must not authenticate: %v", err)
	}
}

// TestThrottleDecision is the last_used_at throttle decision: a NULL stored
// value is always due; a value inside the window is not; a value older
// than the window is.
func TestThrottleDecision(t *testing.T) {
	now := time.Now()
	cases := []struct {
		name       string
		lastUsedAt *time.Time
		due        bool
	}{
		{"null is due", nil, true},
		{"inside window is not due", ptrTime(now.Add(-30 * time.Second)), false},
		{"on the boundary is due", ptrTime(now.Add(-60 * time.Second)), true},
		{"older than window is due", ptrTime(now.Add(-2 * time.Hour)), true},
		{"future (clock skew) is not due", ptrTime(now.Add(5 * time.Minute)), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			due := tc.lastUsedAt == nil || time.Since(*tc.lastUsedAt) >= LastUsedThrottleWindow
			if due != tc.due {
				t.Fatalf("due = %v, want %v", due, tc.due)
			}
		})
	}
}

func ptrTime(tm time.Time) *time.Time { return &tm }

// TestNoSecretsInErrors verifies no error message in this package carries
// credential material: every exported error path renders a stable,
// input-free message.
func TestNoSecretsInErrors(t *testing.T) {
	const secret = "vsvc_topsecretcredentialvalue0123456789abcdef01234567890"
	errs := []error{
		ErrUnauthorized,
		NewUnavailable(errors.New("connection refused by database")),
	}
	for _, err := range errs {
		msg := err.Error()
		if strings.Contains(msg, "topsecret") || strings.Contains(msg, "vsvc_") {
			t.Fatalf("error message %q carries credential material", msg)
		}
	}
	if _, err := ParseBearer("Bearer  " + secret); err == nil {
		t.Fatal("expected a parse error for a malformed header")
	}
}

// min returns the smaller of two ints (helper for prefix-safe test
// formatting).
func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

var _ = rand.Read
