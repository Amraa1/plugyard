package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Amraa1/plugyard/internal/csmstest"
	"github.com/Amraa1/plugyard/internal/ocpp/v16"
)

// asCLIEnv makes the test binary behave as the plugyard command. "site up
// -d" starts a copy of the running program in the background; under "go
// test" that program is the test binary, so it has to be able to play the
// part.
const asCLIEnv = "PLUGYARD_TEST_AS_CLI"

func TestMain(m *testing.M) {
	if os.Getenv(asCLIEnv) == "1" {
		os.Exit(Run(os.Args[1:], os.Stdout, os.Stderr))
	}
	os.Exit(m.Run())
}

// writeSite writes a site file with its Chargerfile into a new directory and
// returns the site file's path.
func writeSite(t *testing.T, csmsURL, body string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "Chargerfile"), []byte(testChargerfile), 0o600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "site.yaml")
	content := "csms: { url: " + csmsURL + " }\n" + body
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

const fiveChargers = `
name: depot
evs:
  small: { battery: 40kWh, soc: 50% }
  big:   { battery: 100kWh, soc: 10%, curve: dc-fast }
chargers:
  fast:  { model: ./Chargerfile, count: 4, id: "FAST-{n:02}", ev: small }
  lobby: { model: ./Chargerfile, id: LOBBY-1 }
`

// startSiteUp runs "plugyard site up" in the background, as a first terminal
// would, and waits until every charger is online. It returns a function that
// reports the command's exit code once it has ended.
func startSiteUp(t *testing.T, csms *csmstest.CSMS, path string, ids []string, extra ...string) (stdout *syncBuffer, exited func() int) {
	t.Helper()
	runCtx, interrupt := context.WithCancel(context.Background())
	stdout = &syncBuffer{}
	var stderr syncBuffer
	exit := make(chan int, 1)
	args := append([]string{"site", "up", "-f", path}, extra...)
	go func() { exit <- RunContext(runCtx, args, stdout, &stderr) }()

	code := -1
	exited = func() int {
		if code == -1 {
			select {
			case code = <-exit:
			case <-time.After(10 * time.Second):
				t.Fatalf("site up did not exit; stderr: %s", stderr.String())
			}
		}
		return code
	}
	t.Cleanup(func() {
		interrupt()
		exited()
	})

	for _, id := range ids {
		csms.WaitForCalls(t, id, v16.ActionStatusNotification, 1)
	}
	waitForSite(t, ids[len(ids)-1])
	return stdout, exited
}

var fiveIDs = []string{"FAST-01", "FAST-02", "FAST-03", "FAST-04", "LOBBY-1"}

func TestSiteValidateShowsWhatTheFileCreates(t *testing.T) {
	path := writeSite(t, "ws://localhost:9000/ocpp", fiveChargers)

	code, stdout, stderr := run("site", "validate", "-f", path)
	want := "Site depot is valid: 5 chargers, CSMS ws://localhost:9000/ocpp\n" +
		"  fast   4 chargers  FAST-01 to FAST-04  Acme Wallbox\n" +
		"  lobby  1 charger   LOBBY-1             Acme Wallbox\n" +
		"EV presets: big, small\n"
	if code != ExitOK || stdout != want {
		t.Errorf("exit %d, stderr %q, stdout:\n%s\nwant:\n%s", code, stderr, stdout, want)
	}
}

func TestSiteValidateReportsProblemsAsUsageErrors(t *testing.T) {
	cases := map[string]struct{ body, wantErr string }{
		"template without a number": {
			"name: s\nchargers:\n  fast: { model: ./Chargerfile, count: 20, id: FAST }\n",
			`20 chargers need different IDs; add {n} to the template, for example "FAST-{n}"`,
		},
		"unset variable": {
			"name: ${PLUGYARD_TEST_UNSET_VARIABLE}\nchargers:\n  fast: { model: ./Chargerfile, id: A }\n",
			"PLUGYARD_TEST_UNSET_VARIABLE is not set",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			path := writeSite(t, "ws://localhost:9000/ocpp", tc.body)
			code, _, stderr := run("site", "validate", "-f", path)
			if code != ExitUsage || !strings.Contains(stderr, tc.wantErr) {
				t.Errorf("exit %d, stderr %q; want exit %d mentioning %q", code, stderr, ExitUsage, tc.wantErr)
			}
		})
	}

	t.Run("bad CSMS URL", func(t *testing.T) {
		path := writeSite(t, "http://localhost:9000/ocpp", "name: s\nchargers:\n  fast: { model: ./Chargerfile, id: A }\n")
		code, _, stderr := run("site", "validate", "-f", path)
		if code != ExitUsage || !strings.Contains(stderr, "ws:// or wss://") {
			t.Errorf("exit %d, stderr %q", code, stderr)
		}
	})

	t.Run("no site file here", func(t *testing.T) {
		t.Chdir(t.TempDir())
		code, _, stderr := run("site", "validate")
		if code != ExitUsage || !strings.Contains(stderr, "there is no site.yaml in this directory") {
			t.Errorf("exit %d, stderr %q", code, stderr)
		}
	})
}

func TestTheExampleSiteIsValid(t *testing.T) {
	code, stdout, stderr := run("site", "validate", "-f", filepath.Join("..", "..", "examples", "site.yaml"))
	if code != ExitOK || !strings.Contains(stdout, "21 chargers") || !strings.Contains(stdout, "FAST-001 to FAST-020") {
		t.Errorf("exit %d, stderr %q, stdout:\n%s", code, stderr, stdout)
	}
}

func TestSiteUpRunsEveryChargerUntilSiteDown(t *testing.T) {
	useTempDataDir(t)
	csms := csmstest.Start(t)
	path := writeSite(t, csms.URL(), fiveChargers)
	stdout, exited := startSiteUp(t, csms, path, fiveIDs)

	// ps and site ls see the site from a second terminal.
	_, ps, _ := run("ps")
	if lines := strings.Count(ps, "\n"); lines != 6 {
		t.Errorf("ps printed %d lines, want a header and 5 chargers:\n%s", lines, ps)
	}
	if !strings.Contains(ps, "FAST-03          depot  online      1:Available") {
		t.Errorf("ps does not list FAST-03 in site depot:\n%s", ps)
	}

	code, ls, _ := run("site", "ls")
	wantLs := fmt.Sprintf("SITE   CHARGERS  PROCESS  CSMS\ndepot  5         %-7d  %s\n", os.Getpid(), csms.URL())
	if code != ExitOK || ls != wantLs {
		t.Errorf("site ls printed:\n%s\nwant:\n%s", ls, wantLs)
	}

	// Starting it a second time is refused.
	code, _, stderr := run("site", "up", "-f", path)
	if code != ExitFailure || !strings.Contains(stderr, `site "depot": the site is already running`) {
		t.Errorf("second site up: exit %d, stderr %q", code, stderr)
	}

	// The foreground log shows every charger's events and messages.
	out := stdout.String()
	for _, want := range []string{
		"Starting site depot with 5 chargers\n",
		"FAST-01 | accepted by the CSMS",
		`LOBBY-1 > [2,`,
		`"BootNotification"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("site up output is missing %q", want)
		}
	}

	// site down, from the second terminal, ends the first one cleanly.
	code, down, stderr := run("site", "down", "-f", path)
	if code != ExitOK || down != "Site depot is down\n" {
		t.Errorf("site down: exit %d, stdout %q, stderr %q", code, down, stderr)
	}
	if got := exited(); got != ExitOK {
		t.Errorf("site up exited with %d after site down, want %d", got, ExitOK)
	}
	for _, id := range fiveIDs {
		if ends := csms.WaitDisconnects(t, id, 1); !ends[0].Clean {
			t.Errorf("%s was dropped, want a normal close", id)
		}
	}
	if _, ps, _ := run("ps"); strings.Contains(ps, "FAST") {
		t.Errorf("ps still lists chargers after site down:\n%s", ps)
	}

	code, _, stderr = run("site", "down", "-s", "depot")
	if code != ExitFailure || !strings.Contains(stderr, `site "depot" is not running`) {
		t.Errorf("site down when not running: exit %d, stderr %q", code, stderr)
	}
}

func TestPlugUsesEVPresets(t *testing.T) {
	useTempDataDir(t)
	csms := csmstest.Start(t)
	path := writeSite(t, csms.URL(), fiveChargers)
	startSiteUp(t, csms, path, fiveIDs)

	// The fast entry's default preset, the built-in EV for the lobby.
	_, stdout, _ := run("exec", "*-01", "plug")
	if stdout != "FAST-01  connector 1  ok  EV plugged in (40 kWh, 50%)\n" {
		t.Errorf("default preset: %q", stdout)
	}
	_, stdout, _ = run("exec", "LOBBY-1", "plug")
	if stdout != "LOBBY-1  connector 1  ok  EV plugged in (60 kWh, 20%)\n" {
		t.Errorf("built-in EV: %q", stdout)
	}

	// A named preset, then a flag on top of it.
	_, stdout, _ = run("exec", "FAST-02", "plug", "--ev", "big")
	if stdout != "FAST-02  connector 1  ok  EV plugged in (100 kWh, 10%)\n" {
		t.Errorf("named preset: %q", stdout)
	}
	_, stdout, _ = run("exec", "FAST-03", "plug", "--ev", "big", "--soc", "70%", "--curve", "standard")
	if stdout != "FAST-03  connector 1  ok  EV plugged in (100 kWh, 70%)\n" {
		t.Errorf("preset with a flag: %q", stdout)
	}

	code, stdout, _ := run("exec", "FAST-04", "plug", "--ev", "ghost")
	if code != ExitFailure || !strings.Contains(stdout, `failed  the site has no EV preset named "ghost"; it has big, small`) {
		t.Errorf("unknown preset: exit %d, %q", code, stdout)
	}

	code, _, stderr := run("exec", "FAST-04", "plug", "--curve", "warp")
	if code != ExitUsage || !strings.Contains(stderr, "unknown charge curve") {
		t.Errorf("unknown curve: exit %d, %q", code, stderr)
	}
}

func TestSiteUpFlagsOverrideTheFile(t *testing.T) {
	useTempDataDir(t)
	csms := csmstest.Start(t)
	path := writeSite(t, csms.URL(), "name: paced\nchargers:\n  c: { model: ./Chargerfile, count: 5, id: \"C-{n}\" }\n")

	started := time.Now()
	startSiteUp(t, csms, path, []string{"C-1", "C-2", "C-3", "C-4", "C-5"}, "--connect-rate", "20", "--speed", "360")

	// Five connections at 20 a second: the last one starts 200ms in.
	if elapsed := time.Since(started); elapsed < 180*time.Millisecond {
		t.Errorf("5 chargers were online after %s, want the 20-a-second pace to take about 200ms", elapsed)
	}

	// --speed reached the chargers: a minute of charging in well under one.
	run("exec", "C-1", "plug")
	run("exec", "C-1", "tap", "TAG")
	time.Sleep(300 * time.Millisecond)
	_, stdout, _ := run("ps", "--format", "json")
	for _, line := range strings.Split(strings.TrimSpace(stdout), "\n") {
		var state struct {
			ChargePointID string
			Connectors    []struct{ MeterWh float64 }
		}
		if err := json.Unmarshal([]byte(line), &state); err != nil {
			t.Fatalf("ps line %q: %v", line, err)
		}
		// 7.4 kW for 300ms at 360x is about 220 Wh; at 1x it would be 0.6.
		if state.ChargePointID == "C-1" && state.Connectors[0].MeterWh < 50 {
			t.Errorf("C-1 has delivered %.0f Wh, want about 220 at 360x", state.Connectors[0].MeterWh)
		}
	}

	for _, args := range [][]string{
		{"site", "up", "-f", path, "--speed", "0"},
		{"site", "up", "-f", path, "--connect-rate", "-1"},
		{"site", "up", "-f", path, "extra"},
		{"site", "sideways"},
	} {
		if code, _, stderr := run(args...); code != ExitUsage {
			t.Errorf("%v: exit %d, want %d (stderr %q)", args, code, ExitUsage, stderr)
		}
	}
}

func TestSiteUpDetachedKeepsRunningInTheBackground(t *testing.T) {
	dataDir := useTempDataDir(t)
	t.Setenv(asCLIEnv, "1")
	csms := csmstest.Start(t)
	path := writeSite(t, csms.URL(), fiveChargers)

	code, stdout, stderr := run("site", "up", "-d", "-f", path, "--speed", "60")
	if code != ExitOK {
		t.Fatalf("site up -d: exit %d, stdout %q, stderr %q", code, stdout, stderr)
	}
	// Whatever happens next, do not leave the background process behind.
	t.Cleanup(func() { run("site", "down", "-s", "depot") })

	if !strings.HasPrefix(stdout, "Site depot is running in the background with 5 chargers (process ") {
		t.Errorf("site up -d printed %q", stdout)
	}
	if strings.Contains(stdout, fmt.Sprintf("(process %d)", os.Getpid())) {
		t.Error("the site runs in this process, want a separate background one")
	}

	// The command has returned, and the chargers are online and reachable.
	for _, id := range fiveIDs {
		csms.WaitForCalls(t, id, v16.ActionStatusNotification, 1)
	}
	waitForSite(t, "LOBBY-1")
	code, stdout, _ = run("exec", "FAST-*", "plug")
	if code != ExitOK || !strings.HasSuffix(stdout, "4 of 4 succeeded\n") {
		t.Errorf("exec against the background site: exit %d\n%s", code, stdout)
	}

	// Its log went to a file in the data directory.
	logs, _ := filepath.Glob(filepath.Join(dataDir, "*.log"))
	if len(logs) != 1 {
		t.Fatalf("found %d log files in the data directory, want 1", len(logs))
	}
	if log, _ := os.ReadFile(logs[0]); !strings.Contains(string(log), "FAST-01 | accepted by the CSMS") {
		t.Errorf("the background log does not show the chargers starting:\n%s", log)
	}

	// A second start is refused, and site down ends the background process.
	code, _, stderr = run("site", "up", "-d", "-f", path)
	if code != ExitFailure || !strings.Contains(stderr, "already running") {
		t.Errorf("second site up -d: exit %d, stderr %q", code, stderr)
	}

	code, stdout, stderr = run("site", "down", "-f", path)
	if code != ExitOK || stdout != "Site depot is down\n" {
		t.Errorf("site down: exit %d, stdout %q, stderr %q", code, stdout, stderr)
	}
	for _, id := range fiveIDs {
		if ends := csms.WaitDisconnects(t, id, 1); !ends[0].Clean {
			t.Errorf("%s was dropped, want a normal close", id)
		}
	}
	if _, ls, _ := run("site", "ls"); strings.Contains(ls, "depot") {
		t.Errorf("site ls still lists the site:\n%s", ls)
	}
}

func TestSiteUpDetachedFailsWhenTheDataDirectoryIsUnusable(t *testing.T) {
	useTempDataDir(t)
	t.Setenv(asCLIEnv, "1")
	csms := csmstest.Start(t)
	path := writeSite(t, csms.URL(), fiveChargers)

	// The data directory is a file, so no site can run there.
	blocked := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(blocked, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PLUGYARD_DATA_DIR", blocked)

	code, _, stderr := run("site", "up", "-d", "-f", path)
	if code != ExitFailure {
		t.Errorf("exit %d, want %d; stderr %q", code, ExitFailure, stderr)
	}
}
