package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/Amraa1/plugyard/internal/charger"
	"github.com/Amraa1/plugyard/internal/control"
	"github.com/Amraa1/plugyard/internal/datadir"
	"github.com/Amraa1/plugyard/internal/site"
	"github.com/Amraa1/plugyard/internal/sitefile"
)

// detachedEnv marks the background copy of plugyard that "site up -d"
// starts.
const detachedEnv = "PLUGYARD_DETACHED"

// detachTimeout is how long "site up -d" waits for the background copy to
// report that the site is running.
const detachTimeout = 30 * time.Second

func newSite() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "site",
		Short: "Start and stop the chargers of a site file together",
		Long: "A site is a named group of chargers declared in a site file, by default\n" +
			"site.yaml in the current directory, and started and stopped together.",
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) > 0 {
				return &UsageError{Err: fmt.Errorf("unknown command %q for \"plugyard site\"", args[0])}
			}
			return cmd.Help()
		},
	}
	cmd.AddCommand(newSiteUp(), newSiteDown(), newSiteLs(), newSiteValidate())
	return cmd
}

// addFileFlag adds -f, --file to a site command.
func addFileFlag(cmd *cobra.Command, file *string) {
	cmd.Flags().StringVarP(file, "file", "f", sitefile.DefaultName, "site file")
}

// loadSiteFile reads a site file. Anything wrong with it is a validation
// error.
func loadSiteFile(path string) (*sitefile.Site, error) {
	sf, err := sitefile.Load(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) && path == sitefile.DefaultName {
			return nil, &UsageError{Err: fmt.Errorf("there is no %s in this directory; name a site file with -f", sitefile.DefaultName)}
		}
		return nil, &UsageError{Err: err}
	}
	return sf, nil
}

// buildSite creates the chargers a site file declares, without starting
// them.
func buildSite(sf *sitefile.Site, log func(charger.Entry)) (*site.Site, error) {
	pacer := site.NewPacer(sf.ConnectRate)

	var chargers []*charger.Charger
	var urls []string
	for _, entry := range sf.Entries {
		for _, spec := range entry.Chargers {
			c, err := charger.New(charger.Config{
				ID:         spec.ID,
				Model:      spec.Model,
				CSMS:       spec.CSMS,
				Speed:      sf.Speed,
				BeforeDial: pacer,
				Log:        log,
			})
			if err != nil {
				return nil, &UsageError{Err: fmt.Errorf("chargers.%s: %w", entry.Name, err)}
			}
			chargers = append(chargers, c)
			urls = append(urls, spec.CSMS.URL)
		}
	}

	s := site.New(sf.Name, chargers...)
	s.SetCSMS(urls)
	s.SetPresets(sf.EVs)
	for _, spec := range sf.Chargers() {
		s.SetDefaultEV(spec.ID, spec.EV)
	}
	return s, nil
}

// serveSite runs a site in this process until ctx is cancelled or a command
// asks it to shut down, answering other commands on its socket meanwhile.
func serveSite(ctx context.Context, dataDir string, s *site.Site) error {
	ctx, shutdown := context.WithCancel(ctx)
	defer shutdown()

	server, err := control.Serve(dataDir, s, shutdown)
	if err != nil {
		return err
	}
	defer server.Close()

	return s.Run(ctx)
}

func newSiteUp() *cobra.Command {
	var (
		file        string
		detach      bool
		speed       float64
		connectRate float64
	)

	cmd := &cobra.Command{
		Use:   "up",
		Short: "Start the chargers of a site",
		Long: "Create the chargers a site file declares and start them. The site runs in\n" +
			"the foreground showing the log of every charger; press Ctrl+C to shut it\n" +
			"down. With -d it runs in the background instead, and \"plugyard site down\"\n" +
			"shuts it down.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			sf, err := loadSiteFile(file)
			if err != nil {
				return err
			}
			if cmd.Flags().Changed("speed") {
				if speed <= 0 {
					return &UsageError{Err: errors.New("--speed must be above 0")}
				}
				sf.Speed = speed
			}
			if cmd.Flags().Changed("connect-rate") {
				if connectRate < 0 {
					return &UsageError{Err: errors.New("--connect-rate cannot be negative; use 0 for no limit")}
				}
				sf.ConnectRate = connectRate
			}

			out := cmd.OutOrStdout()
			s, err := buildSite(sf, newLogPrinter(out))
			if err != nil {
				return err
			}
			count := len(s.Chargers())
			if err := checkFileLimit(count); err != nil {
				return err
			}

			dir, err := datadir.Path()
			if err != nil {
				return err
			}

			if detach {
				var passthrough []string
				if cmd.Flags().Changed("speed") {
					passthrough = append(passthrough, "--speed", strconv.FormatFloat(speed, 'f', -1, 64))
				}
				if cmd.Flags().Changed("connect-rate") {
					passthrough = append(passthrough, "--connect-rate", strconv.FormatFloat(connectRate, 'f', -1, 64))
				}
				return startDetached(cmd.Context(), out, dir, sf, passthrough)
			}

			fmt.Fprintf(out, "Starting site %s with %s\n", sf.Name, countChargers(count))
			return serveSite(cmd.Context(), dir, s)
		},
	}

	addFileFlag(cmd, &file)
	cmd.Flags().BoolVarP(&detach, "detach", "d", false, "run the site in the background")
	cmd.Flags().Float64Var(&speed, "speed", 1, "multiply the rate of EV charging and energy accumulation")
	cmd.Flags().Float64Var(&connectRate, "connect-rate", 0, "most new connections to open per second; 0 means no limit")
	return cmd
}

// startDetached starts a background copy of plugyard that runs the site,
// waits until it answers, and reports.
func startDetached(ctx context.Context, out io.Writer, dataDir string, sf *sitefile.Site, passthrough []string) error {
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return err
	}
	if client, err := control.Probe(ctx, dataDir, sf.Name); err == nil {
		client.Close()
		return fmt.Errorf("site %q: %w", sf.Name, control.ErrAlreadyRunning)
	}

	// The background copy has no terminal, so what it prints goes to a file
	// beside the site's socket.
	logPath := strings.TrimSuffix(control.SocketPath(dataDir, sf.Name), ".sock") + ".log"
	logFile, err := os.Create(logPath)
	if err != nil {
		return err
	}
	defer logFile.Close()

	exe, err := os.Executable()
	if err != nil {
		return err
	}
	args := append([]string{"site", "up", "-f", sf.Path}, passthrough...)
	child := exec.Command(exe, args...)
	child.Stdout = logFile
	child.Stderr = logFile
	child.Env = append(os.Environ(), detachedEnv+"=1")
	child.Dir = filepath.Dir(sf.Path)
	setDetached(child)
	if err := child.Start(); err != nil {
		return fmt.Errorf("cannot start the background process: %w", err)
	}

	exited := make(chan error, 1)
	go func() { exited <- child.Wait() }()

	deadline := time.After(detachTimeout)
	for {
		if client, err := control.Probe(ctx, dataDir, sf.Name); err == nil {
			info := client.Info
			client.Close()
			fmt.Fprintf(out, "Site %s is running in the background with %s (process %d)\n", info.Site, countChargers(info.Chargers), info.PID)
			fmt.Fprintf(out, "Its log is in %s\n", logPath)
			return nil
		}
		select {
		case <-exited:
			return fmt.Errorf("the site did not start:\n%s", strings.TrimSpace(tail(logPath, 20)))
		case <-deadline:
			return fmt.Errorf("the site did not report as running within %s; see %s", detachTimeout, logPath)
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// tail returns the last lines of a text file.
func tail(path string, lines int) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	all := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(all) > lines {
		all = all[len(all)-lines:]
	}
	return strings.Join(all, "\n")
}

func countChargers(n int) string {
	if n == 1 {
		return "1 charger"
	}
	return fmt.Sprintf("%d chargers", n)
}

func newSiteDown() *cobra.Command {
	var (
		file     string
		siteName string
	)

	cmd := &cobra.Command{
		Use:   "down",
		Short: "Shut down a running site",
		Long: "Shut down the chargers of a running site in an orderly way: transactions\n" +
			"end with StopTransaction and connections are closed. The site is the one\n" +
			"declared by site.yaml in the current directory unless -f or -s says\n" +
			"otherwise.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			name := siteName
			if name == "" {
				var err error
				if name, err = sitefile.Name(file); err != nil {
					if errors.Is(err, os.ErrNotExist) && file == sitefile.DefaultName {
						return &UsageError{Err: fmt.Errorf("there is no %s in this directory; name a site with -s or a site file with -f", sitefile.DefaultName)}
					}
					return &UsageError{Err: err}
				}
			}

			dir, err := datadir.Path()
			if err != nil {
				return err
			}
			client, err := control.Probe(cmd.Context(), dir, name)
			if err != nil {
				return fmt.Errorf("site %q is not running", name)
			}
			defer client.Close()

			ctx, cancel := context.WithTimeout(cmd.Context(), 2*time.Minute)
			defer cancel()
			if err := client.Shutdown(ctx); err != nil {
				return fmt.Errorf("site %q did not shut down: %w", name, err)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Site %s is down\n", name)
			return nil
		},
	}

	addFileFlag(cmd, &file)
	cmd.Flags().StringVarP(&siteName, "site", "s", "", "name of the site to shut down")
	return cmd
}

// siteRow is one line of "site ls".
type siteRow struct {
	Site     string   `json:"site"`
	Chargers int      `json:"chargers"`
	CSMS     []string `json:"csms"`
	PID      int      `json:"pid"`
}

func newSiteLs() *cobra.Command {
	var (
		all    bool
		format string
	)

	cmd := &cobra.Command{
		Use:   "ls",
		Short: "List running sites",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := checkFormat(format); err != nil {
				return err
			}
			dir, err := datadir.Path()
			if err != nil {
				return err
			}
			clients, err := control.Discover(cmd.Context(), dir)
			if err != nil {
				return err
			}

			rows := []siteRow{}
			for _, client := range clients {
				rows = append(rows, siteRow{
					Site:     client.Info.Site,
					Chargers: client.Info.Chargers,
					CSMS:     client.Info.CSMS,
					PID:      client.Info.PID,
				})
				client.Close()
			}

			out := cmd.OutOrStdout()
			if format == formatJSON {
				return writeJSONLines(out, rows)
			}
			table := [][]cell{{plain("SITE"), plain("CHARGERS"), plain("PROCESS"), plain("CSMS")}}
			for _, r := range rows {
				table = append(table, []cell{
					plain(r.Site),
					plain(strconv.Itoa(r.Chargers)),
					plain(strconv.Itoa(r.PID)),
					plain(strings.Join(r.CSMS, ", ")),
				})
			}
			writeTable(out, table)
			return nil
		},
	}

	// Stopped sites need saved state, which does not exist yet; the flag is
	// accepted so that scripts written against the spec keep working.
	cmd.Flags().BoolVarP(&all, "all", "a", false, "include stopped sites")
	addFormatFlag(cmd, &format)
	return cmd
}

func newSiteValidate() *cobra.Command {
	var file string

	cmd := &cobra.Command{
		Use:   "validate",
		Short: "Check a site file without starting anything",
		Long: "Check a site file and the Chargerfiles it refers to, and show the chargers\n" +
			"it would create.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			sf, err := loadSiteFile(file)
			if err != nil {
				return err
			}
			s, err := buildSite(sf, nil)
			if err != nil {
				return err
			}

			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "Site %s is valid: %s, CSMS %s\n", sf.Name, countChargers(len(s.Chargers())), strings.Join(s.CSMS(), ", "))

			var rows [][]cell
			for _, entry := range sf.Entries {
				first, last := entry.Chargers[0], entry.Chargers[len(entry.Chargers)-1]
				ids := first.ID
				if len(entry.Chargers) > 1 {
					ids = first.ID + " to " + last.ID
				}
				rows = append(rows, []cell{
					plain("  " + entry.Name),
					plain(countChargers(len(entry.Chargers))),
					plain(ids),
					plain(first.Model.Vendor + " " + first.Model.Name),
				})
			}
			writeTable(out, rows)

			if len(sf.EVs) > 0 {
				names := make([]string, 0, len(sf.EVs))
				for name := range sf.EVs {
					names = append(names, name)
				}
				sort.Strings(names)
				fmt.Fprintf(out, "EV presets: %s\n", strings.Join(names, ", "))
			}
			return nil
		},
	}

	addFileFlag(cmd, &file)
	return cmd
}
