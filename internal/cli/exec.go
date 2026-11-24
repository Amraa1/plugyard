package cli

import (
	"errors"
	"fmt"
	"io"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/Amraa1/plugyard/internal/charger"
	"github.com/Amraa1/plugyard/internal/control"
	"github.com/Amraa1/plugyard/internal/ev"
	"github.com/Amraa1/plugyard/internal/model"
	"github.com/Amraa1/plugyard/internal/ocpp/v16"
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
		info      string
		vendor    string
		duration  time.Duration
	)

	cmd := &cobra.Command{
		Use:   "exec <charger> <action> [arguments]",
		Short: "Run an action on running chargers",
		Long: "Run an action on one charger, named by its charge point ID or its name, or\n" +
			"on every charger matching a pattern such as 'FAST-*'. One line is printed per charger saying whether the action was\n" +
			"carried out and, if not, why.\n\n" +
			"Actions for a driver:\n" +
			"  plug                Plug an EV into the connector\n" +
			"  unplug              Remove the EV\n" +
			"  tap <idTag>         Present a tag: authorize it, then start or stop the transaction\n\n" +
			"Actions for testing what goes wrong:\n" +
			"  fault <errorCode>   Report a fault, as real hardware does; a transaction there ends\n" +
			"  clear               Recover from the fault\n" +
			"  offline             Take the network connection down; the charger keeps running\n" +
			"  online              Restore the network connection\n" +
			"  reboot              Reboot the charger\n\n" +
			"Error codes for fault: " + strings.Join(v16.ErrorCodes[:8], ", ") + ",\n" +
			"  " + strings.Join(v16.ErrorCodes[8:], ", ") + "\n\n" +
			"fault and clear act on connector 1 unless -n says otherwise; -n 0 means the\n" +
			"charger as a whole. While offline the charger queues what it has to tell the\n" +
			"CSMS, and sends it in order once it is back.",
		Example: "  plugyard exec CP-1 plug --soc 30% --battery 82kWh\n" +
			"  plugyard exec CP-1 tap 04A1B2C3\n" +
			"  plugyard exec 'FAST-*' unplug -n 2\n" +
			"  plugyard exec CP-1 fault GroundFailure\n" +
			"  plugyard exec CP-1 fault OverCurrentFailure -n 2 --info \"phase L2\"\n" +
			"  plugyard exec CP-1 clear\n" +
			"  plugyard exec 'FAST-*' offline --for 30s\n" +
			"  plugyard exec CP-1 reboot",
		Args: cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := checkFormat(format); err != nil {
				return err
			}
			pattern, action, rest := args[0], args[1], args[2:]
			if err := checkPattern(pattern); err != nil {
				return err
			}
			onCharger := action == control.ActionFault || action == control.ActionClear
			switch {
			case control.WholeCharger(action) && cmd.Flags().Changed("connector"):
				return &UsageError{Err: fmt.Errorf("%s acts on the whole charger, so it takes no --connector", action)}
			case connector < 0, connector == 0 && !onCharger:
				return &UsageError{Err: errors.New("the connector number must be 1 or more")}
			}

			req := control.ExecRequest{Action: action, Connector: connector, For: duration}
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
			case control.ActionFault:
				if err := wantArgs(1, "exactly one argument, the error code: "+strings.Join(v16.ErrorCodes, ", ")); err != nil {
					return err
				}
				if !slices.Contains(v16.ErrorCodes, rest[0]) {
					return &UsageError{Err: fmt.Errorf("unknown error code %q: OCPP 1.6 has %s", rest[0], strings.Join(v16.ErrorCodes, ", "))}
				}
				req.Fault = charger.Fault{Code: rest[0], Info: info, VendorCode: vendor}
			case control.ActionClear, control.ActionOnline, control.ActionReboot, control.ActionOffline:
				if err := wantArgs(0, "no arguments"); err != nil {
					return err
				}
			default:
				return &UsageError{Err: fmt.Errorf("unknown action %q: want plug, unplug, tap, fault, clear, offline, online or reboot", action)}
			}
			if (info != "" || vendor != "") && action != control.ActionFault {
				return &UsageError{Err: errors.New("--info and --vendor-code only apply to fault")}
			}
			if cmd.Flags().Changed("for") {
				if action != control.ActionOffline {
					return &UsageError{Err: errors.New("--for only applies to offline")}
				}
				if duration <= 0 {
					return &UsageError{Err: errors.New("--for must be a time above zero, such as 30s")}
				}
			}
			if evFlagsGiven && action != control.ActionPlug {
				return &UsageError{Err: errors.New("--ev, --battery, --soc, --max-power, --target-soc and --curve only apply to plug")}
			}

			w, err := loadWorld(cmd.Context(), siteName)
			if err != nil {
				return err
			}
			defer w.close()

			// Actions need a charger that is powered on.
			matched, err := w.match(pattern, func(t target) bool { return t.state.Powered })
			if err != nil {
				return err
			}
			if len(matched) == 0 {
				if stopped, _ := w.match(pattern, nil); len(stopped) > 0 && !isPattern(pattern) {
					return fmt.Errorf("charger %q is stopped; start it with \"plugyard start %s\"", pattern, pattern)
				}
				return fmt.Errorf("no running charger matches %q", pattern)
			}

			// Work out which chargers the argument names, site by site.
			type group struct {
				client *control.Client
				ids    []string
			}
			var targets []*group
			bySite := map[string]*group{}
			for _, t := range matched {
				g := bySite[t.state.Site]
				if g == nil {
					g = &group{client: t.client}
					bySite[t.state.Site] = g
					targets = append(targets, g)
				}
				g.ids = append(g.ids, t.state.ChargePointID)
			}

			var results []control.ExecResult
			for _, t := range targets {
				req.ChargePointIDs = t.ids
				got, err := t.client.Exec(cmd.Context(), req)
				if err != nil {
					// The whole site failed to answer: every charger in it
					// gets the reason.
					for _, id := range t.ids {
						got = append(got, control.ExecResult{
							ChargePointID: id,
							Connector:     req.Connector,
							Message:       fmt.Sprintf("site %q did not answer: %v", t.client.Info.Site, err),
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
	flags.StringVar(&info, "info", "", "fault: free text about the fault, sent to the CSMS")
	flags.StringVar(&vendor, "vendor-code", "", "fault: the vendor's own code for the fault")
	flags.DurationVar(&duration, "for", 0, "offline: come back online after this long, such as 30s")
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
		row := []cell{{plain: r.ChargePointID, rendered: p.bold(r.ChargePointID)}}
		// An operation on the whole charger names no connector.
		if r.Connector != 0 {
			row = append(row, plain(fmt.Sprintf("connector %d", r.Connector)))
		}
		rows = append(rows, append(row, outcome, plain(r.Message)))
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
