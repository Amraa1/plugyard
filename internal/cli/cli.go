// Package cli defines the plugyard command line.
package cli

import (
	"errors"
	"fmt"
	"io"

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

// Run executes plugyard with the given arguments (without the program name)
// and returns the process exit code.
func Run(args []string, stdout, stderr io.Writer) int {
	// started becomes true once cobra has accepted the command, flags and
	// arguments. An error before that point is a usage error.
	started := false

	root := newRoot()
	root.SetArgs(args)
	root.SetOut(stdout)
	root.SetErr(stderr)
	root.PersistentPreRun = func(*cobra.Command, []string) { started = true }

	err := root.Execute()
	if err == nil {
		return ExitOK
	}

	fmt.Fprintf(stderr, "plugyard: %v\n", err)

	var usage *UsageError
	if !started || errors.As(err, &usage) {
		fmt.Fprintln(stderr, "Run 'plugyard --help' for usage.")
		return ExitUsage
	}
	return ExitFailure
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

	root.AddCommand(newVersion())
	return root
}
