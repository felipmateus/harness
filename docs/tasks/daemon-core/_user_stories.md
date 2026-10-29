# User Stories: daemon-core

Canonical behavior catalog for `daemon-core`. Companion to `_spec.md`. Consumed by:
- `_spec.md` Part II (component mapping);
- `_tests.md` (coverage matrix).

There is no UI, so no `_uiux.md`. Revised on 2026-10-28 with the findings of [review-001](review-001.md).

## Personas

- **Operator**: the developer running harness on their own macOS or Linux machine. They start, stop and inspect the daemon from a terminal, and expect it to keep working after the terminal closes.
- **Agent**: a coding agent or script that drives harness through the CLI. It reads structured output (`-o json|jsonl`), branches on exit codes and error codes, and never parses human text.
- **Feature author**: whoever builds the next roadmap features (`acp-sessions`, `loop-engine`). They need a ledger that never loses or reorders events, so session and loop events can rely on it.

## Story Index

| ID | Feature Area | Persona | Story |
|---|---|---|---|
| US-001 | Lifecycle | Operator | Start the daemon in the background and know when it is ready |
| US-002 | Lifecycle | Operator | Run the daemon in the foreground for debugging |
| US-003 | Lifecycle | Operator | Only one daemon can own a home |
| US-004 | Lifecycle | Operator | Stop the daemon gracefully |
| US-005 | Lifecycle | Operator | Recover cleanly after a crash |
| US-006 | Inspection | Operator | See the daemon's status |
| US-007 | Inspection | Operator | Diagnose setup problems, even when the daemon cannot start |
| US-008 | Event stream | Operator | Read the daemon's event history |
| US-009 | Event stream | Operator | Follow events live without gaps |
| US-010 | Event stream | Feature author | Rely on a durable, ordered ledger |
| US-011 | Configuration | Operator | Configure the daemon with a validated file |
| US-012 | Automation | Agent | Operate harness through structured output and exit codes |
| US-013 | Security | Operator | Keep the daemon private to my user and its logs free of secrets |

## Lifecycle

### US-001: Start in the background

**As an** operator, **I want** `harness daemon start` to launch the daemon in the background and return only once it is ready, **so that** the next command I run works immediately.

Acceptance criteria:

- AC-1: Given no daemon is running, when I run `harness daemon start`, then it prints the PID and socket path and exits 0, and `harness status` immediately reports `running` with that PID.
- AC-2: Given the daemon was started from a terminal, when I close that terminal, then the daemon keeps running.
- AC-3: Given a daemon is already running for the home, when I run `harness daemon start`, then it reports `already running (pid N)` and exits 0 without starting a second process.

Edge cases:

- **EC-1: The daemon exits during boot for an unrecognised reason** (e.g. unreadable database). `start` exits 69 with `daemon_start_failed`, including the PID, the exit code and the last 20 lines of daemon output (`details.output_tail` in JSON). No daemon stays running.
- **EC-2: The config file is invalid.** `start` exits 78 with `config_invalid` and the first error. The daemon does not start.
- **EC-3: The socket path is too long.** The home path makes the socket path longer than 103 bytes (macOS) or 107 bytes (Linux), measured on the cleaned absolute path. `start` exits 78 with `socket_path_too_long` and a hint to shorten `HARNESS_HOME`. No process is spawned and no directory is created.
- **EC-4: Two starts run at the same moment.** Exactly one daemon runs. Both commands exit 0; one reports `already running`.
- **EC-5: The home directory does not exist.** It is created with mode 0700 before the daemon is spawned.
- **EC-6: The daemon is still booting when `--timeout` expires.**
  - `start` stops it: SIGTERM, then SIGKILL after 2 s.
  - `start` exits 69 with `daemon_start_timeout` and the output tail.
  - No daemon stays running.
- **EC-7: A daemon is draining (stopping).** `start` waits until it has released the home, then starts a new daemon and reports the new PID, exit 0. If the old daemon has not released the home within `--timeout`, `start` exits 69 with `daemon_start_timeout`. It never reports `already running` for a daemon that is going away.
- **EC-8: The daemon fails for a recognised reason** (`ledger_schema_ahead`, `home_insecure`). `start` reports that code with its own exit code, not `daemon_start_failed`.

### US-002: Run in the foreground

**As an** operator, **I want** `harness daemon start --foreground` to run the daemon in my terminal with logs on stderr, **so that** I can watch and debug it.

Acceptance criteria:

- AC-1: Given no daemon is running, when I run `start --foreground`, then log lines appear on stderr and the command does not return while the daemon runs.
- AC-2: Given a foreground daemon, when I press Ctrl-C, then it stops gracefully and the command exits 0. The `daemon.stopping` (reason `signal`) and `daemon.stopped` events are recorded.

Edge cases:

- EC-1: SIGTERM or SIGHUP (terminal closed) instead of Ctrl-C → same graceful stop, exit 0.
- EC-2: A second signal arrives before `daemon.stopped` is recorded → the daemon exits immediately with code 1, and the next start detects an unclean shutdown (US-005).

### US-003: One daemon per home

**As an** operator, **I want** at most one daemon per home directory, **so that** two daemons never write the same state.

Acceptance criteria:

- AC-1: Given a daemon is running for home H, when a foreground start runs for H, then it fails with `daemon_already_running` (exit 69), and the first daemon keeps serving.
- AC-2: Given two different homes (via `--home` or `HARNESS_HOME`), when a daemon starts for each, then both run independently.

Edge cases:

- EC-1: The previous daemon was killed with SIGKILL → its lock is released by the OS, and a new start succeeds (see US-005).
- EC-2: A losing start never erases the running daemon's recorded PID, so `daemon_already_running` always names the right PID.

### US-004: Stop gracefully

**As an** operator, **I want** `harness daemon stop` to shut the daemon down cleanly, **so that** no event is lost and no stale files remain.

Acceptance criteria:

- AC-1: Given a running daemon, when I run `harness daemon stop`, then it exits 0 once the daemon has released the home (lock free, socket gone), printing the PID and the daemon's uptime.
- AC-2: Given a graceful stop, when I read the daemon stream after restarting, then it contains `daemon.stopping` (reason `request`) and then `daemon.stopped`, both carrying the daemon's PID, before the next `config.loaded`.
- AC-3: Given a graceful stop, when it completes, then the socket file and `daemon.json` no longer exist.

Edge cases:

- EC-1: No daemon is running → `stop` prints `not running` and exits 0 (`"stopped":false,"reason":"not_running"`).
- EC-2: A follower is attached during stop → it receives the final events, then the close notice, and exits 0.
- EC-3: Shutdown exceeds `daemon.shutdown_grace` → the daemon exits anyway (code 1), logging which step overran. `stop` returns once the home is released.
- EC-4: A new request (other than status or shutdown) arrives while draining → `503 daemon_draining` (CLI exit 69).
- EC-5: `stop` runs while the daemon is already stopping (e.g. a second `stop`, or after a SIGTERM) → it waits for the same exit and reports it as stopped, exit 0.
- EC-6: `stop` runs while the daemon is still starting → the start is cancelled. The daemon records `daemon.stopping` (reason `request`) and `daemon.stopped` if it had already recorded `config.loaded`, then exits. `stop` exits 0.
- EC-7: A follower stops reading (e.g. `--follow | less` paused) during stop → shutdown still completes well within the grace period.
- EC-8: The daemon has not released the home within `shutdown_grace` + 5 s → `stop` exits 69 with `daemon_stop_timeout`, naming the PID.

### US-005: Recover after a crash

**As an** operator, **I want** a start after a crash, SIGKILL, OS crash or power loss to clean up leftovers and tell me the previous run ended badly, **so that** I never have to delete files by hand.

Acceptance criteria:

- AC-1: Given the previous daemon was SIGKILLed, when I run `harness daemon start`, then it starts normally despite the stale socket and `daemon.json`.
- AC-2: Given the same situation, when I read the daemon stream, then a `daemon.unclean_shutdown_detected` event precedes the new `config.loaded`. It carries the previous PID (taken from the last recorded event), the last recorded sequence and its type.
- AC-3: Given the crash happened during normal operation, when the daemon restarts, then every event persisted before the crash is still readable, in the same order with the same sequences.

Edge cases:

- EC-1: `daemon.json` exists but the PID in it belongs to an unrelated live process → the file is treated as stale anyway. The lock, not the PID, decides ownership.
- EC-2: The database was left mid-transaction → SQLite recovery applies, and only complete events are visible.
- EC-3: The previous start failed after the ledger was open (e.g. the socket could not be created) → it recorded `daemon.boot_failed`, and the next start does not report an unclean shutdown.

## Inspection

### US-006: See status

**As an** operator, **I want** `harness status` to show state, PID, version, uptime, home, ledger and config, **so that** I can tell at a glance whether harness is healthy.

Acceptance criteria:

- AC-1: Given a running daemon, when I run `harness status`, then it shows the state `running`, PID, version, uptime, home, ledger path, schema version, stream and event counts, and config path and presence.
- AC-2: Given a running daemon, when I run `harness status -o json`, then stdout holds a single JSON object `{"status":{…}}` with the fields of `_dx.md`.

Edge cases:

- EC-1: No daemon → exit 69 with `daemon_unavailable` and the start hint.
- EC-2: The daemon is still booting or is draining → status answers with state `starting` or `draining` (status is never blocked by the gate).

### US-007: Diagnose problems

**As an** operator, **I want** `harness doctor` to check permissions, config, ledger integrity and schema, and log writability, whether or not the daemon runs, **so that** I can fix problems, including the ones that stop the daemon from starting.

Acceptance criteria:

- AC-1: Given a healthy running daemon, when I run `harness doctor`, then every item is `OK` and the exit code is 0.
- AC-2: Given the home directory is 0755, when I run `harness doctor`, then `home.permissions` is `ERROR` with the suggested `chmod 700` fix, and the exit code is 70.
- AC-3: Given any outcome, when I use `-o json`, then stdout holds the report. It lists every item with `id`, `severity` and `message`, plus `source` (`daemon` or `local`) and a summary of counts.
- AC-4: Given no daemon is running, when I run `harness doctor`, then the file-level checks run locally, `daemon.reachable` is `WARN`, and the exit code follows the same rule (0, or 70 if any error).

Edge cases:

- EC-1: Doctor reports problems but never changes files or permissions itself.
- EC-2: The daemon cannot start because the database is unreadable → `doctor`, run without a daemon, reports `ledger.integrity` as `ERROR`.
- EC-3: The config file on disk was edited while the daemon runs → `config.valid` is `WARN` ("restart to apply") if the file is valid, and `ERROR` ("the next start will fail") if it is not.
- EC-4: A single check takes longer than 2 s → that item is `ERROR` ("timed out after 2s"), and the other checks still run.

## Event stream

### US-008: Read history

**As an** operator, **I want** `harness events --stream daemon` to list what the daemon recorded, **so that** I can see when it started, stopped or crashed.

Acceptance criteria:

- AC-1: Given a daemon that has started once, when I list the daemon stream, then I see `config.loaded` (seq 1) and `daemon.started` (seq 2) with their times and data.
- AC-2: Given `--after N --limit L`, when I list, then I get at most L events with sequence > N, in ascending order, within the 8 MiB page budget. The page reports `next_after`, `has_more` and the stream `head`.
- AC-3: Given `-o jsonl`, when I list, then each line is one complete event object, and the last line is the `page` object.

Edge cases:

- EC-1: A stream with no events → an empty list, `head: 0`, exit 0.
- EC-2: An invalid stream ID, a negative `--after` or a limit outside 1–1000 → exit 65 with `invalid_stream_id`, `invalid_cursor` or `invalid_limit`, whether or not a daemon runs. The messages are identical to the API's, and nothing is clamped silently.
- EC-3: `--after` beyond the head → an empty list with the real `head`, exit 0.
- EC-4: Events whose data adds up to more than 8 MiB → the page ends early with `has_more: true`, and the next page continues from `next_after`.

### US-009: Follow live without gaps

**As an** operator, **I want** `harness events --follow` to show history and then new events as they happen, **so that** I can watch the daemon live and trust I missed nothing.

Acceptance criteria:

- AC-1: Given a follower, when the daemon persists new events, then each appears once, in sequence order, with no gaps.
- AC-2: Given a follower that disconnects after seeing sequence N, when it reconnects with `--after N` (or `Last-Event-ID: N` over the API), then it receives exactly the events after N, with no duplicates.
- AC-3: Given the daemon stops, when the follower is attached, then it prints the final events and the close notice (`stream closed: daemon stopped`, or a final `{"closed":…}` line in jsonl), and exits 0.

Edge cases:

- EC-1: The follower is too slow to keep up with a burst → it still receives every event in order, because the server catches it up from the ledger instead of dropping events.
- EC-2: Following a stream that has no events yet → it waits until the first event appears.
- EC-3: Many followers on the same stream → each receives the full sequence independently.
- EC-4: Silence for a long time → keepalive frames keep the connection open, and the follower does not time out.
- EC-5: The daemon hits an internal error mid-stream → the follower prints `error: internal error` and exits 1. It never claims the daemon is not running.
- EC-6: The connection drops without a close notice, or nothing arrives for 45 s → exit 69 with `daemon_unavailable`.
- EC-7: `--follow -o json` → exit 64 with a `usage` error pointing to `-o jsonl`.

### US-010: Durable, ordered ledger

**As a** feature author, **I want** every recorded event to be durably persisted before anyone can observe it, with a per-stream sequence that never has gaps or reorderings, **so that** session and loop features can build on the ledger without their own persistence.

Acceptance criteria:

- AC-1: Given an event is observed by any follower, when the daemon process is killed or the machine loses power right after, then that event is readable after restart with the same sequence.
- AC-2: Given concurrent appends to one stream, when they complete, then their sequences are consecutive integers starting at head+1, with no gaps.
- AC-3: Given an append with an event ID that already exists with identical content, when it is retried, then no new event is created and the original sequence is returned.

Edge cases:

- EC-1: An event payload over 1 MiB → the append is rejected (internal error, not a wire code), and the sequence does not advance.
- EC-2: The database file belongs to a newer schema than the binary knows → the daemon refuses to start with `ledger_schema_ahead`, leaving the file byte-for-byte untouched.
- EC-3: An append with an invalid type (e.g. containing a newline) or data that is not valid JSON → rejected, and the head does not advance.
- EC-4: An append reusing an existing ID with different stream, type or data → rejected as an ID conflict. The caller never receives another stream's event.
- EC-5: The producer gives up waiting (its context ends) after the append was queued → the event may still be committed. The producer can retry with the same ID and gets the committed event back, never a duplicate.

## Configuration

### US-011: Validated config

**As an** operator, **I want** an optional `config.toml` that is strictly validated, **so that** a typo never silently falls back to a default.

Acceptance criteria:

- AC-1: Given no config file, when the daemon starts, then defaults apply and `config.loaded` records `exists: false`.
- AC-2: Given a file with an unknown key or an out-of-range value, when I run `harness config validate`, then every problem is listed with its key and code, and the exit code is 78.
- AC-3: Given a valid file, when the daemon starts, then `config.loaded` records the path and a SHA-256 digest of the file, and `config show` prints the effective values.

Edge cases:

- EC-1: The file is not valid TOML → `config_invalid` with the line number.
- EC-2: The config changes while the daemon runs → nothing changes until the next start (no hot reload in this feature).

## Automation

### US-012: Agent-operable CLI

**As an** agent, **I want** every command to offer `-o json|jsonl` with errors as JSON on stderr, and documented exit codes, **so that** I can operate harness without parsing human text.

Acceptance criteria:

- AC-1: Given any command with `-o json` or `jsonl`, when it succeeds, then stdout holds the result and stderr is empty. With `-o json` the result is exactly one JSON document.
- AC-2: Given any command with `-o json` or `jsonl`, when it fails with an error, then stdout is empty, stderr holds `{"error":{"code","message","hint"?,"details"?}}`, and the exit code matches the `_dx.md` table.
- AC-3: Given `doctor` with findings (exit 70) or `config validate` on an invalid file (exit 78), when run with `-o json`, then the report is on stdout and stderr is empty. These are reports, not errors.
- AC-4: Given the API, when any request fails (including wrong method or unknown path), then the body uses the single error shape `{"error":{"code","message"}}`.

Edge cases:

- EC-1: Unknown flag with `-o json` → exit 64 with `{"error":{"code":"usage",…}}` on stderr. In human mode the usage text is printed instead.

## Security

### US-013: Private daemon, clean logs

**As an** operator, **I want** the daemon reachable only by my user and its logs free of secrets, **so that** other local users cannot drive it and credentials never leak into files.

Acceptance criteria:

- AC-1: Given a running daemon, when I inspect the files, then the home and `logs/` are 0700. The socket, `daemon.json`, `daemon.lock`, `harness.db`, `harness.db-wal`, `harness.db-shm` and the log files are 0600, regardless of my umask.
- AC-2: Given a log record contains a bearer token, an `Authorization` header, a `password=`/`token=`/`secret=`/`api_key=` assignment or URL credentials, when it is written, then the secret value is replaced by `[REDACTED]`.
- AC-3: Given the daemon, when it runs, then it listens on no TCP port.

Edge cases:

- EC-1: An existing home that is only readable by others (e.g. 0755) → the daemon still starts. `doctor` reports `home.permissions` as an error with the fix. Permissions are not silently changed.
- EC-2: An existing home that is a symlink, is owned by another user, or is writable by group or others → `start` refuses with `home_insecure` (exit 78) before spawning anything. Any user able to write the directory could replace the socket.
