---
provider: manual
pr:
round: 2
round_created_at: 2026-10-29T03:32:41Z
status: resolved
file: internal/daemon/signals.go
line: 56
severity: medium
author: claude-code
provider_ref:
---

# Issue 001: Foreground daemon is killed by SIGPIPE when its stderr pipe closes

## Review Comment

`daemon.Run` handles SIGINT, SIGTERM and SIGHUP (`internal/daemon/signals.go:56`), but nothing in the program handles or ignores SIGPIPE (`grep -rn SIGPIPE internal cmd` finds nothing). In Go, a write to fd 1 or 2 that fails with EPIPE kills the process with SIGPIPE unless the program has called `signal.Notify` for SIGPIPE. A foreground daemon mirrors every log record to `os.Stderr` (`internal/cli/start.go:92` → `logging.Options.Mirror`). `switchWriter.Write` (`internal/logging/logging.go:90-95`) intends that "a broken mirror never fails the log write", but the runtime kills the process inside `s.w.Write(p)`, before that comment can apply. So once the reader of the daemon's stderr is gone (`2>&1 | head -1`, `| grep -m1 running`, a log shipper that dies), the next log record that reaches the mirror ends the daemon with signal 13, wherever it is.

Runtime evidence. Binary built from this tree (`go build -o <scratch>/harness ./cmd/harness`), run on a throwaway `HARNESS_HOME`/`HOME` from `mktemp -d`, as `harness daemon start --foreground 2>&1 | sleep 2` (the reader exits after 2 s without reading), then a stop request after 3 s. Script: `<scratch>/r2/sigpipe.sh`, home removed afterwards.

- **Default config (`log.level` info), `harness daemon stop`:** `stop` reports `stopped (pid 6842, uptime 3s)`, but the daemon's exit status is `141` (SIGPIPE), not 0. The kill lands at shutdown step 10, on the `daemon stopped` record, after the ledger was closed and the lock released. Boot and shutdown step 10 "log `daemon stopped` and exit 0" is violated.
- **`log.level = "debug"`, SIGTERM:** exit status `141`. The last log record is `shutdown step 1 draining`. Left in the home: `daemon.sock`, `daemon.json`, `harness.db-wal`, `harness.db-shm`. Daemon stream: `config.loaded`, `daemon.started` only, so no `daemon.stopping` or `daemon.stopped` (Business Rule 10). The next `harness daemon start` recorded `3|daemon.unclean_shutdown_detected|{"pid":7355,"previous_pid":6948,"last_seq":2,"last_type":"daemon.started"}`. A SIGTERM-requested stop is therefore reported as a crash.

At the default level the same thing happens mid-run on the first warn or error record (for example `subscriber degraded`, `request failed`, `streams cut at shutdown`), and at debug level on any request (`request` record). The daemon then dies while running, without a graceful stop. Detached daemons are not affected: their stderr is `daemon.out`, a file, and the mirror is off once running.

No test covers a foreground daemon whose stderr reader has gone away.

Suggested fix: in `Run`, next to the other handlers, also call `signal.Notify` for `syscall.SIGPIPE` (a buffered channel that nobody reads is enough). With SIGPIPE notified, the runtime returns EPIPE from the write instead of killing the process, and `switchWriter` already ignores the error. Optionally, `switchWriter` should turn itself off after the first write error, so a dead pipe costs one failed syscall rather than one per record. Do not use `signal.Ignore(SIGPIPE)`: an ignored disposition is inherited across `exec`, and the agent CLIs that later features spawn would start with SIGPIPE ignored. Add an integration test (`integration && harness_testhooks` process suite): a foreground daemon whose stderr pipe reader is closed after `daemon running`, then SIGTERM. Expect exit 0, `daemon.stopping` and `daemon.stopped` in the stream, and no files left in the home.

## Triage

- Decision: `VALID`
- Notes:
  - **Valid.** Reproduced before the fix by `TestIT058_ForegroundDaemonSurvivesItsStderrReader` (real binary, stderr on a pipe whose reader the test closes): at `info` the daemon exited `signal: broken pipe` instead of 0 (killed at the `daemon stopped` record), and at `debug` a `status` request after the close killed it while it was running. `UT-083` also failed before the fix (the mirror was written once per record, 4 times for 4 records).
  - **Root cause.** Nothing in the program asked for SIGPIPE. Go ends a process whose write to descriptor 1 or 2 fails with EPIPE unless the program called `signal.Notify` for SIGPIPE, so the kill happened inside `os.Stderr.Write`, before `switchWriter` could swallow the error. Separately, `switchWriter` kept writing to a dead mirror on every record.
  - **Fix.**
    - `internal/daemon/signals.go`: `watch` also calls `signal.Notify` for SIGPIPE on a buffered channel nobody reads, and `signal.Stop` on it with the other handlers. `signal.Ignore` was rejected as the reviewer advised: an ignored disposition is inherited across `exec`, a handled one is reset in the child.
    - `internal/logging/logging.go`: `switchWriter.Write` switches the mirror off at its first write error. The log write never fails, the file log keeps every record, and a dead pipe costs one failed write, not one per record. (A reader that is alive but stopped, so the pipe is full, still blocks the write; that is ordinary pipe behaviour, not addressed here.)
  - **Tests.** `TestIT058_ForegroundDaemonSurvivesItsStderrReader` (`integration && harness_testhooks`, subtests `info` and `debug`; helper `startCLIOnPipe` in `process_helpers_integration_test.go`) and `TestLogger_BrokenMirrorStopsMirroringAndKeepsTheFile` (UT-083). Both fail without the fix. `_tests.md`: IT-058, UT-083 and a matrix row.
  - **Spec amendment (needs operator confirmation).** `_spec.md` › Boot and shutdown sequences › "Before step 1": added "`Run` also takes SIGPIPE and drops it, so a write to a closed stderr makes the log mirror fail instead of ending the process; the mirror switches itself off at its first write error and the file log goes on."
  - **Verified (round 3, 2026-10-29):** `watch` notifies SIGPIPE on an unread buffered channel (`internal/daemon/signals.go:67-68`) and stops it with the other handlers; `switchWriter.Write` switches the mirror off at its first error (`internal/logging/logging.go:110-116`). Without SIGPIPE notified (overlay mutation of `signals.go`, passed through `GOFLAGS` so the test's inner `go build` also used it), `TestIT058_ForegroundDaemonSurvivesItsStderrReader` failed in both subtests with `signal: broken pipe`; without `s.on.Store(false)`, UT-083 failed (`the mirror was written 4 times, want 2`). On the real tree IT-058 passed twice (`-race -count=2 -tags 'integration harness_testhooks'`) and UT-083 passed. Resolved. One related gap is tracked separately in round 3 (`reviews-003/issue_001.md`): the handler goes away when `Run` returns, before the CLI writes `Run`'s error to stderr.
