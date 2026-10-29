package daemon

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"testing"
	"time"
)

// closedByPeer reports whether the far end of a pipe sees the connection closed within
// wait. A pipe is closed synchronously, so a short wait is enough to say "not closed".
func closedByPeer(t *testing.T, far net.Conn, wait time.Duration) bool {
	t.Helper()
	far.SetReadDeadline(time.Now().Add(wait))
	_, err := far.Read(make([]byte, 1))
	if errors.Is(err, io.EOF) {
		return true
	}
	if errors.Is(err, os.ErrDeadlineExceeded) {
		return false
	}
	t.Fatalf("read: %v", err)
	return false
}

// UT-139: the connections that have not started a request are the ones the server's shutdown
// closes before http.Server.Shutdown, which would wait for each until it is 5 s old. A
// connection that is serving a request is not touched (step 5 owns it, and the streams'
// shutdown frame depends on it), one accepted afterwards is closed at once, and the set the
// stream drain waits on is unchanged by any of it.
func TestConnTracker_ClosesTheConnectionsThatHaveNotStartedARequest(t *testing.T) {
	c := newConnTracker()
	silent, silentFar := net.Pipe()
	serving, servingFar := net.Pipe()
	defer silentFar.Close()
	defer servingFar.Close()

	c.track(silent, http.StateNew)
	c.track(serving, http.StateNew)
	c.track(serving, http.StateActive) // it sent a request: no longer unstarted
	short, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := c.waitIdle(short); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("waitIdle = %v, want it to wait for the connection that is serving", err)
	}

	if n := c.closeUnstarted(); n != 1 {
		t.Fatalf("closeUnstarted closed %d connections, want the 1 that never sent a request", n)
	}
	if !closedByPeer(t, silentFar, 2*time.Second) {
		t.Error("the silent connection is still open")
	}
	if closedByPeer(t, servingFar, 50*time.Millisecond) {
		t.Error("the connection that is serving a request was closed")
	}
	if n := c.closeUnstarted(); n != 0 {
		t.Errorf("a second call closed %d connections, want 0", n)
	}

	late, lateFar := net.Pipe()
	defer lateFar.Close()
	c.track(late, http.StateNew)
	if !closedByPeer(t, lateFar, 2*time.Second) {
		t.Error("a connection accepted after the shutdown began was not closed")
	}

	if n := c.closeActive(); n != 1 {
		t.Errorf("closeActive closed %d, want the 1 that is serving", n)
	}
	c.track(serving, http.StateClosed)
	if err := c.waitIdle(context.Background()); err != nil {
		t.Errorf("waitIdle = %v once the last connection closed", err)
	}
}

// A connection that starts its request before the shutdown is not unstarted any more, and
// one that closes on its own leaves no trace.
func TestConnTracker_ForgetsAConnectionOnceItHasStartedOrClosed(t *testing.T) {
	c := newConnTracker()
	started, startedFar := net.Pipe()
	gone, goneFar := net.Pipe()
	defer startedFar.Close()
	defer goneFar.Close()

	c.track(started, http.StateNew)
	c.track(started, http.StateActive)
	c.track(started, http.StateIdle)
	c.track(gone, http.StateNew)
	c.track(gone, http.StateClosed)

	if n := c.closeUnstarted(); n != 0 {
		t.Errorf("closeUnstarted closed %d connections, want 0: none was unstarted", n)
	}
	if closedByPeer(t, startedFar, 50*time.Millisecond) {
		t.Error("a connection that had served a request was closed")
	}
}
