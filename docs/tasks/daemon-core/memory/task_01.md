# Task 1 memory

## Objective Snapshot

Repository foundation, completed 2026-10-28:
- the module, the gate, `AGENTS.md`/`CLAUDE.md` and `mise.toml`;
- the `version`, `home`, `config`, `api/contract`, `redact`, `logging` and `archtest` packages;
- the `testhome` helper.

## Important Decisions

- `go.mod` declares `go 1.27`; `mise.toml` pins `go = "1.27"` (the user added it before the task).
- `config.Duration` keeps the operator's text, so `config show` echoes `"45s"` instead of `"45s0ms"`. Parsing and range checks happen in validation, so every error is collected.
- A wrong TOML type aborts decoding in BurntSushi/toml and is reported as one `parse_error`. Unknown keys, invalid values and out-of-range values are collected together.
- The `CheckSecurity` hint is `chmod 700 <home>` for mode problems, and a specific instruction for a symlink, a non-directory or a foreign owner.
- The redaction rules handle Authorization headers, bearer tokens, assignments (`password|passwd|token|secret|api_key`) and URL userinfo. The logger also redacts error values, and stringifies any non-error `Any` value before redacting it.

## Learnings

- The lumberjack v2.2.1 log file is created with mode 0600, and an existing file is opened without changing its mode.

## Files / Surfaces

- New: `Makefile`, `.gitignore`, `AGENTS.md`, `CLAUDE.md`, `go.mod`, `go.sum`, `cmd/harness/main.go`, `internal/{version,home,config,api/contract,redact,logging,archtest,testutil/testhome}`.

## Errors / Corrections

- The UT-005 test first used `[daemon\n`. Fixed to the contract's exact input `[daemon`.
- The stub `main` imported `internal/version`, which the architecture test rejected. Fixed to import nothing internal.

## Ready for Next Run

**Verification evidence (macOS, Go 1.27.3):**
- `make gate`: gofmt, `go vet ./...`, `go test -race ./...` and the build, all green.
- `go test -race -count=3 ./internal/home/ ./internal/archtest/`: green.
- After the runs, `~/.harness` did not exist and no `/tmp/hz*` directories were left.
