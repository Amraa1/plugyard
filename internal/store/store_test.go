package store

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Amraa1/plugyard/internal/charger"
	"github.com/Amraa1/plugyard/internal/ev"
	"github.com/Amraa1/plugyard/internal/model"
)

func testSpec() Spec {
	return Spec{
		Model: &model.Model{
			Vendor: "Acme", Name: "Wallbox", Firmware: "1.0",
			Connectors: []model.Connector{{ID: 1, Type: "Type2", MaxPower: 7400}},
			Config:     map[string]string{"HeartbeatInterval": "60"},
			OnEvFull:   model.OnEvFullStop,
			StartOn:    model.StartOnTap,
		},
		CSMSURL:     "ws://localhost:9000/ocpp",
		HasPassword: true,
		EV:          "compact",
	}
}

func openTemp(t *testing.T, name string) (*Store, string) {
	t.Helper()
	dir := t.TempDir()
	st, err := Open(dir, name)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st, dir
}

func TestStateSurvivesClosingAndReopening(t *testing.T) {
	st, dir := openTemp(t, "depot")

	battery := 40000.0
	meta := Meta{Name: "depot", File: "/sites/site.yaml", Speed: 60, EVs: map[string]ev.Overrides{"compact": {BatteryWh: &battery}}}
	if err := st.SetMeta(meta); err != nil {
		t.Fatal(err)
	}
	err := st.Apply(Change{Put: []Charger{
		{ID: "B", Name: "brisk_volt", Power: true, Spec: testSpec()},
		{ID: "A", Name: "quiet_ampere", Power: false, Spec: testSpec()},
	}})
	if err != nil {
		t.Fatal(err)
	}

	// A charger that has never run has no state.
	if state, err := st.LoadState("A"); err != nil || state != nil {
		t.Errorf("LoadState before any save = %v, %v, want nil", state, err)
	}
	at := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	state := charger.State{SavedAt: at, Connectors: []charger.ConnectorState{{ID: 1, MeterWh: 1500.5, Finishing: true}}}
	if err := st.SaveState("A", state); err != nil {
		t.Fatal(err)
	}
	st.Close()

	st, err = OpenExisting(dir, "depot")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	got, err := st.Load()
	if err != nil {
		t.Fatal(err)
	}
	if got.Meta.Name != "depot" || got.Meta.Speed != 60 || *got.Meta.EVs["compact"].BatteryWh != 40000 {
		t.Errorf("meta = %+v", got.Meta)
	}
	if len(got.Chargers) != 2 || got.Chargers[0].ID != "A" || got.Chargers[1].ID != "B" {
		t.Fatalf("chargers = %+v, want A then B", got.Chargers)
	}
	a, b := got.Chargers[0], got.Chargers[1]
	if a.Name != "quiet_ampere" || a.Power || !b.Power {
		t.Errorf("A = %+v, B power = %v", a, b.Power)
	}
	if a.Spec.Model.Connectors[0].MaxPower != 7400 || a.Spec.Model.Config["HeartbeatInterval"] != "60" || !a.Spec.HasPassword || a.Spec.EV != "compact" {
		t.Errorf("A's spec = %+v", a.Spec)
	}
	if a.State == nil || !a.State.SavedAt.Equal(at) || a.State.Connectors[0].MeterWh != 1500.5 || !a.State.Connectors[0].Finishing {
		t.Errorf("A's state = %+v", a.State)
	}
	if b.State != nil {
		t.Errorf("B's state = %+v, want none", b.State)
	}
}

func TestApplyKeepsStateOnUpdateAndWipesItOnReset(t *testing.T) {
	st, _ := openTemp(t, "depot")
	put := func(name string) {
		t.Helper()
		if err := st.Apply(Change{Put: []Charger{{ID: "A", Name: name, Power: true, Spec: testSpec()}}}); err != nil {
			t.Fatal(err)
		}
	}
	put("first")
	if err := st.SaveState("A", charger.State{Connectors: []charger.ConnectorState{{ID: 1, MeterWh: 10}}}); err != nil {
		t.Fatal(err)
	}

	// Putting the charger again renames it and keeps its state.
	put("second")
	chargers, _ := st.Chargers()
	if chargers[0].Name != "second" || chargers[0].State == nil || chargers[0].State.Connectors[0].MeterWh != 10 {
		t.Errorf("after a second Put: %+v", chargers[0])
	}

	if err := st.Apply(Change{Reset: []string{"A"}}); err != nil {
		t.Fatal(err)
	}
	if state, _ := st.LoadState("A"); state != nil {
		t.Errorf("state after Reset = %+v, want none", state)
	}

	if err := st.Remove("A"); err != nil {
		t.Fatal(err)
	}
	if chargers, _ := st.Chargers(); len(chargers) != 0 {
		t.Errorf("chargers after Remove = %+v", chargers)
	}
	// Saving for a charger that is gone changes nothing.
	if err := st.SaveState("A", charger.State{}); err != nil {
		t.Errorf("SaveState for a removed charger: %v", err)
	}
	if chargers, _ := st.Chargers(); len(chargers) != 0 {
		t.Error("SaveState brought a removed charger back")
	}
}

func TestNamesAreUniqueWithinASite(t *testing.T) {
	st, _ := openTemp(t, "depot")
	err := st.Apply(Change{Put: []Charger{
		{ID: "A", Name: "same", Spec: testSpec()},
		{ID: "B", Name: "same", Spec: testSpec()},
	}})
	if err == nil {
		t.Fatal("two chargers got the same name")
	}
	// The change is applied as a whole or not at all.
	if chargers, _ := st.Chargers(); len(chargers) != 0 {
		t.Errorf("a failed change left %d chargers behind", len(chargers))
	}
}

func TestSetPower(t *testing.T) {
	st, _ := openTemp(t, "depot")
	var put []Charger
	for _, id := range []string{"A", "B", "C"} {
		put = append(put, Charger{ID: id, Name: "n-" + id, Power: true, Spec: testSpec()})
	}
	if err := st.Apply(Change{Put: put}); err != nil {
		t.Fatal(err)
	}
	powered := func() string {
		chargers, _ := st.Chargers()
		var on []string
		for _, c := range chargers {
			if c.Power {
				on = append(on, c.ID)
			}
		}
		return strings.Join(on, ",")
	}

	if err := st.SetPower([]string{"B"}); err != nil || powered() != "B" {
		t.Errorf("after SetPower(B): %q, %v", powered(), err)
	}
	if err := st.SetPower(nil); err != nil || powered() != "A,B,C" {
		t.Errorf("after SetPower(all): %q, %v", powered(), err)
	}
}

func TestManySavesAtOnce(t *testing.T) {
	st, _ := openTemp(t, "depot")
	const chargers = 200
	var put []Charger
	for i := range chargers {
		put = append(put, Charger{ID: fmt.Sprintf("CP-%03d", i), Name: fmt.Sprintf("n-%d", i), Spec: testSpec()})
	}
	if err := st.Apply(Change{Put: put}); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	errs := make(chan error, chargers)
	for i := range chargers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			state := charger.State{Connectors: []charger.ConnectorState{{ID: 1, MeterWh: float64(i)}}}
			errs <- st.SaveState(fmt.Sprintf("CP-%03d", i), state)
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}

	saved, _ := st.Chargers()
	for i, c := range saved {
		if c.State == nil || c.State.Connectors[0].MeterWh != float64(i) {
			t.Fatalf("%s has state %+v, want meter %d", c.ID, c.State, i)
		}
	}

	st.Close()
	if err := st.SaveState("CP-000", charger.State{}); err == nil {
		t.Error("SaveState after Close succeeded")
	}
}

func TestAllAndDelete(t *testing.T) {
	dir := t.TempDir()
	if sites, err := All(filepath.Join(dir, "missing")); err != nil || len(sites) != 0 {
		t.Errorf("All on a missing directory = %v, %v", sites, err)
	}
	if _, err := OpenExisting(dir, "depot"); !errors.Is(err, ErrNoSite) {
		t.Errorf("OpenExisting with nothing saved returned %v, want ErrNoSite", err)
	}

	for _, name := range []string{"yard", "depot"} {
		st, err := Open(dir, name)
		if err != nil {
			t.Fatal(err)
		}
		if err := st.Apply(Change{Put: []Charger{{ID: "A", Name: "a-" + name, Spec: testSpec()}}}); err != nil {
			t.Fatal(err)
		}
		st.Close()
	}

	sites, err := All(dir)
	if err != nil || len(sites) != 2 || sites[0].Meta.Name != "depot" || sites[1].Meta.Name != "yard" {
		t.Fatalf("All = %+v, %v, want depot then yard", sites, err)
	}
	if sites[1].Chargers[0].Name != "a-yard" {
		t.Errorf("yard's charger = %+v", sites[1].Chargers[0])
	}

	if err := os.WriteFile(LogPath(dir, "depot"), []byte("log"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Delete(dir, "depot"); err != nil {
		t.Fatal(err)
	}
	if Exists(dir, "depot") || !Exists(dir, "yard") {
		t.Error("Delete removed the wrong site, or none")
	}
	if left, _ := filepath.Glob(filepath.Join(dir, "*")); len(left) != 1 {
		t.Errorf("after deleting depot the directory holds %v, want only yard's database", left)
	}
	if err := Delete(dir, "depot"); err != nil {
		t.Errorf("Delete of a site that is gone: %v", err)
	}
}

func TestMemoryStoreLeavesNothingOnDisk(t *testing.T) {
	st, err := OpenMemory("CP-1")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.Apply(Change{Put: []Charger{{ID: "CP-1", Name: "n", Spec: testSpec()}}}); err != nil {
		t.Fatal(err)
	}
	if err := st.SaveState("CP-1", charger.State{Connectors: []charger.ConnectorState{{ID: 1, MeterWh: 5}}}); err != nil {
		t.Fatal(err)
	}
	state, err := st.LoadState("CP-1")
	if err != nil || state == nil || state.Connectors[0].MeterWh != 5 {
		t.Errorf("LoadState = %+v, %v", state, err)
	}
	if meta, _ := st.Meta(); meta.Name != "CP-1" {
		t.Errorf("meta = %+v", meta)
	}
}

func TestStateFromANewerVersionIsRefused(t *testing.T) {
	st, dir := openTemp(t, "depot")
	st.Close()

	db, err := sql.Open("sqlite", Path(dir, "depot"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(fmt.Sprintf("PRAGMA user_version = %d", schemaVersion+1)); err != nil {
		t.Fatal(err)
	}
	db.Close()

	if _, err := Open(dir, "depot"); err == nil || !strings.Contains(err.Error(), "newer version of plugyard") {
		t.Errorf("Open returned %v, want a refusal that names a newer version", err)
	}
}
