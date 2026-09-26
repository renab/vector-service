# Vector Service API v1

## Purpose

Vector Service provides generic shared vector storage for multiple independent applications.

It owns:

- application registration
- namespace registration
- application authentication
- vector-space compatibility enforcement
- generic vector-record persistence
- nearest-neighbor search
- generic metadata filtering
- vector-record lifecycle

It does not own:

- embedding generation
- embedding query instructions
- source parsing
- chunking
- semantic interpretation
- candidate orchestration
- reranking
- provenance semantics
- temporal semantics
- application-specific retrieval behavior

Those remain the responsibility of consuming applications.

---

# Base Path

All version 1 endpoints use:

```text
/v1
```

Health endpoints are not versioned.

---

# Content Type

Requests and responses use:

```text
application/json
```

unless otherwise documented.

---

# Authentication

## Data-Plane Authentication

Normal application requests use:

```text
Authorization: Bearer <opaque-application-credential>
```

The credential identifies exactly one registered application.

The caller does not provide:

- `application_id`
- `application_key`
- any equivalent application selector

in normal data-plane requests.

Application identity is derived exclusively from authentication.

---

## Authentication Failure

A missing, unknown, disabled, or expired application credential returns:

```text
401 Unauthorized
```

---

# Namespace Isolation

Every data-plane operation is explicitly namespace scoped.

There is:

- no default namespace
- no wildcard namespace
- no implicit all-namespace operation
- no implicit cross-namespace search

A namespace must belong to the authenticated application.

If a namespace:

- does not exist
- is disabled
- belongs to another application

the service returns:

```text
404 Not Found
```

The API deliberately does not reveal whether a namespace exists under another application.

---

# Vector Spaces

Every vector write and vector search explicitly selects a registered vector space.

Example:

```text
example-embedding-1024-cosine-v1
```

A vector space defines an immutable compatibility contract including:

- embedding model
- embedding model version
- dimensionality
- distance metric
- service-level compatibility version

The service rejects vectors incompatible with the selected vector space.

Normal data-plane callers cannot create or modify vector spaces.

---

# Record Identity

A logical vector record is uniquely identified by:

```text
application
namespace
object_id
projection_id
vector_space
```

`object_id` and `projection_id` are opaque caller-owned strings.

Vector Service does not interpret their semantics.

---

# Upsert Records

```text
PUT /v1/namespaces/{namespace}/records
```

Upserts one or more vector records into an explicit namespace.

## Request

```json
{
  "vector_space": "example-embedding-1024-cosine-v1",
  "records": [
    {
      "object_id": "document-123",
      "projection_id": "chunk-0001",
      "content_hash": "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
      "source_updated_at": "2026-01-01T12:00:00Z",
      "metadata": {
        "document_type": "note",
        "state": "active"
      },
      "vector": [
        0.0123,
        -0.456,
        0.078
      ]
    }
  ]
}
```

The example vector is abbreviated. Actual vector length must match the selected vector space.

## Requirements

`records` must:

- contain at least one record
- remain within the configured maximum batch size
- use one vector space per request

Each record requires:

- non-empty `object_id`
- non-empty `projection_id`
- valid SHA-256 `content_hash`
- valid vector
- JSON-object metadata

`source_updated_at` is optional.

Vectors must:

- match the vector-space dimensionality
- contain only finite numeric values
- not contain NaN
- not contain positive or negative infinity

## Behavior

Upsert identity is:

```text
namespace
object_id
projection_id
vector_space
```

within the authenticated application.

An existing matching record is updated.

A missing record is inserted.

The service owns the internal record UUID.

A record whose current representation is already identical may be treated as unchanged.

## Response

```json
{
  "upserted": 18,
  "unchanged": 42
}
```

---

# Search

```text
POST /v1/namespaces/{namespace}/search
```

Performs nearest-neighbor search within exactly one namespace.

## Request

```json
{
  "vector_space": "example-embedding-1024-cosine-v1",
  "vector": [
    0.0123,
    -0.456,
    0.078
  ],
  "limit": 20,
  "filters": {}
}
```

## `limit`

Allowed range:

```text
1..200
```

The deployment may configure a lower operational maximum.

## Response

```json
{
  "matches": [
    {
      "record_id": "01900000-0000-7000-8000-000000000001",
      "object_id": "document-123",
      "projection_id": "chunk-0004",
      "score": 0.9123,
      "content_hash": "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
      "source_updated_at": "2026-01-01T12:00:00Z",
      "metadata": {
        "document_type": "note",
        "state": "active"
      }
    }
  ]
}
```

The stored embedding is not returned.

## Score Semantics

`score` is normalized by Vector Service so that:

```text
higher = better match
```

Consumers must not depend on raw pgvector distance values.

Exact score transformation is an implementation detail, but it must remain consistent for a given vector-space distance metric.

---

# Metadata Filtering

API v1 exposes a constrained structured filter language.

It does not expose:

- SQL
- SQL operators
- JSONPath
- arbitrary PostgreSQL expressions

Initial operators are:

```text
eq
in
```

Only top-level metadata keys are supported in API v1.

---

## Equality Filter

```json
{
  "filters": {
    "all": [
      {
        "field": "document_type",
        "op": "eq",
        "value": "note"
      }
    ]
  }
}
```

---

## IN Filter

```json
{
  "filters": {
    "all": [
      {
        "field": "state",
        "op": "in",
        "values": [
          "active",
          "current"
        ]
      }
    ]
  }
}
```

---

## Combined Filters

```json
{
  "filters": {
    "all": [
      {
        "field": "document_type",
        "op": "eq",
        "value": "note"
      },
      {
        "field": "state",
        "op": "in",
        "values": [
          "active",
          "current"
        ]
      }
    ]
  }
}
```

All filters in `all` must match.

API v1 does not define arbitrary nested boolean filter expressions.

That capability may be added later if a demonstrated requirement exists.

---

# Get Record

```text
GET /v1/namespaces/{namespace}/records/{record_id}
```

Returns one visible record.

## Response

```json
{
  "record_id": "01900000-0000-7000-8000-000000000001",
  "object_id": "document-123",
  "projection_id": "chunk-0001",
  "vector_space": "example-embedding-1024-cosine-v1",
  "content_hash": "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
  "source_updated_at": "2026-01-01T12:00:00Z",
  "metadata": {
    "document_type": "note"
  },
  "created_at": "2026-01-01T12:01:00Z",
  "updated_at": "2026-01-01T12:01:00Z"
}
```

The stored embedding is not returned.

A record outside the authenticated application's namespace scope returns:

```text
404 Not Found
```

---

# Delete Logical Projection

```text
DELETE /v1/namespaces/{namespace}/records
```

Deletes one logical projection.

## Request

```json
{
  "object_id": "document-123",
  "projection_id": "chunk-0001",
  "vector_space": "example-embedding-1024-cosine-v1"
}
```

## Response

```json
{
  "deleted": 1
}
```

Repeated deletion is idempotent:

```json
{
  "deleted": 0
}
```

---

# Delete Object

```text
DELETE /v1/namespaces/{namespace}/objects/{object_id}
```

Deletes all projections belonging to an object within exactly one namespace.

## Optional Vector-Space Filter

```text
?vector_space=example-embedding-1024-cosine-v1
```

When supplied, only projections belonging to that vector space are removed.

When omitted, all vector-space projections for the object within the selected namespace are removed.

This operation never crosses namespace boundaries.

## Response

```json
{
  "deleted": 14
}
```

---

# Health Endpoint

```text
GET /healthz
```

Confirms that the service process is functioning.

This endpoint does not require application authentication.

It must not expose application or database contents.

## Successful Response

```json
{
  "status": "ok"
}
```

---

# Readiness Endpoint

```text
GET /readyz
```

Confirms that the service is ready to satisfy requests.

Readiness includes at minimum:

- successful configuration
- required database migrations applied
- usable PostgreSQL connection

This endpoint does not require application authentication.

## Successful Response

```json
{
  "status": "ready"
}
```

## Unavailable Response

```text
503 Service Unavailable
```

---

# Administrative API

Administrative endpoints are logically separate from normal application operations.

Administrative authentication must be distinct from application authentication.

Administrative endpoints are intended to remain on trusted internal networks.

---

# Register Application

```text
POST /v1/admin/applications
```

## Request

```json
{
  "application_key": "notes-service",
  "display_name": "Notes Service"
}
```

## Response

```json
{
  "id": "01900000-0000-7000-8000-000000000010",
  "application_key": "notes-service",
  "display_name": "Notes Service",
  "enabled": true
}
```

Application keys are immutable after creation.

Attempting to register an existing conflicting key returns:

```text
409 Conflict
```

---

# Register Namespace

```text
POST /v1/admin/applications/{application}/namespaces
```

`{application}` is the application key.

## Request

```json
{
  "namespace_key": "project-alpha",
  "display_name": "Project Alpha",
  "metadata": {}
}
```

## Response

```json
{
  "id": "01900000-0000-7000-8000-000000000020",
  "application_key": "notes-service",
  "namespace_key": "project-alpha",
  "display_name": "Project Alpha",
  "enabled": true,
  "metadata": {}
}
```

Namespace keys are unique within an application.

---

# Create Application Credential

```text
POST /v1/admin/applications/{application}/credentials
```

## Request

```json
{
  "credential_name": "production"
}
```

## Response

```json
{
  "credential_id": "01900000-0000-7000-8000-000000000030",
  "credential_name": "production",
  "credential": "vsvc_<opaque-secret>"
}
```

The raw credential is returned exactly once.

Only its cryptographic digest is persisted.

The client is responsible for securely storing the returned credential.

---

# Disable Application Credential

```text
DELETE /v1/admin/applications/{application}/credentials/{credential_id}
```

This disables the credential immediately.

It does not remove:

- the application
- the namespace
- vector records
- historical credential identity

## Response

```json
{
  "disabled": true
}
```

Repeated disablement is idempotent.

---

# List Vector Spaces

```text
GET /v1/vector-spaces
```

Returns vector spaces currently available for normal operations.

This endpoint requires normal application authentication.

## Response

```json
{
  "vector_spaces": [
    {
      "key": "example-embedding-1024-cosine-v1",
      "embedding_model": "example-embedding-model",
      "embedding_model_version": "v1",
      "dimensions": 1024,
      "distance_metric": "cosine"
    }
  ]
}
```

Retired or disabled vector spaces may be omitted from normal responses.

---

# PostgreSQL Isolation

For every authenticated data-plane request, Vector Service:

1. resolves the credential to an application UUID
2. resolves the namespace within that application
3. starts a PostgreSQL transaction
4. establishes transaction-local application context
5. performs all application-scoped SQL within that transaction
6. commits or rolls back

Conceptually:

```sql
SELECT set_config(
    'vector.application_id',
    '<application UUID>',
    true
);
```

PostgreSQL Row-Level Security independently enforces the same application boundary.

Application isolation therefore exists at two layers:

1. Vector Service authorization
2. PostgreSQL RLS

---

# Error Format

Errors use a stable JSON structure.

Example:

```json
{
  "error": {
    "code": "invalid_vector_dimensions",
    "message": "vector dimensions do not match the selected vector space",
    "request_id": "01900000-0000-7000-8000-000000000040"
  }
}
```

`code` is machine-readable.

`message` is intended for operators/developers.

`request_id` allows correlation with service logs.

Raw PostgreSQL errors must not be returned directly.

---

# HTTP Status Codes

## `400 Bad Request`

Examples:

- malformed JSON
- missing required field
- unsupported metadata filter
- invalid SHA-256 representation
- invalid limit
- invalid vector number

## `401 Unauthorized`

Examples:

- missing application credential
- invalid application credential
- disabled credential
- expired credential
- disabled application

## `404 Not Found`

Examples:

- namespace not visible to authenticated application
- record not visible within namespace
- object not visible within namespace

The API must not reveal whether a hidden resource exists under another application.

## `409 Conflict`

Examples:

- duplicate application key
- duplicate namespace registration
- immutable resource conflict

## `413 Content Too Large`

Examples:

- HTTP body exceeds configured maximum
- record batch exceeds configured maximum

## `422 Unprocessable Entity`

Examples:

- vector dimensions incompatible with vector space
- vector contains NaN or infinity
- valid request references an incompatible vector-space state

## `503 Service Unavailable`

Examples:

- PostgreSQL unavailable
- required database state unavailable
- service has not completed required migrations

---

# Request IDs

The service should assign or accept a request ID.

If accepted from callers, the external value must be validated and bounded.

Request IDs should be returned in responses and included in structured logs.

A request ID is not an authentication or authorization token.

---

# Batch Limits

Upsert operations must have bounded batch size.

The exact deployment limit is configuration.

The API must reject oversized batches rather than accepting unbounded memory or database work.

---

# Search Limits

Search result counts are bounded.

API v1 allows:

```text
1..200
```

The configured deployment maximum may be lower.

The service must not permit an unbounded result set.

---

# Vector Validation

Before database search or insertion, vectors must be validated for:

- JSON numeric representation
- expected dimensions
- finite values
- selected vector-space compatibility

NaN and infinity are invalid.

---

# Metadata

Metadata must be a JSON object.

Examples of acceptable metadata:

```json
{
  "type": "note",
  "state": "active",
  "language": "en"
}
```

Metadata is application data.

It is not authoritative for:

- application identity
- namespace identity
- authorization
- vector-space identity

Those are represented separately.

---

# API Versioning

Breaking API changes require a new major API path.

Example:

```text
/v2
```

Compatible additive changes may remain within `/v1`.

Examples of additive changes:

- new optional response fields
- new administrative endpoints
- new filter operators with explicit semantics

Existing endpoint semantics must not silently change.

---

# Non-Goals

API v1 deliberately does not provide:

- text-to-vector embedding
- natural-language query endpoints
- reranking
- chunk generation
- document ingestion
- source-content storage
- wildcard namespace operations
- cross-application search
- implicit cross-namespace search
- arbitrary SQL
- arbitrary JSONPath
- arbitrary PostgreSQL expressions
