package daemon

import (
	"context"

	"github.com/felipmateus/harness/internal/api/contract"
	"github.com/felipmateus/harness/internal/doctor"
)

// doctorRunner is what GET /api/doctor runs in daemon mode.
type doctorRunner interface {
	Run(ctx context.Context) (contract.Doctor, error)
}

// doctor returns the runner behind GET /api/doctor: the seven probes in daemon mode, told
// which config digest this run loaded so that config.valid can tell a file edited since.
// It is built when the server is wired, after boot step 4 has loaded the config.
func (d *daemon) doctor() doctorRunner {
	return doctor.New(doctor.Options{
		Home:         d.home,
		Mode:         doctor.ModeDaemon,
		LoadedDigest: d.cfg.Digest,
	})
}
