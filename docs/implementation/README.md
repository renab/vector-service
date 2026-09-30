# Implementation Contracts — Overview

This directory is the complete implementation contract for building the
Vector Service. It is deliberately small: one overview (this file) and
five dependency-ordered work packages. The packages define boundaries,
invariants, and acceptance criteria — not line-by-line pseudocode.

Authoritative sources remain authoritative: `docs/API.md`,
`docs/ARCHITECTURE.md`, `docs/DATA_MODEL.md`, `docs/SECURITY.md`,
`docs/MIGRATIONS.md`, `docs/DEVELOPMENT.md`, the released migrations in
`migrations/`, and `AGENTS.md`. Where a package appears to conflict with
them, the authoritative source wins and the discrepancy must be fixed in
the package.

## Work packages

| # | Package | File | Delivers | Depends on |
|---|---------|------|----------|------------|
| 1 | Startup, database, migrations | `package-1-startup-database.md` | Binary skeleton, `serve`/`migrate` subcommands, fail-fast configuration, runtime pgx pool (`vector_api`), migration-identity connection, embedded canonical migration runner, pgvector text codec | — |
| 2 | Identity and isolation | `package-2-identity-isolation.md` | Data-plane credential auth, admin token auth, transaction helpers (`WithAppContext`, `WithShortTx`) with transaction-local RLS, namespace resolution | 1, 5a (test harness) |
| 3 | Vector operations | `package-3-vector-operations.md` | Upsert, get, projection delete, object delete, search with the v1 filter language, vector validation, vector-space resolution | 2, 5a |
| 4 | HTTP API | `package-4-http-api.md` | All routes, middleware chain, strict JSON, request IDs, access logging, error rendering (the single classification boundary), `/healthz`, `/readyz`, bounded shutdown, admin endpoints | 1, 2, 3, 5a |
| 5 | Verification and runtime | `package-5-verification-runtime.md` | **5a** test harness (first deliverable, lands right after package 1); **5b** release-critical isolation matrix, migration matrix, HTTP integration; **5c** container image and deployment shape | 1 (5a); 1–4 (5b, 5c) |

Sequencing is vertical-slice: packages 2–4 are each delivered **with**
their real-PostgreSQL integration tests, which run on the 5a harness.
The 5a harness therefore lands immediately after package 1, before
package 2 starts. The full release-critical matrices (5b) complete with
the last package.

## Non-negotiable invariants

One list, referenced by every package. These come from the authoritative
documents and the released migrations; none may be relaxed by
implementation convenience.

1. **Identity from credentials only.** Every normal data-plane caller
   belongs to exactly one registered application, determined by the
   authenticated credential. No header, body field, or path segment ever
   supplies an application ID on the data plane.
2. **Namespace scope on everything.** Every data-plane operation is
   scoped to exactly one explicit namespace. No default namespace, no
   wildcard, no implicit cross-namespace operation. Missing / disabled /
   foreign namespaces all return a uniform 404.
3. **Forced RLS stays on.** Row-Level Security remains enabled and
   forced on `vector_records` and `namespaces` (migration `0001`). Every
   application-scoped transaction establishes transaction-local context
   with `SELECT set_config('vector.application_id', '<uuid>', true)` as
   its **first statement** — transaction-local (third argument `true`),
   never session-persistent, on pooled connections. The runtime role is
   never `BYPASSRLS` and never the database owner.
4. **Two identities, canonical boundary.** The runtime role is
   exactly `vector_api` (granted by migrations `0004` and `0005`); the
   migration identity is a distinct privileged role (the database
   owner, canonically `vector_owner`). Identity collapse is rejected
   at startup. The migration identity is used only for the migration
   step and owns the `vector` database, both migrated schemas, and
   every in-scope object — every `pg_class` row in the
   `vector_control` and `vector_data` schemas (all tables, sequences,
   indexes, views, and the migration-history table), every `pg_type`
   row (all migrated types), and every `pg_proc` row (all migrated
   functions and procedures) — including any future object those
   schemas acquire. Startup and migration convergence fail closed on
   any ownership mismatch — before serving and before accepting
   pre-existing migrated state. Both connections are
   identity-asserted at connect time (defense in depth): the runtime
   pool verifies `current_user` is exactly `vector_api` and the role
   is neither superuser, `BYPASSRLS`, nor database owner; the
   migration connection verifies `current_user` is exactly the
   configured canonical migration identity, differs from `vector_api`
   (runtime-role substitution is rejected), is **not** a superuser,
   and **owns** the `vector` database before any bootstrap. The
   assertions establish the **complete effective privilege boundary**
   of `vector_api` (not merely the nominal role), verified through the
   PostgreSQL catalogs on the runtime connection: the role owns none
   of the migrated schemas or objects (the `vector_control` or
   `vector_data` schemas, the migration-history table, or any table,
   sequence, function, index, or type in them) and is not the database
   owner; its **effective** privileges — direct grants, grants on any
   role in the runtime role's INHERIT membership closure (transitive
   `pg_auth_members` edges `member → roleid` following only edges
   whose PostgreSQL 18 `inherit_option` is true — `inherit_option`
   controls automatic privilege inheritance, `set_option` controls
   whether `SET ROLE` can assume the granted role, and `admin_option`
   controls whether the member may grant/revoke or alter that
   membership), and `PUBLIC` grants —
   equal exactly the canonical runtime grant set established by
   migrations `0004` (direct grants: `CONNECT` on the database,
   schema `USAGE` without `CREATE`, the table grants, no grant on
   `vector_control.schema_migrations`) and `0005` (direct `EXECUTE` on
   the migrated functions, direct `USAGE` on the migrated types, and
   the `PUBLIC` baseline: no `PUBLIC` `TEMPORARY` on the database, no
   `PUBLIC` `EXECUTE` on the migrated functions, no `PUBLIC` `USAGE`
   on the migrated types) and nothing else — including, at column
   level, exactly the column-applicable subset of each table's
   canonical set on every column of every in-scope table — and no
   privilege of any kind on `vector_control.schema_migrations` or on
   any other object outside that set; no reachable membership edge
   has `set_option` or `admin_option` true (the zero-membership
   rule; the `admin_option` half is checked explicitly in the
   membership closure), and no role in that membership closure is
   the migration identity, a superuser, `BYPASSRLS`,
   `CREATEDB`/`CREATEROLE`, or an owner of the `vector` database or
   any migrated schema or object (package 1, section 2 — the check is
   a bounded, exact comparison against the canonical grant set, not a
   proof about privileges the cluster could ever gain).
   Pre-existing migrated schemas or objects are accepted only when
   canonically owned by the migration identity — every in-scope
   `pg_class`, `pg_type`, and `pg_proc` row, not just the schemas and
   history; anything else is a fatal, actionable error before serving,
   before bootstrap, and before the state is accepted. An unexpected
   role, superuser, runtime-role
   substitution, ownership mismatch, or effective-privilege violation
   is fatal. The two TLS identities remain distinct (separate client
   certificate/key pairs, verified at startup).
5. **Immutable vector spaces.** Vector spaces are immutable compatibility
   contracts. The service never creates or modifies space rows at
   runtime; new or changed spaces enter only through new migrations.
6. **No embeddings in the service.** The service stores and compares
   caller-supplied vectors only. No embedding-model clients, no
   natural-language query embedding, no model routing, no reranking.
7. **Application neutral.** No domain-specific concepts from any
   consumer (note types, provenance models, project entities,
   application-specific ranking) may appear in the shared layer.
8. **Explicit SQL.** Explicit SQL with all caller-controlled values
   bound as query parameters. No ORM. Metadata filtering uses only the
   constrained v1 filter language (`all` + `eq`/`in`, top-level keys).
9. **Bounded inputs.** Request bodies, batch sizes, search limits,
   filter counts and values, metadata sizes, request deadlines, and
   shutdown deadlines are all explicitly bounded.
10. **Migrations are a compatibility contract.** Released migrations are
    immutable and forward-only. The runner enforces checksums, executes
    each migration atomically, and writes history only as the migration
    identity.
11. **Stable errors.** Responses carry only catalog error codes with
    stable messages (below). Raw PostgreSQL error text never reaches a
    response body.
12. **Structured logs, no secrets.** Structured JSON logs with request
    ID, application UUID, namespace UUID, operation, duration, result
    count, and status. Never: raw credentials, digests, TLS private
    keys, full embedding vectors, arbitrary caller metadata, source
    document contents.
13. **TLS to PostgreSQL.** Both identities connect with
    TLS client-certificate authentication and full server verification.
    `VEC_PG_TLS_MODE=plain` exists solely for explicit development and
    testing; it is never the default.
14. **Real database in tests.** RLS, transaction-scoped `set_config`,
    triggers, `ON CONFLICT`, advisory locks, and `vector` operators are
    tested against real PostgreSQL + pgvector — never mocked.

## Error model — one classification boundary

- Service functions (everything below the HTTP handler) return plain
  `error`. They do not build HTTP statuses or response codes.
- **The HTTP handler layer is the single classification boundary.**
  Each endpoint handler converts every raw error it receives **exactly
  once** into a typed `*apierr.Error` (code + status + stable message
  from the catalog) via the shared classifier. The classifier first
  checks the request context, then applies the SQLSTATE table, then the
  `P0001` trigger-prefix sub-classification. Already-typed values pass
  through unchanged.
- **Transaction helpers are a clear, small boundary.** The package-2
  helpers own transaction lifecycle (acquire, `BEGIN`, `set_config`,
  `COMMIT`, `ROLLBACK`). On **infrastructure** failure they return one
  of two typed outcomes: the abort sentinel (inactive request context
  with a client-initiated cause) or `unavailable` (503) — including an
  inactive context with a service-initiated cause (operation deadline
  or serve shutdown). **Business** failures from the callback pass
  through unclassified and are classified exactly once at the handler
  boundary.
- **Cancellation (per-request settlement, one mechanism).** Every
  request carries one small synchronized **settlement state** (one
  mutex; package 4, section 2) that atomically records the **first**
  of three outcomes: `client`, `service`, or `response`. The first
  claim wins; later claims and later markers are no-ops and can never
  reclassify the request. Cancellation never carries classification
  information of its own — only these claims do:
  - **`client`** — the client disconnected and the net/http server
    canceled the transport request context. The `client` outcome is
    recorded **only** at a settlement point that **synchronously**
    observes the transport context inactive, each check running under
    the settlement state's synchronization **before** any outcome is
    recorded (package 4, section 2 — first-cause rule): the deadline
    stage's fire path, the response gate's first commit, the shared
    cause rule, and the deferred path's finalization. There is no
    asynchronous watcher and no separate claimer: a transport
    cancellation that happens-before a service settlement is seen by
    that settlement's own check and settles the request as `client`; a
    service event records `service` only while the transport is still
    active, and can therefore never reclassify an already-canceled
    transport request, whatever the scheduling. → **abort**: no
    response is written at all — the write is suppressed because the
    transport's closure was already known at the render decision
    (access log `status_code` 0; nothing is committed);
  - **`service`** — the operation deadline fired (the deadline stage
    claims `service` in the settlement state **before** it cancels the
    effective context), or serve shutdown (the lifecycle publishes the
    process-level shutdown marker **before** canceling the serve root;
    the marker is a process-global fallback that settles a request as
    service **only while no per-request outcome has been recorded and
    the transport is still active**).
    → **exactly one `503 unavailable` is committed** (catalog body;
    access log `status_code` 503) when the transport is still active at
    the render; if the transport's closure was already known at the
    render decision (the client disconnected after the `service` claim
    and before the render), the write is suppressed — nothing is
    committed and the access line carries `status_code` 0 — while the
    recorded outcome stays `service` (never reclassified). A committed
    status is never retroactively zeroed by a later, unobserved
    disconnect (commit is not proof of delivery, and delivery is not
    asserted);
  - **`response`** — the first status commit (package 4, section 2
    response gate). It races atomically with the cancellation claims:
    a success commit that lands first is preserved even if a
    cancellation follows, and no success commit can land after a
    `service` claim (the gate rejects it; the centralized responder
    writes the 503 instead).
  The process-level shutdown marker never reclassifies a request whose
  per-request outcome already won: a client-claimed request stays an
  abort (never a 503) even when the marker is later set, and a
  committed response keeps its first status. One shared cause rule
  (implemented once; used by the package-2 helpers and the package-4
  classifier/renderer) reads an inactive effective context's settlement
  state through its synchronization and settles it by that state:
  `client` → the abort sentinel; `service` → `unavailable` (503);
  `response` → no further response (the committed status stands);
  no outcome yet → the shutdown marker if set, else client (a
  markerless inactive context is a transport cancel). There is no
  generic "inactive context" rule: the abort sentinel is the **only**
  no-response outcome, and it is produced only by the client outcome
  (claimed or markerless). Cancellation is an internal control outcome,
  not a catalog code.

Classifier mapping (implemented once, package 4). The first two rows
apply the shared cause rule to the request's settlement state:
`service` outcome (operation-deadline claim) or no outcome with the
shutdown marker set → service-initiated; `client` outcome, or no
outcome with no marker → client-initiated; a `response` outcome means
the committed status stands and nothing further is rendered.

| Condition | Outcome |
|-----------|---------|
| effective context inactive, service settlement (`service` outcome, or no outcome + shutdown marker set) | `unavailable` (503) |
| effective context inactive, client settlement (`client` outcome, or no outcome + no marker) | abort sentinel (no response; see Cancellation) |
| already-typed `*apierr.Error` | pass through |
| SQLSTATE `23505` (unique violation) | `conflict` (409) |
| SQLSTATE `23514` (CHECK) / `22P02` (invalid `vector`) / `23503` / `55P03` / anything else | the catalog code the owning flow knows about, else `internal` (500) |
| SQLSTATE `P0001`, message prefix `unknown vector_space_id: ` | `vector_space_not_found` (404) |
| SQLSTATE `P0001`, message prefix `vector space is disabled: ` | `vector_space_unavailable` (422) |
| SQLSTATE `P0001`, message prefix `embedding dimension mismatch: ` | `invalid_vector_dimensions` (422) |
| SQLSTATE `P0001`, any other prefix | `internal` (500) |
| connection failure, pool exhaustion, acquisition or query timeout (context still active) | `unavailable` (503) |

The three `P0001` prefixes come from the `vector_records_validate`
trigger in migration `0001`, which uses unqualified `RAISE EXCEPTION`.
They map to the **same** catalog codes the service's own validation
emits for the same conditions — trigger enforcement is defense in
depth, not a new error surface.

## Configuration reference

Environment variables (flags may mirror them). Missing or invalid
required values fail fast at startup: structured error, non-zero exit,
no network activity, no listening port.

| Variable | Meaning | Default |
|----------|---------|---------|
| `VEC_LISTEN_ADDR` | HTTP listen address | `127.0.0.1:8080` |
| `VEC_PG_HOST` | PostgreSQL host | *(required)* |
| `VEC_PG_PORT` | PostgreSQL port | `5432` |
| `VEC_PG_DATABASE` | Database name. Must be `vector` — migration `0004` hardcodes `GRANT CONNECT ON DATABASE vector` | `vector` (validated) |
| `VEC_PG_USER` | Runtime role. Fixed to exactly `vector_api` — the role migration `0004` binds runtime privileges to; any other value is rejected at startup | `vector_api` (validated) |
| `VEC_PG_MIGRATION_USER` | Distinct privileged migration identity (database owner; canonically `vector_owner`). **Required, no default**; must differ from the runtime role — identity collapse is rejected before any network activity | *(required)* |
| `VEC_PG_TLS_MODE` | Transport for **both** identities; the single source of truth. `tls`: client-certificate authentication with full server verification (production). `plain`: explicit development/test only; never default, never implicit; no TLS path variables may be set in `plain` mode. Any other value is a startup error | `tls` |
| `VEC_PG_CA_CERT` | CA certificate path, runtime connection (`tls` mode) | *(required in `tls` mode)* |
| `VEC_PG_CLIENT_CERT` / `VEC_PG_CLIENT_KEY` | TLS client certificate/key, runtime connection (scoped to `vector_api`) | *(required in `tls` mode)* |
| `VEC_PG_MIGRATION_CA_CERT` | CA certificate path, migration connection | *(optional; defaults to `VEC_PG_CA_CERT`)* |
| `VEC_PG_MIGRATION_CLIENT_CERT` / `VEC_PG_MIGRATION_CLIENT_KEY` | TLS client certificate/key, migration identity (distinct from the runtime pair; verified by parsing) | *(required pair in `tls` mode)* |
| `VEC_PG_TLS_SERVER_NAME` | Hostname for server-certificate validation (`tls` mode) | `VEC_PG_HOST` |
| `VEC_ADMIN_TOKEN` | High-entropy admin bearer token (admin plane only) | *(required)* |
| `VEC_HTTP_MAX_BODY_BYTES` | Max HTTP request body | `16777216` (16 MiB) |
| `VEC_HTTP_REQUEST_TIMEOUT` | Request-operation deadline: bounds every request's effective context, including probes. Distinct from the `http.Server` connection timeouts (transport guards only) | `30s` |
| `VEC_HTTP_SHUTDOWN_GRACE` | Bounded drain deadline (`Server.Shutdown`) on SIGINT/SIGTERM. The sum of this value and `VEC_HTTP_SHUTDOWN_CLOSE_TIMEOUT` must not exceed 40 s — configs violating the bound are rejected at startup | `30s` |
| `VEC_HTTP_SHUTDOWN_CLOSE_TIMEOUT` | Hard deadline for the bounded pool close during shutdown; exit 1 if exceeded. The sum of this value and `VEC_HTTP_SHUTDOWN_GRACE` must not exceed 40 s — configs violating the bound are rejected at startup | `10s` |
| `VEC_UPSERT_MAX_RECORDS` | Max records per upsert batch | `100` |
| `VEC_SEARCH_MAX_LIMIT` | Max search results (API hard ceiling 200) | `200` |
| `VEC_MAX_FILTERS` | Max filter entries per request | `10` |
| `VEC_MAX_FILTER_VALUES` | Max values per `in` filter | `50` |
| `VEC_MAX_METADATA_BYTES` | Max serialized metadata per record | `16384` (16 KiB) |
| `VEC_MIGRATION_LOCK_WAIT` | Bounded wait for the migration advisory lock | `300s` |
| `VEC_LOG_LEVEL` | `debug` / `info` / `warn` / `error` | `info` |
| `VEC_LOG_FORMAT` | `json` (only supported format) | `json` |

Default limit values are proposals consistent with `docs/API.md`; the
existence of every bound is required.

## Error code catalog (v1)

Stable machine-readable codes. This is the complete set that can appear
in a response body.

| Code | Status | Meaning |
|------|--------|---------|
| `unauthorized` | 401 | Missing/unknown/invalid/expired/disabled application credential, disabled application, or missing/invalid admin token. Uniform — no enumeration of failure kinds |
| `invalid_json` | 400 | Malformed JSON body or trailing content after the document |
| `unknown_field` | 400 | Unrecognized request field (strict decoding) |
| `invalid_content_type` | 400 | `Content-Type` not `application/json` on a JSON endpoint |
| `missing_field` | 400 | Required field absent (including an empty `records` array) |
| `invalid_application_key` | 400 | Application key fails `^[a-z][a-z0-9_-]{0,63}$` (mirrors schema constraint) |
| `invalid_namespace_key` | 400 | Namespace key empty, contains `/` or control characters, or too long |
| `invalid_limit` | 400 | Search `limit` outside `1..min(200, configured max)` or not an integer |
| `invalid_uuid` | 400 | Path/parameter value is not a valid UUID |
| `invalid_timestamp` | 400 | Not a valid RFC 3339 timestamp |
| `invalid_content_hash` | 400 | Not 64 lowercase hex characters |
| `invalid_vector` | 400 | Vector is not a JSON array of numbers |
| `non_finite_vector` | 422 | Vector element is NaN or ±Infinity; zero-norm vector in a cosine space (upsert or search) |
| `invalid_vector_dimensions` | 422 | Vector length differs from the vector space's dimensions |
| `vector_space_not_found` | 404 | Unknown `vector_space` key |
| `vector_space_unavailable` | 422 | Disabled vector space (any operation that names it) or retired vector space (upsert only) |
| `namespace_not_found` | 404 | Namespace missing, disabled, or not owned by caller (uniform, data plane) |
| `record_not_found` | 404 | Record not visible in the requested namespace |
| `application_not_found` | 404 | Admin plane: valid-grammar key with no registered application |
| `credential_not_found` | 404 | Admin plane: credential ID unknown or not owned by the named application |
| `not_found` | 404 | Unknown route (no matching API path) |
| `method_not_allowed` | 405 | Known path, unsupported HTTP method |
| `conflict` | 409 | Duplicate application key, namespace key, or credential name |
| `duplicate_record` | 400 | Upsert batch repeats the same `(object_id, projection_id)`; rejected before any write |
| `batch_too_large` | 413 | Upsert batch exceeds the configured maximum |
| `body_too_large` | 413 | HTTP body exceeds the configured maximum |
| `metadata_not_object` | 400 | Metadata is not a JSON object |
| `metadata_too_large` | 400 | Metadata exceeds the configured size |
| `invalid_filter` | 400 | Unsupported operator, bad field grammar, non-string value, or filter limit exceeded |
| `unavailable` | 503 | Database unavailable, migrations incomplete, or request abandoned by deadline/shutdown while the connection was still usable |
| `internal` | 500 | Unexpected server error |

Notes:

- All authentication failures share one code and status.
- `namespace_not_found` uniformity is a data-plane requirement; admin
  endpoints may return precise 404s (`application_not_found`,
  `credential_not_found`) because the admin caller is trusted.
- Vector-space state gating: `422` applies to operations that **name**
  the space — upsert, search, projection delete, object delete with
  `?vector_space=` — and for **retired** spaces, only to upsert.
  Record-level operations that do not name a space (get by record ID,
  object delete without the parameter) are not gated on space state, so
  records in disabled or retired spaces remain retrievable and
  deletable by ID.
- The abort sentinel is not a catalog code (see Error model).

## Conventions used by all packages

- Go standard library for HTTP; `pgx` (v5) for PostgreSQL; no framework.
- Small focused packages under `internal/`; `cmd/vector-service` for the
  binary.
- `log/slog` with JSON output; one structured access line per request.
- Table-driven tests; deterministic synthetic fixtures.
- Generic names only in docs and tests (applications
  `memory-service`, `notes-service`, `search-service`; namespaces
  `project-alpha`, `project-beta`, `research`; per `AGENTS.md`).
- Context propagation everywhere; every database operation runs under
  the request's effective (deadline-bounded) context.

## Unresolved

- **C13 — `GET /v1/vector-spaces`.** `docs/API.md` defines this endpoint
  as requiring normal application authentication, but it has no namespace
  segment, which conflicts with the established rule that **every**
  data-plane operation is namespace-scoped (`AGENTS.md`, `README.md`).
  Settled interim handling: **the endpoint is not implemented** until the
  authoritative docs are explicitly revised; requests to the path receive
  the standard unknown-route 404. No other previously-surfaced item is an
  open conflict: the migration provisioning shape is deliberately
  canonical-only (see package 1), and the documented `set_config`
  ordering is fixed by the request-transaction sequence in package 2.
