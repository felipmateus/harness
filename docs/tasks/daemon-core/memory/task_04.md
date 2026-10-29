# Task 4 memory

## Objective Snapshot

`internal/admission` and `internal/api`, completed 2026-10-28:
- `admission.Lifecycle` (the concrete `Gate`) with forward-only transitions and in-flight tracking;
- `api.Server`, an `http.Handler` with the five routes of `_dx.md` › HTTP API, strict requests, the single error shape, the gate exemptions and the SSE adapter;
- tests for UT-050–UT-057, UT-065, UT-067–UT-069, UT-112–UT-117, plus supplementary cases;
- message constructors added to `api/contract` (see Decisions).

## Important Decisions

- **`admission.Gate` is the spec's interface; the concrete type is `admission.Lifecycle`** (`admission.New()` returns it, starting). Same pattern as `broker.Broker`/`Hub`. `Set` ignores a backward, repeated or unknown transition (the interface has no error). `Enter` checks the state and counts under one lock, so once `Set(Draining)` returns the in-flight count only goes down. `release` is idempotent. `Wait` returns nil when idle, even with a cancelled ctx.
- **Route table, not per-handler checks.** `api/routes.go` holds one `route{pattern, method, exempt, stream, query, handle}` per path. Chain per route: recovery (outermost, global) → canonical-path check → `ServeMux` → admission + in-flight (`admit`) → debug log → method and query check (`strict`) → handler. The in-flight tracking is the deferred `release` of `Gate.Enter` in `admit`; a stream route releases at once, an exempt route never calls the gate.
- **404 never depends on the gate.** Unknown paths and non-canonical paths answer JSON 404 in every state; a known path is gated first (so `POST /api/doctor` while draining is 503, not 405).
- **Path-clean redirects are avoided up front.** `canonical` compares `path.Clean(r.URL.EscapedPath())` to the path (the string `ServeMux` cleans) and answers the JSON 404 instead of letting the mux redirect. Go 1.27's mux redirects with 307, not 301. The same check covers `*` and the empty path of a CONNECT target.
- **`api.Options`** carries: `Gate`, `Status`, `Events`, `Doctor`, `Follow` (`FollowFunc`), `RequestStop`, `StreamContext`, `Logger` (`*slog.Logger`), `KeepaliveInterval`, `WriteTimeout`. `New` returns an error for a missing `Gate`, `Status`, `Events`, `Follow` or `RequestStop`; `Doctor` may be nil (the route then answers 500 and logs) until task 8 plugs it in. The interface types are unexported, as the spec names them, and still assignable from other packages (checked with a throwaway package that wired `*ledger.Ledger`, `*broker.Hub` and `*admission.Lifecycle`).
- **`statusSource` has two methods:** `Status(ctx) (contract.Status, error)` (everything except `State`, which the handler stamps from the gate) and `ShutdownInfo() contract.ShutdownInfo` (PID, uptime, grace; cheap, no ledger, because the shutdown route must answer while starting or draining). The handler sets `Stopping=true`.
- **One stop callback, `RequestStop`, called once.** While starting it cancels the boot, while running it starts the shutdown (the daemon decides which), while draining it is not called. `sync.Once` covers the window before the daemon flips the gate. It runs *before* the 202 is written and must only signal.
- **`doctorRunner.Run(ctx) (contract.Doctor, error)`.** The handler guarantees `items` is `[]`, not null.
- **SSE adapter.** Follows with the request ctx merged with `Options.StreamContext` (`context.AfterFunc`), cancelled when the handler returns. Flushes the headers (200, `text/event-stream`, `no-cache`) before the first event. Every write, headers flush included, is `SetWriteDeadline(now+timeout)`, write, `Flush`; 10 s (`DefaultWriteTimeout`) for events and keepalives, 1 s for the final frame. **A writer that cannot take a deadline ends the stream** (fail closed, logged), so Safety Invariant 8 cannot be skipped silently.
- **A closed follow channel with a live ctx is `ErrClosed`** (task 3 handoff), written as the `shutdown` frame. A closed channel with an ended ctx returns silently.
- **Log records written by the adapter:** `stream write failed` (info) and `stream error` (error), both with `stream`, `cursor` (the last sequence written, or `after`), `err`. The `request` debug record is written when a handler returns, so for a stream it appears at the end; a request refused by the gate is not logged (the log layer sits inside admission, as the spec orders it).
- **Strictness beyond the spec table, all `invalid_request`:** a known query parameter given twice (`parameter "after" given more than once`), a malformed query string (`invalid query string: …`), an empty reason (`reason must be "request"`), trailing data after the body (`invalid request body: only one JSON object is allowed`), a body over 4 KiB, a wrong-typed field. Unknown parameters are reported in sorted order. `after` is validated even when `Last-Event-ID` overrides it. An empty `Last-Event-ID` header counts as absent; `?after=` and `?limit=` empty mean the defaults (the contract's `ParseAfter("")`/`ParseLimit("")`).
- **Messages live in `api/contract/messages.go`** (AGENTS.md rule): `NotReadyError`, `DrainingError`, `NotFoundError`, `MethodNotAllowedError`, `UnknownParameterError`, `DuplicateParameterError`, `UnknownFieldError`, `InvalidBodyError`, `InvalidQueryError`, `InternalError`, `InternalMessage`, `ValidateShutdownReason`. Task 6 can reuse them.
- **Panic handling.** The 500 body is the generic one. The log gets `panic` and `stack`, both redacted. If the response had already started, the handler panics with `http.ErrAbortHandler` so the client sees a truncated response, not a complete one. An `ErrAbortHandler` raised by a handler is passed through, not logged.
- **`Allow` header** is set on every 405.

## Learnings

- **`http.Server` answers `OPTIONS *` itself** (200, empty) unless `DisableGeneralOptionsHandler` is set. The handler already answers `*` with the JSON 404 if it is ever reached; task 5 should set the option so every request target gets the JSON shape.
- **`httptest.ResponseRecorder` has no `SetWriteDeadline`**, so SSE tests use a real `httptest.Server`, or a recording `ResponseWriter` for the deadline values.
- **A stalled-client test needs small socket buffers**, or a megabyte event fits in the kernel. `smallBufListener` sets a 4 KiB send buffer on accepted conns; the raw client sets a 4 KiB receive buffer. A 512 KiB event then blocks the write until the 50 ms deadline.
- **The spec's wall-clock tests (UT-065, UT-112, UT-113) flake under heavy parallel load** (eight race-enabled copies on eight cores: a 200 ms bound became 226–376 ms, a 30 ms item gap lost to a 50 ms timer). They keep the spec's numbers and run as `eventually` scenarios, up to three attempts: a defect fails every attempt, jitter does not. After that, 14 parallel copies × 6 runs × `-cpu 1,4` were green.
- **A test that waits on a handler that may never return must not hang the suite:** the no-deadline test runs the handler on a goroutine, and the test client has a 5 s response-header timeout.
- `go test` serves `internal/archtest` from cache when a package changes; use `-count=1`.

## Files / Surfaces

- New: `internal/admission/{gate.go,gate_test.go}`.
- New: `internal/api/{server.go,routes.go,middleware.go,errors.go,status.go,doctor.go,shutdown.go,events.go,sse.go}` and tests `{api_test.go,middleware_test.go,sse_test.go,helpers_test.go}`.
- New: `internal/api/contract/{messages.go,messages_test.go}`.
- `docs/roadmap.md`: feature 1 now shows "tasks 1–4 done".
- No new module dependency. `api` imports `contract`, `stream`, `ledger`, `admission`, `redact` only; `admission` imports no internal package (checked with `go list` and the architecture test).

## Errors / Corrections

- The first mutation run hung for 300 s on "no write deadline": the no-deadline test called `ServeHTTP` synchronously, and a handler with no deadline never returns. Rewritten to run the handler on a goroutine and fail after 1 s. The "headers not flushed" mutation then timed out the whole suite; the test client got a 5 s response-header timeout.
- The first UT-112 payload (1 MiB) pushed the marshalling cost into the measured 200 ms under load; 512 KiB still stalls the 4 KiB socket buffers.

## Ready for Next Run

**Verification evidence (macOS, Go 1.27.3):**
- `go test -race -count=1 ./internal/admission/ ./internal/api/...` green (73 tests); `-count=10` green; `-count=5 -cpu 1,2,8` green.
- `go test -race -count=1 ./internal/archtest/` green. Temporary imports of `broker` and of `home` in `internal/api` each made it fail, then were removed.
- 21 mutations applied one at a time and reverted, each killed by at least one test: no canonical check, no `Unwrap`, no keepalive reset, no write deadline, final frame on the long deadline, stream counted as in flight, stop not once, no `Last-Event-ID` override, unknown parameters allowed, shutdown/status not exempt, shutdown triggers while draining, no recovery, no error log, closed channel silent, stream ctx ignored, no redaction, wrong `next_after`, head read before the page, headers not flushed, duplicate parameters allowed.
- `make gate` green (gofmt, `go vet ./...`, `go test -race ./...`, build).
- After the runs, `~/.harness` did not exist and no `/tmp/hz*` directory was left.

**For task 5 (daemon):**
- `gate := admission.New()`; `gate.Set(admission.Running)` at boot step 12 and `gate.Set(admission.Draining)` at shutdown step 1 (`starting → draining` is allowed for a cancelled boot). Call `gate.Wait(ctx)` at step 3.
- `srv, err := api.New(api.Options{Gate: gate, Status: …, Events: ledger, Doctor: …, Follow: func(ctx, s, after) { return stream.Follow(ctx, ledger, hub, s, after) }, RequestStop: …, StreamContext: streamCtx, Logger: logger.Logger, KeepaliveInterval/WriteTimeout from `Hooks` (zero keeps 15 s / 10 s)})`; `http.Server{Handler: srv, DisableGeneralOptionsHandler: true}`. Do not set a `WriteTimeout` on the server: the adapter sets its own deadlines.
- `streamCtx, cancel := context.WithCancel(...)`; `httpSrv.RegisterOnShutdown(cancel)` (backstop to the broker close).
- `RequestStop` must only signal (cancel a ctx, send on a channel). It is called at most once, while the gate is not draining.
- `Status` returns a `contract.Status` with `State` left empty; `ShutdownInfo` returns `{PID, UptimeSeconds, ShutdownGrace}` without touching the ledger. `ShutdownGrace` is the config text (`"30s"`).
- `Doctor` may stay nil until task 8; the route then answers 500 `internal`. Pass a real runner once `internal/doctor` exists.
- The `subscriber degraded` record still has no producer: the adapter cannot see degradation (`Follow` hides it). A daemon-side wrapper around the `broker.Subscription` that logs when `Degraded()` returns true would work but cannot supply `cursor`. Decide in task 5 or drop the record from the spec.
- IT-005 (gate states over the socket), IT-030–IT-034 (follow over the socket) are the integration checks for this task's behaviour.

**For task 6 (CLI):** reuse the `contract` message constructors; the SSE frames are `id:`, `event:`, `data:` (one line, up to about 1 MiB), `: keepalive`, and the two final frames `event: shutdown` and `event: error`.
