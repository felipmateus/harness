package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/felipmateus/harness/internal/api/contract"
	"github.com/felipmateus/harness/internal/daemon"
	"github.com/felipmateus/harness/internal/home"
	"github.com/felipmateus/harness/internal/testutil/testhome"
)

// fakeClock is a virtual clock. In auto mode After advances the time by itself, so a
// polling loop runs instantly in virtual time; otherwise the test calls Advance.
type fakeClock struct {
	mu     sync.Mutex
	now    time.Time
	timers []*fakeTimer
	auto   bool
}

type fakeTimer struct {
	c    *fakeClock
	at   time.Time
	fire func()
	live bool
}

func newFakeClock(auto bool) *fakeClock {
	return &fakeClock{now: time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC), auto: auto}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) elapsed(since time.Time) time.Duration { return c.Now().Sub(since) }

func (c *fakeClock) After(d time.Duration) <-chan time.Time {
	ch := make(chan time.Time, 1)
	c.mu.Lock()
	if c.auto {
		c.advanceLocked(d)
		ch <- c.now
		c.mu.Unlock()
		return ch
	}
	c.addLocked(d, func() { ch <- c.Now() })
	c.mu.Unlock()
	return ch
}

func (c *fakeClock) AfterFunc(d time.Duration, f func()) timer {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.addLocked(d, func() { go f() })
}

func (c *fakeClock) addLocked(d time.Duration, fire func()) *fakeTimer {
	t := &fakeTimer{c: c, at: c.now.Add(d), fire: fire, live: true}
	c.timers = append(c.timers, t)
	return t
}

func (t *fakeTimer) Reset(d time.Duration) bool {
	t.c.mu.Lock()
	defer t.c.mu.Unlock()
	was := t.live
	t.at, t.live = t.c.now.Add(d), true
	if !slices.Contains(t.c.timers, t) {
		t.c.timers = append(t.c.timers, t)
	}
	return was
}

func (t *fakeTimer) Stop() bool {
	t.c.mu.Lock()
	defer t.c.mu.Unlock()
	was := t.live
	t.live = false
	return was
}

// Advance moves the clock forward and fires every timer that falls due, in order.
func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.advanceLocked(d)
}

func (c *fakeClock) advanceLocked(d time.Duration) {
	target := c.now.Add(d)
	for {
		var next *fakeTimer
		for _, t := range c.timers {
			if t.live && !t.at.After(target) && (next == nil || t.at.Before(next.at)) {
				next = t
			}
		}
		if next == nil {
			break
		}
		next.live = false
		if next.at.After(c.now) {
			c.now = next.at
		}
		next.fire()
	}
	c.now = target
}

// waitForTimerAt waits until a live timer is set for exactly at.
func (c *fakeClock) waitForTimerAt(t *testing.T, at time.Time) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		c.mu.Lock()
		for _, tm := range c.timers {
			if tm.live && tm.at.Equal(at) {
				c.mu.Unlock()
				return
			}
		}
		c.mu.Unlock()
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("no timer set for %v", at)
}

// fakeAPI is a daemonAPI whose answers the test scripts.
type fakeAPI struct {
	statusFn   func(call int) (contract.Status, error)
	shutdownFn func() (contract.ShutdownInfo, error)
	doctorFn   func() (contract.Doctor, error)
	eventsFn   func(stream string, after int64, limit int) (contract.EventsPage, error)
	streamFn   func(ctx context.Context, stream string, after int64) (io.ReadCloser, error)

	statusCalls, shutdownCalls, eventsCalls, streamCalls, doctorCalls atomic.Int32
}

func (f *fakeAPI) Status(context.Context) (contract.Status, error) {
	n := int(f.statusCalls.Add(1))
	if f.statusFn == nil {
		return contract.Status{}, noDaemonError("fake")
	}
	return f.statusFn(n)
}

func (f *fakeAPI) Shutdown(context.Context) (contract.ShutdownInfo, error) {
	f.shutdownCalls.Add(1)
	if f.shutdownFn == nil {
		return contract.ShutdownInfo{}, noDaemonError("fake")
	}
	return f.shutdownFn()
}

func (f *fakeAPI) Doctor(context.Context) (contract.Doctor, error) {
	f.doctorCalls.Add(1)
	if f.doctorFn == nil {
		return contract.Doctor{}, noDaemonError("fake")
	}
	return f.doctorFn()
}

func (f *fakeAPI) Events(_ context.Context, stream string, after int64, limit int) (contract.EventsPage, error) {
	f.eventsCalls.Add(1)
	if f.eventsFn == nil {
		return contract.EventsPage{}, noDaemonError("fake")
	}
	return f.eventsFn(stream, after, limit)
}

func (f *fakeAPI) OpenStream(ctx context.Context, stream string, after int64) (io.ReadCloser, error) {
	f.streamCalls.Add(1)
	if f.streamFn == nil {
		return nil, noDaemonError("fake")
	}
	return f.streamFn(ctx, stream, after)
}

func (f *fakeAPI) calls() int {
	return int(f.statusCalls.Load() + f.shutdownCalls.Load() + f.eventsCalls.Load() + f.streamCalls.Load() + f.doctorCalls.Load())
}

func runningStatus(pid int) contract.Status {
	return contract.Status{State: "running", PID: pid, Version: "0.1.0-dev"}
}

// fakeProc is a spawned child the test controls.
type fakeProc struct {
	pid  int
	done chan struct{}
	once sync.Once
	code int

	mu       sync.Mutex
	signals  []syscall.Signal
	onSignal func(*fakeProc, syscall.Signal)
}

func newFakeProc(pid int) *fakeProc { return &fakeProc{pid: pid, done: make(chan struct{})} }

func (p *fakeProc) PID() int              { return p.pid }
func (p *fakeProc) Done() <-chan struct{} { return p.done }
func (p *fakeProc) ExitCode() int         { return p.code }

func (p *fakeProc) exit(code int) {
	p.once.Do(func() {
		p.code = code
		close(p.done)
	})
}

func (p *fakeProc) Signal(sig syscall.Signal) error {
	p.mu.Lock()
	p.signals = append(p.signals, sig)
	fn := p.onSignal
	p.mu.Unlock()
	if fn != nil {
		fn(p, sig)
	}
	return nil
}

func (p *fakeProc) sent() []syscall.Signal {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.signals)
}

// fakeSpawner records the spawn and hands out a scripted child.
type fakeSpawner struct {
	mu     sync.Mutex
	specs  []spawnSpec
	proc   *fakeProc
	err    error
	onJump func(spec spawnSpec, p *fakeProc) // runs inside Spawn, like the child's first actions
}

func (s *fakeSpawner) Spawn(spec spawnSpec) (process, error) {
	s.mu.Lock()
	s.specs = append(s.specs, spec)
	s.mu.Unlock()
	if s.err != nil {
		return nil, s.err
	}
	if s.proc == nil {
		s.proc = newFakeProc(42)
	}
	if s.onJump != nil {
		s.onJump(spec, s.proc)
	}
	return s.proc, nil
}

func (s *fakeSpawner) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.specs)
}

// fakeProbe scripts the lock and the socket. freeAfter is the number of LockFree calls that
// report "held" before the lock frees; negative means never.
type fakeProbe struct {
	freeAfter   int
	socketAfter int // SocketExists calls that report "present" before it is gone
	pid         int
	lockCalls   int
	sockCalls   int
	err         error
}

func (p *fakeProbe) LockFree() (bool, error) {
	p.lockCalls++
	if p.err != nil {
		return false, p.err
	}
	return p.freeAfter >= 0 && p.lockCalls > p.freeAfter, nil
}
func (p *fakeProbe) LockPID() int { return p.pid }
func (p *fakeProbe) SocketExists() bool {
	p.sockCalls++
	return p.sockCalls <= p.socketAfter
}

// env is one CLI invocation's world, with every boundary faked.
type env struct {
	t       *testing.T
	app     *app
	home    home.Home
	clock   *fakeClock
	api     *fakeAPI
	spawner *fakeSpawner
	probe   *fakeProbe
	stdout  bytes.Buffer
	stderr  bytes.Buffer
	ran     []string
	daemons []daemon.Options
}

func newEnv(t *testing.T) *env {
	t.Helper()
	e := &env{
		t:       t,
		home:    testhome.New(t),
		clock:   newFakeClock(true),
		api:     &fakeAPI{},
		spawner: &fakeSpawner{},
		probe:   &fakeProbe{freeAfter: 0},
	}
	a := newApp(&e.stdout, &e.stderr)
	a.clock = e.clock
	a.newClient = func(string) daemonAPI { return e.api }
	a.spawner = e.spawner
	a.probe = func(home.Home) homeProbe { return e.probe }
	a.executable = func() (string, error) { return "/fake/harness", nil }
	a.runDaemon = func(_ context.Context, o daemon.Options) error {
		e.daemons = append(e.daemons, o)
		return nil
	}
	a.interrupt = func(ctx context.Context) (context.Context, context.CancelFunc) {
		return context.WithCancel(ctx)
	}
	e.app = a
	return e
}

// run executes the CLI against the env's home and returns the exit code.
func (e *env) run(args ...string) int {
	e.t.Helper()
	return e.runCtx(context.Background(), args...)
}

func (e *env) runCtx(ctx context.Context, args ...string) int {
	e.t.Helper()
	e.stdout.Reset()
	e.stderr.Reset()
	if !slices.Contains(args, "--home") {
		args = append(args, "--home", e.home.Dir)
	}
	e.ran = args
	return e.app.execute(ctx, args)
}

func (e *env) out() string { return e.stdout.String() }
func (e *env) err() string { return e.stderr.String() }

// wireError decodes the one JSON error object a structured failure leaves on stderr.
func (e *env) wireError() contract.Error {
	e.t.Helper()
	var body contract.ErrorBody
	dec := json.NewDecoder(strings.NewReader(e.err()))
	if err := dec.Decode(&body); err != nil || body.Error == nil {
		e.t.Fatalf("stderr is not an error object: %q (%v)", e.err(), err)
	}
	if dec.More() {
		e.t.Fatalf("stderr holds more than one JSON value: %q", e.err())
	}
	return *body.Error
}

// writeDaemonOut appends to logs/daemon.out the way the child would, creating it when needed.
func writeDaemonOut(t *testing.T, h home.Home, s string) {
	t.Helper()
	if err := os.MkdirAll(h.LogsDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(h.DaemonOutPath(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString(s); err != nil {
		t.Fatal(err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func exists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

func join(parts ...string) string { return filepath.Join(parts...) }

func contains(s, sub string) bool { return strings.Contains(s, sub) }
