package cli

import (
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/felipmateus/harness/internal/api/contract"
)

func (a *app) newStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show the state of the running daemon",
		Args:  cobra.NoArgs,
		RunE: a.run(func(cmd *cobra.Command) error {
			h, err := a.resolveHome()
			if err != nil {
				return err
			}
			st, err := a.newClient(h.SocketPath()).Status(cmd.Context())
			if err != nil {
				return err
			}
			return a.printStatus(st)
		}),
	}
}

func (a *app) printStatus(st contract.Status) error {
	p := a.printer()
	if p.mode.structured() {
		return p.document(contract.StatusResponse{Status: st})
	}
	row := func(key, value string) { p.line("%-10s%s", key, value) }
	row("State", st.State)
	row("PID", fmt.Sprint(st.PID))
	row("Version", st.Version)
	row("Uptime", (time.Duration(st.UptimeSeconds) * time.Second).String())
	row("Home", st.Home)
	row("Ledger", fmt.Sprintf("%s (schema %d, %s, %s)", st.Ledger.Path, st.Ledger.SchemaVersion,
		plural(st.Ledger.Streams, "stream"), plural(st.Ledger.Events, "event")))
	row("Config", configSummary(st.Config))
	return nil
}

func plural(n int64, noun string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, noun)
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

func configSummary(c contract.ConfigStatus) string {
	if !c.Exists {
		return c.Path + " (not present, defaults in use)"
	}
	digest := "no digest"
	if c.Digest != nil {
		digest = strings.TrimSpace(*c.Digest)
	}
	return fmt.Sprintf("%s (present, %s)", c.Path, digest)
}
