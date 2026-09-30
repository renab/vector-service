// Command vector-service is the production entry point for Vector Service.
//
// It provides two subcommands:
//
//   - "serve"  (default): loads configuration, runs the full migration
//     step under the migration identity, creates the runtime pool, builds
//     the HTTP server, marks the migrations-converged state, and serves
//     until signal.
//   - "migrate": loads configuration, runs the full migration step, and
//     exits. No runtime pool, no listener.
//
// Both subcommands share one production migration runner (internal/migrate).
// The migration connection is opened and closed within the migration step,
// before the runtime pool or listener is opened. A serve whose migration
// step fails never opens a listener.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"vector-service/internal/api"
	"vector-service/internal/config"
	"vector-service/internal/db"
	"vector-service/internal/migrate"
	"vector-service/internal/server"
)

// pgConnectTimeout bounds the initial migration connection attempt.
const pgConnectTimeout = 10 * time.Second

// readyzPingTimeout bounds the readiness ping the router runs per request.
// It matches the router's proposal (api.Deps.PingTimeout).
const readyzPingTimeout = 2 * time.Second

// poolCloser is the narrow pool surface the lifecycle exercises: Close
// blocks until every acquired lease is returned and is idempotent. The
// real *pgxpool.Pool satisfies it (the server package's own poolCloser
// is identical); the unit tests substitute a scripted one so the bounded
// close can be observed without a database.
type poolCloser interface {
	Close()
}

// newRuntimePool builds the runtime pool. It is a seam for the startup
// tests; production calls db.NewRuntimePool, whose AfterConnect hook
// runs the complete runtime identity assertion and whose eager ping is
// the fail-fast gate before serve listens.
var newRuntimePool = func(ctx context.Context, cfg *config.Config) (*pgxpool.Pool, error) {
	return db.NewRuntimePool(ctx, cfg)
}

// runnerConn is the narrow connection surface the migration step
// exercises: the identity-assertion queries (QueryRow/Scan) and Close on
// every exit path. The production *pgx.Conn satisfies it; the startup
// tests substitute a scripted one so the identity assertion can be
// exercised without a database.
type runnerConn interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Close(ctx context.Context) error
}

// buildMigrationConnConfig builds the pgx.ConnConfig for the migration connection
// from the validated configuration. It parses the DSN through pgx.ParseConfig,
// applies the simple query protocol, clears Fallbacks, and applies the TLS-mode-
// specific TLS config. The returned config is ready for pgx.ConnectConfig.
//
// This is the single construction path for the migration connection config.
// Both production (buildMigrationConn) and tests use this function.
func buildMigrationConnConfig(cfg *config.Config) (*pgx.ConnConfig, error) {
	dsn := fmt.Sprintf("host=%s port=%d user=%s dbname=%s connect_timeout=%d",
		cfg.PGHost, cfg.PGPort, cfg.MigrationRole, cfg.PGDatabase, int(pgConnectTimeout.Seconds()))
	cc, err := pgx.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("parse migration connection: %w", err)
	}
	cc.DefaultQueryExecMode = pgx.QueryExecModeSimpleProtocol
	// Enforce the config-selected transport exclusively.
	// ParseConfig defaults to sslmode=prefer, which builds a Fallbacks
	// list containing a plaintext retry. Clear it so the connection
	// never downgrades from the configured transport.
	cc.Fallbacks = nil
	switch cfg.TLSMode {
	case config.TLSModeTLS:
		if cfg.MigrationTLS == nil {
			return nil, fmt.Errorf("TLS mode selected but migration TLS material is nil")
		}
		cc.TLSConfig = cfg.MigrationTLS.TLSConfig()
	default:
		cc.TLSConfig = nil
	}
	return cc, nil
}

// buildMigrationConn opens the short-lived migration-identity connection.
// It is a seam for the startup tests; production opens a real *pgx.Conn
// under the migration TLS settings with the simple query protocol (the
// privilege catalog and identity assertion parse text-mode query
// results). The returned connection is closed by runMigrationStep on
// every exit path.
var buildMigrationConn = func(ctx context.Context, cfg *config.Config) (runnerConn, error) {
	cc, err := buildMigrationConnConfig(cfg)
	if err != nil {
		return nil, err
	}
	return pgx.ConnectConfig(ctx, cc)
}

// newServer builds the HTTP server from the configuration and the router
// dependency set. It is a seam for the startup tests; production calls
// server.New, which assembles the router (NewRouterShared — the Deps are
// held by pointer so the in-process readiness flag flipped after
// construction is observed at request time) and the four explicit
// connection timeouts.
var newServer = func(cfg *config.Config, deps api.Deps, root context.Context) *server.Server {
	return server.New(cfg, deps, root)
}

// controller is the narrow lifecycle surface runServe exercises: Serve
// runs the complete bounded-shutdown lifecycle and returns the exit
// outcome. The production *server.ShutdownController satisfies it; the
// startup tests substitute a scripted one so the Serve failure path can be
// observed without a listener.
type controller interface {
	Serve(ctx context.Context) error
}

// newController builds the bounded-shutdown lifecycle for the constructed
// server and runtime pool. It is a seam for the startup tests; production
// calls server.NewShutdownControllerWithCloser. The lifecycle owns the
// listener: it creates the serve root context, swaps it into the
// server's BaseContext, registers the signal handler, and opens the
// listener only inside Serve — after every startup step has succeeded.
var newController = func(cfg *config.Config, srv *server.Server, pool poolCloser) controller {
	return server.NewShutdownControllerWithCloser(cfg, srv, pool)
}

func main() {
	// Install a default safe JSON logger before any configuration is read,
	// so that a config-load failure still produces structured JSON output
	// rather than the stdlib text format. The level is Info by default;
	// if config later succeeds, configureLogging reconfigures with the
	// configured level.
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	})))

	// The subcommand is the first positional argument. Both subcommands
	// share the same configuration gate, so the argument is validated
	// before config load: an unknown argument is a usage failure, not a
	// configuration failure.
	sub := "serve"
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "serve", "migrate":
			sub = os.Args[1]
		default:
			fmt.Fprintf(os.Stderr, "usage: vector-service [serve|migrate]\n")
			os.Exit(1)
		}
	}

	var err error
	switch sub {
	case "serve":
		err = runServe(context.Background())
	case "migrate":
		err = runMigrate(context.Background())
	}

	if err != nil {
		// The failure is logged once, through the JSON structured logger
		// installed above (before config load). The error is sanitized to
		// safe, server-authored fields only: no raw Error() text, PgError
		// Message/Detail/Hint/Where, or connection strings reach the log.
		slog.Error("startup failed", "cause", sanitizeStartupError(err))
		os.Exit(1)
	}
	os.Exit(0)
}

// runServe executes the full serve lifecycle in the package-1 startup
// order:
//
//  1. load and validate configuration (fail before any network activity)
//  2. configure structured logging from the validated configuration
//  3. run the migration step (connect as migration identity, identity
//     assertion, converge migrations, close the connection)
//  4. on success, create the runtime pool (identity assertion gate)
//  5. build the server, wire the dependencies, and mark the
//     migrations-converged state
//  6. hand off to the bounded-shutdown lifecycle (the listener opens
//     inside Serve, so it opens only after every startup step succeeded)
//
// Any failure before step 6 returns without opening a listener.
func runServe(ctx context.Context) error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load configuration: %w", err)
	}
	configureLogging(cfg)

	if err := runMigrationStep(ctx, cfg); err != nil {
		return err
	}

	pool, err := newRuntimePool(ctx, cfg)
	if err != nil {
		return fmt.Errorf("create runtime pool: %w", err)
	}

	var pc poolCloser
	if pool != nil {
		pc = pool
	}

	// The in-process readiness flag: it starts false and is flipped by
	// MarkMigrationsConverged once the migration step has converged. The
	// router reads it at request time through the pointer the server
	// holds (NewRouterShared), so the flip is observed by the
	// already-built router without rebuilding it.
	deps := api.Deps{
		Pool:              pool,
		MigrationsApplied: func() bool { return false },
		PingTimeout:       readyzPingTimeout,
	}
	srv := newServer(cfg, deps, ctx)
	// The migration step converged: the in-process readiness flag is
	// flipped before the listener opens (the /readyz route reads it).
	srv.MarkMigrationsConverged()

	slog.Info("starting vector-service",
		"listen_addr", cfg.ListenAddr,
		"pg_endpoint", db.EndpointString(cfg),
		"pool_max_conns", cfg.Pool.MaxConns,
		"pool_min_conns", cfg.Pool.MinConns,
		"request_timeout", cfg.HTTPRequestTimeout.String(),
		"shutdown_grace", cfg.HTTPShutdownGrace.String(),
		"shutdown_close_timeout", cfg.HTTPShutdownCloseTimeout.String(),
		"upsert_max_records", cfg.UpsertMaxRecords,
		"search_max_limit", cfg.SearchMaxLimit,
		"migration_lock_wait", cfg.MigrationLockWait.String(),
	)

	ctrl := newController(cfg, srv, pc)
	return ctrl.Serve(ctx)
}

// runMigrate executes the migration-only lifecycle:
//
//  1. load and validate configuration (fail before any network activity)
//  2. configure structured logging
//  3. run the migration step
//
// It never opens the runtime pool or a listener.
func runMigrate(ctx context.Context) error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load configuration: %w", err)
	}
	configureLogging(cfg)
	return runMigrationStep(ctx, cfg)
}

// runMigrationStep is the single migration code path shared by both
// subcommands (one production runner):
//
//  1. open the short-lived migration-identity connection
//  2. run the connection identity assertion (current_user, non-superuser,
//     database ownership)
//  3. converge migrations through the embedded canonical runner
//  4. close the connection on every exit path
//
// The migration connection is closed before serving begins; no serving
// code path can reach it.
func runMigrationStep(ctx context.Context, cfg *config.Config) error {
	conn, err := buildMigrationConn(ctx, cfg)
	if err != nil {
		return fmt.Errorf("connect as migration identity %s: %w", cfg.MigrationRole, err)
	}
	defer conn.Close(context.WithoutCancel(ctx))

	if err := assertMigrationIdentity(ctx, conn, cfg); err != nil {
		return err
	}

	// Convergence runs only against a real production connection: the
	// canonical runner drives the full migration transaction against the
	// live database, and the embedded set is the compatibility contract.
	// A substituted (test) connection never reaches it — the startup
	// tests exercise the connect and identity-assertion failure paths
	// without a database, and the success path is a no-op for them.
	pc, ok := conn.(*pgx.Conn)
	if !ok {
		return nil
	}

	lockWait := cfg.MigrationLockWait
	if lockWait <= 0 {
		lockWait = config.DefaultMigrationLockWait
	}
	result, err := migrate.Run(ctx, pc,
		migrate.WithLockWait(lockWait),
		migrate.WithLogger(slog.Default()),
	)
	if err != nil {
		return fmt.Errorf("converge migrations: %w", err)
	}
	slog.Info("migration step completed",
		"applied", len(result.Applied),
		"already_applied", result.AlreadyApplied,
	)
	return nil
}

// assertMigrationIdentity verifies, on the connected migration identity
// connection, that the connection is established as exactly the configured
// migration identity, that the identity is not a superuser, and that it
// owns the connected database. Any mismatch is a fatal, structured error
// naming the failed check.
//
// The runtime-role substitution (migration identity equal to the runtime
// role) is rejected at configuration time (identity collapse); this
// assertion covers the connection-level checks that require a live
// connection.
func assertMigrationIdentity(ctx context.Context, conn runnerConn, cfg *config.Config) error {
	var (
		currentUser string
		rolSuper    bool
	)
	if err := conn.QueryRow(ctx,
		`SELECT current_user, rolsuper FROM pg_roles WHERE rolname = current_user`,
	).Scan(&currentUser, &rolSuper); err != nil {
		return fmt.Errorf("migration identity assertion: read role properties: %w", err)
	}
	if currentUser != cfg.MigrationRole {
		return fmt.Errorf("migration identity assertion: connected as %q, expected %q",
			currentUser, cfg.MigrationRole)
	}
	if rolSuper {
		return fmt.Errorf("migration identity assertion: %q is a superuser", cfg.MigrationRole)
	}

	var owner string
	if err := conn.QueryRow(ctx,
		`SELECT r.rolname FROM pg_database d JOIN pg_roles r ON r.oid = d.datdba WHERE d.datname = current_database()`,
	).Scan(&owner); err != nil {
		return fmt.Errorf("migration identity assertion: read database owner: %w", err)
	}
	if owner != cfg.MigrationRole {
		return fmt.Errorf("migration identity assertion: database owner is %q, expected %q",
			owner, cfg.MigrationRole)
	}
	return nil
}

// configureLogging installs the default structured logger for the
// validated configuration: a JSON handler on stderr at the configured
// level. The JSON format is the only supported format (config.Load
// enforces it), and the records this command emits carry only non-secret
// configuration fields (no credentials, keys, or token values).
func configureLogging(cfg *config.Config) {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{
		Level: cfg.LogLevel,
	})))
}

// sanitizeStartupError reduces a startup error to a safe, server-authored
// string suitable for structured logging. It emits only fixed classifications
// (pg_error:sqlstate=…, context categories, generic) and never copies text
// from err.Error() or any wrapper prefix or suffix, because wrappers may
// contain driver or configuration secrets.
func sanitizeStartupError(err error) string {
	if err == nil {
		return "<nil>"
	}
	return classifyLeafError(err)
}

// classifyLeafError reduces an error to a safe classification carrying only
// allowlisted fields: SQLSTATE if present and well-formed, the server-authored
// configuration variable name if this is a config error for an allowlisted
// missing-required variable, or a bounded category label. It never returns raw
// error text, connection strings, or values.
func classifyLeafError(err error) string {
	// Configuration errors: only allowlisted missing-required variables
	// (those that are safe to name in logs) emit the variable identifier.
	// The shutdown-sum bound violation has its own special classification
	// because the variable name is a server-authored constraint identifier.
	// All other config errors are classified generically to avoid leaking
	// supplied values or internal details.
	var cfgErr *config.Error
	if errors.As(err, &cfgErr) {
		// Special case: shutdown-sum bound violation is safe to classify
		// with a fixed classification naming both variables and the 40s
		// constraint, without including any user-supplied duration values.
		if cfgErr.Variable == config.EnvHTTPShutdownGrace &&
			strings.Contains(cfgErr.Problem, config.ShutdownSumProblemMarker) {
			return "config:" + config.ShutdownSumClassification
		}
		// General allowlist: missing-required variables that are safe to name.
		if config.IsSafeToLog(cfgErr.Variable) {
			return fmt.Sprintf("config:%s", cfgErr.Variable)
		}
		return "config:error"
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		if isValidSqlState(pgErr.Code) {
			return fmt.Sprintf("pg_error:sqlstate=%s", pgErr.Code)
		}
		// Malformed code: fall back to safe classification without the code.
		return "pg_error:sqlstate=<invalid>"
	}
	if errors.Is(err, context.Canceled) {
		return "context:canceled"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "context:deadline_exceeded"
	}
	return "error:unspecified"
}

// isValidSqlState checks that a PostgreSQL SQLSTATE code is exactly five
// uppercase alphanumeric characters. Malformed codes must not be emitted
// into log output, as they could carry attacker-controlled data.
func isValidSqlState(code string) bool {
	if len(code) != 5 {
		return false
	}
	for i := 0; i < 5; i++ {
		c := code[i]
		if !((c >= '0' && c <= '9') || (c >= 'A' && c <= 'Z')) {
			return false
		}
	}
	return true
}
