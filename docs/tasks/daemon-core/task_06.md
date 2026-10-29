---
status: completed
title: "Task 6: CLI: daemon start and stop, status and events"
type: feature
complexity: high
---

# Task 6: CLI: daemon start and stop, status and events

## Overview

This task delivers the Motivating Problem end to end. The `harness` binary now:
- starts the daemon in the background (with the pre-spawn home checks) and returns once it is ready;
- runs it in the foreground;
- stops it idempotently;
- shows status;
- lists and follows events with the human, json and jsonl output rules.

This closes slice 1.

## Shippable Outcome

- **Outcome.** The `_dx.md` Golden Path runs as written on macOS:
  - `harness daemon start`, `status`, `events`, `events --follow` and `daemon stop`;
  - the stream continues at sequence 5 after a restart;
  - every listed command honours `-o json|jsonl` and the exit-code table.
- **Verify in this task:**
  - the assigned unit tests (fake spawner, fake clock, fake lock prober);
  - the assigned E2E transcripts against the built binary;
  - the Golden Path, run once by hand on a temporary `HARNESS_HOME`, recorded in the task memory.
- **Integration verification:** task 9 reruns every E2E suite on macOS and Linux.

## Requirements

**Contracts:**
- `_dx.md` › Golden Path, › `daemon start` (success, already running, a daemon stopping, home rejected), › `--foreground`, › `daemon stop`, › `status`, › `events`, › Usage errors, › Errors.
- `_spec.md` › Detached start (steps 1–4 and 6 here; step 5's child-error re-raise is task 7) and › Stop (steps 1–4; the unreachable-but-locked branch is task 7).
- `_spec.md` › CLI `events` and `--follow`.
- `_spec.md` › Agent Manageability Plan.
- `_user_stories.md` US-001, US-002, US-004, US-006, US-008, US-009, US-012.

**Behaviour that must hold:**
- **Output rules.**
  - Success goes to stdout.
  - Errors go to stderr as a single JSON object under json and jsonl, with stdout empty.
  - Usage errors under `-o json` emit `{"error":{"code":"usage"}}`.
- **Pre-spawn order:** `Resolve` → `CheckSocketPath` → `CheckSecurity` (existing home) → `EnsureLayout`, before any network call or spawn.
- **Spawn:**
  - open `daemon.out` with `O_APPEND` and 0600, and record the offset;
  - `Setsid`, `Dir` = home, stdin `/dev/null`;
  - run `daemon start --foreground --detached`;
  - poll every 100 ms until `running` with a matching PID.
- **Stop:** POST shutdown, then wait until `LockFree` and the socket is gone, for up to the reported `shutdown_grace` + 5 s. Otherwise `daemon_stop_timeout`.
- **Events:**
  - client-side validation through `api/contract`, so the exit is 65 without a daemon;
  - the jsonl output ends with a `page` line;
  - `--follow` never sends `limit`, and `--follow -o json` is a usage error;
  - the SSE reader has no token limit, and the client a 45 s idle deadline;
  - human rendering uses 5/24/32 columns.
- **Request timeouts:** 2 s for status and shutdown requests.

## Subtasks

- [x] 6.1 Build the cobra root: global `-o` and `--home`, the output-rule writer, the error-to-exit mapping, usage errors in JSON.
- [x] 6.2 Build the UDS HTTP client with timeouts, and the SSE reader.
- [x] 6.3 Implement `daemon start --foreground` (calls `daemon.Run` with `Interactive`) and detached start: pre-spawn checks, spawn, poll, timeout path.
- [x] 6.4 Implement `daemon stop` and `status`.
- [x] 6.5 Implement `events`: list, jsonl page line, follow with endings and idle deadline, human rendering.
- [x] 6.6 Add the E2E harness (`e2e/` package, `TestMain` builds the binary) and the assigned transcripts.

## Implementation Details

See `_spec.md` Part II › Detached start, › Stop, › CLI `events` and `--follow`.

### Relevant Files

- `cmd/harness/main.go` — wire the root command.
- `internal/cli/{root.go,output.go,exitcodes.go,client.go,sse.go,start.go,stop.go,status.go,events.go,render.go}` — new.
- `e2e/{main_test.go,lifecycle_test.go,events_test.go}` — new, `//go:build e2e`.

### Dependent Files

- Task 7 extends `start.go` and `stop.go`. Task 8 adds the `doctor`, `config` and `version` verbs to the root.

### External References

- `~/Projetos/Pessoal/compozy/internal/cli/daemon.go:390-436` — the detached readiness poll until the PID matches.
- `~/Projetos/Pessoal/compozy/internal/procutil/detached_unix.go:30-84` — the `O_APPEND` log for the detached child; kill and reap on failure.
- `~/Projetos/Pessoal/compozy/internal/cli/client_transport.go:389-405` — `daemon_unavailable` with the start hint.
- `~/Projetos/Pessoal/compozy/internal/cli/client_api_errors.go:238-253` — HTTP status to sysexits mapping.

### Related ADRs

- [ADR-002](adrs/adr-002.md) — the CLI dials the socket; the socket-path limit is checked before spawning.

## Deliverables

- The `harness` CLI with `daemon start|stop`, `status` and `events`.
- The E2E harness and the assigned transcripts.
- A Golden Path run recorded in the task memory.

## Tests

- [x] UT-040, UT-041, UT-042, UT-043, UT-044, UT-045, UT-046, UT-047, UT-048, UT-049 — start/stop basics, status rendering, output rules, error mapping, usage, follow endings.
- [x] UT-120, UT-125, UT-133 — pre-spawn checks, the stop flow, `daemon.out` handling.
- [x] UT-127, UT-128, UT-129, UT-130, UT-131 — jsonl and follow rules, client-side validation, usage JSON, the follow client, human rendering.
- [x] E2E-001, E2E-002, E2E-004, E2E-005, E2E-006, E2E-007, E2E-008 — Golden Path journeys, idempotent start, socket limit, stop and history, no daemon, JSON page, follow endings.
- [x] E2E-011, E2E-013, E2E-015, E2E-016, E2E-017, E2E-023, E2E-024 — offline validation, usage text, foreground and second foreground, `stop -o json`, human and jsonl events, usage JSON, `--follow -o json`.

## Success Criteria

- The Golden Path transcript in `_dx.md` reproduces on macOS, with matching output shapes and exit codes.
- `go test -race ./internal/cli/...` and `go test -tags e2e ./e2e/...` are green.
