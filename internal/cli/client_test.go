package cli

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/felipmateus/harness/internal/api/contract"
	"github.com/felipmateus/harness/internal/testutil/leakcheck"
	"github.com/felipmateus/harness/internal/testutil/testhome"
)

// serveUnix serves h on a Unix socket in a temporary home and returns the socket path.
func serveUnix(t *testing.T, h http.Handler) string {
	t.Helper()
	sock := testhome.New(t).SocketPath()
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: h}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		srv.Serve(ln)
	}()
	t.Cleanup(func() {
		srv.Close()
		wg.Wait()
	})
	return sock
}

type seen struct {
	mu      sync.Mutex
	method  string
	path    string
	query   string
	body    string
	accept  string
	ctype   string
	lastEvt string
}

func (s *seen) record(r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.method, s.path, s.query, s.body = r.Method, r.URL.EscapedPath(), r.URL.RawQuery, string(body)
	s.accept, s.ctype, s.lastEvt = r.Header.Get("Accept"), r.Header.Get("Content-Type"), r.Header.Get("Last-Event-ID")
}

func TestClientStatus(t *testing.T) {
	var got seen
	sock := serveUnix(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.record(r)
		fmt.Fprint(w, statusDocument)
	}))
	st, err := newClient(sock).Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if st.PID != 48211 || st.State != "running" || st.Ledger.Events != 2 {
		t.Errorf("status %+v", st)
	}
	if got.method != "GET" || got.path != "/api/status" {
		t.Errorf("requested %s %s", got.method, got.path)
	}
}

// The shutdown request is a POST with the one reason the API accepts.
func TestClientShutdown(t *testing.T) {
	var got seen
	sock := serveUnix(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.record(r)
		w.WriteHeader(http.StatusAccepted)
		fmt.Fprint(w, `{"daemon":{"stopping":true,"pid":7,"uptime_seconds":72,"shutdown_grace":"30s"}}`)
	}))
	info, err := newClient(sock).Shutdown(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !info.Stopping || info.PID != 7 || info.UptimeSeconds != 72 || info.ShutdownGrace != "30s" {
		t.Errorf("info %+v", info)
	}
	if got.method != "POST" || got.path != "/api/daemon/shutdown" || got.body != `{"reason":"request"}` || got.ctype != "application/json" {
		t.Errorf("requested %s %s body %q type %q", got.method, got.path, got.body, got.ctype)
	}
}

func TestClientEventsQuery(t *testing.T) {
	var got seen
	sock := serveUnix(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.record(r)
		fmt.Fprint(w, `{"events":[],"page":{"after":5,"next_after":5,"has_more":false,"limit":10,"head":5}}`)
	}))
	page, err := newClient(sock).Events(context.Background(), "session:01JB", 5, 10)
	if err != nil {
		t.Fatal(err)
	}
	if page.Page.Head != 5 {
		t.Errorf("page %+v", page)
	}
	if got.path != "/api/streams/session:01JB/events" || got.query != "after=5&limit=10" {
		t.Errorf("requested %s?%s", got.path, got.query)
	}
}

// UT-127: the follow request carries `after` and never `limit`, and asks for an event stream.
func TestUT127_FollowRequestHasNoLimit(t *testing.T) {
	var got seen
	sock := serveUnix(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.record(r)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, shutdownFrame)
	}))
	e := newEnv(t)
	e.app.newClient = func(string) daemonAPI { return newClient(sock) }

	if code := e.run("events", "--follow", "--after", "3", "--limit", "10", "-o", "jsonl"); code != 0 {
		t.Fatalf("exit %d, stderr %q", code, e.err())
	}
	if got.path != "/api/streams/daemon/events/stream" || got.query != "after=3" {
		t.Errorf("requested %s?%s, want after=3 and nothing else", got.path, got.query)
	}
	if got.accept != "text/event-stream" {
		t.Errorf("Accept %q", got.accept)
	}
	if e.out() != `{"closed":{"reason":"daemon stopped"}}`+"\n" {
		t.Errorf("stdout %q", e.out())
	}
}

// Error bodies of the API come back as the daemon wrote them.
func TestClientDecodesTheErrorShape(t *testing.T) {
	sock := serveUnix(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/status":
			w.WriteHeader(http.StatusServiceUnavailable)
			fmt.Fprint(w, `{"error":{"code":"daemon_not_ready","message":"daemon is starting"}}`)
		case "/api/daemon/shutdown":
			w.WriteHeader(http.StatusInternalServerError)
			fmt.Fprint(w, "<html>boom</html>")
		default:
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprint(w, `{"error":{"code":"invalid_limit","message":"limit 5000 must be between 1 and 1000","hint":"h"}}`)
		}
	}))
	c := newClient(sock)

	_, err := c.Status(context.Background())
	if w := asWireError(err); w.Code != contract.CodeDaemonNotReady || w.Message != "daemon is starting" {
		t.Errorf("status error %+v", w)
	}
	_, err = c.Shutdown(context.Background())
	if w := asWireError(err); w.Code != contract.CodeInternal || !strings.Contains(w.Message, "HTTP 500") {
		t.Errorf("shutdown error %+v", w)
	}
	_, err = c.Events(context.Background(), "daemon", 0, 5000)
	if w := asWireError(err); w.Code != contract.CodeInvalidLimit || w.Hint != "h" {
		t.Errorf("events error %+v", w)
	}
	_, err = c.OpenStream(context.Background(), "daemon", 0)
	if w := asWireError(err); w.Code != contract.CodeInvalidLimit {
		t.Errorf("stream error %+v", w)
	}
}

// No socket, and a socket nobody serves, are both "no daemon".
func TestClientNoDaemon(t *testing.T) {
	h := testhome.New(t)
	c := newClient(h.SocketPath())
	if _, err := c.Status(context.Background()); !isUnavailable(err) || asWireError(err).Message != "no daemon is listening on "+h.SocketPath() {
		t.Errorf("missing socket: %v", err)
	}

	// A stale socket file: a listener that was closed without unlinking.
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: h.SocketPath(), Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	ln.SetUnlinkOnClose(false)
	ln.Close()
	if _, err := c.Shutdown(context.Background()); !isUnavailable(err) || asWireError(err).Message != "no daemon is listening on "+h.SocketPath() {
		t.Errorf("stale socket: %v", err)
	}
}

// A daemon that accepts and never answers is unavailable after the request timeout, not a hang.
func TestClientRequestTimeout(t *testing.T) {
	leakcheck.Check(t)
	release := make(chan struct{})
	sock := serveUnix(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(func() { close(release) })
	c := newClient(sock)
	c.control = 100 * time.Millisecond

	start := time.Now()
	_, err := c.Status(context.Background())
	if !isUnavailable(err) || !strings.Contains(asWireError(err).Message, "did not answer") {
		t.Errorf("error %v", err)
	}
	if time.Since(start) > 3*time.Second {
		t.Errorf("took %v", time.Since(start))
	}
	if defaultControl := newClient(sock).control; defaultControl != 2*time.Second {
		t.Errorf("status and shutdown time out after %v, want 2s", defaultControl)
	}
}

// A cancelled context is the user's interrupt, not a daemon failure.
func TestClientCancelledContextIsAnInterrupt(t *testing.T) {
	release := make(chan struct{})
	sock := serveUnix(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(func() { close(release) })
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(50*time.Millisecond, cancel)
	if _, err := newClient(sock).Status(ctx); err != errInterrupted {
		t.Errorf("error %v, want the interrupt", err)
	}
}
