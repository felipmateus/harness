package contract

import (
	"fmt"
	"regexp"
	"strconv"
)

// Stream IDs: lowercase first character; uppercase allowed afterwards so IDs such as
// ULIDs can be embedded (e.g. "session:01JBJ7QK3W").
const StreamPattern = `^[a-z][A-Za-z0-9_.:-]{0,127}$`

const (
	DefaultLimit = 100
	MinLimit     = 1
	MaxLimit     = 1000
)

var streamRE = regexp.MustCompile(StreamPattern)

// ValidateStream checks a stream ID.
func ValidateStream(id string) error {
	if streamRE.MatchString(id) {
		return nil
	}
	return &Error{
		Code:    CodeInvalidStreamID,
		Message: fmt.Sprintf("invalid stream id %q: must match %s", id, StreamPattern),
	}
}

// ParseAfter parses an `after` cursor or Last-Event-ID value. Empty means 0.
func ParseAfter(raw string) (int64, error) {
	if raw == "" {
		return 0, nil
	}
	v, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || v < 0 {
		return 0, afterError(raw)
	}
	return v, nil
}

// ValidateAfter checks an already-parsed cursor.
func ValidateAfter(v int64) error {
	if v < 0 {
		return afterError(strconv.FormatInt(v, 10))
	}
	return nil
}

func afterError(raw string) error {
	return &Error{Code: CodeInvalidCursor, Message: fmt.Sprintf("after %s must be >= 0", raw)}
}

// ParseLimit parses a `limit` value. Empty means DefaultLimit.
func ParseLimit(raw string) (int, error) {
	if raw == "" {
		return DefaultLimit, nil
	}
	v, err := strconv.Atoi(raw)
	if err != nil {
		return 0, limitError(raw)
	}
	if err := ValidateLimit(v); err != nil {
		return 0, err
	}
	return v, nil
}

// ValidateLimit checks an already-parsed limit.
func ValidateLimit(v int) error {
	if v < MinLimit || v > MaxLimit {
		return limitError(strconv.Itoa(v))
	}
	return nil
}

func limitError(raw string) error {
	return &Error{
		Code:    CodeInvalidLimit,
		Message: fmt.Sprintf("limit %s must be between %d and %d", raw, MinLimit, MaxLimit),
	}
}
