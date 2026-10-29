---
status: completed
title: "Task 1: Repository foundation: module, gate, AGENTS.md, home, config, contract, logging"
type: infra
complexity: medium
---

# Task 1: Repository foundation: module, gate, AGENTS.md, home, config, contract, logging

## Overview

This task turns `~/Projetos/Pessoal/harness` into a Go repository. It adds the leaf packages every later task consumes:
- home handling (paths, socket-path limit, home security, lock rules, atomic `daemon.json`);
- strict config;
- the API contract with its shared validation and error codes;
- redaction and logging;
- build info.

It also installs the delivery gate and the architecture test, so the boundaries hold from the first line of code.

Consumers:
- task 2: the module and the gate;
- task 4: `api/contract`;
- task 5: `home`, `config`, `logging`;
- task 6: `contract`, `home`, `version`.

## Shippable Outcome

- **Outcome.**
  - `go build ./...` and `make gate` succeed on macOS.
  - `go build ./cmd/harness` produces a binary; its `main` is a stub until task 6.
  - The architecture test enforces the full boundaries table, including rows for packages that do not exist yet.
- **Verify in this task:** the assigned unit tests, `make gate`.
- **Integration verification:** task 9 runs every suite together.

## Requirements

**Contracts:**
- `_spec.md` › Architectural Boundaries (the direct-import table and the transitive rule).
- `_spec.md` › Core Interfaces › `internal/home`.
- `_spec.md` › Data Models: Config model, Files in the home, API contract types.
- `_spec.md` › Business Rules 1–4 (lock), 21–25 (validation), 26–28 (home and permissions).
- `_dx.md` › Errors: the codes and messages `api/contract` defines.

**Behaviour that must hold:**
- **Lock rules (invariant 2).**
  - `AcquireLock` opens `daemon.lock` with `O_CREATE|O_RDWR` and 0600, never `O_TRUNC`.
  - Only after `TryLock` succeeds does the owner truncate the file and write its PID in place.
  - `LockFree` probes with `LOCK_EX|LOCK_NB` on a read-only descriptor and releases at once.
  - The lock file is never removed or renamed.
- **Socket-path limit:** at most 103 bytes on darwin and 107 on linux, measured on the cleaned absolute path. The message is exactly the `_dx.md` one.
- **`CheckSecurity`:** use `lstat`. Refuse a symlink, a foreign owner, or group/other write; accept 0755.
- **Config:** decoding collects every error (`MetaData.Undecoded()` → `unknown_key`), and the range for `shutdown_grace` is 1s–5m.
- **Contract validation:** `ValidateStream` (`^[a-z][A-Za-z0-9_.:-]{0,127}$`), `ValidateAfter` and `ValidateLimit` return the exact `_dx.md` messages. The API (task 4) and the CLI (task 6) both call them.
- **Logging:** slog JSON through a redacting handler; lumberjack at 10 MB, 5 backups, 30 days; a stderr mirror that can be switched off at runtime.
- **AGENTS.md** records the project rules:
  - English only;
  - Go 1.27;
  - the gate command;
  - the boundaries table (by reference to the spec);
  - test layers and build tags;
  - temporary homes only in tests;
  - ask before commit, push or installs;
  - conversation in Brazilian Portuguese, artifacts in English.

  `CLAUDE.md` points to `AGENTS.md`.

## Subtasks

- [x] 1.1 Check `go version`.
  - If Go 1.27 is missing, stop and ask the user to install it (e.g. `brew install go`). Do not install it yourself.
  - Ask the user before running `git init` in the repository.
- [x] 1.2 Create `go.mod` (`github.com/felipmateus/harness`, `go 1.27`), `cmd/harness/main.go` (stub), the `Makefile` with `gate`, and `.gitignore`.
- [x] 1.3 Write `AGENTS.md` and `CLAUDE.md` with the project rules above.
- [x] 1.4 Implement `internal/version`.
- [x] 1.5 Implement `internal/home`:
  - `Resolve`, `CheckSocketPath`, `CheckSecurity`, `EnsureLayout`;
  - `AcquireLock` with the lock rules, and `LockFree`;
  - `WriteInfo` (atomic: temp file, fsync, rename, directory fsync).
- [x] 1.6 Implement `internal/config`: defaults, strict decode, collected validation, digest.
- [x] 1.7 Implement `internal/api/contract`: DTOs, error codes, `ErrorBody` with `details`, and the shared validation functions.
- [x] 1.8 Implement `internal/redact` and `internal/logging`.
- [x] 1.9 Implement `internal/archtest` covering every row of the boundaries table. A row whose package does not exist yet passes vacuously.
- [x] 1.10 Add the `testhome.New(t)` helper: a short `/tmp/hz…` directory at 0700, removed on cleanup.

## Implementation Details

See `_spec.md` Part II › Core Interfaces (`internal/home`) and › Data Models.

### Relevant Files

- `go.mod`, `Makefile`, `.gitignore`, `AGENTS.md`, `CLAUDE.md` — new.
- `cmd/harness/main.go` — new stub; task 6 wires the CLI.
- `internal/version/version.go` — build info via `-ldflags -X` and `runtime/debug`.
- `internal/home/{home.go,lock.go,security.go,info.go}` — the home package.
- `internal/config/config.go` — the config model, `Load` and validation.
- `internal/api/contract/{types.go,errors.go,validate.go}` — DTOs, codes and shared validation.
- `internal/redact/redact.go`, `internal/logging/logging.go` — redaction and the log handler.
- `internal/archtest/archtest_test.go` — the boundary rules.
- `internal/testutil/testhome/testhome.go` — the test helper.

### Dependent Files

- Every later task imports these packages. Their exported shapes must match `_spec.md` exactly.

### External References

- `~/Projetos/Pessoal/compozy/internal/daemon/lock.go:57-106` — non-blocking flock singleton with the PID recorded; mirror it, but the lock decides.
- `~/Projetos/Pessoal/compozy/internal/daemon/lock.go:170-182` — PID written in place only after `TryLock`.
- `~/Projetos/Pessoal/compozy/internal/daemon/info.go:74` — atomic info file.
- `~/Projetos/Pessoal/compozy/internal/logger/logger.go:70-135` and `~/Projetos/Pessoal/compozy/internal/logger/redacting_handler.go:15-31` — slog JSON, rotation, redacting handler.
- `~/Projetos/Pessoal/compozy/internal/redact/redact.go:12-38` — redaction patterns.

### Related ADRs

- [ADR-002](adrs/adr-002.md) — the socket-path limit and the private-home requirement.
- [ADR-003](adrs/adr-003.md) — consumer-declared interfaces and direct-import boundaries.

## Deliverables

- A buildable Go module with the gate.
- `AGENTS.md` and `CLAUDE.md`.
- The `version`, `home`, `config`, `api/contract`, `redact`, `logging` and `archtest` packages, and the `testhome` helper.

## Tests

- [x] UT-001, UT-002, UT-003, UT-004, UT-005, UT-006 — config decoding, defaults, collected errors, ranges.
- [x] UT-010, UT-011, UT-012, UT-014, UT-015, UT-016, UT-017, UT-018, UT-019 — home: socket-path limit, layout, lock acquire, atomic info, precedence, security, lock-file rules, lock probe, cleaned paths.
- [x] UT-080, UT-081 — redaction and the redacting handler.
- [x] UT-090 — architecture test over the whole boundaries table.

## Success Criteria

- `make gate` is green on macOS.
- `AGENTS.md` exists and states the rules listed in Requirements.
- No test touches the real `~/.harness`.
