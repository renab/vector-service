# Package 5 — Verification and Runtime

Three deliverables in dependency order:

- **5a** — the test harness: disposable real-PostgreSQL environments and
  shared fixtures for every package's integration tests. Lands
  immediately after package 1, before package 2 starts.
- **5b** — the two release-critical matrices (eight isolation cases from
  `docs/DEVELOPMENT.md`; six migration cases from `docs/MIGRATIONS.md`),
  the release-critical logging regression matrix (raw PostgreSQL driver
  text never reaches structured logs), and the full-path HTTP
  integration layer. Completes with the last package.
- **5c** — the container image and deployment shape. Documentation plus
  `Dockerfile`/build inputs only; it adds no Go code beyond what
  packages 1–4 already require.

## Objective

- **5a** provides the only test-database setup mechanism in the codebase
  (DEVELOPMENT.md: "Avoid maintaining a second unrelated schema-setup
  mechanism"; "Tests and production should exercise the same
  migrations").
- **5b** owns the cross-package verification that gates release: the
  Required Isolation Tests (release-critical), the Testing Migrations
  matrix ("CI should test at minimum"), the release-critical logging
  regression matrix (package-4 diagnostic-record policy — the raw
  driver primary message is never logged by default, and no supplied
  payload, raw message, or digest ever reaches a structured log
  record), plus error-format conformance against `docs/API.md`.
- **5c** defines how the binary ships: multi-stage build, runtime image
  contents, entrypoint/subcommand behavior, bounded stop, and the
  canonical provisioning shape the image runs.

## Authority

- `docs/DEVELOPMENT.md` — test layers (unit / database integration /
  HTTP integration); "Do not mock PostgreSQL for behaviors that
  specifically depend on PostgreSQL semantics"; the eight-case Required
  Isolation Tests, marked release-critical; Docker build rules
  (multi-stage, minimal runtime image, non-root user); subcommand
  determinism ("no two independent migration runners").
- `docs/MIGRATIONS.md` — "Testing Migrations" (six cases); startup
  order; schema preconditions (database, pgvector binaries, runtime
  connection identity, TLS connectivity) are infrastructure-provided.
- `docs/SECURITY.md` — RLS is defense in depth that must be tested as
  active; runtime role never `BYPASSRLS`; no raw credentials in any
  output; private keys never persisted in image layers or logged.
- `docs/API.md` — endpoint behavior and error format conformance targets
  for the HTTP integration layer.
- Root `README.md` — the PostgreSQL **18** pin.
- `migrations/0001`–`0006` — the exact schema and privilege boundary
  under test: forced RLS policies, the validation trigger, the seeded
  space, the HNSW index, the `vector_api` grants (including the
  hardcoded `GRANT CONNECT ON DATABASE vector`), and the
  `0005`/`0006` privilege-boundary completion (direct function/type
  grants and the `PUBLIC` baseline revokes, including the implicit
  row types of the migrated tables).
- `AGENTS.md` — generic fixture names; obviously synthetic fixtures.
- Overview (this directory's `README.md`) — invariant 14 (real database
  in tests), conventions, error code catalog.
- Packages 1–4 — the behavior under test; specifically: the package-1
  production runner `migrate.Run` and its package-internal source seam,
  the migrations-converged flag, fail-fast configuration; the package-2
  credential generation in `internal/auth` and the `WithAppContext` /
  `WithShortTx` helpers; the package-3 operations and response shapes;
  the package-4 handler chain, `api.Classify` / `api.Render`, the
  diagnostic-record policy (the safe allowlist for DB-error logging —
  raw driver messages never logged by default), and the bounded-shutdown
  exit-status contract.

## Dependencies

- **5a:** package 1 (configuration surface, production migration
  runner, runtime pool construction).
- **5b:** packages 1–4 (the matrices exercise their behavior).
- **5c:** packages 1 and 4 (binary, subcommands, shutdown semantics)
  and 5a (the disposable-DB verification flow).
- A real PostgreSQL **18** instance with the pgvector extension
  available in the test environment. No authoritative source pins the
  **pgvector extension release**: the harness and CI pin a concrete
  release that supports the `hnsw` index type used by migration `0003`
  and record the choice in the test notes (infrastructure decision, not
  a contract claim).

## Scope

### 5a — Test harness

The harness is a Go test-helper package under `internal/` (e.g.
`internal/testdb` — `internal` placement so production packages can
never import it). It provisions, per test **suite** (package), a
disposable environment:

1. Connect as the test **bootstrap identity** (superuser or an
   infrastructure admin). This identity is **harness-only**: no service
   code path ever runs as it.
2. Create a database named **`vector`** — configuration validation and
   `0004`'s `GRANT CONNECT ON DATABASE vector` hardcode the name, so
   the test database must match.
3. In that database: `CREATE EXTENSION vector;` — no migration creates
   the extension (prerequisite validation, package 1).
4. Create the runtime role `vector_api` with `LOGIN` and no
   superuser/`BYPASSRLS` attributes — `0004`'s grants require the role
   to exist; `SECURITY.md` forbids `BYPASSRLS`.
5. Provision the **canonical shape only** (the two-identity decision —
   overview invariant 4; package 1): the harness creates the migration
   identity
   (a generated role, e.g. `vector_owner`) and makes it the **owner**
   of the `vector` database. The runner's bootstrap then creates
   `vector_control`, `schema_migrations`, and the schemas as that
   identity. Alternate provisioning shapes (pre-provisioned, explicit
   grants) are deliberately out of scope — the harness never builds or
   asserts them.
6. **Convergence is a separate, optional stage.** The harness exposes
   provisioning (steps 1–5) and convergence (`migrate.Run`, the
   production function the `migrate` subcommand invokes — one runner,
   embedded source) independently, so behavior tests start from a
   converged database while the migration matrix (5b) starts from an
   un-converged one and drives the runner itself.
7. Expose the full runtime configuration for the disposable database —
   a complete `VEC_*` environment (`VEC_PG_TLS_MODE=plain`, generated
   admin token, connection details) — so service-function tests and
   HTTP tests run against the same provisioned state.
8. Tear down at suite end: `DROP DATABASE`.

Application and namespace fixtures are created through the service's own
paths (fixture conventions below), never through harness DDL: the
harness provisions only the database, the extension, and the roles —
the same infrastructure boundary production has — plus the test-only
negative-identity variants below (rejection states only; they are not
alternate provisioning shapes and are never converged).

#### Negative identity variants (test-only)

The negative identity matrices (package 1, Validation — migration- and
runtime-identity assertions) need states in which the canonical
boundary is broken. The harness builds them as **mutations of the
canonical provisioning** by the bootstrap identity — one per test case,
in the disposable suite database, pre-convergence or post-convergence
as the case names it:

- **`vector_api`-owned schema:** `CREATE SCHEMA vector_control
  AUTHORIZATION vector_api` (or `vector_data`) pre-created so the
  runner's canonical-ownership gate finds a non-migration owner
  (pre-convergence variant); the same variant post-convergence is
  `ALTER SCHEMA <s> OWNER TO vector_api`;
- **`vector_api`-owned history table:** pre-created
  `vector_control.schema_migrations` with `AUTHORIZATION vector_api`
  (the schemas remain canonically owned, so the gate's history check
  is what fires);
- **`vector_api`-owned migrated table:** post-convergence
  `ALTER TABLE <migrated table> OWNER TO vector_api` (e.g.
  `vector_data.vector_records`) — the canonical-ownership gate
  (package 1, Scope 4) rejects it at the next `migrate`/`serve` pass
  before bootstrap, history read, or pool open; the runtime pool
  identity assertion independently forbids the same ownership;
- **`vector_api`-owned migrated function:** post-convergence
  `ALTER FUNCTION <migrated function> OWNER TO vector_api`, one form
  each for `vector_data.validate_vector_record()` and
  `vector_control.set_updated_at()` (the bootstrap identity performs
  the transfer) — gate rejection on `pg_proc.proowner`, as above;
- **unreachable-role-owned migrated table:** a fresh role `U` (no
  memberships, no privileges, no other ownership — unreachable from
  `vector_api`, so the runtime closure check cannot see it) receives
  post-convergence `ALTER TABLE vector_data.vector_records OWNER TO
  U` — the canonical-ownership gate must reject it on
  `pg_class.relowner`; the rejection is the gate's, proving the
  migration-identity ownership rule is independent of the runtime
  no-ownership rule;
- **unreachable-role-owned migrated function:** one fixture per
  migrated function, on a fresh unreachable role `U`: `ALTER
  FUNCTION vector_data.validate_vector_record() OWNER TO U` and
  `ALTER FUNCTION vector_control.set_updated_at() OWNER TO U` — gate
  rejection on `pg_proc.proowner` in each;
- **direct membership:** `GRANT <migration identity> TO vector_api
  WITH INHERIT FALSE, SET FALSE, ADMIN FALSE`;
- **transitive membership:** a fresh intermediate role `G` with
  `GRANT <migration identity> TO G WITH INHERIT FALSE, SET FALSE,
  ADMIN FALSE` and `GRANT G TO vector_api WITH INHERIT FALSE, SET
  FALSE, ADMIN FALSE`;
- **extra direct grant:** a grant outside the canonical
  `0004`–`0006` set on an in-scope object, e.g. `GRANT SELECT ON
  vector_control.schema_migrations TO vector_api` (canonically none)
  or `GRANT TRUNCATE ON vector_data.vector_records TO vector_api`
  (canonically absent);
- **inherited grant:** a fresh harmless role `G` (no privileges of
  its own) with `GRANT SELECT ON vector_control.schema_migrations TO
  G` and `GRANT G TO vector_api WITH INHERIT TRUE, SET FALSE, ADMIN
  FALSE`;
- **direct column grant:** a column-level grant beyond the table's
  canonical set — `GRANT SELECT (version) ON
  vector_control.schema_migrations TO vector_api` (the table is
  canonically none) and `GRANT REFERENCES (object_id) ON
  vector_data.vector_records TO vector_api` (canonically no
  `REFERENCES`);
- **inherited column grant:** a fresh harmless role `G` (no
  privileges of its own) with the same column-level grant (e.g.
  `GRANT SELECT (version) ON vector_control.schema_migrations TO G`)
  and `GRANT G TO vector_api WITH INHERIT TRUE, SET FALSE, ADMIN
  FALSE`;
- **`PUBLIC` column grant:** `GRANT REFERENCES (object_id) ON
  vector_data.vector_records TO PUBLIC` (canonically absent);
- **grant option (every ACL catalog):** one real-PostgreSQL fixture
  per in-scope ACL catalog, each carrying a grant option for a
  boundary grantee — column (stored in `pg_attribute.attacl`, not
  `pg_class.relacl`): `GRANT SELECT (version) ON
  vector_control.schema_migrations TO vector_api WITH GRANT OPTION`;
  database: `GRANT CONNECT ON DATABASE vector TO vector_api WITH
  GRANT OPTION`; schema: `GRANT USAGE ON SCHEMA vector_data TO
  vector_api WITH GRANT OPTION`; function: `GRANT EXECUTE ON
  FUNCTION vector_data.validate_vector_record() TO vector_api WITH
  GRANT OPTION`; type: `GRANT USAGE ON TYPE
  vector_control.distance_metric TO vector_api WITH GRANT OPTION`;
  and table: `GRANT SELECT ON vector_control.applications TO
  vector_api WITH GRANT OPTION` (plus a table-level grant-option
  variant on the canonical `vector_data.vector_records` table);
- **provenance substitution — inherited:** one fixture per canonical
  grant source: revoke a direct canonical grant from `vector_api`
  and grant the same privilege through a fresh harmless role `G`
  (no other privileges, no ownership, no privileged attributes) with
  `GRANT G TO vector_api WITH INHERIT TRUE, SET FALSE, ADMIN
  FALSE`, so the effective set — every `has_*` predicate — is
  unchanged, e.g. `REVOKE CONNECT ON DATABASE vector FROM
  vector_api; GRANT CONNECT ON DATABASE vector TO G; GRANT G TO
  vector_api WITH INHERIT TRUE, SET FALSE, ADMIN FALSE` (plus one
  form each for a schema `USAGE`, a table
  privilege, a function `EXECUTE`, and a type
  `USAGE`);
- **provenance substitution — inherited (column-derived):** a
  table-level provenance substitution on a column-relevant table
  (`vector_data.vector_records` with `SELECT`), which replaces the
  canonical table-level direct grant with an inherited grant through
  a fresh harmless role `G`. Canonical migrations grant only at table
  level (`pg_class.relacl`); there is no per-column ACL
  (`pg_attribute.attacl`). PostgreSQL derives column-level
  `has_column_privilege()` results from the table's `relacl` when no
  per-column ACL exists. The assertion code's column-level privilege
  grid (the `has_column_privilege()` predicates for each column of
  each in-scope table) therefore resolves identically before and after
  the substitution, because the inherited table-level grant still
  covers every column. The rejection fires on the **table-level**
  provenance check: the column ACL catalog (`pg_attribute.attacl`)
  carries no `required` provenance field — column provenance is
  satisfied by the table's provenance. This fixture proves the column
  `has_*` grid is exercised alongside the table provenance gate,
  confirming the assertion pipeline does not skip column-level
  predicate evaluation when the table-level provenance failure is
  detected. The implementation uses `REVOKE SELECT ON
  vector_data.vector_records FROM vector_api; GRANT SELECT ON
  vector_data.vector_records TO G; GRANT G TO vector_api WITH
  INHERIT TRUE, SET FALSE, ADMIN FALSE` — a table-level operation
  that affects column predicates.
- **provenance substitution — `PUBLIC`:** one fixture per canonical
  grant source: revoke a direct canonical grant from `vector_api`
  and grant the same privilege to `PUBLIC`, leaving the effective
  set unchanged, e.g. `REVOKE USAGE ON SCHEMA vector_data FROM
  vector_api; GRANT USAGE ON SCHEMA vector_data TO PUBLIC` (plus one
  form each for a table privilege, a function
  `EXECUTE`, and a type `USAGE`) — canonically `PUBLIC` carries
  only the preserved `CONNECT` on the database `vector`;
- **provenance substitution — `PUBLIC` (column-derived):** a
  table-level provenance substitution on a column-relevant table
  (`vector_data.vector_records` with `SELECT`), which replaces the
  canonical table-level direct grant with a `PUBLIC` grant. Same
  semantics as the inherited column-derived variant: the column
  `has_column_privilege()` predicates resolve identically (PUBLIC
  covers every column), the rejection fires on the table-level
  provenance check, and the fixture proves the column `has_*` grid is
  exercised in the assertion pipeline. The implementation uses
  `REVOKE SELECT ON vector_data.vector_records FROM vector_api;
  GRANT SELECT ON vector_data.vector_records TO PUBLIC` — a
  table-level operation that affects column predicates.
- **`MAINTAIN` grant:** `GRANT MAINTAIN ON vector_data.vector_records
  TO vector_api` (PostgreSQL 18; canonically absent everywhere);
- **`set_option`-true membership:** a fresh harmless role `G` (no
  privileges, no ownership, no privileged attributes) with
  `GRANT G TO vector_api WITH INHERIT FALSE, SET TRUE, ADMIN FALSE`
  (direct — all membership flags set explicitly; `set_option` is the
  assumption right, so the zero-membership rule rejects it), and a
  two-hop variant through a fresh intermediate role `M` (`GRANT G TO
  M WITH INHERIT FALSE, SET TRUE, ADMIN FALSE; GRANT M TO vector_api
  WITH INHERIT FALSE, SET TRUE, ADMIN FALSE`) — both must be
  rejected even though the privilege grid stays unbroken;
- **`admin_option`-true membership (release-critical, ADMIN-only):**
  a fresh harmless role `G` (no privileges, no ownership, no
  privileged attributes) with
  `GRANT G TO vector_api WITH INHERIT FALSE, SET FALSE, ADMIN TRUE`
  (direct — all membership flags set explicitly) and a two-hop
  variant through a fresh intermediate role `M` (`GRANT G TO M WITH
  INHERIT FALSE, SET FALSE, ADMIN TRUE; GRANT M TO vector_api WITH
  INHERIT FALSE, SET FALSE, ADMIN TRUE`) — the edge conveys no
  privilege (it is not in the INHERIT closure) and permits no
  `SET ROLE` assumption (`set_option` false), but the admin option
  is the regrant authority (the member may grant, revoke, or alter
  that membership), so it must be rejected by the explicit
  `admin_option` check in the membership closure even though the
  privilege grid stays unbroken;
- **reachable privileged role:** a fresh role `H` that owns a
  migrated object (`ALTER TABLE vector_control.applications OWNER TO
  H`, or `ALTER FUNCTION vector_control.set_updated_at() OWNER TO H`
  for the function-ownership variant) plus `GRANT H TO vector_api
  WITH INHERIT FALSE, SET FALSE, ADMIN FALSE`; a fresh role `P`
  created `WITH BYPASSRLS` plus `GRANT P TO vector_api WITH INHERIT
  FALSE, SET FALSE, ADMIN FALSE`; a fresh superuser `S` plus
  `GRANT S TO vector_api WITH INHERIT FALSE, SET FALSE, ADMIN FALSE`;
- **runtime role attribute mutation:** `ALTER ROLE vector_api
  BYPASSRLS TRUE`.

These variants are **rejection fixtures only**: the runner's gate or
the runtime pool assertion must fail them (nothing converges from a
broken pre-convergence state; a broken post-convergence state is
detected at the next `migrate`/`serve` pass before serving or
bootstrap). The canonical shape (steps 1–5, unmodified) remains the
only positive provisioning path, and the harness never asserts a
variant as a viable deployment.

#### Test layers

| layer | runs against | purpose |
|-------|--------------|---------|
| unit | nothing (pure Go) | validation, codecs, config, error mapping, filter builder, score transforms, credential generation/parsing, request-ID rules, SQL builder placeholder ordering |
| database integration | real PostgreSQL + pgvector, via the service's own service/DB functions (not HTTP) | RLS behavior, isolation, upsert/delete/search/filter SQL, triggers, migration runner, credential lookup |
| HTTP integration | the production handler chain (full middleware) on an `httptest` server (test port) → auth → service → PostgreSQL | the full path for critical isolation endpoints and error-format conformance |

PostgreSQL-specific semantics (RLS, transaction-scoped `set_config`,
`ON CONFLICT`, `->>` JSONB lookup, `vector` operators, triggers,
advisory locks) are **never mocked** (DEVELOPMENT.md). The process
boundary (subcommand dispatch, env config loading, process exit codes)
is covered by package-1 tests and the 5c container verification, not
re-asserted per test.

#### Fixture conventions

- **Names:** generic only (AGENTS.md): applications
  `memory-service` / `notes-service` / `search-service`; namespaces
  `project-alpha` / `project-beta` / `research`. Obviously synthetic.
- **Vectors:** deterministic synthetic 1024-dim `float32` vectors for
  the seeded space — e.g. basis/unit vectors per record (nearest-
  neighbor outcomes exact and stable), scaled variants for
  distance-ranking tests. No real embeddings, no network fetches.
- **Metadata:** small fixed objects (`{"document_type":"note",
  "state":"active"}` style) so filter tests are exact.
- **IDs/credentials:** generated via the service's own functions
  (UUID, package-2 credential generation) so tests exercise production
  code paths; no hardcoded secrets.
- **Isolation fixtures:** a standard two-application, two-namespace
  fixture (app A: `project-alpha`; app B: `project-beta`; plus a second
  A-namespace `research` for the namespace-axis cases), created through
  the **admin API path** in HTTP tests and through the **service
  functions** in DB tests.
- Tests must be **re-runnable** (fresh database per suite) and
  **order-independent** (no shared mutable state between test functions
  beyond the suite database; per-test data uses distinct
  `object_id`s).

### 5b — Release-critical matrices and HTTP integration

#### Required isolation tests (release-critical)

The eight cases from `docs/DEVELOPMENT.md`, verbatim, each with the
concrete scenarios pinned here:

```text
1. application A cannot read application B
   - A: GET record (B's record UUID, A's namespace) → 404
   - A: search in A's namespace with query vector equal to B's stored
     vector and a filter matching only B's metadata → matches contain
     no B rows
2. application A cannot search application B
   - same search as (1); additionally B's records are in a *different
     namespace of A* → still invisible (namespace axis independent of
     the application axis)
3. application A cannot delete application B
   - A: DELETE projection of B's (object_id, projection_id) in A's
     namespace → {"deleted": 0}; DELETE object of B's object_id →
     {"deleted": 0}; direct DB assertion: B's rows still present
4. application A cannot discover application B namespaces
   - A: any data-plane request with B's namespace key → 404 uniform
     namespace_not_found; there is no data-plane endpoint listing
     namespaces (assert by router test that none exists)
5. namespace X cannot accidentally expose namespace Y
   - same application, two namespaces: get/search/delete across the
     boundary → 404 / empty / {"deleted": 0}
6. missing namespace does not become all namespaces
   - request with a nonexistent namespace key → 404; direct DB
     assertion that no wildcard/all-namespace code path exists (no
     statement in the codebase queries vector_records without a
     namespace predicate — enforced by review, spot-checked by grep in
     CI if practical)
7. missing RLS context exposes no application records
   - as vector_api, open a transaction with NO set_config: SELECT on
     vector_records and namespaces → 0 rows; INSERT into
     vector_records → rejected by WITH CHECK (0 rows / error). This is
     a database-integration test, not an HTTP test
8. pooled connection reuse does not leak application identity
   - on one shared pool: A upsert → B upsert → A get (A sees only A);
     interleave enough requests that the pool must reuse physical
     connections; additionally: A's transaction aborts (forced error)
     and the *next* transaction on the same connection as B sees no A
     context (set_config is transaction-scoped — prove it)
```

All eight are **release-critical** (DEVELOPMENT.md): CI runs them on
every change; a red matrix blocks release. Cases 1–6 and 8 run through
the HTTP layer (production chain, real authentication); case 7 is
direct `vector_api` SQL against the suite database. The matrix is a
named, integration-tagged entry point (`TestIsolationMatrix…`) so CI
can target it explicitly.

#### Migration test matrix

The six cases from `docs/MIGRATIONS.md`, each run against a real
PostgreSQL + pgvector database (fresh per case, provisioned by 5a steps
1–5 without convergence):

```text
1. empty database → latest schema
   - fresh `vector` db, run the production runner → all six
     migrations applied, history rows present with checksums, schema
     objects exist (schemas, tables, RLS enabled+forced, policies,
     trigger, seeded space row, HNSW index), vector_api's effective
     privileges match the canonical set of 0004–0006 (the 0004 direct
     grants, the 0005/0006 direct function/type grants and PUBLIC
     baseline, the column-level, grant-option, and direct-grant
     provenance checks) — including
     an assertion that `vector_api` can open a connection to the
     database (proving `0004`'s
     `GRANT CONNECT ON DATABASE vector TO vector_api` executed)
   - **canonical owner assertion:** the migration identity owns the
     `vector` database, both migrated schemas, and every in-scope
     `pg_class` row (`relowner`), `pg_type` row (`typowner`), and
     `pg_proc` row (`proowner`) in them — the history table, every
     table, sequence, index, type, and both migrated functions —
     while `vector_api` owns none of them
2. previous released schema → latest schema
   - build the prior state from the immutable canonical migrations
     0001–0004 only: apply exactly those four files (byte-identical
     contents, through the package-internal source seam) with their
     history rows and checksums, so the database holds the released
     0001–0004 schema and nothing later; then run the production
     migration runner (`migrate.Run`, embedded source — the seam was
     used only to construct the prior state) → it applies exactly
     0005_runtime_privilege_boundary.sql followed by
     0006_runtime_privilege_boundary_completion.sql, in that order;
     afterward: the history holds all six canonical rows — the exact
     names and SHA-256 checksums of 0001–0006 (the
     migrations/README.md table) — 0001–0004 are not re-applied
     (their history rows unchanged), and vector_api's effective
     privileges match the canonical set of 0004–0006 (the 0005/0006
     direct function/type grants and the PUBLIC baseline revokes,
     including the implicit row types of the migrated tables
3. migration history checksum validation
   - (a) tamper a history row's checksum (direct fixture SQL as the
     migration identity) → re-run the production runner → fails
     fatally with a drift error, no migrations applied;
     (b) through the seam: a source whose file bytes differ from the
     recorded checksum → the same fatal drift behavior
4. repeated startup with no pending migrations
   - run migrate twice (and a third time) → second/third are no-ops,
     exit 0, history unchanged; serve startup after migrate → ready
5. concurrent migration attempts
   - two concurrent `migrate.Run` invocations (goroutines or
     processes) on one fresh database → both succeed (one applies,
     one waits on the advisory lock within VEC_MIGRATION_LOCK_WAIT and
     observes applied state); history has exactly one row per
     migration
6. failure behavior for invalid migrations
   - a test-only migration set (via the seam) containing a failing
     statement (syntax error mid-migration, or a statement rejected by
     the role) → runner returns failure, the failed migration has NO
     history row, the database schema is unchanged by the failed
     migration (transactional per-migration semantics, package 1), a
     corrected re-run applies cleanly;
   - **history-INSERT boundary** (failure at the runner's own history
     write, not at a body statement — package 1, Scope 4 test seam):
     a test-only migration whose DDL is valid is applied with the
     history `INSERT` rejected by a test-only `BEFORE INSERT` trigger
     on `vector_control.schema_migrations` (released migrations are
     never modified) → the DDL succeeds, the run fails, and a direct
     database assertion proves **both** the migration's DDL and the
     would-be history row rolled back (no object created, no history
     row); the failure fixture (trigger + function) is removed and the
     unchanged migration set is re-run → converges cleanly (exit 0,
     history row present, DDL object present)
```

**Placement.** The whole matrix is an integration-tagged test inside
the `internal/migrate` package: cases 2, 3(b), and 6 (both forms)
require the package-internal source seam (package 1, Scope 4), and
collocation keeps the release gate in one place. Cases 1, 3(a), 4, and 5 exercise
the production `migrate.Run` (embedded source) unchanged. There is no
exported global override, no mutable package-level source state, and no
build-tag variant of the runner; `migrate.Run` always uses the
embedded source. The suite is race-clean (`go test -race`) and leaves
no residual fixture state (fresh database per case, per-case teardown).

Package 1's own runner tests (empty → latest, repeated no-op, drift,
concurrency, failing statement) already exercise these behaviors
per-package; the matrix consolidates them under the six canonical case
names for the CI release gate. It complements, not replaces, the
per-package tests.

#### Logging regression matrix (release-critical)

A release-critical regression proving the package-4 diagnostic-record
policy against **real PostgreSQL + pgvector** (5a environment, never
mocked): the raw driver primary message is never logged by default, and
no supplied payload ever reaches a structured log record. The matrix
runs the production handler chain (full middleware) with a test-owned
`slog` JSON sink attached, so the captured records are exactly what
production would emit (access lines, diagnostic records, everything
else the process logs during the case).

**Payload markers.** The absence checks below are keyed to markers
derived per test at test time (the service's own randomness — no
hardcoded marker strings, no wall-clock dependence). A marker must be
**collision-resistant**: it must be a string that cannot naturally
occur in any value of the field kinds the service emits — `status`
(a 3-digit integer), `duration` (a single float), `count` (a small
integer), a UUID (version-form hex with hyphens), a SQLSTATE (a
5-character class code), a request ID, or any stable catalog code —
so that the check passes every compliant log and can fail only on a
genuine leak. Two marker families:

- **Malformed-literal marker (case 1):** a per-test token of the
  form `vsvlog-` + 32 random lowercase hex characters (16 bytes of
  entropy), placed as the **first element** of the crafted malformed
  `vector` literal (e.g. `"[vsvlog-<hex>, 2, 3]"`). pgvector rejects
  the literal with `22P02` and the server's raw message quotes the
  literal — so the raw driver message *contains* the token, which is
  what makes its absence from the logs a meaningful test of the
  policy. Because the token leads the literal, every prefix of the
  raw message that still carries supplied vector data carries the
  token, and every proper substring of the token is a proper prefix
  of it: the marker set below therefore also catches truncated forms
  of the token. The fixed `vsvlog-` prefix plus 128 bits of generated
  entropy makes natural occurrence in a status/duration/count/
  UUID/SQLSTATE/request-ID field impossible.
- **Vector payload markers (case 4):** the case vector is derived
  from the 5a deterministic synthetic base with **one generated
  distinctive element**. The distinctive element is a per-test
  float32 value produced by a deterministic reject-and-retry
  generator: generate candidate decimal tokens of the form
  `0.` + 15 random decimal digits; convert each candidate to
  `float32`; reject and retry if the result equals the base element
  `0.1` (in float32), or if the result is non-finite (NaN or
  ±Inf). The loop terminates on the first accepted candidate, which
  is the distinctive element. The marker is derived exclusively from
  the exact final float32 serialization through the production codec
  (`vectors.EncodeText` — `strconv.FormatFloat` with `%g` precision
  -1, bit-size 32), which produces the shortest decimal that
  round-trips bit-exactly. The distinctive property derives from the
  per-test random generation and the explicit rejection of the base
  element, not from any fixed digit pattern or assumed precision
  gap. From the exact values the test binds, it computes the
  vector's serialized text form (the pgvector text form
  `[a,b,c,...]` the package-1 codec round-trips) and keys two
  markers:
  - **complete form** — the full serialization; it contains `[`,
    `]`, and multiple commas, so it cannot be a substring of any
    status/duration/count/UUID/SQLSTATE field value;
  - **partial form** — a contiguous fragment of the serialization
    spanning at least three consecutive elements, containing at
    least one comma and the distinctive element's serialization; it
    carries the distinctive element's decimal rendering plus the
    comma structure, so it too cannot occur in any of those field
    values, and it is the fragment a truncated (partial) vector
    leak would carry.

**The absence check (defined once, applied by every case).** Given a
case's marker set and the records captured during the case: for
every field of every captured record, serialize the field's value to
the exact string the sink emits, and fail the test if any marker —
or any proper prefix of a marker of more than 8 characters (the
truncated-form rule above) — occurs as a substring of that
serialization. A full leak *and* a partial leak (a truncated digest
fragment, a quoted vector prefix, a truncated token) both fail the
test. The check is applied to the case's own capture window only
(canary records from the negative control below never enter it).
Marker sets per case:

- case 1: the malformed-literal token (with the truncated-prefix
  rule) and the raw `22P02` server message as captured by the
  database-layer fixture verification;
- case 2: the raw trigger message(s) (both forms) and the embedded
  space UUID;
- case 3: the metadata JSON object, each metadata value, and the
  per-test metadata marker;
- case 4: the complete serialization and the partial fragment;
- case 5: the raw credential, its hex digest, and each generated
  fragment of the two (per the 5a conventions, at least one 16-
  character fragment of each, derived from the generated values).

The long stable boilerplate of a driver message that carries no
supplied data (e.g. pgvector's fixed `invalid input syntax for type
vector:` lead-in, without the quoted literal) is deliberately **not**
in any marker set: it contains no caller value, and the stable
classification fields (`22P02`/`P0001` SQLSTATE, catalog code,
status) are asserted present, not absent.

Cases (one capture-and-assert unit each):

1. **Real `22P02` — malformed vector against pgvector.** A test-only
   handler seam (same pattern as the package-4 panic-injection test
   route) bypasses the Go-level vector pre-validation and forwards
   the crafted malformed `vector` literal — first element the
   per-test malformed-literal marker — to the database, so the real
   pgx `22P02` error reaches the classifier and its diagnostic
   record (the server's raw message quotes the literal, so the raw
   message contains the marker). Assert: the response carries the
   stable catalog error (no driver text in the body), and the
   absence check passes over the captured records for the whole
   case-1 marker set, while the diagnostic record carries its stable
   fields — code, status, SQLSTATE, request ID (the `22P02`
   classification preserved; the raw message suppressed).
2. **Real `P0001` — trigger text from the real trigger.** Force
   `vector_records_validate` to fire (dimension mismatch and
   unknown-space forms via the same test-only seam, against real
   seeded-space data): the raw messages are `embedding dimension
   mismatch: expected 1024, received <n>` and `unknown vector_space_id:
   <uuid>`. Assert: classification is preserved — the stable catalog
   codes still render (the prefix gate reads the message for
   classification only) — and the absence check passes over the
   captured records for the whole case-2 marker set (the raw trigger
   messages and the embedded space UUID).
3. **Caller metadata.** Upsert a record with distinctive synthetic
   metadata (AGENTS.md generic style plus a generated per-test marker,
   e.g. `{"document_type":"note","state":"active","logreg_marker":"<uuid>"}`)
   — both a succeeding and a failing request in the same case. Assert:
   the absence check passes over the captured records for the whole
   case-3 marker set (the metadata JSON object, each metadata value,
   and the per-test marker).
4. **Complete vectors.** A successful upsert with the case vector
   derived per the vector payload markers convention — the 5a
   deterministic synthetic base plus one generated distinctive
   element, with the complete serialization computed from the exact
   values bound. Assert: the absence check passes over the captured
   records for both markers — the complete form (the full
   serialization) and the partial form (the distinctive fragment): a
   full leak and a partial leak (a truncated vector prefix) both
   fail the test.
5. **Credentials and digests.** Run the `create_credential` admin flow
   end-to-end: the raw credential (returned exactly once in the
   response) and its SHA-256 digest (computed by the test from the
   returned raw value). Assert: the absence check passes over the
   captured records for the whole case-5 marker set (the raw
   credential, its hex digest, and the generated fragments of the
   two), and the raw value occurs in the entire test output exactly
   once — in the response body.

Capture and assertion rules:

- **Capture:** the sink records every structured log record the
  service emits during the case — access lines, diagnostic records,
  shutdown/startup records alike.
- **The absence check is the one defined above** (the case's marker
  set, every field of every captured record, the truncated-prefix
  rule for truncated forms), applied to the case's own capture
  window. The markers are per-test generated and
  collision-resistant, so the check passes every compliant log and
  fails on a full *or* partial leak (a truncated digest fragment, a
  quoted vector prefix, a truncated token).
- **Negative control (the check must be able to fail):** before the
  production assertion of each case, the test proves its absence
  check is not vacuous: it emits canary record(s) into the same
  test-owned `slog` sink in a **separate** capture window — a record
  tagged `logreg_canary` carrying the case's marker as the value of
  an explicit `logreg_marker` field; for cases 1 and 4 the canary
  runs twice, once with the full marker and once with a proper
  prefix of more than 8 characters (the truncated form) — and
  asserts that the absence check **fails** on that window for the
  injected marker. The canary window is discarded, the canary
  records never enter the production capture window (asserted: no
  captured record in any production window carries the
  `logreg_canary` tag), and the production assertion then runs
  against the compliant capture. A case whose canary assertion does
  not fail — i.e. whose absence check misses an injected full or
  partial marker — is a broken test and fails the matrix.
- **Diagnostic information is preserved, not silenced:** for each case
  that classifies a raw driver error, assert the diagnostic record
  carries its stable fields — code, status, SQLSTATE, request ID — so
  the matrix pins "suppressed raw text, preserved classification", not
  "no logging".
- **Fixture verification at the database layer:** cases 1–2 first run
  the same SQL as raw `vector_api` statements (database-integration
  layer, no HTTP) to prove the real server returns the expected
  SQLSTATE/message shape on the pgvector build under test — and case
  1 captures the raw `22P02` server message verbatim from that run
  into the case-1 marker set — the log assertions then apply to the
  service-layer runs, where the diagnostic record is emitted.
- Fixtures follow the 5a conventions (generic names, deterministic
  synthetic vectors, per-test generated markers — the high-entropy
  tokens, the distinctive vector element, and the metadata marker —
  and credentials via the service's own randomness; no hardcoded
  marker strings or secrets, no wall-clock dependence).

Placement: named `TestLoggingMatrix…`, integration-tagged, in
`integration/` beside the isolation matrix (it needs the production
chain, the 5a fixtures, and the test-only seam). It is
**release-critical**: CI runs it on every change; a red logging matrix
blocks release, exactly like the isolation matrix.

#### HTTP integration

Full path: the production middleware chain on an `httptest` server
(test port), real authentication with generated credentials, real pool
against the disposable database:

- authenticate → upsert → get → search → delete round trip on the
  seeded space with the deterministic synthetic vectors (upsert
  idempotence asserted separately from the `upserted` count);
- uniform 401 for every authentication-failure shape — missing,
  malformed, unknown, **expired**, disabled credential; admin token
  presented to a data-plane route; data-plane credential presented to
  an admin route — one code and status, no enumeration of failure
  kinds; the expired-credential fixture is a directly-updated past
  `expires_at` (package 2's uniform-401 matrix owns the
  byte-identity assertion);
- admin endpoints end-to-end: register application, register namespace
  (inside the application context), create credential (raw value
  returned exactly once, never again), disable credential (subsequent
  use → uniform 401; the row is not deleted);
- strict-JSON and bound failures: `invalid_json`, `unknown_field`,
  `invalid_content_type`, `body_too_large`, `batch_too_large`,
  `invalid_limit`, `invalid_filter`, `duplicate_record`,
  `invalid_vector`, `non_finite_vector`;
- error-format conformance: for a battery of failure conditions
  (unknown vector space, disabled space, retired space with upsert,
  dimension mismatch, missing namespace, missing record, conflict,
  database unavailable → 503, operation deadline → 503) the response
  body carries exactly the catalog code + stable message (overview
  catalog) — raw PostgreSQL error text never reaches a response;
- request-ID behavior and one structured access line per request;
  `status_code` 0 on abort (client disconnect);
- `/healthz` 200 with no pool involvement; `/readyz` 503 while the
  migrations-converged flag is unset or the ping fails, 200 once
  converged.

The process-level shutdown exit-status contract (normal drain → exit 0;
wedged pool close → `shutdown_pool_close_timeout`, exit 1) is owned by
package 4's validation; 5c restates it at container level.

#### CI integration

- One entry point starts the PostgreSQL + pgvector environment (Docker
  or a local service — implementation detail), runs the harness, and
  tears it down.
- The isolation, migration, and logging matrices run on **every** CI
  change (release-critical; "CI should test at minimum"), not only on
  scheduled runs. None may be skipped by build tag in the pipeline —
  CI passes the integration build tag.
- The PostgreSQL version pin is **18** (root `README.md`), documented
  in the test notes, not scattered through code. The **pgvector
  extension release** is not pinned by any authoritative source; CI
  pins a concrete release supporting the `hnsw` index type used by
  `0003` and records the selection in the test notes (infrastructure
  decision).

### 5c — Container image and deployment shape

#### Multi-stage Dockerfile

```text
stage 1: builder
  FROM golang:<pinned-version>          # matches go.mod (package 1)
  COPY go.mod go.sum → go mod download  (layer-cached)
  COPY source → CGO_ENABLED=0 go build -o /out/vector-service ./cmd/vector-service
  # static binary; no runtime CGO dependency

stage 2: runtime
  FROM gcr.io/distroless/static:<pinned-version>   # proposal; review point
  COPY --from=builder /out/vector-service /usr/local/bin/vector-service
  USER vector-service (non-root UID, e.g. 65532)
  ENTRYPOINT ["/usr/local/bin/vector-service"]
  CMD ["serve"]
```

Runtime image content constraints (DEVELOPMENT.md):

- **Only** the compiled binary (a static Go binary on
  distroless/static needs nothing else).
- TLS connections: the Go binary uses its own crypto; TLS **client
  certificates for PostgreSQL are mounted at runtime** (files or a
  projected volume), never baked into the image (SECURITY.md — private
  keys must not be persisted in image layers).
- Health endpoints: nothing extra required (the process itself serves
  `/healthz`, `/readyz`).
- **No** source, no `go` toolchain, no package manager, no shell (a
  shell-less image is the default; a debug shell is a deliberate,
  documented deviation — review point).
- No migration files: migrations are embedded in the binary
  (package 1); the image never depends on a filesystem migration path.
- No test binaries, no test-harness artifacts.
- Base image version is pinned (tag, not `latest`) and updated
  deliberately, like any dependency.

#### Entrypoint and subcommands in the container

- `CMD ["serve"]`: the container's default lifecycle. `serve`
  performs the full startup order (config → migration step → pool →
  serving → `/readyz`). A container that starts and dies before
  serving is a **configuration or prerequisite failure** and must
  surface a non-zero exit with the structured startup error on stderr —
  the operator reads it from container logs.
- `migrate` is available for explicit one-shot migration jobs (e.g. a
  pre-rollout job that converges schema before new replicas start).
  Both subcommands use the **same** embedded runner (one runner; "no
  two independent migration runners").
- Determinism: `serve` never skips the migration step; a deployment
  that wants "migrate only" uses the `migrate` subcommand, not a flag.
  There is no third mode.
- **Bounded stop.** On SIGTERM: the serve root context is canceled and
  the process-level shutdown flag is set, so in-flight requests settle
  as service-initiated (exactly one 503 `unavailable` rendered while the
  connection is still usable; no wire response only where the connection
  was already closed; in-flight PostgreSQL work canceled) →
  `Server.Shutdown` under
  `VEC_HTTP_SHUTDOWN_GRACE` (default 30 s) → pool close under the hard
  `VEC_HTTP_SHUTDOWN_CLOSE_TIMEOUT` (default 10 s) → **exit 0** when
  the pool close completes, **exit 1** when it does not (package 4's
  exit-status contract). Worst-case stop time is
  `VEC_HTTP_SHUTDOWN_GRACE` + `VEC_HTTP_SHUTDOWN_CLOSE_TIMEOUT`. The
  config enforces that this sum does not exceed 40 s (the defaults sum
  to exactly 40 s); configs violating the bound are rejected at startup.
  The orchestrator's stop grace (e.g. `terminationGracePeriodSeconds`)
  must exceed the worst-case stop time; the defaults leave ~10 s of
  headroom in a typical 50 s container stop budget. The binary must not
  require SIGKILL for a stop within the worst-case bound. SIGKILL remains the out-of-band fallback: it interrupts the
  shutdown at any point (connections drop, open transactions roll back
  server-side, a restarted container re-converges) — a failure mode,
  not a designed path. `migrate` is a short-lived job with no drain: if
  SIGTERM arrives mid-run it exits non-zero, the advisory lock is
  released by the database on disconnect, and a re-run converges.

#### Configuration surface in the container

- All runtime configuration is environment variables (`VEC_*`,
  overview reference) — 12-factor style; no config files in the image.
  Secrets (admin token, TLS key paths) arrive as env references to
  mounted/secret-provisioned material, never as image content.
- `VEC_LISTEN_ADDR` is the in-container listen address (typically
  `0.0.0.0:<port>`); TLS termination for HTTP is **external**
  (reverse proxy / LB) — the container exposes plain HTTP on its listen
  port and PostgreSQL TLS client authentication per the `VEC_PG_*`
  variables.

#### Provisioning shape

The image runs the **canonical shape only**: the migration identity
(`VEC_PG_MIGRATION_USER`, canonically `vector_owner`) owns the
`vector` database and, by the runner's own bootstrap and migrations,
ends up owning both migrated schemas and every in-scope object in
them (the history table, every table, sequence, index, type, and
both migrated functions — every `pg_class`, `pg_type`, and `pg_proc`
row in `vector_control`/`vector_data`); the runtime role `vector_api`
pre-exists with `LOGIN` and owns none of them; the `vector`
extension is pre-provisioned. The canonical-ownership gate (package 1,
Scope 4) enforces this complete ownership contract on every
`migrate`/`serve` pass and fails closed on any mismatch before
serving or before accepting pre-existing migrated state. The runner
bootstraps idempotently as the migration identity and fails
actionably (naming the missing prerequisite) when the shape is not
met. Alternate
provisioning shapes are deliberately out of scope for this release
(package 1); adopting one is a documented architecture decision, not a
container feature.

#### Image build/verification (contract level)

- `docker build` reproduces the binary from `go.mod`/`go.sum` pins
  (same Go version as package 1).
- Verification commands after build (CI, against a disposable
  PostgreSQL from the 5a environment):
  - `docker run --rm <image> migrate` (the subcommand is appended to
    the `ENTRYPOINT`) → exit 0, history populated;
  - `docker run --rm <image>` (i.e. `serve`) → `/readyz` 200 after
    startup; `/healthz` 200; bounded shutdown via `docker stop --time`
    (SIGTERM), container reaches `exited` state with exit code 0 within
    the configured timeout;
  - `docker run --rm <image>` with a missing required `VEC_*`
    variable → non-zero exit, structured error naming the variable,
    no port ever listening. The "no listener" guarantee is proven by
    two tiers: (a) structural — config validation (startup step 1)
    precedes listener construction (startup step 5) in `main.go`,
    and unit tests prove this ordering; (b) defense-in-depth — TCP
    polling on an isolated Docker network with a unique published port,
    evaluated only if the container is observed in a Docker "running"
    state before exit. If the container exits before observation
    (common for fast config failures), the TCP check is skipped; the
    structural guarantees remain authoritative.
  - image inspection: no shell, no `go` toolchain, no `migrations/`
    directory, single non-root user, no root-owned secrets in any
    layer; filesystem allowlist derived by comparing the built image
    against the pinned distroless base image (exact digest), asserting
    the only difference is the binary at `/usr/local/bin/vector-service`.

## Interfaces and boundaries

- **5a harness package** (e.g. `internal/testdb`): provisioning
  (database, extension, roles, canonical ownership), the
  negative-identity variants (ownership of migrated schemas and
  objects — every in-scope `pg_class`, `pg_type`, and `pg_proc` row
  transferred to `vector_api` or to an unrelated unreachable role,
  including `pg_proc.proowner` for each migrated function — plus
  table-, column-, function-, type-, schema-, and database-level
  privilege grants, including grant options in every ACL catalog
  and `MAINTAIN`,
  provenance substitutions of direct grants via inherited or
  `PUBLIC` grants, membership including `set_option`-true and
  `admin_option`-true edges (the ADMIN-only release-critical
  fixture), and role-attribute rejection fixtures for
  the package-1 identity matrices), the optional convergence stage,
  teardown,
  configuration generation, the shared fixture builder. Consumes: packages 1–4 (the production code under
  test). Exposes: fixture helpers to test packages only — `internal/`
  placement means production code can never import it.
- **5b matrix entry points** are named `TestIsolationMatrix…` /
  `TestMigrationMatrix…` / `TestLoggingMatrix…` and integration-tagged
  so CI can target them explicitly. The migration matrix lives inside
  `internal/migrate` (seam access, placement rule above); the
  isolation and logging matrices and the HTTP integration live where
  they can reach the production handler and service functions — the
  logging matrix additionally consumes the 5a fixtures, a test-owned
  `slog` sink, and the test-only vector-bypass seam (the test-only
  route pattern package-4 validation already uses for panic
  injection; it exists so real `22P02`/`P0001` errors reach the
  classifier for the diagnostic-record policy under test).
- **5c:** `Dockerfile`, `.dockerignore` (exclude docs, tests, `.git`
  from the build context), and the deployment notes in this file. The
  image's only interface is the binary's: subcommands, `VEC_*`
  environment, the HTTP listen port, and the PostgreSQL connection.
  One image, one binary, two subcommands. No sidecars. A `Makefile`
  build target or image-label convention is routine and left to the
  implementer.

## Invariants and correctness constraints

**5a/5b (tests):**

- **Real PostgreSQL for PostgreSQL semantics** — RLS,
  transaction-scoped `set_config`, triggers, `ON CONFLICT`, advisory
  locks, and `vector` operators are never mocked (overview invariant
  14; DEVELOPMENT.md).
- **Same migrations as production** (DEVELOPMENT.md): the harness runs
  the embedded migration set through the production runner; there is no
  parallel test-only DDL. Test-only migration *sets* (matrix cases 2,
  3(b), 6) reach the runner only through the package-internal source
  seam — no exported global test override, no mutable package-level
  source state, no build-tag variant of the runner; `migrate.Run`
  always uses the embedded source.
- **RLS stays active in tests**: service-path tests run as
  `vector_api` (no superuser, no `BYPASSRLS`); the bootstrap identity
  is used only for provisioning and direct-DB fixture assertions,
  never to exercise service code paths.
- **No secrets in test output**: generated credentials appear in test
  output in neither raw form nor digested form (invariant 12: "Never:
  raw credentials, digests, …"; SECURITY.md "Never Log"; AGENTS.md);
  fixtures are deterministic and synthetic.
- **No raw driver text in service logs** (package-4 diagnostic-record
  policy): the release-critical logging regression matrix proves,
  against real PostgreSQL + pgvector, that the structured logs the
  service emits carry no raw driver primary message, SQLSTATE `22P02`
  detail, `P0001` trigger text, caller metadata, complete or partial
  vectors, credentials, or credential digests — the proof is keyed to
  per-test generated, collision-resistant payload markers with the
  single defined absence check applied to every field of every
  captured record, and each case's canary negative control proves the
  check fails on an injected marker — while the stable diagnostic
  fields (SQLSTATE, classification, status, request ID) and the stable
  API error rendering, including `P0001` classification, are asserted
  present.
- **Determinism**: fixed synthetic vectors, no wall-clock dependence,
  no external network, re-runnable, order-independent, race-clean.
- The eight isolation cases are **release-critical** and the six
  migration cases are CI-required (MIGRATIONS.md "CI should test at
  minimum"); neither may be skipped by build tag in the CI pipeline.

**5c (container):**

- **Non-root** runtime user (DEVELOPMENT.md).
- **No secrets in the image**: no TLS private keys, no admin token, no
  credentials in any layer; keys are mounted at runtime (SECURITY.md).
- **Embedded migrations only**: the image works with no filesystem
  access to `migrations/`.
- **Single runner**: `serve` and `migrate` share the package-1 runner.
- **Fail-fast startup**: a container with unmet preconditions (missing
  extension, missing role, wrong database name, non-canonical
  provisioning) exits non-zero with an actionable structured error; it
  never serves half-ready.
- **Forward-only**: the image never attempts down-migrations or schema
  repair; corrective changes are new migrations.
- **Reproducible**: pinned Go version, pinned base image,
  `go.sum`-pinned dependencies.
- **Bounded stop**: on SIGTERM the `serve` container stops within
  `VEC_HTTP_SHUTDOWN_GRACE` + `VEC_HTTP_SHUTDOWN_CLOSE_TIMEOUT` — the
  config enforces that this sum does not exceed 40 s (the defaults sum
  to exactly 40 s). The process never hangs on a signal. Exit 0 = the
  pool close completed within the hard deadline; exit 1 = it did not
  (the orchestrator should surface this). The deployment's stop grace
  must exceed the worst-case stop time so a clean stop precedes any
  SIGKILL.

## Expected implementation surface

```text
internal/testdb/ (or similar, under internal/)
  provision.go    # bootstrap: db `vector`, extension, vector_api role,
                  # migration-identity ownership (canonical shape only),
                  # negative-identity variants (ownership of
                  # in-scope objects incl. function proowner,
                  # transferred to vector_api or an unreachable
                  # role, all-level grants incl. grant
                  # options/MAINTAIN, provenance substitutions,
                  # membership incl. set_option/admin_option-true
                  # edges, attribute fixtures)
  config.go       # full VEC_* env for the disposable database,
                  # generated admin token and credentials
  fixtures.go     # two-app/two-namespace fixture builder,
                  # deterministic synthetic vectors
  lifecycle.go    # per-suite start/teardown, convergence stage
                  # (foldable into provision)
internal/migrate/migration_matrix_test.go   # the six cases (integration tag)
integration/isolation_matrix_test.go        # the eight release-critical cases
integration/logging_matrix_test.go          # the release-critical logging
                                            # regression: real 22P02 / P0001 /
                                            # metadata / vector / credential+
                                            # digest cases, captured structured
                                            # logs, per-test generated
                                            # collision-resistant markers, the
                                            # defined absence check, and the
                                            # per-case canary negative control
integration/http_integration_test.go        # full-path + error conformance
Dockerfile                    # two stages, as sketched
.dockerignore                 # exclude docs, tests, .git
test notes                    # PG 18 pin, chosen pgvector release,
                              # how to run the matrices
```

Exact placement is the implementer's choice; the constraint is that
test-only code is never importable from production packages.

## Validation

This package is complete when:

- **5a:** the harness provisions a disposable `vector` database
  (extension present, `vector_api` pre-created, migration identity
  owning the database), exposes both the provisioning and convergence
  stages, exposes a full runtime configuration, and tears the database
  down; every negative-identity variant (ownership of migrated
  schemas and objects — each in-scope `pg_class`, `pg_type`, and
  `pg_proc` row transferred to `vector_api` or to an unrelated
  unreachable role —; table-, column-, function-,
  type-, schema-, and database-level grants from all three sources —
  direct, inherited, and
  `PUBLIC` — plus grant options in every ACL catalog, `MAINTAIN`,
  provenance substitutions in which the effective `has_*` set is
  unchanged, `set_option`-true and `admin_option`-true memberships
  (the ADMIN-only release-critical fixture included) at direct and
  multi-hop depth, and attribute mutations) is constructible
  by the bootstrap identity in the disposable suite database and each
  is rejected by the package-1 identity checks against real
  PostgreSQL — the canonical-ownership gate for the ownership
  variants (before bootstrap, history read, or pool open), the
  runtime-identity assertion for the rest; a clean re-run of the
  full suite from scratch (fresh
  databases) passes with no order dependence.
- **5b:** the eight isolation cases exist, are tagged release-critical
  in CI, and pass against real PostgreSQL + pgvector; the six
  migration cases pass, including the concurrency case (two
  simultaneous runners), the drift/tamper cases, and the
  history-INSERT boundary case (DDL and history row both roll back;
  fixture removed; corrected re-run converges), and the whole
  migration suite is race-clean; the per-package validation sections of
  packages 1–4 all run against the harness (their test lists are the
  per-package acceptance checklists — 5b adds the cross-cutting
  matrices on top); the HTTP integration battery passes (round trip
  with idempotence, uniform 401 for every failure shape, admin
  end-to-end including raw-credential-once, strict-JSON and bound
  failures, error-format conformance with no raw PostgreSQL text in
  any response, `/readyz` flag behavior); the logging regression
  matrix passes — for the real-`22P02`, real-`P0001`, caller-metadata,
  complete-vector, and credential/digest cases, the defined absence
  check passes over every field of every captured structured log
  record for the case's per-test generated, collision-resistant marker
  set (full and truncated forms), and the per-case canary negative
  control proves the check fails on an injected marker — while the
  stable diagnostic fields (code, status,
  SQLSTATE, request ID) and the stable API error rendering — including
  `P0001` classification — are asserted present; no service code path
  is exercised as superuser in any test; a grep of test output shows no
  raw credential material and no credential digests.
- **5c:** the four image verification commands pass in CI; `serve` in
  a container against a fresh (correctly provisioned) database reaches
  `/readyz` 200 and handles a full authenticate → upsert → search
  round trip; **bounded stop (container level)** — SIGTERM to `serve`
  → in-flight requests whose connection is still usable receive exactly
  one 503 `unavailable` (their access lines carry `status_code` 503), a
  request whose connection was already closed has no wire response
  (access line `status_code` 0), and the access lines assert the actual
  status; no new connection is accepted and the container **exits 0**
  within the worst-case bound (the
  normal drain completes far earlier — the bounds are backstops). The
  exit status is asserted, not just the stop: a wedged backend (hold a
  transaction open and block its rollback/release) makes the container
  **exit 1** with the `shutdown_pool_close_timeout` record in its
  logs, and the container stops on its own — the test's own timeout
  (set longer than the worst-case stop time) must not fire, proving no
  indefinite wait. Image audit: single non-root user, no root-owned
  files containing key material, no shell/toolchain/source.

## Out of scope

- Performance/load benchmarks (DEVELOPMENT.md development priority —
  after isolation, data correctness, and failure behavior are done).
- Fuzzing, chaos tests, and failover scenarios.
- Testing the reverse proxy, TLS termination, or external
  infrastructure (the deployment boundary).
- Kubernetes/Helm manifests, service mesh, service discovery,
  autoscaling policy, and orchestrator stop-timeout/termination-grace
  settings (this package documents the worst-case stop time the
  deployment must exceed; the manifest value itself is the operator's).
- In-process HTTP TLS termination (external proxy).
- Multi-architecture (e.g. `arm64`) build matrices — adopt if the
  deployment requires it; the Dockerfile shape is unchanged.
- Image signing/attestation policy (operator decision).
- Any second container or sidecar in the service deployment.
- Coverage of hypothetical future endpoints — v1 tests cover v1
  behavior only. `GET /v1/vector-spaces` is unimplemented per the
  overview's Unresolved entry (C13): the unknown-route 404 is the
  conformance target.
- Alternate migration-provisioning shapes (deferred, package 1).

## Open issues

None. The pgvector extension release pin and the base-image/shell
choices are flagged inline as proposals/infrastructure decisions; none
blocks implementation.
