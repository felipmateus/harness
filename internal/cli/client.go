package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"syscall"
	"time"

	"github.com/felipmateus/harness/internal/api/contract"
)

// Request timeouts. Status and shutdown are small and must answer quickly; a page of
// events may carry up to 8 MiB. A follow has no overall timeout, only an idle deadline.
const (
	controlTimeout = 2 * time.Second
	listTimeout    = 30 * time.Second
	// doctorTimeout covers a daemon-mode report: its probes run in parallel under 2 s each,
	// so this only guards against a daemon that hangs.
	doctorTimeout = 10 * time.Second
	followIdle    = 45 * time.Second
)

// Bounds on what the client will read from the daemon.
const (
	maxJSONBody  = 32 << 20 // a page is at most 8 MiB of event data plus framing
	maxErrorBody = 1 << 20
)

// daemonAPI is what the commands use of the daemon: the API contract, over the socket. The
// real implementation is *client; tests supply a fake.
type daemonAPI interface {
	Status(ctx context.Context) (contract.Status, error)
	Shutdown(ctx context.Context) (contract.ShutdownInfo, error)
	Doctor(ctx context.Context) (contract.Doctor, error)
	Events(ctx context.Context, stream string, after int64, limit int) (contract.EventsPage, error)
	// OpenStream opens the SSE stream of a ledger stream. The caller closes the body.
	OpenStream(ctx context.Context, stream string, after int64) (io.ReadCloser, error)
}

// client is an HTTP client whose transport dials the daemon's Unix socket (ADR-002).
type client struct {
	socket  string
	http    *http.Client
	control time.Duration // timeout of status and shutdown
	list    time.Duration // timeout of an events page
}

func newClient(socket string) *client {
	dialer := &net.Dialer{}
	return &client{
		socket: socket,
		http: &http.Client{Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return dialer.DialContext(ctx, "unix", socket)
			},
			DisableKeepAlives:  true,
			DisableCompression: true,
		}},
		control: controlTimeout,
		list:    listTimeout,
	}
}

// The host is ignored by the dialer; HTTP/1.1 only needs one.
const baseURL = "http://harness"

func (c *client) Status(ctx context.Context) (contract.Status, error) {
	var out contract.StatusResponse
	err := c.json(ctx, c.control, http.MethodGet, "/api/status", nil, &out)
	return out.Status, err
}

func (c *client) Shutdown(ctx context.Context) (contract.ShutdownInfo, error) {
	var out contract.ShutdownResponse
	body, _ := json.Marshal(contract.ShutdownRequest{Reason: contract.ShutdownReasonRequest})
	err := c.json(ctx, c.control, http.MethodPost, "/api/daemon/shutdown", body, &out)
	return out.Daemon, err
}

func (c *client) Doctor(ctx context.Context) (contract.Doctor, error) {
	var out contract.DoctorResponse
	err := c.json(ctx, doctorTimeout, http.MethodGet, "/api/doctor", nil, &out)
	return out.Doctor, err
}

func (c *client) Events(ctx context.Context, stream string, after int64, limit int) (contract.EventsPage, error) {
	q := url.Values{"after": {strconv.FormatInt(after, 10)}, "limit": {strconv.Itoa(limit)}}
	var out contract.EventsPage
	err := c.json(ctx, c.list, http.MethodGet, eventsPath(stream)+"?"+q.Encode(), nil, &out)
	return out, err
}

// OpenStream never sends `limit`: the API has no such parameter on the stream route.
func (c *client) OpenStream(ctx context.Context, stream string, after int64) (io.ReadCloser, error) {
	path := eventsPath(stream) + "/stream?after=" + strconv.FormatInt(after, 10)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "text/event-stream")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, c.transportError(ctx, nil, err)
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		return nil, readAPIError(resp)
	}
	return resp.Body, nil
}

func eventsPath(stream string) string {
	return "/api/streams/" + url.PathEscape(stream) + "/events"
}

// json performs one request under timeout and decodes a 2xx body into out.
func (c *client) json(ctx context.Context, timeout time.Duration, method, path string, body []byte, out any) error {
	reqCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(reqCtx, method, baseURL+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return c.transportError(ctx, reqCtx, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return readAPIError(resp)
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxJSONBody)).Decode(out); err != nil {
		if reqCtx.Err() != nil {
			return c.transportError(ctx, reqCtx, reqCtx.Err())
		}
		return &contract.Error{Code: contract.CodeInternal, Message: "unreadable answer from the daemon: " + err.Error()}
	}
	return nil
}

// noDaemonError is the error of a socket nobody listens on.
func noDaemonError(socket string) *contract.Error {
	return &contract.Error{
		Code:    contract.CodeDaemonUnavailable,
		Message: "no daemon is listening on " + socket,
		Hint:    "run `harness daemon start`",
	}
}

// transportError classifies a failed round trip. A cancelled parent context is the user's
// interrupt; a dial that is refused or finds no socket is "no daemon"; anything else means
// a daemon that did not answer.
func (c *client) transportError(parent, req context.Context, err error) error {
	switch {
	case parent.Err() != nil:
		return errInterrupted
	case errors.Is(err, syscall.ENOENT), errors.Is(err, syscall.ECONNREFUSED), errors.Is(err, syscall.ENOTDIR):
		return noDaemonError(c.socket)
	case req != nil && errors.Is(req.Err(), context.DeadlineExceeded):
		return &contract.Error{
			Code:    contract.CodeDaemonUnavailable,
			Message: fmt.Sprintf("daemon did not answer on %s", c.socket),
		}
	default:
		return &contract.Error{
			Code:    contract.CodeDaemonUnavailable,
			Message: fmt.Sprintf("connection to the daemon on %s failed", c.socket),
		}
	}
}

// readAPIError decodes the daemon's {"error":{...}} body.
func readAPIError(resp *http.Response) error {
	data, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
	var body contract.ErrorBody
	if err := json.Unmarshal(data, &body); err == nil && body.Error != nil && body.Error.Code != "" {
		return body.Error
	}
	return &contract.Error{
		Code:    contract.CodeInternal,
		Message: fmt.Sprintf("unexpected answer from the daemon: HTTP %d", resp.StatusCode),
	}
}

// isUnavailable reports whether err says there is no daemon to talk to.
func isUnavailable(err error) bool {
	var wire *contract.Error
	return errors.As(err, &wire) && wire.Code == contract.CodeDaemonUnavailable
}
