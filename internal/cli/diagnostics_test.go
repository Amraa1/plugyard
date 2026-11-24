package cli

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Amraa1/plugyard/internal/csmstest"
	"github.com/Amraa1/plugyard/internal/ocpp/v16"
)

func TestDiagnosticsUploadTheLogOfTheCharger(t *testing.T) {
	var mu sync.Mutex
	uploaded := map[string]string{}
	files := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		uploaded[r.URL.Path] = string(body)
		mu.Unlock()
	}))
	defer files.Close()

	useTempDataDir(t)
	csms := csmstest.Start(t)
	startRun(t, csms, "CP-1")
	startRun(t, csms, "CP-2")
	mustRun(t, "exec", "CP-1", "plug")

	asked := 0
	get := func(req v16.GetDiagnosticsRequest) string {
		t.Helper()
		asked++
		req.Location = files.URL + "/diagnostics/"
		var resp v16.GetDiagnosticsResponse
		if err := csms.Call(context.Background(), "CP-1", v16.ActionGetDiagnostics, req, &resp); err != nil {
			t.Fatalf("GetDiagnostics: %v", err)
		}
		// Uploading and Uploaded, for each request. Only then is the file
		// that of this request: two requests within one second get the
		// same file name, and the later upload replaces the earlier one.
		csms.WaitForCalls(t, "CP-1", v16.ActionDiagnosticsStatusNotification, 2*asked)
		mu.Lock()
		defer mu.Unlock()
		file, ok := uploaded["/diagnostics/"+resp.FileName]
		if !ok {
			t.Fatalf("no file %s was uploaded", resp.FileName)
		}
		return file
	}

	// The file is the log of this charger, as "plugyard logs" shows it.
	file := get(v16.GetDiagnosticsRequest{})
	for _, want := range []string{" CP-1 | powered on\n", " CP-1 | accepted by the CSMS\n", ` CP-1 > [2,"`, " CP-1 | connector 1: EV plugged in", " CP-1 | diagnostics: the CSMS asked for "} {
		if !strings.Contains(file, want) {
			t.Errorf("the diagnostics file is missing %q:\n%s", want, file)
		}
	}
	if strings.Contains(file, "CP-2") {
		t.Errorf("the diagnostics file of CP-1 has lines of CP-2:\n%s", file)
	}
	if logs := mustRun(t, "logs", "CP-1"); !strings.HasPrefix(logs, file) {
		t.Errorf("the diagnostics file is not the start of the charger's log:\n%s\nlog:\n%s", file, logs)
	}

	// startTime and stopTime choose the lines.
	if late := get(v16.GetDiagnosticsRequest{StartTime: &v16.Time{Time: time.Now().Add(time.Hour)}}); late != "plugyard has no log lines of charger CP-1 for the time asked for\n" {
		t.Errorf("diagnostics from an hour ahead:\n%s", late)
	}
	early := get(v16.GetDiagnosticsRequest{StopTime: &v16.Time{Time: time.Now().Add(-time.Hour)}})
	if strings.Contains(early, "powered on") {
		t.Errorf("diagnostics until an hour ago:\n%s", early)
	}
}
