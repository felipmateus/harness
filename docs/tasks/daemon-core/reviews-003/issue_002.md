---
provider: manual
pr:
round: 3
round_created_at: 2026-10-29T04:05:35Z
status: resolved
file: internal/daemon/boot.go
line: 155
severity: low
author: claude-code
provider_ref:
---

# Issue 002: A second signal during the boot-cancel undo is ignored

## Review Comment

`_spec.md:713` and `_dx.md:174` say that a second signal during shutdown exits immediately with code 1. `watch` (`internal/daemon/signals.go:48-50`) implements that by closing `d.abort` on any signal that comes once a stop is under way. Only the shutdown sequence (`runShutdownStep`, `internal/daemon/shutdown.go`) selects on `d.abort`. When the stop request arrives during boot, `cancelBoot` (`internal/daemon/boot.go:142-156`) and `failBoot` (`boot.go:120-133`) run `d.unwind(ctx)` under the `shutdown_grace` budget, and nothing watches `d.abort`. A second Ctrl-C there is dropped. The undo runs to the end, or to the end of the budget, and `Run` returns nil (exit 0).

The undo is usually fast, so this only shows when an undo action is slow. The server's undo is one such action: it is `shutdownServer` → `http.Server.Shutdown(ctx)` (`boot.go:436`), which waits for a connection that has sent nothing until it is about 5 s old (`reviews-003/issue_003.md`).

Runtime evidence. This was a throwaway in-process test, added through `go test -overlay`. `AfterBootStep(10)` (listen) is held, a unix-socket client connects and sends nothing, then `Run`'s ctx is cancelled and the hook is released. A SIGTERM goes to the test process 300 ms later. The control subtest does the same during a running shutdown:

```
GOFLAGS="-count=1 -overlay=<scratch>/r3b.json" go test -tags integration \
  -run TestReviewR3_SecondSignalWithSilentClient -v ./internal/daemon/
boot-undo: returned=true 6.120875666s after the second signal, err=<nil>
shutdown: returned=true 11.652ms after the second signal, err=shutdown interrupted by a second signal at step 6 (server)
```

So the same pair of stop requests ends at once with exit 1 in one stop path, and 6 s later with exit 0 in the other.

Suggested fix: let the boot undo observe `d.abort`. One way is to derive the undo ctx from the budget and cancel it when `d.abort` closes, so `http.Server.Shutdown` returns and `shutdownServer` falls back to `Close`. Then return `shutdown_aborted` (exit 1), and still remove the files and release the lock on the way out. Another is to state in `_spec.md` that the second-signal rule covers the shutdown sequence only, if that is the intent. Add an in-process test along the lines above.

## Triage

- Decision: `VALID`
- Notes:
  - **Valid.** `_spec.md` (Before step 1), `_dx.md:174` and US-002 EC-2 promise that a second signal while the stop is under way exits at once with code 1. Reproduced on the real tree before the fix by `TestIT061_SecondSignalDuringTheBootUndoExitsAtOnce` (both subtests: `Run did not return after the second signal`, 10 s wait). The slow undo action is `http.Server.Shutdown` waiting on a request in flight (a POST to the shutdown route that announced a body and sent none; the server's `100 Continue` shows the handler is reading it); that keeps the test independent of issue_003, whose fix removes the silent client as the slow action.
  - **Root cause.** `watch` closes `d.abort` on the second signal, but only `runShutdownStep` selected on it. `cancelBoot` and `failBoot` ran `d.unwind(ctx)` under the `shutdown_grace` budget with nothing watching `d.abort`: the signal was dropped, the undo ran to its end or to the end of the budget, and `Run` returned nil (exit 0) for a cancelled boot.
  - **Fix** (`internal/daemon/boot.go`).
    - `runUndo(work)` (`runUndoWait` with the wait given) runs the records and the undo of an ending boot under the budget and selects on `d.abort`. On the second signal it cancels the context the undo runs under: `shutdownServer` falls back to `Close`, a record is not appended, the ledger aborts the transaction in flight and closes. The actions that are left still run in stack order, so the files go and the lock is released. The lock release is the undo action after the ledger's close, and a boot has no other `Close` going on (the first `Close` is synchronous), so the lock is still released only once the ledger has finished closing (Safety Invariant 1, the round 2 guarantee). An action that never returns is waited for `ledgerCloseWait` (2 s, the forced shutdown's bound) and then left running; the lock stays held and the exit that follows releases it, as in `forceDown`. A second signal that came before the undo began cancels the context before the work starts.
    - `cancelBoot` returns `*ExitError{1, "shutdown_aborted"}` instead of `errBootCancelled` when interrupted, so `Run` exits 1 and a detached child reports the line.
    - `failBoot` also goes through `runUndo`, and keeps returning the boot failure's own `ExitError`: the boot already ends with a non-zero code and a specific cause, and a failure that has a recognised code (steps 1-6) has a trivial undo that no second signal can interrupt, so the code is 1 whenever the undo is slow.
  - **Tests.**
    - `TestIT061_SecondSignalDuringTheBootUndoExitsAtOnce` (`integration`, in-process, real SIGTERM/SIGHUP to the test process, subtests `cancelled boot` and `failed boot`).
    - `TestRunUndo_ASecondSignalEndsTheContextAndTheRestStillRuns`, `TestRunUndo_ASecondSignalBeforeTheUndoEndsItsContextAtOnce` (UT-137) and `TestRunUndo_KeepsTheLockWhileAStuckLedgerCloseHasNotReturned` (UT-138, the lock is held until the stuck close returns).
    - Mutation (`go test -overlay`, tree untouched): in `runUndoWait`, the `case <-d.abort:` replaced by a channel that never fires makes both IT-061 subtests fail (`Run did not return after the second signal`) and UT-137 and UT-138 fail (`the undo did not return after the second signal`, `the undo did not give up on the stuck action`). The "before the undo began" test is not killed by removing the pre-cancel, because the select that follows cancels before the work goroutine usually gets to run; the pre-cancel makes it deterministic, but the test cannot prove it.
  - **Spec amendment (needs operator confirmation).** `_spec.md` › Boot and shutdown sequences › "Before step 1": "A second signal during shutdown exits immediately with code 1." now reads "A second signal while a stop is under way, in the shutdown or in the undo of a boot that is ending, exits immediately with code 1 (`shutdown_aborted`; a boot that is failing reports its own error)." The text of US-002 EC-2 and `_dx.md:174` is unchanged.
  - Verified: (review round 4, 2026-10-29) the fix matches the issue. `runUndoWait` (`internal/daemon/boot.go:207`) cancels the undo's context on `d.abort`, and `unwind` keeps running the remaining actions once each, newest first. In a boot the first and only ledger `Close` is the `close ledger` undo action itself. That call is synchronous: with an expired context it aborts the transaction in flight, waits for the writer (`<-l.done`) and closes both connections before it returns. `release lock`, which comes later in the stack, therefore still runs only after the ledger has finished closing (Safety Invariant 1). The round 2 guarantee holds. A stuck action makes `runUndoWait` give up after `ledgerCloseWait`. The goroutine it leaves behind still holds the lock, and releases it only if the stuck action returns. This is the same accepted trade-off as `forceDown`, and the process exits right after. No action runs twice and none is skipped: the remaining actions run with a cancelled context, and every one of them (shutdownServer → `Close`, `removeIfExists`, `Ledger.Close`, `Lock.Release`) copes with that. Mutation through `go test -overlay`: `case <-d.abort:` in `runUndoWait` replaced by a channel that never fires. It killed both IT-061 subtests and UT-137 and UT-138, as the fixer reported. Removing the pre-cancel survives (`ok`). That is not a real gap: with `d.abort` already closed, the `select` that follows cancels within microseconds, before any action can block. The only difference is that a record started in those microseconds may still be appended. No contracted outcome changes, and neither exit 1 nor the lock order depends on it. The amendment is right: US-002 EC-2 and `_dx.md:174` read as the user sees a stop, and a cancelled boot is one. Keeping a failing boot's own error code is the more informative report, and that code is non-zero in any case.
