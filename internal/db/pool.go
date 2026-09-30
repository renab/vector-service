package db

import (
	"context"
	"fmt"
	"strconv"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"vector-service/internal/config"
	"vector-service/internal/privilege"
)

// buildRuntimePoolConfig builds the pgxpool.Config for the runtime pool
// from the validated configuration. It parses the DSN through pgxpool.ParseConfig,
// applies pool parameters, clears Fallbacks, and applies the TLS-mode-specific
// TLS config. The returned config is ready for pgxpool.NewWithConfig.
//
// This is the single construction path for the runtime pool config. Both
// production (NewRuntimePool) and tests use this function.
func buildRuntimePoolConfig(cfg *config.Config) (*pgxpool.Config, error) {
	dsn := fmt.Sprintf("host=%s port=%d user=%s dbname=%s",
		cfg.PGHost, cfg.PGPort, cfg.RuntimeRole, cfg.PGDatabase)
	pc, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("parse runtime pool config: %w", err)
	}
	pc.ConnConfig.DefaultQueryExecMode = pgx.QueryExecModeSimpleProtocol
	pc.MaxConns = cfg.Pool.MaxConns
	pc.MinConns = cfg.Pool.MinConns
	pc.MaxConnLifetime = cfg.Pool.MaxConnLifetime
	pc.MaxConnLifetimeJitter = cfg.Pool.MaxConnLifetimeJitter
	pc.MaxConnIdleTime = cfg.Pool.MaxConnIdleTime
	pc.HealthCheckPeriod = cfg.Pool.HealthCheckPeriod
	pc.AfterConnect = afterConnect(cfg)
	// Enforce the config-selected transport exclusively.
	// ParseConfig defaults to sslmode=prefer, which builds a Fallbacks
	// list containing a plaintext retry. Clear it so the connection
	// never downgrades from the configured transport.
	pc.ConnConfig.Fallbacks = nil
	switch cfg.TLSMode {
	case config.TLSModeTLS:
		if cfg.RuntimeTLS == nil {
			return nil, fmt.Errorf("TLS mode selected but runtime TLS material is nil")
		}
		pc.ConnConfig.TLSConfig = cfg.RuntimeTLS.TLSConfig()
	default:
		pc.ConnConfig.TLSConfig = nil
	}
	return pc, nil
}

// NewRuntimePool builds the single runtime connection pool: one pool for the
// canonical runtime role (vector_api), the only runtime connection factory in
// the codebase (package-1 Scope 2). The pool carries the explicit pool
// parameters from the configuration (no implicit pgxpool defaults) and runs
// the complete runtime identity assertion (RuntimeBoundary) on the startup of
// every physical connection, through the pool's AfterConnect hook: the
// assertion is the fail-fast gate at startup (the first open, during the
// eager ping, happens before serve listens) and the defense in depth on every
// later connection. Any assertion failure is fatal to that connection.
//
// ctx bounds pool construction only: it bounds the initial open (the first
// physical connection's handshake and assertion) and nothing else — the pool
// outlives the context and keeps serving. A nil context is not supported;
// callers pass an explicit bounded context.
func NewRuntimePool(ctx context.Context, cfg *config.Config) (*pgxpool.Pool, error) {
	pc, err := buildRuntimePoolConfig(cfg)
	if err != nil {
		return nil, err
	}

	pool, err := pgxpool.NewWithConfig(ctx, pc)
	if err != nil {
		return nil, fmt.Errorf("open runtime pool: %w", err)
	}
	// Fail fast: open one physical connection and run the runtime identity
	// assertion (RuntimeBoundary) on it before serving may listen. With
	// MinConns = 0 (the explicit configuration default) NewWithConfig opens
	// nothing eagerly, so this ping is what makes the assertion the startup
	// gate rather than a per-request discovery. A connection whose AfterConnect
	// assertion fails is never made available: the ping reports the failure
	// and the pool is closed by the caller (startup failure, no listen).
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("runtime identity assertion on first pool connection: %w", err)
	}
	return pool, nil
}

// afterConnect is the pool's AfterConnect hook: the complete runtime identity
// assertion on every physical connection. pgxpool hands the hook the pooled
// connection's *pgx.Conn; the assertion runs on it through the
// privilege.PgxQuerier adapter, which speaks the simple query protocol the
// text contract requires (the pool's connections are built with
// DefaultQueryExecMode = pgx.QueryExecModeSimpleProtocol).
func afterConnect(cfg *config.Config) func(context.Context, *pgx.Conn) error {
	return func(ctx context.Context, conn *pgx.Conn) error {
		q := privilege.PgxQuerier{Conn: conn}
		if err := RuntimeBoundary(ctx, q, cfg.RuntimeRole, cfg.MigrationRole, config.DatabaseName); err != nil {
			return fmt.Errorf("runtime identity assertion on pool connection: %w", err)
		}
		return nil
	}
}

// EndpointString renders the configured endpoint for the startup summary log.
// It names host, port, and database only: no credential or certificate
// material ever appears.
func EndpointString(cfg *config.Config) string {
	return fmt.Sprintf("%s:%s/%s", cfg.PGHost, strconv.Itoa(cfg.PGPort), cfg.PGDatabase)
}
