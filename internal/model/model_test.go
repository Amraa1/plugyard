package model

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// The example from docs/SPEC.md section 5.
const specExample = `
vendor: Acme
model: FastOne 50
firmware: 1.4.2
connectors:
  - { id: 1, type: CCS2, maxPower: 50kW }
  - { id: 2, type: CHAdeMO, maxPower: 50kW }
config:
  HeartbeatInterval: 300
  MeterValueSampleInterval: 60
onEvFull: stop
startOn: tap
startTag: FREEVEND
`

const minimal = `
vendor: Acme
model: Wallbox
firmware: "1.0"
connectors:
  - { id: 1, type: Type2, maxPower: 7.4kW }
`

func TestParseSpecExample(t *testing.T) {
	got, err := Parse([]byte(specExample))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	want := &Model{
		Vendor:   "Acme",
		Name:     "FastOne 50",
		Firmware: "1.4.2",
		Connectors: []Connector{
			{ID: 1, Type: "CCS2", MaxPower: 50000},
			{ID: 2, Type: "CHAdeMO", MaxPower: 50000},
		},
		Config: map[string]string{
			"HeartbeatInterval":        "300",
			"MeterValueSampleInterval": "60",
		},
		OnEvFull: OnEvFullStop,
		StartOn:  StartOnTap,
		StartTag: "FREEVEND",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Parse =\n%+v\nwant\n%+v", got, want)
	}
}

func TestParseAppliesDefaults(t *testing.T) {
	m, err := Parse([]byte(minimal))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if m.OnEvFull != OnEvFullStop {
		t.Errorf("OnEvFull = %q, want %q", m.OnEvFull, OnEvFullStop)
	}
	if m.StartOn != StartOnTap {
		t.Errorf("StartOn = %q, want %q", m.StartOn, StartOnTap)
	}
	if m.Connectors[0].MaxPower != 7400 {
		t.Errorf("MaxPower = %v, want 7400", float64(m.Connectors[0].MaxPower))
	}
}

func TestParseRejectsInvalidFiles(t *testing.T) {
	cases := []struct {
		name    string
		yaml    string
		wantErr string
	}{
		{"empty file", "", "empty"},
		{"missing vendor", strings.Replace(minimal, "vendor: Acme", "", 1), "vendor is required"},
		{"vendor too long", strings.Replace(minimal, "Acme", "A Vendor Name Beyond Twenty", 1), "vendor is longer"},
		{"no connectors", "vendor: A\nmodel: B\nfirmware: C\n", "at least one connector"},
		{"connector ids not from 1", strings.Replace(minimal, "id: 1", "id: 2", 1), "id is 2, want 1"},
		{"power without unit", strings.Replace(minimal, "7.4kW", "50", 1), "invalid power"},
		{"negative power", strings.Replace(minimal, "7.4kW", "-5kW", 1), "invalid power"},
		{"missing connector type", strings.Replace(minimal, "type: Type2, ", "", 1), "type is required"},
		{"unknown field", minimal + "colour: red\n", "colour"},
		{"bad onEvFull", minimal + "onEvFull: explode\n", `onEvFull is "explode"`},
		{"bad startOn", minimal + "startOn: magic\n", `startOn is "magic"`},
		{"plug without tag", minimal + "startOn: plug\n", "startTag is required"},
		{"nested config value", minimal + "config:\n  Key: [1, 2]\n", "config.Key"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse([]byte(tc.yaml))
			if err == nil {
				t.Fatal("Parse succeeded, want an error")
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error = %q, want it to contain %q", err, tc.wantErr)
			}
		})
	}
}

func TestParseReportsEveryProblem(t *testing.T) {
	_, err := Parse([]byte("model: B\nconnectors:\n  - { id: 1, type: CCS2, maxPower: lots }\n"))
	if err == nil {
		t.Fatal("Parse succeeded, want an error")
	}
	for _, want := range []string{"vendor is required", "firmware is required", "invalid power"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not mention %q:\n%v", want, err)
		}
	}
}

func TestLoadNamesTheFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "Chargerfile")
	if err := os.WriteFile(path, []byte("vendor: A\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Load(path)
	if err == nil || !strings.Contains(err.Error(), "Chargerfile") {
		t.Errorf("Load error = %v, want it to name the file", err)
	}
}

func TestParsePower(t *testing.T) {
	cases := map[string]Power{
		"50kW":   50000,
		"7.4 kW": 7400,
		"3600W":  3600,
		"0.35MW": 350000,
		"22KW":   22000,
	}
	for in, want := range cases {
		got, err := ParsePower(in)
		if err != nil {
			t.Errorf("ParsePower(%q): %v", in, err)
		} else if got != want {
			t.Errorf("ParsePower(%q) = %v, want %v", in, float64(got), float64(want))
		}
	}
}

func TestDataTransferVendors(t *testing.T) {
	const base = "vendor: Acme\nmodel: Wallbox\nfirmware: \"1.0\"\nconnectors:\n  - { id: 1, type: Type2, maxPower: 7.4kW }\n"

	m, err := Parse([]byte(base + "dataTransfer: [com.acme, \" com.other \"]\n"))
	if err != nil {
		t.Fatal(err)
	}
	if len(m.DataTransfer) != 2 || m.DataTransfer[0] != "com.acme" || m.DataTransfer[1] != "com.other" {
		t.Errorf("DataTransfer = %q", m.DataTransfer)
	}

	if _, err := Parse([]byte(base + "dataTransfer: [\"\"]\n")); err == nil || !strings.Contains(err.Error(), "dataTransfer[0]: the vendor ID is empty") {
		t.Errorf("an empty vendor ID returned %v", err)
	}
}
