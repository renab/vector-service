# Security Model

## Purpose

Vector Service is shared infrastructure serving multiple logically isolated applications.

Security therefore focuses primarily on preventing:

- cross-application data access
- cross-namespace data access
- credential leakage
- unscoped database operations
- direct database bypass by consumers
- accidental privilege expansion

---

# Trust Boundaries

Conceptually:

```text
consumer
   │
   │ application credential
   ▼
Vector Service
   │
   │ PostgreSQL client certificate
   ▼
PostgreSQL
```

The consumer is not trusted to select its application identity.

Vector Service authenticates the consumer and determines application identity.

PostgreSQL independently enforces application scoping using Row-Level Security.

---

# Application Authentication

Normal callers authenticate using opaque application credentials.

Example conceptual header:

```text
Authorization: Bearer <opaque credential>
```

Credentials must be:

- generated using a cryptographically secure random source
- sufficiently high entropy for machine authentication
- transmitted only over trusted encrypted transport
- returned in plaintext only at creation time
- stored only as a digest

## Credential Digest

The database stores:

```text
SHA-256(raw credential)
```

The raw credential is not persisted.

Because application credentials are high-entropy random secrets rather than human-chosen passwords, intentionally slow password hashing is not required for brute-force resistance.

Comparison should avoid obvious timing side channels.

---

# Application Identity

A valid credential resolves to exactly one application.

The normal data-plane API must not accept a free-form application selector.

Incorrect:

```json
{
  "application": "another-application"
}
```

Correct:

```text
application identity = authenticated credential
```

This prevents callers from attempting to cross application boundaries by changing request parameters.

---

# Namespace Authorization

Namespaces belong to applications.

For every namespace-scoped request:

1. authenticate caller
2. resolve application
3. resolve namespace only within that application
4. reject if no visible namespace exists

The service should return the same externally visible failure for:

- nonexistent namespace
- namespace belonging to another application

Recommended:

```text
404 Not Found
```

This reduces namespace enumeration across application boundaries.

---

# PostgreSQL Runtime Identity

Vector Service connects to PostgreSQL using a dedicated runtime role.

Example:

```text
vector_api
```

This role must be:

- login enabled
- non-superuser
- non-database-owner
- non-schema-owner
- `BYPASSRLS = false`
- limited to required tables and operations

The service must not connect as:

```text
postgres
vector_owner
```

during normal operation.

---

# PostgreSQL Transport Authentication

The preferred runtime connection uses infrastructure-managed TLS client authentication.

The client certificate should be:

- issued by database infrastructure
- mounted as a Kubernetes Secret
- rotated automatically where supported
- scoped to the runtime PostgreSQL role

The service should validate the PostgreSQL server certificate.

Do not disable TLS validation.

---

# Row-Level Security

RLS provides defense in depth.

For every authenticated data-plane request, the service starts a transaction and sets application context:

```sql
SELECT set_config(
    'vector.application_id',
    '<application UUID>',
    true
);
```

The third argument must remain:

```text
true
```

This makes the setting transaction-local.

This is required because database connections are expected to be pooled.

Session-level application state can leak one application's identity into another request and must not be used.

---

# Transaction Boundary

Application-scoped database work must occur in this order:

```text
BEGIN
  ↓
SET LOCAL application context
  ↓
all request SQL
  ↓
COMMIT / ROLLBACK
```

Do not:

1. set application context
2. return the connection to the pool
3. perform later SQL outside that transaction

---

# RLS Fail-Closed Behavior

Missing application context should expose no application rows.

There must be no fallback to:

```text
all applications
```

when the transaction variable is missing or invalid.

A failure to establish valid context must terminate the request.

---

# Database Ownership

Schema ownership and runtime access must remain separate.

Conceptually:

```text
vector_owner
    owns schema/database objects

vector_api
    performs constrained runtime operations
```

This separation prevents normal application code from bypassing RLS through ownership privileges.

---

# Consumer Database Access

Consumers should not connect directly to the vector PostgreSQL database.

Expected topology:

```text
consumer
   ↓
Vector Service
   ↓
PostgreSQL
```

Not:

```text
consumer ────────────────┐
                         ▼
Vector Service       PostgreSQL
```

NetworkPolicy should enforce this where practical.

---

# Administrative Authentication

Administrative endpoints are distinct from normal application operations.

Examples:

```text
register application
register namespace
create application credential
disable credential
```

Administrative authority must not be inferred from a normal application credential.

Administrative credentials must use a separate authentication mechanism or distinct credential class.

---

# Credential Rotation

Applications may have multiple simultaneous credentials.

Recommended rotation flow:

```text
create new credential
    ↓
deploy consumer with new credential
    ↓
verify successful use
    ↓
disable old credential
```

Application UUID remains unchanged.

---

# Disabled Applications

If an application is disabled:

- its normal credentials must no longer authenticate successfully
- existing records remain intact
- namespaces remain intact
- historical control-plane data remains intact

Disablement is not deletion.

---

# Disabled Namespaces

A disabled namespace should not permit normal data-plane operations.

Existing records remain until explicitly removed.

---

# Logging

Structured logs should include only operationally useful identifiers.

Recommended fields:

```text
request_id
application_id
namespace_id
operation
duration_ms
result_count
status_code
```

## Never Log

Do not log:

- bearer credentials
- credential digests unless specifically required for forensic debugging
- PostgreSQL private keys
- TLS private keys
- complete embeddings
- source document contents
- arbitrary metadata bodies by default

Application and namespace UUIDs are preferred over potentially sensitive display labels in general-purpose logs.

---

# Error Handling

Errors must not reveal cross-tenant information.

Examples:

An authenticated caller requests a namespace belonging to another application:

```text
404
```

not:

```text
403 namespace belongs to application X
```

An unknown record ID should not disclose whether the record exists under another application.

---

# Request Limits

The service must enforce bounds for:

- HTTP body size
- batch record count
- vector dimensions
- search result limit
- metadata size
- metadata-filter count

Limits protect both resource usage and database stability.

---

# Metadata Safety

Metadata is untrusted caller input.

Never:

- interpolate metadata keys into SQL without validation
- accept raw SQL filters
- accept arbitrary SQL operators
- treat metadata as authorization state

Initial filter operators should remain constrained to the documented structured API.

---

# SQL Injection Defense

All caller-controlled values must use PostgreSQL query parameters.

Identifiers chosen dynamically by callers should be avoided.

If a field name is accepted by the API, validate it against the supported metadata-field grammar before using it.

Do not expose arbitrary table names, schema names, index names, or expressions through the API.

---

# Embedding Safety

Embeddings are untrusted numeric inputs.

Validate:

- expected array structure
- dimensions
- finite values
- vector-space compatibility

Reject:

- NaN
- infinity
- dimension mismatch
- unknown vector spaces

---

# Secret Management

Secrets must not be committed to source control.

Examples include:

- application credentials
- administrative credentials
- database private keys
- TLS client private keys

Runtime secrets should be injected through deployment infrastructure.

---

# Network Exposure

Initial deployment is cluster internal.

There should be no public ingress by default.

If external exposure is added later, it requires a deliberate security review covering:

- authentication
- TLS termination
- rate limiting
- auditability
- public attack surface
- administrative endpoint isolation

---

# Threat Model Summary

The service must protect against:

- caller selecting another application
- caller selecting another application's namespace
- unscoped SQL bugs
- pooled-connection identity leakage
- credential disclosure
- metadata-based injection
- direct database bypass
- overbroad database privileges

The architecture intentionally uses multiple enforcement layers rather than relying on one application-code check.
