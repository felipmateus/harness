package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/felipmateus/harness/internal/admission"
	"github.com/felipmateus/harness/internal/api/contract"
)

// maxShutdownBody bounds the optional body; the only valid one is a few dozen bytes.
const maxShutdownBody = 4 << 10

// handleShutdown answers POST /api/daemon/shutdown. It is exempt from the gate and always
// accepted with 202:
//   - while starting or running it asks the daemon to stop (while starting that cancels the
//     boot), once, however many requests arrive before the daemon reports draining;
//   - while draining it starts nothing: the daemon is already stopping.
//
// The body is optional. When present it must be {"reason":"request"} and nothing else.
func (s *Server) handleShutdown(w http.ResponseWriter, r *http.Request, _ url.Values) {
	reason, err := decodeShutdownRequest(w, r)
	if err != nil {
		writeContractError(w, err)
		return
	}
	if reason != nil {
		if err := contract.ValidateShutdownReason(*reason); err != nil {
			writeContractError(w, err)
			return
		}
	}

	// Trigger first: the callback only signals the daemon, so by the time the client reads
	// the 202 the stop is under way. http.Server.Shutdown still lets this response finish.
	if s.gate.State() != admission.Draining {
		s.stopOnce.Do(s.requestStop)
	}

	info := s.status.ShutdownInfo()
	info.Stopping = true
	_ = writeJSON(w, http.StatusAccepted, contract.ShutdownResponse{Daemon: info})
}

// decodeShutdownRequest reads the optional body strictly: unknown fields are rejected, and
// so is anything after the first JSON value. It returns nil when no reason was sent.
func decodeShutdownRequest(w http.ResponseWriter, r *http.Request) (*string, error) {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxShutdownBody))
	dec.DisallowUnknownFields()
	var req struct {
		Reason *string `json:"reason"`
	}
	if err := dec.Decode(&req); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, nil // an empty body is the same as no body
		}
		return nil, bodyError(err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, contract.InvalidBodyError("only one JSON object is allowed")
	}
	return req.Reason, nil
}

// bodyError turns a decoding failure into the wire error, without leaking Go type names.
func bodyError(err error) *contract.Error {
	var tooBig *http.MaxBytesError
	var typeErr *json.UnmarshalTypeError
	switch {
	case errors.As(err, &tooBig):
		return contract.InvalidBodyError("body is larger than " + strconv.FormatInt(tooBig.Limit, 10) + " bytes")
	case errors.As(err, &typeErr):
		if typeErr.Field != "" {
			return contract.InvalidBodyError("field " + strconv.Quote(typeErr.Field) + " has the wrong type")
		}
		return contract.InvalidBodyError("a JSON object is expected")
	}
	// encoding/json reports an unknown field only as text: `json: unknown field "name"`.
	if rest, ok := strings.CutPrefix(err.Error(), "json: unknown field "); ok {
		if name, uerr := strconv.Unquote(rest); uerr == nil {
			return contract.UnknownFieldError(name)
		}
	}
	return contract.InvalidBodyError(err.Error())
}
