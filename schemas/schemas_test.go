package schemas

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"go.yaml.in/yaml/v3"
)

func compile(t *testing.T, kind string) *jsonschema.Schema {
	t.Helper()
	data, err := Get(kind)
	if err != nil {
		t.Fatal(err)
	}
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("the %s schema is not JSON: %v", kind, err)
	}
	c := jsonschema.NewCompiler()
	if err := c.AddResource(URL(kind), doc); err != nil {
		t.Fatal(err)
	}
	schema, err := c.Compile(URL(kind))
	if err != nil {
		t.Fatalf("the %s schema does not compile: %v", kind, err)
	}
	return schema
}

// check validates YAML against a schema, as an editor would.
func check(t *testing.T, schema *jsonschema.Schema, text []byte) error {
	t.Helper()
	var value any
	if err := yaml.Unmarshal(text, &value); err != nil {
		t.Fatalf("not YAML: %v", err)
	}
	// Through JSON, so that the validator sees JSON's types.
	asJSON, err := json.Marshal(textKeys(value))
	if err != nil {
		t.Fatal(err)
	}
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(asJSON))
	if err != nil {
		t.Fatal(err)
	}
	return schema.Validate(doc)
}

// textKeys turns the keys of every map into text, as JSON has them. YAML
// reads a key such as the connector number 1 as a number.
func textKeys(value any) any {
	switch v := value.(type) {
	case map[string]any:
		for key, item := range v {
			v[key] = textKeys(item)
		}
	case map[any]any:
		out := make(map[string]any, len(v))
		for key, item := range v {
			out[fmt.Sprint(key)] = textKeys(item)
		}
		return out
	case []any:
		for i, item := range v {
			v[i] = textKeys(item)
		}
	}
	return value
}

// The files plugyard ships must pass their own schemas, so that the schemas
// cannot drift away from what the program reads.
func TestShippedFilesFitTheirSchemas(t *testing.T) {
	for file, kind := range map[string]string{
		"../examples/Chargerfile":               "chargerfile",
		"../examples/wallbox.Chargerfile":       "chargerfile",
		"../examples/site.yaml":                 "site",
		"../examples/scenario.yaml":             "scenario",
		"../internal/cli/starter/Chargerfile":   "chargerfile",
		"../internal/cli/starter/site.yaml":     "site",
		"../internal/cli/starter/scenario.yaml": "scenario",
	} {
		text, err := os.ReadFile(filepath.FromSlash(file))
		if err != nil {
			t.Errorf("%s: %v", file, err)
			continue
		}
		if err := check(t, compile(t, kind), text); err != nil {
			t.Errorf("%s does not fit the %s schema:\n%v", file, kind, err)
		}
		// A file that names its schema names the right one.
		if first, _, _ := strings.Cut(string(text), "\n"); strings.Contains(first, "$schema=") && !strings.HasSuffix(strings.TrimSpace(first), "$schema="+URL(kind)) {
			t.Errorf("%s points at another schema: %s", file, first)
		}
	}
}

func TestSchemasAcceptEverySetting(t *testing.T) {
	for kind, text := range map[string]string{
		"chargerfile": `
vendor: Acme
model: FastOne 50
firmware: 1.0
ocpp: 1.6J
connectors:
  - { id: 1, type: CCS2, maxPower: 50kW }
  - { id: 2, type: Type2, maxPower: 7.4 kW }
config: { HeartbeatInterval: 300, MeterValuesSampledData: "SoC", StopTransactionOnInvalidId: true }
dataTransfer: [com.acme]
voltage: 400
phases: 1
onEvFull: suspend
startOn: plug
startTag: 12345678
`,
		"site": `
name: depot
csms: { url: "wss://csms.example.com/ocpp", password: "${CSMS_PASSWORD}", insecureSkipVerify: false }
speed: 60
connectRate: "${RATE:-0}"
log: { maxSize: 100MB, files: 5 }
evs:
  compact: { battery: 40kWh, maxPower: 50kW, soc: 10%, targetSoc: 80, curve: dc-fast }
  custom: { curve: [[0%, 100%], [80%, 100%], [100, 10]] }
chargers:
  fast: { model: ./Chargerfile, count: 20, id: "FAST-{n:03}", start: 5, ev: compact, config: { MeterValueSampleInterval: 10 } }
  odd: { model: ./Chargerfile, ids: [12994897, p025b2036387], csms: { url: "ws://other/ocpp" } }
  lobby: { model: ./wallbox.Chargerfile, id: LOBBY-1, name: front-desk }
  bay:
    model: ./Chargerfile
    id: BAY-1
    state:
      connection: offline
      connectors:
        0: { fault: InternalError }
        1: { ev: compact, fault: { code: GroundFailure, info: RCD tripped, vendorCode: E42 } }
        2: {}
  spare: { model: ./Chargerfile, id: SPARE-1, state: { connection: stopped } }
`,
		"scenario": `
name: everything
steps:
  - { at: 0s, charger: "FAST-*", do: plug, ev: compact, battery: 60kWh, soc: 20%, maxPower: 11kW, targetSoc: 80%, curve: standard, connector: 2 }
  - { at: 5s, charger: FAST-001, do: tap, tag: 12345678 }
  - { at: 1m, charger: front-desk, do: fault, code: WeakSignal, info: far away, vendorCode: E-1, connector: 0 }
  - { at: 2m, charger: front-desk, do: clear, connector: 0 }
  - { at: 3m, charger: "*", do: offline, for: 30s }
  - { at: 4m, charger: "*", do: online }
  - { at: 5m, charger: FAST-001, do: reboot }
  - { at: 6m, charger: FAST-001, do: kill }
  - { at: 7m, charger: FAST-001, do: start }
  - { at: 8m, charger: FAST-001, do: restart }
  - { at: 9m, charger: FAST-001, do: stop }
  - { at: 1h30m, charger: "*", do: unplug }
`,
	} {
		if err := check(t, compile(t, kind), []byte(text)); err != nil {
			t.Errorf("the %s schema rejects a valid file:\n%v", kind, err)
		}
	}
}

func TestSchemasCatchMistakes(t *testing.T) {
	for name, tc := range map[string]struct{ kind, text string }{
		"a misspelled Chargerfile setting":        {"chargerfile", "vendor: A\nmodel: B\nfirmware: C\nconnectors: [{ id: 1, type: CCS2, maxPower: 50kW }]\nonEVFull: stop\n"},
		"a Chargerfile with no connectors":        {"chargerfile", "vendor: A\nmodel: B\nfirmware: C\nconnectors: []\n"},
		"a power with no unit":                    {"chargerfile", "vendor: A\nmodel: B\nfirmware: C\nconnectors: [{ id: 1, type: CCS2, maxPower: \"50\" }]\n"},
		"an OCPP version plugyard does not speak": {"chargerfile", "vendor: A\nmodel: B\nfirmware: C\nconnectors: [{ id: 1, type: CCS2, maxPower: 50kW }]\nocpp: 2.0.1\n"},
		"four phases":                             {"chargerfile", "vendor: A\nmodel: B\nfirmware: C\nconnectors: [{ id: 1, type: CCS2, maxPower: 50kW }]\nphases: 4\n"},
		"a site with no chargers":                 {"site", "name: x\nchargers: {}\n"},
		"a site with no name":                     {"site", "chargers: { a: { model: ./Chargerfile } }\n"},
		"a misspelled site setting":               {"site", "name: x\nchargers: { a: { model: ./Chargerfile, cuont: 3 } }\n"},
		"a charger entry with no model":           {"site", "name: x\nchargers: { a: { count: 3 } }\n"},
		"a declared connection that is not one":   {"site", "name: x\nchargers: { a: { model: ./Chargerfile, state: { connection: online } } }\n"},
		"a misspelled declared state setting":     {"site", "name: x\nchargers: { a: { model: ./Chargerfile, state: { power: stopped } } }\n"},
		"a declared fault with an unknown code":   {"site", "name: x\nchargers: { a: { model: ./Chargerfile, state: { connectors: { 1: { fault: Exploded } } } } }\n"},
		"a declared connector that is no number":  {"site", "name: x\nchargers: { a: { model: ./Chargerfile, state: { connectors: { one: {} } } } }\n"},
		"a step with no time":                     {"scenario", "name: x\nsteps: [{ charger: CP-1, do: plug }]\n"},
		"an unknown action":                       {"scenario", "name: x\nsteps: [{ at: 0s, charger: CP-1, do: explode }]\n"},
		"a tap with no tag":                       {"scenario", "name: x\nsteps: [{ at: 0s, charger: CP-1, do: tap }]\n"},
		"a fault with an unknown code":            {"scenario", "name: x\nsteps: [{ at: 0s, charger: CP-1, do: fault, code: Exploded }]\n"},
		"a scenario with a loop":                  {"scenario", "name: x\nsteps: [{ at: 0s, charger: CP-1, do: plug }]\nloop: 3\n"},
	} {
		if err := check(t, compile(t, tc.kind), []byte(tc.text)); err == nil {
			t.Errorf("the %s schema accepts %s", tc.kind, name)
		}
	}
}

func TestKinds(t *testing.T) {
	if got := strings.Join(Kinds(), " "); got != "chargerfile scenario site" {
		t.Errorf("Kinds = %q", got)
	}
	if _, err := Get("nope"); err == nil {
		t.Error("Get(nope) succeeded")
	}
}
