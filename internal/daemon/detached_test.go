package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/felipmateus/harness/internal/api/contract"
	"github.com/felipmateus/harness/internal/home"
	"github.com/felipmateus/harness/internal/testutil/testhome"
)

// errorLines returns the lines of s that are {"error":{...}} objects, decoded, and the
// total number of non-empty lines.
func errorLines(t *testing.T, s string) (errs []contract.Error, total int, last string) {
	t.Helper()
	for _, line := range strings.Split(s, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		total++
		last = line
		var body contract.ErrorBody
		if err := json.Unmarshal([]byte(line), &body); err == nil && body.Error != nil && body.Error.Code != "" {
			errs = append(errs, *body.Error)
		}
	}
	return errs, total, last
}

// A --detached child that fails to boot writes exactly one {"error":{code,message,hint}}
// line to its stderr, and it is the last line: the CLI that spawned it reads it back.
func TestDetached_AFailedBootEndsWithOneErrorLine(t *testing.T) {
	checkLeaks(t)
	h := testhome.New(t)
	writeConfig(t, h, "[daemon]\nshutdown_grase = \"30s\"\n")
	var stderr syncBuffer

	err := Run(context.Background(), Options{Home: h, Mode: Detached, Stderr: &stderr})
	exit := exitError(t, err)

	errs, total, last := errorLines(t, stderr.String())
	if len(errs) != 1 || total != 1 {
		t.Fatalf("stderr has %d error lines among %d lines, want exactly one line: %q", len(errs), total, stderr.String())
	}
	var body contract.ErrorBody
	if err := json.Unmarshal([]byte(last), &body); err != nil || body.Error == nil {
		t.Fatalf("the last line %q is not the error line", last)
	}
	got := *body.Error
	if got.Code != contract.CodeConfigInvalid || exit.Code != 78 {
		t.Errorf("line %+v, exit %d: want config_invalid and 78", got, exit.Code)
	}
	if got.Code != exit.ErrCode || got.Message != exit.Message || got.Hint != exit.Hint {
		t.Errorf("line %+v does not carry the ExitError %+v", got, exit)
	}
	if !strings.HasSuffix(stderr.String(), "\n") || strings.Count(stderr.String(), "\n") != 1 {
		t.Errorf("stderr %q is not exactly one newline-terminated line", stderr.String())
	}
}

// The error line follows the log mirror that is still on while the boot runs, and it is the
// last thing written.
func TestDetached_TheErrorLineComesAfterTheMirroredLog(t *testing.T) {
	checkLeaks(t)
	h := testhome.New(t)
	var stderr syncBuffer
	hooks := &Hooks{AfterBootStep: func(step int) error {
		if step == 10 {
			return errors.New("listen failed")
		}
		return nil
	}}

	err := Run(context.Background(), Options{Home: h, Mode: Detached, Stderr: &stderr, Hooks: hooks})
	exit := exitError(t, err)

	errs, total, last := errorLines(t, stderr.String())
	if len(errs) != 1 || total < 2 {
		t.Fatalf("stderr %q: want mirrored log lines and one error line", stderr.String())
	}
	if !strings.Contains(last, `"error"`) || errs[0].Code != exit.ErrCode || errs[0].Code != "listen_failed" {
		t.Errorf("last line %q, error %+v", last, errs[0])
	}
	if !strings.Contains(stderr.String(), "boot step failed") {
		t.Errorf("the mirror lost the boot failure: %q", stderr.String())
	}
}

// A socket path that is too long is refused at step 1, before the logger exists.
func TestDetached_ASocketPathThatIsTooLongIsReportedBeforeTheLoggerExists(t *testing.T) {
	checkLeaks(t)
	parent := testhome.New(t)
	long := home.Home{Dir: parent.Dir + "/" + strings.Repeat("a", 60) + "/" + strings.Repeat("b", 60)}
	var stderr syncBuffer

	err := Run(context.Background(), Options{Home: long, Mode: Detached, Stderr: &stderr})
	if exit := exitError(t, err); exit.Code != 78 {
		t.Fatalf("exit %d, want 78", exit.Code)
	}
	errs, total, _ := errorLines(t, stderr.String())
	if len(errs) != 1 || total != 1 || errs[0].Code != contract.CodeSocketPathTooLong || errs[0].Hint == "" {
		t.Errorf("stderr %q: errors %+v", stderr.String(), errs)
	}
}

// Only a detached child writes the line: a foreground start reports through the CLI.
func TestDetached_AnInteractiveStartWritesNoErrorLine(t *testing.T) {
	checkLeaks(t)
	h := testhome.New(t)
	writeConfig(t, h, "bogus = 1\n")
	var stderr syncBuffer

	err := Run(context.Background(), Options{Home: h, Mode: Interactive, Stderr: &stderr})
	exitError(t, err)
	if errs, _, _ := errorLines(t, stderr.String()); len(errs) != 0 {
		t.Errorf("an interactive start wrote error lines: %q", stderr.String())
	}
}

// A boot that is cancelled and a daemon that stops cleanly are not failures: no line.
func TestDetached_NothingIsWrittenWhenTheBootIsCancelled(t *testing.T) {
	checkLeaks(t)
	h := testhome.New(t)
	var stderr syncBuffer
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := Run(ctx, Options{Home: h, Mode: Detached, Stderr: &stderr}); err != nil {
		t.Fatalf("Run = %v, want nil for a cancelled boot", err)
	}
	if errs, _, _ := errorLines(t, stderr.String()); len(errs) != 0 {
		t.Errorf("a cancelled boot wrote an error line: %q", stderr.String())
	}
}

// The final line is valid JSON on one line even when the message holds quotes and newlines.
func TestFinalErrorLineIsOneJSONLine(t *testing.T) {
	line, err := finalErrorLine(&ExitError{Code: 1, ErrCode: "x", Message: "a \"quoted\"\nmessage", Hint: "h"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(line), "\n") != 1 || !strings.HasSuffix(string(line), "\n") {
		t.Fatalf("line %q is not a single line", line)
	}
	var body contract.ErrorBody
	if err := json.Unmarshal(line, &body); err != nil || body.Error.Message != "a \"quoted\"\nmessage" {
		t.Errorf("decoded %+v: %v", body.Error, err)
	}
	// An empty hint is left out, like everywhere else on the wire.
	line, _ = finalErrorLine(&ExitError{ErrCode: "x", Message: "m"})
	if strings.Contains(string(line), "hint") {
		t.Errorf("line %q carries an empty hint", line)
	}
}
