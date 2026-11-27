package charger_test

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/Amraa1/plugyard/internal/charger"
	"github.com/Amraa1/plugyard/internal/csmstest"
	"github.com/Amraa1/plugyard/internal/ev"
	"github.com/Amraa1/plugyard/internal/model"
	"github.com/Amraa1/plugyard/internal/ocpp/v16"
)

// disk stands in for saved state: it keeps what the charger saves, and hands
// it back as it would come off a disk.
type disk struct {
	mu     sync.Mutex
	states [][]byte
}

func (d *disk) save(st charger.State) error {
	data, err := json.Marshal(st)
	if err != nil {
		return err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.states = append(d.states, data)
	return nil
}

func (d *disk) count() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.states)
}

// last returns the state saved most recently.
func (d *disk) last(t *testing.T) *charger.State {
	t.Helper()
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.states) == 0 {
		t.Fatal("nothing was saved")
	}
	st := &charger.State{}
	if err := json.Unmarshal(d.states[len(d.states)-1], st); err != nil {
		t.Fatal(err)
	}
	return st
}

// waitSaved waits until the last saved state satisfies ok.
func (d *disk) waitSaved(t *testing.T, what string, ok func(*charger.State) bool) *charger.State {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if d.count() > 0 {
			if st := d.last(t); ok(st) {
				return st
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("the charger never saved %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func startOnPlug(config map[string]string) *model.Model {
	return modelWith(func(m *model.Model) {
		m.StartOn = model.StartOnPlug
		m.StartTag = "FREEVEND"
		for key, value := range config {
			m.Config[key] = value
		}
	})
}

// unreachable is a CSMS URL nothing listens on.
const unreachable = "ws://127.0.0.1:1/ocpp"

func TestPowerCutIsReportedOnTheNextStart(t *testing.T) {
	csms := csmstest.Start(t)
	d := &disk{}
	m := startOnPlug(map[string]string{"MeterSaveInterval": "1"})
	r := online(t, csms, charger.Config{Model: m, Speed: 3600, Save: d.save})

	must(t)(r.Plug(ctx, 1, ev.Defaults()))
	csms.WaitForCalls(t, "CP-1", v16.ActionStartTransaction, 1)

	// Meter readings are saved every MeterSaveInterval seconds, with the
	// transaction still open.
	saved := d.waitSaved(t, "a meter reading during the transaction", func(st *charger.State) bool {
		return st.Connectors[0].Tx != nil && st.Connectors[0].Tx.ID == 1 && st.Connectors[0].MeterWh > 100
	})

	r.Kill()
	if ends := csms.WaitDisconnects(t, "CP-1", 1); ends[0].Clean {
		t.Error("the connection was closed normally, want it dropped as in a power cut")
	}
	r.stop()
	r.log.waitFor(t, charger.Event, "power cut")
	for _, call := range csms.Calls("CP-1") {
		if call.Action == v16.ActionStopTransaction {
			t.Fatal("the charger sent StopTransaction when its power was cut")
		}
	}
	// Nothing is saved once the power is gone.
	saved = d.last(t)
	if saved.Connectors[0].Tx == nil {
		t.Fatal("the saved state has no open transaction after a power cut")
	}
	if err := r.Run(context.Background()); err == nil {
		t.Error("Run succeeded a second time, want an error: a charger is powered on once")
	}

	// The same charger comes back from its saved state.
	r2 := online(t, csms, charger.Config{Model: m, Speed: 3600, Save: d.save, Restore: saved})
	r2.log.waitFor(t, charger.Event, "transaction 1 was cut short by a power loss")

	stop := stopRequest(t, csms)
	if stop.Reason != v16.ReasonPowerLoss || stop.TransactionID != 1 {
		t.Errorf("StopTransaction = %+v, want PowerLoss for transaction 1", stop)
	}
	if stop.MeterStop != int(saved.Connectors[0].MeterWh) {
		t.Errorf("meterStop = %d, want the last saved reading %d", stop.MeterStop, int(saved.Connectors[0].MeterWh))
	}
	if !stop.Timestamp.Time.Equal(saved.SavedAt.Truncate(time.Millisecond)) {
		t.Errorf("timestamp = %s, want the time of the last save %s", stop.Timestamp.Time, saved.SavedAt)
	}
	if len(stop.TransactionData) != 2 {
		t.Errorf("transactionData has %d readings, want the begin and the end", len(stop.TransactionData))
	}

	// The EV is still plugged in and the connector waits for it to leave. No
	// new transaction starts by itself.
	waitStatus(t, csms, 1, v16.StatusFinishing)
	snap := r2.Snapshot().Connectors[0]
	if snap.Status != v16.StatusFinishing || snap.SoC == nil || snap.TransactionID != 0 {
		t.Errorf("connector after the restart = %+v", snap)
	}
	if after := d.waitSaved(t, "the end of the transaction", func(st *charger.State) bool { return st.Connectors[0].Tx == nil }); !after.Connectors[0].Finishing {
		t.Error("the saved state does not say the connector is finishing")
	}

	// The meter never runs backwards: the next transaction starts where the
	// register stood.
	must(t)(r2.Unplug(1))
	must(t)(r2.Plug(ctx, 1, ev.Defaults()))
	var start v16.StartTransactionRequest
	csms.WaitForCalls(t, "CP-1", v16.ActionStartTransaction, 2)[1].Decode(t, &start)
	if start.MeterStart != stop.MeterStop {
		t.Errorf("the next meterStart = %d, want %d", start.MeterStart, stop.MeterStop)
	}
}

func TestQueuedMessagesSurviveAPowerCut(t *testing.T) {
	// The charger cannot reach the CSMS, so everything it has to say queues.
	d := &disk{}
	m := startOnPlug(nil)
	r := powerOn(t, charger.Config{ID: "CP-1", Model: m, CSMS: charger.CSMS{URL: unreachable}, Speed: 3600, Save: d.save})

	short, cancel := context.WithTimeout(ctx, 20*time.Millisecond)
	defer cancel()
	must(t)(r.Plug(short, 1, ev.Defaults()))
	saved := d.waitSaved(t, "the queued StartTransaction", func(st *charger.State) bool {
		return len(st.Outbox) == 1 && st.Outbox[0].Action == v16.ActionStartTransaction && st.Outbox[0].TxRef != 0
	})
	r.Kill()
	r.stop()

	// Back with power and a CSMS: the transaction is started and then
	// stopped, and the stop carries the ID the CSMS just gave.
	csms := csmstest.Start(t)
	online(t, csms, charger.Config{Model: m, Save: d.save, Restore: saved})

	stop := stopRequest(t, csms)
	if stop.Reason != v16.ReasonPowerLoss || stop.TransactionID != 1 {
		t.Errorf("StopTransaction = %+v, want PowerLoss with the transaction ID from the CSMS", stop)
	}
	var order []string
	for _, action := range csms.Actions("CP-1") {
		if action == v16.ActionStartTransaction || action == v16.ActionStopTransaction {
			order = append(order, action)
		}
	}
	if len(order) != 2 || order[0] != v16.ActionStartTransaction {
		t.Errorf("transaction messages arrived as %v, want StartTransaction then StopTransaction", order)
	}
	d.waitSaved(t, "an empty queue", func(st *charger.State) bool { return len(st.Outbox) == 0 })
}

func TestOrderlyShutdownWhileOfflineEndsTheTransaction(t *testing.T) {
	d := &disk{}
	m := startOnPlug(nil)
	r := powerOn(t, charger.Config{ID: "CP-1", Model: m, CSMS: charger.CSMS{URL: unreachable}, Speed: 3600, Save: d.save})

	short, cancel := context.WithTimeout(ctx, 20*time.Millisecond)
	defer cancel()
	must(t)(r.Plug(short, 1, ev.Defaults()))
	r.stop()

	saved := d.last(t)
	if saved.Connectors[0].Tx != nil || !saved.Connectors[0].Finishing || len(saved.Outbox) != 2 {
		t.Fatalf("saved state after an orderly shutdown = %+v, want no open transaction and two queued messages", saved)
	}
	if _, err := r.Unplug(1); err == nil {
		t.Error("an action on a charger without power succeeded")
	}

	// The CSMS hears about it after the next start, as an ordinary stop.
	csms := csmstest.Start(t)
	online(t, csms, charger.Config{Model: m, Save: d.save, Restore: saved})
	if stop := stopRequest(t, csms); stop.Reason != v16.ReasonOther || stop.TransactionID != 1 {
		t.Errorf("StopTransaction = %+v, want reason Other for transaction 1", stop)
	}
}

func TestStateOfAChargerThatIsOff(t *testing.T) {
	m := startOnPlug(nil)

	// A charger that has never run shows every connector as Available.
	var none *charger.State
	snap := none.Snapshot("CP-1", m, nil)
	if snap.Connection != charger.Stopped || len(snap.Connectors) != 2 || snap.Connectors[0].Status != v16.StatusAvailable {
		t.Errorf("snapshot of a charger that never ran = %+v", snap)
	}

	// One that stopped with an EV plugged in will report Finishing.
	st := &charger.State{Connectors: []charger.ConnectorState{
		{ID: 1, MeterWh: 1234.9, EV: &ev.State{Params: ev.Defaults(), SoC: 0.5}, Finishing: true},
		{ID: 2, EV: &ev.State{Params: ev.Defaults(), SoC: 0.2}},
	}}
	snap = st.Snapshot("CP-1", m, nil)
	if k := snap.Connectors[0]; k.Status != v16.StatusFinishing || k.MeterWh != 1234 || k.SoC == nil || *k.SoC != 0.5 {
		t.Errorf("connector 1 = %+v", k)
	}
	if k := snap.Connectors[1]; k.Status != v16.StatusPreparing {
		t.Errorf("connector 2 = %+v, want Preparing: an EV is plugged in and no transaction has run", k)
	}
}
