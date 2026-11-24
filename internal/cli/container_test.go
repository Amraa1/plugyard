package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Amraa1/plugyard/internal/csmstest"
)

func TestSiteUpWithTheAPI(t *testing.T) {
	useTempDataDir(t)
	csms := csmstest.Start(t)
	path := writeSite(t, csms.URL(), "name: yard\nchargers:\n  fast: { model: ./Chargerfile, id: FAST-1 }\n")

	// In the background the API is started by its own command.
	code, _, stderr := run("site", "up", "-f", path, "-d", "--api", "127.0.0.1:0")
	if code != ExitUsage || !strings.Contains(stderr, `with -d, start it with "plugyard api up -d"`) {
		t.Errorf("site up -d --api: exit %d, stderr %q", code, stderr)
	}
	// An address others can reach needs a token, here as everywhere.
	code, _, stderr = run("site", "up", "-f", path, "--api", "0.0.0.0:0")
	if code != ExitFailure || !strings.Contains(stderr, "no token exists") {
		t.Errorf("site up --api beyond this machine without a token: exit %d, stderr %q", code, stderr)
	}
	if _, stdout, _ := run("ps", "-a"); strings.Contains(stdout, "online") {
		t.Fatalf("the site runs though the API was refused:\n%s", stdout)
	}

	stdout, exited := startSiteUp(t, csms, path, []string{"FAST-1"}, "--api", "127.0.0.1:0")
	out := mustRun(t, "api", "status")
	base := ""
	for _, field := range strings.Fields(out) {
		if strings.HasPrefix(field, "http://127.0.0.1:") {
			base = field
		}
	}
	if base == "" {
		t.Fatalf("the API does not run beside the site:\n%s", out)
	}
	if got := stdout.String(); !strings.Contains(got, "The API is listening at "+base+"\n") || !strings.Contains(got, "Starting site yard with 1 charger\n") {
		t.Errorf("site up --api printed:\n%s", got)
	}

	// It is the API of the machine, and its calls show with the site's log.
	if status, body := apiCall(t, base, "POST", "/v1/chargers/FAST-1/plug", "", "{}"); status != 200 || !strings.Contains(body, `"ok":true`) {
		t.Errorf("plug through the API = %d %s", status, body)
	}
	eventually(t, "the call shows in the output of site up", func() bool {
		return strings.Contains(stdout.String(), " local 127.0.0.1 200 plug FAST-1: EV plugged in")
	})
	if code, _, stderr := run("api", "up", "-d"); code != ExitFailure || !strings.Contains(stderr, "the API is already running, at "+base) {
		t.Errorf("a second API beside the one of the site: exit %d, stderr %q", code, stderr)
	}

	// The API ends with the site.
	mustRun(t, "site", "stop", "-s", "yard")
	if code := exited(); code != ExitOK {
		t.Errorf("site up --api exited with %d", code)
	}
	if out := mustRun(t, "api", "status"); !strings.HasPrefix(out, "The API is not running.") {
		t.Errorf("the API outlived the site:\n%s", out)
	}
}

// "plugyard api down" stops the API of a site and leaves the site.
func TestAPIDownLeavesTheSite(t *testing.T) {
	useTempDataDir(t)
	csms := csmstest.Start(t)
	path := writeSite(t, csms.URL(), "name: yard\nchargers:\n  fast: { model: ./Chargerfile, id: FAST-1 }\n")
	startSiteUp(t, csms, path, []string{"FAST-1"}, "--api", "127.0.0.1:0")

	mustRun(t, "api", "down")
	if out := mustRun(t, "api", "status"); !strings.HasPrefix(out, "The API is not running.") {
		t.Errorf("api status after down:\n%s", out)
	}
	if row := psRow(t, "FAST-1"); !strings.Contains(row, "online") {
		t.Errorf("api down stopped the site: %q", row)
	}
}

// In the container image the program is replaced by pulling a newer image.
func TestUpdateAndUninstallInAContainer(t *testing.T) {
	useTempDataDir(t)
	t.Setenv(containerEnv, "1")
	exe := fakeRelease(t, "v0.1.0", "v0.2.0", "the new program", false)

	if out := mustRun(t, "update", "--check"); !strings.Contains(out, "v0.2.0 is available") || !strings.Contains(out, "Pull the new one: docker pull amraa1/plugyard:0.2.0\n") {
		t.Errorf("update --check in a container printed %q", out)
	}
	code, _, stderr := run("update", "--yes")
	if code != ExitFailure || !strings.Contains(stderr, "Pull the new image instead: docker pull amraa1/plugyard:0.2.0") {
		t.Errorf("update in a container: exit %d, stderr %q", code, stderr)
	}
	if data, _ := os.ReadFile(exe); string(data) != "the old program" {
		t.Error("update in a container replaced the program")
	}
	code, _, stderr = run("uninstall", "--yes")
	if code != ExitFailure || !strings.Contains(stderr, "Remove the container") {
		t.Errorf("uninstall in a container: exit %d, stderr %q", code, stderr)
	}
	if !exists(exe) {
		t.Error("uninstall in a container removed the program")
	}
}

// The files the image and the compose example are made of are valid, and
// the copies among them have not drifted from the starter files.
func TestContainerFiles(t *testing.T) {
	root := filepath.Join("..", "..")
	starterFile := func(name string) string {
		data, err := starter.ReadFile("starter/" + name)
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
	}
	same := func(path, name string) {
		t.Helper()
		data, err := os.ReadFile(filepath.Join(root, path))
		if err != nil {
			t.Fatal(err)
		}
		if strings.ReplaceAll(string(data), "\r\n", "\n") != starterFile(name) {
			t.Errorf("%s is no longer the starter %s that \"plugyard init\" writes", path, name)
		}
	}
	same("examples/docker/site.yaml", "site.yaml")
	same("examples/docker/Chargerfile", "Chargerfile")

	// The site of the image, with the Chargerfile the Dockerfile puts
	// beside it.
	dir := t.TempDir()
	site, err := os.ReadFile(filepath.Join(root, "docker", "site", "site.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(dir, "site.yaml"), site, 0o600)
	os.WriteFile(filepath.Join(dir, "Chargerfile"), []byte(starterFile("Chargerfile")), 0o600)
	if out := mustRun(t, "site", "validate", "-f", filepath.Join(dir, "site.yaml")); !strings.Contains(out, "Site plugyard is valid: 3 chargers, CSMS ws://host.docker.internal:9000/ocpp") {
		t.Errorf("the site of the image:\n%s", out)
	}
	t.Setenv("CSMS_URL", "wss://csms.example.com/ocpp")
	t.Setenv("CSMS_PASSWORD", "s3cret")
	if out := mustRun(t, "site", "validate", "-f", filepath.Join(dir, "site.yaml")); !strings.Contains(out, "CSMS wss://csms.example.com/ocpp") {
		t.Errorf("the site of the image with a CSMS from the environment:\n%s", out)
	}

	dockerfile, err := os.ReadFile(filepath.Join(root, "Dockerfile"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"COPY internal/cli/starter/Chargerfile /site/Chargerfile", containerEnv + "=1", "PLUGYARD_DATA_DIR=/data",
		":nonroot", `ENTRYPOINT ["plugyard"]`, `CMD ["site", "up"]`,
	} {
		if !strings.Contains(string(dockerfile), want) {
			t.Errorf("the Dockerfile is missing %q", want)
		}
	}
	compose, _ := os.ReadFile(filepath.Join(root, "examples", "docker", "compose.yaml"))
	if !strings.Contains(string(compose), "image: "+containerImage+":") {
		t.Errorf("the compose example does not use the image %s", containerImage)
	}
}
