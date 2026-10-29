// Package cli is the harness command line: the cobra command tree, the output rules and
// exit codes of `_dx.md`, the client of the daemon's Unix-socket API, and the pre-spawn
// home checks and process handling of a detached start.
//
// It talks to a running daemon only through the API contract. The one exception is
// `daemon start --foreground`, which runs the daemon in this process through daemon.Run.
package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"

	"github.com/spf13/cobra"

	"github.com/felipmateus/harness/internal/api/contract"
	"github.com/felipmateus/harness/internal/daemon"
	"github.com/felipmateus/harness/internal/home"
)

// Execute runs the command line in args (without the program name), writes to stdout and
// stderr, and returns the process exit code.
func Execute(args []string, stdout, stderr io.Writer) int {
	return newApp(stdout, stderr).execute(context.Background(), args)
}

// app is one invocation of the CLI: its streams, the dependencies that cross a process or
// time boundary (so tests can replace them), and the values of the global flags.
type app struct {
	stdout, stderr io.Writer

	clock      clock
	newClient  func(socket string) daemonAPI
	spawner    spawner
	probe      func(home.Home) homeProbe
	executable func() (string, error)
	runDaemon  func(ctx context.Context, opts daemon.Options) error
	// interrupt returns a context that ends on Ctrl-C.
	interrupt func(ctx context.Context) (context.Context, context.CancelFunc)

	outputFlag string
	homeFlag   string
	mode       outputMode

	// atExit holds what a command takes for the life of the process and gives back once
	// execute is done, the error report included. It runs in reverse order.
	atExit []func()
}

func newApp(stdout, stderr io.Writer) *app {
	return &app{
		stdout:     stdout,
		stderr:     stderr,
		clock:      realClock{},
		newClient:  func(socket string) daemonAPI { return newClient(socket) },
		spawner:    execSpawner{},
		probe:      func(h home.Home) homeProbe { return fsProbe{h} },
		executable: os.Executable,
		runDaemon:  daemon.Run,
		interrupt: func(ctx context.Context) (context.Context, context.CancelFunc) {
			return signal.NotifyContext(ctx, os.Interrupt)
		},
		mode: outHuman,
	}
}

func (a *app) printer() printer { return printer{mode: a.mode, out: a.stdout, err: a.stderr} }

// resolveHome applies --home, then $HARNESS_HOME, then ~/.harness.
func (a *app) resolveHome() (home.Home, error) { return home.Resolve(a.homeFlag) }

func (a *app) newRoot() *cobra.Command {
	root := &cobra.Command{
		Use:           "harness",
		Short:         "harness runs agent CLIs and records everything in a durable event ledger",
		SilenceUsage:  true,
		SilenceErrors: true,
		PersistentPreRunE: func(cmd *cobra.Command, _ []string) error {
			mode, ok := parseOutputMode(a.outputFlag)
			if !ok {
				return usageError(cmd.CommandPath(),
					fmt.Sprintf("invalid output format %q: must be one of human, json, jsonl", a.outputFlag))
			}
			a.mode = mode
			return nil
		},
	}
	root.SetOut(a.stdout)
	root.SetErr(a.stderr)
	root.CompletionOptions.DisableDefaultCmd = true
	root.PersistentFlags().StringVarP(&a.outputFlag, "output", "o", string(outHuman), "output format: human, json or jsonl")
	root.PersistentFlags().StringVar(&a.homeFlag, "home", "", "harness home directory (default $HARNESS_HOME, else ~/.harness)")
	root.AddCommand(a.newDaemonCmd(), a.newStatusCmd(), a.newEventsCmd(), a.newDoctorCmd(), a.newConfigCmd(), a.newVersionCmd())
	return root
}

// execute parses args, runs the command and returns the exit code.
func (a *app) execute(ctx context.Context, args []string) int {
	defer a.runAtExit()
	root := a.newRoot()
	root.SetArgs(args)
	root.SetContext(ctx)
	cmd, err := root.ExecuteC()
	if err == nil {
		return exitOK
	}
	return a.report(cmd, args, err)
}

// holdUntilExit registers release to run when execute is done: after the command has
// returned and its error, if any, has been written.
func (a *app) holdUntilExit(release func()) { a.atExit = append(a.atExit, release) }

func (a *app) runAtExit() {
	for i := len(a.atExit) - 1; i >= 0; i-- {
		a.atExit[i]()
	}
	a.atExit = nil
}

// report writes a failed command's error under the output rules and returns its exit code.
func (a *app) report(cmd *cobra.Command, args []string, err error) int {
	if errors.Is(err, errInterrupted) {
		return exitInterrupted
	}
	var silent *silentExit
	if errors.As(err, &silent) {
		return silent.code
	}
	// The mode comes from the raw arguments: when the command line is wrong, flag parsing
	// may have stopped before -o was read.
	p := printer{mode: outputModeFromArgs(args), out: a.stdout, err: a.stderr}
	var wire *contract.Error
	if isWireError(err) {
		wire = asWireError(err)
	} else {
		// Only cobra's own failures reach here untyped: bad flags, bad arguments, an
		// unknown command. Every error a command returns is typed by run.
		wire = usageError(cmd.CommandPath(), err.Error())
	}
	if wire.Code == contract.CodeUsage && !p.mode.structured() {
		fmt.Fprintf(a.stderr, "error: %s\n\n%s", wire.Message, cmd.UsageString())
	} else {
		p.failure(wire)
	}
	return exitCodeFor(wire.Code)
}

// run adapts a command body to cobra: whatever it returns is a typed error or nil, so the
// error path can tell the commands' failures from cobra's usage errors.
func (a *app) run(fn func(cmd *cobra.Command) error) func(*cobra.Command, []string) error {
	return func(cmd *cobra.Command, _ []string) error {
		err := fn(cmd)
		var silent *silentExit
		if err == nil || errors.Is(err, errInterrupted) || errors.As(err, &silent) || isWireError(err) {
			return err
		}
		return asWireError(err)
	}
}

// isWireError reports whether err is one of the typed errors asWireError understands.
func isWireError(err error) bool {
	var wire *contract.Error
	var homeErr *home.Error
	var exitErr *daemon.ExitError
	return errors.As(err, &wire) || errors.As(err, &homeErr) || errors.As(err, &exitErr)
}
