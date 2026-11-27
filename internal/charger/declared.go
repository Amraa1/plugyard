package charger

import (
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/Amraa1/plugyard/internal/ev"
	"github.com/Amraa1/plugyard/internal/model"
	"github.com/Amraa1/plugyard/internal/ocpp/v16"
)

// Declared is the declared state of a charger: the state its site file says
// it is in each time it powers on. Where it speaks, it takes the place of
// the saved state (ADR 0009).
type Declared struct {
	// Connection is Offline for a charger that powers on with its network
	// connection taken down, Stopped for one its site leaves powered off,
	// and empty otherwise.
	Connection string `json:"connection,omitempty"`
	// Connectors holds the connectors the site file lists, in the order of
	// their numbers. A connector that is not listed follows the saved
	// state.
	Connectors []DeclaredConnector `json:"connectors,omitempty"`
}

// DeclaredConnector is the declared state of one connector. It is declared
// whole: without an EV nothing is plugged in, and without a fault there is
// none.
type DeclaredConnector struct {
	// ID is the connector's number, or 0 for the charger as a whole.
	ID    int         `json:"id"`
	EV    *DeclaredEV `json:"ev,omitempty"`
	Fault *Fault      `json:"fault,omitempty"`
}

// DeclaredEV is an EV a site file declares to be plugged in.
type DeclaredEV struct {
	// Preset names the EV preset the EV is made from.
	Preset string `json:"preset"`
	// Params is that EV in full, as the presets stood when the site file
	// was read.
	Params ev.Params `json:"params"`
}

// Empty reports whether nothing is declared.
func (d *Declared) Empty() bool {
	return d == nil || (d.Connection == "" && len(d.Connectors) == 0)
}

// Stopped reports whether the charger is declared to be left powered off.
func (d *Declared) Stopped() bool {
	return d != nil && d.Connection == Stopped
}

// connector returns what is declared for a connector, where 0 stands for
// the charger as a whole, or nil when the connector is not listed.
func (d *Declared) connector(id int) *DeclaredConnector {
	if d == nil {
		return nil
	}
	for i := range d.Connectors {
		if d.Connectors[i].ID == id {
			return &d.Connectors[i]
		}
	}
	return nil
}

// declare puts the charger in its declared state. It runs before the
// charger is powered on, after its saved state is restored.
func (c *Charger) declare(d *Declared) {
	if d.Connection == Offline {
		c.netDown = true
		c.connection = Offline
		c.recovered = append(c.recovered, "offline, as its site file declares")
	}
	for _, dk := range d.Connectors {
		k, err := c.connectorOrCharger(dk.ID)
		if err != nil {
			continue
		}
		k.fault = nil
		if dk.Fault != nil {
			fault := *dk.Fault
			k.fault = &fault
			c.recovered = append(c.recovered, fmt.Sprintf("%s: fault %s, as its site file declares", describe(k), fault.Code))
		}
		if k.id != 0 {
			k.ev, k.finishing = nil, false
			if dk.EV != nil {
				p := dk.EV.Params
				k.ev = ev.New(p)
				c.recovered = append(c.recovered, fmt.Sprintf("connector %d: EV plugged in (%s kWh, %.0f%%), as its site file declares",
					k.id, strconv.FormatFloat(p.BatteryWh/1000, 'f', -1, 64), p.SoC*100))
			}
		}
		k.status, k.code = k.currentStatus(), k.errorCode()
	}
}

// arrive starts the transactions the declared EVs bring on a model that
// starts on plug-in: a declared EV is a new arrival each time the charger
// powers on. The CSMS hears of them once the charger has booted.
func (c *Charger) arrive() {
	d := c.cfg.Declared
	if d == nil || c.cfg.Model.StartOn != model.StartOnPlug {
		return
	}
	c.mu.Lock()
	defer c.unlock()
	for _, dk := range d.Connectors {
		if dk.EV == nil {
			continue
		}
		k, err := c.connector(dk.ID)
		if err != nil || k.ev == nil || k.tx != nil || c.blocked(k) != "" {
			continue
		}
		c.startTx(k, c.cfg.Model.StartTag, "")
		c.reportStatus(k)
	}
}

// Check lists what is wrong with a declared state for a charger of the
// given model.
func (d *Declared) Check(m *model.Model) []error {
	if d == nil {
		return nil
	}
	var problems []error
	problem := func(format string, args ...any) {
		problems = append(problems, fmt.Errorf(format, args...))
	}

	switch d.Connection {
	case "", Offline, Stopped:
	default:
		problem("connection: %q is not understood; use %s or %s", d.Connection, Offline, Stopped)
	}

	seen := map[int]bool{}
	for _, k := range d.Connectors {
		at := "connectors." + strconv.Itoa(k.ID)
		switch {
		case seen[k.ID]:
			problem("%s: the connector is listed twice", at)
			continue
		case k.ID != 0 && !hasConnector(m, k.ID):
			problem("%s: the model has no connector %d; it has %s", at, k.ID, connectorNumbers(m))
			continue
		}
		seen[k.ID] = true

		if k.Fault != nil {
			if err := checkFault(*k.Fault); err != nil {
				problem("%s: fault: %v", at, err)
			}
		}
		switch {
		case k.EV == nil:
		case k.ID == 0:
			problem("%s: ev: connector 0 is the charger as a whole, which no EV plugs into; name a connector", at)
		default:
			if err := k.EV.Params.Validate(); err != nil {
				problem("%s: ev: %v", at, err)
			}
		}
	}
	return problems
}

func hasConnector(m *model.Model, id int) bool {
	return slices.ContainsFunc(m.Connectors, func(mc model.Connector) bool { return mc.ID == id })
}

// connectorNumbers lists a model's connectors in words: "1", "1 and 2".
func connectorNumbers(m *model.Model) string {
	numbers := make([]string, 0, len(m.Connectors))
	for _, mc := range m.Connectors {
		numbers = append(numbers, strconv.Itoa(mc.ID))
	}
	if len(numbers) < 2 {
		return strings.Join(numbers, "")
	}
	return strings.Join(numbers[:len(numbers)-1], ", ") + " and " + numbers[len(numbers)-1]
}

// checkFault reports what is wrong with a fault, if anything.
func checkFault(fault Fault) error {
	if !slices.Contains(v16.ErrorCodes, fault.Code) {
		return fmt.Errorf("unknown error code %q: OCPP 1.6 has %s", fault.Code, strings.Join(v16.ErrorCodes, ", "))
	}
	if len(fault.Info) > 50 || len(fault.VendorCode) > 50 {
		return errors.New("the info and the vendor code may each be 50 characters at most")
	}
	return nil
}
