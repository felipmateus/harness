// Package doctor runs the health probes behind `harness doctor`. It reports problems and
// suggests fixes; it never changes a file, a mode or a timestamp in the home.
//
// There are seven probes and two modes. In daemon mode the daemon runs them for
// GET /api/doctor and adds socket.permissions; in local mode the CLI runs them itself when
// no daemon answers and adds daemon.reachable, so doctor works exactly when the daemon
// cannot start. Every probe runs under its own 2 s timeout, in parallel, so one stuck probe
// cannot hide the others and the whole report is bounded by that timeout.
package doctor

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"sync"
	"time"

	"github.com/felipmateus/harness/internal/api/contract"
	"github.com/felipmateus/harness/internal/home"
	"github.com/felipmateus/harness/internal/ledger"
)

// ProbeTimeout is how long one probe may run before its item becomes an error.
const ProbeTimeout = 2 * time.Second

// Stable probe IDs. Agents branch on them.
const (
	IDHomePermissions   = "home.permissions"
	IDSocketPermissions = "socket.permissions"
	IDDaemonReachable   = "daemon.reachable"
	IDConfigValid       = "config.valid"
	IDLedgerIntegrity   = "ledger.integrity"
	IDLedgerSchema      = "ledger.schema"
	IDLogWritable       = "log.writable"
)

// Mode says where the probes run.
type Mode string

const (
	// ModeDaemon runs inside the daemon that answers the request.
	ModeDaemon Mode = contract.SourceDaemon
	// ModeLocal runs in the CLI when no daemon answers.
	ModeLocal Mode = contract.SourceLocal
)

// Options configure a Runner.
type Options struct {
	Home home.Home
	Mode Mode
	// LoadedDigest is, in daemon mode, the digest of the config file the daemon loaded at
	// boot ("sha256:<hex>"), or nil when it found no file. config.valid compares the file on
	// disk with it. Local mode ignores it.
	LoadedDigest *string
}

// probe is one health check. It must return when ctx ends; one that does not is abandoned
// when its timeout passes and its goroutine lingers until it returns.
type probe struct {
	id  string
	run func(ctx context.Context, s *state) contract.DoctorItem
}

// Runner runs the probes of one mode.
type Runner struct {
	source  string
	probes  []probe
	timeout time.Duration
	state   func() *state
}

// New returns a Runner for opts. An unknown mode is treated as local, the mode that needs
// nothing from a daemon.
func New(opts Options) *Runner {
	mode := opts.Mode
	if mode != ModeDaemon {
		mode = ModeLocal
	}
	r := &Runner{source: string(mode), timeout: ProbeTimeout}
	r.probes = registry(mode)
	r.state = func() *state { return newState(opts.Home, mode, opts.LoadedDigest) }
	return r
}

// registry lists the probes of a mode in report order.
func registry(mode Mode) []probe {
	all := []struct {
		probe
		daemon, local bool
	}{
		{probe{IDDaemonReachable, probeDaemonReachable}, false, true},
		{probe{IDHomePermissions, probeHomePermissions}, true, true},
		{probe{IDSocketPermissions, probeSocketPermissions}, true, false},
		{probe{IDConfigValid, probeConfigValid}, true, true},
		{probe{IDLedgerIntegrity, probeLedgerIntegrity}, true, true},
		{probe{IDLedgerSchema, probeLedgerSchema}, true, true},
		{probe{IDLogWritable, probeLogWritable}, true, true},
	}
	var out []probe
	for _, p := range all {
		if (mode == ModeDaemon && p.daemon) || (mode == ModeLocal && p.local) {
			out = append(out, p.probe)
		}
	}
	return out
}

// Run runs every probe and builds the report. It returns an error only when ctx ends
// before the probes finish; a probe that fails or times out is an item, not an error.
func (r *Runner) Run(ctx context.Context) (contract.Doctor, error) {
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel() // ends a shared inspection still running when the report is done
	s := r.state()
	s.ctx = runCtx

	items := make([]contract.DoctorItem, len(r.probes))
	var wg sync.WaitGroup
	for i, p := range r.probes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			items[i] = r.runOne(runCtx, s, p)
		}()
	}
	wg.Wait()
	if err := ctx.Err(); err != nil {
		return contract.Doctor{}, err
	}
	return report(r.source, items), nil
}

// runOne runs one probe under its timeout.
func (r *Runner) runOne(ctx context.Context, s *state, p probe) contract.DoctorItem {
	pctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()
	done := make(chan contract.DoctorItem, 1)
	go func() {
		item := p.run(pctx, s)
		item.ID = p.id
		done <- item
	}()
	select {
	case item := <-done:
		if pctx.Err() == nil {
			return item
		}
	case <-pctx.Done():
	}
	// Past the deadline an answer is not trusted either: a probe that honours its context
	// returns the moment the timeout fires, and its answer then describes the interruption.
	if errors.Is(pctx.Err(), context.DeadlineExceeded) {
		return contract.DoctorItem{
			ID:       p.id,
			Severity: contract.SeverityError,
			Message:  "timed out after " + r.timeout.String(),
		}
	}
	// The run itself was cancelled: Run reports that as an error and drops the items.
	return contract.DoctorItem{ID: p.id, Severity: contract.SeverityError, Message: "cancelled"}
}

// report assembles the document: the status is the worst severity, the summary the counts.
func report(source string, items []contract.DoctorItem) contract.Doctor {
	d := contract.Doctor{Status: contract.SeverityOK, Source: source, Items: items}
	for _, it := range items {
		switch it.Severity {
		case contract.SeverityError:
			d.Summary.Error++
		case contract.SeverityWarn:
			d.Summary.Warn++
		default:
			d.Summary.OK++
		}
	}
	switch {
	case d.Summary.Error > 0:
		d.Status = contract.SeverityError
	case d.Summary.Warn > 0:
		d.Status = contract.SeverityWarn
	}
	return d
}

// state is what the probes of one run share: the home, the mode, and one ledger inspection
// that the two ledger probes read between them.
type state struct {
	ctx          context.Context // the run's context, not a probe's
	home         home.Home
	mode         Mode
	loadedDigest *string

	inspectOnce sync.Once
	inspectDone chan struct{}
	inspection  ledger.Inspection
	inspectErr  error
}

func newState(h home.Home, mode Mode, loaded *string) *state {
	return &state{home: h, mode: mode, loadedDigest: loaded, inspectDone: make(chan struct{})}
}

// inspectLedger inspects the ledger once per run. Each caller waits under its own context,
// so a slow quick_check times out both ledger probes instead of blocking one of them.
func (s *state) inspectLedger(ctx context.Context) (ledger.Inspection, error) {
	s.inspectOnce.Do(func() {
		go func() {
			defer close(s.inspectDone)
			s.inspection, s.inspectErr = inspectFile(s.ctx, s.home.DBPath())
		}()
	})
	select {
	case <-s.inspectDone:
		return s.inspection, s.inspectErr
	case <-ctx.Done():
		return ledger.Inspection{}, ctx.Err()
	}
}

// inspectFile opens the ledger read-only. It reads the file first, so a mode that forbids
// it fails with a plain permission error instead of SQLite's generic "unable to open".
func inspectFile(ctx context.Context, path string) (ledger.Inspection, error) {
	f, err := os.Open(path)
	if err != nil {
		return ledger.Inspection{}, err
	}
	if err := f.Close(); err != nil {
		return ledger.Inspection{}, err
	}
	return ledger.Inspect(ctx, path)
}

func item(severity, format string, args ...any) contract.DoctorItem {
	return contract.DoctorItem{Severity: severity, Message: fmt.Sprintf(format, args...)}
}

func withFix(it contract.DoctorItem, format string, args ...any) contract.DoctorItem {
	it.Fix = fmt.Sprintf(format, args...)
	return it
}

// missing reports whether err says a file or directory is absent.
func missing(err error) bool { return errors.Is(err, fs.ErrNotExist) }
