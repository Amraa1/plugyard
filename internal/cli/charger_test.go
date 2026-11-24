package cli

import (
	"sort"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/Amraa1/plugyard/internal/csmstest"
)

// flagNames lists the flags of a command with their defaults.
func flagNames(cmd *cobra.Command) string {
	var names []string
	cmd.Flags().VisitAll(func(f *pflag.Flag) {
		names = append(names, f.Name+"/"+f.Shorthand+"="+f.DefValue)
	})
	sort.Strings(names)
	return strings.Join(names, " ")
}

// Every command of the suite is a short command under a second name.
func TestChargerSuiteMirrorsTheShortCommands(t *testing.T) {
	root := newRoot()
	suite, _, err := root.Find([]string{"charger"})
	if err != nil || suite.Name() != "charger" {
		t.Fatalf("no charger suite: %v", err)
	}

	want := []string{"exec", "inspect", "kill", "logs", "ls", "restart", "rm", "run", "start", "stats", "stop", "wait"}
	var got []string
	for _, sub := range suite.Commands() {
		got = append(got, sub.Name())
		short := sub.Name()
		if short == "ls" {
			short = "ps"
		}
		top, _, err := root.Find([]string{short})
		if err != nil || top.Name() != short || top.Parent() != root {
			t.Errorf("charger %s has no short command %q", sub.Name(), short)
			continue
		}
		if flagNames(sub) != flagNames(top) {
			t.Errorf("charger %s and %s take different flags:\n%s\n%s", sub.Name(), short, flagNames(sub), flagNames(top))
		}
		if sub.Name() != "ls" && (sub.Use != top.Use || sub.Long != top.Long) {
			t.Errorf("charger %s and %s describe themselves differently", sub.Name(), short)
		}
	}
	sort.Strings(got)
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("the charger suite has %v, want %v", got, want)
	}
}

func TestChargerSuite(t *testing.T) {
	useTempDataDir(t)
	csms := csmstest.Start(t)
	startRun(t, csms, "CP-1")

	// The suite and the short commands print the same.
	for _, args := range [][]string{{"ps"}, {"ps", "-a", "--format", "json"}, {"inspect", "CP-1"}, {"logs", "CP-1", "--events"}} {
		suite := append([]string{"charger"}, args...)
		if suite[1] == "ps" {
			suite[1] = "ls"
		}
		if short, long := mustRun(t, args...), mustRun(t, suite...); short != long {
			t.Errorf("plugyard %v printed:\n%s\nplugyard %v printed:\n%s", args, short, suite, long)
		}
	}

	// It acts as the short commands act.
	if out := mustRun(t, "charger", "exec", "CP-1", "plug"); out != "CP-1  connector 1  ok  EV plugged in (60 kWh, 20%)\n" {
		t.Errorf("charger exec printed %q", out)
	}
	if out := mustRun(t, "charger", "wait", "CP-1", "Preparing"); out != "CP-1: connector 1 is Preparing\n" {
		t.Errorf("charger wait printed %q", out)
	}
	if row := psRow(t, "CP-1"); row != "CP-1 cp-1 CP-1 online 1:Preparing" {
		t.Errorf("ps row = %q", row)
	}

	// The suite alone shows its help; a command it does not have is a mistake.
	if out := mustRun(t, "charger"); !strings.Contains(out, "plugyard charger [command]") || !strings.Contains(out, `List running chargers (the same as "plugyard ps")`) {
		t.Errorf("plugyard charger printed:\n%s", out)
	}
	if code, _, stderr := run("charger", "up"); code != ExitUsage || !strings.Contains(stderr, `unknown command "up" for "plugyard charger"`) {
		t.Errorf("charger up: exit %d, stderr %q", code, stderr)
	}
	if code, _, stderr := run("charger", "ls", "extra"); code != ExitUsage || stderr == "" {
		t.Errorf("charger ls with an argument: exit %d", code)
	}
}
