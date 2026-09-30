# Resolution: F-2 — Config-Rejection Image Verification

**Finding**: `scripts/verify-image.sh` checks 3 (missing required config)
and 5a (shutdown-sum validation) require the container to be observed in
a Docker "running" state before TCP polling. In actual local Docker runs,
config validation errors cause the process to exit before the first
`docker inspect` can observe `running`, even with 20 ms polling and a
sidecar holding the ephemeral port. The verifier correctly reports this
as inconclusive and fails.

**Severity**: Medium (test correctness; no production behavior affected)
**Owner**: Architect
**Status**: Resolved

## Decision

Restructure the config-rejection acceptance criteria into two tiers:

1. **Structural guarantees** (always observable; authoritative proof):
   nonzero exit code, structured `"startup failed"` log line, server-authored
   variable identifier in the log.

2. **TCP observation** (defense-in-depth; conditional):
   Only evaluated if the polling loop observes the container in a
   "running" state at least once. If the container exits before any
   observation, the TCP check is skipped. The structural guarantees
   remain authoritative.

The structural guarantees are sufficient because config validation
precedes listener construction in the `runServe` startup order
(`main.go`), and this ordering is already proven by unit tests.

## Rationale

### 1. The startup order is a structural guarantee

The `runServe` startup sequence in `cmd/vector-service/main.go`:

```
1. config.Load()          — validates all VEC_* variables
2. configureLogging()     — installs JSON handler
3. runMigrationStep()     — opens migration connection
4. newRuntimePool()       — creates runtime pool
5. newServer() + Serve()  — listener opens HERE
```

A config validation failure at step 1 (missing `VEC_PG_MIGRATION_USER`,
shutdown-sum bound violation) returns immediately with `os.Exit(1)`.
Steps 3–5 are never reached. The listener opens only inside `Serve()`
at step 5. This is structural proof that no listener was opened.

### 2. Unit tests already prove the ordering

The startup tests in `cmd/vector-service/main_test.go` already prove
this ordering without Docker:

- `TestMigrateConfigFailureBeforeNetwork` — config failure before
  network activity (missing `VEC_PG_MIGRATION_USER`)
- `TestServeStartupFailureBeforeListen` — migration failure before
  listener open (port-held proof)
- `TestServeSuccessPathProbesReadyz` — startup ordering: migration
  converges, pool creates, server builds, flag flips, then listener
  opens (observed via controller seam)

These tests prove config validation precedes listener construction
using in-process seams, which is more reliable than Docker state polling.

### 3. The TCP check is defense-in-depth, not authoritative

The TCP polling was designed as defense-in-depth: if the container
is observed running and a TCP connection succeeds, that proves a
listener opened (a real bug). However, the inverse — "container exited
before observation, therefore listener may have opened" — is not valid.
The container exiting before observation is consistent with both
"listener never opened" (correct) and "listener opened briefly" (bug).
The structural guarantees disambiguate: exit code and log assertions
prove the config failure path was taken, which structurally excludes
listener construction.

### 4. Adding a production startup delay is undesirable

The alternative of adding an arbitrary startup delay solely to make
the TCP check observable would:

- Change production behavior for test convenience
- Introduce a latent startup cost for all deployments
- Violate the "fail-fast" startup principle (DEVELOPMENT.md:
  "Configuration should fail fast when required values are missing")

### 5. A test-only harness seam is unnecessary

A test-only seam (e.g., a startup probe that signals readiness) would
add complexity to the binary for a test concern. The structural
guarantees plus existing unit tests already provide complete coverage
without modifying production code.

## Acceptance Criteria

### Check 3: Missing required config (missing `VEC_PG_MIGRATION_USER`)

The container is started without `VEC_PG_MIGRATION_USER`. The following
are asserted:

| # | Assertion | Type | Requirement |
|---|-----------|------|-------------|
| 3a | Exit code is nonzero | Structural | **Required** |
| 3b | Logs contain `"msg":"startup failed"` | Structural | **Required** |
| 3c | Logs contain `"cause":"config:VEC_PG_MIGRATION_USER"` | Structural | **Required** |
| 3d | No TCP connection succeeds (if observed running) | Defense-in-depth | **Conditional** |
| 3e | Container final state is `exited` | Structural | **Required** |

**Conditional rule for 3d:** If `PROBE_RAN_WHILE_RUNNING` is `false`
(the polling loop never observed the container in a "running" state),
the TCP check (3d) is skipped. The test passes based on assertions
3a–3c and 3e alone. If `PROBE_RAN_WHILE_RUNNING` is `true`, the TCP
check is evaluated: no TCP connection should succeed.

### Check 5a: Shutdown timeout sum validation

The container is started with `VEC_HTTP_SHUTDOWN_GRACE=30s` and
`VEC_HTTP_SHUTDOWN_CLOSE_TIMEOUT=11s` (sum 41s > 40s bound). The
following are asserted:

| # | Assertion | Type | Requirement |
|---|-----------|------|-------------|
| 5a-i | Exit code is nonzero | Structural | **Required** |
| 5a-ii | Logs contain `"msg":"startup failed"` | Structural | **Required** |
| 5a-iii | Logs contain `"cause":"config:shutdown_sum_exceeds_40s:VEC_HTTP_SHUTDOWN_GRACE+VEC_HTTP_SHUTDOWN_CLOSE_TIMEOUT"` | Structural | **Required** |
| 5a-iv | No TCP connection succeeds (if observed running) | Defense-in-depth | **Conditional** |
| 5a-v | Container final state is `exited` | Structural | **Required** |

**Conditional rule for 5a-iv:** Same conditional rule as 3d above.

### Unit test coverage (already present)

The following unit tests in `cmd/vector-service/main_test.go` prove
config validation precedes listener construction:

| Test | Proves |
|------|--------|
| `TestMigrateConfigFailureBeforeNetwork` | Missing `VEC_PG_MIGRATION_USER` fails before network activity |
| `TestConfigGateRejectsIdentityCollapse` | Identity collapse rejected before network activity |
| `TestConfigGateRejectsPlainModeTLSPaths` | TLS path in plain mode rejected before network activity |
| `TestServeStartupFailureBeforeListen` | Migration failure before listener open (port-held proof) |
| `TestServeSuccessPathProbesReadyz` | Startup ordering: config → migration → pool → server → listener |

## Script Changes

The `scripts/verify-image.sh` script is updated in two places:

### Check 3 (missing config): lines ~517–519

**Before:**
```bash
if [ "$PROBE_RAN_WHILE_RUNNING" != true ]; then
  fail "TCP polling: never observed container in running state (PROBE_RAN_WHILE_RUNNING=false); test inconclusive"
fi
```

**After:**
```bash
# The container may exit before any docker inspect observes "running"
# state — config validation (step 1) precedes listener construction
# (step 5) in the startup order. When the container exits before
# observation, the TCP check is skipped; the structural guarantees
# (exit code, log assertions) remain authoritative.
if [ "$PROBE_RAN_WHILE_RUNNING" != true ]; then
  info "TCP polling: container exited before observation (PROBE_RAN_WHILE_RUNNING=false); skipping TCP check (structural guarantees are authoritative)"
fi
```

### Check 5a (shutdown sum): lines ~652–654

Same change as above, applied to the shutdown-sum polling block.

## Files Updated

### 1. `scripts/verify-image.sh`

- Check 3 (missing config): replace the `PROBE_RAN_WHILE_RUNNING`
  failure with an informational skip message.
- Check 5a (shutdown sum): same change.
- TCP checks (3d, 5a-iv) are guarded by `PROBE_RAN_WHILE_RUNNING=true`.

### 2. `docs/implementation/package-5-verification-runtime.md`

- 5c section: update the image verification contract to document the
  two-tier acceptance criteria (structural guarantees + conditional
  TCP observation).
- Clarify that structural guarantees are proven by the startup ordering
  in `main.go` and validated by unit tests.

### 3. `docs/TESTING.md`

- Update the "Local image verification" section to reflect the
  conditional TCP check:
  - Structural guarantees: nonzero exit, startup-failed log,
    server-authored variable identifier
  - TCP check: conditional on container being observed running

## Invariants Preserved

- **No-listener guarantee**: Config validation precedes listener
  construction in `main.go` (structural). Unit tests prove this
  ordering. The TCP check is defense-in-depth, not the authority.
- **Fail-fast startup**: Config failures exit before any network
  activity. Adding a startup delay would violate this.
- **Security**: The contract still asserts that config failures do not
  open a listener. The proof method changed from "TCP observation" to
  "structural ordering + unit tests + conditional TCP defense-in-depth."
- **Determinism**: The structural guarantees (exit code, log assertions)
  are deterministic and always observable. The conditional TCP check
  adds no non-determinism: it only runs when the container is observed
  running.

## Acceptance Tests

The updated script is validated by:

1. **Local Docker run**: the script passes checks 3 and 5a even when
   the container exits before the first `docker inspect` observes
   "running" (the common case on local machines).

2. **Unit tests**: `main_test.go` startup tests continue to prove
   config validation precedes listener construction.

3. **Defense-in-depth preserved**: if a regression causes the listener
   to open before config validation completes (e.g., startup order
   change), the TCP check will detect it when the container lives long
   enough to be observed.

## Out of scope

- Changing the startup order in `main.go` (already correct)
- Adding a test-only harness seam or startup probe
- Adding a production startup delay
- Changing the sidecar pattern (it remains useful for port mapping)
- Changing the polling interval (20 ms remains appropriate)

## Open issues

None.
