package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/Amraa1/plugyard/internal/control"
	"github.com/Amraa1/plugyard/internal/datadir"
)

const (
	formatTable = "table"
	formatJSON  = "json"
)

// addFormatFlag adds --format to a command that can print JSON.
func addFormatFlag(cmd *cobra.Command, format *string) {
	cmd.Flags().StringVar(format, "format", formatTable, `output format: "table" or "json" (one object per line)`)
}

func checkFormat(format string) error {
	if format != formatTable && format != formatJSON {
		return &UsageError{Err: fmt.Errorf("unknown format %q: want %q or %q", format, formatTable, formatJSON)}
	}
	return nil
}

// runningSite is a running site and the state of its chargers.
type runningSite struct {
	client   *control.Client
	chargers []control.ChargerState
}

// runningSites finds the running sites, narrowed to one site when siteName
// is given, and reads the state of their chargers. The caller closes the
// clients.
func runningSites(ctx context.Context, siteName string) ([]runningSite, error) {
	dir, err := datadir.Path()
	if err != nil {
		return nil, err
	}
	clients, err := control.Discover(ctx, dir)
	if err != nil {
		return nil, err
	}

	var sites []runningSite
	for _, client := range clients {
		if siteName != "" && client.Info.Site != siteName {
			client.Close()
			continue
		}
		chargers, err := client.Chargers(ctx)
		if err != nil {
			return nil, fmt.Errorf("site %q did not answer: %w", client.Info.Site, err)
		}
		sites = append(sites, runningSite{client: client, chargers: chargers})
	}
	if siteName != "" && len(sites) == 0 {
		return nil, fmt.Errorf("no running site is named %q", siteName)
	}
	return sites, nil
}

func closeSites(sites []runningSite) {
	for _, s := range sites {
		s.client.Close()
	}
}

func newPs() *cobra.Command {
	var (
		all      bool
		siteName string
		format   string
	)

	cmd := &cobra.Command{
		Use:   "ps",
		Short: "List running chargers",
		Long: "List running chargers with their site, connection state and the status of\n" +
			"each connector.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := checkFormat(format); err != nil {
				return err
			}
			sites, err := runningSites(cmd.Context(), siteName)
			if err != nil {
				return err
			}
			defer closeSites(sites)

			var chargers []control.ChargerState
			for _, s := range sites {
				chargers = append(chargers, s.chargers...)
			}
			sort.SliceStable(chargers, func(i, j int) bool {
				if chargers[i].ChargePointID != chargers[j].ChargePointID {
					return chargers[i].ChargePointID < chargers[j].ChargePointID
				}
				return chargers[i].Site < chargers[j].Site
			})

			out := cmd.OutOrStdout()
			if format == formatJSON {
				return writeJSONLines(out, chargers)
			}
			writePsTable(out, chargers)
			return nil
		},
	}

	// Stopped chargers need saved state, which does not exist yet; the flag
	// is accepted so that scripts written against the spec keep working.
	cmd.Flags().BoolVarP(&all, "all", "a", false, "include stopped chargers")
	cmd.Flags().StringVarP(&siteName, "site", "s", "", "only show chargers of this site")
	addFormatFlag(cmd, &format)
	return cmd
}

func writePsTable(w io.Writer, chargers []control.ChargerState) {
	p := newPalette(w)
	rows := [][]cell{{
		plain("CHARGE POINT ID"), plain("SITE"), plain("CONNECTION"), plain("CONNECTORS"),
	}}
	for _, c := range chargers {
		var plainParts, coloredParts []string
		for _, k := range c.Connectors {
			plainParts = append(plainParts, fmt.Sprintf("%d:%s", k.ID, k.Status))
			coloredParts = append(coloredParts, fmt.Sprintf("%d:%s", k.ID, p.status(k.Status)))
		}
		rows = append(rows, []cell{
			plain(c.ChargePointID),
			plain(c.Site),
			{plain: c.Connection, rendered: p.connection(c.Connection)},
			{plain: strings.Join(plainParts, " "), rendered: strings.Join(coloredParts, " ")},
		})
	}
	writeTable(w, rows)
}

// writeJSONLines prints each item as one JSON object on its own line.
func writeJSONLines[T any](w io.Writer, items []T) error {
	enc := json.NewEncoder(w)
	for _, item := range items {
		if err := enc.Encode(item); err != nil {
			return err
		}
	}
	return nil
}
