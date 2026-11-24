// Package scenario reads scenario files. A scenario is a file of timed
// actions that plugyard carries out against a running site.
package scenario

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"slices"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/Amraa1/plugyard/internal/charger"
	"github.com/Amraa1/plugyard/internal/control"
	"github.com/Amraa1/plugyard/internal/ev"
	"github.com/Amraa1/plugyard/internal/model"
	"github.com/Amraa1/plugyard/internal/ocpp/v16"
)

// Actions lists what a step can do, in the order help shows them: the
// actions of "plugyard exec", then the commands that power a charger on and
// off.
var Actions = []string{
	control.ActionPlug, control.ActionUnplug, control.ActionTap,
	control.ActionFault, control.ActionClear, control.ActionOffline, control.ActionOnline, control.ActionReboot,
	control.ActionFail,
	control.OpStop, control.OpStart, control.OpRestart, control.OpKill,
}

// Scenario is a scenario file.
type Scenario struct {
	Name string
	// Steps are in time order, which is also the order of the file.
	Steps []Step
}

// Step is one timed action.
type Step struct {
	// Number is the step's position in the scenario, counting from 1.
	Number int
	// At is when the step runs, counted from the start of the run.
	At time.Duration
	// Charger is a charge point ID, a name, or a pattern.
	Charger string
	// Do is the action, one of Actions.
	Do string
	// Request holds the action with its options, for the actions of
	// "plugyard exec". Its list of chargers is filled in when the step runs.
	Request control.ExecRequest
}

// Power reports whether the step powers chargers on or off, as "plugyard
// start", "stop", "restart" and "kill" do.
func (s Step) Power() bool {
	switch s.Do {
	case control.OpStop, control.OpStart, control.OpRestart, control.OpKill:
		return true
	}
	return false
}

// Describe returns the action with its options as it would be typed after
// "plugyard exec <charger>".
func (s Step) Describe() string {
	parts := []string{s.Do}
	req := s.Request
	switch s.Do {
	case control.ActionTap:
		parts = append(parts, req.Tag)
	case control.ActionFault:
		parts = append(parts, req.Fault.Code)
	case control.ActionFail:
		parts = append(parts, req.Fail)
	case control.ActionPlug:
		if req.EVPreset != "" {
			parts = append(parts, "--ev", req.EVPreset)
		}
	case control.ActionOffline:
		if req.For > 0 {
			parts = append(parts, "--for", req.For.String())
		}
	}
	if !s.Power() && !control.WholeCharger(s.Do) && req.Connector != 1 {
		parts = append(parts, "-n", fmt.Sprint(req.Connector))
	}
	return strings.Join(parts, " ")
}

// Duration returns the time of the last step.
func (s *Scenario) Duration() time.Duration {
	if len(s.Steps) == 0 {
		return 0
	}
	return s.Steps[len(s.Steps)-1].At
}

type file struct {
	Name  string     `yaml:"name"`
	Steps []fileStep `yaml:"steps"`
}

type fileStep struct {
	At        *string `yaml:"at"`
	Charger   string  `yaml:"charger"`
	Do        string  `yaml:"do"`
	Connector *int    `yaml:"connector"`

	Tag string `yaml:"tag"`

	EV        string `yaml:"ev"`
	Battery   string `yaml:"battery"`
	SoC       string `yaml:"soc"`
	MaxPower  string `yaml:"maxPower"`
	TargetSoC string `yaml:"targetSoc"`
	Curve     string `yaml:"curve"`

	Code       string `yaml:"code"`
	Info       string `yaml:"info"`
	VendorCode string `yaml:"vendorCode"`

	For string `yaml:"for"`

	Step string `yaml:"step"`
}

// Load reads and validates the scenario file at path.
func Load(path string) (*Scenario, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	s, err := Parse(data)
	if err != nil {
		return nil, fmt.Errorf("%s:\n%w", path, err)
	}
	return s, nil
}

// Parse reads and validates the contents of a scenario file. When the file
// has several problems, the returned error lists all of them.
func Parse(data []byte) (*Scenario, error) {
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

	s := &Scenario{Name: strings.TrimSpace(f.Name)}
	if s.Name == "" {
		problem("name is required")
	}
	if len(f.Steps) == 0 {
		problem("steps: at least one step is required")
	}

	var last time.Duration
	for i, fs := range f.Steps {
		number := i + 1
		step, errs := parseStep(fs)
		for _, err := range errs {
			problem("step %d: %v", number, err)
		}
		step.Number = number
		// One numbering serves the file, the plan and --step: steps are
		// written in the order they run.
		if step.At < last {
			problem("step %d: at %s is earlier than the step before it (%s); write the steps in time order", number, step.At, last)
		} else {
			last = step.At
		}
		s.Steps = append(s.Steps, step)
	}

	if len(problems) > 0 {
		return nil, errors.Join(problems...)
	}
	return s, nil
}

func parseStep(fs fileStep) (Step, []error) {
	var errs []error
	fail := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }

	step := Step{Charger: strings.TrimSpace(fs.Charger), Do: strings.TrimSpace(fs.Do)}
	step.Request.Action = step.Do
	step.Request.Connector = 1

	switch {
	case fs.At == nil:
		fail("at is required: when the step runs, counted from the start, such as 0s, 30s or 10m")
	default:
		at, err := time.ParseDuration(strings.TrimSpace(*fs.At))
		switch {
		case err != nil:
			fail("at %q is not a time such as 0s, 30s or 10m", *fs.At)
		case at < 0:
			fail("at cannot be negative")
		}
		step.At = at
	}

	switch {
	case step.Charger == "":
		fail("charger is required: a charge point ID, a name, or a pattern such as \"FAST-*\"")
	default:
		if _, err := path.Match(step.Charger, ""); err != nil {
			fail("charger %q is not a valid pattern", step.Charger)
		}
	}

	known := slices.Contains(Actions, step.Do)
	switch {
	case step.Do == "":
		fail("do is required: one of %s", strings.Join(Actions, ", "))
	case !known:
		fail("do %q is unknown: want one of %s", step.Do, strings.Join(Actions, ", "))
	}
	if !known {
		return step, errs
	}

	// Each option belongs to some actions only. An option on the wrong
	// action is a mistake worth reporting, not something to ignore.
	given := map[string]bool{
		"tag": fs.Tag != "", "ev": fs.EV != "", "battery": fs.Battery != "", "soc": fs.SoC != "",
		"maxPower": fs.MaxPower != "", "targetSoc": fs.TargetSoC != "", "curve": fs.Curve != "",
		"code": fs.Code != "", "info": fs.Info != "", "vendorCode": fs.VendorCode != "",
		"for": fs.For != "", "connector": fs.Connector != nil, "step": fs.Step != "",
	}
	allowed := map[string][]string{
		control.ActionPlug:    {"connector", "ev", "battery", "soc", "maxPower", "targetSoc", "curve"},
		control.ActionUnplug:  {"connector"},
		control.ActionTap:     {"connector", "tag"},
		control.ActionFault:   {"connector", "code", "info", "vendorCode"},
		control.ActionClear:   {"connector"},
		control.ActionOffline: {"for"},
		control.ActionFail:    {"step"},
	}[step.Do]
	for _, option := range []string{"connector", "tag", "ev", "battery", "soc", "maxPower", "targetSoc", "curve", "code", "info", "vendorCode", "for", "step"} {
		if given[option] && !slices.Contains(allowed, option) {
			fail("%s does not apply to %s", option, step.Do)
		}
	}

	if fs.Connector != nil && slices.Contains(allowed, "connector") {
		wholeChargerAllowed := step.Do == control.ActionFault || step.Do == control.ActionClear
		switch n := *fs.Connector; {
		case n < 0, n == 0 && !wholeChargerAllowed:
			fail("connector must be 1 or more")
		default:
			step.Request.Connector = n
		}
	}

	switch step.Do {
	case control.ActionTap:
		tag := strings.TrimSpace(fs.Tag)
		switch {
		case tag == "":
			fail("tag is required for tap")
		case len(tag) > 20:
			fail("tag is longer than the 20 characters OCPP 1.6 allows")
		}
		step.Request.Tag = tag

	case control.ActionFault:
		switch {
		case fs.Code == "":
			fail("code is required for fault: one of %s", strings.Join(v16.ErrorCodes, ", "))
		case !slices.Contains(v16.ErrorCodes, fs.Code):
			fail("code %q is unknown: OCPP 1.6 has %s", fs.Code, strings.Join(v16.ErrorCodes, ", "))
		}
		if len(fs.Info) > 50 || len(fs.VendorCode) > 50 {
			fail("info and vendorCode may each be 50 characters at most")
		}
		step.Request.Fault = charger.Fault{Code: fs.Code, Info: fs.Info, VendorCode: fs.VendorCode}

	case control.ActionFail:
		switch {
		case fs.Step == "":
			fail("step is required for fail: one of %s", strings.Join(charger.Failures, ", "))
		case !slices.Contains(charger.Failures, fs.Step):
			fail("step %q is unknown: want one of %s", fs.Step, strings.Join(charger.Failures, ", "))
		}
		step.Request.Fail = fs.Step

	case control.ActionOffline:
		if fs.For != "" {
			d, err := time.ParseDuration(strings.TrimSpace(fs.For))
			if err != nil || d <= 0 {
				fail("for %q is not a time above zero, such as 30s", fs.For)
			}
			step.Request.For = d
		}

	case control.ActionPlug:
		step.Request.EVPreset = strings.TrimSpace(fs.EV)
		o, err := parseEV(fs)
		if err != nil {
			fail("%v", err)
		}
		step.Request.EV = o
	}
	return step, errs
}

func parseEV(fs fileStep) (ev.Overrides, error) {
	var o ev.Overrides
	if fs.Battery != "" {
		v, err := ev.ParseEnergy(fs.Battery)
		if err != nil {
			return o, fmt.Errorf("battery: %w", err)
		}
		o.BatteryWh = &v
	}
	if fs.SoC != "" {
		v, err := ev.ParsePercent(fs.SoC)
		if err != nil {
			return o, fmt.Errorf("soc: %w", err)
		}
		o.SoC = &v
	}
	if fs.MaxPower != "" {
		p, err := model.ParsePower(fs.MaxPower)
		if err != nil {
			return o, fmt.Errorf("maxPower: %w", err)
		}
		v := float64(p)
		o.MaxPowerW = &v
	}
	if fs.TargetSoC != "" {
		v, err := ev.ParsePercent(fs.TargetSoC)
		if err != nil {
			return o, fmt.Errorf("targetSoc: %w", err)
		}
		o.TargetSoC = &v
	}
	if fs.Curve != "" {
		c, err := ev.BuiltinCurve(fs.Curve)
		if err != nil {
			return o, fmt.Errorf("curve: %w", err)
		}
		o.Curve = &c
	}
	return o, nil
}
