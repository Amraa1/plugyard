package charger

import (
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Amraa1/plugyard/internal/ocpp/v16"
)

// keyKind is what a configuration key holds.
type keyKind int

const (
	kindText keyKind = iota
	kindInt
	kindBool
	kindMeasurands
)

type keySpec struct {
	kind     keyKind
	readonly bool
}

// knownKeys are the configuration keys the charger acts on. Keys a
// Chargerfile adds besides these are text the CSMS can read and change.
var knownKeys = map[string]keySpec{
	"AllowOfflineTxForUnknownId":              {kind: kindBool},
	"AuthorizationCacheEnabled":               {kind: kindBool},
	"AuthorizeRemoteTxRequests":               {kind: kindBool},
	"ChargeProfileMaxStackLevel":              {kind: kindInt, readonly: true},
	"ChargingScheduleAllowedChargingRateUnit": {kind: kindText, readonly: true},
	"ChargingScheduleMaxPeriods":              {kind: kindInt, readonly: true},
	"MaxChargingProfilesInstalled":            {kind: kindInt, readonly: true},
	"ConnectionTimeOut":                       {kind: kindInt},
	"GetConfigurationMaxKeys":                 {kind: kindInt, readonly: true},
	"HeartbeatInterval":                       {kind: kindInt},
	"LocalAuthorizeOffline":                   {kind: kindBool},
	"MeterSaveInterval":                       {kind: kindInt},
	"MeterValueSampleInterval":                {kind: kindInt},
	"MeterValuesSampledData":                  {kind: kindMeasurands},
	"NumberOfConnectors":                      {kind: kindInt, readonly: true},
	"StopTransactionOnInvalidId":              {kind: kindBool},
	"StopTxnSampledData":                      {kind: kindMeasurands},
	"SupportedFeatureProfiles":                {kind: kindText, readonly: true},
	"TransactionMessageAttempts":              {kind: kindInt},
	"TransactionMessageRetryInterval":         {kind: kindInt},
}

// maxConfigKeys is how many keys one GetConfiguration answer may carry.
const maxConfigKeys = 500

// configBool reads a configuration key holding true or false. The caller
// holds c.mu.
func (c *Charger) configBool(key string) bool {
	return strings.EqualFold(strings.TrimSpace(c.config[key]), "true")
}

// configInt reads a configuration key holding a number, or returns fallback.
// The caller holds c.mu.
func (c *Charger) configInt(key string, fallback int) int {
	n, err := strconv.Atoi(strings.TrimSpace(c.config[key]))
	if err != nil {
		return fallback
	}
	return n
}

// getConfiguration answers GetConfiguration.
func (c *Charger) getConfiguration(req v16.GetConfigurationRequest) v16.GetConfigurationResponse {
	c.mu.Lock()
	defer c.mu.Unlock()

	var resp v16.GetConfigurationResponse
	keys := req.Key
	if len(keys) == 0 {
		for key := range c.config {
			keys = append(keys, key)
		}
		sort.Strings(keys)
	}
	for _, key := range keys {
		value, ok := c.config[key]
		if !ok {
			resp.UnknownKey = append(resp.UnknownKey, key)
			continue
		}
		if len(resp.ConfigurationKey) == maxConfigKeys {
			break
		}
		resp.ConfigurationKey = append(resp.ConfigurationKey, v16.KeyValue{
			Key:      key,
			Readonly: knownKeys[key].readonly,
			Value:    value,
		})
	}
	return resp
}

// changeConfiguration answers ChangeConfiguration, and applies the change at
// once when it accepts it.
func (c *Charger) changeConfiguration(req v16.ChangeConfigurationRequest) string {
	c.mu.Lock()
	defer c.unlock()

	if _, exists := c.config[req.Key]; !exists {
		return v16.StatusNotSupported
	}
	spec := knownKeys[req.Key]
	value := strings.TrimSpace(req.Value)
	if spec.readonly || len(value) > 500 || !validValue(spec.kind, value) {
		c.event("the CSMS tried to set %s to %q, which was rejected", req.Key, req.Value)
		return v16.StatusRejected
	}

	c.config[req.Key] = value
	c.overrides[req.Key] = value
	c.dirty = true
	c.event("the CSMS set %s to %q", req.Key, value)

	switch req.Key {
	case "HeartbeatInterval":
		select {
		case c.heartbeatChanged <- struct{}{}:
		default:
		}
	case "MeterValueSampleInterval":
		// Running transactions follow the new interval from now on.
		interval := c.configSeconds(req.Key)
		for _, k := range c.connectors {
			if k.tx == nil {
				continue
			}
			k.tx.nextSample = time.Time{}
			if interval > 0 {
				k.tx.nextSample = time.Now().Add(interval)
			}
		}
	case "AuthorizationCacheEnabled":
		if !c.configBool(req.Key) {
			c.authCache = map[string]v16.IDTagInfo{}
		}
	}
	return v16.StatusAccepted
}

func validValue(kind keyKind, value string) bool {
	switch kind {
	case kindInt:
		n, err := strconv.Atoi(value)
		return err == nil && n >= 0
	case kindBool:
		return strings.EqualFold(value, "true") || strings.EqualFold(value, "false")
	case kindMeasurands:
		if value == "" {
			return true
		}
		for _, name := range strings.Split(value, ",") {
			if !slices.Contains(v16.Measurands, strings.TrimSpace(name)) {
				return false
			}
		}
	}
	return true
}
