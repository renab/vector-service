# Shared Vector Database Migrations

This directory owns the database schema for Chimera's shared vector-storage
service.

The `vector` PostgreSQL database is shared infrastructure for multiple
applications, including Bookshelf and the future Vault service.

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
