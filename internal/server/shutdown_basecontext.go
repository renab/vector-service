//go:build !testseam

package server

import (
	"context"
	"net"
)

// baseContextForRoot returns the BaseContext the lifecycle swaps into the
// HTTP server. It wraps the lifecycle's real serve root in serveRootCtx so
// that, once the lifecycle publishes the process-level shutdown marker and
// cancels the root (the normative signal order), every request context
// descended from the BaseContext reports a drain-annotated error (a
// *serveRootDrainError, implementing dbctx's transportDrainError) instead
// of a bare context.Canceled. That is what lets the settlement primitives
// (dbctx's shared cause rule, the fire path, the finalization step, the
// response gate) classify a shutdown-induced cancellation as
// service-initiated (503) rather than as a client abort (finding F-5): a
// markerless cancellation — the caller-abort and startup-failure paths —
// carries the bare context.Canceled and settles as a transport cancel.
//
// serveRootCtx consults dbctx.ShutdownMarked() on every Err() observation:
// the marker is published by the lifecycle (the signal handler) before the
// root is canceled, so the annotation appears exactly when the process is
// shutting down — never earlier (an uncanceled, markerless root reports a
// nil error), and never for a plain cancellation. The stdlib net/http layer
// wraps this output in its per-request WithCancel context, so per-request
// transport observations see the drain error wrapped (dbctx unwraps it).
func baseContextForRoot(root context.Context) func(net.Listener) context.Context {
	return productionBaseContextForRoot(root)
}
