// Package logging builds the daemon logger: slog JSON written to a rotated file, every
// message and string attribute passed through redact, and an optional stderr mirror that
// can be switched off at runtime (detached daemons stop mirroring once running) and that
// switches itself off at its first write error.
package logging

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sync/atomic"

	"gopkg.in/natefinch/lumberjack.v2"

	"github.com/felipmateus/harness/internal/redact"
)

// Rotation policy for harness.log.
const (
	maxSizeMB  = 10
	maxBackups = 5
	maxAgeDays = 30
)

// Options configures New.
type Options struct {
	Path   string     // log file; created 0600
	Level  slog.Level // minimum level
	Mirror io.Writer  // optional stderr mirror; nil disables mirroring; off for good after a write error
}

// Logger is the daemon logger. Every record carries the process PID.
type Logger struct {
	*slog.Logger
	file   *lumberjack.Logger
	mirror *switchWriter
}

// New opens the log file and returns the logger.
func New(opts Options) (*Logger, error) {
	file := &lumberjack.Logger{
		Filename:   opts.Path,
		MaxSize:    maxSizeMB,
		MaxBackups: maxBackups,
		MaxAge:     maxAgeDays,
	}
	// Open now so a log path problem fails boot instead of the first record.
	if _, err := file.Write(nil); err != nil {
		return nil, fmt.Errorf("open log %s: %w", opts.Path, err)
	}
	mirror := &switchWriter{w: opts.Mirror}
	mirror.on.Store(opts.Mirror != nil)
	out := io.MultiWriter(file, mirror)
	h := &redactingHandler{inner: slog.NewJSONHandler(out, &slog.HandlerOptions{Level: opts.Level, ReplaceAttr: utcTime})}
	return &Logger{
		Logger: slog.New(h).With("pid", os.Getpid()),
		file:   file,
		mirror: mirror,
	}, nil
}

// SetMirror turns the stderr mirror on or off.
func (l *Logger) SetMirror(on bool) {
	l.mirror.on.Store(on && l.mirror.w != nil)
}

// Close closes the log file.
func (l *Logger) Close() error { return l.file.Close() }

// timeLayout is the form of every time the daemon writes, the log included: UTC with exactly
// three fractional digits. It matches the ledger's, so a log record and an event line up by
// string comparison. (logging imports only redact, so the layout is repeated here.)
const timeLayout = "2006-01-02T15:04:05.000Z"

// utcTime rewrites the record's own `time` field. slog's default is local time with a zone
// offset and no trailing zeros, which is neither fixed width nor comparable with event times.
func utcTime(groups []string, a slog.Attr) slog.Attr {
	if len(groups) == 0 && a.Key == slog.TimeKey && a.Value.Kind() == slog.KindTime {
		return slog.String(slog.TimeKey, a.Value.Time().UTC().Format(timeLayout))
	}
	return a
}

// ParseLevel maps a config level name to a slog level; unknown names mean info.
func ParseLevel(name string) slog.Level {
	switch name {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

type switchWriter struct {
	w  io.Writer
	on atomic.Bool
}

// Write copies p to the mirror while it is on. A broken mirror never fails the log write: the
// file log is the record, the mirror a courtesy copy. The first error switches the mirror
// off, because a pipe whose reader has gone (EPIPE) or a closed descriptor does not come
// back, and every further record would only repeat the failed write. Without this, a daemon
// with a dead stderr pipe pays one failed syscall per record for the rest of its life.
func (s *switchWriter) Write(p []byte) (int, error) {
	if s.on.Load() {
		if _, err := s.w.Write(p); err != nil {
			s.on.Store(false)
		}
	}
	return len(p), nil
}

// redactingHandler redacts the message and every string-valued attribute, including
// errors and values inside groups.
type redactingHandler struct {
	inner slog.Handler
}

func (h *redactingHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.inner.Enabled(ctx, level)
}

func (h *redactingHandler) Handle(ctx context.Context, r slog.Record) error {
	clean := slog.NewRecord(r.Time, r.Level, redact.String(r.Message), r.PC)
	r.Attrs(func(a slog.Attr) bool {
		clean.AddAttrs(redactAttr(a))
		return true
	})
	return h.inner.Handle(ctx, clean)
}

func (h *redactingHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	clean := make([]slog.Attr, len(attrs))
	for i, a := range attrs {
		clean[i] = redactAttr(a)
	}
	return &redactingHandler{inner: h.inner.WithAttrs(clean)}
}

func (h *redactingHandler) WithGroup(name string) slog.Handler {
	return &redactingHandler{inner: h.inner.WithGroup(name)}
}

func redactAttr(a slog.Attr) slog.Attr {
	v := a.Value.Resolve()
	switch v.Kind() {
	case slog.KindString:
		return slog.String(a.Key, redact.String(v.String()))
	case slog.KindGroup:
		group := v.Group()
		clean := make([]any, len(group))
		for i, g := range group {
			clean[i] = redactAttr(g)
		}
		return slog.Group(a.Key, clean...)
	case slog.KindAny:
		if err, ok := v.Any().(error); ok {
			return slog.String(a.Key, redact.String(err.Error()))
		}
		return slog.String(a.Key, redact.String(fmt.Sprint(v.Any())))
	default:
		return slog.Attr{Key: a.Key, Value: v}
	}
}
