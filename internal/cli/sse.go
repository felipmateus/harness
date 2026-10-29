package cli

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"sync/atomic"
	"time"
)

// maxSSELine bounds one line of an SSE stream. An event holds up to 1 MiB of data, which
// JSON escaping can double, so 16 MiB leaves a wide margin; the reader has no token limit
// of the bufio.Scanner kind below it.
const maxSSELine = 16 << 20

var errSSELineTooLong = errors.New("event stream line is longer than 16 MiB")

// sseFrame is one dispatched frame of a text/event-stream body.
type sseFrame struct {
	id    string // empty when the frame carries no id: only events carry one
	hasID bool
	event string
	data  []byte
}

// sseReader parses frames from a stream.
type sseReader struct {
	br *bufio.Reader
}

func newSSEReader(r io.Reader) *sseReader {
	return &sseReader{br: bufio.NewReaderSize(r, 64<<10)}
}

// next returns the next frame. Comment lines (keepalives) never produce a frame. A stream
// that ends inside a frame ends without it: io.EOF or the read error.
func (r *sseReader) next() (sseFrame, error) {
	var f sseFrame
	var data [][]byte
	seen := false
	for {
		line, err := r.readLine()
		if err != nil {
			return sseFrame{}, err
		}
		if len(line) == 0 {
			if !seen {
				continue
			}
			f.data = bytes.Join(data, []byte("\n"))
			return f, nil
		}
		if line[0] == ':' {
			continue // a comment, such as `: keepalive`
		}
		name, value, _ := bytes.Cut(line, []byte(":"))
		value = bytes.TrimPrefix(value, []byte(" "))
		switch string(name) {
		case "id":
			f.id, f.hasID, seen = string(value), true, true
		case "event":
			f.event, seen = string(value), true
		case "data":
			data, seen = append(data, append([]byte(nil), value...)), true
		}
	}
}

// readLine reads one line, up to maxSSELine bytes, without the line terminator.
func (r *sseReader) readLine() ([]byte, error) {
	var line []byte
	for {
		chunk, more, err := r.br.ReadLine()
		if err != nil {
			return nil, err
		}
		line = append(line, chunk...)
		if len(line) > maxSSELine {
			return nil, errSSELineTooLong
		}
		if !more {
			return line, nil
		}
	}
}

// idleReader resets a timer on every read that returns bytes, keepalives included. When
// the timer fires, onIdle runs; the follower uses it to cancel the request.
type idleReader struct {
	r     io.Reader
	timer timer
	idle  time.Duration
}

func (i *idleReader) Read(p []byte) (int, error) {
	n, err := i.r.Read(p)
	if n > 0 {
		i.timer.Reset(i.idle)
	}
	return n, err
}

// flag is a boolean set once, from any goroutine.
type flag struct{ v atomic.Bool }

func (f *flag) set()        { f.v.Store(true) }
func (f *flag) isSet() bool { return f.v.Load() }
