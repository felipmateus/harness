---
provider: manual
pr:
round: 1
round_created_at: 2026-10-29T03:13:03Z
status: resolved
file: internal/config/config.go
line: 142
severity: medium
author: claude-code
provider_ref:
---

# Issue 001: Empty shutdown_grace is silently accepted as the 30s default

## Review Comment

Business Rule 24 and US-011 ("a typo never silently falls back to a default") require a wrong value to make the file invalid. An empty string for `daemon.shutdown_grace` is accepted and becomes 30s.

Cause: `Duration.UnmarshalText` stores the raw text (`config.go:39-42`); `Load` copies `Default()` and clears only `raw`, so `Duration` stays 30s (`config.go:114-115`); `md.IsDefined("daemon","shutdown_grace")` is true, so nothing resets it (`config.go:128`); and `validate` checks the value only when `grace.raw != ""` (`config.go:142`). An explicitly empty value therefore skips validation and keeps the default. A whitespace value (`"  "`) is correctly rejected, which shows the empty case is a hole, not a choice.

Evidence (binary built from this tree, run on a throwaway `HARNESS_HOME` with a fake `HOME`):

```
config.toml: [daemon]\nshutdown_grace = ""
$ harness config validate -o json
{"config":{"path":"/tmp/hzrev.81UA/config.toml","valid":true,"errors":[]}}   exit=0
$ harness config show -o json
{"config":{"daemon":{"shutdown_grace":"30s"},"log":{"level":"info"}}}         exit=0
config.toml: shutdown_grace = "  "  ->  valid:false, invalid_value, exit=78
```

The daemon (boot step 4) uses the same `config.Load`, so it starts with 30s too. No UT covers an empty value (UT-001…UT-006 do not).

Suggested fix: track "defined" separately from "raw text" (for example, a `set bool` written by `UnmarshalText`, or validate whenever `md.IsDefined(...)` is true) so an empty string reaches `time.ParseDuration` and yields `invalid_value`. Add a UT case for `shutdown_grace = ""` next to UT-006.

## Triage

- Decision: `VALID`
- Notes:
  - **Valid.** Reproduced with a failing test before the fix: `shutdown_grace = ""` loaded without error, while `"  "`, `"30"`, `"soon"` and the integer `30` were already `invalid_value`. Business Rule 24 and US-011 (a typo never falls back to a default) require the file to be invalid.
  - **Root cause.** `config.Duration` used `raw != ""` as its "the file gave a value" marker, so an explicitly empty string looked like an absent key and skipped `time.ParseDuration` in `validate`; `Load` then also reset the value to the default.
  - **Fix.** `Duration` has a separate `set` flag, written by `UnmarshalText` (`internal/config/config.go`). `validate` checks the value whenever `set` is true, so an empty string reaches `time.ParseDuration` and yields `invalid_value` with the same message as the other non-durations: `daemon.shutdown_grace "" is not a duration`. `Load` no longer clears `raw` before decoding or resets the default afterwards: the default keeps `set == false`, and an absent key never calls `UnmarshalText`. The daemon (boot step 4) and `config validate` both go through `config.Load`, so both are fixed.
  - **Tests.**
    - New `TestLoad_ShutdownGraceNotADurationIsInvalid` in `internal/config/config_test.go` (documented as UT-007 in `_tests.md`): empty, blank, `"30"`, `"soon"` and the integer `30` each give exactly one `invalid_value` for `daemon.shutdown_grace`. The empty case failed before the fix ("want *config.InvalidError, got <nil>"); the others passed before and pass after, so they pin the style.
    - New subtest "an empty shutdown_grace is invalid, not the default" in `TestConfigValidate` (`internal/cli/tooling_test.go`): `config validate` exits 78 with the exact `-o json` document and the human text, in the same shape as the unknown-key and `log.level` cases.
  - **Contract files touched:** `_tests.md` (UT-007 and its matrix row).
  - Verified: (round 2, 2026-10-29) `config.Duration` now carries `set`, written by `UnmarshalText` (`internal/config/config.go:39-45`), and `validate` parses whenever `set` is true (`config.go:142`), so `shutdown_grace = ""` reaches `time.ParseDuration` and yields `invalid_value`. `go test -count=1 -run TestLoad_ShutdownGraceNotADurationIsInvalid ./internal/config/` and `go test -count=1 -run TestConfigValidate ./internal/cli/` pass. Mutation check with `go test -overlay` (tree untouched), `if grace.set` put back to `if grace.raw != ""`: UT-007 fails on the `empty` case (`want *config.InvalidError, got <nil>`) and `TestConfigValidate/an_empty_shutdown_grace_is_invalid,_not_the_default` fails. An absent key keeps the default (`set == false`, `raw "30s"`). Resolved.
