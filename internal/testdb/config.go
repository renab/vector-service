package testdb

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strconv"

	"vector-service/internal/config"
)

// GenerateAdminToken returns a high-entropy machine-generated admin-plane
// bearer token: 32 random bytes, hex-encoded (64 characters, above the
// configuration's 32-byte minimum). The value is secret material: never log
// it, and keep it out of test output and error text.
func GenerateAdminToken() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("testdb: generate admin token: %w", err)
	}
	return hex.EncodeToString(buf), nil
}

// RuntimeEnv is the complete VEC_* environment for the provisioned
// disposable database: plain transport mode (no TLS variables, as plain
// mode forbids), the canonical roles, the suite endpoint, and a generated
// high-entropy admin token. config.Load succeeds against it, so service-
// function and HTTP tests run against the same provisioned state the
// database-integration tests use.
func (h *Harness) RuntimeEnv() (map[string]string, error) {
	token, err := GenerateAdminToken()
	if err != nil {
		return nil, err
	}
	host := h.cfg.Host
	if host == "" {
		host = "127.0.0.1"
	}
	return map[string]string{
		config.EnvListenAddr:      "127.0.0.1:0",
		config.EnvPGHost:          host,
		config.EnvPGPort:          strconv.FormatUint(uint64(h.cfg.Port), 10),
		config.EnvPGDatabase:      DatabaseName,
		config.EnvPGUser:          RuntimeRole,
		config.EnvPGMigrationUser: MigrationRole,
		config.EnvPGTLSMode:       string(config.TLSModePlain),
		config.EnvAdminToken:      token,
		config.EnvLogLevel:        "error",
	}, nil
}
