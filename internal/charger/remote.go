package charger

import (
	"context"
	"encoding/json"
	"slices"
	"time"

	"github.com/Amraa1/plugyard/internal/ocpp/ocppj"
	"github.com/Amraa1/plugyard/internal/ocpp/v16"
)

// actDelay is how long the charger waits after answering a request before it
// acts on it, so that the answer reaches the CSMS first.
const actDelay = 20 * time.Millisecond

// after runs fn once the answer to the current request has been sent.
func (c *Charger) after(fn func()) {
	time.AfterFunc(actDelay, func() {
		c.mu.Lock()
		gone := c.off || c.killed
		c.mu.Unlock()
		if !gone {
			fn()
		}
	})
}

// handle answers a request from the CSMS.
func (c *Charger) handle(_ context.Context, action string, payload json.RawMessage) (any, *ocppj.CallError) {
	// decode reads the request, and reports a payload that does not fit.
	var malformed *ocppj.CallError
	decode := func(v any) bool {
		if err := json.Unmarshal(payload, v); err != nil {
			malformed = &ocppj.CallError{Code: ocppj.ErrFormationViolation, Description: "the payload of " + action + " could not be read: " + err.Error()}
			return false
		}
		return true
	}
	status := func(s string) (any, *ocppj.CallError) { return v16.StatusAnswer{Status: s}, nil }

	switch action {
	case v16.ActionRemoteStartTransaction:
		var req v16.RemoteStartTransactionRequest
		if !decode(&req) {
			return nil, malformed
		}
		return status(c.remoteStart(req))

	case v16.ActionRemoteStopTransaction:
		var req v16.RemoteStopTransactionRequest
		if !decode(&req) {
			return nil, malformed
		}
		return status(c.remoteStop(req))

	case v16.ActionReset:
		var req v16.ResetRequest
		if !decode(&req) {
			return nil, malformed
		}
		switch req.Type {
		case v16.ResetSoft:
			c.event("the CSMS asked for a soft reset")
			c.after(func() { c.reboot(v16.ReasonSoftReset, false) })
		case v16.ResetHard:
			c.event("the CSMS asked for a hard reset")
			c.after(func() { c.reboot(v16.ReasonHardReset, true) })
		default:
			return status(v16.StatusRejected)
		}
		return status(v16.StatusAccepted)

	case v16.ActionGetConfiguration:
		var req v16.GetConfigurationRequest
		if !decode(&req) {
			return nil, malformed
		}
		return c.getConfiguration(req), nil

	case v16.ActionChangeConfiguration:
		var req v16.ChangeConfigurationRequest
		if !decode(&req) {
			return nil, malformed
		}
		return status(c.changeConfiguration(req))

	case v16.ActionChangeAvailability:
		var req v16.ChangeAvailabilityRequest
		if !decode(&req) {
			return nil, malformed
		}
		return status(c.changeAvailability(req))

	case v16.ActionUnlockConnector:
		var req v16.UnlockConnectorRequest
		if !decode(&req) {
			return nil, malformed
		}
		return status(c.unlockConnector(req))

	case v16.ActionClearCache:
		c.mu.Lock()
		c.authCache = map[string]v16.IDTagInfo{}
		c.dirty = true
		c.event("the CSMS cleared the authorization cache")
		c.unlock()
		return status(v16.StatusAccepted)

	case v16.ActionDataTransfer:
		var req v16.DataTransferRequest
		if !decode(&req) {
			return nil, malformed
		}
		if slices.Contains(c.cfg.Model.DataTransfer, req.VendorID) {
			return v16.DataTransferResponse{Status: v16.StatusAccepted}, nil
		}
		return v16.DataTransferResponse{Status: v16.StatusUnknownVendor}, nil

	case v16.ActionTriggerMessage:
		var req v16.TriggerMessageRequest
		if !decode(&req) {
			return nil, malformed
		}
		return status(c.triggerMessage(req))
	}

	if slices.Contains(v16.UnsupportedActions, action) {
		return nil, &ocppj.CallError{Code: ocppj.ErrNotSupported, Description: action + " belongs to a feature profile this charger does not support"}
	}
	return nil, &ocppj.CallError{Code: ocppj.ErrNotImplemented, Description: action + " is not implemented"}
}

// free reports whether a transaction could start on a connector. The caller
// holds c.mu.
func (c *Charger) free(k *connector) bool {
	return k.tx == nil && k.pending == nil && !k.finishing && c.blocked(k) == ""
}

func (c *Charger) remoteStart(req v16.RemoteStartTransactionRequest) string {
	if req.IDTag == "" || len(req.IDTag) > 20 {
		return v16.StatusRejected
	}

	c.mu.Lock()
	var k *connector
	if req.ConnectorID != nil {
		if found, err := c.connector(*req.ConnectorID); err == nil && c.free(found) {
			k = found
		}
	} else {
		// A connector with an EV waiting comes first, then the lowest free
		// one.
		for _, candidate := range c.connectors {
			if c.free(candidate) && candidate.ev != nil {
				k = candidate
				break
			}
		}
		for _, candidate := range c.connectors {
			if k == nil && c.free(candidate) {
				k = candidate
			}
		}
	}
	authorizeFirst := c.configBool("AuthorizeRemoteTxRequests")
	c.mu.Unlock()

	if k == nil {
		c.event("the CSMS asked to start a transaction with tag %s, which was rejected: no connector is free", req.IDTag)
		return v16.StatusRejected
	}
	c.event("connector %d: the CSMS asked to start a transaction with tag %s", k.id, req.IDTag)

	c.after(func() {
		parent := ""
		if authorizeFirst {
			info, err := c.authorize(context.Background(), req.IDTag)
			if err != nil {
				c.event("connector %d: tag %s could not be authorized: %v", k.id, req.IDTag, err)
				return
			}
			if info.Status != v16.AuthorizationAccepted {
				c.event("connector %d: tag %s rejected by the CSMS (%s)", k.id, req.IDTag, info.Status)
				return
			}
			parent = info.ParentIDTag
		}

		c.mu.Lock()
		defer c.unlock()
		if !c.free(k) {
			c.event("connector %d: the remote start came too late, the connector is no longer free", k.id)
			return
		}
		c.admit(k, req.IDTag, parent)
		c.reportStatus(k)
	})
	return v16.StatusAccepted
}

func (c *Charger) remoteStop(req v16.RemoteStopTransactionRequest) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, k := range c.connectors {
		tx := k.tx
		if tx == nil || tx.id != req.TransactionID || tx.id == 0 {
			continue
		}
		c.event("connector %d: the CSMS asked to stop transaction %d", k.id, tx.id)
		c.after(func() {
			c.mu.Lock()
			defer c.unlock()
			if k.tx == tx {
				c.stopTx(k, v16.ReasonRemote, tx.idTag)
				c.reportStatus(k)
			}
		})
		return v16.StatusAccepted
	}
	return v16.StatusRejected
}

func (c *Charger) changeAvailability(req v16.ChangeAvailabilityRequest) string {
	var inoperative bool
	switch req.Type {
	case v16.AvailabilityInoperative:
		inoperative = true
	case v16.AvailabilityOperative:
	default:
		return v16.StatusRejected
	}

	c.mu.Lock()
	defer c.unlock()

	// Connector 0 stands for the charger as a whole: every connector
	// follows it.
	targets := c.connectors
	if req.ConnectorID != 0 {
		k, err := c.connector(req.ConnectorID)
		if err != nil {
			return v16.StatusRejected
		}
		targets = []*connector{k}
	} else {
		c.zero.inoperative = inoperative
		c.reportStatus(c.zero)
	}

	answer := v16.StatusAccepted
	for _, k := range targets {
		switch {
		case !inoperative:
			k.inoperative, k.scheduled = false, false
		case k.tx != nil:
			// The transaction is left to finish first.
			k.scheduled = true
			answer = v16.StatusScheduled
		default:
			k.inoperative = true
			if k.pending != nil {
				k.pending.timer.Stop()
				k.pending = nil
			}
		}
		c.reportStatus(k)
	}
	c.dirty = true
	c.event("the CSMS made connector %d %s (%s)", req.ConnectorID, req.Type, answer)
	return answer
}

func (c *Charger) unlockConnector(req v16.UnlockConnectorRequest) string {
	c.mu.Lock()
	defer c.unlock()
	k, err := c.connector(req.ConnectorID)
	if err != nil {
		return v16.StatusNotSupported
	}
	c.event("connector %d: the CSMS asked to unlock the connector", k.id)
	if k.tx != nil {
		c.stopTx(k, v16.ReasonUnlockCommand, k.tx.idTag)
		c.reportStatus(k)
	}
	return v16.StatusUnlocked
}

func (c *Charger) triggerMessage(req v16.TriggerMessageRequest) string {
	c.mu.Lock()
	targets := c.connectors
	if req.ConnectorID != nil {
		k, err := c.connector(*req.ConnectorID)
		if err != nil {
			c.mu.Unlock()
			return v16.StatusRejected
		}
		targets = []*connector{k}
	}
	conn := c.conn
	c.mu.Unlock()

	c.event("the CSMS asked for a %s", req.RequestedMessage)
	switch req.RequestedMessage {
	case v16.ActionBootNotification:
		c.after(func() {
			if conn != nil {
				_ = c.call(context.Background(), conn, v16.ActionBootNotification, c.bootRequest(), nil)
			}
		})
	case v16.ActionHeartbeat:
		c.after(func() {
			if conn != nil {
				_ = c.call(context.Background(), conn, v16.ActionHeartbeat, v16.HeartbeatRequest{}, nil)
			}
		})
	case v16.ActionMeterValues:
		c.after(func() {
			c.mu.Lock()
			defer c.unlock()
			now := time.Now()
			for _, k := range targets {
				c.advance(k, now)
				c.enqueueMeterValues(k, k.tx, now, v16.ContextTrigger)
			}
		})
	case v16.ActionStatusNotification:
		c.after(func() {
			c.mu.Lock()
			defer c.mu.Unlock()
			for _, k := range targets {
				c.enqueueStatus(k)
			}
		})
	default:
		// DiagnosticsStatusNotification and FirmwareStatusNotification
		// belong to a profile the charger does not have.
		return v16.StatusNotImplemented
	}
	return v16.StatusAccepted
}
