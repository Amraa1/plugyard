package charger_test

import (
	"encoding/json"
	"errors"
	"slices"
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

// ask sends a request from the CSMS to the charger and returns the status it
// answers with.
func ask(t *testing.T, csms *csmstest.CSMS, action string, payload any) string {
	t.Helper()
	var answer v16.StatusAnswer
	if err := csms.Call(ctx, "CP-1", action, payload, &answer); err != nil {
		t.Fatalf("%s: %v", action, err)
	}
	return answer.Status
}

// count returns how many calls of an action the charger has made.
func count(csms *csmstest.CSMS, action string) int {
	n := 0
	for _, a := range csms.Actions("CP-1") {
		if a == action {
			n++
		}
	}
	return n
}

func intPtr(n int) *int { return &n }

// charging powers a charger on and starts a transaction on connector 1.
func charging(t *testing.T, csms *csmstest.CSMS, cfg charger.Config) *running {
	t.Helper()
	r := online(t, csms, cfg)
	must(t)(r.Plug(ctx, 1, ev.Defaults()))
	must(t)(r.Tap(ctx, 1, "TAG-1"))
	csms.WaitForCalls(t, "CP-1", v16.ActionStartTransaction, 1)
	return r
}

func TestRemoteStartWithAnEVWaiting(t *testing.T) {
	csms := csmstest.Start(t)
	r := online(t, csms, charger.Config{})
	must(t)(r.Plug(ctx, 2, ev.Defaults()))

	// No connector is named: the one with an EV waiting is chosen.
	if got := ask(t, csms, v16.ActionRemoteStartTransaction, v16.RemoteStartTransactionRequest{IDTag: "REMOTE"}); got != v16.StatusAccepted {
		t.Fatalf("RemoteStartTransaction answered %s", got)
	}
	var start v16.StartTransactionRequest
	csms.WaitForCalls(t, "CP-1", v16.ActionStartTransaction, 1)[0].Decode(t, &start)
	if start.ConnectorID != 2 || start.IDTag != "REMOTE" {
		t.Errorf("StartTransaction = %+v, want connector 2 with the tag from the CSMS", start)
	}
	if n := count(csms, v16.ActionAuthorize); n != 0 {
		t.Errorf("the charger sent %d Authorize, want none: AuthorizeRemoteTxRequests is false", n)
	}
	waitStatus(t, csms, 2, v16.StatusCharging)
}

func TestRemoteStartWaitsForAnEV(t *testing.T) {
	csms := csmstest.Start(t)
	r := online(t, csms, charger.Config{})

	req := v16.RemoteStartTransactionRequest{ConnectorID: intPtr(2), IDTag: "REMOTE"}
	if got := ask(t, csms, v16.ActionRemoteStartTransaction, req); got != v16.StatusAccepted {
		t.Fatalf("RemoteStartTransaction answered %s", got)
	}
	// As after an accepted tap, the connector waits for an EV.
	waitStatus(t, csms, 2, v16.StatusPreparing)
	if n := count(csms, v16.ActionStartTransaction); n != 0 {
		t.Fatalf("a transaction started with no EV plugged in")
	}

	// A connector that is not free is refused, as is one that does not
	// exist, or a tag that is too long.
	for name, bad := range map[string]v16.RemoteStartTransactionRequest{
		"a waiting connector":  req,
		"an unknown connector": {ConnectorID: intPtr(9), IDTag: "REMOTE"},
		"an overlong tag":      {IDTag: strings.Repeat("X", 21)},
	} {
		if got := ask(t, csms, v16.ActionRemoteStartTransaction, bad); got != v16.StatusRejected {
			t.Errorf("RemoteStartTransaction for %s answered %s, want Rejected", name, got)
		}
	}

	must(t)(r.Plug(ctx, 2, ev.Defaults()))
	csms.WaitForCalls(t, "CP-1", v16.ActionStartTransaction, 1)
}

func TestRemoteStartOnAFinishingConnector(t *testing.T) {
	csms := csmstest.Start(t)
	charging(t, csms, charger.Config{})
	if got := ask(t, csms, v16.ActionRemoteStopTransaction, v16.RemoteStopTransactionRequest{TransactionID: 1}); got != v16.StatusAccepted {
		t.Fatalf("RemoteStopTransaction answered %s", got)
	}
	waitStatus(t, csms, 1, v16.StatusFinishing)

	// The EV was never unplugged, and the CSMS starts charging again.
	req := v16.RemoteStartTransactionRequest{ConnectorID: intPtr(1), IDTag: "REMOTE"}
	if got := ask(t, csms, v16.ActionRemoteStartTransaction, req); got != v16.StatusAccepted {
		t.Fatalf("RemoteStartTransaction answered %s", got)
	}
	var start v16.StartTransactionRequest
	csms.WaitForCalls(t, "CP-1", v16.ActionStartTransaction, 2)[1].Decode(t, &start)
	if start.ConnectorID != 1 || start.IDTag != "REMOTE" {
		t.Errorf("second StartTransaction = %+v, want connector 1 with the tag from the CSMS", start)
	}
	waitStatuses(t, csms, 1,
		v16.StatusAvailable, v16.StatusPreparing, v16.StatusCharging,
		v16.StatusFinishing, v16.StatusPreparing, v16.StatusCharging)
}

func TestRemoteStartWithNoConnectorNamedChoosesByWhatIsPluggedIn(t *testing.T) {
	remoteStart := func(t *testing.T, csms *csmstest.CSMS) v16.StartTransactionRequest {
		t.Helper()
		if got := ask(t, csms, v16.ActionRemoteStartTransaction, v16.RemoteStartTransactionRequest{IDTag: "REMOTE"}); got != v16.StatusAccepted {
			t.Fatalf("RemoteStartTransaction answered %s", got)
		}
		var start v16.StartTransactionRequest
		csms.WaitForCalls(t, "CP-1", v16.ActionStartTransaction, 2)[1].Decode(t, &start)
		return start
	}

	t.Run("an EV waiting comes before a Finishing connector", func(t *testing.T) {
		csms := csmstest.Start(t)
		r := online(t, csms, charger.Config{})
		finished(t, csms, r, 1)
		must(t)(r.Plug(ctx, 2, ev.Defaults()))

		if start := remoteStart(t, csms); start.ConnectorID != 2 || start.IDTag != "REMOTE" {
			t.Errorf("StartTransaction = %+v, want connector 2, where an EV is waiting", start)
		}
	})

	t.Run("a Finishing connector comes before an empty one", func(t *testing.T) {
		csms := csmstest.Start(t)
		r := online(t, csms, charger.Config{})
		finished(t, csms, r, 2)

		if start := remoteStart(t, csms); start.ConnectorID != 2 || start.IDTag != "REMOTE" {
			t.Errorf("StartTransaction = %+v, want connector 2, where an EV is plugged in", start)
		}
	})
}

func TestRemoteStartAuthorizesWhenConfigured(t *testing.T) {
	csms := csmstest.Start(t)
	csms.Handle(v16.ActionAuthorize, func(string, json.RawMessage) (any, *ocppj.CallError) {
		return v16.AuthorizeResponse{IDTagInfo: v16.IDTagInfo{Status: "Blocked"}}, nil
	})
	r := online(t, csms, charger.Config{Model: modelWith(func(m *model.Model) {
		m.Config["AuthorizeRemoteTxRequests"] = "true"
	})})
	must(t)(r.Plug(ctx, 1, ev.Defaults()))

	if got := ask(t, csms, v16.ActionRemoteStartTransaction, v16.RemoteStartTransactionRequest{IDTag: "REMOTE"}); got != v16.StatusAccepted {
		t.Fatalf("RemoteStartTransaction answered %s", got)
	}
	csms.WaitForCalls(t, "CP-1", v16.ActionAuthorize, 1)
	r.log.waitFor(t, charger.Event, "tag REMOTE rejected by the CSMS (Blocked)")
	if n := count(csms, v16.ActionStartTransaction); n != 0 {
		t.Error("a transaction started with a tag the CSMS rejected")
	}
}

func TestRemoteStop(t *testing.T) {
	csms := csmstest.Start(t)
	charging(t, csms, charger.Config{})

	if got := ask(t, csms, v16.ActionRemoteStopTransaction, v16.RemoteStopTransactionRequest{TransactionID: 99}); got != v16.StatusRejected {
		t.Errorf("RemoteStopTransaction for an unknown transaction answered %s", got)
	}
	if got := ask(t, csms, v16.ActionRemoteStopTransaction, v16.RemoteStopTransactionRequest{TransactionID: 1}); got != v16.StatusAccepted {
		t.Fatalf("RemoteStopTransaction answered %s", got)
	}
	if stop := stopRequest(t, csms); stop.Reason != v16.ReasonRemote || stop.TransactionID != 1 {
		t.Errorf("StopTransaction = %+v, want reason Remote", stop)
	}
	waitStatus(t, csms, 1, v16.StatusFinishing)
}

func TestReset(t *testing.T) {
	for _, tc := range []struct {
		kind, reason string
		clean        bool
	}{
		{v16.ResetSoft, v16.ReasonSoftReset, true},
		{v16.ResetHard, v16.ReasonHardReset, false},
	} {
		t.Run(tc.kind, func(t *testing.T) {
			csms := csmstest.Start(t)
			r := charging(t, csms, charger.Config{})

			if got := ask(t, csms, v16.ActionReset, v16.ResetRequest{Type: tc.kind}); got != v16.StatusAccepted {
				t.Fatalf("Reset answered %s", got)
			}
			// The transaction ends before the charger goes away.
			if stop := stopRequest(t, csms); stop.Reason != tc.reason {
				t.Errorf("StopTransaction reason = %s, want %s", stop.Reason, tc.reason)
			}
			if ends := csms.WaitDisconnects(t, "CP-1", 1); ends[0].Clean != tc.clean {
				t.Errorf("the connection ended clean=%v, want %v", ends[0].Clean, tc.clean)
			}
			// It boots again, with the EV still plugged in.
			csms.WaitForCalls(t, "CP-1", v16.ActionBootNotification, 2)
			r.log.waitFor(t, charger.Event, "rebooting")
			csms.WaitForCalls(t, "CP-1", v16.ActionStatusNotification, 6)
			if got := r.Snapshot().Connectors[0].Status; got != v16.StatusFinishing {
				t.Errorf("connector 1 after the reset is %s, want Finishing", got)
			}
		})
	}

	csms := csmstest.Start(t)
	online(t, csms, charger.Config{})
	if got := ask(t, csms, v16.ActionReset, v16.ResetRequest{Type: "Gentle"}); got != v16.StatusRejected {
		t.Errorf("Reset of an unknown type answered %s", got)
	}
}

func TestGetAndChangeConfiguration(t *testing.T) {
	csms := csmstest.Start(t)
	d := &disk{}
	m := modelWith(func(m *model.Model) {
		m.Config["HeartbeatInterval"] = "300"
		m.Config["VendorSpecial"] = "blue"
	})
	online(t, csms, charger.Config{Model: m, Save: d.save})

	get := func(keys ...string) v16.GetConfigurationResponse {
		t.Helper()
		var resp v16.GetConfigurationResponse
		if err := csms.Call(ctx, "CP-1", v16.ActionGetConfiguration, v16.GetConfigurationRequest{Key: keys}, &resp); err != nil {
			t.Fatal(err)
		}
		return resp
	}
	value := func(resp v16.GetConfigurationResponse, key string) (string, bool) {
		for _, kv := range resp.ConfigurationKey {
			if kv.Key == key {
				return kv.Value, kv.Readonly
			}
		}
		t.Fatalf("GetConfiguration did not report %s", key)
		return "", false
	}

	// With no keys named, every key is reported, the Chargerfile's own too.
	all := get()
	if v, readonly := value(all, "NumberOfConnectors"); v != "2" || !readonly {
		t.Errorf("NumberOfConnectors = %q readonly=%v, want 2 and read-only", v, readonly)
	}
	if v, readonly := value(all, "VendorSpecial"); v != "blue" || readonly {
		t.Errorf("VendorSpecial = %q readonly=%v", v, readonly)
	}
	if v, _ := value(all, "SupportedFeatureProfiles"); v != "Core,FirmwareManagement,RemoteTrigger,SmartCharging" {
		t.Errorf("SupportedFeatureProfiles = %q", v)
	}
	some := get("HeartbeatInterval", "Nope")
	if len(some.ConfigurationKey) != 1 || !slices.Equal(some.UnknownKey, []string{"Nope"}) {
		t.Errorf("GetConfiguration for one known and one unknown key = %+v", some)
	}

	change := func(key, v string) string {
		return ask(t, csms, v16.ActionChangeConfiguration, v16.ChangeConfigurationRequest{Key: key, Value: v})
	}
	for _, tc := range []struct{ key, value, want string }{
		{"NumberOfConnectors", "4", v16.StatusRejected},             // read-only
		{"Nope", "1", v16.StatusNotSupported},                       // unknown
		{"MeterValueSampleInterval", "soon", v16.StatusRejected},    // not a number
		{"StopTransactionOnInvalidId", "maybe", v16.StatusRejected}, // not true or false
		{"MeterValuesSampledData", "Voltage", v16.StatusRejected},   // a measurand the charger lacks
		{"MeterValuesSampledData", "SoC,Power.Active.Import", v16.StatusAccepted},
		{"VendorSpecial", "green", v16.StatusAccepted},
		{"MeterSaveInterval", "2", v16.StatusAccepted},
	} {
		if got := change(tc.key, tc.value); got != tc.want {
			t.Errorf("ChangeConfiguration %s=%s answered %s, want %s", tc.key, tc.value, got, tc.want)
		}
	}

	// A change takes effect at once: heartbeats now come every second.
	before := count(csms, v16.ActionHeartbeat)
	if got := change("HeartbeatInterval", "1"); got != v16.StatusAccepted {
		t.Fatalf("ChangeConfiguration HeartbeatInterval answered %s", got)
	}
	csms.WaitForCalls(t, "CP-1", v16.ActionHeartbeat, before+1)

	// What the CSMS changed is saved, and wins over the Chargerfile when the
	// charger comes back. What it did not change follows the file.
	saved := d.waitSaved(t, "the changed keys", func(st *charger.State) bool { return st.Config["HeartbeatInterval"] == "1" })
	if len(saved.Config) != 4 || saved.Config["VendorSpecial"] != "green" {
		t.Errorf("saved configuration = %v, want only the four keys the CSMS changed", saved.Config)
	}
}

func TestSavedConfigurationWinsOverTheChargerfile(t *testing.T) {
	csms := csmstest.Start(t)
	saved := &charger.State{Config: map[string]string{"HeartbeatInterval": "1"}}
	online(t, csms, charger.Config{Restore: saved, Model: modelWith(func(m *model.Model) {
		m.Config["HeartbeatInterval"] = "900"
		m.Config["ConnectionTimeOut"] = "45"
	})})

	var resp v16.GetConfigurationResponse
	if err := csms.Call(ctx, "CP-1", v16.ActionGetConfiguration, v16.GetConfigurationRequest{Key: []string{"HeartbeatInterval", "ConnectionTimeOut"}}, &resp); err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, kv := range resp.ConfigurationKey {
		got[kv.Key] = kv.Value
	}
	if got["HeartbeatInterval"] != "1" || got["ConnectionTimeOut"] != "45" {
		t.Errorf("configuration = %v, want the saved HeartbeatInterval and the file's ConnectionTimeOut", got)
	}
}

func TestChangeAvailability(t *testing.T) {
	csms := csmstest.Start(t)
	d := &disk{}
	r := charging(t, csms, charger.Config{Save: d.save})
	availability := func(connector int, kind string) string {
		return ask(t, csms, v16.ActionChangeAvailability, v16.ChangeAvailabilityRequest{ConnectorID: connector, Type: kind})
	}

	// An idle connector becomes unavailable at once, and refuses a tag.
	if got := availability(2, v16.AvailabilityInoperative); got != v16.StatusAccepted {
		t.Fatalf("ChangeAvailability on an idle connector answered %s", got)
	}
	waitStatus(t, csms, 2, v16.StatusUnavailable)
	if _, err := r.Tap(ctx, 2, "TAG-2"); err == nil || !strings.Contains(err.Error(), "unavailable") {
		t.Errorf("tap on an unavailable connector returned %v", err)
	}

	// One in a transaction finishes it first.
	if got := availability(1, v16.AvailabilityInoperative); got != v16.StatusScheduled {
		t.Fatalf("ChangeAvailability during a transaction answered %s, want Scheduled", got)
	}
	if got := r.Snapshot().Connectors[0].Status; got != v16.StatusCharging {
		t.Errorf("connector 1 is %s, want it still Charging", got)
	}
	must(t)(r.Tap(ctx, 1, "TAG-1"))
	waitStatus(t, csms, 1, v16.StatusUnavailable)

	// It is remembered across a restart.
	saved := d.waitSaved(t, "both connectors as unavailable", func(st *charger.State) bool {
		return st.Connectors[0].Inoperative && st.Connectors[1].Inoperative
	})
	if snap := saved.Snapshot("CP-1", twoConnectorModel(), nil); snap.Connectors[0].Status != v16.StatusUnavailable {
		t.Errorf("the stopped charger shows connector 1 as %s", snap.Connectors[0].Status)
	}

	if got := availability(1, v16.AvailabilityOperative); got != v16.StatusAccepted {
		t.Fatalf("ChangeAvailability Operative answered %s", got)
	}
	waitStatus(t, csms, 1, v16.StatusFinishing)

	// Connector 0 is the charger as a whole.
	must(t)(r.Unplug(1))
	if got := availability(0, v16.AvailabilityInoperative); got != v16.StatusAccepted {
		t.Fatalf("ChangeAvailability on connector 0 answered %s", got)
	}
	waitStatus(t, csms, 0, v16.StatusUnavailable)
	waitStatus(t, csms, 1, v16.StatusUnavailable)
	if snap := r.Snapshot(); len(snap.Connectors) != 3 || snap.Connectors[0].ID != 0 || snap.Connectors[0].Status != v16.StatusUnavailable {
		t.Errorf("snapshot = %+v, want connector 0 listed as Unavailable", snap.Connectors)
	}
	availability(0, v16.AvailabilityOperative)
	waitStatus(t, csms, 0, v16.StatusAvailable)
	waitStatus(t, csms, 2, v16.StatusAvailable)

	for name, req := range map[string]v16.ChangeAvailabilityRequest{
		"an unknown connector": {ConnectorID: 9, Type: v16.AvailabilityOperative},
		"an unknown type":      {ConnectorID: 1, Type: "Broken"},
	} {
		if got := ask(t, csms, v16.ActionChangeAvailability, req); got != v16.StatusRejected {
			t.Errorf("ChangeAvailability with %s answered %s, want Rejected", name, got)
		}
	}
}

func TestUnlockConnector(t *testing.T) {
	csms := csmstest.Start(t)
	r := charging(t, csms, charger.Config{})

	if got := ask(t, csms, v16.ActionUnlockConnector, v16.UnlockConnectorRequest{ConnectorID: 1}); got != v16.StatusUnlocked {
		t.Fatalf("UnlockConnector answered %s", got)
	}
	if stop := stopRequest(t, csms); stop.Reason != v16.ReasonUnlockCommand {
		t.Errorf("StopTransaction reason = %s, want UnlockCommand", stop.Reason)
	}
	// The EV is still there until someone takes the cable out.
	waitStatus(t, csms, 1, v16.StatusFinishing)
	if r.Snapshot().Connectors[0].SoC == nil {
		t.Error("the EV is gone after UnlockConnector")
	}
	if got := ask(t, csms, v16.ActionUnlockConnector, v16.UnlockConnectorRequest{ConnectorID: 9}); got != v16.StatusNotSupported {
		t.Errorf("UnlockConnector for an unknown connector answered %s", got)
	}
}

func TestDataTransfer(t *testing.T) {
	csms := csmstest.Start(t)
	online(t, csms, charger.Config{Model: modelWith(func(m *model.Model) { m.DataTransfer = []string{"com.acme"} })})

	for vendor, want := range map[string]string{"com.acme": v16.StatusAccepted, "com.other": v16.StatusUnknownVendor} {
		var resp v16.DataTransferResponse
		if err := csms.Call(ctx, "CP-1", v16.ActionDataTransfer, v16.DataTransferRequest{VendorID: vendor, Data: "x"}, &resp); err != nil {
			t.Fatal(err)
		}
		if resp.Status != want {
			t.Errorf("DataTransfer for %s answered %s, want %s", vendor, resp.Status, want)
		}
	}
}

func TestTriggerMessage(t *testing.T) {
	csms := csmstest.Start(t)
	charging(t, csms, charger.Config{})
	trigger := func(message string, connector *int) string {
		return ask(t, csms, v16.ActionTriggerMessage, v16.TriggerMessageRequest{RequestedMessage: message, ConnectorID: connector})
	}

	before := len(statuses(t, csms, 2))
	if got := trigger(v16.ActionStatusNotification, intPtr(2)); got != v16.StatusAccepted {
		t.Fatalf("TriggerMessage StatusNotification answered %s", got)
	}
	deadline := time.Now().Add(5 * time.Second)
	for len(statuses(t, csms, 2)) != before+1 {
		if time.Now().After(deadline) {
			t.Fatal("no StatusNotification for connector 2 followed the trigger")
		}
		time.Sleep(5 * time.Millisecond)
	}

	if got := trigger(v16.ActionMeterValues, intPtr(1)); got != v16.StatusAccepted {
		t.Fatalf("TriggerMessage MeterValues answered %s", got)
	}
	var values v16.MeterValuesRequest
	csms.WaitForCalls(t, "CP-1", v16.ActionMeterValues, 1)[0].Decode(t, &values)
	if values.ConnectorID != 1 || values.TransactionID == nil || values.MeterValue[0].SampledValue[0].Context != v16.ContextTrigger {
		t.Errorf("triggered MeterValues = %+v, want connector 1, its transaction and context Trigger", values)
	}

	trigger(v16.ActionHeartbeat, nil)
	csms.WaitForCalls(t, "CP-1", v16.ActionHeartbeat, 1)
	trigger(v16.ActionBootNotification, nil)
	csms.WaitForCalls(t, "CP-1", v16.ActionBootNotification, 2)

	if got := trigger("MakeCoffee", nil); got != v16.StatusNotImplemented {
		t.Errorf("TriggerMessage for an unknown message answered %s, want NotImplemented", got)
	}
	if got := trigger(v16.ActionStatusNotification, intPtr(9)); got != v16.StatusRejected {
		t.Errorf("TriggerMessage for an unknown connector answered %s, want Rejected", got)
	}
}

func TestRequestsTheChargerDoesNotServe(t *testing.T) {
	csms := csmstest.Start(t)
	online(t, csms, charger.Config{})

	code := func(action string, payload any) string {
		t.Helper()
		err := csms.Call(ctx, "CP-1", action, payload, nil)
		var callErr *ocppj.CallError
		if !errors.As(err, &callErr) {
			t.Fatalf("%s returned %v, want an error answer", action, err)
		}
		return callErr.Code
	}

	// Feature profiles the charger does not have.
	for _, action := range v16.UnsupportedActions {
		if got := code(action, struct{}{}); got != ocppj.ErrNotSupported {
			t.Errorf("%s answered %s, want NotSupported", action, got)
		}
	}
	// A request OCPP 1.6 does not have.
	if got := code("MakeCoffee", struct{}{}); got != ocppj.ErrNotImplemented {
		t.Errorf("MakeCoffee answered %s, want NotImplemented", got)
	}
	// A request whose payload does not fit.
	if got := code(v16.ActionReset, map[string]any{"type": 5}); got != ocppj.ErrFormationViolation {
		t.Errorf("a Reset with a number for its type answered %s, want FormationViolation", got)
	}
}
