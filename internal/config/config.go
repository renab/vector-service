// Package config loads and validates the Vector Service's environment
// configuration: the VEC_* variables of the configuration reference in
// docs/implementation/README.md.
//
// It is the only place VEC_* variables are read. Load performs every
// validation the startup requires — including parsing TLS certificate,
// key, and CA material from disk — before it returns, so a successful
// Load is the gate that permits any network activity: a configuration
// failure is always a startup failure, before any port is opened, and no
// insecure default is substituted for a missing or invalid value.
//
// Load never logs, and never embeds in errors, secret material: TLS paths
// are named, certificate material is at most counted, and the admin token
// is never echoed.
package config

import (
	"fmt"
	"log/slog"
	"net"
	"os"
	"strconv"
	"strings"
	"time"
)

// RuntimeRole is the fixed runtime database role. Migration 0004 binds the
// runtime grant set to this role by name; VEC_PG_USER may only ever
// restate it.
const RuntimeRole = "vector_api"

// DatabaseName is the fixed database name. Migration 0004 hardcodes the
// database name in its CONNECT grant; VEC_PG_DATABASE may only ever
// restate it.
const DatabaseName = "vector"

// Variable names of the configuration reference.
const (
	EnvListenAddr               = "VEC_LISTEN_ADDR"
	EnvPGHost                   = "VEC_PG_HOST"
	EnvPGPort                   = "VEC_PG_PORT"
	EnvPGDatabase               = "VEC_PG_DATABASE"
	EnvPGURL                    = "VEC_PG_URL"
	EnvPGUser                   = "VEC_PG_USER"
	EnvPGMigrationUser          = "VEC_PG_MIGRATION_USER"
	EnvPGTLSMode                = "VEC_PG_TLS_MODE"
	EnvPGCACert                 = "VEC_PG_CA_CERT"
	EnvPGClientCert             = "VEC_PG_CLIENT_CERT"
	EnvPGClientKey              = "VEC_PG_CLIENT_KEY"
	EnvPGMigrationCACert        = "VEC_PG_MIGRATION_CA_CERT"
	EnvPGMigrationClientCert    = "VEC_PG_MIGRATION_CLIENT_CERT"
	EnvPGMigrationClientKey     = "VEC_PG_MIGRATION_CLIENT_KEY"
	EnvPGTLSServerName          = "VEC_PG_TLS_SERVER_NAME"
	EnvAdminToken               = "VEC_ADMIN_TOKEN"
	EnvHTTPMaxBodyBytes         = "VEC_HTTP_MAX_BODY_BYTES"
	EnvHTTPRequestTimeout       = "VEC_HTTP_REQUEST_TIMEOUT"
	EnvHTTPShutdownGrace        = "VEC_HTTP_SHUTDOWN_GRACE"
	EnvHTTPShutdownCloseTimeout = "VEC_HTTP_SHUTDOWN_CLOSE_TIMEOUT"
	EnvUpsertMaxRecords         = "VEC_UPSERT_MAX_RECORDS"
	EnvSearchMaxLimit           = "VEC_SEARCH_MAX_LIMIT"
	EnvMaxFilters               = "VEC_MAX_FILTERS"
	EnvMaxFilterValues          = "VEC_MAX_FILTER_VALUES"
	EnvMaxMetadataBytes         = "VEC_MAX_METADATA_BYTES"
	EnvMigrationLockWait        = "VEC_MIGRATION_LOCK_WAIT"
	EnvLogLevel                 = "VEC_LOG_LEVEL"
	EnvLogFormat                = "VEC_LOG_FORMAT"
)

// TLSMode is the transport mode for both PostgreSQL identities. It is the
// single source of transport truth.
type TLSMode string

// Transport modes.
const (
	// TLSModeTLS is client-certificate authentication with full server
	// verification: the production transport.
	TLSModeTLS TLSMode = "tls"
	// TLSModePlain is explicit development and testing only. It is never
	// the default, never implicit, and forbids every TLS variable.
	TLSModePlain TLSMode = "plain"
)

// Pool holds the explicit runtime-pool parameters. The configuration
// reference defines no VEC_* variables for them; they are fixed explicit
// values (not pgxpool's implicit defaults), documented here, and part of
// the package-1 startup summary.
type Pool struct {
	// MaxConns bounds the pool. pgxpool's own default is the greater of 4
	// and the host's CPU count, which is unbounded on large hosts; the
	// workload is itself bounded (batch sizes, request deadline), so the
	// pool is bounded explicitly.
	MaxConns int32
	// MinConns is the minimum pool size. Zero: connections are opened on
	// demand; readiness probes drive the first open.
	MinConns int32
	// MaxConnLifetime bounds session age. Session-persistent state is
	// forbidden on pooled connections (overview invariant 3), so a bounded
	// lifetime bounds the exposure of any leaked session state and turns
	// over connections for certificate rotation.
	MaxConnLifetime time.Duration
	// MaxConnLifetimeJitter staggers lifetime recycles.
	MaxConnLifetimeJitter time.Duration
	// MaxConnIdleTime closes idle connections.
	MaxConnIdleTime time.Duration
	// HealthCheckPeriod is the interval at which the pool probes idle
	// connections; it bounds how fast /readyz reflects a database failure.
	HealthCheckPeriod time.Duration
}

// Config is the fully validated service configuration. Every field is
// explicitly set by Load; zero values never mean "unconfigured default".
type Config struct {
	// ListenAddr is the HTTP listen address (host:port).
	ListenAddr string

	// PostgreSQL endpoint, resolved from either VEC_PG_URL or the
	// VEC_PG_HOST/VEC_PG_PORT/VEC_PG_DATABASE settings (never both).
	PGHost     string
	PGPort     int
	PGDatabase string

	// RuntimeRole is exactly RuntimeRole; MigrationRole is the distinct
	// privileged migration identity (database owner).
	RuntimeRole   string
	MigrationRole string

	// TLSMode is the transport for both identities.
	TLSMode TLSMode
	// ServerName is the hostname used for server-certificate validation in
	// TLS mode (defaults to the PostgreSQL host). It is empty in plain
	// mode.
	ServerName string
	// RuntimeTLS and MigrationTLS hold the parsed TLS material for each
	// identity; both are nil in plain mode.
	RuntimeTLS   *TLSMaterial
	MigrationTLS *TLSMaterial

	// AdminToken is the high-entropy admin-plane bearer token. It is
	// secret: never log it, never include it in an error or log line.
	AdminToken string

	// HTTP bounds.
	HTTPMaxBodyBytes         int
	HTTPRequestTimeout       time.Duration
	HTTPShutdownGrace        time.Duration
	HTTPShutdownCloseTimeout time.Duration

	// The four http.Server connection timeouts. Package 4 builds the
	// server from these; the unconfigured default server is never used in
	// production. The reference table defines no VEC_* variables for
	// them, so they are fixed explicit values.
	ServerReadHeaderTimeout time.Duration
	ServerReadTimeout       time.Duration
	ServerWriteTimeout      time.Duration
	ServerIdleTimeout       time.Duration

	// Operation bounds.
	UpsertMaxRecords int
	SearchMaxLimit   int
	MaxFilters       int
	MaxFilterValues  int
	MaxMetadataBytes int

	// MigrationLockWait bounds the wait for the migration advisory lock.
	MigrationLockWait time.Duration

	// Logging.
	LogLevel  slog.Level
	LogFormat string

	// Pool is the explicit runtime-pool parameter set.
	Pool Pool
}

// Defaults of the configuration reference.
const (
	DefaultListenAddr               = "127.0.0.1:8080"
	DefaultPGPort                   = 5432
	DefaultHTTPMaxBodyBytes         = 16 * 1024 * 1024 // 16 MiB
	DefaultHTTPRequestTimeout       = 30 * time.Second
	DefaultHTTPShutdownGrace        = 30 * time.Second
	DefaultHTTPShutdownCloseTimeout = 10 * time.Second
	DefaultUpsertMaxRecords         = 100
	DefaultSearchMaxLimit           = 200 // API hard ceiling
	DefaultMaxFilters               = 10
	DefaultMaxFilterValues          = 50
	DefaultMaxMetadataBytes         = 16 * 1024 // 16 KiB
	DefaultMigrationLockWait        = 300 * time.Second
)

// Fixed explicit values for the four http.Server connection timeouts.
const (
	DefaultServerReadHeaderTimeout = 5 * time.Second
	DefaultServerReadTimeout       = 60 * time.Second
	DefaultServerWriteTimeout      = 60 * time.Second
	DefaultServerIdleTimeout       = 2 * time.Minute
)

// Fixed explicit values for the runtime pool.
const (
	DefaultPoolMaxConns              = int32(10)
	DefaultPoolMinConns              = int32(0)
	DefaultPoolMaxConnLifetime       = 30 * time.Minute
	DefaultPoolMaxConnLifetimeJitter = 5 * time.Minute
	DefaultPoolMaxConnIdleTime       = 5 * time.Minute
	DefaultPoolHealthCheckPeriod     = 30 * time.Second
)

// Error is a configuration failure. Variable is the VEC_* variable that
// failed ("" for cross-variable failures); Problem is a stable,
// secret-free description. Callers can type-assert to this type to obtain
// the variable name.
type Error struct {
	Variable string
	Problem  string
}

func (e *Error) Error() string {
	if e.Variable == "" {
		return "configuration: " + e.Problem
	}
	return "configuration: " + e.Variable + ": " + e.Problem
}

func fail(variable, problem string) error {
	return &Error{Variable: variable, Problem: problem}
}

// safeToLogVariables is the allowlist of configuration variables whose
// errors are safe to name in log output. These are variables whose names
// are server-authored identifiers (not user-supplied content) and whose
// failure is an operational concern that operators need to identify from
// container logs. The variable name itself is not a secret; it is part of
// the documented configuration surface.
//
// Variables not on this list (e.g., TLS paths, values that may contain
// user-supplied content) are classified generically to avoid leaking
// information.
//
// NOTE: EnvHTTPShutdownGrace and EnvHTTPShutdownCloseTimeout are NOT on
// this list. The shutdown-sum bound violation has its own special
// classification (see ShutdownSumProblemMarker) because the variable name
// is a server-authored constraint identifier. Individual parsing errors
// for these variables are classified generically to avoid leaking supplied
// values.
var safeToLogVariables = map[string]struct{}{
	EnvPGHost:          {},
	EnvPGMigrationUser: {},
	EnvAdminToken:      {},
	EnvPGURL:           {},
}

// ShutdownSumProblemMarker is a prefix of the Problem field for shutdown-sum
// bound violations. The error classifier uses it to safely identify this
// specific constraint violation and emit the server-authored classification
// rather than a generic classification.
const ShutdownSumProblemMarker = "shutdown timeout sum"

// ShutdownSumClassification is the fixed, value-free classification string
// emitted for shutdown-sum bound violations. It names both configuration
// variables and the 40-second constraint without including any user-supplied
// duration values.
const ShutdownSumClassification = "shutdown_sum_exceeds_40s:" + EnvHTTPShutdownGrace + "+" + EnvHTTPShutdownCloseTimeout

// IsSafeToLog reports whether the given configuration variable name is
// safe to emit in log output as an identifier. Only allowlisted
// missing-required variables return true.
func IsSafeToLog(variable string) bool {
	_, ok := safeToLogVariables[variable]
	return ok
}

// Lookup returns the value of an environment variable. os.LookupEnv is the
// production implementation.
type Lookup func(key string) (string, bool)

// Load reads and validates the complete configuration from the process
// environment. It performs no network activity.
func Load() (*Config, error) {
	return load(os.LookupEnv)
}

func load(lookup Lookup) (*Config, error) {
	cfg := &Config{
		ListenAddr:               DefaultListenAddr,
		PGPort:                   DefaultPGPort,
		PGDatabase:               DatabaseName,
		RuntimeRole:              RuntimeRole,
		TLSMode:                  TLSModeTLS,
		HTTPMaxBodyBytes:         DefaultHTTPMaxBodyBytes,
		HTTPRequestTimeout:       DefaultHTTPRequestTimeout,
		HTTPShutdownGrace:        DefaultHTTPShutdownGrace,
		HTTPShutdownCloseTimeout: DefaultHTTPShutdownCloseTimeout,
		ServerReadHeaderTimeout:  DefaultServerReadHeaderTimeout,
		ServerReadTimeout:        DefaultServerReadTimeout,
		ServerWriteTimeout:       DefaultServerWriteTimeout,
		ServerIdleTimeout:        DefaultServerIdleTimeout,
		UpsertMaxRecords:         DefaultUpsertMaxRecords,
		SearchMaxLimit:           DefaultSearchMaxLimit,
		MaxFilters:               DefaultMaxFilters,
		MaxFilterValues:          DefaultMaxFilterValues,
		MaxMetadataBytes:         DefaultMaxMetadataBytes,
		MigrationLockWait:        DefaultMigrationLockWait,
		LogLevel:                 slog.LevelInfo,
		LogFormat:                "json",
		Pool: Pool{
			MaxConns:              DefaultPoolMaxConns,
			MinConns:              DefaultPoolMinConns,
			MaxConnLifetime:       DefaultPoolMaxConnLifetime,
			MaxConnLifetimeJitter: DefaultPoolMaxConnLifetimeJitter,
			MaxConnIdleTime:       DefaultPoolMaxConnIdleTime,
			HealthCheckPeriod:     DefaultPoolHealthCheckPeriod,
		},
	}

	steps := []struct {
		name string
		fn   func(*Config, Lookup) error
	}{
		{"listen address", (*Config).loadListenAddr},
		{"TLS mode", (*Config).loadTLSMode},
		{"roles", (*Config).loadRoles},
		{"PostgreSQL endpoint", (*Config).loadEndpoint},
		{"TLS material", (*Config).loadTLS},
		{"admin token", (*Config).loadAdminToken},
		{"HTTP timeouts", (*Config).loadHTTPTimeouts},
		{"bounds", (*Config).loadBounds},
		{"logging", (*Config).loadLogging},
	}
	for _, step := range steps {
		if err := step.fn(cfg, lookup); err != nil {
			return nil, err
		}
	}
	return cfg, nil
}

func (c *Config) loadListenAddr(lookup Lookup) error {
	v, ok := lookup(EnvListenAddr)
	if !ok {
		return nil
	}
	if v == "" {
		return fail(EnvListenAddr, "must not be empty")
	}
	host, port, err := net.SplitHostPort(v)
	if err != nil {
		return fail(EnvListenAddr, fmt.Sprintf("must be host:port, got %q", v))
	}
	if host == "" {
		return fail(EnvListenAddr, fmt.Sprintf("missing host in listen address %q", v))
	}
	if _, err := strconv.ParseUint(port, 10, 16); err != nil || port == "0" {
		return fail(EnvListenAddr, fmt.Sprintf("port must be in 1..65535, got %q", port))
	}
	c.ListenAddr = v
	return nil
}

func (c *Config) loadTLSMode(lookup Lookup) error {
	v, ok := lookup(EnvPGTLSMode)
	if !ok {
		return nil
	}
	if v == "" {
		return fail(EnvPGTLSMode, "must not be empty")
	}
	switch TLSMode(v) {
	case TLSModeTLS, TLSModePlain:
		c.TLSMode = TLSMode(v)
	default:
		return fail(EnvPGTLSMode,
			fmt.Sprintf("must be %q or %q, got %q", TLSModeTLS, TLSModePlain, v))
	}
	return nil
}

func (c *Config) loadRoles(lookup Lookup) error {
	if v, ok := lookup(EnvPGUser); ok {
		if v == "" {
			return fail(EnvPGUser, "must not be empty")
		}
		if v != RuntimeRole {
			return fail(EnvPGUser,
				fmt.Sprintf("must equal exactly %q; the runtime role is fixed", RuntimeRole))
		}
	}
	mig, ok := lookup(EnvPGMigrationUser)
	if !ok {
		return fail(EnvPGMigrationUser, "is required")
	}
	if mig == "" {
		return fail(EnvPGMigrationUser, "must not be empty")
	}
	if mig == c.RuntimeRole {
		return fail(EnvPGMigrationUser,
			fmt.Sprintf("must differ from the runtime role %q (identity collapse)", c.RuntimeRole))
	}
	c.MigrationRole = mig
	return nil
}

// loadEndpoint resolves the PostgreSQL endpoint from either VEC_PG_URL or
// the individual VEC_PG_HOST/VEC_PG_PORT/VEC_PG_DATABASE settings. The two
// forms are alternatives; mixing them is a startup error.
func (c *Config) loadEndpoint(lookup Lookup) error {
	_, urlSet := lookup(EnvPGURL)
	_, hostSet := lookup(EnvPGHost)
	_, portSet := lookup(EnvPGPort)
	_, dbSet := lookup(EnvPGDatabase)

	if urlSet {
		var conflicts []string
		if hostSet {
			conflicts = append(conflicts, EnvPGHost)
		}
		if portSet {
			conflicts = append(conflicts, EnvPGPort)
		}
		if dbSet {
			conflicts = append(conflicts, EnvPGDatabase)
		}
		if len(conflicts) > 0 {
			return fail(EnvPGURL,
				"conflicts with the individual connection settings: "+strings.Join(conflicts, ", "))
		}
		return c.loadEndpointFromURL(lookup)
	}

	host, ok := lookup(EnvPGHost)
	if !ok {
		return fail(EnvPGHost, "is required (or set "+EnvPGURL+")")
	}
	if host == "" {
		return fail(EnvPGHost, "must not be empty")
	}
	c.PGHost = host

	if v, ok := lookup(EnvPGPort); ok {
		if v == "" {
			return fail(EnvPGPort, "must not be empty")
		}
		port, err := strconv.ParseUint(v, 10, 16)
		if err != nil || port == 0 || port > 65535 {
			return fail(EnvPGPort, fmt.Sprintf("must be a port in 1..65535, got %q", v))
		}
		c.PGPort = int(port)
	}

	if v, ok := lookup(EnvPGDatabase); ok {
		if v == "" {
			return fail(EnvPGDatabase, "must not be empty")
		}
		if v != DatabaseName {
			return fail(EnvPGDatabase,
				fmt.Sprintf("must equal exactly %q; migration 0004 hardcodes the database name", DatabaseName))
		}
	}
	return nil
}

// loadEndpointFromURL resolves the endpoint from VEC_PG_URL. The URL is a
// postgres:// or postgresql:// reference to host:port/database only: it
// must not contain credentials (the roles are configured separately and
// there is no password transport), and it must not carry any query
// parameter (TLS mode is the single source of transport truth, and every
// bound is explicit configuration, not a URL option).
func (c *Config) loadEndpointFromURL(lookup Lookup) error {
	raw, _ := lookup(EnvPGURL)
	if raw == "" {
		return fail(EnvPGURL, "must not be empty")
	}
	u, err := parseURL(raw)
	if err != nil {
		return fail(EnvPGURL, err.Error())
	}
	if u.Scheme != "postgres" && u.Scheme != "postgresql" {
		return fail(EnvPGURL,
			"scheme must be postgres or postgresql, got "+strconv.Quote(u.Scheme))
	}
	if u.Host == "" {
		return fail(EnvPGURL, "missing host")
	}
	if u.User != nil {
		return fail(EnvPGURL,
			"must not contain a user or password; roles are configured separately and there is no password transport")
	}
	if n := len(u.Query()); n > 0 {
		return fail(EnvPGURL,
			fmt.Sprintf("must not carry query parameters (%d present); TLS mode is the single source of transport truth and all bounds are explicit configuration", n))
	}

	host := u.Hostname()
	if host == "" {
		return fail(EnvPGURL, "missing host")
	}
	c.PGHost = host

	port := DefaultPGPort
	if p := u.Port(); p != "" {
		n, err := strconv.ParseUint(p, 10, 16)
		if err != nil || n == 0 || n > 65535 {
			return fail(EnvPGURL, fmt.Sprintf("port must be in 1..65535, got %q", p))
		}
		port = int(n)
	}
	c.PGPort = port

	db := strings.TrimPrefix(u.Path, "/")
	if db == "" {
		db = DatabaseName
	}
	if db != DatabaseName {
		return fail(EnvPGURL,
			fmt.Sprintf("database must be %q; migration 0004 hardcodes the database name", DatabaseName))
	}
	c.PGDatabase = db
	return nil
}

// loadTLS validates the TLS variables against the declared mode and parses
// the material. TLS mode requires the complete runtime triple (CA, client
// certificate, client key) and the complete migration pair (certificate,
// key; CA defaulting to the runtime CA). Plain mode forbids every TLS
// variable. A partial triple in any mode is a startup error.
func (c *Config) loadTLS(lookup Lookup) error {
	tlsVars := []string{
		EnvPGCACert,
		EnvPGClientCert, EnvPGClientKey,
		EnvPGMigrationCACert,
		EnvPGMigrationClientCert, EnvPGMigrationClientKey,
		EnvPGTLSServerName,
	}

	if c.TLSMode == TLSModePlain {
		for _, name := range tlsVars {
			v, ok := lookup(name)
			if !ok {
				continue
			}
			if v == "" {
				return fail(name, "must not be empty")
			}
			return fail(name, "must not be set in plain mode; TLS mode is the single source of transport truth")
		}
		return nil
	}

	vals := make(map[string]string, len(tlsVars))
	for _, name := range tlsVars {
		v, ok := lookup(name)
		if !ok {
			continue
		}
		if v == "" {
			return fail(name, "must not be empty")
		}
		vals[name] = v
	}

	// Complete triples: every member of each identity's material is
	// required in TLS mode. A missing member of a partially set pair is a
	// partial triple; either way the first missing member is named.
	required := []string{
		EnvPGCACert,
		EnvPGClientCert, EnvPGClientKey,
		EnvPGMigrationClientCert, EnvPGMigrationClientKey,
	}
	for _, name := range required {
		if _, ok := vals[name]; !ok {
			if name == EnvPGClientCert || name == EnvPGClientKey {
				return fail(name, "is required in TLS mode (partial runtime client triple)")
			}
			if name == EnvPGMigrationClientCert || name == EnvPGMigrationClientKey {
				return fail(name, "is required in TLS mode (partial migration client triple)")
			}
			return fail(name, "is required in TLS mode")
		}
	}

	migCAVar := EnvPGMigrationCACert
	migCA := vals[EnvPGMigrationCACert]
	if migCA == "" {
		migCA = vals[EnvPGCACert] // migration CA defaults to the runtime CA
		migCAVar = EnvPGCACert
	}

	serverName := c.PGHost
	if v, ok := vals[EnvPGTLSServerName]; ok {
		serverName = v
	}

	runtime, err := parseTLSMaterial(
		EnvPGCACert, vals[EnvPGCACert],
		EnvPGClientCert, vals[EnvPGClientCert],
		EnvPGClientKey, vals[EnvPGClientKey],
		serverName,
	)
	if err != nil {
		return err
	}
	c.RuntimeTLS = runtime

	migration, err := parseTLSMaterial(
		migCAVar, migCA,
		EnvPGMigrationClientCert, vals[EnvPGMigrationClientCert],
		EnvPGMigrationClientKey, vals[EnvPGMigrationClientKey],
		serverName,
	)
	if err != nil {
		return err
	}
	c.MigrationTLS = migration

	// The two identities never share transport credentials: the migration
	// client pair must be distinct from the runtime pair.
	if sameClientCertificate(runtime, migration) {
		return fail(EnvPGMigrationClientCert,
			"must be a certificate distinct from the runtime client certificate; the two identities never share transport credentials")
	}

	c.ServerName = serverName
	return nil
}

func (c *Config) loadAdminToken(lookup Lookup) error {
	v, ok := lookup(EnvAdminToken)
	if !ok {
		return fail(EnvAdminToken, "is required")
	}
	if v == "" {
		return fail(EnvAdminToken, "must not be empty")
	}
	const minAdminTokenBytes = 32
	if len(v) < minAdminTokenBytes {
		// The length bound is a structural check for a machine-generated
		// high-entropy token; the value itself is never echoed.
		return fail(EnvAdminToken,
			fmt.Sprintf("must be at least %d bytes (high-entropy machine token)", minAdminTokenBytes))
	}
	c.AdminToken = v
	return nil
}

// maxShutdownSum is the maximum allowed sum of HTTPShutdownGrace and
// HTTPShutdownCloseTimeout. The sum determines worst-case stop time;
// exceeding the orchestrator's stop budget causes SIGKILL, defeating
// bounded graceful shutdown.
const maxShutdownSum = 40 * time.Second

func (c *Config) loadHTTPTimeouts(lookup Lookup) error {
	if v, ok := lookup(EnvHTTPMaxBodyBytes); ok {
		if v == "" {
			return fail(EnvHTTPMaxBodyBytes, "must not be empty")
		}
		n, err := strconv.ParseUint(v, 10, 63)
		if err != nil || n == 0 {
			return fail(EnvHTTPMaxBodyBytes, fmt.Sprintf("must be a positive integer byte count, got %q", v))
		}
		c.HTTPMaxBodyBytes = int(n)
	}
	durs := []struct {
		name  string
		value *time.Duration
	}{
		{EnvHTTPRequestTimeout, &c.HTTPRequestTimeout},
		{EnvHTTPShutdownGrace, &c.HTTPShutdownGrace},
		{EnvHTTPShutdownCloseTimeout, &c.HTTPShutdownCloseTimeout},
	}
	for _, d := range durs {
		v, ok := lookup(d.name)
		if !ok {
			continue
		}
		if v == "" {
			return fail(d.name, "must not be empty")
		}
		dur, err := time.ParseDuration(v)
		if err != nil || dur <= 0 {
			return fail(d.name, fmt.Sprintf("must be a positive duration, got %q", v))
		}
		*d.value = dur
	}

	// Validate the combined shutdown timeout sum does not exceed the bound.
	// The sum determines worst-case stop time and must fit within the
	// orchestrator's stop budget.
	//
	// Use subtraction instead of addition to avoid overflow when both
	// durations are near math.MaxInt64: grace + close > maxShutdownSum
	// rewrites to grace > maxShutdownSum - close, which is safe because
	// both values are already validated as positive.
	if c.HTTPShutdownGrace > maxShutdownSum-c.HTTPShutdownCloseTimeout {
		return fail(EnvHTTPShutdownGrace,
			fmt.Sprintf("%s (%s + %s) must not exceed %s",
				ShutdownSumProblemMarker, EnvHTTPShutdownGrace, EnvHTTPShutdownCloseTimeout, maxShutdownSum))
	}
	return nil
}

func (c *Config) loadBounds(lookup Lookup) error {
	ints := []struct {
		name string
		max  int // 0 means no upper bound
		out  *int
	}{
		{EnvUpsertMaxRecords, 0, &c.UpsertMaxRecords},
		{EnvSearchMaxLimit, 200, &c.SearchMaxLimit}, // API hard ceiling
		{EnvMaxFilters, 0, &c.MaxFilters},
		{EnvMaxFilterValues, 0, &c.MaxFilterValues},
		{EnvMaxMetadataBytes, 0, &c.MaxMetadataBytes},
	}
	for _, f := range ints {
		v, ok := lookup(f.name)
		if !ok {
			continue
		}
		if v == "" {
			return fail(f.name, "must not be empty")
		}
		n, err := strconv.ParseInt(v, 10, 63)
		if err != nil || n <= 0 {
			return fail(f.name, fmt.Sprintf("must be a positive integer, got %q", v))
		}
		if f.max > 0 && n > int64(f.max) {
			return fail(f.name,
				fmt.Sprintf("must be at most %d (API hard ceiling), got %d", f.max, n))
		}
		*f.out = int(n)
	}

	if v, ok := lookup(EnvMigrationLockWait); ok {
		if v == "" {
			return fail(EnvMigrationLockWait, "must not be empty")
		}
		dur, err := time.ParseDuration(v)
		if err != nil || dur <= 0 {
			return fail(EnvMigrationLockWait, fmt.Sprintf("must be a positive duration, got %q", v))
		}
		c.MigrationLockWait = dur
	}
	return nil
}

func (c *Config) loadLogging(lookup Lookup) error {
	if v, ok := lookup(EnvLogLevel); ok {
		if v == "" {
			return fail(EnvLogLevel, "must not be empty")
		}
		var level slog.Level
		switch strings.ToLower(v) {
		case "debug":
			level = slog.LevelDebug
		case "info":
			level = slog.LevelInfo
		case "warn":
			level = slog.LevelWarn
		case "error":
			level = slog.LevelError
		default:
			return fail(EnvLogLevel,
				fmt.Sprintf("must be one of debug, info, warn, error, got %q", v))
		}
		c.LogLevel = level
	}
	if v, ok := lookup(EnvLogFormat); ok {
		if v == "" {
			return fail(EnvLogFormat, "must not be empty")
		}
		if v != "json" {
			return fail(EnvLogFormat, fmt.Sprintf("only %q is supported, got %q", "json", v))
		}
		c.LogFormat = "json"
	}
	return nil
}
