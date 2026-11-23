package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	"github.com/Amraa1/plugyard/internal/control"
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

func newPs() *cobra.Command {
	var (
		all      bool
		siteName string
		format   string
	)

	cmd := &cobra.Command{
		Use:   "ps",
		Short: "List running chargers",
		Long: "List running chargers with their name, site, connection state and the\n" +
			"status of each connector. With -a, stopped chargers are listed too, with\n" +
			"the status each connector will report when the charger starts.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := checkFormat(format); err != nil {
				return err
			}
			w, err := loadWorld(cmd.Context(), siteName)
			if err != nil {
				return err
			}
			defer w.close()

			chargers := []control.ChargerState{}
			for _, t := range w.targets {
				if all || t.state.Powered {
					chargers = append(chargers, t.state)
				}
			}

			out := cmd.OutOrStdout()
			if format == formatJSON {
				return writeJSONLines(out, chargers)
			}
			writePsTable(out, chargers)
			return nil
		},
	}

	cmd.Flags().BoolVarP(&all, "all", "a", false, "include stopped chargers")
	cmd.Flags().StringVarP(&siteName, "site", "s", "", "only show chargers of this site")
	addFormatFlag(cmd, &format)
	return cmd
}

func writePsTable(w io.Writer, chargers []control.ChargerState) {
	p := newPalette(w)
	rows := [][]cell{{
		plain("CHARGE POINT ID"), plain("NAME"), plain("SITE"), plain("CONNECTION"), plain("CONNECTORS"),
	}}
	for _, c := range chargers {
		var plainParts, coloredParts []string
		for _, k := range c.Connectors {
			plainParts = append(plainParts, fmt.Sprintf("%d:%s", k.ID, k.Status))
			coloredParts = append(coloredParts, fmt.Sprintf("%d:%s", k.ID, p.status(k.Status)))
		}
		rows = append(rows, []cell{
			plain(c.ChargePointID),
			plain(c.Name),
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
