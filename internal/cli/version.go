package cli

import (
	"github.com/spf13/cobra"

	"github.com/felipmateus/harness/internal/version"
)

type versionDocument struct {
	Version version.Info `json:"version"`
}

func (a *app) newVersionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the version of this binary",
		Args:  cobra.NoArgs,
		RunE: a.run(func(*cobra.Command) error {
			info := version.Get()
			p := a.printer()
			if p.mode.structured() {
				return p.document(versionDocument{Version: info})
			}
			p.line("harness %s (commit %s, %s, %s)", info.Version, info.Commit, info.Go, info.Platform)
			return nil
		}),
	}
}
