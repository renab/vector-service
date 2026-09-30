package config

import (
	"bytes"
	"crypto/tls"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func envLookup(env map[string]string) Lookup {
	return func(key string) (string, bool) {
		v, ok := env[key]
		return v, ok
	}
}

func baseEnv() map[string]string {
	return map[string]string{
		EnvPGHost:          "db.example.test",
		EnvPGMigrationUser: "vector_migrate",
		EnvAdminToken:      strings.Repeat("x", 48),
	}
}

func wantConfigError(t *testing.T, err error, variable string) {
	t.Helper()
	var e *Error
	if !errors.As(err, &e) {
		t.Fatalf("expected *config.Error, got %T: %v", err, err)
	}
	if e.Variable != variable {
		t.Fatalf("error variable = %q, want %q (problem: %s)", e.Variable, variable, e.Problem)
	}
}

type tlsFixture struct {
	vars      map[string]string
	runtime   *testLeaf
	migration *testLeaf
}

func setupTLSFixture(t *testing.T) *tlsFixture {
	t.Helper()
	ca, err := newTestCA("test-ca")
	if err != nil {
		t.Fatal(err)
	}
	rt, err := newTestLeaf("runtime-client", ca)
	if err != nil {
		t.Fatal(err)
	}
	mig, err := newTestLeaf("migration-client", ca)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	write := func(name string, data []byte) string {
		t.Helper()
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, data, 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	return &tlsFixture{
		vars: map[string]string{
			EnvPGCACert:              write("ca.pem", ca.pem),
			EnvPGClientCert:          write("runtime-cert.pem", rt.certPEM),
			EnvPGClientKey:           write("runtime-key.pem", rt.keyPEM),
			EnvPGMigrationClientCert: write("migration-cert.pem", mig.certPEM),
			EnvPGMigrationClientKey:  write("migration-key.pem", mig.keyPEM),
		},
		runtime:   rt,
		migration: mig,
	}
}

func TestLoadTLSMode(t *testing.T) {
	fx := setupTLSFixture(t)
	env := baseEnv()
	for k, v := range fx.vars {
		env[k] = v
	}
	cfg, err := load(envLookup(env))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.TLSMode != TLSModeTLS {
		t.Errorf("TLSMode = %q, want %q", cfg.TLSMode, TLSModeTLS)
	}
	if cfg.RuntimeTLS == nil || cfg.MigrationTLS == nil {
		t.Fatalf("expected both TLS materials to be parsed, got runtime=%v migration=%v", cfg.RuntimeTLS, cfg.MigrationTLS)
	}
	// Server name defaults to the PostgreSQL host.
	if cfg.ServerName != "db.example.test" {
		t.Errorf("ServerName = %q, want %q", cfg.ServerName, "db.example.test")
	}
	// Migration CA defaults to the runtime CA file.
	if cfg.MigrationTLS.CAFile != fx.vars[EnvPGCACert] {
		t.Errorf("migration CAFile = %q, want the runtime CA file", cfg.MigrationTLS.CAFile)
	}
	if cfg.RuntimeTLS.RootCAs() == nil || cfg.MigrationTLS.RootCAs() == nil {
		t.Error("expected parsed CA pools on both identities")
	}
}

func TestLoadTLSModeExplicitServerNameAndMigrationCA(t *testing.T) {
	fx := setupTLSFixture(t)
	ca2, err := newTestCA("test-ca-2")
	if err != nil {
		t.Fatal(err)
	}
	env := baseEnv()
	for k, v := range fx.vars {
		env[k] = v
	}
	env[EnvPGTLSServerName] = "pg.internal.example.test"
	ca2Path := filepath.Join(t.TempDir(), "ca2.pem")
	if err := os.WriteFile(ca2Path, ca2.pem, 0o600); err != nil {
		t.Fatal(err)
	}
	env[EnvPGMigrationCACert] = ca2Path

	cfg, err := load(envLookup(env))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.ServerName != "pg.internal.example.test" {
		t.Errorf("ServerName = %q, want %q", cfg.ServerName, "pg.internal.example.test")
	}
	if cfg.RuntimeTLS.ServerName() != "pg.internal.example.test" {
		t.Errorf("runtime ServerName = %q, want %q", cfg.RuntimeTLS.ServerName(), "pg.internal.example.test")
	}
	if cfg.MigrationTLS.CAFile != ca2Path {
		t.Errorf("migration CAFile = %q, want %q", cfg.MigrationTLS.CAFile, ca2Path)
	}
	// Distinct CA files must yield distinct pools.
	if cfg.MigrationTLS.RootCAs() == cfg.RuntimeTLS.RootCAs() {
		t.Error("expected distinct CA pools for distinct CA files")
	}
}

func TestLoadTLSModeSameMigrationPair(t *testing.T) {
	fx := setupTLSFixture(t)
	env := baseEnv()
	for k, v := range fx.vars {
		env[k] = v
	}
	env[EnvPGMigrationClientCert] = fx.vars[EnvPGClientCert]
	env[EnvPGMigrationClientKey] = fx.vars[EnvPGClientKey]
	_, err := load(envLookup(env))
	wantConfigError(t, err, EnvPGMigrationClientCert)
}

func TestLoadTLSModePartialTriple(t *testing.T) {
	fx := setupTLSFixture(t)
	env := baseEnv()
	for k, v := range fx.vars {
		env[k] = v
	}
	delete(env, EnvPGClientKey)
	_, err := load(envLookup(env))
	wantConfigError(t, err, EnvPGClientKey)
}

func TestLoadPlainMode(t *testing.T) {
	fx := setupTLSFixture(t)

	// Plain mode with no TLS variables succeeds and parses no material.
	env := baseEnv()
	env[EnvPGTLSMode] = "plain"
	cfg, err := load(envLookup(env))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.TLSMode != TLSModePlain {
		t.Errorf("TLSMode = %q, want %q", cfg.TLSMode, TLSModePlain)
	}
	if cfg.RuntimeTLS != nil || cfg.MigrationTLS != nil {
		t.Error("expected no TLS material in plain mode")
	}
	if cfg.ServerName != "" {
		t.Errorf("ServerName = %q, want empty in plain mode", cfg.ServerName)
	}

	// Plain mode forbids every TLS variable.
	env = baseEnv()
	env[EnvPGTLSMode] = "plain"
	env[EnvPGCACert] = fx.vars[EnvPGCACert]
	_, err = load(envLookup(env))
	wantConfigError(t, err, EnvPGCACert)
}

func TestLoadEndpointURL(t *testing.T) {
	env := baseEnv()
	env[EnvPGTLSMode] = "plain"
	delete(env, EnvPGHost)
	env[EnvPGURL] = "postgres://db.example.test:5433/vector"
	cfg, err := load(envLookup(env))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.PGHost != "db.example.test" || cfg.PGPort != 5433 || cfg.PGDatabase != DatabaseName {
		t.Errorf("endpoint = %s:%d/%s, want db.example.test:5433/%s",
			cfg.PGHost, cfg.PGPort, cfg.PGDatabase, DatabaseName)
	}

	// The URL form conflicts with the individual settings.
	env[EnvPGHost] = "other.example.test"
	_, err = load(envLookup(env))
	wantConfigError(t, err, EnvPGURL)
}

func TestLoadAdminTokenErrorIsSecretSafe(t *testing.T) {
	env := baseEnv()
	env[EnvPGTLSMode] = "plain"
	token := "s3cr3t-value-abc-0123456789" // 27 bytes: below the minimum
	env[EnvAdminToken] = token
	_, err := load(envLookup(env))
	wantConfigError(t, err, EnvAdminToken)
	if strings.Contains(err.Error(), token) {
		t.Errorf("error echoes the admin token: %v", err)
	}
}

func TestTLSMaterialConfig(t *testing.T) {
	fx := setupTLSFixture(t)
	env := baseEnv()
	for k, v := range fx.vars {
		env[k] = v
	}
	cfg, err := load(envLookup(env))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	m := cfg.RuntimeTLS
	tc := m.TLSConfig()
	if tc.InsecureSkipVerify {
		t.Error("InsecureSkipVerify must be false")
	}
	if tc.MinVersion != tls.VersionTLS12 {
		t.Errorf("MinVersion = %x, want TLS 1.2", uint16(tc.MinVersion))
	}
	if tc.RootCAs == nil {
		t.Error("RootCAs is nil")
	}
	if tc.ServerName != "db.example.test" {
		t.Errorf("ServerName = %q, want %q", tc.ServerName, "db.example.test")
	}
	if tc.GetClientCertificate == nil {
		t.Fatal("GetClientCertificate callback is nil")
	}
	cert, err := m.GetClientCertificate(nil)
	if err != nil {
		t.Fatalf("GetClientCertificate: %v", err)
	}
	if !bytes.Equal(cert.Certificate[0], fx.runtime.certDER) {
		t.Error("GetClientCertificate returned a different certificate than the configured leaf")
	}
}

func TestShutdownTimeoutSumBounds(t *testing.T) {
	// Table-driven tests for the combined shutdown timeout sum bound.
	// The sum of HTTPShutdownGrace and HTTPShutdownCloseTimeout must not
	// exceed 40s. Tests verify the bound applies to the effective sum
	// (after defaults are applied), not just explicitly-set values.
	tests := []struct {
		name    string
		grace   string // empty means use default (30s)
		close   string // empty means use default (10s)
		wantErr bool
	}{
		{
			name:    "default values sum to 40s",
			grace:   "",
			close:   "",
			wantErr: false,
		},
		{
			name:    "equal split 20+20",
			grace:   "20s",
			close:   "20s",
			wantErr: false,
		},
		{
			name:    "skewed valid 35+5",
			grace:   "35s",
			close:   "5s",
			wantErr: false,
		},
		{
			name:    "under limit 15+10",
			grace:   "15s",
			close:   "10s",
			wantErr: false,
		},
		{
			name:    "minimum valid 1+1",
			grace:   "1s",
			close:   "1s",
			wantErr: false,
		},
		{
			name:    "sum exceeds bound 30+11",
			grace:   "30s",
			close:   "11s",
			wantErr: true,
		},
		{
			name:    "large exceed 120+60",
			grace:   "120s",
			close:   "60s",
			wantErr: true,
		},
		{
			name:    "grace at limit close positive 40+1",
			grace:   "40s",
			close:   "1s",
			wantErr: true,
		},
		{
			name:    "empty grace default 30 + close 11 = 41",
			grace:   "",
			close:   "11s",
			wantErr: true,
		},
		{
			name:    "grace 31 + empty close default 10 = 41",
			grace:   "31s",
			close:   "",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := baseEnv()
			env[EnvPGTLSMode] = "plain"
			if tt.grace != "" {
				env[EnvHTTPShutdownGrace] = tt.grace
			}
			if tt.close != "" {
				env[EnvHTTPShutdownCloseTimeout] = tt.close
			}
			_, err := load(envLookup(env))
			if tt.wantErr {
				if err == nil {
					t.Errorf("expected error for %s+%s, got nil", tt.grace, tt.close)
				} else {
					var e *Error
					if !errors.As(err, &e) {
						t.Errorf("expected *config.Error, got %T: %v", err, err)
					} else if e.Variable != EnvHTTPShutdownGrace {
						t.Errorf("error variable = %q, want %q", e.Variable, EnvHTTPShutdownGrace)
					}
				}
			} else {
				if err != nil {
					t.Errorf("unexpected error: %v", err)
				}
			}
		})
	}
}

func TestShutdownTimeoutSumOverflow(t *testing.T) {
	// Verify the shutdown-sum bound check is overflow-safe: when both
	// durations are near math.MaxInt64 nanoseconds, the subtraction-based
	// check must still reject the combination rather than accepting a
	// wrapped-negative sum.
	tests := []struct {
		name  string
		grace string
		close string
	}{
		{
			name: "near MaxInt64 grace with small close",
			// MaxInt64 nanoseconds ~292 years. Use a large but parseable value.
			// time.ParseDuration accepts up to ~292 years in seconds;
			// use a value that parses but vastly exceeds 40s.
			grace: "1000000000s", // ~31.7 years
			close: "1s",
		},
		{
			name:  "near MaxInt64 close with small grace",
			grace: "1s",
			close: "1000000000s",
		},
		{
			name:  "both very large",
			grace: "1000000000s",
			close: "1000000000s",
		},
		{
			name:  "grace exceeds maxShutdownSum individually",
			grace: "41s",
			close: "1s",
		},
		{
			name:  "close exceeds maxShutdownSum individually",
			grace: "1s",
			close: "41s",
		},
		{
			name:  "grace exactly maxShutdownSum with positive close",
			grace: "40s",
			close: "1ns",
		},
		{
			name:  "close exactly maxShutdownSum with positive grace",
			grace: "1ns",
			close: "40s",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := baseEnv()
			env[EnvPGTLSMode] = "plain"
			env[EnvHTTPShutdownGrace] = tt.grace
			env[EnvHTTPShutdownCloseTimeout] = tt.close
			_, err := load(envLookup(env))
			if err == nil {
				t.Errorf("expected error for %s+%s (overflow or bound violation), got nil", tt.grace, tt.close)
			} else {
				var e *Error
				if !errors.As(err, &e) {
					t.Errorf("expected *config.Error, got %T: %v", err, err)
				}
				// The error variable should be EnvHTTPShutdownGrace (the sum check variable)
				// unless one of the individual durations fails parsing first.
				if e.Variable != EnvHTTPShutdownGrace && e.Variable != EnvHTTPShutdownCloseTimeout {
					t.Errorf("error variable = %q, want %q or %q",
						e.Variable, EnvHTTPShutdownGrace, EnvHTTPShutdownCloseTimeout)
				}
			}
		})
	}
}

func TestShutdownTimeoutSumBoundary(t *testing.T) {
	// Boundary cases: values exactly at, just below, and just above the 40s limit.
	tests := []struct {
		name    string
		grace   string
		close   string
		wantErr bool
	}{
		{
			name:    "exact 40s sum (39s + 1s)",
			grace:   "39s",
			close:   "1s",
			wantErr: false,
		},
		{
			name:    "exact 40s sum (20s + 20s)",
			grace:   "20s",
			close:   "20s",
			wantErr: false,
		},
		{
			name:    "exact 40s sum (1s + 39s)",
			grace:   "1s",
			close:   "39s",
			wantErr: false,
		},
		{
			name: "exact 40s sum with nanoseconds (39999999999ns + 1000000001ns = 41000000000ns = 41s)",
			// 39999999999 + 1000000001 = 41000000000ns = 41s > 40s
			grace:   "39999999999ns",
			close:   "1000000001ns",
			wantErr: true,
		},
		{
			name:    "exact 40s with nanoseconds (39000000000ns + 1000000000ns = 40000000000ns = 40s)",
			grace:   "39000000000ns",
			close:   "1000000000ns",
			wantErr: false,
		},
		{
			name:    "one nanosecond under 40s sum",
			grace:   "30000000000ns",
			close:   "9999999999ns",
			wantErr: false,
		},
		{
			name:    "one nanosecond over 40s sum",
			grace:   "30000000000ns",
			close:   "10000000001ns",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := baseEnv()
			env[EnvPGTLSMode] = "plain"
			env[EnvHTTPShutdownGrace] = tt.grace
			env[EnvHTTPShutdownCloseTimeout] = tt.close
			_, err := load(envLookup(env))
			if tt.wantErr {
				if err == nil {
					t.Errorf("expected error for %s+%s, got nil", tt.grace, tt.close)
				}
			} else {
				if err != nil {
					t.Errorf("unexpected error for %s+%s: %v", tt.grace, tt.close, err)
				}
			}
		})
	}
}
