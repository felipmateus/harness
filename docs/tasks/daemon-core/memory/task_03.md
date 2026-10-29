# Task 3 memory

## Objective Snapshot

`internal/broker` and `internal/stream`, completed 2026-10-28:
- `broker.Hub`, the in-memory `Broker`: per-stream subscriber sets, 64-event buffers, degrade-and-remove on overflow, `Done`, idempotent `Close`, safe late `Subscribe`;
- `stream.Follow` with `Item`, `ErrClosed`, `Reader`, `Subscriber`, `CatchUpPage`;
- tests for UT-030–UT-034, UT-060–UT-064, UT-066, UT-110, UT-111, plus supplementary cases;
- the shared leak-check helper `internal/testutil/leakcheck`.

## Important Decisions

- **The concrete broker is `broker.Hub`, built with `broker.New()`.** The spec only names the `Broker` and `Subscription` interfaces; ADR-003 says constructors return concrete types. `Hub` satisfies `Broker` (asserted at compile time). One mutex guards the subscriber sets and every send on or close of a subscription channel, so a send never races a close; `Publish` holds it only for non-blocking sends. `Hub.Close` closes `Done` first, then every subscription (not degraded), all under the lock.
- **The output channel of `Follow` has a small buffer (`outBuffer = 8`).** Without it the terminal `ErrClosed` ("send, non-blocking") is lost whenever the consumer is between two receives at the instant the broker closes, which makes UT-060 and UT-110 flaky and would drop the shutdown notice of a busy SSE adapter. The buffer is not a backpressure hider: a full buffer is exactly the stuck-consumer case the `Done` escape handles.
- **`send` is data-first.** `ctx` ends the follow first; then an item that the consumer (or the buffer) can take right now is handed over even if the broker has just closed; only then does it wait on `out`, `ctx.Done()` and `s.Done()`. Pure `select` would pick randomly between the item and `Done` and drop the final events (`daemon.stopping`, `daemon.stopped`) half the time, which breaks US-004.EC-2. A stuck consumer still never blocks: the wait is the normal three-way select.
- **Terminal `ErrClosed` is best-effort, as the spec's "non-blocking" says.** If the consumer's buffer is full when the broker closes, the channel closes without a terminal item. `Follow`'s doc states the rule: a consumer that sees the channel close while its own ctx is live treats it as `ErrClosed`.
- **The catch-up checks `ctx` and `s.Done()` before each page (spec) and once more right after each `Read` returns**, so a broker that closes during a `Read` ends the follow without delivering the page it just read. `ctx` wins when both fired (no terminal item). UT-110 holds the second `Read` and closes the broker during it: exactly 2 reads, then `ErrClosed`.
- **A read error after the broker closed is reported as `ErrClosed`**, not as the read error (the ledger closes right behind the broker at shutdown).
- **Two defensive terminal errors in catch-up, for Safety Invariant 6:** a page whose first event is not `cursor+1`, and `HasMore` with no events. Both mean a corrupt ledger; the follow ends with a descriptive error instead of delivering a gap or spinning.
- **A negative `after` is treated as 0** (the API rejects it; an in-process caller cannot make the cursor logic misfire).
- **A ctx that ends closes the channel without a terminal item**, so `ctx.Err()` is never delivered as an `Item` even though the spec's `Item` comment lists it.
- **UT-110 uses an in-memory gap-free reader (`memLedger`, test-only).** 5,000 real appends would take about 30 s at 6 ms each. The cases that exercise the ledger itself (UT-060–UT-064, UT-066, and one real-ledger-plus-real-broker case) use a real ledger with small counts.

## Learnings

- **UT-111 must not read the channel to prove the goroutine exited.** The consumer is stuck by definition, so the test counts goroutines (`leakcheck.Settle(baseline, 100ms)`) after `Close`.
- **A consumer that never reads makes the subscription degrade deterministically:** `Follow` takes `outBuffer + 1` events, the rest pile up in the 64-event subscription, and the 65th overflows. The real-ledger case relies on this with `1 + outBuffer + 1 + 64 + 10` appends (about 0.6 s).
- **Fast-subscriber tests need pacing.** UT-032 publishes in batches of 32 and waits for all 100 readers, otherwise a reader goroutine that is not scheduled in time overflows its 64-event buffer and degrades (a correct broker behaviour, but not what the case tests).
- Mutations applied and reverted, each making the intended tests fail: subscribe after the first read, no dedupe, stop on a short page, no resubscribe, no pre-page check, no post-read check, no `Done` in `send`, blocking `Publish`, open late subscription, `Close` that degrades.

## Files / Surfaces

- New: `internal/broker/{broker.go,broker_test.go}`, `internal/stream/{follow.go,follow_test.go,helpers_test.go}`, `internal/testutil/leakcheck/leakcheck.go` (`Baseline`, `Settle`, `Check`).
- `docs/roadmap.md`: feature 1 now shows "tasks 1–3 done".
- No new module dependency. `broker` imports only `ledger`; `stream` imports only `ledger` and `broker` (checked with `go list` and the architecture test).

## Errors / Corrections

- The first version of UT-031 called `t.Fatal` from a goroutine; rewritten to report through a channel.
- My first `assertIdle` treated a closed channel as an unexpected receive in the late-subscribe test; replaced by a direct check that the closed channel yields no event.

## Ready for Next Run

**Verification evidence (macOS, Go 1.27.3):**
- `go test -race -count=1 ./internal/broker/ ./internal/stream/`: green; `-count=10` and `-cpu 1,2,8` green; six parallel copies of each test binary with `-count=10 -cpu 1,4` green (no flake under load).
- `go test -race -count=1 ./internal/archtest/`: green.
- `make gate`: green (gofmt, `go vet ./...`, `go test -race ./...`, build).
- After the runs, `~/.harness` did not exist and no `/tmp/hz*` directory was left.

**For task 4 (SSE adapter):**
- Call `stream.Follow(ctx, ledger, broker, stream, after)` through the follow function injected into the adapter; `api` never imports `broker`. Cancel the follow ctx when the request ends, or the goroutine lives until the broker closes.
- Treat a closed follow channel while the request ctx is still live as `ErrClosed`: write the `shutdown` frame (the terminal item can be dropped when the consumer's buffer was full at close).
- An `Item` is either an event or the final error; `ErrClosed` means shutdown, any other `Err` means `event: error`.
- The spec's `subscriber degraded` log record (`stream`, `cursor`, warn) cannot be written inside `Follow` or the broker (neither may import logging). If it is wanted, the daemon or the adapter must observe it; recorded as a follow-up, not done here.

**For task 5 (daemon):**
- `h := broker.New()`; `ledger.Open(ctx, Options{Path, OnCommit: h.Publish})` (`Publish` never blocks, as `OnCommit` requires); pass `h` to `stream.Follow` as the `Subscriber`; call `h.Close()` at shutdown step 5 (idempotent).
- IT-030–IT-035 and IT-050 are the integration checks for this task's behaviour over the socket.
