# Unit 08 — Nearest-Neighbor Search with Metadata Filtering

## Objective

Implement the data-plane search endpoint defined by `docs/API.md`:

```text
POST /v1/namespaces/{namespace}/search
```

Nearest-neighbor search over exactly one namespace and exactly one vector
space, with the constrained v1 metadata filter language (`all` + `eq`/`in`
only). The stored embedding is never returned.

## Authority

- `docs/API.md` — endpoint shape; request fields
  (`vector_space`, `vector`, `limit`, `filters`); `limit` range `1..200`
  with a lower deployment maximum allowed; response shape
  (`matches[]` with `record_id`, `object_id`, `projection_id`, `score`,
  `content_hash`, `source_updated_at`, `metadata`); "The stored embedding
  is not returned"; Score Semantics — normalized, higher = better,
  consumers must not depend on raw pgvector distances, exact transformation
  is an implementation detail that must stay consistent for a given
  vector-space distance metric; Metadata Filtering — constrained
  structured language, no SQL/JSONPath/arbitrary expressions, v1
  operators `eq` and `in`, top-level metadata keys only, `filters.all`
  array, all filters must match, no nested boolean expressions.
- `docs/SECURITY.md` — no embedding generation or reranking; caller
  metadata is caller data, never an authorization input; uniform 404s.
- `docs/DATA_MODEL.md` — search reads visible records only; metadata is
  untrusted caller data.
- `migrations/0001_shared_vector_schema.sql` — `vector_records` columns;
  `distance_metric` enum (`cosine | l2 | inner_product`); forced RLS;
  `vector_records_content_hash_idx`; the 5-tuple identity.
- `migrations/0002_qwen3_embedding_space.sql` — seeded space
  `qwen3-embedding-0.6b-1024-cosine-v1`, UUID
  `0199f31e-1000-7000-8000-000000000001`, 1024 dims, cosine.
- `migrations/0003_qwen3_hnsq.sql` — the **only** HNSW index: partial
  `(embedding::vector(1024)) vector_cosine_ops` restricted to the seeded
  space UUID. Search SQL for any other space or metric falls back to an
  exact (sequential) scan; the index must not be assumed to exist
  anywhere else.
- `docs/implementation/04-isolation-core.md` — `WithAppContext`
  pipeline, namespace resolution, read access to `vector_spaces`.
- `docs/implementation/05-authentication.md` — authenticated identity.
- `docs/implementation/07-vector-crud.md` — shared vector-space
  resolution and request-side vector validation helpers (owned by unit
  07, consumed here); error semantics; the 404/422 space-state split (C7).
- `docs/implementation/README.md` — config reference
  (`VEC_SEARCH_MAX_LIMIT`, `VEC_MAX_FILTERS`, `VEC_MAX_FILTER_VALUES`);
  error catalog (`invalid_limit`, `invalid_filter`,
  `vector_space_not_found`, `vector_space_unavailable`,
  `namespace_not_found`, …); Global Invariants.

## Dependencies

- Units 01–05 (apierr, pool, `WithAppContext`, namespace resolution,
  authentication).
- Unit 02 (pgx `vector` text-format codec — the query vector is bound as
  text `[a,b,c]`).
- Unit 07 (space resolution + vector validation helpers, shared code).
- Unit 09a (strict JSON, body cap) is consumed at runtime.

## Scope

### Request and validation order

Request body (strict decoding per unit 09a; unknown top-level fields →
400 `unknown_field`):

```json
{
  "vector_space": "qwen3-embedding-0.6b-1024-cosine-v1",
  "vector": [ 0.0123, -0.456, "…1024 elements…" ],
  "limit": 20,
  "filters": { "all": [ … ] }
}
```

Validation runs cheap-first, before any database work (DEVELOPMENT.md):

1. `vector_space`: present, non-empty string → 400 `missing_field`.
2. `limit`: optional. When present it must be an integer (no fractions,
   no strings) in `1..min(200, VEC_SEARCH_MAX_LIMIT)` → else 400
   `invalid_limit`. When omitted, default **20** (proposal; review point —
   the API example uses 20 but does not pin a default; the *existence* of
   the hard ceiling 200 and the configurable lower maximum is required).
3. `vector`: shared vector validation (unit 07): JSON array of numbers
   → else 400 `invalid_vector`; every element finite → else 422
   `non_finite_vector` (per unit 07's decode-path rule). The dimension
   check (`len(vector) == space.dimensions`, 422
   `invalid_vector_dimensions`) runs after space resolution because it
   needs the resolved dimensionality.
4. `filters`: optional. When present, validate the filter language
   (below) before database work → 400 `invalid_filter`.

Then: namespace resolution (unit 04, uniform 404
`namespace_not_found`) → vector-space resolution (unit 07 shared helper;
unknown → 404 `vector_space_not_found`; disabled → 422
`vector_space_unavailable`; retired-but-enabled → **allowed**, search is a
read operation under the C7 split) → dimension check → execute the search
statement.

### Metadata filter language (v1)

Accepted forms, exactly:

```json
{}                                                  // no filters
{ "all": [] }                                       // no predicates
{
  "all": [
    { "field": "document_type", "op": "eq", "value": "note" },
    { "field": "state", "op": "in", "values": [ "active", "current" ] }
  ]
}
```

Rules (all violations → 400 `invalid_filter`):

- `filters` must be a JSON **object** with at most one key, `all`.
  Any other key (e.g. `any`, `not`, `or`) is an unsupported v1 construct.
- `all` must be a JSON array.
- Entry count ≤ `VEC_MAX_FILTERS` (default 10).
- Each entry is a JSON object with exactly the fields:
  - `field`: non-empty string, length ≤ 128 (proposal; review point).
    The field is always bound as a query parameter — the grammar check is
    a defensive bound, not a SQL-injection control (Global Invariant 11).
  - `op`: exactly `"eq"` or `"in"`.
  - For `op: "eq"`: exactly `value` present, a JSON string; `values`
    absent. (Non-string `value` → `invalid_filter` per the catalog's
    "non-string value".)
  - For `op: "in"`: exactly `values` present, a non-empty JSON array of
    strings, count ≤ `VEC_MAX_FILTER_VALUES` (default 50); `value`
      absent.
- Unknown or missing keys on an entry → `invalid_filter`.
- Duplicate filters on the same field are **allowed** (they compose by
  AND; the result may legitimately be empty). v1 does not forbid them.
- No nested boolean expressions, no other operators, no JSONPath, no
  dotted field names — API v1 is top-level keys only. (Dotted strings
  are legal key names in `->>` lookups but v1 callers should not rely on
  them; the grammar bound above is all that is promised.)

Semantics: **all** entries in `all` must match. A record whose metadata
lacks a filtered key does not match, because `metadata ->> $field`
yields `NULL` for a missing key and `NULL = …` / `NULL = ANY(…)` is not
true.

### Filter SQL construction

Built in Go; every field name and value is a bound parameter (Global
Invariant 11). No SQL fragment is ever interpolated from caller input.

Per entry:

```sql
-- eq
AND r.metadata ->> $f = $v

-- in   (pgx encodes the []string parameter as a PostgreSQL array)
AND r.metadata ->> $f = ANY($vals)
```

The `->>` operator performs a text lookup on the JSONB object; the bound
field parameter is the JSON object key. The resulting predicates are
appended to the WHERE clause in the order given.

### Search statement

One statement, inside the unit 04 request transaction
(`WithAppContext(appID, fn)`), after namespace + space resolution:

```sql
-- D = the resolved space's dimensions (server-side integer);
-- op = the metric operator selected in Go (see the table below).
SELECT
    r.id,
    r.object_id,
    r.projection_id,
    r.content_hash,
    r.source_updated_at,
    r.metadata,
    r.embedding::vector(D) op $q  AS distance
FROM vector_data.vector_records r
WHERE r.application_id  = $app
  AND r.namespace_id    = $ns
  AND r.vector_space_id = $space
  [AND <filter predicates in order>]
ORDER BY distance ASC
LIMIT $limit;
```

- The query vector `$q` is encoded as `vector` text (unit 02 codec),
  with the resolved space's dimensionality (dimension pre-checked).
- **Metric operator selection** from the resolved space's
  `distance_metric`:

  | metric | expression | raw distance meaning |
  |--------|------------|----------------------|
  | `cosine` | `r.embedding::vector(D) <=> $q` | cosine distance, `0..2`, lower = better |
  | `l2` | `r.embedding::vector(D) <-> $q` | L2 (Euclidean) distance, `0..∞`, lower = better |
  | `inner_product` | `r.embedding::vector(D) <#> $q` | negative inner product, lower = better |

  All three are "lower = better", so `ORDER BY distance ASC` is uniform.
  The operator is selected in Go from the resolved `distance_metric`
  value (a server-side enum, not caller input); the cast carries the
  dimensionality because the stored column is an unconstrained `vector`.
- **Dimension cast `D`.** `D` is the resolved space row's `dimensions`
  integer. It is interpolated into the statement as a plain decimal
  integer — it is not caller input (Global Invariant 11: every
  *caller-controlled* value is a bound parameter; the only interpolated
  fragments are the Go-selected operator symbol and this
  server-resolved integer). For the seeded space `D = 1024`, and the
  expression `r.embedding::vector(1024) <=> $q` is then
  **structurally identical** to the indexed expression
  `(embedding::vector(1024)) vector_cosine_ops` from migration `0003` —
  which is what makes the index usable for that space. A per-dimension
  cast is required because pgvector ANN indexes are defined on
  constrained `vector(D)` expressions, not on the bare column.
- **Index behavior:** for the seeded 1024-dim cosine space, the
  statement's ORDER BY expression and the partial predicate
  `vector_space_id = $space` (bound to the seeded space's UUID)
  structurally match migration `0003`'s partial HNSW index, so the
  planner can use it for the nearest-neighbor ordering. For any other
  space or metric — or if the index is absent — PostgreSQL falls back
  to an exact (sequential) scan. **No code path may assume the HNSW
  index exists**; the SQL is correct with or without it.
- **ANN semantics.** HNSW is approximate. Even without filters it may
  return fewer than `limit` rows when the approximate search finds
  fewer candidates; with metadata filters the filter predicates are
  applied to the ANN candidates, so a selective filter can yield fewer
  than `limit` rows (or omit a closer record that does not match). This
  is inherent to the released index design; the API makes no exactness
  promise, and the response is the best matching rows the plan found,
  capped at `limit`.
- **Score transformation** (implementation detail; must be a fixed
  function of the raw distance per metric; higher = better; documented in
  code):

  | metric | `score` |
  |--------|---------|
  | `cosine` | `1 - distance / 2` → `[0, 1]` (identical vectors = 1, orthogonal = 0.5, opposite = 0) |
  | `l2` | `1 / (1 + distance)` → `(0, 1]` (identical = 1) |
  | `inner_product` | `-distance` (the raw inner product; higher = better, unbounded) |

  These specific formulas are proposals; the only contract is the API's:
  normalized, higher = better, consistent per metric, never raw
  pgvector distance. **Review point.**
- `score` is computed in Go from the raw `distance` value (float) and
  serialized with full precision (no rounding beyond Go's JSON float
  encoding).

### Response

```json
{
  "matches": [
    {
      "record_id": "…uuid…",
      "object_id": "document-123",
      "projection_id": "chunk-0004",
      "score": 0.9123,
      "content_hash": "…64 lowercase hex…",
      "source_updated_at": "2026-01-01T12:00:00Z",
      "metadata": { "document_type": "note" }
    }
  ]
}
```

- Field set matches `docs/API.md` exactly. **No `embedding` field, ever.**
- `content_hash` is the stored 32 bytes rendered as lowercase hex (unit
  07 convention).
- `source_updated_at` omitted when `NULL`.
- No matches → 200 with `"matches": []` (empty array, not omitted).
- No pagination in v1; the `limit` cap is the only bound.

### Errors

| case | status / code |
|------|---------------|
| missing `vector_space` | 400 `missing_field` |
| `limit` out of `1..min(200, VEC_SEARCH_MAX_LIMIT)` or non-integer | 400 `invalid_limit` |
| bad filter language (any rule above) | 400 `invalid_filter` |
| `vector` not an array of numbers | 400 `invalid_vector` |
| non-finite vector element | 422 `non_finite_vector` |
| namespace missing / disabled / foreign | 404 `namespace_not_found` (uniform) |
| unknown vector space | 404 `vector_space_not_found` (C7 proposal) |
| disabled vector space | 422 `vector_space_unavailable` |
| retired-but-enabled vector space | allowed (read operation) |
| dimension mismatch | 422 `invalid_vector_dimensions` |
| database failure | 503 `unavailable` (pool exhaustion / connection failure) |
| anything unexpected | 500 `internal` (never raw SQLSTATE text to callers) |

### Logging

Structured, per Global Invariant 14: `request_id`, application UUID,
namespace UUID, operation `search`, vector-space key, `limit`, filter
count, `matches` count, duration, status. Never: the query vector,
metadata bodies, or filter values.

## Interfaces and boundaries

- `internal/vector/search.go` (or the unit 07 service package): filter
  model types, filter validation, filter-SQL builder, search service
  function. The HTTP handler stays thin.
- Consumes: `WithAppContext` + `ResolveNamespace` (unit 04); identity
  (unit 05); `vector` codec (unit 02); space resolution + vector
  validation (unit 07); strict JSON (unit 09a).
- Exposes: nothing beyond the endpoint. No new public contracts.
- The filter model is request-scoped and internal; it must not become a
  reusable query DSL for other packages (v1 has no other consumer).

## Invariants and correctness constraints

- **Namespace isolation**: the statement always carries
  `application_id = $app AND namespace_id = $ns`; forced RLS applies
  independently. Search never crosses namespaces and never reads another
  application's records, with or without filters (Global Invariant 2).
- **No caller-controlled SQL**: field names and values are bound
  parameters; the only dynamic SQL is Go-constructed predicate *structure*
  (`->> $f = $v` / `->> $f = ANY($vals)`) with placeholders (Global
  Invariant 11).
- **Bounded**: `limit` ≤ `min(200, VEC_SEARCH_MAX_LIMIT)`; filter count
  and `in`-list size bounded; body size bounded (unit 09a).
- **Score contract**: higher = better, consistent per metric, raw
  distances never exposed (API Score Semantics).
- **No embedding in responses**; no embedding generation or reranking
  (Global Invariant 6).
- **Two-layer enforcement** as in unit 07; the `vector_records_validate`
  trigger does not apply to SELECTs — read-path correctness rests on RLS
  + explicit predicates.
- **Deterministic ordering ties**: two records with equal distance may be
  returned in any order; the API does not promise tie-breaking.

## Expected implementation surface

```text
internal/vector/search.go       # filter types, validation, SQL builder, service fn
internal/vector/search_test.go  # table-driven validation + builder tests
```

(Or equivalents within the unit 07 service package.)

## Validation

- Unit tests (no database), table-driven:
  - `limit`: absent (default), 0, 201, `VEC_SEARCH_MAX_LIMIT + 1`,
    fractional, string, negative → `invalid_limit`; boundaries 1 and
    `min(200, configured)` accepted.
  - filter grammar: empty object; `all: []`; `eq` with non-string value;
    `in` with empty values / non-string element / `VEC_MAX_FILTER_VALUES
    + 1` values; `VEC_MAX_FILTERS + 1` entries; unknown top-level filter
    key (`any`); unknown entry key; missing `field`; empty `field`;
    `field` > 128 chars; `op` of `gt`/`neq`/anything else; both `value`
    and `values` present; `eq` with `values`.
  - filter SQL builder: given a valid filter set, assert the produced
    placeholder sequence and parameter order (no literals), predicate
    order matches request order.
  - search statement builder: for each of the three metrics, assert the
    produced statement contains exactly `r.embedding::vector(D) op $q`
    with the metric's operator (`<=>` for cosine, `<->` for l2, `<#>` for
    inner_product), `D` rendered as the server-resolved dimension
    integer (not a placeholder, not caller input), and the full
    placeholder sequence for all other parameters (no caller literals —
    Global Invariant 11).
  - score transforms: table over the three metrics at boundary distances
    (cosine 0/1/2; l2 0/1/1000; inner_product negative/zero/positive
    raw values) — higher raw-better ⇒ higher score, monotone.
- Integration tests (real PostgreSQL + pgvector, unit 10 infrastructure),
  using the seeded 1024-dim cosine space with deterministic synthetic
  vectors (e.g. unit vectors per test record so nearest-neighbor results
  are exact):
  - search with no filters returns the inserted closest record first;
    `limit` respected; empty result → `{"matches": []}`.
  - `eq` filter returns only matching records; `in` filter returns the
    union; combined `all` returns the intersection; record missing the
    filtered key is excluded.
  - **isolation (spot checks; full matrix in unit 10):** search in
    namespace X of app A with a query vector equal to app B's stored
    record in another namespace of app A returns only app-A/namespace-X
    rows; a filter crafted to match only app B's metadata still returns
    nothing of B's.
  - **index usage:** an `EXPLAIN` of a seeded-space search shows an
    index scan on `vector_records_qwen3_06b_1024_cosine_hnsw` (the
    ORDER BY expression `embedding::vector(1024) <=> …` and the partial
    predicate on the seeded space UUID structurally match migration
    `0003`); a search with an `eq` filter still uses the index (HNSW
    applies the filter to ANN candidates) and returns ≤ `limit` rows;
    a search in a non-seeded test-fixture space falls back to a
    sequential scan and still returns the exact nearest-neighbor
    ordering.
  - search with a 1023/1025-dim vector → 422
    `invalid_vector_dimensions`; unknown space → 404; disabled space
    (test fixture UPDATE) → 422; retired-but-enabled space → search
    succeeds.
  - response contains no `embedding` field; `content_hash` lowercase hex;
    `source_updated_at` omitted when NULL.
  - score sanity: the exact-match record (query = stored vector) scores
    strictly higher than a less-similar record.

## Out of scope

- Nested boolean filter expressions, other operators (`gt`, `range`,
  `exists`, …), and non-top-level (dotted/nested) metadata keys —
  explicitly deferred by `docs/API.md` until a demonstrated requirement
  exists.
- Pagination, cursors, and result offsets.
- Reranking, hybrid search, full-text search, or any embedding
  generation.
- Record listing endpoints (none in API v1).
- Changing migration `0003` or adding indexes — the partial HNSW index
  is the released contract for the seeded space; new spaces/indexes enter
  only through new migrations.

## Open issues

- **Score transformation formulas** are proposals; only the API contract
  (higher = better, consistent per metric, non-raw) is pinned. Review
  point.
- **Default `limit` = 20** is a proposal (API example value, not
  pinned). Review point.
- **Filter field grammar (non-empty, ≤ 128 chars)** is a defensive
  proposal; the API does not pin a field grammar. Review point.
- **C7** (unknown space → 404, disabled/retired → 422, with the
  read-operation retirement split) is inherited from unit 07; not
  re-litigated here.
