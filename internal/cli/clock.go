package cli

import (
	"context"
	"time"
)

// clock is the time the CLI's waits and deadlines run on. Production uses the wall clock;
// tests inject a virtual one, so a 45 s idle deadline or a 35 s stop wait costs no real time.
type clock interface {
	Now() time.Time
	// After returns a channel that receives once d has passed.
	After(d time.Duration) <-chan time.Time
	// AfterFunc calls f in its own goroutine once d has passed.
	AfterFunc(d time.Duration, f func()) timer
}

// timer is the part of *time.Timer the CLI uses.
type timer interface {
	Reset(d time.Duration) bool
	Stop() bool
}

type realClock struct{}

func (realClock) Now() time.Time                         { return time.Now() }
func (realClock) After(d time.Duration) <-chan time.Time { return time.After(d) }
func (realClock) AfterFunc(d time.Duration, f func()) timer {
	return time.AfterFunc(d, f)
}

// sleep waits d on c, or until ctx ends.
func sleep(ctx context.Context, c clock, d time.Duration) error {
	select {
	case <-c.After(d):
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
