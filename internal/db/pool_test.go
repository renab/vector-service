package db

import (
	"context"
	"strings"
	"testing"
	"time"

	"vector-service/internal/config"
)

// TestNewRuntimePoolNoPanicOnConfigConstruction verifies that NewRuntimePool
// constructs its pgxpool.Config through pgxpool.ParseConfig rather than by
// manually assembling pgxpool.Config and pgx.ConnConfig structs. Manually
// constructed configs panic in pgx v5 ("config must be created by
// ParseConfig"). This test connects to an unreachable host; the expected
// failure is a connection error, not a panic.
func TestNewRuntimePoolNoPanicOnConfigConstruction(t *testing.T) {
	cfg := &config.Config{
		PGHost:     "127.0.0.1",
		PGPort:     1, // unreachable port
		PGDatabase: config.DatabaseName,
		RuntimeRole: config.RuntimeRole,
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
	// The error must be a connection failure, not a config-construction panic.
	// If the config was manually constructed, pgx v5 panics before reaching
	// the connection attempt, so this test would not reach here.
	if !strings.Contains(err.Error(), "runtime pool") &&
		!strings.Contains(err.Error(), "runtime identity assertion") {
		t.Fatalf("expected a runtime pool error, got: %v", err)
	}
}
