package ev

import (
	"errors"
	"fmt"
	"sort"
	"strings"
)

// Point is one point of a charge curve: at state of charge SoC (0 to 1) the
// EV accepts Fraction (above 0, at most 1) of its maximum power.
type Point struct {
	SoC      float64 `json:"soc"`
	Fraction float64 `json:"fraction"`
}

// Curve is a charge curve: how much of its maximum power an EV accepts at
// each state of charge. Between points the value changes in a straight line.
// The zero Curve is the standard curve.
type Curve struct {
	// Name is the name of a built-in curve, or empty for a custom one.
	Name   string  `json:"name,omitempty"`
	Points []Point `json:"points,omitempty"`
}

// Names of the built-in curves.
const (
	CurveStandard = "standard"
	CurveDCFast   = "dc-fast"
)

var builtinCurves = map[string][]Point{
	// Full power up to 80%, then a straight fall to 10% of it at 100%.
	CurveStandard: {{0, 1}, {0.80, 1}, {1, 0.10}},
	// The shape of DC fast charging: a ramp, a plateau, a taper from 50%,
	// and a slow top-off above 80%.
	CurveDCFast: {{0, 0.30}, {0.10, 1}, {0.50, 1}, {0.80, 0.28}, {1, 0.07}},
}

// BuiltinCurveNames returns the names of the built-in curves, sorted.
func BuiltinCurveNames() []string {
	names := make([]string, 0, len(builtinCurves))
	for name := range builtinCurves {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// BuiltinCurve returns the built-in curve with the given name.
func BuiltinCurve(name string) (Curve, error) {
	points, ok := builtinCurves[name]
	if !ok {
		return Curve{}, fmt.Errorf("unknown charge curve %q: the built-in curves are %s", name, strings.Join(BuiltinCurveNames(), " and "))
	}
	return Curve{Name: name, Points: points}, nil
}

// NewCurve returns a custom curve through the given points.
func NewCurve(points []Point) (Curve, error) {
	c := Curve{Points: points}
	if err := c.Validate(); err != nil {
		return Curve{}, err
	}
	return c, nil
}

// Validate reports the first problem with the curve, if any. A curve must
// cover 0% to 100% and may never reach zero, or an EV could stop gaining
// charge before its target and stay there forever.
func (c Curve) Validate() error {
	if len(c.Points) == 0 {
		return nil // the standard curve
	}
	if len(c.Points) < 2 {
		return errors.New("a charge curve needs at least two points")
	}
	if c.Points[0].SoC != 0 {
		return errors.New("a charge curve must start at 0% state of charge")
	}
	if c.Points[len(c.Points)-1].SoC != 1 {
		return errors.New("a charge curve must end at 100% state of charge")
	}
	for i, p := range c.Points {
		if p.Fraction <= 0 || p.Fraction > 1 {
			return fmt.Errorf("charge curve point %d: the power must be above 0%% and at most 100%%", i+1)
		}
		if i > 0 && p.SoC <= c.Points[i-1].SoC {
			return fmt.Errorf("charge curve point %d: the state of charge must be higher than the point before it", i+1)
		}
	}
	return nil
}

// At returns the fraction of its maximum power the EV accepts at the given
// state of charge.
func (c Curve) At(soc float64) float64 {
	points := c.Points
	if len(points) == 0 {
		points = builtinCurves[CurveStandard]
	}
	if soc <= points[0].SoC {
		return points[0].Fraction
	}
	for i := 1; i < len(points); i++ {
		a, b := points[i-1], points[i]
		if soc <= b.SoC {
			return a.Fraction + (b.Fraction-a.Fraction)*(soc-a.SoC)/(b.SoC-a.SoC)
		}
	}
	return points[len(points)-1].Fraction
}

// Overrides are EV parameters that may or may not have been given. They are
// what an EV preset holds, and what the flags of a plug carry.
type Overrides struct {
	BatteryWh *float64 `json:"batteryWh,omitempty"`
	SoC       *float64 `json:"soc,omitempty"`
	MaxPowerW *float64 `json:"maxPowerW,omitempty"`
	TargetSoC *float64 `json:"targetSoc,omitempty"`
	Curve     *Curve   `json:"curve,omitempty"`
}

// With returns p with every parameter that o gives replaced.
func (p Params) With(o Overrides) Params {
	if o.BatteryWh != nil {
		p.BatteryWh = *o.BatteryWh
	}
	if o.SoC != nil {
		p.SoC = *o.SoC
	}
	if o.MaxPowerW != nil {
		p.MaxPowerW = *o.MaxPowerW
	}
	if o.TargetSoC != nil {
		p.TargetSoC = *o.TargetSoC
	}
	if o.Curve != nil {
		p.Curve = *o.Curve
	}
	return p
}
