package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/Amraa1/plugyard/internal/charger"
	"github.com/Amraa1/plugyard/internal/csmstest"
	"github.com/Amraa1/plugyard/internal/model"
	"github.com/Amraa1/plugyard/internal/ocpp/v16"
	"github.com/Amraa1/plugyard/internal/store"
)

// background runs a plugyard command that stays in the foreground, as a
// first terminal would. stop interrupts it, as Ctrl+C does, and returns its
// exit code.
func background(t *testing.T, args ...string) (stdout *syncBuffer, stop func() int) {
	t.Helper()
	ctx, interrupt := context.WithCancel(context.Background())
	stdout = &syncBuffer{}
	var stderr syncBuffer
	exit := make(chan int, 1)
	go func() { exit <- RunContext(ctx, args, stdout, &stderr) }()

	code := -1
	stop = func() int {
		interrupt()
		if code == -1 {
			select {
			case code = <-exit:
			case <-time.After(10 * time.Second):
				t.Fatalf("%v did not exit; stderr: %s", args, stderr.String())
			}
		}
		return code
	}
	t.Cleanup(func() { stop() })
	return stdout, stop
}

// eventually waits until ok reports true.
func eventually(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting until %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// psRow returns the line of "ps -a" for a charge point ID, with runs of
// spaces collapsed.
func psRow(t *testing.T, id string) string {
	t.Helper()
	_, stdout, _ := run("ps", "-a")
	for _, line := range strings.Split(stdout, "\n") {
		if fields := strings.Fields(line); len(fields) > 0 && fields[0] == id {
			return strings.Join(fields, " ")
		}
	}
	return ""
}

func dbFiles(t *testing.T, dataDir string) []string {
	t.Helper()
	files, _ := filepath.Glob(filepath.Join(dataDir, "*.db"))
	return files
}

func decodeCall[T any](t *testing.T, call csmstest.Call) T {
	t.Helper()
	var v T
	call.Decode(t, &v)
	return v
}

func TestRunBringsBackTheSameCharger(t *testing.T) {
	dataDir := useTempDataDir(t)
	csms := csmstest.Start(t)
	path := writeChargerfile(t, testChargerfile)
	runArgs := []string{"run", path, "--csms", csms.URL(), "--id", "CP-1", "--speed", "3600"}

	_, stop := background(t, runArgs...)
	csms.WaitForCalls(t, "CP-1", v16.ActionStatusNotification, 1)
	waitForSite(t, "CP-1")

	// The charger was given a made-up name.
	row := psRow(t, "CP-1")
	match := regexp.MustCompile(`^CP-1 ([a-z]+_[a-z]+) CP-1 online 1:Available$`).FindStringSubmatch(row)
	if match == nil {
		t.Fatalf("ps row = %q, want a made-up name such as brisk_volt", row)
	}
	name := match[1]

	run("exec", "CP-1", "plug")
	if code, stdout, stderr := run("exec", name, "tap", "TAG-1"); code != ExitOK || !strings.Contains(stdout, "transaction 1 started") {
		t.Fatalf("exec by name: exit %d, stdout %q, stderr %q", code, stdout, stderr)
	}
	time.Sleep(100 * time.Millisecond)

	// Ctrl+C is an orderly shutdown: the transaction ends, and the charger
	// stays, stopped, with its EV still plugged in.
	if code := stop(); code != ExitOK {
		t.Fatalf("run exited with %d", code)
	}
	first := decodeCall[v16.StopTransactionRequest](t, csms.WaitForCalls(t, "CP-1", v16.ActionStopTransaction, 1)[0])
	if first.Reason != v16.ReasonOther || first.MeterStop <= 0 {
		t.Errorf("StopTransaction on shutdown = %+v", first)
	}
	if _, stdout, _ := run("ps"); strings.Contains(stdout, "CP-1 ") {
		t.Errorf("ps still lists the stopped charger:\n%s", stdout)
	}
	if row := psRow(t, "CP-1"); row != "CP-1 "+name+" CP-1 stopped 1:Finishing" {
		t.Errorf("ps -a row = %q", row)
	}
	if _, ls, _ := run("site", "ls", "-a"); !regexp.MustCompile(`CP-1 +stopped +1 +- `).MatchString(ls) {
		t.Errorf("site ls -a printed:\n%s", ls)
	}
	if code, _, stderr := run("exec", "CP-1", "unplug"); code != ExitFailure || !strings.Contains(stderr, `charger "CP-1" is stopped; start it with "plugyard start CP-1"`) {
		t.Errorf("exec on a stopped charger: exit %d, stderr %q", code, stderr)
	}

	// Running the same charge point ID again brings the same charger back.
	_, stop = background(t, runArgs...)
	csms.WaitForCalls(t, "CP-1", v16.ActionBootNotification, 2)
	eventually(t, "the charger is online again", func() bool {
		return psRow(t, "CP-1") == "CP-1 "+name+" CP-1 online 1:Finishing"
	})
	run("exec", "CP-1", "unplug")
	run("exec", "CP-1", "plug")
	run("exec", "CP-1", "tap", "TAG-1")
	second := decodeCall[v16.StartTransactionRequest](t, csms.WaitForCalls(t, "CP-1", v16.ActionStartTransaction, 2)[1])
	if second.MeterStart != first.MeterStop {
		t.Errorf("the meter restarted: meterStart = %d after meterStop = %d", second.MeterStart, first.MeterStop)
	}

	// rm refuses a running charger, and wipes a stopped one.
	if code, stdout, _ := run("rm", "CP-1"); code != ExitFailure || !strings.Contains(stdout, "failed  the charger is running; stop it first, or use -f") {
		t.Errorf("rm of a running charger: exit %d, stdout %q", code, stdout)
	}
	stop()
	if code, stdout, stderr := run("rm", "CP-1"); code != ExitOK || stdout != "CP-1  ok  removed\n" {
		t.Errorf("rm: exit %d, stdout %q, stderr %q", code, stdout, stderr)
	}
	if row := psRow(t, "CP-1"); row != "" {
		t.Errorf("ps -a still lists the removed charger: %q", row)
	}
	if files := dbFiles(t, dataDir); len(files) != 0 {
		t.Errorf("saved state left behind after rm: %v", files)
	}
}

func TestRunRmSavesNothing(t *testing.T) {
	dataDir := useTempDataDir(t)
	csms := csmstest.Start(t)
	path := writeChargerfile(t, testChargerfile)

	_, stop := background(t, "run", path, "--csms", csms.URL(), "--id", "CP-1", "--rm")
	csms.WaitForCalls(t, "CP-1", v16.ActionStatusNotification, 1)
	waitForSite(t, "CP-1")
	if files := dbFiles(t, dataDir); len(files) != 0 {
		t.Errorf("run --rm wrote saved state: %v", files)
	}
	stop()
	if row := psRow(t, "CP-1"); row != "" {
		t.Errorf("ps -a lists a charger run with --rm: %q", row)
	}

	// A charger that has saved state is not silently run without it.
	_, stop = background(t, "run", path, "--csms", csms.URL(), "--id", "CP-1")
	csms.WaitForCalls(t, "CP-1", v16.ActionBootNotification, 2)
	waitForSite(t, "CP-1")
	stop()
	code, _, stderr := run("run", path, "--csms", csms.URL(), "--id", "CP-1", "--rm")
	if code != ExitFailure || !strings.Contains(stderr, "has saved state, which --rm would not use") {
		t.Errorf("run --rm over saved state: exit %d, stderr %q", code, stderr)
	}
}

// upDetached brings the five-charger site up in the background.
func upDetached(t *testing.T, csms *csmstest.CSMS) string {
	t.Helper()
	t.Setenv(asCLIEnv, "1")
	path := writeSite(t, csms.URL(), fiveChargers)
	code, stdout, stderr := run("site", "up", "-d", "-f", path, "--speed", "3600")
	if code != ExitOK {
		t.Fatalf("site up -d: exit %d, stdout %q, stderr %q", code, stdout, stderr)
	}
	// Whatever happens next, do not leave a background process behind.
	t.Cleanup(func() { run("site", "down", "-s", "depot") })
	for _, id := range fiveIDs {
		csms.WaitForCalls(t, id, v16.ActionStatusNotification, 1)
	}
	waitForSite(t, "LOBBY-1")
	return path
}

func startTransaction(t *testing.T, csms *csmstest.CSMS, id string) {
	t.Helper()
	run("exec", id, "plug")
	if code, stdout, stderr := run("exec", id, "tap", "TAG-1"); code != ExitOK {
		t.Fatalf("tap on %s: exit %d, stdout %q, stderr %q", id, code, stdout, stderr)
	}
	csms.WaitForCalls(t, id, v16.ActionStartTransaction, 1)
}

func TestStopStartAndKillSingleChargers(t *testing.T) {
	useTempDataDir(t)
	csms := csmstest.Start(t)
	upDetached(t, csms)
	startTransaction(t, csms, "FAST-01")
	time.Sleep(50 * time.Millisecond)

	// kill is a power cut: no message, and the charger is listed as stopped.
	code, stdout, stderr := run("kill", "FAST-01")
	if code != ExitOK || stdout != "FAST-01  ok  killed\n" {
		t.Fatalf("kill: exit %d, stdout %q, stderr %q", code, stdout, stderr)
	}
	if ends := csms.WaitDisconnects(t, "FAST-01", 1); ends[0].Clean {
		t.Error("kill closed the connection normally, want a drop")
	}
	if row := psRow(t, "FAST-01"); !strings.HasSuffix(row, " depot stopped 1:Finishing") {
		t.Errorf("ps -a row after kill = %q", row)
	}
	if _, ps, _ := run("ps"); strings.Contains(ps, "FAST-01") || strings.Count(ps, "\n") != 5 {
		t.Errorf("ps after kill printed:\n%s", ps)
	}
	if code, stdout, _ := run("kill", "FAST-01"); code != ExitFailure || !strings.Contains(stdout, "failed  the charger is not running") {
		t.Errorf("second kill: exit %d, stdout %q", code, stdout)
	}

	// start brings it back, and it reports the interrupted transaction.
	code, stdout, stderr = run("start", "FAST-01")
	if code != ExitOK || stdout != "FAST-01  ok  started\n" {
		t.Fatalf("start: exit %d, stdout %q, stderr %q", code, stdout, stderr)
	}
	stop := decodeCall[v16.StopTransactionRequest](t, csms.WaitForCalls(t, "FAST-01", v16.ActionStopTransaction, 1)[0])
	if stop.Reason != v16.ReasonPowerLoss || stop.TransactionID == 0 {
		t.Errorf("StopTransaction after kill and start = %+v, want reason PowerLoss", stop)
	}

	// stop on a pattern is orderly, and reports each charger.
	startTransaction(t, csms, "FAST-02")
	code, stdout, _ = run("stop", "FAST-*")
	if code != ExitOK || !strings.HasSuffix(stdout, "4 of 4 succeeded\n") || !strings.Contains(stdout, "FAST-02  ok  stopped\n") {
		t.Errorf("stop FAST-*: exit %d\n%s", code, stdout)
	}
	if stop := decodeCall[v16.StopTransactionRequest](t, csms.WaitForCalls(t, "FAST-02", v16.ActionStopTransaction, 1)[0]); stop.Reason != v16.ReasonOther {
		t.Errorf("StopTransaction on stop = %+v, want reason Other", stop)
	}
	if ends := csms.WaitDisconnects(t, "FAST-02", 1); !ends[0].Clean {
		t.Error("stop dropped the connection, want a normal close")
	}
	if code, stdout, _ := run("stop", "FAST-02"); code != ExitOK || stdout != "FAST-02  ok  already stopped\n" {
		t.Errorf("second stop: exit %d, stdout %q", code, stdout)
	}
	if code, _, stderr := run("stop", "NOPE"); code != ExitFailure || !strings.Contains(stderr, `no charger matches "NOPE"`) {
		t.Errorf("stop of an unknown charger: exit %d, stderr %q", code, stderr)
	}

	// restart is a stop and a start.
	if code, stdout, _ := run("restart", "LOBBY-1"); code != ExitOK || stdout != "LOBBY-1  ok  restarted\n" {
		t.Errorf("restart: exit %d, stdout %q", code, stdout)
	}
	csms.WaitForCalls(t, "LOBBY-1", v16.ActionBootNotification, 2)

	// Stopping the last running charger ends the site's process.
	run("stop", "LOBBY-1")
	eventually(t, "the site's process has ended", func() bool {
		_, ls, _ := run("site", "ls")
		return !strings.Contains(ls, "depot")
	})
	if _, ls, _ := run("site", "ls", "-a"); !regexp.MustCompile(`depot +stopped +5 +- `).MatchString(ls) {
		t.Errorf("site ls -a printed:\n%s", ls)
	}
	if _, ps, _ := run("ps", "-a"); strings.Count(ps, " stopped ") != 5 {
		t.Errorf("ps -a does not list five stopped chargers:\n%s", ps)
	}

	// Starting one charger of a stopped site starts the site with only it.
	code, stdout, stderr = run("start", "FAST-03")
	if code != ExitOK || !strings.HasPrefix(stdout, "FAST-03  ok  started\nSite depot is running in the background with 1 charger (process ") {
		t.Fatalf("start on a stopped site: exit %d, stdout %q, stderr %q", code, stdout, stderr)
	}
	csms.WaitForCalls(t, "FAST-03", v16.ActionBootNotification, 2)
	eventually(t, "FAST-03 is online", func() bool { return strings.HasSuffix(psRow(t, "FAST-03"), " depot online 1:Available") })
	if _, ps, _ := run("ps"); strings.Count(ps, "\n") != 2 {
		t.Errorf("ps lists more than the one started charger:\n%s", ps)
	}

	// The others can then be started inside the running site.
	if code, stdout, _ := run("start", "FAST-04"); code != ExitOK || stdout != "FAST-04  ok  started\n" {
		t.Errorf("start inside a running site: exit %d, stdout %q", code, stdout)
	}
	csms.WaitForCalls(t, "FAST-04", v16.ActionBootNotification, 2)

	// rm -f stops a running charger and wipes it.
	if code, stdout, _ := run("rm", "-f", "FAST-04"); code != ExitOK || stdout != "FAST-04  ok  removed\n" {
		t.Errorf("rm -f: exit %d, stdout %q", code, stdout)
	}
	if row := psRow(t, "FAST-04"); row != "" {
		t.Errorf("ps -a still lists FAST-04: %q", row)
	}
}

func TestSiteStopAndStart(t *testing.T) {
	useTempDataDir(t)
	csms := csmstest.Start(t)
	upDetached(t, csms)
	startTransaction(t, csms, "FAST-01")

	code, stdout, stderr := run("site", "stop", "-s", "depot")
	if code != ExitOK || stdout != "Site depot is stopped\n" {
		t.Fatalf("site stop: exit %d, stdout %q, stderr %q", code, stdout, stderr)
	}
	if stop := decodeCall[v16.StopTransactionRequest](t, csms.WaitForCalls(t, "FAST-01", v16.ActionStopTransaction, 1)[0]); stop.Reason != v16.ReasonOther {
		t.Errorf("StopTransaction on site stop = %+v", stop)
	}
	if code, stdout, _ := run("site", "stop", "-s", "depot"); code != ExitOK || stdout != "Site depot is already stopped\n" {
		t.Errorf("second site stop: exit %d, stdout %q", code, stdout)
	}
	if _, ps, _ := run("ps", "-a"); strings.Count(ps, " stopped ") != 5 {
		t.Errorf("ps -a after site stop:\n%s", ps)
	}

	// site start reads no file: it works from anywhere, by name.
	code, stdout, stderr = run("site", "start", "-s", "depot")
	if code != ExitOK || !strings.HasPrefix(stdout, "Site depot is running in the background with 5 chargers (process ") {
		t.Fatalf("site start: exit %d, stdout %q, stderr %q", code, stdout, stderr)
	}
	for _, id := range fiveIDs {
		csms.WaitForCalls(t, id, v16.ActionBootNotification, 2)
	}
	// The site's settings came back with it: the EV preset still applies.
	eventually(t, "FAST-02 is online", func() bool { return strings.Contains(psRow(t, "FAST-02"), " online ") })
	if code, stdout, _ := run("exec", "FAST-02", "plug"); code != ExitOK || !strings.Contains(stdout, "EV plugged in (40 kWh, 50%)") {
		t.Errorf("plug after site start: exit %d, stdout %q", code, stdout)
	}
	if code, _, stderr := run("site", "start", "-s", "depot"); code != ExitFailure || !strings.Contains(stderr, "already running") {
		t.Errorf("site start while running: exit %d, stderr %q", code, stderr)
	}

	// A site that was taken down is started with site up, not site start.
	run("site", "down", "-s", "depot")
	if _, ps, _ := run("ps", "-a"); strings.Contains(ps, "FAST") {
		t.Errorf("ps -a lists the chargers of a site that is down:\n%s", ps)
	}
	code, _, stderr = run("site", "start", "-s", "depot")
	if code != ExitFailure || !strings.Contains(stderr, `site "depot" was taken down; bring it back with "plugyard site up"`) {
		t.Errorf("site start on a site that is down: exit %d, stderr %q", code, stderr)
	}
	code, _, stderr = run("site", "start", "-s", "ghost")
	if code != ExitFailure || !strings.Contains(stderr, `site "ghost" has no saved state`) {
		t.Errorf("site start with nothing saved: exit %d, stderr %q", code, stderr)
	}
}

func TestKillingTheProcessMidTransactionReportsPowerLoss(t *testing.T) {
	useTempDataDir(t)
	csms := csmstest.Start(t)
	path := upDetached(t, csms)
	startTransaction(t, csms, "FAST-01")
	names := map[string]string{}
	for _, id := range fiveIDs {
		names[id] = strings.Fields(psRow(t, id))[1]
	}

	// End the site's process the hard way, as a crash or a pulled plug would.
	_, ls, _ := run("site", "ls", "--format", "json")
	var row siteRow
	if err := json.Unmarshal([]byte(ls), &row); err != nil || row.PID == 0 {
		t.Fatalf("site ls --format json printed %q (%v)", ls, err)
	}
	process, err := os.FindProcess(row.PID)
	if err != nil {
		t.Fatal(err)
	}
	if err := process.Kill(); err != nil {
		t.Fatal(err)
	}
	if ends := csms.WaitDisconnects(t, "FAST-01", 1); ends[0].Clean {
		t.Error("the connection closed normally although the process was killed")
	}
	eventually(t, "the site is seen as stopped", func() bool {
		_, ls, _ := run("site", "ls", "-a")
		return regexp.MustCompile(`depot +stopped `).MatchString(ls)
	})

	// Starting again yields the same chargers, and the one that was charging
	// reports its transaction with PowerLoss.
	code, stdout, stderr := run("site", "up", "-d", "-f", path)
	if code != ExitOK {
		t.Fatalf("site up after the crash: exit %d, stdout %q, stderr %q", code, stdout, stderr)
	}
	stop := decodeCall[v16.StopTransactionRequest](t, csms.WaitForCalls(t, "FAST-01", v16.ActionStopTransaction, 1)[0])
	if stop.Reason != v16.ReasonPowerLoss || stop.TransactionID == 0 {
		t.Errorf("StopTransaction after the crash = %+v, want reason PowerLoss", stop)
	}
	for _, id := range fiveIDs {
		csms.WaitForCalls(t, id, v16.ActionBootNotification, 2)
		eventually(t, id+" is online", func() bool { return strings.Contains(psRow(t, id), " online ") })
		if got := strings.Fields(psRow(t, id))[1]; got != names[id] {
			t.Errorf("%s is now named %q, was %q", id, got, names[id])
		}
	}
	if row := psRow(t, "FAST-01"); !strings.HasSuffix(row, " online 1:Finishing") {
		t.Errorf("FAST-01 after the crash = %q, want its EV still plugged in", row)
	}
	for _, id := range fiveIDs[1:] {
		for _, call := range csms.Calls(id) {
			if call.Action == v16.ActionStopTransaction {
				t.Errorf("%s sent StopTransaction although it had no transaction", id)
			}
		}
	}
}

const twoEntries = `
name: depot
chargers:
  fast:  { model: ./Chargerfile, count: 2, id: "FAST-{n}" }
  lobby: { model: ./Chargerfile, id: LOBBY-1, name: front-desk }
`

// answer makes the next confirmation prompt read the given reply.
func answer(t *testing.T, reply string) {
	t.Helper()
	oldStdin, oldIsTerminal := stdin, stdinIsTerminal
	stdin, stdinIsTerminal = strings.NewReader(reply), func() bool { return true }
	t.Cleanup(func() { stdin, stdinIsTerminal = oldStdin, oldIsTerminal })
}

func TestSiteUpAsksBeforeSavedStateIsLost(t *testing.T) {
	useTempDataDir(t)
	csms := csmstest.Start(t)
	path := writeSite(t, csms.URL(), twoEntries)
	ids := []string{"FAST-1", "FAST-2", "LOBBY-1"}

	_, exited := startSiteUp(t, csms, path, ids)
	nameBefore := strings.Fields(psRow(t, "FAST-1"))[1]
	run("site", "stop", "-f", path)
	exited()

	// The lobby charger leaves the file, and the model gains a connector.
	rewrite := func(file, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(filepath.Dir(path), file), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	rewrite("site.yaml", "csms: { url: "+csms.URL()+" }\n"+strings.Replace(twoEntries, "  lobby:", "  # lobby:", 1))
	rewrite("Chargerfile", testChargerfile+"  - { id: 2, type: Type2, maxPower: 7.4kW }\n")

	// With nobody to ask, nothing happens.
	oldIsTerminal := stdinIsTerminal
	stdinIsTerminal = func() bool { return false }
	t.Cleanup(func() { stdinIsTerminal = oldIsTerminal })
	code, stdout, stderr := run("site", "up", "-f", path)
	if code != ExitFailure || !strings.Contains(stderr, "3 chargers would lose saved state; run again with --yes to allow it") {
		t.Errorf("site up without a terminal: exit %d, stderr %q", code, stderr)
	}
	for _, want := range []string{
		"The files no longer match the saved state of these chargers:",
		"LOBBY-1  is no longer declared; its saved state will be deleted",
		"FAST-1   went from 1 connector to 2 connectors; its saved state will be reset",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("site up output is missing %q:\n%s", want, stdout)
		}
	}
	if row := psRow(t, "LOBBY-1"); row == "" {
		t.Error("the lobby charger was deleted although nothing was confirmed")
	}

	// Answering no changes nothing either.
	answer(t, "n\n")
	code, stdout, stderr = run("site", "up", "-f", path)
	if code != ExitFailure || !strings.Contains(stdout, "Reset these 3 chargers? [y/N]") || !strings.Contains(stderr, "nothing was changed and nothing was started") {
		t.Errorf("site up answered with no: exit %d, stdout %q, stderr %q", code, stdout, stderr)
	}
	if row := psRow(t, "LOBBY-1"); row == "" {
		t.Error("the lobby charger was deleted although the answer was no")
	}

	// Answering yes resets the affected chargers and starts the site.
	answer(t, "y\n")
	startSiteUp(t, csms, path, []string{"FAST-1", "FAST-2"})
	eventually(t, "both connectors are reported", func() bool {
		return strings.HasSuffix(psRow(t, "FAST-1"), " depot online 1:Available 2:Available")
	})
	if row := psRow(t, "LOBBY-1"); row != "" {
		t.Errorf("ps -a still lists the lobby charger: %q", row)
	}
	// A reset wipes the state, not the name.
	if got := strings.Fields(psRow(t, "FAST-1"))[1]; got != nameBefore {
		t.Errorf("FAST-1 is now named %q, was %q", got, nameBefore)
	}
}

func TestSiteUpYesSkipsTheQuestion(t *testing.T) {
	useTempDataDir(t)
	csms := csmstest.Start(t)
	path := writeSite(t, csms.URL(), twoEntries)

	_, exited := startSiteUp(t, csms, path, []string{"FAST-1", "FAST-2", "LOBBY-1"})
	run("site", "stop", "-f", path)
	exited()

	content := "csms: { url: " + csms.URL() + " }\n" + strings.Replace(twoEntries, "  lobby:", "  # lobby:", 1)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	stdout, _ := startSiteUp(t, csms, path, []string{"FAST-1", "FAST-2"}, "--yes")
	if !strings.Contains(stdout.String(), "LOBBY-1  is no longer declared") || strings.Contains(stdout.String(), "[y/N]") {
		t.Errorf("site up --yes printed:\n%s", stdout.String())
	}
	if row := psRow(t, "LOBBY-1"); row != "" {
		t.Errorf("ps -a still lists the lobby charger: %q", row)
	}
}

func TestChargerNames(t *testing.T) {
	useTempDataDir(t)
	csms := csmstest.Start(t)
	path := writeSite(t, csms.URL(), twoEntries)
	startSiteUp(t, csms, path, []string{"FAST-1", "FAST-2", "LOBBY-1"})

	// The name the file gives is used, and the others are made up.
	if row := psRow(t, "LOBBY-1"); row != "LOBBY-1 front-desk depot online 1:Available" {
		t.Errorf("ps row = %q", row)
	}
	fast1, fast2 := strings.Fields(psRow(t, "FAST-1"))[1], strings.Fields(psRow(t, "FAST-2"))[1]
	made := regexp.MustCompile(`^[a-z]+_[a-z]+\d*$`)
	if !made.MatchString(fast1) || !made.MatchString(fast2) || fast1 == fast2 {
		t.Errorf("made-up names = %q and %q, want two different ones", fast1, fast2)
	}

	// A charger can be named by its name wherever it can by its ID, and
	// patterns match names too. Results are labelled with the ID.
	if code, stdout, _ := run("exec", "front-desk", "plug"); code != ExitOK || !strings.HasPrefix(stdout, "LOBBY-1  connector 1  ok  ") {
		t.Errorf("exec by name: exit %d, stdout %q", code, stdout)
	}
	if code, stdout, _ := run("exec", "front-*", "unplug"); code != ExitOK || !strings.HasPrefix(stdout, "LOBBY-1  connector 1  ok  ") {
		t.Errorf("exec by name pattern: exit %d, stdout %q", code, stdout)
	}
	if code, stdout, _ := run("restart", fast1); code != ExitOK || stdout != "FAST-1  ok  restarted\n" {
		t.Errorf("restart by name: exit %d, stdout %q", code, stdout)
	}

	_, stdout, _ := run("ps", "--format", "json", "-s", "depot")
	var first struct {
		ChargePointID string `json:"chargePointId"`
		Name          string `json:"name"`
		Connection    string `json:"connection"`
	}
	if err := json.Unmarshal([]byte(strings.SplitN(stdout, "\n", 2)[0]), &first); err != nil || first.ChargePointID != "FAST-1" || first.Name != fast1 {
		t.Errorf("ps --format json first line = %+v (%v), from %q", first, err, stdout)
	}

	// Names are unique on the machine, across sites.
	chargerfile := writeChargerfile(t, testChargerfile)
	code, _, stderr := run("run", chargerfile, "--csms", csms.URL(), "--id", "CP-9", "--name", "front-desk")
	if code != ExitUsage || !strings.Contains(stderr, `the name "front-desk" is already used by charger LOBBY-1 of site depot`) {
		t.Errorf("run with a taken name: exit %d, stderr %q", code, stderr)
	}
	code, _, stderr = run("run", chargerfile, "--csms", csms.URL(), "--id", "CP-9", "--name", "no spaces")
	if code != ExitUsage || !strings.Contains(stderr, "--name: invalid name") {
		t.Errorf("run with an invalid name: exit %d, stderr %q", code, stderr)
	}

	// The same charge point ID in two sites is told apart by -s or by name.
	_, stop := background(t, "run", chargerfile, "--csms", csms.URL(), "--id", "FAST-1", "--name", "loner")
	csms.WaitForCalls(t, "FAST-1", v16.ActionBootNotification, 3)
	eventually(t, "the second FAST-1 is online", func() bool {
		_, ps, _ := run("ps", "-s", "FAST-1")
		return strings.Contains(ps, " online ")
	})
	code, _, stderr = run("stop", "FAST-1")
	if code != ExitFailure || !strings.Contains(stderr, `charger "FAST-1" exists in sites FAST-1 and depot; choose one with -s, or use its name`) {
		t.Errorf("stop of an ambiguous ID: exit %d, stderr %q", code, stderr)
	}
	if code, stdout, _ := run("stop", "loner"); code != ExitOK || stdout != "FAST-1  ok  stopped\n" {
		t.Errorf("stop by name: exit %d, stdout %q", code, stdout)
	}
	if code := stop(); code != ExitOK {
		t.Errorf("run exited with %d after its charger was stopped", code)
	}
}

func TestSiteFileNameRules(t *testing.T) {
	path := writeSite(t, "ws://localhost:9000/ocpp", `
name: depot
chargers:
  fast: { model: ./Chargerfile, count: 2, id: "FAST-{n}", name: speedy }
  a:    { model: ./Chargerfile, id: A, name: twin }
  b:    { model: ./Chargerfile, id: B, name: twin }
  c:    { model: ./Chargerfile, id: C, name: "not valid" }
`)
	code, _, stderr := run("site", "validate", "-f", path)
	if code != ExitUsage {
		t.Fatalf("exit %d, want %d; stderr %q", code, ExitUsage, stderr)
	}
	for _, want := range []string{
		"chargers.fast: name can only be given to an entry that creates one charger; this one creates 2",
		`chargers.b: the name "twin" is already used by chargers.a`,
		`chargers.c: invalid name "not valid"`,
	} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr is missing %q:\n%s", want, stderr)
		}
	}

	good := writeSite(t, "ws://localhost:9000/ocpp", twoEntries)
	if code, stdout, _ := run("site", "validate", "-f", good); code != ExitOK || !strings.Contains(stdout, "name front-desk") {
		t.Errorf("site validate: exit %d\n%s", code, stdout)
	}
}

func TestReconcile(t *testing.T) {
	spec := func(connectors int) store.Spec {
		m := &model.Model{}
		for i := 1; i <= connectors; i++ {
			m.Connectors = append(m.Connectors, model.Connector{ID: i})
		}
		return store.Spec{Model: m}
	}
	ran := &charger.State{}
	have := []store.Charger{
		{ID: "KEEP", Name: "keeper", Spec: spec(1), State: ran},
		{ID: "GROW", Name: "grower", Spec: spec(1), State: ran},
		{ID: "GONE", Name: "leaver", Spec: spec(1), State: ran},
		{ID: "FRESH-GONE", Name: "unused", Spec: spec(1)},
	}
	others := []store.Site{
		{Meta: store.Meta{Name: "depot"}, Chargers: have},
		{Meta: store.Meta{Name: "yard"}, Chargers: []store.Charger{{ID: "Y", Name: "elsewhere", Spec: spec(1)}}},
	}
	want := []desired{
		{ID: "KEEP", Spec: spec(1)},
		{ID: "GROW", Name: "renamed", Spec: spec(2)},
		{ID: "NEW", Spec: spec(1)},
		{ID: "NAMED", Name: "leaver", Spec: spec(1)},
	}

	p, err := reconcile("depot", want, have, others)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(p.change.Remove, ","); got != "GONE,FRESH-GONE" {
		t.Errorf("Remove = %s", got)
	}
	if got := strings.Join(p.change.Reset, ","); got != "GROW" {
		t.Errorf("Reset = %s", got)
	}
	// Only chargers that would lose state need a yes: not the one that
	// never ran.
	if len(p.confirm) != 2 || p.confirm[0][0] != "GONE" || p.confirm[1][0] != "GROW" {
		t.Errorf("confirm = %v, want GONE and GROW", p.confirm)
	}
	names := map[string]string{}
	for _, c := range p.change.Put {
		names[c.ID] = c.Name
		if !c.Power {
			t.Errorf("%s is not marked to be powered on", c.ID)
		}
	}
	// A kept charger keeps its name, a given name wins, and a name freed by
	// a removed charger can be given again.
	if names["KEEP"] != "keeper" || names["GROW"] != "renamed" || names["NAMED"] != "leaver" {
		t.Errorf("names = %v", names)
	}
	if !regexp.MustCompile(`^[a-z]+_[a-z]+$`).MatchString(names["NEW"]) {
		t.Errorf("the new charger is named %q, want a made-up name", names["NEW"])
	}

	// A name in use, in this site or another, is refused.
	for name, where := range map[string]string{"keeper": "charger KEEP", "elsewhere": "charger Y of site yard"} {
		_, err := reconcile("depot", []desired{{ID: "KEEP", Spec: spec(1)}, {ID: "X", Name: name, Spec: spec(1)}}, have, others)
		if err == nil || !strings.Contains(err.Error(), "is already used by "+where) {
			t.Errorf("reconcile with the taken name %q returned %v", name, err)
		}
	}
}
