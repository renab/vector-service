# Resolution: F-1 — Shutdown Timeout Semantic Bounds

**Finding**: `internal/config/config.go` accepts arbitrary positive
`VEC_HTTP_SHUTDOWN_GRACE` and `VEC_HTTP_SHUTDOWN_CLOSE_TIMEOUT`;
package-5 requires worst-case stop ≤ 40 seconds (defaults 30+10).

**Severity**: High
**Owner**: Architect
**Status**: Resolved

## Decision

Impose a maximum-sum bound in config validation AND revise the
operational contract documentation.

The sum of `VEC_HTTP_SHUTDOWN_GRACE` and `VEC_HTTP_SHUTDOWN_CLOSE_TIMEOUT`
must not exceed **40 seconds**. Configs violating this bound are rejected
at startup with a structured error.

## Rationale

1. **Architecture contract.** The overview invariant 9 states "shutdown
   deadlines are all explicitly bounded." The bounded shutdown contract
   is not merely a default; it is documented across package-4 section 7,
   package-5c, TESTING.md, and the config reference.

2. **Consistent with existing validation.** The config package already
   enforces other architecture contracts at startup: `VEC_PG_DATABASE`
   must be `vector`, `VEC_PG_USER` must be `vector_api`, migration
   identity must differ from runtime identity. The shutdown bound is
   the same category.

3. **Operational correctness.** The worst-case stop time
   (`HTTPShutdownGrace` + `HTTPShutdownCloseTimeout`) directly determines
   the orchestrator's `terminationGracePeriodSeconds`. If the sum
   exceeds the orchestrator's stop budget, the orchestrator sends SIGKILL,
   defeating the purpose of bounded graceful shutdown. The defaults
   (30+10=40s) are designed to fit within a typical 50s container stop
   budget with ~10s headroom.

4. **Testability.** The bound is verifiable at startup (config
   validation) and at runtime (the bounded shutdown test in TESTING.md
   step 4).

5. **Flexibility preserved.** Operators may set any combination whose
   sum is ≤ 40s (e.g., 40+0, 20+20, 35+5). The bound constrains the
   worst-case stop, not the individual phases.

## Exact Bound

```
VEC_HTTP_SHUTDOWN_GRACE + VEC_HTTP_SHUTDOWN_CLOSE_TIMEOUT ≤ 40s
```

Both values must be positive durations (existing validation). The sum
must not exceed 40 seconds (new validation).

## Validation Error

When the bound is violated, config loading fails with a structured
startup error. The error should name both variables and state the
constraint:

```
config:VEC_HTTP_SHUTDOWN_GRACE: shutdown timeout sum
  (VEC_HTTP_SHUTDOWN_GRACE + VEC_HTTP_SHUTDOWN_CLOSE_TIMEOUT)
  must not exceed 40s
```

The error is emitted before any network activity. The process exits
non-zero.

## Files Updated

### 1. `docs/implementation/README.md` — Configuration Reference

Updated the config reference table entries for `VEC_HTTP_SHUTDOWN_GRACE`
and `VEC_HTTP_SHUTDOWN_CLOSE_TIMEOUT` to state the sum constraint.

### 2. `docs/implementation/package-5-verification-runtime.md` — 5c Container Contract

Updated the bounded stop section to:
- State the 40-second maximum explicitly as a config-enforced bound
- Clarify the relationship between the sum and `terminationGracePeriodSeconds`
- Add the config-validation rejection case to the acceptance criteria

### 3. `docs/TESTING.md` — Runtime Test Script

Updated the bounded shutdown test step to note the config validation
test case (sum exceeds bound → startup rejection).

## Acceptance Tests

The following acceptance tests are required. Validation owner:
Orchestrator.

### Config validation tests (unit)

| # | Scenario | Config | Expected |
|---|----------|--------|----------|
| 1 | Default values | unset (defaults 30s+10s) | Accept, sum=40s |
| 2 | Equal split | `GRACE=20s`, `CLOSE=20s` | Accept, sum=40s |
| 3 | Skewed valid | `GRACE=35s`, `CLOSE=5s` | Accept, sum=40s |
| 4 | Under limit | `GRACE=15s`, `CLOSE=10s` | Accept, sum=25s |
| 5 | Minimum valid | `GRACE=1s`, `CLOSE=1s` | Accept, sum=2s |
| 6 | Sum exceeds bound | `GRACE=30s`, `CLOSE=11s` | Reject, sum=41s |
| 7 | Large exceed | `GRACE=120s`, `CLOSE=60s` | Reject, sum=180s |
| 8 | Grace at limit, close positive | `GRACE=40s`, `CLOSE=1s` | Reject, sum=41s |
| 9 | Empty grace (uses default) | unset `GRACE`, `CLOSE=11s` | Reject, sum=41s (30+11) |
| 10 | Empty close (uses default) | `GRACE=31s`, unset `CLOSE` | Reject, sum=41s (31+10) |

Test 9 and 10 verify that the bound applies to the effective sum
(after defaults are applied), not just the explicitly-set values.

### Runtime test (integration)

The existing bounded shutdown test (TESTING.md step 4) remains valid:
- `docker stop --time` sends SIGTERM
- Container reaches `exited` state within the configured worst-case
  stop time (≤ 40s with defaults)
- Exit code 0

No change needed to this test; the config bound ensures the worst-case
stop is always ≤ 40s.

## Out of scope

- Changing the default values (30s + 10s remain defaults)
- Adding per-variable maximums beyond the sum constraint
- Runtime renegotiation of shutdown bounds
- Changes to the shutdown sequence or exit-status contract

## Open issues

None.
