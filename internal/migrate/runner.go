package migrate

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"vector-service/internal/config"
	"vector-service/internal/privilege"
)

// advisoryLockKey is the canonical single-flight key for migration runs.
// It is hashed with PostgreSQL's hashtext and held as a session advisory
// lock; PostgreSQL drops it automatically if the connection disconnects,
// which is the backstop for every exit path.
const advisoryLockKey = "vector.service.migration"

const (
	// defaultLockWait bounds how long a replica waits for the migration
	// advisory lock. Exceeding it is fatal.
	defaultLockWait = 300 * time.Second
	// lockPollInterval is the interval between pg_try_advisory_lock
	// attempts while waiting for the migration advisory lock.
	lockPollInterval = 250 * time.Millisecond
)

// bootstrapSQL establishes the canonical migration-history shape
// idempotently. It runs under the advisory lock as the migration identity.
const bootstrapSQL = `
CREATE SCHEMA IF NOT EXISTS vector_control;
CREATE TABLE IF NOT EXISTS vector_control.schema_migrations (
    version     INTEGER PRIMARY KEY,
    name        TEXT NOT NULL,
    checksum    TEXT NOT NULL,
    applied_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);`

const historySelectSQL = `
SELECT version, name, checksum
FROM vector_control.schema_migrations
ORDER BY version`

const historyInsertSQL = `
INSERT INTO vector_control.schema_migrations (version, name, checksum)
VALUES ($1, $2, $3)`

// gateSchemas are the two migrated schemas the canonical ownership gate
// sweeps. Every in-scope object lives in one of them.
var gateSchemas = []string{"vector_control", "vector_data"}

// Applied summarizes one migration applied by a run.
type Applied struct {
	Version  int
	Name     string
	Checksum string
	Duration time.Duration
}

// Result summarizes a migration run.
type Result struct {
	// Applied lists the migrations applied during this run, in order.
	Applied []Applied
	// AlreadyApplied counts already-applied migrations whose stored
	// checksums were re-validated and matched.
	AlreadyApplied int
}

// Runner applies the embedded canonical migration set to a connected
// database. One runner serves both subcommands; there are no independent
// migration runners.
type Runner struct {
	source   migrationSource
	lockWait time.Duration
	logger   *slog.Logger
}

// Option configures a Runner.
type Option func(*Runner)

// WithLockWait sets the bounded wait for the migration advisory lock.
func WithLockWait(d time.Duration) Option {
	return func(r *Runner) { r.lockWait = d }
}

// WithLogger sets the structured logger used by the runner.
func WithLogger(l *slog.Logger) Option {
	return func(r *Runner) { r.logger = l }
}

// withSource substitutes the migration source. It is the package-internal
// test seam: production entry points always use the embedded source, and no
// exported override exists.
func withSource(s migrationSource) Option {
	return func(r *Runner) { r.source = s }
}

// New builds a Runner over the embedded migration set.
func New(opts ...Option) *Runner {
	r := &Runner{
		source:   embeddedSource{},
		lockWait: defaultLockWait,
		logger:   slog.Default(),
	}
	for _, opt := range opts {
		opt(r)
	}
	return r
}

// Run is the production entry point for both subcommands: it converges the
// connected database to the embedded migration set. conn must already be
// connected as the migration identity (identity assertion, scope 3, is
// outside this package's core); it is the only connection the run touches.
//
// Sequence: load and validate the migration set (framing and naming are
// fatal before anything is applied) → validate the pre-provisioned
// prerequisites (the connected database is the canonical database, the
// vector extension exists, the runtime role exists — no migration creates
// any of them) → acquire the advisory lock (bounded wait) → run the
// canonical ownership gate (the migration identity must own the database,
// both migrated schemas, and every in-scope pg_class, pg_type, and pg_proc
// row that exists; pre-existing state is accepted only after this passes,
// and the gate runs on every pass) → bootstrap the history schema → read
// history → validate stored checksums → apply each pending migration in one
// runner-owned transaction (framing-stripped body plus the history INSERT,
// committed atomically) → release the lock. Any failure exits non-zero with
// a structured error; the lock is released on every exit path.
func Run(ctx context.Context, conn *pgx.Conn, opts ...Option) (*Result, error) {
	return New(opts...).Run(ctx, conn)
}

// Run executes the migration step on the given connection.
func (r *Runner) Run(ctx context.Context, conn *pgx.Conn) (*Result, error) {
	migs, err := r.source.migrations()
	if err != nil {
		return nil, fmt.Errorf("load migration set: %w", err)
	}
	if len(migs) == 0 {
		return nil, fmt.Errorf("load migration set: no migrations found")
	}

	if err := r.validatePrerequisites(ctx, conn); err != nil {
		return nil, err
	}

	if err := r.acquireLock(ctx, conn); err != nil {
		return nil, err
	}
	// Best-effort release on every exit path; the database also drops the
	// lock if the connection disconnects. A cancelled caller context must
	// not suppress the release attempt.
	defer r.releaseLock(context.Background(), conn)

	if err := r.canonicalOwnershipGate(ctx, conn); err != nil {
		return nil, err
	}

	if err := r.bootstrap(ctx, conn); err != nil {
		return nil, err
	}

	applied, err := r.readHistory(ctx, conn)
	if err != nil {
		return nil, err
	}

	// Fail closed on history rows that no embedded migration accounts for:
	// the database was migrated by a different migration set.
	known := make(map[int]bool, len(migs))
	for _, m := range migs {
		known[m.version] = true
	}
	for version, row := range applied {
		if !known[version] {
			return nil, &unknownApplied{version: version, name: row.name}
		}
	}

	result := &Result{}
	for _, m := range migs {
		stored, ok := applied[m.version]
		if ok {
			if stored.checksum != m.checksum {
				return nil, &checksumDrift{
					version:  m.version,
					name:     m.name,
					stored:   stored.checksum,
					embedded: m.checksum,
				}
			}
			result.AlreadyApplied++
			continue
		}
		a, err := r.applyOne(ctx, conn, m)
		if err != nil {
			// The partial result reports the migrations committed before
			// the failure; the error carries the failure itself.
			return result, err
		}
		result.Applied = append(result.Applied, a)
	}

	r.logger.Info("migrations converged",
		"operation", "migrate",
		"applied", len(result.Applied),
		"already_applied", result.AlreadyApplied,
	)
	return result, nil
}

// acquireLock takes the migration advisory lock with a bounded wait,
// polling pg_try_advisory_lock. Exceeding the wait is fatal.
func (r *Runner) acquireLock(ctx context.Context, conn *pgx.Conn) error {
	deadline := time.Now().Add(r.lockWait)
	for {
		var acquired bool
		err := conn.QueryRow(ctx,
			"SELECT pg_try_advisory_lock(hashtext($1))", advisoryLockKey).Scan(&acquired)
		if err != nil {
			return fmt.Errorf("acquire migration advisory lock: %w", err)
		}
		if acquired {
			r.logger.Info("migration advisory lock acquired",
				"operation", "migrate",
				"key", advisoryLockKey,
			)
			return nil
		}

		remaining := time.Until(deadline)
		if remaining <= 0 {
			return &lockTimeout{wait: r.lockWait}
		}
		delay := remaining
		if delay > lockPollInterval {
			delay = lockPollInterval
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("acquire migration advisory lock: %w", ctx.Err())
		case <-time.After(delay):
		}
	}
}

// releaseLock drops the migration advisory lock, best-effort.
//
// A lock-release failure is warn-logged with only server-authored, safe
// fields. The raw error text (Error(), PgError Message/Detail/Hint/Where,
// or any connection string) is never logged. The failure is not propagated
// because the lock is advisory: PostgreSQL drops it automatically when the
// connection disconnects, which is the backstop for every exit path.
func (r *Runner) releaseLock(ctx context.Context, conn *pgx.Conn) {
	var released bool
	if err := conn.QueryRow(ctx,
		"SELECT pg_advisory_unlock(hashtext($1))", advisoryLockKey).Scan(&released); err != nil {
		r.logger.Warn("release migration advisory lock",
			"operation", "migrate",
			"key", advisoryLockKey,
			"cause", sanitizeError(err),
		)
		return
	}
	r.logger.Info("migration advisory lock released",
		"operation", "migrate",
		"key", advisoryLockKey,
	)
}

// validatePrerequisites checks, as the migration identity and before any
// schema work (and before the advisory lock), that the pre-provisioned
// infrastructure the canonical provisioning shape requires is present:
// the connected database is the canonical vector database (configuration
// validation guarantees it; the runner re-verifies it fail-closed), the
// vector extension (pgvector) exists — no migration creates it — and the
// runtime role exists — migration 0004's grants require it.
func (r *Runner) validatePrerequisites(ctx context.Context, conn *pgx.Conn) error {
	var db string
	if err := conn.QueryRow(ctx, "SELECT current_database()").Scan(&db); err != nil {
		return fmt.Errorf("validate prerequisites: read current database: %w", err)
	}
	if db != config.DatabaseName {
		return &IncorrectDatabaseError{Connected: db, Expected: config.DatabaseName}
	}

	var extVersion string
	err := conn.QueryRow(ctx,
		"SELECT extversion FROM pg_extension WHERE extname = 'vector'").Scan(&extVersion)
	if err != nil {
		if err == pgx.ErrNoRows {
			return &MissingExtensionError{Name: "vector"}
		}
		return fmt.Errorf("validate prerequisites: read pg_extension: %w", err)
	}

	var roleOID uint32
	err = conn.QueryRow(ctx,
		"SELECT oid FROM pg_roles WHERE rolname = $1", config.RuntimeRole).Scan(&roleOID)
	if err != nil {
		if err == pgx.ErrNoRows {
			return &MissingRuntimeRoleError{Name: config.RuntimeRole}
		}
		return fmt.Errorf("validate prerequisites: read pg_roles: %w", err)
	}
	return nil
}

// canonicalOwnershipGate enforces the complete canonical ownership contract
// on the pre-existing state of the connected database, under the advisory
// lock and before the bootstrap DDL: the migration identity (the
// connection's current role) must own the vector database, both migrated
// schemas, and every in-scope object that exists — every pg_class row
// (relowner), every pg_type row (typowner), and every pg_proc row
// (proowner) in the two schemas. Any object owned by another role is a
// fatal, structured error naming the object, the catalog it was found in,
// and its actual owner. Pre-existing migrated state is accepted — the
// bootstrap's IF NOT EXISTS clauses are allowed to adopt it — only after
// this gate passes on that same locked pass, and the gate runs on every
// pass: fresh databases have nothing to check, converged databases pass by
// construction, and a post-convergence ownership change to any other role
// is fatal at the next pass before any DDL or history write.
func (r *Runner) canonicalOwnershipGate(ctx context.Context, conn *pgx.Conn) error {
	q := privilege.PgxQuerier{Conn: conn}

	identity, roleOID, err := privilege.CurrentRole(ctx, q)
	if err != nil {
		return fmt.Errorf("canonical ownership gate: %w", err)
	}
	roles, _, err := privilege.Roles(ctx, q)
	if err != nil {
		return fmt.Errorf("canonical ownership gate: %w", err)
	}
	names := privilege.RoleNames(roles)

	db, err := privilege.Database(ctx, q)
	if err != nil {
		return fmt.Errorf("canonical ownership gate: %w", err)
	}
	schemas, err := privilege.Schemas(ctx, q, gateSchemas...)
	if err != nil {
		return fmt.Errorf("canonical ownership gate: %w", err)
	}
	relations, err := privilege.Relations(ctx, q, gateSchemas...)
	if err != nil {
		return fmt.Errorf("canonical ownership gate: %w", err)
	}
	types, err := privilege.Types(ctx, q, gateSchemas...)
	if err != nil {
		return fmt.Errorf("canonical ownership gate: %w", err)
	}
	functions, err := privilege.Functions(ctx, q, gateSchemas...)
	if err != nil {
		return fmt.Errorf("canonical ownership gate: %w", err)
	}

	sweep := privilege.OwnershipSweep{
		Database:  db,
		Schemas:   schemas,
		Relations: relations,
		Types:     types,
		Functions: functions,
	}
	mismatches := privilege.CheckOwnership(sweep, roleOID, names, true)
	if len(mismatches) > 0 {
		return &OwnershipGateError{Identity: identity, Mismatches: mismatches}
	}
	return nil
}

// bootstrap creates the vector_control schema and the migration-history
// table idempotently, in one transaction.
func (r *Runner) bootstrap(ctx context.Context, conn *pgx.Conn) error {
	tx, err := conn.Begin(ctx)
	if err != nil {
		return fmt.Errorf("bootstrap migration history: begin transaction: %w", err)
	}
	defer func() {
		if rbErr := tx.Rollback(ctx); rbErr != nil {
			// Rollback after a successful commit is a no-op.
			// If commit failed, log the cleanup failure with sanitized fields only.
			r.logger.Warn("bootstrap migration history: rollback cleanup",
				"operation", "migrate",
				"cause", sanitizeError(rbErr),
			)
		}
	}()
	if _, err := tx.Exec(ctx, bootstrapSQL); err != nil {
		return fmt.Errorf("bootstrap migration history: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("bootstrap migration history: commit: %w", err)
	}
	return nil
}

type historyRow struct {
	name     string
	checksum string
}

// readHistory loads the applied migration history.
func (r *Runner) readHistory(ctx context.Context, conn *pgx.Conn) (map[int]historyRow, error) {
	rows, err := conn.Query(ctx, historySelectSQL)
	if err != nil {
		return nil, fmt.Errorf("read migration history: %w", err)
	}
	defer rows.Close()

	history := make(map[int]historyRow)
	for rows.Next() {
		var (
			version  int
			name     string
			checksum string
		)
		if err := rows.Scan(&version, &name, &checksum); err != nil {
			return nil, fmt.Errorf("read migration history: %w", err)
		}
		if _, dup := history[version]; dup {
			return nil, fmt.Errorf("read migration history: duplicate row for version %d", version)
		}
		history[version] = historyRow{name: name, checksum: checksum}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read migration history: %w", err)
	}
	return history, nil
}

// applyOne applies a single pending migration in exactly one runner-owned
// transaction: BEGIN → the framing-stripped body (verbatim) → the history
// INSERT (version, name, checksum of the original bytes) → COMMIT. Any
// failure rolls the whole transaction back: no partial schema change and no
// history row.
func (r *Runner) applyOne(ctx context.Context, conn *pgx.Conn, m migration) (Applied, error) {
	start := time.Now()
	tx, err := conn.Begin(ctx)
	if err != nil {
		return Applied{}, fmt.Errorf("apply migration %d (%s): begin transaction: %w", m.version, m.name, err)
	}
	err = func() error {
		if len(m.body) > 0 {
			if _, err := tx.Exec(ctx, string(m.body)); err != nil {
				return fmt.Errorf("execute migration body: %w", err)
			}
		}
		if _, err := tx.Exec(ctx, historyInsertSQL, m.version, m.name, m.checksum); err != nil {
			return fmt.Errorf("insert migration history row: %w", err)
		}
		return tx.Commit(ctx)
	}()
	if err != nil {
		if rbErr := tx.Rollback(ctx); rbErr != nil {
			r.logger.Warn("rollback failed migration",
				"operation", "migrate",
				"version", m.version,
				"name", m.name,
				"cause", sanitizeError(rbErr),
			)
		}
		return Applied{}, fmt.Errorf("apply migration %d (%s): %w", m.version, m.name, err)
	}
	r.logger.Info("migration applied",
		"operation", "migrate",
		"version", m.version,
		"name", m.name,
		"duration", time.Since(start),
	)
	return Applied{
		Version:  m.version,
		Name:     m.name,
		Checksum: m.checksum,
		Duration: time.Since(start),
	}, nil
}

// lockTimeout is returned when the bounded wait for the migration advisory
// lock is exceeded.
type lockTimeout struct{ wait time.Duration }

func (e *lockTimeout) Error() string {
	return fmt.Sprintf("timed out after %s waiting for the %q migration advisory lock", e.wait, advisoryLockKey)
}

// unknownApplied is returned when the migration history contains a version
// that no embedded migration provides. The database was migrated by a
// different migration set; the run fails closed.
type unknownApplied struct {
	version int
	name    string
}

func (e *unknownApplied) Error() string {
	return fmt.Sprintf("migration history contains applied version %d (%s) which is not present in the embedded migration set", e.version, e.name)
}

// checksumDrift is returned when an applied migration's stored checksum
// differs from the embedded file's checksum. The released contract has
// changed after being applied; startup must fail rather than accept it.
type checksumDrift struct {
	version  int
	name     string
	stored   string
	embedded string
}

func (e *checksumDrift) Error() string {
	return fmt.Sprintf(
		"migration checksum drift for version %d (%s): stored %s, embedded %s — the released migration file changed after being applied",
		e.version, e.name, e.stored, e.embedded,
	)
}

// IncorrectDatabaseError is returned by prerequisite validation when the
// runner is connected to a database other than the canonical one.
type IncorrectDatabaseError struct {
	// Connected is the name of the database the connection is to.
	Connected string
	// Expected is the canonical database name.
	Expected string
}

func (e *IncorrectDatabaseError) Error() string {
	return fmt.Sprintf(
		"connected to database %q, which is not the canonical database %q; the migration runner runs against the %q database only",
		e.Connected, e.Expected, e.Expected,
	)
}

// MissingExtensionError is returned by prerequisite validation when a
// required pre-provisioned extension is absent. No migration creates it.
type MissingExtensionError struct {
	// Name is the required extension name.
	Name string
}

func (e *MissingExtensionError) Error() string {
	return fmt.Sprintf(
		"the %s extension is not installed in the connected database; pre-provision it with CREATE EXTENSION %s (no migration creates it)",
		e.Name, e.Name,
	)
}

// MissingRuntimeRoleError is returned by prerequisite validation when the
// canonical runtime role does not exist. Migration 0004's grants require it.
type MissingRuntimeRoleError struct {
	// Name is the required runtime role name.
	Name string
}

func (e *MissingRuntimeRoleError) Error() string {
	return fmt.Sprintf(
		"the runtime role %q does not exist; pre-provision it before migrating (migration 0004's grants require it)",
		e.Name,
	)
}

// OwnershipGateError is returned by the canonical ownership gate when a
// pre-existing in-scope object is not owned by the migration identity.
// Identity is the connection role the gate ran as; Mismatches names every
// violating object with its catalog and actual owner. The run fails before
// the bootstrap DDL and before any history write: nothing is adopted.
type OwnershipGateError struct {
	// Identity is the connection role (the migration identity) the gate
	// ran as.
	Identity string
	// Mismatches is the complete set of in-scope objects not owned by the
	// migration identity, in sweep order.
	Mismatches []privilege.OwnerMismatch
}

func (e *OwnershipGateError) Error() string {
	const shown = 20
	var b strings.Builder
	for i, m := range e.Mismatches {
		if i >= shown {
			fmt.Fprintf(&b, " and %d more", len(e.Mismatches)-shown)
			break
		}
		if i > 0 {
			b.WriteString("; ")
		}
		b.WriteString(m.Error())
	}
	return fmt.Sprintf(
		"canonical ownership gate: the migration identity %q must own every in-scope object, but %s; the pre-existing state is not adopted and no bootstrap DDL or history write occurs",
		e.Identity, b.String(),
	)
}
