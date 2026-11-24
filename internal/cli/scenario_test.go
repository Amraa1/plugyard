package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Amraa1/plugyard/internal/csmstest"
	"github.com/Amraa1/plugyard/internal/ocpp/v16"
)

// writeScenario writes a scenario file and returns its path.
func writeScenario(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "scenario.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

const morning = `
name: morning
steps:
  - { at: 0s,  charger: "FAST-*", do: plug, ev: small }
  - { at: 5s,  charger: "FAST-*", do: tap, tag: TAG-1 }
  - { at: 10m, charger: FAST-02, do: fault, code: GroundFailure }
  - { at: 12m, charger: FAST-02, do: clear }
  - { at: 20m, charger: "FAST-*", do: unplug }
`

func TestScenarioRunsAgainstASite(t *testing.T) {
	useTempDataDir(t)
	csms := csmstest.Start(t)
	// At 600x, the scenario's twenty minutes take two seconds.
	startSiteUp(t, csms, writeSite(t, csms.URL(), fiveChargers), fiveIDs, "--speed", "600")

	started := time.Now()
	code, stdout, stderr := run("scenario", "run", writeScenario(t, morning))
	took := time.Since(started)
	if code != ExitOK {
		t.Fatalf("scenario run: exit %d, stderr %q, stdout:\n%s", code, stderr, stdout)
	}
	if took < 1900*time.Millisecond || took > 8*time.Second {
		t.Errorf("the scenario took %s, want about 2s: twenty minutes at the site's speed of 600", took)
	}
	for _, want := range []string{
		"Running scenario morning against site depot: 5 steps over 20m0s (2s at speed 600)\n",
		"[0s] step 1: plug --ev small on FAST-*\n",
		"FAST-01  connector 1  ok  EV plugged in (40 kWh, 50%)\n",
		"4 of 4 succeeded\n",
		"[5s] step 2: tap TAG-1 on FAST-*\n",
		"[10m0s] step 3: fault GroundFailure on FAST-02\nFAST-02  connector 1  ok  fault GroundFailure reported, transaction",
		"[12m0s] step 4: clear on FAST-02\n",
		"[20m0s] step 5: unplug on FAST-*\n",
		"Scenario morning finished: 5 steps, all 14 actions succeeded\n",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("the output is missing %q:\n%s", want, stdout)
		}
	}

	// The CSMS saw what the scenario did.
	for _, id := range fiveIDs[:4] {
		csms.WaitForCalls(t, id, v16.ActionStartTransaction, 1)
		stop := decodeCall[v16.StopTransactionRequest](t, csms.WaitForCalls(t, id, v16.ActionStopTransaction, 1)[0])
		// At this speed the EVs took in energy between the steps.
		if id != "FAST-02" && (stop.Reason != v16.ReasonEVDisconnected || stop.MeterStop < 1000) {
			t.Errorf("%s: StopTransaction = %+v, want EVDisconnected with energy on the meter", id, stop)
		}
		if id == "FAST-02" && stop.Reason != v16.ReasonOther {
			t.Errorf("FAST-02: StopTransaction reason = %s, want Other: it faulted", stop.Reason)
		}
	}
	if n := len(csms.Calls("LOBBY-1")); n > 4 {
		t.Errorf("LOBBY-1 made %d calls; the scenario does not name it", n)
	}
}

func TestScenarioFailures(t *testing.T) {
	useTempDataDir(t)
	csms := csmstest.Start(t)
	startSiteUp(t, csms, writeSite(t, csms.URL(), fiveChargers), fiveIDs)

	path := writeScenario(t, `
name: rough
steps:
  - { at: 0s, charger: FAST-01, do: unplug }
  - { at: 0s, charger: "NOPE-*", do: plug }
  - { at: 0s, charger: "FAST-*", do: plug }
  - { at: 0s, charger: FAST-01, do: plug }
`)

	// Every failure is reported, the run goes on, and the exit code is 1.
	code, stdout, _ := run("scenario", "run", path)
	if code != ExitFailure {
		t.Fatalf("scenario run: exit %d, want %d:\n%s", code, ExitFailure, stdout)
	}
	for _, want := range []string{
		"step 1: unplug on FAST-01\nFAST-01  connector 1  failed  the connector has no EV\n",
		"step 2: plug on NOPE-*\n  failed  no running charger matches \"NOPE-*\"\n",
		"step 3: plug on FAST-*\n", "4 of 4 succeeded\n",
		"step 4: plug on FAST-01\nFAST-01  connector 1  failed  the connector already has an EV\n",
		"Scenario rough finished: 4 steps, 3 of 7 actions failed\n",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("the output is missing %q:\n%s", want, stdout)
		}
	}

	// --fail-fast stops at the first step that failed.
	mustRun(t, "exec", "FAST-*", "unplug")
	code, stdout, _ = run("scenario", "run", path, "--fail-fast")
	if code != ExitFailure || strings.Contains(stdout, "step 2") || !strings.Contains(stdout, "Scenario rough stopped at the first failure, after 1 step of 4\n") {
		t.Errorf("scenario run --fail-fast: exit %d:\n%s", code, stdout)
	}

	// As JSON, one object per charger and step.
	code, stdout, _ = run("scenario", "run", path, "--format", "json")
	type line struct {
		Step          int    `json:"step"`
		At            string `json:"at"`
		Do            string `json:"do"`
		ChargePointID string `json:"chargePointId"`
		Connector     int    `json:"connector"`
		OK            bool   `json:"ok"`
		Message       string `json:"message"`
	}
	var got []line
	for _, raw := range lines(stdout) {
		var l line
		if err := json.Unmarshal([]byte(raw), &l); err != nil {
			t.Fatalf("line %q is not a JSON object: %v", raw, err)
		}
		got = append(got, l)
	}
	if code != ExitFailure || len(got) != 7 {
		t.Fatalf("scenario run --format json: exit %d, %d lines:\n%s", code, len(got), stdout)
	}
	if first := got[0]; first.Step != 1 || first.At != "0s" || first.Do != "unplug" || first.ChargePointID != "FAST-01" || first.Connector != 1 || first.OK || first.Message == "" {
		t.Errorf("first line = %+v", first)
	}
	if nobody := got[1]; nobody.Step != 2 || nobody.ChargePointID != "" || nobody.OK || !strings.Contains(nobody.Message, "no running charger matches") {
		t.Errorf("the line of the step that matched nothing = %+v", nobody)
	}
	if third := got[2]; third.Step != 3 || third.Do != "plug" || !third.OK {
		t.Errorf("third line = %+v", third)
	}
}

func TestScenarioPartsAndPowerSteps(t *testing.T) {
	useTempDataDir(t)
	csms := csmstest.Start(t)
	startSiteUp(t, csms, writeSite(t, csms.URL(), fiveChargers), fiveIDs)

	path := writeScenario(t, `
name: power-cut
steps:
  - { at: 0s,  charger: LOBBY-1, do: plug }
  - { at: 1s,  charger: LOBBY-1, do: tap, tag: TAG-1 }
  - { at: 30m, charger: LOBBY-1, do: kill }
  - { at: 31m, charger: LOBBY-1, do: start }
  - { at: 2h,  charger: LOBBY-1, do: unplug }
`)

	// One step, now: no waiting for its time.
	started := time.Now()
	out := mustRun(t, "scenario", "run", path, "--step", "1")
	if !strings.Contains(out, "Running scenario power-cut against site depot: 1 step over 0s\n") || !strings.Contains(out, "step 1: plug on LOBBY-1") || strings.Contains(out, "step 2") {
		t.Errorf("scenario run --step 1:\n%s", out)
	}

	// A part starts at once, and keeps the gaps between its steps. --speed
	// overrides the site's speed.
	out = mustRun(t, "scenario", "run", path, "--from", "2", "--to", "4", "--speed", "600")
	if took := time.Since(started); took > 8*time.Second {
		t.Errorf("steps 1 to 4 took %s; the part should start at once and run at speed 600", took)
	}
	for _, want := range []string{
		"3 steps over 30m59s (3.098s at speed 600)\n",
		"step 2: tap TAG-1 on LOBBY-1\nLOBBY-1  connector 1  ok  tag accepted, transaction",
		"step 3: kill on LOBBY-1\nLOBBY-1  ok  killed\n",
		"step 4: start on LOBBY-1\nLOBBY-1  ok  started\n",
		"all 3 actions succeeded",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the output is missing %q:\n%s", want, out)
		}
	}
	// The power cut is reported when the charger is back.
	stop := decodeCall[v16.StopTransactionRequest](t, csms.WaitForCalls(t, "LOBBY-1", v16.ActionStopTransaction, 1)[0])
	if stop.Reason != v16.ReasonPowerLoss {
		t.Errorf("StopTransaction reason = %s, want PowerLoss", stop.Reason)
	}

	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"--step", "9"}, "the scenario has steps 1 to 5; steps 9 to 9 cannot be run"},
		{[]string{"--from", "4", "--to", "2"}, "steps 4 to 2 cannot be run"},
		{[]string{"--step", "1", "--from", "1"}, "give either --step, or --from and --to, not both"},
		{[]string{"--speed", "0"}, "--speed must be above 0"},
		{[]string{"--format", "xml"}, `unknown format "xml"`},
	} {
		code, _, stderr := run(append([]string{"scenario", "run", path}, tc.args...)...)
		if code != ExitUsage || !strings.Contains(stderr, tc.want) {
			t.Errorf("%v: exit %d, stderr %q; want exit %d and %q", tc.args, code, stderr, ExitUsage, tc.want)
		}
	}
}

func TestScenarioIsInterruptedByCtrlC(t *testing.T) {
	useTempDataDir(t)
	csms := csmstest.Start(t)
	startRun(t, csms, "CP-1")

	path := writeScenario(t, "name: slow\nsteps:\n  - { at: 0s, charger: CP-1, do: plug }\n  - { at: 1h, charger: CP-1, do: unplug }\n")
	stdout, stop := background(t, "scenario", "run", path)
	eventually(t, "the first step has run", func() bool { return strings.Contains(stdout.String(), "EV plugged in") })
	if code := stop(); code != ExitFailure || !strings.Contains(stdout.String(), "Scenario slow was interrupted after 1 step; the site keeps running\n") {
		t.Errorf("interrupted scenario: exit %d:\n%s", code, stdout.String())
	}
	// The site is still there.
	if row := psRow(t, "CP-1"); !strings.Contains(row, "online 1:Preparing") {
		t.Errorf("ps row after the interrupt = %q", row)
	}
}

func TestScenarioNeedsARunningSite(t *testing.T) {
	useTempDataDir(t)
	path := writeScenario(t, morning)

	code, _, stderr := run("scenario", "run", path)
	if code != ExitFailure || !strings.Contains(stderr, `no site is running; start one with "plugyard site up" first`) {
		t.Errorf("with nothing running: exit %d, stderr %q", code, stderr)
	}

	csms := csmstest.Start(t)
	startRun(t, csms, "CP-1")
	startRun(t, csms, "CP-2")
	code, _, stderr = run("scenario", "run", path)
	if code != ExitFailure || !strings.Contains(stderr, "several sites are running (CP-1, CP-2); choose one with -s") {
		t.Errorf("with two sites running: exit %d, stderr %q", code, stderr)
	}
	code, _, stderr = run("scenario", "run", path, "-s", "depot")
	if code != ExitFailure || !strings.Contains(stderr, `site "depot" is not running; a scenario runs against a site that is up`) {
		t.Errorf("against a site that is not running: exit %d, stderr %q", code, stderr)
	}
	// -s picks one of several; its chargers are the only ones matched.
	code, stdout, _ := run("scenario", "run", writeScenario(t, "name: one\nsteps:\n  - { at: 0s, charger: \"CP-*\", do: plug }\n"), "-s", "CP-2")
	if code != ExitOK || !strings.Contains(stdout, "CP-2  connector 1  ok") || strings.Contains(stdout, "CP-1 ") {
		t.Errorf("scenario run -s CP-2: exit %d:\n%s", code, stdout)
	}
}

func TestScenarioValidate(t *testing.T) {
	useTempDataDir(t)
	path := writeScenario(t, morning)

	// With no site to look at, the file alone is checked.
	want := "Scenario morning is valid: 5 steps over 20m0s\n" +
		"  STEP  AT     CHARGER  ACTION\n" +
		"  1     0s     FAST-*   plug --ev small\n" +
		"  2     5s     FAST-*   tap TAG-1\n" +
		"  3     10m0s  FAST-02  fault GroundFailure\n" +
		"  4     12m0s  FAST-02  clear\n" +
		"  5     20m0s  FAST-*   unplug\n" +
		"No site is running, so the charger patterns were not checked; name a site with -s, or start one.\n"
	if out := mustRun(t, "scenario", "validate", path); out != want {
		t.Errorf("scenario validate =\n%s\nwant\n%s", out, want)
	}

	// With a site, each step says how many chargers it would act on.
	csms := csmstest.Start(t)
	startSiteUp(t, csms, writeSite(t, csms.URL(), fiveChargers), fiveIDs, "--speed", "60")
	out := mustRun(t, "scenario", "validate", writeScenario(t, morning+"  - { at: 25m, charger: FAST-99, do: unplug }\n"))
	for _, piece := range []string{
		"Scenario morning is valid: 6 steps over 25m0s (25s against site depot, which runs at speed 60)\n",
		"CHARGERS OF depot\n",
		"plug --ev small      4\n",
		"fault GroundFailure  1\n",
		"FAST-99  unplug               none\n",
		"1 step matches no charger of site depot and would fail\n",
	} {
		if !strings.Contains(out, piece) {
			t.Errorf("scenario validate is missing %q:\n%s", piece, out)
		}
	}
	// Nothing was sent to a charger.
	if n := len(csms.Calls("FAST-01")); n > 4 {
		t.Errorf("FAST-01 made %d calls during validate", n)
	}

	// A file with mistakes lists all of them and exits with 2.
	bad := writeScenario(t, "name: bad\nsteps:\n  - { at: 0s, charger: CP-1, do: tap }\n  - { at: 1s, charger: CP-1, do: fly }\n")
	for _, command := range []string{"validate", "run"} {
		code, stdout, stderr := run("scenario", command, bad)
		if code != ExitUsage || stdout != "" || !strings.Contains(stderr, "step 1: tag is required for tap") || !strings.Contains(stderr, `step 2: do "fly" is unknown`) {
			t.Errorf("scenario %s of a bad file: exit %d, stdout %q, stderr %q", command, code, stdout, stderr)
		}
	}
	if code, _, stderr := run("scenario", "validate", filepath.Join(t.TempDir(), "missing.yaml")); code != ExitUsage || stderr == "" {
		t.Errorf("scenario validate of a missing file: exit %d, stderr %q", code, stderr)
	}
	if code, _, stderr := run("scenario", "fly"); code != ExitUsage || !strings.Contains(stderr, `unknown command "fly" for "plugyard scenario"`) {
		t.Errorf("scenario fly: exit %d, stderr %q", code, stderr)
	}
}

func TestInitThenSiteUpThenScenario(t *testing.T) {
	useTempDataDir(t)
	csms := csmstest.Start(t)
	dir := filepath.Join(t.TempDir(), "my-site")

	out := mustRun(t, "init", dir, "--csms", csms.URL())
	for _, want := range []string{"Wrote site.yaml, Chargerfile and scenario.yaml in " + dir, "cd " + dir, "plugyard site up", "they connect to " + csms.URL(), "plugyard scenario run scenario.yaml"} {
		if !strings.Contains(out, want) {
			t.Errorf("init output is missing %q:\n%s", want, out)
		}
	}
	site := filepath.Join(dir, "site.yaml")

	// The files work as they are: validate, up, and the sample scenario.
	if out := mustRun(t, "site", "validate", "-f", site); !strings.Contains(out, "Site my-site is valid: 3 chargers, CSMS "+csms.URL()) {
		t.Errorf("site validate of the starter:\n%s", out)
	}
	startSiteUp(t, csms, site, []string{"CP-1", "CP-2", "CP-3"}, "--speed", "1200")
	scenario := filepath.Join(dir, "scenario.yaml")
	if out := mustRun(t, "scenario", "validate", scenario); !strings.Contains(out, "5 steps over 20m0s (1s against site my-site") || strings.Contains(out, "none") {
		t.Errorf("scenario validate of the starter:\n%s", out)
	}
	if out := mustRun(t, "scenario", "run", scenario); !strings.Contains(out, "Scenario first-charge finished: 5 steps, all 11 actions succeeded") {
		t.Errorf("scenario run of the starter:\n%s", out)
	}
	for _, id := range []string{"CP-1", "CP-2", "CP-3"} {
		csms.WaitForCalls(t, id, v16.ActionStopTransaction, 1)
	}

	// What is there is never overwritten by accident.
	if err := os.WriteFile(site, []byte("mine"), 0o600); err != nil {
		t.Fatal(err)
	}
	code, _, stderr := run("init", dir)
	if code != ExitFailure || !strings.Contains(stderr, "site.yaml, Chargerfile and scenario.yaml already exists here and was left alone; nothing was written") {
		t.Errorf("init over existing files: exit %d, stderr %q", code, stderr)
	}
	if data, _ := os.ReadFile(site); string(data) != "mine" {
		t.Error("init overwrote site.yaml without --force")
	}
	// Without --csms the address comes from the environment, with a default.
	mustRun(t, "init", dir, "--force")
	if data, _ := os.ReadFile(site); !strings.Contains(string(data), "url: ${CSMS_URL:-ws://localhost:9000/ocpp}") {
		t.Errorf("site.yaml after init --force:\n%s", data)
	}

	for _, bad := range []string{"localhost:9000", "http://localhost:9000/ocpp"} {
		if code, _, stderr := run("init", t.TempDir(), "--csms", bad); code != ExitUsage || !strings.Contains(stderr, "is not a CSMS address") {
			t.Errorf("init --csms %s: exit %d, stderr %q", bad, code, stderr)
		}
	}
}

func TestInitInTheCurrentDirectory(t *testing.T) {
	t.Chdir(t.TempDir())
	out := mustRun(t, "init")
	if !strings.HasPrefix(out, "Wrote site.yaml, Chargerfile and scenario.yaml.\n\nNext:\n  plugyard site up") || strings.Contains(out, "cd ") {
		t.Errorf("init output:\n%s", out)
	}
	for _, name := range starterFiles {
		if _, err := os.Stat(name); err != nil {
			t.Errorf("init did not write %s: %v", name, err)
		}
	}
	// The starter site works with nothing set: it falls back to localhost.
	if out := mustRun(t, "site", "validate"); !strings.Contains(out, "CSMS ws://localhost:9000/ocpp") {
		t.Errorf("site validate of the starter:\n%s", out)
	}
}

func TestSchemaCommand(t *testing.T) {
	for _, kind := range []string{"chargerfile", "site", "scenario"} {
		var doc struct {
			ID    string `json:"$id"`
			Title string `json:"title"`
		}
		if err := json.Unmarshal([]byte(mustRun(t, "schema", kind)), &doc); err != nil || !strings.HasSuffix(doc.ID, "/schemas/"+kind+".schema.json") || doc.Title == "" {
			t.Errorf("schema %s: %v, %+v", kind, err, doc)
		}
	}
	if code, _, stderr := run("schema", "nope"); code != ExitUsage || !strings.Contains(stderr, "want chargerfile, scenario and site") {
		t.Errorf("schema nope: exit %d, stderr %q", code, stderr)
	}
	if code, _, _ := run("schema"); code != ExitUsage {
		t.Errorf("schema with no argument: exit %d", code)
	}
}
