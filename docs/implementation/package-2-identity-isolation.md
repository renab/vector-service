# Package 2 — Identity and Isolation

## Objective

Deliver everything that turns an HTTP request into an isolated,
RLS-protected database transaction: credential authentication for the
data plane, token authentication for the admin plane, the two
transaction helpers that own every database transaction the service
runs, and namespace resolution. After this package, an authenticated
request can run any SQL inside a transaction where forced RLS
establishes exactly the caller's application scope.

## Authority

- `docs/SECURITY.md` — opaque high-entropy credentials, digests only,
  uniform authentication failures, distinct admin authentication,
  transaction-local application context, structured auth-failure logs.
- `docs/API.md` — data-plane `Authorization: Bearer` semantics,
  authentication-failure shape (401 — the API's 401 list explicitly
  includes an **expired** credential), namespace-scoping rules and the
  uniform 404 for missing / disabled / foreign namespaces.
- `docs/ARCHITECTURE.md` and `AGENTS.md` — application identity from
  credentials only; namespace isolation; the `set_config(..., true)`
  rule; no `BYPASSRLS`.
- `migrations/0001_shared_vector_schema.sql` — forced RLS on
  `vector_records` and `namespaces` with policies keyed on
  `current_setting('vector.application_id', true)`; the `namespaces`
  policy's `WITH CHECK` clause (which makes the package-4 admin
  namespace-registration insert require an app context).
- `migrations/0004_runtime_identity.sql` — the
  `vector_control.application_credentials` table (the `credential_hash`
  digest column, `enabled`, `last_used_at`), the
  `vector_control.applications` table, and the exact `vector_api`
  grants.
- Package 1 — pool, bounded acquisition, configuration.
- Overview — invariants 1, 2, 3; error model (transaction-helper
  boundary).

## Scope

### 1. Credential model

- Credentials are opaque, high-entropy, machine-generated secrets with
  a stable textual shape (proposal: `vsvc_` prefix followed by a
  base-encoded random payload of at least 256 bits, generated with
  `crypto/rand`). Generation is deterministic in shape, random in value;
  no timestamps or counters in the payload.
- **Only the SHA-256 digest** of the credential is persisted —
  `vector_control.application_credentials.credential_hash` (32 raw
  bytes) — with `credential_name`, `enabled`, `last_used_at`. The raw
  credential is produced exactly once at creation (returned by the
  package-4 admin endpoint) and is never stored, logged, or
  reconstructable.
- Digest comparison in SQL is by equality on the digest value — the
  32-byte SHA-256 digest is bound as a `bytea` parameter against
  `credential_hash` — no constant-time requirement applies to the
  database lookup (the lookup is a keyed row fetch), but the raw secret
  is never compared in Go and never logged.

### 2. Data-plane authentication

- Middleware for every data-plane route. Header grammar: exactly
  `Authorization: Bearer <credential>`. Missing header, wrong scheme,
  or empty token → uniform 401 `unauthorized`.
- **Digest lookup** (short transaction, runtime role, bounded
  acquisition): fetch the credential row by `credential_hash`, join
  `vector_control.applications`, and require credential `enabled`,
  credential **not expired**, and application `enabled` — all in the
  one keyed lookup. The SQL contract, normative: the lookup predicate
  is the digest equality **and**

  ```sql
  (c.enabled
   AND (c.expires_at IS NULL OR c.expires_at > now())
   AND a.enabled)
  ```

  with the digest and no other caller-derived value bound as
  parameters; `now()` is evaluated server-side. Semantics: a `NULL`
  `expires_at` is a non-expiring credential (the schema default); a
  past — or exactly-now — `expires_at` is expired. Expiry is checked
  **only** in this lookup; there is no second expired-credential code
  path and no separate "expiry" query. Every failure shape — unknown
  digest, disabled credential, **expired credential**, disabled
  application — returns the **same** 401 `unauthorized`; no response,
  log line, or error message enumerates which kind failed. The
  auth-failure log line carries no identity (there is none), no
  token, and no digest.
- A lookup failure that is database-infrastructure (not an unknown
  credential) is `unavailable` (503) while the request context is
  active; an inactive context settles by the shared cause rule
  (overview error model): the request's settlement state is read —
  `service` outcome, or no outcome with the process-level shutdown
  marker set → `unavailable` (503); `client` outcome (or no outcome
  with the marker unset) → the abort sentinel; a `response` outcome
  means the committed status stands.
- **On success:** the authenticated application identity (application
  UUID) is attached to the request context and to the per-request log
  state (`application_id`). This is the only source of application
  identity for data-plane handlers (invariant 1).
- **`last_used_at` (best-effort, one auth concern):** after a
  successful lookup, if the stored `last_used_at` is older than a
  throttle window (proposal: 60 s), run `UPDATE
  vector_control.application_credentials SET last_used_at = now()
  WHERE id = $id` in its **own** short transaction. A failure of this update is
  warn-logged only — it never fails, delays, or changes the outcome of
  the request. The throttle window prevents write amplification on hot
  credentials.

### 3. Admin authentication

- Middleware for the four `/v1/admin/...` routes. Header grammar as
  above; the token is compared **constant-time**
  (`crypto/subtle.ConstantTimeCompare`) against the configured
  `VEC_ADMIN_TOKEN`. Missing / malformed / wrong → uniform 401
  `unauthorized`. No database access on this path.
- **Exactly one call site** invokes the admin check. Admin
  middleware places no identity in the request context — admin handlers
  resolve the named application from the `{application}` path key
  (package 4). Disjointness by construction: a data-plane credential
  presented to an admin route is compared to the admin token and fails;
  the admin token presented to a data-plane route is digest-looked-up
  and fails.

### 4. Transaction helpers (the isolation core)

Two helpers own **every** database transaction the service runs. All
other code executes SQL only inside them.

- **`WithAppContext(ctx, appID, fn)`** — the application-scoped
  transaction. Sequence, normative:

  1. acquire a pool connection (bounded acquisition; failure → typed
     outcome per below);
  2. `BEGIN`;
  3. **first statement:** `SELECT set_config('vector.application_id',
     $appID, true)` — transaction-local; the UUID is a bound parameter;
     the third argument stays `true`;
  4. run `fn(tx)` (namespace resolution and all business SQL run here);
  5. `COMMIT` (or `ROLLBACK` on `fn` error).

  The request context (deadline-bounded, package 4) is passed to every
  pgx call; cancellation rolls the transaction back and releases the
  lease.
- **`WithShortTx(ctx, fn)`** — a context-less short transaction for
  paths that touch no RLS table as a caller (the credential digest
  lookup, admin lookups/inserts that are not namespace registration).
  No `set_config` is executed. Same lifecycle and failure semantics.
- **Exception (driven by the schema):** the admin **namespace
  registration** insert (package 4) runs inside `WithAppContext(appID)`
  — not `WithShortTx` — because the forced RLS policy on `namespaces`
  carries a `WITH CHECK` on the application context that rejects an
  insert performed without it. This is the only admin-plane operation
  that uses the app-context helper.
- **Failure semantics (the helper boundary, per the overview):**
  outcomes are checked in this order at every failure point:
  1. effective context no longer active → settle by the **shared cause
     rule** (overview error model; implemented once, shared with the
     package-4 classifier), which reads the request's settlement state
     (package 4, section 2) through its synchronization: `client`
     outcome, or no outcome with the shutdown marker unset → the
     **abort sentinel** (internal control outcome; the renderer settles
     it — no response is written); `service` outcome, or no outcome
     with the process-level shutdown marker set → typed `unavailable`
     (503); `response` outcome → the committed status stands (no
     further response);
  2. any other infrastructure failure — bounded-acquisition failure,
     `BEGIN`, `set_config`, `COMMIT`, or `ROLLBACK` failure → typed
     `unavailable` (503).
  A business error returned by `fn` passes through the helper
  **unchanged** (classified exactly once at the handler boundary,
  package 4). A `ROLLBACK`/cleanup failure is warn-logged and attached
  to the original error; it never replaces it. A `COMMIT` failure after
  a successful `fn` is itself the helper outcome (rule 1, then rule 2).
- **No second transaction mechanism:** no `db.Begin` outside these two
  helpers; no goroutine holds a transaction across a handler boundary;
  each request runs its own transactions and releases every lease on
  every exit path (the bounded shutdown, package 4, depends on this).

### 5. Namespace resolution

- Runs **inside** `WithAppContext`, before any business SQL: resolve
  the `{namespace}` key against `vector_control.namespaces` for the
  authenticated application, requiring `enabled`.
- No row, disabled row, or foreign row (impossible to read under RLS,
  but the predicate includes `application_id` regardless) → uniform
  404 `namespace_not_found`. This is the documented data-plane
  behavior; the RLS policy enforces the same scope independently.
- On success the namespace UUID and key are set on the per-request log
  state and made available to the operation (bound as a parameter by
  package-3 SQL).
- **Ordering note (settled):** authoritative docs describe namespace
  resolution conceptually before the write transaction. Under forced
  RLS on `namespaces` (migration `0001`), a pre-transaction lookup as
  `vector_api` would see no rows. The schema is the immutable
  authority: the canonical order is **authenticate (short tx) →
  `WithAppContext` (BEGIN + `set_config`) → resolve namespace →
  business SQL**, all of the last three in one transaction.

## Interfaces and boundaries

- `internal/auth` — credential generation, header parsing, digest
  lookup, admin check, the data-plane auth middleware, the admin auth
  middleware, `last_used_at` update.
- `internal/dbctx` (or equivalent) — `WithAppContext`, `WithShortTx`,
  the typed helper outcomes, the abort sentinel, and the shared cause
  rule (evaluation of the request's settlement state — first claim
  wins — with the process-level shutdown marker as fallback; shared
  with the package-4 classifier/renderer, which own the settlements'
  origins).
- `internal/namespaces` (or inside `internal/dbctx`) — namespace
  resolution.
- Consumes: package 1 (pool, bounded acquisition, config).
- Exposes: authenticated identity on the request context; the two
  helpers; namespace resolution; the admin middleware. No route
  registration (package 4).

## Invariants and correctness constraints

- Invariant 1: no data-plane path accepts a caller-supplied application
  ID; identity flows only from the authenticated credential.
- Invariant 2: namespace scope on every data-plane operation; uniform
  404; no wildcard or default namespace behavior.
- Invariant 3: `set_config('vector.application_id', $uuid, true)` is the
  **first statement** of every `WithAppContext` transaction; it is
  transaction-local; no session-persistent context exists on pooled
  connections; no code path runs business SQL as `vector_api` without
  an app-context transaction.
- Uniform 401 for every data-plane authentication failure — unknown
  digest, disabled credential, **expired credential**, disabled
  application (one code, one status, no enumeration); expiry is part
  of the digest lookup (`expires_at IS NULL OR expires_at > now()`),
  never a separate path. Uniform 404 for every namespace failure; the
  two planes' credentials are disjoint (invariant via separate
  middleware, separate credential spaces).
- `last_used_at` is best-effort only: throttled, isolated in its own
  short transaction, failure-irrelevant to the request.
- Every helper exit path releases its pool lease and rolls back on
  error — the bounded shutdown's pool-close bound (package 4) assumes
  no leaked leases.
- No raw credential, digest, or admin token in logs, errors, or
  responses (invariant 12).

## Validation

**Unit (no database), table-driven:**

- Credential generation: shape, minimum entropy, uniqueness across
  many generations, round-trip digest (same input → same digest), raw
  value never equals the stored digest.
- Header parsing: missing header, wrong scheme, empty token, valid
  form.
- Admin check: exact match, mismatch, empty configured token rejected
  at startup (configuration), constant-time comparison used.
- Helper control flow against a stubbed transaction seam: context
  inactive at acquire with a `client` outcome → abort sentinel;
  inactive with a `service` outcome → `unavailable`; inactive with no
  outcome and the process-level shutdown marker set → `unavailable`;
  inactive with no outcome and no marker (the markerless transport
  cancel) → abort sentinel; `BEGIN` failure (active context) →
  `unavailable`; `fn` error → original error passes through with
  rollback; commit failure → typed outcome;
  rollback-failure-after-fn-error → original error retained; lease
  released on every path.

**Integration (real PostgreSQL + pgvector, 5a harness):**

- Credential lookup matrix (the real-PostgreSQL uniform-401 matrix):
  unknown digest → 401; disabled credential → 401; **expired
  credential → 401**; disabled application → 401; valid → identity
  attached. The expired fixture is a directly-updated past
  `expires_at` on a known-good credential (the service exposes no
  expiry-setting endpoint, so the fixture writes the column as
  `vector_api`, which holds `UPDATE` on the table); the complementary
  fixtures prove the gate does not over-reject — a **future**
  `expires_at` authenticates normally, and a `NULL` `expires_at`
  remains the non-expiring default. All **four** 401 responses are
  **byte-identical in code and message**; each fixture differs from
  the valid baseline in exactly the field under test.
- **RLS active, no context:** as `vector_api`, a raw transaction with
  **no** `set_config` → `SELECT` on `vector_records` and `namespaces`
  returns 0 rows; `INSERT` into `vector_records` is rejected by the
  policy's `WITH CHECK` (this is isolation-matrix case 7, package 5b —
  the capability is exercised here first).
- **`set_config` transaction scoping:** open an app-context
  transaction for app A, abort it mid-transaction, then on the same
  pooled physical connection run one for app B → B sees no A context
  (transaction-local semantics, matrix case 8).
- **Pooled connection reuse:** on one shared pool, interleave A
  upsert-eligible reads, B reads, A reads enough to force physical
  connection reuse → each request sees exactly its own application's
  rows (matrix case 8).
- Namespace resolution: missing key → 404; disabled namespace → 404;
  another application's namespace key (same key, foreign app) → 404;
  the three responses are uniform.
- `last_used_at`: first auth updates it; a second auth within the
  throttle window does not issue the UPDATE (observed via
  `last_used_at` unchanged / query-count seam); making the UPDATE fail
  (test seam) leaves the request succeeding.
- Admin namespace-registration insert inside `WithAppContext` succeeds
  where the same insert in `WithShortTx` is rejected by RLS (proves the
  `WITH CHECK` requirement).

## Out of scope

- HTTP transport, routing, rendering, request IDs, access logging
  (package 4).
- Vector-space resolution and all vector operations (package 3).
- Admin endpoint handlers beyond what is needed for the auth middleware
  (package 4).
- Cross-namespace retrieval of any kind (explicitly not a capability;
  would require a separate architecture decision).

## Open issues

None.
