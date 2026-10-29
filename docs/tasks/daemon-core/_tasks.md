---
schema_version: "spec-tasks/v2"
workflow: daemon-core
graph:
  nodes:
    - id: task_01
      file: task_01.md
    - id: task_02
      file: task_02.md
    - id: task_03
      file: task_03.md
    - id: task_04
      file: task_04.md
    - id: task_05
      file: task_05.md
    - id: task_06
      file: task_06.md
    - id: task_07
      file: task_07.md
    - id: task_08
      file: task_08.md
    - id: task_09
      file: task_09.md
  edges:
    - from: task_01
      to: task_02
    - from: task_02
      to: task_03
    - from: task_01
      to: task_04
    - from: task_03
      to: task_04
    - from: task_04
      to: task_05
    - from: task_05
      to: task_06
    - from: task_06
      to: task_07
    - from: task_06
      to: task_08
    - from: task_07
      to: task_09
    - from: task_08
      to: task_09
---

# Tasks: daemon-core — the harness daemon, its ledger and its CLI

> Spec: [_spec.md](_spec.md) (approved 2026-10-28, revised with [review-001](review-001.md)) · Stories: [_user_stories.md](_user_stories.md) · Surface: [_dx.md](_dx.md) · Tests: [_tests.md](_tests.md) · [ADR-001](adrs/adr-001.md) · [ADR-002](adrs/adr-002.md) · [ADR-003](adrs/adr-003.md)

**Rules for every task.**
- Read the repository `AGENTS.md` (created by task 1).
- English only in code, comments, docs and commits; Go 1.27.
- `make gate` must be green: `gofmt`, `go vet`, `go test -race`, the architecture test.
- Tests use temporary homes only (`testhome.New`). **Never read or write the operator's real `~/.harness`.**
- Ask the user before committing, pushing, or installing anything on the machine.
- Reverse-spec references point to the Compozy checkout at `~/Projetos/Pessoal/compozy` (branch `study/v0.4.2`).

## Slices

**Slice 1 — tasks 1–6, the MVP.** It solves the Motivating Problem end to end:
- `harness daemon start` returns once a background daemon is ready;
- `harness events --follow` shows `config.loaded`, `daemon.started`, `daemon.stopping` and `daemon.stopped` live;
- the stream continues without gaps after a stop or a restart.

The daemon-side resilience that belongs to the boot and shutdown sequences lands with the runtime in task 5: unclean-shutdown and boot-failed events, boot cancellation, the shutdown budget, and home refusal.

**Slice 2 — task 7.** The CLI and process-level guarantees under races, crashes and timeouts:
- child error re-raise;
- start while draining, and stop on an unreachable daemon that still holds the lock;
- kill escalation;
- the hook-enabled subprocess crash suite.

**Slice 3 — tasks 8–9.** Operator tooling (doctor in both modes, `config` verbs, `version`), then the final delivery check: the full suites, Linux verification, operator docs and the roadmap.

| Task | Title | Type | Complexity | Depends on | Tests owned |
|---|---|---|---|---|---|
| task_01 | Repository foundation: module, gate, AGENTS.md, home, config, contract, logging | infra | medium | — | UT-001–UT-006, UT-010–UT-012, UT-014–UT-019, UT-080, UT-081, UT-090 |
| task_02 | Durable event ledger | backend | high | task_01 | UT-020–UT-029, UT-100–UT-108 |
| task_03 | Broker and stream.Follow | backend | high | task_02 | UT-030–UT-034, UT-060–UT-064, UT-066, UT-110, UT-111 |
| task_04 | Local API, admission gate and SSE adapter | backend | medium | task_01, task_03 | UT-050–UT-057, UT-065, UT-067–UT-069, UT-112–UT-117 |
| task_05 | Daemon runtime: boot, shutdown and the daemon stream | backend | high | task_04 | UT-013, IT-001–IT-006, IT-030–IT-035, IT-041–IT-043, IT-045–IT-047, IT-051 |
| task_06 | CLI: daemon start and stop, status and events | feature | high | task_05 | UT-040–UT-049, UT-120, UT-125, UT-127–UT-131, UT-133, E2E-001, E2E-002, E2E-004–E2E-008, E2E-011, E2E-013, E2E-015–E2E-017, E2E-023, E2E-024 |
| task_07 | Resilient start and stop under races, crashes and timeouts | backend | high | task_06 | UT-121–UT-124, UT-126, IT-020–IT-027, IT-048–IT-050, IT-052–IT-056, E2E-014, E2E-021, E2E-022 |
| task_08 | Doctor in daemon and local mode, config verbs and version | feature | medium | task_06 | UT-070–UT-076, UT-132, IT-040, E2E-003, E2E-009, E2E-010, E2E-012, E2E-018–E2E-020 |
| task_09 | Final verification, Linux run, operator docs and roadmap | qa | low | task_07, task_08 | no new IDs; runs every suite and records the evidence |

## Graph notes

- The chain 1 → 2 → 3 → 4 → 5 → 6 follows the contracts: ledger types → follow → API → daemon wiring → CLI. Each task consumes the previous one's interfaces as the spec defines them, so no task invents a contract another task owns.
- Tasks 7 and 8 both build on the CLI of task 6 and touch disjoint files: task 7 owns the start/stop resilience code and the test-hooks build tag; task 8 owns `internal/doctor` and the `doctor`, `config` and `version` verbs. They can run in parallel.
- Task 9 owns no test IDs. It runs the unit, integration and E2E suites together, verifies on Linux, and records the two open technical checks. That is where integration evidence is collected once.
- **Open technical checks:**
  - Go's default socket unlink-on-close: task 5 asserts it with IT-051.
  - `fullfsync` honoured by `modernc.org/sqlite`: task 2 asserts it with UT-029.

  Task 9 records both outcomes.
