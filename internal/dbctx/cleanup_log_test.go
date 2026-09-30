package dbctx

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

// The cleanup-failure redaction matrix (F-7): a ROLLBACK (or other cleanup)
// failure is warn-logged with the stable safe allowlist fields only — never
// the failure's raw text. These tests capture the slog record the helper
// actually emits and assert, for every cleanup-failure shape (a raw
// context-cancel error, a pgconn.PgError with its full primary/detail/hint/
// caller text, a generic error, and a tx-closed error), that no raw error
// text, message, detail, hint, or caller line reaches the record, and that
// only the allowlisted fields (event, operation, request_id, and the
// bounded server-authored cause class) are present.

// logCapturingHandler is a slog handler that records every record's message
// and fields so the cleanup-failure redaction policy can be asserted against
// the record the helper actually emits.
type logCapturingHandler struct {
	mu    sync.Mutex
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
	h.mu.Lock()
	defer h.mu.Unlock()
	h.msgs = append(h.msgs, rec.Message)
	h.attrs = append(h.attrs, attrs)
	return nil
}

// allFieldValues returns every string value across all captured records.
func (h *logCapturingHandler) allFieldValues() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []string
	for _, a := range h.attrs {
		for _, at := range a {
			out = append(out, at.Value.String())
		}
	}
	return out
}

func (h *logCapturingHandler) messageValues() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string{}, h.msgs...)
}

func (h *logCapturingHandler) count() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.msgs)
}

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

// pgxRollbackErr builds a *pgconn.PgError with the full raw text a real
// cleanup failure might carry: primary message, Detail, Hint, and a caller
// position. Every field is forbidden to reach the log record.
func pgxRollbackErr() *pgconn.PgError {
	return &pgconn.PgError{
		Severity: "FATAL",
		Code:     "57P01",
		Message:  "terminating connection due to administrator command",
		Detail:   "the connection was closed while in a transaction",
		Hint:     "the rollback was interrupted by a restart",
		Where:    "while executing a ROLLBACK",
	}
}

// assertNoRawText asserts that none of the forbidden substrings appears in
// any captured record's message or field value. The forbidden substrings are
// every piece of raw text the cleanup failure could carry: its Error()
// rendering, its primary message, its Detail, its Hint, and its Where
// (caller) line.
func assertNoRawText(t *testing.T, h *logCapturingHandler, forbidden ...string) {
	t.Helper()
	blobs := append(h.messageValues(), h.allFieldValues()...)
	for _, b := range blobs {
		for _, f := range forbidden {
			if strings.Contains(b, f) {
				t.Errorf("log record carries raw text %q (blob %q): the cleanup-failure record must carry only the safe allowlist fields, never raw error text", f, b)
			}
		}
	}
}

// TestRunTxRollbackFailure_RedactsRawErrorText is the cleanup-failure
// redaction matrix: a ROLLBACK failure is warn-logged once, and the record
// carries no raw rollback/cleanup text. The forbidden set is the failure's
// Error() text and every raw subfield it embeds; the allowed set is the
// stable allowlist (event, operation, request_id, cause).
func TestRunTxRollbackFailure_RedactsRawErrorText(t *testing.T) {
	t.Run("raw pgx error with primary, detail, hint, and caller", func(t *testing.T) {
		h := withLogCapture(t)
		rbErr := pgxRollbackErr()
		forbidden := []string{
			rbErr.Error(),
			rbErr.Message,
			rbErr.Detail,
			rbErr.Hint,
			rbErr.Where,
		}

		// Business error + failed cleanup: the original stands; the cleanup
		// failure is attached for diagnostics and logged once.
		f := &fakeFactory{}
		f.txs = append(f.txs, &fakeTx{
			fn:          func() error { return errBusiness },
			rollbackErr: rbErr,
		})
		runCase(t, "business error + pgx rollback", true, "0199f31e-2000-7000-8000-0000000000a1", f, context.Background(),
			wantOutcome{isBusiness: errBusiness, hasRollback: true, rollbackIs: rbErr, wantRelease: 1})

		if h.count() == 0 {
			t.Fatal("no cleanup-failure log record was emitted")
		}
		assertNoRawText(t, h, forbidden...)
	})

	t.Run("canceled context (context error as the rollback cause)", func(t *testing.T) {
		h := withLogCapture(t)
		rbErr := context.Canceled
		forbidden := []string{
			rbErr.Error(), // "context canceled"
		}

		f := &fakeFactory{}
		f.txs = append(f.txs, &fakeTx{
			fn:          func() error { return errBusiness },
			rollbackErr: rbErr,
		})
		runCase(t, "business error + canceled rollback", false, "", f, context.Background(),
			wantOutcome{isBusiness: errBusiness, hasRollback: true, rollbackIs: rbErr, wantRelease: 1})

		if h.count() == 0 {
			t.Fatal("no cleanup-failure log record was emitted")
		}
		assertNoRawText(t, h, forbidden...)
	})

	t.Run("generic error and tx-closed error", func(t *testing.T) {
		for _, tc := range []struct {
			name  string
			rbErr error
		}{
			{"generic error", errors.New("rollback failed: FATAL: password required")},
			{"tx closed error", errors.New("tx is closed")},
		} {
			t.Run(tc.name, func(t *testing.T) {
				h := withLogCapture(t)
				forbidden := []string{tc.rbErr.Error()}

				f := &fakeFactory{}
				f.txs = append(f.txs, &fakeTx{
					fn:          func() error { return errBusiness },
					rollbackErr: tc.rbErr,
				})
				runCase(t, tc.name, false, "", f, context.Background(),
					wantOutcome{isBusiness: errBusiness, hasRollback: true, rollbackIs: tc.rbErr, wantRelease: 1})

				if h.count() == 0 {
					t.Fatal("no cleanup-failure log record was emitted")
				}
				assertNoRawText(t, h, forbidden...)
			})
		}
	})
}

// TestBuildCleanupCauseRecord_AllowlistFields pins the exact field set the
// cleanup-failure record carries: the stable allowlist (operation, request_id
// when present, and the bounded server-authored cause class) and nothing
// else. A raw rollback error is never among the fields.
func TestBuildCleanupCauseRecord_AllowlistFields(t *testing.T) {
	// No request ID on the context: the record carries the operation and the
	// cause class only.
	operation, requestID, cause := buildCleanupCauseRecord(true, context.Background())
	if operation != "with_app_context" {
		t.Fatalf("operation = %q, want with_app_context", operation)
	}
	if requestID != "" {
		t.Fatalf("request_id = %q, want empty (no request ID on the context)", requestID)
	}
	if cause != "unspecified" {
		t.Fatalf("cause = %q, want unspecified (no allowlisted class for a markerless, non-canceled context)", cause)
	}

	// A canceled context resolves to the allowlisted "cancelled" class.
	_, _, causeC := buildCleanupCauseRecord(false, canceledCtx())
	if causeC != "cancelled" {
		t.Fatalf("cause = %q, want cancelled for a canceled context", causeC)
	}

	// The request ID is carried through when the context has one.
	ctx := WithRequestID(context.Background(), "req-42")
	_, rid, _ := buildCleanupCauseRecord(false, ctx)
	if rid != "req-42" {
		t.Fatalf("request_id = %q, want req-42", rid)
	}
}

// canceledCtx returns a canceled context (context.Err() == Canceled).
func canceledCtx() context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return ctx
}

// TestCleanupCauseAllowlist_TruncationAndDrop exercises the allowlist
// mechanism via the RegisterCleanupCause seam: an allowlisted over-long
// class value is truncated to its bound, and a cause class not on the
// allowlist is dropped (not truncated).
func TestCleanupCauseAllowlist_TruncationAndDrop(t *testing.T) {
	t.Run("allowlisted over-long value is truncated", func(t *testing.T) {
		unreg := RegisterCleanupCause("cancelled", 4)
		defer unreg()
		// The "cancelled" class is 9 bytes; the bound is 4, so it must
		// truncate to 4 bytes ("canc").
		_, _, cause := buildCleanupCauseRecord(true, canceledCtx())
		if cause != "canc" {
			t.Fatalf("cause = %q, want the truncated value \"canc\"", cause)
		}
	})
	t.Run("non-allowlisted cause class is dropped", func(t *testing.T) {
		// Temporarily remove "unspecified" so a non-canceled context's
		// cause class is not on the allowlist and must be dropped.
		delete(cleanupCauseAllowlist, "unspecified")
		defer func() {
			cleanupCauseAllowlist["unspecified"] = cleanupCauseEntry{name: "unspecified", bound: 11}
		}()
		_, _, cause := buildCleanupCauseRecord(true, context.Background())
		if cause != "" {
			t.Fatalf("cause = %q, want empty (a non-allowlisted class is dropped, not truncated)", cause)
		}
	})
}
