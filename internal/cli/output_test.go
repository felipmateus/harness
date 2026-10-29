package cli

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/felipmateus/harness/internal/api/contract"
)

// oneDocument fails unless s is exactly one JSON document followed by a newline.
func oneDocument(t *testing.T, label, s string) {
	t.Helper()
	if !strings.HasSuffix(s, "\n") || strings.Count(s, "\n") != 1 {
		t.Errorf("%s: want exactly one line, got %q", label, s)
		return
	}
	var v any
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		t.Errorf("%s: not JSON: %q", label, s)
	}
}

// UT-046: on success every verb with -o json writes one document on stdout and nothing on
// stderr.
func TestUT046_SuccessIsOneDocumentOnStdout(t *testing.T) {
	cases := []struct {
		name  string
		setup func(e *env)
		args  []string
	}{
		{"start already running", func(e *env) {
			e.api.statusFn = func(int) (contract.Status, error) { return runningStatus(7), nil }
		}, []string{"daemon", "start"}},
		{"start detached", func(e *env) { becomesRunning(e.api, 2, 42) }, []string{"daemon", "start"}},
		{"stop", func(e *env) {
			e.api.shutdownFn = func() (contract.ShutdownInfo, error) {
				return contract.ShutdownInfo{PID: 7, UptimeSeconds: 1, ShutdownGrace: "30s"}, nil
			}
		}, []string{"daemon", "stop"}},
		{"stop not running", func(*env) {}, []string{"daemon", "stop"}},
		{"status", func(e *env) {
			e.api.statusFn = func(int) (contract.Status, error) { return dxStatus(), nil }
		}, []string{"status"}},
		{"events", func(e *env) {
			e.api.eventsFn = func(string, int64, int) (contract.EventsPage, error) { return samplePage(), nil }
		}, []string{"events"}},
		{"events empty", func(e *env) {
			e.api.eventsFn = func(string, int64, int) (contract.EventsPage, error) { return contract.EventsPage{}, nil }
		}, []string{"events"}},
	}
	for _, tc := range cases {
		for _, mode := range []string{"json", "jsonl"} {
			e := newEnv(t)
			tc.setup(e)
			args := append(append([]string{}, tc.args...), "-o", mode)
			code := e.run(args...)
			if code != 0 {
				t.Errorf("%s -o %s: exit %d, stderr %q", tc.name, mode, code, e.err())
				continue
			}
			if e.err() != "" {
				t.Errorf("%s -o %s: stderr %q, want empty", tc.name, mode, e.err())
			}
			if tc.name == "events" && mode == "jsonl" {
				continue // one line per event, then the page line: UT-127
			}
			oneDocument(t, tc.name+" -o "+mode, e.out())
		}
	}
}

// UT-047: every error code maps to its exit code, and under -o json stdout is empty and
// stderr holds {"error":{...}}.
func TestUT047_ErrorMapping(t *testing.T) {
	cases := map[string]int{
		contract.CodeInvalidLimit:         65,
		contract.CodeInvalidRequest:       65,
		contract.CodeInvalidStreamID:      65,
		contract.CodeInvalidCursor:        65,
		contract.CodeDaemonUnavailable:    69,
		contract.CodeDaemonNotReady:       69,
		contract.CodeDaemonDraining:       69,
		contract.CodeDaemonAlreadyRunning: 69,
		contract.CodeDaemonStartFailed:    69,
		contract.CodeDaemonStartTimeout:   69,
		contract.CodeDaemonStopTimeout:    69,
		contract.CodeLedgerSchemaAhead:    69,
		contract.CodeNotFound:             69,
		contract.CodeConfigInvalid:        78,
		contract.CodeHomeInsecure:         78,
		contract.CodeSocketPathTooLong:    78,
		contract.CodeUsage:                64,
		contract.CodeInternal:             1,
		contract.CodeMethodNotAllowed:     1,
		"lock_failed":                     1, // a step code of a failed boot
	}
	for code, want := range cases {
		if got := exitCodeFor(code); got != want {
			t.Errorf("exitCodeFor(%q) = %d, want %d", code, got, want)
		}

		e := newEnv(t)
		wire := &contract.Error{Code: code, Message: "message for " + code, Hint: "hint for " + code}
		e.api.statusFn = func(int) (contract.Status, error) { return contract.Status{}, wire }
		if got := e.run("status", "-o", "json"); got != want {
			t.Errorf("status failing with %q: exit %d, want %d", code, got, want)
		}
		if e.out() != "" {
			t.Errorf("%q: stdout %q, want empty", code, e.out())
		}
		if got := e.wireError(); got.Code != code || got.Message != wire.Message || got.Hint != wire.Hint {
			t.Errorf("%q: stderr error %+v", code, got)
		}

		e.run("status", "-o", "jsonl")
		if got := e.wireError(); got.Code != code {
			t.Errorf("%q under jsonl: %+v", code, got)
		}
	}
}

// Human errors are `error:` and `hint:` lines on stderr, with stdout empty.
func TestErrorsInHumanMode(t *testing.T) {
	e := newEnv(t)
	e.api.statusFn = func(int) (contract.Status, error) {
		return contract.Status{}, &contract.Error{Code: contract.CodeDaemonNotReady, Message: "daemon is starting"}
	}
	if code := e.run("status"); code != 69 {
		t.Fatalf("exit %d", code)
	}
	if e.err() != "error: daemon is starting\n" || e.out() != "" {
		t.Errorf("stderr %q stdout %q", e.err(), e.out())
	}
}

// UT-048: an unknown flag in human mode is exit 64 with the usage text on stderr.
func TestUT048_UnknownFlagInHumanMode(t *testing.T) {
	e := newEnv(t)
	code := e.run("events", "--bogus")
	if code != 64 {
		t.Fatalf("exit %d, want 64", code)
	}
	if !strings.HasPrefix(e.err(), "error: unknown flag: --bogus\n") || !strings.Contains(e.err(), "Usage:\n  harness events [flags]") {
		t.Errorf("stderr %q lacks the error line and the usage text", e.err())
	}
	if e.out() != "" {
		t.Errorf("stdout %q, want empty", e.out())
	}
	if e.api.calls() != 0 {
		t.Error("the daemon was contacted for a usage error")
	}
}

// Usage errors that are not flags: an unknown verb, a stray argument, a bad -o.
func TestUsageErrorsOfOtherKinds(t *testing.T) {
	for _, args := range [][]string{
		{"bogus"},
		{"daemon", "bogus"},
		{"status", "extra"},
		{"daemon", "stop", "extra"},
		{"status", "-o", "xml"},
	} {
		e := newEnv(t)
		if code := e.run(args...); code != 64 {
			t.Errorf("%v: exit %d, want 64; stderr %q", args, code, e.err())
		}
		if e.out() != "" {
			t.Errorf("%v: stdout %q", args, e.out())
		}
	}
	e := newEnv(t)
	e.run("status", "-o", "xml")
	if !strings.HasPrefix(e.err(), `error: invalid output format "xml": must be one of human, json, jsonl`) {
		t.Errorf("stderr %q", e.err())
	}
}

// UT-129: with an unknown flag and -o json, stderr is exactly the usage error object.
func TestUT129_UsageErrorUnderJSON(t *testing.T) {
	for _, args := range [][]string{
		{"events", "--bogus", "-o", "json"},
		{"events", "-o", "json", "--bogus"},
		{"events", "--bogus", "--output=json"},
		{"events", "--bogus", "-ojson"},
		{"events", "--bogus", "-o", "jsonl"},
	} {
		e := newEnv(t)
		if code := e.run(args...); code != 64 {
			t.Fatalf("%v: exit %d, want 64", args, code)
		}
		want := `{"error":{"code":"usage","message":"unknown flag: --bogus","hint":"run ` + "`harness events --help`" + `"}}` + "\n"
		if e.err() != want {
			t.Errorf("%v: stderr %q, want %q", args, e.err(), want)
		}
		if e.out() != "" {
			t.Errorf("%v: stdout %q, want empty", args, e.out())
		}
	}
}

func TestOutputModeFromArgs(t *testing.T) {
	cases := []struct {
		args []string
		want outputMode
	}{
		{nil, outHuman},
		{[]string{"events"}, outHuman},
		{[]string{"-o", "json"}, outJSON},
		{[]string{"-o=jsonl"}, outJSONL},
		{[]string{"-ojson"}, outJSON},
		{[]string{"--output", "jsonl"}, outJSONL},
		{[]string{"--output=json"}, outJSON},
		{[]string{"-o", "json", "-o", "human"}, outHuman},
		{[]string{"-o", "xml"}, outHuman},
		{[]string{"-o"}, outHuman},
		{[]string{"--", "-o", "json"}, outHuman},
		{[]string{"events", "--stream", "-o"}, outHuman},
	}
	for _, tc := range cases {
		if got := outputModeFromArgs(tc.args); got != tc.want {
			t.Errorf("outputModeFromArgs(%v) = %q, want %q", tc.args, got, tc.want)
		}
	}
}

// A command's own bug is an internal error, never mistaken for a usage error.
func TestUntypedCommandErrorIsInternal(t *testing.T) {
	e := newEnv(t)
	e.app.executable = func() (string, error) { return "", errString("no executable") }
	if code := e.run("daemon", "start", "-o", "json"); code != 1 {
		t.Fatalf("exit %d, want 1; stderr %q", code, e.err())
	}
	if got := e.wireError(); got.Code != contract.CodeInternal {
		t.Errorf("error %+v", got)
	}
}

type errString string

func (e errString) Error() string { return string(e) }
