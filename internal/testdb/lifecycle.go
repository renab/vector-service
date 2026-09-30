package testdb

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"

	"vector-service/internal/migrate"
)

// connect builds a connection from the bootstrap connection settings as the
// named user and database.
func (h *Harness) connect(ctx context.Context, user, dbname string) (*pgx.Conn, error) {
	cfg := h.cfg.Copy()
	cfg.User = user
	cfg.Database = dbname
	// The privilege catalog and assertion layers parse query results as raw
	// text fields (the privilege.Querier contract); pgx's default extended
	// protocol returns binary results, which violate it. The simple query
	// protocol is text-only, so every result on these connections is text.
	cfg.DefaultQueryExecMode = pgx.QueryExecModeSimpleProtocol
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("testdb: connect as %q to %q: %w", user, dbname, err)
	}
	return conn, nil
}

// BootstrapConn returns a connection as the test bootstrap identity to the
// named database (the maintenance database for cluster-level work, the
// suite database for fixture DDL).
func (h *Harness) BootstrapConn(ctx context.Context, dbname string) (*pgx.Conn, error) {
	return h.connect(ctx, h.cfg.User, dbname)
}

// MigrationConn returns a connection to the suite database as the
// canonical migration identity, the role the migration step runs as.
func (h *Harness) MigrationConn(ctx context.Context) (*pgx.Conn, error) {
	return h.connect(ctx, MigrationRole, DatabaseName)
}

// RuntimeConn returns a connection to the suite database as the canonical
// runtime role, for the runtime identity assertion and for proving that
// migration 0004's CONNECT grant is live.
func (h *Harness) RuntimeConn(ctx context.Context) (*pgx.Conn, error) {
	return h.connect(ctx, RuntimeRole, DatabaseName)
}

// Converge is the harness's separate, optional convergence stage: it runs
// the production migration runner — the same function the migrate
// subcommand invokes, one runner, embedded source — as the canonical
// migration identity on the suite database.
func (h *Harness) Converge(ctx context.Context) (*migrate.Result, error) {
	conn, err := h.MigrationConn(ctx)
	if err != nil {
		return nil, err
	}
	defer conn.Close(ctx)
	return migrate.Run(ctx, conn)
}

// Teardown drops the suite database (5a step 8, suite end). It also drops
// the database a mid-case reprovisioning left behind; roles created by
// negative-identity variants are cluster-global fixture roles and are left
// in place (they own nothing once the suite database is gone, and
// Provision recreates the two canonical roles deterministically).
//
// Teardown also restores the canonical vector_api and vector_owner roles
// to their default attributes. This prevents a test that mutated canonical
// role attributes (e.g. BYPASSRLS on vector_api) from polluting subsequent
// test runs when the test fails before the next Provision call.
func (h *Harness) Teardown(ctx context.Context) error {
	if err := h.dropDatabase(ctx); err != nil {
		return err
	}
	if err := h.RestoreCanonicalRoles(ctx); err != nil {
		return err
	}
	return nil
}

// dropDatabase drops the suite database through the bootstrap maintenance
// connection.
func (h *Harness) dropDatabase(ctx context.Context) error {
	conn, err := h.BootstrapConn(ctx, h.cfg.Database)
	if err != nil {
		return err
	}
	defer conn.Close(ctx)
	if _, err := conn.Exec(ctx, "DROP DATABASE IF EXISTS "+qi(DatabaseName)+" WITH (FORCE)"); err != nil {
		return fmt.Errorf("testdb: drop suite database: %w", err)
	}
	return nil
}

// RestoreCanonicalRoles drops and recreates the canonical vector_api and
// vector_owner roles with their default attributes (LOGIN INHERIT, no
// superuser/BYPASSRLS/CREATEDB/CREATEROLE). This is idempotent and safe
// to call even if the roles were never created (DROP IF EXISTS). It is
// called by Teardown and can also be called directly by tests that need
// to ensure canonical role state after a failure.
func (h *Harness) RestoreCanonicalRoles(ctx context.Context) error {
	conn, err := h.BootstrapConn(ctx, h.cfg.Database)
	if err != nil {
		return fmt.Errorf("testdb: connect for role restoration: %w", err)
	}
	defer conn.Close(ctx)

	// Drop any stale canonical roles so recreation is deterministic.
	if _, err := conn.Exec(ctx, "DROP ROLE IF EXISTS "+qi(RuntimeRole)); err != nil {
		return fmt.Errorf("testdb: drop stale runtime role: %w", err)
	}
	if _, err := conn.Exec(ctx, "DROP ROLE IF EXISTS "+qi(MigrationRole)); err != nil {
		return fmt.Errorf("testdb: drop stale migration role: %w", err)
	}

	// Recreate with canonical attributes: LOGIN INHERIT, no superuser,
	// no BYPASSRLS, no CREATEDB, no CREATEROLE.
	if _, err := conn.Exec(ctx, "CREATE ROLE "+qi(RuntimeRole)+" LOGIN INHERIT"); err != nil {
		return fmt.Errorf("testdb: create runtime role: %w", err)
	}
	if _, err := conn.Exec(ctx, "CREATE ROLE "+qi(MigrationRole)+" LOGIN INHERIT"); err != nil {
		return fmt.Errorf("testdb: create migration role: %w", err)
	}
	return nil
}

// DropRoles drops the named cluster-global fixture roles through the
// bootstrap maintenance connection. These roles are created by negative-
// identity variants and persist beyond the suite database; this method
// cleans them up so subsequent test runs start clean. It connects to the
// maintenance database (not the suite database, which may already be gone).
//
// It uses a two-pass approach:
//
//  1. Revocation pass: Query pg_auth_members to discover every membership
//     edge where both the member and the granted role are in the supplied
//     name set, then revoke each edge. This completes all revocations
//     while every role still exists, avoiding the interleaving bug where
//     dropping G before processing M leaves M's membership of G unrevoked.
//     Only edges that actually exist are revoked; PostgreSQL REVOKE has no
//     IF EXISTS clause, so catalog checks are necessary.
//
// 2. Drop pass: Drop every named role with DROP ROLE IF EXISTS.
func (h *Harness) DropRoles(ctx context.Context, names ...string) error {
	conn, err := h.BootstrapConn(ctx, h.cfg.Database)
	if err != nil {
		return fmt.Errorf("testdb: connect for role cleanup: %w", err)
	}
	defer conn.Close(ctx)

	// --- Pass 1: Revoke all memberships within the fixture role set. ---
	// Query pg_auth_members to discover every edge where both the member
	// and the granted role are in our cleanup set. Only revoke edges that
	// actually exist; REVOKE has no IF EXISTS clause.
	rows, err := conn.Query(ctx,
		`SELECT r_member.rolname, r_granted.rolname
		   FROM pg_auth_members am
		   JOIN pg_roles r_member ON r_member.oid = am.member
		   JOIN pg_roles r_granted ON r_granted.oid = am.roleid
		  WHERE r_member.rolname = ANY($1::text[])
			AND r_granted.rolname = ANY($2::text[])`,
		names, names,
	)
	if err != nil {
		return fmt.Errorf("testdb: query fixture role memberships: %w", err)
	}

	type membership struct {
		member  string
		granted string
	}
	var memberships []membership

	for rows.Next() {
		var m membership
		if err := rows.Scan(&m.member, &m.granted); err != nil {
			rows.Close()
			return fmt.Errorf("testdb: scan membership row: %w", err)
		}
		memberships = append(memberships, m)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("testdb: membership query error: %w", err)
	}

	for _, m := range memberships {
		if _, err := conn.Exec(ctx,
			"REVOKE "+qi(m.granted)+" FROM "+qi(m.member)); err != nil {
			return fmt.Errorf("testdb: revoke %q from %q: %w", m.granted, m.member, err)
		}
	}

	// --- Pass 2: Drop all named roles. ---
	for _, name := range names {
		if _, err := conn.Exec(ctx, "DROP ROLE IF EXISTS "+qi(name)); err != nil {
			return fmt.Errorf("testdb: drop fixture role %q: %w", name, err)
		}
	}

	return nil
}

// AssertRolesAbsent queries pg_roles and fails if any of the named roles
// still exist in the cluster. It is intended for test cleanup verification
// so that fixture role leakage is caught immediately rather than surfacing
// as a mysterious failure in a later test.
func (h *Harness) AssertRolesAbsent(ctx context.Context, t testingT, names ...string) {
	conn, err := h.BootstrapConn(ctx, h.cfg.Database)
	if err != nil {
		t.Errorf("testdb: connect for role absence check: %v", err)
		return
	}
	defer conn.Close(ctx)

	for _, name := range names {
		var exists bool
		err := conn.QueryRow(ctx,
			"SELECT EXISTS(SELECT 1 FROM pg_roles WHERE rolname = $1)",
			name,
		).Scan(&exists)
		if err != nil {
			t.Errorf("testdb: check role %q absence: %v", name, err)
			continue
		}
		if exists {
			t.Errorf("fixture role %q still exists after cleanup", name)
		}
	}
}

// testingT is the minimal interface required by AssertRolesAbsent.
// It matches *testing.T and *testing.B.
type testingT interface {
	Errorf(format string, args ...interface{})
}

// HistoryRow is one row from the migration-history table.
type HistoryRow struct {
	Version   int
	Name      string
	Checksum  string
	AppliedAt time.Time
}

// ReadHistory reads the migration history from the suite database as the
// bootstrap identity. It returns an error if the history table does not
// exist (pre-convergence rejection fixtures may not have bootstrapped it).
func (h *Harness) ReadHistory(ctx context.Context) ([]HistoryRow, error) {
	conn, err := h.BootstrapConn(ctx, DatabaseName)
	if err != nil {
		return nil, err
	}
	defer conn.Close(ctx)

	rows, err := conn.Query(ctx,
		`SELECT version, name, checksum, applied_at FROM vector_control.schema_migrations ORDER BY version`)
	if err != nil {
		return nil, fmt.Errorf("testdb: read migration history: %w", err)
	}
	defer rows.Close()

	var out []HistoryRow
	for rows.Next() {
		var r HistoryRow
		if err := rows.Scan(&r.Version, &r.Name, &r.Checksum, &r.AppliedAt); err != nil {
			rows.Close()
			return nil, fmt.Errorf("testdb: scan migration history row: %w", err)
		}
		out = append(out, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("testdb: read migration history: %w", err)
	}
	return out, nil
}

// TransferTableOwnershipToRole creates a fixture role with the given name
// (no memberships, no privileges, no other ownership) and transfers the
// named migrated table to it. This is the parameterized form of
// TransferTableOwnershipToUnreachableRole, allowing the caller to scope
// the role name to avoid cross-test collisions.
func (h *Harness) TransferTableOwnershipToRole(ctx context.Context, table, role string) error {
	if err := h.CreateRole(ctx, role); err != nil {
		return err
	}
	return h.TransferTableOwnership(ctx, table, role)
}

// TransferFunctionOwnershipToRole creates a fixture role with the given
// name and transfers the named migrated function to it. This is the
// parameterized form of TransferFunctionOwnershipToUnreachableRole,
// allowing the caller to scope the role name to avoid cross-test collisions.
func (h *Harness) TransferFunctionOwnershipToRole(ctx context.Context, function, role string) error {
	if err := h.CreateRole(ctx, role); err != nil {
		return err
	}
	return h.TransferFunctionOwnership(ctx, function, role)
}

// ServerVersionNum returns the PostgreSQL server_version_num integer from
// the bootstrap maintenance connection. It is used by tests that need to
// conditionally skip based on server version (e.g., MAINTAIN requires
// PostgreSQL 18+).
func (h *Harness) ServerVersionNum(ctx context.Context) (int, error) {
	conn, err := h.BootstrapConn(ctx, h.cfg.Database)
	if err != nil {
		return 0, fmt.Errorf("testdb: connect for version check: %w", err)
	}
	defer conn.Close(ctx)

	var versionStr string
	if err := conn.QueryRow(ctx, "SHOW server_version_num").Scan(&versionStr); err != nil {
		return 0, fmt.Errorf("testdb: read server_version_num: %w", err)
	}
	versionNum, err := strconv.Atoi(versionStr)
	if err != nil {
		return 0, fmt.Errorf("testdb: parse server_version_num %q: %w", versionStr, err)
	}
	return versionNum, nil
}
