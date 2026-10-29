---
status: completed
title: "Task 4: Local API, admission gate and SSE adapter"
type: backend
complexity: medium
---

# Task 4: Local API, admission gate and SSE adapter

## Overview

This task builds `internal/admission` (the `starting`/`running`/`draining` gate) and `internal/api`, the HTTP handlers the CLI and agents talk to.

The API provides:
- strict requests;
- the single error shape, with JSON 404 and 405;
- the gate exemptions for status and shutdown;
- the SSE adapter that frames `stream.Follow` with write deadlines and a keepalive timer.

Everything is tested with `httptest` and fakes. Task 5 serves it on the Unix socket.

## Shippable Outcome

- **Outcome.** An `http.Handler` built with fakes of its consumer interfaces answers every route in `_dx.md` › HTTP API with the exact JSON shapes and codes. It streams SSE frames (event, keepalive, shutdown, error) with deadlines.
- **Verify in this task:** the assigned unit tests (`httptest`, injected keepalive and write-timeout values).
- **Integration verification:** task 5 serves it over the socket (IT-005, IT-030–IT-034); task 6 replays it through the CLI.

## Requirements

**Contracts:**
- `_dx.md` › HTTP API, › SSE frames, › Errors (codes, statuses, messages).
- `_spec.md` › API Endpoints (routing, middleware order, strictness, route table, SSE adapter).
- `_spec.md` › Core Interfaces › `internal/admission`.
- `_spec.md` › Safety Invariant 14.
- `_spec.md` › Business Rules 6, 21–23.

**Behaviour that must hold:**
- **Routing.** Register each path without a method pattern and dispatch on method inside the handler (405 JSON otherwise). A catch-all `/` answers 404 JSON. A path-clean redirect never escapes as plain text.
- **Middleware**, in order: recovery, admission (status and shutdown exempt), in-flight tracking (non-stream routes only), debug log. Every wrapper implements `Unwrap`.
- **Strictness:**
  - allowed query parameters per route;
  - `DisallowUnknownFields` on bodies;
  - shutdown `reason` must be `"request"`;
  - validation via `api/contract` functions only.
- **The shutdown route:**
  - while `starting`, call the boot-cancel callback once and answer 202;
  - while `draining`, answer the same 202 without triggering again;
  - the body carries `pid`, `uptime_seconds` and `shutdown_grace`.
- **The SSE adapter:**
  - **injected dependencies:** the follow function, the keepalive interval (15 s) and the write timeout (10 s);
  - **keepalive:** the timer resets after every frame;
  - **write deadlines:** set through `http.NewResponseController`, with 1 s for the final frame;
  - **`ErrClosed`:** a `shutdown` frame;
  - **other errors:** an `event: error` frame;
  - **ordering and dedupe:** none of its own.
- **`api` declares its own interfaces** (`statusSource`, `eventReader`, `doctorRunner`, `gate`). It never imports `daemon`, `cli`, `config`, `doctor`, `broker` or `home`.

## Subtasks

- [x] 4.1 Implement `internal/admission`: forward-only transitions (`starting→draining` allowed), `Enter`/`Wait` over non-stream requests.
- [x] 4.2 Implement routing, the middleware chain with `Unwrap`, and `writeError` with redaction.
- [x] 4.3 Implement the status, doctor (via `doctorRunner`), shutdown, events-list and events-stream handlers.
- [x] 4.4 Implement the SSE adapter with injected timings and deadlines.

## Implementation Details

See `_spec.md` Part II › API Endpoints.

### Relevant Files

- `internal/admission/gate.go` — new.
- `internal/api/{server.go,routes.go,middleware.go,errors.go,status.go,doctor.go,shutdown.go,events.go,sse.go}` — new.

### Dependent Files

- `internal/daemon` (task 5) constructs the server and supplies the real implementations of the interfaces.

### External References

- `~/Projetos/Pessoal/compozy/internal/admission/gate.go:35-110` — boot and drain gate.
- `~/Projetos/Pessoal/compozy/internal/api/core/event_stream.go:127` — per-write deadline on SSE writes.
- `~/Projetos/Pessoal/compozy/internal/api/udsapi/server_start.go:72-90` — stream contexts cancelled through `RegisterOnShutdown` (task 5 wires this).

### Related ADRs

- [ADR-002](adrs/adr-002.md) — `net/http` over the socket, no router dependency.
- [ADR-003](adrs/adr-003.md) — consumer-declared interfaces; the SSE adapter owns only transport concerns.

## Deliverables

- `internal/admission` and `internal/api` with their tests.

## Tests

- [x] UT-050, UT-051, UT-052, UT-053, UT-054, UT-055, UT-056, UT-057 — gate states, events page, validation errors, panic handling, shutdown body.
- [x] UT-114, UT-115, UT-116 — unknown parameters, JSON 404/405, shutdown-route exemptions.
- [x] UT-065, UT-067, UT-068, UT-069, UT-112, UT-113, UT-117 — SSE keepalive, frames, error frame, `Last-Event-ID`, write deadline, keepalive reset, `ResponseController` through middleware.

## Success Criteria

- `go test -race ./internal/admission/... ./internal/api/...` is green.
- The architecture test confirms that `api` imports neither `doctor` nor `broker`.
