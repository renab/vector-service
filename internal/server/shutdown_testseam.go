package server

import (
	"net"
	"os"

	"vector-service/internal/config"
)

// NewShutdownControllerForSignals returns a lifecycle whose signal source is
// the supplied channel: Serve selects on it in place of the real os/signal
// handler. A test observes readiness via SignalChannelForTest (non-nil once
// Serve has registered the source) and then delivers a signal by sending on
// the channel (exactly what a registered handler would receive). The channel
// must be buffered. The bounded close uses the supplied poolCloser (nil: no
// pool). When l is non-nil it is served directly (the test owns the port);
// when nil the production ListenAndServe path is used. This constructor
// composes the signal, close, and listener seams independently.
//
// It is intentionally untagged: the bounded-close test seam
// (NewShutdownControllerWithCloser) is already untagged, and both are
// constructors consumed only by tests in this package. The production
// signal path (realSignals) is unaffected: this merely swaps the source the
// lifecycle selects on.
func NewShutdownControllerForSignals(cfg *config.Config, srv *Server, poolCloser interface{ Close() }, ch chan os.Signal, l net.Listener) *ShutdownController {
	c := NewShutdownControllerWithCloser(cfg, srv, poolCloser)
	c.sigSrc = func() chan os.Signal { return ch }
	if l != nil {
		c.listener = l
	}
	return c
}
