//go:build testseam

package api

import (
	"testing"

	"vector-service/internal/dbctx"
)

// TestShutdownMarkerResetSeam exercises the testseam-gated reset of the
// process-level shutdown marker: publishing and then resetting leaves the
// marker unset, so later tests in the same process settle markerless.
func TestShutdownMarkerResetSeam(t *testing.T) {
	dbctx.MarkShutdown()
	if !dbctx.ShutdownMarked() {
		t.Fatal("marker not set after MarkShutdown")
	}
	dbctx.ClearShutdownMarker()
	if dbctx.ShutdownMarked() {
		t.Fatal("marker still set after ClearShutdownMarker")
	}
}
