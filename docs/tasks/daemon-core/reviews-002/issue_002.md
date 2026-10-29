---
provider: manual
pr:
round: 2
round_created_at: 2026-10-29T03:32:41Z
status: resolved
file: internal/logging/logging.go
line: 55
severity: low
author: claude-code
provider_ref:
---

# Issue 002: Log record times are local with a zone offset, not fixed-width UTC

## Review Comment

`AGENTS.md` › Conventions says "Times are UTC with exactly three fractional digits", and `_spec.md` › Assumptions and Defaults (line 1046) says UTC fixed width with milliseconds "everywhere, wire and storage". The daemon log does not follow it. `logging.New` builds `slog.NewJSONHandler(out, &slog.HandlerOptions{Level: opts.Level})` (`internal/logging/logging.go:55`) with no `ReplaceAttr`, so slog writes the `time` field in local time with a zone offset and a variable number of fractional digits.

Runtime evidence. These are records of `harness.log` from a daemon built from this tree, run on a throwaway home during this review (machine zone UTC-3):

```
{"time":"2026-10-29T00:30:01.55676-03:00","level":"INFO","msg":"daemon running",...}
{"time":"2026-10-29T00:32:29.296724-03:00","level":"DEBUG","msg":"shutdown step",...}
```

Two problems follow:

- The `time` field is neither UTC nor fixed width: 5 and 6 fractional digits above, and slog drops trailing zeros.
- Log records cannot be lined up with ledger event times (`...Z`, three digits) by string comparison, which an operator or agent does when matching `daemon.stopping` to `shutdown step` records.

The same handler feeds the stderr mirror, so `daemon.out` and the foreground stderr carry the same format. No test pins the log `time` format.

Suggested fix: pass a `ReplaceAttr` that rewrites the top-level `slog.TimeKey` as `t.UTC().Format("2006-01-02T15:04:05.000Z")`. The daemon already has this layout as `timeLayout` (`internal/daemon/events.go:105`); `logging` cannot import `daemon`, so keep a local constant or share it through a leaf package the boundaries table allows. Add a unit test in `internal/logging` that decodes one record and checks the `time` field against `^\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d\.\d{3}Z$`.

## Triage

- Decision: `VALID`
- Notes:
  - **Valid.** `TestLogger_TimeIsFixedWidthUTC` (UT-082) failed before the fix with exactly the reviewer's shapes: `2026-10-29T00:30:01.55676-03:00`, `...01.5-03:00`, `...02-03:00` and `...59.12Z`, in the file and in the mirror.
  - **Root cause.** `logging.New` built `slog.NewJSONHandler` with no `ReplaceAttr`, so slog wrote its default `time`: local zone, offset, and a variable number of fractional digits without trailing zeros. AGENTS.md › Conventions and `_spec.md` › Assumptions and Defaults (Times) require UTC with exactly three fractional digits; the `_dx.md` log samples already show that form.
  - **Fix.** `internal/logging/logging.go`: a local `timeLayout` (`2006-01-02T15:04:05.000Z`, repeated because `logging` imports only `redact`) and `utcTime`, a `ReplaceAttr` that rewrites the top-level `time` attribute as `t.UTC().Format(timeLayout)`. Digits below the millisecond are cut, as in the ledger.
  - **Tests.** UT-082 decodes records in the file and in the mirror: five fixed times (a UTC-3 time, `.500`, a whole second, `.999999999` cut not rounded, an already-UTC time) plus a real-clock record, each matched against `^\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d\.\d{3}Z$` and the exact expected string. It fails without the fix. `_tests.md`: UT-082 and a matrix row.
  - **No spec amendment:** the spec and `_dx.md` already say this; the code now matches.
  - **Verified (round 3, 2026-10-29):** `logging.New` passes `ReplaceAttr: utcTime`, which rewrites only the top-level `time` attribute as `UTC().Format("2006-01-02T15:04:05.000Z")` (`internal/logging/logging.go:56,74-84`). UT-082 passed on the tree. With the `ReplaceAttr` removed (overlay mutation), it failed with the reviewer's shapes (`2026-10-29T00:30:01.55676-03:00`, `...01.5-03:00`). A real overrun record from IT-059 in this round reads `time:2026-10-29T03:58:59.137Z`. Resolved.
