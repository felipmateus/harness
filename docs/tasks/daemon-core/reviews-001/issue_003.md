---
provider: manual
pr:
round: 1
round_created_at: 2026-10-29T03:13:03Z
status: resolved
file: internal/daemon/boot.go
line: 308
severity: low
author: claude-code
provider_ref:
---

# Issue 003: subscriber degraded logs seq, not the spec's cursor field

## Review Comment

The `_spec.md` Monitoring table defines the record `subscriber degraded` with fields `stream`, `cursor` (warn). The daemon logs `stream` and `seq` (`boot.go:306-309`). `seq` is the sequence of the event that did not fit, which is not the follower's cursor: the broker cannot see the cursor (`broker.go:64-71`). The other stream records keep `cursor` (`api/sse.go:122-126`, `api/sse.go:152-156`).

This deviates from a documented observability contract, and no decision is recorded. `memory/MEMORY.md` Open Risks says: "The conductor should amend the row or accept the difference", but the spec row is unchanged. An operator or agent that greps the log for the documented `cursor` field finds nothing.

Suggested fix: pick one and record it. Either (a) amend the spec row to `stream`, `seq` (the event that overflowed) with a note on why, or (b) have `stream.Follow` log or report its cursor when it sees `Degraded()` (it holds `f.cursor`; the daemon could pass a logger or callback into the follow closure). IT-033 asserts only that the message appears, so the test needs no change under (a).

## Triage

- Decision: `VALID`
- Notes:
  - **Valid as a finding:** the code and the Monitoring table disagreed, and no decision was recorded. Resolved by **amending the contract, not the code**, because the spec's field is the one that cannot be produced reliably.
  - **Why `cursor` is wrong for this record.**
    - The record's only reliable observation point is the broker's degrade observer (`broker.OnDegrade`), which runs when the subscription overflows. It sees the stream and the `seq` of the event that did not fit, not a cursor.
    - A follower cannot log it instead. `stream.Follow` reaches the `sub.Degraded()` check (`internal/stream/follow.go`, `live`) only after it has drained the closed subscription's channel. A stalled client leaves the follower blocked in `send` (the output channel is full), so the check is never reached, and the record would be missing for the very case it exists for. When a follower does notice, its cursor has advanced to about `seq - 1`, so `cursor` would add no information over `seq`.
    - Making `stream.Follow` report its cursor would also change the spec-fixed signature of `Follow`, and leave `broker.OnDegrade` (with its tests) without a user.
  - **Change.** `_spec.md` › Monitoring: the row is now `subscriber degraded | stream, seq (warn)`, with a note under the table saying why (who logs it, what `seq` is, why a follower `cursor` is not used). The other stream records keep `cursor`, because the SSE adapter knows it. `_tests.md` IT-033 now says the record carries `stream` and the `seq` of the event that did not fit; the test (`TestIT033_SlowFollowerIsDegradedAndCaughtUp`) already asserts level, stream and `seq >= 65`, so no test change is needed. `internal/daemon/boot.go` only gains a comment pointing at the spec; behaviour is unchanged. `memory/MEMORY.md`: the open risk is removed and the decision is recorded.
  - **For the operator:** if you prefer a follower `cursor` in the record, that needs a follow-up that changes `stream.Follow`; the trade-off above applies.
  - Verified: (round 2, 2026-10-29) The `_spec.md` Monitoring row now reads `subscriber degraded | stream, seq (warn)` with an explanatory note (`_spec.md:916` and `:922`), `_tests.md` IT-033 says the same, and `internal/daemon/boot.go:303-307` logs `stream` and `seq`. `TestIT033_SlowFollowerIsDegradedAndCaughtUp` asserts level WARN, stream and `seq >= 65` (`internal/daemon/streams_integration_test.go:167-171`).
  - Amendment judgement: the right call. The fixer's reasoning holds in the code: `stream.Follow` checks `sub.Degraded()` only after it has drained the closed subscription channel (`internal/stream/follow.go:161-165`), and a follower whose client stopped reading is parked in `send` (`follow.go:220-228`), so a follower-side record would be missing in exactly the case it exists for. The broker's `OnDegrade` callback is the only reliable observation point and sees only `(stream, seq)`. Getting a real cursor would need a new channel from `Follow` back to the daemon and would change the spec-fixed `Follow` signature for no diagnostic gain (`seq` already locates the overflow). Operator confirmation still pending, as agreed. Resolved.
