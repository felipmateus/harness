// Package daemon is the composition root: the only package that wires the ledger, the
// broker, the admission gate, the API, the config, the logger and the home together.
//
// Run owns the process-level concerns (umask, signals) and the two fixed sequences of
// `_spec.md` Part II: the twelve boot steps, each with an undo action and a cancellation
// check at its boundary, and the ten shutdown steps inside the shutdown budget. Everything
// that happens along the way is recorded in the `daemon` stream of the ledger.
package daemon

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/felipmateus/harness/internal/admission"
	"github.com/felipmateus/harness/internal/api/contract"
	"github.com/felipmateus/harness/internal/broker"
	"github.com/felipmateus/harness/internal/config"
	"github.com/felipmateus/harness/internal/home"
	"github.com/felipmateus/harness/internal/ledger"
	"github.com/felipmateus/harness/internal/logging"
	"github.com/felipmateus/harness/internal/version"
)

// Mode says how the daemon was started.
type Mode int

const (
	// Interactive is a foreground start: logs are mirrored to Options.Stderr for the
	// whole run, and a held lock is retried briefly before it is reported.
	Interactive Mode = iota
	// Detached is the background child of `harness daemon start`: logs are mirrored to
	// Options.Stderr (the CLI's daemon.out) until the daemon is running.
	Detached
)

// Process exit codes of a failed Run (sysexits.h).
const (
	exitFailure     = 1  // unexpected failure, or a shutdown that overran its budget
	exitUnavailable = 69 // EX_UNAVAILABLE: another daemon owns the home, or the ledger is newer
	exitConfig      = 78 // EX_CONFIG: invalid config, insecure home, socket path too long
)

// Options configure Run.
type Options struct {
	// Home is the resolved home directory. Run never falls back to the operator's default
	// home: an empty Home is a failed boot.
	Home home.Home
	Mode Mode
	// Stderr receives the log mirror. Nil means os.Stderr.
	Stderr io.Writer
	// Version is recorded in daemon.started and reported by status. The zero value means
	// the identity of the running binary.
	Version version.Info
	// Hooks is for tests only; nil in production.
	Hooks *Hooks
}

// ExitError is the failure of a boot, or of a shutdown that ran out of budget. The caller
// turns it into the process exit code and the one-line error report.
type ExitError struct {
	Code    int    // process exit code: 69, 78 or 1
	ErrCode string // contract error code, e.g. "config_invalid"
	Message string
	Hint    string
}

func (e *ExitError) Error() string { return e.Message }

// Run sets umask 077, installs the SIGINT, SIGTERM and SIGHUP handlers, boots, serves
// until a signal, a shutdown request or the end of ctx, then shuts down.
//
// A boot that fails returns *ExitError after undoing what it had done. A boot or a run
// that is stopped returns nil. A shutdown that overruns shutdown_grace, or that is
// interrupted by a second signal, returns *ExitError with code 1.
func Run(ctx context.Context, opts Options) error {
	// First statement: every file the daemon creates from here on is private (Safety
	// Invariant 3). The umask is process-wide and is not restored.
	syscall.Umask(0o077)

	d := newDaemon(opts)
	defer d.watch(ctx)()
	defer d.closeLogger()
	err := d.run()
	if opts.Mode == Detached {
		d.reportFinalError(err)
	}
	return err
}

// errBootCancelled is the boot's answer when a stop request ended it. The boot has already
// recorded and undone what it must; Run returns nil.
var errBootCancelled = errors.New("daemon: boot cancelled")

// daemon is the state of one Run. The fields in the second group are set by the boot
// steps, in order, and are read by later steps and by the shutdown.
type daemon struct {
	opts     Options
	hooks    Hooks
	hooksErr error // HARNESS_TESTHOOKS could not be read (test builds only)
	pid      int

	started time.Time // when Run began: the basis of boot_ms, started_at and uptime
	trig    *trigger  // the first request to stop
	undo    undoStack

	// appended is set once this run has put an event in the daemon stream. From then on the
	// previous run's last event no longer stands, so a cancelled boot must end its own run.
	appended atomic.Bool

	abort     chan struct{} // closed by a second signal during shutdown
	abortOnce sync.Once

	home   home.Home
	lock   *home.Lock
	cfg    config.Loaded
	logger *logging.Logger
	hub    *broker.Hub
	led    *ledger.Ledger
	gate   *admission.Lifecycle

	ln           *net.UnixListener
	httpSrv      *http.Server
	conns        *connTracker
	serveDone    chan struct{} // closed when http.Server.Serve has returned
	streamCancel context.CancelFunc
}

func newDaemon(opts Options) *daemon {
	d := &daemon{
		opts:    opts,
		pid:     os.Getpid(),
		started: time.Now(),
		trig:    newTrigger(),
		abort:   make(chan struct{}),
		gate:    admission.New(),
		conns:   newConnTracker(),
	}
	if opts.Hooks != nil {
		d.hooks = *opts.Hooks
	} else if hooks, err := hooksFromEnv(); err != nil {
		d.hooksErr = err
	} else if hooks != nil {
		d.hooks = *hooks
	}
	if d.opts.Version == (version.Info{}) {
		d.opts.Version = version.Get()
	}
	if d.opts.Stderr == nil {
		d.opts.Stderr = os.Stderr
	}
	return d
}

// run boots, serves and shuts down.
func (d *daemon) run() error {
	if d.hooksErr != nil {
		return &ExitError{Code: exitFailure, ErrCode: contract.CodeInternal, Message: d.hooksErr.Error()}
	}
	if err := d.boot(); err != nil {
		if errors.Is(err, errBootCancelled) {
			return nil
		}
		return err
	}
	<-d.trig.fired
	err := d.shutdown()
	if d.hooks.BlockExit {
		blockForever()
	}
	return err
}

// log returns the daemon logger, or a logger that discards until step 5 has opened the file.
func (d *daemon) log() *slog.Logger {
	if d.logger == nil {
		return slog.New(slog.DiscardHandler)
	}
	return d.logger.Logger
}

// closeLogger closes the log file. It is the last thing Run does, so nothing logs to a
// closed file.
func (d *daemon) closeLogger() {
	if d.logger != nil {
		d.logger.Close()
	}
}

// uptimeSeconds is the whole seconds since Run began.
func (d *daemon) uptimeSeconds() int64 { return int64(time.Since(d.started) / time.Second) }

// undoAction is the inverse of one boot step.
type undoAction struct {
	name string
	run  func(ctx context.Context) error
}

// undoStack holds the undo actions of the steps that have run, oldest first.
type undoStack struct{ actions []undoAction }

func (s *undoStack) push(name string, run func(ctx context.Context) error) {
	s.actions = append(s.actions, undoAction{name: name, run: run})
}

// unwind runs the undo actions newest first. A failing action is logged and the rest still
// run: leaving the lock or the socket behind would be worse than the failure.
func (d *daemon) unwind(ctx context.Context) {
	for i := len(d.undo.actions) - 1; i >= 0; i-- {
		a := d.undo.actions[i]
		if err := a.run(ctx); err != nil {
			d.log().Warn("boot undo failed", "action", a.name, "err", err)
		}
	}
	d.undo.actions = nil
}
