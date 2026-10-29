# Task 9 memory

## Objective Snapshot

Final verification, operator docs and roadmap for `daemon-core`, completed 2026-10-29:
- every suite run together on macOS (below);
- Linux recorded as **unverified**, with the reason and the commands for a later run;
- the two open technical checks answered;
- `README.md` written and walked through on temporary homes;
- `docs/roadmap.md` marks feature 1 done.

No production code and no test changed. The task owns no test IDs.

## Open technical checks (both match the spec's assumption)

| Check | Result | Evidence |
|---|---|---|
| IT-051: does Go unlink a Unix socket on `Close` by default? | **Yes.** Closing a `UnixListener` with default settings unlinks its socket. The design calls `SetUnlinkOnClose(false)` explicitly, so it holds either way (spec Technical Dependencies). | `go test -count=1 -tags integration -v -run TestIT051 ./internal/daemon/`: `Go go1.27.3: closing a UnixListener with the default settings unlinks its socket: true` |
| UT-029: does `modernc.org/sqlite` honour `PRAGMA fullfsync` on darwin? | **Yes.** `fullfsync` and `checkpoint_fullfsync` read back `1` on the writer and on all 4 readers. | `go test -count=1 -v -run TestOpen_DurabilityPragmasAreInEffectOnEveryConnection ./internal/ledger/`: `writer: fullfsync=1 checkpoint_fullfsync=1 (GOOS=darwin)`, same for `reader 0`..`reader 3` |

Neither contradicts the spec. The first was re-observed on Go 1.27.3 darwin/arm64 only; the Linux half is not confirmed (see below), although the net package documents the default ("unlink only when package net created the socket") for every Unix.

## macOS verification (macOS 15, Darwin 25.6, arm64, Go 1.27.3)

All run on the final tree, 2026-10-28 23:57 to 2026-10-29 00:01 (-03), from `/Users/felipemateusdecarvalho/Projetos/Pessoal/harness`, with `GOFLAGS=-count=1` so no cached result hides a run. Wall time is the `time` total.

| # | Command | Result | Wall time |
|---|---|---|---|
| 1 | `GOFLAGS=-count=1 make gate` (gofmt check, `go vet ./...`, `go test -race ./...`, build) | PASS, 14 packages ok | 17.8 s |
| 2 | `GOFLAGS=-count=1 make test-integration` (`go test -race -tags integration ./...`) | PASS, 14 packages ok (`internal/daemon` 56.5 s) | 58.0 s |
| 3 | `GOFLAGS=-count=1 make test-e2e` (`go test -tags e2e ./e2e/...`) | PASS | 7.5 s |
| 4 | `GOFLAGS=-count=1 make test-hooks` (`go test -race -tags harness_testhooks ./...`) | PASS, 14 packages ok | 16.7 s |
| 5 | `GOFLAGS=-count=1 make test-integration-hooks` (`go test -race -tags "integration harness_testhooks" ./...`) | PASS, 14 packages ok (`internal/daemon` 77.0 s, the process and crash suite) | 1 m 18 s |
| 6 | `GOFLAGS=-count=1 make test-e2e-hooks` (`go test -tags "e2e harness_testhooks" ./e2e/...`) | PASS | 7.9 s |
| 7 | `go test -race -count=1 ./internal/archtest/` | PASS | 2.2 s |
| 8 | `go test -count=1 -tags integration -v -run TestIT051 ./internal/daemon/` | PASS (open check) | 0.5 s |
| 9 | `go test -count=1 -v -run TestOpen_DurabilityPragmasAreInEffectOnEveryConnection ./internal/ledger/` | PASS (open check) | 0.5 s |

- Runs 1 to 6 were sequential, not parallel, so the wall-clock SSE tests (UT-065, UT-112, UT-113) saw no host load from another suite. None needed a retry.
- Compile-only cross-check (not a Linux run): `GOOS=linux GOARCH=arm64 go vet -tags "integration e2e harness_testhooks" ./...` passed, and `GOOS=linux GOARCH=amd64 go build -o /dev/null ./cmd/harness` passed. This shows the code compiles for Linux; it says nothing about behaviour there.

## Linux verification (2026-10-29, after the user allowed Docker)

The user allowed it in chat: "Pode iniciar o Docker Desktop e baixar golang:1.27 para verificar a daemon-core no Linux." The conductor started Docker Desktop (server 27.4.0, `linux/arm64`) and pulled `golang:1.27` (`sha256:2920bb124f1a…`, Go 1.27.3, `linux/arm64`).

The command was the one below, with these changes:
- each target ran separately rather than chained with `&&`, so one failure would not hide the rest;
- each log went to a mounted output directory;
- IT-051 and UT-029 ran alone at the end.

The read-only module cache worked. Kernel `6.10.14-linuxkit aarch64`, uid 0. The test cache was empty at the start, and no package result was `(cached)` in any suite.

| Target | Result | Wall time |
|---|---|---|
| `make gate` | PASS, 14 packages ok | 54 s (cold build cache) |
| `make test-integration` | PASS, 14 ok | 38 s |
| `make test-e2e` | PASS | 8 s |
| `make test-hooks` | PASS, 14 ok | 7 s |
| `make test-integration-hooks` | PASS, 14 ok (`internal/daemon` 65.3 s) | 67 s |
| `make test-e2e-hooks` | PASS | 6 s |
| IT-051 alone (`-v`) | PASS: `Go go1.27.3: closing a UnixListener with the default settings unlinks its socket: true` | — |
| UT-029 alone (`-v`) | PASS: `fullfsync=0 checkpoint_fullfsync=0 (GOOS=linux)` on the writer and all 4 readers, as the spec expects (`fullfsync` is darwin-only) | — |

The IT-051 answer is now confirmed on Linux as well.

**Limits of the run:**
- It ran as root, so the unreadable-file cases skipped.
- It ran on arm64 only; `linux/amd64` is compile-checked only.
- There was no systemd login session, so the `KillUserProcesses` caveat is still untried.

## Linux: unverified

**Reason (recorded as decided by the conductor).** Docker's CLI is installed at `/usr/local/bin/docker` (27.4.0), but the Docker daemon is not running and no image is available locally. Starting Docker Desktop or pulling an image would be an install, which `AGENTS.md` forbids without the user's permission. Nothing was started, pulled or installed. Linux was **not** run, and nothing here claims it was.

**What a later Linux run needs** (once the user allows Docker and the `golang:1.27` pull). The repository and the host module cache are mounted read-only, so the container works on a copy; run from the repo root:

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

- The host module cache is 373 MB here. If Go cannot use a read-only module cache, mount it read-write or copy it first (not tried).
- Confirm the IT-051 answer on Linux from the `-v` line of `TestIT051_GoUnlinksTheSocketOnCloseUnlessToldNot` (run it alone with `-tags integration -run TestIT051 -v ./internal/daemon/`).
- Expect `make test-integration-hooks` to take a minute or more; IT-053 needs about 7 s of real time. The unreadable-file cases in `internal/doctor` and `e2e/tooling_test.go` skip as root in the container.
- The tests were written to be Linux-neutral (socket paths under `/tmp`, TCP probe through `/proc`, 4 KiB frames to outgrow the larger Linux socket buffers, `syscall.Getpgid`), but that is by design, not by evidence.

## README walkthrough

`README.md` (new, English): requirements, install from source, the Golden Path, a "try it without touching `~/.harness`" block, `HARNESS_HOME`, config, the errors table pointer (`docs/tasks/daemon-core/_dx.md` > Errors) with the exit-code table, platform status, the Linux commands above, the Linux `KillUserProcesses` caveat (workarounds `loginctl enable-linger` and `systemd-run --user`, from the spec, untried), and the Makefile test targets.

Walked through twice against `bin/harness`, with `HOME` faked and `HARNESS_HOME` set inline on the same command line to a directory created in that same command (`/tmp/hzw.*`, then `/tmp/harness.*` through the README's own `mktemp` line):
- `daemon start`, `status`, `events --stream daemon`, a background `events --follow` that received `daemon.stopping`, `daemon.stopped` and `stream closed: daemon stopped`, `daemon stop` (exit 0), `doctor` (6 ok on a running daemon; 5 ok + 1 warn without one), `config validate`, `config show -o json`, `version`, a second `daemon start` that continued the stream at sequence 5 and 6;
- the README's `export HARNESS_HOME="$(mktemp -d /tmp/harness.XXXXXX)"; harness daemon start && harness status && harness daemon stop; rm -rf "$HARNESS_HOME"` block ran as written;
- `events --follow -o json` exits 64; `chmod 755` on the home gives `ERROR home.permissions ... fix: chmod 700 ...`;
- the `-ldflags "-X github.com/felipmateus/harness/internal/version.Commit=..."` line in the README prints `commit abc1234`.

Differences from the `_dx.md` transcripts, none a defect: human `config validate` on a missing file prints `valid: <path> (not present, defaults in use)` (task 8's decision; `_dx.md` shows an existing file); `events` and `status` without a daemon exit 69 `daemon_unavailable`, which the README now says.

Cleanup: every temporary home and the daemons started were removed or stopped by the walkthrough itself; no `/tmp/hz*` or `/tmp/harness.*` directory remains, no `harness daemon` process of this task is running, and `~/.harness` does not exist.

## Decisions

- **README includes the Linux `docker run` commands** next to the "unverified" statement, so the user can run them without opening the memory files. They are labelled as not run.
- **Suites ran with `GOFLAGS=-count=1`** instead of editing the Makefile: the Makefile targets are the contract, and the env var bypasses the cache without changing them.

## Open items for the conductor

- **Linux: closed on 2026-10-29** (see › Linux verification). The user allowed Docker and the pull. All six suites are green on `linux/arm64`.
- **Pid 88714 (task 7's stray test daemon, home `/tmp/hz3174794282` already deleted) was still running** at the end of this task; I did not touch it. The operator ends it with `kill 88714`.
- Items carried over from tasks 5 to 8 stay open and unchanged (see `memory/MEMORY.md` > Open Risks): `subscriber degraded` logs `stream`/`seq` not `cursor`; `append rejected` has no producer yet; a boot cancelled after an unclean event records another unclean event; a crash between the `daemon.json` temp write and the rename leaves `.daemon.json.*`; Ctrl-C during `daemon start` leaves the spawned child running; doctor after a crash may rewrite SQLite's `-shm`.
- The repository still has no commits; the user commits. Nothing was committed or pushed.
