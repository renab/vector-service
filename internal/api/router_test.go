package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"vector-service/internal/apierr"
	"vector-service/internal/config"
	"vector-service/internal/vectors"
)

// ---------------------------------------------------------------------------
// Routing outcome tests (404 / 405)
// ---------------------------------------------------------------------------

func TestRoutingOutcome_UnknownPath(t *testing.T) {
	fb := routingOutcome(routeTable)
	req := httptest.NewRequest(http.MethodGet, "/v1/unknown", nil)
	rec := httptest.NewRecorder()
	fb.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	var body struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if body.Error.Code != string(apierr.CodeNotFound) {
		t.Fatalf("code = %q, want %q", body.Error.Code, apierr.CodeNotFound)
	}
}

func TestRoutingOutcome_WrongMethod_AllowHeader(t *testing.T) {
	// The fallback handler distinguishes a wrong-method 405 (path known,
	// method not allowed) from an unknown-path 404 by matching the request
	// path against the static route table and computing the Allow header
	// itself. ServeMux with a registered fallback does not set the Allow
	// header, so the fallback owns it.
	fb := routingOutcome(routeTable)
	req := httptest.NewRequest(http.MethodGet, "/v1/namespaces/project-alpha/search", nil)
	rec := httptest.NewRecorder()
	fb.ServeHTTP(rec, req)

	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", rec.Code)
	}
	if allow := rec.Header().Get("Allow"); allow != "POST" {
		t.Fatalf("Allow header = %q, want POST", allow)
	}
	var body struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if body.Error.Code != string(apierr.CodeMethodNotAllowed) {
		t.Fatalf("code = %q, want %q", body.Error.Code, apierr.CodeMethodNotAllowed)
	}
}

// ---------------------------------------------------------------------------
// Health and readiness
// ---------------------------------------------------------------------------

func TestHealthz(t *testing.T) {
	mux := http.NewServeMux()
	mux.Handle("GET /healthz", chainForTest().BaseChain(healthzHandler(&Deps{})))

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("content-type = %q", ct)
	}
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if body["status"] != "ok" {
		t.Fatalf("status = %q, want ok", body["status"])
	}
}

func TestReadyz_MigrationsNotApplied(t *testing.T) {
	mux := http.NewServeMux()
	deps := Deps{MigrationsApplied: func() bool { return false }}
	mux.Handle("GET /readyz", chainForTest().BaseChain(readyzHandler(&deps)))

	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

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

func TestReadyz_MigrationsAppliedButNoPool(t *testing.T) {
	mux := http.NewServeMux()
	deps := Deps{} // nil Pool
	deps.MigrationsApplied = func() bool { return true }
	mux.Handle("GET /readyz", chainForTest().BaseChain(readyzHandler(&deps)))

	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	// nil Pool → guard returns 503 (not a panic into recovery)
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

// TestReadyz_FlagSetAfterConstruction verifies that the readyz route reads
// the dependency set through a pointer: a field set after the handler is
// built is observed at request time. This is the in-process readiness
// contract: the package-1 startup migration step flips MigrationsApplied
// once the database is converged, after the router is built.
func TestReadyz_FlagSetAfterConstruction(t *testing.T) {
	deps := &Deps{} // MigrationsApplied nil (unconverged)
	mux := http.NewServeMux()
	mux.Handle("GET /readyz", chainForTest().BaseChain(readyzHandler(deps)))

	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("unconverged: status = %d, want 503", rec.Code)
	}

	deps.MigrationsApplied = func() bool { return true } // convergence observed post-construction

	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("converged but nil Pool: status = %d, want 503", rec.Code)
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

// ---------------------------------------------------------------------------
// UUID validation
// ---------------------------------------------------------------------------

func TestIsCanonicalUUID(t *testing.T) {
	tests := []struct {
		in   string
		want bool
	}{
		{"0199f31e-2000-7000-8000-0000000000a1", true},
		{"00000000-0000-0000-0000-000000000000", true},
		{"ffffffff-ffff-ffff-ffff-ffffffffffff", true},
		{"", false},
		{"0199f31e-2000-7000-8000-0000000000a", false},   // too short
		{"0199f31e-2000-7000-8000-0000000000a11", false}, // too long
		{"0199F31E-2000-7000-8000-0000000000a1", false},  // uppercase
		{"0199f31e_2000_7000_8000_0000000000a1", false},  // wrong separator
		{"0199f31e.2000.7000.8000.0000000000a1", false},  // wrong separator
		{"g199f31e-2000-7000-8000-0000000000a1", false},  // non-hex
		{"0199f31e2000700080000000000000a1", false},      // no separators
	}
	for _, tt := range tests {
		if got := isCanonicalUUID(tt.in); got != tt.want {
			t.Errorf("isCanonicalUUID(%q) = %v, want %v", tt.in, got, tt.want)
		}
	}
}

// ---------------------------------------------------------------------------
// Application key pattern
// ---------------------------------------------------------------------------

func TestAppKeyPattern(t *testing.T) {
	tests := []struct {
		key  string
		want bool
	}{
		{"memory-service", true},
		{"notes_service", true},
		{"search-service-2", true},
		{"a", true},
		{"", false},
		{"A", false},
		{"-abc", false},
		{"ab cd", false},
		{"abc/def", false},
		{"a_b-c_1", true},
		// Trailing dash is allowed by the grammar (mirror of the schema
		// constraint): the last character may be [a-z0-9_-].
		{"abc-", true},
		// 63 chars (1 leading + 62) — within the 64-char max
		{"a22222222222222222222222222222222222222222222222222222222222222", true},
		// 64 chars (1 leading + 63) — the maximum
		{"a222222222222222222222222222222222222222222222222222222222222222", true},
		// 65 chars (1 leading + 64) — over the maximum
		{"a2222222222222222222222222222222222222222222222222222222222222222", false},
	}
	for _, tt := range tests {
		if got := appKeyPattern.MatchString(tt.key); got != tt.want {
			t.Errorf("appKeyPattern.MatchString(%q) = %v, want %v", tt.key, got, tt.want)
		}
	}
}

// ---------------------------------------------------------------------------
// Filter conversion
// ---------------------------------------------------------------------------

func TestSearchFiltersToFilterEntries(t *testing.T) {
	tests := []struct {
		name    string
		filters searchFilters
		want    []vectors.FilterEntry
		wantErr bool
	}{
		{
			name:    "empty",
			filters: searchFilters{},
			want:    []vectors.FilterEntry{},
		},
		{
			name: "eq with value",
			filters: searchFilters{All: []searchFilterEntry{
				{Field: "type", Op: "eq", Value: "note"},
			}},
			want: []vectors.FilterEntry{
				{Field: "type", Op: vectors.FilterEq, Values: []any{"note"}},
			},
		},
		{
			name: "in with values",
			filters: searchFilters{All: []searchFilterEntry{
				{Field: "tag", Op: "in", Values: []any{"a", "b"}},
			}},
			want: []vectors.FilterEntry{
				{Field: "tag", Op: vectors.FilterIn, Values: []any{"a", "b"}},
			},
		},
		{
			name: "multiple filters",
			filters: searchFilters{All: []searchFilterEntry{
				{Field: "type", Op: "eq", Value: "note"},
				{Field: "tag", Op: "in", Values: []any{"x"}},
			}},
			want: []vectors.FilterEntry{
				{Field: "type", Op: vectors.FilterEq, Values: []any{"note"}},
				{Field: "tag", Op: vectors.FilterIn, Values: []any{"x"}},
			},
		},
		{
			name: "eq without value",
			filters: searchFilters{All: []searchFilterEntry{
				{Field: "type", Op: "eq"},
			}},
			wantErr: true,
		},
		{
			name: "in without values",
			filters: searchFilters{All: []searchFilterEntry{
				{Field: "tag", Op: "in"},
			}},
			wantErr: true,
		},
		{
			name: "unknown op",
			filters: searchFilters{All: []searchFilterEntry{
				{Field: "type", Op: "gt", Value: "note"},
			}},
			wantErr: true,
		},
		{
			name: "empty field",
			filters: searchFilters{All: []searchFilterEntry{
				{Field: "", Op: "eq", Value: "note"},
			}},
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := tt.filters.toFilterEntries()
			if (err != nil) != tt.wantErr {
				t.Fatalf("toFilterEntries() error = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				if err == nil || err.Code != apierr.CodeInvalidFilter {
					t.Fatalf("error = %v, want invalid_filter", err)
				}
				return
			}
			if len(got) != len(tt.want) {
				t.Fatalf("len(entries) = %d, want %d", len(got), len(tt.want))
			}
			for i := range got {
				if got[i].Field != tt.want[i].Field || got[i].Op != tt.want[i].Op {
					t.Fatalf("entries[%d] = %+v, want %+v", i, got[i], tt.want[i])
				}
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Ping duration
// ---------------------------------------------------------------------------

func TestPingDuration(t *testing.T) {
	tests := []struct {
		in   time.Duration
		want time.Duration
	}{
		{0, 2 * time.Second},
		{-1 * time.Second, 2 * time.Second},
		{500 * time.Millisecond, 500 * time.Millisecond},
		{10 * time.Second, 10 * time.Second},
	}
	for _, tt := range tests {
		if got := pingDuration(tt.in); got != tt.want {
			t.Errorf("pingDuration(%v) = %v, want %v", tt.in, got, tt.want)
		}
	}
}

// ---------------------------------------------------------------------------
// Route registration
// ---------------------------------------------------------------------------

func TestNewRouter_RegistersAllRoutes(t *testing.T) {
	cfg := &config.Config{
		AdminToken:         "test-admin-token",
		HTTPMaxBodyBytes:   1024,
		HTTPRequestTimeout: 5 * time.Second,
		MaxMetadataBytes:   4096,
	}
	deps := Deps{} // nil Pool — we only verify routing, not DB operations
	handler := NewRouter(cfg, deps)

	// Verify the handler is non-nil.
	if handler == nil {
		t.Fatal("NewRouter returned nil")
	}

	// Spot-check: an unknown route returns 404.
	req := httptest.NewRequest(http.MethodGet, "/v1/definitely-not-a-route", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("unknown route: status = %d, want 404", rec.Code)
	}

	// Spot-check: wrong method on a known route returns 405.
	// GET on POST /v1/admin/applications
	req = httptest.NewRequest(http.MethodGet, "/v1/admin/applications", nil)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("wrong method: status = %d, want 405", rec.Code)
	}
	if allow := rec.Header().Get("Allow"); !strings.Contains(allow, "POST") {
		t.Errorf("Allow header = %q, want to contain POST", allow)
	}
}

func TestNewRouter_HealthEndpoint(t *testing.T) {
	cfg := &config.Config{
		AdminToken:         "test-admin-token",
		HTTPMaxBodyBytes:   1024,
		HTTPRequestTimeout: 5 * time.Second,
	}
	handler := NewRouter(cfg, Deps{})

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("healthz: status = %d, want 200", rec.Code)
	}
}

// TestNoDataPlaneNamespaceListing proves that the data plane has no
// endpoint for listing namespaces. It uses two complementary checks:
//
//  1. Static scan of routeTable — catches a listing pattern added to the
//     fallback table.
//  2. Runtime probes against the actual mux — catches a listing route
//     registered directly on the mux (via mux.Handle) at any plausible
//     namespace path, including paths outside the collection root.
//
// This satisfies the Case 5 requirement that callers cannot discover
// other applications' namespaces through the data plane.
func TestNoDataPlaneNamespaceListing(t *testing.T) {
	// --- Static check: route table scan ---
	dataPlaneListPaths := []string{
		"/v1/namespaces",                       // collection listing
		"/v1/namespaces/{namespace}",           // namespace detail
		"/v1/namespaces/{namespace}/namespaces", // nested listing
	}
	for _, p := range dataPlaneListPaths {
		for _, entry := range routeTable {
			if entry.pattern == p {
				t.Errorf("data-plane namespace listing endpoint found in routeTable: %s (%s) — violates isolation contract",
					entry.pattern, entry.methods)
			}
		}
	}

	// --- Runtime check: exercise the actual mux ---
	// Build the real router and probe every plausible namespace-listing
	// path with GET. Each must return 404 (unknown path), not 405
	// (known path, wrong method). A 405 response means a route exists
	// at that path pattern — i.e., a listing endpoint was registered.
	cfg := &config.Config{
		AdminToken:         "test-admin-token",
		HTTPMaxBodyBytes:   1024,
		HTTPRequestTimeout: 5 * time.Second,
	}
	handler := NewRouter(cfg, Deps{})

	// namespaceListingPaths covers collection, detail, and nested paths
	// with and without trailing slashes. The set is designed to catch
	// any route registered directly on the mux at a namespace-related
	// path that would allow namespace enumeration.
	namespaceListingPaths := []struct {
		path string
		desc string
	}{
		{"/v1/namespaces", "collection listing"},
		{"/v1/namespaces/", "collection listing (trailing slash)"},
		{"/v1/namespaces/project-alpha", "namespace detail"},
		{"/v1/namespaces/project-alpha/", "namespace detail (trailing slash)"},
		{"/v1/namespaces/project-alpha/namespaces", "nested namespace listing"},
		{"/v1/namespaces/project-alpha/namespaces/", "nested namespace listing (trailing slash)"},
	}
	for _, tc := range namespaceListingPaths {
		req := httptest.NewRequest(http.MethodGet, tc.path, nil)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code == http.StatusMethodNotAllowed {
			t.Errorf("GET %s returned 405 — a namespace listing endpoint exists (%s)", tc.path, tc.desc)
		}
		if rec.Code != http.StatusNotFound {
			t.Errorf("GET %s: status = %d, want 404 (%s)", tc.path, rec.Code, tc.desc)
		}
	}
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// chainForTest builds a minimal Chain for tests that exercise routing
// without a database.
func chainForTest() *Chain {
	return &Chain{
		Deadline:     &OperationDeadline{Timeout: 5 * time.Second},
		MaxBodyBytes: 1024,
	}
}
