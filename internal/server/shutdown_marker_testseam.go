//go:build testseam

package server

import "vector-service/internal/dbctx"

// ResetShutdownMarkerForTest is a test-only seam (behind the testseam build
// tag, so it is excluded from production builds) that clears the process-level
// shutdown marker after a test has driven a full shutdown. The marker is
// process-global: in production dbctx publishes it once per process and never
// clears it (the process exits after the drain). The shutdown tests in this
// package drive the real lifecycle, which publishes the marker; each such
// test must clear it so later tests in the same process settle markerless.
// It is the server-package counterpart of dbctx's testseam reset.
func ResetShutdownMarkerForTest() {
	dbctx.ClearShutdownMarker()
}
