package cli

import (
	"context"
	"errors"
	"fmt"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/felipmateus/harness/internal/api/contract"
	"github.com/felipmateus/harness/internal/daemon"
	"github.com/felipmateus/harness/internal/home"
	"github.com/felipmateus/harness/internal/version"
)

// Timings of a detached start.
const (
	defaultStartTimeout = 10 * time.Second
	startPollInterval   = 100 * time.Millisecond
	// termGrace is how long a daemon that was told to stop (SIGTERM) gets to exit when its
	// start timed out, before it is killed.
	termGrace = 2 * time.Second
	// killGrace bounds the wait for a killed child to be reaped: SIGKILL cannot be ignored,
	// so this only covers a process stuck in the kernel.
	killGrace = 5 * time.Second
)

func (a *app) newDaemonCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "daemon",
		Short: "Start and stop the harness daemon",
		// Runnable, so that an unknown verb fails argument validation (a usage error)
		// instead of printing the help page and exiting 0.
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error { return cmd.Help() },
	}
	cmd.AddCommand(a.newStartCmd(), a.newStopCmd())
	return cmd
}

func (a *app) newStartCmd() *cobra.Command {
	var foreground, detached bool
	var timeout time.Duration
	cmd := &cobra.Command{
		Use:   "start",
		Short: "Start the daemon in the background and wait until it is running",
		Long: "Start the daemon in the background and wait until it reports running. Starting is\n" +
			"idempotent: a daemon that already runs is reported, not restarted.\n\n" +
			"With --foreground the daemon runs in this process, logs go to stderr, and\n" +
			"Ctrl-C, SIGTERM or SIGHUP stop it gracefully.",
		Args: cobra.NoArgs,
		RunE: a.run(func(cmd *cobra.Command) error {
			if detached && !foreground {
				return usageError(cmd.CommandPath(), "--detached requires --foreground")
			}
			h, err := a.resolveHome()
			if err != nil {
				return err
			}
			if foreground {
				return a.runForeground(cmd.Context(), h, detached)
			}
			res, err := a.startDetached(cmd.Context(), h, timeout)
			if err != nil {
				return err
			}
			return a.printStarted(res)
		}),
	}
	f := cmd.Flags()
	f.BoolVar(&foreground, "foreground", false, "run the daemon in this process instead of in the background")
	f.BoolVar(&detached, "detached", false, "internal: the background child started by `daemon start`")
	_ = f.MarkHidden("detached")
	f.DurationVar(&timeout, "timeout", defaultStartTimeout, "how long to wait for the daemon to become ready")
	return cmd
}

// runForeground runs the daemon in this process. daemon.Run owns the signals and both the
// boot and the shutdown sequences; its failures come back as *daemon.ExitError, which the
// error path turns into the documented message and exit code. A detached child reports
// the failure itself, in one JSON line, and only exits with the code.
//
// The foreground daemon process takes SIGPIPE from here until execute is done, not only for
// the length of Run: the error path writes Run's failure to stderr after Run has returned,
// and a stderr reader that has gone away would turn that write into a SIGPIPE death instead
// of the documented exit code (`_spec.md` Shutdown overruns).
func (a *app) runForeground(ctx context.Context, h home.Home, detached bool) error {
	a.holdUntilExit(daemon.TakeSIGPIPE())
	mode := daemon.Interactive
	if detached {
		mode = daemon.Detached
	}
	err := a.runDaemon(ctx, daemon.Options{
		Home:    h,
		Mode:    mode,
		Stderr:  a.stderr,
		Version: version.Get(),
	})
	var exit *daemon.ExitError
	if detached && errors.As(err, &exit) {
		// The daemon has written its final error line to daemon.out, which is where the CLI
		// that spawned this child looks for it. Another line would bury it.
		return &silentExit{code: exit.Code}
	}
	return err
}

// startResult is what a successful start reports.
type startResult struct {
	PID            int
	Socket         string
	AlreadyRunning bool
}

// startedDocument is the `-o json` form of a start.
type startedDocument struct {
	Daemon startedDaemon `json:"daemon"`
}

type startedDaemon struct {
	PID            int    `json:"pid"`
	State          string `json:"state"`
	Socket         string `json:"socket"`
	AlreadyRunning bool   `json:"already_running"`
}

func (a *app) printStarted(res startResult) error {
	p := a.printer()
	if p.mode.structured() {
		return p.document(startedDocument{Daemon: startedDaemon{
			PID: res.PID, State: "running", Socket: res.Socket, AlreadyRunning: res.AlreadyRunning,
		}})
	}
	if res.AlreadyRunning {
		p.line("harness daemon already running (pid %d)", res.PID)
		return nil
	}
	p.line("harness daemon started (pid %d)", res.PID)
	p.line("socket: %s", res.Socket)
	return nil
}

// startDetached starts the daemon in the background and returns once it reports running
// with the PID of the child it spawned (`_spec.md` Detached start).
//
// The home is checked first and locally, with no daemon contacted and nothing spawned:
// the socket path must fit the OS limit, an existing home must be private, and a missing
// one is created at 0700. Then settleHome decides what to do about whatever already uses
// the home (the decision table below). Only a free home gets a child: `daemon start
// --foreground --detached` in its own session, with its output appended to logs/daemon.out.
// The status is polled until that child is the one that runs, exits, or the timeout, which
// bounds the whole command, ends it.
//
// A child that finds the home owned by another daemon (`daemon_already_running`) is not a
// failure: another start won the race, and this one goes back to the decision table.
func (a *app) startDetached(ctx context.Context, h home.Home, timeout time.Duration) (startResult, error) {
	w := &startWait{deadline: a.clock.Now().Add(timeout), timeout: timeout}

	if err := preSpawnChecks(h); err != nil {
		return startResult{}, err
	}

	api := a.newClient(h.SocketPath())
	for round := 0; ; round++ {
		if round > 0 && !a.clock.Now().Before(w.deadline) {
			return startResult{}, a.waitTimedOut(h, fmt.Sprintf("daemon did not become ready within %s", timeout))
		}
		res, settled, err := a.settleHome(ctx, h, api, w)
		if err != nil || settled {
			return res, err
		}

		out, offset, err := openDaemonOut(h)
		if err != nil {
			return startResult{}, err
		}
		exe, err := a.executable()
		if err != nil {
			out.Close()
			return startResult{}, fmt.Errorf("find the harness executable: %w", err)
		}
		proc, err := a.spawner.Spawn(spawnSpec{
			Exe:  exe,
			Args: []string{"daemon", "start", "--foreground", "--detached", "--home", h.Dir},
			Dir:  h.Dir,
			Out:  out,
		})
		out.Close() // the child holds its own descriptor
		if err != nil {
			return startResult{}, &contract.Error{
				Code:    contract.CodeDaemonStartFailed,
				Message: "could not start the daemon: " + err.Error(),
				Hint:    "see " + h.LogPath(),
			}
		}

		res, err = a.awaitChild(ctx, h, api, proc, offset, w)
		if errors.Is(err, errHomeOwned) {
			continue
		}
		return res, err
	}
}

// The states the status route reports that a start tells apart.
const (
	daemonRunning  = "running"
	daemonStarting = "starting"
)

// startWait is what a start knows about its own deadline and what it has told the operator.
type startWait struct {
	deadline  time.Time
	timeout   time.Duration
	announced bool // the "waiting for the previous daemon" line was printed
}

// errHomeOwned says the child that was spawned found another daemon on the home. It sends
// the start back to the decision table and is never shown.
var errHomeOwned = errors.New("another daemon owns the home")

// settleHome is the start decision table (`_spec.md` Detached start, step 2). It looks at
// whatever uses the home and returns when there is a result, an error, or a free home:
//
//	status running                     -> already running; nothing is spawned
//	status starting                    -> another start is in progress: wait for it
//	status draining                    -> a daemon is going away: wait until it has released the home
//	unreachable, lock held             -> a daemon that is booting or exiting: wait the same way
//	unreachable, lock free             -> the home is free: spawn
//
// Waiting never ends in "already running" unless the status itself says running, so a
// daemon that is going away is never reported as the one that runs. Every wait is bounded
// by the start's deadline, with daemon_start_timeout and nothing to stop.
func (a *app) settleHome(ctx context.Context, h home.Home, api daemonAPI, w *startWait) (res startResult, settled bool, err error) {
	probe := a.probe(h)
	for {
		var timeoutMsg string
		st, err := api.Status(ctx)
		switch {
		case err == nil && st.State == daemonRunning:
			return startResult{PID: st.PID, Socket: socketOf(st, h), AlreadyRunning: true}, true, nil
		case err == nil && st.State == daemonStarting:
			timeoutMsg = fmt.Sprintf("daemon (pid %d) did not become ready within %s", st.PID, w.timeout)
		case err == nil:
			w.announce(a, st.PID)
			timeoutMsg = fmt.Sprintf("previous daemon (%s) did not exit within %s", pidText(st.PID), w.timeout)
		case !isUnavailable(err):
			return startResult{}, false, err
		default:
			free, lockErr := probe.LockFree()
			if lockErr != nil {
				return startResult{}, false, lockErr
			}
			if free {
				return startResult{}, false, nil
			}
			pid := probe.LockPID()
			w.announce(a, pid)
			timeoutMsg = fmt.Sprintf("previous daemon (%s) did not exit within %s", pidText(pid), w.timeout)
		}

		if !a.clock.Now().Before(w.deadline) {
			return startResult{}, false, a.waitTimedOut(h, timeoutMsg)
		}
		if err := sleep(ctx, a.clock, startPollInterval); err != nil {
			return startResult{}, false, errInterrupted
		}
	}
}

// announce prints the progress line of a start that waits for a daemon to exit. It goes to
// stderr, once, and only in human mode (`_dx.md`: a daemon is stopping).
func (w *startWait) announce(a *app, pid int) {
	if w.announced || a.mode.structured() {
		return
	}
	w.announced = true
	fmt.Fprintf(a.stderr, "waiting for the previous daemon (%s) to exit\n", pidText(pid))
}

func pidText(pid int) string {
	if pid <= 0 {
		return "pid unknown"
	}
	return fmt.Sprintf("pid %d", pid)
}

// waitTimedOut is the timeout of a start that never got to spawn, or that gave the home up
// again and again: there is no child to stop and no output to show.
func (a *app) waitTimedOut(h home.Home, message string) error {
	return &contract.Error{
		Code:    contract.CodeDaemonStartTimeout,
		Message: message,
		Hint:    "see " + h.LogPath(),
	}
}

// awaitChild polls the status until the child is the daemon that runs. It ends when the
// child exits (reported by childExited) or the deadline passes (the child is stopped).
func (a *app) awaitChild(ctx context.Context, h home.Home, api daemonAPI, proc process, offset int64, w *startWait) (startResult, error) {
	for {
		select {
		case <-proc.Done():
			return startResult{}, a.childExited(h, proc, offset)
		default:
		}

		st, err := api.Status(ctx)
		if err == nil && st.State == daemonRunning && st.PID == proc.PID() {
			return startResult{PID: proc.PID(), Socket: socketOf(st, h)}, nil
		}
		// Any other answer, including an error, means not ready yet: the child's exit or the
		// timeout ends the wait, and neither leaves the child behind.

		if !a.clock.Now().Before(w.deadline) {
			return startResult{}, a.startTimedOut(h, proc, offset, w.timeout)
		}
		select {
		case <-proc.Done():
		case <-a.clock.After(startPollInterval):
		case <-ctx.Done():
			return startResult{}, errInterrupted
		}
	}
}

// preSpawnChecks validates the home before anything is spawned or contacted: the socket
// path length, then the security of an existing home, then the layout.
func preSpawnChecks(h home.Home) error {
	if err := h.CheckSocketPath(); err != nil {
		return err
	}
	if err := h.CheckSecurity(); err != nil {
		return err
	}
	return h.EnsureLayout()
}

func socketOf(st contract.Status, h home.Home) string {
	if st.Socket != "" {
		return st.Socket
	}
	return h.SocketPath()
}

// childExited reports a child that exited before it became ready. The last error line it
// wrote after the offset decides: a failure the daemon recognises is re-raised with its own
// code and exit status, `daemon_already_running` sends the start back to the decision table
// (errHomeOwned), and anything else, or no line at all, is daemon_start_failed with the
// child's PID, its exit code and the last 20 lines of its output.
func (a *app) childExited(h home.Home, proc process, offset int64) error {
	lines := readOutput(h.DaemonOutPath(), offset)
	if ce, ok := lastChildError(lines); ok {
		switch {
		case ce.reraisable():
			return ce.wire()
		case ce.alreadyRunning():
			return errHomeOwned
		}
	}
	if len(lines) > tailLines {
		lines = lines[len(lines)-tailLines:]
	}
	code := proc.ExitCode()
	return &contract.Error{
		Code:    contract.CodeDaemonStartFailed,
		Message: fmt.Sprintf("daemon exited during startup (pid %d, exit %d)", proc.PID(), code),
		Hint:    "see " + h.LogPath(),
		Details: contract.StartFailedDetails{
			PID:        proc.PID(),
			ExitCode:   code,
			OutputTail: lines,
		},
	}
}

// startTimedOut stops the child that did not become ready in time and reports it. No
// daemon is left behind: SIGTERM to the child's process group, a 2 s wait, then SIGKILL to
// the group and a wait for the child to be reaped.
func (a *app) startTimedOut(h home.Home, proc process, offset int64, timeout time.Duration) error {
	a.stopChild(proc)
	return &contract.Error{
		Code:    contract.CodeDaemonStartTimeout,
		Message: fmt.Sprintf("daemon did not become ready within %s (pid %d); it was stopped", timeout, proc.PID()),
		Hint:    "see " + h.LogPath(),
		Details: contract.StartTimeoutDetails{
			PID:        proc.PID(),
			OutputTail: readTail(h.DaemonOutPath(), offset, tailLines),
		},
	}
}

// stopChild ends a child that is not wanted: SIGTERM, which a booting daemon honours at
// its next step boundary, then SIGKILL after termGrace. The child is reaped when this
// returns, unless it is stuck in the kernel for longer than killGrace.
func (a *app) stopChild(proc process) {
	_ = proc.Signal(syscall.SIGTERM)
	if a.waitExit(proc, termGrace) {
		return
	}
	_ = proc.Signal(syscall.SIGKILL)
	a.waitExit(proc, killGrace)
}

// waitExit waits up to d for the child to exit, and reports whether it has. An exit that
// already happened wins over an elapsed wait.
func (a *app) waitExit(proc process, d time.Duration) bool {
	select {
	case <-proc.Done():
		return true
	default:
	}
	select {
	case <-proc.Done():
		return true
	case <-a.clock.After(d):
	}
	select {
	case <-proc.Done():
		return true
	default:
		return false
	}
}
