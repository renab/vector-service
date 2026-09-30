//go:build integration

package integration

import (
	"bytes"
	"context"
	"crypto/rand"

	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"vector-service/internal/api"
	"vector-service/internal/apierr"
	"vector-service/internal/auth"
	"vector-service/internal/config"
	"vector-service/internal/dbctx"
	"vector-service/internal/namespaces"
	"vector-service/internal/testdb"
	"vector-service/internal/vectors"
)

// The package-5 logging matrix, cases 1–2 (release-critical): it proves the
// diagnostic-record policy holds when REAL PostgreSQL driver errors reach
// the classifier — a real pgvector 22P02 (malformed vector) and real P0001
// trigger text (dimension mismatch and unknown space) — by running the
// production classification boundary (api.Classify + emitDiagnostic) behind
// the production middleware chain, capturing every structured log record the
// service emits through a test-owned slog JSON sink, and asserting:
//
//  1. the response carries the stable catalog error (no driver text);
//  2. the absence check passes over the whole case marker set (the raw
//     server message, the malformed literal, the trigger text, and the
//     embedded space UUID never appear in any captured record — full or
//     truncated form);
//  3. the diagnostic record is preserved, not silenced — it carries its
//     stable fields (code, status, SQLSTATE, request ID);
//  4. the negative control (canary) proves the absence check is not
//     vacuous: an injected marker makes the check fail.
//
// The test-only vector-bypass seam (the same route pattern package-4
// validation uses for panic injection) bypasses the Go-level vector
// pre-validation (vectors.CheckDimensions / CheckZeroNorm / EncodeText) and
// forwards the crafted malformed vector literal to the database so the real
// pgx 22P02 / P0001 error reaches the classifier. The seam is a test-local
// api.Handler mounted behind the production full chain (Chain.FullChain)
// with a test-owned auth seam that resolves the fixture application identity;
// it resolves the namespace, runs the raw INSERT that fires the real
// vector_records_validate trigger (set_config + RLS established by
// WithAppContext), and classifies the resulting driver error through
// api.Classify — the exact production boundary under test. It exists only
// in this test file; no production code references it.
//
// The test skips when VECTOR_SERVICE_TEST_PG_DSN is unset (the testdb
// bootstrap identity is not configured), so the unit layer stays green on a
// host with no database.

// ---------------------------------------------------------------------------
// Fixture
// ---------------------------------------------------------------------------

// loggingFixture is one test's provisioned suite: the runtime pool under
// test (the canonical vector_api role, RLS active), the bootstrap
// connection for fixture SQL, the registered application/namespace, and the
// generated data-plane credential.
type loggingFixture struct {
	pool      *pgxpool.Pool
	runtime   *pgx.Conn // direct vector_api connection for DB-layer verification
	bootstrap *pgx.Conn

	appID       string
	appKey      string
	nsID        string
	nsKey       string
	credential  string
	adminToken  string // known admin token for production admin auth
	spaceID     string
	spaceKey    string
	spaceDims   int
	unknownUUID string // a generated, non-existent vector_space_id (case 2)

	server  *httptest.Server // production router server (cases 3–5)
	seamSrv *httptest.Server // seam server for vector-bypass (cases 1–2)
}

// setupLoggingFixture builds the suite fixture and skips when the bootstrap
// identity is not configured.
func setupLoggingFixture(t *testing.T) *loggingFixture {
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
	pc, perr := pgxpool.ParseConfig(
		fmt.Sprintf("host=%s port=%d dbname=%s user=%s sslmode=%s",
			connCfg.Host, connCfg.Port, connCfg.Database, connCfg.User,
			sslModeForLogging(connCfg)))
	if perr != nil {
		runtimeConn.Close(ctx)
		t.Fatalf("parse runtime pool config: %v", perr)
	}
	pool, perr := pgxpool.NewWithConfig(ctx, pc)
	if perr != nil {
		runtimeConn.Close(ctx)
		t.Fatalf("open runtime pool: %v", perr)
	}
	t.Cleanup(pool.Close)

	bootConn, berr := h.BootstrapConn(ctx, testdb.DatabaseName)
	if berr != nil {
		t.Fatalf("bootstrap connect: %v", berr)
	}
	t.Cleanup(func() { bootConn.Close(context.Background()) })

	t.Cleanup(func() { runtimeConn.Close(context.Background()) })
	fx := &loggingFixture{
		pool:      pool,
		runtime:   runtimeConn,
		bootstrap: bootConn,
		appKey:    testdb.AppMemoryService,
		nsKey:     testdb.NamespaceProjectAlpha,
		spaceID:   testdb.SeededSpaceID,
		spaceKey:  testdb.SeededSpaceKey,
		spaceDims: testdb.SeededSpaceDimensions,
	}
	if fx.appID, err = testdb.NewUUID(); err != nil {
		t.Fatalf("generate application UUID: %v", err)
	}
	if fx.nsID, err = testdb.NewUUID(); err != nil {
		t.Fatalf("generate namespace UUID: %v", err)
	}
	if fx.unknownUUID, err = testdb.NewUUID(); err != nil {
		t.Fatalf("generate unknown-space UUID: %v", err)
	}
	if fx.credential, err = auth.GenerateCredential(); err != nil {
		t.Fatalf("generate credential: %v", err)
	}
	if err := fx.insertApplication(ctx); err != nil {
		t.Fatalf("insert application: %v", err)
	}
	if err := fx.insertNamespace(ctx); err != nil {
		t.Fatalf("insert namespace: %v", err)
	}
	if err := fx.insertCredential(ctx); err != nil {
		t.Fatalf("insert credential: %v", err)
	}

	// Generate a known admin token for production admin auth (Case 5).
	if fx.adminToken, err = auth.GenerateCredential(); err != nil {
		t.Fatalf("generate admin token: %v", err)
	}

	// Build the production router (cases 3–5). NewRouterShared wires up the
	// real FullChain with real auth stages, real handlers, and real render.
	// The only seams are: the pool (test-owned) and the admin token (known).
	// Data-plane auth works because the fixture credential row is already
	// seeded; admin auth works because we supply the known token.
	cfg := loggingTestConfig()
	cfg.AdminToken = fx.adminToken
	deps := &api.Deps{
		Pool:              fx.pool,
		MigrationsApplied: func() bool { return true },
		PingTimeout:       2 * time.Second,
	}
	fx.server = httptest.NewServer(api.NewRouterShared(cfg, deps))
	t.Cleanup(func() { fx.server.Close() })

	// Build the seam server (cases 1–2 only: vector-bypass seam).
	fx.seamSrv = httptest.NewServer(newLoggingSeamHandler(fx))
	t.Cleanup(func() { fx.seamSrv.Close() })

	return fx
}

// insertApplication registers the fixture application row through bootstrap
// SQL (the same boundary the router and vectors integration suites use).
func (f *loggingFixture) insertApplication(ctx context.Context) error {
	_, err := f.bootstrap.Exec(ctx,
		`INSERT INTO vector_control.applications (id, application_key, display_name)
		 VALUES ($1, $2, $3)`,
		f.appID, testdb.AppMemoryService, "memory-service (fixture)")
	return err
}

// insertNamespace registers the fixture namespace row through bootstrap SQL.
func (f *loggingFixture) insertNamespace(ctx context.Context) error {
	_, err := f.bootstrap.Exec(ctx,
		`INSERT INTO vector_control.namespaces (id, application_id, namespace_key, display_name)
		 VALUES ($1, $2, $3, $4)`,
		f.nsID, f.appID, f.nsKey, "project-alpha (fixture)")
	return err
}

// insertCredential seeds a data-plane credential row (keyed digest only —
// the raw secret is never persisted) through bootstrap SQL.
func (f *loggingFixture) insertCredential(ctx context.Context) error {
	id, err := testdb.NewUUID()
	if err != nil {
		return err
	}
	digest := auth.Digest(f.credential)
	_, err = f.bootstrap.Exec(ctx,
		`INSERT INTO vector_control.application_credentials
			(id, application_id, credential_name, credential_hash, enabled)
		 VALUES ($1, $2, 'fixture-credential', $3, true)`,
		id, f.appID, digest[:])
	return err
}

// sslModeForLogging returns the sslmode for the test pool.
// The harness always uses TLSModePlain (VEC_PG_TLS_MODE=plain), so test
// pools must explicitly disable SSL.
func sslModeForLogging(_ *pgx.ConnConfig) string {
	return "disable"
}

// ---------------------------------------------------------------------------
// The test-only vector-bypass seam
// ---------------------------------------------------------------------------

// seamRequest is the crafted body the vector-bypass seam accepts. The
// embedding field is a RAW pgvector text literal (a string), not the JSON
// array-of-numbers the production upsert decodes — the seam bypasses the Go
// codec and the pre-validation and forwards the literal verbatim to the
// database, so the real trigger and the real driver error fire.
type seamRequest struct {
	// Embedding is the raw vector literal (a string) bound as the embedding
	// parameter. For case 1 it is a malformed literal (its first element is
	// the per-test marker); for case 2 it is a valid literal with a wrong
	// dimension (dimension mismatch) or paired with an unknown space.
	Embedding string `json:"embedding"`

	// UnknownSpaceUUID, when set, is bound as vector_space_id instead of the
	// resolved space ID (the case-2 unknown-space form).
	UnknownSpaceUUID string `json:"unknown_space_uuid,omitempty"`

	// Metadata, when set, is the caller metadata object the raw-literal seam
	// binds (its canonical JSON form, the same canonical form the
	// production upsert binds) for the metadata-logging case.
	Metadata map[string]any `json:"metadata,omitempty"`
}

// seamPayload is a deterministic content hash / object / projection so the
// raw INSERT is well-formed against the schema (only the embedding and the
// vector_space_id vary per case).
type seamPayload struct {
	objectID     string
	projectionID string
	contentHash  string // 32 raw bytes, hex-encoded in the body
}

// seamHandler is the test-local api.Handler for the vector-bypass seam: it
// bypasses Go-level vector pre-validation (CheckDimensions / CheckZeroNorm /
// EncodeText) and forwards the crafted raw vector literal to the database so
// the real pgx 22P02 / P0001 driver error reaches the production classifier.
// It is mounted behind Chain.FullChain with a test-owned auth seam — the
// production middleware chain under test.
func seamHandler(fx *loggingFixture) api.Handler {
	const stmt = `
		INSERT INTO vector_data.vector_records
			(id, application_id, namespace_id, vector_space_id,
			 object_id, projection_id, content_hash, metadata, embedding)
		 VALUES ($1, $2, $3, $4, $5, $6, $7::bytea, '{}'::jsonb, $8::vector)`

	return func(ctx context.Context, w http.ResponseWriter, in *api.HandlerInput) *apierr.Error {
		var req seamRequest
		if err := api.DecodeJSON(in.Req, &req); err != nil {
			return err
		}

		// A deterministic, obviously-synthetic payload for the well-formed
		// columns (the schema requires a 32-byte content hash and non-empty
		// object/projection).
		p := seamPayload{
			objectID:     "0199f31e-a000-7000-8000-0000000000c1",
			projectionID: "0199f31e-a000-7000-8000-0000000000c2",
			contentHash:  "000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f",
		}
		hashBytes, herr := hex.DecodeString(p.contentHash)
		if herr != nil {
			return api.Classify(ctx, herr)
		}
		recordID, rerr := testdb.NewUUID()
		if rerr != nil {
			return api.Classify(ctx, rerr)
		}

		// Resolve the namespace and space, then run the raw INSERT that fires
		// the trigger — all in one WithAppContext transaction, the production
		// upsert's shape minus the Go pre-validation. The application identity
		// is already on the context from the auth stage.
		var resolvedNS namespaces.Result
		var boundSpaceID string
		txErr := dbctx.WithAppContext(ctx, fx.pool, fx.appID, func(tx pgx.Tx) error {
			ns, err := namespaces.ResolveInTx(ctx, tx, fx.nsKey)
			if err != nil {
				return err
			}
			resolvedNS = ns
			if req.UnknownSpaceUUID != "" {
				// Case-2 unknown-space form: bind the non-existent UUID
				// directly (no resolution — that is the point).
				boundSpaceID = req.UnknownSpaceUUID
				_, err = tx.Exec(ctx, stmt,
					recordID, fx.appID, ns.NamespaceID, boundSpaceID,
					p.objectID, p.projectionID, hashBytes, req.Embedding)
				return err
			}
			space, err := vectors.ResolveInTx(ctx, tx, fx.spaceKey, true)
			if err != nil {
				return err
			}
			boundSpaceID = space.ID
			_, err = tx.Exec(ctx, stmt,
				recordID, fx.appID, ns.NamespaceID, boundSpaceID,
				p.objectID, p.projectionID, hashBytes, req.Embedding)
			return err
		})

		// Establish the per-request log state the diagnostic record reads:
		// the application identity (already on the context from auth stage)
		// and the resolved namespace. buildDiagnostic reads dbctx.AppID and
		// namespaces.Namespace from this context.
		logCtx := dbctx.WithAppID(ctx, fx.appID)
		if resolvedNS.NamespaceID != "" {
			logCtx = namespaces.WithContext(logCtx, resolvedNS)
		}

		if txErr != nil {
			return api.Classify(logCtx, txErr)
		}
		// The INSERT succeeded (unexpected for the crafted cases): return an
		// internal error so the test fails loudly rather than silently.
		return api.Classify(logCtx, fmt.Errorf("seam: unexpected successful insert"))
	}
}

// newLoggingSeamHandler mounts the vector-bypass seam behind the production
// full chain (Chain.FullChain) with a test-owned auth seam. The auth seam
// resolves the fixture's application identity — the same propagation the
// production data-plane auth stage performs. The body-cap stage is enabled
// (the production contract for body endpoints).
func newLoggingSeamHandler(fx *loggingFixture) http.Handler {
	chain := &api.Chain{
		Deadline:     &api.OperationDeadline{Timeout: config.DefaultHTTPRequestTimeout},
		MaxBodyBytes: config.DefaultHTTPMaxBodyBytes,
	}
	// Test-owned auth seam: resolves the fixture application identity,
	// mirroring what the production data-plane auth stage does.
	testAuth := func(ctx context.Context, r *http.Request) (string, error) {
		return fx.appID, nil
	}
	return chain.FullChain("seam", testAuth, true, seamHandler(fx))
}

// ---------------------------------------------------------------------------
// slog JSON sink capture
// ---------------------------------------------------------------------------

// capturedRecord is one structured log record as the service emitted it: the
// level, the message, and the complete ordered field set. The fields are the
// exact key/value pairs the sink observed — the absence check serializes each
// value to the string the sink emits and searches for markers.
type capturedRecord struct {
	Level  slog.Level
	Msg    string
	Fields []slog.Attr
}

// logSink is the test-owned slog JSON sink: it records every record it
// handles into a guarded slice, and it is the ONLY place the test inspects
// the serialized form of a field value (through attrValueString, the same
// rendering the JSON handler uses).
type logSink struct {
	mu      sync.Mutex
	records []capturedRecord
}

func (s *logSink) reset() {
	s.mu.Lock()
	s.records = nil
	s.mu.Unlock()
}

func (s *logSink) enable() *slog.Logger {
	prev := slog.Default()
	slog.SetDefault(slog.New(&jsonLogHandler{sink: s}))
	return prev
}

func (s *logSink) disable(prev *slog.Logger) {
	slog.SetDefault(prev)
}

func (s *logSink) snapshot() []capturedRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]capturedRecord, len(s.records))
	copy(out, s.records)
	return out
}

// recordCount returns the current number of captured records.
func (s *logSink) recordCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.records)
}

// extractRange returns the records in [start, end) and removes them from
// the sink, shifting later records to fill the gap. This is used to extract
// canary records from the production capture window.
func (s *logSink) extractRange(start, end int) []capturedRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]capturedRecord, end-start)
	copy(out, s.records[start:end])
	// Remove the range, preserving records before and after.
	s.records = append(s.records[:start], s.records[end:]...)
	return out
}

// jsonLogHandler is the test-owned slog.Handler: it correctly implements
// slog group semantics (nested objects, not flattened keys) by tracking
// group boundaries and building the nested field structure at Handle time.
// Each captured record carries top-level fields; fields whose value is a
// group are stored as slog.Attr with Value.Any() == map[string]any.
type jsonLogHandler struct {
	sink      *logSink
	attrs     []slog.Attr // accumulated handler-level attributes
	groupCuts []groupCut  // indices and names where each group starts
}

// groupCut records the name of a group and the index in attrs where it starts.
type groupCut struct {
	name string
	idx  int
}

func (h *jsonLogHandler) Enabled(_ context.Context, _ slog.Level) bool { return true }

func (h *jsonLogHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	newAttrs := make([]slog.Attr, len(h.attrs), len(h.attrs)+len(attrs))
	copy(newAttrs, h.attrs)
	newAttrs = append(newAttrs, attrs...)
	return &jsonLogHandler{
		sink:      h.sink,
		attrs:     newAttrs,
		groupCuts: slices.Clone(h.groupCuts),
	}
}

func (h *jsonLogHandler) WithGroup(name string) slog.Handler {
	newAttrs := make([]slog.Attr, len(h.attrs))
	copy(newAttrs, h.attrs)
	newGroups := make([]groupCut, len(h.groupCuts)+1)
	copy(newGroups, h.groupCuts)
	newGroups[len(h.groupCuts)] = groupCut{name: name, idx: len(newAttrs)}
	return &jsonLogHandler{
		sink:      h.sink,
		attrs:     newAttrs,
		groupCuts: newGroups,
	}
}

// attrToMapValue converts a slog.Attr value to the map structure used by
// scanValue/containsNeedle. For slog.Group values (whose Value.Any() returns
// []slog.Attr), it recursively converts the group into a nested
// map[string]any. For all other kinds it returns Value.Any() directly.
func attrToMapValue(a slog.Attr) any {
	if a.Value.Kind() == slog.KindGroup {
		groupAttrs, ok := a.Value.Any().([]slog.Attr)
		if !ok {
			return a.Value.Any()
		}
		return slogGroupToMap(groupAttrs)
	}
	return a.Value.Any()
}

// slogGroupToMap converts a []slog.Attr (the internal representation of
// slog.Group) into a map[string]any, recursively converting any nested
// groups. This matches the structure produced by slog.JSONHandler.
func slogGroupToMap(attrs []slog.Attr) map[string]any {
	m := make(map[string]any, len(attrs))
	for _, a := range attrs {
		m[a.Key] = attrToMapValue(a)
	}
	return m
}

// buildGroupStruct builds the nested map[string]any structure for all groups
// and their attributes. It processes groups from innermost to outermost,
// nesting each group under its name within its parent. Group-valued attrs
// inside groups are recursively converted to map[string]any so that
// scanValue/containsNeedle can descend into them.
func buildGroupStruct(groups []groupCut, attrs []slog.Attr) map[string]any {
	if len(groups) == 0 {
		return nil
	}
	// Start with the innermost group.
	lastIdx := len(groups) - 1
	lastStart := groups[lastIdx].idx
	result := make(map[string]any)
	for i := lastStart; i < len(attrs); i++ {
		a := attrs[i]
		result[a.Key] = attrToMapValue(a)
	}
	// Process outer groups, nesting the accumulated result.
	for i := len(groups) - 2; i >= 0; i-- {
		outer := make(map[string]any)
		groupStart := groups[i].idx
		nextStart := groups[i+1].idx
		for j := groupStart; j < nextStart; j++ {
			a := attrs[j]
			outer[a.Key] = attrToMapValue(a)
		}
		outer[groups[i+1].name] = result
		result = outer
	}
	return result
}

func (h *jsonLogHandler) Handle(_ context.Context, r slog.Record) error {
	rec := capturedRecord{Level: r.Level, Msg: r.Message}
	// Build top-level fields from handler attrs (those before the first group).
	// When there are no groups, all handler attrs are top-level.
	firstGroupStart := len(h.attrs)
	if len(h.groupCuts) > 0 {
		firstGroupStart = h.groupCuts[0].idx
	}
	for i := 0; i < firstGroupStart; i++ {
		rec.Fields = append(rec.Fields, slog.Any(h.attrs[i].Key, attrToMapValue(h.attrs[i])))
	}
	// Add group fields (nested maps) and record-level attributes.
	if len(h.groupCuts) > 0 {
		// Temporarily append record attrs to the handler attrs for group building.
		allAttrs := make([]slog.Attr, len(h.attrs))
		copy(allAttrs, h.attrs)
		r.Attrs(func(a slog.Attr) bool {
			allAttrs = append(allAttrs, a)
			return true
		})
		nested := buildGroupStruct(h.groupCuts, allAttrs)
		// The top-level group becomes a field.
		rec.Fields = append(rec.Fields, slog.Any(h.groupCuts[0].name, nested))
	} else {
		r.Attrs(func(a slog.Attr) bool {
			// Convert group-valued attrs (slog.Group) to map[string]any so
			// that scanValue/containsNeedle can descend into them.
			if a.Value.Kind() == slog.KindGroup {
				rec.Fields = append(rec.Fields, slog.Any(a.Key, attrToMapValue(a)))
			} else {
				rec.Fields = append(rec.Fields, a)
			}
			return true
		})
	}
	h.sink.mu.Lock()
	h.sink.records = append(h.sink.records, rec)
	h.sink.mu.Unlock()
	return nil
}

// attrValueString renders a slog.Attr's value to the exact string the JSON
// handler emits for it: strings as their content (the JSON encoder would add
// quotes and escape, but for a substring-absence check the unquoted content
// is the conservative choice — if the marker is absent from the content it is
// absent from the JSON-encoded form too, and a quoted form can only ADD
// characters around the content, never remove the content substring). Other
// kinds are rendered through slog's Value.String, the handler's own
// rendering for non-string attributes.
func attrValueString(a slog.Attr) string {
	switch a.Value.Kind() {
	case slog.KindString:
		return a.Value.String()
	default:
		return a.Value.String()
	}
}

// recordHasTag reports whether a captured record carries a field with the
// given key (used to assert the canary window never leaks into a production
// window: no production record carries the logreg_canary tag).
func recordHasTag(rec capturedRecord, key string) bool {
	for _, a := range rec.Fields {
		if a.Key == key {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// Absence check
// ---------------------------------------------------------------------------

// isUUID reports whether s is a canonical UUID (36 chars, 4 hyphens at the
// fixed positions). UUIDs are fixed-length identifiers: a truncated UUID is
// not a meaningful leak form (the absence check must not flag a partial
// UUID match against a different UUID in the same record).
func isUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for _, want := range []int{8, 13, 18, 23} {
		if s[want] != '-' {
			return false
		}
	}
	return true
}

// properPrefixes returns, for each marker, the marker itself and every
// proper prefix longer than 8 characters (the truncated forms). A leak of a
// truncated form (a quoted vector prefix, a truncated token) must fail the
// check exactly as a full leak does. UUID-length markers (fixed-length
// identifiers) are added as exact markers only — their prefixes are not
// meaningful leak forms.
func properPrefixes(markers ...string) []string {
	var out []string
	seen := map[string]bool{}
	add := func(s string) {
		if s != "" && !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	for _, m := range markers {
		add(m)
		if isUUID(m) {
			continue
		}
		for i := 9; i < len(m); i++ {
			add(m[:i])
		}
	}
	return out
}

// scanValue recursively checks a value for marker needles. For map[string]any
// (nested slog groups), it descends into each entry. For all other types it
// renders the value through Value.String and checks the rendered form.
func scanValue(t *testing.T, where string, recIdx int, key string, val any, needles []string) {
	t.Helper()
	switch v := val.(type) {
	case map[string]any:
		// Nested group: recurse into each entry.
		for k, inner := range v {
			scanValue(t, where, recIdx, key+"."+k, inner, needles)
		}
	default:
		rendered := fmt.Sprintf("%v", v)
		for _, n := range needles {
			if strings.Contains(rendered, n) {
				t.Fatalf("%s: marker %q (len %d) leaked into record %d field %q (value: %s)",
					where, n, len(n), recIdx, key, rendered)
			}
		}
	}
}

// absenceCheck fails t if any marker (or any proper prefix of a marker
// longer than 8 characters) occurs as a substring of any field value of any
// captured record. It returns the count of records inspected so the test can
// assert the window was non-empty (a vacuous window passes vacuously).
// For fields whose value is a nested map (slog groups), it recursively
// scans all nested entries.
func absenceCheck(t *testing.T, where string, records []capturedRecord, markers ...string) int {
	t.Helper()
	if len(records) == 0 {
		t.Fatalf("%s: capture window is empty — the absence check is vacuous", where)
	}
	needles := properPrefixes(markers...)
	for i, rec := range records {
		for _, a := range rec.Fields {
			scanValue(t, where, i, a.Key, a.Value.Any(), needles)
		}
		// The message is logged text too; check it as well.
		for _, n := range needles {
			if strings.Contains(rec.Msg, n) {
				t.Fatalf("%s: marker %q (len %d) leaked into record %d message %q",
					where, n, len(n), i, rec.Msg)
			}
		}
	}
	return len(records)
}

// emitCanaryRecords emits canary record(s) carrying the injected marker into
// the SAME test-owned slog sink in a separate capture window, proving the
// absence check is not vacuous. For each marker it emits the full marker and,
// if the marker is long enough, a proper prefix of more than 8 characters
// (the truncated form). The emitted records are extracted from the sink and
// returned; they are removed from the production capture window so no
// production record carries the logreg_canary tag.
func emitCanaryRecords(t *testing.T, sink *logSink, markers ...string) []capturedRecord {
	t.Helper()
	start := sink.recordCount()
	for _, m := range markers {
		// The full marker.
		slog.Info("canary", "logreg_canary", "1", "logreg_marker", m)
		// A proper prefix of more than 8 characters (the truncated form),
		// when the marker is long enough to have one.
		if len(m) > 9 {
			slog.Info("canary", "logreg_canary", "1", "logreg_marker", m[:len(m)-1])
		}
	}
	end := sink.recordCount()
	// Extract the canary records from the sink and remove them from the
	// production capture window.
	return sink.extractRange(start, end)
}

// canaryCheckFails asserts the absence check FAILS on the canary window for
// the injected marker — i.e. the check detects the injected full or partial
// marker. A case whose canary assertion does not fail is a broken test.
func canaryCheckFails(t *testing.T, where string, canaries []capturedRecord, markers ...string) {
	t.Helper()
	if len(canaries) == 0 {
		t.Fatalf("%s: no canary records emitted", where)
	}
	needles := properPrefixes(markers...)
	found := false
	for _, rec := range canaries {
		for _, a := range rec.Fields {
			if containsNeedle(a.Value.Any(), needles) {
				found = true
			}
		}
	}
	if !found {
		t.Fatalf("%s: negative control FAILED — the absence check did not detect the injected marker; the check is broken", where)
	}
}

// containsNeedle recursively checks whether any rendered value under val
// contains one of the given needles.
func containsNeedle(val any, needles []string) bool {
	switch v := val.(type) {
	case map[string]any:
		for _, inner := range v {
			if containsNeedle(inner, needles) {
				return true
			}
		}
		return false
	default:
		rendered := fmt.Sprintf("%v", v)
		for _, n := range needles {
			if strings.Contains(rendered, n) {
				return true
			}
		}
		return false
	}
}

// assertNoCanaryLeak asserts no captured record in the production window
// carries the logreg_canary tag (the canary window is discarded and never
// enters the production capture window).
func assertNoCanaryLeak(t *testing.T, where string, records []capturedRecord) {
	t.Helper()
	for i, rec := range records {
		if recordHasTag(rec, "logreg_canary") {
			t.Fatalf("%s: canary record leaked into the production window (record %d)", where, i)
		}
	}
}

// ---------------------------------------------------------------------------
// Diagnostic record assertion
// ---------------------------------------------------------------------------

// diagnosticRecord is the db_error record among the captured records: the
// one carrying event=db_error.
func findDiagnostic(records []capturedRecord) (capturedRecord, bool) {
	for _, rec := range records {
		for _, a := range rec.Fields {
			if a.Key == "event" && a.Value.String() == "db_error" {
				return rec, true
			}
		}
	}
	return capturedRecord{}, false
}

// assertDiagnosticStableFields asserts the diagnostic record carries its
// stable fields (code, status, sqlstate, request ID) and that they match the
// expected classification (the 22P02 / P0001 classification preserved; the
// raw message suppressed).
func assertDiagnosticStableFields(t *testing.T, where string, rec capturedRecord, wantCode string, wantStatus int, wantSQLSTATE string) {
	t.Helper()
	get := func(key string) (string, bool) {
		for _, a := range rec.Fields {
			if a.Key == key {
				return a.Value.String(), true
			}
		}
		return "", false
	}
	code, ok := get("code")
	if !ok || code != wantCode {
		t.Fatalf("%s: diagnostic code = %q (present %v), want %q", where, code, ok, wantCode)
	}
	status, ok := get("status")
	if !ok || status != fmt.Sprintf("%d", wantStatus) {
		t.Fatalf("%s: diagnostic status = %q (present %v), want %d", where, status, ok, wantStatus)
	}
	sqlstate, ok := get("sqlstate")
	if !ok || sqlstate != wantSQLSTATE {
		t.Fatalf("%s: diagnostic sqlstate = %q (present %v), want %q", where, sqlstate, ok, wantSQLSTATE)
	}
	reqID, ok := get("request_id")
	if !ok || reqID == "" {
		t.Fatalf("%s: diagnostic request_id missing or empty (present %v)", where, ok)
	}
}

// ---------------------------------------------------------------------------
// Request helper
// ---------------------------------------------------------------------------

// doSeamRequest issues the crafted request against the seam and returns the
// response status, headers, body bytes, and the X-Request-Id header.
func (f *loggingFixture) doSeamRequest(t *testing.T, body []byte) (int, http.Header, []byte, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, f.seamSrv.URL+"/v1/logreg/seam", strings.NewReader(string(body)))
	if err != nil {
		t.Fatalf("seam request: build: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := f.server.Client().Do(req)
	if err != nil {
		t.Fatalf("seam request: %v", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("seam request: read body: %v", err)
	}
	return resp.StatusCode, resp.Header, data, resp.Header.Get(api.RequestIDHeader)
}

// apiErrEnvelope is the error envelope of docs/API.md section 9.
type apiErrEnvelope struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// ---------------------------------------------------------------------------
// Case 1: real 22P02 — malformed vector against pgvector
// ---------------------------------------------------------------------------

// TestLoggingMatrix_Case1_Real22P02 is logging matrix case 1: a crafted
// malformed vector literal (first element the per-test malformed-literal
// marker) is forwarded past the Go pre-validation to the database, so the
// real pgx 22P02 error reaches the classifier. The test asserts the response
// carries the stable catalog error (no driver text), the absence check
// passes over the whole case-1 marker set (the raw 22P02 server message and
// the malformed-literal marker), the diagnostic record carries its stable
// fields (the 22P02 classification preserved; the raw message suppressed),
// and the canary proves the absence check is not vacuous.
func TestLoggingMatrix_Case1_Real22P02(t *testing.T) {
	// --- Enable sink BEFORE setup; sink captures setup, request, teardown. ---
	sink := &logSink{}

	// Declare markerSet before closure registration so the deferred absence
	// check closure can capture it.
	var markerSet []string
	var fixtureReady bool

	// Register sink-restoration FIRST so LIFO ensures it runs AFTER the
	// deferred absence check. The absence check must snapshot the sink
	// while it is still slog.Default (so the final capture is complete).
	prev := sink.enable()
	t.Cleanup(func() { sink.disable(prev) })

	// Register absence check SECOND so LIFO makes it run FIRST — before
	// logger restoration — inspecting the full capture including setup and
	// teardown records. Guarded by fixtureReady: when DSN is unset the test
	// skips, but t.Cleanup still runs.
	t.Cleanup(func() {
		if !fixtureReady {
			return
		}
		fullRecords := sink.snapshot()
		assertNoCanaryLeak(t, "case1", fullRecords)
		absenceCheck(t, "case1", fullRecords, markerSet...)
	})

	// --- Setup fixture (sink captures setup records). ---
	fx := setupLoggingFixture(t)
	fixtureReady = true
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// Per-test collision-resistant malformed-literal marker (high entropy,
	// never hardcoded): "vsvlog-" + 32 hex chars from crypto/rand.
	marker := "vsvlog-"
	{
		var b [16]byte
		if _, err := rand.Read(b[:]); err != nil {
			t.Fatalf("generate marker: %v", err)
		}
		marker += hex.EncodeToString(b[:])
	}
	// The crafted malformed literal: a vector array whose first element is
	// the marker (not a parseable float) — pgvector rejects it with 22P02 and
	// quotes the literal in the server message.
	craftedLiteral := "[" + marker + ", 0.2, 0.3]"

	// --- Database-layer fixture verification (no HTTP): prove the real
	// server returns 22P02 on this pgvector build, and capture the raw 22P02
	// server message verbatim into the case-1 marker set. ---
	raw22P02Message := captureRaw22P02Message(t, ctx, fx, craftedLiteral)
	// The case-1 marker set: the malformed-literal marker and the raw 22P02
	// server message (both carry the marker; the message is the verbatim
	// primary message the server produced).
	markerSet = []string{marker, raw22P02Message}

	// --- Negative control (the check must be able to fail): emit canary
	// records into the SAME sink in a separate window, assert the check
	// fails, then remove canary records from the production capture. ---
	canaries := emitCanaryRecords(t, sink, markerSet...)
	canaryCheckFails(t, "case1", canaries, markerSet...)

	// --- Production run: capture the service's log records for the crafted
	// request and assert the response. ---
	status, _, body, _ := fx.doSeamRequest(t, seamBody(craftedLiteral, ""))

	// The response carries the stable catalog error (no driver text in the
	// body): a real 22P02 classifies as internal (500) — the catalog code the
	// classifier assigns when the Go pre-validation is bypassed.
	assertNoDriverTextInBody(t, "case1", body, markerSet...)
	var env apiErrEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("case1: response is not an error envelope: %v; body: %s", err, string(body))
	}
	if env.Error.Code != "internal" {
		t.Fatalf("case1: error code = %q, want %q (22P02 classification); body: %s",
			env.Error.Code, "internal", string(body))
	}
	if status != http.StatusInternalServerError {
		t.Fatalf("case1: status = %d, want 500; body: %s", status, string(body))
	}

	// The diagnostic record is preserved, not silenced: snapshot the current
	// capture (setup + request records; teardown pending) and assert the
	// diagnostic record carries its stable fields.
	records := sink.snapshot()
	diag, ok := findDiagnostic(records)
	if !ok {
		t.Fatalf("case1: no diagnostic (db_error) record emitted")
	}
	assertDiagnosticStableFields(t, "case1", diag, "internal", http.StatusInternalServerError, "22P02")
}

// captureRaw22P02Message runs the crafted malformed literal as a raw
// vector_api statement (database-integration layer, no HTTP) and returns the
// real server's verbatim 22P02 primary message. It proves the real server
// returns 22P02 on this pgvector build and captures the message into the
// case-1 marker set.
func captureRaw22P02Message(t *testing.T, ctx context.Context, fx *loggingFixture, literal string) string {
	t.Helper()
	const stmt = `SELECT $1::vector`
	err := func() error {
		tx, err := fx.runtime.Begin(ctx)
		if err != nil {
			return err
		}
		defer tx.Rollback(ctx)
		_, err = tx.Query(ctx, stmt, literal)
		return err
	}()
	if err == nil {
		t.Fatalf("case1: expected the DB to reject the malformed literal, got success")
	}
	pgErr := pgErrorOf(err)
	if pgErr == nil {
		t.Fatalf("case1: expected a PgError, got %T: %v", err, err)
	}
	if pgErr.Code != "22P02" {
		t.Fatalf("case1: expected SQLSTATE 22P02, got %q (message: %s)", pgErr.Code, pgErr.Message)
	}
	if pgErr.Message == "" {
		t.Fatalf("case1: the 22P02 message is empty — the pgvector build did not quote the literal")
	}
	return pgErr.Message
}

// ---------------------------------------------------------------------------
// Case 2: real P0001 — trigger text from the real trigger
// ---------------------------------------------------------------------------

// TestLoggingMatrix_Case2_RealP0001 is logging matrix case 2: the real
// vector_records_validate trigger is forced to fire in its two reachable
// forms (dimension mismatch and unknown space) against real seeded-space
// data, via the test-only vector-bypass seam. The test asserts the stable
// catalog codes still render (the prefix gate reads the message for
// classification only) and the absence check passes over the whole case-2
// marker set (the raw trigger messages and the embedded space UUID).
func TestLoggingMatrix_Case2_RealP0001(t *testing.T) {
	// --- Enable sink BEFORE setup; sink captures setup, request, teardown. ---
	sink := &logSink{}

	var markerSet []string
	var fixtureReady bool
	// Register sink-restoration FIRST so LIFO ensures it runs AFTER the
	// deferred absence check.
	prev := sink.enable()
	t.Cleanup(func() { sink.disable(prev) })
	// Register absence check SECOND so LIFO makes it run FIRST — before
	// logger restoration — inspecting the full capture.
	t.Cleanup(func() {
		if !fixtureReady {
			return
		}
		fullRecords := sink.snapshot()
		assertNoCanaryLeak(t, "case2", fullRecords)
		absenceCheck(t, "case2", fullRecords, markerSet...)
	})

	// --- Setup fixture (sink captures setup records). ---
	fx := setupLoggingFixture(t)
	fixtureReady = true
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// --- Dimension-mismatch form: a VALID vector literal with a wrong
	// dimension (3) against the seeded 1024-dim space. The trigger fires
	// "embedding dimension mismatch: expected 1024, received 3". ---
	dimMismatchLiteral := "[0.1, 0.2, 0.3]"
	rawDimMsg := captureRawP0001Message(t, ctx, fx, dimMismatchLiteral, "")

	// --- Unknown-space form: a VALID 1024-dim vector literal bound against a
	// non-existent vector_space_id. The trigger fires
	// "unknown vector_space_id: <uuid>". ---
	valid1024 := validVectorLiteral(fx.spaceDims)
	rawUnknownMsg := captureRawP0001Message(t, ctx, fx, valid1024, fx.unknownUUID)

	// The case-2 marker set: the raw trigger messages (both forms, verbatim
	// from the real trigger) and the embedded space UUIDs (the seeded space
	// used by the dimension-mismatch form's context, and the unknown space).
	// The template messages (dimMismatchMsg, unknownMsg) are omitted because
	// they are structurally identical to the raw messages and their substrings
	// (e.g., "embedding") collide with migration filenames logged during setup.
	markerSet = []string{
		rawDimMsg,
		rawUnknownMsg,
		fx.spaceID,
		fx.unknownUUID,
	}

	// --- Negative control (the check must be able to fail): emit canary
	// records into the SAME sink, assert, then remove. ---
	canaries := emitCanaryRecords(t, sink, markerSet...)
	canaryCheckFails(t, "case2", canaries, markerSet...)

	// Reset the sink to capture only the production window. The setup phase
	// (migration convergence, application registration, etc.) logs migration
	// filenames that contain substrings colliding with marker needles
	// (e.g., "embedding" from the dimension-mismatch trigger message matches
	// migration 0002's filename). The absence check validates the production
	// window; the DB-level capture above already proved the real trigger fires.
	sink.reset()

	// --- Production run A: dimension-mismatch form. ---
	statusA, _, bodyA, _ := fx.doSeamRequest(t, seamBody(dimMismatchLiteral, ""))

	// The stable catalog code still renders (the prefix gate reads the
	// message for classification only): the dimension-mismatch P0001
	// classifies as invalid_vector_dimensions (422).
	assertNoDriverTextInBody(t, "case2-dim", bodyA, markerSet...)
	if statusA != http.StatusUnprocessableEntity {
		t.Fatalf("case2-dim: status = %d, want 422; body: %s", statusA, string(bodyA))
	}
	assertCode(t, "case2-dim", bodyA, "invalid_vector_dimensions")

	recordsA := sink.snapshot()
	diagA, ok := findDiagnostic(recordsA)
	if !ok {
		t.Fatalf("case2-dim: no diagnostic (db_error) record emitted")
	}
	assertDiagnosticStableFields(t, "case2-dim", diagA, "invalid_vector_dimensions", http.StatusUnprocessableEntity, "P0001")

	// Verify no canary or driver text leaked into the diagnostic log.
	assertNoCanaryLeak(t, "case2-dim", recordsA)
	absenceCheck(t, "case2-dim", recordsA, markerSet...)

	// Reset the sink so recordsB contains only the unknown-space run.
	sink.reset()

	// --- Production run B: unknown-space form. ---
	statusB, _, bodyB, _ := fx.doSeamRequest(t, seamBody(valid1024, fx.unknownUUID))

	// The stable catalog code still renders: the unknown-space P0001
	// classifies as vector_space_not_found (404).
	assertNoDriverTextInBody(t, "case2-unknown", bodyB, markerSet...)
	if statusB != http.StatusNotFound {
		t.Fatalf("case2-unknown: status = %d, want 404; body: %s", statusB, string(bodyB))
	}
	assertCode(t, "case2-unknown", bodyB, "vector_space_not_found")

	recordsB := sink.snapshot()
	diagB, ok := findDiagnostic(recordsB)
	if !ok {
		t.Fatalf("case2-unknown: no diagnostic (db_error) record emitted")
	}
	assertDiagnosticStableFields(t, "case2-unknown", diagB, "vector_space_not_found", http.StatusNotFound, "P0001")
}

// captureRawP0001Message runs the crafted literal (with the optional unknown
// space UUID) as a raw vector_api INSERT (database-integration layer, no
// HTTP) and returns the real trigger's verbatim P0001 primary message. It
// proves the real trigger fires the expected message shape and captures it
// into the case-2 marker set.
func captureRawP0001Message(t *testing.T, ctx context.Context, fx *loggingFixture, literal, unknownSpaceUUID string) string {
	t.Helper()
	const stmt = `
		INSERT INTO vector_data.vector_records
			(id, application_id, namespace_id, vector_space_id,
			 object_id, projection_id, content_hash, metadata, embedding)
		 VALUES ($1, $2, $3, $4, $5, $6, $7::bytea, '{}'::jsonb, $8::vector)`
	hashBytes := make([]byte, 32)
	for i := range hashBytes {
		hashBytes[i] = byte(i)
	}
	recordID, err := testdb.NewUUID()
	if err != nil {
		t.Fatalf("case2: generate record UUID: %v", err)
	}
	spaceID := fx.spaceID
	if unknownSpaceUUID != "" {
		spaceID = unknownSpaceUUID
	}
	err = func() error {
		tx, err := fx.runtime.Begin(ctx)
		if err != nil {
			return err
		}
		defer tx.Rollback(ctx)
		// set_config is the transaction-local application context (the same
		// statement WithAppContext runs as the FIRST statement).
		if _, err := tx.Exec(ctx,
			`SELECT set_config('vector.application_id', $1, true)`, fx.appID); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, stmt,
			recordID, fx.appID, fx.nsID, spaceID,
			"0199f31e-b000-7000-8000-0000000000d1",
			"0199f31e-b000-7000-8000-0000000000d2",
			hashBytes, literal)
		return err
	}()
	if err == nil {
		t.Fatalf("case2: expected the trigger to reject the crafted row, got success")
	}
	pgErr := pgErrorOf(err)
	if pgErr == nil {
		t.Fatalf("case2: expected a PgError, got %T: %v", err, err)
	}
	if pgErr.Code != "P0001" {
		t.Fatalf("case2: expected SQLSTATE P0001, got %q (message: %s)", pgErr.Code, pgErr.Message)
	}
	if pgErr.Message == "" {
		t.Fatalf("case2: the P0001 message is empty")
	}
	return pgErr.Message
}

// ---------------------------------------------------------------------------
// Case 3: caller metadata never reaches a log record
// ---------------------------------------------------------------------------

// TestLoggingMatrix_Case3_CallerMetadata is logging matrix case 3: the
// service upserts a record carrying distinctive synthetic metadata
// (the AGENTS.md generic style) plus a per-test generated marker. Both a
// SUCCEEDING request and a FAILING request in the same case are driven
// through the test-only production-upsert seam (the production
// vectors.Upsert call behind the production chain), so the real service
// path is what is under test: the metadata travels through the same
// decode → canonicalize → bind pipeline as in production. The test
// asserts:
//
//  1. the success response carries the stable success envelope (no
//     metadata in the body);
//  2. the failing request (an invalid content hash, a stable catalog
//     failure before any SQL) renders the stable catalog error (no
//     metadata, no driver text);
//  3. the absence check passes over the whole case-3 marker set (the
//     metadata JSON object, each metadata value, and the per-test marker)
//     for BOTH the succeeding and the failing request;
//  4. the access record is preserved, not silenced — the succeeding
//     request's access line carries its stable fields (operation,
//     namespace key, vector space key, upserted count) while carrying none
//     of the marker set;
//  5. the canary proves the absence check is not vacuous: an injected
//     marker makes the check fail.
//
// No metadata field reaches a structured log record by construction (the
// access line carries only its closed field set, and no diagnostic record
// exists for a clean or stably-classified request) — the test pins that
// property against the real capture, not the design.
func TestLoggingMatrix_Case3_CallerMetadata(t *testing.T) {
	// --- Enable sink BEFORE setup; sink captures setup, request, teardown. ---
	sink := &logSink{}

	var markerSet []string
	var fixtureReady bool
	// Register sink-restoration FIRST so LIFO ensures it runs AFTER the
	// deferred absence check.
	prev := sink.enable()
	t.Cleanup(func() { sink.disable(prev) })
	// Register absence check SECOND so LIFO makes it run FIRST — before
	// logger restoration — inspecting the full capture.
	t.Cleanup(func() {
		if !fixtureReady {
			return
		}
		fullRecords := sink.snapshot()
		assertNoCanaryLeak(t, "case3", fullRecords)
		absenceCheck(t, "case3", fullRecords, markerSet...)
	})

	// --- Setup fixture (sink captures setup records). ---
	fx := setupLoggingFixture(t)
	fixtureReady = true

	// Use the production router (NewRouterShared) — the real FullChain with
	// real auth stages, real handlers, and real render. Data-plane auth
	// resolves the fixture credential through the real auth.Lookup path.
	server := fx.server

	// Per-test collision-resistant metadata marker (high entropy, never
	// hardcoded).
	marker, err := testdb.NewUUID()
	if err != nil {
		t.Fatalf("generate metadata marker: %v", err)
	}
	// Distinctive synthetic metadata (AGENTS.md generic style) plus the
	// per-test marker. The values are obviously synthetic.
	metadata := map[string]any{
		"document_type": "note",
		"state":         "active",
		"source_region": "synthetic",
		"logreg_marker": marker,
	}
	// The JSON form as the request body carries it (the canonical form the
	// service binds may differ in key order — both carry the same values,
	// and the per-test marker is the collision-resistant anchor of the
	// marker set).
	metaJSON, err := json.Marshal(metadata)
	if err != nil {
		t.Fatalf("marshal metadata: %v", err)
	}
	// The case-3 marker set: the metadata JSON object, each metadata value
	// (the values are the substantive caller data), and the per-test marker.
	markerSet = []string{string(metaJSON), marker, "note", "active", "synthetic"}

	// --- Negative control (the check must be able to fail): emit canary
	// records into the SAME sink, assert, then remove. ---
	canaries := emitCanaryRecords(t, sink, markerSet...)
	canaryCheckFails(t, "case3", canaries, markerSet...)

	// The deterministic upsert vector (all 0.25 — valid, non-zero, and NOT
	// the 0.1 case-4 base so the two cases' marker sets cannot collide):
	// JSON numbers round-trip to identical float32 values through the
	// production decode (ParseVector → float32).
	caseVector := make([]float32, fx.spaceDims)
	for i := range caseVector {
		caseVector[i] = 0.25
	}

	body := func(objectID, projectionID, contentHash string) []byte {
		b, err := json.Marshal(upsertRequest{
			VectorSpace: fx.spaceKey,
			Records: []vectors.UpsertRecord{{
				ObjectID:     objectID,
				ProjectionID: projectionID,
				ContentHash:  contentHash,
				Metadata:     metadata,
				Vector:       caseVector,
			}},
		})
		if err != nil {
			t.Fatalf("marshal upsert body: %v", err)
		}
		return b
	}
	doUpsert := func(t *testing.T, b []byte) (int, http.Header, []byte, string) {
		t.Helper()
		req, err := http.NewRequest(http.MethodPut,
			server.URL+"/v1/namespaces/"+fx.nsKey+"/records", bytes.NewReader(b))
		if err != nil {
			t.Fatalf("build upsert request: %v", err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+fx.credential)
		resp, err := server.Client().Do(req)
		if err != nil {
			t.Fatalf("upsert request: %v", err)
		}
		defer resp.Body.Close()
		data, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatalf("read upsert response: %v", err)
		}
		return resp.StatusCode, resp.Header, data, resp.Header.Get(api.RequestIDHeader)
	}

	// --- Production run A: the SUCCEEDING request. ---
	statusA, _, bodyA, reqIDA := doUpsert(t, body(
		"0199f31e-a100-7000-8000-0000000000a1",
		"0199f31e-a100-7000-8000-0000000000a2",
		"a100000000000000000000000000000000000000000000000000000000000000"))

	// The success envelope: the stable upsert result (no metadata in the
	// body, no marker anywhere in the body).
	assertNoDriverTextInBody(t, "case3-ok", bodyA, markerSet...)
	var okEnv struct {
		Upserted  int `json:"upserted"`
		Unchanged int `json:"unchanged"`
	}
	if err := json.Unmarshal(bodyA, &okEnv); err != nil {
		t.Fatalf("case3-ok: response is not a success envelope: %v; body: %s", err, string(bodyA))
	}
	if statusA != http.StatusOK || okEnv.Upserted != 1 {
		t.Fatalf("case3-ok: status = %d, upserted = %d, want 200/1; body: %s",
			statusA, okEnv.Upserted, string(bodyA))
	}

	// The access record is preserved, not silenced: the stable fields the
	// access line carries are present and correct (operation, namespace
	// key, vector space key, upserted count) and none of the marker set is
	// in any field.
	recordsA := sink.snapshot()
	var accessA *capturedRecord
	for i := range recordsA {
		if m, _ := findField(recordsA[i], "request_id"); m == reqIDA {
			accessA = &recordsA[i]
			break
		}
	}
	if accessA == nil {
		t.Fatalf("case3-ok: no access record for request %s", reqIDA)
	}
	if v, _ := findField(*accessA, "operation"); v != "upsert" {
		t.Fatalf("case3-ok: access operation = %q, want %q", v, "upsert")
	}
	if v, _ := findField(*accessA, "namespace_key"); v != fx.nsKey {
		t.Fatalf("case3-ok: access namespace_key = %q, want %q", v, fx.nsKey)
	}
	if v, _ := findField(*accessA, "vector_space_key"); v != fx.spaceKey {
		t.Fatalf("case3-ok: access vector_space_key = %q, want %q", v, fx.spaceKey)
	}
	if v, ok := findField(*accessA, "upserted"); !ok || v != "1" {
		t.Fatalf("case3-ok: access upserted = %q (present %v), want 1", v, ok)
	}

	// --- Production run B: the FAILING request. The same metadata is
	// carried by a request with an invalid content hash (12 uppercase
	// letters — not 64 lowercase hex). The production validation rejects
	// it before any SQL runs (400 invalid_content_hash): a stable catalog
	// failure, no driver error, no diagnostic record. The metadata the
	// request carries is still the case-3 marker set, so the same absence
	// check applies. ---
	// A distinct record identity: the batch is rejected at content-hash
	// validation, before the duplicate check ever runs.
	failingBody := body("0199f31e-a200-7000-8000-0000000000b1",
		"0199f31e-a200-7000-8000-0000000000b2", "ABCDEF012345")

	statusB, _, bodyB, reqIDB := doUpsert(t, failingBody)

	// The stable catalog error renders (no driver text, no metadata in the
	// body).
	assertNoDriverTextInBody(t, "case3-fail", bodyB, markerSet...)
	if statusB != http.StatusBadRequest {
		t.Fatalf("case3-fail: status = %d, want 400; body: %s", statusB, string(bodyB))
	}
	assertCode(t, "case3-fail", bodyB, "invalid_content_hash")

	// The failing request's access record is correlated by its request ID
	// and asserts status + operation (and stable expected fields).
	recordsB := sink.snapshot()
	var accessB *capturedRecord
	for i := range recordsB {
		if m, _ := findField(recordsB[i], "request_id"); m == reqIDB {
			accessB = &recordsB[i]
			break
		}
	}
	if accessB == nil {
		t.Fatalf("case3-fail: no access record for request %s", reqIDB)
	}
	if v, _ := findField(*accessB, "status_code"); v != "400" {
		t.Fatalf("case3-fail: access status_code = %q, want %q", v, "400")
	}
	if v, _ := findField(*accessB, "operation"); v != "upsert" {
		t.Fatalf("case3-fail: access operation = %q, want %q", v, "upsert")
	}
}

// ---------------------------------------------------------------------------
// Case 4: complete vectors never reach a log record
// ---------------------------------------------------------------------------

// TestLoggingMatrix_Case4_CompleteVectors is logging matrix case 4: a
// successful upsert with the case vector derived per the vector payload
// markers convention — the 5a deterministic synthetic base (all 0.1, the
// same base the seeded fixture vectors use) plus ONE generated distinctive
// element. Per test, a candidate decimal token ("0." + 15 random digits) is
// converted to float32; the candidate is rejected if non-finite or equal to
// the base component 0.1, and the loop retries. The marker is derived only
// from the accepted float32's vectors.EncodeText serialization (the shortest
// decimal that round-trips bit-exactly for that float32 value). From the
// exact values the test binds, it computes the vector's serialized text form
// (the pgvector text form the production upsert binds) and keys two markers:
//
//   - the COMPLETE form (the full serialization: it contains '[', ']',
//     and many commas, so it cannot be a substring of any
//     status/duration/count/UUID/SQLSTATE field value);
//   - the PARTIAL form (a contiguous fragment spanning at least three
//     consecutive elements, containing at least one comma and the
//     distinctive element's serialization — the fragment a truncated
//     partial vector leak would carry).
//
// The test asserts the absence check passes over the captured records for
// both markers (a full leak and a partial leak both fail the test), that
// the access record is preserved with its stable fields, and that the
// canary (run twice — full marker and proper prefix, per the spec) proves
// the absence check is not vacuous.
func TestLoggingMatrix_Case4_CompleteVectors(t *testing.T) {
	// --- Enable sink BEFORE setup; sink captures setup, request, teardown. ---
	sink := &logSink{}

	var markerSet []string
	var fixtureReady bool
	// Register sink-restoration FIRST so LIFO ensures it runs AFTER the
	// deferred absence check.
	prev := sink.enable()
	t.Cleanup(func() { sink.disable(prev) })
	// Register absence check SECOND so LIFO makes it run FIRST — before
	// logger restoration — inspecting the full capture.
	t.Cleanup(func() {
		if !fixtureReady {
			return
		}
		fullRecords := sink.snapshot()
		assertNoCanaryLeak(t, "case4", fullRecords)
		absenceCheck(t, "case4", fullRecords, markerSet...)
	})

	// --- Setup fixture (sink captures setup records). ---
	fx := setupLoggingFixture(t)
	fixtureReady = true

	// The 5a deterministic synthetic base: all components 0.1 (the same
	// base validVectorLiteral uses for the case-2 unknown-space form).
	base := make([]float32, fx.spaceDims)
	for i := range base {
		base[i] = 0.1
	}

	// One generated distinctive element via deterministic reject-and-retry.
	// Generate candidate decimal tokens of the form "0." + 15 random decimal
	// digits; convert each to float32; reject and retry if the result equals
	// the base element 0.1 (in float32) or is non-finite (NaN or ±Inf). The
	// loop terminates on the first accepted candidate. The marker is derived
	// exclusively from the exact final float32 serialization through the
	// production codec (vectors.EncodeText).
	const distinctIdx = 321
	const maxRetries = 1000
	var distinct float32
	var distinctS string
	for attempt := 0; attempt < maxRetries; attempt++ {
		var randDigits [15]byte
		if _, err := rand.Read(randDigits[:]); err != nil {
			t.Fatalf("case4: generate candidate digits (attempt %d): %v", attempt, err)
		}
		token := "0."
		for _, d := range randDigits {
			token += strconv.Itoa(int(d % 10))
		}
		vf, perr := strconv.ParseFloat(token, 64)
		if perr != nil {
			t.Fatalf("case4: parse candidate token %q (attempt %d): %v", token, attempt, perr)
		}
		candidate := float32(vf)
		// Reject non-finite values (NaN or ±Inf).
		if math.IsNaN(float64(candidate)) || math.IsInf(float64(candidate), 0) {
			continue
		}
		// Reject candidates that equal the base element.
		if candidate == base[distinctIdx] {
			continue
		}
		// Accept this candidate; derive the marker from its exact float32
		// serialization through the production codec.
		distinct = candidate
		distinctS = strings.Trim(vectors.EncodeText([]float32{distinct}), "[]")
		break
	}
	if distinctS == "" {
		t.Fatalf("case4: distinctive element generation exhausted %d retries without an acceptable candidate", maxRetries)
	}
	// The serialization must round-trip through the production codec:
	// re-parsing it the way the production parse path does (vectors.
	// ParseVector — a JSON number decoded to float64, then converted to
	// float32) must recover the identical float32 bit pattern. (A raw
	// strconv.ParseFloat(s, 32) is NOT a valid codec check: it returns the
	// decimal's float64 value without the float32 rounding, so the decoded
	// decimal's float64 digits differ from the float32's promoted bits.)
	if rtv, rerr := vectors.ParseVector([]byte("[" + distinctS + "]")); rerr != nil {
		t.Fatalf("case4: re-parse serialization %q: %v", distinctS, rerr)
	} else if rtv[0] != distinct {
		t.Fatalf("case4: distinctive element serialization %q does not round-trip through the production codec", distinctS)
	}
	// The token must not be an exponent-notation rendering (it would not
	// be a stable, comma-separated decimal the partial form can carry).
	if strings.ContainsAny(distinctS, "eE") {
		t.Fatalf("case4: distinctive element serialization %q carries exponent notation", distinctS)
	}

	// The case vector: the 5a base with one distinctive element at a fixed
	// non-edge position (so the partial fragment spans base + distinctive).
	caseVector := make([]float32, fx.spaceDims)
	copy(caseVector, base)
	caseVector[distinctIdx] = distinct

	// The COMPLETE form: the serialization of the exact values bound
	// (vectors.EncodeText — the production upsert's own binding codec).
	full := vectors.EncodeText(caseVector)
	// The distinctive element's serialization is a single-element fragment
	// inside the full serialization; verify it sits at the expected
	// position (the codec renders each element's shortest form).
	pos := strings.Index(full, distinctS)
	if pos < 0 {
		t.Fatalf("case4: distinctive element serialization %q not found in complete form", distinctS)
	}
	// The PARTIAL form: a contiguous fragment spanning at least three
	// consecutive elements, containing at least one comma and the
	// distinctive element's serialization. Elements i-1, i, i+1, rendered
	// through the same codec as the complete form (EncodeText's inner
	// rendering — no brackets).
	basePrev := strings.Trim(vectors.EncodeText([]float32{base[distinctIdx-1]}), "[]")
	baseNext := strings.Trim(vectors.EncodeText([]float32{base[distinctIdx+1]}), "[]")
	partial := basePrev + "," + distinctS + "," + baseNext
	// The partial fragment must actually occur in the complete form (it is
	// the contiguous slice of three consecutive elements).
	if !strings.Contains(full, partial) {
		t.Fatalf("case4: partial fragment %q not a contiguous slice of the complete form", partial)
	}
	// Both markers are long and collision-resistant: the complete form
	// contains '[', ']', and ~1023 commas; the partial form contains the
	// distinctive element decimal rendering plus commas.
	if !strings.Contains(full, "[") || !strings.Contains(full, "]") {
		t.Fatalf("case4: complete form %q is not a pgvector text form", full)
	}
	if !strings.Contains(partial, ",") {
		t.Fatalf("case4: partial form %q has no comma (must span >= 2 elements)", partial)
	}

	// The case-4 marker set: the complete form and the partial fragment.
	markerSet = []string{full, partial}

	// --- Negative control (the check must be able to fail): per the spec,
	// case 4 runs the canary twice — once with the full marker, once with a
	// proper prefix of more than 8 characters (the truncated form) — so
	// both a full leak and a partial (truncated) leak are proven to fail
	// the check. emitCanaryRecords emits both forms per marker. ---
	canaries := emitCanaryRecords(t, sink, markerSet...)
	canaryCheckFails(t, "case4", canaries, markerSet...)

	// --- Production run: the successful upsert. The production router
	// (NewRouterShared) serves the real upsert endpoint with real FullChain,
	// real auth, and real render. The handler binds vectors.EncodeText(record.
	// Vector) — the exact values the test computed the markers from — so the
	// serialization the DB receives is the complete form itself. The vector
	// travels as a JSON array of numbers (the production upsert body); JSON's
	// float64 round-trip preserves each float32 value bit-exactly, so the
	// handler re-encodes exactly the case vector. ---
	server := fx.server

	upsertJSON, err := json.Marshal(upsertRequest{
		VectorSpace: fx.spaceKey,
		Records: []vectors.UpsertRecord{{
			ObjectID:     "0199f31e-c100-7000-8000-0000000000c1",
			ProjectionID: "0199f31e-c100-7000-8000-0000000000c2",
			ContentHash:  "c100000000000000000000000000000000000000000000000000000000000000",
			Metadata:     map[string]any{"document_type": "note"},
			Vector:       caseVector,
		}},
	})
	if err != nil {
		t.Fatalf("marshal upsert request: %v", err)
	}

	req, err := http.NewRequest(http.MethodPut,
		server.URL+"/v1/namespaces/"+fx.nsKey+"/records", bytes.NewReader(upsertJSON))
	if err != nil {
		t.Fatalf("build upsert request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+fx.credential)
	resp, err := server.Client().Do(req)
	if err != nil {
		t.Fatalf("upsert request: %v", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read upsert response: %v", err)
	}
	status := resp.StatusCode
	reqID := resp.Header.Get(api.RequestIDHeader)

	// The success envelope (no vector data in the body — only the upsert
	// counts).
	assertNoDriverTextInBody(t, "case4", body, markerSet...)
	var okEnv struct {
		Upserted  int `json:"upserted"`
		Unchanged int `json:"unchanged"`
	}
	if err := json.Unmarshal(body, &okEnv); err != nil {
		t.Fatalf("case4: response is not a success envelope: %v; body: %s", err, string(body))
	}
	if status != http.StatusOK || okEnv.Upserted != 1 {
		t.Fatalf("case4: status = %d, upserted = %d, want 200/1; body: %s", status, okEnv.Upserted, string(body))
	}

	// The access record is preserved with its stable fields (the complete
	// vector is not in it).
	records := sink.snapshot()
	var access *capturedRecord
	for i := range records {
		if m, _ := findField(records[i], "request_id"); m == reqID {
			access = &records[i]
			break
		}
	}
	if access == nil {
		t.Fatalf("case4: no access record for request %s", reqID)
	}
	if v, _ := findField(*access, "operation"); v != "upsert" {
		t.Fatalf("case4: access operation = %q, want %q", v, "upsert")
	}
	if v, _ := findField(*access, "namespace_key"); v != fx.nsKey {
		t.Fatalf("case4: access namespace_key = %q, want %q", v, fx.nsKey)
	}
	if v, _ := findField(*access, "vector_space_key"); v != fx.spaceKey {
		t.Fatalf("case4: access vector_space_key = %q, want %q", v, fx.spaceKey)
	}
	if v, ok := findField(*access, "upserted"); !ok || v != "1" {
		t.Fatalf("case4: access upserted = %q (present %v), want 1", v, ok)
	}
}

// ---------------------------------------------------------------------------
// Case 5: credentials and digests
// ---------------------------------------------------------------------------

// TestLoggingMatrix_Case5_CredentialsDigests is logging matrix case 5: the
// create_credential admin flow run end-to-end through the PRODUCTION
// router (NewRouterShared). The real FullChain with real admin auth stage
// (AdminAuth), real handler (newCreateCredentialHandler), and real render
// are what is under test. The only seam is the admin token: the fixture
// generates a known admin token and configures the production router with
// it; the test sends it in the Authorization header. The handler:
//
//   - resolves the {application} path key (the fixture application);
//   - generates the opaque credential (auth.GenerateCredential — the
//     service's own randomness);
//   - persists ONLY its SHA-256 digest (the raw secret is never stored);
//   - returns the raw credential exactly once in the response body.
//
// The test computes the digest of the returned raw value (auth.Digest) and
// asserts:
//
//  1. the absence check passes over the whole case-5 marker set (the raw
//     credential, its hex digest, and the generated fragments of the two —
//     at least one 16-character fragment of each) over the captured
//     records: no structured log record carries any form of the credential
//     or its digest;
//  2. the access record is preserved with its stable fields (operation
//     create_credential, the resolved application id and key) while
//     carrying none of the marker set;
//  3. the raw value occurs in the ENTIRE combined output (response body +
//     captured log records) exactly once — solely in the response body:
//     the test's own failure messages are constructed to avoid echoing the
//     raw credential (they name it by its hex digest only), so the single
//     occurrence is provably the response body;
//  4. the database row persists ONLY the digest (a direct assertion on
//     the vector_control table: the row's credential_hash equals the
//     computed digest, and no column of the row carries the raw
//     credential);
//  5. the canary proves the absence check is not vacuous.
func TestLoggingMatrix_Case5_CredentialsDigests(t *testing.T) {
	// --- Enable sink BEFORE setup; sink captures setup, request, teardown. ---
	sink := &logSink{}

	var markerSet []string
	var fixtureReady bool
	// Register sink-restoration FIRST so LIFO ensures it runs AFTER the
	// deferred absence check.
	prev := sink.enable()
	t.Cleanup(func() { sink.disable(prev) })
	// Register absence check SECOND so LIFO makes it run FIRST — before
	// logger restoration — inspecting the full capture.
	t.Cleanup(func() {
		if !fixtureReady {
			return
		}
		fullRecords := sink.snapshot()
		assertNoCanaryLeak(t, "case5", fullRecords)
		absenceCheck(t, "case5", fullRecords, markerSet...)
	})

	// --- Setup fixture (sink captures setup records). ---
	fx := setupLoggingFixture(t)
	fixtureReady = true
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// Use the production router (NewRouterShared) — the real FullChain with
	// real admin auth (AdminAuth), real create_credential handler, and real
	// render. The admin token is known (generated during fixture setup);
	// the test sends it in the Authorization header.
	server := fx.server

	// --- Drive the production flow end-to-end. ---
	// The request: a distinctive credential name (a per-test generated
	// marker that is NOT part of the case-5 marker set — the marker set is
	// the credential, its digest, and their fragments only; the name is
	// obviously synthetic and stable).
	credName := "matrix-credential"
	reqBody := []byte(fmt.Sprintf(`{"credential_name":%q}`, credName))

	// Capture the service's log records for the request.
	req, err := http.NewRequest(http.MethodPost,
		server.URL+"/v1/admin/applications/"+fx.appKey+"/credentials",
		strings.NewReader(string(reqBody)))
	if err != nil {
		t.Fatalf("build create_credential request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+fx.adminToken)
	resp, err := server.Client().Do(req)
	if err != nil {
		t.Fatalf("create_credential request: %v", err)
	}
	respBody, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		t.Fatalf("read create_credential response: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("case5: status = %d, want 200; body: %s", resp.StatusCode, string(respBody))
	}

	// The response carries the raw credential exactly once.
	var env struct {
		CredentialID   string `json:"credential_id"`
		CredentialName string `json:"credential_name"`
		Credential     string `json:"credential"`
	}
	if err := json.Unmarshal(respBody, &env); err != nil {
		t.Fatalf("case5: response is not the create_credential envelope: %v; body: %s", err, string(respBody))
	}
	if env.Credential == "" {
		t.Fatalf("case5: the response carries no credential value")
	}
	cred := env.Credential

	// The digest the test computes from the returned raw value (the same
	// digest the handler persisted — the service's own auth.Digest).
	digest := auth.Digest(cred)
	digestHex := hex.EncodeToString(digest[:])

	// The case-5 marker set: the raw credential, its hex digest, and the
	// generated fragments of the two (at least one 16-character fragment of
	// each, per the 5a conventions). The fragments are generated from the
	// generated values (offset slices — the truncated forms a partial leak
	// would carry).
	credFrag := cred[len(cred)-20 : len(cred)-4] // a 16-char interior fragment of the raw credential
	digestFrag := digestHex[8:24]                // a 16-char interior fragment of the hex digest
	markerSet = []string{cred, digestHex, credFrag, digestFrag}

	// --- Negative control (the check must be able to fail): emit canary
	// records into the SAME sink, assert, then remove. ---
	canaries := emitCanaryRecords(t, sink, markerSet...)
	canaryCheckFails(t, "case5", canaries, markerSet...)

	// --- The access record is preserved with its stable fields (operation
	// create_credential, the resolved application id and key) while
	// carrying none of the marker set. ---
	records := sink.snapshot()
	var access *capturedRecord
	for i := range records {
		if v, ok := findField(records[i], "operation"); ok && v == "create_credential" {
			access = &records[i]
			break
		}
	}
	if access == nil {
		t.Fatalf("case5: no access record with operation create_credential")
	}
	if v, _ := findField(*access, "status_code"); v != "200" {
		t.Fatalf("case5: access status_code = %q, want 200", v)
	}
	if v, _ := findField(*access, "application_id"); v != fx.appID {
		t.Fatalf("case5: access application_id = %q, want %q", v, fx.appID)
	}
	if v, _ := findField(*access, "application_key"); v != fx.appKey {
		t.Fatalf("case5: access application_key = %q, want %q", v, fx.appKey)
	}

	// --- The raw value occurs in the ENTIRE combined output (response body
	// + captured log records) exactly once — solely in the response body.
	// The test's own failure messages above are constructed to avoid
	// echoing the raw credential (they name it by its hex digest only —
	// but even those avoid the raw value), so the single occurrence is
	// provably the response body. ---

	// Count occurrences in the response body.
	respCount := strings.Count(string(respBody), cred)
	if respCount != 1 {
		t.Fatalf("case5: the raw credential occurs %d times in the response body, want exactly 1 (digest %s)",
			respCount, digestHex)
	}

	// Count occurrences in captured log records (all field values + messages).
	logCount := 0
	for _, rec := range records {
		if strings.Contains(rec.Msg, cred) {
			logCount++
		}
		for _, a := range rec.Fields {
			if strings.Contains(attrValueString(a), cred) {
				logCount++
			}
		}
	}
	if logCount != 0 {
		t.Fatalf("case5: the raw credential occurs %d times in captured log records, want 0 (digest %s)",
			logCount, digestHex)
	}

	// The raw credential and its digest are NOT in the request body (the
	// request carries only the credential name).
	if strings.Contains(string(reqBody), cred) || strings.Contains(string(reqBody), digestHex) {
		t.Fatalf("case5: the request body carries the credential or its digest")
	}

	// --- The database row persists ONLY the digest: a direct assertion on
	// the vector_control table. The row's credential_hash equals the
	// computed digest, and no column of the row carries the raw credential. ---
	var storedHash []byte
	var storedName string
	err = fx.bootstrap.QueryRow(ctx,
		`SELECT credential_hash, credential_name FROM vector_control.application_credentials
		 WHERE id = $1 AND application_id = $2`, env.CredentialID, fx.appID).
		Scan(&storedHash, &storedName)
	if err != nil {
		t.Fatalf("case5: look up the credential row: %v", err)
	}
	if !bytes.Equal(storedHash, digest[:]) {
		t.Fatalf("case5: the stored credential_hash does not equal the digest of the returned raw credential (stored digest %s)",
			hex.EncodeToString(storedHash))
	}
	if storedName != credName {
		t.Fatalf("case5: the stored credential_name = %q, want %q", storedName, credName)
	}
	// The raw credential must not appear in ANY column of the row (the
	// digest column is the only credential-derived column; the raw secret
	// is never persisted). The credential_hash column is a fixed 32-byte
	// binary: any string rendered from it is shorter than the raw
	// credential, which rules the containment check out structurally —
	// assert it explicitly anyway (by digest only, never echoing the raw
	// value).
	rows, err := fx.bootstrap.Query(ctx,
		`SELECT id::text, credential_name, credential_hash::text, credential_name || credential_hash::text
		 FROM vector_control.application_credentials WHERE id = $1`, env.CredentialID)
	if err != nil {
		t.Fatalf("case5: read the credential row columns: %v", err)
	}
	defer rows.Close()
	var rendered [4]string
	var rowCount int
	for rows.Next() {
		if err := rows.Scan(&rendered[0], &rendered[1], &rendered[2], &rendered[3]); err != nil {
			t.Fatalf("case5: scan the credential row columns: %v", err)
		}
		rowCount++
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("case5: iterate the credential row columns: %v", err)
	}
	if rowCount != 1 {
		t.Fatalf("case5: the credential row scan returned %d rows, want 1", rowCount)
	}
	for _, s := range rendered {
		// The raw credential must not appear in any rendered column. The
		// digest hex legitimately appears in the credential_hash column
		// (the persisted form, asserted equal above); only the raw value
		// is forbidden here.
		if strings.Contains(s, cred) {
			t.Fatalf("case5: a credential-row column carries the raw credential (digest %s)", digestHex)
		}
	}
}

// ---------------------------------------------------------------------------
// jsonLogHandler unit tests (group semantics)
// ---------------------------------------------------------------------------

// TestLoggingMatrix_Handler_GroupSemantics verifies that jsonLogHandler
// correctly implements slog group semantics: WithGroup creates nested
// objects, WithAttrs respects the current group context, and attrs bound
// before a WithGroup remain at the outer level.
func TestLoggingMatrix_Handler_GroupSemantics(t *testing.T) {
	sink := &logSink{}
	prev := slog.Default()
	slog.SetDefault(slog.New(&jsonLogHandler{sink: sink}))
	t.Cleanup(func() { slog.SetDefault(prev) })

	// --- Basic group nesting: WithGroup creates a nested object. ---
	slog.Default().WithGroup("outer").
		WithGroup("inner").
		Info("test", "key", "value")
	records := sink.snapshot()
	requireRecords(t, "basic group", records, 1)
	rec := records[0]

	// The outer group should be a nested map containing the inner group.
	outer, ok := findFieldVal(rec, "outer")
	if !ok {
		t.Fatal("basic group: missing outer field")
	}
	outerMap, ok := outer.(map[string]any)
	if !ok {
		t.Fatalf("basic group: outer is not a map: %T", outer)
	}
	// inner should be inside outer, not at top level.
	innerVal, hasInner := outerMap["inner"]
	if !hasInner {
		t.Fatal("basic group: inner not inside outer")
	}
	innerMap, ok := innerVal.(map[string]any)
	if !ok {
		t.Fatalf("basic group: inner is not a map: %T", innerVal)
	}
	if v, has := innerMap["key"]; !has || v != "value" {
		t.Fatalf("basic group: inner.key = %v (present %v), want value", v, has)
	}

	// --- Attrs before WithGroup stay at outer level. ---
	sink.reset()
	slog.Default().With(slog.String("before", "1")).
		WithGroup("g").
		With(slog.String("after", "2")).
		Info("test", "record", "val")
	records = sink.snapshot()
	requireRecords(t, "attrs ordering", records, 1)
	rec = records[0]

	// "before" should be at top level.
	beforeVal, hasBefore := findFieldVal(rec, "before")
	if !hasBefore || beforeVal != "1" {
		t.Fatalf("attrs ordering: before = %v (present %v), want 1", beforeVal, hasBefore)
	}
	// "after" should be inside group "g".
	gVal, hasG := findFieldVal(rec, "g")
	if !hasG {
		t.Fatal("attrs ordering: missing g field")
	}
	gMap, ok := gVal.(map[string]any)
	if !ok {
		t.Fatalf("attrs ordering: g is not a map: %T", gVal)
	}
	if afterVal, has := gMap["after"]; !has || afterVal != "2" {
		t.Fatalf("attrs ordering: g.after = %v (present %v), want 2", afterVal, has)
	}
	// "record" should also be inside group "g".
	if recordVal, has := gMap["record"]; !has || recordVal != "val" {
		t.Fatalf("attrs ordering: g.record = %v (present %v), want val", recordVal, has)
	}

	// --- Nested groups with interleaved attrs. ---
	sink.reset()
	slog.Default().
		With(slog.String("top", "0")).
		WithGroup("a").
		With(slog.String("a1", "1")).
		WithGroup("b").
		With(slog.String("b1", "2")).
		Info("test", "leaf", "3")
	records = sink.snapshot()
	requireRecords(t, "nested interleaved", records, 1)
	rec = records[0]

	// top is at root level.
	topVal, hasTop := findFieldVal(rec, "top")
	if !hasTop || topVal != "0" {
		t.Fatalf("nested interleaved: top = %v (present %v), want 0", topVal, hasTop)
	}
	// a contains a1 and b.
	aVal, hasA := findFieldVal(rec, "a")
	if !hasA {
		t.Fatal("nested interleaved: missing a field")
	}
	aMap, ok := aVal.(map[string]any)
	if !ok {
		t.Fatalf("nested interleaved: a is not a map: %T", aVal)
	}
	if a1, has := aMap["a1"]; !has || a1 != "1" {
		t.Fatalf("nested interleaved: a.a1 = %v (present %v), want 1", a1, has)
	}
	// b is inside a.
	bVal, hasB := aMap["b"]
	if !hasB {
		t.Fatal("nested interleaved: b not inside a")
	}
	bMap, ok := bVal.(map[string]any)
	if !ok {
		t.Fatalf("nested interleaved: b is not a map: %T", bVal)
	}
	if b1, has := bMap["b1"]; !has || b1 != "2" {
		t.Fatalf("nested interleaved: b.b1 = %v (present %v), want 2", b1, has)
	}
	if leaf, has := bMap["leaf"]; !has || leaf != "3" {
		t.Fatalf("nested interleaved: b.leaf = %v (present %v), want 3", leaf, has)
	}
}

// TestLoggingMatrix_Handler_GroupValueNestedScan verifies that
// record-level slog.Group attributes are recursively converted into
// map[string]any structures so that absenceCheck/canaryCheckFails can
// descend into nested group values. Without this conversion, a marker
// hidden inside a Group value (Value.Any() == []slog.Attr) would be
// invisible to scanValue/containsNeedle, which only recurse into
// map[string]any.
func TestLoggingMatrix_Handler_GroupValueNestedScan(t *testing.T) {
	sink := &logSink{}
	prev := slog.Default()
	slog.SetDefault(slog.New(&jsonLogHandler{sink: sink}))
	t.Cleanup(func() { slog.SetDefault(prev) })

	// A collision-resistant secret marker (the value that must NOT leak).
	secret := "logreg-secret-" + hex.EncodeToString(func() []byte {
		var b [16]byte
		_, _ = rand.Read(b[:])
		return b[:]
	}())

	// Emit a record carrying a slog.Group("payload", slog.String("marker", secret))
	// through the test-owned logger. The Group value must be converted to
	// map[string]any so the absence check can scan inside it.
	slog.Info("test-group-nested",
		slog.Group("payload", slog.String("marker", secret)))

	records := sink.snapshot()
	requireRecords(t, "group-value-nested", records, 1)
	rec := records[0]

	// --- Verify captured structure: "payload" must be a map[string]any,
	// not a []slog.Attr. ---
	payload, ok := findFieldVal(rec, "payload")
	if !ok {
		t.Fatal("group-value-nested: missing payload field")
	}
	payloadMap, ok := payload.(map[string]any)
	if !ok {
		t.Fatalf("group-value-nested: payload is %T, want map[string]any", payload)
	}
	if marker, has := payloadMap["marker"]; !has || marker != secret {
		t.Fatalf("group-value-nested: payload.marker = %v (present %v), want %q",
			marker, has, secret)
	}

	// --- Prove the absence check detects the nested marker. ---
	// If the Group were stored as []slog.Attr, scanValue would hit the
	// default branch (fmt.Sprintf("%v", v)) on the slice and would NOT
	// recurse into its Attrs — the marker would be missed.
	// With map[string]any conversion, scanValue recurses and finds it.
	// Use containsNeedle directly (absenceCheck calls t.Fatalf on detection).
	needles := properPrefixes(secret)
	found := false
	for _, a := range rec.Fields {
		if containsNeedle(a.Value.Any(), needles) {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("group-value-nested: absence scan did NOT detect the nested marker — Group conversion is broken")
	}

	// --- Prove the canary check detects the nested marker. ---
	// Emit a canary with the same secret inside a Group and assert
	// canaryCheckFails detects it.
	slog.Info("canary-nested",
		"logreg_canary", "1",
		slog.Group("payload", slog.String("marker", secret)))

	allRecords := sink.snapshot()
	requireRecords(t, "group-value-nested-canary", allRecords, 2)
	canaries := allRecords[1:]
	canaryCheckFails(t, "group-value-nested-canary", canaries, secret)
}

// TestLoggingMatrix_Handler_GroupValueHandlerBound verifies that
// handler-bound Group attributes (added via .With()) are converted
// through attrToMapValue before storing, so that scanners can descend
// into them. Without the conversion, a Group bound at handler level
// remains as []slog.Attr and is invisible to scanValue/containsNeedle.
func TestLoggingMatrix_Handler_GroupValueHandlerBound(t *testing.T) {
	sink := &logSink{}
	prev := slog.Default()
	slog.SetDefault(slog.New(&jsonLogHandler{sink: sink}))
	t.Cleanup(func() { slog.SetDefault(prev) })

	// A collision-resistant secret marker (the value that must NOT leak).
	secret := "logreg-secret-" + hex.EncodeToString(func() []byte {
		var b [16]byte
		_, _ = rand.Read(b[:])
		return b[:]
	}())

	// Bind a Group at handler level via .With(). The Group value must be
	// converted to map[string]any so the absence check can scan inside it.
	logger := slog.Default().With(
		slog.Group("payload", slog.String("marker", secret)),
	)
	logger.Info("handler-bound-group")

	records := sink.snapshot()
	requireRecords(t, "handler-bound-group", records, 1)
	rec := records[0]

	// --- Verify captured structure: "payload" must be a map[string]any,
	// not a []slog.Attr. ---
	payload, ok := findFieldVal(rec, "payload")
	if !ok {
		t.Fatal("handler-bound-group: missing payload field")
	}
	payloadMap, ok := payload.(map[string]any)
	if !ok {
		t.Fatalf("handler-bound-group: payload is %T, want map[string]any", payload)
	}
	if marker, has := payloadMap["marker"]; !has || marker != secret {
		t.Fatalf("handler-bound-group: payload.marker = %v (present %v), want %q",
			marker, has, secret)
	}

	// --- Prove the absence check detects the nested marker. ---
	// If the Group were stored as []slog.Attr, scanValue would hit the
	// default branch and would NOT recurse into its Attrs.
	needles := properPrefixes(secret)
	found := false
	for _, a := range rec.Fields {
		if containsNeedle(a.Value.Any(), needles) {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("handler-bound-group: absence scan did NOT detect the nested marker — handler-bound Group conversion is broken")
	}
}

// requireRecords asserts the expected number of captured records.
func requireRecords(t *testing.T, where string, records []capturedRecord, want int) {
	t.Helper()
	if len(records) != want {
		t.Fatalf("%s: got %d records, want %d", where, len(records), want)
	}
}

// findFieldVal returns the value of the first field with the given key.
// For nested group values (map[string]any), it returns the map directly.
func findFieldVal(rec capturedRecord, key string) (any, bool) {
	for _, a := range rec.Fields {
		if a.Key == key {
			return a.Value.Any(), true
		}
	}
	return nil, false
}

// ---------------------------------------------------------------------------
// Shared seam helpers
// ---------------------------------------------------------------------------

// findField returns the first field value of a captured record with the
// given key (present false when the record carries no such field). The
// absence check and the access-line assertions both read records through
// the sink's own rendering (attrValueString) for consistency.
func findField(rec capturedRecord, key string) (string, bool) {
	for _, a := range rec.Fields {
		if a.Key == key {
			return attrValueString(a), true
		}
	}
	return "", false
}

// upsertRequest is the strict request shape of the production upsert
// endpoint (docs/API.md, "Upsert Records"): the same shape the production
// newUpsertHandler decodes (vector_space + records with vector as a JSON
// array of numbers, metadata as a JSON object).
type upsertRequest struct {
	VectorSpace string                 `json:"vector_space"`
	Records     []vectors.UpsertRecord `json:"records"`
}

// loggingTestConfig returns a minimal, valid *config.Config for the test
// seams (the chain and bounds need only the HTTP and package-3 limits).
// The test does not read the database or TLS fields.
func loggingTestConfig() *config.Config {
	return &config.Config{
		HTTPMaxBodyBytes:   config.DefaultHTTPMaxBodyBytes,
		HTTPRequestTimeout: config.DefaultHTTPRequestTimeout,
		UpsertMaxRecords:   config.DefaultUpsertMaxRecords,
		SearchMaxLimit:     config.DefaultSearchMaxLimit,
		MaxFilters:         config.DefaultMaxFilters,
		MaxFilterValues:    config.DefaultMaxFilterValues,
		MaxMetadataBytes:   config.DefaultMaxMetadataBytes,
	}
}

// ---------------------------------------------------------------------------
// Small helpers
// ---------------------------------------------------------------------------

// isPgError extracts a *pgconn.PgError from err via the standard library
// errors.As and returns it (nil when err is not a PgError).
func pgErrorOf(err error) *pgconn.PgError {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr
	}
	return nil
}

// validVectorLiteral renders a valid pgvector text literal of the given
// dimension (all components 0.1) — a well-formed vector the trigger accepts
// dimensionally, used for the case-2 unknown-space form.
func validVectorLiteral(dims int) string {
	var b strings.Builder
	b.WriteByte('[')
	for i := 0; i < dims; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString("0.1")
	}
	b.WriteByte(']')
	return b.String()
}

// seamBody renders the crafted seam request body (the raw literal and the
// optional unknown space UUID).
func seamBody(literal, unknownSpaceUUID string) []byte {
	body, _ := json.Marshal(seamRequest{Embedding: literal, UnknownSpaceUUID: unknownSpaceUUID})
	return body
}

// assertNoDriverTextInBody asserts the response body carries none of the
// markers (the raw driver text / trigger text / markers never reach the
// response body — only the stable catalog code and message do).
func assertNoDriverTextInBody(t *testing.T, where string, body []byte, markers ...string) {
	t.Helper()
	s := string(body)
	for _, m := range markers {
		for _, n := range properPrefixes(m) {
			if strings.Contains(s, n) {
				t.Fatalf("%s: marker %q (len %d) leaked into the response body: %s",
					where, n, len(n), s)
			}
		}
	}
}

// assertCode asserts the response is a JSON error envelope with the expected
// error code (classification preserved — the stable catalog code still
// renders).
func assertCode(t *testing.T, where string, body []byte, wantCode string) {
	t.Helper()
	var env apiErrEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("%s: response is not an error envelope: %v; body: %s", where, err, string(body))
	}
	if env.Error.Code != wantCode {
		t.Fatalf("%s: error code = %q, want %q; body: %s", where, env.Error.Code, wantCode, string(body))
	}
}
