package sitefile

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Amraa1/plugyard/internal/charger"
	"github.com/Amraa1/plugyard/internal/ev"
)

const fastChargerfile = `
vendor: Acme
model: FastOne 50
firmware: 1.4.2
connectors:
  - { id: 1, type: CCS2, maxPower: 50kW }
config:
  MeterValueSampleInterval: 60
`

const acChargerfile = `
vendor: Acme
model: Wallbox
firmware: "1.0"
connectors:
  - { id: 1, type: Type2, maxPower: 7.4kW }
`

// dirWithModels returns a directory holding the two Chargerfiles the tests
// refer to.
func dirWithModels(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for name, content := range map[string]string{"fast.Chargerfile": fastChargerfile, "ac.Chargerfile": acChargerfile} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func env(vars map[string]string) func(string) (string, bool) {
	return func(name string) (string, bool) {
		v, ok := vars[name]
		return v, ok
	}
}

func parse(t *testing.T, yaml string, vars map[string]string) (*Site, error) {
	t.Helper()
	return Parse([]byte(yaml), dirWithModels(t), env(vars))
}

func mustParse(t *testing.T, yaml string, vars map[string]string) *Site {
	t.Helper()
	site, err := parse(t, yaml, vars)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	return site
}

// The example from docs/SPEC.md section 6.
const specExample = `
name: depot-north
csms:
  url: wss://csms.example.com/ocpp
  password: ${CSMS_PASSWORD}
  insecureSkipVerify: false
speed: 1
evs:
  compact:   { battery: 40kWh, maxPower: 50kW }
  longrange: { battery: 82kWh, maxPower: 170kW, soc: 10% }
chargers:
  fast:
    model: ./fast.Chargerfile
    count: 20
    id: "FAST-{n:03}"
    ev: longrange
  lobby:
    model: ./ac.Chargerfile
    id: "LOBBY-1"
    config: { MeterValueSampleInterval: 10 }
`

func TestParseSpecExample(t *testing.T) {
	site := mustParse(t, specExample, map[string]string{"CSMS_PASSWORD": "s3cret"})

	if site.Name != "depot-north" || site.Speed != 1 || site.ConnectRate != 0 {
		t.Errorf("site = %q, speed %v, connectRate %v", site.Name, site.Speed, site.ConnectRate)
	}
	if len(site.Entries) != 2 || site.Entries[0].Name != "fast" || site.Entries[1].Name != "lobby" {
		t.Fatalf("entries are not fast then lobby, in file order: %+v", site.Entries)
	}

	fast := site.Entries[0].Chargers
	if len(fast) != 20 || fast[0].ID != "FAST-001" || fast[19].ID != "FAST-020" {
		t.Errorf("fast has %d chargers, %s to %s; want 20, FAST-001 to FAST-020", len(fast), fast[0].ID, fast[len(fast)-1].ID)
	}
	if fast[0].EV != "longrange" || fast[0].Model.Name != "FastOne 50" {
		t.Errorf("fast[0] = %+v", fast[0])
	}
	if fast[0].CSMS.URL != "wss://csms.example.com/ocpp" || fast[0].CSMS.Password != "s3cret" {
		t.Errorf("fast[0].CSMS = %+v, want the site's URL and the password from the environment", fast[0].CSMS)
	}
	if fast[0].Model.Config["MeterValueSampleInterval"] != "60" {
		t.Errorf("fast config = %v, want the model's own value", fast[0].Model.Config)
	}

	lobby := site.Entries[1].Chargers
	if len(lobby) != 1 || lobby[0].ID != "LOBBY-1" || lobby[0].EV != "" {
		t.Errorf("lobby = %+v", lobby)
	}
	if lobby[0].Model.Config["MeterValueSampleInterval"] != "10" {
		t.Errorf("lobby config = %v, want the entry's override", lobby[0].Model.Config)
	}

	longrange := site.EVs["longrange"]
	if *longrange.BatteryWh != 82000 || *longrange.MaxPowerW != 170000 || *longrange.SoC != 0.10 || longrange.TargetSoC != nil {
		t.Errorf("longrange preset = %+v", longrange)
	}
	if len(site.Chargers()) != 21 {
		t.Errorf("Chargers() returned %d, want 21", len(site.Chargers()))
	}
}

func TestIDTemplates(t *testing.T) {
	cases := []struct {
		entry string
		want  []string
	}{
		{`id: "CP-1"`, []string{"CP-1"}},
		{`{ id: "FAST-{n}", count: 3 }`, []string{"FAST-1", "FAST-2", "FAST-3"}},
		{`{ id: "FAST-{n:03}", count: 2 }`, []string{"FAST-001", "FAST-002"}},
		{`{ id: "{n:012}", count: 2, start: 202310090165 }`, []string{"202310090165", "202310090166"}},
		{`{ id: "p025b{n:07}", count: 1, start: 2036387 }`, []string{"p025b2036387"}},
		{`{ id: "R{n}-C{n:02}", count: 2 }`, []string{"R1-C01", "R2-C02"}},
		{`ids: [12994897, p025b2036387, "202310090165"]`, []string{"12994897", "p025b2036387", "202310090165"}},
	}
	for _, tc := range cases {
		t.Run(tc.entry, func(t *testing.T) {
			entry := strings.TrimPrefix(strings.TrimSuffix(tc.entry, " }"), "{ ")
			yaml := "name: s\ncsms: { url: ws://localhost/ocpp }\nchargers:\n  e: { model: ./ac.Chargerfile, " + entry + " }\n"
			site := mustParse(t, yaml, nil)
			var got []string
			for _, c := range site.Chargers() {
				got = append(got, c.ID)
			}
			if strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Errorf("IDs = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestEnvironmentVariables(t *testing.T) {
	yaml := func(url string) string {
		return "name: s\ncsms:\n  url: " + url + "\n  password: 'pa$$word-${SUFFIX:-none}'\nspeed: ${SPEED:-1}\nchargers:\n  e: { model: ./ac.Chargerfile, id: CP-1 }\n"
	}

	t.Run("set", func(t *testing.T) {
		site := mustParse(t, yaml("${CSMS_URL}"), map[string]string{"CSMS_URL": "ws://csms:9000/ocpp", "SUFFIX": "x", "SPEED": "60"})
		c := site.Chargers()[0]
		if c.CSMS.URL != "ws://csms:9000/ocpp" || c.CSMS.Password != "pa$word-x" || site.Speed != 60 {
			t.Errorf("URL %q, password %q, speed %v", c.CSMS.URL, c.CSMS.Password, site.Speed)
		}
	})

	t.Run("fallback", func(t *testing.T) {
		site := mustParse(t, yaml("${CSMS_URL:-ws://localhost:9000/ocpp}"), nil)
		c := site.Chargers()[0]
		if c.CSMS.URL != "ws://localhost:9000/ocpp" || c.CSMS.Password != "pa$word-none" || site.Speed != 1 {
			t.Errorf("URL %q, password %q, speed %v", c.CSMS.URL, c.CSMS.Password, site.Speed)
		}
	})

	t.Run("unset is an error that names the variable", func(t *testing.T) {
		_, err := parse(t, yaml("${CSMS_URL}"), nil)
		if err == nil || !strings.Contains(err.Error(), "CSMS_URL is not set") || !strings.Contains(err.Error(), "line 3") {
			t.Errorf("error = %v, want one naming CSMS_URL and its line", err)
		}
	})
}

func TestEVPresetCurves(t *testing.T) {
	site := mustParse(t, `
name: s
csms: { url: ws://localhost/ocpp }
evs:
  named:  { curve: dc-fast }
  custom:
    maxPower: 230kW
    curve:
      - [0%, 30%]
      - [10%, 100%]
      - [50%, 100%]
      - [80%, 28%]
      - [100%, 7%]
chargers:
  e: { model: ./ac.Chargerfile, id: CP-1 }
`, nil)

	if got := site.EVs["named"].Curve; got == nil || got.Name != ev.CurveDCFast {
		t.Errorf("named curve = %+v, want the built-in dc-fast", got)
	}
	custom := site.EVs["custom"].Curve
	if custom == nil || len(custom.Points) != 5 || custom.Points[3] != (ev.Point{SoC: 0.80, Fraction: 0.28}) {
		t.Errorf("custom curve = %+v", custom)
	}
}

func TestEntryCanOverrideTheCSMS(t *testing.T) {
	site := mustParse(t, `
name: s
csms: { url: wss://main/ocpp, password: one, insecureSkipVerify: true }
chargers:
  a: { model: ./ac.Chargerfile, id: A }
  b: { model: ./ac.Chargerfile, id: B, csms: { url: ws://other/ocpp } }
`, nil)

	a, b := site.Chargers()[0].CSMS, site.Chargers()[1].CSMS
	if a.URL != "wss://main/ocpp" || a.Password != "one" || !a.InsecureSkipVerify {
		t.Errorf("a = %+v", a)
	}
	// Only what the entry gives is replaced.
	if b.URL != "ws://other/ocpp" || b.Password != "one" || !b.InsecureSkipVerify {
		t.Errorf("b = %+v", b)
	}
}

func TestConfigOverridesDoNotLeakBetweenEntries(t *testing.T) {
	site := mustParse(t, `
name: s
csms: { url: ws://localhost/ocpp }
chargers:
  a: { model: ./fast.Chargerfile, id: A, config: { MeterValueSampleInterval: 5, Extra: true } }
  b: { model: ./fast.Chargerfile, id: B }
`, nil)

	a, b := site.Chargers()[0].Model.Config, site.Chargers()[1].Model.Config
	if a["MeterValueSampleInterval"] != "5" || a["Extra"] != "true" {
		t.Errorf("a config = %v", a)
	}
	if b["MeterValueSampleInterval"] != "60" || b["Extra"] != "" {
		t.Errorf("b config = %v, want the model's values untouched", b)
	}
}

func TestParseRejectsInvalidFiles(t *testing.T) {
	entry := func(body string) string {
		return "name: s\ncsms: { url: ws://localhost/ocpp }\nchargers:\n  e: { " + body + " }\n"
	}
	cases := []struct {
		name    string
		yaml    string
		wantErr string
	}{
		{"empty", "", "empty"},
		{"no name", "csms: { url: ws://x/ocpp }\nchargers:\n  e: { model: ./ac.Chargerfile, id: A }\n", "name is required"},
		{"no chargers", "name: s\ncsms: { url: ws://x/ocpp }\n", "at least one entry"},
		{"unknown field", entry("model: ./ac.Chargerfile, id: A") + "colour: red\n", "colour"},
		{"unknown entry field", entry("model: ./ac.Chargerfile, id: A, amount: 3"), "amount"},
		{"no model", entry("id: A"), "model is required"},
		{"missing Chargerfile", entry("model: ./nope.Chargerfile, id: A"), "nope.Chargerfile"},
		{"no id", entry("model: ./ac.Chargerfile"), "id is required"},
		{"count without {n}", entry(`model: ./ac.Chargerfile, id: FAST, count: 20`), `add {n} to the template, for example "FAST-{n}"`},
		{"unknown placeholder", entry(`model: ./ac.Chargerfile, id: "FAST-{i}"`), "{i} is not understood"},
		{"zero count", entry(`model: ./ac.Chargerfile, id: "F-{n}", count: 0`), "count must be 1 or more"},
		{"ids and id", entry(`model: ./ac.Chargerfile, id: A, ids: [B, C]`), "either ids, or id with count"},
		{"ids and wrong count", entry(`model: ./ac.Chargerfile, count: 3, ids: [B, C]`), "leave count out"},
		{"unknown EV preset", entry(`model: ./ac.Chargerfile, id: A, ev: ghost`), `no EV preset named "ghost"`},
		{"bad config value", entry(`model: ./ac.Chargerfile, id: A, config: { K: [1] }`), "config.K"},
		{"no CSMS", "name: s\nchargers:\n  e: { model: ./ac.Chargerfile, id: A }\n", "no CSMS URL"},
		{"zero speed", "speed: 0\n" + entry("model: ./ac.Chargerfile, id: A"), "speed must be above 0"},
		{"negative connectRate", "connectRate: -1\n" + entry("model: ./ac.Chargerfile, id: A"), "connectRate cannot be negative"},
		{"bad preset battery", "evs: { x: { battery: big } }\n" + entry("model: ./ac.Chargerfile, id: A"), "evs.x: battery"},
		{"unknown curve", "evs: { x: { curve: warp } }\n" + entry("model: ./ac.Chargerfile, id: A"), "evs.x: curve: unknown charge curve"},
		{"curve reaching zero", "evs: { x: { curve: [[0%, 100%], [100%, 0%]] } }\n" + entry("model: ./ac.Chargerfile, id: A"), "above 0%"},
		{"upload host with a scheme", "uploads: { allow: [\"ftp://files.example.com\"] }\n" + entry("model: ./ac.Chargerfile, id: A"), `uploads.allow: "ftp://files.example.com" is not a host`},
		{"upload host with a port", "uploads: { allow: [\"files.example.com:21\"] }\n" + entry("model: ./ac.Chargerfile, id: A"), "leave out the port"},
		{"empty upload host", "uploads: { allow: [\"\"] }\n" + entry("model: ./ac.Chargerfile, id: A"), "uploads.allow: a host cannot be empty"},
		{"unknown uploads field", "uploads: { deny: [x] }\n" + entry("model: ./ac.Chargerfile, id: A"), "deny"},
		{"malformed curve point", "evs: { x: { curve: [[0%, 100%], [100%]] } }\n" + entry("model: ./ac.Chargerfile, id: A"), "want a pair"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parse(t, tc.yaml, nil)
			if err == nil {
				t.Fatal("Parse succeeded, want an error")
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error = %q, want it to contain %q", err, tc.wantErr)
			}
		})
	}
}

func TestDuplicateChargePointIDsAreRejected(t *testing.T) {
	_, err := parse(t, `
name: s
csms: { url: ws://localhost/ocpp }
chargers:
  a: { model: ./ac.Chargerfile, id: "CP-{n}", count: 3 }
  b: { model: ./ac.Chargerfile, id: "CP-2" }
`, nil)
	if err == nil || !strings.Contains(err.Error(), `charge point ID "CP-2" is already used by chargers.a`) {
		t.Errorf("error = %v, want the duplicate named", err)
	}
}

func TestParseReportsEveryProblem(t *testing.T) {
	_, err := parse(t, `
csms: { url: ws://localhost/ocpp }
chargers:
  a: { model: ./ac.Chargerfile }
  b: { id: B }
`, nil)
	if err == nil {
		t.Fatal("Parse succeeded, want an error")
	}
	for _, want := range []string{"name is required", "chargers.a: id is required", "chargers.b: model is required"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not mention %q:\n%v", want, err)
		}
	}
}

func TestLoadResolvesModelsBesideTheSiteFile(t *testing.T) {
	dir := dirWithModels(t)
	path := filepath.Join(dir, "site.yaml")
	content := "name: depot\ncsms: { url: ws://localhost/ocpp }\nchargers:\n  e: { model: ./ac.Chargerfile, id: A }\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	// Load from somewhere else: the model path is relative to the file.
	site, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if site.Path != path || site.Chargers()[0].Model.Name != "Wallbox" {
		t.Errorf("site path %q, model %q", site.Path, site.Chargers()[0].Model.Name)
	}

	name, err := Name(path)
	if err != nil || name != "depot" {
		t.Errorf("Name = %q, %v", name, err)
	}

	// Name still works when the rest of the file has become invalid.
	if err := os.WriteFile(path, []byte("name: depot\nchargers: nonsense\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if name, err := Name(path); err != nil || name != "depot" {
		t.Errorf("Name of a broken file = %q, %v", name, err)
	}
	if _, err := Load(path); err == nil {
		t.Error("Load of a broken file succeeded")
	}
}

func TestLargeCount(t *testing.T) {
	site := mustParse(t, fmt.Sprintf("name: s\ncsms: { url: ws://localhost/ocpp }\nchargers:\n  e: { model: ./ac.Chargerfile, id: \"CP-{n:04}\", count: %d }\n", 2000), nil)
	chargers := site.Chargers()
	if len(chargers) != 2000 || chargers[1999].ID != "CP-2000" {
		t.Errorf("got %d chargers ending %s", len(chargers), chargers[len(chargers)-1].ID)
	}
	// Every charger of an entry shares one model.
	if chargers[0].Model != chargers[1999].Model {
		t.Error("chargers of one entry have separate copies of the model")
	}
}

func TestUploadsAllow(t *testing.T) {
	body := "name: s\ncsms: { url: ws://localhost/ocpp }\nchargers:\n  e: { model: ./ac.Chargerfile, id: A }\n"
	if site := mustParse(t, body, nil); site.AllowUploads != nil {
		t.Errorf("a site file without uploads allows %v", site.AllowUploads)
	}
	site := mustParse(t, "uploads: { allow: [files.example.com, 10.0.0.5, \"fd12::5\"] }\n"+body, nil)
	if got := strings.Join(site.AllowUploads, " "); got != "files.example.com 10.0.0.5 fd12::5" {
		t.Errorf("uploads.allow = %q", got)
	}
	if site.UploadsInsecureSkipVerify {
		t.Error("certificates of upload servers are not checked, though the file does not say so")
	}
	if site := mustParse(t, "uploads: { insecureSkipVerify: true }\n"+body, nil); !site.UploadsInsecureSkipVerify || site.AllowUploads != nil {
		t.Errorf("uploads.insecureSkipVerify: true gave %v and %v", site.UploadsInsecureSkipVerify, site.AllowUploads)
	}
}

const declaredStateFile = `
name: s
csms: { url: ws://localhost/ocpp }
evs:
  compact:   { battery: 40kWh, targetSoc: 90% }
  longrange: { battery: 82kWh, soc: 10% }
chargers:
  lobby:
    model: ./ac.Chargerfile
    id: LOBBY-1
    ev: compact
    state:
      connectors:
        1: { ev: longrange }
  bay:
    model: ./fast.Chargerfile
    id: "BAY-{n}"
    count: 2
    state:
      connectors:
        1: { fault: { code: GroundFailure, info: RCD tripped, vendorCode: E42 } }
        0: { fault: InternalError }
  free:
    model: ./ac.Chargerfile
    id: FREE-1
    state: { connectors: { 1: {} } }
  gone:  { model: ./ac.Chargerfile, id: GONE-1, state: { connection: offline } }
  spare: { model: ./ac.Chargerfile, id: SPARE-1, state: { connection: stopped } }
  plain: { model: ./ac.Chargerfile, id: PLAIN-1 }
  blank: { model: ./ac.Chargerfile, id: BLANK-1, state: {} }
`

func TestDeclaredState(t *testing.T) {
	site := mustParse(t, declaredStateFile, nil)
	declared := map[string]*charger.Declared{}
	for _, c := range site.Chargers() {
		declared[c.ID] = c.Declared
	}

	// The declared EV is worked out in full: the built-in EV, then the
	// entry's default preset, then the preset the connector names.
	longrange := ev.Defaults()
	longrange.BatteryWh, longrange.SoC, longrange.TargetSoC = 82000, 0.10, 0.90
	want := map[string]*charger.Declared{
		"LOBBY-1": {Connectors: []charger.DeclaredConnector{
			{ID: 1, EV: &charger.DeclaredEV{Preset: "longrange", Params: longrange}},
		}},
		// Connectors come in the order of their numbers, the charger as a
		// whole first, and every charger of an entry gets the same.
		"BAY-1": {Connectors: []charger.DeclaredConnector{
			{ID: 0, Fault: &charger.Fault{Code: "InternalError"}},
			{ID: 1, Fault: &charger.Fault{Code: "GroundFailure", Info: "RCD tripped", VendorCode: "E42"}},
		}},
		// A connector listed with nothing is declared to have nothing.
		"FREE-1":  {Connectors: []charger.DeclaredConnector{{ID: 1}}},
		"GONE-1":  {Connection: charger.Offline},
		"SPARE-1": {Connection: charger.Stopped},
		// No state block, or an empty one, declares nothing.
		"PLAIN-1": nil,
		"BLANK-1": nil,
	}
	want["BAY-2"] = want["BAY-1"]
	for id, w := range want {
		if got := declared[id]; !reflect.DeepEqual(got, w) {
			t.Errorf("%s: declared state = %+v, want %+v", id, got, w)
		}
	}
}

func TestParseRejectsInvalidDeclaredState(t *testing.T) {
	state := func(body string) string {
		return "name: s\ncsms: { url: ws://localhost/ocpp }\nevs: { compact: { battery: 40kWh } }\n" +
			"chargers:\n  e: { model: ./fast.Chargerfile, id: A, state: { " + body + " } }\n"
	}
	cases := []struct {
		name    string
		yaml    string
		wantErr string
	}{
		{"unknown connection", state("connection: away"), `chargers.e: state: connection: "away" is not understood; use offline or stopped`},
		{"online", state("connection: online"), `connection: "online" is not understood`},
		{"unknown state field", state("power: off"), "power"},
		{"unknown connector field", state("connectors: { 1: { car: compact } }"), "car"},
		{"connector the model lacks", state("connectors: { 2: {} }"), "chargers.e: state: connectors.2: the model has no connector 2; it has 1"},
		{"connector that is not a number", state("connectors: { one: {} }"), "chargers.e: state"},
		{"unknown EV preset", state("connectors: { 1: { ev: ghost } }"), `chargers.e: state: connectors.1: ev: there is no EV preset named "ghost"`},
		{"EV on the charger as a whole", state("connectors: { 0: { ev: compact } }"), "connectors.0: ev: connector 0 is the charger as a whole"},
		{"unknown error code", state("connectors: { 1: { fault: Boom } }"), `connectors.1: fault: unknown error code "Boom"`},
		{"unknown fault field", state("connectors: { 1: { fault: { code: GroundFailure, colour: red } } }"), "colour"},
		{"fault without a code", state("connectors: { 1: { fault: { info: hot } } }"), `connectors.1: fault: unknown error code ""`},
		{"fault that is a list", state("connectors: { 1: { fault: [GroundFailure] } }"), "connectors.1: fault: want an error code"},
		{"connector that is a word", state("connectors: { 1: compact }"), "chargers.e: state: connectors.1: want a block, such as { ev: compact }"},
		{"state that is a word", "name: s\ncsms: { url: ws://localhost/ocpp }\nchargers:\n  e: { model: ./fast.Chargerfile, id: A, state: offline }\n", "chargers.e: state: want a block, such as { connection: offline }"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parse(t, tc.yaml, nil)
			if err == nil {
				t.Fatal("Parse succeeded, want an error")
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error = %q, want it to contain %q", err, tc.wantErr)
			}
		})
	}
}

func TestParseReportsEveryProblemOfADeclaredState(t *testing.T) {
	_, err := parse(t, `
name: s
csms: { url: ws://localhost/ocpp }
chargers:
  e:
    model: ./fast.Chargerfile
    id: A
    state:
      connection: away
      connectors:
        1: { ev: ghost, fault: Boom }
        7: {}
`, nil)
	if err == nil {
		t.Fatal("Parse succeeded, want an error")
	}
	for _, want := range []string{`connection: "away"`, `no EV preset named "ghost"`, `unknown error code "Boom"`, "no connector 7"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not mention %q:\n%v", want, err)
		}
	}
}

func TestDeclaredEVRenewal(t *testing.T) {
	site := mustParse(t, `
name: s
csms: { url: ws://localhost/ocpp }
evs: { compact: { battery: 40kWh } }
chargers:
  lobby:
    model: ./ac.Chargerfile
    id: LOBBY-1
    state: { connectors: { 1: { ev: compact, renew: 1m30s } } }
`, nil)
	k := site.Chargers()[0].Declared.Connectors[0]
	if k.EV == nil || k.EV.Preset != "compact" || k.EV.Renew != 90*time.Second {
		t.Errorf("declared EV = %+v, want compact, renewed after 1m30s", k.EV)
	}

	entry := func(connector string) string {
		return "name: s\ncsms: { url: ws://localhost/ocpp }\nevs: { compact: { battery: 40kWh } }\n" +
			"chargers:\n  e: { model: ./ac.Chargerfile, id: A, state: { connectors: { 1: { " + connector + " } } } }\n"
	}
	for name, tc := range map[string]struct{ yaml, wantErr string }{
		"renew with no EV":      {entry("renew: 5m"), "chargers.e: state: connectors.1: renew: there is no EV to renew; give ev as well"},
		"a time with no unit":   {entry("ev: compact, renew: 5"), `connectors.1: renew: "5" is not a time; write it as 30s, 5m or 1h30m`},
		"a time of nothing":     {entry("ev: compact, renew: 0s"), "connectors.1: renew: the time must be above 0"},
		"a time before now":     {entry("ev: compact, renew: -5m"), "connectors.1: renew: the time must be above 0"},
		"renew beside a fault":  {entry("fault: GroundFailure, renew: 5m"), "renew: there is no EV to renew"},
		"renew that is a block": {entry("ev: compact, renew: { after: 5m }"), "chargers.e: state: connectors.1"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := parse(t, tc.yaml, nil)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error = %v, want it to contain %q", err, tc.wantErr)
			}
		})
	}
}
