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

// TestGrantOptionRejection is the F-10 grant-option negative matrix slice.
// Each subtest provisions a fresh disposable harness, converges it through
// the production runner, applies exactly one WITH GRANT OPTION fixture on
// a single in-scope ACL catalog, and verifies that db.RuntimeBoundary
// rejects it through the runtime connection with a typed AssertionError.
// Most subtests expect a grant-option check; the column-acl-schema-migrations-
// version case expects column-privilege-boundary because the privilege grid
// fires before the grant-option check (schema_migrations is not canonical).
// Every subtest tears down its harness independently.
//
// The matrix covers every in-scope ACL catalog with a grant-option fixture
// for the runtime role (vector_api), as specified by the authoritative
// contract (docs/implementation/package-5-verification-runtime.md, lines
// 203–216):
//
//   - column ACL on schema_migrations.version (pg_attribute.attacl)
//   - database CONNECT (pg_database.datacl)
//   - schema USAGE (pg_namespace.nspacl)
//   - function EXECUTE on validate_vector_record() (pg_proc.proacl)
//   - type USAGE on distance_metric (pg_type.typacl)
//   - table SELECT on vector_control.applications (pg_class.relacl)
//   - table-level grant-option on canonical vector_data.vector_records
//     (pg_class.relacl)
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
func TestGrantOptionRejection(t *testing.T) {
	cases := []struct {
		name     string
		expected string // expected AssertionError.Check
		apply    func(*testing.T, context.Context, *testdb.Harness)
	}{
		{
			// Column ACL on schema_migrations.version: grant option stored
			// in pg_attribute.attacl, not pg_class.relacl. The canonical
			// set for schema_migrations is empty (it is deliberately absent
			// from canonicalTables), so this fixture is both a column
			// privilege and a grant-option violation. The privilege grid
			// check fires before the grant-option check in the boundary
			// assertion order, so the first assertion failure is
			// column-privilege-boundary, not grant-option.
			name:     "column-acl-schema-migrations-version",
			expected: "column-privilege-boundary",
			apply: func(t *testing.T, ctx context.Context, h *testdb.Harness) {
				if err := h.GrantOptionColumnHistorySelect(ctx); err != nil {
					t.Fatalf("apply fixture: %v", err)
				}
			},
		},
		{
			// Column ACL on vector_control.applications(id): grant option
			// stored in pg_attribute.attacl. Unlike the schema_migrations
			// case above, applications IS canonical, so the privilege grid
			// passes and the grant-option check is what fires. This is the
			// pure column-catalog grant-option rejection proof.
			name:     "column-acl-applications-id",
			expected: "grant-option",
			apply: func(t *testing.T, ctx context.Context, h *testdb.Harness) {
				if err := h.GrantOptionColumnApplicationsSelect(ctx); err != nil {
					t.Fatalf("apply fixture: %v", err)
				}
			},
		},
		{
			// Database CONNECT: grant option on the database-level ACL
			// (pg_database.datacl). The runtime role already has CONNECT
			// canonically; adding WITH GRANT OPTION makes it a regrant
			// channel.
			name:     "database-connect",
			expected: "grant-option",
			apply: func(t *testing.T, ctx context.Context, h *testdb.Harness) {
				if err := h.GrantOptionDatabaseConnect(ctx); err != nil {
					t.Fatalf("apply fixture: %v", err)
				}
			},
		},
		{
			// Schema USAGE: grant option on the schema-level ACL
			// (pg_namespace.nspacl) for vector_data. The runtime role
			// already has USAGE canonically; adding WITH GRANT OPTION
			// makes it a regrant channel.
			name:     "schema-usage",
			expected: "grant-option",
			apply: func(t *testing.T, ctx context.Context, h *testdb.Harness) {
				if err := h.GrantOptionSchemaUsage(ctx, "vector_data"); err != nil {
					t.Fatalf("apply fixture: %v", err)
				}
			},
		},
		{
			// Function EXECUTE: grant option on the function-level ACL
			// (pg_proc.proacl) for validate_vector_record(). The runtime
			// role already has EXECUTE canonically; adding WITH GRANT
			// OPTION makes it a regrant channel.
			name:     "function-execute",
			expected: "grant-option",
			apply: func(t *testing.T, ctx context.Context, h *testdb.Harness) {
				if err := h.GrantOptionFunctionExecute(ctx); err != nil {
					t.Fatalf("apply fixture: %v", err)
				}
			},
		},
		{
			// Type USAGE: grant option on the type-level ACL
			// (pg_type.typacl) for distance_metric. The runtime role
			// already has USAGE canonically; adding WITH GRANT OPTION
			// makes it a regrant channel.
			name:     "type-usage",
			expected: "grant-option",
			apply: func(t *testing.T, ctx context.Context, h *testdb.Harness) {
				if err := h.GrantOptionTypeUsage(ctx); err != nil {
					t.Fatalf("apply fixture: %v", err)
				}
			},
		},
		{
			// Table SELECT on vector_control.applications: grant option on
			// the table-level ACL (pg_class.relacl). The runtime role
			// already has SELECT canonically; adding WITH GRANT OPTION
			// makes it a regrant channel.
			name:     "table-select-applications",
			expected: "grant-option",
			apply: func(t *testing.T, ctx context.Context, h *testdb.Harness) {
				if err := h.GrantOptionTableSelect(ctx); err != nil {
					t.Fatalf("apply fixture: %v", err)
				}
			},
		},
		{
			// Table-level grant-option on the canonical
			// vector_data.vector_records table: grant option on the
			// table-level ACL (pg_class.relacl). The runtime role already
			// has UPDATE canonically; adding WITH GRANT OPTION makes it
			// a regrant channel.
			name:     "table-grant-option-vector-records",
			expected: "grant-option",
			apply: func(t *testing.T, ctx context.Context, h *testdb.Harness) {
				if err := h.GrantOptionTableVectorRecords(ctx); err != nil {
					t.Fatalf("apply fixture: %v", err)
				}
			},
		},
	}

	for _, tc := range cases {
		tc := tc

		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
			defer cancel()

			h, err := testdb.New(ctx)
			if err != nil {
				t.Skipf("testdb: bootstrap identity not configured (set %s to a superuser DSN to run the integration suite): %v",
					testdb.EnvBootstrapDSN, err)
			}

			// Register cleanup immediately after New so that failures in
			// Provision, Converge, apply, or RuntimeConn still clean up
			// the database and restore canonical role state.
			t.Cleanup(func() {
				cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cleanupCancel()

				// Teardown the suite database and restore canonical roles.
				if err := h.Teardown(cleanupCtx); err != nil {
					t.Errorf("teardown: %v", err)
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

			// Apply exactly one WITH GRANT OPTION fixture on a single
			// in-scope ACL catalog.
			tc.apply(t, ctx, h)

			// Open a runtime connection and assert that RuntimeBoundary
			// rejects the grant-option state. Each subtest uses its own
			// freshly provisioned harness, so the fixture is isolated.
			rt, err := h.RuntimeConn(ctx)
			if err != nil {
				t.Fatalf("runtime connect: %v", err)
			}
			defer rt.Close(ctx)

			q := privilege.PgxQuerier{Conn: rt}
			boundaryErr := db.RuntimeBoundary(ctx, q, testdb.RuntimeRole, testdb.MigrationRole, testdb.DatabaseName)

			// Assert the boundary was rejected with the expected check.
			if boundaryErr == nil {
				t.Fatal("runtime boundary passed; the grant-option fixture is not a rejection fixture")
			}

			var assertErr *db.AssertionError
			if !errors.As(boundaryErr, &assertErr) {
				t.Fatalf("expected *db.AssertionError, got: %v", boundaryErr)
			}
			if assertErr.Check != tc.expected {
				t.Fatalf("expected check %q, got %q: %v", tc.expected, assertErr.Check, assertErr)
			}
			t.Logf("grant-option fixture correctly rejected (check=%s): %v", assertErr.Check, assertErr)
		})
	}
}
