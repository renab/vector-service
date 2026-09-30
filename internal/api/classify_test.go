package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"vector-service/internal/apierr"
	"vector-service/internal/auth"
	"vector-service/internal/dbctx"
	"vector-service/internal/namespaces"
	"vector-service/internal/vectors"
)

// recordingHandler is a slog handler that records every record's fields so
// the diagnostic-record policy can be asserted against the record the
// classifier actually emits.
type recordingHandler struct {
	mu      sync.Mutex
	records []slog.Record
	attrs   [][]slog.Attr
}

func newRecordingHandler() *recordingHandler {
	return &recordingHandler{}
}

func (h *recordingHandler) Enabled(_ context.Context, _ slog.Level) bool { return true }

func (h *recordingHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return h // no state per group in this test
}

func (h *recordingHandler) WithGroup(_ string) slog.Handler { return h }

func (h *recordingHandler) Handle(_ context.Context, rec slog.Record) error {
	var attrs []slog.Attr
	rec.Attrs(func(a slog.Attr) bool {
		attrs = append(attrs, a)
		return true
	})
	h.mu.Lock()
	defer h.mu.Unlock()
	h.records = append(h.records, rec)
	h.attrs = append(h.attrs, attrs)
	return nil
}

// fieldValues returns the map of the i-th recorded record's attributes.
func (h *recordingHandler) fieldValues(i int) map[string]string {
	h.mu.Lock()
	defer h.mu.Unlock()
	m := map[string]string{}
	for _, a := range h.attrs[i] {
		m[a.Key] = a.Value.String()
	}
	return m
}

func (h *recordingHandler) count() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.records)
}

// withLogs swaps the global slog logger for the duration of the test.
func withLogs(t *testing.T, h *recordingHandler) {
	t.Helper()
	defaultLogger := slog.Default()
	slog.SetDefault(slog.New(h))
	t.Cleanup(func() { slog.SetDefault(defaultLogger) })
}

// pgErr builds a *pgconn.PgError with the given SQLSTATE and message.
func pgErr(code, message string) *pgconn.PgError {
	return &pgconn.PgError{Code: code, Message: message}
}

func TestClassifyNil(t *testing.T) {
	if got := Classify(context.Background(), nil); got != nil {
		t.Fatalf("Classify(nil) = %v, want nil", got)
	}
}

func TestClassifyTypedPassThrough(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want *apierr.Error
	}{
		{name: "already typed internal", err: apierr.ErrInternal, want: apierr.ErrInternal},
		{name: "already typed conflict", err: apierr.ErrConflict, want: apierr.ErrConflict},
		{name: "wrapped typed", err: fmt.Errorf("wrap: %w", apierr.ErrConflict), want: apierr.ErrConflict},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := Classify(context.Background(), tc.err)
			if got != tc.want {
				t.Fatalf("Classify() = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestClassifyServiceSentinels(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want apierr.Code
	}{
		{name: "namespace not found", err: namespaces.ErrNotFound, want: apierr.CodeNamespaceNotFound},
		{name: "namespace key invalid", err: namespaces.ErrInvalidKey, want: apierr.CodeInvalidNamespaceKey},
		{name: "namespace invalid app", err: namespaces.ErrInvalidApp, want: apierr.CodeInternal},
		{name: "auth unauthorized", err: auth.ErrUnauthorized, want: apierr.CodeUnauthorized},
		{name: "auth unavailable", err: auth.NewUnavailable(errors.New("pool down")), want: apierr.CodeUnavailable},
		{name: "dbctx unavailable", err: &dbctx.Unavailable{Cause: errors.New("begin failed")}, want: apierr.CodeUnavailable},
		{name: "invalid vector", err: vectors.ErrInvalidVector, want: apierr.CodeInvalidVector},
		{name: "non finite vector", err: vectors.ErrNonFiniteVector, want: apierr.CodeNonFiniteVector},
		{name: "invalid dimensions", err: vectors.ErrInvalidDimensions, want: apierr.CodeInvalidVectorDimensions},
		{name: "space not found", err: vectors.ErrSpaceNotFound, want: apierr.CodeVectorSpaceNotFound},
		{name: "space unavailable", err: vectors.ErrSpaceUnavailable, want: apierr.CodeVectorSpaceUnavailable},
		{name: "invalid space key", err: vectors.ErrInvalidSpaceKey, want: apierr.CodeInvalidJSON},
		{name: "missing field", err: vectors.ErrMissingField, want: apierr.CodeMissingField},
		{name: "invalid content hash", err: vectors.ErrInvalidContentHash, want: apierr.CodeInvalidContentHash},
		{name: "metadata not object", err: vectors.ErrMetadataNotObject, want: apierr.CodeMetadataNotObject},
		{name: "metadata too large", err: vectors.ErrMetadataTooLarge, want: apierr.CodeMetadataTooLarge},
		{name: "invalid timestamp", err: vectors.ErrInvalidTimestamp, want: apierr.CodeInvalidTimestamp},
		{name: "duplicate record", err: vectors.ErrDuplicateRecord, want: apierr.CodeDuplicateRecord},
		{name: "batch too large", err: vectors.ErrBatchTooLarge, want: apierr.CodeBatchTooLarge},
		{name: "invalid limit", err: vectors.ErrInvalidLimit, want: apierr.CodeInvalidLimit},
		{name: "invalid filter", err: vectors.ErrInvalidFilter, want: apierr.CodeInvalidFilter},
		{name: "invalid uuid", err: vectors.ErrInvalidUUID, want: apierr.CodeInvalidUUID},
		{name: "record not found", err: vectors.ErrRecordNotFound, want: apierr.CodeRecordNotFound},
		{name: "wrapped sentinel", err: fmt.Errorf("op: %w", vectors.ErrSpaceNotFound), want: apierr.CodeVectorSpaceNotFound},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := newRecordingHandler()
			withLogs(t, h)
			got := Classify(context.Background(), tc.err)
			if got == nil || got.Code != tc.want {
				t.Fatalf("Classify() = %+v, want code %s", got, tc.want)
			}
			if got.Status != tc.want.Status() {
				t.Fatalf("status = %d, want %d", got.Status, tc.want.Status())
			}
			// Service sentinels are not raw driver errors: no diagnostic
			// record.
			if n := h.count(); n != 0 {
				t.Fatalf("diagnostic records = %d, want 0 (no raw driver error)", n)
			}
		})
	}
}

func TestClassifySQLSTATE(t *testing.T) {
	tests := []struct {
		name     string
		state    string
		message  string
		wantCode apierr.Code
	}{
		{name: "unique violation", state: "23505", message: "duplicate key value violates unique constraint", wantCode: apierr.CodeConflict},
		{name: "check violation", state: "23514", message: "null value violates not-null constraint", wantCode: apierr.CodeInternal},
		{name: "invalid vector text", state: "22P02", message: "malformed array literal", wantCode: apierr.CodeInternal},
		{name: "foreign key violation", state: "23503", message: "insert or update on a child table violates a foreign key constraint", wantCode: apierr.CodeInternal},
		{name: "object in use", state: "55P03", message: "cannot drop a role because it is currently used by an active transaction", wantCode: apierr.CodeInternal},
		{name: "unknown state", state: "XX000", message: "something else", wantCode: apierr.CodeInternal},
		{name: "connection failure", state: "08006", message: "connection failure", wantCode: apierr.CodeUnavailable},
		{name: "client not able", state: "08001", message: "client not able to execute", wantCode: apierr.CodeUnavailable},
		{name: "connection does not exist", state: "08003", message: "connection does not exist", wantCode: apierr.CodeUnavailable},
		{name: "sql client unresponsive", state: "08004", message: "sql client unresponsive", wantCode: apierr.CodeUnavailable},
		{name: "system", state: "08007", message: "transaction rollback", wantCode: apierr.CodeUnavailable},
		{name: "insufficient resources", state: "58000", message: "out of memory", wantCode: apierr.CodeUnavailable},
		{name: "query canceled", state: "57014", message: "query was canceled", wantCode: apierr.CodeUnavailable},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := newRecordingHandler()
			withLogs(t, h)
			got := Classify(context.Background(), pgErr(tc.state, tc.message))
			if got == nil || got.Code != tc.wantCode {
				t.Fatalf("Classify() = %+v, want code %s", got, tc.wantCode)
			}
			// Exactly one diagnostic record for a raw driver error.
			if n := h.count(); n != 1 {
				t.Fatalf("diagnostic records = %d, want 1", n)
			}
			f := h.fieldValues(0)
			if f["code"] != string(tc.wantCode) {
				t.Errorf("record code = %q, want %q", f["code"], tc.wantCode)
			}
			if f["sqlstate"] != tc.state {
				t.Errorf("record sqlstate = %q, want %q", f["sqlstate"], tc.state)
			}
			// The raw message never appears in any field value.
			for k, v := range f {
				if v == tc.message {
					t.Errorf("record field %q equals the raw driver message", k)
				}
			}
		})
	}
}

func TestClassifyP0001Prefixes(t *testing.T) {
	tests := []struct {
		name     string
		message  string
		wantCode apierr.Code
	}{
		{
			name:     "unknown space prefix",
			message:  "unknown vector_space_id: 0199f31e-2000-7000-8000-0000000000a1",
			wantCode: apierr.CodeVectorSpaceNotFound,
		},
		{
			name:     "disabled space prefix",
			message:  "vector space is disabled: 0199f31e-2000-7000-8000-0000000000b2",
			wantCode: apierr.CodeVectorSpaceUnavailable,
		},
		{
			name:     "dimension mismatch prefix",
			message:  "embedding dimension mismatch: got 3, want 4",
			wantCode: apierr.CodeInvalidVectorDimensions,
		},
		{
			name:     "unrecognized prefix",
			message:  "something else happened",
			wantCode: apierr.CodeInternal,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := newRecordingHandler()
			withLogs(t, h)
			got := Classify(context.Background(), pgErr("P0001", tc.message))
			if got == nil || got.Code != tc.wantCode {
				t.Fatalf("Classify() = %+v, want code %s", got, tc.wantCode)
			}
			if n := h.count(); n != 1 {
				t.Fatalf("diagnostic records = %d, want 1", n)
			}
			// The P0001 trigger text never appears in any field value.
			f := h.fieldValues(0)
			for k, v := range f {
				if len(v) > 0 && (v == tc.message || len(tc.message) > 0 && containsFold(v, tc.message)) {
					t.Errorf("record field %q contains the P0001 trigger text", k)
				}
			}
		})
	}
}

// TestClassifyNonP0001PrefixGuarantee pins the SQLSTATE gate: an error whose
// message carries a trigger-like prefix but whose SQLSTATE is not P0001 must
// classify by the SQLSTATE table, not by the prefix.
func TestClassifyNonP0001PrefixGate(t *testing.T) {
	h := newRecordingHandler()
	withLogs(t, h)
	err := pgErr("23503", "unknown vector_space_id: 0199f31e-2000-7000-8000-0000000000a1")
	got := Classify(context.Background(), err)
	if got == nil || got.Code != apierr.CodeInternal {
		t.Fatalf("Classify() = %+v, want internal (SQLSTATE gates first)", got)
	}
}

func TestClassifyMaxBytesError(t *testing.T) {
	// *http.MaxBytesError is recognized by identity through errors.As: the
	// same overflow the decode helper classifies (413) is still a
	// caller-input condition when it surfaces later in the request path.
	err := &http.MaxBytesError{Limit: 8}
	got := Classify(context.Background(), err)
	if got == nil || got.Code != apierr.CodeBodyTooLarge {
		t.Fatalf("Classify() = %+v, want body_too_large", got)
	}
}

func TestClassifyUnknownError(t *testing.T) {
	h := newRecordingHandler()
	withLogs(t, h)
	got := Classify(context.Background(), errors.New("boom"))
	if got == nil || got.Code != apierr.CodeInternal {
		t.Fatalf("Classify() = %+v, want internal", got)
	}
	// A non-driver error is not a raw driver error: no diagnostic record.
	if n := h.count(); n != 0 {
		t.Fatalf("diagnostic records = %d, want 0", n)
	}
}

// --- Rule 1: inactive effective context settles by the shared cause rule ---

func TestClassifyInactiveContext(t *testing.T) {
	t.Run("client settlement aborts", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		// No settlement state: markerless inactive context is a transport
		// cancel (client).
		got := Classify(ctx, errors.New("whatever"))
		if got != abortSentinel {
			t.Fatalf("Classify() = %+v, want abort sentinel", got)
		}
	})

	t.Run("client outcome wins", func(t *testing.T) {
		// The settlement is read only once the effective context is no
		// longer active: a client claim wins over a later cancel.
		ctx, cancel := context.WithCancel(context.Background())
		st := &dbctx.SettlementState{}
		st.Claim(dbctx.OutcomeClient)
		ctx = dbctx.WithSettlement(ctx, st)
		cancel()
		got := Classify(ctx, errors.New("x"))
		if got != abortSentinel {
			t.Fatalf("Classify() = %+v, want abort sentinel", got)
		}
	})

	t.Run("service outcome yields unavailable", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		st := &dbctx.SettlementState{}
		st.Claim(dbctx.OutcomeService)
		ctx = dbctx.WithSettlement(ctx, st)
		cancel()
		got := Classify(ctx, errors.New("x"))
		if got == nil || got.Code != apierr.CodeUnavailable {
			t.Fatalf("Classify() = %+v, want unavailable", got)
		}
	})

	t.Run("response outcome renders nothing", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		st := &dbctx.SettlementState{}
		st.Claim(dbctx.OutcomeResponse)
		ctx = dbctx.WithSettlement(ctx, st)
		cancel()
		got := Classify(ctx, errors.New("x"))
		if got != nil {
			t.Fatalf("Classify() = %+v, want nil (committed status stands)", got)
		}
	})

	t.Run("markerless cancel with active state is a client abort", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		st := &dbctx.SettlementState{}
		ctx = dbctx.WithSettlement(ctx, st)
		cancel()
		got := Classify(ctx, errors.New("x"))
		if got != abortSentinel {
			t.Fatalf("Classify() = %+v, want abort sentinel", got)
		}
	})
}

// --- Diagnostic-record policy: allowlist mechanism ---

func TestDiagnosticRecordAllowlist(t *testing.T) {
	t.Run("over-long value truncated to bound", func(t *testing.T) {
		unreg := RegisterDetail("test_detail", 4)
		defer unreg()
		got := allowedDetailValues(
			slog.String("test_detail", "abcdefghijklmnop"),
			slog.String("not_allowlisted", "value"),
		)
		if len(got) != 1 {
			t.Fatalf("allowedDetailValues() = %v, want exactly 1 attr", got)
		}
		if got[0].Key != "test_detail" || got[0].Value.String() != "abcd" {
			t.Fatalf("allowedDetailValues() = %v, want test_detail=abcd", got)
		}
	})

	t.Run("non-allowlisted value dropped", func(t *testing.T) {
		got := allowedDetailValues(
			slog.String("not_allowlisted", "value"),
			slog.String("also_not", "value"),
		)
		if len(got) != 0 {
			t.Fatalf("allowedDetailValues() = %v, want 0 attrs", got)
		}
	})

	t.Run("representative driver error per row: no payload in record", func(t *testing.T) {
		h := newRecordingHandler()
		withLogs(t, h)

		// A 22P02 whose detail quotes the malformed vector input (caller
		// data) and a P0001 whose message carries trigger text with a
		// vector_space_id.
		payloads := []string{
			`malformed vector input "[1, NaN, 3]"`,
			"unknown vector_space_id: 0199f31e-2000-7000-8000-0000000000a1",
		}
		errs := []error{
			pgErr("22P02", "invalid input syntax for type vector: "+payloads[0]),
			pgErr("P0001", payloads[1]),
		}
		for i, err := range errs {
			got := Classify(context.Background(), err)
			if got == nil {
				t.Fatalf("Classify(%d) = nil", i)
			}
			f := h.fieldValues(i)
			// Always present: code, status, sqlstate (request_id only
			// where set on the context).
			if f["code"] == "" || f["status"] == "" || f["sqlstate"] == "" {
				t.Fatalf("record %d missing required fields: %v", i, f)
			}
			// No field value equals or contains any supplied payload.
			for k, v := range f {
				for _, p := range payloads {
					if v == p || containsFold(v, p) {
						t.Fatalf("record %d field %q contains the supplied payload", i, k)
					}
				}
			}
		}
	})
}

// containsFold reports whether s contains sub (case-insensitive).
func containsFold(s, sub string) bool {
	return strings.Contains(strings.ToLower(s), strings.ToLower(sub))
}
