# Implementation Contracts — Overview

This directory contains the dependency-ordered implementation contracts for a
clean implementation restart of Vector Service. The repository currently
contains **only authoritative sources**:

- `README.md`, `AGENTS.md` (repository root)
- `docs/ARCHITECTURE.md`, `docs/DATA_MODEL.md`, `docs/API.md`,
  `docs/SECURITY.md`, `docs/MIGRATIONS.md`, `docs/DEVELOPMENT.md`
- `migrations/README.md`, `migrations/0001..0004_*.sql`

The four SQL migrations are the **exact, immutable schema authority**. They
are never modified by this implementation. Every contract below derives from
them and from the documentation.

---

# Dependency Sequence

Unit 09 is split into two build phases because it is both a dependency of
and a dependency *consumer* of units 06–08:

- **09a — shared HTTP primitives** (strict JSON decoding, content-type
  checks, body bounds, error renderer + SQLSTATE classifier, request-ID
  handling, access logging, panic recovery). Depends on 01–02 only.
- **09b — router assembly, health/readiness, graceful shutdown.** Wires the
  05–08 handlers onto the router and owns `/healthz`, `/readyz`. Depends on
  03 (the pre-serving migration step and its in-process convergence state)
  and 05–08 (handler registration).

This split removes the former 06–08 ↔ 09 cycle: 06–08 consume 09a at build
time; only 09b depends back on 06–08.

| Unit | File | Depends on |
|------|------|-----------|
| 01 | [01-foundation.md](01-foundation.md) | — |
| 02 | [02-database-connectivity.md](02-database-connectivity.md) | 01 |
| 03 | [03-embedded-migrations.md](03-embedded-migrations.md) | 02 |
| 04 | [04-isolation-core.md](04-isolation-core.md) | 02 |
| 05 | [05-authentication.md](05-authentication.md) | 04 |
| 09a | [09-api-hardening.md](09-api-hardening.md) (Scope A) | 01, 02 |
| 06 | [06-admin-api.md](06-admin-api.md) | 05, 04, 09a |
| 07 | [07-vector-crud.md](07-vector-crud.md) | 05, 04, 09a |
| 08 | [08-search-filtering.md](08-search-filtering.md) | 07, 09a |
| 09b | [09-api-hardening.md](09-api-hardening.md) (Scope B) | 03, 05–08 |
| 10 | [10-testing.md](10-testing.md) | 01–09 (both phases) |
| 11 | [11-container-runtime.md](11-container-runtime.md) | 01–09 (both phases) |

Units 04–08 are the isolation-critical core and should be reviewed before
unit 10's release-critical tests are considered sufficient.

---

# Global Invariants

These hold across all units. A change that breaks any of them is a defect,
not an implementation choice.

1. **Application identity comes only from authentication.** No data-plane
   request field may select an application.
2. **Every data-plane operation is namespace-scoped.** No default, wildcard,
   or implicit all-namespace behavior. The single documented exception is the
   global vector-space catalog endpoint (`GET /v1/vector-spaces`, Conflict
   C13), which exposes read-only metadata only.
3. **Transaction-local RLS context.** Every application-scoped transaction
   runs `SELECT set_config('vector.application_id', <uuid>, true)` as its
   first statement; `true` is mandatory (transaction-local). No session
   persistent context, ever.
4. **Fail-closed isolation.** Missing or invalid application context must
   expose no rows (schema `0001` RLS policies make this true by construction
   via `NULLIF(current_setting('vector.application_id', true), '')::uuid`).
5. **RLS remains enabled and forced** on `vector_data.vector_records` and
   `vector_control.namespaces`. The runtime role has no `BYPASSRLS` and does
   not own the database.
6. **No embedding generation.** Vectors arrive pre-embedded; the service
   validates and stores them.
7. **Vector spaces are immutable compatibility contracts.** Dimension and
   state validation happens in the service **and** is re-enforced by the
   `vector_records_validate` trigger from migration `0001`.
8. **Opaque credentials.** Raw application credentials are returned exactly
   once at creation and never persisted or logged. Only `SHA-256(raw)`
   (32 bytes, per `0004` and `docs/SECURITY.md`) is stored.
9. **Admin authority is distinct** from application credentials: a separate
   authentication mechanism (unit 05) over separate endpoints.
10. **Migrations are forward-only, embedded, checksum-verified, and
    advisory-locked** (unit 03). Released migration files are never edited.
11. **No caller-controlled SQL.** All caller values are query parameters,
    including metadata filter field names (bound via `metadata ->> $1`).
12. **Bounded everything**: request body, batch size, search limit, filter
    count, filter value list, metadata size.
13. **Error responses are stable JSON** with machine-readable `code`,
    operator-facing `message`, and `request_id`. Raw PostgreSQL errors are
    never returned to callers.
14. **Structured logs only** (`log/slog`), with operational fields
    (`request_id`, application UUID, namespace UUID, operation, duration,
    result count, status). Never: credentials, credential digests (by
    default), vectors, metadata bodies, source contents.

---

# Configuration Reference (proposed)

Environment variables (flags may mirror them). Required values fail fast at
startup with a non-zero exit; no insecure defaults are substituted.

| Variable | Meaning | Default |
|----------|---------|---------|
| `VEC_LISTEN_ADDR` | HTTP listen address | `127.0.0.1:8080` |
| `VEC_PG_HOST` | PostgreSQL host | *(required)* |
| `VEC_PG_PORT` | PostgreSQL port | `5432` |
| `VEC_PG_DATABASE` | Database name | must be `vector` (validated, see Conflict C4) |
| `VEC_PG_USER` | Runtime role (C2: `vector_api`, the constrained runtime role) | `vector_api` |
| `VEC_PG_MIGRATION_USER` | Distinct, privileged migration identity (C2/C12: `vector_owner` in the canonical shape). **No default** — must be set when it differs from the runtime role, which is always in a correctly provisioned deployment (see C2) | *(required)* |
| `VEC_PG_CA_CERT` | Path to PostgreSQL CA certificate for the **runtime** connection | *(required)* |
| `VEC_PG_CLIENT_CERT` | Path to TLS client certificate for the **runtime** connection (scoped to `vector_api`, per SECURITY.md) | *(required)* |
| `VEC_PG_CLIENT_KEY` | Path to TLS client private key for the **runtime** connection | *(required)* |
| `VEC_PG_MIGRATION_CA_CERT` | Path to PostgreSQL CA certificate for the **migration** connection (may equal `VEC_PG_CA_CERT`) | *(optional; defaults to `VEC_PG_CA_CERT`)* |
| `VEC_PG_MIGRATION_CLIENT_CERT` | Path to TLS client certificate for the **migration** connection (scoped to the migration identity; distinct from the runtime certificate) | *(required when `VEC_PG_MIGRATION_USER` is set)* |
| `VEC_PG_MIGRATION_CLIENT_KEY` | Path to TLS client private key for the **migration** connection | *(required when `VEC_PG_MIGRATION_USER` is set)* |
| `VEC_ADMIN_TOKEN` | High-entropy admin bearer token | *(required)* |
| `VEC_HTTP_MAX_BODY_BYTES` | Max HTTP request body | `16777216` (16 MiB) |
| `VEC_UPSERT_MAX_RECORDS` | Max records per upsert batch | `100` |
| `VEC_SEARCH_MAX_LIMIT` | Max search results (hard API ceiling 200) | `200` |
| `VEC_MAX_FILTERS` | Max filter entries per request | `10` |
| `VEC_MAX_FILTER_VALUES` | Max values per `in` filter | `50` |
| `VEC_MAX_METADATA_BYTES` | Max serialized metadata per record | `16384` (16 KiB) |
| `VEC_MIGRATION_LOCK_WAIT` | Bounded wait for migration advisory lock | `300s` |
| `VEC_LOG_LEVEL` | `debug` / `info` / `warn` / `error` | `info` |
| `VEC_LOG_FORMAT` | `json` (only) | `json` |

Defaults for limits are proposals consistent with `docs/API.md` and
`docs/SECURITY.md`; the exact values are a review point, but the *existence*
of every bound is required.

---

# Error Code Catalog (v1)

Stable machine-readable codes. Status mapping per `docs/API.md`.

| Code | Status | Meaning |
|------|--------|---------|
| `unauthorized` | 401 | Missing/invalid/disabled/expired application credential, disabled application, or missing/invalid admin token |
| `invalid_json` | 400 | Malformed JSON body |
| `unknown_field` | 400 | Request body contains an unrecognized field (strict decoding) |
| `invalid_content_type` | 400 | `Content-Type` is not `application/json` on a JSON endpoint |
| `missing_field` | 400 | Required request field absent (including empty `records` array) |
| `invalid_application_key` | 400 | Application key absent/empty or fails `^[a-z][a-z0-9_-]{0,63}$` (mirrors schema constraint) |
| `invalid_namespace_key` | 400 | Namespace key empty, contains `/` or control characters, or unreasonably long |
| `invalid_limit` | 400 | Search `limit` outside `1..min(200, configured maximum)` |
| `invalid_timestamp` | 400 | Not a valid RFC 3339 timestamp |
| `invalid_uuid` | 400 | Path/parameter value is not a valid UUID (e.g. `record_id`, `credential_id`) |
| `invalid_content_hash` | 400 | Not 64 hex characters |
| `invalid_vector_number` | 400 | Vector element is not a parseable finite number |
| `non_finite_vector` | 422 | Vector element is NaN or ±Infinity |
| `invalid_vector` | 400 | Vector is not a JSON array of numbers |
| `invalid_vector_dimensions` | 422 | Vector length differs from vector-space dimensions |
| `vector_space_not_found` | 404 | Unknown `vector_space` key |
| `vector_space_unavailable` | 422 | Vector space is disabled (any operation that names it) or retired (upsert only; reads/deletes of existing records remain allowed) |
| `namespace_not_found` | 404 | Namespace missing, disabled, or not owned by caller (uniform) |
| `record_not_found` | 404 | Record not visible in the requested namespace |
| `application_not_found` | 404 | (Admin plane only) Valid-grammar application key with no registered application |
| `credential_not_found` | 404 | (Admin plane only) Credential ID unknown or not owned by the named application |
| `not_found` | 404 | Unknown route (no matching API path) — not a resource-level 404 |
| `method_not_allowed` | 405 | Known path, unsupported HTTP method |
| `conflict` | 409 | Duplicate application key, namespace key, or credential name |
| `duplicate_record` | 400 | Upsert batch contains the same logical identity (`object_id` + `projection_id`) more than once; rejected in Go before any write (unit 07) |
| `batch_too_large` | 413 | Upsert batch exceeds configured maximum |
| `body_too_large` | 413 | HTTP body exceeds configured maximum |
| `metadata_not_object` | 400 | Metadata is not a JSON object |
| `metadata_too_large` | 400 | Metadata exceeds configured size |
| `invalid_filter` | 400 | Unsupported operator, bad field grammar, non-string value, or limit exceeded |
| `unavailable` | 503 | Database unavailable or migrations incomplete |
| `internal` | 500 | Unexpected server error |

Notes:

- All authentication failures share one code and status (no enumeration).
- `namespace_not_found` is deliberately uniform for missing / disabled /
  foreign namespaces (`docs/SECURITY.md`). This uniformity is a
  **data-plane** requirement; admin-plane endpoints (unit 06) may return
  precise 404s (`application_not_found`, `credential_not_found`) because
  the admin caller is trusted.
- Unknown vector space → `404`; disabled or retired vector space → `422`
  (Conflict C7, settled per unit 07: the 422 applies to operations that
  *name* the space — upsert, search, projection delete, object delete
  with `?vector_space=` — and, for retired spaces, only to upsert.
  Record-level operations that do not name a space (get by record ID,
  object delete without the parameter) are not gated on space state, so
  records in disabled or retired spaces remain retrievable and deletable
  by ID — matching the schema trigger, which blocks writes only, and the
  MIGRATIONS.md retire-then-purge flow). The status codes for the other
  rows follow `docs/API.md` directly.

---

# Surfaced Conflicts and Open Issues

These are apparent conflicts or ambiguities in the authoritative sources.
They are **not** resolved silently. Each is restated in the contract that is
most affected.

**C1. Namespace resolution order vs. schema RLS.** `docs/ARCHITECTURE.md`
and `docs/API.md` conceptually resolve the namespace *before* `BEGIN` /
`set_config`. But migration `0001` enables **forced** RLS on
`vector_control.namespaces`, so a namespace row is invisible to any
transaction without application context. The only workable order is:
authenticate → `BEGIN` → `set_config` → resolve namespace → continue, all in
one transaction. Unit 04 mandates that order. The documentation's conceptual
flow is superseded by the schema, which is immutable.

**C2. Which role executes migrations.** `0004` grants privileges **to**
`vector_api`; `docs/SECURITY.md` describes `vector_owner` (owns
schema/database objects) vs `vector_api` (constrained runtime operations)
and states the service **must not connect as `postgres` or
`vector_owner` during normal operation**; `docs/DEVELOPMENT.md` lists a
single PostgreSQL user in configuration. **Decision taken for this restart
(review point):** the two-role model is the supported contract.

- `vector_api` (configured via `VEC_PG_USER`) is the **runtime** role. It
  performs all normal-operation work and only the privileges granted by
  migration `0004`. It does **not** own the database or any schema object
  and never holds `CREATE` on the `vector` database or either schema in a
  correctly provisioned deployment.
- The **migration identity** (configured via `VEC_PG_MIGRATION_USER`) is a
  distinct, privileged role — `vector_owner` in the canonical shape — that
  owns the database (or, at minimum, holds `CREATE` and `CONNECT … WITH
  GRANT OPTION` on it — see the grant-authority note in C12) and executes
  the unit 03 migration step. `docs/SECURITY.md`'s prohibition applies to
  *normal operation*; the startup migration phase (the `migrate`
  subcommand, and `serve`'s pre-serving migration step) may use the
  migration identity. A deployment that cannot provide a distinct
  migration identity must fall back to the C12 option 2 or 3 shapes
  (pre-provisioned objects, or explicit `CREATE` + `CONNECT … WITH GRANT
  OPTION` grants) and must document that deviation — a single-role
  deployment in which `vector_api` both migrates and serves violates the
  SECURITY.md owner/runtime separation and is **not** a supported shape.
- The two identities require **separate connection material**: distinct
  roles, distinct TLS client certificates (SECURITY.md: the client
  certificate is "scoped to the runtime PostgreSQL role"), and therefore
  the migration connection has its own CA/cert/key paths
  (`VEC_PG_MIGRATION_*`, configuration reference below). The migration
  connection is built by the same unit 02 builder with different
  role/TLS inputs (unit 03).
- Readiness and all data-plane/admin traffic run **only** as
  `vector_api`; `/readyz` never connects as the migration identity.

Complete migration credentials (role, database access, TLS identity) are
specified per deployment shape in unit 03 (Scope: role model and
privileges) and in unit 11 (deployment shapes).

**C3. pgvector extension is not created by any migration.** `0001` uses the
`vector` type, so the extension must already exist in the `vector` database
(infrastructure contract per `docs/MIGRATIONS.md` "pgvector Extension"). The
migration runner validates this as a prerequisite and fails with a clear
error (unit 03). This is a startup precondition, not a silent assumption.

**C4. Database name is hardcoded in `0004`.** `GRANT CONNECT ON DATABASE
vector TO vector_api` requires the target database to be named `vector`.
Configuration validates this at startup; test infrastructure must create a
database named `vector` (units 02, 03, 10).

**C5. Roles must pre-exist for `0004` to apply.** `0004` grants to
`vector_api`, so that role must exist before the first migration
(`GRANT … TO vector_api` fails otherwise). The migration identity itself
is the pre-existing infrastructure role that *performs* the grant — it
cannot be created by the migration set. Provisioning of both identities
is infrastructure's job (`docs/MIGRATIONS.md` ownership boundary); the
runner validates both and surfaces the failure clearly if either is
absent.

**C6. Non-generic names in `migrations/README.md`.** That file references
project-specific names (Chimera, Bookshelf, Vault, Galaxy, Obsidian), which
conflicts with `AGENTS.md`'s rule that examples use generic names. The file
is authoritative and is not modified; it does not change any service
behavior, and no contract depends on those names.

**C7. Unknown vector space status code is unpinned.** `docs/API.md` lists
examples for 400/404/422 but none explicitly covers "unknown vector space
key". Unit 07 proposes `404 vector_space_not_found` for unknown keys and
`422 vector_space_unavailable` for disabled/retired spaces. Review point.

**C8. Credential wire format.** The `docs/API.md` create-credential example
returns `"credential": "vsvc_<opaque-secret>"`. The contracts take this to
mean: generated credential = `vsvc_` + base64url(32 random bytes), and the
stored digest is `SHA-256` of the **full string including the prefix**.
Review point (format only; hashing rule is authoritative per `SECURITY.md`).

**C9. Admin namespace registration requires application RLS context.**
Not documented explicitly, but a consequence of the `WITH CHECK` clause of
the forced `namespaces_application_isolation` policy: inserting a namespace
row outside an application-context transaction is rejected by RLS. Unit 06
therefore runs admin namespace registration inside `WithAppContext(appID)`.

**C10. `0004` grants `DELETE` on `application_credentials`**, but the API's
`DELETE /credentials/{id}` endpoint means *disable*, not row deletion. The
service must never delete credential rows; the extra grant is accepted as
authoritative schema state and simply unused.

**C11. Filename typo in `0003`.** The file is named `0003_qwen3_hnsq.sql`
(likely "hnsw"). Authoritative as-is; the runner orders by numeric prefix,
so the typo has no effect.

**C12. No migration creates the history table.** `docs/MIGRATIONS.md`
requires the runner to maintain `vector_control.schema_migrations`, but
none of the four released migrations creates it, and `0004` grants
`vector_api` only `USAGE` (not `CREATE`) on both schemas. Under the C2
two-role decision this is resolved deterministically:

> **Grant authority (applies to every shape).** Migration `0004` executes
> `GRANT CONNECT ON DATABASE vector TO vector_api`. PostgreSQL only permits
> a role to grant `CONNECT` on a database if it **owns the database** or
> holds `CONNECT` on it **with grant option**. A bare `CREATE` grant on the
> database is *not* sufficient — it allows `CREATE SCHEMA` but conveys no
> grantable `CONNECT`. Every supported shape below therefore gives the
> migration identity database ownership or `CREATE` + `CONNECT … WITH
> GRANT OPTION`, and unit 10's integration tests verify that a `migrate`
> run succeeds through **all four** migrations — including `0004`'s
> `GRANT` statements — in each supported shape.

- **Canonical shape (recommended production):** the migration identity
  (`vector_owner`, `VEC_PG_MIGRATION_USER`) is a sufficiently privileged,
  pre-provisioned role — in the canonical case the **owner** of the
  `vector` database; at minimum it must satisfy the grant-authority note
  above. On a fresh database it creates the `vector_control` schema
  **and** the history table *before* migration `0001` runs (unit 03,
  bootstrap step), then applies each migration body plus its history
  `INSERT` in one transaction. `vector_api` never touches the history
  table — it has no grant on it, and none is needed: history is written
  exclusively by the migration identity. **Readiness does not read
  migration history via the migration identity:** `/readyz` uses only the
  in-process startup-migration convergence state plus a runtime-pool
  health check (unit 09b). The migration credentials and connection exist
  only during the pre-serving migration phase (or the `migrate`
  subcommand) and are closed and released before the HTTP server begins
  serving; no migration connection exists in steady state.
- **Option 2 (pre-provisioned):** infrastructure pre-creates the schemas
  and history table and grants `vector_api` the `0004` runtime
  privileges; migrations are then applied by the migration identity as
  in the canonical shape. This shape is advertised **only if** the
  pre-provisioning gives the migration identity everything needed to
  execute all pending migration DDL (`0001`–`0004`) as that identity:
  `CREATE` on (or ownership of) both pre-created schemas, full
  `SELECT`/`INSERT` on the pre-created `schema_migrations`, and the
  grant authority the note above requires (database ownership, or
  `CREATE` + `CONNECT ON DATABASE vector WITH GRANT OPTION`) — in
  practice, the pre-provisioning is performed **by or as the configured
  migration identity**. A pre-provisioning that gives the migration
  identity only partial privileges (e.g. merely `INSERT` on the history
  table, or `CREATE` without `CONNECT … WITH GRANT OPTION`) cannot apply
  `0001`–`0004` and is **not** a supported shape; the runner fails on
  the first statement it cannot execute with an actionable error naming
  the missing privilege.
- **Option 3 (explicit grants, constrained fallback):** infrastructure
  grants the configured migration identity **both** `CREATE ON DATABASE
  vector` (so the bootstrap can create the schema and history table)
  **and** `CONNECT ON DATABASE vector … WITH GRANT OPTION` (so migration
  `0004` can execute `GRANT CONNECT ON DATABASE vector TO vector_api`;
  a bare `CREATE` grant alone is not a supported shape — it cannot
  execute `0004`). The runner's bootstrap creates the schema + history
  table as that identity. `vector_api` is still granted `CREATE` on
  nothing.

In **no** shape does `vector_api` create schemas or the history table,
and in no shape does the runtime role record history. The unit 03 runner
therefore has a single deterministic bootstrap path (identity-based)
instead of a probe-and-fallback ladder; a bootstrap that cannot create
the history table fails with an actionable error naming the missing
privilege. It never silently skips history recording. Review point: the
default deployment shape must be settled by the operator before first
startup; the service supports the three shapes above.

**C13. Global vector-space catalog vs. the namespace-scoping rule.**
`AGENTS.md` and the root `README.md` state that *every* data-plane
operation is namespace-scoped, but `docs/API.md` defines `GET
/v1/vector-spaces` as an authenticated endpoint with **no namespace path
segment**, and `vector_control.vector_spaces` (migration `0001`) has no
application or namespace column — the catalog is global metadata. This
conflict is surfaced, not silently resolved. The contracts take the
smallest clearly-labeled position: the vector-space catalog listing is an
**explicitly documented exception** to the namespace-scoping rule. It is
read-only, requires normal application authentication, exposes no record
data, and is the only data-plane endpoint without a namespace segment. It
does not introduce cross-namespace retrieval of any kind (unit 07). The
authoritative documents are not modified. Review point: if the blanket
"every data-plane operation is namespace-scoped" rule is intended to
cover catalog metadata as well, `docs/API.md`'s endpoint definition must
be changed first — that is an architecture decision, not an implementation
decision.

---

# Conventions Used by All Contracts

- Language: Go (current stable), `github.com/jackc/pgx/v5` (+ `pgxpool`) as
  the only expected runtime dependency; `log/slog` for logging.
- Module path: `vector-service` (placeholder; adjust to the canonical VCS
  path if one exists — review point).
- Package layout follows `docs/DEVELOPMENT.md`'s illustrative tree with one
  deviation: a dedicated `internal/control` package for control-plane
  operations (application/namespace/credential lifecycle). The README tree
  is explicitly "illustrative, not mandatory".
- Vector type handling: pgvector's `vector` is an extension type unknown to
  pgx by default. The database layer registers a **text-format codec** for
  `vector` (OID looked up from `pg_type`), encoding/decoding `[]float32`
  against the `[a,b,c]` text representation. Dimension and finiteness
  validation happens in Go before encoding.
- UUIDs for new rows (applications, namespaces, credentials, records) are
  generated by the service (`crypto/rand`, UUID v4).
- Timestamps in JSON are RFC 3339 (`...Z`). Nullable fields
  (`source_updated_at`) are omitted from JSON when NULL.
- Content hashes are accepted as 64 hex characters (either case) and
  normalized to lowercase before storage (stored as raw 32 bytes).
