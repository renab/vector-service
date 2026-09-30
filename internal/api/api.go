// Package api is the HTTP surface of the Vector Service (implementation
// package 4): the handler contract (section 1), the strict JSON decode
// helper and the request-ID grammar (sections 1 and 2), and the error
// renderer and classifier — the single classification boundary (section 3).
//
// The middleware chain, route registration, endpoint handlers, and the
// server/bounded-shutdown lifecycle are the remainder of package 4 and are
// not part of this package's scope here; this package defines only the
// primitives those later parts consume, per the package-4 contract.
//
// The single-classification-boundary invariant (overview error model):
// service functions below the handler return plain error values; the
// handler layer converts every raw error exactly once into a typed
// *apierr.Error through Classify, and Render is the only code that writes
// response bytes. Raw PostgreSQL driver text never reaches a response body
// or a log record: Classify emits at most one structured diagnostic record
// per classified raw driver error, carrying only the safe allowlist fields
// (code, status, SQLSTATE, request ID, and — where set — the resolved
// application/namespace identifiers), never the driver's primary message.
package api

import (
	"context"
	"net/http"

	"vector-service/internal/apierr"
)

// Handler is the single handler contract of the service (implementation
// package 4, section 1). Data-plane and admin handlers implement it; the
// endpoint adapter (section 4) is the only site that converts it to
// http.HandlerFunc.
//
// The handler owns its success response: it writes the success status and
// JSON body to w itself, then returns nil. A failure path writes nothing to
// w and returns a typed *apierr.Error — each raw error from the service
// path converted exactly once through Classify. The concrete pointer return
// type means a raw driver error cannot cross the boundary by construction.
type Handler func(ctx context.Context, w http.ResponseWriter, in *HandlerInput) *apierr.Error

// HandlerInput is the per-request input the adapter builds for a handler.
type HandlerInput struct {
	// Req is the raw request; the handler decodes its own body (through the
	// strict-JSON helper) as its first action on body endpoints.
	Req *http.Request

	// AppID is the authenticated application identity, carried as the
	// canonical UUID string (the same string dbctx.WithAppID carries; the
	// service carries the identity as a string throughout, and the
	// database's SQL cast is the authoritative check). On admin routes the
	// adapter leaves it empty (the zero identity): the admin stage
	// authorizes but resolves no application, and admin handlers resolve
	// the {application} path key themselves.
	AppID string

	// Path holds the raw path-segment values (unvalidated). Their grammar
	// (UUID shape, non-emptiness) is validated by the owning handler (400
	// invalid_uuid / missing_field), not the router.
	Path map[string]string
}
