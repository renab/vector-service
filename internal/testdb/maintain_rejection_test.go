//go:build integration

package testdb_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"vector-service/internal/db"
	"vector-service/internal/privilege"
	"vector-service/internal/testdb"
)

// TestMaintainRejection is the F-10 MAINTAIN-privilege negative matrix
// slice. It provisions a fresh disposable harness, converges it through the
// production runner, grants PostgreSQL 18's MAINTAIN on
// vector_data.vector_records (canonically absent everywhere), and verifies
// that db.RuntimeBoundary rejects it with a typed AssertionError whose
// check is privilege-boundary.
//
// MAINTAIN is a PostgreSQL 18+ table privilege. The test detects
// server_version_num and skips with an explicit reason when the server is
// older than 18.0.
//
// It is integration-tagged and skips when VECTOR_SERVICE_TEST_PG_DSN is
// not configured, so `go test ./...` stays green on a host with no database.
//
// NOTE: This test runs serially (no t.Parallel). It shares the same fixed
// database cluster and canonical roles (vector_api, vector_owner) with
// other integration tests. The release gate must run integration-tagged
// packages serialized:
//
//	go test -p 1 -tags integration ./...
func TestMaintainRejection(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	h, err := testdb.New(ctx)
	if err != nil {
		t.Skipf("testdb: bootstrap identity not configured (set %s to a superuser DSN to run the integration suite): %v",
			testdb.EnvBootstrapDSN, err)
	}

	// Register cleanup immediately after New so that failures in
	// Provision, Converge, apply, or RuntimeConn still clean up the
	// database and restore canonical role state.
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cleanupCancel()

		// Teardown the suite database and restore canonical roles.
		if err := h.Teardown(cleanupCtx); err != nil {
			t.Errorf("teardown: %v", err)
		}
	})

	// Check PostgreSQL version before provisioning. MAINTAIN was introduced
	// in PostgreSQL 18; the GRANT statement fails on older servers.
	versionNum, err := h.ServerVersionNum(ctx)
	if err != nil {
		t.Fatalf("read server_version_num: %v", err)
	}
	if versionNum < 180000 {
		t.Skipf("skipping MAINTAIN rejection test: server_version_num=%d (< 180000); MAINTAIN requires PostgreSQL 18+", versionNum)
	}
	t.Logf("server_version_num=%d; MAINTAIN supported", versionNum)

	// Provision the canonical shape on a fresh disposable database.
	if err := h.Provision(ctx); err != nil {
		t.Fatalf("provision: %v", err)
	}

	// Converge through the production runner.
	res, err := h.Converge(ctx)
	if err != nil {
		t.Fatalf("converge: %v", err)
	}
	if len(res.Applied) != 6 {
		t.Fatalf("converge applied %d migrations, want 6", len(res.Applied))
	}

	// Apply the MAINTAIN excess-grant variant on the converged database.
	// MAINTAIN is canonically absent everywhere; the canonical table set
	// for vector_records is {SELECT, INSERT, UPDATE, DELETE}.
	if err := h.GrantMaintainOnVectorRecords(ctx); err != nil {
		t.Fatalf("apply MAINTAIN variant: %v", err)
	}

	// Open a runtime connection and assert that RuntimeBoundary rejects
	// the MAINTAIN grant state.
	rt, err := h.RuntimeConn(ctx)
	if err != nil {
		t.Fatalf("runtime connect: %v", err)
	}
	defer rt.Close(ctx)

	q := privilege.PgxQuerier{Conn: rt}
	boundaryErr := db.RuntimeBoundary(ctx, q, testdb.RuntimeRole, testdb.MigrationRole, testdb.DatabaseName)

	// Assert the boundary was rejected with the expected check.
	if boundaryErr == nil {
		t.Fatal("runtime boundary passed; the MAINTAIN variant is not a rejection fixture")
	}

	var assertErr *db.AssertionError
	if !errors.As(boundaryErr, &assertErr) {
		t.Fatalf("expected *db.AssertionError, got: %v", boundaryErr)
	}
	expected := "privilege-boundary"
	if assertErr.Check != expected {
		t.Fatalf("expected check %q, got %q: %v", expected, assertErr.Check, assertErr)
	}
	t.Logf("MAINTAIN variant correctly rejected (check=%s): %v", assertErr.Check, assertErr)
}
