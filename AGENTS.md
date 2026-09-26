# AGENTS.md

Instructions for automated coding agents and contributors working in this repository.

These rules describe established architecture. Do not silently replace them with a more convenient design.

## Required Reading

Before modifying behavior, read:

1. `README.md`
2. `docs/ARCHITECTURE.md`
3. `docs/DATA_MODEL.md`
4. `docs/API.md`
5. `docs/SECURITY.md`
6. `docs/MIGRATIONS.md`
7. all existing migrations related to the change
8. existing tests for the affected behavior

## Core Architectural Rules

Vector Service is shared infrastructure.

It must remain application neutral.

Do not introduce domain-specific concepts from any consumer.

Examples of inappropriate shared-layer concepts include:

- memory assertions
- claim streams
- note types
- Markdown headings
- project-specific entities
- provenance models
- temporal reasoning models
- application-specific ranking rules

Such semantics belong to consumers.

## Application Isolation

Every normal caller belongs to exactly one registered application.

The application identity is determined by authentication.

Normal data-plane requests must not accept an arbitrary caller-supplied application ID.

Do not introduce:

```json
{
  "application": "some-app"
}
```

into normal data-plane operations.

Application identity must come from the authenticated credential.

## Namespace Isolation

Every data-plane operation must be namespace scoped.

There is:

- no default namespace
- no wildcard namespace
- no automatic cross-namespace query
- no implicit "all namespaces"

Do not add convenience behavior that changes this.

Cross-namespace retrieval, if ever required, must be designed as a separate explicit capability.

## Database Isolation

PostgreSQL Row-Level Security is defense in depth and must remain enabled.

For every application-scoped transaction, Vector Service must establish transaction-local application context:

```sql
SELECT set_config(
    'vector.application_id',
    '<application UUID>',
    true
);
```

The third argument must remain `true`.

Do not use session-persistent application context with pooled database connections.

Do not disable RLS for normal service operation.

Do not give the runtime database role `BYPASSRLS`.

Do not run the service as the database owner or superuser.

## Embeddings

Vector Service does not generate embeddings.

The API accepts vectors produced elsewhere.

Do not add:

- embedding-model clients
- embedding prompts
- natural-language query embedding
- model routing
- reranking

without an explicit architecture change.

## Vector Spaces

Vector spaces are immutable compatibility contracts.

Do not modify the semantic meaning of an existing vector-space key.

If any compatibility-relevant property changes, create a new vector space.

Examples include:

- embedding model
- model artifact
- dimensionality
- distance metric
- normalization behavior
- implementation changes that make old and new vectors incompatible

## Database Migrations

Released migrations are immutable.

Never rewrite an already-released migration to change behavior.

Create a new migration.

Migrations live in this repository because they are part of the service's compatibility contract.

See `docs/MIGRATIONS.md`.

## SQL

Prefer explicit SQL.

Do not introduce an ORM unless there is a demonstrated requirement that explicit SQL cannot reasonably satisfy.

All caller-controlled values must be passed as query parameters.

Never interpolate caller-provided SQL fragments.

Metadata filtering must use the structured filter language defined in `docs/API.md`.

## Authentication

Application credentials are opaque, high-entropy machine-generated secrets.

Never log raw credentials.

Never persist raw credentials.

Persist only cryptographic digests as defined by the implementation.

Administrative authentication must remain distinct from application data-plane authentication.

## Logging

Use structured logs.

Appropriate operational fields include:

- request ID
- application UUID
- namespace UUID
- operation
- duration
- result count
- response status

Do not log by default:

- bearer credentials
- PostgreSQL private keys
- TLS private keys
- complete embedding vectors
- arbitrary caller metadata
- source document contents

## API Compatibility

`docs/API.md` defines the service contract.

Do not casually change:

- endpoint paths
- authentication semantics
- isolation behavior
- error semantics
- record identity
- namespace behavior
- vector-space behavior

Breaking changes require explicit versioning or an architecture decision.

## Generic Examples

Use generic names in documentation and tests.

Preferred examples:

```text
application:
  memory-service
  notes-service
  search-service

namespace:
  project-alpha
  project-beta
  personal-notes
  research
  archive
```

Do not use:

- personal names
- household names
- private repository names
- private vault names
- real user content
- production secrets

Synthetic fixtures should be obviously synthetic.

## Scope Discipline

If a requested change conflicts with established architecture:

1. do not silently implement around it
2. identify the conflict
3. explain the affected boundary
4. propose the smallest compatible alternative

Do not "simplify" security or isolation constraints simply to make tests pass.

## Implementation Style

Prefer:

- Go standard library where practical
- `pgx` for PostgreSQL
- small focused packages
- explicit interfaces only where useful
- table-driven tests
- deterministic behavior
- context propagation
- bounded request sizes
- bounded batch sizes
- explicit timeouts
- structured errors

Avoid:

- framework-heavy designs
- unnecessary abstraction layers
- speculative distributed systems machinery
- generic plugin architectures without a current requirement
- premature table partitioning
- hidden global state

## Before Completing a Change

Verify:

- application isolation still holds
- namespace isolation still holds
- RLS still applies
- no raw credentials are logged
- migrations remain forward-only
- API behavior matches documentation
- tests cover failure paths
- vector dimension validation still occurs
- no application-specific semantics leaked into shared storage
