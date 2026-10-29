package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/felipmateus/harness/internal/admission"
	"github.com/felipmateus/harness/internal/api/contract"
	"github.com/felipmateus/harness/internal/ledger"
	"github.com/felipmateus/harness/internal/stream"
)

// fakeStatus is a statusSource with fixed answers.
type fakeStatus struct {
	mu     sync.Mutex
	status contract.Status
	err    error
	info   contract.ShutdownInfo
}

func (f *fakeStatus) Status(context.Context) (contract.Status, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.status, f.err
}

func (f *fakeStatus) ShutdownInfo() contract.ShutdownInfo {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.info
}

// readCall is one recorded eventReader.Read.
type readCall struct {
	stream string
	after  int64
	limit  int
}

// fakeEvents is an eventReader that records its calls and returns a canned page.
type fakeEvents struct {
	mu      sync.Mutex
	page    ledger.Page
	head    int64
	readErr error
	headErr error
	reads   []readCall
}

func (f *fakeEvents) Read(_ context.Context, stream string, after int64, limit int) (ledger.Page, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reads = append(f.reads, readCall{stream, after, limit})
	return f.page, f.readErr
}

func (f *fakeEvents) Head(context.Context, string) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.head, f.headErr
}

func (f *fakeEvents) readCalls() []readCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]readCall(nil), f.reads...)
}

// fakeDoctor is a doctorRunner with a fixed report.
type fakeDoctor struct {
	report contract.Doctor
	err    error
}

func (f *fakeDoctor) Run(context.Context) (contract.Doctor, error) { return f.report, f.err }

// followCall is one recorded FollowFunc call.
type followCall struct {
	stream string
	after  int64
}

// fakeFollow is the follow function of the SSE tests. The test pushes items into items and
// closes it to end the follow; ctxDone is closed when the follow's context ends.
type fakeFollow struct {
	items   chan stream.Item
	ctxDone chan struct{}
	started chan struct{}

	mu    sync.Mutex
	calls []followCall
	once  sync.Once
}

func newFakeFollow() *fakeFollow {
	return &fakeFollow{
		items:   make(chan stream.Item, 64),
		ctxDone: make(chan struct{}),
		started: make(chan struct{}),
	}
}

func (f *fakeFollow) follow(ctx context.Context, streamID string, after int64) <-chan stream.Item {
	f.mu.Lock()
	f.calls = append(f.calls, followCall{streamID, after})
	f.mu.Unlock()
	f.once.Do(func() { close(f.started) })
	go func() {
		<-ctx.Done()
		select {
		case <-f.ctxDone:
		default:
			close(f.ctxDone)
		}
	}()
	return f.items
}

func (f *fakeFollow) callList() []followCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]followCall(nil), f.calls...)
}

// logBuffer captures slog output as JSON lines.
type logBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *logBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *logBuffer) records(t testing.TB) []map[string]any {
	t.Helper()
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(l.buf.String()), "\n") {
		if line == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("log line %q: %v", line, err)
		}
		out = append(out, rec)
	}
	return out
}

// find returns the records with the given message.
func (l *logBuffer) find(t testing.TB, msg string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, rec := range l.records(t) {
		if rec["msg"] == msg {
			out = append(out, rec)
		}
	}
	return out
}

// fixture is a Server built from fakes.
type fixture struct {
	srv    *Server
	gate   *admission.Lifecycle
	status *fakeStatus
	events *fakeEvents
	doctor *fakeDoctor
	follow *fakeFollow
	logs   *logBuffer
	stops  atomic.Int32
}

// newFixture builds a Server with the gate moved to state. mods adjust the Options before
// the Server is built.
func newFixture(t testing.TB, state admission.State, mods ...func(*Options)) *fixture {
	t.Helper()
	fx := &fixture{
		gate: admission.New(),
		status: &fakeStatus{
			status: contract.Status{
				PID:           48211,
				Version:       "0.1.0-dev",
				StartedAt:     "2026-10-28T22:10:03.140Z",
				UptimeSeconds: 72,
				Home:          "/h",
				Socket:        "/h/daemon.sock",
				Ledger:        contract.LedgerStatus{Path: "/h/harness.db", SchemaVersion: 1, Streams: 1, Events: 2},
				Config:        contract.ConfigStatus{Path: "/h/config.toml"},
			},
			info: contract.ShutdownInfo{PID: 48211, UptimeSeconds: 72, ShutdownGrace: "30s"},
		},
		events: &fakeEvents{},
		doctor: &fakeDoctor{report: contract.Doctor{
			Status: contract.SeverityOK, Source: contract.SourceDaemon,
			Items:   []contract.DoctorItem{{ID: "home.permissions", Severity: contract.SeverityOK, Message: "home is private"}},
			Summary: contract.DoctorSummary{OK: 1},
		}},
		follow: newFakeFollow(),
		logs:   &logBuffer{},
	}
	fx.gate.Set(state)
	opts := Options{
		Gate:        fx.gate,
		Status:      fx.status,
		Events:      fx.events,
		Doctor:      fx.doctor,
		Follow:      fx.follow.follow,
		RequestStop: func() { fx.stops.Add(1) },
		Logger:      slog.New(slog.NewJSONHandler(fx.logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
	}
	for _, mod := range mods {
		mod(&opts)
	}
	srv, err := New(opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	fx.srv = srv
	return fx
}

// do sends one request through the handler and returns the recorded response.
func (fx *fixture) do(method, target, body string, headers ...string) *httptest.ResponseRecorder {
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, target, rd)
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	rec := httptest.NewRecorder()
	fx.srv.ServeHTTP(rec, req)
	return rec
}

// wantError asserts rec is the JSON error with the given status, code and message.
func wantError(t testing.TB, rec *httptest.ResponseRecorder, status int, code, message string) {
	t.Helper()
	if rec.Code != status {
		t.Fatalf("status = %d, want %d; body %s", rec.Code, status, rec.Body)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json; body %s", ct, rec.Body)
	}
	var body contract.ErrorBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || body.Error == nil {
		t.Fatalf("body %q is not an error body: %v", rec.Body, err)
	}
	if body.Error.Code != code {
		t.Fatalf("code = %q, want %q; body %s", body.Error.Code, code, rec.Body)
	}
	if message != "" && body.Error.Message != message {
		t.Fatalf("message = %q, want %q", body.Error.Message, message)
	}
}

// ledgerEvent builds a ledger event with a fixed time.
func ledgerEvent(streamID string, seq int64, typ string, data string) ledger.Event {
	var tm ledger.Time
	if err := json.Unmarshal([]byte(`"2026-10-28T22:10:03.140Z"`), &tm); err != nil {
		panic(err)
	}
	return ledger.Event{
		Pos:    seq,
		Stream: streamID,
		Seq:    seq,
		ID:     "01JBJ7QK3W5Z8V2N4T6R9YXC1D",
		Type:   typ,
		Time:   tm,
		Data:   json.RawMessage(data),
	}
}

// within fails the test if cond does not become true in d.
func within(t testing.TB, d time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("%s did not happen within %v", what, d)
		}
		time.Sleep(time.Millisecond)
	}
}

// rawRequest sends request bytes over a fresh TCP connection and returns everything the
// server answers until it closes the connection.
func rawRequest(t testing.TB, addr, request string) string {
	t.Helper()
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.WriteString(conn, request); err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(conn)
	return string(b)
}

// newClient returns an HTTP client with no keep-alive, so a test leaves no connection
// goroutines behind. A server that never sends its response head fails the test after 5s
// instead of hanging it.
func newClient(t testing.TB) *http.Client {
	t.Helper()
	tr := &http.Transport{DisableKeepAlives: true, ResponseHeaderTimeout: 5 * time.Second}
	t.Cleanup(tr.CloseIdleConnections)
	return &http.Client{Transport: tr}
}

// newTextLogger returns a JSON logger at the given level writing to w.
func newTextLogger(w io.Writer, level slog.Level) *slog.Logger {
	return slog.New(slog.NewJSONHandler(w, &slog.HandlerOptions{Level: level}))
}
