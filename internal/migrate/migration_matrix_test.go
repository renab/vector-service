//go:build integration

package migrate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"vector-service/internal/config"
	"vector-service/internal/db"
	"vector-service/internal/privilege"
	"vector-service/migrations"
)

// The canonical release gate matrix of docs/MIGRATIONS.md, run against a real
// PostgreSQL + pgvector database (5a environment, never mocked). Six cases,
// each on a fresh provisioned-but-un-converged disposable vector database.
//
// The bootstrap identity (a superuser, test-only, supplied through
// VECTOR_SERVICE_TEST_PG_DSN) provisions steps 1-5 exactly as internal/testdb
// does; the migration matrix package cannot import internal/testdb (it
// imports this package), so the provisioning is replicated here verbatim.
// Cases 2, 3(b), and 6 use the package-internal source seam only to
// construct prior states or failing migrations; every other run is the
// production migrate.Run with the embedded source.

const (
	matrixBootstrapDSNEnv = "VECTOR_SERVICE_TEST_PG_DSN"
	matrixMigrationRole   = "vector_owner"
	matrixRuntimeRole     = config.RuntimeRole
	matrixDatabase        = config.DatabaseName
)

// canonicalChecksums is the migrations/README.md SHA-256 table of the
// released set. Every case asserts against it, and it is self-checked
// against the embedded bytes in assertCanonicalChecksums.
var canonicalChecksums = map[string]string{
	"0001_shared_vector_schema.sql":                  "fd7c7dc08ac93a759621a4504023506c4f11e22009a33a6a76b7fa6544967fbc",
	"0002_qwen3_embedding_space.sql":                 "de65b429ca983e24990f95499ae96dced9c34cf2e02786b860aa065350c70060",
	"0003_qwen3_hnsq.sql":                            "730b80c77ab232ca31dacad704df579241d74c54a79a870e02f29dc48f640b51",
	"0004_runtime_identity.sql":                      "bc13c6618de1a30fb80bb81db2426052e4e39e2316a596c606eaf5aa9508a21f",
	"0005_runtime_privilege_boundary.sql":            "e7097c444a650855773ccad87f1677d4d37e9f6935014a9745d2eb0fb4b20490",
	"0006_runtime_privilege_boundary_completion.sql": "29bd9e54d5da131cf9eeefab674cef2c8c829b76bbe74264f7f9dff4c5b7dfb9",
}

// matrixSuite is one case's disposable environment: the bootstrap
// connection settings and the provisioned vector database it owns, plus the
// teardown it owes the cluster.
type matrixSuite struct {
	t   *testing.T
	cfg *pgx.ConnConfig
}

func newMatrixSuite(t *testing.T) *matrixSuite {
	t.Helper()
	dsn := os.Getenv(matrixBootstrapDSNEnv)
	if dsn == "" {
		t.Skipf("%s is not set: the migration matrix requires a real PostgreSQL with the vector extension", matrixBootstrapDSNEnv)
	}
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse %s: %v", matrixBootstrapDSNEnv, err)
	}
	return &matrixSuite{t: t, cfg: cfg}
}

func (s *matrixSuite) connect(user, database string) *pgx.Conn {
	s.t.Helper()
	ctx, cancel := context.WithTimeout(s.t.Context(), 30*time.Second)
	defer cancel()
	cfg := s.cfg.Copy()
	cfg.User = user
	cfg.Database = database
	// The privilege catalog and assertion layers parse query results as raw
	// text fields (the privilege.Querier contract); the simple query
	// protocol is text-only (see internal/testdb).
	cfg.DefaultQueryExecMode = pgx.QueryExecModeSimpleProtocol
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		s.t.Fatalf("connect as %q to %q: %v", user, database, err)
	}
	s.t.Cleanup(func() { conn.Close(context.Background()) })
	return conn
}

// provision builds the canonical provisioning shape, replicating 5a steps
// 1-5 verbatim (internal/testdb.Provision): a fresh vector database, the
// vector extension, the two canonical roles, and the migration identity as
// database owner. Idempotent: stale state from a previous suite is dropped
// first. The suite database is dropped when the test ends.
func (s *matrixSuite) provision() {
	s.t.Helper()
	ctx, cancel := context.WithTimeout(s.t.Context(), 2*time.Minute)
	defer cancel()

	boot := s.connect(s.cfg.User, s.cfg.Database)
	var (
		who   string
		super bool
	)
	if err := boot.QueryRow(ctx,
		"SELECT current_user, rolsuper FROM pg_roles WHERE rolname = current_user").Scan(&who, &super); err != nil {
		s.t.Fatalf("read bootstrap identity: %v", err)
	}
	if !super {
		s.t.Fatalf("the bootstrap identity %q is not a superuser; provisioning the vector extension requires one", who)
	}
	for _, stmt := range []string{
		`DROP DATABASE IF EXISTS "vector" WITH (FORCE)`,
		`DROP ROLE IF EXISTS "vector_api"`,
		`DROP ROLE IF EXISTS "vector_owner"`,
		`CREATE DATABASE "vector"`,
		`CREATE ROLE "vector_api" LOGIN INHERIT`,
		`CREATE ROLE "vector_owner" LOGIN INHERIT`,
	} {
		if _, err := boot.Exec(ctx, stmt); err != nil {
			s.t.Fatalf("provision: %s: %v", stmt, err)
		}
	}
	vec := s.connect(s.cfg.User, matrixDatabase)
	for _, stmt := range []string{
		`CREATE EXTENSION IF NOT EXISTS vector`,
		`ALTER DATABASE "vector" OWNER TO "vector_owner"`,
	} {
		if _, err := vec.Exec(ctx, stmt); err != nil {
			s.t.Fatalf("provision: %s: %v", stmt, err)
		}
	}
	// Teardown runs after the test context is done, so it uses a fresh
	// context and the stored bootstrap settings directly.
	s.t.Cleanup(func() {
		cctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		conn, err := pgx.ConnectConfig(cctx, s.cfg.Copy())
		if err != nil {
			s.t.Logf("teardown connect: %v", err)
			return
		}
		defer conn.Close(context.Background())
		if _, err := conn.Exec(cctx, `DROP DATABASE IF EXISTS "vector" WITH (FORCE)`); err != nil {
			s.t.Logf("teardown drop suite database: %v", err)
		}
	})
}

func (s *matrixSuite) migrationConn() *pgx.Conn {
	return s.connect(matrixMigrationRole, matrixDatabase)
}

func (s *matrixSuite) runtimeConn() *pgx.Conn { return s.connect(matrixRuntimeRole, matrixDatabase) }

// assertCanonicalChecksums self-checks the embedded migration set against
// the documented table: exactly six files, each byte string hashing to its
// documented SHA-256.
func assertCanonicalChecksums(t *testing.T) {
	t.Helper()
	files, err := migrations.Files()
	if err != nil {
		t.Fatalf("read embedded migrations: %v", err)
	}
	if len(files) != len(canonicalChecksums) {
		t.Fatalf("embedded migration count = %d, want %d", len(files), len(canonicalChecksums))
	}
	for _, f := range files {
		want, ok := canonicalChecksums[f.Name]
		if !ok {
			t.Fatalf("embedded migration %s is not in the documented release set", f.Name)
		}
		sum := sha256.Sum256(f.Raw)
		got := hex.EncodeToString(sum[:])
		if got != want {
			t.Fatalf("embedded migration %s checksum = %s, documented %s: the embedded bytes do not match the release table", f.Name, got, want)
		}
	}
}

// embeddedFile returns one embedded migration's raw bytes by name.
func embeddedFile(t *testing.T, name string) []byte {
	t.Helper()
	files, err := migrations.Files()
	if err != nil {
		t.Fatalf("read embedded migrations: %v", err)
	}
	for _, f := range files {
		if f.Name == name {
			return f.Raw
		}
	}
	t.Fatalf("embedded migration %s not found", name)
	return nil
}

type matrixHistoryRow struct {
	version   int
	name      string
	checksum  string
	appliedAt time.Time
}

func (s *matrixSuite) readHistory(conn *pgx.Conn) []matrixHistoryRow {
	s.t.Helper()
	ctx := s.t.Context()
	rows, err := conn.Query(ctx,
		`SELECT version, name, checksum, applied_at FROM vector_control.schema_migrations ORDER BY version`)
	if err != nil {
		s.t.Fatalf("read migration history: %v", err)
	}
	defer rows.Close()
	var out []matrixHistoryRow
	for rows.Next() {
		var h matrixHistoryRow
		if err := rows.Scan(&h.version, &h.name, &h.checksum, &h.appliedAt); err != nil {
			s.t.Fatalf("scan migration history row: %v", err)
		}
		out = append(out, h)
	}
	if err := rows.Err(); err != nil {
		s.t.Fatalf("read migration history: %v", err)
	}
	return out
}

func (h matrixHistoryRow) same(o matrixHistoryRow) bool {
	return h.version == o.version && h.name == o.name && h.checksum == o.checksum && h.appliedAt.Equal(o.appliedAt)
}

func assertHistory(t *testing.T, got []matrixHistoryRow, versions ...int) {
	t.Helper()
	if len(got) != len(versions) {
		t.Fatalf("history row count = %d, want %d: %v", len(got), len(versions), got)
	}
	for i, v := range versions {
		if got[i].version != v {
			t.Errorf("history[%d] version = %d, want %d", i, got[i].version, v)
		}
		wantName := fmt.Sprintf("%04d_", v)
		if len(got[i].name) < len(wantName) || got[i].name[:len(wantName)] != wantName {
			t.Errorf("history[%d] name %q does not start with %q", i, got[i].name, wantName)
		}
		wantSum, ok := canonicalChecksums[got[i].name]
		if !ok {
			t.Errorf("history[%d] name %q is not in the documented release set", i, got[i].name)
			continue
		}
		if got[i].checksum != wantSum {
			t.Errorf("history[%d] (%s) checksum = %s, documented %s", i, got[i].name, got[i].checksum, wantSum)
		}
	}
}

// sweep reads the complete in-scope ownership state on the connection.
func sweep(ctx context.Context, q privilege.Querier) (*privilege.OwnershipSweep, string, int32, func(int32) string, error) {
	identity, roleOID, err := privilege.CurrentRole(ctx, q)
	if err != nil {
		return nil, "", 0, nil, fmt.Errorf("read current role: %w", err)
	}
	roles, _, err := privilege.Roles(ctx, q)
	if err != nil {
		return nil, "", 0, nil, fmt.Errorf("read roles: %w", err)
	}
	names := privilege.RoleNames(roles)
	dbi, err := privilege.Database(ctx, q)
	if err != nil {
		return nil, "", 0, nil, fmt.Errorf("read database: %w", err)
	}
	schemas, err := privilege.Schemas(ctx, q, "vector_control", "vector_data")
	if err != nil {
		return nil, "", 0, nil, fmt.Errorf("read schemas: %w", err)
	}
	relations, err := privilege.Relations(ctx, q, "vector_control", "vector_data")
	if err != nil {
		return nil, "", 0, nil, fmt.Errorf("read relations: %w", err)
	}
	types, err := privilege.Types(ctx, q, "vector_control", "vector_data")
	if err != nil {
		return nil, "", 0, nil, fmt.Errorf("read types: %w", err)
	}
	functions, err := privilege.Functions(ctx, q, "vector_control", "vector_data")
	if err != nil {
		return nil, "", 0, nil, fmt.Errorf("read functions: %w", err)
	}
	sw := &privilege.OwnershipSweep{
		Database:  dbi,
		Schemas:   schemas,
		Relations: relations,
		Types:     types,
		Functions: functions,
	}
	return sw, identity, roleOID, names, nil
}

// assertSchemaInventory verifies the converged schema state of case 1:
// schemas, tables, RLS, policies, triggers, the seeded space row, and the
// HNSW index.
func assertSchemaInventory(t *testing.T, conn *pgx.Conn) {
	t.Helper()
	ctx := t.Context()
	q := func(sql string) (int, error) {
		var n int
		return n, conn.QueryRow(ctx, sql).Scan(&n)
	}
	mustBe := func(name, sql string, want int) {
		t.Helper()
		got, err := q(sql)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if got != want {
			t.Errorf("%s = %d, want %d", name, got, want)
		}
	}
	mustBe("schemas", `SELECT count(*) FROM pg_namespace WHERE nspname IN ('vector_control','vector_data')`, 2)
	mustBe("tables", `SELECT count(*) FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname IN ('vector_control','vector_data') AND c.relkind = 'r'
		  AND c.relname IN ('applications','namespaces','vector_spaces','application_credentials','schema_migrations','vector_records')`, 6)
	mustBe("migrated functions", `SELECT count(*) FROM pg_proc p JOIN pg_namespace n ON n.oid = p.pronamespace
		WHERE n.nspname IN ('vector_control','vector_data') AND p.proname IN ('set_updated_at','validate_vector_record')`, 2)
	mustBe("RLS enabled+forced on vector_records", `SELECT count(*) FROM pg_class WHERE relname = 'vector_records' AND relrowsecurity AND relforcerowsecurity`, 1)
	mustBe("RLS enabled+forced on namespaces", `SELECT count(*) FROM pg_class WHERE relname = 'namespaces' AND relrowsecurity AND relforcerowsecurity`, 1)
	mustBe("policies", `SELECT count(*) FROM pg_policy p JOIN pg_class c ON c.oid = p.polrelid JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname IN ('vector_control','vector_data') AND p.polname IN ('vector_records_application_isolation','namespaces_application_isolation')`, 2)
	mustBe("triggers", `SELECT count(*) FROM pg_trigger tr JOIN pg_class c ON c.oid = tr.tgrelid JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname IN ('vector_control','vector_data') AND NOT tr.tgisinternal
		  AND tr.tgname IN ('vector_records_validate','applications_set_updated_at','namespaces_set_updated_at','vector_records_set_updated_at')`, 4)
	mustBe("seeded vector space row", `SELECT count(*) FROM vector_control.vector_spaces WHERE id = '0199f31e-1000-7000-8000-000000000001'`, 1)
	mustBe("HNSW index", `SELECT count(*) FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = 'vector_data' AND c.relkind = 'i' AND c.relname = 'vector_records_qwen3_06b_1024_cosine_hnsw'`, 1)
	var (
		dimensions  int
		metric, key string
	)
	if err := conn.QueryRow(ctx,
		`SELECT dimensions, distance_metric, vector_space_key FROM vector_control.vector_spaces WHERE id = '0199f31e-1000-7000-8000-000000000001'`).
		Scan(&dimensions, &metric, &key); err != nil {
		t.Fatalf("read seeded vector space: %v", err)
	}
	if dimensions != 1024 || metric != "cosine" || key != "qwen3-embedding-0.6b-1024-cosine-v1" {
		t.Errorf("seeded vector space = (%d, %s, %s), want (1024, cosine, qwen3-embedding-0.6b-1024-cosine-v1)", dimensions, metric, key)
	}
}

// assertRuntimeBoundary runs the production runtime identity assertion on a
// live vector_api connection: identity, no ownership, the exact canonical
// grant boundary (effective grid, column level, array view, provenance,
// grant options, default ACLs), and the membership-closure rule.
func assertRuntimeBoundary(t *testing.T, conn *pgx.Conn) {
	t.Helper()
	if err := db.RuntimeBoundary(t.Context(), privilege.PgxQuerier{Conn: conn}, matrixRuntimeRole, matrixMigrationRole, matrixDatabase); err != nil {
		t.Fatalf("runtime identity assertion: %v", err)
	}
}

// TestMigrationMatrix is the docs/MIGRATIONS.md six-case release gate.
func TestMigrationMatrix(t *testing.T) {
	assertCanonicalChecksums(t)

	t.Run("1_empty_to_latest", func(t *testing.T) {
		s := newMatrixSuite(t)
		s.provision()
		conn := s.migrationConn()
		res, err := Run(t.Context(), conn)
		if err != nil {
			t.Fatalf("migrate on empty database: %v", err)
		}
		if len(res.Applied) != 6 {
			t.Fatalf("applied count = %d, want 6: %v", len(res.Applied), res.Applied)
		}
		for i, a := range res.Applied {
			if a.Version != i+1 {
				t.Errorf("applied[%d] version = %d, want %d", i, a.Version, i+1)
			}
			if want, ok := canonicalChecksums[a.Name]; !ok {
				t.Errorf("applied[%d] name %q is not in the documented release set", i, a.Name)
			} else if a.Checksum != want {
				t.Errorf("applied[%d] (%s) checksum = %s, documented %s", i, a.Name, a.Checksum, want)
			}
		}
		assertHistory(t, s.readHistory(conn), 1, 2, 3, 4, 5, 6)
		assertSchemaInventory(t, conn)

		// Canonical owner assertion: the migration identity owns the
		// database, both schemas, and every in-scope pg_class, pg_type, and
		// pg_proc row; vector_api owns none of them.
		own := s.migrationConn()
		ctx := t.Context()
		sw, identity, oid, names, err := sweep(ctx, privilege.PgxQuerier{Conn: own})
		if err != nil {
			t.Fatalf("ownership sweep as migration identity: %v", err)
		}
		if identity != matrixMigrationRole {
			t.Fatalf("sweep identity = %q, want %q", identity, matrixMigrationRole)
		}
		if mismatches := privilege.CheckOwnership(*sw, oid, names, true); len(mismatches) != 0 {
			t.Errorf("the migration identity does not own every in-scope object: %v", mismatches)
		}
		api := s.runtimeConn() // a live vector_api connection proves 0004's CONNECT grant executed
		sw2, identity2, apiOID, names2, err := sweep(ctx, privilege.PgxQuerier{Conn: api})
		if err != nil {
			t.Fatalf("ownership sweep as runtime role: %v", err)
		}
		if identity2 != matrixRuntimeRole {
			t.Fatalf("sweep identity = %q, want %q", identity2, matrixRuntimeRole)
		}
		if mismatches := privilege.CheckOwnership(*sw2, apiOID, names2, false); len(mismatches) != 0 {
			t.Errorf("the runtime role owns an in-scope object: %v", mismatches)
		}
		assertRuntimeBoundary(t, api)
	})

	t.Run("2_previous_released_schema_to_latest", func(t *testing.T) {
		s := newMatrixSuite(t)
		s.provision()
		conn := s.migrationConn()

		// Prior state: the immutable canonical 0001-0004, byte-identical,
		// through the package-internal seam.
		prior := map[string][]byte{}
		for _, n := range []string{"0001_shared_vector_schema.sql", "0002_qwen3_embedding_space.sql", "0003_qwen3_hnsq.sql", "0004_runtime_identity.sql"} {
			prior[n] = embeddedFile(t, n)
		}
		res, err := New(withSource(newSyntheticSource(prior))).Run(t.Context(), conn)
		if err != nil {
			t.Fatalf("build prior state 0001-0004: %v", err)
		}
		if len(res.Applied) != 4 {
			t.Fatalf("prior-state applied count = %d, want 4: %v", len(res.Applied), res.Applied)
		}
		before := s.readHistory(conn)
		assertHistory(t, before, 1, 2, 3, 4)

		// The production runner over the same connection applies exactly
		// 0005 and 0006, in order, and nothing else.
		res, err = Run(t.Context(), conn)
		if err != nil {
			t.Fatalf("migrate prior state to latest: %v", err)
		}
		if len(res.Applied) != 2 {
			t.Fatalf("applied count = %d, want exactly 2 (0005, 0006): %v", len(res.Applied), res.Applied)
		}
		wantNames := []string{"0005_runtime_privilege_boundary.sql", "0006_runtime_privilege_boundary_completion.sql"}
		for i, a := range res.Applied {
			if a.Name != wantNames[i] {
				t.Errorf("applied[%d] = %s, want %s (0005 must precede 0006)", i, a.Name, wantNames[i])
			}
			if a.Checksum != canonicalChecksums[a.Name] {
				t.Errorf("applied %s checksum = %s, documented %s", a.Name, a.Checksum, canonicalChecksums[a.Name])
			}
		}
		if res.AlreadyApplied != 4 {
			t.Errorf("already applied = %d, want 4", res.AlreadyApplied)
		}
		after := s.readHistory(conn)
		assertHistory(t, after, 1, 2, 3, 4, 5, 6)
		for i := 0; i < 4; i++ {
			if !before[i].same(after[i]) {
				t.Errorf("history row for version %d changed on re-run: %v -> %v", before[i].version, before[i], after[i])
			}
		}
		// 0005/0006 establish the canonical boundary end to end, including
		// the implicit row types of the migrated tables.
		assertRuntimeBoundary(t, s.runtimeConn())
	})

	t.Run("3_history_checksum_validation", func(t *testing.T) {
		t.Run("a_stored_checksum_tampered", func(t *testing.T) {
			s := newMatrixSuite(t)
			s.provision()
			conn := s.migrationConn()
			if _, err := Run(t.Context(), conn); err != nil {
				t.Fatalf("converge: %v", err)
			}
			if _, err := conn.Exec(t.Context(),
				`UPDATE vector_control.schema_migrations SET checksum = '0000000000000000000000000000000000000000000000000000000000000000' WHERE version = 3`); err != nil {
				t.Fatalf("tamper stored checksum: %v", err)
			}
			res, err := Run(t.Context(), conn)
			if err == nil {
				t.Fatalf("expected checksum drift, run succeeded: %+v", res)
			}
			drift, ok := err.(*checksumDrift)
			if !ok {
				t.Fatalf("drift error = %T (%v), want *checksumDrift", err, err)
			}
			if drift.version != 3 || drift.name != "0003_qwen3_hnsq.sql" {
				t.Errorf("drift names version %d (%s), want 3 (0003_qwen3_hnsq.sql)", drift.version, drift.name)
			}
			if drift.stored != "0000000000000000000000000000000000000000000000000000000000000000" || drift.embedded != canonicalChecksums["0003_qwen3_hnsq.sql"] {
				t.Errorf("drift stored/embedded = (%s, %s), want (tampered, documented 0003)", drift.stored, drift.embedded)
			}
			// Nothing was applied or repaired by the failing pass.
			h := s.readHistory(conn)
			if len(h) != 6 || h[2].checksum != "0000000000000000000000000000000000000000000000000000000000000000" {
				t.Errorf("failing pass altered history: %v", h)
			}
		})
		t.Run("b_source_bytes_differ", func(t *testing.T) {
			s := newMatrixSuite(t)
			s.provision()
			conn := s.migrationConn()

			prior := map[string][]byte{}
			for _, n := range []string{"0001_shared_vector_schema.sql", "0002_qwen3_embedding_space.sql", "0003_qwen3_hnsq.sql", "0004_runtime_identity.sql"} {
				prior[n] = embeddedFile(t, n)
			}
			if _, err := New(withSource(newSyntheticSource(prior))).Run(t.Context(), conn); err != nil {
				t.Fatalf("build prior state 0001-0004: %v", err)
			}
			// The same set again, but 0003's bytes differ: the recorded
			// checksum no longer matches the source.
			tampered := map[string][]byte{
				"0001_shared_vector_schema.sql":  embeddedFile(t, "0001_shared_vector_schema.sql"),
				"0002_qwen3_embedding_space.sql": embeddedFile(t, "0002_qwen3_embedding_space.sql"),
				// A comment after the COMMIT keeps the framing valid while
				// changing the bytes — the drift is the checksum's, not a
				// framing repair.
				"0003_qwen3_hnsq.sql":       append(append([]byte{}, embeddedFile(t, "0003_qwen3_hnsq.sql")...), []byte("\n-- tampered after release\n")...),
				"0004_runtime_identity.sql": embeddedFile(t, "0004_runtime_identity.sql"),
			}
			res, err := New(withSource(newSyntheticSource(tampered))).Run(t.Context(), conn)
			if err == nil {
				t.Fatalf("expected checksum drift, run succeeded: %+v", res)
			}
			drift, ok := err.(*checksumDrift)
			if !ok {
				t.Fatalf("drift error = %T (%v), want *checksumDrift", err, err)
			}
			if drift.version != 3 {
				t.Errorf("drift version = %d, want 3", drift.version)
			}
		})
	})

	t.Run("4_repeated_startup_no_pending", func(t *testing.T) {
		s := newMatrixSuite(t)
		s.provision()
		conn := s.migrationConn()
		if _, err := Run(t.Context(), conn); err != nil {
			t.Fatalf("first migrate: %v", err)
		}
		first := s.readHistory(conn)
		for pass := 2; pass <= 3; pass++ {
			res, err := Run(t.Context(), conn)
			if err != nil {
				t.Fatalf("pass %d: %v", pass, err)
			}
			if len(res.Applied) != 0 {
				t.Errorf("pass %d applied %d migrations, want 0: %v", pass, len(res.Applied), res.Applied)
			}
			if res.AlreadyApplied != 6 {
				t.Errorf("pass %d already applied = %d, want 6", pass, res.AlreadyApplied)
			}
			again := s.readHistory(conn)
			for i := range first {
				if !first[i].same(again[i]) {
					t.Errorf("pass %d changed history row %d: %v -> %v", pass, first[i].version, first[i], again[i])
				}
			}
		}
	})

	t.Run("5_concurrent_migration_attempts", func(t *testing.T) {
		s := newMatrixSuite(t)
		s.provision()

		type outcome struct {
			applied []int
			err     error
		}
		run := func() outcome {
			conn := s.migrationConn()
			ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
			defer cancel()
			res, err := Run(ctx, conn)
			if err != nil {
				return outcome{err: err}
			}
			var v []int
			for _, a := range res.Applied {
				v = append(v, a.Version)
			}
			return outcome{applied: v}
		}
		var (
			wg    sync.WaitGroup
			got   [2]outcome
			start = make(chan struct{})
		)
		for i := 0; i < 2; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				<-start // both goroutines race for the advisory lock together
				got[i] = run()
			}(i)
		}
		close(start)
		wg.Wait()
		seen := map[int]int{}
		var total int
		for i, o := range got {
			if o.err != nil {
				t.Fatalf("concurrent run %d failed: %v", i, o.err)
			}
			total += len(o.applied)
			for _, v := range o.applied {
				seen[v]++
			}
		}
		if total != 6 {
			t.Errorf("total applied across both runs = %d, want 6: %v", total, got)
		}
		for v := 1; v <= 6; v++ {
			if seen[v] != 1 {
				t.Errorf("version %d applied %d times across both runs, want exactly 1: %v", v, seen[v], got)
			}
		}
		h := s.readHistory(s.migrationConn())
		assertHistory(t, h, 1, 2, 3, 4, 5, 6)
	})

	t.Run("6_failure_behavior", func(t *testing.T) {
		t.Run("a_failing_statement_rolls_back", func(t *testing.T) {
			s := newMatrixSuite(t)
			s.provision()
			conn := s.migrationConn()

			ok := []byte("BEGIN;\nCREATE TABLE mm_a_first (id int);\nCOMMIT;\n")
			bad := []byte("BEGIN;\nCREATE TABLE mm_a_fail (id int);\nSELECT * FROM mm_a_missing;\nCOMMIT;\n")
			src := newSyntheticSource(map[string][]byte{
				"0001_first.sql":   ok,
				"0002_failing.sql": bad,
			})
			res, err := New(withSource(src)).Run(t.Context(), conn)
			if err == nil {
				t.Fatalf("expected the failing migration to fail, run succeeded: %+v", res)
			}
			var appliedNames []string
			if res != nil {
				for _, a := range res.Applied {
					appliedNames = append(appliedNames, a.Name)
				}
			}
			if len(appliedNames) != 1 || appliedNames[0] != "0001_first.sql" {
				t.Fatalf("applied before failure = %v, want [0001_first.sql]", appliedNames)
			}
			h := s.readHistory(conn)
			if len(h) != 1 || h[0].version != 1 {
				t.Fatalf("history after failure = %v, want exactly the 0001 row (the failed migration has NO history row)", h)
			}
			ctx := t.Context()
			var n int
			if err := conn.QueryRow(ctx, `SELECT count(*) FROM pg_class WHERE relname = 'mm_a_first'`).Scan(&n); err != nil || n != 1 {
				t.Errorf("committed 0001 object missing: n=%d err=%v", n, err)
			}
			if err := conn.QueryRow(ctx, `SELECT count(*) FROM pg_class WHERE relname = 'mm_a_fail'`).Scan(&n); err != nil || n != 0 {
				t.Errorf("failed migration's DDL was not rolled back: n=%d err=%v", n, err)
			}
			// Corrected re-run: the same 0001, the corrected 0002.
			fixed := []byte("BEGIN;\nCREATE TABLE mm_a_fail (id int);\nCOMMIT;\n")
			res, err = New(withSource(newSyntheticSource(map[string][]byte{
				"0001_first.sql":   ok,
				"0002_failing.sql": fixed,
			}))).Run(t.Context(), conn)
			if err != nil {
				t.Fatalf("corrected re-run: %v", err)
			}
			if len(res.Applied) != 1 || res.Applied[0].Version != 2 {
				t.Errorf("corrected re-run applied = %v, want exactly version 2", res.Applied)
			}
			if res.AlreadyApplied != 1 {
				t.Errorf("corrected re-run already applied = %d, want 1", res.AlreadyApplied)
			}
			if err := conn.QueryRow(ctx, `SELECT count(*) FROM pg_class WHERE relname = 'mm_a_fail'`).Scan(&n); err != nil || n != 1 {
				t.Errorf("corrected migration object missing: n=%d err=%v", n, err)
			}
		})

		t.Run("b_history_insert_boundary", func(t *testing.T) {
			s := newMatrixSuite(t)
			s.provision()
			conn := s.migrationConn()
			ctx := t.Context()

			// First, a no-op migration that lets the runner bootstrap the
			// history table.
			seed := []byte("BEGIN;\nSELECT 1;\nCOMMIT;\n")
			if _, err := New(withSource(newSyntheticSource(map[string][]byte{
				"0001_seed.sql": seed,
			}))).Run(ctx, conn); err != nil {
				t.Fatalf("bootstrap the history table: %v", err)
			}
			// The test-only failure fixture: a BEFORE INSERT trigger on the
			// history table (in a separate fixture schema, outside the
			// gate's sweep scope) rejects the runner's own history write.
			boot := s.connect(s.cfg.User, matrixDatabase)
			for _, stmt := range []string{
				`CREATE SCHEMA mm_fixture`,
				`CREATE FUNCTION mm_fixture.reject_history() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'fixture: history insert rejected';
END;
$$`,
				`CREATE TRIGGER mm_history_guard
BEFORE INSERT ON vector_control.schema_migrations
FOR EACH ROW EXECUTE FUNCTION mm_fixture.reject_history()`,
			} {
				if _, err := boot.Exec(ctx, stmt); err != nil {
					t.Fatalf("failure fixture: %v", err)
				}
			}
			probe := []byte("BEGIN;\nCREATE SCHEMA mm_probe;\nCREATE TABLE mm_probe.probe (id int);\nCOMMIT;\n")
			res, err := New(withSource(newSyntheticSource(map[string][]byte{
				"0001_seed.sql":  seed,
				"0002_probe.sql": probe,
			}))).Run(ctx, conn)
			if err == nil {
				t.Fatalf("expected the rejected history INSERT to fail the run, succeeded: %+v", res)
			}
			if !strings.Contains(err.Error(), "history insert rejected") && !strings.Contains(err.Error(), "insert migration history row") {
				t.Errorf("failure does not name the history write: %v", err)
			}
			var n int
			if err := conn.QueryRow(ctx, `SELECT count(*) FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace WHERE n.nspname = 'mm_probe' AND c.relname = 'probe'`).Scan(&n); err != nil || n != 0 {
				t.Errorf("the migration's DDL was not rolled back with the history INSERT: n=%d err=%v", n, err)
			}
			h := s.readHistory(conn)
			if len(h) != 1 || h[0].version != 1 {
				t.Fatalf("history after rejected INSERT = %v, want exactly the 0001 row", h)
			}
			// Remove the fixture and re-run the unchanged set.
			for _, stmt := range []string{
				`DROP TRIGGER mm_history_guard ON vector_control.schema_migrations`,
				`DROP FUNCTION mm_fixture.reject_history()`,
				`DROP SCHEMA mm_fixture`,
			} {
				if _, err := boot.Exec(ctx, stmt); err != nil {
					t.Fatalf("remove failure fixture: %v", err)
				}
			}
			res, err = New(withSource(newSyntheticSource(map[string][]byte{
				"0001_seed.sql":  seed,
				"0002_probe.sql": probe,
			}))).Run(ctx, conn)
			if err != nil {
				t.Fatalf("converged re-run: %v", err)
			}
			if len(res.Applied) != 1 || res.Applied[0].Version != 2 {
				t.Errorf("converged re-run applied = %v, want exactly version 2", res.Applied)
			}
			if err := conn.QueryRow(ctx, `SELECT count(*) FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace WHERE n.nspname = 'mm_probe' AND c.relname = 'probe'`).Scan(&n); err != nil || n != 1 {
				t.Errorf("converged re-run object missing: n=%d err=%v", n, err)
			}
			h = s.readHistory(conn)
			if len(h) != 2 || h[1].version != 2 {
				t.Errorf("converged re-run history = %v, want both rows", h)
			}
		})
	})
}
