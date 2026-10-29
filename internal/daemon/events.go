package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/felipmateus/harness/internal/ledger"
)

// daemonStream is the ledger stream that records the daemon's own lifecycle.
const daemonStream = "daemon"

// Event types of the daemon stream (`_spec.md` Data Models).
const (
	typeConfigLoaded    = "config.loaded"
	typeStarted         = "daemon.started"
	typeStopping        = "daemon.stopping"
	typeStopped         = "daemon.stopped"
	typeBootFailed      = "daemon.boot_failed"
	typeUncleanDetected = "daemon.unclean_shutdown_detected"
)

// Why the daemon stopped. They are the values of the `reason` field of daemon.stopping.
const (
	reasonRequest = "request" // POST /api/daemon/shutdown
	reasonSignal  = "signal"  // SIGINT, SIGTERM or SIGHUP
	reasonContext = "context" // the context given to Run ended (tests and embedding)
)

// The payloads below list their fields in the order the spec documents them: encoding/json
// writes struct fields in declaration order, so the stored JSON keeps it. Every payload
// starts with the PID of the process that recorded it (Business Rule 11).

type configLoadedData struct {
	PID    int     `json:"pid"`
	Path   string  `json:"path"`
	Exists bool    `json:"exists"`
	Digest *string `json:"digest"` // "sha256:<hex>"; null when the file does not exist
}

type startedData struct {
	PID     int    `json:"pid"`
	Version string `json:"version"`
	BootMS  int64  `json:"boot_ms"`
}

type stoppingData struct {
	PID    int    `json:"pid"`
	Reason string `json:"reason"`
}

type stoppedData struct {
	PID           int   `json:"pid"`
	UptimeSeconds int64 `json:"uptime_seconds"`
}

type bootFailedData struct {
	PID  int    `json:"pid"`
	Step int    `json:"step"`
	Code string `json:"code"`
}

type uncleanData struct {
	PID         int    `json:"pid"`
	PreviousPID *int   `json:"previous_pid"` // null when the last event carried no PID
	LastSeq     int64  `json:"last_seq"`
	LastType    string `json:"last_type"`
}

// record appends one event to the daemon stream. The ledger commits and syncs it before
// any follower can see it, so a returned nil means the event is durable.
func (d *daemon) record(ctx context.Context, typ string, payload any) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("encode %s: %w", typ, err)
	}
	if _, _, err := d.led.Append(ctx, ledger.NewEvent{Stream: daemonStream, Type: typ, Data: data}); err != nil {
		return fmt.Errorf("record %s: %w", typ, err)
	}
	d.appended.Store(true)
	return nil
}

// previousPID reads the PID out of a daemon-stream event's data. It is nil when the field
// is missing or is not a number, which the unclean event records as null.
func previousPID(data json.RawMessage) *int {
	var payload struct {
		PID *int `json:"pid"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		return nil
	}
	return payload.PID
}

// isCleanEnd reports whether a daemon-stream event type closes a run properly: a graceful
// stop, or a boot failure that was recorded (Business Rule 13).
func isCleanEnd(eventType string) bool {
	return eventType == typeStopped || eventType == typeBootFailed
}

// timeLayout is the wire form of every timestamp: UTC with three fractional digits.
const timeLayout = "2006-01-02T15:04:05.000Z"

func formatTime(t time.Time) string { return t.UTC().Format(timeLayout) }
