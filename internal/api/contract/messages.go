package contract

import "fmt"

// InternalMessage is the only text a client ever sees for an unexpected failure; the
// details go to the log.
const InternalMessage = "internal error"

// The constructors below build the server-side errors of the `_dx.md` Errors table, so the
// API and the CLI share one wording. Each returns the single wire shape.

// NotReadyError is the refusal of a request while the daemon is starting.
func NotReadyError() *Error {
	return &Error{Code: CodeDaemonNotReady, Message: "daemon is starting"}
}

// DrainingError is the refusal of a request while the daemon is stopping.
func DrainingError() *Error {
	return &Error{Code: CodeDaemonDraining, Message: "daemon is stopping"}
}

// NotFoundError answers a request for a path with no route.
func NotFoundError(method, path string) *Error {
	return &Error{Code: CodeNotFound, Message: fmt.Sprintf("no route for %s %s", method, path)}
}

// MethodNotAllowedError answers a known path requested with the wrong method.
func MethodNotAllowedError(method, path string) *Error {
	return &Error{Code: CodeMethodNotAllowed, Message: fmt.Sprintf("method %s not allowed on %s", method, path)}
}

// UnknownParameterError rejects a query parameter the route does not accept.
func UnknownParameterError(name string) *Error {
	return &Error{Code: CodeInvalidRequest, Message: fmt.Sprintf("unknown parameter %q", name)}
}

// DuplicateParameterError rejects a query parameter given more than once; taking one of
// the values and ignoring the rest would be a silent choice.
func DuplicateParameterError(name string) *Error {
	return &Error{Code: CodeInvalidRequest, Message: fmt.Sprintf("parameter %q given more than once", name)}
}

// InvalidQueryError rejects a query string that cannot be parsed.
func InvalidQueryError(detail string) *Error {
	return &Error{Code: CodeInvalidRequest, Message: "invalid query string: " + detail}
}

// UnknownFieldError rejects a body field the route does not accept.
func UnknownFieldError(name string) *Error {
	return &Error{Code: CodeInvalidRequest, Message: fmt.Sprintf("unknown field %q", name)}
}

// InvalidBodyError rejects a body that is not one well-formed JSON object.
func InvalidBodyError(detail string) *Error {
	return &Error{Code: CodeInvalidRequest, Message: "invalid request body: " + detail}
}

// InternalError is the answer to an unexpected failure.
func InternalError() *Error {
	return &Error{Code: CodeInternal, Message: InternalMessage}
}

// ValidateShutdownReason accepts the only reason a client may send. An empty string is
// not valid: a client that omits the field sends no reason at all.
func ValidateShutdownReason(reason string) error {
	if reason == ShutdownReasonRequest {
		return nil
	}
	return &Error{Code: CodeInvalidRequest, Message: fmt.Sprintf("reason must be %q", ShutdownReasonRequest)}
}
