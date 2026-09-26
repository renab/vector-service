# Unit 05 — Authentication: Application Credentials and Admin Token

## Objective

Implement the two distinct authentication mechanisms:

1. **Data-plane application authentication** — resolve an opaque
   `Bearer` credential to exactly one enabled application by
   SHA-256 digest lookup, with uniform 401 for every failure mode and no
   propagation of the raw credential beyond the authentication code.
2. **Admin authentication** — a separate configured high-entropy token
   (`VEC_ADMIN_TOKEN`) that authorizes only administrative endpoints and
   can never authenticate data-plane operations.

This unit produces the authenticated identity that unit 04's
`WithAppContext` and unit 06's admin handlers consume.

## Authority

- `docs/API.md` — `Authorization: Bearer <opaque-application-credential>`;
  401 for missing/unknown/disabled/expired credential and disabled
  application; credential returned exactly once at creation; admin
  authentication distinct from application authentication.
- `docs/SECURITY.md` — high-entropy machine credentials; store only
  `SHA-256(raw)`; avoid obvious timing side channels; credential resolves
  to exactly one application; no free-form application selector; disabled
  applications authenticate nothing; admin credentials are a separate
  mechanism.
- `docs/DATA_MODEL.md` — `application_credentials` fields, multiple
  credentials per application, disablement semantics.
- `migrations/0004_runtime_identity.sql` — `credential_hash bytea NOT NULL`
  with `octet_length = 32`, `UNIQUE (credential_hash)`, `enabled`,
  `expires_at`, `last_used_at`; FK to `applications` with `ON DELETE
  CASCADE`.
- `docs/DEVELOPMENT.md` — authentication steps (validate header → hash →
  look up enabled credential → verify application enabled → update usage
  metadata as appropriate → attach identity); do not propagate the raw
  credential.
- `docs/implementation/README.md` — Conflicts C8, C10; Global Invariant 8
  (digest rule), Invariant 9 (admin distinctness); error catalog.

## Dependencies

- Unit 01 (`apierr`, logging, config — `VEC_ADMIN_TOKEN`).
- Unit 02 (pool, bounded acquisition).
- Unit 04 (the authenticated identity is consumed by `WithAppContext`;
  this unit does not depend on its internals).

## Scope

### Data-plane authentication

1. **Header parsing** (`internal/auth`).
   - `Authorization` header must be exactly `Bearer <token>`: case-insensitive
     `Bearer` scheme, single space, non-empty token, no extra whitespace.
   - Bounded token length (proposal: 512 bytes; review point) — longer or
     otherwise malformed headers are rejected **before** any database work.
   - Every rejection (missing header, malformed, unknown, disabled,
     expired, disabled application) returns the **same** error:
     `unauthorized` (401). No response or default log line may distinguish
     which failure occurred.

2. **Digest and lookup.**
   - Compute `SHA-256` over the **full raw credential string exactly as
     received** (Conflict C8: the wire format is `vsvc_` + base64url(32
     random bytes); the digest covers the prefix as well). The raw string
     is discarded immediately after hashing and is never logged, never
     stored in context, and never passed to any other package.
   - Look up by digest in one statement on a short-lived bounded
     transaction:

     ```sql
     SELECT c.id AS credential_id, c.application_id, c.last_used_at
     FROM vector_control.application_credentials c
     JOIN vector_control.applications a ON a.id = c.application_id
     WHERE c.credential_hash = $1
       AND c.enabled = true
       AND (c.expires_at IS NULL OR c.expires_at > now())
       AND a.enabled = true;
     ```

   - Zero rows → 401. One row (guaranteed by `UNIQUE (credential_hash)`) →
     success with the application UUID.
   - `vector_api` holds `SELECT` on both tables (migration `0004`); no RLS
     applies to either (only `vector_records` and `namespaces` are RLS
     forced), so this lookup needs no application context.
   - **Timing side channel**: when the digest lookup finds no row, perform
     `crypto/subtle.ConstantTimeCompare` against a fixed dummy 32-byte
     digest before failing, so the unknown-credential path does not
     short-circuit at a visibly different time (SECURITY.md: "avoid
     obvious timing side channels").

3. **Identity propagation.** On success, attach to the request context:
   application UUID, credential ID (for logging and `last_used_at`).
   Nothing else. Handlers retrieve identity only from context; no handler
   accepts an application selector (Global Invariant 1).

4. **`last_used_at` policy.** Usage metadata is best-effort and must never
   fail or delay the request:
   - Only when the stored `last_used_at` is NULL or older than a throttle
     window (proposal: 60 s; review point), issue
     `UPDATE vector_control.application_credentials SET last_used_at = now()
     WHERE id = $1` in a short separate transaction after authentication
     succeeds.
   - Failure (including DB unavailability) is logged at warn with the
     credential UUID only, and the request proceeds.
   - Never log the credential digest in these logs by default
     (SECURITY.md logging rules).

5. **Database failure vs authentication failure.** Pool acquisition or
   query failure → `unavailable` (503), not 401: an unprovable identity is
   an infrastructure problem, not a credential problem. The two outcomes
   remain distinct in responses.

### Admin authentication

6. **Admin token.** `VEC_ADMIN_TOKEN` (required, unit 01) is a
   high-entropy secret injected by infrastructure. Admin endpoints
   (unit 06) authenticate with `Authorization: Bearer <admin token>`
   compared via `crypto/subtle.ConstantTimeCompare` against the configured
   value. No database involvement.
   - Missing/malformed/incorrect token → `unauthorized` (401) with the same
     uniform body as any other 401.
   - The admin token is **never** consulted by data-plane handlers, and
     application credentials are never accepted on admin endpoints. The
     two mechanisms share no code path beyond the 401 response (DEVELOPMENT.md:
     keep admin handlers separated).
   - The token is never logged; logs identify admin requests by
     `request_id` and operation only.

7. **Credential generation (used by unit 06).** Provide the generator:
   `vsvc_` + unpadded base64url of 32 bytes from `crypto/rand` (Conflict
   C8). The returned raw string is the only plaintext that ever exists; the
   caller (admin endpoint) persists only `SHA-256(raw)`.

## Interfaces and boundaries

- `auth.Authenticate(ctx, db, bearer string) (Identity, error)` where
  `Identity` carries application UUID + credential ID; error is
  `apierr`-typed (401 or 503 only).
- `auth.CheckAdmin(bearer string, cfg) error` — admin endpoints only.
- `auth.GenerateCredential() (raw string, err error)` — unit 06 only.
- Context accessors for the authenticated application UUID (unit 01/04
  logging fields).

## Invariants and correctness constraints

- **Uniform 401.** No enumeration: missing, malformed, unknown, disabled,
  expired, and disabled-application cases are externally identical.
- **Digest is the only persisted representation.** 32-byte SHA-256 of the
  full raw string; the raw value exists only transiently in the
  authentication code.
- **Constant-time comparison** at every secret comparison (credential
  digest path equalized with a dummy compare; admin token via
  `subtle.ConstantTimeCompare`).
- **Identity is single-valued.** One credential → one application; no
  request field can alter it.
- **Disabled application ⇒ no authentication**, even with an enabled,
  unexpired credential (the JOIN enforces both).
- **Raw credential never logged, never in context, never in error
  messages.**
- **Admin and data-plane auth are disjoint mechanisms** (Global Invariant
  9).

## Expected implementation surface

```text
internal/auth/auth.go              # header parsing, digest lookup, identity
internal/auth/admin.go             # admin token check
internal/auth/credential.go        # vsvc_ generator, last_used_at throttle
internal/auth/*_test.go
```

## Validation

- Unit tests (no database), table-driven:
  - header parsing: valid; missing header; empty token; lowercase `bearer`;
    double space; trailing garbage; over-length token — all rejected
    uniformly;
  - generation: prefix, base64url alphabet, 32-byte entropy (length
    check), uniqueness across draws;
  - digest rule: `SHA-256` of the full `vsvc_...` string (fixture with
    known digest);
  - admin token: exact match passes, any difference fails, constant-time
    API used (assert via code review / interface).
- Integration tests (real PostgreSQL; unit 10 infrastructure):
  - valid enabled credential of an enabled application → identity;
  - disabled credential → 401; expired credential → 401; disabled
    application with enabled credential → 401; unknown digest → 401
    (response bodies identical across all four);
  - DB down → 503, not 401;
  - `last_used_at` remains NULL or stale until a throttled update fires; a
    second request within the window issues no UPDATE (assert via
    statement count or timestamp stability);
  - credential digest column holds exactly 32 bytes and equals the expected
    SHA-256 of the generated raw value.

## Out of scope

- Admin endpoint implementations (unit 06).
- Data-plane operation authorization beyond "authenticated application +
  resolved namespace" (unit 04).
- Rate limiting, lockouts, and audit logging of failed logins (not in v1
  authority; a future operational addition).
- Credential expiry enforcement beyond the `expires_at` predicate.

## Open issues

- **Credential wire format (C8).** The `vsvc_` prefix convention and the
  digest-over-full-string rule follow the `docs/API.md` example; the exact
  entropy length (32 bytes) and base64url encoding are the contracts'
  proposal. The digest rule itself is authoritative. Review point.
- **`last_used_at` throttle window (60 s) and best-effort semantics** are
  proposals consistent with DEVELOPMENT.md's "as appropriate"; review
  point.
