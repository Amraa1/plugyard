package cli

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/Amraa1/plugyard/internal/csmstest"
	"github.com/Amraa1/plugyard/internal/ocpp/v16"
	"github.com/Amraa1/plugyard/internal/selfupdate"
)

// fakeRelease serves one release of plugyard for the system the test runs
// on, whose program is the given text, and points "plugyard update" at it.
// It returns the path of the stand-in for the installed program.
func fakeRelease(t *testing.T, installed, tag, program string, corrupt bool) string {
	t.Helper()
	asset := selfupdate.AssetName(runtime.GOOS, runtime.GOARCH)

	var archive bytes.Buffer
	if strings.HasSuffix(asset, ".zip") {
		zw := zip.NewWriter(&archive)
		w, _ := zw.Create("plugyard.exe")
		w.Write([]byte(program))
		zw.Close()
	} else {
		gz := gzip.NewWriter(&archive)
		tw := tar.NewWriter(gz)
		tw.WriteHeader(&tar.Header{Name: "plugyard", Mode: 0o755, Size: int64(len(program)), Typeflag: tar.TypeReg})
		tw.Write([]byte(program))
		tw.Close()
		gz.Close()
	}
	sum := sha256.Sum256(archive.Bytes())
	sums := hex.EncodeToString(sum[:]) + "  " + asset + "\n"
	if corrupt {
		sums = strings.Repeat("0", 64) + "  " + asset + "\n"
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/releases/latest", func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.UserAgent(), "plugyard/") {
			t.Errorf("User-Agent = %q, want plugyard and its version", r.UserAgent())
		}
		http.Redirect(w, r, "/releases/tag/"+tag, http.StatusFound)
	})
	mux.HandleFunc("/releases/download/"+tag+"/"+asset, func(w http.ResponseWriter, _ *http.Request) { w.Write(archive.Bytes()) })
	mux.HandleFunc("/releases/download/"+tag+"/checksums.txt", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte(sums)) })
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	t.Setenv(updateURLEnv, srv.URL+"/")

	exe := filepath.Join(t.TempDir(), "plugyard.exe")
	if err := os.WriteFile(exe, []byte("the old program"), 0o755); err != nil {
		t.Fatal(err)
	}
	oldVersion, oldPath := currentVersion, executablePath
	currentVersion = func() string { return installed }
	executablePath = func() (string, error) { return exe, nil }
	t.Cleanup(func() { currentVersion, executablePath = oldVersion, oldPath })
	return exe
}

func TestUpdate(t *testing.T) {
	useTempDataDir(t)
	exe := fakeRelease(t, "v0.1.0", "v0.2.0", "the new program", false)

	// --check only looks.
	out := mustRun(t, "update", "--check")
	if out != "plugyard v0.1.0 is installed; v0.2.0 is available.\nRun \"plugyard update\" to install it.\n" {
		t.Errorf("update --check printed %q", out)
	}
	if data, _ := os.ReadFile(exe); string(data) != "the old program" {
		t.Fatal("update --check changed the program")
	}

	out = mustRun(t, "update")
	for _, want := range []string{
		"plugyard v0.1.0 is installed; v0.2.0 is available.\n",
		"Downloading " + selfupdate.AssetName(runtime.GOOS, runtime.GOARCH) + "\n",
		"Updated plugyard to v0.2.0.\n",
		"/releases/tag/v0.2.0\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("update output is missing %q:\n%s", want, out)
		}
	}
	if data, _ := os.ReadFile(exe); string(data) != "the new program" {
		t.Errorf("the program holds %q after the update", data)
	}
	// Nothing is left beside it once plugyard has run again.
	run("version")
	if left, _ := filepath.Glob(filepath.Join(filepath.Dir(exe), "*")); len(left) != 1 {
		t.Errorf("the update left %v", left)
	}
}

func TestUpdateWhenUpToDate(t *testing.T) {
	useTempDataDir(t)
	exe := fakeRelease(t, "v0.2.0", "v0.2.0", "the same program", false)
	for _, args := range [][]string{{"update"}, {"update", "--check"}} {
		if out := mustRun(t, args...); out != "plugyard v0.2.0 is the latest version.\n" {
			t.Errorf("%v printed %q", args, out)
		}
	}
	if data, _ := os.ReadFile(exe); string(data) != "the old program" {
		t.Error("an update with nothing newer changed the program")
	}
}

func TestUpdateRefusesWhatItCannotTrust(t *testing.T) {
	useTempDataDir(t)
	exe := fakeRelease(t, "v0.1.0", "v0.2.0", "tampered with", true)
	code, _, stderr := run("update")
	if code != ExitFailure || !strings.Contains(stderr, "does not match its checksum; nothing was changed") {
		t.Errorf("update with a wrong checksum: exit %d, stderr %q", code, stderr)
	}
	if data, _ := os.ReadFile(exe); string(data) != "the old program" {
		t.Error("the program was replaced by a download that failed its checksum")
	}
}

func TestUpdateRestartsWhatIsRunning(t *testing.T) {
	useTempDataDir(t)
	csms := csmstest.Start(t)
	upDetached(t, csms)
	// One charger is stopped and one is charging, to see that both come
	// back as they were.
	mustRun(t, "stop", "LOBBY-1")
	startTransaction(t, csms, "FAST-01")
	base := apiUp(t)
	exe := fakeRelease(t, "v0.1.0", "v0.2.0", "the new program", false)

	unchanged := func(when string) {
		t.Helper()
		if data, _ := os.ReadFile(exe); string(data) != "the old program" {
			t.Fatalf("%s: the program was replaced", when)
		}
		if row := psRow(t, "FAST-02"); !strings.Contains(row, "online") {
			t.Fatalf("%s: the site was stopped: %q", when, row)
		}
	}

	// With nobody to ask, nothing happens, and what is running is named.
	oldIsTerminal := stdinIsTerminal
	stdinIsTerminal = func() bool { return false }
	code, stdout, stderr := run("update")
	stdinIsTerminal = oldIsTerminal
	if code != ExitFailure || !strings.Contains(stdout, "Running now: site depot (4 chargers) and the API at "+base+".\n") ||
		!strings.Contains(stdout, "they are stopped and started again on v0.2.0, in the background. 1 transaction ends with StopTransaction.\n") ||
		!strings.Contains(stderr, "run again with --yes to restart them on the new version, or stop them first. Nothing was changed") {
		t.Errorf("update with a site running and nobody to ask: exit %d, stdout %q, stderr %q", code, stdout, stderr)
	}
	unchanged("with nobody to ask")
	// Anything but yes leaves everything.
	answer(t, "no\n")
	if code, stdout, stderr := run("update"); code != ExitFailure || !strings.Contains(stdout, "Update and restart them? [y/N] ") || !strings.Contains(stderr, "nothing was changed") {
		t.Errorf("update answered with no: exit %d, stdout %q, stderr %q", code, stdout, stderr)
	}
	unchanged("answered with no")
	// Looking needs no answer.
	if out := mustRun(t, "update", "--check"); !strings.Contains(out, "v0.2.0 is available") || strings.Contains(out, "Running now") {
		t.Errorf("update --check with a site running printed %q", out)
	}

	out := mustRun(t, "update", "--yes")
	if strings.Contains(out, "[y/N]") {
		t.Errorf("update --yes asked:\n%s", out)
	}
	// In this order: download, stop, replace, start again.
	last := -1
	for _, want := range []string{
		"Downloading ", "Site depot is stopped\n", "The API at " + base + " is stopped\n", "Updated plugyard to v0.2.0.\n",
		"Site depot is running again, in the background, with 4 chargers\n", "The API is running again, in the background, at " + base + "\n",
	} {
		at := strings.Index(out, want)
		if at < 0 || at < last {
			t.Fatalf("update --yes is missing %q, or has it out of order:\n%s", want, out)
		}
		last = at
	}
	if data, _ := os.ReadFile(exe); string(data) != "the new program" {
		t.Errorf("the program holds %q after the update", data)
	}

	// The transaction ended in the orderly way, and everything is back as
	// it was: the same chargers on, the stopped one still stopped.
	csms.WaitForCalls(t, "FAST-01", v16.ActionStopTransaction, 1)
	mustRun(t, "wait", "FAST-*", "online", "--timeout", "10s")
	if row := psRow(t, "LOBBY-1"); !strings.Contains(row, "stopped") {
		t.Errorf("a charger that was stopped came back on: %q", row)
	}
	if status, body := apiCall(t, base, "GET", "/v1/chargers/FAST-02", "", ""); status != 200 || !strings.Contains(body, `"chargePointId":"FAST-02"`) {
		t.Errorf("the API after the update = %d %s", status, body)
	}

	if code, _, stderr := run("update", "now"); code != ExitUsage || stderr == "" {
		t.Errorf("update with an argument: exit %d", code)
	}
}

// A download that fails stops nothing.
func TestUpdateStopsNothingWhenTheDownloadFails(t *testing.T) {
	useTempDataDir(t)
	csms := csmstest.Start(t)
	upDetached(t, csms)
	exe := fakeRelease(t, "v0.1.0", "v0.2.0", "tampered with", true)

	code, stdout, stderr := run("update", "--yes")
	if code != ExitFailure || !strings.Contains(stderr, "does not match its checksum; nothing was changed") || strings.Contains(stdout, "is stopped") {
		t.Errorf("update with a wrong checksum: exit %d, stdout %q, stderr %q", code, stdout, stderr)
	}
	if data, _ := os.ReadFile(exe); string(data) != "the old program" {
		t.Error("the program was replaced by a download that failed its checksum")
	}
	if row := psRow(t, "FAST-01"); !strings.Contains(row, "online") {
		t.Errorf("the site was stopped for a download that failed: %q", row)
	}
}
