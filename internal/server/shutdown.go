// The bounded-shutdown lifecycle (implementation package 4, section 7,
// "Server and bounded shutdown").
//
// ShutdownController.Serve runs the complete lifecycle: it creates the serve
// root context (context.WithCancel(context.Background())), swaps it into the
// constructed HTTP server's BaseContext (so every request context — and
// through it every PostgreSQL operation — is a descendant cancelable by the
// lifecycle), and registers exactly one signal handler for SIGINT and
// SIGTERM.
//
// On the first such signal the normative sequence runs, in exact order:
//
//  1. log a shutdown-start record, then publish the process-level shutdown
//     marker (dbctx.MarkShutdown) — the signal handler is the sole writer,
//     it writes exactly once, and the marker is never cleared (shutdown is
//     terminal for the process) — then cancel the serve root context. The
//     order is normative and race-free: the marker is published before the
//     cancel, so no request's classifier can observe the resulting
//     cancellation without the marker.
//
//  2. drain: Server.Shutdown under VEC_HTTP_SHUTDOWN_GRACE, from an
//     independent context (never derived from the just-canceled serve root).
//     A grace-deadline return is a backstop only: it interrupts nothing and
//     changes no exit status.
//
//  3. close the runtime pool, bounded: DB.Close() (blocks until every
//     acquired lease is returned) in a goroutine, observed against the hard
//     VEC_HTTP_SHUTDOWN_CLOSE_TIMEOUT.
//
// Exit outcome: nil when the pool close completes within the hard deadline;
// ErrPoolCloseTimeout (the structured shutdown_pool_close_timeout outcome)
// when it does not. No other shutdown outcome exists, and the process never
// hangs on a signal (worst-case stop = grace + close timeout). Repeated
// signals are ignored.
//
// Startup failure (no drain): if serving fails before listening, the
// constructed pool (if any) is closed directly and the startup error is
// returned; no signal path runs.
//
// Serve is a library unit: it returns an outcome (it does not call
// os.Exit). The command entrypoint (a later package) maps the outcome to the
// process exit status (0 for nil, 1 for ErrPoolCloseTimeout, non-zero for a
// startup error).
package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"vector-service/internal/config"
	"vector-service/internal/dbctx"
)

// ErrPoolCloseTimeout is the structured shutdown_pool_close_timeout outcome:
// the pool close exceeded the hard VEC_HTTP_SHUTDOWN_CLOSE_TIMEOUT during
// bounded shutdown. It is not an API response code (the response catalog is
// the complete set of response bodies); it is the lifecycle's exit-outcome
// value, which the command entrypoint maps to exit status 1.
type ErrPoolCloseTimeout struct {
	// Timeout is the hard bound the close exceeded.
	Timeout time.Duration
}

// Error renders the stable structured outcome message.
func (e *ErrPoolCloseTimeout) Error() string {
	return fmt.Sprintf("shutdown_pool_close_timeout: pool close exceeded %s", e.Timeout)
}

// IsPoolCloseTimeout reports whether err is the pool-close-timeout outcome.
func IsPoolCloseTimeout(err error) bool {
	var t *ErrPoolCloseTimeout
	return errors.As(err, &t)
}

// poolCloser is the bounded surface the lifecycle exercises on the runtime
// pool: Close blocks until every acquired lease is returned (the bounded
// shutdown invariant, package 2: every exit path releases its lease), and is
// idempotent. The real *pgxpool.Pool satisfies it; the unit tests substitute
// a scripted one so the close bound can be observed without a database.
type poolCloser interface {
	Close()
}

// ShutdownController is the bounded-shutdown lifecycle (section 7). It holds
// the configuration, the constructed HTTP server, and the runtime pool. The
// only process-global mutable state it touches is the process-level shutdown
// marker in dbctx — which the signal handler publishes exactly once and
// never clears (shutdown is terminal for the process).
type ShutdownController struct {
	// cfg is the validated configuration: it carries the two shutdown bounds
	// (HTTPShutdownGrace, HTTPShutdownCloseTimeout).
	cfg *config.Config
	// srv is the constructed HTTP server. The lifecycle owns its serve-root
	// cancel and swaps the root into its BaseContext at Serve time; it does
	// not rebuild the server.
	srv *Server
	// pool is the runtime pool closed during shutdown (nil: no pool
	// constructed, the close step is a no-op).
	pool poolCloser
	// root and rootCancel are the lifecycle-owned serve root context and its
	// cancel, created at Serve time. The marker is published before
	// rootCancel runs (the normative order).
	root       context.Context
	rootCancel context.CancelFunc
	// sigSrc is the signal source. In production it is nil and Serve
	// registers the real os/signal handler (realSignals); a test injects a
	// concrete channel through NewShutdownControllerForSignals so signals are
	// delivered directly (real signal delivery is unreliable to self-send
	// repeatedly in one process).
	sigSrc func() chan os.Signal
	// sigCh is the signal channel Serve selects on: either the channel
	// sigSrc returns (production: the real os/signal channel; test: the
	// injected one) or nil until Serve runs. Storing it (rather than a
	// fresh value per Serve) means the channel the lifecycle selects on is
	// the same channel the test delivers to.
	sigCh chan os.Signal
	// listener, when non-nil, is served directly instead of via
	// ListenAndServe. Production leaves it nil (ListenAndServe binds). A test
	// injects a pre-bound listener so it owns the port and two lifecycle
	// tests never collide; the production bind path is left for the
	// startup-failure test.
	listener net.Listener
}

// NewShutdownController returns the lifecycle for the constructed server and
// runtime pool. pool may be nil (no pool constructed); in that case the
// bounded close step is a no-op. In production pool is always the real
// *pgxpool.Pool; the test form NewShutdownControllerWithCloser accepts a
// scripted poolCloser.
func NewShutdownController(cfg *config.Config, srv *Server, pool *pgxpool.Pool) *ShutdownController {
	var pc poolCloser
	if pool != nil {
		pc = pool
	}
	return &ShutdownController{cfg: cfg, srv: srv, pool: pc, sigSrc: realSignals}
}

// NewShutdownControllerWithCloser returns the lifecycle with an explicit
// poolCloser (the unit-test seam for the bounded close).
func NewShutdownControllerWithCloser(cfg *config.Config, srv *Server, pool poolCloser) *ShutdownController {
	return &ShutdownController{cfg: cfg, srv: srv, pool: pool, sigSrc: realSignals}
}

// realSignals is the production signal source: it registers the single
// handler for SIGINT and SIGTERM and returns the channel they deliver to. It
// is called exactly once per Serve (inside Serve, at registration time); the
// channel it returns is stopped by Serve's defer.
func realSignals() chan os.Signal {
	ch := make(chan os.Signal, 2)
	signal.Notify(ch, syscall.SIGINT, syscall.SIGTERM)
	return ch
}

// Serve runs the complete bounded-shutdown lifecycle.
//
// The lifecycle creates the serve root context and swaps it into the
// server's BaseContext (before the listener opens, so no request can exist
// yet), then registers the single signal handler. On the first SIGINT or
// SIGTERM it runs the normative sequence and returns the exit outcome (nil
// or ErrPoolCloseTimeout). If ctx becomes inactive first (the caller aborted
// before any signal), no signal path runs: the pool is closed directly and
// Serve returns nil.
func (c *ShutdownController) Serve(ctx context.Context) error {
	// The serve root: the lifecycle-owned cancelable context that becomes
	// the server's BaseContext. Every request context is its descendant.
	c.root, c.rootCancel = context.WithCancel(context.Background())
	defer c.rootCancel() // safety net on early return; the signal path cancels too

	// Swap the serve root into the server's BaseContext before the listener
	// opens (no request can observe it otherwise). This is the section-7
	// contract: the lifecycle creates the root and the server serves from
	// it. (Under the testseam tag the lifecycle wraps the root in the drain
	// model, shutdown_drain_testseam.go, so the bounded drain's transport
	// close is annotated as a drain rather than a peer abort — see
	// shutdown_basecontext_testseam.go.)
	c.srv.httpServer.BaseContext = baseContextForRoot(c.root)

	// Register the signal source. In production realSignals installs the
	// single handler for SIGINT and SIGTERM; a test may inject its own source
	// (NewShutdownControllerForSignals) so signals are delivered directly.
	// The returned channel is the sole writer of the process-level shutdown
	// marker: on the first signal the marker is published exactly once,
	// before the serve root is canceled. A second signal merely queues on the
	// buffered channel and is never drained after the first wins — the write
	// is exactly once by construction (repeated signals ignored). Storing it
	// on the controller (rather than a local) means the channel Serve selects
	// on is the same channel an injected source returns, so a test can both
	// observe readiness (channel non-nil) and deliver to it.
	sigCh := c.sigSrc()
	c.sigCh = sigCh
	defer signal.Stop(sigCh)

	// Start serving. When a listener was injected (the test seam) it is
	// served directly; otherwise ListenAndServe binds the configured address.
	// Both return an error when the listener closes (after the drain) or on a
	// startup failure.
	srvErr := make(chan error, 1)
	go func() {
		if c.listener != nil {
			srvErr <- c.srv.httpServer.Serve(c.listener)
			return
		}
		srvErr <- c.srv.httpServer.ListenAndServe()
	}()

	select {
	case <-ctx.Done():
		// The caller aborted before any shutdown signal. No signal path
		// runs: stop serving, cancel the root, close the pool directly
		// (the startup-failure / external-abort path — no drain), and
		// return.
		c.rootCancel()
		_ = c.srv.httpServer.Close() // unblocks ListenAndServe
		<-srvErr                     // drain the serve goroutine
		c.closePoolDirect()
		return nil

	case sig := <-sigCh:
		// The first SIGINT or SIGTERM: run the normative sequence and
		// return its exit outcome.
		return c.shutdown(sig)

	case err := <-srvErr:
		// Serving failed (the startup-failure, no-drain path): close the
		// pool directly and return the startup error. No signal path ran.
		c.rootCancel()
		c.closePoolDirect()
		return err
	}
}

// shutdown runs the exact section-7 sequence on the first signal and returns
// the exit outcome (nil or ErrPoolCloseTimeout).
func (c *ShutdownController) shutdown(sig os.Signal) error {
	// Step 1 (normative order): log the shutdown-start record, publish the
	// process-level shutdown marker (sole writer, exactly once, never
	// cleared), then cancel the serve root. The marker is published before
	// the cancel — race-free: no classifier can observe the cancellation
	// without the marker.
	slog.Info("shutdown started", "signal", sig.String())
	dbctx.MarkShutdown()
	c.rootCancel()

	// Step 2: drain. Server.Shutdown under the grace bound, from an
	// independent context — never derived from the just-canceled serve
	// root. A grace-deadline return is a backstop only: it changes no exit
	// status. The outstanding handlers (already context-canceled) complete
	// on their own.
	graceCtx, graceCancel := context.WithTimeout(context.Background(), c.cfg.HTTPShutdownGrace)
	defer graceCancel()
	if err := c.srv.httpServer.Shutdown(graceCtx); err != nil {
		slog.Warn("shutdown drain: grace deadline or error", "error", err.Error())
	}

	// Step 3: bounded pool close. The outcome drives the exit status.
	return c.boundedPoolClose()
}

// boundedPoolClose closes the runtime pool, bounded by the hard close
// timeout. It returns nil when the close completes in time and
// ErrPoolCloseTimeout when it does not. A nil pool is a no-op.
func (c *ShutdownController) boundedPoolClose() error {
	if c.pool == nil {
		return nil
	}
	done := make(chan struct{})
	go func() {
		// DB.Close blocks until every acquired lease is returned. It is
		// idempotent, so the timeout path (which does not call Close
		// again) cannot double-close.
		c.pool.Close()
		close(done)
	}()

	select {
	case <-done:
		return nil
	case <-time.After(c.cfg.HTTPShutdownCloseTimeout):
		return &ErrPoolCloseTimeout{Timeout: c.cfg.HTTPShutdownCloseTimeout}
	}
}

// closePoolDirect closes the runtime pool without a bound (the
// startup-failure / external-abort path: no drain, no close-timeout race). A
// nil pool is a no-op.
func (c *ShutdownController) closePoolDirect() {
	if c.pool != nil {
		c.pool.Close()
	}
}
