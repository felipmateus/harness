package api

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/felipmateus/harness/internal/api/contract"
	"github.com/felipmateus/harness/internal/redact"
)

// statusFor maps an error code to its HTTP status (`_dx.md` Errors table).
func statusFor(code string) int {
	switch code {
	case contract.CodeInvalidStreamID, contract.CodeInvalidCursor, contract.CodeInvalidLimit, contract.CodeInvalidRequest:
		return http.StatusBadRequest
	case contract.CodeNotFound:
		return http.StatusNotFound
	case contract.CodeMethodNotAllowed:
		return http.StatusMethodNotAllowed
	case contract.CodeDaemonNotReady, contract.CodeDaemonDraining:
		return http.StatusServiceUnavailable
	default:
		return http.StatusInternalServerError
	}
}

// writeError is the single error writer: every error body on the wire is
// {"error":{code,message,hint?,details?}}, and the text is redacted first.
func writeError(w http.ResponseWriter, status int, code, message, hint string, details any) {
	body, err := json.Marshal(contract.ErrorBody{Error: &contract.Error{
		Code:    code,
		Message: redact.String(message),
		Hint:    redact.String(hint),
		Details: details,
	}})
	if err != nil {
		// Only unmarshalable details can get here; the client still gets the shape.
		status = http.StatusInternalServerError
		body, _ = json.Marshal(contract.ErrorBody{Error: contract.InternalError()})
	}
	writeBody(w, status, body)
}

// writeContractError writes err as the error body, with the status of its code. An error
// that is not a *contract.Error is an internal one: its text never reaches the client.
func writeContractError(w http.ResponseWriter, err error) {
	var ce *contract.Error
	if !errors.As(err, &ce) {
		ce = contract.InternalError()
	}
	writeError(w, statusFor(ce.Code), ce.Code, ce.Message, ce.Hint, ce.Details)
}

// writeJSON writes v as the body of a response. A value that cannot be marshalled is an
// internal error, reported before any header is written.
func writeJSON(w http.ResponseWriter, status int, v any) error {
	body, err := json.Marshal(v)
	if err != nil {
		writeContractError(w, contract.InternalError())
		return err
	}
	writeBody(w, status, body)
	return nil
}

func writeBody(w http.ResponseWriter, status int, body []byte) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	// A write error means the client is gone; there is nobody to tell.
	_, _ = w.Write(append(body, '\n'))
}
