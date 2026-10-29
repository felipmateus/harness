# Task 2 memory

## Objective Snapshot

`internal/ledger`, completed 2026-10-28:
- the durable, append-only event store, with schema v1 (`migrations/00001_events.sql`);
- `Open`, `Append`, `Read`, `Head`, `Last`, `Stats`, `Inspect`, `Close`;
- the in-package tests for UT-020–UT-029 and UT-100–UT-108, plus supplementary tests.

## Open check answered: `fullfsync` on darwin

`PRAGMA fullfsync` **reads back `1`**, and so does `PRAGMA checkpoint_fullfsync`:
- on the writer connection and on each of the 4 reader connections;
- on macOS 15 (Darwin 25.6.0), arm64, Go 1.27.3, `modernc.org/sqlite` v1.61.0, with the DSN pragmas `fullfsync(ON)` and `checkpoint_fullfsync(ON)`;
- UT-029 asserts it on darwin and logs the values with `go test -v`.

`synchronous` reads `2` (FULL), `journal_mode` `wal`, `busy_timeout` `5000`, `foreign_keys` `1`.

Measured cost: about 6 ms per commit with `synchronous=FULL` and `fullfsync` (50 sequential appends). That is why UT-022 (1000 appends) takes about 8 s under `-race`. Group commit is the documented escape hatch (ADR-001) and is not needed now.

## Important Decisions

- **`ErrSchemaAhead` is both.** The spec's sentinel `ErrSchemaAhead` stays a package var. `Open` returns `*SchemaAheadError{DB, Binary int}`, which `Unwrap`s to the sentinel. Consumers use `errors.Is(err, ledger.ErrSchemaAhead)` for the check and `errors.As(err, &*SchemaAheadError)` for the numbers. Its message is the `_dx.md` text: `ledger schema 3 is newer than this binary supports (1)`.
- **`ErrInvalidID` was added.** The spec gives the ID pattern but no sentinel for it. Producer-supplied IDs are in-process only, so no wire code is involved.
- **A lone digit (`1`, `7`) is rejected as `ErrInvalidData`.** It is valid JSON, but the schema's `CHECK (length(data) BETWEEN 2 AND 1048576)` would refuse it as an opaque DB error. Every other JSON value of two bytes or more is accepted (`null`, `[]`, `"x"`, `10`, `true`). A zero-length `Data` counts as absent and is stored as `{}`.
- **Idempotency compares bytes.** `{"a":1}` and `{"a": 1}` under the same ID are an `ErrIDConflict`. An idempotent hit returns the stored event, `created=false`, and does **not** call `OnCommit` (nothing was committed).
- **Every error return is `Event{ID: id}` only** (no stream, seq, data), so an `ErrIDConflict` result can never be mistaken for the stored event. A rejected event never moves the head.
- **`Append` copies `Data`** before enqueueing, because a cancelled caller may reuse its slice while the writer still holds the request.
- **Shutdown.** `Close(ctx)` closes a `closing` channel first (it wakes appenders blocked on a full queue), then flips `closed` under the write lock and closes the queue; the writer drains it and exits. If `ctx` ends first, the lifetime context is cancelled: the transaction in flight is rolled back, queued appends fail with `ErrClosed`, the connections are closed, and `Close` returns an error wrapping `ctx.Err()`. Reads after `Close` return `ErrClosed`. `Close` is idempotent and safe to call concurrently.
- **`Read` streams rows one by one** and stops at the byte budget, so a stream of 1 MiB events is never held beyond about 9 MiB. `limit < 1` is an error. An empty result is a non-nil empty slice (JSON `[]`). `readBudget` is the in-package variant that takes the budget, used to test the at-least-one rule.
- **`Stats` JSON shape** follows `_dx.md`: `{"schema_version":1,"streams":2,"events":5}`. The status builder adds `path`.
- **`Inspection`** is `{SchemaVersion, BinaryVersion int; QuickCheck []string}` with `IntegrityOK()` (`["ok"]`) and `SchemaAhead()`. A missing file wraps `os.ErrNotExist` and creates nothing; a file that is not a database is an error. Inspect works on a live ledger (WAL).
- **`harness.db` is created 0600** by `Open` (`O_EXCL`, so an existing file and its mode are never touched). SQLite gives `-wal` and `-shm` the main file's mode, so all three are 0600 whatever the umask.
- **Test seams** are unexported fields of `Options` (`clock`, `hooks{schemaCheck, beforeDequeue, inTx}`), reachable only from inside the package, so they add no production API. `beforeDequeue` runs before the writer takes a request (so a paused writer leaves exactly 256 queued); `inTx` runs after the INSERT and before COMMIT.

## Learnings

- **The read-only schema check leaves `-wal` and `-shm` behind.** On a WAL database that was closed cleanly, a `mode=ro` connection still creates an empty `-wal` (0 bytes) and a 32 KiB `-shm`, both 0600 like the main file. The main file's bytes and mtime are untouched, which is what UT-025 and E2E-014 (SHA-256 of `harness.db`) check. Deleting those two files after the check was rejected: it could break a concurrent `doctor` reader. `immutable=1` was rejected too: it ignores the WAL and could miss a version row that is committed but not yet checkpointed.
- Goose's version is `SELECT MAX(version_id) FROM goose_db_version`; the ahead check uses the same query, and a missing table means version 0.
- `go test` caches `internal/archtest` even when a new package appears (its `go list` subprocess is not tracked). Run it with `-count=1` after adding a package.
- The `_pragma` DSN keys apply to every pooled connection, in lexicographic order with `busy_timeout` first, so reopened connections keep the durability settings.
- The goose provider (`NewProvider`) must not be `Close`d: it closes the `*sql.DB` it was given.

## Files / Surfaces

- New: `internal/ledger/{ledger.go,writer.go,read.go,validate.go,time.go,inspect.go,schema.go}` and `internal/ledger/migrations/00001_events.sql`.
- New tests: `internal/ledger/{ledger_test.go,helpers_test.go}`.
- `go.mod`, `go.sum`: added `modernc.org/sqlite` v1.61.0, `github.com/pressly/goose/v3` v3.29.0, `github.com/oklog/ulid/v2` v2.1.2.
- The ledger imports no internal package; its tests import `internal/testutil/testhome` (allowed: the architecture test checks non-test imports).

## Errors / Corrections

- None that needed rework. Three mutations (publish before commit, no time floor, idempotent hit reporting `created=true`) were applied temporarily and each made the intended tests fail; the sources were restored.

## Ready for Next Run

**Verification evidence (macOS, Go 1.27.3):**
- `make gate`: green (gofmt, `go vet ./...`, `go test -race ./...`, build).
- `go test -race -count=3 ./internal/ledger/` and `-count=1 -cpu 1`: green.
- `go test -race -count=1 ./internal/archtest/`: green; `internal/ledger` has no internal import.
- After the runs, `~/.harness` did not exist and no `/tmp/hz*` directories were left.

**For task 3 (`stream.Follow`):** `ledger.Page`, `ledger.Event` and `Reader.Read` are final. `Read` returns an empty non-nil slice and never exceeds `limit` events or 8 MiB (except for a first event that alone exceeds it, which cannot happen with the 1 MiB cap).
**For task 5:**
- `Open(ctx, Options{Path, OnCommit})`; `OnCommit` runs on the writer goroutine after COMMIT, in commit order, and must not block. Call `Close(ctx)` with the shutdown budget.
- Map `errors.Is(err, ledger.ErrSchemaAhead)` to `ledger_schema_ahead` (exit 69), reading `*SchemaAheadError` for the two numbers in the message.
- The `Appender` returns `Event{ID}` on every error, so retrying with that ID is idempotent.
**For task 8:** `ledger.Inspect(ctx, path)` is read-only and never migrates; map `QuickCheck`, `SchemaVersion`, `BinaryVersion` to `ledger.integrity` and `ledger.schema`.
