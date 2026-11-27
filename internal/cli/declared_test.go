package cli

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/Amraa1/plugyard/internal/csmstest"
	"github.com/Amraa1/plugyard/internal/ocpp/v16"
)

// A standing fleet: every charger of it is declared to be in some state.
const declaredFleet = `
name: fleet
evs:
  small: { battery: 40kWh, soc: 50% }
chargers:
  lobby: { model: ./Chargerfile, id: LOBBY-1, state: { connectors: { 1: { ev: small } } } }
  bay:   { model: ./Chargerfile, id: BAY-1, state: { connectors: { 1: { fault: GroundFailure } } } }
  gone:  { model: ./Chargerfile, id: GONE-1, state: { connection: offline } }
  spare: { model: ./Chargerfile, id: SPARE-1, state: { connection: stopped, connectors: { 0: { fault: InternalError } } } }
`

// psShows waits until the line of "ps -a" for a charger ends as given.
func psShows(t *testing.T, id, suffix string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		row := psRow(t, id)
		if strings.HasSuffix(row, suffix) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("ps -a shows %q, want it to end with %q", row, suffix)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestSiteValidateShowsDeclaredState(t *testing.T) {
	path := writeSite(t, "ws://localhost:9000/ocpp", declaredFleet)

	code, stdout, stderr := run("site", "validate", "-f", path)
	want := "Site fleet is valid: 4 chargers, CSMS ws://localhost:9000/ocpp\n" +
		"  lobby  1 charger  LOBBY-1  Acme Wallbox  declared: connector 1 with EV small\n" +
		"  bay    1 charger  BAY-1    Acme Wallbox  declared: connector 1 with fault GroundFailure\n" +
		"  gone   1 charger  GONE-1   Acme Wallbox  declared: offline\n" +
		"  spare  1 charger  SPARE-1  Acme Wallbox  declared: stopped, the charger with fault InternalError\n" +
		"EV presets: small\n"
	if code != ExitOK || stdout != want {
		t.Errorf("exit %d, stderr %q, stdout:\n%s\nwant:\n%s", code, stderr, stdout, want)
	}
}

func TestDeclaredStateHoldsAtEveryPowerOn(t *testing.T) {
	useTempDataDir(t)
	csms := csmstest.Start(t)
	t.Setenv(asCLIEnv, "1")
	path := writeSite(t, csms.URL(), declaredFleet)

	if code, stdout, stderr := run("site", "up", "-d", "-f", path); code != ExitOK {
		t.Fatalf("site up -d: exit %d, stdout %q, stderr %q", code, stdout, stderr)
	}
	t.Cleanup(func() { run("site", "down", "-s", "fleet") })

	// Each charger comes up in the state the file declares.
	psShows(t, "LOBBY-1", " online 1:Preparing")
	psShows(t, "BAY-1", " online 1:Faulted(GroundFailure)")
	psShows(t, "GONE-1", " offline 1:Available")
	psShows(t, "SPARE-1", " stopped 0:Faulted(InternalError) 1:Available")
	if n := csms.Connects("GONE-1") + csms.Connects("SPARE-1"); n != 0 {
		t.Errorf("the chargers declared offline and stopped connected %d times, want none", n)
	}

	// A change made by hand holds while the charger has power.
	for _, args := range [][]string{
		{"exec", "LOBBY-1", "unplug"},
		{"exec", "BAY-1", "clear"},
		{"exec", "GONE-1", "online"},
		{"start", "SPARE-1"},
	} {
		if code, stdout, stderr := run(args...); code != ExitOK {
			t.Fatalf("%v: exit %d, stdout %q, stderr %q", args, code, stdout, stderr)
		}
	}
	psShows(t, "LOBBY-1", " online 1:Available")
	psShows(t, "BAY-1", " online 1:Available")
	psShows(t, "GONE-1", " online 1:Available")
	// The rest of a stopped charger's declared state applies when it is
	// started by hand.
	psShows(t, "SPARE-1", " online 0:Faulted(InternalError) 1:Available")

	// One charger is put back by restarting it.
	if code, stdout, stderr := run("restart", "BAY-1"); code != ExitOK {
		t.Fatalf("restart BAY-1: exit %d, stdout %q, stderr %q", code, stdout, stderr)
	}
	psShows(t, "BAY-1", " online 1:Faulted(GroundFailure)")

	// A stopped site lists what each charger will report when it starts.
	if code, stdout, stderr := run("site", "stop", "-s", "fleet"); code != ExitOK {
		t.Fatalf("site stop: exit %d, stdout %q, stderr %q", code, stdout, stderr)
	}
	psShows(t, "LOBBY-1", " stopped 1:Preparing")
	psShows(t, "BAY-1", " stopped 1:Faulted(GroundFailure)")

	// site start reads no file, and the declared state holds all the same.
	if code, stdout, stderr := run("site", "start", "-s", "fleet"); code != ExitOK {
		t.Fatalf("site start: exit %d, stdout %q, stderr %q", code, stdout, stderr)
	}
	psShows(t, "LOBBY-1", " online 1:Preparing")
	psShows(t, "BAY-1", " online 1:Faulted(GroundFailure)")
	psShows(t, "GONE-1", " offline 1:Available")
	psShows(t, "SPARE-1", " stopped 0:Faulted(InternalError) 1:Available")
	if n := csms.Connects("GONE-1"); n != 1 {
		t.Errorf("GONE-1 connected %d times, want only the once it was told to by hand", n)
	}
	// The declared EV is a fresh one each time.
	if _, stdout, _ := run("inspect", "LOBBY-1"); !strings.Contains(stdout, "EV: 50% of 40 kWh") {
		t.Errorf("inspect LOBBY-1 does not show the declared EV:\n%s", stdout)
	}
}

func TestInspectShowsDeclaredState(t *testing.T) {
	useTempDataDir(t)
	csms := csmstest.Start(t)
	path := writeSite(t, csms.URL(), declaredFleet)
	startSiteUp(t, csms, path, []string{"LOBBY-1", "BAY-1"})

	_, stdout, _ := run("inspect", "LOBBY-1")
	if !strings.Contains(stdout, "  Declared    connector 1 with EV small\n") {
		t.Errorf("inspect LOBBY-1 does not show its declared state:\n%s", stdout)
	}

	// A stopped charger shows the state it will start in.
	_, stdout, _ = run("inspect", "SPARE-1")
	for _, want := range []string{
		"  Connection  stopped\n",
		"  Declared    stopped, the charger with fault InternalError\n",
		"  Fault       InternalError\n",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("inspect SPARE-1 does not show %q:\n%s", want, stdout)
		}
	}

	type declared struct {
		Connection string `json:"connection"`
		Connectors []struct {
			ID    int    `json:"id"`
			EV    string `json:"ev"`
			Fault *struct {
				Code string `json:"code"`
			} `json:"fault"`
		} `json:"connectors"`
	}
	var view struct {
		Declared *declared `json:"declared"`
	}
	_, stdout, _ = run("inspect", "SPARE-1", "--format", "json")
	if err := json.Unmarshal([]byte(stdout), &view); err != nil {
		t.Fatalf("inspect --format json: %v\n%s", err, stdout)
	}
	d := view.Declared
	if d == nil || d.Connection != "stopped" || len(d.Connectors) != 1 || d.Connectors[0].ID != 0 || d.Connectors[0].Fault == nil || d.Connectors[0].Fault.Code != "InternalError" {
		t.Errorf("declared = %+v, want stopped with a fault on the charger as a whole", d)
	}
	_, stdout, _ = run("inspect", "LOBBY-1", "--format", "json")
	view.Declared = nil
	if err := json.Unmarshal([]byte(stdout), &view); err != nil {
		t.Fatalf("inspect --format json: %v\n%s", err, stdout)
	}
	if d := view.Declared; d == nil || len(d.Connectors) != 1 || d.Connectors[0].EV != "small" {
		t.Errorf("declared = %+v, want the EV preset small on connector 1", d)
	}
}

func TestASiteWhereEveryChargerIsDeclaredStopped(t *testing.T) {
	useTempDataDir(t)
	csms := csmstest.Start(t)
	t.Setenv(asCLIEnv, "1")
	path := writeSite(t, csms.URL(), `
name: spares
chargers:
  spare: { model: ./Chargerfile, id: "SPARE-{n}", count: 2, state: { connection: stopped } }
`)
	t.Cleanup(func() { run("site", "down", "-s", "spares") })

	// Nothing is powered on, which is odd but not an error.
	code, stdout, stderr := run("site", "up", "-d", "-f", path)
	want := "Site spares has 2 chargers, all declared stopped: none was powered on\n"
	if code != ExitOK || stdout != want {
		t.Fatalf("site up -d: exit %d, stderr %q, stdout %q, want %q", code, stderr, stdout, want)
	}
	psShows(t, "SPARE-1", " stopped 1:Available")
	if code, stdout, _ := run("site", "up", "-f", path); code != ExitOK || stdout != want {
		t.Errorf("site up in the foreground: exit %d, stdout %q, want %q", code, stdout, want)
	}
	if code, stdout, _ := run("site", "start", "-s", "spares"); code != ExitOK || stdout != want {
		t.Errorf("site start: exit %d, stdout %q, want %q", code, stdout, want)
	}

	// A charger started by name runs until it is stopped.
	if code, stdout, stderr := run("start", "SPARE-2"); code != ExitOK {
		t.Fatalf("start SPARE-2: exit %d, stdout %q, stderr %q", code, stdout, stderr)
	}
	csms.WaitForCalls(t, "SPARE-2", v16.ActionBootNotification, 1)
	psShows(t, "SPARE-1", " stopped 1:Available")
}
