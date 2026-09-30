//go:build testseam

package api

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"vector-service/internal/dbctx"
)

// The testseam-gated chain composition: the lifecycle tests in
// internal/server need to mount the real base chain (request ID → access
// logging → operation deadline → recovery) with a test-local transport
// context that annotates the bounded drain. Production never does this (the
// stdlib never annotates the drain); the testseam chain is the only code
// that does.
//
// The drain-aware transport is installed into the access-logging stage's
// response gate (the first-commit settlement rule) and the deadline stage's
// deadlineStage (the fire path and the finalization step). The real
// r.Context() is still used for the handler's effective context and every
// Done/Value/Dadline delegation; only the settlement primitives observe the
// drain error (finding F-5: a bounded drain is a service-initiated
// settlement, not a peer abort).
//
// No production path references this file.

// drainTransport is the test-local transport context: it delegates Done,
// Deadline, and Value to the real transport (r.Context()) and returns the
// drain error (ErrTransport) in Err() once the real transport is inactive.
// It is the only code that annotates the drain; production never does.
type drainTransport struct {
	transport context.Context
}

func (d *drainTransport) Deadline() (deadline time.Time, ok bool) {
	return d.transport.Deadline()
}

func (d *drainTransport) Done() <-chan struct{} { return d.transport.Done() }

func (d *drainTransport) Err() error {
	err := d.transport.Err()
	if err == nil {
		return nil
	}
	return NewErrTransport(err)
}

func (d *drainTransport) Value(key any) any { return d.transport.Value(key) }

// drainTransportFor returns the test-local transport context that wraps the
// real transport with the drain annotation.
func drainTransportFor(transport context.Context) context.Context {
	return &drainTransport{transport: transport}
}

// accessLogTestseam is the testseam-gated access-logging stage: it is the
// production access-logging stage with the response gate's transport set to
// the drain-aware transport (drainTransportFor) rather than the raw
// r.Context(). The settlement primitives (ResolveNone, SettleAndRecord,
// AuthorizeCommit) observe the drain error when the transport is inactive.
func (c *Chain) accessLogTestseam(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		st := &dbctx.SettlementState{}
		logSt := &LogState{}
		transport := drainTransportFor(r.Context())
		rec := &responseGate{
			ResponseWriter: w,
			st:             st,
			transport:      transport,
		}
		ctx := r.Context()
		ctx = dbctx.WithSettlement(ctx, st)
		ctx = WithLogState(ctx, logSt)
		ctx = context.WithValue(ctx, recorderKey{}, rec)
		ctx = context.WithValue(ctx, drainTransportKey{}, transport)
		next.ServeHTTP(rec, r.WithContext(ctx))

		attrs := []any{
			slog.String("request_id", dbctx.RequestID(ctx)),
			slog.String("method", r.Method),
			slog.String("path", r.URL.Path),
			slog.Int("status_code", rec.Status()),
			slog.Int64("duration_ms", time.Since(start).Milliseconds()),
		}
		if logSt.ApplicationID != "" {
			attrs = append(attrs, slog.String("application_id", logSt.ApplicationID))
		}
		if logSt.NamespaceID != "" {
			attrs = append(attrs, slog.String("namespace_id", logSt.NamespaceID))
		}
		if logSt.NamespaceKey != "" {
			attrs = append(attrs, slog.String("namespace_key", logSt.NamespaceKey))
		}
		if logSt.Operation != "" {
			attrs = append(attrs, slog.String("operation", logSt.Operation))
		}
		if logSt.VectorSpaceKey != "" {
			attrs = append(attrs, slog.String("vector_space_key", logSt.VectorSpaceKey))
		}
		if logSt.ApplicationKey != "" {
			attrs = append(attrs, slog.String("application_key", logSt.ApplicationKey))
		}
		if logSt.ResultCount != nil {
			attrs = append(attrs, slog.Int("result_count", *logSt.ResultCount))
		}
		if logSt.Limit != nil {
			attrs = append(attrs, slog.Int("limit", *logSt.Limit))
		}
		if logSt.FilterCount != nil {
			attrs = append(attrs, slog.Int("filter_count", *logSt.FilterCount))
		}
		if logSt.Upserted != nil {
			attrs = append(attrs, slog.Int("upserted", *logSt.Upserted))
		}
		if logSt.Unchanged != nil {
			attrs = append(attrs, slog.Int("unchanged", *logSt.Unchanged))
		}
		slog.Info("access", attrs...)
	})
}

// drainTransportKey carries the drain-aware transport context in the request
// context. The testseam access-logging stage installs it; the testseam
// deadline stage reads it.
type drainTransportKey struct{}

// DrainTransportOf returns the drain-aware transport context carried by the
// context, or nil when none is carried (a context outside the testseam
// access-logging stage).
func DrainTransportOf(ctx context.Context) context.Context {
	t, _ := ctx.Value(drainTransportKey{}).(context.Context)
	return t
}

// BaseChainTestable composes the real base chain (request ID → access
// logging → operation deadline → recovery) over next, exactly as
// Chain.BaseChain does, with the testseam-gated access-logging and deadline
// stages (the drain-aware transport). It exists so the lifecycle tests can
// mount the real chain stages — with the real deadline stage deriving the
// effective context from the serve-root-descended transport context — while
// next (the test handler) captures the per-request settlement state and gate
// from its request context.
func (c *Chain) BaseChainTestable(next http.Handler) http.Handler {
	var h http.Handler = next
	h = c.recovery(h)
	h = c.operationDeadlineTestseam(h)
	h = c.accessLogTestseam(h)
	return c.requestID(h)
}

// operationDeadlineTestseam is the testseam-gated operation deadline stage:
// it is the production OperationDeadline.WithOperationDeadline with the
// deadlineStage's transport set to the drain-aware transport
// (drainTransportFor) rather than the raw r.Context(). The fire path and the
// finalization step observe the drain error when the transport is inactive.
func (c *Chain) operationDeadlineTestseam(next http.Handler) http.Handler {
	d := c.Deadline
	now := d.now
	if now == nil {
		now = time.Now
	}
	newTimer := d.newTimer
	if newTimer == nil {
		newTimer = time.AfterFunc
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The access-logging stage (outer) has installed the recorder and
		// the settlement state. Read them from the request context.
		st := dbctx.SettlementOf(r.Context())
		rec := recorderOf(r.Context())
		transport := DrainTransportOf(r.Context())
		if transport == nil {
			transport = r.Context()
		}
		if st == nil {
			// Outside the chain (a direct call): nothing to settle.
			next.ServeHTTP(w, r)
			return
		}

		// The effective context: derived with WithCancel — never
		// WithTimeout (the timer is lifecycle-controlled; a bare timer
		// would cancel with no settlement hook).
		ctx, cancel := context.WithCancel(r.Context())
		stage := &deadlineStage{
			transport: transport,
			st:        st,
			rec:       rec,
			cancel:    cancel,
		}
		ctx = context.WithValue(ctx, deadlineStageKey{}, stage)
		r = r.WithContext(ctx)

		// Arm the request timer: one timer per request, at the bound.
		if d.Timeout > 0 {
			stage.timer = newTimer(d.Timeout, func() { stage.fire() })
		}

		defer func() {
			// The deferred path (request goroutine, every return):
			// finalization step, then mark settled, then stop the timer
			// (best effort — a fire already scheduled or running loses
			// its claim against the committed outcome), then cancel
			// (idempotent).
			stage.finalize()
			stage.settled = true
			if stage.timer != nil {
				stage.timer.Stop()
			}
			cancel()
		}()

		next.ServeHTTP(w, r)
	})
}
