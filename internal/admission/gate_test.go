package admission

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/felipmateus/harness/internal/testutil/leakcheck"
)

func TestNewGateStartsStarting(t *testing.T) {
	if got := New().State(); got != Starting {
		t.Fatalf("state = %q, want %q", got, Starting)
	}
}

// Transitions only go forward; starting may jump straight to draining (a shutdown request
// while the daemon is still booting).
func TestSetIsForwardOnly(t *testing.T) {
	tests := []struct {
		name  string
		start State
		set   State
		want  State
	}{
		{"starting to running", Starting, Running, Running},
		{"starting to draining", Starting, Draining, Draining},
		{"running to draining", Running, Draining, Draining},
		{"running to starting is ignored", Running, Starting, Running},
		{"draining to running is ignored", Draining, Running, Draining},
		{"draining to starting is ignored", Draining, Starting, Draining},
		{"repeating a state is a no-op", Running, Running, Running},
		{"an unknown state is ignored", Running, State("exited"), Running},
		{"the empty state is ignored", Starting, State(""), Starting},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := New()
			g.Set(tt.start)
			g.Set(tt.set)
			if got := g.State(); got != tt.want {
				t.Fatalf("after Set(%q) then Set(%q): state = %q, want %q", tt.start, tt.set, got, tt.want)
			}
		})
	}
}

func TestEnterByState(t *testing.T) {
	g := New()
	if release, err := g.Enter(); !errors.Is(err, ErrNotReady) || release != nil {
		t.Fatalf("Enter while starting = (%v, %v), want ErrNotReady and no release", release != nil, err)
	}

	g.Set(Running)
	release, err := g.Enter()
	if err != nil || release == nil {
		t.Fatalf("Enter while running = (%v, %v), want a release func", release != nil, err)
	}
	release()

	g.Set(Draining)
	if release, err := g.Enter(); !errors.Is(err, ErrDraining) || release != nil {
		t.Fatalf("Enter while draining = (%v, %v), want ErrDraining and no release", release != nil, err)
	}
}

func TestWaitReturnsAtOnceWhenIdle(t *testing.T) {
	g := New()
	g.Set(Running)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := g.Wait(ctx); err != nil {
		t.Fatalf("Wait on an idle gate: %v", err)
	}
}

func TestWaitBlocksUntilTheLastReleaseAndOnlyThen(t *testing.T) {
	leakcheck.Check(t)
	g := New()
	g.Set(Running)
	r1, _ := g.Enter()
	r2, _ := g.Enter()
	g.Set(Draining)

	done := make(chan error, 1)
	go func() { done <- g.Wait(context.Background()) }()

	r1()
	select {
	case err := <-done:
		t.Fatalf("Wait returned (%v) with one request still in flight", err)
	case <-time.After(50 * time.Millisecond):
	}

	r2()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Wait: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Wait did not return after the last release")
	}
}

func TestWaitHonoursContext(t *testing.T) {
	leakcheck.Check(t)
	g := New()
	g.Set(Running)
	release, _ := g.Enter()
	defer release()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if err := g.Wait(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Wait = %v, want context.DeadlineExceeded", err)
	}
}

// A double release must not count a request out twice: the second call would make the
// count go negative and let Wait return while another request is still running.
func TestReleaseIsIdempotent(t *testing.T) {
	g := New()
	g.Set(Running)
	r1, _ := g.Enter()
	r2, _ := g.Enter()
	r1()
	r1()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if err := g.Wait(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Wait = %v with one request still in flight, want a timeout", err)
	}
	r2()
	if err := g.Wait(context.Background()); err != nil {
		t.Fatalf("Wait after the last release: %v", err)
	}
}

// Once Set(Draining) has returned, no request enters, so Wait only ever sees the count go
// down. Run under -race with many goroutines entering while the gate drains.
func TestNoEntryAfterDrainingBegins(t *testing.T) {
	leakcheck.Check(t)
	g := New()
	g.Set(Running)

	var wg sync.WaitGroup
	admitted := make(chan func(), 1000)
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 100 {
				if release, err := g.Enter(); err == nil {
					admitted <- release
				}
			}
		}()
	}
	g.Set(Draining)
	wg.Wait()
	close(admitted)

	// Everything admitted was admitted before draining began; releasing it all lets Wait return.
	for release := range admitted {
		release()
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := g.Wait(ctx); err != nil {
		t.Fatalf("Wait after releasing everything: %v", err)
	}
	if _, err := g.Enter(); !errors.Is(err, ErrDraining) {
		t.Fatalf("Enter after draining = %v, want ErrDraining", err)
	}
}

// A request that enters, leaves and is followed by another one still drains correctly: the
// idle channel is replaced each time the count rises from zero.
func TestWaitAcrossRepeatedIdlePeriods(t *testing.T) {
	g := New()
	g.Set(Running)
	for range 3 {
		release, err := g.Enter()
		if err != nil {
			t.Fatal(err)
		}
		release()
		if err := g.Wait(context.Background()); err != nil {
			t.Fatalf("Wait: %v", err)
		}
	}
}
