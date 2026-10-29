---
provider: manual
pr:
round: 2
round_created_at: 2026-10-29T03:32:41Z
status: resolved
file: internal/daemon/shutdown.go
line: 171
severity: medium
author: claude-code
provider_ref:
---

# Issue 003: Overrun at shutdown step 7 releases the lock while the ledger is closing

## Review Comment

On a budget overrun or a second signal at shutdown step 7, `runShutdownStep` abandons the goroutine that runs `d.led.Close(ctx)` (`internal/daemon/shutdown.go:141-147`). Then `force()` calls `d.led.Close(expired)` (`shutdown.go:169-173`). `ledger.Close` runs only once: the abandoned call is the first caller and is still inside `shutdown()` (`internal/ledger/ledger.go:279-299`, stopping the writer, `<-l.done`, then `rdb.Close()`/`wdb.Close()`, which checkpoints the WAL into `harness.db` and deletes it). The second caller, `force`, has an expired ctx, so it returns `ctx.Err()` at once (`ledger.go:271-275`). `force` then removes the files and releases the lock (`shutdown.go:175-180`) while this process still has the database open and is still writing to it.

This breaks Safety Invariant 1 and Business Rule 2 (only the lock owner writes `harness.db`), and the documented overrun order: "force-closes the listener and the ledger, removes its files, releases the lock". A new owner that takes the lock in that window opens a ledger that another process is still checkpointing. In-process callers of `daemon.Run` (reason `context`, "tests and embedding") keep the abandoned goroutine alive after `Run` returns, so for them the window lasts as long as the close does. In the binary it lasts until the process exits.

Runtime evidence. This was a throwaway test, added only through `go test -overlay` (the tree is untouched; the source is in the reviewer's scratchpad as `zz_review_r2_test.go`). It sets `shutdown_grace = "1s"`. From `BeforeShutdownStep(7)` it fills the writer queue with 300 appends of 900 kB, so `Close` has to drain it, and then cancels `Run`:

```
go test -count=1 -tags integration -overlay <scratch>/ov_b.json \
  -run TestReviewR2_OverrunAtLedgerCloseReleasesLockEarly -v ./internal/daemon/
Run returned ok=true err=shutdown exceeded its budget of 1s at step 7 (ledger);
  lock free=true; committed so far=86; wal exists=true
wal removed 9.400833ms after Run returned (lock already free)
--- FAIL
```

So the lock was free when `Run` returned, and about 9 ms later this process checkpointed and deleted `harness.db-wal`, writing the ledger without the lock. No existing test covers an overrun at step 7. The overrun tests hold earlier steps, where `force` is the first `Close` caller and waits.

Suggested fix: `force` must not release the lock until the ledger is actually closed. The close that is already running is bounded: its ctx has expired, so it is on the `l.stop()` path (one in-flight commit plus the connection close). Expose the ledger's `finished` channel (for example `Ledger.Closed() <-chan struct{}`) or let a second `Close` wait for `finished` once the first caller's ctx has ended. Then have `force` close with an expired ctx and wait for it before `removeFiles`/`releaseLock`. Add an integration test along the lines above: an overrun at step 7 with a busy writer, then assert that `harness.db-wal` is gone and the writer has stopped before the lock is free.

## Triage

- Decision: `VALID`
- Notes:
  - **Valid.** Reproduced before the fix by `TestIT059_OverrunAtStepSevenKeepsTheLockUntilTheLedgerIsClosed` (an in-process `daemon.Run`, writer queue kept full, budget ending inside `ledger.Close`): with the old `force`, 5 of 5 runs failed all three checks (lock seen free while `Ledger.Closed()` was still open; `Run` returned before the ledger was closed; `harness.db-wal` still present at return).
  - **Root cause.** `ledger.Close` runs once. When step 7 overran, its own `Close` (abandoned by `runShutdownStep`) was still in `shutdown()` (`l.stop()`, `<-l.done`, then `rdb.Close()`/`wdb.Close()`, which checkpoints and deletes the WAL). `force` called `Close` with an expired ctx; as the second caller it returned `ctx.Err()` at once (`ledger.go` second-caller branch), and `force` then removed the files and released the lock while this process could still write `harness.db`. Safety Invariant 1 and Business Rule 2 broken.
  - **Fix.**
    - `internal/ledger/ledger.go`: new `Ledger.Closed() <-chan struct{}`, closed when the first `Close` has completed (the writer returned and the connections are closed). `Close` keeps its behaviour; its doc says to wait on `Closed` when the caller must know the close has finished.
    - `internal/daemon/shutdown.go`: `force` delegates to `forceDown(led ledgerCloser, wait)`. `closeLedgerByForce` closes with an expired ctx and then waits on `Closed()` for up to `ledgerCloseWait` (2 s). Files are removed while the lock is still held. The lock is released only if the ledger closed; otherwise the lock is kept, `lock kept: the ledger did not finish closing` is logged, and `Run` still returns `shutdown_budget_exceeded` (exit 1), and the process exit releases the lock and ends every write at the same instant. This keeps the exit bounded (budget + at most 2 s) and never frees the lock while the ledger may write. `ledgerCloser` is the small consumer interface (ADR-003) that makes the bound testable.
    - Same path for the second-signal abort (`shutdown_aborted`): it calls the same `force`.
  - **Tests.**
    - `TestIT059_OverrunAtStepSevenKeepsTheLockUntilTheLedgerIsClosed` (`integration`): fails without the fix (5 of 5 with a mutated `closeLedgerByForce` that does not wait); passes with it.
    - `TestForceDown_ReleasesTheLockOnlyOnceTheLedgerIsClosed` (UT-134) and `TestForceDown_KeepsTheLockWhenTheLedgerDoesNotClose` (UT-135): both fail without the wait (deterministic, fake ledger), plus `TestForceDown_ClosedLedgerOrNoLedgerReleasesTheLock`.
    - `TestClosed_WaitsForTheCloseAnotherCallerStarted` (UT-109, ledger).
    - `_tests.md`: UT-109, UT-134, UT-135, IT-059, matrix rows incl. Safety Invariant 1.
  - **Spec amendment (needs operator confirmation).** `_spec.md` › Shutdown overruns: appended "The lock is released only once the ledger's close has finished, including a close that step 7 started and the budget abandoned (Safety Invariant 1): the forced path waits up to 2 s for it, and a close that has not finished by then leaves the lock held, so that the exit releases it."
  - **Verified (round 3, 2026-10-29):** `Ledger.Close` closes `finished` right after `shutdown` returns on the first caller's only path. `shutdown` has no early return, so the channel closes on success, on a drain interrupted by ctx and when `rdb`/`wdb.Close` return errors. `forceDown` holds no lock and waits on nothing that the Run goroutine owns. Its `Close(expired)` is either the first caller, which is bounded by one interrupted commit because queued requests fail fast once `life` is cancelled and `res` is buffered, or a second caller that returns at once, followed by the 2 s `Closed()` wait. No deadlock path found. Mutations through `go test -overlay`: (a) `closeLedgerByForce` returning true without waiting made UT-134 and UT-135 fail and IT-059 fail 3 of 3 (`Run returned before the ledger was closed`, `harness.db-wal still exists`, and in 2 of 3 runs `the lock was free while the ledger was still open`); (b) closing `finished` before `shutdown` made UT-109 fail (`Closed is closed while the first Close is still waiting for the writer`). On the tree IT-059 passed 20 of 20: `-race -count=5`, `-count=10` without race and `-race -cpu 1 -count=5`. Each run overran at step 7 and `Run` returned at about 1.016 s (1.4 s on one CPU). UT-109, UT-134 and UT-135 passed under `-race`. Resolved. Residual risk, not filed because nothing reproduces it: when the forced Close is the first caller (an overrun before step 7), the 2 s bound does not apply to that Close itself, and only a commit stuck in the kernel (a hung disk) could hold it. That was also true before this fix.
