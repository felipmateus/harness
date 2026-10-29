# Task 6 memory

## Objective Snapshot

`internal/cli`, `cmd/harness` and `e2e/`, completed 2026-10-28:
- the cobra command tree: `daemon start` (detached and `--foreground`, plus the hidden `--detached` for the child), `daemon stop`, `status`, `events` (list and `--follow`);
- the output rules and the exit-code table of `_dx.md`; usage errors in JSON under `-o json|jsonl`;
- the UDS HTTP client with its timeouts, and the SSE reader;
- tests: UT-040–UT-049, UT-120, UT-125, UT-127–UT-131, UT-133 (`internal/cli`), and E2E-001, 002, 004–008, 011, 013, 015, 016, 017, 023, 024 against the built binary (`e2e/`), plus extra cases listed under Files.

## Important Decisions

- **What was left to task 7 on purpose** (the task body and the dispatch prompt put these there), and where it plugs in:
  - **Start decision table.** `startDetached` (`start.go`) treats only `running` as already running. `starting`, `draining` and "locked but unreachable" fall through to the spawn; the child then fails on the lock and the CLI reports `daemon_start_failed`. Task 7 extends the `switch` right after the first `api.Status` probe.
  - **Child final error line.** `startFailed` (`start.go`) always reports `daemon_start_failed` (exit 69) with `details{pid, exit_code, output_tail}` (the last 20 lines after the recorded offset, UT-133). Task 7 parses the last `{"error":…}` line first and re-raises it, or loops to the probe for `daemon_already_running`. The `--detached` child itself prints errors like any CLI error (human `error:` lines), because `internal/daemon/detached.go` (the one-line JSON) is task 7's.
  - **Kill escalation.** `startTimedOut` sends SIGTERM to the child's group and waits up to 2 s for it to exit; there is no SIGKILL. The message still says "it was stopped". Task 7 adds the SIGKILL and the reap guarantee (UT-124, IT-056).
  - **Stop, unreachable but locked.** `stopDaemon` (`stop.go`) returns the `daemon_unavailable` error in that case. Task 7 adds the wait for `LockFree`, the PID from the lock file and `uptime_seconds: null` (UT-126). `stoppedDaemon` uses `omitempty` on `uptime_seconds` (a pointer), so task 7 has to emit an explicit `null` there.
  - **`harness_testhooks`** is not started.
- **Seams, all on the `app` struct (`root.go`):** `clock`, `newClient` (a `daemonAPI`), `spawner` (+ `process`), `probe` (`homeProbe`: `LockFree`, `LockPID`, `SocketExists`), `executable`, `runDaemon` (defaults to `daemon.Run`), `interrupt`. Unit tests replace all of them (`helpers_test.go`); production wires the real ones in `newApp`.
- **Fake clock (`helpers_test.go`).** `After` in auto mode advances virtual time by itself, so polling loops and timeouts run instantly and the test asserts elapsed virtual time (UT-125's 6 s, UT-042's 400 ms). Manual mode (`Advance`) drives the 45 s follow idle deadline (UT-130). `After` advances eagerly when it is *called*, so a `select` that also waits on a process exit still costs the full wait in virtual time; tests assert the time of the event (for example when SIGTERM was sent), not the end time.
- **Output mode for errors comes from the raw arguments** (`outputModeFromArgs`), because cobra stops parsing at the first bad flag and `-o json` may come after it (UT-129). Success output uses the parsed flag (`a.mode`, set in `PersistentPreRunE`).
- **Usage errors in human mode** print `error: <message>`, a blank line and cobra's usage text; the hint line is left out because the usage text already is the help. Under json/jsonl they are `{"error":{"code":"usage","message":…,"hint":"run `harness <verb> --help`"}}`.
- **Every error a command returns is typed** (`run` in `root.go` wraps untyped ones as `internal`), so an untyped error out of cobra's `ExecuteC` is always a parse failure and maps to `usage`.
- **`--after` and `--limit` are string flags** parsed by `contract.ParseAfter`/`ParseLimit`. A non-numeric `--limit abc` is therefore `invalid_limit` (65) with the API's message, not a pflag parse error (64). `--limit` with `--follow` is validated but never sent.
- **`daemon` is a runnable parent that prints help.** Cobra skips `Args` validation for a non-runnable command and shows help with exit 0, so `harness daemon bogus` would have exited 0 without it.
- **SSE: only frames with an `id` are events.** Event types may be called `error` or `shutdown` (the ledger's type pattern allows them), so the ending frames are recognised as frames *without* an id. `TestFollowDistinguishesEventsNamedLikeTheEndingFrames` covers it.
- **Idle deadline.** The 45 s timer is armed before the request is sent, and every read that returns bytes (keepalives included) resets it. When it fires it cancels the request context; the follower reports `daemon_unavailable` if its own flag says the timer fired, `130` if the caller's context ended, and `daemon_unavailable` for an EOF or reset without a final frame.
- **Timeouts.** `controlTimeout` 2 s (status, shutdown), `listTimeout` 30 s (a page; not in the spec, chosen so a hung daemon cannot hang the CLI), `followIdle` 45 s, no overall timeout on a follow. The client sets `DisableKeepAlives`, so a finished command leaves no idle connection for the daemon's graceful shutdown to wait on.
- **`contract.StartTimeoutDetails{PID, OutputTail}`** was added (`api/contract/errors.go`) next to `StartFailedDetails`: the spec says `daemon_start_timeout` carries the tail, the Errors table gives no details shape for it.
- **Human `daemon_start_failed` and `daemon_start_timeout`** print `--- daemon output (last 20 lines) ---` and the lines, or `(no output)`.
- **Not implemented, deliberately:** the `kill(pid, 0)` enrichment of the stop-timeout message. The spec says it may only enrich, and the `_dx.md` message is frozen.
- **`status` human rendering.** Keys are padded to 10 columns; `Ledger` pluralises stream and event; `Config` shows `(not present, defaults in use)` or `(present, <digest>)`; `Uptime` is a Go duration (`1m12s`, `1h2m5s`).
- **Detached start details.** Child: `<exe> daemon start --foreground --detached --home <abs home>`, `Dir` = home, `Setsid`, stdin the null device, stdout and stderr the same `daemon.out` descriptor (opened `O_CREATE|O_WRONLY|O_APPEND`, 0600; the size at open is the tail offset). The CLI closes its copy right after the spawn. Polling is every 100 ms, with the 2 s per-request timeout inside the client; the `--timeout` check runs after each poll, so a start can overshoot it by one slow request.
- **A polling error never aborts a start.** While waiting for readiness, any status error (including a 500) means "not ready yet": returning early would leave the spawned child running.
- **`e2e/` safety.** `harness()` refuses an empty home and sets `HOME` to an empty temp directory (removed by `TestMain`), so a missing `HARNESS_HOME` can never fall back to the operator's real `~/.harness`. Every test home is stopped in a cleanup, and a daemon that will not stop is killed and fails the test.

## Learnings

- **Shell variables do not survive between Bash tool calls.** An `export HARNESS_HOME=$H` in a later call expanded to an empty string, `home.Resolve` fell back to `~/.harness`, and the manual smoke run created a real home (see Errors / Corrections). Always define and use the home in the same command, and guard against an empty value.
- **Cobra skips `Args` for a non-runnable command** and prints help (exit 0); with `Args` set it also skips `legacyArgs`, so an unknown verb under a parent needs a `RunE`.
- **pflag takes the next argument as the value of a `--flag value` even when it starts with `-`,** so `--after -1` reaches validation as `-1`.
- **`-o json` can come after a bad flag.** pflag stops at the first error, so the CLI scans the raw arguments for `-o` when it reports a parse failure.
- **The Go test cache can reuse a package's result when a dependency changed but the test binary did not** (an unused new type is linked out): `make test-integration` showed `internal/daemon (cached)` after a `contract` edit. Rerun with `-count=1` when a claim depends on it (done: 48.8 s, green).

## Files / Surfaces

- New: `internal/cli/{root.go,output.go,exitcodes.go,client.go,sse.go,clock.go,spawn.go,start.go,stop.go,status.go,events.go,render.go}`.
- New tests: `internal/cli/{helpers_test.go,start_test.go,stop_status_test.go,output_test.go,events_test.go,client_test.go,sse_test.go}`.
  - Beyond the assigned IDs: start failure and timeout rendering, spawn failure, an unexpected probe error, foreground start (modes, `--detached` without `--foreground`, `ExitError` mapping), socket path before security, usage errors of other kinds, the real client over a Unix socket (query, body, error decoding, no daemon, stale socket, timeouts, interrupt), SSE parsing and the line limit, `readTail`, Ctrl-C exit 130, events named `error` and `shutdown`.
- New: `e2e/{main_test.go,lifecycle_test.go,events_test.go}` (`//go:build e2e`): E2E-001, 002, 004, 005, 006, 007, 008 (human and jsonl), 011, 013, 015, 016, 017, 023, 024, and a Ctrl-C follow (exit 130).
- Changed: `cmd/harness/main.go` (calls `cli.Execute`), `api/contract/errors.go` (`StartTimeoutDetails`), `go.mod`/`go.sum` (`github.com/spf13/cobra v1.10.2`, `pflag` indirect), `docs/roadmap.md` ("tasks 1–6 done").
- `internal/cli` imports `api/contract`, `daemon`, `home`, `version` (architecture test green with `-count=1`).

## Errors / Corrections

- **The manual smoke run touched the operator's real `~/.harness`.** In a Bash call separate from the one that created the temp home, `$H` was empty, so `HARNESS_HOME=` resolved to the default. It started and stopped a daemon twice there, creating `~/.harness` (`daemon.lock`, `harness.db`, `logs/harness.log`, `logs/daemon.out`; no config, no socket left). Every entry was born within the same second, so nothing was pre-existing; I removed exactly those files and the two directories, and `~/.harness` does not exist now. No process was left. Repeated protection: the e2e helper refuses an empty home and fakes `HOME`.
- **UT-042 and UT-125 first asserted the wrong virtual elapsed time** (they counted sleeps wrongly: the first poll after the spawn does not sleep, and the socket is only looked at once the lock is free). The expectations were corrected to what the loops do (400 ms; 1.2 s); they still pin the 100 ms cadence.
- **The timeout test first measured the end of the virtual clock,** which includes the eagerly advanced 2 s SIGTERM wait; it now asserts the time SIGTERM was sent.
- **Mutation M9 (security checked before the socket path) first survived;** `TestUT120_SocketPathIsCheckedBeforeSecurity` now kills it.

## Open items for the conductor

- **Task 7** needs the five extension points above (decision table, child error line, SIGKILL, locked-unreachable stop, `uptime_seconds: null`).
- **`--timeout` can be overshot** by up to one 2 s status request; the spec says `--timeout` bounds the whole command. Cheap to fix in task 7 with a context deadline from the real clock, if wanted.
- **`listTimeout` (30 s)** is mine; say so if the spec should name a number.
- **A help request for a bare `harness daemon`** exits 0 and prints help; the spec says nothing.
- **Task 9:** run `make test-e2e` on Linux. The e2e tests use `/tmp` homes and `Setsid`; the 113-byte socket-path case assumes the 103/107 limits only through the message, not the exit code.

## Ready for Next Run

**Verification evidence (macOS 15, Darwin 25.6, arm64, Go 1.27.3):**
- `go test -race -count=1 ./internal/cli/`: green (88% statements).
- `make gate`: green (gofmt, `go vet ./...`, `go test -race ./...`, build).
- `go test -race -count=1 ./internal/archtest/`: green.
- `make test-integration`: green; `go test -race -tags integration -count=1 ./internal/daemon/`: green (48.8 s).
- `make test-e2e` (`go test -tags e2e ./e2e/...`): green (about 5 s).
- Mutations applied one at a time and reverted, all 20 killed: readiness ignoring the PID, stop ignoring the socket, `daemon.out` without `O_APPEND`, tail ignoring the offset, follow sending `limit`, idle timer not reset, type column 30, line limit 1 MiB, checks in the wrong order, jsonl without the page line, stop margin 4 s, `--follow -o json` allowed, internal exit 70, ending frame ignored, home not created, no SIGTERM on timeout, usage exit 1, already-running spawning, JSON error on stdout, control timeout 5 s.
- After the runs: `~/.harness` does not exist, no `/tmp/hz*` directory, no `harness-e2e*` temp directory and no `bin/harness` process is left.

**Golden Path by hand** (`HARNESS_HOME=/tmp/hzg.XXXXXX`, `HOME` a temp dir, built `bin/harness`), summary of the transcript:
- `daemon start` printed `harness daemon started (pid 79025)` and the `socket:` line, exit 0.
- `status` showed State running, PID, Version `0.1.0-dev`, Uptime `0s`, Home, `Ledger … (schema 1, 1 stream, 2 events)` and `Config … (not present, defaults in use)`.
- `events --stream daemon` listed `config.loaded` (seq 1, `digest=null`) and `daemon.started` (seq 2, `boot_ms=55`) in the 5/24/32 layout.
- A background `events --follow` printed the same two rows; `daemon stop` printed `harness daemon stopped (pid 79025, uptime 1s)`, exit 0; the follower then showed seq 3 `daemon.stopping reason=request` and seq 4 `daemon.stopped uptime_seconds=1`, wrote `stream closed: daemon stopped` on stderr and exited 0.
- The next `daemon start` gave pid 79044, and `events --after 4` showed seq 5 `config.loaded` and seq 6 `daemon.started`: the stream continues.
- `daemon stop -o json` gave `{"daemon":{"stopped":true,"pid":79044,"uptime_seconds":0}}`; `status -o json` with no daemon gave the `daemon_unavailable` object on stderr and exit 69.

**For task 7:** see Important Decisions › "What was left to task 7". The `app` seams make UT-121–UT-124 and UT-126 plain unit tests (fake spawner with `onJump` writing to `daemon.out` and exiting, `fakeProc.onSignal` for a process that ignores SIGTERM, `fakeProbe` for the lock). The subprocess suites can reuse `e2e/main_test.go` (`startBackground`, `waitFor`, the clean env).

**For task 8:** add `doctor`, `config` and `version` to `a.newRoot()` in `root.go`; use `a.run` for the body, `a.printer()` for output and `a.newClient(...)` for the daemon (add `Doctor` to `daemonAPI` and `*client`). Reports with findings (doctor 70, config validate 78) are documents on stdout: return a sentinel exit from the body instead of an error, or add an `exitWith(code)` error type that `report` prints nothing for.
