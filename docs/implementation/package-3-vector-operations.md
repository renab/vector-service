# Package 3 — Vector Operations

## Objective

Deliver every data-plane vector operation as a service function that
runs inside the package-2 app-context transaction: upsert, get record,
delete projection, delete object, and nearest-neighbor search with the
v1 metadata filter language — including vector validation and
vector-space resolution. Each operation is exposed as a handler value of
the package-4 handler contract.

## Authority

- `docs/API.md` — the five data-plane endpoints, request/response
  shapes, record identity, upsert behavior ("A record whose current
  representation is already identical may be treated as unchanged"),
  score semantics (normalized, higher = better, consistent per metric,
  never raw distance), the constrained v1 filter language
  (`all` + `eq`/`in`, top-level keys, all filters must match).
- `docs/DATA_MODEL.md` — `vector_records` columns, 5-tuple identity,
  `content_hash` as 32 raw bytes, `metadata` as a JSON object,
  unconstrained `vector` column.
- `docs/SECURITY.md` — no embedding generation or reranking; caller
  metadata is caller data, never an authorization input; no embeddings
  in responses.
- `migrations/0001_shared_vector_schema.sql` — the
  `vector_records_validate` trigger (unqualified `RAISE EXCEPTION`,
  i.e. SQLSTATE `P0001`): rejects unknown space, **disabled** space,
  and dimension mismatch. The trigger does **not** check `retired_at`.
  Forced RLS on `vector_records`.
- `migrations/0002_qwen3_embedding_space.sql` — the seeded space
  (key `qwen3-embedding-0.6b-1024-cosine-v1`, UUID
  `0199f31e-1000-7000-8000-000000000001`, 1024 dimensions, cosine).
- `migrations/0003_qwen3_hnsq.sql` — the only index: a **partial** HNSW
  index on `embedding::vector(1024)` with `vector_cosine_ops`,
  restricted to the seeded space's UUID. No code path may assume this
  index exists; it applies to that space and metric only.
- Package 2 — `WithAppContext`, namespace resolution, helpers.
- Overview — invariants 5, 6, 8, 9; error catalog and classifier table.

## Scope

### 1. Vector-space resolution (shared)

Resolves a `vector_space` key from `vector_spaces` **inside the
request transaction** (the runtime role holds only `SELECT` on
`vector_spaces`, per migration `0004`). Outcomes:

- Unknown key → 404 `vector_space_not_found`.
- **Disabled** space → 422 `vector_space_unavailable` for every
  operation that names the space (upsert, search, projection delete,
  object delete with `?vector_space=`).
- **Retired** space → 422 `vector_space_unavailable` for **upsert
  only**. Get, search, and delete on a retired space are allowed —
  matching the retire-then-purge flow in `docs/MIGRATIONS.md` and the
  trigger, which blocks writes on disabled (not retired) spaces.
- Record-level operations that do not name a space (get by record ID,
  object delete without the parameter) are not gated on space state at
  all: records in disabled or retired spaces remain retrievable and
  deletable by ID.

**No row locking.** The write-authorization rule is grant-compatible
(read-then-write inside the request transaction). Bounded concurrent
behavior, stated explicitly: if a space is retired or disabled and that
change commits after the service read the space row but before the
request commits, the in-flight write is **not** rejected — the worst
case is one write landing in a space that was active at read time; all
subsequent writes are rejected. No privilege changes, no new
migrations, no locking clause.

### 2. Vector validation (shared)

For every `vector` field, before any database work:

- must be a JSON array of numbers → else 400 `invalid_vector`;
- every element must be finite (no NaN, no ±Infinity) → else 422
  `non_finite_vector`;
- every element must be representable as `float32` (values are sent to
  the database in pgvector text form, which is single-precision);
- length must equal the **resolved** space's `dimensions` → else 422
  `invalid_vector_dimensions` (the dimension check runs after space
  resolution because it needs the resolved dimensionality);
- **zero norm — cosine spaces only (after resolution):** if the
  resolved space's `distance_metric` is `cosine`, a vector whose
  elements are **all exactly `0.0`** (the deterministic zero-norm
  test: every element zero after float32 conversion — no epsilon, no
  norm computation) is rejected with 422 `non_finite_vector` — the
  existing vector-validation code and status class, returned in Go
  **before any SQL runs**, for both the upsert records' vectors and
  the search query vector. Rationale: cosine similarity is undefined
  at zero norm, and pgvector's `<=>` produces a non-finite distance
  against a zero vector (section 7, finite-scores rule). `l2` and
  `inner_product` spaces are **unchanged**: a zero vector is a valid,
  accepted vector in those spaces, and no zero-norm check applies
  there.

The `vector_records_validate` trigger re-enforces unknown-space,
disabled-space, and dimension violations at write time (defense in
depth, overview invariant via the classifier's `P0001` rows). Its
rejections map to the same catalog codes the service validation emits.

### 3. Upsert records — `PUT /v1/namespaces/{namespace}/records`

**Request validation** (cheap-first, before any database work):

- `vector_space`: present, non-empty → else 400 `missing_field`.
- `records`: present, non-empty array → 400 `missing_field`; count ≤
  `VEC_UPSERT_MAX_RECORDS` → else 413 `batch_too_large`.
- Per record: non-empty `object_id` and `projection_id`; `content_hash`
  exactly 64 lowercase hex characters → 400 `invalid_content_hash`;
  `metadata` a JSON object ≤ `VEC_MAX_METADATA_BYTES` when serialized →
  400 `metadata_not_object` / `metadata_too_large`; `source_updated_at`
  a valid RFC 3339 timestamp when present → 400 `invalid_timestamp`;
  vector per section 2 (structural checks now; dimension and
  zero-norm checks after space resolution).
- Duplicate `(object_id, projection_id)` within the batch → 400
  `duplicate_record`, rejected in Go before any write.

**Execution**, all inside one `WithAppContext` transaction (namespace
resolution first, then space resolution and its state gate):

- One statement per record, all in the same transaction:

  ```sql
  INSERT INTO vector_data.vector_records
      (id, application_id, namespace_id, object_id, projection_id,
       vector_space_id, content_hash, source_updated_at, metadata,
       embedding)
  VALUES ($id, $app, $ns, $object, $projection, $space, $hash,
          $updated, $metadata, $vector)
  ON CONFLICT (application_id, namespace_id, object_id, projection_id,
               vector_space_id)
  DO UPDATE SET
      content_hash        = EXCLUDED.content_hash,
      source_updated_at   = EXCLUDED.source_updated_at,
      metadata            = EXCLUDED.metadata,
      embedding           = EXCLUDED.embedding,
      updated_at          = now();
  ```

  (`$id` is a service-generated UUID — see below; `content_hash` is the
  32 raw bytes decoded from the hex string; `embedding` is bound through
  the package-1 text codec; the 5-tuple conflict target is the schema's
  unique identity.)

- **Service-owned record UUID:** the schema defines no default on
  `vector_records.id`; the service generates one UUID (UUIDv4 from
  `crypto/rand`) per new record in the batch and binds it as `$id`.
  `DO UPDATE SET` never touches `id`, so an existing record's UUID is
  preserved and the generated value is simply unused on conflict.
  Record identity is stable across re-upserts of the same 5-tuple.
- **Atomicity:** the batch is all-or-nothing — any trigger rejection or
  constraint failure rolls back the entire batch; API v1 defines no
  partial success.
- **Response:** `{"upserted": N, "unchanged": 0}` where `N` is the
  number of records processed. The API's "may be treated as unchanged"
  is permissive: the service counts processed records as upserted and
  does not compute a pre-state comparison. Idempotency is a separate
  tested property (an identical re-upsert yields the same data
  representation; `updated_at` is not part of the idempotence
  contract), not a response-field guarantee.

### 4. Get record — `GET /v1/namespaces/{namespace}/records/{record_id}`

- `record_id` path grammar: valid UUID → else 400 `invalid_uuid`.
- Inside `WithAppContext`: namespace resolution, then
  `SELECT` by record UUID with the namespace predicate
  (`application_id`, `namespace_id`) — the record row carries its
  `namespace_id`. No row → 404 `record_not_found` (uniform: covers
  missing, foreign-namespace, and cross-application equally).
- Response per `docs/API.md`: `record_id`, `object_id`,
  `projection_id`, `vector_space` (the space **key**, joined),
  `content_hash` (lowercase hex), `source_updated_at` (omitted when
  NULL), `metadata`, `created_at`, `updated_at`. **No `embedding`
  field, ever.**

### 5. Delete projection — `DELETE /v1/namespaces/{namespace}/records`

- Body: `object_id`, `projection_id`, `vector_space` (all present).
  Space resolution + state gate as in section 1 (a named disabled
  space → 422; retired → allowed).
- `DELETE FROM vector_data.vector_records WHERE application_id = $app
  AND namespace_id = $ns AND object_id = $o AND projection_id = $p AND
  vector_space_id = $space` → `{"deleted": n}`. Repeated deletion is
  idempotent: `{"deleted": 0}`.

### 6. Delete object — `DELETE /v1/namespaces/{namespace}/objects/{object_id}`

- Optional `?vector_space=` parameter. When present: space resolution +
  state gate, and the delete is restricted to that space. When absent:
  delete all of the object's projections in the namespace; no space
  gating.
- `DELETE FROM vector_data.vector_records WHERE application_id = $app
  AND namespace_id = $ns AND object_id = $o [AND vector_space_id =
  $space]` → `{"deleted": n}`. Idempotent (`{"deleted": 0}`). Never
  crosses the namespace.

### 7. Search — `POST /v1/namespaces/{namespace}/search`

**Request validation** (cheap-first):

- `vector_space`: present → else 400 `missing_field`.
- `limit`: optional integer in `1..min(200, VEC_SEARCH_MAX_LIMIT)` →
  else 400 `invalid_limit` (non-integer, fractional, or out of range).
  Default when omitted: **20** (proposal; the API pins the 1..200 range
  and the configurable lower maximum, not the default).
- `vector`: shared validation (section 2).
- `filters`: the v1 filter language (below) → any violation 400
  `invalid_filter`.

**Filter language (v1), exactly:**

```json
{}                          // no filters
{ "all": [] }               // no predicates
{ "all": [
    { "field": "document_type", "op": "eq", "value": "note" },
    { "field": "state", "op": "in", "values": ["active", "current"] }
] }
```

Rules (every violation → 400 `invalid_filter`):

- `filters` is a JSON object with at most one key, `all`. Any other key
  (`any`, `not`, …) is an unsupported v1 construct.
- `all` is a JSON array of at most `VEC_MAX_FILTERS` entries.
- Each entry is a JSON object with exactly: `field` (non-empty string,
  ≤ 128 chars — a defensive bound, not an injection control), `op`
  exactly `"eq"` or `"in"`, and for `eq` exactly `value` present
  (a JSON string); for `in` exactly `values` present (non-empty string
  array, ≤ `VEC_MAX_FILTER_VALUES`), `value` absent. Unknown or missing
  keys → `invalid_filter`.
- Duplicate filters on the same field are allowed (they AND together;
  an empty result is legitimate).

**Semantics:** all entries in `all` must match. A record whose
`metadata` lacks a filtered key does not match (`metadata ->> $field`
is NULL for a missing key; `NULL = …` / `NULL = ANY(…)` is not true).

**Filter SQL:** built in Go; every field name and value is a bound
parameter; no SQL fragment is ever interpolated from caller input:

```sql
-- eq:
AND r.metadata ->> $f = $v
-- in:
AND r.metadata ->> $f = ANY($vals)
```

**Statement** (inside `WithAppContext`, after namespace + space
resolution; space state gate: disabled → 422, retired → allowed):

```sql
SELECT r.id, r.object_id, r.projection_id, r.content_hash,
       r.source_updated_at, r.metadata,
       r.embedding::vector(D) op $q AS distance
FROM vector_data.vector_records r
WHERE r.application_id  = $app
  AND r.namespace_id    = $ns
  AND r.vector_space_id = $space
  [AND <filter predicates, in request order>]
ORDER BY distance ASC
LIMIT $limit;
```

- `op` is selected in Go from the resolved space's `distance_metric`:
  cosine `<=>`, l2 `<->`, `inner_product` `<#>`. All three are
  lower-is-better raw distances, so `ORDER BY distance ASC` is uniform.
- `$q` is bound through the text codec at the resolved dimensionality
  (pre-checked). `D` is the resolved space's `dimensions` — a
  server-side integer interpolated as a plain decimal, never caller
  input; the `::vector(D)` cast carries the dimensionality because the
  stored column is unconstrained and pgvector ANN indexes are defined
  on the constrained expression.
- **Index behavior:** for the seeded 1024-dim cosine space, the ORDER
  BY expression `embedding::vector(1024) <=> …` and the partial
  predicate on the seeded space UUID structurally match migration
  `0003`'s partial HNSW index, so the planner may use it. For any other
  space or metric — or if the index is absent — PostgreSQL falls back
  to an exact scan. **No code path may assume the index exists.**
- **ANN semantics:** HNSW is approximate; results may contain fewer
  than `limit` rows, and filters apply to ANN candidates. The API makes
  no exactness promise; the response is the best matching rows the plan
  found, capped at `limit`.

**Score:** computed in Go from the raw distance, normalized, higher =
better, consistent per metric, never the raw pgvector distance
(implementation detail; only the API contract is pinned). Proposed
transforms: cosine `1 - distance/2` in `[0,1]`; l2 `1/(1+distance)`;
inner product `-distance`. Full-precision JSON float encoding.

**Finite scores only (hard rule).** No NaN or ±Infinity may ever be
rendered as a score. For accepted inputs this holds by construction:
zero-norm query vectors are rejected in cosine spaces (section 2),
every element is finite, the cosine distance lies in `[0,2]` (score in
`[0,1]`), and the l2 distance is `>= 0` (score in `(0,1]`). Two
residual sources of a non-finite score remain possible — a **stored**
vector written outside the service (a zero vector in a cosine space
makes `<=>` non-finite; the service's own upsert now rejects that, so
only pre-existing or out-of-band rows can do this), and
**single-precision overflow** of the inner-product distance for
extreme finite inputs. Deterministic handling: a match whose computed
score is non-finite is **excluded from `matches`** (warn-logged,
`result_count` reflects the rows actually returned) — never rendered,
never aborting the search — and the remaining matches keep their
order. The response JSON therefore contains only finite numbers.

**Response:** `{"matches": [ … ]}` — empty array, not omitted; no
pagination. Per match: `record_id`, `object_id`, `projection_id`,
`score`, `content_hash` (lowercase hex), `source_updated_at` (omitted
when NULL), `metadata`. **No `embedding` field, ever.**

### 8. Handler surface

Each operation is a handler value of the package-4 contract
(`func(ctx context.Context, w http.ResponseWriter, in *HandlerInput)
*apierr.Error`; the exact contract types are defined in package 4 and
imported here, never re-declared). Handlers decode their own request body at the head
(package-4 strict-JSON helper), validate, resolve (namespace via
package 2, space via section 1), call the service function, and either
write the success response or return the classified typed error
(exactly once, via the package-4 classifier).

## Invariants and correctness constraints

- Every statement carries `application_id = $app AND namespace_id =
  $ns`; forced RLS applies independently (invariant 2). Search never
  crosses namespaces and never reads another application's rows, with
  or without filters.
- No caller-controlled SQL (invariant 8): field names, values, IDs,
  keys, vectors are all bound parameters; the only dynamic SQL is
  Go-constructed predicate structure with placeholders, the Go-selected
  metric operator, and the server-resolved dimension integer.
- All inputs bounded (invariant 9): batch size, metadata size, limit,
  filter count/values.
- Record identity is the 5-tuple; the service owns the record UUID —
  the schema has no default on `vector_records.id`, the service
  generates and binds it, and conflict updates preserve the existing
  UUID.
- Upsert is atomic per batch; deletes are idempotent; get/search never
  return the embedding (invariant 6); no embedding generation or
  reranking anywhere.
- Vector-space rows are never written by the service (invariant 5);
  the space-state rule of section 1 is the only space-state logic.
- Zero-norm rule: an all-`0.0` vector in a `cosine` space is rejected
  with 422 `non_finite_vector` before any SQL — for the upsert
  vectors and the search query vector, both operations; `l2` and
  `inner_product` spaces accept zero vectors, unchanged. A non-finite
  score is never rendered; such matches are excluded (section 7).

## Validation

**Unit (no database), table-driven:**

- Vector validation: non-array, mixed types, NaN/Infinity (422), empty
  array, wrong dimensions (against a resolved dimension), float32
  non-representable values; zero-norm rule — an all-zero vector in a
  resolved `cosine` space → 422 `non_finite_vector` (before any SQL),
  the same all-zero vector in a resolved `l2` / `inner_product` space
  → accepted, and a single non-zero element in any position defeats
  the rule (both query and upsert vectors).
- Upsert request validation: missing `vector_space`, empty `records`,
  batch over the limit (413), bad `content_hash` (wrong length,
  uppercase, non-hex), metadata not an object / over size, bad
  `source_updated_at`, duplicate `(object_id, projection_id)`.
- Filter grammar: the full violation table (unknown top-level key,
  non-array `all`, too many entries, missing/empty/overlong `field`,
  wrong `op`, `eq` with `values`, `in` with `value` / empty / non-string
  / too many values, unknown entry keys).
- Filter + search SQL builders: for valid inputs, assert the placeholder
  sequence, parameter order, predicate order = request order, and that
  no caller literal appears anywhere; per metric, the statement contains
  exactly `r.embedding::vector(D) op $q` with the correct operator and
  `D` rendered as the resolved integer.
- Score transforms: per-metric boundary table (cosine distances 0/1/2;
  l2 0/1/1000; inner-product negative/zero/positive) — monotone,
  higher-raw-better ⇒ higher score — plus NaN / ±Inf input distances:
  the transform reports the result non-finite, which the search path
  turns into match exclusion (never a rendered score).
- Upsert statement: `id` bound per record (never in `DO UPDATE SET`),
  5-tuple conflict target, `DO UPDATE SET` field set, one statement per
  record.
- Space-state gate table: unknown → 404; disabled → 422 (upsert/search/
  named deletes); retired → 422 upsert, allowed get/search/delete.

**Integration (real PostgreSQL + pgvector, 5a harness; deterministic
synthetic unit vectors so nearest-neighbor results are exact):**

- Upsert: insert → get shows the record; re-upsert with different data
  → updated; identical re-upsert → data representation unchanged
  (idempotence test, separate from the `upserted` count); response is
  `{"upserted": N, "unchanged": 0}`; a batch with one invalid record →
  400/422 and **no** record from the batch is written (all-or-nothing).
- **Service-owned UUID acceptance:** a fresh insert yields a record
  whose `record_id` is a well-formed, service-generated UUID (distinct
  per record in the same batch; retrievable via `GET …/records/{record_id}`);
  a re-upsert of the same 5-tuple with different data preserves the
  original `record_id` (direct DB assertion that `id` is unchanged even
  though a new UUID was generated and bound).
- Trigger defense in depth: force a condition the service validation
  cannot catch (e.g. write a mismatched-dimension vector via a direct
  service call that bypasses the length check) → `P0001` rejection →
  mapped to 422 `invalid_vector_dimensions`, batch rolled back.
- Search: nearest record first; `limit` respected; empty →
  `{"matches": []}`; `eq` returns only matches; `in` returns the union;
  combined `all` is the intersection; records missing the filtered key
  are excluded; response has no `embedding` field; `content_hash` is
  lowercase hex; NULL `source_updated_at` omitted; exact-match query
  scores strictly higher than a less-similar record.
- **Zero-norm cosine vectors (real pgvector):** upserting an
  all-zero 1024-dim vector into the seeded cosine space → 422
  `non_finite_vector`, and with the zero vector inside a two-record
  batch the **whole** batch is rejected (no record of the batch is
  written — all-or-nothing); a search with an all-zero query vector in
  the seeded space (namespace holding valid records) → 422
  `non_finite_vector`, no matches. In the `l2` / `inner_product`
  fixture spaces (direct-DB fixture space rows, as in the space-state
  matrix) the same all-zero vector upserts successfully (200) and a
  zero-query search returns normally with finite scores —
  `l2`/`inner_product` behavior is unchanged by the rule.
- **No NaN/Inf scores (real pgvector):** in a cosine fixture space
  (direct-DB, unindexed — exact scan), insert via the bootstrap
  identity a degenerate all-zero stored record **plus** valid records
  (bypassing the service, which would reject it); a search with a
  valid non-zero query vector and a `limit` large enough to fetch
  every row returns exactly the valid records with finite scores — the
  degenerate row's non-finite distance/score is excluded from
  `matches`, the response body contains no NaN or Inf value (assert
  every `score` is finite and the wire body parses to finite JSON
  numbers), and the warn log records the exclusion.
- **Index usage:** `EXPLAIN` of a seeded-space search shows an index
  scan on the `0003` partial HNSW index (with and without an `eq`
  filter, ≤ `limit` rows); a search in a non-seeded fixture space
  falls back to an exact scan and still returns the exact
  nearest-neighbor order.
- Space-state matrix through the API: unknown → 404; disabled (test
  fixture UPDATE) → 422 on upsert/search/named delete, get-by-ID
  still 200; retired → upsert 422, get/search/delete 200.
- Get: foreign-namespace record UUID → 404 `record_not_found`;
  malformed `record_id` → 400 `invalid_uuid`.
- Deletes: projection delete of an absent projection → `{"deleted": 0}`;
  object delete across the namespace boundary (same object ID, another
  namespace) → `{"deleted": 0}` and the other namespace's rows intact
  (direct DB assertion); `?vector_space=` restricts to the space.
- Isolation spot checks (full matrix in 5b): app A searching its own
  namespace with a query equal to app B's stored vector returns only
  A's rows; a filter matching only B's metadata returns nothing of B's.

## Out of scope

- Record listing or any other retrieval endpoint (not in API v1).
- Pagination, cursors, result offsets.
- Nested boolean filter expressions, operators beyond `eq`/`in`,
  non-top-level metadata keys (deferred by `docs/API.md`).
- Reranking, hybrid search, full-text search, embedding generation.
- Vector-space management endpoints — `GET /v1/vector-spaces` is **not**
  implemented (overview, Unresolved C13); the path 404s.
- Creating indexes or modifying migrations `0001`–`0003`.

## Open issues

- Default `limit` = 20 is a proposal (the API pins the range and the
  configurable maximum, not the default).
- The score-transform formulas are proposals; the API contract (higher
  = better, consistent per metric, non-raw) is what is pinned.
- The 128-char field-name bound in the filter grammar is a defensive
  proposal; the API does not pin a field grammar.
