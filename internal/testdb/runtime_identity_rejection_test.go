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

// TestRuntimeIdentityRejection is the F-10 negative runtime identity matrix
// slice. Each subtest provisions a fresh disposable harness, converges it
// through the production runner, applies one broken-state variant, and
// verifies that db.RuntimeBoundary rejects it through the runtime connection.
// Every subtest tears down its harness independently.
//
// The matrix covers the membership-closure and role-attribute rejection
// cases: direct and transitive migration identity membership, SET-option
// true membership (direct and two-hop), ADMIN-option true membership
// (direct and two-hop), reachable owner role, reachable BYPASSRLS role,
// reachable superuser role, and the vector_api BYPASSRLS attribute.
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
func TestRuntimeIdentityRejection(t *testing.T) {
	cases := []struct {
		name     string
		roles    []string // cluster-global fixture roles created by this case
		expected string   // expected AssertionError.Check
		apply    func(*testing.T, context.Context, *testdb.Harness, []string)
	}{
		{
			name:     "direct-migration-identity-membership",
			roles:    nil,
			expected: "grant-provenance",
			apply: func(t *testing.T, ctx context.Context, h *testdb.Harness, _ []string) {
				if err := h.GrantMigrationIdentityToRuntime(ctx); err != nil {
					t.Fatalf("apply variant: %v", err)
				}
			},
		},
		{
			name:     "transitive-migration-identity-membership",
			roles:    []string{"G"},
			expected: "grant-provenance",
			apply: func(t *testing.T, ctx context.Context, h *testdb.Harness, scoped []string) {
				if err := h.GrantMigrationIdentityToRuntimeViaIntermediate(ctx, scoped[0]); err != nil {
					t.Fatalf("apply variant: %v", err)
				}
			},
		},
		{
			name:     "set-option-true-direct",
			roles:    []string{"G"},
			expected: "membership-options",
			apply: func(t *testing.T, ctx context.Context, h *testdb.Harness, scoped []string) {
				if err := h.GrantSetOptionMembershipToRuntime(ctx, scoped[0]); err != nil {
					t.Fatalf("apply variant: %v", err)
				}
			},
		},
		{
			name:     "set-option-true-two-hop",
			roles:    []string{"G", "M"},
			expected: "membership-options",
			apply: func(t *testing.T, ctx context.Context, h *testdb.Harness, scoped []string) {
				if err := h.GrantSetOptionMembershipToRuntimeViaIntermediate(ctx, scoped[0], scoped[1]); err != nil {
					t.Fatalf("apply variant: %v", err)
				}
			},
		},
		{
			name:     "admin-option-true-direct",
			roles:    []string{"G"},
			expected: "membership-options",
			apply: func(t *testing.T, ctx context.Context, h *testdb.Harness, scoped []string) {
				if err := h.GrantAdminOptionMembershipToRuntime(ctx, scoped[0]); err != nil {
					t.Fatalf("apply variant: %v", err)
				}
			},
		},
		{
			name:     "admin-option-true-two-hop",
			roles:    []string{"G", "M"},
			expected: "membership-options",
			apply: func(t *testing.T, ctx context.Context, h *testdb.Harness, scoped []string) {
				if err := h.GrantAdminOptionMembershipToRuntimeViaIntermediate(ctx, scoped[0], scoped[1]); err != nil {
					t.Fatalf("apply variant: %v", err)
				}
			},
		},
		{
			name:     "reachable-owner-role",
			roles:    []string{"H"},
			expected: "grant-provenance",
			apply: func(t *testing.T, ctx context.Context, h *testdb.Harness, scoped []string) {
				if err := h.GrantHarmlessRoleToRuntime(ctx, scoped[0], "vector_control.applications"); err != nil {
					t.Fatalf("apply variant: %v", err)
				}
			},
		},
		{
			name:     "reachable-bypassrls-role",
			roles:    []string{"P"},
			expected: "membership-closure",
			apply: func(t *testing.T, ctx context.Context, h *testdb.Harness, scoped []string) {
				if err := h.GrantBypassRLSRoleToRuntime(ctx, scoped[0]); err != nil {
					t.Fatalf("apply variant: %v", err)
				}
			},
		},
		{
			name:     "reachable-superuser-role",
			roles:    []string{"S"},
			expected: "membership-closure",
			apply: func(t *testing.T, ctx context.Context, h *testdb.Harness, scoped []string) {
				if err := h.GrantSuperuserRoleToRuntime(ctx, scoped[0]); err != nil {
					t.Fatalf("apply variant: %v", err)
				}
			},
		},
		{
			name:     "runtime-role-bypassrls-attribute",
			roles:    nil,
			expected: "role-attributes",
			apply: func(t *testing.T, ctx context.Context, h *testdb.Harness, _ []string) {
				if err := h.AlterRuntimeRoleBypassRLS(ctx); err != nil {
					t.Fatalf("apply variant: %v", err)
				}
			},
		},
	}

	for i, tc := range cases {
		tc := tc
		// Scope fixture role names to this test case so cross-package tests
		// sharing the same cluster do not collide with canonical role names.
		suffix := fmt.Sprintf("_%d", i)
		scopedRoles := make([]string, len(tc.roles))
		for j, r := range tc.roles {
			scopedRoles[j] = r + suffix
		}

		t.Run(tc.name, func(t *testing.T) {
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
				// Teardown is safe to call even if Provision was never called
				// (DROP DATABASE IF EXISTS) and it restores vector_api and
				// vector_owner to their canonical attributes, preventing
				// pollution from attribute-mutating variants (F-2).
				if err := h.Teardown(cleanupCtx); err != nil {
					t.Errorf("teardown: %v", err)
				}

				// 2. Drop cluster-global fixture roles through bootstrap
				// maintenance connection. These roles are cluster-global and
				// persist beyond the suite database; leaving them behind
				// pollutes subsequent test runs.
				if len(scopedRoles) > 0 {
					if err := h.DropRoles(cleanupCtx, scopedRoles...); err != nil {
						t.Errorf("drop fixture roles %v: %v", scopedRoles, err)
					}
					// Verify both roles are actually gone (F-4: two-hop
					// cleanup must remove all fixture roles).
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

			// Apply the broken-state variant on the converged database,
			// using scoped fixture role names.
			tc.apply(t, ctx, h, scopedRoles)

			// Open a runtime connection and assert that RuntimeBoundary rejects
			// the broken state. Each subtest uses its own freshly provisioned
			// harness, so the variant is isolated.
			rt, err := h.RuntimeConn(ctx)
			if err != nil {
				t.Fatalf("runtime connect: %v", err)
			}
			defer rt.Close(ctx)

			q := privilege.PgxQuerier{Conn: rt}
			boundaryErr := db.RuntimeBoundary(ctx, q, testdb.RuntimeRole, testdb.MigrationRole, testdb.DatabaseName)

			// F-3: assert the boundary was rejected with the expected check.
			if boundaryErr == nil {
				t.Fatal("runtime boundary passed; the variant is not a rejection fixture")
			}

			var assertErr *db.AssertionError
			if !errors.As(boundaryErr, &assertErr) {
				// Any non-AssertionError (query/catalog error) is a failure.
				t.Fatalf("expected *db.AssertionError, got: %v", boundaryErr)
			}
			if assertErr.Check != tc.expected {
				t.Fatalf("expected check %q, got %q: %v", tc.expected, assertErr.Check, assertErr)
			}
			t.Logf("variant correctly rejected (check=%s): %v", assertErr.Check, assertErr)
		})
	}
}
