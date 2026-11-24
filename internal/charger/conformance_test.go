package charger_test

import (
	"errors"
	"fmt"
	"net"
	"sync"
	"testing"
	"time"

	ocpp16 "github.com/lorenzodonini/ocpp-go/ocpp1.6"
	"github.com/lorenzodonini/ocpp-go/ocpp1.6/core"
	"github.com/lorenzodonini/ocpp-go/ocpp1.6/smartcharging"
	"github.com/lorenzodonini/ocpp-go/ocpp1.6/types"

	"github.com/Amraa1/plugyard/internal/charger"
)

// The other tests talk to a fake CSMS built on plugyard's own OCPP code, so a
// mistake in that code could hide on both sides. This test runs the charger
// against the central system of ocpp-go, an independent OCPP 1.6
// implementation that validates every message it receives.

type recordingCentralSystem struct {
	mu       sync.Mutex
	boots    []core.BootNotificationRequest
	statuses []core.StatusNotificationRequest
	beats    int
}

func (r *recordingCentralSystem) OnBootNotification(_ string, req *core.BootNotificationRequest) (*core.BootNotificationConfirmation, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.boots = append(r.boots, *req)
	return core.NewBootNotificationConfirmation(types.NewDateTime(time.Now()), 1, core.RegistrationStatusAccepted), nil
}

func (r *recordingCentralSystem) OnStatusNotification(_ string, req *core.StatusNotificationRequest) (*core.StatusNotificationConfirmation, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.statuses = append(r.statuses, *req)
	return core.NewStatusNotificationConfirmation(), nil
}

func (r *recordingCentralSystem) OnHeartbeat(string, *core.HeartbeatRequest) (*core.HeartbeatConfirmation, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.beats++
	return core.NewHeartbeatConfirmation(types.NewDateTime(time.Now())), nil
}

var errUnexpected = errors.New("not expected in this test")

func (r *recordingCentralSystem) OnAuthorize(string, *core.AuthorizeRequest) (*core.AuthorizeConfirmation, error) {
	return nil, errUnexpected
}

func (r *recordingCentralSystem) OnDataTransfer(string, *core.DataTransferRequest) (*core.DataTransferConfirmation, error) {
	return nil, errUnexpected
}

func (r *recordingCentralSystem) OnMeterValues(string, *core.MeterValuesRequest) (*core.MeterValuesConfirmation, error) {
	return nil, errUnexpected
}

func (r *recordingCentralSystem) OnStartTransaction(string, *core.StartTransactionRequest) (*core.StartTransactionConfirmation, error) {
	return nil, errUnexpected
}

func (r *recordingCentralSystem) OnStopTransaction(string, *core.StopTransactionRequest) (*core.StopTransactionConfirmation, error) {
	return nil, errUnexpected
}

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func TestHandshakeAgainstIndependentCentralSystem(t *testing.T) {
	rec := &recordingCentralSystem{}
	cs := ocpp16.NewCentralSystem(nil, nil)
	cs.SetCoreHandler(rec)

	port := freePort(t)
	go cs.Start(port, "/{ws}")
	t.Cleanup(cs.Stop)

	// The charger retries until the central system is listening.
	powerOn(t, charger.Config{
		ID:   "CP-1",
		CSMS: charger.CSMS{URL: fmt.Sprintf("ws://127.0.0.1:%d", port)},
	})

	// A heartbeat arrives only after ocpp-go has accepted the
	// BootNotification and both StatusNotifications as valid.
	deadline := time.Now().Add(10 * time.Second)
	for {
		rec.mu.Lock()
		done := rec.beats >= 1
		rec.mu.Unlock()
		if done {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no heartbeat reached the central system")
		}
		time.Sleep(10 * time.Millisecond)
	}

	rec.mu.Lock()
	defer rec.mu.Unlock()

	if len(rec.boots) != 1 {
		t.Fatalf("got %d BootNotifications, want 1", len(rec.boots))
	}
	boot := rec.boots[0]
	if boot.ChargePointVendor != "Acme" || boot.ChargePointModel != "FastOne 50" || boot.FirmwareVersion != "1.4.2" {
		t.Errorf("BootNotification = %+v", boot)
	}

	if len(rec.statuses) != 2 {
		t.Fatalf("got %d StatusNotifications, want 2", len(rec.statuses))
	}
	for i, status := range rec.statuses {
		if status.ConnectorId != i+1 || status.Status != core.ChargePointStatusAvailable || status.ErrorCode != core.NoError {
			t.Errorf("StatusNotification %d = %+v", i, status)
		}
	}
}

func TestSmartChargingAgainstIndependentCentralSystem(t *testing.T) {
	rec := &recordingCentralSystem{}
	cs := ocpp16.NewCentralSystem(nil, nil)
	cs.SetCoreHandler(rec)

	port := freePort(t)
	go cs.Start(port, "/{ws}")
	t.Cleanup(cs.Stop)

	powerOn(t, charger.Config{
		ID:   "CP-1",
		CSMS: charger.CSMS{URL: fmt.Sprintf("ws://127.0.0.1:%d", port)},
	})
	deadline := time.Now().Add(10 * time.Second)
	for {
		rec.mu.Lock()
		booted := len(rec.statuses) >= 2
		rec.mu.Unlock()
		if booted {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the charger did not boot against the central system")
		}
		time.Sleep(10 * time.Millisecond)
	}

	// ocpp-go builds each request and validates each answer.
	fail := make(chan error, 3)
	set := make(chan *smartcharging.SetChargingProfileConfirmation, 1)
	profile := types.NewChargingProfile(1, 0, types.ChargingProfilePurposeTxDefaultProfile, types.ChargingProfileKindAbsolute,
		types.NewChargingSchedule(types.ChargingRateUnitAmperes, types.NewChargingSchedulePeriod(0, 16)))
	if err := cs.SetChargingProfile("CP-1", func(conf *smartcharging.SetChargingProfileConfirmation, err error) {
		if err != nil {
			fail <- err
			return
		}
		set <- conf
	}, 1, profile); err != nil {
		t.Fatalf("SetChargingProfile: %v", err)
	}
	select {
	case conf := <-set:
		if conf.Status != smartcharging.ChargingProfileStatusAccepted {
			t.Fatalf("SetChargingProfile answered %s", conf.Status)
		}
	case err := <-fail:
		t.Fatalf("SetChargingProfile: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("no answer to SetChargingProfile")
	}

	composite := make(chan *smartcharging.GetCompositeScheduleConfirmation, 1)
	if err := cs.GetCompositeSchedule("CP-1", func(conf *smartcharging.GetCompositeScheduleConfirmation, err error) {
		if err != nil {
			fail <- err
			return
		}
		composite <- conf
	}, 1, 3600, func(req *smartcharging.GetCompositeScheduleRequest) {
		req.ChargingRateUnit = types.ChargingRateUnitAmperes
	}); err != nil {
		t.Fatalf("GetCompositeSchedule: %v", err)
	}
	select {
	case conf := <-composite:
		if conf.Status != smartcharging.GetCompositeScheduleStatusAccepted || conf.ChargingSchedule == nil {
			t.Fatalf("GetCompositeSchedule answered %+v", conf)
		}
		periods := conf.ChargingSchedule.ChargingSchedulePeriod
		if len(periods) != 1 || periods[0].Limit != 16 || conf.ChargingSchedule.ChargingRateUnit != types.ChargingRateUnitAmperes {
			t.Errorf("composite schedule = %+v, want 16 A throughout", conf.ChargingSchedule)
		}
	case err := <-fail:
		t.Fatalf("GetCompositeSchedule: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("no answer to GetCompositeSchedule")
	}

	cleared := make(chan *smartcharging.ClearChargingProfileConfirmation, 1)
	if err := cs.ClearChargingProfile("CP-1", func(conf *smartcharging.ClearChargingProfileConfirmation, err error) {
		if err != nil {
			fail <- err
			return
		}
		cleared <- conf
	}); err != nil {
		t.Fatalf("ClearChargingProfile: %v", err)
	}
	select {
	case conf := <-cleared:
		if conf.Status != smartcharging.ClearChargingProfileStatusAccepted {
			t.Errorf("ClearChargingProfile answered %s", conf.Status)
		}
	case err := <-fail:
		t.Fatalf("ClearChargingProfile: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("no answer to ClearChargingProfile")
	}
}
