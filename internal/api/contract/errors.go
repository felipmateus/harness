// Package contract holds the wire contract shared by the API and the CLI: error codes,
// request and response shapes, and the validation both sides apply with identical
// messages. It is a leaf package.
package contract

// Error codes. The `_dx.md` Errors table is the authority for messages and exit codes.
const (
	CodeDaemonUnavailable    = "daemon_unavailable"
	CodeDaemonNotReady       = "daemon_not_ready"
	CodeDaemonDraining       = "daemon_draining"
	CodeDaemonAlreadyRunning = "daemon_already_running"
	CodeDaemonStartFailed    = "daemon_start_failed"
	CodeDaemonStartTimeout   = "daemon_start_timeout"
	CodeDaemonStopTimeout    = "daemon_stop_timeout"
	CodeLedgerSchemaAhead    = "ledger_schema_ahead"
	CodeConfigInvalid        = "config_invalid"
	CodeHomeInsecure         = "home_insecure"
	CodeSocketPathTooLong    = "socket_path_too_long"
	CodeInvalidStreamID      = "invalid_stream_id"
	CodeInvalidCursor        = "invalid_cursor"
	CodeInvalidLimit         = "invalid_limit"
	CodeInvalidRequest       = "invalid_request"
	CodeMethodNotAllowed     = "method_not_allowed"
	CodeNotFound             = "not_found"
	CodeUsage                = "usage"
	CodeInternal             = "internal"
)

// Error is the single error shape on the wire and on the CLI's stderr.
type Error struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Hint    string `json:"hint,omitempty"`
	Details any    `json:"details,omitempty"`
}

func (e *Error) Error() string { return e.Message }

// ErrorBody wraps Error as {"error":{...}}.
type ErrorBody struct {
	Error *Error `json:"error"`
}

// StartFailedDetails is the `details` object of daemon_start_failed.
type StartFailedDetails struct {
	PID        int      `json:"pid"`
	ExitCode   int      `json:"exit_code"`
	OutputTail []string `json:"output_tail"`
}

// StartTimeoutDetails is the `details` object of daemon_start_timeout: the daemon that was
// stopped, and the last lines of its startup output.
type StartTimeoutDetails struct {
	PID        int      `json:"pid"`
	OutputTail []string `json:"output_tail"`
}
