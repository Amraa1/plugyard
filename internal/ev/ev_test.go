package ev

import (
	"math"
	"testing"
	"time"
)

func near(a, b, tolerance float64) bool { return math.Abs(a-b) <= tolerance }

func TestPowerFollowsTheChargeCurve(t *testing.T) {
	cases := []struct {
		soc  float64
		want float64
	}{
		{0.20, 50000}, // full power below 80%
		{0.80, 50000},
		{0.90, 27500}, // halfway down the taper: 55% of full power
		{0.99, 7250},
	}
	for _, tc := range cases {
		e := New(Params{BatteryWh: 60000, SoC: tc.soc, TargetSoC: 1})
		if got := e.Power(50000); !near(got, tc.want, 1) {
			t.Errorf("at %.0f%% power = %.0f W, want %.0f W", tc.soc*100, got, tc.want)
		}
	}
}

func TestPowerIsLimitedByTheEV(t *testing.T) {
	e := New(Params{BatteryWh: 60000, SoC: 0.2, MaxPowerW: 11000, TargetSoC: 1})
	if got := e.Power(50000); got != 11000 {
		t.Errorf("power = %.0f W, want the EV's 11000 W limit", got)
	}
	if got := e.Power(7400); got != 7400 {
		t.Errorf("power = %.0f W, want the connector's 7400 W", got)
	}
}

func TestChargeAddsEnergyWithoutLosses(t *testing.T) {
	e := New(Params{BatteryWh: 60000, SoC: 0.20, TargetSoC: 1})

	energy := e.Charge(50000, 30*time.Minute)

	if !near(energy, 25000, 1) {
		t.Errorf("energy = %.0f Wh, want 25000 Wh for 30 minutes at 50 kW", energy)
	}
	if want := 0.20 + 25000.0/60000; !near(e.SoC(), want, 1e-6) {
		t.Errorf("SoC = %.4f, want %.4f", e.SoC(), want)
	}
}

func TestChargeStopsAtTheTarget(t *testing.T) {
	e := New(Params{BatteryWh: 60000, SoC: 0.20, TargetSoC: 0.80})

	energy := e.Charge(50000, 24*time.Hour)

	if !near(energy, 36000, 1) {
		t.Errorf("energy = %.0f Wh, want 36000 Wh to go from 20%% to 80%%", energy)
	}
	if !e.Full() || !near(e.SoC(), 0.80, 1e-9) {
		t.Errorf("SoC = %.4f, full = %v; want 0.80 and full", e.SoC(), e.Full())
	}
	if got := e.Power(50000); got != 0 {
		t.Errorf("a full EV draws %.0f W, want 0", got)
	}
	if more := e.Charge(50000, time.Hour); more != 0 {
		t.Errorf("a full EV took %.0f Wh more, want 0", more)
	}
}

func TestChargeTapersAbove80Percent(t *testing.T) {
	// From 80% to 100% of 60 kWh is 12 kWh. At a constant 50 kW that takes
	// 14.4 minutes; with the taper it must take clearly longer.
	e := New(Params{BatteryWh: 60000, SoC: 0.80, TargetSoC: 1})

	energy := e.Charge(50000, 14*time.Minute+24*time.Second)

	if e.Full() {
		t.Error("EV is full after 14.4 minutes, want the taper to slow it down")
	}
	if energy >= 12000 {
		t.Errorf("energy = %.0f Wh, want less than 12000 Wh", energy)
	}

	total := energy + e.Charge(50000, 3*time.Hour)
	if !e.Full() || !near(total, 12000, 1) {
		t.Errorf("after 3 more hours: full = %v, total = %.0f Wh; want full with 12000 Wh", e.Full(), total)
	}
}

func TestChargeGivesTheSameResultInOneStepOrMany(t *testing.T) {
	one := New(Params{BatteryWh: 60000, SoC: 0.70, TargetSoC: 1})
	many := New(Params{BatteryWh: 60000, SoC: 0.70, TargetSoC: 1})

	a := one.Charge(50000, 20*time.Minute)
	b := 0.0
	for range 1200 {
		b += many.Charge(50000, time.Second)
	}

	if !near(a, b, a*0.01) {
		t.Errorf("one step gave %.0f Wh, many steps gave %.0f Wh; want them within 1%%", a, b)
	}
}

func TestValidate(t *testing.T) {
	if err := Defaults().Validate(); err != nil {
		t.Errorf("Defaults are invalid: %v", err)
	}
	bad := []Params{
		{BatteryWh: 0, SoC: 0.2, TargetSoC: 1},
		{BatteryWh: 60000, SoC: 1.2, TargetSoC: 1},
		{BatteryWh: 60000, SoC: 0.2, TargetSoC: 0},
		{BatteryWh: 60000, SoC: 0.2, TargetSoC: 1, MaxPowerW: -1},
	}
	for _, p := range bad {
		if p.Validate() == nil {
			t.Errorf("%+v is accepted, want an error", p)
		}
	}
}

func TestParseEnergyAndPercent(t *testing.T) {
	if got, err := ParseEnergy("60kWh"); err != nil || got != 60000 {
		t.Errorf("ParseEnergy(60kWh) = %v, %v", got, err)
	}
	if got, err := ParseEnergy("500 Wh"); err != nil || got != 500 {
		t.Errorf("ParseEnergy(500 Wh) = %v, %v", got, err)
	}
	if _, err := ParseEnergy("60"); err == nil {
		t.Error("ParseEnergy(60) succeeded, want an error for the missing unit")
	}
	if got, err := ParsePercent("20%"); err != nil || got != 0.2 {
		t.Errorf("ParsePercent(20%%) = %v, %v", got, err)
	}
	if got, err := ParsePercent("80"); err != nil || got != 0.8 {
		t.Errorf("ParsePercent(80) = %v, %v", got, err)
	}
	if _, err := ParsePercent("120%"); err == nil {
		t.Error("ParsePercent(120%) succeeded, want an error")
	}
}

func TestCurveAt(t *testing.T) {
	dc, err := BuiltinCurve(CurveDCFast)
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct{ soc, want float64 }{
		{0, 0.30}, {0.05, 0.65}, {0.10, 1}, {0.30, 1}, {0.50, 1},
		{0.65, 0.64}, {0.80, 0.28}, {0.90, 0.175}, {1, 0.07},
	}
	for _, tc := range cases {
		if got := dc.At(tc.soc); !near(got, tc.want, 1e-9) {
			t.Errorf("dc-fast at %.0f%% = %.3f, want %.3f", tc.soc*100, got, tc.want)
		}
	}

	// The zero Curve is the standard curve.
	var zero Curve
	if zero.At(0.5) != 1 || !near(zero.At(0.9), 0.55, 1e-9) {
		t.Errorf("zero curve at 50%% and 90%% = %.2f and %.2f, want 1 and 0.55", zero.At(0.5), zero.At(0.9))
	}

	if _, err := BuiltinCurve("warp"); err == nil {
		t.Error("BuiltinCurve(warp) succeeded, want an error")
	}
}

func TestCurveBelongsToTheEV(t *testing.T) {
	dc, _ := BuiltinCurve(CurveDCFast)

	// A 230 kW EV on a 350 kW connector shows its whole curve.
	fast := func(soc float64) *EV {
		return New(Params{BatteryWh: 82000, SoC: soc, MaxPowerW: 230000, TargetSoC: 1, Curve: dc})
	}
	if got := fast(0.30).Power(350000); got != 230000 {
		t.Errorf("at 30%% on a 350 kW connector: %.0f W, want the EV's 230000", got)
	}
	if got := fast(0.80).Power(350000); !near(got, 64400, 1) {
		t.Errorf("at 80%% on a 350 kW connector: %.0f W, want 64400", got)
	}

	// The same EV on a 50 kW connector stays at 50 kW while its curve is
	// above that, and follows the curve once it drops below.
	if got := fast(0.80).Power(50000); got != 50000 {
		t.Errorf("at 80%% on a 50 kW connector: %.0f W, want the connector's 50000", got)
	}
	if got := fast(0.95).Power(50000); !near(got, 230000*0.1225, 1) {
		t.Errorf("at 95%% on a 50 kW connector: %.0f W, want %.0f", got, 230000*0.1225)
	}

	// With no maximum of its own, the EV takes the connector's.
	open := New(Params{BatteryWh: 60000, SoC: 0, TargetSoC: 1, Curve: dc})
	if got := open.Power(100000); !near(got, 30000, 1) {
		t.Errorf("no EV maximum, at 0%% on a 100 kW connector: %.0f W, want 30000", got)
	}
}

func TestCurveValidation(t *testing.T) {
	bad := map[string][]Point{
		"one point":           {{0, 1}},
		"does not start at 0": {{0.1, 1}, {1, 0.5}},
		"does not end at 100": {{0, 1}, {0.9, 0.5}},
		"reaches zero":        {{0, 1}, {1, 0}},
		"above 100% power":    {{0, 1.2}, {1, 0.5}},
		"goes backwards":      {{0, 1}, {0.5, 1}, {0.4, 0.8}, {1, 0.1}},
	}
	for name, points := range bad {
		if _, err := NewCurve(points); err == nil {
			t.Errorf("%s: NewCurve succeeded, want an error", name)
		}
	}
	if _, err := NewCurve([]Point{{0, 0.5}, {1, 0.5}}); err != nil {
		t.Errorf("a valid curve was rejected: %v", err)
	}
	for _, name := range BuiltinCurveNames() {
		c, _ := BuiltinCurve(name)
		if err := c.Validate(); err != nil {
			t.Errorf("built-in curve %s is invalid: %v", name, err)
		}
	}
}

func TestWithAppliesOnlyWhatWasGiven(t *testing.T) {
	soc, battery := 0.5, 82000.0
	dc, _ := BuiltinCurve(CurveDCFast)

	got := Defaults().With(Overrides{SoC: &soc}).With(Overrides{BatteryWh: &battery, Curve: &dc})

	if got.SoC != 0.5 || got.BatteryWh != 82000 || got.TargetSoC != 1 || got.Curve.Name != CurveDCFast {
		t.Errorf("With = %+v", got)
	}
}

func TestALimitCapsThePowerAndLeavesTheCurveAlone(t *testing.T) {
	// An EV with no maximum of its own, at 90%: the standard curve gives it
	// 55% of the connector's 50 kW.
	p := Defaults()
	p.SoC = 0.90
	e := New(p)
	if got := e.PowerWithin(50000, 50000); math.Abs(got-27500) > 1 {
		t.Fatalf("power with no limit = %v, want 27500", got)
	}
	// A limit above that changes nothing; one below it is what the EV gets.
	if got := e.PowerWithin(50000, 40000); math.Abs(got-27500) > 1 {
		t.Errorf("power under a 40 kW limit = %v, want 27500", got)
	}
	if got := e.PowerWithin(50000, 10000); got != 10000 {
		t.Errorf("power under a 10 kW limit = %v, want 10000", got)
	}
	if got := e.PowerWithin(50000, 0); got != 0 {
		t.Errorf("power under a limit of zero = %v, want 0", got)
	}
	if got := e.ChargeWithin(50000, 10000, time.Minute); math.Abs(got-10000.0/60) > 0.01 {
		t.Errorf("a minute under a 10 kW limit gave %v Wh, want %v", got, 10000.0/60)
	}
}
