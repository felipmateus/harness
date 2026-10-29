//go:build e2e

package e2e

import (
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// jsonLines decodes every line of s as a JSON object.
func jsonLines(t *testing.T, s string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSuffix(s, "\n"), "\n") {
		if line == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("not a JSON line: %q", line)
		}
		out = append(out, m)
	}
	return out
}

// errorCode decodes the one {"error":{...}} object on stderr and returns it.
func errorObject(t *testing.T, stderr string) map[string]any {
	t.Helper()
	lines := jsonLines(t, stderr)
	if len(lines) != 1 {
		t.Fatalf("stderr %q: want one JSON object", stderr)
	}
	obj, ok := lines[0]["error"].(map[string]any)
	if !ok {
		t.Fatalf("stderr %q: no error object", stderr)
	}
	return obj
}

// E2E-001
func TestE2E001_StartThenStatus(t *testing.T) {
	h := newMissingHome(t)
	if _, err := os.Stat(h.Dir); err == nil {
		t.Fatal("the home exists before the test starts")
	}

	res := harness(t, h, "daemon", "start")
	if res.exit != 0 || res.stderr != "" {
		t.Fatalf("exit %d, stderr %q", res.exit, res.stderr)
	}
	re := regexp.MustCompile(`^harness daemon started \(pid (\d+)\)\nsocket: ` + regexp.QuoteMeta(h.SocketPath()) + `\n$`)
	m := re.FindStringSubmatch(res.stdout)
	if m == nil {
		t.Fatalf("stdout %q does not match the golden path", res.stdout)
	}

	status := harness(t, h, "status")
	if status.exit != 0 || status.stderr != "" {
		t.Fatalf("status: exit %d, stderr %q", status.exit, status.stderr)
	}
	if !strings.Contains(status.stdout, "State     running\n") || !strings.Contains(status.stdout, "PID       "+m[1]+"\n") {
		t.Errorf("status:\n%s", status.stdout)
	}
	for _, want := range []string{"Home      " + h.Dir + "\n", "(schema 1, 1 stream, 2 events)\n", "(not present, defaults in use)\n"} {
		if !strings.Contains(status.stdout, want) {
			t.Errorf("status lacks %q:\n%s", want, status.stdout)
		}
	}

	fi, err := os.Stat(h.Dir)
	if err != nil || fi.Mode().Perm() != 0o700 {
		t.Errorf("home: %v, mode %v; want 0700", err, fi.Mode().Perm())
	}
}

// E2E-002
func TestE2E002_StartIsIdempotent(t *testing.T) {
	h := newHome(t)
	pid := start(t, h)

	res := harness(t, h, "daemon", "start")
	if res.exit != 0 || res.stdout != fmt.Sprintf("harness daemon already running (pid %d)\n", pid) || res.stderr != "" {
		t.Errorf("second start: exit %d stdout %q stderr %q", res.exit, res.stdout, res.stderr)
	}

	res = harness(t, h, "daemon", "start", "-o", "json")
	want := fmt.Sprintf(`{"daemon":{"pid":%d,"state":"running","socket":%q,"already_running":true}}`+"\n", pid, h.SocketPath())
	if res.exit != 0 || res.stdout != want || res.stderr != "" {
		t.Errorf("json start: exit %d stdout %q stderr %q, want %q", res.exit, res.stdout, res.stderr, want)
	}
}

// E2E-004
func TestE2E004_SocketPathTooLong(t *testing.T) {
	base := newMissingHome(t)
	// A path whose socket path is 113 bytes, as in `_dx.md`.
	long := base.Dir + "/" + strings.Repeat("d", 113-len("/daemon.sock")-len(base.Dir)-1)
	h := base
	h.Dir = long
	if got := len(h.SocketPath()); got != 113 {
		t.Fatalf("socket path is %d bytes, want 113", got)
	}

	res := harness(t, h, "daemon", "start", "-o", "json")
	if res.exit != 78 || res.stdout != "" {
		t.Fatalf("exit %d, stdout %q", res.exit, res.stdout)
	}
	obj := errorObject(t, res.stderr)
	if obj["code"] != "socket_path_too_long" || obj["hint"] != "set HARNESS_HOME to a shorter directory" {
		t.Errorf("error %v", obj)
	}
	if _, err := os.Stat(base.Dir); err == nil {
		t.Errorf("%s was created", base.Dir)
	}
}

// E2E-005
func TestE2E005_StopThenHistory(t *testing.T) {
	h := newHome(t)
	first := start(t, h)

	res := harness(t, h, "daemon", "stop")
	if res.exit != 0 || res.stderr != "" {
		t.Fatalf("stop: exit %d, stderr %q", res.exit, res.stderr)
	}
	if !regexp.MustCompile(fmt.Sprintf(`^harness daemon stopped \(pid %d, uptime \S+\)\n$`, first)).MatchString(res.stdout) {
		t.Errorf("stop printed %q", res.stdout)
	}
	for _, f := range []string{h.SocketPath(), h.InfoPath()} {
		if _, err := os.Stat(f); err == nil {
			t.Errorf("%s still exists after the stop", f)
		}
	}
	if free, _ := h.LockFree(); !free {
		t.Error("the lock is still held after the stop")
	}

	second := start(t, h)
	if second == first {
		t.Errorf("the restart has the same pid %d", second)
	}
	res = harness(t, h, "events", "-o", "jsonl")
	events := jsonLines(t, res.stdout)
	wantTypes := []string{"config.loaded", "daemon.started", "daemon.stopping", "daemon.stopped", "config.loaded", "daemon.started"}
	if len(events) != len(wantTypes)+1 {
		t.Fatalf("got %d lines, want %d events and a page line:\n%s", len(events), len(wantTypes), res.stdout)
	}
	for i, want := range wantTypes {
		ev := events[i]
		if ev["type"] != want || ev["seq"] != float64(i+1) {
			t.Errorf("event %d: type %v seq %v, want %s and %d", i, ev["type"], ev["seq"], want, i+1)
		}
		data := ev["data"].(map[string]any)
		wantPID := float64(first)
		if i >= 4 {
			wantPID = float64(second)
		}
		if data["pid"] != wantPID {
			t.Errorf("event %d: pid %v, want %v", i+1, data["pid"], wantPID)
		}
	}
	if reason := events[2]["data"].(map[string]any)["reason"]; reason != "request" {
		t.Errorf("daemon.stopping reason %v, want request", reason)
	}
}

// E2E-006
func TestE2E006_NoDaemon(t *testing.T) {
	h := newHome(t)

	res := harness(t, h, "daemon", "stop")
	if res.exit != 0 || res.stdout != "harness daemon not running\n" || res.stderr != "" {
		t.Errorf("stop: exit %d stdout %q stderr %q", res.exit, res.stdout, res.stderr)
	}

	res = harness(t, h, "status", "-o", "json")
	if res.exit != 69 || res.stdout != "" {
		t.Fatalf("status: exit %d, stdout %q", res.exit, res.stdout)
	}
	obj := errorObject(t, res.stderr)
	if obj["code"] != "daemon_unavailable" || obj["message"] != "no daemon is listening on "+h.SocketPath() ||
		obj["hint"] != "run `harness daemon start`" {
		t.Errorf("error %v", obj)
	}
}

// E2E-015
func TestE2E015_ForegroundAndSecondForeground(t *testing.T) {
	h := newHome(t)
	first := startBackground(t, h, "daemon", "start", "--foreground")
	waitFor(t, "the foreground daemon to report running", 15*time.Second, func() bool {
		return strings.Contains(first.stderr.String(), `"msg":"daemon running"`)
	})

	second := harness(t, h, "daemon", "start", "--foreground")
	want := fmt.Sprintf("error: daemon already running for %s (pid %d)\n", h.Dir, first.cmd.Process.Pid)
	if second.exit != 69 || second.stderr != want || second.stdout != "" {
		t.Errorf("second foreground: exit %d stderr %q stdout %q, want exit 69 and stderr %q", second.exit, second.stderr, second.stdout, want)
	}

	// The foreground daemon answers like any other.
	if st := harness(t, h, "status"); !strings.Contains(st.stdout, fmt.Sprintf("PID       %d\n", first.cmd.Process.Pid)) {
		t.Errorf("status of the foreground daemon:\n%s", st.stdout)
	}

	if err := first.cmd.Process.Signal(syscall.SIGINT); err != nil {
		t.Fatal(err)
	}
	if code := first.wait(t, 15*time.Second); code != 0 {
		t.Errorf("foreground daemon exited %d after SIGINT, want 0\nstderr %s", code, first.stderr.String())
	}
	if !strings.Contains(first.stderr.String(), `"msg":"daemon stopped"`) {
		t.Errorf("no `daemon stopped` record in its stderr:\n%s", first.stderr.String())
	}
}

// E2E-016
func TestE2E016_StopJSON(t *testing.T) {
	h := newHome(t)
	pid := start(t, h)

	res := harness(t, h, "daemon", "stop", "-o", "json")
	if res.exit != 0 || res.stderr != "" {
		t.Fatalf("exit %d, stderr %q", res.exit, res.stderr)
	}
	re := regexp.MustCompile(fmt.Sprintf(`^\{"daemon":\{"stopped":true,"pid":%d,"uptime_seconds":(\d+)\}\}\n$`, pid))
	m := re.FindStringSubmatch(res.stdout)
	if m == nil {
		t.Fatalf("stdout %q", res.stdout)
	}
	if u, _ := strconv.Atoi(m[1]); u < 0 {
		t.Errorf("uptime %d", u)
	}
}
