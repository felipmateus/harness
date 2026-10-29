# harness

`harness` is a local Go daemon that runs agent CLIs over ACP and records everything in a durable event ledger. Feature specs live in `docs/tasks/<slug>/`; the epic and feature order are in `docs/roadmap.md`.

## Working rules

- **Language.** Conversation with the user is in Brazilian Portuguese. Everything written to this repository is in English: code, identifiers, comments, docs, specs, test names, log messages, CLI output and commit messages.
- **Ask first.**
  - Ask the user before committing, pushing, or installing anything on the machine.
  - Never use destructive Git commands (`reset`, `checkout --`, `clean`, `stash`) without explicit permission.
- **Scope.** Work within the current task file (`docs/tasks/<slug>/task_NN.md`) and its spec. Record follow-up work instead of widening scope.
- **Never touch the operator's real home.** Tests use temporary homes only (`internal/testutil/testhome`). No test, script or tool may read or write `~/.harness`.

## Toolchain

- Go 1.27. The repository pins it in `mise.toml` (`mise install` gives the right version); a system Go 1.27 works too.
- Dependencies are added with `go get`; keep `go.mod` tidy.

## Verification

- `make gate` must pass before a task is marked complete. It runs `gofmt` check, `go vet`, `go test -race ./...` (which includes the architecture test) and the build.
- Integration tests: `make test-integration` (`//go:build integration`).
- E2E tests: `make test-e2e` (`//go:build e2e`).
- Subprocess tests that need daemon hooks build with `-tags harness_testhooks`: `make test-hooks`, `make test-integration-hooks` (the process and crash suite runs only here) and `make test-e2e-hooks`.
- Fix production causes; never weaken a test to make it pass.

## Architecture

- `internal/daemon` is the only composition root.
- Packages depend only on what the boundaries table in `docs/tasks/daemon-core/_spec.md` › *Architectural Boundaries* allows. `internal/archtest` enforces it on direct imports.
- Consumers declare the small interfaces they use (ADR-003). Ports exist only at real variation edges: the ACP agent runtime and loop action execution.
- Production code never imports `internal/testutil`.

## Conventions

- One error envelope on the wire: `{"error":{"code","message","hint"?,"details"?}}`. Codes and messages live in `internal/api/contract`.
- Files the daemon creates are 0600, directories 0700.
- Times are UTC with exactly three fractional digits.
