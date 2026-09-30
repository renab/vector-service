//go:build integration

package testdb_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"vector-service/internal/migrate"
	"vector-service/internal/testdb"
)

// TestOwnershipGateRejection is the F-10 ownership-gate negative matrix.
// Each subtest provisions a fresh disposable harness, applies a broken-state
// ownership variant (pre-convergence or post-convergence), and verifies that
// the production migration runner's canonical-ownership gate rejects it with
// a structured OwnershipGateError. After rejection, migration history remains
// unchanged, proving the gate fails before bootstrap DDL or history writes.
//
// The matrix covers:
//   - Pre-convergence: vector_api-owned schema, vector_api-owned history table
//   - Post-convergence: vector_api-owned schema, vector_api-owned migrated
//     table, vector_api-owned migrated functions (both), unreachable-role-owned
//     migrated table, unreachable-role-owned migrated functions (both)
//
// It is integration-tagged and serial (no t.Parallel). All subtests share
// the same fixed database cluster and canonical roles. The fixture role U
// is cleaned up even on test failure. Teardown restores canonical roles.
//
// Release gate must run integration-tagged packages serialized:
//
//	go test -p 1 -tags integration ./...
func TestOwnershipGateRejection(t *testing.T) {
	cases := []struct {
		name          string
		roles         []string // cluster-global fixture roles created by this case
		preConverge   bool     // true = apply variant before Converge; false = converge first then apply
		historyAbsent bool     // pre-convergence only: true when the fixture does not create the history table
		apply         func(*testing.T, context.Context, *testdb.Harness, []string)
	}{
		// ---- Pre-convergence variants ----
		// The variant is applied before Converge. The gate runs before
		// bootstrap DDL, so no history table exists when the gate fires.

		{
			name:          "pre-convergence-vector-api-owned-schema",
			roles:         nil,
			preConverge:   true,
			historyAbsent: true, // schema exists but no history table was created
			apply: func(t *testing.T, ctx context.Context, h *testdb.Harness, _ []string) {
				if err := h.PrecreateSchemaOwnedByRuntime(ctx, "vector_control"); err != nil {
					t.Fatalf("apply variant: %v", err)
				}
			},
		},
		{
			name:          "pre-convergence-vector-api-owned-history-table",
			roles:         nil,
			preConverge:   true,
			historyAbsent: false, // history table was pre-created (empty, readable)
			apply: func(t *testing.T, ctx context.Context, h *testdb.Harness, _ []string) {
				if err := h.PrecreateHistoryTableOwnedByRuntime(ctx); err != nil {
					t.Fatalf("apply variant: %v", err)
				}
			},
		},

		// ---- Post-convergence variants ----
		// Converge first (6 migrations applied), then apply the variant.
		// The gate fires on the next Converge pass, rejecting before any
		// DDL or history write. History must remain unchanged.

		{
			name:        "post-convergence-vector-api-owned-schema",
			roles:       nil,
			preConverge: false,
			apply: func(t *testing.T, ctx context.Context, h *testdb.Harness, _ []string) {
				if err := h.TransferSchemaOwnership(ctx, "vector_control", testdb.RuntimeRole); err != nil {
					t.Fatalf("apply variant: %v", err)
				}
			},
		},
		{
			name:        "post-convergence-vector-api-owned-migrated-table",
			roles:       nil,
			preConverge: false,
			apply: func(t *testing.T, ctx context.Context, h *testdb.Harness, _ []string) {
				if err := h.TransferTableOwnership(ctx, "vector_data.vector_records", testdb.RuntimeRole); err != nil {
					t.Fatalf("apply variant: %v", err)
				}
			},
		},
		{
			name:        "post-convergence-vector-api-owned-function-validate",
			roles:       nil,
			preConverge: false,
			apply: func(t *testing.T, ctx context.Context, h *testdb.Harness, _ []string) {
				if err := h.TransferFunctionOwnership(ctx, "vector_data.validate_vector_record()", testdb.RuntimeRole); err != nil {
					t.Fatalf("apply variant: %v", err)
				}
			},
		},
		{
			name:        "post-convergence-vector-api-owned-function-set-updated-at",
			roles:       nil,
			preConverge: false,
			apply: func(t *testing.T, ctx context.Context, h *testdb.Harness, _ []string) {
				if err := h.TransferFunctionOwnership(ctx, "vector_control.set_updated_at()", testdb.RuntimeRole); err != nil {
					t.Fatalf("apply variant: %v", err)
				}
			},
		},
		{
			name:        "post-convergence-unreachable-role-owned-migrated-table",
			roles:       []string{"U"},
			preConverge: false,
			apply: func(t *testing.T, ctx context.Context, h *testdb.Harness, scoped []string) {
				if err := h.TransferTableOwnershipToRole(ctx, "vector_data.vector_records", scoped[0]); err != nil {
					t.Fatalf("apply variant: %v", err)
				}
			},
		},
		{
			name:        "post-convergence-unreachable-role-owned-function-validate",
			roles:       []string{"U"},
			preConverge: false,
			apply: func(t *testing.T, ctx context.Context, h *testdb.Harness, scoped []string) {
				if err := h.TransferFunctionOwnershipToRole(ctx, "vector_data.validate_vector_record()", scoped[0]); err != nil {
					t.Fatalf("apply variant: %v", err)
				}
			},
		},
		{
			name:        "post-convergence-unreachable-role-owned-function-set-updated-at",
			roles:       []string{"U"},
			preConverge: false,
			apply: func(t *testing.T, ctx context.Context, h *testdb.Harness, scoped []string) {
				if err := h.TransferFunctionOwnershipToRole(ctx, "vector_control.set_updated_at()", scoped[0]); err != nil {
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
			// Provision, Converge, apply, or ReadHistory still clean up
			// the database and restore canonical role state.
			t.Cleanup(func() {
				cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cleanupCancel()

				// 1. Teardown the suite database and restore canonical roles.
				if err := h.Teardown(cleanupCtx); err != nil {
					t.Errorf("teardown: %v", err)
				}

				// 2. Drop cluster-global fixture roles through bootstrap
				// maintenance connection. These roles are cluster-global
				// and persist beyond the suite database.
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

			var historyBefore []testdb.HistoryRow

			if tc.preConverge {
				// Pre-convergence variant: apply the broken-state mutation
				// before Converge. The gate runs before bootstrap DDL, so
				// no history table exists when the gate fires.
				tc.apply(t, ctx, h, scopedRoles)

				// Attempt Converge: the ownership gate must reject.
				_, err := h.Converge(ctx)
				if err == nil {
					t.Fatal("ownership gate passed; the variant is not a rejection fixture")
				}

				// Assert the error is a structured OwnershipGateError,
				// not a generic error.
				var gateErr *migrate.OwnershipGateError
				if !errors.As(err, &gateErr) {
					t.Fatalf("expected *migrate.OwnershipGateError, got %T: %v", err, err)
				}
				if len(gateErr.Mismatches) == 0 {
					t.Fatal("OwnershipGateError has no mismatches; the gate did not identify a violation")
				}
				t.Logf("variant correctly rejected by ownership gate (identity=%s, mismatches=%d): %v",
					gateErr.Identity, len(gateErr.Mismatches), gateErr)

				// History check: distinguish between the two pre-convergence
				// fixtures. The schema-only mutation creates a schema but no
				// history table, so ReadHistory must fail with SQLSTATE 42P01.
				// The history-table mutation pre-creates an empty history table,
				// so ReadHistory must succeed with zero rows.
				if tc.historyAbsent {
					// Schema-only variant: no history table exists.
					// ReadHistory must fail with undefined_table.
					history, err := h.ReadHistory(ctx)
					if err == nil {
						t.Fatalf("expected ReadHistory to fail (no history table), got %d rows", len(history))
					}
					var pgErr *pgconn.PgError
					if !errors.As(err, &pgErr) {
						t.Fatalf("expected pgconn.PgError from ReadHistory, got %T: %v", err, err)
					}
					if pgErr.Code != "42P01" {
						t.Fatalf("expected SQLSTATE 42P01 (undefined_table), got %s: %v", pgErr.Code, err)
					}
					t.Logf("history table absent after pre-convergence rejection (expected, SQLSTATE=%s): %v",
						pgErr.Code, err)
				} else {
					// History-table variant: table exists, is readable, and is empty.
					history, err := h.ReadHistory(ctx)
					if err != nil {
						t.Fatalf("expected ReadHistory to succeed (history table exists), got error: %v", err)
					}
					if len(history) > 0 {
						t.Errorf("history has %d rows after pre-convergence rejection; expected 0", len(history))
					}
					t.Logf("history table empty after pre-convergence rejection (expected, rows=%d)", len(history))
				}
			} else {
				// Post-convergence variant: converge first (6 migrations),
				// then apply the broken-state mutation.

				// Converge through the production runner.
				res, err := h.Converge(ctx)
				if err != nil {
					t.Fatalf("initial converge: %v", err)
				}
				if len(res.Applied) != 6 {
					t.Fatalf("initial converge applied %d migrations, want 6", len(res.Applied))
				}

				// Read history before applying the variant.
				historyBefore, err = h.ReadHistory(ctx)
				if err != nil {
					t.Fatalf("read history before variant: %v", err)
				}
				if len(historyBefore) != 6 {
					t.Fatalf("history before variant has %d rows, want 6", len(historyBefore))
				}

				// Apply the broken-state variant on the converged database.
				tc.apply(t, ctx, h, scopedRoles)

				// Attempt Converge again: the ownership gate must reject.
				_, err = h.Converge(ctx)
				if err == nil {
					t.Fatal("ownership gate passed; the variant is not a rejection fixture")
				}

				// Assert the error is a structured OwnershipGateError,
				// not a generic error.
				var gateErr *migrate.OwnershipGateError
				if !errors.As(err, &gateErr) {
					t.Fatalf("expected *migrate.OwnershipGateError, got %T: %v", err, err)
				}
				if len(gateErr.Mismatches) == 0 {
					t.Fatal("OwnershipGateError has no mismatches; the gate did not identify a violation")
				}
				t.Logf("variant correctly rejected by ownership gate (identity=%s, mismatches=%d): %v",
					gateErr.Identity, len(gateErr.Mismatches), gateErr)

				// Migration history must remain unchanged after rejection.
				// The gate runs before bootstrap DDL and before any history write.
				historyAfter, err := h.ReadHistory(ctx)
				if err != nil {
					t.Fatalf("read history after rejection: %v", err)
				}
				if len(historyAfter) != len(historyBefore) {
					t.Errorf("history row count changed after rejection: before=%d, after=%d",
						len(historyBefore), len(historyAfter))
				}
				for i := range historyBefore {
					if i >= len(historyAfter) {
						break
					}
					if historyBefore[i].Version != historyAfter[i].Version ||
						historyBefore[i].Name != historyAfter[i].Name ||
						historyBefore[i].Checksum != historyAfter[i].Checksum ||
						!historyBefore[i].AppliedAt.Equal(historyAfter[i].AppliedAt) {
						t.Errorf("history row %d changed after rejection: %v -> %v",
							i, historyBefore[i], historyAfter[i])
					}
				}
			}
		})
	}
}
