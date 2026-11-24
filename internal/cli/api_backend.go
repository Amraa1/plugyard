package cli

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/Amraa1/plugyard/internal/api"
	"github.com/Amraa1/plugyard/internal/charger"
	"github.com/Amraa1/plugyard/internal/control"
	"github.com/Amraa1/plugyard/internal/ev"
	"github.com/Amraa1/plugyard/internal/model"
	"github.com/Amraa1/plugyard/internal/ocpp/v16"
)

// apiBackend does for the API what the commands do for a terminal, with
// the same code, so that a call and the command of the same name cannot
// drift apart.
type apiBackend struct{}

// apiError turns what a command would print into what the API answers.
func apiError(err error) error {
	var match *matchError
	if errors.As(err, &match) {
		switch match.kind {
		case matchStopped:
			return api.Errorf(http.StatusConflict, "%s", match.text)
		case matchAmbiguous:
			// The command line names the site with -s; a call does it
			// in its address.
			text := strings.Replace(match.text, "choose one with -s", "choose one with ?site=<name>", 1)
			return api.Errorf(http.StatusConflict, "%s", text)
		default:
			return api.Errorf(http.StatusNotFound, "%s", match.text)
		}
	}
	return err
}

func (apiBackend) Chargers(ctx context.Context, site string) ([]control.ChargerState, error) {
	w, err := loadWorld(ctx, site)
	if err != nil {
		return nil, apiError(err)
	}
	defer w.close()
	var chargers []control.ChargerState
	for _, t := range w.targets {
		chargers = append(chargers, t.state)
	}
	return chargers, nil
}

func (apiBackend) Inspect(ctx context.Context, site, name string) (any, error) {
	if isPattern(name) {
		return nil, api.Errorf(http.StatusBadRequest, "this shows one charger, so %q cannot be a pattern", name)
	}
	view, err := inspectOne(ctx, site, name)
	if err != nil {
		return nil, apiError(err)
	}
	return view, nil
}

// apiOptions says which options an action of the API takes.
var apiOptions = map[string][]string{
	control.ActionPlug:    {"connector", "ev", "battery", "soc", "maxPower", "targetSoc", "curve"},
	control.ActionUnplug:  {"connector"},
	control.ActionTap:     {"connector", "tag"},
	control.ActionFault:   {"connector", "code", "info", "vendorCode"},
	control.ActionClear:   {"connector"},
	control.ActionOffline: {"for"},
	control.ActionOnline:  {},
	control.ActionReboot:  {},
	control.ActionFail:    {"step"},
}

// execRequest checks the options of an action and turns them into what a
// site is asked to do.
func execRequest(a api.Action) (control.ExecRequest, error) {
	req := control.ExecRequest{Action: a.Name, Connector: 1}
	bad := func(format string, args ...any) (control.ExecRequest, error) {
		return req, api.Errorf(http.StatusBadRequest, format, args...)
	}

	given := map[string]bool{
		"connector": a.Connector != nil, "tag": a.Tag != "", "code": a.Code != "", "info": a.Info != "",
		"vendorCode": a.VendorCode != "", "for": a.For != "", "step": a.Step != "", "ev": a.EV != "",
		"battery": a.Battery != "", "soc": a.SoC != "", "maxPower": a.MaxPower != "", "targetSoc": a.TargetSoC != "", "curve": a.Curve != "",
	}
	takes := apiOptions[a.Name]
	for option, set := range given {
		if set && !slices.Contains(takes, option) {
			if len(takes) == 0 {
				return bad("%s takes no options, and %q was given", a.Name, option)
			}
			return bad("%s does not take %q: it takes %s", a.Name, option, strings.Join(takes, ", "))
		}
	}

	if a.Connector != nil {
		whole := a.Name == control.ActionFault || a.Name == control.ActionClear
		if *a.Connector < 0 || (*a.Connector == 0 && !whole) {
			return bad("the connector number must be 1 or more")
		}
		req.Connector = *a.Connector
	}

	switch a.Name {
	case control.ActionPlug:
		var params ev.Overrides
		option := func(name, value string, parse func(string) (float64, error), into **float64) error {
			if value == "" {
				return nil
			}
			v, err := parse(value)
			if err != nil {
				return api.Errorf(http.StatusBadRequest, "%s: %v", name, err)
			}
			*into = &v
			return nil
		}
		power := func(s string) (float64, error) {
			p, err := model.ParsePower(s)
			return float64(p), err
		}
		for _, err := range []error{
			option("battery", a.Battery, ev.ParseEnergy, &params.BatteryWh),
			option("soc", a.SoC, ev.ParsePercent, &params.SoC),
			option("maxPower", a.MaxPower, power, &params.MaxPowerW),
			option("targetSoc", a.TargetSoC, ev.ParsePercent, &params.TargetSoC),
		} {
			if err != nil {
				return req, err
			}
		}
		if a.Curve != "" {
			curve, err := ev.BuiltinCurve(a.Curve)
			if err != nil {
				return bad("curve: %v", err)
			}
			params.Curve = &curve
		}
		req.EV, req.EVPreset = params, a.EV
	case control.ActionTap:
		if a.Tag == "" {
			return bad("tap needs \"tag\", the idTag that is presented")
		}
		req.Tag = a.Tag
	case control.ActionFault:
		if !slices.Contains(v16.ErrorCodes, a.Code) {
			return bad("fault needs \"code\", one of the OCPP 1.6 error codes: %s", strings.Join(v16.ErrorCodes, ", "))
		}
		req.Fault = charger.Fault{Code: a.Code, Info: a.Info, VendorCode: a.VendorCode}
	case control.ActionOffline:
		if a.For != "" {
			d, err := time.ParseDuration(a.For)
			if err != nil || d <= 0 {
				return bad("for must be a time above zero, such as 30s")
			}
			req.For = d
		}
	case control.ActionFail:
		if !slices.Contains(charger.Failures, a.Step) {
			return bad("fail needs \"step\", one of %s", strings.Join(charger.Failures, ", "))
		}
		req.Fail = a.Step
	}
	return req, nil
}

func (apiBackend) Exec(ctx context.Context, site, pattern string, action api.Action) ([]control.ExecResult, error) {
	if err := checkPattern(pattern); err != nil {
		return nil, api.Errorf(http.StatusBadRequest, "%v", err)
	}
	req, err := execRequest(action)
	if err != nil {
		return nil, err
	}
	w, err := loadWorld(ctx, site)
	if err != nil {
		return nil, apiError(err)
	}
	defer w.close()
	results, err := execOn(ctx, w, pattern, req)
	if err != nil {
		return nil, apiError(err)
	}
	return results, nil
}

// apiRunningError says that the API has to be stopped before the program
// is replaced or removed, or nil when none is running.
func apiRunningError(ctx context.Context, dataDir, then string) error {
	info, err := api.Probe(ctx, dataDir)
	if err != nil {
		return nil
	}
	return fmt.Errorf("the API is running at %s; stop it first with \"plugyard api down\", then %s", info.URL(), then)
}
