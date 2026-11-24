package charger_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Amraa1/plugyard/internal/charger"
	"github.com/Amraa1/plugyard/internal/csmstest"
	"github.com/Amraa1/plugyard/internal/model"
	"github.com/Amraa1/plugyard/internal/ocpp/ocppj"
	"github.com/Amraa1/plugyard/internal/ocpp/v16"
)

func twoConnectorModel() *model.Model {
	return &model.Model{
		Vendor:   "Acme",
		Name:     "FastOne 50",
		Firmware: "1.4.2",
		Connectors: []model.Connector{
			{ID: 1, Type: "CCS2", MaxPower: 50000},
			{ID: 2, Type: "CHAdeMO", MaxPower: 50000},
		},
		Config: map[string]string{},
	}
}

// logBook collects a charger's log entries.
type logBook struct {
	mu      sync.Mutex
	entries []charger.Entry
}

func (l *logBook) add(e charger.Entry) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.entries = append(l.entries, e)
}

func (l *logBook) has(kind charger.EntryKind, substr string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, e := range l.entries {
		if e.Kind == kind && strings.Contains(e.Text, substr) {
			return true
		}
	}
	return false
}

func (l *logBook) waitFor(t *testing.T, kind charger.EntryKind, substr string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !l.has(kind, substr) {
		if time.Now().After(deadline) {
			t.Fatalf("no log entry containing %q", substr)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// running is a charger powered on for the length of a test.
type running struct {
	*charger.Charger
	log  *logBook
	stop func() // orderly shutdown; waits for Run to return
}

func powerOn(t *testing.T, cfg charger.Config) *running {
	t.Helper()
	log := &logBook{}
	cfg.Log = log.add
	if cfg.Model == nil {
		cfg.Model = twoConnectorModel()
	}
	cfg.Timing = charger.Timing{
		CallTimeout:  2 * time.Second,
		ReconnectMin: 10 * time.Millisecond,
		ReconnectMax: 50 * time.Millisecond,
		Tick:         10 * time.Millisecond,
		Reboot:       20 * time.Millisecond,
		Download:     40 * time.Millisecond,
		Install:      40 * time.Millisecond,
		Upload:       40 * time.Millisecond,
	}

	c, err := charger.New(cfg)
	if err != nil {
		t.Fatalf("charger.New: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- c.Run(ctx) }()

	var once sync.Once
	stop := func() {
		once.Do(func() {
			cancel()
			select {
			case err := <-done:
				if err != nil {
					t.Errorf("Run returned %v, want nil after an orderly shutdown", err)
				}
			case <-time.After(5 * time.Second):
				t.Error("Run did not return after its context was cancelled")
			}
		})
	}
	t.Cleanup(stop)
	return &running{Charger: c, log: log, stop: stop}
}

func TestBootHandshake(t *testing.T) {
	csms := csmstest.Start(t)
	powerOn(t, charger.Config{ID: "CP-1", CSMS: charger.CSMS{URL: csms.URL()}})

	statuses := csms.WaitForCalls(t, "CP-1", v16.ActionStatusNotification, 2)

	// BootNotification comes first and carries the model's identity.
	calls := csms.Calls("CP-1")
	if calls[0].Action != v16.ActionBootNotification {
		t.Fatalf("first call is %s, want BootNotification; calls: %v", calls[0].Action, csms.Actions("CP-1"))
	}
	var boot v16.BootNotificationRequest
	calls[0].Decode(t, &boot)
	want := v16.BootNotificationRequest{ChargePointVendor: "Acme", ChargePointModel: "FastOne 50", FirmwareVersion: "1.4.2"}
	if boot != want {
		t.Errorf("BootNotification = %+v, want %+v", boot, want)
	}

	// Then one StatusNotification per connector, in order.
	for i, call := range statuses {
		var status v16.StatusNotificationRequest
		call.Decode(t, &status)
		if status.ConnectorID != i+1 || status.Status != v16.StatusAvailable || status.ErrorCode != v16.ErrorCodeNoError {
			t.Errorf("StatusNotification %d = %+v, want connector %d Available with NoError", i, status, i+1)
		}
		if status.Timestamp == nil || time.Since(status.Timestamp.Time) > time.Minute {
			t.Errorf("StatusNotification %d has timestamp %v, want the current time", i, status.Timestamp)
		}
	}
}

func TestMessagesAreWellFormedOCPPJ(t *testing.T) {
	csms := csmstest.Start(t)
	r := powerOn(t, charger.Config{ID: "CP-1", CSMS: charger.CSMS{URL: csms.URL()}})
	csms.WaitForCalls(t, "CP-1", v16.ActionStatusNotification, 2)

	// The log holds the raw frames in both directions.
	r.log.waitFor(t, charger.MessageSent, `"BootNotification",{"chargePointVendor":"Acme"`)
	r.log.waitFor(t, charger.MessageReceived, `"status":"Accepted"`)
	r.log.waitFor(t, charger.Event, "accepted by the CSMS")

	// Timestamps are UTC with milliseconds, as OCPP 1.6 examples show them.
	var status struct{ Timestamp string }
	csms.Calls("CP-1")[1].Decode(t, &status)
	if _, err := time.Parse("2006-01-02T15:04:05.000Z", status.Timestamp); err != nil {
		t.Errorf("timestamp %q is not in the form 2006-01-02T15:04:05.000Z", status.Timestamp)
	}
}

func TestHeartbeatUsesIntervalFromBootNotification(t *testing.T) {
	csms := csmstest.Start(t, csmstest.HeartbeatInterval(1))
	powerOn(t, charger.Config{ID: "CP-1", CSMS: charger.CSMS{URL: csms.URL()}})

	beats := csms.WaitForCalls(t, "CP-1", v16.ActionHeartbeat, 2)

	gap := beats[1].Time.Sub(beats[0].Time)
	if gap < 800*time.Millisecond || gap > 1500*time.Millisecond {
		t.Errorf("heartbeats are %s apart, want about 1s", gap)
	}
	if string(beats[0].Payload) != "{}" {
		t.Errorf("Heartbeat payload = %s, want {}", beats[0].Payload)
	}
}

func TestStaysQuietUntilAccepted(t *testing.T) {
	csms := csmstest.Start(t)

	var mu sync.Mutex
	boots := 0
	csms.Handle(v16.ActionBootNotification, func(string, json.RawMessage) (any, *ocppj.CallError) {
		mu.Lock()
		defer mu.Unlock()
		boots++
		status := v16.RegistrationPending
		if boots > 1 {
			status = v16.RegistrationAccepted
		}
		return v16.BootNotificationResponse{Status: status, CurrentTime: v16.Time{Time: time.Now()}, Interval: 1}, nil
	})

	powerOn(t, charger.Config{ID: "CP-1", CSMS: charger.CSMS{URL: csms.URL()}})

	csms.WaitForCalls(t, "CP-1", v16.ActionStatusNotification, 2)

	got := csms.Actions("CP-1")[:3]
	want := []string{v16.ActionBootNotification, v16.ActionBootNotification, v16.ActionStatusNotification}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("calls = %v, want %v", got, want)
	}
	calls := csms.Calls("CP-1")
	if gap := calls[1].Time.Sub(calls[0].Time); gap < 800*time.Millisecond {
		t.Errorf("second BootNotification came after %s, want it to wait the 1s interval", gap)
	}
}

func TestReconnectsAndBootsAgainAfterConnectionLoss(t *testing.T) {
	csms := csmstest.Start(t)
	r := powerOn(t, charger.Config{ID: "CP-1", CSMS: charger.CSMS{URL: csms.URL()}})
	csms.WaitForCalls(t, "CP-1", v16.ActionStatusNotification, 2)

	csms.Drop("CP-1")

	csms.WaitForCalls(t, "CP-1", v16.ActionBootNotification, 2)
	csms.WaitForCalls(t, "CP-1", v16.ActionStatusNotification, 4)
	if got := csms.Connects("CP-1"); got != 2 {
		t.Errorf("charger connected %d times, want 2", got)
	}
	r.log.waitFor(t, charger.Event, "connection lost")
}

func TestKeepsTryingWhileTheCSMSIsUnreachable(t *testing.T) {
	// Nothing listens on this port.
	r := powerOn(t, charger.Config{ID: "CP-1", CSMS: charger.CSMS{URL: "ws://127.0.0.1:1/ocpp"}})

	r.log.waitFor(t, charger.Event, "could not connect")
	r.log.waitFor(t, charger.Event, "retrying in")
	r.stop() // must return promptly even though it never connected
}

func TestOrderlyShutdownClosesTheConnectionCleanly(t *testing.T) {
	csms := csmstest.Start(t)
	r := powerOn(t, charger.Config{ID: "CP-1", CSMS: charger.CSMS{URL: csms.URL()}})
	csms.WaitForCalls(t, "CP-1", v16.ActionStatusNotification, 2)

	r.stop()

	ends := csms.WaitDisconnects(t, "CP-1", 1)
	if !ends[0].Clean {
		t.Error("the connection was dropped, want a normal WebSocket close")
	}
	if got := csms.Connects("CP-1"); got != 1 {
		t.Errorf("charger connected %d times, want 1 (no reconnect after shutdown)", got)
	}
}

func TestBasicAuth(t *testing.T) {
	t.Run("correct password", func(t *testing.T) {
		csms := csmstest.Start(t, csmstest.RequirePassword("s3cret"))
		powerOn(t, charger.Config{ID: "CP-1", CSMS: charger.CSMS{URL: csms.URL(), Password: "s3cret"}})

		csms.WaitForCalls(t, "CP-1", v16.ActionBootNotification, 1)
	})

	t.Run("wrong password", func(t *testing.T) {
		csms := csmstest.Start(t, csmstest.RequirePassword("s3cret"))
		r := powerOn(t, charger.Config{ID: "CP-1", CSMS: charger.CSMS{URL: csms.URL(), Password: "wrong"}})

		r.log.waitFor(t, charger.Event, "401")
		if got := csms.Connects("CP-1"); got != 0 {
			t.Errorf("charger connected %d times, want 0", got)
		}
	})
}

func TestChargePointIDIsEscapedInTheURL(t *testing.T) {
	csms := csmstest.Start(t)
	powerOn(t, charger.Config{ID: "Depot A#1", CSMS: charger.CSMS{URL: csms.URL() + "/"}})

	csms.WaitForCalls(t, "Depot A#1", v16.ActionBootNotification, 1)
}

func TestWarnsWhenTheCSMSDoesNotSelectTheSubprotocol(t *testing.T) {
	csms := csmstest.Start(t, csmstest.WithoutSubprotocol())
	r := powerOn(t, charger.Config{ID: "CP-1", CSMS: charger.CSMS{URL: csms.URL()}})

	r.log.waitFor(t, charger.Event, "subprotocol")
	// It carries on regardless.
	csms.WaitForCalls(t, "CP-1", v16.ActionBootNotification, 1)
}

func TestAnswersUnknownRequestsWithNotImplemented(t *testing.T) {
	csms := csmstest.Start(t)
	powerOn(t, charger.Config{ID: "CP-1", CSMS: charger.CSMS{URL: csms.URL()}})
	csms.WaitForCalls(t, "CP-1", v16.ActionStatusNotification, 2)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := csms.Call(ctx, "CP-1", "MadeUpAction", map[string]any{}, nil)

	var callErr *ocppj.CallError
	if !errors.As(err, &callErr) || callErr.Code != ocppj.ErrNotImplemented {
		t.Errorf("Call returned %v, want a NotImplemented call error", err)
	}
}

func TestNewRejectsBadConfig(t *testing.T) {
	cases := map[string]charger.Config{
		"empty ID":         {ID: " ", CSMS: charger.CSMS{URL: "ws://localhost/ocpp"}},
		"http URL":         {ID: "CP-1", CSMS: charger.CSMS{URL: "http://localhost/ocpp"}},
		"URL without host": {ID: "CP-1", CSMS: charger.CSMS{URL: "ws:///ocpp"}},
		"empty URL":        {ID: "CP-1"},
	}
	for name, cfg := range cases {
		t.Run(name, func(t *testing.T) {
			cfg.Model = twoConnectorModel()
			if _, err := charger.New(cfg); err == nil {
				t.Error("New succeeded, want an error")
			}
		})
	}
}

// dump prints the log, for working out why a test failed.
func (l *logBook) dump(t *testing.T) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, e := range l.entries {
		t.Logf("%s %d %s", e.Time.Format("15:04:05.000"), e.Kind, e.Text)
	}
}
