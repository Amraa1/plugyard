// Package cli defines the plugyard command line.
package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"
)

// Exit codes, as defined in docs/SPEC.md.
const (
	ExitOK      = 0
	ExitFailure = 1 // runtime failure
	ExitUsage   = 2 // usage or validation error
)

// UsageError marks an error raised by a command as a usage or validation
// error, so that Run exits with ExitUsage instead of ExitFailure.
type UsageError struct{ Err error }

func (e *UsageError) Error() string { return e.Err.Error() }
func (e *UsageError) Unwrap() error { return e.Err }

// errFailed makes Run exit with ExitFailure without printing anything more.
// A command returns it after it has already reported what went wrong.
var errFailed = errors.New("failed")

// Run executes plugyard with the given arguments (without the program name)
// and returns the process exit code. Ctrl+C or a termination signal asks the
// running command to shut down in an orderly way.
func Run(args []string, stdout, stderr io.Writer) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return RunContext(ctx, args, stdout, stderr)
}

// RunContext is Run with a caller-supplied context; cancelling it asks the
// running command to shut down.
func RunContext(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	// started becomes true once cobra has accepted the command, flags and
	// arguments. An error before that point is a usage error.
	started := false

	root := newRoot()
	root.SetArgs(args)
	root.SetOut(stdout)
	root.SetErr(stderr)
	markStarted(root, &started)

	err := root.ExecuteContext(ctx)
	if err == nil {
		return ExitOK
	}

	if errors.Is(err, errFailed) {
		return ExitFailure
	}

	fmt.Fprintf(stderr, "plugyard: %v\n", err)

	var usage *UsageError
	if !started || errors.As(err, &usage) {
		fmt.Fprintln(stderr, "Run 'plugyard --help' for usage.")
		return ExitUsage
	}
	return ExitFailure
}

// markStarted wraps the run function of cmd and of every command below it so
// that *started is set once cobra has finished its own checks.
func markStarted(cmd *cobra.Command, started *bool) {
	if run := cmd.RunE; run != nil {
		cmd.RunE = func(c *cobra.Command, args []string) error {
			*started = true
			return run(c, args)
		}
	}
	for _, sub := range cmd.Commands() {
		markStarted(sub, started)
	}
}

func newRoot() *cobra.Command {
	root := &cobra.Command{
		Use:   "plugyard",
		Short: "Simulate EV chargers for testing a CSMS",
		Long: "plugyard simulates EV chargers speaking OCPP, so you can develop and\n" +
			"test a CSMS without hardware.",
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
	}
	root.CompletionOptions.DisableDefaultCmd = true

	root.AddCommand(newExec(), newPs(), newRun(), newSite(), newVersion())
	root.AddCommand(newPowerCommands()...)
	return root
}
