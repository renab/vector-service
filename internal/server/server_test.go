package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"vector-service/internal/api"
	"vector-service/internal/apierr"
	"vector-service/internal/config"
)

// testConfig returns a validated-shape configuration with distinct timeout
// values so the construction test can assert each bound individually.
func testConfig() *config.Config {
	return &config.Config{
		ListenAddr:              "127.0.0.1:8080",
		AdminToken:              "test-admin-token",
		HTTPMaxBodyBytes:        1024,
		HTTPRequestTimeout:      5 * time.Second,
		MaxMetadataBytes:        4096,
		ServerReadHeaderTimeout: 1 * time.Second,
		ServerReadTimeout:       2 * time.Second,
		ServerWriteTimeout:      3 * time.Second,
		ServerIdleTimeout:       4 * time.Second,
	}
}

// ---------------------------------------------------------------------------
// Construction
// ---------------------------------------------------------------------------

func TestNew_ConstructsServerFromConfig(t *testing.T) {
	cfg := testConfig()
	root := context.Background()

	s := New(cfg, api.Deps{}, root)

	hs := s.HTTPServer()
	if hs == nil {
		t.Fatal("HTTPServer() returned nil")
	}
	if hs.Handler == nil {
		t.Fatal("Handler is nil; the assembled router is required")
	}
	if hs.BaseContext == nil {
		t.Fatal("BaseContext is nil; the serve root context is required")
	}
	if hs.Addr != cfg.ListenAddr {
		t.Errorf("Addr = %q, want %q", hs.Addr, cfg.ListenAddr)
	}
	if hs.ReadHeaderTimeout != cfg.ServerReadHeaderTimeout {
		t.Errorf("ReadHeaderTimeout = %v, want %v", hs.ReadHeaderTimeout, cfg.ServerReadHeaderTimeout)
	}
	if hs.ReadTimeout != cfg.ServerReadTimeout {
		t.Errorf("ReadTimeout = %v, want %v", hs.ReadTimeout, cfg.ServerReadTimeout)
	}
	if hs.WriteTimeout != cfg.ServerWriteTimeout {
		t.Errorf("WriteTimeout = %v, want %v", hs.WriteTimeout, cfg.ServerWriteTimeout)
	}
	if hs.IdleTimeout != cfg.ServerIdleTimeout {
		t.Errorf("IdleTimeout = %v, want %v", hs.IdleTimeout, cfg.ServerIdleTimeout)
	}
}

func TestNewWithRoot_BaseContextReturnsRoot(t *testing.T) {
	cfg := testConfig()

	root, cancel := context.WithCancel(context.Background())
	defer cancel()

	s := NewWithRoot(cfg, api.Deps{}, root)
	if s.HTTPServer().BaseContext == nil {
		t.Fatal("BaseContext is nil")
	}
	got := s.HTTPServer().BaseContext(nil)
	if got != root {
		t.Fatalf("BaseContext(nil) = %v, want the serve root context", got)
	}
}

func TestNewWithRoot_NilRootFallsBackToBackground(t *testing.T) {
	cfg := testConfig()

	s := NewWithRoot(cfg, api.Deps{}, nil)
	if s.HTTPServer().BaseContext == nil {
		t.Fatal("BaseContext is nil")
	}
	if got := s.HTTPServer().BaseContext(nil); got == nil {
		t.Fatal("BaseContext(nil) returned nil; must return a non-nil context")
	}
}

func TestDefaultBaseContext(t *testing.T) {
	if got := DefaultBaseContext(); got == nil {
		t.Fatal("DefaultBaseContext() returned nil")
	}
}

// ---------------------------------------------------------------------------
// Health and readiness
// ---------------------------------------------------------------------------

func TestHealthz(t *testing.T) {
	s := New(testConfig(), api.Deps{}, context.Background())

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	s.HTTPServer().Handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("content-type = %q, want application/json", ct)
	}
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if body["status"] != "ok" {
		t.Fatalf("status = %q, want ok", body["status"])
	}
}

func TestReadyz_Unconverged(t *testing.T) {
	// MigrationsApplied is nil (unconverged): 503 unavailable, no pool
	// needed.
	s := New(testConfig(), api.Deps{}, context.Background())

	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	rec := httptest.NewRecorder()
	s.HTTPServer().Handler.ServeHTTP(rec, req)

	assertUnavailable(t, rec)
}

func TestReadyz_Converged_NilPool(t *testing.T) {
	deps := api.Deps{MigrationsApplied: func() bool { return true }} // nil Pool
	s := New(testConfig(), deps, context.Background())

	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	rec := httptest.NewRecorder()
	s.HTTPServer().Handler.ServeHTTP(rec, req)

	assertUnavailable(t, rec)
}

// TestReadyz_MarkMigrationsConvergedAfterNew verifies the in-process
// readiness contract through the Server: MarkMigrationsConverged flips the
// shared flag after construction, and the already-built router observes it
// at request time. The 503 remains because the pool is nil (the ping step
// is unreachable without a live database; the flag check is the unit under
// test here).
func TestReadyz_MarkMigrationsConvergedAfterNew(t *testing.T) {
	s := New(testConfig(), api.Deps{}, context.Background())

	before := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	rec := httptest.NewRecorder()
	s.HTTPServer().Handler.ServeHTTP(rec, before)
	assertUnavailable(t, rec)

	s.MarkMigrationsConverged() // convergence observed post-construction

	after := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	rec = httptest.NewRecorder()
	s.HTTPServer().Handler.ServeHTTP(rec, after)
	assertUnavailable(t, rec) // flag now true, but nil Pool → 503
}

func TestMarkMigrationsConverged_Idempotent(t *testing.T) {
	s := New(testConfig(), api.Deps{}, context.Background())

	s.MarkMigrationsConverged()
	s.MarkMigrationsConverged() // second call is a no-op

	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	rec := httptest.NewRecorder()
	s.HTTPServer().Handler.ServeHTTP(rec, req)
	assertUnavailable(t, rec)
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// assertUnavailable asserts the typed 503 unavailable response.
func assertUnavailable(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
	var body struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if body.Error.Code != string(apierr.CodeUnavailable) {
		t.Fatalf("code = %q, want %q", body.Error.Code, apierr.CodeUnavailable)
	}
}
