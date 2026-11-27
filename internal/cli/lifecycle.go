package cli

import (
	"context"
	"fmt"
	"sort"

	"github.com/spf13/cobra"

	"github.com/Amraa1/plugyard/internal/control"
	"github.com/Amraa1/plugyard/internal/store"
)

// newPowerCommands returns start, stop, restart, kill and rm: the commands
// that power chargers on and off, or remove them.
func newPowerCommands() []*cobra.Command {
	specs := []struct {
		op, use, short, long string
	}{
		{control.OpStart, "start <charger>", "Power on stopped chargers",
			"Power on one charger, or every charger matching a pattern such as 'FAST-*'.\n" +
				"The charger comes back as it was saved, or in the state its site file\n" +
				"declares for it: it boots, reports its connectors, and sends anything it\n" +
				"still owed the CSMS. When its site is not running, the site is started in\n" +
				"the background with only the named chargers on."},
		{control.OpStop, "stop <charger>", "Shut chargers down in an orderly way",
			"Shut down one charger, or every charger matching a pattern: transactions\n" +
				"end with StopTransaction, then the connection closes. State is kept, and\n" +
				"\"plugyard start\" brings the charger back. Stopping the last running\n" +
				"charger of a site ends the site's process."},
		{control.OpRestart, "restart <charger>", "Stop chargers and start them again",
			"Shut down one charger, or every charger matching a pattern, as \"plugyard\n" +
				"stop\" does, and power it on again, as \"plugyard start\" does: transactions\n" +
				"end with StopTransaction, and the charger boots and reports its connectors.\n" +
				"A charger whose site file declares a state comes back in that state, which\n" +
				"undoes what was changed by hand since. The CSMS sees a new BootNotification;\n" +
				"for a reboot the charger does by itself, use \"plugyard exec <charger> reboot\"."},
		{control.OpKill, "kill <charger>", "Cut the power of chargers",
			"Cut the power of one charger, or of every charger matching a pattern: the\n" +
				"connection drops with no message, as in a real power cut. State is kept.\n" +
				"On the next start the charger boots and reports any transaction that was\n" +
				"running with StopTransaction, reason PowerLoss, and the last meter reading\n" +
				"it saved."},
		{control.OpRemove, "rm <charger>", "Wipe the saved state of chargers",
			"Remove one charger, or every charger matching a pattern, with everything\n" +
				"saved for it. A running charger is refused unless -f is given, which\n" +
				"stops it first. A charger its site file still declares is created afresh\n" +
				"by the next \"plugyard site up\"."},
	}

	var cmds []*cobra.Command
	for _, spec := range specs {
		var (
			siteName string
			format   string
			force    bool
		)
		cmd := &cobra.Command{
			Use:   spec.use,
			Short: spec.short,
			Long:  spec.long,
			Args:  cobra.ExactArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				return runPower(cmd, spec.op, args[0], siteName, format, force)
			},
		}
		cmd.Flags().StringVarP(&siteName, "site", "s", "", "only act on chargers of this site")
		addFormatFlag(cmd, &format)
		if spec.op == control.OpRemove {
			cmd.Flags().BoolVarP(&force, "force", "f", false, "stop a running charger first")
		}
		cmds = append(cmds, cmd)
	}
	return cmds
}

func runPower(cmd *cobra.Command, op, arg, siteName, format string, force bool) error {
	if err := checkFormat(format); err != nil {
		return err
	}
	if err := checkPattern(arg); err != nil {
		return err
	}
	ctx := cmd.Context()
	w, err := loadWorld(ctx, siteName)
	if err != nil {
		return err
	}
	defer w.close()

	results, started, err := powerOn(ctx, w, op, arg, force)
	if err != nil {
		return err
	}

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
		for _, info := range started {
			reportBackground(out, w.dir, info)
		}
	}
	if failed > 0 {
		return errFailed
	}
	return nil
}

// powerOn powers the chargers a charger argument names on or off, or
// removes them, and returns one result per charger, sorted by charge point
// ID, with the sites it had to start in the background for that. An error
// means nothing was done.
func powerOn(ctx context.Context, w *world, op, arg string, force bool) ([]control.ExecResult, []control.Info, error) {
	targets, err := w.match(arg, nil)
	if err != nil {
		return nil, nil, err
	}
	if len(targets) == 0 {
		return nil, nil, fmt.Errorf("no charger matches %q", arg)
	}

	// Group the chargers by site: a running site is asked to act, and a
	// site with no process is acted on through its saved state.
	type group struct {
		client *control.Client
		ids    []string
	}
	groups := map[string]*group{}
	var order []string
	for _, t := range targets {
		g := groups[t.state.Site]
		if g == nil {
			g = &group{client: t.client}
			groups[t.state.Site] = g
			order = append(order, t.state.Site)
		}
		g.ids = append(g.ids, t.state.ChargePointID)
	}

	var results []control.ExecResult
	var started []control.Info
	failAll := func(ids []string, reason string) {
		for _, id := range ids {
			results = append(results, control.ExecResult{ChargePointID: id, Message: reason})
		}
	}
	okAll := func(ids []string, message string) {
		for _, id := range ids {
			results = append(results, control.ExecResult{ChargePointID: id, OK: true, Message: message})
		}
	}

	for _, name := range order {
		g := groups[name]
		if g.client != nil {
			got, err := g.client.Power(ctx, control.PowerRequest{ChargePointIDs: g.ids, Op: op, Force: force})
			if err != nil {
				failAll(g.ids, fmt.Sprintf("site %q did not answer: %v", name, err))
				continue
			}
			results = append(results, got...)
			continue
		}

		switch op {
		case control.OpStop:
			okAll(g.ids, "already stopped")
		case control.OpKill:
			failAll(g.ids, "the charger is not running")
		case control.OpStart, control.OpRestart:
			st, err := store.OpenExisting(w.dir, name)
			if err == nil {
				err = st.SetPower(g.ids)
				st.Close()
			}
			var info control.Info
			if err == nil {
				info, err = startBackground(ctx, w.dir, name)
			}
			if err != nil {
				failAll(g.ids, err.Error())
				continue
			}
			started = append(started, info)
			okAll(g.ids, "started")
		case control.OpRemove:
			st, err := store.OpenExisting(w.dir, name)
			if err == nil {
				err = st.Apply(store.Change{Remove: g.ids})
			}
			if err != nil {
				if st != nil {
					st.Close()
				}
				failAll(g.ids, err.Error())
				continue
			}
			left, _ := st.Chargers()
			st.Close()
			if len(left) == 0 {
				_ = store.Delete(w.dir, name)
			}
			okAll(g.ids, "removed")
		}
	}
	sort.SliceStable(results, func(i, j int) bool { return results[i].ChargePointID < results[j].ChargePointID })
	return results, started, nil
}
