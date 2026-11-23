// Package ev models the EV on a connector: just enough of a battery to make
// the energy flow of a transaction look real.
package ev

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Params are the parameters of an EV.
type Params struct {
	// BatteryWh is the battery capacity in Wh.
	BatteryWh float64
	// SoC is the state of charge at plug-in, from 0 to 1.
	SoC float64
	// MaxPowerW is the most power the EV accepts, in W. Zero means no limit
	// of its own, so the connector's maximum applies.
	MaxPowerW float64
	// TargetSoC is the state of charge at which the EV stops charging, from
	// 0 to 1.
	TargetSoC float64
	// Curve is the charge curve: the share of its maximum power the EV
	// accepts at each state of charge. The zero value is the standard curve.
	Curve Curve
}

// Defaults returns the built-in EV: 60 kWh, 20% charged, no power limit,
// charging to 100%.
func Defaults() Params {
	return Params{BatteryWh: 60000, SoC: 0.20, MaxPowerW: 0, TargetSoC: 1}
}

// Validate reports the first problem with p, if any.
func (p Params) Validate() error {
	switch {
	case p.BatteryWh <= 0:
		return fmt.Errorf("the battery capacity must be positive")
	case p.SoC < 0 || p.SoC > 1:
		return fmt.Errorf("the state of charge must be between 0%% and 100%%")
	case p.TargetSoC <= 0 || p.TargetSoC > 1:
		return fmt.Errorf("the target state of charge must be above 0%% and at most 100%%")
	case p.MaxPowerW < 0:
		return fmt.Errorf("the maximum power cannot be negative")
	}
	return p.Curve.Validate()
}

// EV is a plugged-in EV.
type EV struct {
	params Params
	soc    float64
}

// New returns an EV with the given parameters.
func New(p Params) *EV {
	return &EV{params: p, soc: p.SoC}
}

// Params returns the parameters the EV was created with.
func (e *EV) Params() Params { return e.params }

// SoC returns the current state of charge, from 0 to 1.
func (e *EV) SoC() float64 { return e.soc }

// Full reports whether the EV has reached its target and wants no more
// energy.
func (e *EV) Full() bool { return e.soc >= e.params.TargetSoC }

// Power returns the power in W the EV draws right now when the connector
// offers up to offeredW.
//
// The charge curve belongs to the EV: it scales the EV's own maximum power,
// and the connector then caps the result. An EV whose maximum is far above
// the connector's therefore charges at the connector's full power until late,
// as a fast-charging car does on a slow charger. An EV with no maximum of
// its own takes the connector's as its maximum.
func (e *EV) Power(offeredW float64) float64 {
	if e.Full() || offeredW <= 0 {
		return 0
	}
	maximum := e.params.MaxPowerW
	if maximum <= 0 {
		maximum = offeredW
	}
	return min(offeredW, maximum*e.params.Curve.At(e.soc))
}

// Charge advances the EV by d of charging with up to offeredW on offer, and
// returns the energy taken in Wh. There are no losses: all of it goes into
// the battery.
func (e *EV) Charge(offeredW float64, d time.Duration) float64 {
	// Take steps small enough that the taper is followed closely even when
	// d is long, as it is at high simulation speeds.
	const maxStepSoC = 0.002

	remaining := d.Hours()
	total := 0.0
	for remaining > 0 && !e.Full() {
		power := e.Power(offeredW)
		if power <= 0 {
			break
		}
		step := remaining
		if most := maxStepSoC * e.params.BatteryWh / power; step > most {
			step = most
		}
		energy := power * step
		if room := (e.params.TargetSoC - e.soc) * e.params.BatteryWh; energy >= room {
			energy = room
			e.soc = e.params.TargetSoC
		} else {
			e.soc += energy / e.params.BatteryWh
		}
		total += energy
		remaining -= step
	}
	return total
}

// ParseEnergy reads an energy value such as "60kWh" or "500Wh" into Wh.
func ParseEnergy(s string) (float64, error) {
	lower := strings.ToLower(strings.TrimSpace(s))
	units := []struct {
		suffix string
		factor float64
	}{{"mwh", 1e6}, {"kwh", 1e3}, {"wh", 1}}

	for _, u := range units {
		number, ok := strings.CutSuffix(lower, u.suffix)
		if !ok {
			continue
		}
		value, err := strconv.ParseFloat(strings.TrimSpace(number), 64)
		if err != nil || value <= 0 {
			break
		}
		return value * u.factor, nil
	}
	return 0, fmt.Errorf("invalid energy %q: want a positive number with a unit, such as 60kWh", s)
}

// ParsePercent reads a percentage such as "20%" or "20" into a fraction
// from 0 to 1.
func ParsePercent(s string) (float64, error) {
	number := strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(s), "%"))
	value, err := strconv.ParseFloat(number, 64)
	if err != nil || value < 0 || value > 100 {
		return 0, fmt.Errorf("invalid percentage %q: want a number from 0 to 100, such as 20%%", s)
	}
	return value / 100, nil
}

// State is a plugged-in EV as it is saved: its parameters and how far it has
// charged.
type State struct {
	Params Params  `json:"params"`
	SoC    float64 `json:"soc"`
}

// State returns the EV as it is now, for saving.
func (e *EV) State() State { return State{Params: e.params, SoC: e.soc} }

// Restore returns the EV described by a saved state.
func Restore(s State) *EV { return &EV{params: s.Params, soc: s.SoC} }
