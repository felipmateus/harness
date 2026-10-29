package cli

import (
	"errors"
	"testing"
	"time"

	"github.com/felipmateus/harness/internal/api/contract"
)

// UT-043
func TestUT043_StopWithNoDaemonIsIdempotent(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"daemon", "stop"}, "harness daemon not running\n"},
		{[]string{"daemon", "stop", "-o", "json"}, `{"daemon":{"stopped":false,"reason":"not_running"}}` + "\n"},
		{[]string{"daemon", "stop", "-o", "jsonl"}, `{"daemon":{"stopped":false,"reason":"not_running"}}` + "\n"},
	} {
		e := newEnv(t)
		e.probe.freeAfter = 0 // the lock is free
		if code := e.run(tc.args...); code != 0 {
			t.Fatalf("%v: exit %d, stderr %q", tc.args, code, e.err())
		}
		if e.out() != tc.want || e.err() != "" {
			t.Errorf("%v: stdout %q stderr %q, want %q", tc.args, e.out(), e.err(), tc.want)
		}
	}
}

// A failing lock probe is reported, not read as "not running".
func TestStopReportsALockProbeFailure(t *testing.T) {
	e := newEnv(t)
	e.probe.err = errors.New("probe lock: input/output error")
	if code := e.run("daemon", "stop", "-o", "json"); code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
	if got := e.wireError(); got.Code != contract.CodeInternal {
		t.Errorf("error %+v", got)
	}
}

// UT-125
func TestUT125_StopWaitsUntilTheLockIsFreeAndTheSocketIsGone(t *testing.T) {
	e := newEnv(t)
	e.api.shutdownFn = func() (contract.ShutdownInfo, error) {
		return contract.ShutdownInfo{Stopping: true, PID: 7, UptimeSeconds: 72, ShutdownGrace: "1s"}, nil
	}
	e.probe.freeAfter = 4   // the lock frees on the fifth look
	e.probe.socketAfter = 8 // the socket outlives the lock
	start := e.clock.Now()

	if code := e.run("daemon", "stop"); code != 0 {
		t.Fatalf("exit %d, stderr %q", code, e.err())
	}
	if e.out() != "harness daemon stopped (pid 7, uptime 1m12s)\n" || e.err() != "" {
		t.Errorf("stdout %q stderr %q", e.out(), e.err())
	}
	if e.probe.sockCalls < 9 {
		t.Errorf("socket looked at %d times: stop returned while the socket was still there", e.probe.sockCalls)
	}
	// Four looks while the lock is held, then eight while the socket is still there.
	if got := e.clock.elapsed(start); got != 1200*time.Millisecond {
		t.Errorf("waited %v, want twelve 100 ms polls", got)
	}
}

func TestUT125_StopJSON(t *testing.T) {
	e := newEnv(t)
	e.api.shutdownFn = func() (contract.ShutdownInfo, error) {
		return contract.ShutdownInfo{Stopping: true, PID: 7, UptimeSeconds: 72, ShutdownGrace: "30s"}, nil
	}
	if code := e.run("daemon", "stop", "-o", "json"); code != 0 {
		t.Fatalf("exit %d, stderr %q", code, e.err())
	}
	if want := `{"daemon":{"stopped":true,"pid":7,"uptime_seconds":72}}` + "\n"; e.out() != want || e.err() != "" {
		t.Errorf("stdout %q stderr %q, want %q", e.out(), e.err(), want)
	}
}

// UT-125: a daemon whose lock never frees is a stop timeout after grace + 5 s.
func TestUT125_StopTimesOutAfterGracePlusFiveSeconds(t *testing.T) {
	e := newEnv(t)
	e.api.shutdownFn = func() (contract.ShutdownInfo, error) {
		return contract.ShutdownInfo{Stopping: true, PID: 7, UptimeSeconds: 72, ShutdownGrace: "1s"}, nil
	}
	e.probe.freeAfter = -1 // never
	start := e.clock.Now()

	code := e.run("daemon", "stop")
	if code != 69 {
		t.Fatalf("exit %d, want 69", code)
	}
	want := "error: daemon (pid 7) did not exit within 6s\nhint: see " + e.home.LogPath() + "\n"
	if e.err() != want || e.out() != "" {
		t.Errorf("stderr %q stdout %q, want stderr %q", e.err(), e.out(), want)
	}
	if got := e.clock.elapsed(start); got != 6*time.Second {
		t.Errorf("gave up after %v of virtual time, want 6s", got)
	}
}

func TestUT125_StopTimeoutJSON(t *testing.T) {
	e := newEnv(t)
	e.api.shutdownFn = func() (contract.ShutdownInfo, error) {
		return contract.ShutdownInfo{Stopping: true, PID: 7, ShutdownGrace: "30s"}, nil
	}
	e.probe.freeAfter = -1
	if code := e.run("daemon", "stop", "-o", "json"); code != 69 {
		t.Fatalf("exit %d, want 69", code)
	}
	got := e.wireError()
	if got.Code != contract.CodeDaemonStopTimeout || got.Message != "daemon (pid 7) did not exit within 35s" {
		t.Errorf("error %+v", got)
	}
	if e.out() != "" {
		t.Errorf("stdout %q, want empty", e.out())
	}
}

// An answer without a readable grace falls back to the default one.
func TestStopFallsBackToTheDefaultGrace(t *testing.T) {
	e := newEnv(t)
	e.api.shutdownFn = func() (contract.ShutdownInfo, error) {
		return contract.ShutdownInfo{Stopping: true, PID: 7, ShutdownGrace: "soon"}, nil
	}
	e.probe.freeAfter = -1
	e.run("daemon", "stop", "-o", "json")
	if got := e.wireError().Message; got != "daemon (pid 7) did not exit within 35s" {
		t.Errorf("message %q", got)
	}
}

// A shutdown the daemon refuses is reported as the daemon said it.
func TestStopReportsAnApiError(t *testing.T) {
	e := newEnv(t)
	e.api.shutdownFn = func() (contract.ShutdownInfo, error) {
		return contract.ShutdownInfo{}, &contract.Error{Code: contract.CodeInternal, Message: "internal error"}
	}
	if code := e.run("daemon", "stop"); code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
	if e.err() != "error: internal error\n" {
		t.Errorf("stderr %q", e.err())
	}
}

const statusDocument = `{"status":{"state":"running","pid":48211,"version":"0.1.0-dev","started_at":"2026-10-28T22:10:03.140Z","uptime_seconds":4,"home":"/Users/felipe/.harness","socket":"/Users/felipe/.harness/daemon.sock","ledger":{"path":"/Users/felipe/.harness/harness.db","schema_version":1,"streams":1,"events":2},"config":{"path":"/Users/felipe/.harness/config.toml","exists":false,"digest":null}}}`

func dxStatus() contract.Status {
	return contract.Status{
		State: "running", PID: 48211, Version: "0.1.0-dev", StartedAt: "2026-10-28T22:10:03.140Z",
		UptimeSeconds: 4, Home: "/Users/felipe/.harness", Socket: "/Users/felipe/.harness/daemon.sock",
		Ledger: contract.LedgerStatus{Path: "/Users/felipe/.harness/harness.db", SchemaVersion: 1, Streams: 1, Events: 2},
		Config: contract.ConfigStatus{Path: "/Users/felipe/.harness/config.toml"},
	}
}

// UT-044
func TestUT044_StatusRendering(t *testing.T) {
	e := newEnv(t)
	e.api.statusFn = func(int) (contract.Status, error) { return dxStatus(), nil }

	if code := e.run("status"); code != 0 {
		t.Fatalf("exit %d, stderr %q", code, e.err())
	}
	want := "State     running\n" +
		"PID       48211\n" +
		"Version   0.1.0-dev\n" +
		"Uptime    4s\n" +
		"Home      /Users/felipe/.harness\n" +
		"Ledger    /Users/felipe/.harness/harness.db (schema 1, 1 stream, 2 events)\n" +
		"Config    /Users/felipe/.harness/config.toml (not present, defaults in use)\n"
	if e.out() != want {
		t.Errorf("human output:\n%s\nwant:\n%s", e.out(), want)
	}

	for _, mode := range []string{"json", "jsonl"} {
		if code := e.run("status", "-o", mode); code != 0 {
			t.Fatalf("%s: exit %d, stderr %q", mode, code, e.err())
		}
		if e.out() != statusDocument+"\n" || e.err() != "" {
			t.Errorf("%s: stdout %q stderr %q", mode, e.out(), e.err())
		}
	}
}

func TestUT044_StatusWithALoadedConfigAndPlurals(t *testing.T) {
	e := newEnv(t)
	digest := "sha256:abc"
	st := dxStatus()
	st.State, st.UptimeSeconds = "draining", 3725
	st.Ledger.Streams, st.Ledger.Events = 2, 1
	st.Config = contract.ConfigStatus{Path: "/c.toml", Exists: true, Digest: &digest}
	e.api.statusFn = func(int) (contract.Status, error) { return st, nil }

	e.run("status")
	for _, want := range []string{"State     draining\n", "Uptime    1h2m5s\n", "(schema 1, 2 streams, 1 event)\n", "Config    /c.toml (present, sha256:abc)\n"} {
		if !contains(e.out(), want) {
			t.Errorf("output lacks %q:\n%s", want, e.out())
		}
	}
}

// UT-045: the real client against a socket nobody listens on.
func TestUT045_StatusWithNoDaemon(t *testing.T) {
	e := newEnv(t)
	e.app.newClient = func(socket string) daemonAPI { return newClient(socket) }

	code := e.run("status")
	if code != 69 {
		t.Fatalf("exit %d, want 69", code)
	}
	want := "error: no daemon is listening on " + e.home.SocketPath() + "\nhint: run `harness daemon start`\n"
	if e.err() != want || e.out() != "" {
		t.Errorf("stderr %q stdout %q, want stderr %q", e.err(), e.out(), want)
	}

	if code := e.run("status", "-o", "json"); code != 69 {
		t.Fatalf("json: exit %d, want 69", code)
	}
	got := e.wireError()
	if got.Code != contract.CodeDaemonUnavailable || got.Message != "no daemon is listening on "+e.home.SocketPath() ||
		got.Hint != "run `harness daemon start`" {
		t.Errorf("error %+v", got)
	}
	if e.out() != "" {
		t.Errorf("stdout %q, want empty", e.out())
	}
}
