# Spec: daemon-core

| | |
|---|---|
| Slug | `daemon-core` |
| Roadmap | Feature 1 of the Harness MVP epic ([roadmap](../../roadmap.md)) |
| Status | **Approved** by the user on 2026-10-28. Revised the same day with every finding of [review-001](review-001.md), per the user's decision (F1 option A: `synchronous=FULL` + `fullfsync` on macOS). Amended during code review (rounds 1–3, 2026-10-29) in six places, all confirmed by the user on 2026-10-29: Business Rule 12 (a cancelled boot that appended records its stop), the `subscriber degraded` Monitoring row (`seq`), SIGPIPE held by the foreground daemon process until exit, the lock released only after the ledger's close on an overrun, the second signal during a boot's undo, and shutdown step 6 closing connections that have not sent a request. Each amendment is argued in its issue file under `reviews-001/` to `reviews-003/`. |
| Date | 2026-10-28 |
| Decisions recorded | Name `harness`, home `~/.harness`, `HARNESS_HOME`; module `github.com/felipmateus/harness`; macOS + Linux; visible outcome = the daemon's own event stream (user answers, 2026-10-28). Go; local-socket transport only (ADR-002); a single ledger file (ADR-001); ports only at variation edges (ADR-003); full durability (review-001 F1-A). |
| Companions | [_user_stories.md](_user_stories.md) · [_dx.md](_dx.md) · [_tests.md](_tests.md) · [ADR-001](adrs/adr-001.md) · [ADR-002](adrs/adr-002.md) · [ADR-003](adrs/adr-003.md) · [review-001](review-001.md) |

# Part I — Product

## Overview

**Motivating Problem.** Every later harness feature needs a process that outlives the terminal, plus a place where what happened is recorded durably and in order:
- sessions that run for hours;
- loops that must resume after a crash;
- followers that must not miss events.

Today nothing exists. The simplest end-to-end behaviour that proves the foundation, observable from outside:

1. `harness daemon start` returns once a background daemon is ready.
2. `harness events --stream daemon --follow` shows the daemon's own lifecycle events live: `config.loaded`, `daemon.started`, and on stop `daemon.stopping` and `daemon.stopped`.
3. After a stop, restart, crash or power loss, the same stream continues with no gaps, duplicates or lost history.

Slice 1 delivers this.

**Who it is for.**
- The operator: the developer running harness locally.
- Agents that drive harness through its CLI.
- The authors of the next features, `acp-sessions` and `loop-engine`, who will write their events to the same ledger.

**Why it is valuable.**
- It is the runtime boundary that the whole epic, and the full spec-cycle flow (Milestone A), sits on.
- It applies the reverse spec's most load-bearing lessons from the start: persist before publish, a single writer, gap-free replay, a singleton lock, graded shutdown (OBS-STO-003…007, OBS-DMN-001…008).

**Terms.**

| Term | Meaning |
|---|---|
| Home | The directory holding all daemon state: `$HARNESS_HOME`, else `~/.harness`. |
| Ledger | The durable, append-only event store inside the home. |
| Stream | A named, independently ordered sequence of events in the ledger (this feature uses `daemon`). |
| Sequence (`seq`) | The 1-based position of an event within its stream. |
| Head | A stream's highest sequence; 0 when empty. |
| Follower | A client that reads a stream's history and then receives new events live. |
| Release the home | The daemon has exited its lock (the lock is free) and removed its socket. |

## Goals

- The operator can start, stop, inspect and diagnose a background daemon with one command each. The daemon survives the terminal that started it.
- At most one daemon owns a home. A crash never requires manual cleanup, and the next start says the previous run ended uncleanly.
- Every event the daemon records is durable on stable storage — surviving process crash, OS crash and power loss — before anyone can see it.
- Within a stream, events are numbered 1, 2, 3… with no gaps.
- Any follower can resume from any sequence without missing or repeating events.
- Agents can operate everything through `-o json|jsonl`, a single error shape and documented exit codes.
- The daemon is reachable only by the owning user and writes no secrets to its logs.

## User Stories

- Lifecycle — US-001…US-005: start, foreground, single instance, graceful stop, crash recovery.
- Inspection — US-006…US-007: status, doctor (also without a daemon).
- Event stream — US-008…US-010: history, live follow without gaps, durable ordered ledger.
- Configuration — US-011: strict, optional `config.toml`.
- Automation — US-012: structured output and exit codes.
- Security — US-013: private home and socket, file modes, log redaction.

[Full user stories](_user_stories.md)

## Core Features

1. **Daemon lifecycle.**
   - `daemon start` (background by default, idempotent, waits out a stopping daemon) and `daemon start --foreground`.
   - `daemon stop` (graceful, idempotent, cancels a daemon that is still starting).
   - A single-instance lock per home.
   - Crash detection and cleanup of leftovers.
2. **Event ledger and streams.**
   - Durable, ordered, idempotent, validated append.
   - Paged reads with a byte budget.
   - Live follow with resume by sequence.
   - In this feature only the `daemon` stream has a producer, the daemon itself. Later features add streams.
3. **Inspection.**
   - `status`: state, PID, version, uptime, home, ledger and config summary.
   - `doctor`: permissions, config, ledger integrity and schema, log writability. It reports problems and never fixes them. It runs inside the daemon when one answers, and locally otherwise.
4. **Configuration.** An optional `config.toml` with strict keys. `config path|validate|show` work without a daemon.
5. **Local API.** A request/response API on a user-private local socket, with a single error shape and a live event stream for follow. This is the contract the CLI and agents use (wire details in `_dx.md`).
6. **Operational hygiene.** Structured JSON logs with rotation and secret redaction, restrictive file modes and a private home.

**Interactions.**
- The lifecycle produces the `daemon` stream's events.
- Inspection reads ledger and config state.
- The CLI is a thin client of the local API. The exceptions are `config` and `version`, which run standalone, and `doctor`, which falls back to local checks.

## Business Rules

**Single instance**

1. At most one daemon process owns a home, and ownership is the OS file lock on `<home>/daemon.lock`.
2. Only the lock owner may create or remove `<home>/daemon.sock` or `<home>/daemon.json`, or write to the ledger.
3. A leftover socket or `daemon.json` without a lock owner is stale and is removed by the next owner. A PID recorded in a file never decides ownership.
4. `daemon.lock` itself is never deleted, renamed or replaced. A process that does not hold the lock never modifies it.

**Lifecycle states**

5. The daemon moves through `starting → running → draining → exited`. No other transition exists. A shutdown request while `starting` moves it to `draining`.
6. While `starting` or `draining`, every API request except `GET /api/status` and `POST /api/daemon/shutdown` is refused with `daemon_not_ready` or `daemon_draining`.
7. `harness daemon start` is idempotent. If a daemon for the home answers `running`, it reports it and exits 0. If a daemon is draining, `start` waits until it has released the home and then starts a new one. It never reports `already running` for a daemon that is going away.
8. `harness daemon stop` is idempotent:
   - with no daemon, it reports `not_running` and exits 0;
   - with a daemon already draining, it waits for that same exit;
   - with a daemon still starting, it cancels the start.

   In every case it returns only once the home is released, or with `daemon_stop_timeout`.

**The `daemon` stream**

9. On every start the daemon records `config.loaded`, then `daemon.started`.
10. On a graceful stop it records `daemon.stopping`, then `daemon.stopped`. The reason is `request` (API), `signal` (SIGINT, SIGTERM or SIGHUP) or `context` (in-process cancellation, used by tests and embedding).
11. Every `daemon`-stream event carries the PID of the daemon that recorded it.
12. A boot that fails after the ledger is open records `daemon.boot_failed`. A boot that is cancelled after it has appended an event (`daemon.unclean_shutdown_detected` at step 7, or `config.loaded` at step 9) records `daemon.stopping` and `daemon.stopped`, so the event it appended is not left as the last one of the stream.
13. If, at start, the last event of the `daemon` stream is neither `daemon.stopped` nor `daemon.boot_failed`, the daemon records `daemon.unclean_shutdown_detected` before `config.loaded`. The event carries the previous PID (from that last event), the last sequence and the last type.

**Ledger invariants**

14. An event is never visible to any reader or follower before it is committed and synced to stable storage. It survives process crash, OS crash and power loss.
15. Within a stream, sequences start at 1 and increase by exactly 1 per event.
16. History is append-only. No event is updated, deleted or renumbered by this feature.
17. Appending an event whose ID already exists with identical stream, type and data creates nothing and returns the existing event. The same ID with different content is rejected as a conflict.
18. An event is accepted only if its type matches `^[a-z][a-z0-9_.:-]{0,127}$`, its data is valid JSON (absent data is stored as `{}`), and its data is at most 1 MiB. A rejected append does not move the head.
19. A follower that resumes after sequence N receives exactly the events with `seq > N`, in order, each once, including events committed while it was disconnected or falling behind.
20. Event times never decrease along a ledger's commit order.

**Validation**

21. Stream IDs match `^[a-z][A-Za-z0-9_.:-]{0,127}$`. Uppercase is allowed after the first character, so IDs such as ULIDs can be embedded.
22. `after` is an integer ≥ 0. `limit` is an integer from 1 to 1000, default 100.
23. Values outside these ranges, unknown query parameters and unknown body fields are rejected, never clamped or ignored. The CLI applies the same validation before contacting the daemon, with identical messages.
24. Config keys are strict: an unknown section, an unknown key, a wrong type or an out-of-range value makes the file invalid.
25. A missing config file is valid (defaults). An invalid file prevents the daemon from starting.

**Limits and defaults**

| Setting | Value |
|---|---|
| `daemon.shutdown_grace` | `30s`, range 1s–5m |
| `log.level` | `info` |
| Readiness wait for `daemon start` | `10s` (`--timeout`), then SIGTERM, then SIGKILL after 2 s |
| `daemon stop` wait | the daemon's `shutdown_grace` + 5 s |
| Page size | `limit` events and at most 8 MiB of event data |
| Follow keepalive | after 15 s without any frame |
| Follow client idle limit | 45 s without any byte |
| Log rotation | 10 MB, 5 backups, 30 days |
| Socket path | at most 103 bytes (macOS) or 107 bytes (Linux), cleaned absolute path |

**Home and permissions**

26. The home is accepted only if it is a real directory (not a symlink), owned by the current user, and not writable by group or others. Otherwise `start` refuses with `home_insecure` before anything is spawned or created inside it.
27. Home and `logs/` are 0700. Socket, `daemon.json`, `daemon.lock`, `harness.db` (with `-wal` and `-shm`) and log files are 0600, independent of the user's umask.
28. The daemon never changes the permissions of a pre-existing home. A home readable by others is accepted, and `doctor` reports it.
29. The daemon opens no TCP port.

## User Experience

**Personas.** Operator, Agent and Feature author ([user stories](_user_stories.md#personas)).

**Operator flow:**
1. Install the binary.
2. `harness daemon start` — prints PID and socket.
3. `harness status` — confirms it is running.
4. `harness events --follow` in a second terminal.
5. Work.
6. `harness daemon stop`.

**Recovery flow:**
1. A crash, power loss or reboot happens.
2. `harness daemon start` just works.
3. `harness events` shows `daemon.unclean_shutdown_detected`.

**Diagnosis flow:**
1. Something is off, possibly the daemon will not even start.
2. `harness doctor` lists each check with a fix hint, running locally when no daemon answers.
3. `harness config validate` lists every config problem.

**Agent flow.**
- Every command runs with `-o json` (or `-o jsonl` for streams).
- Agents branch on exit code and `error.code`.
- Reports (`doctor`, `config validate`) are documents on stdout, even with a non-zero exit.
- The `_dx.md` Errors table is the contract.

**Discoverability.**
- `harness --help` and per-verb help.
- Every error that has an obvious next step prints a `hint:` line (or a `hint` field).

There is no UI, so accessibility requirements do not apply.

## High-Level Technical Constraints

- **Platforms:** macOS and Linux (arm64 and amd64). Windows is out of scope (user decision, 2026-10-28).
- **Distribution:** a single binary, `harness`, written in Go (module `github.com/felipmateus/harness`). It is fully static on Linux (`CGO_ENABLED=0`); on macOS it links only the system library, as every Go binary does there.
- **Locality:** all state lives under the home. There are no network listeners and no outbound calls.
- **Durability:** an acknowledged event survives power loss, at the cost of one storage flush per commit.
- **Agent manageability:** every capability is reachable through CLI verbs with structured output and through the local API. Nothing is human-only.
- **Extensions:** no extension points in this feature (the roadmap defers extensions).
- **Performance**, as the user experiences it:
  - `daemon start` returns within 1 s on a warm machine;
  - `status` within 100 ms;
  - a persisted event reaches attached followers within 100 ms.

## Non-Goals (Out of Scope)

- **Agent sessions, ACP, providers, agent definitions.** Feature `acp-sessions`.
- **Loops, tasks, scheduling, automation.** Features `loop-engine` and later.
- **TCP/HTTP listener, web UI, remote access, auth tokens.** Outside the epic (ADR-002).
- **Windows support.** User decision.
- **Config hot reload.** Config applies on the next start.
- **Ledger retention, compaction or deletion.** No producer in this feature generates volume. Revisit with `acp-sessions`.
- **Auto-start at login (launchd/systemd units).** The operator starts the daemon explicitly. Can follow as its own feature.
- **Writing arbitrary events through the API.** Only the daemon writes in this feature. Producers arrive with their features through the same internal append contract.
- **Self-repair by `doctor`.** It reports and suggests, never changes files.

## Open Questions

None block this spec. Two dependencies are tracked under *Technical Dependencies*:
- installing the Go toolchain;
- the session-stream naming that `acp-sessions` will confirm (rule 21 already allows ULIDs).

---

# Part II — Technical

## Executive Summary

`harness` is one Go binary that is both the CLI and the daemon.
- `harness daemon start --foreground` runs the daemon in-process.
- The default `daemon start` checks the home locally, re-executes the same binary detached (`setsid`, hidden `--detached` flag) and polls `GET /api/status` until the reported PID matches and the state is `running`.
- A daemon that fails or times out is identified precisely: the child writes its final error as one JSON line, which the parent re-raises.

The daemon is built in one composition root (`internal/daemon`). `Run` first sets the umask to 077 and installs its signal handlers, then performs twelve ordered boot steps, each registering an undo action:
1. resolve the home and check the socket-path length;
2. check home security and create the layout;
3. take the lock;
4. load config;
5. start the logger;
6. open the ledger (schema-ahead check, migrations, writer);
7. run the unclean-shutdown check;
8. remove stale files;
9. record `config.loaded`;
10. listen on the socket;
11. write `daemon.json`;
12. record `daemon.started` and become `running`.

Cancellation is checked between steps. The API is served with `net/http` over `<home>/daemon.sock` (ADR-002).

The ledger is one SQLite file, `harness.db`, written by **one writer goroutine** (ADR-001):
- `synchronous=FULL`, plus `fullfsync` on macOS, so an acknowledged event survives power loss.
- Each append runs in a `BEGIN IMMEDIATE` transaction that validates the event and assigns `seq = MAX(seq)+1` for the stream.
- Only after commit does the writer hand the event to an in-memory **broker** that fans it out to followers.

Following a stream is a transport-agnostic function, `internal/stream.Follow`:
- It **subscribes first, then catches up from the ledger, then deduplicates by sequence**.
- A follower that falls behind is marked degraded and re-caught-up from the ledger.
- Every send also watches the broker's `Done` channel and the caller's context, so shutdown never waits on a stuck consumer.
- The SSE handler only frames its output, with write deadlines. `loop-engine` will reuse the same function (ADR-003).

## MVP Boundary

All three planned slices are the MVP of this feature, and nothing in this spec is post-MVP.

- **Slice 1** solves the Motivating Problem end to end:
  - daemon start (foreground and detached) and stop;
  - status, events list and follow;
  - the `daemon` stream's lifecycle events;
  - the final schema and durability settings (they become immutable at the first release).
- **Slice 2** adds the resilience guarantees:
  - home security and the lock-file rules;
  - crash recovery and the unclean-shutdown and boot-failed events;
  - start under races and while draining, idempotent stop, boot cancellation;
  - shutdown budget and stream cancellation;
  - degraded-follower catch-up, keepalive, `Last-Event-ID`;
  - append validation and page budgets.
- **Slice 3** adds operator tooling:
  - doctor (daemon and local);
  - `config` verbs and `version`;
  - log redaction and rotation;
  - the full exit-code and error table, usage errors in JSON;
  - the architecture test.

The exact task cut belongs to `cy-create-tasks`. Out of scope: everything in *Non-Goals*.

## Developer Experience

- [Developer experience contract](_dx.md):
  - **CLI:** `daemon start|stop`, `status`, `events`, `doctor`, `config path|validate|show`, `version`; output rules; exit codes.
  - **API:** HTTP over UDS, five routes, strict requests, SSE framing (event, keepalive, shutdown and error frames).
  - **Config:** `config.toml`, two keys.
  - **Errors:** the full error/code/exit table.
- No UI, so no `_uiux.md`.

## System Architecture

| Component | Responsibility | Boundary |
|---|---|---|
| `cmd/harness` | `main`: builds the root command and exits with its code | Imports only `internal/cli` |
| `internal/cli` | Cobra commands; output rendering and output rules; client-side validation; UDS HTTP client with timeouts; pre-spawn home checks; detached spawn, child-error re-raise, readiness polling and kill escalation; stop with lock-free probing; local doctor fallback; exit-code mapping | Talks to the daemon only through the API contract. Calls `daemon.Run` only for `start --foreground`. |
| `internal/daemon` | **Composition root**: umask and signals, boot steps with undo stack and cancellation, shutdown sequence, wiring, test hooks | The only package that wires `ledger`, `broker`, `api`, `config`, `logging`, `admission`, `doctor`, `home` together |
| `internal/home` | Resolve the home; cleaned absolute paths; socket-path check; home security check; layout creation with modes; lock acquire (owner) and lock-free probe (anyone); atomic `daemon.json` | Leaf |
| `internal/config` | Defaults, strict TOML decoding, validation that collects all errors, digest | Leaf. Used by the daemon, the standalone CLI verbs and doctor |
| `internal/ledger` | Read-only schema-ahead check; SQLite open with pragmas; embedded goose migrations; single writer goroutine with validation, idempotency and time assignment; reader pool with page byte budget; `Append`/`Read`/`Head`/`Last`/`Stats`/`Inspect`; commit callback | Leaf |
| `internal/broker` | Per-stream subscriber sets; non-blocking publish in commit order; degraded signalling; `Done`; close-all and safe late subscribe | Imports only `ledger` (types) |
| `internal/stream` | `Follow`: the transport-agnostic follow algorithm. A plain function, not a port (ADR-003) | Imports only `ledger` and `broker` (types); declares the interfaces it consumes |
| `internal/admission` | Gate states `starting`/`running`/`draining`; in-flight tracking for non-stream requests | Leaf |
| `internal/api` | Path-level routing with JSON 404/405; strict query and body parsing; single error writer; admission middleware (status and shutdown exempt); SSE adapter (framing, write deadlines, keepalive timer, shutdown cancellation) around a follow function | Declares its own consumer interfaces; never imports `daemon`, `cli`, `config`, `doctor`, `broker` or `home` |
| `internal/api/contract` | Request/response DTOs, error codes, shared validation functions and messages (used by API and CLI) | Leaf |
| `internal/doctor` | Probe registry, the seven probes, `daemon` and `local` modes, 2 s per-probe timeout | Imports `api/contract`, `config`, `home`, `ledger` |
| `internal/logging` | slog JSON handler, rotation (lumberjack), redacting handler, stderr mirror that can be turned off at runtime | Imports only `redact` |
| `internal/redact` | Secret patterns and replacement | Leaf |
| `internal/version` | Build info (`-ldflags -X`), `runtime/debug` Go version | Leaf |

**Data flow of one recorded event:**

```
daemon (producer) → Append (validate, ULID) → writer queue → BEGIN IMMEDIATE / id check / seq=MAX+1 / time / INSERT / COMMIT (FULL sync)
                  → onCommit(event) → broker.Publish (non-blocking, in seq order)
                  → each subscription chan → stream.Follow (catch-up, dedupe by seq) → SSE adapter (frame, flush, deadline, keepalive) → CLI follower
```

**External interactions:** none. Filesystem and SQLite only.

## Architectural Boundaries

**Direct imports.** These rules are checked on direct imports (`go list -json`, field `Imports`):

| Package | May import (`internal/*`) |
|---|---|
| `cmd/harness` | `cli` |
| `cli` | `api/contract`, `home`, `config`, `doctor`, `version`, `redact`, `daemon` (only for `start --foreground`) |
| `daemon` | any `internal/*` except `cli` |
| `api` | `api/contract`, `stream`, `ledger` (types), `admission`, `redact` |
| `stream` | `ledger`, `broker` (types) |
| `broker` | `ledger` (types) |
| `doctor` | `api/contract`, `config`, `home`, `ledger` |
| `logging` | `redact` |
| `ledger`, `home`, `config`, `admission`, `redact`, `version`, `api/contract` | none |

**Transitive rule.** No package other than `cmd/harness` (via `cli`) and `cli` itself depends on `internal/daemon`.

**Consumer-side interfaces.**
- `api` declares the interfaces it needs (`statusSource`, `eventReader`, `doctorRunner`, `gate`, plus a follow function) instead of importing provider-side ones.
- `stream` declares `Reader` and `Subscriber`.

**Ports policy (ADR-003).** No ports in this feature: SQLite, the socket and the broker stay concrete. Real ports are introduced only at the ACP agent runtime (`acp-sessions`) and loop action execution (`loop-engine`).

**Enforcement.** The architecture test `internal/archtest` enforces every row above (UT-090).

## Implementation Design

### Core Interfaces

```go
// internal/ledger
package ledger

// Time marshals as a fixed-width UTC timestamp with milliseconds: 2006-01-02T15:04:05.000Z.
type Time struct{ time.Time }

type Event struct {
	Pos    int64           `json:"-"`      // global commit order (rowid); internal
	Stream string          `json:"stream"`
	Seq    int64           `json:"seq"`
	ID     string          `json:"id"`     // idempotency key; ULID unless the producer supplies one
	Type   string          `json:"type"`
	Time   Time            `json:"time"`   // assigned inside the writer transaction; never decreases
	Data   json.RawMessage `json:"data"`
}

type NewEvent struct {
	Stream string          // ^[a-z][A-Za-z0-9_.:-]{0,127}$
	ID     string          // empty → ULID generated by Append before enqueueing; else ^[A-Za-z0-9_.:-]{1,64}$
	Type   string          // ^[a-z][a-z0-9_.:-]{0,127}$
	Data   json.RawMessage // valid JSON, ≤ 1 MiB; nil → {}
}

var (
	ErrInvalidStream    = errors.New("ledger: invalid stream id")
	ErrInvalidType      = errors.New("ledger: invalid event type")
	ErrInvalidData      = errors.New("ledger: data is not valid JSON")
	ErrPayloadTooLarge  = errors.New("ledger: data exceeds 1 MiB") // internal; not a wire code
	ErrIDConflict       = errors.New("ledger: id exists with different content")
	ErrClosed           = errors.New("ledger: closed")
	ErrSchemaAhead      = errors.New("ledger: schema newer than binary") // wraps {DB, Binary int}
)

// Appender is served by exactly one writer goroutine per ledger.
//   - Validation happens in Append, before enqueueing; invalid events never reach the writer.
//   - ctx bounds only enqueueing (the queue holds 256; a full queue blocks) and waiting for the result.
//     The writer runs every transaction under its own lifetime context, so a cancelled caller
//     can never abort or half-apply a commit.
//   - The returned Event always carries the ID, even with an error, so a retry is idempotent.
//   - On success, the event is committed and synced, and OnCommit has been called.
type Appender interface {
	Append(ctx context.Context, e NewEvent) (ev Event, created bool, err error)
}

type Page struct {
	Events  []Event
	HasMore bool
}

const PageByteBudget = 8 << 20 // sum of len(Data) per page; a page always holds at least one event

type Reader interface {
	// Read returns events with seq > after, ascending, at most limit, within PageByteBudget.
	Read(ctx context.Context, stream string, after int64, limit int) (Page, error)
	Head(ctx context.Context, stream string) (int64, error)       // 0 when empty
	Last(ctx context.Context, stream string) (Event, bool, error) // for unclean-shutdown detection
	Stats(ctx context.Context) (Stats, error)                     // streams, events, schema version
}

type Options struct {
	Path     string
	OnCommit func(Event) // called by the writer goroutine after COMMIT, in seq order; must not block
}

// Open checks the schema version on a read-only connection first and returns ErrSchemaAhead
// without writing anything if the database is newer than the embedded migrations; then it
// opens the writer and reader pools, migrates, and starts the writer.
func Open(ctx context.Context, opts Options) (*Ledger, error)

// Close makes later Appends return ErrClosed, drains the queue, then closes the connections.
func (l *Ledger) Close(ctx context.Context) error

// Inspect opens path read-only and reports integrity (quick_check) and schema version
// without migrating; used by doctor in local mode.
func Inspect(ctx context.Context, path string) (Inspection, error)
```

```go
// internal/broker
package broker

type Subscription interface {
	Events() <-chan ledger.Event // closed when the broker closes or the subscription degrades
	Degraded() bool              // true if closed because the buffer overflowed
	Close()
}

type Broker interface {
	Subscribe(stream string) Subscription // after Close: an already-closed, non-degraded subscription
	Publish(ev ledger.Event)              // non-blocking; full buffer → degraded and removed; no-op after Close
	Done() <-chan struct{}                // closed by Close
	Close()                               // closes every subscription (not degraded); idempotent
}

const SubscriberBuffer = 64
```

```go
// internal/stream — transport-agnostic follow (ADR-003: a plain function, not a port)
package stream

// Consumer-side interfaces, satisfied by *ledger.Ledger and broker.Broker.
type Reader interface {
	Read(ctx context.Context, stream string, after int64, limit int) (ledger.Page, error)
}
type Subscriber interface {
	Subscribe(stream string) broker.Subscription
	Done() <-chan struct{}
}

var ErrClosed = errors.New("stream: closed") // the broker closed (daemon shutting down)

// Item carries one event or, as the final item only, the terminal error
// (ErrClosed, ctx.Err(), or a ledger read error).
type Item struct {
	Event ledger.Event
	Err   error
}

// Follow delivers every event of stream with seq > after, history first and then live,
// each exactly once and in seq order, until ctx is done or the broker closes.
// Every send selects on ctx.Done() and s.Done(), so Follow never blocks forever on a
// consumer that stopped reading. The channel is closed right after the terminal Item
// (or without one if ctx ended first).
func Follow(ctx context.Context, r Reader, s Subscriber, stream string, after int64) <-chan Item

const CatchUpPage = 500
```

```go
// internal/admission
package admission

type State string // "starting" | "running" | "draining"

type Gate interface {
	State() State
	Set(State)                          // forward transitions only (starting→draining allowed)
	Enter() (release func(), err error) // ErrNotReady / ErrDraining; status and shutdown routes bypass it
	Wait(ctx context.Context) error     // waits for in-flight non-stream requests (shutdown)
}
```

```go
// internal/home
package home

type Home struct{ Dir string } // cleaned absolute path

func Resolve(flag string) (Home, error)                 // --home > HARNESS_HOME > ~/.harness
func (h Home) CheckSocketPath() error                    // socket_path_too_long; 103 (darwin) / 107 (linux) bytes
func (h Home) CheckSecurity() error                      // home_insecure: symlink, foreign owner, group/other-writable
func (h Home) EnsureLayout() error                       // home and logs/ at 0700 when missing
func (h Home) AcquireLock() (*Lock, error)               // owner only; open O_CREATE|O_RDWR 0600 (never O_TRUNC),
                                                          // TryLock, then truncate+write PID in place;
                                                          // ErrAlreadyRunning{PID} read without modifying the file
func (h Home) LockFree() (bool, error)                   // probe: LOCK_EX|LOCK_NB on a read-only fd, released at once
func (h Home) WriteInfo(Info) error                      // daemon.json: temp in home, fsync, rename, fsync dir
```

```go
// internal/daemon
package daemon

type Options struct {
	Home     home.Home
	Mode     Mode           // Interactive (mirror logs to stderr) | Detached (mirror until running)
	Stderr   io.Writer
	Version  version.Info
	Hooks    *Hooks         // test-only; nil in production
}

// Hooks lets tests hold or fail boot and shutdown steps and reach internals. In subprocess
// test binaries they are configured from HARNESS_TESTHOOKS, compiled only with the
// `harness_testhooks` build tag.
type Hooks struct {
	AfterBootStep      func(step int) error   // may block, or return an error to simulate failure
	BeforeShutdownStep func(step int)         // may block (e.g. hold the in-flight drain)
	OnRunning          func(TestAccess)       // exposes the Appender, the Gate and the logger
	KeepaliveInterval  time.Duration          // overrides 15 s
	WriteTimeout       time.Duration          // overrides 10 s
}

type ExitError struct {
	Code    int    // process exit code: 69, 78 or 1
	ErrCode string // contract error code, e.g. "config_invalid"
	Message string
	Hint    string
}

// Run sets umask 077, installs SIGINT/SIGTERM/SIGHUP handlers, boots, serves until a signal,
// a shutdown request or ctx cancel, then shuts down. Boot failures return *ExitError.
func Run(ctx context.Context, opts Options) error
```

### Data Models

**SQLite schema v1** (`internal/ledger/migrations/00001_events.sql`, goose, embedded):

```sql
-- +goose Up
CREATE TABLE events (
  pos    INTEGER PRIMARY KEY,                                   -- rowid: global commit order
  stream TEXT    NOT NULL CHECK (length(stream) BETWEEN 1 AND 128),
  seq    INTEGER NOT NULL CHECK (seq > 0),
  id     TEXT    NOT NULL UNIQUE,
  type   TEXT    NOT NULL CHECK (length(type) BETWEEN 1 AND 128),
  time   TEXT    NOT NULL CHECK (length(time) = 24),            -- 2006-01-02T15:04:05.000Z
  data   BLOB    NOT NULL CHECK (length(data) BETWEEN 2 AND 1048576),
  UNIQUE (stream, seq)
);
-- +goose Down
DROP TABLE events;
```

**Schema notes.**
- **Ordinary rowid table, not `WITHOUT ROWID`.** SQLite recommends `WITHOUT ROWID` only for rows well under a twentieth of a page. A measurement with 2,000 rows of 3 KB payloads found 2,000 overflow pages with `WITHOUT ROWID` and 0 with this layout (review-001 F14).
- **Indexes.** `UNIQUE (stream, seq)` serves range reads (`stream = ? AND seq > ? ORDER BY seq LIMIT ?`) and `MAX(seq)` as a single seek. `id UNIQUE` provides idempotency. `pos` gives a global commit order for future cross-stream reads.
- **Time.** The fixed-width text sorts correctly (review-001 F13). The writer assigns `time = max(now, last assigned)`, truncated to milliseconds. On `Open`, the last assigned time is initialised from the row with the highest `pos`.
- **Stats.** `Stats` uses `COUNT(*)` and `COUNT(DISTINCT stream)`, which is acceptable at this volume; counters can be added when sessions bring volume.
- **Typed columns vs JSON.** `data` is an opaque JSON blob, not typed columns or side tables. Payload shapes differ per event type, they are always read whole, and nothing in this feature queries inside them. The only matchable state (stream, sequence, ID, type, time) is in real columns covered by indexes. A future feature that needs to query event fields adds a projection table that it owns.

**Connections.**
- Writer: one connection (`SetMaxOpenConns(1)`) with `_txlock=immediate`.
- Readers: a separate pool of 4 connections with `PRAGMA query_only=1`.
- Pragmas on both: `journal_mode=WAL`, **`synchronous=FULL`**, `busy_timeout=5000`, `foreign_keys=ON`. On darwin also **`fullfsync=ON`** and **`checkpoint_fullfsync=ON`**, because a plain `fsync` there does not reach stable storage.
- The schema-ahead check runs first, on a separate `mode=ro` connection that never writes.

**`daemon` stream event types.** Payload fields appear in this order:

| Type | `data` fields |
|---|---|
| `config.loaded` | `pid int`, `path string`, `exists bool`, `digest string\|null` (`"sha256:<hex>"`) |
| `daemon.started` | `pid int`, `version string`, `boot_ms int` |
| `daemon.stopping` | `pid int`, `reason "request"\|"signal"\|"context"` |
| `daemon.stopped` | `pid int`, `uptime_seconds int` |
| `daemon.boot_failed` | `pid int`, `step int`, `code string` |
| `daemon.unclean_shutdown_detected` | `pid int`, `previous_pid int\|null`, `last_seq int`, `last_type string` |

**Files in the home:**

| Path | Mode | Content |
|---|---|---|
| `<home>/` | 0700 | — |
| `config.toml` | operator-owned | optional |
| `daemon.lock` | 0600 | flock target. Never deleted, renamed or replaced. The owner writes its PID in place after locking; for diagnostics only |
| `daemon.sock` | 0600 | UDS; removed only at shutdown step 8 |
| `daemon.json` | 0600 | `{"pid":48211,"version":"0.1.0-dev","socket":"…/daemon.sock","started_at":"…"}`, written atomically |
| `harness.db`, `-wal`, `-shm` | 0600 | ledger (umask 077 set before creation) |
| `logs/` | 0700 | — |
| `logs/harness.log` (+ rotations) | 0600 | slog JSON |
| `logs/daemon.out` | 0600 | Opened `O_APPEND` by the CLI, never truncated. Receives the detached daemon's boot output, its final error line and any crash output. Grows only by boot output; not rotated |

**Config model:**

```go
type Config struct {
	Daemon struct{ ShutdownGrace Duration `toml:"shutdown_grace"` } `toml:"daemon"` // default 30s, 1s..5m
	Log    struct{ Level string `toml:"level"` }                     `toml:"log"`    // default "info"
}
```

Decoding uses `BurntSushi/toml`. `MetaData.Undecoded()` turns every unknown key into an error, and validation collects all errors with `{key, code, message}`.

**API contract types** (`internal/api/contract`):

| Type | Fields |
|---|---|
| `StatusResponse` | `{Status}` |
| `EventsPage` | `{Events []ledger.Event, Page{After, NextAfter, HasMore, Limit, Head}}` |
| `DoctorResponse` | `{Doctor{Status, Source "daemon"\|"local", Items[]{ID, Severity, Message, Fix?}, Summary}}` |
| `ShutdownRequest` | `{Reason?}` (only `"request"`) |
| `ShutdownResponse` | `{Daemon{Stopping, PID, UptimeSeconds, ShutdownGrace}}` |
| `ErrorBody` | `{Error{Code, Message, Hint?, Details?}}` |

Shared validation lives here too: `ValidateStream`, `ValidateAfter`, `ValidateLimit`, each returning the exact `_dx.md` message.

Field names and JSON shapes are exactly those in `_dx.md`.

### API Endpoints

**Routing.** Each path is registered without a method pattern. The handler dispatches on method and answers any other method with JSON `405 method_not_allowed`, and a catch-all `/` answers JSON `404 not_found`. This keeps `ServeMux`'s plain-text 404/405/301 responses out of the contract.

**Middleware**, in order:
1. Panic recovery → `500 internal`.
2. Admission (`Gate.Enter`). `GET /api/status` and `POST /api/daemon/shutdown` are exempt.
3. In-flight tracking for non-stream routes.
4. Debug request log.

Wrappers implement `Unwrap`, so `http.NewResponseController` reaches `Flush` and `SetWriteDeadline`.

**Strictness.** Every route lists its allowed query parameters. Anything else is `400 invalid_request`. Bodies are decoded with `DisallowUnknownFields` and a single-value check.

| Route | Handler behaviour | Errors |
|---|---|---|
| `GET /api/status` | Gate state, PID, version, start time and uptime, home, socket, `ledger.Stats`, config path/exists/digest | `500 internal` |
| `GET /api/doctor` | `doctor.Run(daemon mode)`: every probe at most 2 s; status = worst severity | gate `503` |
| `POST /api/daemon/shutdown` | Optional body `{"reason":"request"}`. While `starting`, it cancels the boot; while `running`, it starts the shutdown; while `draining`, it starts nothing new. Always `202` with PID, uptime and `shutdown_grace` | `400 invalid_request` (unknown field, bad reason) |
| `GET /api/streams/{stream}/events` | Params `after`, `limit`. Validates with the shared functions; `Read` fills `events` and `has_more`; `Head` fills the page | `400 invalid_stream_id` / `invalid_cursor` / `invalid_limit` / `invalid_request` |
| `GET /api/streams/{stream}/events/stream` | Param `after` only. `Last-Event-ID`, when present, overrides it (a non-numeric or negative value is `invalid_cursor`). SSE adapter over `stream.Follow` | the same `400`s, before the stream starts |

All errors go through `writeError(w, status, code, message, hint, details)`. Bodies are `{"error":{...}}`, and messages are passed through `redact`.

**Follow algorithm** (`internal/stream.Follow`; `cursor` = last delivered seq; transport-agnostic):

1. **Subscribe.** `sub := s.Subscribe(stream)`. Subscribing **before** reading history closes the race where an event commits between the read and the subscribe. A subscription obtained after the broker closed is already closed and not degraded, so step 3 ends the follow.
2. **Catch up.** Loop `r.Read(stream, cursor, 500)`:
   - send each event as an `Item` and set `cursor = seq`;
   - stop when `!HasMore`;
   - before each page, check `ctx.Done()` and `s.Done()`. If either fired, end; `s.Done()` sends `ErrClosed`.
3. **Live.** `select` on:
   - `ev, ok := <-sub.Events()`:
     - if `ok` and `ev.Seq <= cursor`, skip it (duplicate from the overlap);
     - if `ok` and `ev.Seq == cursor+1`, send it and advance;
     - if `ok` and `ev.Seq > cursor+1`, re-run step 2 (defensive; should not occur);
     - if `!ok` and `sub.Degraded()`, resubscribe (step 1) and re-run step 2;
     - if `!ok` otherwise (the broker closed), send `Item{Err: ErrClosed}` and close.
   - `ctx.Done()`: close without a terminal item.
4. **Every send** is `select { case out <- item: case <-ctx.Done(): return; case <-s.Done(): send ErrClosed (non-blocking), return }`.
5. **A ledger read error** is sent as the terminal `Item`.

**SSE adapter** (`internal/api`, per request). The handler's context is the request context merged with a server-wide stream context. That stream context is cancelled by `http.Server.RegisterOnShutdown`, as a backstop to the broker close.

It loops over the follow channel together with a keepalive timer (interval injected; default 15 s, reset after every frame):
- **event item:** frame `id: <seq>\nevent: <type>\ndata: <event json>\n\n`, written under a 10 s write deadline (`ResponseController.SetWriteDeadline`), then flushed. A failed write ends the request.
- **timer:** `: keepalive`, under the same deadline.
- **`ErrClosed`:** `event: shutdown` frame under a 1 s deadline, then return.
- **context ended:** return.
- **any other error:** log it, write `event: error` with `{"error":{"code":"internal","message":"internal error"}}` under a 1 s deadline, then return.

The adapter holds no ordering or dedupe logic of its own. It receives the follow function and the timing parameters as constructor dependencies (`stream.Follow`, 15 s, 10 s in production), so framing is testable in isolation.

**CLI `events` and `--follow`.**
- Validation happens client-side with the shared functions (exit 65) before any connection.
- `--follow` never sends `limit`.
- The SSE reader uses a `bufio.Reader` with no token limit (lines up to 16 MiB). The HTTP client has no overall timeout, and a 45 s idle deadline that each byte, keepalives included, resets.

Endings:

| Ending | Human mode | jsonl | Exit |
|---|---|---|---|
| `event: shutdown` | `stream closed: daemon stopped` on stderr | final `{"closed":{"reason":"daemon stopped"}}` line | 0 |
| `event: error` | `error: internal error` | — | 1 |
| EOF without a final frame, or idle | `daemon_unavailable` | — | 69 |
| SIGINT | — | — | 130 |

Other output rules:
- `-o jsonl` without `--follow` prints the events, then a final `{"page":{…}}` line.
- `--follow -o json` is a usage error (64).

### Boot and shutdown sequences

**Before step 1**, `Run` sets `syscall.Umask(0o077)` and installs handlers for SIGINT, SIGTERM and SIGHUP. Signals map to reason `signal`. A second signal while a stop is under way, in the shutdown or in the undo of a boot that is ending, exits immediately with code 1 (`shutdown_aborted`; a boot that is failing reports its own error). The foreground daemon process also takes SIGPIPE and drops it, from before step 1 until it exits (the CLI's final write of an error report included), so a write to a closed stderr makes the log mirror fail instead of ending the process, and the exit code stands; the mirror switches itself off at its first write error and the file log goes on.

**Boot.** Each step registers an undo action that runs in reverse order if a later step fails or the boot is cancelled. Cancellation (ctx, signal or shutdown request) is checked at every step boundary.

1. Resolve the home (cleaned absolute path). Check the socket-path length → `socket_path_too_long` (78).
2. `CheckSecurity` → `home_insecure` (78). `EnsureLayout`: home and `logs/` at 0700 when missing.
3. `AcquireLock` → `daemon_already_running` (69), naming the holder PID. For an interactive foreground start, retry 3 times over 100 ms first, so a momentary lock-free probe from another CLI is not mistaken for an owner.
4. Load config strictly → `config_invalid` (78).
5. Start the logger: file, plus the stderr mirror per `Mode`.
6. `ledger.Open`:
   - read-only schema check → `ledger_schema_ahead` (69), file untouched;
   - pragmas, migrations, start the writer;
   - wire `OnCommit` → `broker.Publish`.
7. Read `Last("daemon")`. If an event exists and its type is neither `daemon.stopped` nor `daemon.boot_failed`, append `daemon.unclean_shutdown_detected{pid, previous_pid: last.data.pid or null, last_seq, last_type}`.
8. Remove a stale `daemon.sock` and `daemon.json`; safe because the lock is held.
9. Append `config.loaded{pid, path, exists, digest}`.
10. Gate = `starting`. Listen on the socket with `UnixListener.SetUnlinkOnClose(false)` (umask 077 already gives 0600; `chmod 0600` is applied anyway), and serve.
11. Write `daemon.json` atomically.
12. Append `daemon.started{pid, version, boot_ms}`. Gate = `running`. In `Detached` mode, turn the stderr mirror off. Log `daemon running`.

**When the boot fails or is cancelled:**
- **Before step 6** (no ledger): undo, then return the `*ExitError`.
- **Failure in steps 6–12**, once the ledger is open: append `daemon.boot_failed{pid, step, code}`, then undo.
- **Cancellation once this boot has appended an event** (step 7's `daemon.unclean_shutdown_detected`, from step 9 on also `config.loaded`): append `daemon.stopping{pid, reason}` and `daemon.stopped{pid, uptime_seconds}`, then undo, then return nil. Without them the unclean event would be the last one, and the next start would report a second unclean shutdown for a run that stopped on request.
- **Cancellation in steps 6–8 that appended nothing** (the previous run ended cleanly, or had no events): undo only. The previous run's last event still stands, so no false unclean event is produced.
- **In `Detached` mode**, the `--detached` child writes the final `*ExitError` to stderr (`daemon.out`) as one line, `{"error":{"code":…,"message":…,"hint":…}}`, before exiting with its code.

**Shutdown.** It is triggered by a signal, `POST /api/daemon/shutdown`, or ctx cancel (reason `context`). The whole sequence is bounded by `shutdown_grace`, measured from the trigger.

1. Gate = `draining`. The status and shutdown routes keep answering.
2. Append `daemon.stopping{pid, reason}`.
3. `Gate.Wait`: wait for in-flight non-stream requests.
4. Append `daemon.stopped{pid, uptime_seconds}`.
5. `broker.Close()`: `Done` closes. Every `Follow` ends with `ErrClosed`, and every SSE adapter writes its `shutdown` frame (1 s deadline) and returns.
6. Close the connections that have not sent a request (`http.Server.Shutdown` counts them as busy until they are 5 s old, so a silent client would hold the budget), then `http.Server.Shutdown(remaining budget)`. Its `RegisterOnShutdown` hook cancels the stream contexts as a backstop. The listener closes, but the socket file stays (`SetUnlinkOnClose(false)`).
7. `ledger.Close`: drain the writer, checkpoint, close the connections.
8. Remove `daemon.json`, then `daemon.sock`.
9. Release the lock. The lock file stays.
10. Log `daemon stopped` and exit 0.

**Shutdown overruns.** If the budget expires, the daemon logs the overrunning step, force-closes the listener and the ledger, removes its files, releases the lock and exits 1. The lock is released only once the ledger's close has finished, including a close that step 7 started and the budget abandoned (Safety Invariant 1): the forced path waits up to 2 s for it, and a close that has not finished by then leaves the lock held, so that the exit releases it.
- An overrun **before step 4** leaves no `daemon.stopped`, and the next start reports an unclean shutdown.
- An overrun **after step 4** leaves the ledger clean; only the log records it.

**Detached start**, in `internal/cli`:

1. **Pre-spawn, with no daemon contacted:**
   - `Resolve`;
   - `CheckSocketPath` (78);
   - `CheckSecurity` (78), if the home exists;
   - `EnsureLayout`.
2. **Probe.** `GET /api/status` with a 2 s timeout:
   - `running` → report `already_running` and exit 0.
   - `starting` → another start is in progress. Poll until `running`, then report `already_running`.
   - `draining`, or unreachable while `LockFree` is false → in human mode, print `waiting for the previous daemon (pid N) to exit` on stderr. Poll every 100 ms until `LockFree`, then continue.
   - unreachable and the lock is free → continue.
3. **Spawn.**
   - Open `logs/daemon.out` with `O_CREATE|O_WRONLY|O_APPEND`, 0600, and record its size as the tail offset.
   - Run `os.Executable() daemon start --foreground --detached --home <abs home>` with `Setsid: true`, `Dir: <home>`, stdin `/dev/null`, and stdout/stderr → `daemon.out`.
   - `cmd.Wait` runs in a goroutine.
4. **Poll** status every 100 ms (2 s per request) until `state == running && pid == child.pid`, then print and exit 0.
5. **If the child exits first**, read `daemon.out` from the offset and take the last line that parses as `{"error":…}`:
   - `config_invalid`, `home_insecure`, `socket_path_too_long` or `ledger_schema_ahead` → re-raise that error with its exit code;
   - `daemon_already_running` → another daemon owns the home; go back to step 2;
   - anything else, or no such line → `daemon_start_failed` (69), with `details{pid, exit_code, output_tail}` holding up to 20 lines after the offset.
6. **On timeout** (`--timeout`, which bounds the whole command): SIGTERM the child's process group, wait up to 2 s, SIGKILL the group, reap the child, then report `daemon_start_timeout` (69) with the tail.

**Stop**, in `internal/cli`:

1. `POST /api/daemon/shutdown` with a 2 s timeout.
   - **Unreachable and `LockFree`:** report `not_running`, exit 0.
   - **Unreachable but the lock is held** (a daemon booting before listen, or exiting): wait for `LockFree` up to the default `shutdown_grace` + 5 s. Then report `stopped`, with the PID read from the lock file and `uptime_seconds: null`.
2. **`202`:** take `pid`, `uptime_seconds` and `shutdown_grace` from the response. The same answer comes back while starting (boot cancelled) and while draining.
3. **Wait** until `LockFree` and the socket is gone, polling every 100 ms, for up to `shutdown_grace` + 5 s. `kill(pid, 0)` is used only to enrich the message, never to decide.
4. **Report.** Print the PID and uptime and exit 0. If the wait expires, exit 69 with `daemon_stop_timeout`.

## Impact Analysis

| Component | Impact Type | Description and Risk | Required Action |
|---|---|---|---|
| Repository `~/Projetos/Pessoal/harness` | new | First code in the repo: `go.mod`, `cmd/`, `internal/`. Low risk. | `git init`; module `github.com/felipmateus/harness`; Go 1.27 |
| `<home>` state (`~/.harness`) | new | First persisted user state: ledger schema v1, durability settings and the file layout. **From the first release, migrations are append-only and immutable** (Compozy L-008/L-021: OBS-STO-009). Medium risk if the schema is changed carelessly later; F13/F14 were resolved before release for this reason. | Embedded goose migrations; read-only schema-ahead refusal |
| CLI surface (`harness …`) | new | First public surface: verbs, flags, JSON shapes, error and exit codes, output rules. | Treat `_dx.md` as the contract; later changes follow the deprecation rule below |
| `docs/roadmap.md` | modified | Feature 1 status → in progress, then done. | Update at delivery |

**Compatibility policy.** This is the project default, with no prior release.
- Persisted user state (the ledger, the home layout) migrates losslessly in the same change that alters it.
- Public surfaces (CLI verbs and flags, JSON shapes, error codes, exit codes, API routes) keep the old shape for one release behind the new one, with a named removal release.
- Internal code hard-cuts in the same change.

There are no delete targets: this is a greenfield feature.

## Extensibility Integration Plan

Not applicable. The project has no extension surfaces yet (no hooks, plugins, MCP or skills hosting). Checked against the roadmap, where `hooks`, `hosted-tools` and `skills-catalog` are later features. This feature adds none.

## Agent Manageability Plan

- **Every operation is a CLI verb** with `-o json|jsonl`: `daemon start|stop`, `status`, `events` (`--follow` with jsonl), `doctor`, `config path|validate|show`, `version`.
- **Output rules** (`_dx.md`):
  - success puts the result on stdout;
  - errors put a single JSON object on stderr and leave stdout empty;
  - reports (`doctor` 70, `config validate` 78) are documents on stdout;
  - usage errors (64) are JSON under `-o json`.
- **Exit codes:** deterministic, per the `_dx.md` Errors table (0, 1, 64, 65, 69, 70, 78, 130).
- **Idempotency:** `daemon start` and `daemon stop` are idempotent in every daemon state, so agents can call them blindly.
- **Discovery:** `status` exposes paths, schema version and config state. `doctor` exposes health with stable probe IDs, with or without a daemon.
- **Raw API access:** `curl --unix-socket <home>/daemon.sock http://harness/api/...` with the same JSON shapes.

## Config Lifecycle

- **New keys:** `daemon.shutdown_grace` (Duration, default `30s`, range 1s–5m) and `log.level` (enum `debug|info|warn|error`, default `info`).
- **File:** `<home>/config.toml`, a single layer. No workspace overlay or profiles; the reverse spec §8.1 says simplify.
- **Validation:** strict decode plus range checks, collecting every error.
- **Commands:** `harness config validate` prints all errors (report on stdout, exit 78); `config show` prints the effective config. Both work without a daemon.
- **Daemon:** an invalid file makes the daemon refuse to start. `doctor`'s `config.valid` flags a file that changed or broke after the daemon loaded it.
- **Reload:** none; changes apply on restart. `daemon stop` uses the running daemon's grace, reported in the shutdown response.
- **Docs:** the `_dx.md` Config section is the reference until a docs site exists.
- **Tests:** UT for decoding, defaults and each validation error; E2E for `config validate` exit 78.

## Testing Approach

- **Tools:** Go `testing`, table-driven, `go test -race ./...`. No mocking framework. Fakes appear only at I/O seams where needed; the real SQLite is used everywhere.
- **Short temporary homes.** macOS `t.TempDir()` paths can exceed the 103-byte socket limit, so integration and E2E tests use `os.MkdirTemp("/tmp", "hz")` through a `testhome.New(t)` helper. Homes are created at 0700 so `CheckSecurity` passes, and removed on cleanup.
- **Hooks.** In-process tests pass `daemon.Options.Hooks`. Subprocess tests build `harness` with `-tags harness_testhooks` and configure hooks through `HARNESS_TESTHOOKS`, e.g. `hold-boot-step=10`, `hold-shutdown-step=3`, `fail-boot-step=10`, `block-exit`.
- **Unit tests:**
  - `config`, `home` (resolution, path-length and security checks, lock rules, atomic write);
  - `ledger` (validation, append, idempotency and conflicts, cancellation, ordering, time, page budget, schema-ahead, pragmas);
  - `broker` (fan-out, degradation, close, late subscribe);
  - `stream.Follow` (real ledger on a temp DB plus a fake subscriber; no HTTP);
  - `admission`, `redact`, `doctor`;
  - `api` handlers and the SSE adapter (`httptest`, fake follow source, injected intervals);
  - CLI output rules, validation, start/stop decision logic (fake spawner, fake lock prober, fake clock) and exit-code mapping.
- **Integration tests** run `daemon.Run` in-process, or as a hook-enabled subprocess for crash and signal cases, on a temporary home with the real CLI client. They cover:
  - boot and shutdown ordering;
  - boot failure and cancellation;
  - persist-before-publish;
  - follow, resume and shutdown with slow or paused clients;
  - gate states, concurrent start and stop;
  - crashes (SIGKILL), file modes, no TCP.
- **Power loss** cannot be simulated in CI. The durability contract is owned by UT-029, which asserts the pragmas actually in effect, and by the ADR-001 rationale.
- **E2E tests** build the binary once in `TestMain` (`go build -o $TMP/harness ./cmd/harness`, plus a `-tags harness_testhooks` variant). They run the `_dx.md` transcripts as subprocesses, asserting stdout, stderr, exit codes and files. Human output is matched with regexes per the rendering rules.
- **Architecture test:** `internal/archtest` enforces every boundary row above.

## Development Sequencing

### Build Order

1. **Contracts first.**
   - `api/contract`, including shared validation;
   - `ledger` (schema v1, pragmas, `Appender`/`Reader`, validation, page budget, time);
   - `broker` (with `Done` and late subscribe);
   - `admission`;
   - `home` (security, lock rules, probe).

   Gate: unit tests.
2. **`daemon.Run`**: umask, signals, boot steps 1–12, the shutdown sequence, and the `daemon` stream events. Gate: integration test "start, read stream, stop".
3. **`stream.Follow`**, then the **`api`** routes and the SSE adapter. Gate: unit tests for `Follow`, then an integration test for follow and resume over the socket.
4. **`cli`:** foreground, detached start (pre-spawn checks, spawn, poll), stop, status, events, follow. Gate: Golden Path E2E. **Slice 1 ends here** (Motivating Problem solved).
5. **Resilience:**
   - unclean-shutdown and boot-failed events;
   - stale-file recovery;
   - child error re-raise and `daemon_start_failed`;
   - start race and start-while-draining;
   - idempotent stop and boot cancellation;
   - readiness timeout with kill escalation;
   - shutdown budget and stream cancellation;
   - degraded catch-up, keepalive, `Last-Event-ID`.

   Gate: crash, race and shutdown integration tests (**slice 2**).
6. **Operator tooling:**
   - doctor (daemon and local) and the `config` verbs;
   - `version`;
   - logging rotation and redaction;
   - the full error table and usage JSON;
   - the archtest.

   Gate: remaining E2E (**slice 3**).

### Technical Dependencies

- **Go toolchain 1.27 is not installed** on the development machine (checked 2026-10-28). It must be installed (e.g. `brew install go` or `mise use go@1.27`) before task 1.
- **Go modules:** `spf13/cobra`, `modernc.org/sqlite`, `pressly/goose/v3`, `BurntSushi/toml`, `gofrs/flock`, `oklog/ulid/v2`, `gopkg.in/natefinch/lumberjack.v2`. Everything else is the standard library (`net/http`, `log/slog`).
- **Verify in task 1:**
  - whether Go's `UnixListener` unlinks its socket on `Close` by default (review-001 F3c, not verified locally). The design calls `SetUnlinkOnClose(false)` explicitly, so it holds either way;
  - that `modernc.org/sqlite` honours `PRAGMA fullfsync` on darwin.
- **Stream naming convention** (`daemon` now; `session:<id>` and `loop:<run>` later) is set here. `acp-sessions` must confirm it, but it does not block this feature.

## Monitoring and Observability

The log is slog JSON at `<home>/logs/harness.log`. Every record carries `time`, `level`, `msg`, `pid`.

**Key records:**

| Message | Fields |
|---|---|
| `boot step` | `step`, `duration_ms` (debug) |
| `boot step failed` | `step`, `code`, `err` (error) |
| `boot cancelled` | `step`, `reason` (info) |
| `daemon running` | `socket`, `boot_ms` |
| `shutdown step` | `step`, `duration_ms` |
| `shutdown budget exceeded` | `step`, `budget` (error) |
| `daemon stopped` | `reason`, `uptime_seconds` |
| `subscriber degraded` | `stream`, `seq` (warn) |
| `stream write failed` | `stream`, `cursor`, `err` (info) |
| `stream error` | `stream`, `cursor`, `err` (error) |
| `append rejected` | `stream`, `type`, `reason` (warn) |
| `request` | `method`, `path`, `status`, `duration_ms` (debug) |

`subscriber degraded` carries `seq`, the sequence of the event that did not fit the subscriber's buffer, and not a follower `cursor`. The daemon logs it from the broker's degrade observer (`broker.OnDegrade`) at the moment the subscription is degraded. That is the only point that sees a stalled consumer: its follower is blocked sending to a client that stopped reading, and never reaches the check for `Degraded()`. The broker cannot see a follower's cursor, and by the time a follower notices, its cursor has advanced to about `seq - 1`, so it would add no information.

**Operational signals:**
- The `daemon` stream is itself the lifecycle audit trail; unclean shutdowns and boot failures are first-class events.
- `doctor` is the health check, available even when the daemon is down.
- There is no metrics endpoint or alerting; it is a local single-user tool, and the reverse spec notes Compozy has none either.

## Technical Considerations

### Key Decisions

| Decision | Rationale | Rejected |
|---|---|---|
| One binary is both CLI and daemon; detached start re-execs itself | One artifact to install and version; the CLI can always spawn the matching daemon | Separate `harnessd` binary (version-skew risk) |
| Idempotent `start`/`stop` in every daemon state | Agents and scripts can call them blindly (US-012); review-001 F3 | Failing on "already running", "not running", "draining" |
| The flock decides ownership, not the PID in a file; stop waits for the lock to be free | Survives PID reuse and zombies; the OS releases the lock on crash (US-005 EC-1, review-001 F10) | PID file + `kill -0` |
| Child reports its final error as one JSON line | The parent can re-raise the exact code instead of guessing from an exit code (review-001 F2) | Mapping by exit code; scraping free text |
| `synchronous=FULL` + `fullfsync` on darwin | Rule 14 holds across power loss; sequences already seen by followers are never reassigned (review-001 F1, option A) | `synchronous=NORMAL` with a cursor-ahead check |
| Rowid table with `UNIQUE (stream, seq)` | Payload BLOBs fit SQLite's rowid layout; adds a global order (review-001 F14) | `WITHOUT ROWID` |
| Follow logic in `internal/stream`, separate from SSE | One implementation for HTTP now and for `loop-engine` and MCP later; testable without HTTP (ADR-003) | Algorithm inside the HTTP handler |
| Every follow send watches `ctx` and the broker's `Done`; SSE writes have deadlines; stream contexts are cancelled on server shutdown | A paused or slow client can never hold shutdown (review-001 F4; Compozy does the same) | Relying on `Server.Shutdown` alone |
| Ports only at the ACP and loop-action edges | No speculative indirection; the real variation points get ports in their own features (ADR-003) | Full hexagonal; literal Clean Architecture |
| Subscribe-then-catch-up with resubscribe and re-catch-up on degradation | Followers never see gaps, even when slow (rule 19). Compozy switches the handler to permanent server-side polling instead; we return to live delivery | Dropping slow followers; permanent polling |
| Doctor runs locally when no daemon answers | It must work exactly when the daemon cannot start (review-001 F22) | Daemon-only doctor |
| Strict config with all errors collected | A typo never silently becomes a default (US-011); one run of `validate` shows everything | Compozy's stop-at-first-error validation (OBS-CFG-001) |
| One error envelope everywhere, plus a clear report/error split | The CLI needs one parser; avoids Compozy's 16 domain parsers (§8.2 of the reverse spec) | Domain-specific error bodies |
| Readers on a separate `query_only` pool | Reads never queue behind the writer in WAL mode | One shared pool |
| No hot reload of config | Two keys, both safe to change on restart; reload is extra state to get wrong | `SIGHUP` reload (SIGHUP now means "stop gracefully") |

### Known Risks

- **Flush cost of full durability:** `F_FULLFSYNC` on macOS can take milliseconds per commit. That is negligible for this feature. Mitigation, if session volume needs it: the writer can group-commit queued appends in one transaction, which `acp-sessions` decides together with chunk coalescing.
- **Lock-probe collisions:** a `LockFree` probe holds the lock for microseconds. A daemon starting at that instant could see `already_running`. Mitigated by the retry loop in detached start and the 3-attempt retry in foreground start.
- **Socket path length on macOS** (103 bytes): low likelihood with the default home. Mitigated by the pre-spawn check and short test homes.
- **Linux session teardown:** with systemd-logind `KillUserProcesses=yes`, closing the login session kills the detached daemon despite `setsid`. Workaround: `loginctl enable-linger` or `systemd-run --user`. Documented, not handled.
- **Writer queue saturation once session streams arrive:** not in this feature. Mitigated by chunk coalescing in `acp-sessions` (ADR-001 risks).
- **SQLite WAL growth:** WAL autocheckpoint at the 1000-page default. `doctor ledger.integrity` uses `quick_check`.

## Safety Invariants

1. Only the process holding the flock on `daemon.lock` creates or removes `daemon.sock` or `daemon.json`, or writes to `harness.db`.
2. `daemon.lock` is never unlinked, renamed or replaced. Non-owners open it without truncation and never write it. The owner writes its PID in place only after acquiring the lock.
3. `umask 077` is set before `Run` creates any file.
4. `broker.Publish(e)` is called only from the writer goroutine, only after the transaction containing `e` has committed and synced.
5. For each stream, the writer publishes events in strictly increasing `seq`, each exactly `previous + 1`.
6. Inside `stream.Follow`, the `cursor` never decreases, and an event is sent only for `seq == cursor + 1`, so no duplicate and no gap reaches any consumer (SSE or in-process).
7. Subscribing happens before the first history read on every (re)subscription.
8. Every send in `stream.Follow` also selects on `ctx.Done()` and the broker's `Done()`. Every SSE write has a deadline. No follow can block shutdown indefinitely.
9. An append with an existing `id` and identical stream, type and data returns the stored event. With different content it returns `ErrIDConflict`. Neither inserts a row nor advances the head.
10. A rejected append (validation, size, conflict, closed, DB error) does not advance the stream head.
11. The writer runs transactions under its own lifetime context. A caller's cancellation never aborts a commit in progress.
12. `daemon.json` is replaced atomically (temp file in the same directory, fsync, rename, directory fsync) and is never partially readable.
13. Shutdown order is fixed: `draining` → `daemon.stopping` → in-flight drain → `daemon.stopped` → broker close → server shutdown → ledger close → remove `daemon.json` → remove socket → release lock. The socket file is removed only at its step, never by the listener.
14. `GET /api/status` and `POST /api/daemon/shutdown` are never blocked by the admission gate. Every other route is refused while `starting` or `draining`.
15. Recorded migration files are never edited after a release. A database whose schema version exceeds the binary's is detected on a read-only connection and never opened for writing.

## File References

### Repo Files

- `docs/roadmap.md` — feature 1 scope, dependencies, and what Milestone A needs from this foundation.
- `specs/compozy_reverse_spec.md` — the observed behaviours this design mirrors or simplifies. Read OBS-DMN-001…009, OBS-STO-001…009, OBS-API-001/004/006/007, OBS-CLI-002/003/006, OBS-CFG-001/002, and §5.1, §5.5, §8.1 and §8.2.
- `docs/tasks/daemon-core/_dx.md` — the frozen public surface; E2E tests replay it verbatim.
- `docs/tasks/daemon-core/_user_stories.md` — the acceptance criteria and edge cases each task must satisfy.
- `docs/tasks/daemon-core/review-001.md` — why each revised rule exists (F1…F24); read it before changing a decision it records.

### External References

Reference implementation: CompozyOS checkout at `~/Projetos/Pessoal/compozy`, branch `study/v0.4.2`, commit `14136a511`.

**Lifecycle and boot**
- `~/Projetos/Pessoal/compozy/internal/daemon/lock.go:57-106` — non-blocking flock singleton with the PID recorded. Mirror it, but let the lock, not the PID, decide.
- `~/Projetos/Pessoal/compozy/internal/daemon/lock.go:170-182` — PID written in place with `O_TRUNC` only after `TryLock`.
- `~/Projetos/Pessoal/compozy/internal/daemon/info.go:74` — atomic `daemon.json` write.
- `~/Projetos/Pessoal/compozy/internal/daemon/boot_components.go:8-53` — ordered boot steps with an undo stack (Compozy has about 34; ours has 12).
- `~/Projetos/Pessoal/compozy/internal/admission/gate.go:35-110` — boot and drain gate.
- `~/Projetos/Pessoal/compozy/internal/daemon/daemon_lifecycle.go:124-156` — shutdown budget and order.
- `~/Projetos/Pessoal/compozy/internal/cli/daemon.go:390-436` — detached readiness poll until the PID matches.
- `~/Projetos/Pessoal/compozy/internal/procutil/detached_unix.go:30-84` — `O_APPEND` log for the detached child; SIGKILL of the process group plus a reap on failure.
- `~/Projetos/Pessoal/compozy/internal/store/migration_refusal.go:70-89` — explicit refusal of a database ahead of the embedded migrations (goose does not refuse by itself).

**Ledger and storage**
- `~/Projetos/Pessoal/compozy/internal/store/sessiondb/session_writer.go:30-89` — the single writer goroutine.
- `~/Projetos/Pessoal/compozy/internal/store/sessiondb/session_event_query.go:77-83` — `MAX(sequence)+1` inside the transaction.
- `~/Projetos/Pessoal/compozy/internal/store/sessiondb/session_event_write.go:126-152` — `AppendEventIfAbsent` idempotency.
- `~/Projetos/Pessoal/compozy/internal/store/sqlite_config.go:65-96` and `~/Projetos/Pessoal/compozy/internal/store/write.go:19-140` — SQLite pragmas and `BEGIN IMMEDIATE` retry. Compozy uses `synchronous=NORMAL`; we deliberately do not.

**Event fan-out and streaming**
- `~/Projetos/Pessoal/compozy/internal/session/manager_prompt_event_storage.go:139-161` — persist, then hooks, then publish.
- `~/Projetos/Pessoal/compozy/internal/session/session_stream_broadcast.go:16-75` — 64-event subscriber buffer and the degraded marker.
- `~/Projetos/Pessoal/compozy/internal/api/core/session_stream_handler.go:35-70` and `~/Projetos/Pessoal/compozy/internal/api/core/session_stream.go:235-346` — subscribe-then-catch-up, dedupe by sequence, server-side polling fallback. We resubscribe instead.
- `~/Projetos/Pessoal/compozy/internal/api/udsapi/server_start.go:72-90` — stream contexts cancelled through `RegisterOnShutdown`.
- `~/Projetos/Pessoal/compozy/internal/api/core/event_stream.go:127` — per-write deadline on SSE writes.

**Transport and CLI**
- `~/Projetos/Pessoal/compozy/internal/api/udsapi/server_start.go:60-80` — UDS listener with 0600 and stale-socket removal.
- `~/Projetos/Pessoal/compozy/internal/cli/client_transport.go:389-405` — the `daemon_unavailable` diagnostic with the start hint.
- `~/Projetos/Pessoal/compozy/internal/cli/client_api_errors.go:238-253` — HTTP status → sysexits mapping.

**Logging**
- `~/Projetos/Pessoal/compozy/internal/logger/logger.go:70-135` and `~/Projetos/Pessoal/compozy/internal/logger/redacting_handler.go:15-31` — slog JSON, rotation, redacting handler.
- `~/Projetos/Pessoal/compozy/internal/redact/redact.go:12-38` — redaction patterns.

### Design and Analysis Sources

- `docs/tasks/daemon-core/adrs/adr-001.md` — single SQLite file and per-stream ledger, full durability.
- `docs/tasks/daemon-core/adrs/adr-002.md` — UDS-only with the standard library.
- `docs/tasks/daemon-core/adrs/adr-003.md` — architecture style: idiomatic Go, ports only at the ACP and loop-action edges, `stream.Follow` as a shared function.
- `docs/tasks/daemon-core/review-001.md` — independent review findings and the decisions taken.

## Assumptions and Defaults

| Item | Default or assumption |
|---|---|
| Home | `--home` flag > `HARNESS_HOME` > `~/.harness`, as a cleaned absolute path |
| Files | socket `<home>/daemon.sock`; database `<home>/harness.db`; logs `<home>/logs/harness.log`; detached output `<home>/logs/daemon.out` |
| Version | `0.1.0-dev` until a release process exists; commit from `-ldflags`, else `unknown` |
| Go | 1.27; platforms `darwin`/`linux` × `arm64`/`amd64` |
| `events` | `--stream` default `daemon`, `--after` default `0`, `--limit` default `100` (max 1000); page byte budget 8 MiB; follow catch-up pages of 500 |
| Timeouts | readiness `10s`; kill escalation `2s`; stop wait `shutdown_grace + 5s`; client request timeout `2s` (status, shutdown); doctor probe `2s` each; SSE keepalive `15s` (reset per frame); SSE write deadline `10s` (final frame `1s`); follow client idle `45s` |
| Broker and writer | 64-event buffer per subscription; writer queue capacity 256 appends (full → `Append` blocks, honouring ctx) |
| Payload | valid JSON, ≤ 1 MiB per event; absent → `{}` |
| Logs | Rotation at 10 MB, 5 backups, 30 days, no compression |
| Event IDs | Generated as ULIDs in `Append` when the producer gives none; producer IDs match `^[A-Za-z0-9_.:-]{1,64}$` |
| Times | UTC, fixed width with milliseconds (`2006-01-02T15:04:05.000Z`) everywhere, wire and storage; never decreasing |
| Stop reasons | `request` (API), `signal` (SIGINT/SIGTERM/SIGHUP), `context` (in-process cancel) |
| Ledger dependency | No feature in this spec writes events other than the daemon itself |

## Architecture Decision Records

- [ADR-001: One SQLite file holding a per-stream event ledger](adrs/adr-001.md) — one `harness.db`, one writer goroutine, rowid table with `UNIQUE (stream, seq)`, idempotent IDs, full durability (`synchronous=FULL` + `fullfsync`), append-only goose migrations.
- [ADR-002: Unix socket only, served by the standard library](adrs/adr-002.md) — `net/http` over `daemon.sock` (0600) inside a private home, no TCP, no auth tokens, no router dependency.
- [ADR-003: Idiomatic Go architecture, with ports only at real variation edges](adrs/adr-003.md) — no full hexagonal or Clean layering; `AgentRuntime` and `ActionExecutor` ports deferred to `acp-sessions` and `loop-engine`; follow extracted to `internal/stream`; consumers declare their interfaces.
