package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/Amraa1/plugyard/internal/buildinfo"
)

func newVersion() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the version",
		Long: "Print the version of plugyard and nothing else, so that a script can use\n" +
			"it as it is. \"plugyard info\" says more: the OCPP versions, the Go version\n" +
			"and the system plugyard was built for.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			fmt.Fprintln(cmd.OutOrStdout(), buildinfo.Version())
			return nil
		},
	}
}
