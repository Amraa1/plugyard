package cli

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/Amraa1/plugyard/internal/control"
	"github.com/Amraa1/plugyard/internal/ocpp/v16"
)

// statsRow is one charger in "plugyard stats".
type statsRow struct {
	Site          string `json:"site"`
	Name          string `json:"name"`
	ChargePointID string `json:"chargePointId"`
	Connection    string `json:"connection"`
	// PowerW is the power the charger delivers now, over all connectors.
	PowerW float64 `json:"powerW"`
	// EnergyWh is the sum of the charger's meter registers.
	EnergyWh float64 `json:"energyWh"`
	// Transactions is the number of connectors with a transaction.
	Transactions int `json:"transactions"`
	// MessagesSent and MessagesReceived count OCPP messages since the
	// charger was powered on.
	MessagesSent     int64 `json:"messagesSent"`
	MessagesReceived int64 `json:"messagesReceived"`
}

func newStatsRow(c control.ChargerState) statsRow {
	row := statsRow{
		Site:             c.Site,
		Name:             c.Name,
		ChargePointID:    c.ChargePointID,
		Connection:       c.Connection,
		MessagesSent:     c.MessagesSent,
		MessagesReceived: c.MessagesReceived,
	}
	for _, k := range c.Connectors {
		row.PowerW += k.PowerW
		row.EnergyWh += k.MeterWh
		switch k.Status {
		case v16.StatusCharging, v16.StatusSuspendedEV, v16.StatusSuspendedEVSE:
			row.Transactions++
		}
	}
	return row
}

func newStats() *cobra.Command {
	var (
		siteName string
		format   string
		noStream bool
	)

	cmd := &cobra.Command{
		Use:   "stats",
		Short: "Show power, energy and message counts of running chargers",
		Long: "Show, for every running charger, the power it delivers, the energy on its\n" +
			"meters, its transactions and the OCPP messages it has sent and received\n" +
			"since it was powered on. The first line is the total.\n\n" +
			"In a terminal the table refreshes every second until Ctrl+C, with the\n" +
			"busiest chargers first, as many as fit on the screen. With --no-stream,\n" +
			"with --format json, or when the output is not a terminal, it is printed\n" +
			"once, with every charger.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := checkFormat(format); err != nil {
				return err
			}
			ctx, out := cmd.Context(), cmd.OutOrStdout()

			rows, err := gatherStats(ctx, siteName)
			if err != nil {
				return err
			}
			if format == formatJSON {
				return writeJSONLines(out, rows)
			}
			terminal, isTerminal := out.(*os.File)
			if noStream || !isTerminal || !term.IsTerminal(int(terminal.Fd())) || !enableTerminalColors(terminal) {
				writeStatsTable(out, rows, 0)
				return nil
			}

			ticker := time.NewTicker(time.Second)
			defer ticker.Stop()
			for {
				// Busiest first, so that what matters is on the screen.
				sort.SliceStable(rows, func(i, j int) bool { return rows[i].PowerW > rows[j].PowerW })
				limit := 0
				if _, height, err := term.GetSize(int(terminal.Fd())); err == nil && height > 4 {
					limit = height - 4
				}
				var frame bytes.Buffer
				// Move to the top left and clear what was there.
				frame.WriteString("\x1b[H\x1b[J")
				writeStatsTable(&frame, rows, limit)
				_, _ = out.Write(frame.Bytes())

				select {
				case <-ctx.Done():
					return nil
				case <-ticker.C:
				}
				if rows, err = gatherStats(ctx, siteName); err != nil {
					if ctx.Err() != nil {
						return nil
					}
					return err
				}
			}
		},
	}

	cmd.Flags().StringVarP(&siteName, "site", "s", "", "only show chargers of this site")
	cmd.Flags().BoolVar(&noStream, "no-stream", false, "print once instead of refreshing")
	addFormatFlag(cmd, &format)
	return cmd
}

// gatherStats reads the running chargers, sorted by charge point ID.
func gatherStats(ctx context.Context, siteName string) ([]statsRow, error) {
	w, err := loadWorld(ctx, siteName)
	if err != nil {
		return nil, err
	}
	defer w.close()
	rows := []statsRow{}
	for _, t := range w.targets {
		if t.state.Powered {
			rows = append(rows, newStatsRow(t.state))
		}
	}
	return rows, nil
}

// writeStatsTable prints the total and then the chargers: all of them, or
// the first limit when limit is above zero.
func writeStatsTable(w io.Writer, rows []statsRow, limit int) {
	p := newPalette(w)
	var total statsRow
	for _, r := range rows {
		total.PowerW += r.PowerW
		total.EnergyWh += r.EnergyWh
		total.Transactions += r.Transactions
		total.MessagesSent += r.MessagesSent
		total.MessagesReceived += r.MessagesReceived
	}
	line := func(r statsRow) []cell {
		return []cell{
			plain(formatKilo(r.PowerW, "kW")),
			plain(formatKilo(r.EnergyWh, "kWh")),
			plain(strconv.Itoa(r.Transactions)),
			plain(strconv.FormatInt(r.MessagesSent, 10)),
			plain(strconv.FormatInt(r.MessagesReceived, 10)),
		}
	}

	table := [][]cell{{
		plain("CHARGE POINT ID"), plain("NAME"), plain("SITE"), plain("CONNECTION"),
		plain("POWER"), plain("ENERGY"), plain("TX"), plain("SENT"), plain("RECEIVED"),
	}}
	table = append(table, append([]cell{plain("TOTAL"), plain(countChargers(len(rows))), plain(""), plain("")}, line(total)...))
	hidden := 0
	for i, r := range rows {
		if limit > 0 && i >= limit {
			hidden = len(rows) - i
			break
		}
		table = append(table, append([]cell{
			plain(r.ChargePointID), plain(r.Name), plain(r.Site),
			{plain: r.Connection, rendered: p.connection(r.Connection)},
		}, line(r)...))
	}
	writeTable(w, table)
	if hidden > 0 {
		fmt.Fprintf(w, "... and %s more; --no-stream lists them all\n", countChargers(hidden))
	}
}

// formatKilo prints a value in W or Wh as kW or kWh with one decimal.
func formatKilo(value float64, unit string) string {
	return strconv.FormatFloat(value/1000, 'f', 1, 64) + " " + unit
}
