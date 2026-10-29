//go:build integration && harness_testhooks

package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/felipmateus/harness/internal/api/contract"
	"github.com/felipmateus/harness/internal/home"
	"github.com/felipmateus/harness/internal/testutil/testhome"
)

// The process suites run the real `harness` binary, built once with -tags harness_testhooks,
// on short temporary homes. HARNESS_HOME is always set and HOME is an empty temporary
// directory, so nothing here can reach the operator's real ~/.harness. Every home is stopped
// when its test ends, and a daemon that will not stop is killed and fails the test.

var (
	binOnce     sync.Once
	binPath     string
	fakeHomeDir string
	binErr      error
)

// hookBinary builds the hook-enabled binary on first use.
func hookBinary(t testing.TB) string {
	t.Helper()
	binOnce.Do(func() {
		dir, err := os.MkdirTemp("", "harness-it-bin")
		if err != nil {
			binErr = err
			return
		}
		registerCleanup(func() { os.RemoveAll(dir) })
		fakeHomeDir, err = os.MkdirTemp("", "harness-it-home")
		if err != nil {
			binErr = err
			return
		}
		registerCleanup(func() { os.RemoveAll(fakeHomeDir) })
		out := filepath.Join(dir, "harness")
		build := exec.Command("go", "build", "-tags", "harness_testhooks", "-o", out, "./cmd/harness")
		build.Dir = "../.." // the module root: go test runs in the package directory
		if b, err := build.CombinedOutput(); err != nil {
			binErr = fmt.Errorf("%v\n%s", err, b)
			return
		}
		binPath = out
	})
	if binErr != nil {
		t.Fatalf("build the hook-enabled harness binary: %v", binErr)
	}
	return binPath
}

// procEnv is the environment of a harness process: the home, a fake $HOME, and the hooks.
func procEnv(t testing.TB, h home.Home, hooks string) []string {
	t.Helper()
	if h.Dir == "" {
		t.Fatal("refusing to run harness with an empty home")
	}
	hookBinary(t)
	var env []string
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "HARNESS_") && !strings.HasPrefix(kv, "HOME=") {
			env = append(env, kv)
		}
	}
	env = append(env, "HARNESS_HOME="+h.Dir, "HOME="+fakeHomeDir)
	if hooks != "" {
		env = append(env, "HARNESS_TESTHOOKS="+hooks)
	}
	return env
}

// cliResult is one finished invocation.
type cliResult struct {
	stdout, stderr string
	exit           int
	took           time.Duration
}

// runCLI runs harness to completion (90 s limit). hooks is the HARNESS_TESTHOOKS value, which
// a detached daemon spawned by the command inherits.
func runCLI(t testing.TB, h home.Home, hooks string, args ...string) cliResult {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, hookBinary(t), args...)
	cmd.Env = procEnv(t, h, hooks)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	began := time.Now()
	err := cmd.Run()
	res := cliResult{stdout: stdout.String(), stderr: stderr.String(), took: time.Since(began)}
	var exitErr *exec.ExitError
	switch {
	case err == nil:
	case errors.As(err, &exitErr):
		res.exit = exitErr.ExitCode()
	default:
		t.Fatalf("harness %v: %v", args, err)
	}
	return res
}

// bgProc is a harness process that runs while the test does other things.
type bgProc struct {
	cmd            *exec.Cmd
	stdout, stderr *syncBuffer
	done           chan struct{}
	exit           int
	began          time.Time
}

// startCLI starts harness without waiting for it. It is killed when the test ends if it is
// still running.
func startCLI(t testing.TB, h home.Home, hooks string, args ...string) *bgProc {
	t.Helper()
	cmd := exec.Command(hookBinary(t), args...)
	cmd.Env = procEnv(t, h, hooks)
	p := &bgProc{cmd: cmd, stdout: &syncBuffer{}, stderr: &syncBuffer{}, done: make(chan struct{}), began: time.Now()}
	cmd.Stdout, cmd.Stderr = p.stdout, p.stderr
	p.start(t)
	return p
}

// startCLIOnPipe starts harness with its stderr on a pipe the test owns, and returns the
// read end. Closing it leaves the process writing to a pipe nobody reads, the way
// `harness daemon start --foreground 2>&1 | head -1` does once head has exited. Nothing
// reads the pipe, so p.stderr stays empty and the log records pile up in the pipe's buffer
// (16 KiB on macOS) until the test closes it.
func startCLIOnPipe(t testing.TB, h home.Home, hooks string, args ...string) (*bgProc, *os.File) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() })
	cmd := exec.Command(hookBinary(t), args...)
	cmd.Env = procEnv(t, h, hooks)
	p := &bgProc{cmd: cmd, stdout: &syncBuffer{}, stderr: &syncBuffer{}, done: make(chan struct{}), began: time.Now()}
	cmd.Stdout, cmd.Stderr = p.stdout, w // an *os.File is handed to the child as it is
	p.start(t)
	w.Close() // the child holds the only write end now
	return p, r
}

// start runs p.cmd, records its exit, and kills it when the test ends if it is still running.
func (p *bgProc) start(t testing.TB) {
	t.Helper()
	cmd := p.cmd
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() {
		err := cmd.Wait()
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			p.exit = exitErr.ExitCode()
		}
		close(p.done)
	}()
	t.Cleanup(func() {
		select {
		case <-p.done:
		default:
			cmd.Process.Kill()
			<-p.done
		}
	})
}

func (p *bgProc) pid() int { return p.cmd.Process.Pid }

func (p *bgProc) signal(t testing.TB, sig syscall.Signal) {
	t.Helper()
	if err := p.cmd.Process.Signal(sig); err != nil {
		t.Fatalf("signal %v: %v", sig, err)
	}
}

// wait returns the exit code once the process ends, and fails the test after d.
func (p *bgProc) wait(t testing.TB, d time.Duration) int {
	t.Helper()
	select {
	case <-p.done:
		return p.exit
	case <-time.After(d):
		t.Fatalf("process did not exit within %v\nstdout %s\nstderr %s", d, p.stdout.String(), p.stderr.String())
		return -1
	}
}

func (p *bgProc) exited() bool {
	select {
	case <-p.done:
		return true
	default:
		return false
	}
}

// procHome returns an existing private home whose daemon is stopped when the test ends, and
// killed (failing the test) when it does not stop.
func procHome(t *testing.T) home.Home {
	t.Helper()
	h := testhome.New(t)
	t.Cleanup(func() { reapHome(t, h) })
	return h
}

func reapHome(t *testing.T, h home.Home) {
	t.Helper()
	if _, err := os.Stat(h.Dir); err != nil {
		return
	}
	runCLI(t, h, "", "daemon", "stop")
	if free, _ := h.LockFree(); free {
		return
	}
	if pid := h.LockPID(); pid > 0 {
		syscall.Kill(pid, syscall.SIGKILL)
		t.Errorf("a daemon (pid %d) was left running for %s and was killed", pid, h.Dir)
		waitGone(t, pid, 5*time.Second)
	}
}

// alive reports whether a process with pid exists.
func alive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// waitGone waits until no process has pid; it fails the test after d.
func waitGone(t testing.TB, pid int, d time.Duration) {
	t.Helper()
	deadline := time.Now().Add(d)
	for alive(pid) {
		if time.Now().After(deadline) {
			t.Fatalf("process %d is still alive %v later", pid, d)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// startedPID extracts N from `harness daemon started (pid N)`.
func startedPID(t testing.TB, out string) int {
	t.Helper()
	line, _, _ := strings.Cut(out, "\n")
	rest, ok := strings.CutPrefix(line, "harness daemon started (pid ")
	var pid int
	if _, err := fmt.Sscanf(strings.TrimSuffix(rest, ")"), "%d", &pid); !ok || err != nil {
		t.Fatalf("daemon start printed %q", out)
	}
	return pid
}

// startDetached runs `harness daemon start` and returns the PID it reports.
func startDetached(t *testing.T, h home.Home, hooks string) int {
	t.Helper()
	res := runCLI(t, h, hooks, "daemon", "start")
	if res.exit != 0 {
		t.Fatalf("daemon start: exit %d\nstdout %s\nstderr %s", res.exit, res.stdout, res.stderr)
	}
	return startedPID(t, res.stdout)
}

// cliState returns the state and PID `harness status` reports, or ok=false when it fails.
func cliState(t testing.TB, h home.Home) (state string, pid int, ok bool) {
	t.Helper()
	res := runCLI(t, h, "", "status", "-o", "json")
	if res.exit != 0 {
		return "", 0, false
	}
	var doc contract.StatusResponse
	if err := json.Unmarshal([]byte(res.stdout), &doc); err != nil {
		t.Fatalf("status output %q: %v", res.stdout, err)
	}
	return doc.Status.State, doc.Status.PID, true
}

// waitState waits until `harness status` reports state, and returns the PID it reports.
func waitState(t testing.TB, h home.Home, state string, d time.Duration) int {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if got, pid, ok := cliState(t, h); ok && got == state {
			return pid
		}
		time.Sleep(30 * time.Millisecond)
	}
	t.Fatalf("the daemon did not report %q within %v", state, d)
	return 0
}

// cliEvents reads the daemon stream of a running daemon through `harness events`.
func cliEvents(t testing.TB, h home.Home) []contract.Event {
	t.Helper()
	res := runCLI(t, h, "", "events", "--stream", "daemon", "-o", "json")
	if res.exit != 0 {
		t.Fatalf("events: exit %d, stderr %s", res.exit, res.stderr)
	}
	var page contract.EventsPage
	if err := json.Unmarshal([]byte(res.stdout), &page); err != nil {
		t.Fatalf("events output %q: %v", res.stdout, err)
	}
	return page.Events
}

// errorOf decodes the one {"error":{...}} object a structured failure writes to stderr.
func errorOf(t testing.TB, stderr string) contract.Error {
	t.Helper()
	var body contract.ErrorBody
	if err := json.Unmarshal([]byte(strings.TrimSpace(stderr)), &body); err != nil || body.Error == nil {
		t.Fatalf("stderr %q is not one error object: %v", stderr, err)
	}
	return *body.Error
}

// apiEventData decodes the data of an API event.
func apiEventData(t testing.TB, ev contract.Event) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(ev.Data, &m); err != nil {
		t.Fatalf("decode event data %s: %v", ev.Data, err)
	}
	return m
}

// numberOf reads a JSON number out of decoded event data.
func numberOf(v any) int {
	f, _ := v.(float64)
	return int(f)
}

// foreground starts `harness daemon start --foreground` and waits until the daemon is
// running (or, with waitFor == "starting", until its socket answers while it boots).
func foreground(t *testing.T, h home.Home, hooks, waitFor string) *bgProc {
	t.Helper()
	p := startCLI(t, h, hooks, "daemon", "start", "--foreground")
	waitState(t, h, waitFor, 30*time.Second)
	return p
}

// wantTypes fails unless the events have exactly these types, in order, with seqs 1..n.
func wantTypes(t testing.TB, events []contract.Event, types ...string) {
	t.Helper()
	got := wireTypes(events)
	if strings.Join(got, ",") != strings.Join(types, ",") {
		t.Fatalf("daemon stream types %v, want %v", got, types)
	}
	for i, ev := range events {
		if ev.Seq != int64(i+1) {
			t.Fatalf("event %d has seq %d, want %d", i, ev.Seq, i+1)
		}
	}
}
