package ledger

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"

	"github.com/oklog/ulid/v2"
)

// maxDataBytes is the largest accepted event payload (1 MiB); the schema CHECKs it too.
const maxDataBytes = 1 << 20

// minDataBytes is the shortest payload the schema accepts: `{}`.
const minDataBytes = 2

var (
	streamPattern = regexp.MustCompile(`^[a-z][A-Za-z0-9_.:-]{0,127}$`)
	idPattern     = regexp.MustCompile(`^[A-Za-z0-9_.:-]{1,64}$`)
	typePattern   = regexp.MustCompile(`^[a-z][a-z0-9_.:-]{0,127}$`)

	emptyObject = []byte("{}")
)

// validate checks e and returns a copy that is safe to hand to the writer: the ID is set
// (a ULID when the producer gave none) and Data is a private, non-empty JSON value.
// The returned event always carries the ID, even with an error.
func validate(e NewEvent) (NewEvent, error) {
	out := NewEvent{Stream: e.Stream, ID: e.ID, Type: e.Type}
	if out.ID == "" {
		out.ID = ulid.Make().String()
	}
	switch {
	case !streamPattern.MatchString(e.Stream):
		return out, ErrInvalidStream
	case !typePattern.MatchString(e.Type):
		return out, ErrInvalidType
	case !idPattern.MatchString(out.ID):
		return out, ErrInvalidID
	}
	if len(e.Data) == 0 {
		out.Data = bytes.Clone(emptyObject)
		return out, nil
	}
	if len(e.Data) > maxDataBytes {
		return out, fmt.Errorf("%w: %d bytes", ErrPayloadTooLarge, len(e.Data))
	}
	if !json.Valid(e.Data) {
		return out, ErrInvalidData
	}
	if len(e.Data) < minDataBytes {
		// Valid JSON, but shorter than the schema's two-byte minimum (a lone digit).
		return out, fmt.Errorf("%w: a single-character value is below the 2-byte minimum", ErrInvalidData)
	}
	// A private copy: the caller may reuse its slice after a cancelled Append while the
	// writer still holds the request.
	out.Data = bytes.Clone(e.Data)
	return out, nil
}
