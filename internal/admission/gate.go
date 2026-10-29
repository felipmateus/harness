// Package admission is the daemon's request gate. It holds the lifecycle state a request
// is admitted under (starting, running, draining) and counts the in-flight non-stream
// requests that a graceful shutdown waits for. It imports no other internal package.
package admission

import (
	"context"
	"errors"
	"sync"
)

// State is the daemon lifecycle state as the gate sees it. The lifecycle also has an
// `exited` state, but no request is ever served in it, so the gate does not model it.
type State string

const (
	Starting State = "starting"
	Running  State = "running"
	Draining State = "draining"
)

// Entry errors. The API maps them to daemon_not_ready and daemon_draining.
var (
	ErrNotReady = errors.New("admission: daemon is starting")
	ErrDraining = errors.New("admission: daemon is stopping")
)

// Gate is the contract the API and the daemon share (Safety Invariant 14: the status and
// shutdown routes are never blocked, which the API implements by not calling Enter for
// them).
type Gate interface {
	State() State
	// Set moves the gate forward: starting to running or draining, running to draining.
	// A backward or repeated transition, or an unknown state, is ignored.
	Set(State)
	// Enter admits one request. It fails with ErrNotReady while starting and ErrDraining
	// while draining. On success the request counts as in flight until release is called;
	// release is idempotent.
	Enter() (release func(), err error)
	// Wait blocks until no admitted request is in flight, or until ctx ends.
	Wait(ctx context.Context) error
}

// rank orders the states; transitions only go up.
func rank(s State) int {
	switch s {
	case Starting:
		return 0
	case Running:
		return 1
	case Draining:
		return 2
	default:
		return -1
	}
}

// Lifecycle is the concrete Gate. The zero value is not usable; call New.
type Lifecycle struct {
	mu       sync.Mutex
	state    State
	inflight int
	// idle is open while at least one request is in flight and closed when the count
	// returns to zero. Wait blocks on the channel that is current when it is called.
	idle chan struct{}
}

var _ Gate = (*Lifecycle)(nil)

// New returns a gate in the starting state.
func New() *Lifecycle {
	return &Lifecycle{state: Starting}
}

// State returns the current state.
func (g *Lifecycle) State() State {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.state
}

// Set implements Gate.
func (g *Lifecycle) Set(next State) {
	if rank(next) < 0 {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if rank(next) > rank(g.state) {
		g.state = next
	}
}

// Enter implements Gate. The state check and the count change happen under one lock, so
// once Set(Draining) has returned, no request can enter and the count only goes down.
func (g *Lifecycle) Enter() (func(), error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	switch g.state {
	case Starting:
		return nil, ErrNotReady
	case Draining:
		return nil, ErrDraining
	}
	if g.inflight == 0 {
		g.idle = make(chan struct{})
	}
	g.inflight++
	var once sync.Once
	return func() { once.Do(g.leave) }, nil
}

func (g *Lifecycle) leave() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.inflight--
	if g.inflight == 0 {
		close(g.idle)
	}
}

// Wait implements Gate.
func (g *Lifecycle) Wait(ctx context.Context) error {
	g.mu.Lock()
	if g.inflight == 0 {
		g.mu.Unlock()
		return nil
	}
	idle := g.idle
	g.mu.Unlock()

	select {
	case <-idle:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
