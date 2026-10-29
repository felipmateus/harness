# Task 8 memory

## Objective Snapshot

Operator tooling, completed 2026-10-28 (slice 3):
- `internal/doctor`: the probe registry, the seven probes, daemon and local modes, 2 s per probe;
- `(*daemon).doctor()` returns the real runner, so `GET /api/doctor` answers 200;
- the `harness doctor`, `config path|validate|show` and `version` verbs;
- tests: UT-070–UT-076, UT-132, IT-040, E2E-003, 009, 010, 012, 018, 019, 020 plus extras below.

## Important Decisions

- **API of `internal/doctor`:** `doctor.New(Options{Home, Mode, LoadedDigest}).Run(ctx) (contract.Doctor, error)`. `Mode` is `ModeDaemon` or `ModeLocal` (`source` is the same string). `LoadedDigest` is `d.cfg.Digest` of the running daemon (nil: it loaded no file); local mode ignores it. Probe IDs are exported constants (`IDHomePermissions`, ...). Report order is fixed: local = `daemon.reachable`, `home.permissions`, `config.valid`, `ledger.integrity`, `ledger.schema`, `log.writable`; daemon = the same without `daemon.reachable` and with `socket.permissions` after `home.permissions`.
- **Probes run in parallel,** each under its own `ProbeTimeout` (2 s), so a report takes at most about 2 s and one stuck probe cannot delay or hide the others. The two ledger probes share one `ledger.Inspect` per run (`state.inspectLedger`); each waits under its own context. A probe that returns after its deadline is still a timeout (`timed out after 2s`): a cooperative probe returns the instant the deadline fires, and its answer then describes the interruption. The first version let such an answer win the race and UT-073 failed once in `make test-integration-hooks`.
- **`ledger.Inspect` reads as `immutable=1` when neither `-wal` nor `-shm` exists** (a cleanly closed ledger: everything is in the main file), and as the plain `mode=ro` connection otherwise. This is a change to task 2's `Inspect`: it rejected `immutable=1` in general because it ignores a WAL that holds committed pages; here it is used only when there is no WAL to ignore. The effect is that the local doctor creates no `harness.db-wal`/`-shm` (a plain `mode=ro` connection creates both). `TestInspect_AfterACleanCloseLeavesNoFilesBehind` and mutation M3 cover it; E2E `TestDoctorWithoutADaemonLeavesTheHomeAsItWas` checks the whole home (names, modes, sizes, mtimes) after a daemon stop.
  - **Remaining limit:** after a crash (SIGKILL) `-wal` and `-shm` exist; the `mode=ro` connection then recovers through them and may rewrite SQLite's own `-shm` bookkeeping. `harness.db` itself stays byte-identical.
- **`log.writable` does not write:** it opens an existing log `O_WRONLY` (no create, no truncate, nothing written, so mtime is untouched). With no log yet it checks the nearest existing directory with `access(W_OK)`; with no home at all it is `ok` ("does not exist yet").
- **A home or ledger that does not exist yet is `ok`,** not a finding: a fresh home is healthy, and E2E-020 needs exit 0 there. Messages: `<home> does not exist yet (the first start creates it, 0700)`, `no ledger yet (...)`.
- **`home.permissions`:** any mode other than 0700 is an error (`<dir> is 0755; expected 0700`, fix `chmod 700 <dir>`). A symlink, a non-directory or a foreign owner reuses `home.CheckSecurity`'s message and hint, so doctor and the daemon's refusal say the same words.
- **`ledger.schema` when the ledger cannot be read is a `warn`** pointing at `ledger.integrity`: one root cause, one error. Schema ahead is an error (`upgrade harness`); an older schema is a warn ("the next start migrates it").
- **`config.valid`:** the file on disk through `config.Load`. Invalid or unreadable: error, "the next start will fail", fix `run \`harness config validate\``, with `(and N more)` for further problems. Daemon mode only: a digest that differs from `LoadedDigest` (changed, added or removed since boot) is a warn, "restart to apply".
- **`daemon.reachable` is a statement, not a probe:** the CLI only runs local mode after the daemon failed to answer, so the item is always `warn` with the `_dx.md` text and no fix line.
- **The CLI falls back to local only on `daemon_unavailable`** (no socket, refused, or no answer within 10 s). A daemon that answers 503 (`daemon_not_ready`, `daemon_draining`) or 500 is an error like any other route (Errors table: "any route except status and shutdown"), exit 69 or 1.
- **Doctor and config exits use `silentExit`:** 70 for a doctor report with an error item, 78 for an invalid `config validate`. The report is on stdout in every output mode; stderr stays empty.
- **Shapes the `_dx.md` left open:**
  - `config path -o json` is `{"config":{"path":"..."}}`;
  - `config validate -o json` is `{"config":{"path","valid","errors":[...]}}`, with `"errors":[]` when valid;
  - human `config validate`: `valid: <path>` (plus ` (not present, defaults in use)` for a missing file), or `invalid: <path>` and one `  - <message> (<code>)` line per problem;
  - `config show` human prints the effective config as TOML; an invalid file is a `config_invalid` error (78), not a report;
  - an unreadable `config.toml` is `internal` (exit 1), like the daemon's boot step 4 (`config_unreadable`, exit 1);
  - human `doctor` rows are `%-6s%-21s%s`, the fix line is six spaces and `fix: ...`.
- **`client.Doctor`** uses `doctorTimeout` (10 s): the daemon bounds itself at 2 s per probe in parallel, so 10 s only guards a hung daemon.

## Learnings

- **A cooperative probe loses the "finished at the same moment" race:** after the deadline, an answer is a timeout. See the second decision.
- **`go test` caches the e2e suite across source changes** (it builds the binary in a child `go build`, which the cache does not track): use `-count=1` after changing production code when a claim depends on the e2e result.
- A WAL database that was closed cleanly has no `-wal`/`-shm`; any plain read-only connection recreates them. That is what UT-072 would have caught had the test home been cleanly closed (it is).

## Files / Surfaces

- New: `internal/doctor/{doctor.go,probes.go,doctor_test.go}`, `internal/cli/{doctor.go,config.go,version.go,tooling_test.go}`, `internal/daemon/doctor_integration_test.go`, `e2e/tooling_test.go`.
- Changed: `internal/daemon/doctor.go` (real runner), `internal/cli/{root.go,client.go,helpers_test.go}` (verbs, `Doctor` on `daemonAPI` and `*client`, `fakeAPI.doctorFn`), `internal/ledger/{inspect.go,schema.go,ledger_test.go}` (conditional immutable read), `docs/roadmap.md`, `task_08.md`.
- Tests beyond the assigned IDs: loose-home modes (0755, 0750, 0777, 0500), symlink home, schema ahead and older, a corrupt ledger, unreadable config, socket mode and a missing socket, log not writable (file, logs dir, home), a home that never started, parallel probes, a cancelled context, `config.valid` in daemon mode against a real daemon (IT-040 extras), real-client doctor over a Unix socket, the human transcripts of `_dx.md`, usage errors of the new verbs, E2E on a stopped home leaving the file set untouched, and an unreadable ledger through the binary.
- No new module dependency. The architecture test is green with `-count=1` (`cli` imports `doctor`, `config`, `version`; `daemon` imports `doctor`; `api` does not).

## Open items for the conductor

- **`ledger.Inspect` changed** (conditional `immutable=1`); task 2's note said immutable was rejected. **Accepted by the user on 2026-10-29.** The spec needs no amendment: `_spec.md` › Core Interfaces only says `Inspect` opens read-only, and an immutable read is read-only.
- **The crash case still touches SQLite's `-shm`** when doctor runs on a home with a leftover `-wal`.
- **`daemon.reachable` has no fix line** (the `_dx.md` transcript shows none); `harness daemon start` would be a natural hint.
- **A starting or draining daemon gives exit 69 from `doctor`,** not a local report. Falling back to local there would need a different `daemon.reachable` message ("a daemon is starting"); the spec table does not ask for it.
- **Pid 88714 (task 7's stray test daemon) was still running** during this task; I did not touch it.
- **Task 9:** run `make test-e2e`, `make test-e2e-hooks` and the doctor tests on Linux. `home.permissions` uses `syscall.Stat_t` and `syscall.Access`, both present on Linux (the package cross-compiles with `GOOS=linux`); the unreadable-file tests skip as root.

## Errors / Corrections

- **UT-073 was flaky once** (a probe that returned on its own deadline won the race against the timeout). Fixed at the cause in `runOne`; `go test -race -count=8 -run UT073` and the full suites are green after it.
- **`TestUT073_ADeadlineIsPerProbeNotPerReport` first did not prove parallelism** (three 1.2 s probes pass under per-probe timeouts sequentially too). It is now `TestRun_ProbesRunInParallel` and asserts the elapsed time; mutation M5 (sequential probes) is killed by it.

## Ready for Next Run

**Verification evidence (macOS, Darwin 25.6, arm64, Go 1.27.3):**
- `make gate`: green (gofmt, `go vet ./...`, `go test -race ./...`, build).
- `make test-integration`: green (`internal/daemon` 50 s).
- `make test-e2e`: green, `-count=1`, about 6.5 s.
- `make test-hooks`: green. `make test-integration-hooks`: green (`internal/daemon` 72 s). `make test-e2e-hooks`: green, `-count=1`.
- `go test -race -count=1 ./internal/archtest/`: green.
- Mutations applied one at a time and reverted, all killed: home 0755 accepted, no digest comparison in `config.valid`, timeout message, `socket.permissions` in local mode, sequential probes, daemon runner without the loaded digest, doctor exit 1 on an error, validate exit 1, local fallback on any error, the log probe creating the file, `Inspect` always read-only (killed by the new ledger test).
- By hand with the built binary on `/tmp/hzs.XXXXXX` (`HOME` and `HARNESS_HOME` inline, one command): `version`, `config path|validate|show`, `doctor` on a missing home (5 ok, 1 warn, exit 0), after `daemon start` (6 ok), with the home at 0755 (exit 70, fix line), and after `daemon stop` (local). The home directory was removed afterwards; `~/.harness` never existed.
- After the runs no `/tmp/hz*`, `harness-it*` or `harness-e2e*` directory is left, and the only `harness` process is task 7's pid 88714.
