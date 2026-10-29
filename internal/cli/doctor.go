package cli

import (
	"strings"

	"github.com/spf13/cobra"

	"github.com/felipmateus/harness/internal/api/contract"
	"github.com/felipmateus/harness/internal/doctor"
)

// exitReportError is the exit code of a report that holds an error: a doctor report with
// an error item, and (as exitConfig) a config validation report with problems. The report
// itself is the output, so these exits print nothing more.
const exitReportError = 70

func (a *app) newDoctorCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "doctor",
		Short: "Check the home, config, ledger and log, with or without a daemon",
		Long: "Run the health checks. When a daemon answers they run inside it; otherwise they run\n" +
			"here, against the files, and daemon.reachable says so. doctor reports problems and\n" +
			"suggests fixes; it never changes a file.\n\n" +
			"Exit 0 when no item is an error, 70 when one is. The report is on stdout either way.",
		Args: cobra.NoArgs,
		RunE: a.run(func(cmd *cobra.Command) error {
			h, err := a.resolveHome()
			if err != nil {
				return err
			}
			ctx := cmd.Context()
			rep, err := a.newClient(h.SocketPath()).Doctor(ctx)
			if isUnavailable(err) {
				// No daemon answers: this is exactly when doctor must still work.
				rep, err = doctor.New(doctor.Options{Home: h, Mode: doctor.ModeLocal}).Run(ctx)
				if err != nil && ctx.Err() != nil {
					return errInterrupted
				}
			}
			if err != nil {
				return err
			}
			a.printDoctor(rep)
			if rep.Summary.Error > 0 || rep.Status == contract.SeverityError {
				return &silentExit{code: exitReportError}
			}
			return nil
		}),
	}
}

// printDoctor writes the report: one JSON document, or one line per item.
func (a *app) printDoctor(rep contract.Doctor) {
	p := a.printer()
	if rep.Items == nil {
		rep.Items = []contract.DoctorItem{}
	}
	if p.mode.structured() {
		_ = p.document(contract.DoctorResponse{Doctor: rep})
		return
	}
	for _, it := range rep.Items {
		p.line("%-6s%-21s%s", strings.ToUpper(it.Severity), it.ID, it.Message)
		if it.Fix != "" {
			p.line("      fix: %s", it.Fix)
		}
	}
	p.line("summary: %d ok, %d warn, %d error", rep.Summary.OK, rep.Summary.Warn, rep.Summary.Error)
}
