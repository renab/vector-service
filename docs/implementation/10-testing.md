# Unit 10 — Test Infrastructure, Isolation Matrix, and Migration Test Matrix

## Objective

Own the shared test infrastructure and the two release-critical test
matrices for the service:

1. the **Required Isolation Tests** of `docs/DEVELOPMENT.md`
   (release-critical; a release cannot ship with any of them missing or
   red);
2. the **Testing Migrations** matrix of `docs/MIGRATIONS.md` (six cases).

This unit also defines the test layers, fixture conventions, and the
cross-reference of every per-unit validation section (units 03–09 each
own their local test lists; this unit owns the infrastructure they run
on and the matrices that cut across units).

## Authority

- `docs/DEVELOPMENT.md` — test layers (unit / database integration /
  HTTP integration); "Do not mock PostgreSQL for behaviors that
  specifically depend on PostgreSQL semantics"; "Tests and production
  should exercise the same migrations"; "Avoid maintaining a second
  unrelated schema-setup mechanism"; the eight-case Required Isolation
  Tests, marked release-critical; `log/slog`; development priorities
  (isolation correctness first).
- `docs/MIGRATIONS.md` — "Testing Migrations": CI should test at minimum
  the six cases (empty → latest; previous released → latest; history
  checksum validation; repeated startup with no pending migrations;
  concurrent migration attempts; failure behavior for invalid
  migrations); real supported PostgreSQL + pgvector "where practical".
- `docs/SECURITY.md` — RLS is defense in depth that must be tested as
  active; runtime role never `BYPASSRLS`; no raw credentials in any
  output.
- `docs/implementation/README.md` — Conflicts C2 (migration role),
  C3 (pgvector extension pre-provisioned), C4 (database must be named
  `vector`), C5 (`vector_api` must pre-exist), C12 (three provisioning
  options for schema/history creation); conventions (pgx v5, synthetic
  fixtures, UUID convention).
- `migrations/0001–0004` — the exact schema under test; the seeded
  space and HNSW index; the trigger and RLS policies the tests must
  prove active.
- `AGENTS.md` (repository root) — generic names only in fixtures and tests
  (`memory-service`, `notes-service`, `search-service`; `project-alpha`,
  `project-beta`, …); obviously synthetic fixtures.
- Units 03–09 — the per-unit validation sections this infrastructure
  serves.

## Dependencies

- All of units 01–09 implemented (the matrices exercise their
  behavior).
- PostgreSQL **18** with the pgvector extension available — the root
  `README.md` pins the service to PostgreSQL 18, and the test and CI
  environments must use it. No authoritative source pins the **pgvector
  extension release** (`docs/MIGRATIONS.md` delegates extension
  availability to infrastructure); the concrete pgvector release
  installed in the test/CI environment is an **infrastructure/human
  decision** (Open issues), not a contract claim.

## Scope

### Test database provisioning

The harness (Go test helper package, e.g. `internal/testdb`, or a
`testinfra` package at the module root — implementer's choice)
provisions, per test **suite** (package), a disposable environment:

1. Connect as the test bootstrap identity (superuser or an
   infrastructure admin — this identity is **harness-only**; no service
   code path ever runs as it).
2. Create a database named **`vector`** (C4: config validation and
   `0004`'s `GRANT CONNECT ON DATABASE vector` hardcode the name; the
   test database must match).
3. In that database: `CREATE EXTENSION vector;` (C3: no migration
   creates the extension; the runner validates its presence).
4. Create the runtime role `vector_api` with `LOGIN` and no
   superuser/`BYPASSRLS` attributes (C5: `0004`'s `GRANT … TO
   vector_api` requires the role to exist; SECURITY.md forbids
   `BYPASSRLS`).
5. Apply the C12 provisioning decision for the test environment
   (proposal — review point; see below).
6. Run the service's **own** migration path (the `migrate` subcommand
   logic, unit 03) to build the schema — no second schema-setup
   mechanism (DEVELOPMENT.md).
7. Tear down the database at suite end (`DROP DATABASE`).

**C12 shape in tests (proposal; review point):** default harness uses
**option 3** — the bootstrap grants the **distinct migration role**
(`VEC_PG_MIGRATION_USER`) **both** `CREATE ON DATABASE vector` and
`CONNECT … WITH GRANT OPTION`, and the runner's bootstrap creates the
schemas + history table as that identity, exactly as C12 option 3
defines it (both grants are required: a `CREATE`-only grant cannot
execute `0004`'s `GRANT CONNECT`); the migration matrix must assert
that the `migrate` run succeeds through all four migrations —
including `0004`'s grants — in every shape it exercises. `vector_api`
is granted `CREATE` nowhere (the C2 two-role decision — a single-role
migrate-then-serve deployment is not a supported shape, so the harness
never exercises one). Additionally, at least one migration-suite
variant (case 1 of the migration matrix) runs under **option 1** (the
canonical shape — the migration identity owns the `vector` database)
to prove the recommended production shape works through the same
runner. Both shapes must pass; the choice of which is default in tests
is a harness convenience, not a service decision.

Test configuration: the suite generates a full runtime config pointing
at the disposable database (env-style or struct injection per unit 01's
config surface), including a test admin token and generated
application credentials (unit 05 `GenerateCredential`), so HTTP
integration tests exercise real authentication.

### Test layers

| layer | runs against | purpose |
|-------|--------------|---------|
| unit | nothing (pure Go) | validation, codecs, config, error mapping, filter builder, score transforms, credential generation/parsing, request-ID rules, SQL builder placeholder ordering |
| database integration | real PostgreSQL + pgvector, via the service's own service/DB functions (not HTTP) | RLS behavior, isolation, upsert/delete/search/filter SQL, triggers, migration runner, credential lookup |
| HTTP integration | real server (test port) → auth → service → PostgreSQL | the full path for critical isolation endpoints and error-format conformance |

PostgreSQL-specific semantics (RLS, `set_config` transaction scope,
`ON CONFLICT`, `->>` JSONB lookup, `vector` operators, triggers,
advisory locks) are **never mocked** (DEVELOPMENT.md).

### Required Isolation Tests (release-critical)

The eight cases from `docs/DEVELOPMENT.md`, verbatim, each with the
concrete scenarios this contract pins:

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

All eight are **release-critical** (DEVELOPMENT.md): CI must run them on
every change; a red matrix blocks release.

### Migration test matrix

The six cases from `docs/MIGRATIONS.md`, each run against a real
PostgreSQL + pgvector database (fresh per case):

```text
1. empty database → latest schema
   - fresh `vector` db (harness steps 1–5), run migrate → all four
     migrations applied, history rows present with checksums, schema
     objects exist (schemas, tables, RLS enabled+forced, policies,
     trigger, seeded space row, HNSW index), vector_api privileges
     match 0004 — including an assertion that `vector_api` can open a
     connection to the database (proving `0004`'s `GRANT CONNECT ON
     DATABASE vector TO vector_api` executed under the shape being
     tested)
2. previous released schema → latest schema
   - operationalized (proposal; review point): build the prior state by
     applying a prefix of the released set (0001–0003) with history
     rows, then run the latest runner → applies exactly 0004, no
     re-application, history consistent
3. migration history checksum validation
   - (a) tamper a history row's checksum → runner fails fatally with a
     drift error, no migrations applied; (b) modify an applied
     migration file's bytes (test-only copy) → same fatal drift
     behavior
4. repeated startup with no pending migrations
   - run migrate twice (and a third time) → second/third are no-ops,
     exit 0, history unchanged; serve startup after migrate → ready
5. concurrent migration attempts
   - two runner processes start simultaneously on one fresh database →
     both exit 0 (one applies, one waits on the advisory lock within
     VEC_MIGRATION_LOCK_WAIT and observes applied state); history has
     exactly one row per migration
6. failure behavior for invalid migrations
   - test-only migration set containing a failing statement (syntax
     error mid-migration, or a statement rejected by the role) →
     runner exits non-zero, the failed migration has NO history row,
     the database schema is unchanged by the failed migration
     (transactional per-migration semantics, unit 03), a corrected
     re-run applies cleanly
```

### Fixture conventions

- **Names**: generic only (AGENTS.md): applications
  `memory-service` / `notes-service` / `search-service`; namespaces
  `project-alpha` / `project-beta` / `research`. Obviously synthetic.
- **Vectors**: deterministic synthetic 1024-dim `float32` vectors for
  the seeded space — e.g. basis/unit vectors per record (nearest-
  neighbor outcomes exact and stable), scaled variants for
  distance-ranking tests. No real embeddings, no network fetches.
- **Metadata**: small fixed objects (`{"document_type":"note",
  "state":"active"}` style) so filter tests are exact.
- **IDs/credentials**: generated via the service's own functions
  (UUID v4, `GenerateCredential`) so tests exercise production code
  paths; no hardcoded secrets.
- **Isolation fixtures**: a standard two-application, two-namespace
  fixture (app A: `project-alpha`; app B: `project-beta`; plus a second
  A-namespace `research` for the namespace-axis cases), created through
  the **admin API path** (unit 06) in HTTP tests and through the
  service functions in DB tests.
- Tests must be **re-runnable** (fresh database per suite) and
  **order-independent** (no shared mutable state between test functions
  beyond the suite database; per-test data uses distinct
  object_ids).

### CI integration (contract level)

- One entry point starts the PostgreSQL + pgvector environment (Docker
  or a local service — implementation detail), runs the harness, and
  tears it down.
- The isolation matrix and migration matrix run on **every** CI change
  (release-critical), not only on scheduled runs.
- The PostgreSQL version pin is **18** (root `README.md`), documented
  in the test README, not scattered through code. The **pgvector
  extension release** is not pinned by any authoritative source; the CI
  environment must pin a concrete pgvector release that supports the
  `hnsw` index type used by `0003` and record the selection in the test
  README (infrastructure/human decision — Open issues).

## Interfaces and boundaries

- `internal/testdb` (or `testinfra/`): provisioning, teardown, config
  generation, the shared two-app/two-namespace fixture builder.
- Matrix tests live next to the behavior they test (per-package
  `*_test.go`) but are **tagged** (e.g. `//go:build integration`) so
  unit-test runs stay fast and dependency-free; the matrix entry points
  are named `TestIsolationMatrix…` / `TestMigrationMatrix…` so CI can
  target them explicitly.
- Consumes: every unit 01–09 package. Exposes: fixture helpers to test
  packages only (never imported by production code — build-tag or
  `internal` placement enforces this).

## Invariants and correctness constraints

- **Real PostgreSQL for PostgreSQL semantics** — RLS, transaction-scoped
  `set_config`, triggers, `ON CONFLICT`, advisory locks, `vector`
  operators are never mocked (DEVELOPMENT.md).
- **Same migrations as production** (DEVELOPMENT.md): the harness runs
  the embedded migration set through the unit 03 runner; there is no
  parallel test-only DDL (test-only migration *sets* for case 6 are
  runner inputs, not separate schema mechanisms).
- **RLS stays active in tests**: service-path tests run as
  `vector_api` (no superuser, no `BYPASSRLS`); direct-DB tests that
  inspect schema state may use the bootstrap identity but must not use
  it to exercise service code paths.
- **No secrets in test output**: generated credentials appear only as
  digests in test logs; fixtures are deterministic and synthetic
  (AGENTS.md, SECURITY.md).
- **Determinism**: fixed synthetic vectors, no wall-clock dependence
  (fixed RFC 3339 timestamps), no external network, re-runnable.
- The eight isolation cases are **release-critical** and the six
  migration cases are CI-required (MIGRATIONS.md "CI should test at
  minimum"); neither may be skipped by build-tag in the CI pipeline.

## Expected implementation surface

```text
testinfra/provision.go        # bootstrap: db, role, extension, C12 shape
testinfra/config.go           # runtime config + admin token + credentials
testinfra/fixtures.go         # two-app/two-namespace fixture builder
internal/testdb (or same pkg) # per-suite lifecycle, teardown
<packages>/isolation_test.go  # the eight release-critical cases
migrations/... or testinfra/migration_matrix_test.go  # the six cases
test README (test README section or docs/implementation/10 notes)
```

(Exact placement is the implementer's choice; the constraint is that
test-only code is never importable from production packages.)

## Validation

This contract is complete when:

- the eight isolation cases exist, are tagged release-critical in CI,
  and pass against real PostgreSQL + pgvector;
- the six migration cases pass, including the concurrency case (two
  simultaneous runners) and the drift/tamper cases;
- the per-unit validation sections of units 03–09 all run against the
  shared harness (their test lists are the acceptance checklist for
  each unit);
- a clean re-run of the full suite from scratch (fresh databases)
  passes with no order dependence;
- no service code path is exercised as superuser in any test;
- grep of test output shows no raw credential material.

## Out of scope

- Performance/load benchmarks (DEVELOPMENT.md development priority 6 —
  after isolation, data correctness, and failure behavior are done).
- Fuzzing, chaos tests, and failover scenarios.
- Testing the reverse proxy, TLS termination, or external infrastructure
  (the deployment boundary, unit 11).
- Coverage of hypothetical future endpoints — v1 tests cover v1
  behavior only.

## Open issues

- **C12 test shape**: default harness uses option 3 (explicit grants on
  the **distinct migration role** — `CREATE ON DATABASE vector` plus
  `CONNECT … WITH GRANT OPTION`; `vector_api` never holds `CREATE`)
  with one option-1 (canonical — migration identity owns the database)
  variant — proposal; the *production* default remains an operator
  decision per C12. Review point.
- **Case 2 operationalization** ("previous released schema" = the
  0001–0003 prefix of the same released set) is a proposal; if a truly
  distinct prior release ever exists, the fixture must switch to it.
  Review point.
- **pgvector extension release pin** is unspecified by the authoritative
  docs (PostgreSQL itself is pinned to **18** by the root `README.md`,
  and the harness must use it). The harness must pin a concrete pgvector
  release that supports the `hnsw` index type used by `0003` and
  document the selection in the test README. This is an
  **infrastructure/human decision** — no authoritative source supports a
  specific pgvector release, so the contracts do not claim one. Review
  point.
