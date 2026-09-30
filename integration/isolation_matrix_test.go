//go:build integration

// TestIsolationMatrix is the integration test that pins the eight
// application/namespace isolation behaviors documented in
// docs/DEVELOPMENT.md (the isolation matrix) and
// docs/implementation/package-5-verification-runtime.md.
//
// Cases 1-6 run through the production HTTP chain (real bearer
// authentication, the real router, real service functions) against the
// provisioned test database. Case 5 pins the RLS namespace-discovery
// boundary directly on the runtime pool. Case 7 is a database-integration
// test: it issues vector_api SQL without any set_config context and relies
// on the FORCE RLS policies to hide both applications' rows. Case 8
// exercises pool reuse directly on the runtime pool: transaction-local
// set_config must not leak an application context across transactions on
// the same physical connection.
//
// Fixture convention: two synthetic applications (memory-service with
// namespaces project-alpha and research; notes-service with namespace
// project-beta), provisioned through the administrative API plane.
package integration

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"vector-service/internal/api"
	"vector-service/internal/config"
	"vector-service/internal/testdb"
)

const isoSpaceKey = testdb.SeededSpaceKey // seeded by migration 0002, shared by all apps

// ---------------------------------------------------------------------
// HTTP helpers
// ---------------------------------------------------------------------

type isoResponse struct {
	status    int
	body      string
	requestID string
}

// doHTTP issues a request against the test server and records the response
// status, body, and request ID. payload may be nil (no body).
func doHTTP(t *testing.T, client *http.Client, srv *httptest.Server, method, path, cred string, payload any) isoResponse {
	t.Helper()
	var body io.Reader
	if payload != nil {
		data, err := json.Marshal(payload)
		if err != nil {
			t.Fatalf("isolation matrix: marshal request body: %v", err)
		}
		body = bytes.NewReader(data)
	}
	req, err := http.NewRequest(method, srv.URL+path, body)
	if err != nil {
		t.Fatalf("isolation matrix: build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if cred != "" {
		req.Header.Set("Authorization", "Bearer "+cred)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("isolation matrix: issue %s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("isolation matrix: read response body: %v", err)
	}
	return isoResponse{
		status:    resp.StatusCode,
		body:      string(data),
		requestID: resp.Header.Get(api.RequestIDHeader),
	}
}

// ---------------------------------------------------------------------
// Provisioning: the full production chain over the provisioned database
// ---------------------------------------------------------------------

// isoServer serves the real production router (full middleware chain, real
// data-plane bearer authentication, real admin-token authorization) against
// the provisioned test database as the vector_api runtime role. It returns
// the server, the runtime pool (shared with the test for direct DB checks),
// and the admin token plus a cleanup.
func isoServer(t *testing.T, h *testdb.Harness) (*httptest.Server, *pgxpool.Pool, string, func()) {
	t.Helper()
	env, err := h.RuntimeEnv()
	if err != nil {
		t.Fatalf("isolation matrix: runtime env: %v", err)
	}
	for k, v := range env {
		t.Setenv(k, v)
	}
	// The harness env carries 127.0.0.1:0 (no listener); the router is
	// served by the httptest server, so any valid port satisfies config
	// validation.
	t.Setenv(config.EnvListenAddr, "127.0.0.1:40000")
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("isolation matrix: config.Load: %v", err)
	}

	// The runtime pool connects as vector_api (the role the service runs as
	// in production). Build it from the harness runtime connection config —
	// the same pattern the logging fixture uses.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	rconn, err := h.RuntimeConn(ctx)
	if err != nil {
		t.Fatalf("isolation matrix: runtime connection: %v", err)
	}
	cc := rconn.Config()
	poolCfg, err := pgxpool.ParseConfig(
		fmt.Sprintf("host=%s port=%d dbname=%s user=%s sslmode=disable",
			cc.Host, cc.Port, cc.Database, cc.User))
	if err != nil {
		rconn.Close(ctx)
		t.Fatalf("isolation matrix: parse pool config: %v", err)
	}
	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		rconn.Close(ctx)
		t.Fatalf("isolation matrix: open pool: %v", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		rconn.Close(ctx)
		t.Fatalf("isolation matrix: pool ping: %v", err)
	}
	rconn.Close(ctx)

	dps := &api.Deps{
		Pool:              pool,
		MigrationsApplied: func() bool { return true },
	}
	srv := httptest.NewServer(api.NewRouterShared(cfg, dps))

	cleanup := func() {
		srv.Close()
		pool.Close()
	}
	return srv, pool, cfg.AdminToken, cleanup
}

// ---------------------------------------------------------------------
// Fixture: two applications provisioned through the admin plane
// ---------------------------------------------------------------------

// isoApp is a provisioned application and its data-plane credential. The
// IDs are the canonical string form (the admin responses return them as
// strings; the test never needs a typed UUID).
type isoApp struct {
	key  string
	id   string
	cred string
}

// registerApp provisions one application, its namespace, and one data-plane
// credential through the administrative API plane — the same surface an
// operator uses — and returns the application identity and the generated
// credential.
func registerApp(t *testing.T, client *http.Client, srv *httptest.Server, adminToken, appKey, nsKey string) isoApp {
	t.Helper()

	res := doHTTP(t, client, srv, http.MethodPost, "/v1/admin/applications", adminToken,
		map[string]string{"application_key": appKey, "display_name": appKey})
	if res.status != http.StatusOK {
		t.Fatalf("isolation matrix: register app %s: status %d body %s", appKey, res.status, res.body)
	}
	var app struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(res.body), &app); err != nil {
		t.Fatalf("isolation matrix: decode app %s: %v (body %s)", appKey, err, res.body)
	}
	if app.ID == "" {
		t.Fatalf("isolation matrix: empty app id for %s", appKey)
	}

	// The admin plane resolves {application} by application key (docs/API.md).
	ns := doHTTP(t, client, srv, http.MethodPost,
		"/v1/admin/applications/"+appKey+"/namespaces", adminToken,
		map[string]string{"namespace_key": nsKey, "display_name": nsKey})
	if ns.status != http.StatusOK {
		t.Fatalf("isolation matrix: create namespace %s for %s: status %d body %s", nsKey, appKey, ns.status, ns.body)
	}

	cred := doHTTP(t, client, srv, http.MethodPost,
		"/v1/admin/applications/"+appKey+"/credentials", adminToken,
		map[string]string{"credential_name": "iso-matrix-credential"})
	if cred.status != http.StatusOK {
		t.Fatalf("isolation matrix: create credential for %s: status %d body %s", appKey, cred.status, cred.body)
	}
	var credOut struct {
		Credential string `json:"credential"`
	}
	if err := json.Unmarshal([]byte(cred.body), &credOut); err != nil {
		t.Fatalf("isolation matrix: decode credential %s: %v (body %s)", appKey, err, cred.body)
	}
	if credOut.Credential == "" {
		t.Fatalf("isolation matrix: empty credential for %s", appKey)
	}

	return isoApp{key: appKey, id: app.ID, cred: credOut.Credential}
}

// recordIDsByApp reads the record UUIDs belonging to one application from
// the provisioned database (bootstrap role, superuser: no RLS scoping).
// The upsert endpoint does not return record IDs, so cross-application read
// and delete attempts need them.
func recordIDsByApp(t *testing.T, h *testdb.Harness, appID string) []string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, err := h.BootstrapConn(ctx, testdb.DatabaseName)
	if err != nil {
		t.Fatalf("isolation matrix: bootstrap connection: %v", err)
	}
	defer conn.Close(ctx)
	rows, err := conn.Query(ctx,
		`SELECT id FROM vector_data.vector_records WHERE application_id = $1 ORDER BY id`,
		appID)
	if err != nil {
		t.Fatalf("isolation matrix: query record ids: %v", err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("isolation matrix: scan record id: %v", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("isolation matrix: record ids rows: %v", err)
	}
	return ids
}

// isoStringSlicesEqual reports whether two string slices are equal element
// by element. It is used by Case6 to compare record-identity snapshots
// before and after nonexistent-namespace operations.
func isoStringSlicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// ---------------------------------------------------------------------
// Fixture vectors and metadata
// ---------------------------------------------------------------------

// contentHashOf produces a deterministic 32-byte (64-hex) content hash for
// a fixture record. The value is synthetic and stable across runs.
func contentHashOf(app, namespace string, ordinal int) string {
	var b strings.Builder
	for i := 0; i < 32; i++ {
		fmt.Fprintf(&b, "%02x", (i+ordinal)%256)
	}
	return b.String()
}

// upsertPayload is the strict PUT /v1/namespaces/{ns}/records body for one
// fixture record.
func upsertPayload(objID, projID, spaceKey string, vec []float32, hash string, meta map[string]any) map[string]any {
	return map[string]any{
		"vector_space": spaceKey,
		"records": []any{
			map[string]any{
				"object_id":     objID,
				"projection_id": projID,
				"content_hash":  hash,
				"metadata":      meta,
				"vector":        vec,
			},
		},
	}
}

// ---------------------------------------------------------------------
// The matrix
// ---------------------------------------------------------------------

// TestIsolationMatrix pins the eight application/namespace isolation
// behaviors (docs/DEVELOPMENT.md). It skips when the bootstrap identity is
// not configured (no database on the host), mirroring the logging fixture.
func TestIsolationMatrix(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	h, err := testdb.New(ctx)
	if err != nil {
		t.Skipf("testdb: bootstrap identity not configured (set %s to a superuser DSN to run the integration suite): %v",
			testdb.EnvBootstrapDSN, err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
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

	srv, pool, adminToken, cleanup := isoServer(t, h)
	defer cleanup()

	client := srv.Client()
	fx := testdb.NewIsolationFixture()

	a := registerApp(t, client, srv, adminToken, fx.AppA, "project-alpha")
	// The fixture gives A a second namespace (research) for the
	// namespace-axis cases.
	if r := doHTTP(t, client, srv, http.MethodPost,
		"/v1/admin/applications/"+a.key+"/namespaces", adminToken,
		map[string]string{"namespace_key": "research", "display_name": "research"}); r.status != http.StatusOK {
		t.Fatalf("isolation matrix: create second namespace for A: status %d body %s", r.status, r.body)
	}
	b := registerApp(t, client, srv, adminToken, fx.AppB, "project-beta")

	// Seed one record per application through the production upsert
	// endpoint (real authentication, real service path, RLS-scoped
	// transaction). The vectors are basis vectors 0 and 1 (orthogonal), so
	// cross-application similarity search is unambiguously empty even if
	// the distance layer were misapplied. Both applications use the single
	// seeded space (migration 0002) — spaces are application-agnostic.
	objA := testdb.DeterministicObjectID(fx.AppA, 101)
	projA := testdb.DeterministicProjectionID(fx.AppA, 101, 1)
	objB := testdb.DeterministicObjectID(fx.AppB, 202)
	projB := testdb.DeterministicProjectionID(fx.AppB, 202, 1)

	upA := doHTTP(t, client, srv, http.MethodPut,
		"/v1/namespaces/project-alpha/records", a.cred,
		upsertPayload(objA, projA, isoSpaceKey, fx.VectorsA, contentHashOf(fx.AppA, "project-alpha", 1), fx.MetadataA))
	if upA.status != http.StatusOK {
		t.Fatalf("isolation matrix: upsert A: status %d body %s", upA.status, upA.body)
	}
	upB := doHTTP(t, client, srv, http.MethodPut,
		"/v1/namespaces/project-beta/records", b.cred,
		upsertPayload(objB, projB, isoSpaceKey, fx.VectorsB, contentHashOf(fx.AppB, "project-beta", 2), fx.MetadataB))
	if upB.status != http.StatusOK {
		t.Fatalf("isolation matrix: upsert B: status %d body %s", upB.status, upB.body)
	}

	// Capture the service-generated record UUIDs for cross-application
	// read/delete attempts (the upsert response does not return them).
	idsA := recordIDsByApp(t, h, a.id)
	idsB := recordIDsByApp(t, h, b.id)
	if len(idsA) != 1 || len(idsB) != 1 {
		t.Fatalf("isolation matrix: expected 1 record per app, got A=%d B=%d", len(idsA), len(idsB))
	}

	t.Run("Case1_CrossApplicationDataRead", func(t *testing.T) {
		// A's credential, B's namespace key, B's record UUID: 404 — the
		// namespace is not visible to A (RLS), so the record is not
		// resolvable.
		res := doHTTP(t, client, srv, http.MethodGet,
			"/v1/namespaces/project-beta/records/"+idsB[0], a.cred, nil)
		if res.status != http.StatusNotFound {
			t.Fatalf("case 1: A reading B's record under B's namespace: status %d body %s (want 404)",
				res.status, res.body)
		}
		// A's credential, A's own namespace, B's record UUID: likewise
		// 404 (the record is not in A's namespace).
		res = doHTTP(t, client, srv, http.MethodGet,
			"/v1/namespaces/project-alpha/records/"+idsB[0], a.cred, nil)
		if res.status != http.StatusNotFound {
			t.Fatalf("case 1: A reading B's record under A's namespace: status %d body %s (want 404)",
				res.status, res.body)
		}
		// B's credential, B's record UUID (in B's namespace): 200 — the
		// read succeeds for the owner.
		res = doHTTP(t, client, srv, http.MethodGet,
			"/v1/namespaces/project-beta/records/"+idsB[0], b.cred, nil)
		if res.status != http.StatusOK {
			t.Fatalf("case 1: B reading own record: status %d body %s (want 200)", res.status, res.body)
		}
		if strings.Contains(res.body, a.id) {
			t.Errorf("case 1: B's response body references A's application id: %s", res.body)
		}
		if !strings.Contains(res.body, objB) {
			t.Errorf("case 1: B's own record missing its object id %s: %s", objB, res.body)
		}
		// Cross-application search proof: A searches A's own namespace with
		// B's exact stored vector and a structured filter matching only B's
		// metadata. Even though the vector matches B's record perfectly and
		// the filter selects B's metadata, A's namespace contains no B rows,
		// so the search returns zero matches.
		res = doHTTP(t, client, srv, http.MethodPost,
			"/v1/namespaces/project-alpha/search", a.cred,
			map[string]any{
				"vector_space": isoSpaceKey,
				"vector":       fx.VectorsB,
				"limit":        10,
				"filters": map[string]any{
					"all": []map[string]any{
						{
							"field": "service",
							"op":    "eq",
							"value": testdb.AppNotesService,
						},
					},
				},
			})
		if res.status != http.StatusOK {
			t.Fatalf("case 1: A searching own namespace with B's vector/filter: status %d body %s (want 200)",
				res.status, res.body)
		}
		var searchOut struct {
			Matches []struct {
				ObjectID string `json:"object_id"`
			} `json:"matches"`
		}
		if err := json.Unmarshal([]byte(res.body), &searchOut); err != nil {
			t.Fatalf("case 1: decode search response: %v (body %s)", err, res.body)
		}
		for _, m := range searchOut.Matches {
			if m.ObjectID == objB {
				t.Fatalf("case 1: A's search in A's namespace returned B's object %s — cross-application leak", m.ObjectID)
			}
		}
	})

	t.Run("Case2_CrossApplicationDataWrite", func(t *testing.T) {
		// A's credential upserting under B's namespace key: the namespace
		// is not visible to A, so the upsert cannot write into B's
		// namespace (404 namespace_not_found). B's record count is unchanged.
		res := doHTTP(t, client, srv, http.MethodPut,
			"/v1/namespaces/project-beta/records", a.cred,
			upsertPayload(
				testdb.DeterministicObjectID(fx.AppA, 303),
				testdb.DeterministicProjectionID(fx.AppA, 303, 1),
				isoSpaceKey, fx.VectorsA,
				contentHashOf(fx.AppA, "project-beta", 3), fx.MetadataA))
		if res.status != http.StatusNotFound {
			t.Fatalf("case 2: A upserting into B's namespace: status %d body %s (want 404)",
				res.status, res.body)
		}
		var apiErr struct {
			Error struct {
				Code string `json:"code"`
			} `json:"error"`
		}
		if err := json.Unmarshal([]byte(res.body), &apiErr); err != nil {
			t.Fatalf("case 2: decode error envelope: %v (body %s)", err, res.body)
		}
		if apiErr.Error.Code != "namespace_not_found" {
			t.Fatalf("case 2: error code = %q, want namespace_not_found; body: %s",
				apiErr.Error.Code, res.body)
		}
		if got := len(recordIDsByApp(t, h, b.id)); got != 1 {
			t.Fatalf("case 2: B's record count changed: got %d (want 1)", got)
		}
	})

	t.Run("Case3_CrossApplicationDelete", func(t *testing.T) {
		// A's credential deleting B's projection in A's own namespace:
		// the request succeeds (namespace is visible to A) but deletes
		// zero records because B's identifiers do not exist in A's
		// namespace. B's record remains untouched.
		res := doHTTP(t, client, srv, http.MethodDelete,
			"/v1/namespaces/project-alpha/records", a.cred,
			map[string]string{
				"object_id":     objB,
				"projection_id": projB,
				"vector_space":  isoSpaceKey,
			})
		if res.status != http.StatusOK {
			t.Fatalf("case 3: A deleting B's projection in A's namespace: status %d body %s (want 200)",
				res.status, res.body)
		}
		var del struct {
			Deleted int `json:"deleted"`
		}
		if err := json.Unmarshal([]byte(res.body), &del); err != nil {
			t.Fatalf("case 3: decode delete response: %v (body %s)", err, res.body)
		}
		if del.Deleted != 0 {
			t.Fatalf("case 3: projection delete deleted %d records (want 0) — cross-application leak", del.Deleted)
		}

		// A's credential deleting B's object in A's own namespace:
		// likewise succeeds with zero deletions.
		res = doHTTP(t, client, srv, http.MethodDelete,
			"/v1/namespaces/project-alpha/objects/"+objB, a.cred, nil)
		if res.status != http.StatusOK {
			t.Fatalf("case 3: A deleting B's object in A's namespace: status %d body %s (want 200)",
				res.status, res.body)
		}
		if err := json.Unmarshal([]byte(res.body), &del); err != nil {
			t.Fatalf("case 3: decode object delete response: %v (body %s)", err, res.body)
		}
		if del.Deleted != 0 {
			t.Fatalf("case 3: object delete deleted %d records (want 0) — cross-application leak", del.Deleted)
		}

		// Direct DB assertion: B's rows are still present after both
		// delete attempts.
		if got := len(recordIDsByApp(t, h, b.id)); got != 1 {
			t.Fatalf("case 3: B's record count changed after A's deletes: got %d (want 1)", got)
		}
	})

	t.Run("Case3_SameAppNamespaceIsolation", func(t *testing.T) {
		// "Namespace X cannot accidentally expose namespace Y": same
		// application, two namespaces (project-alpha and research).
		// A record seeded into research must not be reachable through
		// project-alpha via GET, search, or delete.
		// Ordinals 500+ avoid collision with fixture rows (ordinals
		// 101, 202, 303, 404, 801, 802, 999).

		// Use a unique vector and metadata for the research record so
		// search assertions are isolated: the research vector (basis 2)
		// and metadata filter uniquely identify only the research row,
		// independent of the project-alpha record (basis 0, MetadataA).
		vecResearch := testdb.BasisVector(2)
		metaResearch := map[string]any{
			"document_type": "note",
			"state":         "active",
			"service":       fx.AppA,
			"namespace":     "research",
		}

		// Seed a unique A-owned record into the research namespace.
		objResearch := testdb.DeterministicObjectID(fx.AppA, 501)
		projResearch := testdb.DeterministicProjectionID(fx.AppA, 501, 1)
		upResearch := doHTTP(t, client, srv, http.MethodPut,
			"/v1/namespaces/research/records", a.cred,
			upsertPayload(objResearch, projResearch, isoSpaceKey,
				vecResearch, contentHashOf(fx.AppA, "research", 501), metaResearch))
		if upResearch.status != http.StatusOK {
			t.Fatalf("case 3 same-app: upsert into research: status %d body %s",
				upResearch.status, upResearch.body)
		}

		// Register cleanup immediately after successful upsert so that
		// test failures during the subsequent DB lookup do not leave
		// the research row and contaminate later subtests (Case8 expects
		// exactly one record per app).
		t.Cleanup(func() {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			conn, err := h.BootstrapConn(ctx, testdb.DatabaseName)
			if err != nil {
				t.Logf("case 3 same-app: cleanup: bootstrap connection: %v", err)
				return
			}
			defer conn.Close(ctx)
			_, err = conn.Exec(ctx,
				`DELETE FROM vector_data.vector_records `+
					`WHERE application_id = $1 `+
					`AND namespace_id = (SELECT id FROM vector_control.namespaces `+
					`  WHERE application_id = $1 AND namespace_key = $2) `+
					`AND object_id = $3 `+
					`AND projection_id = $4 `+
					`AND vector_space_id = $5`,
				a.id, testdb.NamespaceResearch,
				objResearch, projResearch, testdb.SeededSpaceID)
			if err != nil {
				t.Logf("case 3 same-app: cleanup research record: %v", err)
			}
		})

		// Verify the record is actually retrievable through its own
		// namespace (research) — sanity check that provisioning worked.
		idsResearch := recordIDsByApp(t, h, a.id)
		var researchRecordID string
		for _, id := range idsResearch {
			// The research record is the one with object_id == objResearch.
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			conn, connErr := h.BootstrapConn(ctx, testdb.DatabaseName)
			if connErr != nil {
				cancel()
				t.Fatalf("case 3 same-app: bootstrap connection: %v", connErr)
			}
			var objID string
			scanErr := conn.QueryRow(ctx,
				`SELECT object_id FROM vector_data.vector_records WHERE id = $1`, id).Scan(&objID)
			conn.Close(ctx)
			cancel()
			if scanErr != nil {
				t.Fatalf("case 3 same-app: scan object_id: %v", scanErr)
			}
			if objID == objResearch {
				researchRecordID = id
				break
			}
		}
		if researchRecordID == "" {
			t.Fatalf("case 3 same-app: research record not found in DB (seeded object %s)", objResearch)
		}

		// --- (a) GET: retrieve research record via project-alpha → 404 ---
		res := doHTTP(t, client, srv, http.MethodGet,
			"/v1/namespaces/project-alpha/records/"+researchRecordID, a.cred, nil)
		if res.status != http.StatusNotFound {
			t.Fatalf("case 3 same-app: GET research record via project-alpha: status %d body %s (want 404)",
				res.status, res.body)
		}

		// --- (b) Search: search project-alpha with the research record's
		// unique vector and metadata filter → exactly zero matches.
		// The research vector (basis 2) is orthogonal to the project-alpha
		// record (basis 0), and the metadata filter further restricts to
		// the research namespace tag, so any match would indicate a
		// namespace-boundary leak.
		res = doHTTP(t, client, srv, http.MethodPost,
			"/v1/namespaces/project-alpha/search", a.cred,
			map[string]any{
				"vector_space": isoSpaceKey,
				"vector":       vecResearch,
				"limit":        10,
				"filters": map[string]any{
					"all": []map[string]any{
						{
							"field": "namespace",
							"op":    "eq",
							"value": "research",
						},
					},
				},
			})
		if res.status != http.StatusOK {
			t.Fatalf("case 3 same-app: search project-alpha: status %d body %s (want 200)",
				res.status, res.body)
		}
		var searchOut struct {
			Matches []struct {
				ObjectID string `json:"object_id"`
			} `json:"matches"`
		}
		if err := json.Unmarshal([]byte(res.body), &searchOut); err != nil {
			t.Fatalf("case 3 same-app: decode search response: %v (body %s)", err, res.body)
		}
		if len(searchOut.Matches) != 0 {
			t.Fatalf("case 3 same-app: search in project-alpha returned %d match(es) for research filter (want 0) — namespace boundary leak: %s",
				len(searchOut.Matches), res.body)
		}

		// --- (c) Projection delete: delete research record via project-alpha → 200, deleted=0 ---
		res = doHTTP(t, client, srv, http.MethodDelete,
			"/v1/namespaces/project-alpha/records", a.cred,
			map[string]string{
				"object_id":     objResearch,
				"projection_id": projResearch,
				"vector_space":  isoSpaceKey,
			})
		if res.status != http.StatusOK {
			t.Fatalf("case 3 same-app: projection delete via project-alpha: status %d body %s (want 200)",
				res.status, res.body)
		}
		var del struct {
			Deleted int `json:"deleted"`
		}
		if err := json.Unmarshal([]byte(res.body), &del); err != nil {
			t.Fatalf("case 3 same-app: decode projection delete response: %v (body %s)", err, res.body)
		}
		if del.Deleted != 0 {
			t.Fatalf("case 3 same-app: projection delete via project-alpha deleted %d records (want 0) — namespace boundary leak", del.Deleted)
		}

		// --- (d) Object delete: delete research object via project-alpha → 200, deleted=0 ---
		res = doHTTP(t, client, srv, http.MethodDelete,
			"/v1/namespaces/project-alpha/objects/"+objResearch, a.cred, nil)
		if res.status != http.StatusOK {
			t.Fatalf("case 3 same-app: object delete via project-alpha: status %d body %s (want 200)",
				res.status, res.body)
		}
		if err := json.Unmarshal([]byte(res.body), &del); err != nil {
			t.Fatalf("case 3 same-app: decode object delete response: %v (body %s)", err, res.body)
		}
		if del.Deleted != 0 {
			t.Fatalf("case 3 same-app: object delete via project-alpha deleted %d records (want 0) — namespace boundary leak", del.Deleted)
		}

		// --- (e) Direct DB assertion: research record is unchanged ---
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		dbConn, err := h.BootstrapConn(ctx, testdb.DatabaseName)
		if err != nil {
			t.Fatalf("case 3 same-app: bootstrap connection: %v", err)
		}
		defer dbConn.Close(ctx)
		var count int
		err = dbConn.QueryRow(ctx,
			`SELECT count(*) FROM vector_data.vector_records WHERE id = $1`,
			researchRecordID).Scan(&count)
		if err != nil {
			t.Fatalf("case 3 same-app: DB count research record: %v", err)
		}
		if count != 1 {
			t.Fatalf("case 3 same-app: research record deleted or mutated by project-alpha operations (count %d, want 1)", count)
		}
	})

	t.Run("Case4_CrossApplicationSearch", func(t *testing.T) {
		// A's credential searching B's namespace: the namespace is not
		// visible, so the search cannot run (404).
		res := doHTTP(t, client, srv, http.MethodPost,
			"/v1/namespaces/project-beta/search", a.cred,
			map[string]any{"vector_space": isoSpaceKey, "vector": fx.VectorsA, "limit": 10})
		if res.status == http.StatusOK {
			t.Fatalf("case 4: A searching B's namespace succeeded: body %s", res.body)
		}
		// A's credential searching A's own namespace returns exactly
		// A's record (basis vector 0 matches only A's basis-0 record;
		// B's record is in a different namespace and application).
		res = doHTTP(t, client, srv, http.MethodPost,
			"/v1/namespaces/project-alpha/search", a.cred,
			map[string]any{"vector_space": isoSpaceKey, "vector": fx.VectorsA, "limit": 10})
		if res.status != http.StatusOK {
			t.Fatalf("case 4: A searching own namespace: status %d body %s", res.status, res.body)
		}
		var out struct {
			Matches []struct {
				ObjectID string `json:"object_id"`
			} `json:"matches"`
		}
		if err := json.Unmarshal([]byte(res.body), &out); err != nil {
			t.Fatalf("case 4: decode search: %v (body %s)", err, res.body)
		}
		for _, m := range out.Matches {
			if m.ObjectID == objB {
				t.Fatalf("case 4: A's search returned B's object %s", m.ObjectID)
			}
		}
		if len(out.Matches) != 1 || out.Matches[0].ObjectID != objA {
			t.Fatalf("case 4: A's search returned %d matches (want exactly A's own record): %s", len(out.Matches), res.body)
		}
		// Cross-application search proof: A searches A's namespace with
		// B's exact stored vector and a structured filter matching only
		// B's metadata. The result must contain no B rows, proving that
		// neither the vector similarity layer nor the metadata filter
		// bypasses application isolation.
		res = doHTTP(t, client, srv, http.MethodPost,
			"/v1/namespaces/project-alpha/search", a.cred,
			map[string]any{
				"vector_space": isoSpaceKey,
				"vector":       fx.VectorsB,
				"limit":        10,
				"filters": map[string]any{
					"all": []map[string]any{
						{
							"field": "service",
							"op":    "eq",
							"value": testdb.AppNotesService,
						},
					},
				},
			})
		if res.status != http.StatusOK {
			t.Fatalf("case 4: A searching own namespace with B's vector/filter: status %d body %s (want 200)",
				res.status, res.body)
		}
		var searchOut struct {
			Matches []struct {
				ObjectID string `json:"object_id"`
			} `json:"matches"`
		}
		if err := json.Unmarshal([]byte(res.body), &searchOut); err != nil {
			t.Fatalf("case 4: decode search response: %v (body %s)", err, res.body)
		}
		for _, m := range searchOut.Matches {
			if m.ObjectID == objB {
				t.Fatalf("case 4: A's search returned B's object %s — cross-application leak via search+filter", m.ObjectID)
			}
		}
	})

	t.Run("Case5_CrossApplicationNamespaceDiscovery", func(t *testing.T) {
		// "Application A cannot discover application B namespaces":
		// (a) The namespaces table is FORCE RLS scoped by
		//     vector.application_id — with A's transaction context, A
		//     sees exactly its own namespaces (project-alpha and
		//     research), never B's (project-beta).
		// (b) Every data-plane request using B's namespace key under A's
		//     credential returns uniform HTTP 404 namespace_not_found.
		// (c) There is no data-plane endpoint listing namespaces
		//     (asserted by router test TestNoDataPlaneNamespaceListing).

		// --- (a) Direct RLS visibility checks ---
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		conn, err := pool.Acquire(ctx)
		if err != nil {
			t.Fatalf("case 5: acquire: %v", err)
		}
		defer conn.Release()

		tx, err := conn.Begin(ctx)
		if err != nil {
			t.Fatalf("case 5: begin: %v", err)
		}
		if _, err := tx.Exec(ctx,
			`SELECT set_config('vector.application_id', $1, true)`, a.id); err != nil {
			t.Fatalf("case 5: set A context: %v", err)
		}
		rows, err := tx.Query(ctx,
			`SELECT application_id, namespace_key FROM vector_control.namespaces ORDER BY namespace_key`)
		if err != nil {
			t.Fatalf("case 5: list namespaces as A: %v", err)
		}
		var (
			keys   []string
			appIDs []string
		)
		for rows.Next() {
			var (
				appID string
				key   string
			)
			if err := rows.Scan(&appID, &key); err != nil {
				t.Fatalf("case 5: scan namespace: %v", err)
			}
			keys = append(keys, key)
			appIDs = append(appIDs, appID)
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("case 5: rows: %v", err)
		}
		_ = tx.Commit(ctx)

		// Exactly A's two namespaces, both belonging to A.
		want := []string{"project-alpha", "research"}
		if len(keys) != len(want) {
			t.Fatalf("case 5: A sees %d namespaces %v (want %v) — discovery crosses applications or loses rows",
				len(keys), keys, want)
		}
		for i, k := range keys {
			if k != want[i] {
				t.Fatalf("case 5: A sees namespace %q at position %d (want %q)", k, i, want[i])
			}
			if appIDs[i] != a.id {
				t.Fatalf("case 5: A sees namespace %q owned by %s (want A)", k, appIDs[i])
			}
		}

		// Symmetrically, with B's context B sees only its own namespace.
		tx, err = conn.Begin(ctx)
		if err != nil {
			t.Fatalf("case 5: begin B: %v", err)
		}
		if _, err := tx.Exec(ctx,
			`SELECT set_config('vector.application_id', $1, true)`, b.id); err != nil {
			t.Fatalf("case 5: set B context: %v", err)
		}
		rows, err = tx.Query(ctx,
			`SELECT namespace_key FROM vector_control.namespaces ORDER BY namespace_key`)
		if err != nil {
			t.Fatalf("case 5: list namespaces as B: %v", err)
		}
		var bKeys []string
		for rows.Next() {
			var key string
			if err := rows.Scan(&key); err != nil {
				t.Fatalf("case 5: scan B namespace: %v", err)
			}
			bKeys = append(bKeys, key)
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("case 5: B rows: %v", err)
		}
		_ = tx.Commit(ctx)
		if len(bKeys) != 1 || bKeys[0] != "project-beta" {
			t.Fatalf("case 5: B sees namespaces %v (want [project-beta])", bKeys)
		}

		// --- (b) HTTP assertions: A's credential + B's namespace key → 404 namespace_not_found ---
		assertHTTPNamespaceNotFound := func(t *testing.T, method, path string, payload any) {
			t.Helper()
			res := doHTTP(t, client, srv, method, path, a.cred, payload)
			if res.status != http.StatusNotFound {
				t.Fatalf("case 5 HTTP: %s %s: status %d body %s (want 404)",
					method, path, res.status, res.body)
			}
			var apiErr struct {
				Error struct {
					Code string `json:"code"`
				} `json:"error"`
			}
			if err := json.Unmarshal([]byte(res.body), &apiErr); err != nil {
				t.Fatalf("case 5 HTTP: decode error envelope: %v (body %s)", err, res.body)
			}
			if apiErr.Error.Code != "namespace_not_found" {
				t.Fatalf("case 5 HTTP: error code = %q, want namespace_not_found; body: %s",
					apiErr.Error.Code, res.body)
			}
		}

		// GET record: A's credential, B's namespace key, any record UUID.
		assertHTTPNamespaceNotFound(t, http.MethodGet,
			"/v1/namespaces/project-beta/records/"+idsB[0], nil)

		// POST search: A's credential, B's namespace key.
		assertHTTPNamespaceNotFound(t, http.MethodPost,
			"/v1/namespaces/project-beta/search",
			map[string]any{"vector_space": isoSpaceKey, "vector": fx.VectorsA, "limit": 10})

		// PUT upsert: A's credential, B's namespace key.
		assertHTTPNamespaceNotFound(t, http.MethodPut,
			"/v1/namespaces/project-beta/records",
			upsertPayload(
				testdb.DeterministicObjectID(fx.AppA, 404),
				testdb.DeterministicProjectionID(fx.AppA, 404, 1),
				isoSpaceKey, fx.VectorsA,
				contentHashOf(fx.AppA, "project-beta", 4), fx.MetadataA))

		// DELETE projection: A's credential, B's namespace key.
		assertHTTPNamespaceNotFound(t, http.MethodDelete,
			"/v1/namespaces/project-beta/records",
			map[string]string{
				"object_id":     objB,
				"projection_id": projB,
				"vector_space":  isoSpaceKey,
			})

		// DELETE object: A's credential, B's namespace key.
		assertHTTPNamespaceNotFound(t, http.MethodDelete,
			"/v1/namespaces/project-beta/objects/"+objB, nil)
	})

	t.Run("Case6_MissingNamespaceIsNotAllNamespaces", func(t *testing.T) {
		// "Missing namespace does not become all namespaces": a request
		// targeting a nonexistent namespace key must return 404
		// namespace_not_found — it must not silently fall through to a
		// wildcard/all-namespace query that would expose records from
		// other namespaces.
		const fakeNamespace = "nonexistent-namespace-key"

		assertNamespaceNotFound := func(t *testing.T, method, path string, payload any) {
			t.Helper()
			res := doHTTP(t, client, srv, method, path, a.cred, payload)
			if res.status != http.StatusNotFound {
				t.Fatalf("case 6: %s %s: status %d body %s (want 404)",
					method, path, res.status, res.body)
			}
			var apiErr struct {
				Error struct {
					Code string `json:"code"`
				} `json:"error"`
			}
			if err := json.Unmarshal([]byte(res.body), &apiErr); err != nil {
				t.Fatalf("case 6: decode error envelope: %v (body %s)", err, res.body)
			}
			if apiErr.Error.Code != "namespace_not_found" {
				t.Fatalf("case 6: error code = %q, want namespace_not_found; body: %s",
					apiErr.Error.Code, res.body)
			}
		}

		// Snapshot record identities before any nonexistent-namespace
		// operations. Assert identity preservation (not just count)
		// afterwards so that an update preserving row count is caught.
		snapshotA := recordIDsByApp(t, h, a.id)
		snapshotB := recordIDsByApp(t, h, b.id)

		// GET record under nonexistent namespace: 404 namespace_not_found,
		// not a wildcard query that returns A's or B's records.
		assertNamespaceNotFound(t, http.MethodGet,
			"/v1/namespaces/"+fakeNamespace+"/records/"+idsA[0], nil)

		// POST search under nonexistent namespace: 404 namespace_not_found,
		// not an empty-or-wildcard search that leaks records.
		assertNamespaceNotFound(t, http.MethodPost,
			"/v1/namespaces/"+fakeNamespace+"/search",
			map[string]any{"vector_space": isoSpaceKey, "vector": fx.VectorsA, "limit": 10})

		// PUT upsert under nonexistent namespace: 404 namespace_not_found,
		// not a silent insertion into some default namespace.
		assertNamespaceNotFound(t, http.MethodPut,
			"/v1/namespaces/"+fakeNamespace+"/records",
			upsertPayload(
				testdb.DeterministicObjectID(fx.AppA, 999),
				testdb.DeterministicProjectionID(fx.AppA, 999, 1),
				isoSpaceKey, fx.VectorsA,
				contentHashOf(fx.AppA, fakeNamespace, 9), fx.MetadataA))

		// DELETE projection under nonexistent namespace: 404 namespace_not_found,
		// not a wildcard delete that removes records from other namespaces.
		assertNamespaceNotFound(t, http.MethodDelete,
			"/v1/namespaces/"+fakeNamespace+"/records",
			map[string]string{
				"object_id":     objA,
				"projection_id": projA,
				"vector_space":  isoSpaceKey,
			})

		// DELETE object with ?vector_space= under nonexistent namespace:
		// 404 namespace_not_found — not a wildcard delete that removes
		// records from other namespaces.
		assertNamespaceNotFound(t, http.MethodDelete,
			"/v1/namespaces/"+fakeNamespace+"/objects/"+objA+"?vector_space="+isoSpaceKey, nil)

		// Direct DB assertion: the nonexistent namespace requests did not
		// mutate any data — both applications still have the same records
		// (identity check, not just count).
		afterA := recordIDsByApp(t, h, a.id)
		afterB := recordIDsByApp(t, h, b.id)
		if !isoStringSlicesEqual(afterA, snapshotA) {
			t.Fatalf("case 6: A's records changed after nonexistent-namespace operations: %v != %v",
				afterA, snapshotA)
		}
		if !isoStringSlicesEqual(afterB, snapshotB) {
			t.Fatalf("case 6: B's records changed after nonexistent-namespace operations: %v != %v",
				afterB, snapshotB)
		}
	})

	t.Run("Case7_NoRLSContextVectorAPISQL", func(t *testing.T) {
		// Database-integration test: a vector_api connection WITHOUT any
		// set_config('vector.application_id', ...) context. The FORCE RLS
		// policies must hide both applications' rows (the GUC is unset, so
		// the policy's equality test fails for every row).
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		conn, err := h.RuntimeConn(ctx)
		if err != nil {
			t.Fatalf("case 7: runtime connection: %v", err)
		}
		defer conn.Close(ctx)

		var n int
		err = conn.QueryRow(ctx,
			`SELECT count(*) FROM vector_data.vector_records`).Scan(&n)
		if err != nil {
			t.Fatalf("case 7: count without RLS context: %v", err)
		}
		if n != 0 {
			t.Fatalf("case 7: vector_api WITHOUT application context sees %d records (want 0) — RLS is not enforcing", n)
		}

		// A SELECT scoped to A's application by parameter alone (no
		// context) is also empty: RLS hides the rows before the WHERE
		// clause would matter.
		err = conn.QueryRow(ctx,
			`SELECT count(*) FROM vector_data.vector_records WHERE application_id = $1`,
			a.id).Scan(&n)
		if err != nil {
			t.Fatalf("case 7: scoped count without context: %v", err)
		}
		if n != 0 {
			t.Fatalf("case 7: scoped SELECT without context sees %d rows (want 0) — RLS is not enforcing", n)
		}
		// INSERT denial: a runtime connection without application context
		// must be denied by RLS even with a valid synthetic row matching
		// the production schema.
		{
			ctxBootstrap, cancelBootstrap := context.WithTimeout(context.Background(), 10*time.Second)
			bootConn, errBoot := h.BootstrapConn(ctxBootstrap, testdb.DatabaseName)
			if errBoot != nil {
				cancelBootstrap()
				t.Fatalf("case 7: bootstrap connection for namespace resolution: %v", errBoot)
			}
			var nsID string
			err = bootConn.QueryRow(ctxBootstrap,
				`SELECT id FROM vector_control.namespaces WHERE application_id = $1 AND namespace_key = $2`,
				a.id, "project-alpha").Scan(&nsID)
			bootConn.Close(ctxBootstrap)
			cancelBootstrap()
			if err != nil {
				t.Fatalf("case 7: resolve namespace id: %v", err)
			}

			recordID, err := testdb.NewUUID()
			if err != nil {
				t.Fatalf("case 7: generate record uuid: %v", err)
			}
			hashBytes, _ := hex.DecodeString("000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f")
			vectorLit := fmt.Sprintf("[%s]", strings.Repeat("0.1,", testdb.SeededSpaceDimensions-1)+"0.1")

			_, err = conn.Exec(ctx,
				`INSERT INTO vector_data.vector_records
					(id, application_id, namespace_id, object_id, projection_id,
					 vector_space_id, content_hash, source_updated_at, metadata, embedding)
				 VALUES ($1, $2, $3, $4, $5, $6, $7, NULL, '{}'::jsonb, $8::vector)`,
				recordID, a.id, nsID,
				"0199f31e-a000-7000-8000-0000000000c1",
				"0199f31e-a000-7000-8000-0000000000c2",
				testdb.SeededSpaceID, hashBytes, vectorLit)
			if err == nil {
				t.Fatalf("case 7: INSERT without application context was not denied by RLS")
			}
			var pgErr *pgconn.PgError
			if !errors.As(err, &pgErr) {
				t.Fatalf("case 7: INSERT error: expected *pgconn.PgError, got %T: %v", err, err)
			}
			if pgErr.Code != "42501" {
				t.Fatalf("case 7: INSERT denied by RLS: expected error code '42501', got '%s'", pgErr.Code)
			}
		}
	})

	t.Run("Case8_PoolReuseSetConfigScope", func(t *testing.T) {
		// Case 8 has three sub-tests:
		//   (a) DirectTransactionSequence — existing baseline: three
		//       back-to-back transactions on one acquired connection prove
		//       set_config(is_local=true) does not leak across transactions.
		//   (b) HTTPPoolReuse — A upsert → B upsert → A get through the
		//       real HTTP/auth/production chain on a single-connection pool,
		//       proving A sees only A even when the same physical connection
		//       serves both applications.
		//   (c) ConnectionReuseAbort — A's transaction is forced to abort,
		//       then the next transaction on that same physical connection as
		//       B sees no A context or data leakage.

		t.Run("DirectTransactionSequence", func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			conn, err := pool.Acquire(ctx)
			if err != nil {
				t.Fatalf("case 8: acquire: %v", err)
			}
			defer conn.Release()

			tx, err := conn.Begin(ctx)
			if err != nil {
				t.Fatalf("case 8: begin A: %v", err)
			}
			if _, err := tx.Exec(ctx,
				`SELECT set_config('vector.application_id', $1, true)`, a.id); err != nil {
				t.Fatalf("case 8: set A context: %v", err)
			}
			var nA int
			if err := tx.QueryRow(ctx,
				`SELECT count(*) FROM vector_data.vector_records`).Scan(&nA); err != nil {
				t.Fatalf("case 8: count A: %v", err)
			}
			if err := tx.Commit(ctx); err != nil {
				t.Fatalf("case 8: commit A: %v", err)
			}
			if nA != 1 {
				t.Fatalf("case 8: A context sees %d records (want 1)", nA)
			}

			// Same physical connection, next transaction, B's context.
			tx, err = conn.Begin(ctx)
			if err != nil {
				t.Fatalf("case 8: begin B: %v", err)
			}
			if _, err := tx.Exec(ctx,
				`SELECT set_config('vector.application_id', $1, true)`, b.id); err != nil {
				t.Fatalf("case 8: set B context: %v", err)
			}
			var nB int
			if err := tx.QueryRow(ctx,
				`SELECT count(*) FROM vector_data.vector_records`).Scan(&nB); err != nil {
				t.Fatalf("case 8: count B: %v", err)
			}
			if err := tx.Commit(ctx); err != nil {
				t.Fatalf("case 8: commit B: %v", err)
			}
			if nB != 1 {
				t.Fatalf("case 8: B context sees %d records (want 1) — previous A context leaked or RLS misapplied", nB)
			}

			// Same physical connection, next transaction, NO context: the
			// is_local=true setting must have rolled back with A's and B's
			// transactions.
			tx, err = conn.Begin(ctx)
			if err != nil {
				t.Fatalf("case 8: begin unscoped: %v", err)
			}
			var nNone int
			if err := tx.QueryRow(ctx,
				`SELECT count(*) FROM vector_data.vector_records`).Scan(&nNone); err != nil {
				t.Fatalf("case 8: count unscoped: %v", err)
			}
			_ = tx.Commit(ctx)
			if nNone != 0 {
				t.Fatalf("case 8: pooled connection without context sees %d records (want 0) — set_config leaked across transactions", nNone)
			}
		})

		t.Run("HTTPPoolReuse", func(t *testing.T) {
			// Exercise A upsert → B upsert → A get through the real
			// HTTP/auth/production chain on a single-connection pool.
			// With MaxConns=1 the pool must reuse the same physical
			// connection for every request, proving that the service
			// correctly scopes set_config per-transaction and A sees
			// only its own data even after B's request has used the
			// same connection.
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			// Create a single-connection pool to force physical connection reuse.
			rconn, err := h.RuntimeConn(ctx)
			if err != nil {
				t.Fatalf("case 8 HTTP: runtime connection: %v", err)
			}
			cc := rconn.Config()
			rconn.Close(ctx)
			singlePoolCfg, err := pgxpool.ParseConfig(
				fmt.Sprintf("host=%s port=%d dbname=%s user=%s sslmode=disable",
					cc.Host, cc.Port, cc.Database, cc.User))
			if err != nil {
				t.Fatalf("case 8 HTTP: parse pool config: %v", err)
			}
			singlePoolCfg.MaxConns = 1
			singlePool, err := pgxpool.NewWithConfig(ctx, singlePoolCfg)
			if err != nil {
				t.Fatalf("case 8 HTTP: open single-connection pool: %v", err)
			}
			defer singlePool.Close()
			if err := singlePool.Ping(ctx); err != nil {
				t.Fatalf("case 8 HTTP: single pool ping: %v", err)
			}

			// Serve the real production router against the single-connection pool.
			// The parent test has already set the runtime environment, so
			// config.Load picks up the same database connection settings.
			singleCfg, err := config.Load()
			if err != nil {
				t.Fatalf("case 8 HTTP: config.Load: %v", err)
			}
			singleDeps := &api.Deps{
				Pool:              singlePool,
				MigrationsApplied: func() bool { return true },
			}
			singleSrv := httptest.NewServer(api.NewRouterShared(singleCfg, singleDeps))
			defer singleSrv.Close()

			// Register two applications through the admin plane with unique
			// keys that do not collide with the parent test's fixture apps.
			ha := registerApp(t, client, singleSrv, adminToken, "iso-http-app-a", "iso-http-alpha")
			hb := registerApp(t, client, singleSrv, adminToken, "iso-http-app-b", "iso-http-beta")

			// Unique synthetic IDs for this sub-test (ordinals 800+ to avoid
			// collision with the seeded fixture rows).
			objA8 := testdb.DeterministicObjectID("iso-http-app-a", 801)
			projA8 := testdb.DeterministicProjectionID("iso-http-app-a", 801, 1)
			objB8 := testdb.DeterministicObjectID("iso-http-app-b", 802)
			projB8 := testdb.DeterministicProjectionID("iso-http-app-b", 802, 1)

			// A upsert (first request on the single-connection pool).
			upA := doHTTP(t, client, singleSrv, http.MethodPut,
				"/v1/namespaces/iso-http-alpha/records", ha.cred,
				upsertPayload(objA8, projA8, isoSpaceKey, fx.VectorsA,
					contentHashOf(fx.AppA, "iso-http-alpha", 801), fx.MetadataA))
			if upA.status != http.StatusOK {
				t.Fatalf("case 8 HTTP: A upsert: status %d body %s", upA.status, upA.body)
			}

			// B upsert (second request — same physical connection, different app).
			upB := doHTTP(t, client, singleSrv, http.MethodPut,
				"/v1/namespaces/iso-http-beta/records", hb.cred,
				upsertPayload(objB8, projB8, isoSpaceKey, fx.VectorsB,
					contentHashOf(fx.AppB, "iso-http-beta", 802), fx.MetadataB))
			if upB.status != http.StatusOK {
				t.Fatalf("case 8 HTTP: B upsert: status %d body %s", upB.status, upB.body)
			}

			// A get via search (third request — same physical connection again).
			// A must see only A's record, never B's, even though the same
			// physical connection just served B's upsert.
			searchA := doHTTP(t, client, singleSrv, http.MethodPost,
				"/v1/namespaces/iso-http-alpha/search", ha.cred,
				map[string]any{
					"vector_space": isoSpaceKey,
					"vector":       fx.VectorsA,
					"limit":        10,
				})
			if searchA.status != http.StatusOK {
				t.Fatalf("case 8 HTTP: A search: status %d body %s", searchA.status, searchA.body)
			}

			var searchOut struct {
				Matches []struct {
					ObjectID string `json:"object_id"`
				} `json:"matches"`
			}
			if err := json.Unmarshal([]byte(searchA.body), &searchOut); err != nil {
				t.Fatalf("case 8 HTTP: decode search response: %v (body %s)", err, searchA.body)
			}

			// A must see exactly one match (its own record).
			if len(searchOut.Matches) != 1 {
				t.Fatalf("case 8 HTTP: A search returned %d matches (want 1) on reused connection: %s",
					len(searchOut.Matches), searchA.body)
			}
			if searchOut.Matches[0].ObjectID != objA8 {
				t.Fatalf("case 8 HTTP: A search returned object %q (want %q) — pool reuse leaked application context",
					searchOut.Matches[0].ObjectID, objA8)
			}
			// A must not see B's object.
			for _, m := range searchOut.Matches {
				if m.ObjectID == objB8 {
					t.Fatalf("case 8 HTTP: A search returned B's object %s — cross-application leak via pool reuse", objB8)
				}
			}
		})

		t.Run("ConnectionReuseAbort", func(t *testing.T) {
			// A's transaction is forced to abort (ROLLBACK), then the next
			// transaction on that same physical connection as B sees no A
			// context or data leakage. This proves set_config(is_local=true)
			// is fully cleaned up by ROLLBACK on a pooled connection.
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			conn, err := pool.Acquire(ctx)
			if err != nil {
				t.Fatalf("case 8 abort: acquire: %v", err)
			}
			defer conn.Release()

			// Record the physical connection PID to prove reuse.
			var pidBefore int32
			err = conn.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&pidBefore)
			if err != nil {
				t.Fatalf("case 8 abort: read pid: %v", err)
			}

			// A's transaction: set context, then force an error to abort.
			tx, err := conn.Begin(ctx)
			if err != nil {
				t.Fatalf("case 8 abort: begin A: %v", err)
			}
			if _, err := tx.Exec(ctx,
				`SELECT set_config('vector.application_id', $1, true)`, a.id); err != nil {
				t.Fatalf("case 8 abort: set A context: %v", err)
			}
			// Force a division-by-zero error to abort the transaction.
			_, err = tx.Exec(ctx, `SELECT 1/0`)
			if err == nil {
				t.Fatalf("case 8 abort: expected division-by-zero error")
			}
			// Rollback the aborted transaction.
			if err := tx.Rollback(ctx); err != nil {
				t.Fatalf("case 8 abort: rollback A: %v", err)
			}

			// B's transaction: same physical connection, fresh context.
			tx, err = conn.Begin(ctx)
			if err != nil {
				t.Fatalf("case 8 abort: begin B: %v", err)
			}
			if _, err := tx.Exec(ctx,
				`SELECT set_config('vector.application_id', $1, true)`, b.id); err != nil {
				t.Fatalf("case 8 abort: set B context: %v", err)
			}

			// Verify same physical connection is still in use.
			var pidAfter int32
			err = tx.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&pidAfter)
			if err != nil {
				t.Fatalf("case 8 abort: read pid after: %v", err)
			}
			if pidBefore != pidAfter {
				t.Fatalf("case 8 abort: connection PID changed %d -> %d (expected same physical connection)",
					pidBefore, pidAfter)
			}

			// B must see only B's records (1 record), not A's.
			var nB int
			if err := tx.QueryRow(ctx,
				`SELECT count(*) FROM vector_data.vector_records`).Scan(&nB); err != nil {
				t.Fatalf("case 8 abort: count B: %v", err)
			}
			if err := tx.Commit(ctx); err != nil {
				t.Fatalf("case 8 abort: commit B: %v", err)
			}
			if nB != 1 {
				t.Fatalf("case 8 abort: B sees %d records after A's aborted transaction (want 1) — A context leaked on reused connection", nB)
			}

			// Verify no residual A context remains after B's transaction.
			tx, err = conn.Begin(ctx)
			if err != nil {
				t.Fatalf("case 8 abort: begin unscoped: %v", err)
			}
			var nNone int
			if err := tx.QueryRow(ctx,
				`SELECT count(*) FROM vector_data.vector_records`).Scan(&nNone); err != nil {
				t.Fatalf("case 8 abort: count unscoped: %v", err)
			}
			_ = tx.Commit(ctx)
			if nNone != 0 {
				t.Fatalf("case 8 abort: pooled connection sees %d records without context (want 0) — set_config leaked after abort", nNone)
			}
		})
	})
}
