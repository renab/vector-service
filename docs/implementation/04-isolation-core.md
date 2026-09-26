# Unit 04 — Isolation Core: Application Context and Namespace Resolution

## Objective

Provide the single, mandatory path through which every application-scoped
database operation executes: a transaction helper that begins a
PostgreSQL transaction, establishes transaction-local application context
via `set_config(..., true)` as the **first** statement, resolves the
namespace within the application, and runs the operation's SQL inside that
same transaction. This unit is the enforcement point for application
isolation, namespace isolation, and the RLS fail-closed contract.

## Authority

- `docs/ARCHITECTURE.md` — request isolation flow; two-layer enforcement
  (service authorization + RLS); fail-closed philosophy.
- `docs/SECURITY.md` — `SET LOCAL`/`set_config` with `true`; transaction
  boundary ordering; fail-closed behavior; pooled-connection safety;
  uniform namespace failures.
- `docs/API.md` — namespace isolation (404 uniform for missing/disabled/
  foreign), PostgreSQL isolation steps, no default or wildcard namespace.
- `migrations/0001_shared_vector_schema.sql` — **forced** RLS on
  `vector_data.vector_records` and `vector_control.namespaces` with
  `USING`/`WITH CHECK` on
  `application_id = NULLIF(current_setting('vector.application_id', true), '')::uuid`;
  `namespaces` has `UNIQUE (application_id, namespace_key)`, an `enabled`
  column, and `ON DELETE RESTRICT` FK to `applications`.
- `docs/implementation/README.md` — Conflict C1 (ordering), Global
  Invariants 1–5.

## Dependencies

- Unit 02 (`DB`/pool, bounded acquisition).
- Unit 05's authenticator is the *caller* of this helper but is not a
  dependency for the helper itself: unit 04 exposes
  `WithAppContext(ctx, applicationID, fn)` and does not know how the
  application identity was obtained. Unit 05 calls it (or unit 04's
  request pipeline) after resolving the credential.

## Scope

1. **Transaction helper** (`internal/isolation`, or an extension of
   `internal/database` — implementer's choice; one package owns this):

   ```go
   WithAppContext(ctx context.Context, appID uuid.UUID,
       fn func(tx *pgx.Tx) error) error
   ```

   Behavior, in exact order:

   1. Acquire a connection from the pool with the bounded deadline
      (unit 02). Failure → `unavailable` (503).
   2. `BEGIN` (via `pool.BeginTx(ctx, ...)`; `AccessMode` default
      read/write, `IsoLevel` `ReadCommitted`).
   3. **First statement of the transaction**:
      `SELECT set_config('vector.application_id', $1, true)` with the
      application UUID as a parameter. The `true` (transaction-local)
      argument is mandatory and must never change. If this statement
      fails, roll back and fail the request; **no other statement may
      precede it**.
   4. Execute `fn(tx)`. All business SQL for the request runs only through
      the provided `tx` — the pool must not be used inside `fn`.
   5. `COMMIT` on success; `ROLLBACK` on any error. The transaction-local
      setting dies with the transaction, so returning the connection to
      the pool is always safe; no session residue is possible.

   The helper is the **only** code path that may call
   `set_config('vector.application_id', ...)`. No other package may set
   that GUC, and it must never be set outside a transaction (session-level
   context is prohibited on pooled connections).

2. **Namespace resolution** (same transaction, after `set_config`):

   ```sql
   SELECT id, enabled
   FROM vector_control.namespaces
   WHERE application_id = $1 AND namespace_key = $2;
   ```

   - The explicit `application_id = $1` predicate is the service-level
     enforcement; forced RLS on `namespaces` independently hides foreign
     rows. Both layers must remain.
   - Outcomes, all uniform to the caller:
     - zero rows → `namespace_not_found` (404);
     - row with `enabled = false` → `namespace_not_found` (404)
       (disabled namespaces permit no data-plane operation,
       `docs/SECURITY.md`);
     - row with `enabled = true` → proceed with the namespace UUID.
   - There is no code path that treats an empty, `*`, `all`, or missing
     namespace key as "everything". A missing `{namespace}` path segment is
     a routing 404 before this code runs.
   - The resolved namespace UUID and application UUID are attached to the
     request context for logging (unit 01 fields) and for the business SQL
     in `fn`.

3. **Request pipeline shape** (data plane, consumed by units 05–08):

   ```text
   authenticate credential            (unit 05, outside the transaction)
     → application UUID (fail → 401)
     → WithAppContext(appID, func(tx) {
           ns := resolveNamespace(tx, appID, key)   (fail → 404)
           ...operation SQL on tx...
         })
     → commit / rollback
   ```

   Authentication (credential digest lookup) happens **before** `BEGIN` on
   its own short-lived transaction, because `application_credentials` and
   `applications` carry no RLS and are admin-visible control-plane tables.
   The transaction containing `set_config` contains the namespace
   resolution and all data-plane SQL. (Conflict C1: the conceptual
   "resolve namespace before BEGIN" in the docs is impossible under forced
   RLS on `namespaces`; this ordering is mandatory.)

4. **Fail-closed guarantees.**
   - If the application context is missing or invalid, both RLS policies
     evaluate `NULLIF(..., '')::uuid` to `NULL` and match **no** rows. The
     service additionally refuses to run any `fn` without a context.
   - If `set_config` fails, the request fails; there is no fallback to an
     unscoped query, ever.
   - Unexpected SQL errors inside `fn` propagate as errors and trigger
     `ROLLBACK`; they are classified by unit 01's `apierr` rules.

5. **Read access to vector spaces.** `vector_control.vector_spaces` has
   **no RLS** and `vector_api` holds `SELECT` (migration `0004`). Vector
   space lookups (units 07–08) may run inside the same transaction; no
   application context is required for them, but they still execute within
   the request transaction for consistency.

## Interfaces and boundaries

- `isolation.WithAppContext(ctx, appID, fn) error` — the only sanctioned
  way to run application-scoped SQL.
- `isolation.ResolveNamespace(tx, appID, key) (uuid.UUID, bool, error)` —
  returns the namespace UUID; `ok=false` maps to `namespace_not_found`.
  (Exact signature is the implementer's; the *semantics* — uniform not-found
  for missing/disabled/foreign — are fixed.)
- Context accessors (unit 01): application UUID and namespace UUID are
  retrievable from `context.Context` for logging by later units.
- The pool and its connections carry no application context outside a
  `WithAppContext` transaction (unit 02 invariant preserved).

## Invariants and correctness constraints

- **Ordering is fixed**: `BEGIN` → `set_config(…, true)` → namespace
  resolution → business SQL → `COMMIT`/`ROLLBACK`, one transaction.
- **`set_config` is the first statement** of every application-scoped
  transaction, with the `true` argument.
- **Uniform 404**: missing, disabled, and foreign namespaces are
  indistinguishable in responses and (by default) in logs beyond the
  standard 404 access log line.
- **Two-layer enforcement**: the `application_id` predicate in service SQL
  and RLS both remain; neither may be removed "because the other
  suffices".
- **No session context**: no `SET` (non-LOCAL) of `vector.application_id`
  anywhere; no setting the GUC on a pooled connection outside a
  transaction.
- **No cross-namespace SQL**: a single request transaction operates on
  exactly one resolved namespace (deletes, upserts, and searches in units
  07–08 all filter by the resolved `namespace_id`).
- **Pooled-connection hygiene**: every path returns the connection to the
  pool only after `COMMIT` or `ROLLBACK`.
- **Cancellation**: `ctx` cancellation rolls back the transaction and
  releases the connection; no context outlives the request.

## Expected implementation surface

```text
internal/isolation/isolation.go      # WithAppContext
internal/isolation/namespace.go      # ResolveNamespace
internal/isolation/*_test.go
```

## Validation

- Unit tests (no database):
  - helper contract tests with a fake transaction layer: `set_config` is
    issued first; failure of `set_config` prevents `fn` from running and
    rolls back; `fn` error → rollback; success → commit.
  - namespace resolution mapping: zero rows / disabled row / enabled row
    → correct outcomes; no "all namespaces" branch exists (code review).
- Integration tests (real PostgreSQL + pgvector; unit 10 infrastructure):
  - RLS fail-closed: a raw query on `vector_records` in a transaction
    **without** `set_config` returns zero rows (defense-in-depth check);
  - `set_config(…, true)` set for application A, then a *new* transaction
    on the same pooled connection sees no context (transaction-local
    proof);
  - namespace resolution: missing key → 404; disabled namespace → 404;
    key owned by another application → 404 (uniform, same response body
    code);
  - after `WithAppContext` completes, a subsequent unrelated transaction on
    the reused connection has empty `current_setting('vector.application_id',
    true)`.

## Out of scope

- Credential authentication itself (unit 05).
- Any `/v1` endpoint, vector-space resolution, vector validation, search
  SQL (units 07–08).
- Admin operations (unit 06) — they reuse `WithAppContext` where the
  operation targets namespace-scoped state (Conflict C9), but admin
  application/credential endpoints that touch non-RLS control tables do
  not require it.

## Open issues

None. Conflict C1 is resolved by the mandatory ordering above; no other
authority requires a different order.
