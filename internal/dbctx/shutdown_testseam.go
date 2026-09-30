//go:build testseam

package dbctx

// ClearShutdownMarker is a test-only seam (behind the testseam build tag, so
// it is excluded from production builds) that clears the process-level
// shutdown marker. A test that publishes the marker via MarkShutdown must
// clear it so later tests in the same process settle markerless — the marker
// is process-global. It is the public counterpart of the unexported
// resetShutdown.
func ClearShutdownMarker() { resetShutdown() }
