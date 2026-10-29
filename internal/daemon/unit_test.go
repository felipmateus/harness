package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/felipmateus/harness/internal/config"
	"github.com/felipmateus/harness/internal/home"
	"github.com/felipmateus/harness/internal/ledger"
	"github.com/felipmateus/harness/internal/testutil/testhome"
)

// UT-013 (happy): with the process umask at 022 and after Run's umask call, every file the
// daemon creates is 0600 (and every directory it creates 0700). That covers daemon.json,
// the lock file, the log file, the socket and harness.db with its -wal and -shm.
func TestRun_EveryFileIsPrivateUnderUmask022(t *testing.T) {
	checkLeaks(t)
	old := syscall.Umask(0o022)
	t.Cleanup(func() { syscall.Umask(old) })
	h := testhome.NewMissing(t) // the daemon creates the home itself

	r := startDaemon(t, h, nil)

	// Run's first statement is the umask call. The daemon makes its files private
	// explicitly as well, so the modes alone cannot tell; read the umask back (setting it
	// returns the previous one, which is the one Run left).
	if got := syscall.Umask(0o022); got != 0o077 {
		t.Errorf("umask after Run's boot = %04o, want 0077", got)
	}

	// -wal and -shm exist while the ledger is open; require them so the check is not vacuous.
	for _, name := range []string{"harness.db", "harness.db-wal", "harness.db-shm", "daemon.json", "daemon.lock", "daemon.sock", "logs/harness.log"} {
		if !exists(filepath.Join(h.Dir, name)) {
			t.Fatalf("%s does not exist while the daemon runs", name)
		}
	}
	assertTreeIsPrivate(t, h.Dir)

	if err := r.stop(t); err != nil {
		t.Fatalf("Run: %v", err)
	}
	assertTreeIsPrivate(t, h.Dir) // what is left after shutdown is private too
}

// assertTreeIsPrivate fails unless every directory under root is 0700 and every other
// entry (regular file or socket) is 0600.
func assertTreeIsPrivate(t *testing.T, root string) {
	t.Helper()
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		want := fs.FileMode(0o600)
		if d.IsDir() {
			want = 0o700
		}
		if got := info.Mode().Perm(); got != want {
			t.Errorf("%s has mode %04o, want %04o", path, got, want)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// The payloads keep the field order the spec documents: the stored JSON is what followers
// and `harness events` show.
func TestEventPayloads_KeepTheDocumentedFieldOrder(t *testing.T) {
	digest := "sha256:ab"
	prev := 4242
	tests := []struct {
		name    string
		payload any
		want    string
	}{
		{"config.loaded without a file", configLoadedData{PID: 7, Path: "/h/config.toml"}, `{"pid":7,"path":"/h/config.toml","exists":false,"digest":null}`},
		{"config.loaded with a file", configLoadedData{PID: 7, Path: "/h/config.toml", Exists: true, Digest: &digest}, `{"pid":7,"path":"/h/config.toml","exists":true,"digest":"sha256:ab"}`},
		{"daemon.started", startedData{PID: 7, Version: "0.1.0-dev", BootMS: 41}, `{"pid":7,"version":"0.1.0-dev","boot_ms":41}`},
		{"daemon.stopping", stoppingData{PID: 7, Reason: reasonRequest}, `{"pid":7,"reason":"request"}`},
		{"daemon.stopped", stoppedData{PID: 7, UptimeSeconds: 72}, `{"pid":7,"uptime_seconds":72}`},
		{"daemon.boot_failed", bootFailedData{PID: 7, Step: 10, Code: "listen_failed"}, `{"pid":7,"step":10,"code":"listen_failed"}`},
		{"unclean with a previous pid", uncleanData{PID: 7, PreviousPID: &prev, LastSeq: 2, LastType: typeStarted}, `{"pid":7,"previous_pid":4242,"last_seq":2,"last_type":"daemon.started"}`},
		{"unclean without a previous pid", uncleanData{PID: 7, LastSeq: 2, LastType: typeStopping}, `{"pid":7,"previous_pid":null,"last_seq":2,"last_type":"daemon.stopping"}`},
	}
	for _, tt := range tests {
		got, err := json.Marshal(tt.payload)
		if err != nil {
			t.Fatalf("%s: %v", tt.name, err)
		}
		if string(got) != tt.want {
			t.Errorf("%s:\n got  %s\n want %s", tt.name, got, tt.want)
		}
	}
}

func TestPreviousPID(t *testing.T) {
	tests := []struct {
		data string
		want int // 0 means nil
	}{
		{`{"pid":4242,"reason":"signal"}`, 4242},
		{`{}`, 0},
		{`{"pid":null}`, 0},
		{`{"pid":"4242"}`, 0},
		{`null`, 0},
		{`[]`, 0},
		{`not json`, 0},
	}
	for _, tt := range tests {
		got := previousPID(json.RawMessage(tt.data))
		switch {
		case tt.want == 0 && got != nil:
			t.Errorf("previousPID(%s) = %d, want nil", tt.data, *got)
		case tt.want != 0 && (got == nil || *got != tt.want):
			t.Errorf("previousPID(%s) = %v, want %d", tt.data, got, tt.want)
		}
	}
}

func TestIsCleanEnd(t *testing.T) {
	for typ, want := range map[string]bool{
		typeStopped:         true,
		typeBootFailed:      true,
		typeStarted:         false,
		typeStopping:        false,
		typeConfigLoaded:    false,
		typeUncleanDetected: false,
	} {
		if got := isCleanEnd(typ); got != want {
			t.Errorf("isCleanEnd(%q) = %v, want %v", typ, got, want)
		}
	}
}

func TestTrigger_TheFirstReasonWins(t *testing.T) {
	tr := newTrigger()
	if _, _, ok := tr.state(); ok {
		t.Fatal("a new trigger reports a stop request")
	}
	if !tr.fire(reasonRequest) {
		t.Error("the first fire did not report being first")
	}
	if tr.fire(reasonSignal) {
		t.Error("the second fire reported being first")
	}
	reason, at, ok := tr.state()
	if !ok || reason != reasonRequest || at.IsZero() {
		t.Errorf("state = %q, %v, %v; want request, a time, true", reason, at, ok)
	}
	select {
	case <-tr.fired:
	default:
		t.Error("fired is not closed")
	}
}

// The failures the CLI re-raises keep their own code and exit status, even when wrapped;
// everything else is a generic failure of its step with exit code 1.
func TestExitErrorFor(t *testing.T) {
	d := &daemon{home: home.Home{Dir: "/h"}}
	step := bootStep{n: 10, name: "listen", code: "listen_failed"}

	tests := []struct {
		name     string
		err      error
		wantCode int
		wantErr  string
		wantMsg  string
		wantHint string
	}{
		{
			name:     "socket path too long",
			err:      &home.Error{Code: home.CodeSocketPathTooLong, Message: "socket path is too long", Hint: "set HARNESS_HOME to a shorter directory"},
			wantCode: 78, wantErr: "socket_path_too_long", wantMsg: "socket path is too long", wantHint: "set HARNESS_HOME to a shorter directory",
		},
		{
			name:     "home insecure",
			err:      &home.Error{Code: home.CodeHomeInsecure, Message: "home /h is not private", Hint: "chmod 700 /h"},
			wantCode: 78, wantErr: "home_insecure", wantMsg: "home /h is not private", wantHint: "chmod 700 /h",
		},
		{
			name:     "already running",
			err:      &home.Error{Code: home.CodeAlreadyRunning, Message: "daemon already running for /h (pid 7)", PID: 7},
			wantCode: 69, wantErr: "daemon_already_running", wantMsg: "daemon already running for /h (pid 7)",
		},
		{
			name:     "config invalid, wrapped",
			err:      fmt.Errorf("load: %w", &config.InvalidError{Path: "/h/config.toml", Errors: []config.FieldError{{Message: `unknown key "x"`}}}),
			wantCode: 78, wantErr: "config_invalid", wantMsg: `config invalid: /h/config.toml: unknown key "x"`, wantHint: "run `harness config validate`",
		},
		{
			name:     "schema ahead, wrapped",
			err:      fmt.Errorf("open ledger: %w", &ledger.SchemaAheadError{DB: 3, Binary: 1}),
			wantCode: 69, wantErr: "ledger_schema_ahead", wantMsg: "ledger schema 3 is newer than this binary supports (1)", wantHint: "upgrade harness",
		},
		{
			name:     "unrecognised failure",
			err:      errors.New("boom"),
			wantCode: 1, wantErr: "listen_failed", wantMsg: "boom",
		},
		{
			name:     "an ExitError passes through",
			err:      &ExitError{Code: 78, ErrCode: "config_invalid", Message: "injected"},
			wantCode: 78, wantErr: "config_invalid", wantMsg: "injected",
		},
	}
	for _, tt := range tests {
		got := d.exitErrorFor(step, tt.err)
		if got.Code != tt.wantCode || got.ErrCode != tt.wantErr || got.Message != tt.wantMsg || got.Hint != tt.wantHint {
			t.Errorf("%s: got {%d %q %q %q}, want {%d %q %q %q}", tt.name,
				got.Code, got.ErrCode, got.Message, got.Hint, tt.wantCode, tt.wantErr, tt.wantMsg, tt.wantHint)
		}
	}
}

// Run never falls back to the operator's home: no Home is a failed boot, with nothing
// touched.
func TestRun_NoHomeIsAFailedBoot(t *testing.T) {
	checkLeaks(t)
	err := Run(context.Background(), Options{Stderr: &syncBuffer{}})
	exit := exitError(t, err)
	if exit.Code != 1 || exit.ErrCode != "home_unusable" {
		t.Errorf("error = %+v, want code 1 home_unusable", exit)
	}
}

// A context that is already cancelled stops the boot at its first boundary: nothing is
// created, and Run returns nil.
func TestRun_ACancelledContextBootsNothing(t *testing.T) {
	checkLeaks(t)
	h := testhome.New(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := Run(ctx, Options{Home: h, Stderr: &syncBuffer{}}); err != nil {
		t.Fatalf("Run = %v, want nil", err)
	}
	entries, err := os.ReadDir(h.Dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("a boot cancelled before step 1 left %d entries in the home: %v", len(entries), entries)
	}
}

// The acquire loop of an interactive start retries a held lock for about 100 ms; a detached
// start tries once (the CLI loops itself).
func TestAcquireLock_RetriesOnlyInteractive(t *testing.T) {
	h := testhome.New(t)
	holder, err := h.AcquireLock()
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Release()

	d := &daemon{home: h, trig: newTrigger(), opts: Options{Mode: Interactive}}
	if got := d.lockAttempts(); got != 1+lockRetries {
		t.Errorf("interactive attempts = %d, want %d", got, 1+lockRetries)
	}
	began := time.Now()
	_, err = d.acquireLock()
	// The retries sleep at least lockRetryEvery each, so this lower bound is certain.
	if elapsed := time.Since(began); elapsed < lockRetries*lockRetryEvery {
		t.Errorf("interactive acquire gave up after %s, want at least %s of retries", elapsed, lockRetries*lockRetryEvery)
	}
	var he *home.Error
	if !errors.As(err, &he) || he.Code != home.CodeAlreadyRunning {
		t.Errorf("interactive acquire error = %v, want daemon_already_running", err)
	}

	d.opts.Mode = Detached
	if got := d.lockAttempts(); got != 1 {
		t.Errorf("detached attempts = %d, want 1", got)
	}
	if _, err = d.acquireLock(); !errors.As(err, &he) || he.Code != home.CodeAlreadyRunning {
		t.Errorf("detached acquire error = %v, want daemon_already_running", err)
	}
}

// A stop request ends the retry wait at once.
func TestAcquireLock_AStopRequestEndsTheRetries(t *testing.T) {
	h := testhome.New(t)
	holder, err := h.AcquireLock()
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Release()

	d := &daemon{home: h, trig: newTrigger(), opts: Options{Mode: Interactive}}
	d.trig.fire(reasonSignal)
	began := time.Now()
	if _, err = d.acquireLock(); err == nil {
		t.Fatal("acquire succeeded against a held lock")
	}
	// Waiting out the retries takes at least lockRetries*lockRetryEvery; honouring the stop
	// request takes microseconds.
	if elapsed := time.Since(began); elapsed >= lockRetries*lockRetryEvery {
		t.Errorf("acquire waited %s after a stop request", elapsed)
	}
}

func TestUnwind_RunsNewestFirstAndSurvivesAFailure(t *testing.T) {
	d := &daemon{}
	var order []string
	d.undo.push("first", func(context.Context) error { order = append(order, "first"); return nil })
	d.undo.push("second", func(context.Context) error { order = append(order, "second"); return errors.New("boom") })
	d.undo.push("third", func(context.Context) error { order = append(order, "third"); return nil })

	d.unwind(context.Background())

	if got := strings.Join(order, ","); got != "third,second,first" {
		t.Errorf("undo order = %s, want third,second,first", got)
	}
	if len(d.undo.actions) != 0 {
		t.Error("the undo stack is not empty after unwind")
	}
}

// fakeLedger stands in for the ledger of the forced shutdown. Close answers at once with
// closeErr, the way a Close whose ctx has ended answers while another Close is still
// running; the ledger is closed only when the test closes the channel.
type fakeLedger struct {
	closeErr   error
	closed     chan struct{}
	closeCalls atomic.Int32
}

func newFakeLedger() *fakeLedger {
	return &fakeLedger{closeErr: context.Canceled, closed: make(chan struct{})}
}

func (f *fakeLedger) Close(context.Context) error { f.closeCalls.Add(1); return f.closeErr }
func (f *fakeLedger) Closed() <-chan struct{}     { return f.closed }

// forceFixture is a daemon that holds a real lock on a temporary home, with daemon.json and
// the socket file in place, as it is when a shutdown has to be forced.
func forceFixture(t *testing.T) (d *daemon, h home.Home, lock *home.Lock) {
	t.Helper()
	h = testhome.New(t)
	lock, err := h.AcquireLock()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { lock.Release() }) // idempotent
	for _, path := range []string{h.InfoPath(), h.SocketPath()} {
		if err := os.WriteFile(path, []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	d = newDaemon(Options{Home: h, Hooks: &Hooks{}})
	d.home, d.lock = h, lock
	return d, h, lock
}

// UT-134 (Safety Invariant 1): the forced shutdown releases the lock only once the ledger
// is closed. Close with an ended context returns at once when the close that step 7 started
// is still running, so only Closed says the writer has stopped and the files are shut.
func TestForceDown_ReleasesTheLockOnlyOnceTheLedgerIsClosed(t *testing.T) {
	d, h, _ := forceFixture(t)
	led := newFakeLedger()

	done := make(chan struct{})
	go func() { d.forceDown(led, time.Minute); close(done) }()
	eventually(t, 10*time.Second, "the forced shutdown to call Close", func() bool { return led.closeCalls.Load() == 1 })

	select {
	case <-done:
		t.Fatal("the forced shutdown returned while the ledger was still closing")
	case <-time.After(100 * time.Millisecond):
	}
	if lockIsFree(t, h) {
		t.Fatal("the lock was released while the ledger was still closing")
	}

	close(led.closed)
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the forced shutdown did not finish after the ledger closed")
	}
	if !lockIsFree(t, h) {
		t.Error("the lock is still held after the ledger closed")
	}
	if exists(h.InfoPath()) || exists(h.SocketPath()) {
		t.Error("daemon.json or the socket were left behind")
	}
}

// UT-135: a ledger that does not finish closing must not hold the exit. The forced shutdown
// gives up waiting after the bound and keeps the lock: the process exits right after, and
// the exit releases the lock and ends every write at the same instant. It still removes its
// own files, which it may do while it holds the lock.
func TestForceDown_KeepsTheLockWhenTheLedgerDoesNotClose(t *testing.T) {
	d, h, _ := forceFixture(t)
	led := newFakeLedger() // Closed never closes

	began := time.Now()
	d.forceDown(led, 50*time.Millisecond)
	if elapsed := time.Since(began); elapsed < 50*time.Millisecond || elapsed > 5*time.Second {
		t.Errorf("the forced shutdown took %s, want about the 50 ms bound", elapsed)
	}
	if lockIsFree(t, h) {
		t.Error("the lock was released although the ledger never finished closing")
	}
	if exists(h.InfoPath()) || exists(h.SocketPath()) {
		t.Error("the files the lock owner may remove were left behind")
	}
}

// closeAbort closes d.abort as the watcher does on a second signal.
func closeAbort(d *daemon) { d.abortOnce.Do(func() { close(d.abort) }) }

// UT-137 (second signal in the undo of a boot): a second signal ends the context the undo
// runs under, so an action that waits on it returns; the actions after it still run, so the
// files go and the lock is released; and runUndo says it was interrupted.
func TestRunUndo_ASecondSignalEndsTheContextAndTheRestStillRuns(t *testing.T) {
	d, h, _ := forceFixture(t)
	entered := make(chan struct{})
	var slowErr atomic.Value
	d.undo.push("release lock", func(context.Context) error { return d.lock.Release() })
	d.undo.push("remove files", func(context.Context) error { return d.removeFiles() })
	d.undo.push("slow", func(ctx context.Context) error { // runs first: newest
		close(entered)
		<-ctx.Done()
		slowErr.Store(ctx.Err())
		return ctx.Err()
	})
	d.trig.fire(reasonSignal)

	result := make(chan bool, 1)
	go func() { result <- d.runUndo(d.unwindWork) }()
	<-entered
	select {
	case <-result:
		t.Fatal("the undo returned before the second signal")
	case <-time.After(100 * time.Millisecond):
	}
	if lockIsFree(t, h) {
		t.Fatal("the lock was released while the first action was still waiting")
	}

	began := time.Now()
	closeAbort(d)
	select {
	case aborted := <-result:
		if !aborted {
			t.Error("runUndo reported no second signal")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the undo did not return after the second signal")
	}
	if took := time.Since(began); took > 2*time.Second {
		t.Errorf("the undo took %s to give up", took)
	}
	if err, _ := slowErr.Load().(error); !errors.Is(err, context.Canceled) {
		t.Errorf("the waiting action saw %v, want its context cancelled by the second signal", err)
	}
	if !lockIsFree(t, h) || exists(h.InfoPath()) || exists(h.SocketPath()) {
		t.Error("the actions after the interrupted one did not run")
	}
	if len(d.undo.actions) != 0 {
		t.Error("the undo stack is not empty")
	}
}

// A second signal that arrived before the undo began (while a boot step was running) ends
// its context at once: an action that waits on it is not held.
func TestRunUndo_ASecondSignalBeforeTheUndoEndsItsContextAtOnce(t *testing.T) {
	d, _, _ := forceFixture(t)
	var saw atomic.Value
	d.undo.push("check", func(ctx context.Context) error { saw.Store(ctx.Err()); return nil })
	d.trig.fire(reasonSignal)
	closeAbort(d)
	if !d.runUndo(d.unwindWork) {
		t.Error("runUndo reported no second signal")
	}
	if err, _ := saw.Load().(error); !errors.Is(err, context.Canceled) {
		t.Errorf("the action saw %v, want a context that was already cancelled", err)
	}
}

// UT-138 (Safety Invariant 1, boot undo): an action that does not return after the second
// signal is waited for up to the bound and then left running, and the lock, which is the
// action after it in the stack, stays held until it does return: the stuck action stands
// for the ledger's close, and nothing releases the lock before that close is over.
func TestRunUndo_KeepsTheLockWhileAStuckLedgerCloseHasNotReturned(t *testing.T) {
	d, h, _ := forceFixture(t)
	entered, release, released := make(chan struct{}), make(chan struct{}), make(chan struct{})
	d.undo.push("release lock", func(context.Context) error {
		defer close(released)
		return d.lock.Release()
	})
	d.undo.push("close ledger", func(context.Context) error { // ignores its context
		close(entered)
		<-release
		return nil
	})
	d.trig.fire(reasonSignal)

	result := make(chan bool, 1)
	began := time.Now()
	go func() { result <- d.runUndoWait(d.unwindWork, 50*time.Millisecond) }()
	<-entered
	closeAbort(d)
	select {
	case aborted := <-result:
		if !aborted {
			t.Error("runUndo reported no second signal")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the undo did not give up on the stuck action")
	}
	if took := time.Since(began); took < 50*time.Millisecond || took > 5*time.Second {
		t.Errorf("the undo returned after %s, want about the 50 ms bound", took)
	}
	if lockIsFree(t, h) {
		t.Fatal("the lock was released while the ledger close had not returned")
	}

	close(release)
	select {
	case <-released:
	case <-time.After(10 * time.Second):
		t.Fatal("the lock was not released once the stuck close returned")
	}
	if !lockIsFree(t, h) {
		t.Error("the lock is still held after the release action ran")
	}
}

// unwindWork is the undo alone, as the work of runUndo.
func (d *daemon) unwindWork(ctx context.Context) { d.unwind(ctx) }

// A ledger whose Close finished it itself (nothing else was closing it) is closed when Close
// returns: no wait, and the lock goes at once. The same holds with no ledger yet.
func TestForceDown_ClosedLedgerOrNoLedgerReleasesTheLock(t *testing.T) {
	t.Run("closed by the call", func(t *testing.T) {
		d, h, _ := forceFixture(t)
		led := newFakeLedger()
		led.closeErr = nil
		close(led.closed)
		d.forceDown(led, time.Minute)
		if !lockIsFree(t, h) {
			t.Error("the lock is still held")
		}
	})
	t.Run("no ledger", func(t *testing.T) {
		d, h, _ := forceFixture(t)
		d.forceDown(nil, time.Minute)
		if !lockIsFree(t, h) {
			t.Error("the lock is still held")
		}
	})
}
