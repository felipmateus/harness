package cli

import (
	"io"
	"strings"
	"testing"
)

func TestSSEReaderFrames(t *testing.T) {
	in := ": keepalive\n\n" +
		"id: 7\nevent: daemon.started\ndata: {\"a\":1}\n\n" +
		"event: shutdown\r\ndata: one\r\ndata: two\r\n\r\n" +
		"\n\n" + // blank lines between frames are nothing
		"retry: 100\n\n" + // an ignored field does not make a frame
		"id: 8\ndata:no-space\n\n" +
		"id: 9\nevent: unfinished\n"
	r := newSSEReader(strings.NewReader(in))

	f, err := r.next()
	if err != nil || !f.hasID || f.id != "7" || f.event != "daemon.started" || string(f.data) != `{"a":1}` {
		t.Fatalf("frame 1: %+v %v", f, err)
	}
	f, err = r.next()
	if err != nil || f.hasID || f.event != "shutdown" || string(f.data) != "one\ntwo" {
		t.Fatalf("frame 2: %+v %v", f, err)
	}
	f, err = r.next()
	if err != nil || f.id != "8" || string(f.data) != "no-space" {
		t.Fatalf("frame 3: %+v %v", f, err)
	}
	if f, err = r.next(); err != io.EOF {
		t.Fatalf("an unfinished frame must not be delivered: %+v %v", f, err)
	}
}

func TestSSEReaderLineLimit(t *testing.T) {
	r := newSSEReader(strings.NewReader("data: " + strings.Repeat("y", maxSSELine) + "\n\n"))
	if _, err := r.next(); err != errSSELineTooLong {
		t.Fatalf("a line of exactly 16 MiB plus the field name: error %v, want the limit", err)
	}

	ok := newSSEReader(strings.NewReader("data: " + strings.Repeat("y", 3<<20) + "\n\n"))
	if f, err := ok.next(); err != nil || len(f.data) != 3<<20 {
		t.Fatalf("a 3 MiB line: %d bytes, %v", len(f.data), err)
	}
}

func TestReadTail(t *testing.T) {
	e := newEnv(t)
	if got := readTail(e.home.DaemonOutPath(), 0, 20); len(got) != 0 || got == nil {
		t.Errorf("missing file: %#v, want an empty, non-nil slice", got)
	}
	writeDaemonOut(t, e.home, "a\nb\n")
	offset := int64(len("a\nb\n"))
	if got := readTail(e.home.DaemonOutPath(), offset, 20); len(got) != 0 {
		t.Errorf("nothing after the offset: %#v", got)
	}
	writeDaemonOut(t, e.home, "c\nd\ne without newline")
	got := readTail(e.home.DaemonOutPath(), offset, 2)
	if strings.Join(got, "|") != "d|e without newline" {
		t.Errorf("last two lines: %#v", got)
	}
	if got := readTail(e.home.DaemonOutPath(), 0, 20); strings.Join(got, "|") != "a|b|c|d|e without newline" {
		t.Errorf("from the start: %#v", got)
	}
}

func TestReadTailBoundsWhatItReads(t *testing.T) {
	e := newEnv(t)
	writeDaemonOut(t, e.home, strings.Repeat("x", maxTailRead*2)+"\nlast\n")
	got := readTail(e.home.DaemonOutPath(), 0, 20)
	// The head of the read is cut mid-line, so that partial line is dropped.
	if len(got) != 1 || got[0] != "last" {
		t.Errorf("tail of a huge line: %d lines, first %.20q", len(got), got[0])
	}
}
