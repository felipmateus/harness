---
status: completed
title: "Task 7: Resilient start and stop under races, crashes and timeouts"
type: backend
complexity: high
---

# Task 7: Resilient start and stop under races, crashes and timeouts

## Overview

This task makes start and stop correct when things go wrong:
- **A failing detached daemon** reports its exact error through a final JSON line, which the CLI re-raises. Otherwise the CLI reports `daemon_start_failed` with the output tail.
- **Concurrent and draining starts** converge on one daemon.
- **A stuck boot** is killed: SIGTERM, then SIGKILL.
- **A stop on an unreachable daemon** that still holds the lock waits for it.

The task also adds the subprocess test hooks and the crash suite. This is slice 2.

## Shippable Outcome

- **Outcome.** Under races, crashes, signals and timeouts, `harness daemon start` and `stop`:
  - behave exactly as `_dx.md` and US-001.EC-1…EC-8 and US-004.EC-5…EC-8 describe;
  - never report a daemon that is going away as running;
  - never leave a daemon behind after a failed or timed-out start.
- **Verify in this task:** the assigned unit tests, plus the subprocess integration and E2E cases with a binary built with `-tags harness_testhooks`.
- **Integration verification:** task 9 reruns everything on macOS and Linux.

## Requirements

**Contracts:**
- `_spec.md` › Detached start step 5 (child error re-raise, `daemon_already_running` loop, `daemon_start_failed` details) and step 6 (kill escalation).
- `_spec.md` › Stop step 1 (the unreachable-but-locked branch).
- `_spec.md` › Boot and shutdown sequences: the `Detached` final error line.
- `_dx.md` › `daemon start`: exits during startup, errors with their own code, readiness timeout, a daemon stopping.
- `_dx.md` › `daemon stop` (unreachable but holding the home).
- `_user_stories.md` US-001.EC-1/4/6/7/8, US-002.EC-1/2, US-004.EC-5/7/8, US-005.

**Behaviour that must hold:**
- **The `Detached` daemon** writes exactly one `{"error":{code,message,hint}}` line to stderr before exiting on any boot failure.
- **The CLI reads `daemon.out` only after its recorded offset:**
  - `config_invalid`, `home_insecure`, `socket_path_too_long`, `ledger_schema_ahead` → re-raised with their exit codes;
  - `daemon_already_running` → back to the status probe;
  - anything else, or no line → `daemon_start_failed` with `details.pid`, `details.exit_code` and `details.output_tail` (at most 20 lines).
- **The start decision table:** `running`, `starting`, `draining` or locked-unreachable, free. It never reports `already_running` unless the status says `running`.
- **On timeout:**
  1. SIGTERM the child's process group.
  2. Wait 2 s.
  3. SIGKILL the group, then reap the child.
  4. Report `daemon_start_timeout`.
- **Foreground start** retries `AcquireLock` 3 times over 100 ms before reporting `daemon_already_running` (lock-probe collisions).
- **Subprocess hooks:** `HARNESS_TESTHOOKS` is parsed only in binaries built with `-tags harness_testhooks`, and maps onto `daemon.Options.Hooks`:
  - `hold-boot-step=N`, `fail-boot-step=N`, `hold-shutdown-step=N`, `block-exit`.

## Subtasks

- [x] 7.1 Add the `Detached`-mode final error line in `daemon start --foreground --detached`, and turn the stderr mirror off at `running`.
- [x] 7.2 Implement the CLI child-error parsing, re-raise and `daemon_start_failed` details.
- [x] 7.3 Implement the start decision table, including waiting out a draining daemon and the `daemon_already_running` loop.
- [x] 7.4 Implement kill escalation on timeout, and the stop unreachable-but-locked branch.
- [x] 7.5 Add the `harness_testhooks` build tag with the `HARNESS_TESTHOOKS` parser, and the subprocess crash and race suite.

## Implementation Details

See `_spec.md` Part II › Detached start, › Stop, › When the boot fails or is cancelled.

### Relevant Files

- `internal/cli/{start.go,stop.go,childerror.go}` — extend or new.
- `internal/daemon/detached.go` — the final error line and mirror cut-off.
- `internal/daemon/testhooks_env.go` — `//go:build harness_testhooks`.
- `internal/daemon/*_integration_test.go`, `e2e/resilience_test.go` — new cases.

### Dependent Files

- `internal/cli/start.go` and `stop.go` from task 6.

### External References

- `~/Projetos/Pessoal/compozy/internal/procutil/detached_unix.go:30-84` — `O_APPEND` log, SIGKILL of the group, reap.
- `~/Projetos/Pessoal/compozy/internal/cli/daemon.go:390-436` — readiness poll.

### Related ADRs

- [ADR-002](adrs/adr-002.md), [ADR-003](adrs/adr-003.md).

## Deliverables

- Resilient start and stop.
- The `harness_testhooks` subprocess hooks.
- The crash and race suite.

## Tests

- [x] UT-121, UT-122, UT-123, UT-124, UT-126 — child-error re-raise, `daemon_start_failed`, decision table, kill escalation, unreachable-but-locked stop.
- [x] IT-020, IT-021, IT-022, IT-023, IT-024, IT-025, IT-026, IT-027 — session survival, start failure, concurrent starts, second signal, SIGKILL recovery, budget overrun, stale `daemon.json`, mid-transaction crash.
- [x] IT-048, IT-049, IT-050 — concurrent stops, start while draining, paused follower during stop.
- [x] IT-052, IT-053, IT-054, IT-055, IT-056 — final error line, stop timeout, SIGHUP/SIGTERM, SIGTERM during boot, readiness timeout leaves no daemon.
- [x] E2E-014, E2E-021, E2E-022 — schema ahead, insecure home, start failure with details.

## Success Criteria

- The subprocess suites are green on macOS under `-tags "integration harness_testhooks"` and `-tags "e2e harness_testhooks"`.
- No test leaves a process behind.
