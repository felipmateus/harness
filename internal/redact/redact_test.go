package redact_test

import (
	"testing"

	"github.com/felipmateus/harness/internal/redact"
)

// UT-080
func TestString_RedactsSecretValues(t *testing.T) {
	cases := []struct{ in, want string }{
		{"call with Bearer abc.def now", "call with Bearer [REDACTED] now"},
		{"Authorization: Basic xyz sent", "Authorization: [REDACTED] sent"},
		{"Authorization: Bearer abc123", "Authorization: [REDACTED]"},
		{"url?token=abc&x=1", "url?token=[REDACTED]&x=1"},
		{"login password=hunter2 ok", "login password=[REDACTED] ok"},
		{"secret=s done", "secret=[REDACTED] done"},
		{"api_key=k end", "api_key=[REDACTED] end"},
		{"fetch https://user:pass@host/path", "fetch https://[REDACTED]@host/path"},
		{"nothing to hide here", "nothing to hide here"},
	}
	for _, tc := range cases {
		if got := redact.String(tc.in); got != tc.want {
			t.Errorf("String(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
