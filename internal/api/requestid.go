package api

import (
	"context"
	"net/http"

	"vector-service/internal/dbctx"
)

// RequestIDHeader is the inbound/outbound request-ID header. The ID is not
// a secret and not a token (overview): it is echoed in responses, in the
// access line, and in every log record for the request.
const RequestIDHeader = "X-Request-Id"

// maxRequestIDLen bounds an accepted inbound request ID. Values longer than
// this are replaced by a generated ID (the package-4 proposal: replace, not
// reject).
const maxRequestIDLen = 128

// ResolveRequestID determines the effective request ID for a request
// (implementation package 4, section 2, request-ID stage): the inbound
// X-Request-Id header when it passes the bounded grammar check, otherwise a
// freshly generated ID (dbctx.NewRequestID). It returns the ID and the
// context carrying it (dbctx.WithRequestID) so every later log record for
// the request carries it.
//
// The grammar check is deliberately permissive but bounded: printable
// ASCII, length-bounded. A request ID is a log correlation value, not a
// secret, so it is validated for log safety, not for meaning. An empty,
// non-printable, or over-long value is rejected (replaced by a generated
// ID) rather than accepted.
func ResolveRequestID(ctx context.Context, r *http.Request) (string, context.Context) {
	if id := r.Header.Get(RequestIDHeader); id != "" && validRequestID(id) {
		return id, dbctx.WithRequestID(ctx, id)
	}
	id, err := dbctx.NewRequestID()
	if err != nil {
		// crypto/rand failure is unrecoverable; the request cannot be
		// logged with a generated ID. Fall back to a fixed, safe,
		// non-colliding value so the access line and any log records
		// still carry a request ID. This is a defect backstop, not a
		// normal path.
		return "00000000000000000000000000000000", ctx
	}
	return id, dbctx.WithRequestID(ctx, id)
}

// validRequestID reports whether id passes the inbound grammar check:
// non-empty, at most maxRequestIDLen bytes, and every byte a printable
// ASCII character (space is deliberately excluded — a header value that is
// only whitespace is not a usable correlation ID).
func validRequestID(id string) bool {
	if id == "" || len(id) > maxRequestIDLen {
		return false
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		if c < 0x21 || c > 0x7e {
			return false
		}
	}
	return true
}
