package daemon

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/felipmateus/harness/internal/admission"
	"github.com/felipmateus/harness/internal/api"
	"github.com/felipmateus/harness/internal/api/contract"
	"github.com/felipmateus/harness/internal/broker"
	"github.com/felipmateus/harness/internal/config"
	"github.com/felipmateus/harness/internal/home"
	"github.com/felipmateus/harness/internal/ledger"
	"github.com/felipmateus/harness/internal/logging"
	"github.com/felipmateus/harness/internal/stream"
)

// A foreground start retries a held lock a few times before reporting it, so a CLI that is
// probing the lock at that very instant is not mistaken for another daemon (`_spec.md`
// boot step 3). The retries span about 100 ms.
const (
	lockRetries    = 3
	lockRetryEvery = 33 * time.Millisecond
)

// serverReadHeaderTimeout bounds how long a client may take to send its request headers.
// There is deliberately no write timeout: the SSE adapter sets its own write deadlines.
const serverReadHeaderTimeout = 10 * time.Second

// bootStep is one of the twelve steps of the boot sequence.
type bootStep struct {
	n    int
	name string
	// code is the error code of this step for a failure no step recognises: the `code` of
	// daemon.boot_failed, and the code of the final error report.
	code string
	run  func() error
}

func (d *daemon) bootSteps() []bootStep {
	return []bootStep{
		{1, "home", "home_unusable", d.stepHome},
		{2, "security", "home_unusable", d.stepSecurity},
		{3, "lock", "lock_failed", d.stepLock},
		{4, "config", "config_unreadable", d.stepConfig},
		{5, "logger", "log_unavailable", d.stepLogger},
		{6, "ledger", "ledger_open_failed", d.stepLedger},
		{7, "unclean", "ledger_failed", d.stepUnclean},
		{8, "stale", "stale_cleanup_failed", d.stepStale},
		{9, "config.loaded", "ledger_failed", d.stepConfigLoaded},
		{10, "listen", "listen_failed", d.stepListen},
		{11, "daemon.json", "info_write_failed", d.stepInfo},
		{12, "started", "ledger_failed", d.stepStarted},
	}
}

// boot runs the twelve steps. It returns nil when the daemon is running, errBootCancelled
// when a stop request ended it, or the *ExitError of the step that failed. In the last two
// cases everything the boot did has been undone.
//
// Cancellation is checked at every step boundary. The steps themselves are not
// interrupted: a half-finished migration or a half-written event is worse than a boot that
// notices the request a moment later.
func (d *daemon) boot() error {
	steps := d.bootSteps()
	if reason, _, ok := d.trig.state(); ok {
		return d.cancelBoot(0, reason)
	}
	for _, s := range steps {
		began := time.Now()
		err := s.run()
		took := time.Since(began)
		if err == nil {
			err = d.afterBootStep(s.n)
		}
		if err != nil {
			return d.failBoot(s, err)
		}
		d.log().Debug("boot step", "step", s.n, "name", s.name, "duration_ms", took.Milliseconds())
		if s.n < len(steps) {
			if reason, _, ok := d.trig.state(); ok {
				return d.cancelBoot(s.n, reason)
			}
		}
	}
	return nil
}

// afterBootStep runs the test hook for a completed step. A hook that blocks is waited for
// in a goroutine, so a stop request ends the wait: the daemon moves on to the cancellation
// and the hook goroutine is left to finish on its own.
func (d *daemon) afterBootStep(n int) error {
	hook := d.hooks.AfterBootStep
	if hook == nil {
		return nil
	}
	if _, _, cancelled := d.trig.state(); cancelled {
		return nil
	}
	result := make(chan error, 1)
	go func() { result <- hook(n) }()
	select {
	case err := <-result:
		return err
	case <-d.trig.fired:
		return nil
	}
}

// failBoot handles a step that failed. Once the ledger is open it records
// daemon.boot_failed first, so the next start does not report an unclean shutdown; then it
// undoes the steps that ran, and returns the error for the caller to report. A second
// signal during the undo cuts it short (see runUndo); the error returned is still the
// failure's own, which already ends the process with a non-zero code.
func (d *daemon) failBoot(s bootStep, err error) error {
	exit := d.exitErrorFor(s, err)
	d.log().Error("boot step failed", "step", s.n, "name", s.name, "code", exit.ErrCode, "err", err)
	d.gate.Set(admission.Draining)
	aborted := d.runUndo(func(ctx context.Context) {
		if d.led != nil {
			data := bootFailedData{PID: d.pid, Step: s.n, Code: exit.ErrCode}
			if rerr := d.record(ctx, typeBootFailed, data); rerr != nil {
				d.log().Error("boot failure not recorded", "err", rerr)
			}
		}
		d.unwind(ctx)
	})
	if aborted {
		d.log().Error("boot undo interrupted by a second signal", "step", s.n)
	}
	return exit
}

// cancelBoot handles a stop request noticed after done steps. Once this boot has appended
// an event (daemon.unclean_shutdown_detected at step 7, config.loaded at step 9), that event
// is the stream's last, and the next start would take it for a run that never ended: so the
// boot records the stop it is about to make (Business Rule 12). A boot that has appended
// nothing only undoes. The previous run's last event still stands, so no false unclean
// shutdown is detected next time.
//
// A second signal before this is over is the second signal of a shutdown: the boot gives up
// on what it is doing, undoes the rest by force, and returns shutdown_aborted (exit 1)
// instead of nil.
func (d *daemon) cancelBoot(done int, reason string) error {
	d.log().Info("boot cancelled", "step", done, "reason", reason)
	d.gate.Set(admission.Draining)
	aborted := d.runUndo(func(ctx context.Context) {
		if d.appended.Load() {
			if err := d.record(ctx, typeStopping, stoppingData{PID: d.pid, Reason: reason}); err != nil {
				d.log().Error("cancellation not recorded", "err", err)
			}
			if err := d.record(ctx, typeStopped, stoppedData{PID: d.pid, UptimeSeconds: d.uptimeSeconds()}); err != nil {
				d.log().Error("cancellation not recorded", "err", err)
			}
		}
		d.unwind(ctx)
	})
	if aborted {
		d.log().Error("boot cancellation interrupted by a second signal", "step", done)
		return &ExitError{
			Code:    exitFailure,
			ErrCode: "shutdown_aborted",
			Message: fmt.Sprintf("shutdown interrupted by a second signal while the boot cancelled after step %d was being undone", done),
		}
	}
	return errBootCancelled
}

// budget returns the context that bounds an undo or a shutdown: shutdown_grace, measured
// from the stop request when there is one and from now otherwise.
func (d *daemon) budget() (context.Context, context.CancelFunc) {
	from := time.Now()
	if _, at, ok := d.trig.state(); ok {
		from = at
	}
	return context.WithDeadline(context.Background(), from.Add(d.grace()))
}

// runUndo runs work, the records and the undo of a boot that ends early, under the budget of
// a stop. It reports whether a second signal arrived before it was over.
//
// The second signal is the one rule of a stop that holds for the whole of it: the shutdown
// sequence gives up on the step it is in (runShutdownStep), and so does this. The context
// work runs under ends, so every action that waits on it returns: the API server falls back
// to closing its connections, a record is not appended, and the ledger aborts the
// transaction in flight and closes. The actions that are left still run, in order, so the
// files go and the lock is released, and the lock comes after the ledger's own close in the
// stack (a boot has no other Close going on), so it is released only once the ledger has
// finished closing (Safety Invariant 1). A second signal that came before the undo began
// ends the context at once.
//
// An action that does not return is waited for up to wait after the signal, and then left
// running: the process is about to exit with code 1, and the exit releases the lock the
// stuck action never got to release, as the forced shutdown does (forceDown).
func (d *daemon) runUndo(work func(ctx context.Context)) (aborted bool) {
	return d.runUndoWait(work, ledgerCloseWait)
}

// runUndoWait is runUndo with the wait given.
func (d *daemon) runUndoWait(work func(ctx context.Context), wait time.Duration) (aborted bool) {
	ctx, cancel := d.budget()
	defer cancel()
	if d.aborted() {
		cancel()
	}
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		work(ctx)
	}()
	select {
	case <-finished:
	case <-d.abort:
		cancel()
		timer := time.NewTimer(wait)
		defer timer.Stop()
		select {
		case <-finished:
		case <-timer.C:
			d.log().Error("boot undo abandoned: it did not finish after the second signal", "wait", wait.String())
		}
	}
	return d.aborted()
}

// aborted reports whether a second signal has asked the daemon to give up on its stop.
func (d *daemon) aborted() bool {
	select {
	case <-d.abort:
		return true
	default:
		return false
	}
}

// grace is the configured shutdown_grace, or its default before the config is loaded.
func (d *daemon) grace() time.Duration {
	if g := d.cfg.Config.Daemon.ShutdownGrace.Duration; g > 0 {
		return g
	}
	return config.Default().Daemon.ShutdownGrace.Duration
}

// exitErrorFor turns a step's failure into the error Run returns. The failures the CLI
// re-raises keep their own code and exit status; anything else is a generic failure of the
// step with exit code 1.
func (d *daemon) exitErrorFor(s bootStep, err error) *ExitError {
	var (
		exit *ExitError
		he   *home.Error
		ce   *config.InvalidError
		sae  *ledger.SchemaAheadError
	)
	switch {
	case errors.As(err, &exit):
		return exit
	case errors.As(err, &he):
		code := exitConfig
		if he.Code == home.CodeAlreadyRunning {
			code = exitUnavailable
		}
		return &ExitError{Code: code, ErrCode: he.Code, Message: he.Message, Hint: he.Hint}
	case errors.As(err, &ce):
		return &ExitError{
			Code:    exitConfig,
			ErrCode: contract.CodeConfigInvalid,
			Message: "config invalid: " + ce.Error(),
			Hint:    "run `harness config validate`",
		}
	case errors.As(err, &sae):
		return &ExitError{
			Code:    exitUnavailable,
			ErrCode: contract.CodeLedgerSchemaAhead,
			Message: sae.Error(),
			Hint:    "upgrade harness",
		}
	}
	exit = &ExitError{Code: exitFailure, ErrCode: s.code, Message: err.Error()}
	if d.logger != nil {
		exit.Hint = "see " + d.home.LogPath()
	}
	return exit
}

// Step 1: the home, as a cleaned absolute path, and the socket path that goes in it.
func (d *daemon) stepHome() error {
	if d.opts.Home.Dir == "" {
		return errors.New("no home directory given")
	}
	abs, err := filepath.Abs(d.opts.Home.Dir)
	if err != nil {
		return fmt.Errorf("resolve home %q: %w", d.opts.Home.Dir, err)
	}
	d.home = home.Home{Dir: filepath.Clean(abs)}
	return d.home.CheckSocketPath()
}

// Step 2: the home must be private, and its layout exists. Nothing is created inside a
// home that is refused.
func (d *daemon) stepSecurity() error {
	if err := d.home.CheckSecurity(); err != nil {
		return err
	}
	return d.home.EnsureLayout()
}

// Step 3: the lock decides ownership of the home.
func (d *daemon) stepLock() error {
	lock, err := d.acquireLock()
	if err != nil {
		return err
	}
	d.lock = lock
	d.undo.push("release lock", func(context.Context) error { return d.lock.Release() })
	return nil
}

// lockAttempts is how many times step 3 tries the lock: once for a detached daemon, whose
// CLI loops on its own, and with a few retries for an interactive one.
func (d *daemon) lockAttempts() int {
	if d.opts.Mode == Interactive {
		return 1 + lockRetries
	}
	return 1
}

func (d *daemon) acquireLock() (*home.Lock, error) {
	attempts := d.lockAttempts()
	for i := 1; ; i++ {
		lock, err := d.home.AcquireLock()
		var he *home.Error
		if err == nil || !errors.As(err, &he) || he.Code != home.CodeAlreadyRunning || i >= attempts {
			return lock, err
		}
		select {
		case <-time.After(lockRetryEvery):
		case <-d.trig.fired:
			return nil, err
		}
	}
}

// Step 4: the config, strictly. An invalid file stops the boot.
func (d *daemon) stepConfig() error {
	loaded, err := config.Load(d.home.ConfigPath())
	if err != nil {
		return err
	}
	d.cfg = loaded
	return nil
}

// Step 5: the logger, mirrored to stderr. The Detached daemon stops mirroring once running.
func (d *daemon) stepLogger() error {
	logger, err := logging.New(logging.Options{
		Path:   d.home.LogPath(),
		Level:  logging.ParseLevel(d.cfg.Config.Log.Level),
		Mirror: d.opts.Stderr,
	})
	if err != nil {
		return err
	}
	d.logger = logger
	return nil
}

// Step 6: the ledger. Open checks the schema read-only first, migrates and starts the
// writer; every commit is published to the broker, which logs a subscriber it degrades.
func (d *daemon) stepLedger() error {
	d.hub = broker.New(broker.OnDegrade(func(stream string, seq int64) {
		// `_spec.md` Monitoring: the record carries `seq`, the event that did not fit. The
		// broker cannot see a follower's cursor, and a stalled follower never reports one.
		d.log().Warn("subscriber degraded", "stream", stream, "seq", seq)
	}))
	led, err := ledger.Open(context.Background(), ledger.Options{
		Path:     d.home.DBPath(),
		OnCommit: d.hub.Publish,
	})
	if err != nil {
		return fmt.Errorf("open ledger %s: %w", d.home.DBPath(), err)
	}
	d.led = led
	d.undo.push("close ledger", func(ctx context.Context) error {
		d.hub.Close()
		return d.led.Close(ctx)
	})
	return nil
}

// Step 7: a previous run whose last event is not a clean end left the ledger without one.
// Record it, with the PID that run recorded (Business Rule 13).
func (d *daemon) stepUnclean() error {
	ctx := context.Background()
	last, ok, err := d.led.Last(ctx, daemonStream)
	if err != nil {
		return fmt.Errorf("read the last daemon event: %w", err)
	}
	if !ok || isCleanEnd(last.Type) {
		return nil
	}
	data := uncleanData{PID: d.pid, PreviousPID: previousPID(last.Data), LastSeq: last.Seq, LastType: last.Type}
	d.log().Warn("unclean shutdown detected", "previous_pid", data.PreviousPID, "last_seq", last.Seq, "last_type", last.Type)
	return d.record(ctx, typeUncleanDetected, data)
}

// Step 8: a socket or daemon.json left by a daemon that is gone. Removing them is safe
// because this process holds the lock (Business Rule 3).
func (d *daemon) stepStale() error {
	for _, path := range []string{d.home.SocketPath(), d.home.InfoPath()} {
		if err := removeIfExists(path); err != nil {
			return fmt.Errorf("remove stale file: %w", err)
		}
	}
	return nil
}

// Step 9: the config this run loaded.
func (d *daemon) stepConfigLoaded() error {
	return d.record(context.Background(), typeConfigLoaded, configLoadedData{
		PID:    d.pid,
		Path:   d.cfg.Path,
		Exists: d.cfg.Exists,
		Digest: d.cfg.Digest,
	})
}

// Step 10: listen on the socket and serve the API, with the gate still starting.
//
// The listener is told not to unlink the socket when it closes. Go does unlink a Unix
// socket on Close by default (IT-051 shows it), which would remove the file at shutdown
// step 6; the socket must stay until step 8 (Safety Invariant 13).
func (d *daemon) stepListen() error {
	sock := d.home.SocketPath()
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: sock, Net: "unix"})
	if err != nil {
		return fmt.Errorf("listen on %s: %w", sock, err)
	}
	ln.SetUnlinkOnClose(false)
	d.ln = ln
	// Registered before the chmod: from here on the socket file exists and must go.
	d.undo.push("remove socket", func(context.Context) error { return removeIfExists(sock) })
	d.undo.push("close server", d.shutdownServer)
	// The umask already makes the file private; the explicit chmod does not rely on it.
	if err := os.Chmod(sock, 0o600); err != nil {
		return fmt.Errorf("restrict %s: %w", sock, err)
	}

	streamCtx, cancel := context.WithCancel(context.Background())
	d.streamCancel = cancel
	handler, err := api.New(api.Options{
		Gate:   d.gate,
		Status: statusProvider{d},
		Events: d.led,
		Doctor: d.doctor(),
		Follow: func(ctx context.Context, name string, after int64) <-chan stream.Item {
			return stream.Follow(ctx, d.led, d.hub, name, after)
		},
		RequestStop:       func() { d.trig.fire(reasonRequest) },
		StreamContext:     streamCtx,
		Logger:            d.logger.Logger,
		KeepaliveInterval: d.hooks.KeepaliveInterval,
		WriteTimeout:      d.hooks.WriteTimeout,
	})
	if err != nil {
		return fmt.Errorf("build the api: %w", err)
	}
	d.httpSrv = &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: serverReadHeaderTimeout,
		// Every request target, `OPTIONS *` included, gets the JSON error shape.
		DisableGeneralOptionsHandler: true,
		ErrorLog:                     slogLogger(d.logger),
		ConnState:                    d.conns.track,
	}
	// A backstop to the broker close: ends every open stream when the server shuts down.
	d.httpSrv.RegisterOnShutdown(cancel)
	d.serveDone = make(chan struct{})
	go d.serve()
	return nil
}

// serve runs the HTTP server until it is shut down. A failure of the accept loop that
// nobody asked for stops the daemon.
func (d *daemon) serve() {
	defer close(d.serveDone)
	if err := d.httpSrv.Serve(d.ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		d.log().Error("api server stopped unexpectedly", "err", err)
		d.trig.fire(reasonContext)
	}
}

// shutdownServer stops the API server: connections that never started a request are closed,
// the listener closes (the socket file stays), open streams are cancelled, and in-flight
// requests get until ctx ends. When ctx ends first, the remaining connections are dropped
// and ctx's error is returned.
//
// The connections without a request go first because http.Server.Shutdown would wait for
// each of them until it is 5 s old (see connTracker), which a silent client could turn into
// an overrun of the whole budget. The event streams have said goodbye by now (shutdown step
// 5), and they are not among these: a stream is a request in flight.
func (d *daemon) shutdownServer(ctx context.Context) error {
	if d.httpSrv == nil {
		// The boot failed between listening and serving.
		if d.ln != nil {
			return d.ln.Close()
		}
		return nil
	}
	if n := d.conns.closeUnstarted(); n > 0 {
		d.log().Debug("connections without a request closed", "connections", n)
	}
	err := d.httpSrv.Shutdown(ctx)
	if err != nil {
		d.httpSrv.Close()
	}
	d.streamCancel()
	<-d.serveDone
	return err
}

// Step 11: daemon.json, for discovery. It never decides ownership.
func (d *daemon) stepInfo() error {
	d.undo.push("remove daemon.json", func(context.Context) error { return removeIfExists(d.home.InfoPath()) })
	return d.home.WriteInfo(home.Info{
		PID:       d.pid,
		Version:   d.opts.Version.Version,
		Socket:    d.home.SocketPath(),
		StartedAt: formatTime(d.started),
	})
}

// Step 12: record the start, open the gate, and report running.
func (d *daemon) stepStarted() error {
	bootMS := time.Since(d.started).Milliseconds()
	err := d.record(context.Background(), typeStarted, startedData{
		PID:     d.pid,
		Version: d.opts.Version.Version,
		BootMS:  bootMS,
	})
	if err != nil {
		return err
	}
	d.gate.Set(admission.Running)
	if d.opts.Mode == Detached {
		d.logger.SetMirror(false)
	}
	d.log().Info("daemon running", "socket", d.home.SocketPath(), "boot_ms", bootMS)
	if d.hooks.OnRunning != nil {
		d.hooks.OnRunning(TestAccess{Appender: d.led, Gate: d.gate, Logger: d.logger.Logger})
	}
	return nil
}

// removeIfExists removes a file the daemon owns. A file that is already gone is fine.
func removeIfExists(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}
