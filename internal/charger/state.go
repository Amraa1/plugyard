package charger

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/Amraa1/plugyard/internal/ev"
	"github.com/Amraa1/plugyard/internal/model"
	"github.com/Amraa1/plugyard/internal/ocpp/v16"
)

// Stopped is the connection state shown for a charger that is powered off.
const Stopped = "stopped"

// State is what a charger remembers while it is powered off: everything
// needed to come back as the same charger.
type State struct {
	// SavedAt is the moment the state was taken. After a power cut it is the
	// last moment the charger is known to have been delivering energy.
	SavedAt time.Time `json:"savedAt"`
	// Config holds the configuration keys the CSMS changed. Keys it did not
	// change follow the model.
	Config     map[string]string `json:"config,omitempty"`
	Connectors []ConnectorState  `json:"connectors"`
	// Outbox holds the messages still owed to the CSMS, in order.
	Outbox    []QueuedMessage `json:"outbox,omitempty"`
	NextTxRef int             `json:"nextTxRef,omitempty"`
	// Fault and Inoperative describe the charger as a whole, which OCPP
	// addresses as connector 0.
	Fault       *Fault `json:"fault,omitempty"`
	Inoperative bool   `json:"inoperative,omitempty"`
	// AuthCache holds what the CSMS said about tags, when the authorization
	// cache is on.
	AuthCache map[string]v16.IDTagInfo `json:"authCache,omitempty"`
}

// ConnectorState is the saved state of one connector.
type ConnectorState struct {
	ID      int     `json:"id"`
	MeterWh float64 `json:"meterWh"`
	// EV is the EV plugged in, or nil.
	EV        *ev.State `json:"ev,omitempty"`
	Finishing bool      `json:"finishing,omitempty"`
	// Fault is the fault the connector has, or nil.
	Fault *Fault `json:"fault,omitempty"`
	// Inoperative says the CSMS has made the connector unavailable.
	Inoperative bool `json:"inoperative,omitempty"`
	// Tx is the open transaction, or nil. A charger that starts with one was
	// not shut down in an orderly way.
	Tx *TxState `json:"tx,omitempty"`
}

// TxState is the saved state of an open transaction.
type TxState struct {
	// Ref identifies the transaction inside the charger until the CSMS has
	// given it an ID.
	Ref         int             `json:"ref"`
	ID          int             `json:"id,omitempty"`
	IDTag       string          `json:"idTag"`
	ParentIDTag string          `json:"parentIdTag,omitempty"`
	Begin       *v16.MeterValue `json:"begin,omitempty"`
	EVFull      bool            `json:"evFull,omitempty"`
}

// QueuedMessage is a message still owed to the CSMS.
type QueuedMessage struct {
	Action  string          `json:"action"`
	Payload json.RawMessage `json:"payload"`
	// TxRef, when not zero, names the transaction whose ID the message needs
	// and the CSMS has not given yet.
	TxRef int `json:"txRef,omitempty"`
}

// Snapshot describes a powered-off charger from its saved state: each
// connector shows the status it will report when the charger starts. A nil
// state describes a charger that has never run.
func (s *State) Snapshot(chargePointID string, m *model.Model) Snapshot {
	snap := Snapshot{ChargePointID: chargePointID, Connection: Stopped}
	if s != nil {
		switch {
		case s.Fault != nil:
			snap.Connectors = append(snap.Connectors, ConnectorSnapshot{Status: v16.StatusFaulted, ErrorCode: s.Fault.Code})
		case s.Inoperative:
			snap.Connectors = append(snap.Connectors, ConnectorSnapshot{Status: v16.StatusUnavailable})
		}
	}
	for _, mc := range m.Connectors {
		ks := ConnectorSnapshot{ID: mc.ID, Status: v16.StatusAvailable}
		if s != nil {
			for _, saved := range s.Connectors {
				if saved.ID != mc.ID {
					continue
				}
				ks.MeterWh = float64(int(saved.MeterWh))
				if saved.EV != nil {
					soc := saved.EV.SoC
					ks.SoC = &soc
				}
				// An open transaction ends when the charger starts.
				switch {
				case saved.Fault != nil:
					ks.Status, ks.ErrorCode = v16.StatusFaulted, saved.Fault.Code
				case saved.Inoperative:
					ks.Status = v16.StatusUnavailable
				case saved.EV != nil && (saved.Finishing || saved.Tx != nil):
					ks.Status = v16.StatusFinishing
				case saved.EV != nil:
					ks.Status = v16.StatusPreparing
				}
			}
		}
		snap.Connectors = append(snap.Connectors, ks)
	}
	return snap
}

// state returns the charger's state as of now. The caller holds c.mu.
func (c *Charger) state(now time.Time) State {
	st := State{SavedAt: now, NextTxRef: c.nextTxRef, Fault: c.zero.fault, Inoperative: c.zero.inoperative}
	if len(c.authCache) > 0 {
		st.AuthCache = make(map[string]v16.IDTagInfo, len(c.authCache))
		for tag, info := range c.authCache {
			st.AuthCache[tag] = info
		}
	}
	if len(c.overrides) > 0 {
		st.Config = make(map[string]string, len(c.overrides))
		for key, value := range c.overrides {
			st.Config[key] = value
		}
	}
	for _, k := range c.connectors {
		c.advance(k, now)
		// A connector due to become unavailable when its transaction ends
		// is saved as unavailable: the transaction does not outlive a stop.
		ks := ConnectorState{ID: k.id, MeterWh: k.meterWh, Finishing: k.finishing, Fault: k.fault, Inoperative: k.inoperative || k.scheduled}
		if k.ev != nil {
			evState := k.ev.State()
			ks.EV = &evState
		}
		if tx := k.tx; tx != nil {
			ks.Tx = &TxState{
				Ref:         tx.ref,
				ID:          tx.id,
				IDTag:       tx.idTag,
				ParentIDTag: tx.parentIDTag,
				Begin:       tx.begin,
				EVFull:      tx.evFull,
			}
		}
		st.Connectors = append(st.Connectors, ks)
	}
	for _, m := range c.outbox {
		if m.volatile {
			continue
		}
		payload, err := json.Marshal(m.req)
		if err != nil {
			continue
		}
		st.Outbox = append(st.Outbox, QueuedMessage{Action: m.action, Payload: payload, TxRef: m.txRef})
	}
	return st
}

// restore brings the charger back to a saved state. It runs before the
// charger is powered on.
func (c *Charger) restore(st *State) {
	c.nextTxRef = st.NextTxRef
	c.zero.fault, c.zero.inoperative = st.Fault, st.Inoperative
	c.zero.status, c.zero.code = c.zero.currentStatus(), c.zero.errorCode()
	for tag, info := range st.AuthCache {
		c.authCache[tag] = info
	}
	for key, value := range st.Config {
		c.overrides[key] = value
		c.config[key] = value
	}

	for _, q := range st.Outbox {
		var req any
		switch q.Action {
		case v16.ActionStartTransaction:
			req = &v16.StartTransactionRequest{}
		case v16.ActionStopTransaction:
			req = &v16.StopTransactionRequest{}
		case v16.ActionMeterValues:
			req = &v16.MeterValuesRequest{}
		default:
			continue
		}
		if json.Unmarshal(q.Payload, req) != nil {
			continue
		}
		c.outbox = append(c.outbox, &outMsg{action: q.Action, req: req, txRef: q.TxRef})
	}

	for _, ks := range st.Connectors {
		k, err := c.connector(ks.ID)
		if err != nil {
			continue
		}
		k.meterWh = ks.MeterWh
		k.finishing = ks.Finishing
		k.fault, k.inoperative = ks.Fault, ks.Inoperative
		if ks.EV != nil {
			k.ev = ev.Restore(*ks.EV)
		}

		// A transaction still open in the saved state means the charger lost
		// power: it could not end the transaction then, so it does now, with
		// the last reading it saved.
		if tx := ks.Tx; tx != nil {
			var data []v16.MeterValue
			if tx.Begin != nil {
				data = append(data, *tx.Begin)
				if end := c.sample(k, c.configMeasurands("StopTxnSampledData"), v16.ContextTransactionEnd, st.SavedAt); end != nil {
					data = append(data, *end)
				}
			}
			m := &outMsg{
				action: v16.ActionStopTransaction,
				req: &v16.StopTransactionRequest{
					MeterStop:       int(k.meterWh),
					Timestamp:       v16.Time{Time: st.SavedAt},
					TransactionID:   tx.ID,
					Reason:          v16.ReasonPowerLoss,
					TransactionData: data,
				},
			}
			if tx.ID == 0 && c.startPending(tx.Ref) {
				m.txRef = tx.Ref
			}
			c.outbox = append(c.outbox, m)
			k.finishing = k.ev != nil

			if tx.ID != 0 {
				c.recovered = append(c.recovered, fmt.Sprintf("connector %d: transaction %d was cut short by a power loss", k.id, tx.ID))
			} else {
				c.recovered = append(c.recovered, fmt.Sprintf("connector %d: a transaction was cut short by a power loss", k.id))
			}
		}
		k.status, k.code = k.currentStatus(), k.errorCode()
	}
}

// startPending reports whether the StartTransaction of the transaction with
// the given reference is still waiting for the CSMS. The caller holds c.mu.
func (c *Charger) startPending(ref int) bool {
	for _, m := range c.outbox {
		if m.action == v16.ActionStartTransaction && m.txRef == ref {
			return true
		}
	}
	return false
}

// flush saves the charger's state if it has changed. The caller holds c.mu.
func (c *Charger) flush() {
	if !c.dirty {
		return
	}
	c.dirty = false
	if c.cfg.Save == nil || c.killed || c.off {
		return
	}
	now := time.Now()
	interval := c.configSeconds("MeterSaveInterval")
	if interval <= 0 {
		interval = defaultMeterSaveInterval
	}
	c.nextSave = now.Add(interval)
	if err := c.cfg.Save(c.state(now)); err != nil {
		c.event("could not save the charger's state: %v", err)
	}
}

// unlock saves the state if needed and releases c.mu.
func (c *Charger) unlock() {
	c.flush()
	c.mu.Unlock()
}
