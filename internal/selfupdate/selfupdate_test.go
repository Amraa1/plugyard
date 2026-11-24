package selfupdate

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func tarGz(t *testing.T, name string, content []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, f := range []struct {
		name string
		data []byte
	}{{"README.md", []byte("read me")}, {name, content}} {
		if err := tw.WriteHeader(&tar.Header{Name: f.name, Mode: 0o755, Size: int64(len(f.data)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		tw.Write(f.data)
	}
	tw.Close()
	gz.Close()
	return buf.Bytes()
}

func zipped(t *testing.T, name string, content []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, f := range []struct {
		name string
		data []byte
	}{{"LICENSE", []byte("license")}, {name, content}} {
		w, err := zw.Create(f.name)
		if err != nil {
			t.Fatal(err)
		}
		w.Write(f.data)
	}
	zw.Close()
	return buf.Bytes()
}

func sum(data []byte) string {
	s := sha256.Sum256(data)
	return hex.EncodeToString(s[:])
}

// releases serves a project page with one release, as GitHub does.
func releases(t *testing.T, tag string, files map[string][]byte) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/releases/latest", func(w http.ResponseWriter, r *http.Request) {
		if tag == "" {
			http.Redirect(w, r, "/releases", http.StatusFound)
			return
		}
		http.Redirect(w, r, "/releases/tag/"+tag, http.StatusFound)
	})
	mux.HandleFunc("/releases/download/", func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimPrefix(r.URL.Path, "/releases/download/"+tag+"/")
		data, ok := files[name]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Write(data)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestLatestAndDownload(t *testing.T) {
	linux := tarGz(t, "plugyard", []byte("the linux program"))
	windows := zipped(t, "plugyard.exe", []byte("the windows program"))
	sums := sum(linux) + "  plugyard_linux_arm64.tar.gz\n" + sum(windows) + "  plugyard_windows_amd64.zip\n"
	srv := releases(t, "v0.2.0", map[string][]byte{
		"plugyard_linux_arm64.tar.gz": linux,
		"plugyard_windows_amd64.zip":  windows,
		ChecksumsName:                 []byte(sums),
	})
	ctx := context.Background()

	for _, tc := range []struct{ os, arch, want string }{
		{"linux", "arm64", "the linux program"},
		{"windows", "amd64", "the windows program"},
	} {
		u := &Updater{Base: srv.URL, OS: tc.os, Arch: tc.arch, UserAgent: "plugyard/test"}
		tag, err := u.Latest(ctx)
		if err != nil || tag != "v0.2.0" {
			t.Fatalf("Latest = %q, %v", tag, err)
		}
		got, err := u.Download(ctx, tag)
		if err != nil || string(got) != tc.want {
			t.Errorf("Download for %s/%s = %q, %v", tc.os, tc.arch, got, err)
		}
	}

	// A system the release has no build for.
	u := &Updater{Base: srv.URL, OS: "plan9", Arch: "amd64"}
	if _, err := u.Download(ctx, "v0.2.0"); err == nil || !strings.Contains(err.Error(), "release v0.2.0 has no build for plan9/amd64") {
		t.Errorf("Download for plan9 = %v", err)
	}
}

func TestDownloadRefusesWhatDoesNotMatchItsChecksum(t *testing.T) {
	archive := tarGz(t, "plugyard", []byte("tampered with"))
	srv := releases(t, "v0.2.0", map[string][]byte{
		"plugyard_linux_amd64.tar.gz": archive,
		ChecksumsName:                 []byte(sum([]byte("something else")) + "  plugyard_linux_amd64.tar.gz\n"),
	})
	u := &Updater{Base: srv.URL, OS: "linux", Arch: "amd64"}
	if _, err := u.Download(context.Background(), "v0.2.0"); err == nil || !strings.Contains(err.Error(), "does not match its checksum") {
		t.Errorf("Download = %v, want a checksum error", err)
	}
}

func TestLatestWithNoRelease(t *testing.T) {
	srv := releases(t, "", nil)
	u := &Updater{Base: srv.URL, OS: "linux", Arch: "amd64"}
	if _, err := u.Latest(context.Background()); !errors.Is(err, ErrNoRelease) {
		t.Errorf("Latest = %v, want ErrNoRelease", err)
	}
	unreachable := &Updater{Base: "http://127.0.0.1:1", OS: "linux", Arch: "amd64"}
	if _, err := unreachable.Latest(context.Background()); err == nil || !strings.Contains(err.Error(), "cannot reach") {
		t.Errorf("Latest of an unreachable address = %v", err)
	}
}

func TestReplace(t *testing.T) {
	exe := filepath.Join(t.TempDir(), "plugyard.exe")
	if err := os.WriteFile(exe, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := Replace(exe, []byte("new")); err != nil {
		t.Fatalf("Replace: %v", err)
	}
	if data, _ := os.ReadFile(exe); string(data) != "new" {
		t.Errorf("the program holds %q, want the new one", data)
	}
	CleanOld(exe)
	if left, _ := filepath.Glob(filepath.Join(filepath.Dir(exe), "*")); len(left) != 1 {
		t.Errorf("Replace left %v", left)
	}

	// A program that is not there cannot be replaced, and nothing is left.
	missing := filepath.Join(t.TempDir(), "plugyard")
	if err := Replace(missing, []byte("new")); err == nil {
		t.Error("Replace of a missing program succeeded")
	}
	if left, _ := filepath.Glob(filepath.Join(filepath.Dir(missing), "*")); len(left) != 0 {
		t.Errorf("a failed Replace left %v", left)
	}
}

func TestNewer(t *testing.T) {
	for _, tc := range []struct {
		current, latest string
		want            bool
	}{
		{"v0.1.0", "v0.2.0", true},
		{"v0.1.0", "v0.1.1", true},
		{"v0.9.0", "v0.10.0", true},
		{"v0.2.0", "v0.2.0", false},
		{"v0.2.0", "v0.1.9", false},
		{"v1.0.0", "v0.9.9", false},
		{"dev", "v0.1.0", true},
		{"v0.1.0-beta.1", "v0.1.0", true},
		{"v0.1.0", "v0.1.0-beta.1", false},
		{"v0.1.0-beta.1", "v0.1.0-beta.2", true},
		// A build of "go install" at a commit after a release.
		{"v0.1.1-0.20260101000000-abcdef123456", "v0.1.0", false},
		{"v0.1.1-0.20260101000000-abcdef123456", "v0.1.1", true},
		{"v0.1.0", "nonsense", false},
	} {
		if got := Newer(tc.current, tc.latest); got != tc.want {
			t.Errorf("Newer(%q, %q) = %v, want %v", tc.current, tc.latest, got, tc.want)
		}
	}
}

func TestAssetName(t *testing.T) {
	for system, want := range map[[2]string]string{
		{"windows", "amd64"}: "plugyard_windows_amd64.zip",
		{"linux", "arm64"}:   "plugyard_linux_arm64.tar.gz",
		{"darwin", "arm64"}:  "plugyard_darwin_arm64.tar.gz",
	} {
		if got := AssetName(system[0], system[1]); got != want {
			t.Errorf("AssetName(%v) = %q, want %q", system, got, want)
		}
	}
}
