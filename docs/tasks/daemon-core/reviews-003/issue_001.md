---
provider: manual
pr:
round: 3
round_created_at: 2026-10-29T04:05:35Z
status: resolved
file: internal/daemon/signals.go
line: 95
severity: low
author: claude-code
provider_ref:
---

# Issue 001: Foreground overrun with a dead stderr dies of SIGPIPE, not exit 1

## Review Comment

The round 2 fix (`reviews-002/issue_001.md`) notifies SIGPIPE inside `watch` (`internal/daemon/signals.go:68`) and removes the handler when `Run` returns (`signals.go:95`, `defer d.watch(ctx)()` in `internal/daemon/run.go:90`). For a foreground daemon, `Run` returning is not the end of the process. `runForeground` (`internal/cli/start.go:89`) hands `Run`'s `*ExitError` to the CLI error path, and that path writes the message to stderr. The handler is gone by then, so if the stderr reader has left, Go's default applies again: the write to fd 2 fails with EPIPE and the runtime kills the process with SIGPIPE. Every non-zero foreground exit is affected (`shutdown_budget_exceeded`, `shutdown_aborted`, boot failures). The documented exit code (`_spec.md` › Shutdown overruns, "exits 1"; `_dx.md:174`) turns into a signal death, status 141 in a shell. The success path is not affected: it prints nothing after `Run`, which is why IT-058 passes.

Runtime evidence. A throwaway test was added only through `go test -overlay`, so the tree is untouched. It uses the IT-058 helper `startCLIOnPipe` with `shutdown_grace = "1s"` and `hold-shutdown-step=3`, sends SIGTERM, and in one subtest closes the stderr reader first:

```
GOFLAGS="-count=1 -overlay=<scratch>/r3.json" go test -tags 'integration harness_testhooks' \
  -run TestReviewR3_OverrunExitWithDeadStderr -v ./internal/daemon/
live-reader: exit 1 (exit status 1)
dead-reader: exit -1 (signal: broken pipe)
--- FAIL: TestReviewR3_OverrunExitWithDeadStderr/dead-reader
```

The forced path itself ran normally inside `Run`: the overrun log record hit EPIPE while SIGPIPE was still notified, and the mirror switched off. Only the exit status is wrong. A supervisor that reads the status (launchd, systemd, a wrapper script) sees "killed by SIGPIPE" instead of "shutdown budget exceeded".

Suggested fix: keep SIGPIPE notified until the foreground process exits, not only for the length of `Run`. One way is for `runForeground` (or `cmd/harness` for the `daemon start --foreground` path only) to call `signal.Notify` for SIGPIPE before `runDaemon` and leave it in place, so the CLI's final write fails with EPIPE and the exit code stands. Other commands should keep the usual `| head` behaviour. Amendment (1) in `_spec.md` › "Before step 1" would then read "the foreground daemon process", not "`Run`". Extend IT-058 with an overrun case: a dead reader, `hold-shutdown-step=3`, a short grace, and exit 1 expected.

## Triage

- Decision: `VALID`
- Notes:
  - **Valid.** Reproduced on the real tree before any edit by the new `TestIT058_OverrunExitsOneWithoutAStderrReader` (real binary, `shutdown_grace=1s`, `hold-shutdown-step=3`, SIGTERM): `live-reader` exit 1, `dead-reader` `exit -1 (signal: broken pipe)`.
  - **Root cause.** `watch` took SIGPIPE only for the length of `Run` (`defer d.watch(ctx)()`). A foreground `Run` returning is not the end of the process: `runForeground` hands the `*ExitError` to `report`, which writes it to stderr after `Run` has returned and the handler is gone. With the reader gone the write fails with EPIPE, and Go's default (end the process with SIGPIPE) applies again. Every non-zero foreground exit was exposed (`shutdown_budget_exceeded`, `shutdown_aborted`, boot failures).
  - **Fix.**
    - `internal/daemon/signals.go`: the `signal.Notify` for SIGPIPE moved out of `watch` into the exported `TakeSIGPIPE() (release func())`, with the reasoning for `Notify` rather than `signal.Ignore` (an ignored signal is inherited across exec, a handled one is not) in its doc comment. `watch` still calls it, so `Run` is safe for any caller on its own.
    - `internal/cli/start.go`: `runForeground` calls `a.holdUntilExit(daemon.TakeSIGPIPE())` before `runDaemon`, so SIGPIPE stays handled across the CLI's final write.
    - `internal/cli/root.go`: `app.atExit`, `holdUntilExit` and `runAtExit`; `execute` runs the releases (in reverse order) once the command has returned and `report` has written. SIGPIPE is held only on the `daemon start --foreground` path (the detached child included); every other command keeps the default `| head` behaviour. `signal.Ignore` is not used.
  - **Tests.**
    - `TestIT058_OverrunExitsOneWithoutAStderrReader` (`integration && harness_testhooks`, subtests `live-reader` and `dead-reader`): exit 1 in both, the error report on stderr with the live reader, `shutdown budget exceeded` in the log, no file or lock left. `_tests.md`: IT-058 extended and the matrix row.
    - `TestForegroundStart/SIGPIPE_is_held_through_the_final_error_write` (UT-136, `internal/cli`): the hold is registered at every stderr write of the error report and released once `execute` returns.
    - Mutations (`go test -overlay`, tree untouched): `internal/cli/start.go` without the `holdUntilExit(daemon.TakeSIGPIPE())` line makes `dead-reader` fail with `exit -1 (signal: broken pipe), want 1` and UT-136 fail with `write 0 ran with 0 process-lifetime holds, want 1`.
  - **Spec amendment (needs operator confirmation).** `_spec.md` › Boot and shutdown sequences › "Before step 1": "`Run` also takes SIGPIPE and drops it, so a write to a closed stderr makes the log mirror fail instead of ending the process; the mirror switches itself off at its first write error and the file log goes on." now reads "The foreground daemon process also takes SIGPIPE and drops it, from before step 1 until it exits (the CLI's final write of an error report included), so a write to a closed stderr makes the log mirror fail instead of ending the process, and the exit code stands; the mirror switches itself off at its first write error and the file log goes on."
  - Verified: (review round 4, 2026-10-29) the fix matches the issue. `runForeground` (`internal/cli/start.go:90`) registers `daemon.TakeSIGPIPE()` with `holdUntilExit` before `runDaemon`, and `execute` (`internal/cli/root.go:102`) releases it in a `defer`, so the release runs after `report` on every return path. A usage error fails before `runForeground` and takes nothing. A second signal comes back as `shutdown_aborted`, and its report is written under the hold. A panic unwinds through the same `defer`, and the runtime's panic print does not go through the EPIPE check. `watch` still takes SIGPIPE for itself, so an in-process `Run` stays safe. The hold uses `signal.Notify`, not `Ignore`, so spawned children do not inherit it. `cli` → `daemon` was already an allowed import, and `make gate` passed in round 4 with `GOFLAGS=-count=1` (archtest included). Mutations through `go test -overlay` (with `GOFLAGS`, so the inner binary build used them too) both killed `IT-058 dead-reader` (`exit -1 (signal: broken pipe), want 1`, `process_integration_test.go:723`) and UT-136 (`write 0 ran with 0 process-lifetime holds, want 1`). The two mutations were (a) dropping the `holdUntilExit(daemon.TakeSIGPIPE())` line and (b) running `a.runAtExit()` before `report` in `execute`. The amendment is right: it restores the documented exit 1 and does not change any promise.
