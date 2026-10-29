package cli

import (
	"errors"
	"fmt"

	"github.com/felipmateus/harness/internal/api/contract"
	"github.com/felipmateus/harness/internal/daemon"
	"github.com/felipmateus/harness/internal/home"
)

// Process exit codes (sysexits.h, plus the shell's 130 for an interrupt). The `_dx.md`
// Errors table is the authority.
const (
	exitOK          = 0
	exitFailure     = 1
	exitUsage       = 64  // EX_USAGE: bad flags or arguments
	exitDataErr     = 65  // EX_DATAERR: input that fails validation
	exitUnavailable = 69  // EX_UNAVAILABLE: no daemon, or the daemon could not do it
	exitConfig      = 78  // EX_CONFIG: invalid config, insecure home, home path too long
	exitInterrupted = 130 // SIGINT
)

// errInterrupted ends a command that Ctrl-C stopped. It prints nothing.
var errInterrupted = errors.New("interrupted")

// silentExit ends a command with an exit code whose error has already been written, so the
// error path prints nothing more. The detached daemon child uses it: its final error line is
// the one JSON line the spawning CLI reads from logs/daemon.out.
type silentExit struct{ code int }

func (e *silentExit) Error() string { return fmt.Sprintf("exit %d", e.code) }

// exitCodeFor maps an error code to the process exit code.
func exitCodeFor(code string) int {
	switch code {
	case contract.CodeUsage:
		return exitUsage
	case contract.CodeInvalidStreamID, contract.CodeInvalidCursor, contract.CodeInvalidLimit,
		contract.CodeInvalidRequest:
		return exitDataErr
	case contract.CodeDaemonUnavailable, contract.CodeDaemonNotReady, contract.CodeDaemonDraining,
		contract.CodeDaemonAlreadyRunning, contract.CodeDaemonStartFailed,
		contract.CodeDaemonStartTimeout, contract.CodeDaemonStopTimeout,
		contract.CodeLedgerSchemaAhead, contract.CodeNotFound:
		return exitUnavailable
	case contract.CodeConfigInvalid, contract.CodeHomeInsecure, contract.CodeSocketPathTooLong:
		return exitConfig
	default:
		// internal, method_not_allowed, and every step code of a failed boot.
		return exitFailure
	}
}

// asWireError turns any error a command can return into the single error shape. An error of
// a kind the CLI does not know is an unexpected failure: code `internal`, with the text of
// the error, which is local and so safe to show.
func asWireError(err error) *contract.Error {
	var wire *contract.Error
	var homeErr *home.Error
	var exitErr *daemon.ExitError
	switch {
	case errors.As(err, &wire):
		return wire
	case errors.As(err, &homeErr):
		return &contract.Error{Code: homeErr.Code, Message: homeErr.Message, Hint: homeErr.Hint}
	case errors.As(err, &exitErr):
		return &contract.Error{Code: exitErr.ErrCode, Message: exitErr.Message, Hint: exitErr.Hint}
	default:
		return &contract.Error{Code: contract.CodeInternal, Message: err.Error()}
	}
}

// usageError is a failure of the command line itself.
func usageError(commandPath, message string) *contract.Error {
	return &contract.Error{
		Code:    contract.CodeUsage,
		Message: message,
		Hint:    "run `" + commandPath + " --help`",
	}
}
