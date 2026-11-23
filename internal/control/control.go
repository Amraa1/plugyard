// Package control is the channel between plugyard commands and the process
// that owns a running site: HTTP with JSON over a Unix domain socket in the
// data directory (see docs/adr/0004). The channel is internal and may change
// between versions.
package control

import (
	"crypto/sha256"
	"encoding/hex"
	"path/filepath"

	"github.com/Amraa1/plugyard/internal/charger"
	"github.com/Amraa1/plugyard/internal/ev"
)

// ProtocolVersion changes whenever requests or answers change shape. A
// command only talks to a site that speaks the same version.
const ProtocolVersion = 2

const (
	socketPrefix = "s-"
	socketSuffix = ".sock"
)

// SocketPath returns the socket file of the site with the given name. The
// file name is a short hash, because site names may be long or contain
// characters that file names cannot, and socket paths have a tight length
// limit.
func SocketPath(dataDir, siteName string) string {
	sum := sha256.Sum256([]byte(siteName))
	return filepath.Join(dataDir, socketPrefix+hex.EncodeToString(sum[:6])+socketSuffix)
}

// Info identifies the process behind a socket.
type Info struct {
	Protocol int    `json:"protocol"`
	Site     string `json:"site"`
	PID      int    `json:"pid"`
	// Chargers is the number of chargers in the site.
	Chargers int `json:"chargers"`
	// CSMS lists the distinct CSMS URLs the site connects to.
	CSMS []string `json:"csms"`
}

// ChargerState is a charger's state together with the site it runs in.
type ChargerState struct {
	Site string `json:"site"`
	charger.Snapshot
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
	Connector     int    `json:"connector"`
	OK            bool   `json:"ok"`
	// Message is the outcome when OK, and the reason when not.
	Message string `json:"message"`
}
