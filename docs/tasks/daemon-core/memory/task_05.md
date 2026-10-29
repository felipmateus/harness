# Task 5 memory

## Objective Snapshot

`internal/daemon`, the composition root, completed 2026-10-28:
- `daemon.Run`: umask 077, SIGINT/SIGTERM/SIGHUP, the twelve boot steps with an undo stack and a cancellation check at every boundary, the ten-step shutdown inside `shutdown_grace`, the `daemon` stream events, the in-process `Hooks`;
- an optional observer on the broker (`broker.OnDegrade`) so the daemon can log `subscriber degraded`;
- tests: UT-013, IT-001–IT-006, IT-030–IT-035, IT-041–IT-043, IT-045–IT-047, IT-051, plus the resilience cases listed under Files.

## Open check answered: does Go unlink a Unix socket on `Close`?

**Yes.** On Go 1.27.3 (darwin/arm64), `net.ListenUnix` followed by `Close()` removes the socket file by default. With `SetUnlinkOnClose(false)` the file stays.
- `TestIT051_GoUnlinksTheSocketOnCloseUnlessToldNot` asserts both sides and logs the answer with `go test -v`.
- The net package documents the default as "unlink only when package net created the socket", which holds on every Unix. Task 9 should confirm it on Linux.
- The daemon depends on the opposite behaviour: the socket must survive the listener closing at shutdown step 6 and go at step 8. `TestIT051_SocketIsKeptUntilStepEight` and `TestIT002_StopOrderAndFiles` fail when the call is removed (mutation M1).

## Important Decisions

- **`subscriber degraded` has a producer now (the conductor's open item).**
  - `broker.New(opts ...Option)` and `broker.OnDegrade(func(stream string, seq int64))`. A plain function type, no new import; existing `New()` callers are unchanged.
  - The callback runs on the goroutine that called `Publish` (the ledger writer), once per degraded subscription, **after the hub lock is released**, so it may call back into the hub. A test (`TestOnDegrade_RunsOutsideTheHubLock`) deadlocks if it ever runs under the lock.
  - **Spec field difference.** The broker cannot see a follower's cursor, so the record is `{"msg":"subscriber degraded","level":"WARN","stream":…,"seq":…}`, where `seq` is the sequence of the event that did not fit. The spec's Monitoring table says `stream`, `cursor`. The record keeps the spec's message and level. The conductor should amend the table row to `stream`, `seq`, or accept the difference.
  - Rejected alternatives: wrapping `broker.Subscription` in the daemon (extra goroutine and channel hop per follower, still no cursor); changing `stream.Follow`'s signature (spec-fixed).
- **Shutdown step 5 also waits for the event streams to say goodbye (`internal/daemon/conns.go`).**
  - Why: `http.Server.Shutdown` runs its `RegisterOnShutdown` hooks at its start, so the stream contexts are cancelled right after `hub.Close()`. An SSE adapter that has not yet written its `shutdown` frame then returns without it, and the client sees a bare EOF. `TestIT005_GateStates` caught it the first time.
  - How: a `connTracker` (`http.Server.ConnState`) counts connections that are serving a request. After `hub.Close()`, step 5 waits up to `streamFinalFrameGrace` (1 s, the adapter's final-frame deadline) for none to be active. Connections still active after that are blocked on a client that stopped reading: they are closed and `streams cut at shutdown` is logged (warn).
  - Effect 1: IT-030 and the shutdown frame are deterministic. Effect 2: a stalled follower holds the shutdown for about 1 s, not for its 10 s write deadline. `TestShutdown_AStalledFollowerCannotHoldIt` is the in-process form of IT-050; task 7 only needs the CLI/subprocess form.
  - A follower with a backlog gets a gap-free prefix and then the `shutdown` frame: `stream.Follow` ends as soon as the broker closes (task 3 design). The client resumes with `Last-Event-ID`. `TestShutdown_ASlowFollowerStillGetsItsFinalFrame` asserts exactly that.
- **Shutdown is an explicit ten-step list (`shutdown.go`); boot undo is the stack (`run.go`).** They share primitives (`shutdownServer`, `removeIfExists`, `lock.Release`) but not order: undo runs newest first (daemon.json, then server and socket, then ledger, then lock), the shutdown keeps Safety Invariant 13 (server, ledger, then files). A boot cancelled after step 9 appends `stopping` and `stopped` and then unwinds; it does not run the ten shutdown steps (no `BeforeShutdownStep` calls, no drain).
- **Hooks (`hooks.go`).**
  - `AfterBootStep(n)` runs after step n completes, in its own goroutine. A stop request ends the wait, so a held hook never blocks a cancellation; the abandoned goroutine ends when the test releases it. A boot that is already cancelled does not start the hook.
  - `BeforeShutdownStep(n)` runs before step n inside the step's goroutine, so it is bounded by the budget and by a second signal. After the daemon gives up on a step, a hook that returns late never runs the step's action.
  - `OnRunning(TestAccess{Appender, Gate, Logger})` runs on the boot goroutine after `daemon running` is logged. It must return promptly (hand the access to the test).
  - `TestAccess.Gate.Enter()` held by a test is an in-flight request: the drain (step 3) waits for it. This is how IT-005 holds a request open during draining.
- **Overrun and second signal share one forced path (`force()`).** It closes the server and every connection, closes the ledger with an already-ended context (a transaction in flight is rolled back), removes `daemon.json` and the socket, releases the lock, and returns `*ExitError{Code: 1}`: `shutdown_budget_exceeded` for the budget, `shutdown_aborted` for a second signal. An overrun before step 4 leaves no `stopped`, so the next start reports an unclean shutdown (`previous_pid`, `last_type: daemon.stopping`); after step 4 the ledger is clean.
- **Boot failures keep the recognised codes; everything else is exit 1 with a step code.** Recognised: `config_invalid` (78), `home_insecure` (78), `socket_path_too_long` (78), `ledger_schema_ahead` (69), `daemon_already_running` (69). Step codes for the rest: `home_unusable`, `lock_failed`, `config_unreadable`, `log_unavailable`, `ledger_open_failed`, `ledger_failed`, `stale_cleanup_failed`, `listen_failed`, `info_write_failed`. The `code` of `daemon.boot_failed` is the same string. A hook that returns an error at step N fails step N with its step code (IT-047: `listen_failed` at step 10).
- **The foreground lock retry is here, not in task 7.** `Interactive` tries the lock, then retries 3 times 33 ms apart; `Detached` tries once (the CLI loops itself). The retry wait ends at once on a stop request. Task 7's "foreground start retries `AcquireLock`" item is already satisfied.
- **The `Detached` mirror cut-off is here too:** the mirror is turned off right after `gate = running`, before `daemon running` is logged (spec order). Task 7 still owns the final error line.
- **Log records.** `boot step` (debug: `step` int, `name`, `duration_ms`), `boot step failed` (error: `step`, `name`, `code`, `err`), `boot cancelled` (info: `step`, `reason`), `daemon running`, `shutdown step` (debug), `shutdown budget exceeded` (error: `step`, `name`, `budget`), `daemon stopped`, `streams cut at shutdown`, and `unclean shutdown detected` (warn, extra). The `_dx.md` sample shows `"step":"ledger"` (a name); the record carries the number in `step` and the name in `name`.
- **Doctor slot:** `internal/daemon/doctor.go` has `func (d *daemon) doctor() doctorRunner { return nil }` and an unexported `doctorRunner` interface. Task 8 replaces the body. Until then `GET /api/doctor` on a running daemon answers 500 `internal`; a gated state answers 503 as usual.
- **`Options.Home` is never defaulted.** An empty home is a failed boot (`home_unusable`), so no code path can reach the operator's real home by accident.
- **`started_at`, `boot_ms` and uptime all count from the moment `Run` begins.**
- **`http.Server`:** `ReadHeaderTimeout` 10 s, no write timeout, `DisableGeneralOptionsHandler: true` (`OPTIONS *` gets the JSON 404), `ErrorLog` routed to the daemon logger at warn.
- **An accept-loop failure that nobody asked for** logs `api server stopped unexpectedly` and requests a stop with reason `context`.

## Learnings

- **Lumberjack v2.2.1 leaks one goroutine per `Logger`** (`millRun`; `Close` never stops it). Every `Run` in a test process leaves one behind. The daemon tests use their own `checkLeaks` (`helpers_test.go`), which does not count goroutines whose stack mentions `lumberjack`. A daemon process exits, so production is unaffected; a follow-up could replace or patch the rotation (task 1's package).
- **`signal.Notify` starts a process-wide goroutine on its first use.** `main_test.go` primes it with an unused signal before any test takes a goroutine baseline.
- **The explicit modes hide a missing umask.** Every file and directory is made 0600/0700 explicitly, so UT-013 reads the umask back (`syscall.Umask(0o022)` returns what `Run` left, which must be `0077`); mutation M6 (umask 022) is killed by that, not by the modes.
- **`Shutdown` runs `RegisterOnShutdown` hooks in new goroutines, at its start.** The stream contexts are cancelled concurrently with the first step of the server shutdown; that is why step 5 must finish the streams first.
- **A stalled-client test must outgrow the socket buffers on Linux too** (about 200 KB each way, against 8 KB on macOS): IT-033 and the stalled-follower test use 4 KiB frames and stall until 300 events are in.
- **IT-033 as the spec words it cannot degrade a follower.** A reader that sleeps 5 ms per frame keeps up with a producer that commits every 6 ms. The test keeps the 5 ms pace and the 2000 events, and adds an initial stall (not reading until 300 events are in), so the 64-event subscription overflows. Several rounds of degrade and catch-up follow.
- **In-process signals work.** `syscall.Kill(os.Getpid(), sig)` while a `Run` is active reaches only its handler. The signal tests run one daemon at a time; two daemons in one process would both stop (each `Run` registers its own channel).
- **A tests-only dependency:** `database/sql` with `modernc.org/sqlite` fakes a newer schema (a goose table with version 3) for the `ledger_schema_ahead` refusal test.

## Files / Surfaces

- New: `internal/daemon/{run.go,boot.go,shutdown.go,signals.go,hooks.go,events.go,status.go,doctor.go,conns.go}`.
- New tests: `internal/daemon/{main_test.go,helpers_test.go,unit_test.go}` (UT-013, exit-error mapping, payload field order, lock retry, undo order); `internal/daemon/{lifecycle,streams,security,resilience}_integration_test.go` (`//go:build integration`).
  - lifecycle: IT-001–IT-006, IT-045–IT-047, IT-051.
  - streams: IT-030–IT-035, the stalled and slow follower shutdowns.
  - security: IT-041–IT-043.
  - resilience (extra, from the task body): unclean detection (7 cases), boot cancellation after each of steps 1–11, refusals (invalid config, socket path too long, schema ahead), stale files, overrun before and after step 4, SIGINT/SIGTERM/SIGHUP, a signal during boot, the second signal, the log mirror by mode, the configured grace in the shutdown answer, `OPTIONS *`, `Hooks.KeepaliveInterval`, a held hook not blocking a cancel.
- Changed: `internal/broker/{broker.go,broker_test.go}` (`Option`, `OnDegrade`, three tests).
- Changed: `docs/roadmap.md` (feature 1 shows "tasks 1–5 done").
- No new module dependency.
- `internal/daemon` imports `admission`, `api`, `api/contract`, `broker`, `config`, `home`, `ledger`, `logging`, `stream`, `version` (checked with `go list` and the architecture test); `broker` still imports only `ledger`.

## Errors / Corrections

- **The first IT-005 run lost the `shutdown` frame** (see the step 5 decision). Fixed with the connection tracker, not by weakening the test.
- **The first version of the slow-follower test expected all 100 events before the `shutdown` frame.** That contradicts the follow design (it ends at once when the broker closes); the test now asserts a gap-free prefix and the frame.
- **Mutation M2 (no stream drain) first survived** because the mutation script's test pattern was not updated; after adding the slow-follower test and fixing the pattern, IT-005 and that test kill it.
- **Two mutation patterns did not compile** (unused import or variable) and were redone as compile-safe mutations.
- **A failing assertion printed a whole 4 KiB frame.** The messages now print only the frame's id and event.

## Open items for the conductor

- **Spec Monitoring table:** `subscriber degraded` is `stream`, `seq` (not `cursor`).
- **`append rejected` has no producer in this feature.** Nothing appends from outside the daemon, and the daemon's own events are never rejected; whoever adds an append route should write it.
- **Edge of Business Rule 12/13:** if step 7 appended `daemon.unclean_shutdown_detected` and the boot is cancelled before step 9, the last `daemon` event is that unclean one, so the next start records another unclean event whose `previous_pid` is the cancelled daemon's. The spec says "undo only" for steps 6–8, and that is what is implemented.
- **A crash between the temp write and the rename of `daemon.json` leaves `.daemon.json.*` in the home.** Step 8 removes only the socket and `daemon.json`.
- **Lumberjack's goroutine leak** (see Learnings).
- **Task 9:** confirm the IT-051 answer on Linux and run the new tests there (they are Linux-neutral by design: socket path under `/tmp`, TCP probe through `/proc`).

## Ready for Next Run

**Verification evidence (macOS 15, Darwin 25.6, arm64, Go 1.27.3):**
- `make gate`: green (gofmt, `go vet ./...`, `go test -race ./...`, build).
- `make test-integration` (`go test -race -tags integration ./...`): green; `internal/daemon` takes about 56 s (IT-033 and IT-035 append 2000 events each at about 6 ms per commit).
- `go test -race -count=1 ./internal/archtest/`: green; `internal/daemon` imports no `cli`.
- `go test -race -tags integration -count=1 -cpu 1,4 ./internal/daemon/`: see the final report for the result.
- Mutations applied one at a time and reverted, each killed: Go default unlink kept (M1), no stream drain (M2), `stopping` treated as clean (M3), cancellation records from step 7 (M4), no `boot_failed` (M5), umask 022 (M6), gate never draining (M7), no interactive lock retry (M8), forced path keeps the lock (M9), second signal ignored (M10), degrade observer not wired (M11), hook not abandoned on cancel (M12), socket removed at step 6 (M13), stale files kept (M14), ledger not force-closed (M15), and in the broker the observer called under the hub lock.
- After the runs, `~/.harness` did not exist and no `/tmp/hz*` directory or `daemon.sock` was left.

**For task 6 (CLI):**
- `daemon.Run(ctx, daemon.Options{Home: h, Mode: daemon.Interactive, Stderr: os.Stderr, Version: version.Get()})`. `Run` owns the signals (SIGINT/SIGTERM/SIGHUP): the CLI must not stop the daemon itself, and should pass a ctx it never cancels except to embed.
- A nil return is exit 0. `*daemon.ExitError{Code, ErrCode, Message, Hint}` maps to the exit code `Code` and the error body `{"error":{"code":ErrCode,"message":Message,"hint":Hint}}`. Boot failures before the logger opens (steps 1–4) print nothing themselves, so the caller must report them.
- `Interactive` mirrors the log to `Options.Stderr` for the whole run; the stderr text for a second foreground start is `daemon already running for <home> (pid N)` (the message already says it).
- The status JSON carries `state` from the gate, `started_at` from `Run`'s start, and `config.digest` as loaded at boot. `POST /api/daemon/shutdown` answers `{"daemon":{"stopping":true,"pid","uptime_seconds","shutdown_grace":"30s"}}` with the grace as configured.

**For task 7:**
- `Hooks` mapping for `HARNESS_TESTHOOKS`: `hold-boot-step=N` is an `AfterBootStep` that blocks forever at N (a cancel still works); `fail-boot-step=N` returns an error at N; `hold-shutdown-step=N` is a `BeforeShutdownStep` that blocks forever at N (the budget or a second signal ends it: IT-023, IT-025). `block-exit` cannot be a daemon hook: the forced path always returns; keep the process alive after `Run` returns, in the test-hooks `main`.
- The in-process forms of IT-023 (second signal), IT-025 (overrun), IT-054 (signals), IT-055 (SIGTERM during boot), IT-046/047, IT-050 (stalled follower) and IT-026 (stale files) already pass in `internal/daemon`; the subprocess suite adds the real process boundary and the CLI.
- `Detached` already stops mirroring at running; add `internal/daemon/detached.go` for the one-line final error (`ExitError.ErrCode`, `Message`, `Hint`).

**For task 8:** replace the body of `(*daemon).doctor` in `internal/daemon/doctor.go` with the daemon-mode runner; `GET /api/doctor` then answers 200. IT-040 is task 8's.
