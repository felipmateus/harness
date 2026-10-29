package api

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/felipmateus/harness/internal/admission"
	"github.com/felipmateus/harness/internal/api/contract"
	"github.com/felipmateus/harness/internal/stream"
	"github.com/felipmateus/harness/internal/testutil/leakcheck"
)

const streamPath = "/api/streams/daemon/events/stream"

// smallBufListener gives every accepted connection a tiny send buffer, so a client that
// does not read stalls the server's writes after a few kilobytes instead of a few megabytes.
type smallBufListener struct{ net.Listener }

func (l smallBufListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if tc, ok := c.(*net.TCPConn); ok {
		_ = tc.SetWriteBuffer(4096)
	}
	return c, err
}

// startServer serves h on a loopback listener and closes it, waiting for every handler, at
// cleanup.
func startServer(t testing.TB, h http.Handler) *httptest.Server {
	t.Helper()
	srv := httptest.NewUnstartedServer(h)
	srv.Listener = smallBufListener{srv.Listener}
	srv.Start()
	t.Cleanup(srv.Close)
	return srv
}

// frame is one blank-line-terminated SSE frame and the time it was read.
type frame struct {
	text string
	at   time.Time
}

func (f frame) isKeepalive() bool { return f.text == ": keepalive\n\n" }

// readFrames splits body into frames until it ends.
func readFrames(body io.Reader) <-chan frame {
	out := make(chan frame, 1024)
	go func() {
		defer close(out)
		br := bufio.NewReader(body)
		var sb strings.Builder
		for {
			line, err := br.ReadString('\n')
			sb.WriteString(line)
			if line == "\n" {
				out <- frame{text: sb.String(), at: time.Now()}
				sb.Reset()
			}
			if err != nil {
				return
			}
		}
	}()
	return out
}

// openStream connects to path and returns once the response headers are in. The request is
// cancelled at cleanup.
func openStream(t testing.TB, srv *httptest.Server, path string, headers ...string) (*http.Response, <-chan frame) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	req, err := http.NewRequestWithContext(ctx, "GET", srv.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	resp, err := newClient(t).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp, readFrames(resp.Body)
}

// next waits for the next frame.
func next(t testing.TB, frames <-chan frame, d time.Duration) frame {
	t.Helper()
	select {
	case f, ok := <-frames:
		if !ok {
			t.Fatal("the stream ended; wanted another frame")
		}
		return f
	case <-time.After(d):
		t.Fatalf("no frame within %v", d)
		return frame{}
	}
}

// wantEnd asserts that the stream ends without another frame.
func wantEnd(t testing.TB, frames <-chan frame, d time.Duration) {
	t.Helper()
	select {
	case f, ok := <-frames:
		if ok {
			t.Fatalf("unexpected frame %q; wanted the end of the stream", f.text)
		}
	case <-time.After(d):
		t.Fatalf("the stream did not end within %v", d)
	}
}

// drain returns the frames that have already arrived, without waiting.
func drain(frames <-chan frame) []frame {
	var out []frame
	for {
		select {
		case f, ok := <-frames:
			if !ok {
				return out
			}
			out = append(out, f)
		default:
			return out
		}
	}
}

func (fx *fixture) push(items ...stream.Item) {
	for _, it := range items {
		fx.follow.items <- it
	}
}

func withTimings(keepalive, writeTimeout time.Duration) func(*Options) {
	return func(o *Options) {
		o.KeepaliveInterval = keepalive
		o.WriteTimeout = writeTimeout
	}
}

// UT-067
func TestEventFrameThenShutdownFrame(t *testing.T) {
	leakcheck.Check(t)
	fx := newFixture(t, admission.Running)
	srv := startServer(t, fx.srv)

	ev := ledgerEvent("daemon", 2, "daemon.started", `{"pid":48211,"version":"0.1.0-dev","boot_ms":41}`)
	fx.push(stream.Item{Event: ev}, stream.Item{Err: stream.ErrClosed})

	resp, err := newClient(t).Get(srv.URL + streamPath)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading the stream to its end: %v", err)
	}

	// The event JSON is exactly what ledger.Event marshals to: contract.Event mirrors it.
	eventJSON, err := json.Marshal(ev)
	if err != nil {
		t.Fatal(err)
	}
	want := "id: 2\nevent: daemon.started\ndata: " + string(eventJSON) + "\n\n" +
		"event: shutdown\ndata: {\"reason\":\"daemon stopped\"}\n\n"
	if string(body) != want {
		t.Fatalf("body =\n%q\nwant\n%q", body, want)
	}
	if want := `{"stream":"daemon","seq":2,"id":"01JBJ7QK3W5Z8V2N4T6R9YXC1D","type":"daemon.started","time":"2026-10-28T22:10:03.140Z","data":{"pid":48211,"version":"0.1.0-dev","boot_ms":41}}`; string(eventJSON) != want {
		t.Fatalf("event JSON = %s, want the _dx.md shape %s", eventJSON, want)
	}

	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "text/event-stream" || resp.Header.Get("Cache-Control") != "no-cache" {
		t.Fatalf("headers: %d %v", resp.StatusCode, resp.Header)
	}
}

// The response head is flushed at once, so a follower on an empty stream knows it is
// connected before any event exists.
func TestHeadersAreFlushedBeforeTheFirstEvent(t *testing.T) {
	leakcheck.Check(t)
	fx := newFixture(t, admission.Running)
	srv := startServer(t, fx.srv)

	resp, frames := openStream(t, srv, streamPath)
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("got %d %v", resp.StatusCode, resp.Header)
	}
	if got := drain(frames); len(got) != 0 {
		t.Fatalf("frames before any event: %v", got)
	}
}

// UT-068
func TestReaderErrorBecomesAnErrorFrameAndIsLogged(t *testing.T) {
	leakcheck.Check(t)
	fx := newFixture(t, admission.Running)
	srv := startServer(t, fx.srv)

	fx.push(
		stream.Item{Event: ledgerEvent("daemon", 1, "config.loaded", `{"pid":1}`)},
		stream.Item{Err: errors.New("ledger: read: database disk image is malformed")},
	)

	resp, err := newClient(t).Get(srv.URL + streamPath)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}

	const wantTail = "event: error\ndata: {\"error\":{\"code\":\"internal\",\"message\":\"internal error\"}}\n\n"
	if !strings.HasSuffix(string(body), wantTail) {
		t.Fatalf("body = %q, want it to end with %q", body, wantTail)
	}
	if strings.Contains(string(body), "shutdown") || strings.Contains(string(body), "malformed") {
		t.Fatalf("body has a shutdown frame or leaks the cause: %q", body)
	}
	if n := strings.Count(string(body), "event: error"); n != 1 {
		t.Fatalf("%d error frames, want 1", n)
	}

	recs := fx.logs.find(t, "stream error")
	if len(recs) != 1 {
		t.Fatalf("want one `stream error` record, got %v", fx.logs.records(t))
	}
	rec := recs[0]
	if rec["level"] != "ERROR" || rec["stream"] != "daemon" || rec["cursor"] != float64(1) {
		t.Fatalf("record = %v", rec)
	}
	if msg, _ := rec["err"].(string); !strings.Contains(msg, "malformed") {
		t.Fatalf("the cause must be in the log, got err=%v", rec["err"])
	}
}

// UT-069
func TestLastEventIDOverridesAfter(t *testing.T) {
	leakcheck.Check(t)
	tests := []struct {
		name    string
		target  string
		headers []string
		want    int64
	}{
		{"no cursor", streamPath, nil, 0},
		{"after only", streamPath + "?after=5", nil, 5},
		{"last-event-id overrides after", streamPath + "?after=0", []string{"Last-Event-ID", "3"}, 3},
		{"last-event-id alone", streamPath, []string{"Last-Event-ID", "9"}, 9},
		{"an empty last-event-id is absent", streamPath + "?after=4", []string{"Last-Event-ID", ""}, 4},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fx := newFixture(t, admission.Running)
			srv := startServer(t, fx.srv)
			openStream(t, srv, tt.target, tt.headers...)
			<-fx.follow.started
			calls := fx.follow.callList()
			if len(calls) != 1 || calls[0] != (followCall{"daemon", tt.want}) {
				t.Fatalf("follow calls = %+v, want one {daemon %d}", calls, tt.want)
			}
		})
	}
}

// UT-069 (error half) and the other rejections that come before the stream starts.
func TestStreamRequestsAreValidatedBeforeAnyFrame(t *testing.T) {
	tests := []struct {
		name    string
		target  string
		headers []string
		code    string
		message string
	}{
		{"malformed Last-Event-ID", streamPath, []string{"Last-Event-ID", "x"}, contract.CodeInvalidCursor, "after x must be >= 0"},
		{"negative Last-Event-ID", streamPath, []string{"Last-Event-ID", "-2"}, contract.CodeInvalidCursor, "after -2 must be >= 0"},
		{"negative after", streamPath + "?after=-1", nil, contract.CodeInvalidCursor, "after -1 must be >= 0"},
		{"bad after wins even when Last-Event-ID is valid", streamPath + "?after=abc", []string{"Last-Event-ID", "3"}, contract.CodeInvalidCursor, "after abc must be >= 0"},
		{"bad stream id", "/api/streams/Daemon%20Log/events/stream", nil, contract.CodeInvalidStreamID, `invalid stream id "Daemon Log": must match ^[a-z][A-Za-z0-9_.:-]{0,127}$`},
		{"limit is not a stream parameter", streamPath + "?limit=5", nil, contract.CodeInvalidRequest, `unknown parameter "limit"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fx := newFixture(t, admission.Running)
			rec := fx.do("GET", tt.target, "", tt.headers...)
			wantError(t, rec, 400, tt.code, tt.message)
			if n := len(fx.follow.callList()); n != 0 {
				t.Fatalf("Follow started %d times for a rejected request", n)
			}
		})
	}
}

func TestStreamIsRefusedByTheGate(t *testing.T) {
	for state, code := range map[admission.State]string{
		admission.Starting: contract.CodeDaemonNotReady,
		admission.Draining: contract.CodeDaemonDraining,
	} {
		fx := newFixture(t, state)
		rec := fx.do("GET", streamPath, "")
		wantError(t, rec, 503, code, "")
		if n := len(fx.follow.callList()); n != 0 {
			t.Fatalf("[%s] Follow started for a refused request", state)
		}
	}
}

// timingAttempts is how many times a wall-clock scenario may run before the test fails.
// UT-065, UT-112 and UT-113 assert tens of milliseconds, as the spec writes them, and a
// loaded machine can delay a goroutine by that much. A scenario that passes once shows the
// timing holds; a defect (a timer that does not reset, a write with no deadline) fails every
// attempt, so retrying does not let one through.
const timingAttempts = 3

func eventually(t *testing.T, scenario func() error) {
	t.Helper()
	var err error
	for range timingAttempts {
		if err = scenario(); err == nil {
			return
		}
	}
	t.Fatalf("%v (the scenario failed %d times in a row)", err, timingAttempts)
}

// session is one open event stream with its server, for scenarios that report errors
// instead of failing the test, so they can be retried.
type session struct {
	fx     *fixture
	srv    *httptest.Server
	resp   *http.Response
	frames <-chan frame
	cancel context.CancelFunc
	tr     *http.Transport
}

func openSession(fx *fixture, path string) (*session, error) {
	srv := httptest.NewUnstartedServer(fx.srv)
	srv.Listener = smallBufListener{srv.Listener}
	srv.Start()
	ctx, cancel := context.WithCancel(context.Background())
	tr := &http.Transport{DisableKeepAlives: true, ResponseHeaderTimeout: 5 * time.Second}
	req, _ := http.NewRequestWithContext(ctx, "GET", srv.URL+path, nil)
	resp, err := (&http.Client{Transport: tr}).Do(req)
	if err != nil {
		cancel()
		srv.Close()
		return nil, err
	}
	return &session{fx: fx, srv: srv, resp: resp, frames: readFrames(resp.Body), cancel: cancel, tr: tr}, nil
}

func (s *session) close() {
	s.cancel()
	s.resp.Body.Close()
	s.tr.CloseIdleConnections()
	s.srv.Close()
}

// UT-065
func TestKeepaliveAfterTheInterval(t *testing.T) {
	leakcheck.Check(t)
	eventually(t, func() error {
		fx := newFixture(t, admission.Running, withTimings(50*time.Millisecond, time.Second))
		s, err := openSession(fx, streamPath)
		if err != nil {
			return err
		}
		defer s.close()

		time.Sleep(75 * time.Millisecond)
		if got := drain(s.frames); len(got) != 1 || !got[0].isKeepalive() {
			return fmt.Errorf("frames in the first 75ms = %q, want exactly one `: keepalive`", texts(got))
		}
		return nil
	})
}

// UT-113
func TestKeepaliveTimerResetsAfterEveryFrame(t *testing.T) {
	leakcheck.Check(t)
	eventually(t, func() error {
		fx := newFixture(t, admission.Running, withTimings(50*time.Millisecond, time.Second))
		s, err := openSession(fx, streamPath)
		if err != nil {
			return err
		}
		defer s.close()

		// An item every 30ms for 200ms.
		sent := make(chan int, 1)
		go func() {
			tick := time.NewTicker(30 * time.Millisecond)
			defer tick.Stop()
			deadline := time.Now().Add(200 * time.Millisecond)
			seq := 0
			for now := range tick.C {
				if now.After(deadline) {
					break
				}
				seq++
				fx.push(stream.Item{Event: ledgerEvent("daemon", int64(seq), "tick", `{}`)})
			}
			sent <- seq
		}()

		// Collect frames up to the first keepalive.
		var before []frame
		var keepalive frame
		for {
			select {
			case f, ok := <-s.frames:
				if !ok {
					return errors.New("the stream ended before any keepalive")
				}
				if f.isKeepalive() {
					keepalive = f
				} else {
					before = append(before, f)
				}
			case <-time.After(2 * time.Second):
				return errors.New("no keepalive after the items stopped")
			}
			if keepalive.text != "" {
				break
			}
		}
		total := <-sent

		if len(before) != total {
			return fmt.Errorf("%d event frames before the first keepalive but %d were sent: a keepalive was written between events, so the timer did not reset", len(before), total)
		}
		if gap := keepalive.at.Sub(before[len(before)-1].at); gap < 45*time.Millisecond || gap > 250*time.Millisecond {
			return fmt.Errorf("the keepalive came %v after the last event, want about 50ms", gap)
		}
		return nil
	})
}

// A keepalive does not stop the timer: the next one follows one interval later.
func TestKeepalivesRepeat(t *testing.T) {
	leakcheck.Check(t)
	fx := newFixture(t, admission.Running, withTimings(30*time.Millisecond, time.Second))
	srv := startServer(t, fx.srv)
	_, frames := openStream(t, srv, streamPath)
	for range 3 {
		if f := next(t, frames, time.Second); !f.isKeepalive() {
			t.Fatalf("frame %q, want a keepalive", f.text)
		}
	}
}

// UT-112
func TestStuckClientEndsTheHandlerWithinTheWriteTimeout(t *testing.T) {
	leakcheck.Check(t)
	var fx *fixture
	eventually(t, func() error {
		fx = newFixture(t, admission.Running, withTimings(time.Minute, 50*time.Millisecond))
		srv := httptest.NewUnstartedServer(fx.srv)
		srv.Listener = smallBufListener{srv.Listener}
		srv.Start()
		defer srv.Close()

		// A raw client that sends the request and then never reads a byte.
		conn, err := net.Dial("tcp", srv.Listener.Addr().String())
		if err != nil {
			return err
		}
		defer conn.Close()
		if tc, ok := conn.(*net.TCPConn); ok {
			_ = tc.SetReadBuffer(4096)
		}
		if _, err := io.WriteString(conn, "GET "+streamPath+" HTTP/1.1\r\nHost: harness\r\n\r\n"); err != nil {
			return err
		}
		<-fx.follow.started

		// One large event: far more than the socket buffers can hold, so the write stalls.
		big := ledgerEvent("daemon", 1, "big", `"`+strings.Repeat("a", 512<<10)+`"`)
		start := time.Now()
		fx.push(stream.Item{Event: big})

		select {
		case <-fx.follow.ctxDone:
			if elapsed := time.Since(start); elapsed > 200*time.Millisecond {
				return fmt.Errorf("the follow context ended after %v, want within 200ms", elapsed)
			}
			return nil
		case <-time.After(2 * time.Second):
			return errors.New("the handler is still writing to a client that stopped reading: the follow context was never cancelled")
		}
	})

	// The handler gave up because of the write deadline, and logged it.
	var recs []map[string]any
	within(t, time.Second, "the `stream write failed` record", func() bool {
		recs = fx.logs.find(t, "stream write failed")
		return len(recs) == 1
	})
	if recs[0]["level"] != "INFO" || recs[0]["stream"] != "daemon" || recs[0]["cursor"] != float64(0) {
		t.Fatalf("record = %v", recs[0])
	}
	if msg, _ := recs[0]["err"].(string); !strings.Contains(msg, "timeout") {
		t.Fatalf("err = %v, want a timeout", recs[0]["err"])
	}
}

// deadlineWriter is a ResponseWriter that records every write deadline and write, in order.
type deadlineWriter struct {
	hdr http.Header

	mu  sync.Mutex
	ops []op
}

type op struct {
	kind     string // "deadline", "write" or "flush"
	deadline time.Time
	data     string
	at       time.Time
}

func newDeadlineWriter() *deadlineWriter { return &deadlineWriter{hdr: http.Header{}} }

func (w *deadlineWriter) Header() http.Header { return w.hdr }
func (w *deadlineWriter) WriteHeader(int)     {}
func (w *deadlineWriter) record(o op) {
	w.mu.Lock()
	defer w.mu.Unlock()
	o.at = time.Now()
	w.ops = append(w.ops, o)
}
func (w *deadlineWriter) Write(b []byte) (int, error) {
	w.record(op{kind: "write", data: string(b)})
	return len(b), nil
}
func (w *deadlineWriter) SetWriteDeadline(t time.Time) error {
	w.record(op{kind: "deadline", deadline: t})
	return nil
}
func (w *deadlineWriter) Flush() { w.record(op{kind: "flush"}) }

func (w *deadlineWriter) snapshot() []op {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]op(nil), w.ops...)
}

// deadlineBefore returns, for every write op, the deadline set most recently before it.
func deadlineBefore(t testing.TB, ops []op) map[string]time.Duration {
	t.Helper()
	out := map[string]time.Duration{}
	var last *op
	for i := range ops {
		switch ops[i].kind {
		case "deadline":
			last = &ops[i]
		case "write":
			if last == nil {
				t.Fatalf("write %q with no deadline set before it", ops[i].data)
			}
			out[ops[i].data] = last.deadline.Sub(last.at)
			last = nil // the next write needs a deadline of its own
		}
	}
	return out
}

// Safety Invariant 8: every write has a deadline, and the final frame's is the short one.
func TestEveryWriteHasItsOwnDeadlineAndTheFinalFrameTheShortOne(t *testing.T) {
	leakcheck.Check(t)
	fx := newFixture(t, admission.Running, withTimings(20*time.Millisecond, 3*time.Second))
	w := newDeadlineWriter()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		fx.srv.ServeHTTP(w, httptest.NewRequest("GET", streamPath, nil).WithContext(ctx))
	}()

	<-fx.follow.started
	ev := ledgerEvent("daemon", 1, "config.loaded", `{}`)
	fx.push(stream.Item{Event: ev})
	within(t, time.Second, "a keepalive after the event", func() bool {
		n := 0
		for _, o := range w.snapshot() {
			if o.kind == "write" && o.data == string(keepaliveFrame) {
				n++
			}
		}
		return n >= 1
	})
	fx.push(stream.Item{Err: stream.ErrClosed})
	<-done

	got := deadlineBefore(t, w.snapshot())
	eventFrame, _ := eventFrame(toContractEvent(ev))
	near := func(name string, d, want time.Duration) {
		t.Helper()
		if d > want || d < want-500*time.Millisecond {
			t.Errorf("%s deadline = %v ahead, want about %v", name, d, want)
		}
	}
	near("event frame", got[string(eventFrame)], 3*time.Second)
	near("keepalive", got[string(keepaliveFrame)], 3*time.Second)
	near("shutdown frame", got[string(shutdownFrame)], time.Second)

	// The headers flush is under a deadline too (a write with no data has no entry above,
	// so look for the first deadline directly).
	if ops := w.snapshot(); len(ops) < 2 || ops[0].kind != "deadline" || ops[1].kind != "flush" {
		t.Fatalf("the first operations must be a deadline then the headers flush, got %+v", ops[:min(2, len(ops))])
	}
}

// A writer that cannot take a deadline cannot honour Safety Invariant 8, so the stream
// fails closed: no frame is written and the follow is cancelled.
func TestNoWriteDeadlineMeansNoStream(t *testing.T) {
	leakcheck.Check(t)
	fx := newFixture(t, admission.Running)
	rec := httptest.NewRecorder() // a ResponseRecorder does not support deadlines

	// Run the handler on its own goroutine: if it wrote without a deadline it would wait
	// for the follow forever, and the test must fail instead of hanging.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		fx.srv.ServeHTTP(rec, httptest.NewRequest("GET", streamPath, nil).WithContext(ctx))
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		cancel()
		<-done
		t.Fatal("the stream kept running on a writer that cannot take a write deadline")
	}

	if body := rec.Body.String(); body != "" {
		t.Fatalf("frames written without a write deadline: %q", body)
	}
	select {
	case <-fx.follow.ctxDone:
	case <-time.After(time.Second):
		t.Fatal("the follow was not cancelled")
	}
	recs := fx.logs.find(t, "stream write failed")
	if len(recs) != 1 {
		t.Fatalf("want one `stream write failed` record, got %v", fx.logs.records(t))
	}
}

// A client that goes away ends the follow: no goroutine outlives its request.
func TestClientDisconnectCancelsTheFollow(t *testing.T) {
	leakcheck.Check(t)
	fx := newFixture(t, admission.Running)
	srv := startServer(t, fx.srv)

	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, "GET", srv.URL+streamPath, nil)
	resp, err := newClient(t).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	<-fx.follow.started

	cancel()
	select {
	case <-fx.follow.ctxDone:
	case <-time.After(2 * time.Second):
		t.Fatal("the follow context was not cancelled when the client disconnected")
	}
}

// The server-wide stream context is the backstop to the broker close: cancelling it ends
// every open stream, with no final frame.
func TestStreamContextEndsOpenStreams(t *testing.T) {
	leakcheck.Check(t)
	streamCtx, stopStreams := context.WithCancel(context.Background())
	defer stopStreams()
	fx := newFixture(t, admission.Running, func(o *Options) { o.StreamContext = streamCtx })
	srv := startServer(t, fx.srv)

	_, frames := openStream(t, srv, streamPath)
	<-fx.follow.started
	fx.push(stream.Item{Event: ledgerEvent("daemon", 1, "config.loaded", `{}`)})
	if f := next(t, frames, time.Second); !strings.HasPrefix(f.text, "id: 1\n") {
		t.Fatalf("frame %q", f.text)
	}

	stopStreams()
	select {
	case <-fx.follow.ctxDone:
	case <-time.After(time.Second):
		t.Fatal("the follow context was not cancelled by the stream context")
	}
	wantEnd(t, frames, time.Second)
}

// stream.Follow closes its channel without a terminal item when the broker closes while
// the consumer's buffer is full. A closed channel with a live context is a shutdown.
func TestClosedChannelWithALiveContextIsAShutdown(t *testing.T) {
	leakcheck.Check(t)
	fx := newFixture(t, admission.Running)
	srv := startServer(t, fx.srv)
	_, frames := openStream(t, srv, streamPath)

	fx.push(stream.Item{Event: ledgerEvent("daemon", 1, "config.loaded", `{}`)})
	close(fx.follow.items)

	if f := next(t, frames, time.Second); !strings.HasPrefix(f.text, "id: 1\n") {
		t.Fatalf("frame %q", f.text)
	}
	if f := next(t, frames, time.Second); f.text != "event: shutdown\ndata: {\"reason\":\"daemon stopped\"}\n\n" {
		t.Fatalf("frame %q, want the shutdown frame", f.text)
	}
	wantEnd(t, frames, time.Second)
}

// The adapter owns no ordering or dedupe: it frames what it is given, in the order given.
func TestAdapterFramesItemsAsGivenWithoutOrderingOrDedupe(t *testing.T) {
	leakcheck.Check(t)
	fx := newFixture(t, admission.Running)
	srv := startServer(t, fx.srv)
	_, frames := openStream(t, srv, streamPath)

	fx.push(
		stream.Item{Event: ledgerEvent("daemon", 3, "a", `{}`)},
		stream.Item{Event: ledgerEvent("daemon", 3, "a", `{}`)},
		stream.Item{Event: ledgerEvent("daemon", 2, "b", `{}`)},
	)
	var ids []string
	for range 3 {
		f := next(t, frames, time.Second)
		ids = append(ids, strings.SplitN(f.text, "\n", 2)[0])
	}
	if strings.Join(ids, ",") != "id: 3,id: 3,id: 2" {
		t.Fatalf("ids = %v, want 3,3,2 as delivered", ids)
	}
}

// Every frame is one write, so a large event arrives whole and intact.
func TestLargeEventArrivesIntact(t *testing.T) {
	leakcheck.Check(t)
	fx := newFixture(t, admission.Running)
	srv := startServer(t, fx.srv)
	_, frames := openStream(t, srv, streamPath)

	payload := strings.Repeat("x", 1<<20-2)
	fx.push(stream.Item{Event: ledgerEvent("daemon", 1, "big", `"`+payload+`"`)}, stream.Item{Err: stream.ErrClosed})

	f := next(t, frames, 5*time.Second)
	lines := strings.Split(strings.TrimSuffix(f.text, "\n\n"), "\n")
	if len(lines) != 3 || lines[0] != "id: 1" || lines[1] != "event: big" || !strings.HasPrefix(lines[2], "data: ") {
		t.Fatalf("frame has %d lines; head %q", len(lines), f.text[:min(len(f.text), 60)])
	}
	var ev contract.Event
	if err := json.Unmarshal([]byte(strings.TrimPrefix(lines[2], "data: ")), &ev); err != nil {
		t.Fatal(err)
	}
	if len(ev.Data) != 1<<20 || ev.Seq != 1 {
		t.Fatalf("data length %d seq %d", len(ev.Data), ev.Seq)
	}
}

// Each stream request starts its own follow, for its own stream, with its own context.
func TestEachRequestStartsItsOwnFollow(t *testing.T) {
	leakcheck.Check(t)
	var mu sync.Mutex
	var seen []followCall
	fx := newFixture(t, admission.Running, func(o *Options) {
		o.Follow = func(ctx context.Context, streamID string, after int64) <-chan stream.Item {
			mu.Lock()
			seen = append(seen, followCall{streamID, after})
			mu.Unlock()
			ch := make(chan stream.Item)
			go func() { <-ctx.Done(); close(ch) }()
			return ch
		}
	})
	srv := startServer(t, fx.srv)
	openStream(t, srv, "/api/streams/daemon/events/stream?after=1")
	openStream(t, srv, "/api/streams/session:01JBJ7QK3W/events/stream?after=2")
	within(t, time.Second, "both follows", func() bool { mu.Lock(); defer mu.Unlock(); return len(seen) == 2 })

	mu.Lock()
	defer mu.Unlock()
	got := map[followCall]bool{seen[0]: true, seen[1]: true}
	if !got[followCall{"daemon", 1}] || !got[followCall{"session:01JBJ7QK3W", 2}] {
		t.Fatalf("follow calls = %+v", seen)
	}
}

func TestDefaultTimingsMatchTheSpec(t *testing.T) {
	if DefaultKeepaliveInterval != 15*time.Second || DefaultWriteTimeout != 10*time.Second || finalFrameTimeout != time.Second {
		t.Fatalf("keepalive %v, write timeout %v, final frame %v; want 15s, 10s, 1s", DefaultKeepaliveInterval, DefaultWriteTimeout, finalFrameTimeout)
	}
	fx := newFixture(t, admission.Running)
	if fx.srv.keepalive != DefaultKeepaliveInterval || fx.srv.writeTO != DefaultWriteTimeout {
		t.Fatalf("a zero Options must default to %v and %v, got %v and %v", DefaultKeepaliveInterval, DefaultWriteTimeout, fx.srv.keepalive, fx.srv.writeTO)
	}
}

func TestEventFrameFormat(t *testing.T) {
	got, err := eventFrame(contract.Event{Stream: "s", Seq: 12, ID: "i", Type: "a.b", Time: "2026-10-28T22:10:03.140Z", Data: json.RawMessage("{\n  \"k\": [1,\n2]\n}")})
	if err != nil {
		t.Fatal(err)
	}
	want := "id: 12\nevent: a.b\ndata: {\"stream\":\"s\",\"seq\":12,\"id\":\"i\",\"type\":\"a.b\",\"time\":\"2026-10-28T22:10:03.140Z\",\"data\":{\"k\":[1,2]}}\n\n"
	if string(got) != want {
		t.Fatalf("frame =\n%q\nwant\n%q", got, want)
	}
	if strings.Count(string(got), "\n") != 4 {
		t.Fatalf("a frame must have exactly one data line; got %q", got)
	}
}

func texts(fs []frame) []string {
	out := make([]string, len(fs))
	for i, f := range fs {
		out[i] = f.text
	}
	return out
}
