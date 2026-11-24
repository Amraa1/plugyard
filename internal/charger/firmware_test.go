package charger_test

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Amraa1/plugyard/internal/charger"
	"github.com/Amraa1/plugyard/internal/csmstest"
	"github.com/Amraa1/plugyard/internal/ev"
	"github.com/Amraa1/plugyard/internal/ocpp/ocppj"
	"github.com/Amraa1/plugyard/internal/ocpp/v16"
)

// transferStatuses returns the statuses the charger has reported with
// FirmwareStatusNotification or DiagnosticsStatusNotification, in order.
func transferStatuses(t *testing.T, csms *csmstest.CSMS, action string) []string {
	t.Helper()
	var out []string
	for _, call := range csms.Calls("CP-1") {
		if call.Action == action {
			var req v16.TransferStatusNotification
			call.Decode(t, &req)
			out = append(out, req.Status)
		}
	}
	return out
}

// waitTransfer waits until the charger has reported exactly the given
// statuses, and fails if it reports anything else.
func waitTransfer(t *testing.T, csms *csmstest.CSMS, action string, want ...string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		got := transferStatuses(t, csms, action)
		if slices.Equal(got, want) {
			return
		}
		if len(got) > len(want) || !slices.Equal(got, want[:len(got)]) || time.Now().After(deadline) {
			t.Fatalf("%s reported %v, want %v", action, got, want)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// update asks the charger to update its firmware now.
func update(t *testing.T, csms *csmstest.CSMS, req v16.UpdateFirmwareRequest) {
	t.Helper()
	if req.Location == "" {
		req.Location = "ftp://files.example/firmware/fw-2.4.1.bin"
	}
	if req.RetrieveDate.IsZero() {
		req.RetrieveDate = v16.Time{Time: time.Now()}
	}
	if err := csms.Call(ctx, "CP-1", v16.ActionUpdateFirmware, req, nil); err != nil {
		t.Fatalf("UpdateFirmware: %v", err)
	}
}

func bootedWith(t *testing.T, csms *csmstest.CSMS, n int) string {
	t.Helper()
	var boot v16.BootNotificationRequest
	csms.WaitForCalls(t, "CP-1", v16.ActionBootNotification, n)[n-1].Decode(t, &boot)
	return boot.FirmwareVersion
}

func TestFirmwareUpdate(t *testing.T) {
	csms := csmstest.Start(t)
	var saved charger.State
	r := online(t, csms, charger.Config{Save: func(st charger.State) error { saved = st; return nil }})
	if got := bootedWith(t, csms, 1); got != "1.4.2" {
		t.Fatalf("the charger booted with firmware %q, want the one of its model", got)
	}

	update(t, csms, v16.UpdateFirmwareRequest{})
	waitTransfer(t, csms, v16.ActionFirmwareStatusNotification,
		v16.FirmwareDownloading, v16.FirmwareDownloaded, v16.FirmwareInstalling, v16.FirmwareInstalled)

	// The charger reboots, and says what it runs now.
	if got := bootedWith(t, csms, 2); got != "fw-2.4.1" {
		t.Errorf("after the update the charger booted with firmware %q, want fw-2.4.1", got)
	}
	r.log.waitFor(t, charger.Event, "firmware: fw-2.4.1 installed; rebooting")
	// Installed was reported before the charger went away.
	calls := csms.Actions("CP-1")
	if slices.Index(calls, v16.ActionFirmwareStatusNotification) > slices.Index(calls[1:], v16.ActionBootNotification)+1 {
		t.Errorf("the first firmware status came after the second boot: %v", calls)
	}
	// The connectors were kept free for the installation, and are back.
	if got := statuses(t, csms, 1); !slices.Contains(got, v16.StatusUnavailable) {
		t.Errorf("connector 1 reported %v, want Unavailable during the installation", got)
	}
	waitStatus(t, csms, 1, v16.StatusAvailable)
	if got := r.Snapshot().Connectors[0].Status; got != v16.StatusAvailable {
		t.Errorf("connector 1 after the update is %s, want Available", got)
	}

	// The firmware stays with the charger when it is powered off and on.
	r.stop()
	if saved.Firmware != "fw-2.4.1" {
		t.Fatalf("the saved state has firmware %q", saved.Firmware)
	}
	online(t, csms, charger.Config{Restore: &saved})
	if got := bootedWith(t, csms, 3); got != "fw-2.4.1" {
		t.Errorf("the restored charger booted with firmware %q", got)
	}
}

func TestFirmwareWaitsForTheRetrieveDate(t *testing.T) {
	csms := csmstest.Start(t)
	online(t, csms, charger.Config{})
	trigger := func() {
		t.Helper()
		req := v16.TriggerMessageRequest{RequestedMessage: v16.ActionFirmwareStatusNotification}
		if got := ask(t, csms, v16.ActionTriggerMessage, req); got != v16.StatusAccepted {
			t.Fatalf("TriggerMessage FirmwareStatusNotification answered %s", got)
		}
	}

	at := time.Now().Add(400 * time.Millisecond)
	update(t, csms, v16.UpdateFirmwareRequest{RetrieveDate: v16.Time{Time: at}})
	// Until then the charger has nothing to report.
	trigger()
	waitTransfer(t, csms, v16.ActionFirmwareStatusNotification, v16.FirmwareIdle)

	calls := csms.WaitForCalls(t, "CP-1", v16.ActionFirmwareStatusNotification, 2)
	if calls[1].Time.Before(at.Add(-20 * time.Millisecond)) {
		t.Errorf("the download started %s before the retrieve date", at.Sub(calls[1].Time))
	}
}

func TestFirmwareIsInstalledOnceTransactionsHaveEnded(t *testing.T) {
	csms := csmstest.Start(t)
	r := charging(t, csms, charger.Config{})

	update(t, csms, v16.UpdateFirmwareRequest{})
	waitTransfer(t, csms, v16.ActionFirmwareStatusNotification, v16.FirmwareDownloading, v16.FirmwareDownloaded)
	r.log.waitFor(t, charger.Event, "waiting for 1 transaction(s) to end before installing")

	// The free connector takes no new transaction; the busy one goes on.
	waitStatus(t, csms, 2, v16.StatusUnavailable)
	must(t)(r.Plug(ctx, 2, ev.Defaults()))
	if _, err := r.Tap(ctx, 2, "TAG-2"); err == nil || !strings.Contains(err.Error(), "installing firmware") {
		t.Errorf("Tap on a connector kept free for the installation returned %v", err)
	}
	time.Sleep(150 * time.Millisecond)
	if got := r.Snapshot().Connectors[0].Status; got != v16.StatusCharging {
		t.Errorf("connector 1 is %s, want Charging", got)
	}
	waitTransfer(t, csms, v16.ActionFirmwareStatusNotification, v16.FirmwareDownloading, v16.FirmwareDownloaded)

	must(t)(r.Unplug(1))
	waitTransfer(t, csms, v16.ActionFirmwareStatusNotification,
		v16.FirmwareDownloading, v16.FirmwareDownloaded, v16.FirmwareInstalling, v16.FirmwareInstalled)
	if got := bootedWith(t, csms, 2); got != "fw-2.4.1" {
		t.Errorf("after the update the charger booted with firmware %q", got)
	}
}

func TestFirmwareFailures(t *testing.T) {
	t.Run("download", func(t *testing.T) {
		csms := csmstest.Start(t)
		r := online(t, csms, charger.Config{})
		if got := must(t)(r.Fail(charger.FailDownload)); got != "the next firmware download will fail" {
			t.Errorf("Fail answered %q", got)
		}
		if _, err := r.Fail(charger.FailDownload); err == nil {
			t.Error("Fail twice succeeded")
		}

		update(t, csms, v16.UpdateFirmwareRequest{Retries: intPtr(1)})
		waitTransfer(t, csms, v16.ActionFirmwareStatusNotification, v16.FirmwareDownloading, v16.FirmwareDownloadFailed)
		// It was tried as often as the CSMS asked.
		r.log.waitFor(t, charger.Event, "firmware: downloading again (attempt 2 of 2)")

		// The failure is spent: the next update goes through.
		update(t, csms, v16.UpdateFirmwareRequest{})
		waitTransfer(t, csms, v16.ActionFirmwareStatusNotification,
			v16.FirmwareDownloading, v16.FirmwareDownloadFailed,
			v16.FirmwareDownloading, v16.FirmwareDownloaded, v16.FirmwareInstalling, v16.FirmwareInstalled)
	})

	t.Run("install", func(t *testing.T) {
		csms := csmstest.Start(t)
		r := online(t, csms, charger.Config{})
		must(t)(r.Fail(charger.FailInstall))

		update(t, csms, v16.UpdateFirmwareRequest{})
		waitTransfer(t, csms, v16.ActionFirmwareStatusNotification,
			v16.FirmwareDownloading, v16.FirmwareDownloaded, v16.FirmwareInstalling, v16.FirmwareInstallationFailed)
		r.log.waitFor(t, charger.Event, "the installation failed; the charger keeps firmware 1.4.2")

		// No reboot, the old firmware, and the connectors are back.
		waitStatus(t, csms, 1, v16.StatusUnavailable)
		deadline := time.Now().Add(5 * time.Second)
		for r.Snapshot().Connectors[0].Status != v16.StatusAvailable {
			if time.Now().After(deadline) {
				t.Fatal("connector 1 did not become Available again")
			}
			time.Sleep(5 * time.Millisecond)
		}
		if n := count(csms, v16.ActionBootNotification); n != 1 {
			t.Errorf("the charger booted %d times, want 1", n)
		}
		if got := r.State().Firmware; got != "" {
			t.Errorf("the state has firmware %q after a failed installation", got)
		}
	})

	csms := csmstest.Start(t)
	r := online(t, csms, charger.Config{})
	if _, err := r.Fail("coffee"); err == nil {
		t.Error("Fail of an unknown step succeeded")
	}
	// An update with nowhere to fetch it from is refused.
	err := csms.Call(ctx, "CP-1", v16.ActionUpdateFirmware, v16.UpdateFirmwareRequest{RetrieveDate: v16.Time{Time: time.Now()}}, nil)
	var callErr *ocppj.CallError
	if !errors.As(err, &callErr) || callErr.Code != ocppj.ErrPropertyConstraintViolation {
		t.Errorf("UpdateFirmware with no location returned %v", err)
	}
}

func TestDiagnostics(t *testing.T) {
	csms := csmstest.Start(t)
	r := online(t, csms, charger.Config{})
	get := func(req v16.GetDiagnosticsRequest) string {
		t.Helper()
		var resp v16.GetDiagnosticsResponse
		if err := csms.Call(ctx, "CP-1", v16.ActionGetDiagnostics, req, &resp); err != nil {
			t.Fatalf("GetDiagnostics: %v", err)
		}
		return resp.FileName
	}
	trigger := v16.TriggerMessageRequest{RequestedMessage: v16.ActionDiagnosticsStatusNotification}

	// With nothing under way, the charger is idle.
	if got := ask(t, csms, v16.ActionTriggerMessage, trigger); got != v16.StatusAccepted {
		t.Fatalf("TriggerMessage DiagnosticsStatusNotification answered %s", got)
	}
	waitTransfer(t, csms, v16.ActionDiagnosticsStatusNotification, v16.DiagnosticsIdle)

	name := get(v16.GetDiagnosticsRequest{Location: "ftp://files.example/diagnostics/"})
	if !strings.HasPrefix(name, "diagnostics-CP-1-") || !strings.HasSuffix(name, ".log") {
		t.Errorf("GetDiagnostics named the file %q", name)
	}
	waitTransfer(t, csms, v16.ActionDiagnosticsStatusNotification,
		v16.DiagnosticsIdle, v16.DiagnosticsUploading, v16.DiagnosticsUploaded)

	must(t)(r.Fail(charger.FailUpload))
	get(v16.GetDiagnosticsRequest{Location: "ftp://files.example/diagnostics/", Retries: intPtr(2)})
	waitTransfer(t, csms, v16.ActionDiagnosticsStatusNotification,
		v16.DiagnosticsIdle, v16.DiagnosticsUploading, v16.DiagnosticsUploaded,
		v16.DiagnosticsUploading, v16.DiagnosticsUploadFailed)
	r.log.waitFor(t, charger.Event, "diagnostics: uploading again (attempt 3 of 3)")

	err := csms.Call(ctx, "CP-1", v16.ActionGetDiagnostics, v16.GetDiagnosticsRequest{}, nil)
	var callErr *ocppj.CallError
	if !errors.As(err, &callErr) || callErr.Code != ocppj.ErrPropertyConstraintViolation {
		t.Errorf("GetDiagnostics with no location returned %v", err)
	}
}
