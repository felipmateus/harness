package ledger

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// request is one validated append waiting for the writer.
type request struct {
	ev  NewEvent
	res chan result // capacity 1: the writer never blocks delivering the outcome
}

type result struct {
	ev      Event
	created bool
	err     error
}

// Append validates e, hands it to the writer and waits for the outcome.
//
// Every return path carries the event ID, so a caller whose context ended can retry with
// the same ID: the retry returns the committed event with created=false instead of
// appending twice. An identical retry (same ID, stream, type and data) never inserts a
// second row; a different payload under the same ID is ErrIDConflict.
func (l *Ledger) Append(ctx context.Context, e NewEvent) (Event, bool, error) {
	ne, err := validate(e)
	if err != nil {
		return Event{ID: ne.ID}, false, err
	}
	fail := func(err error) (Event, bool, error) { return Event{ID: ne.ID}, false, err }

	if err := ctx.Err(); err != nil {
		return fail(err)
	}
	req := &request{ev: ne, res: make(chan result, 1)}
	if err := l.enqueue(ctx, req); err != nil {
		return fail(err)
	}

	select {
	case r := <-req.res:
		return r.ev, r.created, r.err
	case <-ctx.Done():
		// The writer owns the request now and finishes it on its own context. Prefer a
		// result that is already there; otherwise report ctx's error, with the ID.
		select {
		case r := <-req.res:
			return r.ev, r.created, r.err
		default:
			return fail(ctx.Err())
		}
	}
}

// enqueue offers req to the writer. It holds the read lock for the send, so Close can flip
// closed and close the queue only once no sender is mid-send; Close wakes a blocked sender
// through l.closing first.
func (l *Ledger) enqueue(ctx context.Context, req *request) error {
	l.mu.RLock()
	defer l.mu.RUnlock()
	if l.closed {
		return ErrClosed
	}
	select {
	case l.queue <- req:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-l.closing:
		return ErrClosed
	}
}

// run is the single writer goroutine. It outlives every caller's context and returns when
// Close has closed the queue and the queue is empty.
func (l *Ledger) run() {
	defer close(l.done)
	for {
		if l.hooks.beforeDequeue != nil {
			l.hooks.beforeDequeue()
		}
		req, ok := <-l.queue
		if !ok {
			return
		}
		req.res <- l.commit(req.ev)
	}
}

// commit performs one append in one transaction under the ledger's own lifetime context:
// check the ID, assign seq and time, insert, COMMIT, then OnCommit.
func (l *Ledger) commit(ne NewEvent) result {
	ctx := l.life
	fail := func(err error) result {
		if ctx.Err() != nil { // forced Close aborted the work
			err = ErrClosed
		}
		return result{ev: Event{ID: ne.ID}, err: err}
	}
	if ctx.Err() != nil {
		return fail(nil)
	}

	tx, err := l.wdb.BeginTx(ctx, nil) // BEGIN IMMEDIATE (_txlock)
	if err != nil {
		return fail(fmt.Errorf("ledger: begin: %w", err))
	}
	defer tx.Rollback() //nolint:errcheck // a no-op after Commit

	// Idempotency: the ID decides first.
	stored, found, err := eventByID(ctx, tx, ne.ID)
	if err != nil {
		return fail(fmt.Errorf("ledger: lookup id: %w", err))
	}
	if found {
		if stored.Stream == ne.Stream && stored.Type == ne.Type && bytes.Equal(stored.Data, ne.Data) {
			return result{ev: stored, created: false}
		}
		return result{ev: Event{ID: ne.ID}, err: ErrIDConflict}
	}

	var seq int64
	if err := tx.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(seq), 0) + 1 FROM events WHERE stream = ?`, ne.Stream).Scan(&seq); err != nil {
		return fail(fmt.Errorf("ledger: next seq: %w", err))
	}

	// Times never go backwards: a clock that stepped back is held at the last value.
	at := l.clock().UTC().Truncate(time.Millisecond)
	if at.Before(l.last) {
		at = l.last
	}
	stamp := formatTime(at)

	var pos int64
	if err := tx.QueryRowContext(ctx,
		`INSERT INTO events (stream, seq, id, type, time, data) VALUES (?, ?, ?, ?, ?, ?) RETURNING pos`,
		ne.Stream, seq, ne.ID, ne.Type, stamp, []byte(ne.Data)).Scan(&pos); err != nil {
		return fail(fmt.Errorf("ledger: insert: %w", err))
	}

	if l.hooks.inTx != nil {
		l.hooks.inTx(ne.ID)
	}

	if err := tx.Commit(); err != nil {
		return fail(fmt.Errorf("ledger: commit: %w", err))
	}
	l.last = at

	ev := Event{Pos: pos, Stream: ne.Stream, Seq: seq, ID: ne.ID, Type: ne.Type, Time: Time{at}, Data: ne.Data}
	if l.onCommit != nil {
		l.onCommit(ev)
	}
	return result{ev: ev, created: true}
}

// eventByID returns the event stored under id, if any.
func eventByID(ctx context.Context, q rowQuerier, id string) (Event, bool, error) {
	ev, err := scanEvent(q.QueryRowContext(ctx, `SELECT `+eventColumns+` FROM events WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Event{}, false, nil
	}
	if err != nil {
		return Event{}, false, err
	}
	return ev, true, nil
}
