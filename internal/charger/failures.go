package charger

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/Amraa1/plugyard/internal/ocpp/v16"
)

// Fault is something wrong with a connector, or with the charger as a whole.
type Fault struct {
	// Code is one of the OCPP 1.6 error codes, such as GroundFailure.
	Code string `json:"code"`
	// Info is free text about the fault.
	Info string `json:"info,omitempty"`
	// VendorCode is the vendor's own code for the fault.
	VendorCode string `json:"vendorCode,omitempty"`
}

// reboot is a request to reboot the charger.
type reboot struct {
	reason string
	hard   bool
}

var (
	errReboot  = errors.New("the charger is rebooting")
	errNetwork = errors.New("the network connection was taken down")
)

// connectorOrCharger returns the connector with the given number, where 0
// stands for the charger as a whole. The caller holds c.mu.
func (c *Charger) connectorOrCharger(id int) (*connector, error) {
	if id == 0 {
		return c.zero, nil
	}
	return c.connector(id)
}

func describe(k *connector) string {
	if k.id == 0 {
		return "the charger"
	}
	return fmt.Sprintf("connector %d", k.id)
}

// Fault makes a connector, or with connector 0 the charger as a whole,
// report a fault. A transaction running there ends.
func (c *Charger) Fault(connectorID int, fault Fault) (string, error) {
	if !slices.Contains(v16.ErrorCodes, fault.Code) {
		return "", fmt.Errorf("unknown error code %q: OCPP 1.6 has %s", fault.Code, strings.Join(v16.ErrorCodes, ", "))
	}
	if len(fault.Info) > 50 || len(fault.VendorCode) > 50 {
		return "", errors.New("the info and the vendor code may each be 50 characters at most")
	}

	c.mu.Lock()
	defer c.unlock()
	if c.off {
		return "", errOff
	}
	k, err := c.connectorOrCharger(connectorID)
	if err != nil {
		return "", err
	}

	k.fault = &fault
	c.dirty = true
	outcome := "fault " + fault.Code + " reported"
	c.event("%s: fault %s", describe(k), fault.Code)

	// A fault of the whole charger ends every transaction.
	affected := []*connector{k}
	if k.id == 0 {
		affected = c.connectors
	}
	for _, a := range affected {
		if a.pending != nil {
			a.pending.timer.Stop()
			a.pending = nil
		}
		if a.tx != nil {
			outcome += ", " + c.stopTx(a, v16.ReasonOther, a.tx.idTag)
		}
		c.reportStatus(a)
	}
	c.reportStatus(k)
	return outcome, nil
}

// ClearFault recovers a connector, or the charger as a whole, from its
// fault.
func (c *Charger) ClearFault(connectorID int) (string, error) {
	c.mu.Lock()
	defer c.unlock()
	if c.off {
		return "", errOff
	}
	k, err := c.connectorOrCharger(connectorID)
	if err != nil {
		return "", err
	}
	if k.fault == nil {
		return "", fmt.Errorf("%s has no fault", describe(k))
	}
	code := k.fault.Code
	k.fault = nil
	c.dirty = true
	c.event("%s: fault %s cleared", describe(k), code)
	c.reportStatus(k)
	return fmt.Sprintf("fault %s cleared, %s is %s", code, describe(k), k.status), nil
}

// Offline takes the charger's network connection down. The charger keeps
// running and queues what it has to tell the CSMS. It stays away until
// Online, or for the given time when that is not zero.
func (c *Charger) Offline(d time.Duration) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	switch {
	case c.off:
		return "", errOff
	case c.netDown:
		return "", errors.New("the charger is already offline")
	}
	c.netDown = true
	c.signalNetwork()

	if d <= 0 {
		c.event("network connection taken down")
		return "offline", nil
	}
	c.event("network connection taken down for %s", d)
	c.netTimer = time.AfterFunc(d, func() { _, _ = c.Online() })
	return fmt.Sprintf("offline for %s", d), nil
}

// Online restores the network connection taken down by Offline.
func (c *Charger) Online() (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	switch {
	case c.off:
		return "", errOff
	case !c.netDown:
		return "", errors.New("the charger was not taken offline; when it loses the CSMS by itself, it reconnects by itself")
	}
	c.netDown = false
	if c.netTimer != nil {
		c.netTimer.Stop()
		c.netTimer = nil
	}
	c.signalNetwork()
	c.event("network connection restored")
	return "online, connecting", nil
}

// signalNetwork wakes whoever waits for the network to change. The caller
// holds c.mu.
func (c *Charger) signalNetwork() {
	select {
	case c.network <- struct{}{}:
	default:
	}
}

// Reboot reboots the charger as a person at the charger would: transactions
// end, and the charger boots again.
func (c *Charger) Reboot() (string, error) {
	c.mu.Lock()
	off := c.off
	c.mu.Unlock()
	if off {
		return "", errOff
	}
	c.reboot(v16.ReasonReboot, false)
	return "rebooting", nil
}

// reboot ends the transactions with the given reason and has the charger
// boot again. A hard reboot drops the connection instead of closing it.
func (c *Charger) reboot(reason string, hard bool) {
	c.mu.Lock()
	connected := c.conn != nil
	if !connected {
		// With no connection there is nothing to restart but the charger's
		// own state: the transactions end, and the CSMS hears of it later.
		c.endForReboot(reason)
		c.event("rebooted")
	}
	c.unlock()
	if connected {
		select {
		case c.reboots <- reboot{reason: reason, hard: hard}:
		default:
		}
	}
}

// endForReboot ends what a reboot ends. The caller holds c.mu.
func (c *Charger) endForReboot(reason string) {
	for _, k := range c.connectors {
		if k.pending != nil {
			k.pending.timer.Stop()
			k.pending = nil
		}
		if k.tx != nil {
			c.stopTx(k, reason, k.tx.idTag)
		}
		c.reportStatus(k)
	}
}
