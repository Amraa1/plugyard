package charger_test

import (
	"context"
	"encoding/json"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Amraa1/plugyard/internal/charger"
	"github.com/Amraa1/plugyard/internal/csmstest"
	"github.com/Amraa1/plugyard/internal/ev"
	"github.com/Amraa1/plugyard/internal/model"
	"github.com/Amraa1/plugyard/internal/ocpp/ocppj"
	"github.com/Amraa1/plugyard/internal/ocpp/v16"
)

// timeout returns a context that ends soon, for actions that would otherwise
// wait for the CSMS to confirm a transaction it cannot hear of yet.
func timeout(d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, d)
}

// lastStatus returns the last StatusNotification sent for a connector.
func lastStatus(t *testing.T, csms *csmstest.CSMS, connector int) v16.StatusNotificationRequest {
	t.Helper()
	var last v16.StatusNotificationRequest
	for _, call := range csms.Calls("CP-1") {
		if call.Action != v16.ActionStatusNotification {
			continue
		}
		var req v16.StatusNotificationRequest
		call.Decode(t, &req)
		if req.ConnectorID == connector {
			last = req
		}
	}
	return last
}

func TestFaultEndsTheTransactionAndClearRecovers(t *testing.T) {
	csms := csmstest.Start(t)
	d := &disk{}
	r := charging(t, csms, charger.Config{Save: d.save})

	outcome := must(t)(r.Fault(1, charger.Fault{Code: "GroundFailure", Info: "leak on L2", VendorCode: "E-42"}))
	if outcome != "fault GroundFailure reported, transaction 1 stopped" {
		t.Errorf("fault outcome = %q", outcome)
	}
	if stop := stopRequest(t, csms); stop.Reason != v16.ReasonOther {
		t.Errorf("StopTransaction reason = %s, want Other", stop.Reason)
	}
	waitStatus(t, csms, 1, v16.StatusFaulted)
	if st := lastStatus(t, csms, 1); st.ErrorCode != "GroundFailure" || st.Info != "leak on L2" || st.VendorErrorCode != "E-42" {
		t.Errorf("StatusNotification = %+v, want the fault's code, info and vendor code", st)
	}
	if snap := r.Snapshot().Connectors[0]; snap.Status != v16.StatusFaulted || snap.ErrorCode != "GroundFailure" {
		t.Errorf("snapshot = %+v", snap)
	}

	// Nothing starts on a faulted connector, by tag or from the CSMS.
	if _, err := r.Tap(ctx, 1, "TAG-1"); err == nil || !strings.Contains(err.Error(), "has a fault (GroundFailure); clear it first") {
		t.Errorf("tap on a faulted connector returned %v", err)
	}
	if got := ask(t, csms, v16.ActionRemoteStartTransaction, v16.RemoteStartTransactionRequest{ConnectorID: intPtr(1), IDTag: "REMOTE"}); got != v16.StatusRejected {
		t.Errorf("RemoteStartTransaction on a faulted connector answered %s", got)
	}

	// The fault outlives a restart.
	saved := d.waitSaved(t, "the fault", func(st *charger.State) bool { return st.Connectors[0].Fault != nil })
	if snap := saved.Snapshot("CP-1", twoConnectorModel(), nil).Connectors[0]; snap.Status != v16.StatusFaulted || snap.ErrorCode != "GroundFailure" {
		t.Errorf("the stopped charger shows connector 1 as %+v", snap)
	}

	// Clearing it returns the connector to where it stands: the EV is still
	// plugged in.
	if outcome := must(t)(r.ClearFault(1)); outcome != "fault GroundFailure cleared, connector 1 is Finishing" {
		t.Errorf("clear outcome = %q", outcome)
	}
	waitStatus(t, csms, 1, v16.StatusFinishing)
	if st := lastStatus(t, csms, 1); st.ErrorCode != v16.ErrorCodeNoError || st.Info != "" {
		t.Errorf("StatusNotification after clear = %+v, want NoError", st)
	}
	if _, err := r.ClearFault(1); err == nil || !strings.Contains(err.Error(), "connector 1 has no fault") {
		t.Errorf("a second clear returned %v", err)
	}

	// A mistyped code is answered with the codes there are.
	_, err := r.Fault(1, charger.Fault{Code: "Exploded"})
	if err == nil || !strings.Contains(err.Error(), "GroundFailure") || !strings.Contains(err.Error(), "WeakSignal") {
		t.Errorf("an unknown error code returned %v, want the list of codes", err)
	}
	if _, err := r.Fault(9, charger.Fault{Code: "OtherError"}); err == nil {
		t.Error("a fault on an unknown connector succeeded")
	}
}

func TestFaultOfTheWholeCharger(t *testing.T) {
	csms := csmstest.Start(t)
	d := &disk{}
	r := charging(t, csms, charger.Config{Save: d.save})

	outcome := must(t)(r.Fault(0, charger.Fault{Code: "InternalError"}))
	if !strings.HasSuffix(outcome, "transaction 1 stopped") {
		t.Errorf("fault outcome = %q, want the transaction stopped", outcome)
	}
	waitStatus(t, csms, 0, v16.StatusFaulted)
	if st := lastStatus(t, csms, 0); st.ErrorCode != "InternalError" {
		t.Errorf("StatusNotification for connector 0 = %+v", st)
	}
	if snap := r.Snapshot(); snap.Connectors[0].ID != 0 || snap.Connectors[0].ErrorCode != "InternalError" {
		t.Errorf("snapshot = %+v, want connector 0 listed first with the fault", snap.Connectors)
	}
	if _, err := r.Tap(ctx, 2, "TAG-2"); err == nil || !strings.Contains(err.Error(), "the charger has a fault") {
		t.Errorf("tap while the charger is faulted returned %v", err)
	}

	// After a restart the charger reports the fault again when it boots.
	saved := d.waitSaved(t, "the charger's fault", func(st *charger.State) bool { return st.Fault != nil })
	r.stop()
	before := len(statuses(t, csms, 0))
	online(t, csms, charger.Config{Restore: saved})
	deadline := time.Now().Add(5 * time.Second)
	for len(statuses(t, csms, 0)) == before {
		if time.Now().After(deadline) {
			t.Fatal("the restarted charger did not report connector 0")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if st := lastStatus(t, csms, 0); st.Status != v16.StatusFaulted || st.ErrorCode != "InternalError" {
		t.Errorf("StatusNotification for connector 0 after the restart = %+v", st)
	}
}

func TestATransactionSpanningANetworkLossReplaysInOrder(t *testing.T) {
	csms := csmstest.Start(t)
	r := charging(t, csms, charger.Config{Speed: 3600, Model: modelWith(func(m *model.Model) {
		m.Config["MeterValueSampleInterval"] = "1"
	})})

	if outcome := must(t)(r.Offline(0)); outcome != "offline" {
		t.Errorf("offline outcome = %q", outcome)
	}
	if ends := csms.WaitDisconnects(t, "CP-1", 1); ends[0].Clean {
		t.Error("going offline closed the connection normally, want it dropped")
	}
	if _, err := r.Offline(0); err == nil || !strings.Contains(err.Error(), "already offline") {
		t.Errorf("a second offline returned %v", err)
	}
	sent := len(csms.Calls("CP-1"))

	// Charging goes on while the charger is away, and the transaction ends
	// there too.
	time.Sleep(1200 * time.Millisecond)
	if snap := r.Snapshot(); snap.Connection != charger.Offline || snap.Connectors[0].Status != v16.StatusCharging {
		t.Errorf("snapshot while offline = %+v", snap)
	}
	must(t)(r.Unplug(1))
	if got := len(csms.Calls("CP-1")); got != sent {
		t.Fatalf("the CSMS received %d messages while the charger was offline", got-sent)
	}
	connects := csms.Connects("CP-1")
	time.Sleep(100 * time.Millisecond)
	if csms.Connects("CP-1") != connects {
		t.Fatal("the charger reconnected by itself while taken offline")
	}

	if outcome := must(t)(r.Online()); outcome != "online, connecting" {
		t.Errorf("online outcome = %q", outcome)
	}
	stop := stopRequest(t, csms)
	if stop.Reason != v16.ReasonEVDisconnected || stop.TransactionID != 1 || stop.MeterStop <= 0 {
		t.Errorf("StopTransaction = %+v", stop)
	}

	// What was queued arrives after the boot, in the order it happened:
	// meter values, then the stop, then where the connectors stand now.
	var replay []string
	var lastMeter, stopAt time.Time
	for _, call := range csms.Calls("CP-1")[sent:] {
		replay = append(replay, call.Action)
		switch call.Action {
		case v16.ActionMeterValues:
			var mv v16.MeterValuesRequest
			call.Decode(t, &mv)
			if mv.TransactionID == nil || *mv.TransactionID != 1 {
				t.Errorf("a replayed MeterValues carries transaction %v, want 1", mv.TransactionID)
			}
			lastMeter = mv.MeterValue[0].Timestamp.Time
		case v16.ActionStopTransaction:
			stopAt = stop.Timestamp.Time
		}
	}
	if len(replay) < 3 || replay[0] != v16.ActionBootNotification || replay[1] != v16.ActionMeterValues {
		t.Fatalf("after coming back the charger sent %v, want BootNotification, then MeterValues", replay)
	}
	if lastMeter.After(stopAt) {
		t.Errorf("a meter value (%s) is later than the stop (%s)", lastMeter, stopAt)
	}
	stopIndex := -1
	for i, action := range replay {
		if action == v16.ActionStopTransaction {
			stopIndex = i
		}
		if action == v16.ActionMeterValues && stopIndex != -1 {
			t.Errorf("MeterValues arrived after StopTransaction: %v", replay)
		}
	}
	waitStatus(t, csms, 1, v16.StatusAvailable)

	if _, err := r.Online(); err == nil || !strings.Contains(err.Error(), "was not taken offline") {
		t.Errorf("online while online returned %v", err)
	}
}

func TestOfflineForATimeComesBackByItself(t *testing.T) {
	csms := csmstest.Start(t)
	r := online(t, csms, charger.Config{})

	if outcome := must(t)(r.Offline(150 * time.Millisecond)); outcome != "offline for 150ms" {
		t.Errorf("offline outcome = %q", outcome)
	}
	csms.WaitDisconnects(t, "CP-1", 1)
	csms.WaitForCalls(t, "CP-1", v16.ActionBootNotification, 2)
	r.log.waitFor(t, charger.Event, "network connection restored")
}

func TestStartingATransactionWhileOffline(t *testing.T) {
	t.Run("refused by default", func(t *testing.T) {
		csms := csmstest.Start(t)
		r := online(t, csms, charger.Config{})
		must(t)(r.Plug(ctx, 1, ev.Defaults()))
		must(t)(r.Offline(0))
		csms.WaitDisconnects(t, "CP-1", 1)
		if _, err := r.Tap(ctx, 1, "TAG-1"); err == nil || !strings.Contains(err.Error(), "offline, so the tag cannot be authorized") {
			t.Errorf("tap while offline returned %v", err)
		}
	})

	t.Run("any tag with AllowOfflineTxForUnknownId", func(t *testing.T) {
		csms := csmstest.Start(t)
		r := online(t, csms, charger.Config{Model: modelWith(func(m *model.Model) {
			m.Config["AllowOfflineTxForUnknownId"] = "true"
		})})
		must(t)(r.Plug(ctx, 1, ev.Defaults()))
		must(t)(r.Offline(0))
		csms.WaitDisconnects(t, "CP-1", 1)

		short, cancel := timeout(50 * time.Millisecond)
		defer cancel()
		if outcome := must(t)(r.Tap(short, 1, "STRANGER")); !strings.HasPrefix(outcome, "tag accepted, transaction started") {
			t.Errorf("tap outcome = %q", outcome)
		}
		// The CSMS hears of the transaction when the charger is back.
		must(t)(r.Online())
		var start v16.StartTransactionRequest
		csms.WaitForCalls(t, "CP-1", v16.ActionStartTransaction, 1)[0].Decode(t, &start)
		if start.IDTag != "STRANGER" {
			t.Errorf("StartTransaction = %+v", start)
		}
	})

	t.Run("known tags with the authorization cache", func(t *testing.T) {
		csms := csmstest.Start(t)
		d := &disk{}
		r := online(t, csms, charger.Config{Save: d.save, Model: modelWith(func(m *model.Model) {
			m.Config["AuthorizationCacheEnabled"] = "true"
		})})

		// The CSMS accepts TAG-1 once while the charger is online.
		must(t)(r.Plug(ctx, 1, ev.Defaults()))
		must(t)(r.Tap(ctx, 1, "TAG-1"))
		must(t)(r.Tap(ctx, 1, "TAG-1"))
		must(t)(r.Unplug(1))
		saved := d.waitSaved(t, "the authorization cache", func(st *charger.State) bool { return len(st.AuthCache) == 1 })
		if saved.AuthCache["TAG-1"].Status != v16.AuthorizationAccepted {
			t.Errorf("saved cache = %+v", saved.AuthCache)
		}

		must(t)(r.Plug(ctx, 1, ev.Defaults()))
		must(t)(r.Offline(0))
		csms.WaitDisconnects(t, "CP-1", 1)
		if _, err := r.Tap(ctx, 1, "STRANGER"); err == nil {
			t.Error("a tag the CSMS never accepted was accepted offline")
		}
		short, cancel := timeout(50 * time.Millisecond)
		defer cancel()
		if outcome := must(t)(r.Tap(short, 1, "TAG-1")); !strings.HasPrefix(outcome, "tag accepted") {
			t.Errorf("tap of a cached tag outcome = %q", outcome)
		}
		r.log.waitFor(t, charger.Event, "tag TAG-1 accepted from the authorization cache while offline")

		// ClearCache empties it.
		must(t)(r.Online())
		csms.WaitForCalls(t, "CP-1", v16.ActionStartTransaction, 2)
		if got := ask(t, csms, v16.ActionClearCache, struct{}{}); got != v16.StatusAccepted {
			t.Fatalf("ClearCache answered %s", got)
		}
		d.waitSaved(t, "an empty cache", func(st *charger.State) bool { return len(st.AuthCache) == 0 })
	})
}

func TestTransactionMessageIsSentAgainAfterAnError(t *testing.T) {
	csms := csmstest.Start(t)
	var calls atomic.Int32
	csms.Handle(v16.ActionStartTransaction, func(string, json.RawMessage) (any, *ocppj.CallError) {
		if calls.Add(1) == 1 {
			return nil, &ocppj.CallError{Code: ocppj.ErrInternalError, Description: "database is down"}
		}
		return v16.StartTransactionResponse{IDTagInfo: v16.IDTagInfo{Status: v16.AuthorizationAccepted}, TransactionID: 7}, nil
	})
	r := online(t, csms, charger.Config{Model: modelWith(func(m *model.Model) {
		m.Config["TransactionMessageRetryInterval"] = "1"
	})})

	must(t)(r.Plug(ctx, 1, ev.Defaults()))
	short, cancel := timeout(50 * time.Millisecond)
	defer cancel()
	must(t)(r.Tap(short, 1, "TAG-1"))
	first := time.Now()
	r.log.waitFor(t, charger.Event, "sending it again in 1s (attempt 2 of 3)")

	// The same message is sent again after the interval, and the
	// transaction gets its ID.
	attempts := csms.WaitForCalls(t, "CP-1", v16.ActionStartTransaction, 2)
	if waited := time.Since(first); waited < 800*time.Millisecond {
		t.Errorf("the second attempt came after %s, want about 1s", waited)
	}
	if string(attempts[0].Payload) != string(attempts[1].Payload) {
		t.Errorf("the second attempt differs from the first:\n%s\n%s", attempts[0].Payload, attempts[1].Payload)
	}
	r.log.waitFor(t, charger.Event, "the CSMS confirmed transaction 7")
}

func TestTransactionMessageIsDroppedAfterTheLastAttempt(t *testing.T) {
	csms := csmstest.Start(t)
	// An answer that cannot be read counts as an error too.
	csms.Handle(v16.ActionStartTransaction, func(string, json.RawMessage) (any, *ocppj.CallError) {
		return map[string]any{"transactionId": "seven"}, nil
	})
	r := online(t, csms, charger.Config{Model: modelWith(func(m *model.Model) {
		m.Config["TransactionMessageAttempts"] = "2"
		m.Config["TransactionMessageRetryInterval"] = "1"
	})})

	must(t)(r.Plug(ctx, 1, ev.Defaults()))
	short, cancel := timeout(50 * time.Millisecond)
	defer cancel()
	t.Cleanup(func() {
		if t.Failed() {
			r.log.dump(t)
		}
	})
	must(t)(r.Tap(short, 1, "TAG-1"))
	r.log.waitFor(t, charger.Event, "with an error 2 times")
	r.log.waitFor(t, charger.Event, "the message is dropped")
	if n := count(csms, v16.ActionStartTransaction); n != 2 {
		t.Errorf("StartTransaction was sent %d times, want 2", n)
	}

	// The queue moves on: the stop that follows is still sent.
	must(t)(r.Unplug(1))
	if stop := stopRequest(t, csms); stop.TransactionID != 0 {
		t.Errorf("StopTransaction = %+v, want transaction 0: the CSMS never gave an ID", stop)
	}
}

func TestRebootByHand(t *testing.T) {
	csms := csmstest.Start(t)
	r := charging(t, csms, charger.Config{})

	if outcome := must(t)(r.Reboot()); outcome != "rebooting" {
		t.Errorf("reboot outcome = %q", outcome)
	}
	if stop := stopRequest(t, csms); stop.Reason != v16.ReasonReboot {
		t.Errorf("StopTransaction reason = %s, want Reboot", stop.Reason)
	}
	if ends := csms.WaitDisconnects(t, "CP-1", 1); !ends[0].Clean {
		t.Error("the reboot dropped the connection, want a normal close")
	}
	csms.WaitForCalls(t, "CP-1", v16.ActionBootNotification, 2)
}
