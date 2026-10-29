//go:build integration && harness_testhooks

package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/felipmateus/harness/internal/api/contract"
	"github.com/felipmateus/harness/internal/home"
	"github.com/felipmateus/harness/internal/ledger"
	"github.com/felipmateus/harness/internal/testutil/testhome"
)

// The process and crash suite (`_tests.md` IT-020 .. IT-056): the real binary, built with
// -tags harness_testhooks, a real detached daemon, real signals.

// IT-020: the daemon is in a session of its own. Killing the process group that started it
// (a closed terminal, a killed script) leaves it running.
func TestIT020_DaemonSurvivesItsStartersProcessGroup(t *testing.T) {
	h := procHome(t)

	// The starter is a shell in its own process group: it runs `daemon start`, then lingers,
	// like a script or a terminal session that outlives the command.
	script := `"$0" daemon start >/dev/null 2>&1; exec sleep 300`
	starter := exec.Command("sh", "-c", script, hookBinary(t))
	starter.Env = procEnv(t, h, "")
	starter.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := starter.Start(); err != nil {
		t.Fatal(err)
	}
	waitStarter := make(chan struct{})
	go func() { starter.Wait(); close(waitStarter) }()
	t.Cleanup(func() { syscall.Kill(-starter.Process.Pid, syscall.SIGKILL); <-waitStarter })

	pid := waitState(t, h, "running", 30*time.Second)
	if pgid, err := syscall.Getpgid(pid); err != nil || pgid != pid {
		t.Fatalf("daemon %d has process group %d (%v): it must lead a session of its own", pid, pgid, err)
	}
	if pgid, _ := syscall.Getpgid(pid); pgid == starter.Process.Pid {
		t.Fatal("the daemon shares the starter's process group")
	}

	// A closed terminal sends SIGHUP to the foreground group, then the group goes away.
	syscall.Kill(-starter.Process.Pid, syscall.SIGHUP)
	syscall.Kill(-starter.Process.Pid, syscall.SIGKILL)
	select {
	case <-waitStarter:
	case <-time.After(10 * time.Second):
		t.Fatal("the starter's group did not die")
	}

	time.Sleep(500 * time.Millisecond)
	if state, got, ok := cliState(t, h); !ok || state != "running" || got != pid {
		t.Fatalf("after the starter's group died the daemon reports %q pid %d (ok=%v), want running pid %d", state, got, ok, pid)
	}
	if !alive(pid) {
		t.Fatal("the daemon process is gone")
	}
}

// IT-021: a daemon that dies for an unrecognised reason is reported as daemon_start_failed
// with its output, leaves no process, and keeps what daemon.out already held.
func TestIT021_UnreadableLedgerIsAStartFailure(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads a mode 000 file")
	}
	h := procHome(t)
	startDetached(t, h, "")
	if res := runCLI(t, h, "", "daemon", "stop"); res.exit != 0 {
		t.Fatalf("stop: %+v", res)
	}
	before, err := os.ReadFile(h.DaemonOutPath())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(h.DBPath(), 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(h.DBPath(), 0o600) })

	res := runCLI(t, h, "", "daemon", "start", "-o", "json")
	if res.exit != 69 || res.stdout != "" {
		t.Fatalf("exit %d, stdout %q, stderr %q; want 69", res.exit, res.stdout, res.stderr)
	}
	got := errorOf(t, res.stderr)
	if got.Code != contract.CodeDaemonStartFailed {
		t.Fatalf("error %+v, want daemon_start_failed", got)
	}
	var details contract.StartFailedDetails
	raw, _ := json.Marshal(got.Details)
	if err := json.Unmarshal(raw, &details); err != nil || details.PID <= 0 || details.ExitCode != 1 {
		t.Fatalf("details %s: %v", raw, err)
	}
	if !strings.Contains(strings.Join(details.OutputTail, "\n"), "ledger") {
		t.Errorf("the output tail does not mention the ledger: %q", details.OutputTail)
	}
	waitGone(t, details.PID, time.Second)

	after, err := os.ReadFile(h.DaemonOutPath())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(after), string(before)) || len(after) == len(before) {
		t.Errorf("daemon.out did not keep its earlier content and append the new output")
	}
}

// IT-022: five starts at the same moment make one daemon. Every command exits 0 and reports
// that same daemon; one started it and four found it. daemon.out, which the children share,
// holds whole lines only.
func TestIT022_ConcurrentStartsMakeOneDaemon(t *testing.T) {
	h := procHome(t)
	const n = 5
	type outcome struct {
		res cliResult
		doc struct {
			Daemon struct {
				PID            int  `json:"pid"`
				AlreadyRunning bool `json:"already_running"`
			} `json:"daemon"`
		}
	}
	outcomes := make([]outcome, n)
	var wg sync.WaitGroup
	for i := range outcomes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			outcomes[i].res = runCLI(t, h, "", "daemon", "start", "-o", "json")
		}()
	}
	wg.Wait()

	already, pids := 0, map[int]bool{}
	for i, o := range outcomes {
		if o.res.exit != 0 {
			t.Fatalf("start %d: exit %d, stdout %q, stderr %q", i, o.res.exit, o.res.stdout, o.res.stderr)
		}
		if err := json.Unmarshal([]byte(o.res.stdout), &outcomes[i].doc); err != nil {
			t.Fatalf("start %d printed %q: %v", i, o.res.stdout, err)
		}
		pids[outcomes[i].doc.Daemon.PID] = true
		if outcomes[i].doc.Daemon.AlreadyRunning {
			already++
		}
	}
	if len(pids) != 1 || already != n-1 {
		t.Fatalf("pids %v and %d already_running; want one daemon and %d already_running", pids, already, n-1)
	}
	wantTypes(t, cliEvents(t, h), "config.loaded", "daemon.started")

	data, err := os.ReadFile(h.DaemonOutPath())
	if err != nil {
		t.Fatal(err)
	}
	if strings.ContainsRune(string(data), 0) {
		t.Error("daemon.out holds NUL bytes")
	}
	if len(data) > 0 && data[len(data)-1] != '\n' {
		t.Error("daemon.out ends in a truncated line")
	}
	for _, line := range strings.Split(strings.TrimSuffix(string(data), "\n"), "\n") {
		var v map[string]any
		if err := json.Unmarshal([]byte(line), &v); err != nil {
			t.Errorf("daemon.out holds a line that is not whole JSON: %q", line)
		}
	}
}

// IT-023: a second signal while the daemon drains ends it at once with exit 1, and the next
// start records that the stop was not clean.
func TestIT023_SecondSignalExitsAtOnceAndTheNextStartIsUnclean(t *testing.T) {
	h := procHome(t)
	p := foreground(t, h, "hold-shutdown-step=3", "running")
	old := p.pid()

	p.signal(t, syscall.SIGINT)
	waitState(t, h, "draining", 10*time.Second)
	p.signal(t, syscall.SIGINT)
	if code := p.wait(t, 10*time.Second); code != 1 {
		t.Fatalf("exit %d, want 1\nstderr %s", code, p.stderr.String())
	}

	startDetached(t, h, "")
	events := cliEvents(t, h)
	var unclean *contract.Event
	for i := range events {
		if events[i].Type == "daemon.unclean_shutdown_detected" {
			unclean = &events[i]
		}
	}
	if unclean == nil {
		t.Fatalf("no unclean shutdown recorded: %v", wireTypes(events))
	}
	data := apiEventData(t, *unclean)
	if numberOf(data["previous_pid"]) != old || data["last_type"] != "daemon.stopping" {
		t.Errorf("unclean event %v, want previous_pid %d and last_type daemon.stopping", data, old)
	}
}

// IT-024: SIGKILL leaves the ledger, the lock and the stale files for the next start to
// deal with: it succeeds with no cleanup, keeps the old events, and records the crash.
func TestIT024_RestartAfterSIGKILL(t *testing.T) {
	h := procHome(t)
	old := startDetached(t, h, "")
	if err := syscall.Kill(old, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	waitGone(t, old, 5*time.Second)
	if free, _ := h.LockFree(); !free {
		t.Fatal("the OS did not release the lock of a killed daemon")
	}

	pid := startDetached(t, h, "")
	events := cliEvents(t, h)
	wantTypes(t, events, "config.loaded", "daemon.started", "daemon.unclean_shutdown_detected", "config.loaded", "daemon.started")
	if numberOf(apiEventData(t, events[0])["pid"]) != old || numberOf(apiEventData(t, events[1])["pid"]) != old {
		t.Error("the events from before the crash changed")
	}
	data := apiEventData(t, events[2])
	if numberOf(data["pid"]) != pid || numberOf(data["previous_pid"]) != old ||
		numberOf(data["last_seq"]) != 2 || data["last_type"] != "daemon.started" {
		t.Errorf("unclean event %v, want pid %d, previous_pid %d, last_seq 2, last_type daemon.started", data, pid, old)
	}
}

// IT-025: a shutdown that does not finish inside shutdown_grace is cut: the process exits 1
// within the budget, logs which step overran, removes its files and frees the home.
func TestIT025_ShutdownBudgetOverrun(t *testing.T) {
	h := procHome(t)
	writeShutdownGrace(t, h, "1s")
	p := foreground(t, h, "hold-shutdown-step=3", "running")
	old := p.pid()

	began := time.Now()
	p.signal(t, syscall.SIGTERM)
	if code := p.wait(t, 10*time.Second); code != 1 {
		t.Fatalf("exit %d, want 1\nstderr %s", code, p.stderr.String())
	}
	if took := time.Since(began); took > 2500*time.Millisecond {
		t.Errorf("the daemon took %v to exit with a 1s budget", took)
	}
	if !hasLog(logRecords(t, h), "shutdown budget exceeded") {
		t.Error("the log has no `shutdown budget exceeded` record")
	}
	if exists(h.InfoPath()) || exists(h.SocketPath()) || !lockIsFree(t, h) {
		t.Error("an overrun left daemon.json, the socket or the lock behind")
	}

	startDetached(t, h, "")
	var found bool
	for _, ev := range cliEvents(t, h) {
		if ev.Type != "daemon.unclean_shutdown_detected" {
			continue
		}
		found = true
		if d := apiEventData(t, ev); numberOf(d["previous_pid"]) != old || d["last_type"] != "daemon.stopping" {
			t.Errorf("unclean event %v, want previous_pid %d and last_type daemon.stopping", d, old)
		}
	}
	if !found {
		t.Error("the next start did not record the unclean shutdown")
	}
}

// IT-026: a daemon.json that names a live process which is not a daemon decides nothing:
// the start succeeds and replaces the file.
func TestIT026_StaleDaemonJSONNamingALivePIDIsReplaced(t *testing.T) {
	h := procHome(t)
	stale := home.Info{PID: os.Getpid(), Version: "0.0.0", Socket: h.SocketPath(), StartedAt: "2026-01-01T00:00:00.000Z"}
	data, _ := json.Marshal(stale)
	if err := os.WriteFile(h.InfoPath(), data, 0o600); err != nil {
		t.Fatal(err)
	}

	pid := startDetached(t, h, "")
	info, err := h.ReadInfo()
	if err != nil {
		t.Fatal(err)
	}
	if info.PID != pid || info.PID == os.Getpid() {
		t.Errorf("daemon.json names pid %d, want the new daemon %d", info.PID, pid)
	}
}

// helperAppenderEnv makes the test binary act as the appending subprocess of IT-027.
const helperAppenderEnv = "HARNESS_IT_APPENDER_HOME"

// TestHelperAppender is not a test: it is the subprocess of IT-027. It appends events to the
// ledger in a loop until it is killed.
func TestHelperAppender(t *testing.T) {
	dir := os.Getenv(helperAppenderEnv)
	if dir == "" {
		t.Skip("helper process of IT-027")
	}
	ctx := context.Background()
	l, err := ledger.Open(ctx, ledger.Options{Path: home.Home{Dir: dir}.DBPath()})
	if err != nil {
		fmt.Fprintln(os.Stderr, "open:", err)
		os.Exit(2)
	}
	for i := 1; ; i++ {
		if err := appendPadded(ctx, l, "crash", i, 2000); err != nil {
			fmt.Fprintln(os.Stderr, "append:", err)
			os.Exit(3)
		}
		if i == 1 {
			fmt.Println("ready")
		}
	}
}

// IT-027: a process killed while it appends leaves only whole events: after reopening, every
// row has complete JSON data and the seqs run 1..head with no gap.
func TestIT027_MidTransactionCrashLeavesNoPartialEvent(t *testing.T) {
	h := testhome.New(t)
	cmd := exec.Command(os.Args[0], "-test.run=^TestHelperAppender$")
	cmd.Env = append(os.Environ(), helperAppenderEnv+"="+h.Dir)
	stdout := &syncBuffer{}
	cmd.Stdout = stdout
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	waited := make(chan struct{})
	go func() { cmd.Wait(); close(waited) }()
	t.Cleanup(func() { cmd.Process.Kill(); <-waited })

	eventually(t, 30*time.Second, "the appender to start", func() bool { return strings.Contains(stdout.String(), "ready") })
	time.Sleep(400 * time.Millisecond) // let it commit a few dozen events
	if err := cmd.Process.Signal(syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	<-waited

	l, err := ledger.Open(context.Background(), ledger.Options{Path: h.DBPath()})
	if err != nil {
		t.Fatalf("reopen the ledger after the crash: %v", err)
	}
	defer l.Close(context.Background())
	head, err := l.Head(context.Background(), "crash")
	if err != nil || head < 2 {
		t.Fatalf("head %d (%v): the appender committed too little to prove anything", head, err)
	}
	var seq int64
	for seq < head {
		page, err := l.Read(context.Background(), "crash", seq, 500)
		if err != nil || len(page.Events) == 0 {
			t.Fatalf("read after %d: %v", seq, err)
		}
		for _, ev := range page.Events {
			seq++
			if ev.Seq != seq {
				t.Fatalf("event has seq %d, want %d: a gap or a duplicate", ev.Seq, seq)
			}
			var data map[string]any
			if err := json.Unmarshal(ev.Data, &data); err != nil || numberOf(data["n"]) != int(seq) {
				t.Fatalf("event %d has data that is not complete JSON: %v", seq, err)
			}
		}
	}
}

// IT-048: two stops while the daemon is held in its drain both wait for it and both report
// the same daemon once it is gone.
func TestIT048_ConcurrentStopsReportTheSameDaemon(t *testing.T) {
	h := procHome(t)
	writeShutdownGrace(t, h, "2s")
	pid := startDetached(t, h, "hold-shutdown-step=3")

	var wg sync.WaitGroup
	results := make([]cliResult, 2)
	for i := range results {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i] = runCLI(t, h, "", "daemon", "stop", "-o", "json")
		}()
	}
	wg.Wait()

	for i, res := range results {
		var doc struct {
			Daemon struct {
				Stopped bool `json:"stopped"`
				PID     int  `json:"pid"`
			} `json:"daemon"`
		}
		if res.exit != 0 || json.Unmarshal([]byte(res.stdout), &doc) != nil || !doc.Daemon.Stopped || doc.Daemon.PID != pid {
			t.Errorf("stop %d: exit %d, stdout %q, stderr %q; want stopped pid %d", i, res.exit, res.stdout, res.stderr, pid)
		}
	}
	if !lockIsFree(t, h) || exists(h.SocketPath()) {
		t.Error("the home is not free after both stops returned")
	}
}

// IT-049: a start issued while the daemon is draining says it waits, and once the previous
// daemon has left it starts a new one and reports that one, never the old.
func TestIT049_StartWhileDrainingStartsANewDaemon(t *testing.T) {
	h := procHome(t)
	writeShutdownGrace(t, h, "2s")
	old := startDetached(t, h, "hold-shutdown-step=3")

	stop := startCLI(t, h, "", "daemon", "stop")
	waitState(t, h, "draining", 10*time.Second)

	res := runCLI(t, h, "", "daemon", "start")
	if res.exit != 0 {
		t.Fatalf("start: exit %d\nstdout %s\nstderr %s", res.exit, res.stdout, res.stderr)
	}
	if want := fmt.Sprintf("waiting for the previous daemon (pid %d) to exit\n", old); res.stderr != want {
		t.Errorf("stderr %q, want %q", res.stderr, want)
	}
	pid := startedPID(t, res.stdout)
	if pid == old {
		t.Errorf("start reported the old daemon %d", old)
	}
	if state, got, ok := cliState(t, h); !ok || state != "running" || got != pid {
		t.Errorf("status %q pid %d (ok=%v), want running pid %d", state, got, ok, pid)
	}
	stop.wait(t, 30*time.Second)
}

// IT-050: a follower that stopped reading cannot hold a stop: the real `harness daemon stop`
// returns well inside the grace and the daemon exits 0. The daemon runs in this process, so
// the test can feed it events; the CLI is the real binary.
func TestIT050_StopCompletesWithAStalledFollower(t *testing.T) {
	checkLeaks(t)
	h := testhome.New(t)
	writeShutdownGrace(t, h, "30s")
	r := startDaemon(t, h, nil)

	stall := make(chan struct{}) // never opened: the client reads the headers and nothing else
	t.Cleanup(func() { close(stall) })
	openFollower(t, h.SocketPath(), "test", followOpts{stall: stall})
	for i := 1; i <= 300; i++ { // 4 KiB frames outgrow the socket buffers on macOS and Linux
		if err := appendPadded(t.Context(), r.access.Appender, "test", i, 4096); err != nil {
			t.Fatal(err)
		}
	}

	res := runCLI(t, h, "", "daemon", "stop")
	if res.exit != 0 || !strings.HasPrefix(res.stdout, "harness daemon stopped (pid ") {
		t.Fatalf("stop: exit %d, stdout %q, stderr %q", res.exit, res.stdout, res.stderr)
	}
	if res.took > 3*time.Second {
		t.Errorf("stop took %v with a stalled follower, want under 3s", res.took)
	}
	err, ok := r.wait(10 * time.Second)
	if !ok || err != nil {
		t.Errorf("Run = %v (returned: %v), want a clean exit", err, ok)
	}
}

// IT-052: a detached child with an invalid config writes exactly one error line, as the last
// line of its output, and exits 78. The output goes to an O_APPEND file, like daemon.out.
func TestIT052_FinalErrorLineOfADetachedChild(t *testing.T) {
	h := procHome(t)
	writeConfig(t, h, "[daemon]\nshutdown_grase = \"30s\"\n")
	if err := os.MkdirAll(h.LogsDir(), 0o700); err != nil { // the CLI's job before it spawns the child
		t.Fatal(err)
	}
	out, err := os.OpenFile(h.DaemonOutPath(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	cmd := exec.Command(hookBinary(t), "daemon", "start", "--foreground", "--detached", "--home", h.Dir)
	cmd.Env = procEnv(t, h, "")
	cmd.Stdout, cmd.Stderr = out, out
	err = cmd.Run()
	var exit *exec.ExitError
	if !asExit(err, &exit) || exit.ExitCode() != 78 {
		t.Fatalf("the child exited with %v, want 78", err)
	}

	data, err := os.ReadFile(h.DaemonOutPath())
	if err != nil {
		t.Fatal(err)
	}
	errs, _, last := errorLines(t, string(data))
	if len(errs) != 1 || errs[0].Code != contract.CodeConfigInvalid || errs[0].Hint == "" {
		t.Fatalf("daemon.out %q: want exactly one config_invalid error line", data)
	}
	var body contract.ErrorBody
	if json.Unmarshal([]byte(last), &body) != nil || body.Error == nil || body.Error.Code != contract.CodeConfigInvalid {
		t.Errorf("the last line is %q, not the error line", last)
	}
}

func asExit(err error, target **exec.ExitError) bool {
	ee, ok := err.(*exec.ExitError)
	if ok {
		*target = ee
	}
	return ok
}

// IT-053: a daemon that never releases the home is a stop timeout: exit 69 after the grace
// plus 5 s, naming the PID.
func TestIT053_StopTimesOutOnADaemonThatNeverExits(t *testing.T) {
	h := procHome(t)
	writeShutdownGrace(t, h, "1s")
	pid := startDetached(t, h, "block-exit")

	res := runCLI(t, h, "", "daemon", "stop", "-o", "json")
	if res.exit != 69 || res.stdout != "" {
		t.Fatalf("stop: exit %d, stdout %q, stderr %q; want 69", res.exit, res.stdout, res.stderr)
	}
	got := errorOf(t, res.stderr)
	if got.Code != contract.CodeDaemonStopTimeout || got.Message != fmt.Sprintf("daemon (pid %d) did not exit within 6s", pid) {
		t.Errorf("error %+v", got)
	}
	if res.took < 5500*time.Millisecond || res.took > 9*time.Second {
		t.Errorf("stop took %v, want about 6s", res.took)
	}
	if !alive(pid) || lockIsFree(t, h) {
		t.Error("the blocked daemon should still hold the home")
	}
	syscall.Kill(pid, syscall.SIGKILL)
	waitGone(t, pid, 5*time.Second)
}

// IT-054: a terminal that closes (SIGHUP) or a service manager (SIGTERM) stops the daemon
// the way Ctrl-C does: gracefully, with the reason `signal`, exit 0.
func TestIT054_SIGHUPAndSIGTERMStopGracefully(t *testing.T) {
	for _, sig := range []syscall.Signal{syscall.SIGHUP, syscall.SIGTERM} {
		t.Run(sig.String(), func(t *testing.T) {
			h := procHome(t)
			p := foreground(t, h, "", "running")
			p.signal(t, sig)
			if code := p.wait(t, 15*time.Second); code != 0 {
				t.Fatalf("exit %d, want 0\nstderr %s", code, p.stderr.String())
			}
			events := ledgerEvents(t, h, "daemon")
			if got := ledgerTypes(events); strings.Join(got, ",") != "config.loaded,daemon.started,daemon.stopping,daemon.stopped" {
				t.Fatalf("daemon stream %v", got)
			}
			if d := dataOf(t, events[2].Data); d["reason"] != "signal" {
				t.Errorf("daemon.stopping %v, want reason signal", d)
			}
			if exists(h.SocketPath()) || exists(h.InfoPath()) || !lockIsFree(t, h) {
				t.Error("the stop left the socket, daemon.json or the lock behind")
			}
		})
	}
}

// IT-055: SIGTERM while the boot is held after step 10 cancels it: the stream records the
// stop, the process exits 0 and no listener is left.
func TestIT055_SIGTERMDuringBoot(t *testing.T) {
	h := procHome(t)
	p := foreground(t, h, "hold-boot-step=10", "starting")

	p.signal(t, syscall.SIGTERM)
	if code := p.wait(t, 15*time.Second); code != 0 {
		t.Fatalf("exit %d, want 0\nstderr %s", code, p.stderr.String())
	}
	events := ledgerEvents(t, h, "daemon")
	if got := ledgerTypes(events); strings.Join(got, ",") != "config.loaded,daemon.stopping,daemon.stopped" {
		t.Fatalf("daemon stream %v", got)
	}
	if d := dataOf(t, events[1].Data); d["reason"] != "signal" {
		t.Errorf("daemon.stopping %v, want reason signal", d)
	}
	if conn, err := net.Dial("unix", h.SocketPath()); err == nil {
		conn.Close()
		t.Error("a listener is still on the socket")
	}
	if exists(h.SocketPath()) || exists(h.InfoPath()) || !lockIsFree(t, h) {
		t.Error("the cancelled boot left the socket, daemon.json or the lock behind")
	}
}

// IT-056: a daemon whose boot is stuck is stopped when --timeout passes: the command exits 69
// with daemon_start_timeout, and no daemon process is left.
func TestIT056_ReadinessTimeoutLeavesNoDaemon(t *testing.T) {
	h := procHome(t)
	res := runCLI(t, h, "hold-boot-step=10", "daemon", "start", "--timeout", "1s", "-o", "json")
	if res.exit != 69 || res.stdout != "" {
		t.Fatalf("exit %d, stdout %q, stderr %q; want 69", res.exit, res.stdout, res.stderr)
	}
	got := errorOf(t, res.stderr)
	if got.Code != contract.CodeDaemonStartTimeout {
		t.Fatalf("error %+v, want daemon_start_timeout", got)
	}
	var details contract.StartTimeoutDetails
	raw, _ := json.Marshal(got.Details)
	if err := json.Unmarshal(raw, &details); err != nil || details.PID <= 0 {
		t.Fatalf("details %s: %v", raw, err)
	}
	waitGone(t, details.PID, 3*time.Second)
	if exists(h.SocketPath()) || exists(h.InfoPath()) || !lockIsFree(t, h) {
		t.Error("the stopped daemon left the socket, daemon.json or the lock behind")
	}
}

// IT-057: a boot cancelled after step 7 appended daemon.unclean_shutdown_detected still ends
// its own run on the stream. After a SIGKILL, a foreground start held after step 7 gets a
// SIGTERM: the stream records the stop, and the next start reports no second unclean
// shutdown for a run that stopped on request (Business Rules 12 and 13).
func TestIT057_SIGTERMAfterTheUncleanCheckLeavesNoFalseUncleanShutdown(t *testing.T) {
	h := procHome(t)
	killed := startDetached(t, h, "")
	if err := syscall.Kill(killed, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	waitGone(t, killed, 5*time.Second)

	p := startCLI(t, h, "hold-boot-step=7", "daemon", "start", "--foreground")
	// The record is logged just before step 7 appends the event, and a stop request is only
	// noticed when the step is over, so a SIGTERM sent from here lands after the append.
	eventually(t, 30*time.Second, "the unclean shutdown to be detected", func() bool {
		data, _ := os.ReadFile(h.LogPath())
		return strings.Contains(string(data), `"unclean shutdown detected"`)
	})
	p.signal(t, syscall.SIGTERM)
	if code := p.wait(t, 15*time.Second); code != 0 {
		t.Fatalf("exit %d, want 0\nstderr %s", code, p.stderr.String())
	}
	events := ledgerEvents(t, h, daemonStream)
	if got := ledgerTypes(events); strings.Join(got, ",") !=
		"config.loaded,daemon.started,daemon.unclean_shutdown_detected,daemon.stopping,daemon.stopped" {
		t.Fatalf("daemon stream after the cancelled boot %v", got)
	}
	if d := dataOf(t, events[3].Data); d["reason"] != "signal" || numberOf(d["pid"]) != p.pid() {
		t.Errorf("daemon.stopping %v, want reason signal and pid %d", d, p.pid())
	}

	startDetached(t, h, "")
	wantTypes(t, cliEvents(t, h),
		"config.loaded", "daemon.started",
		"daemon.unclean_shutdown_detected", "daemon.stopping", "daemon.stopped",
		"config.loaded", "daemon.started")
}

// IT-058: a foreground daemon whose stderr reader has gone away (`2>&1 | head -1`, a log
// shipper that died) is not killed by SIGPIPE. The stderr mirror is a courtesy copy of
// harness.log: once nobody reads it the daemon goes on running and writing the file, and a
// SIGTERM still stops it gracefully, with exit 0, the stop on the stream and no file left.
//
// At the default level the first record the dead pipe sees is `daemon stopped`, the last
// one of the shutdown. At debug level every request writes a record, so the daemon would
// die while running, and a SIGTERM would die at `shutdown step 1`.
func TestIT058_ForegroundDaemonSurvivesItsStderrReader(t *testing.T) {
	for _, level := range []string{"info", "debug"} {
		t.Run(level, func(t *testing.T) {
			h := procHome(t)
			writeConfig(t, h, fmt.Sprintf("[log]\nlevel = %q\n", level))
			p, stderr := startCLIOnPipe(t, h, "", "daemon", "start", "--foreground")
			waitState(t, h, "running", 30*time.Second)
			stderr.Close() // the reader is gone

			if level == "debug" {
				// A status request logs a debug record, which now reaches a broken pipe.
				if state, pid, ok := cliState(t, h); !ok || state != "running" || pid != p.pid() {
					t.Fatalf("after the reader closed, status = %q pid %d ok=%v, want running pid %d (%s)",
						state, pid, ok, p.pid(), exitDescription(p))
				}
			}

			p.signal(t, syscall.SIGTERM)
			if code := p.wait(t, 15*time.Second); code != 0 {
				t.Fatalf("exit %d (%s), want 0", code, exitDescription(p))
			}
			events := ledgerEvents(t, h, daemonStream)
			if got := ledgerTypes(events); strings.Join(got, ",") != "config.loaded,daemon.started,daemon.stopping,daemon.stopped" {
				t.Errorf("daemon stream %v, want the stop recorded", got)
			}
			for _, path := range []string{h.SocketPath(), h.InfoPath(), h.DBPath() + "-wal", h.DBPath() + "-shm"} {
				if exists(path) {
					t.Errorf("%s was left behind", path)
				}
			}
			if !lockIsFree(t, h) {
				t.Error("the lock was left held")
			}
			if !hasLog(logRecords(t, h), "daemon stopped") {
				t.Error("harness.log has no `daemon stopped` record: the file log must go on without the mirror")
			}
		})
	}
}

// IT-058 (overrun): a foreground daemon whose shutdown overruns exits 1 whether or not
// anybody still reads its stderr. Run returns the *ExitError, and the CLI then writes the
// failure to stderr: with the reader gone that write fails with EPIPE, and the process dies
// of SIGPIPE (status 141 in a shell) unless SIGPIPE is still taken at that point. The
// handler therefore has to outlive Run, up to the CLI's final write: a supervisor (launchd,
// systemd, a wrapper script) reads the status, and "budget exceeded" is not "killed".
func TestIT058_OverrunExitsOneWithoutAStderrReader(t *testing.T) {
	for _, tc := range []struct {
		name       string
		readerGone bool
	}{
		{"live-reader", false},
		{"dead-reader", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := procHome(t)
			writeShutdownGrace(t, h, "1s")
			p, stderr := startCLIOnPipe(t, h, "hold-shutdown-step=3", "daemon", "start", "--foreground")
			waitState(t, h, "running", 30*time.Second)
			if tc.readerGone {
				stderr.Close()
			} else {
				go io.Copy(p.stderr, stderr) // ends when the test closes the read end
			}

			p.signal(t, syscall.SIGTERM)
			if code := p.wait(t, 15*time.Second); code != 1 {
				t.Fatalf("exit %d (%s), want 1\nstderr %s", code, exitDescription(p), p.stderr.String())
			}
			if !tc.readerGone {
				eventually(t, 5*time.Second, "the error report on stderr", func() bool {
					return strings.Contains(p.stderr.String(), "shutdown exceeded its budget")
				})
			}
			if !hasLog(logRecords(t, h), "shutdown budget exceeded") {
				t.Error("the log has no `shutdown budget exceeded` record")
			}
			if exists(h.InfoPath()) || exists(h.SocketPath()) || !lockIsFree(t, h) {
				t.Error("an overrun left daemon.json, the socket or the lock behind")
			}
		})
	}
}

// exitDescription says how a finished process ended, for a failure message: "exit status 1",
// or "signal: broken pipe" for a process the kernel killed.
func exitDescription(p *bgProc) string {
	select {
	case <-p.done:
		return p.cmd.ProcessState.String()
	default:
		return "still running"
	}
}
