//go:build integration

package testdb_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"vector-service/internal/db"
	"vector-service/internal/privilege"
	"vector-service/internal/testdb"
)

// TestExcessGrantRejection is the F-10 excess-privilege-grant negative matrix
// slice. Each subtest provisions a fresh disposable harness, converges it
// through the production runner, applies one excess-grant variant, and
// verifies that db.RuntimeBoundary rejects it through the runtime connection.
// Every subtest tears down its harness independently.
//
// The matrix covers the five documented excess-grant variants:
//   - extra direct table grant (TRUNCATE on vector_records)
//   - inherited table grant via a fresh harmless role
//   - direct column grant (REFERENCES on vector_records.object_id)
//   - inherited column grant via a fresh harmless role
//   - PUBLIC column grant (REFERENCES on vector_records.object_id)
//
// It is integration-tagged and skips when VECTOR_SERVICE_TEST_PG_DSN is
// not configured, so `go test ./...` stays green on a host with no database.
//
// NOTE: This test runs serially (no t.Parallel). All subtests share the same
// fixed database cluster and canonical roles (vector_api, vector_owner).
// Cross-package integration tests also share these cluster-global roles.
// The release gate must run integration-tagged packages serialized:
//
//	go test -p 1 -tags integration ./...
func TestExcessGrantRejection(t *testing.T) {
	cases := []struct {
		name     string
		roles    []string // cluster-global fixture roles created by this case
		expected string   // expected AssertionError.Check
		apply    func(*testing.T, context.Context, *testdb.Harness, []string)
	}{
		{
			// (1) Extra direct table grant: TRUNCATE on vector_records.
			// The canonical set for vector_records is {SELECT, INSERT, UPDATE, DELETE}.
			// TRUNCATE is not in it; has_table_privilege(..., 'TRUNCATE') returns true
			// when it should be false, so the privilege-boundary check fires.
			name:     "extra-direct-table-grant-truncate",
			roles:    nil,
			expected: "privilege-boundary",
			apply: func(t *testing.T, ctx context.Context, h *testdb.Harness, _ []string) {
				if err := h.GrantVectorRecordsTruncateToRuntime(ctx); err != nil {
					t.Fatalf("apply variant: %v", err)
				}
			},
		},
		{
			// (2) Inherited table grant via a fresh harmless role G.
			// G receives SELECT on schema_migrations (canonically none).
			// vector_api inherits from G with INHERIT TRUE.
			// has_table_privilege(schema_migrations, 'SELECT') returns true when
			// it should be false, so the privilege-boundary check fires.
			name:     "inherited-table-grant-via-role",
			roles:    []string{"G"},
			expected: "privilege-boundary",
			apply: func(t *testing.T, ctx context.Context, h *testdb.Harness, scoped []string) {
				g := scoped[0]
				if err := h.CreateRole(ctx, g); err != nil {
					t.Fatalf("create role %s: %v", g, err)
				}
				if err := h.GrantHistoryTableSelectToRole(ctx, g); err != nil {
					t.Fatalf("grant SELECT on schema_migrations to %s: %v", g, err)
				}
				if err := h.GrantMembership(ctx, g, testdb.RuntimeRole, true, false, false); err != nil {
					t.Fatalf("grant %s to %s: %v", g, testdb.RuntimeRole, err)
				}
			},
		},
		{
			// (3) Direct column grant: REFERENCES on vector_records.object_id.
			// The canonical column subset for vector_records is {SELECT, INSERT, UPDATE}.
			// REFERENCES is not in it; has_column_privilege(..., 'REFERENCES') returns
			// true when it should be false, so the column-privilege-boundary check fires.
			name:     "direct-column-grant-references",
			roles:    nil,
			expected: "column-privilege-boundary",
			apply: func(t *testing.T, ctx context.Context, h *testdb.Harness, _ []string) {
				if err := h.GrantRecordsObjectIDReferencesToRuntime(ctx); err != nil {
					t.Fatalf("apply variant: %v", err)
				}
			},
		},
		{
			// (4) Inherited column grant via a fresh harmless role G.
			// G receives SELECT(version) on schema_migrations (canonically none).
			// vector_api inherits from G with INHERIT TRUE.
			// has_column_privilege(schema_migrations, version, 'SELECT') returns true
			// when it should be false, so the column-privilege-boundary check fires.
			name:     "inherited-column-grant-via-role",
			roles:    []string{"G"},
			expected: "column-privilege-boundary",
			apply: func(t *testing.T, ctx context.Context, h *testdb.Harness, scoped []string) {
				g := scoped[0]
				if err := h.CreateRole(ctx, g); err != nil {
					t.Fatalf("create role %s: %v", g, err)
				}
				if err := h.GrantHistoryVersionColumnSelectToRole(ctx, g); err != nil {
					t.Fatalf("grant SELECT(version) on schema_migrations to %s: %v", g, err)
				}
				if err := h.GrantMembership(ctx, g, testdb.RuntimeRole, true, false, false); err != nil {
					t.Fatalf("grant %s to %s: %v", g, testdb.RuntimeRole, err)
				}
			},
		},
		{
			// (5) PUBLIC column grant: REFERENCES on vector_records.object_id.
			// PUBLIC receives REFERENCES on the column. Because vector_api inherits
			// from PUBLIC, has_column_privilege(..., 'REFERENCES') returns true when
			// it should be false, so the column-privilege-boundary check fires.
			name:     "public-column-grant-references",
			roles:    nil,
			expected: "column-privilege-boundary",
			apply: func(t *testing.T, ctx context.Context, h *testdb.Harness, _ []string) {
				if err := h.GrantRecordsObjectIDReferencesToPublic(ctx); err != nil {
					t.Fatalf("apply variant: %v", err)
				}
			},
		},
	}

	for _, tc := range cases {
		tc := tc

		t.Run(tc.name, func(t *testing.T) {
			// Generate a unique role name per invocation so that an
			// interrupted prior run cannot leave a stale role with
			// leftover memberships or attributes. The deterministic
			// index-based suffix is insufficient because CreateRole
			// is idempotent and would reuse the stale role.
			uuidSuffix, err := testdb.NewUUID()
			if err != nil {
				t.Fatalf("generate fixture role suffix: %v", err)
			}
			suffix := fmt.Sprintf("_%s", uuidSuffix)
			scopedRoles := make([]string, len(tc.roles))
			for j, r := range tc.roles {
				scopedRoles[j] = r + suffix
			}

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

				// 1. Teardown the suite database and restore canonical roles.
				if err := h.Teardown(cleanupCtx); err != nil {
					t.Errorf("teardown: %v", err)
				}

				// 2. Drop cluster-global fixture roles through bootstrap
				// maintenance connection.
				if len(scopedRoles) > 0 {
					if err := h.DropRoles(cleanupCtx, scopedRoles...); err != nil {
						t.Errorf("drop fixture roles %v: %v", scopedRoles, err)
					}
					h.AssertRolesAbsent(cleanupCtx, t, scopedRoles...)
				}
			})

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

			// Apply the excess-grant variant on the converged database,
			// using scoped fixture role names.
			tc.apply(t, ctx, h, scopedRoles)

			// Open a runtime connection and assert that RuntimeBoundary rejects
			// the excess-grant state. Each subtest uses its own freshly provisioned
			// harness, so the variant is isolated.
			rt, err := h.RuntimeConn(ctx)
			if err != nil {
				t.Fatalf("runtime connect: %v", err)
			}
			defer rt.Close(ctx)

			q := privilege.PgxQuerier{Conn: rt}
			boundaryErr := db.RuntimeBoundary(ctx, q, testdb.RuntimeRole, testdb.MigrationRole, testdb.DatabaseName)

			// Assert the boundary was rejected with the expected check.
			if boundaryErr == nil {
				t.Fatal("runtime boundary passed; the variant is not a rejection fixture")
			}

			var assertErr *db.AssertionError
			if !errors.As(boundaryErr, &assertErr) {
				t.Fatalf("expected *db.AssertionError, got: %v", boundaryErr)
			}
			if assertErr.Check != tc.expected {
				t.Fatalf("expected check %q, got %q: %v", tc.expected, assertErr.Check, assertErr)
			}
			t.Logf("variant correctly rejected (check=%s): %v", assertErr.Check, assertErr)
		})
	}
}
