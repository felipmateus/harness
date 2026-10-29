---
status: completed
title: "Task 9: Final verification, Linux run, operator docs and roadmap"
type: qa
complexity: low
---

# Task 9: Final verification, Linux run, operator docs and roadmap

## Overview

This task closes `daemon-core`:
- it runs every suite together on macOS and on Linux;
- it records the spec's two open technical checks;
- it writes the operator README from `_dx.md`;
- it marks feature 1 done in the roadmap.

It owns no test IDs. It owns the evidence that the owned tests pass together.

## Shippable Outcome

- **Outcome.**
  - A clean checkout passes `make gate` plus the integration and E2E suites (with and without `harness_testhooks`) on macOS, and on Linux if available.
  - The README lets a new operator run the Golden Path.
  - `docs/roadmap.md` marks feature 1 done.
- **Verify in this task:** the full-suite runs and the README walkthrough, recorded in the task memory.
- **Integration verification:** this task is the integration owner.

## Requirements

**Runs:**
- **macOS:** `make gate`, `go test -race -tags integration ./...`, `go test -tags e2e ./e2e/...`, and the `harness_testhooks` variants of the last two.
- **Linux:** the same, in a container if one is available. If none is, record Linux as **unverified** with the reason; do not claim it.

**Record in the task memory:**
- The IT-051 result: does Go unlink the socket on listener close by default?
- The UT-029 result: does `fullfsync` read back as `1` on darwin?
- If either contradicts the spec's assumption, report it to the user before closing.

**README** (`README.md`): install from source, the Golden Path, `HARNESS_HOME`, the error table pointer, and the Linux `KillUserProcesses` caveat from Known Risks.

**Ask the user** before any commit or push.

## Subtasks

- [x] 9.1 Run the full suites on macOS and record the commands and outcomes. All six Makefile suites plus the architecture test passed on the final tree; see `memory/task_09.md` › macOS verification.
- [x] 9.2 Run the full suites on Linux, or record why it was not possible. **Linux is unverified:** Docker's CLI is installed at `/usr/local/bin/docker`, but the Docker daemon is not running and no image is available locally, and starting Docker Desktop or pulling an image would be an install, which `AGENTS.md` forbids without the user's permission. The exact commands for a later run (`golang:1.27`, repo and module cache mounted read-only, `GOFLAGS=-mod=mod GOPROXY=off`, the same make targets) are in `memory/task_09.md` › Linux and in the README.
- [x] 9.3 Record the two open technical checks. IT-051: Go does unlink a Unix socket on `Close` by default. UT-029: `fullfsync` reads back `1` on darwin. Both match the spec; see `memory/task_09.md`.
- [x] 9.4 Write `README.md` from `_dx.md`, and walk through it once on a temporary home.
- [x] 9.5 Update `docs/roadmap.md` (feature 1 done).

## Implementation Details

### Relevant Files

- `README.md` — new.
- `docs/roadmap.md` — the feature 1 status row.
- `docs/tasks/daemon-core/memory/task_09.md` — the evidence.

## Deliverables

- The full-suite evidence (macOS, and Linux or its documented absence).
- `README.md`.
- The updated roadmap.

## Tests

- No new IDs. Every ID in `_tests.md` is owned by tasks 1–8. This task reruns them together and records the results.

## Success Criteria

- Every suite is green together on macOS, with the commands listed.
- Linux is verified, or explicitly listed as unverified.
- Both open technical checks are answered.
