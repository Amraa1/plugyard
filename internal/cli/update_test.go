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

func TestUpdateWaitsForSitesToStop(t *testing.T) {
	useTempDataDir(t)
	exe := fakeRelease(t, "v0.1.0", "v0.2.0", "the new program", false)
	csms := csmstest.Start(t)
	startRun(t, csms, "CP-1")

	code, _, stderr := run("update")
	if code != ExitFailure || !strings.Contains(stderr, "site CP-1 is running; stop every site first") || !strings.Contains(stderr, "Nothing was changed") {
		t.Errorf("update with a site running: exit %d, stderr %q", code, stderr)
	}
	if data, _ := os.ReadFile(exe); string(data) != "the old program" {
		t.Error("the program was replaced while a site was running")
	}
	// Looking is still allowed.
	if out := mustRun(t, "update", "--check"); !strings.Contains(out, "v0.2.0 is available") {
		t.Errorf("update --check with a site running printed %q", out)
	}

	if code, _, stderr := run("update", "now"); code != ExitUsage || stderr == "" {
		t.Errorf("update with an argument: exit %d", code)
	}
}
