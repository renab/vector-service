//go:build integration

// Package namespaces_test holds the integration tests for namespace
// resolution. The tests live in the external test package so the suite can
// compose the namespace resolution (this package) with the vector-space
// resolution (internal/vectors) without an import cycle: vectors imports
// namespaces, so an in-package test file could not import it back.
package namespaces_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"vector-service/internal/dbctx"
	"vector-service/internal/namespaces"
	"vector-service/internal/testdb"
	"vector-service/internal/vectors"
)

// The real-PostgreSQL namespace isolation matrix (package 2, Validation,
// "Namespace resolution"): missing key → 404; disabled namespace → 404;
// another application's namespace key (same key, foreign app) → 404; the
// three responses are uniform. Every case runs inside the composed
// Resolve — the canonical path of this package — over a real pool
// connected as the runtime role, so the real schema, the forced RLS
// policy, and the transaction-local set_config are exercised end to end.
//
// The fixture registers two generic applications (the repository's standard
// isolation fixture names, AGENTS.md) directly through bootstrap SQL: the
// service exposes no namespace-registration endpoint yet (the package-4
// admin plane), so the test's own SQL is the fixture path — the same
// boundary the auth integration suite uses.

// resolveFixture is one test's provisioned suite: the runtime pool under
// test, the bootstrap connection for fixture SQL, and the registered
// identities.
type resolveFixture struct {
	pool      *pgxpool.Pool
	bootstrap *pgx.Conn

	appA string // application A's UUID
	appB string // application B's UUID
}

// setupResolveFixture builds the suite fixture. It skips the test when the
// bootstrap identity is not configured, so the unit layer stays green on a
// host with no database.
func setupResolveFixture(t *testing.T) *resolveFixture {
	t.Helper()
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
	res, err := h.Converge(ctx)
	if err != nil {
		t.Fatalf("converge: %v", err)
	}
	if len(res.Applied) != 6 {
		t.Fatalf("converge applied %d migrations, want 6", len(res.Applied))
	}

	// The pool under test connects as the canonical runtime role to the
	// suite database over the endpoint the bootstrap identity reached —
	// the same construction the auth integration suite uses, so the real
	// grants and RLS apply to the pool's connections.
	runtimeConn, rerr := h.RuntimeConn(ctx)
	if rerr != nil {
		t.Fatalf("runtime connect: %v", rerr)
	}
	connCfg := runtimeConn.Config()
	runtimeConn.Close(ctx)
	pc, perr := pgxpool.ParseConfig(
		fmt.Sprintf("host=%s port=%d dbname=%s user=%s sslmode=%s",
			connCfg.Host, connCfg.Port, connCfg.Database, connCfg.User,
			sslModeForPool(connCfg)))
	if perr != nil {
		t.Fatalf("parse runtime pool config: %v", perr)
	}
	pool, perr := pgxpool.NewWithConfig(ctx, pc)
	if perr != nil {
		t.Fatalf("open runtime pool: %v", perr)
	}
	t.Cleanup(pool.Close)

	// The bootstrap connection runs the fixture SQL as the bootstrap
	// identity (the superuser the operator supplied): it bypasses RLS,
	// which is exactly what a fixture path outside the service needs.
	bootstrap, berr := h.BootstrapConn(ctx, testdb.DatabaseName)
	if berr != nil {
		t.Fatalf("bootstrap connect: %v", berr)
	}
	t.Cleanup(func() { bootstrap.Close(context.Background()) })

	fx := &resolveFixture{
		pool:      pool,
		bootstrap: bootstrap,
		appA:      "0199f31e-4000-7000-8000-0000000000a1",
		appB:      "0199f31e-4000-7000-8000-0000000000a2",
	}
	if err := fx.insertApplication(ctx, fx.appA, testdb.AppMemoryService); err != nil {
		t.Fatalf("register application A: %v", err)
	}
	if err := fx.insertApplication(ctx, fx.appB, testdb.AppNotesService); err != nil {
		t.Fatalf("register application B: %v", err)
	}
	return fx
}

// insertApplication registers an application row.
func (f *resolveFixture) insertApplication(ctx context.Context, id, key string) error {
	_, err := f.bootstrap.Exec(ctx,
		`INSERT INTO vector_control.applications (id, application_key, display_name)
		 VALUES ($1, $2, $3)`,
		id, key, key+" (fixture)")
	return err
}

// insertNamespace registers a namespace row for the named application.
func (f *resolveFixture) insertNamespace(ctx context.Context, appID, key string) error {
	id, err := testdb.NewUUID()
	if err != nil {
		return err
	}
	_, err = f.bootstrap.Exec(ctx,
		`INSERT INTO vector_control.namespaces (id, application_id, namespace_key, display_name)
		 VALUES ($1, $2, $3, $4)`,
		id, appID, key, key+" (fixture)")
	return err
}

// disableNamespace disables the namespace row for (app, key).
func (f *resolveFixture) disableNamespace(ctx context.Context, appID, key string) error {
	_, err := f.bootstrap.Exec(ctx,
		`UPDATE vector_control.namespaces SET enabled = false
		 WHERE application_id = $1 AND namespace_key = $2`,
		appID, key)
	return err
}

// TestResolveNamespaceIsolation is the real-PostgreSQL namespace isolation
// matrix: a missing key, a disabled namespace, and a foreign namespace
// (the same key registered under another application) all yield the
// uniform typed 404 outcome, while the application's own enabled namespace
// resolves with its UUID and key.
func TestResolveNamespaceIsolation(t *testing.T) {
	fx := setupResolveFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	if err := fx.insertNamespace(ctx, fx.appA, "project-alpha"); err != nil {
		t.Fatalf("register application A namespace: %v", err)
	}
	if err := fx.insertNamespace(ctx, fx.appA, "research"); err != nil {
		t.Fatalf("register application A second namespace: %v", err)
	}
	if err := fx.disableNamespace(ctx, fx.appA, "research"); err != nil {
		t.Fatalf("disable research namespace: %v", err)
	}
	// Register "project-alpha" only for appA. AppB does not have this
	// namespace, so appB resolving "project-alpha" is the foreign-namespace
	// case (uniform 404).

	cases := []struct {
		name    string
		appID   string // the authenticated application
		key     string
		wantErr bool
	}{
		{"missing key", fx.appA, "does-not-exist", true},
		{"disabled namespace", fx.appA, "research", true},
		{"foreign namespace (same key, other app)", fx.appB, "project-alpha", true},
		{"valid own namespace", fx.appA, "project-alpha", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reqCtx := dbctx.WithAppID(context.Background(), tc.appID)
			_, res, err := namespaces.Resolve(reqCtx, fx.pool, tc.key)
			if tc.wantErr {
				if !errors.Is(err, namespaces.ErrNotFound) {
					t.Fatalf("Resolve error = %v, want ErrNotFound (uniform 404)", err)
				}
				if res != (namespaces.Result{}) {
					t.Fatalf("failure returned a result: %+v; a 404 must carry no identity", res)
				}
				return
			}
			if err != nil {
				t.Fatalf("Resolve error = %v, want nil", err)
			}
			if res.NamespaceID == "" || res.Key != tc.key {
				t.Fatalf("resolved = %+v, want a UUID and key %q", res, tc.key)
			}
			// The resolved UUID must be the one registered for this
			// application's namespace, not the foreign one.
			var nsID string
			if err := fx.bootstrap.QueryRow(ctx,
				`SELECT id FROM vector_control.namespaces
				 WHERE application_id = $1 AND namespace_key = $2`,
				tc.appID, tc.key).Scan(&nsID); err != nil {
				t.Fatalf("read expected namespace id: %v", err)
			}
			if res.NamespaceID != nsID {
				t.Fatalf("resolved namespace id = %s, want %s", res.NamespaceID, nsID)
			}
		})
	}

	// The "disabled namespace" subtest above disabled appA's "research"
	// row. Restore it so later runs of the suite (if any) observe the
	// canonical fixture state; the database is dropped at teardown, but the
	// restore keeps this test order-independent if it is extended.
	if _, err := fx.bootstrap.Exec(ctx,
		`UPDATE vector_control.namespaces SET enabled = true
		 WHERE application_id = $1 AND namespace_key = $2`,
		fx.appA, "research"); err != nil {
		t.Fatalf("restore research namespace: %v", err)
	}
}

// TestResolveUniform404AcrossFailureKinds proves the three failure shapes
// (missing, disabled, foreign) settle to the identical typed outcome: the
// same error value, the same stable message, and no identity in it — the
// API deliberately does not reveal whether a namespace exists under
// another application.
func TestResolveUniform404AcrossFailureKinds(t *testing.T) {
	fx := setupResolveFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	if err := fx.insertNamespace(ctx, fx.appA, "project-alpha"); err != nil {
		t.Fatalf("register application A namespace: %v", err)
	}
	// Do not register "project-alpha" for appB. The "foreign" subtest
	// resolves "project-alpha" as appB; since appB has no such namespace,
	// the resolution returns the uniform 404 (the same outcome as missing
	// or disabled). This exercises the indistinguishability requirement.
	if err := fx.insertNamespace(ctx, fx.appA, "archive"); err != nil {
		t.Fatalf("register application A disabled candidate: %v", err)
	}
	if err := fx.disableNamespace(ctx, fx.appA, "archive"); err != nil {
		t.Fatalf("disable archive: %v", err)
	}

	var outcomes []string
	for name, entry := range map[string][2]string{
		"missing":  {fx.appA, "does-not-exist"},
		"disabled": {fx.appA, "archive"},
		"foreign":  {fx.appB, "project-alpha"},
	} {
		t.Run(name, func(t *testing.T) {
			reqCtx := dbctx.WithAppID(context.Background(), entry[0])
			_, res, err := namespaces.Resolve(reqCtx, fx.pool, entry[1])
			if !errors.Is(err, namespaces.ErrNotFound) {
				t.Fatalf("Resolve error = %v, want ErrNotFound", err)
			}
			if res != (namespaces.Result{}) {
				t.Fatalf("failure returned a result: %+v", res)
			}
			if err.Error() != namespaces.MessageNotFound {
				t.Fatalf("failure message = %q, want the uniform message %q", err.Error(), namespaces.MessageNotFound)
			}
			outcomes = append(outcomes, err.Error())
		})
	}
	// All three failure shapes render the identical message, so the 404
	// body the renderer writes is byte-identical (uniform 404, no
	// enumeration).
	if len(outcomes) != 3 || outcomes[0] != outcomes[1] || outcomes[1] != outcomes[2] {
		t.Fatalf("failure messages differ across shapes: %v", outcomes)
	}
}

// TestResolveAppContextLeak is the pooled-connection-reuse isolation case:
// the transaction-local application context does not leak across requests: after a resolution for
// application A, a resolution for application B (on the same pool, which
// may reuse the same physical connection) sees no A rows — B's own
// namespace resolves, and A's namespace key is a uniform 404 for B. This
// is the pooled-connection-reuse half of the package-2 isolation
// expectation: the set_config third argument stays true (transaction
// scope), so a connection returned to the pool carries no residual
// application context.
func TestResolveAppContextLeak(t *testing.T) {
	fx := setupResolveFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	if err := fx.insertNamespace(ctx, fx.appA, "project-alpha"); err != nil {
		t.Fatalf("register application A namespace: %v", err)
	}
	if err := fx.insertNamespace(ctx, fx.appB, "project-beta"); err != nil {
		t.Fatalf("register application B namespace: %v", err)
	}

	// Resolve for A first, then immediately for B on the same pool: if
	// the application context leaked to session scope, B's lookup would
	// see A's rows (or its own set_config would be redundant) — the
	// uniform 404 for A's key under B's identity is the observable
	// invariant.
	if _, _, err := namespaces.Resolve(dbctx.WithAppID(context.Background(), fx.appA), fx.pool, "project-alpha"); err != nil {
		t.Fatalf("resolve for app A: %v", err)
	}
	if _, _, err := namespaces.Resolve(dbctx.WithAppID(context.Background(), fx.appB), fx.pool, "project-beta"); err != nil {
		t.Fatalf("resolve for app B after app A on the same pool: %v", err)
	}
	if _, _, err := namespaces.Resolve(dbctx.WithAppID(context.Background(), fx.appB), fx.pool, "project-alpha"); !errors.Is(err, namespaces.ErrNotFound) {
		t.Fatalf("app B resolving app A's namespace key = %v, want ErrNotFound (the context must not leak)", err)
	}
}

// TestResolveInsideAppContextResolves is the canonical ordering test: the
// composed Resolve runs the namespace lookup inside the WithAppContext
// transaction (after BEGIN and the transaction-local set_config), the only
// order in which vector_api can see any namespaces rows under the forced
// RLS policy (a pre-transaction lookup sees no rows). It proves the
// positive half — the composed path resolves a real namespace and returns
// a context carrying both the app identity and the resolved namespace for
// package-3 operations and package-4 logging. The negative half (a raw
// pre-transaction lookup sees 0 rows) is the package-2 "RLS active, no
// context" matrix case, exercised by the dbctx/auth integration suites.
func TestResolveInsideAppContextResolves(t *testing.T) {
	fx := setupResolveFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	if err := fx.insertNamespace(ctx, fx.appA, "project-alpha"); err != nil {
		t.Fatalf("register namespace: %v", err)
	}

	resolvedCtx, res, err := namespaces.Resolve(dbctx.WithAppID(context.Background(), fx.appA), fx.pool, "project-alpha")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if res.NamespaceID == "" || res.Key != "project-alpha" {
		t.Fatalf("resolved = %+v, want a non-empty UUID and key", res)
	}
	// The resolved context carries both the app identity and the
	// resolved namespace for the operation and the logging state.
	if dbctx.AppID(resolvedCtx) != fx.appA {
		t.Fatalf("resolved context lost the app identity: %q", dbctx.AppID(resolvedCtx))
	}
	got, ok := namespaces.Namespace(resolvedCtx)
	if !ok || got != res {
		t.Fatalf("Namespace(resolvedCtx) = (%+v, %v), want (%+v, true)", got, ok, res)
	}
}

// TestResolveInTxComposesInOneTransaction is the package-3 composition
// test: namespace resolution (ResolveInTx), vector-space resolution
// (vectors.ResolveInTx), and a data-plane statement all run inside ONE
// WithAppContext transaction, so they share the single transaction-local
// application context the helper established as its first statement.
//
// It proves the positive half (the composed path sees the namespace, the
// seeded space, and the record row) and the negative half (the transaction
// rolls back on any failure, so the business write is absent afterwards —
// the all-or-nothing property of the package-3 upsert). The fixture
// exercises the seeded space row from migration 0002; its state is
// canonical (enabled, not retired).
func TestResolveInTxComposesInOneTransaction(t *testing.T) {
	fx := setupResolveFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	if err := fx.insertNamespace(ctx, fx.appA, "project-alpha"); err != nil {
		t.Fatalf("register namespace: %v", err)
	}

	const (
		objectID     = "fixture-object-1"
		projectionID = "fixture-projection-1"
	)
	// 32 zero bytes as raw bytes (the column is bytea): an obviously
	// synthetic content hash that satisfies the schema's 32-byte CHECK.
	contentHash := make([]byte, 32)
	embeddingText := testdb.EncodeVectorText(testdb.BasisVector(0))
	recordID, rerr := testdb.NewUUID()
	if rerr != nil {
		t.Fatalf("fixture uuid: %v", rerr)
	}

	// The composed success path: one WithAppContext transaction running
	// namespace resolution first, space resolution second, then a
	// data-plane INSERT. Commit.
	err := dbctx.WithAppContext(dbctx.WithAppID(context.Background(), fx.appA), fx.pool, fx.appA,
		func(tx pgx.Tx) error {
			reqCtx := dbctx.WithAppID(context.Background(), fx.appA)
			ns, err := namespaces.ResolveInTx(reqCtx, tx, "project-alpha")
			if err != nil {
				return err
			}
			space, err := vectors.ResolveInTx(reqCtx, tx, testdb.SeededSpaceKey, true)
			if err != nil {
				return err
			}
			if _, err := tx.Exec(reqCtx, `
				INSERT INTO vector_data.vector_records
					(id, application_id, namespace_id, object_id, projection_id,
					 vector_space_id, content_hash, metadata, embedding)
				VALUES ($1, $2, $3, $4, $5, $6, $7, '{}'::jsonb, $8::vector)`,
				recordID, fx.appA, ns.NamespaceID, objectID, projectionID,
				space.ID, contentHash, embeddingText); err != nil {
				return err
			}
			return nil
		})
	if err != nil {
		t.Fatalf("composed transaction: %v", err)
	}

	// The record is visible: the composed path wrote and committed it.
	var rows int
	if err := fx.bootstrap.QueryRow(ctx,
		`SELECT count(*) FROM vector_data.vector_records
		 WHERE application_id = $1 AND object_id = $2`,
		fx.appA, objectID).Scan(&rows); err != nil {
		t.Fatalf("count records: %v", err)
	}
	if rows != 1 {
		t.Fatalf("record rows after commit = %d, want 1", rows)
	}

	// The rollback half: a second composed transaction whose business SQL
	// fails must leave no trace — the namespace and space resolved fine,
	// the INSERT's dimension violation (the trigger's defense in depth)
	// rolls the whole transaction back, and no row appears.
	wrongDimsText := testdb.EncodeVectorText(testdb.BasisVector(0)[:3]) // 3 dims, not 1024
	badRecordID, rerr := testdb.NewUUID()
	if rerr != nil {
		t.Fatalf("fixture uuid: %v", rerr)
	}
	if err := dbctx.WithAppContext(dbctx.WithAppID(context.Background(), fx.appA), fx.pool, fx.appA,
		func(tx pgx.Tx) error {
			reqCtx := dbctx.WithAppID(context.Background(), fx.appA)
			ns, err := namespaces.ResolveInTx(reqCtx, tx, "project-alpha")
			if err != nil {
				return err
			}
			space, err := vectors.ResolveInTx(reqCtx, tx, testdb.SeededSpaceKey, true)
			if err != nil {
				return err
			}
			_, err = tx.Exec(reqCtx, `
				INSERT INTO vector_data.vector_records
					(id, application_id, namespace_id, object_id, projection_id,
					 vector_space_id, content_hash, metadata, embedding)
				VALUES ($1, $2, $3, $4, $5, $6, $7, '{}'::jsonb, $8::vector)`,
				badRecordID, fx.appA, ns.NamespaceID, objectID, "fixture-projection-2",
				space.ID, contentHash, wrongDimsText)
			return err
		}); err == nil {
		t.Fatal("composed transaction with a dimension-mismatched embedding committed, want a rollback")
	}
	if err := fx.bootstrap.QueryRow(ctx,
		`SELECT count(*) FROM vector_data.vector_records
		 WHERE application_id = $1 AND object_id = $2`,
		fx.appA, objectID).Scan(&rows); err != nil {
		t.Fatalf("count records after rollback: %v", err)
	}
	if rows != 1 {
		t.Fatalf("record rows after the failed transaction = %d, want still 1 (all-or-nothing rollback)", rows)
	}
}

// sslModeForPool returns the sslmode for the test pool.
// The harness always uses TLSModePlain (VEC_PG_TLS_MODE=plain), so test
// pools must explicitly disable SSL.
func sslModeForPool(_ *pgx.ConnConfig) string {
	return "disable"
}
