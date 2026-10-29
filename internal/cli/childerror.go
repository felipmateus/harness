package cli

import (
	"encoding/json"
	"strings"

	"github.com/felipmateus/harness/internal/api/contract"
)

// childError is the error a failed detached daemon reported as its last output line:
// {"error":{"code","message","hint"}} (`_spec.md` Detached start, step 5).
type childError struct {
	Code, Message, Hint string
}

// lastChildError finds the last line of lines that is an error object, scanning from the
// end. A log record is a JSON object too, but it has no top-level "error" key, so it is
// never mistaken for the report.
func lastChildError(lines []string) (childError, bool) {
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if !strings.HasPrefix(line, "{") {
			continue
		}
		var body struct {
			Error *struct {
				Code    string `json:"code"`
				Message string `json:"message"`
				Hint    string `json:"hint"`
			} `json:"error"`
		}
		if err := json.Unmarshal([]byte(line), &body); err != nil || body.Error == nil || body.Error.Code == "" {
			continue
		}
		return childError{Code: body.Error.Code, Message: body.Error.Message, Hint: body.Error.Hint}, true
	}
	return childError{}, false
}

// reraisable says whether the CLI reports the child's error as its own: the failures the
// daemon recognises keep their code and exit status. Everything else is a generic start
// failure, and daemon_already_running is not an error at all (another daemon owns the home).
func (e childError) reraisable() bool {
	switch e.Code {
	case contract.CodeConfigInvalid, contract.CodeHomeInsecure, contract.CodeSocketPathTooLong,
		contract.CodeLedgerSchemaAhead:
		return true
	}
	return false
}

func (e childError) alreadyRunning() bool { return e.Code == contract.CodeDaemonAlreadyRunning }

func (e childError) wire() *contract.Error {
	return &contract.Error{Code: e.Code, Message: e.Message, Hint: e.Hint}
}
