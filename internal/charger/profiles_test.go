package charger_test

import (
	"math"
	"testing"
	"time"

	"github.com/Amraa1/plugyard/internal/charger"
	"github.com/Amraa1/plugyard/internal/csmstest"
	"github.com/Amraa1/plugyard/internal/ev"
	"github.com/Amraa1/plugyard/internal/model"
	"github.com/Amraa1/plugyard/internal/ocpp/v16"
)

// profile returns an Absolute charging profile with one period that starts
// at once.
func profile(id int, purpose string, stackLevel int, unit string, limit float64) v16.ChargingProfile {
	return v16.ChargingProfile{
		ChargingProfileID:      id,
		StackLevel:             stackLevel,
		ChargingProfilePurpose: purpose,
		ChargingProfileKind:    v16.KindAbsolute,
		ChargingSchedule: v16.ChargingSchedule{
			ChargingRateUnit:       unit,
			ChargingSchedulePeriod: []v16.ChargingSchedulePeriod{{StartPeriod: 0, Limit: limit}},
		},
	}
}

// install sends a charging profile to the charger and returns its answer.
func install(t *testing.T, csms *csmstest.CSMS, connector int, p v16.ChargingProfile) string {
	t.Helper()
	return ask(t, csms, v16.ActionSetChargingProfile, v16.SetChargingProfileRequest{ConnectorID: connector, CsChargingProfiles: p})
}

func mustInstall(t *testing.T, csms *csmstest.CSMS, connector int, p v16.ChargingProfile) {
	t.Helper()
	if got := install(t, csms, connector, p); got != v16.StatusAccepted {
		t.Fatalf("SetChargingProfile %d on connector %d answered %s, want Accepted", p.ChargingProfileID, connector, got)
	}
}

func power(r *running, connector int) float64 {
	for _, k := range r.Snapshot().Connectors {
		if k.ID == connector {
			return k.PowerW
		}
	}
	return math.NaN()
}

// waitPower waits until a connector delivers the given power.
func waitPower(t *testing.T, r *running, connector int, want float64) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		got := power(r, connector)
		if math.Abs(got-want) <= 1 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("connector %d delivers %v W, want %v W", connector, got, want)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// waitLastStatus waits until the status a connector reported last is the
// given one.
func waitLastStatus(t *testing.T, csms *csmstest.CSMS, connector int, status string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		all := statuses(t, csms, connector)
		if len(all) > 0 && all[len(all)-1] == status {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("connector %d last reported %v, want %s", connector, all, status)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestAChargingProfileThrottlesARunningTransaction(t *testing.T) {
	csms := csmstest.Start(t)
	r := charging(t, csms, charger.Config{Model: modelWith(func(m *model.Model) {
		m.Config["MeterValuesSampledData"] = "Power.Active.Import,Power.Offered,Current.Offered"
	})})
	waitPower(t, r, 1, 50000)

	mustInstall(t, csms, 1, profile(7, v16.PurposeTx, 0, v16.UnitWatts, 10000))
	waitPower(t, r, 1, 10000)
	r.log.waitFor(t, charger.Event, "connector 1: a charging profile limits charging to 10kW")

	// The meter values the CSMS gets show the limit and what is delivered
	// under it.
	if got := ask(t, csms, v16.ActionTriggerMessage, v16.TriggerMessageRequest{RequestedMessage: v16.ActionMeterValues, ConnectorID: intPtr(1)}); got != v16.StatusAccepted {
		t.Fatalf("TriggerMessage answered %s", got)
	}
	var values v16.MeterValuesRequest
	csms.WaitForCalls(t, "CP-1", v16.ActionMeterValues, 1)[0].Decode(t, &values)
	got := map[string]string{}
	for _, v := range values.MeterValue[0].SampledValue {
		got[v.Measurand] = v.Value + " " + v.Unit
	}
	for measurand, want := range map[string]string{
		v16.MeasurandPower:          "10000 W",
		v16.MeasurandPowerOffered:   "10000 W",
		v16.MeasurandCurrentOffered: "14.5 A", // 10 kW over 3 phases of 230 V
	} {
		if got[measurand] != want {
			t.Errorf("%s = %q, want %q", measurand, got[measurand], want)
		}
	}

	// With the profile cleared the EV is back at full power.
	clear := v16.ClearChargingProfileRequest{ID: intPtr(7)}
	if got := ask(t, csms, v16.ActionClearChargingProfile, clear); got != v16.StatusAccepted {
		t.Fatalf("ClearChargingProfile answered %s", got)
	}
	waitPower(t, r, 1, 50000)
	r.log.waitFor(t, charger.Event, "connector 1: no charging profile limits charging any more")
	if got := ask(t, csms, v16.ActionClearChargingProfile, clear); got != v16.StatusUnknown {
		t.Errorf("clearing a profile that is gone answered %s, want Unknown", got)
	}
}

func TestALimitOfZeroSuspendsCharging(t *testing.T) {
	csms := csmstest.Start(t)
	r := charging(t, csms, charger.Config{})
	waitLastStatus(t, csms, 1, v16.StatusCharging)

	mustInstall(t, csms, 1, profile(1, v16.PurposeTx, 0, v16.UnitAmps, 0))
	waitLastStatus(t, csms, 1, v16.StatusSuspendedEVSE)
	waitPower(t, r, 1, 0)
	before := r.Snapshot().Connectors[0].MeterWh

	// The same purpose and stack level replaces the profile.
	mustInstall(t, csms, 1, profile(2, v16.PurposeTx, 0, v16.UnitAmps, 10))
	waitLastStatus(t, csms, 1, v16.StatusCharging)
	waitPower(t, r, 1, 6900)
	if after := r.Snapshot().Connectors[0].MeterWh; after < before {
		t.Errorf("the meter went from %v to %v Wh", before, after)
	}
	if n := count(csms, v16.ActionStopTransaction); n != 0 {
		t.Errorf("the transaction stopped while it was suspended")
	}
}

func TestLimitsInAmpsFollowTheSupply(t *testing.T) {
	csms := csmstest.Start(t)
	r := charging(t, csms, charger.Config{})

	// The default supply is 3 phases of 230 V.
	mustInstall(t, csms, 1, profile(1, v16.PurposeTx, 0, v16.UnitAmps, 16))
	waitPower(t, r, 1, 16*230*3)

	// The profile's own number of phases comes first.
	single := profile(1, v16.PurposeTx, 0, v16.UnitAmps, 16)
	single.ChargingSchedule.ChargingSchedulePeriod[0].NumberPhases = intPtr(1)
	mustInstall(t, csms, 1, single)
	waitPower(t, r, 1, 16*230)
	r.stop()

	// A Chargerfile can describe another supply, such as a DC charger's.
	csms2 := csmstest.Start(t)
	dc := charging(t, csms2, charger.Config{Model: modelWith(func(m *model.Model) {
		m.Voltage, m.Phases = 400, 1
	})})
	mustInstall(t, csms2, 1, profile(1, v16.PurposeTx, 0, v16.UnitAmps, 100))
	waitPower(t, dc, 1, 40000)
}

func TestWhichChargingProfileWins(t *testing.T) {
	csms := csmstest.Start(t)
	r := charging(t, csms, charger.Config{})

	// A TxDefaultProfile of the whole charger applies to every transaction.
	mustInstall(t, csms, 0, profile(1, v16.PurposeTxDefault, 0, v16.UnitWatts, 20000))
	waitPower(t, r, 1, 20000)
	// One of the connector itself overrides it.
	mustInstall(t, csms, 1, profile(2, v16.PurposeTxDefault, 0, v16.UnitWatts, 15000))
	waitPower(t, r, 1, 15000)
	// A TxProfile overrides both.
	mustInstall(t, csms, 1, profile(3, v16.PurposeTx, 0, v16.UnitWatts, 8000))
	waitPower(t, r, 1, 8000)
	// Within one purpose the highest stack level decides, whatever its
	// limit.
	mustInstall(t, csms, 1, profile(4, v16.PurposeTx, 1, v16.UnitWatts, 9000))
	waitPower(t, r, 1, 9000)
	// A ChargePointMaxProfile caps the result.
	mustInstall(t, csms, 0, profile(5, v16.PurposeChargePointMax, 0, v16.UnitWatts, 6000))
	waitPower(t, r, 1, 6000)

	// The limit of the whole charger is shared by the connectors that are
	// charging.
	must(t)(r.Plug(ctx, 2, ev.Defaults()))
	must(t)(r.Tap(ctx, 2, "TAG-2"))
	waitPower(t, r, 1, 3000)
	waitPower(t, r, 2, 3000)

	// A TxProfile ends with its transaction; the other profiles stay.
	must(t)(r.Tap(ctx, 1, "TAG-1"))
	waitPower(t, r, 2, 6000)
	if got := ask(t, csms, v16.ActionClearChargingProfile, v16.ClearChargingProfileRequest{ChargingProfilePurpose: v16.PurposeTx}); got != v16.StatusUnknown {
		t.Errorf("clearing the TxProfiles after the transaction answered %s, want Unknown: they are gone", got)
	}
	if got := ask(t, csms, v16.ActionClearChargingProfile, v16.ClearChargingProfileRequest{ConnectorID: intPtr(0), ChargingProfilePurpose: v16.PurposeChargePointMax}); got != v16.StatusAccepted {
		t.Fatalf("clearing the ChargePointMaxProfile answered %s", got)
	}
	waitPower(t, r, 2, 20000)

	must(t)(r.Unplug(1))
	must(t)(r.Plug(ctx, 1, ev.Defaults()))
	must(t)(r.Tap(ctx, 1, "TAG-1"))
	waitPower(t, r, 1, 15000)
}

func TestChargingProfilesTheChargerRejects(t *testing.T) {
	csms := csmstest.Start(t)
	charging(t, csms, charger.Config{})

	change := func(p v16.ChargingProfile, fn func(*v16.ChargingProfile)) v16.ChargingProfile {
		fn(&p)
		return p
	}
	ok := profile(1, v16.PurposeTxDefault, 0, v16.UnitWatts, 5000)
	for name, tc := range map[string]struct {
		connector int
		profile   v16.ChargingProfile
	}{
		"an unknown connector":                   {9, ok},
		"a TxProfile with no transaction":        {2, profile(1, v16.PurposeTx, 0, v16.UnitWatts, 5000)},
		"a TxProfile on connector 0":             {0, profile(1, v16.PurposeTx, 0, v16.UnitWatts, 5000)},
		"a TxProfile for another transaction":    {1, change(profile(1, v16.PurposeTx, 0, v16.UnitWatts, 5000), func(p *v16.ChargingProfile) { p.TransactionID = intPtr(99) })},
		"a ChargePointMaxProfile on a connector": {1, profile(1, v16.PurposeChargePointMax, 0, v16.UnitWatts, 5000)},
		"an unknown purpose":                     {1, profile(1, "Whatever", 0, v16.UnitWatts, 5000)},
		"an unknown unit":                        {1, profile(1, v16.PurposeTxDefault, 0, "kW", 5)},
		"a stack level that is too high":         {1, profile(1, v16.PurposeTxDefault, 11, v16.UnitWatts, 5000)},
		"a negative limit":                       {1, profile(1, v16.PurposeTxDefault, 0, v16.UnitWatts, -1)},
		"no periods":                             {1, change(ok, func(p *v16.ChargingProfile) { p.ChargingSchedule.ChargingSchedulePeriod = nil })},
		"a first period that does not start at 0": {1, change(ok, func(p *v16.ChargingProfile) {
			p.ChargingSchedule.ChargingSchedulePeriod = []v16.ChargingSchedulePeriod{{StartPeriod: 5, Limit: 1}}
		})},
		"periods out of order": {1, change(ok, func(p *v16.ChargingProfile) {
			p.ChargingSchedule.ChargingSchedulePeriod = []v16.ChargingSchedulePeriod{{StartPeriod: 0, Limit: 1}, {StartPeriod: 60, Limit: 2}, {StartPeriod: 60, Limit: 3}}
		})},
		"a Recurring profile with no recurrency": {1, change(ok, func(p *v16.ChargingProfile) { p.ChargingProfileKind = v16.KindRecurring })},
		"an unknown kind":                        {1, change(ok, func(p *v16.ChargingProfile) { p.ChargingProfileKind = "Sometimes" })},
	} {
		if got := install(t, csms, tc.connector, tc.profile); got != v16.StatusRejected {
			t.Errorf("%s answered %s, want Rejected", name, got)
		}
	}

	// Nothing of that was installed.
	if got := ask(t, csms, v16.ActionClearChargingProfile, v16.ClearChargingProfileRequest{}); got != v16.StatusUnknown {
		t.Errorf("clearing every profile answered %s, want Unknown: none is installed", got)
	}
}

func TestScheduleFollowsTheRealClockAtAnySpeed(t *testing.T) {
	csms := csmstest.Start(t)
	// At 3600x the EV takes in an hour of energy every second. The schedule
	// still changes after one real second.
	r := charging(t, csms, charger.Config{Speed: 3600, Model: modelWith(func(m *model.Model) {
		m.Config["MeterValueSampleInterval"] = "0"
	})})

	p := profile(1, v16.PurposeTx, 0, v16.UnitWatts, 6000)
	p.ChargingSchedule.ChargingSchedulePeriod = append(p.ChargingSchedule.ChargingSchedulePeriod, v16.ChargingSchedulePeriod{StartPeriod: 1, Limit: 12000})
	installed := time.Now()
	mustInstall(t, csms, 1, p)
	waitPower(t, r, 1, 6000)

	// Energy runs at the simulated speed: 6 kW for half a real second is
	// 3 kWh at 3600x.
	m1, t1 := r.Snapshot().Connectors[0].MeterWh, time.Now()
	time.Sleep(400 * time.Millisecond)
	m2, t2 := r.Snapshot().Connectors[0].MeterWh, time.Now()
	want := 6000 * t2.Sub(t1).Hours() * 3600
	if got := m2 - m1; got < want*0.7 || got > want*1.3 {
		t.Errorf("the meter gained %v Wh in %s, want about %v Wh", got, t2.Sub(t1), want)
	}

	waitPower(t, r, 1, 12000)
	if waited := time.Since(installed); waited < 900*time.Millisecond {
		t.Errorf("the second period started after %s, want one real second", waited)
	}
}

func TestCompositeSchedule(t *testing.T) {
	csms := csmstest.Start(t)
	online(t, csms, charger.Config{})

	composite := func(connector, duration int, unit string) v16.GetCompositeScheduleResponse {
		t.Helper()
		var resp v16.GetCompositeScheduleResponse
		req := v16.GetCompositeScheduleRequest{ConnectorID: connector, Duration: duration, ChargingRateUnit: unit}
		if err := csms.Call(ctx, "CP-1", v16.ActionGetCompositeSchedule, req, &resp); err != nil {
			t.Fatalf("GetCompositeSchedule: %v", err)
		}
		return resp
	}
	// check compares the periods of an answer, allowing a period to start a
	// second early or late.
	check := func(resp v16.GetCompositeScheduleResponse, want ...v16.ChargingSchedulePeriod) {
		t.Helper()
		if resp.Status != v16.StatusAccepted || resp.ChargingSchedule == nil || resp.ScheduleStart == nil {
			t.Fatalf("GetCompositeSchedule answered %+v", resp)
		}
		got := resp.ChargingSchedule.ChargingSchedulePeriod
		if len(got) != len(want) {
			t.Fatalf("periods = %+v, want %+v", got, want)
		}
		for i := range want {
			if d := got[i].StartPeriod - want[i].StartPeriod; d < -1 || d > 1 || got[i].Limit != want[i].Limit {
				t.Errorf("period %d = %+v, want %+v", i, got[i], want[i])
			}
		}
	}

	// With no profile a connector offers its full power.
	check(composite(1, 900, ""), v16.ChargingSchedulePeriod{StartPeriod: 0, Limit: 50000})

	// 20 kW for five minutes, 10 kW for five more, and nothing after that.
	p := profile(1, v16.PurposeTxDefault, 0, v16.UnitWatts, 20000)
	start := v16.Time{Time: time.Now()}
	duration := 600
	p.ChargingSchedule.StartSchedule, p.ChargingSchedule.Duration = &start, &duration
	p.ChargingSchedule.ChargingSchedulePeriod = append(p.ChargingSchedule.ChargingSchedulePeriod, v16.ChargingSchedulePeriod{StartPeriod: 300, Limit: 10000})
	mustInstall(t, csms, 1, p)

	resp := composite(1, 900, v16.UnitWatts)
	check(resp,
		v16.ChargingSchedulePeriod{StartPeriod: 0, Limit: 20000},
		v16.ChargingSchedulePeriod{StartPeriod: 300, Limit: 10000},
		v16.ChargingSchedulePeriod{StartPeriod: 600, Limit: 50000},
	)
	if *resp.ConnectorID != 1 || *resp.ChargingSchedule.Duration != 900 || resp.ChargingSchedule.ChargingRateUnit != v16.UnitWatts {
		t.Errorf("GetCompositeSchedule answered %+v with schedule %+v", resp, resp.ChargingSchedule)
	}

	// In amps, over 3 phases of 230 V.
	amps := composite(1, 200, v16.UnitAmps)
	check(amps, v16.ChargingSchedulePeriod{StartPeriod: 0, Limit: 29})
	if n := amps.ChargingSchedule.ChargingSchedulePeriod[0].NumberPhases; n == nil || *n != 3 {
		t.Errorf("numberPhases = %v, want 3", n)
	}

	// Connector 0 is the charger as a whole: both connectors together, and
	// never more than a ChargePointMaxProfile allows.
	check(composite(0, 200, ""), v16.ChargingSchedulePeriod{StartPeriod: 0, Limit: 70000})
	mustInstall(t, csms, 0, profile(2, v16.PurposeChargePointMax, 0, v16.UnitWatts, 30000))
	check(composite(0, 200, ""), v16.ChargingSchedulePeriod{StartPeriod: 0, Limit: 30000})

	for name, resp := range map[string]v16.GetCompositeScheduleResponse{
		"an unknown connector": composite(9, 60, ""),
		"no duration":          composite(1, 0, ""),
		"an unknown unit":      composite(1, 60, "kW"),
	} {
		if resp.Status != v16.StatusRejected || resp.ChargingSchedule != nil {
			t.Errorf("GetCompositeSchedule with %s answered %+v, want Rejected", name, resp)
		}
	}
}

func TestRemoteStartWithAChargingProfile(t *testing.T) {
	csms := csmstest.Start(t)
	r := online(t, csms, charger.Config{})

	p := profile(1, v16.PurposeTxDefault, 0, v16.UnitWatts, 7000)
	req := v16.RemoteStartTransactionRequest{ConnectorID: intPtr(1), IDTag: "REMOTE", ChargingProfile: &p}
	if got := ask(t, csms, v16.ActionRemoteStartTransaction, req); got != v16.StatusRejected {
		t.Errorf("a remote start with a TxDefaultProfile answered %s, want Rejected", got)
	}

	// The profile waits with the tag for an EV, and limits the transaction
	// from its first moment.
	p.ChargingProfilePurpose = v16.PurposeTx
	if got := ask(t, csms, v16.ActionRemoteStartTransaction, req); got != v16.StatusAccepted {
		t.Fatalf("RemoteStartTransaction answered %s", got)
	}
	waitStatus(t, csms, 1, v16.StatusPreparing)
	must(t)(r.Plug(ctx, 1, ev.Defaults()))
	csms.WaitForCalls(t, "CP-1", v16.ActionStartTransaction, 1)
	waitPower(t, r, 1, 7000)
}

func TestChargingProfilesAreSaved(t *testing.T) {
	csms := csmstest.Start(t)
	d := &disk{}
	r := charging(t, csms, charger.Config{Save: d.save})
	mustInstall(t, csms, 0, profile(1, v16.PurposeTxDefault, 0, v16.UnitWatts, 11000))
	mustInstall(t, csms, 1, profile(2, v16.PurposeTx, 0, v16.UnitWatts, 5000))
	waitPower(t, r, 1, 5000)

	saved := d.waitSaved(t, "the charging profiles", func(st *charger.State) bool { return len(st.Profiles) == 2 })
	r.Kill()
	r.stop()

	// The TxDefaultProfile is back after a restart. The TxProfile ended with
	// its transaction, which the power cut ended.
	r2 := online(t, csms, charger.Config{Save: d.save, Restore: saved})
	csms.WaitForCalls(t, "CP-1", v16.ActionBootNotification, 2)
	for deadline := time.Now().Add(5 * time.Second); r2.Snapshot().Connection != charger.Online; {
		if time.Now().After(deadline) {
			t.Fatal("the charger did not come back online")
		}
		time.Sleep(5 * time.Millisecond)
	}
	must(t)(r2.Unplug(1))
	must(t)(r2.Plug(ctx, 1, ev.Defaults()))
	must(t)(r2.Tap(ctx, 1, "TAG-1"))
	waitPower(t, r2, 1, 11000)
	if got := ask(t, csms, v16.ActionClearChargingProfile, v16.ClearChargingProfileRequest{ID: intPtr(2)}); got != v16.StatusUnknown {
		t.Errorf("the TxProfile outlived its transaction: clearing it answered %s", got)
	}
}
