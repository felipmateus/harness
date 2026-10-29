package stream

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/felipmateus/harness/internal/broker"
	"github.com/felipmateus/harness/internal/ledger"
	"github.com/felipmateus/harness/internal/testutil/testhome"
)

const wait = 5 * time.Second

// testCtx returns a context that is cancelled when the test ends, before the goroutine
// leak check runs (cleanups are last-in first-out).
func testCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	return ctx
}

// openLedger opens a real ledger in a temporary home and closes it when the test ends.
func openLedger(t *testing.T, onCommit func(ledger.Event)) *ledger.Ledger {
	t.Helper()
	l, err := ledger.Open(context.Background(), ledger.Options{
		Path:     filepath.Join(testhome.New(t).Dir, "harness.db"),
		OnCommit: onCommit,
	})
	if err != nil {
		t.Fatalf("ledger.Open: %v", err)
	}
	t.Cleanup(func() { l.Close(context.Background()) })
	return l
}

// appendTick commits one event to stream and returns it.
func appendTick(t *testing.T, l *ledger.Ledger, stream string) ledger.Event {
	t.Helper()
	ev, _, err := l.Append(context.Background(), ledger.NewEvent{Stream: stream, Type: "tick"})
	if err != nil {
		t.Fatalf("Append: %v", err)
	}
	return ev
}

// appendTicks commits n events to stream.
func appendTicks(t *testing.T, l *ledger.Ledger, stream string, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		appendTick(t, l, stream)
	}
}

// next returns the next item, failing when the channel closes or stays silent.
func next(t *testing.T, ch <-chan Item) Item {
	t.Helper()
	select {
	case it, ok := <-ch:
		if !ok {
			t.Fatal("channel closed, want another item")
		}
		return it
	case <-time.After(wait):
		t.Fatal("no item within the timeout")
		return Item{}
	}
}

// wantEvent asserts that the next item is the event with the given seq.
func wantEvent(t *testing.T, ch <-chan Item, seq int64) {
	t.Helper()
	it := next(t, ch)
	if it.Err != nil || it.Event.Seq != seq {
		t.Fatalf("item = {seq %d, err %v}, want event seq %d", it.Event.Seq, it.Err, seq)
	}
}

// wantEvents asserts items for seq from..to.
func wantEvents(t *testing.T, ch <-chan Item, from, to int64) {
	t.Helper()
	for seq := from; seq <= to; seq++ {
		wantEvent(t, ch, seq)
	}
}

// wantTerminal asserts that the next item is the terminal error.
func wantTerminal(t *testing.T, ch <-chan Item, want error) {
	t.Helper()
	it := next(t, ch)
	if it.Err == nil || it.Event.Seq != 0 {
		t.Fatalf("item = {seq %d, err %v}, want terminal %v", it.Event.Seq, it.Err, want)
	}
	if !errors.Is(it.Err, want) {
		t.Fatalf("terminal error = %v, want %v", it.Err, want)
	}
}

// wantClosed asserts that the channel closes without another item.
func wantClosed(t *testing.T, ch <-chan Item) {
	t.Helper()
	select {
	case it, ok := <-ch:
		if ok {
			t.Fatalf("item = {seq %d, err %v}, want the channel closed", it.Event.Seq, it.Err)
		}
	case <-time.After(wait):
		t.Fatal("channel not closed within the timeout")
	}
}

// wantIdle asserts that nothing arrives for d. It can only miss a late item, never invent one.
func wantIdle(t *testing.T, ch <-chan Item, d time.Duration) {
	t.Helper()
	select {
	case it, ok := <-ch:
		t.Fatalf("unexpected receive: {seq %d, err %v}, open=%v", it.Event.Seq, it.Err, ok)
	case <-time.After(d):
	}
}

// waitFor polls cond until it holds or the timeout passes.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(wait)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

// callLog records the order of calls made by the fakes.
type callLog struct {
	mu    sync.Mutex
	calls []string
}

func (l *callLog) add(call string) {
	l.mu.Lock()
	l.calls = append(l.calls, call)
	l.mu.Unlock()
}

func (l *callLog) snapshot() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.calls...)
}

// recordingReader logs every Read before delegating to the real reader.
type recordingReader struct {
	Reader
	log *callLog
}

func (r recordingReader) Read(ctx context.Context, stream string, after int64, limit int) (ledger.Page, error) {
	r.log.add("read")
	return r.Reader.Read(ctx, stream, after, limit)
}

// fakeSub is a subscription the test drives by hand: push delivers a live event, degrade
// closes it the way the broker closes a subscription whose buffer overflowed.
type fakeSub struct {
	mu       sync.Mutex
	ch       chan ledger.Event
	closed   bool
	degraded atomic.Bool
}

func (s *fakeSub) Events() <-chan ledger.Event { return s.ch }
func (s *fakeSub) Degraded() bool              { return s.degraded.Load() }
func (s *fakeSub) Close()                      { s.shut(false) }
func (s *fakeSub) degrade()                    { s.shut(true) }

func (s *fakeSub) push(ev ledger.Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	select {
	case s.ch <- ev:
	default:
		panic("fakeSub: test overflowed the subscription buffer")
	}
}

func (s *fakeSub) shut(degraded bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	s.closed = true
	if degraded {
		s.degraded.Store(true)
	}
	close(s.ch)
}

// fakeSubscriber hands out fakeSubs and closes on demand, like a broker the test controls.
type fakeSubscriber struct {
	mu     sync.Mutex
	subs   []*fakeSub
	done   chan struct{}
	closed bool
	log    *callLog // optional
}

func newFakeSubscriber(log *callLog) *fakeSubscriber {
	return &fakeSubscriber{done: make(chan struct{}), log: log}
}

func (f *fakeSubscriber) Subscribe(string) broker.Subscription {
	if f.log != nil {
		f.log.add("subscribe")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	s := &fakeSub{ch: make(chan ledger.Event, broker.SubscriberBuffer)}
	if f.closed {
		s.closed = true
		close(s.ch)
	}
	f.subs = append(f.subs, s)
	return s
}

func (f *fakeSubscriber) Done() <-chan struct{} { return f.done }

// count is how many times Subscribe was called.
func (f *fakeSubscriber) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.subs)
}

// sub returns the n-th (1-based) subscription, waiting until it exists.
func (f *fakeSubscriber) sub(t *testing.T, n int) *fakeSub {
	t.Helper()
	waitFor(t, fmt.Sprintf("subscription %d", n), func() bool { return f.count() >= n })
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.subs[n-1]
}

// closeBroker closes Done and every subscription without degrading, like broker.Close.
func (f *fakeSubscriber) closeBroker() {
	f.mu.Lock()
	if f.closed {
		f.mu.Unlock()
		return
	}
	f.closed = true
	close(f.done)
	subs := append([]*fakeSub(nil), f.subs...)
	f.mu.Unlock()
	for _, s := range subs {
		s.shut(false)
	}
}

// memLedger is a minimal in-memory Reader for cases that need thousands of events, which a
// real ledger would take seconds to commit. It is gap-free and returns pages the way the
// ledger does.
type memLedger struct {
	mu       sync.Mutex
	events   map[string][]ledger.Event
	pageCap  int // when > 0, no page holds more events than this, whatever the limit
	reads    atomic.Int32
	onRead   func(n int)                           // called at the start of the n-th Read (1-based); may block
	err      error                                 // returned by Read when set
	override func(after int64) (ledger.Page, bool) // replaces the answer when it returns true
}

func newMemLedger() *memLedger {
	return &memLedger{events: make(map[string][]ledger.Event)}
}

// append adds the next event of stream, as the ledger would assign it.
func (m *memLedger) append(stream string) ledger.Event {
	m.mu.Lock()
	defer m.mu.Unlock()
	seq := int64(len(m.events[stream]) + 1)
	ev := ledger.Event{Stream: stream, Seq: seq, ID: fmt.Sprintf("%s-%d", stream, seq), Type: "tick", Data: []byte(`{}`)}
	m.events[stream] = append(m.events[stream], ev)
	return ev
}

func (m *memLedger) fill(stream string, n int) {
	for i := 0; i < n; i++ {
		m.append(stream)
	}
}

func (m *memLedger) Read(_ context.Context, stream string, after int64, limit int) (ledger.Page, error) {
	n := m.reads.Add(1)
	if m.onRead != nil {
		m.onRead(int(n))
	}
	if m.err != nil {
		return ledger.Page{}, m.err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.override != nil {
		if page, ok := m.override(after); ok {
			return page, nil
		}
	}
	all := m.events[stream]
	if m.pageCap > 0 && limit > m.pageCap {
		limit = m.pageCap
	}
	page := ledger.Page{Events: []ledger.Event{}}
	for i := int(after); i < len(all); i++ {
		if len(page.Events) == limit {
			page.HasMore = true
			break
		}
		page.Events = append(page.Events, all[i])
	}
	return page, nil
}

// countingSubscriber counts Subscribe calls on a real broker.
type countingSubscriber struct {
	Subscriber
	n atomic.Int32
}

func (c *countingSubscriber) Subscribe(stream string) broker.Subscription {
	c.n.Add(1)
	return c.Subscriber.Subscribe(stream)
}

func (c *countingSubscriber) count() int { return int(c.n.Load()) }
