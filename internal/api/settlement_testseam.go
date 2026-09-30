//go:build testseam

package api

import (
	"context"

	"vector-service/internal/dbctx"
)

// The testseam-gated settlement surface: the lifecycle tests in
// internal/server need to observe one in-flight request's per-request
// settlement state and response gate through the public chain (the base
// chain's /readyz probe), which the production code never exposes. The
// seams here are consumed only by tests. No production path references them.
//
// Two seams exist:
//
//  1. Chain.BaseChainTestable (chain_testseam.go) composes the base chain
//     stages over next with the access-logging and operation-deadline
//     stages carrying a drain-aware transport (drainTransport), while next
//     (the test handler) captures the per-request settlement state and gate
//     from its request context. The SettledRefs / GateOf accessors hand the
//     captured state back to the test.
//
//  2. drainTransport (chain_testseam.go) is the test-local transport
//     context. The stdlib net/http layer closes a request's transport
//     context as part of the bounded drain (Server.Shutdown closes idle
//     connections and the drain's grace deadline closes the rest), so a
//     handler that observes its own transport context after the marker is
//     published sees it inactive — even though the client never aborted.
//     The settlement design distinguishes a shutdown drain (service) from a
//     peer abort (client) by the transport error's identity
//     (dbctx.isShutdownDrain): an inactive transport whose error is a
//     transport-drain error is a drain; anything else is a peer abort.
//     Because the stdlib never annotates the drain, the test-local
//     transport wraps the real r.Context() with a context that carries the
//     drain error (ErrTransport) once the real transport is inactive. The
//     real r.Context() is still used for every Done/Value/Deadline
//     delegation, so the request lifecycle is unchanged.

// SettledRefs carries one in-flight request's per-request settlement state
// (dbctx.SettlementState) and response gate (the access-logging recorder
// whose first committed status is the access line's status_code). The
// per-request handler hands them to the test through a buffered channel; the
// send establishes the happens-before edge over both.
type SettledRefs struct {
	St  *dbctx.SettlementState
	Rec *ResponseGate
}

// ResponseGate is the access-logging recorder / response gate. It is the
// only http.ResponseWriter the inner chain ever sees, and its first
// committed status is the access line's status_code. Exposed under the
// testseam tag so the lifecycle tests can assert the committed status of an
// in-flight request without reaching into package internals.
type ResponseGate = responseGate

// ErrTransport is a synthetic transport error the settlement package's
// transport-observation step (isShutdownDrain) can classify as a shutdown
// drain rather than a peer abort: an inactive transport context whose error
// is an *ErrTransport (or wraps one) is a drain induced by the process
// shutting down (the marker published), not a client abort. Production code
// never creates it; only the testseam lifecycle (a BaseContext that returns
// it in Err()) injects it, so the tests can prove the distinction the
// settlement design draws between a bounded shutdown drain and a peer abort
// (finding F-5).
type ErrTransport struct{ cause error }

// NewErrTransport returns a transport-drain error carrying the drain cause
// (the effective context error at drain, when present). It is the only
// constructor: the cause field is unexported so the error's identity is
// stable (a *ErrTransport, recognized by the dbctx isShutdownDrain check).
func NewErrTransport(cause error) *ErrTransport {
	return &ErrTransport{cause: cause}
}

func (e *ErrTransport) Error() string {
	if e.cause == nil {
		return "transport closed (shutdown drain)"
	}
	return "transport closed: " + e.cause.Error()
}

// Unwrap returns the drain cause (the effective context error at drain, when
// present).
func (e *ErrTransport) Unwrap() error { return e.cause }

// TransportDrain marks ErrTransport as a transport drain (the dbctx
// isShutdownDrain check): a bounded shutdown closing the transport, not a
// peer abort.
func (e *ErrTransport) TransportDrain() {}

// GateOf returns the response gate carried by the request context, or nil
// when the context is outside the access-logging stage. It is the public
// counterpart of the package-private recorderOf, exposed under the testseam
// tag for the lifecycle tests.
func GateOf(ctx context.Context) *ResponseGate {
	return recorderOf(ctx)
}
