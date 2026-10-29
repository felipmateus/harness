package contract

import (
	"encoding/json"
	"errors"
	"testing"
)

// The wording is the `_dx.md` Errors table; the API and the CLI both depend on it.
func TestServerErrorMessages(t *testing.T) {
	tests := []struct {
		name     string
		err      *Error
		wantCode string
		wantMsg  string
	}{
		{"not ready", NotReadyError(), CodeDaemonNotReady, "daemon is starting"},
		{"draining", DrainingError(), CodeDaemonDraining, "daemon is stopping"},
		{"not found", NotFoundError("GET", "/api/nope"), CodeNotFound, "no route for GET /api/nope"},
		{"method not allowed", MethodNotAllowedError("POST", "/api/status"), CodeMethodNotAllowed, "method POST not allowed on /api/status"},
		{"unknown parameter", UnknownParameterError("limit"), CodeInvalidRequest, `unknown parameter "limit"`},
		{"duplicate parameter", DuplicateParameterError("after"), CodeInvalidRequest, `parameter "after" given more than once`},
		{"invalid query", InvalidQueryError("invalid semicolon separator in query"), CodeInvalidRequest, "invalid query string: invalid semicolon separator in query"},
		{"unknown field", UnknownFieldError("why"), CodeInvalidRequest, `unknown field "why"`},
		{"invalid body", InvalidBodyError("unexpected EOF"), CodeInvalidRequest, "invalid request body: unexpected EOF"},
		{"internal", InternalError(), CodeInternal, "internal error"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.err.Code != tt.wantCode || tt.err.Message != tt.wantMsg {
				t.Fatalf("got {%s, %q}, want {%s, %q}", tt.err.Code, tt.err.Message, tt.wantCode, tt.wantMsg)
			}
			if tt.err.Hint != "" || tt.err.Details != nil {
				t.Fatalf("hint %q / details %v: the _dx table has neither for this error", tt.err.Hint, tt.err.Details)
			}
		})
	}
}

func TestErrorBodyShape(t *testing.T) {
	b, err := json.Marshal(ErrorBody{Error: InternalError()})
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"error":{"code":"internal","message":"internal error"}}`; string(b) != want {
		t.Fatalf("body = %s, want %s", b, want)
	}
}

func TestValidateShutdownReason(t *testing.T) {
	if err := ValidateShutdownReason("request"); err != nil {
		t.Fatalf("request: %v", err)
	}
	for _, bad := range []string{"signal", "context", "", "Request"} {
		err := ValidateShutdownReason(bad)
		var ce *Error
		if !errors.As(err, &ce) || ce.Code != CodeInvalidRequest || ce.Message != `reason must be "request"` {
			t.Errorf("reason %q: got %v, want invalid_request `reason must be \"request\"`", bad, err)
		}
	}
}
