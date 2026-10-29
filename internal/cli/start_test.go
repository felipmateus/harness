package cli

import (
	"context"
	"fmt"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/felipmateus/harness/internal/api/contract"
	"github.com/felipmateus/harness/internal/daemon"
	"github.com/felipmateus/harness/internal/testutil/testhome"
)

// becomesRunning scripts the status: unavailable for the first `after` calls, then running
// with pid.
func becomesRunning(api *fakeAPI, after, pid int) {
	api.statusFn = func(call int) (contract.Status, error) {
		if call <= after {
			return contract.Status{}, noDaemonError("fake")
		}
		return runningStatus(pid), nil
	}
}

// UT-040
func TestUT040_DetachedStartReportsTheChildWhenRunning(t *testing.T) {
	e := newEnv(t)
	becomesRunning(e.api, 3, 42)

	if code := e.run("daemon", "start"); code != 0 {
		t.Fatalf("exit %d, stderr %q", code, e.err())
	}
	want := fmt.Sprintf("harness daemon started (pid 42)\nsocket: %s\n", e.home.SocketPath())
	if e.out() != want {
		t.Fatalf("stdout %q, want %q", e.out(), want)
	}
	if e.err() != "" {
		t.Fatalf("stderr %q", e.err())
	}
	if e.spawner.count() != 1 {
		t.Fatalf("spawn count %d, want 1", e.spawner.count())
	}
	spec := e.spawner.specs[0]
	wantArgs := []string{"daemon", "start", "--foreground", "--detached", "--home", e.home.Dir}
	if spec.Exe != "/fake/harness" || strings.Join(spec.Args, " ") != strings.Join(wantArgs, " ") {
		t.Errorf("spawned %s %v, want /fake/harness %v", spec.Exe, spec.Args, wantArgs)
	}
	if spec.Dir != e.home.Dir {
		t.Errorf("child directory %q, want the home %q", spec.Dir, e.home.Dir)
	}
}

// UT-040: -o json is one document with already_running false.
func TestUT040_JSONDocument(t *testing.T) {
	e := newEnv(t)
	becomesRunning(e.api, 2, 42)

	if code := e.run("daemon", "start", "-o", "json"); code != 0 {
		t.Fatalf("exit %d, stderr %q", code, e.err())
	}
	want := fmt.Sprintf(`{"daemon":{"pid":42,"state":"running","socket":%q,"already_running":false}}`+"\n", e.home.SocketPath())
	if e.out() != want {
		t.Fatalf("stdout %q, want %q", e.out(), want)
	}
}

// UT-041
func TestUT041_StartIsIdempotent(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want func(e *env) string
	}{
		{[]string{"daemon", "start"}, func(*env) string { return "harness daemon already running (pid 7)\n" }},
		{[]string{"daemon", "start", "-o", "json"}, func(e *env) string {
			return fmt.Sprintf(`{"daemon":{"pid":7,"state":"running","socket":%q,"already_running":true}}`+"\n", e.home.SocketPath())
		}},
	} {
		e := newEnv(t)
		e.api.statusFn = func(int) (contract.Status, error) { return runningStatus(7), nil }
		if code := e.run(tc.args...); code != 0 {
			t.Fatalf("%v: exit %d, stderr %q", tc.args, code, e.err())
		}
		if e.out() != tc.want(e) {
			t.Errorf("%v: stdout %q, want %q", tc.args, e.out(), tc.want(e))
		}
		if e.spawner.count() != 0 {
			t.Errorf("%v: spawned %d times, want 0", tc.args, e.spawner.count())
		}
	}
}

// UT-042
func TestUT042_ReadinessNeedsTheChildsPID(t *testing.T) {
	e := newEnv(t)
	e.api.statusFn = func(call int) (contract.Status, error) {
		switch {
		case call == 1:
			return contract.Status{}, noDaemonError("fake") // the probe before the spawn
		case call <= 5:
			return runningStatus(99), nil // another daemon, not the child
		default:
			return runningStatus(42), nil
		}
	}
	start := e.clock.Now()

	if code := e.run("daemon", "start"); code != 0 {
		t.Fatalf("exit %d, stderr %q", code, e.err())
	}
	if !strings.HasPrefix(e.out(), "harness daemon started (pid 42)\n") {
		t.Fatalf("stdout %q", e.out())
	}
	if got := e.api.statusCalls.Load(); got != 6 {
		t.Errorf("status polled %d times, want 6 (probe, four mismatches, the match)", got)
	}
	if got := e.clock.elapsed(start); got != 400*time.Millisecond {
		t.Errorf("waited %v in virtual time, want 100 ms between the five polls", got)
	}
}

// UT-120: a home whose socket path is too long.
func TestUT120_SocketPathTooLongSpawnsNothingAndCreatesNothing(t *testing.T) {
	e := newEnv(t)
	parent := testhome.NewMissing(t)
	long := join(parent.Dir, strings.Repeat("a", 60), strings.Repeat("b", 60))

	code := e.run("daemon", "start", "-o", "json", "--home", long)
	if code != 78 {
		t.Fatalf("exit %d, want 78; stderr %q", code, e.err())
	}
	if got := e.wireError(); got.Code != contract.CodeSocketPathTooLong || got.Hint != "set HARNESS_HOME to a shorter directory" {
		t.Errorf("error %+v", got)
	}
	if e.out() != "" {
		t.Errorf("stdout %q, want empty", e.out())
	}
	if e.spawner.count() != 0 || e.api.calls() != 0 {
		t.Errorf("spawned %d, called the daemon %d times; want 0 and 0", e.spawner.count(), e.api.calls())
	}
	if exists(parent.Dir) {
		t.Errorf("%s was created", parent.Dir)
	}
}

// UT-120: a home that others can write.
func TestUT120_InsecureHomeIsRefusedBeforeSpawning(t *testing.T) {
	e := newEnv(t)
	if err := os.Chmod(e.home.Dir, 0o777); err != nil {
		t.Fatal(err)
	}

	code := e.run("daemon", "start")
	if code != 78 {
		t.Fatalf("exit %d, want 78; stderr %q", code, e.err())
	}
	want := fmt.Sprintf("error: home %s is not private: mode 0777 is writable by group or others\nhint: chmod 700 %s\n", e.home.Dir, e.home.Dir)
	if e.err() != want {
		t.Errorf("stderr %q, want %q", e.err(), want)
	}
	if e.spawner.count() != 0 || e.api.calls() != 0 {
		t.Errorf("spawned %d, called the daemon %d times; want 0 and 0", e.spawner.count(), e.api.calls())
	}
}

// UT-120: a missing home is created private, then the daemon is spawned.
func TestUT120_MissingHomeIsCreatedPrivateThenSpawned(t *testing.T) {
	e := newEnv(t)
	e.home = testhome.NewMissing(t)
	becomesRunning(e.api, 1, 42)

	if code := e.run("daemon", "start"); code != 0 {
		t.Fatalf("exit %d, stderr %q", code, e.err())
	}
	for _, dir := range []string{e.home.Dir, e.home.LogsDir()} {
		fi, err := os.Stat(dir)
		if err != nil {
			t.Fatalf("%s: %v", dir, err)
		}
		if fi.Mode().Perm() != 0o700 {
			t.Errorf("%s has mode %v, want 0700", dir, fi.Mode().Perm())
		}
	}
	if e.spawner.count() != 1 {
		t.Errorf("spawned %d times, want 1", e.spawner.count())
	}
}

// UT-120: the socket path is checked before the security of the home, so a home that fails
// both is reported for its path.
func TestUT120_SocketPathIsCheckedBeforeSecurity(t *testing.T) {
	e := newEnv(t)
	long := join(testhome.New(t).Dir, strings.Repeat("a", 60), strings.Repeat("b", 60))
	if err := os.MkdirAll(long, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(long, 0o777); err != nil {
		t.Fatal(err)
	}

	if code := e.run("daemon", "start", "-o", "json", "--home", long); code != 78 {
		t.Fatalf("exit %d, want 78", code)
	}
	if got := e.wireError(); got.Code != contract.CodeSocketPathTooLong {
		t.Errorf("error %+v, want the socket path error", got)
	}
}

// UT-120: the order is socket path, then security, then layout, with the daemon contacted
// last; a refusal at an earlier check never reaches a later one.
func TestUT120_ChecksRunBeforeAnyNetworkCall(t *testing.T) {
	e := newEnv(t)
	if err := os.Chmod(e.home.Dir, 0o777); err != nil {
		t.Fatal(err)
	}
	e.api.statusFn = func(int) (contract.Status, error) {
		t.Error("the daemon was contacted before the home was checked")
		return contract.Status{}, noDaemonError("fake")
	}
	e.run("daemon", "start")
	if exists(e.home.LogsDir()) {
		t.Error("the layout was created in a home that failed the security check")
	}
}

// UT-133: daemon.out is appended to, never truncated, and the tail starts at the old size.
func TestUT133_DaemonOutIsAppendedAndTheTailStartsAtTheOffset(t *testing.T) {
	e := newEnv(t)
	writeDaemonOut(t, e.home, "old 1\nold 2\nold 3\n")
	e.spawner.onJump = func(spec spawnSpec, p *fakeProc) {
		// The file is the child's stdout: a write lands at the end whatever the position,
		// which only O_APPEND guarantees.
		if _, err := spec.Out.Seek(0, 0); err != nil {
			t.Error(err)
		}
		if _, err := spec.Out.WriteString("new 1\nnew 2\n"); err != nil {
			t.Error(err)
		}
		p.exit(1)
	}

	code := e.run("daemon", "start", "-o", "json")
	if code != 69 {
		t.Fatalf("exit %d, want 69; stderr %q", code, e.err())
	}
	if got := readFile(t, e.home.DaemonOutPath()); got != "old 1\nold 2\nold 3\nnew 1\nnew 2\n" {
		t.Errorf("daemon.out is %q: the old lines must stay, and the new ones follow them", got)
	}
	werr := e.wireError()
	if werr.Code != contract.CodeDaemonStartFailed {
		t.Fatalf("error %+v", werr)
	}
	details := fmt.Sprint(werr.Details)
	if strings.Contains(details, "old") || !strings.Contains(details, "new 1") || !strings.Contains(details, "new 2") {
		t.Errorf("details %s: the tail must hold the new lines and none of the old", details)
	}
}

// UT-133: a daemon.out that does not exist is created 0600.
func TestUT133_DaemonOutIsCreatedPrivate(t *testing.T) {
	e := newEnv(t)
	e.spawner.onJump = func(_ spawnSpec, p *fakeProc) { p.exit(1) }
	e.run("daemon", "start")

	fi, err := os.Stat(e.home.DaemonOutPath())
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("daemon.out has mode %v, want 0600", fi.Mode().Perm())
	}
}

// A child that exits before it is ready is a start failure with the last 20 lines of its
// output, in human mode below the hint.
func TestStartFailureReportsTheLastTwentyLines(t *testing.T) {
	e := newEnv(t)
	e.spawner.onJump = func(spec spawnSpec, p *fakeProc) {
		var b strings.Builder
		for i := 1; i <= 25; i++ {
			fmt.Fprintf(&b, "line %d\n", i)
		}
		spec.Out.WriteString(b.String())
		p.exit(1)
	}

	code := e.run("daemon", "start")
	if code != 69 {
		t.Fatalf("exit %d, want 69", code)
	}
	var want strings.Builder
	fmt.Fprintf(&want, "error: daemon exited during startup (pid 42, exit 1)\nhint: see %s\n", e.home.LogPath())
	want.WriteString("--- daemon output (last 20 lines) ---\n")
	for i := 6; i <= 25; i++ {
		fmt.Fprintf(&want, "line %d\n", i)
	}
	if e.err() != want.String() {
		t.Errorf("stderr:\n%s\nwant:\n%s", e.err(), want.String())
	}
	if e.out() != "" {
		t.Errorf("stdout %q, want empty", e.out())
	}
}

// With no output at all, the block says so.
func TestStartFailureWithNoOutput(t *testing.T) {
	e := newEnv(t)
	e.spawner.onJump = func(_ spawnSpec, p *fakeProc) { p.exit(3) }
	if code := e.run("daemon", "start"); code != 69 {
		t.Fatalf("exit %d", code)
	}
	if !strings.HasSuffix(e.err(), "--- daemon output (last 20 lines) ---\n(no output)\n") {
		t.Errorf("stderr %q", e.err())
	}
	if !strings.Contains(e.err(), "(pid 42, exit 3)") {
		t.Errorf("stderr %q does not carry the exit code", e.err())
	}
}

// The readiness timeout stops the child it spawned (SIGTERM to its group) and reports it
// with the tail, within the timeout that bounds the whole command.
func TestStartTimeoutStopsTheChild(t *testing.T) {
	e := newEnv(t)
	e.api.statusFn = func(int) (contract.Status, error) { return contract.Status{}, noDaemonError("fake") }
	start := e.clock.Now()
	var termAt time.Duration
	e.spawner.onJump = func(spec spawnSpec, p *fakeProc) {
		spec.Out.WriteString("booting\n")
		p.onSignal = func(p *fakeProc, sig syscall.Signal) {
			if sig == syscall.SIGTERM {
				termAt = e.clock.elapsed(start)
				p.exit(0)
			}
		}
	}

	code := e.run("daemon", "start", "--timeout", "2s", "-o", "json")
	if code != 69 {
		t.Fatalf("exit %d, want 69; stderr %q", code, e.err())
	}
	werr := e.wireError()
	if werr.Code != contract.CodeDaemonStartTimeout ||
		werr.Message != "daemon did not become ready within 2s (pid 42); it was stopped" ||
		werr.Hint != "see "+e.home.LogPath() {
		t.Errorf("error %+v", werr)
	}
	if !strings.Contains(fmt.Sprint(werr.Details), "booting") {
		t.Errorf("details %v lack the tail", werr.Details)
	}
	if got := e.spawner.proc.sent(); len(got) != 1 || got[0] != syscall.SIGTERM {
		t.Errorf("signals sent %v, want exactly SIGTERM", got)
	}
	if termAt < 2*time.Second || termAt > 2200*time.Millisecond {
		t.Errorf("SIGTERM sent after %v of virtual time, want the 2s timeout", termAt)
	}
}

func TestStartTimeoutHumanShowsTheDaemonOutput(t *testing.T) {
	e := newEnv(t)
	e.spawner.onJump = func(_ spawnSpec, p *fakeProc) {
		p.onSignal = func(p *fakeProc, _ syscall.Signal) { p.exit(0) }
	}
	e.api.statusFn = func(int) (contract.Status, error) { return contract.Status{}, noDaemonError("fake") }

	if code := e.run("daemon", "start", "--timeout", "1s"); code != 69 {
		t.Fatalf("exit %d", code)
	}
	want := fmt.Sprintf("error: daemon did not become ready within 1s (pid 42); it was stopped\nhint: see %s\n--- daemon output (last 20 lines) ---\n(no output)\n", e.home.LogPath())
	if e.err() != want {
		t.Errorf("stderr %q, want %q", e.err(), want)
	}
}

// A spawn that cannot even start is a start failure, not an internal error.
func TestStartSpawnFailure(t *testing.T) {
	e := newEnv(t)
	e.spawner.err = fmt.Errorf("exec format error")
	if code := e.run("daemon", "start", "-o", "json"); code != 69 {
		t.Fatalf("exit %d, want 69", code)
	}
	if got := e.wireError(); got.Code != contract.CodeDaemonStartFailed || !strings.Contains(got.Message, "exec format error") {
		t.Errorf("error %+v", got)
	}
}

// A status error that is not "no daemon" stops the start before anything is spawned.
func TestStartStopsOnAnUnexpectedProbeError(t *testing.T) {
	e := newEnv(t)
	e.api.statusFn = func(int) (contract.Status, error) { return contract.Status{}, contract.InternalError() }
	if code := e.run("daemon", "start"); code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
	if e.spawner.count() != 0 {
		t.Error("spawned although the probe failed unexpectedly")
	}
}

// Foreground start hands the home, the mode and the stderr to daemon.Run, and turns its
// failure into the documented message and exit code.
func TestForegroundStart(t *testing.T) {
	t.Run("interactive", func(t *testing.T) {
		e := newEnv(t)
		if code := e.run("daemon", "start", "--foreground"); code != 0 {
			t.Fatalf("exit %d, stderr %q", code, e.err())
		}
		if len(e.daemons) != 1 {
			t.Fatalf("Run called %d times", len(e.daemons))
		}
		got := e.daemons[0]
		if got.Home.Dir != e.home.Dir || got.Mode != daemon.Interactive || got.Stderr != &e.stderr {
			t.Errorf("options %+v", got)
		}
		if got.Version.Version == "" {
			t.Error("the version identity was not passed")
		}
		if e.spawner.count() != 0 || e.api.calls() != 0 {
			t.Error("a foreground start must not spawn or probe")
		}
	})
	t.Run("detached child", func(t *testing.T) {
		e := newEnv(t)
		if code := e.run("daemon", "start", "--foreground", "--detached"); code != 0 {
			t.Fatalf("exit %d", code)
		}
		if e.daemons[0].Mode != daemon.Detached {
			t.Errorf("mode %v, want Detached", e.daemons[0].Mode)
		}
	})
	t.Run("detached needs foreground", func(t *testing.T) {
		e := newEnv(t)
		if code := e.run("daemon", "start", "--detached", "-o", "json"); code != 64 {
			t.Fatalf("exit %d, want 64", code)
		}
		if got := e.wireError(); got.Code != contract.CodeUsage {
			t.Errorf("error %+v", got)
		}
	})
	t.Run("second foreground", func(t *testing.T) {
		e := newEnv(t)
		e.app.runDaemon = func(_ context.Context, _ daemon.Options) error {
			return &daemon.ExitError{Code: 69, ErrCode: contract.CodeDaemonAlreadyRunning,
				Message: "daemon already running for /h (pid 9)"}
		}
		if code := e.run("daemon", "start", "--foreground"); code != 69 {
			t.Fatalf("exit %d, want 69", code)
		}
		if e.err() != "error: daemon already running for /h (pid 9)\n" || e.out() != "" {
			t.Errorf("stderr %q, stdout %q", e.err(), e.out())
		}
	})
	t.Run("config invalid", func(t *testing.T) {
		e := newEnv(t)
		e.app.runDaemon = func(_ context.Context, _ daemon.Options) error {
			return &daemon.ExitError{Code: 78, ErrCode: contract.CodeConfigInvalid,
				Message: "config invalid: /h/config.toml: unknown key", Hint: "run `harness config validate` for all problems"}
		}
		if code := e.run("daemon", "start", "--foreground", "-o", "json"); code != 78 {
			t.Fatalf("exit %d, want 78", code)
		}
		if got := e.wireError(); got.Code != contract.CodeConfigInvalid || got.Hint == "" {
			t.Errorf("error %+v", got)
		}
	})
	// UT-136: the foreground daemon process takes SIGPIPE for more than the length of Run:
	// the error path writes Run's failure to stderr afterwards, and a write to a stderr
	// whose reader has gone must not end the process with SIGPIPE instead of the exit code
	// (IT-058 runs it against a real broken pipe).
	t.Run("SIGPIPE is held through the final error write", func(t *testing.T) {
		e := newEnv(t)
		var heldAtWrite []int
		e.app.stderr = writerFunc(func(p []byte) (int, error) {
			heldAtWrite = append(heldAtWrite, len(e.app.atExit))
			return e.stderr.Write(p)
		})
		e.app.runDaemon = func(_ context.Context, _ daemon.Options) error {
			return &daemon.ExitError{Code: 1, ErrCode: "shutdown_budget_exceeded",
				Message: "shutdown exceeded its budget of 1s at step 3 (drain)"}
		}
		if code := e.run("daemon", "start", "--foreground"); code != 1 {
			t.Fatalf("exit %d, want 1", code)
		}
		if len(heldAtWrite) == 0 {
			t.Fatal("the failure was not written to stderr")
		}
		for i, held := range heldAtWrite {
			if held != 1 {
				t.Errorf("write %d ran with %d process-lifetime holds, want 1: SIGPIPE was already released", i, held)
			}
		}
		if n := len(e.app.atExit); n != 0 {
			t.Errorf("%d holds left after execute: SIGPIPE stays taken for the rest of the process", n)
		}
	})
}

// writerFunc is an io.Writer made of a function.
type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }
