package api

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"runtime/debug"
	"time"

	"github.com/felipmateus/harness/internal/admission"
	"github.com/felipmateus/harness/internal/api/contract"
	"github.com/felipmateus/harness/internal/redact"
)

// trackingWriter records the status and whether the response has started. Every wrapper
// of a ResponseWriter in this package implements Unwrap, so http.NewResponseController
// reaches the real connection's Flush and SetWriteDeadline through the whole chain.
type trackingWriter struct {
	http.ResponseWriter
	status  int
	started bool
}

func (w *trackingWriter) WriteHeader(code int) {
	// A 1xx response is informational; the final status comes later.
	if code >= http.StatusOK || code == http.StatusSwitchingProtocols {
		if !w.started {
			w.status = code
			w.started = true
		}
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *trackingWriter) Write(b []byte) (int, error) {
	if !w.started {
		w.status = http.StatusOK
		w.started = true
	}
	return w.ResponseWriter.Write(b)
}

// Unwrap lets http.ResponseController reach the wrapped writer.
func (w *trackingWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// recoverPanics is the outermost layer: a panic in any handler becomes a 500 with the
// generic body. The panic value and the stack go to the log only, redacted. If the
// response had already started, the connection is aborted instead, so a client never
// mistakes a truncated response for a complete one.
func (s *Server) recoverPanics(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tw := &trackingWriter{ResponseWriter: w}
		defer func() {
			v := recover()
			if v == nil {
				return
			}
			if v == http.ErrAbortHandler {
				panic(v) // net/http's own signal to abort a response; not a fault
			}
			s.log.Error("panic recovered",
				slog.String("method", r.Method),
				slog.String("path", r.URL.Path),
				slog.String("panic", redact.String(fmt.Sprint(v))),
				slog.String("stack", redact.String(string(debug.Stack()))),
			)
			if tw.started {
				panic(http.ErrAbortHandler)
			}
			writeContractError(tw, contract.InternalError())
		}()
		next.ServeHTTP(tw, r)
	})
}

// admit is the admission layer and, for non-stream routes, the in-flight tracking: the
// release returned by Gate.Enter is held until the handler returns, so a graceful
// shutdown can wait for it. A stream route is admitted but released at once, because a
// stream stays open until the daemon closes it. Exempt routes (status, shutdown) never
// touch the gate.
func (s *Server) admit(rt route, next http.Handler) http.Handler {
	if rt.exempt {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		release, err := s.gate.Enter()
		if err != nil {
			switch {
			case errors.Is(err, admission.ErrNotReady):
				writeContractError(w, contract.NotReadyError())
			case errors.Is(err, admission.ErrDraining):
				writeContractError(w, contract.DrainingError())
			default:
				s.internalError(w, r, err)
			}
			return
		}
		if rt.stream {
			release()
		} else {
			defer release()
		}
		next.ServeHTTP(w, r)
	})
}

// logRequests writes the debug `request` record once a handler has finished.
func (s *Server) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		tw := &trackingWriter{ResponseWriter: w}
		next.ServeHTTP(tw, r)
		if !s.log.Enabled(r.Context(), slog.LevelDebug) {
			return
		}
		status := tw.status
		if status == 0 {
			status = http.StatusOK
		}
		s.log.LogAttrs(r.Context(), slog.LevelDebug, "request",
			slog.String("method", r.Method),
			slog.String("path", r.URL.Path),
			slog.Int("status", status),
			slog.Int64("duration_ms", time.Since(start).Milliseconds()),
		)
	})
}

// internalError logs err and answers 500 with the generic body. The text of err never
// reaches the client.
func (s *Server) internalError(w http.ResponseWriter, r *http.Request, err error) {
	s.log.Error("request failed",
		slog.String("method", r.Method),
		slog.String("path", r.URL.Path),
		slog.Any("err", err),
	)
	writeContractError(w, contract.InternalError())
}
