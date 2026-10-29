---
provider: manual
pr:
round: 1
round_created_at: 2026-10-29T03:13:03Z
status: resolved
file: internal/daemon/boot.go
line: 40
severity: medium
author: claude-code
provider_ref:
---

# Issue 002: Boot cancelled after step 7 leads to a false unclean shutdown

## Review Comment

`_spec.md` (When the boot fails or is cancelled) says: "Cancellation in steps 6–9: undo only. The previous run's last event still stands, so no false unclean event is produced." That holds only when step 7 appended nothing. When the previous run ended uncleanly, step 7 appends `daemon.unclean_shutdown_detected` with *this* boot's PID (`boot.go:327-339`). If a stop request (SIGTERM, `POST /api/daemon/shutdown` cannot arrive before step 10, but a signal or ctx cancel can) is noticed at the step-7 or step-8 boundary, `cancelBoot(done=7|8)` skips recording because `firstRecordedStep = 9` (`boot.go:40`, check at `boot.go:150`), and only undoes.

The last `daemon` event is then `daemon.unclean_shutdown_detected`, which `isCleanEnd` does not accept (`events.go:99-101`). So the next start records a second `daemon.unclean_shutdown_detected` with `previous_pid` = the cancelled boot's PID and `last_type: "daemon.unclean_shutdown_detected"`. That is a false report about a run that stopped cleanly on request, which is exactly what the spec rule is meant to prevent.

Failure mode (deterministic from the code): kill -9 a daemon; run `harness daemon start --foreground`; send SIGTERM while it is between step 7 and step 9 (in a hook-enabled build: `hold-boot-step=7` + SIGTERM). Then start again: the stream gains a second unclean event naming the cancelled PID.

The implementer recorded this in `memory/MEMORY.md` (Open Risks: "A boot cancelled between steps 7 and 9 after an unclean event was appended ...") and left it for the conductor. No decision is recorded, and no test covers it. IT-055 cancels only at step 10.

Suggested fix (needs a one-line spec amendment): once this boot has appended any event, record `daemon.stopping`/`daemon.stopped` on cancellation. For example, set a `recorded` flag in `stepUnclean` when it appends, and use `done >= firstRecordedStep || d.recorded` in `cancelBoot`. Add an IT with `hold-boot-step=7` after a SIGKILLed run that asserts the next start records no unclean event.

## Triage

- Decision: `VALID`
- Notes:
  - **Valid.** The spec's "Cancellation in steps 6–9: undo only" assumes steps 6–8 append nothing, but step 7 appends `daemon.unclean_shutdown_detected` when the previous run ended uncleanly. A stop noticed at the step 7 or step 8 boundary then left that event as the last `daemon` event, and `isCleanEnd` rejects it, so the next start recorded a second unclean shutdown naming the cancelled boot's PID. That is a false report about a run that stopped on request, the exact outcome the spec rule wants to prevent. Reproduced with a failing test before the fix: the cancelled boot added only `daemon.unclean_shutdown_detected`.
  - **Root cause.** `cancelBoot` decided whether to record by step number (`done >= firstRecordedStep`, 9) instead of by whether this boot had put an event in the stream.
  - **Fix.** `daemon.appended` (an `atomic.Bool`, `internal/daemon/run.go`) is set by `record` after every successful append (`internal/daemon/events.go`). `cancelBoot` records `daemon.stopping` and `daemon.stopped` when it is set, and only undoes otherwise (`internal/daemon/boot.go`). The `firstRecordedStep` constant is gone: step 9 always appends, so the one rule covers it. Nothing changes for a boot that appended nothing (fresh home, or a previous run that ended cleanly). `failBoot` already recorded `daemon.boot_failed` once the ledger was open, so a failure after step 7 was never affected.
  - **Contract amended (this is the one-line spec change the finding asked for, and a decision for the operator to confirm).** `_spec.md` Business Rule 12 and "When the boot fails or is cancelled" now read: cancellation once this boot has appended an event records `daemon.stopping` and `daemon.stopped`; cancellation in steps 6–8 that appended nothing is undo only. `_tests.md` gains IT-057 and a matrix entry. `memory/MEMORY.md`: the open risk is removed and the change is listed under Shared Decisions.
  - **Tests.**
    - New `TestBootCancelledAfterAnUncleanEventRecordsItsStop` (`internal/daemon/resilience_integration_test.go`, `integration` tag, in-process hold hook): a ledger seeded with a crashed run, then a cancel held after step 6 (control: nothing appended, the next start reports the crash once with `previous_pid` 4242), after step 7 and after step 8 (the stream gains the unclean event, `stopping` with reason `context` and `stopped`, both with this boot's PID; the next start adds only `config.loaded` and `daemon.started`). Steps 7 and 8 failed before the fix.
    - New IT-057 `TestIT057_SIGTERMAfterTheUncleanCheckLeavesNoFalseUncleanShutdown` (`internal/daemon/process_integration_test.go`, `integration && harness_testhooks`): SIGKILL a daemon, start `--foreground` with `hold-boot-step=7`, SIGTERM once the unclean shutdown is logged, expect exit 0 and `unclean, stopping{signal}, stopped`, then a normal start adds only `config.loaded` and `daemon.started`.
    - Mutation check: with the condition put back to `done >= 9`, IT-057 and the step 7 and 8 subtests fail; restored afterwards. Existing `TestBootCancellationAtEveryBoundary`, `TestBootCancelledBeforeConfigLoadedAddsNoUncleanEvent` and IT-055 still pass.
  - Verified: (round 2, 2026-10-29) `record` sets `daemon.appended` after a successful append (`internal/daemon/events.go:82`) and `cancelBoot` records `daemon.stopping`/`daemon.stopped` when it is set (`internal/daemon/boot.go:147-154`). `go test -count=1 -tags integration -run 'TestBootCancelledAfterAnUncleanEventRecordsItsStop|TestBootCancellationAtEveryBoundary|TestBootCancelledBeforeConfigLoadedAddsNoUncleanEvent' ./internal/daemon/` passes. Mutation check with `go test -overlay` (tree untouched), condition put back to `done >= 9`: the step 7 and step 8 subtests fail (`the cancelled boot added [daemon.unclean_shutdown_detected], want [... daemon.stopping daemon.stopped]`); the step 6 control still passes. IT-057 (`process_integration_test.go:612`) asserts the same end to end with a real SIGTERM and was read, not rerun (the fixer ran `make test-integration-hooks` green on this tree).
  - Amendment judgement: the right call, not a contract bent to fit the code. The old "steps 6–9: undo only" rested on a false premise (that steps 6–8 append nothing) and contradicted its own stated purpose ("no false unclean event"). The new Business Rule 12 and bullets keep that purpose, key the decision on the observable fact (has this boot appended an event) and agree with Rules 10 and 13. Nothing in production reads `daemon.stopping`/`daemon.started` pairs, so a stop without a start breaks no consumer. `_user_stories.md` US-004 EC-6 still says "if it had already recorded `config.loaded`", but that edge case is about `stop`, which can only reach a booting daemon through the API after step 10, so it stays true. Operator confirmation still pending, as agreed. Resolved.
