# Unit 01 — Foundation: Module, Binary, Configuration, Logging, Errors

## Objective

Establish the Go module, the binary layout with `serve`/`migrate` subcommands,
environment-driven configuration with fail-fast validation, structured logging
with request-ID assignment, and the internal error model that maps to the API
error catalog. This unit contains **no database access and no endpoint
handlers** — it is the substrate every later unit builds on.

## Authority

- `docs/DEVELOPMENT.md` — language, dependencies, configuration, commands,
  logging, HTTP requirements.
- `docs/API.md` — error format, request IDs, HTTP status codes.
- `docs/SECURITY.md` — logging field rules, never-log list.
- `docs/MIGRATIONS.md` — startup order (drives the subcommand decision).
- `docs/implementation/README.md` — configuration reference, error code
  catalog, module path, package conventions.

## Dependencies

None. First unit in the sequence.

## Scope

1. **Go module.** `go.mod` at repository root, module path `vector-service`
   (placeholder — see Open issues). Pin the current stable Go version in `go.mod`
   and in CI/container builds (`docs/DEVELOPMENT.md`). `github.com/jackc/pgx/v5`
   is the only expected external dependency; standard library everywhere else.

2. **Binary layout.** `cmd/vector-service/main.go` with exactly two
   subcommands:

   - `vector-service serve` — load config, connect, run/validate migrations
     (unit 03), start the HTTP server, block until shutdown.
   - `vector-service migrate` — load config, connect, run/validate migrations,
     exit 0 on convergence, non-zero on failure.

   Both subcommands call the **same** migration runner from unit 03
   (`docs/DEVELOPMENT.md`: "Do not implement two independent migration
   runners"). No subcommand → print usage, exit code 2. `serve` starts serving
   only after the migration step succeeds (startup order per
   `docs/MIGRATIONS.md`: load config → connect → validate prerequisites →
   run/validate migrations → start serving → readyz succeeds).

3. **Configuration package** (`internal/config`). Parse all `VEC_*`
   environment variables defined in the overview configuration reference.
   Rules:

   - Missing **required** value → startup failure, non-zero exit, structured
     error naming the variable. No insecure defaults are substituted.
   - Validate: integer parsing and positivity for limits; `VEC_LISTEN_ADDR`
     parses as a host:port; port range for `VEC_PG_PORT`;
     `VEC_PG_DATABASE` must equal exactly `vector` (Conflict C4 — migration
     `0004` grants on `DATABASE vector`); `VEC_SEARCH_MAX_LIMIT` must not
     exceed the hard API ceiling of 200 (`docs/API.md`); `VEC_LOG_LEVEL` ∈
     {debug, info, warn, error}; `VEC_LOG_FORMAT` = `json`.
   - TLS is configured **per identity, each triple all-or-none**
     (Conflict C2, two-role model):
     - **Runtime triple** (`VEC_PG_CA_CERT`, `VEC_PG_CLIENT_CERT`,
       `VEC_PG_CLIENT_KEY`): all three set (TLS client-authentication
       connection) or none set (plain local/test connection). Production
       deployments must set all three (operational requirement,
       `docs/SECURITY.md`).
     - `VEC_PG_MIGRATION_USER` is **required** (no default): the
       distinct, privileged migration identity. A deployment without it
       fails startup — there is no fallback to the runtime role
       (single-role migrate-then-serve is not a supported shape, C2).
     - **Migration triple**: `VEC_PG_MIGRATION_CLIENT_CERT` and
       `VEC_PG_MIGRATION_CLIENT_KEY` are a **pair** (both or neither);
       `VEC_PG_MIGRATION_CA_CERT` is optional and defaults to
       `VEC_PG_CA_CERT`. When the runtime triple is set (production),
       the migration cert/key pair is required as well — both
       identities use TLS client authentication. The two identities
       must use **distinct client certificates** (SECURITY.md scopes a
       client certificate to one PostgreSQL role); the same CA may
       serve both.
   - Collect **all** validation errors and report them together; fail fast
     before any network activity.
   - The resulting `config.Config` is an immutable value passed explicitly to
     every component. No global configuration state.

4. **Logging** (`internal/log`). `log/slog` with a JSON handler to stderr.
   - Level from configuration.
   - Request ID assignment: accept inbound header `X-Request-ID` **only if** it
     matches `^[A-Za-z0-9._-]{1,128}$`; otherwise generate a UUID v4
     (crypto/rand). Store it in the request context, echo it in the response
     header `X-Request-ID`, and attach it to every log record for that
     request. A request ID is not an authentication token and must never be
     treated as one.
   - Operational fields per `docs/SECURITY.md`: `request_id`,
     `application_id` (UUID only), `namespace_id` (UUID only), `operation`,
     `duration_ms`, `result_count`, `status_code`.
   - Never log: bearer credentials, the admin token, credential digests,
     private keys, complete embeddings, metadata bodies, source contents.

5. **Error model** (`internal/apierr`). An internal error type carrying
   `code` (stable string from the overview catalog), `status` (HTTP status),
   `message` (operator-facing, no cross-tenant detail), and an optional
   wrapped cause. Requirements:

   - Constructor helpers for every code in the catalog.
   - Classification of `pgx`/`pgconn` failures: connection-level failures and
     pool acquisition timeouts map to `unavailable` (503); unexpected SQL
     errors (constraint violations, trigger exceptions, unknown SQLSTATE) map
     to `internal` (500). Unique violations are classified separately because
     the upsert/admin paths interpret them as `conflict` (409) (units 06–07).
   - Raw PostgreSQL error text is **never** placed in an API response body; it
     may appear in structured logs (unit 09 owns the response writer).

6. **Shared domain types** (`internal/model`). Plain structs for
   `Application`, `Namespace`, `VectorSpace`, `CredentialInfo`, `Record`, plus
   request/response DTOs shared by more than one package. Timestamps are RFC
   3339 in JSON; nullable fields (`source_updated_at`, `expires_at`) are
   omitted from JSON when NULL. No logic beyond (de)serialization.

7. **HTTP server skeleton** (`internal/api`). Construct `*http.Server` with
   explicit timeouts (defaults, review points): `ReadHeaderTimeout` 10 s,
   `ReadTimeout` 30 s, `WriteTimeout` 60 s, `IdleTimeout` 120 s. Standard
   library `http.ServeMux` with method+path patterns — no router library.
   Middleware chain, in order: request-ID assignment → panic recovery (→ 500
   `internal`, logged) → access logging → bounded body reading. Endpoint
   registration for `/healthz`, `/readyz`, `/v1/...` is wired by later units;
   this unit provides the chain and a working `/healthz` (see unit 09 for the
   readiness endpoint). Graceful shutdown: SIGTERM/SIGINT → `Server.Shutdown`
   with 30 s grace, then close database resources (units 02/03).

## Interfaces and boundaries

- `config.Load() (config.Config, error)` — the single entry point; every
  component receives `config.Config` by value.
- `log.RequestID(ctx)` / `log.WithRequestID(ctx, id)` — context plumbing used
  by every handler.
- `apierr.Error` (code/status/message/cause) — the **only** error type that
  crosses from service code into the HTTP response writer. All other errors
  must be wrapped into it.
- `api.NewServer(cfg, deps) *http.Server` — `deps` is the struct assembled by
  `main` holding the pool, authenticators, and service implementations from
  later units.

## Invariants and correctness constraints

- No network I/O, no database, no global mutable state in this unit.
- Configuration is fail-fast: an invalid deployment exits before listening.
- No insecure default substitutes a missing required value.
- Request IDs are bounded, validated, and never derived from credentials.
- Error messages are stable and tenant-neutral.

## Expected implementation surface

```text
go.mod / go.sum
cmd/vector-service/main.go          # subcommand dispatch, shutdown wiring
internal/config/config.go           # env parsing + validation
internal/config/config_test.go
internal/log/log.go                 # slog setup, request ID helpers
internal/log/log_test.go
internal/apierr/errors.go           # error type, constructors, pgx classification
internal/apierr/errors_test.go
internal/model/model.go             # domain + DTO types
internal/api/server.go              # http.Server, middleware chain
internal/api/middleware_test.go
```

## Validation

- `go build ./...` and `go vet ./...` clean.
- Table-driven unit tests:
  - config: every variable parsed; each failure mode (missing required, bad
    integer, bad database name, limit above 200, partial TLS triple) produces a
    startup error; valid combinations load.
  - request ID: accepted when valid, generated (and different) when missing or
    invalid; bounded length enforced.
  - error model: every catalog code maps to its documented status; pgx
    connection failure → 503; unexpected SQL error → 500.
  - subcommands: no args → exit 2 with usage.
- No integration test in this unit (no dependencies yet).

## Out of scope

- Database connectivity, migrations, authentication, any `/v1` endpoint,
  readiness checks, Dockerfile, test infrastructure.

## Open issues

- **Module path.** `vector-service` is a placeholder. If a canonical VCS path
  exists for this repository, `go.mod` must use it. Review point.
