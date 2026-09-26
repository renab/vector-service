# Architecture

## Purpose

Vector Service is a shared vector-storage substrate for multiple independent applications.

It provides:

- application isolation
- namespace isolation
- vector persistence
- nearest-neighbor search
- generic metadata filtering
- vector-space compatibility enforcement
- record lifecycle management

It deliberately does not provide application-specific retrieval semantics.

## System Boundary

Conceptually:

```text
                   embedding service
                          ▲
                          │
        ┌─────────────────┴─────────────────┐
        │                                   │
  consumer A                          consumer B
        │                                   │
        │ vector operations                 │
        └─────────────────┬─────────────────┘
                          ▼
                   Vector Service
                          │
                          ▼
                PostgreSQL + pgvector
```

Consumers may separately call embedding or reranking infrastructure.

Vector Service is not responsible for orchestrating those systems.

## Retrieval Ownership

A consumer owns:

- source representation
- chunking or projection generation
- embedding input construction
- query embedding input construction
- candidate-count strategy
- reranking
- application-specific filtering
- context/result assembly

Vector Service owns:

- vector compatibility
- generic storage
- generic nearest-neighbor search
- generic metadata filtering
- application isolation
- namespace isolation

## Shared Database

All applications use the same Vector Service and shared vector database.

The logical model is:

```text
vector database
│
├── vector_control
│   ├── applications
│   ├── namespaces
│   ├── vector_spaces
│   └── application_credentials
│
└── vector_data
    └── vector_records
```

Application-specific schemas are intentionally avoided.

Consumers must not create domain-specific tables inside the shared vector database.

## Application Identity

Applications are registered resources with stable UUID identifiers.

Examples:

```text
memory-service
notes-service
search-service
```

The caller's credential determines its application identity.

Normal data-plane API requests do not accept an application selector.

## Namespace Identity

Namespaces are registered under exactly one application.

Example:

```text
application: notes-service

namespaces:
  project-alpha
  project-beta
  archive
```

Namespace keys only need to be unique within an application.

There is deliberately no global namespace.

## Request Isolation

The normal data flow is:

```text
request
  ↓
authenticate credential
  ↓
resolve application
  ↓
resolve namespace within application
  ↓
BEGIN
  ↓
SET LOCAL vector.application_id = <uuid>
  ↓
execute SQL
  ↓
COMMIT
```

Application identity is therefore enforced twice:

1. service-level authorization
2. PostgreSQL Row-Level Security

## PostgreSQL Identity

Vector Service connects to PostgreSQL using one dedicated runtime database role.

That role:

- may log in
- is not a superuser
- does not own the database
- does not own application tables
- cannot bypass RLS
- receives only required privileges

Transport authentication should use infrastructure-managed TLS credentials.

Consumers never connect directly to PostgreSQL.

## Vector Representation

The base record table stores an unconstrained:

```text
vector
```

rather than globally using:

```text
vector(1024)
```

This allows multiple vector spaces and dimensions to coexist.

Compatibility is enforced by registered vector spaces and database validation.

ANN indexes are created for specific vector spaces.

Example:

```text
vector space:
  model: example-embedding-model
  dimensions: 1024
  distance: cosine
```

may have an HNSW expression index using:

```text
embedding::vector(1024)
```

and `vector_cosine_ops`.

## Physical Partitioning

Application-level PostgreSQL partitioning is not part of the initial architecture.

Reasons:

- expected consumer count is small
- expected scale is moderate
- dynamic partition management adds operational complexity
- the API contract does not require physical partitions
- pgvector supports filtered ANN retrieval
- physical optimization should be driven by measurements

If later measurements show significant ANN recall or latency degradation from application filtering, physical partitioning may be added underneath the existing API.

That must not alter logical semantics.

## Derived State

Vector records are rebuildable projections.

Vector Service must never become the authoritative store for source documents or application semantics.

Examples:

```text
source documents
   ↓
consumer projection
   ↓
embedding
   ↓
vector record
```

or:

```text
canonical application data
   ↓
retrieval projection
   ↓
embedding
   ↓
vector record
```

Deleting the vector index must not destroy canonical consumer state.

## Failure Philosophy

The service should prefer correctness over availability when isolation cannot be established.

Examples:

- missing authentication → reject
- missing namespace → reject
- invalid vector-space key → reject
- incompatible dimensions → reject
- database unavailable → return unavailable
- missing application transaction context → RLS denies data

There should be no fallback to an unscoped query.

## Scalability

The initial design assumes:

- one shared service
- one PostgreSQL database
- modest number of applications
- modest number of namespaces
- HNSW indexing per vector space

Potential future scaling paths include:

- connection pooling
- additional service replicas
- physical table partitioning
- application-specific partial indexes
- dedicated PostgreSQL resources
- separate vector database cluster

These are implementation changes, not API changes.

## Non-Goals

Vector Service does not provide:

- natural-language search orchestration
- embedding generation
- reranking
- LLM access
- document parsing
- source storage
- generic full-text search
- application-specific semantics
- provenance engines
- temporal reasoning engines
- graph storage
- arbitrary cross-application federation
