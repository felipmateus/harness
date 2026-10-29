# Workflow memory: daemon-core

## Current State

- Task 1 is completed (2026-10-28). `make gate` is green on macOS (Go 1.27.3, darwin/arm64).
- Task 2 is completed (2026-10-28): `internal/ledger` with schema v1. `make gate` is still green. `PRAGMA fullfsync` reads back `1` on darwin.
- Task 3 is completed (2026-10-28): `internal/broker` (`Hub`) and `internal/stream` (`Follow`). `make gate` is still green.
- Task 4 is completed (2026-10-28): `internal/admission` (`Lifecycle`) and `internal/api` (`Server`, SSE adapter). `make gate` is still green.
- Task 5 is completed (2026-10-28): `internal/daemon` (`Run`, boot, shutdown, the `daemon` stream, in-process `Hooks`) and `broker.OnDegrade`. `make gate` and `make test-integration` are green. The IT-051 answer: **Go does unlink a Unix socket on `Close` by default** (`memory/task_05.md`).
- Task 6 is completed (2026-10-28): `internal/cli`, `cmd/harness`, `e2e/`; the `_dx.md` Golden Path runs by hand. `make gate`, `make test-integration` and `make test-e2e` are green. Task 7 extension points are listed in `memory/task_06.md`.
- Task 7 is completed (2026-10-28): start/stop under races, crashes and timeouts, the `harness_testhooks` hooks and the process and crash suite. `make gate`, `make test-integration`, `make test-e2e` and the hook-enabled variants (`make test-hooks`, `make test-integration-hooks`, `make test-e2e-hooks`) are green. Details and open items in `memory/task_07.md`.
- Task 8 is completed (2026-10-28): `internal/doctor`, the `doctor`, `config path|validate|show` and `version` verbs, and the daemon runner behind `GET /api/doctor`. `make gate`, `make test-integration`, `make test-e2e` and the three hook variants are green. Details and open items in `memory/task_08.md`.
- Task 9 is completed (2026-10-29): every suite green together on macOS with `-count=1` (`make gate`, `test-integration`, `test-e2e`, the three hook variants, the architecture test); Linux was then verified the same day, once the user allowed Docker: all six suites green in a `golang:1.27` container (`linux/arm64`, as root), and IT-051 confirmed there (`memory/task_09.md` › Linux verification); the two open checks answered (IT-051: Go unlinks on `Close` by default; UT-029: `fullfsync` reads `1` on darwin; both match the spec); `README.md` written and walked through; roadmap feature 1 marked done. `daemon-core` is complete.
- The repository has had `git init` (branch `main`) and has no commits. The user commits; never commit without an explicit request.

## Shared Decisions

- **`api/contract` is a leaf package**, so it defines its own `contract.Event` with the same JSON shape as `ledger.Event`. The API (task 4) converts between them. The spec text `EventsPage{Events []ledger.Event}` is satisfied by shape, not by type.
- **Flock comes from `syscall.Flock` directly, not `gofrs/flock`.** This gives exact control over the open flags (`O_RDWR` without `O_TRUNC` for the owner, `O_RDONLY` for probes). The spec's module list named gofrs/flock as a plan only.
- **`internal/home` errors** are `*home.Error{Code, Message, Hint, PID}`, using the contract code strings (`socket_path_too_long`, `home_insecure`, `daemon_already_running`). Consumers map them via `errors.As`.
- **`cmd/harness` may import only `internal/cli`.** `main` is `os.Exit(cli.Execute(os.Args[1:], os.Stdout, os.Stderr))`.
- **`ledger.ErrSchemaAhead` is a sentinel *and* a typed error.** `Open` returns `*ledger.SchemaAheadError{DB, Binary}` that unwraps to the sentinel: use `errors.Is` to test and `errors.As` for the numbers.
- **Every `Append` error returns `Event{ID}` only**, so a retry with that ID is idempotent; an idempotent hit returns `created=false` and does not call `OnCommit`.
- **The ledger's test seams are unexported fields of `ledger.Options`** (clock and hooks). Other packages cannot reach them; task 3 and task 5 tests use the real `Open` and `Append`.
- **The ledger creates `harness.db` as 0600** itself, so the file and its `-wal`/`-shm` are private whatever the umask.
- **The concrete broker is `broker.Hub` (`broker.New()`)**; `broker.Broker` and `broker.Subscription` are the spec's interfaces. Wire it as `OnCommit: hub.Publish`.
- **`stream.Follow`'s channel has an 8-item buffer and `send` is data-first** (ctx, then a ready consumer, then the `Done` escape). The terminal `ErrClosed` is best-effort: a consumer that sees the channel close while its own ctx is live treats it as `ErrClosed`. Details and reasons in `memory/task_03.md`.
- **`internal/testutil/leakcheck`** (`Baseline`, `Settle`, `Check`) is the goroutine-leak check (no goleak dependency). Arm `Check(t)` first in a test; later cleanups (ledger, ctx cancel) run before it.
- **`admission.Gate` is the interface, `admission.Lifecycle` (`admission.New()`) the concrete type.** `Set` is forward-only and silent; `Enter` admits only while running; the stream route is admitted but never counted in flight (details in `memory/task_04.md`).
- **`api.New(api.Options{...})` takes the consumer interfaces plus `Follow`, `RequestStop`, `StreamContext`, `Logger` and the two timings.** `statusSource` has `Status` and `ShutdownInfo`; `RequestStop` is called once and must only signal; `Doctor` may be nil until task 8.
- **`broker.New(opts ...Option)` takes `broker.OnDegrade(func(stream string, seq int64))`.** The callback runs after the hub lock is released, on the writer goroutine, and may call back into the hub. The daemon logs it as `subscriber degraded` with `stream` and `seq` (the event that did not fit). Review round 1 (issue 003) settled it: the spec's Monitoring row now says `seq`, because only the broker sees a stalled follower and it cannot see a cursor.
- **`daemon.Run(ctx, Options{Home, Mode, Stderr, Version, Hooks})`** owns umask, signals and both sequences. It returns nil for a stop or a cancelled boot, and `*daemon.ExitError{Code, ErrCode, Message, Hint}` for a failed boot (recognised codes keep their own: `config_invalid`, `home_insecure`, `socket_path_too_long` 78; `ledger_schema_ahead`, `daemon_already_running` 69; the rest exit 1 with a step code) or a shutdown that overran its budget (`shutdown_budget_exceeded`) or was interrupted by a second signal (`shutdown_aborted`). Hook semantics, the stream drain at shutdown step 5 and the log records are in `memory/task_05.md`.
- **Shutdown step 5 waits (up to 1 s) for the event streams to write their `shutdown` frame, then cuts the connections still blocked** (`internal/daemon/conns.go`). Without it, `http.Server.Shutdown` cancels the stream contexts first and the frame is lost.
- **Error and validation messages live in `api/contract`** (`messages.go` added by task 4): the API and the CLI share one wording.
- **Unknown paths answer JSON 404 in every gate state;** a known path is gated before its method and query are checked.

- **The CLI is `internal/cli`** (cobra). `app` (`root.go`) holds the seams (`clock`, `newClient`, `spawner`, `probe`, `executable`, `runDaemon`, `interrupt`); `run` types every command error; `report` writes it under the output rules and maps it to the exit code (`exitcodes.go`). Success output goes through `a.printer()`.
- **Output mode of an error comes from the raw arguments** (`outputModeFromArgs`), so a usage error still follows `-o json` when the bad flag precedes it.
- **`e2e/` is the E2E harness** (`TestMain` builds the binary; `harness()`, `startBackground`, `newHome`). It always sets `HARNESS_HOME`, refuses an empty one and fakes `HOME`.
- **`contract.StartTimeoutDetails`** carries the output tail of `daemon_start_timeout`, like `StartFailedDetails` does for `daemon_start_failed`.

- **The `--detached` child reports a failure itself.** `daemon.Run` writes `{"error":{code,message,hint}}` as the last line of `Options.Stderr` (`daemon.out`) for every `*ExitError` it returns in `Detached` mode; the CLI child then exits through `silentExit` with no further output. The spawning CLI re-raises `config_invalid`, `home_insecure`, `socket_path_too_long` and `ledger_schema_ahead`, loops back to the status probe on `daemon_already_running`, and otherwise reports `daemon_start_failed` with the tail (`internal/cli/childerror.go`).
- **`settleHome` is the start decision table** (`internal/cli/start.go`): it re-probes the status on every poll, so waiting for a held home ends in `already_running` when the holder finishes booting. `stopUnreachable` (`stop.go`) is the stop on a daemon that holds the lock but does not answer (35 s, PID from the lock file, `uptime_seconds: null`, shutdown re-sent every poll).
- **`HARNESS_TESTHOOKS`** exists only with `-tags harness_testhooks` (`internal/daemon/testhooks_env.go`): `hold-boot-step=N`, `fail-boot-step=N`, `hold-shutdown-step=N`, `block-exit` (comma-separated). `Hooks.BlockExit` keeps the lock and blocks after the shutdown; it is a daemon hook, not a `main` one.
- **`make test-integration-hooks` is where the process and crash suite runs** (`internal/daemon/process_integration_test.go`); `make test-integration` does not include it.

- **Doctor is `internal/doctor`:** `doctor.New(Options{Home, Mode, LoadedDigest}).Run(ctx)`, probes in parallel under 2 s each, a deadline that passed means a timeout even if the probe answered. The daemon wires `ModeDaemon` with `d.cfg.Digest`; the CLI runs `ModeLocal` only when the daemon answers `daemon_unavailable`. A report with an error item exits 70 and `config validate` on an invalid file exits 78, both through `silentExit` with the document on stdout.
- **`ledger.Inspect` reads as `immutable=1` when no `-wal`/`-shm` exists** (a cleanly closed ledger), so the local doctor creates no files; otherwise it is the plain `mode=ro` connection. Accepted by the user on 2026-10-29. See `memory/task_08.md`.
- **Review round 1 changes:** a boot cancelled once it has appended an event (step 7's unclean event, step 9's `config.loaded`) records `daemon.stopping` and `daemon.stopped` (`daemon.appended`, set by `record`); the spec's Business Rule 12 and boot section say so, and IT-057 plus `TestBootCancelledAfterAnUncleanEventRecordsItsStop` cover it. An empty `daemon.shutdown_grace` is now `invalid_value` (`config.Duration.set`, UT-007).
- **The six spec amendments from review rounds 1–3 are confirmed by the user (2026-10-29).** Their "needs operator confirmation" notes in `reviews-00*/issue_*.md` are settled; the spec's Status row lists them. Linux was verified on 2026-10-29. Still open with the user: committing.

## Shared Learnings

- **Shell variables do not survive between Bash tool calls.** A smoke run that relied on `$H` from an earlier call fell back to the real `~/.harness` and created it (cleaned up; see `memory/task_06.md`). Define and use a home in one command, and guard against an empty value.
- Cobra skips argument validation for a non-runnable parent command and prints help with exit 0; give `daemon` a `RunE` so an unknown verb is a usage error.
- flock locks conflict between separate `open()` calls even inside one process. The tests still use a helper subprocess (re-exec of the test binary through `TestMain`) to match the spec's wording.
- BurntSushi/toml reports the line of the offending token. The exact UT-005 input `[daemon` (no trailing newline) reports `line 1`; `[daemon\n` reports `line 2`.
- The architecture test already caught one violation (the stub `main` importing `internal/version`), so it is wired correctly. `go test` may serve it from cache when a package is added; use `-count=1` to be sure.
- A `mode=ro` SQLite connection on a cleanly closed WAL database still creates an empty `-wal` and a 32 KiB `-shm` (0600). The main file stays byte-identical, which is what UT-025 and E2E-014 assert.
- Each commit costs about 6 ms with `synchronous=FULL` and `fullfsync` on macOS, so a test that makes 1000 appends takes about 8 s under `-race`. Keep append counts in tests modest.
- A stuck-consumer test cannot read the channel to see the goroutine exit; it counts goroutines instead. A test that publishes to many fast subscribers must pace itself below the 64-event buffer, or a reader that is not scheduled in time is degraded (correctly).
- Ledger-based tests that need thousands of events use a gap-free in-memory reader instead (a real append costs about 6 ms).
- The spec's wall-clock SSE tests (UT-065, UT-112, UT-113) are `eventually` scenarios (three attempts). The numbers stay the spec's; the retry absorbs host load, and a real defect fails every attempt.
- `signal.Notify` starts a process-wide goroutine on first use: prime it in `TestMain` before any goroutine baseline. In-process `syscall.Kill(os.Getpid(), sig)` reaches only the active `Run`'s handler; run one daemon at a time in signal tests.
- The daemon makes its files 0600/0700 explicitly, so only reading the umask back proves the `Run` call (UT-013 does).
- A stalled-client test must outgrow the socket buffers on Linux (about 200 KB) as well as macOS (8 KB): use 4 KiB frames.

## Open Risks

- **`append rejected` has no producer in this feature:** nothing appends from outside the daemon. Whoever adds an append route writes it.
- **A crash between the temp write and the rename of `daemon.json`** leaves `.daemon.json.*` in the home; boot step 8 does not remove it.
- **Lumberjack v2.2.1 leaks one goroutine per `Logger`** (never stopped, not even by `Close`). Harmless for a process that exits; the daemon tests use `checkLeaks`, which skips those goroutines.
- After a crash, `harness doctor` on a home with a leftover `-wal` reads through SQLite's recovery and may rewrite the `-shm` bookkeeping file (the main `harness.db` stays byte-identical).

## Handoffs

- **Task 6 (done):** see `memory/task_06.md`.
- **Task 7 (done):** see `memory/task_07.md`. (Original extension points: in `memory/task_06.md` › Important Decisions (decision table, child error line, SIGKILL, locked-unreachable stop, `uptime_seconds: null`). The hooks map onto `daemon.Hooks` (`hold-boot-step`, `fail-boot-step`, `hold-shutdown-step`); `block-exit` became `Hooks.BlockExit` in the daemon. The in-process forms of IT-023, 025, 026, 046, 047, 050, 054 and 055 already passed.)
- **Task 8 (done):** see `memory/task_08.md`. (Original note: add the verbs to `a.newRoot()` in `internal/cli/root.go` (see `memory/task_06.md` › Ready for Next Run). `ledger.Inspect` is read-only (no migration, no creation); see `memory/task_02.md` for the `Inspection` fields. Replace the body of `(*daemon).doctor` in `internal/daemon/doctor.go` with the daemon-mode runner.)
- **Task 9 (done):** see `memory/task_09.md`. The Linux confirmation was done on 2026-10-29 (`memory/task_09.md` › Linux verification), on arm64, as root, in a container. Still not done: amd64, a real systemd session, and the unreadable-file cases as a normal user. Original note: confirm the IT-051 answer (Go unlinks on `Close` by default) on Linux, and run the new daemon integration tests there, including `make test-integration-hooks` and `make test-e2e-hooks` (IT-053 needs about 7 s of real time; the suite uses `/tmp` homes, `sh`, `sleep` and `syscall.Getpgid`). Also the doctor suites (`internal/doctor`, `e2e/tooling_test.go`); the unreadable-file cases skip as root.
