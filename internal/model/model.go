// Package model reads Chargerfiles. A Chargerfile declares one model: a kind
// of charger that many chargers can be instantiated from.
package model

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"

	"go.yaml.in/yaml/v3"
)

// Behavior settings accepted in a Chargerfile.
const (
	OnEvFullStop    = "stop"
	OnEvFullSuspend = "suspend"

	StartOnTap  = "tap"
	StartOnPlug = "plug"
)

// Field length limits that OCPP 1.6 places on BootNotification.
const (
	maxVendorLen   = 20
	maxModelLen    = 20
	maxFirmwareLen = 50
	maxIDTagLen    = 20
)

// Model is a kind of charger, as declared by one Chargerfile.
type Model struct {
	Vendor     string
	Name       string
	Firmware   string
	Connectors []Connector
	// Config holds the default values of OCPP configuration keys.
	Config   map[string]string
	OnEvFull string // OnEvFullStop or OnEvFullSuspend
	StartOn  string // StartOnTap or StartOnPlug
	StartTag string // idTag used when StartOn is StartOnPlug
	// DataTransfer lists the vendor IDs whose DataTransfer requests from the
	// CSMS the charger accepts.
	DataTransfer []string
	// Voltage and Phases turn a limit a charging profile gives in amps into
	// power: amps x Voltage x Phases. Zero means the default.
	Voltage float64
	Phases  int
}

// The electrical supply assumed when a Chargerfile does not say.
const (
	DefaultVoltage = 230
	DefaultPhases  = 3
)

// Supply returns the voltage and the number of phases of the charger's
// supply.
func (m *Model) Supply() (voltage float64, phases int) {
	voltage, phases = m.Voltage, m.Phases
	if voltage <= 0 {
		voltage = DefaultVoltage
	}
	if phases <= 0 {
		phases = DefaultPhases
	}
	return voltage, phases
}

// Connector is a numbered outlet on a charger.
type Connector struct {
	ID       int
	Type     string
	MaxPower Power
}

// Power is an amount of electrical power in watts.
type Power float64

func (p Power) String() string {
	switch {
	case p >= 1e6:
		return strconv.FormatFloat(float64(p)/1e6, 'f', -1, 64) + "MW"
	case p >= 1e3:
		return strconv.FormatFloat(float64(p)/1e3, 'f', -1, 64) + "kW"
	default:
		return strconv.FormatFloat(float64(p), 'f', -1, 64) + "W"
	}
}

// ParsePower reads a power value such as "50kW", "7.4 kW" or "3600W".
func ParsePower(s string) (Power, error) {
	text := strings.TrimSpace(s)
	lower := strings.ToLower(text)

	// Longest suffixes first, so that "kw" is not read as "w".
	units := []struct {
		suffix string
		factor float64
	}{{"mw", 1e6}, {"kw", 1e3}, {"w", 1}}

	for _, u := range units {
		number, ok := strings.CutSuffix(lower, u.suffix)
		if !ok {
			continue
		}
		value, err := strconv.ParseFloat(strings.TrimSpace(number), 64)
		if err != nil || value <= 0 {
			break
		}
		return Power(value * u.factor), nil
	}
	return 0, fmt.Errorf("invalid power %q: want a positive number with a unit, such as 50kW", s)
}

// file mirrors the YAML layout of a Chargerfile.
type file struct {
	Vendor     string          `yaml:"vendor"`
	Model      string          `yaml:"model"`
	Firmware   string          `yaml:"firmware"`
	Connectors []fileConnector `yaml:"connectors"`
	Config     map[string]any  `yaml:"config"`
	OnEvFull   string          `yaml:"onEvFull"`
	StartOn    string          `yaml:"startOn"`
	StartTag   string          `yaml:"startTag"`
	// DataTransfer lists vendor IDs.
	DataTransfer []string `yaml:"dataTransfer"`
	Voltage      *float64 `yaml:"voltage"`
	Phases       *int     `yaml:"phases"`
}

type fileConnector struct {
	ID       int    `yaml:"id"`
	Type     string `yaml:"type"`
	MaxPower string `yaml:"maxPower"`
}

// Load reads and validates the Chargerfile at path.
func Load(path string) (*Model, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	m, err := Parse(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return m, nil
}

// Parse reads and validates the contents of a Chargerfile. When the file has
// several problems, the returned error lists all of them.
func Parse(data []byte) (*Model, error) {
	var f file
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&f); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, errors.New("the file is empty")
		}
		return nil, err
	}

	var problems []error
	problem := func(format string, args ...any) {
		problems = append(problems, fmt.Errorf(format, args...))
	}

	m := &Model{
		Vendor:   strings.TrimSpace(f.Vendor),
		Name:     strings.TrimSpace(f.Model),
		Firmware: strings.TrimSpace(f.Firmware),
		Config:   map[string]string{},
		OnEvFull: f.OnEvFull,
		StartOn:  f.StartOn,
		StartTag: f.StartTag,
	}
	for i, vendor := range f.DataTransfer {
		if vendor = strings.TrimSpace(vendor); vendor == "" {
			problem("dataTransfer[%d]: the vendor ID is empty", i)
		} else {
			m.DataTransfer = append(m.DataTransfer, vendor)
		}
	}

	m.Voltage, m.Phases = DefaultVoltage, DefaultPhases
	if f.Voltage != nil {
		if m.Voltage = *f.Voltage; m.Voltage <= 0 {
			problem("voltage is %v, want a number of volts above zero, such as 230", *f.Voltage)
		}
	}
	if f.Phases != nil {
		if m.Phases = *f.Phases; m.Phases < 1 || m.Phases > 3 {
			problem("phases is %d, want 1, 2 or 3", *f.Phases)
		}
	}

	requireText := func(field, value string, max int) {
		switch {
		case value == "":
			problem("%s is required", field)
		case len(value) > max:
			problem("%s is longer than the %d characters OCPP 1.6 allows", field, max)
		}
	}
	requireText("vendor", m.Vendor, maxVendorLen)
	requireText("model", m.Name, maxModelLen)
	requireText("firmware", m.Firmware, maxFirmwareLen)

	if len(f.Connectors) == 0 {
		problem("connectors: at least one connector is required")
	}
	for i, fc := range f.Connectors {
		// OCPP 1.6 numbers connectors from 1 with no gaps.
		if fc.ID != i+1 {
			problem("connectors[%d]: id is %d, want %d (connectors are numbered from 1 in order)", i, fc.ID, i+1)
		}
		if strings.TrimSpace(fc.Type) == "" {
			problem("connectors[%d]: type is required", i)
		}
		power, err := ParsePower(fc.MaxPower)
		if fc.MaxPower == "" {
			problem("connectors[%d]: maxPower is required", i)
		} else if err != nil {
			problem("connectors[%d]: maxPower: %v", i, err)
		}
		m.Connectors = append(m.Connectors, Connector{ID: fc.ID, Type: strings.TrimSpace(fc.Type), MaxPower: power})
	}

	keys := make([]string, 0, len(f.Config))
	for key := range f.Config {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		switch v := f.Config[key].(type) {
		case string:
			m.Config[key] = v
		case int, int64, uint64, float64, bool:
			m.Config[key] = fmt.Sprint(v)
		default:
			problem("config.%s: want a single value such as a number, true/false or text", key)
		}
	}

	switch m.OnEvFull {
	case "":
		m.OnEvFull = OnEvFullStop
	case OnEvFullStop, OnEvFullSuspend:
	default:
		problem("onEvFull is %q, want %q or %q", m.OnEvFull, OnEvFullStop, OnEvFullSuspend)
	}

	switch m.StartOn {
	case "":
		m.StartOn = StartOnTap
	case StartOnTap, StartOnPlug:
	default:
		problem("startOn is %q, want %q or %q", m.StartOn, StartOnTap, StartOnPlug)
	}

	if m.StartOn == StartOnPlug && m.StartTag == "" {
		problem("startTag is required when startOn is %q", StartOnPlug)
	}
	if len(m.StartTag) > maxIDTagLen {
		problem("startTag is longer than the %d characters OCPP 1.6 allows", maxIDTagLen)
	}

	if len(problems) > 0 {
		return nil, errors.Join(problems...)
	}
	return m, nil
}
