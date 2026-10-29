//go:build integration

package daemon

import (
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/felipmateus/harness/internal/admission"
	"github.com/felipmateus/harness/internal/api/contract"
	"github.com/felipmateus/harness/internal/testutil/testhome"
	"github.com/felipmateus/harness/internal/version"
)

// IT-001: a daemon on an empty home records config.loaded then daemon.started, answers
// the API as running, and leaves a daemon.json with its PID.
func TestIT001_BootRecordsTheStreamAndServes(t *testing.T) {
	checkLeaks(t)
	h := testhome.New(t)
	r := startDaemon(t, h, nil)
	pid := os.Getpid()

	events := r.events(t, "daemon")
	if got, want := wireTypes(events), []string{"config.loaded", "daemon.started"}; !slices.Equal(got, want) {
		t.Fatalf("daemon stream = %v, want %v", got, want)
	}
	for i, e := range events {
		if e.Seq != int64(i+1) {
			t.Errorf("event %d has seq %d", i, e.Seq)
		}
		if len(e.Time) != 24 || !strings.HasSuffix(e.Time, "Z") {
			t.Errorf("event %d time %q is not UTC with three fractional digits", i, e.Time)
		}
		if got := dataOf(t, e.Data)["pid"]; got != float64(pid) {
			t.Errorf("%s carries pid %v, want %d", e.Type, got, pid)
		}
	}
	loaded := dataOf(t, events[0].Data)
	if loaded["path"] != h.ConfigPath() || loaded["exists"] != false || loaded["digest"] != nil {
		t.Errorf("config.loaded = %v, want path %s, exists false, digest null", loaded, h.ConfigPath())
	}
	started := dataOf(t, events[1].Data)
	if started["version"] != version.Get().Version {
		t.Errorf("daemon.started version = %v, want %s", started["version"], version.Get().Version)
	}
	if ms, ok := started["boot_ms"].(float64); !ok || ms < 0 {
		t.Errorf("daemon.started boot_ms = %v, want a non-negative number", started["boot_ms"])
	}

	st := r.status(t)
	if st.State != "running" || st.PID != pid || st.Home != h.Dir || st.Socket != h.SocketPath() {
		t.Errorf("status = %+v, want running pid %d home %s", st, pid, h.Dir)
	}
	if st.Ledger.Path != h.DBPath() || st.Ledger.SchemaVersion != 1 || st.Ledger.Streams != 1 || st.Ledger.Events != 2 {
		t.Errorf("status ledger = %+v, want schema 1, 1 stream, 2 events", st.Ledger)
	}
	if st.Config.Path != h.ConfigPath() || st.Config.Exists || st.Config.Digest != nil {
		t.Errorf("status config = %+v, want defaults in use", st.Config)
	}

	info, err := h.ReadInfo()
	if err != nil {
		t.Fatalf("read daemon.json: %v", err)
	}
	if info.PID != pid || info.Socket != h.SocketPath() || info.Version != version.Get().Version || info.StartedAt != st.StartedAt {
		t.Errorf("daemon.json = %+v, want pid %d, socket %s, started_at %s", info, pid, h.SocketPath(), st.StartedAt)
	}
	if got := h.LockPID(); got != pid {
		t.Errorf("lock file records pid %d, want %d", got, pid)
	}
	if lockIsFree(t, h) {
		t.Error("the lock is free while the daemon runs")
	}
}

// shutdownSnapshot is what the home looked like just before one shutdown step.
type shutdownSnapshot struct {
	gate      admission.State
	sock      bool
	info      bool
	lockFree  bool
	listening bool // only probed from step 7, when connecting cannot hold the server open
}

// IT-002: cancelling the context stops the daemon in the fixed order (Safety Invariant 13),
// removes its files before Run returns and releases the lock but not the lock file; after a
// restart the stream continues at seq 3.
func TestIT002_StopOrderAndFiles(t *testing.T) {
	checkLeaks(t)
	h := testhome.New(t)

	var mu sync.Mutex
	snaps := map[int]shutdownSnapshot{}
	var access TestAccess
	hooks := &Hooks{
		OnRunning: func(a TestAccess) { access = a },
		BeforeShutdownStep: func(step int) {
			s := shutdownSnapshot{
				gate:     access.Gate.State(),
				sock:     exists(h.SocketPath()),
				info:     exists(h.InfoPath()),
				lockFree: lockIsFree(t, h),
			}
			if step >= 7 {
				c, err := net.Dial("unix", h.SocketPath())
				if err == nil {
					c.Close()
				}
				s.listening = err == nil
			}
			mu.Lock()
			snaps[step] = s
			mu.Unlock()
		},
	}
	r := startDaemon(t, h, hooks)

	if err := r.stop(t); err != nil {
		t.Fatalf("Run returned %v after its context was cancelled, want nil", err)
	}
	if exists(h.SocketPath()) || exists(h.InfoPath()) {
		t.Errorf("socket (%v) or daemon.json (%v) still exists after Run returned", exists(h.SocketPath()), exists(h.InfoPath()))
	}
	if !lockIsFree(t, h) {
		t.Error("the lock is still held after Run returned")
	}
	if !exists(h.LockPath()) {
		t.Error("the lock file was removed; it must stay")
	}

	mu.Lock()
	defer mu.Unlock()
	for step := 1; step <= 10; step++ {
		if _, ok := snaps[step]; !ok {
			t.Fatalf("shutdown step %d never ran (steps seen: %v)", step, snaps)
		}
	}
	want := map[int]shutdownSnapshot{
		1:  {gate: admission.Running, sock: true, info: true},
		2:  {gate: admission.Draining, sock: true, info: true},
		3:  {gate: admission.Draining, sock: true, info: true},
		4:  {gate: admission.Draining, sock: true, info: true},
		5:  {gate: admission.Draining, sock: true, info: true},
		6:  {gate: admission.Draining, sock: true, info: true},
		7:  {gate: admission.Draining, sock: true, info: true}, // listener closed, socket file kept
		8:  {gate: admission.Draining, sock: true, info: true},
		9:  {gate: admission.Draining, sock: false, info: false}, // daemon.json, then the socket, are gone
		10: {gate: admission.Draining, lockFree: true},           // the lock is released at step 9
	}
	for step := 1; step <= 10; step++ {
		if snaps[step] != want[step] {
			t.Errorf("before shutdown step %d: %+v, want %+v", step, snaps[step], want[step])
		}
	}

	records := logRecords(t, h)
	var stopped map[string]any
	for _, rec := range records {
		if rec["msg"] == "daemon stopped" {
			stopped = rec
		}
	}
	if stopped == nil || stopped["reason"] != "context" || stopped["pid"] != float64(os.Getpid()) {
		t.Errorf("log record %v, want daemon stopped with reason context and the pid", stopped)
	}

	// A restart continues the stream: 3 stopping, 4 stopped, 5 config.loaded.
	r2 := startDaemon(t, h, nil)
	events := r2.events(t, "daemon")
	if got, want := wireTypes(events), []string{"config.loaded", "daemon.started", "daemon.stopping", "daemon.stopped", "config.loaded", "daemon.started"}; !slices.Equal(got, want) {
		t.Fatalf("daemon stream after restart = %v, want %v", got, want)
	}
	stopping, stoppedEv := dataOf(t, events[2].Data), dataOf(t, events[3].Data)
	if events[2].Seq != 3 || stopping["reason"] != "context" || stopping["pid"] != float64(os.Getpid()) {
		t.Errorf("seq 3 = %+v %v, want daemon.stopping with reason context", events[2], stopping)
	}
	if events[3].Seq != 4 || stoppedEv["pid"] != float64(os.Getpid()) {
		t.Errorf("seq 4 = %+v %v, want daemon.stopped with the pid", events[3], stoppedEv)
	}
	if _, ok := stoppedEv["uptime_seconds"].(float64); !ok {
		t.Errorf("daemon.stopped data = %v, want an uptime_seconds number", stoppedEv)
	}
	if events[4].Seq != 5 {
		t.Errorf("config.loaded after the restart has seq %d, want 5", events[4].Seq)
	}
}

// IT-003: a second daemon on a home that is owned returns daemon_already_running (69),
// naming the holder, and leaves the first daemon and its files alone.
func TestIT003_SecondDaemonIsRefused(t *testing.T) {
	checkLeaks(t)
	h := testhome.New(t)
	a := startDaemon(t, h, nil)
	lockBefore, err := os.Stat(h.LockPath())
	if err != nil {
		t.Fatal(err)
	}

	began := time.Now()
	err = Run(t.Context(), Options{Home: h, Stderr: &syncBuffer{}})
	elapsed := time.Since(began)

	exit := exitError(t, err)
	if exit.Code != 69 || exit.ErrCode != "daemon_already_running" {
		t.Fatalf("second Run = %+v, want code 69 daemon_already_running", exit)
	}
	if want := "daemon already running for " + h.Dir + " (pid "; !strings.Contains(exit.Message, want) {
		t.Errorf("message %q does not contain %q", exit.Message, want)
	}
	// An interactive start retries the lock before it gives up (about 100 ms).
	if elapsed < lockRetries*lockRetryEvery {
		t.Errorf("second Run gave up after %s, want it to retry for at least %s", elapsed, lockRetries*lockRetryEvery)
	}

	if st := a.status(t); st.State != "running" {
		t.Errorf("first daemon state = %q after the refused start, want running", st.State)
	}
	if !exists(h.SocketPath()) || !exists(h.InfoPath()) {
		t.Error("the refused start removed the first daemon's socket or daemon.json")
	}
	lockAfter, err := os.Stat(h.LockPath())
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(lockBefore, lockAfter) || lockAfter.Size() != lockBefore.Size() {
		t.Error("the lock file was replaced or rewritten by a process that does not own it")
	}
	if got := len(a.events(t, "daemon")); got != 2 {
		t.Errorf("the refused start added events: the daemon stream has %d, want 2", got)
	}
}

// A foreground start that finds the lock held for a moment (a CLI probing it) waits it out.
func TestIT003_ForegroundStartRetriesAMomentaryLock(t *testing.T) {
	checkLeaks(t)
	h := testhome.New(t)
	probe, err := h.AcquireLock()
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		time.Sleep(20 * time.Millisecond)
		probe.Release()
	}()

	r := startDaemon(t, h, nil)
	if st := r.status(t); st.State != "running" {
		t.Errorf("state = %q, want running after the momentary lock was released", st.State)
	}
}

// IT-004: two homes, two daemons: both running, with their own sockets and streams.
func TestIT004_TwoHomesAreIndependent(t *testing.T) {
	checkLeaks(t)
	h1, h2 := testhome.New(t), testhome.New(t)
	r1, r2 := startDaemon(t, h1, nil), startDaemon(t, h2, nil)

	for i, r := range []*daemonRun{r1, r2} {
		st := r.status(t)
		if st.State != "running" || st.Socket != r.home.SocketPath() {
			t.Errorf("daemon %d status = %+v, want running on its own socket", i+1, st)
		}
	}
	if h1.SocketPath() == h2.SocketPath() {
		t.Fatal("the two homes share a socket path")
	}

	// A write to one home's ledger is invisible in the other.
	if err := appendTest(t.Context(), r1.access.Appender, "test", 1); err != nil {
		t.Fatal(err)
	}
	if got := len(r1.events(t, "test")); got != 1 {
		t.Errorf("home 1 test stream has %d events, want 1", got)
	}
	if got := len(r2.events(t, "test")); got != 0 {
		t.Errorf("home 2 test stream has %d events, want 0", got)
	}
	for i, r := range []*daemonRun{r1, r2} {
		if got := wireTypes(r.events(t, "daemon")); !slices.Equal(got, []string{"config.loaded", "daemon.started"}) {
			t.Errorf("daemon %d stream = %v", i+1, got)
		}
	}

	if err := r1.stop(t); err != nil {
		t.Fatal(err)
	}
	if st := r2.status(t); st.State != "running" {
		t.Errorf("stopping daemon 1 changed daemon 2 to %q", st.State)
	}
}

// IT-005: the gate states over the socket. While starting, status answers and everything
// else is daemon_not_ready; while draining with a request in flight, status answers, the
// rest is daemon_draining, and shutdown is still accepted.
func TestIT005_GateStates(t *testing.T) {
	checkLeaks(t)
	h := testhome.New(t)
	hk := newHold()
	r := begin(t, h, holdBootAt(hk, 10))
	r.releaseAtCleanup(hk)
	hk.waitEntered(t)

	// Starting: the API is up (step 10) but the gate is still starting.
	if st := r.status(t); st.State != "starting" {
		t.Errorf("status while held at step 10 = %q, want starting", st.State)
	}
	for _, path := range []string{"/api/streams/daemon/events", "/api/doctor"} {
		code, body := r.do(t, http.MethodGet, path, "")
		if code != http.StatusServiceUnavailable || errorCode(body) != contract.CodeDaemonNotReady {
			t.Errorf("GET %s while starting = %d %s, want 503 daemon_not_ready", path, code, body)
		}
	}

	hk.open()
	r.waitRunning(t)

	// Draining with an in-flight request held open.
	f := openFollower(t, h.SocketPath(), "daemon", followOpts{lastEventID: "2"})
	release, err := r.access.Gate.Enter()
	if err != nil {
		t.Fatalf("Enter on a running daemon: %v", err)
	}
	released := false
	defer func() {
		if !released {
			release()
		}
	}()

	code, body := r.do(t, http.MethodPost, "/api/daemon/shutdown", "")
	if code != http.StatusAccepted {
		t.Fatalf("POST shutdown = %d %s, want 202", code, body)
	}
	eventually(t, 10*time.Second, "the daemon to report draining", func() bool {
		return r.status(t).State == "draining"
	})
	for _, path := range []string{"/api/streams/daemon/events", "/api/doctor"} {
		code, body := r.do(t, http.MethodGet, path, "")
		if code != http.StatusServiceUnavailable || errorCode(body) != contract.CodeDaemonDraining {
			t.Errorf("GET %s while draining = %d %s, want 503 daemon_draining", path, code, body)
		}
	}
	code, body = r.do(t, http.MethodPost, "/api/daemon/shutdown", "")
	var again contract.ShutdownResponse
	if code != http.StatusAccepted || json.Unmarshal(body, &again) != nil || !again.Daemon.Stopping || again.Daemon.PID != os.Getpid() {
		t.Errorf("second POST shutdown while draining = %d %s, want the same 202", code, body)
	}

	// daemon.stopping is recorded before the drain; daemon.stopped waits for it.
	fr, ok := f.next(10 * time.Second)
	if !ok || fr.Event != "daemon.stopping" {
		t.Fatalf("first frame while draining = %+v (open=%v), want daemon.stopping", fr, ok)
	}
	if fr, ok := f.next(300 * time.Millisecond); ok {
		t.Fatalf("frame %+v arrived while a request was still in flight; daemon.stopped must wait for the drain", fr)
	}

	release()
	released = true
	fr, ok = f.next(10 * time.Second)
	if !ok || fr.Event != "daemon.stopped" {
		t.Fatalf("frame after the drain = %+v (open=%v), want daemon.stopped", fr, ok)
	}
	if fr, ok = f.next(10 * time.Second); !ok || fr.Event != "shutdown" {
		t.Fatalf("last frame = %+v (open=%v), want the shutdown frame", fr, ok)
	}
	f.waitEnded(t, 10*time.Second)
	if err, ok := r.wait(10 * time.Second); !ok || err != nil {
		t.Fatalf("Run = %v (returned=%v), want nil", err, ok)
	}
}

// IT-006: there is no hot reload. A config change while the daemon runs changes nothing
// (not the reported digest, not the log level); after a restart the new file is what
// config.loaded records.
func TestIT006_NoHotReload(t *testing.T) {
	checkLeaks(t)
	h := testhome.New(t)
	r := startDaemon(t, h, nil)
	if st := r.status(t); st.Config.Exists || st.Config.Digest != nil {
		t.Fatalf("status config = %+v, want defaults in use", st.Config)
	}

	writeConfig(t, h, "[log]\nlevel = \"debug\"\n")
	time.Sleep(300 * time.Millisecond)

	if st := r.status(t); st.Config.Exists || st.Config.Digest != nil {
		t.Errorf("status config after the edit = %+v, want it unchanged", st.Config)
	}
	for _, rec := range logRecords(t, h) {
		if rec["level"] == "DEBUG" {
			t.Fatalf("a debug record %v was logged although the daemon loaded log.level=info", rec)
		}
	}
	if err := r.stop(t); err != nil {
		t.Fatal(err)
	}

	r2 := startDaemon(t, h, nil)
	events := r2.events(t, "daemon")
	first, second := dataOf(t, events[0].Data), dataOf(t, events[len(events)-2].Data)
	if first["digest"] != nil || first["exists"] != false {
		t.Errorf("first config.loaded = %v, want no file", first)
	}
	digest, _ := second["digest"].(string)
	if second["exists"] != true || !strings.HasPrefix(digest, "sha256:") {
		t.Fatalf("config.loaded after the restart = %v, want exists true and a sha256 digest", second)
	}
	if st := r2.status(t); st.Config.Digest == nil || *st.Config.Digest != digest || !st.Config.Exists {
		t.Errorf("status config after the restart = %+v, want digest %s", st.Config, digest)
	}
	debug := false
	for _, rec := range logRecords(t, h) {
		debug = debug || rec["level"] == "DEBUG"
	}
	if !debug {
		t.Error("after the restart no debug record was logged although log.level=debug")
	}
}

// IT-045: a home that others can write to is refused with home_insecure (78), and nothing
// is created inside it.
func TestIT045_InsecureHomeIsRefused(t *testing.T) {
	checkLeaks(t)
	h := testhome.New(t)
	if err := os.Chmod(h.Dir, 0o777); err != nil {
		t.Fatal(err)
	}

	err := Run(t.Context(), Options{Home: h, Stderr: &syncBuffer{}})

	exit := exitError(t, err)
	if exit.Code != 78 || exit.ErrCode != "home_insecure" || !strings.Contains(exit.Message, h.Dir) || exit.Hint != "chmod 700 "+h.Dir {
		t.Errorf("error = %+v, want 78 home_insecure naming the home, with the chmod hint", exit)
	}
	entries, rerr := os.ReadDir(h.Dir)
	if rerr != nil {
		t.Fatal(rerr)
	}
	if len(entries) != 0 {
		t.Errorf("a refused home got %d entries: %v", len(entries), entries)
	}
	if mode := statMode(t, h.Dir); mode != 0o777 {
		t.Errorf("the daemon changed the mode of the home to %04o", mode)
	}
}

// IT-046: a shutdown request while the daemon is still starting cancels the boot. The
// request is accepted with 202, the stream records the stop, the files are gone, Run returns
// nil, and the next start finds nothing unclean.
func TestIT046_ShutdownWhileStartingCancelsTheBoot(t *testing.T) {
	checkLeaks(t)
	h := testhome.New(t)
	hk := newHold()
	r := begin(t, h, holdBootAt(hk, 10))
	r.releaseAtCleanup(hk)
	hk.waitEntered(t)

	code, body := r.do(t, http.MethodPost, "/api/daemon/shutdown", `{"reason":"request"}`)
	var resp contract.ShutdownResponse
	if code != http.StatusAccepted || json.Unmarshal(body, &resp) != nil {
		t.Fatalf("POST shutdown while starting = %d %s, want 202", code, body)
	}
	if !resp.Daemon.Stopping || resp.Daemon.PID != os.Getpid() || resp.Daemon.ShutdownGrace != "30s" {
		t.Errorf("shutdown answer = %+v, want stopping, the pid and 30s", resp.Daemon)
	}

	if err, ok := r.wait(15 * time.Second); !ok || err != nil {
		t.Fatalf("Run = %v (returned=%v), want nil", err, ok)
	}
	select {
	case <-r.running:
		t.Error("the daemon reported running although its boot was cancelled")
	default:
	}
	if exists(h.SocketPath()) || exists(h.InfoPath()) {
		t.Errorf("socket (%v) or daemon.json (%v) left behind", exists(h.SocketPath()), exists(h.InfoPath()))
	}
	if !lockIsFree(t, h) {
		t.Error("the lock is still held")
	}

	r2 := startDaemon(t, h, nil)
	events := r2.events(t, "daemon")
	want := []string{"config.loaded", "daemon.stopping", "daemon.stopped", "config.loaded", "daemon.started"}
	if got := wireTypes(events); !slices.Equal(got, want) {
		t.Fatalf("daemon stream = %v, want %v (no daemon.started for the cancelled boot, no unclean event)", got, want)
	}
	if reason := dataOf(t, events[1].Data)["reason"]; reason != "request" {
		t.Errorf("daemon.stopping reason = %v, want request", reason)
	}
}

// IT-047: a boot that fails after the ledger is open records daemon.boot_failed with the
// step and code, undoes everything, and returns a non-zero error; the next start reports
// no unclean shutdown.
func TestIT047_BootFailureIsRecorded(t *testing.T) {
	checkLeaks(t)
	h := testhome.New(t)
	hooks := &Hooks{AfterBootStep: func(step int) error {
		if step == 10 {
			return errors.New("listen failed (injected)")
		}
		return nil
	}}
	r := begin(t, h, hooks)

	err, ok := r.wait(15 * time.Second)
	if !ok {
		t.Fatal("Run did not return after a failed boot")
	}
	exit := exitError(t, err)
	if exit.Code != 1 || exit.ErrCode != "listen_failed" || !strings.Contains(exit.Message, "listen failed (injected)") {
		t.Errorf("error = %+v, want code 1 listen_failed carrying the cause", exit)
	}
	if exists(h.SocketPath()) || exists(h.InfoPath()) {
		t.Errorf("socket (%v) or daemon.json (%v) left behind by a failed boot", exists(h.SocketPath()), exists(h.InfoPath()))
	}
	if !lockIsFree(t, h) {
		t.Error("the lock is still held after a failed boot")
	}
	if !hasLog(logRecords(t, h), "boot step failed") {
		t.Error("the log has no `boot step failed` record")
	}

	r2 := startDaemon(t, h, nil)
	events := r2.events(t, "daemon")
	want := []string{"config.loaded", "daemon.boot_failed", "config.loaded", "daemon.started"}
	if got := wireTypes(events); !slices.Equal(got, want) {
		t.Fatalf("daemon stream = %v, want %v (no unclean event after a recorded failure)", got, want)
	}
	failed := dataOf(t, events[1].Data)
	if failed["pid"] != float64(os.Getpid()) || failed["step"] != float64(10) || failed["code"] != "listen_failed" {
		t.Errorf("daemon.boot_failed = %v, want pid, step 10, code listen_failed", failed)
	}
}

// IT-051: the socket file stays until shutdown step 8. The listener is closed at step 6 and
// the file is still there when step 7 starts; after Run returns it is gone.
//
// The open technical check of the spec is answered by the second test: Go does unlink a
// Unix socket on Close unless SetUnlinkOnClose(false) is called, which is why the daemon
// calls it.
func TestIT051_SocketIsKeptUntilStepEight(t *testing.T) {
	checkLeaks(t)
	h := testhome.New(t)
	type probe struct {
		sockExists bool
		listening  bool
	}
	var mu sync.Mutex
	probes := map[int]probe{}
	hooks := &Hooks{BeforeShutdownStep: func(step int) {
		if step != 7 && step != 8 && step != 9 {
			return
		}
		c, err := net.Dial("unix", h.SocketPath())
		if err == nil {
			c.Close()
		}
		mu.Lock()
		probes[step] = probe{sockExists: exists(h.SocketPath()), listening: err == nil}
		mu.Unlock()
	}}
	r := startDaemon(t, h, hooks)

	if err := r.stop(t); err != nil {
		t.Fatal(err)
	}

	mu.Lock()
	defer mu.Unlock()
	if got := probes[7]; !got.sockExists || got.listening {
		t.Errorf("at step 7: %+v, want the socket file present and nobody listening", got)
	}
	if got := probes[8]; !got.sockExists || got.listening {
		t.Errorf("at step 8: %+v, want the socket file still present and nobody listening", got)
	}
	if got := probes[9]; got.sockExists {
		t.Errorf("at step 9: %+v, want the socket file removed by step 8", got)
	}
	if exists(h.SocketPath()) {
		t.Error("the socket file exists after Run returned")
	}
}

func TestIT051_GoUnlinksTheSocketOnCloseUnlessToldNot(t *testing.T) {
	dir := testhome.New(t).Dir

	listen := func(name string) (*net.UnixListener, string) {
		path := dir + "/" + name
		ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
		if err != nil {
			t.Fatal(err)
		}
		return ln, path
	}

	byDefault, defaultPath := listen("default.sock")
	if !exists(defaultPath) {
		t.Fatal("the socket file does not exist while listening")
	}
	byDefault.Close()
	defaultUnlinked := !exists(defaultPath)
	t.Logf("Go %s: closing a UnixListener with the default settings unlinks its socket: %v", goVersion(), defaultUnlinked)
	if !defaultUnlinked {
		t.Errorf("the socket file survived Close with the default settings; the spec's open check is answered differently than recorded (Go would not unlink)")
	}

	kept, keptPath := listen("kept.sock")
	kept.SetUnlinkOnClose(false)
	kept.Close()
	if !exists(keptPath) {
		t.Error("the socket file was unlinked although SetUnlinkOnClose(false) was called")
	}
}

func statMode(t *testing.T, path string) os.FileMode {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return fi.Mode().Perm()
}
