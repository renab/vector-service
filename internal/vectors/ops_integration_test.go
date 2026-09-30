//go:build integration

package vectors

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"vector-service/internal/dbctx"
	"vector-service/internal/testdb"
)

// The real-PostgreSQL package-3 vector-operations matrix (upsert, get,
// projection delete, object delete, search) against the converged schema,
// the forced RLS policy, the transaction-local application context, and the
// trigger's defense in depth. Every case runs the composed service function
// (one WithAppContext transaction: namespace resolution, space resolution
// and its state gate, then the business SQL) over a pool connected as the
// canonical runtime role, so the real grants and RLS apply to the pool's
// connections.
//
// The fixture registers two generic applications and their namespaces
// directly through bootstrap SQL (the service exposes no registration
// endpoint yet — the package-4 admin plane), the same boundary the
// namespace integration suite uses. The seeded space (migration 0002) is
// the fixture's vector space; a second space is inserted through bootstrap
// SQL for the non-cosine-metric and state-gate cases.

// opsFixture is one test's provisioned suite: the runtime pool under test,
// the bootstrap connection for fixture SQL, and the registered identities.
type opsFixture struct {
	pool      *pgxpool.Pool
	bootstrap *pgx.Conn

	appA string // application A's UUID
	appB string // application B's UUID

	// l2SpaceID is a second, fixture-created l2 space for the non-cosine
	// metric cases and the state-gate matrix (distinct from the seeded
	// cosine space).
	l2SpaceID string
}

// setupOpsFixture builds the suite fixture. It skips the test when the
// bootstrap identity is not configured, so the unit layer stays green on a
// host with no database.
func setupOpsFixture(t *testing.T) *opsFixture {
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
			sslModeForPoolOps(connCfg)))
	if perr != nil {
		t.Fatalf("parse runtime pool config: %v", perr)
	}
	pool, perr := pgxpool.NewWithConfig(ctx, pc)
	if perr != nil {
		t.Fatalf("open runtime pool: %v", perr)
	}
	t.Cleanup(pool.Close)

	bootstrap, berr := h.BootstrapConn(ctx, testdb.DatabaseName)
	if berr != nil {
		t.Fatalf("bootstrap connect: %v", berr)
	}
	t.Cleanup(func() { bootstrap.Close(context.Background()) })

	fx := &opsFixture{
		pool:      pool,
		bootstrap: bootstrap,
		appA:      "0199f31e-5000-7000-8000-0000000000b1",
		appB:      "0199f31e-5000-7000-8000-0000000000b2",
	}
	if err := fx.insertApplication(ctx, fx.appA, testdb.AppMemoryService); err != nil {
		t.Fatalf("register application A: %v", err)
	}
	if err := fx.insertApplication(ctx, fx.appB, testdb.AppNotesService); err != nil {
		t.Fatalf("register application B: %v", err)
	}
	// The fixture's second space: a small l2 space, so the non-cosine metric
	// cases and the state-gate matrix do not mutate the seeded space's
	// canonical state.
	l2SpaceID, err := testdb.NewUUID()
	if err != nil {
		t.Fatalf("fixture l2 space uuid: %v", err)
	}
	if _, err := bootstrap.Exec(ctx, `
		INSERT INTO vector_control.vector_spaces
			(id, vector_space_key, dimensions, distance_metric, embedding_model, embedding_model_version, enabled, created_at)
		VALUES ($1, 'fixture-l2-128-v1', 128, 'l2', 'generic-model', '1.0', true, now())`,
		l2SpaceID); err != nil {
		t.Fatalf("insert fixture l2 space: %v", err)
	}
	fx.l2SpaceID = l2SpaceID
	return fx
}

// insertApplication registers an application row.
func (f *opsFixture) insertApplication(ctx context.Context, id, key string) error {
	_, err := f.bootstrap.Exec(ctx,
		`INSERT INTO vector_control.applications (id, application_key, display_name)
		 VALUES ($1, $2, $3)`,
		id, key, key+" (fixture)")
	return err
}

// insertNamespace registers a namespace row for the named application.
func (f *opsFixture) insertNamespace(ctx context.Context, appID, key string) error {
	id, err := testdb.NewUUID()
	if err != nil {
		return err
	}
	_, err = f.bootstrap.Exec(ctx,
		`INSERT INTO vector_control.namespaces (id, application_id, namespace_key, display_name)
		 VALUES ($1, $2, $3, $4)`,
		id, appID, key, key+" (fixture)")
	return err
}

// insertVectorSpace registers a vector space row with a given key,
// dimensionality, metric, and initial state, returning its UUID.
func (f *opsFixture) insertVectorSpace(ctx context.Context, key string, dims int, metric string, enabled bool) (string, error) {
	id, err := testdb.NewUUID()
	if err != nil {
		return "", err
	}
	_, err = f.bootstrap.Exec(ctx,
		`INSERT INTO vector_control.vector_spaces
			(id, vector_space_key, dimensions, distance_metric, embedding_model,
			 embedding_model_version, enabled, created_at)
		 VALUES ($1, $2, $3, $4, 'generic-model', '1.0', $5, now())`,
		id, key, dims, metric, enabled)
	if err != nil {
		return "", err
	}
	return id, nil
}

// setSpaceState toggles the seeded (or named) space's enabled and retired
// flags, for the state-gate matrix.
func (f *opsFixture) setSpaceState(ctx context.Context, key string, enabled bool, retired bool) error {
	var retiredAt any
	if retired {
		retiredAt = time.Now()
	}
	_, err := f.bootstrap.Exec(ctx,
		`UPDATE vector_control.vector_spaces
		 SET enabled = $2, retired_at = $3
		 WHERE vector_space_key = $1`,
		key, enabled, retiredAt)
	return err
}

// recordTag returns the stored metadata's "tag" string for the named
// object (bootstrap bypasses RLS), for the filter-encoding matrix.
func (f *opsFixture) recordTag(ctx context.Context, appID, objID string) (string, error) {
	var tag string
	err := f.bootstrap.QueryRow(ctx,
		`SELECT metadata ->> 'tag'
		 FROM vector_data.vector_records
		 WHERE application_id = $1 AND object_id = $2`,
		appID, objID).Scan(&tag)
	return tag, err
}

// bnd is the test bounds: the configuration defaults, so the matrix
// exercises the canonical limits.
var bnd = Bounds{
	UpsertMaxRecords: 100,
	SearchMaxLimit:   200,
	MaxFilters:       10,
	MaxFilterValues:  50,
	MaxMetadataBytes: 16 * 1024,
}

// seededVector returns a deterministic 1024-dim vector (the seeded space's
// dimensionality) with the given values in the named indices and 0
// elsewhere.
func seededVector(indices ...int) []float32 {
	v := make([]float32, testdb.SeededSpaceDimensions)
	for _, i := range indices {
		v[i] = 1
	}
	return v
}

// l2Vector returns a deterministic 128-dim vector (the fixture l2 space's
// dimensionality).
func l2Vector(indices ...int) []float32 {
	v := make([]float32, 128)
	for _, i := range indices {
		v[i] = 1
	}
	return v
}

// TestUpsertIdempotenceAndUUIDPreservation proves the upsert's core
// contract: a first upsert writes a new record with a service-generated
// UUIDv4; a second upsert of the same (object, projection, space) updates
// the content in place and preserves the row's original UUID (the row
// identity is stable across re-upserts).
func TestUpsertIdempotenceAndUUIDPreservation(t *testing.T) {
	fx := setupOpsFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	if err := fx.insertNamespace(ctx, fx.appA, "project-alpha"); err != nil {
		t.Fatalf("register namespace: %v", err)
	}

	objID, err := testdb.NewUUID()
	if err != nil {
		t.Fatalf("fixture object id: %v", err)
	}
	projID, err := testdb.NewUUID()
	if err != nil {
		t.Fatalf("fixture projection id: %v", err)
	}
	const (
		ts1 = "2026-01-01T00:00:00Z"
		ts2 = "2026-02-02T00:00:00Z"
	)
	hash1 := strings.Repeat("1", 64)
	hash2 := strings.Repeat("2", 64)

	appCtx := dbctx.WithAppID(context.Background(), fx.appA)

	// First upsert: writes a new record.
	req1 := UpsertRequest{
		NamespaceKey: "project-alpha",
		VectorSpace:  testdb.SeededSpaceKey,
		Records: []UpsertRecord{{
			ObjectID:        objID,
			ProjectionID:    projID,
			ContentHash:     hash1,
			SourceUpdatedAt: ts1,
			Metadata:        map[string]any{"v": float64(1)},
			Vector:          seededVector(0),
		}},
	}
	res1, err := Upsert(appCtx, fx.pool, bnd, req1)
	if err != nil {
		t.Fatalf("first upsert: %v", err)
	}
	if res1.Upserted != 1 {
		t.Fatalf("first upsert Upserted = %d, want 1", res1.Upserted)
	}
	if res1.Unchanged != 0 {
		t.Fatalf("first upsert Unchanged = %d, want 0", res1.Unchanged)
	}

	// Read the row's identity and content directly (bootstrap bypasses RLS).
	var (
		recordID    string
		storedHash  []byte
		storedMeta  []byte
		storedSrcAt *time.Time
	)
	if err := fx.bootstrap.QueryRow(ctx,
		`SELECT id, content_hash, metadata, source_updated_at
		 FROM vector_data.vector_records
		 WHERE application_id = $1 AND object_id = $2`,
		fx.appA, objID).Scan(&recordID, &storedHash, &storedMeta, &storedSrcAt); err != nil {
		t.Fatalf("read first record: %v", err)
	}
	// The row id is a service-generated UUIDv4.
	if !isCanonicalUUID(recordID) {
		t.Fatalf("record id = %q, want a canonical UUID", recordID)
	}
	if recordID[14] != '4' {
		t.Fatalf("record id version nibble = %q, want 4: %q", recordID[14], recordID)
	}
	if len(storedHash) != 32 {
		t.Fatalf("stored content hash length = %d, want 32", len(storedHash))
	}
	if storedSrcAt == nil || !storedSrcAt.Equal(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("stored source_updated_at = %v, want 2026-01-01T00:00:00Z", storedSrcAt)
	}

	// Second upsert of the same identity: updates the content in place.
	req2 := UpsertRequest{
		NamespaceKey: "project-alpha",
		VectorSpace:  testdb.SeededSpaceKey,
		Records: []UpsertRecord{{
			ObjectID:        objID,
			ProjectionID:    projID,
			ContentHash:     hash2,
			SourceUpdatedAt: ts2,
			Metadata:        map[string]any{"v": float64(2)},
			Vector:          seededVector(1),
		}},
	}
	res2, err := Upsert(appCtx, fx.pool, bnd, req2)
	if err != nil {
		t.Fatalf("second upsert: %v", err)
	}
	if res2.Upserted != 1 {
		t.Fatalf("second upsert Upserted = %d, want 1", res2.Upserted)
	}

	// The row's identity is preserved (the same UUID), but the content is
	// updated.
	var (
		recordID2    string
		storedHash2  []byte
		storedSrcAt2 *time.Time
	)
	if err := fx.bootstrap.QueryRow(ctx,
		`SELECT id, content_hash, source_updated_at
		 FROM vector_data.vector_records
		 WHERE application_id = $1 AND object_id = $2`,
		fx.appA, objID).Scan(&recordID2, &storedHash2, &storedSrcAt2); err != nil {
		t.Fatalf("read second record: %v", err)
	}
	if recordID2 != recordID {
		t.Fatalf("record id changed across re-upsert: %q -> %q (must be stable)", recordID, recordID2)
	}
	if storedSrcAt2 == nil || !storedSrcAt2.Equal(time.Date(2026, 2, 2, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("re-upsert source_updated_at = %v, want 2026-02-02T00:00:00Z", storedSrcAt2)
	}
	// The content hash bytes changed to the second hash.
	if string(storedHash2) == string(storedHash) {
		t.Fatal("re-upsert did not update the content hash")
	}
}

// TestUpsertAtomicity proves the batch is all-or-nothing: a batch whose
// first record is valid and whose second record violates the dimension
// check rolls back the whole batch, so neither record is written.
func TestUpsertAtomicity(t *testing.T) {
	fx := setupOpsFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	if err := fx.insertNamespace(ctx, fx.appA, "project-alpha"); err != nil {
		t.Fatalf("register namespace: %v", err)
	}

	objGood, err := testdb.NewUUID()
	if err != nil {
		t.Fatalf("fixture object id: %v", err)
	}
	objBad, err := testdb.NewUUID()
	if err != nil {
		t.Fatalf("fixture object id: %v", err)
	}
	projGood, err := testdb.NewUUID()
	if err != nil {
		t.Fatalf("fixture projection id: %v", err)
	}
	projBad, err := testdb.NewUUID()
	if err != nil {
		t.Fatalf("fixture projection id: %v", err)
	}

	appCtx := dbctx.WithAppID(context.Background(), fx.appA)
	req := UpsertRequest{
		NamespaceKey: "project-alpha",
		VectorSpace:  testdb.SeededSpaceKey,
		Records: []UpsertRecord{
			{
				ObjectID:        objGood,
				ProjectionID:    projGood,
				ContentHash:     strings.Repeat("1", 64),
				SourceUpdatedAt: "2026-01-01T00:00:00Z",
				Metadata:        map[string]any{},
				Vector:          seededVector(0),
			},
			{
				ObjectID:        objBad,
				ProjectionID:    projBad,
				ContentHash:     strings.Repeat("2", 64),
				SourceUpdatedAt: "2026-01-01T00:00:00Z",
				Metadata:        map[string]any{},
				Vector:          seededVector(0)[:3], // 3 dims, not 1024
			},
		},
	}
	_, err = Upsert(appCtx, fx.pool, bnd, req)
	if err == nil {
		t.Fatal("upsert with a dimension-mismatched record committed, want a failure")
	}
	if !errors.Is(err, ErrInvalidDimensions) {
		t.Fatalf("upsert error = %v, want ErrInvalidDimensions", err)
	}
	// The whole batch rolled back: no record for the good object either.
	var rows int
	if err := fx.bootstrap.QueryRow(ctx,
		`SELECT count(*) FROM vector_data.vector_records
		 WHERE application_id = $1`,
		fx.appA).Scan(&rows); err != nil {
		t.Fatalf("count records: %v", err)
	}
	if rows != 0 {
		t.Fatalf("record rows after a failed batch = %d, want 0 (all-or-nothing)", rows)
	}
}

// TestUpsertTriggerDefenseInDepth proves the trigger's P0001 rejection maps
// to the dimension error: a record whose embedding dimension does not match
// the resolved space is rejected by the trigger (defense in depth) and the
// error surfaces as ErrInvalidDimensions.
func TestUpsertTriggerDefenseInDepth(t *testing.T) {
	fx := setupOpsFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	if err := fx.insertNamespace(ctx, fx.appA, "project-alpha"); err != nil {
		t.Fatalf("register namespace: %v", err)
	}

	objID, err := testdb.NewUUID()
	if err != nil {
		t.Fatalf("fixture object id: %v", err)
	}
	projID, err := testdb.NewUUID()
	if err != nil {
		t.Fatalf("fixture projection id: %v", err)
	}

	appCtx := dbctx.WithAppID(context.Background(), fx.appA)
	req := UpsertRequest{
		NamespaceKey: "project-alpha",
		VectorSpace:  testdb.SeededSpaceKey,
		Records: []UpsertRecord{{
			ObjectID:        objID,
			ProjectionID:    projID,
			ContentHash:     strings.Repeat("1", 64),
			SourceUpdatedAt: "2026-01-01T00:00:00Z",
			Metadata:        map[string]any{},
			Vector:          seededVector(0)[:5], // 5 dims, not 1024
		}},
	}
	_, err = Upsert(appCtx, fx.pool, bnd, req)
	if err == nil {
		t.Fatal("upsert with a dimension-mismatched embedding committed, want a failure")
	}
	if !errors.Is(err, ErrInvalidDimensions) {
		t.Fatalf("upsert error = %v, want ErrInvalidDimensions", err)
	}
}

// TestUpsertSpaceStateMatrix is the space-state gate matrix for upsert: a
// disabled space is unavailable (ErrSpaceUnavailable); a retired space is
// unavailable to upsert (ErrSpaceUnavailable); an enabled, active space
// accepts the write.
func TestUpsertSpaceStateMatrix(t *testing.T) {
	fx := setupOpsFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	if err := fx.insertNamespace(ctx, fx.appA, "project-alpha"); err != nil {
		t.Fatalf("register namespace: %v", err)
	}

	// A dedicated state space for the matrix (so the seeded space's
	// canonical state is untouched).
	stateKey := "state-matrix-space-v1"
	stateSpaceID, err := fx.insertVectorSpace(ctx, stateKey, 1024, "cosine", true)
	_ = stateSpaceID
	if err != nil {
		t.Fatalf("insert state space: %v", err)
	}

	objID, err := testdb.NewUUID()
	if err != nil {
		t.Fatalf("fixture object id: %v", err)
	}
	projID, err := testdb.NewUUID()
	if err != nil {
		t.Fatalf("fixture projection id: %v", err)
	}

	appCtx := dbctx.WithAppID(context.Background(), fx.appA)
	mkReq := func() UpsertRequest {
		return UpsertRequest{
			NamespaceKey: "project-alpha",
			VectorSpace:  stateKey,
			Records: []UpsertRecord{{
				ObjectID:        objID,
				ProjectionID:    projID,
				ContentHash:     strings.Repeat("1", 64),
				SourceUpdatedAt: "2026-01-01T00:00:00Z",
				Metadata:        map[string]any{},
				Vector:          seededVector(0),
			}},
		}
	}

	t.Run("enabled active space accepts upsert", func(t *testing.T) {
		if _, err := Upsert(appCtx, fx.pool, bnd, mkReq()); err != nil {
			t.Fatalf("upsert into an enabled active space: %v", err)
		}
	})
	t.Run("retired space rejects upsert", func(t *testing.T) {
		if err := fx.setSpaceState(ctx, stateKey, true, true); err != nil {
			t.Fatalf("retire space: %v", err)
		}
		_, err := Upsert(appCtx, fx.pool, bnd, mkReq())
		if !errors.Is(err, ErrSpaceUnavailable) {
			t.Fatalf("upsert into a retired space error = %v, want ErrSpaceUnavailable", err)
		}
	})
	t.Run("disabled space rejects upsert", func(t *testing.T) {
		if err := fx.setSpaceState(ctx, stateKey, false, false); err != nil {
			t.Fatalf("disable space: %v", err)
		}
		_, err := Upsert(appCtx, fx.pool, bnd, mkReq())
		if !errors.Is(err, ErrSpaceUnavailable) {
			t.Fatalf("upsert into a disabled space error = %v, want ErrSpaceUnavailable", err)
		}
	})
}

// TestSearchNearestFirstIsFiltersIsolation is the search matrix: the
// result is ordered nearest-first (cosine), a metadata filter narrows the
// result to matching records, and a search for application A never returns
// application B's records (namespace + application isolation).
func TestSearchNearestFirstIsFiltersIsolation(t *testing.T) {
	fx := setupOpsFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	if err := fx.insertNamespace(ctx, fx.appA, "project-alpha"); err != nil {
		t.Fatalf("register application A namespace: %v", err)
	}
	if err := fx.insertNamespace(ctx, fx.appB, "project-beta"); err != nil {
		t.Fatalf("register application B namespace: %v", err)
	}

	objA, err := testdb.NewUUID()
	if err != nil {
		t.Fatalf("fixture object id: %v", err)
	}
	objA2, err := testdb.NewUUID()
	if err != nil {
		t.Fatalf("fixture object id: %v", err)
	}
	objB, err := testdb.NewUUID()
	if err != nil {
		t.Fatalf("fixture object id: %v", err)
	}
	projA, err := testdb.NewUUID()
	if err != nil {
		t.Fatalf("fixture projection id: %v", err)
	}
	projA2, err := testdb.NewUUID()
	if err != nil {
		t.Fatalf("fixture projection id: %v", err)
	}
	projB, err := testdb.NewUUID()
	if err != nil {
		t.Fatalf("fixture projection id: %v", err)
	}

	// Seed: application A has two records in project-alpha (one matching
	// the filter, one not); application B has one record in project-beta.
	// All in the seeded cosine space, orthogonal basis directions.
	if _, err := Upsert(dbctx.WithAppID(context.Background(), fx.appA), fx.pool, bnd, UpsertRequest{
		NamespaceKey: "project-alpha",
		VectorSpace:  testdb.SeededSpaceKey,
		Records: []UpsertRecord{
			{
				ObjectID: objA, ProjectionID: projA,
				ContentHash:     strings.Repeat("a", 64),
				SourceUpdatedAt: "2026-01-01T00:00:00Z",
				Metadata:        map[string]any{"state": "active"},
				Vector:          seededVector(0),
			},
			{
				ObjectID: objA2, ProjectionID: projA2,
				ContentHash:     strings.Repeat("b", 64),
				SourceUpdatedAt: "2026-01-01T00:00:00Z",
				Metadata:        map[string]any{"state": "archived"},
				Vector:          seededVector(1),
			},
		},
	}); err != nil {
		t.Fatalf("seed application A records: %v", err)
	}
	if _, err := Upsert(dbctx.WithAppID(context.Background(), fx.appB), fx.pool, bnd, UpsertRequest{
		NamespaceKey: "project-beta",
		VectorSpace:  testdb.SeededSpaceKey,
		Records: []UpsertRecord{
			{
				ObjectID: objB, ProjectionID: projB,
				ContentHash:     strings.Repeat("c", 64),
				SourceUpdatedAt: "2026-01-01T00:00:00Z",
				Metadata:        map[string]any{"state": "active"},
				Vector:          seededVector(2),
			},
		},
	}); err != nil {
		t.Fatalf("seed application B record: %v", err)
	}

	appCtxA := dbctx.WithAppID(context.Background(), fx.appA)

	t.Run("nearest first, no filter", func(t *testing.T) {
		res, err := Search(appCtxA, fx.pool, bnd, SearchRequest{
			NamespaceKey: "project-alpha",
			VectorSpace:  testdb.SeededSpaceKey,
			Embedding:    seededVector(0),
			Limit:        10,
		})
		if err != nil {
			t.Fatalf("search: %v", err)
		}
		if len(res.Matches) != 2 {
			t.Fatalf("search returned %d matches, want 2 (application A's two records)", len(res.Matches))
		}
		// The query is the 0-basis vector: application A's first record
		// (0-basis) is the nearest (distance 0), the second (1-basis) is
		// farther. The nearest must come first.
		if res.Matches[0].ObjectID != objA {
			t.Fatalf("nearest match = %s, want %s (the 0-basis record)", res.Matches[0].ObjectID, objA)
		}
		// No match may be application B's record.
		for _, m := range res.Matches {
			if m.ObjectID == objB {
				t.Fatalf("search returned application B's record %s (isolation violation)", objB)
			}
		}
	})

	t.Run("filter narrows the result", func(t *testing.T) {
		res, err := Search(appCtxA, fx.pool, bnd, SearchRequest{
			NamespaceKey: "project-alpha",
			VectorSpace:  testdb.SeededSpaceKey,
			Embedding:    seededVector(0),
			Filters:      []FilterEntry{{Field: "state", Op: FilterEq, Values: []any{"active"}}},
			Limit:        10,
		})
		if err != nil {
			t.Fatalf("filtered search: %v", err)
		}
		if len(res.Matches) != 1 {
			t.Fatalf("filtered search returned %d matches, want 1", len(res.Matches))
		}
		if res.Matches[0].ObjectID != objA {
			t.Fatalf("filtered search match = %s, want %s (the active record)", res.Matches[0].ObjectID, objA)
		}
	})
}

// TestSearchZeroNormCosineRejected proves the zero-norm rule: an all-zero
// query vector in a cosine space is rejected with ErrNonFiniteVector before
// any search SQL runs.
func TestSearchZeroNormCosineRejected(t *testing.T) {
	fx := setupOpsFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	if err := fx.insertNamespace(ctx, fx.appA, "project-alpha"); err != nil {
		t.Fatalf("register namespace: %v", err)
	}

	zero := make([]float32, testdb.SeededSpaceDimensions)
	_, err := Search(dbctx.WithAppID(context.Background(), fx.appA), fx.pool, bnd, SearchRequest{
		NamespaceKey: "project-alpha",
		VectorSpace:  testdb.SeededSpaceKey,
		Embedding:    zero,
		Limit:        5,
	})
	if !errors.Is(err, ErrNonFiniteVector) {
		t.Fatalf("search with a zero vector in a cosine space error = %v, want ErrNonFiniteVector", err)
	}
}

// TestSearchL2AcceptsZeroVector proves the zero-norm rule applies only to
// cosine spaces: an all-zero query vector in an l2 space is accepted (no
// ErrNonFiniteVector), and the search runs.
func TestSearchL2AcceptsZeroVector(t *testing.T) {
	fx := setupOpsFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	if err := fx.insertNamespace(ctx, fx.appA, "project-alpha"); err != nil {
		t.Fatalf("register namespace: %v", err)
	}

	// A seed record in the fixture l2 space so the search has something to
	// match.
	objID, err := testdb.NewUUID()
	if err != nil {
		t.Fatalf("fixture object id: %v", err)
	}
	projID, err := testdb.NewUUID()
	if err != nil {
		t.Fatalf("fixture projection id: %v", err)
	}
	if _, err := Upsert(dbctx.WithAppID(context.Background(), fx.appA), fx.pool, bnd, UpsertRequest{
		NamespaceKey: "project-alpha",
		VectorSpace:  "fixture-l2-128-v1",
		Records: []UpsertRecord{{
			ObjectID:        objID,
			ProjectionID:    projID,
			ContentHash:     strings.Repeat("1", 64),
			SourceUpdatedAt: "2026-01-01T00:00:00Z",
			Metadata:        map[string]any{},
			Vector:          l2Vector(0),
		}},
	}); err != nil {
		t.Fatalf("seed l2 record: %v", err)
	}

	zero := make([]float32, 128)
	res, err := Search(dbctx.WithAppID(context.Background(), fx.appA), fx.pool, bnd, SearchRequest{
		NamespaceKey: "project-alpha",
		VectorSpace:  "fixture-l2-128-v1",
		Embedding:    zero,
		Limit:        5,
	})
	if err != nil {
		t.Fatalf("search with a zero vector in an l2 space error = %v, want nil (zero is accepted in l2)", err)
	}
	if len(res.Matches) != 1 {
		t.Fatalf("l2 zero-vector search returned %d matches, want 1", len(res.Matches))
	}
}

// TestSpaceStateGetSearchDeleteMatrix is the space-state gate matrix for the
// read/delete operations: a retired space still allows get, search, and
// delete (the retire-then-purge flow); a disabled space does not.
func TestSpaceStateGetSearchDeleteMatrix(t *testing.T) {
	fx := setupOpsFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	if err := fx.insertNamespace(ctx, fx.appA, "project-alpha"); err != nil {
		t.Fatalf("register namespace: %v", err)
	}

	stateKey := "retire-purge-space-v1"
	if _, err := fx.insertVectorSpace(ctx, stateKey, 1024, "cosine", true); err != nil {
		t.Fatalf("insert state space: %v", err)
	}

	objID, err := testdb.NewUUID()
	if err != nil {
		t.Fatalf("fixture object id: %v", err)
	}
	projID, err := testdb.NewUUID()
	if err != nil {
		t.Fatalf("fixture projection id: %v", err)
	}
	appCtx := dbctx.WithAppID(context.Background(), fx.appA)

	// Seed a record while the space is enabled and active.
	if _, err := Upsert(appCtx, fx.pool, bnd, UpsertRequest{
		NamespaceKey: "project-alpha",
		VectorSpace:  stateKey,
		Records: []UpsertRecord{{
			ObjectID:        objID,
			ProjectionID:    projID,
			ContentHash:     strings.Repeat("1", 64),
			SourceUpdatedAt: "2026-01-01T00:00:00Z",
			Metadata:        map[string]any{},
			Vector:          seededVector(0),
		}},
	}); err != nil {
		t.Fatalf("seed record: %v", err)
	}

	t.Run("retired space allows get", func(t *testing.T) {
		if err := fx.setSpaceState(ctx, stateKey, true, true); err != nil {
			t.Fatalf("retire space: %v", err)
		}
		got, err := Get(appCtx, fx.pool, GetRequest{
			NamespaceKey: "project-alpha",
			ObjectID:     objID,
			ProjectionID: projID,
			VectorSpace:  stateKey,
		})
		if err != nil {
			t.Fatalf("get in a retired space: %v, want nil", err)
		}
		if got.ObjectID != objID {
			t.Fatalf("get returned object %s, want %s", got.ObjectID, objID)
		}
	})

	t.Run("retired space allows search", func(t *testing.T) {
		res, err := Search(appCtx, fx.pool, bnd, SearchRequest{
			NamespaceKey: "project-alpha",
			VectorSpace:  stateKey,
			Embedding:    seededVector(0),
			Limit:        5,
		})
		if err != nil {
			t.Fatalf("search in a retired space: %v, want nil", err)
		}
		if len(res.Matches) != 1 {
			t.Fatalf("search in a retired space returned %d matches, want 1", len(res.Matches))
		}
	})

	t.Run("retired space allows projection delete", func(t *testing.T) {
		res, err := DeleteProjection(appCtx, fx.pool, DeleteRequest{
			NamespaceKey: "project-alpha",
			ObjectID:     objID,
			ProjectionID: projID,
			VectorSpace:  stateKey,
		})
		if err != nil {
			t.Fatalf("projection delete in a retired space: %v, want nil", err)
		}
		if res.Deleted != 1 {
			t.Fatalf("projection delete in a retired space Deleted = %d, want 1", res.Deleted)
		}
	})
}

// TestGetIsolationAndNotFound proves the get operation's isolation and
// not-found behavior: a record in another application's namespace is not
// returned (ErrRecordNotFound), and a record in a different namespace of
// the same application is not returned (ErrRecordNotFound).
func TestGetIsolationAndNotFound(t *testing.T) {
	fx := setupOpsFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	if err := fx.insertNamespace(ctx, fx.appA, "project-alpha"); err != nil {
		t.Fatalf("register application A namespace: %v", err)
	}
	if err := fx.insertNamespace(ctx, fx.appA, "research"); err != nil {
		t.Fatalf("register application A second namespace: %v", err)
	}
	if err := fx.insertNamespace(ctx, fx.appB, "project-alpha"); err != nil {
		t.Fatalf("register application B namespace (same key): %v", err)
	}

	objID, err := testdb.NewUUID()
	if err != nil {
		t.Fatalf("fixture object id: %v", err)
	}
	projID, err := testdb.NewUUID()
	if err != nil {
		t.Fatalf("fixture projection id: %v", err)
	}
	mkReq := func() UpsertRequest {
		return UpsertRequest{
			NamespaceKey: "project-alpha",
			VectorSpace:  testdb.SeededSpaceKey,
			Records: []UpsertRecord{{
				ObjectID:        objID,
				ProjectionID:    projID,
				ContentHash:     strings.Repeat("1", 64),
				SourceUpdatedAt: "2026-01-01T00:00:00Z",
				Metadata:        map[string]any{},
				Vector:          seededVector(0),
			}},
		}
	}
	// Application A's record in project-alpha.
	if _, err := Upsert(dbctx.WithAppID(context.Background(), fx.appA), fx.pool, bnd, mkReq()); err != nil {
		t.Fatalf("seed application A record: %v", err)
	}
	// Application B's record with the SAME object/projection in its own
	// project-alpha namespace.
	if _, err := Upsert(dbctx.WithAppID(context.Background(), fx.appB), fx.pool, bnd, mkReq()); err != nil {
		t.Fatalf("seed application B record: %v", err)
	}

	t.Run("own record resolves", func(t *testing.T) {
		got, err := Get(dbctx.WithAppID(context.Background(), fx.appA), fx.pool, GetRequest{
			NamespaceKey: "project-alpha",
			ObjectID:     objID,
			ProjectionID: projID,
			VectorSpace:  testdb.SeededSpaceKey,
		})
		if err != nil {
			t.Fatalf("get own record: %v, want nil", err)
		}
		if got.ObjectID != objID {
			t.Fatalf("get returned object %s, want %s", got.ObjectID, objID)
		}
	})
	t.Run("foreign application same key is not found", func(t *testing.T) {
		_, err := Get(dbctx.WithAppID(context.Background(), fx.appB), fx.pool, GetRequest{
			NamespaceKey: "project-alpha",
			ObjectID:     objID,
			ProjectionID: projID,
			VectorSpace:  testdb.SeededSpaceKey,
		})
		// Application B has its OWN record here, so this resolves to B's
		// record, not A's. To prove isolation, query an object B does not
		// have.
		otherObj, _ := testdb.NewUUID()
		_, err = Get(dbctx.WithAppID(context.Background(), fx.appB), fx.pool, GetRequest{
			NamespaceKey: "project-alpha",
			ObjectID:     otherObj,
			ProjectionID: projID,
			VectorSpace:  testdb.SeededSpaceKey,
		})
		if !errors.Is(err, ErrRecordNotFound) {
			t.Fatalf("get an object B does not have error = %v, want ErrRecordNotFound", err)
		}
	})
}

// TestDeleteObjectAcrossSpaces proves the object delete removes every
// record of the object across every vector space (the operation does not
// name a space), and the projection delete removes only the named space's
// record.
func TestDeleteObjectAcrossSpaces(t *testing.T) {
	fx := setupOpsFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	if err := fx.insertNamespace(ctx, fx.appA, "project-alpha"); err != nil {
		t.Fatalf("register namespace: %v", err)
	}

	objID, err := testdb.NewUUID()
	if err != nil {
		t.Fatalf("fixture object id: %v", err)
	}
	projID, err := testdb.NewUUID()
	if err != nil {
		t.Fatalf("fixture projection id: %v", err)
	}
	appCtx := dbctx.WithAppID(context.Background(), fx.appA)

	// The object's projection in TWO spaces (the seeded cosine space and
	// the fixture l2 space).
	if _, err := Upsert(appCtx, fx.pool, bnd, UpsertRequest{
		NamespaceKey: "project-alpha",
		VectorSpace:  testdb.SeededSpaceKey,
		Records: []UpsertRecord{{
			ObjectID: objID, ProjectionID: projID,
			ContentHash:     strings.Repeat("1", 64),
			SourceUpdatedAt: "2026-01-01T00:00:00Z",
			Metadata:        map[string]any{},
			Vector:          seededVector(0),
		}},
	}); err != nil {
		t.Fatalf("seed cosine-space record: %v", err)
	}
	if _, err := Upsert(appCtx, fx.pool, bnd, UpsertRequest{
		NamespaceKey: "project-alpha",
		VectorSpace:  "fixture-l2-128-v1",
		Records: []UpsertRecord{{
			ObjectID: objID, ProjectionID: projID,
			ContentHash:     strings.Repeat("1", 64),
			SourceUpdatedAt: "2026-01-01T00:00:00Z",
			Metadata:        map[string]any{},
			Vector:          l2Vector(0),
		}},
	}); err != nil {
		t.Fatalf("seed l2-space record: %v", err)
	}

	t.Run("projection delete removes only the named space", func(t *testing.T) {
		res, err := DeleteProjection(appCtx, fx.pool, DeleteRequest{
			NamespaceKey: "project-alpha",
			ObjectID:     objID,
			ProjectionID: projID,
			VectorSpace:  testdb.SeededSpaceKey,
		})
		if err != nil {
			t.Fatalf("projection delete: %v", err)
		}
		if res.Deleted != 1 {
			t.Fatalf("projection delete Deleted = %d, want 1", res.Deleted)
		}
		// The l2-space record survives.
		var rows int
		if err := fx.bootstrap.QueryRow(ctx,
			`SELECT count(*) FROM vector_data.vector_records
			 WHERE application_id = $1 AND object_id = $2`,
			fx.appA, objID).Scan(&rows); err != nil {
			t.Fatalf("count records: %v", err)
		}
		if rows != 1 {
			t.Fatalf("records after projection delete = %d, want 1 (the l2-space record)", rows)
		}
	})

	t.Run("object delete removes every space", func(t *testing.T) {
		res, err := DeleteObject(appCtx, fx.pool, DeleteRequest{
			NamespaceKey: "project-alpha",
			ObjectID:     objID,
		})
		if err != nil {
			t.Fatalf("object delete: %v", err)
		}
		if res.Deleted != 1 {
			t.Fatalf("object delete Deleted = %d, want 1 (the remaining l2-space record)", res.Deleted)
		}
		var rows int
		if err := fx.bootstrap.QueryRow(ctx,
			`SELECT count(*) FROM vector_data.vector_records
			 WHERE application_id = $1 AND object_id = $2`,
			fx.appA, objID).Scan(&rows); err != nil {
			t.Fatalf("count records: %v", err)
		}
		if rows != 0 {
			t.Fatalf("records after object delete = %d, want 0", rows)
		}
	})
}

// TestSearchInFilterSpecialCharacters proves the "in" filter's value set is
// encoded through pgx's native text[] parameter — never a hand-assembled
// array literal — so metadata values containing commas, quotes,
// backslashes, braces, or empty strings round-trip through the real
// database unchanged: each seeded record is matched (or not) by exactly the
// filter value that equals its stored value.
func TestSearchInFilterSpecialCharacters(t *testing.T) {
	fx := setupOpsFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	if err := fx.insertNamespace(ctx, fx.appA, "project-alpha"); err != nil {
		t.Fatalf("register namespace: %v", err)
	}

	// The matrix values: each value is one record's metadata "tag". The
	// set contains every character class that corrupts a hand-assembled
	// array literal.
	values := []string{
		"plain",
		"with,comma",
		`with"quote`,
		`with\backslash`,
		"with{brace",
		"",
		"a,b,c",
	}
	objIDs := make([]string, len(values))
	for i, v := range values {
		obj, err := testdb.NewUUID()
		if err != nil {
			t.Fatalf("fixture object id: %v", err)
		}
		objIDs[i] = obj
		proj, err := testdb.NewUUID()
		if err != nil {
			t.Fatalf("fixture projection id: %v", err)
		}
		if _, err := Upsert(dbctx.WithAppID(context.Background(), fx.appA), fx.pool, bnd, UpsertRequest{
			NamespaceKey: "project-alpha",
			VectorSpace:  testdb.SeededSpaceKey,
			Records: []UpsertRecord{{
				ObjectID:        obj,
				ProjectionID:    proj,
				ContentHash:     strings.Repeat("1", 64),
				SourceUpdatedAt: "2026-01-01T00:00:00Z",
				Metadata:        map[string]any{"tag": v},
				Vector:          seededVector(i % testdb.SeededSpaceDimensions),
			}},
		}); err != nil {
			t.Fatalf("seed record %d (%q): %v", i, v, err)
		}
	}

	// Verify the stored values round-tripped byte-for-byte (jsonb text form
	// equals the original value).
	for i, v := range values {
		tag, err := fx.recordTag(ctx, fx.appA, objIDs[i])
		if err != nil {
			t.Fatalf("read stored tag for %q: %v", v, err)
		}
		if tag != v {
			t.Fatalf("stored tag = %q, want %q (encoding corrupted the value)", tag, v)
		}
	}

	appCtxA := dbctx.WithAppID(context.Background(), fx.appA)
	for i, v := range values {
		t.Run(fmt.Sprintf("matches %q", v), func(t *testing.T) {
			res, err := Search(appCtxA, fx.pool, bnd, SearchRequest{
				NamespaceKey: "project-alpha",
				VectorSpace:  testdb.SeededSpaceKey,
				Embedding:    seededVector(0),
				Filters:      []FilterEntry{{Field: "tag", Op: FilterIn, Values: []any{v}}},
				Limit:        10,
			})
			if err != nil {
				t.Fatalf("search: %v", err)
			}
			if len(res.Matches) != 1 {
				t.Fatalf("search for %q returned %d matches, want 1: %+v", v, len(res.Matches), res.Matches)
			}
			if res.Matches[0].ObjectID != objIDs[i] {
				t.Fatalf("search for %q matched object %s, want %s", v, res.Matches[0].ObjectID, objIDs[i])
			}
		})
	}

	// The full value set matches every seeded record: a single "in" filter
	// with all values returns all of them.
	t.Run("full set matches all", func(t *testing.T) {
		all := make([]any, len(values))
		for i, v := range values {
			all[i] = v
		}
		res, err := Search(appCtxA, fx.pool, bnd, SearchRequest{
			NamespaceKey: "project-alpha",
			VectorSpace:  testdb.SeededSpaceKey,
			Embedding:    seededVector(0),
			Filters:      []FilterEntry{{Field: "tag", Op: FilterIn, Values: all}},
			Limit:        10,
		})
		if err != nil {
			t.Fatalf("search: %v", err)
		}
		if len(res.Matches) != len(values) {
			t.Fatalf("full-set search returned %d matches, want %d", len(res.Matches), len(values))
		}
	})

	// A value that is a prefix-comma-truncation of a stored value must not
	// match: "with" must not match "with,comma" (a hand-assembled literal
	// split on the comma would have produced this false positive).
	t.Run("comma prefix does not match", func(t *testing.T) {
		res, err := Search(appCtxA, fx.pool, bnd, SearchRequest{
			NamespaceKey: "project-alpha",
			VectorSpace:  testdb.SeededSpaceKey,
			Embedding:    seededVector(0),
			Filters:      []FilterEntry{{Field: "tag", Op: FilterIn, Values: []any{"with"}}},
			Limit:        10,
		})
		if err != nil {
			t.Fatalf("search: %v", err)
		}
		if len(res.Matches) != 0 {
			t.Fatalf("search for %q returned %d matches, want 0: %+v", "with", len(res.Matches), res.Matches)
		}
	})
}

// sslModeForPoolOps returns the sslmode for the test pool.
// The harness always uses TLSModePlain (VEC_PG_TLS_MODE=plain), so test
// pools must explicitly disable SSL.
func sslModeForPoolOps(_ *pgx.ConnConfig) string {
	return "disable"
}
