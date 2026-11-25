package cli

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/Amraa1/plugyard/internal/api"
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

			wait := waiting{site: siteName, patterns: patterns, state: state, connection: connection, connector: connector, timeout: timeout}
			chargers, err := wait.run(cmd.Context())
			if err != nil {
				return err
			}
			missing := 0
			for _, c := range chargers {
				if !c.Reached {
					missing++
					fmt.Fprintf(cmd.ErrOrStderr(), "%s, after %s\n", waitedLine(c, state), timeout)
				}
			}
			if missing > 0 {
				return errFailed
			}
			for _, c := range chargers {
				fmt.Fprintln(cmd.OutOrStdout(), waitedLine(c, state))
			}
			return nil
		},
	}

	flags := cmd.Flags()
	flags.IntVarP(&connector, "connector", "n", 1, "connector number, for a connector status")
	flags.StringVarP(&siteName, "site", "s", "", "only wait for chargers of this site")
	flags.DurationVar(&timeout, "timeout", 30*time.Second, "give up after this long; 0 waits without limit")
	return cmd
}

// waitedLine says in words where a charger was when a wait ended.
func waitedLine(c api.Waited, state string) string {
	what := ""
	if c.Connector > 0 {
		what = fmt.Sprintf("connector %d is ", c.Connector)
	}
	if c.Reached {
		return fmt.Sprintf("%s: %s%s", c.ChargePointID, what, c.State)
	}
	return fmt.Sprintf("%s: %s%s, not %s", c.ChargePointID, what, c.State, state)
}

// waiting is a wait for a state: what the command and the API both do.
type waiting struct {
	site     string
	patterns []string
	// state is the state as waitState returned it, and connection says
	// whether it is one of the whole charger.
	state      string
	connection bool
	connector  int
	// timeout is how long to wait at most; 0 waits without limit.
	timeout time.Duration
}

// run looks at the chargers until every one of them is in the state or
// the time is up, and says where each was then.
func (q waiting) run(ctx context.Context) ([]api.Waited, error) {
	var deadline <-chan time.Time
	if q.timeout > 0 {
		timer := time.NewTimer(q.timeout)
		defer timer.Stop()
		deadline = timer.C
	}
	ticker := time.NewTicker(waitInterval)
	defer ticker.Stop()

	for first := true; ; first = false {
		chargers, err := q.look(ctx, first)
		if err != nil {
			return nil, err
		}
		missing := 0
		for _, c := range chargers {
			if !c.Reached {
				missing++
			}
		}
		if missing == 0 {
			return chargers, nil
		}
		select {
		case <-ctx.Done():
			return nil, errFailed
		case <-deadline:
			return chargers, nil
		case <-ticker.C:
		}
	}
}

// look says where every charger is right now.
func (q waiting) look(ctx context.Context, first bool) ([]api.Waited, error) {
	w, err := loadWorld(ctx, q.site)
	if err != nil {
		return nil, err
	}
	defer w.close()

	var chargers []api.Waited
	seen := map[string]bool{}
	for _, p := range q.patterns {
		matched, err := w.match(p, nil)
		if err == nil && len(matched) == 0 {
			err = &matchError{kind: matchNone, text: fmt.Sprintf("no charger matches %q", p)}
			// A charger that was never there is a mistake in the
			// command.
			if first {
				err = &UsageError{Err: err}
			}
		}
		if err != nil {
			return nil, err
		}
		for _, t := range matched {
			key := t.state.Site + "/" + t.state.ChargePointID
			if seen[key] {
				continue
			}
			seen[key] = true

			c := api.Waited{Site: t.state.Site, ChargePointID: t.state.ChargePointID, State: t.state.Connection}
			if !t.state.Powered {
				c.State = charger.Stopped
			}
			if !q.connection && t.state.Powered {
				found := false
				for _, k := range t.state.Connectors {
					if k.ID == q.connector {
						c.State, found = k.Status, true
					}
				}
				if !found {
					return nil, &UsageError{Err: fmt.Errorf("charger %s has no connector %d", t.state.ChargePointID, q.connector)}
				}
				c.Connector = q.connector
			}
			c.Reached = c.State == q.state
			chargers = append(chargers, c)
		}
	}
	return chargers, nil
}
