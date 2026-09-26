# Unit 09 — HTTP Layer, Strict JSON, Error Mapping, Logging, Health/Readiness

## Objective

Implement the cross-cutting HTTP and API-hygiene layer that every
endpoint in units 05–08 depends on:

- request routing and method dispatch
- strict JSON decoding with unknown-field rejection
- content-type enforcement
- bounded request bodies
- sane HTTP server timeouts
- stable error mapping to the v1 error catalog (never raw PostgreSQL
  text to callers)
- request IDs
- structured logging
- `GET /healthz` and `GET /readyz`
- panic recovery

This unit owns no business logic. It is the membrane between HTTP and the
service layer.

## Authority

- `docs/API.md` — base path `/v1` (health endpoints not versioned);
  `application/json` content type; error format
  `{"error": {"code", "message", "request_id"}}`; "Raw PostgreSQL errors
  must not be returned directly"; HTTP status examples (400/401/404/409/
  413/422/503); Request IDs — service assigns or accepts, accepted
  external values validated and bounded, returned in responses and
  included in logs, not a token; `GET /healthz` process-only, no auth,
  `{"status":"ok"}`; `GET /readyz` requires successful configuration +
  applied migrations + usable PostgreSQL, no auth, `{"status":"ready"}`
  or 503.
- `docs/DEVELOPMENT.md` — stdlib HTTP, JSON request/response structs,
  reject malformed JSON, consider rejecting unknown fields where
  strictness improves safety (v1 decision: **all** JSON endpoints,
  matching the README error catalog), bounded bodies, request IDs, sane
  timeouts (read-header, read, write, idle — "Do not use an unconfigured
  default HTTP server in production"); internal errors map to stable
  responses, logs may carry structured DB error details while responses
  stay controlled; `log/slog` preferred; never log vectors or
  credentials.
- `docs/SECURITY.md` — structured logs; allowed fields (request ID,
  application UUID, namespace UUID, operation, duration, result count,
  response status); forbidden-by-default fields (bearer credentials,
  keys, full embeddings, arbitrary caller metadata, source contents).
- `docs/implementation/README.md` — config reference
  (`VEC_HTTP_MAX_BODY_BYTES`, `VEC_LOG_LEVEL`, `VEC_LOG_FORMAT`,
  `VEC_ADMIN_TOKEN` presence, PG settings); error catalog; Global
  Invariants 11, 12, 14.
- `docs/implementation/05–08` — the handlers whose requests this layer
  decodes; error codes they emit.
- `docs/implementation/03-embedded-migrations.md` — the in-process
  migration state from the pre-serving `migrate.Run` step that readiness
  consumes (readiness never reads the history table and never connects
  as the migration identity).
- `docs/implementation/02-database-connectivity.md` — the pool readiness
  readiness checks use.
- `migrations/0001_shared_vector_schema.sql` — the
  `vector_data.validate_vector_record` trigger: **unqualified**
  `RAISE EXCEPTION` (every rejection it performs carries SQLSTATE
  `P0001`) and the three message formats the classifier sub-classifies
  (Scope, "Error mapping" → `P0001` sub-classification).

## Dependencies

This unit is built in two phases per the README dependency table (09a /
09b); the split is what keeps the unit graph acyclic:

- **09a — shared HTTP primitives** (server construction, strict JSON,
  content-type/body caps, error renderer + SQLSTATE classifier,
  request-ID handling, structured logging, panic recovery). Depends on:
  - Unit 01 (config, `serve` subcommand, `apierr` error type).
  - Unit 02 (pool, for readiness checks).

  Units 05–08 consume 09a's primitives at build time; 09a itself
  depends on none of them.
- **09b — router assembly, health/readiness, graceful shutdown.**
  Wires the 05–08 handlers onto the router and owns `/healthz`,
  `/readyz`. Depends on:
  - 09a.
  - Unit 03 (startup migration state for the readiness check).
  - Units 05–08 (the endpoint handlers registered on this router).

## Scope

### Server construction

- `net/http` `ServeMux` with Go 1.22+ pattern methods
  (`"PUT /v1/namespaces/{namespace}/records"` style); exact patterns for
  every API.md endpoint; no subtree wildcards on API paths (the two
  path-segment endpoints use `{...}` segment patterns).
- `http.Server` with explicit timeouts (defaults are proposals — review
  point; existence is required):

  | setting | proposal |
  |---------|----------|
  | `ReadHeaderTimeout` | 10s |
  | `ReadTimeout` | 30s |
  | `WriteTimeout` | 60s |
  | `IdleTimeout` | 120s |

- TLS termination is **external** (reverse proxy) in the assumed
  deployment; the binary listens on plain TCP per the listen-address
  config. (If a deployment needs in-process TLS that is a separate
  decision — out of scope here.)
- Graceful shutdown on SIGINT/SIGTERM: stop accepting, drain in-flight
  requests with a bounded deadline, close the pool (unit 02), exit 0.

### Routing outcomes

- Unknown path → 404, error body with code `not_found` (new generic
  catalog entry — README catalog updated; review point).
- Known path, wrong method → 405 with `Allow` header, error body code
  `method_not_allowed` (new generic catalog entry — README catalog
  updated; review point).
- Malformed path-segment values (e.g. `{record_id}` that is not a UUID,
  empty `{namespace}`) are handled by the owning handler's validation
  (400 `invalid_uuid` / `missing_field`), not by the router.

### JSON decoding (strict)

- Every JSON endpoint decodes into an **explicit request struct** with
  `json.Decoder` configured to reject unknown fields
  (DisallowUnknownFields-equivalent). Unknown top-level field → 400
  `unknown_field`.
- Malformed JSON → 400 `invalid_json`.
- Trailing content after the JSON document → 400 `invalid_json`
  (decoder must consume exactly one value).
- `metadata` fields (upsert records, namespace registration) are the
  one intentionally-generic value: decoded as a raw JSON token,
  re-validated as an **object** (400 `metadata_not_object`), size-capped
  by `VEC_MAX_METADATA_BYTES` (400 `metadata_too_large`), and re-encoded
  compactly before storage. No generic map deserialization for anything
  else (DEVELOPMENT.md).
- Numeric vector elements: decoded so non-finite input yields the typed
  `non_finite_vector` per unit 07 (custom number handling where needed).
- Bodies are read through `http.MaxBytesReader` capped at
  `VEC_HTTP_MAX_BODY_BYTES` → 413 `body_too_large`.
- `Content-Type` on body-bearing endpoints: must be `application/json`
  (charset parameter allowed, e.g. `application/json; charset=utf-8`) →
  else 400 `invalid_content_type`. `DELETE …/records` carries a JSON
  body and is checked like other body endpoints.

### Error mapping

- `apierr` (unit 01) is the single error type crossing the HTTP
  boundary: `(code, status, message)` from the README catalog. Handlers
  return `apierr` values; the transport renders the API error shape:

  ```json
  { "error": { "code": "…", "message": "…", "request_id": "…" } }
  ```

  with the mapped status. `message` is a stable, human-readable string
  per code; it never interpolates caller input or driver text.
- PostgreSQL errors are classified by SQLSTATE into catalog codes (the
  sole exception: `P0001`, sub-classified by message prefix — see the
  note below the table); the mapping table lives in the `apierr`/service
  layer:

  | SQLSTATE | meaning | mapping |
  |----------|---------|---------|
  | `23503` | FK violation | `internal` (should be unreachable via validated flows) |
  | `23505` | unique violation (admin key/name) | `conflict` |
  | `23514` | CHECK violation (metadata object, hash length, …) | the validation's code if the flow knows it, else `internal` |
  | `22P02` | invalid `vector` input | `invalid_vector_dimensions`/`invalid_vector` contextually, else `internal` |
  | `P0001` | plpgsql `RAISE EXCEPTION` with no explicit SQLSTATE (includes the `0001` `vector_records_validate` trigger) | message-prefix sub-classification (below): `vector_space_not_found` (404) / `vector_space_unavailable` (422) / `invalid_vector_dimensions` (422); any other `P0001` → `internal` (500) |
  | `55P03` | read-only transaction etc. | `internal` |
  | connection/timeout failures | pool exhausted, network, canceled context | `unavailable` (503) |
  | anything else | — | `internal` (500) |

- **`P0001` sub-classification.** The `0001` trigger
  `vector_data.validate_vector_record` raises with **unqualified**
  `RAISE EXCEPTION`, so every rejection it performs arrives as SQLSTATE
  `P0001` with a message that interpolates the offending values (space
  UUID; expected/received dimensions). `P0001` is the only row in the
  table whose mapping depends on the message: the classifier matches the
  pgx `PgError` primary-message prefix, gated on `Code == "P0001"`:

  | trigger message prefix (`0001`) | code | status |
  |---------------------------------|------|--------|
  | `unknown vector_space_id: ` | `vector_space_not_found` | 404 |
  | `vector space is disabled: ` | `vector_space_unavailable` | 422 |
  | `embedding dimension mismatch: ` | `invalid_vector_dimensions` | 422 |

  Any other `P0001` message → `internal` (500). A non-`P0001` error is
  never mapped by these prefixes (the SQLSTATE gates first). The three
  codes are the same catalog codes the service's own validation emits
  for the same conditions (unit 07, "Trigger rejection mapping
  (defense in depth)"), so trigger-level defense in depth (Global
  Invariant 7) adds no new error surface: the response carries only the
  catalog code and its stable message — never the trigger message, the
  SQLSTATE, or the interpolated values (the structured-log rule below
  still applies).
- Raw PostgreSQL error strings never reach a response body. They **may**
  appear in server logs as a structured `db_error` field (DEVELOPMENT.md
  error model) — and only in the structured log, never in the `message`.
- Context cancellation (client disconnect) → the request is aborted
  cleanly; no 5xx is written (no response possible); logged as
  `aborted`, not `internal`.
- Panic recovery middleware: any handler panic → 500 `internal`,
  stack trace to server logs only, connection kept sane. Because the
  request-ID middleware runs **before** recovery (Middleware order,
  below), the recovery path always has the effective request ID in
  context: the `X-Request-Id` response header, the error body's
  `request_id`, and the panic's server log line must all carry the
  **same** effective request ID (consistent with unit 01's middleware
  chain). The response body is still the catalog shape
  (`code: internal`, stable message); only the logs carry the stack
  trace.

### Request IDs

- Per request: if the inbound `X-Request-Id` header matches
  `^[A-Za-z0-9._-]{1,128}$`, it is accepted (validated and bounded per
  API.md); otherwise the service **generates** a UUID v4 (proposal:
  replace rather than reject invalid inbound IDs — review point).
- The effective request ID is: returned in the `X-Request-Id` response
  header on every response (success and error), embedded in the error
  body's `request_id`, and included in every log line for the request.
- A request ID is never an authentication input and is never logged as
  sensitive (it is an identifier, Global Invariant 14-safe).

### Structured logging

- `log/slog`, JSON handler, `VEC_LOG_FORMAT=json` (only format),
  `VEC_LOG_LEVEL`.
- Request lifecycle line (one per request): `request_id`, method, path,
  operation, application UUID (when authenticated), namespace UUID (when
  resolved), vector-space key (when resolved), status, duration_ms,
  result count where meaningful (`upserted`/`unchanged`/`deleted`/
  `matches`/`created`).
- Forbidden by default (Global Invariant 14): bearer credentials or
  credential digests, TLS/PG private keys, complete embedding vectors,
  arbitrary caller metadata bodies, source document contents. Filter
  values are not logged (search logging records filter count only,
  unit 08).
- Auth failures log `auth_failure` with the credential's
  **application-independent** context (no credential material, no
  digests) so operators can see failure volume without enumeration
  help (unit 05's uniform 401 discipline extends to logs: do not log
  which credential matched/failed).
- Startup logs: config summary (no secret values — token length at
  most), pool parameters, migration state, listen address.

### `/healthz`

```text
GET /healthz        → 200 {"status":"ok"}
```

- Process liveness only: the HTTP handler runs. No authentication, no
  database, no config re-check. Never 500/503 from this endpoint
  except an unrecoverable process fault (in which case it won't answer
  at all).

### `/readyz`

```text
GET /readyz   →  200 {"status":"ready"}   when ready
              →  503 {"error":{"code":"unavailable", …}}   otherwise
```

Readiness = all of (API.md minimum):

1. **Configuration successful** — true from process start (config is
   fail-fast, unit 01); a process that is running has satisfied this.
2. **Required migrations applied** — startup check: the unit 03
   `migrate.Run` step (prerequisite validation + checksum comparison +
   application of pending migrations) must have succeeded during
   `serve` startup, and its success state is held in-process. The
   runtime role has **no grant on `vector_control.schema_migrations`**
   (C12: history is written exclusively by the migration identity), so
   readiness must observe schema currency through that startup step —
   never by reading the history table as `vector_api`. Incomplete or
   drifted → not ready (and per unit 03, `serve` fails fast before
   listening when the startup step fails, so a running process always
   started from a current or verifiable schema state).
3. **Usable PostgreSQL connection** — per request: `SELECT 1` on a
   short-lived bounded check (proposal: 2s timeout, from the pool) →
   failure means not ready. (Proposal; review point — the alternative is
   cached state refreshed on a short interval; per-request check is
   simplest and always true.)

- No authentication. Readiness failures are 503 `unavailable`; the
  response body carries the generic error shape (no diagnostic detail
  beyond the code — readiness is not a diagnostic endpoint; operators
  use logs).
- Startup ordering: the `serve` command connects, verifies migration
  state, and only then starts serving. If initial migration verification
  fails, the process refuses to serve and exits non-zero (proposal:
  fail-fast; review point — the alternative is serve-and-report-503
  until a `migrate` run fixes state; fail-fast matches "required
  database migrations applied" being a hard readiness precondition and
  keeps the migration runner the single remediation path, unit 03).
- While the process is running and the DB later dies, `/readyz` flips to
  503 via the per-request check and `/healthz` stays 200 — that
  asymmetry is the intended liveness/readiness split.

### Middleware order

```text
request ID → recover → (auth: data-plane | admin | none) →
content-type/body-cap (body endpoints) → handler
```

- Request-ID assignment is the **outermost** middleware: it runs before
  panic recovery so that a recovered panic (and its log line) uses the
  same effective request ID as the response, and before auth so that
  auth-failure logs carry it too.
- `/healthz`, `/readyz` bypass auth entirely.
- Auth middleware per unit 05 (data plane) and unit 06 (admin plane);
  they are separate middleware so a data-plane credential can never reach
  an admin handler and vice versa (DEVELOPMENT.md).

## Interfaces and boundaries

- `internal/api/transport.go` (or `internal/http`): router, server
  construction, middleware, error rendering, request-ID handling.
- `internal/api/errors.go`: the SQLSTATE→apierr classifier.
- `internal/api/health.go`: `/healthz`, `/readyz` handlers + readiness
  state.
- Handlers from units 05–08 register via a small interface
  (`ServeHTTP(ctx, identity, pathParams, body)` or thin HTTP handlers) —
  implementer's choice; the point is that business code never touches
  `http.ResponseWriter` directly except through the error renderer.
- Consumes: `apierr` (01), config (01), pool (02), migration state (03).

## Invariants and correctness constraints

- **No raw driver text in responses** (API.md, DEVELOPMENT.md);
  responses contain only catalog codes and fixed messages. Driver message
  text is *read* only for the `P0001` prefix sub-classification; it is
  never rendered into a response body or a `message` field.
- **Bounded bodies** before decoding (413) — the decoder must never see
  more than `VEC_HTTP_MAX_BODY_BYTES` (Global Invariant 12).
- **No unconfigured default server** (DEVELOPMENT.md): all four timeout
  fields set explicitly.
- **Auth planes disjoint**: data-plane and admin auth are different
  middleware over different route sets; no shared credential path
  (DEVELOPMENT.md admin separation, unit 05/06).
- **Request IDs are not secrets and not tokens** (API.md).
- **Logging discipline** per Global Invariant 14 and SECURITY.md; the
  log line is the only place DB error detail may appear.
- **Health vs readiness**: `/healthz` never touches the database;
  `/readyz` is the only endpoint that reports DB/migration state.
- **404 uniformity on the data plane is untouched** by routing: unknown
  *API resources* keep the unit 04/07/08 semantics; `not_found`/
  `method_not_allowed` apply only to unknown *routes*.

## Expected implementation surface

```text
internal/api/transport.go     # server, mux, middleware, shutdown
internal/api/errors.go        # apierr rendering + SQLSTATE classifier
internal/api/requestid.go     # inbound validation + generation
internal/api/health.go        # healthz/readyz + readiness state
internal/api/transport_test.go
```

## Validation

- Unit tests (no database), table-driven:
  - JSON strictness: unknown field → `unknown_field`; malformed JSON →
    `invalid_json`; trailing garbage → `invalid_json`; empty body where
    required → `missing_field`; `Content-Type` without `application/json`
    (with and without charset) → `invalid_content_type`; oversized body
    → `body_too_large`.
  - SQLSTATE classifier: full table over the mapping rows above —
    including the three `P0001` trigger-prefix rows (`unknown
    vector_space_id: ` → 404 `vector_space_not_found`, `vector space is
    disabled: ` → 422 `vector_space_unavailable`, `embedding dimension
    mismatch: ` → 422 `invalid_vector_dimensions`), an unrecognized
    `P0001` message → `internal` (500), a non-`P0001` error carrying a
    trigger-like prefix (unmapped by the prefixes — the SQLSTATE gates
    first), and unknown states → `internal` — with an assertion that no
    rendered body contains the matched raw message or SQLSTATE.
  - Request ID: valid inbound accepted and echoed; invalid inbound
    (bad chars, > 128, empty) replaced by generated UUID; echo header
    present on 2xx and error responses.
  - Panic recovery: handler panics (test-only route) → 500 `internal`
    with the catalog body shape, no stack trace in the body; the
    response `X-Request-Id` header, the error body's `request_id`, and
    the server log record all carry the **same** effective request ID
    (inbound ID echoed when valid; generated UUID when not); the log
    record carries the stack trace.
  - Error rendering: exact body shape for one success-adjacent 4xx and
    the 503 shape; no `message` ever contains SQLSTATE text.
  - Readiness: ready (all green) → 200; DB down → 503; migrations
    incomplete → 503; `/healthz` 200 while `/readyz` 503 (asymmetry).
- HTTP integration tests (unit 10 infrastructure): real server on a
  test port; full path `HTTP → auth → service → PostgreSQL` for one
  data-plane and one admin call; request-ID echo; 405 with `Allow`; 404
  `not_found` for an unknown path; panic-injection test (handler
  panicking via a test-only route) → 500 `internal`, no stack in body,
  and the response `X-Request-Id` header, the error body's
  `request_id`, and the structured log line all carry the same
  effective request ID.

## Out of scope

- In-process TLS termination (external proxy assumed; in-process TLS
  would be a separate decision).
- Rate limiting, request quotas, and admission control beyond body
  caps.
- OpenTelemetry/tracing, metrics endpoints (DEVELOPMENT.md: metrics not
  required for the first milestone).
- CORS configuration (no documented cross-origin consumer; a reverse
  proxy owns that if ever needed).
- API versioning mechanics beyond the fixed `/v1` base path.

## Open issues

- **HTTP timeout defaults** (10s/30s/60s/120s, matching unit 01) are
  proposals; the
  requirement is that all four are configured explicitly. Review point.
- **Invalid inbound `X-Request-Id` → replace with generated ID**
  (rather than 400) is a proposal. Review point.
- **Readiness mechanics** (per-request `SELECT 1`, 2s timeout;
  fail-fast startup when migrations are incomplete) are proposals
  consistent with API.md's readiness definition. Review point.
- **New catalog entries** `not_found` (404) and `method_not_allowed`
  (405) for unknown routes — README catalog to be updated; review point
  (the authoritative API.md does not enumerate unknown-route errors).
