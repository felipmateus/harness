package daemon

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/felipmateus/harness/internal/admission"
)

// shutdownStep is one of the ten steps of the shutdown sequence (Safety Invariant 13).
type shutdownStep struct {
	n    int
	name string
	run  func(ctx context.Context) error
}

// shutdownSteps lists the steps in their fixed order. The socket file is removed at step 8
// only: the listener closes at step 6 without unlinking it.
func (d *daemon) shutdownSteps(reason string) []shutdownStep {
	return []shutdownStep{
		{1, "draining", func(context.Context) error {
			// The status and shutdown routes keep answering; every other route is refused.
			d.gate.Set(admission.Draining)
			return nil
		}},
		{2, "stopping", func(ctx context.Context) error {
			return d.record(ctx, typeStopping, stoppingData{PID: d.pid, Reason: reason})
		}},
		{3, "drain", d.gate.Wait},
		{4, "stopped", func(ctx context.Context) error {
			return d.record(ctx, typeStopped, stoppedData{PID: d.pid, UptimeSeconds: d.uptimeSeconds()})
		}},
		{5, "broker", func(ctx context.Context) error {
			// Every follow ends with ErrClosed and every SSE stream writes its final frame.
			d.hub.Close()
			d.drainStreams(ctx)
			return nil
		}},
		{6, "server", d.shutdownServer},
		{7, "ledger", d.led.Close},
		{8, "files", func(context.Context) error { return d.removeFiles() }},
		{9, "lock", func(context.Context) error { return d.releaseLock() }},
		{10, "stopped log", func(context.Context) error {
			d.log().Info("daemon stopped", "reason", reason, "uptime_seconds", d.uptimeSeconds())
			return nil
		}},
	}
}

// drainStreams waits for the event streams that the broker close just ended to write their
// shutdown frame and return. Step 6 cancels the stream contexts, and a stream that is
// cancelled before it has written its frame never writes it, so the frame is the reason
// this wait exists. A stream that is still open after streamFinalFrameGrace is blocked on a
// client that stopped reading; its connection is closed, so no follower can hold the
// shutdown (Safety Invariant 8).
func (d *daemon) drainStreams(ctx context.Context) {
	waitCtx, cancel := context.WithTimeout(ctx, streamFinalFrameGrace)
	defer cancel()
	if d.conns.waitIdle(waitCtx) == nil {
		return
	}
	if n := d.conns.closeActive(); n > 0 {
		d.log().Warn("streams cut at shutdown", "connections", n)
	}
}

// shutdown runs the sequence inside shutdown_grace, measured from the stop request.
//
// A step that fails (other than by running out of budget) is logged and the sequence goes
// on: a daemon that cannot record its own stop must still release the home. A budget that
// runs out, or a second signal, ends the sequence: see overrun.
func (d *daemon) shutdown() error {
	reason, _, _ := d.trig.state()
	ctx, cancel := d.budget()
	defer cancel()

	for _, s := range d.shutdownSteps(reason) {
		began := time.Now()
		outcome, err := d.runShutdownStep(ctx, cancel, s)
		switch outcome {
		case stepAborted:
			d.log().Error("shutdown interrupted by a second signal", "step", s.n, "name", s.name)
			d.force()
			return &ExitError{
				Code:    exitFailure,
				ErrCode: "shutdown_aborted",
				Message: fmt.Sprintf("shutdown interrupted by a second signal at step %d (%s)", s.n, s.name),
			}
		case stepOverran:
			d.log().Error("shutdown budget exceeded", "step", s.n, "name", s.name, "budget", d.grace().String())
			d.force()
			return &ExitError{
				Code:    exitFailure,
				ErrCode: "shutdown_budget_exceeded",
				Message: fmt.Sprintf("shutdown exceeded its budget of %s at step %d (%s)", d.grace(), s.n, s.name),
			}
		}
		if err != nil {
			d.log().Error("shutdown step failed", "step", s.n, "name", s.name, "err", err)
		}
		d.log().Debug("shutdown step", "step", s.n, "name", s.name, "duration_ms", time.Since(began).Milliseconds())
	}
	return nil
}

type stepOutcome int

const (
	stepDone stepOutcome = iota
	stepOverran
	stepAborted
)

// runShutdownStep runs the test hook and then the step in a goroutine, and waits for it, for
// the end of the budget, or for a second signal. When the daemon gives up on a step the
// goroutine is abandoned: it never starts the step's action after a held hook returns, and
// the actions themselves return once the (cancelled or expired) ctx ends.
func (d *daemon) runShutdownStep(ctx context.Context, cancel context.CancelFunc, s shutdownStep) (stepOutcome, error) {
	var abandoned atomic.Bool
	result := make(chan error, 1)
	go func() {
		if hook := d.hooks.BeforeShutdownStep; hook != nil {
			hook(s.n)
			if abandoned.Load() {
				result <- nil
				return
			}
		}
		result <- s.run(ctx)
	}()

	select {
	case err := <-result:
		if err != nil && ctx.Err() != nil && errors.Is(err, ctx.Err()) {
			return stepOverran, err // the step itself stopped at the end of the budget
		}
		return stepDone, err
	case <-ctx.Done():
		abandoned.Store(true)
		return stepOverran, ctx.Err()
	case <-d.abort:
		abandoned.Store(true)
		cancel() // ends every step that waits on ctx
		return stepAborted, nil
	}
}

// ledgerCloseWait bounds how long the forced shutdown waits for the ledger to finish
// closing before it removes the files and releases the lock. The close it waits for runs
// with an expired context: it aborts the transaction in flight and closes the connections,
// so it takes milliseconds. The bound is for a close that does not return at all (a disk
// that hangs); it matches the kill escalation grace of `harness daemon start`.
const ledgerCloseWait = 2 * time.Second

// ledgerCloser is the part of the ledger the forced shutdown uses.
type ledgerCloser interface {
	Close(ctx context.Context) error
	Closed() <-chan struct{}
}

// force ends a shutdown that cannot finish: it drops the listener and every connection,
// closes the ledger without waiting for the queue to drain (the transaction in flight is
// aborted and rolled back, so the ledger stays consistent), removes the daemon's files and
// releases the lock. It is the path of the budget overrun and of the second signal; both
// exit with code 1.
//
// The lock is released only once the ledger is closed. When step 7 is what overran, its own
// Close is still running, and the Close called here with an expired context returns at once
// instead of waiting for it; the writer and the connections are shut only when Closed is
// closed. Until then this process may still write harness.db, and only the lock owner may
// (Safety Invariant 1). If the close has not finished within ledgerCloseWait the lock is
// kept: the process is about to exit with code 1, and the exit releases the lock and ends
// every write at the same instant.
//
// An overrun before step 4 leaves no daemon.stopped, so the next start reports an unclean
// shutdown. After step 4 the ledger is clean and only the log records the overrun.
func (d *daemon) force() {
	var led ledgerCloser
	if d.led != nil {
		led = d.led
	}
	d.forceDown(led, ledgerCloseWait)
}

// forceDown is force with the ledger and the wait given.
func (d *daemon) forceDown(led ledgerCloser, wait time.Duration) {
	if d.httpSrv != nil {
		d.httpSrv.Close()
	}
	if d.streamCancel != nil {
		d.streamCancel()
	}
	if d.hub != nil {
		d.hub.Close()
	}
	ledgerClosed := led == nil || d.closeLedgerByForce(led, wait)
	if err := d.removeFiles(); err != nil {
		d.log().Warn("files not removed", "err", err)
	}
	if !ledgerClosed {
		d.log().Error("lock kept: the ledger did not finish closing", "wait", wait.String())
		return
	}
	if err := d.releaseLock(); err != nil {
		d.log().Warn("lock not released cleanly", "err", err)
	}
}

// closeLedgerByForce closes led with an expired context and waits up to wait for the close
// to be complete, whoever started it. It reports whether the ledger is closed.
func (d *daemon) closeLedgerByForce(led ledgerCloser, wait time.Duration) bool {
	expired, cancel := context.WithCancel(context.Background())
	cancel()
	if err := led.Close(expired); err != nil {
		d.log().Warn("ledger closed by force", "err", err)
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-led.Closed():
		return true
	case <-timer.C:
		return false
	}
}

// releaseLock gives the home up at the end of a shutdown. The BlockExit test hook keeps it,
// to simulate a daemon that never exits.
func (d *daemon) releaseLock() error {
	if d.hooks.BlockExit {
		return nil
	}
	return d.lock.Release()
}

// blockForever never returns. It polls a timer instead of waiting on a channel, so the
// runtime's deadlock detector does not end the process.
func blockForever() {
	for {
		time.Sleep(time.Hour)
	}
}

// removeFiles removes daemon.json, then the socket. Only the lock owner calls it.
func (d *daemon) removeFiles() error {
	return errors.Join(removeIfExists(d.home.InfoPath()), removeIfExists(d.home.SocketPath()))
}
