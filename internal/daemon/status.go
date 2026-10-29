package daemon

import (
	"context"
	"log"
	"log/slog"

	"github.com/felipmateus/harness/internal/api/contract"
	"github.com/felipmateus/harness/internal/logging"
)

// statusProvider answers the two questions the API cannot answer itself. It is only used
// once the server runs, which is after every field it reads has been set.
type statusProvider struct{ d *daemon }

// Status reports GET /api/status, except the state, which the handler reads from the gate
// so that the report and the admission decision always agree.
func (p statusProvider) Status(ctx context.Context) (contract.Status, error) {
	d := p.d
	stats, err := d.led.Stats(ctx)
	if err != nil {
		return contract.Status{}, err
	}
	return contract.Status{
		PID:           d.pid,
		Version:       d.opts.Version.Version,
		StartedAt:     formatTime(d.started),
		UptimeSeconds: d.uptimeSeconds(),
		Home:          d.home.Dir,
		Socket:        d.home.SocketPath(),
		Ledger: contract.LedgerStatus{
			Path:          d.home.DBPath(),
			SchemaVersion: stats.SchemaVersion,
			Streams:       int64(stats.Streams),
			Events:        int64(stats.Events),
		},
		Config: contract.ConfigStatus{
			Path:   d.cfg.Path,
			Exists: d.cfg.Exists,
			Digest: d.cfg.Digest,
		},
	}, nil
}

// ShutdownInfo is the answer to POST /api/daemon/shutdown. It touches no ledger and takes
// no lock, because the route answers in every state, a starting daemon included. The grace
// is the text the operator wrote (or the default), so the CLI can show it back.
func (p statusProvider) ShutdownInfo() contract.ShutdownInfo {
	d := p.d
	grace, err := d.cfg.Config.Daemon.ShutdownGrace.MarshalText()
	if err != nil {
		grace = []byte(d.grace().String())
	}
	return contract.ShutdownInfo{
		PID:           d.pid,
		UptimeSeconds: d.uptimeSeconds(),
		ShutdownGrace: string(grace),
	}
}

// slogLogger adapts the daemon logger for net/http's ErrorLog, so the server's own
// complaints reach harness.log and never the detached daemon's stderr.
func slogLogger(l *logging.Logger) *log.Logger {
	return slog.NewLogLogger(l.Handler(), slog.LevelWarn)
}
