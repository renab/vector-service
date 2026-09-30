//go:build integration

package auth

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"vector-service/internal/dbctx"
	"vector-service/internal/testdb"
)

// The real-PostgreSQL matrices of the authentication slice (package 2,
// Validation). Every test provisions the disposable suite database through
// the harness (provision + converge through the production runner) and
// registers two generic applications with credentials directly through SQL
// as the bootstrap identity: the service exposes no credential-creation
// endpoint yet (the package-4 admin plane), so the test's own SQL is the
// fixture path. The Lookup under test then runs over a real pool connected
// as the runtime role, so the real schema, constraints, and grants are
// exercised end to end.
//
// Fixture identities follow the repository's generic test conventions
// (AGENTS.md): obviously synthetic, no real content, no secrets.

// authFixture is one test's provisioned suite: the runtime pool under test,
// the bootstrap connection for fixture SQL, and the registered identities.
type authFixture struct {
	pool      *pgxpool.Pool
	bootstrap *pgx.Conn

	appA  string // application A's UUID
	appB  string // application B's UUID
	appAC string // the application A credential (the valid baseline)
}

// setupAuthFixture builds the suite fixture. It skips the test when the
// bootstrap identity is not configured, so the unit layer stays green on a
// host with no database.
func setupAuthFixture(t *testing.T) *authFixture {
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

	bootstrap, err := h.BootstrapConn(ctx, testdb.DatabaseName)
	if err != nil {
		t.Fatalf("bootstrap connect: %v", err)
	}
	t.Cleanup(func() { bootstrap.Close(context.Background()) })

	// The pool under test connects as the canonical runtime role to the
	// suite database over the endpoint the bootstrap identity reached.
	// The harness's RuntimeConn returns a single connection built from the
	// harness's own ConnConfig with the runtime role and suite database;
	// we derive the pool's connection config from that connection so the
	// pool is bound to the same endpoint without re-deriving host/port.
	runtimeConn, rerr := h.RuntimeConn(ctx)
	if rerr != nil {
		t.Fatalf("runtime connect: %v", rerr)
	}
	connCfg := runtimeConn.Config()
	runtimeConn.Close(ctx)
	// Build a pool from the same connection settings: the host, port,
	// database, and user of the harness's runtime connection. SSL is
	// explicitly disabled for the test pool (the harness uses
	// TLSModePlain, so the pool uses sslmode=disable).
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

	fx := &authFixture{
		pool:      pool,
		bootstrap: bootstrap,
		appA:      "0199f31e-2000-7000-8000-0000000000a1",
		appB:      "0199f31e-2000-7000-8000-0000000000a2",
	}
	if err := fx.insertApplication(ctx, fx.appA, testdb.AppMemoryService); err != nil {
		t.Fatalf("register application A: %v", err)
	}
	if err := fx.insertApplication(ctx, fx.appB, testdb.AppNotesService); err != nil {
		t.Fatalf("register application B: %v", err)
	}
	fx.appAC, _, err = fx.registerCredentialRow(ctx, fx.appA, "primary")
	if err != nil {
		t.Fatalf("register application A credential: %v", err)
	}
	return fx
}

// insertApplication registers an application row.
func (f *authFixture) insertApplication(ctx context.Context, id, key string) error {
	_, err := f.bootstrap.Exec(ctx,
		`INSERT INTO vector_control.applications (id, application_key, display_name)
		 VALUES ($1, $2, $3)`,
		id, key, key+" (fixture)")
	return err
}

// registerCredentialRow generates a service credential, persists only its
// digest (the raw value is never stored — the 32-byte digest is the only
// persisted form), and returns the row ID and the raw value. The raw value
// is a synthetic fixture secret; it is presented to Lookup and never
// logged or embedded in a failure message.
func (f *authFixture) registerCredentialRow(ctx context.Context, appID, name string) (string, string, error) {
	raw, err := GenerateCredential()
	if err != nil {
		return "", "", err
	}
	id, err := testdb.NewUUID()
	if err != nil {
		return "", "", err
	}
	if _, err := f.bootstrap.Exec(ctx,
		`INSERT INTO vector_control.application_credentials (id, application_id, credential_name, credential_hash)
		 VALUES ($1, $2, $3, $4)`,
		id, appID, name, bytea(Digest(raw))); err != nil {
		return "", "", err
	}
	return id, raw, nil
}

// TestLookupUniform401Matrix is the real-PostgreSQL uniform-401 matrix:
// unknown digest, disabled credential, expired credential, and disabled
// application all yield the uniform 401 with no identity attached, while a
// valid credential — including one with a future expiry — and the
// non-expiring NULL-expiry default authenticate with the identity attached.
// Each failure fixture differs from the valid baseline in exactly the field
// under test, and every failure shape renders the identical code and
// message (one code, one status, no enumeration).
func TestLookupUniform401Matrix(t *testing.T) {
	fx := setupAuthFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// Baseline: a fresh valid credential of an enabled application.
	baselineID, baselineRaw, err := fx.registerCredentialRow(ctx, fx.appA, "valid")
	if err != nil {
		t.Fatalf("register baseline credential: %v", err)
	}

	failures := []struct {
		name string
		seam string // bootstrap SQL executed on the fixture row after insert
	}{
		{
			name: "disabled credential",
			seam: `UPDATE vector_control.application_credentials
			       SET enabled = false WHERE id = $1`,
		},
		{
			// The fixture writes the column directly as the bootstrap
			// identity: the service exposes no expiry-setting endpoint, so
			// the column is the fixture seam (a past expiry). The schema
			// constraint requires expires_at > created_at, so the seam
			// backdates created_at to satisfy it.
			name: "expired credential",
			seam: `UPDATE vector_control.application_credentials
			       SET created_at = now() - interval '2 hours',
	                   expires_at = now() - interval '1 minute'
			       WHERE id = $1`,
		},
		{
			name: "disabled application",
			seam: `UPDATE vector_control.applications
			       SET enabled = false
			       WHERE id = (SELECT application_id
				         FROM vector_control.application_credentials
				         WHERE id = $1)`,
		},
	}

	for i, tc := range failures {
		t.Run(tc.name, func(t *testing.T) {
			credID, raw, err := fx.registerCredentialRow(ctx, fx.appA, fmt.Sprintf("case-%d", i))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := fx.bootstrap.Exec(ctx, tc.seam, credID); err != nil {
				t.Fatalf("apply seam: %v", err)
			}
			res, err := Lookup(ctx, fx.pool, raw)
			if !errors.Is(err, ErrUnauthorized) {
				t.Fatalf("Lookup error = %v, want ErrUnauthorized", err)
			}
			if res != (AuthResult{}) {
				t.Fatalf("failure returned a result: %+v; a 401 must carry no identity", res)
			}
		})
	}

	// Unknown digest: a credential whose digest no row carries.
	t.Run("unknown digest", func(t *testing.T) {
		unknown, err := GenerateCredential()
		if err != nil {
			t.Fatal(err)
		}
		res, err := Lookup(ctx, fx.pool, unknown)
		if !errors.Is(err, ErrUnauthorized) {
			t.Fatalf("Lookup error = %v, want ErrUnauthorized", err)
		}
		if res != (AuthResult{}) {
			t.Fatalf("failure returned a result: %+v; a 401 must carry no identity", res)
		}
	})

	// Uniformity: every failure shape renders the identical code and
	// message, so the 401 body the renderer writes is byte-identical, and
	// the error carries no credential material or identity.
	t.Run("uniform 401 across all failure shapes", func(t *testing.T) {
		if ErrUnauthorized.Error() != MessageUnauthorized {
			t.Fatalf("error message %q is not the uniform unauthorized message %q",
				ErrUnauthorized.Error(), MessageUnauthorized)
		}
	})

	t.Run("valid credential", func(t *testing.T) {
		// The "disabled application" subtest above disabled appA as its
		// seam. Restore it so the baseline credential (which belongs to
		// appA) can authenticate.
		if _, err := fx.bootstrap.Exec(ctx,
			`UPDATE vector_control.applications SET enabled = true WHERE id = $1`,
			fx.appA); err != nil {
			t.Fatalf("restore appA: %v", err)
		}
		res, err := Lookup(ctx, fx.pool, baselineRaw)
		if err != nil {
			t.Fatalf("valid credential failed: %v", err)
		}
		if res.ApplicationID != fx.appA {
			t.Fatalf("application ID = %s, want %s", res.ApplicationID, fx.appA)
		}
		if res.CredentialID != baselineID {
			t.Fatalf("credential ID = %s, want %s", res.CredentialID, baselineID)
		}
	})

	// Complementary gate cases: the expiry check must not over-reject.
	t.Run("future expiry authenticates", func(t *testing.T) {
		credID, raw, err := fx.registerCredentialRow(ctx, fx.appB, "future-expiry")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := fx.bootstrap.Exec(ctx,
			`UPDATE vector_control.application_credentials
			   SET expires_at = now() + interval '1 hour' WHERE id = $1`, credID); err != nil {
			t.Fatalf("set future expiry: %v", err)
		}
		res, err := Lookup(ctx, fx.pool, raw)
		if err != nil {
			t.Fatalf("future-expiry credential failed: %v", err)
		}
		if res.ApplicationID != fx.appB {
			t.Fatalf("application ID = %s, want %s", res.ApplicationID, fx.appB)
		}
	})

	t.Run("null expiry is the non-expiring default", func(t *testing.T) {
		credID, raw, err := fx.registerCredentialRow(ctx, fx.appB, "no-expiry")
		if err != nil {
			t.Fatal(err)
		}
		var storedExpiry *time.Time
		if err := fx.bootstrap.QueryRow(ctx,
			`SELECT expires_at FROM vector_control.application_credentials WHERE id = $1`,
			credID).Scan(&storedExpiry); err != nil {
			t.Fatal(err)
		}
		if storedExpiry != nil {
			t.Fatal("schema default: expires_at must be NULL for a newly registered credential")
		}
		res, err := Lookup(ctx, fx.pool, raw)
		if err != nil {
			t.Fatalf("non-expiring credential failed: %v", err)
		}
		if res.ApplicationID != fx.appB {
			t.Fatalf("application ID = %s, want %s", res.ApplicationID, fx.appB)
		}
	})
}

// TestLookupLastUsedAt is the real-PostgreSQL last_used_at matrix: the
// first successful lookup stamps the row (the stored value is NULL before
// the first use), a second lookup inside the throttle window does not
// rewrite it, a stamp forced outside the window is rewritten, and making
// the UPDATE fail leaves the request succeeding (the stamp is best-effort:
// warn-logged and dropped, never failing or changing the outcome).
func TestLookupLastUsedAt(t *testing.T) {
	fx := setupAuthFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	credID, raw, err := fx.registerCredentialRow(ctx, fx.appA, "usage")
	if err != nil {
		t.Fatal(err)
	}

	// First lookup: the stored value is NULL before use, so the update is
	// due and stamps the row.
	res, err := Lookup(ctx, fx.pool, raw)
	if err != nil {
		t.Fatalf("first lookup: %v", err)
	}
	if res.LastUsedAt != nil {
		t.Fatalf("first lookup saw a stored last_used_at = %v, want nil (never stamped)", *res.LastUsedAt)
	}
	first, err := fx.storedLastUsedAt(ctx, credID)
	if err != nil {
		t.Fatal(err)
	}
	if first == nil {
		t.Fatal("first lookup did not stamp last_used_at; a NULL stored value is always due")
	}

	// Second lookup inside the throttle window: the UPDATE must not run,
	// so the stored value is unchanged.
	time.Sleep(20 * time.Millisecond)
	res2, err := Lookup(ctx, fx.pool, raw)
	if err != nil {
		t.Fatalf("second lookup: %v", err)
	}
	if res2.LastUsedAt == nil {
		t.Fatal("second lookup saw a nil last_used_at after the first stamp")
	}
	second, err := fx.storedLastUsedAt(ctx, credID)
	if err != nil {
		t.Fatal(err)
	}
	if second == nil || !second.Equal(*first) {
		t.Fatalf("second lookup rewrote last_used_at (throttle failed): first %v, second %v", *first, second)
	}

	// Force the stamp outside the window and verify the rewrite.
	if _, err := fx.bootstrap.Exec(ctx,
		`UPDATE vector_control.application_credentials
		   SET last_used_at = now() - interval '2 hours' WHERE id = $1`,
		credID); err != nil {
		t.Fatalf("age the stamp: %v", err)
	}
	_, err = Lookup(ctx, fx.pool, raw)
	if err != nil {
		t.Fatalf("third lookup: %v", err)
	}
	third, err := fx.storedLastUsedAt(ctx, credID)
	if err != nil {
		t.Fatal(err)
	}
	if third == nil || third.Equal(*first) {
		t.Fatalf("out-of-window stamp was not rewritten: first %v, third %v", *first, third)
	}

	// Making the UPDATE fail leaves the request succeeding: revoke the
	// runtime role's UPDATE on the table, re-age the stamp so the update is
	// due, and the lookup must still authenticate (the stamp failure is
	// warn-logged and dropped).
	if _, err := fx.bootstrap.Exec(ctx,
		`REVOKE UPDATE ON vector_control.application_credentials FROM vector_api;`); err != nil {
		t.Fatalf("revoke UPDATE: %v", err)
	}
	// Verify the revocation is effective: vector_api must no longer have
	// UPDATE on the table. The pool connects as vector_api, so the lookup's
	// best-effort UPDATE must fail with permission denied.
	var hasUpdate bool
	if err := fx.bootstrap.QueryRow(ctx,
		`SELECT has_table_privilege('vector_api',
			'vector_control.application_credentials', 'UPDATE')`).Scan(&hasUpdate); err != nil {
		t.Fatalf("check vector_api UPDATE privilege: %v", err)
	}
	if hasUpdate {
		t.Fatal("REVOKE did not remove UPDATE from vector_api")
	}
	t.Cleanup(func() {
		cctx, ccancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer ccancel()
		// Restore the canonical grant (the harness drops the database at
		// teardown; the restore keeps this test order-independent).
		_, _ = fx.bootstrap.Exec(cctx,
			`GRANT UPDATE ON vector_control.application_credentials TO vector_api;`)
	})
	if _, err := fx.bootstrap.Exec(ctx,
		`UPDATE vector_control.application_credentials
		   SET last_used_at = now() - interval '2 hours' WHERE id = $1`,
		credID); err != nil {
		t.Fatalf("age the stamp: %v", err)
	}
	// Capture the re-aged value: the bootstrap (superuser) SET this to
	// now() - 2h. The Lookup's best-effort UPDATE must fail (REVOKE is
	// effective), so the stored value must remain at this re-aged value.
	aged, err := fx.storedLastUsedAt(ctx, credID)
	if err != nil {
		t.Fatal(err)
	}
	if aged == nil {
		t.Fatal("re-age left last_used_at as NULL")
	}
	res4, err := Lookup(ctx, fx.pool, raw)
	if err != nil {
		t.Fatalf("lookup with a failing stamp: %v; a stamp failure must not fail the request", err)
	}
	if res4.ApplicationID != fx.appA {
		t.Fatalf("application ID = %s, want %s", res4.ApplicationID, fx.appA)
	}
	// The stored value must be unchanged from the re-aged value: the
	// Lookup's UPDATE failed (permission denied), so it left the row at
	// now() - 2h.
	fourth, err := fx.storedLastUsedAt(ctx, credID)
	if err != nil {
		t.Fatal(err)
	}
	if fourth == nil {
		t.Fatal("fourth lookup left last_used_at as NULL")
	}
	if !fourth.Equal(*aged) {
		t.Fatalf("last_used_at changed despite the failed stamp: aged UTC=%s, fourth UTC=%s",
			aged.UTC().Format(time.RFC3339Nano), fourth.UTC().Format(time.RFC3339Nano))
	}
}

// TestLookupInfraFailureIsUnavailable is the typed-outcome case: with the
// database unreachable, the lookup's error must be the typed
// database-infrastructure outcome (503), not the uniform 401 — the two
// outcomes are distinct and the 401 is reserved for credential failures.
func TestLookupInfraFailureIsUnavailable(t *testing.T) {
	pc, err := pgxpool.ParseConfig(
		"host=127.0.0.1 port=1 dbname=" + testdb.DatabaseName + " user=" + testdb.RuntimeRole +
			" sslmode=disable connect_timeout=2")
	if err != nil {
		t.Fatalf("parse unreachable pool config: %v", err)
	}
	pool, err := pgxpool.NewWithConfig(context.Background(), pc)
	if err != nil {
		t.Fatalf("open unreachable pool: %v", err)
	}
	defer pool.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	cred, err := GenerateCredential()
	if err != nil {
		t.Fatal(err)
	}
	_, err = Lookup(ctx, pool, cred)
	if err == nil {
		t.Fatal("lookup against an unreachable database succeeded")
	}
	if errors.Is(err, ErrUnauthorized) {
		t.Fatalf("infrastructure failure rendered as the uniform 401: %v", err)
	}
	if !IsUnavailable(err) && !isDBCtxUnavailable(err) {
		t.Fatalf("infrastructure failure = %v, want the typed unavailable outcome", err)
	}
}

// sslModeForPool returns the sslmode for the test pool.
// The harness always uses TLSModePlain (VEC_PG_TLS_MODE=plain), so test
// pools must explicitly disable SSL.
func sslModeForPool(_ *pgx.ConnConfig) string {
	return "disable"
}

// bytea renders the 32-byte SHA-256 digest as a PostgreSQL bytea hex literal
// (the \x-prefixed form) for binding as a PostgreSQL bytea parameter. The
// raw 32 bytes are never logged.
func bytea(d [CredentialDigestLen]byte) string {
	return "\\x" + hex.EncodeToString(d[:])
}

// isDBCtxUnavailable reports whether err is the dbctx typed 503 outcome
// (the helper-boundary outcome for every infrastructure failure while the
// context is active). The auth.IsUnavailable check covers the auth package's
// own typed outcome; this covers the dbctx one.
func isDBCtxUnavailable(err error) bool {
	var u *dbctx.Unavailable
	return errors.As(err, &u)
}

// storedLastUsedAt reads the stored last_used_at of a credential row.
func (f *authFixture) storedLastUsedAt(ctx context.Context, credID string) (*time.Time, error) {
	var v *time.Time
	if err := f.bootstrap.QueryRow(ctx,
		`SELECT last_used_at FROM vector_control.application_credentials WHERE id = $1`,
		credID).Scan(&v); err != nil {
		return nil, err
	}
	return v, nil
}
