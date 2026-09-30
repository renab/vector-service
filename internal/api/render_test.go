package api

import (
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"vector-service/internal/apierr"
	"vector-service/internal/dbctx"
)

// testStatusRecorder models the response gate's committed-status check: it
// reports the first committed status (the recorder's `Status()`).
type testStatusRecorder struct {
	status int
	body   strings.Builder
}

func (r *testStatusRecorder) Header() http.Header { return http.Header{} }
func (r *testStatusRecorder) WriteHeader(code int) {
	if r.status == 0 {
		r.status = code
	}
}
func (r *testStatusRecorder) Write(p []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	return r.body.Write(p)
}
func (r *testStatusRecorder) Status() int { return r.status }

// wantBody is the canonical catalog body for a code, with the given
// request ID.
func wantBody(code apierr.Code, requestID string) []byte {
	return []byte(`{"error":{"code":"` + string(code) +
		`","message":"` + code.Message() +
		`","request_id":"` + requestID + `"}}` + "\n")
}

func TestRender(t *testing.T) {
	requestID := "0199f31e-2000-7000-8000-0000000000a1"
	ctx := dbctx.WithRequestID(context.Background(), requestID)

	t.Run("exact body shape for a 4xx", func(t *testing.T) {
		rec := &testStatusRecorder{}
		Render(ctx, rec, apierr.ErrNamespaceNotFound)
		if rec.status != http.StatusNotFound {
			t.Fatalf("status = %d, want 404", rec.status)
		}
		want := wantBody(apierr.CodeNamespaceNotFound, requestID)
		if got := []byte(rec.body.String()); !reflect.DeepEqual(got, want) {
			t.Fatalf("body = %s, want %s", got, want)
		}
		// The body must parse to exactly the three documented fields.
		var parsed struct {
			Error struct {
				Code      string `json:"code"`
				Message   string `json:"message"`
				RequestID string `json:"request_id"`
			} `json:"error"`
		}
		if err := json.Unmarshal([]byte(rec.body.String()), &parsed); err != nil {
			t.Fatalf("body is not valid JSON: %v", err)
		}
		if parsed.Error.Code != "namespace_not_found" || parsed.Error.Message != "namespace not found" || parsed.Error.RequestID != requestID {
			t.Fatalf("parsed = %+v", parsed.Error)
		}
	})

	t.Run("exact body shape for the 503", func(t *testing.T) {
		rec := &testStatusRecorder{}
		Render(ctx, rec, apierr.ErrUnavailable)
		if rec.status != http.StatusServiceUnavailable {
			t.Fatalf("status = %d, want 503", rec.status)
		}
		want := wantBody(apierr.CodeUnavailable, requestID)
		if got := []byte(rec.body.String()); !reflect.DeepEqual(got, want) {
			t.Fatalf("body = %s, want %s", got, want)
		}
	})

	t.Run("abort sentinel writes nothing", func(t *testing.T) {
		rec := &testStatusRecorder{}
		Render(ctx, rec, abortSentinel)
		if rec.status != 0 {
			t.Fatalf("status = %d, want 0 (nothing committed)", rec.status)
		}
		if rec.body.Len() != 0 {
			t.Fatalf("body = %q, want empty", rec.body.String())
		}
	})

	t.Run("nil writes nothing", func(t *testing.T) {
		rec := &testStatusRecorder{}
		Render(ctx, rec, nil)
		if rec.status != 0 || rec.body.Len() != 0 {
			t.Fatalf("status = %d body = %q, want nothing", rec.status, rec.body.String())
		}
	})

	t.Run("already-committed response is never overwritten", func(t *testing.T) {
		rec := &testStatusRecorder{}
		rec.WriteHeader(http.StatusOK) // the handler's committed success
		Render(ctx, rec, apierr.ErrUnavailable)
		if rec.status != http.StatusOK {
			t.Fatalf("status = %d, want the preserved 200", rec.status)
		}
		// No 503 body was appended to the committed response.
		var parsed map[string]any
		if err := json.Unmarshal([]byte(rec.body.String()), &parsed); err == nil {
			t.Fatalf("body was written on top of the committed response: %s", rec.body.String())
		}
	})

	t.Run("request id comes from the context, not the error", func(t *testing.T) {
		rec := &testStatusRecorder{}
		Render(context.Background(), rec, apierr.ErrInternal) // no request ID on the context
		var parsed struct {
			Error struct {
				RequestID string `json:"request_id"`
			} `json:"error"`
		}
		if err := json.Unmarshal([]byte(rec.body.String()), &parsed); err != nil {
			t.Fatalf("body is not valid JSON: %v", err)
		}
		if parsed.Error.RequestID != "" {
			t.Fatalf("request_id = %q, want empty", parsed.Error.RequestID)
		}
	})

	t.Run("stable message never interpolates", func(t *testing.T) {
		rec := &testStatusRecorder{}
		Render(ctx, rec, apierr.ErrInvalidVectorDimensions)
		if !strings.Contains(rec.body.String(), `"message":"vector dimensions do not match the vector space"`) {
			t.Fatalf("body = %s", rec.body.String())
		}
	})
}
