# Shared Vector Database Migrations

This directory owns the database schema for Chimera's shared vector-storage
service.

The `vector` PostgreSQL database is shared infrastructure for multiple
applications, including Bookshelf and the future Vault service.

## Migration inventory

Migrations are applied in numeric order by the embedded canonical runner
(implementation package 1). The expected released set is exactly the six
files below. The checksum is the SHA-256 of the file bytes; it is recorded
in `vector_control.schema_migrations` when a migration is applied and
re-validated on every later run — an applied migration whose file has
changed is a fatal drift error at startup.

| Version | File | Purpose | SHA-256 |
|---------|------|---------|---------|
| 1 | `0001_shared_vector_schema.sql` | Schemas, control tables, `vector_records`, dimension-compatibility trigger, forced RLS | `fd7c7dc08ac93a759621a4504023506c4f11e22009a33a6a76b7fa6544967fbc` |
| 2 | `0002_qwen3_embedding_space.sql` | Seed the initial vector space row | `de65b429ca983e24990f95499ae96dced9c34cf2e02786b860aa065350c70060` |
| 3 | `0003_qwen3_hnsq.sql` | HNSW index for the seeded space | `730b80c77ab232ca31dacad704df579241d74c54a79a870e02f29dc48f640b51` |
| 4 | `0004_runtime_identity.sql` | `application_credentials` table; direct `vector_api` grants | `bc13c6618de1a30fb80bb81db2426052e4e39e2316a596c606eaf5aa9508a21f` |
| 5 | `0005_runtime_privilege_boundary.sql` | Canonical `PUBLIC` privilege baseline; direct function/type grants for `vector_api` | `e7097c444a650855773ccad87f1677d4d37e9f6935014a9745d2eb0fb4b20490` |
| 6 | `0006_runtime_privilege_boundary_completion.sql` | Completes 0005: revokes `PUBLIC` `USAGE` on every type in the migrated schemas (enum + table row/composite types) | `29bd9e54d5da131cf9eeefab674cef2c8c829b76bbe74264f7f9dff4c5b7dfb9` |

Released migrations are immutable and forward-only (`docs/MIGRATIONS.md`):
once a file is merged, released, or applied, it is never edited — a fix or
correction enters only as a new numbered file. The checksum rule above is
the enforcement mechanism.

## Runtime privilege baseline (0004 + 0005 + 0006)

The canonical `vector_api` privilege boundary is established jointly by
migrations `0004`, `0005`, and `0006`. **`0004` alone does not establish
it:** on a database with PostgreSQL's default `PUBLIC` privileges, the
runtime role's effective privileges include `PUBLIC EXECUTE` on the
migrated functions, `PUBLIC USAGE` on the migrated enum type, `PUBLIC
USAGE` on every migrated table's implicit row (composite) type, and
`PUBLIC TEMPORARY` on the database, and the service's startup assertion
(package 1) rejects that state.

- `0004` grants `vector_api` the direct runtime privileges: `CONNECT` on
  the database; `USAGE` (without `CREATE`) on the two schemas; the
  table-level DML grants; and no grant on
  `vector_control.schema_migrations`.
- `0005` establishes the `PUBLIC` baseline: it revokes the PostgreSQL 18
  defaults within the migrated scope that the canonical boundary
  excludes — `TEMPORARY` on the database, `EXECUTE` on the two migrated
  functions, and `USAGE` on the migrated enum type. Schemas and tables
  grant nothing to `PUBLIC` by default.
- `0005` grants the service's own use of the migrated objects directly
  to `vector_api`: `EXECUTE` on `vector_data.validate_vector_record()`
  and `vector_control.set_updated_at()`, and `USAGE` on
  `vector_control.distance_metric`. The canonical boundary
  (implementation overview, invariant 4) requires the runtime role's
  function and type privileges to be direct grants, not `PUBLIC`-derived.
- `0005` preserves the one remaining default, `CONNECT` on the database:
  it does not extend the runtime role's effective boundary, because
  `vector_api` already holds `CONNECT` directly via `0004`.
- `0006` completes the type half of the `PUBLIC` baseline. PostgreSQL 18
  also grants `USAGE` to `PUBLIC` by default on every type — including
  the implicit row (composite) type created for every table — and `0005`
  revoked the enum type only. `0006` revokes `PUBLIC USAGE` on every
  type in `vector_control` and `vector_data`: the enum type (already
  revoked by `0005`, a no-op re-statement) and the row type of every
  migrated table, including the runner-bootstrapped
  `schema_migrations`. With `0005` + `0006`, no `PUBLIC` default inside
  the migrated scope remains except the preserved `CONNECT`.

After full convergence (`0001`–`0006`), `vector_api`'s effective
privileges equal exactly the canonical runtime grant set defined by the
implementation overview (invariant 4), which the service's startup
assertion (package 1) verifies through the PostgreSQL catalogs, and
`PUBLIC` holds only the preserved `CONNECT` default within the migrated
scope.

## Ownership boundary

The shared vector layer owns only generic vector-storage concepts:

- applications
- namespaces
- vector spaces
- object identifiers
- projection identifiers
- content hashes
- caller-supplied metadata
- embeddings
- storage/search lifecycle

It does not own application retrieval semantics.

Bookshelf remains responsible for its memory model, temporal/provenance
semantics, retrieval strategy, query representation, candidate selection,
reranking, and context assembly.

Vault remains responsible for source discovery, document/chunk semantics,
query representation, candidate selection, reranking, and result assembly.

The shared vector database must not contain Bookshelf-, Vault-, Obsidian-,
or Galaxy-specific semantic columns.

## Isolation

Application and namespace are explicit resources.

There is no default application.

There is no default namespace.

There is no wildcard namespace.

Every application-scoped database transaction must establish application
context using:

    SET LOCAL vector.application_id = '<application UUID>';

Row-Level Security provides defense-in-depth against accidentally unscoped
queries.

The vector service is the only intended runtime client of this database.
Bookshelf, Vault, Galaxy, and other consumers should call vector-service
rather than connect directly to PostgreSQL.

## Vector spaces

Vector spaces are immutable compatibility contracts.

A vector space identifies:

- embedding model
- embedding model version
- dimensions
- distance metric
- service-level schema/version identity

Changing any compatibility-relevant behavior requires creation of a new
vector space rather than mutation of an existing space.

Multiple vector spaces may coexist during migrations and re-indexing.

## Derived-state model

Vector records are derived state.

Authoritative application data remains with its owning application.

Examples:

- Vault source Markdown/Git repositories remain authoritative.
- Bookshelf canonical memory state remains authoritative.

Loss of the vector database must be recoverable by re-projecting and
re-embedding authoritative application state.

Backups are useful for recovery speed but are not the source of truth.

## Record identity

Each logical projection is unique by:

    application
    namespace
    object_id
    projection_id
    vector_space

This permits idempotent upserts and simultaneous representation of the same
source projection in multiple vector spaces during embedding migrations.

## Content hashes

`content_hash` contains a raw 32-byte SHA-256 digest of the normalized source
representation used to create the embedding.

Consumers may use this to avoid unnecessary re-embedding when both the source
representation and vector space are unchanged.

The consumer owns normalization semantics.

## Metadata

`metadata` must be a JSON object.

Metadata is caller supplied and opaque to the shared vector layer.

Metadata must never be used as the primary application or namespace security
boundary.

Supported metadata filtering will be defined by the vector-service API rather
than exposing arbitrary PostgreSQL expressions to callers.

## ANN indexing

The base table stores `embedding` as unconstrained `vector` so multiple vector
dimensions can coexist.

Each ANN index is scoped to one known vector space and casts the embedding to
the vector space's declared dimensionality.

Physical partitioning by application or namespace is intentionally deferred
until workload measurements show it is useful.

That optimization must not change the public vector-service API.
