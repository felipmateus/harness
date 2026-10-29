package cli

import (
	"context"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/felipmateus/harness/internal/api/contract"
	"github.com/felipmateus/harness/internal/home"
)

const (
	// stopPollInterval is how often a stop looks at the lock and the socket.
	stopPollInterval = 100 * time.Millisecond
	// stopMargin is added to the daemon's shutdown_grace: the time a daemon that used its
	// whole budget still needs to leave.
	stopMargin = 5 * time.Second
	// defaultGrace is used when the daemon's answer carries no readable shutdown_grace.
	defaultGrace = 30 * time.Second
)

func (a *app) newStopCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "stop",
		Short: "Stop the daemon and wait until it has released the home",
		Long: "Request a graceful shutdown and wait until the daemon's lock is free and its\n" +
			"socket is gone. Stopping is idempotent: with no daemon it reports so and exits 0.",
		Args: cobra.NoArgs,
		RunE: a.run(func(cmd *cobra.Command) error {
			h, err := a.resolveHome()
			if err != nil {
				return err
			}
			res, err := a.stopDaemon(cmd.Context(), h)
			if err != nil {
				return err
			}
			return a.printStopped(res)
		}),
	}
}

// stopResult is what a stop reports.
type stopResult struct {
	Stopped bool
	// PID is the daemon that stopped; 0 when it could not be learned.
	PID int
	// Uptime is the daemon's uptime in seconds at the stop request; nil when the daemon never
	// answered (it was still booting, or already exiting), which reads as null.
	Uptime *int64
	Reason string
}

// stoppedDocument is the `-o json` form of a stop that stopped a daemon. A daemon that never
// answered has no known uptime, and one whose lock file was unreadable has no known PID: both
// read as null.
type stoppedDocument struct {
	Daemon stoppedDaemon `json:"daemon"`
}

type stoppedDaemon struct {
	Stopped       bool   `json:"stopped"`
	PID           *int   `json:"pid"`
	UptimeSeconds *int64 `json:"uptime_seconds"`
}

// notRunningDocument is the `-o json` form of a stop that found no daemon.
type notRunningDocument struct {
	Daemon notRunningDaemon `json:"daemon"`
}

type notRunningDaemon struct {
	Stopped bool   `json:"stopped"`
	Reason  string `json:"reason"`
}

const reasonNotRunning = "not_running"

func (a *app) printStopped(res stopResult) error {
	p := a.printer()
	if p.mode.structured() {
		if !res.Stopped {
			return p.document(notRunningDocument{Daemon: notRunningDaemon{Stopped: false, Reason: res.Reason}})
		}
		d := stoppedDaemon{Stopped: true, UptimeSeconds: res.Uptime}
		if res.PID > 0 {
			d.PID = &res.PID
		}
		return p.document(stoppedDocument{Daemon: d})
	}
	switch {
	case !res.Stopped:
		p.line("harness daemon not running")
	case res.PID <= 0:
		p.line("harness daemon stopped")
	case res.Uptime == nil:
		p.line("harness daemon stopped (pid %d)", res.PID)
	default:
		p.line("harness daemon stopped (pid %d, uptime %s)", res.PID, time.Duration(*res.Uptime)*time.Second)
	}
	return nil
}

// stopDaemon asks the daemon to shut down and waits until it has released the home: its
// lock is free and its socket is gone (`_spec.md` Stop).
//
// The shutdown request is accepted in every state (while starting it cancels the boot,
// while draining it starts nothing new), so a stop is idempotent. With no daemon on the
// socket and a free lock there is nothing to stop; with no daemon on the socket and a held
// lock, a daemon is booting or exiting (stopUnreachable). The wait lasts the daemon's own
// shutdown_grace plus a margin, and the lock and the socket decide when it ends, never the
// process table.
func (a *app) stopDaemon(ctx context.Context, h home.Home) (stopResult, error) {
	api := a.newClient(h.SocketPath())
	info, err := api.Shutdown(ctx)
	if err != nil {
		if !isUnavailable(err) {
			return stopResult{}, err
		}
		return a.stopUnreachable(ctx, h, api)
	}
	return a.awaitStopped(ctx, h, info)
}

// awaitStopped waits for the daemon that accepted the shutdown request to release the home.
func (a *app) awaitStopped(ctx context.Context, h home.Home, info contract.ShutdownInfo) (stopResult, error) {
	grace := defaultGrace
	if d, perr := time.ParseDuration(info.ShutdownGrace); perr == nil && d > 0 {
		grace = d
	}
	wait := grace + stopMargin
	deadline := a.clock.Now().Add(wait)
	probe := a.probe(h)
	for {
		free, lockErr := probe.LockFree()
		if lockErr != nil {
			return stopResult{}, lockErr
		}
		if free && !probe.SocketExists() {
			uptime := info.UptimeSeconds
			return stopResult{Stopped: true, PID: info.PID, Uptime: &uptime}, nil
		}
		if !a.clock.Now().Before(deadline) {
			return stopResult{}, a.stopTimedOut(h, info.PID, wait)
		}
		if err := sleep(ctx, a.clock, stopPollInterval); err != nil {
			return stopResult{}, errInterrupted
		}
	}
}

// stopUnreachable handles a stop when nothing answers on the socket. A free lock means no
// daemon: nothing to stop. A held lock means a daemon owns the home without serving yet or
// any more: it is booting before its socket exists, or it is exiting.
//
// The stop waits for the lock to be released, for the default shutdown_grace plus the usual
// margin, since the grace the daemon would have reported is unknown. It asks again for the
// shutdown at every poll: a daemon that gets to its socket while the stop waits is then told
// to stop (a boot that is cancelled), instead of being waited on until the timeout. The PID is
// the one in the lock file, and the uptime is unknown.
func (a *app) stopUnreachable(ctx context.Context, h home.Home, api daemonAPI) (stopResult, error) {
	probe := a.probe(h)
	free, err := probe.LockFree()
	if err != nil {
		return stopResult{}, err
	}
	if free {
		return stopResult{Stopped: false, Reason: reasonNotRunning}, nil
	}

	wait := defaultGrace + stopMargin
	deadline := a.clock.Now().Add(wait)
	pid := probe.LockPID()
	for {
		if err := sleep(ctx, a.clock, stopPollInterval); err != nil {
			return stopResult{}, errInterrupted
		}
		info, serr := api.Shutdown(ctx)
		switch {
		case serr == nil:
			return a.awaitStopped(ctx, h, info)
		case !isUnavailable(serr):
			return stopResult{}, serr
		}
		free, err := probe.LockFree()
		if err != nil {
			return stopResult{}, err
		}
		if free {
			return stopResult{Stopped: true, PID: pid}, nil
		}
		if p := probe.LockPID(); p > 0 {
			pid = p
		}
		if !a.clock.Now().Before(deadline) {
			return stopResult{}, a.stopTimedOut(h, pid, wait)
		}
	}
}

func (a *app) stopTimedOut(h home.Home, pid int, wait time.Duration) error {
	return &contract.Error{
		Code:    contract.CodeDaemonStopTimeout,
		Message: fmt.Sprintf("daemon (pid %d) did not exit within %s", pid, wait),
		Hint:    "see " + h.LogPath(),
	}
}
