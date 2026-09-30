//go:build testseam

package server

import (
	"context"
	"errors"
	"testing"

	"vector-service/internal/dbctx"
)

// Unit tests for the production BaseContext (shutdown_basecontext.go).
//
// These exercise the exact function the lifecycle installs on the HTTP
// server — under the testseam build baseContextForRoot delegates to the
// production implementation (shutdown_basecontext_testseam.go →
// shutdown_basecontext_impl.go) — so the unit and the end-to-end lifecycle
// test (shutdown_settle_test.go) pin the same code.
//
// The marker is process-global; the marker helpers below hold markerMutex
// (shared with the lifecycle marker tests in shutdown_settle_test.go) and
// reset the marker before and after, so later tests settle markerless.
// NOTE: a test that holds markerMutex must not call t.Fatal/t.Fatalf in a
// deferred cleanup (the fatal's panic would unwind across the held mutex);
// the helpers are written to be fatal-free in their cleanup.

// markerGuard holds the process-global marker lock and restores the marker
// to unset on release. It returns the current marker state (the caller
// asserts it is unset before publishing).
func markerGuard(t *testing.T) (wasSet bool, release func()) {
	t.Helper()
	markerMutex.Lock()
	resetShutdownMarkerForTest()
	wasSet = dbctx.ShutdownMarked()
	return wasSet, func() {
		resetShutdownMarkerForTest()
		markerMutex.Unlock()
	}
}

// TestBaseContextProduction_DrainAnnotation pins the production distinction:
// once the process-level shutdown marker is set, an inactive serve root
// reports a drain-annotated error (implementing dbctx's transportDrainError)
// that unwraps to the root's cancellation; without the marker a plain
// cancellation reports the bare context.Canceled. The annotation consults
// the marker at observation time, and the lifecycle's normative order
// (marker published before the cancel) is what makes a shutdown cancel
// annotated.
func TestBaseContextProduction_DrainAnnotation(t *testing.T) {
	wasSet, release := markerGuard(t)
	if wasSet {
		t.Fatal("marker already set at guard; the marker tests must not overlap")
	}
	defer release()

	// Case 1: markerless cancellation — the bare context error.
	{
		root, cancel := context.WithCancel(context.Background())
		ctx := baseContextForRoot(root)(nil)
		if err := ctx.Err(); err != nil {
			t.Fatalf("active root reports Err() = %v, want nil", err)
		}
		cancel()
		if err := ctx.Err(); err != context.Canceled {
			t.Fatalf("markerless canceled root reports Err() = %v, want bare context.Canceled", err)
		}
	}

	// Case 2: marker published (the normative lifecycle order publishes the
	// marker before the cancel), then the root canceled — the drain
	// annotation must appear, unwrapping to the root's cancellation.
	dbctx.MarkShutdown()
	root, cancel := context.WithCancel(context.Background())
	ctx := baseContextForRoot(root)(nil)
	if err := ctx.Err(); err != nil {
		t.Fatalf("active root reports Err() = %v, want nil (the marker alone never deactivates the root)", err)
	}
	cancel()
	err := ctx.Err()
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("drain error does not unwrap to context.Canceled: %v", err)
	}
	drain, ok := err.(*serveRootDrainError)
	if !ok {
		t.Fatalf("canceled, marker-set root reports Err() type %T (%v), want *serveRootDrainError", err, err)
	}
	if drain.cause != context.Canceled {
		t.Fatalf("drain error cause = %v, want context.Canceled", drain.cause)
	}
	// The drain error must satisfy dbctx's transportDrainError interface
	// (that is what dbctx.isShutdownDrain checks on an inactive context).
	if _, ok := any(err).(interface{ TransportDrain() }); !ok {
		t.Fatal("drain error does not implement the dbctx transportDrainError interface (TransportDrain)")
	}

	// Case 3: repeated observations return the same cached drain error.
	if again := ctx.Err(); again != err {
		t.Fatalf("repeated Err() observation = %v, want the same cached drain error %v", again, err)
	}

	// Case 4: delegation — Deadline and Value flow to the root.
	if _, ok := ctx.Deadline(); ok {
		t.Fatal("Deadline() ok = true, want false (a bare WithCancel root has no deadline)")
	}
	type valKey struct{}
	root2, cancel2 := context.WithCancel(context.WithValue(context.Background(), valKey{}, "v"))
	defer cancel2()
	ctx2 := baseContextForRoot(root2)(nil)
	if got := ctx2.Value(valKey{}); got != "v" {
		t.Fatalf("Value() = %v, want \"v\" (delegation to the root)", got)
	}
}

// TestBaseContextProduction_MarkerlessCancelStaysBare pins that a plain
// cancellation WITHOUT the marker (the caller-abort and startup-failure
// paths: the lifecycle cancels the root but never publishes the marker)
// reports the bare context.Canceled — never a drain annotation.
func TestBaseContextProduction_MarkerlessCancelStaysBare(t *testing.T) {
	wasSet, release := markerGuard(t)
	if wasSet {
		t.Fatal("marker already set at guard; the marker tests must not overlap")
	}
	defer release()

	root, cancel := context.WithCancel(context.Background())
	ctx := baseContextForRoot(root)(nil)
	cancel()
	if err := ctx.Err(); err != context.Canceled {
		t.Fatalf("markerless canceled root reports Err() = %v, want bare context.Canceled (no drain annotation without the marker)", err)
	}
}
