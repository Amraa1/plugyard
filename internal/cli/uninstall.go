package cli

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/Amraa1/plugyard/internal/datadir"
	"github.com/Amraa1/plugyard/internal/store"
)

// dataFilePrefixes start the names of the files plugyard keeps in its data
// directory: those of sites (see datadir.SiteBase) and those of the API.
var dataFilePrefixes = []string{"s-", "api."}

// programFolder is the name of a folder that was made for plugyard alone,
// as the Windows install script makes one. Only such a folder is removed
// with the program: a folder of another name, such as ~/.local/bin or
// Go's bin folder, belongs to more than plugyard even while it is empty.
const programFolder = "plugyard"

// ownFolder reports whether the folder of the program is plugyard's own and
// holds nothing but the program, so that it can go with it.
func ownFolder(exe string) bool {
	dir := filepath.Dir(exe)
	if !strings.EqualFold(filepath.Base(dir), programFolder) {
		return false
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	for _, e := range entries {
		// What an update left behind goes with the program.
		if name := e.Name(); name != filepath.Base(exe) && name != filepath.Base(exe)+".old" {
			return false
		}
	}
	return true
}

// withoutPathEntry takes a folder out of a PATH value. The second result
// says whether it was there.
func withoutPathEntry(path, dir string, sep string) (string, bool) {
	clean := func(s string) string { return strings.ToLower(strings.TrimRight(strings.TrimSpace(s), `\/`)) }
	var kept []string
	found := false
	for _, entry := range strings.Split(path, sep) {
		if entry != "" && clean(entry) == clean(dir) {
			found = true
			continue
		}
		kept = append(kept, entry)
	}
	return strings.Join(kept, sep), found
}

// dataFiles lists what plugyard keeps in its data directory.
func dataFiles(dir string) []string {
	entries, _ := os.ReadDir(dir)
	var files []string
	for _, e := range entries {
		for _, prefix := range dataFilePrefixes {
			if strings.HasPrefix(e.Name(), prefix) {
				files = append(files, filepath.Join(dir, e.Name()))
			}
		}
	}
	return files
}

// removeReleased deletes a file of the data directory. A site or an API
// that was just stopped may hold its files a moment longer, and Windows
// does not delete a file that is open, so it tries for a few seconds.
func removeReleased(path string) error {
	deadline := time.Now().Add(5 * time.Second)
	for {
		err := os.RemoveAll(path)
		if err == nil || time.Now().After(deadline) {
			return err
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func newUninstall() *cobra.Command {
	var (
		yes      bool
		keepData bool
	)

	cmd := &cobra.Command{
		Use:   "uninstall",
		Short: "Remove plugyard from this machine",
		Long: "Remove the plugyard program, and the saved chargers and logs it keeps. On\n" +
			"Windows the entry the install script added to your PATH goes too. It shows\n" +
			"what it is about to remove and asks first.\n\n" +
			"Sites and the API that are running are stopped first; the question names\n" +
			"them. --keep-data leaves the saved chargers and logs, for when you plan to\n" +
			"install plugyard again.\n\n" +
			"The folder of the program is removed with it only when it is named\n" +
			"\"plugyard\" and holds nothing else; a folder plugyard shares with other\n" +
			"programs is left as it is, and so is your PATH.",
		Example: "  plugyard uninstall\n" +
			"  plugyard uninstall --keep-data\n" +
			"  plugyard uninstall --yes",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, out := cmd.Context(), cmd.OutOrStdout()

			dataDir, err := datadir.Path()
			if err != nil {
				return err
			}
			// Sites and the API hold the program open and write to the
			// data. They are stopped first, once the user has said yes.
			live, err := findRunning(ctx, dataDir)
			if err != nil {
				return err
			}

			exe, err := executablePath()
			if err != nil {
				return fmt.Errorf("cannot find the installed program: %w", err)
			}
			folder := ""
			if ownFolder(exe) {
				folder = filepath.Dir(exe)
			}
			onPath := folder != "" && onUserPath(folder)

			files := dataFiles(dataDir)
			sites, chargers := 0, 0
			if saved, err := store.All(dataDir); err == nil {
				sites = len(saved)
				for _, s := range saved {
					chargers += len(s.Chargers)
				}
			}
			saved := dataDir
			if sites > 0 {
				plural := "s"
				if sites == 1 {
					plural = ""
				}
				saved += fmt.Sprintf(" (%d site%s, %s)", sites, plural, countChargers(chargers))
			}

			fmt.Fprintln(out, "This removes plugyard from this machine:")
			rows := [][]cell{{plain("  the program"), plain(exe)}}
			if folder != "" {
				rows = append(rows, []cell{plain("  its folder"), plain(folder)})
			}
			if onPath {
				rows = append(rows, []cell{plain("  the entry on your PATH"), plain(folder)})
			}
			if len(files) > 0 && !keepData {
				rows = append(rows, []cell{plain("  saved chargers and logs"), plain(saved)})
			}
			writeTable(out, rows)
			if len(files) > 0 && keepData {
				fmt.Fprintf(out, "Your saved chargers and logs are kept: %s\n", saved)
			}

			if live.any() {
				fmt.Fprintf(out, "Running now, and stopped first: %s.%s\n", live.describe(), live.ending())
			}

			if !yes {
				if !stdinIsTerminal() {
					return errors.New("there is nobody to ask; run again with --yes to remove plugyard. Nothing was removed")
				}
				fmt.Fprint(out, "Remove plugyard? [y/N] ")
				answer, _ := bufio.NewReader(stdin).ReadString('\n')
				switch strings.ToLower(strings.TrimSpace(answer)) {
				case "y", "yes":
				default:
					return errors.New("nothing was removed")
				}
			}

			if err := live.stop(ctx, dataDir, out); err != nil {
				return fmt.Errorf("%w. Nothing was removed", err)
			}

			// The program goes first: it is the one thing that may not be
			// ours to remove, and then nothing else is touched.
			finish, err := removeProgram(exe, folder != "")
			if err != nil {
				hint := "Delete that file yourself, or run the command from a terminal that may write there"
				if errors.Is(err, os.ErrPermission) && os.PathSeparator == '/' {
					hint = "Run \"sudo plugyard uninstall\", or delete that file yourself"
				}
				return fmt.Errorf("cannot remove %s: %w. %s. Nothing was removed", exe, err, hint)
			}

			var problems []string
			if !keepData {
				for _, f := range files {
					if err := removeReleased(f); err != nil {
						problems = append(problems, err.Error())
					}
				}
				// The folder goes only when nothing else is in it.
				_ = os.Remove(dataDir)
			}
			if onPath {
				if err := removeFromUserPath(folder); err != nil {
					problems = append(problems, fmt.Sprintf("could not take %s off your PATH: %v", folder, err))
				}
			}
			note := finish()

			fmt.Fprintln(out, "plugyard is removed.")
			if note != "" {
				fmt.Fprintln(out, note)
			}
			if len(problems) > 0 {
				for _, p := range problems {
					fmt.Fprintf(cmd.ErrOrStderr(), "plugyard: %s\n", p)
				}
				return errFailed
			}
			return nil
		},
	}
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "do not ask")
	cmd.Flags().BoolVar(&keepData, "keep-data", false, "leave the saved chargers and logs")
	return cmd
}
