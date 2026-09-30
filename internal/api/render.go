package api

import (
	"context"
	"encoding/json"
	"net/http"

	"vector-service/internal/apierr"
	"vector-service/internal/dbctx"
)

// errorBody is the single stable error response shape (docs/API.md):
// {"error": {"code", "message", "request_id"}}. code and message come from
// the closed catalog (a stable string per code, never interpolating caller
// input or driver text); request_id comes from the request context (the
// effective ID resolved by the request-ID stage) at render time.
type errorBody struct {
	Error errorDetail `json:"error"`
}

type errorDetail struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	RequestID string `json:"request_id"`
}

// Render is the only renderer and the centralized cancellation responder
// (implementation package 4, section 3). It accepts exactly the typed
// error, never classifies, and writes the catalog body with the mapped
// status. Raw PostgreSQL text never reaches a response body.
//
// Wire semantics (per section 3):
//
//   - for the abort sentinel (client settlement) it writes nothing (no
//     status, no body): the transport's closure was already known before
//     the render decision, so the write is suppressed (the access line
//     carries status_code 0);
//   - for a nil error it writes nothing (the committed status stands —
//     dbctx.Settle returned nil for a `response` outcome);
//   - for any catalog error it commits exactly one status and the body. A
//     status already committed on the writer (the first status is
//     unreplaceable) is never overwritten: a second status or body is
//     never written;
//   - for unavailable (503) settled from a service-initiated cancellation
//     it is the only writer of the cancellation response: the
//     already-committed check above preserves a success the handler
//     committed, and otherwise exactly one 503 is committed.
func Render(ctx context.Context, w http.ResponseWriter, err *apierr.Error) {
	if err == nil || err == abortSentinel {
		// Abort (client settlement): nothing is written. nil: the committed
		// status stands; nothing further is rendered.
		return
	}
	// The first-committed status is unreplaceable: a response the handler
	// already committed preserves its first status, and the 503 (or any
	// other error status) is dropped, never appended.
	if recorder, ok := w.(interface{ Status() int }); ok {
		if recorder.Status() != 0 {
			return
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(err.Status)
	_ = json.NewEncoder(w).Encode(errorBody{
		Error: errorDetail{
			Code:      string(err.Code),
			Message:   err.Message,
			RequestID: dbctx.RequestID(ctx),
		},
	})
}
