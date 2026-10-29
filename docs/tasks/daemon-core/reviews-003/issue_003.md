---
provider: manual
pr:
round: 3
round_created_at: 2026-10-29T04:05:35Z
status: resolved
file: internal/daemon/boot.go
line: 436
severity: low
author: claude-code
provider_ref:
---

# Issue 003: A silent socket client turns a graceful stop into an overrun

## Review Comment

Shutdown step 6 is `http.Server.Shutdown(remaining budget)` (`_spec.md:747`, `internal/daemon/boot.go:436`). `net/http` treats a connection that has not sent its first byte (`StateNew`) as busy until it is about 5 s old, and only then closes it as idle. The daemon's `connTracker` (`internal/daemon/conns.go:32-47`) follows `StateActive` connections only. Step 5's `closeActive` therefore never cuts such a connection, and no other code does. So one local process that has connected to `daemon.sock` and is silent holds step 6 for 5–6 s:

- With the default `shutdown_grace` (30 s), a graceful stop takes about 5.3 s instead of milliseconds. The budget is respected, but most of the latency is the idle client.
- With a valid shorter grace (the range is 1 s–5 m, `_spec.md:146`), the budget runs out at step 6. A healthy daemon then takes the forced path and exits 1 with `shutdown_budget_exceeded` instead of 0. `harness daemon start --foreground` reports a failure, and the log records an overrun that no step caused.

Runtime evidence. This was a throwaway in-process test, added through `go test -overlay`. `startDaemon` runs, a unix-socket client dials and sends nothing, then `Run`'s ctx is cancelled:

```
GOFLAGS="-count=1 -overlay=<scratch>/r3.json" go test -tags 'integration harness_testhooks' \
  -run TestReviewR3_SilentClientAtStop -v ./internal/daemon/
grace 3s:  Run returned=true after 3.012498625s, err=shutdown exceeded its budget of 3s at step 6 (server)
grace 30s: Run returned=true after 5.263850583s, err=<nil>
```

The second-signal rule still holds during step 6: `runShutdownStep` cancels the ctx on `d.abort` and the stop ends in about 12 ms with exit 1 (control subtest of `reviews-003/issue_002.md`). The boot undo is the exception, see issue_002.

The socket is 0600 in a 0700 directory, so only the operator's own processes can do this, for example a probe or a script that opens the socket and waits. The impact is a stop that is slow, or that fails at short graces, not a safety break.

Suggested fix: at step 6, also close the connections that never started a request. One way is to track `StateNew` in `connTracker` and close those connections once `Shutdown` has begun: no request is in flight on them, so this loses nothing. Another is to call `Shutdown` with a short ctx and fall back to `Close` for whatever is left. If the spec text "`http.Server.Shutdown(remaining budget)`" is meant literally, adjust it to match. Add an integration test: a silent client connected at stop, `shutdown_grace = "3s"`, and a graceful stop (nil error) well under a second expected.

## Triage

- Decision: `VALID`
- Notes:
  - **Valid.** Reproduced on the real tree before the fix by the new `TestIT060_ASilentClientCannotHoldTheShutdown` (in-process daemon, `shutdown_grace = "3s"`, a unix-socket client that sends nothing, then a stop): `Run = shutdown exceeded its budget of 3s at step 6 (server), want a graceful stop (nil)`, for the silent client and for a client that sent only `GET /api/sta`.
  - **Where the 5 s comes from.** Not the daemon's own timeouts (`ReadHeaderTimeout` is 10 s and plays no part). It is `net/http` itself: `Server.closeIdleConns` (`net/http/server.go`, Go 1.27.3, line 3326, net/http issue 22682) treats a `StateNew` connection as idle only once its state is more than 5 s old; until then `Shutdown` counts it as busy and keeps polling. A connection moves from `StateNew` to `StateActive` only after `readRequest` has returned having read at least one byte, so a client that is silent, or that holds the start of a request, stays `StateNew`.
  - **Root cause.** `connTracker` followed `StateActive` connections only, so step 5's `closeActive` never cut a `StateNew` connection and nothing else did: `http.Server.Shutdown` at step 6 waited for it for about 5 s (the default 30 s grace: a stop of 5.3 s instead of milliseconds; a valid shorter grace: an overrun at step 6, exit 1 and `shutdown_budget_exceeded`, for a healthy daemon).
  - **Fix.**
    - `internal/daemon/conns.go`: `connTracker` also tracks `StateNew` connections (`unstarted`, forgotten on `StateActive`, `StateIdle`, `StateClosed` and `StateHijacked`) and gets `closeUnstarted()`, which sets `refusing`, closes every unstarted connection and returns how many; a connection accepted after that is closed as soon as `track` sees it, so one accepted between this call and the listener closing cannot hold `Shutdown` either. The active set that `waitIdle` and `closeActive` use is untouched.
    - `internal/daemon/boot.go`: `shutdownServer` calls `d.conns.closeUnstarted()` before `http.Server.Shutdown(ctx)` (debug log `connections without a request closed`). It is the action of shutdown step 6 and of the undo's `close server`, so the boot undo is covered too. Step 5, which waits for the event streams' `shutdown` frame and then cuts the active connections, runs before and is unchanged: a stream is a request in flight, never one of the unstarted connections.
    - Trade-off: a connection that was accepted but whose first request bytes the server has not read yet when step 6 begins is closed instead of served, a window of microseconds. The listener closes in the same step, so a request one moment later gets connection refused anyway, and the CLI's `stop` and `status` treat an unreachable daemon as such.
  - **Tests.**
    - `TestIT060_ASilentClientCannotHoldTheShutdown` (`integration`, subtests `connected and silent` and `start of a request line`): graceful stop (nil) under 2 s with `shutdown_grace=3s`, the silent connection closed, files and lock gone. `_tests.md`: IT-060 and a matrix row.
    - `TestConnTracker_ClosesTheConnectionsThatHaveNotStartedARequest` and `TestConnTracker_ForgetsAConnectionOnceItHasStartedOrClosed` (UT-139, `internal/daemon/conns_test.go`).
    - Mutations (`go test -overlay`, tree untouched): without the `closeUnstarted` call in `shutdownServer` both IT-060 subtests fail with the `step 6 (server)` overrun above; without the `refusing` check in `track`, UT-139 fails (`a connection accepted after the shutdown began was not closed`).
  - **Spec amendment (needs operator confirmation).** `_spec.md` › Boot and shutdown sequences › Shutdown, step 6: "`http.Server.Shutdown(remaining budget)`. Its `RegisterOnShutdown` hook ..." now reads "Close the connections that have not sent a request (`http.Server.Shutdown` counts them as busy until they are 5 s old, so a silent client would hold the budget), then `http.Server.Shutdown(remaining budget)`. Its `RegisterOnShutdown` hook ...". The rest of the step is unchanged.
  - Verified: (review round 4, 2026-10-29) the fix matches the issue. `connTracker` (`internal/daemon/conns.go:49`) tracks `StateNew` connections apart from the active set. `waitIdle` and `closeActive` only read that active set, so step 5 and the SSE `shutdown`-frame ordering are unchanged: an event stream is a request in flight and is never "unstarted". `closeUnstarted` runs inside `shutdownServer`, after step 5 and before `http.Server.Shutdown`. A `status` (or `stop`) whose connection is cut in that window gets `daemon_unavailable` ("connection to the daemon … failed"; `internal/cli/client.go:181`), which is the same code and exit (69) as the refused dial that follows a moment later. `stop` treats it as unreachable and waits on the lock (`stopUnreachable`), so the outcome is sane. The trade-off is acceptable: the window is microseconds, the listener closes in the same step, and nothing was in flight on those connections. Mutations through `go test -overlay`: dropping the `closeUnstarted` call killed both IT-060 subtests (`shutdown exceeded its budget of 3s at step 6 (server)`), and dropping the `refusing` check killed UT-139 (`a connection accepted after the shutdown began was not closed`). The amendment is right: it describes how the step meets its budget, and the old text never promised to serve a silent client.
