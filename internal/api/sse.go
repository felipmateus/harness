package api

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/felipmateus/harness/internal/api/contract"
	"github.com/felipmateus/harness/internal/stream"
)

// Frames that carry no event.
var (
	keepaliveFrame = []byte(": keepalive\n\n")
	shutdownFrame  = []byte("event: shutdown\ndata: {\"reason\":\"daemon stopped\"}\n\n")
)

// handleEventStream answers GET /api/streams/{stream}/events/stream. Everything that can be
// rejected is rejected before the first byte: a bad stream ID, a bad `after`, and a bad
// Last-Event-ID, which overrides `after` when present.
func (s *Server) handleEventStream(w http.ResponseWriter, r *http.Request, q url.Values) {
	streamID := r.PathValue("stream")
	if err := contract.ValidateStream(streamID); err != nil {
		writeContractError(w, err)
		return
	}
	after, err := contract.ParseAfter(q.Get("after"))
	if err != nil {
		writeContractError(w, err)
		return
	}
	if lastID := r.Header.Get("Last-Event-ID"); lastID != "" {
		if after, err = contract.ParseAfter(lastID); err != nil {
			writeContractError(w, err)
			return
		}
	}
	s.serveEventStream(w, r, streamID, after)
}

// serveEventStream is the SSE adapter. It frames what the follow function delivers and
// nothing more: ordering, dedupe and catch-up belong to stream.Follow.
//
// The follow runs under the request context merged with the server-wide stream context,
// and is cancelled when this function returns, so no follow outlives its request. Every
// write has a deadline (Safety Invariant 8): a client that stops reading cannot hold the
// handler, and through it a graceful shutdown, for longer than the write timeout.
func (s *Server) serveEventStream(w http.ResponseWriter, r *http.Request, streamID string, after int64) {
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	defer context.AfterFunc(s.streamCtx, cancel)()

	items := s.follow(ctx, streamID, after)
	cursor := after // the last sequence written; only for the log records

	rc := http.NewResponseController(w)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	// Flush the headers now, so the client knows the stream is open before any event.
	if err := s.send(rc, w, nil, s.writeTO); err != nil {
		s.logWriteFailed(ctx, streamID, cursor, err)
		return
	}

	timer := time.NewTimer(s.keepalive)
	defer timer.Stop()

	for {
		select {
		case item, ok := <-items:
			if !ok {
				if ctx.Err() != nil {
					return // the request or the server ended it; nobody is waiting for a frame
				}
				// A follow that closes while its context is live ended because the broker
				// closed and its terminal item could not be delivered: it is a shutdown.
				s.finish(ctx, rc, w, streamID, cursor, shutdownFrame)
				return
			}
			if item.Err != nil {
				if errors.Is(item.Err, stream.ErrClosed) {
					s.finish(ctx, rc, w, streamID, cursor, shutdownFrame)
					return
				}
				s.failStream(ctx, rc, w, streamID, cursor, item.Err)
				return
			}
			frame, err := eventFrame(toContractEvent(item.Event))
			if err != nil {
				s.failStream(ctx, rc, w, streamID, cursor, err)
				return
			}
			if err := s.send(rc, w, frame, s.writeTO); err != nil {
				s.logWriteFailed(ctx, streamID, cursor, err)
				return
			}
			cursor = item.Event.Seq
			timer.Reset(s.keepalive)

		case <-timer.C:
			if err := s.send(rc, w, keepaliveFrame, s.writeTO); err != nil {
				s.logWriteFailed(ctx, streamID, cursor, err)
				return
			}
			timer.Reset(s.keepalive)

		case <-ctx.Done():
			return
		}
	}
}

// failStream logs err and ends the stream with the generic error frame; the cause never
// reaches the client.
func (s *Server) failStream(ctx context.Context, rc *http.ResponseController, w http.ResponseWriter, streamID string, cursor int64, err error) {
	s.log.LogAttrs(ctx, slog.LevelError, "stream error",
		slog.String("stream", streamID),
		slog.Int64("cursor", cursor),
		slog.Any("err", err),
	)
	s.finish(ctx, rc, w, streamID, cursor, errorFrame)
}

// finish writes the last frame of a stream under the short deadline.
func (s *Server) finish(ctx context.Context, rc *http.ResponseController, w http.ResponseWriter, streamID string, cursor int64, frame []byte) {
	if err := s.send(rc, w, frame, finalFrameTimeout); err != nil {
		s.logWriteFailed(ctx, streamID, cursor, err)
	}
}

// send writes frame (when not nil) and flushes, all under a deadline of timeout from now.
// A nil frame only flushes what is already buffered, such as the response headers.
func (s *Server) send(rc *http.ResponseController, w http.ResponseWriter, frame []byte, timeout time.Duration) error {
	if err := rc.SetWriteDeadline(time.Now().Add(timeout)); err != nil {
		return err
	}
	if frame != nil {
		if _, err := w.Write(frame); err != nil {
			return err
		}
	}
	return rc.Flush()
}

func (s *Server) logWriteFailed(ctx context.Context, streamID string, cursor int64, err error) {
	s.log.LogAttrs(ctx, slog.LevelInfo, "stream write failed",
		slog.String("stream", streamID),
		slog.Int64("cursor", cursor),
		slog.Any("err", err),
	)
}

// eventFrame is `id: <seq>`, `event: <type>` and one `data:` line holding the event JSON.
// The JSON is compact, so it never contains a newline, and a type matches the ledger's
// pattern, so it cannot break the frame either.
func eventFrame(ev contract.Event) ([]byte, error) {
	data, err := json.Marshal(ev)
	if err != nil {
		return nil, err
	}
	frame := make([]byte, 0, len(data)+len(ev.Type)+48)
	frame = append(frame, "id: "...)
	frame = strconv.AppendInt(frame, ev.Seq, 10)
	frame = append(frame, "\nevent: "...)
	frame = append(frame, ev.Type...)
	frame = append(frame, "\ndata: "...)
	frame = append(frame, data...)
	frame = append(frame, "\n\n"...)
	return frame, nil
}

// errorFrame is the last frame of a stream that failed. Its body is the generic error; the
// cause is in the log.
var errorFrame = func() []byte {
	data, _ := json.Marshal(contract.ErrorBody{Error: contract.InternalError()})
	return append(append([]byte("event: error\ndata: "), data...), "\n\n"...)
}()
