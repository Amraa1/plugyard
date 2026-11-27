package charger_test

import (
	"context"
	"encoding/json"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Amraa1/plugyard/internal/charger"
	"github.com/Amraa1/plugyard/internal/csmstest"
	"github.com/Amraa1/plugyard/internal/ev"
	"github.com/Amraa1/plugyard/internal/model"
	"github.com/Amraa1/plugyard/internal/ocpp/ocppj"
	"github.com/Amraa1/plugyard/internal/ocpp/v16"
)

var ctx = context.Background()

// modelWith returns the two-connector model with extra settings applied.
func modelWith(change func(*model.Model)) *model.Model {
	m := twoConnectorModel()
	m.OnEvFull = model.OnEvFullStop
	m.StartOn = model.StartOnTap
	change(m)
	return m
}

// online powers a charger on against csms and waits until it has booted.
func online(t *testing.T, csms *csmstest.CSMS, cfg charger.Config) *running {
	t.Helper()
	cfg.ID = "CP-1"
	cfg.CSMS = charger.CSMS{URL: csms.URL()}
	if cfg.Model == nil {
		cfg.Model = modelWith(func(*model.Model) {})
	}
	r := powerOn(t, cfg)
	csms.WaitForCalls(t, "CP-1", v16.ActionStatusNotification, len(cfg.Model.Connectors))
	return r
}

// waitStatus waits until the charger has reported the given status for a
// connector.
func waitStatus(t *testing.T, csms *csmstest.CSMS, connector int, status string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		var seen []string
		for _, call := range csms.Calls("CP-1") {
			if call.Action != v16.ActionStatusNotification {
				continue
			}
			var req v16.StatusNotificationRequest
			call.Decode(t, &req)
			if req.ConnectorID == connector {
				seen = append(seen, req.Status)
				if req.Status == status {
					return
				}
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("connector %d never reported %s; it reported %v", connector, status, seen)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// statuses returns every status reported for a connector, in order.
func statuses(t *testing.T, csms *csmstest.CSMS, connector int) []string {
	t.Helper()
	var out []string
	for _, call := range csms.Calls("CP-1") {
		if call.Action != v16.ActionStatusNotification {
			continue
		}
		var req v16.StatusNotificationRequest
		call.Decode(t, &req)
		if req.ConnectorID == connector {
			out = append(out, req.Status)
		}
	}
	return out
}

// waitStatuses waits until the statuses reported for a connector are exactly
// the ones given, in order.
func waitStatuses(t *testing.T, csms *csmstest.CSMS, connector int, want ...string) {
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

// finished leaves a connector Finishing: a transaction started with TAG-A
// has ended there and the EV is still plugged in.
func finished(t *testing.T, csms *csmstest.CSMS, r *running, connector int) {
	t.Helper()
	must(t)(r.Plug(ctx, connector, ev.Defaults()))
	must(t)(r.Tap(ctx, connector, "TAG-A"))
	must(t)(r.Tap(ctx, connector, "TAG-A"))
	waitStatus(t, csms, connector, v16.StatusFinishing)
}

// must wraps the result of an action, failing the test if the action could
// not be carried out: must(t)(r.Plug(...)).
func must(t *testing.T) func(outcome string, err error) string {
	return func(outcome string, err error) string {
		t.Helper()
		if err != nil {
			t.Fatalf("action failed: %v", err)
		}
		return outcome
	}
}

func stopRequest(t *testing.T, csms *csmstest.CSMS) v16.StopTransactionRequest {
	t.Helper()
	var req v16.StopTransactionRequest
	csms.WaitForCalls(t, "CP-1", v16.ActionStopTransaction, 1)[0].Decode(t, &req)
	return req
}

func TestPlugThenTapStartsATransaction(t *testing.T) {
	csms := csmstest.Start(t)
	r := online(t, csms, charger.Config{})

	outcome := must(t)(r.Plug(ctx, 1, ev.Defaults()))
	if outcome != "EV plugged in (60 kWh, 20%)" {
		t.Errorf("plug outcome = %q", outcome)
	}
	waitStatus(t, csms, 1, v16.StatusPreparing)

	outcome = must(t)(r.Tap(ctx, 1, "TAG-A"))
	if outcome != "tag accepted, transaction 1 started" {
		t.Errorf("tap outcome = %q", outcome)
	}
	waitStatus(t, csms, 1, v16.StatusCharging)

	var auth v16.AuthorizeRequest
	csms.WaitForCalls(t, "CP-1", v16.ActionAuthorize, 1)[0].Decode(t, &auth)
	if auth.IDTag != "TAG-A" {
		t.Errorf("Authorize idTag = %q, want TAG-A", auth.IDTag)
	}

	var start v16.StartTransactionRequest
	csms.WaitForCalls(t, "CP-1", v16.ActionStartTransaction, 1)[0].Decode(t, &start)
	if start.ConnectorID != 1 || start.IDTag != "TAG-A" || start.MeterStart != 0 {
		t.Errorf("StartTransaction = %+v, want connector 1, tag TAG-A, meterStart 0", start)
	}

	// The other connector is untouched.
	if got := statuses(t, csms, 2); len(got) != 1 || got[0] != v16.StatusAvailable {
		t.Errorf("connector 2 reported %v, want only Available", got)
	}

	snap := r.Snapshot()
	if snap.Connection != charger.Online || snap.Connectors[0].Status != v16.StatusCharging || snap.Connectors[0].TransactionID != 1 {
		t.Errorf("snapshot = %+v", snap)
	}
}

func TestTapThenPlugStartsATransaction(t *testing.T) {
	csms := csmstest.Start(t)
	r := online(t, csms, charger.Config{})

	outcome := must(t)(r.Tap(ctx, 1, "TAG-A"))
	if !strings.HasPrefix(outcome, "tag accepted, waiting") {
		t.Errorf("tap outcome = %q", outcome)
	}
	waitStatus(t, csms, 1, v16.StatusPreparing)
	if n := len(csms.WaitForCalls(t, "CP-1", v16.ActionAuthorize, 1)); n != 1 {
		t.Errorf("got %d Authorize calls, want 1", n)
	}

	outcome = must(t)(r.Plug(ctx, 1, ev.Defaults()))
	if !strings.HasSuffix(outcome, "transaction 1 started") {
		t.Errorf("plug outcome = %q", outcome)
	}
	waitStatus(t, csms, 1, v16.StatusCharging)
}

func TestAcceptedTagIsForgottenAfterConnectionTimeOut(t *testing.T) {
	csms := csmstest.Start(t)
	r := online(t, csms, charger.Config{Model: modelWith(func(m *model.Model) {
		m.Config["ConnectionTimeOut"] = "1"
	})})

	must(t)(r.Tap(ctx, 1, "TAG-A"))
	waitStatus(t, csms, 1, v16.StatusPreparing)
	tapped := time.Now()

	// Back to Available by itself: the third status after Available and
	// Preparing.
	deadline := time.Now().Add(5 * time.Second)
	for len(statuses(t, csms, 1)) < 3 {
		if time.Now().After(deadline) {
			t.Fatalf("connector 1 reported %v, want it to return to Available", statuses(t, csms, 1))
		}
		time.Sleep(5 * time.Millisecond)
	}
	if got := statuses(t, csms, 1)[2]; got != v16.StatusAvailable {
		t.Errorf("status after the timeout = %s, want Available", got)
	}
	if waited := time.Since(tapped); waited < 900*time.Millisecond {
		t.Errorf("tag was forgotten after %s, want about 1s", waited)
	}

	// A plug-in now does not start a transaction.
	must(t)(r.Plug(ctx, 1, ev.Defaults()))
	time.Sleep(100 * time.Millisecond)
	if n := len(csms.Calls("CP-1")); n > 0 {
		for _, call := range csms.Calls("CP-1") {
			if call.Action == v16.ActionStartTransaction {
				t.Error("a transaction started after the tag had been forgotten")
			}
		}
	}
}

func TestEnergyFlowsAndMeterValuesAreSent(t *testing.T) {
	csms := csmstest.Start(t)
	// At 360x, one real second is six simulated minutes: 5 kWh at 50 kW.
	r := online(t, csms, charger.Config{
		Speed: 360,
		Model: modelWith(func(m *model.Model) {
			m.Config["MeterValueSampleInterval"] = "1"
			m.Config["MeterValuesSampledData"] = "Energy.Active.Import.Register, Power.Active.Import, SoC"
		}),
	})

	must(t)(r.Plug(ctx, 1, ev.Defaults()))
	must(t)(r.Tap(ctx, 1, "TAG-A"))

	calls := csms.WaitForCalls(t, "CP-1", v16.ActionMeterValues, 2)

	read := func(call csmstest.Call) map[string]v16.SampledValue {
		var req v16.MeterValuesRequest
		call.Decode(t, &req)
		if req.ConnectorID != 1 || req.TransactionID == nil || *req.TransactionID != 1 {
			t.Errorf("MeterValues is for connector %d, transaction %v; want connector 1, transaction 1", req.ConnectorID, req.TransactionID)
		}
		if len(req.MeterValue) != 1 {
			t.Fatalf("MeterValues has %d meterValue entries, want 1", len(req.MeterValue))
		}
		values := map[string]v16.SampledValue{}
		for _, v := range req.MeterValue[0].SampledValue {
			if v.Context != v16.ContextSamplePeriodic {
				t.Errorf("%s has context %q, want Sample.Periodic", v.Measurand, v.Context)
			}
			values[v.Measurand] = v
		}
		return values
	}
	number := func(v v16.SampledValue) int {
		n, err := strconv.Atoi(v.Value)
		if err != nil {
			t.Fatalf("%s value %q is not a whole number", v.Measurand, v.Value)
		}
		return n
	}

	first, second := read(calls[0]), read(calls[1])

	energy1, energy2 := number(first[v16.MeasurandEnergy]), number(second[v16.MeasurandEnergy])
	if first[v16.MeasurandEnergy].Unit != "Wh" {
		t.Errorf("energy unit = %q, want Wh", first[v16.MeasurandEnergy].Unit)
	}
	// About 5 kWh per second; allow for scheduling.
	if energy1 < 3000 || energy1 > 8000 {
		t.Errorf("energy after one interval = %d Wh, want about 5000", energy1)
	}
	if gained := energy2 - energy1; gained < 3000 || gained > 8000 {
		t.Errorf("energy gained in the second interval = %d Wh, want about 5000", gained)
	}

	if power := number(first[v16.MeasurandPower]); power < 45000 || power > 55000 {
		t.Errorf("power = %d W, want about 50000", power)
	}
	if first[v16.MeasurandPower].Unit != "W" {
		t.Errorf("power unit = %q, want W", first[v16.MeasurandPower].Unit)
	}

	soc1, soc2 := number(first[v16.MeasurandSoC]), number(second[v16.MeasurandSoC])
	if soc1 <= 20 || soc2 <= soc1 {
		t.Errorf("SoC went %d%% then %d%%, want it rising from 20%%", soc1, soc2)
	}

	if meter := r.Snapshot().Connectors[0].MeterWh; meter < float64(energy2) {
		t.Errorf("snapshot meter = %.0f Wh, want at least %d", meter, energy2)
	}
}

func TestDefaultMeterValuesLeaveOutSoC(t *testing.T) {
	csms := csmstest.Start(t)
	r := online(t, csms, charger.Config{Model: modelWith(func(m *model.Model) {
		m.Config["MeterValueSampleInterval"] = "1"
	})})

	must(t)(r.Plug(ctx, 1, ev.Defaults()))
	must(t)(r.Tap(ctx, 1, "TAG-A"))

	var req v16.MeterValuesRequest
	csms.WaitForCalls(t, "CP-1", v16.ActionMeterValues, 1)[0].Decode(t, &req)
	var measurands []string
	for _, v := range req.MeterValue[0].SampledValue {
		measurands = append(measurands, v.Measurand)
	}
	if got := strings.Join(measurands, ","); got != "Energy.Active.Import.Register,Power.Active.Import" {
		t.Errorf("measurands = %s, want energy and power only", got)
	}
}

func TestSecondTapOfTheSameTagStopsTheTransaction(t *testing.T) {
	csms := csmstest.Start(t)
	r := online(t, csms, charger.Config{Speed: 360})

	must(t)(r.Plug(ctx, 1, ev.Defaults()))
	must(t)(r.Tap(ctx, 1, "TAG-A"))
	waitStatus(t, csms, 1, v16.StatusCharging)
	time.Sleep(300 * time.Millisecond) // let some energy flow

	outcome := must(t)(r.Tap(ctx, 1, "TAG-A"))
	if outcome != "transaction 1 stopped" {
		t.Errorf("tap outcome = %q", outcome)
	}

	stop := stopRequest(t, csms)
	if stop.TransactionID != 1 || stop.IDTag != "TAG-A" || stop.Reason != v16.ReasonLocal {
		t.Errorf("StopTransaction = %+v, want transaction 1, tag TAG-A, reason Local", stop)
	}
	if stop.MeterStop < 500 {
		t.Errorf("meterStop = %d Wh, want the energy delivered in 300ms at 360x (about 1500)", stop.MeterStop)
	}

	// transactionData carries the register at the start and at the end.
	if len(stop.TransactionData) != 2 {
		t.Fatalf("transactionData has %d entries, want 2", len(stop.TransactionData))
	}
	begin, end := stop.TransactionData[0].SampledValue[0], stop.TransactionData[1].SampledValue[0]
	if begin.Context != v16.ContextTransactionBegin || begin.Value != "0" || begin.Measurand != v16.MeasurandEnergy {
		t.Errorf("first transactionData value = %+v, want Transaction.Begin with 0 Wh", begin)
	}
	if end.Context != v16.ContextTransactionEnd || end.Value != strconv.Itoa(stop.MeterStop) {
		t.Errorf("last transactionData value = %+v, want Transaction.End with %d Wh", end, stop.MeterStop)
	}

	// Only one Authorize: stopping with the starting tag needs none.
	if n := len(csms.WaitForCalls(t, "CP-1", v16.ActionAuthorize, 1)); n != 1 {
		t.Errorf("got %d Authorize calls, want 1", n)
	}

	// The EV is still plugged in, so the connector is Finishing.
	waitStatus(t, csms, 1, v16.StatusFinishing)

	must(t)(r.Unplug(1))
	if got := r.Snapshot().Connectors[0].Status; got != v16.StatusAvailable {
		t.Errorf("status after unplugging = %s, want Available", got)
	}

	// The meter register keeps its reading for the next transaction.
	must(t)(r.Plug(ctx, 1, ev.Defaults()))
	must(t)(r.Tap(ctx, 1, "TAG-A"))
	var second v16.StartTransactionRequest
	csms.WaitForCalls(t, "CP-1", v16.ActionStartTransaction, 2)[1].Decode(t, &second)
	if second.MeterStart != stop.MeterStop {
		t.Errorf("second transaction has meterStart %d, want %d where the first one stopped", second.MeterStart, stop.MeterStop)
	}
}

func TestTapOnAFinishingConnectorStartsANewTransaction(t *testing.T) {
	csms := csmstest.Start(t)
	r := online(t, csms, charger.Config{})
	finished(t, csms, r, 1)

	// The EV was never unplugged, and a tap starts charging again.
	if outcome := must(t)(r.Tap(ctx, 1, "TAG-B")); outcome != "tag accepted, transaction 2 started" {
		t.Errorf("tap outcome = %q", outcome)
	}
	var start v16.StartTransactionRequest
	csms.WaitForCalls(t, "CP-1", v16.ActionStartTransaction, 2)[1].Decode(t, &start)
	if start.ConnectorID != 1 || start.IDTag != "TAG-B" {
		t.Errorf("second StartTransaction = %+v, want connector 1 with tag TAG-B", start)
	}

	// OCPP 1.6 has no step from Finishing to Charging: the connector goes
	// through Preparing.
	waitStatuses(t, csms, 1,
		v16.StatusAvailable, v16.StatusPreparing, v16.StatusCharging,
		v16.StatusFinishing, v16.StatusPreparing, v16.StatusCharging)
}

func TestRejectedTagLeavesAFinishingConnectorFinishing(t *testing.T) {
	csms := csmstest.Start(t)
	csms.Handle(v16.ActionAuthorize, func(_ string, payload json.RawMessage) (any, *ocppj.CallError) {
		var req v16.AuthorizeRequest
		if err := json.Unmarshal(payload, &req); err != nil {
			return nil, &ocppj.CallError{Code: ocppj.ErrFormationViolation}
		}
		status := v16.AuthorizationAccepted
		if req.IDTag == "TAG-BAD" {
			status = "Invalid"
		}
		return v16.AuthorizeResponse{IDTagInfo: v16.IDTagInfo{Status: status}}, nil
	})
	r := online(t, csms, charger.Config{})
	finished(t, csms, r, 1)

	if outcome := must(t)(r.Tap(ctx, 1, "TAG-BAD")); outcome != "tag rejected by the CSMS (Invalid)" {
		t.Errorf("tap outcome = %q", outcome)
	}
	time.Sleep(100 * time.Millisecond)
	if n := count(csms, v16.ActionStartTransaction); n != 1 {
		t.Errorf("got %d StartTransaction, want only the first: the second tag was rejected", n)
	}
	if got := r.Snapshot().Connectors[0].Status; got != v16.StatusFinishing {
		t.Errorf("connector status = %s, want it to stay Finishing", got)
	}
	// Preparing is not reported for a tag that was not accepted.
	want := []string{v16.StatusAvailable, v16.StatusPreparing, v16.StatusCharging, v16.StatusFinishing}
	if got := statuses(t, csms, 1); !slices.Equal(got, want) {
		t.Errorf("connector 1 reported %v, want %v", got, want)
	}
}

func TestUnplugStopsTheTransaction(t *testing.T) {
	csms := csmstest.Start(t)
	r := online(t, csms, charger.Config{})

	must(t)(r.Plug(ctx, 1, ev.Defaults()))
	must(t)(r.Tap(ctx, 1, "TAG-A"))
	waitStatus(t, csms, 1, v16.StatusCharging)

	outcome := must(t)(r.Unplug(1))
	if outcome != "EV unplugged, transaction 1 stopped" {
		t.Errorf("unplug outcome = %q", outcome)
	}

	stop := stopRequest(t, csms)
	if stop.Reason != v16.ReasonEVDisconnected || stop.TransactionID != 1 {
		t.Errorf("StopTransaction = %+v, want transaction 1 with reason EVDisconnected", stop)
	}
	csms.WaitForCalls(t, "CP-1", v16.ActionStatusNotification, 5)
	got := statuses(t, csms, 1)
	want := "Available,Preparing,Charging,Available"
	if strings.Join(got, ",") != want {
		t.Errorf("connector 1 reported %v, want %s", got, want)
	}
}

func TestFullEVStopsTheTransactionByDefault(t *testing.T) {
	csms := csmstest.Start(t)
	r := online(t, csms, charger.Config{Speed: 3600})

	// 1% of 60 kWh short of the target: 600 Wh to go.
	params := ev.Defaults()
	params.SoC = 0.79
	params.TargetSoC = 0.80
	must(t)(r.Plug(ctx, 1, params))
	must(t)(r.Tap(ctx, 1, "TAG-A"))

	stop := stopRequest(t, csms)
	if stop.Reason != v16.ReasonOther {
		t.Errorf("reason = %s, want Other when the EV is full", stop.Reason)
	}
	// The register is truncated to whole Wh, so allow one below.
	if stop.MeterStop < 599 || stop.MeterStop > 600 {
		t.Errorf("meterStop = %d Wh, want the 600 Wh the EV had room for", stop.MeterStop)
	}
	waitStatus(t, csms, 1, v16.StatusFinishing)
}

func TestANewTransactionOnAFullEVEndsAtOnce(t *testing.T) {
	csms := csmstest.Start(t)
	r := online(t, csms, charger.Config{Speed: 3600})

	params := ev.Defaults()
	params.SoC = 0.79
	params.TargetSoC = 0.80
	must(t)(r.Plug(ctx, 1, params))
	must(t)(r.Tap(ctx, 1, "TAG-A"))
	first := stopRequest(t, csms)
	waitStatus(t, csms, 1, v16.StatusFinishing)

	// The EV is at its target and still plugged in. A new transaction
	// starts, as the charger cannot know that before it tries, and ends
	// with no energy delivered.
	must(t)(r.Tap(ctx, 1, "TAG-A"))
	var second v16.StopTransactionRequest
	csms.WaitForCalls(t, "CP-1", v16.ActionStopTransaction, 2)[1].Decode(t, &second)
	if second.TransactionID != 2 || second.Reason != v16.ReasonOther || second.MeterStop != first.MeterStop {
		t.Errorf("second StopTransaction = %+v, want transaction 2 stopped with reason Other at %d Wh", second, first.MeterStop)
	}
	waitStatuses(t, csms, 1,
		v16.StatusAvailable, v16.StatusPreparing, v16.StatusCharging,
		v16.StatusFinishing, v16.StatusPreparing, v16.StatusCharging, v16.StatusFinishing)
}

func TestFullEVSuspendsWhenTheModelSaysSo(t *testing.T) {
	csms := csmstest.Start(t)
	r := online(t, csms, charger.Config{
		Speed: 3600,
		Model: modelWith(func(m *model.Model) { m.OnEvFull = model.OnEvFullSuspend }),
	})

	params := ev.Defaults()
	params.SoC = 0.79
	params.TargetSoC = 0.80
	must(t)(r.Plug(ctx, 1, params))
	must(t)(r.Tap(ctx, 1, "TAG-A"))

	waitStatus(t, csms, 1, v16.StatusSuspendedEV)
	for _, call := range csms.Calls("CP-1") {
		if call.Action == v16.ActionStopTransaction {
			t.Fatal("the transaction was stopped, want it left open")
		}
	}

	// It stays open until something ends it.
	must(t)(r.Unplug(1))
	if stop := stopRequest(t, csms); stop.Reason != v16.ReasonEVDisconnected || stop.MeterStop < 599 || stop.MeterStop > 600 {
		t.Errorf("StopTransaction = %+v, want EVDisconnected at 600 Wh", stop)
	}
}

func TestStartOnPlugStartsWithoutATap(t *testing.T) {
	csms := csmstest.Start(t)
	r := online(t, csms, charger.Config{Model: modelWith(func(m *model.Model) {
		m.StartOn = model.StartOnPlug
		m.StartTag = "FREEVEND"
	})})

	outcome := must(t)(r.Plug(ctx, 1, ev.Defaults()))
	if !strings.HasSuffix(outcome, "transaction 1 started") {
		t.Errorf("plug outcome = %q", outcome)
	}

	var start v16.StartTransactionRequest
	csms.WaitForCalls(t, "CP-1", v16.ActionStartTransaction, 1)[0].Decode(t, &start)
	if start.IDTag != "FREEVEND" {
		t.Errorf("StartTransaction idTag = %q, want the model's startTag", start.IDTag)
	}
	for _, call := range csms.Calls("CP-1") {
		if call.Action == v16.ActionAuthorize {
			t.Error("the charger sent Authorize, want none when starting on plug-in")
		}
	}

	// A tap of the start tag still stops it.
	must(t)(r.Tap(ctx, 1, "FREEVEND"))
	if stop := stopRequest(t, csms); stop.Reason != v16.ReasonLocal {
		t.Errorf("reason = %s, want Local", stop.Reason)
	}
}

func TestRejectedTagStartsNothing(t *testing.T) {
	csms := csmstest.Start(t)
	csms.Handle(v16.ActionAuthorize, func(string, json.RawMessage) (any, *ocppj.CallError) {
		return v16.AuthorizeResponse{IDTagInfo: v16.IDTagInfo{Status: "Invalid"}}, nil
	})
	r := online(t, csms, charger.Config{})
	must(t)(r.Plug(ctx, 1, ev.Defaults()))

	// A rejected tag is an outcome, not a failed action.
	outcome := must(t)(r.Tap(ctx, 1, "TAG-A"))
	if outcome != "tag rejected by the CSMS (Invalid)" {
		t.Errorf("tap outcome = %q", outcome)
	}

	time.Sleep(100 * time.Millisecond)
	for _, call := range csms.Calls("CP-1") {
		if call.Action == v16.ActionStartTransaction {
			t.Error("a transaction started with a rejected tag")
		}
	}
	if got := r.Snapshot().Connectors[0].Status; got != v16.StatusPreparing {
		t.Errorf("connector status = %s, want it to stay Preparing", got)
	}
}

func TestTagRejectedAtStartTransactionStopsWithDeAuthorized(t *testing.T) {
	csms := csmstest.Start(t)
	csms.Handle(v16.ActionStartTransaction, func(string, json.RawMessage) (any, *ocppj.CallError) {
		return v16.StartTransactionResponse{IDTagInfo: v16.IDTagInfo{Status: "Blocked"}, TransactionID: 7}, nil
	})
	r := online(t, csms, charger.Config{})
	must(t)(r.Plug(ctx, 1, ev.Defaults()))

	outcome := must(t)(r.Tap(ctx, 1, "TAG-A"))
	if outcome != "tag accepted, transaction 7 started, then stopped" {
		t.Errorf("tap outcome = %q", outcome)
	}

	stop := stopRequest(t, csms)
	if stop.Reason != v16.ReasonDeAuthorized || stop.TransactionID != 7 {
		t.Errorf("StopTransaction = %+v, want transaction 7 with reason DeAuthorized", stop)
	}
}

func TestOnlyATagWithTheSameParentStopsTheTransaction(t *testing.T) {
	csms := csmstest.Start(t)
	parents := map[string]string{"TAG-A": "FLEET", "TAG-B": "FLEET", "TAG-C": "", "TAG-D": "OTHER"}
	csms.Handle(v16.ActionAuthorize, func(_ string, payload json.RawMessage) (any, *ocppj.CallError) {
		var req v16.AuthorizeRequest
		_ = json.Unmarshal(payload, &req)
		return v16.AuthorizeResponse{IDTagInfo: v16.IDTagInfo{
			Status:      v16.AuthorizationAccepted,
			ParentIDTag: parents[req.IDTag],
		}}, nil
	})
	r := online(t, csms, charger.Config{})
	must(t)(r.Plug(ctx, 1, ev.Defaults()))
	must(t)(r.Tap(ctx, 1, "TAG-A"))
	waitStatus(t, csms, 1, v16.StatusCharging)

	for _, stranger := range []string{"TAG-C", "TAG-D"} {
		outcome := must(t)(r.Tap(ctx, 1, stranger))
		if !strings.Contains(outcome, "transaction continues") {
			t.Errorf("tap of %s: outcome = %q, want the transaction to continue", stranger, outcome)
		}
	}
	if got := r.Snapshot().Connectors[0].Status; got != v16.StatusCharging {
		t.Fatalf("connector status = %s, want Charging after unrelated tags", got)
	}

	outcome := must(t)(r.Tap(ctx, 1, "TAG-B"))
	if outcome != "transaction 1 stopped" {
		t.Errorf("tap of TAG-B: outcome = %q", outcome)
	}
	if stop := stopRequest(t, csms); stop.IDTag != "TAG-B" || stop.Reason != v16.ReasonLocal {
		t.Errorf("StopTransaction = %+v, want tag TAG-B and reason Local", stop)
	}
}

func TestNoTransactionDataWhenStopTxnSampledDataIsEmpty(t *testing.T) {
	csms := csmstest.Start(t)
	r := online(t, csms, charger.Config{Model: modelWith(func(m *model.Model) {
		m.Config["StopTxnSampledData"] = ""
	})})
	must(t)(r.Plug(ctx, 1, ev.Defaults()))
	must(t)(r.Tap(ctx, 1, "TAG-A"))
	must(t)(r.Tap(ctx, 1, "TAG-A"))

	var raw map[string]json.RawMessage
	csms.WaitForCalls(t, "CP-1", v16.ActionStopTransaction, 1)[0].Decode(t, &raw)
	if _, present := raw["transactionData"]; present {
		t.Errorf("StopTransaction has transactionData %s, want none", raw["transactionData"])
	}
}

func TestOrderlyShutdownStopsTransactions(t *testing.T) {
	csms := csmstest.Start(t)
	r := online(t, csms, charger.Config{})
	must(t)(r.Plug(ctx, 2, ev.Defaults()))
	must(t)(r.Tap(ctx, 2, "TAG-A"))
	waitStatus(t, csms, 2, v16.StatusCharging)

	r.stop()

	stop := stopRequest(t, csms)
	if stop.TransactionID != 1 || stop.Reason != v16.ReasonOther {
		t.Errorf("StopTransaction = %+v, want transaction 1 with reason Other", stop)
	}
	if ends := csms.WaitDisconnects(t, "CP-1", 1); !ends[0].Clean {
		t.Error("the connection was dropped, want a normal close after the StopTransaction")
	}
}

func TestActionsThatCannotBeCarriedOut(t *testing.T) {
	csms := csmstest.Start(t)
	r := online(t, csms, charger.Config{})

	fails := func(name string, err error, want string) {
		t.Helper()
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: error = %v, want one containing %q", name, err, want)
		}
	}

	_, err := r.Unplug(1)
	fails("unplug with no EV", err, "has no EV")

	_, err = r.Plug(ctx, 9, ev.Defaults())
	fails("plug into a missing connector", err, "no connector 9")

	must(t)(r.Plug(ctx, 1, ev.Defaults()))
	_, err = r.Plug(ctx, 1, ev.Defaults())
	fails("plug twice", err, "already has an EV")

	bad := ev.Defaults()
	bad.SoC = 1.5
	_, err = r.Plug(ctx, 2, bad)
	fails("plug an impossible EV", err, "state of charge")

	_, err = r.Tap(ctx, 1, "")
	fails("tap without a tag", err, "tag is empty")

	_, err = r.Tap(ctx, 1, strings.Repeat("X", 21))
	fails("tap an over-long tag", err, "20 characters")
}

func TestTapFailsWhileOffline(t *testing.T) {
	r := powerOn(t, charger.Config{ID: "CP-1", CSMS: charger.CSMS{URL: "ws://127.0.0.1:1/ocpp"}})
	r.log.waitFor(t, charger.Event, "could not connect")

	if _, err := r.Tap(ctx, 1, "TAG-A"); err == nil || !strings.Contains(err.Error(), "offline") {
		t.Errorf("tap while offline returned %v, want an error saying the charger is offline", err)
	}
	if got := r.Snapshot().Connection; got != charger.Offline {
		t.Errorf("connection = %s, want offline", got)
	}
}
