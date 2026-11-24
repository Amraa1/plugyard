package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Amraa1/plugyard/internal/csmstest"
	"github.com/Amraa1/plugyard/internal/ocpp/v16"
)

// scaleEnv sets how many chargers TestScale runs. Every test run uses 200;
// the release check is PLUGYARD_SCALE=2000.
const scaleEnv = "PLUGYARD_SCALE"

// scaleMemoryLimit is the most memory the test process may have taken from
// the system by the end: the site, and the fake CSMS on the other end of
// every connection.
const scaleMemoryLimit = 1 << 30

// The 2,000 are for a normal build. Under the race detector the test is right
// up to about 1,400 chargers: past roughly 8,000 goroutines the detector
// itself becomes many times slower and takes gigabytes.
//
// TestScale runs one site with many chargers through a whole day's work:
// every charger connects, runs a transaction with meter values, and shuts
// down in an orderly way.
func TestScale(t *testing.T) {
	count := 200
	if text := os.Getenv(scaleEnv); text != "" {
		n, err := strconv.Atoi(text)
		if err != nil || n < 1 {
			t.Fatalf("%s=%q is not a number of chargers", scaleEnv, text)
		}
		count = n
	}
	if testing.Short() {
		t.Skip("skipped with -short")
	}

	useTempDataDir(t)
	csms := csmstest.Start(t)
	dir := t.TempDir()
	chargerfile := testChargerfile + "config: { MeterValueSampleInterval: 1 }\n"
	if err := os.WriteFile(filepath.Join(dir, "Chargerfile"), []byte(chargerfile), 0o600); err != nil {
		t.Fatal(err)
	}
	site := filepath.Join(dir, "site.yaml")
	body := fmt.Sprintf("name: big\ncsms: { url: %s }\nchargers:\n  all: { model: ./Chargerfile, count: %d, id: \"S-{n:04}\" }\n", csms.URL(), count)
	if err := os.WriteFile(site, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	ids := make([]string, count)
	for i := range ids {
		ids[i] = fmt.Sprintf("S-%04d", i+1)
	}
	// wait waits until every charger has made n calls of an action.
	wait := func(action string, n int) {
		t.Helper()
		for _, id := range ids {
			csms.WaitForCalls(t, id, action, n)
		}
	}
	all := func(args ...string) {
		t.Helper()
		code, stdout, stderr := run(args...)
		want := fmt.Sprintf("%d of %d succeeded\n", count, count)
		if code != ExitOK || !strings.HasSuffix(stdout, want) {
			tail := stdout
			if len(tail) > 2000 {
				tail = tail[len(tail)-2000:]
			}
			t.Fatalf("%v: exit %d, stderr %q, want %q at the end of:\n%s", args, code, stderr, want, tail)
		}
	}

	started := time.Now()
	_, exited := startSiteUp(t, csms, site, ids, "--speed", "60")
	connected := time.Since(started)

	phase := time.Now()
	all("exec", "*", "plug")
	all("exec", "*", "tap", "TAG-1")
	wait(v16.ActionStartTransaction, 1)
	wait(v16.ActionMeterValues, 1)
	all("exec", "*", "unplug")
	wait(v16.ActionStopTransaction, 1)
	transacted := time.Since(phase)

	// Every charger reported energy, and every listing still answers.
	for _, id := range []string{ids[0], ids[count/2], ids[count-1]} {
		stop := decodeCall[v16.StopTransactionRequest](t, csms.WaitForCalls(t, id, v16.ActionStopTransaction, 1)[0])
		if stop.Reason != v16.ReasonEVDisconnected || stop.MeterStop <= 0 {
			t.Errorf("%s: StopTransaction = %+v, want EVDisconnected with energy on the meter", id, stop)
		}
	}
	if rows := lines(mustRun(t, "ps")); len(rows) != count+1 {
		t.Errorf("ps lists %d chargers, want %d", len(rows)-1, count)
	}
	if rows := lines(mustRun(t, "stats", "--format", "json")); len(rows) != count {
		t.Errorf("stats lists %d chargers, want %d", len(rows), count)
	}

	var mem runtime.MemStats
	runtime.ReadMemStats(&mem)
	goroutines := runtime.NumGoroutine()

	phase = time.Now()
	mustRun(t, "site", "stop", "-s", "big")
	if code := exited(); code != ExitOK {
		t.Errorf("site up exited with %d after site stop", code)
	}
	stopped := time.Since(phase)
	for _, id := range ids {
		if ends := csms.WaitDisconnects(t, id, 1); !ends[0].Clean {
			t.Fatalf("%s did not close its connection in an orderly way", id)
		}
	}

	t.Logf("%d chargers: all online after %s, a transaction each in %s, shut down in %s; %d MB taken from the system and %d goroutines at the peak (site and fake CSMS together)",
		count, connected.Round(10*time.Millisecond), transacted.Round(10*time.Millisecond), stopped.Round(10*time.Millisecond), mem.Sys>>20, goroutines)
	if mem.Sys > scaleMemoryLimit {
		t.Errorf("the test process took %d MB from the system, want at most %d MB", mem.Sys>>20, scaleMemoryLimit>>20)
	}
}
