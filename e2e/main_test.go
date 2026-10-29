//go:build e2e

// Package e2e runs the built harness binary the way `_dx.md` documents it. TestMain builds
// the binary once; every test uses a temporary home under /tmp (HARNESS_HOME) and a
// temporary HOME, so nothing here can reach the operator's real ~/.harness.
package e2e

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/felipmateus/harness/internal/home"
	"github.com/felipmateus/harness/internal/testutil/testhome"
)

var (
	// binary is the path of the harness binary built by TestMain.
	binary string
	// fakeHomeDir is the $HOME of every harness process of the run.
	fakeHomeDir string
)

func TestMain(m *testing.M) {
	os.Exit(run(m))
}

func run(m *testing.M) int {
	dir, err := os.MkdirTemp("", "harness-e2e-bin")
	if err != nil {
		fmt.Fprintln(os.Stderr, "e2e:", err)
		return 1
	}
	defer os.RemoveAll(dir)

	// An empty $HOME for every harness process, so that no default-home lookup can find a
	// real ~/.harness.
	fakeHomeDir, err = os.MkdirTemp("", "harness-e2e-home")
	if err != nil {
		fmt.Fprintln(os.Stderr, "e2e:", err)
		return 1
	}
	defer os.RemoveAll(fakeHomeDir)

	binary = filepath.Join(dir, "harness")
	buildArgs := []string{"build"}
	if buildTags != "" {
		buildArgs = append(buildArgs, "-tags", buildTags)
	}
	build := exec.Command("go", append(buildArgs, "-o", binary, "./cmd/harness")...)
	build.Dir = ".." // the module root: go test runs in the package directory
	if out, err := build.CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "e2e: build the harness binary: %v\n%s", err, out)
		return 1
	}
	return m.Run()
}

// result is one finished invocation.
type result struct {
	stdout, stderr string
	exit           int
}

// env builds the environment of a harness process: HARNESS_HOME is always set, and HOME
// points at an empty temporary directory, so a default-home lookup cannot find a real one.
func env(t testing.TB, h home.Home) []string {
	t.Helper()
	if h.Dir == "" {
		t.Fatal("e2e: refusing to run with an empty home")
	}
	var out []string
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "HARNESS_") && !strings.HasPrefix(kv, "HOME=") {
			out = append(out, kv)
		}
	}
	return append(out, "HARNESS_HOME="+h.Dir, "HOME="+fakeHomeDir)
}

// harness runs the binary to completion with a 60 s limit.
func harness(t testing.TB, h home.Home, args ...string) result {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.Env = env(t, h)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	res := result{stdout: stdout.String(), stderr: stderr.String()}
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

// newHome returns an existing private home that is stopped and removed when the test ends.
func newHome(t *testing.T) home.Home {
	t.Helper()
	h := testhome.New(t)
	t.Cleanup(func() { stopDaemon(t, h) })
	return h
}

// newMissingHome returns a home that does not exist yet, cleaned up like newHome.
func newMissingHome(t *testing.T) home.Home {
	t.Helper()
	h := testhome.NewMissing(t)
	t.Cleanup(func() { stopDaemon(t, h) })
	return h
}

// stopDaemon leaves no daemon behind: it stops the home's daemon, and kills one that does
// not stop, which fails the test.
func stopDaemon(t *testing.T, h home.Home) {
	t.Helper()
	if _, err := os.Stat(h.Dir); err != nil {
		return
	}
	harness(t, h, "daemon", "stop")
	free, _ := h.LockFree()
	if free {
		return
	}
	if pid := h.LockPID(); pid > 0 {
		syscall.Kill(pid, syscall.SIGKILL)
		t.Errorf("a daemon (pid %d) was left running for %s and was killed", pid, h.Dir)
	}
}

// start starts the daemon of h in the background and returns its PID.
func start(t *testing.T, h home.Home) int {
	t.Helper()
	res := harness(t, h, "daemon", "start")
	if res.exit != 0 {
		t.Fatalf("daemon start: exit %d\nstdout %s\nstderr %s", res.exit, res.stdout, res.stderr)
	}
	pid, ok := startedPID(res.stdout)
	if !ok {
		t.Fatalf("daemon start printed %q", res.stdout)
	}
	return pid
}

// startedPID extracts N from `harness daemon started (pid N)`.
func startedPID(out string) (int, bool) {
	const prefix = "harness daemon started (pid "
	line, _, _ := strings.Cut(out, "\n")
	rest, ok := strings.CutPrefix(line, prefix)
	if !ok {
		return 0, false
	}
	pid, err := strconv.Atoi(strings.TrimSuffix(rest, ")"))
	return pid, err == nil
}

// syncBuffer collects a process's output while the test reads it.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// background is a harness process started without waiting for it.
type background struct {
	cmd    *exec.Cmd
	stdout *syncBuffer
	stderr *syncBuffer
	done   chan struct{}
	exit   int
}

// startBackground runs the binary and returns at once. The process is killed when the test
// ends if it still runs.
func startBackground(t *testing.T, h home.Home, args ...string) *background {
	t.Helper()
	cmd := exec.Command(binary, args...)
	cmd.Env = env(t, h)
	b := &background{cmd: cmd, stdout: &syncBuffer{}, stderr: &syncBuffer{}, done: make(chan struct{})}
	cmd.Stdout, cmd.Stderr = b.stdout, b.stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() {
		err := cmd.Wait()
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			b.exit = exitErr.ExitCode()
		}
		close(b.done)
	}()
	t.Cleanup(func() {
		select {
		case <-b.done:
		default:
			cmd.Process.Kill()
			<-b.done
		}
	})
	return b
}

// wait returns the exit code once the process ends, or fails after d.
func (b *background) wait(t *testing.T, d time.Duration) int {
	t.Helper()
	select {
	case <-b.done:
		return b.exit
	case <-time.After(d):
		t.Fatalf("process did not exit within %v\nstdout %s\nstderr %s", d, b.stdout.String(), b.stderr.String())
		return -1
	}
}

// waitFor polls cond every 20 ms until it holds, and fails the test after d.
func waitFor(t *testing.T, what string, d time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out after %v waiting for %s", d, what)
}
