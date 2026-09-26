# Unit 06 — Administrative API: Application, Namespace, and Credential Lifecycle

## Objective

Implement the four administrative endpoints defined by `docs/API.md`
(Administrative API) over the distinct admin-token authentication of unit
05, producing registered applications, namespaces, and application
credentials from the trusted control plane:

```text
POST   /v1/admin/applications
POST   /v1/admin/applications/{application}/namespaces
POST   /v1/admin/applications/{application}/credentials
DELETE /v1/admin/applications/{application}/credentials/{credential_id}
```

`{application}` is the application **key** (not the UUID), per the API
contract. This unit is the only code path that writes to
`vector_control.applications`, `vector_control.namespaces`, and
`vector_control.application_credentials`.

## Authority

- `docs/API.md` — the four endpoint paths, request/response shapes,
  application-key path segment, 409 on conflicting keys, raw credential
  returned exactly once, disablement is not deletion and is idempotent,
  admin authentication distinct from application authentication, admin
  endpoints on trusted internal networks.
- `docs/SECURITY.md` — admin credentials are a separate mechanism; raw
  credentials never persisted or logged; only `SHA-256(raw)` stored.
- `docs/DATA_MODEL.md` — application key immutability; namespaces owned by
  exactly one application; namespace keys unique within an application;
  namespace metadata is optional caller JSON object; multiple credentials
  per application; credential disablement prevents future authentication
  without deleting the parent.
- `migrations/0001_shared_vector_schema.sql` — `applications` columns and
  `application_key ~ '^[a-z][a-z0-9_-]{0,63}$'` CHECK; `namespaces`
  columns, `UNIQUE (application_id, namespace_key)`,
  `jsonb_typeof(metadata) = 'object'` CHECK, `ON DELETE RESTRICT` FK,
  **forced RLS with `WITH CHECK`** on `namespaces` (Conflict C9).
- `migrations/0004_runtime_identity.sql` — `application_credentials`
  columns, `UNIQUE (application_id, credential_name)`,
  `UNIQUE (credential_hash)`, `octet_length(credential_hash) = 32` CHECK,
  `credential_name` non-empty CHECK; grants: `vector_api` holds
  `SELECT, INSERT, UPDATE` on `applications` and `namespaces`, full CRUD
  on `application_credentials`, `SELECT` on `vector_spaces`.
- `docs/implementation/05-authentication.md` — `auth.CheckAdmin`,
  `auth.GenerateCredential`, digest rule (Global Invariant 8).
- `docs/implementation/04-isolation-core.md` — `WithAppContext` for the
  namespace INSERT (Conflict C9).
- `docs/implementation/README.md` — error catalog (including
  `application_not_found`, `credential_not_found`, `invalid_uuid`),
  configuration reference, Conflicts C9/C10, Global Invariants 8/9.

## Dependencies

- Unit 01 (apierr, request IDs, structured logging, config).
- Unit 02 (pool, bounded acquisition, pgx).
- Unit 04 (`WithAppContext` for namespace registration; application and
  credential operations use plain short transactions and do not depend on
  it).
- Unit 05 (admin token check; credential generator; digest rule).
- Unit 09a (shared HTTP primitives: strict-JSON decoding,
  content-type checks, error renderer, request IDs) — consumed at
  build time. 09a depends on neither this unit nor 09b, so the build
  order is acyclic (README dependency table).

## Scope

### Common handling for all four endpoints

1. **Authentication.** Admin token only, via `auth.CheckAdmin` (unit 05).
   Missing/malformed/wrong token → uniform `unauthorized` (401).
   Application credentials are **never** accepted on these endpoints, and
   the admin token is never consulted by data-plane handlers (Global
   Invariant 9). Keep the admin route handlers in a separate package
   (`internal/control`) so no shared handler can accidentally authorize
   with the wrong mechanism (DEVELOPMENT.md).
2. **Strict JSON bodies.** `Content-Type: application/json` required
   (400 `invalid_content_type` otherwise); malformed JSON →
   `invalid_json`; unknown fields → `unknown_field` (unit 09 mechanics).
3. **Path validation.** The `{application}` segment must match
   `^[a-z][a-z0-9_-]{0,63}$` (mirrors the schema CHECK) or the request
   fails with 400 `invalid_application_key` **before** any database work.
   A grammatically valid but unregistered key fails with
   404 `application_not_found` after lookup. (Admin plane: unlike the
   data plane, precise 404s are allowed — the admin caller is trusted and
   names keys it must know.)
4. **Path UUID validation.** `{credential_id}` must parse as a UUID or
   the request fails with 400 `invalid_uuid` before any database work.
5. **Application lookup** for endpoints 2–4, in a short bounded
   transaction (`applications` has no RLS; no application context needed):

   ```sql
   SELECT id, application_key, enabled
   FROM vector_control.applications
   WHERE application_key = $1;
   ```

   Zero rows → 404 `application_not_found`.
6. **Validation before database work** (DEVELOPMENT.md): field
   presence/grammar checks run in Go before any SQL.
7. **Logging.** Structured fields: `request_id`, `operation`, `status`,
   `duration`, application key/UUID, namespace key/UUID where applicable.
   Never: the admin token, the raw credential, or the credential digest
   (Global Invariants 8/14).

### Register application — `POST /v1/admin/applications`

Request body (strict):

```json
{ "application_key": "notes-service", "display_name": "Notes Service" }
```

- `application_key`: required; must match
  `^[a-z][a-z0-9_-]{0,63}$` → else 400 `invalid_application_key`.
- `display_name`: required, non-empty, ≤ 200 chars (proposal; review
  point) → else 400 `missing_field`.
- INSERT in a short transaction (no RLS on `applications`):

  ```sql
  INSERT INTO vector_control.applications
      (id, application_key, display_name)
  VALUES ($1, $2, $3)
  RETURNING id, application_key, display_name, enabled;
  ```

  `id` is a service-generated UUID v4. The `set_updated_at` trigger
  maintains `updated_at`.
- Duplicate key (SQLSTATE `23505` on `application_key`) →
  409 `conflict`. Application keys are immutable after creation; there is
  no endpoint to change them.
- Response 201:

  ```json
  { "id": "…", "application_key": "notes-service",
    "display_name": "Notes Service", "enabled": true }
  ```

### Register namespace — `POST /v1/admin/applications/{application}/namespaces`

Request body (strict):

```json
{ "namespace_key": "project-alpha", "display_name": "Project Alpha",
  "metadata": { "team": "search" } }
```

- `namespace_key`: required, non-empty, no `/`, no control characters,
  ≤ 128 chars (proposal; review point) → else 400
  `invalid_namespace_key`.
- `display_name`: required non-empty (same rule as above).
- `metadata`: optional; default `{}`. If present it must be a JSON object
  (400 `metadata_not_object`) with serialized size ≤
  `VEC_MAX_METADATA_BYTES` (400 `metadata_too_large`). It is stored as
  `jsonb` and is never an authorization input.

**Execution order (Conflict C9 — mandatory):**

1. Resolve the application key to its UUID (step 5 above), **outside**
   the context transaction.
2. Run the INSERT inside `isolation.WithAppContext(appID, fn)`:

   ```sql
   INSERT INTO vector_control.namespaces
       (id, application_id, namespace_key, display_name, metadata)
   VALUES ($1, $2, $3, $4, $5::jsonb)
   RETURNING id, namespace_key, display_name, enabled, metadata;
   ```

   The INSERT's `application_id` is the resolved UUID. Because the
   transaction-local `vector.application_id` equals that same UUID, the
   forced `namespaces_application_isolation` policy's `WITH CHECK`
   passes. Inserting this row **without** the context transaction is
   rejected by RLS — there is no shortcut path, and none may be added.
3. Duplicate `(application_id, namespace_key)` (`23505`) → 409 `conflict`.

Response 201:

```json
{ "id": "…", "application_key": "notes-service",
  "namespace_key": "project-alpha", "display_name": "Project Alpha",
  "enabled": true, "metadata": { "team": "search" } }
```

### Create credential — `POST /v1/admin/applications/{application}/credentials`

Request body (strict):

```json
{ "credential_name": "production" }
```

- `credential_name`: required, non-empty (schema CHECK
  `length(credential_name) > 0`) → else 400 `missing_field`.
- Steps:
  1. Resolve application key → UUID (404 if unknown).
  2. `auth.GenerateCredential()` (unit 05): `vsvc_` + unpadded base64url
     of 32 `crypto/rand` bytes (Conflict C8).
  3. Compute `SHA-256` over the **full raw string including the prefix**;
     the result is 32 bytes (satisfies the `0004` CHECK).
  4. INSERT in a short transaction (no RLS on
     `application_credentials`):

     ```sql
     INSERT INTO vector_control.application_credentials
         (id, application_id, credential_name, credential_hash)
     VALUES ($1, $2, $3, $4::bytea)
     RETURNING id;
     ```

     `expires_at` is inserted as `NULL`: the v1 API does not expose
     expiry setting (the column and its enforcement in unit 05 exist and
     remain authoritative; see Observation below).
  5. Duplicate `(application_id, credential_name)` (`23505`) →
     409 `conflict`.
- Response 201:

  ```json
  { "credential_id": "…", "credential_name": "production",
    "credential": "vsvc_…opaque…" }
  ```

  The raw credential appears **exactly once**, in this one response body.
  It must not be logged, stored in context, or echoed by any other code
  path. Only the 32-byte digest persists.

### Disable credential — `DELETE /v1/admin/applications/{application}/credentials/{credential_id}`

- No request body.
- Steps:
  1. Resolve application key → UUID (404 if unknown); validate
     `{credential_id}` as UUID (400 `invalid_uuid`).
  2. In one short transaction:
     - `SELECT 1 FROM vector_control.application_credentials
       WHERE id = $1 AND application_id = $2` — zero rows →
       404 `credential_not_found` (covers unknown ID **and** ID belonging
       to a different application; both are 404, no distinction).
     - `UPDATE vector_control.application_credentials
       SET enabled = false WHERE id = $1` — this is a **disable via
       UPDATE only**. The service never executes `DELETE` on
       `application_credentials` (Conflict C10); the `0004` DELETE grant
       is accepted as schema state and deliberately unused.
- Idempotent: re-disabling an already-disabled credential returns the
  same success.
- Response 200:

  ```json
  { "disabled": true }
  ```

- Disablement takes effect immediately for authentication (the unit 05
  lookup requires `enabled = true`); it deletes nothing — no application,
  namespace, or vector records are touched.

### Observations (no behavior change)

- The v1 API defines **no** endpoints to list applications/namespaces/
  credentials, to disable applications or namespaces, or to set credential
  `expires_at`. The corresponding schema columns exist and their checks
  are honored (e.g., unit 05 still enforces `expires_at`), but no v1 code
  path writes them except with defaults/`NULL`. Do not invent endpoints.
- `applications.enabled` and `namespaces.enabled` therefore remain `true`
  for the life of v1 unless changed out-of-band; the data-plane checks on
  those flags (units 04/07) remain in place because disabling may happen
  operationally and future API additions are compatible per
  `docs/API.md`.

## Interfaces and boundaries

- `internal/control` package owns all four handlers and the application
  key → UUID resolution helper. It is the only package that writes to the
  three control tables.
- Consumes: `auth.CheckAdmin`, `auth.GenerateCredential` (unit 05);
  `isolation.WithAppContext` (unit 04); `apierr` and request-ID plumbing
  (unit 01); strict-JSON helpers (unit 09).
- Does **not** accept an application UUID or key in a request body for
  data-plane purposes; the `{application}` path key is admin-plane
  identity for these four endpoints only, per the API contract.
- Response field sets match `docs/API.md` exactly; no extra fields.

## Invariants and correctness constraints

- **Admin/data-plane auth disjointness** (Global Invariant 9): an
  application credential sent to an admin endpoint is rejected 401; the
  admin token sent to a data-plane endpoint is rejected 401 (it is not a
  valid credential digest and the lookup fails).
- **Raw credential exists exactly once** (Global Invariant 8): generated
  in the request, hashed, INSERTed as digest, returned in the one
  response body, then discarded. No log line, no error message, no
  context value ever contains it.
- **Digest rule:** 32-byte `SHA-256` of the full raw string; the schema
  CHECK enforces the length server-side.
- **Namespace INSERT is context-bound** (Conflict C9): the INSERT runs
  only inside `WithAppContext(appID)`; a review must confirm no code path
  writes `namespaces` outside that helper.
- **No row deletion of credentials** (Conflict C10): disablement is
  `UPDATE … SET enabled = false`; repeated disablement is idempotent.
- **Immutability:** `application_key` and `vector_space` rows are never
  updated after creation; the only UPDATEs this unit issues are
  credential disablement. (`updated_at` triggers fire as a consequence of
  those UPDATEs; that is schema behavior.)
- **Two-layer namespace isolation preserved:** registration writes
  `application_id` relationally and the RLS `WITH CHECK` independently
  verifies it; both layers remain.
- **Errors:** `23505` → 409 `conflict` with a message naming the
  constraint target generically (e.g., "application key already
  registered") — never raw PostgreSQL text. All other SQL errors → 500
  `internal` (logged with detail) or 503 `unavailable` on pool failure
  (unit 01 mapping).
- **Bounded input:** every string field has the length limits above;
  bodies are subject to `VEC_HTTP_MAX_BODY_BYTES` (unit 09).

## Expected implementation surface

```text
internal/control/control.go        # router wiring, shared validation, app lookup
internal/control/applications.go   # register application
internal/control/namespaces.go     # register namespace (WithAppContext path)
internal/control/credentials.go    # create + disable credential
internal/control/*_test.go
```

## Validation

- Unit tests (no database), table-driven:
  - application-key grammar table: valid keys; uppercase; leading digit;
    64 vs 65 chars; empty — mapped to `invalid_application_key`;
  - namespace-key table: empty; contains `/`; control character; over
    length — `invalid_namespace_key`;
  - strict decoding: unknown field in any of the four bodies →
    `unknown_field`; missing `application_key` / `display_name` /
    `credential_name` → `missing_field`; non-object metadata →
    `metadata_not_object`; oversized metadata → `metadata_too_large`;
  - malformed `{credential_id}` → `invalid_uuid`.
- Integration tests (real PostgreSQL + pgvector; unit 10 infrastructure),
  with an admin token:
  - register application → 201 with UUID; re-register same key →
    409 `conflict`;
  - register namespace under app A → 201; same key under app B → 201
    (distinct namespace, per DATA_MODEL); same key under app A again →
    409;
  - **RLS proof for C9:** attempt the namespace INSERT SQL directly in a
    transaction *without* `set_config` → rejected by the policy (assert
    the service path would have failed); the service path succeeds;
  - create credential → 201; stored `credential_hash` is 32 bytes and
    equals `SHA-256(raw)` of the returned credential; a second request
    cannot recover the raw value (no endpoint returns it again);
    duplicate `credential_name` under same app → 409; same name under
    different app → 201;
  - the returned credential immediately authenticates data-plane requests
    as the correct application (cross-check with unit 05 integration
    harness);
  - disable credential → `{"disabled": true}`; second disable → same
    response; the credential row still exists with `enabled = false`; the
    credential no longer authenticates (401);
  - disable with unknown ID → 404 `credential_not_found`; disable an ID
    that belongs to another application → 404 (same code);
  - application key in path: valid grammar but unregistered →
    404 `application_not_found`; malformed grammar → 400.
- Negative isolation checks (unit 10 matrix consumes these):
  - an application credential (not the admin token) on any admin
    endpoint → 401;
  - the admin token on a data-plane endpoint → 401.

## Out of scope

- Data-plane endpoints (units 07–08).
- Any endpoint not in `docs/API.md` (listing, application/namespace
  disablement, vector-space management, credential expiry setting).
- Rate limiting, audit logs of admin actions beyond the structured access
  log, and multi-admin roles.
- TLS termination specifics (unit 02/11).

## Open issues

None new. Conflicts C9 and C10 are restated above and remain the
governing constraints. The absence of v1 endpoints for
application/namespace disablement and credential expiry is an observation
about the API contract, not an unresolved decision: implement exactly the
four endpoints.
