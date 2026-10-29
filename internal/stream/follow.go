// Package stream implements Follow, the transport-agnostic follow algorithm.
//
// It is a plain function, not a port (ADR-003): the SSE adapter frames its output today, and
// the loop engine will call it later to watch session streams. The package declares the two
// small interfaces it consumes (Reader, Subscriber) and imports only the ledger's and the
// broker's types.
package stream

import (
	"context"
	"errors"
	"fmt"

	"github.com/felipmateus/harness/internal/broker"
	"github.com/felipmateus/harness/internal/ledger"
)

// CatchUpPage is how many events one catch-up Read asks for.
const CatchUpPage = 500

// outBuffer is the capacity of the channel Follow returns. It lets the events that arrive
// in the same instant as a broker close, and the terminal item after them, land without
// waiting for a consumer that is momentarily busy writing its previous frame. It does not
// hide a consumer that has stopped: a full buffer is exactly what the stuck-consumer escape
// in send reacts to.
const outBuffer = 8

// Reader reads committed history. It is satisfied by *ledger.Ledger.
type Reader interface {
	Read(ctx context.Context, stream string, after int64, limit int) (ledger.Page, error)
}

// Subscriber hands out live subscriptions and reports the broker's close. It is satisfied
// by broker.Broker.
type Subscriber interface {
	Subscribe(stream string) broker.Subscription
	Done() <-chan struct{}
}

// ErrClosed is the terminal error when the broker closes (the daemon is shutting down).
var ErrClosed = errors.New("stream: closed")

// Item carries one event or, as the final item only, a terminal error: ErrClosed, or an
// error from the reader. A follow that ends because ctx ended has no terminal item.
type Item struct {
	Event ledger.Event
	Err   error
}

// Follow delivers every event of stream with seq > after, history first and then live, each
// exactly once and in seq order, until ctx is done or the broker closes.
//
// It subscribes before it reads history, so no event can commit unseen in between, and it
// deduplicates by sequence, so the overlap of history and live is invisible. A subscription
// that degrades is replaced: Follow resubscribes and catches up again from the ledger.
//
// Every send also watches ctx and the subscriber's Done, so Follow never blocks forever on a
// consumer that stopped reading. An event that is ready is handed over before a close is
// honoured, so a consumer that keeps reading sees the final events of a stream, then
// ErrClosed. The terminal ErrClosed is sent without blocking: a consumer that is not
// reading when the broker closes, and whose buffer is full, does not receive it. The
// channel is then closed without a terminal item. A consumer that sees the channel close
// while its own ctx is still live should treat that as ErrClosed.
//
// The channel is closed right after the terminal item (or without one if ctx ended first).
// A consumer that stops reading must cancel ctx, or the goroutine lives until the broker
// closes.
func Follow(ctx context.Context, r Reader, s Subscriber, stream string, after int64) <-chan Item {
	f := &follower{
		ctx:    ctx,
		r:      r,
		s:      s,
		stream: stream,
		cursor: max(after, 0), // the API rejects a negative cursor; the in-process caller gets 0
		out:    make(chan Item, outBuffer),
	}
	go f.run()
	return f.out
}

type follower struct {
	ctx    context.Context
	r      Reader
	s      Subscriber
	stream string
	cursor int64 // the last delivered seq; it never decreases
	out    chan Item
}

// run owns the output channel and closes it when the follow ends.
func (f *follower) run() {
	defer close(f.out)
	for {
		// Safety Invariant 7: subscribe before the first history read, on every
		// (re)subscription.
		sub := f.s.Subscribe(f.stream)
		resubscribe := f.catchUp() && f.live(sub)
		sub.Close()
		if !resubscribe {
			return
		}
	}
}

// catchUp delivers history after the cursor, page by page, until the ledger reports no
// more. It returns false when the follow is over.
func (f *follower) catchUp() bool {
	for {
		if !f.alive() {
			return false
		}
		page, err := f.r.Read(f.ctx, f.stream, f.cursor, CatchUpPage)
		if err != nil {
			// A read that fails because the broker closed meanwhile is a shutdown, not an
			// error worth reporting.
			if f.alive() {
				f.send(Item{Err: err})
			}
			return false
		}
		// The broker may have closed while the page was being read; do not deliver a page
		// to a follow that is already over.
		if !f.alive() {
			return false
		}
		if !f.deliver(page) {
			return false
		}
		// Stop on HasMore, never on a short page: a page is also cut by the byte budget.
		if !page.HasMore {
			return true
		}
	}
}

// deliver sends one page. It enforces Safety Invariant 6 against the reader too: history is
// gap-free, so anything else means a corrupt ledger and ends the follow loudly.
func (f *follower) deliver(page ledger.Page) bool {
	if page.HasMore && len(page.Events) == 0 {
		f.send(Item{Err: fmt.Errorf("stream %s: reader reported more events after seq %d but returned none", f.stream, f.cursor)})
		return false
	}
	for _, ev := range page.Events {
		if ev.Seq != f.cursor+1 {
			f.send(Item{Err: fmt.Errorf("stream %s: reader returned seq %d after seq %d", f.stream, ev.Seq, f.cursor)})
			return false
		}
		if !f.send(Item{Event: ev}) {
			return false
		}
		f.cursor = ev.Seq
	}
	return true
}

// live forwards the subscription until it ends. It returns true when the subscription
// degraded and the caller must resubscribe and catch up again.
func (f *follower) live(sub broker.Subscription) (resubscribe bool) {
	for {
		select {
		case ev, ok := <-sub.Events():
			if !ok {
				if sub.Degraded() {
					return true
				}
				f.send(Item{Err: ErrClosed}) // the broker closed
				return false
			}
			switch {
			case ev.Seq <= f.cursor:
				// A duplicate from the overlap between history and live.
			case ev.Seq == f.cursor+1:
				if !f.send(Item{Event: ev}) {
					return false
				}
				f.cursor = ev.Seq
			default:
				// A gap. A subscription never skips, so this should not occur; the ledger
				// holds the missing events.
				if !f.catchUp() {
					return false
				}
			}
		case <-f.ctx.Done():
			return false
		}
	}
}

// alive reports whether the follow may continue. A finished ctx ends it silently; a closed
// broker ends it with ErrClosed. ctx wins when both fired.
func (f *follower) alive() bool {
	select {
	case <-f.ctx.Done():
		return false
	default:
	}
	select {
	case <-f.s.Done():
		f.sendClosed()
		return false
	default:
		return true
	}
}

// send delivers item, or reports false when the follow is over (Safety Invariant 8).
// ctx wins over everything. A consumer that can take the item now gets it, even when the
// broker has just closed, so the final events are not lost to a race. Otherwise send waits
// for the consumer, but only until ctx ends or the broker closes.
func (f *follower) send(item Item) bool {
	if f.ctx.Err() != nil {
		return false
	}
	select {
	case f.out <- item:
		return true
	default:
	}
	select {
	case f.out <- item:
		return true
	case <-f.ctx.Done():
		return false
	case <-f.s.Done():
		f.sendClosed()
		return false
	}
}

// sendClosed makes a single non-blocking attempt to deliver the terminal ErrClosed.
func (f *follower) sendClosed() {
	select {
	case f.out <- Item{Err: ErrClosed}:
	default:
	}
}
