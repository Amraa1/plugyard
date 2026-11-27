package charger_test

import (
	"slices"
	"testing"
	"time"

	"github.com/Amraa1/plugyard/internal/charger"
	"github.com/Amraa1/plugyard/internal/csmstest"
	"github.com/Amraa1/plugyard/internal/ev"
	"github.com/Amraa1/plugyard/internal/model"
	"github.com/Amraa1/plugyard/internal/ocpp/v16"
)

// renewTime is how long a full EV stays, and how long its connector then
// stays empty, in these tests.
const renewTime = 80 * time.Millisecond

// almostFull declares, on connector 1, an EV that is 600 Wh short of its
// target and is renewed when it is done.
func almostFull() *charger.Declared {
	params := ev.Defaults()
	params.SoC, params.TargetSoC = 0.79, 0.80
	return &charger.Declared{Connectors: []charger.DeclaredConnector{
		{ID: 1, EV: &charger.DeclaredEV{Preset: "compact", Params: params, Renew: renewTime}},
	}}
}

// reported waits until the statuses reported for a connector are exactly
// the ones given, in order.
func reported(t *testing.T, csms *csmstest.CSMS, connector int, want ...string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		got := statuses(t, csms, connector)
		if slices.Equal(got, want) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("connector %d reported %v, want %v", connector, got, want)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestAFullDeclaredEVIsRenewed(t *testing.T) {
	csms := csmstest.Start(t)
	r := online(t, csms, charger.Config{Speed: 3600, Declared: almostFull()})

	must(t)(r.Tap(ctx, 1, "TAG-A"))
	stopRequest(t, csms)
	left := time.Now()

	// The full EV leaves, and after a while a fresh one arrives.
	reported(t, csms, 1,
		v16.StatusPreparing, v16.StatusCharging, v16.StatusFinishing, v16.StatusAvailable, v16.StatusPreparing)
	// It waits the renew time twice: once to leave, once more to arrive.
	// One wait alone would be well under one and a half times that.
	if took := time.Since(left); took < renewTime*3/2 {
		t.Errorf("the EV was renewed %s after its transaction ended, want the renew time (%s) twice over", took, renewTime)
	}
	if k := r.Snapshot().Connectors[0]; k.SoC == nil || *k.SoC != 0.79 {
		t.Errorf("connector 1 = %+v, want a fresh EV at 79%%", k)
	}
	r.log.waitFor(t, charger.Event, "connector 1: the EV is full and left; a fresh one arrives in 80ms")
	r.log.waitFor(t, charger.Event, "connector 1: a fresh EV plugged in (60 kWh, 79%)")

	// The fresh EV is renewed in its turn.
	must(t)(r.Tap(ctx, 1, "TAG-A"))
	csms.WaitForCalls(t, "CP-1", v16.ActionStopTransaction, 2)
	reported(t, csms, 1,
		v16.StatusPreparing, v16.StatusCharging, v16.StatusFinishing, v16.StatusAvailable,
		v16.StatusPreparing, v16.StatusCharging, v16.StatusFinishing, v16.StatusAvailable, v16.StatusPreparing)
}

func TestARenewedEVStartsATransactionWhenTheModelStartsOnPlug(t *testing.T) {
	csms := csmstest.Start(t)
	online(t, csms, charger.Config{
		Speed: 3600,
		Model: modelWith(func(m *model.Model) {
			m.StartOn = model.StartOnPlug
			m.StartTag = "FREEVEND"
		}),
		Declared: almostFull(),
	})

	// Nobody taps: each EV that arrives charges, fills up and is renewed.
	starts := csms.WaitForCalls(t, "CP-1", v16.ActionStartTransaction, 3)
	var third v16.StartTransactionRequest
	starts[2].Decode(t, &third)
	if third.ConnectorID != 1 || third.IDTag != "FREEVEND" {
		t.Errorf("third StartTransaction = %+v, want connector 1 with the model's start tag", third)
	}
}

func TestAnEVLeftSuspendedIsNotRenewed(t *testing.T) {
	csms := csmstest.Start(t)
	r := online(t, csms, charger.Config{
		Speed:    3600,
		Model:    modelWith(func(m *model.Model) { m.OnEvFull = model.OnEvFullSuspend }),
		Declared: almostFull(),
	})

	must(t)(r.Tap(ctx, 1, "TAG-A"))
	waitStatus(t, csms, 1, v16.StatusSuspendedEV)

	// Its transaction is still open, so the EV is not done.
	time.Sleep(4 * renewTime)
	if got := r.Snapshot().Connectors[0].Status; got != v16.StatusSuspendedEV {
		t.Errorf("connector 1 is %s, want it to stay SuspendedEV", got)
	}
}

func TestOnlyTheDeclaredEVIsRenewed(t *testing.T) {
	csms := csmstest.Start(t)
	r := online(t, csms, charger.Config{Declared: almostFull()})

	// Unplugged by hand, the declared EV does not come back by itself.
	must(t)(r.Unplug(1))
	time.Sleep(4 * renewTime)
	if got := r.Snapshot().Connectors[0].Status; got != v16.StatusAvailable {
		t.Fatalf("connector 1 is %s, want it to stay Available after an unplug by hand", got)
	}

	// An EV plugged in by hand stays, full as it is.
	full := ev.Defaults()
	full.SoC, full.TargetSoC = 0.80, 0.80
	must(t)(r.Plug(ctx, 1, full))
	time.Sleep(4 * renewTime)
	if got := r.Snapshot().Connectors[0].Status; got != v16.StatusPreparing {
		t.Errorf("connector 1 is %s, want the EV plugged in by hand to stay", got)
	}
}

func TestAnEVPluggedInByHandWhileAFreshOneIsAwaitedStays(t *testing.T) {
	csms := csmstest.Start(t)
	declared := almostFull()
	declared.Connectors[0].EV.Renew = 400 * time.Millisecond
	r := online(t, csms, charger.Config{Speed: 3600, Declared: declared})

	must(t)(r.Tap(ctx, 1, "TAG-A"))
	stopRequest(t, csms)
	waitStatus(t, csms, 1, v16.StatusAvailable)

	// Someone plugs in before the fresh EV is due: theirs is the EV now.
	params := ev.Defaults()
	params.SoC = 0.33
	must(t)(r.Plug(ctx, 1, params))
	time.Sleep(600 * time.Millisecond)
	if k := r.Snapshot().Connectors[0]; k.Status != v16.StatusPreparing || k.SoC == nil || *k.SoC != 0.33 {
		t.Errorf("connector 1 = %+v, want the EV plugged in by hand, at 33%%", k)
	}
}
