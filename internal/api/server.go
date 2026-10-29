// Package api is the daemon's local HTTP API: path-level routing with JSON 404 and 405,
// strict query and body parsing, one error writer, the admission and in-flight
// middleware, and the SSE adapter that frames stream.Follow.
//
// The package declares the interfaces it consumes (ADR-003) and imports only the wire
// contract, stream, the ledger's types, admission and redact. The daemon wires the real
// implementations; it serves this handler on the Unix socket.
package api

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/felipmateus/harness/internal/admission"
	"github.com/felipmateus/harness/internal/api/contract"
	"github.com/felipmateus/harness/internal/ledger"
	"github.com/felipmateus/harness/internal/stream"
)

// Production timings of the SSE adapter (`_spec.md` timeouts).
const (
	// DefaultKeepaliveInterval is how long a stream may stay silent before a keepalive
	// comment is written.
	DefaultKeepaliveInterval = 15 * time.Second
	// DefaultWriteTimeout bounds each event and keepalive write.
	DefaultWriteTimeout = 10 * time.Second
	// finalFrameTimeout bounds the last frame (shutdown or error) of a stream.
	finalFrameTimeout = time.Second
)

// gate is the admission contract the API needs. admission.Lifecycle satisfies it.
type gate interface {
	State() admission.State
	Enter() (release func(), err error)
}

// statusSource supplies what only the daemon knows. The gate state is not part of it: the
// handler reads it from the gate, so the report and the admission decision always agree.
type statusSource interface {
	// Status returns the daemon facts behind GET /api/status, with State left empty.
	Status(ctx context.Context) (contract.Status, error)
	// ShutdownInfo returns the PID, uptime and shutdown grace for the shutdown answer.
	// It must not touch the ledger or block: the shutdown route answers in every state.
	ShutdownInfo() contract.ShutdownInfo
}

// eventReader reads committed history. *ledger.Ledger satisfies it.
type eventReader interface {
	Read(ctx context.Context, stream string, after int64, limit int) (ledger.Page, error)
	Head(ctx context.Context, stream string) (int64, error)
}

// doctorRunner runs the daemon-mode health checks. The doctor package (not imported here)
// satisfies it.
type doctorRunner interface {
	Run(ctx context.Context) (contract.Doctor, error)
}

// FollowFunc starts a follow of one stream. The daemon passes a closure over stream.Follow.
// The SSE adapter owns the channel's consumer and cancels ctx when the request ends.
type FollowFunc func(ctx context.Context, stream string, after int64) <-chan stream.Item

// Options are the dependencies of a Server.
type Options struct {
	Gate   gate
	Status statusSource
	Events eventReader
	// Doctor may be nil until the doctor package is wired; the route then answers 500.
	Doctor doctorRunner
	Follow FollowFunc
	// RequestStop asks the daemon to stop with reason "request". While the daemon is still
	// starting it cancels the boot. The Server calls it at most once, and only while the
	// gate is not draining. It must return promptly: the daemon runs the shutdown itself.
	RequestStop func()
	// StreamContext is the server-wide stream context: when it ends, every open event
	// stream ends too. The daemon cancels it from http.Server.RegisterOnShutdown, as a
	// backstop to the broker close. Nil means a context that is never cancelled.
	StreamContext context.Context
	// Logger receives request, stream and panic records. Nil discards them.
	Logger *slog.Logger
	// KeepaliveInterval and WriteTimeout default to the production values when zero.
	KeepaliveInterval time.Duration
	WriteTimeout      time.Duration
}

// Server is the API handler.
type Server struct {
	gate        gate
	status      statusSource
	events      eventReader
	doctor      doctorRunner
	follow      FollowFunc
	requestStop func()
	streamCtx   context.Context
	log         *slog.Logger
	keepalive   time.Duration
	writeTO     time.Duration

	stopOnce sync.Once
	handler  http.Handler
}

var _ http.Handler = (*Server)(nil)

// New validates opts and builds the handler.
func New(opts Options) (*Server, error) {
	switch {
	case opts.Gate == nil:
		return nil, errors.New("api: Options.Gate is required")
	case opts.Status == nil:
		return nil, errors.New("api: Options.Status is required")
	case opts.Events == nil:
		return nil, errors.New("api: Options.Events is required")
	case opts.Follow == nil:
		return nil, errors.New("api: Options.Follow is required")
	case opts.RequestStop == nil:
		return nil, errors.New("api: Options.RequestStop is required")
	case opts.KeepaliveInterval < 0 || opts.WriteTimeout < 0:
		return nil, errors.New("api: timings must not be negative")
	}
	s := &Server{
		gate:        opts.Gate,
		status:      opts.Status,
		events:      opts.Events,
		doctor:      opts.Doctor,
		follow:      opts.Follow,
		requestStop: opts.RequestStop,
		streamCtx:   opts.StreamContext,
		log:         opts.Logger,
		keepalive:   opts.KeepaliveInterval,
		writeTO:     opts.WriteTimeout,
	}
	if s.streamCtx == nil {
		s.streamCtx = context.Background()
	}
	if s.log == nil {
		s.log = slog.New(slog.DiscardHandler)
	}
	if s.keepalive == 0 {
		s.keepalive = DefaultKeepaliveInterval
	}
	if s.writeTO == 0 {
		s.writeTO = DefaultWriteTimeout
	}
	s.handler = s.buildHandler(s.routes())
	return s, nil
}

// ServeHTTP implements http.Handler.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.handler.ServeHTTP(w, r)
}
