---
status: completed
title: "Task 5: Daemon runtime: boot, shutdown and the daemon stream"
type: backend
complexity: high
---

# Task 5: Daemon runtime: boot, shutdown and the daemon stream

## Overview

This task builds `internal/daemon`, the composition root. `daemon.Run`:
- sets umask 077 and owns SIGINT, SIGTERM and SIGHUP;
- performs the twelve boot steps with undo and cancellation;
- serves the API on `daemon.sock`;
- shuts down in the fixed order within `shutdown_grace`.

Along the way it records the `daemon` stream: `config.loaded`, `daemon.started`, `daemon.stopping`, `daemon.stopped`, `daemon.boot_failed`, `daemon.unclean_shutdown_detected`.

The daemon-side resilience is part of this sequence: boot failure and cancellation, the budget overrun, home refusal, stale-file cleanup and unclean detection.

## Shippable Outcome

- **Outcome.** `daemon.Run` on a temporary home:
  - boots to `running`;
  - answers the API over the socket;
  - records the `daemon` stream with a PID in every event;
  - follows and resumes without gaps;
  - stops in the documented order, with the socket removed only at step 8.

  A boot cancelled or failing at any step leaves the ledger and the home in the documented state.
- **Verify in this task:** the assigned integration tests, run in-process with `Options.Hooks`, under `-race`.
- **Integration verification:** task 6 drives it through the CLI; task 7 adds the subprocess crash suite.

## Requirements

**Contracts:**
- `_spec.md` › Boot and shutdown sequences.
- `_spec.md` › Core Interfaces › `internal/daemon` (`Options`, `Hooks`, `ExitError`, `Run`).
- `_spec.md` › Business Rules 5–6 and 9–13.
- `_spec.md` › Safety Invariants 1, 3, 12, 13, 14.
- `_spec.md` › Monitoring and Observability (log records).

**Behaviour that must hold:**
- **`Run`:** `syscall.Umask(0o077)` is its first statement. It installs SIGINT/SIGTERM/SIGHUP handlers (reason `signal`). Ctx cancel uses reason `context`. A second signal during shutdown exits 1.
- **Boot.** Steps 1–12 exactly as specified, with cancellation checked at every boundary.
- **When the boot fails or is cancelled:**
  - **failure once the ledger is open:** append `daemon.boot_failed{pid, step, code}`, then undo;
  - **cancellation after step 9:** append `stopping` and `stopped`, then undo;
  - **cancellation between steps 6 and 9:** undo only.
- **Unclean detection (step 7):** if the last `daemon` event is neither `daemon.stopped` nor `daemon.boot_failed`, record `previous_pid` from that event's `data.pid`.
- **Listener:** `UnixListener.SetUnlinkOnClose(false)`. **IT-051 must show whether Go would otherwise unlink the socket on close**; record the answer in the task memory, as it is an open check from the spec.
- **Shutdown:**
  - order: `draining`, `stopping`, drain, `stopped`, broker close, `Server.Shutdown` (with `RegisterOnShutdown` cancelling stream contexts), ledger close, remove `daemon.json`, then the socket, then release the lock;
  - budget: an overrun is logged and the daemon exits 1.
- **`Options.Mode`:** `Interactive` mirrors logs to stderr. `Detached` mirrors until `running`; task 7 adds the final error line on stderr.
- **`Options.Hooks`** (in-process): `AfterBootStep`, `BeforeShutdownStep`, `OnRunning` (exposing the Appender, the Gate and the logger), and timing overrides. The subprocess form (`HARNESS_TESTHOOKS`, build tag `harness_testhooks`) is task 7's.

## Subtasks

- [x] 5.1 Write the `Run` skeleton: umask, signal ownership, `ExitError`, and the boot-step runner with an undo stack and cancellation checks.
- [x] 5.2 Implement boot steps 1–12, wiring `home`, `config`, `logging`, `ledger` (`OnCommit` → `broker.Publish`), `broker`, `admission` and `api`.
- [x] 5.3 Record the `daemon` stream events, including unclean detection, `boot_failed`, and cancellation records.
- [x] 5.4 Implement the shutdown sequence with the budget, overrun handling and second-signal exit.
- [x] 5.5 Add the in-process `Hooks` and the integration tests.

## Implementation Details

See `_spec.md` Part II › Boot and shutdown sequences.

### Relevant Files

- `internal/daemon/{run.go,boot.go,shutdown.go,signals.go,hooks.go,events.go,status.go}` — new.

### Dependent Files

- `internal/cli` (task 6) calls `daemon.Run` for `start --foreground` and spawns it detached.
- `internal/doctor` (task 8) plugs into the `doctorRunner` slot.

### External References

- `~/Projetos/Pessoal/compozy/internal/daemon/boot_components.go:8-53` — ordered boot steps with an undo stack.
- `~/Projetos/Pessoal/compozy/internal/daemon/daemon_lifecycle.go:124-156` — the shutdown budget and its order.
- `~/Projetos/Pessoal/compozy/internal/session/manager_prompt_event_storage.go:139-161` — persist, then publish.
- `~/Projetos/Pessoal/compozy/internal/api/udsapi/server_start.go:60-80` — UDS listener with 0600 and stale-socket removal.
- `~/Projetos/Pessoal/compozy/internal/api/udsapi/server_start.go:72-90` — `RegisterOnShutdown` stream cancellation.

### Related ADRs

- [ADR-001](adrs/adr-001.md), [ADR-002](adrs/adr-002.md), [ADR-003](adrs/adr-003.md).

## Deliverables

- `internal/daemon` with `Run`, the in-process hooks and the integration tests.
- A task-memory note with the IT-051 result on Go's default unlink behaviour.

## Tests

- [x] UT-013 — file modes under umask 022, including `-wal` and `-shm`.
- [x] IT-001, IT-002, IT-003, IT-004, IT-005, IT-006 — boot, stop order, single instance, two homes, gate states, no hot reload.
- [x] IT-030, IT-031, IT-032, IT-033, IT-034, IT-035 — follow over the socket: shutdown frame, live, resume, slow follower, many followers, concurrent appends.
- [x] IT-041, IT-042, IT-043 — file modes, log redaction, no TCP listener.
- [x] IT-045, IT-046, IT-047, IT-051 — insecure home refused, shutdown while starting, boot failure records `boot_failed`, socket kept until step 8.

## Success Criteria

- `go test -race -tags integration ./internal/daemon/...` is green.
- `make gate` is green.
- No integration test leaves a process, socket or lock behind.
