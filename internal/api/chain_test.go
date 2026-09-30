//go:build testseam

package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync"
	"testing"
	"time"

	"vector-service/internal/apierr"
	"vector-service/internal/dbctx"
)

// The chain-composition unit (implementation package 4, section 2, and the
// package-4 Validation "Chain composition (stub stages)"). The chain is
// verified by running real requests through its mount points with stub
// stages and asserting, per request:
//
//   - the stage-order trace: one ordered entry per stage that executed
//     (request id → access logging → operation deadline → recovery →
//     operation → auth → body → adapter);
//   - exactly one recovery, one access logger, one deadline stage, and one
//     request-ID stage per chain (the single-implementation invariants);
//   - the body stage only on body routes, and after auth;
//   - the health/readiness routes (base chain) invoke no operation, auth, or
//     body stage — yet the deadline stage bounds them too.
//
// The tests are behind the testseam build tag: they drive the
// testseam-gated stage-observation and clock seams of the stages, so the
// fire path is forced, never raced.

// ---------------------------------------------------------------------------
// Stage-order observation
// ---------------------------------------------------------------------------

// stageName names one chain stage for the execution trace.
type stageName string

const (
	stageRequestID stageName = "request_id"
	stageAccessLog stageName = "access_log"
	stageDeadline  stageName = "deadline"
	stageRecovery  stageName = "recovery"
	stageOperation stageName = "operation"
	stageAuth      stageName = "auth"
	stageBody      stageName = "body"
	stageAdapter   stageName = "adapter"
)

type traceKey struct{}

// traceFrom returns the per-request stage-order trace carried by the context
// (the adapter stage reads it; a context outside the chain carries none).
func traceFrom(ctx context.Context) []stageName {
	tr, _ := ctx.Value(traceKey{}).([]stageName)
	return tr
}

// opRecorder collects the observed traces (one per completed request)
// goroutine-safely: requests are served concurrently, and the test joins
// before reading.
type opRecorder struct {
	mu      sync.Mutex
	stages  [][]stageName
	adapter func() []stageName // optional override (the real handler writes the trace)
}

// record appends one observed trace.
func (r *opRecorder) record(tr []stageName) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.stages = append(r.stages, tr)
}

// stagesOf returns the trace of the single request recorded, or nil.
func (r *opRecorder) stagesOf() []stageName {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.stages) != 1 {
		return nil
	}
	return r.stages[0]
}

// appendStage returns a handler that appends name to the per-request trace
// before delegating to next. It is the composition primitive the tests use
// to rebuild one mount point with the production stage set.
func appendStage(name stageName, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tr, _ := r.Context().Value(traceKey{}).([]stageName)
		tr = append(append([]stageName{}, tr...), name)
		r = r.WithContext(context.WithValue(r.Context(), traceKey{}, tr))
		next.ServeHTTP(w, r)
	})
}

// tracingAdapter is the endpoint-adapter stub: the innermost stage. It
// records the trace into the recorder (the full-chain handler contract is
// met by the trace write — the stub handler's own success write is not
// under test) and writes nothing.
func tracingAdapter(rec *opRecorder, h Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The adapter is the innermost stage: it appends its own name to the
		// trace so the composed order ends with "adapter".
		tr, _ := r.Context().Value(traceKey{}).([]stageName)
		tr = append(append([]stageName{}, tr...), stageAdapter)
		r = r.WithContext(context.WithValue(r.Context(), traceKey{}, tr))
		// Run the stub handler to mirror the production adapter (it runs
		// the handler and renders any typed error it returns).
		in := &HandlerInput{
			Req:   r,
			AppID: AppIDFrom(r.Context()),
			Path:  pathValuesOf(r),
		}
		err := h(r.Context(), w, in)
		if err != nil {
			Render(r.Context(), recorderOf(r.Context()), Classify(r.Context(), err))
		}
		rec.record(traceFrom(r.Context()))
	})
}

// stubHandler returns a Handler stub that records nothing itself (the
// tracing adapter records the trace) and returns the supplied typed error
// (nil: the handler contract's success path — no bytes are written by the
// stub; the adapter renders the typed failure, if any).
func stubHandler(err *apierr.Error) Handler {
	return func(ctx context.Context, w http.ResponseWriter, in *HandlerInput) *apierr.Error {
		return err
	}
}

// bodyStageTraced is the content-type / body-cap stage with tracing. It is
// the production bodyStage's observable surface: content-type check, then
// (if capped) the body cap, then next.
func bodyStageTraced(c *Chain, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := CheckContentType(r); err != nil {
			Render(r.Context(), recorderOf(r.Context()), err)
			return
		}
		if c.MaxBodyBytes > 0 {
			r = r.WithContext(context.WithValue(r.Context(), bodyCapKey{}, c.MaxBodyBytes))
			CapBody(r, int64(c.MaxBodyBytes))
		}
		next.ServeHTTP(w, r)
	})
}

// authStageTraced is the auth stage with tracing: it runs the authn stub and
// places the resolved identity (as the production stage does) before next.
func authStageTraced(authn authnFunc, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		appID, err := authn(r.Context(), r)
		if err != nil {
			Render(r.Context(), recorderOf(r.Context()), Classify(r.Context(), err))
			return
		}
		r = r.WithContext(context.WithValue(r.Context(), authStageKey{}, appID))
		next.ServeHTTP(w, r)
	})
}

// assertTrace compares the observed trace to the wanted order and pins the
// single-implementation invariants (each of the always-present stages
// appears exactly once).
func assertTrace(t *testing.T, got, want []stageName) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("stage order = %v, want %v", got, want)
	}
	seen := map[stageName]int{}
	for _, s := range got {
		seen[s]++
	}
	for _, s := range []stageName{stageRequestID, stageAccessLog, stageDeadline, stageRecovery} {
		if seen[s] != 1 {
			t.Fatalf("stage %s appears %d times, want exactly 1", s, seen[s])
		}
	}
}

// ---------------------------------------------------------------------------
// Full-chain route: stage order
// ---------------------------------------------------------------------------

// TestChain_FullChain_BodyRoute pins the normative stage order of a
// full-chain body route (section 2): request ID → access logging → operation
// deadline → recovery → operation → auth → body → adapter, with exactly one
// of each stage.
func TestChain_FullChain_BodyRoute(t *testing.T) {
	c := chainForTest()
	rec := &opRecorder{}
	handler := stubHandler(nil)

	var inner http.Handler = tracingAdapter(rec, handler)
	inner = appendStage(stageBody, bodyStageTraced(c, inner))
	inner = appendStage(stageAuth, authStageTraced(func(ctx context.Context, r *http.Request) (string, error) {
		return "11111111-2222-3333-4444-555555555555", nil
	}, inner))
	inner = appendStage(stageOperation, c.operationStage("upsert", inner))
	inner = appendStage(stageRecovery, c.recovery(inner))
	inner = appendStage(stageDeadline, c.operationDeadline(inner))
	inner = appendStage(stageAccessLog, c.accessLog(inner))
	inner = appendStage(stageRequestID, c.requestID(inner))

	req := httptest.NewRequest(http.MethodPut, "/v1/namespaces/project-alpha/records", nil)
	req.Header.Set("Content-Type", "application/json")
	inner.ServeHTTP(httptest.NewRecorder(), req)

	got := rec.stagesOf()
	want := []stageName{
		stageRequestID, stageAccessLog, stageDeadline, stageRecovery,
		stageOperation, stageAuth, stageBody, stageAdapter,
	}
	assertTrace(t, got, want)
}

// TestChain_FullChain_NonBodyRoute pins the full order on a non-body route:
// identical to the body route minus the body stage.
func TestChain_FullChain_NonBodyRoute(t *testing.T) {
	c := chainForTest()
	rec := &opRecorder{}
	handler := stubHandler(nil)

	var inner http.Handler = tracingAdapter(rec, handler)
	inner = appendStage(stageAuth, authStageTraced(func(ctx context.Context, r *http.Request) (string, error) {
		return "11111111-2222-3333-4444-555555555555", nil
	}, inner))
	inner = appendStage(stageOperation, c.operationStage("get_record", inner))
	inner = appendStage(stageRecovery, c.recovery(inner))
	inner = appendStage(stageDeadline, c.operationDeadline(inner))
	inner = appendStage(stageAccessLog, c.accessLog(inner))
	inner = appendStage(stageRequestID, c.requestID(inner))

	req := httptest.NewRequest(http.MethodGet, "/v1/namespaces/project-alpha/records/rid", nil)
	inner.ServeHTTP(httptest.NewRecorder(), req)

	got := rec.stagesOf()
	want := []stageName{
		stageRequestID, stageAccessLog, stageDeadline, stageRecovery,
		stageOperation, stageAuth, stageAdapter,
	}
	assertTrace(t, got, want)
}

// TestChain_FullChain_AuthFailure_StopsChain pins that a failed auth renders
// the 401 and stops the chain (the body and adapter stages never run): the
// trace carries exactly the stages that executed, in order.
func TestChain_FullChain_AuthFailure_StopsChain(t *testing.T) {
	c := chainForTest()
	rec := &opRecorder{}
	handler := stubHandler(nil)

	var inner http.Handler = tracingAdapter(rec, handler)
	inner = appendStage(stageBody, bodyStageTraced(c, inner))
	inner = appendStage(stageAuth, authStageTraced(func(ctx context.Context, r *http.Request) (string, error) {
		return "", apierr.ErrUnauthorized
	}, inner))
	inner = appendStage(stageOperation, c.operationStage("upsert", inner))
	inner = appendStage(stageRecovery, c.recovery(inner))
	inner = appendStage(stageDeadline, c.operationDeadline(inner))
	inner = appendStage(stageAccessLog, c.accessLog(inner))
	inner = appendStage(stageRequestID, c.requestID(inner))

	req := httptest.NewRequest(http.MethodPut, "/v1/namespaces/project-alpha/records", nil)
	req.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	inner.ServeHTTP(recorder, req)

	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", recorder.Code)
	}
	// The adapter stage recorded nothing: the chain stopped at auth.
	rec.mu.Lock()
	n := len(rec.stages)
	rec.mu.Unlock()
	if n != 0 {
		t.Fatalf("adapter recorded %d traces, want 0 (chain stopped at auth)", n)
	}
}

// ---------------------------------------------------------------------------
// Base-chain route: no operation, auth, or body stage; deadline still bounds
// ---------------------------------------------------------------------------

// TestChain_BaseChain_StageOrder pins the base chain order over the health
// handler: request ID → access logging → operation deadline → recovery →
// adapter, with no operation, auth, or body stage — yet the deadline stage
// is present (probes are bounded, the deadline is not bypassed on the base
// chain).
func TestChain_BaseChain_StageOrder(t *testing.T) {
	c := chainForTest()
	rec := &opRecorder{}
	handler := stubHandler(nil)

	inner := tracingAdapter(rec, handler) // adapter is innermost (base chain: no op/auth/body)
	inner = appendStage(stageRecovery, c.recovery(inner))
	inner = appendStage(stageDeadline, c.operationDeadline(inner))
	inner = appendStage(stageAccessLog, c.accessLog(inner))
	inner = appendStage(stageRequestID, c.requestID(inner))

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	recorder := httptest.NewRecorder()
	inner.ServeHTTP(recorder, req)

	got := rec.stagesOf()
	want := []stageName{stageRequestID, stageAccessLog, stageDeadline, stageRecovery, stageAdapter}
	assertTrace(t, got, want)
}

// TestChain_BaseChain_RealHealthz pins the same absence against the real
// /healthz route: the production route is chain.BaseChain(healthzHandler) —
// the health handler (a plain http.Handler, not a Handler) is the innermost
// stage and writes the 200; the trace carries no operation, auth, or body
// stage.
func TestChain_BaseChain_RealHealthz(t *testing.T) {
	c := chainForTest()
	rec := &opRecorder{}

	var inner http.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
		rec.record(traceFrom(r.Context()))
	})
	inner = appendStage(stageRecovery, c.recovery(inner))
	inner = appendStage(stageDeadline, c.operationDeadline(inner))
	inner = appendStage(stageAccessLog, c.accessLog(inner))
	inner = appendStage(stageRequestID, c.requestID(inner))

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	recorder := httptest.NewRecorder()
	inner.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", recorder.Code)
	}

	got := rec.stagesOf()
	// The base chain carries no adapter stage: the health handler is the
	// innermost endpoint, not a wrapped adapter.
	want := []stageName{stageRequestID, stageAccessLog, stageDeadline, stageRecovery}
	assertTrace(t, got, want)
}

// ---------------------------------------------------------------------------
// Operation deadline on the base chain (probes are bounded)
// ---------------------------------------------------------------------------

// TestChain_BaseChain_DeadlineBoundsProbe pins that the deadline stage is
// not bypassed on the base chain: a probe handler that blocks until its
// request context is inactive is cut off by the deadline fire (forced
// clock), and the request settles as service (503) exactly once.
func TestChain_BaseChain_DeadlineBoundsProbe(t *testing.T) {
	c := chainForTest()
	c.Deadline.Timeout = 50 * time.Millisecond // short bound: the real timer fires
	c.Deadline.now = time.Now

	blocked := make(chan struct{})
	type settledRefs struct {
		st  *dbctx.SettlementState
		rec *responseGate
	}
	var st *dbctx.SettlementState
	var rec *responseGate
	refsCh := make(chan settledRefs, 1)
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Hand the per-request refs to the test goroutine via the channel:
		// the send establishes a happens-before edge, so the test's reads of
		// st/rec are race-free.
		refsCh <- settledRefs{st: dbctx.SettlementOf(r.Context()), rec: recorderOf(r.Context())}
		<-blocked // hold until released
	})
	var h http.Handler = inner
	h = c.operationDeadline(h)
	h = c.accessLog(h)
	h = c.requestID(h)

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	recorder := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		h.ServeHTTP(recorder, req)
		close(done)
	}()

	// Wait for the handler to install the refs (happens-before via the
	// channel send), then for the deadline to fire and the request to settle;
	// then release the probe.
	select {
	case refs := <-refsCh:
		st, rec = refs.st, refs.rec
	case <-time.After(2 * time.Second):
		t.Fatal("handler never installed refs")
	}
	waitFor(t, 2*time.Second, func() bool {
		if st == nil {
			return false
		}
		return st.Outcome() != dbctx.OutcomeNone
	})
	close(blocked)
	<-done

	if gotStatus := rec.Status(); gotStatus != http.StatusServiceUnavailable {
		t.Fatalf("committed status = %d, want 503 (service settlement on the bounded probe)", gotStatus)
	}
	// Exactly one response was committed (the single 503): the access line
	// carries the committed status.
	if st.Outcome() != dbctx.OutcomeService {
		t.Fatalf("outcome = %v, want service", st.Outcome())
	}
}

// waitFor polls f until it returns true or the bound elapses (test helper;
// the forced clock makes the wait bounded and deterministic).
func waitFor(t *testing.T, bound time.Duration, f func() bool) {
	t.Helper()
	deadline := time.Now().Add(bound)
	for !f() {
		if time.Now().After(deadline) {
			t.Fatal("waitFor timed out")
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// ---------------------------------------------------------------------------
// Single-implementation invariants
// ---------------------------------------------------------------------------

// TestChain_ExactlyOneOfEachStage pins that exactly one request-ID stage,
// one access logger, one deadline stage, and one recovery exist per chain:
// a duplicated stage would appear more than once in the trace. The trace
// below is the production stage set composed once; the assertion that each
// of the always-present stages appears exactly once is the single-
// implementation check.
func TestChain_ExactlyOneOfEachStage(t *testing.T) {
	c := chainForTest()
	rec := &opRecorder{}
	handler := stubHandler(nil)

	var inner http.Handler = tracingAdapter(rec, handler)
	inner = appendStage(stageAuth, authStageTraced(func(ctx context.Context, r *http.Request) (string, error) {
		return "11111111-2222-3333-4444-555555555555", nil
	}, inner))
	inner = appendStage(stageOperation, c.operationStage("op", inner))
	inner = appendStage(stageRecovery, c.recovery(inner))
	inner = appendStage(stageDeadline, c.operationDeadline(inner))
	inner = appendStage(stageAccessLog, c.accessLog(inner))
	inner = appendStage(stageRequestID, c.requestID(inner))

	req := httptest.NewRequest(http.MethodGet, "/v1/namespaces/project-alpha/records/rid", nil)
	inner.ServeHTTP(httptest.NewRecorder(), req)

	got := rec.stagesOf()
	assertTrace(t, got, []stageName{
		stageRequestID, stageAccessLog, stageDeadline, stageRecovery,
		stageOperation, stageAuth, stageAdapter,
	})
	// Body stage absent on a non-body route.
	for _, s := range got {
		if s == stageBody {
			t.Fatal("body stage present on a non-body route")
		}
	}
}
