# Developer Experience: daemon-core

Public-surface contract for `daemon-core`.
- `_spec.md` Part II is designed to serve this surface.
- `_tests.md` E2E journeys use these exact invocations.

**Conventions.**
- Paths shown use `/Users/felipe` as the home directory.
- Every command accepts `-o human|json|jsonl` (default `human`) and `--home <dir>` (default `$HARNESS_HOME`, else `~/.harness`).
- Times are UTC with exactly three fractional digits: `2026-10-28T22:10:03.140Z`.

**Output rules for agents** (`-o json` and `-o jsonl`):

| Outcome | stdout | stderr |
|---|---|---|
| Success | The result document | Empty |
| **Error** | Empty | One JSON object `{"error":{"code","message","hint"?,"details"?}}` |
| **Report with findings** (`doctor` exit 70, `config validate` exit 78) | The report document | Empty |

## Golden Path

```console
$ harness daemon start
harness daemon started (pid 48211)
socket: /Users/felipe/.harness/daemon.sock

$ harness status
State     running
PID       48211
Version   0.1.0-dev
Uptime    4s
Home      /Users/felipe/.harness
Ledger    /Users/felipe/.harness/harness.db (schema 1, 1 stream, 2 events)
Config    /Users/felipe/.harness/config.toml (not present, defaults in use)

$ harness events --stream daemon
SEQ    TIME                      TYPE                              DATA
1      2026-10-28T22:10:03.112Z  config.loaded                     pid=48211 path=/Users/felipe/.harness/config.toml exists=false digest=null
2      2026-10-28T22:10:03.140Z  daemon.started                    pid=48211 version=0.1.0-dev boot_ms=41

$ harness events --stream daemon --follow &
SEQ    TIME                      TYPE                              DATA
1      2026-10-28T22:10:03.112Z  config.loaded                     pid=48211 path=/Users/felipe/.harness/config.toml exists=false digest=null
2      2026-10-28T22:10:03.140Z  daemon.started                    pid=48211 version=0.1.0-dev boot_ms=41

$ harness daemon stop
3      2026-10-28T22:11:15.020Z  daemon.stopping                   pid=48211 reason=request
4      2026-10-28T22:11:15.061Z  daemon.stopped                    pid=48211 uptime_seconds=72
stream closed: daemon stopped
harness daemon stopped (pid 48211, uptime 1m12s)
```

Lines 3 and 4 above come from the background `--follow`. Its output and the output of `daemon stop` interleave on the same terminal.

After the next `harness daemon start`, the same stream continues at sequence 5 (`config.loaded`) and 6 (`daemon.started`). History is never rewritten.

## CLI

### `harness daemon start`

Starts the daemon in the background and waits until it reports `running`.

Before spawning anything, the CLI checks the home locally:
1. The socket path fits the OS limit.
2. An existing home is private.
3. A missing home is created with mode 0700.

```console
$ harness daemon start
harness daemon started (pid 48211)
socket: /Users/felipe/.harness/daemon.sock
```

```console
$ harness daemon start -o json
{"daemon":{"pid":48211,"state":"running","socket":"/Users/felipe/.harness/daemon.sock","already_running":false}}
```

Exit code: `0`.

**Already running.** Start is idempotent: it reports the running daemon instead of failing.

```console
$ harness daemon start
harness daemon already running (pid 48211)
$ echo $?
0
```

With `-o json`, `already_running` is `true`.

**A daemon is stopping.** The CLI waits for it to exit (up to `--timeout`), then starts a new one. In human mode it shows a progress line on stderr.

```console
$ harness daemon start
waiting for the previous daemon (pid 48211) to exit
harness daemon started (pid 48300)
socket: /Users/felipe/.harness/daemon.sock
```

**The daemon exits during startup.** The CLI shows the daemon's startup output and exits `69`.

```console
$ harness daemon start
error: daemon exited during startup (pid 48230, exit 1)
hint: see /Users/felipe/.harness/logs/harness.log
--- daemon output (last 20 lines) ---
{"time":"2026-10-28T22:20:01.004Z","level":"ERROR","msg":"boot step failed","pid":48230,"step":"ledger","err":"open /Users/felipe/.harness/harness.db: permission denied"}
$ echo $?
69
```

With `-o json`, the error goes to stderr (shown here on one line):

```console
$ harness daemon start -o json
{"error":{"code":"daemon_start_failed","message":"daemon exited during startup (pid 48230, exit 1)","hint":"see /Users/felipe/.harness/logs/harness.log","details":{"pid":48230,"exit_code":1,"output_tail":["{\"time\":\"2026-10-28T22:20:01.004Z\",\"level\":\"ERROR\",\"msg\":\"boot step failed\",\"pid\":48230,\"step\":\"ledger\",\"err\":\"open /Users/felipe/.harness/harness.db: permission denied\"}"]}}}
```

**Startup errors with their own code.** When the daemon fails for a reason it recognises, the CLI reports that code instead of `daemon_start_failed`:
- `config_invalid`, exit 78;
- `home_insecure`, exit 78;
- `ledger_schema_ahead`, exit 69.

```console
$ harness daemon start
error: config invalid: /Users/felipe/.harness/config.toml: unknown key "daemon.shutdown_grase"
hint: run `harness config validate` for all problems
$ echo $?
78
```

**Readiness timeout.** The daemon is still booting when `--timeout` (default `10s`) expires. The CLI stops it: SIGTERM, then SIGKILL after 2 s if it is still alive. No daemon is left running.

```console
$ harness daemon start --timeout 2s
error: daemon did not become ready within 2s (pid 48230); it was stopped
hint: see /Users/felipe/.harness/logs/harness.log
--- daemon output (last 20 lines) ---
(no output)
$ echo $?
69
```

**Home rejected before anything starts.**

```console
$ chmod 777 ~/.harness && harness daemon start
error: home /Users/felipe/.harness is not private: mode 0777 is writable by group or others
hint: chmod 700 /Users/felipe/.harness
$ echo $?
78

$ HARNESS_HOME=/Users/felipe/projects/a-very-long-directory-name-used-only-to-show-the-limit/and-another-level/state harness daemon start
error: socket path /Users/felipe/projects/a-very-long-directory-name-used-only-to-show-the-limit/and-another-level/state/daemon.sock is 113 bytes; at most 103 bytes on macOS and 107 on Linux
hint: set HARNESS_HOME to a shorter directory
$ echo $?
78
```

A home is accepted when it is a real directory (not a symlink), owned by the current user, and not writable by group or others. A home that is only readable by others, such as mode 0755, is accepted; `doctor` reports it.

### `harness daemon start --foreground`

Runs the daemon in the current process. Logs are mirrored to stderr. Ctrl-C (SIGINT), SIGTERM or SIGHUP (terminal closed) stop it gracefully.

```console
$ harness daemon start --foreground
{"time":"2026-10-28T22:10:03.140Z","level":"INFO","msg":"daemon running","pid":48211,"socket":"/Users/felipe/.harness/daemon.sock","boot_ms":41}
^C
{"time":"2026-10-28T22:10:09.880Z","level":"INFO","msg":"daemon stopped","pid":48211,"reason":"signal","uptime_seconds":6}
```

Exit code: `0` after a graceful stop. A second signal during shutdown exits immediately with `1`.

A second foreground start for the same home fails, because only one daemon may own a home:

```console
$ harness daemon start --foreground
error: daemon already running for /Users/felipe/.harness (pid 48211)
$ echo $?
69
```

### `harness daemon stop`

Requests a graceful shutdown and waits until the daemon has released the home: its lock is free and its socket is gone. The wait is at most the daemon's `shutdown_grace` (reported by the daemon) plus 5 s.

```console
$ harness daemon stop
harness daemon stopped (pid 48211, uptime 1m12s)
```

```console
$ harness daemon stop -o json
{"daemon":{"stopped":true,"pid":48211,"uptime_seconds":72}}
```

Stop is idempotent. With no daemon:

```console
$ harness daemon stop
harness daemon not running
$ harness daemon stop -o json
{"daemon":{"stopped":false,"reason":"not_running"}}
```

A stop issued while the daemon is already stopping waits for the same exit and reports it as stopped. A stop issued while the daemon is still starting cancels the start. If the daemon cannot be reached but still holds the home (it is booting before its socket exists, or exiting), `stop` waits for it to release the home and reports it stopped with `uptime_seconds: null`.

Exit code: `0` in all these cases.

**The daemon does not exit in time:**

```console
$ harness daemon stop
error: daemon (pid 48211) did not exit within 35s
hint: see /Users/felipe/.harness/logs/harness.log
$ echo $?
69
```

### `harness status`

```console
$ harness status -o json
{"status":{"state":"running","pid":48211,"version":"0.1.0-dev","started_at":"2026-10-28T22:10:03.140Z","uptime_seconds":4,"home":"/Users/felipe/.harness","socket":"/Users/felipe/.harness/daemon.sock","ledger":{"path":"/Users/felipe/.harness/harness.db","schema_version":1,"streams":1,"events":2},"config":{"path":"/Users/felipe/.harness/config.toml","exists":false,"digest":null}}}
```

`state` is one of `starting`, `running`, `draining`.

**Daemon not running:**

```console
$ harness status
error: daemon unavailable: no daemon is listening on /Users/felipe/.harness/daemon.sock
hint: run `harness daemon start`
$ echo $?
69
```

```console
$ harness status -o json
{"error":{"code":"daemon_unavailable","message":"no daemon is listening on /Users/felipe/.harness/daemon.sock","hint":"run `harness daemon start`"}}
```

The JSON error above is written to stderr; stdout is empty.

### `harness events`

Lists events of one stream in sequence order.

```console
$ harness events --stream daemon --after 1 --limit 1
SEQ    TIME                      TYPE                              DATA
2      2026-10-28T22:10:03.140Z  daemon.started                    pid=48211 version=0.1.0-dev boot_ms=41
```

**JSON page:**

```console
$ harness events --stream daemon --after 1 --limit 1 -o json
{"events":[{"stream":"daemon","seq":2,"id":"01JBJ7QK3W5Z8V2N4T6R9YXC1D","type":"daemon.started","time":"2026-10-28T22:10:03.140Z","data":{"pid":48211,"version":"0.1.0-dev","boot_ms":41}}],"page":{"after":1,"next_after":2,"has_more":true,"limit":1,"head":4}}
```

**JSON Lines.** One event per line, then a final `page` line, so agents can tell whether more events exist:

```console
$ harness events --stream daemon -o jsonl
{"stream":"daemon","seq":1,"id":"01JBJ7QK2F0M3A7H1B9C4D6E8G","type":"config.loaded","time":"2026-10-28T22:10:03.112Z","data":{"pid":48211,"path":"/Users/felipe/.harness/config.toml","exists":false,"digest":null}}
{"stream":"daemon","seq":2,"id":"01JBJ7QK3W5Z8V2N4T6R9YXC1D","type":"daemon.started","time":"2026-10-28T22:10:03.140Z","data":{"pid":48211,"version":"0.1.0-dev","boot_ms":41}}
{"page":{"after":0,"next_after":2,"has_more":false,"limit":100,"head":2}}
```

**Options:**

| Flag | Default | Effect |
|---|---|---|
| `--stream <id>` | `daemon` | Stream to read |
| `--after <seq>` | `0` | Return events with a sequence greater than this |
| `--limit <n>` | `100` | Page size, 1–1000; not used with `--follow` |
| `--follow` | off | Print history after `--after`, then new events live, until interrupted or the daemon stops |

**Page size.** A page holds at most `--limit` events and at most 8 MiB of event data. It may therefore hold fewer events than `--limit`; `has_more` tells whether more exist.

**Human rendering.**
- Columns are `SEQ` (5 wide), `TIME` (24), `TYPE` (32) and `DATA`, separated by two spaces. A longer type is printed in full, followed by two spaces.
- `DATA` lists the payload's top-level fields as `key=value`, in the order they were recorded.
- `null` prints as `null`, and nested objects or arrays print as compact JSON.

**Follow.** `--follow` prints the history after `--after`, then new events as they are persisted, with no gaps or duplicates, even under load.

Endings:

| Ending | Human mode | `-o jsonl` | Exit |
|---|---|---|---|
| Daemon stops | `stream closed: daemon stopped` on stderr | final stdout line `{"closed":{"reason":"daemon stopped"}}` | 0 |
| Ctrl-C | — | — | 130 |
| Daemon reports an internal error mid-stream | `error: internal error` | — | 1 |
| Connection lost, or no byte received for 45 s | `daemon_unavailable` | — | 69 |

`--follow -o json` is rejected with exit 64, because a stream is not a single document; use `-o jsonl`.

A stream with no events yet returns an empty page with `head: 0`, and `--follow` waits for its first event.

**Invalid input.** Input is validated by the CLI before contacting the daemon, so these exit 65 whether or not a daemon runs. The messages are identical to the API's.

```console
$ harness events --stream "Daemon Log"
error: invalid stream id "Daemon Log": must match ^[a-z][A-Za-z0-9_.:-]{0,127}$
$ echo $?
65

$ harness events --limit 5000
error: limit 5000 must be between 1 and 1000
$ echo $?
65

$ harness events --after -1
error: after -1 must be >= 0
$ echo $?
65

$ harness events --follow -o json
error: --follow cannot be used with -o json; use -o jsonl
$ echo $?
64
```

### `harness doctor`

Runs health checks. When a daemon answers, the checks run inside it. Otherwise the CLI runs the same file-level checks itself and reports the daemon as not reachable, so `doctor` works exactly when the daemon cannot start.

```console
$ harness doctor
OK    home.permissions     /Users/felipe/.harness is 0700
OK    socket.permissions   /Users/felipe/.harness/daemon.sock is 0600
OK    config.valid         defaults in use (no config file)
OK    ledger.integrity     quick_check ok
OK    ledger.schema        schema 1 (current)
OK    log.writable         /Users/felipe/.harness/logs/harness.log is writable
summary: 6 ok, 0 warn, 0 error
```

```console
$ harness doctor -o json
{"doctor":{"status":"ok","source":"daemon","items":[{"id":"home.permissions","severity":"ok","message":"/Users/felipe/.harness is 0700"},{"id":"socket.permissions","severity":"ok","message":"/Users/felipe/.harness/daemon.sock is 0600"},{"id":"config.valid","severity":"ok","message":"defaults in use (no config file)"},{"id":"ledger.integrity","severity":"ok","message":"quick_check ok"},{"id":"ledger.schema","severity":"ok","message":"schema 1 (current)"},{"id":"log.writable","severity":"ok","message":"/Users/felipe/.harness/logs/harness.log is writable"}],"summary":{"ok":6,"warn":0,"error":0}}}
```

**Without a daemon** (`"source":"local"`):

```console
$ harness doctor
WARN  daemon.reachable     no daemon is listening on /Users/felipe/.harness/daemon.sock (checks ran locally)
OK    home.permissions     /Users/felipe/.harness is 0700
OK    config.valid         defaults in use (no config file)
OK    ledger.integrity     quick_check ok
OK    ledger.schema        schema 1 (current)
OK    log.writable         /Users/felipe/.harness/logs/harness.log is writable
summary: 5 ok, 1 warn, 0 error
$ echo $?
0
```

**`config.valid`** checks the file on disk:
- **ERROR** when the file does not validate ("the next start will fail").
- **WARN** when a running daemon loaded different content ("restart to apply").

**Exit codes:** `0` when no item has severity `error`, `70` when at least one does. A report with errors is written to stdout, also under `-o json`.

A home readable by others is reported, not fixed:

```console
$ chmod 755 ~/.harness && harness doctor
ERROR home.permissions     /Users/felipe/.harness is 0755; expected 0700
      fix: chmod 700 /Users/felipe/.harness
...
summary: 5 ok, 0 warn, 1 error
$ echo $?
70
```

### `harness config`

These commands work without a daemon.

```console
$ harness config path
/Users/felipe/.harness/config.toml

$ harness config validate
valid: /Users/felipe/.harness/config.toml

$ harness config validate -o json
{"config":{"path":"/Users/felipe/.harness/config.toml","valid":false,"errors":[{"key":"daemon.shutdown_grase","code":"unknown_key","message":"unknown key \"daemon.shutdown_grase\""},{"key":"log.level","code":"invalid_value","message":"log.level \"verbose\" must be one of debug, info, warn, error"}]}}
$ echo $?
78

$ harness config show -o json
{"config":{"daemon":{"shutdown_grace":"30s"},"log":{"level":"info"}}}
```

- `config validate` reports **all** problems, not just the first, and exits `78` when the file is invalid. The report goes to stdout.
- A missing file is valid: defaults are used.
- `config show` prints the effective configuration, defaults merged.

### `harness version`

```console
$ harness version
harness 0.1.0-dev (commit 3f9c2ab, go1.27.1, darwin/arm64)

$ harness version -o json
{"version":{"version":"0.1.0-dev","commit":"3f9c2ab","go":"go1.27.1","platform":"darwin/arm64"}}
```

It works without a daemon.

### Usage errors

```console
$ harness events --bogus -o json
{"error":{"code":"usage","message":"unknown flag: --bogus","hint":"run `harness events --help`"}}
$ echo $?
64
```

In human mode, the usage text is printed to stderr instead.

## HTTP API (over the Unix socket)

The daemon serves HTTP/1.1 only on `<home>/daemon.sock` (mode 0600). There is no TCP listener. The socket's file permission inside a private home is the only access control.

```console
$ curl -s --unix-socket ~/.harness/daemon.sock http://harness/api/status
{"status":{"state":"running","pid":48211, ...same as `harness status -o json`...}}
```

| Method | Path | Success | Notes |
|---|---|---|---|
| GET | `/api/status` | 200 `{"status":{...}}` | Always answers, including while `starting`/`draining` |
| GET | `/api/doctor` | 200 `{"doctor":{...}}` | `"source":"daemon"` |
| POST | `/api/daemon/shutdown` | 202 `{"daemon":{"stopping":true,"pid":48211,"uptime_seconds":72,"shutdown_grace":"30s"}}` | Always accepted: while `starting` it cancels the boot; while `draining` it returns the same answer. Optional body `{"reason":"request"}`; `"request"` is the only accepted value. |
| GET | `/api/streams/{stream}/events?after=0&limit=100` | 200 `{"events":[...],"page":{...}}` | |
| GET | `/api/streams/{stream}/events/stream?after=0` | 200 `text/event-stream` | `Last-Event-ID` header overrides `after`; there is no `limit` parameter |

**Strict requests.**
- An unknown query parameter or body field returns 400 `invalid_request`.
- A known path with the wrong method returns 405 `method_not_allowed`.
- Any other path returns 404 `not_found`.

All of these use the JSON error shape.

**SSE frames.** One event per frame. The `id` is the sequence, so clients resume with `Last-Event-ID`.

```text
id: 2
event: daemon.started
data: {"stream":"daemon","seq":2,"id":"01JBJ7QK3W5Z8V2N4T6R9YXC1D","type":"daemon.started","time":"2026-10-28T22:10:03.140Z","data":{"pid":48211,"version":"0.1.0-dev","boot_ms":41}}

: keepalive

event: shutdown
data: {"reason":"daemon stopped"}

```

- **Keepalive:** a `: keepalive` comment after 15 s without any frame.
- **Shutdown:** `event: shutdown` is the last frame when the daemon stops.
- **Error:** if the daemon hits an internal error mid-stream, the last frame is:

```text
event: error
data: {"error":{"code":"internal","message":"internal error"}}

```

**Error responses** always use one shape:

```json
{"error":{"code":"invalid_limit","message":"limit 5000 must be between 1 and 1000"}}
```

## Config

File: `<home>/config.toml`. It is optional; when absent, every default applies. Changes take effect on the next daemon start.

```toml
[daemon]
# How long a graceful shutdown may take before the daemon exits anyway (1s–5m).
shutdown_grace = "30s"

[log]
# Minimum level written to <home>/logs/harness.log: debug | info | warn | error.
level = "info"
```

- `daemon.shutdown_grace`: in-flight requests and streams get this long to finish when stopping. `harness daemon stop` waits for this plus 5 s, using the value the running daemon reports.
- `log.level`: filters what reaches the log file and, in foreground mode, stderr.

Unknown keys, unknown sections and out-of-range values are errors: the daemon will not start, and `config validate` lists them.

## Errors

| Condition | Code | HTTP | CLI exit | Message / hint |
|---|---|---|---|---|
| No daemon on the socket, or follow connection lost / idle 45 s | `daemon_unavailable` | — | 69 | `no daemon is listening on <socket>`; hint `run \`harness daemon start\`` |
| Request during boot (any route except status and shutdown) | `daemon_not_ready` | 503 | 69 | `daemon is starting` |
| Request during shutdown (any route except status and shutdown) | `daemon_draining` | 503 | 69 | `daemon is stopping` |
| Second daemon for the same home (foreground) | `daemon_already_running` | — | 69 | `daemon already running for <home> (pid N)` |
| Daemon exited during startup for an unrecognised reason | `daemon_start_failed` | — | 69 | `daemon exited during startup (pid N, exit C)`; `details.pid`, `details.exit_code`, `details.output_tail` (≤ 20 lines) |
| Readiness timeout on start (the daemon is then stopped) | `daemon_start_timeout` | — | 69 | `daemon did not become ready within <d> (pid N); it was stopped` |
| Daemon did not exit within `shutdown_grace` + 5 s after `stop` | `daemon_stop_timeout` | — | 69 | `daemon (pid N) did not exit within <d>`; hint `see <home>/logs/harness.log` |
| Ledger written by a newer harness | `ledger_schema_ahead` | — | 69 | `ledger schema 3 is newer than this binary supports (1)`; hint `upgrade harness` |
| Invalid config | `config_invalid` | — | 78 | `config invalid: <path>: <first error>`; hint `run \`harness config validate\`` |
| Home not private (symlink, other owner, or writable by group/others) | `home_insecure` | — | 78 | `home <path> is not private: <reason>`; hint `chmod 700 <path>` |
| Home path makes socket path too long | `socket_path_too_long` | — | 78 | `socket path <path> is <n> bytes; at most 103 bytes on macOS and 107 on Linux`; hint `set HARNESS_HOME to a shorter directory` |
| Invalid stream ID | `invalid_stream_id` | 400 | 65 | `invalid stream id "<id>": must match ^[a-z][A-Za-z0-9_.:-]{0,127}$` |
| Negative or non-numeric `after` / `Last-Event-ID` | `invalid_cursor` | 400 | 65 | `after <v> must be >= 0` |
| Limit outside 1–1000 | `invalid_limit` | 400 | 65 | `limit <n> must be between 1 and 1000` |
| Unknown query parameter or body field, bad `reason` | `invalid_request` | 400 | 65 | `unknown parameter "<name>"` / `unknown field "<name>"` / `reason must be "request"` |
| Wrong method on a known path | `method_not_allowed` | 405 | 1 | `method <M> not allowed on <path>` |
| Unknown route | `not_found` | 404 | 69 | `no route for <method> <path>` |
| Bad flags or arguments | `usage` | — | 64 | the parse error; hint `run \`harness <verb> --help\`` |
| Doctor found an error | — | 200 | 70 | the doctor report itself, on stdout |
| Config file invalid in `config validate` | — | — | 78 | the validation report itself, on stdout |
| Unexpected failure (including an SSE `event: error` frame) | `internal` | 500 | 1 | `internal error` (details only in the log) |
| Interrupted by Ctrl-C | — | — | 130 | — |
