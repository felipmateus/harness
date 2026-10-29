package daemon

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/felipmateus/harness/internal/api/contract"
)

// reportFinalError writes the one line a failed --detached child leaves behind: the
// *ExitError Run is about to return, as {"error":{"code","message","hint"}}. The CLI that
// spawned the child reads it from logs/daemon.out and re-raises the error, or starts over
// when another daemon owns the home (`_spec.md` Detached start, step 5).
//
// It is the last thing the child writes: the logger has done with its mirror by then, and
// nothing else writes to Options.Stderr afterwards. A nil err, or an error that is not an
// *ExitError, writes nothing; a cancelled boot and a clean stop both end with nil.
func (d *daemon) reportFinalError(err error) {
	var exit *ExitError
	if !errors.As(err, &exit) {
		return
	}
	line, mErr := finalErrorLine(exit)
	if mErr != nil {
		d.log().Error("final error line not written", "err", mErr)
		return
	}
	// One Write, so the line stays whole next to other writers of an O_APPEND file.
	if _, wErr := d.opts.Stderr.Write(line); wErr != nil {
		d.log().Error("final error line not written", "err", wErr)
	}
}

// finalErrorLine is the JSON line, newline included.
func finalErrorLine(e *ExitError) ([]byte, error) {
	body := contract.ErrorBody{Error: &contract.Error{Code: e.ErrCode, Message: e.Message, Hint: e.Hint}}
	data, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("encode the final error line: %w", err)
	}
	return append(data, '\n'), nil
}
