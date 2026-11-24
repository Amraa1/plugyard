package cli

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/Amraa1/plugyard/internal/buildinfo"
	"github.com/Amraa1/plugyard/internal/selfupdate"
)

// updateURLEnv points "plugyard update" at another copy of the releases,
// such as a mirror inside a company network.
const updateURLEnv = "PLUGYARD_RELEASES_URL"

// These are variables so that tests can stand in for the installed program.
var (
	currentVersion = buildinfo.Version
	executablePath = func() (string, error) {
		exe, err := os.Executable()
		if err != nil {
			return "", err
		}
		return filepath.EvalSymlinks(exe)
	}
)

// cleanAfterUpdate removes the program an earlier update replaced, which
// Windows could not delete while it was running.
func cleanAfterUpdate() {
	if exe, err := executablePath(); err == nil {
		selfupdate.CleanOld(exe)
	}
}

func newUpdate() *cobra.Command {
	var check bool

	cmd := &cobra.Command{
		Use:   "update",
		Short: "Update plugyard to the latest release",
		Long: "Download the latest release of plugyard for this system, check it against\n" +
			"the release's checksums, and put it in place of the program that is\n" +
			"running. Saved state and logs are kept.\n\n" +
			"Running sites must be stopped first: a site started by one version cannot\n" +
			"be controlled by another. --check only says whether a newer release exists.",
		Example: "  plugyard update --check\n" +
			"  plugyard update",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, out := cmd.Context(), cmd.OutOrStdout()
			current := currentVersion()

			base := strings.TrimRight(os.Getenv(updateURLEnv), "/")
			if base == "" {
				base = selfupdate.DefaultBase
			}
			u := &selfupdate.Updater{
				Base:      base,
				Client:    &http.Client{Timeout: 5 * time.Minute},
				OS:        runtime.GOOS,
				Arch:      runtime.GOARCH,
				UserAgent: "plugyard/" + current,
			}

			latest, err := u.Latest(ctx)
			if err != nil {
				return err
			}
			if !selfupdate.Newer(current, latest) {
				fmt.Fprintf(out, "plugyard %s is the latest version.\n", current)
				return nil
			}
			fmt.Fprintf(out, "plugyard %s is installed; %s is available.\n", current, latest)
			if check {
				fmt.Fprintln(out, `Run "plugyard update" to install it.`)
				return nil
			}

			// A site keeps running the old program, and the new one may not
			// be able to talk to it.
			w, err := loadWorld(ctx, "")
			if err != nil {
				return err
			}
			var running []string
			for _, client := range w.clients {
				running = append(running, client.Info.Site)
			}
			w.close()
			if len(running) > 0 {
				what := "site " + running[0] + " is"
				if len(running) > 1 {
					what = "sites " + joinWords(running) + " are"
				}
				return fmt.Errorf("%s running; stop every site first with \"plugyard site stop -s <name>\", then update. Nothing was changed", what)
			}

			exe, err := executablePath()
			if err != nil {
				return fmt.Errorf("cannot find the installed program: %w", err)
			}
			fmt.Fprintf(out, "Downloading %s\n", selfupdate.AssetName(u.OS, u.Arch))
			program, err := u.Download(ctx, latest)
			if err != nil {
				return err
			}
			if err := selfupdate.Replace(exe, program); err != nil {
				if errors.Is(err, os.ErrPermission) {
					return fmt.Errorf("cannot replace %s: %w. Run the command from a terminal that may write there, or install again with the install script", exe, err)
				}
				return fmt.Errorf("cannot replace %s: %w", exe, err)
			}
			fmt.Fprintf(out, "Updated plugyard to %s.\nWhat changed: %s/releases/tag/%s\n", latest, base, latest)
			return nil
		},
	}
	cmd.Flags().BoolVar(&check, "check", false, "only say whether a newer release exists")
	return cmd
}
