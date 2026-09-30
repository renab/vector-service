package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"vector-service/internal/apierr"
	"vector-service/internal/auth"
	"vector-service/internal/config"
	"vector-service/internal/dbctx"
	"vector-service/internal/namespaces"
	"vector-service/internal/vectors"
)

// Deps carries the dependencies every route handler needs. Handlers read
// them through the pointer the router holds, so a field can be dynamic:
// MigrationsApplied is an in-process readiness flag that the package-1
// startup migration step flips to true once the database is converged
// (the runtime role has no grant on vector_control.schema_migrations, so
// readiness never reads migration history). A static value (for example in
// tests) is equally valid. PingTimeout bounds the per-request readiness
// ping; it is a proposal of 2 s, carried here so the router stays a pure
// configuration consumer.
type Deps struct {
	Pool              *pgxpool.Pool
	MigrationsApplied func() bool
	PingTimeout       time.Duration
}

// NewRouter builds the complete HTTP handler for the service (implementation
// package 4, section 4). Every docs/API.md endpoint is registered exactly
// once, with its stable operation name and its auth stage:
//
//   - data plane (DataPlaneAuth stage): the five records/search endpoints;
//   - admin plane (AdminAuth stage): the four management endpoints;
//   - base chain (no auth): /healthz and /readyz.
//
// GET /v1/vector-spaces is deliberately NOT registered (C13, blocked): a
// request to that path falls through to the unknown-route 404. An unknown
// path renders 404 not_found; a known path with the wrong method renders
// 405 method_not_allowed with the Allow header.
//
// NewRouter takes the dependency set by value; handlers read the value they
// captured at construction. Use NewRouterShared for a set whose fields
// change after construction (the in-process readiness flag flips after the
// router is built).
func NewRouter(cfg *config.Config, deps Deps) http.Handler {
	d := deps
	return NewRouterShared(cfg, &d)
}

// NewRouterShared builds the complete HTTP handler for the service from a
// dependency set held by pointer. Handlers read the set's fields at request
// time through the pointer, so a field set after construction is observed
// (MigrationsApplied is set by the package-1 startup migration step once
// the database is converged). This is the form internal/server uses.
func NewRouterShared(cfg *config.Config, deps *Deps) http.Handler {
	bounds := vectors.BoundsFromConfig(cfg)
	chain := &Chain{
		Deadline:     &OperationDeadline{Timeout: cfg.HTTPRequestTimeout},
		MaxBodyBytes: cfg.HTTPMaxBodyBytes,
	}

	mux := http.NewServeMux()

	// withPathKeys wraps an http.Handler to inject the route's closed
	// path-key set into the request context before the chain runs. The
	// endpoint adapter (handlerAdapter) reads this set to build
	// HandlerInput.Path.
	withPathKeys := func(keys []string, next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			r = r.WithContext(WithRoutePathKeys(r.Context(), keys))
			next.ServeHTTP(w, r)
		})
	}

	// --- Data plane (DataPlaneAuth stage) ---
	dataAuth := func(ctx context.Context, r *http.Request) (string, error) {
		appID, err := DataPlaneAuth(ctx, r, func(ctx context.Context, cred string) (string, error) {
			result, err := auth.Lookup(ctx, deps.Pool, cred)
			if err != nil {
				return "", err
			}
			if st := LogStateOf(ctx); st != nil {
				st.ApplicationID = result.ApplicationID
			}
			return result.ApplicationID, nil
		})
		if err != nil {
			return "", err
		}
		// The application ID is already propagated into the request context
		// via dbctx.WithAppID by the chain's authStage (chain.go). The local
		// r.WithContext reassignment here was dead code: the caller receives
		// only the returned appID string, not the mutated request. Do NOT
		// reintroduce it — the canonical propagation lives in chain.go.
		return appID, nil
	}

	mux.Handle("PUT /v1/namespaces/{namespace}/records",
		withPathKeys([]string{"namespace"},
			chain.FullChain("upsert", dataAuth, true,
				newUpsertHandler(deps, bounds))))
	mux.Handle("POST /v1/namespaces/{namespace}/search",
		withPathKeys([]string{"namespace"},
			chain.FullChain("search", dataAuth, true,
				newSearchHandler(deps, bounds))))
	mux.Handle("GET /v1/namespaces/{namespace}/records/{record_id}",
		withPathKeys([]string{"namespace", "record_id"},
			chain.FullChain("get_record", dataAuth, false,
				newGetRecordHandler(deps))))
	mux.Handle("DELETE /v1/namespaces/{namespace}/records",
		withPathKeys([]string{"namespace"},
			chain.FullChain("delete_projection", dataAuth, true,
				newDeleteProjectionHandler(deps))))
	mux.Handle("DELETE /v1/namespaces/{namespace}/objects/{object_id}",
		withPathKeys([]string{"namespace", "object_id"},
			chain.FullChain("delete_object", dataAuth, false,
				newDeleteObjectHandler(deps))))

	// --- Admin plane (AdminAuth stage) ---
	adminAuth := func(ctx context.Context, r *http.Request) (string, error) {
		return AdminAuth(ctx, r, cfg.AdminToken)
	}

	mux.Handle("POST /v1/admin/applications",
		withPathKeys(nil,
			chain.FullChain("register_application", adminAuth, true,
				newRegisterApplicationHandler(deps))))
	mux.Handle("POST /v1/admin/applications/{application}/namespaces",
		withPathKeys([]string{"application"},
			chain.FullChain("register_namespace", adminAuth, true,
				newRegisterNamespaceHandler(deps, cfg))))
	mux.Handle("POST /v1/admin/applications/{application}/credentials",
		withPathKeys([]string{"application"},
			chain.FullChain("create_credential", adminAuth, true,
				newCreateCredentialHandler(deps))))
	mux.Handle("DELETE /v1/admin/applications/{application}/credentials/{credential_id}",
		withPathKeys([]string{"application", "credential_id"},
			chain.FullChain("disable_credential", adminAuth, false,
				newDisableCredentialHandler(deps))))

	// --- Base chain (no auth): health and readiness ---
	mux.Handle("GET /healthz", chain.BaseChain(healthzHandler(deps)))
	mux.Handle("GET /readyz", chain.BaseChain(readyzHandler(deps)))

	// Unknown-route and method-not-allowed outcomes (section 4). The
	// fallback handler computes the Allow header from the static route
	// table because ServeMux with a registered fallback does not set it.
	mux.HandleFunc("/", routingOutcome(routeTable))

	return mux
}

// routeEntry records the allowed methods for one path pattern in the
// fallback route table.
type routeEntry struct {
	// pattern is the path pattern (without method) for matching.
	pattern string
	// methods is the space-joined list of allowed methods for the Allow
	// header.
	methods string
}

// routeTable is the static set of registered routes used by the fallback
// handler to distinguish a wrong-method 405 (path known, method not) from
// an unknown-path 404. It is built once at construction.
var routeTable = []routeEntry{
	{"/v1/namespaces/{namespace}/records", "PUT, DELETE"},
	{"/v1/namespaces/{namespace}/search", "POST"},
	{"/v1/namespaces/{namespace}/records/{record_id}", "GET"},
	{"/v1/namespaces/{namespace}/objects/{object_id}", "DELETE"},
	{"/v1/admin/applications", "POST"},
	{"/v1/admin/applications/{application}/namespaces", "POST"},
	{"/v1/admin/applications/{application}/credentials", "POST"},
	{"/v1/admin/applications/{application}/credentials/{credential_id}", "DELETE"},
	{"/healthz", "GET"},
	{"/readyz", "GET"},
}

// routingOutcome returns the mux fallback handler (section 4). It renders
// the unknown-route 404 and the wrong-method 405 (with the Allow header)
// through api.Render. The Allow header is computed from the static route
// table because ServeMux with a registered fallback handler does not set
// it on the request.
func routingOutcome(table []routeEntry) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		// Check whether the request path matches a known route pattern
		// (ignoring method). If so, it is a 405 with the Allow header.
		if allow := matchRouteTable(table, r.Method, r.URL.Path); allow != "" {
			w.Header().Set("Allow", allow)
			Render(ctx, w, apierr.ErrMethodNotAllowed)
			return
		}
		Render(ctx, w, apierr.ErrNotFound)
	}
}

// matchRouteTable checks whether the request path matches a known route
// pattern (ignoring the method). If the path matches but the method is not
// allowed, it returns the Allow header value. If the method is allowed, it
// returns "" (should not happen — the mux would have routed it). If the
// path matches no known route, it returns "" (404).
func matchRouteTable(table []routeEntry, method, path string) string {
	for _, entry := range table {
		if !pathMatchesPattern(path, entry.pattern) {
			continue
		}
		// Path matches. If the method is in the allowed set, this should
		// not have reached the fallback (the mux would have routed it).
		// If the method is not allowed, return the Allow value.
		for _, m := range strings.Split(entry.methods, ",") {
			if m == method {
				// Method is allowed but we're in the fallback: this
				// should not happen in practice. Return "" to signal
				// no match (defensive).
				return ""
			}
		}
		// Path matches, method not allowed: 405 with Allow.
		return entry.methods
	}
	return ""
}

// pathMatchesPattern reports whether a concrete path matches a ServeMux
// path pattern (the Go 1.22+ pattern syntax: {key} segments match exactly
// one path segment).
func pathMatchesPattern(path, pattern string) bool {
	patternParts := strings.Split(strings.Trim(pattern, "/"), "/")
	pathParts := strings.Split(strings.Trim(path, "/"), "/")
	if len(patternParts) != len(pathParts) {
		return false
	}
	for i, pp := range patternParts {
		if strings.HasPrefix(pp, "{") && strings.HasSuffix(pp, "}") {
			// Wildcard segment: matches any non-empty path segment.
			if pathParts[i] == "" {
				return false
			}
			continue
		}
		if pp != pathParts[i] {
			return false
		}
	}
	return true
}

// healthzHandler returns the base-chain handler for /healthz. It requires
// no authentication and no database: 200 {"status":"ok"}. The dependency
// set is not consulted (liveness never touches the database).
func healthzHandler(deps *Deps) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})
}

// readyzHandler returns the base-chain handler for /readyz. It reports
// 200 {"status":"ready"} iff both hold: (1) the in-process
// migrations-converged flag is set, and (2) a per-request Ping on the
// runtime pool succeeds under a short bounded context. Any failure yields
// the typed 503 unavailable.
func readyzHandler(deps *Deps) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if deps.MigrationsApplied == nil || !deps.MigrationsApplied() {
			writeJSONError(r, w, apierr.ErrUnavailable)
			return
		}
		// A nil pool is a configuration error, not a panic: report it as
		// unavailable. (In production the pool is always non-nil; this
		// guard keeps a misconfigured dependency from panicking into the
		// recovery stage, which would render 500 instead of 503.)
		if deps.Pool == nil {
			writeJSONError(r, w, apierr.ErrUnavailable)
			return
		}
		pingCtx, cancel := context.WithTimeout(r.Context(), pingDuration(deps.PingTimeout))
		defer cancel()
		if err := deps.Pool.Ping(pingCtx); err != nil {
			writeJSONError(r, w, apierr.ErrUnavailable)
			return
		}
		h := w.Header()
		h.Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ready"}`))
	})
}

// pingDuration returns the readiness ping bound (the section-6 proposal of
// 2 s).
func pingDuration(d time.Duration) time.Duration {
	if d <= 0 {
		return 2 * time.Second
	}
	return d
}

// writeJSONError writes a catalog error response through Render. It is used
// by the base-chain handlers (health/readiness) that sit outside the full
// chain and therefore have no recorder in the context.
func writeJSONError(r *http.Request, w http.ResponseWriter, err *apierr.Error) {
	Render(r.Context(), w, err)
}

// writeSuccess writes a success status and a pre-serialized JSON body, then
// checks the write result. The handler owns its success response (section 1).
func writeSuccess(w http.ResponseWriter, status int, body string) error {
	h := w.Header()
	h.Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if _, err := w.Write([]byte(body)); err != nil {
		return err
	}
	return nil
}

// ---------------------------------------------------------------------------
// Data-plane handlers (package 3 service functions)
// ---------------------------------------------------------------------------

// upsertReq is the strict request shape of PUT /v1/namespaces/{ns}/records
// (docs/API.md, "Upsert Records").
type upsertReq struct {
	VectorSpace string                 `json:"vector_space"`
	Records     []vectors.UpsertRecord `json:"records"`
}

// newUpsertHandler returns the upsert handler. It decodes the strict JSON
// body, validates the batch, calls vectors.Upsert, and writes
// {"upserted":N,"unchanged":M}.
func newUpsertHandler(deps *Deps, bounds vectors.Bounds) Handler {
	return func(ctx context.Context, w http.ResponseWriter, in *HandlerInput) *apierr.Error {
		var req upsertReq
		if err := DecodeJSON(in.Req, &req); err != nil {
			return err
		}
		nsKey := in.Path["namespace"]
		st := LogStateOf(ctx)
		if st != nil {
			st.NamespaceKey = nsKey
			st.VectorSpaceKey = req.VectorSpace
		}
		result, err := vectors.Upsert(ctx, deps.Pool, bounds, vectors.UpsertRequest{
			NamespaceKey: nsKey,
			VectorSpace:  req.VectorSpace,
			Records:      req.Records,
		})
		if err != nil {
			return Classify(ctx, err)
		}
		if st != nil {
			n := result.Upserted
			m := result.Unchanged
			st.Upserted = &n
			st.Unchanged = &m
		}
		body := fmt.Sprintf(`{"upserted":%d,"unchanged":%d}`, result.Upserted, result.Unchanged)
		if err := writeSuccess(w, http.StatusOK, body); err != nil {
			return Classify(ctx, err)
		}
		return nil
	}
}

// searchReq is the strict request shape of POST /v1/namespaces/{ns}/search
// (docs/API.md, "Search").
type searchReq struct {
	VectorSpace string        `json:"vector_space"`
	Vector      []float32     `json:"vector"`
	Limit       int           `json:"limit"`
	Filters     searchFilters `json:"filters"`
}

// searchFilters is the constrained filter object: {"all": [entries]}.
type searchFilters struct {
	All []searchFilterEntry `json:"all"`
}

// searchFilterEntry is one structured filter: {field, op, value?/values?}.
type searchFilterEntry struct {
	Field  string `json:"field"`
	Op     string `json:"op"`
	Value  any    `json:"value"`
	Values []any  `json:"values"`
}

// toFilterEntries converts the decoded filter entries into the package-3
// FilterEntry set.
func (f searchFilters) toFilterEntries() ([]vectors.FilterEntry, *apierr.Error) {
	entries := make([]vectors.FilterEntry, 0, len(f.All))
	for _, e := range f.All {
		if e.Field == "" {
			return nil, apierr.ErrInvalidFilter
		}
		switch e.Op {
		case "eq":
			if e.Value == nil {
				return nil, apierr.ErrInvalidFilter
			}
			entries = append(entries, vectors.FilterEntry{
				Field: e.Field, Op: vectors.FilterEq, Values: []any{e.Value},
			})
		case "in":
			if len(e.Values) == 0 {
				return nil, apierr.ErrInvalidFilter
			}
			entries = append(entries, vectors.FilterEntry{
				Field: e.Field, Op: vectors.FilterIn, Values: e.Values,
			})
		default:
			return nil, apierr.ErrInvalidFilter
		}
	}
	return entries, nil
}

// matchOut is one search match as rendered to the client (docs/API.md):
// the stored embedding is never returned.
type matchOut struct {
	RecordID        string         `json:"record_id"`
	ObjectID        string         `json:"object_id"`
	ProjectionID    string         `json:"projection_id"`
	Score           float64        `json:"score"`
	ContentHash     string         `json:"content_hash"`
	SourceUpdatedAt *time.Time     `json:"source_updated_at,omitempty"`
	Metadata        map[string]any `json:"metadata"`
}

// newSearchHandler returns the search handler. It decodes the strict JSON
// body, validates the filters, calls vectors.Search, and writes
// {"matches":[...]}.
func newSearchHandler(deps *Deps, bounds vectors.Bounds) Handler {
	return func(ctx context.Context, w http.ResponseWriter, in *HandlerInput) *apierr.Error {
		var req searchReq
		if err := DecodeJSON(in.Req, &req); err != nil {
			return err
		}
		nsKey := in.Path["namespace"]
		st := LogStateOf(ctx)
		if st != nil {
			st.NamespaceKey = nsKey
			st.VectorSpaceKey = req.VectorSpace
			l := req.Limit
			st.Limit = &l
			fc := len(req.Filters.All)
			st.FilterCount = &fc
		}
		filters, ferr := req.Filters.toFilterEntries()
		if ferr != nil {
			return ferr
		}
		result, err := vectors.Search(ctx, deps.Pool, bounds, vectors.SearchRequest{
			NamespaceKey: nsKey,
			VectorSpace:  req.VectorSpace,
			Embedding:    req.Vector,
			Filters:      filters,
			Limit:        req.Limit,
		})
		if err != nil {
			return Classify(ctx, err)
		}
		matches := make([]matchOut, 0, len(result.Matches))
		for _, m := range result.Matches {
			if m.Score == nil {
				continue
			}
			matches = append(matches, matchOut{
				RecordID:        m.RecordID,
				ObjectID:        m.ObjectID,
				ProjectionID:    m.ProjectionID,
				Score:           *m.Score,
				ContentHash:     m.ContentHash,
				SourceUpdatedAt: m.SourceUpdatedAt,
				Metadata:        m.Metadata,
			})
		}
		if st != nil {
			rc := len(matches)
			st.ResultCount = &rc
		}
		body, err := json.Marshal(map[string]any{"matches": matches})
		if err != nil {
			return apierr.ErrInternal
		}
		if err := writeSuccess(w, http.StatusOK, string(body)); err != nil {
			return Classify(ctx, err)
		}
		return nil
	}
}

// newGetRecordHandler returns the get-record handler. The path carries
// {record_id}; the handler looks the record up by its service-generated
// UUID within the caller's namespace. A record outside the scope returns 404.
func newGetRecordHandler(deps *Deps) Handler {
	return func(ctx context.Context, w http.ResponseWriter, in *HandlerInput) *apierr.Error {
		nsKey := in.Path["namespace"]
		recordID := in.Path["record_id"]
		st := LogStateOf(ctx)
		if st != nil {
			st.NamespaceKey = nsKey
		}
		result, err := getRecordByID(ctx, deps.Pool, nsKey, recordID)
		if err != nil {
			return Classify(ctx, err)
		}
		out := recordOut{
			RecordID:        result.RecordID,
			ObjectID:        result.ObjectID,
			ProjectionID:    result.ProjectionID,
			VectorSpace:     result.VectorSpace,
			ContentHash:     result.ContentHash,
			SourceUpdatedAt: result.SourceUpdatedAt,
			Metadata:        result.Metadata,
			CreatedAt:       result.CreatedAt,
			UpdatedAt:       result.UpdatedAt,
		}
		body, err := json.Marshal(out)
		if err != nil {
			return apierr.ErrInternal
		}
		if err := writeSuccess(w, http.StatusOK, string(body)); err != nil {
			return Classify(ctx, err)
		}
		return nil
	}
}

// getRecordByID resolves the requested record by its service-generated UUID
// within the caller's namespace. The record must belong to the authenticated
// application's namespace. The handler runs inside a transaction with
// transaction-local application context (dbctx.WithAppContext) so RLS and
// explicit WHERE scoping both enforce application and namespace isolation.
func getRecordByID(ctx context.Context, pool *pgxpool.Pool, nsKey, recordID string) (vectors.GetResult, error) {
	if !isCanonicalUUID(recordID) {
		return vectors.GetResult{}, apierr.ErrInvalidUUID
	}
	appID := dbctx.AppID(ctx)
	if appID == "" {
		return vectors.GetResult{}, apierr.ErrInternal
	}
	const stmt = `
		SELECT r.id, r.object_id, r.projection_id, s.vector_space_key,
		       r.content_hash, r.source_updated_at, r.metadata,
		       r.created_at, r.updated_at
		FROM vector_data.vector_records r
		JOIN vector_control.vector_spaces s ON s.id = r.vector_space_id
		WHERE r.id = $1 AND r.application_id = $2 AND r.namespace_id = $3`
	var (
		res       vectors.GetResult
		hashBytea []byte
		metaBytea []byte
		srcAt     pgtype.Timestamptz
		createdAt pgtype.Timestamptz
		updatedAt pgtype.Timestamptz
	)
	err := dbctx.WithAppContext(ctx, pool, appID, func(tx pgx.Tx) error {
		ns, err := namespaces.ResolveInTx(ctx, tx, nsKey)
		if err != nil {
			return err
		}
		return tx.QueryRow(ctx, stmt, recordID, appID, ns.NamespaceID).Scan(
			&res.RecordID, &res.ObjectID, &res.ProjectionID, &res.VectorSpace,
			&hashBytea, &srcAt, &metaBytea, &createdAt, &updatedAt)
	})
	if err != nil {
		if errors.Is(err, namespaces.ErrNotFound) {
			return vectors.GetResult{}, apierr.ErrNamespaceNotFound
		}
		if errors.Is(err, pgx.ErrNoRows) {
			return vectors.GetResult{}, apierr.ErrRecordNotFound
		}
		return vectors.GetResult{}, err
	}
	res.ContentHash = hex.EncodeToString(hashBytea)
	if len(metaBytea) > 0 {
		_ = json.Unmarshal(metaBytea, &res.Metadata)
	}
	if srcAt.Valid {
		t := srcAt.Time
		res.SourceUpdatedAt = &t
	}
	if createdAt.Valid {
		res.CreatedAt = createdAt.Time
	}
	if updatedAt.Valid {
		res.UpdatedAt = updatedAt.Time
	}
	return res, nil
}

// deleteProjectionReq is the strict request shape of DELETE
// /v1/namespaces/{ns}/records (docs/API.md, "Delete Logical Projection").
type deleteProjectionReq struct {
	ObjectID     string `json:"object_id"`
	ProjectionID string `json:"projection_id"`
	VectorSpace  string `json:"vector_space"`
}

// newDeleteProjectionHandler returns the projection-delete handler.
func newDeleteProjectionHandler(deps *Deps) Handler {
	return func(ctx context.Context, w http.ResponseWriter, in *HandlerInput) *apierr.Error {
		var req deleteProjectionReq
		if err := DecodeJSON(in.Req, &req); err != nil {
			return err
		}
		nsKey := in.Path["namespace"]
		st := LogStateOf(ctx)
		if st != nil {
			st.NamespaceKey = nsKey
			st.VectorSpaceKey = req.VectorSpace
		}
		result, err := vectors.DeleteProjection(ctx, deps.Pool, vectors.DeleteRequest{
			NamespaceKey: nsKey,
			ObjectID:     req.ObjectID,
			ProjectionID: req.ProjectionID,
			VectorSpace:  req.VectorSpace,
		})
		if err != nil {
			return Classify(ctx, err)
		}
		if err := writeSuccess(w, http.StatusOK,
			fmt.Sprintf(`{"deleted":%d}`, result.Deleted)); err != nil {
			return Classify(ctx, err)
		}
		return nil
	}
}

// newDeleteObjectHandler returns the object-delete handler. The optional
// ?vector_space= query parameter restricts the delete to that space.
func newDeleteObjectHandler(deps *Deps) Handler {
	return func(ctx context.Context, w http.ResponseWriter, in *HandlerInput) *apierr.Error {
		nsKey := in.Path["namespace"]
		objectID := in.Path["object_id"]
		space := in.Req.URL.Query().Get("vector_space")
		st := LogStateOf(ctx)
		if st != nil {
			st.NamespaceKey = nsKey
			if space != "" {
				st.VectorSpaceKey = space
			}
		}
		var result vectors.DeleteResult
		var err error
		if space != "" {
			result, err = deleteObjectInSpace(ctx, deps.Pool, nsKey, objectID, space)
		} else {
			result, err = vectors.DeleteObject(ctx, deps.Pool, vectors.DeleteRequest{
				NamespaceKey: nsKey,
				ObjectID:     objectID,
			})
		}
		if err != nil {
			return Classify(ctx, err)
		}
		if err := writeSuccess(w, http.StatusOK,
			fmt.Sprintf(`{"deleted":%d}`, result.Deleted)); err != nil {
			return Classify(ctx, err)
		}
		return nil
	}
}

// deleteObjectInSpace deletes the object's projections within one vector
// space. It runs a WithAppContext transaction (the transaction-local
// application context + RLS scope the operation requires).
func deleteObjectInSpace(ctx context.Context, pool *pgxpool.Pool, nsKey, objectID, spaceKey string) (vectors.DeleteResult, error) {
	if !isCanonicalUUID(objectID) {
		return vectors.DeleteResult{}, apierr.ErrInvalidUUID
	}
	appID := dbctx.AppID(ctx)
	if appID == "" {
		return vectors.DeleteResult{}, apierr.ErrInternal
	}
	var result vectors.DeleteResult
	err := dbctx.WithAppContext(ctx, pool, appID, func(tx pgx.Tx) error {
		var nsID, spaceID string
		if err := tx.QueryRow(ctx,
			`SELECT id FROM vector_control.namespaces
			 WHERE application_id = $1 AND namespace_key = $2 AND enabled`,
			appID, nsKey).Scan(&nsID); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return namespaces.ErrNotFound
			}
			return err
		}
		if err := tx.QueryRow(ctx,
			`SELECT id FROM vector_control.vector_spaces WHERE vector_space_key = $1`,
			spaceKey).Scan(&spaceID); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return vectors.ErrSpaceNotFound
			}
			return err
		}
		tag, err := tx.Exec(ctx, `
			DELETE FROM vector_data.vector_records
			WHERE namespace_id = $1
			  AND object_id = $2
			  AND vector_space_id = $3`,
			nsID, objectID, spaceID)
		if err != nil {
			return err
		}
		result.Deleted = int(tag.RowsAffected())
		return nil
	})
	if err != nil {
		return vectors.DeleteResult{}, err
	}
	return result, nil
}

// ---------------------------------------------------------------------------
// Admin handlers
// ---------------------------------------------------------------------------

// registerApplicationReq is the strict request shape of POST
// /v1/admin/applications (docs/API.md, "Register Application").
type registerApplicationReq struct {
	ApplicationKey string `json:"application_key"`
	DisplayName    string `json:"display_name"`
}

// newRegisterApplicationHandler returns the register-application handler.
func newRegisterApplicationHandler(deps *Deps) Handler {
	return func(ctx context.Context, w http.ResponseWriter, in *HandlerInput) *apierr.Error {
		var req registerApplicationReq
		if err := DecodeJSON(in.Req, &req); err != nil {
			return err
		}
		if !appKeyPattern.MatchString(req.ApplicationKey) {
			return apierr.ErrInvalidApplicationKey
		}
		id, err := newUUID()
		if err != nil {
			return apierr.ErrInternal
		}
		err = dbctx.WithShortTx(ctx, deps.Pool, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `
				INSERT INTO vector_control.applications
					(id, application_key, display_name, enabled)
				VALUES ($1, $2, $3, true)`, id, req.ApplicationKey, req.DisplayName)
			return err
		})
		if err != nil {
			return Classify(ctx, err)
		}
		if st := LogStateOf(ctx); st != nil {
			st.ApplicationID = id
			st.ApplicationKey = req.ApplicationKey
		}
		body := fmt.Sprintf(`{"id":%q,"application_key":%q,"display_name":%q,"enabled":true}`,
			id, req.ApplicationKey, req.DisplayName)
		if err := writeSuccess(w, http.StatusOK, body); err != nil {
			return Classify(ctx, err)
		}
		return nil
	}
}

// registerNamespaceReq is the strict request shape of POST
// /v1/admin/applications/{application}/namespaces.
type registerNamespaceReq struct {
	NamespaceKey string         `json:"namespace_key"`
	DisplayName  string         `json:"display_name"`
	Metadata     map[string]any `json:"metadata"`
}

// newRegisterNamespaceHandler returns the register-namespace handler. It
// resolves {application}, validates the namespace key grammar, and inserts
// the namespace under a WithAppContext transaction (the forced RLS WITH
// CHECK on namespaces requires the app context).
func newRegisterNamespaceHandler(deps *Deps, cfg *config.Config) Handler {
	return func(ctx context.Context, w http.ResponseWriter, in *HandlerInput) *apierr.Error {
		var req registerNamespaceReq
		if err := DecodeJSON(in.Req, &req); err != nil {
			return err
		}
		app, err := resolveApplication(ctx, deps.Pool, in.Path["application"])
		if err != nil {
			return Classify(ctx, err)
		}
		if req.NamespaceKey == "" || strings.ContainsAny(req.NamespaceKey, "/\x00") || len(req.NamespaceKey) > 200 {
			return apierr.ErrInvalidNamespaceKey
		}
		if req.Metadata == nil {
			req.Metadata = map[string]any{}
		}
		metaBytes, err := json.Marshal(req.Metadata)
		if err != nil {
			return apierr.ErrMetadataNotObject
		}
		if len(metaBytes) > cfg.MaxMetadataBytes {
			return apierr.ErrMetadataTooLarge
		}
		nsID, err := newUUID()
		if err != nil {
			return apierr.ErrInternal
		}
		err = dbctx.WithAppContext(ctx, deps.Pool, app.id, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `
				INSERT INTO vector_control.namespaces
					(id, application_id, namespace_key, display_name, enabled, metadata)
				VALUES ($1, $2, $3, $4, true, $5::jsonb)`,
				nsID, app.id, req.NamespaceKey, req.DisplayName, string(metaBytes))
			return err
		})
		if err != nil {
			return Classify(ctx, err)
		}
		if st := LogStateOf(ctx); st != nil {
			st.ApplicationID = app.id
			st.ApplicationKey = app.key
			st.NamespaceID = nsID
			st.NamespaceKey = req.NamespaceKey
		}
		body := fmt.Sprintf(`{"id":%q,"application_key":%q,"namespace_key":%q,"display_name":%q,"enabled":true,"metadata":%s}`,
			nsID, app.key, req.NamespaceKey, req.DisplayName, string(metaBytes))
		if err := writeSuccess(w, http.StatusOK, body); err != nil {
			return Classify(ctx, err)
		}
		return nil
	}
}

// createCredentialReq is the strict request shape of POST
// /v1/admin/applications/{application}/credentials.
type createCredentialReq struct {
	CredentialName string `json:"credential_name"`
}

// newCreateCredentialHandler returns the create-credential handler. It
// resolves {application}, generates the opaque credential, persists only
// its digest, and returns the raw credential exactly once.
func newCreateCredentialHandler(deps *Deps) Handler {
	return func(ctx context.Context, w http.ResponseWriter, in *HandlerInput) *apierr.Error {
		var req createCredentialReq
		if err := DecodeJSON(in.Req, &req); err != nil {
			return err
		}
		if req.CredentialName == "" {
			return apierr.ErrMissingField
		}
		app, err := resolveApplication(ctx, deps.Pool, in.Path["application"])
		if err != nil {
			return Classify(ctx, err)
		}
		cred, err := auth.GenerateCredential()
		if err != nil {
			return apierr.ErrInternal
		}
		digest := auth.Digest(cred)
		id, err := newUUID()
		if err != nil {
			return apierr.ErrInternal
		}
		err = dbctx.WithShortTx(ctx, deps.Pool, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `
				INSERT INTO vector_control.application_credentials
					(id, application_id, credential_name, credential_hash)
				VALUES ($1, $2, $3, $4)`, id, app.id, req.CredentialName, digest[:])
			return err
		})
		if err != nil {
			return Classify(ctx, err)
		}
		if st := LogStateOf(ctx); st != nil {
			st.ApplicationID = app.id
			st.ApplicationKey = app.key
		}
		body := fmt.Sprintf(`{"credential_id":%q,"credential_name":%q,"credential":%q}`,
			id, req.CredentialName, cred)
		if err := writeSuccess(w, http.StatusOK, body); err != nil {
			return Classify(ctx, err)
		}
		return nil
	}
}

// newDisableCredentialHandler returns the disable-credential handler. It
// resolves {application}, validates {credential_id}, and sets enabled=false
// (the row is never deleted).
func newDisableCredentialHandler(deps *Deps) Handler {
	return func(ctx context.Context, w http.ResponseWriter, in *HandlerInput) *apierr.Error {
		app, err := resolveApplication(ctx, deps.Pool, in.Path["application"])
		if err != nil {
			return Classify(ctx, err)
		}
		credID := in.Path["credential_id"]
		if !isCanonicalUUID(credID) {
			return apierr.ErrInvalidUUID
		}
		var affected int64
		err = dbctx.WithShortTx(ctx, deps.Pool, func(tx pgx.Tx) error {
			tag, err := tx.Exec(ctx, `
				UPDATE vector_control.application_credentials
				SET enabled = false
				WHERE id = $1 AND application_id = $2`, credID, app.id)
			if err != nil {
				return err
			}
			affected = tag.RowsAffected()
			return nil
		})
		if err != nil {
			return Classify(ctx, err)
		}
		if affected == 0 {
			return apierr.ErrCredentialNotFound
		}
		if st := LogStateOf(ctx); st != nil {
			st.ApplicationID = app.id
			st.ApplicationKey = app.key
		}
		if err := writeSuccess(w, http.StatusOK, `{"disabled":true}`); err != nil {
			return Classify(ctx, err)
		}
		return nil
	}
}

// applicationRow is a resolved application for an admin handler.
type applicationRow struct {
	id  string
	key string
}

// resolveApplication resolves the {application} path key: grammar check
// (400 invalid_application_key) then a keyed lookup (404 application_not_found).
func resolveApplication(ctx context.Context, pool *pgxpool.Pool, key string) (applicationRow, error) {
	if !appKeyPattern.MatchString(key) {
		return applicationRow{}, apierr.ErrInvalidApplicationKey
	}
	var row applicationRow
	err := dbctx.WithShortTx(ctx, pool, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT id, application_key FROM vector_control.applications
			 WHERE application_key = $1`, key).
			Scan(&row.id, &row.key)
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return applicationRow{}, apierr.ErrApplicationNotFound
		}
		return applicationRow{}, err
	}
	return row, nil
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

var appKeyPattern = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,63}$`)

// recordOut is the get-record response shape (docs/API.md, "Get Record"):
// the stored embedding is never returned.
type recordOut struct {
	RecordID        string         `json:"record_id"`
	ObjectID        string         `json:"object_id"`
	ProjectionID    string         `json:"projection_id"`
	VectorSpace     string         `json:"vector_space"`
	ContentHash     string         `json:"content_hash"`
	SourceUpdatedAt *time.Time     `json:"source_updated_at,omitempty"`
	Metadata        map[string]any `json:"metadata"`
	CreatedAt       time.Time      `json:"created_at"`
	UpdatedAt       time.Time      `json:"updated_at"`
}

// newUUID returns a random UUIDv4 (RFC 4122) as a lowercase hyphenated string.
func newUUID() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	raw[6] = (raw[6] & 0x0f) | 0x40 // version 4
	raw[8] = (raw[8] & 0x3f) | 0x80 // variant 10
	return fmt.Sprintf("%x-%x-%x-%x-%x", raw[0:4], raw[4:6], raw[6:8], raw[8:10], raw[10:16]), nil
}

// isCanonicalUUID reports whether s is a lowercase hyphenated UUID (8-4-4-4-12).
func isCanonicalUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i, c := range s {
		switch i {
		case 8, 13, 18, 23:
			if c != '-' {
				return false
			}
		default:
			if !isHexByte(c) {
				return false
			}
		}
	}
	return true
}

func isHexByte(c rune) bool {
	return (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')
}
