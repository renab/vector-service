//go:build integration

package testdb_test

import (
	"context"
	"testing"
	"time"

	"vector-service/internal/db"
	"vector-service/internal/privilege"
	"vector-service/internal/testdb"
)

// TestHarnessEndToEnd is the 5a harness smoke test: it proves the harness
// works end to end against a real PostgreSQL + pgvector cluster — provision,
// converge through the production runner, open the runtime connection,
// assert the canonical runtime identity boundary, reject a broken boundary
// (a negative-identity variant), and tear down. It is the harness's own
// integration test; the package-1 identity matrices and the 5b matrices are
// separate entry points that consume the harness.
//
// It is integration-tagged and skips when the bootstrap identity is not
// configured (VECTOR_SERVICE_TEST_PG_DSN unset), so `go test ./...` stays
// green on a host with no database. CI runs it with the integration tag and
// the environment variable set.
func TestHarnessEndToEnd(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	h, err := testdb.New(ctx)
	if err != nil {
		t.Skipf("testdb: bootstrap identity not configured (set %s to a superuser DSN to run the integration suite): %v",
			testdb.EnvBootstrapDSN, err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := h.Teardown(ctx); err != nil {
			t.Errorf("teardown: %v", err)
		}
	})

	if err := h.Provision(ctx); err != nil {
		t.Fatalf("provision: %v", err)
	}

	// Converge through the production runner (the same function the migrate
	// subcommand invokes — one runner, embedded source). All six migrations
	// are applied on a fresh database.
	res, err := h.Converge(ctx)
	if err != nil {
		t.Fatalf("converge: %v", err)
	}
	if len(res.Applied) != 6 {
		t.Fatalf("converge applied %d migrations, want 6", len(res.Applied))
	}
	if res.AlreadyApplied != 0 {
		t.Fatalf("converge reported %d already-applied migrations, want 0 on a fresh database", res.AlreadyApplied)
	}

	// Convergence is idempotent: a second pass applies nothing.
	res2, err := h.Converge(ctx)
	if err != nil {
		t.Fatalf("converge (second pass): %v", err)
	}
	if len(res2.Applied) != 0 || res2.AlreadyApplied != 6 {
		t.Fatalf("second converge applied %d / already-applied %d, want 0 / 6",
			len(res2.Applied), res2.AlreadyApplied)
	}

	// The runtime connection is reachable as the canonical runtime role (it
	// proves migration 0004's CONNECT grant is live), and the full runtime
	// identity boundary holds on it.
	rt, err := h.RuntimeConn(ctx)
	if err != nil {
		t.Fatalf("runtime connect: %v", err)
	}
	defer rt.Close(ctx)
	q := privilege.PgxQuerier{Conn: rt}
	if err := db.RuntimeBoundary(ctx, q, testdb.RuntimeRole, testdb.MigrationRole, testdb.DatabaseName); err != nil {
		t.Fatalf("canonical runtime boundary failed on a provisioned+converged database: %v", err)
	}

	// A negative-identity variant breaks the boundary: granting the runtime
	// role membership in the migration identity reaches the migration
	// identity in the runtime role's closure, which the membership-closure
	// rule rejects.
	if err := h.GrantMigrationIdentityToRuntime(ctx); err != nil {
		t.Fatalf("apply negative variant: %v", err)
	}
	if err := db.RuntimeBoundary(ctx, q, testdb.RuntimeRole, testdb.MigrationRole, testdb.DatabaseName); err == nil {
		t.Fatal("runtime boundary passed after the runtime role was granted membership in the migration identity; the variant is not a rejection fixture")
	} else {
		t.Logf("negative variant correctly rejected: %v", err)
	}

	// The migration connection is reachable as the canonical migration
	// identity (it is the identity the runner ran as).
	if _, err := h.MigrationConn(ctx); err != nil {
		t.Fatalf("migration connect: %v", err)
	}
}
