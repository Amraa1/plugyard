// Package control is the channel between plugyard commands and the process
// that owns a running site: HTTP with JSON over a Unix domain socket in the
// data directory (see docs/adr/0004). The channel is internal and may change
// between versions.
package control

import (
	"github.com/Amraa1/plugyard/internal/charger"
	"github.com/Amraa1/plugyard/internal/datadir"
	"github.com/Amraa1/plugyard/internal/ev"
)

// ProtocolVersion changes whenever requests or answers change shape. A
// command only talks to a site that speaks the same version.
const ProtocolVersion = 3

const (
	socketPrefix = "s-"
	socketSuffix = ".sock"
)

// SocketPath returns the socket file of the site with the given name.
func SocketPath(dataDir, siteName string) string {
	return datadir.SiteBase(dataDir, siteName) + socketSuffix
}

// Info identifies the process behind a socket.
type Info struct {
	Protocol int    `json:"protocol"`
	Site     string `json:"site"`
	PID      int    `json:"pid"`
	// Chargers is the number of chargers in the site, powered on or not.
	Chargers int `json:"chargers"`
	// Powered is the number of chargers that are powered on.
	Powered int `json:"powered"`
	// CSMS lists the distinct CSMS URLs the site connects to.
	CSMS []string `json:"csms"`
}

// ChargerState is a charger's state together with the site it runs in.
type ChargerState struct {
	Site string `json:"site"`
	// Name is the charger's name, unique on the machine.
	Name string `json:"name"`
	// Powered says whether the charger is powered on. It is not part of the
	// JSON output: a charger that is off shows "stopped" as its connection.
	Powered bool `json:"-"`
	charger.Snapshot
}

// wireState is ChargerState as it travels over the socket, where Powered
// must not be left out.
type wireState struct {
	ChargerState
	Powered bool `json:"powered"`
}

// Power operations that PowerRequest can carry.
const (
	OpStart   = "start"
	OpStop    = "stop"
	OpRestart = "restart"
	OpKill    = "kill"
	OpRemove  = "rm"
)

// PowerRequest asks a site to power some of its chargers on or off, or to
// remove them.
type PowerRequest struct {
	ChargePointIDs []string `json:"chargePointIds"`
	Op             string   `json:"op"`
	// Force lets OpRemove stop a running charger first.
	Force bool `json:"force,omitempty"`
}

// Actions that ExecRequest can carry.
const (
	ActionPlug   = "plug"
	ActionUnplug = "unplug"
	ActionTap    = "tap"
)

// ExecRequest asks a site to run one action on some of its chargers.
type ExecRequest struct {
	ChargePointIDs []string `json:"chargePointIds"`
	Action         string   `json:"action"`
	Connector      int      `json:"connector"`
	// Tag is the tag presented by a tap.
	Tag string `json:"tag,omitempty"`
	// EVPreset names an EV preset of the site to plug in.
	EVPreset string `json:"evPreset,omitempty"`
	// EV holds the EV parameters given with a plug. They take precedence
	// over the preset.
	EV ev.Overrides `json:"ev"`
}

// ExecResult says what an action did on one charger.
type ExecResult struct {
	ChargePointID string `json:"chargePointId"`
	// Connector is zero for an operation on the whole charger.
	Connector int  `json:"connector,omitempty"`
	OK        bool `json:"ok"`
	// Message is the outcome when OK, and the reason when not.
	Message string `json:"message"`
}
