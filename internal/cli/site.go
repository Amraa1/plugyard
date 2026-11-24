package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
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
	"github.com/Amraa1/plugyard/internal/store"
)

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
	cmd.AddCommand(newSiteUp(), newSiteStop(), newSiteStart(), newSiteDown(), newSiteLs(), newSiteLogs(), newSiteValidate(), newSiteServe())
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

// checkSiteFile makes sure every charger a site file declares can be
// created, and returns the distinct CSMS URLs they connect to.
func checkSiteFile(sf *sitefile.Site) ([]string, error) {
	var members []site.Member
	for _, entry := range sf.Entries {
		for _, spec := range entry.Chargers {
			m := site.Member{ID: spec.ID, Model: spec.Model, CSMS: spec.CSMS}
			if _, err := charger.New(charger.Config{ID: m.ID, Model: m.Model, CSMS: m.CSMS}); err != nil {
				return nil, &UsageError{Err: fmt.Errorf("chargers.%s: %w", entry.Name, err)}
			}
			members = append(members, m)
		}
	}
	s, err := site.New(site.Config{Name: sf.Name}, members)
	if err != nil {
		return nil, &UsageError{Err: err}
	}
	return s.CSMS(), nil
}

// desiredFromFile lists the chargers a site file declares, in the form
// saved state is compared with.
func desiredFromFile(sf *sitefile.Site) []desired {
	var want []desired
	for _, c := range sf.Chargers() {
		want = append(want, desired{
			ID:   c.ID,
			Name: c.Name,
			Spec: store.Spec{
				Model:              c.Model,
				CSMSURL:            c.CSMS.URL,
				InsecureSkipVerify: c.CSMS.InsecureSkipVerify,
				HasPassword:        c.CSMS.Password != "",
				EV:                 c.EV,
			},
		})
	}
	return want
}

func newSiteUp() *cobra.Command {
	var (
		file        string
		detach      bool
		yes         bool
		speed       float64
		connectRate float64
	)

	cmd := &cobra.Command{
		Use:   "up",
		Short: "Start the chargers of a site",
		Long: "Create the chargers a site file declares, reusing the state saved for\n" +
			"them, and start them. The site runs in the foreground showing the log of\n" +
			"every charger; press Ctrl+C to stop it. With -d it runs in the background\n" +
			"instead.\n\n" +
			"When the file has changed so that saved state would be lost, for example\n" +
			"because a charger was removed from it, the affected chargers are listed\n" +
			"and nothing happens until you agree. --yes agrees in advance.",
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
			if _, err := checkSiteFile(sf); err != nil {
				return err
			}
			count := len(sf.Chargers())
			if err := checkFileLimit(count); err != nil {
				return err
			}

			dir, err := datadir.Path()
			if err != nil {
				return err
			}
			meta := store.Meta{
				Name:        sf.Name,
				File:        sf.Path,
				Speed:       sf.Speed,
				ConnectRate: sf.ConnectRate,
				EVs:         sf.EVs,
				LogMaxSize:  sf.LogMaxSize,
				LogFiles:    sf.LogFiles,

				AllowUploads:              sf.AllowUploads,
				UploadsInsecureSkipVerify: sf.UploadsInsecureSkipVerify,
			}
			st, err := prepare(cmd.Context(), cmd, dir, meta, desiredFromFile(sf), yes, false)
			if err != nil {
				return err
			}

			out := cmd.OutOrStdout()
			if detach {
				st.Close()
				info, err := startBackground(cmd.Context(), dir, sf.Name)
				if err != nil {
					return err
				}
				reportBackground(out, dir, info)
				return nil
			}

			fmt.Fprintf(out, "Starting site %s with %s\n", sf.Name, countChargers(count))
			passwords := filePasswords(sf)
			return serveSaved(cmd.Context(), dir, st, out, true, func(id string) string { return passwords[id] })
		},
	}

	addFileFlag(cmd, &file)
	cmd.Flags().BoolVarP(&detach, "detach", "d", false, "run the site in the background")
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "reset or delete saved state that no longer fits the files, without asking")
	cmd.Flags().Float64Var(&speed, "speed", 1, "multiply the rate of EV charging and energy accumulation")
	cmd.Flags().Float64Var(&connectRate, "connect-rate", 0, "most new connections to open per second; 0 means no limit")
	return cmd
}

// newSiteServe is the command a background copy of plugyard runs. It is not
// meant to be typed.
func newSiteServe() *cobra.Command {
	var siteName string
	cmd := &cobra.Command{
		Use:    "serve",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			dir, err := datadir.Path()
			if err != nil {
				return err
			}
			st, err := store.OpenExisting(dir, siteName)
			if err != nil {
				return err
			}
			saved, err := st.Load()
			if err != nil {
				st.Close()
				return err
			}
			password, err := savedPasswords(saved)
			if err != nil {
				st.Close()
				return err
			}
			// A site in the background has no terminal: its log goes to the
			// log files only.
			return serveSaved(cmd.Context(), dir, st, nil, true, password)
		},
	}
	cmd.Flags().StringVarP(&siteName, "site", "s", "", "name of the site to run")
	_ = cmd.MarkFlagRequired("site")
	return cmd
}

// shutDownSite asks a running site to shut down in an orderly way and waits
// for it. It reports whether the site was running.
func shutDownSite(ctx context.Context, dir, name string) (bool, error) {
	client, err := control.Probe(ctx, dir, name)
	if err != nil {
		return false, nil
	}
	defer client.Close()

	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	if err := client.Shutdown(ctx); err != nil {
		return true, fmt.Errorf("site %q did not shut down: %w", name, err)
	}
	return true, nil
}

func newSiteStop() *cobra.Command {
	var (
		file     string
		siteName string
	)

	cmd := &cobra.Command{
		Use:   "stop",
		Short: "Power off the chargers of a site",
		Long: "Shut down the chargers of a running site in an orderly way: transactions\n" +
			"end with StopTransaction and connections are closed. The chargers stay\n" +
			"listed in \"plugyard ps -a\" and come back with \"plugyard site start\".",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			name, err := siteNameFromFlags(file, siteName)
			if err != nil {
				return err
			}
			dir, err := datadir.Path()
			if err != nil {
				return err
			}
			wasRunning, err := shutDownSite(cmd.Context(), dir, name)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			switch {
			case wasRunning:
				fmt.Fprintf(out, "Site %s is stopped\n", name)
			case store.Exists(dir, name):
				fmt.Fprintf(out, "Site %s is already stopped\n", name)
			default:
				return fmt.Errorf("site %q is not running", name)
			}
			return nil
		},
	}

	addFileFlag(cmd, &file)
	cmd.Flags().StringVarP(&siteName, "site", "s", "", "name of the site to stop")
	return cmd
}

func newSiteStart() *cobra.Command {
	var (
		file     string
		siteName string
	)

	cmd := &cobra.Command{
		Use:   "start",
		Short: "Power on the chargers of a stopped site",
		Long: "Start every charger of a stopped site again, in the background, exactly\n" +
			"as it was saved. No file is read: to apply changes made to the site file,\n" +
			"use \"plugyard site up\" instead.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			name, err := siteNameFromFlags(file, siteName)
			if err != nil {
				return err
			}
			dir, err := datadir.Path()
			if err != nil {
				return err
			}
			if client, err := control.Probe(cmd.Context(), dir, name); err == nil {
				client.Close()
				return fmt.Errorf("site %q: %w", name, control.ErrAlreadyRunning)
			}
			st, err := store.OpenExisting(dir, name)
			if err != nil {
				return fmt.Errorf("site %q has no saved state; bring it up with \"plugyard site up\"", name)
			}
			meta, err := st.Meta()
			if err == nil && meta.Down {
				err = fmt.Errorf("site %q was taken down; bring it back with \"plugyard site up\"", name)
			}
			if err == nil {
				err = st.SetPower(nil)
			}
			st.Close()
			if err != nil {
				return err
			}

			info, err := startBackground(cmd.Context(), dir, name)
			if err != nil {
				return err
			}
			reportBackground(cmd.OutOrStdout(), dir, info)
			return nil
		},
	}

	addFileFlag(cmd, &file)
	cmd.Flags().StringVarP(&siteName, "site", "s", "", "name of the site to start")
	return cmd
}

func newSiteDown() *cobra.Command {
	var (
		file     string
		siteName string
		reset    bool
	)

	cmd := &cobra.Command{
		Use:   "down",
		Short: "Stop a site and take its chargers off the list",
		Long: "Shut down the chargers of a site in an orderly way, as \"site stop\" does,\n" +
			"and take them off the list of \"plugyard ps -a\". Their saved state is\n" +
			"kept and used by the next \"plugyard site up\", unless --reset is given,\n" +
			"which wipes it. The site is the one declared by site.yaml in the current\n" +
			"directory unless -f or -s says otherwise.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			name, err := siteNameFromFlags(file, siteName)
			if err != nil {
				return err
			}
			dir, err := datadir.Path()
			if err != nil {
				return err
			}
			wasRunning, err := shutDownSite(cmd.Context(), dir, name)
			if err != nil {
				return err
			}
			if !wasRunning && !store.Exists(dir, name) {
				return fmt.Errorf("site %q is not running and has no saved state", name)
			}

			out := cmd.OutOrStdout()
			if reset {
				if err := store.Delete(dir, name); err != nil {
					return fmt.Errorf("cannot wipe the saved state of site %q: %w", name, err)
				}
				fmt.Fprintf(out, "Site %s is down and its saved state is wiped\n", name)
				return nil
			}
			if store.Exists(dir, name) {
				st, err := store.Open(dir, name)
				if err != nil {
					return err
				}
				defer st.Close()
				meta, err := st.Meta()
				if err != nil {
					return err
				}
				meta.Down = true
				if err := st.SetMeta(meta); err != nil {
					return err
				}
			}
			fmt.Fprintf(out, "Site %s is down\n", name)
			return nil
		},
	}

	addFileFlag(cmd, &file)
	cmd.Flags().StringVarP(&siteName, "site", "s", "", "name of the site to take down")
	cmd.Flags().BoolVar(&reset, "reset", false, "also wipe the saved state of the site's chargers")
	return cmd
}

// Site statuses shown by "site ls".
const (
	siteRunning = "running"
	siteStopped = "stopped"
	siteDown    = "down"
)

// siteRow is one line of "site ls".
type siteRow struct {
	Site     string   `json:"site"`
	Status   string   `json:"status"`
	Chargers int      `json:"chargers"`
	CSMS     []string `json:"csms"`
	// PID is the site's process, or zero when it is not running.
	PID int `json:"pid,omitempty"`
}

// siteRows lists the running sites and, with all, the stopped and the down
// ones too, sorted by name.
func siteRows(w *world, all bool) []siteRow {
	rows := []siteRow{}
	running := map[string]bool{}
	for _, client := range w.clients {
		running[client.Info.Site] = true
		rows = append(rows, siteRow{
			Site:     client.Info.Site,
			Status:   siteRunning,
			Chargers: client.Info.Chargers,
			CSMS:     client.Info.CSMS,
			PID:      client.Info.PID,
		})
	}
	if !all {
		return rows
	}
	for _, s := range w.saved {
		if running[s.Meta.Name] {
			continue
		}
		row := siteRow{Site: s.Meta.Name, Status: siteStopped, Chargers: len(s.Chargers), CSMS: []string{}}
		if s.Meta.Down {
			row.Status = siteDown
		}
		seen := map[string]bool{}
		for _, c := range s.Chargers {
			if !seen[c.Spec.CSMSURL] {
				seen[c.Spec.CSMSURL] = true
				row.CSMS = append(row.CSMS, c.Spec.CSMSURL)
			}
		}
		sort.Strings(row.CSMS)
		rows = append(rows, row)
	}
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].Site < rows[j].Site })
	return rows
}

func newSiteLs() *cobra.Command {
	var (
		all    bool
		format string
	)

	cmd := &cobra.Command{
		Use:   "ls",
		Short: "List sites",
		Long: "List running sites. With -a, also list stopped sites and sites that were\n" +
			"taken down but still have saved state.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := checkFormat(format); err != nil {
				return err
			}
			w, err := loadWorld(cmd.Context(), "")
			if err != nil {
				return err
			}
			defer w.close()

			rows := siteRows(w, all)

			out := cmd.OutOrStdout()
			if format == formatJSON {
				return writeJSONLines(out, rows)
			}
			p := newPalette(out)
			table := [][]cell{{plain("SITE"), plain("STATUS"), plain("CHARGERS"), plain("PROCESS"), plain("CSMS")}}
			for _, r := range rows {
				status := cell{plain: r.Status, rendered: r.Status}
				process := "-"
				if r.Status == siteRunning {
					status.rendered = p.green(r.Status)
					process = strconv.Itoa(r.PID)
				}
				table = append(table, []cell{
					plain(r.Site),
					status,
					plain(strconv.Itoa(r.Chargers)),
					plain(process),
					plain(strings.Join(r.CSMS, ", ")),
				})
			}
			writeTable(out, table)
			return nil
		},
	}

	cmd.Flags().BoolVarP(&all, "all", "a", false, "include sites that are stopped or down")
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
			urls, err := checkSiteFile(sf)
			if err != nil {
				return err
			}

			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "Site %s is valid: %s, CSMS %s\n", sf.Name, countChargers(len(sf.Chargers())), strings.Join(urls, ", "))

			var rows [][]cell
			for _, entry := range sf.Entries {
				first, last := entry.Chargers[0], entry.Chargers[len(entry.Chargers)-1]
				ids := first.ID
				if len(entry.Chargers) > 1 {
					ids = first.ID + " to " + last.ID
				}
				row := []cell{
					plain("  " + entry.Name),
					plain(countChargers(len(entry.Chargers))),
					plain(ids),
					plain(first.Model.Vendor + " " + first.Model.Name),
				}
				if first.Name != "" {
					row = append(row, plain("name "+first.Name))
				}
				rows = append(rows, row)
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
