package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/felipmateus/harness/internal/admission"
	"github.com/felipmateus/harness/internal/api/contract"
	"github.com/felipmateus/harness/internal/ledger"
)

// UT-050
func TestDrainingRefusesRoutesWithDaemonDraining(t *testing.T) {
	fx := newFixture(t, admission.Draining)
	rec := fx.do("GET", "/api/streams/daemon/events", "")
	wantError(t, rec, 503, contract.CodeDaemonDraining, "daemon is stopping")
	if n := len(fx.events.readCalls()); n != 0 {
		t.Fatalf("the reader was called %d times for a refused request", n)
	}
}

// UT-051
func TestStartingAnswersStatusAndRefusesTheRest(t *testing.T) {
	fx := newFixture(t, admission.Starting)

	rec := fx.do("GET", "/api/status", "")
	if rec.Code != 200 {
		t.Fatalf("status route while starting = %d, want 200; body %s", rec.Code, rec.Body)
	}
	var got contract.StatusResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Status.State != "starting" {
		t.Fatalf("state = %q, want starting", got.Status.State)
	}

	wantError(t, fx.do("GET", "/api/streams/daemon/events", ""), 503, contract.CodeDaemonNotReady, "daemon is starting")
	wantError(t, fx.do("GET", "/api/doctor", ""), 503, contract.CodeDaemonNotReady, "daemon is starting")
	wantError(t, fx.do("GET", "/api/streams/daemon/events/stream", ""), 503, contract.CodeDaemonNotReady, "daemon is starting")
}

// The status route is exempt in every state and reports the gate's own state.
func TestStatusShapeAndStateInEveryGateState(t *testing.T) {
	for _, state := range []admission.State{admission.Starting, admission.Running, admission.Draining} {
		t.Run(string(state), func(t *testing.T) {
			fx := newFixture(t, state)
			rec := fx.do("GET", "/api/status", "")
			if rec.Code != 200 || rec.Header().Get("Content-Type") != "application/json" {
				t.Fatalf("got %d %q: %s", rec.Code, rec.Header().Get("Content-Type"), rec.Body)
			}
			want := `{"status":{"state":"` + string(state) + `","pid":48211,"version":"0.1.0-dev","started_at":"2026-10-28T22:10:03.140Z","uptime_seconds":72,"home":"/h","socket":"/h/daemon.sock","ledger":{"path":"/h/harness.db","schema_version":1,"streams":1,"events":2},"config":{"path":"/h/config.toml","exists":false,"digest":null}}}`
			if got := strings.TrimSpace(rec.Body.String()); got != want {
				t.Fatalf("body = %s\nwant   %s", got, want)
			}
		})
	}
}

func TestStatusSourceFailureIsInternal(t *testing.T) {
	fx := newFixture(t, admission.Running)
	fx.status.err = errors.New("stats: password=hunter2 unreachable")
	rec := fx.do("GET", "/api/status", "")
	wantError(t, rec, 500, contract.CodeInternal, "internal error")
	if strings.Contains(rec.Body.String(), "hunter2") || strings.Contains(rec.Body.String(), "unreachable") {
		t.Fatalf("the cause reached the client: %s", rec.Body)
	}
	if recs := fx.logs.find(t, "request failed"); len(recs) != 1 {
		t.Fatalf("want one `request failed` record, got %v", fx.logs.records(t))
	}
}

// UT-052
func TestEventsPage(t *testing.T) {
	fx := newFixture(t, admission.Running)
	fx.events.page = ledger.Page{Events: []ledger.Event{ledgerEvent("daemon", 2, "daemon.started", `{"pid":48211,"version":"0.1.0-dev","boot_ms":41}`)}, HasMore: true}
	fx.events.head = 4

	rec := fx.do("GET", "/api/streams/daemon/events?after=1&limit=1", "")
	if rec.Code != 200 {
		t.Fatalf("status = %d; body %s", rec.Code, rec.Body)
	}
	want := `{"events":[{"stream":"daemon","seq":2,"id":"01JBJ7QK3W5Z8V2N4T6R9YXC1D","type":"daemon.started","time":"2026-10-28T22:10:03.140Z","data":{"pid":48211,"version":"0.1.0-dev","boot_ms":41}}],"page":{"after":1,"next_after":2,"has_more":true,"limit":1,"head":4}}`
	if got := strings.TrimSpace(rec.Body.String()); got != want {
		t.Fatalf("body = %s\nwant   %s", got, want)
	}
	if calls := fx.events.readCalls(); len(calls) != 1 || calls[0] != (readCall{"daemon", 1, 1}) {
		t.Fatalf("reader calls = %+v, want one {daemon 1 1}", calls)
	}
}

// A stream with no events yet is an empty page, with [] and head 0, not null.
func TestEventsPageOfAnEmptyStream(t *testing.T) {
	fx := newFixture(t, admission.Running)
	fx.events.page = ledger.Page{Events: []ledger.Event{}}

	rec := fx.do("GET", "/api/streams/daemon/events", "")
	want := `{"events":[],"page":{"after":0,"next_after":0,"has_more":false,"limit":100,"head":0}}`
	if got := strings.TrimSpace(rec.Body.String()); rec.Code != 200 || got != want {
		t.Fatalf("got %d %s\nwant 200 %s", rec.Code, got, want)
	}
	if calls := fx.events.readCalls(); len(calls) != 1 || calls[0].limit != 100 {
		t.Fatalf("the default limit must be 100, got %+v", calls)
	}
}

// With no events to advance the cursor, next_after stays at after.
func TestEventsNextAfterStaysWhenThePageIsEmpty(t *testing.T) {
	fx := newFixture(t, admission.Running)
	fx.events.page = ledger.Page{Events: nil}
	fx.events.head = 7
	rec := fx.do("GET", "/api/streams/daemon/events?after=7", "")
	want := `{"events":[],"page":{"after":7,"next_after":7,"has_more":false,"limit":100,"head":7}}`
	if got := strings.TrimSpace(rec.Body.String()); got != want {
		t.Fatalf("body = %s\nwant   %s", got, want)
	}
}

func TestEventsReaderFailureIsInternal(t *testing.T) {
	for name, set := range map[string]func(*fakeEvents){
		"read": func(f *fakeEvents) { f.readErr = errors.New("sqlite: disk I/O error") },
		"head": func(f *fakeEvents) { f.headErr = errors.New("sqlite: disk I/O error") },
	} {
		t.Run(name, func(t *testing.T) {
			fx := newFixture(t, admission.Running)
			set(fx.events)
			rec := fx.do("GET", "/api/streams/daemon/events", "")
			wantError(t, rec, 500, contract.CodeInternal, "internal error")
			if strings.Contains(rec.Body.String(), "sqlite") {
				t.Fatalf("the cause reached the client: %s", rec.Body)
			}
		})
	}
}

// UT-053
func TestEventsStreamIDValidation(t *testing.T) {
	fx := newFixture(t, admission.Running)

	rec := fx.do("GET", "/api/streams/Daemon%20Log/events", "")
	wantError(t, rec, 400, contract.CodeInvalidStreamID, `invalid stream id "Daemon Log": must match ^[a-z][A-Za-z0-9_.:-]{0,127}$`)

	rec = fx.do("GET", "/api/streams/session:01JBJ7QK3W/events", "")
	if rec.Code != 200 {
		t.Fatalf("session:01JBJ7QK3W = %d, want 200; body %s", rec.Code, rec.Body)
	}
	if calls := fx.events.readCalls(); len(calls) != 1 || calls[0].stream != "session:01JBJ7QK3W" {
		t.Fatalf("reader calls = %+v", calls)
	}

	// An encoded slash stays inside one path segment and fails the ID check, not the router.
	rec = fx.do("GET", "/api/streams/a%2Fb/events", "")
	wantError(t, rec, 400, contract.CodeInvalidStreamID, `invalid stream id "a/b": must match ^[a-z][A-Za-z0-9_.:-]{0,127}$`)
}

// UT-054
func TestEventsAfterValidation(t *testing.T) {
	fx := newFixture(t, admission.Running)
	wantError(t, fx.do("GET", "/api/streams/daemon/events?after=-1", ""), 400, contract.CodeInvalidCursor, "after -1 must be >= 0")
	wantError(t, fx.do("GET", "/api/streams/daemon/events?after=abc", ""), 400, contract.CodeInvalidCursor, "after abc must be >= 0")
	wantError(t, fx.do("GET", "/api/streams/daemon/events?after=1.5", ""), 400, contract.CodeInvalidCursor, "after 1.5 must be >= 0")
	if n := len(fx.events.readCalls()); n != 0 {
		t.Fatalf("the reader was called %d times for invalid input", n)
	}
}

// UT-055
func TestEventsLimitValidation(t *testing.T) {
	fx := newFixture(t, admission.Running)
	wantError(t, fx.do("GET", "/api/streams/daemon/events?limit=0", ""), 400, contract.CodeInvalidLimit, "limit 0 must be between 1 and 1000")
	wantError(t, fx.do("GET", "/api/streams/daemon/events?limit=1001", ""), 400, contract.CodeInvalidLimit, "limit 1001 must be between 1 and 1000")
	wantError(t, fx.do("GET", "/api/streams/daemon/events?limit=abc", ""), 400, contract.CodeInvalidLimit, "limit abc must be between 1 and 1000")
	wantError(t, fx.do("GET", "/api/streams/daemon/events?limit=-3", ""), 400, contract.CodeInvalidLimit, "limit -3 must be between 1 and 1000")
	if n := len(fx.events.readCalls()); n != 0 {
		t.Fatalf("the reader was called %d times for invalid input", n)
	}
	for _, limit := range []string{"1", "1000"} {
		if rec := fx.do("GET", "/api/streams/daemon/events?limit="+limit, ""); rec.Code != 200 {
			t.Fatalf("limit=%s = %d, want 200; body %s", limit, rec.Code, rec.Body)
		}
	}
}

// UT-057
func TestShutdownBodyStrictness(t *testing.T) {
	fx := newFixture(t, admission.Running)
	wantError(t, fx.do("POST", "/api/daemon/shutdown", `{"why":"x"}`), 400, contract.CodeInvalidRequest, `unknown field "why"`)
	wantError(t, fx.do("POST", "/api/daemon/shutdown", `{"reason":"signal"}`), 400, contract.CodeInvalidRequest, `reason must be "request"`)
	if n := fx.stops.Load(); n != 0 {
		t.Fatalf("a rejected request stopped the daemon %d times", n)
	}
}

// The rest of the body rules: single value, well-formed, typed, bounded.
func TestShutdownBodyEdgeCases(t *testing.T) {
	tests := []struct {
		name    string
		body    string
		wantMsg string // empty means accepted
	}{
		{"no body", ``, ""},
		{"whitespace only", " \n", ""},
		{"empty object", `{}`, ""},
		{"null", `null`, ""},
		{"the one valid reason", `{"reason":"request"}`, ""},
		{"empty reason is a bad reason", `{"reason":""}`, `reason must be "request"`},
		{"second value after the first", `{"reason":"request"}{}`, "invalid request body: only one JSON object is allowed"},
		{"trailing garbage", `{"reason":"request"} x`, "invalid request body: only one JSON object is allowed"},
		{"truncated", `{"reason":`, "invalid request body: unexpected EOF"},
		{"not an object", `[]`, "invalid request body: a JSON object is expected"},
		{"wrong type", `{"reason":5}`, `invalid request body: field "reason" has the wrong type`},
		{"too large", `{"reason":"` + strings.Repeat("a", 5000) + `"}`, "invalid request body: body is larger than 4096 bytes"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fx := newFixture(t, admission.Running)
			rec := fx.do("POST", "/api/daemon/shutdown", tt.body)
			if tt.wantMsg == "" {
				if rec.Code != 202 {
					t.Fatalf("status = %d, want 202; body %s", rec.Code, rec.Body)
				}
				return
			}
			wantError(t, rec, 400, contract.CodeInvalidRequest, tt.wantMsg)
		})
	}
}

// UT-114
func TestUnknownQueryParametersAreRejected(t *testing.T) {
	fx := newFixture(t, admission.Running)
	wantError(t, fx.do("GET", "/api/streams/daemon/events/stream?limit=5", ""), 400, contract.CodeInvalidRequest, `unknown parameter "limit"`)
	wantError(t, fx.do("GET", "/api/status?x=1", ""), 400, contract.CodeInvalidRequest, `unknown parameter "x"`)
	wantError(t, fx.do("GET", "/api/doctor?verbose=1", ""), 400, contract.CodeInvalidRequest, `unknown parameter "verbose"`)
	wantError(t, fx.do("GET", "/api/streams/daemon/events?before=3", ""), 400, contract.CodeInvalidRequest, `unknown parameter "before"`)
	wantError(t, fx.do("POST", "/api/daemon/shutdown?force=1", ""), 400, contract.CodeInvalidRequest, `unknown parameter "force"`)
	if len(fx.follow.callList()) != 0 || len(fx.events.readCalls()) != 0 {
		t.Fatal("a rejected request reached a handler")
	}
}

func TestQueryStringEdgeCases(t *testing.T) {
	fx := newFixture(t, admission.Running)
	// The first unknown name in sorted order is the one reported.
	wantError(t, fx.do("GET", "/api/status?zeta=1&alpha=2", ""), 400, contract.CodeInvalidRequest, `unknown parameter "alpha"`)
	// A known parameter given twice is not silently resolved to one of its values.
	wantError(t, fx.do("GET", "/api/streams/daemon/events?after=1&after=2", ""), 400, contract.CodeInvalidRequest, `parameter "after" given more than once`)
	wantError(t, fx.do("GET", "/api/streams/daemon/events?limit=1&limit=2", ""), 400, contract.CodeInvalidRequest, `parameter "limit" given more than once`)
	// A query string that cannot be parsed is rejected, not dropped.
	wantError(t, fx.do("GET", "/api/streams/daemon/events?after=1;limit=2", ""), 400, contract.CodeInvalidRequest, "invalid query string: invalid semicolon separator in query")
	wantError(t, fx.do("GET", "/api/streams/daemon/events?after=%zz", ""), 400, contract.CodeInvalidRequest, `invalid query string: invalid URL escape "%zz"`)
}

// UT-115
func TestJSONNotFoundAndMethodNotAllowed(t *testing.T) {
	fx := newFixture(t, admission.Running)

	rec := fx.do("POST", "/api/status", "")
	wantError(t, rec, 405, contract.CodeMethodNotAllowed, "method POST not allowed on /api/status")
	if allow := rec.Header().Get("Allow"); allow != "GET" {
		t.Fatalf("Allow = %q, want GET", allow)
	}

	wantError(t, fx.do("GET", "/api/nope", ""), 404, contract.CodeNotFound, "no route for GET /api/nope")
	wantError(t, fx.do("GET", "/", ""), 404, contract.CodeNotFound, "no route for GET /")

	rec = fx.do("GET", "/api/daemon/shutdown", "")
	wantError(t, rec, 405, contract.CodeMethodNotAllowed, "method GET not allowed on /api/daemon/shutdown")
	if allow := rec.Header().Get("Allow"); allow != "POST" {
		t.Fatalf("Allow = %q, want POST", allow)
	}
	wantError(t, fx.do("DELETE", "/api/streams/daemon/events", ""), 405, contract.CodeMethodNotAllowed, "method DELETE not allowed on /api/streams/daemon/events")
	wantError(t, fx.do("POST", "/api/streams/daemon/events/stream", ""), 405, contract.CodeMethodNotAllowed, "")
	wantError(t, fx.do("POST", "/api/doctor", ""), 405, contract.CodeMethodNotAllowed, "")
	if n := fx.stops.Load(); n != 0 {
		t.Fatal("a wrong-method request stopped the daemon")
	}
}

// UT-115 (path-clean redirect): ServeMux would answer these with a redirect and a
// plain-text body. Every one of them must come back as the JSON error instead.
func TestNoServeMuxRedirectOrPlainTextEverEscapes(t *testing.T) {
	paths := []string{
		"/api//status",
		"//api/status",
		"/api/./status",
		"/api/../api/status",
		"/api/status/",
		"/api/status//",
		"/api/streams//events",
		"/api/streams/daemon/events/",
		"/api/streams/daemon/events/stream/",
		"/api/streams/daemon//events",
		"/api",
		"/api/",
		"/api/streams/daemon",
		"/api/streams/daemon/events/extra",
		"/api/%2e%2e/status",
		"/%2e",
		"/api/status%2F",
	}
	for _, state := range []admission.State{admission.Starting, admission.Running, admission.Draining} {
		fx := newFixture(t, state)
		for _, p := range paths {
			for _, method := range []string{"GET", "POST", "OPTIONS"} {
				req := httptest.NewRequest(method, "http://harness"+p, nil)
				rec := httptest.NewRecorder()
				fx.srv.ServeHTTP(rec, req)
				if loc := rec.Header().Get("Location"); loc != "" {
					t.Errorf("[%s] %s %s redirected to %q (status %d)", state, method, p, loc, rec.Code)
				}
				var body contract.ErrorBody
				if rec.Header().Get("Content-Type") != "application/json" || json.Unmarshal(rec.Body.Bytes(), &body) != nil || body.Error == nil {
					t.Errorf("[%s] %s %s: status %d, content-type %q, body %q: not the JSON error shape",
						state, method, p, rec.Code, rec.Header().Get("Content-Type"), rec.Body)
					continue
				}
				if rec.Code != 404 {
					t.Errorf("[%s] %s %s = %d (%s), want 404 not_found", state, method, p, rec.Code, body.Error.Code)
				}
			}
		}
	}
}

// ServeMux answers `OPTIONS *` with an empty 400 and a CONNECT target (no path) with a plain
// 404. Neither is a rooted path, so the canonical check answers both with the JSON error.
func TestRequestTargetsThatAreNotPaths(t *testing.T) {
	fx := newFixture(t, admission.Running)
	for _, tt := range []struct{ method, target string }{
		{"OPTIONS", "*"},
		{"CONNECT", "example.com:443"},
	} {
		req := httptest.NewRequest(tt.method, tt.target, nil)
		rec := httptest.NewRecorder()
		fx.srv.ServeHTTP(rec, req)
		wantError(t, rec, 404, contract.CodeNotFound, "")
	}
}

// The same paths over a real connection, as the CLI and curl send them.
func TestUncleanPathsOverARealConnection(t *testing.T) {
	fx := newFixture(t, admission.Running)
	srv := httptest.NewServer(fx.srv)
	defer srv.Close()

	resp := rawRequest(t, srv.Listener.Addr().String(), "GET /api//status HTTP/1.1\r\nHost: harness\r\nConnection: close\r\n\r\n")
	if !strings.HasPrefix(resp, "HTTP/1.1 404 Not Found\r\n") || !strings.Contains(resp, `"code":"not_found"`) || strings.Contains(resp, "Location:") {
		t.Errorf("GET /api//status answered:\n%s", resp)
	}
}

// A 404 does not depend on the gate: a path with no route is not something the daemon is
// too busy to answer.
func TestNotFoundIsTheSameInEveryGateState(t *testing.T) {
	for _, state := range []admission.State{admission.Starting, admission.Running, admission.Draining} {
		fx := newFixture(t, state)
		wantError(t, fx.do("GET", "/api/nope", ""), 404, contract.CodeNotFound, "no route for GET /api/nope")
	}
}

func TestDoctorRoute(t *testing.T) {
	fx := newFixture(t, admission.Running)
	rec := fx.do("GET", "/api/doctor", "")
	want := `{"doctor":{"status":"ok","source":"daemon","items":[{"id":"home.permissions","severity":"ok","message":"home is private"}],"summary":{"ok":1,"warn":0,"error":0}}}`
	if got := strings.TrimSpace(rec.Body.String()); rec.Code != 200 || got != want {
		t.Fatalf("got %d %s\nwant 200 %s", rec.Code, got, want)
	}

	wantError(t, newFixture(t, admission.Draining).do("GET", "/api/doctor", ""), 503, contract.CodeDaemonDraining, "")
}

func TestDoctorFailuresAreInternal(t *testing.T) {
	t.Run("runner error", func(t *testing.T) {
		fx := newFixture(t, admission.Running)
		fx.doctor.err = errors.New("probe registry exploded")
		wantError(t, fx.do("GET", "/api/doctor", ""), 500, contract.CodeInternal, "internal error")
	})
	t.Run("no runner wired", func(t *testing.T) {
		fx := newFixture(t, admission.Running, func(o *Options) { o.Doctor = nil })
		wantError(t, fx.do("GET", "/api/doctor", ""), 500, contract.CodeInternal, "internal error")
		if recs := fx.logs.find(t, "request failed"); len(recs) != 1 {
			t.Fatalf("the missing runner must be logged, got %v", fx.logs.records(t))
		}
	})
}

func TestDoctorReportAlwaysHasItemsArray(t *testing.T) {
	fx := newFixture(t, admission.Running)
	fx.doctor.report = contract.Doctor{Status: contract.SeverityOK, Source: contract.SourceDaemon}
	rec := fx.do("GET", "/api/doctor", "")
	if !strings.Contains(rec.Body.String(), `"items":[]`) {
		t.Fatalf("body = %s, want an empty items array, not null", rec.Body)
	}
}

// UT-116: the shutdown route bypasses the gate.
func TestShutdownBypassesTheGate(t *testing.T) {
	const want = `{"daemon":{"stopping":true,"pid":48211,"uptime_seconds":72,"shutdown_grace":"30s"}}`

	t.Run("starting cancels the boot once", func(t *testing.T) {
		fx := newFixture(t, admission.Starting)
		rec := fx.do("POST", "/api/daemon/shutdown", "")
		if got := strings.TrimSpace(rec.Body.String()); rec.Code != 202 || got != want {
			t.Fatalf("got %d %s\nwant 202 %s", rec.Code, got, want)
		}
		if n := fx.stops.Load(); n != 1 {
			t.Fatalf("the stop callback ran %d times, want 1", n)
		}
	})

	t.Run("draining answers the same and starts nothing", func(t *testing.T) {
		fx := newFixture(t, admission.Draining)
		rec := fx.do("POST", "/api/daemon/shutdown", `{"reason":"request"}`)
		if got := strings.TrimSpace(rec.Body.String()); rec.Code != 202 || got != want {
			t.Fatalf("got %d %s\nwant 202 %s", rec.Code, got, want)
		}
		if n := fx.stops.Load(); n != 0 {
			t.Fatalf("the stop callback ran %d times while draining, want 0", n)
		}
	})

	t.Run("running starts the shutdown once", func(t *testing.T) {
		fx := newFixture(t, admission.Running)
		if rec := fx.do("POST", "/api/daemon/shutdown", `{"reason":"request"}`); rec.Code != 202 {
			t.Fatalf("status = %d; body %s", rec.Code, rec.Body)
		}
		if n := fx.stops.Load(); n != 1 {
			t.Fatalf("the stop callback ran %d times, want 1", n)
		}
	})
}

// Repeated requests before the daemon reports draining must not trigger the stop twice.
func TestShutdownTriggersOnceWhateverTheRequestCount(t *testing.T) {
	fx := newFixture(t, admission.Running)
	for range 5 {
		if rec := fx.do("POST", "/api/daemon/shutdown", ""); rec.Code != 202 {
			t.Fatalf("status = %d", rec.Code)
		}
	}
	if n := fx.stops.Load(); n != 1 {
		t.Fatalf("the stop callback ran %d times, want 1", n)
	}
}

// The shutdown answer must not depend on the ledger or the status source: it has to work
// in the middle of a boot or a drain.
func TestShutdownAnswersEvenWhenTheStatusSourceFails(t *testing.T) {
	fx := newFixture(t, admission.Draining)
	fx.status.err = errors.New("ledger is closed")
	if rec := fx.do("POST", "/api/daemon/shutdown", ""); rec.Code != 202 {
		t.Fatalf("status = %d, want 202; body %s", rec.Code, rec.Body)
	}
}

// Errors are redacted on the way out: a secret in a message never reaches a client.
func TestWriteErrorRedactsMessageAndHint(t *testing.T) {
	rec := httptest.NewRecorder()
	writeError(rec, 500, contract.CodeInternal, "dial https://user:s3cret@host failed", "retry with token=abc123", map[string]int{"n": 1})
	var body contract.ErrorBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(rec.Body.String(), "s3cret") || strings.Contains(rec.Body.String(), "abc123") {
		t.Fatalf("secret in body: %s", rec.Body)
	}
	if body.Error.Message != "dial https://[REDACTED]@host failed" || body.Error.Hint != "retry with token=[REDACTED]" {
		t.Fatalf("got message %q hint %q", body.Error.Message, body.Error.Hint)
	}
	if rec.Code != 500 || rec.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("got %d %q", rec.Code, rec.Header().Get("Content-Type"))
	}
}

func TestWriteErrorKeepsDetailsAndOmitsEmptyHint(t *testing.T) {
	rec := httptest.NewRecorder()
	writeError(rec, 500, contract.CodeDaemonStartFailed, "daemon exited", "", contract.StartFailedDetails{PID: 7, ExitCode: 1, OutputTail: []string{"boom"}})
	want := `{"error":{"code":"daemon_start_failed","message":"daemon exited","details":{"pid":7,"exit_code":1,"output_tail":["boom"]}}}`
	if got := strings.TrimSpace(rec.Body.String()); got != want {
		t.Fatalf("body = %s\nwant   %s", got, want)
	}
}

// An error that is not a contract error is internal; its text never reaches the client.
func TestWriteContractErrorHidesForeignErrors(t *testing.T) {
	rec := httptest.NewRecorder()
	writeContractError(rec, errors.New("open /secret/path: permission denied"))
	wantError(t, rec, http.StatusInternalServerError, contract.CodeInternal, "internal error")
	if strings.Contains(rec.Body.String(), "secret") {
		t.Fatalf("foreign error text leaked: %s", rec.Body)
	}
}

func TestStatusForEveryCode(t *testing.T) {
	want := map[string]int{
		contract.CodeInvalidStreamID: 400, contract.CodeInvalidCursor: 400, contract.CodeInvalidLimit: 400, contract.CodeInvalidRequest: 400,
		contract.CodeNotFound: 404, contract.CodeMethodNotAllowed: 405,
		contract.CodeDaemonNotReady: 503, contract.CodeDaemonDraining: 503,
		contract.CodeInternal: 500,
	}
	for code, status := range want {
		if got := statusFor(code); got != status {
			t.Errorf("statusFor(%s) = %d, want %d", code, got, status)
		}
	}
}

func TestNewRejectsMissingDependencies(t *testing.T) {
	good := newFixture(t, admission.Running)
	base := Options{
		Gate: good.gate, Status: good.status, Events: good.events,
		Follow: good.follow.follow, RequestStop: func() {},
	}
	if _, err := New(base); err != nil {
		t.Fatalf("a complete Options (no Doctor, no Logger) must be accepted: %v", err)
	}
	for name, mod := range map[string]func(*Options){
		"gate":          func(o *Options) { o.Gate = nil },
		"status":        func(o *Options) { o.Status = nil },
		"events":        func(o *Options) { o.Events = nil },
		"follow":        func(o *Options) { o.Follow = nil },
		"request stop":  func(o *Options) { o.RequestStop = nil },
		"keepalive":     func(o *Options) { o.KeepaliveInterval = -1 },
		"write timeout": func(o *Options) { o.WriteTimeout = -1 },
	} {
		opts := base
		mod(&opts)
		if _, err := New(opts); err == nil {
			t.Errorf("New accepted Options with a bad %s", name)
		}
	}
}
