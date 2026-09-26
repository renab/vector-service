# Unit 03 — Embedded Migration Runner

## Objective

Provide a single migration runner, shared by `serve` and `migrate`
(unit 01's binary layout), that: embeds the released SQL migrations into the
binary; validates the embedded set generically (not against a hardcoded
name list); validates infrastructure prerequisites; serializes concurrent
startups with an advisory lock; deterministically bootstraps the history
table under the migration identity; and applies pending migrations in
order, each with its history row in one transaction. The runner is the
**only** code path that mutates schema state or writes migration history.
`serve` runs it at startup before listening; `migrate` runs it and exits.

## Authority

- `docs/MIGRATIONS.md` — ordering, immutability, history table, checksum
  validation, advisory locking, per-migration transactions, startup order,
  embedding via Go `embed`, pgvector prerequisite, no second runner.
- `migrations/0001_shared_vector_schema.sql` — creates schemas
  (`IF NOT EXISTS`), tables, trigger, and **forced** RLS on
  `vector_data.vector_records` and `vector_control.namespaces`.
- `migrations/0002_qwen3_embedding_space.sql` — inserts the
  `qwen3-embedding-0.6b-1024-cosine-v1` vector space
  (UUID `0199f31e-1000-7000-8000-000000000001`, 1024 dimensions, cosine)
  with `ON CONFLICT (vector_space_key) DO NOTHING`.
- `migrations/0003_qwen3_hnsq.sql` — creates the partial HNSW index
  `vector_records_qwen3_06b_1024_cosine_hnsw` on
  `(embedding::vector(1024)) vector_cosine_ops`
  `WHERE vector_space_id = '0199f31e-1000-7000-8000-000000000001'`.
  Filename typo is authoritative (Conflict C11); ordering is by numeric
  prefix, never by filename suffix.
- `migrations/0004_runtime_identity.sql` — creates
  `vector_control.application_credentials` and grants **to
  `vector_api`** exactly: `CONNECT` on `DATABASE vector`, `USAGE` (not
  `CREATE`) on both schemas, `SELECT, INSERT, UPDATE` on
  `applications` and `namespaces`, `SELECT`-only on `vector_spaces`,
  full CRUD on `application_credentials` and `vector_records`. Note that
  `0004` grants **nothing** on `vector_control.schema_migrations` — this is
  intentional and load-bearing for the role model below.
- `docs/SECURITY.md` — owner vs runtime role separation; the runtime role
  must not own the database or bypass RLS; the client certificate is
  scoped to a single PostgreSQL role.
- `docs/implementation/README.md` — Conflicts C2–C5 and C12 (two-role
  decision and deterministic bootstrap settlement), configuration
  reference (`VEC_PG_USER`, `VEC_PG_MIGRATION_USER`,
  `VEC_MIGRATION_LOCK_WAIT`).

## Dependencies

- Unit 01 (`config.Config`, `apierr`, logging).
- Unit 02 (`database.ConnConfig(cfg, logger, id ConnIdentity)` — the
  migration connection reuses the same TLS/connection builder as the
  runtime connection, with the migration identity and its own
  certificate/key/CA paths).

## Scope

1. **Embedding.** Go `embed` may only reference files within the embedding
   package's own directory, so a small package lives **in** `migrations/`
   (e.g. `migrations/embed.go`, package `migrations`, with
   `//go:embed *.sql`): the embedded set is exactly the `*.sql` files in
   `migrations/`, and `README.md` is excluded automatically by the
   `*.sql` pattern. `internal/migrate` imports that package.

2. **Embedded-set validation (generic, no name allowlist).** The runner
   validates the embedded set by *shape*, not by matching a hardcoded list
   of the four current filenames (a name allowlist breaks the moment a
   fifth released migration is added, and it would couple the runner to
   the exact build tree). Validation rules, each failure a startup error:
   - every filename matches `^[0-9]{4}_(.+)\.sql$`;
   - numeric prefixes are unique;
   - the set is non-empty;
   - the set is treated as an **ordered set**: migrations are applied in
     ascending numeric prefix order, and the descriptive suffix is
     informational only (Conflict C11).
   The runner computes each file's SHA-256 over its exact embedded bytes.
   - **Test-only seam.** The embedded set is exposed through a single
     accessor with a test-only override
     (`migrations.SetForTesting(set []Migration)`, panics or is
     compile-guarded so production builds always see the embedded set).
     This is the *only* injection point, and it exists so unit 10's
     "failure behavior for invalid migrations" case can run a synthetic
     failing migration set (e.g. a migration whose body errors
     mid-transaction, or a file with a malformed `BEGIN`/`COMMIT` shape)
     without editing released files. Production code never calls the
     override; the runner has exactly one execution path for either
     source.

3. **Migration identity (two-role model, Conflict C2).** The migration
   connection uses the identity configured in `VEC_PG_MIGRATION_USER`
   with its own TLS client certificate/key and CA
   (`VEC_PG_MIGRATION_CLIENT_CERT/KEY`, `VEC_PG_MIGRATION_CA_CERT`).
   `VEC_PG_MIGRATION_USER` has **no default and is required**: a
   correctly provisioned deployment always provides a distinct,
   privileged migration identity. The runner does **not** fall back to
   `VEC_PG_USER` — running migrations as the constrained runtime role
   violates the SECURITY.md owner/runtime separation and is not a
   supported shape (a single-role migrate-then-serve deployment is
   rejected by design). The migration identity is never used for
   serving: no pool, no data-plane or admin traffic, no request
   handling.

   **Role model and privileges.** The supported deployment shapes
   (Conflict C12) differ only in how the migration identity obtains
   object-creation capability; in every shape:

   | Object | `vector_api` (runtime; per `0004`) | Migration identity |
   |--------|------------------------------------|--------------------|
   | `DATABASE vector` | `CONNECT` | owner **or** `CREATE` + `CONNECT … WITH GRANT OPTION` (canonical: owner; shape C: explicit grants) |
   | `SCHEMA vector_control` | `USAGE` only | `CREATE` (owned or pre-granted) |
   | `SCHEMA vector_data` | `USAGE` only | `CREATE` (owned or pre-granted) |
   | `vector_control.schema_migrations` | **no grant — never reads or writes history** | full (creates and owns the table) |
   | `applications`, `namespaces` | `SELECT, INSERT, UPDATE` | owner/creator (via `0001`) |
   | `vector_spaces` | `SELECT` | owner/creator (via `0001`/`0002`) |
   | `application_credentials` | full CRUD | owner/creator (via `0004`) |
   | `vector_data.vector_records` | full CRUD | owner/creator (via `0001`) |

   The three shapes: **(A) canonical** — migration identity owns the
   `vector` database; **(B) pre-provisioned** — infrastructure pre-creates
   the schemas and history table, then migrations run under the
   migration identity; **(C) explicit grants (constrained fallback)** —
   infrastructure grants the migration identity **both** `CREATE ON
   DATABASE vector` (so the bootstrap can create the schema and history
   table) **and** `CONNECT ON DATABASE vector … WITH GRANT OPTION` (so
   migration `0004` can execute `GRANT CONNECT ON DATABASE vector TO
   vector_api`). PostgreSQL only allows a role to grant `CONNECT` on a
   database if it owns the database or holds `CONNECT` on it with grant
   option; a bare `CREATE` grant is **not** a supported shape because it
   cannot execute `0004`. In none of the shapes does `vector_api` hold
   `CREATE` anywhere, own any object, or touch `schema_migrations`.

   **Shape B is conditional.** It is supported only when the
   pre-provisioning gives the migration identity everything needed to
   execute all pending migration DDL (`0001`–`0004`) as that identity:
   `CREATE` on (or ownership of) both pre-created schemas, full
   `SELECT`/`INSERT` on the pre-created `schema_migrations`, and the
   grant authority `0004`'s `GRANT` statements require (database
   ownership, or `CREATE` + `CONNECT ON DATABASE vector WITH GRANT
   OPTION`) — in practice, the pre-provisioning is performed **by or as
   the configured migration identity**. A pre-provisioning that grants
   the migration identity only partial privileges (e.g. merely `INSERT`
   on the history table, or `CREATE` on the database without `CONNECT …
   WITH GRANT OPTION`) cannot apply `0001`–`0004` and is **not** a
   supported shape; the runner fails on the first statement it cannot
   execute with an actionable error naming the missing privilege.

4. **Prerequisite validation (before any DDL).** In order, each failure
   aborts startup with a structured error naming the missing
   prerequisite:

   - **Database name**: already validated by configuration (Conflict C4,
     unit 01); re-assert via `SELECT current_database()` for the log
     line.
   - **pgvector extension** (Conflict C3):
     `SELECT 1 FROM pg_extension WHERE extname = 'vector'`. Missing →
     failure instructing the operator that infrastructure must ensure
     `CREATE EXTENSION vector` has been applied (per
     `docs/MIGRATIONS.md` the service does not install OS extension
     packages).
   - **`vector_api` role exists** (Conflict C5):
     `SELECT 1 FROM pg_roles WHERE rolname = 'vector_api'`. Missing →
     failure, because migration `0004` cannot grant to a non-existent
     role. (The migration identity's own capabilities are exercised
     directly by the bootstrap below; a missing privilege there surfaces
     as the bootstrap's actionable error, not a separate probe.)

5. **Advisory lock.** Before touching the history table, acquire a named
   session-level advisory lock:
   `SELECT pg_advisory_lock(hashtext('vector.service.migration'))`.
   Acquire with a bounded wait (`VEC_MIGRATION_LOCK_WAIT`, default 300 s):
   poll `pg_try_advisory_lock` every 1 s until the deadline, or use a
   blocking `pg_advisory_lock` combined with a statement-level deadline
   the implementer chooses; the observable contract is *bounded wait,
   then startup failure* if the lock is not acquired. The lock is held
   for the entire migrate phase (it releases on connection close) and is
   the only concurrency mechanism — no file locks, no out-of-band
   coordination. Other replicas wait and then proceed once history is
   current.

6. **Deterministic bootstrap (Conflict C12).** After acquiring the lock,
   the runner executes a **single, fixed bootstrap step as the migration
   identity** — no probing, no capability ladder:

   ```sql
   CREATE SCHEMA IF NOT EXISTS vector_control;
   CREATE TABLE IF NOT EXISTS vector_control.schema_migrations (
       version    text PRIMARY KEY,
       name       text NOT NULL,
       checksum   bytea NOT NULL,
       applied_at timestamptz NOT NULL DEFAULT now()
   );
   ```

   Logical history structure: `version` is the 4-digit numeric prefix
   (e.g. `0001`); `name` is the full filename; `checksum` is the SHA-256
   of the file bytes.

   - On a fresh database (shape A or C) this creates the `vector_control`
     schema and history table; the migration identity owns both.
   - On a pre-provisioned database (shape B) both statements are no-ops
     (`IF NOT EXISTS`); the identity's privileges over the
     pre-created objects are then exercised by the first real history
     `INSERT` (full access to the pre-created table) and by
     migration `0001`'s DDL (`CREATE` on the pre-created schemas). A
     pre-provisioning that does not provide those privileges is not a
     supported shape B deployment and fails here or at `0001` with an
     actionable error.
   - Any failure here (e.g. the identity lacks `CREATE` on the database
     or `INSERT` on the pre-provisioned table) is a **startup failure
     with an actionable error naming the missing privilege and the
     applicable shape** — the runner never skips history recording,
     never writes history to any other table, and never retries with a
     different identity.
   - `vector_api` is not involved in the bootstrap and never touches
     `schema_migrations` (it has no grant on it).

   On every startup (even with zero pending migrations) the runner
   validates that every **already applied** row's stored checksum matches
   the embedded file's checksum. Mismatch → startup failure (drift
   detection per `docs/MIGRATIONS.md`). Missing embedded file for a
   recorded version (downgrade/older binary against newer schema) →
   startup failure with a message naming the unknown version.

7. **Per-migration execution — atomicity with history.** Each released
   file is exactly one transaction: it begins with `BEGIN;` and ends with
   `COMMIT;`. The runner enforces this shape (after stripping whitespace
   and `--` comment lines) and then restructures execution as **one**
   PostgreSQL transaction:

   ```text
   BEGIN;
     <file body: the SQL between the file's BEGIN and COMMIT>
     INSERT INTO vector_control.schema_migrations (version, name, checksum)
       VALUES ($1, $2, $3);
   COMMIT;
   ```

   This makes the schema change and its history record commit or roll
   back together. A failure mid-migration leaves no partial schema change
   and no history row; the next startup retries the same migration. A
   failure after the file's statements but at the history `INSERT` rolls
   back the entire migration and fails startup with the error. History
   writes happen exclusively as the migration identity (Scope 3).

   Constraint: this protocol requires every migration to be fully
   transactional. If a future migration must run non-transactionally
   (e.g. `CREATE INDEX CONCURRENTLY`), the protocol must be extended with
   an explicit documented marker before that migration is added
   (`docs/MIGRATIONS.md` requires such migrations to document why). This
   unit does not implement the extension.

8. **Idempotency and repeated startup.** With zero pending migrations and
   checksums matching, the runner does nothing (no DDL, no writes) and
   exits 0. `migrate` on an already-current database is a no-op.
   Migration `0002`'s `ON CONFLICT DO NOTHING` makes the vector-space row
   safe even if re-applied; the runner's history check makes re-application
   unnecessary.

9. **Runner result contract.** The runner returns either success (schema
   current) or an error. `serve` maps failure to: process exits non-zero
   **before** the HTTP server starts, and readiness (unit 09b) can never
   report ready while migrations are pending or failed
   (`docs/MIGRATIONS.md`: "The service must not report ready while
   required migrations are pending or failed"). Log each applied
   migration (version, name, duration) and the final "schema current"
   summary at info level. Never log migration file contents.

## Interfaces and boundaries

- `migrate.Run(ctx, cfg, logger) error` — single entry point, called by
  both `serve` (startup) and `migrate` (subcommand).
- Consumes `database.ConnConfig(cfg, logger, id ConnIdentity)` (unit 02)
  with the migration identity to build its dedicated connection (not the
  pool). The pool is not used for migration. The connection is opened
  inside `Run` and closed on return: in `serve` it exists **only** during
  the pre-serving migration phase and is released before the HTTP server
  begins serving. No migration connection exists in steady state, and
  readiness never opens one (unit 09b).
- Exposes the embedded-set accessor with the test-only override
  (Scope 2) — consumed only by unit 10 tests.
- Produces no state visible to request handlers other than schema
  currency; unit 09b readiness observes schema currency through the
  startup phase of the same process (see unit 09b for how readiness is
  composed without a runtime grant on `schema_migrations`).

## Invariants and correctness constraints

- **Embedded set is the schema authority.** The binary contains exactly
  what was built; no runtime filesystem read of migration files.
- **Forward-only.** No down-migration, no `DROP` of history rows, no
  rewriting of recorded versions. Released files are never modified by
  this code path.
- **Checksum drift is fatal.** Applied-but-changed file → startup failure,
  never silent acceptance.
- **Exactly one migrator at a time.** Advisory lock held across the whole
  migrate phase (including bootstrap); bounded wait, then failure. No
  lockless "check then apply" path.
- **History and schema change are atomic** (single transaction per
  migration, including the history `INSERT`).
- **History is written only by the migration identity.** `vector_api`
  creates nothing and never reads or writes `schema_migrations`; no code
  path may grant it access.
- **Bootstrap is deterministic.** One fixed step as the migration
  identity; no probe-and-fallback, no identity switching, no skipping.
- **RLS stays forced after 0001.** Nothing in this unit disables, alters,
  or bypasses RLS. The migration identity is not a superuser and the
  runner never issues `SET ... BYPASSRLS`; `FORCE ROW LEVEL SECURITY`
  (applied by `0001`) applies to it too.
- **Prerequisite failures are startup failures**, not warnings.
- **No credential or vector content is logged**; logs carry versions,
  names, durations, and role names only (never raw credentials or keys).

## Expected implementation surface

```text
migrations/embed.go                  # package migrations, //go:embed *.sql, set accessor + test-only override
internal/migrate/migrate.go          # Run: prerequisites, lock, bootstrap, history, apply loop
internal/migrate/validate.go         # embedded-set shape validation (grammar, uniqueness, ordering)
internal/migrate/history.go          # checksum validation against recorded rows
internal/migrate/bootstrap.go        # schema + history table creation (Scope 6)
internal/migrate/transaction.go      # BEGIN/COMMIT strip + re-wrap protocol
internal/migrate/*_test.go
cmd/vector-service/main.go           # (unit 01) wiring for serve/migrate
```

## Validation

- Unit tests (no database):
  - filename parsing: valid prefixes, non-empty set required, non-numeric
    prefix, duplicate prefix, missing `.sql`, non-matching names all
    rejected; `README.md` excluded by the embed pattern;
  - ordered-set semantics: a set containing a synthetic `0005_*.sql`
    validates and orders after `0004` (proves there is no four-name
    allowlist);
  - checksum stability: SHA-256 of known bytes;
  - transaction re-wrap: the four real migration files parse to a body
    and re-wrap into a single `BEGIN; ... COMMIT;` containing the body
    plus the history `INSERT` with parameters (assert no `BEGIN` remains
    inside the body);
  - drift: recorded checksum ≠ embedded → error; unknown recorded version
    → error;
  - test seam: with an injected synthetic set, a mid-transaction failure
    produces an error and (in an integration test) no history row and no
    partial schema change.
- Integration tests (real PostgreSQL + pgvector; infrastructure from unit
  10, canonical two-role shape — distinct privileged migration identity
  plus `vector_api`):
  - fresh `vector` database → `migrate` → bootstrap creates
    `vector_control` and `schema_migrations` (owner = migration
    identity), all four migrations applied, history has four rows with
    correct checksums, `vector_api` can open a connection to `vector`
    (proving `0004`'s `GRANT CONNECT ON DATABASE vector TO vector_api`
    executed), `has_schema_privilege('vector_api', 'vector_data', 'USAGE')`
    is true, and `vector_records` RLS is enabled and forced
    (`rowsecurity = true`, `forcerowsecurity = true`), the HNSW index
    exists with the partial predicate on the 0002 UUID, and
    `has_table_privilege('vector_api', 'vector_control.schema_migrations',
    'SELECT')` is **false** (runtime role never touches history);
  - repeated `migrate` → no-op (no DDL, history unchanged);
  - concurrent startup: two `migrate` processes → exactly one applies,
    the other waits on the advisory lock and observes a current schema
    (assert via history row count and absence of duplicate errors);
  - lock wait: hold the advisory lock in a separate session, set
    `VEC_MIGRATION_LOCK_WAIT=5s` → startup fails with the lock error
    after ~5 s;
  - drift: alter a stored checksum row → startup fails with drift error;
  - missing pgvector → clear prerequisite error (C3); missing `vector_api`
    role → clear error (C5); missing `VEC_PG_MIGRATION_USER` →
    configuration failure (C2);
  - bootstrap privilege failure (C12): a migration identity with
    `USAGE` on `vector_control` but no `CREATE` on the database (and no
    pre-provisioned history table) against a fresh database → startup
    fails with an actionable error naming the missing privilege;
  - shape B (supported): schemas + history table pre-created **by or
    as the migration identity** (or the identity granted `CREATE` on
    both schemas plus full access to the history table and the grant
    authority `0004` needs — database ownership, or `CREATE` on the
    database plus `CONNECT … WITH GRANT OPTION`) → `migrate` succeeds
    and records history; `vector_api` still has no grant on the history
    table;
  - shape B (unsupported pre-provisioning): schemas + history table
    pre-created by an owner role with the migration identity granted
    **only** `INSERT` on `schema_migrations` → `migrate` fails with an
    actionable error on the first DDL it cannot execute (e.g. `0001`'s
    `CREATE TABLE`); no history rows for pending migrations, no
    partial schema change.
  - shape C (supported): fresh database, migration identity granted
    `CREATE ON DATABASE vector` **and** `CONNECT … WITH GRANT OPTION` →
    bootstrap creates the schema + history table, `migrate` succeeds
    through all four migrations including `0004`'s grants (assert
    `vector_api` can connect afterwards);
  - shape C (unsupported, `CREATE` only): fresh database, migration
    identity granted **only** `CREATE ON DATABASE vector` → bootstrap
    succeeds and `0001`–`0003` apply, but `0004`'s `GRANT CONNECT`
    fails with a permission error; the transaction rolls back, so
    history has exactly three rows, `0004`'s effects are absent, and
    the error is actionable (it must name the missing `CONNECT … WITH
    GRANT OPTION` grant or database ownership, not merely "permission
    denied").

## Out of scope

- Non-transactional migration protocol (future, documented extension).
- `CREATE INDEX CONCURRENTLY`, partitioning, rollback tooling.
- Declarative schema diffing (not required in v1 per `docs/MIGRATIONS.md`).
- Readiness endpoint (unit 09b) — this unit only guarantees the startup
  ordering it depends on.

## Open issues

- **Default deployment shape (C12).** Shapes A/B/C are all supported and
  behavior-identical from the service's point of view; the canonical
  shape (A: migration identity owns the `vector` database) is the
  recommended default and is what units 10 and 11 target. The operator
  must state which shape a given deployment uses before first startup;
  no service behavior depends on the choice.
