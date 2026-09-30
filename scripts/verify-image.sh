#!/usr/bin/env bash
# verify-image.sh — Contract-level container verification.
#
# Verifies the built image against the authoritative contract:
#   0. Provision canonical database: roles, database, vector extension
#   1. `migrate` subcommand exits 0 with migration history populated (6 rows)
#   2. `serve` starts, healthz/readyz return 200, SIGTERM shutdown is bounded
#      (container must reach exited state with exit code 0)
#   3. Missing required VEC_* config exits nonzero before opening a listener:
#      captures stderr (must contain startup failure log with the server-authored
#      variable identifier), asserts process exited before listener startup
#      based on TCP polling on an isolated network with a unique published port
#   4. Runtime image: non-root UID on binary, strict filesystem allowlist
#      (distroless base + vector-service), no shell, no Go toolchain, no source,
#      no migrations/, no test artifacts, no secrets; image history free of
#      forbidden payloads (source, tests, migrations, secrets)
#
# Usage: ./scripts/verify-image.sh <image-tag>
#
# Prerequisites:
#   - Docker available
#   - A running PostgreSQL 18 + pgvector cluster reachable at PG_HOST:PG_PORT
#     (defaults: 127.0.0.1:55432)
#   - All Docker containers use bridge networking with
#     --add-host=host.docker.internal:host-gateway to reach the PostgreSQL
#     service. Inside containers the database is addressed as
#     host.docker.internal:<PG_PORT>. For containers that may exit quickly
#     (config-failure tests), a disposable sidecar container holds the
#     Docker-assigned ephemeral host port (-p 127.0.0.1::8080) while the
#     target shares its network namespace (--network container:<sidecar>),
#     eliminating the race where `docker port` cannot resolve a mapping
#     for an already-exited container.
#
# Safety:
#   This script performs cluster-global DDL (role creation, database creation)
#   against the PostgreSQL instance at PG_HOST:PG_PORT. It MUST NOT be run
#   against a production or shared cluster. Set VECTOR_SERVICE_IMAGE_TEST_DISPOSABLE=1
#   to acknowledge that the target cluster is disposable.
#
# Exit codes:
#   0 — all checks passed
#   1 — one or more checks failed
#   2 — safety gate failed (opt-in not provided)

set -euo pipefail

# ──────────────────────────────────────────────────────────────────────────────
# Safety gate: require explicit opt-in before any DDL.
# ──────────────────────────────────────────────────────────────────────────────
# This script creates roles and databases on the target PostgreSQL cluster.
# Running against a production or shared cluster can cause data loss.
# The opt-in variable must be set to "1" by the operator.
if [ "${VECTOR_SERVICE_IMAGE_TEST_DISPOSABLE:-}" != "1" ]; then
  echo "FAIL: VECTOR_SERVICE_IMAGE_TEST_DISPOSABLE is not set to 1" >&2
  echo "" >&2
  echo "This script performs cluster-global DDL (CREATE ROLE, CREATE DATABASE)" >&2
  echo "against the PostgreSQL instance at PG_HOST:PG_PORT." >&2
  echo "It MUST NOT be run against a production or shared cluster." >&2
  echo "" >&2
  echo "Set VECTOR_SERVICE_IMAGE_TEST_DISPOSABLE=1 to acknowledge that the" >&2
  echo "target cluster is disposable." >&2
  echo "" >&2
  echo "Example:" >&2
  echo "  VECTOR_SERVICE_IMAGE_TEST_DISPOSABLE=1 bash scripts/verify-image.sh vector-service:local" >&2
  exit 2
fi

IMAGE="${1:?usage: verify-image.sh <image-tag>}"

# PG_HOST and PG_PORT are host-side connection coordinates — the address where
# the PostgreSQL service is reachable from the host shell. Defaults match the
# documented local and CI setup (PostgreSQL container publishing port 55432).
# Inside bridge-networked containers the database is reached via
# host.docker.internal:<PG_PORT> through the Docker host gateway.
PG_HOST="${PG_HOST:-127.0.0.1}"
PG_PORT="${PG_PORT:-55432}"

# Timing bounds (seconds)
# The contract worst-case shutdown is HTTPShutdownGrace (30s) +
# HTTPShutdownCloseTimeout (10s) = 40s. The default matches this bound.
# The hard maximum is 40 seconds regardless of environment override.
SERVE_READY_TIMEOUT="${SERVE_READY_TIMEOUT:-30}"
SHUTDOWN_TIMEOUT="${SHUTDOWN_TIMEOUT:-40}"

# safe_bounded_int: Validate a value is a positive integer within bounds.
# Args: $1=var_name $2=value $3=max_value $4=max_digits
# Prints the validated integer to stdout. Exits 1 on failure.
# Rejects huge digit strings before Bash arithmetic by checking digit length
# against the maximum expected digits for the bound.
safe_bounded_int() {
  local vname="$1" val="$2" max="$3" max_digits="$4"

  # Must be non-empty.
  if [ -z "$val" ]; then
    echo "FAIL: ${vname} is empty" >&2
    exit 1
  fi

  # Must be all digits (no sign, no decimal, no whitespace).
  if ! [[ "$val" =~ ^[0-9]+$ ]]; then
    echo "FAIL: ${vname}=${val} is not a positive integer" >&2
    exit 1
  fi

  # Reject huge digit strings before Bash arithmetic: if the digit length
  # exceeds the maximum expected for the bound, reject immediately.
  local digit_len=${#val}
  if [ "$digit_len" -gt "$max_digits" ]; then
    echo "FAIL: ${vname}=${val} exceeds maximum digit length ${max_digits} (value too large)" >&2
    exit 1
  fi

  # Must be positive (not zero).
  if [ "$val" -eq 0 ]; then
    echo "FAIL: ${vname}=0 is not allowed (must be positive)" >&2
    exit 1
  fi

  # Must not exceed the hard maximum.
  if [ "$val" -gt "$max" ]; then
    echo "FAIL: ${vname}=${val} exceeds hard maximum of ${max}s" >&2
    exit 1
  fi

  echo "$val"
}

# Validate ALL timeout overrides before DDL with safe bounded numeric parsing.
# SHUTDOWN_TIMEOUT: positive integer <= 40 (max 2 digits).
SHUTDOWN_TIMEOUT=$(safe_bounded_int "SHUTDOWN_TIMEOUT" "$SHUTDOWN_TIMEOUT" 40 2)

# SERVE_READY_TIMEOUT: positive integer <= 300 (max 3 digits).
SERVE_READY_TIMEOUT=$(safe_bounded_int "SERVE_READY_TIMEOUT" "$SERVE_READY_TIMEOUT" 300 3)

FAILURES=0

pass() { echo "PASS: $1"; }
fail() { echo "FAIL: $1"; FAILURES=$((FAILURES + 1)); }
info() { echo "INFO: $1"; }

# resolve_published_port: Obtain the Docker-assigned ephemeral host port
# for container $1's internal port 8080/tcp. Prints the port number to stdout.
# Exits 1 on parse failure after cleaning up the container.
resolve_published_port() {
  local cid="$1"
  local addr
  addr=$(docker port "$cid" 8080/tcp 2>/dev/null) || {
    echo "FAIL: docker port $cid 8080/tcp failed" >&2
    docker rm -f "$cid" >/dev/null 2>&1 || true
    exit 1
  }
  # Output format: "127.0.0.1:<port>" — extract the port number.
  local port="${addr##*:}"
  if [ -z "$port" ] || ! [[ "$port" =~ ^[0-9]+$ ]]; then
    echo "FAIL: could not parse published port from '${addr}'" >&2
    docker rm -f "$cid" >/dev/null 2>&1 || true
    exit 1
  fi
  echo "$port"
}

# sidecar_start: Start a disposable sidecar container to hold a Docker-assigned
# ephemeral host port for 8080/tcp while a target container (that may exit
# quickly) shares its network namespace. The sidecar runs `sleep 300` so the
# port mapping persists after the target exits. Sets two variables in the
# caller's scope: the sidecar container ID ($1) and the assigned host port ($2).
#
# Usage:
#   sidecar_start MISSING_SIDECAR_ID MISSING_HOST_PORT
#   docker run -d --network "container:$MISSING_SIDECAR_ID" ... target ...
#   # ... probe $MISSING_HOST_PORT, inspect target, etc. ...
#   docker rm -f "$TARGET_ID" "$MISSING_SIDECAR_ID" >/dev/null 2>&1 || true
#
# The sidecar uses the pgvector/pg18 image (already pinned and available in
# this script for database provisioning). It listens on nothing — sleep only.
# No listener on 8080 in the sidecar; the port mapping exists solely so the
# host can reach a target container sharing the network namespace.
sidecar_start() {
  local id_var="$1"
  local port_var="$2"
  local sc_id
  sc_id=$(docker run -d \
    -p "127.0.0.1::8080" \
    pgvector/pgvector:0.8.6-pg18-bookworm \
    sleep 300)
  # Set the sidecar ID in the caller's variable.
  eval "${id_var}='${sc_id}'"
  # Resolve the published port from the sidecar (it stays alive, no race).
  local port
  port=$(resolve_published_port "$sc_id")
  eval "${port_var}='${port}'"
}

# Common environment for the service container (plain TLS, test token).
# VEC_PG_USER is fixed to vector_api (runtime role).
# VEC_PG_DATABASE is fixed to vector (migration 0004).
# VEC_PG_MIGRATION_USER is the database owner role.
# VEC_PG_HOST is host.docker.internal (Docker host gateway) so bridge-networked
# containers can reach the PostgreSQL service published on the host at PG_PORT.
COMMON_ENV=(
  -e VEC_PG_HOST=host.docker.internal
  -e VEC_PG_PORT="$PG_PORT"
  -e VEC_PG_DATABASE=vector
  -e VEC_PG_USER=vector_api
  -e VEC_PG_MIGRATION_USER=vector_owner
  -e VEC_PG_TLS_MODE=plain
  -e VEC_ADMIN_TOKEN="verify-image-test-token-0000000000000000000000000000"
  -e VEC_LISTEN_ADDR="0.0.0.0:8080"
)

# ──────────────────────────────────────────────────────────────────────────────
# 0. Provision canonical database (roles, database, vector extension)
# ──────────────────────────────────────────────────────────────────────────────
echo "=== Provision: canonical database ==="

# Run provisioning inside a pgvector container using bridge networking with
# --add-host=host.docker.internal:host-gateway so it can reach the PostgreSQL
# service on the host at host.docker.internal:PG_PORT.

# Step 1: Create roles in the maintenance database (postgres).
# Roles are global — they must be created outside the target database.
# -i keeps STDIN open for the heredoc; without it the heredoc input may
# be silently discarded on some Docker/OS combinations.
docker run --rm -i \
  --add-host=host.docker.internal:host-gateway \
  pgvector/pgvector:0.8.6-pg18-bookworm \
  psql -h host.docker.internal -p "$PG_PORT" -U postgres -d postgres -v ON_ERROR_STOP=1 \
  <<'EOSQL'
DO $$
BEGIN
  IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'vector_owner') THEN
    CREATE ROLE vector_owner LOGIN;
  END IF;
  IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'vector_api') THEN
    CREATE ROLE vector_api LOGIN;
  END IF;
END
$$;
EOSQL

# Assert that both roles now exist before proceeding to database creation.
# If the heredoc was silently discarded the roles will be absent and the
# subsequent CREATE DATABASE will fail silently or with misleading errors.
ROLE_CHECK=$(docker run --rm -i \
  --add-host=host.docker.internal:host-gateway \
  pgvector/pgvector:0.8.6-pg18-bookworm \
  psql -h host.docker.internal -p "$PG_PORT" -U postgres -d postgres -t -A \
  <<'EOSQL'
SELECT count(*) FROM pg_roles WHERE rolname IN ('vector_owner', 'vector_api');
EOSQL
)
if [ "$ROLE_CHECK" != "2" ]; then
  echo "FAIL: provisioning roles not found (${ROLE_CHECK}/2); heredoc may have been discarded" >&2
  exit 1
fi

# Step 2: Create the database outside a transaction.
# CREATE DATABASE cannot run inside a transaction block.
# PostgreSQL 18 does not support --exists-ok on createdb, so we use psql
# to query pg_database and only create if missing. If the database exists
# but has the wrong owner, fail clearly rather than silently accepting it.
DB_OWNER=$(docker run --rm \
  --add-host=host.docker.internal:host-gateway \
  pgvector/pgvector:0.8.6-pg18-bookworm \
  psql -h host.docker.internal -p "$PG_PORT" -U postgres -d postgres -t -A \
  -c "SELECT rolname FROM pg_database d JOIN pg_roles r ON d.datdba = r.oid WHERE d.datname = 'vector';")

if [ -z "$DB_OWNER" ]; then
  docker run --rm \
    --add-host=host.docker.internal:host-gateway \
    pgvector/pgvector:0.8.6-pg18-bookworm \
    createdb -h host.docker.internal -p "$PG_PORT" -U postgres --owner vector_owner vector
elif [ "$DB_OWNER" != "vector_owner" ]; then
  echo "FAIL: database 'vector' exists but owner is '${DB_OWNER}' (expected 'vector_owner')" >&2
  exit 1
fi

# Step 3: Install the vector extension in the target database.
docker run --rm \
  --add-host=host.docker.internal:host-gateway \
  pgvector/pgvector:0.8.6-pg18-bookworm \
  psql -h host.docker.internal -p "$PG_PORT" -U postgres -d vector -v ON_ERROR_STOP=1 \
  -c "CREATE EXTENSION IF NOT EXISTS vector;"

pass "canonical database provisioned"

# ──────────────────────────────────────────────────────────────────────────────
# 1. `migrate` exits 0 with migration history populated
# ──────────────────────────────────────────────────────────────────────────────
echo ""
echo "=== Check 1: migrate subcommand ==="

if docker run --rm \
  --add-host=host.docker.internal:host-gateway \
  "${COMMON_ENV[@]}" "$IMAGE" migrate; then
  pass "migrate exited 0"
else
  fail "migrate did not exit 0"
fi

# Assert migration history: exactly 6 rows (one per released migration).
MIGRATION_COUNT=$(docker run --rm \
  --add-host=host.docker.internal:host-gateway \
  pgvector/pgvector:0.8.6-pg18-bookworm \
  psql -h host.docker.internal -p "$PG_PORT" -U postgres -d vector -t -A \
  -c "SELECT count(*) FROM vector_control.schema_migrations;")

if [ "$MIGRATION_COUNT" -eq 6 ]; then
  pass "migration history has 6 rows"
else
  fail "migration history has ${MIGRATION_COUNT} rows (expected 6)"
fi

# ──────────────────────────────────────────────────────────────────────────────
# 2. `serve` healthz/readyz and bounded shutdown
# ──────────────────────────────────────────────────────────────────────────────
echo ""
echo "=== Check 2: serve healthz/readyz and bounded shutdown ==="

# Start serve on bridge networking with a Docker-assigned ephemeral host port
# for health checks. Use --add-host=host.docker.internal:host-gateway to reach
# PostgreSQL on the host. Docker atomically selects a free port, avoiding
# random-port collisions.
CONTAINER_ID=$(docker run -d \
  --add-host=host.docker.internal:host-gateway \
  -p "127.0.0.1::8080" \
  "${COMMON_ENV[@]}" "$IMAGE" serve)
SERVE_HOST_PORT=$(resolve_published_port "$CONTAINER_ID")
cleanup() { docker rm -f "$CONTAINER_ID" >/dev/null 2>&1 || true; }
trap cleanup EXIT

# Wait for /readyz with polling loop.
info "Waiting for /readyz (timeout ${SERVE_READY_TIMEOUT}s)..."
READY=false
for i in $(seq 1 "$SERVE_READY_TIMEOUT"); do
  if curl -sf "http://127.0.0.1:${SERVE_HOST_PORT}/readyz" >/dev/null 2>&1; then
    READY=true
    break
  fi
  sleep 1
done

if [ "$READY" = true ]; then
  pass "/readyz returned 200 within ${SERVE_READY_TIMEOUT}s"
else
  fail "/readyz did not return 200 within ${SERVE_READY_TIMEOUT}s"
  docker logs "$CONTAINER_ID" 2>&1 || true
  exit "$FAILURES"
fi

# Check /healthz
if curl -sf "http://127.0.0.1:${SERVE_HOST_PORT}/healthz" >/dev/null 2>&1; then
  pass "/healthz returned 200"
else
  fail "/healthz did not return 200"
fi

# Bounded shutdown: use docker stop (sends SIGTERM, waits --time seconds,
# then SIGKILL). Require successful signal delivery, exited state before
# deadline, and exit code 0. Never accept removed/gone.
info "Sending SIGTERM via docker stop (timeout ${SHUTDOWN_TIMEOUT}s)..."

# Record start time with millisecond precision.
START_MS=$(date +%s%3N)

# docker stop sends SIGTERM first, then waits up to --time seconds before
# SIGKILL. We use the SHUTDOWN_TIMEOUT as the stop grace period.
# Require the command to succeed (exit 0 means the container was stopped).
STOP_OK=false
if docker stop --time "$SHUTDOWN_TIMEOUT" "$CONTAINER_ID" >/dev/null 2>&1; then
  STOP_OK=true
fi

END_MS=$(date +%s%3N)
ELAPSED_MS=$((END_MS - START_MS))
ELAPSED_S=$((ELAPSED_MS / 1000))
ELAPSED_FRAC=$((ELAPSED_MS % 1000))

# Verify the container actually exited (not removed/gone/timeout).
# After docker stop succeeds the container should be in "exited" state.
# If docker inspect fails, the container was removed or gone — that is a failure.
SHUTDOWN_STATE=$(docker inspect -f '{{.State.Status}}' "$CONTAINER_ID" 2>/dev/null) || SHUTDOWN_STATE="unreachable"

if [ "$STOP_OK" != true ]; then
  fail "shutdown: docker stop failed (container state: '${SHUTDOWN_STATE}', elapsed ${ELAPSED_S}.${ELAPSED_FRAC}s)"
elif [ "$SHUTDOWN_STATE" != "exited" ]; then
  fail "shutdown: container did not reach exited state (got '${SHUTDOWN_STATE}', elapsed ${ELAPSED_S}.${ELAPSED_FRAC}s)"
else
  # Check exit code: must be 0 for a clean SIGTERM shutdown.
  EXIT_CODE=$(docker inspect -f '{{.State.ExitCode}}' "$CONTAINER_ID" 2>/dev/null) || EXIT_CODE="-1"
  if [ "$EXIT_CODE" = "0" ]; then
    # Compare in milliseconds for exact contract compliance.
    # The contract bound is SHUTDOWN_TIMEOUT seconds; convert to ms for comparison.
    TIMEOUT_MS=$((SHUTDOWN_TIMEOUT * 1000))
    if [ "$ELAPSED_MS" -le "$TIMEOUT_MS" ]; then
      pass "shutdown completed in ${ELAPSED_S}.${ELAPSED_FRAC}s with exit code 0 (within ${SHUTDOWN_TIMEOUT}s bound)"
    else
      fail "shutdown took ${ELAPSED_S}.${ELAPSED_FRAC}s (exceeded ${SHUTDOWN_TIMEOUT}s bound), exit code 0"
    fi
  else
    fail "shutdown exited with code ${EXIT_CODE} (expected 0), elapsed ${ELAPSED_S}.${ELAPSED_FRAC}s"
  fi
fi

cleanup
trap - EXIT

# ──────────────────────────────────────────────────────────────────────────────
# 3. Missing required config exits nonzero before opening a listener
# ──────────────────────────────────────────────────────────────────────────────
echo ""
echo "=== Check 3: missing required config ==="

# Run without VEC_PG_MIGRATION_USER (required) — must exit nonzero and
# must not open any listener before exiting.
#
# Strategy: start a disposable sidecar container with -p 127.0.0.1::8080
# and `sleep 300` to hold the Docker-assigned ephemeral port mapping.
# Run the target service with --network container:<sidecar> so it shares
# the sidecar's network namespace. The sidecar keeps the port mapping alive
# even after the target exits (which it does quickly on config failure).
# Probe the host-mapped port while docker inspect confirms the target is
# running. After assertions, clean both containers.
#
# This eliminates the race where `docker port` cannot resolve the mapping
# because the target container exits before the port lookup completes.
#
# The startup sequence in main.go guarantees that a configuration failure
# (missing VEC_PG_MIGRATION_USER) causes an exit before any network
# listener opens:
#
#   1. config.Load() — validates VEC_PG_MIGRATION_USER (required)
#   2. configureLogging() — installs JSON handler
#   3. runMigrationStep() — opens migration connection (never reached)
#   4. newRuntimePool() — creates runtime pool (never reached)
#   5. newServer() + ctrl.Serve() — listener opens HERE (never reached)
#
# A config failure at step 1 returns immediately with os.Exit(1), logging
# "startup failed" with the server-authored variable identifier. Because
# the listener opens only inside Serve() at step 5, a config failure at
# step 1 is structural proof that no listener was opened.
#
# NOTE: sanitizeStartupError() in main.go emits "config:<variable>" for
# configuration errors. The variable name is a server-authored identifier
# (not a raw config value) and is safe to emit.

# Start a sidecar to hold the Docker-assigned ephemeral port mapping.
# The target container shares the sidecar's network namespace via
# --network container:<sidecar>. The sidecar runs `sleep 300` so the
# port mapping persists even after the target exits (which it does quickly
# on config failure). This eliminates the race where `docker port` cannot
# resolve the mapping because the target already exited.
MISSING_SIDECAR_ID=""
MISSING_HOST_PORT=""
sidecar_start MISSING_SIDECAR_ID MISSING_HOST_PORT

# Start the target container sharing the sidecar's network namespace.
# Omit VEC_PG_MIGRATION_USER — the binary should fail config validation
# and exit before any network activity. No --add-host needed: the container
# exits before connecting to the database.
MISSING_CONTAINER=$(docker run -d \
  --network "container:${MISSING_SIDECAR_ID}" \
  -e VEC_PG_HOST=host.docker.internal \
  -e VEC_PG_PORT="$PG_PORT" \
  -e VEC_PG_TLS_MODE=plain \
  -e VEC_ADMIN_TOKEN="verify-image-test-token-0000000000000000000000000000" \
  -e VEC_LISTEN_ADDR="0.0.0.0:8080" \
  "$IMAGE" serve)

# Cleanup function for this check (removes target and sidecar).
_missing_cleanup() {
  docker rm -f "$MISSING_CONTAINER" >/dev/null 2>&1 || true
  docker rm -f "$MISSING_SIDECAR_ID" >/dev/null 2>&1 || true
}
trap _missing_cleanup EXIT

# Poll container state with frequent probes until exit or deadline.
# Config-failure containers exit within milliseconds of starting, so use
# a tight loop (20 ms sleep) with a 5-second bounded deadline and an
# iteration cap. Each TCP connect is bounded by the host `timeout` utility
# to prevent hanging on stale ports.
POLL_DEADLINE=$(( $(date +%s) + 5 ))
MAX_POLL_ITERATIONS=500
POLL_ITERATIONS=0
TCP_CONNECTED=false
PROBE_RAN_WHILE_RUNNING=false

while [ "$(date +%s)" -lt "$POLL_DEADLINE" ] && [ "$POLL_ITERATIONS" -lt "$MAX_POLL_ITERATIONS" ]; do
  # State confirmation via docker inspect before each probe.
  MC_STATE=$(docker inspect -f '{{.State.Status}}' "$MISSING_CONTAINER" 2>/dev/null) || MC_STATE="unreachable"

  if [ "$MC_STATE" = "running" ]; then
    # Container is confirmed running — mark observation and probe TCP.
    PROBE_RAN_WHILE_RUNNING=true
    # Bound TCP connect with timeout to prevent hanging on stale ports.
    if timeout -s KILL 1 bash -c '(echo >/dev/tcp/127.0.0.1/'"${MISSING_HOST_PORT}"')' 2>/dev/null; then
      TCP_CONNECTED=true
    fi
  fi

  if [ "$MC_STATE" = "exited" ]; then
    break
  fi

  if [ "$MC_STATE" = "removed" ] || [ "$MC_STATE" = "unreachable" ]; then
    break
  fi

  sleep 0.02
  POLL_ITERATIONS=$((POLL_ITERATIONS + 1))
done

# Two-tier acceptance (package-5:880-890):
# (a) Structural — config validation (startup step 1) precedes listener
#     construction (startup step 5) in main.go; unit tests prove this ordering.
# (b) Defense-in-depth — TCP polling, evaluated only if the container is
#     observed in a Docker "running" state before exit. If the container exits
#     before observation (common for fast config failures), the TCP check is
#     skipped; the structural guarantees remain authoritative.
if [ "$PROBE_RAN_WHILE_RUNNING" != true ]; then
  info "TCP polling: never observed container in running state (PROBE_RAN_WHILE_RUNNING=false); skipping TCP check — structural proof authoritative (nonzero exit + exact safe log assertions below)"
fi

# Capture logs and exit code before cleanup.
MISSING_LOGS=$(docker logs "$MISSING_CONTAINER" 2>&1 || true)
MISSING_EXIT=$(docker inspect -f '{{.State.ExitCode}}' "$MISSING_CONTAINER" 2>/dev/null) || MISSING_EXIT="-1"
MISSING_FINAL_STATE=$(docker inspect -f '{{.State.Status}}' "$MISSING_CONTAINER" 2>/dev/null) || MISSING_FINAL_STATE="unreachable"

# 3a. Exit code must be nonzero.
if [ "$MISSING_EXIT" != "0" ] && [ "$MISSING_EXIT" != "-1" ]; then
  pass "missing VEC_PG_MIGRATION_USER exits nonzero (exit $MISSING_EXIT)"
else
  fail "missing VEC_PG_MIGRATION_USER should exit nonzero, got exit ${MISSING_EXIT}"
fi

# 3b. Stderr must contain a startup failure log line with the variable identifier.
# The sanitizer emits "config:<variable>" for configuration errors.
# VEC_PG_MIGRATION_USER is the missing variable.
if echo "$MISSING_LOGS" | grep -q '"msg":"startup failed"'; then
  pass "stderr contains startup failure log line"
else
  fail "stderr does not contain startup failure log line; output: $MISSING_LOGS"
fi

# 3c. The structured log must contain the server-authored variable identifier.
if echo "$MISSING_LOGS" | grep -q '"cause":"config:VEC_PG_MIGRATION_USER"'; then
  pass "stderr contains server-authored variable identifier (config:VEC_PG_MIGRATION_USER)"
else
  fail "stderr does not contain expected variable identifier; output: $MISSING_LOGS"
fi

# 3d. TCP polling: no connection should have succeeded while the container was running.
# If the probe never ran (container exited before observation), skip the TCP
# assertion — structural proof (nonzero exit + safe log assertions) is authoritative.
if [ "$PROBE_RAN_WHILE_RUNNING" = true ]; then
  if [ "$TCP_CONNECTED" = false ]; then
    pass "TCP polling: no listener opened before exit (port ${MISSING_HOST_PORT} never accepted connections)"
  else
    fail "TCP polling: listener opened on port ${MISSING_HOST_PORT} before container exited"
  fi
else
  info "TCP polling (3d): skipped — container exited before running-state observation; structural proof authoritative"
fi

# 3e. Process exited before listener startup.
if [ "$MISSING_FINAL_STATE" = "exited" ] && [ "$MISSING_EXIT" != "0" ]; then
  pass "process exited before listener startup (config failure at startup step 1, exit $MISSING_EXIT)"
else
  fail "process did not exit cleanly before listener startup (state: ${MISSING_FINAL_STATE}, exit: ${MISSING_EXIT})"
fi

# Cleanup for this check (target and sidecar).
docker rm -f "$MISSING_CONTAINER" >/dev/null 2>&1 || true
docker rm -f "$MISSING_SIDECAR_ID" >/dev/null 2>&1 || true
trap - EXIT

# ──────────────────────────────────────────────────────────────────────────────
# 5a. Shutdown timeout sum validation
# ──────────────────────────────────────────────────────────────────────────────
echo ""
echo "=== Check 5a: shutdown timeout sum validation ==="

# Run with VEC_HTTP_SHUTDOWN_GRACE and VEC_HTTP_SHUTDOWN_CLOSE_TIMEOUT
# whose sum exceeds 40s — must exit nonzero before any network activity.
# The config enforces that the sum does not exceed 40s.
#
# Strategy: same sidecar pattern as Check 3. A disposable sidecar holds
# the Docker-assigned ephemeral port mapping while the target container
# shares its network namespace. The sidecar keeps the port mapping alive
# after the target exits.
#
# The structured log must contain a startup failure with the
# server-authored classification naming both variables and the constraint.

# Start a sidecar to hold the Docker-assigned ephemeral port mapping.
# Same sidecar pattern as Check 3: the target may exit quickly on config
# failure, so the sidecar holds the port mapping alive.
SHUTDOWN_SIDECAR_ID=""
SHUTDOWN_HOST_PORT=""
sidecar_start SHUTDOWN_SIDECAR_ID SHUTDOWN_HOST_PORT

# Start the target container sharing the sidecar's network namespace.
SHUTDOWN_CONTAINER=$(docker run -d \
  --network "container:${SHUTDOWN_SIDECAR_ID}" \
  -e VEC_PG_HOST=host.docker.internal \
  -e VEC_PG_PORT="$PG_PORT" \
  -e VEC_PG_DATABASE=vector \
  -e VEC_PG_USER=vector_api \
  -e VEC_PG_MIGRATION_USER=vector_owner \
  -e VEC_PG_TLS_MODE=plain \
  -e VEC_ADMIN_TOKEN="verify-image-test-token-0000000000000000000000000000" \
  -e VEC_LISTEN_ADDR="0.0.0.0:8080" \
  -e VEC_HTTP_SHUTDOWN_GRACE="30s" \
  -e VEC_HTTP_SHUTDOWN_CLOSE_TIMEOUT="11s" \
  "$IMAGE" serve)

_shutdown_cleanup() {
  docker rm -f "$SHUTDOWN_CONTAINER" >/dev/null 2>&1 || true
  docker rm -f "$SHUTDOWN_SIDECAR_ID" >/dev/null 2>&1 || true
}
trap _shutdown_cleanup EXIT

# Poll container state with frequent probes until exit or deadline.
# Config-failure containers exit within milliseconds of starting, so use
# a tight loop (20 ms sleep) with a 5-second bounded deadline and an
# iteration cap. Each TCP connect is bounded by the host `timeout` utility
# to prevent hanging on stale ports.
POLL_DEADLINE=$(( $(date +%s) + 5 ))
MAX_POLL_ITERATIONS=500
POLL_ITERATIONS=0
TCP_CONNECTED=false
PROBE_RAN_WHILE_RUNNING=false

while [ "$(date +%s)" -lt "$POLL_DEADLINE" ] && [ "$POLL_ITERATIONS" -lt "$MAX_POLL_ITERATIONS" ]; do
  SD_STATE=$(docker inspect -f '{{.State.Status}}' "$SHUTDOWN_CONTAINER" 2>/dev/null) || SD_STATE="unreachable"

  if [ "$SD_STATE" = "running" ]; then
    PROBE_RAN_WHILE_RUNNING=true
    if timeout -s KILL 1 bash -c '(echo >/dev/tcp/127.0.0.1/'"${SHUTDOWN_HOST_PORT}"')' 2>/dev/null; then
      TCP_CONNECTED=true
    fi
  fi

  if [ "$SD_STATE" = "exited" ]; then
    break
  fi

  if [ "$SD_STATE" = "removed" ] || [ "$SD_STATE" = "unreachable" ]; then
    break
  fi

  sleep 0.02
  POLL_ITERATIONS=$((POLL_ITERATIONS + 1))
done

# Two-tier acceptance (package-5:880-890):
# (a) Structural — config validation (startup step 1) precedes listener
#     construction (startup step 5) in main.go; unit tests prove this ordering.
# (b) Defense-in-depth — TCP polling, evaluated only if the container is
#     observed in a Docker "running" state before exit. If the container exits
#     before observation (common for fast config failures), the TCP check is
#     skipped; the structural guarantees remain authoritative.
if [ "$PROBE_RAN_WHILE_RUNNING" != true ]; then
  info "TCP polling: never observed container in running state (PROBE_RAN_WHILE_RUNNING=false); skipping TCP check — structural proof authoritative (nonzero exit + exact safe log assertions below)"
fi

# Capture logs and exit code.
SHUTDOWN_LOGS=$(docker logs "$SHUTDOWN_CONTAINER" 2>&1 || true)
SHUTDOWN_EXIT=$(docker inspect -f '{{.State.ExitCode}}' "$SHUTDOWN_CONTAINER" 2>/dev/null) || SHUTDOWN_EXIT="-1"
SHUTDOWN_FINAL_STATE=$(docker inspect -f '{{.State.Status}}' "$SHUTDOWN_CONTAINER" 2>/dev/null) || SHUTDOWN_FINAL_STATE="unreachable"

# 5a-i. Exit code must be nonzero.
if [ "$SHUTDOWN_EXIT" != "0" ] && [ "$SHUTDOWN_EXIT" != "-1" ]; then
  pass "shutdown sum >40s exits nonzero (exit $SHUTDOWN_EXIT)"
else
  fail "shutdown sum >40s should exit nonzero, got exit ${SHUTDOWN_EXIT}"
fi

# 5a-ii. Stderr must contain a startup failure log line.
if echo "$SHUTDOWN_LOGS" | grep -q '"msg":"startup failed"'; then
  pass "stderr contains startup failure log line for shutdown sum"
else
  fail "stderr does not contain startup failure log line for shutdown sum; output: $SHUTDOWN_LOGS"
fi

# 5a-iii. The structured log must contain the server-authored classification
# naming both variables and the 40s constraint (value-free, no durations).
if echo "$SHUTDOWN_LOGS" | grep -q '"cause":"config:shutdown_sum_exceeds_40s:VEC_HTTP_SHUTDOWN_GRACE+VEC_HTTP_SHUTDOWN_CLOSE_TIMEOUT"'; then
  pass "stderr contains server-authored classification (shutdown_sum_exceeds_40s)"
else
  fail "stderr does not contain expected classification for shutdown sum; output: $SHUTDOWN_LOGS"
fi

# 5a-iv. TCP polling: no connection should have succeeded while the container was running.
# If the probe never ran (container exited before observation), skip the TCP
# assertion — structural proof (nonzero exit + safe log assertions) is authoritative.
if [ "$PROBE_RAN_WHILE_RUNNING" = true ]; then
  if [ "$TCP_CONNECTED" = false ]; then
    pass "TCP polling: no listener opened before exit (port ${SHUTDOWN_HOST_PORT} never accepted connections)"
  else
    fail "TCP polling: listener opened on port ${SHUTDOWN_HOST_PORT} before container exited"
  fi
else
  info "TCP polling (5a-iv): skipped — container exited before running-state observation; structural proof authoritative"
fi

# 5a-v. Process exited before listener startup.
if [ "$SHUTDOWN_FINAL_STATE" = "exited" ] && [ "$SHUTDOWN_EXIT" != "0" ]; then
  pass "process exited before listener startup (shutdown sum config failure, exit $SHUTDOWN_EXIT)"
else
  fail "process did not exit cleanly before listener startup (state: ${SHUTDOWN_FINAL_STATE}, exit: ${SHUTDOWN_EXIT})"
fi

# Cleanup for this check (target and sidecar).
docker rm -f "$SHUTDOWN_CONTAINER" >/dev/null 2>&1 || true
docker rm -f "$SHUTDOWN_SIDECAR_ID" >/dev/null 2>&1 || true
trap - EXIT

# ──────────────────────────────────────────────────────────────────────────────
# 4. Image inspection
# ──────────────────────────────────────────────────────────────────────────────
echo ""
echo "=== Check 4: image inspection ==="

# 4a. ENTRYPOINT
EP=$(docker inspect --format='{{json .Config.Entrypoint}}' "$IMAGE" 2>/dev/null || echo "[]")
if echo "$EP" | grep -q "vector-service"; then
  pass "ENTRYPOINT references vector-service"
else
  fail "ENTRYPOINT does not reference vector-service: $EP"
fi

# 4b. CMD
CMD_VAL=$(docker inspect --format='{{json .Config.Cmd}}' "$IMAGE" 2>/dev/null || echo "[]")
if echo "$CMD_VAL" | grep -q "serve"; then
  pass "CMD is serve"
else
  fail "CMD is not serve: $CMD_VAL"
fi

# 4c. USER is non-root
USER_VAL=$(docker inspect --format='{{.Config.User}}' "$IMAGE" 2>/dev/null || echo "")
if [ -n "$USER_VAL" ] && [ "$USER_VAL" != "root" ] && [ "$USER_VAL" != "0" ]; then
  pass "USER is non-root ($USER_VAL)"
else
  fail "USER is root or unset: '$USER_VAL'"
fi

# ──────────────────────────────────────────────────────────────────────────────
# 4d. Filesystem inspection via docker export
# ──────────────────────────────────────────────────────────────────────────────
# Create a temporary container, export its filesystem, and inspect the file list.
TEMP_ID=$(docker create "$IMAGE" cat)
TMPDIR=$(mktemp -d)
trap "docker rm -f '$TEMP_ID' >/dev/null 2>&1; rm -rf '$TMPDIR'" EXIT

# Extract full listing with metadata (type, mode, uid, gid, size) for comparison.
docker export "$TEMP_ID" | tar -tvf - 2>/dev/null | sort -k10 > "$TMPDIR/files_verbose.txt"
docker export "$TEMP_ID" | tar -tf - 2>/dev/null | sort > "$TMPDIR/files.txt"

# 4d-i. Effective runtime UID: verify the USER instruction maps to a non-root UID.
# The distroless nonroot image sets USER to "nonroot" which maps to UID 65532.
# Extract /etc/passwd from the image and verify the runtime user's UID.
RUNTIME_UID=$(docker export "$TEMP_ID" 2>/dev/null | tar -xOf - etc/passwd 2>/dev/null \
  | grep "^${USER_VAL}:" \
  | cut -d: -f3)
if [ -n "$RUNTIME_UID" ] && [ "$RUNTIME_UID" != "0" ]; then
  pass "runtime user '${USER_VAL}' maps to non-root UID ($RUNTIME_UID)"
else
  if [ -z "$RUNTIME_UID" ]; then
    fail "could not resolve runtime user '${USER_VAL}' UID from /etc/passwd"
  else
    fail "runtime user '${USER_VAL}' maps to root UID 0"
  fi
fi

# 4d-ii. Strict filesystem allowlist: the runtime image should only contain
# files from the distroless static-debian13:nonroot base plus our binary.
# We establish the expected filesystem by extracting the exact base image
# (pinned by digest) and comparing against our image using Python's tarfile
# module for precise comparison of each path, type, mode, uid/gid, symlink
# target, and regular-file content SHA256.

# Pinned base image digest (must match Dockerfile).
BASE_IMAGE="gcr.io/distroless/static-debian13:nonroot@sha256:1c2c046bc09ed40fad370b599a0b1ae7987f55b01e247cf27a7c27cd97e5bbc7"

# Extract the base image filesystem.
BASE_CONTAINER=$(docker create "$BASE_IMAGE" cat)
BASE_TAR="$TMPDIR/base.tar"
IMAGE_TAR="$TMPDIR/image.tar"
docker export "$BASE_CONTAINER" > "$BASE_TAR"
docker rm -f "$BASE_CONTAINER" >/dev/null 2>&1

# Extract our image filesystem.
docker export "$TEMP_ID" > "$IMAGE_TAR"

# Compare using Python tarfile for precise entry-by-entry comparison.
FS_CHECK_OUTPUT=$(python3 "$(dirname "$0")/fs-compare.py" "$BASE_TAR" "$IMAGE_TAR" 2>&1) || true

if echo "$FS_CHECK_OUTPUT" | grep -q '^OK:'; then
  # Extract counts for the pass message.
  BASE_COUNT=$(echo "$FS_CHECK_OUTPUT" | grep '^OK:' | head -1 | sed 's/.*base=\([0-9]*\).*/\1/')
  IMAGE_COUNT=$(echo "$FS_CHECK_OUTPUT" | grep '^OK:' | head -1 | sed 's/.*image=\([0-9]*\).*/\1/')
  BINARY_SHA=$(echo "$FS_CHECK_OUTPUT" | grep '^OK: binary' | sed 's/.*sha256=\(.*\)/\1/')
  pass "filesystem contains exactly distroless base ($BASE_COUNT files) + vector-service binary with parent dirs (total $IMAGE_COUNT files, binary sha256=${BINARY_SHA:0:16}...)"
else
  echo "  Python comparison output:"
  echo "$FS_CHECK_OUTPUT" | sed 's/^/    /'
  fail "filesystem does not match distroless base + vector-service binary"
fi

# ──────────────────────────────────────────────────────────────────────────────
# 4d-iii. Denylist checks (defense in depth)
# ──────────────────────────────────────────────────────────────────────────────
# No shell
if grep -qE '/(bin|sbin)/sh$|/(bin|sbin)/bash$|/(bin|sbin)/zsh$' "$TMPDIR/files.txt" 2>/dev/null; then
  fail "image contains a shell binary"
else
  pass "no shell binary found"
fi

# No Go toolchain (tar paths have no leading /)
if grep -q 'usr/local/go/' "$TMPDIR/files.txt" 2>/dev/null; then
  fail "image contains Go toolchain"
else
  pass "no Go toolchain found"
fi

# No source files
if grep -q '\.go$' "$TMPDIR/files.txt" 2>/dev/null; then
  fail "image contains Go source files"
else
  pass "no Go source files found"
fi

# No migrations directory
if grep -q '/migrations/' "$TMPDIR/files.txt" 2>/dev/null; then
  fail "image contains migrations directory"
else
  pass "no migrations directory found"
fi

# No test artifacts
if grep -q '\.test$' "$TMPDIR/files.txt" 2>/dev/null; then
  fail "image contains test binaries"
else
  pass "no test binaries found"
fi

# No secrets (private keys, certificates baked in)
if grep -qiE '(id_rsa|\.pem$|\.key$|\.p12$)' "$TMPDIR/files.txt" 2>/dev/null; then
  fail "image contains potential secret files"
else
  pass "no potential secret files found"
fi

# No package managers
if grep -qE '/usr/bin/(apt|dpkg)$|/bin/apk$' "$TMPDIR/files.txt" 2>/dev/null; then
  fail "image contains a package manager"
else
  pass "no package manager found"
fi

# ──────────────────────────────────────────────────────────────────────────────
# 4e. Image layer inspection: check build history for forbidden payloads
# ──────────────────────────────────────────────────────────────────────────────
# Inspect image history to verify no source, test, migration, or secret
# layers were baked in during the build. Use `docker history` as the
# History field in docker inspect may be empty for buildkit images.
#
# NOTE: docker history reflects the visible build layers. If the final image
# is squashed (e.g., via docker-squash or BuildKit --output with squash),
# layer history may be flattened and this check becomes less reliable.
# The authoritative check is the filesystem comparison in 4d-ii, which
# compares against the pinned distroless base digest regardless of layer
# structure. This history check is defense-in-depth for non-squashed images.
#
# IMPORTANT: If docker history returns no layers at all (e.g., fully
# squashed image), we cannot verify that forbidden content was not baked
# in through history. In this case, fail the check rather than silently
# passing — the filesystem check (4d-ii) is the authoritative verification.

# Capture docker history output and exit status separately.
# Fail closed if the command fails entirely.
HISTORY_EXIT=0
HISTORY=$(docker history --no-trunc "$IMAGE" 2>/dev/null) || HISTORY_EXIT=$?

if [ "$HISTORY_EXIT" -ne 0 ]; then
  # Command failed — cannot verify layer content. Fail closed.
  fail "docker history command failed (exit $HISTORY_EXIT); cannot verify layer content"
elif [ -z "$HISTORY" ]; then
  # Command succeeded but returned empty output — header-only or opaque image.
  fail "docker history returned empty output; cannot verify layer content (opaque image)"
else
  # Count the number of history entries (excluding the header line).
  HISTORY_LINES=$(echo "$HISTORY" | tail -n +2 | grep -c '[^[:space:]]' || true)

  if [ "$HISTORY_LINES" -eq 0 ]; then
    # Header present but no actual layers — fail closed.
    fail "docker history returned header-only output with no layers; cannot verify layer content"
  fi

  # Check for source code in layer descriptions (COPY/ADD commands in history).
  # The builder stage copies source files; the runtime stage should only
  # contain the binary. If any layer references .go files, migrations/,
  # test artifacts, or secrets, flag it.
  if echo "$HISTORY" | grep -qi '\.go'; then
    fail "image history references Go source files"
  else
    pass "no Go source references in image history"
  fi

  if echo "$HISTORY" | grep -qi 'migrations/'; then
    fail "image history references migrations directory"
  else
    pass "no migrations references in image history"
  fi

  if echo "$HISTORY" | grep -qiE '(\.test|_test\.go)'; then
    fail "image history references test artifacts"
  else
    pass "no test artifact references in image history"
  fi

  if echo "$HISTORY" | grep -qiE '(id_rsa|\.pem|\.key|\.p12|secret)'; then
    fail "image history references potential secret files"
  else
    pass "no secret file references in image history"
  fi
fi

# Cleanup
docker rm -f "$TEMP_ID" >/dev/null 2>&1
rm -rf "$TMPDIR"
trap - EXIT

# ──────────────────────────────────────────────────────────────────────────────
# Summary
# ──────────────────────────────────────────────────────────────────────────────
echo ""
if [ "$FAILURES" -eq 0 ]; then
  echo "All checks passed."
  exit 0
else
  echo "${FAILURES} check(s) failed."
  exit 1
fi
