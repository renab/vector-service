//go:build testseam

package server

import (
	"context"
	"sync"
	"time"

	"vector-service/internal/api"
)

// The testseam-gated drain model.
//
// Finding F-5: the settlement design distinguishes a peer transport
// cancellation (client abort) from a serve-root / drain-induced
// cancellation (service). The stdlib net/http layer closes a request's
// transport context as part of the bounded drain (Server.Shutdown closes
// idle connections and the drain's grace deadline closes the rest), so a
// handler that observes its own transport context after the marker is
// published sees it inactive — even though the client never aborted. The
// settlement primitives (dbctx) classify an inactive transport by its
// error: an error that is (or wraps) a transport-drain error is a drain;
// anything else is a peer abort.
//
// This file models that distinction for the lifecycle test. It wraps the
// real serve root in a context whose Err() returns an api drain error once
// the root is canceled.
//
// The per-request transport that the settlement primitives observe is the
// raw r.Context() — and the stdlib net/http layer wraps the BaseContext's
// output in a WithCancel context per request, so the drain annotation
// placed here is not what ResolveNone, SettleAndRecord, and AuthorizeCommit
// see (that observation is modeled by the testseam chain's drainTransport,
// see internal/api/chain_testseam.go). This context is the defense-in-depth
// annotation for any code path that reads the BaseContext-descended context
// directly (rather than through the stdlib's per-request wrapper), and it
// keeps the testseam BaseContext self-consistent: the lifecycle's own
// BaseContext observation (e.g. a handler that blocks on the serve root)
// sees the drain error too.
type drainCtx struct {
	parent context.Context
	mu     sync.Mutex
	drain  error
}

func (c *drainCtx) Deadline() (deadline time.Time, ok bool) {
	return c.parent.Deadline()
}

func (c *drainCtx) Done() <-chan struct{} { return c.parent.Done() }

func (c *drainCtx) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.drain != nil {
		return c.drain
	}
	err := c.parent.Err()
	if err == nil {
		return nil
	}
	// The root was canceled: the drain (the bounded shutdown) closes the
	// transport. Annotate the error so the settlement primitives classify
	// the observation as a drain, not a peer abort.
	c.drain = api.NewErrTransport(err)
	return c.drain
}

func (c *drainCtx) Value(key any) any { return c.parent.Value(key) }
