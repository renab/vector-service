# Vector Service

Shared, application-neutral vector storage and nearest-neighbor search service backed by PostgreSQL and pgvector.

Vector Service is designed for multiple independent consumers that need shared embedding storage without sharing retrieval semantics.

Typical consumers may include:

- memory systems
- document or knowledge-base services
- note systems
- search services
- other internal applications that need vector retrieval

Vector Service provides storage and search primitives. It does not perform embedding inference, reranking, document parsing, chunking, provenance reasoning, or application-specific retrieval orchestration.

## Core Principles

The service is built around a few hard constraints:

- the database is shared infrastructure
- the service is shared infrastructure
- applications are explicitly registered
- namespaces are explicitly registered
- every data-plane request is application isolated
- every data-plane request is namespace scoped
- no default namespace exists
- no wildcard namespace exists
- vector spaces are immutable compatibility contracts
- authoritative source data remains outside Vector Service
- vector indexes are derived and rebuildable state
- consumers own their own retrieval semantics

## Technology

The initial implementation uses:

- Go
- PostgreSQL 18
- pgvector
- `pgx`
- HTTP/JSON
- PostgreSQL Row-Level Security
- TLS client authentication between Vector Service and PostgreSQL

Embedding and reranking services are external dependencies of consuming applications, not Vector Service.

## Repository Layout

```text
vector-service/
├── cmd/
│   └── vector-service/
│       └── main.go
├── internal/
│   ├── api/
│   ├── auth/
│   ├── config/
│   ├── database/
│   ├── model/
│   └── vector/
├── migrations/
├── docs/
├── AGENTS.md
├── Dockerfile
├── go.mod
├── go.sum
└── README.md
```

The package layout is illustrative, not mandatory. Do not create empty or artificial packages merely to match this tree.

## Documentation

Read these before making architectural changes:

- [`AGENTS.md`](AGENTS.md) — rules for coding agents and contributors
- [`docs/ARCHITECTURE.md`](docs/ARCHITECTURE.md) — service boundaries and system design
- [`docs/DATA_MODEL.md`](docs/DATA_MODEL.md) — applications, namespaces, vector spaces, and records
- [`docs/API.md`](docs/API.md) — HTTP API contract
- [`docs/SECURITY.md`](docs/SECURITY.md) — authentication, RLS, isolation, and secret handling
- [`docs/MIGRATIONS.md`](docs/MIGRATIONS.md) — migration ownership and database evolution

## Ownership Boundary

This repository owns:

- Vector Service source code
- HTTP API behavior
- database schema migrations
- database access logic
- authentication behavior
- vector-storage semantics
- tests
- container build
- image publishing (GHCR)
- service documentation

Deployment infrastructure belongs elsewhere.

The deployment repository should own:

- Kubernetes deployment
- PostgreSQL provisioning
- pgvector installation
- database creation
- database roles
- certificates
- NetworkPolicy
- runtime configuration
- image pinning

Application schema migrations must remain in this repository.

## What Vector Service Does

Vector Service provides generic operations for:

- application registration
- namespace registration
- application credential management
- vector-space discovery
- vector record upsert
- vector record deletion
- vector record lookup
- namespace-scoped nearest-neighbor search
- structured metadata filtering

## What Vector Service Does Not Do

Vector Service must not perform:

- text embedding
- embedding prompt construction
- query rewriting
- reranking
- document parsing
- Markdown parsing
- chunk generation
- LLM calls
- application-specific provenance handling
- application-specific temporal reasoning
- application-specific context assembly

Consumers remain responsible for those behaviors.

## Data Authority

Vector Service stores derived retrieval state.

Examples:

```text
authoritative documents
        ↓
consumer-specific processing
        ↓
embedding
        ↓
Vector Service
```

or:

```text
authoritative application state
        ↓
retrieval projection
        ↓
embedding
        ↓
Vector Service
```

Loss of the vector database must be recoverable from authoritative consumer-owned state.

## Image Publishing

The service container image is published to GitHub Container Registry (GHCR)
for master pushes and can also be triggered manually via workflow_dispatch:

```
ghcr.io/renab/vector-service:sha-<commit>
ghcr.io/renab/vector-service:stable
```

- `sha-<commit>` — commit-addressed tag bound to a specific source commit.
  The workflow verifies the pushed artifact matches the locally verified
  image by comparing config digests (rootfs identity) before promotion.
  Protected against accidental workflow overwrite; not protected against
  external writers (repository owners with packages:write scope may mutate
  any tag).
- `stable` — mutable tag promoted to the latest eligible successful master
  publication.

Images are published only after passing the full CI pipeline: unit tests,
integration tests, and container image verification. The exact verified
image is published — not a second rebuild.

**Concurrency**: Master publishes are serialized. A newer push may supersede
a pending run. The last eligible run that actually executes publishes its
SHA tag and promotes stable. Runs that are superseded never publish. Every
run that does execute publish creates its SHA tag.

Pull requests and non-master branches do not publish images.

Deployment repositories should pin by digest for production deployments
and use `stable` for canary or development environments.

For detailed publishing semantics and known limitations, see
[`docs/PUBLISHING.md`](docs/PUBLISHING.md).

## Initial Vector Space

The initial production vector space is expected to use:

```text
qwen3-embedding-0.6b-1024-cosine-v1
```

The exact model identifier is not special to the service.

Vector Service treats it as an immutable registered compatibility contract containing:

- model
- model version
- dimensions
- distance metric
- vector-space version

See [`docs/DATA_MODEL.md`](docs/DATA_MODEL.md).

## Development

Start by reading:

1. `AGENTS.md`
2. `docs/ARCHITECTURE.md`
3. `docs/API.md`
4. `docs/DATA_MODEL.md`
5. all existing migrations
6. existing tests

Prefer explicit SQL and small dependencies.

Do not weaken isolation or architectural boundaries in the name of convenience.
