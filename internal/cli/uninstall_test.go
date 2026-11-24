package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Amraa1/plugyard/internal/api"
	"github.com/Amraa1/plugyard/internal/csmstest"
)

// fakeProgram stands a file in for the installed program, in a folder of
// the given name, and returns its path.
func fakeProgram(t *testing.T, folder string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), folder)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	exe := filepath.Join(dir, "plugyard.exe")
	if err := os.WriteFile(exe, []byte("the program"), 0o755); err != nil {
		t.Fatal(err)
	}
	old := executablePath
	executablePath = func() (string, error) { return exe, nil }
	t.Cleanup(func() { executablePath = old })
	return exe
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// savedSite leaves the saved state and log of a site of three chargers in
// the data directory, with no process running.
func savedSite(t *testing.T) {
	t.Helper()
	csms := csmstest.Start(t)
	path := writeSite(t, csms.URL(), twoEntries)
	_, exited := startSiteUp(t, csms, path, []string{"FAST-1", "FAST-2", "LOBBY-1"})
	run("site", "stop", "-f", path)
	exited()
}

func TestUninstall(t *testing.T) {
	dataDir := useTempDataDir(t)
	savedSite(t)
	exe := fakeProgram(t, "plugyard")
	// A file that is not plugyard's, in the data directory.
	notes := filepath.Join(dataDir, "notes.txt")
	if err := os.WriteFile(notes, []byte("mine"), 0o600); err != nil {
		t.Fatal(err)
	}
	if len(dataFiles(dataDir)) == 0 {
		t.Fatal("the site saved nothing")
	}

	untouched := func(when string) {
		t.Helper()
		if !exists(exe) || len(dataFiles(dataDir)) == 0 {
			t.Fatalf("%s: something was removed", when)
		}
	}

	// With nobody to ask, nothing happens.
	oldIsTerminal := stdinIsTerminal
	stdinIsTerminal = func() bool { return false }
	code, _, stderr := run("uninstall")
	stdinIsTerminal = oldIsTerminal
	if code != ExitFailure || !strings.Contains(stderr, "run again with --yes to remove plugyard. Nothing was removed") {
		t.Errorf("uninstall without a terminal: exit %d, stderr %q", code, stderr)
	}
	untouched("without a terminal")

	// Anything but yes leaves everything.
	answer(t, "\n")
	code, stdout, stderr := run("uninstall")
	if code != ExitFailure || !strings.Contains(stdout, "Remove plugyard? [y/N] ") || !strings.Contains(stderr, "nothing was removed") {
		t.Errorf("uninstall answered with no: exit %d, stdout %q, stderr %q", code, stdout, stderr)
	}
	untouched("answered with no")

	answer(t, "y\n")
	code, stdout, stderr = run("uninstall")
	if code != ExitOK {
		t.Fatalf("uninstall: exit %d, stdout %q, stderr %q", code, stdout, stderr)
	}
	for _, want := range []string{
		"This removes plugyard from this machine:\n",
		"  the program              " + exe + "\n",
		"  its folder               " + filepath.Dir(exe) + "\n",
		"  saved chargers and logs  " + dataDir + " (1 site, 3 chargers)\n",
		"plugyard is removed.\n",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("uninstall output is missing %q:\n%s", want, stdout)
		}
	}
	if strings.Contains(stdout, "PATH") {
		t.Errorf("uninstall speaks of a PATH it did not change:\n%s", stdout)
	}
	if exists(exe) {
		t.Error("the program is still there")
	}
	// On Windows the folder goes a few seconds after the command ends.
	eventually(t, "the folder of the program is gone", func() bool { return !exists(filepath.Dir(exe)) })
	if left := dataFiles(dataDir); len(left) != 0 {
		t.Errorf("uninstall left %v", left)
	}
	if !exists(notes) {
		t.Error("uninstall removed a file in the data directory that is not plugyard's")
	}
}

func TestUninstallKeepsDataAndSharedFolders(t *testing.T) {
	dataDir := useTempDataDir(t)
	savedSite(t)

	// A folder that is not plugyard's own stays, even when empty.
	exe := fakeProgram(t, "bin")
	stdout := mustRun(t, "uninstall", "--keep-data", "--yes")
	if strings.Contains(stdout, "its folder") || strings.Contains(stdout, "  saved chargers and logs") ||
		!strings.Contains(stdout, "Your saved chargers and logs are kept: "+dataDir+" (1 site, 3 chargers)\n") ||
		strings.Contains(stdout, "[y/N]") {
		t.Errorf("uninstall --keep-data --yes printed:\n%s", stdout)
	}
	if exists(exe) || !exists(filepath.Dir(exe)) {
		t.Errorf("program there: %v, its folder there: %v; want false and true", exists(exe), exists(filepath.Dir(exe)))
	}
	if len(dataFiles(dataDir)) == 0 {
		t.Error("--keep-data removed the saved chargers")
	}

	// So does plugyard's own folder when something else is in it.
	exe = fakeProgram(t, "plugyard")
	other := filepath.Join(filepath.Dir(exe), "other.txt")
	if err := os.WriteFile(other, []byte("mine"), 0o600); err != nil {
		t.Fatal(err)
	}
	stdout = mustRun(t, "uninstall", "-y")
	if strings.Contains(stdout, "its folder") {
		t.Errorf("uninstall lists a folder that holds more than the program:\n%s", stdout)
	}
	if exists(exe) || !exists(other) {
		t.Errorf("program there: %v, the other file there: %v; want false and true", exists(exe), exists(other))
	}
	if len(dataFiles(dataDir)) != 0 {
		t.Error("the saved chargers are still there")
	}
}

func TestUninstallStopsWhatIsRunning(t *testing.T) {
	dataDir := useTempDataDir(t)
	csms := csmstest.Start(t)
	upDetached(t, csms)
	base := apiUp(t)
	exe := fakeProgram(t, "plugyard")

	// The question names what is running, and no leaves it running.
	answer(t, "n\n")
	code, stdout, stderr := run("uninstall")
	if code != ExitFailure || !strings.Contains(stdout, "Running now, and stopped first: site depot (5 chargers) and the API at "+base+".\n") || !strings.Contains(stderr, "nothing was removed") {
		t.Errorf("uninstall answered with no: exit %d, stdout %q, stderr %q", code, stdout, stderr)
	}
	if !exists(exe) || !strings.Contains(psRow(t, "FAST-01"), "online") {
		t.Fatal("something was stopped or removed after the answer no")
	}

	out := mustRun(t, "uninstall", "--yes")
	last := -1
	for _, want := range []string{"Running now, and stopped first: ", "Site depot is stopped\n", "The API at " + base + " is stopped\n", "plugyard is removed.\n"} {
		at := strings.Index(out, want)
		if at < 0 || at < last {
			t.Fatalf("uninstall --yes is missing %q, or has it out of order:\n%s", want, out)
		}
		last = at
	}
	if exists(exe) {
		t.Error("the program is still there")
	}
	if left := dataFiles(dataDir); len(left) != 0 {
		t.Errorf("uninstall left %v", left)
	}
	for _, id := range fiveIDs {
		csms.WaitDisconnects(t, id, 1)
	}
	if _, err := api.Probe(context.Background(), dataDir); err == nil {
		t.Error("the API still answers after the uninstall")
	}

	if code, _, stderr := run("uninstall", "now"); code != ExitUsage || stderr == "" {
		t.Errorf("uninstall with an argument: exit %d", code)
	}
}

func TestWithoutPathEntry(t *testing.T) {
	dir := `C:\Users\me\AppData\Local\Programs\plugyard`
	for _, tc := range []struct {
		path, want string
		found      bool
	}{
		{`C:\a;` + dir + `;C:\b`, `C:\a;C:\b`, true},
		{dir, ``, true},
		{`C:\a;` + strings.ToUpper(dir) + `\`, `C:\a`, true},
		{`%USERPROFILE%\bin;` + dir + `;`, `%USERPROFILE%\bin;`, true},
		{`C:\a;` + dir + `-old`, `C:\a;` + dir + `-old`, false},
		{``, ``, false},
	} {
		got, found := withoutPathEntry(tc.path, dir, ";")
		if got != tc.want || found != tc.found {
			t.Errorf("withoutPathEntry(%q) = %q, %v; want %q, %v", tc.path, got, found, tc.want, tc.found)
		}
	}
}
