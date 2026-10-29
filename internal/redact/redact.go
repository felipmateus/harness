// Package redact removes secrets from text before it reaches logs or the wire.
package redact

import "regexp"

// Placeholder replaces every secret value.
const Placeholder = "[REDACTED]"

type rule struct {
	re   *regexp.Regexp
	repl string
}

// Order matters: an Authorization header is redacted whole before the bearer rule runs.
var rules = []rule{
	{regexp.MustCompile(`(?i)(authorization:\s*)(?:(?:basic|bearer|token)\s+)?[^\s,;]+`), "${1}" + Placeholder},
	{regexp.MustCompile(`(?i)\b(bearer\s+)[A-Za-z0-9._~+/=-]+`), "${1}" + Placeholder},
	{regexp.MustCompile(`(?i)\b(password|passwd|token|secret|api[_-]?key)=[^\s&"',;]+`), "${1}=" + Placeholder},
	{regexp.MustCompile(`(?i)\b([a-z][a-z0-9+.-]*://)[^/\s:@]+:[^/\s@]+@`), "${1}" + Placeholder + "@"},
}

// String returns s with every recognised secret value replaced by Placeholder.
func String(s string) string {
	for _, r := range rules {
		s = r.re.ReplaceAllString(s, r.repl)
	}
	return s
}
