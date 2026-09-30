//go:build testseam

package api

import (
	"sync"
	"testing"

	"vector-service/internal/dbctx"
)

// markerMu serializes the process-level shutdown marker between tests. The
// marker is process-global: in production dbctx publishes it once per
// process and never clears it (the process exits after the drain). These
// unit tests therefore reset it themselves, and two tests may never hold it
// at once. The reset lives behind the testseam build tag
// (dbctx.ClearShutdownMarker); these files compile only with that tag, so
// this is the single seam the marker tests go through.
var markerMu sync.Mutex

// markShutdown publishes the process-level shutdown marker for the duration
// of one test. It returns a release function the test defers: the reset
// (dbctx.ClearShutdownMarker) runs there, after the test body, so later
// tests settle markerless.
func markShutdown(t *testing.T) func() {
	t.Helper()
	markerMu.Lock()
	dbctx.MarkShutdown()
	return func() {
		dbctx.ClearShutdownMarker()
		markerMu.Unlock()
	}
}
