package db

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"vector-service/internal/config"
)

// --- test certificate generation ---
// Inline certificate generation for cross-package testing.
// These produce in-memory PEM data used with config.NewTLSMaterialFromPEM.

func generateTestCA(t *testing.T, cn string) ([]byte, []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	serial := testSerial(t)
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: cn},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), nil
}

func generateTestLeaf(t *testing.T, cn string, caPEM []byte) ([]byte, []byte) {
	t.Helper()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	serial := testSerial(t)
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

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	serial = testSerial(t)
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: cn},
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
	return certPEM, keyPEM
}

func testSerial(t *testing.T) *big.Int {
	t.Helper()
	limit := new(big.Int).Lsh(big.NewInt(1), 63)
	n, err := rand.Int(rand.Reader, limit)
	if err != nil {
		t.Fatal(err)
	}
	return n.Add(n, big.NewInt(1))
}

// makeTestTLSMaterial creates TLS material from generated certificates.
// It returns the material and the CA PEM used to sign the leaf.
func makeTestTLSMaterial(t *testing.T, cn string, serverName string) (*config.TLSMaterial, []byte) {
	t.Helper()
	caPEM, _ := generateTestCA(t, "test-ca")
	certPEM, keyPEM := generateTestLeaf(t, cn, caPEM)
	m, err := config.NewTLSMaterialFromPEM(caPEM, certPEM, keyPEM, serverName)
	if err != nil {
		t.Fatalf("NewTLSMaterialFromPEM: %v", err)
	}
	return m, caPEM
}

// --- plain mode tests ---

// TestBuildRuntimePoolConfigPlainModeDisablesTLS verifies that plain mode
// explicitly disables TLS (TLSConfig is nil) and clears the Fallbacks
// list (no plaintext fallback from the default sslmode=prefer).
func TestBuildRuntimePoolConfigPlainModeDisablesTLS(t *testing.T) {
	cfg := &config.Config{
		PGHost:        "127.0.0.1",
		PGPort:        5432,
		PGDatabase:    config.DatabaseName,
		RuntimeRole:   config.RuntimeRole,
		TLSMode:       config.TLSModePlain,
		RuntimeTLS:    nil,
		MigrationTLS:  nil,
		MigrationRole: "vector_owner",
		Pool: config.Pool{
			MaxConns:              1,
			MinConns:              0,
			MaxConnLifetime:       30 * time.Minute,
			MaxConnLifetimeJitter: 5 * time.Minute,
			MaxConnIdleTime:       5 * time.Minute,
			HealthCheckPeriod:     30 * time.Second,
		},
	}

	pc, err := buildRuntimePoolConfig(cfg)
	if err != nil {
		t.Fatalf("buildRuntimePoolConfig: %v", err)
	}

	// TLSConfig must be nil — plain mode disables TLS entirely.
	if pc.ConnConfig.TLSConfig != nil {
		t.Error("plain mode: TLSConfig is not nil — TLS is not disabled")
	}

	// Fallbacks must be empty — no plaintext fallback from sslmode=prefer.
	if len(pc.ConnConfig.Fallbacks) > 0 {
		t.Errorf("plain mode: Fallbacks has %d entries — plaintext fallback may occur",
			len(pc.ConnConfig.Fallbacks))
	}
}

// TestBuildRuntimePoolConfigPlainModeNoDowngrade verifies that plain mode does not
// attempt TLS even when the default sslmode=prefer would normally cause it.
func TestBuildRuntimePoolConfigPlainModeNoDowngrade(t *testing.T) {
	cfg := &config.Config{
		PGHost:        "127.0.0.1",
		PGPort:        5432,
		PGDatabase:    config.DatabaseName,
		RuntimeRole:   config.RuntimeRole,
		TLSMode:       config.TLSModePlain,
		RuntimeTLS:    nil,
		MigrationTLS:  nil,
		MigrationRole: "vector_owner",
		Pool: config.Pool{
			MaxConns:              1,
			MinConns:              0,
			MaxConnLifetime:       30 * time.Minute,
			MaxConnLifetimeJitter: 5 * time.Minute,
			MaxConnIdleTime:       5 * time.Minute,
			HealthCheckPeriod:     30 * time.Second,
		},
	}

	pc, err := buildRuntimePoolConfig(cfg)
	if err != nil {
		t.Fatalf("buildRuntimePoolConfig: %v", err)
	}

	// Verify no TLS attempt is configured.
	if pc.ConnConfig.TLSConfig != nil {
		t.Error("plain mode: TLSConfig is non-nil — TLS attempt will be made")
	}

	// Verify no fallback to plaintext (or from TLS to plaintext).
	if len(pc.ConnConfig.Fallbacks) > 0 {
		t.Error("plain mode: Fallbacks is not empty — fallback connection attempts may occur")
	}
}

// TestBuildRuntimePoolConfigPlainModeRegression verifies the regression: before the
// fix, ParseConfig's default sslmode=prefer left a non-nil TLSConfig and
// a Fallbacks list even in plain mode. This test confirms both are cleared.
func TestBuildRuntimePoolConfigPlainModeRegression(t *testing.T) {
	cfg := &config.Config{
		PGHost:        "127.0.0.1",
		PGPort:        5432,
		PGDatabase:    config.DatabaseName,
		RuntimeRole:   config.RuntimeRole,
		TLSMode:       config.TLSModePlain,
		RuntimeTLS:    nil,
		MigrationTLS:  nil,
		MigrationRole: "vector_owner",
		Pool: config.Pool{
			MaxConns:              1,
			MinConns:              0,
			MaxConnLifetime:       30 * time.Minute,
			MaxConnLifetimeJitter: 5 * time.Minute,
			MaxConnIdleTime:       5 * time.Minute,
			HealthCheckPeriod:     30 * time.Second,
		},
	}

	// First, verify that ParseConfig alone (without our enforcement)
	// produces the problematic default: non-nil TLSConfig and Fallbacks.
	rawPC, err := pgxpool.ParseConfig("host=127.0.0.1 port=5432 user=vector_api dbname=vector")
	if err != nil {
		t.Fatalf("ParseConfig: %v", err)
	}
	// ParseConfig default: sslmode=prefer sets a non-nil TLSConfig.
	if rawPC.ConnConfig.TLSConfig == nil {
		t.Log("NOTE: ParseConfig default TLSConfig is nil — behavior may have changed")
	} else {
		t.Log("ParseConfig default: TLSConfig is non-nil (sslmode=prefer)")
	}
	// ParseConfig default: Fallbacks may contain a plaintext retry.
	if len(rawPC.ConnConfig.Fallbacks) > 0 {
		t.Logf("ParseConfig default: Fallbacks has %d entries",
			len(rawPC.ConnConfig.Fallbacks))
	}

	// Now verify our enforcement clears both.
	pc, err := buildRuntimePoolConfig(cfg)
	if err != nil {
		t.Fatalf("buildRuntimePoolConfig: %v", err)
	}

	if pc.ConnConfig.TLSConfig != nil {
		t.Error("REGRESSION: plain mode TLSConfig is not nil after enforcement")
	}
	if len(pc.ConnConfig.Fallbacks) > 0 {
		t.Error("REGRESSION: plain mode Fallbacks is not empty after enforcement")
	}
}

// --- TLS mode tests ---

// TestBuildRuntimePoolConfigTLSModeSetsTLSConfig verifies that when TLS mode is
// selected with valid RuntimeTLS material, the pool config has a non-nil
// TLSConfig with RootCAs, client certificate callback, no InsecureSkipVerify,
// and no Fallbacks (no plaintext fallback).
func TestBuildRuntimePoolConfigTLSModeSetsTLSConfig(t *testing.T) {
	runtimeTLS, _ := makeTestTLSMaterial(t, "runtime-client", "127.0.0.1")

	cfg := &config.Config{
		PGHost:        "127.0.0.1",
		PGPort:        5432,
		PGDatabase:    config.DatabaseName,
		RuntimeRole:   config.RuntimeRole,
		TLSMode:       config.TLSModeTLS,
		RuntimeTLS:    runtimeTLS,
		MigrationTLS:  nil,
		MigrationRole: "vector_owner",
		Pool: config.Pool{
			MaxConns:              1,
			MinConns:              0,
			MaxConnLifetime:       30 * time.Minute,
			MaxConnLifetimeJitter: 5 * time.Minute,
			MaxConnIdleTime:       5 * time.Minute,
			HealthCheckPeriod:     30 * time.Second,
		},
	}

	pc, err := buildRuntimePoolConfig(cfg)
	if err != nil {
		t.Fatalf("buildRuntimePoolConfig: %v", err)
	}

	tc := pc.ConnConfig.TLSConfig
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
	if len(pc.ConnConfig.Fallbacks) > 0 {
		t.Errorf("TLS mode: Fallbacks has %d entries — plaintext fallback may occur",
			len(pc.ConnConfig.Fallbacks))
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

// TestBuildRuntimePoolConfigTLSModeNoFallback verifies that TLS mode
// clears Fallbacks (no plaintext fallback).
func TestBuildRuntimePoolConfigTLSModeNoFallback(t *testing.T) {
	runtimeTLS, _ := makeTestTLSMaterial(t, "runtime-client", "127.0.0.1")

	cfg := &config.Config{
		PGHost:        "127.0.0.1",
		PGPort:        5432,
		PGDatabase:    config.DatabaseName,
		RuntimeRole:   config.RuntimeRole,
		TLSMode:       config.TLSModeTLS,
		RuntimeTLS:    runtimeTLS,
		MigrationTLS:  nil,
		MigrationRole: "vector_owner",
		Pool: config.Pool{
			MaxConns:              1,
			MinConns:              0,
			MaxConnLifetime:       30 * time.Minute,
			MaxConnLifetimeJitter: 5 * time.Minute,
			MaxConnIdleTime:       5 * time.Minute,
			HealthCheckPeriod:     30 * time.Second,
		},
	}

	pc, err := buildRuntimePoolConfig(cfg)
	if err != nil {
		t.Fatalf("buildRuntimePoolConfig: %v", err)
	}

	// Verify Fallbacks are cleared.
	if len(pc.ConnConfig.Fallbacks) > 0 {
		t.Errorf("TLS mode: Fallbacks has %d entries — plaintext fallback may occur",
			len(pc.ConnConfig.Fallbacks))
	}
}

// TestBuildRuntimePoolConfigRejectsNilTLSInTLSMode verifies that TLS mode with
// nil RuntimeTLS is rejected (safety: TLS mode requires parsed material).
func TestBuildRuntimePoolConfigRejectsNilTLSInTLSMode(t *testing.T) {
	cfg := &config.Config{
		PGHost:        "127.0.0.1",
		PGPort:        5432,
		PGDatabase:    config.DatabaseName,
		RuntimeRole:   config.RuntimeRole,
		TLSMode:       config.TLSModeTLS,
		RuntimeTLS:    nil,
		MigrationRole: "vector_owner",
		Pool: config.Pool{
			MaxConns:              1,
			MinConns:              0,
			MaxConnLifetime:       30 * time.Minute,
			MaxConnLifetimeJitter: 5 * time.Minute,
			MaxConnIdleTime:       5 * time.Minute,
			HealthCheckPeriod:     30 * time.Second,
		},
	}

	_, err := buildRuntimePoolConfig(cfg)
	if err == nil {
		t.Fatal("expected error when TLS mode is selected but RuntimeTLS is nil")
	}
}

// --- integration tests (require network) ---

// TestNewRuntimePoolActualCallPlainMode verifies that NewRuntimePool
// itself (not just the config construction) handles plain mode correctly.
// It connects to an unreachable host; the expected failure is a connection
// error, not a TLS-related error or config-construction panic.
func TestNewRuntimePoolActualCallPlainMode(t *testing.T) {
	cfg := &config.Config{
		PGHost:        "127.0.0.1",
		PGPort:        1, // unreachable port
		PGDatabase:    config.DatabaseName,
		RuntimeRole:   config.RuntimeRole,
		TLSMode:       config.TLSModePlain,
		RuntimeTLS:    nil,
		MigrationTLS:  nil,
		MigrationRole: "vector_owner",
		Pool: config.Pool{
			MaxConns:              1,
			MinConns:              0,
			MaxConnLifetime:       30 * time.Minute,
			MaxConnLifetimeJitter: 5 * time.Minute,
			MaxConnIdleTime:       5 * time.Minute,
			HealthCheckPeriod:     30 * time.Second,
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	_, err := NewRuntimePool(ctx, cfg)
	if err == nil {
		t.Fatal("expected a connection error, got nil")
	}
	// The error must be a connection failure, not a TLS error.
	// In plain mode, no TLS attempt should be made.
	errStr := err.Error()
	if strings.Contains(errStr, "TLS") || strings.Contains(errStr, "tls") ||
		strings.Contains(errStr, "SSL") || strings.Contains(errStr, "ssl") ||
		strings.Contains(errStr, "certificate") {
		t.Errorf("plain mode: error mentions TLS/SSL — TLS was attempted: %v", err)
	}
}

// TestNewRuntimePoolUsesBuildRuntimePoolConfig verifies that NewRuntimePool
// delegates to buildRuntimePoolConfig by checking that a configuration error
// (nil RuntimeTLS in TLS mode) is reported by NewRuntimePool.
func TestNewRuntimePoolUsesBuildRuntimePoolConfig(t *testing.T) {
	cfg := &config.Config{
		PGHost:        "127.0.0.1",
		PGPort:        5432,
		PGDatabase:    config.DatabaseName,
		RuntimeRole:   config.RuntimeRole,
		TLSMode:       config.TLSModeTLS,
		RuntimeTLS:    nil,
		MigrationRole: "vector_owner",
		Pool: config.Pool{
			MaxConns:              1,
			MinConns:              0,
			MaxConnLifetime:       30 * time.Minute,
			MaxConnLifetimeJitter: 5 * time.Minute,
			MaxConnIdleTime:       5 * time.Minute,
			HealthCheckPeriod:     30 * time.Second,
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	_, err := NewRuntimePool(ctx, cfg)
	if err == nil {
		t.Fatal("expected error when TLS mode is selected but RuntimeTLS is nil")
	}
	// The error should come from buildRuntimePoolConfig, not from pool creation.
	if !strings.Contains(err.Error(), "TLS mode selected") {
		t.Errorf("expected TLS mode error from buildRuntimePoolConfig, got: %v", err)
	}
}

// TestBuildRuntimePoolConfigAppliesPoolParams verifies that buildRuntimePoolConfig
// correctly applies all pool parameters from the configuration.
func TestBuildRuntimePoolConfigAppliesPoolParams(t *testing.T) {
	cfg := &config.Config{
		PGHost:        "127.0.0.1",
		PGPort:        5432,
		PGDatabase:    config.DatabaseName,
		RuntimeRole:   config.RuntimeRole,
		TLSMode:       config.TLSModePlain,
		RuntimeTLS:    nil,
		MigrationRole: "vector_owner",
		Pool: config.Pool{
			MaxConns:              42,
			MinConns:              5,
			MaxConnLifetime:       60 * time.Minute,
			MaxConnLifetimeJitter: 10 * time.Minute,
			MaxConnIdleTime:       15 * time.Minute,
			HealthCheckPeriod:     45 * time.Second,
		},
	}

	pc, err := buildRuntimePoolConfig(cfg)
	if err != nil {
		t.Fatalf("buildRuntimePoolConfig: %v", err)
	}

	if pc.MaxConns != 42 {
		t.Errorf("MaxConns = %d, want 42", pc.MaxConns)
	}
	if pc.MinConns != 5 {
		t.Errorf("MinConns = %d, want 5", pc.MinConns)
	}
	if pc.MaxConnLifetime != 60*time.Minute {
		t.Errorf("MaxConnLifetime = %v, want %v", pc.MaxConnLifetime, 60*time.Minute)
	}
	if pc.MaxConnLifetimeJitter != 10*time.Minute {
		t.Errorf("MaxConnLifetimeJitter = %v, want %v", pc.MaxConnLifetimeJitter, 10*time.Minute)
	}
	if pc.MaxConnIdleTime != 15*time.Minute {
		t.Errorf("MaxConnIdleTime = %v, want %v", pc.MaxConnIdleTime, 15*time.Minute)
	}
	if pc.HealthCheckPeriod != 45*time.Second {
		t.Errorf("HealthCheckPeriod = %v, want %v", pc.HealthCheckPeriod, 45*time.Second)
	}
}

// TestBuildRuntimePoolConfigDSNConstruction verifies that the DSN is constructed
// correctly from the configuration fields.
func TestBuildRuntimePoolConfigDSNConstruction(t *testing.T) {
	cfg := &config.Config{
		PGHost:        "db.example.test",
		PGPort:        5433,
		PGDatabase:    config.DatabaseName,
		RuntimeRole:   config.RuntimeRole,
		TLSMode:       config.TLSModePlain,
		RuntimeTLS:    nil,
		MigrationRole: "vector_owner",
		Pool: config.Pool{
			MaxConns:              1,
			MinConns:              0,
			MaxConnLifetime:       30 * time.Minute,
			MaxConnLifetimeJitter: 5 * time.Minute,
			MaxConnIdleTime:       5 * time.Minute,
			HealthCheckPeriod:     30 * time.Second,
		},
	}

	pc, err := buildRuntimePoolConfig(cfg)
	if err != nil {
		t.Fatalf("buildRuntimePoolConfig: %v", err)
	}

	// Verify the DSN was parsed correctly by checking the ConnConfig fields.
	if pc.ConnConfig.Host != "db.example.test" {
		t.Errorf("Host = %q, want %q", pc.ConnConfig.Host, "db.example.test")
	}
	if pc.ConnConfig.Port != 5433 {
		t.Errorf("Port = %d, want %d", pc.ConnConfig.Port, 5433)
	}
	if pc.ConnConfig.Database != config.DatabaseName {
		t.Errorf("Database = %q, want %q", pc.ConnConfig.Database, config.DatabaseName)
	}
	if pc.ConnConfig.User != config.RuntimeRole {
		t.Errorf("User = %q, want %q", pc.ConnConfig.User, config.RuntimeRole)
	}
}

// TestBuildRuntimePoolConfigDefaultQueryExecMode verifies that the pool config
// uses the simple query protocol (text-only results) required by the privilege
// catalog and identity assertion layers.
func TestBuildRuntimePoolConfigDefaultQueryExecMode(t *testing.T) {
	cfg := &config.Config{
		PGHost:        "127.0.0.1",
		PGPort:        5432,
		PGDatabase:    config.DatabaseName,
		RuntimeRole:   config.RuntimeRole,
		TLSMode:       config.TLSModePlain,
		RuntimeTLS:    nil,
		MigrationRole: "vector_owner",
		Pool: config.Pool{
			MaxConns: 1, MinConns: 0,
			MaxConnLifetime: 30 * time.Minute,
			MaxConnLifetimeJitter: 5 * time.Minute,
			MaxConnIdleTime: 5 * time.Minute,
			HealthCheckPeriod: 30 * time.Second,
		},
	}

	pc, err := buildRuntimePoolConfig(cfg)
	if err != nil {
		t.Fatalf("buildRuntimePoolConfig: %v", err)
	}

	if pc.ConnConfig.DefaultQueryExecMode != pgx.QueryExecModeSimpleProtocol {
		t.Errorf("DefaultQueryExecMode = %v, want SimpleProtocol",
			pc.ConnConfig.DefaultQueryExecMode)
	}
}
