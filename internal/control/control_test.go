package control_test

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Amraa1/plugyard/internal/charger"
	"github.com/Amraa1/plugyard/internal/control"
	"github.com/Amraa1/plugyard/internal/csmstest"
	"github.com/Amraa1/plugyard/internal/ev"
	"github.com/Amraa1/plugyard/internal/model"
	"github.com/Amraa1/plugyard/internal/ocpp/v16"
	"github.com/Amraa1/plugyard/internal/site"
)

var ctx = context.Background()

// shortTempDir returns a temporary directory with a short path. Socket paths
// have a tight length limit, which t.TempDir can exceed on macOS.
func shortTempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "py")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

// startSite runs a site with the given chargers against csms and serves its
// control socket in dataDir.
func startSite(t *testing.T, dataDir, name string, csms *csmstest.CSMS, ids ...string) *control.Server {
	t.Helper()
	m := &model.Model{
		Vendor: "Acme", Name: "Wallbox", Firmware: "1.0",
		Connectors: []model.Connector{{ID: 1, Type: "Type2", MaxPower: 7400}},
		Config:     map[string]string{},
		OnEvFull:   model.OnEvFullStop,
		StartOn:    model.StartOnTap,
	}

	runCtx, cancel := context.WithCancel(ctx)
	var chargers []*charger.Charger
	done := make(chan struct{})
	for _, id := range ids {
		c, err := charger.New(charger.Config{
			ID: id, Model: m, CSMS: charger.CSMS{URL: csms.URL()},
			Timing: charger.Timing{CallTimeout: 2 * time.Second, Tick: 10 * time.Millisecond},
		})
		if err != nil {
			t.Fatal(err)
		}
		chargers = append(chargers, c)
		go func() {
			_ = c.Run(runCtx)
			done <- struct{}{}
		}()
	}
	t.Cleanup(func() {
		cancel()
		for range ids {
			<-done
		}
	})
	for _, id := range ids {
		csms.WaitForCalls(t, id, v16.ActionStatusNotification, 1)
	}

	server, err := control.Serve(dataDir, site.New(name, chargers...), nil)
	if err != nil {
		t.Fatalf("Serve: %v", err)
	}
	t.Cleanup(server.Close)
	return server
}

func discover(t *testing.T, dataDir string) []*control.Client {
	t.Helper()
	clients, err := control.Discover(ctx, dataDir)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	t.Cleanup(func() {
		for _, c := range clients {
			c.Close()
		}
	})
	return clients
}

func TestDiscoverFindsRunningSites(t *testing.T) {
	csms := csmstest.Start(t)
	dir := shortTempDir(t)
	startSite(t, dir, "depot-north", csms, "N-2", "N-1")
	startSite(t, dir, "depot-south", csms, "S-1")

	clients := discover(t, dir)

	if len(clients) != 2 || clients[0].Info.Site != "depot-north" || clients[1].Info.Site != "depot-south" {
		t.Fatalf("Discover found %d sites, want depot-north and depot-south", len(clients))
	}
	if clients[0].Info.PID != os.Getpid() || clients[0].Info.Protocol != control.ProtocolVersion {
		t.Errorf("Info = %+v", clients[0].Info)
	}

	chargers, err := clients[0].Chargers(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(chargers) != 2 || chargers[0].ChargePointID != "N-1" || chargers[1].ChargePointID != "N-2" {
		t.Fatalf("Chargers = %+v, want N-1 then N-2", chargers)
	}
	got := chargers[0]
	if got.Site != "depot-north" || got.Connection != charger.Online || got.Connectors[0].Status != v16.StatusAvailable {
		t.Errorf("charger state = %+v", got)
	}
}

func TestDiscoverWithNoDataDirectoryFindsNothing(t *testing.T) {
	clients, err := control.Discover(ctx, shortTempDir(t)+"/missing")
	if err != nil || len(clients) != 0 {
		t.Errorf("Discover = %d sites, %v; want none and no error", len(clients), err)
	}
}

func TestExecRunsAnActionAndReportsEachCharger(t *testing.T) {
	csms := csmstest.Start(t)
	dir := shortTempDir(t)
	startSite(t, dir, "depot", csms, "A", "B")
	client := discover(t, dir)[0]

	soc := 0.5
	plug := control.ExecRequest{
		ChargePointIDs: []string{"A", "B", "GHOST"},
		Action:         control.ActionPlug,
		Connector:      1,
		EV:             ev.Overrides{SoC: &soc},
	}
	results, err := client.Exec(ctx, plug)
	if err != nil {
		t.Fatal(err)
	}

	want := []control.ExecResult{
		{ChargePointID: "A", Connector: 1, OK: true, Message: "EV plugged in (60 kWh, 50%)"},
		{ChargePointID: "B", Connector: 1, OK: true, Message: "EV plugged in (60 kWh, 50%)"},
		{ChargePointID: "GHOST", Connector: 1, OK: false, Message: "no such charger in this site"},
	}
	if len(results) != len(want) {
		t.Fatalf("got %d results, want %d", len(results), len(want))
	}
	for i := range want {
		if results[i] != want[i] {
			t.Errorf("result %d = %+v, want %+v", i, results[i], want[i])
		}
	}

	// The same action again cannot be carried out, and says why.
	plug.ChargePointIDs = []string{"A"}
	results, err = client.Exec(ctx, plug)
	if err != nil {
		t.Fatal(err)
	}
	if results[0].OK || results[0].Message != "the connector already has an EV" {
		t.Errorf("second plug = %+v, want a failure naming the reason", results[0])
	}

	// A tap goes all the way to the CSMS and back.
	results, err = client.Exec(ctx, control.ExecRequest{
		ChargePointIDs: []string{"A"}, Action: control.ActionTap, Connector: 1, Tag: "TAG-A",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !results[0].OK || results[0].Message != "tag accepted, transaction 1 started" {
		t.Errorf("tap = %+v", results[0])
	}
	csms.WaitForCalls(t, "A", v16.ActionStartTransaction, 1)

	results, err = client.Exec(ctx, control.ExecRequest{
		ChargePointIDs: []string{"A"}, Action: "dance", Connector: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if results[0].OK {
		t.Errorf("unknown action succeeded: %+v", results[0])
	}
}

func TestServeRefusesASiteThatIsAlreadyRunning(t *testing.T) {
	csms := csmstest.Start(t)
	dir := shortTempDir(t)
	startSite(t, dir, "depot", csms, "A")

	_, err := control.Serve(dir, site.New("depot"), nil)
	if !errors.Is(err, control.ErrAlreadyRunning) {
		t.Errorf("second Serve returned %v, want ErrAlreadyRunning", err)
	}
}

func TestSocketLeftByADeadProcessIsCleanedUp(t *testing.T) {
	dir := shortTempDir(t)
	stale := control.SocketPath(dir, "depot")
	if err := os.WriteFile(stale, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	// Discovery ignores and removes it.
	if clients := discover(t, dir); len(clients) != 0 {
		t.Fatalf("Discover found %d sites behind a dead socket, want none", len(clients))
	}
	if _, err := os.Lstat(stale); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the dead socket file is still there (Lstat: %v)", err)
	}

	// And it does not stop the site from starting again.
	if err := os.WriteFile(stale, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	server, err := control.Serve(dir, site.New("depot"), nil)
	if err != nil {
		t.Fatalf("Serve over a dead socket: %v", err)
	}
	server.Close()
	if _, err := os.Lstat(stale); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("Close left the socket file behind (Lstat: %v)", err)
	}
}

func TestSocketPathIsShortAndStable(t *testing.T) {
	a := control.SocketPath("/data", "Depot A#1 with a very long name indeed, far beyond any socket path limit")
	b := control.SocketPath("/data", "depot-b")
	if a == b {
		t.Error("two sites share a socket path")
	}
	if len(a) != len(b) {
		t.Errorf("socket paths differ in length: %q and %q", a, b)
	}
	if a != control.SocketPath("/data", "Depot A#1 with a very long name indeed, far beyond any socket path limit") {
		t.Error("the socket path of a site is not stable")
	}
}

func TestServeExplainsADataDirectoryPathThatIsTooLong(t *testing.T) {
	dir := shortTempDir(t) + "/" + strings.Repeat("deep/", 30)

	_, err := control.Serve(dir, site.New("depot"), nil)
	if err == nil || !strings.Contains(err.Error(), "too long for a socket") || !strings.Contains(err.Error(), "PLUGYARD_DATA_DIR") {
		t.Errorf("Serve returned %v, want an explanation that names PLUGYARD_DATA_DIR", err)
	}
}
