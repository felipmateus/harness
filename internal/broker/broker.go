// Package broker fans committed ledger events out to in-memory subscribers.
//
// The ledger's writer goroutine calls Publish after each commit, so Publish must never
// block: every subscription has a fixed buffer, and one that overflows is degraded (its
// channel is closed and Degraded reports true) and removed. The follower then catches up
// from the ledger and resubscribes, so a slow consumer costs history reads, never writer
// latency. The package imports only the ledger's types.
package broker

import (
	"sync"
	"sync/atomic"

	"github.com/felipmateus/harness/internal/ledger"
)

// SubscriberBuffer is how many events one subscription holds before it degrades.
const SubscriberBuffer = 64

// Subscription is one consumer's view of a stream.
type Subscription interface {
	// Events delivers published events in publish order. The channel is closed when the
	// broker closes, when the subscription is closed, or when it degrades; events buffered
	// before the close are still delivered first. Events share their Data slice with every
	// other subscriber: treat it as read-only.
	Events() <-chan ledger.Event
	// Degraded reports that the channel was closed because the buffer overflowed.
	Degraded() bool
	// Close unsubscribes. It is idempotent and safe after the broker closed.
	Close()
}

// Broker is the fan-out contract.
type Broker interface {
	// Subscribe registers a subscription for stream. After Close it returns a subscription
	// that is already closed and not degraded.
	Subscribe(stream string) Subscription
	// Publish offers ev to every subscription of ev.Stream without blocking. A full buffer
	// degrades and removes that subscription. After Close it does nothing.
	Publish(ev ledger.Event)
	// Done is closed by Close.
	Done() <-chan struct{}
	// Close closes every subscription (not degraded) and Done. It is idempotent.
	Close()
}

// Hub is the in-memory Broker.
//
// A single mutex guards the subscriber sets and every send on or close of a subscription
// channel, so a send never races a close. Publish holds it only for non-blocking sends.
type Hub struct {
	mu        sync.Mutex
	closed    bool
	subs      map[string]map[*subscription]struct{}
	done      chan struct{}
	onDegrade func(stream string, seq int64)
}

var _ Broker = (*Hub)(nil)

// Option configures a Hub.
type Option func(*Hub)

// OnDegrade registers fn to be told when a subscription overflows and is degraded. The
// broker cannot see the follower's own cursor, so fn receives the stream and the sequence
// of the event that did not fit: the newest event of the stream at that moment.
//
// fn runs on the goroutine that called Publish (the ledger's writer), once per degraded
// subscription and after the Hub's lock is released. It must not block, but it may call
// back into the Hub. The broker imports no logging: the owner of the Hub turns this
// signal into a log record.
func OnDegrade(fn func(stream string, seq int64)) Option {
	return func(h *Hub) { h.onDegrade = fn }
}

// New returns an open Hub.
func New(opts ...Option) *Hub {
	h := &Hub{
		subs: make(map[string]map[*subscription]struct{}),
		done: make(chan struct{}),
	}
	for _, opt := range opts {
		opt(h)
	}
	return h
}

// Subscribe implements Broker.
func (h *Hub) Subscribe(stream string) Subscription {
	s := &subscription{hub: h, stream: stream, events: make(chan ledger.Event, SubscriberBuffer)}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		s.closed = true
		close(s.events)
		return s
	}
	set := h.subs[stream]
	if set == nil {
		set = make(map[*subscription]struct{})
		h.subs[stream] = set
	}
	set[s] = struct{}{}
	return s
}

// Publish implements Broker.
func (h *Hub) Publish(ev ledger.Event) {
	degraded := h.publish(ev)
	// The callback runs outside the lock: a slow or re-entrant observer can never stall
	// another publisher or deadlock against Subscribe.
	if h.onDegrade != nil {
		for range degraded {
			h.onDegrade(ev.Stream, ev.Seq)
		}
	}
}

// publish offers ev to the stream's subscriptions and returns how many it degraded.
func (h *Hub) publish(ev ledger.Event) (degraded int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return 0
	}
	for s := range h.subs[ev.Stream] {
		select {
		case s.events <- ev:
		default:
			// The buffer is full, so the consumer has stopped keeping up. Degrade it
			// instead of waiting: Publish runs on the ledger's writer goroutine.
			h.removeLocked(s)
			s.shutLocked(true)
			degraded++
		}
	}
	return degraded
}

// Done implements Broker.
func (h *Hub) Done() <-chan struct{} { return h.done }

// Close implements Broker.
func (h *Hub) Close() {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return
	}
	h.closed = true
	close(h.done)
	for _, set := range h.subs {
		for s := range set {
			s.shutLocked(false)
		}
	}
	h.subs = nil
}

// removeLocked drops s from its stream's set. The caller holds h.mu.
func (h *Hub) removeLocked(s *subscription) {
	set := h.subs[s.stream]
	delete(set, s)
	if len(set) == 0 {
		delete(h.subs, s.stream)
	}
}

type subscription struct {
	hub      *Hub
	stream   string
	events   chan ledger.Event
	degraded atomic.Bool
	closed   bool // guarded by hub.mu
}

func (s *subscription) Events() <-chan ledger.Event { return s.events }

func (s *subscription) Degraded() bool { return s.degraded.Load() }

func (s *subscription) Close() {
	s.hub.mu.Lock()
	defer s.hub.mu.Unlock()
	if s.closed {
		return
	}
	s.hub.removeLocked(s)
	s.shutLocked(false)
}

// shutLocked closes the events channel once. The degraded flag is stored first, so a
// reader that sees the channel closed also sees the flag. The caller holds hub.mu.
func (s *subscription) shutLocked(degraded bool) {
	if s.closed {
		return
	}
	s.closed = true
	if degraded {
		s.degraded.Store(true)
	}
	close(s.events)
}
