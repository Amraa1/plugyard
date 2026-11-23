package cli

import (
	"errors"
	"fmt"
	"io"
	"path"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/Amraa1/plugyard/internal/control"
	"github.com/Amraa1/plugyard/internal/ev"
	"github.com/Amraa1/plugyard/internal/model"
)

func newExec() *cobra.Command {
	var (
		connector int
		siteName  string
		format    string
		battery   string
		soc       string
		maxPower  string
		targetSoC string
		curve     string
		preset    string
	)

	cmd := &cobra.Command{
		Use:   "exec <charger> <action> [arguments]",
		Short: "Run an action on running chargers",
		Long: "Run an action on one charger, or on every charger matching a pattern such\n" +
			"as 'FAST-*'. One line is printed per charger saying whether the action was\n" +
			"carried out and, if not, why.\n\n" +
			"Actions:\n" +
			"  plug           Plug an EV into the connector\n" +
			"  unplug         Remove the EV\n" +
			"  tap <idTag>    Present a tag: authorize it, then start or stop the transaction",
		Example: "  plugyard exec CP-1 plug --soc 30% --battery 82kWh\n" +
			"  plugyard exec CP-1 tap 04A1B2C3\n" +
			"  plugyard exec 'FAST-*' unplug -n 2",
		Args: cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := checkFormat(format); err != nil {
				return err
			}
			pattern, action, rest := args[0], args[1], args[2:]
			if _, err := path.Match(pattern, ""); err != nil {
				return &UsageError{Err: fmt.Errorf("invalid charger pattern %q", pattern)}
			}
			if connector < 1 {
				return &UsageError{Err: errors.New("the connector number must be 1 or more")}
			}

			req := control.ExecRequest{Action: action, Connector: connector}
			evFlagsGiven := battery != "" || soc != "" || maxPower != "" || targetSoC != "" || curve != "" || preset != ""

			wantArgs := func(n int, what string) error {
				if len(rest) != n {
					return &UsageError{Err: fmt.Errorf("%s takes %s", action, what)}
				}
				return nil
			}

			switch action {
			case control.ActionPlug:
				if err := wantArgs(0, "no arguments"); err != nil {
					return err
				}
				params, err := parseEVFlags(battery, soc, maxPower, targetSoC, curve)
				if err != nil {
					return &UsageError{Err: err}
				}
				req.EV = params
				req.EVPreset = preset
			case control.ActionUnplug:
				if err := wantArgs(0, "no arguments"); err != nil {
					return err
				}
			case control.ActionTap:
				if err := wantArgs(1, "exactly one argument, the tag"); err != nil {
					return err
				}
				req.Tag = rest[0]
			default:
				return &UsageError{Err: fmt.Errorf("unknown action %q: want plug, unplug or tap", action)}
			}
			if evFlagsGiven && action != control.ActionPlug {
				return &UsageError{Err: errors.New("--ev, --battery, --soc, --max-power, --target-soc and --curve only apply to plug")}
			}

			sites, err := runningSites(cmd.Context(), siteName)
			if err != nil {
				return err
			}
			defer closeSites(sites)

			// Work out which chargers the argument names, site by site.
			type target struct {
				site runningSite
				ids  []string
			}
			var targets []target
			var matchedSites []string
			total := 0
			for _, s := range sites {
				var ids []string
				for _, c := range s.chargers {
					if ok, _ := path.Match(pattern, c.ChargePointID); ok {
						ids = append(ids, c.ChargePointID)
					}
				}
				if len(ids) > 0 {
					targets = append(targets, target{site: s, ids: ids})
					matchedSites = append(matchedSites, s.client.Info.Site)
					total += len(ids)
				}
			}
			if total == 0 {
				return fmt.Errorf("no running charger matches %q", pattern)
			}
			if !strings.ContainsAny(pattern, `*?[\`) && total > 1 {
				return fmt.Errorf("charger %q runs in sites %s; choose one with -s", pattern, strings.Join(matchedSites, " and "))
			}

			var results []control.ExecResult
			for _, t := range targets {
				req.ChargePointIDs = t.ids
				got, err := t.site.client.Exec(cmd.Context(), req)
				if err != nil {
					// The whole site failed to answer: every charger in it
					// gets the reason.
					for _, id := range t.ids {
						got = append(got, control.ExecResult{
							ChargePointID: id,
							Connector:     connector,
							Message:       fmt.Sprintf("site %q did not answer: %v", t.site.client.Info.Site, err),
						})
					}
				}
				results = append(results, got...)
			}
			sort.SliceStable(results, func(i, j int) bool { return results[i].ChargePointID < results[j].ChargePointID })

			failed := 0
			for _, r := range results {
				if !r.OK {
					failed++
				}
			}

			out := cmd.OutOrStdout()
			if format == formatJSON {
				if err := writeJSONLines(out, results); err != nil {
					return err
				}
			} else {
				writeExecResults(out, results, failed)
			}

			if failed > 0 {
				return errFailed
			}
			return nil
		},
	}

	flags := cmd.Flags()
	flags.IntVarP(&connector, "connector", "n", 1, "connector number")
	flags.StringVarP(&siteName, "site", "s", "", "only act on chargers of this site")
	flags.StringVar(&battery, "battery", "", "plug: battery capacity, such as 60kWh (default 60kWh)")
	flags.StringVar(&soc, "soc", "", "plug: starting state of charge, such as 20% (default 20%)")
	flags.StringVar(&maxPower, "max-power", "", "plug: most power the EV accepts, such as 11kW (default: no limit)")
	flags.StringVar(&targetSoC, "target-soc", "", "plug: state of charge at which the EV stops, such as 80% (default 100%)")
	flags.StringVar(&curve, "curve", "", "plug: charge curve, "+strings.Join(ev.BuiltinCurveNames(), " or ")+" (default standard)")
	flags.StringVar(&preset, "ev", "", "plug: EV preset from the site file")
	addFormatFlag(cmd, &format)
	return cmd
}

func parseEVFlags(battery, soc, maxPower, targetSoC, curve string) (ev.Overrides, error) {
	var params ev.Overrides
	if battery != "" {
		v, err := ev.ParseEnergy(battery)
		if err != nil {
			return params, fmt.Errorf("--battery: %w", err)
		}
		params.BatteryWh = &v
	}
	if soc != "" {
		v, err := ev.ParsePercent(soc)
		if err != nil {
			return params, fmt.Errorf("--soc: %w", err)
		}
		params.SoC = &v
	}
	if maxPower != "" {
		p, err := model.ParsePower(maxPower)
		if err != nil {
			return params, fmt.Errorf("--max-power: %w", err)
		}
		v := float64(p)
		params.MaxPowerW = &v
	}
	if targetSoC != "" {
		v, err := ev.ParsePercent(targetSoC)
		if err != nil {
			return params, fmt.Errorf("--target-soc: %w", err)
		}
		params.TargetSoC = &v
	}
	if curve != "" {
		c, err := ev.BuiltinCurve(curve)
		if err != nil {
			return params, fmt.Errorf("--curve: %w", err)
		}
		params.Curve = &c
	}
	return params, nil
}

// writeExecResults prints one line per charger, and a summary when the
// action ran on more than one.
func writeExecResults(w io.Writer, results []control.ExecResult, failed int) {
	p := newPalette(w)
	var rows [][]cell
	for _, r := range results {
		outcome := cell{plain: "ok", rendered: p.green("ok")}
		if !r.OK {
			outcome = cell{plain: "failed", rendered: p.red("failed")}
		}
		rows = append(rows, []cell{
			{plain: r.ChargePointID, rendered: p.bold(r.ChargePointID)},
			plain(fmt.Sprintf("connector %d", r.Connector)),
			outcome,
			plain(r.Message),
		})
	}
	writeTable(w, rows)

	if len(results) > 1 {
		summary := fmt.Sprintf("%d of %d succeeded", len(results)-failed, len(results))
		if failed > 0 {
			summary += ", " + p.red(fmt.Sprintf("%d failed", failed))
		}
		fmt.Fprintln(w, summary)
	}
}
