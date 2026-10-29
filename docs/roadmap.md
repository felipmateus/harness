# Harness roadmap

| | |
|---|---|
| Epic | **Harness MVP**: a local Go daemon that runs agent CLIs over ACP and executes the Compozy spec-cycle flow durably. It is also our lab for studying context engineering. |
| Language | Go (decided 2026-10-28) |
| Architecture | Idiomatic Go: components with consumer-side interfaces and one composition root. **Ports only at real variation edges**: the ACP agent runtime and loop action execution. Shared logic is extracted as plain functions, e.g. `stream.Follow` ([ADR-003](tasks/daemon-core/adrs/adr-003.md)). |
| Reference | `specs/compozy_reverse_spec.md`: reverse spec of CompozyOS v0.4.2, cited below as `OBS-*` |
| Process | One feature = one spec folder `docs/tasks/<slug>/`, made with `cy-create-spec` → `cy-create-tasks` → implementation. Each feature is specified only when it is next, not all up front. |

## Why this order

The epic's motivating problem is to run the **full spec-cycle flow** from our own daemon:

```
cy-create-spec → cy-create-tasks → implement-tasks loop → review-and-fix loop → cy-final-verify
```

The table shows what each step needs from a daemon. In Compozy these capabilities are provided by `extensions/spec-cycle` together with the Loop engine.

| Step | Needs from the daemon | Feature |
|---|---|---|
| `cy-create-spec`, `cy-create-tasks`, `cy-final-verify` | Nothing. These are authoring skills and already run in Claude Code. | — |
| `implement-tasks`, per-task mode (Compozy's default) | `input`, `route`, `fan-out`, `transform` and `collect` nodes; `run-agent` with an isolated session, `output_schema`, retry and timeout; a built-in `import_tasks` action; agent definitions | 1, 2, 3, 4 |
| `review-and-fix` | `run-agent` returning `issues[]`; `branch`; the `write_review_artifacts` and `finalize_review_round` actions; `stop_when` (CEL), `iteration_cap`, `no_progress`, budget | 1, 2, 3, 4 |
| `implement-tasks`, orchestrated mode (optional) | `goal` node with judges; spawning workers with TTL, auto-stop and idempotency; prompt/stop/list sessions; delegated tool IDs | 6, 7 |

Features 1–4 deliver **Milestone A: the full flow automated**. The context-engineering study (feature 5) comes right after, because it changes how agents are briefed, not whether the flow runs.

## Features

| # | Slug | Outcome | Reverse-spec scope | Depends on | Status |
|---|---|---|---|---|---|
| 1 | `daemon-core` | One `harness` binary runs a single local daemon per home, reachable over a Unix socket, with a durable, replayable event ledger and `status`/`doctor` | DMN, STO, API (UDS only), CLI (core verbs), CFG (minimal), logging and redaction | — | **done** (2026-10-29). Spec approved and revised with review-001; 9 tasks done; every suite green on macOS and, in a `golang:1.27` container, on Linux arm64 (2026-10-29; see `README.md` and `docs/tasks/daemon-core/memory/task_09.md`) |
| 2 | `acp-sessions` | Start a Claude session over ACP, prompt it, stream its events into the ledger, and cancel or stop it safely. Includes AGENT.md definitions, basic permission modes, and a fake ACP agent for tests | PRV, ACP, SES, PERM, CFG-007/009 | 1 | planned. **Spec must define the `AgentRuntime` port** with two adapters: ACP over stdio (provider command) and the fake ACP agent (ADR-003). Session streams `session:<id>` reuse `stream.Follow`. |
| 3 | `loop-engine` | Run a YAML Loop: input/route/branch/fan-out/collect/transform/run-agent/built-in actions; CEL conditions; output schema with one repair; retry and timeout; `stop_when`, iteration cap, no-progress and budget; a pure planner with an atomic commit | LOOP, TASK-001 | 2 | planned. **Spec must define the `ActionExecutor` port** with a registry keyed by action kind (`run-agent`, built-in actions, judges), so `spec-cycle-runtime` only registers adapters (ADR-003). It watches `run-agent` sessions with `stream.Follow`. |
| 4 | `spec-cycle-runtime` | Built-in `import_tasks`, `write_review_artifacts` and `finalize_review_round` actions, plus ported `implement-tasks` (per-task) and `review-and-fix` loops and their agents, all working on `docs/tasks/<slug>/` | EXT-006 (as built-ins, no extension protocol) | 3 | planned |
| — | **Milestone A** | `harness loop run implement-tasks --input slug=<x>`, then `review-and-fix`, complete a real spec end to end | | 1–4 | |
| 5 | `context-assembly` | Ordered, budgeted prompt sections; section delta stubs that reset after compaction; provenance labels; bounded replay | CTX | 2 | planned (study focus) |
| 6 | `goal-judges` | `goal` node: turns from a checkpoint, command judges after every turn, worst-outcome aggregation, fenced diagnostics | GOAL | 3 | later |
| 7 | `subagents` | Spawn with lineage, TTL, auto-stop-on-parent, idempotency and permission subset; enables orchestrated mode | SUB | 2, 6 | later |
| 8 | `hosted-tools` | One hosted MCP server, a single dispatch funnel, result budget with artifact offload | TOOL | 2 | later |
| 9 | `skills-catalog` | Skill catalog with on-demand bodies, for providers that do not load skills natively | SKL | 5, 8 | later |
| 10 | `hooks` | Typed `pre_*` hooks that can only deny or narrow | HOOK | 2, 8 | later |

**Out of the epic:**
- tasks/scheduler/automation;
- extensions protocol;
- human gates;
- HTTP surface and web UI;
- worktrees;
- marketplace.

Revisit these after Milestone A.

## Notes

- Skill bodies do not need the daemon for Claude. The Claude ACP adapter is expected to load `~/.claude/skills` natively; Compozy suppresses natively loaded skills from its own catalog (OBS-SKL-004). This must be confirmed in `acp-sessions` with the default (operator) home policy.
- **Prerequisite:** the Go toolchain is not installed on the development machine yet (checked 2026-10-28). Compozy pins Go 1.27.1 via `mise`.
