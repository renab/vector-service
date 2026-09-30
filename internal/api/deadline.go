package api

import (
	"context"
	"net/http"
	"time"

	"vector-service/internal/apierr"
	"vector-service/internal/dbctx"
)

// OperationDeadline is the operation deadline stage, the sole deadline
// implementation (implementation package 4, section 2, "Operation
// deadline"). It wraps next with the per-request lifecycle:
//
//   - it derives the request's effective context with
//     context.WithCancel(r.Context()) — never context.WithTimeout: a
//     WithTimeout timer cancels on its own with no hook to settle the
//     request first, and a cancel carries no classification information.
//     The WithCancel makes the cancel lifecycle-controlled: the effective
//     context can become inactive only through this stage's fire path,
//     this stage's deferred path (normal completion — it claims nothing
//     new; the response is already committed), or a cancel of an ancestor
//     (the serve root). The service-origin cancels — the fire path's and
//     the serve root's — each settle the request BEFORE they cancel; the
//     transport (client) cancel settles through synchronous transport
//     observation at the settlement points (there is no asynchronous
//     watcher);
//   - it arms one time.AfterFunc per request at the configured bound
//     (VEC_HTTP_REQUEST_TIMEOUT). The callback is the fire path (below);
//   - on every return of the wrapped chain it runs the finalization step
//     (below), then settles (marks settled), stops the timer (best
//     effort), and invokes cancel (idempotent).
//
// The stage itself classifies nothing and emits no log record. The
// derived context is the request's effective context for auth, decoding,
// handlers, and every PostgreSQL operation. It composes into both mount
// points (the full chain and the base chain): every request — including
// probes — is bounded by the operation deadline.
//
// When the deadline fires with the transport still active the request
// settles as service-initiated: exactly one stable 503 unavailable is
// committed (section 3), and in-flight PostgreSQL work cancels through the
// ordinary pgx context path. When the transport was already canceled, the
// fire path settles it as client instead (first-cause rule) — an abort,
// no response.
type OperationDeadline struct {
	// Timeout is the request bound (VEC_HTTP_REQUEST_TIMEOUT).
	Timeout time.Duration
	// now is the clock the timer arms against. Tests override it with a
	// manual clock so the fire path is forced, never raced. Production
	// uses time.Now (the zero value).
	now func() time.Time
	// newTimer is the timer constructor. Tests substitute a manual timer
	// so the fire path can be invoked synchronously; production uses
	// time.AfterFunc (the zero value).
	newTimer func(d time.Duration, fn func()) *time.Timer
}

// deadlineStage is the per-request deadline state, carried in the
// effective context so the fire path (a separate goroutine) and the
// deferred path (the request goroutine) observe the same state.
type deadlineStage struct {
	// transport is the transport request context (r.Context()): the
	// context the fire path observes synchronously in its cause-resolution
	// step (first-cause rule).
	transport context.Context
	// st is the request's settlement state (shared with the gate).
	st *dbctx.SettlementState
	// rec is the access-logging recorder / response gate (the only
	// http.ResponseWriter the inner chain sees).
	rec *responseGate
	// cancel cancels the effective context. Invoked by the fire path
	// (after its claim) and by the deferred path (idempotent).
	cancel context.CancelFunc
	// timer is the armed request timer. Stopped (best effort) by the
	// deferred path.
	timer *time.Timer
	// settled is set by the deferred path under st's synchronization.
	// The fire path's claim is a no-op after settle (a cancel never
	// settles a request that is already settled).
	settled bool
}

type deadlineStageKey struct{}

// WithOperationDeadline wraps next in the operation deadline stage.
// timeout is the request bound (VEC_HTTP_REQUEST_TIMEOUT). The
// transport-context and timer seams (the test seams that expose the claim,
// publish, cancel, and write steps) are taken from the stage's now and
// newTimer fields.
func (d *OperationDeadline) WithOperationDeadline(next http.Handler) http.Handler {
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
			transport: r.Context(),
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

// StageOf returns the per-request deadline stage carried by the effective
// context, or nil when none is carried (a context outside the stage, or
// the transport context r.Context()).
func StageOf(ctx context.Context) *deadlineStage {
	s, _ := ctx.Value(deadlineStageKey{}).(*deadlineStage)
	return s
}

// fire is the fire path (timer goroutine). It runs the cause-resolution
// step under the settlement state's synchronization, in exact order:
//
//  1. an outcome already recorded → release, cancel, settle nothing (a
//     late fire — a cancel never settles a request that is already
//     settled);
//  2. otherwise observe the transport request context (r.Context()) — if
//     it is already inactive, the client disconnected BEFORE the deadline
//     fired: record the client outcome (the first cause) and cancel;
//  3. otherwise record the service outcome (cause operation_deadline) and
//     cancel. The cancel itself never records or changes an outcome.
func (s *deadlineStage) fire() {
	outcome := s.st.SettleAndRecord(s.transport)
	// A recorded outcome (not service) means the fire path lost the
	// claim: the request already settled (response, client, or a
	// service claim by another settlement point). The cancel is still
	// invoked (idempotent — the deferred path also cancels) so the
	// effective context is deactivated; the cancel never records or
	// changes an outcome.
	if outcome == dbctx.OutcomeService {
		s.cancel()
	}
}

// finalize is the finalization step (request goroutine, after the inner
// chain returns, before settle). It reads the settlement state once under
// its synchronization, then acts:
//
//   - a status was committed (response outcome) → do nothing: the
//     first-committed status is unreplaceable, whatever else the state
//     shows;
//   - a client outcome → do nothing (abort; no response);
//   - a service outcome → render the cancellation response through
//     Render with typed unavailable: the centralized responder commits
//     exactly one 503 (access status_code 503; commit is not proof of
//     delivery); if the transport closed after the claim and before the
//     render, the write is suppressed and the access line carries 0 (the
//     outcome stays service — it is never reclassified);
//   - no outcome recorded → resolve it now, under the same
//     synchronization, by the shared cause rule: the transport observed
//     inactive → record client and do nothing (abort); the transport
//     active with the shutdown marker set → record service (cause
//     shutdown) and render the 503; the transport active with no marker →
//     the inner chain returned without committing any status (a handler
//     contract violation: it returned nil without writing a success) →
//     render 500 internal so a live connection always receives exactly
//     one response. This is a defect backstop, not a normal path.
func (s *deadlineStage) finalize() {
	outcome := s.st.ResolveNone(s.transport)

	switch outcome {
	case dbctx.OutcomeResponse:
		// The first-committed status stands; nothing further is rendered.
		// (A committed status implies no cancellation response is owed.)
	case dbctx.OutcomeClient:
		// Abort: no response.
	case dbctx.OutcomeService:
		// The centralized cancellation response: exactly one 503. If the
		// transport closed after the claim and before the render, the gate
		// suppresses the commit and the access line carries 0 (the
		// outcome stays service — it is never reclassified).
		Render(s.transport, s.rec, apierr.ErrUnavailable)
	default:
		// No outcome resolved: the inner chain returned without
		// committing any status (a handler contract violation). Render
		// 500 internal so a live connection always receives exactly one
		// response. This is a defect backstop, not a normal path.
		Render(s.transport, s.rec, apierr.ErrInternal)
	}
}
