//go:build e2e

package e2e

import (
	"encoding/json"
	"fmt"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"syscall"
	"testing"
	"time"
)

func keysOf(m map[string]any) []string {
	var keys []string
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// E2E-007
func TestE2E007_JSONPageAndStatusShape(t *testing.T) {
	h := newHome(t)
	pid := start(t, h)

	res := harness(t, h, "events", "--stream", "daemon", "--after", "1", "--limit", "1", "-o", "json")
	if res.exit != 0 || res.stderr != "" {
		t.Fatalf("exit %d, stderr %q", res.exit, res.stderr)
	}
	var page struct {
		Events []struct {
			Stream string         `json:"stream"`
			Seq    int64          `json:"seq"`
			ID     string         `json:"id"`
			Type   string         `json:"type"`
			Time   string         `json:"time"`
			Data   map[string]any `json:"data"`
		} `json:"events"`
		Page struct {
			After     int64 `json:"after"`
			NextAfter int64 `json:"next_after"`
			HasMore   bool  `json:"has_more"`
			Limit     int   `json:"limit"`
			Head      int64 `json:"head"`
		} `json:"page"`
	}
	if err := json.Unmarshal([]byte(res.stdout), &page); err != nil {
		t.Fatalf("stdout %q: %v", res.stdout, err)
	}
	if len(page.Events) != 1 {
		t.Fatalf("got %d events, want exactly 1:\n%s", len(page.Events), res.stdout)
	}
	ev := page.Events[0]
	if ev.Seq != 2 || ev.Type != "daemon.started" || ev.Data["pid"] != float64(pid) || ev.Stream != "daemon" {
		t.Errorf("event %+v", ev)
	}
	if !regexp.MustCompile(`^\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d\.\d{3}Z$`).MatchString(ev.Time) {
		t.Errorf("time %q is not UTC with three fractional digits", ev.Time)
	}
	if page.Page.After != 1 || page.Page.Limit != 1 || page.Page.NextAfter != 2 {
		t.Errorf("page %+v", page.Page)
	}
	if page.Page.HasMore != (page.Page.NextAfter < page.Page.Head) {
		t.Errorf("has_more %v does not match next_after %d and head %d", page.Page.HasMore, page.Page.NextAfter, page.Page.Head)
	}

	// The status document has exactly the fields of `_dx.md`.
	st := harness(t, h, "status", "-o", "json")
	if st.exit != 0 || st.stderr != "" {
		t.Fatalf("status: exit %d, stderr %q", st.exit, st.stderr)
	}
	var doc map[string]map[string]any
	if err := json.Unmarshal([]byte(st.stdout), &doc); err != nil {
		t.Fatal(err)
	}
	status := doc["status"]
	if !reflect.DeepEqual(keysOf(status), []string{"config", "home", "ledger", "pid", "socket", "started_at", "state", "uptime_seconds", "version"}) {
		t.Errorf("status keys %v", keysOf(status))
	}
	if !reflect.DeepEqual(keysOf(status["ledger"].(map[string]any)), []string{"events", "path", "schema_version", "streams"}) {
		t.Errorf("ledger keys %v", keysOf(status["ledger"].(map[string]any)))
	}
	cfg := status["config"].(map[string]any)
	if !reflect.DeepEqual(keysOf(cfg), []string{"digest", "exists", "path"}) || cfg["exists"] != false || cfg["digest"] != nil {
		t.Errorf("config %v", cfg)
	}
	if status["state"] != "running" || status["pid"] != float64(pid) || status["home"] != h.Dir || status["socket"] != h.SocketPath() {
		t.Errorf("status %v", status)
	}
	ledger := status["ledger"].(map[string]any)
	if ledger["schema_version"] != float64(1) || ledger["streams"] != float64(1) || ledger["events"] != float64(2) {
		t.Errorf("ledger %v", ledger)
	}
}

// E2E-008, human mode.
func TestE2E008_FollowEndsWhenTheDaemonStops(t *testing.T) {
	h := newHome(t)
	start(t, h)

	f := startBackground(t, h, "events", "--follow")
	waitFor(t, "the follower to print the history", 15*time.Second, func() bool {
		return strings.Contains(f.stdout.String(), "daemon.started")
	})

	if res := harness(t, h, "daemon", "stop"); res.exit != 0 {
		t.Fatalf("stop: exit %d, stderr %q", res.exit, res.stderr)
	}
	if code := f.wait(t, 15*time.Second); code != 0 {
		t.Fatalf("follower exited %d, want 0\nstdout %s\nstderr %s", code, f.stdout.String(), f.stderr.String())
	}
	rows := strings.Split(strings.TrimSuffix(f.stdout.String(), "\n"), "\n")
	if len(rows) != 5 { // header, config.loaded, daemon.started, daemon.stopping, daemon.stopped
		t.Fatalf("got %d lines, want the header and four events:\n%s", len(rows), f.stdout.String())
	}
	if !strings.Contains(rows[3], "daemon.stopping") || !strings.Contains(rows[3], "reason=request") || !strings.Contains(rows[4], "daemon.stopped") {
		t.Errorf("the output does not end with the stopping and stopped rows:\n%s", f.stdout.String())
	}
	if f.stderr.String() != "stream closed: daemon stopped\n" {
		t.Errorf("stderr %q", f.stderr.String())
	}
}

// E2E-008, jsonl mode.
func TestE2E008_FollowJSONLEndsWithTheClosedLine(t *testing.T) {
	h := newHome(t)
	start(t, h)

	f := startBackground(t, h, "events", "--follow", "-o", "jsonl")
	waitFor(t, "the follower to print the history", 15*time.Second, func() bool {
		return strings.Contains(f.stdout.String(), "daemon.started")
	})
	harness(t, h, "daemon", "stop")
	if code := f.wait(t, 15*time.Second); code != 0 {
		t.Fatalf("follower exited %d, want 0\nstderr %s", code, f.stderr.String())
	}
	lines := strings.Split(strings.TrimSuffix(f.stdout.String(), "\n"), "\n")
	if last := lines[len(lines)-1]; last != `{"closed":{"reason":"daemon stopped"}}` {
		t.Errorf("last line %q", last)
	}
	if len(lines) != 5 { // four events and the closed line
		t.Errorf("got %d lines:\n%s", len(lines), f.stdout.String())
	}
	if f.stderr.String() != "" {
		t.Errorf("stderr %q, want empty", f.stderr.String())
	}
}

// A follow ended by Ctrl-C exits 130 and says nothing.
func TestFollowInterruptedExits130(t *testing.T) {
	h := newHome(t)
	start(t, h)

	f := startBackground(t, h, "events", "--follow", "-o", "jsonl")
	waitFor(t, "the follower to print the history", 15*time.Second, func() bool {
		return strings.Contains(f.stdout.String(), "daemon.started")
	})
	if err := f.cmd.Process.Signal(syscall.SIGINT); err != nil {
		t.Fatal(err)
	}
	if code := f.wait(t, 15*time.Second); code != 130 {
		t.Errorf("exit %d, want 130", code)
	}
	if f.stderr.String() != "" {
		t.Errorf("stderr %q, want empty", f.stderr.String())
	}
	// The daemon is untouched by a follower that leaves.
	if st := harness(t, h, "status"); st.exit != 0 {
		t.Errorf("status after the follower left: exit %d", st.exit)
	}
}

// E2E-011: invalid input is refused before any daemon is contacted.
func TestE2E011_OfflineValidation(t *testing.T) {
	h := newHome(t)
	cases := []struct {
		args []string
		want string
	}{
		{[]string{"events", "--stream", "Daemon Log"}, `error: invalid stream id "Daemon Log": must match ^[a-z][A-Za-z0-9_.:-]{0,127}$` + "\n"},
		{[]string{"events", "--limit", "5000"}, "error: limit 5000 must be between 1 and 1000\n"},
		{[]string{"events", "--after", "-1"}, "error: after -1 must be >= 0\n"},
	}
	for _, tc := range cases {
		res := harness(t, h, tc.args...)
		if res.exit != 65 || res.stdout != "" || res.stderr != tc.want {
			t.Errorf("%v: exit %d stdout %q stderr %q, want exit 65 and stderr %q", tc.args, res.exit, res.stdout, res.stderr, tc.want)
		}
	}

	res := harness(t, h, "events", "--limit", "5000", "-o", "json")
	if res.exit != 65 || res.stdout != "" || errorObject(t, res.stderr)["code"] != "invalid_limit" {
		t.Errorf("json: exit %d stdout %q stderr %q", res.exit, res.stdout, res.stderr)
	}
}

// E2E-013
func TestE2E013_UsageTextForAnUnknownFlag(t *testing.T) {
	h := newHome(t)
	res := harness(t, h, "events", "--bogus")
	if res.exit != 64 || res.stdout != "" {
		t.Fatalf("exit %d, stdout %q", res.exit, res.stdout)
	}
	if !strings.HasPrefix(res.stderr, "error: unknown flag: --bogus\n") || !strings.Contains(res.stderr, "Usage:\n  harness events [flags]") {
		t.Errorf("stderr %q", res.stderr)
	}
}

// E2E-017
func TestE2E017_HumanAndJSONLEvents(t *testing.T) {
	h := newHome(t)
	pid := start(t, h)

	res := harness(t, h, "events")
	if res.exit != 0 || res.stderr != "" {
		t.Fatalf("exit %d, stderr %q", res.exit, res.stderr)
	}
	lines := strings.Split(strings.TrimSuffix(res.stdout, "\n"), "\n")
	if len(lines) != 3 || lines[0] != "SEQ    TIME                      TYPE                              DATA" {
		t.Fatalf("human output:\n%s", res.stdout)
	}
	row1 := regexp.MustCompile(`^1 {6}\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d\.\d{3}Z  config\.loaded {21}pid=` +
		fmt.Sprint(pid) + ` path=` + regexp.QuoteMeta(h.ConfigPath()) + ` exists=false digest=null$`)
	row2 := regexp.MustCompile(`^2 {6}\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d\.\d{3}Z  daemon\.started {20}pid=` +
		fmt.Sprint(pid) + ` version=\S+ boot_ms=\d+$`)
	if !row1.MatchString(lines[1]) {
		t.Errorf("row 1 %q", lines[1])
	}
	if !row2.MatchString(lines[2]) {
		t.Errorf("row 2 %q", lines[2])
	}

	res = harness(t, h, "events", "-o", "jsonl")
	if res.exit != 0 || res.stderr != "" {
		t.Fatalf("jsonl: exit %d, stderr %q", res.exit, res.stderr)
	}
	all := jsonLines(t, res.stdout)
	last := all[len(all)-1]
	pg, ok := last["page"].(map[string]any)
	if !ok || pg["has_more"] != false || pg["head"] != float64(2) || pg["next_after"] != float64(2) || pg["limit"] != float64(100) || pg["after"] != float64(0) {
		t.Errorf("last line %v", last)
	}
	if len(all) != 3 {
		t.Errorf("got %d lines, want two events and the page line", len(all))
	}
}

// E2E-023
func TestE2E023_UsageErrorAsJSON(t *testing.T) {
	h := newHome(t)
	res := harness(t, h, "events", "--bogus", "-o", "json")
	if res.exit != 64 || res.stdout != "" {
		t.Fatalf("exit %d, stdout %q", res.exit, res.stdout)
	}
	obj := errorObject(t, res.stderr)
	if obj["code"] != "usage" || obj["message"] != "unknown flag: --bogus" || obj["hint"] != "run `harness events --help`" {
		t.Errorf("error %v", obj)
	}
}

// E2E-024
func TestE2E024_FollowWithJSONIsRejected(t *testing.T) {
	h := newHome(t)
	res := harness(t, h, "events", "--follow", "-o", "json")
	if res.exit != 64 || res.stdout != "" {
		t.Fatalf("exit %d, stdout %q", res.exit, res.stdout)
	}
	obj := errorObject(t, res.stderr)
	if obj["code"] != "usage" || obj["message"] != "--follow cannot be used with -o json; use -o jsonl" {
		t.Errorf("error %v", obj)
	}
}
