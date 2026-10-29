package daemon

import (
	"context"
	"net"
	"net/http"
	"sync"
	"time"
)

// streamFinalFrameGrace is how long shutdown step 5 waits for the event streams to write
// their final frame after the broker closes. The SSE adapter writes it under a 1 s
// deadline, so a healthy stream is done long before; a stream still open after this is
// blocked on a client that stopped reading, and is cut.
const streamFinalFrameGrace = time.Second

// connTracker follows the HTTP connections, through http.Server.ConnState, for two things.
//
// The first is knowing when the event streams have said goodbye: http.Server.Shutdown
// cancels their contexts the moment it starts, and a stream that is cancelled first never
// writes its shutdown frame. That is the set of connections that are serving a request
// (active).
//
// The second is the connections that have not started one (unstarted): accepted, and silent
// so far, or holding only the start of a request. http.Server.Shutdown treats such a
// connection as busy until it is 5 s old (net/http issue 22682: a StateNew connection counts
// as idle only after five seconds), so one local process that connected and said nothing
// would hold the shutdown for that long, and a shutdown_grace shorter than that would run
// out at step 6. No request is in flight on them, so closing them loses nothing.
type connTracker struct {
	mu        sync.Mutex
	active    map[net.Conn]struct{}
	unstarted map[net.Conn]struct{}
	// refusing is set once the server is shutting down: from then on a connection is closed
	// as soon as it is accepted.
	refusing bool
	changed  chan struct{} // closed and replaced whenever the active set changes
}

func newConnTracker() *connTracker {
	return &connTracker{
		active:    make(map[net.Conn]struct{}),
		unstarted: make(map[net.Conn]struct{}),
		changed:   make(chan struct{}),
	}
}

// track is the http.Server.ConnState callback.
func (c *connTracker) track(conn net.Conn, state http.ConnState) {
	c.mu.Lock()
	switch state {
	case http.StateNew:
		if c.refusing {
			c.mu.Unlock()
			conn.Close()
			return
		}
		c.unstarted[conn] = struct{}{}
		c.mu.Unlock()
		return
	case http.StateActive:
		delete(c.unstarted, conn)
		c.active[conn] = struct{}{}
	case http.StateIdle, http.StateClosed, http.StateHijacked:
		delete(c.unstarted, conn)
		if _, ok := c.active[conn]; !ok {
			c.mu.Unlock()
			return
		}
		delete(c.active, conn)
	default:
		c.mu.Unlock()
		return
	}
	close(c.changed)
	c.changed = make(chan struct{})
	c.mu.Unlock()
}

// waitIdle blocks until no connection is serving a request, or until ctx ends.
func (c *connTracker) waitIdle(ctx context.Context) error {
	for {
		c.mu.Lock()
		busy, changed := len(c.active), c.changed
		c.mu.Unlock()
		if busy == 0 {
			return nil
		}
		select {
		case <-changed:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// closeActive closes every connection that is still serving a request and returns how many
// it closed. Closing a connection unblocks a handler that is stuck writing to it.
func (c *connTracker) closeActive() int {
	c.mu.Lock()
	conns := make([]net.Conn, 0, len(c.active))
	for conn := range c.active {
		conns = append(conns, conn)
	}
	c.mu.Unlock()
	for _, conn := range conns {
		conn.Close()
	}
	return len(conns)
}

// closeUnstarted closes every connection that has not started a request, and every one that
// is accepted from now on, and returns how many it closed now. The server's shutdown calls it
// before http.Server.Shutdown, which would otherwise wait for them (see connTracker).
func (c *connTracker) closeUnstarted() int {
	c.mu.Lock()
	c.refusing = true
	conns := make([]net.Conn, 0, len(c.unstarted))
	for conn := range c.unstarted {
		conns = append(conns, conn)
	}
	clear(c.unstarted)
	c.mu.Unlock()
	for _, conn := range conns {
		conn.Close()
	}
	return len(conns)
}
