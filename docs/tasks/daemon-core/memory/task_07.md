# Task 7 memory

## Objective Snapshot

Resilient start and stop under races, crashes and timeouts, completed 2026-10-28 (slice 2):
- the `--detached` child writes its final `{"error":…}` line, and the CLI reads it back (re-raise, `daemon_already_running` loop, `daemon_start_failed` details);
- the start decision table, waiting out a starting or draining daemon, bounded by `--timeout`;
- kill escalation on a start timeout (SIGTERM, 2 s, SIGKILL, reap);
- stop on a daemon that holds the home but does not answer (`uptime_seconds: null`);
- the `harness_testhooks` build tag with the `HARNESS_TESTHOOKS` parser, and the process and crash suite.

## Important Decisions

- **The final error line is written by `daemon.Run`, for every `*ExitError` it returns in `Detached` mode** (`internal/daemon/detached.go`, called from `Run` after `d.run()` returns, so after the undo and after the last mirrored log line). That includes a shutdown overrun, because the spec says "the final `*ExitError`". The CLI's `--detached` child then exits through `silentExit{code}` (`exitcodes.go`) and prints nothing more, so the JSON line is the last line of `daemon.out`. A cancelled boot and a clean stop return nil and write nothing. Interactive mode never writes it.
- **`silentExit` is a reusable CLI error type** (an exit code whose message was already written elsewhere). `a.run` lets it through and `report` returns its code with no output. Task 8 may use it for the doctor (70) and `config validate` (78) exits.
- **Start decision table = `settleHome` (`internal/cli/start.go`).** One loop that probes the status every 100 ms:
  - `running` -> `already_running`, nothing spawned;
  - `starting` -> wait (another start is in progress);
  - `draining` (or any other state) -> print `waiting for the previous daemon (pid N) to exit` once on stderr (human mode only), wait;
  - unreachable and the lock held (a daemon booting before listen, or exiting) -> the same line and wait;
  - unreachable and the lock free -> spawn.
  Because the loop re-probes the status on every pass, the home that is held by a daemon that finishes booting ends in `already_running` for the right PID, never in a wait for a lock that will not free (needed by IT-022: five starts, four report `already_running`). This goes one step beyond the spec text ("poll until `LockFree`"), which would wait out the timeout for a daemon that stays up.
- **Wait timeouts are `daemon_start_timeout` with no details** (nothing was spawned, there is no output to show): `previous daemon (pid N) did not exit within <d>` and `daemon (pid N) did not become ready within <d>`; hint `see <log>`. A start that keeps losing races to a free-looking lock gives up with `daemon did not become ready within <d>`.
- **`daemon_already_running` from the child sends the start back to `settleHome`** (`errHomeOwned`, never shown). The other recognised codes (`config_invalid`, `home_insecure`, `socket_path_too_long`, `ledger_schema_ahead`) are re-raised with their own message, hint and exit code and no details. The last `{"error":{"code":…}}` line after the recorded offset decides; log records have no top-level `error` key, so they are never mistaken for it (`childerror.go`).
- **Kill escalation (`stopChild`)**: SIGTERM to the group, `waitExit(termGrace = 2 s)`, SIGKILL to the group, `waitExit(killGrace = 5 s)` for the reap. `waitExit` checks `Done` before and after the timer, so a child that already exited is never killed (the fake clock's `After` fires eagerly, so a plain `select` would race). `killGrace` is mine: it only bounds a process stuck in the kernel so the CLI cannot hang.
- **Stop on an unreachable daemon that holds the lock (`stopUnreachable`)**: waits `defaultGrace + 5 s` (35 s) for `LockFree`, then reports `stopped` with the PID read from the lock file and `uptime_seconds: null`. It re-sends the shutdown request on every poll: a daemon that was booting and gets to its socket meanwhile is told to stop (the boot is cancelled) instead of being waited on until the timeout; the normal wait then follows with its reported grace. `stoppedDaemon` now always carries `pid` and `uptime_seconds` (`null` when unknown; `pid` is `null` too if the lock file has none), `notRunningDocument` is the other shape. Human forms: `harness daemon stopped (pid N)` and `harness daemon stopped`.
- **`Hooks.BlockExit` lives in the daemon, not in a test-hooks `main`** (this changes the plan in `memory/task_05.md`). IT-053 needs the home to stay locked after the shutdown "never completes"; a `main` that blocks after `Run` returns would see the lock already released and `stop` would succeed. With `BlockExit` the lock release (shutdown step 9 and the forced path) is skipped and `run()` blocks forever after the shutdown, so only a kill ends the process.
- **`HARNESS_TESTHOOKS`** (`testhooks_env.go`, `//go:build harness_testhooks`; `testhooks_noenv.go` is the production stub): a comma-separated list, `hold-boot-step=N` (1..12), `fail-boot-step=N`, `hold-shutdown-step=N` (1..10), `block-exit`. Holds block forever (`blockForever` sleeps in a loop, so the runtime deadlock detector cannot end the process); a held boot is ended by a stop request, a held shutdown step by the budget or a second signal. An unknown hook, a missing or out-of-range step is a failed `Run` (`internal`, exit 1) before the boot, so a mistyped hook fails a test instead of running without it. `Options.Hooks` wins over the environment.
- **The process and crash suite is in `internal/daemon`** (`process_integration_test.go`, `process_helpers_integration_test.go`, `//go:build integration && harness_testhooks`), run by `make test-integration-hooks`. `hookBinary` builds `./cmd/harness` once with `-tags harness_testhooks`; every subprocess gets `HARNESS_HOME` and a fake `HOME`; `procHome` stops the home at cleanup and kills (and fails the test for) a daemon that will not stop. The `TestMain` of the package now runs registered cleanups after `m.Run()` (`registerCleanup`).
- **IT-050 CLI form**: the daemon runs in-process (the test needs its `Appender`), and the real `harness daemon stop` runs as a subprocess against it. 300 events of 4 KiB instead of 10,000: a commit costs about 6 ms here, and 300 outgrow the socket buffers on macOS and Linux (as task 5 found).
- **IT-048/049 "hold released"** means the budget expiring: `hold-shutdown-step=3` blocks forever, `shutdown_grace=2s` forces the exit, the lock is released and the waiting stops/start proceed.
- **E2E**: `e2e/main_test.go` builds the binary with `-tags harness_testhooks` when the suite is compiled with it (`hooks_on_test.go`/`hooks_off_test.go` define `buildTags`), so `-tags "e2e harness_testhooks"` runs the whole suite against the hook-enabled binary. E2E-014, 021 and 022 are in `e2e/resilience_test.go` and need no hook.
- **Makefile**: `test-hooks`, `test-integration-hooks`, `test-e2e-hooks` added; `AGENTS.md` names them. `make gate` is unchanged (no tag).

## Learnings

- **`rm -f $S/v_*.log && …` under zsh aborts the whole line when the glob matches nothing** (`no matches found`): a verification script that was "started" never ran. Check that a background job exists before waiting on it.
- **The CLI's `LockFree` probe takes the flock for an instant**, so a daemon child that tries the lock at that moment loses with `daemon_already_running` and an unknown PID. The `errHomeOwned` loop is what makes this harmless; the foreground retry of task 5 covers the interactive case.
- **A fake clock whose `After` fires eagerly makes `select { Done; After }` racy** when `Done` is already closed; always test `Done` first (`waitExit`).
- **`syscall.Getpgid` is enough to show a daemon leads its own session** (pgid == pid, different from the starter's group) on both macOS and Linux; `Getsid` is not needed.

## Files / Surfaces

- New: `internal/cli/childerror.go`, `internal/daemon/{detached.go,testhooks_env.go,testhooks_noenv.go}`.
- Changed: `internal/cli/{start.go,stop.go,spawn.go,exitcodes.go,root.go}`, `internal/daemon/{run.go,shutdown.go,hooks.go,main_test.go}`, `Makefile`, `AGENTS.md`, `e2e/main_test.go`.
- New tests: `internal/cli/{resilience_test.go,spawn_test.go}` (UT-121–UT-124, UT-126 plus the cases below), `internal/daemon/{detached_test.go,testhooks_env_test.go}`, `internal/daemon/process_{helpers_,}integration_test.go` (IT-020–027, 048–050, 052–056), `e2e/{resilience_test.go,hooks_on_test.go,hooks_off_test.go}` (E2E-014, 021, 022).
  - Beyond the assigned IDs: an old error line before the offset, the last line wins, a free-looking home respawn, the loop bounded by the timeout, the human-only waiting line, wait timeouts in both modes, unreachable then running, a stuck-in-the-kernel child, the unknown PID forms, a boot that reaches its socket during a stop, the silent detached child, the real spawner killing a SIGTERM-proof process group (about 2 s), the hooks parser, `Run` reading the environment.
- No new module dependency. `internal/cli` and `internal/daemon` import nothing new (architecture test green).

## Open items for the conductor

- **Ctrl-C during `daemon start`** leaves the spawned child running (it is in its own session and the CLI returns 130). The spec is silent; stopping it would be a one-line `stopChild` in `awaitChild`. Not done: out of the task's scope.
- **`--timeout` can still be overshot** by one 2 s status request (task 6's note); the checks run after each poll.
- **Two concurrent `stop`s with no daemon** can, in a microsecond window, see the other's lock probe as a held lock and print `stopped (pid unknown)` instead of `not running`. Harmless; a retry of the probe would close it.
- **A shutdown overrun in a detached daemon** also writes the error line to `daemon.out` (spec: the final `*ExitError`). Nobody reads it; the log has the record.
- **Task 9:** run the new suites on Linux (`make test-integration-hooks`, `make test-e2e-hooks`); the process tests use `/tmp` homes, `sh`, `sleep` and `syscall.Getpgid`. IT-053 needs about 7 s of real time.

## Errors / Corrections

- **A mutation experiment left one stuck test daemon behind.** Mutation M17 (the CLI no longer stops the child on a start timeout) made `TestIT056` fail the way it should, and the daemon that test had started, held at boot step 10 by `hold-boot-step=10`, stayed alive: pid 88714, started 23:34:34, `.../harness-it-bin*/harness daemon start --foreground --detached --home /tmp/hz3174794282`. Its home directory is gone (the test removed it), it holds no listener the operator uses, and it is not part of any passing run. The permission classifier denied my `kill -9 88714`, so it is still running: the user can end it with `kill 88714`. No suite run leaves a process: after the final verification no `harness` process other than this one exists, no `/tmp/hz*` or `harness-it*`/`harness-e2e*` directory is left, and `~/.harness` does not exist.
- **Two mutations first "failed" only by not compiling** (an unused variable, and vet's redundant `|| true` check). They were redone as compile-safe mutations (M7, M13) and killed by `TestUT126_*`.
- **A verification script that never started** (zsh `no matches found` on `rm -f v_*.log` aborted the `&&` chain) cost a 10-minute wait; see Learnings.

## Ready for Next Run

**Verification evidence (macOS 15, Darwin 25.6, arm64, Go 1.27.3):**
- `make gate`: green (gofmt, `go vet ./...`, `go test -race ./...`, build).
- `go test -race -count=1 ./internal/archtest/`: green.
- `make test-integration`: green (`internal/daemon` 51.7 s).
- `make test-e2e`: green (about 6 s).
- `make test-hooks` (`go test -race -tags harness_testhooks ./...`): green.
- `make test-integration-hooks` (`-tags "integration harness_testhooks"`): green (`internal/daemon` 79 s; the process and crash suite alone takes about 24 s).
- `make test-e2e-hooks` (`-tags "e2e harness_testhooks"`): green (about 5 s), every e2e test against the hook-enabled binary.
- Mutations applied one at a time and reverted, all killed: draining reported as running, no SIGKILL, schema-ahead not re-raised, tail offset ignored, stop does not wait for the lock, waiting line in json mode, waiting line on every poll, stop does not re-send the shutdown, `daemon_already_running` treated as a failure, the first error line winning, the silent detached child printing, no final error line, the final line in interactive mode, `block-exit` releasing the lock, the child not stopped on a start timeout.
- By hand: `harness daemon start`, `stop`, a foreground `--detached` child stopped by `stop`, and an invalid `config.toml` (exit 78, the JSON line last in `logs/daemon.out`) on `HARNESS_HOME=/tmp/hzm.XXXXXX` with `HOME` set to the same directory, in single commands; `~/.harness` did not exist before or after.

**For task 8:** `silentExit` (`internal/cli/exitcodes.go`) is the way to exit with a code and no error output. Nothing else in this task touches `doctor`, `config` or `version`.

**For task 9:** run `make test-integration-hooks` and `make test-e2e-hooks` on Linux; see Open items.
