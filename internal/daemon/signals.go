package daemon

import (
	"context"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"
)

// trigger records why and when the daemon was first asked to stop. The first request wins:
// the reason of a later one is dropped, so the daemon.stopping event names the real cause.
type trigger struct {
	once   sync.Once
	fired  chan struct{} // closed by the first fire
	reason string        // written before fired is closed
	at     time.Time     // written before fired is closed
}

func newTrigger() *trigger { return &trigger{fired: make(chan struct{})} }

// fire records the first stop request and reports whether this call was it.
func (t *trigger) fire(reason string) bool {
	first := false
	t.once.Do(func() {
		first = true
		t.reason = reason
		t.at = time.Now()
		close(t.fired)
	})
	return first
}

// state reports the reason and time of the stop request, and whether one was made.
func (t *trigger) state() (reason string, at time.Time, ok bool) {
	select {
	case <-t.fired:
		return t.reason, t.at, true
	default:
		return "", time.Time{}, false
	}
}

// watch installs the signal handlers and watches ctx. It runs for the whole of Run, so a
// signal that arrives during boot cancels the boot instead of killing the process.
//
//   - ctx ending is a stop request with reason "context";
//   - the first SIGINT, SIGTERM or SIGHUP is a stop request with reason "signal";
//   - any signal once a stop is already under way is the second one: it closes d.abort, and
//     the shutdown gives up on the step it is in and exits 1 (the budget path, early);
//   - SIGPIPE is taken and dropped, see TakeSIGPIPE.
//
// The returned function removes the handlers and ends the watcher. The SIGPIPE handler is
// only needed for as long as Run runs: a caller that goes on writing to stderr after Run
// has returned (the foreground CLI reports Run's error there) takes SIGPIPE itself.
func (d *daemon) watch(ctx context.Context) (stop func()) {
	sigs := make(chan os.Signal, 4)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)

	releasePipes := TakeSIGPIPE()
	if ctx.Err() != nil {
		// Already cancelled: the boot must see the request at its very first boundary,
		// not whenever the watcher goroutine first runs.
		d.trig.fire(reasonContext)
	}
	quit := make(chan struct{})
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		ctxDone := ctx.Done()
		for {
			select {
			case <-ctxDone:
				d.trig.fire(reasonContext)
				ctxDone = nil // fire once; a nil channel never becomes ready
			case <-sigs:
				if !d.trig.fire(reasonSignal) {
					d.abortOnce.Do(func() { close(d.abort) })
				}
			case <-quit:
				return
			}
		}
	}()
	return func() {
		signal.Stop(sigs)
		releasePipes()
		close(quit)
		<-finished
	}
}

// TakeSIGPIPE asks the runtime for SIGPIPE, and drops it, until the returned function is
// called.
//
// Go ends a process whose write to descriptor 1 or 2 fails with EPIPE, unless the program
// has asked for SIGPIPE. A foreground daemon mirrors its log to stderr, so a reader that
// went away (`2>&1 | head -1`) would otherwise kill it inside the next log record, in the
// middle of a run or of a shutdown, and again inside the CLI's final write of an error
// report after Run has returned. With SIGPIPE requested the write returns EPIPE instead:
// the logger stops mirroring (internal/logging) and the report is lost, but the exit status
// stands. Nobody reads the channel: delivery never blocks, and a full channel drops the
// signal.
//
// signal.Ignore would be wrong: an ignored signal stays ignored across exec, and every child
// the daemon spawns would inherit it; a handled one goes back to its default in the child.
//
// Calls nest: SIGPIPE stays taken until every release function has run.
func TakeSIGPIPE() (release func()) {
	pipes := make(chan os.Signal, 1)
	signal.Notify(pipes, syscall.SIGPIPE)
	return func() { signal.Stop(pipes) }
}
