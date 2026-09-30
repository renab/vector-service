//go:build testseam

package server

import (
	"context"
	"net"
)

// baseContextForRoot returns the production BaseContext under the testseam
// build too (the drain annotation now lives in the production file,
// shutdown_basecontext.go). It exists only to keep the testseam build
// compiling with the same baseContextForRoot call site (shutdown.go, Serve)
// and to keep the testseam lifecycle's BaseContext self-consistent: the
// production serve-root wrapper annotates the drain by consulting the
// process-level marker at observation time, which is exactly what the
// testseam settlement tests assert end-to-end (finding F-5). The older
// testseam drain model (a drainCtx wrapping api.ErrTransport, which the
// stdlib per-request WithCancel layer did not propagate to the request
// transport observations anyway) is gone: the production path is the one
// the tests exercise.
func baseContextForRoot(root context.Context) func(net.Listener) context.Context {
	return productionBaseContextForRoot(root)
}
