package site

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/Amraa1/plugyard/internal/ev"
)

func ptr(v float64) *float64 { return &v }

func TestEVParamsPrecedence(t *testing.T) {
	dc, _ := ev.BuiltinCurve(ev.CurveDCFast)
	s := New("depot")
	s.SetPresets(map[string]ev.Overrides{
		"longrange": {BatteryWh: ptr(82000), SoC: ptr(0.10), Curve: &dc},
		"compact":   {BatteryWh: ptr(40000), MaxPowerW: ptr(50000)},
	})
	s.SetDefaultEV("FAST-1", "longrange")

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
