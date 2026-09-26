# Development Guide

## Purpose

This document describes local development and implementation conventions for Vector Service.

Architecture and product behavior are defined elsewhere:

- `../README.md`
- `../AGENTS.md`
- `ARCHITECTURE.md`
- `DATA_MODEL.md`
- `API.md`
- `SECURITY.md`
- `MIGRATIONS.md`

Do not treat this document as authority for changing those contracts.

---

# Language

Vector Service is implemented in Go.

Recommended baseline:

```text
current supported stable Go release
```

Pin the selected Go version in CI and container builds.

---

# Dependencies

Prefer the standard library.

Expected core PostgreSQL dependency:

```text
github.com/jackc/pgx/v5
```

A small HTTP router may be used if it reduces boilerplate.

Avoid heavyweight frameworks and ORMs without a demonstrated need.

---

# Package Structure

Suggested structure:

```text
cmd/
  vector-service/

internal/
  api/
  auth/
  config/
  database/
  model/
  vector/
```

This is guidance rather than a requirement.

Do not create packages that contain only one trivial wrapper merely to mirror this tree.

Prefer cohesive packages based on real responsibilities.

---

# Configuration

Runtime configuration should be supplied externally.

Expected configuration includes:

```text
HTTP listen address

PostgreSQL host
PostgreSQL port
PostgreSQL database
PostgreSQL user

PostgreSQL CA certificate path
PostgreSQL client certificate path
PostgreSQL client key path

administrative authentication configuration

HTTP request-size limits
batch-size limits
search-result limits

logging settings
```

Configuration should fail fast when required values are missing or invalid.

Do not silently substitute insecure defaults.

---

# PostgreSQL Connections

Use `pgxpool` for runtime connection pooling.

Every application-scoped operation must:

1. begin a transaction
2. set transaction-local application context
3. perform all request queries
4. commit or roll back

Do not set application context on a pooled connection outside a transaction.

---

# Context and Cancellation

Pass `context.Context` through:

- HTTP handlers
- authentication
- PostgreSQL operations
- migration operations where appropriate

Database operations must honor request cancellation and timeouts.

---

# HTTP

Use normal HTTP semantics.

Requirements:

- JSON request/response bodies
- explicit content type
- bounded request bodies
- structured validation errors
- request IDs
- sane server timeouts

Configure at minimum:

- read-header timeout
- read timeout where appropriate
- write timeout where appropriate
- idle timeout

Do not use an unconfigured default HTTP server in production.

---

# JSON

Use explicit request and response structs.

Reject malformed JSON.

Consider rejecting unknown fields for control-plane/admin APIs where strictness improves safety.

Do not deserialize arbitrary request content into generic maps unless the field is intentionally generic, such as caller metadata.

---

# Authentication

Application credentials arrive through:

```text
Authorization: Bearer ...
```

Authentication should:

1. validate header structure
2. hash credential
3. look up enabled credential
4. verify application is enabled
5. update or schedule update of usage metadata as appropriate
6. attach resolved application identity to request context

Do not propagate the raw credential beyond authentication code.

---

# Administrative API

Administrative authentication should be implemented separately from application authentication.

Keep admin handlers separated enough that normal application credentials cannot accidentally gain administrative behavior.

---

# Validation

Validate requests before opening expensive database operations where practical.

Examples:

- required fields
- batch size
- vector length
- finite numeric vector values
- supported filter operators
- content-hash encoding
- search-result limits

Database constraints remain the final source of enforcement.

---

# Error Model

Internal errors should map to stable API responses.

Do not expose raw PostgreSQL error strings directly to callers.

Logs may contain appropriate structured database error details while responses remain controlled.

---

# Testing

Prefer table-driven Go tests.

Test layers should include:

## Unit tests

Examples:

- credential parsing
- credential hashing
- request validation
- filter validation
- response mapping
- configuration parsing

## Database integration tests

Use real PostgreSQL with pgvector.

Test:

- RLS
- application isolation
- namespace isolation
- upsert
- delete
- vector dimension rejection
- search
- metadata filters
- migrations
- credential lookup

Do not mock PostgreSQL for behaviors that specifically depend on PostgreSQL semantics.

## HTTP integration tests

Exercise:

```text
HTTP
→ auth
→ service
→ PostgreSQL
```

for critical isolation paths.

---

# Required Isolation Tests

At minimum:

```text
application A cannot read application B
application A cannot search application B
application A cannot delete application B
application A cannot discover application B namespaces

namespace X cannot accidentally expose namespace Y
missing namespace does not become all namespaces
missing RLS context exposes no application records
pooled connection reuse does not leak application identity
```

These tests are release-critical.

---

# Logging

Use structured logs.

Prefer a standard library-compatible structured logger such as:

```text
log/slog
```

unless there is a demonstrated need for another library.

Never log complete vectors or credentials.

---

# Metrics

Metrics are useful but not required for the first functional milestone.

Potential future metrics:

```text
request count
request latency
database latency
search latency
records upserted
records searched
result counts
authentication failures
database connection usage
migration status
```

Avoid high-cardinality labels such as:

```text
object_id
projection_id
credential_id
```

Application-level labels should be evaluated carefully before use.

---

# Docker Build

Prefer a multi-stage build.

Conceptually:

```text
Go builder
    ↓
compiled binary
    ↓
minimal runtime image
```

The runtime image should contain only what is needed to:

- run the binary
- establish TLS connections
- expose health endpoints

Do not include source code or build tooling unless required.

Run as a non-root user.

---

# Migrations in Development

The service owns migrations.

For development:

- start supported PostgreSQL + pgvector
- create the expected database
- run service migration command or startup migration path
- execute integration tests

Avoid maintaining a second unrelated schema-setup mechanism.

Tests and production should exercise the same migrations.

---

# Commands

The initial binary may support subcommands if useful:

```text
vector-service serve
vector-service migrate
```

Alternatively, startup may automatically migrate before serving.

Whichever pattern is selected should remain deterministic and documented.

Do not implement two independent migration runners.

---

# Readiness

`/readyz` should fail until:

- configuration is valid
- PostgreSQL is reachable
- required migrations are complete
- database prerequisites are satisfied

`/healthz` should reflect process health rather than dependency readiness.

---

# First Implementation Milestone

The first useful milestone is:

```text
service starts
  ↓
migrations succeed
  ↓
health/readiness work
  ↓
application auth works
  ↓
namespace-scoped upsert works
  ↓
namespace-scoped exact read/delete works
  ↓
vector search works
```

Metadata filtering and broader operational features can follow after the isolation-critical path is tested.

---

# Development Priorities

Order of importance:

1. isolation correctness
2. data correctness
3. predictable failure behavior
4. testability
5. operability
6. performance
7. convenience

Do not sacrifice the first three for the last two.
