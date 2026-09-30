# Test Notes

Test-environment decisions and run instructions for the Vector Service
integration test suite. This is the single home for the test-infrastructure
decisions (database and extension pins, how to run) that the implementation
contract (package 5, scope 5a) says must not be scattered through code.

The harness itself is `internal/testdb` (see that package's documentation);
this file records the environment it runs against and how to invoke the
tests.

---

## Test layers

| Layer | Runs on | Build tag |
|-------|---------|-----------|
| Unit tests | No database (pure logic: config, codec, framing, filter grammar, error classification) | *(none — default)* |
| Database integration tests | Real PostgreSQL + pgvector, via the `internal/testdb` harness | `integration` |
| HTTP integration tests | Real PostgreSQL + pgvector, via the harness, against the production handler chain | `integration` |

`go test ./...` (no tag) runs the unit layer only and is always green on a
host with no database. The integration layer is run explicitly with the tag
and a configured bootstrap identity; it is what CI runs on every change
(release-critical matrices — see package 5, scope 5b).

RLS, transaction-scoped `set_config`, triggers, `ON CONFLICT`, advisory
locks, and `vector` operators are never mocked: they are tested against real
PostgreSQL + pgvector (overview invariant 14; `DEVELOPMENT.md`).

---

## Database and extension pins

- **PostgreSQL: 18.** The pin is the root `README.md`'s requirement and is
  documented here, not repeated in code.
- **pgvector: 0.8.6.** The extension release is not pinned by any
  authoritative source; this is an **infrastructure decision** recorded here.
  The 0.8.x line supports the `hnsw` index type that migration `0003`
  creates (`USING hnsw`), which is the minimum the service requires.
- Reference image for a disposable test cluster:
  `pgvector/pgvector:0.8.6-pg18-bookworm` (PostgreSQL 18 + pgvector 0.8.6).
- Go builder: `golang:1.27.1-alpine3.23` (matches `go.mod`).
- Distroless runtime:
  `gcr.io/distroless/static-debian13:nonroot@sha256:1c2c046bc09ed40fad370b599a0b1ae7987f55b01e247cf27a7c27cd97e5bbc7`
  (Debian 13, nonroot, static binary; pinned by digest).

---

## The bootstrap identity

The harness (`internal/testdb`) is driven by a **test bootstrap identity**:
a superuser (or infrastructure admin) used only for provisioning and
direct-DB fixture assertions. No service code path ever runs as it.

The bootstrap identity's connection string is supplied by the operator
through the environment variable:

```
VECTOR_SERVICE_TEST_PG_DSN
```

It must be a superuser connection to the maintenance database (e.g.
`postgres`). The value is never logged by the harness and never embedded in
an error message.

The harness provisions, per test suite (package), a disposable database
named `vector` (the name is fixed by configuration validation and by
migration `0004`'s `GRANT CONNECT ON DATABASE vector`), the `vector`
extension, the runtime role `vector_api`, and the canonical migration
identity `vector_owner` (the database owner). Provisioning and convergence
(`migrate.Run`, the production runner) are separate stages; teardown drops
the database.

The harness never builds or asserts alternate provisioning shapes, and it
never converges a negative-identity variant (those are rejection fixtures
only).

---

## Running the integration tests

### 1. Start a disposable PostgreSQL + pgvector cluster

The two identities the harness creates — `vector_api` and `vector_owner` —
must be able to connect to the suite database. For a disposable test
cluster the simplest trust model is host authentication:

```sh
docker run -d --name vector-test-pg \
  -e POSTGRES_HOST_AUTH_METHOD=trust \
  -p 55432:5432 \
  pgvector/pgvector:0.8.6-pg18-bookworm
```

(Any real PostgreSQL 18 + pgvector 0.8.6 works; Docker is one convenient
implementation. A local service with the two roles present and connectable
is equivalent. The container above is disposable: kill it after the run.)

### 2. Set the bootstrap DSN

```sh
export VECTOR_SERVICE_TEST_PG_DSN="postgres://postgres@127.0.0.1:55432/postgres"
```

(No password in the example because the container uses `trust`; with a
real cluster use the bootstrap identity's credentials.)

### 3. Run the tests

```sh
# Unit layer only (no database needed):
go test ./...

# Compile-check the integration layer without running it:
go test -tags integration -run xxx_none ./...

# Run the harness smoke test (provision -> converge -> boundary -> teardown):
go test -tags integration -v -run TestHarnessEndToEnd ./internal/testdb/

# Run the full integration suite (all integration-tagged tests):
# Use -p 1 to run packages serially; integration tests share fixed
# cluster-global roles (vector_api, vector_owner) and a single database
# cluster, so parallel package execution causes cross-test interference.
go test -p 1 -tags integration -v ./...
```

### 4. Tear down the cluster

```sh
docker rm -f vector-test-pg
```

The harness drops the suite database at suite end; the container itself is
removed by the operator.

---

## CI

CI starts one disposable PostgreSQL + pgvector environment (Docker or a
local service), sets `VECTOR_SERVICE_TEST_PG_DSN`, and runs the integration
tag on **every** change. The release-critical isolation matrix, the
migration matrix, and the HTTP integration tests all run there and must not
be skippable by build tag in the pipeline. See package 5, scope 5b and 5c,
for the matrix entry points and the image-level verification commands.

The GitHub Actions workflow (`.github/workflows/ci.yml`) runs three jobs:

1. **Unit tests** — `go test ./...` (no database, no build tag).
2. **Integration tests** — `go test -p 1 -tags=integration -v ./...`
   against a PostgreSQL 18 + pgvector 0.8.6 service container.
3. **Container image** — builds the Dockerfile, provisions a canonical
   database (roles, database, vector extension), runs the contract-level
   verification script (`scripts/verify-image.sh`). The verification
   containers use bridge networking with
   `--add-host=host.docker.internal:host-gateway` to reach the PostgreSQL
   service container through the Docker host gateway. The script requires
   `VECTOR_SERVICE_IMAGE_TEST_DISPOSABLE=1` before performing any DDL.

---

## Container image

### Build

```sh
docker build -t vector-service:local .
```

This produces a two-stage image:

- **Stage 1 (builder):** `golang:1.27.1-alpine3.23` with `CGO_ENABLED=0`
  producing a fully static binary at `/out/vector-service`.
- **Stage 2 (runtime):** `gcr.io/distroless/static-debian13:nonroot`
  containing only the binary at `/usr/local/bin/vector-service`, running
  as `nonroot` (UID 65532), with `ENTRYPOINT ["/usr/local/bin/vector-service"]`
  and `CMD ["serve"]`.

The runtime image contains no shell, no Go toolchain, no source files,
no migration directory, no test artifacts, and no secrets.

### Local image verification

Run the contract-level verification script against a local PostgreSQL
cluster. The script uses bridge networking with
`--add-host=host.docker.internal:host-gateway` so Docker containers can
reach the PostgreSQL service running on the host machine through the
Docker host gateway.

The script requires `VECTOR_SERVICE_IMAGE_TEST_DISPOSABLE=1` before
performing any DDL (role creation, database creation). This safety gate
prevents accidental execution against a production or shared cluster.

```sh
# Start a disposable PostgreSQL + pgvector cluster (if not already running):
# Run on bridge networking with port mapping so the PostgreSQL port
# (5432 inside the container) is published as 55432 on the host.
docker run -d --name vector-test-pg \
  -e POSTGRES_HOST_AUTH_METHOD=trust \
  -p 55432:5432 \
  pgvector/pgvector:0.8.6-pg18-bookworm

# Run the verification script:
# The script uses bridge networking with --add-host=host.docker.internal:host-gateway
# so verifier containers reach the host-published PostgreSQL port at
# host.docker.internal:55432. PG_HOST defaults to 127.0.0.1 (host-side
# coordinate); PG_PORT defaults to 55432 (the published port).
# VECTOR_SERVICE_IMAGE_TEST_DISPOSABLE=1 acknowledges that the target
# cluster is disposable and will be modified by the script.
VECTOR_SERVICE_IMAGE_TEST_DISPOSABLE=1 \
  bash scripts/verify-image.sh vector-service:local
```

The script performs the following steps:

0. **Provision canonical database** — creates LOGIN roles `vector_owner`
   and `vector_api`, creates database `vector` owned by `vector_owner`,
   installs the `vector` extension.
1. **`migrate` exits 0** — the subcommand completes successfully against
   the test PostgreSQL cluster.
2. **Migration history assertion** — queries `vector_control.schema_migrations` and
   asserts exactly 6 rows (one per released migration).
3. **`serve` healthz/readyz** — the default `serve` command starts,
   `/readyz` returns 200, `/healthz` returns 200.
4. **Bounded shutdown** — `docker stop --time` sends SIGTERM; the script
   requires successful signal delivery, the container to reach `exited`
   state (not removed/gone) before the configured timeout (≤ 40s,
   matching HTTPShutdownGrace 30s + HTTPShutdownCloseTimeout 10s; the
   config enforces that their sum does not exceed 40s),
   and exit code 0. Timing is reported in seconds with millisecond
   precision. Fails if `docker stop` itself fails, the container does not
   reach `exited` state, or the exit code is nonzero.
5a. **Shutdown timeout sum validation** — running with
   `VEC_HTTP_SHUTDOWN_GRACE` and `VEC_HTTP_SHUTDOWN_CLOSE_TIMEOUT` whose
   sum exceeds 40s exits nonzero before any network activity. Logs must
   contain a structured startup error naming both variables and stating
   the sum constraint. The "no listener" guarantee is proven by two tiers:
   (a) structural — config validation (startup step 1) precedes listener
   construction (startup step 5) in `main.go`, and unit tests prove this
   ordering; (b) defense-in-depth — TCP polling on an isolated Docker
   network with a unique published port, evaluated only if the container
   is observed in a Docker "running" state before exit. If the container
   exits before observation (common for fast config failures), the TCP
   check is skipped; the structural guarantees remain authoritative.
6. **Missing config exits nonzero** — running without `VEC_PG_MIGRATION_USER`
   exits nonzero. A disposable sidecar container (`pgvector/pg18` with `sleep`)
   holds a Docker-assigned ephemeral port (`-p 127.0.0.1::8080`) while the
   target service shares its network namespace (`--network container:<sidecar>`).
   The sidecar keeps the port mapping alive even after the target exits quickly
   on config failure, eliminating the race where `docker port` cannot resolve
   the mapping for an already-exited container. The script polls the target
   container state via Docker inspect, attempting TCP connections via
   `/dev/tcp` only when Docker confirms the target is in "running" state
   for that probe iteration. The "no listener" guarantee is proven by two
   tiers: (a) structural — config validation (startup step 1) precedes
   listener construction (startup step 5) in `main.go`, and unit tests
   prove this ordering; (b) defense-in-depth — TCP polling, evaluated only
   if the container is observed in a Docker "running" state before exit. If
   the container exits before observation (common for fast config failures),
   the TCP check is skipped; the structural guarantees remain authoritative.
   The script asserts that the container exits with a nonzero code. Logs are
   captured afterward and must contain a structured `"startup failed"` log
   line with the server-authored variable identifier (e.g.
   `"cause":"config:VEC_PG_MIGRATION_USER"`). The variable name is a
   server-authored identifier, not a raw config value, and is safe to emit.
7. **Image inspection**:
   - ENTRYPOINT references `vector-service`; CMD is `serve`.
   - USER config is non-root.
   - Runtime user effective UID is non-root (resolved from /etc/passwd).
   - Strict filesystem allowlist: the built image's filesystem is compared
     against the pinned distroless base image
     (`gcr.io/distroless/static-debian13:nonroot@sha256:...`). The only
     expected difference is the binary at `/usr/local/bin/vector-service`.
     Any extra files or missing base files cause failure.
   - Filesystem metadata comparison: each base file's type, mode, uid, and
     gid are compared against the image's corresponding entry. A mismatch
     in any field indicates an unexpected modification to base files.
   - Denylist checks (defense in depth): no shell, no Go toolchain,
     no source files, no migrations directory, no test artifacts,
     no secrets, no package managers.
   - Image history inspection: no references to `.go` source files,
     `migrations/`, test artifacts (`.test`, `_test.go`), or secret
     files (`id_rsa`, `.pem`, `.key`, `.p12`, `secret`). If `docker
     history` returns zero layers (e.g., a fully squashed image), the
     check fails closed rather than silently passing, as layer content
     cannot be verified; the filesystem comparison remains authoritative.

**Network configuration:** The PostgreSQL container runs on bridge networking
with `-p 55432:5432` to publish its port on the host. The verification
containers use bridge networking with `--add-host=host.docker.internal:host-gateway`
to reach the host-published PostgreSQL port at `host.docker.internal:55432`.
`PG_HOST` and `PG_PORT` are host-side coordinates (defaults: `127.0.0.1:55432`);
inside containers the database is addressed as `host.docker.internal:<PG_PORT>`.

For Check 3 (missing config) and Check 5a (shutdown sum), a disposable
sidecar container holds the Docker-assigned ephemeral port mapping while
the target service shares its network namespace. The sidecar ensures the
port mapping persists after the target exits, eliminating the race where
`docker port` cannot resolve the mapping for an already-exited container.
The TCP check is defense-in-depth: it is evaluated only if the container
is observed in a Docker "running" state before exit. If the container
exits before observation (common for fast config failures), the TCP check
is skipped; the structural guarantees (nonzero exit code, structured log
assertions) remain authoritative. The startup ordering (config validation
at step 1, listener construction at step 5) is proven by unit tests.

```sh
# Tear down the test cluster:
docker rm -f vector-test-pg
```

---

## Fixture conventions

- **Generic names only** (`AGENTS.md`): applications `memory-service`,
  `notes-service`, `search-service`; namespaces `project-alpha`,
  `project-beta`, `research`. No personal names, no real user content.
- **Deterministic synthetic vectors** for the seeded space (1024 dimensions,
  cosine, migration `0002`): basis/unit vectors per record (nearest-neighbor
  outcomes exact and stable) and scaled variants for distance-ranking tests.
  No real embeddings, no network fetches. `internal/testdb/fixtures.go`
  provides the constructors.
- **Small fixed metadata** (`{"document_type":"note","state":"active"}`
  style) so filter tests are exact.
- **IDs/credentials** are generated via the service's own functions where
  they exist; `fixtures.go` provides the deterministic UUID and ID helpers
  in the meantime. No hardcoded secrets; no raw credential or digest in
  test output (invariant 12).
- **Re-runnable and order-independent**: fresh database per suite; per-test
  data uses distinct `object_id`s; no shared mutable state between test
  functions beyond the suite database.

---

## What the harness does not do

- It does not create application or namespace fixtures through DDL — those
  go through the service's own paths (admin API in HTTP tests, service
  functions in DB tests).
- It does not create or modify vector spaces at runtime — the seeded space
  is migration `0002`'s, and new spaces enter only through new migrations.
- It does not build alternate provisioning shapes or converge a
  negative-identity variant.
- It is under `internal/` so production code can never import it.
