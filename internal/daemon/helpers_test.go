package daemon

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/felipmateus/harness/internal/api/contract"
	"github.com/felipmateus/harness/internal/home"
	"github.com/felipmateus/harness/internal/ledger"
)

// syncBuffer is a bytes.Buffer that several goroutines may use: the log mirror is written
// by the daemon while the test reads it.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// daemonRun is one daemon.Run on its own goroutine.
type daemonRun struct {
	t      *testing.T
	home   home.Home
	cancel context.CancelFunc
	stderr *syncBuffer

	running chan TestAccess // receives the access when the daemon reports running
	access  TestAccess
	client  *http.Client

	done   chan error // receives Run's result once
	mu     sync.Mutex
	ended  bool
	result error
}

// begin starts Run on h and returns at once, before the daemon is running. A cleanup stops
// the daemon and fails the test if it does not stop, so no test leaves a daemon behind.
func begin(t *testing.T, h home.Home, hooks *Hooks) *daemonRun {
	return beginMode(t, h, hooks, Interactive)
}

func beginMode(t *testing.T, h home.Home, hooks *Hooks, mode Mode) *daemonRun {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	r := &daemonRun{
		t:       t,
		home:    h,
		cancel:  cancel,
		stderr:  &syncBuffer{},
		running: make(chan TestAccess, 1),
		done:    make(chan error, 1),
		client:  unixClient(h.SocketPath()),
	}
	var hk Hooks
	if hooks != nil {
		hk = *hooks
	}
	userOnRunning := hk.OnRunning
	hk.OnRunning = func(a TestAccess) {
		if userOnRunning != nil {
			userOnRunning(a)
		}
		r.running <- a
	}
	go func() {
		r.done <- Run(ctx, Options{Home: h, Mode: mode, Stderr: r.stderr, Hooks: &hk})
	}()
	t.Cleanup(func() {
		cancel()
		if _, ok := r.wait(15 * time.Second); !ok {
			t.Errorf("daemon on %s did not stop within 15s after its context was cancelled", h.Dir)
		}
	})
	return r
}

// startDaemon boots a daemon on h and returns once it reports running.
func startDaemon(t *testing.T, h home.Home, hooks *Hooks) *daemonRun {
	t.Helper()
	r := begin(t, h, hooks)
	r.waitRunning(t)
	return r
}

// waitRunning fails the test if Run returns, or the daemon is not running within 30 s.
func (r *daemonRun) waitRunning(t *testing.T) {
	t.Helper()
	select {
	case r.access = <-r.running:
	case err := <-r.done:
		r.record(err)
		t.Fatalf("daemon returned before it was running: %v", err)
	case <-time.After(30 * time.Second):
		t.Fatal("daemon not running within 30s")
	}
}

func (r *daemonRun) record(err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.ended {
		r.ended, r.result = true, err
	}
}

// wait returns Run's result, and false if Run has not returned within timeout. It may be
// called any number of times.
func (r *daemonRun) wait(timeout time.Duration) (error, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.ended {
		return r.result, true
	}
	select {
	case err := <-r.done:
		r.ended, r.result = true, err
		return err, true
	case <-time.After(timeout):
		return nil, false
	}
}

// stop ends the daemon through its context, as an embedding caller would, and returns Run's
// result.
func (r *daemonRun) stop(t *testing.T) error {
	t.Helper()
	r.cancel()
	err, ok := r.wait(30 * time.Second)
	if !ok {
		t.Fatal("daemon did not stop within 30s")
	}
	return err
}

// unixClient is an HTTP client that talks to the daemon socket. Connections are not reused,
// so no idle connection or goroutine outlives a request.
func unixClient(sock string) *http.Client {
	return &http.Client{
		Timeout: 15 * time.Second,
		Transport: &http.Transport{
			DisableKeepAlives: true,
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", sock)
			},
		},
	}
}

// do sends a request and returns the status and the whole body.
func (r *daemonRun) do(t *testing.T, method, path, body string) (int, []byte) {
	t.Helper()
	return doRequest(t, r.client, method, path, body)
}

func doRequest(t *testing.T, c *http.Client, method, path, body string) (int, []byte) {
	t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, "http://harness"+path, rd)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("%s %s: read body: %v", method, path, err)
	}
	return resp.StatusCode, data
}

// status fetches GET /api/status.
func (r *daemonRun) status(t *testing.T) contract.Status {
	t.Helper()
	code, body := r.do(t, http.MethodGet, "/api/status", "")
	if code != http.StatusOK {
		t.Fatalf("GET /api/status = %d: %s", code, body)
	}
	var resp contract.StatusResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode status: %v: %s", err, body)
	}
	return resp.Status
}

// errorCode returns the code of an error body, or "" when the body is not one.
func errorCode(body []byte) string {
	var resp contract.ErrorBody
	if err := json.Unmarshal(body, &resp); err != nil || resp.Error == nil {
		return ""
	}
	return resp.Error.Code
}

// events reads a whole stream through the API, oldest first.
func (r *daemonRun) events(t *testing.T, stream string) []contract.Event {
	t.Helper()
	code, body := r.do(t, http.MethodGet, "/api/streams/"+stream+"/events?limit=1000", "")
	if code != http.StatusOK {
		t.Fatalf("GET events of %s = %d: %s", stream, code, body)
	}
	var page contract.EventsPage
	if err := json.Unmarshal(body, &page); err != nil {
		t.Fatalf("decode events: %v: %s", err, body)
	}
	return page.Events
}

// ledgerEvents reads the `daemon` stream straight from a closed ledger file, for the
// moments when no daemon is running.
func ledgerEvents(t *testing.T, h home.Home, stream string) []ledger.Event {
	t.Helper()
	l, err := ledger.Open(context.Background(), ledger.Options{Path: h.DBPath()})
	if err != nil {
		t.Fatalf("open ledger: %v", err)
	}
	defer l.Close(context.Background())
	page, err := l.Read(context.Background(), stream, 0, 1000)
	if err != nil {
		t.Fatalf("read %s: %v", stream, err)
	}
	return page.Events
}

// wireTypes lists the types of API events, in order.
func wireTypes(events []contract.Event) []string {
	out := make([]string, len(events))
	for i, e := range events {
		out[i] = e.Type
	}
	return out
}

// ledgerTypes lists the types of ledger events, in order.
func ledgerTypes(events []ledger.Event) []string {
	out := make([]string, len(events))
	for i, e := range events {
		out[i] = e.Type
	}
	return out
}

// dataOf decodes the data of an event into a map.
func dataOf(t *testing.T, raw json.RawMessage) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("decode event data %s: %v", raw, err)
	}
	return m
}

// logRecords parses harness.log: one JSON object per line.
func logRecords(t *testing.T, h home.Home) []map[string]any {
	t.Helper()
	data, err := os.ReadFile(h.LogPath())
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	var records []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if line == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("log line is not JSON: %q: %v", line, err)
		}
		records = append(records, rec)
	}
	return records
}

// hasLog reports whether the log holds a record with the message.
func hasLog(records []map[string]any, msg string) bool {
	for _, rec := range records {
		if rec["msg"] == msg {
			return true
		}
	}
	return false
}

// eventually polls cond until it holds or the timeout passes.
func eventually(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %s waiting for %s", timeout, what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// exists reports whether a path exists.
func exists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

// lockIsFree probes the lock the way a CLI would.
func lockIsFree(t *testing.T, h home.Home) bool {
	t.Helper()
	free, err := h.LockFree()
	if err != nil {
		t.Fatalf("probe lock: %v", err)
	}
	return free
}

// exitError unwraps an *ExitError from err.
func exitError(t *testing.T, err error) *ExitError {
	t.Helper()
	var exit *ExitError
	if !errors.As(err, &exit) {
		t.Fatalf("error = %v (%T), want *ExitError", err, err)
	}
	return exit
}

// ---- Server-sent events client ----

// frame is one SSE frame.
type frame struct {
	ID    string
	Event string
	Data  string
}

// follower is a raw SSE client on the daemon socket, so a test controls exactly when and how
// fast it reads.
type follower struct {
	conn   net.Conn
	frames chan frame
	ended  chan struct{} // closed when the stream has ended (EOF, error or close)
	status int           // set before openFollower returns
	err    error         // why the read loop ended; read only after ended is closed

	keepalives atomic.Int64 // `: keepalive` comments received
}

type followOpts struct {
	lastEventID string
	// stall, when set, keeps the client from reading the body until it is closed: the
	// kernel buffers fill and the server's writes block.
	stall <-chan struct{}
	// pace is how long the client sleeps after each frame.
	pace time.Duration
}

// openFollower opens the event stream of a stream and returns once the response headers
// are in. It fails the test unless the answer is 200.
func openFollower(t *testing.T, sock, streamID string, o followOpts) *follower {
	t.Helper()
	conn, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatalf("dial %s: %v", sock, err)
	}
	req := fmt.Sprintf("GET /api/streams/%s/events/stream HTTP/1.1\r\nHost: harness\r\nAccept: text/event-stream\r\n", streamID)
	if o.lastEventID != "" {
		req += "Last-Event-ID: " + o.lastEventID + "\r\n"
	}
	req += "\r\n"
	if _, err := io.WriteString(conn, req); err != nil {
		conn.Close()
		t.Fatalf("write request: %v", err)
	}
	f := &follower{conn: conn, frames: make(chan frame, 16384), ended: make(chan struct{})}
	ready := make(chan struct{})
	go f.read(o, ready)
	t.Cleanup(f.close)
	select {
	case <-ready:
	case <-time.After(15 * time.Second):
		t.Fatal("no response headers within 15s")
	}
	if f.status != http.StatusOK {
		t.Fatalf("event stream answered %d (%v), want 200", f.status, f.err)
	}
	return f
}

func (f *follower) close() { f.conn.Close() }

func (f *follower) read(o followOpts, ready chan struct{}) {
	defer close(f.ended)
	defer close(f.frames)
	resp, err := http.ReadResponse(bufio.NewReader(f.conn), nil)
	if err != nil {
		f.err = err
		close(ready)
		return
	}
	f.status = resp.StatusCode
	close(ready)
	defer resp.Body.Close()
	if o.stall != nil {
		<-o.stall
	}
	br := bufio.NewReaderSize(resp.Body, 1<<20)
	var cur frame
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			f.err = err
			return
		}
		line = strings.TrimRight(line, "\r\n")
		switch {
		case line == "":
			if cur != (frame{}) {
				f.frames <- cur
				cur = frame{}
				if o.pace > 0 {
					time.Sleep(o.pace)
				}
			}
		case strings.HasPrefix(line, ":"): // a comment: the keepalive
			f.keepalives.Add(1)
		case strings.HasPrefix(line, "id: "):
			cur.ID = strings.TrimPrefix(line, "id: ")
		case strings.HasPrefix(line, "event: "):
			cur.Event = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			cur.Data = strings.TrimPrefix(line, "data: ")
		}
	}
}

// next returns the next frame, or false when the stream ended or the timeout passed.
func (f *follower) next(timeout time.Duration) (frame, bool) {
	select {
	case fr, ok := <-f.frames:
		return fr, ok
	case <-time.After(timeout):
		return frame{}, false
	}
}

// seqs reads event frames until n have arrived, and returns their sequence numbers. It
// fails the test on a timeout or when the stream ends first.
func (f *follower) seqs(t *testing.T, n int, timeout time.Duration) []int64 {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var out []int64
	for len(out) < n {
		fr, ok := f.next(time.Until(deadline))
		if !ok {
			t.Fatalf("got %d of %d events before the stream ended or %s passed (stream error: %v)", len(out), n, timeout, f.err)
		}
		if fr.Event == "shutdown" || fr.Event == "error" {
			t.Fatalf("got a %q frame after %d of %d events", fr.Event, len(out), n)
		}
		var seq int64
		if _, err := fmt.Sscan(fr.ID, &seq); err != nil {
			t.Fatalf("frame id %q is not a sequence: %v", fr.ID, err)
		}
		out = append(out, seq)
	}
	return out
}

// wantSequence fails unless seqs is exactly from, from+1, ..., from+len-1.
func wantSequence(t *testing.T, seqs []int64, from int64) {
	t.Helper()
	for i, s := range seqs {
		if s != from+int64(i) {
			t.Fatalf("event %d has seq %d, want %d (each seq exactly once, in order)", i, s, from+int64(i))
		}
	}
}

// waitEnded fails the test unless the stream has ended within timeout.
func (f *follower) waitEnded(t *testing.T, timeout time.Duration) {
	t.Helper()
	select {
	case <-f.ended:
	case <-time.After(timeout):
		t.Fatalf("stream still open after %s", timeout)
	}
}

// appendTest appends one small event to a test stream through the daemon's own Appender.
func appendTest(ctx context.Context, a ledger.Appender, stream string, i int) error {
	return appendPadded(ctx, a, stream, i, 240)
}

// appendPadded appends an event whose data carries pad bytes of filler, for tests that need
// frames large enough to fill the kernel's socket buffers.
func appendPadded(ctx context.Context, a ledger.Appender, stream string, i, pad int) error {
	data, _ := json.Marshal(map[string]any{"n": i, "pad": strings.Repeat("x", pad)})
	_, _, err := a.Append(ctx, ledger.NewEvent{Stream: stream, Type: "test.tick", Data: data})
	return err
}

// ---- Goroutine leak check ----

// checkLeaks is leakcheck.Check, with one exception. gopkg.in/natefinch/lumberjack.v2
// v2.2.1 starts a "mill" goroutine for every Logger and never stops it, not even in Close,
// so every Run in a test process leaves one behind (a daemon process exits, so production
// is unaffected). Those goroutines are not counted; every other goroutine a test starts
// must be gone, up to three seconds after the test ended.
func checkLeaks(t testing.TB) {
	t.Helper()
	baseline := goroutinesExceptLumberjack()
	t.Cleanup(func() {
		deadline := time.Now().Add(3 * time.Second)
		for {
			n := goroutinesExceptLumberjack()
			if n <= baseline {
				return
			}
			if time.Now().After(deadline) {
				buf := make([]byte, 1<<16)
				buf = buf[:runtime.Stack(buf, true)]
				t.Errorf("goroutine leak: %d running, baseline %d\n%s", n, baseline, buf)
				return
			}
			time.Sleep(time.Millisecond)
		}
	})
}

func goroutinesExceptLumberjack() int {
	buf := make([]byte, 1<<16)
	for {
		n := runtime.Stack(buf, true)
		if n < len(buf) {
			buf = buf[:n]
			break
		}
		buf = make([]byte, 2*len(buf))
	}
	count := 0
	for _, g := range strings.Split(string(buf), "\n\n") {
		if strings.TrimSpace(g) == "" || strings.Contains(g, "lumberjack") {
			continue
		}
		count++
	}
	return count
}

// ---- Holding hooks ----

// hold is a gate a hook waits at: the daemon is held there until the test opens it.
type hold struct {
	entered     chan struct{}
	release     chan struct{}
	enterOnce   sync.Once
	releaseOnce sync.Once
}

func newHold() *hold {
	return &hold{entered: make(chan struct{}), release: make(chan struct{})}
}

// wait is called by the hook: it reports that the daemon arrived, then blocks.
func (h *hold) wait() {
	h.enterOnce.Do(func() { close(h.entered) })
	<-h.release
}

// open lets the held hook go. It is idempotent.
func (h *hold) open() { h.releaseOnce.Do(func() { close(h.release) }) }

// waitEntered fails the test unless the daemon reached the hold within 30 s.
func (h *hold) waitEntered(t *testing.T) {
	t.Helper()
	select {
	case <-h.entered:
	case <-time.After(30 * time.Second):
		t.Fatal("the daemon did not reach the hold within 30s")
	}
}

// releaseAtCleanup opens the hold when the test ends, so an abandoned hook goroutine can
// finish. Call it right after begin: cleanups run last-in first-out, so the hold opens
// before the daemon's own cleanup waits for it to stop.
func (r *daemonRun) releaseAtCleanup(h *hold) {
	r.t.Cleanup(h.open)
}

// holdBootAt returns hooks that hold the boot after step n.
func holdBootAt(h *hold, n int) *Hooks {
	return &Hooks{AfterBootStep: func(step int) error {
		if step == n {
			h.wait()
		}
		return nil
	}}
}

// holdShutdownAt returns hooks that hold the shutdown before step n.
func holdShutdownAt(h *hold, n int) *Hooks {
	return &Hooks{BeforeShutdownStep: func(step int) {
		if step == n {
			h.wait()
		}
	}}
}

// writeConfig writes <home>/config.toml.
func writeConfig(t *testing.T, h home.Home, content string) {
	t.Helper()
	if err := os.WriteFile(h.ConfigPath(), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func goVersion() string { return runtime.Version() }
