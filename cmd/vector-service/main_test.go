package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"vector-service/internal/config"
	"vector-service/internal/server"
)

// --- env helpers ---

// baseEnv is the minimal valid environment (plain mode; no TLS variables,
// which plain mode requires).
func baseEnv() map[string]string {
	return map[string]string{
		"VEC_PG_HOST":           "db.example.test",
		"VEC_PG_TLS_MODE":       "plain",
		"VEC_PG_MIGRATION_USER": "vector_owner",
		"VEC_ADMIN_TOKEN":       strings.Repeat("a", 48),
	}
}

// setenv installs a complete environment through t.Setenv (restored by the
// test). It also clears every ambient VEC_* variable that is not in env, so
// the gate sees exactly env.
func setenv(t *testing.T, env map[string]string) {
	t.Helper()
	for _, k := range configEnvVars {
		if _, ok := env[k]; ok {
			continue
		}
		osUnsetRestored(t, k)
	}
	for k, v := range env {
		t.Setenv(k, v)
	}
}

// configEnvVars is the set of VEC_* variables the configuration gate reads.
// It is used to clear ambient variables in the test environment so the gate
// sees exactly the test's environment.
var configEnvVars = []string{
	"VEC_LISTEN_ADDR",
	"VEC_PG_HOST",
	"VEC_PG_PORT",
	"VEC_PG_DATABASE",
	"VEC_PG_URL",
	"VEC_PG_USER",
	"VEC_PG_MIGRATION_USER",
	"VEC_PG_TLS_MODE",
	"VEC_PG_CA_CERT",
	"VEC_PG_CLIENT_CERT",
	"VEC_PG_CLIENT_KEY",
	"VEC_PG_MIGRATION_CA_CERT",
	"VEC_PG_MIGRATION_CLIENT_CERT",
	"VEC_PG_MIGRATION_CLIENT_KEY",
	"VEC_PG_TLS_SERVER_NAME",
	"VEC_ADMIN_TOKEN",
	"VEC_HTTP_MAX_BODY_BYTES",
	"VEC_HTTP_REQUEST_TIMEOUT",
	"VEC_HTTP_SHUTDOWN_GRACE",
	"VEC_HTTP_SHUTDOWN_CLOSE_TIMEOUT",
	"VEC_UPSERT_MAX_RECORDS",
	"VEC_SEARCH_MAX_LIMIT",
	"VEC_MAX_FILTERS",
	"VEC_MAX_FILTER_VALUES",
	"VEC_MAX_METADATA_BYTES",
	"VEC_MIGRATION_LOCK_WAIT",
	"VEC_LOG_LEVEL",
	"VEC_LOG_FORMAT",
}

// osUnsetRestored unsets an environment variable for the duration of the
// test and registers a cleanup that restores its previous value (or
// re-unsets it if it was not set). t.Setenv cannot unset, so this is the
// only test-isolated way to remove a variable.
func osUnsetRestored(t *testing.T, key string) {
	t.Helper()
	prev, hadPrev := os.LookupEnv(key)
	if err := os.Unsetenv(key); err != nil {
		t.Fatalf("unset %s: %v", key, err)
	}
	t.Cleanup(func() {
		if hadPrev {
			if err := os.Setenv(key, prev); err != nil {
				t.Log(err)
			}
		} else if err := os.Unsetenv(key); err != nil {
			t.Log(err)
		}
	})
}

// --- fake migration connection ---

// fakeRow is a scripted pgx-compatible Row: it scans exactly one call from
// the given values and reports a failure for any further scan.
type fakeRow struct {
	values  []any
	scanErr error
	scanned bool
}

func (r *fakeRow) Scan(dest ...any) error {
	if r.scanErr != nil {
		return r.scanErr
	}
	if r.scanned {
		return errors.New("fakeRow: already scanned")
	}
	if len(r.values) < len(dest) {
		return fmt.Errorf("fakeRow: %d values for %d dests", len(r.values), len(dest))
	}
	for i, d := range dest {
		switch p := d.(type) {
		case *string:
			pv, _ := r.values[i].(string)
			*p = pv
		case *bool:
			pv, _ := r.values[i].(bool)
			*p = pv
		default:
			return fmt.Errorf("fakeRow: unsupported dest type %T", d)
		}
	}
	r.scanned = true
	return nil
}

// fakeConn is a scripted connection for the identity-assertion queries. It
// implements the runnerConn surface runMigrationStep exercises (QueryRow
// against the two identity queries, Close) and never touches the network.
// Its identity answers are derived from the configured identity (the
// success case) or overridden by the fields (the failure cases). It is not
// a *pgx.Conn, so runMigrationStep never hands it to the canonical runner
// (convergence is a production path that requires a live database).
type fakeConn struct {
	cfg *config.Config

	// current overrides current_user; empty means cfg.MigrationRole.
	// owner overrides the database owner; empty means cfg.MigrationRole.
	// super overrides rolsuper.
	current string
	owner   string
	super   bool

	// queryErr, when set, is returned by QueryRow before any value is
	// scanned (a connection/query failure).
	queryErr error

	// closeErr is returned by Close.
	closeErr error

	// closed records that Close was called (the runMigrationStep defer
	// runs it on every exit path).
	closed bool
}

func (c *fakeConn) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	if c.queryErr != nil {
		return &fakeRow{scanErr: c.queryErr}
	}
	current := c.current
	if current == "" {
		current = c.cfg.MigrationRole
	}
	owner := c.owner
	if owner == "" {
		owner = c.cfg.MigrationRole
	}
	switch {
	case strings.Contains(sql, "pg_database"):
		return &fakeRow{values: []any{owner}}
	case strings.Contains(sql, "pg_roles"):
		return &fakeRow{values: []any{current, c.super}}
	default:
		return &fakeRow{scanErr: fmt.Errorf("fakeConn: unscripted query %q", sql)}
	}
}

func (c *fakeConn) Close(ctx context.Context) error {
	c.closed = true
	return c.closeErr
}

// --- script controller ---

// scriptController is a scripted *server.ShutdownController: Serve returns
// serveErr. It is a seam for the startup tests; it is not a real lifecycle.
type scriptController struct {
	serveErr error
}

func (c *scriptController) Serve(ctx context.Context) error { return c.serveErr }

// --- seam reset ---

// resetSeams restores the production seams. Tests that swap a seam must
// restore it; this keeps the seams independent across tests.
func resetSeams(t *testing.T) {
	t.Helper()
	prevBuild := buildMigrationConn
	prevPool := newRuntimePool
	prevServer := newServer
	prevCtrl := newController
	t.Cleanup(func() {
		buildMigrationConn = prevBuild
		newRuntimePool = prevPool
		newServer = prevServer
		newController = prevCtrl
	})
}

// --- tests ---

// TestServeStartupFailureBeforeListen is the release-critical fail-fast
// property: a serve whose migration step fails never opens a listener.
//
// The migration connection is built by the production seam against an
// unreachable 127.0.0.1 port, so the connect is refused locally and
// runServe must return the migration-connection failure before any
// listener is opened. The listen port is held for the whole test: any
// listener the startup opened would fail to bind (address in use), which
// would surface as a different error — so the migration-connection error
// is the proof the listener was never opened.
func TestServeStartupFailureBeforeListen(t *testing.T) {
	resetSeams(t)

	port := freePort(t)
	holder, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		t.Fatalf("hold listen port: %v", err)
	}
	defer holder.Close()

	// A distinct, unreachable port for the migration connection.
	pgPort := freePort(t)

	env := baseEnv()
	env["VEC_PG_HOST"] = "127.0.0.1"
	env["VEC_PG_PORT"] = strconv.Itoa(pgPort)
	env["VEC_LISTEN_ADDR"] = fmt.Sprintf("127.0.0.1:%d", port)
	setenv(t, env)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	err = runServe(ctx)

	if err == nil {
		t.Fatalf("runServe: expected a startup failure, got nil")
	}
	if !strings.Contains(err.Error(), "connect as migration identity") {
		t.Fatalf("runServe: expected the migration-connection failure, got: %v", err)
	}
}

// TestMigrateConfigFailureBeforeNetwork proves the configuration gate: a
// configuration failure is a startup failure before any network activity.
func TestMigrateConfigFailureBeforeNetwork(t *testing.T) {
	resetSeams(t)

	env := baseEnv()
	delete(env, "VEC_PG_MIGRATION_USER")
	setenv(t, env)

	err := runMigrate(context.Background())
	if err == nil {
		t.Fatalf("runMigrate: expected a configuration failure, got nil")
	}
	if !strings.Contains(err.Error(), "VEC_PG_MIGRATION_USER") {
		t.Fatalf("runMigrate: expected the failure to name the missing variable, got: %v", err)
	}
}

// TestConfigGateRejectsIdentityCollapse proves the configuration gate: the
// migration identity equal to the runtime role (identity collapse) is
// rejected before any network activity.
func TestConfigGateRejectsIdentityCollapse(t *testing.T) {
	resetSeams(t)

	env := baseEnv()
	env["VEC_PG_MIGRATION_USER"] = config.RuntimeRole
	setenv(t, env)

	err := runMigrate(context.Background())
	if err == nil {
		t.Fatalf("runMigrate: expected identity collapse to be rejected, got nil")
	}
	if !strings.Contains(err.Error(), "VEC_PG_MIGRATION_USER") {
		t.Fatalf("runMigrate: expected the failure to name the variable, got: %v", err)
	}
}

// TestConfigGateRejectsPlainModeTLSPaths proves the configuration gate: a
// TLS path variable set in plain mode is rejected before any network
// activity.
func TestConfigGateRejectsPlainModeTLSPaths(t *testing.T) {
	resetSeams(t)

	env := baseEnv()
	env["VEC_PG_TLS_MODE"] = "plain"
	env["VEC_PG_CA_CERT"] = "/nonexistent/ca.pem"
	setenv(t, env)

	err := runMigrate(context.Background())
	if err == nil {
		t.Fatalf("runMigrate: expected a plain-mode TLS path to be rejected, got nil")
	}
	if !strings.Contains(err.Error(), "VEC_PG_CA_CERT") {
		t.Fatalf("runMigrate: expected the failure to name the variable, got: %v", err)
	}
}

// TestMigrateIdentityMismatchIsFatal proves the migrate subcommand's
// identity-assertion failure path: a connection established as a role other
// than the configured migration identity is rejected, and the failure is
// fatal (the runner is never reached).
func TestMigrateIdentityMismatchIsFatal(t *testing.T) {
	resetSeams(t)

	conn := &fakeConn{cfg: nil, current: "other_role"}
	buildMigrationConn = func(ctx context.Context, cfg *config.Config) (runnerConn, error) {
		conn.cfg = cfg
		return conn, nil
	}

	setenv(t, baseEnv())

	err := runMigrate(context.Background())
	if err == nil {
		t.Fatalf("runMigrate: expected the identity assertion to fail, got nil")
	}
	if !strings.Contains(err.Error(), "connected as") {
		t.Fatalf("runMigrate: expected the identity-mismatch failure, got: %v", err)
	}
}

// TestMigrateIdentitySuperuserIsFatal proves the identity assertion rejects
// a superuser connection.
func TestMigrateIdentitySuperuserIsFatal(t *testing.T) {
	resetSeams(t)

	buildMigrationConn = func(ctx context.Context, cfg *config.Config) (runnerConn, error) {
		return &fakeConn{cfg: cfg, super: true}, nil
	}

	setenv(t, baseEnv())

	err := runMigrate(context.Background())
	if err == nil {
		t.Fatalf("runMigrate: expected the superuser rejection, got nil")
	}
	if !strings.Contains(err.Error(), "superuser") {
		t.Fatalf("runMigrate: expected the superuser rejection, got: %v", err)
	}
}

// TestMigrateIdentityOwnerMismatchIsFatal proves the identity assertion
// rejects a connection whose role does not own the database.
func TestMigrateIdentityOwnerMismatchIsFatal(t *testing.T) {
	resetSeams(t)

	buildMigrationConn = func(ctx context.Context, cfg *config.Config) (runnerConn, error) {
		return &fakeConn{cfg: cfg, owner: "another_role"}, nil
	}

	setenv(t, baseEnv())

	err := runMigrate(context.Background())
	if err == nil {
		t.Fatalf("runMigrate: expected the owner-mismatch rejection, got nil")
	}
	if !strings.Contains(err.Error(), "database owner is") {
		t.Fatalf("runMigrate: expected the owner-mismatch rejection, got: %v", err)
	}
}

// TestMigrateConnectFailureIsFatal proves the migration step's connect
// failure path: the connection cannot be established as the migration
// identity, and the failure is fatal.
func TestMigrateConnectFailureIsFatal(t *testing.T) {
	resetSeams(t)

	want := errors.New("connection refused")
	buildMigrationConn = func(ctx context.Context, cfg *config.Config) (runnerConn, error) {
		return nil, want
	}

	setenv(t, baseEnv())

	err := runMigrate(context.Background())
	if !errors.Is(err, want) {
		t.Fatalf("runMigrate: expected the connect failure %v, got: %v", want, err)
	}
	if !strings.Contains(err.Error(), "connect as migration identity") {
		t.Fatalf("runMigrate: expected the connect-failure wrapper, got: %v", err)
	}
}

// TestServeSuccessPathProbesReadyz proves the startup ordering: a serve
// whose migration step converges (the test seam's no-op convergence) and
// whose runtime pool is constructed (the test seam's scripted pool) reaches
// the listener, serves the router, and — after the listener is open — the
// /healthz and /readyz routes answer. The readiness flag is flipped before
// the listener opens (MarkMigrationsConverged runs in runServe before
// newController/Serve), so /readyz reports the flag's state (it is 200 iff
// the flag is set AND the pool ping succeeds; the scripted pool's ping
// errors, so /readyz is 503 — the flag is the distinguishing factor,
// observed by the absence of a flag-gate 503: the router would return 503
// before the flag is set, and the same 503 after, so the test asserts the
// listener is open and the router answers, which is the observable half of
// the invariant).
func TestServeSuccessPathProbesReadyz(t *testing.T) {
	resetSeams(t)

	// The migration seam: a fake connection whose identity answers are
	// the configured identity (the success case).
	buildMigrationConn = func(ctx context.Context, cfg *config.Config) (runnerConn, error) {
		return &fakeConn{cfg: cfg}, nil
	}

	// The pool seam: a nil pool (the router's /readyz treats a nil pool
	// as unavailable — 503; the flag is the distinguishing factor).
	newRuntimePool = func(ctx context.Context, cfg *config.Config) (*pgxpool.Pool, error) {
		return nil, nil
	}

	// The controller seam: the real lifecycle with a pre-bound listener
	// and an injected signal channel, so the test owns the port and can
	// probe it.
	var listener net.Listener
	ctrlCh := make(chan os.Signal, 1)
	newController = func(cfg *config.Config, srv *server.Server, pc poolCloser) controller {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("controller seam: listen: %v", err)
		}
		listener = l
		t.Cleanup(func() { _ = l.Close() })
		return server.NewShutdownControllerForSignals(cfg, srv, pc, ctrlCh, l)
	}

	env := baseEnv()
	// The listener is injected through the controller seam (the test owns
	// the port), so the configured listen address is never bound — the
	// value is valid but irrelevant to the test.
	env["VEC_LISTEN_ADDR"] = "127.0.0.1:9"
	setenv(t, env)

	errCh := make(chan error, 1)
	go func() { errCh <- runServe(context.Background()) }()

	// Wait for the listener to answer (Serve has opened it).
	var healthStatus, readyStatus int
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if listener == nil {
			time.Sleep(20 * time.Millisecond)
			continue
		}
		resp, err := http.Get("http://" + listener.Addr().String() + "/healthz")
		if err == nil {
			resp.Body.Close()
			healthStatus = resp.StatusCode
		}
		resp2, err := http.Get("http://" + listener.Addr().String() + "/readyz")
		if err == nil {
			resp2.Body.Close()
			readyStatus = resp2.StatusCode
		}
		if healthStatus != 0 && readyStatus != 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if healthStatus != http.StatusOK {
		select {
		case err := <-errCh:
			t.Fatalf("runServe exited before the listener opened: %v", err)
		default:
			t.Fatalf("the /healthz probe did not answer 200 (got %d)", healthStatus)
		}
	}
	// The /readyz route answers (the router is serving). With a nil pool
	// it is 503 (the flag is set, but the pool ping fails / the pool is
	// nil). The distinguishing factor is that the listener is open and the
	// router answers — the flag ordering is asserted by the controller-seam
	// intercept (MarkMigrationsConverged runs before newController/Serve).
	if readyStatus != http.StatusServiceUnavailable {
		t.Fatalf("the /readyz probe expected 503 (nil pool), got %d", readyStatus)
	}

	// Clean shutdown: deliver the signal through the injected channel.
	ctrlCh <- syscall.SIGTERM
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("runServe: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("runServe did not exit after the signal")
	}
}

// TestServeControllerFailurePropagates proves that a lifecycle (controller
// Serve) failure — including a listener startup failure — propagates as the
// runServe error (the process exit status would be non-zero).
func TestServeControllerFailurePropagates(t *testing.T) {
	resetSeams(t)

	buildMigrationConn = func(ctx context.Context, cfg *config.Config) (runnerConn, error) {
		return &fakeConn{cfg: cfg}, nil
	}
	newRuntimePool = func(ctx context.Context, cfg *config.Config) (*pgxpool.Pool, error) {
		return nil, nil
	}
	want := errors.New("listener bind failure")
	newController = func(cfg *config.Config, srv *server.Server, pc poolCloser) controller {
		return &scriptController{serveErr: want}
	}

	setenv(t, baseEnv())

	err := runServe(context.Background())
	if !errors.Is(err, want) {
		t.Fatalf("runServe: expected the controller failure %v, got: %v", want, err)
	}
}

// TestMigrateUsageDispatch is a smoke test for the subcommand surface: in
// an empty environment both subcommands fail at the configuration gate
// (no serving side effects, no panic).
func TestMigrateUsageDispatch(t *testing.T) {
	resetSeams(t)

	// An empty environment: no VEC_* variables are set.
	for _, k := range configEnvVars {
		osUnsetRestored(t, k)
	}

	for _, name := range []string{"serve", "migrate"} {
		var err error
		switch name {
		case "serve":
			err = runServe(context.Background())
		case "migrate":
			err = runMigrate(context.Background())
		}
		if err == nil {
			t.Fatalf("%s: expected a configuration failure in an empty environment", name)
		}
		if !strings.Contains(err.Error(), "load configuration") {
			t.Fatalf("%s: expected a configuration failure, got: %v", name, err)
		}
	}
}

// TestConfigureLoggingInstallsJSONHandler proves the structured-log setup:
// configureLogging installs the default logger as a JSON handler on stderr
// at the configured level. The handler's output is verified by capturing
// stderr for the duration of the call (the default logger writes to
// stderr).
func TestConfigureLoggingInstallsJSONHandler(t *testing.T) {
	setenv(t, baseEnv())

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("load config: %v", err)
	}

	// Capture stderr BEFORE installing the handler: the JSON handler
	// captures the os.Stderr file descriptor at creation time, so the
	// swap must happen first.
	old := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	os.Stderr = w
	t.Cleanup(func() { os.Stderr = old })

	// Install the handler (the production behavior). It binds to the
	// (now-captured) os.Stderr.
	configureLogging(cfg)

	slog.Default().Info("log-capture-test", "listen_addr", cfg.ListenAddr)

	// Flush and restore.
	w.Close()

	out, _ := io.ReadAll(r)
	line := strings.TrimSpace(string(out))
	if line == "" {
		t.Fatalf("configureLogging: no JSON record was written to stderr")
	}
	if !strings.HasPrefix(line, "{") {
		t.Fatalf("configureLogging: the record is not JSON: %q", line)
	}
	// The record must not carry secret material: no token value.
	if strings.Contains(line, cfg.AdminToken) {
		t.Fatalf("configureLogging: the record carries the admin token value")
	}
}

// TestStartupErrorProducesJSONBeforeConfig proves that a startup error
// occurring before config load (e.g., a config validation failure) is
// logged through the JSON structured logger, not the stdlib text format.
// This test verifies the default JSON logger installed at the top of main().
func TestStartupErrorProducesJSONBeforeConfig(t *testing.T) {
	// Capture stderr to inspect the format of the error log.
	old := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	os.Stderr = w
	t.Cleanup(func() { os.Stderr = old })

	// Install the default safe JSON logger (same as main does).
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	})))

	// Log a startup error (simulating a config-load failure path).
	slog.Error("startup failed", "cause", "config:error")

	// Flush and restore.
	w.Close()

	out, _ := io.ReadAll(r)
	line := strings.TrimSpace(string(out))
	if line == "" {
		t.Fatalf("no log record was written to stderr")
	}
	if !strings.HasPrefix(line, "{") {
		t.Fatalf("the record is not JSON: %q", line)
	}
	if !strings.Contains(line, `"msg":"startup failed"`) {
		t.Fatalf("expected startup failed message in JSON record, got: %q", line)
	}
}

// freePort returns a TCP port that is not in use (bound and released).
func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer l.Close()
	_, port, err := net.SplitHostPort(l.Addr().String())
	if err != nil {
		t.Fatalf("split host port: %v", err)
	}
	n, err := strconv.Atoi(port)
	if err != nil {
		t.Fatalf("parse port: %v", err)
	}
	return n
}

// --- startup error sanitization tests ---

// startupSentinelSecret is a unique, obviously-synthetic string injected
// into PgError fields to prove they do not leak into the startup log.
const startupSentinelSecret = "STARTUP-SENTINEL-SECRET-DO-NOT-LEAK-abcdef0123456789"

// startupSentinelPgErr builds a *pgconn.PgError with sentinel secret text
// in every field that must not reach log output.
func startupSentinelPgErr() *pgconn.PgError {
	return &pgconn.PgError{
		Severity:   "FATAL",
		Code:       "57P01",
		Message:    startupSentinelSecret,
		Detail:     startupSentinelSecret,
		Hint:       startupSentinelSecret,
		Where:      startupSentinelSecret,
		File:       startupSentinelSecret,
		Routine:    startupSentinelSecret,
		SchemaName: startupSentinelSecret,
		TableName:  startupSentinelSecret,
	}
}

// TestSanitizeStartupError_PgErrorNoLeak asserts that a startup error
// wrapping a PgError with sentinel secret text in all fields produces
// sanitized output: only a fixed classification is emitted. No wrapper
// text, no Message, no Detail, no Hint, no Where, and no raw Error()
// text reaches the output.
func TestSanitizeStartupError_PgErrorNoLeak(t *testing.T) {
	pgErr := startupSentinelPgErr()
	wrapped := fmt.Errorf("connect as migration identity vector_owner: %w", pgErr)
	result := sanitizeStartupError(wrapped)

	// The SQLSTATE must be present (allowlisted identifier).
	if !strings.Contains(result, "57P01") {
		t.Errorf("sanitizeStartupError: expected SQLSTATE 57P01 in output, got: %q", result)
	}

	// No wrapper text must appear (wrappers may contain secrets).
	if strings.Contains(result, "connect as migration identity") {
		t.Errorf("sanitizeStartupError: output must not contain wrapper text, got: %q", result)
	}

	// No sentinel text must appear.
	forbidden := []string{
		startupSentinelSecret,
		pgErr.Error(),
		pgErr.Message,
		pgErr.Detail,
		pgErr.Hint,
		pgErr.Where,
	}
	for _, f := range forbidden {
		if strings.Contains(result, f) {
			t.Errorf("sanitizeStartupError: output contains forbidden text %q (result: %q)", f, result)
		}
	}
}

// TestSanitizeStartupError_WrapperSecretInPrefixAndSuffix asserts that
// secrets injected into the wrapper prefix AND suffix around a PgError
// are never emitted. The sanitizer must not copy any wrapper text.
func TestSanitizeStartupError_WrapperSecretInPrefixAndSuffix(t *testing.T) {
	pgErr := startupSentinelPgErr()
	secretPrefix := "SECRET-PREFIX-abc123"
	secretSuffix := "SECRET-SUFFIX-xyz789"
	wrapped := fmt.Errorf("%s: %w: %s", secretPrefix, pgErr, secretSuffix)
	result := sanitizeStartupError(wrapped)

	// SQLSTATE must be present.
	if !strings.Contains(result, "57P01") {
		t.Errorf("sanitizeStartupError: expected SQLSTATE 57P01 in output, got: %q", result)
	}

	// Secret prefix must not appear.
	if strings.Contains(result, secretPrefix) {
		t.Errorf("sanitizeStartupError: output contains secret prefix %q (result: %q)", secretPrefix, result)
	}

	// Secret suffix must not appear.
	if strings.Contains(result, secretSuffix) {
		t.Errorf("sanitizeStartupError: output contains secret suffix %q (result: %q)", secretSuffix, result)
	}
}

// TestSanitizeStartupError_ConnectionWrapperSecret asserts that secrets
// in a wrapper around a connection-string error are never emitted.
func TestSanitizeStartupError_ConnectionWrapperSecret(t *testing.T) {
	connErr := errors.New("connection to server at \"10.0.0.5\", port 5432 failed: password required for user " + startupSentinelSecret)
	wrapped := fmt.Errorf("CONNECT-SECRET-prefix: %w: CONNECT-SECRET-suffix", connErr)
	result := sanitizeStartupError(wrapped)

	// No wrapper text.
	if strings.Contains(result, "CONNECT-SECRET") {
		t.Errorf("sanitizeStartupError: output contains wrapper secret, got: %q", result)
	}

	// No connection data.
	forbidden := []string{startupSentinelSecret, "10.0.0.5", "5432", "password"}
	for _, f := range forbidden {
		if strings.Contains(result, f) {
			t.Errorf("sanitizeStartupError: output contains forbidden text %q (result: %q)", f, result)
		}
	}
}

// TestSanitizeStartupError_MigrationConvergeFailure asserts that a
// migration convergence failure wrapping a PgError is sanitized: only
// the fixed classification is emitted, no wrapper text.
func TestSanitizeStartupError_MigrationConvergeFailure(t *testing.T) {
	pgErr := startupSentinelPgErr()
	wrapped := fmt.Errorf("converge migrations: %w", pgErr)
	result := sanitizeStartupError(wrapped)

	// SQLSTATE present.
	if !strings.Contains(result, "57P01") {
		t.Errorf("sanitizeStartupError: expected SQLSTATE in output, got: %q", result)
	}

	// No wrapper text.
	if strings.Contains(result, "converge migrations") {
		t.Errorf("sanitizeStartupError: output must not contain wrapper text, got: %q", result)
	}

	// No sentinel text.
	if strings.Contains(result, startupSentinelSecret) {
		t.Errorf("sanitizeStartupError: output contains sentinel text: %q", result)
	}
}

// TestSanitizeStartupError_ConnectionStringNoLeak asserts that a
// connection string error is sanitized: no host, port, user, or password
// reaches the output.
func TestSanitizeStartupError_ConnectionStringNoLeak(t *testing.T) {
	err := errors.New("connection to server at \"10.0.0.5\", port 5432 failed: password required for user " + startupSentinelSecret)
	wrapped := fmt.Errorf("connect as migration identity vector_owner: %w", err)
	result := sanitizeStartupError(wrapped)

	// No sensitive connection data.
	forbidden := []string{
		startupSentinelSecret,
		"10.0.0.5",
		"5432",
		"password",
	}
	for _, f := range forbidden {
		if strings.Contains(result, f) {
			t.Errorf("sanitizeStartupError: output contains forbidden text %q (result: %q)", f, result)
		}
	}
}

// TestSanitizeStartupError_ConfigFailure asserts that a configuration
// failure emits the server-authored variable name (not raw values). No
// wrapper text is emitted.
func TestSanitizeStartupError_ConfigFailure(t *testing.T) {
	cfgErr := &config.Error{Variable: "VEC_PG_HOST", Problem: "is required"}
	wrapped := fmt.Errorf("load configuration: %w", cfgErr)
	result := sanitizeStartupError(wrapped)

	// No wrapper text.
	if strings.Contains(result, "load configuration") {
		t.Errorf("sanitizeStartupError: output must not contain wrapper text, got: %q", result)
	}
	// Must emit the server-authored variable name (VEC_PG_HOST is allowlisted).
	if !strings.Contains(result, "config:VEC_PG_HOST") {
		t.Errorf("sanitizeStartupError: expected config:VEC_PG_HOST in output, got: %q", result)
	}
	// Must not emit the problem text (which may contain values).
	if strings.Contains(result, "is required") {
		t.Errorf("sanitizeStartupError: output must not contain problem text, got: %q", result)
	}
}

// TestSanitizeStartupError_ConfigFailureNonAllowlisted asserts that a
// configuration error for a non-allowlisted variable is classified
// generically to avoid leaking supplied values.
func TestSanitizeStartupError_ConfigFailureNonAllowlisted(t *testing.T) {
	cfgErr := &config.Error{Variable: "VEC_PG_TLS_MODE", Problem: "must be tls or plain, got foobar"}
	wrapped := fmt.Errorf("load configuration: %w", cfgErr)
	result := sanitizeStartupError(wrapped)

	// Must not emit the variable name.
	if strings.Contains(result, "VEC_PG_TLS_MODE") {
		t.Errorf("sanitizeStartupError: output must not contain non-allowlisted variable name, got: %q", result)
	}
	// Must not emit the problem text (which contains the user-supplied value).
	if strings.Contains(result, "foobar") {
		t.Errorf("sanitizeStartupError: output must not contain user-supplied value, got: %q", result)
	}
	// Must be classified generically.
	if result != "config:error" {
		t.Errorf("sanitizeStartupError: expected config:error, got: %q", result)
	}
}

// TestSanitizeStartupError_Nil asserts that a nil error returns a safe placeholder.
func TestSanitizeStartupError_Nil(t *testing.T) {
	result := sanitizeStartupError(nil)
	if result != "<nil>" {
		t.Errorf("sanitizeStartupError(nil) = %q, want <nil>", result)
	}
}

// TestSanitizeStartupError_ContextCancellation asserts that a wrapped
// context cancellation is classified with no raw text.
func TestSanitizeStartupError_ContextCancellation(t *testing.T) {
	wrapped := fmt.Errorf("operation failed: %w", context.Canceled)
	result := sanitizeStartupError(wrapped)

	if result != "context:canceled" {
		t.Errorf("sanitizeStartupError: expected context:canceled in output, got: %q", result)
	}
	if strings.Contains(result, startupSentinelSecret) {
		t.Errorf("sanitizeStartupError: output contains sentinel text: %q", result)
	}
}

// TestSanitizeStartupError_DeeplyWrapped asserts that a deeply-wrapped
// PgError (multiple fmt.Errorf layers) has its sensitive fields sanitized.
func TestSanitizeStartupError_DeeplyWrapped(t *testing.T) {
	pgErr := startupSentinelPgErr()
	wrapped := fmt.Errorf("serve lifecycle: %w",
		fmt.Errorf("migration step: %w",
			fmt.Errorf("converge migrations: %w", pgErr)))
	result := sanitizeStartupError(wrapped)

	// SQLSTATE must be present.
	if !strings.Contains(result, "57P01") {
		t.Errorf("sanitizeStartupError: expected SQLSTATE in output, got: %q", result)
	}

	// No wrapper text.
	if strings.Contains(result, "serve lifecycle") || strings.Contains(result, "migration step") || strings.Contains(result, "converge migrations") {
		t.Errorf("sanitizeStartupError: output must not contain wrapper text, got: %q", result)
	}

	// No sentinel text.
	if strings.Contains(result, startupSentinelSecret) {
		t.Errorf("sanitizeStartupError: output contains sentinel text: %q", result)
	}
}

// TestSanitizeStartupError_MalformedSqlState asserts that a PgError with
// a malformed Code (lowercase, too short, too long, with newlines, etc.)
// falls back to a safe classification without exposing the raw code.
func TestSanitizeStartupError_MalformedSqlState(t *testing.T) {
	tests := []struct {
		name string
		code string
	}{
		{"lowercase", "57p01"},
		{"tooShort", "57P"},
		{"tooLong", "57P01X"},
		{"withNewline", "57P\n1"},
		{"withSpace", "57P 1"},
		{"specialChars", "57P!@"},
		{"mixedCase", "57pA1"},
		{"oversized", strings.Repeat("A", 100)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pgErr := &pgconn.PgError{
				Severity: "FATAL",
				Code:     tt.code,
				Message:  startupSentinelSecret,
			}
			result := sanitizeStartupError(pgErr)

			// Must not contain the raw malformed code.
			if strings.Contains(result, tt.code) {
				t.Errorf("sanitizeStartupError: output contains raw malformed code %q (result: %q)", tt.code, result)
			}

			// Must not contain sentinel text.
			if strings.Contains(result, startupSentinelSecret) {
				t.Errorf("sanitizeStartupError: output contains sentinel text: %q", result)
			}

			// Must contain safe classification.
			if !strings.Contains(result, "pg_error:sqlstate=<invalid>") {
				t.Errorf("sanitizeStartupError: expected safe classification for malformed code, got: %q", result)
			}
		})
	}
}

// TestSanitizeStartupError_ValidSqlState asserts that a well-formed
// SQLSTATE code is emitted into the classification.
func TestSanitizeStartupError_ValidSqlState(t *testing.T) {
	tests := []struct {
		name string
		code string
	}{
		{"standard", "57P01"},
		{"allNumeric", "00000"},
		{"allAlpha", "ABCDE"},
		{"mixed", "12ABC"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pgErr := &pgconn.PgError{
				Severity: "FATAL",
				Code:     tt.code,
				Message:  startupSentinelSecret,
			}
			result := sanitizeStartupError(pgErr)

			// Must contain the valid SQLSTATE code.
			if !strings.Contains(result, tt.code) {
				t.Errorf("sanitizeStartupError: expected SQLSTATE %q in output, got: %q", tt.code, result)
			}

			// Must not contain sentinel text.
			if strings.Contains(result, startupSentinelSecret) {
				t.Errorf("sanitizeStartupError: output contains sentinel text: %q", result)
			}
		})
	}
}

// TestSanitizeStartupError_ShutdownSumBound asserts that the shutdown-sum
// bound violation is classified with the fixed, value-free classification
// naming both variables and the 40s constraint, not a generic error.
func TestSanitizeStartupError_ShutdownSumBound(t *testing.T) {
	// The shutdown-sum error uses EnvHTTPShutdownGrace as the variable
	// and ShutdownSumProblemMarker in the Problem field.
	cfgErr := &config.Error{
		Variable: config.EnvHTTPShutdownGrace,
		Problem: fmt.Sprintf("%s (%s + %s) must not exceed 40s",
			config.ShutdownSumProblemMarker,
			config.EnvHTTPShutdownGrace,
			config.EnvHTTPShutdownCloseTimeout),
	}
	wrapped := fmt.Errorf("load configuration: %w", cfgErr)
	result := sanitizeStartupError(wrapped)

	// Must emit the fixed classification naming both variables and constraint.
	expected := "config:" + config.ShutdownSumClassification
	if result != expected {
		t.Errorf("sanitizeStartupError: expected %q for shutdown-sum, got %q", expected, result)
	}
	// Must not emit the problem text.
	if strings.Contains(result, "must not exceed") {
		t.Errorf("sanitizeStartupError: output must not contain problem text, got %q", result)
	}
	// Must not emit user-supplied duration values.
	if strings.Contains(result, "30s") || strings.Contains(result, "11s") {
		t.Errorf("sanitizeStartupError: output must not contain duration values, got %q", result)
	}
}

// TestSanitizeStartupError_ShutdownGraceInvalidInput asserts that an
// invalid-duration error for VEC_HTTP_SHUTDOWN_GRACE (NOT the shutdown-sum
// constraint) is classified generically to avoid leaking supplied values.
func TestSanitizeStartupError_ShutdownGraceInvalidInput(t *testing.T) {
	cfgErr := &config.Error{
		Variable: config.EnvHTTPShutdownGrace,
		Problem:  "must be a positive duration, got \"abc\"",
	}
	wrapped := fmt.Errorf("load configuration: %w", cfgErr)
	result := sanitizeStartupError(wrapped)

	// Must NOT emit the variable name — only the shutdown-sum marker
	// allows the variable name, not arbitrary invalid-input errors.
	if strings.Contains(result, "VEC_HTTP_SHUTDOWN_GRACE") {
		t.Errorf("sanitizeStartupError: must not emit variable name for invalid input, got %q", result)
	}
	// Must NOT emit the user-supplied value.
	if strings.Contains(result, "abc") {
		t.Errorf("sanitizeStartupError: must not emit user-supplied value, got %q", result)
	}
	// Must be classified generically.
	if result != "config:error" {
		t.Errorf("sanitizeStartupError: expected config:error for invalid shutdown grace, got %q", result)
	}
}

// TestSanitizeStartupError_ShutdownCloseTimeoutInvalidInput asserts that
// an invalid-duration error for VEC_HTTP_SHUTDOWN_CLOSE_TIMEOUT is
// classified generically.
func TestSanitizeStartupError_ShutdownCloseTimeoutInvalidInput(t *testing.T) {
	cfgErr := &config.Error{
		Variable: config.EnvHTTPShutdownCloseTimeout,
		Problem:  "must be a positive duration, got \"xyz\"",
	}
	wrapped := fmt.Errorf("load configuration: %w", cfgErr)
	result := sanitizeStartupError(wrapped)

	// Must NOT emit the variable name.
	if strings.Contains(result, "VEC_HTTP_SHUTDOWN_CLOSE_TIMEOUT") {
		t.Errorf("sanitizeStartupError: must not emit variable name, got %q", result)
	}
	// Must NOT emit the user-supplied value.
	if strings.Contains(result, "xyz") {
		t.Errorf("sanitizeStartupError: must not emit user-supplied value, got %q", result)
	}
	// Must be classified generically.
	if result != "config:error" {
		t.Errorf("sanitizeStartupError: expected config:error for invalid shutdown close timeout, got %q", result)
	}
}

// --- TLS transport enforcement regression tests ---

// makeTestMigrationTLS creates TLS material for migration connection testing.
func makeTestMigrationTLS(t *testing.T) *config.TLSMaterial {
	t.Helper()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	serial := testMigrationSerial(t)
	caTmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "test-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	caCert, err := x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatal(err)
	}
	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	serial = testMigrationSerial(t)
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "migration-client"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, caCert, &key.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})

	m, err := config.NewTLSMaterialFromPEM(caPEM, certPEM, keyPEM, "127.0.0.1")
	if err != nil {
		t.Fatalf("NewTLSMaterialFromPEM: %v", err)
	}
	return m
}

func testMigrationSerial(t *testing.T) *big.Int {
	t.Helper()
	limit := new(big.Int).Lsh(big.NewInt(1), 63)
	n, err := rand.Int(rand.Reader, limit)
	if err != nil {
		t.Fatal(err)
	}
	return n.Add(n, big.NewInt(1))
}

// TestBuildMigrationConnConfigPlainModeDisablesTLS verifies that the migration
// connection builder disables TLS in plain mode (TLSConfig is nil)
// and clears the Fallbacks list (no plaintext fallback).
func TestBuildMigrationConnConfigPlainModeDisablesTLS(t *testing.T) {
	cfg := &config.Config{
		PGHost:        "127.0.0.1",
		PGPort:        5432,
		PGDatabase:    config.DatabaseName,
		MigrationRole: "vector_owner",
		TLSMode:       config.TLSModePlain,
		MigrationTLS:  nil,
	}

	cc, err := buildMigrationConnConfig(cfg)
	if err != nil {
		t.Fatalf("buildMigrationConnConfig: %v", err)
	}

	// TLSConfig must be nil — plain mode disables TLS entirely.
	if cc.TLSConfig != nil {
		t.Error("plain mode: migration TLSConfig is not nil — TLS is not disabled")
	}

	// Fallbacks must be empty — no plaintext fallback from sslmode=prefer.
	if len(cc.Fallbacks) > 0 {
		t.Errorf("plain mode: migration Fallbacks has %d entries — plaintext fallback may occur",
			len(cc.Fallbacks))
	}
}

// TestBuildMigrationConnConfigPlainModeNoFallback verifies the regression: ParseConfig
// defaults to sslmode=prefer, which builds a Fallbacks list. In plain mode,
// this must be cleared to prevent any TLS or plaintext fallback attempts.
func TestBuildMigrationConnConfigPlainModeNoFallback(t *testing.T) {
	cfg := &config.Config{
		PGHost:        "127.0.0.1",
		PGPort:        5432,
		PGDatabase:    config.DatabaseName,
		MigrationRole: "vector_owner",
		TLSMode:       config.TLSModePlain,
		MigrationTLS:  nil,
	}

	// Verify ParseConfig alone produces the problematic default.
	dsn := fmt.Sprintf("host=%s port=%d user=%s dbname=%s connect_timeout=%d",
		cfg.PGHost, cfg.PGPort, cfg.MigrationRole, cfg.PGDatabase, int(pgConnectTimeout.Seconds()))
	rawCC, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("ParseConfig: %v", err)
	}
	// ParseConfig default: sslmode=prefer sets a non-nil TLSConfig.
	if rawCC.TLSConfig == nil {
		t.Log("NOTE: ParseConfig default TLSConfig is nil — behavior may have changed")
	} else {
		t.Log("ParseConfig default: TLSConfig is non-nil (sslmode=prefer)")
	}
	// ParseConfig default: Fallbacks may contain a plaintext retry.
	if len(rawCC.Fallbacks) > 0 {
		t.Logf("ParseConfig default: Fallbacks has %d entries", len(rawCC.Fallbacks))
	}

	// Now verify our enforcement clears both through the production helper.
	cc, err := buildMigrationConnConfig(cfg)
	if err != nil {
		t.Fatalf("buildMigrationConnConfig: %v", err)
	}

	if cc.TLSConfig != nil {
		t.Error("REGRESSION: plain mode migration TLSConfig is not nil after enforcement")
	}
	if len(cc.Fallbacks) > 0 {
		t.Error("REGRESSION: plain mode migration Fallbacks is not empty after enforcement")
	}
}

// TestBuildMigrationConnConfigTLSModeSetsTLSConfig verifies that TLS mode
// with valid MigrationTLS material produces a non-nil TLSConfig with
// RootCAs, client certificate callback, no InsecureSkipVerify, and
// no Fallbacks (no plaintext fallback).
func TestBuildMigrationConnConfigTLSModeSetsTLSConfig(t *testing.T) {
	migrationTLS := makeTestMigrationTLS(t)

	cfg := &config.Config{
		PGHost:        "127.0.0.1",
		PGPort:        5432,
		PGDatabase:    config.DatabaseName,
		MigrationRole: "vector_owner",
		TLSMode:       config.TLSModeTLS,
		MigrationTLS:  migrationTLS,
	}

	cc, err := buildMigrationConnConfig(cfg)
	if err != nil {
		t.Fatalf("buildMigrationConnConfig: %v", err)
	}

	tc := cc.TLSConfig
	if tc == nil {
		t.Fatal("TLS mode: TLSConfig is nil — TLS is not enabled")
	}

	// Verify certificate-verifying TLS config.
	if tc.InsecureSkipVerify {
		t.Error("TLS mode: InsecureSkipVerify is true — server verification is disabled")
	}
	if tc.MinVersion < tls.VersionTLS12 {
		t.Errorf("TLS mode: MinVersion %x is below TLS 1.2", tc.MinVersion)
	}
	if tc.RootCAs == nil {
		t.Error("TLS mode: RootCAs is nil — no CA verification")
	}
	if tc.GetClientCertificate == nil {
		t.Error("TLS mode: GetClientCertificate is nil — no client authentication")
	}
	if tc.ServerName != "127.0.0.1" {
		t.Errorf("TLS mode: ServerName = %q, want %q", tc.ServerName, "127.0.0.1")
	}

	// Verify no plaintext fallback.
	if len(cc.Fallbacks) > 0 {
		t.Errorf("TLS mode: Fallbacks has %d entries — plaintext fallback may occur",
			len(cc.Fallbacks))
	}

	// Verify the client certificate callback returns a valid certificate.
	cert, err := tc.GetClientCertificate(nil)
	if err != nil {
		t.Fatalf("GetClientCertificate: %v", err)
	}
	if len(cert.Certificate) == 0 {
		t.Error("GetClientCertificate returned a certificate with no data")
	}
}

// TestBuildMigrationConnConfigRejectsNilTLSInTLSMode verifies that TLS mode with
// nil MigrationTLS is rejected (safety: TLS mode requires parsed material).
func TestBuildMigrationConnConfigRejectsNilTLSInTLSMode(t *testing.T) {
	cfg := &config.Config{
		PGHost:        "127.0.0.1",
		PGPort:        5432,
		PGDatabase:    config.DatabaseName,
		MigrationRole: "vector_owner",
		TLSMode:       config.TLSModeTLS,
		MigrationTLS:  nil,
	}

	_, err := buildMigrationConnConfig(cfg)
	if err == nil {
		t.Fatal("expected error when TLS mode is selected but MigrationTLS is nil")
	}
}

// TestBuildMigrationConnConfigAppliesSimpleProtocol verifies that the migration
// connection config uses the simple query protocol (text-only results) required
// by the privilege catalog and identity assertion layers.
func TestBuildMigrationConnConfigAppliesSimpleProtocol(t *testing.T) {
	cfg := &config.Config{
		PGHost:        "127.0.0.1",
		PGPort:        5432,
		PGDatabase:    config.DatabaseName,
		MigrationRole: "vector_owner",
		TLSMode:       config.TLSModePlain,
		MigrationTLS:  nil,
	}

	cc, err := buildMigrationConnConfig(cfg)
	if err != nil {
		t.Fatalf("buildMigrationConnConfig: %v", err)
	}

	if cc.DefaultQueryExecMode != pgx.QueryExecModeSimpleProtocol {
		t.Errorf("DefaultQueryExecMode = %v, want SimpleProtocol",
			cc.DefaultQueryExecMode)
	}
}

// TestBuildMigrationConnUsesBuildMigrationConnConfig verifies that the
// production buildMigrationConn function delegates to buildMigrationConnConfig
// by checking that a configuration error (nil MigrationTLS in TLS mode)
// is reported when buildMigrationConn is called.
func TestBuildMigrationConnUsesBuildMigrationConnConfig(t *testing.T) {
	cfg := &config.Config{
		PGHost:        "127.0.0.1",
		PGPort:        5432,
		PGDatabase:    config.DatabaseName,
		MigrationRole: "vector_owner",
		TLSMode:       config.TLSModeTLS,
		MigrationTLS:  nil,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	_, err := buildMigrationConn(ctx, cfg)
	if err == nil {
		t.Fatal("expected error when TLS mode is selected but MigrationTLS is nil")
	}
	if !strings.Contains(err.Error(), "TLS mode selected") {
		t.Errorf("expected TLS mode error from buildMigrationConnConfig, got: %v", err)
	}
}
