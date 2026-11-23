package cli

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Amraa1/plugyard/internal/control"
	"github.com/Amraa1/plugyard/internal/csmstest"
	"github.com/Amraa1/plugyard/internal/datadir"
	"github.com/Amraa1/plugyard/internal/ocpp/v16"
)

// useTempDataDir points plugyard at an empty data directory with a short
// path, since socket paths have a tight length limit.
func useTempDataDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "py")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	t.Setenv(datadir.Env, dir)
	return dir
}

// startRun runs "plugyard run" for a charger in the background, as a first
// terminal would, and waits until the charger is online.
func startRun(t *testing.T, csms *csmstest.CSMS, id string, extra ...string) {
	t.Helper()
	path := writeChargerfile(t, testChargerfile)

	runCtx, interrupt := context.WithCancel(context.Background())
	var stdout, stderr syncBuffer
	exit := make(chan int, 1)
	// A name of its own keeps what ps prints predictable.
	args := append([]string{"run", path, "--csms", csms.URL(), "--id", id, "--name", strings.ToLower(id)}, extra...)
	go func() { exit <- RunContext(runCtx, args, &stdout, &stderr) }()

	t.Cleanup(func() {
		interrupt()
		select {
		case <-exit:
		case <-time.After(5 * time.Second):
			t.Error("run did not exit after being interrupted")
		}
	})
	csms.WaitForCalls(t, id, v16.ActionStatusNotification, 1)
	waitForSite(t, id)
}

// waitForSite waits until ps can see the charger.
func waitForSite(t *testing.T, id string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		_, stdout, _ := run("ps")
		if strings.Contains(stdout, id) && strings.Contains(stdout, "online") {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s never showed as online in ps:\n%s", id, stdout)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestATransactionDrivenFromASecondTerminal(t *testing.T) {
	useTempDataDir(t)
	csms := csmstest.Start(t)
	startRun(t, csms, "CP-1", "--speed", "360")

	code, stdout, stderr := run("ps")
	if code != ExitOK {
		t.Fatalf("ps: exit %d, stderr %q", code, stderr)
	}
	wantPs := "CHARGE POINT ID  NAME  SITE  CONNECTION  CONNECTORS\n" +
		"CP-1             cp-1  CP-1  online      1:Available\n"
	if stdout != wantPs {
		t.Errorf("ps printed:\n%s\nwant:\n%s", stdout, wantPs)
	}

	code, stdout, stderr = run("exec", "CP-1", "plug", "--soc", "30%", "--battery", "82kWh")
	if code != ExitOK || stdout != "CP-1  connector 1  ok  EV plugged in (82 kWh, 30%)\n" {
		t.Errorf("exec plug: exit %d, stdout %q, stderr %q", code, stdout, stderr)
	}

	code, stdout, stderr = run("exec", "CP-1", "tap", "04A1B2C3")
	if code != ExitOK || stdout != "CP-1  connector 1  ok  tag accepted, transaction 1 started\n" {
		t.Errorf("exec tap: exit %d, stdout %q, stderr %q", code, stdout, stderr)
	}

	_, stdout, _ = run("ps")
	if !strings.Contains(stdout, "1:Charging") {
		t.Errorf("ps does not show the connector charging:\n%s", stdout)
	}

	time.Sleep(200 * time.Millisecond) // let energy flow

	code, stdout, stderr = run("exec", "CP-1", "unplug")
	if code != ExitOK || stdout != "CP-1  connector 1  ok  EV unplugged, transaction 1 stopped\n" {
		t.Errorf("exec unplug: exit %d, stdout %q, stderr %q", code, stdout, stderr)
	}

	// The energy reached the CSMS.
	var stop v16.StopTransactionRequest
	csms.WaitForCalls(t, "CP-1", v16.ActionStopTransaction, 1)[0].Decode(t, &stop)
	if stop.MeterStop <= 0 || stop.IDTag != "04A1B2C3" {
		t.Errorf("StopTransaction = %+v, want energy delivered for tag 04A1B2C3", stop)
	}
}

func TestExecOnSeveralChargersListsEachOutcome(t *testing.T) {
	useTempDataDir(t)
	csms := csmstest.Start(t)
	startRun(t, csms, "FAST-1")
	startRun(t, csms, "FAST-2")
	startRun(t, csms, "LOBBY-1")

	// One of the three already has an EV.
	if code, _, stderr := run("exec", "FAST-2", "plug"); code != ExitOK {
		t.Fatalf("exec plug: exit %d, stderr %q", code, stderr)
	}

	code, stdout, stderr := run("exec", "FAST-*", "plug")
	want := "FAST-1  connector 1  ok      EV plugged in (60 kWh, 20%)\n" +
		"FAST-2  connector 1  failed  the connector already has an EV\n" +
		"1 of 2 succeeded, 1 failed\n"
	if stdout != want {
		t.Errorf("exec printed:\n%s\nwant:\n%s", stdout, want)
	}
	if code != ExitFailure {
		t.Errorf("exit code = %d, want %d because one charger failed", code, ExitFailure)
	}
	if stderr != "" {
		t.Errorf("stderr = %q, want nothing more than the listing", stderr)
	}

	code, stdout, _ = run("exec", "*", "unplug")
	want = "FAST-1   connector 1  ok      EV unplugged\n" +
		"FAST-2   connector 1  ok      EV unplugged\n" +
		"LOBBY-1  connector 1  failed  the connector has no EV\n" +
		"2 of 3 succeeded, 1 failed\n"
	if stdout != want || code != ExitFailure {
		t.Errorf("exec printed (exit %d):\n%s\nwant:\n%s", code, stdout, want)
	}

	code, stdout, _ = run("exec", "FAST-*", "plug", "-n", "1")
	if code != ExitOK || !strings.HasSuffix(stdout, "2 of 2 succeeded\n") {
		t.Errorf("exec printed (exit %d):\n%s", code, stdout)
	}
}

func TestExecAndPsAsJSON(t *testing.T) {
	useTempDataDir(t)
	csms := csmstest.Start(t)
	startRun(t, csms, "CP-1")

	code, stdout, _ := run("exec", "CP-1", "plug", "--format", "json")
	if code != ExitOK {
		t.Fatalf("exec: exit %d", code)
	}
	var result control.ExecResult
	if err := json.Unmarshal([]byte(stdout), &result); err != nil {
		t.Fatalf("exec output %q is not one JSON object: %v", stdout, err)
	}
	if result.ChargePointID != "CP-1" || result.Connector != 1 || !result.OK {
		t.Errorf("exec result = %+v", result)
	}

	code, stdout, _ = run("ps", "--format", "json")
	if code != ExitOK {
		t.Fatalf("ps: exit %d", code)
	}
	var state control.ChargerState
	if err := json.Unmarshal([]byte(stdout), &state); err != nil {
		t.Fatalf("ps output %q is not one JSON object: %v", stdout, err)
	}
	if state.ChargePointID != "CP-1" || state.Site != "CP-1" || state.Connection != "online" ||
		len(state.Connectors) != 1 || state.Connectors[0].Status != v16.StatusPreparing {
		t.Errorf("ps state = %+v", state)
	}
}

func TestPsWithNothingRunningPrintsOnlyTheHeader(t *testing.T) {
	useTempDataDir(t)

	code, stdout, stderr := run("ps")
	if code != ExitOK || stdout != "CHARGE POINT ID  NAME  SITE  CONNECTION  CONNECTORS\n" {
		t.Errorf("ps: exit %d, stdout %q, stderr %q", code, stdout, stderr)
	}
}

func TestSiteFlagNarrowsToOneSite(t *testing.T) {
	useTempDataDir(t)
	csms := csmstest.Start(t)
	startRun(t, csms, "CP-1")
	startRun(t, csms, "CP-2")

	_, stdout, _ := run("ps", "-s", "CP-2")
	if strings.Contains(stdout, "CP-1") || !strings.Contains(stdout, "CP-2") {
		t.Errorf("ps -s CP-2 printed:\n%s", stdout)
	}

	code, _, stderr := run("ps", "-s", "nowhere")
	if code != ExitFailure || !strings.Contains(stderr, `no site is named "nowhere"`) {
		t.Errorf("ps -s nowhere: exit %d, stderr %q", code, stderr)
	}

	code, _, stderr = run("exec", "CP-1", "plug", "-s", "CP-2")
	if code != ExitFailure || !strings.Contains(stderr, `no running charger matches "CP-1"`) {
		t.Errorf("exec in the wrong site: exit %d, stderr %q", code, stderr)
	}
}

func TestSecondRunOfTheSameChargePointIDIsRefused(t *testing.T) {
	useTempDataDir(t)
	csms := csmstest.Start(t)
	startRun(t, csms, "CP-1")

	path := writeChargerfile(t, testChargerfile)
	code, _, stderr := run("run", path, "--csms", csms.URL(), "--id", "CP-1")
	if code != ExitFailure || !strings.Contains(stderr, `site "CP-1": the site is already running`) {
		t.Errorf("second run: exit %d, stderr %q", code, stderr)
	}
}

func TestExecFailures(t *testing.T) {
	useTempDataDir(t)
	csms := csmstest.Start(t)
	startRun(t, csms, "CP-1")

	t.Run("no charger matches", func(t *testing.T) {
		code, stdout, stderr := run("exec", "oasd", "plug")
		if code != ExitFailure || stdout != "" || stderr != "plugyard: no running charger matches \"oasd\"\n" {
			t.Errorf("exit %d, stdout %q, stderr %q", code, stdout, stderr)
		}
	})

	usage := map[string][]string{
		"no action":           {"exec", "CP-1"},
		"unknown action":      {"exec", "CP-1", "dance"},
		"tap without a tag":   {"exec", "CP-1", "tap"},
		"plug with arguments": {"exec", "CP-1", "plug", "extra"},
		"bad battery":         {"exec", "CP-1", "plug", "--battery", "big"},
		"bad state of charge": {"exec", "CP-1", "plug", "--soc", "150%"},
		"EV flags on a tap":   {"exec", "CP-1", "tap", "TAG", "--soc", "50%"},
		"connector zero":      {"exec", "CP-1", "plug", "-n", "0"},
		"bad pattern":         {"exec", "CP-[", "plug"},
		"bad format":          {"exec", "CP-1", "plug", "--format", "xml"},
	}
	for name, args := range usage {
		t.Run(name, func(t *testing.T) {
			if code, _, stderr := run(args...); code != ExitUsage {
				t.Errorf("exit %d, want %d (stderr %q)", code, ExitUsage, stderr)
			}
		})
	}

	// None of the rejected commands touched the charger.
	if _, stdout, _ := run("ps"); !strings.Contains(stdout, "1:Available") {
		t.Errorf("the charger changed state:\n%s", stdout)
	}
}

func TestColorsOnlyWhenWanted(t *testing.T) {
	on, off := palette{on: true}, palette{}

	if got := on.green("ok"); got != "\x1b[32mok\x1b[0m" {
		t.Errorf("colored ok = %q", got)
	}
	if got := off.red("failed"); got != "failed" {
		t.Errorf("plain failed = %q", got)
	}
	if on.status(v16.StatusCharging) != on.green("Charging") ||
		on.status(v16.StatusFinishing) != on.yellow("Finishing") ||
		on.status(v16.StatusFaulted) != on.red("Faulted") ||
		on.status(v16.StatusAvailable) != "Available" {
		t.Error("connector statuses have the wrong colors")
	}
	if on.connection("online") != on.green("online") || on.connection("offline") != on.red("offline") {
		t.Error("connection states have the wrong colors")
	}

	// A buffer is not a terminal, so output written to one is never colored;
	// NO_COLOR turns color off even on a terminal.
	if newPalette(&syncBuffer{}).on {
		t.Error("color is on for output that is not a terminal")
	}
	t.Setenv("NO_COLOR", "1")
	if newPalette(os.Stdout).on {
		t.Error("color is on although NO_COLOR is set")
	}

	// Color codes must not disturb the column alignment.
	var plainOut, coloredOut syncBuffer
	rows := func(p palette) [][]cell {
		return [][]cell{
			{{plain: "ok", rendered: p.green("ok")}, plain("first")},
			{{plain: "failed", rendered: p.red("failed")}, plain("second")},
		}
	}
	writeTable(&plainOut, rows(off))
	writeTable(&coloredOut, rows(on))
	strip := strings.NewReplacer("\x1b[32m", "", "\x1b[31m", "", "\x1b[0m", "")
	if strip.Replace(coloredOut.String()) != plainOut.String() {
		t.Errorf("colored table is aligned differently:\n%q\n%q", coloredOut.String(), plainOut.String())
	}
}
