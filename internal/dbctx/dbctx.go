// Package dbctx is the transaction-helper boundary of implementation
// package 2: the two helpers that own every database transaction the
// service runs, the typed helper outcomes, and the shared cause rule.
//
// Two helpers own all transactions (overview error model, "Transaction
// helpers are a clear, small boundary"):
//
//   - WithAppContext: the application-scoped transaction. BEGIN →
//     set_config('vector.application_id', $uuid, true) as the FIRST
//     statement (transaction-local; the third argument stays true) → the
//     callback → COMMIT / ROLLBACK. The transaction-local application
//     context is what the forced RLS policies on vector_records and
//     namespaces read (migrations 0001).
//
//   - WithShortTx: the context-less short transaction for paths that
//     touch no RLS-gated table (the credential digest lookup, the
//     throttled last_used_at stamp, admin lookups/inserts other than
//     namespace registration). No set_config is executed.
//
// Failure semantics (the helper boundary). At every failure point the
// helper checks, in order:
//
//  1. the effective context is no longer active → settle by the shared
//     cause rule (Settle): the request's settlement state is read through
//     its synchronization — a `client` outcome, or no outcome with the
//     process-level shutdown marker unset, yields the abort sentinel (no
//     response is written); a `service` outcome, or no outcome with the
//     marker set, yields the typed unavailable outcome (503); a
//     `response` outcome means the committed status stands and no further
//     response is written.
//
//  2. any other infrastructure failure (bounded-acquisition failure,
//     BEGIN, set_config, COMMIT, or ROLLBACK) → the typed unavailable
//     outcome (503).
//
// A business error returned by the callback passes through the helper
// unchanged (it is classified exactly once at the handler boundary,
// package 4). A ROLLBACK failure after a business error is warn-logged and
// attached to the original error; it never replaces it. A COMMIT failure
// after a successful callback is itself the helper outcome (rule 1, then
// rule 2). Every exit path releases its pool lease.
//
// No code outside these helpers begins a transaction: no db.Begin
// anywhere else; each request runs its own transactions and releases every
// lease on every exit path (the bounded shutdown, package 4, depends on
// this).
package dbctx

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrAbort is the abort sentinel: the internal control outcome of an
// inactive request context with a client-initiated cause. It is not a
// catalog code and never produces a response: the renderer settles it by
// writing nothing (access-log status 0).
//
// It is a value type, not an error: the helpers return it as a typed
// (zero-value error) outcome distinct from every database failure, and it
// carries no message that could leak request detail.
type ErrAbort struct{}

func (ErrAbort) Error() string { return "abort" }

// abortError wraps ErrAbort so errors.Is(err, dbctx.ErrAbort{}) matches a
// value while the outcome remains distinguishable from a database error.
type abortError struct{}

func (abortError) Error() string { return "abort" }

func (abortError) Is(target error) bool {
	_, ok := target.(ErrAbort)
	return ok
}

// ErrAbortOutcome is the abort sentinel outcome. errors.Is(err,
// ErrAbortOutcome) reports whether a helper returned it.
var ErrAbortOutcome = abortError{}

// IsAbort reports whether err is the abort sentinel outcome.
func IsAbort(err error) bool { return errors.Is(err, ErrAbortOutcome) }

// Unavailable is the typed database-infrastructure outcome (503). It is the
// helper's result for every infrastructure failure while the request
// context is active, and for an inactive context with a service-initiated
// cause. The wrapped cause is diagnostic only (raw PostgreSQL text never
// reaches a response); Error renders the stable message.
type Unavailable struct {
	Cause error
}

// Error renders the stable 503 message.
func (e *Unavailable) Error() string { return "unavailable" }

// Unwrap returns the diagnostic cause.
func (e *Unavailable) Unwrap() error { return e.Cause }

// Settle is the shared cause rule, implemented once here and shared with
// the package-4 classifier/renderer. It settles an inactive effective
// context by reading the request's settlement state through its
// synchronization:
//
//   - a `client` outcome (or no outcome with the shutdown marker unset —
//     a markerless inactive context is a transport cancel) → the abort
//     sentinel;
//   - a `service` outcome (or no outcome with the marker set) → the typed
//     unavailable outcome (503);
//   - a `response` outcome → the committed status stands: no further
//     response is written (the helper returns nil to its caller only in
//     the sense that no outcome is produced; callers treat it as
//     "settled").
//
// Cancellation carries no classification of its own; only these claims do.
// The rule never reclassifies: a claim that already won the settlement is
// the outcome, whatever the marker later does.
func Settle(ctx context.Context) error {
	st := stateOf(ctx)
	var outcome Outcome
	if st != nil {
		st.mu.Lock()
		outcome = st.outcome
		st.mu.Unlock()
	}
	switch outcome {
	case OutcomeClient:
		return ErrAbortOutcome
	case OutcomeService:
		return &Unavailable{Cause: ctx.Err()}
	case OutcomeResponse:
		// The committed status stands; nothing further is rendered.
		return nil
	default:
		// No outcome yet: the process-level shutdown marker is the
		// fallback. Set → service-initiated (503); unset → a markerless
		// transport cancel (abort).
		if shutdown.Load() != 0 {
			return &Unavailable{Cause: ctx.Err()}
		}
		return ErrAbortOutcome
	}
}

// Outcome is a settlement outcome: the first of the three claims recorded
// in a request's settlement state.
type Outcome int

// The three settlement outcomes (overview, "Cancellation"): the first claim
// wins, later claims are no-ops, and no outcome is ever reclassified.
const (
	OutcomeNone Outcome = iota
	// OutcomeClient: the transport request context was observed inactive
	// at a settlement point (client disconnect). → abort sentinel.
	OutcomeClient
	// OutcomeService: the operation deadline fired, or the process is
	// shutting down (or the shutdown marker is set with no outcome). →
	// unavailable (503).
	OutcomeService
	// OutcomeResponse: the first status was committed; the committed
	// status stands.
	OutcomeResponse
)

// SettlementState is the request's small synchronized settlement state:
// one mutex guarding the first-of-three outcome claims (overview: "one
// small synchronized settlement state (one mutex)"). The first claim wins;
// later claims return false and never reclassify the request.
type SettlementState struct {
	mu      sync.Mutex
	outcome Outcome
}

// Claim records the first outcome. It returns true when this claim won.
// Claims are made under the state's synchronization; the caller performs
// any synchronous observation of the transport context before claiming.
func (s *SettlementState) Claim(o Outcome) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.outcome != OutcomeNone {
		return false
	}
	s.outcome = o
	return true
}

// Outcome returns the recorded outcome (OutcomeNone when none yet).
func (s *SettlementState) Outcome() Outcome {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.outcome
}

// ReadOutcomeMarker is a single synchronized read of the outcome and the
// process-level shutdown marker, returning them together so a caller
// observes a consistent snapshot (the outcome and the marker are read
// under the same critical section). It is the settlement primitive the
// package-4 deadline stage's fire path and finalization step use.
func (s *SettlementState) ReadOutcomeMarker() (Outcome, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.outcome, shutdown.Load() != 0
}

// ResolveNone resolves a none-outcome state in place under the state's
// synchronization by the shared cause rule: the transport context is
// observed inactive for a peer abort → record client (first-cause rule);
// inactive as a shutdown drain (isShutdownDrain — the process-level marker
// was published) or still active with the marker set → record service. The
// resolved outcome is returned. If the state already carries a recorded
// outcome (not none), that outcome is returned unchanged (the first claim
// wins).
func (s *SettlementState) ResolveNone(transport context.Context) Outcome {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.outcome != OutcomeNone {
		return s.outcome
	}
	if transport.Err() != nil {
		if isShutdownDrain(transport) {
			s.outcome = OutcomeService
			return OutcomeService
		}
		s.outcome = OutcomeClient
		return OutcomeClient
	}
	if shutdown.Load() != 0 {
		s.outcome = OutcomeService
		return OutcomeService
	}
	return OutcomeNone
}

// SettleAndRecord performs the fire-path settlement step under the state's
// synchronization: if an outcome is already recorded, it returns that
// outcome (the fire path releases; a cancel never settles a request that
// is already settled). Otherwise it observes the transport context:
// inactive for a peer abort → record client (first-cause rule) and return
// client; inactive as a shutdown drain (isShutdownDrain — the process-level
// marker was published) or still active → record service and return
// service. The caller (the fire path) cancels the effective context after
// this returns; the cancel itself never records or changes an outcome.
func (s *SettlementState) SettleAndRecord(transport context.Context) Outcome {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.outcome != OutcomeNone {
		return s.outcome
	}
	if transport.Err() != nil && !isShutdownDrain(transport) {
		s.outcome = OutcomeClient
		return OutcomeClient
	}
	s.outcome = OutcomeService
	return OutcomeService
}

// AuthorizeCommit evaluates the settlement rule for one commit candidate
// under the state's synchronization and returns (outcome, status, ok).
// It is the settlement primitive the response gate uses:
//
//   - outcome is the state's recorded outcome (or the resolved outcome
//     if the state was none and a claim was made);
//   - status is the status the commit should carry (0 when rejected);
//   - ok is true only when the commit may proceed.
//
// The rule (the first-commit settlement rule, package 4 section 2):
//
//   - response outcome → ok=true, status=0 (a subsequent write; the
//     first status is unreplaceable; the gate uses its own recorded
//     status);
//   - client outcome → ok=false, status=0;
//   - service outcome → ok=true only if status==503, else ok=false;
//   - none outcome → observe the transport: inactive for a peer abort →
//     record client, ok=false; inactive as a shutdown drain (isShutdownDrain)
//     → ok=true only if status==503 (record response), else ok=false; active
//   - marker → ok=true only if status==503 (record response); active, no
//     marker → record response, ok=true.
func (s *SettlementState) AuthorizeCommit(transport context.Context, status int) (Outcome, int, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch s.outcome {
	case OutcomeResponse:
		return OutcomeResponse, 0, true
	case OutcomeClient:
		return OutcomeClient, 0, false
	case OutcomeService:
		if status == 503 {
			return OutcomeService, status, true
		}
		return OutcomeService, 0, false
	default: // OutcomeNone
		if transport.Err() != nil {
			if isShutdownDrain(transport) {
				// A shutdown drain (the marker was published): the commit
				// is the centralized cancellation response, and only 503
				// is authorized (record response on the commit).
				if status == 503 {
					s.outcome = OutcomeResponse
					return OutcomeResponse, status, true
				}
				return OutcomeNone, 0, false
			}
			s.outcome = OutcomeClient
			return OutcomeClient, 0, false
		}
		if shutdown.Load() != 0 {
			if status == 503 {
				s.outcome = OutcomeResponse
				return OutcomeResponse, status, true
			}
			return OutcomeNone, 0, false
		}
		s.outcome = OutcomeResponse
		return OutcomeResponse, status, true
	}
}

// isShutdownDrain reports whether an inactive transport context was closed
// by a bounded shutdown (the process-level shutdown marker was published)
// rather than by a peer abort. It is true when the transport context is
// inactive, the process-level shutdown marker is set, and the transport
// error (or something it wraps) is a transport-drain error. A transport
// context without a drain error is always a peer abort, whatever the marker
// says (the marker alone does not turn a client abort into a drain). The
// first-cause rule it feeds into: a drain never reclassifies a request as
// client (finding F-5).
func isShutdownDrain(transport context.Context) bool {
	if transport.Err() == nil || shutdown.Load() == 0 {
		return false
	}
	for err := transport.Err(); err != nil; {
		if _, ok := err.(transportDrainError); ok {
			return true
		}
		w, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = w.Unwrap()
	}
	return false
}

// transportDrainError is the interface a transport-drain error implements.
// The testseam package's ErrTransport (internal/api) implements it; a
// production drain implementation would implement it the same way.
type transportDrainError interface {
	error
	// TransportDrain marks the error as a transport drain (a bounded
	// shutdown closing the transport, not a peer abort).
	TransportDrain()
}

type settlementKey struct{}

// WithSettlement returns a context carrying the request's settlement state.
// A context without one (background, or a caller that has not yet built its
// request state) settles markerless: no outcome, marker as set.
func WithSettlement(ctx context.Context, st *SettlementState) context.Context {
	return context.WithValue(ctx, settlementKey{}, st)
}

func stateOf(ctx context.Context) *SettlementState {
	st, _ := ctx.Value(settlementKey{}).(*SettlementState)
	return st
}

// SettlementOf returns the settlement state carried by the context, or nil
// when none is carried (a context outside the access-logging stage). The
// package-4 deadline stage reads it from the request context.
func SettlementOf(ctx context.Context) *SettlementState {
	return stateOf(ctx)
}

// shutdown is the process-level shutdown marker (atomic uint32): published
// by the lifecycle before the serve root is canceled (package 4). It is the
// fallback the shared cause rule uses when a request has no recorded
// outcome. It never reclassifies a request whose per-request outcome
// already won.
var shutdown atomic.Uint32

// MarkShutdown publishes the process-level shutdown marker. It is a no-op
// after the first publication.
func MarkShutdown() { shutdown.CompareAndSwap(0, 1) }

// ShutdownMarked reports whether the process-level shutdown marker is set.
func ShutdownMarked() bool { return shutdown.Load() != 0 }

// resetShutdown clears the process-level shutdown marker. It is the
// production-free reset used by the testseam build-tag seam
// (ClearShutdownMarker) for test isolation: the marker is process-global,
// and a test that publishes it must clear it so later tests in the same
// process settle markerless.
func resetShutdown() { shutdown.Store(0) }

// WithAppContext runs fn inside the application-scoped transaction.
// Sequence (normative): acquire a pool connection (bounded acquisition) →
// BEGIN → set_config('vector.application_id', $appID, true) as the FIRST
// statement (transaction-local; the UUID is a bound parameter) → fn(tx) →
// COMMIT (or ROLLBACK on fn error). The context deadline bounds every pgx
// call; cancellation rolls the transaction back and releases the lease.
//
// appID must be a valid UUID string (the authenticated application
// identity, attached by authentication); an invalid value makes
// set_config raise an invalid input error, which the helper settles as
// unavailable.
func WithAppContext(ctx context.Context, pool *pgxpool.Pool, appID string, fn func(tx pgx.Tx) error) error {
	return runTx(ctx, poolFactory{pool: pool}, true, appID, fn)
}

// WithShortTx runs fn inside a context-less short transaction: acquire →
// BEGIN → fn(tx) → COMMIT (or ROLLBACK on fn error). No set_config is
// executed, so no application context exists on the transaction: it is for
// paths that touch no RLS-gated table as a caller.
func WithShortTx(ctx context.Context, pool *pgxpool.Pool, fn func(tx pgx.Tx) error) error {
	return runTx(ctx, poolFactory{pool: pool}, false, "", fn)
}

// txFactory is the private transaction seam of the two helpers: it bounds
// the helper's access to the pool to bounded acquisition and BEGIN, which
// is the complete surface the lifecycle exercises. Production always runs
// the real pool factory; the unit matrix (package 2, Validation: "helper
// control flow against a stubbed transaction seam") substitutes a scripted
// one to exercise the control flow — every exit path — without a database
// (RLS and transaction-scoped set_config themselves are integration
// territory, never mocked).
type txFactory interface {
	// begin acquires a pool connection (bounded acquisition) and begins a
	// transaction on it, returning the transaction and a release function
	// that returns the connection to the pool. The release function is
	// called on every exit path of runTx (the bounded shutdown invariant).
	begin(ctx context.Context) (tx pgx.Tx, release func(), err error)
}

type poolFactory struct{ pool *pgxpool.Pool }

func (f poolFactory) begin(ctx context.Context) (pgx.Tx, func(), error) {
	conn, err := f.pool.Acquire(ctx)
	if err != nil {
		return nil, nil, err
	}
	release := func() { conn.Release() }
	tx, err := conn.Begin(ctx)
	if err != nil {
		conn.Release()
		return nil, nil, err
	}
	return tx, release, nil
}

// runTx is the shared transaction lifecycle of the two helpers.
//
// The helper owns every step of the lifecycle directly, through the
// transaction seam (in production: the pool's bounded acquisition and the
// transaction pgx returns):
//
//  1. acquire a pool connection (bounded acquisition; the pool bounds its
//     own connections and waiters) and BEGIN. Failure while the context is
//     no longer active settles by the shared cause rule; otherwise the
//     typed unavailable outcome. The acquired lease is released on EVERY
//     exit path (the bounded shutdown, package 4, assumes no leaked
//     leases).
//  2. (WithAppContext only) set_config('vector.application_id', $uuid,
//     true) as the FIRST statement of the transaction — transaction-local
//     (the third argument stays true; the UUID is a bound parameter). This
//     is what the forced RLS policies read (migrations 0001).
//  3. the callback (namespace resolution and all business SQL run here).
//  4. COMMIT (or ROLLBACK on any failure).
//
// Failure semantics at every point, in order: (1) the effective context is
// no longer active → Settle (the shared cause rule); (2) any other
// infrastructure failure (bounded-acquisition, BEGIN, set_config, COMMIT,
// ROLLBACK) → the typed unavailable outcome (503). A business error from
// the callback passes through unchanged (classified exactly once at the
// handler boundary, package 4). A ROLLBACK failure is warn-logged and
// attached to the original failure via rollbackFailure — it never replaces
// it.
func runTx(ctx context.Context, f txFactory, appScope bool, appID string, fn func(tx pgx.Tx) error) error {
	// Step 1: bounded acquisition and BEGIN. Cancellation of the request
	// context propagates into both.
	tx, release, err := f.begin(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return Settle(ctx)
		}
		return &Unavailable{Cause: err}
	}
	// Every exit path below releases the lease. The connection returns to
	// the pool even when the request was canceled: a canceled request
	// still releases its lease (bounded shutdown invariant), and the
	// transaction-local set_config cannot leak across requests (it is
	// transaction-scoped, not session-scoped).
	defer release()

	var (
		fnErr     error // the business error, passed through unchanged
		infraErr  error // a helper-boundary failure (settle / unavailable)
		committed bool
	)
	func() {
		// Step 4 (failure path): ROLLBACK on every exit that has not
		// committed. The request context is passed: a canceled request
		// must still be settled by the shared cause rule (Settle), and pgx
		// performs the rollback on any error, so the cleanup is never
		// suppressed. A rollback failure is warn-logged and attached to
		// the original failure via rollbackFailure — it never replaces it
		// (the business error's identity and stable rendering stand; the
		// rollback cause is attached for diagnostics).
		defer func() {
			if committed {
				return
			}
			if rbErr := tx.Rollback(ctx); rbErr != nil {
				logCleanupFailure(appScope, ctx)
				if fnErr != nil {
					// Business error + failed cleanup: the original
					// business error stands; the cleanup failure is
					// attached for diagnostics.
					fnErr = &rollbackFailure{original: fnErr, rollback: rbErr}
				} else if infraErr != nil {
					infraErr = &rollbackFailure{original: infraErr, rollback: rbErr}
				} else {
					// A rollback failure with no preceding failure: a
					// cleanup-only infrastructure failure.
					infraErr = &rollbackFailure{original: nil, rollback: rbErr}
				}
			}
		}()

		// Step 2 (WithAppContext only): the FIRST statement of every
		// application-scoped transaction is the transaction-local
		// application context. The third argument stays true
		// (transaction-local, not session-persistent, on pooled
		// connections); the UUID is a bound parameter, never
		// interpolated.
		if appScope {
			if _, err := tx.Exec(ctx, setConfigSQL, appID); err != nil {
				if ctx.Err() != nil {
					infraErr = Settle(ctx)
					return
				}
				infraErr = &Unavailable{Cause: err}
				return
			}
		}

		// Step 3: the callback.
		if fnErr = fn(tx); fnErr != nil {
			// Business errors pass through unchanged. The deferred
			// rollback runs here.
			return
		}

		// Step 4 (success path): COMMIT.
		if cErr := tx.Commit(ctx); cErr != nil {
			if ctx.Err() != nil {
				infraErr = Settle(ctx)
				return
			}
			infraErr = &Unavailable{Cause: cErr}
			return
		}
		committed = true
	}()

	if fnErr != nil {
		return fnErr
	}
	if infraErr != nil {
		return infraErr
	}
	return nil
}

// rollbackFailure is the attached representation of a ROLLBACK (or
// other cleanup) failure. It preserves the original failure it is
// attached to (Unwrap returns it, so errors.Is matches the original
// outcome — a business error keeps its identity, an Unavailable keeps its
// 503 message), carries the rollback cause for diagnostics, and renders
// the stable original message: the helper's stable outcomes and
// unclassified business errors never have their external rendering
// changed by a cleanup failure (raw PostgreSQL text never reaches a
// response).
type rollbackFailure struct {
	original error // nil only when there was no original failure
	rollback error
}

func (e *rollbackFailure) Error() string {
	if e.original != nil {
		return e.original.Error()
	}
	return "transaction rollback failed"
}

func (e *rollbackFailure) Unwrap() []error {
	var out []error
	if e.original != nil {
		out = append(out, e.original)
	}
	if e.rollback != nil {
		out = append(out, e.rollback)
	}
	return out
}

// Original returns the failure the rollback failure is attached to, or
// nil when it stands alone (a failed cleanup with no preceding failure).
func (e *rollbackFailure) Original() error { return e.original }

// RollbackCause returns the rollback/cleanup failure that was attached.
func (e *rollbackFailure) RollbackCause() error { return e.rollback }

// setConfigSQL is the transaction-local application-context statement: the
// FIRST statement of every WithAppContext transaction. The third argument
// is true (transaction-local, per SECURITY.md and AGENTS.md); the UUID is a
// bound parameter, never interpolated.
const setConfigSQL = `SELECT set_config('vector.application_id', $1, true)`

// txOperation renders the operation label for diagnostic logging. It
// carries no identity.
func txOperation(appScope bool) string {
	if appScope {
		return "with_app_context"
	}
	return "with_short_tx"
}

// ---------------------------------------------------------------------------
// Cleanup-failure logging (the diagnostic safe allowlist)
// ---------------------------------------------------------------------------

// Cleanup-failure record policy.
//
// When a ROLLBACK (or other cleanup) fails the helper emits exactly one
// structured slog.Warn record. That record carries only fields from this
// closed set: the event name, the operation label (with_app_context /
// with_short_tx), the request ID (when the effective context carries one),
// and — only when explicitly allowlisted — a bounded, server-authored
// cleanup-cause class (see cleanupCauseAllowlist).
//
// The record never carries the rollback/cleanup failure's raw text — not
// Error(), not a pgconn.PgError's primary message, Detail, Hint, or any
// caller line. The cause is reduced to a server-authored class from the
// allowlist; everything else about it is dropped. This mirrors the
// package-4 diagnostic-record policy (the safe allowlist): suppression is
// at the log boundary only — the failure is still attached to the original
// outcome for classification (rollbackFailure), and the response keeps its
// stable catalog code and message.

// cleanupCauseEntry is one row of the cleanup-cause allowlist. Each entry
// maps a class name to the bounded server-authored text it may carry. The
// set is closed: adding an entry is a reviewed decision, not an
// implementation convenience.
type cleanupCauseEntry struct {
	name  string // the class name (the slog field value)
	bound int    // the length bound in bytes for the class value
}

// cleanupCauseAllowlist is the closed set of server-authored cleanup-cause
// classes the cleanup-failure record may carry. It is the single source of
// truth for the class text: buildCleanupCauseRecord reads it, and the test
// seam (RegisterCleanupCause) mutates it only to exercise the mechanism.
// Production code never mutates the map after startup.
var cleanupCauseAllowlist = map[string]cleanupCauseEntry{
	// "cancelled" is the class for a canceled or deadline context: it is
	// server-authored, bounded, and carries no caller data.
	"cancelled": {name: "cancelled", bound: 9},
	// "unspecified" is the class for every other cleanup cause (including
	// any *pgconn.PgError): it is a bounded server-authored token that
	// carries no caller data and no raw driver text.
	"unspecified": {name: "unspecified", bound: 11},
}

// RegisterCleanupCause is a test seam: it registers a synthetic
// cleanup-cause class so a test can exercise the allowlist mechanism
// (a non-allowlisted cause class is dropped, an over-long class value is
// truncated). It returns a function that removes the entry. Production
// code does not call it.
func RegisterCleanupCause(name string, bound int) (unregister func()) {
	cleanupCauseAllowlist[name] = cleanupCauseEntry{name: name, bound: bound}
	return func() { delete(cleanupCauseAllowlist, name) }
}

// cleanupCauseClass reduces a cleanup failure to its server-authored class.
// The result is one of the allowlisted class names: the class of a canceled
// or deadline context is "cancelled"; every other cause (including any
// *pgconn.PgError with its primary/detail/hint/caller text) is the class
// "unspecified". The raw error is never returned, rendered, or otherwise
// inspected for its text.
func cleanupCauseClass(err error) string {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return "cancelled"
	}
	return "unspecified"
}

// buildCleanupCauseRecord assembles the cleanup-failure record's fields.
// The operation label and the request ID are read from their sources; the
// cleanup-cause class is the allowlisted, bounded, server-authored value.
// No raw rollback/cleanup text (Error, pgconn.PgError primary/detail/hint,
// or caller line) is part of the record.
func buildCleanupCauseRecord(appScope bool, ctx context.Context) (operation, requestID, cause string) {
	operation = txOperation(appScope)
	requestID = RequestID(ctx)
	cause = cleanupCauseClass(ctx.Err())
	// The class value is bounded to its allowlisted length. The class is a
	// server-authored token, so it is never over the bound; the truncation
	// is the invariant the mechanism guarantees.
	if e, ok := cleanupCauseAllowlist[cause]; ok {
		if len(cause) > e.bound {
			cause = cause[:e.bound]
		}
	} else {
		// Not on the allowlist: dropped, not truncated.
		cause = ""
	}
	return operation, requestID, cause
}

// logCleanupFailure writes the single structured log record for a ROLLBACK
// (or other cleanup) failure (slog.Warn). It is called at most once per
// cleanup failure, by runTx. The record carries only the safe allowlist
// fields built by buildCleanupCauseRecord — never the failure's raw text.
func logCleanupFailure(appScope bool, ctx context.Context) {
	operation, requestID, cause := buildCleanupCauseRecord(appScope, ctx)
	var attrs []slog.Attr
	attrs = append(attrs,
		slog.String("event", "db_cleanup_failure"),
		slog.String("operation", operation),
	)
	if requestID != "" {
		attrs = append(attrs, slog.String("request_id", requestID))
	}
	if cause != "" {
		attrs = append(attrs, slog.String("cause", cause))
	}
	args := make([]any, len(attrs))
	for i, a := range attrs {
		args[i] = a
	}
	slog.Warn("transaction cleanup failed", args...)
}

// RequestID is an operational, non-sensitive request identifier for
// structured logs (overview logging fields). NewRequestID returns a random
// 16-byte, 32-character hex identifier.
func NewRequestID() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("dbctx: generate request id: %w", err)
	}
	return hex.EncodeToString(buf), nil
}

type requestIDKey struct{}

// WithRequestID returns a context carrying the request identifier.
func WithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, requestIDKey{}, id)
}

// RequestID returns the request identifier carried by the context, or "".
func RequestID(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey{}).(string)
	return id
}

type appIDKey struct{}

// WithAppID returns a context carrying the authenticated application
// identity (the application UUID). It is the only source of application
// identity for data-plane handlers (overview invariant 1).
func WithAppID(ctx context.Context, appID string) context.Context {
	return context.WithValue(ctx, appIDKey{}, appID)
}

// AppID returns the application identity carried by the context, or "".
func AppID(ctx context.Context) string {
	id, _ := ctx.Value(appIDKey{}).(string)
	return id
}
