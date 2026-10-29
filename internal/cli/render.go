package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"unicode"

	"github.com/felipmateus/harness/internal/api/contract"
)

// Column widths of the human `events` rendering (`_dx.md`): SEQ, TIME and TYPE are padded to
// 5, 24 and 32 characters, then DATA, with two spaces between columns. A longer value is
// printed in full.
const (
	seqWidth  = 5
	timeWidth = 24
	typeWidth = 32
)

// eventsHeader is the heading line of the human rendering.
func eventsHeader() string {
	return fmt.Sprintf("%-*s  %-*s  %-*s  %s", seqWidth, "SEQ", timeWidth, "TIME", typeWidth, "TYPE", "DATA")
}

// eventRow renders one event as a table row.
func eventRow(ev contract.Event) string {
	return fmt.Sprintf("%-*d  %-*s  %-*s  %s",
		seqWidth, ev.Seq, timeWidth, ev.Time, typeWidth, ev.Type, formatData(ev.Data))
}

// formatData lists the top-level fields of an event's payload as key=value, in the order
// they were recorded. A string prints bare, null prints as null, and a nested object or
// array prints as compact JSON. A payload that is not an object prints as compact JSON.
func formatData(raw json.RawMessage) string {
	dec := json.NewDecoder(bytes.NewReader(raw))
	tok, err := dec.Token()
	if delim, ok := tok.(json.Delim); err != nil || !ok || delim != '{' {
		return compactJSON(raw)
	}
	var parts []string
	for dec.More() {
		keyTok, err := dec.Token()
		key, ok := keyTok.(string)
		if err != nil || !ok {
			return compactJSON(raw)
		}
		var value json.RawMessage
		if err := dec.Decode(&value); err != nil {
			return compactJSON(raw)
		}
		parts = append(parts, key+"="+formatValue(value))
	}
	return strings.Join(parts, " ")
}

// formatValue renders one field value.
func formatValue(raw json.RawMessage) string {
	raw = bytes.TrimSpace(raw)
	switch {
	case len(raw) == 0:
		return "null"
	case raw[0] == '"':
		var s string
		if err := json.Unmarshal(raw, &s); err != nil || hasControl(s) {
			return compactJSON(raw) // keeps a multi-line value on one line
		}
		return s
	case raw[0] == '{' || raw[0] == '[':
		return compactJSON(raw)
	default:
		return string(raw) // a number, true, false or null
	}
}

func hasControl(s string) bool {
	return strings.IndexFunc(s, unicode.IsControl) >= 0
}

func compactJSON(raw json.RawMessage) string {
	var b bytes.Buffer
	if err := json.Compact(&b, raw); err != nil {
		return string(raw)
	}
	return b.String()
}
