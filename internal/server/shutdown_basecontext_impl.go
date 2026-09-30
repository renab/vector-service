// productionServeRoot implements the production serve-root BaseContext:
// the serve-root wrapper (serveRootCtx) and its drain-annotated error
// (serveRootDrainError). It compiles under every build tag — including
// testseam — so the testseam lifecycle (shutdown.go, Serve) and the
// testseam tests exercise the exact same production code the service runs:
// under testseam, baseContextForRoot (shutdown_basecontext_testseam.go)
// delegates to productionBaseContextForRoot in this file.
package server

import (
	"context"
	"net"
	"sync"
	"time"

	"vector-service/internal/dbctx"
)

// productionBaseContextForRoot is the production serve-root BaseContext.
// It is named (not the bare baseContextForRoot) so the testseam build
// (shutdown_basecontext_testseam.go) can call the exact same function: the
// testseam lifecycle exercises the production BaseContext, not a model.
func productionBaseContextForRoot(root context.Context) func(net.Listener) context.Context {
	return func(_ net.Listener) context.Context {
		return &serveRootCtx{parent: root}
	}
}

// serveRootCtx is the production serve-root wrapper installed as the
// HTTP server's BaseContext. It delegates Deadline, Done, and Value to the
// lifecycle's real serve root; only Err() is annotated. Once the root is
// inactive, Err() returns the bare root error unless the process-level
// shutdown marker is set (dbctx.ShutdownMarked), in which case it returns a
// drain-annotated error wrapping the root error (a *serveRootDrainError,
// whose Unwrap returns it). The first drain error observed is cached so
// every subsequent observation returns the same error value.
//
// The wrapper observes only the marker; it never writes it. The sole
// writer remains the lifecycle's signal handler, which publishes the
// marker before it cancels the root (the normative order in shutdown.go).
type serveRootCtx struct {
	parent context.Context
	mu     sync.Mutex
	drain  error
}

func (c *serveRootCtx) Deadline() (deadline time.Time, ok bool) {
	return c.parent.Deadline()
}

func (c *serveRootCtx) Done() <-chan struct{} { return c.parent.Done() }

func (c *serveRootCtx) Err() error {
	err := c.parent.Err()
	if err == nil {
		return nil
	}
	// The root is inactive. Only a cancellation the lifecycle performed as
	// part of the bounded shutdown (marker published, normative order) is
	// annotated: a markerless inactive root is a plain cancellation (the
	// caller-abort and startup-failure paths) and reports the bare error.
	if !dbctx.ShutdownMarked() {
		return err
	}
	c.mu.Lock()
	if c.drain == nil {
		c.drain = &serveRootDrainError{cause: err}
	}
	return c.drain
}

func (c *serveRootCtx) Value(key any) any { return c.parent.Value(key) }

// serveRootDrainError is the production transport-drain error: a bounded
// shutdown closing the serve root (and through it the request transports),
// not a peer abort. It implements dbctx's transportDrainError interface
// (TransportDrain), so dbctx.isShutdownDrain classifies an inactive
// context carrying it as a shutdown drain. Its Unwrap returns the root's
// cancellation error, so errors.Is(err, context.Canceled) still matches.
type serveRootDrainError struct {
	cause error
}

func (e *serveRootDrainError) Error() string {
	if e.cause == nil {
		return "serve root closed (shutdown drain)"
	}
	return "serve root closed: " + e.cause.Error()
}

func (e *serveRootDrainError) Unwrap() error { return e.cause }

// TransportDrain marks the error as a transport drain (a bounded shutdown
// closing the serve root, not a peer abort): the dbctx isShutdownDrain
// check.
func (e *serveRootDrainError) TransportDrain() {}
