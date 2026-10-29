# Test Specification: daemon-core

Canonical test contract for `daemon-core`. Companion to `_spec.md`. It is derived from:
- `_user_stories.md` (behaviour);
- `_spec.md` Part II (components, safety invariants);
- `_dx.md` (CLI and API journeys).

Revised on 2026-10-28 with the findings of [review-001](review-001.md). No task references these IDs yet.

## Strategy

**Frameworks and harnesses**
- Go `testing` with table-driven cases. No mocking library.
- The real SQLite (`modernc.org/sqlite`) is used in every suite.
- Fakes exist only for interfaces at I/O seams:
  - the consumer interfaces of `api` and `stream`;
  - the clock, process spawner and lock prober in `cli`;
  - `stat` results for ownership cases in `home`.

**Homes.** Integration and E2E tests use `testhome.New(t)`. It creates a short directory via `os.MkdirTemp("/tmp", "hz")` at mode 0700, so the socket path stays under the 103-byte macOS limit and `CheckSecurity` passes, and removes it on cleanup.

**Suites**

| Suite | How it runs |
|---|---|
| Unit | `go test -race ./internal/...` |
| Integration | `*_integration_test.go`, `//go:build integration`. Runs `daemon.Run` in-process with `Options.Hooks`, or as a subprocess built with `-tags harness_testhooks` and configured through `HARNESS_TESTHOOKS` for signal and crash cases |
| E2E | `e2e/` package, `//go:build e2e`. `TestMain` builds `harness` once (and a `harness_testhooks` variant); each case runs the binary as a subprocess with `HARNESS_HOME` set |

**Conventions**
- Test names follow `Test<Unit>_<Behavior>`.
- Unit tests call `t.Parallel()` where they touch no process-global state. `umask` and signal tests are serial. E2E tests are serial per home.
- Every integration and E2E case stops its daemon in `t.Cleanup`.
- Human output is matched with regexes per the `_dx.md` rendering rules; JSON output is compared structurally.
- Power loss cannot be simulated. The durability contract is owned by UT-029, which asserts the pragmas in effect.

## Coverage Matrix

| Source | Behavior | Unit | Integration | E2E |
|---|---|---|---|---|
| US-001.AC-1 | Detached start returns when running with the child's PID | UT-040, UT-042 | — | E2E-001 |
| US-001.AC-2 | Survives terminal close (new session) | — | IT-020 | — |
| US-001.AC-3 | Idempotent start | UT-041, UT-123 | — | E2E-002 |
| US-001.EC-1 | Unrecognised boot failure → `daemon_start_failed` with details | UT-122, UT-133 | IT-021 | E2E-022 |
| US-001.EC-2 | Invalid config → 78 via child error line | UT-121 | IT-052 | E2E-003 |
| US-001.EC-3 | Socket path too long → 78, nothing spawned or created | UT-010, UT-019, UT-120 | — | E2E-004 |
| US-001.EC-4 | Concurrent starts → one daemon | UT-121 | IT-022 | — |
| US-001.EC-5 | Missing home created 0700 before spawn | UT-011, UT-120 | — | E2E-001 |
| US-001.EC-6 | Readiness timeout → SIGTERM, SIGKILL, 69 | UT-124 | IT-056 | — |
| US-001.EC-7 | Start while draining waits, then starts | UT-123 | IT-049 | — |
| US-001.EC-8 | Recognised failure re-raised with its own code | UT-121 | IT-052 | E2E-014, E2E-021 |
| US-002.AC-1 / AC-2 | Foreground run; graceful Ctrl-C stop | — | IT-001, IT-002 | E2E-015 |
| US-002.EC-1 | SIGTERM / SIGHUP graceful | — | IT-054 | — |
| US-002.EC-2 | Second signal before `stopped` → exit 1, unclean next; also in the undo of a boot that is ending | UT-137, UT-138 | IT-023, IT-061 | — |
| US-003.AC-1 | Second foreground for same home → 69 | UT-012 | IT-003 | E2E-015 |
| US-003.AC-2 | Two homes run independently | — | IT-004 | — |
| US-003.EC-1 | Lock freed after SIGKILL | — | IT-024 | — |
| US-003.EC-2 | Loser never erases the owner's PID | UT-017 | — | — |
| US-004.AC-1 | Stop returns once home released; PID and uptime | UT-125 | IT-002 | E2E-005, E2E-016 |
| US-004.AC-2 | `stopping{request}` and `stopped` with PID, in order | — | IT-002 | E2E-005 |
| US-004.AC-3 | Socket and `daemon.json` removed | — | IT-002, IT-051 | E2E-005 |
| US-004.EC-1 | Stop with no daemon → 0 `not_running` | UT-043, UT-126 | — | E2E-006 |
| US-004.EC-2 | Follower gets final events + close notice | UT-060 | IT-030 | E2E-008 |
| US-004.EC-3 | Shutdown budget overrun | — | IT-025 | — |
| US-004.EC-4 | Request while draining → 503 | UT-050 | IT-005 | — |
| US-004.EC-5 | Stop while draining is idempotent | UT-116 | IT-048 | — |
| US-004.EC-6 | Stop while starting cancels the boot | UT-116 | IT-046 | — |
| US-004.EC-7 | Paused follower cannot hold shutdown | UT-111, UT-112 | IT-050 | — |
| US-004.EC-8 | Stop timeout → 69 | UT-125 | IT-053 | — |
| US-005.AC-1 / AC-2 / AC-3 | Restart after SIGKILL; unclean event with previous PID; history intact | — | IT-024 | — |
| US-005.EC-1 | Unrelated live PID in `daemon.json` ignored | — | IT-026 | — |
| US-005.EC-2 | Mid-transaction crash leaves no partial event | — | IT-027 | — |
| US-005.EC-3 | Boot failure after ledger open → `boot_failed`, no unclean next | — | IT-047 | — |
| US-006.AC-1 | Status human | UT-044 | — | E2E-001 |
| US-006.AC-2 | Status JSON | UT-044 | — | E2E-007 |
| US-006.EC-1 | Status with no daemon → 69 | UT-045 | — | E2E-006 |
| US-006.EC-2 | Status answers while starting/draining | UT-051 | IT-005 | — |
| US-007.AC-1 | Healthy doctor via daemon | UT-070 | IT-040 | E2E-009 |
| US-007.AC-2 | Loose home perms → ERROR, 70 | UT-071 | — | E2E-010 |
| US-007.AC-3 | JSON report on stdout with `source` | UT-070, UT-132 | — | E2E-009 |
| US-007.AC-4 | Doctor without daemon runs locally | UT-075 | — | E2E-020 |
| US-007.EC-1 | Doctor never mutates | UT-072 | — | E2E-010 |
| US-007.EC-2 | Unreadable DB reported locally | UT-076 | — | — |
| US-007.EC-3 | Config changed/broken after load | UT-074 | — | — |
| US-007.EC-4 | Probe timeout isolated | UT-073 | — | — |
| US-008.AC-1 | List the daemon stream | UT-020 | — | E2E-007 |
| US-008.AC-2 | Paging with byte budget | UT-020, UT-052, UT-106 | — | E2E-007 |
| US-008.AC-3 | jsonl events + final page line | UT-127 | — | E2E-017 |
| US-008.EC-1 / EC-3 | Empty stream; after beyond head | UT-021 | — | — |
| US-008.EC-2 | Invalid input → 65, with or without a daemon | UT-053, UT-054, UT-055, UT-128 | — | E2E-011 |
| US-008.EC-4 | Page ends early at 8 MiB | UT-106 | — | — |
| US-009.AC-1 | Live follow in order, no gaps | UT-061 | IT-031 | E2E-008 |
| US-009.AC-2 | Resume with `after` / `Last-Event-ID` | UT-062, UT-069 | IT-032 | — |
| US-009.AC-3 | Close notice on stop (human and jsonl) | UT-049, UT-060 | IT-030 | E2E-008 |
| US-009.EC-1 | Slow follower degraded and caught up | UT-031, UT-063 | IT-033 | — |
| US-009.EC-2 | Follow empty stream waits | UT-064 | — | — |
| US-009.EC-3 | Many followers | UT-032 | IT-034 | — |
| US-009.EC-4 | Keepalive, reset per frame | UT-065, UT-113 | — | — |
| US-009.EC-5 | Internal error mid-stream → exit 1, not "unavailable" | UT-068, UT-130 | — | — |
| US-009.EC-6 | Drop or idle 45 s → 69 | UT-049, UT-130 | — | — |
| US-009.EC-7 | `--follow -o json` → 64 | UT-127 | — | E2E-024 |
| US-010.AC-1 | Durable before visible; survives kill; pragmas for power loss | UT-026, UT-029 | IT-024 | — |
| US-010.AC-2 | Concurrent appends consecutive | UT-022 | IT-035 | — |
| US-010.AC-3 | Idempotent append | UT-023 | — | — |
| US-010.EC-1 | Payload over 1 MiB rejected | UT-024 | — | — |
| US-010.EC-2 | Schema ahead refused, file untouched | UT-025 | — | E2E-014 |
| US-010.EC-3 | Bad type / invalid JSON rejected | UT-100, UT-101 | — | — |
| US-010.EC-4 | ID conflict rejected | UT-102 | — | — |
| US-010.EC-5 | Cancelled producer retries without duplicates | UT-104, UT-105 | — | — |
| US-011.AC-1 | No file → defaults, `exists:false` | UT-002 | IT-001 | E2E-012 |
| US-011.AC-2 | All errors listed, exit 78 | UT-001, UT-004 | — | E2E-003 |
| US-011.AC-3 | Digest recorded; `config show` | UT-003 | IT-006 | E2E-012 |
| US-011.EC-1 | Invalid TOML with line number | UT-005 | — | — |
| US-011.EC-2 | No hot reload | — | IT-006 | — |
| US-012.AC-1 | Success output rules | UT-046 | — | E2E-016 |
| US-012.AC-2 | Error output rules and exit codes | UT-047 | — | E2E-006, E2E-011 |
| US-012.AC-3 | Reports on stdout with non-zero exit | UT-132 | — | E2E-010 |
| US-012.AC-4 | Single API error shape, incl. 404/405 | UT-056, UT-115 | — | — |
| US-012.EC-1 | Usage error: JSON under `-o json`, text in human | UT-048, UT-129 | — | E2E-013, E2E-023 |
| US-013.AC-1 | File modes regardless of umask, incl. `-wal`/`-shm` | UT-013 | IT-041 | — |
| US-013.AC-2 | Log redaction | UT-080, UT-081 | IT-042 | — |
| US-013.AC-3 | No TCP listener | — | IT-043 | — |
| US-013.EC-1 | Readable-by-others home accepted, reported, unchanged | UT-016, UT-072 | — | E2E-010 |
| US-013.EC-2 | Insecure home refused before spawn | UT-016, UT-120 | IT-045 | E2E-021 |
| config (Business Rule limits) | `shutdown_grace` range boundaries; a written value that is not a duration (empty included) is invalid | UT-006, UT-007 | — | — |
| home (Assumptions: home precedence) | `--home` > `HARNESS_HOME` > default, cleaned | UT-015, UT-019 | — | — |
| home lock probe | `LockFree` true/false; releases at once | UT-018 | — | — |
| ledger.Last (rule 13) | Last event and its PID for unclean detection | UT-028 | IT-024 | — |
| ledger time and order (rule 20) | Fixed-width, non-decreasing time; global `pos` | UT-107, UT-108 | — | — |
| ledger close (Core Interfaces) | `Append` after `Close` → `ErrClosed` | UT-103 | — | — |
| Human `events` rendering (`_dx.md`) | Column widths, null, nested, key order | UT-131 | — | E2E-017 |
| broker (Core Interfaces) | Fan-out isolation; close; late subscribe; `Done` | UT-030, UT-033, UT-034 | — | — |
| `stream.Follow` termination (invariant 8) | Ends on broker close during catch-up; never blocks on a stuck consumer | UT-110, UT-111 | — | — |
| SSE adapter (API Endpoints) | Framing, shutdown/error frames, `Last-Event-ID`, write deadline, `ResponseController` through wrappers | UT-067, UT-068, UT-069, UT-112, UT-117 | — | — |
| API strictness | Unknown params / fields → 400; shutdown body | UT-057, UT-114 | — | — |
| `daemon.out` handling | `O_APPEND`, never truncated, tail from offset | UT-133 | IT-022 | — |
| Boot cancellation (rule 12) | SIGTERM during boot after `config.loaded`; after step 7 appended an unclean event | — | IT-055, IT-057 | — |
| Foreground stderr mirror (`daemon start --foreground`) | A dead stderr reader neither kills the daemon (SIGPIPE) nor stops the file log; the mirror switches itself off. SIGPIPE stays taken until the CLI's final write, so an overrun still exits 1 | UT-083, UT-136 | IT-058 | — |
| Log time format (Conventions; Assumptions: Times) | Fixed-width UTC with three fractional digits | UT-082 | — | — |
| Shutdown overrun at step 7 (Safety Invariant 1) | The forced path releases the lock only once the ledger is closed; bounded wait; exit 1 | UT-109, UT-134, UT-135 | IT-059 | — |
| Shutdown step 6 (`_spec.md` Shutdown; US-004.EC-3) | A client that connected and sent nothing, or only the start of a request, cannot hold the shutdown or overrun a short `shutdown_grace` | UT-139 | IT-060 | — |
| Socket removal order (invariant 13) | Socket kept until step 8 | — | IT-051 | — |
| Safety Invariant 1 | Only the lock owner writes `harness.db`: never released while the ledger is open (forced shutdown, and the undo of a boot cut short by a second signal) | UT-109, UT-134, UT-135, UT-138 | IT-059 | — |
| Safety Invariant 3 | umask before files | UT-013 | IT-041 | — |
| Safety Invariant 4 | Publish only after commit | UT-026 | — | — |
| Safety Invariant 5 | Publish in seq order | UT-027 | — | — |
| Safety Invariant 6/7 | Follower cursor, subscribe-before-read (`stream.Follow`) | UT-066 | — | — |
| Safety Invariant 11 | Writer ignores caller cancellation | UT-105 | — | — |
| Safety Invariant 12 | Atomic `daemon.json` | UT-014 | — | — |
| Safety Invariant 13 | Shutdown order | — | IT-002, IT-051 | — |
| Safety Invariant 14 | Gate exemptions | UT-051, UT-116 | IT-005 | — |
| Architectural Boundaries | Every direct-import rule + transitive `daemon` rule | UT-090 | — | — |
| `version` and `config path` (`_dx.md`) | Transcripts | — | — | E2E-018, E2E-019 |

## Unit Tests

### config (Spec: Config Lifecycle)

- **UT-001** (error): `config.Load(path)` — given `[daemon]\nshutdown_grase = "10s"`, returns an error list containing `{key:"daemon.shutdown_grase", code:"unknown_key"}`.
- **UT-002** (happy): `config.Load` — given a non-existent path, returns `shutdown_grace=30s`, `level="info"`, `exists=false`, `digest=nil`.
- **UT-003** (happy): `config.Load` — given a valid file, returns its values and `digest="sha256:<hex of file bytes>"`.
- **UT-004** (error): `config.Load` — given an unknown key, `log.level="verbose"` and `shutdown_grace="10m"`, returns exactly 3 errors, with codes `unknown_key`, `invalid_value` and `out_of_range`.
- **UT-005** (error): `config.Load` — given `[daemon` (unterminated), returns one `parse_error` whose message contains `line 1`.
- **UT-006** (boundary): `config.Load` — `shutdown_grace="1s"` and `"5m"` are valid; `"999ms"` and `"5m1s"` give `out_of_range`.
- **UT-007** (error): `config.Load` — a `shutdown_grace` that is written but is not a duration (`""`, `"  "`, `"30"`, `"soon"`, the integer `30`) gives exactly one `invalid_value` for `daemon.shutdown_grace`, message `daemon.shutdown_grace "<text>" is not a duration`. An empty value never falls back to the default (Business Rule 24).

### home (Spec: `internal/home`; Business Rules 1–4, 26–27; Safety Invariants 2, 12)

- **UT-010** (boundary): `CheckSocketPath` — on darwin, a socket path of exactly 103 bytes passes and 104 bytes returns `socket_path_too_long`, with the message `… is 104 bytes; at most 103 bytes on macOS and 107 on Linux`. On linux the boundary is 107/108.
- **UT-011** (happy): `EnsureLayout` — given a missing dir, creates `<home>` and `<home>/logs` with mode 0700.
- **UT-012** (error): `AcquireLock` — a second acquire on the same home from a helper subprocess returns `ErrAlreadyRunning` with the holder PID read from the lock file.
- **UT-013** (happy): with the process umask at 022 and after `Run`'s umask call, every file the daemon creates is 0600. That covers `daemon.json`, the lock file, the log file, and `harness.db` with its `-wal` and `-shm`.
- **UT-014** (state): `WriteInfo` — a crash simulated between temp write and rename leaves either the old file or none, never a partial file. A successful write is readable as valid JSON.
- **UT-015** (happy): `Resolve` precedence — `--home` > `HARNESS_HOME` > `~/.harness`.
- **UT-016** (error): `CheckSecurity` (fake `lstat` where ownership needs it):
  - **fails with `home_insecure`:** a symlink to a 0700 dir; a dir owned by another UID; mode 0770; mode 0777;
  - **passes:** mode 0700, and mode 0755 (readable by others; reported by doctor instead).
- **UT-017** (state): lock-file rules:
  - the owner holds the lock with PID 111 written;
  - a loser process tries `AcquireLock` and fails;
  - the file's content is still `111` and its inode is unchanged;
  - after the owner releases, a new owner writes its PID in place, and the inode is still unchanged.
- **UT-018** (happy): `LockFree` — returns false while another process holds the lock, and true when it does not. Immediately after a `true` probe, `AcquireLock` in the same process succeeds (the probe released the lock).
- **UT-019** (boundary): `Resolve("./a/../b")` returns the cleaned absolute path, and `CheckSocketPath` measures that cleaned path.

### ledger (Spec: Core Interfaces, Data Models; Business Rules 14–20)

- **UT-020** (happy): `Read(stream, after=1, limit=1)` on a stream with seqs 1–4 returns the page `[seq 2]` with `HasMore=true`. `Head` returns 4.
- **UT-021** (boundary): `Read` on an empty stream returns no events, and `Head` returns 0. `Read(after=99)` on head 4 returns no events.
- **UT-022** (concurrency): 50 goroutines × 20 `Append`s to stream `t` produce seqs 1..1000, with no gaps and no duplicates, each assigned once.
- **UT-023** (idempotency): `Append` with ID `X` twice with identical content returns the same `seq`, with `created=false` the second time. The row count is unchanged.
- **UT-024** (error): `Append` with 1 MiB + 1 byte of valid JSON data returns `ErrPayloadTooLarge`, and `Head` is unchanged.
- **UT-025** (error): a DB whose goose version table says 3, opened by a binary embedding version 1:
  - `Open` returns `ErrSchemaAhead{DB:3, Binary:1}`;
  - the check ran on a `mode=ro` connection;
  - no migration ran;
  - the file's bytes and mtime are unchanged.
- **UT-026** (ordering): an `OnCommit` callback that queries `Head` through the reader pool sees `Head >= ev.Seq` for every event (publish only after commit).
- **UT-027** (ordering): with 4 concurrent producers, the sequence of `OnCommit` calls per stream is strictly `+1`.
- **UT-028** (happy): `Last("daemon")` returns the highest-seq event, with its `data.pid`, or `ok=false` when empty.
- **UT-029** (state): after `Open`, on both the writer and a reader connection:
  - `PRAGMA journal_mode` is `wal`;
  - `PRAGMA synchronous` is `2` (FULL);
  - `PRAGMA busy_timeout` is `5000`;
  - on darwin, `PRAGMA fullfsync` and `PRAGMA checkpoint_fullfsync` are `1`.
- **UT-100** (error): `Append` with type `"daemon\nstarted"` returns `ErrInvalidType`, type `"Daemon"` returns `ErrInvalidType`, and stream `"9x"` returns `ErrInvalidStream`. The head does not advance.
- **UT-101** (error): `Append` with data `{"a":` returns `ErrInvalidData`. `Append` with nil data stores and returns `{}`.
- **UT-102** (idempotency): `Append` with an existing ID but different data, or a different stream, returns `ErrIDConflict`. No row is inserted, and the returned event is not the stored one.
- **UT-103** (state): after `Close`, `Append` returns `ErrClosed`.
- **UT-104** (boundary): with the writer paused by a test hook and 256 appends queued:
  - the 257th `Append` with a 50 ms deadline returns `context.DeadlineExceeded`;
  - the returned event carries the generated ID;
  - after the writer resumes, no event with that ID exists.
- **UT-105** (state): a caller's ctx is cancelled after its append was dequeued, while the writer is inside the transaction (hook):
  - the commit completes, and `Append` returns ctx's error with the ID;
  - a retry with the same ID returns the committed event with `created=false` and the same `seq`.
- **UT-106** (boundary): with 20 events of 1 MiB data each:
  - `Read(after=0, limit=1000)` returns 8 events, `HasMore=true`;
  - the next `Read(after=8)` returns 8 more;
  - a single event of exactly 1 MiB is always returned alone if needed.
- **UT-107** (ordering): with a fake clock that goes backwards between appends:
  - every stored `time` is exactly 24 characters (`…T…:…:….000Z`);
  - times never decrease in `pos` order, across a `Close`/`Open` too;
  - lexicographic order of `time` matches `pos` order.
- **UT-108** (ordering): interleaved appends to streams `a` and `b` get strictly increasing `pos` in commit order.
- **UT-109** (state): `Closed()` stays open until a `Close` has completed. With the writer held and a first `Close` whose ctx has expired, a second `Close` with an ended ctx returns its ctx error at once while `Closed()` is still open; once the writer is released `Closed()` closes, the first `Close` returns its deadline error, and `harness.db-wal` is gone.

### broker (Spec: Core Interfaces — broker)

- **UT-030** (happy): two subscriptions on `daemon` both receive a published event; a subscription on `other` receives nothing.
- **UT-031** (boundary): a subscription that never reads, after 65 publishes, has its channel closed with `Degraded()==true`, and no further publish blocks.
- **UT-032** (concurrency): 100 subscriptions × 1000 publishes; each fast subscriber receives 1000 events in order.
- **UT-033** (state): `Close()` closes every subscription with `Degraded()==false`, and a second `Close()` is a no-op.
- **UT-034** (state): after `Close()`:
  - `Done()` is closed;
  - `Subscribe` returns a subscription whose channel is already closed with `Degraded()==false`;
  - `Publish` does not panic and delivers nothing.

### stream.Follow (Spec: Follow algorithm; Safety Invariants 6–8; ADR-003)

These tests use a real ledger on a temp DB and a fake `Subscriber` that can degrade on demand. There is no HTTP; the cases assert the `Item`s received from the channel.

- **UT-060** (state): when the broker closes, `Follow` delivers pending events, then a terminal `Item{Err: ErrClosed}`, then closes the channel.
- **UT-061** (happy): history seqs 1–3 plus live 4–5 yield items with seq 1..5, in order, each once.
- **UT-062** (happy): `Follow(after=3)` on a stream holding 1–5 yields 4 and 5 only.
- **UT-063** (concurrency): a fake subscription degrades after delivering seq 10 while the ledger holds 1–20. The consumer still receives exactly 1..20, and `Subscribe` is called twice.
- **UT-064** (boundary): following an empty stream yields nothing until a publish of seq 1, then yields it.
- **UT-066** (ordering): the fake ledger and subscriber record their call order. `Subscribe` precedes the first `Read`, on the initial call and on every resubscribe. A live event with `seq <= cursor` is never yielded.
- **UT-110** (state): with 5,000 events in history (10 catch-up pages), `Done` is closed after the first page is delivered. `Follow` ends with `ErrClosed` without issuing more than one further `Read`.
- **UT-111** (concurrency): a consumer reads one item and then stops reading while live events keep arriving. When the broker closes, the `Follow` goroutine exits within 100 ms (goroutine count returns to baseline).

### SSE adapter (Spec: API Endpoints — SSE adapter; `_dx.md` SSE frames)

These tests use `httptest`, a fake follow source, and injected keepalive and write-timeout values. They check framing and timing only.

- **UT-065** (boundary): with the keepalive interval set to 50 ms and no items, exactly one `: keepalive` comment is written in the first 75 ms.
- **UT-067** (happy): an item with seq 2 of type `daemon.started`, followed by `ErrClosed`, produces exactly `id: 2\nevent: daemon.started\ndata: <event json>\n\n`, then `event: shutdown\ndata: {"reason":"daemon stopped"}\n\n`, then the end of the response.
- **UT-068** (error): a terminal item carrying a ledger read error produces an `event: error` frame with `{"error":{"code":"internal","message":"internal error"}}`, then the end of the response. No `shutdown` frame, and the error is logged.
- **UT-069** (happy): a `Last-Event-ID: 3` header with `?after=0` makes the handler start `Follow` with `after=3`. A malformed `Last-Event-ID: x` returns 400 `invalid_cursor` before any frame.
- **UT-112** (boundary): a client that stops reading, with a 50 ms write timeout and a large item, makes the handler return within 200 ms. The follow context is cancelled.
- **UT-113** (boundary): with a 50 ms keepalive interval and an item every 30 ms for 200 ms, no keepalive is written; after the items stop, one keepalive appears after 50 ms (the timer resets per frame).
- **UT-117** (happy): through the full middleware chain (recovery, admission, in-flight, logging), `http.NewResponseController(w).Flush()` and `SetWriteDeadline` succeed, because wrappers implement `Unwrap`.

### api (Spec: API Endpoints; Business Rules 6, 21–23)

- **UT-050** (state): gate `draining` → `GET /api/streams/daemon/events` returns 503 with `{"error":{"code":"daemon_draining",…}}`.
- **UT-051** (state): gate `starting` → `GET /api/status` returns 200 with `state:"starting"`, and `GET /api/streams/daemon/events` returns 503 `daemon_not_ready`.
- **UT-052** (happy): `GET /api/streams/daemon/events?after=1&limit=1` with a fake reader (head 4) returns the page `{after:1,next_after:2,has_more:true,limit:1,head:4}`.
- **UT-053** (error): stream `Daemon Log` → 400 `invalid_stream_id`, with the exact `_dx.md` message. `session:01JBJ7QK3W` → 200.
- **UT-054** (error): `after=-1` → 400 `invalid_cursor` (`after -1 must be >= 0`); `after=abc` → 400 `invalid_cursor`.
- **UT-055** (boundary): `limit=0` and `limit=1001` → 400 `invalid_limit`; `limit=1` and `limit=1000` → 200.
- **UT-056** (error): a handler panic → 500 `internal`, with a body that has no stack trace or secrets.
- **UT-057** (error): `POST /api/daemon/shutdown` with body `{"why":"x"}` → 400 `invalid_request`; with `{"reason":"signal"}` → 400 `invalid_request` (`reason must be "request"`).
- **UT-114** (error): `GET /api/streams/daemon/events/stream?limit=5` → 400 `invalid_request` (`unknown parameter "limit"`); `GET /api/status?x=1` → 400 `invalid_request`.
- **UT-115** (error):
  - `POST /api/status` → 405 JSON `method_not_allowed`;
  - `GET /api/nope` → 404 JSON `not_found`;
  - `GET /api//status` → a JSON error or status body, never `ServeMux`'s plain-text 301.
- **UT-116** (state): the shutdown route bypasses the gate:
  - gate `starting` → 202, and the boot-cancel callback is invoked once;
  - gate `draining` → 202 with the same PID, uptime and `shutdown_grace`, and no second shutdown is triggered.

### cli (Spec: Detached start, Stop, Agent Manageability Plan)

- **UT-040** (happy): `startDetached` with a fake spawner, whose fake status reaches `running` with pid 42, prints `harness daemon started (pid 42)` and returns exit 0.
- **UT-041** (idempotency): `start` when the status probe answers `running` pid 7 prints `already running (pid 7)`, exits 0, and calls the spawner 0 times.
- **UT-042** (state): readiness requires a PID match. A status of `running` with pid 99 while the child is pid 42 keeps polling; success comes only when the PIDs match.
- **UT-043** (idempotency): `stop` when the socket refuses connection and `LockFree` is true prints `harness daemon not running` and exits 0. With `-o json` it prints `{"daemon":{"stopped":false,"reason":"not_running"}}`.
- **UT-044** (happy): `status` rendering — human output has the keys State, PID, Version, Uptime, Home, Ledger, Config in that order; JSON equals the `_dx.md` shape.
- **UT-045** (error): `status` with no daemon exits 69 with the `daemon_unavailable` message and hint.
- **UT-046** (happy): for every verb with `-o json` (fake client), stdout gets exactly one JSON document and stderr nothing on success.
- **UT-047** (error): error mapping table:
  - `invalid_limit`, `invalid_request` → 65;
  - `daemon_unavailable`, `daemon_not_ready`, `daemon_start_failed`, `daemon_start_timeout`, `daemon_stop_timeout`, `ledger_schema_ahead`, `not_found` → 69;
  - `config_invalid`, `home_insecure`, `socket_path_too_long` → 78;
  - `internal`, `method_not_allowed` → 1.

  Under `-o json`, stdout is empty and stderr holds `{"error":{…}}`.
- **UT-048** (error): an unknown flag in human mode exits 64 with usage text on stderr.
- **UT-049** (state): `--follow`:
  - an `event: shutdown` frame → prints `stream closed: daemon stopped` and exits 0;
  - EOF without a final frame → exits 69 with `daemon_unavailable`.
- **UT-120** (state): pre-spawn checks, none of which spawn anything:
  - a too-long home → exit 78 `socket_path_too_long`; the spawner is not called and no directory is created;
  - a 0777 home → exit 78 `home_insecure`; the spawner is not called;
  - a missing home → created at 0700, then the spawner is called.
- **UT-121** (state): child exits first, with a `{"error":…}` line in `daemon.out` after the offset:
  - `config_invalid` → exit 78 with that message;
  - `home_insecure` → 78;
  - `ledger_schema_ahead` → 69 with that code;
  - `daemon_already_running` → the CLI returns to the status probe and reports `already_running` when the fake status becomes `running`.
- **UT-122** (error): the child exits with code 1 and no error line. Result: exit 69 `daemon_start_failed`, with `details.pid`, `details.exit_code=1` and `details.output_tail` holding the last 20 lines written after the offset. Older lines before the offset are excluded.
- **UT-123** (state): start decision table, with a fake status, a fake `LockFree` and a fake clock:
  - `running` → `already_running`, no spawn;
  - `starting` → poll, then `already_running`, no spawn;
  - `draining` → the waiting line is printed, the CLI waits until `LockFree`, then spawns;
  - `draining` and never free before `--timeout` → `daemon_start_timeout`, no spawn.
- **UT-124** (state): readiness timeout with a fake process that ignores SIGTERM. The CLI sends SIGTERM to the process group, waits 2 s (fake clock), sends SIGKILL to the group, reaps the child, then reports `daemon_start_timeout` with the tail, exit 69.
- **UT-125** (state): stop flow:
  - the 202 response `{pid:7, uptime_seconds:72, shutdown_grace:"1s"}` makes the CLI wait until `LockFree` and the socket is absent, then print `harness daemon stopped (pid 7, uptime 1m12s)`;
  - if the fake lock never frees, the CLI exits 69 with `daemon_stop_timeout` after 6 s (fake clock).
- **UT-126** (state): `stop` when unreachable while the lock is held waits until `LockFree`, then prints the stopped result with the PID read from the lock file and `uptime_seconds:null` (JSON).
- **UT-127** (happy):
  - `events -o jsonl` prints one line per event, then `{"page":{…}}`;
  - `events --follow -o json` exits 64 (`usage`);
  - the follow request URL carries no `limit` parameter;
  - a jsonl follow ends with `{"closed":{"reason":"daemon stopped"}}`.
- **UT-128** (error): client-side validation, with no network call made. For stream `Daemon Log`, `--after -1` and `--limit 5000`, the CLI exits 65, and its messages equal `contract.ValidateStream/After/Limit`'s messages, which equal the API's.
- **UT-129** (error): with an unknown flag and `-o json`, stderr is exactly `{"error":{"code":"usage",…}}` and the exit is 64.
- **UT-130** (state): follow client with a fake clock:
  - 45 s without bytes → exit 69 `daemon_unavailable`;
  - a 2 MiB `data:` line is parsed;
  - an `event: error` frame → prints `error: internal error` and exits 1.
- **UT-131** (happy): human `events` rendering:
  - columns are 5/24/32 wide with two-space separators;
  - a 32-character type is followed by two spaces, and a longer type is printed in full;
  - `null` prints as `null`;
  - nested data prints as compact JSON;
  - keys print in recorded order.
- **UT-132** (happy): reports with `-o json`:
  - `doctor` with an error item → exit 70, report on stdout, stderr empty;
  - `config validate` on an invalid file → exit 78, report on stdout, stderr empty.
- **UT-133** (state): `daemon.out` handling. Given an existing `daemon.out` with 3 lines, a spawn:
  - opens it with `O_APPEND`;
  - keeps the 3 lines;
  - records the offset at the old size;
  - excludes the old lines from the reported tail.
- **UT-136** (state): a foreground start takes SIGPIPE (`daemon.TakeSIGPIPE`) for the life of the process, not the length of `Run`. With a `Run` that fails with `shutdown_budget_exceeded`, every write of the error report to stderr runs while the hold is registered, and `execute` has released it once it returns (`TestForegroundStart/SIGPIPE_is_held_through_the_final_error_write`).

### doctor (Spec: `internal/doctor`)

- **UT-070** (happy): daemon mode, with all six probes healthy → status `ok`, `source:"daemon"`, summary `{ok:6,warn:0,error:0}`.
- **UT-071** (error): home mode 0755 → `home.permissions` severity `error` with fix `chmod 700 <home>`; status `error`.
- **UT-072** (state): running every probe in either mode never changes the mode, content or mtime of any file in the home (snapshot before and after).
- **UT-073** (error): a probe exceeding 2 s → that item has severity `error` with the message `timed out after 2s`; the other probes still run.
- **UT-074** (state): `config.valid` in daemon mode:
  - the on-disk file is valid but its digest differs from the loaded one → `warn` (`restart to apply`);
  - the on-disk file is invalid → `error` (`the next start will fail`).
- **UT-075** (happy): local mode (no daemon). Items are `daemon.reachable` (`warn`), `home.permissions`, `config.valid`, `ledger.integrity`, `ledger.schema` and `log.writable`; there is no `socket.permissions`; `source:"local"`. `ledger.*` use `ledger.Inspect` (read-only).
- **UT-076** (error): local mode with `harness.db` at mode 000 → `ledger.integrity` severity `error`, and exit 70.

### logging and redact (Spec: Monitoring; US-013)

- **UT-080** (happy): `redact.String` replaces each value with `[REDACTED]` for `Bearer abc.def`, `Authorization: Basic xyz`, `token=abc`, `password=hunter2`, `secret=s`, `api_key=k` and `https://user:pass@host`, and leaves the surrounding text intact.
- **UT-081** (happy): a slog record with attribute `err="call failed: token=abc"` is written to the file as `token=[REDACTED]`.
- **UT-082** (boundary): the `time` of a record is fixed-width UTC (`^\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d\.\d{3}Z$`), in the file and in the mirror. A record stamped `00:30:01.55676-03:00` is written as `03:30:01.556Z`; `.5` and a whole second keep their zeros (`.500Z`, `.000Z`); digits below the millisecond are cut, not rounded.
- **UT-083** (error): a mirror whose write fails (a stderr pipe whose reader has gone, `EPIPE`) is switched off at its first error: it is written to once more, not once per record. The file log keeps every record and no log call fails.

### daemon forced shutdown (Spec: Boot and shutdown sequences › Shutdown overruns; Safety Invariant 1)

- **UT-134** (state): with a fake ledger whose `Close` answers at once with an ended-context error (the answer while another `Close` is still running) and whose `Closed()` is open, the forced shutdown does not return and the lock stays held. After `Closed()` closes it returns, the lock is free, and `daemon.json` and the socket are gone. A ledger whose own `Close` finished it, and no ledger at all, release the lock at once.
- **UT-135** (error): with a fake ledger whose `Closed()` never closes, the forced shutdown gives up after its bound (50 ms here), removes `daemon.json` and the socket, and keeps the lock: the exit that follows releases it.

### daemon connections (Spec: Boot and shutdown sequences › Shutdown, step 6)

- **UT-139** (state): `connTracker` separates the connections that have not started a request from those serving one. `closeUnstarted` closes the former (and says how many), leaves the latter open and counted by `waitIdle`/`closeActive` as before, closes a connection that is accepted afterwards at once, and ignores a connection that started its request or closed (`TestConnTracker_ClosesTheConnectionsThatHaveNotStartedARequest`, `TestConnTracker_ForgetsAConnectionOnceItHasStartedOrClosed`).

### daemon boot undo (Spec: Boot and shutdown sequences › Before step 1, When the boot fails or is cancelled)

- **UT-137** (state): a second signal in the undo of a boot that ends early ends the context the undo runs under, and the actions that are left still run.
  - Given an undo whose newest action waits on its context, then removes the files, then releases the lock: while the second signal has not come, `runUndo` does not return and the lock is held. After it, the action sees its context cancelled, the files are gone, the lock is free, the stack is empty, and `runUndo` reports it was interrupted (`TestRunUndo_ASecondSignalEndsTheContextAndTheRestStillRuns`).
  - A second signal that came before the undo began: the first action already sees a cancelled context (`TestRunUndo_ASecondSignalBeforeTheUndoEndsItsContextAtOnce`).
- **UT-138** (error, Safety Invariant 1): an undo action that ignores its context and does not return (the ledger's close standing in) is waited for up to the bound (50 ms here) after the second signal, then left running; the lock, the action after it in the stack, stays held until that action has returned, and is released then.

### architecture (Spec: Architectural Boundaries)

- **UT-090** (state): `archtest` reads `go list -json ./...`.
  - For **every** row of the boundaries table, each package's direct `Imports` restricted to `internal/*` is a subset of its allowed set.
  - The transitive check: no package other than `cmd/harness` and `internal/cli` has `internal/daemon` in its `Deps`.
  - `internal/api` declares (not imports) its reader, status, doctor and gate interfaces: it does not import `internal/doctor` or `internal/broker`.

## Integration Tests

### Lifecycle (in-process `daemon.Run`, real socket, real SQLite)

- **IT-001**: run `daemon.Run` on an empty home. Expect:
  - via `GET /api/streams/daemon/events`: seq 1 `config.loaded{pid, exists:false}`, then seq 2 `daemon.started{pid}`;
  - `GET /api/status` → `running`;
  - `daemon.json` with the same PID.
- **IT-002**: run the daemon, then cancel its ctx. Expect:
  - `Run` returns nil;
  - after restart, the events: seq 3 `daemon.stopping{pid, reason:"context"}`, seq 4 `daemon.stopped{pid}`, then seq 5 `config.loaded`;
  - `daemon.sock` and `daemon.json` removed before `Run` returns;
  - the lock released, with the lock file still present.
- **IT-003**: daemon A running on home H; a second `daemon.Run` on H returns `ExitError{69, "daemon_already_running"}`. A's status still answers.
- **IT-004**: run daemons on homes H1 and H2. Both report `running`, with distinct sockets and independent `daemon` streams.
- **IT-005**: hold the gate in `starting` (hook on boot step 10). Expect:
  - status → 200 `starting`;
  - events → 503 `daemon_not_ready`.

  Then, while an in-flight request is held open by a hook during draining:
  - status → `draining`;
  - events → 503 `daemon_draining`;
  - shutdown → 202.
- **IT-006**: start, change `config.toml` (`log.level="debug"`), and wait. The `GET /api/status` digest is unchanged. After a restart, the new `config.loaded` digest differs.
- **IT-059**: `shutdown_grace=1s`; 300 producers append 200 kB events in a loop, so the writer queue is full when step 7 begins, and the hook at step 7 spends the budget down to about 150 ms. The budget ends inside `ledger.Close`. Expect:
  - `Run` returns `ExitError{1, "shutdown_budget_exceeded"}` and the log's overrun record names step 7;
  - a watcher that probes the lock from the stop request on never sees it free while the ledger is open (`Ledger.Closed()` not yet closed) (Safety Invariant 1);
  - when `Run` returns, `Closed()` is closed and `harness.db-wal` is gone;
  - `daemon.sock`, `daemon.json` are gone and the lock is free;
  - the next start finds `daemon.stopped`: no unclean shutdown.
- **IT-060**: `shutdown_grace=3s`; a client connects to `daemon.sock` and sends nothing (second case: only `GET /api/sta`, the start of a request line), then the daemon is stopped. `http.Server.Shutdown` counts a connection that has not sent a request as busy until it is 5 s old; the daemon closes those connections when its shutdown reaches step 6 (the listener closes), so no silent client consumes the budget. Expect:
  - `Run` returns nil (a graceful stop), in well under a second, not `shutdown_budget_exceeded` at step 6;
  - the silent connection is closed;
  - `daemon.sock` and `daemon.json` are gone and the lock is free.
- **IT-061**: a request is in flight and never finishes (a `POST /api/daemon/shutdown` that announces a body and sends none; the server answers `100 Continue` once its handler reads the body), so the undo of an ending boot waits on `http.Server.Shutdown`. A second signal then gives up on that wait.
  - *Cancelled boot* (`hold-boot-step=10`, first stop request = ctx cancel, second = SIGTERM): `Run` returns `ExitError{1, "shutdown_aborted"}` within 3 s of the second signal; the socket and `daemon.json` are gone and the lock is free; the stop was recorded before the undo (`daemon.stopping`, `daemon.stopped`), so the next start finds a clean ledger.
  - *Failed boot* (the step 10 hook fails the boot, SIGTERM then SIGHUP): `Run` returns the boot's own error (`ExitError{1, "listen_failed"}`) within 3 s; files and lock are gone; the stream is `config.loaded`, `daemon.boot_failed`.

### Process and crash (subprocess built with `-tags harness_testhooks`)

- **IT-020**: start detached via the CLI function from a test parent process, then kill the parent's process group. The daemon is still `running` (it is in a different session).
- **IT-021**: make `harness.db` unreadable (mode 000), then run a detached start. Expect:
  - exit 69 `daemon_start_failed`, with the tail containing `ledger`;
  - no `harness` process remains for the home after 1 s;
  - `daemon.out` keeps its earlier content.
- **IT-022**: 5 concurrent detached starts on one home produce exactly one daemon process. All 5 return exit 0, and 4 report `already_running`. `daemon.out` contains no NUL bytes and no truncated lines.
- **IT-023**: hook `hold-shutdown-step=3` on a foreground subprocess.
  1. Send SIGINT; the daemon is now draining, held at step 3.
  2. Send a second SIGINT; the process exits 1.
  3. Start again. The new start records `daemon.unclean_shutdown_detected{previous_pid: <old pid>, last_type:"daemon.stopping"}`.
- **IT-024**: start, SIGKILL the daemon process, then start again. Expect:
  - the new start succeeds with no manual cleanup;
  - the stream shows the pre-crash events with unchanged seqs;
  - then `daemon.unclean_shutdown_detected{pid:<new>, previous_pid:<old>, last_seq:2, last_type:"daemon.started"}`;
  - then `config.loaded` and `daemon.started`.
- **IT-025**: `shutdown_grace=1s` with `hold-shutdown-step=3`. Expect:
  - the process exits 1 within about 1.5 s;
  - the log contains `shutdown budget exceeded`;
  - files are removed;
  - the next start records an unclean shutdown with `previous_pid` = the old PID and `last_type:"daemon.stopping"`.
- **IT-026**: write a stale `daemon.json` holding the PID of the test process itself (alive, unrelated), then start. The start succeeds, and `daemon.json` is replaced with the new PID.
- **IT-027**: a subprocess appends in a loop; SIGKILL it mid-run. After reopening, every event row has complete JSON data, and the seqs are contiguous 1..head.
- **IT-045**: home at mode 0777 → `daemon.Run` returns `ExitError{78, "home_insecure"}`; no file is created inside the home.
- **IT-046**: hook `hold-boot-step=10`, then `POST /api/daemon/shutdown` while starting. Expect:
  - 202;
  - the boot is cancelled;
  - the stream gains `daemon.stopping{reason:"request"}` and `daemon.stopped`;
  - the process exits 0;
  - socket and `daemon.json` are absent;
  - the next start records no unclean event.
- **IT-047**: hook `fail-boot-step=10` (listen fails). Expect:
  - the stream gains `daemon.boot_failed{pid, step:10, code}`;
  - the process exits non-zero;
  - the next start (no hook) records no `daemon.unclean_shutdown_detected`.
- **IT-048**: with `hold-shutdown-step=3` and two concurrent `harness daemon stop`, both exit 0 and report the same PID once the hold is released.
- **IT-049**: with `hold-shutdown-step=3` and a `harness daemon start` issued while draining:
  - start prints the waiting line;
  - after the hold is released, a new daemon with a new PID is `running`;
  - start exits 0.
- **IT-050**: `shutdown_grace=30s`. A follower client reads the history, then stops reading (the socket buffer fills) while a test producer appends 10,000 events. `harness daemon stop` completes in under 3 s, and the daemon exits 0.
- **IT-051**: hook at shutdown step 7. When step 7 starts, `daemon.sock` still exists although the listener is closed. After `Run` returns, it is gone.
- **IT-052**: a detached child with an invalid `config.toml` writes exactly one `{"error":{"code":"config_invalid",…}}` line as the last line of its `daemon.out` output and exits 78.
- **IT-053**: hook `block-exit` (shutdown never completes) with `shutdown_grace=1s`. `harness daemon stop` exits 69 with `daemon_stop_timeout` after about 6 s.
- **IT-054**: SIGHUP to a foreground subprocess → graceful stop, `daemon.stopping{reason:"signal"}`, exit 0. The same holds for SIGTERM.
- **IT-055**: hook `hold-boot-step=10`, then SIGTERM. Expect:
  - the stream gains `daemon.stopping{reason:"signal"}` and `daemon.stopped`;
  - the process exits 0;
  - no listener was left behind.
- **IT-056**: hook `hold-boot-step=10` held forever, then `harness daemon start --timeout 1s`. Expect:
  - exit 69 `daemon_start_timeout`;
  - within 3 s no daemon process remains for the home (SIGTERM is honoured at the step boundary, or SIGKILL after 2 s).
- **IT-057**: SIGKILL a running daemon, then hook `hold-boot-step=7` on a foreground start and SIGTERM it once the unclean shutdown is detected. Expect:
  - the stream gains `daemon.unclean_shutdown_detected`, `daemon.stopping{reason:"signal"}` and `daemon.stopped`, in that order;
  - the process exits 0;
  - the next start adds only `config.loaded` and `daemon.started`: no second `daemon.unclean_shutdown_detected`.
  - The in-process form (`TestBootCancelledAfterAnUncleanEventRecordsItsStop`) also covers a cancellation after step 8, and a cancellation before step 7, where nothing is appended and the next start reports the crash once.
- **IT-058**: `harness daemon start --foreground` with its stderr on a pipe, read by nobody; the test closes the read end once the daemon is `running` (the case of `2>&1 | head -1`). Then SIGTERM. At `log.level=info` and at `debug`, expect:
  - the process exits 0, not by SIGPIPE;
  - the stream gains `daemon.stopping` and `daemon.stopped`;
  - `daemon.sock`, `daemon.json`, `harness.db-wal`, `harness.db-shm` are gone and the lock is free;
  - `harness.log` still has `daemon stopped`: the file log goes on without the mirror.
  - At `debug`, a `status` request between the close and the SIGTERM also answers `running` (every request logs a record, which reaches the dead pipe).
  - **Overrun** (`TestIT058_OverrunExitsOneWithoutAStderrReader`): `shutdown_grace=1s` and `hold-shutdown-step=3`, then SIGTERM, once with a stderr reader that is alive and once with one that is gone. Both exit 1, not by SIGPIPE: `Run` returns the overrun and the CLI then writes it to stderr, so SIGPIPE has to stay taken until that write. With the live reader, stderr carries `shutdown exceeded its budget`. The log has `shutdown budget exceeded` and no file or lock is left.

### Streams

- **IT-030**: a follower is attached and `POST /api/daemon/shutdown` is sent. The follower receives `daemon.stopping`, `daemon.stopped`, then the `shutdown` frame, then EOF.
- **IT-031**: a follower is attached; a test producer (hook `OnRunning`) appends 200 events to stream `test`. The follower receives seqs 1..200 in order, each once.
- **IT-032**: a follower reads up to seq 50 and disconnects; 50 more events are appended; it reconnects with `Last-Event-ID: 50` and receives exactly 51..100.
- **IT-033**: a follower whose client reads slowly (sleep 5 ms per frame) while 2000 events are appended still receives 1..2000 in order. The log contains `subscriber degraded` at least once, with `stream` and the `seq` of the event that did not fit.
- **IT-034**: 20 concurrent followers + 500 appends → each follower receives 1..500.
- **IT-035**: 8 goroutines append 250 events each through the in-process `Appender` while 2 followers run. The final head is 2000, and both followers saw 1..2000.

### Security and ops

- **IT-040**: `GET /api/doctor` on a fresh home gives all six items `ok`, with `source:"daemon"`.
- **IT-041**: with the test process umask at 022, after boot the modes are:
  - home and `logs/`: 0700;
  - `daemon.sock`, `daemon.json`, `daemon.lock`, `harness.db`, `harness.db-wal`, `harness.db-shm` and `logs/harness.log`: 0600.
- **IT-042**: a test hook logs `fetch failed: Authorization: Bearer abc123`. `harness.log` contains `[REDACTED]` and not `abc123`.
- **IT-043**: while the daemon runs, the process's open sockets include no TCP listener (`lsof -a -p <pid> -iTCP -sTCP:LISTEN` on darwin, `/proc/<pid>/net/tcp` on linux) — empty.

## End-to-End Tests

All cases run the built `harness` binary with `HARNESS_HOME` set to a short temp home, and replay `_dx.md` invocations. Human output is matched by regex.

### Lifecycle (US-001…US-004, US-006)

- **E2E-001**: `harness daemon start` → stdout matches `harness daemon started (pid \d+)` and the `socket:` line; exit 0. Then `harness status` → `State     running`, with the same PID. `HARNESS_HOME` did not exist beforehand and is now 0700.
- **E2E-002**: `harness daemon start` run twice → the second prints `harness daemon already running (pid N)`, exit 0; `-o json` shows `"already_running":true`.
- **E2E-003**: a `config.toml` with `shutdown_grase` and `log.level="verbose"`:
  - `harness daemon start` → exit 78, stderr starts with `error: config invalid:`, no daemon running;
  - `harness config validate -o json` → stdout `valid:false` with 2 errors, stderr empty, exit 78.
- **E2E-004**: `HARNESS_HOME` set to a path whose socket path is 113 bytes, then `harness daemon start -o json` → stderr `{"error":{"code":"socket_path_too_long",…}}`, exit 78; the home directory was not created.
- **E2E-005**: start, `harness daemon stop` → `harness daemon stopped (pid N, uptime …)`, exit 0. Then:
  - `daemon.sock` and `daemon.json` are absent;
  - after a restart, `harness events -o jsonl` shows types `config.loaded`, `daemon.started`, `daemon.stopping`, `daemon.stopped`, `config.loaded`, `daemon.started`, with seqs 1–6;
  - seq 3 has `reason:"request"`, and seqs 1–4 carry the first PID.
- **E2E-006**: with no daemon:
  - `harness daemon stop` → `harness daemon not running`, exit 0;
  - `harness status -o json` → stdout empty, stderr `{"error":{"code":"daemon_unavailable",…}}`, exit 69.
- **E2E-014**: create a DB with the start/stop cycle, then set its goose version to 99 with `sqlite3` and record the file's SHA-256. Then `harness daemon start -o json` → stderr `{"error":{"code":"ledger_schema_ahead",…}}`, exit 69, and the SHA-256 is unchanged.
- **E2E-015**: `harness daemon start --foreground` in the background → its stderr shows `daemon running`. A second `harness daemon start --foreground` → stderr `error: daemon already running for <home> (pid N)`, exit 69. Then SIGINT to the first → exit 0.
- **E2E-016**: start, then `harness daemon stop -o json` → stdout `{"daemon":{"stopped":true,"pid":N,"uptime_seconds":U}}` with integer U ≥ 0; stderr empty.
- **E2E-021**: `chmod 777 $HARNESS_HOME`, then `harness daemon start -o json` → stderr `{"error":{"code":"home_insecure",…}}`, exit 78, no daemon process.
- **E2E-022**: start and stop once (the DB exists), `chmod 000 harness.db`, then `harness daemon start -o json` → stderr `{"error":{"code":"daemon_start_failed",…,"details":{"pid":…,"exit_code":…,"output_tail":[…]}}}`, exit 69; one tail line contains `ledger`.

### Events (US-008, US-009, US-012)

- **E2E-007**: after a start, `harness events --stream daemon --after 1 --limit 1 -o json` → exactly one event, with seq 2, type `daemon.started` and a `data.pid`, and `page.has_more` matching the head. `harness status -o json` matches the `_dx.md` field set.
- **E2E-008**: `harness events --follow` in the background, then `harness daemon stop`. The follower's output ends with the `daemon.stopping` and `daemon.stopped` rows, its stderr has `stream closed: daemon stopped`, and it exits 0. The same with `-o jsonl`: the last stdout line is `{"closed":{"reason":"daemon stopped"}}`.
- **E2E-011**: with **no** daemon running, `harness events --stream "Daemon Log"`, `--limit 5000` and `--after -1` each exit 65 with the `_dx.md` message.
- **E2E-017**: after a start:
  - `harness events` human output matches the rendering regex (5/24/32 columns, `digest=null`);
  - `harness events -o jsonl` ends with a `{"page":{…,"has_more":false,…}}` line.
- **E2E-024**: `harness events --follow -o json` → exit 64, with a `usage` error on stderr.

### Doctor, config, version, usage (US-007, US-011, US-012)

- **E2E-009**: start, then `harness doctor` → 6 `OK` lines and `summary: 6 ok, 0 warn, 0 error`, exit 0; `-o json` shows `"source":"daemon"`.
- **E2E-010**: start, `chmod 755 $HARNESS_HOME`, then `harness doctor -o json` → stdout report with `home.permissions` severity `error`, stderr empty, exit 70. Afterwards the home mode is still 0755.
- **E2E-012**: with no config file, `harness config validate` → `valid: …`, exit 0; `harness config show -o json` → `{"config":{"daemon":{"shutdown_grace":"30s"},"log":{"level":"info"}}}`.
- **E2E-013**: `harness events --bogus` → exit 64 with usage text on stderr.
- **E2E-018**: `harness version` matches `harness \S+ \(commit \S+, go\S+, (darwin|linux)/(arm64|amd64)\)`; `-o json` has the keys `version`, `commit`, `go`, `platform`. No daemon is needed.
- **E2E-019**: `harness config path` prints `<HARNESS_HOME>/config.toml`, exit 0, with no daemon.
- **E2E-020**: with no daemon, `harness doctor -o json` → `"source":"local"`, `daemon.reachable` severity `warn`, no `socket.permissions` item, exit 0.
- **E2E-023**: `harness events --bogus -o json` → stderr `{"error":{"code":"usage",…}}`, stdout empty, exit 64.
