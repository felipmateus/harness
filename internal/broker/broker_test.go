package broker

import (
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/felipmateus/harness/internal/ledger"
	"github.com/felipmateus/harness/internal/testutil/leakcheck"
)

func event(stream string, seq int64) ledger.Event {
	return ledger.Event{
		Stream: stream,
		Seq:    seq,
		ID:     fmt.Sprintf("%s-%d", stream, seq),
		Type:   "tick",
		Data:   []byte(`{}`),
	}
}

// recv returns the next event, or fails when none arrives within a second.
func recv(t *testing.T, s Subscription) ledger.Event {
	t.Helper()
	select {
	case ev, ok := <-s.Events():
		if !ok {
			t.Fatal("subscription closed, want an event")
		}
		return ev
	case <-time.After(time.Second):
		t.Fatal("no event within 1s")
		return ledger.Event{}
	}
}

// assertClosed fails unless the subscription's channel is closed once its buffer is read.
func assertClosed(t *testing.T, s Subscription) {
	t.Helper()
	deadline := time.After(time.Second)
	for {
		select {
		case _, ok := <-s.Events():
			if !ok {
				return
			}
		case <-deadline:
			t.Fatal("channel not closed within 1s")
		}
	}
}

func assertIdle(t *testing.T, s Subscription) {
	t.Helper()
	select {
	case ev, ok := <-s.Events():
		t.Fatalf("unexpected receive: %+v, open=%v", ev, ok)
	default:
	}
}

// UT-030
func TestPublish_FansOutToSubscribersOfTheSameStream(t *testing.T) {
	leakcheck.Check(t)
	h := New()
	a, b, other := h.Subscribe("daemon"), h.Subscribe("daemon"), h.Subscribe("other")

	h.Publish(event("daemon", 1))

	for name, s := range map[string]Subscription{"a": a, "b": b} {
		if got := recv(t, s); got.Seq != 1 || got.Stream != "daemon" {
			t.Errorf("subscriber %s got %+v, want daemon seq 1", name, got)
		}
	}
	assertIdle(t, other)
}

// UT-031
func TestPublish_DegradesAnUnreadSubscriptionWithoutBlocking(t *testing.T) {
	leakcheck.Check(t)
	h := New()
	slow := h.Subscribe("daemon")
	fast := h.Subscribe("daemon")

	// Publishing runs in its own goroutine, so a Publish that blocks fails the test with a
	// timeout instead of hanging it. Neither goroutine calls t.Fatal.
	failed := make(chan string, 1)
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		// The fast subscriber keeps up, so only the slow one overflows. Publishing to the
		// degraded, removed subscription afterwards must not block either.
		for i := int64(1); i <= SubscriberBuffer+10; i++ {
			h.Publish(event("daemon", i))
			select {
			case ev := <-fast.Events():
				if ev.Seq != i {
					failed <- fmt.Sprintf("fast subscriber got seq %d, want %d", ev.Seq, i)
					return
				}
			case <-time.After(time.Second):
				failed <- fmt.Sprintf("fast subscriber got nothing for seq %d", i)
				return
			}
		}
	}()
	select {
	case <-finished:
	case <-time.After(5 * time.Second):
		t.Fatal("Publish blocked on a subscription that never reads")
	}
	select {
	case msg := <-failed:
		t.Fatal(msg)
	default:
	}

	// The slow subscription still yields what was buffered before it degraded, then closes.
	var seqs []int64
	for ev := range slow.Events() {
		seqs = append(seqs, ev.Seq)
	}
	if len(seqs) != SubscriberBuffer {
		t.Fatalf("slow subscription delivered %d events, want the %d buffered", len(seqs), SubscriberBuffer)
	}
	for i, seq := range seqs {
		if seq != int64(i+1) {
			t.Fatalf("buffered event %d has seq %d, want %d", i, seq, i+1)
		}
	}
	if !slow.Degraded() {
		t.Error("Degraded() = false after the buffer overflowed")
	}
	if fast.Degraded() {
		t.Error("the subscription that kept up was degraded")
	}
}

// UT-031 (boundary): the 64th event still fits; the 65th is the one that overflows.
func TestPublish_BufferHoldsExactlySubscriberBuffer(t *testing.T) {
	h := New()
	s := h.Subscribe("daemon")
	for i := int64(1); i <= SubscriberBuffer; i++ {
		h.Publish(event("daemon", i))
	}
	if s.Degraded() {
		t.Fatalf("degraded after %d publishes, the buffer holds %d", SubscriberBuffer, SubscriberBuffer)
	}
	h.Publish(event("daemon", SubscriberBuffer+1))
	if !s.Degraded() {
		t.Fatalf("not degraded after %d publishes", SubscriberBuffer+1)
	}
	if SubscriberBuffer != 64 {
		t.Fatalf("SubscriberBuffer = %d, the contract is 64", SubscriberBuffer)
	}
}

// UT-032
func TestPublish_FastSubscribersReceiveEveryEventInOrder(t *testing.T) {
	leakcheck.Check(t)
	const (
		subscribers = 100
		events      = 1000
		batch       = 32 // well under SubscriberBuffer, so a fast reader can never overflow
	)
	h := New()

	var received atomic.Int64
	errs := make(chan error, subscribers)
	var wg sync.WaitGroup
	for i := 0; i < subscribers; i++ {
		sub := h.Subscribe("daemon")
		wg.Add(1)
		go func() {
			defer wg.Done()
			want := int64(1)
			for ev := range sub.Events() {
				if ev.Seq != want {
					errs <- fmt.Errorf("received seq %d, want %d", ev.Seq, want)
					return
				}
				want++
				received.Add(1)
			}
			if want != events+1 || sub.Degraded() {
				errs <- fmt.Errorf("stream ended after %d events (degraded=%v), want %d", want-1, sub.Degraded(), events)
			}
		}()
	}

	for i := int64(1); i <= events; i++ {
		h.Publish(event("daemon", i))
		if i%batch == 0 || i == events {
			waitFor(t, func() bool { return received.Load() == int64(subscribers)*i })
		}
	}
	h.Close()
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

// waitFor polls cond for up to ten seconds.
func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met within 10s")
		}
		runtime.Gosched()
	}
}

// UT-033
func TestClose_ClosesEverySubscriptionWithoutDegrading(t *testing.T) {
	h := New()
	subs := []Subscription{h.Subscribe("a"), h.Subscribe("a"), h.Subscribe("b")}
	h.Publish(event("a", 1))

	h.Close()
	h.Close() // a second Close is a no-op, not a panic

	if got := recv(t, subs[0]); got.Seq != 1 {
		t.Errorf("event buffered before Close was lost: %+v", got)
	}
	for i, s := range subs {
		assertClosed(t, s)
		if s.Degraded() {
			t.Errorf("subscription %d degraded by Close", i)
		}
	}
	select {
	case <-h.Done():
	default:
		t.Error("Done is not closed after Close")
	}
}

// UT-034
func TestClose_LateSubscribeAndPublishAreSafe(t *testing.T) {
	h := New()
	h.Close()

	select {
	case <-h.Done():
	default:
		t.Fatal("Done is not closed after Close")
	}
	late := h.Subscribe("daemon")
	select {
	case _, ok := <-late.Events():
		if ok {
			t.Fatal("late subscription delivered an event")
		}
	default:
		t.Fatal("late subscription's channel is not closed")
	}
	if late.Degraded() {
		t.Error("late subscription is degraded; it must read as a plain close")
	}

	h.Publish(event("daemon", 1)) // no panic, nothing delivered
	late.Close()                  // safe on an already-closed subscription
	if ev, ok := <-late.Events(); ok {
		t.Fatalf("Publish after Close delivered %+v", ev)
	}
}

func TestSubscriptionClose_UnsubscribesOnlyItself(t *testing.T) {
	h := New()
	keep, drop := h.Subscribe("daemon"), h.Subscribe("daemon")

	drop.Close()
	drop.Close() // idempotent
	h.Publish(event("daemon", 1))

	assertClosed(t, drop)
	if drop.Degraded() {
		t.Error("an explicit Close is not a degradation")
	}
	if got := recv(t, keep); got.Seq != 1 {
		t.Errorf("remaining subscription got %+v", got)
	}
	h.Close()
	drop.Close() // after the broker closed
}

func TestSubscriptionClose_DropsEmptyStreamSets(t *testing.T) {
	h := New()
	s := h.Subscribe("daemon")
	s.Close()
	h.mu.Lock()
	n := len(h.subs)
	h.mu.Unlock()
	if n != 0 {
		t.Errorf("%d stream sets kept after the last subscriber left, want 0", n)
	}
}

func TestPublish_DegradedSubscriptionStaysDegradedAfterClose(t *testing.T) {
	h := New()
	s := h.Subscribe("daemon")
	for i := int64(1); i <= SubscriberBuffer+1; i++ {
		h.Publish(event("daemon", i))
	}
	s.Close()
	h.Close()
	if !s.Degraded() {
		t.Error("Degraded() flipped to false after Close")
	}
}

func TestPublish_OnlyTheSlowSubscriberIsDegraded(t *testing.T) {
	h := New()
	slow, fast := h.Subscribe("daemon"), h.Subscribe("daemon")
	for i := int64(1); i <= 3*SubscriberBuffer; i++ {
		h.Publish(event("daemon", i))
		recv(t, fast)
	}
	if !slow.Degraded() || fast.Degraded() {
		t.Fatalf("degraded: slow=%v fast=%v, want true/false", slow.Degraded(), fast.Degraded())
	}
}

func TestBroker_ConcurrentUseIsRaceFree(t *testing.T) {
	leakcheck.Check(t)
	h := New()
	var wg sync.WaitGroup

	wg.Add(1)
	go func() { // the writer goroutine
		defer wg.Done()
		for i := int64(1); i <= 2000; i++ {
			h.Publish(event("daemon", i))
		}
	}()
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func() { // followers that come and go, some reading and some not
			defer wg.Done()
			for i := 0; i < 50; i++ {
				s := h.Subscribe("daemon")
				select {
				case <-s.Events():
				default:
				}
				s.Close()
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		time.Sleep(time.Millisecond)
		h.Close()
	}()
	wg.Wait()
}

// degradeCall is one OnDegrade notification.
type degradeCall struct {
	stream string
	seq    int64
}

// OnDegrade: the observer hears about each degraded subscription once, with the stream and
// the sequence of the event that did not fit, and about nothing else.
func TestOnDegrade_ReportsEachDegradedSubscriptionOnce(t *testing.T) {
	leakcheck.Check(t)
	var mu sync.Mutex
	var calls []degradeCall
	h := New(OnDegrade(func(stream string, seq int64) {
		mu.Lock()
		defer mu.Unlock()
		calls = append(calls, degradeCall{stream, seq})
	}))
	slowA, slowB := h.Subscribe("daemon"), h.Subscribe("daemon")
	fast := h.Subscribe("daemon")
	h.Subscribe("other") // a different stream: never overflows

	snapshot := func() []degradeCall {
		mu.Lock()
		defer mu.Unlock()
		return append([]degradeCall(nil), calls...)
	}

	// The 64th event still fits in every buffer: no notification yet.
	for i := int64(1); i <= SubscriberBuffer; i++ {
		h.Publish(event("daemon", i))
		<-fast.Events()
	}
	if got := snapshot(); len(got) != 0 {
		t.Fatalf("OnDegrade called %v before any buffer overflowed", got)
	}

	// The 65th overflows both slow subscriptions; the fast one keeps up.
	h.Publish(event("daemon", SubscriberBuffer+1))
	<-fast.Events()
	want := []degradeCall{{"daemon", SubscriberBuffer + 1}, {"daemon", SubscriberBuffer + 1}}
	if got := snapshot(); len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("OnDegrade calls = %v, want %v", got, want)
	}
	if !slowA.Degraded() || !slowB.Degraded() || fast.Degraded() {
		t.Errorf("degraded flags a=%v b=%v fast=%v, want true true false", slowA.Degraded(), slowB.Degraded(), fast.Degraded())
	}

	// A degraded subscription is gone, so more events report nothing; neither does a
	// normal unsubscribe or the broker closing.
	h.Publish(event("daemon", SubscriberBuffer+2))
	<-fast.Events()
	fast.Close()
	h.Close()
	if got := snapshot(); len(got) != 2 {
		t.Errorf("OnDegrade calls = %v, want exactly the two from the overflow", got)
	}
}

// OnDegrade runs after the Hub's lock is released: an observer may call back into the Hub
// (here Subscribe, which takes the lock) without deadlocking the publisher.
func TestOnDegrade_RunsOutsideTheHubLock(t *testing.T) {
	leakcheck.Check(t)
	var h *Hub
	reentered := make(chan struct{}, 1)
	h = New(OnDegrade(func(stream string, _ int64) {
		sub := h.Subscribe(stream)
		sub.Close()
		reentered <- struct{}{}
	}))
	h.Subscribe("daemon")

	finished := make(chan struct{})
	go func() {
		defer close(finished)
		for i := int64(1); i <= SubscriberBuffer+1; i++ {
			h.Publish(event("daemon", i))
		}
	}()
	select {
	case <-finished:
	case <-time.After(5 * time.Second):
		t.Fatal("Publish deadlocked: the observer ran under the Hub's lock")
	}
	select {
	case <-reentered:
	default:
		t.Fatal("the observer was not called")
	}
}

// A Hub without an observer degrades silently, as before.
func TestOnDegrade_AbsentObserverIsHarmless(t *testing.T) {
	h := New()
	s := h.Subscribe("daemon")
	for i := int64(1); i <= SubscriberBuffer+1; i++ {
		h.Publish(event("daemon", i))
	}
	if !s.Degraded() {
		t.Error("subscription not degraded")
	}
}
