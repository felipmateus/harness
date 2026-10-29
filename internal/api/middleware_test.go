package api

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/felipmateus/harness/internal/admission"
	"github.com/felipmateus/harness/internal/api/contract"
	"github.com/felipmateus/harness/internal/testutil/leakcheck"
)

// probeHandler builds a Server's full chain around one test route, so a test can reach the
// ResponseWriter a handler really receives.
func probeHandler(fx *fixture, rt route) http.Handler {
	rt.pattern = "/probe"
	if rt.method == "" {
		rt.method = http.MethodGet
	}
	return fx.srv.buildHandler([]route{rt})
}

// UT-056
func TestPanicBecomesInternalErrorWithoutStackOrSecrets(t *testing.T) {
	fx := newFixture(t, admission.Running)
	h := probeHandler(fx, route{handle: func(http.ResponseWriter, *http.Request, url.Values) {
		panic("kaboom: dial https://user:hunter2@db password=hunter2")
	}})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/probe", nil))

	wantError(t, rec, 500, contract.CodeInternal, "internal error")
	body := rec.Body.String()
	for _, leak := range []string{"hunter2", "kaboom", "goroutine", ".go:", "runtime", "panic"} {
		if strings.Contains(body, leak) {
			t.Fatalf("body leaks %q: %s", leak, body)
		}
	}

	recs := fx.logs.find(t, "panic recovered")
	if len(recs) != 1 {
		t.Fatalf("want one `panic recovered` record, got %v", fx.logs.records(t))
	}
	if recs[0]["level"] != "ERROR" || recs[0]["method"] != "GET" || recs[0]["path"] != "/probe" {
		t.Fatalf("record = %v", recs[0])
	}
	if p, _ := recs[0]["panic"].(string); !strings.Contains(p, "kaboom") || strings.Contains(p, "hunter2") {
		t.Fatalf("the logged panic must name the fault and be redacted, got %q", p)
	}
	if stack, _ := recs[0]["stack"].(string); !strings.Contains(stack, "goroutine") || strings.Contains(stack, "hunter2") {
		t.Fatalf("the log must carry a stack, redacted; got %q", stack)
	}
}

// A panic after the response has started must abort the connection: finishing it cleanly
// would let the client take a truncated body for a complete one.
func TestPanicAfterTheResponseStartedAbortsTheConnection(t *testing.T) {
	leakcheck.Check(t)
	fx := newFixture(t, admission.Running)
	h := probeHandler(fx, route{handle: func(w http.ResponseWriter, _ *http.Request, _ url.Values) {
		w.WriteHeader(200)
		_, _ = w.Write([]byte("partial"))
		_ = http.NewResponseController(w).Flush()
		panic("late fault")
	}})
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	resp, err := newClient(t).Get(srv.URL + "/probe")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err == nil {
		t.Fatalf("the body %q ended cleanly after a panic", body)
	}
	if recs := fx.logs.find(t, "panic recovered"); len(recs) != 1 {
		t.Fatalf("want one `panic recovered` record, got %v", fx.logs.records(t))
	}
}

// net/http's own abort signal is not a fault and must not be logged as one.
func TestAbortHandlerIsNotLoggedAsAPanic(t *testing.T) {
	fx := newFixture(t, admission.Running)
	h := probeHandler(fx, route{handle: func(http.ResponseWriter, *http.Request, url.Values) { panic(http.ErrAbortHandler) }})
	defer func() {
		if v := recover(); v != http.ErrAbortHandler {
			t.Fatalf("recovered %v, want http.ErrAbortHandler to propagate", v)
		}
		if recs := fx.logs.find(t, "panic recovered"); len(recs) != 0 {
			t.Fatalf("an abort was logged as a panic: %v", recs)
		}
	}()
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/probe", nil))
}

// UT-117
func TestResponseControllerReachesTheConnectionThroughTheChain(t *testing.T) {
	leakcheck.Check(t)
	fx := newFixture(t, admission.Running)

	type result struct {
		depth    int
		flush    error
		deadline error
	}
	got := make(chan result, 1)

	// Probe with the real chain: recovery, admission and in-flight tracking, logging.
	probe := probeHandler(fx, route{handle: func(w http.ResponseWriter, _ *http.Request, _ url.Values) {
		depth := 0
		raw := w
		for {
			u, ok := raw.(interface{ Unwrap() http.ResponseWriter })
			if !ok {
				break
			}
			depth++
			raw = u.Unwrap()
		}
		rc := http.NewResponseController(w)
		got <- result{
			depth:    depth,
			flush:    rc.Flush(),
			deadline: rc.SetWriteDeadline(time.Now().Add(time.Second)),
		}
	}})
	srv := httptest.NewServer(probe)
	t.Cleanup(srv.Close)

	resp, err := newClient(t).Get(srv.URL + "/probe")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	r := <-got
	if r.depth < 2 {
		t.Fatalf("the handler saw %d wrapper(s); the chain should wrap the writer at least twice (recovery and logging)", r.depth)
	}
	if r.flush != nil {
		t.Errorf("Flush through the chain: %v", r.flush)
	}
	if r.deadline != nil {
		t.Errorf("SetWriteDeadline through the chain: %v", r.deadline)
	}
}

// The control for UT-117: a wrapper without Unwrap hides the connection, which is exactly
// what the chain must not do.
func TestAWrapperWithoutUnwrapWouldHideTheConnection(t *testing.T) {
	type hiding struct{ http.ResponseWriter }
	rec := httptest.NewRecorder()
	if err := http.NewResponseController(hiding{rec}).Flush(); !errors.Is(err, http.ErrNotSupported) {
		t.Fatalf("Flush through a wrapper with no Unwrap = %v, want ErrNotSupported", err)
	}
	if err := http.NewResponseController(&trackingWriter{ResponseWriter: rec}).Flush(); err != nil {
		t.Fatalf("Flush through trackingWriter: %v", err)
	}
}

func TestTrackingWriterRecordsTheFinalStatus(t *testing.T) {
	t.Run("explicit status", func(t *testing.T) {
		tw := &trackingWriter{ResponseWriter: httptest.NewRecorder()}
		tw.WriteHeader(http.StatusTeapot)
		if !tw.started || tw.status != http.StatusTeapot {
			t.Fatalf("started=%v status=%d", tw.started, tw.status)
		}
	})
	t.Run("implicit 200 on first write", func(t *testing.T) {
		tw := &trackingWriter{ResponseWriter: httptest.NewRecorder()}
		_, _ = tw.Write([]byte("x"))
		if !tw.started || tw.status != 200 {
			t.Fatalf("started=%v status=%d", tw.started, tw.status)
		}
	})
	t.Run("informational status does not count as started", func(t *testing.T) {
		tw := &trackingWriter{ResponseWriter: httptest.NewRecorder()}
		tw.WriteHeader(http.StatusEarlyHints)
		if tw.started {
			t.Fatal("a 103 response is not the final one")
		}
	})
}

// Admission and in-flight tracking: a non-stream request holds the drain, a stream and the
// exempt routes do not.
func TestInFlightTracking(t *testing.T) {
	leakcheck.Check(t)

	// hold runs a request whose handler blocks until release is closed.
	hold := func(t *testing.T, fx *fixture, rt route) (started chan struct{}, release chan struct{}, done chan struct{}) {
		started, release, done = make(chan struct{}), make(chan struct{}), make(chan struct{})
		rt.handle = func(http.ResponseWriter, *http.Request, url.Values) {
			close(started)
			<-release
		}
		h := probeHandler(fx, rt)
		go func() {
			defer close(done)
			h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/probe", nil))
		}()
		<-started
		return started, release, done
	}
	waitFor := func(fx *fixture, d time.Duration) error {
		ctx, cancel := context.WithTimeout(context.Background(), d)
		defer cancel()
		return fx.gate.Wait(ctx)
	}

	t.Run("a plain route holds the drain until it returns", func(t *testing.T) {
		fx := newFixture(t, admission.Running)
		_, release, done := hold(t, fx, route{})
		fx.gate.Set(admission.Draining)
		if err := waitFor(fx, 40*time.Millisecond); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("Wait = %v with a request in flight, want a timeout", err)
		}
		close(release)
		<-done
		if err := waitFor(fx, time.Second); err != nil {
			t.Fatalf("Wait after the request returned: %v", err)
		}
	})

	t.Run("a stream route is admitted but never holds the drain", func(t *testing.T) {
		fx := newFixture(t, admission.Running)
		_, release, done := hold(t, fx, route{stream: true})
		fx.gate.Set(admission.Draining)
		if err := waitFor(fx, time.Second); err != nil {
			t.Fatalf("Wait = %v; an open stream must not count as in flight", err)
		}
		close(release)
		<-done
	})

	t.Run("an exempt route never touches the gate", func(t *testing.T) {
		fx := newFixture(t, admission.Running)
		_, release, done := hold(t, fx, route{exempt: true})
		if err := waitFor(fx, time.Second); err != nil {
			t.Fatalf("Wait = %v; an exempt route must not count as in flight", err)
		}
		close(release)
		<-done
	})

	t.Run("a panic still releases the request", func(t *testing.T) {
		fx := newFixture(t, admission.Running)
		h := probeHandler(fx, route{handle: func(http.ResponseWriter, *http.Request, url.Values) { panic("fault") }})
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/probe", nil))
		if err := waitFor(fx, time.Second); err != nil {
			t.Fatalf("Wait after a panicking request: %v", err)
		}
	})

	t.Run("a refused request is not counted", func(t *testing.T) {
		fx := newFixture(t, admission.Draining)
		var ran atomic.Bool
		h := probeHandler(fx, route{handle: func(http.ResponseWriter, *http.Request, url.Values) { ran.Store(true) }})
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", "/probe", nil))
		wantError(t, rec, 503, contract.CodeDaemonDraining, "")
		if ran.Load() {
			t.Fatal("the handler ran for a refused request")
		}
		if err := waitFor(fx, time.Second); err != nil {
			t.Fatalf("Wait: %v", err)
		}
	})
}

// A gate that fails in an unexpected way is an internal error, not a refusal.
func TestUnexpectedGateErrorIsInternal(t *testing.T) {
	fx := newFixture(t, admission.Running, func(o *Options) { o.Gate = brokenGate{} })
	rec := fx.do("GET", "/api/doctor", "")
	wantError(t, rec, 500, contract.CodeInternal, "internal error")
}

type brokenGate struct{}

func (brokenGate) State() admission.State { return admission.Running }
func (brokenGate) Enter() (func(), error) { return nil, errors.New("gate imploded") }

func TestDebugRequestLog(t *testing.T) {
	fx := newFixture(t, admission.Running)
	fx.do("GET", "/api/status", "")
	fx.do("GET", "/api/nope", "")

	recs := fx.logs.find(t, "request")
	if len(recs) != 2 {
		t.Fatalf("want 2 `request` records, got %v", fx.logs.records(t))
	}
	for i, want := range []struct {
		path   string
		status float64
	}{{"/api/status", 200}, {"/api/nope", 404}} {
		rec := recs[i]
		if rec["level"] != "DEBUG" || rec["method"] != "GET" || rec["path"] != want.path || rec["status"] != want.status {
			t.Errorf("record %d = %v", i, rec)
		}
		if _, ok := rec["duration_ms"].(float64); !ok {
			t.Errorf("record %d has no duration_ms: %v", i, rec)
		}
	}
}

func TestDebugRequestLogIsSilentAtInfo(t *testing.T) {
	logs := &logBuffer{}
	fx := newFixture(t, admission.Running, func(o *Options) {
		o.Logger = newTextLogger(logs, 0) // info
	})
	fx.do("GET", "/api/status", "")
	if got := logs.find(t, "request"); len(got) != 0 {
		t.Fatalf("a debug record was written at info level: %v", got)
	}
}
