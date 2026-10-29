package stream

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/felipmateus/harness/internal/broker"
	"github.com/felipmateus/harness/internal/ledger"
	"github.com/felipmateus/harness/internal/testutil/leakcheck"
)

// UT-060
func TestFollow_BrokerCloseDeliversPendingEventsThenErrClosed(t *testing.T) {
	leakcheck.Check(t)
	ctx := testCtx(t)
	l := openLedger(t, nil)
	appendTicks(t, l, "daemon", 2)
	fs := newFakeSubscriber(nil)

	ch := Follow(ctx, l, fs, "daemon", 0)
	wantEvents(t, ch, 1, 2)

	// Two events are published and the broker closes before the consumer reads either.
	sub := fs.sub(t, 1)
	sub.push(appendTick(t, l, "daemon"))
	sub.push(appendTick(t, l, "daemon"))
	fs.closeBroker()

	wantEvents(t, ch, 3, 4)
	wantTerminal(t, ch, ErrClosed)
	wantClosed(t, ch)
}

// UT-060 against the real broker: the events published before Close are delivered, then
// ErrClosed, then the channel closes.
func TestFollow_RealBrokerCloseDeliversPendingEventsThenErrClosed(t *testing.T) {
	leakcheck.Check(t)
	ctx := testCtx(t)
	hub := broker.New()
	l := openLedger(t, hub.Publish)
	appendTicks(t, l, "daemon", 1)

	ch := Follow(ctx, l, hub, "daemon", 0)
	wantEvent(t, ch, 1)

	appendTicks(t, l, "daemon", 2) // published to the live subscription
	hub.Close()

	wantEvents(t, ch, 2, 3)
	wantTerminal(t, ch, ErrClosed)
	wantClosed(t, ch)
}

// UT-061
func TestFollow_HistoryThenLiveInOrderEachOnce(t *testing.T) {
	leakcheck.Check(t)
	ctx := testCtx(t)
	l := openLedger(t, nil)
	appendTicks(t, l, "daemon", 3)
	fs := newFakeSubscriber(nil)

	ch := Follow(ctx, l, fs, "daemon", 0)
	wantEvents(t, ch, 1, 3)

	sub := fs.sub(t, 1)
	sub.push(appendTick(t, l, "daemon"))
	sub.push(appendTick(t, l, "daemon"))
	wantEvents(t, ch, 4, 5)
	wantIdle(t, ch, 30*time.Millisecond)
}

// UT-062
func TestFollow_AfterSkipsEarlierEvents(t *testing.T) {
	leakcheck.Check(t)
	ctx := testCtx(t)
	l := openLedger(t, nil)
	appendTicks(t, l, "daemon", 5)
	fs := newFakeSubscriber(nil)

	ch := Follow(ctx, l, fs, "daemon", 3)

	wantEvents(t, ch, 4, 5)
	wantIdle(t, ch, 30*time.Millisecond)
	fs.closeBroker()
	wantTerminal(t, ch, ErrClosed)
	wantClosed(t, ch)
}

// UT-063
func TestFollow_DegradedSubscriptionResubscribesAndCatchesUp(t *testing.T) {
	leakcheck.Check(t)
	ctx := testCtx(t)
	l := openLedger(t, nil)
	appendTicks(t, l, "daemon", 5)
	fs := newFakeSubscriber(nil)

	ch := Follow(ctx, l, fs, "daemon", 0)
	wantEvents(t, ch, 1, 5)

	// The first subscription delivers 6..10 live, then degrades. Events 11..20 commit
	// after it stopped receiving, so only the ledger holds them.
	first := fs.sub(t, 1)
	for i := 0; i < 5; i++ {
		first.push(appendTick(t, l, "daemon"))
	}
	wantEvents(t, ch, 6, 10)
	appendTicks(t, l, "daemon", 10)
	first.degrade()

	wantEvents(t, ch, 11, 20)
	if got := fs.count(); got != 2 {
		t.Fatalf("Subscribe called %d times, want 2", got)
	}

	// The second subscription is live again.
	fs.sub(t, 2).push(appendTick(t, l, "daemon"))
	wantEvent(t, ch, 21)
	fs.closeBroker()
	wantTerminal(t, ch, ErrClosed)
	wantClosed(t, ch)
}

// UT-064
func TestFollow_EmptyStreamWaitsForTheFirstEvent(t *testing.T) {
	leakcheck.Check(t)
	ctx := testCtx(t)
	l := openLedger(t, nil)
	fs := newFakeSubscriber(nil)

	ch := Follow(ctx, l, fs, "daemon", 0)
	wantIdle(t, ch, 50*time.Millisecond)

	fs.sub(t, 1).push(appendTick(t, l, "daemon"))
	wantEvent(t, ch, 1)
	wantIdle(t, ch, 30*time.Millisecond)
}

// UT-066
func TestFollow_SubscribesBeforeEveryReadAndNeverYieldsStaleEvents(t *testing.T) {
	leakcheck.Check(t)
	ctx := testCtx(t)
	l := openLedger(t, nil)
	appendTicks(t, l, "daemon", 3)
	log := &callLog{}
	fs := newFakeSubscriber(log)

	ch := Follow(ctx, recordingReader{Reader: l, log: log}, fs, "daemon", 0)
	wantEvents(t, ch, 1, 3)

	// A live event at or below the cursor is the overlap with history: never yielded.
	first := fs.sub(t, 1)
	stale := ledger.Event{Stream: "daemon", Seq: 2, ID: "stale", Type: "tick"}
	first.push(stale)
	first.push(appendTick(t, l, "daemon"))
	wantEvent(t, ch, 4)

	// Every resubscription subscribes first, then reads.
	appendTick(t, l, "daemon")
	first.degrade()
	wantEvent(t, ch, 5)
	second := fs.sub(t, 2)
	appendTick(t, l, "daemon")
	second.degrade()
	wantEvent(t, ch, 6)
	second.push(stale)
	third := fs.sub(t, 3)
	third.push(appendTick(t, l, "daemon"))
	wantEvent(t, ch, 7)
	wantIdle(t, ch, 30*time.Millisecond)

	want := []string{"subscribe", "read", "subscribe", "read", "subscribe", "read"}
	got := log.snapshot()
	if len(got) != len(want) {
		t.Fatalf("calls = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("calls = %v, want %v", got, want)
		}
	}
}

// UT-110
func TestFollow_BrokerClosingDuringCatchUpEndsWithErrClosed(t *testing.T) {
	leakcheck.Check(t)
	ctx := testCtx(t)
	ml := newMemLedger()
	ml.fill("daemon", 5000) // ten catch-up pages
	fs := newFakeSubscriber(nil)

	// Hold the second Read so the broker closes while it is in flight.
	inSecondRead := make(chan struct{})
	release := make(chan struct{})
	ml.onRead = func(n int) {
		if n == 2 {
			close(inSecondRead)
			<-release
		}
	}

	ch := Follow(ctx, ml, fs, "daemon", 0)
	wantEvents(t, ch, 1, CatchUpPage) // the first page, delivered
	select {
	case <-inSecondRead:
	case <-time.After(wait):
		t.Fatal("Follow did not read the second page")
	}
	fs.closeBroker()
	close(release)

	// The second page was already read, but the follow is over: ErrClosed, not events.
	wantTerminal(t, ch, ErrClosed)
	wantClosed(t, ch)
	if got := ml.reads.Load(); got != 2 {
		t.Fatalf("Read called %d times, want 2 (one further Read after the first page)", got)
	}
}

func TestFollow_BrokerClosedBeforeTheFirstPageReadsNothing(t *testing.T) {
	leakcheck.Check(t)
	ctx := testCtx(t)
	ml := newMemLedger()
	ml.fill("daemon", 10)
	hub := broker.New()
	hub.Close()

	ch := Follow(ctx, ml, hub, "daemon", 0)

	wantTerminal(t, ch, ErrClosed)
	wantClosed(t, ch)
	if got := ml.reads.Load(); got != 0 {
		t.Fatalf("Read called %d times on a closed broker, want 0", got)
	}
}

// UT-111
func TestFollow_BrokerCloseFreesAStuckConsumer(t *testing.T) {
	ctx := testCtx(t)
	ml := newMemLedger()
	hub := broker.New()
	baseline := leakcheck.Baseline()
	leakcheck.Check(t)

	ch := Follow(ctx, ml, hub, "daemon", 0)
	publish := func() { hub.Publish(ml.append("daemon")) }

	// The consumer reads one item, then stops while live events keep arriving: more than
	// the output buffer and the subscription buffer hold together.
	publish()
	wantEvent(t, ch, 1)
	for i := 0; i < outBuffer+broker.SubscriberBuffer+20; i++ {
		publish()
	}
	waitFor(t, "the output buffer to fill", func() bool { return len(ch) == cap(ch) })

	started := time.Now()
	hub.Close()
	if n := leakcheck.Settle(baseline, 100*time.Millisecond); n > baseline {
		t.Fatalf("Follow goroutine still running %v after Close (%d goroutines, baseline %d)", time.Since(started), n, baseline)
	}

	// What was buffered is intact: consecutive events, then the channel closes (the
	// terminal item could not be placed in a full buffer).
	want := int64(2)
	for it := range ch {
		if it.Err != nil {
			if !errors.Is(it.Err, ErrClosed) {
				t.Fatalf("terminal error = %v", it.Err)
			}
			continue
		}
		if it.Event.Seq != want {
			t.Fatalf("buffered event seq %d, want %d", it.Event.Seq, want)
		}
		want++
	}
	if want-2 != outBuffer {
		t.Fatalf("%d buffered events survived, want %d", want-2, outBuffer)
	}
}

// UT-111 (context): cancelling ctx frees the follow too.
func TestFollow_ContextCancelFreesAStuckConsumer(t *testing.T) {
	ml := newMemLedger()
	hub := broker.New()
	baseline := leakcheck.Baseline()
	leakcheck.Check(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	ch := Follow(ctx, ml, hub, "daemon", 0)
	for i := 0; i < outBuffer+5; i++ {
		hub.Publish(ml.append("daemon"))
	}
	wantEvent(t, ch, 1)
	waitFor(t, "the output buffer to fill", func() bool { return len(ch) == cap(ch) })

	cancel()
	if n := leakcheck.Settle(baseline, 100*time.Millisecond); n > baseline {
		t.Fatalf("Follow goroutine still running after cancel (%d goroutines, baseline %d)", n, baseline)
	}
	for it := range ch {
		if errors.Is(it.Err, ErrClosed) {
			t.Fatal("a cancelled follow reported ErrClosed")
		}
	}
}

func TestFollow_ContextCancelEndsWithoutATerminalItem(t *testing.T) {
	leakcheck.Check(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	l := openLedger(t, nil)
	appendTicks(t, l, "daemon", 2)
	fs := newFakeSubscriber(nil)

	ch := Follow(ctx, l, fs, "daemon", 0)
	wantEvents(t, ch, 1, 2)
	cancel()

	wantClosed(t, ch)
}

func TestFollow_ContextWinsOverABrokerThatClosedToo(t *testing.T) {
	leakcheck.Check(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	ml := newMemLedger()
	ml.fill("daemon", 3)
	fs := newFakeSubscriber(nil)
	cancel()
	fs.closeBroker()

	ch := Follow(ctx, ml, fs, "daemon", 0)

	wantClosed(t, ch)
	if got := ml.reads.Load(); got != 0 {
		t.Fatalf("Read called %d times after cancel, want 0", got)
	}
}

func TestFollow_ReadErrorIsTheTerminalItem(t *testing.T) {
	leakcheck.Check(t)
	ctx := testCtx(t)
	boom := errors.New("disk on fire")
	ml := newMemLedger()
	ml.err = boom
	fs := newFakeSubscriber(nil)

	ch := Follow(ctx, ml, fs, "daemon", 0)

	wantTerminal(t, ch, boom)
	wantClosed(t, ch)
}

func TestFollow_ReadErrorAfterTheBrokerClosedIsAShutdown(t *testing.T) {
	leakcheck.Check(t)
	ctx := testCtx(t)
	ml := newMemLedger()
	fs := newFakeSubscriber(nil)
	ml.onRead = func(int) { fs.closeBroker() }
	ml.err = ledger.ErrClosed // the ledger closes right behind the broker

	ch := Follow(ctx, ml, fs, "daemon", 0)

	wantTerminal(t, ch, ErrClosed)
	wantClosed(t, ch)
}

func TestFollow_ReadErrorDuringLiveGapCatchUpIsTerminal(t *testing.T) {
	leakcheck.Check(t)
	ctx := testCtx(t)
	ml := newMemLedger()
	ml.fill("daemon", 1)
	fs := newFakeSubscriber(nil)

	ch := Follow(ctx, ml, fs, "daemon", 0)
	wantEvent(t, ch, 1)

	boom := errors.New("read failed")
	ml.err = boom
	fs.sub(t, 1).push(ledger.Event{Stream: "daemon", Seq: 9, ID: "x", Type: "tick"}) // a gap

	wantTerminal(t, ch, boom)
	wantClosed(t, ch)
}

func TestFollow_LiveGapIsFilledFromTheLedger(t *testing.T) {
	leakcheck.Check(t)
	ctx := testCtx(t)
	l := openLedger(t, nil)
	appendTicks(t, l, "daemon", 2)
	fs := newFakeSubscriber(nil)

	ch := Follow(ctx, l, fs, "daemon", 0)
	wantEvents(t, ch, 1, 2)

	// Events 3 and 4 commit, but only 5 reaches the subscription: a gap the broker never
	// produces. Follow reads the missing events instead of skipping them.
	appendTicks(t, l, "daemon", 2)
	fifth := appendTick(t, l, "daemon")
	sub := fs.sub(t, 1)
	sub.push(fifth)
	wantEvents(t, ch, 3, 5)

	// The overlap is deduplicated afterwards, and the subscription stays in use.
	sub.push(fifth)
	sub.push(appendTick(t, l, "daemon"))
	wantEvent(t, ch, 6)
	if got := fs.count(); got != 1 {
		t.Fatalf("Subscribe called %d times, a gap needs no resubscription", got)
	}
}

func TestFollow_CatchUpPagesUntilHasMoreIsFalse(t *testing.T) {
	leakcheck.Check(t)
	ctx := testCtx(t)
	ml := newMemLedger()
	ml.fill("daemon", 3*CatchUpPage+7)
	fs := newFakeSubscriber(nil)

	ch := Follow(ctx, ml, fs, "daemon", 0)

	wantEvents(t, ch, 1, 3*CatchUpPage+7)
	wantIdle(t, ch, 20*time.Millisecond)
	if got := ml.reads.Load(); got != 4 {
		t.Fatalf("Read called %d times, want 4 pages", got)
	}
}

// A page can be shorter than the limit because the ledger cut it at its byte budget. Only
// !HasMore ends the catch-up.
func TestFollow_ShortPageWithHasMoreDoesNotEndTheCatchUp(t *testing.T) {
	leakcheck.Check(t)
	ctx := testCtx(t)
	ml := newMemLedger()
	ml.pageCap = 3
	ml.fill("daemon", 10)
	fs := newFakeSubscriber(nil)

	ch := Follow(ctx, ml, fs, "daemon", 0)

	wantEvents(t, ch, 1, 10)
	wantIdle(t, ch, 20*time.Millisecond)
	if got := ml.reads.Load(); got != 4 {
		t.Fatalf("Read called %d times, want 4 short pages", got)
	}
}

func TestFollow_AfterBeyondTheHeadWaitsForTheNextSequence(t *testing.T) {
	leakcheck.Check(t)
	ctx := testCtx(t)
	ml := newMemLedger()
	ml.fill("daemon", 2)
	fs := newFakeSubscriber(nil)

	ch := Follow(ctx, ml, fs, "daemon", 4)
	wantIdle(t, ch, 30*time.Millisecond)

	sub := fs.sub(t, 1)
	sub.push(ml.append("daemon")) // seq 3: already covered by the cursor
	sub.push(ml.append("daemon")) // seq 4
	sub.push(ml.append("daemon")) // seq 5
	wantEvent(t, ch, 5)
	wantIdle(t, ch, 20*time.Millisecond)
}

func TestFollow_NegativeAfterStartsAtTheBeginning(t *testing.T) {
	leakcheck.Check(t)
	ctx := testCtx(t)
	ml := newMemLedger()
	ml.fill("daemon", 2)
	fs := newFakeSubscriber(nil)

	ch := Follow(ctx, ml, fs, "daemon", -5)

	wantEvents(t, ch, 1, 2)
}

func TestFollow_OtherStreamsAreNotDelivered(t *testing.T) {
	leakcheck.Check(t)
	ctx := testCtx(t)
	hub := broker.New()
	l := openLedger(t, hub.Publish)
	appendTick(t, l, "daemon")

	ch := Follow(ctx, l, hub, "daemon", 0)
	wantEvent(t, ch, 1)

	appendTick(t, l, "other")
	appendTick(t, l, "daemon")
	wantEvent(t, ch, 2)
	wantIdle(t, ch, 20*time.Millisecond)
}

// The reader must not break Safety Invariant 6: a gap or a stall in history ends the
// follow with an error instead of reaching the consumer.
func TestFollow_CorruptHistoryIsATerminalError(t *testing.T) {
	skip := func(after int64) (ledger.Page, bool) {
		return ledger.Page{Events: []ledger.Event{{Stream: "daemon", Seq: after + 2, ID: "g", Type: "tick"}}}, true
	}
	stall := func(int64) (ledger.Page, bool) {
		return ledger.Page{Events: []ledger.Event{}, HasMore: true}, true
	}
	for name, override := range map[string]func(int64) (ledger.Page, bool){"gap": skip, "stalled page": stall} {
		t.Run(name, func(t *testing.T) {
			leakcheck.Check(t)
			ctx := testCtx(t)
			ml := newMemLedger()
			ml.override = override
			fs := newFakeSubscriber(nil)

			ch := Follow(ctx, ml, fs, "daemon", 0)

			it := next(t, ch)
			if it.Err == nil || errors.Is(it.Err, ErrClosed) {
				t.Fatalf("item = %+v, want a descriptive terminal error", it)
			}
			wantClosed(t, ch)
		})
	}
}

// The shippable outcome end to end: a real ledger publishing to a real broker, and a
// consumer that stops reading long enough for its subscription to degrade. It still
// receives every event exactly once and in order.
func TestFollow_RealLedgerAndBrokerSlowConsumerReceivesEveryEventOnce(t *testing.T) {
	leakcheck.Check(t)
	ctx := testCtx(t)
	hub := broker.New()
	l := openLedger(t, hub.Publish)
	counter := &countingSubscriber{Subscriber: hub}

	appendTick(t, l, "daemon")
	ch := Follow(ctx, l, counter, "daemon", 0)
	wantEvent(t, ch, 1)

	// Follow takes at most outBuffer+1 more events before it blocks on the consumer; the
	// rest pile up in the subscription until it overflows.
	const total = 1 + outBuffer + 1 + broker.SubscriberBuffer + 10
	appendTicks(t, l, "daemon", total-1)

	wantEvents(t, ch, 2, total)
	if got := counter.count(); got < 2 {
		t.Fatalf("Subscribe called %d times, the subscription should have degraded and been replaced", got)
	}

	appendTick(t, l, "daemon")
	wantEvent(t, ch, total+1)
	hub.Close()
	wantTerminal(t, ch, ErrClosed)
	wantClosed(t, ch)
}
