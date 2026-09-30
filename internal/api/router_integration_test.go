//go:build integration

package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"vector-service/internal/auth"
	"vector-service/internal/config"
	"vector-service/internal/testdb"
)

// The package-4 HTTP router matrix against a real converged PostgreSQL:
// the composed request path (HTTP request -> route -> chain -> auth ->
// handler -> service function -> pool with the canonical runtime role and
// RLS) is exercised end to end. The fixture provisions a fresh database
// through the production runner, registers applications and namespaces and
// data-plane credentials through bootstrap SQL (the same boundary the
// vectors integration suite uses), and then drives real HTTP requests
// through the router over httptest.
//
// The admin credential is the config admin token compared in-process; the
// data-plane credentials are generated secrets whose keyed digests are
// stored in vector_control.application_credentials by the fixture.

// apiFixture is one test's provisioned suite.
type apiFixture struct {
	cfg  *config.Config
	srv  *httptest.Server
	boot *pgx.Conn
}

// setupAPIFixture builds the suite fixture and skips when the bootstrap
// identity is not configured.
func setupAPIFixture(t *testing.T) *apiFixture {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	h, err := testdb.New(ctx)
	if err != nil {
		t.Skipf("testdb: bootstrap identity not configured (set %s to a superuser DSN to run the integration suite): %v",
			testdb.EnvBootstrapDSN, err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := h.Teardown(ctx); err != nil {
			t.Errorf("teardown: %v", err)
		}
	})

	if err := h.Provision(ctx); err != nil {
		t.Fatalf("provision: %v", err)
	}
	res, err := h.Converge(ctx)
	if err != nil {
		t.Fatalf("converge: %v", err)
	}
	if len(res.Applied) != 6 {
		t.Fatalf("converge applied %d migrations, want 6", len(res.Applied))
	}

	runtimeConn, rerr := h.RuntimeConn(ctx)
	if rerr != nil {
		t.Fatalf("runtime connect: %v", rerr)
	}
	connCfg := runtimeConn.Config()
	runtimeConn.Close(ctx)
	pc, perr := pgxpool.ParseConfig(
		fmt.Sprintf("host=%s port=%d dbname=%s user=%s sslmode=%s",
			connCfg.Host, connCfg.Port, connCfg.Database, connCfg.User,
			sslModeForAPITest(connCfg)))
	if perr != nil {
		t.Fatalf("parse runtime pool config: %v", perr)
	}
	pool, perr := pgxpool.NewWithConfig(ctx, pc)
	if perr != nil {
		t.Fatalf("open runtime pool: %v", perr)
	}
	t.Cleanup(pool.Close)

	bootConn, berr := h.BootstrapConn(ctx, testdb.DatabaseName)
	if berr != nil {
		t.Fatalf("bootstrap connect: %v", berr)
	}
	t.Cleanup(func() { bootConn.Close(context.Background()) })

	fx := &apiFixture{
		cfg: &config.Config{
			AdminToken: "fixture-admin-token-0123456789abcdef0123456789abcdef",
		},
		srv:  nil,
		boot: bootConn,
	}
	// Set operation bounds so the router's chain has sane limits.
	fx.cfg.HTTPMaxBodyBytes = config.DefaultHTTPMaxBodyBytes
	fx.cfg.HTTPRequestTimeout = config.DefaultHTTPRequestTimeout
	fx.cfg.UpsertMaxRecords = config.DefaultUpsertMaxRecords
	fx.cfg.SearchMaxLimit = config.DefaultSearchMaxLimit
	fx.cfg.MaxFilters = config.DefaultMaxFilters
	fx.cfg.MaxFilterValues = config.DefaultMaxFilterValues
	fx.cfg.MaxMetadataBytes = config.DefaultMaxMetadataBytes

	fx.srv = httptest.NewServer(NewRouter(fx.cfg, Deps{Pool: pool}))
	t.Cleanup(func() { fx.srv.Close() })
	return fx
}

// insertApplication registers an application row through bootstrap SQL.
func (f *apiFixture) insertApplication(ctx context.Context, id, key string) error {
	_, err := f.boot.Exec(ctx,
		`INSERT INTO vector_control.applications (id, application_key, display_name)
		 VALUES ($1, $2, $3)`,
		id, key, key+" (fixture)")
	return err
}

// insertNamespace registers a namespace row through bootstrap SQL.
func (f *apiFixture) insertNamespace(ctx context.Context, appID, key string) error {
	id, err := testdb.NewUUID()
	if err != nil {
		return err
	}
	_, err = f.boot.Exec(ctx,
		`INSERT INTO vector_control.namespaces (id, application_id, namespace_key, display_name)
		 VALUES ($1, $2, $3, $4)`,
		id, appID, key, key+" (fixture)")
	return err
}

// insertCredential seeds a data-plane credential row for the named
// application with the given generated secret.
func (f *apiFixture) insertCredential(ctx context.Context, appID, credential string) error {
	id, err := testdb.NewUUID()
	if err != nil {
		return err
	}
	digest := auth.Digest(credential)
	_, err = f.boot.Exec(ctx,
		`INSERT INTO vector_control.application_credentials
			(id, application_id, credential_name, credential_hash, enabled)
		 VALUES ($1, $2, 'fixture-credential', $3, true)`,
		id, appID, digest[:])
	return err
}

// do performs an HTTP request against the fixture server and returns the
// response status, headers, and body bytes.
func (f *apiFixture) do(t *testing.T, method, path, bearer string, body []byte) (int, http.Header, []byte) {
	t.Helper()
	var reader io.Reader
	if body != nil {
		reader = strings.NewReader(string(body))
	}
	// Use the server URL to construct a proper absolute URL. httptest.NewRequest
	// creates a request with a relative URL (no scheme), which causes
	// Client().Do to fail with "unsupported protocol scheme".
	req, err := http.NewRequest(method, f.srv.URL+path, reader)
	if err != nil {
		t.Fatalf("%s %s: build request: %v", method, path, err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := f.srv.Client().Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("%s %s: read body: %v", method, path, err)
	}
	return resp.StatusCode, resp.Header, data
}

// apiErr is the error envelope of docs/API.md section 9.
type apiErr struct {
	Error struct {
		Code       string `json:"code"`
		Message    string `json:"message"`
		TraceID    string `json:"trace_id,omitempty"`
		RetryAfter int    `json:"retry_after,omitempty"`
	} `json:"error"`
}

// expectError asserts the response is a JSON error envelope with the
// expected status and error code.
func expectError(t *testing.T, got int, body []byte, wantStatus int, wantCode string) {
	t.Helper()
	if got != wantStatus {
		t.Fatalf("status = %d, want %d; body: %s", got, wantStatus, string(body))
	}
	var env apiErr
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("response is not an error envelope: %v; body: %s", err, string(body))
	}
	if env.Error.Code != wantCode {
		t.Fatalf("error code = %q, want %q; body: %s", env.Error.Code, wantCode, string(body))
	}
}

// upsertPayload is a deterministic upsert body for the seeded space.
func upsertPayload(objectID, projectionID, contentHash string) []byte {
	emb := make([]float32, testdb.SeededSpaceDimensions)
	emb[0] = 1
	body, _ := json.Marshal(map[string]any{
		"vector_space": testdb.SeededSpaceKey,
		"records": []map[string]any{
			{
				"object_id":         objectID,
				"projection_id":     projectionID,
				"content_hash":      contentHash,
				"source_updated_at": "2026-01-01T00:00:00Z",
				"metadata":          map[string]any{"state": "active"},
				"vector":            emb,
			},
		},
	})
	return body
}

// searchPayload is a deterministic search body.
func searchPayload(limit int) []byte {
	emb := make([]float32, testdb.SeededSpaceDimensions)
	emb[0] = 1
	body, _ := json.Marshal(map[string]any{
		"vector_space": testdb.SeededSpaceKey,
		"vector":       emb,
		"limit":        limit,
	})
	return body
}

// deleteProjectionPayload is a deterministic projection-delete body.
func deleteProjectionPayload(objectID, projectionID string) []byte {
	body, _ := json.Marshal(map[string]any{
		"object_id":     objectID,
		"projection_id": projectionID,
		"vector_space":  testdb.SeededSpaceKey,
	})
	return body
}

// TestRouterHealthAndRouting is the non-DB surface of the composed router:
// healthz and readyz over HTTP, an unknown path is a 404 with a JSON error
// envelope, and a wrong method on a known path is a 405 with the Allow
// header.
func TestRouterHealthAndRouting(t *testing.T) {
	fx := setupAPIFixture(t)
	_ = context.Background()

	// readyz: the MigrationsApplied func is nil in the test, so it returns
	// 503 unavailable. healthz always returns 200.
	status, _, body := fx.do(t, http.MethodGet, "/healthz", "", nil)
	if status != http.StatusOK {
		t.Fatalf("healthz status = %d, want 200; body: %s", status, string(body))
	}
	if !strings.Contains(string(body), `"status":"ok"`) {
		t.Fatalf("healthz body = %s, want a status ok envelope", string(body))
	}

	// readyz with nil MigrationsApplied: 503 unavailable.
	status, _, body = fx.do(t, http.MethodGet, "/readyz", "", nil)
	if status != http.StatusServiceUnavailable {
		t.Fatalf("readyz status = %d, want 503 (MigrationsApplied nil); body: %s", status, string(body))
	}
	expectError(t, status, body, http.StatusServiceUnavailable, "unavailable")

	// Unknown path: 404 not_found.
	status, _, body = fx.do(t, http.MethodGet, "/definitely/not/here", "", nil)
	expectError(t, status, body, http.StatusNotFound, "not_found")

	// Wrong method on a known data path: 405 method_not_allowed with Allow.
	status, hdr, body := fx.do(t, http.MethodGet, "/v1/namespaces/project-alpha/records", "", nil)
	if status != http.StatusMethodNotAllowed {
		t.Fatalf("GET on a PUT/DELETE path status = %d, want 405; body: %s", status, string(body))
	}
	if got := hdr.Get("Allow"); got != "PUT, DELETE" {
		t.Fatalf("Allow header = %q, want 'PUT, DELETE'", got)
	}
	expectError(t, status, body, http.StatusMethodNotAllowed, "method_not_allowed")

	// Wrong method on a known admin path: 405.
	status, hdr, body = fx.do(t, http.MethodGet, "/v1/admin/applications", "", nil)
	if status != http.StatusMethodNotAllowed {
		t.Fatalf("GET on admin POST path status = %d, want 405; body: %s", status, string(body))
	}
	if got := hdr.Get("Allow"); got != "POST" {
		t.Fatalf("Allow header = %q, want POST", got)
	}
	expectError(t, status, body, http.StatusMethodNotAllowed, "method_not_allowed")
}

// TestRouterUpsertGetSearchRoundTrip is the core data-plane round trip over
// the composed router: a PUT upsert writes a record, a GET fetches it back
// (proving the auth stage's app ID reaches the handler and service), and a
// POST search returns it with the expected score.
func TestRouterUpsertGetSearchRoundTrip(t *testing.T) {
	fx := setupAPIFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	appA := "0199f31e-5000-7000-8000-0000000000a1"
	if err := fx.insertApplication(ctx, appA, testdb.AppMemoryService); err != nil {
		t.Fatalf("register application A: %v", err)
	}
	if err := fx.insertNamespace(ctx, appA, "project-alpha"); err != nil {
		t.Fatalf("register namespace: %v", err)
	}
	credA, err := auth.GenerateCredential()
	if err != nil {
		t.Fatalf("generate credential: %v", err)
	}
	if err := fx.insertCredential(ctx, appA, credA); err != nil {
		t.Fatalf("seed credential: %v", err)
	}

	objID, err := testdb.NewUUID()
	if err != nil {
		t.Fatalf("fixture object id: %v", err)
	}
	projID, err := testdb.NewUUID()
	if err != nil {
		t.Fatalf("fixture projection id: %v", err)
	}

	// Upsert through the HTTP surface.
	status, _, body := fx.do(t, http.MethodPut,
		"/v1/namespaces/project-alpha/records", credA,
		upsertPayload(objID, projID, strings.Repeat("1", 64)))
	if status != http.StatusOK {
		t.Fatalf("upsert status = %d, want 200; body: %s", status, string(body))
	}
	var up struct {
		Upserted  int `json:"upserted"`
		Unchanged int `json:"unchanged"`
	}
	if err := json.Unmarshal(body, &up); err != nil {
		t.Fatalf("unmarshal upsert response: %v; body: %s", err, string(body))
	}
	if up.Upserted != 1 || up.Unchanged != 0 {
		t.Fatalf("upsert result = %+v, want upserted 1 / unchanged 0", up)
	}

	// Find the record ID by reading the database (the upsert response does
	// not return the record ID; the service generates it).
	var recordID string
	if err := fx.boot.QueryRow(ctx,
		`SELECT id FROM vector_data.vector_records
		 WHERE application_id = $1 AND object_id = $2`,
		appA, objID).Scan(&recordID); err != nil {
		t.Fatalf("read record id: %v", err)
	}

	// Get it back: the auth stage must have attached application A's ID to
	// the context, otherwise the service's WithAppContext would fail.
	status, _, body = fx.do(t, http.MethodGet,
		fmt.Sprintf("/v1/namespaces/project-alpha/records/%s", recordID), credA, nil)
	if status != http.StatusOK {
		t.Fatalf("get status = %d, want 200; body: %s", status, string(body))
	}
	var rec struct {
		RecordID     string         `json:"record_id"`
		ObjectID     string         `json:"object_id"`
		ProjectionID string         `json:"projection_id"`
		VectorSpace  string         `json:"vector_space"`
		ContentHash  string         `json:"content_hash"`
		Metadata     map[string]any `json:"metadata"`
	}
	if err := json.Unmarshal(body, &rec); err != nil {
		t.Fatalf("unmarshal get response: %v; body: %s", err, string(body))
	}
	if rec.ObjectID != objID || rec.ProjectionID != projID {
		t.Fatalf("get returned object %s / projection %s, want %s / %s",
			rec.ObjectID, rec.ProjectionID, objID, projID)
	}
	if rec.ContentHash != strings.Repeat("1", 64) {
		t.Fatalf("get content hash = %q, want the seeded hash", rec.ContentHash)
	}
	if rec.Metadata["state"] != "active" {
		t.Fatalf("get metadata = %v, want state active", rec.Metadata)
	}

	// Search returns the record (the query is the record's own vector, so
	// the score should be ~1 for cosine).
	status, _, body = fx.do(t, http.MethodPost,
		"/v1/namespaces/project-alpha/search", credA, searchPayload(5))
	if status != http.StatusOK {
		t.Fatalf("search status = %d, want 200; body: %s", status, string(body))
	}
	var sr struct {
		Matches []struct {
			RecordID     string         `json:"record_id"`
			ObjectID     string         `json:"object_id"`
			ProjectionID string         `json:"projection_id"`
			Score        float64        `json:"score"`
			Metadata     map[string]any `json:"metadata"`
		} `json:"matches"`
	}
	if err := json.Unmarshal(body, &sr); err != nil {
		t.Fatalf("unmarshal search response: %v; body: %s", err, string(body))
	}
	if len(sr.Matches) != 1 {
		t.Fatalf("search returned %d matches, want 1; body: %s", len(sr.Matches), string(body))
	}
	if sr.Matches[0].ObjectID != objID {
		t.Fatalf("search match = %s, want %s", sr.Matches[0].ObjectID, objID)
	}
	if sr.Matches[0].Score < 0.99 {
		t.Fatalf("search score = %f, want ~1 (same vector)", sr.Matches[0].Score)
	}
}

// TestRouterIsolationAndAuthFailures proves the composed router's
// authentication and isolation boundaries: an absent bearer token is a 401;
// an unknown token is a 401; a request for an unregistered namespace is a
// 404 namespace_not_found; and application B cannot read application A's
// record even though both applications own a namespace named project-alpha.
func TestRouterIsolationAndAuthFailures(t *testing.T) {
	fx := setupAPIFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	appA := "0199f31e-5000-7000-8000-0000000000b1"
	appB := "0199f31e-5000-7000-8000-0000000000b2"
	if err := fx.insertApplication(ctx, appA, testdb.AppMemoryService); err != nil {
		t.Fatalf("register application A: %v", err)
	}
	if err := fx.insertApplication(ctx, appB, testdb.AppNotesService); err != nil {
		t.Fatalf("register application B: %v", err)
	}
	if err := fx.insertNamespace(ctx, appA, "project-alpha"); err != nil {
		t.Fatalf("register application A namespace: %v", err)
	}
	if err := fx.insertNamespace(ctx, appB, "project-alpha"); err != nil {
		t.Fatalf("register application B namespace (same key): %v", err)
	}
	credA, err := auth.GenerateCredential()
	if err != nil {
		t.Fatalf("generate credential A: %v", err)
	}
	credB, err := auth.GenerateCredential()
	if err != nil {
		t.Fatalf("generate credential B: %v", err)
	}
	if err := fx.insertCredential(ctx, appA, credA); err != nil {
		t.Fatalf("seed application A credential: %v", err)
	}
	if err := fx.insertCredential(ctx, appB, credB); err != nil {
		t.Fatalf("seed application B credential: %v", err)
	}

	objID, err := testdb.NewUUID()
	if err != nil {
		t.Fatalf("fixture object id: %v", err)
	}
	projID, err := testdb.NewUUID()
	if err != nil {
		t.Fatalf("fixture projection id: %v", err)
	}
	// Seed application A's record directly (the service layer's own
	// integration suite already covers the upsert contract; here the point
	// is cross-application isolation at the HTTP boundary).
	// Generate a valid embedding vector literal for the seeded space dimensions.
	embeddingLit := "[" + strings.Repeat("0.1,", testdb.SeededSpaceDimensions-1) + "0.1]"
	_, err = fx.boot.Exec(ctx, `
		INSERT INTO vector_data.vector_records
			(id, application_id, namespace_id, object_id, projection_id,
			 vector_space_id, content_hash, source_updated_at, metadata, embedding)
		SELECT
			gen_random_uuid(), $1, n.id, $2, $3,
			vs.id, decode($4, 'hex'), $5, '{}'::jsonb, $7::vector
		FROM vector_control.namespaces n
		JOIN vector_control.vector_spaces vs ON vs.vector_space_key = $6
		WHERE n.application_id = $1 AND n.namespace_key = 'project-alpha'`,
		appA, objID, projID, strings.Repeat("1", 64),
		"2026-01-01T00:00:00Z", testdb.SeededSpaceKey, embeddingLit)
	if err != nil {
		t.Fatalf("seed application A record: %v", err)
	}
	var recordID string
	if err := fx.boot.QueryRow(ctx,
		`SELECT id FROM vector_data.vector_records
		 WHERE application_id = $1 AND object_id = $2`,
		appA, objID).Scan(&recordID); err != nil {
		t.Fatalf("read record id: %v", err)
	}

	t.Run("no bearer token is 401", func(t *testing.T) {
		status, _, body := fx.do(t, http.MethodGet,
			fmt.Sprintf("/v1/namespaces/project-alpha/records/%s", recordID), "", nil)
		expectError(t, status, body, http.StatusUnauthorized, "unauthorized")
	})

	t.Run("unknown bearer token is 401", func(t *testing.T) {
		unknown, err := auth.GenerateCredential()
		if err != nil {
			t.Fatalf("generate unknown credential: %v", err)
		}
		status, _, body := fx.do(t, http.MethodGet,
			fmt.Sprintf("/v1/namespaces/project-alpha/records/%s", recordID),
			unknown, nil)
		expectError(t, status, body, http.StatusUnauthorized, "unauthorized")
	})

	t.Run("unregistered namespace is 404", func(t *testing.T) {
		status, _, body := fx.do(t, http.MethodGet,
			"/v1/namespaces/archive/records/"+recordID, credA, nil)
		expectError(t, status, body, http.StatusNotFound, "namespace_not_found")
	})

	t.Run("application B cannot read application A record", func(t *testing.T) {
		// Application B owns a namespace with the same key but has no
		// record for the object, so the get must return record_not_found,
		// not application A's record.
		status, _, body := fx.do(t, http.MethodGet,
			fmt.Sprintf("/v1/namespaces/project-alpha/records/%s", recordID), credB, nil)
		expectError(t, status, body, http.StatusNotFound, "record_not_found")
	})
}

// TestRouterDeleteSurfaces exercises the composed router's delete
// operations: a projection delete removes the named space's record (200),
// and an object delete removes every space's record.
func TestRouterDeleteSurfaces(t *testing.T) {
	fx := setupAPIFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	appA := "0199f31e-5000-7000-8000-0000000000c1"
	if err := fx.insertApplication(ctx, appA, testdb.AppMemoryService); err != nil {
		t.Fatalf("register application A: %v", err)
	}
	if err := fx.insertNamespace(ctx, appA, "project-alpha"); err != nil {
		t.Fatalf("register namespace: %v", err)
	}
	credA, err := auth.GenerateCredential()
	if err != nil {
		t.Fatalf("generate credential: %v", err)
	}
	if err := fx.insertCredential(ctx, appA, credA); err != nil {
		t.Fatalf("seed credential: %v", err)
	}

	objID, err := testdb.NewUUID()
	if err != nil {
		t.Fatalf("fixture object id: %v", err)
	}
	projID, err := testdb.NewUUID()
	if err != nil {
		t.Fatalf("fixture projection id: %v", err)
	}

	// Seed a record in the seeded space.
	status, _, body := fx.do(t, http.MethodPut,
		"/v1/namespaces/project-alpha/records", credA,
		upsertPayload(objID, projID, strings.Repeat("1", 64)))
	if status != http.StatusOK {
		t.Fatalf("upsert status = %d, want 200; body: %s", status, string(body))
	}

	t.Run("projection delete", func(t *testing.T) {
		status, _, body := fx.do(t, http.MethodDelete,
			"/v1/namespaces/project-alpha/records", credA,
			deleteProjectionPayload(objID, projID))
		if status != http.StatusOK {
			t.Fatalf("projection delete status = %d, want 200; body: %s", status, string(body))
		}
		var del struct {
			Deleted int `json:"deleted"`
		}
		if err := json.Unmarshal(body, &del); err != nil {
			t.Fatalf("unmarshal delete response: %v; body: %s", err, string(body))
		}
		if del.Deleted != 1 {
			t.Fatalf("projection delete Deleted = %d, want 1", del.Deleted)
		}
	})

	t.Run("projection delete of a missing projection is idempotent", func(t *testing.T) {
		status, _, body := fx.do(t, http.MethodDelete,
			"/v1/namespaces/project-alpha/records", credA,
			deleteProjectionPayload(objID, projID))
		if status != http.StatusOK {
			t.Fatalf("idempotent delete status = %d, want 200; body: %s", status, string(body))
		}
		var del struct {
			Deleted int `json:"deleted"`
		}
		if err := json.Unmarshal(body, &del); err != nil {
			t.Fatalf("unmarshal delete response: %v; body: %s", err, string(body))
		}
		if del.Deleted != 0 {
			t.Fatalf("idempotent delete Deleted = %d, want 0", del.Deleted)
		}
	})

	// Re-seed and object-delete.
	_, _, _ = fx.do(t, http.MethodPut,
		"/v1/namespaces/project-alpha/records", credA,
		upsertPayload(objID, projID, strings.Repeat("1", 64)))

	t.Run("object delete", func(t *testing.T) {
		status, _, body := fx.do(t, http.MethodDelete,
			fmt.Sprintf("/v1/namespaces/project-alpha/objects/%s", objID), credA, nil)
		if status != http.StatusOK {
			t.Fatalf("object delete status = %d, want 200; body: %s", status, string(body))
		}
		var del struct {
			Deleted int `json:"deleted"`
		}
		if err := json.Unmarshal(body, &del); err != nil {
			t.Fatalf("unmarshal delete response: %v; body: %s", err, string(body))
		}
		if del.Deleted != 1 {
			t.Fatalf("object delete Deleted = %d, want 1", del.Deleted)
		}
	})
}

// TestRouterAdminSurfaces exercises the composed router's admin surface: a
// non-admin token on an admin route is a 401; the admin token is NOT a
// valid data-plane credential; the register-namespace and create-credential
// admin operations succeed with 200 and a well-formed body; and a
// data-plane request never gains access through the admin credential.
func TestRouterAdminSurfaces(t *testing.T) {
	fx := setupAPIFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	adminToken := fx.cfg.AdminToken

	// Register application through the admin plane (not bootstrap), so
	// the admin surface is the real path.
	status, _, body := fx.do(t, http.MethodPost, "/v1/admin/applications",
		adminToken,
		[]byte(`{"application_key":"memory-service","display_name":"Memory Service (fixture)"}`))
	if status != http.StatusOK {
		t.Fatalf("register application status = %d, want 200; body: %s", status, string(body))
	}
	var app struct {
		ID             string `json:"id"`
		ApplicationKey string `json:"application_key"`
		DisplayName    string `json:"display_name"`
		Enabled        bool   `json:"enabled"`
	}
	if err := json.Unmarshal(body, &app); err != nil {
		t.Fatalf("unmarshal application: %v; body: %s", err, string(body))
	}
	if app.ApplicationKey != "memory-service" {
		t.Fatalf("application key = %q, want memory-service", app.ApplicationKey)
	}
	if app.ID == "" {
		t.Fatal("application ID is empty")
	}

	// A non-admin token is rejected on an admin route.
	otherToken, err := auth.GenerateCredential()
	if err != nil {
		t.Fatalf("generate non-admin credential: %v", err)
	}
	status, _, body = fx.do(t, http.MethodPost, "/v1/admin/applications",
		otherToken, []byte(`{"application_key":"notes-service","display_name":"x"}`))
	expectError(t, status, body, http.StatusUnauthorized, "unauthorized")

	// The admin token is NOT a valid data-plane credential: a data request
	// with the admin token is a 401 (the data auth stage requires a
	// canonical secret and a stored digest; the admin token has no row).
	status, _, body = fx.do(t, http.MethodGet,
		"/v1/namespaces/project-alpha/records/"+mustUUID(),
		adminToken, nil)
	expectError(t, status, body, http.StatusUnauthorized, "unauthorized")

	// Register a namespace through the admin plane.
	status, _, body = fx.do(t, http.MethodPost,
		fmt.Sprintf("/v1/admin/applications/%s/namespaces", app.ApplicationKey),
		adminToken,
		[]byte(`{"namespace_key":"project-alpha","display_name":"Project Alpha (fixture)"}`))
	if status != http.StatusOK {
		t.Fatalf("register namespace status = %d, want 200; body: %s", status, string(body))
	}
	var ns struct {
		ID             string `json:"id"`
		ApplicationKey string `json:"application_key"`
		NamespaceKey   string `json:"namespace_key"`
		DisplayName    string `json:"display_name"`
		Enabled        bool   `json:"enabled"`
	}
	if err := json.Unmarshal(body, &ns); err != nil {
		t.Fatalf("unmarshal namespace: %v; body: %s", err, string(body))
	}
	if ns.NamespaceKey != "project-alpha" {
		t.Fatalf("namespace key = %q, want project-alpha", ns.NamespaceKey)
	}

	// Create a credential through the admin plane and use it on a data
	// route (proving the admin-created credential is a working data-plane
	// secret end to end).
	status, _, body = fx.do(t, http.MethodPost,
		fmt.Sprintf("/v1/admin/applications/%s/credentials", app.ApplicationKey),
		adminToken,
		[]byte(`{"credential_name":"fixture-api-credential"}`))
	if status != http.StatusOK {
		t.Fatalf("create credential status = %d, want 200; body: %s", status, string(body))
	}
	var cr struct {
		CredentialID   string `json:"credential_id"`
		CredentialName string `json:"credential_name"`
		Credential     string `json:"credential"`
	}
	if err := json.Unmarshal(body, &cr); err != nil {
		t.Fatalf("unmarshal credential: %v; body: %s", err, string(body))
	}
	if cr.CredentialName != "fixture-api-credential" || cr.Credential == "" {
		t.Fatalf("credential = %+v, want name fixture-api-credential and a non-empty secret", cr)
	}

	// The admin-created credential works on the data plane: upsert a record
	// in the newly registered namespace.
	objID, err := testdb.NewUUID()
	if err != nil {
		t.Fatalf("fixture object id: %v", err)
	}
	projID, err := testdb.NewUUID()
	if err != nil {
		t.Fatalf("fixture projection id: %v", err)
	}
	status, _, body = fx.do(t, http.MethodPut,
		"/v1/namespaces/project-alpha/records", cr.Credential,
		upsertPayload(objID, projID, strings.Repeat("1", 64)))
	if status != http.StatusOK {
		t.Fatalf("data upsert with admin-created credential status = %d, want 200; body: %s", status, string(body))
	}

	// Disable the credential through the admin plane; the credential then
	// stops authenticating (401).
	status, _, body = fx.do(t, http.MethodDelete,
		fmt.Sprintf("/v1/admin/applications/%s/credentials/%s", app.ApplicationKey, cr.CredentialID),
		adminToken, nil)
	if status != http.StatusOK {
		t.Fatalf("disable credential status = %d, want 200; body: %s", status, string(body))
	}
	var dresp struct {
		Disabled bool `json:"disabled"`
	}
	if err := json.Unmarshal(body, &dresp); err != nil {
		t.Fatalf("unmarshal disable response: %v; body: %s", err, string(body))
	}
	if !dresp.Disabled {
		t.Fatalf("disable response = %+v, want disabled true", dresp)
	}
	status, _, body = fx.do(t, http.MethodGet,
		"/v1/namespaces/project-alpha/records/"+mustUUID(),
		cr.Credential, nil)
	expectError(t, status, body, http.StatusUnauthorized, "unauthorized")

	_ = ctx
}

// sslModeForAPITest returns the sslmode for the test pool.
// The harness always uses TLSModePlain (VEC_PG_TLS_MODE=plain), so test
// pools must explicitly disable SSL.
func sslModeForAPITest(_ *pgx.ConnConfig) string {
	return "disable"
}

// mustUUID is a test helper that panics on failure (the integration test
// has no error path for UUID generation).
func mustUUID() string {
	id, err := testdb.NewUUID()
	if err != nil {
		panic(err)
	}
	return id
}
