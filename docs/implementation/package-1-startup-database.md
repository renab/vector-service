# Package 1 — Startup, Database Access, and Migrations

## Objective

Deliver the `vector-service` binary skeleton: `serve` and `migrate`
subcommands, fail-fast configuration, the runtime PostgreSQL pool
(`vector_api`), the migration-identity connection, the embedded
canonical migration runner, and the pgvector text codec. After this
package, `vector-service migrate` converges a fresh, correctly
provisioned database to the released schema, and `vector-service serve`
executes the authoritative startup order up to the point of handing off
to the serving lifecycle (package 4).

## Authority

- `docs/MIGRATIONS.md` — embedded migrations, startup order (load
  config → connect → validate prerequisites → run/validate migrations →
  start serving → `/readyz` succeeds), forward-only semantics,
  six-case migration test matrix (executed in package 5b).
- `docs/SECURITY.md` — TLS client-certificate authentication for both
  identities; no `BYPASSRLS`; no runtime role as owner; never log or
  persist credentials or keys.
- `docs/DEVELOPMENT.md` — Go standard library, `pgx`, explicit
  timeouts, subcommand determinism ("no two independent migration
  runners"), multi-stage container notes (package 5c).
- `README.md` (root) — PostgreSQL 18 pin.
- `migrations/0001_shared_vector_schema.sql` through
  `0006_runtime_privilege_boundary_completion.sql` — the exact schema
  and privilege boundary under management; `0004` binds runtime grants
  to `vector_api` and hardcodes `GRANT CONNECT ON DATABASE vector TO
  vector_api`; `0005` establishes the `PUBLIC` baseline and the direct
  function/type grants; `0006` completes the `PUBLIC` type baseline
  (the implicit row/composite types of the migrated tables).
- `AGENTS.md` — explicit SQL, migration immutability, structured logs.
- Overview: configuration reference, invariants 3, 4, 10, 13.

## Scope

### 1. Configuration

Load and validate all `VEC_*` variables from the overview reference.
Rules:

- Missing or invalid required values → structured error naming the
  variable, non-zero exit, **before any network activity** and before
  any port is opened. No insecure defaults are substituted.
- `VEC_PG_USER`, if set, must equal exactly `vector_api`; the effective
  runtime role is `vector_api` in all cases.
- `VEC_PG_MIGRATION_USER` is required and must differ from the runtime
  role. Identity collapse is a startup error.
- `VEC_PG_DATABASE` must be `vector` (migration `0004` hardcodes the
  database name in its `CONNECT` grant).
- TLS mode is the single source of transport truth: `tls` requires the
  complete runtime triple (CA, client cert, client key) and the complete
  migration pair (migration cert/key, CA defaulting to the runtime CA);
  `plain` forbids all TLS path variables. A partial triple in any mode
  is a startup error. TLS material is parsed at startup (certificate
 /key validity, and migration-pair identity distinctness from the
  runtime pair where both are parsed). Path material is read only to
  configure the TLS client; no key or credential material is ever
  logged (length counts at most).
- The four `http.Server` connection timeouts (read header, read, write,
  idle) are explicit configuration values — the unconfigured default
  server is never used in production (package 4 builds the server from
  these).

### 2. Runtime database access (pgxpool)

- One pool for the runtime identity `vector_api`, used by all
  request-serving code and by `/readyz` (package 4).
- TLS client-certificate configuration from the runtime triple; `plain`
  mode per configuration.
- **Connection identity assertion** (defense in depth; runs on the
  startup of **every** physical pool connection — and therefore as the
  fail-fast gate at startup when the pool first opens, before `serve`
  listens). The assertion establishes the **complete effective
  privilege boundary** of `vector_api` (overview invariant 4), not
  merely the nominal role. Any failure subcheck fails the connection
  with a structured error naming the failed check (and, for grant
  checks, the object, the privilege, and the source of the grant); at
  startup that means `serve` refuses to listen. Four parts:
  1. **Identity and attributes** (always, cheap): `current_user`
     equals exactly `vector_api`; the role's attributes
     `rolsuper`, `rolbypassrls`, `rolcreatedb`, and `rolcreaterole`
     are all false, and `rolinherit` is true (the default — part 3's
     INHERIT closure is sound only because the role's `INHERIT`
     attribute actually applies inherited memberships) (read from
     `pg_roles` by the role's OID); and the role is not the owner of
     the connected database (`pg_database.datdba`).
  2. **Ownership:** the runtime role owns none of: the connected
     database; the `vector_control` or `vector_data` schemas
     (`pg_namespace.nspowner`); or **any** object in either schema —
     every `pg_class` row in both namespaces (`relowner`), which by
     construction covers the migration-history table, every migrated
     table, sequence, index, and anything a later migration adds to
     those schemas as a relation; every `pg_type` row in both
     namespaces (`typowner`), which covers every migrated and
     schema-local type — including the true array type
     `vector_control._distance_metric` (`typelem` pointing at the
     `distance_metric` enum), included without exception; and every
     `pg_proc` row in both namespaces
     (`proowner`), which covers **every migrated function and
     procedure** — functions and procedures carry no `pg_class` row of
     their own, so the `pg_proc` check is an independent catalog, not
     an enumeration. No per-object name enumeration.
  3. **Exact effective grant boundary:** the runtime role's effective
     privileges on the in-scope objects must equal **exactly** the
     canonical grant set of migrations `0004`, `0005`, and
     `0006` — nothing more, nothing less.
     - **In-scope objects** (enumerated from the catalogs at check
       time — bounded, and future objects in these schemas are covered
       automatically): the `vector` database; the schemas
       `vector_control` and `vector_data`; every `pg_class` row in
       those schemas — tables, views, sequences, materialized views
       checked for table privileges (PostgreSQL 18's table privilege
       set includes `MAINTAIN`); index rows (`relkind` `i`/`I`)
       carry no independent ACL in PostgreSQL (their access rights are
       exactly the owning table's, already checked — the check asserts
       `relacl IS NULL` for them); any other relation kind present is
       checked under the no-privilege rule; every `pg_type` row in
       those schemas checked for `USAGE` — this includes the
       `distance_metric` enum, the true array type of every in-scope
       element type that carries one (identified by the
       `pg_type.typelem` relationship: the array type's `typelem`
       names its element type; implicit row (composite) types carry
       no array type, so in the released set exactly one is in scope
       — `vector_control._distance_metric`, element
       `vector_control.distance_metric`), **and the implicit row
       (composite) type created for every migrated table**, whose
       `typrelid` points at the table's `pg_class` row; every
       non-dropped
       column (`pg_attribute.attdropped` false) of every in-scope
       table, checked at column level below; every function/procedure
       in `pg_proc` in those schemas checked for `EXECUTE`.
     - **Membership traversal and closures.** Membership edges are
       read from `pg_auth_members`, whose direction is **`member →
       roleid`**: `member` is a member of `roleid`, so privileges
       and attributes flow from `roleid` down to `member`, never the
       reverse. The runtime role's **reachable membership
       subgraph** is the set of roles it is a member of at any depth,
       reached by breadth-first traversal starting from the runtime
       role's OID: a step extends the search from a reached `member`
       to the `roleid` it is a member of. Each edge carries the
       PostgreSQL 18 membership options, with distinct meanings:
       `inherit_option` controls **automatic privilege inheritance**
       — whether the `roleid`'s object privileges are inherited into
       the `member` automatically; `set_option` controls **whether
       `SET ROLE` can assume the granted `roleid`** — it is an
       assumption right, not regrant authority; `admin_option`
       controls **whether the member may grant/revoke or alter that
       membership** — the regrant authority. Over that subgraph the
       check computes the **INHERIT closure**, following only edges
       with `inherit_option` true — through these, the `roleid`'s
       object privileges are inherited into the `member`
       automatically and are what the `has_*` privilege predicates
       below count (given part 1's `rolinherit` assertion). A
       `set_option`-true edge adds no privilege the predicates can
       see: identity assumed through `SET ROLE` is invisible to the
       `has_*` privilege predicates, so no privilege equality can
       bound what `SET ROLE` into the granted role permits while it
       is assumed — which is exactly why the zero-membership rule
       below rejects such an edge. The documented simplest canonical
       form is the **zero-membership rule**: if **any** edge in the
       runtime role's reachable membership subgraph — any
       membership, any depth — has `set_option` true **or**
       `admin_option` true, the connection is rejected. A
       `set_option`-true edge is rejected because the member could
       `SET ROLE` into the granted role; an `admin_option`-true
       edge is rejected because the member may grant, revoke, or
       alter that membership — a regrant channel for a role the
       canonical shape never connects the runtime role to (the
       membership could be re-granted to other roles with default
       options, or its options altered to convey inheritance or
       assumption rights). A membership with all three options
       false conveys only what the INHERIT closure and part 4's
       privileged-role closure already bound.
     - **Effective privileges** of the runtime role on an object =
       direct ACL entries for `vector_api` ∪ direct ACL entries for
       every role in the runtime role's **INHERIT closure** ∪
       `PUBLIC` ACL entries. This is exactly the set the `has_*`
       predicates resolve when run as the runtime role. Membership
       edges outside the INHERIT closure contribute nothing here:
       `set_option` does not confer privileges (only the right to
       assume the granted role via `SET ROLE`), so under the
       zero-membership rule any reachable `set_option`-true edge is
       already a rejection, and a `set_option`-false edge conveys
       nothing unless its `inherit_option` is also true, in which
       case it is an INHERIT-closure edge; a reachable
       `admin_option`-true edge is likewise already a rejection.
     - **Canonical set** (the check encodes exactly this table;
       derived from migrations `0004`, `0005`, and `0006`):

       | Object | Effective privileges (must equal) |
       |--------|-----------------------------------|
       | database `vector` | `CONNECT` (no `CREATE`, no `TEMPORARY`) |
       | schema `vector_control` | `USAGE` (no `CREATE`) |
       | schema `vector_data` | `USAGE` (no `CREATE`) |
       | `vector_control.applications` | `SELECT`, `INSERT`, `UPDATE` |
       | `vector_control.namespaces` | `SELECT`, `INSERT`, `UPDATE` |
       | `vector_control.vector_spaces` | `SELECT` |
       | `vector_control.application_credentials` | `SELECT`, `INSERT`, `UPDATE`, `DELETE` |
       | `vector_control.schema_migrations` | **none** |
       | `vector_data.vector_records` | `SELECT`, `INSERT`, `UPDATE`, `DELETE` |
       | function `vector_control.set_updated_at` | `EXECUTE` (direct grant, `0005`) |
       | function `vector_data.validate_vector_record` | `EXECUTE` (direct grant, `0005`) |
       | every other in-scope function | **none** |
       | enum type `vector_control.distance_metric` | `USAGE` (direct grant, `0005`) |
       | the true array type of every in-scope element type that carries one — identified by the `pg_type.typelem` relationship; in the released set exactly one: `vector_control._distance_metric` (element: the `distance_metric` enum) | **dependent privilege view — no independent grant:** true array types have no independently mutable ACL — `has_type_privilege` follows the element type — so effective `USAGE` must equal the element type's canonical `USAGE`, in both directions (canonically `USAGE` here: the enum's `0005` direct `vector_api` grant), with no independent array ACL or `PUBLIC` grant on the array type |
       | the implicit row (composite) type of every migrated table — `vector_control.applications`, `vector_control.namespaces`, `vector_control.vector_spaces`, `vector_control.application_credentials`, `vector_control.schema_migrations`, `vector_data.vector_records` — each named after its table, with `typrelid` pointing at the table's `pg_class` row | **none** (`0006` revoked the default `PUBLIC` `USAGE`; no direct grant exists) |
       | every other in-scope type (every enum, composite, domain, or base type not named above; true array types are modeled by the dependent-privilege-view row, not by this row) | **none** |
       | every other in-scope object (indexes, sequences, and any future object in the two schemas) | **none** |

       The grid is object × **every applicable privilege** for that
       object class (database: `CONNECT`/`CREATE`/`TEMPORARY`; schema:
       `USAGE`/`CREATE`; table/sequence: `SELECT`/`INSERT`/`UPDATE`/
       `DELETE`/`TRUNCATE`/`REFERENCES`/`TRIGGER`/`MAINTAIN`;
       type: `USAGE`; function: `EXECUTE`). Types are distinguished by
       class: the `distance_metric` enum type carries its direct
       `0005` `USAGE` grant; the true array type of every in-scope
       element type that carries one (identified by the
       `pg_type.typelem` relationship; in the released set exactly
       one — `vector_control._distance_metric`, element
       `vector_control.distance_metric`) is a **dependent privilege
       view, not an independent grant row**: a true array type has
       no independently mutable ACL — the engine resolves
       `has_type_privilege` (and the underlying `pg_type_aclcheck`)
       for an array type by following the element type — so the
       array row's effective `USAGE` must equal the element type's
       canonical `USAGE` in both directions (a `USAGE` effective on
       the array with none canonical on the element — or the
       reverse — is a violation), and no direct-ACL provenance is
       required for it: the row's canonical state is defined
       entirely by the element type's `0005` direct `vector_api`
       `USAGE` grant (the provenance the check does verify on the
       enum) — the array type itself carries no independent ACL
       entry, no direct `vector_api` entry, and no `PUBLIC` grant,
       in its `pg_type.typacl`; the implicit row (composite) types
       are dependent types — each is named after its table and its
       `typrelid` points at the table's `pg_class` row — and carry
       no grant at all, `0006` having revoked their default `PUBLIC`
       `USAGE` and no direct grant existing (composite types carry
       no array type of their own); every other in-scope
       type (any enum, composite, domain, or base type not named
       above, including any future type in the two schemas; a
       future enum's or domain's array type joins the
       dependent-privilege-view row, not this row) carries
       nothing. Equality is checked in
       both directions: every canonical privilege must be effective,
       and **no** privilege outside the table may be effective on
       any in-scope object — including `TRUNCATE`/`REFERENCES`/
       `TRIGGER`/`MAINTAIN` on the canonical tables (PostgreSQL 18's
       `MAINTAIN` is applicable to tables and is canonically absent
       everywhere) and any privilege on `schema_migrations`.
     - **Implementable check** (catalog/`has_*`, bounded — a few
       dozen objects plus at most a few hundred column predicates;
       negligible next to the TLS handshake, so it runs on every
       connection): for each in-scope object and each applicable
       privilege, evaluate the engine's own resolution for the
       runtime role — `has_database_privilege`,
       `has_schema_privilege`, `has_table_privilege`,
       `has_column_privilege`, `has_type_privilege`,
       `has_function_privilege` — which resolve role membership and
       `PUBLIC` internally, so the true set of predicates is exactly
       the effective privilege set (the check runs as the non-
       superuser, non-owning runtime role, so the predicates measure
       grant-based privilege, not ownership or superuser bypass).
       Require that set to equal the canonical row for the object.
       **Column level (exact):** for every non-dropped column of
       every in-scope table and for each column-applicable privilege
       — `SELECT`/`INSERT`/`UPDATE`/`REFERENCES` — require
       `has_column_privilege(vector_api, '<table>.<column>', p)` to
       equal the canonical table row for that table restricted to
       the column-applicable privileges. The expected column set is
       **not** an independent row: it must be exactly the
       column-applicable subset of the table's canonical set and
       nothing else — the migrations never grant per-column, and the
       engine reports a table's privileges on each of its columns,
       so any deviation in either direction is a violation.
       **Array types (dependent privilege view):** for each in-scope
       array type identified by the `pg_type.typelem` relationship,
       require `has_type_privilege(vector_api, <array type>,
       'USAGE')` to equal the element type's canonical `USAGE` — the
       element's own row in the canonical set above — in both
       directions: a true array type has no independently mutable
       ACL, the engine resolves `has_type_privilege` for an array
       type by following the element type, so the array row's
       effective `USAGE` is the element type's `0005` direct grant
       observed through the array type — an engine-enforced
       invariant, not an independent grant. The check further
       requires the array type's `pg_type.typacl` (decoded like the
       other ACL catalogs) to carry no entry for the runtime role,
       any role in its INHERIT closure, or `PUBLIC` — no independent
       array ACL or `PUBLIC` grant — and the provenance rule below
       does not extend to array types: no required canonical grant
       is a direct `vector_api` entry in an array type's
       `typacl`, and no such entry may exist.
       **Provenance (direct grants, exact source):** privilege
       equality alone is not the check. Each required canonical grant
       must exist as a **direct ACL entry for `vector_api`** in the
       applicable catalog: `CONNECT` in `pg_database.datacl`;
       `USAGE` in `pg_namespace.nspacl` for each schema; the table
       privileges in `pg_class.relacl`; `EXECUTE` in `pg_proc.proacl`
       for the two `0005` functions; and `USAGE` in `pg_type.typacl`
       for `vector_control.distance_metric` — the dependent
       privilege-view array row is excluded from this requirement:
       its effective `USAGE` is the element enum's `0005` direct
       grant, which the engine follows through the array type (a
       true array type has no independently mutable ACL), so the
       array type carries no direct `vector_api` entry of its own
       and none is required. In the canonical
       zero-membership shape, inherited grants are **absent**: no ACL
       entry in any in-scope object may name a role in the runtime
       role's membership subgraph as grantee — a grant that reaches
       `vector_api` only through a member role or through `PUBLIC`
       fails the check even when the `has_*` predicates (and hence
       the effective union) resolve it exactly as the canonical set.
       `PUBLIC` carries at most one in-scope privilege: the
       explicitly preserved default `CONNECT` on the database
       `vector` (`0005` preserves it because `vector_api` already
       holds `CONNECT` directly). No migrated object — schema,
       table, column, function, or type, including every in-scope
       true array type — carries any other `PUBLIC` entry: `0005`
       revokes the default `PUBLIC` `USAGE` on the enum, `0006`
       revokes the default `PUBLIC` `USAGE` on the implicit row
       (composite) types, and the array types carry no independent
       ACL at all — their effective `USAGE` follows the element
       type, so a `PUBLIC` entry on an array type would be neither
       canonical nor effective.
       **Grant option (rejection, every ACL catalog):** no ACL entry
       in any in-scope object's ACL may carry a grant option (the
       `+` in an ACL privilege string) for a grantee in the runtime
       role ∪ INHERIT closure ∪ `PUBLIC`. The check inspects every
       applicable ACL catalog: `pg_database.datacl`,
       `pg_namespace.nspacl`, `pg_class.relacl` (table-level
       entries only), `pg_attribute.attacl` (column-level entries —
       column grants and options live here, not in `relacl`),
       `pg_proc.proacl`, and `pg_type.typacl` — direct, inherited,
       and `PUBLIC` entries alike. The canonical grants of
       `0004`–`0006` carry no grant option; any grant option is an
       unauthorized regrant channel.
       **Default ACLs:** additionally require **no `pg_default_acl`
       entry** for the two schemas that grants to the runtime role,
       its closure, or `PUBLIC` (default privileges would silently
       extend the boundary to future objects). On any violation, name
       the object (the column, where applicable), the privilege, and
       the source — direct grant, via a specific member role (from
       the closure walk), or `PUBLIC` — by decoding the object's ACL
       from the applicable catalog (`pg_database.datacl`,
       `pg_namespace.nspacl`, `pg_class.relacl`,
       `pg_attribute.attacl`, `pg_proc.proacl`, `pg_type.typacl`)
       restricted to grantees in the runtime role ∪ closure ∪
       `PUBLIC`; for column-level entries (grant option included)
       the `pg_attribute.attacl` decode proves the grant-option
       rejection, and the `pg_class.relacl` decode proves it for
       table-level entries — column grants and options never appear
       in `relacl`.
  4. **Membership closure — privileged roles and membership
     options:** compute the **full** membership closure — every
     reachable membership edge, whatever its options, any depth
     (breadth-first over `pg_auth_members` from the runtime role's
     OID) and reject the connection if **any** reachable edge has
     `admin_option` true (an explicit check of the edge's options:
     part 3's zero-membership rule rejects every reachable
     `set_option`-true edge; this part rejects every reachable
     `admin_option`-true edge — the admin option is the regrant
     authority, so such an edge is a membership the canonical shape
     does not have, whether or not it currently conveys privilege
     or assumption) or if **any** role in the closure (the runtime
     role itself is covered by parts 1–2) is: the configured
     migration identity; `rolsuper`; `rolbypassrls`; `rolcreatedb`
     or `rolcreaterole`; or an owner of the `vector` database, of
     the `vector_control`/`vector_data` schemas, or of any object in
     them — every `pg_class` row (`relowner`), every `pg_type` row
     (`typowner`), and every `pg_proc` row (`proowner`) in those
     schemas, including every migrated function, whose ownership is
     visible only in `pg_proc.proowner`. This is an enumerated
     threat set against a finite, currently-existing closure — the
     check does not attempt (and is
     not required to) prove that no role could ever be created or
     granted anything later. Together with part 3's rejection of
     every reachable `set_option`-true membership edge, this part —
     which carries the rule's `admin_option` half and the
     privileged-role set — is the closure half of the zero-membership
     rule: the canonical runtime role has no
     memberships at all, so both halves pass by
     construction.
  
  The canonical state passes by construction: infrastructure creates
  `vector_api` as bare `LOGIN` with **zero** role memberships, and
  `0004`–`0006` establish exactly the canonical set (the `0004`
  direct grants, the `0005`/`0006` direct function/type grants and
  `PUBLIC` baseline, nothing else). The pool is never configured as
  owner or superuser, and nothing in the code path can grant
  `BYPASSRLS`.
- **Bounded acquisition:** pool acquisition is bounded by an explicit
  dependency deadline (proposal: 5 s); exhaustion or failure while the
  caller's context is still active is an `unavailable` (503) condition
  per the overview error model. This bounded acquisition is used by the
  transaction helpers in package 2.
- The pool is the only runtime connection factory in the codebase; no
  ad-hoc `pgx.Connect` on the serving path.

### 3. Migration identity connection

- A separate, short-lived connection as `VEC_PG_MIGRATION_USER`, used
  **only** for the migration step (both subcommands), with the
  migration TLS configuration.
- It is opened during the migration step and closed before serving
  begins. No serving code path can reach it; there is no pool for it.
- **Connection identity assertion** (defense in depth, before any
  schema work): after connect, verify on this connection that
  `current_user` equals exactly the configured canonical migration
  identity (`VEC_PG_MIGRATION_USER`); that this identity **differs**
  from the runtime role `vector_api` (runtime-role substitution is
  rejected); that the role is **not** a superuser (`rolsuper`, read via
  the public `pg_roles` view); and that the role **owns** the connected
  `vector` database (`pg_database.datdba` equals the role's OID). Any
  mismatch — unexpected role, superuser, runtime-role substitution, or
  ownership mismatch — is a fatal, structured error naming the failed
  check. The assertion is the only identity check: the runner performs
  no further privilege probing and acts as the migration identity,
  failing actionably (naming the missing prerequisite or privilege)
  when it cannot. The database-ownership check here is the connect-time
  half of the canonical ownership contract (overview invariant 4):
  the complete sweep — both migrated schemas and every in-scope
  `pg_class`, `pg_type`, and `pg_proc` object in them — is the
  canonical ownership gate (Scope 4), run under the advisory lock and
  authoritative over all pre-existing migrated state.
- The migration TLS client certificate/key pair stays distinct from
  the runtime pair (configuration validates both pairs by parsing);
  the two identities never share transport credentials.

### 4. Embedded canonical migration runner

- **Embedding:** migrations are embedded in the binary (`go:embed` over
  `migrations/*.sql`). The image and the process never depend on a
  filesystem migration path. Released files are never modified; new
  behavior enters only as a new versioned file.
- **Versioning:** files are `NNNN_name.sql`; versions are the numeric
  prefixes, applied in ascending order. Checksum is the SHA-256 of the
  file bytes.
- **Prerequisite validation** (as the migration identity, before any
  schema work):
  - the `vector` database is the one connected to (guaranteed by
    configuration validation);
  - the `vector` extension (pgvector) exists — no migration creates it;
    it is pre-provisioned by infrastructure (fatal, actionable error if
    absent);
  - the `vector_api` role exists — required by `0004`'s grants (fatal,
    actionable error if absent).
- **Bootstrap (canonical shape).** The runner supports one provisioning
  shape: the **migration identity owns the `vector` database**
  (canonically a role such as `vector_owner`). As that identity the
  runner executes, idempotently:

  ```sql
  CREATE SCHEMA IF NOT EXISTS vector_control;
  CREATE TABLE IF NOT EXISTS vector_control.schema_migrations (
      version     INTEGER PRIMARY KEY,
      name        TEXT NOT NULL,
      checksum    TEXT NOT NULL,
      applied_at  TIMESTAMPTZ NOT NULL DEFAULT now()
  );
  ```

  **Pre-existing objects (canonical ownership gate).** Under the
  advisory lock, before the bootstrap DDL, the gate verifies the
  **complete canonical ownership contract** (overview invariant 4):
  the migration identity must own the `vector` database
  (`pg_database.datdba`), both migrated schemas
  (`pg_namespace.nspowner` for `vector_control` and `vector_data`),
  and **every in-scope migrated object** — every `pg_class` row in
  those schemas (`relowner`), covering the migration-history table,
  every migrated table, sequence, index, view, and any relation a
  later migration adds to those schemas; every `pg_type` row in those
  schemas (`typowner`), covering every migrated and schema-local
  type; and every `pg_proc` row in those schemas (`proowner`),
  covering **every migrated function and procedure** — functions and
  procedures carry no `pg_class` row of their own, so the `pg_proc`
  sweep is an independent catalog, not an enumeration of names. Any
  object that exists and is owned by a role other than the migration
  identity — `vector_api`, a role reachable from it, or an unrelated
  unreachable role alike — is a fatal, structured error naming the
  object, the catalog it was found in, and its actual owner.
  **The gate is the authoritative owner check: acceptance happens
  only after this validation passes** — a pre-existing migrated
  schema or object is accepted (and the `IF NOT EXISTS` clauses are
  allowed to adopt it) **only** when the check succeeds on that same
  locked pass; it is never adopted silently, and no bootstrap DDL
  runs against a non-conforming pre-existing object. Startup and
  migration convergence therefore fail closed on any ownership
  mismatch **before serving and before any pre-existing migrated
  state is accepted** — a `serve` whose migration step reaches the
  gate never opens the runtime pool or a listener, and `migrate`
  exits non-zero with no bootstrap DDL and no history write. Fresh
  databases have nothing to check; converged databases pass by
  construction (the migration identity created every in-scope
  object), and the gate runs on **every** pass (including
  re-converged databases), so a post-convergence ownership change of
  the database, a migrated schema, or any in-scope object to any
  other role is fatal at the next `migrate` or `serve` before any
  DDL or history write. The gate checks the objects that exist at
  check time; objects created later in the same pass (bootstrap DDL,
  applied migrations) are owned by the migration identity by
  construction and are checked on every subsequent pass.

  The history table is written **only** by the migration identity.
  Migration `0004`'s grants give `vector_api` exactly: `CONNECT` on
  database `vector`; `USAGE` on schemas `vector_control` and
  `vector_data`; `SELECT, INSERT, UPDATE` on
  `vector_control.applications` and `vector_control.namespaces`;
  `SELECT` on `vector_control.vector_spaces`; `SELECT, INSERT,
  UPDATE, DELETE` on `vector_control.application_credentials` and on
  `vector_data.vector_records`. `vector_control.schema_migrations` is
  the only schema object with **no** runtime grants, so the runtime
  role can never read or write migration history. Alternate
  provisioning shapes (pre-provisioned schemas, explicit partial
  grants) are **deliberately out of scope** for this release; if a
  deployment needs one, it is a documented architecture decision, not
  a runner feature.
- **Single-flight:** the runner takes
  `pg_advisory_lock(hashtext('vector.service.migration'))` with a
  bounded wait of `VEC_MIGRATION_LOCK_WAIT` (proposal: 300 s, via
  `pg_try_advisory_lock` polling). Exceeding the wait is fatal (non-zero
  exit, structured error). The lock is acquired **before bootstrap** —
  after the read-only identity assertion and prerequisite validation —
  and is held through the bootstrap DDL, the history read, checksum
  validation, the application of every pending migration, and every
  history write. It is released on all exit paths (and dropped by
  PostgreSQL on disconnect as a backstop). No bootstrap, schema, or
  history mutation occurs outside the lock.
- **Released migrations carry outer transaction framing.** Every
  released file (`0001`–`0006`) begins with a `BEGIN;` line and ends
  with a `COMMIT;` line. Executing such a file inside the runner's own
  transaction would let the file's `COMMIT;` commit early and break
  schema-plus-history atomicity, so the runner strips the outer
  framing from the **execution stream only**:
  - the checksum is always the SHA-256 of the **original embedded file
    bytes**; stripping never alters the checksum or the stored bytes;
  - a file is well-formed only if (a) its first non-blank,
    non-comment line is exactly `BEGIN;`, (b) its last non-blank,
    non-comment line is exactly `COMMIT;` (keywords case-insensitive,
    semicolon required), and (c) after removing exactly those two
    framing lines, the body contains no other top-level transaction
    control statement (`BEGIN`, `START TRANSACTION`, `COMMIT`, `END`,
    `ROLLBACK`, `ABORT`) outside `$$ … $$` quoted bodies and
    single-quoted string literals — checked by a small scanner that
    skips both constructs;
  - **malformed framing is fatal, not repaired:** a file violating any
    clause is rejected before any migration is applied (structured
    error naming the file and the violated clause, non-zero exit). The
    runner never applies a partially stripped stream, and a released
    file is never rewritten to "fix" framing — a future migration that
    breaks the convention is a release-process defect, not a runner
    behavior.
- **Per-migration atomicity:** for each pending migration the runner
  executes exactly one transaction, owned by the runner: `BEGIN` → the
  framing-stripped migration body (verbatim) → the history `INSERT`
  (version, name, checksum of the original bytes) → `COMMIT`. The
  migration's own outer framing is not re-executed. Any failure — in
  any body statement, or at the history `INSERT` itself — rolls back
  the whole transaction: no partial schema change and no history row.
  A migration whose final statement fails therefore leaves the
  database exactly as the previous migration left it, and a corrected
  re-run applies cleanly.
- **Checksum drift:** an applied version whose stored checksum differs
  from the embedded file's checksum is fatal: no further migrations,
  non-zero exit, structured drift error. This protects the released
  contract.
- **Idempotence:** with no pending migrations the runner — still under
  the advisory lock — re-verifies the checksums of all applied rows,
  releases the lock, and exits 0. It never re-applies, downgrades, or
  repairs.
- **Test seam (package-internal):** the runner's core is written over a
  package-private source abstraction so the package-internal tests (and
  package 5b's migration-matrix case 6) can exercise a synthetic,
  failing migration set. The seam also drives the **history-INSERT
  boundary** test with a real PostgreSQL failure mechanism — not a
  failing migration body:
  - on a real, provisioned database, a package-internal test installs
    on a second connection (as the migration identity — the
    history table's owner) a **test-only** `BEFORE INSERT` trigger on
    `vector_control.schema_migrations` whose function `RAISE
    EXCEPTION`s on every insert (SQLSTATE `P0001`);
  - a synthetic test-only migration whose body is valid DDL (e.g. a
    small `vector_data` fixture table) is then applied by the
    production runner through the seam: the DDL executes **and
    succeeds**, the runner's history `INSERT` for that migration is
    rejected by the trigger, and the runner returns failure;
  - the test asserts, on a read connection, that **both** rolled back
    in the runner's single transaction: the migration's schema object
    is absent **and** no history row exists for it;
  - the test **removes the failure fixture** (`DROP TRIGGER`, drop the
    trigger function) — the trigger lives only in the disposable
    suite database, never in a released object — and re-runs the
    unchanged migration set; the corrected re-run converges cleanly
    (exit 0, history row present, DDL object present), proving the
    rollback left no residue;
  - released migration files are never modified by the test; the test
    asserts the embedded files' bytes/checksums are unchanged after
    the case.
  The production entry points (`migrate.Run` for both subcommands)
  always use the embedded source; there is no exported override.

### 5. Subcommands and startup order

- `migrate`: load config → connect as migration identity (identity
  assertion) → validate prerequisites → acquire the advisory lock
  (bounded wait) → bootstrap → read history and validate checksums →
  apply pending (one runner-owned transaction each, ending in the
  history `INSERT`) → release lock → exit 0. Non-zero exit on any
  failure; the lock is released — or dropped by the database on
  disconnect — on every failure path. No serving, no runtime pool.
- `serve`: load config → run the full migration step (same runner, same
  code path as `migrate` — one runner) → **on success** open the runtime
  pool, set the in-process **migrations-converged** flag (consumed by
  `/readyz`, package 4), and hand off to the serving lifecycle
  (package 4). **On migration failure: refuse to serve, structured
  error, non-zero exit** (fail-fast; the migration step is the single
  remediation path).
- Startup logs: one structured summary (listen address, pool
  parameters, migration result, configured bound values — no secret
  material).

### 6. pgvector text codec

- Register a text-format codec for pgvector's `vector` type in the
  runtime pool (and in test connections): the type OID is discovered at
  startup from `pg_type` (name `vector`), and values round-trip as
  pgvector's text form `[a,b,c,...]`.
- This is what lets package 3 bind query vectors as parameters and read
  stored vectors back; the stored column is an unconstrained `vector`,
  and per-dimension casts are done in SQL (`::vector(D)`), not in the
  codec.
- A database without the extension cannot be connected to by the
  runtime pool (prerequisite validation runs before the pool is used).

## Interfaces and boundaries

- `cmd/vector-service` — `serve` and `migrate` subcommand dispatch.
- `internal/config` — configuration loading/validation; the only place
  `VEC_*` is read.
- `internal/db` — runtime pool construction (identity assertion,
  bounded acquisition, codec registration).
- `internal/migrate` — embedded source, runner (bootstrap, advisory
  lock, framing check and execution-stream stripping, per-migration
  runner-owned transactions, drift check), the migrations-converged
  state, and the package-internal source seam.
- Exposes to package 4: the pool, the converged flag, and the
  configuration values (timeouts, bounds) the serving layer needs.
- Consumes: nothing from packages 2–4.

## Invariants and correctness constraints

- Migration immutability and forward-only (overview invariant 10);
  checksum drift is fatal; per-migration atomicity in one runner-owned
  transaction (schema statements + history `INSERT`); checksums always
  over the original embedded bytes; malformed framing is fatal before
  anything is applied; advisory-lock single-flight with bounded wait.
- The advisory lock is acquired before bootstrap and held through
  bootstrap, checksum validation, all pending migrations, and every
  history write; no schema or history mutation occurs outside the lock.
- The two identities never mix: no code path opens a connection that
  could serve both roles; the migration connection is closed before
  serving; `vector_api` holds only the canonical grant set of
  migrations `0004`–`0006` (the `0004` schema `USAGE` and table grants
  listed in Scope 4, plus the `0005`/`0006` direct function/type
  grants and `PUBLIC` baseline) and no grant on
  `vector_control.schema_migrations`.
- Identity assertions fail closed on the **complete effective
  privilege boundary** (overview invariant 4; Scope 2): runtime
  `current_user` exactly `vector_api`; `rolsuper`/`rolbypassrls`/
  `rolcreatedb`/`rolcreaterole` all false and `rolinherit` true; no
  ownership of the database, the migrated schemas, or any object in
  them — every `pg_class` row, every `pg_type` row, and every
  `pg_proc` row (`relowner`/`typowner`/`proowner`), including every
  migrated function; no reachable `set_option`-true or
  `admin_option`-true membership edge (zero-membership rule, part 3);
  and no role in the runtime role's
  full transitive membership closure is the migration identity,
  superuser,
  `BYPASSRLS`, `CREATEDB`/`CREATEROLE`, or an owner of the database
  or any object in the migrated schemas — every migrated relation,
  type, and function (`pg_class.relowner`, `pg_type.typowner`,
  `pg_proc.proowner`) (part 4). The role's effective
  privileges (direct + INHERIT-closure member roles + `PUBLIC`) equal
  **exactly** the canonical grant set of migrations `0004`, `0005`,
  and `0006` on every in-scope object — including no privilege at all
  on `vector_control.schema_migrations` or any other object outside
  the set, no `CREATE` anywhere in the migrated schemas or the
  database, no `MAINTAIN` (PostgreSQL 18) anywhere; at column level,
  `has_column_privilege` equals the column-applicable subset of the
  table's canonical set on every non-dropped column of every in-scope
  table; at type level, the effective `USAGE` of every in-scope true
  array type (the `pg_type.typelem` relationship) equals its element
  type's canonical `USAGE` in both directions (the dependent
  privilege view: a true array type has no independently mutable
  ACL — `has_type_privilege` follows the element type —), and the
  array type's `pg_type.typacl` carries no entry for the runtime
  role, its closure, or `PUBLIC` (no independent array ACL or
  `PUBLIC` grant); each required canonical grant is a **direct**
  entry for `vector_api` in its applicable catalog — no required
  grant arrives via a member role or `PUBLIC` (the dependent
  privilege-view array row excepted from the direct-entry
  requirement: its effective `USAGE` is the element enum's `0005`
  direct grant, which the engine follows through the array type, and
  the array type carries no direct entry of its own)
  — and `PUBLIC` carries only the preserved `CONNECT` on the
  database `vector`, and no privilege on any other migrated object
  (no `PUBLIC` `USAGE` on any in-scope type, array types included);
  no grant option (`+`) in any in-scope ACL entry
  for the runtime role, its closure, or `PUBLIC` — checked across
  `pg_database.datacl`, `pg_namespace.nspacl`, `pg_class.relacl`
  (table-level only), `pg_attribute.attacl` (column-level),
  `pg_proc.proacl`,
  and `pg_type.typacl`; and no `pg_default_acl`
  entry in the two schemas reaches the runtime role, its closure, or
  `PUBLIC`. The check is exact and bounded against the current
  catalog state; it is not a proof about future grants. Migration
  identity: `current_user` exactly the configured canonical identity,
  distinct from the runtime role, non-superuser, owner of the
  `vector` database; and the canonical ownership gate (Scope 4)
  requires the migration identity to own both migrated schemas and
  **every in-scope object** — every `pg_class` row (`relowner`),
  every `pg_type` row (`typowner`), and every `pg_proc` row
  (`proowner`) in `vector_control` and `vector_data` — including the
  migration-history table, every migrated table, sequence, index,
  type, and function, and any future object those schemas acquire.
  Startup and migration convergence fail closed on any ownership
  mismatch **before serving and before any pre-existing migrated
  state is accepted**. Any runtime identity, attribute, ownership,
  grant-boundary, or reachable-privileged-role violation — and any
  migration identity substitution, superuser, or ownership mismatch —
  is fatal. The two ownership rules are separate and both enforced:
  the gate requires the migration identity to own everything
  in-scope, while the runtime pool assertion (parts 2 and 4)
  requires that `vector_api` and no reachable role own anything
  in-scope. Pre-existing migrated schemas or objects are
  **accepted only after the canonical-ownership validation passes**
  (Scope 4); the `IF NOT EXISTS` bootstrap clauses never silently
  adopt an object owned by a non-migration identity, and the gate
  re-runs on every `migrate`/`serve` pass.
- No session-persistent application context anywhere (invariant 3); this
  package installs no `set_config` — that belongs exclusively to the
  package-2 transaction helpers.
- Never log TLS key material, credential material, or certificate
  contents (invariant 12).
- Fail-fast startup: a `serve` whose migration step fails never listens.

## Validation

- **Configuration (unit, table-driven):** every required variable
  missing individually; identity collapse; wrong runtime role; wrong
  database name; `plain` mode with TLS paths set; partial triples;
  invalid TLS mode; invalid timeout/duration values; accepted inbound
  forms for every variable.
- **Framing (unit, no database):** for every embedded file, the
  execution stream equals the original bytes minus exactly the outer
  `BEGIN;`/`COMMIT;` framing lines, and the checksum is computed over
  the original bytes; the scanner does not treat PL/pgSQL `$$` bodies
  (e.g. the `0001` trigger function's `BEGIN`/`END;`) or single-quoted
  literals containing `BEGIN;`-like text as transaction control;
  malformed-framing rejection via the package-internal seam: missing
  leading `BEGIN;`, missing trailing `COMMIT;`, a mid-body top-level
  `COMMIT;` outside a `$$` body → fatal framing error naming the file,
  nothing applied, non-zero exit.
- **Runner (real PostgreSQL via the 5a harness; full matrix in 5b):**
  P1's own tests must prove at minimum: empty → latest (all six
  migrations applied; history rows with checksums; schemas, tables,
  forced RLS, policies, trigger, seeded space row, HNSW index exist;
  **canonical owner assertion** — the migration identity owns the
  `vector` database, both migrated schemas, and every in-scope
  `pg_class` row (`relowner`), `pg_type` row (`typowner`), and
  `pg_proc` row (`proowner`) in them: the history table, every
  migrated table, sequence, index, type — including the array
  `pg_type` row of `vector_control._distance_metric` — and both
  migrated functions, while `vector_api` owns none of them; and
  `vector_api`'s effective
  privileges match the canonical set of
  `0004`–`0006` — including the `0005`/`0006` `PUBLIC` baseline, the
  column-level and grant-option checks, and the array-type
  dependent-privilege-view assertions on real PostgreSQL:
  `has_type_privilege(vector_api, 'vector_control.distance_metric',
  'USAGE')` true and
  `has_type_privilege(vector_api, 'vector_control._distance_metric',
  'USAGE')` true, each equal to the element type's canonical `USAGE`
  (the array row's effective `USAGE` must equal the enum's `0005`
  direct `vector_api` grant in both directions — the engine resolves
  the array type's privilege check against the element type), and
  the array type's `pg_type.typacl` decodes to no entry for
  `vector_api`, any closure role, or `PUBLIC` (no independent array
  ACL or `PUBLIC` grant), while the enum's and every implicit row
  type's `pg_type.typacl` decode to no `PUBLIC` entry (`0005`/`0006`
  baseline) — and a live `vector_api` connection to the database;
  the complete assertion set passes on the converged canonical
  database (canonical startup passes); repeated run is a no-op (exit
  0, history unchanged); tampered checksum → fatal drift, nothing
  applied;
  **simultaneous fresh-database runners** — two concurrent `migrate.Run`
  invocations starting from an empty database, racing on the bootstrap
  itself → both exit 0, the history table exists, and the history has
  exactly one row per migration; a failing migration statement
  (synthetic source via the package-internal seam) → non-zero exit, no
  history row, schema unchanged, clean re-run after correction;
  **failure at the history-INSERT boundary** (synthetic source via the
  package-internal seam; the real-PostgreSQL test-only trigger
  mechanism described under the test seam in Scope 4) → the
  migration's DDL succeeds, the history `INSERT` is rejected, the run
  fails, **both** the DDL and the would-be history row roll back, the
  failure fixture is removed, and the corrected re-run converges
  cleanly.
- **Migration-identity assertions (integration, negative matrix):**
  using harness provisioning variants, each of the following is
  rejected fatally **before bootstrap** with a structured error naming
  the failed check, non-zero exit, and nothing is created: a
  connection whose `current_user` is not the configured migration
  identity; a superuser migration identity; a migration identity equal
  to `vector_api` (runtime-role substitution); a migration identity
  that does not own the `vector` database. Positive path: the
  canonical harness identity passes all assertions.
- **Runtime-identity assertions (integration, negative matrix):** the
  complete effective privilege boundary of the runtime pool (Scope 2)
  is exercised against harness **negative-identity variants** (5a):
  the canonical shape is broken by a provisioning mutation —
  ownership (of schemas, migrated tables, and migrated functions),
  privilege (table-, column-, function-, type-, schema-, and
  database-level, including grant options in every ACL catalog and
  `MAINTAIN`), provenance substitution (a direct grant revoked and
  replaced via an inherited role or `PUBLIC` with the effective
  set unchanged), membership (including `set_option`-true and
  `admin_option`-true edges), and
  role-attribute mutations applied by the harness bootstrap identity,
  and fresh roles created by it where a case needs one —
  pre-convergence or post-convergence as named per case. The
  runtime-pool cases are all **post-convergence** (the pool opens
  only after the migration step succeeds, so that is where the
  assertion can fire). For every case the outcome is the same shape:
  a **fatal, structured error naming the failed check** (for grant
  and closure cases: the object — the column, where applicable — and
  privilege, and whether the source is a direct grant, a specific
  reachable member role, or `PUBLIC`; for membership cases: the role
  and the offending edge/flag; for attribute cases: the attribute),
  **and the process
  fails before serving** — the migration gate passes where named, and
  `serve` never opens the listener (no `/readyz` 200, no port); the
  pre-convergence gate cases fail `migrate` non-zero with no bootstrap
  DDL or history write:
  1. `vector_api` owns the `vector_control` schema (pre-convergence
     variant) → rejected by the canonical-ownership gate (Scope 4)
     before bootstrap; the same state reached by post-convergence
     `ALTER SCHEMA … OWNER TO vector_api` fails the same gate at the
     next `migrate`/`serve` pass.
  2. `vector_api` owns the `vector_data` schema → same gate rejection.
  3. `vector_api` owns the migration-history table (pre-created
     `vector_control.schema_migrations` owned by `vector_api`) → same
     gate rejection.
  4. `vector_api` owns a **migrated table** (post-convergence
     `ALTER TABLE vector_data.vector_records OWNER TO vector_api`):
     the canonical-ownership gate (Scope 4) sweeps every in-scope
     `pg_class` row, so it rejects the transfer before bootstrap,
     history read, or pool open — `migrate`/`serve` fail non-zero
     with a structured error naming the table and its owner, and
     `serve` never opens the listener. The runtime pool identity
     assertion (part 2) independently enforces the same
     no-runtime-ownership rule and would fail the connection on the
     same `relowner` value; in the `serve` flow the gate fires first.
  5. `vector_api` has **direct membership** in the migration identity
     (`GRANT <migration identity> TO vector_api WITH INHERIT FALSE,
     SET FALSE, ADMIN FALSE` — all membership flags set explicitly, so
     the rejection is the privileged-role closure, not a
     membership-option rejection) → part 4 fails: the closure reaches
     the migration identity, which owns the migrated objects → `serve`
     refuses to listen.
  6. `vector_api` has **transitive membership** (a fresh intermediate
     role: `GRANT <migration identity> TO G WITH INHERIT FALSE, SET
     FALSE, ADMIN FALSE; GRANT G TO vector_api WITH INHERIT FALSE, SET
     FALSE, ADMIN FALSE`) → part 4 fails on the two-hop path to the
     migration identity → `serve` refuses to listen.
  7. `vector_api` has an **extra direct grant** outside the canonical
     `0004`–`0006` set: `GRANT SELECT ON
     vector_control.schema_migrations TO vector_api` (canonically
     none) and a `TRUNCATE` variant on `vector_data.vector_records`
     (canonically absent) → part 3 fails naming the object, the
     privilege, and the source (direct grant) → `serve` refuses to
     listen.
  8. `vector_api` has an **inherited grant** through a harmless
     reachable role: a fresh `G` (no privileges of its own) receives
     `GRANT SELECT ON vector_control.schema_migrations TO G`, then
     `GRANT G TO vector_api WITH INHERIT TRUE, SET FALSE, ADMIN FALSE`
     (all membership flags set explicitly: `INHERIT` true is what
     makes the grant effective, while `SET`/`ADMIN` false keep the
     rejection on the grant boundary) → part 3 fails naming the
     object, the privilege, and the source (reachable member role
     `G`) → `serve` refuses to listen.
  9. `vector_api` has a **reachable privileged role** (each
     membership grant is explicit: `WITH INHERIT FALSE, SET FALSE,
     ADMIN FALSE`, isolating the privileged-role closure from the
     membership-option rejections): (a) a fresh role `H` owns a
     migrated object (`ALTER TABLE
     vector_control.applications OWNER TO H`) plus `GRANT H TO
     vector_api WITH INHERIT FALSE, SET FALSE, ADMIN FALSE` → the
     canonical-ownership gate rejects the transfer first (a
     non-migration role owns a migrated object) and part 4
     independently fails (a closure role owns a migrated object);
     (b) a fresh role
     `P` created `WITH BYPASSRLS` plus `GRANT P TO vector_api WITH
     INHERIT FALSE, SET FALSE, ADMIN FALSE` → part 4 fails (a closure
     role has `BYPASSRLS`); (c) a fresh superuser role `S` plus
     `GRANT S TO vector_api WITH INHERIT FALSE, SET FALSE, ADMIN
     FALSE` → part 4 fails (a closure role is a superuser). `serve`
     refuses to listen in all three.
  10. `ALTER ROLE vector_api BYPASSRLS TRUE` → part 1 fails
      (`rolbypassrls` is true) → `serve` refuses to listen.
  11. `vector_api` has a **direct column grant** beyond the table's
      canonical set: `GRANT SELECT (version) ON
      vector_control.schema_migrations TO vector_api` (the table is
      canonically none) and a beyond-table variant on a canonical
      table: `GRANT REFERENCES (object_id) ON
      vector_data.vector_records TO vector_api` (canonically no
      `REFERENCES`) → part 3's column-level check fails naming the
      object, the column, the privilege, and the source (direct
      grant) → `serve` refuses to listen.
  12. `vector_api` has an **inherited column grant** through a
      harmless reachable role: a fresh `G` (no privileges of its own)
      receives `GRANT SELECT (version) ON
      vector_control.schema_migrations TO G` (or a beyond-table
      column privilege on a canonical table), then `GRANT G TO
      vector_api WITH INHERIT TRUE, SET FALSE, ADMIN FALSE` → part 3's
      column-level check fails naming the
      object, the column, the privilege, and the source (reachable
      member role `G`) → `serve` refuses to listen.
  13. `vector_api` has a **`PUBLIC` column grant**: `GRANT
      REFERENCES (object_id) ON vector_data.vector_records TO PUBLIC`
      (canonically absent) → part 3's column-level check fails naming
      the object, the column, the privilege, and the source (`PUBLIC`)
      → `serve` refuses to listen.
  14. `vector_api` has a **grant option** in any in-scope ACL
      catalog, one case per catalog (all are negative fixtures against
      real PostgreSQL): column — `GRANT SELECT (version) ON
      vector_control.schema_migrations TO vector_api WITH GRANT
      OPTION`; database — `GRANT CONNECT ON DATABASE vector TO
      vector_api WITH GRANT OPTION`; schema — `GRANT USAGE ON SCHEMA
      vector_data TO vector_api WITH GRANT OPTION`; function —
      `GRANT EXECUTE ON FUNCTION vector_data.validate_vector_record()
      TO vector_api WITH GRANT OPTION`; type — `GRANT USAGE ON TYPE
      vector_control.distance_metric TO vector_api WITH GRANT
      OPTION`; and the table variants `GRANT SELECT ON
      vector_control.applications TO vector_api WITH GRANT OPTION`
      and a table-level grant option on the canonical
      `vector_data.vector_records` table (column grants and options
      live in `pg_attribute.attacl`, not `relacl`). In each, part 3's
      grant-option rejection fails naming the object, the privilege,
      and the source (direct grant carrying a grant option) → `serve`
      refuses to listen.
  15. `vector_api` is granted **`MAINTAIN`** (PostgreSQL 18) on an
      in-scope table: `GRANT MAINTAIN ON vector_data.vector_records
      TO vector_api` (canonically absent everywhere) → part 3 fails
      naming the object, the privilege, and the source (direct grant)
      → `serve` refuses to listen.
  16. `vector_api` has a **reachable `set_option`-true membership**
      in a harmless role: a fresh `G` (no privileges, no ownership,
      no privileged attributes) with
      `GRANT G TO vector_api WITH INHERIT FALSE, SET TRUE, ADMIN
      FALSE` (direct — all membership flags set explicitly), and a
      two-hop variant through a fresh intermediate `M` (`GRANT G TO
      M WITH INHERIT FALSE, SET TRUE, ADMIN FALSE; GRANT M TO
      vector_api WITH INHERIT FALSE, SET TRUE, ADMIN FALSE`) → part
      3's zero-membership rule fails on the `set_option`-true edge at
      depth one and depth two → `serve` refuses to listen in both
      forms; the privilege grid itself is unbroken, so these cases
      prove the rejection is the closure rule, not the privilege
      equality.
  17. `vector_api` has a **provenance substitution — inherited**:
      the harness revokes one direct canonical grant and grants the
      same privilege through a harmless fresh role `G` (no other
      privileges, no ownership, no privileged attributes) with
      `GRANT G TO vector_api WITH INHERIT TRUE, SET FALSE, ADMIN FALSE`,
      so the effective set — every `has_*` predicate — is unchanged,
      e.g. `REVOKE CONNECT ON DATABASE vector FROM vector_api; GRANT
      CONNECT ON DATABASE vector TO G; GRANT G TO vector_api WITH
      INHERIT TRUE, SET FALSE, ADMIN FALSE` (and one form each for a
      schema `USAGE`, a table privilege, a column privilege, a
      function `EXECUTE`, and a type `USAGE`). Part 3's provenance
      check fails:
      the required grant has no direct `vector_api` entry in the
      applicable catalog (the explicit `SET FALSE, ADMIN FALSE` membership
      flags keep the zero-membership rule satisfied, so the rejection
      is provenance alone) → `serve` refuses to
      listen. The case asserts the `has_*` predicates resolve exactly
      as in the canonical state, proving the rejection is provenance,
      not effective-set equality.
  18. `vector_api` has a **provenance substitution — `PUBLIC`**: the
      harness revokes one direct canonical grant and grants the same
      privilege to `PUBLIC`, leaving the effective set — every
      `has_*` predicate — unchanged, e.g. `REVOKE USAGE ON SCHEMA
      vector_data FROM vector_api; GRANT USAGE ON SCHEMA vector_data
      TO PUBLIC` (and one form each for a table privilege, a column
      privilege, a function `EXECUTE`, and a type `USAGE`). Part 3's
      provenance check fails: the required grant has no direct
      `vector_api` entry, and `PUBLIC` now carries a privilege on a
      migrated object beyond the preserved database `CONNECT` →
      `serve` refuses to listen. As in case 17, the `has_*`
      predicates are asserted unchanged, proving the rejection is
      provenance, not effective-set equality.
  19. A **migrated function is owned by the runtime role or by a
      reachable role** (negative fixtures on real PostgreSQL; the
      harness bootstrap identity, a superuser, performs the ownership
      transfer): (a) post-convergence `ALTER FUNCTION
      vector_data.validate_vector_record() OWNER TO vector_api` →
      the canonical-ownership gate (Scope 4) rejects the transfer on
      `pg_proc.proowner` before bootstrap, history read, or pool
      open — `serve` fails non-zero and never opens the listener; the
      runtime pool identity assertion (part 2) independently enforces
      the same no-runtime-ownership rule on `pg_proc.proowner`;
      (b) post-convergence `ALTER FUNCTION
      vector_control.set_updated_at() OWNER TO H` on a fresh role `H`
      plus `GRANT H TO vector_api WITH INHERIT FALSE, SET FALSE,
      ADMIN FALSE` → the canonical-ownership gate rejects the
      transfer first (a non-migration role owns a migrated function)
      and part 4 independently fails (a closure role owns a migrated
      function) → `serve` refuses to listen either way.
  20. `vector_api` has a **reachable `admin_option`-true membership**
      in a harmless role (release-critical fixture; all membership
      flags set explicitly): a fresh `G` (no privileges, no
      ownership, no privileged attributes) with
      `GRANT G TO vector_api WITH INHERIT FALSE, SET FALSE, ADMIN
      TRUE` — the edge conveys no privilege (it is not in the
      INHERIT closure) and permits no `SET ROLE` assumption
      (`set_option` false), but the member may grant, revoke, or
      alter the membership — an unauthorized regrant channel — and a
      two-hop variant through a fresh intermediate `M` (`GRANT G TO
      M WITH INHERIT FALSE, SET FALSE, ADMIN TRUE; GRANT M TO
      vector_api WITH INHERIT FALSE, SET FALSE, ADMIN TRUE`) →
      part 4's explicit `admin_option` check fails on the edge at
      depth one and depth two → `serve` refuses to listen in both
      forms before opening the listener; the privilege grid itself
      is unbroken (asserted as in case 16), proving the rejection is
      the membership-option rule, not privilege equality.
  21. **A migrated table is owned by an unrelated, unreachable role**
      (negative fixture on real PostgreSQL; the harness bootstrap
      identity, a superuser, performs the transfer): a fresh role
      `U` with no memberships, no privileges, and no other ownership
      — unreachable from `vector_api` (no membership edge from the
      runtime role or its closure, so the runtime closure check
      cannot see it) — receives post-convergence
      `ALTER TABLE vector_data.vector_records OWNER TO U` → the
      canonical-ownership gate (Scope 4) rejects it on
      `pg_class.relowner` before bootstrap, history read, or pool
      open: `migrate` exits non-zero with a structured error naming
      the table and its owner, and `serve` never opens the runtime
      pool or the listener. The rejection is the gate's — the
      runtime pool assertion never runs in this flow — which proves
      the migration-identity ownership rule is independent of the
      runtime no-ownership rule.
  22. **Each migrated function is owned by an unrelated, unreachable
      role** (one negative fixture per migrated function, on real
      PostgreSQL; the harness bootstrap identity performs the
      transfer): a fresh unreachable role `U` (as in case 21)
      receives, post-convergence, `ALTER FUNCTION
      vector_data.validate_vector_record() OWNER TO U`, and in a
      second fixture `ALTER FUNCTION vector_control.set_updated_at()
      OWNER TO U` → in each, the canonical-ownership gate (Scope 4)
      rejects it on `pg_proc.proowner` before bootstrap, history
      read, or pool open — `migrate` exits non-zero naming the
      function and its owner, and `serve` never listens. As in case
      21, the rejection is the gate's: the unreachable owner is
      invisible to the runtime closure check.
  Positive path: the canonical harness provisioning passes the
  migration-identity assertions, the gate, and the runtime pool
  assertions, and `serve` listens. On every converged canonical
  database the canonical owner assertions hold: the migration
  identity owns the `vector` database, both migrated schemas, and
  every in-scope `pg_class` row (`relowner`), `pg_type` row
  (`typowner`), and `pg_proc` row (`proowner`) in them — the
  migration-history table, every migrated table, sequence, index,
  type, and both migrated functions — while `vector_api` owns none
  of them; the gate and the runtime assertion both pass by
  construction.
- **Codec (integration):** bind a 1024-element vector as a parameter,
  store it, read it back in text form, and compare element-wise.
- **Startup (integration):** `migrate` against a fresh provisioned
  database exits 0; `serve` against an un-migrated but provisioned
  database converges and reports convergence (flag observable); `serve`
  against a database with a drifted checksum exits non-zero and never
  listens.

## Out of scope

- Serving behavior beyond the hand-off (package 4): middleware, routes,
  `/readyz` handler, shutdown.
- Authentication and transaction helpers (package 2).
- Vector operations (package 3).
- Test harness and matrices (package 5).
- Container image (package 5c).
- Alternate migration-provisioning shapes (deferred; canonical
  owner-of-database only).
- Down-migrations, schema repair, or history rewriting.

## Open issues

None. The pgvector extension release is not pinned by any authoritative
source (PostgreSQL is pinned to 18); the 5a harness and CI pin a
concrete extension release supporting the `hnsw` index type used by
migration `0003` and record the choice in the test notes — an
infrastructure decision, not a runner requirement.
