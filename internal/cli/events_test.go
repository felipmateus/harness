package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/felipmateus/harness/internal/api/contract"
	"github.com/felipmateus/harness/internal/testutil/leakcheck"
)

func samplePage() contract.EventsPage {
	return contract.EventsPage{
		Events: []contract.Event{
			{Stream: "daemon", Seq: 1, ID: "01JBJ7QK2F0M3A7H1B9C4D6E8G", Type: "config.loaded", Time: "2026-10-28T22:10:03.112Z",
				Data: json.RawMessage(`{"pid":48211,"path":"/Users/felipe/.harness/config.toml","exists":false,"digest":null}`)},
			{Stream: "daemon", Seq: 2, ID: "01JBJ7QK3W5Z8V2N4T6R9YXC1D", Type: "daemon.started", Time: "2026-10-28T22:10:03.140Z",
				Data: json.RawMessage(`{"pid":48211,"version":"0.1.0-dev","boot_ms":41}`)},
		},
		Page: contract.Page{After: 0, NextAfter: 2, HasMore: false, Limit: 100, Head: 2},
	}
}

func listing(e *env) {
	e.api.eventsFn = func(string, int64, int) (contract.EventsPage, error) { return samplePage(), nil }
}

// UT-127: jsonl prints one line per event, then a page line.
func TestUT127_JSONLEndsWithThePageLine(t *testing.T) {
	e := newEnv(t)
	listing(e)

	if code := e.run("events", "-o", "jsonl"); code != 0 {
		t.Fatalf("exit %d, stderr %q", code, e.err())
	}
	want := `{"stream":"daemon","seq":1,"id":"01JBJ7QK2F0M3A7H1B9C4D6E8G","type":"config.loaded","time":"2026-10-28T22:10:03.112Z","data":{"pid":48211,"path":"/Users/felipe/.harness/config.toml","exists":false,"digest":null}}` + "\n" +
		`{"stream":"daemon","seq":2,"id":"01JBJ7QK3W5Z8V2N4T6R9YXC1D","type":"daemon.started","time":"2026-10-28T22:10:03.140Z","data":{"pid":48211,"version":"0.1.0-dev","boot_ms":41}}` + "\n" +
		`{"page":{"after":0,"next_after":2,"has_more":false,"limit":100,"head":2}}` + "\n"
	if e.out() != want || e.err() != "" {
		t.Errorf("stdout:\n%s\nwant:\n%s\nstderr %q", e.out(), want, e.err())
	}
}

func TestUT127_JSONPage(t *testing.T) {
	e := newEnv(t)
	listing(e)
	if code := e.run("events", "--after", "1", "--limit", "1", "-o", "json"); code != 0 {
		t.Fatalf("exit %d, stderr %q", code, e.err())
	}
	var got contract.EventsPage
	if err := json.Unmarshal([]byte(e.out()), &got); err != nil || len(got.Events) != 2 || got.Page.Head != 2 {
		t.Errorf("stdout %q (%v)", e.out(), err)
	}
	if !strings.HasPrefix(e.out(), `{"events":[{"stream":"daemon","seq":1,`) || !strings.HasSuffix(e.out(), `"page":{"after":0,"next_after":2,"has_more":false,"limit":100,"head":2}}`+"\n") {
		t.Errorf("shape %q", e.out())
	}
}

// An empty page is an empty array and a page line, never null.
func TestEmptyPageIsAnEmptyArray(t *testing.T) {
	e := newEnv(t)
	e.api.eventsFn = func(string, int64, int) (contract.EventsPage, error) {
		return contract.EventsPage{Page: contract.Page{Limit: 100}}, nil
	}
	e.run("events", "-o", "json")
	if e.out() != `{"events":[],"page":{"after":0,"next_after":0,"has_more":false,"limit":100,"head":0}}`+"\n" {
		t.Errorf("stdout %q", e.out())
	}
	e.run("events", "-o", "jsonl")
	if e.out() != `{"page":{"after":0,"next_after":0,"has_more":false,"limit":100,"head":0}}`+"\n" {
		t.Errorf("jsonl stdout %q", e.out())
	}
}

// The listing passes the flags to the daemon, with the documented defaults.
func TestEventsPassesItsFlags(t *testing.T) {
	e := newEnv(t)
	var gotStream string
	var gotAfter int64
	var gotLimit int
	e.api.eventsFn = func(stream string, after int64, limit int) (contract.EventsPage, error) {
		gotStream, gotAfter, gotLimit = stream, after, limit
		return contract.EventsPage{}, nil
	}
	e.run("events")
	if gotStream != "daemon" || gotAfter != 0 || gotLimit != 100 {
		t.Errorf("defaults: stream %q after %d limit %d", gotStream, gotAfter, gotLimit)
	}
	e.run("events", "--stream", "session:01JB", "--after", "7", "--limit", "5")
	if gotStream != "session:01JB" || gotAfter != 7 || gotLimit != 5 {
		t.Errorf("stream %q after %d limit %d", gotStream, gotAfter, gotLimit)
	}
}

// UT-127: --follow -o json is a usage error.
func TestUT127_FollowWithJSONIsUsage(t *testing.T) {
	e := newEnv(t)
	if code := e.run("events", "--follow", "-o", "json"); code != 64 {
		t.Fatalf("exit %d, want 64", code)
	}
	if got := e.wireError(); got.Code != contract.CodeUsage || got.Message != "--follow cannot be used with -o json; use -o jsonl" {
		t.Errorf("error %+v", got)
	}
	if e.out() != "" || e.api.calls() != 0 {
		t.Errorf("stdout %q, daemon calls %d", e.out(), e.api.calls())
	}
}

// UT-128: invalid input exits 65 with the API's messages, before any network call.
func TestUT128_ClientSideValidation(t *testing.T) {
	cases := []struct {
		args []string
		err  error
	}{
		{[]string{"events", "--stream", "Daemon Log"}, contract.ValidateStream("Daemon Log")},
		{[]string{"events", "--limit", "5000"}, contract.ValidateLimit(5000)},
		{[]string{"events", "--limit", "0"}, contract.ValidateLimit(0)},
		{[]string{"events", "--after", "-1"}, contract.ValidateAfter(-1)},
		{[]string{"events", "--after", "abc"}, func() error { _, err := contract.ParseAfter("abc"); return err }()},
		{[]string{"events", "--follow", "--stream", "Daemon Log"}, contract.ValidateStream("Daemon Log")},
	}
	for _, tc := range cases {
		e := newEnv(t)
		if code := e.run(tc.args...); code != 65 {
			t.Errorf("%v: exit %d, want 65", tc.args, code)
		}
		want := "error: " + tc.err.Error() + "\n"
		if e.err() != want {
			t.Errorf("%v: stderr %q, want %q", tc.args, e.err(), want)
		}
		if e.out() != "" || e.api.calls() != 0 {
			t.Errorf("%v: stdout %q, daemon calls %d", tc.args, e.out(), e.api.calls())
		}
	}
	// The messages are the _dx.md ones.
	if got := contract.ValidateStream("Daemon Log").Error(); got != `invalid stream id "Daemon Log": must match ^[a-z][A-Za-z0-9_.:-]{0,127}$` {
		t.Errorf("stream message %q", got)
	}

	e := newEnv(t)
	e.run("events", "--limit", "5000", "-o", "json")
	if got := e.wireError(); got.Code != contract.CodeInvalidLimit || got.Message != "limit 5000 must be between 1 and 1000" {
		t.Errorf("json error %+v", got)
	}
}

// pipeBody is the body of a fake event stream. Reads end with the context's error once the
// request's context is cancelled, as a real response body does.
type pipeBody struct {
	r *io.PipeReader
	w *io.PipeWriter
}

func newPipeBody(ctx context.Context) *pipeBody {
	r, w := io.Pipe()
	go func() {
		<-ctx.Done()
		w.CloseWithError(ctx.Err())
	}()
	return &pipeBody{r: r, w: w}
}

func (b *pipeBody) Read(p []byte) (int, error) { return b.r.Read(p) }
func (b *pipeBody) Close() error               { return b.r.Close() }

func (b *pipeBody) send(s string) {
	if _, err := b.w.Write([]byte(s)); err != nil {
		panic(err)
	}
}

// streaming returns an API whose stream is scripted by a goroutine; the body is handed back
// on the channel.
func streaming(e *env) <-chan *pipeBody {
	bodies := make(chan *pipeBody, 1)
	e.api.streamFn = func(ctx context.Context, _ string, _ int64) (io.ReadCloser, error) {
		b := newPipeBody(ctx)
		bodies <- b
		return b, nil
	}
	return bodies
}

func eventFrame(seq int, typ, data string) string {
	return fmt.Sprintf("id: %d\nevent: %s\ndata: {\"stream\":\"daemon\",\"seq\":%d,\"id\":\"X%d\",\"type\":%q,\"time\":\"2026-10-28T22:10:03.140Z\",\"data\":%s}\n\n", seq, typ, seq, seq, typ, data)
}

const shutdownFrame = "event: shutdown\ndata: {\"reason\":\"daemon stopped\"}\n\n"

// staticBody serves a fixed stream and then EOF.
func staticStream(e *env, body string) {
	e.api.streamFn = func(context.Context, string, int64) (io.ReadCloser, error) {
		return io.NopCloser(strings.NewReader(body)), nil
	}
}

// UT-049: a shutdown frame ends the follow with the notice and exit 0.
func TestUT049_FollowEndsOnTheShutdownFrame(t *testing.T) {
	leakcheck.Check(t)
	e := newEnv(t)
	staticStream(e, ": keepalive\n\n"+eventFrame(3, "daemon.stopping", `{"pid":7,"reason":"request"}`)+eventFrame(4, "daemon.stopped", `{"pid":7,"uptime_seconds":72}`)+shutdownFrame)

	if code := e.run("events", "--follow"); code != 0 {
		t.Fatalf("exit %d, stderr %q", code, e.err())
	}
	want := eventsHeader() + "\n" +
		"3      2026-10-28T22:10:03.140Z  daemon.stopping                   pid=7 reason=request\n" +
		"4      2026-10-28T22:10:03.140Z  daemon.stopped                    pid=7 uptime_seconds=72\n"
	if e.out() != want {
		t.Errorf("stdout:\n%s\nwant:\n%s", e.out(), want)
	}
	if e.err() != "stream closed: daemon stopped\n" {
		t.Errorf("stderr %q", e.err())
	}
}

func TestUT049_FollowJSONLEndsWithTheClosedLine(t *testing.T) {
	leakcheck.Check(t)
	e := newEnv(t)
	staticStream(e, eventFrame(3, "daemon.stopping", `{"pid":7,"reason":"request"}`)+shutdownFrame)

	if code := e.run("events", "--follow", "-o", "jsonl"); code != 0 {
		t.Fatalf("exit %d, stderr %q", code, e.err())
	}
	lines := strings.Split(strings.TrimSuffix(e.out(), "\n"), "\n")
	if len(lines) != 2 || lines[1] != `{"closed":{"reason":"daemon stopped"}}` {
		t.Errorf("stdout %q", e.out())
	}
	if !strings.HasPrefix(lines[0], `{"stream":"daemon","seq":3,`) {
		t.Errorf("first line %q", lines[0])
	}
	if e.err() != "" {
		t.Errorf("stderr %q, want empty", e.err())
	}
}

// UT-049: a stream that ends without its final frame is a lost daemon.
func TestUT049_FollowEOFWithoutAFinalFrameIsUnavailable(t *testing.T) {
	leakcheck.Check(t)
	e := newEnv(t)
	staticStream(e, eventFrame(1, "config.loaded", `{"pid":7}`))

	if code := e.run("events", "--follow", "-o", "jsonl"); code != 69 {
		t.Fatalf("exit %d, want 69", code)
	}
	if got := e.wireError(); got.Code != contract.CodeDaemonUnavailable || got.Message != "no daemon is listening on "+e.home.SocketPath() {
		t.Errorf("error %+v", got)
	}
}

// An unfinished frame is not an event.
func TestFollowDropsAnUnfinishedFrame(t *testing.T) {
	leakcheck.Check(t)
	e := newEnv(t)
	staticStream(e, "id: 1\nevent: config.loaded\ndata: {\"seq\":1")
	if code := e.run("events", "--follow", "-o", "jsonl"); code != 69 {
		t.Fatalf("exit %d, want 69", code)
	}
	if e.out() != "" {
		t.Errorf("stdout %q, want empty", e.out())
	}
}

// The follow's daemon refusals come out as the daemon said them.
func TestFollowReportsTheDaemonsRefusal(t *testing.T) {
	e := newEnv(t)
	e.api.streamFn = func(context.Context, string, int64) (io.ReadCloser, error) {
		return nil, &contract.Error{Code: contract.CodeDaemonNotReady, Message: "daemon is starting"}
	}
	if code := e.run("events", "--follow"); code != 69 {
		t.Fatalf("exit %d, want 69", code)
	}
	if e.out() != "" || e.err() != "error: daemon is starting\n" {
		t.Errorf("stdout %q stderr %q (no table header before the stream is open)", e.out(), e.err())
	}
}

// UT-130: 45 s without a byte is a lost daemon; keepalives restart the count.
func TestUT130_FollowIdleDeadline(t *testing.T) {
	leakcheck.Check(t)
	e := newEnv(t)
	e.clock = newFakeClock(false)
	e.app.clock = e.clock
	bodies := streaming(e)
	start := e.clock.Now()

	done := make(chan int, 1)
	go func() { done <- e.run("events", "--follow", "-o", "jsonl") }()
	body := <-bodies
	e.clock.waitForTimerAt(t, start.Add(followIdle))

	// 30 s of silence, then a keepalive: the deadline moves to 45 s after the keepalive.
	e.clock.Advance(30 * time.Second)
	body.send(": keepalive\n\n")
	e.clock.waitForTimerAt(t, start.Add(30*time.Second+followIdle))

	// 44 s more, still alive.
	e.clock.Advance(44 * time.Second)
	select {
	case code := <-done:
		t.Fatalf("gave up after 44 s of silence (exit %d)", code)
	case <-time.After(50 * time.Millisecond):
	}

	e.clock.Advance(time.Second)
	select {
	case code := <-done:
		if code != 69 {
			t.Fatalf("exit %d, want 69", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the follow did not end at the idle deadline")
	}
	if got := e.wireError(); got.Code != contract.CodeDaemonUnavailable {
		t.Errorf("error %+v", got)
	}
}

// UT-130: a 2 MiB data line is parsed.
func TestUT130_FollowParsesA2MiBLine(t *testing.T) {
	leakcheck.Check(t)
	e := newEnv(t)
	big := strings.Repeat("x", 2<<20)
	staticStream(e, eventFrame(1, "big.event", `{"blob":"`+big+`"}`)+shutdownFrame)

	if code := e.run("events", "--follow", "-o", "jsonl"); code != 0 {
		t.Fatalf("exit %d, stderr %.200q", code, e.err())
	}
	lines := strings.Split(strings.TrimSuffix(e.out(), "\n"), "\n")
	if len(lines) != 2 || len(lines[0]) < 2<<20 {
		t.Fatalf("got %d lines, first of %d bytes", len(lines), len(lines[0]))
	}
	var ev contract.Event
	if err := json.Unmarshal([]byte(lines[0]), &ev); err != nil || ev.Seq != 1 {
		t.Errorf("event %v (%v)", ev.Seq, err)
	}
}

// A line past the limit is an error, not unbounded memory.
func TestFollowRefusesALineOver16MiB(t *testing.T) {
	leakcheck.Check(t)
	e := newEnv(t)
	staticStream(e, "data: "+strings.Repeat("x", maxSSELine+1)+"\n\n")
	if code := e.run("events", "--follow", "-o", "jsonl"); code != 1 {
		t.Fatalf("exit %d, want 1; stderr %.200q", code, e.err())
	}
	if got := e.wireError(); got.Code != contract.CodeInternal || !strings.Contains(got.Message, "16 MiB") {
		t.Errorf("error %+v", got)
	}
}

// UT-130: an error frame is `error: internal error` and exit 1, not "unavailable".
func TestUT130_FollowErrorFrame(t *testing.T) {
	leakcheck.Check(t)
	body := eventFrame(1, "config.loaded", `{"pid":7}`) + "event: error\ndata: {\"error\":{\"code\":\"internal\",\"message\":\"internal error\"}}\n\n"

	e := newEnv(t)
	staticStream(e, body)
	if code := e.run("events", "--follow"); code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
	if e.err() != "error: internal error\n" {
		t.Errorf("stderr %q", e.err())
	}

	e = newEnv(t)
	staticStream(e, body)
	if code := e.run("events", "--follow", "-o", "jsonl"); code != 1 {
		t.Fatalf("jsonl: exit %d, want 1", code)
	}
	if got := e.wireError(); got.Code != contract.CodeInternal || got.Message != "internal error" {
		t.Errorf("error %+v", got)
	}
}

// An event whose type is `error` or `shutdown` is an event: only frames without an id end
// a stream.
func TestFollowDistinguishesEventsNamedLikeTheEndingFrames(t *testing.T) {
	leakcheck.Check(t)
	e := newEnv(t)
	staticStream(e, eventFrame(1, "shutdown", `{"a":1}`)+eventFrame(2, "error", `{"b":2}`)+shutdownFrame)
	if code := e.run("events", "--follow", "-o", "jsonl"); code != 0 {
		t.Fatalf("exit %d, stderr %q", code, e.err())
	}
	if n := strings.Count(e.out(), "\n"); n != 3 {
		t.Errorf("got %d lines, want 2 events and the closing line:\n%s", n, e.out())
	}
}

// Ctrl-C ends the follow with 130 and no message.
func TestFollowInterruptedIs130(t *testing.T) {
	leakcheck.Check(t)
	e := newEnv(t)
	bodies := streaming(e)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan int, 1)
	go func() { done <- e.runCtx(ctx, "events", "--follow", "-o", "jsonl") }()
	body := <-bodies
	body.send(eventFrame(1, "config.loaded", `{"pid":7}`))
	cancel()

	select {
	case code := <-done:
		if code != 130 {
			t.Fatalf("exit %d, want 130", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the follow did not end after the interrupt")
	}
	if e.err() != "" {
		t.Errorf("stderr %q, want empty", e.err())
	}
}

// A follow asks for the history after --after and never for a page size.
func TestFollowPassesAfter(t *testing.T) {
	e := newEnv(t)
	var gotAfter int64 = -1
	e.api.streamFn = func(_ context.Context, stream string, after int64) (io.ReadCloser, error) {
		gotAfter = after
		return io.NopCloser(strings.NewReader(shutdownFrame)), nil
	}
	if code := e.run("events", "--follow", "--after", "5", "--limit", "10"); code != 0 {
		t.Fatalf("exit %d, stderr %q", code, e.err())
	}
	if gotAfter != 5 {
		t.Errorf("after %d, want 5", gotAfter)
	}
	if e.api.eventsCalls.Load() != 0 {
		t.Error("a follow must not list a page")
	}
}

// UT-131: the human rendering.
func TestUT131_HumanRendering(t *testing.T) {
	if got, want := eventsHeader(), "SEQ    TIME                      TYPE                              DATA"; got != want {
		t.Errorf("header %q, want %q", got, want)
	}

	ev := func(seq int64, typ, data string) contract.Event {
		return contract.Event{Seq: seq, Type: typ, Time: "2026-10-28T22:10:03.112Z", Data: json.RawMessage(data)}
	}

	// Widths 5/24/32, two spaces between, null as null.
	got := eventRow(ev(1, "config.loaded", `{"pid":48211,"path":"/h/config.toml","exists":false,"digest":null}`))
	want := "1      2026-10-28T22:10:03.112Z  config.loaded                     pid=48211 path=/h/config.toml exists=false digest=null"
	if got != want {
		t.Errorf("row\n%q\nwant\n%q", got, want)
	}

	// A 32-character type is followed by two spaces; a longer one is printed in full, then two spaces.
	t32 := strings.Repeat("t", 32)
	if got, want := eventRow(ev(2, t32, `{"a":1}`)), "2      2026-10-28T22:10:03.112Z  "+t32+"  a=1"; got != want {
		t.Errorf("32-char type: %q, want %q", got, want)
	}
	t40 := strings.Repeat("u", 40)
	if got, want := eventRow(ev(3, t40, `{"a":1}`)), "3      2026-10-28T22:10:03.112Z  "+t40+"  a=1"; got != want {
		t.Errorf("40-char type: %q, want %q", got, want)
	}

	// A wide sequence is not cut.
	if got := eventRow(ev(1234567, "x.y", `{}`)); !strings.HasPrefix(got, "1234567  2026-10-28T22:10:03.112Z  x.y") {
		t.Errorf("wide seq: %q", got)
	}

	// Nested values are compact JSON; keys keep their recorded order, not alphabetical.
	got = eventRow(ev(4, "a.b", `{"z": {"k": [1, 2], "a": null}, "b": "text", "n": 1.50, "t": true}`))
	if want := `z={"k":[1,2],"a":null} b=text n=1.50 t=true`; !strings.HasSuffix(got, "  "+want) {
		t.Errorf("data: %q, want suffix %q", got, want)
	}

	// A string with a newline stays on one line.
	got = eventRow(ev(5, "a.b", `{"m":"line1\nline2"}`))
	if strings.Contains(got, "\n") || !strings.HasSuffix(got, `m="line1\nline2"`) {
		t.Errorf("multi-line string: %q", got)
	}

	// A payload that is not an object prints as compact JSON.
	if got := formatData(json.RawMessage(`[1, 2]`)); got != "[1,2]" {
		t.Errorf("array payload: %q", got)
	}
}
