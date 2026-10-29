---
status: completed
title: "Task 2: Durable event ledger"
type: backend
complexity: high
---

# Task 2: Durable event ledger

## Overview

This task builds `internal/ledger`, the durable, append-only store every later feature writes to. It guarantees:
- an event is committed and synced to stable storage (power loss included) before anyone can see it;
- sequences per stream are gap-free;
- appends are validated and idempotent;
- times never go backwards;
- a newer database is refused without being touched.

Consumers:
- task 3 (`Read`, `Page`, `Event` for `stream.Follow`);
- task 5 (`Open`, `Append`, `Last`, `Stats`, `OnCommit` wiring);
- task 8 (`Inspect`).

## Shippable Outcome

- **Outcome.** In-process callers can:
  - open `harness.db`;
  - append validated events concurrently, and observe them only after commit through `OnCommit`;
  - read paged history within the 8 MiB budget.

  The schema and the durability pragmas are final. They become immutable at the first release.
- **Verify in this task:** the assigned unit tests under `-race`, including UT-029, which asserts the pragmas actually in effect. Record whether `PRAGMA fullfsync` reads back `1` on darwin; this is an open check from the spec.
- **Integration verification:** task 5 (IT-024, IT-027, IT-035) exercises the ledger inside the daemon.

## Requirements

**Contracts:**
- `_spec.md` › Core Interfaces › `internal/ledger` (the full doc comments are normative).
- `_spec.md` › Data Models: SQLite schema v1, Schema notes, Connections, `daemon` stream event types.
- `_spec.md` › Business Rules 14–20.
- `_spec.md` › Safety Invariants 4, 5, 9, 10, 11, 15.

**Behaviour that must hold:**
- **Pragmas:** `journal_mode=WAL`, `synchronous=FULL`, `busy_timeout=5000`, `foreign_keys=ON`; on darwin also `fullfsync=ON` and `checkpoint_fullfsync=ON`.
- **Connections:** writer `_txlock=immediate` with `SetMaxOpenConns(1)`; a separate `query_only` reader pool of 4.
- **Schema-ahead:** checked on a `mode=ro` connection **before** goose runs, because goose does not refuse a newer database by itself. Return `ErrSchemaAhead{DB, Binary}` and leave the file untouched.
- **Validation in `Append`, before enqueueing:**
  - stream, type and ID patterns;
  - `json.Valid`, with nil stored as `{}`;
  - at most 1 MiB.

  The ULID is generated in `Append`, and every return path carries the ID.
- **The writer:**
  - runs under its own lifetime context, so a caller's cancellation only bounds enqueue and wait;
  - its queue holds 256 appends, and a full queue blocks while honouring ctx;
  - checks the ID first: identical content returns the stored event; different content returns `ErrIDConflict`;
  - assigns `seq = MAX(seq)+1` and `time = max(now, last)` (millisecond fixed width);
  - inserts, commits, then calls `OnCommit` in order.
- **`Read`:** queries `limit+1` rows and stops at the byte budget, while always returning at least one event.
- **`Close`:** makes later `Append` calls return `ErrClosed`, then drains the queue.

## Subtasks

- [x] 2.1 Embed the migration `00001_events.sql` (rowid table, `UNIQUE (stream, seq)`, CHECKs).
- [x] 2.2 Implement `Open`: read-only schema-ahead check, pragmas, migrations, initialisation of the last assigned time.
- [x] 2.3 Implement `Append` validation and ID generation, plus the writer goroutine with idempotency, conflict, seq, time and `OnCommit`.
- [x] 2.4 Implement `Read` with the byte budget, `Head`, `Last`, `Stats`, `Inspect`, and `Close`.
- [x] 2.5 Add test hooks internal to the package (pause the writer, hold inside a transaction) for UT-104/105.

## Implementation Details

See `_spec.md` Part II › Implementation Design › Core Interfaces (`internal/ledger`) and › Data Models.

### Relevant Files

- `internal/ledger/{ledger.go,writer.go,read.go,validate.go,time.go,inspect.go}` — new.
- `internal/ledger/migrations/00001_events.sql` — new; immutable after release.

### Dependent Files

- `internal/stream`, `internal/daemon`, `internal/doctor` consume this API (tasks 3, 5, 8).

### External References

- `~/Projetos/Pessoal/compozy/internal/store/sessiondb/session_writer.go:30-89` — the single writer goroutine.
- `~/Projetos/Pessoal/compozy/internal/store/sessiondb/session_event_query.go:77-83` — `MAX(sequence)+1` inside the transaction.
- `~/Projetos/Pessoal/compozy/internal/store/sessiondb/session_event_write.go:126-152` — `AppendEventIfAbsent` idempotency.
- `~/Projetos/Pessoal/compozy/internal/store/sqlite_config.go:65-96` and `~/Projetos/Pessoal/compozy/internal/store/write.go:19-140` — pragmas and `BEGIN IMMEDIATE`. Compozy uses `synchronous=NORMAL`; we deliberately do not.
- `~/Projetos/Pessoal/compozy/internal/store/migration_refusal.go:70-89` — explicit refusal of a database ahead of the binary.

### Related ADRs

- [ADR-001](adrs/adr-001.md) — single file, rowid layout, full durability, append-only migrations.

## Deliverables

- `internal/ledger` with schema v1 and its tests.
- A note in the task memory on whether `fullfsync` reads back as `1` on darwin.

## Tests

- [x] UT-020, UT-021, UT-022, UT-023, UT-024, UT-025, UT-026, UT-027, UT-028, UT-029 — reads, ordering, idempotency, size, schema-ahead, publish-after-commit, `Last`, pragmas.
- [x] UT-100, UT-101, UT-102, UT-103, UT-104, UT-105, UT-106, UT-107, UT-108 — validation, conflict, closed, queue and cancellation, page budget, time, `pos` order.

## Success Criteria

- `go test -race ./internal/ledger/...` is green, and `make gate` stays green.
- No test leaves a database outside its temporary directory.
