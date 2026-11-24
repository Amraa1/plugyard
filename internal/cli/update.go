package cli

import (
	"bufio"
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
	"github.com/Amraa1/plugyard/internal/datadir"
	"github.com/Amraa1/plugyard/internal/selfupdate"
)

// updateURLEnv points "plugyard update" at another copy of the releases,
// such as a mirror inside a company network.
const updateURLEnv = "PLUGYARD_RELEASES_URL"

// containerEnv is set in plugyard's container image. A program in an image
// is replaced by pulling a newer image, and removed with its container.
const containerEnv = "PLUGYARD_CONTAINER"

// containerImage is the name of plugyard's image on Docker Hub.
const containerImage = "amraa1/plugyard"

func inContainer() bool { return os.Getenv(containerEnv) != "" }

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
	var (
		check bool
		yes   bool
	)

	cmd := &cobra.Command{
		Use:   "update",
		Short: "Update plugyard to the latest release",
		Long: "Download the latest release of plugyard for this system, check it against\n" +
			"the release's checksums, and put it in place of the program that is\n" +
			"running. Saved state and logs are kept.\n\n" +
			"A site started by one version cannot be controlled by another, so sites\n" +
			"and the API that are running are stopped for the update and started again\n" +
			"on the new version, in the background. plugyard says which and asks\n" +
			"first; --yes answers in advance. Transactions that are running end with\n" +
			"StopTransaction. --check only says whether a newer release exists.",
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
			if inContainer() {
				// An update inside a container is gone with the container.
				tag := strings.TrimPrefix(latest, "v")
				if check {
					fmt.Fprintf(out, "plugyard runs from a container image here. Pull the new one: docker pull %s:%s\n", containerImage, tag)
					return nil
				}
				return fmt.Errorf("plugyard runs from a container image here, and an update inside the container would be lost with it. "+
					"Pull the new image instead: docker pull %s:%s", containerImage, tag)
			}
			if check {
				fmt.Fprintln(out, `Run "plugyard update" to install it.`)
				return nil
			}

			// A site, and the API, keep running the old program, and the
			// new one may not be able to talk to them. They are stopped
			// for the update and started again on the new version.
			dir, err := datadir.Path()
			if err != nil {
				return err
			}
			live, err := findRunning(ctx, dir)
			if err != nil {
				return err
			}
			if live.any() && !yes {
				fmt.Fprintf(out, "Running now: %s.\n", live.describe())
				fmt.Fprintf(out, "To update, they are stopped and started again on %s, in the background.%s\n", latest, live.ending())
				if !stdinIsTerminal() {
					return errors.New("there is nobody to ask; run again with --yes to restart them on the new version, or stop them first. Nothing was changed")
				}
				fmt.Fprintf(out, "Update and restart them? [y/N] ")
				answer, _ := bufio.NewReader(stdin).ReadString('\n')
				switch strings.ToLower(strings.TrimSpace(answer)) {
				case "y", "yes":
				default:
					return errors.New("nothing was changed")
				}
			}

			exe, err := executablePath()
			if err != nil {
				return fmt.Errorf("cannot find the installed program: %w", err)
			}
			// The download comes first: if it fails, nothing was stopped.
			fmt.Fprintf(out, "Downloading %s\n", selfupdate.AssetName(u.OS, u.Arch))
			program, err := u.Download(ctx, latest)
			if err != nil {
				return err
			}
			if err := live.stop(ctx, dir, out); err != nil {
				return fmt.Errorf("%w. Nothing was changed", err)
			}
			if err := selfupdate.Replace(exe, program); err != nil {
				// What was stopped comes back on the program that is
				// still there.
				failed := live.start(cmd, dir, out)
				for _, f := range failed {
					fmt.Fprintf(cmd.ErrOrStderr(), "plugyard: %s\n", f)
				}
				if errors.Is(err, os.ErrPermission) {
					return fmt.Errorf("cannot replace %s: %w. Run the command from a terminal that may write there, or install again with the install script", exe, err)
				}
				return fmt.Errorf("cannot replace %s: %w", exe, err)
			}
			fmt.Fprintf(out, "Updated plugyard to %s.\nWhat changed: %s/releases/tag/%s\n", latest, base, latest)

			if failed := live.start(cmd, dir, out); len(failed) > 0 {
				for _, f := range failed {
					fmt.Fprintf(cmd.ErrOrStderr(), "plugyard: %s\n", f)
				}
				return errFailed
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&check, "check", false, "only say whether a newer release exists")
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "restart running sites and the API on the new version, without asking")
	return cmd
}
