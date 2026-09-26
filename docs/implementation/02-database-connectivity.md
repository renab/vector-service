# Unit 02 — Database Connectivity: pgxpool, TLS Client Auth, pgvector Codec

## Objective

Provide the runtime PostgreSQL connection layer: a `pgxpool` pool built from
configuration, TLS client-authenticated transport, bounded connection
acquisition that classifies failures as `unavailable`, and a codec for the
pgvector `vector` type so embeddings travel between Go (`[]float32`) and
PostgreSQL as query parameters. This unit contains no schema SQL, no
application context, and no request handling.

## Authority

- `docs/DEVELOPMENT.md` — `pgxpool`, context/cancellation propagation,
  configuration fail-fast, no insecure default substitutions.
- `docs/SECURITY.md` — TLS client authentication, server certificate
  validation mandatory, dedicated runtime role, no password-based fallback.
- `docs/MIGRATIONS.md` — runtime connection identity and TLS are
  infrastructure prerequisites; the service connects as the runtime role.
- `migrations/0001_shared_vector_schema.sql` — `embedding vector NOT NULL`
  (unconstrained pgvector type, mixed dimensions allowed).
- `docs/implementation/README.md` — configuration reference (runtime and
  migration identity variables), conventions (vector codec), Conflicts
  C2/C3/C4.

## Dependencies

- Unit 01 (`config.Config`, `apierr` classification).

## Scope

1. **Pool construction** (`internal/database`).
   `New(ctx, cfg, logger) (*DB, error)` where `DB` wraps `*pgxpool.Pool`.

   - Build a `pgx.ConnConfig` from config: `Host`, `Port`, `User`, `Database`,
     and **no password**. Authentication is TLS client-certificate based;
     if the server demands a password the connect failure surfaces as
     `unavailable`. There is deliberately no password configuration variable.
   - TLS triple set (`VEC_PG_CA_CERT`, `VEC_PG_CLIENT_CERT`,
     `VEC_PG_CLIENT_KEY`) → build `*tls.Config` with
     `MinVersion: tls.VersionTLS12`, `RootCAs` from the CA file, and the
     client certificate/key pair, and assign it to `ConnConfig.TLSConfig`.
     This gives verify-full semantics: server hostname and certificate chain
     are validated. Never set `InsecureSkipVerify`.
   - TLS triple absent → explicit plain local connection for development/test
     only (e.g. `sslmode=disable`). `docs/SECURITY.md` requires the full
     triple in production; the absence of the triple is a deployment concern,
     the config rule (all-or-none, unit 01) is enforced at startup.
   - Pool settings (defaults are review points, not API semantics):
     `MaxConns` 20, `MinConns` 1, `MaxConnLifetime` 30 m,
     `MaxConnIdleTime` 5 m, `HealthCheckPeriod` 1 m.
   - Validate eagerly at construction: acquire a connection with a 10 s
     deadline and execute `SELECT 1`. Any failure returns an error classified
     as `unavailable` (unit 01's pgx classification).

2. **Bounded acquisition.** Every pool use on a request path acquires with a
   bounded deadline (default 5 s, or the request context deadline when shorter).
   Acquisition timeout or connection-level failure → `unavailable` (503).
   No retry loop that masks an outage from readiness.

3. **pgvector `vector` codec.** pgx does not know the extension type
   `vector`. Register a **text-format codec** on each pool connection:
   in `AfterConnect`, look up the type OID from `pg_type`
   (`typname = 'vector'`) and register the codec **into the
   connection's existing type map**:

   ```go
   var oid uint32
   err := conn.QueryRow(ctx,
       `SELECT oid FROM pg_type WHERE typname = 'vector'`).Scan(&oid)
   if errors.Is(err, pgx.ErrNoRows) {
       return nil // extension absent; Conflict C3 reports it (unit 03)
   }
   if err != nil {
       return err
   }
   return conn.TypeMap().RegisterType("vector", oid, &vectorCodec{})
   ```

   `vectorCodec` maps `[]float32` ↔ pgvector text form `[a,b,c,...]`
   (no dimension suffix — the column is unconstrained `vector`).
   - **Registration is on the per-connection type map pgx already
     initialized, and that map must never be replaced.** `AfterConnect`
     runs once for each physical connection, so each pool connection
     gets the codec registered on *its own* existing `conn.TypeMap()`.
     No fresh `pgx.NewTypeMap()` is ever constructed and no existing
     map is ever discarded: doing so would drop every built-in and
     server-discovered mapping — `uuid`, `jsonb`, `bytea`,
     `timestamptz`, `text` and text arrays, `numeric` — and break all
     non-vector queries in the service. Registration is strictly
     additive: `vector` is added on top of the map pgx initialized,
     which must remain fully intact.
   - If the extension is not present the lookup finds no type; skip
     registration. The missing extension is then reported by the unit 03
     prerequisite check (Conflict C3), not here.
   - The codec must round-trip float32 exactly, including zeros, negatives,
     subnormals, and large values.
   - Encoding only ever receives validated vectors (unit 07 performs
     dimension/finiteness validation in Go before encoding).

4. **Shared connection-config builder (both identities).** Export
   `ConnConfig(cfg, logger, id ConnIdentity) (*pgx.ConnConfig, error)`,
   where `ConnIdentity` carries the role name and the TLS triple paths
   (CA, client cert, client key). Configuration derives exactly two
   identities (Conflict C2):

   - the **runtime** identity: `VEC_PG_USER` with
     `VEC_PG_CA_CERT` / `VEC_PG_CLIENT_CERT` / `VEC_PG_CLIENT_KEY`;
   - the **migration** identity: `VEC_PG_MIGRATION_USER` with
     `VEC_PG_MIGRATION_CLIENT_CERT` / `VEC_PG_MIGRATION_CLIENT_KEY` and
     `VEC_PG_MIGRATION_CA_CERT` (defaulting to the runtime CA when unset).
     The two client certificates are distinct TLS identities, each scoped
     to its role per `docs/SECURITY.md`.

   `New` builds the pool with the runtime identity; the unit 03 migration
   runner calls the same builder with the migration identity. One code
   path builds every connection — the difference is inputs only. The
   runtime pool and the migration connection never share a TLS
   configuration object, and no connection is ever built for
   `postgres` or any role not configured above.

5. **Lifecycle.** `Close(ctx)` closes the pool (shutdown, unit 01 wiring);
   `Ping(ctx, timeout) error` for readiness (unit 09).

## Interfaces and boundaries

- `database.New(ctx, cfg, logger) (*DB, error)`
- `DB.Pool() *pgxpool.Pool` — consumed by unit 04's transaction helpers.
- The pool and its connections carry **no** application context. Setting
  `vector.application_id` outside a transaction is prohibited (unit 04).

## Invariants and correctness constraints

- TLS validation is never disabled. A missing or unreadable certificate file
  is a startup failure, never a fallback to plain transport.
- No caller-controlled value reaches the connection string.
- Connection failures classify to `unavailable` only; SQL-level failures
  keep unit 01's classification.
- The codec is the single place that knows the pgvector wire format.

## Expected implementation surface

```text
internal/database/database.go        # pool construction, ConnConfig builder, Ping, Close
internal/database/vector_codec.go    # vector text codec, OID lookup, registration
internal/database/*_test.go
```

## Validation

- Unit tests (no database):
  - config → `ConnConfig` mapping: TLS triple present (certs loaded, RootCAs
    populated, MinVersion ≥ 1.2), triple absent (plain mode);
  - codec round-trip: 1, 1024, and 16000 dimensions; negative/zero/subnormal
    values; exact float32 equality after decode;
- Integration tests (real PostgreSQL + pgvector, infrastructure from unit 10):
  - connect with client certificate against `sslmode` requiring client auth;
  - insert and read back a 1024-dimensional vector through the codec;
  - **built-in/server-discovered type preservation**: on a connection
    whose `AfterConnect` has registered the vector codec, round-trip
    query parameters and results of `uuid`, `jsonb`, `bytea`,
    `timestamptz`, and `text[]` (e.g. `SELECT $1::uuid, $2::jsonb,
    $3::bytea, $4::timestamptz, $5::text[]` with bound Go values) and
    assert exact equality — proving the `vector` registration did not
    disturb the map pgx initialized;
  - pool reuse across sequential requests;
  - unreachable host / rejected certificate → `unavailable` classification.

## Out of scope

- Migration execution (unit 03), transaction/application-context helpers
  (unit 04), any SQL against schema tables, readiness endpoint (unit 09),
  metrics, read replicas.

## Open issues

None. Pool sizing defaults are review points only.
