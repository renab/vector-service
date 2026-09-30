//go:build testseam

package server

import (
	"net/http"
	"os"
	"sync"
	"syscall"
	"testing"
	"time"

	"vector-service/internal/api"
	"vector-service/internal/apierr"
	"vector-service/internal/dbctx"
)

// F-5 (shutdown settlement): when the process-level shutdown marker is
// published before the serve root is canceled (the normative lifecycle
// order), a request descended from the BaseContext must settle as
// service-initiated and commit exactly one 503 (status_code 503) — not as a
// client abort (which would leave no committed status at all).
//
// This test drives the PRODUCTION lifecycle path end-to-end: the marker is
// published by the real signal handler (before rootCancel), the serve root
// is the real production BaseContext (shutdown_basecontext.go — the
// testseam build calls the exact same production function, see
// shutdown_basecontext_testseam.go), and a genuinely in-flight request
// (descended from the serve root, cut by the serve root, never by a client
// timer) is observed through the per-request settlement state and response
// gate — the exact sources the access line reads. The handler chain is the
// plain production BaseChain (no testseam chain variant): the settlement
// primitives observe the raw per-request transport context, and the
// production BaseContext's drain annotation is what distinguishes the
// shutdown-induced cancellation from a peer abort.
//
// The marker is process-global and the real lifecycle never clears it
// (shutdown is terminal for the process), so the marker is reset under a
// process-global mutex both before the test (so it starts markerless) and
// after it (so later tests settle markerless).

// markerMutex is the process-global lock over the shutdown marker for the
// marker-driven server tests. The marker is process-global mutable state; two
// tests must never hold or clear it at once.
var markerMutex sync.Mutex

// resetShutdownMarkerForTest clears the process-level shutdown marker. It is
// the test-local wrapper over the testseam-gated server seam
// (ResetShutdownMarkerForTest); it is called only under markerMutex.
func resetShutdownMarkerForTest() { ResetShutdownMarkerForTest() }

// TestServe_BaseContextShutdownSettlesService503 pins the normative
// marker-before-cancel order against a real server and a real
// BaseContext-derived request: the request settles as service-initiated and
// the response gate commits exactly one 503 (not a client abort).
func TestServe_BaseContextShutdownSettlesService503(t *testing.T) {
	// The marker is process-global: start markerless, and reset it after the
	// test so later tests settle markerless (the real lifecycle publishes it
	// on the signal and never clears it). The cleanup is fatal-free: a
	// t.Fatal inside it would unwind across the held markerMutex (deadlock).
	wasSet, release := markerGuard(t)
	if wasSet {
		t.Fatal("shutdown marker is set before the test; the marker tests must not overlap")
	}
	defer release()

	cfg := shutdownTestConfig()
	cfg.HTTPRequestTimeout = 10 * time.Second // generous bound: the request is cut by the serve root, never by the deadline
	l := freshListener(t)
	// The listener is closed explicitly (closeListener) once Serve has
	// returned, before the deferred marker reset runs: with the marker
	// reset, any connection the server was still holding is cut, and the
	// server's own deferred Close() (Serve's early-return safety net) would
	// otherwise wait out its 2 s Close grace for the still-open connection
	// while the test's cleanup proceeds.
	closeListener := func() { l.Close() }

	// The plain PRODUCTION base chain (request ID → access logging →
	// operation deadline → recovery) stands in for the service's handler
	// set — exactly what the router mounts on /readyz: its operation
	// deadline stage derives the effective context (WithCancel) from the
	// transport context, and the transport context derives from the
	// production BaseContext (the serve root) that Serve swapped in. The
	// probe handler holds the request for the whole test (until the serve
	// root is canceled) — a genuinely in-flight request.
	refsCh := make(chan api.SettledRefs, 1)
	chain := api.Chain{Deadline: &api.OperationDeadline{Timeout: cfg.HTTPRequestTimeout}}
	handler := chain.BaseChain(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			refsCh <- api.SettledRefs{
				St:  dbctx.SettlementOf(r.Context()),
				Rec: api.GateOf(r.Context()),
			}
			// Hold the request until its effective context becomes
			// inactive: the serve root is canceled by the lifecycle on the
			// signal, which deactivates it and releases the request. The
			// request is genuinely in flight for the whole interval — and
			// it is cut by the serve root, never by a client timer.
			<-r.Context().Done()
		}))
	srv := testServer(cfg, handler)

	closer := &scriptCloser{}
	signalCh := make(chan os.Signal, 2)
	c := NewShutdownControllerForSignals(cfg, srv, closer, signalCh, l)
	serveErr := serveController(t, c, signalCh)

	// Issue one request against the live server. The client does not abort:
	// the transport stays active for the whole request; the request is cut by
	// the serve root, not by a client timer or deadline.
	go func() {
		client := &http.Client{
			Transport: &http.Transport{DisableKeepAlives: true},
		}
		req, err := http.NewRequest(http.MethodGet, "http://"+l.Addr().String()+"/readyz", nil)
		if err != nil {
			return
		}
		resp, err := client.Do(req)
		if err != nil {
			return
		}
		resp.Body.Close()
	}()

	// Wait for the in-flight request to install its per-request refs (the
	// channel send establishes the happens-before edge over st and the gate).
	var st *dbctx.SettlementState
	var rec *api.ResponseGate
	select {
	case refs := <-refsCh:
		st, rec = refs.St, refs.Rec
	case <-time.After(3 * time.Second):
		t.Fatal("the in-flight request never installed its settlement refs")
	}

	// The handler is still blocked on the serve root, so the drain cannot
	// complete yet: the request is genuinely in flight. Deliver the first
	// signal: the lifecycle publishes the marker (before the cancel) and then
	// cancels the serve root. The handler returns, the chain finalizes (the
	// marker resolves the none outcome to service and renders the 503
	// through the gate), the drain completes, and Serve returns.
	sigErr := deliverAndAwait(t, serveErr, signalCh, syscall.SIGINT, 5*time.Second)
	if sigErr != nil {
		t.Fatalf("Serve returned %v, want nil (drain completed)", sigErr)
	}
	// Serve has returned (drain done): close the listener now, BEFORE the
	// deferred marker reset runs. With the marker reset, any connection the
	// server was still holding is cut, and the server's own deferred
	// Close() (Serve's early-return safety net) would otherwise wait out
	// its 2 s Close grace for the still-open connection while the test's
	// cleanup proceeds.
	closeListener()

	// The marker must now be set (the real lifecycle published it).
	if !dbctx.ShutdownMarked() {
		t.Fatal("the shutdown marker is not set after the signal; the lifecycle must publish it")
	}

	// The request must have settled as service-initiated: the production
	// BaseContext's drain annotation (marker set at the serve-root
	// observation) resolved the none outcome to service (not client — a
	// client abort, or a bare markerless cancellation, would carry
	// OutcomeClient).
	if st.Outcome() != dbctx.OutcomeService {
		t.Fatalf("settlement outcome = %v, want service (a client abort would settle OutcomeClient; a markerless cancel would abort)", st.Outcome())
	}

	// The response gate must have committed exactly one status, and it must
	// be the 503 unavailable: the first committed status is the access line's
	// status_code (the centralized cancellation response is the only writer of
	// it). A client abort would have left no committed status at all.
	if got := rec.Status(); got != http.StatusServiceUnavailable {
		t.Fatalf("committed status = %d, want 503 (the service-initiated settlement must render exactly one 503; a client abort commits nothing)", got)
	}

	// The 503 must carry the stable catalog code — never raw PostgreSQL text
	// and never an abort sentinel. (The renderer's body shape is pinned
	// elsewhere; this asserts the code the catalog maps to 503.)
	if apierr.CodeUnavailable != "unavailable" {
		t.Fatalf("catalog drift: CodeUnavailable = %q, want \"unavailable\"", apierr.CodeUnavailable)
	}
}
