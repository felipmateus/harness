package cli

import (
	"errors"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/felipmateus/harness/internal/api/contract"
	"github.com/felipmateus/harness/internal/config"
)

// The config verbs never talk to a daemon: they read <home>/config.toml themselves.
func (a *app) newConfigCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "config",
		Short: "Inspect and validate the optional config file",
		// Runnable, like `daemon`, so an unknown verb is a usage error.
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error { return cmd.Help() },
	}
	cmd.AddCommand(a.newConfigPathCmd(), a.newConfigValidateCmd(), a.newConfigShowCmd())
	return cmd
}

// pathDocument, validateDocument and showDocument are the -o json shapes.
type pathDocument struct {
	Config struct {
		Path string `json:"path"`
	} `json:"config"`
}

type validateDocument struct {
	Config validateReport `json:"config"`
}

type validateReport struct {
	Path   string              `json:"path"`
	Valid  bool                `json:"valid"`
	Errors []config.FieldError `json:"errors"`
}

type showDocument struct {
	Config config.Config `json:"config"`
}

func (a *app) newConfigPathCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "path",
		Short: "Print the path of the config file, which may not exist",
		Args:  cobra.NoArgs,
		RunE: a.run(func(*cobra.Command) error {
			h, err := a.resolveHome()
			if err != nil {
				return err
			}
			p := a.printer()
			if p.mode.structured() {
				var doc pathDocument
				doc.Config.Path = h.ConfigPath()
				return p.document(doc)
			}
			p.line("%s", h.ConfigPath())
			return nil
		}),
	}
}

func (a *app) newConfigValidateCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "validate",
		Short: "Check the config file and list every problem",
		Long: "Check <home>/config.toml with the strict rules the daemon applies at start and list\n" +
			"every problem, not just the first. A missing file is valid: defaults apply.\n\n" +
			"Exit 0 when valid, 78 when not. The report is on stdout either way.",
		Args: cobra.NoArgs,
		RunE: a.run(func(*cobra.Command) error {
			h, err := a.resolveHome()
			if err != nil {
				return err
			}
			loaded, err := config.Load(h.ConfigPath())
			var invalid *config.InvalidError
			switch {
			case errors.As(err, &invalid):
				a.printValidation(validateReport{Path: invalid.Path, Errors: invalid.Errors}, true)
				return &silentExit{code: exitConfig}
			case err != nil:
				return err
			}
			a.printValidation(validateReport{Path: loaded.Path, Valid: true, Errors: []config.FieldError{}}, loaded.Exists)
			return nil
		}),
	}
}

// printValidation writes the validation report. exists only shapes the human text: a missing
// file is valid, and the line says defaults are in use.
func (a *app) printValidation(rep validateReport, exists bool) {
	p := a.printer()
	if p.mode.structured() {
		_ = p.document(validateDocument{Config: rep})
		return
	}
	if rep.Valid {
		if !exists {
			p.line("valid: %s (not present, defaults in use)", rep.Path)
			return
		}
		p.line("valid: %s", rep.Path)
		return
	}
	p.line("invalid: %s", rep.Path)
	for _, e := range rep.Errors {
		p.line("  - %s (%s)", e.Message, e.Code)
	}
}

func (a *app) newConfigShowCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "show",
		Short: "Print the effective configuration, defaults merged",
		Args:  cobra.NoArgs,
		RunE: a.run(func(*cobra.Command) error {
			h, err := a.resolveHome()
			if err != nil {
				return err
			}
			loaded, err := config.Load(h.ConfigPath())
			var invalid *config.InvalidError
			switch {
			case errors.As(err, &invalid):
				// Nothing effective to show: an error, with the validate verb as the way on.
				return &contract.Error{
					Code:    contract.CodeConfigInvalid,
					Message: "config invalid: " + invalid.Error(),
					Hint:    "run `harness config validate`",
				}
			case err != nil:
				return err
			}
			p := a.printer()
			if p.mode.structured() {
				return p.document(showDocument{Config: loaded.Config})
			}
			grace, _ := loaded.Config.Daemon.ShutdownGrace.MarshalText()
			p.line("[daemon]")
			p.line("shutdown_grace = %s", quote(string(grace)))
			p.line("")
			p.line("[log]")
			p.line("level = %s", quote(loaded.Config.Log.Level))
			return nil
		}),
	}
}

// quote writes a TOML basic string.
func quote(s string) string {
	return fmt.Sprintf("%q", strings.TrimSpace(s))
}
