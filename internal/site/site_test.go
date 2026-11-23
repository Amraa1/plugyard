package site

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Amraa1/plugyard/internal/charger"
	"github.com/Amraa1/plugyard/internal/csmstest"
	"github.com/Amraa1/plugyard/internal/ev"
	"github.com/Amraa1/plugyard/internal/model"
	"github.com/Amraa1/plugyard/internal/ocpp/v16"
)

func ptr(v float64) *float64 { return &v }

func testModel() *model.Model {
	return &model.Model{
		Vendor: "Acme", Name: "Wallbox", Firmware: "1.0",
		Connectors: []model.Connector{{ID: 1, Type: "Type2", MaxPower: 7400}},
		Config:     map[string]string{},
		OnEvFull:   model.OnEvFullStop,
		StartOn:    model.StartOnPlug,
		StartTag:   "TAG",
	}
}

// memStore keeps charger state in memory, as the real store keeps it on
// disk.
type memStore struct {
	mu      sync.Mutex
	states  map[string]charger.State
	removed []string
}

func (m *memStore) LoadState(id string) (*charger.State, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if st, ok := m.states[id]; ok {
		return &st, nil
	}
	return nil, nil
}

func (m *memStore) SaveState(id string, st charger.State) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.states == nil {
		m.states = map[string]charger.State{}
	}
	m.states[id] = st
	return nil
}

func (m *memStore) Remove(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.states, id)
	m.removed = append(m.removed, id)
	return nil
}

func TestEVParamsPrecedence(t *testing.T) {
	dc, _ := ev.BuiltinCurve(ev.CurveDCFast)
	csms := charger.CSMS{URL: "ws://localhost:9000/ocpp"}
	s, err := New(Config{
		Name: "depot",
		Presets: map[string]ev.Overrides{
			"longrange": {BatteryWh: ptr(82000), SoC: ptr(0.10), Curve: &dc},
			"compact":   {BatteryWh: ptr(40000), MaxPowerW: ptr(50000)},
		},
	}, []Member{
		{ID: "FAST-1", Model: testModel(), CSMS: csms, EV: "longrange"},
		{ID: "LOBBY-1", Model: testModel(), CSMS: csms},
	})
	if err != nil {
		t.Fatal(err)
	}

	// Nothing given, no default preset: the built-in EV.
	got, err := s.EVParams("LOBBY-1", "", ev.Overrides{})
	if err != nil || got.BatteryWh != 60000 || got.SoC != 0.20 || got.TargetSoC != 1 {
		t.Errorf("built-in EV = %+v, %v", got, err)
	}

	// The charger's default preset fills in over the built-in EV.
	got, _ = s.EVParams("FAST-1", "", ev.Overrides{})
	if got.BatteryWh != 82000 || got.SoC != 0.10 || got.TargetSoC != 1 || got.Curve.Name != ev.CurveDCFast {
		t.Errorf("default preset = %+v", got)
	}

	// A named preset goes over the default one; what it does not give stays.
	got, _ = s.EVParams("FAST-1", "compact", ev.Overrides{})
	if got.BatteryWh != 40000 || got.MaxPowerW != 50000 || got.SoC != 0.10 {
		t.Errorf("named preset = %+v", got)
	}

	// Parameters given with the plug win over everything.
	got, _ = s.EVParams("FAST-1", "compact", ev.Overrides{BatteryWh: ptr(100000), SoC: ptr(0.5)})
	if got.BatteryWh != 100000 || got.SoC != 0.5 || got.MaxPowerW != 50000 {
		t.Errorf("flags = %+v", got)
	}

	_, err = s.EVParams("FAST-1", "ghost", ev.Overrides{})
	if err == nil || !strings.Contains(err.Error(), `no EV preset named "ghost"; it has compact, longrange`) {
		t.Errorf("unknown preset error = %v", err)
	}
}

func TestNewRejectsAChargerThatCannotBeCreated(t *testing.T) {
	_, err := New(Config{Name: "depot"}, []Member{{ID: "A", Model: testModel(), CSMS: charger.CSMS{URL: "http://nope"}}})
	if err == nil || !strings.Contains(err.Error(), "charger A") {
		t.Errorf("New returned %v, want an error naming charger A", err)
	}
}

// startSite runs a two-charger site against csms and returns it with a
// function that reports whether Run has returned.
func startSite(t *testing.T, csms *csmstest.CSMS, st Store, powerOn func(string) bool) (*Site, chan struct{}) {
	t.Helper()
	members := []Member{
		{ID: "A", Name: "a", Model: testModel(), CSMS: charger.CSMS{URL: csms.URL()}},
		{ID: "B", Name: "b", Model: testModel(), CSMS: charger.CSMS{URL: csms.URL()}},
	}
	s, err := New(Config{
		Name:   "depot",
		Speed:  3600,
		Store:  st,
		Timing: charger.Timing{CallTimeout: 2 * time.Second, Tick: 10 * time.Millisecond},
	}, members)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		_ = s.Run(ctx, powerOn)
		close(done)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	return s, done
}

// count returns how many calls of an action a charger has made.
func count(csms *csmstest.CSMS, id, action string) int {
	n := 0
	for _, a := range csms.Actions(id) {
		if a == action {
			n++
		}
	}
	return n
}

func ended(done chan struct{}, within time.Duration) bool {
	select {
	case <-done:
		return true
	case <-time.After(within):
		return false
	}
}

func TestStopAndStartOneCharger(t *testing.T) {
	csms := csmstest.Start(t)
	st := &memStore{}
	s, done := startSite(t, csms, st, nil)
	csms.WaitForCalls(t, "A", v16.ActionStatusNotification, 1)
	csms.WaitForCalls(t, "B", v16.ActionStatusNotification, 1)

	if _, err := s.Charger("A").Plug(context.Background(), 1, ev.Defaults()); err != nil {
		t.Fatal(err)
	}
	csms.WaitForCalls(t, "A", v16.ActionStartTransaction, 1)

	// Stop is orderly: the transaction ends and the connection closes.
	if msg, err := s.Stop("A"); err != nil || msg != "stopped" {
		t.Fatalf("Stop = %q, %v", msg, err)
	}
	csms.WaitForCalls(t, "A", v16.ActionStopTransaction, 1)
	if ends := csms.WaitDisconnects(t, "A", 1); !ends[0].Clean {
		t.Error("A was dropped by Stop, want a normal close")
	}
	if s.Charger("A") != nil || s.Powered() != 1 {
		t.Errorf("after Stop: Charger(A) = %v, Powered = %d", s.Charger("A"), s.Powered())
	}
	if msg, _ := s.Stop("A"); msg != "already stopped" {
		t.Errorf("second Stop = %q", msg)
	}

	// While stopped, the charger shows as stopped with its EV still in.
	for _, state := range s.States() {
		if state.ChargePointID != "A" {
			continue
		}
		if state.Powered || state.Connection != charger.Stopped || state.Connectors[0].Status != v16.StatusFinishing {
			t.Errorf("stopped A = %+v", state)
		}
	}
	if ended(done, 50*time.Millisecond) {
		t.Fatal("the site ended although B is still powered on")
	}

	// Start brings the same charger back: it boots again and its EV is
	// still plugged in, waiting to be unplugged.
	if msg, err := s.Start("A"); err != nil || msg != "started" {
		t.Fatalf("Start = %q, %v", msg, err)
	}
	csms.WaitForCalls(t, "A", v16.ActionBootNotification, 2)
	csms.WaitForCalls(t, "A", v16.ActionStatusNotification, 4)
	if got := s.Charger("A").Snapshot().Connectors[0].Status; got != v16.StatusFinishing {
		t.Errorf("after Start the connector is %s, want Finishing", got)
	}
	if n := count(csms, "A", v16.ActionStartTransaction); n != 1 {
		t.Errorf("A sent %d StartTransaction, want only the first: no transaction starts by itself", n)
	}
}

func TestKillThenStartReportsPowerLoss(t *testing.T) {
	csms := csmstest.Start(t)
	st := &memStore{}
	s, _ := startSite(t, csms, st, nil)
	csms.WaitForCalls(t, "A", v16.ActionStatusNotification, 1)

	if _, err := s.Charger("A").Plug(context.Background(), 1, ev.Defaults()); err != nil {
		t.Fatal(err)
	}
	csms.WaitForCalls(t, "A", v16.ActionStartTransaction, 1)

	if msg, err := s.Kill("A"); err != nil || msg != "killed" {
		t.Fatalf("Kill = %q, %v", msg, err)
	}
	if ends := csms.WaitDisconnects(t, "A", 1); ends[0].Clean {
		t.Error("A closed the connection normally on Kill, want a drop")
	}
	if n := count(csms, "A", v16.ActionStopTransaction); n != 0 {
		t.Errorf("A sent %d StopTransaction on Kill, want none", n)
	}
	if _, err := s.Kill("A"); err == nil {
		t.Error("Kill on a charger that is off succeeded, want an error")
	}

	if _, err := s.Start("A"); err != nil {
		t.Fatal(err)
	}
	stops := csms.WaitForCalls(t, "A", v16.ActionStopTransaction, 1)
	if !strings.Contains(string(stops[0].Payload), `"reason":"PowerLoss"`) {
		t.Errorf("StopTransaction after a power cut = %s, want reason PowerLoss", stops[0])
	}
}

func TestSiteEndsWhenItsLastChargerStops(t *testing.T) {
	csms := csmstest.Start(t)
	// Only A is powered on at first.
	s, done := startSite(t, csms, &memStore{}, func(id string) bool { return id == "A" })
	csms.WaitForCalls(t, "A", v16.ActionStatusNotification, 1)
	if s.Powered() != 1 || s.Len() != 2 {
		t.Fatalf("Powered = %d, Len = %d, want 1 of 2", s.Powered(), s.Len())
	}

	// Restarting the only powered charger does not end the site.
	if msg, err := s.Restart("A"); err != nil || msg != "restarted" {
		t.Fatalf("Restart = %q, %v", msg, err)
	}
	csms.WaitForCalls(t, "A", v16.ActionBootNotification, 2)
	if ended(done, 50*time.Millisecond) {
		t.Fatal("the site ended during a restart")
	}

	if _, err := s.Stop("A"); err != nil {
		t.Fatal(err)
	}
	if !ended(done, 5*time.Second) {
		t.Fatal("the site kept running with no charger powered on")
	}
}

func TestRemove(t *testing.T) {
	csms := csmstest.Start(t)
	st := &memStore{}
	s, _ := startSite(t, csms, st, nil)
	csms.WaitForCalls(t, "A", v16.ActionStatusNotification, 1)

	if _, err := s.Remove("A", false); err == nil || !strings.Contains(err.Error(), "use -f") {
		t.Errorf("Remove of a running charger returned %v, want a refusal naming -f", err)
	}
	if msg, err := s.Remove("A", true); err != nil || msg != "removed" {
		t.Fatalf("Remove -f = %q, %v", msg, err)
	}
	if s.Has("A") || s.Len() != 1 {
		t.Errorf("A is still in the site after Remove")
	}
	if len(st.removed) != 1 || st.removed[0] != "A" {
		t.Errorf("the store was asked to remove %v, want [A]", st.removed)
	}
	if _, err := s.Start("A"); err == nil {
		t.Error("Start of a removed charger succeeded")
	}
}

func TestPacerSpacesConnections(t *testing.T) {
	if NewPacer(0) != nil {
		t.Error("a rate of 0 returned a pacer, want none: no limit")
	}

	// 50 a second: the first passes at once, then one every 20ms.
	pace := NewPacer(50)
	start := time.Now()
	for range 6 {
		if err := pace(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if elapsed := time.Since(start); elapsed < 90*time.Millisecond || elapsed > 400*time.Millisecond {
		t.Errorf("6 connections at 50 a second took %s, want about 100ms", elapsed)
	}

	// A cancelled context ends the wait.
	slow := NewPacer(0.01)
	_ = slow(context.Background())
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := slow(ctx); err == nil {
		t.Error("the pacer returned without an error although its context ended")
	}
}
