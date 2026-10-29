# harness

`harness` is a local Go daemon that runs agent CLIs over ACP and records everything in a durable event ledger. The first feature, `daemon-core`, is done: one `harness` binary runs a single daemon per home, reachable over a Unix socket, with a durable and replayable SQLite event ledger plus `status`, `events`, `doctor` and `config` commands. Agent sessions come in later features; see [`docs/roadmap.md`](docs/roadmap.md).

## Requirements

- Go 1.27. `mise.toml` pins it: run `mise install`, or use a system Go 1.27.
- `make`.
- macOS or Linux. See [Platform status](#platform-status): both are verified on arm64; Linux only inside a container.

## Install from source

```sh
git clone <this repository> && cd harness
make build            # writes ./bin/harness
./bin/harness version
```

To put the binary on your `PATH`, run `go install ./cmd/harness` (it lands in `$(go env GOBIN)`, else `$(go env GOPATH)/bin`), or copy `bin/harness` anywhere you like.

A plain build reports `commit unknown`. Set it with `go build -ldflags "-X github.com/felipmateus/harness/internal/version.Commit=$(git rev-parse --short HEAD)" -o bin/harness ./cmd/harness`.

## Golden Path

Every command accepts `-o human|json|jsonl` (default `human`) and `--home <dir>`.

```console
$ harness daemon start
harness daemon started (pid 48211)
socket: /Users/you/.harness/daemon.sock

$ harness status
State     running
PID       48211
Version   0.1.0-dev
Uptime    4s
Home      /Users/you/.harness
Ledger    /Users/you/.harness/harness.db (schema 1, 1 stream, 2 events)
Config    /Users/you/.harness/config.toml (not present, defaults in use)

$ harness events --stream daemon
SEQ    TIME                      TYPE                              DATA
1      2026-10-28T22:10:03.112Z  config.loaded                     pid=48211 path=/Users/you/.harness/config.toml exists=false digest=null
2      2026-10-28T22:10:03.140Z  daemon.started                    pid=48211 version=0.1.0-dev boot_ms=41

$ harness events --stream daemon --follow &      # history first, then live events
$ harness daemon stop
3      2026-10-28T22:11:15.020Z  daemon.stopping                   pid=48211 reason=request
4      2026-10-28T22:11:15.061Z  daemon.stopped                    pid=48211 uptime_seconds=72
stream closed: daemon stopped
harness daemon stopped (pid 48211, uptime 1m12s)
```

Lines 3 and 4 come from the background `--follow`; its output and the output of `daemon stop` interleave on the same terminal. The next `harness daemon start` continues the same stream at sequence 5 and 6: history is never rewritten.

`status` and `events` need a running daemon and exit 69 (`daemon_unavailable`) without one. `doctor`, `config` and `version` work without a daemon.

Other commands you will want:

```sh
harness doctor             # health checks; works even when the daemon cannot start
harness config path        # where the optional config file lives
harness config validate    # all problems at once; exit 78 when invalid
harness config show -o json
harness daemon start --foreground   # run in this terminal; Ctrl-C stops it gracefully
```

### Try it without touching `~/.harness`

Point `HARNESS_HOME` at a short, throwaway directory first. Keep the path short: the socket lives at `<home>/daemon.sock` and the OS limits its length to 103 bytes on macOS and 107 on Linux (`$TMPDIR` on macOS is too long, so name `/tmp` explicitly).

```sh
export HARNESS_HOME="$(mktemp -d /tmp/harness.XXXXXX)"
harness daemon start && harness status && harness daemon stop
rm -rf "$HARNESS_HOME"
```

## `HARNESS_HOME`

The home is where the daemon keeps everything it owns: `daemon.sock`, `daemon.lock`, `daemon.json`, `harness.db` (the ledger), `config.toml` (optional) and `logs/harness.log`. Only one daemon runs per home.

- The home is `--home <dir>` if given, else `$HARNESS_HOME`, else `~/.harness`.
- It must be a real directory (not a symlink), owned by you and not writable by group or others. A missing home is created with mode 0700. `harness doctor` reports a home that others can read (for example 0755) and tells you the `chmod`.
- The daemon creates files as 0600 and directories as 0700.

## Config

`<home>/config.toml` is optional and read at daemon start (no hot reload). Unknown keys and out-of-range values are errors, and `harness config validate` lists all of them.

```toml
[daemon]
shutdown_grace = "30s"   # 1s to 5m: how long a graceful stop may take

[log]
level = "info"           # debug | info | warn | error
```

## Errors and exit codes

Every error has one shape on the wire, `{"error":{"code","message","hint"?,"details"?}}`, and a stable CLI exit code. With `-o json` or `-o jsonl` the error goes to stderr and stdout stays empty. The full table of codes, messages, HTTP statuses and exit codes is in [`docs/tasks/daemon-core/_dx.md`](docs/tasks/daemon-core/_dx.md) under **Errors**.

| Exit | Meaning |
|---|---|
| 0 | Success (including `daemon start` when it is already running and `daemon stop` when none is) |
| 1 | Unexpected failure |
| 64 | Usage error (bad flags or arguments) |
| 65 | Invalid input (stream id, `--after`, `--limit`) |
| 69 | Daemon unavailable, failed to start or stop, or ledger written by a newer harness |
| 70 | `harness doctor` found an error |
| 78 | Invalid config, insecure home or socket path too long |
| 130 | Interrupted by Ctrl-C |

## Platform status

- **macOS: verified.** The full suites ran green together on macOS 15 (Darwin 25.6, arm64) with Go 1.27.3.
- **Linux: verified in a container (arm64).** On 2026-10-29 the same six suites ran green, one after another, in the `golang:1.27` image (Go 1.27.3, `linux/arm64`, Docker Desktop's linuxkit kernel 6.10). Every package ran fresh; none was served from the test cache. Limits of that run:
  - it ran as root, so the unreadable-file cases skipped;
  - `linux/amd64` was cross-compiled but not run;
  - no real systemd login session was involved (see the caveat below).

To run the suites on Linux. The repository and the host module cache are mounted read-only (the read-only cache works), and the container works on a copy:

```sh
docker run --rm \
  -v "$PWD":/src:ro \
  -v "$(go env GOMODCACHE)":/modcache:ro \
  -e GOMODCACHE=/modcache -e GOCACHE=/tmp/gocache \
  -e GOFLAGS=-mod=mod -e GOPROXY=off \
  golang:1.27 sh -c '
    cp -a /src /work && cd /work &&
    make gate &&
    make test-integration &&
    make test-e2e &&
    make test-hooks &&
    make test-integration-hooks &&
    make test-e2e-hooks'
```

The unreadable-file cases skip when the container runs as root.

### Linux: `KillUserProcesses` caveat

With systemd-logind `KillUserProcesses=yes`, closing your login session kills the background daemon even though `daemon start` puts it in its own session (`setsid`). This is a known risk that is documented and not handled. Pick one workaround:

- `loginctl enable-linger "$USER"`, so your user services and processes survive logout; or
- run the daemon as a user service, for example `systemd-run --user --unit=harness-daemon harness daemon start --foreground`.

Neither workaround has been tried here: the Linux run was in a container, which has no systemd login session.

## Development and tests

`AGENTS.md` has the working rules. `make gate` is the delivery check and must pass before a task is complete.

| Target | What it runs |
|---|---|
| `make gate` | `gofmt` check, `go vet`, `go test -race ./...` (includes the architecture test) and the build |
| `make test-integration` | `go test -race -tags integration ./...` |
| `make test-e2e` | `go test -tags e2e ./e2e/...`: builds the binary and drives it as a subprocess |
| `make test-hooks` | `go test -race -tags harness_testhooks ./...` |
| `make test-integration-hooks` | `go test -race -tags "integration harness_testhooks" ./...`: the process and crash suite runs only here |
| `make test-e2e-hooks` | `go test -tags "e2e harness_testhooks" ./e2e/...` |
| `make build` | `go build -o bin/harness ./cmd/harness` |

The `harness_testhooks` tag builds the `HARNESS_TESTHOOKS` subprocess hooks (hold or fail a boot step, hold a shutdown step, block the exit). A normal build has none of them. Tests use temporary homes only and never touch `~/.harness`.

Pass `GOFLAGS=-count=1` to bypass the test cache, for example `GOFLAGS=-count=1 make test-e2e`; the E2E suites otherwise cache across source changes.

## Docs

- [`docs/roadmap.md`](docs/roadmap.md): the epic and the feature order.
- [`docs/tasks/daemon-core/`](docs/tasks/daemon-core/): the spec of this feature (`_spec.md`), its developer-experience contract (`_dx.md`), the test catalogue (`_tests.md`) and the task files.
