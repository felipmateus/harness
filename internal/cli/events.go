package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/felipmateus/harness/internal/api/contract"
)

// reasonDaemonStopped is the reason of the `shutdown` frame, and of the closing line.
const reasonDaemonStopped = "daemon stopped"

func (a *app) newEventsCmd() *cobra.Command {
	var stream, after, limit string
	var follow bool
	cmd := &cobra.Command{
		Use:   "events",
		Short: "List or follow the events of a stream",
		Long: "List the events of one stream in sequence order, or with --follow print the\n" +
			"history after --after and then new events as they are persisted, until\n" +
			"interrupted or the daemon stops.\n\n" +
			"A page holds at most --limit events and at most 8 MiB of event data; has_more in\n" +
			"the page tells whether more exist. --follow needs -o human or -o jsonl.",
		Args: cobra.NoArgs,
		RunE: a.run(func(cmd *cobra.Command) error {
			if follow && a.mode == outJSON {
				return usageError(cmd.CommandPath(), "--follow cannot be used with -o json; use -o jsonl")
			}
			// Validated here, with the functions the API uses, so the exit is 65 whether or
			// not a daemon runs.
			if err := contract.ValidateStream(stream); err != nil {
				return err
			}
			afterSeq, err := contract.ParseAfter(after)
			if err != nil {
				return err
			}
			limitN, err := contract.ParseLimit(limit)
			if err != nil {
				return err
			}
			h, err := a.resolveHome()
			if err != nil {
				return err
			}
			api := a.newClient(h.SocketPath())
			if follow {
				ctx, stop := a.interrupt(cmd.Context())
				defer stop()
				return a.followEvents(ctx, api, h.SocketPath(), stream, afterSeq)
			}
			page, err := api.Events(cmd.Context(), stream, afterSeq, limitN)
			if err != nil {
				return err
			}
			return a.printPage(page)
		}),
	}
	f := cmd.Flags()
	f.StringVar(&stream, "stream", "daemon", "stream to read")
	f.StringVar(&after, "after", "0", "return events with a sequence greater than `seq`")
	f.StringVar(&limit, "limit", "100", "page size, 1-1000; not used with --follow")
	f.BoolVar(&follow, "follow", false, "print the history, then new events live, until interrupted")
	return cmd
}

// printPage prints one page of events in the current output mode.
func (a *app) printPage(page contract.EventsPage) error {
	p := a.printer()
	if page.Events == nil {
		page.Events = []contract.Event{}
	}
	switch p.mode {
	case outJSON:
		return p.document(page)
	case outJSONL:
		for _, ev := range page.Events {
			if err := p.document(ev); err != nil {
				return err
			}
		}
		return p.document(struct {
			Page contract.Page `json:"page"`
		}{page.Page})
	default:
		p.line("%s", eventsHeader())
		for _, ev := range page.Events {
			p.line("%s", eventRow(ev))
		}
		return nil
	}
}

// closedDocument is the last line of a jsonl follow that ended because the daemon stopped.
type closedDocument struct {
	Closed struct {
		Reason string `json:"reason"`
	} `json:"closed"`
}

// followEvents prints the history after `after` and then new events, until the daemon
// stops, the stream breaks, or ctx ends (Ctrl-C).
//
// Every byte from the daemon, keepalives included, restarts a 45 s idle deadline; a stream
// that stays silent that long, or ends without its final frame, is a lost daemon.
func (a *app) followEvents(ctx context.Context, api daemonAPI, socket, stream string, after int64) error {
	reqCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	var idled flag
	idle := a.clock.AfterFunc(followIdle, func() {
		idled.set()
		cancel()
	})
	defer idle.Stop()

	body, err := api.OpenStream(reqCtx, stream, after)
	if err != nil {
		if idled.isSet() {
			return noDaemonError(socket)
		}
		return err
	}
	defer body.Close()
	idle.Reset(followIdle)

	p := a.printer()
	if p.mode == outHuman {
		p.line("%s", eventsHeader())
	}
	frames := newSSEReader(&idleReader{r: body, timer: idle, idle: followIdle})
	for {
		f, err := frames.next()
		if err != nil {
			switch {
			case idled.isSet():
				return noDaemonError(socket)
			case ctx.Err() != nil:
				return errInterrupted
			case errors.Is(err, errSSELineTooLong):
				return &contract.Error{Code: contract.CodeInternal, Message: err.Error()}
			default:
				return noDaemonError(socket) // the connection ended without a final frame
			}
		}
		switch {
		case f.hasID: // events carry an id, the frames that end a stream do not
			var ev contract.Event
			if err := json.Unmarshal(f.data, &ev); err != nil {
				return &contract.Error{Code: contract.CodeInternal, Message: "unreadable event frame: " + err.Error()}
			}
			if p.mode == outJSONL {
				err = p.document(ev)
			} else {
				p.line("%s", eventRow(ev))
			}
			if err != nil {
				return err
			}
		case f.event == "shutdown":
			return a.closeFollow(p, f.data)
		case f.event == "error":
			return streamError(f.data)
		}
	}
}

// closeFollow ends a follow whose daemon stopped.
func (a *app) closeFollow(p printer, data []byte) error {
	var body struct {
		Reason string `json:"reason"`
	}
	_ = json.Unmarshal(data, &body)
	if body.Reason == "" {
		body.Reason = reasonDaemonStopped
	}
	if p.mode == outJSONL {
		var doc closedDocument
		doc.Closed.Reason = body.Reason
		return p.document(doc)
	}
	fmt.Fprintf(a.stderr, "stream closed: %s\n", body.Reason)
	return nil
}

// streamError turns an `event: error` frame into the command's error. The frame carries
// the generic internal error; one that cannot be read is the same error.
func streamError(data []byte) error {
	var body contract.ErrorBody
	if err := json.Unmarshal(data, &body); err != nil || body.Error == nil || body.Error.Code == "" {
		return contract.InternalError()
	}
	return body.Error
}
