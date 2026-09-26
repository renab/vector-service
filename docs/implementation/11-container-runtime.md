# Unit 11 — Container Image and Runtime Deployment Shape

## Objective

Define the container image and the runtime deployment shape for the
`vector-service` binary: multi-stage Docker build, runtime image
contents, entrypoint behavior, subcommand usage in containers, and which
C12 provisioning option each deployment shape assumes.

This unit is documentation + the `Dockerfile`/build inputs only. It adds
no Go code beyond what units 01–10 already require.

## Authority

- `docs/DEVELOPMENT.md` — "Docker Build": prefer multi-stage
  (Go builder → compiled binary → minimal runtime image); the runtime
  image should contain only what is needed to run the binary,
  establish TLS connections, and expose health endpoints; no source
  code or build tooling unless required; **run as a non-root user**.
  "Commands": `vector-service serve` / `vector-service migrate`
  subcommands, whichever pattern is selected must remain deterministic
  and documented; no two independent migration runners.
- `docs/MIGRATIONS.md` — startup order (load config → connect → validate
  prerequisites → run/validate migrations → start serving → readyz);
  "applied before code requiring the new schema becomes ready";
  schema preconditions (database, pgvector binaries, runtime
  connection identity, TLS connectivity are infrastructure-provided).
- `docs/SECURITY.md` — TLS client certificates for PostgreSQL; never
  ship or log private keys; administrative endpoints on trusted internal
  networks.
- `docs/implementation/01-foundation.md` — the single binary, the
  `serve`/`migrate` subcommands, fail-fast config, graceful shutdown.
- `docs/implementation/03-embedded-migrations.md` — migrations are
  **embedded** in the binary (no external migration files needed in the
  image); the runner's C12 bootstrap/behavior.
- `docs/implementation/README.md` — configuration reference (every
  `VEC_*` variable is the container's configuration surface); C2/C3/C4/
  C5/C12.

## Dependencies

- Unit 01 (binary + subcommands + config).
- Unit 03 (embedded migrations — the image carries no migration files).
- Units 02, 09 (TLS client configuration, graceful shutdown — the
  runtime behavior the container relies on).

## Scope

### Multi-stage Dockerfile

```text
stage 1: builder
  FROM golang:<pinned-version>          # matches go.mod (unit 01)
  COPY go.mod go.sum → go mod download  (layer-cached)
  COPY source → CGO_ENABLED=0 go build -o /out/vector-service ./cmd/vector-service
  # static binary; no runtime CGO dependency

stage 2: runtime
  FROM gcr.io/distroless/static:<pinned-version>   # proposal; review point
  COPY --from=builder /out/vector-service /usr/local/bin/vector-service
  USER vector-service (non-root UID, e.g. 65532)
  ENTRYPOINT ["/usr/local/bin/vector-service"]
  CMD ["serve"]
```

Runtime image content constraints (DEVELOPMENT.md):

- **Only** the compiled binary (plus the OS-provided dynamic loader if
  the base image provides one; a static Go binary on distroless/static
  needs nothing else).
- TLS connections: the Go binary uses its own crypto; TLS **client
  certificates for PostgreSQL are mounted at runtime** (files or a
  projected volume), never baked into the image (SECURITY.md — private
  keys must not be persisted in image layers). If the chosen base lacks
  a CA store needed for anything else, that is the one exception to
  "only what is needed" — with distroless/static there is nothing
  else, so no store is needed.
- Health endpoints: nothing extra required (the process itself serves
  `/healthz`, `/readyz`).
- **No** source, no `go` toolchain, no package manager, no shell (a
  shell-less image is the default; if an operator insists on `sh` for
  debugging, that is a deliberate, documented deviation — review point).
- No migration files: unit 03 embeds them; the image must not depend on
  a filesystem migration path (MIGRATIONS.md "Embedded Migrations").
- No test binaries, no `testinfra` artifacts.
- Base image version is pinned (tag, not `latest`) and updated
  deliberately, like any dependency.

### Entrypoint and subcommands in the container

- `CMD ["serve"]`: the container's default lifecycle. `serve` performs
  the full startup order (config → connect → validate prerequisites
  (pgvector present per C3, role per C5, database name per C4) →
  run/validate migrations (embedded set, unit 03) → start serving →
  readyz). A pod that starts and dies before serving is a **configuration
  or prerequisite failure** and must surface a non-zero exit with the
  structured startup error on stderr — the operator reads it from
  container logs (SECURITY.md's startup preconditions are fail-fast,
  unit 01/03).
- `migrate` is available for explicit one-shot migration jobs (e.g. a
  pre-rollout Job that converges schema before new replicas start).
  Both subcommands use the **same** embedded runner (unit 01/03;
  "Do not implement two independent migration runners").
- Determinism: `serve` never skips the migration step; a deployment that
  wants "migrate only" uses the `migrate` subcommand, not a flag.
- Shutdown: SIGTERM → graceful drain (unit 01: 30 s grace) → exit 0.
  Container runtimes send SIGTERM; the binary must not require SIGKILL
  for a clean stop within the grace period.

### Configuration surface in the container

- All runtime configuration is environment variables (`VEC_*`, README
  reference) — 12-factor style; no config files in the image.
  Secrets (admin token, TLS client key paths) arrive as env references
  to mounted/secret-provisioned material, never as image content.
- `VEC_LISTEN_ADDR` is the in-container listen address (typically
  `0.0.0.0:<port>`); TLS termination for HTTP is **external** (reverse
  proxy / LB) per unit 09 — the container exposes plain HTTP on its
  listen port and PostgreSQL TLS client auth per the `VEC_PG_*` triple.
- The container must run identically with `serve` (migrates-then-serves)
  or behind a `migrate` job; there is no third mode.

### Deployment shapes and C12

The image is provisioning-agnostic: the unit 03 runner bootstraps
deterministically (no probing) and behaves per C12. Each supported shape:

| shape | who creates `vector_control`/`vector_data` + `schema_migrations` | how |
|-------|---------------------------------------------------------------|-----|
| **A. distinct migration identity** (recommended production, C12 option 1) | a privileged role supplied via `VEC_PG_MIGRATION_USER` | the runner connects to that identity for the migration step only; `vector_api` (`VEC_PG_USER`) never holds creation rights |
| **B. pre-provisioned** (C12 option 2) | infrastructure (init system / database operator), **by or as the configured migration identity** | pre-provisioning must give the migration identity everything needed to apply `0001`–`0004` as that identity (`CREATE` on the pre-created schemas, full access to the history table, the grant authority `0004` requires — database ownership, or `CREATE` on the database plus `CONNECT … WITH GRANT OPTION`); the runner then applies pending migrations under the migration identity as in shape A. Pre-provisioning with only partial privileges (e.g. `CREATE` without `CONNECT … WITH GRANT OPTION`) is **not** a supported shape (unit 03, C12) |
| **C. explicit grants** (C12 option 3; the test-harness default per unit 10) | the configured migration identity | infrastructure grants the **migration identity** **both** `CREATE ON DATABASE vector` (so the bootstrap can create the schemas + history table) **and** `CONNECT ON DATABASE vector … WITH GRANT OPTION` (so `0004` can execute `GRANT CONNECT ON DATABASE vector TO vector_api`; a bare `CREATE` grant cannot execute `0004` and is not a supported shape); `vector_api` is never granted `CREATE` |

- The container does not encode a preference; it runs whichever shape
  the environment implements and fails actionably (naming the missing
  privilege) when none does (unit 03).
- The **default deployment shape is an operator decision** (C12 review
  point); the recommended shape for production is A.

### Image build/verification (contract level)

- `docker build` reproduces the binary from `go.mod`/`go.sum` pins
  (same Go version as unit 01).
- Verification commands after build (CI):
  - `docker run --rm <image> migrate` (the subcommand is appended to
    the `ENTRYPOINT`, per the Dockerfile sketch) against a disposable
    PostgreSQL (unit 10 environment) → exit 0, history populated;
  - `docker run --rm <image>` (i.e. `serve`) against the same database
    → `/readyz` 200 after startup; `/healthz` 200;
  - `docker run --rm <image>` with a missing required `VEC_*` variable
    → non-zero exit, structured error naming the variable, no port
    ever listening;
  - image inspection: no shell, no `go`, no `migrations/` directory,
    single non-root user, no root-owned secrets in any layer.

## Interfaces and boundaries

- The image's only interface is the binary's: subcommands, `VEC_*`
  environment, the HTTP listen port, and the PostgreSQL connection.
- Consumes: unit 01 (binary/config), unit 03 (embedded migrations),
  unit 10 (the disposable-DB verification flow).
- Exposes: one image, one binary, two subcommands. No sidecars.

## Invariants and correctness constraints

- **Non-root** runtime user (DEVELOPMENT.md).
- **No secrets in the image**: no TLS private keys, no admin token, no
  credentials in any layer; keys are mounted at runtime (SECURITY.md).
- **Embedded migrations only**: the image must work with no filesystem
  access to `migrations/` (MIGRATIONS.md).
- **Single runner**: `serve` and `migrate` share the unit 03 runner
  (DEVELOPMENT.md).
- **Fail-fast startup**: a container with unmet preconditions (C3
  extension, C4 database name, C5 role, C12 provisioning) exits
  non-zero with an actionable structured error; it never serves
  half-ready (MIGRATIONS.md startup behavior, unit 01/03).
- **Forward-only**: the image never attempts down-migrations or schema
  repair; corrective changes are new migrations (MIGRATIONS.md).
- **Reproducible**: pinned Go version, pinned base image,
  `go.sum`-pinned dependencies.

## Expected implementation surface

```text
Dockerfile                  # two stages, as sketched
.dockerignore               # exclude docs, tests, .git from build context
deploy notes (in this file) # shapes A/B/C, C12 mapping
```

No Go code. (If the implementer wants an image-label convention or a
`Makefile` build target, those are routine and left to them.)

## Validation

- The four image verification commands above pass in CI.
- `serve` in a container against a fresh (correctly provisioned)
  database reaches `/readyz` 200 and handles a full
  authenticate → upsert → search round-trip (unit 10 HTTP integration
  flow, pointed at the container).
- SIGTERM to `serve` → clean exit 0 within the 30 s grace, in-flight
  request drained.
- Image audit: single non-root user, no root-owned files containing key
  material, no shell/toolchain/source.

## Out of scope

- Kubernetes/Helm manifests, service mesh, service discovery, and
  autoscaling policy (deployment-infrastructure boundary per
  `docs/MIGRATIONS.md` ownership split).
- In-process HTTP TLS termination (external proxy, unit 09).
- Multi-architecture (e.g. `arm64`) build matrices — adopt if the
  deployment requires it; the Dockerfile shape is unchanged.
- Image signing/attestation policy (operator decision).
- Any second container or sidecar in the service deployment.

## Open issues

- **Base image choice** (`gcr.io/distroless/static` proposed) and its
  pinning policy — review point.
- **Shell-less vs. debug shell** in the runtime image — default is
  shell-less; a debug shell is a deliberate deviation requiring a note.
  Review point.
- **Default C12 provisioning shape in production** (A recommended) —
  inherited from C12; the container supports all three and prefers none.
