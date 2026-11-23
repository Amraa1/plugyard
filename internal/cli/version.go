package cli

import (
	"fmt"
	"runtime"
	"strings"

	"github.com/spf13/cobra"

	"github.com/Amraa1/plugyard/internal/buildinfo"
)

func newVersion() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the version and supported OCPP versions",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "Version:  %s\n", buildinfo.Version())
			fmt.Fprintf(out, "OCPP:     %s\n", strings.Join(buildinfo.OCPPVersions, ", "))
			fmt.Fprintf(out, "Go:       %s\n", runtime.Version())
			fmt.Fprintf(out, "OS/Arch:  %s/%s\n", runtime.GOOS, runtime.GOARCH)
			return nil
		},
	}
}
