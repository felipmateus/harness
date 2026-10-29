---
status: completed
title: "Task 8: Doctor in daemon and local mode, config verbs and version"
type: feature
complexity: medium
---

# Task 8: Doctor in daemon and local mode, config verbs and version

## Overview

This task adds the operator tooling:
- **`harness doctor`:** runs inside the daemon when one answers, and locally otherwise, so it works when the daemon cannot start.
- **`harness config path|validate|show`:** works without a daemon.
- **`harness version`.**

Reports (`doctor` exit 70, `config validate` exit 78) follow the report output rule: the document goes to stdout.

## Shippable Outcome

- **Outcome.** The `_dx.md` sections `doctor`, `config` and `version` run as written, with and without a daemon.
- **Verify in this task:** the assigned unit, integration and E2E tests.
- **Integration verification:** task 9.

## Requirements

**Contracts:**
- `_dx.md` › `harness doctor`, › `harness config`, › `harness version`, › output rules (reports).
- `_spec.md` › Component table (`internal/doctor`: seven probes, two modes, 2 s per probe).
- `_spec.md` › Config Lifecycle.
- `_user_stories.md` US-007 and US-011.

**Behaviour that must hold:**
- **Probes:** `home.permissions`, `socket.permissions` (daemon mode), `daemon.reachable` (local mode, `warn`), `config.valid`, `ledger.integrity`, `ledger.schema`, `log.writable`.
- **`config.valid`:**
  - **error** when the on-disk file is invalid;
  - **warn** when it is valid but its digest differs from the loaded config (daemon mode).
- **Local mode** uses `ledger.Inspect` (read-only), never `ledger.Open`.
- **No probe changes any file**, its mode or its mtime.
- **The doctor route** in `internal/api` (task 4) gets a real `doctorRunner` from `internal/daemon`.

## Subtasks

- [x] 8.1 Implement `internal/doctor`: the probe registry, the seven probes, both modes, per-probe timeout.
- [x] 8.2 Wire the daemon-mode runner into `internal/daemon` for `GET /api/doctor`.
- [x] 8.3 Add the `harness doctor` verb with the daemon-first, local-fallback logic and the report output rule.
- [x] 8.4 Add the `harness config path|validate|show` and `harness version` verbs.

## Implementation Details

See `_spec.md` Part II › System Architecture (`internal/doctor`) and › Config Lifecycle.

### Relevant Files

- `internal/doctor/{doctor.go,probes.go}` — new.
- `internal/daemon/doctor.go` — the runner wiring.
- `internal/cli/{doctor.go,config.go,version.go}` — new verbs.
- `e2e/tooling_test.go` — new.

### Dependent Files

- `internal/api/doctor.go` (task 4) consumes the runner through its `doctorRunner` interface.

### Related ADRs

- [ADR-003](adrs/adr-003.md) — `doctor` is imported by `daemon` and `cli`, never by `api`.

## Deliverables

- `internal/doctor`.
- The `doctor`, `config` and `version` verbs, with their tests.

## Tests

- [x] UT-070, UT-071, UT-072, UT-073, UT-074, UT-075, UT-076 — probes, permissions, no mutation, timeout, `config.valid` semantics, local mode, unreadable ledger.
- [x] UT-132 — report output rule for `doctor` and `config validate`.
- [x] IT-040 — doctor through the daemon on a fresh home.
- [x] E2E-003, E2E-009, E2E-010, E2E-012, E2E-018, E2E-019, E2E-020 — invalid config (start and validate), healthy doctor, loose home, config validate/show, version, config path, doctor without daemon.

## Success Criteria

- `harness doctor` gives a useful report when `harness.db` is unreadable and the daemon cannot start (UT-076).
- All assigned suites are green.
