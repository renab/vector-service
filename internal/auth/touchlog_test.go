package auth

import (
	"context"
	"encoding/hex"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"vector-service/internal/dbctx"
)

// The touchLastUsed redaction matrix (regression): the best-effort
// last_used_at update failure is warn-logged with the stable safe allowlist
// fields only (event, operation, request ID when present, and the bounded
// server-authored failure class). Never — in whole or in part — the
// failure's raw text: not its Error() rendering, not a pgconn.PgError's
// primary message, Detail, Hint, or caller line, and no credential material
// of any form (raw credential, credential payload, or digest). The tests
// below capture the record the code path actually emits and assert the
// forbidden substrings appear in none of its message or field values, and
// that only the allowlisted fields are present.

// logCapturingHandler is a slog handler that records every record's message
// and fields so the touch-failure redaction policy can be asserted against
// the record the code path actually emits.
type logCapturingHandler struct {
	msgs  []string
	attrs [][]slog.Attr
}

func (h *logCapturingHandler) Enabled(_ context.Context, _ slog.Level) bool { return true }

func (h *logCapturingHandler) WithAttrs(_ []slog.Attr) slog.Handler { return h }

func (h *logCapturingHandler) WithGroup(_ string) slog.Handler { return h }

func (h *logCapturingHandler) Handle(_ context.Context, rec slog.Record) error {
	var attrs []slog.Attr
	rec.Attrs(func(a slog.Attr) bool {
		attrs = append(attrs, a)
		return true
	})
	h.msgs = append(h.msgs, rec.Message)
	h.attrs = append(h.attrs, attrs)
	return nil
}

// allBlobs returns the message and every field value across all captured
// records.
func (h *logCapturingHandler) allBlobs() []string {
	out := append([]string{}, h.msgs...)
	for _, a := range h.attrs {
		for _, at := range a {
			out = append(out, at.Value.String())
		}
	}
	return out
}

// fieldValues returns the captured records' field values only.
func (h *logCapturingHandler) fieldValues() []string {
	var out []string
	for _, a := range h.attrs {
		for _, at := range a {
			out = append(out, at.Value.String())
		}
	}
	return out
}

func (h *logCapturingHandler) count() int { return len(h.msgs) }

// withLogCapture swaps the global slog logger for the duration of the test
// and returns the capturing handler.
func withLogCapture(t *testing.T) *logCapturingHandler {
	t.Helper()
	h := &logCapturingHandler{}
	def := slog.Default()
	slog.SetDefault(slog.New(h))
	t.Cleanup(func() { slog.SetDefault(def) })
	return h
}

// assertNoForbidden asserts none of the forbidden substrings appears in any
// captured record's message or field value.
func assertNoForbidden(t *testing.T, h *logCapturingHandler, forbidden ...string) {
	t.Helper()
	for _, blob := range h.allBlobs() {
		for _, f := range forbidden {
			if strings.Contains(blob, f) {
				t.Errorf("log record carries raw text %q (blob %q): the touch-failure record must carry only the safe allowlist fields, never raw error text or credential material", f, blob)
			}
		}
	}
}

// assertOnlyAllowlistFields asserts the captured record's field names are
// exactly the stable allowlist (event, operation, request ID when the
// context carries one, and failure_class when a class resolved).
func assertOnlyAllowlistFields(t *testing.T, h *logCapturingHandler, wantRequestID bool) {
	t.Helper()
	for _, a := range h.attrs {
		for _, at := range a {
			switch at.Key {
			case "event", "operation", "failure_class":
			case "request_id":
				if !wantRequestID {
					t.Errorf("record carries request_id without one on the context")
				}
			default:
				t.Errorf("record carries non-allowlisted field %q", at.Key)
			}
		}
	}
	if h.count() == 0 {
		t.Fatal("no touch-failure log record was emitted")
	}
}

// touchPgErr builds a *pgconn.PgError with the full raw text a real
// last_used_at update failure might carry: primary message, Detail, Hint,
// and a caller position. Every field is forbidden to reach the log record.
func touchPgErr() *pgconn.PgError {
	return &pgconn.PgError{
		Severity: "ERROR",
		Code:     "42501",
		Message:  "permission denied for table application_credentials",
		Detail:   "UPDATE on vector_control.application_credentials was refused",
		Hint:     "the runtime role lacks UPDATE on the credentials table",
		Where:    "while executing the last_used_at stamp UPDATE",
	}
}

// touchCredentialFixture builds the credential-material fixture: a
// recognizable raw credential, its payload, and its digest rendering. Every
// form is forbidden to reach the log record.
func touchCredentialFixture() (raw, payload, digestHex string) {
	payload = "SECRET-TOPSECRET-0123456789abcdef0123456789ABCDEF01"
	raw = CredentialPrefix + payload
	var d [CredentialDigestLen]byte
	copy(d[:], []byte("digest-bytes-0123456789abcdef0123456789ABCD"))
	digestHex = hex.EncodeToString(d[:])
	return raw, payload, digestHex
}

// TestTouchLastUsed_RedactsRawErrorAndCredential is the regression matrix:
// for every failure shape (a raw pgconn.PgError with its full text, a raw
// database error text, a canceled-context error, and a generic error) the
// emitted record carries the stable allowlist fields only, and no raw error
// text, message, detail, hint, caller line, raw credential, credential
// payload, or digest appears in it.
func TestTouchLastUsed_RedactsRawErrorAndCredential(t *testing.T) {
	raw, payload, digestHex := touchCredentialFixture()

	t.Run("raw pgx error with primary, detail, hint, and caller", func(t *testing.T) {
		h := withLogCapture(t)
		pgErr := touchPgErr()
		forbidden := []string{
			pgErr.Error(),
			pgErr.Message,
			pgErr.Detail,
			pgErr.Hint,
			pgErr.Where,
			raw, payload, digestHex,
		}
		logLastUsedFailure(context.Background(), pgErr)
		assertNoForbidden(t, h, forbidden...)
		assertOnlyAllowlistFields(t, h, false)
	})

	t.Run("raw database error text", func(t *testing.T) {
		h := withLogCapture(t)
		rawErr := errors.New("connection to server at \"10.0.0.5\", port 5432 failed: connection refused")
		forbidden := []string{
			rawErr.Error(),
			"10.0.0.5",
			raw, payload, digestHex,
		}
		logLastUsedFailure(context.Background(), rawErr)
		assertNoForbidden(t, h, forbidden...)
		assertOnlyAllowlistFields(t, h, false)
	})

	t.Run("canceled context", func(t *testing.T) {
		h := withLogCapture(t)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		logLastUsedFailure(ctx, context.Canceled)
		assertNoForbidden(t, h, raw, payload, digestHex)
		// The failure class must be the allowlisted "cancelled" token.
		found := false
		for _, v := range h.fieldValues() {
			if v == "cancelled" {
				found = true
			}
		}
		if !found {
			t.Errorf("failure_class = %v, want the allowlisted class \"cancelled\"", h.fieldValues())
		}
		// The raw class text must not appear in the message itself.
		for _, m := range h.msgs {
			if strings.Contains(m, "cancelled") {
				t.Fatalf("record message %q carries the raw class text; it belongs only in the allowlisted failure_class field", m)
			}
		}
		assertOnlyAllowlistFields(t, h, false)
	})

	t.Run("generic error", func(t *testing.T) {
		h := withLogCapture(t)
		genericErr := errors.New("rollback failed: FATAL: password required")
		forbidden := []string{
			genericErr.Error(),
			"password required",
			raw, payload, digestHex,
		}
		logLastUsedFailure(context.Background(), genericErr)
		assertNoForbidden(t, h, forbidden...)
		assertOnlyAllowlistFields(t, h, false)
	})
}

// TestTouchLastUsed_FailureClassResolution pins the allowlisted class each
// failure shape resolves to: a canceled or deadline context resolves to
// "cancelled"; every other failure (raw driver error, generic error,
// unavailable outcome) resolves to "unspecified". The class is a bounded,
// server-authored token carrying no caller data.
func TestTouchLastUsed_FailureClassResolution(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"canceled context", context.Canceled, "cancelled"},
		{"deadline exceeded", context.DeadlineExceeded, "cancelled"},
		{"raw pgx error", touchPgErr(), "unspecified"},
		{"generic error", errors.New("some raw driver text"), "unspecified"},
		{"unavailable outcome", NewUnavailable(touchPgErr()), "unspecified"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := touchFailureClass(tc.err); got != tc.want {
				t.Fatalf("touchFailureClass = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestTouchLastUsed_AllowlistFields pins the exact field set the
// touch-failure record carries: event, operation, request ID when the
// context carries one, and the bounded server-authored failure class. No raw
// error field, no credential field, nothing else.
func TestTouchLastUsed_AllowlistFields(t *testing.T) {
	t.Run("no request ID on the context", func(t *testing.T) {
		h := withLogCapture(t)
		logLastUsedFailure(context.Background(), errors.New("x"))
		assertOnlyAllowlistFields(t, h, false)
		if got := len(h.attrs); got != 1 || len(h.attrs[0]) != 3 {
			t.Fatalf("record carries %d fields, want exactly event, operation, and failure_class", got)
		}
	})

	t.Run("request ID is carried through when present", func(t *testing.T) {
		h := withLogCapture(t)
		ctx := dbctxWithRequestID(context.Background(), "req-42")
		logLastUsedFailure(ctx, errors.New("x"))
		assertOnlyAllowlistFields(t, h, true)
		found := false
		for _, v := range h.fieldValues() {
			if v == "req-42" {
				found = true
			}
		}
		if !found {
			t.Fatalf("record does not carry the request ID: %v", h.fieldValues())
		}
	})
}

// dbctxWithRequestID returns a context carrying the request identifier (the
// stable operational log field).
func dbctxWithRequestID(ctx context.Context, id string) context.Context {
	return dbctx.WithRequestID(ctx, id)
}

// TestTouchLastUsed_NonAllowlistedClassIsDropped exercises the allowlist
// mechanism directly: a failure class not on the allowlist is dropped
// (not truncated) from the record.
func TestTouchLastUsed_NonAllowlistedClassIsDropped(t *testing.T) {
	h := withLogCapture(t)
	delete(touchCauseAllowlist, "unspecified")
	defer func() { touchCauseAllowlist["unspecified"] = 11 }()
	logLastUsedFailure(context.Background(), errors.New("x"))
	if h.count() == 0 {
		t.Fatal("no touch-failure log record was emitted")
	}
	for _, a := range h.attrs {
		for _, at := range a {
			if at.Key == "failure_class" {
				t.Fatalf("record carries a non-allowlisted failure_class value %q; it must be dropped, not truncated", at.Value.String())
			}
		}
	}
}
