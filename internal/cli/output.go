package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/felipmateus/harness/internal/api/contract"
)

// outputMode is the value of -o.
type outputMode string

const (
	outHuman outputMode = "human"
	outJSON  outputMode = "json"
	outJSONL outputMode = "jsonl"
)

// parseOutputMode accepts the three documented values.
func parseOutputMode(s string) (outputMode, bool) {
	switch m := outputMode(s); m {
	case outHuman, outJSON, outJSONL:
		return m, true
	}
	return outHuman, false
}

// structured says whether the mode is machine-readable: stdout holds documents and stderr
// holds one JSON error object.
func (m outputMode) structured() bool { return m == outJSON || m == outJSONL }

// printer writes a command's result and its failure under the output rules of `_dx.md`:
// success goes to stdout; under json and jsonl an error is one JSON object on stderr and
// stdout stays empty.
type printer struct {
	mode outputMode
	out  io.Writer
	err  io.Writer
}

// document writes v as one compact JSON line on stdout.
func (p printer) document(v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("encode output: %w", err)
	}
	_, err = fmt.Fprintf(p.out, "%s\n", data)
	return err
}

// line writes one line of text on stdout.
func (p printer) line(format string, args ...any) {
	fmt.Fprintf(p.out, format+"\n", args...)
}

// failure writes err on stderr in the form the mode calls for.
func (p printer) failure(e *contract.Error) {
	if p.mode.structured() {
		data, err := json.Marshal(contract.ErrorBody{Error: e})
		if err != nil {
			data = []byte(`{"error":{"code":"internal","message":"internal error"}}`)
		}
		fmt.Fprintf(p.err, "%s\n", data)
		return
	}
	fmt.Fprintf(p.err, "error: %s\n", e.Message)
	if e.Hint != "" {
		fmt.Fprintf(p.err, "hint: %s\n", e.Hint)
	}
	if tail, ok := outputTail(e.Details); ok {
		fmt.Fprintln(p.err, "--- daemon output (last 20 lines) ---")
		if len(tail) == 0 {
			fmt.Fprintln(p.err, "(no output)")
		}
		for _, l := range tail {
			fmt.Fprintln(p.err, l)
		}
	}
}

// outputTail extracts the daemon output of a start failure or a start timeout.
func outputTail(details any) ([]string, bool) {
	switch d := details.(type) {
	case contract.StartFailedDetails:
		return d.OutputTail, true
	case contract.StartTimeoutDetails:
		return d.OutputTail, true
	}
	return nil, false
}

// outputModeFromArgs finds the -o value in a raw argument list. It exists for the case
// where the command line itself is wrong: flag parsing stopped before it reached -o, yet
// the error must still follow the requested mode. An unknown value reads as human.
func outputModeFromArgs(args []string) outputMode {
	mode := outHuman
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			break
		}
		var val string
		switch {
		case a == "-o" || a == "--output":
			if i+1 >= len(args) {
				continue
			}
			i++
			val = args[i]
		case strings.HasPrefix(a, "--output="):
			val = strings.TrimPrefix(a, "--output=")
		case strings.HasPrefix(a, "-o="):
			val = strings.TrimPrefix(a, "-o=")
		case strings.HasPrefix(a, "-o") && !strings.HasPrefix(a, "--") && len(a) > 2:
			val = a[2:]
		default:
			continue
		}
		if m, ok := parseOutputMode(val); ok {
			mode = m
		}
	}
	return mode
}
