// Package testdb provisions and tears down the disposable Vector Service
// test environment that integration test suites run against, per
// implementation package 5 (scope 5a).
//
// It lives under internal/ so production code can never import it. The
// harness provisions only what infrastructure provisions in production: a
// database named vector (the name is hardcoded by configuration validation
// and by migration 0004's CONNECT grant, so the test database must match),
// the vector extension (pgvector, pre-provisioned — no migration creates
// it), the runtime role vector_api (LOGIN, no superuser/BYPASSRLS), and the
// canonical two-identity provisioning shape in which the migration identity
// vector_owner owns the vector database. It also builds the test-only
// negative-identity variants (rejection states only) that the package-1
// identity matrices consume; a variant is never a viable provisioning shape
// and is never converged.
//
// The test bootstrap identity (a superuser, used for provisioning and
// direct-DB fixture assertions only — no service code path ever runs as
// it) is supplied by the operator through the environment: EnvBootstrapDSN
// carries its connection string to the maintenance database. No service
// credential, key material, or generated token is ever logged or embedded
// in an error.
//
// Provisioning and convergence are separate, independently exposed stages:
// the migration matrix starts from a provisioned, un-converged database and
// drives the production runner itself, while behavior tests converge first
// and start from a converged database.
package testdb

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/jackc/pgx/v5"

	"vector-service/internal/config"
)

// Canonical suite names. The role names are fixed per fresh suite: the
// bootstrap identity (operator-supplied), the runtime role that 0004's
// grants bind by name, and the migration identity that owns the disposable
// database (canonical shape only — alternate provisioning shapes are
// deliberately out of scope).
const (
	// EnvBootstrapDSN is the environment variable carrying the test
	// bootstrap identity's connection string (a superuser connection to
	// the maintenance database, e.g. postgres). The value is never logged.
	EnvBootstrapDSN = "VECTOR_SERVICE_TEST_PG_DSN"

	// DatabaseName is the disposable test database's name. It is fixed by
	// configuration validation and by migration 0004's
	// GRANT CONNECT ON DATABASE vector, not by the harness.
	DatabaseName = config.DatabaseName

	// RuntimeRole is the canonical runtime role that 0004's grants require
	// to exist.
	RuntimeRole = config.RuntimeRole

	// MigrationRole is the canonical migration identity the harness
	// provisions: LOGIN, no superuser/BYPASSRLS, owner of the vector
	// database.
	MigrationRole = "vector_owner"
)

// Harness is one test suite's disposable environment: the bootstrap
// connection settings and the provisioned vector database.
type Harness struct {
	cfg *pgx.ConnConfig // parsed bootstrap connection settings
}

// New reads the bootstrap connection settings from the environment and
// builds a Harness. No database activity occurs until Provision.
func New(ctx context.Context) (*Harness, error) {
	dsn, ok := os.LookupEnv(EnvBootstrapDSN)
	if !ok || dsn == "" {
		return nil, fmt.Errorf(
			"testdb: %s is not set; it must carry the test bootstrap identity's connection string (a superuser connection to the maintenance database)",
			EnvBootstrapDSN,
		)
	}
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		// The DSN itself is not echoed: it is connection material, and
		// driver errors may quote it.
		return nil, fmt.Errorf("testdb: the %s connection string is not a valid PostgreSQL connection", EnvBootstrapDSN)
	}
	return &Harness{cfg: cfg}, nil
}

// Provision builds the canonical provisioning shape in the bootstrap
// identity's cluster, per the 5a harness steps 1–5, idempotently: it drops
// any stale suite database and stale canonical roles (so the suite is
// re-runnable), creates the vector database, creates vector_api and
// vector_owner, creates the vector extension in the database, and makes
// vector_owner the database's owner. Every statement runs as the bootstrap
// identity, which must be a superuser (CREATE EXTENSION vector requires
// one; the check is fail-closed).
func (h *Harness) Provision(ctx context.Context) error {
	conn, err := h.connect(ctx, h.cfg.User, h.cfg.Database)
	if err != nil {
		return fmt.Errorf("testdb: connect as bootstrap identity: %w", err)
	}
	defer conn.Close(ctx)

	var (
		who   string
		super bool
	)
	if err := conn.QueryRow(ctx,
		"SELECT current_user, rolsuper FROM pg_roles WHERE rolname = current_user").Scan(&who, &super); err != nil {
		return fmt.Errorf("testdb: read bootstrap identity: %w", err)
	}
	if !super {
		return fmt.Errorf(
			"testdb: the bootstrap identity %q is not a superuser; provisioning the vector extension requires one",
			who,
		)
	}

	if _, err := conn.Exec(ctx, "DROP DATABASE IF EXISTS "+qi(DatabaseName)+" WITH (FORCE)"); err != nil {
		return fmt.Errorf("testdb: drop stale suite database: %w", err)
	}
	// Drop and recreate the canonical roles: a previous suite may have
	// mutated their attributes, and provisioning must be deterministic.
	if _, err := conn.Exec(ctx, "DROP ROLE IF EXISTS "+qi(RuntimeRole)); err != nil {
		return fmt.Errorf("testdb: drop stale runtime role: %w", err)
	}
	if _, err := conn.Exec(ctx, "DROP ROLE IF EXISTS "+qi(MigrationRole)); err != nil {
		return fmt.Errorf("testdb: drop stale migration role: %w", err)
	}
	if _, err := conn.Exec(ctx, "CREATE DATABASE "+qi(DatabaseName)); err != nil {
		return fmt.Errorf("testdb: create suite database: %w", err)
	}
	// LOGIN for both roles; INHERIT is part of the canonical vector_api
	// shape (the runtime identity assertion requires rolinherit true).
	// Neither role carries superuser, BYPASSRLS, CREATEDB, or CREATEROLE.
	if _, err := conn.Exec(ctx, "CREATE ROLE "+qi(RuntimeRole)+" LOGIN INHERIT"); err != nil {
		return fmt.Errorf("testdb: create runtime role: %w", err)
	}
	if _, err := conn.Exec(ctx, "CREATE ROLE "+qi(MigrationRole)+" LOGIN INHERIT"); err != nil {
		return fmt.Errorf("testdb: create migration role: %w", err)
	}

	vec, err := h.connect(ctx, h.cfg.User, DatabaseName)
	if err != nil {
		return fmt.Errorf("testdb: connect to suite database: %w", err)
	}
	defer vec.Close(ctx)

	// No migration creates the extension; infrastructure pre-provisions
	// it (package 1 prerequisite validation).
	if _, err := vec.Exec(ctx, "CREATE EXTENSION IF NOT EXISTS vector"); err != nil {
		return fmt.Errorf("testdb: create vector extension: %w", err)
	}
	// The canonical provisioning shape: the migration identity owns the
	// vector database (package 1, Scope 4).
	if _, err := vec.Exec(ctx, "ALTER DATABASE "+qi(DatabaseName)+" OWNER TO "+qi(MigrationRole)); err != nil {
		return fmt.Errorf("testdb: transfer database ownership to the migration identity: %w", err)
	}
	return nil
}

// qi renders a catalog name as a double-quoted SQL identifier, escaping
// embedded quotes. All harness names are canonical constants or fixture
// names; quoting keeps the DDL safe regardless.
func qi(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}
