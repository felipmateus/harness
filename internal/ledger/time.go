package ledger

import (
	"encoding/json"
	"fmt"
	"time"
)

// timeLayout is the fixed-width UTC layout of every stored and marshalled time. Its width
// never varies, so the text sorts in chronological order.
const timeLayout = "2006-01-02T15:04:05.000Z"

// timeWidth is the length of a formatted time; the schema CHECKs it.
const timeWidth = len(timeLayout)

// Time marshals as a fixed-width UTC timestamp with milliseconds: 2006-01-02T15:04:05.000Z.
type Time struct{ time.Time }

// String returns the fixed-width form.
func (t Time) String() string { return formatTime(t.Time) }

// MarshalJSON implements json.Marshaler.
func (t Time) MarshalJSON() ([]byte, error) {
	return json.Marshal(formatTime(t.Time))
}

// UnmarshalJSON implements json.Unmarshaler. Only the fixed-width form is accepted.
func (t *Time) UnmarshalJSON(b []byte) error {
	if string(b) == "null" {
		return nil
	}
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return fmt.Errorf("ledger: time: %w", err)
	}
	parsed, err := parseTime(s)
	if err != nil {
		return err
	}
	t.Time = parsed
	return nil
}

// formatTime renders t in UTC, truncated to milliseconds.
func formatTime(t time.Time) string {
	return t.UTC().Truncate(time.Millisecond).Format(timeLayout)
}

// parseTime reads the fixed-width form back; anything else is an error.
func parseTime(s string) (time.Time, error) {
	if len(s) != timeWidth {
		return time.Time{}, fmt.Errorf("ledger: time %q is not %d characters", s, timeWidth)
	}
	t, err := time.Parse(timeLayout, s)
	if err != nil {
		return time.Time{}, fmt.Errorf("ledger: time %q: %w", s, err)
	}
	return t.UTC(), nil
}
