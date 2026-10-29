package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/felipmateus/harness/internal/api/contract"
	"github.com/felipmateus/harness/internal/daemon"
)

// errorLine is the final line a failed detached daemon writes to daemon.out.
func errorLine(code, message, hint string) string {
	data, _ := json.Marshal(contract.ErrorBody{Error: &contract.Error{Code: code, Message: message, Hint: hint}})
	return string(data) + "\n"
}

// childWrites makes the spawned child write out to daemon.out and exit with code, the way
// a failing --detached daemon does before the CLI first looks at it.
func childWrites(e *env, out string, code int) {
	e.spawner.onJump = func(spec spawnSpec, p *fakeProc) {
		spec.Out.WriteString(out)
		p.exit(code)
	}
}

// UT-121: a failure the daemon recognises is reported with its own code and exit status,
// taken from the last error line the child wrote after the offset.
func TestUT121_RecognisedChildErrorsAreReRaised(t *testing.T) {
	for _, tc := range []struct {
		code     string
		message  string
		hint     string
		wantExit int
	}{
		{contract.CodeConfigInvalid, "config invalid: /h/config.toml: unknown key \"daemon.shutdown_grase\"", "run `harness config validate`", 78},
		{contract.CodeHomeInsecure, "home /h is not private: mode 0777 is writable by group or others", "chmod 700 /h", 78},
		{contract.CodeSocketPathTooLong, "socket path /h/daemon.sock is 113 bytes; at most 103 bytes on macOS and 107 on Linux", "set HARNESS_HOME to a shorter directory", 78},
		{contract.CodeLedgerSchemaAhead, "ledger schema 99 is newer than this binary supports (1)", "upgrade harness", 69},
	} {
		t.Run(tc.code, func(t *testing.T) {
			e := newEnv(t)
			writeDaemonOut(t, e.home, errorLine(contract.CodeConfigInvalid, "an older run", "")) // before the offset
			childWrites(e, `{"time":"t","level":"ERROR","msg":"boot step failed","step":4}`+"\n"+
				errorLine(tc.code, tc.message, tc.hint), tc.wantExit)

			code := e.run("daemon", "start", "-o", "json")
			if code != tc.wantExit {
				t.Fatalf("exit %d, want %d; stderr %q", code, tc.wantExit, e.err())
			}
			got := e.wireError()
			if got.Code != tc.code || got.Message != tc.message || got.Hint != tc.hint || got.Details != nil {
				t.Errorf("error %+v, want %s %q %q and no details", got, tc.code, tc.message, tc.hint)
			}
			if e.out() != "" {
				t.Errorf("stdout %q, want empty", e.out())
			}
			if e.spawner.count() != 1 {
				t.Errorf("spawned %d times, want 1: a recognised failure is not retried", e.spawner.count())
			}
		})
	}
}

// UT-121: in human mode the re-raised error reads like any other.
func TestUT121_ReRaisedErrorInHumanMode(t *testing.T) {
	e := newEnv(t)
	childWrites(e, errorLine(contract.CodeConfigInvalid, "config invalid: /h/config.toml: unknown key", "run `harness config validate`"), 78)
	if code := e.run("daemon", "start"); code != 78 {
		t.Fatalf("exit %d, want 78", code)
	}
	want := "error: config invalid: /h/config.toml: unknown key\nhint: run `harness config validate`\n"
	if e.err() != want {
		t.Errorf("stderr %q, want %q", e.err(), want)
	}
}

// UT-121: the last error line wins, and a line before the offset is never read.
func TestUT121_TheLastErrorLineAfterTheOffsetWins(t *testing.T) {
	e := newEnv(t)
	writeDaemonOut(t, e.home, errorLine(contract.CodeHomeInsecure, "from an earlier start", ""))
	childWrites(e, errorLine(contract.CodeConfigInvalid, "first", "")+"noise\n"+errorLine(contract.CodeLedgerSchemaAhead, "second", ""), 69)

	if code := e.run("daemon", "start", "-o", "json"); code != 69 {
		t.Fatalf("exit %d, want 69", code)
	}
	if got := e.wireError(); got.Code != contract.CodeLedgerSchemaAhead || got.Message != "second" {
		t.Errorf("error %+v, want the last line", got)
	}
}

// UT-121: an error line from before the offset does not turn this start's failure into a
// re-raise.
func TestUT121_AnOldErrorLineIsNotReRaised(t *testing.T) {
	e := newEnv(t)
	writeDaemonOut(t, e.home, errorLine(contract.CodeConfigInvalid, "from an earlier start", ""))
	childWrites(e, "this start only\n", 1)

	if code := e.run("daemon", "start", "-o", "json"); code != 69 {
		t.Fatalf("exit %d, want 69", code)
	}
	if got := e.wireError(); got.Code != contract.CodeDaemonStartFailed {
		t.Errorf("error %+v, want daemon_start_failed", got)
	}
}

// UT-121: daemon_already_running means another daemon owns the home. The start goes back to
// the status probe and reports the daemon that runs, without a second spawn.
func TestUT121_AlreadyRunningGoesBackToTheProbe(t *testing.T) {
	e := newEnv(t)
	childWrites(e, errorLine(contract.CodeDaemonAlreadyRunning, "daemon already running for /h (pid 77)", ""), 69)
	e.api.statusFn = func(call int) (contract.Status, error) {
		if call == 1 {
			return contract.Status{}, noDaemonError("fake") // the first probe: nobody there
		}
		return runningStatus(77), nil // the winner of the race
	}

	if code := e.run("daemon", "start"); code != 0 {
		t.Fatalf("exit %d, stderr %q", code, e.err())
	}
	if e.out() != "harness daemon already running (pid 77)\n" {
		t.Errorf("stdout %q", e.out())
	}
	if e.spawner.count() != 1 {
		t.Errorf("spawned %d times, want 1", e.spawner.count())
	}
}

// UT-121: the winner is still booting (nothing answers, the lock is held): the loser waits
// for it and reports it once it runs. It is never reported before the status says running.
func TestUT121_AlreadyRunningWaitsForTheWinnerToBoot(t *testing.T) {
	e := newEnv(t)
	e.spawner.onJump = func(spec spawnSpec, p *fakeProc) {
		spec.Out.WriteString(errorLine(contract.CodeDaemonAlreadyRunning, "daemon already running for /h (pid 77)", ""))
		e.probe.freeAfter = -1 // from now on the winner holds the lock
		e.probe.pid = 77
		p.exit(69)
	}
	e.api.statusFn = func(call int) (contract.Status, error) {
		if call < 5 {
			return contract.Status{}, noDaemonError("fake")
		}
		return runningStatus(77), nil
	}

	if code := e.run("daemon", "start", "-o", "json"); code != 0 {
		t.Fatalf("exit %d, stderr %q", code, e.err())
	}
	want := fmt.Sprintf(`{"daemon":{"pid":77,"state":"running","socket":%q,"already_running":true}}`+"\n", e.home.SocketPath())
	if e.out() != want {
		t.Errorf("stdout %q, want %q", e.out(), want)
	}
	if e.err() != "" {
		t.Errorf("stderr %q: no progress line in json mode", e.err())
	}
	if e.spawner.count() != 1 {
		t.Errorf("spawned %d times, want 1", e.spawner.count())
	}
}

// UT-121: a probe collision that made the child lose to nobody (the lock is free again when
// the CLI looks) starts over and spawns a second child.
func TestUT121_AlreadyRunningWithAFreeHomeSpawnsAgain(t *testing.T) {
	e := newEnv(t)
	attempt := 0
	e.spawner.onJump = func(spec spawnSpec, p *fakeProc) {
		attempt++
		if attempt == 1 {
			spec.Out.WriteString(errorLine(contract.CodeDaemonAlreadyRunning, "daemon already running (pid unknown)", ""))
			p.exit(69)
		}
	}
	e.spawner.proc = nil
	e.api.statusFn = func(call int) (contract.Status, error) {
		if call <= 3 {
			return contract.Status{}, noDaemonError("fake")
		}
		return runningStatus(42), nil
	}
	// A fresh child for every spawn, like a real spawner.
	e.app.spawner = spawnerFunc(func(spec spawnSpec) (process, error) {
		e.spawner.mu.Lock()
		e.spawner.specs = append(e.spawner.specs, spec)
		e.spawner.mu.Unlock()
		p := newFakeProc(42)
		e.spawner.onJump(spec, p)
		return p, nil
	})

	if code := e.run("daemon", "start"); code != 0 {
		t.Fatalf("exit %d, stderr %q", code, e.err())
	}
	if !strings.HasPrefix(e.out(), "harness daemon started (pid 42)\n") {
		t.Errorf("stdout %q", e.out())
	}
	if e.spawner.count() != 2 {
		t.Errorf("spawned %d times, want 2", e.spawner.count())
	}
}

type spawnerFunc func(spawnSpec) (process, error)

func (f spawnerFunc) Spawn(spec spawnSpec) (process, error) { return f(spec) }

// UT-121: a home that stays owned until the timeout ends in daemon_start_timeout, not a loop.
func TestUT121_AlreadyRunningLoopIsBoundedByTheTimeout(t *testing.T) {
	e := newEnv(t)
	e.spawner.onJump = func(spec spawnSpec, p *fakeProc) {
		spec.Out.WriteString(errorLine(contract.CodeDaemonAlreadyRunning, "daemon already running (pid 77)", ""))
		e.probe.freeAfter = -1
		p.exit(69)
	}
	e.api.statusFn = func(int) (contract.Status, error) { return contract.Status{}, noDaemonError("fake") }
	start := e.clock.Now()

	if code := e.run("daemon", "start", "--timeout", "1s", "-o", "json"); code != 69 {
		t.Fatalf("exit %d, want 69; stderr %q", code, e.err())
	}
	if got := e.wireError(); got.Code != contract.CodeDaemonStartTimeout {
		t.Errorf("error %+v", got)
	}
	if got := e.clock.elapsed(start); got < time.Second || got > 1500*time.Millisecond {
		t.Errorf("gave up after %v of virtual time, want about 1s", got)
	}
}

// UT-122: no error line, exit code 1: daemon_start_failed with the PID, the exit code and
// the last 20 lines written after the offset; the lines before it are left out.
func TestUT122_StartFailedCarriesTheTailAfterTheOffset(t *testing.T) {
	e := newEnv(t)
	var old strings.Builder
	for i := 1; i <= 30; i++ {
		fmt.Fprintf(&old, "old %d\n", i)
	}
	writeDaemonOut(t, e.home, old.String())
	var out strings.Builder
	for i := 1; i <= 25; i++ {
		fmt.Fprintf(&out, "new %d\n", i)
	}
	childWrites(e, out.String(), 1)

	if code := e.run("daemon", "start", "-o", "json"); code != 69 {
		t.Fatalf("exit %d, want 69; stderr %q", code, e.err())
	}
	var body struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
			Details struct {
				PID        int      `json:"pid"`
				ExitCode   int      `json:"exit_code"`
				OutputTail []string `json:"output_tail"`
			} `json:"details"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(e.err()), &body); err != nil {
		t.Fatalf("stderr %q: %v", e.err(), err)
	}
	got := body.Error
	if got.Code != contract.CodeDaemonStartFailed || got.Message != "daemon exited during startup (pid 42, exit 1)" {
		t.Errorf("error %+v", got)
	}
	if got.Details.PID != 42 || got.Details.ExitCode != 1 {
		t.Errorf("details %+v, want pid 42 and exit_code 1", got.Details)
	}
	tail := got.Details.OutputTail
	if len(tail) != 20 || tail[0] != "new 6" || tail[19] != "new 25" {
		t.Errorf("tail %q, want new 6 .. new 25", tail)
	}
	for _, l := range tail {
		if strings.HasPrefix(l, "old") {
			t.Errorf("the tail holds %q from before the offset", l)
		}
	}
}

// UT-122: an error line the CLI does not re-raise is part of the tail like any other line.
func TestUT122_AnUnrecognisedErrorLineIsStillStartFailed(t *testing.T) {
	e := newEnv(t)
	line := errorLine("listen_failed", "listen on /h/daemon.sock: address in use", "see /h/logs/harness.log")
	childWrites(e, line, 1)

	if code := e.run("daemon", "start", "-o", "json"); code != 69 {
		t.Fatalf("exit %d, want 69", code)
	}
	got := e.wireError()
	if got.Code != contract.CodeDaemonStartFailed || !strings.Contains(fmt.Sprint(got.Details), "listen_failed") {
		t.Errorf("error %+v", got)
	}
}

// UT-123: status running means already running, and nothing is spawned.
func TestUT123_RunningIsReportedWithoutSpawning(t *testing.T) {
	e := newEnv(t)
	e.api.statusFn = func(int) (contract.Status, error) { return runningStatus(7), nil }
	if code := e.run("daemon", "start"); code != 0 || e.out() != "harness daemon already running (pid 7)\n" {
		t.Fatalf("exit %d, stdout %q, stderr %q", code, e.out(), e.err())
	}
	if e.spawner.count() != 0 {
		t.Errorf("spawned %d times", e.spawner.count())
	}
}

// UT-123: starting means another start is in progress: poll until running, then report it.
func TestUT123_StartingWaitsThenReportsAlreadyRunning(t *testing.T) {
	e := newEnv(t)
	e.api.statusFn = func(call int) (contract.Status, error) {
		if call <= 3 {
			return contract.Status{State: "starting", PID: 7}, nil
		}
		return runningStatus(7), nil
	}
	start := e.clock.Now()

	if code := e.run("daemon", "start"); code != 0 {
		t.Fatalf("exit %d, stderr %q", code, e.err())
	}
	if e.out() != "harness daemon already running (pid 7)\n" || e.err() != "" {
		t.Errorf("stdout %q stderr %q", e.out(), e.err())
	}
	if e.spawner.count() != 0 {
		t.Errorf("spawned %d times, want 0", e.spawner.count())
	}
	if got := e.clock.elapsed(start); got != 300*time.Millisecond {
		t.Errorf("waited %v, want three 100 ms polls", got)
	}
}

// UT-123: draining means the daemon is going away. The waiting line is printed (once, on
// stderr), the start waits until the lock is free, then spawns a new daemon.
func TestUT123_DrainingWaitsForTheLockThenSpawns(t *testing.T) {
	e := newEnv(t)
	e.probe.freeAfter = 3 // the lock is held for three looks
	e.api.statusFn = func(call int) (contract.Status, error) {
		switch {
		case call <= 2:
			return contract.Status{State: "draining", PID: 9}, nil
		case call <= 6:
			return contract.Status{}, noDaemonError("fake") // the socket is gone, the lock is not yet free
		default:
			return runningStatus(42), nil
		}
	}

	if code := e.run("daemon", "start"); code != 0 {
		t.Fatalf("exit %d, stderr %q", code, e.err())
	}
	if want := "waiting for the previous daemon (pid 9) to exit\n"; e.err() != want {
		t.Errorf("stderr %q, want %q (once)", e.err(), want)
	}
	if !strings.HasPrefix(e.out(), "harness daemon started (pid 42)\n") {
		t.Errorf("stdout %q: a new daemon must be reported, never the old one", e.out())
	}
	if e.spawner.count() != 1 {
		t.Errorf("spawned %d times, want 1", e.spawner.count())
	}
	if e.probe.lockCalls < 4 {
		t.Errorf("the lock was probed %d times: the start did not wait for it to be free", e.probe.lockCalls)
	}
}

// UT-123: the waiting line is for people: json and jsonl modes print nothing on stderr.
func TestUT123_TheWaitingLineIsHumanOnly(t *testing.T) {
	for _, mode := range []string{"json", "jsonl"} {
		e := newEnv(t)
		e.api.statusFn = func(call int) (contract.Status, error) {
			if call <= 2 {
				return contract.Status{State: "draining", PID: 9}, nil
			}
			if call == 3 {
				return contract.Status{}, noDaemonError("fake")
			}
			return runningStatus(42), nil
		}
		if code := e.run("daemon", "start", "-o", mode); code != 0 {
			t.Fatalf("%s: exit %d, stderr %q", mode, code, e.err())
		}
		if e.err() != "" {
			t.Errorf("%s: stderr %q, want empty", mode, e.err())
		}
	}
}

// UT-123: a daemon that is still draining when --timeout passes is a daemon_start_timeout,
// and nothing is spawned.
func TestUT123_DrainingNeverFreeIsAStartTimeout(t *testing.T) {
	e := newEnv(t)
	e.api.statusFn = func(int) (contract.Status, error) { return contract.Status{State: "draining", PID: 9}, nil }
	e.probe.freeAfter = -1
	start := e.clock.Now()

	if code := e.run("daemon", "start", "--timeout", "2s"); code != 69 {
		t.Fatalf("exit %d, want 69; stderr %q", code, e.err())
	}
	want := fmt.Sprintf("waiting for the previous daemon (pid 9) to exit\nerror: previous daemon (pid 9) did not exit within 2s\nhint: see %s\n", e.home.LogPath())
	if e.err() != want {
		t.Errorf("stderr %q, want %q", e.err(), want)
	}
	if e.spawner.count() != 0 {
		t.Errorf("spawned %d times, want 0", e.spawner.count())
	}
	if got := e.clock.elapsed(start); got != 2*time.Second {
		t.Errorf("waited %v, want the 2s timeout", got)
	}
}

func TestUT123_DrainingTimeoutJSON(t *testing.T) {
	e := newEnv(t)
	e.api.statusFn = func(int) (contract.Status, error) { return contract.Status{State: "draining", PID: 9}, nil }
	if code := e.run("daemon", "start", "--timeout", "1s", "-o", "json"); code != 69 {
		t.Fatalf("exit %d, want 69", code)
	}
	got := e.wireError()
	if got.Code != contract.CodeDaemonStartTimeout || got.Details != nil {
		t.Errorf("error %+v, want daemon_start_timeout with no details", got)
	}
}

// UT-123: a daemon that holds the lock but does not answer (booting before it listens, or
// exiting) is waited for; the start then spawns when the home is free.
func TestUT123_UnreachableButLockedWaitsForTheLock(t *testing.T) {
	e := newEnv(t)
	e.probe.freeAfter = 3
	e.probe.pid = 31
	e.api.statusFn = func(call int) (contract.Status, error) {
		if call <= 4 {
			return contract.Status{}, noDaemonError("fake")
		}
		return runningStatus(42), nil
	}
	if code := e.run("daemon", "start"); code != 0 {
		t.Fatalf("exit %d, stderr %q", code, e.err())
	}
	if want := "waiting for the previous daemon (pid 31) to exit\n"; e.err() != want {
		t.Errorf("stderr %q, want %q", e.err(), want)
	}
	if e.spawner.count() != 1 {
		t.Errorf("spawned %d times, want 1", e.spawner.count())
	}
}

// UT-123: the holder of the lock turns out to be a daemon that finished booting: it is
// reported as running, not waited for and not replaced.
func TestUT123_UnreachableButLockedThenRunningIsAlreadyRunning(t *testing.T) {
	e := newEnv(t)
	e.probe.freeAfter = -1
	e.api.statusFn = func(call int) (contract.Status, error) {
		if call <= 3 {
			return contract.Status{}, noDaemonError("fake")
		}
		return runningStatus(31), nil
	}
	if code := e.run("daemon", "start"); code != 0 {
		t.Fatalf("exit %d, stderr %q", code, e.err())
	}
	if !strings.HasSuffix(e.out(), "harness daemon already running (pid 31)\n") {
		t.Errorf("stdout %q", e.out())
	}
	if e.spawner.count() != 0 {
		t.Errorf("spawned %d times, want 0", e.spawner.count())
	}
}

// UT-123: a wait for a daemon to start that never ends is a start timeout, without a child
// of ours to stop.
func TestUT123_StartingForeverIsAStartTimeout(t *testing.T) {
	e := newEnv(t)
	e.api.statusFn = func(int) (contract.Status, error) { return contract.Status{State: "starting", PID: 7}, nil }
	if code := e.run("daemon", "start", "--timeout", "1s", "-o", "json"); code != 69 {
		t.Fatalf("exit %d, want 69", code)
	}
	got := e.wireError()
	if got.Code != contract.CodeDaemonStartTimeout || got.Message != "daemon (pid 7) did not become ready within 1s" {
		t.Errorf("error %+v", got)
	}
	if e.spawner.count() != 0 {
		t.Errorf("spawned %d times, want 0", e.spawner.count())
	}
}

// Ctrl-C ends a wait for the home with the interrupt exit code.
func TestUT123_CtrlCEndsTheWait(t *testing.T) {
	e := newEnv(t)
	e.api.statusFn = func(int) (contract.Status, error) { return contract.Status{State: "draining", PID: 9}, nil }
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if code := e.runCtx(ctx, "daemon", "start"); code != 130 {
		t.Fatalf("exit %d, want 130", code)
	}
}

// UT-124: the daemon ignores SIGTERM. The CLI sends it to the process group, waits 2 s,
// sends SIGKILL to the group, reaps the child, then reports daemon_start_timeout with the tail.
func TestUT124_KillEscalationOnTimeout(t *testing.T) {
	e := newEnv(t)
	e.api.statusFn = func(int) (contract.Status, error) { return contract.Status{}, noDaemonError("fake") }
	start := e.clock.Now()
	type sent struct {
		sig syscall.Signal
		at  time.Duration
	}
	var signals []sent
	e.spawner.onJump = func(spec spawnSpec, p *fakeProc) {
		spec.Out.WriteString("booting\nstep 3 done\n")
		p.onSignal = func(p *fakeProc, sig syscall.Signal) {
			signals = append(signals, sent{sig, e.clock.elapsed(start)})
			if sig == syscall.SIGKILL {
				p.exit(-1) // SIGTERM is ignored; SIGKILL cannot be
			}
		}
	}

	code := e.run("daemon", "start", "--timeout", "3s", "-o", "json")
	if code != 69 {
		t.Fatalf("exit %d, want 69; stderr %q", code, e.err())
	}
	if len(signals) != 2 || signals[0].sig != syscall.SIGTERM || signals[1].sig != syscall.SIGKILL {
		t.Fatalf("signals %+v, want SIGTERM then SIGKILL", signals)
	}
	if gap := signals[1].at - signals[0].at; gap != 2*time.Second {
		t.Errorf("SIGKILL came %v after SIGTERM, want 2s", gap)
	}
	select {
	case <-e.spawner.proc.Done():
	default:
		t.Error("the child was not reaped before the CLI reported")
	}
	got := e.wireError()
	if got.Code != contract.CodeDaemonStartTimeout ||
		got.Message != "daemon did not become ready within 3s (pid 42); it was stopped" {
		t.Errorf("error %+v", got)
	}
	var details struct {
		PID        int      `json:"pid"`
		OutputTail []string `json:"output_tail"`
	}
	raw, _ := json.Marshal(got.Details)
	if err := json.Unmarshal(raw, &details); err != nil || details.PID != 42 ||
		strings.Join(details.OutputTail, "|") != "booting|step 3 done" {
		t.Errorf("details %s: %v", raw, err)
	}
}

// UT-124: a daemon that honours SIGTERM is not killed.
func TestUT124_SigtermIsEnoughWhenTheDaemonExits(t *testing.T) {
	e := newEnv(t)
	e.api.statusFn = func(int) (contract.Status, error) { return contract.Status{}, noDaemonError("fake") }
	e.spawner.onJump = func(_ spawnSpec, p *fakeProc) {
		p.onSignal = func(p *fakeProc, sig syscall.Signal) {
			if sig == syscall.SIGTERM {
				p.exit(0)
			}
		}
	}
	if code := e.run("daemon", "start", "--timeout", "1s", "-o", "json"); code != 69 {
		t.Fatalf("exit %d, want 69", code)
	}
	if got := e.spawner.proc.sent(); len(got) != 1 || got[0] != syscall.SIGTERM {
		t.Errorf("signals %v, want only SIGTERM", got)
	}
}

// UT-124: a child that survives SIGKILL for longer than the reap bound does not hang the CLI:
// the timeout is reported anyway.
func TestUT124_AChildStuckInTheKernelDoesNotHangTheCLI(t *testing.T) {
	e := newEnv(t)
	e.api.statusFn = func(int) (contract.Status, error) { return contract.Status{}, noDaemonError("fake") }
	e.spawner.onJump = func(_ spawnSpec, p *fakeProc) { p.onSignal = func(*fakeProc, syscall.Signal) {} }
	start := e.clock.Now()

	if code := e.run("daemon", "start", "--timeout", "1s", "-o", "json"); code != 69 {
		t.Fatalf("exit %d, want 69", code)
	}
	if got := e.wireError(); got.Code != contract.CodeDaemonStartTimeout {
		t.Errorf("error %+v", got)
	}
	if got := e.clock.elapsed(start); got != 1*time.Second+termGrace+killGrace {
		t.Errorf("gave up after %v of virtual time", got)
	}
}

// UT-126: stop with nothing on the socket and the lock held waits until the lock is free,
// then reports the daemon stopped: the PID is the one in the lock file, the uptime is null.
func TestUT126_StopUnreachableButLockedWaitsForTheLock(t *testing.T) {
	e := newEnv(t)
	e.probe.freeAfter = 3
	e.probe.pid = 48211
	start := e.clock.Now()

	if code := e.run("daemon", "stop", "-o", "json"); code != 0 {
		t.Fatalf("exit %d, stderr %q", code, e.err())
	}
	if want := `{"daemon":{"stopped":true,"pid":48211,"uptime_seconds":null}}` + "\n"; e.out() != want || e.err() != "" {
		t.Errorf("stdout %q stderr %q, want %q", e.out(), e.err(), want)
	}
	if got := e.clock.elapsed(start); got != 300*time.Millisecond {
		t.Errorf("waited %v, want three 100 ms polls", got)
	}
}

func TestUT126_HumanStopWithoutAnUptime(t *testing.T) {
	e := newEnv(t)
	e.probe.freeAfter = 1
	e.probe.pid = 48211
	if code := e.run("daemon", "stop"); code != 0 {
		t.Fatalf("exit %d, stderr %q", code, e.err())
	}
	if want := "harness daemon stopped (pid 48211)\n"; e.out() != want {
		t.Errorf("stdout %q, want %q", e.out(), want)
	}
}

// With no PID in the lock file the document says null rather than inventing one.
func TestUT126_UnknownPIDReadsAsNull(t *testing.T) {
	e := newEnv(t)
	e.probe.freeAfter = 1
	e.probe.pid = 0
	if code := e.run("daemon", "stop", "-o", "json"); code != 0 {
		t.Fatalf("exit %d, stderr %q", code, e.err())
	}
	if want := `{"daemon":{"stopped":true,"pid":null,"uptime_seconds":null}}` + "\n"; e.out() != want {
		t.Errorf("stdout %q, want %q", e.out(), want)
	}
	e.probe.lockCalls = 0
	e.run("daemon", "stop")
	if want := "harness daemon stopped\n"; e.out() != want {
		t.Errorf("human stdout %q, want %q", e.out(), want)
	}
}

// UT-126: a holder that never releases the home is a stop timeout after the default grace
// plus 5 s, naming the PID from the lock file.
func TestUT126_StopUnreachableButLockedTimesOut(t *testing.T) {
	e := newEnv(t)
	e.probe.freeAfter = -1
	e.probe.pid = 48211
	start := e.clock.Now()

	if code := e.run("daemon", "stop"); code != 69 {
		t.Fatalf("exit %d, want 69", code)
	}
	want := "error: daemon (pid 48211) did not exit within 35s\nhint: see " + e.home.LogPath() + "\n"
	if e.err() != want {
		t.Errorf("stderr %q, want %q", e.err(), want)
	}
	if got := e.clock.elapsed(start); got != 35*time.Second {
		t.Errorf("gave up after %v, want 35s", got)
	}
}

// UT-126: a daemon that was booting reaches its socket while the stop waits. The stop asks
// again, cancels that boot, and then waits for it like any other stop.
func TestUT126_ABootThatReachesItsSocketIsStopped(t *testing.T) {
	e := newEnv(t)
	e.probe.freeAfter = 6
	e.probe.pid = 31
	e.api.shutdownFn = func() (contract.ShutdownInfo, error) {
		if e.api.shutdownCalls.Load() < 3 {
			return contract.ShutdownInfo{}, noDaemonError("fake")
		}
		return contract.ShutdownInfo{Stopping: true, PID: 31, UptimeSeconds: 1, ShutdownGrace: "30s"}, nil
	}
	if code := e.run("daemon", "stop", "-o", "json"); code != 0 {
		t.Fatalf("exit %d, stderr %q", code, e.err())
	}
	if want := `{"daemon":{"stopped":true,"pid":31,"uptime_seconds":1}}` + "\n"; e.out() != want {
		t.Errorf("stdout %q, want %q", e.out(), want)
	}
}

// A detached child that fails has already written its error line: the CLI only exits with
// the daemon's code and adds nothing after the line.
func TestDetachedChildReportsNothingAfterItsFinalErrorLine(t *testing.T) {
	e := newEnv(t)
	e.app.runDaemon = func(context.Context, daemon.Options) error {
		return &daemon.ExitError{Code: 78, ErrCode: contract.CodeConfigInvalid, Message: "config invalid: x"}
	}
	if code := e.run("daemon", "start", "--foreground", "--detached"); code != 78 {
		t.Fatalf("exit %d, want 78", code)
	}
	if e.err() != "" || e.out() != "" {
		t.Errorf("stderr %q stdout %q, want both empty", e.err(), e.out())
	}
}
