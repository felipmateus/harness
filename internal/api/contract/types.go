package contract

import "encoding/json"

// Event is the wire form of a ledger event. It mirrors ledger.Event so this package
// stays a leaf; the API converts between them.
type Event struct {
	Stream string          `json:"stream"`
	Seq    int64           `json:"seq"`
	ID     string          `json:"id"`
	Type   string          `json:"type"`
	Time   string          `json:"time"` // 2006-01-02T15:04:05.000Z
	Data   json.RawMessage `json:"data"`
}

// Page describes one page of a stream.
type Page struct {
	After     int64 `json:"after"`
	NextAfter int64 `json:"next_after"`
	HasMore   bool  `json:"has_more"`
	Limit     int   `json:"limit"`
	Head      int64 `json:"head"`
}

// EventsPage is the body of GET /api/streams/{stream}/events.
type EventsPage struct {
	Events []Event `json:"events"`
	Page   Page    `json:"page"`
}

// Status is the daemon state reported by GET /api/status.
type Status struct {
	State         string       `json:"state"`
	PID           int          `json:"pid"`
	Version       string       `json:"version"`
	StartedAt     string       `json:"started_at"`
	UptimeSeconds int64        `json:"uptime_seconds"`
	Home          string       `json:"home"`
	Socket        string       `json:"socket"`
	Ledger        LedgerStatus `json:"ledger"`
	Config        ConfigStatus `json:"config"`
}

type LedgerStatus struct {
	Path          string `json:"path"`
	SchemaVersion int    `json:"schema_version"`
	Streams       int64  `json:"streams"`
	Events        int64  `json:"events"`
}

type ConfigStatus struct {
	Path   string  `json:"path"`
	Exists bool    `json:"exists"`
	Digest *string `json:"digest"`
}

type StatusResponse struct {
	Status Status `json:"status"`
}

// Doctor severities and sources.
const (
	SeverityOK    = "ok"
	SeverityWarn  = "warn"
	SeverityError = "error"

	SourceDaemon = "daemon"
	SourceLocal  = "local"
)

type DoctorItem struct {
	ID       string `json:"id"`
	Severity string `json:"severity"`
	Message  string `json:"message"`
	Fix      string `json:"fix,omitempty"`
}

type DoctorSummary struct {
	OK    int `json:"ok"`
	Warn  int `json:"warn"`
	Error int `json:"error"`
}

type Doctor struct {
	Status  string        `json:"status"`
	Source  string        `json:"source"`
	Items   []DoctorItem  `json:"items"`
	Summary DoctorSummary `json:"summary"`
}

type DoctorResponse struct {
	Doctor Doctor `json:"doctor"`
}

// ShutdownReasonRequest is the only reason a client may send.
const ShutdownReasonRequest = "request"

type ShutdownRequest struct {
	Reason string `json:"reason,omitempty"`
}

type ShutdownInfo struct {
	Stopping      bool   `json:"stopping"`
	PID           int    `json:"pid"`
	UptimeSeconds int64  `json:"uptime_seconds"`
	ShutdownGrace string `json:"shutdown_grace"`
}

type ShutdownResponse struct {
	Daemon ShutdownInfo `json:"daemon"`
}
