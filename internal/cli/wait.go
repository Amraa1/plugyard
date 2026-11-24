package cli

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/Amraa1/plugyard/internal/charger"
	"github.com/Amraa1/plugyard/internal/ocpp/v16"
)

// waitInterval is how often wait looks at the chargers.
const waitInterval = 100 * time.Millisecond

// States wait can wait for: the connection states "ps" shows, and the
// connector statuses a charger reports.
var (
	waitConnections = []string{charger.Online, charger.Offline, charger.Connecting, charger.Stopped}
	waitStatuses    = []string{
		v16.StatusAvailable, v16.StatusPreparing, v16.StatusCharging, v16.StatusSuspendedEV,
		v16.StatusSuspendedEVSE, v16.StatusFinishing, v16.StatusFaulted, v16.StatusUnavailable,
	}
)

// waitState finds the state an argument names, whatever its case, and says
// whether it is a connection state.
func waitState(arg string) (state string, connection bool, err error) {
	for _, s := range waitConnections {
		if strings.EqualFold(arg, s) {
			return s, true, nil
		}
	}
	for _, s := range waitStatuses {
		if strings.EqualFold(arg, s) {
			return s, false, nil
		}
	}
	return "", false, &UsageError{Err: fmt.Errorf("unknown state %q: want a connector status (%s) or a connection state (%s)",
		arg, strings.Join(waitStatuses, ", "), strings.Join(waitConnections, ", "))}
}

func newWait() *cobra.Command {
	var (
		connector int
		siteName  string
		timeout   time.Duration
	)

	cmd := &cobra.Command{
		Use:   "wait <charger>... <state>",
		Short: "Wait until chargers are in a state",
		Long: "Wait until every named charger is in a state, and then print one line per\n" +
			"charger. A script uses it in place of a guessed sleep: it exits with 0 once\n" +
			"the state is reached, at once if it already is, and with 1 if it is not\n" +
			"reached in time. A charger is named by its charge point ID or its name, or\n" +
			"by a pattern such as 'FAST-*'.\n\n" +
			"The state is the status of a connector, as OCPP names it:\n" +
			"  " + strings.Join(waitStatuses, ", ") + "\n" +
			"or the connection state of the charger, as \"plugyard ps\" shows it:\n" +
			"  " + strings.Join(waitConnections, ", ") + "\n\n" +
			"A connector status is that of connector 1 unless -n says otherwise. The\n" +
			"timeout is in real time, whatever the --speed of the site.",
		Example: "  plugyard wait CP-1 Charging\n" +
			"  plugyard wait CP-1 Finishing -n 2 --timeout 2m\n" +
			"  plugyard wait 'FAST-*' online\n" +
			"  plugyard wait CP-1 CP-2 Available --timeout 0",
		Args: cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			patterns := args[:len(args)-1]
			state, connection, err := waitState(args[len(args)-1])
			if err != nil {
				return err
			}
			for _, p := range patterns {
				if err := checkPattern(p); err != nil {
					return err
				}
			}
			switch {
			case connection && cmd.Flags().Changed("connector"):
				return &UsageError{Err: fmt.Errorf("%s is a state of the whole charger, so it takes no --connector", state)}
			case connector < 1:
				return &UsageError{Err: errors.New("the connector number must be 1 or more")}
			case timeout < 0:
				return &UsageError{Err: errors.New("--timeout must be a time such as 30s, or 0 to wait without limit")}
			}

			ctx := cmd.Context()
			var deadline <-chan time.Time
			if timeout > 0 {
				timer := time.NewTimer(timeout)
				defer timer.Stop()
				deadline = timer.C
			}
			ticker := time.NewTicker(waitInterval)
			defer ticker.Stop()

			for first := true; ; first = false {
				// lines says where each charger is; missing counts those
				// not in the state.
				var lines []string
				missing := 0

				w, err := loadWorld(ctx, siteName)
				if err != nil {
					return err
				}
				seen := map[string]bool{}
				for _, p := range patterns {
					matched, err := w.match(p, nil)
					if err == nil && len(matched) == 0 {
						err = fmt.Errorf("no charger matches %q", p)
					}
					if err != nil {
						w.close()
						// A charger that was never there is a mistake
						// in the command.
						if first {
							return &UsageError{Err: err}
						}
						return err
					}
					for _, t := range matched {
						key := t.state.Site + "/" + t.state.ChargePointID
						if seen[key] {
							continue
						}
						seen[key] = true

						now := t.state.Connection
						if !t.state.Powered {
							now = charger.Stopped
						}
						what := ""
						if !connection && t.state.Powered {
							found := false
							for _, k := range t.state.Connectors {
								if k.ID == connector {
									now, found = k.Status, true
								}
							}
							if !found {
								w.close()
								return &UsageError{Err: fmt.Errorf("charger %s has no connector %d", t.state.ChargePointID, connector)}
							}
							what = fmt.Sprintf("connector %d is ", connector)
						}
						if now == state {
							lines = append(lines, fmt.Sprintf("%s: %s%s", t.state.ChargePointID, what, now))
						} else {
							missing++
							lines = append(lines, fmt.Sprintf("%s: %s%s, not %s", t.state.ChargePointID, what, now, state))
						}
					}
				}
				w.close()

				if missing == 0 {
					for _, l := range lines {
						fmt.Fprintln(cmd.OutOrStdout(), l)
					}
					return nil
				}

				select {
				case <-ctx.Done():
					return errFailed
				case <-deadline:
					for _, l := range lines {
						if strings.Contains(l, ", not ") {
							fmt.Fprintf(cmd.ErrOrStderr(), "%s, after %s\n", l, timeout)
						}
					}
					return errFailed
				case <-ticker.C:
				}
			}
		},
	}

	flags := cmd.Flags()
	flags.IntVarP(&connector, "connector", "n", 1, "connector number, for a connector status")
	flags.StringVarP(&siteName, "site", "s", "", "only wait for chargers of this site")
	flags.DurationVar(&timeout, "timeout", 30*time.Second, "give up after this long; 0 waits without limit")
	return cmd
}
