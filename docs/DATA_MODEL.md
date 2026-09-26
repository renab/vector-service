# Data Model

## Purpose

This document defines the logical data model owned by Vector Service.

Vector Service is shared infrastructure. Its schema must remain application neutral.

The database stores only generic vector-storage concepts:

- applications
- namespaces
- vector spaces
- application credentials
- vector records

It must not encode consumer-specific semantics.

---

# Schemas

The vector database uses two PostgreSQL schemas:

```text
vector_control
vector_data
```

`vector_control` contains service control-plane state.

`vector_data` contains derived vector records.

---

# Applications

An application represents one logical consumer of Vector Service.

Examples:

```text
memory-service
notes-service
search-service
```

Applications are explicit registered resources.

Each application has:

- stable UUID identity
- immutable application key
- display name
- enabled state
- created timestamp
- updated timestamp

Conceptually:

```text
vector_control.applications
```

Fields:

```text
id
application_key
display_name
enabled
created_at
updated_at
```

## Application Key

`application_key` is a human-readable stable identifier.

Example:

```text
notes-service
```

It must not be used as the primary relational identity.

Internal relationships use the UUID.

Application keys should be treated as immutable after creation.

## Application Enablement

Disabling an application prevents normal service access.

Disabling an application must not automatically delete:

- namespaces
- credentials
- vector records

Disablement is a control-plane state change, not destructive cleanup.

---

# Namespaces

A namespace is a logical isolation boundary owned by exactly one application.

Examples:

```text
application: notes-service

namespaces:
  project-alpha
  project-beta
  archive
```

Conceptually:

```text
vector_control.namespaces
```

Fields:

```text
id
application_id
namespace_key
display_name
enabled
metadata
created_at
updated_at
```

## Namespace Identity

Namespace keys are unique within an application.

The same namespace key may exist under different applications.

Example:

```text
notes-service / archive
memory-service / archive
```

These are distinct namespaces.

## No Default Namespace

There is deliberately no:

```text
default
```

namespace implied by omission.

Every data-plane operation must select an explicit namespace.

## No Wildcard Namespace

The service must not interpret any namespace value as:

```text
*
all
any
```

Cross-namespace operations require an explicitly designed API.

## Namespace Metadata

Namespace metadata is optional caller-supplied JSON.

It is not an authorization mechanism.

Application ownership is represented relationally.

---

# Vector Spaces

A vector space is an immutable compatibility contract for stored embeddings.

Conceptually:

```text
vector_control.vector_spaces
```

Fields:

```text
id
vector_space_key
embedding_model
embedding_model_version
dimensions
distance_metric
enabled
created_at
retired_at
```

## Purpose

A vector space answers:

> Which vectors can validly be compared to each other?

Two embeddings belong to the same vector space only when all compatibility-relevant assumptions match.

These include at minimum:

- embedding model
- model version or artifact identity
- output dimensions
- distance metric
- representation-generation compatibility

## Example

```text
qwen3-embedding-0.6b-1024-cosine-v1
```

This key is a service-level compatibility identifier.

It is not merely a model name.

## Immutability

The semantic meaning of an existing vector-space key must not change.

If compatibility changes, create a new vector space.

Examples:

```text
example-model-1024-cosine-v1
example-model-1024-cosine-v2
example-model-768-cosine-v1
```

## Distance Metrics

Initial supported distance metrics:

```text
cosine
l2
inner_product
```

The vector space determines which pgvector operator/index family applies.

## Retirement

A vector space may be retired.

Retirement means:

- no new normal writes should target it
- existing records may remain during migration
- reads may remain available where explicitly supported

Retirement must not silently rewrite existing records into another vector space.

---

# Application Credentials

Application credentials authenticate normal data-plane callers.

Conceptually:

```text
vector_control.application_credentials
```

Fields:

```text
id
application_id
credential_name
credential_hash
enabled
created_at
expires_at
last_used_at
```

## Raw Credential Storage

Raw application credentials must never be stored.

Only their digest is persisted.

Credentials are assumed to be high-entropy machine-generated values.

## Multiple Credentials

An application may have multiple active credentials.

This supports:

- rotation
- migration jobs
- temporary reindex workers
- staged deployment changes

Example:

```text
notes-production
notes-reindex
```

These credentials still resolve to the same application identity.

## Disablement

A credential may be disabled without deleting the parent application.

Disabling a credential must immediately prevent future authentication.

---

# Vector Records

Vector records are generic retrieval projections.

Conceptually:

```text
vector_data.vector_records
```

Fields:

```text
id
application_id
namespace_id
vector_space_id

object_id
projection_id

content_hash
source_updated_at
metadata
embedding

created_at
updated_at
```

---

# Record Ownership

Each record belongs to exactly:

```text
one application
one namespace
one vector space
```

A namespace must belong to the same application as the record.

This relationship is enforced relationally.

Caller metadata must not determine application or namespace ownership.

---

# Object ID

`object_id` is an opaque caller-controlled identifier for the authoritative source object.

Examples:

```text
document-123
record-456
entity-789
```

Vector Service does not interpret its structure.

The caller owns its semantics and lifecycle.

---

# Projection ID

`projection_id` identifies one retrieval projection of an object.

Examples:

```text
chunk-0001
summary
title
segment-4
```

Vector Service does not interpret the value.

Different applications may use completely different projection models.

---

# Logical Record Identity

A logical vector projection is unique by:

```text
application
namespace
object_id
projection_id
vector_space
```

This allows:

- idempotent upserts
- deterministic consumer behavior
- simultaneous old/new embedding spaces during migrations

Example:

```text
object_id: document-123
projection_id: chunk-4
```

may exist simultaneously in:

```text
model-a-1024-cosine-v1
model-b-768-cosine-v1
```

during a migration.

---

# Internal Record UUID

Each stored record also has an internal UUID:

```text
id
```

This UUID is service-owned.

Consumers should not attempt to derive application semantics from it.

---

# Content Hash

`content_hash` stores the SHA-256 digest of the normalized source representation used to generate the embedding.

The database stores the raw 32-byte digest.

API representations may use lowercase hexadecimal.

## Ownership of Normalization

The consumer owns normalization.

Vector Service does not:

- trim content
- rewrite content
- tokenize content
- canonicalize Markdown
- perform semantic normalization

It receives the resulting hash.

## Purpose

Consumers may use content hashes to determine that a projection does not need to be re-embedded.

Matching:

```text
object
projection
vector space
content hash
```

may be treated as unchanged.

---

# Source Updated Timestamp

`source_updated_at` is optional caller-provided information describing when the authoritative source changed.

It is informational.

It must not replace:

```text
created_at
updated_at
```

which describe database record lifecycle.

---

# Metadata

`metadata` is caller-supplied JSON.

It must be a JSON object.

Example:

```json
{
  "document_type": "note",
  "state": "active",
  "language": "en"
}
```

## Metadata Rules

Metadata is generic caller data.

It must not be used as the primary source of truth for:

- application identity
- namespace identity
- authorization
- vector-space identity

Those properties have dedicated relational fields.

## Filtering

The API may expose generic structured metadata filters.

The service must not expose raw SQL or arbitrary caller-controlled query fragments.

See:

```text
docs/API.md
```

---

# Embedding

The embedding is stored using pgvector's generic:

```text
vector
```

type.

The base table intentionally does not globally enforce:

```text
vector(1024)
```

because multiple vector spaces with different dimensions may coexist.

Dimension validation is performed against the selected vector-space definition.

---

# ANN Indexes

Approximate-nearest-neighbor indexes belong to individual compatible vector spaces.

An example 1024-dimensional cosine vector space may use:

```text
HNSW
embedding::vector(1024)
vector_cosine_ops
```

with a partial predicate selecting that vector-space UUID.

This allows different dimensions and metrics to coexist in one logical record table.

---

# Physical Partitioning

The initial schema does not physically partition vector records by application.

This is deliberate.

Physical partitioning is an implementation optimization, not part of the logical data model.

It may be introduced later based on:

- measured latency
- recall behavior
- index size
- tenant imbalance
- maintenance cost

Any future partitioning must preserve the same logical API and isolation semantics.

---

# Deletion Semantics

Deleting a vector record deletes only derived vector state.

It never deletes the authoritative source object.

## Delete Projection

Deleting one logical projection identifies:

```text
namespace
object_id
projection_id
vector_space
```

## Delete Object

Deleting an object removes all matching projections within the explicitly selected namespace.

An optional vector-space selector may narrow deletion.

Object deletion must never cross namespace boundaries.

---

# Derived-State Contract

Vector records are rebuildable.

The authoritative source remains with the consumer.

Therefore:

```text
vector DB loss
    ≠
source-data loss
```

Consumers must be capable of reconstructing their vector projections from authoritative state.

Backups improve recovery time but do not change the authority model.