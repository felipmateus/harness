//go:build integration

package daemon

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/felipmateus/harness/internal/api/contract"
	"github.com/felipmateus/harness/internal/home"
	"github.com/felipmateus/harness/internal/ledger"
	"github.com/felipmateus/harness/internal/testutil/testhome"
	_ "modernc.org/sqlite" // the driver, to fake a newer schema
)

// seedLedger writes events to a home's ledger before any daemon runs there, as a previous
// run would have left them.
func seedLedger(t *testing.T, h home.Home, events ...ledger.NewEvent) {
	t.Helper()
	l, err := ledger.Open(context.Background(), ledger.Options{Path: h.DBPath()})
	if err != nil {
		t.Fatalf("seed: open ledger: %v", err)
	}
	for _, e := range events {
		if _, _, err := l.Append(context.Background(), e); err != nil {
			t.Fatalf("seed: append %s: %v", e.Type, err)
		}
	}
	if err := l.Close(context.Background()); err != nil {
		t.Fatalf("seed: close ledger: %v", err)
	}
}

func daemonEvent(typ, data string) ledger.NewEvent {
	return ledger.NewEvent{Stream: daemonStream, Type: typ, Data: json.RawMessage(data)}
}

// Business Rule 13: a start whose previous run did not end cleanly records
// daemon.unclean_shutdown_detected, with the previous PID, before config.loaded; a clean end
// records nothing.
func TestUncleanShutdownDetection(t *testing.T) {
	pid := float64(os.Getpid())
	tests := []struct {
		name     string
		seed     []ledger.NewEvent
		wantType []string // events the new run adds
		want     map[string]any
	}{
		{
			name:     "killed while running",
			seed:     []ledger.NewEvent{daemonEvent(typeConfigLoaded, `{"pid":4242}`), daemonEvent(typeStarted, `{"pid":4242,"version":"x","boot_ms":1}`)},
			wantType: []string{typeUncleanDetected, typeConfigLoaded, typeStarted},
			want:     map[string]any{"pid": pid, "previous_pid": float64(4242), "last_seq": float64(2), "last_type": typeStarted},
		},
		{
			name:     "killed while stopping",
			seed:     []ledger.NewEvent{daemonEvent(typeStarted, `{"pid":4242}`), daemonEvent(typeStopping, `{"pid":4242,"reason":"signal"}`)},
			wantType: []string{typeUncleanDetected, typeConfigLoaded, typeStarted},
			want:     map[string]any{"pid": pid, "previous_pid": float64(4242), "last_seq": float64(2), "last_type": typeStopping},
		},
		{
			name:     "last event carries no pid",
			seed:     []ledger.NewEvent{daemonEvent(typeStarted, `{}`)},
			wantType: []string{typeUncleanDetected, typeConfigLoaded, typeStarted},
			want:     map[string]any{"pid": pid, "previous_pid": nil, "last_seq": float64(1), "last_type": typeStarted},
		},
		{
			name:     "an earlier unclean event is itself unclean",
			seed:     []ledger.NewEvent{daemonEvent(typeUncleanDetected, `{"pid":4243,"previous_pid":4242,"last_seq":1,"last_type":"daemon.started"}`)},
			wantType: []string{typeUncleanDetected, typeConfigLoaded, typeStarted},
			want:     map[string]any{"pid": pid, "previous_pid": float64(4243), "last_seq": float64(1), "last_type": typeUncleanDetected},
		},
		{
			name:     "graceful stop",
			seed:     []ledger.NewEvent{daemonEvent(typeStopping, `{"pid":4242}`), daemonEvent(typeStopped, `{"pid":4242,"uptime_seconds":1}`)},
			wantType: []string{typeConfigLoaded, typeStarted},
		},
		{
			name:     "recorded boot failure",
			seed:     []ledger.NewEvent{daemonEvent(typeBootFailed, `{"pid":4242,"step":10,"code":"listen_failed"}`)},
			wantType: []string{typeConfigLoaded, typeStarted},
		},
		{
			name:     "no history",
			wantType: []string{typeConfigLoaded, typeStarted},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			checkLeaks(t)
			h := testhome.New(t)
			seedLedger(t, h, tt.seed...)
			r := startDaemon(t, h, nil)

			events := r.events(t, daemonStream)
			added := events[len(tt.seed):]
			if got := wireTypes(added); !slices.Equal(got, tt.wantType) {
				t.Fatalf("events added by the start = %v, want %v", got, tt.wantType)
			}
			if tt.want == nil {
				return
			}
			got := dataOf(t, added[0].Data)
			for k, want := range tt.want {
				if got[k] != want {
					t.Errorf("unclean event %s = %v, want %v (data %v)", k, got[k], want, got)
				}
			}
			// The field order of the stored JSON is the documented one.
			if want := `{"pid":`; !strings.HasPrefix(string(added[0].Data), want) {
				t.Errorf("unclean event data %s does not start with the pid", added[0].Data)
			}
			if !hasLog(logRecords(t, h), "unclean shutdown detected") {
				t.Error("the log has no `unclean shutdown detected` record")
			}
		})
	}
}

// A boot that is cancelled records what the sequence says it must. Before the ledger opens
// there is nothing to record; between steps 6 and 8 the boot only undoes (the previous
// run's last event stands, so no false unclean event appears); from step 9, after
// config.loaded, it records daemon.stopping and daemon.stopped.
func TestBootCancellationAtEveryBoundary(t *testing.T) {
	for _, step := range []int{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11} {
		t.Run(fmt.Sprintf("after step %d", step), func(t *testing.T) {
			checkLeaks(t)
			h := testhome.New(t)
			hk := newHold()
			r := begin(t, h, holdBootAt(hk, step))
			r.releaseAtCleanup(hk)
			hk.waitEntered(t)

			r.cancel()

			if err, ok := r.wait(15 * time.Second); !ok || err != nil {
				t.Fatalf("Run = %v (returned=%v), want nil for a cancelled boot", err, ok)
			}
			if exists(h.SocketPath()) || exists(h.InfoPath()) || !lockIsFree(t, h) {
				t.Errorf("a cancelled boot left the socket (%v), daemon.json (%v) or the lock (free=%v)",
					exists(h.SocketPath()), exists(h.InfoPath()), lockIsFree(t, h))
			}
			select {
			case <-r.running:
				t.Fatal("the daemon reported running")
			default:
			}

			switch {
			case step < 6:
				if exists(h.DBPath()) {
					t.Error("the ledger was created by a boot cancelled before step 6")
				}
			case step < 9:
				if got := ledgerEvents(t, h, daemonStream); len(got) != 0 {
					t.Errorf("a boot cancelled after step %d recorded %v, want nothing", step, ledgerTypes(got))
				}
			default:
				got := ledgerEvents(t, h, daemonStream)
				if want := []string{typeConfigLoaded, typeStopping, typeStopped}; !slices.Equal(ledgerTypes(got), want) {
					t.Fatalf("daemon stream = %v, want %v", ledgerTypes(got), want)
				}
				if reason := dataOf(t, got[1].Data)["reason"]; reason != reasonContext {
					t.Errorf("daemon.stopping reason = %v, want context", reason)
				}
			}
			if step >= 5 && !hasLog(logRecords(t, h), "boot cancelled") {
				t.Error("the log has no `boot cancelled` record")
			}
		})
	}
}

// A boot cancelled between steps 6 and 8 leaves the previous run's end in place: after a
// graceful stop, the next start sees a clean ledger.
func TestBootCancelledBeforeConfigLoadedAddsNoUncleanEvent(t *testing.T) {
	checkLeaks(t)
	h := testhome.New(t)
	seedLedger(t, h,
		daemonEvent(typeStopping, `{"pid":4242,"reason":"request"}`),
		daemonEvent(typeStopped, `{"pid":4242,"uptime_seconds":1}`),
	)
	hk := newHold()
	r := begin(t, h, holdBootAt(hk, 7))
	r.releaseAtCleanup(hk)
	hk.waitEntered(t)
	r.cancel()
	if err, ok := r.wait(15 * time.Second); !ok || err != nil {
		t.Fatalf("Run = %v (returned=%v), want nil", err, ok)
	}

	r2 := startDaemon(t, h, nil)
	want := []string{typeStopping, typeStopped, typeConfigLoaded, typeStarted}
	if got := wireTypes(r2.events(t, daemonStream)); !slices.Equal(got, want) {
		t.Fatalf("daemon stream = %v, want %v", got, want)
	}
}

// Business Rules 12 and 13: a boot that is cancelled after it appended
// daemon.unclean_shutdown_detected (step 7) ends its own run on the stream with
// daemon.stopping and daemon.stopped. Without them that unclean event stays the last one, and
// the next start reports a second unclean shutdown, naming the cancelled boot's PID, for a
// run that stopped cleanly on request. A boot cancelled before step 7 has appended nothing,
// so the previous run's unclean end is still the one to report, exactly once.
func TestBootCancelledAfterAnUncleanEventRecordsItsStop(t *testing.T) {
	pid := float64(os.Getpid())
	crashed := []ledger.NewEvent{
		daemonEvent(typeConfigLoaded, `{"pid":4242}`),
		daemonEvent(typeStarted, `{"pid":4242,"version":"x","boot_ms":1}`),
	}
	stopped := []string{typeUncleanDetected, typeStopping, typeStopped}
	tests := []struct {
		name       string
		step       int
		cancelAdds []string // what the cancelled boot adds to the stream
		nextAdds   []string // what the next, successful start adds
	}{
		{"cancelled before the unclean check", 6, nil, []string{typeUncleanDetected, typeConfigLoaded, typeStarted}},
		{"cancelled after the unclean check", 7, stopped, []string{typeConfigLoaded, typeStarted}},
		{"cancelled after the stale cleanup", 8, stopped, []string{typeConfigLoaded, typeStarted}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			checkLeaks(t)
			h := testhome.New(t)
			seedLedger(t, h, crashed...)
			hk := newHold()
			r := begin(t, h, holdBootAt(hk, tt.step))
			r.releaseAtCleanup(hk)
			hk.waitEntered(t)

			r.cancel()

			if err, ok := r.wait(15 * time.Second); !ok || err != nil {
				t.Fatalf("Run = %v (returned=%v), want nil for a cancelled boot", err, ok)
			}
			got := ledgerEvents(t, h, daemonStream)
			if added := ledgerTypes(got[len(crashed):]); !slices.Equal(added, tt.cancelAdds) {
				t.Fatalf("the cancelled boot added %v, want %v", added, tt.cancelAdds)
			}
			if len(tt.cancelAdds) > 0 {
				unclean := dataOf(t, got[2].Data)
				if unclean["previous_pid"] != float64(4242) || unclean["last_type"] != typeStarted {
					t.Errorf("unclean event = %v, want previous_pid 4242 and last_type %s", unclean, typeStarted)
				}
				if data := dataOf(t, got[3].Data); data["pid"] != pid || data["reason"] != reasonContext {
					t.Errorf("daemon.stopping = %v, want this boot's pid and reason context", data)
				}
				if data := dataOf(t, got[4].Data); data["pid"] != pid {
					t.Errorf("daemon.stopped = %v, want this boot's pid", data)
				}
			}

			r2 := startDaemon(t, h, nil)
			events := r2.events(t, daemonStream)
			if added := wireTypes(events[len(crashed)+len(tt.cancelAdds):]); !slices.Equal(added, tt.nextAdds) {
				t.Fatalf("the next start added %v, want %v (stream %v)", added, tt.nextAdds, wireTypes(events))
			}
			if len(tt.cancelAdds) == 0 {
				unclean := dataOf(t, events[len(crashed)].Data)
				if unclean["previous_pid"] != float64(4242) || unclean["last_type"] != typeStarted {
					t.Errorf("unclean event = %v, want previous_pid 4242 and last_type %s", unclean, typeStarted)
				}
			}
		})
	}
}

// Refusals before the ledger: the error carries the contract code and exit status, nothing
// is left behind, and no daemon event is recorded.
func TestBootRefusals(t *testing.T) {
	t.Run("invalid config", func(t *testing.T) {
		checkLeaks(t)
		h := testhome.New(t)
		writeConfig(t, h, "[daemon]\nshutdown_grase = \"10s\"\n")

		exit := exitError(t, Run(t.Context(), Options{Home: h, Stderr: &syncBuffer{}}))

		if exit.Code != 78 || exit.ErrCode != "config_invalid" || exit.Hint != "run `harness config validate`" {
			t.Errorf("error = %+v, want 78 config_invalid with the validate hint", exit)
		}
		if want := "config invalid: " + h.ConfigPath() + ": "; !strings.HasPrefix(exit.Message, want) {
			t.Errorf("message %q does not start with %q", exit.Message, want)
		}
		if exists(h.DBPath()) || exists(h.LogPath()) || !lockIsFree(t, h) {
			t.Errorf("an invalid config left a ledger (%v), a log (%v) or the lock (free=%v)", exists(h.DBPath()), exists(h.LogPath()), lockIsFree(t, h))
		}
	})

	t.Run("socket path too long", func(t *testing.T) {
		checkLeaks(t)
		h := home.Home{Dir: filepath.Join(testhome.New(t).Dir, strings.Repeat("a", 120))}

		exit := exitError(t, Run(t.Context(), Options{Home: h, Stderr: &syncBuffer{}}))

		if exit.Code != 78 || exit.ErrCode != "socket_path_too_long" || exit.Hint != "set HARNESS_HOME to a shorter directory" {
			t.Errorf("error = %+v, want 78 socket_path_too_long with its hint", exit)
		}
		if exists(h.Dir) {
			t.Error("the home was created although its socket path is too long")
		}
	})

	t.Run("ledger written by a newer binary", func(t *testing.T) {
		checkLeaks(t)
		h := testhome.New(t)
		fakeNewerSchema(t, h, 3)
		before, err := os.ReadFile(h.DBPath())
		if err != nil {
			t.Fatal(err)
		}

		exit := exitError(t, Run(t.Context(), Options{Home: h, Stderr: &syncBuffer{}}))

		if exit.Code != 69 || exit.ErrCode != "ledger_schema_ahead" || exit.Hint != "upgrade harness" {
			t.Errorf("error = %+v, want 69 ledger_schema_ahead with the upgrade hint", exit)
		}
		if want := "ledger schema 3 is newer than this binary supports (1)"; exit.Message != want {
			t.Errorf("message = %q, want %q", exit.Message, want)
		}
		after, err := os.ReadFile(h.DBPath())
		if err != nil {
			t.Fatal(err)
		}
		if string(before) != string(after) {
			t.Error("the refused ledger file was modified")
		}
		if exists(h.SocketPath()) || exists(h.InfoPath()) || !lockIsFree(t, h) {
			t.Error("a refused boot left a socket, a daemon.json or the lock")
		}
	})
}

// fakeNewerSchema makes the ledger at h look like it was written by a binary with schema
// version v.
func fakeNewerSchema(t *testing.T, h home.Home, v int) {
	t.Helper()
	db, err := sql.Open("sqlite", h.DBPath())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, stmt := range []string{
		`CREATE TABLE goose_db_version (id INTEGER PRIMARY KEY AUTOINCREMENT, version_id INTEGER NOT NULL, is_applied INTEGER NOT NULL, tstamp TIMESTAMP DEFAULT CURRENT_TIMESTAMP)`,
		fmt.Sprintf(`INSERT INTO goose_db_version (version_id, is_applied) VALUES (%d, 1)`, v),
		`PRAGMA wal_checkpoint(TRUNCATE)`,
	} {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	if err := os.Chmod(h.DBPath(), 0o600); err != nil {
		t.Fatal(err)
	}
}

// Stale files left by a daemon that is gone are replaced, not trusted: a socket nobody
// listens on, and a daemon.json naming a live process that is not the daemon (the PID in a
// file never decides ownership).
func TestStaleFilesAreReplaced(t *testing.T) {
	checkLeaks(t)
	h := testhome.New(t)

	stale, err := net.ListenUnix("unix", &net.UnixAddr{Name: h.SocketPath(), Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	stale.SetUnlinkOnClose(false)
	stale.Close() // the file stays, like after a crash
	if !exists(h.SocketPath()) {
		t.Fatal("the stale socket file was not left behind")
	}
	otherPID := os.Getppid() // alive, and not the daemon
	staleInfo := fmt.Sprintf(`{"pid":%d,"version":"old","socket":%q,"started_at":"2020-01-01T00:00:00.000Z"}`, otherPID, h.SocketPath())
	if err := os.WriteFile(h.InfoPath(), []byte(staleInfo), 0o600); err != nil {
		t.Fatal(err)
	}

	r := startDaemon(t, h, nil)

	if st := r.status(t); st.State != "running" {
		t.Fatalf("state = %q, want running", st.State)
	}
	info, err := h.ReadInfo()
	if err != nil {
		t.Fatal(err)
	}
	if info.PID != os.Getpid() || info.Version == "old" {
		t.Errorf("daemon.json = %+v, want it replaced with this daemon's", info)
	}
}

// writeShutdownGrace sets shutdown_grace in the home's config.
func writeShutdownGrace(t *testing.T, h home.Home, grace string) {
	t.Helper()
	writeConfig(t, h, fmt.Sprintf("[daemon]\nshutdown_grace = %q\n", grace))
}

// A shutdown that overruns its budget before step 4 logs the step, forces the daemon down,
// removes its files, releases the lock and exits 1. daemon.stopped was never written, so the
// next start reports an unclean shutdown naming the old PID.
func TestShutdownOverrunBeforeStepFour(t *testing.T) {
	checkLeaks(t)
	h := testhome.New(t)
	writeShutdownGrace(t, h, "1s")
	hk := newHold()
	r := startDaemon(t, h, holdShutdownAt(hk, 3))
	r.releaseAtCleanup(hk)

	began := time.Now()
	r.cancel()
	err, ok := r.wait(10 * time.Second)
	elapsed := time.Since(began)

	if !ok {
		t.Fatal("Run did not return after the budget expired")
	}
	exit := exitError(t, err)
	if exit.Code != 1 || exit.ErrCode != "shutdown_budget_exceeded" {
		t.Errorf("error = %+v, want code 1 shutdown_budget_exceeded", exit)
	}
	if elapsed < time.Second || elapsed > 4*time.Second {
		t.Errorf("Run returned after %s, want about the 1s grace", elapsed)
	}
	var overrun map[string]any
	for _, rec := range logRecords(t, h) {
		if rec["msg"] == "shutdown budget exceeded" {
			overrun = rec
		}
	}
	if overrun == nil || overrun["step"] != float64(3) || overrun["budget"] != "1s" || overrun["level"] != "ERROR" {
		t.Errorf("overrun record = %v, want an ERROR for step 3 with budget 1s", overrun)
	}
	if exists(h.SocketPath()) || exists(h.InfoPath()) || !lockIsFree(t, h) {
		t.Errorf("the forced shutdown left the socket (%v), daemon.json (%v) or the lock (free=%v)",
			exists(h.SocketPath()), exists(h.InfoPath()), lockIsFree(t, h))
	}

	hk.open() // the abandoned hook goroutine finishes; it must not run its step now
	r2 := startDaemon(t, h, nil)
	events := r2.events(t, daemonStream)
	want := []string{typeConfigLoaded, typeStarted, typeStopping, typeUncleanDetected, typeConfigLoaded, typeStarted}
	if got := wireTypes(events); !slices.Equal(got, want) {
		t.Fatalf("daemon stream = %v, want %v", got, want)
	}
	unclean := dataOf(t, events[3].Data)
	if unclean["previous_pid"] != float64(os.Getpid()) || unclean["last_type"] != typeStopping {
		t.Errorf("unclean event = %v, want previous_pid %d and last_type %s", unclean, os.Getpid(), typeStopping)
	}
}

// An overrun after step 4 leaves the ledger clean: daemon.stopped is there, and only the log
// records the overrun.
func TestShutdownOverrunAfterStepFour(t *testing.T) {
	checkLeaks(t)
	h := testhome.New(t)
	writeShutdownGrace(t, h, "1s")
	hk := newHold()
	r := startDaemon(t, h, holdShutdownAt(hk, 6))
	r.releaseAtCleanup(hk)

	r.cancel()
	err, ok := r.wait(10 * time.Second)
	if !ok {
		t.Fatal("Run did not return after the budget expired")
	}
	if exit := exitError(t, err); exit.Code != 1 {
		t.Errorf("exit code = %d, want 1", exit.Code)
	}
	if !hasLog(logRecords(t, h), "shutdown budget exceeded") {
		t.Error("the log has no `shutdown budget exceeded` record")
	}
	if exists(h.SocketPath()) || exists(h.InfoPath()) || !lockIsFree(t, h) {
		t.Error("the forced shutdown left a file or the lock behind")
	}

	hk.open()
	r2 := startDaemon(t, h, nil)
	want := []string{typeConfigLoaded, typeStarted, typeStopping, typeStopped, typeConfigLoaded, typeStarted}
	if got := wireTypes(r2.events(t, daemonStream)); !slices.Equal(got, want) {
		t.Fatalf("daemon stream = %v, want %v (stopped was recorded: no unclean event)", got, want)
	}
}

// IT-059: an overrun at step 7 while the writer is busy. The ledger's own Close, abandoned
// by the budget, is still stopping the writer and closing the connections (which checkpoints
// and deletes the WAL) when the forced path starts. The forced path must wait for that close
// before it removes the files and releases the lock: until the ledger is closed this process
// may still write harness.db, and only the lock owner may (Safety Invariant 1). A new owner
// that took the lock in the window would open a ledger another process is still
// checkpointing.
//
// The writer is kept busy by 300 producers that append 200 kB events in a loop, so the queue
// (256) is full when step 7 begins and Close has to drain it. The hook gives most of the 1 s
// budget away first, so what is left is a fraction of the drain whatever the disk's speed.
// A watcher probes the lock from the stop request on, as a starting CLI would: whenever it
// sees the lock free, the ledger must already be closed.
func TestIT059_OverrunAtStepSevenKeepsTheLockUntilTheLedgerIsClosed(t *testing.T) {
	checkLeaks(t)
	h := testhome.New(t)
	writeShutdownGrace(t, h, "1s")

	payload, err := json.Marshal(map[string]string{"pad": strings.Repeat("x", 200_000)})
	if err != nil {
		t.Fatal(err)
	}
	var access atomic.Pointer[TestAccess]
	var stopRequested atomic.Pointer[time.Time]
	var producers sync.WaitGroup
	t.Cleanup(producers.Wait) // every producer has seen the ledger close before the leak check
	hooks := &Hooks{BeforeShutdownStep: func(step int) {
		if step != 7 {
			return
		}
		for i := 0; i < 300; i++ {
			producers.Add(1)
			go func() {
				defer producers.Done()
				for { // until Close refuses the append
					_, _, err := access.Load().Appender.Append(context.Background(),
						ledger.NewEvent{Stream: "flood", Type: "test.tick", Data: payload})
					if err != nil {
						return
					}
				}
			}()
		}
		// Leave about 150 ms of the budget for Close: the 256 queued appends take far longer.
		time.Sleep(time.Until(stopRequested.Load().Add(850 * time.Millisecond)))
	}}
	r := startDaemon(t, h, hooks)
	access.Store(&r.access)
	led, ok := r.access.Appender.(*ledger.Ledger)
	if !ok {
		t.Fatalf("the daemon's appender is a %T, want *ledger.Ledger", r.access.Appender)
	}

	var freeWhileOpen atomic.Bool
	stopWatch := make(chan struct{})
	watched := make(chan struct{})
	go func() {
		defer close(watched)
		for {
			select {
			case <-stopWatch:
				return
			default:
			}
			if free, err := h.LockFree(); err == nil && free {
				select {
				case <-led.Closed():
				default:
					freeWhileOpen.Store(true)
				}
				return
			}
			time.Sleep(100 * time.Microsecond)
		}
	}()

	began := time.Now()
	stopRequested.Store(&began)
	r.cancel()
	runErr, returned := r.wait(30 * time.Second)
	elapsed := time.Since(began)
	close(stopWatch)
	<-watched
	if !returned {
		t.Fatal("Run did not return after the budget expired")
	}

	exit := exitError(t, runErr)
	if exit.Code != 1 || exit.ErrCode != "shutdown_budget_exceeded" {
		t.Fatalf("error = %+v, want code 1 shutdown_budget_exceeded", exit)
	}
	var overrun map[string]any
	for _, rec := range logRecords(t, h) {
		if rec["msg"] == "shutdown budget exceeded" {
			overrun = rec
		}
	}
	t.Logf("Run returned after %s; overrun record %v; lock seen free while open: %v", elapsed, overrun, freeWhileOpen.Load())
	if overrun == nil || overrun["step"] != float64(7) {
		t.Fatalf("overrun record = %v, want the budget to end at step 7 (ledger)", overrun)
	}
	if elapsed > time.Second+ledgerCloseWait+2*time.Second {
		t.Errorf("Run took %s to return, want the 1s budget plus at most the %s wait for the ledger", elapsed, ledgerCloseWait)
	}

	if freeWhileOpen.Load() {
		t.Error("the lock was free while the ledger was still open: this process could still write harness.db without it")
	}
	select {
	case <-led.Closed():
	default:
		t.Error("Run returned before the ledger was closed")
	}
	if exists(h.DBPath() + "-wal") {
		t.Error("harness.db-wal still exists after Run returned: the close was still checkpointing")
	}
	if exists(h.SocketPath()) || exists(h.InfoPath()) || !lockIsFree(t, h) {
		t.Errorf("the forced shutdown left the socket (%v), daemon.json (%v) or the lock (free=%v)",
			exists(h.SocketPath()), exists(h.InfoPath()), lockIsFree(t, h))
	}

	// The overrun came after step 4, so the ledger is clean: the next start finds
	// daemon.stopped and reports no unclean shutdown.
	r2 := startDaemon(t, h, nil)
	want := []string{typeConfigLoaded, typeStarted, typeStopping, typeStopped, typeConfigLoaded, typeStarted}
	if got := wireTypes(r2.events(t, daemonStream)); !slices.Equal(got, want) {
		t.Fatalf("daemon stream = %v, want %v", got, want)
	}
}

// SIGINT, SIGTERM and SIGHUP each stop the daemon gracefully with reason "signal".
func TestSignalsStopTheDaemonGracefully(t *testing.T) {
	for _, sig := range []syscall.Signal{syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP} {
		t.Run(sig.String(), func(t *testing.T) {
			checkLeaks(t)
			h := testhome.New(t)
			r := startDaemon(t, h, nil)

			if err := syscall.Kill(os.Getpid(), sig); err != nil {
				t.Fatal(err)
			}

			if err, ok := r.wait(15 * time.Second); !ok || err != nil {
				t.Fatalf("Run = %v (returned=%v), want nil", err, ok)
			}
			r2 := startDaemon(t, h, nil)
			events := r2.events(t, daemonStream)
			if got := wireTypes(events); !slices.Equal(got[:4], []string{typeConfigLoaded, typeStarted, typeStopping, typeStopped}) {
				t.Fatalf("daemon stream = %v", got)
			}
			if reason := dataOf(t, events[2].Data)["reason"]; reason != reasonSignal {
				t.Errorf("daemon.stopping reason = %v, want signal", reason)
			}
		})
	}
}

// A signal during boot cancels the boot instead of killing the process.
func TestSignalDuringBootCancelsIt(t *testing.T) {
	checkLeaks(t)
	h := testhome.New(t)
	hk := newHold()
	r := begin(t, h, holdBootAt(hk, 10))
	r.releaseAtCleanup(hk)
	hk.waitEntered(t)

	if err := syscall.Kill(os.Getpid(), syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}

	if err, ok := r.wait(15 * time.Second); !ok || err != nil {
		t.Fatalf("Run = %v (returned=%v), want nil", err, ok)
	}
	got := ledgerEvents(t, h, daemonStream)
	if want := []string{typeConfigLoaded, typeStopping, typeStopped}; !slices.Equal(ledgerTypes(got), want) {
		t.Fatalf("daemon stream = %v, want %v", ledgerTypes(got), want)
	}
	if reason := dataOf(t, got[1].Data)["reason"]; reason != reasonSignal {
		t.Errorf("daemon.stopping reason = %v, want signal", reason)
	}
}

// A second signal during shutdown exits at once with code 1, after dropping the files and
// the lock; the next start reports the interrupted stop.
func TestSecondSignalDuringShutdownExitsWithCodeOne(t *testing.T) {
	checkLeaks(t)
	h := testhome.New(t)
	hk := newHold()
	r := startDaemon(t, h, holdShutdownAt(hk, 3))
	r.releaseAtCleanup(hk)

	if err := syscall.Kill(os.Getpid(), syscall.SIGINT); err != nil {
		t.Fatal(err)
	}
	hk.waitEntered(t) // draining, held at step 3
	began := time.Now()
	if err := syscall.Kill(os.Getpid(), syscall.SIGINT); err != nil {
		t.Fatal(err)
	}

	err, ok := r.wait(10 * time.Second)
	if !ok {
		t.Fatal("Run did not return after the second signal")
	}
	exit := exitError(t, err)
	if exit.Code != 1 || exit.ErrCode != "shutdown_aborted" {
		t.Errorf("error = %+v, want code 1 shutdown_aborted", exit)
	}
	if elapsed := time.Since(began); elapsed > 3*time.Second {
		t.Errorf("Run took %s to exit after the second signal", elapsed)
	}
	if exists(h.SocketPath()) || exists(h.InfoPath()) || !lockIsFree(t, h) {
		t.Error("the aborted shutdown left a file or the lock behind")
	}

	hk.open()
	r2 := startDaemon(t, h, nil)
	events := r2.events(t, daemonStream)
	want := []string{typeConfigLoaded, typeStarted, typeStopping, typeUncleanDetected, typeConfigLoaded, typeStarted}
	if got := wireTypes(events); !slices.Equal(got, want) {
		t.Fatalf("daemon stream = %v, want %v", got, want)
	}
	if last := dataOf(t, events[3].Data)["last_type"]; last != typeStopping {
		t.Errorf("unclean last_type = %v, want %s", last, typeStopping)
	}
}

// absorbSignals makes the test process take sigs for the length of the test, whatever else
// does. A signal that reaches the process after the daemon's own handlers are gone would end
// the test binary; with this one registered it is only dropped.
func absorbSignals(t *testing.T, sigs ...os.Signal) {
	t.Helper()
	sink := make(chan os.Signal, 8)
	signal.Notify(sink, sigs...)
	t.Cleanup(func() { signal.Stop(sink) })
}

// openInFlightRequest opens a request the daemon cannot finish: a POST to the shutdown route
// that announces a body and sends none. It returns once the handler is reading that body,
// which the server announces with `100 Continue`: from then on the connection is serving a
// request that waits on a client that sends nothing, and http.Server.Shutdown waits for it.
func openInFlightRequest(t *testing.T, sock string) net.Conn {
	t.Helper()
	conn, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatalf("dial %s: %v", sock, err)
	}
	t.Cleanup(func() { conn.Close() })
	req := "POST /api/daemon/shutdown HTTP/1.1\r\nHost: harness\r\nContent-Type: application/json\r\n" +
		"Expect: 100-continue\r\nContent-Length: 64\r\n\r\n"
	if _, err := conn.Write([]byte(req)); err != nil {
		t.Fatalf("write request: %v", err)
	}
	conn.SetReadDeadline(time.Now().Add(15 * time.Second))
	line, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil || !strings.HasPrefix(line, "HTTP/1.1 100 Continue") {
		t.Fatalf("the handler did not start reading the body: %q, %v", line, err)
	}
	conn.SetReadDeadline(time.Time{})
	return conn
}

// IT-060: a client that connected and sent nothing, or only the start of a request, has no
// request in flight, and cannot hold the shutdown. net/http counts such a connection as busy
// until it is 5 s old; with a shutdown_grace of 3 s that used to overrun at step 6 and exit
// 1 for a healthy daemon. The stop is graceful, and quick.
func TestIT060_ASilentClientCannotHoldTheShutdown(t *testing.T) {
	for _, tc := range []struct{ name, sent string }{
		{"connected and silent", ""},
		{"start of a request line", "GET /api/sta"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			checkLeaks(t)
			h := testhome.New(t)
			writeShutdownGrace(t, h, "3s")
			r := startDaemon(t, h, nil)
			conn, err := net.Dial("unix", h.SocketPath())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { conn.Close() })
			if tc.sent != "" {
				if _, err := conn.Write([]byte(tc.sent)); err != nil {
					t.Fatal(err)
				}
			}
			// The server accepts in order: once it answers on a later connection, it has
			// seen this one.
			r.status(t)

			began := time.Now()
			err = r.stop(t)
			elapsed := time.Since(began)
			if err != nil {
				t.Fatalf("Run = %v, want a graceful stop (nil)", err)
			}
			if elapsed > 2*time.Second {
				t.Errorf("the stop took %s with a silent client connected, want well under a second", elapsed)
			}
			conn.SetReadDeadline(time.Now().Add(5 * time.Second))
			if n, err := conn.Read(make([]byte, 1)); n != 0 || err == nil {
				t.Errorf("the silent connection read %d bytes, %v; want it closed", n, err)
			}
			if exists(h.SocketPath()) || exists(h.InfoPath()) || !lockIsFree(t, h) {
				t.Error("the stop left the socket, daemon.json or the lock behind")
			}
		})
	}
}

// IT-061: a second signal during the undo of a boot that ends early exits at once with code
// 1, like a second signal during the shutdown (US-002 EC-2). The undo here waits on a request
// that is in flight and never finishes (http.Server.Shutdown), so without the second signal
// it would run to the end of the budget. The undo gives up on what it waits for, still
// removes the files and releases the lock, and Run returns without waiting for the budget.
func TestIT061_SecondSignalDuringTheBootUndoExitsAtOnce(t *testing.T) {
	t.Run("cancelled boot", func(t *testing.T) {
		checkLeaks(t)
		absorbSignals(t, syscall.SIGTERM)
		h := testhome.New(t)
		hk := newHold()
		r := begin(t, h, holdBootAt(hk, 10))
		r.releaseAtCleanup(hk)
		hk.waitEntered(t)
		openInFlightRequest(t, h.SocketPath())

		r.cancel() // the first stop request: the boot is cancelled and its undo starts
		if _, ok := r.wait(500 * time.Millisecond); ok {
			t.Fatal("the undo ended on its own: it was meant to wait for the request in flight")
		}
		began := time.Now()
		if err := syscall.Kill(os.Getpid(), syscall.SIGTERM); err != nil { // the second
			t.Fatal(err)
		}

		err, ok := r.wait(10 * time.Second)
		if !ok {
			t.Fatal("Run did not return after the second signal")
		}
		exit := exitError(t, err)
		if exit.Code != 1 || exit.ErrCode != "shutdown_aborted" {
			t.Errorf("error = %+v, want code 1 shutdown_aborted", exit)
		}
		if elapsed := time.Since(began); elapsed > 3*time.Second {
			t.Errorf("Run took %s to exit after the second signal", elapsed)
		}
		if exists(h.SocketPath()) || exists(h.InfoPath()) || !lockIsFree(t, h) {
			t.Error("the interrupted undo left a file or the lock behind")
		}

		// The stop was recorded before the undo began: the next start finds a clean ledger.
		r2 := startDaemon(t, h, nil)
		want := []string{typeConfigLoaded, typeStopping, typeStopped, typeConfigLoaded, typeStarted}
		if got := wireTypes(r2.events(t, daemonStream)); !slices.Equal(got, want) {
			t.Errorf("daemon stream = %v, want %v", got, want)
		}
	})

	// A boot that fails is already ending with an error; a second signal cuts its undo short
	// and that error is still the one reported.
	t.Run("failed boot", func(t *testing.T) {
		checkLeaks(t)
		absorbSignals(t, syscall.SIGTERM, syscall.SIGHUP)
		h := testhome.New(t)
		hk := newHold()
		r := begin(t, h, &Hooks{AfterBootStep: func(step int) error {
			if step == 10 {
				hk.wait()
				return errors.New("injected failure after listen")
			}
			return nil
		}})
		r.releaseAtCleanup(hk)
		hk.waitEntered(t)
		openInFlightRequest(t, h.SocketPath())

		hk.open() // the hook fails the boot: it records boot_failed and its undo starts
		if _, ok := r.wait(500 * time.Millisecond); ok {
			t.Fatal("the undo ended on its own: it was meant to wait for the request in flight")
		}
		began := time.Now()
		// The first signal is a stop request, which a boot that is already failing has
		// nothing to do with; the second one is the one that interrupts. Different signals
		// do not coalesce, so both are seen.
		if err := syscall.Kill(os.Getpid(), syscall.SIGTERM); err != nil {
			t.Fatal(err)
		}
		if err := syscall.Kill(os.Getpid(), syscall.SIGHUP); err != nil {
			t.Fatal(err)
		}

		err, ok := r.wait(10 * time.Second)
		if !ok {
			t.Fatal("Run did not return after the second signal")
		}
		exit := exitError(t, err)
		if exit.Code != 1 || exit.ErrCode != "listen_failed" {
			t.Errorf("error = %+v, want the boot failure: code 1 listen_failed", exit)
		}
		if elapsed := time.Since(began); elapsed > 3*time.Second {
			t.Errorf("Run took %s to exit after the second signal", elapsed)
		}
		if exists(h.SocketPath()) || exists(h.InfoPath()) || !lockIsFree(t, h) {
			t.Error("the interrupted undo left a file or the lock behind")
		}
		got := ledgerEvents(t, h, daemonStream)
		if want := []string{typeConfigLoaded, typeBootFailed}; !slices.Equal(ledgerTypes(got), want) {
			t.Errorf("daemon stream = %v, want %v", ledgerTypes(got), want)
		}
	})
}

// Interactive mirrors the log to stderr for the whole run. Detached mirrors it only until
// the daemon is running, so a detached daemon's stderr (daemon.out) holds boot output and
// nothing else; harness.log has everything in both modes.
func TestLogMirrorByMode(t *testing.T) {
	t.Run("interactive", func(t *testing.T) {
		checkLeaks(t)
		h := testhome.New(t)
		writeConfig(t, h, "[log]\nlevel = \"debug\"\n")
		r := beginMode(t, h, nil, Interactive)
		r.waitRunning(t)
		if err := r.stop(t); err != nil {
			t.Fatal(err)
		}
		out := r.stderr.String()
		for _, want := range []string{`"msg":"boot step"`, `"msg":"daemon running"`, `"msg":"daemon stopped"`} {
			if !strings.Contains(out, want) {
				t.Errorf("stderr lacks %s:\n%s", want, out)
			}
		}
	})

	t.Run("detached", func(t *testing.T) {
		checkLeaks(t)
		h := testhome.New(t)
		writeConfig(t, h, "[log]\nlevel = \"debug\"\n")
		r := beginMode(t, h, nil, Detached)
		r.waitRunning(t)
		if err := r.stop(t); err != nil {
			t.Fatal(err)
		}
		out := r.stderr.String()
		if !strings.Contains(out, `"msg":"boot step"`) {
			t.Errorf("a detached daemon mirrors nothing of its boot:\n%s", out)
		}
		for _, notWant := range []string{`"msg":"daemon running"`, `"msg":"daemon stopped"`, `"msg":"shutdown step"`} {
			if strings.Contains(out, notWant) {
				t.Errorf("a running detached daemon still mirrors to stderr: %s\n%s", notWant, out)
			}
		}
		records := logRecords(t, h)
		for _, want := range []string{"daemon running", "daemon stopped"} {
			if !hasLog(records, want) {
				t.Errorf("harness.log lacks %q", want)
			}
		}
	})
}

// The shutdown answer carries the grace the operator configured, as written.
func TestShutdownAnswerCarriesTheConfiguredGrace(t *testing.T) {
	checkLeaks(t)
	h := testhome.New(t)
	writeShutdownGrace(t, h, "45s")
	r := startDaemon(t, h, nil)

	code, body := r.do(t, http.MethodPost, "/api/daemon/shutdown", "")

	var resp contract.ShutdownResponse
	if code != http.StatusAccepted || json.Unmarshal(body, &resp) != nil || resp.Daemon.ShutdownGrace != "45s" {
		t.Fatalf("POST shutdown = %d %s, want 202 with shutdown_grace 45s", code, body)
	}
	if err, ok := r.wait(10 * time.Second); !ok || err != nil {
		t.Fatalf("Run = %v (returned=%v), want nil", err, ok)
	}
}

// Every request target gets the JSON error shape, `OPTIONS *` included: the HTTP server's
// own answer for it is switched off.
func TestOptionsStarGetsTheJSONError(t *testing.T) {
	checkLeaks(t)
	h := testhome.New(t)
	startDaemon(t, h, nil)

	conn, err := net.Dial("unix", h.SocketPath())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := fmt.Fprint(conn, "OPTIONS * HTTP/1.1\r\nHost: harness\r\nConnection: close\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var body contract.ErrorBody
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil || body.Error == nil || body.Error.Code != contract.CodeNotFound {
		t.Errorf("OPTIONS * = %d %+v (%v), want the JSON not_found error", resp.StatusCode, body, err)
	}
}

// Hooks.KeepaliveInterval reaches the SSE adapter.
func TestHooksTimingsReachTheAdapter(t *testing.T) {
	checkLeaks(t)
	h := testhome.New(t)
	startDaemon(t, h, &Hooks{KeepaliveInterval: 50 * time.Millisecond})
	f := openFollower(t, h.SocketPath(), "idle", followOpts{})

	eventually(t, 5*time.Second, "a keepalive on an idle stream", func() bool { return f.keepalives.Load() >= 2 })
}

// A boot hook that is held cannot hold a cancellation: the daemon stops waiting for it.
func TestHeldBootHookDoesNotBlockCancellation(t *testing.T) {
	checkLeaks(t)
	h := testhome.New(t)
	hk := newHold()
	r := begin(t, h, holdBootAt(hk, 12)) // held after the daemon is running
	r.releaseAtCleanup(hk)
	hk.waitEntered(t)

	began := time.Now()
	r.cancel()
	if err, ok := r.wait(10 * time.Second); !ok || err != nil {
		t.Fatalf("Run = %v (returned=%v), want nil", err, ok)
	}
	if elapsed := time.Since(began); elapsed > 5*time.Second {
		t.Errorf("a held hook delayed the stop by %s", elapsed)
	}
}
