//go:build integration

package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/felipmateus/harness/internal/api/contract"
	"github.com/felipmateus/harness/internal/testutil/testhome"
)

// appendN appends n small events to a test stream from the calling goroutine.
func appendN(t *testing.T, r *daemonRun, stream string, from, n int) {
	t.Helper()
	for i := from; i < from+n; i++ {
		if err := appendTest(t.Context(), r.access.Appender, stream, i); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
	}
}

// noMoreFrames fails the test if the follower receives anything within d.
func noMoreFrames(t *testing.T, f *follower, d time.Duration) {
	t.Helper()
	if fr, ok := f.next(d); ok {
		t.Fatalf("unexpected frame %+v; each event must arrive exactly once", fr)
	}
}

// IT-030: a follower attached to the daemon stream gets the stop (daemon.stopping, then
// daemon.stopped), then the shutdown frame, then EOF.
func TestIT030_FollowerSeesTheStopAndTheShutdownFrame(t *testing.T) {
	checkLeaks(t)
	h := testhome.New(t)
	r := startDaemon(t, h, nil)
	f := openFollower(t, h.SocketPath(), "daemon", followOpts{lastEventID: "2"})

	code, body := r.do(t, http.MethodPost, "/api/daemon/shutdown", "")
	if code != http.StatusAccepted {
		t.Fatalf("POST shutdown = %d %s, want 202", code, body)
	}

	var got []string
	for {
		fr, ok := f.next(15 * time.Second)
		if !ok {
			break
		}
		got = append(got, fr.Event)
		switch fr.Event {
		case "daemon.stopping":
			if fr.ID != "3" || dataOf(t, json.RawMessage(eventData(t, fr)))["reason"] != "request" {
				t.Errorf("stopping frame = %+v, want id 3 and reason request", fr)
			}
		case "daemon.stopped":
			if fr.ID != "4" {
				t.Errorf("stopped frame = %+v, want id 4", fr)
			}
		case "shutdown":
			if fr.Data != `{"reason":"daemon stopped"}` {
				t.Errorf("shutdown frame data = %q", fr.Data)
			}
		}
	}
	if want := []string{"daemon.stopping", "daemon.stopped", "shutdown"}; len(got) != len(want) || got[0] != want[0] || got[1] != want[1] || got[2] != want[2] {
		t.Fatalf("frames = %v, want %v, then EOF", got, want)
	}
	f.waitEnded(t, 10*time.Second)
	if err, ok := r.wait(10 * time.Second); !ok || err != nil {
		t.Fatalf("Run = %v (returned=%v), want nil", err, ok)
	}
}

// eventData extracts the `data` object of an event frame.
func eventData(t *testing.T, fr frame) []byte {
	t.Helper()
	var ev contract.Event
	if err := json.Unmarshal([]byte(fr.Data), &ev); err != nil {
		t.Fatalf("frame data %q is not an event: %v", fr.Data, err)
	}
	return ev.Data
}

// IT-031: a live follower receives every event a producer appends, in order, each once.
func TestIT031_LiveFollowIsOrderedAndGapFree(t *testing.T) {
	checkLeaks(t)
	h := testhome.New(t)
	r := startDaemon(t, h, nil)
	f := openFollower(t, h.SocketPath(), "test", followOpts{})

	appendN(t, r, "test", 1, 200)

	seqs := f.seqs(t, 200, 30*time.Second)
	wantSequence(t, seqs, 1)
	noMoreFrames(t, f, 300*time.Millisecond)
}

// IT-032: a follower that disconnects at seq 50 and reconnects with Last-Event-ID: 50
// receives exactly 51..100.
func TestIT032_ResumeAfterLastEventID(t *testing.T) {
	checkLeaks(t)
	h := testhome.New(t)
	r := startDaemon(t, h, nil)

	appendN(t, r, "test", 1, 50)
	first := openFollower(t, h.SocketPath(), "test", followOpts{})
	wantSequence(t, first.seqs(t, 50, 15*time.Second), 1)
	first.close()
	first.waitEnded(t, 10*time.Second)

	appendN(t, r, "test", 51, 50) // committed while nobody was following

	second := openFollower(t, h.SocketPath(), "test", followOpts{lastEventID: "50"})
	wantSequence(t, second.seqs(t, 50, 15*time.Second), 51)
	noMoreFrames(t, second, 300*time.Millisecond)
}

// IT-033: a follower that reads slowly while events keep arriving is degraded by the broker,
// caught up from the ledger, and still receives 1..2000 in order. The log records the
// degradation.
//
// The spec reads "sleep 5 ms per frame". A 5 ms reader alone keeps up with a producer that
// commits about every 6 ms and would never be degraded, so the client also stalls until
// 300 events are in: more than the socket buffers, the SSE write, the follow channel and
// the 64-event subscription can hold. The events carry 4 KiB of filler, so the socket
// buffers fill on Linux as well as on macOS.
func TestIT033_SlowFollowerIsDegradedAndCaughtUp(t *testing.T) {
	checkLeaks(t)
	h := testhome.New(t)
	r := startDaemon(t, h, nil)
	const total, stallUntil = 2000, 300

	stall := make(chan struct{})
	f := openFollower(t, h.SocketPath(), "test", followOpts{stall: stall, pace: 5 * time.Millisecond})

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	produced := make(chan error, 1)
	go func() {
		for i := 1; i <= total; i++ {
			if err := appendPadded(ctx, r.access.Appender, "test", i, 4096); err != nil {
				produced <- err
				return
			}
			if i == stallUntil {
				close(stall)
			}
		}
		produced <- nil
	}()

	wantSequence(t, f.seqs(t, total, 120*time.Second), 1)
	if err := <-produced; err != nil {
		t.Fatalf("producer: %v", err)
	}
	if !hasLog(logRecords(t, h), "subscriber degraded") {
		t.Error("the log has no `subscriber degraded` record although the follower fell behind")
	}
	for _, rec := range logRecords(t, h) {
		if rec["msg"] == "subscriber degraded" {
			if rec["level"] != "WARN" || rec["stream"] != "test" {
				t.Errorf("degraded record = %v, want level WARN and stream test", rec)
			}
			if seq, ok := rec["seq"].(float64); !ok || seq < 65 {
				t.Errorf("degraded record seq = %v, want the sequence of the event that did not fit (at least 65)", rec["seq"])
			}
			break
		}
	}
}

// IT-034: 20 followers and 500 appends: every follower receives 1..500.
func TestIT034_ManyFollowers(t *testing.T) {
	checkLeaks(t)
	h := testhome.New(t)
	r := startDaemon(t, h, nil)

	followers := make([]*follower, 20)
	for i := range followers {
		followers[i] = openFollower(t, h.SocketPath(), "test", followOpts{})
	}
	appendN(t, r, "test", 1, 500)

	for i, f := range followers {
		t.Logf("checking follower %d", i)
		wantSequence(t, f.seqs(t, 500, 60*time.Second), 1)
		noMoreFrames(t, f, 50*time.Millisecond)
	}
}

// IT-035: 8 goroutines append 250 events each through the daemon's Appender while 2
// followers run. The head is 2000, and both followers saw 1..2000 in order.
func TestIT035_ConcurrentAppends(t *testing.T) {
	checkLeaks(t)
	h := testhome.New(t)
	r := startDaemon(t, h, nil)
	f1 := openFollower(t, h.SocketPath(), "test", followOpts{})
	f2 := openFollower(t, h.SocketPath(), "test", followOpts{})

	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 250; i++ {
				if err := appendTest(t.Context(), r.access.Appender, "test", g*250+i); err != nil {
					errs <- err
					return
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("append: %v", err)
	}

	code, body := r.do(t, http.MethodGet, "/api/streams/test/events?limit=1", "")
	var page contract.EventsPage
	if code != http.StatusOK || json.Unmarshal(body, &page) != nil || page.Page.Head != 2000 {
		t.Fatalf("GET events = %d %s, want a page with head 2000", code, body)
	}
	for i, f := range []*follower{f1, f2} {
		t.Logf("checking follower %d", i+1)
		wantSequence(t, f.seqs(t, 2000, 60*time.Second), 1)
		noMoreFrames(t, f, 50*time.Millisecond)
	}
}

// A follower that stopped reading cannot hold the shutdown: the streams get one second to
// say goodbye, and the one that is blocked on its client is cut. (IT-050 runs the same
// scenario through the CLI in a subprocess; this is its in-process form.)
func TestShutdown_AStalledFollowerCannotHoldIt(t *testing.T) {
	checkLeaks(t)
	h := testhome.New(t)
	r := startDaemon(t, h, nil)

	stall := make(chan struct{}) // never opened: the client reads the headers and nothing else
	t.Cleanup(func() { close(stall) })
	openFollower(t, h.SocketPath(), "test", followOpts{stall: stall})

	// Enough data to fill every buffer between the ledger and the client.
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	for i := 1; i <= 300; i++ {
		if err := appendPadded(ctx, r.access.Appender, "test", i, 4096); err != nil {
			t.Fatal(err)
		}
	}

	began := time.Now()
	err := r.stop(t)
	elapsed := time.Since(began)

	if err != nil {
		t.Fatalf("Run = %v, want nil (a clean stop)", err)
	}
	if elapsed > 3*time.Second {
		t.Errorf("shutdown with a stalled follower took %s, want under 3s", elapsed)
	}
	if !hasLog(logRecords(t, h), "streams cut at shutdown") {
		t.Error("the log has no `streams cut at shutdown` record")
	}
	if !lockIsFree(t, h) {
		t.Error("the lock is still held")
	}
}

// A follower that is slow, but not gone, still gets its shutdown frame: the streams are given
// time to finish before the server shutdown cancels them. The client here stops reading with
// a backlog, the daemon is told to stop, and the client resumes a moment later, well inside
// the one-second grace.
//
// A follow ends as soon as the broker closes, so the backlog is not delivered; what the
// client received is a gap-free prefix, and the frame tells it to reconnect.
func TestShutdown_ASlowFollowerStillGetsItsFinalFrame(t *testing.T) {
	checkLeaks(t)
	h := testhome.New(t)
	r := startDaemon(t, h, nil)

	stall := make(chan struct{})
	f := openFollower(t, h.SocketPath(), "test", followOpts{stall: stall})
	for i := 1; i <= 100; i++ {
		if err := appendPadded(t.Context(), r.access.Appender, "test", i, 4096); err != nil {
			t.Fatal(err)
		}
	}

	r.cancel() // the daemon starts to stop while the client is not reading
	time.Sleep(300 * time.Millisecond)
	close(stall)

	var seqs []int64
	var last frame
	for {
		fr, ok := f.next(15 * time.Second)
		if !ok {
			break
		}
		last = fr
		if fr.Event == "shutdown" {
			break
		}
		var seq int64
		if _, err := fmt.Sscan(fr.ID, &seq); err != nil {
			t.Fatalf("frame %+v has no sequence id", fr)
		}
		seqs = append(seqs, seq)
	}
	if last.Event != "shutdown" {
		t.Fatalf("the stream ended after %d events without a shutdown frame (last frame: id %q, event %q)", len(seqs), last.ID, last.Event)
	}
	if len(seqs) == 0 {
		t.Error("the follower received no events before the shutdown frame")
	}
	wantSequence(t, seqs, 1) // whatever arrived is in order and has no gap
	f.waitEnded(t, 10*time.Second)
	if err, ok := r.wait(15 * time.Second); !ok || err != nil {
		t.Fatalf("Run = %v (returned=%v), want nil", err, ok)
	}
	if hasLog(logRecords(t, h), "streams cut at shutdown") {
		t.Error("a follower that resumed within the grace was cut")
	}
}
