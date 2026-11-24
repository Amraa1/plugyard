package cli

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/Amraa1/plugyard/internal/control"
	"github.com/Amraa1/plugyard/internal/scenario"
	"github.com/Amraa1/plugyard/schemas"
)

func newScenario() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "scenario",
		Short: "Drive a running site from a file of timed actions",
		Long: "A scenario is a YAML file of steps. Each step is an action on chargers at a\n" +
			"time counted from the start of the run:\n\n" +
			"  name: commute\n" +
			"  steps:\n" +
			"    - { at: 0s,  charger: \"FAST-*\", do: plug, ev: longrange }\n" +
			"    - { at: 5s,  charger: \"FAST-*\", do: tap, tag: 04A1B2C3 }\n" +
			"    - { at: 10m, charger: FAST-003, do: fault, code: GroundFailure }\n" +
			"    - { at: 30m, charger: \"FAST-*\", do: unplug }\n\n" +
			"A step does what \"plugyard exec\" does (" + strings.Join(scenario.Actions[:8], ", ") + ")\n" +
			"or powers chargers (" + strings.Join(scenario.Actions[8:], ", ") + "), with the same options:\n" +
			"connector, tag, ev, battery, soc, maxPower, targetSoc, curve, code, info,\n" +
			"vendorCode and for.",
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) > 0 {
				return &UsageError{Err: fmt.Errorf("unknown command %q for \"plugyard scenario\"", args[0])}
			}
			return cmd.Help()
		},
	}
	cmd.AddCommand(newScenarioRun(), newScenarioValidate())
	return cmd
}

// loadScenario reads a scenario file. Anything wrong with it is a
// validation error.
func loadScenario(path string) (*scenario.Scenario, error) {
	sc, err := scenario.Load(path)
	if err != nil {
		return nil, &UsageError{Err: err}
	}
	return sc, nil
}

// runningSite finds the site a scenario runs against: the one named, or the
// only one running.
func runningSite(ctx context.Context, siteName string) (control.Info, error) {
	w, err := loadWorld(ctx, "")
	if err != nil {
		return control.Info{}, err
	}
	defer w.close()

	var names []string
	for _, client := range w.clients {
		if client.Info.Site == siteName {
			return client.Info, nil
		}
		names = append(names, client.Info.Site)
	}
	switch {
	case siteName != "":
		return control.Info{}, fmt.Errorf("site %q is not running; a scenario runs against a site that is up", siteName)
	case len(names) == 0:
		return control.Info{}, errors.New("no site is running; start one with \"plugyard site up\" first")
	case len(names) > 1:
		return control.Info{}, fmt.Errorf("several sites are running (%s); choose one with -s", strings.Join(names, ", "))
	}
	return w.clients[0].Info, nil
}

// stepResult is one line of what a scenario did, as --format json prints it.
type stepResult struct {
	Step int    `json:"step"`
	At   string `json:"at"`
	Do   string `json:"do"`
	control.ExecResult
}

func newScenarioRun() *cobra.Command {
	var (
		siteName string
		format   string
		speed    float64
		failFast bool
		only     int
		from, to int
	)

	cmd := &cobra.Command{
		Use:   "run <file>",
		Short: "Run a scenario against a running site",
		Long: "Carry out the steps of a scenario against a site that is up: the one named\n" +
			"with -s, or the only one running.\n\n" +
			"Step times follow the site's speed. Against a site started with --speed 60,\n" +
			"a step at 30m runs after 30 seconds, when the EVs have charged for 30\n" +
			"simulated minutes. --speed overrides that.\n\n" +
			"A step that fails on a charger does not stop the run: every failure is\n" +
			"reported, and the exit code is 1 if there was any. --fail-fast stops at the\n" +
			"first step that failed. A tag the CSMS rejects is an outcome, not a failure.\n\n" +
			"To try steps while writing a scenario, --step N runs one step now, and\n" +
			"--from N and --to N run a part, without the wait before its first step.",
		Example: "  plugyard scenario run commute.yaml\n" +
			"  plugyard scenario run commute.yaml -s depot-north --fail-fast\n" +
			"  plugyard scenario run commute.yaml --step 3\n" +
			"  plugyard scenario run commute.yaml --from 2 --to 4",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := checkFormat(format); err != nil {
				return err
			}
			sc, err := loadScenario(args[0])
			if err != nil {
				return err
			}

			flags := cmd.Flags()
			steps, partial := sc.Steps, false
			switch {
			case flags.Changed("step") && (flags.Changed("from") || flags.Changed("to")):
				return &UsageError{Err: errors.New("give either --step, or --from and --to, not both")}
			case flags.Changed("step"):
				from, to = only, only
				fallthrough
			case flags.Changed("from") || flags.Changed("to"):
				if !flags.Changed("from") && !flags.Changed("step") {
					from = 1
				}
				if !flags.Changed("to") && !flags.Changed("step") {
					to = len(sc.Steps)
				}
				if from < 1 || to > len(sc.Steps) || from > to {
					return &UsageError{Err: fmt.Errorf("the scenario has steps 1 to %d; steps %d to %d cannot be run", len(sc.Steps), from, to)}
				}
				steps, partial = sc.Steps[from-1:to], true
			}
			if flags.Changed("speed") && speed <= 0 {
				return &UsageError{Err: errors.New("--speed must be above 0")}
			}

			ctx, out := cmd.Context(), cmd.OutOrStdout()
			site, err := runningSite(ctx, siteName)
			if err != nil {
				return err
			}
			if !flags.Changed("speed") {
				speed = site.Speed
				if speed <= 0 {
					speed = 1
				}
			}
			// A part of a scenario starts at once.
			base := time.Duration(0)
			if partial {
				base = steps[0].At
			}
			scaled := func(d time.Duration) time.Duration { return time.Duration(float64(d-base) / speed) }

			text := format != formatJSON
			p := newPalette(out)
			if text {
				length := steps[len(steps)-1].At - base
				line := fmt.Sprintf("Running scenario %s against site %s: %s over %s", sc.Name, site.Site, countSteps(len(steps)), length)
				if speed != 1 && length > 0 {
					line += fmt.Sprintf(" (%s at speed %s)", scaled(steps[len(steps)-1].At).Round(time.Millisecond), strconv.FormatFloat(speed, 'f', -1, 64))
				}
				fmt.Fprintln(out, line)
			}

			started := time.Now()
			actions, failed, done := 0, 0, 0
			interrupted := false
		run:
			for _, step := range steps {
				if wait := scaled(step.At) - time.Since(started); wait > 0 {
					timer := time.NewTimer(wait)
					select {
					case <-ctx.Done():
						timer.Stop()
						interrupted = true
						break run
					case <-timer.C:
					}
				} else if ctx.Err() != nil {
					interrupted = true
					break
				}

				results := runStep(ctx, site.Site, step)
				done++
				stepFailed := 0
				for _, r := range results {
					actions++
					if !r.OK {
						stepFailed++
					}
				}
				failed += stepFailed

				if text {
					fmt.Fprintf(out, "\n%s step %d: %s on %s\n", p.bold("["+step.At.String()+"]"), step.Number, step.Describe(), step.Charger)
					writeExecResults(out, results, stepFailed)
				} else {
					lines := make([]stepResult, 0, len(results))
					for _, r := range results {
						lines = append(lines, stepResult{Step: step.Number, At: step.At.String(), Do: step.Do, ExecResult: r})
					}
					if err := writeJSONLines(out, lines); err != nil {
						return err
					}
				}
				if stepFailed > 0 && failFast {
					break
				}
			}

			if text {
				fmt.Fprintln(out)
				switch {
				case interrupted:
					fmt.Fprintf(out, "Scenario %s was interrupted after %s; the site keeps running\n", sc.Name, countSteps(done))
				case done < len(steps):
					fmt.Fprintf(out, "Scenario %s stopped at the first failure, after %s of %d\n", sc.Name, countSteps(done), len(steps))
				case failed > 0:
					fmt.Fprintf(out, "Scenario %s finished: %s, %s\n", sc.Name, countSteps(done), p.red(fmt.Sprintf("%d of %d actions failed", failed, actions)))
				default:
					fmt.Fprintf(out, "Scenario %s finished: %s, %s\n", sc.Name, countSteps(done), p.green(countSucceeded(actions)))
				}
			}
			if failed > 0 || done < len(steps) {
				return errFailed
			}
			return nil
		},
	}

	flags := cmd.Flags()
	flags.StringVarP(&siteName, "site", "s", "", "the site to run against (default: the only one running)")
	flags.Float64Var(&speed, "speed", 0, "how many times faster than written the steps run (default: the site's speed)")
	flags.BoolVar(&failFast, "fail-fast", false, "stop at the first step that failed")
	flags.IntVar(&only, "step", 0, "run only this step, now")
	flags.IntVar(&from, "from", 0, "run from this step on, starting now")
	flags.IntVar(&to, "to", 0, "run up to and including this step")
	addFormatFlag(cmd, &format)
	return cmd
}

// runStep carries out one step and returns one result per charger. A step
// that could act on no charger gives a single failed result that names none.
func runStep(ctx context.Context, siteName string, step scenario.Step) []control.ExecResult {
	fail := func(err error) []control.ExecResult {
		return []control.ExecResult{{Message: err.Error()}}
	}
	// The site is looked at afresh for every step: earlier steps may have
	// stopped or started chargers.
	w, err := loadWorld(ctx, siteName)
	if err != nil {
		return fail(err)
	}
	defer w.close()

	var results []control.ExecResult
	if step.Power() {
		results, _, err = powerOn(ctx, w, step.Do, step.Charger, false)
	} else {
		results, err = execOn(ctx, w, step.Charger, step.Request)
	}
	if err != nil {
		return fail(err)
	}
	return results
}

func countSucceeded(actions int) string {
	if actions == 1 {
		return "its 1 action succeeded"
	}
	return fmt.Sprintf("all %d actions succeeded", actions)
}

func countSteps(n int) string {
	if n == 1 {
		return "1 step"
	}
	return fmt.Sprintf("%d steps", n)
}

func newScenarioValidate() *cobra.Command {
	var siteName string

	cmd := &cobra.Command{
		Use:   "validate <file>",
		Short: "Check a scenario and show its plan, without running it",
		Long: "Check a scenario file and list its steps. Nothing is sent to any charger.\n\n" +
			"When a site is named with -s, or exactly one site is running, the plan also\n" +
			"says how many of that site's chargers each step would act on, so that a\n" +
			"pattern that matches nothing shows up before the run.",
		Example: "  plugyard scenario validate commute.yaml\n" +
			"  plugyard scenario validate commute.yaml -s depot-north",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			sc, err := loadScenario(args[0])
			if err != nil {
				return err
			}
			ctx, out := cmd.Context(), cmd.OutOrStdout()
			p := newPalette(out)

			// The site to check patterns against, if there is one to pick.
			note := ""
			speed := 1.0
			if siteName == "" {
				switch site, err := runningSite(ctx, ""); {
				case err == nil:
					siteName, speed = site.Site, site.Speed
				case strings.HasPrefix(err.Error(), "several"):
					note = "Several sites are running, so the charger patterns were not checked; name a site with -s."
				default:
					note = "No site is running, so the charger patterns were not checked; name a site with -s, or start one."
				}
			}
			var w *world
			if siteName != "" {
				if w, err = loadWorld(ctx, siteName); err != nil {
					return err
				}
				defer w.close()
				for _, client := range w.clients {
					if client.Info.Site == siteName {
						speed = client.Info.Speed
					}
				}
			}

			line := fmt.Sprintf("Scenario %s is valid: %s over %s", sc.Name, countSteps(len(sc.Steps)), sc.Duration())
			if speed > 0 && speed != 1 {
				line += fmt.Sprintf(" (%s against site %s, which runs at speed %s)",
					time.Duration(float64(sc.Duration())/speed).Round(time.Millisecond), siteName, strconv.FormatFloat(speed, 'f', -1, 64))
			}
			fmt.Fprintln(out, line)

			header := []cell{plain("  STEP"), plain("AT"), plain("CHARGER"), plain("ACTION")}
			if w != nil {
				header = append(header, plain("CHARGERS OF "+siteName))
			}
			rows := [][]cell{header}
			unmatched := 0
			for _, step := range sc.Steps {
				row := []cell{plain("  " + strconv.Itoa(step.Number)), plain(step.At.String()), plain(step.Charger), plain(step.Describe())}
				if w != nil {
					matches, _ := w.match(step.Charger, nil)
					count := cell{plain: strconv.Itoa(len(matches)), rendered: strconv.Itoa(len(matches))}
					if len(matches) == 0 {
						unmatched++
						count = cell{plain: "none", rendered: p.red("none")}
					}
					row = append(row, count)
				}
				rows = append(rows, row)
			}
			writeTable(out, rows)

			switch {
			case note != "":
				fmt.Fprintln(out, note)
			case unmatched == 1:
				fmt.Fprintf(out, "%s no charger of site %s and would fail\n", p.yellow("1 step matches"), siteName)
			case unmatched > 1:
				fmt.Fprintf(out, "%s no charger of site %s and would fail\n", p.yellow(countSteps(unmatched)+" match"), siteName)
			}
			return nil
		},
	}
	cmd.Flags().StringVarP(&siteName, "site", "s", "", "check the charger patterns against this site (default: the only one running)")
	return cmd
}

//go:embed starter/*
var starter embed.FS

// starterFiles are what "plugyard init" writes, in the order it lists them.
var starterFiles = []string{"site.yaml", "Chargerfile", "scenario.yaml"}

// starterURL is the CSMS line of the starter site file, which --csms
// replaces.
const starterURL = "${CSMS_URL:-ws://localhost:9000/ocpp}"

func newInit() *cobra.Command {
	var (
		csms  string
		force bool
	)
	cmd := &cobra.Command{
		Use:   "init [directory]",
		Short: "Write a starter site file, Chargerfile and scenario",
		Long: "Write three files to start from, in the current directory or the one\n" +
			"named: site.yaml with three chargers, the Chargerfile they are made from,\n" +
			"and scenario.yaml, a sample scenario. \"plugyard site up\" works on them as\n" +
			"they are.\n\n" +
			"The chargers connect to the CSMS_URL environment variable, or to\n" +
			"ws://localhost:9000/ocpp when it is not set. --csms writes your CSMS's\n" +
			"address into the site file instead. A file that exists is never\n" +
			"overwritten unless --force is given.",
		Example: "  plugyard init\n" +
			"  plugyard init my-site --csms ws://localhost:8080/ocpp",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			dir := "."
			if len(args) == 1 {
				dir = args[0]
			}
			if csms != "" {
				u, err := url.Parse(csms)
				if err != nil || (u.Scheme != "ws" && u.Scheme != "wss") || u.Host == "" {
					return &UsageError{Err: fmt.Errorf("--csms %q is not a CSMS address: it must start with ws:// or wss://", csms)}
				}
			}

			if !force {
				var existing []string
				for _, name := range starterFiles {
					if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
						existing = append(existing, name)
					}
				}
				if len(existing) > 0 {
					return fmt.Errorf("%s already exists here and was left alone; nothing was written. Use --force to overwrite, or name another directory", joinWords(existing))
				}
			}
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return err
			}
			for _, name := range starterFiles {
				data, err := starter.ReadFile("starter/" + name)
				if err != nil {
					return err
				}
				text := string(data)
				if name == "site.yaml" && csms != "" {
					text = strings.Replace(text, starterURL, csms, 1)
				}
				if err := os.WriteFile(filepath.Join(dir, name), []byte(text), 0o644); err != nil {
					return err
				}
			}

			out := cmd.OutOrStdout()
			where := ""
			if dir != "." {
				where = " in " + dir
				fmt.Fprintf(out, "Wrote %s%s.\n\nNext:\n  cd %s\n", joinWords(starterFiles), where, dir)
			} else {
				fmt.Fprintf(out, "Wrote %s.\n\nNext:\n", joinWords(starterFiles))
			}
			target := csms
			if target == "" {
				target = "ws://localhost:9000/ocpp, or the CSMS_URL environment variable"
			}
			writeNext(out, target)
			return nil
		},
	}
	cmd.Flags().StringVar(&csms, "csms", "", "the address of your CSMS, ws:// or wss://, to write into site.yaml")
	cmd.Flags().BoolVar(&force, "force", false, "overwrite files that exist")
	return cmd
}

func writeNext(out io.Writer, target string) {
	writeTable(out, [][]cell{
		{plain("  plugyard site up"), plain("start 3 chargers; they connect to " + target)},
		{plain("  plugyard ps"), plain("in a second terminal: see them")},
		{plain("  plugyard scenario run scenario.yaml"), plain("plug in, charge, fault and unplug by script")},
	})
}

// joinWords joins names as a sentence lists them: "a, b and c".
func joinWords(words []string) string {
	switch len(words) {
	case 0:
		return ""
	case 1:
		return words[0]
	}
	return strings.Join(words[:len(words)-1], ", ") + " and " + words[len(words)-1]
}

func newSchema() *cobra.Command {
	kinds := schemas.Kinds()
	cmd := &cobra.Command{
		Use:   "schema <" + strings.Join(kinds, "|") + ">",
		Short: "Print the JSON schema of a plugyard file type",
		Long: "Print the JSON schema of the Chargerfile, the site file or the scenario\n" +
			"file. Editors use a schema to complete and check a file as you type.\n\n" +
			"The files \"plugyard init\" writes already point at the published schemas\n" +
			"with a first line such as:\n\n" +
			"  # yaml-language-server: $schema=" + schemas.URL("site") + "\n\n" +
			"Save a schema with this command when you work without a network.",
		Example:   "  plugyard schema site > site.schema.json",
		Args:      cobra.ExactArgs(1),
		ValidArgs: kinds,
		RunE: func(cmd *cobra.Command, args []string) error {
			data, err := schemas.Get(strings.ToLower(args[0]))
			if err != nil {
				return &UsageError{Err: fmt.Errorf("%w: want %s", err, joinWords(kinds))}
			}
			_, err = cmd.OutOrStdout().Write(data)
			return err
		},
	}
	return cmd
}
