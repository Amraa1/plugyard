package charger_test

import (
	"strings"
	"testing"
	"time"

	"github.com/Amraa1/plugyard/internal/charger"
	"github.com/Amraa1/plugyard/internal/csmstest"
	"github.com/Amraa1/plugyard/internal/ev"
	"github.com/Amraa1/plugyard/internal/model"
	"github.com/Amraa1/plugyard/internal/ocpp/v16"
)

// declaredEV is an EV a site file declares: 40 kWh, half full.
func declaredEV() *charger.DeclaredEV {
	params := ev.Defaults()
	params.BatteryWh, params.SoC = 40000, 0.5
	return &charger.DeclaredEV{Preset: "compact", Params: params}
}

func TestAChargerPowersOnInItsDeclaredState(t *testing.T) {
	csms := csmstest.Start(t)
	r := online(t, csms, charger.Config{Declared: &charger.Declared{Connectors: []charger.DeclaredConnector{
		{ID: 1, EV: declaredEV()},
		{ID: 2, Fault: &charger.Fault{Code: "GroundFailure", Info: "RCD tripped", VendorCode: "E42"}},
	}}})

	waitStatus(t, csms, 1, v16.StatusPreparing)
	waitStatus(t, csms, 2, v16.StatusFaulted)
	if st := lastStatus(t, csms, 2); st.ErrorCode != "GroundFailure" || st.Info != "RCD tripped" || st.VendorErrorCode != "E42" {
		t.Errorf("connector 2 reported %+v, want the declared fault", st)
	}
	if k := r.Snapshot().Connectors[0]; k.SoC == nil || *k.SoC != 0.5 {
		t.Errorf("connector 1 = %+v, want the declared EV at 50%%", k)
	}
	// With a tag the EV charges like any other.
	must(t)(r.Tap(ctx, 1, "TAG-A"))
	waitStatus(t, csms, 1, v16.StatusCharging)
}

func TestDeclaredStateTakesThePlaceOfSavedState(t *testing.T) {
	csms := csmstest.Start(t)
	saved := &charger.State{
		Fault: &charger.Fault{Code: "InternalError"},
		Connectors: []charger.ConnectorState{
			// Its transaction ended and its EV stayed, and then it faulted.
			{ID: 1, MeterWh: 1500, EV: &ev.State{Params: ev.Defaults(), SoC: 0.63}, Finishing: true, Fault: &charger.Fault{Code: "OverVoltage"}},
			{ID: 2, EV: &ev.State{Params: ev.Defaults(), SoC: 0.4}, Finishing: true},
		},
	}
	r := online(t, csms, charger.Config{Restore: saved, Declared: &charger.Declared{Connectors: []charger.DeclaredConnector{
		{ID: 0},
		{ID: 1, EV: declaredEV()},
	}}})

	// Connector 1 is declared whole: a fresh EV, and no fault.
	waitStatus(t, csms, 1, v16.StatusPreparing)
	snap := r.Snapshot()
	if len(snap.Connectors) != 2 {
		t.Fatalf("snapshot = %+v, want no fault on the charger as a whole", snap)
	}
	if k := snap.Connectors[0]; k.SoC == nil || *k.SoC != 0.5 || k.ErrorCode != "" || k.MeterWh != 1500 {
		t.Errorf("connector 1 = %+v, want the declared EV at 50%%, no fault and the meter untouched", k)
	}
	// Connector 2 is not listed and follows the saved state.
	if k := snap.Connectors[1]; k.Status != v16.StatusFinishing || k.SoC == nil || *k.SoC != 0.4 {
		t.Errorf("connector 2 = %+v, want the saved EV at 40%%, Finishing", k)
	}
}

func TestATransactionCutShortEndsBeforeTheDeclaredEVArrives(t *testing.T) {
	csms := csmstest.Start(t)
	// The charger lost power in the middle of transaction 7.
	saved := &charger.State{Connectors: []charger.ConnectorState{{
		ID: 1, MeterWh: 2500,
		EV: &ev.State{Params: ev.Defaults(), SoC: 0.63},
		Tx: &charger.TxState{Ref: 1, ID: 7, IDTag: "TAG-A"},
	}}}
	r := online(t, csms, charger.Config{Restore: saved, Declared: &charger.Declared{Connectors: []charger.DeclaredConnector{
		{ID: 1, EV: declaredEV()},
	}}})

	// The CSMS is told of the power loss, with the reading that was saved.
	if stop := stopRequest(t, csms); stop.TransactionID != 7 || stop.Reason != v16.ReasonPowerLoss || stop.MeterStop != 2500 {
		t.Errorf("StopTransaction = %+v, want transaction 7 ended by PowerLoss at 2500 Wh", stop)
	}
	// The declared EV is a new arrival: the connector is not Finishing.
	if k := r.Snapshot().Connectors[0]; k.Status != v16.StatusPreparing || k.SoC == nil || *k.SoC != 0.5 {
		t.Errorf("connector 1 = %+v, want Preparing with the declared EV at 50%%", k)
	}
}

func TestAConnectorDeclaredWithNothingIsAvailable(t *testing.T) {
	csms := csmstest.Start(t)
	saved := &charger.State{Connectors: []charger.ConnectorState{
		{ID: 1, EV: &ev.State{Params: ev.Defaults(), SoC: 0.63}, Fault: &charger.Fault{Code: "OverVoltage"}},
		// What the CSMS set is not the site file's to change.
		{ID: 2, EV: &ev.State{Params: ev.Defaults(), SoC: 0.4}, Inoperative: true},
	}}
	r := online(t, csms, charger.Config{Restore: saved, Declared: &charger.Declared{Connectors: []charger.DeclaredConnector{
		{ID: 1}, {ID: 2},
	}}})

	snap := r.Snapshot()
	if k := snap.Connectors[0]; k.Status != v16.StatusAvailable || k.SoC != nil || k.ErrorCode != "" {
		t.Errorf("connector 1 = %+v, want Available with nothing plugged in", k)
	}
	if k := snap.Connectors[1]; k.Status != v16.StatusUnavailable || k.SoC != nil {
		t.Errorf("connector 2 = %+v, want it still Unavailable, with nothing plugged in", k)
	}
}

func TestADeclaredFaultOnTheChargerAsAWhole(t *testing.T) {
	csms := csmstest.Start(t)
	r := online(t, csms, charger.Config{Declared: &charger.Declared{Connectors: []charger.DeclaredConnector{
		{ID: 0, Fault: &charger.Fault{Code: "InternalError"}},
	}}})

	waitStatus(t, csms, 0, v16.StatusFaulted)
	if st := lastStatus(t, csms, 0); st.ErrorCode != "InternalError" {
		t.Errorf("the charger reported %+v, want InternalError", st)
	}
	// It is a fault like any other: it can be cleared by hand.
	must(t)(r.ClearFault(0))
	waitStatus(t, csms, 0, v16.StatusAvailable)
}

func TestADeclaredEVStartsATransactionWhenTheModelStartsOnPlug(t *testing.T) {
	csms := csmstest.Start(t)
	online(t, csms, charger.Config{
		Model: modelWith(func(m *model.Model) {
			m.StartOn = model.StartOnPlug
			m.StartTag = "FREEVEND"
		}),
		Declared: &charger.Declared{Connectors: []charger.DeclaredConnector{{ID: 1, EV: declaredEV()}}},
	})

	var start v16.StartTransactionRequest
	csms.WaitForCalls(t, "CP-1", v16.ActionStartTransaction, 1)[0].Decode(t, &start)
	if start.ConnectorID != 1 || start.IDTag != "FREEVEND" {
		t.Errorf("StartTransaction = %+v, want connector 1 with the model's start tag", start)
	}
	waitStatus(t, csms, 1, v16.StatusCharging)
}

func TestAChargerDeclaredOfflineStaysAwayUntilToldOtherwise(t *testing.T) {
	csms := csmstest.Start(t)
	r := powerOn(t, charger.Config{
		ID:       "CP-1",
		CSMS:     charger.CSMS{URL: csms.URL()},
		Declared: &charger.Declared{Connection: charger.Offline},
	})

	r.log.waitFor(t, charger.Event, "offline, as its site file declares")
	time.Sleep(100 * time.Millisecond)
	if n := csms.Connects("CP-1"); n != 0 {
		t.Fatalf("the charger connected %d times, want none while it is declared offline", n)
	}
	if got := r.Snapshot().Connection; got != charger.Offline {
		t.Errorf("connection = %s, want offline", got)
	}

	// A reboot is not a power-on, and does not bring the charger back.
	must(t)(r.Reboot())
	time.Sleep(100 * time.Millisecond)
	if n := csms.Connects("CP-1"); n != 0 {
		t.Fatalf("the charger connected %d times after a reboot, want none", n)
	}

	// A change made by hand holds until the charger next powers on.
	must(t)(r.Online())
	csms.WaitForCalls(t, "CP-1", v16.ActionBootNotification, 1)
}

func TestNewRejectsADeclaredStateThatDoesNotFitTheModel(t *testing.T) {
	_, err := charger.New(charger.Config{
		ID:    "CP-1",
		Model: twoConnectorModel(),
		CSMS:  charger.CSMS{URL: "ws://localhost/ocpp"},
		Declared: &charger.Declared{Connectors: []charger.DeclaredConnector{
			{ID: 9}, {ID: 1, Fault: &charger.Fault{Code: "Boom"}},
		}},
	})
	if err == nil {
		t.Fatal("New succeeded, want an error")
	}
	for _, want := range []string{"the model has no connector 9; it has 1 and 2", `unknown error code "Boom"`} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not mention %q:\n%v", want, err)
		}
	}
}

func TestSnapshotOfAStoppedChargerShowsItsDeclaredState(t *testing.T) {
	m := twoConnectorModel()
	declared := &charger.Declared{Connectors: []charger.DeclaredConnector{
		{ID: 0, Fault: &charger.Fault{Code: "InternalError"}},
		{ID: 1, EV: declaredEV()},
	}}

	// A charger that has never run shows what it will report when started.
	var none *charger.State
	snap := none.Snapshot("CP-1", m, declared)
	if len(snap.Connectors) != 3 || snap.Connectors[0].Status != v16.StatusFaulted || snap.Connectors[0].ErrorCode != "InternalError" {
		t.Fatalf("snapshot = %+v, want the charger as a whole Faulted first", snap)
	}
	if k := snap.Connectors[1]; k.Status != v16.StatusPreparing || k.SoC == nil || *k.SoC != 0.5 {
		t.Errorf("connector 1 = %+v, want Preparing with the declared EV", k)
	}

	// On a model that starts on plug-in the declared EV brings a
	// transaction, unless a fault stands in the way.
	onPlug := modelWith(func(m *model.Model) { m.StartOn = model.StartOnPlug })
	arriving := &charger.Declared{Connectors: []charger.DeclaredConnector{
		{ID: 1, EV: declaredEV()},
		{ID: 2, EV: declaredEV(), Fault: &charger.Fault{Code: "GroundFailure"}},
	}}
	snap = none.Snapshot("CP-1", onPlug, arriving)
	if snap.Connectors[0].Status != v16.StatusCharging || snap.Connectors[1].Status != v16.StatusFaulted {
		t.Errorf("snapshot = %+v, want connector 1 Charging and connector 2 Faulted", snap.Connectors)
	}
	if snap = none.Snapshot("CP-1", onPlug, declared); snap.Connectors[1].Status != v16.StatusPreparing {
		t.Errorf("connector 1 = %+v, want Preparing while the charger as a whole has a fault", snap.Connectors[1])
	}

	// Declared state wins over what is saved, and leaves the rest alone.
	saved := &charger.State{
		Connectors: []charger.ConnectorState{
			{ID: 1, MeterWh: 900, EV: &ev.State{Params: ev.Defaults(), SoC: 0.63}, Finishing: true, Fault: &charger.Fault{Code: "OverVoltage"}},
			{ID: 2, EV: &ev.State{Params: ev.Defaults(), SoC: 0.4}, Finishing: true},
		},
	}
	snap = saved.Snapshot("CP-1", m, &charger.Declared{Connectors: []charger.DeclaredConnector{{ID: 1}}})
	if len(snap.Connectors) != 2 {
		t.Fatalf("snapshot = %+v, want two connectors", snap)
	}
	if k := snap.Connectors[0]; k.Status != v16.StatusAvailable || k.SoC != nil || k.ErrorCode != "" || k.MeterWh != 900 {
		t.Errorf("connector 1 = %+v, want Available, as declared, with its meter", k)
	}
	if k := snap.Connectors[1]; k.Status != v16.StatusFinishing {
		t.Errorf("connector 2 = %+v, want Finishing, as saved", k)
	}
}
