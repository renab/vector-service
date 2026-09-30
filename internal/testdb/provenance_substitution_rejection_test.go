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

// TestProvenanceSubstitutionRejection is the F-10 provenance-substitution
// negative matrix slice. Each subtest provisions a fresh disposable harness,
// converges it through the production runner, applies one provenance-
// substitution variant (replacing a direct canonical grant with an inherited
// grant through a fresh harmless role or with a PUBLIC grant), and verifies
// that db.RuntimeBoundary rejects it with a typed AssertionError whose check
// is grant-provenance. Every subtest tears down its harness independently.
//
// The matrix covers all canonical grant-source forms specified by the
// authoritative contract
// (docs/implementation/package-5-verification-runtime.md, lines 217–272):
//
// Inherited substitutions (6): the direct canonical grant is revoked and
// the same privilege is granted through a fresh harmless role G with
// INHERIT TRUE, SET FALSE, ADMIN FALSE membership, so every has_* predicate
// resolves identically but the provenance check rejects the missing direct
// entry:
//
//   - database CONNECT
//   - schema USAGE
//   - table privilege (vector_control.applications)
//   - column-derived table relacl substitution (vector_data.vector_records)
//   - function EXECUTE (vector_data.validate_vector_record)
//   - type USAGE (vector_control.distance_metric)
//
// PUBLIC substitutions (5): the direct canonical grant is revoked and the
// same privilege is granted to PUBLIC, leaving the effective set unchanged
// (the spec preserves database CONNECT for PUBLIC, so no database PUBLIC
// substitution is included):
//
//   - schema USAGE
//   - table privilege (vector_control.applications)
//   - column-derived table relacl substitution (vector_data.vector_records)
//   - function EXECUTE (vector_data.validate_vector_record)
//   - type USAGE (vector_control.distance_metric)
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
func TestProvenanceSubstitutionRejection(t *testing.T) {
	cases := []struct {
		name     string
		roles    []string // cluster-global fixture roles created by this case
		expected string   // expected AssertionError.Check
		apply    func(*testing.T, context.Context, *testdb.Harness, []string)
	}{
		// -----------------------------------------------------------------
		// Inherited substitutions (6): direct grant replaced by a grant to
		// a fresh harmless role G, with INHERIT-true membership.
		// The column-derived case is a table-level relacl substitution
		// (REVOKE/GRANT ON table) that exercises the column has_* grid,
		// not an attacl operation.
		// -----------------------------------------------------------------
		{
			name:     "inherited-database-connect",
			roles:    []string{"G"},
			expected: "grant-provenance",
			apply: func(t *testing.T, ctx context.Context, h *testdb.Harness, scoped []string) {
				if err := h.SubstituteDatabaseConnectInherited(ctx, scoped[0]); err != nil {
					t.Fatalf("apply variant: %v", err)
				}
			},
		},
		{
			name:     "inherited-schema-usage",
			roles:    []string{"G"},
			expected: "grant-provenance",
			apply: func(t *testing.T, ctx context.Context, h *testdb.Harness, scoped []string) {
				if err := h.SubstituteSchemaUsageInherited(ctx, "vector_data", scoped[0]); err != nil {
					t.Fatalf("apply variant: %v", err)
				}
			},
		},
		{
			name:     "inherited-table-privilege",
			roles:    []string{"G"},
			expected: "grant-provenance",
			apply: func(t *testing.T, ctx context.Context, h *testdb.Harness, scoped []string) {
				if err := h.SubstituteTablePrivilegesInherited(ctx,
					"vector_control.applications",
					"SELECT, INSERT, UPDATE",
					scoped[0],
				); err != nil {
					t.Fatalf("apply variant: %v", err)
				}
			},
		},
		{
			// Column-derived table relacl substitution: the direct table-level
			// grant on vector_data.vector_records is revoked and re-granted
			// through G. This exercises the column has_* grid (PostgreSQL
			// reports table-level privileges on each column), not attacl.
			name:     "inherited-column-derived-relacl",
			roles:    []string{"G"},
			expected: "grant-provenance",
			apply: func(t *testing.T, ctx context.Context, h *testdb.Harness, scoped []string) {
				if err := h.SubstituteColumnPrivilegesInherited(ctx,
					"vector_data.vector_records",
					"SELECT",
					scoped[0],
				); err != nil {
					t.Fatalf("apply variant: %v", err)
				}
			},
		},
		{
			name:     "inherited-function-execute",
			roles:    []string{"G"},
			expected: "grant-provenance",
			apply: func(t *testing.T, ctx context.Context, h *testdb.Harness, scoped []string) {
				if err := h.SubstituteFunctionExecuteInherited(ctx,
					"vector_data.validate_vector_record()",
					scoped[0],
				); err != nil {
					t.Fatalf("apply variant: %v", err)
				}
			},
		},
		{
			name:     "inherited-type-usage",
			roles:    []string{"G"},
			expected: "grant-provenance",
			apply: func(t *testing.T, ctx context.Context, h *testdb.Harness, scoped []string) {
				if err := h.SubstituteTypeUsageInherited(ctx,
					"vector_control.distance_metric",
					scoped[0],
				); err != nil {
					t.Fatalf("apply variant: %v", err)
				}
			},
		},
		// -----------------------------------------------------------------
		// PUBLIC substitutions (5): direct grant replaced by a grant to
		// PUBLIC. No database PUBLIC substitution (spec preserves database
		// CONNECT for PUBLIC, so it would not be a rejection fixture).
		// The column-derived case is a table-level relacl substitution
		// (REVOKE/GRANT ON table) that exercises the column has_* grid,
		// not an attacl operation.
		// -----------------------------------------------------------------
		{
			name:     "public-schema-usage",
			roles:    nil,
			expected: "grant-provenance",
			apply: func(t *testing.T, ctx context.Context, h *testdb.Harness, _ []string) {
				if err := h.SubstituteSchemaUsagePublic(ctx, "vector_data"); err != nil {
					t.Fatalf("apply variant: %v", err)
				}
			},
		},
		{
			name:     "public-table-privilege",
			roles:    nil,
			expected: "grant-provenance",
			apply: func(t *testing.T, ctx context.Context, h *testdb.Harness, _ []string) {
				if err := h.SubstituteTablePrivilegesPublic(ctx,
					"vector_control.applications",
					"SELECT, INSERT, UPDATE",
				); err != nil {
					t.Fatalf("apply variant: %v", err)
				}
			},
		},
		{
			// Column-derived table relacl substitution: the direct table-level
			// grant on vector_data.vector_records is revoked and re-granted
			// to PUBLIC. This exercises the column has_* grid (PostgreSQL
			// reports table-level privileges on each column), not attacl.
			name:     "public-column-derived-relacl",
			roles:    nil,
			expected: "grant-provenance",
			apply: func(t *testing.T, ctx context.Context, h *testdb.Harness, _ []string) {
				if err := h.SubstituteColumnPrivilegesPublic(ctx,
					"vector_data.vector_records",
					"SELECT",
				); err != nil {
					t.Fatalf("apply variant: %v", err)
				}
			},
		},
		{
			name:     "public-function-execute",
			roles:    nil,
			expected: "grant-provenance",
			apply: func(t *testing.T, ctx context.Context, h *testdb.Harness, _ []string) {
				if err := h.SubstituteFunctionExecutePublic(ctx,
					"vector_data.validate_vector_record()",
				); err != nil {
					t.Fatalf("apply variant: %v", err)
				}
			},
		},
		{
			name:     "public-type-usage",
			roles:    nil,
			expected: "grant-provenance",
			apply: func(t *testing.T, ctx context.Context, h *testdb.Harness, _ []string) {
				if err := h.SubstituteTypeUsagePublic(ctx,
					"vector_control.distance_metric",
				); err != nil {
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
			// leftover memberships or attributes.
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
			// Provision, Converge, apply, or RuntimeConn still clean up
			// the database and restore canonical role state.
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

			// Apply the provenance-substitution variant on the converged
			// database, using scoped fixture role names.
			tc.apply(t, ctx, h, scopedRoles)

			// Open a runtime connection and assert that RuntimeBoundary
			// rejects the broken state. Each subtest uses its own freshly
			// provisioned harness, so the variant is isolated.
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
