package api

import (
	"context"
	"errors"
	"net/http"
	"sync"

	"vector-service/internal/dbctx"
)

// errCommitRejected is the error a rejected first commit (or a re-evaluated
// write on a still-uncommitted writer) returns to the caller. A handler
// that checks its writes converts it through Classify (the inactive context
// settles by the shared cause rule) and returns it as a typed error,
// exactly like any other failure.
var errCommitRejected = errors.New("response: commit rejected by settlement")

// responseGate is the access-logging recorder and the response gate
// (implementation package 4, section 2). It is the only http.ResponseWriter
// the inner chain ever sees: the access-logging stage wraps w exactly once,
// and that single recorder is also the gate that enforces the settlement
// rule at the first status commit.
//
// It remains the access line's status source — the FIRST committed status,
// or 0 when none was committed — and, in addition, enforces the first-cause
// settlement rule at the first commit, under the settlement state's
// synchronization:
//
//   - a status was already committed (response outcome) → the call is a
//     subsequent write: pass through; the first status is unreplaceable;
//   - the outcome is client, or the transport context is observed inactive
//     under the gate's synchronization (first-cause rule: in the none case,
//     record the client outcome before rejecting) → the commit is rejected:
//     no response at all is the client outcome, and nothing — including an
//     error body or the cancellation 503 — may be written to a transport
//     whose closure is already known;
//   - the outcome is service, or the outcome is none with the shutdown
//     marker set (transport active, by the previous rule) → the commit is
//     accepted ONLY if the status is 503: by the handler contract only the
//     renderer writes 503, so this authorizes exactly the centralized
//     cancellation response. Any other status — a handler success write
//     after the deadline fired or shutdown began — is rejected;
//   - otherwise (no outcome, no marker, transport active) → claim response
//     with the commit's status and write it (the success commit, or the
//     first-rendered error status while the request is still open).
//
// A rejected commit records no status, delivers 0 bytes, and returns an
// error. Writes after a rejected first commit are re-evaluated as first
// commits and stay rejected under a winning cancellation, so no partial
// body can leak. The gate never writes response bytes itself; it only
// authorizes or rejects the first commit.
type responseGate struct {
	http.ResponseWriter

	// st is the request's synchronized settlement state.
	st *dbctx.SettlementState
	// transport is the transport request context (r.Context()), observed
	// synchronously under the state's synchronization at the first commit
	// (first-cause rule).
	transport context.Context

	mu      sync.Mutex
	commit  int // 0 = nothing committed yet; otherwise the first committed status
	deliver bool
}

// Status returns the first committed status, or 0 when none was committed.
// A later disconnect never retroactively changes it: commit is not proof of
// delivery, and delivery is not asserted.
func (g *responseGate) Status() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.commit
}

// authorize evaluates the settlement rule for one commit candidate under the
// settlement state's synchronization and returns (status, ok): ok is true
// only when the commit may proceed. It delegates to the settlement state's
// AuthorizeCommit, which records the response claim on an authorized first
// commit and records the client outcome (first-cause rule) when rejecting
// an uncommitted request whose transport is observed inactive.
func (g *responseGate) authorize(status int) (int, bool) {
	_, s, ok := g.st.AuthorizeCommit(g.transport, status)
	return s, ok
}

// firstCommit is the common evaluation of the first status commit (a
// WriteHeader or a Write implying 200) for a still-uncommitted writer.
// On authorization it marks the status committed and allows the bytes.
// On rejection it marks delivery disabled so the write returns 0 with an
// error and no bytes reach the connection.
func (g *responseGate) firstCommit(status int) (bool, int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.commit != 0 {
		// A status was already committed (an explicit WriteHeader or an
		// earlier Write): the call is a subsequent write.
		return g.deliver, g.commit
	}
	s, ok := g.authorize(status)
	if !ok {
		g.deliver = false
		return false, 0
	}
	g.commit = s
	g.deliver = true
	return true, s
}

// WriteHeader implements http.ResponseWriter with the first-commit rule:
// the first WriteHeader is a first commit.
func (g *responseGate) WriteHeader(status int) {
	ok, s := g.firstCommit(status)
	if !ok {
		// Rejected first commit: no status is recorded, no bytes reach the
		// connection. (The net/http layer will still deliver its own 200
		// when the request completes without any committed status; the
		// settlement invariant is carried by the gate, not by the layer.)
		return
	}
	g.ResponseWriter.WriteHeader(s)
}

// Write implements http.ResponseWriter: the first Write is a first commit
// implying 200.
func (g *responseGate) Write(b []byte) (int, error) {
	if !g.deliver {
		ok, _ := g.firstCommit(http.StatusOK)
		if !ok {
			return 0, errCommitRejected
		}
	}
	return g.ResponseWriter.Write(b)
}
