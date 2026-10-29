---
status: completed
title: "Task 3: Broker and stream.Follow"
type: backend
complexity: high
---

# Task 3: Broker and stream.Follow

## Overview

This task builds two packages:
- **`internal/broker`:** fans committed events out to subscribers without ever blocking the writer.
- **`internal/stream`:** `Follow`, the transport-agnostic follow algorithm. It subscribes first, catches up from the ledger, deduplicates by sequence, resubscribes after degradation, and never blocks forever on a stuck consumer.

Consumers:
- task 4 (the SSE adapter wraps `Follow`);
- task 5 (the daemon wires `OnCommit` → `Publish` and closes the broker at shutdown);
- later, `loop-engine` (ADR-003).

## Shippable Outcome

- **Outcome.** An in-process consumer calling `stream.Follow` on a real ledger receives every event with `seq > after` exactly once, in order. This holds:
  - across a degraded subscription;
  - until the broker closes (terminal `ErrClosed`) or its context ends;
  - even when the consumer stops reading.
- **Verify in this task:** the assigned unit tests under `-race`, plus a goroutine-leak check for UT-111.
- **Integration verification:** task 5 (IT-030–IT-035), task 7 (IT-050).

## Requirements

**Contracts:**
- `_spec.md` › Core Interfaces › `internal/broker` and `internal/stream`.
- `_spec.md` › API Endpoints › Follow algorithm (steps 1–5 are normative).
- `_spec.md` › Safety Invariants 6, 7, 8.
- `_spec.md` › Business Rule 19.

**Behaviour that must hold:**
- **Buffer:** 64 events per subscription.
- **Overflow:** degraded and removed; `Publish` never blocks.
- **After `Close`:** `Done()` is closed; `Subscribe` returns an already-closed, non-degraded subscription; `Publish` is a no-op; a second `Close` is safe.
- **Inside `Follow`:**
  - every send selects on the consumer, `ctx.Done()` and `s.Done()`;
  - the catch-up checks both between pages;
  - the catch-up stops on `!HasMore`, not on a short page.
- **`stream` declares its own `Reader` and `Subscriber` interfaces** and imports only `ledger` and `broker` types (UT-090 checks this).

## Subtasks

- [x] 3.1 Implement `internal/broker` with per-stream subscriber sets, degradation, `Done`, close and late subscribe.
- [x] 3.2 Implement `stream.Follow` with the `Item` and `ErrClosed` types.
- [x] 3.3 Add a fake `Subscriber` that degrades on demand, for the tests.

## Implementation Details

See `_spec.md` Part II › Core Interfaces (`internal/broker`, `internal/stream`) and › API Endpoints › Follow algorithm.

### Relevant Files

- `internal/broker/broker.go` — new.
- `internal/stream/follow.go` — new.

### Dependent Files

- `internal/api` (task 4) and `internal/daemon` (task 5).

### External References

- `~/Projetos/Pessoal/compozy/internal/session/session_stream_broadcast.go:16-75` — the 64-event buffer and the degraded marker.
- `~/Projetos/Pessoal/compozy/internal/api/core/session_stream_handler.go:35-70` and `~/Projetos/Pessoal/compozy/internal/api/core/session_stream.go:235-346` — subscribe-then-catch-up, dedupe, and their server-side polling fallback. We resubscribe instead.

### Related ADRs

- [ADR-003](adrs/adr-003.md) — `Follow` is a plain shared function, not a port.

## Deliverables

- `internal/broker` and `internal/stream` with their tests.

## Tests

- [x] UT-030, UT-031, UT-032, UT-033, UT-034 — broker fan-out, degradation, close, late subscribe and `Done`.
- [x] UT-060, UT-061, UT-062, UT-063, UT-064, UT-066 — follow: terminal `ErrClosed`, history plus live, `after`, degraded resubscribe, empty stream, call order.
- [x] UT-110, UT-111 — follow ends when the broker closes during catch-up, and never blocks on a stuck consumer.

## Success Criteria

- `go test -race ./internal/broker/... ./internal/stream/...` is green, with no goroutine left after each test.
- `make gate` (including the architecture test) is green.
