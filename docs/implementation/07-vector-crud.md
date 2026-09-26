# Unit 07 — Vector Record CRUD: Upsert, Get, Delete, and Vector-Space Listing

## Objective

Implement the data-plane record operations defined by `docs/API.md`:

```text
PUT    /v1/namespaces/{namespace}/records            # batch upsert
GET    /v1/namespaces/{namespace}/records/{record_id}
DELETE /v1/namespaces/{namespace}/records            # delete one logical projection
DELETE /v1/namespaces/{namespace}/objects/{object_id}[?vector_space=…]
GET    /v1/vector-spaces                             # list active vector spaces
```

All operations run inside the unit 05/04 pipeline (authenticate →
`WithAppContext` → namespace resolution → business SQL in one
transaction). This unit owns vector-space key resolution and all
request-side vector validation. Search is unit 08.

## Authority

- `docs/API.md` — endpoint shapes; record identity 5-tuple; upsert
  identity (namespace, object_id, projection_id, vector_space) within the
  authenticated application; `records` non-empty and bounded; one vector
  space per request; per-record required fields; vector finiteness and
  dimension rules; upsert response `{"upserted": N, "unchanged": M}`;
  get returns no embedding; projection delete response `{"deleted": N}`
  with idempotent repeat; object delete with optional vector-space
  filter, never crossing namespaces, `{"deleted": N}`; vector-spaces list
  response shape; "retired or disabled vector spaces may be omitted from
  normal responses"; error status examples (400/404/409/413/422/503).
- `docs/SECURITY.md` — no embedding generation; validation of
  dimensions/finiteness; uniform namespace 404s; caller metadata is not
  an identity input.
- `docs/DATA_MODEL.md` — logical record identity; internal service-owned
  UUID; `content_hash` as 32-byte digest (hex in API, lowercase
  convention); `source_updated_at` informational; metadata is a JSON
  object, caller data, never an identity/authorization input; deletion
  semantics (projection vs object, optional space selector, no namespace
  crossing); retirement semantics (no new writes; reads may remain);
  "matching object, projection, vector space, content hash may be
  treated as unchanged."
- `migrations/0001_shared_vector_schema.sql` — `vector_records` columns;
  `UNIQUE (application_id, namespace_id, object_id, projection_id,
  vector_space_id)`; `octet_length(content_hash) = 32` CHECK; metadata
  object CHECK; non-empty object/projection CHECKs; composite FK to
  `namespaces(id, application_id)`; `vector_records_validate` BEFORE
  trigger re-enforcing space dimensions and enabled state (Global
  Invariant 7) — note the trigger does **not** check `retired_at`;
  retirement enforcement on the write path is service-side (see
  "Vector-space state lock"); the trigger raises via **unqualified**
  `RAISE EXCEPTION`, so every rejection it performs carries SQLSTATE
  `P0001` (see "Trigger rejection mapping (defense in depth)"); forced
  RLS on `vector_records`.
- `migrations/0002_qwen3_embedding_space.sql` — the one seeded space:
  key `qwen3-embedding-0.6b-1024-cosine-v1`, UUID
  `0199f31e-1000-7000-8000-000000000001`, 1024 dimensions, cosine.
- `migrations/0004_runtime_identity.sql` — `vector_api` holds `SELECT`
  only on `vector_control.vector_spaces` (no `UPDATE`/`DELETE`); space
  state changes reach the system only through released migrations and
  administrative SQL, never through a data-plane or admin API.
- `docs/implementation/04-isolation-core.md` — transaction pipeline,
  namespace resolution, vector-spaces read access (no RLS on
  `vector_spaces`, `SELECT` grant).
- `docs/implementation/05-authentication.md` — authenticated identity.
- `docs/implementation/README.md` — error catalog; Conflicts C7, C11;
  Global Invariants 1–7, 11–14; content-hash convention (64 hex,
  normalized lowercase, stored raw 32 bytes).

## Dependencies

- Units 01–05 (apierr, pool, `WithAppContext`, namespace resolution,
  authentication).
- Unit 02 (pgx `vector` text-format codec for encoding/decoding
  `[]float32` against the `[a,b,c]` representation).
- Unit 09a (strict JSON, body bounds) — consumed at build time; units
  07/08 share the vector-space resolution and vector validation helpers,
  with unit 07 owning them.

## Scope

### Shared: vector-space resolution

One helper used by upsert, search (unit 08), and deletes:

```sql
SELECT id, dimensions, distance_metric, enabled, retired_at
FROM vector_control.vector_spaces
WHERE vector_space_key = $1;
```

- Executed inside the request transaction (no application context
  required — the table has no RLS — but it stays in the transaction for
  consistency, unit 04 item 5).
- Zero rows → 404 `vector_space_not_found` (**Conflict C7 proposal**;
  the API lists 404 examples for hidden resources and 400/422 examples,
  but none pins this case; unknown key is a "not found" resource).
- Row with `enabled = false` → 422 `vector_space_unavailable` for every
  operation (a disabled space performs no data-plane operation).
- Row with `retired_at IS NOT NULL` (still enabled):
  - upsert → 422 `vector_space_unavailable` ("no new normal writes
    should target it" — DATA_MODEL retirement);
  - get / projection delete / object delete / search → **allowed**
    (existing records remain readable and deletable during migration;
    MIGRATIONS.md's derived-data flow explicitly deletes old records
    after retiring a space).
- Returns the space row (UUID, dimensions, metric) for validation and
  SQL.

### Shared: request-side vector validation

Before any database work (DEVELOPMENT.md):

1. `vector` must be a JSON array of JSON numbers → else 400
   `invalid_vector`.
2. Every element must be finite. (RFC 8259 JSON cannot carry NaN/±Inf
   literals, so these are normally rejected at decode as
   `invalid_json`; the Go decode path must be implemented so that
   non-finite numeric input — including any custom number handling —
   yields the typed `non_finite_vector` (422) per the catalog, and the
   code must never emit a NaN/Inf value to the `vector` codec.)
3. Every element must be **representable as a finite `float32`** (the
   storage type). JSON numbers decode to `float64`; a finite float64
   whose magnitude exceeds the float32 range (≈ 3.4028235e+38, e.g.
   `1e300`) overflows the `float32` conversion to ±Inf. Per element:
   `f32 := float32(f64)`; if the conversion produced an infinite value
   (NaN cannot arise from finite input) → 400
   `invalid_vector_number` (catalog row: "not a parseable finite
   number **in the storage type**"). This is a caller error (the value
   as written cannot be stored), not a programming error.
4. `len(vector) == space.dimensions` → else 422
   `invalid_vector_dimensions`.
5. Values are stored/encoded as `float32`. The text codec (unit 02)
   must reject non-finite floats defensively; a failure there is a
   programming error (500), not a caller error, because steps 2 and 3
   already validated.

Dimension and enabled-state checks are re-enforced server-side by the
`0001` trigger (Global Invariant 7); the service-side checks exist for
good error codes and cheap rejection — neither layer may be removed. A
write that slips past the service checks and is rejected by the trigger
is classified per "Trigger rejection mapping (defense in depth)" below —
the same catalog codes the service emits for the same conditions, never
raw SQLSTATE or trigger message text.

### Upsert — `PUT /v1/namespaces/{namespace}/records`

Request body (strict decoding; unknown fields → 400 `unknown_field`):

```json
{
  "vector_space": "…",
  "records": [
    {
      "object_id": "document-123",
      "projection_id": "chunk-0001",
      "content_hash": "…64 hex…",
      "source_updated_at": "2026-01-01T12:00:00Z",
      "metadata": { "document_type": "note" },
      "vector": [ 0.0123, -0.456 ]
    }
  ]
}
```

Validation order (cheap first):

1. `vector_space` present, non-empty string → `missing_field`.
2. `records` present, non-empty array → `missing_field`;
   `len(records) > VEC_UPSERT_MAX_RECORDS` → 413 `batch_too_large`.
   One vector space per request (the single top-level field enforces
   this structurally).
3. Per record:
   - `object_id`, `projection_id`: non-empty strings → `missing_field`;
   - `content_hash`: exactly 64 hex characters (either case) → 400
     `invalid_content_hash`; normalize to lowercase; convert to raw 32
     bytes for storage;
   - `source_updated_at`: optional; when present, valid RFC 3339 → 400
     `invalid_timestamp`; omitted → `NULL`;
   - `metadata`: optional; default `{}`; JSON object → 400
     `metadata_not_object`; serialized size ≤ `VEC_MAX_METADATA_BYTES`
     → 400 `metadata_too_large`;
   - `vector`: shared vector validation above.
3b. **Batch identity uniqueness (pre-SQL):** the batch must not contain
   two records with the same (`object_id`, `projection_id`) pair →
    400 `duplicate_record` (README error catalog). This is a
    deterministic Go check performed before any SQL or transaction is
    opened; it makes the in-statement accounting exact (see Execution)
    and removes any ambiguity about which occurrence a self-conflict
    would have resolved.
4. Namespace resolution (unit 04) → 404 uniform `namespace_not_found`.
5. Vector-space resolution (above).
6. **Locked state re-check (upsert only):** immediately before the
   upsert statement, the upsert path locks the resolved space row and
   re-checks `enabled`/`retired_at` (see "Vector-space state lock"
   below). `enabled = false` or `retired_at IS NOT NULL` → 422
   `vector_space_unavailable` and the transaction rolls back with no
   write. Get / projection delete / object delete / search do **not**
   lock — the retirement split keeps them working after retirement.

**Execution** — one transaction: `WithAppContext(appID, fn)`. Inside
`fn`, after namespace + space resolution **and the upsert-only locked
state re-check**, process the batch in a single statement:

```sql
WITH input AS (
    -- One row per record, in request order. Each row carries the record's
    -- own service-generated UUID (v.id) plus its fields; v.seq is the
    -- 1-based request position (service-generated, not caller input).
    -- $1…$8 are record 1's parameters, $9…$16 record 2's, and so on;
    -- $app/$ns/$space denote the pipeline-established identifiers (every
    -- placeholder is a positional parameter in the Go code).
    SELECT
        v.seq,
        v.id,
        v.object_id,
        v.projection_id,
        v.content_hash,
        v.source_updated_at,
        v.metadata,
        v.embedding
    FROM (VALUES
        ($1::int, $2::uuid, $3::text, $4::text, $5::bytea,
         $6::timestamptz, $7::jsonb, $8::vector),
        ($9::int, $10::uuid, $11::text, $12::text, $13::bytea,
         $14::timestamptz, $15::jsonb, $16::vector)
        -- …one further row of 8 parameters per additional record…
    ) AS v(seq, id, object_id, projection_id, content_hash,
           source_updated_at, metadata, embedding)
),
pre AS (
    -- Pre-state: the stored hash for each batch identity *before this
    -- statement writes anything*. Within one statement every CTE sees
    -- the pre-statement state (a statement's own modifications are
    -- invisible to the other parts of the statement), so this is
    -- correct at every isolation level. One row per input row (keyed
    -- by seq); duplicate logical identities within a batch are rejected
    -- in Go before this statement runs (validation step 3b), so each
    -- (object_id, projection_id) pair appears at most once.
    SELECT
        i.seq,
        i.object_id,
        i.projection_id,
        r.content_hash AS pre_hash
    FROM input i
    LEFT JOIN vector_data.vector_records r
        ON  r.application_id  = $app
        AND r.namespace_id    = $ns
        AND r.vector_space_id = $space
        AND r.object_id       = i.object_id
        AND r.projection_id   = i.projection_id
),
upsert AS (
    INSERT INTO vector_data.vector_records
        (id, application_id, namespace_id, vector_space_id,
         object_id, projection_id, content_hash, source_updated_at,
         metadata, embedding)
    -- Each row inserts under its own service-generated UUID (i.id); the
    -- statement's modifications stay invisible to the pre CTE above, so
    -- accounting reflects pre-statement state.
    SELECT
        i.id, $app, $ns, $space,
        i.object_id, i.projection_id, i.content_hash, i.source_updated_at,
        i.metadata, i.embedding
    FROM input i
    ON CONFLICT (application_id, namespace_id, object_id, projection_id,
                 vector_space_id)
    DO UPDATE SET
        content_hash      = EXCLUDED.content_hash,
        source_updated_at = EXCLUDED.source_updated_at,
        metadata          = EXCLUDED.metadata,
        embedding         = EXCLUDED.embedding
)
SELECT
    count(*) FILTER (
        WHERE pre.pre_hash IS NULL
           OR pre.pre_hash <> input.content_hash
    ) AS upserted,
    count(*) FILTER (
        WHERE pre.pre_hash = input.content_hash
    ) AS unchanged
FROM input
JOIN pre USING (seq);
```

Implementation notes:

- The `UNIQUE` constraint is on the 5-tuple **with `vector_space_id`**
  (migration `0001`); `ON CONFLICT` must name exactly that column set.
- Record UUIDs are service-generated (unit 01 convention: `crypto/rand`
  UUID v4 in Go, passed as parameters — do not rely on
  `gen_random_uuid()` for identity generation, keeping the convention
  uniform and testable). Each `VALUES` row carries its record's own UUID;
  the statement never substitutes one UUID for the whole batch.
- All caller values are bound parameters (Global Invariant 11). For N
  records the statement has exactly 8N record parameters
  (seq, id, object_id, projection_id, content_hash, source_updated_at,
  metadata, embedding per row, in that order) plus the three
  pipeline identifiers; the `VALUES` rows are built in Go in request
  order. `source_updated_at` is a NULL parameter when the field is
  omitted; `content_hash` is the raw 32-byte digest.
- **`upserted`/`unchanged` accounting — one algorithm, computed
  pre-write.** The counts come from the upsert statement's own final
  `SELECT` (the `pre` CTE joined by `seq`), evaluated against the
  **pre-statement** state: within one statement a CTE cannot see the
  statement's own modifications, so the classification is correct at
  every isolation level. There is **no post-write recount** and no second
  accounting query; the statement's `upserted`/`unchanged` are the
  authoritative values. Rationale: the upsert `DO UPDATE` fires
  unconditionally on conflict (Postgres performs the row write even when
  the new values equal the old), so the only way to honor "a record whose
  current representation is already identical may be treated as
  unchanged" (API.md) is to compare each submitted record's
  `content_hash` against the stored hash captured *before* the write.
  Content-hash equality is the documented unchanged signal (DATA_MODEL:
  object + projection + space + content hash). Because duplicate logical
  identities are rejected pre-SQL (validation step 3b), the two filters
  are complementary per input row and the counts are always exact:
  `upserted + unchanged == len(records)`. The `upsert` CTE is a
  data-modifying CTE: PostgreSQL executes it exactly once even though
  the final `SELECT` references only `input` and `pre` — do not drop it
  or restructure the statement to reference it (that would let the
  write feed the accounting and break the pre-statement guarantee).
- If any record in the batch fails the DB (e.g., trigger rejection), the
  whole transaction rolls back and the request fails — batch
  all-or-nothing. The response status/code for a trigger rejection is
  defined by "Trigger rejection mapping (defense in depth)"; it does not
  change the all-or-nothing semantics. (Proposal consistent with
  single-transaction design; review point only if partial-batch semantics
  are ever demanded — the API defines no partial success.)
- Response 200:

  ```json
  { "upserted": 18, "unchanged": 42 }
  ```

- Logging: `request_id`, application UUID, namespace UUID, operation
  `upsert`, `vector_space` key, record count, `upserted`/`unchanged`,
  duration, status. Never vectors, never metadata bodies (Global
  Invariant 14).

### Vector-space state lock (upsert write path)

The request transaction runs at **READ COMMITTED** (unit 04). The
space-resolution SELECT (validation step 5) reads the row under a
statement snapshot. If a concurrent administrative/migration
transaction commits a disable (`enabled = false`) or a retirement
(`retired_at = now()`, `enabled` still `true`) after that read, the
resolution result is stale by the time of the write:

- **Disable** is caught as defense in depth by the `0001` trigger
  inside the write statement (under READ COMMITTED the trigger's own
  SELECT takes a fresh statement snapshot and sees the committed
  state).
- **Retirement is caught by nothing**: the trigger checks only
  `dimensions` and `enabled`, so a retired-but-still-enabled row
  passes it. Without a lock, an upsert can land in a space that was
  retired between the read and the write, violating "no new normal
  writes should target it" (DATA_MODEL retirement).

**Contract (upsert path only).** After resolution (step 5) and
immediately before the upsert statement — no other business SQL in
between — the upsert path executes exactly:

```sql
SELECT enabled, retired_at
FROM vector_control.vector_spaces
WHERE id = $space
FOR UPDATE;
```

and re-checks the locked row:

- `enabled = false` → 422 `vector_space_unavailable`;
- `retired_at IS NOT NULL` → 422 `vector_space_unavailable`;
- otherwise the space is active → proceed to the upsert statement.
  The row lock is held until COMMIT/ROLLBACK and covers the write
  statement.
- Zero rows is impossible (no released migration deletes a space and
  the runtime role holds no `DELETE` privilege); if ever observed,
  map it to `internal` (500) — not a 404.

**Serialization semantics (READ COMMITTED):**

- **Transition committed before the lock is acquired** (before, or
  between, the resolution read and the `FOR UPDATE`): `SELECT … FOR
  UPDATE` waits for a conflicting in-flight update to settle and then
  returns the **latest committed version** of the row, so the re-check
  sees the committed disable/retirement → 422 rejection, the
  transaction rolls back, and nothing is written.
- **Transition racing while the lock is held**: the transition's
  `UPDATE vector_spaces` blocks on the upsert transaction's row lock
  until that transaction commits. The write is already committed; the
  transition then applies and takes effect for subsequent operations.
  Neither side errors, no update is lost, and the transition is
  ordered **after** the write.

**Consequences and boundaries:**

- Because the lock is held across the write statement, no concurrent
  `UPDATE` of that row can commit between the re-check and the
  trigger's check inside the write statement: a disable is therefore
  rejected by **both** the re-check and the `0001` trigger, while a
  retirement is rejected **only** by this re-check (the trigger never
  sees `retired_at`).
- Lock scope is one `vector_spaces` row (primary-key lookup); other
  spaces are unaffected. Non-upsert operations take **no** such lock:
  get / delete / search must keep working on a retired space per the
  retirement split, and the MIGRATIONS.md retire-then-delete flow must
  not be blocked behind upsert locks.
- This uses the runtime role's **existing `SELECT` privilege** on
  `vector_control.vector_spaces` (migration `0004`; `SELECT … FOR
  UPDATE` requires no privilege beyond `SELECT`). The service exposes
  **no vector-space admin API** — state transitions arrive only
  through released migrations and administrative SQL, and the data
  plane must be correct against any of them racing a write.

### Trigger rejection mapping (defense in depth)

The `0001` trigger `vector_data.validate_vector_record` raises with
**unqualified** `RAISE EXCEPTION` (no explicit SQLSTATE), so every
rejection it performs reaches the service as SQLSTATE `P0001`
(`raise_exception`) with a message that interpolates the offending
values:

```text
unknown vector_space_id: <space UUID>
vector space is disabled: <space UUID>
embedding dimension mismatch: expected <n>, received <m>
```

Classification is deterministic and owned by the unit 09 SQLSTATE
classifier (`internal/api/errors.go`); the unit 07 service functions
propagate pgx errors from the write statement **unmodified** (no
wrapping, no re-mapping, no pre-emptive conversion to `apierr`):

| trigger message prefix (`0001`) | code | status |
|---------------------------------|------|--------|
| `unknown vector_space_id: ` | `vector_space_not_found` | 404 |
| `vector space is disabled: ` | `vector_space_unavailable` | 422 |
| `embedding dimension mismatch: ` | `invalid_vector_dimensions` | 422 |
| any other `P0001` | `internal` | 500 |

- The three prefixes are the trigger's own message formats verbatim;
  matching is prefix-based on the pgx `PgError` primary message, gated
  on `Code == "P0001"` (a non-`P0001` error is never mapped by these
  prefixes — the SQLSTATE gates first).
- The three codes are the **same** catalog codes the service's own
  resolution and validation emit for the same conditions, so the two
  enforcement layers (service checks + trigger, Global Invariant 7) are
  indistinguishable to the caller. No new error codes are introduced.
- **No leaking:** the response carries only the catalog code and its
  stable message — never the trigger message, the SQLSTATE, or the
  interpolated UUID/dimension values. Raw detail may appear only in the
  structured server log (`db_error` field), per unit 09.
- On the normal API path these branches are unreachable: space
  resolution, Go vector validation, and (for upsert) the locked state
  re-check reject the same conditions first. The mapping exists so that
  a rejection that does reach the trigger is classified deterministically
  instead of falling through to an opaque 500.

### Get record — `GET /v1/namespaces/{namespace}/records/{record_id}`

- `{record_id}` must parse as UUID → 400 `invalid_uuid`.
- Inside the request transaction, after namespace resolution:

  ```sql
  SELECT id, object_id, projection_id, vector_space_key, content_hash,
         source_updated_at, metadata, created_at, updated_at
  FROM vector_data.vector_records r
  JOIN vector_control.vector_spaces s ON s.id = r.vector_space_id
  WHERE r.id = $1
    AND r.application_id = $app
    AND r.namespace_id = $ns;
  ```

- Zero rows → 404 `record_not_found` (uniform for missing, foreign
  application, or other namespace — RLS plus the explicit predicates make
  all cases indistinguishable).
- Response 200 exactly the API fields: `record_id`, `object_id`,
  `projection_id`, `vector_space` (the **key**, not UUID), `content_hash`
  (lowercase hex of the stored bytes), `source_updated_at` (omitted when
  NULL), `metadata`, `created_at`, `updated_at` (RFC 3339). **No
  embedding** is ever returned by any endpoint.

### Delete projection — `DELETE /v1/namespaces/{namespace}/records`

Request body (strict): `{ "object_id": "…", "projection_id": "…",
"vector_space": "…" }` — all required → `missing_field`; vector-space
resolution applies (unknown → 404 `vector_space_not_found`; disabled →
422; retired → allowed).

```sql
DELETE FROM vector_data.vector_records
WHERE application_id = $app
  AND namespace_id = $ns
  AND object_id = $1
  AND projection_id = $2
  AND vector_space_id = $space
RETURNING id;
```

Response 200: `{"deleted": <rows affected>}`. Repeated deletion →
`{"deleted": 0}` (idempotent per API.md).

### Delete object — `DELETE /v1/namespaces/{namespace}/objects/{object_id}[?vector_space=…]`

- `{object_id}` path segment: non-empty, ≤ 512 chars (proposal; review
  point), URL-decoded. Empty → 400 `missing_field`.
- Optional `?vector_space=` query parameter: when present, resolve the
  space (same outcomes as above) and add `AND vector_space_id = $space`.
  When omitted, **no space predicate** — all of the object's projections
  in this namespace are deleted. There is no wildcard or "all spaces"
  magic string: absence of the parameter is the only way to express
  all-spaces, and it never crosses namespaces.
- Malformed or unknown space in the query parameter → same 404/422
  outcomes as the body form (proposal; review point: the API does not
  pin the error for a bad optional query parameter; reuse of the
  resolution outcomes is the consistent choice).

```sql
DELETE FROM vector_data.vector_records
WHERE application_id = $app
  AND namespace_id = $ns
  AND object_id = $1 [AND vector_space_id = $space]
RETURNING id;
```

Response 200: `{"deleted": <rows>}`. Deleting an object with no
projections → `{"deleted": 0}` (idempotent).

**Invariants:** deletion is namespace-scoped by the resolved `namespace_id`
predicate plus RLS; it never crosses namespaces (DATA_MODEL). Deleting
records deletes derived state only (the source object lives in the
consumer).

### List vector spaces — `GET /v1/vector-spaces`

- Data-plane authenticated (application credential), **not**
  admin. No namespace segment (the space catalog is global read data —
  the explicitly documented exception to the namespace-scoping rule,
  Conflict C13; it exposes no record data and introduces no
  cross-namespace retrieval).
- No application context required for the query itself, but the request
  still authenticates (unit 05) and the query may run in a short
  transaction:

  ```sql
  SELECT vector_space_key, embedding_model, embedding_model_version,
         dimensions, distance_metric
  FROM vector_control.vector_spaces
  WHERE enabled = true AND retired_at IS NULL
  ORDER BY vector_space_key;
  ```

- Only **active** spaces are listed: disabled and retired are omitted
  (API.md: "Retired or disabled vector spaces may be omitted from normal
  responses"; the endpoint returns spaces "currently available for
  normal operations", and retired spaces no longer accept writes).
- Response 200:

  ```json
  { "vector_spaces": [ { "key": "…", "embedding_model": "…",
    "embedding_model_version": "…", "dimensions": 1024,
    "distance_metric": "cosine" } ] }
  ```

  `distance_metric` is one of `cosine | l2 | inner_product` (the `0001`
  enum, serialized as its text name).

## Interfaces and boundaries

- `internal/vector` (or `internal/api` service layer — implementer's
  choice, one owner): space resolution, vector validation, the five
  handlers' service functions. HTTP handlers stay thin.
- Consumes: `WithAppContext` + `ResolveNamespace` (unit 04); identity
  from context (unit 05); `vector` codec (unit 02); strict JSON and
  DB-error classification (unit 09 SQLSTATE classifier — unit 07
  propagates pgx errors unmodified and never pre-maps them).
- Exposes to unit 08: space resolution + vector validation helpers (same
  semantics, shared code).
- Response field sets match `docs/API.md` exactly; embeddings are never
  serialized.

## Invariants and correctness constraints

- **Identity is the 5-tuple** (application, namespace, object_id,
  projection_id, vector_space); the service owns the record UUID; no
  request field can select application or namespace (Global Invariant
  1).
- **Two-layer enforcement**: every statement carries explicit
  `application_id`/`namespace_id` predicates and forced RLS applies
  independently. Neither layer may be removed.
- **Validation before database work** where practical (DEVELOPMENT.md);
  schema constraints and the `vector_records_validate` trigger remain the
  final enforcement (Global Invariant 7) — a request that slips past Go
  validation still fails at the DB, classified per "Trigger rejection
  mapping (defense in depth)" (the same catalog codes the service emits
  for the same conditions; never raw SQLSTATE or trigger message text in
  the response).
- **All-or-nothing batch**: the upsert batch commits atomically; there is
  no partial-success response in v1.
- **Upsert locks space state**: the upsert path re-reads the resolved
  `vector_spaces` row with `SELECT … FOR UPDATE` and re-checks
  `enabled`/`retired_at` immediately before the write statement; a
  committed disable/retirement is rejected (422, no write) and a
  racing transition serializes after the commit (Scope, "Vector-space
  state lock"). The `0001` trigger never checks `retired_at`, so
  retirement rejection on the write path depends on this re-check and
  may not be removed; get / delete / search take no such lock.
- **One accounting algorithm**: `upserted`/`unchanged` are computed by
  the upsert statement's own final `SELECT` against pre-statement state;
  there is no post-write recount. Duplicate logical identities are
  rejected pre-SQL (`duplicate_record`), so
  `upserted + unchanged == len(records)` always holds.
- **Idempotent deletes**; **idempotent upserts** by 5-tuple.
- **No embedding in any response**; no embedding generation (Global
  Invariant 6).
- **No caller-controlled SQL** (Global Invariant 11): every object_id,
  projection_id, hash, timestamp, metadata value, and vector element is
  a bound parameter; the `VALUES` list is constructed in Go.
- **Bounded**: batch size, metadata size, body size (unit 09);
  `object_id`/`projection_id` lengths bounded by body size implicitly,
  with an explicit per-field cap (proposal: 512 chars; review point).
- **Errors mapped** per the catalog; SQLSTATE `23505` on the 5-tuple
  cannot occur for the caller (upsert resolves it via `ON CONFLICT`);
  trigger rejections (unknown space / space disabled / dimensions)
  surface as the resolution's 404/422 before the write in the normal
  path, and a trigger rejection that does reach the HTTP layer is
  classified by the `P0001` mapping (Scope, "Trigger rejection mapping")
  — never by the generic `internal` catch-all.

## Expected implementation surface

```text
internal/vector/space.go      # space resolution
internal/vector/validate.go   # vector + content hash + metadata validation
internal/vector/records.go    # upsert, get, delete projection, delete object
internal/vector/spaces_list.go
internal/vector/*_test.go
```

(Or equivalents under `internal/api`; one package owns these
responsibilities.)

## Validation

- Unit tests (no database), table-driven:
  - vector validation: non-array → `invalid_vector`; wrong length →
    `invalid_vector_dimensions`; hex hash: 63/65 chars, non-hex, mixed
    case (accepted, normalized); empty object_id/projection_id →
    `missing_field`; bad RFC 3339 → `invalid_timestamp`; non-object
    metadata → `metadata_not_object`; oversized metadata →
    `metadata_too_large`; batch of `VEC_UPSERT_MAX_RECORDS + 1` →
    `batch_too_large`; duplicate (`object_id`, `projection_id`) pair
    within one batch → `duplicate_record` (pre-SQL check).
  - space resolution outcome table: unknown key → 404; disabled → 422;
    retired → 422 for upsert / allowed for get+delete+search (table
    covering all five operations).
- Integration tests (real PostgreSQL + pgvector; unit 10
  infrastructure), using the seeded
  `qwen3-embedding-0.6b-1024-cosine-v1` space (1024 dims; tests generate
  synthetic 1024-dim vectors):
  - upsert accounting (the in-statement `pre` CTE is the **only**
    accounting mechanism; in every case
    `upserted + unchanged == len(records)`):
    - (a) all-new batch → `upserted = N, unchanged = 0`;
    - (b) repeat of an identical batch → `upserted = 0, unchanged = N`;
    - (c) batch that changes `content_hash` on stored records → all
      counted as upserted;
    - (d) mixed batch (new + unchanged + changed in one request) →
      counts exact per classification, e.g. 3 new + 2 unchanged +
      2 changed → `upserted = 5, unchanged = 2`;
    - (e) duplicate logical identity (the same `object_id` +
      `projection_id` appearing twice in one batch) → 400
      `duplicate_record`, no rows written, and direct DB assertion that
      neither occurrence took effect (the rejection is pre-SQL);
  - upsert with 1023/1025-dim vector → 422 `invalid_vector_dimensions`;
  - upsert into an unknown space → 404 `vector_space_not_found`;
  - **trigger defense-in-depth (DB-enforced rejection + `P0001`
    mapping):** INSERT directly as the test fixture role (unit 10
    infrastructure, application context set), bypassing the service
    validation path:
    - (a) **dimension mismatch** — a 512-dim vector into the seeded
      1024-dim space → the trigger rejects; assert the driver error is
      SQLSTATE `P0001` and the primary message has the prefix
      `embedding dimension mismatch: `;
    - (b) **disabled space** — a test fixture UPDATE sets
      `enabled = false` on the seeded space (restored afterwards) →
      the trigger rejects; assert the error is `P0001` with prefix
      `vector space is disabled: `;
    - (c) **unknown space** — an INSERT with a non-existent
      `vector_space_id` (random UUID) → the trigger's unknown-space
      branch fires (BEFORE ROW triggers run before table constraint
      checks, so this yields the trigger's `P0001`, not the FK's
      `23503`); assert the prefix `unknown vector_space_id: `;
    - (d) **mapping composition** — for each of (a)–(c), pipe the
      **captured** `pgconn.PgError` through the unit 09 classifier and
      assert exactly (422, `invalid_vector_dimensions`),
      (422, `vector_space_unavailable`), and (404,
      `vector_space_not_found`); assert the rendered error body carries
      only the catalog code and its stable message — no trigger
      message, no SQLSTATE, no interpolated UUID/dimension. An
      unrecognized `P0001` message (a test-only plpgsql raise)
      classifies to 500 `internal`, and a non-`P0001` error whose text
      happens to contain a trigger prefix is **not** mapped by the
      prefixes (the SQLSTATE gates first).
  - **concurrent state-transition race** (real PostgreSQL, two
    connections; table over transition ∈ {disable: `UPDATE
    vector_control.vector_spaces SET enabled = false WHERE id =
    $space`, retire: `UPDATE vector_control.vector_spaces SET
    retired_at = now() WHERE id = $space` — `enabled` stays
    `true`}):
    - (a) **Committed before lock → rejected (end-to-end through the
      handler).** A side session (test fixture role) commits the
      transition first; then a real upsert request through the full
      service path → 422 `vector_space_unavailable`, and a direct
      table assertion that no record rows were written. For the
      retirement case, additionally assert the space state at
      rejection is `enabled = true AND retired_at IS NOT NULL`, and
      that a **direct** INSERT bypassing the service into that
      retired space is **accepted** by the `0001` trigger (the
      trigger checks `enabled` only — so the 422 above must come
      from the service's locked re-check, not the trigger); clean up
      the directly inserted row;
    - (b) **Racing transition after the lock → serializes after the
      write.** Connection A (test fixture role) drives the documented
      upsert transaction: `BEGIN`,
      `set_config('vector.application_id', …, true)`, `SELECT … FOR
      UPDATE` on the space row, then (after a `pg_locks` poll confirms
      A holds the row lock) connection B (test fixture role) issues
      the transition UPDATE — which must block on A's lock; assert B
      is still in flight while A is uncommitted. A then runs the
      documented upsert statement (single-record batch) and
      `COMMIT`s; B's UPDATE proceeds and B commits. Assert: A's
      upsert succeeded (record row exists, accounting exact); the
      transition took effect **afterward** (space now disabled /
      retired); a subsequent upsert through the real service path is
      now rejected with 422. (A mirrors the documented transaction —
      the handler's lock window is not externally observable; case
      (a) covers the handler end-to-end.)
  - get: own record → 200 with all API fields, no embedding; other
    application's record UUID → 404 `record_not_found`; record in another
    namespace of the same application → 404; malformed record_id → 400
    `invalid_uuid`;
  - delete projection: existing → `deleted: 1`; repeat → `deleted: 0`;
    other application's object → `deleted: 0` (no error, nothing
    visible);
  - delete object: mixed spaces — object in 2 spaces, delete with
    `?vector_space=` removes one (assert the other survives); without the
    parameter removes all in the namespace (assert zero remain and
    sibling objects untouched); object in another namespace →
    `deleted: 0`;
  - content hash stored as raw 32 bytes; API returns lowercase hex even
    when submitted uppercase;
  - list vector-spaces: seeded space present with correct fields; a
    test-fixture disabled or retired space is absent;
  - cross-application isolation spot checks (full matrix in unit 10):
    app B cannot upsert/get/delete into app A's namespace at all (404 at
    namespace resolution).

## Out of scope

- Search and metadata filtering (unit 08).
- Vector-space creation/modification/retirement endpoints — none exist in
  the API; spaces enter the system only through released migrations.
  The upsert state lock (Scope) introduces no vector-space admin API
  and no privilege change: it runs on the runtime role's existing
  `SELECT` grant.
- Partial-batch success semantics, pagination, and record listing
  endpoints.
- Source-content storage or retrieval (never implemented; the service
  stores only the hash).

## Open issues

- **C7 (status split).** Unknown space → `404 vector_space_not_found`;
  disabled/retired → `422 vector_space_unavailable` (with the
  write-vs-read retirement split defined in Scope). The split follows
  from the API's 404 "hidden resource" examples and 422 "incompatible
  vector-space state" example but is not explicitly pinned — review
  point.
- **Retired-space read/delete access** (allowed) is a proposal derived
  from DATA_MODEL retirement semantics and MIGRATIONS.md's
  retire-then-delete flow; review point.
- **`object_id`/`projection_id` per-field length cap (512)** and
  per-record body share of the 16 MiB body limit are proposals; review
  points.
