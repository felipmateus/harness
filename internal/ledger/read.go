package ledger

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// eventColumns is the column list scanEvent expects.
const eventColumns = `pos, stream, seq, id, type, time, data`

type scanner interface{ Scan(dest ...any) error }

// scanEvent reads one row selected with eventColumns.
func scanEvent(s scanner) (Event, error) {
	var (
		ev    Event
		stamp string
		data  []byte
	)
	if err := s.Scan(&ev.Pos, &ev.Stream, &ev.Seq, &ev.ID, &ev.Type, &stamp, &data); err != nil {
		return Event{}, err
	}
	t, err := parseTime(stamp)
	if err != nil {
		return Event{}, err
	}
	ev.Time = Time{t}
	ev.Data = data
	return ev, nil
}

// wrapRead turns a failure caused by a closed reader pool into ErrClosed.
func (l *Ledger) wrapRead(op string, err error) error {
	if err == nil {
		return nil
	}
	if l.readable() != nil {
		return ErrClosed
	}
	return fmt.Errorf("ledger: %s: %w", op, err)
}

// Read returns events of stream with seq > after, ascending, at most limit, and at most
// PageByteBudget bytes of data. It asks for limit+1 rows and reads them one at a time, so
// a page full of large events is never held in memory beyond the budget. A page always
// holds at least one event when any exists. HasMore reports that a further event exists.
func (l *Ledger) Read(ctx context.Context, stream string, after int64, limit int) (Page, error) {
	return l.readBudget(ctx, stream, after, limit, PageByteBudget)
}

// readBudget is Read with an explicit byte budget, so the at-least-one-event rule can be
// exercised with events smaller than PageByteBudget.
func (l *Ledger) readBudget(ctx context.Context, stream string, after int64, limit, budget int) (Page, error) {
	if limit < 1 {
		return Page{}, fmt.Errorf("ledger: read: limit must be at least 1, got %d", limit)
	}
	if err := l.readable(); err != nil {
		return Page{}, err
	}
	rows, err := l.rdb.QueryContext(ctx,
		`SELECT `+eventColumns+` FROM events WHERE stream = ? AND seq > ? ORDER BY seq LIMIT ?`,
		stream, after, int64(limit)+1)
	if err != nil {
		return Page{}, l.wrapRead("read", err)
	}
	defer rows.Close()

	page := Page{Events: []Event{}}
	size := 0
	for rows.Next() {
		ev, err := scanEvent(rows)
		if err != nil {
			return Page{}, l.wrapRead("read", err)
		}
		overLimit := len(page.Events) == limit
		overBudget := len(page.Events) > 0 && size+len(ev.Data) > budget
		if overLimit || overBudget {
			page.HasMore = true
			break
		}
		page.Events = append(page.Events, ev)
		size += len(ev.Data)
	}
	if err := rows.Err(); err != nil {
		return Page{}, l.wrapRead("read", err)
	}
	return page, nil
}

// Head returns the highest seq of stream, or 0 when it has no events.
func (l *Ledger) Head(ctx context.Context, stream string) (int64, error) {
	if err := l.readable(); err != nil {
		return 0, err
	}
	var head int64
	err := l.rdb.QueryRowContext(ctx, `SELECT COALESCE(MAX(seq), 0) FROM events WHERE stream = ?`, stream).Scan(&head)
	if err != nil {
		return 0, l.wrapRead("head", err)
	}
	return head, nil
}

// Last returns the highest-seq event of stream; ok is false when the stream is empty.
func (l *Ledger) Last(ctx context.Context, stream string) (Event, bool, error) {
	if err := l.readable(); err != nil {
		return Event{}, false, err
	}
	ev, err := scanEvent(l.rdb.QueryRowContext(ctx,
		`SELECT `+eventColumns+` FROM events WHERE stream = ? ORDER BY seq DESC LIMIT 1`, stream))
	if errors.Is(err, sql.ErrNoRows) {
		return Event{}, false, nil
	}
	if err != nil {
		return Event{}, false, l.wrapRead("last", err)
	}
	return ev, true, nil
}

// Stats reports the schema version and how many streams and events the ledger holds.
func (l *Ledger) Stats(ctx context.Context) (Stats, error) {
	if err := l.readable(); err != nil {
		return Stats{}, err
	}
	var s Stats
	err := l.rdb.QueryRowContext(ctx, `SELECT COUNT(*), COUNT(DISTINCT stream) FROM events`).Scan(&s.Events, &s.Streams)
	if err != nil {
		return Stats{}, l.wrapRead("stats", err)
	}
	if s.SchemaVersion, err = schemaVersion(ctx, l.rdb); err != nil {
		return Stats{}, l.wrapRead("stats", err)
	}
	return s, nil
}
