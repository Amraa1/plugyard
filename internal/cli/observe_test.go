package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/Amraa1/plugyard/internal/csmstest"
	"github.com/Amraa1/plugyard/internal/ocpp/v16"
)

// lines splits output into its lines.
func lines(output string) []string {
	if strings.TrimSpace(output) == "" {
		return nil
	}
	return strings.Split(strings.TrimSuffix(output, "\n"), "\n")
}

// mustRun runs a command that is expected to succeed and returns what it
// printed.
func mustRun(t *testing.T, args ...string) string {
	t.Helper()
	code, stdout, stderr := run(args...)
	if code != ExitOK {
		t.Fatalf("%v: exit %d, stdout %q, stderr %q", args, code, stdout, stderr)
	}
	return stdout
}

func TestLogsOfACharger(t *testing.T) {
	useTempDataDir(t)
	csms := csmstest.Start(t)
	startRun(t, csms, "CP-1")
	startRun(t, csms, "CP-2")
	mustRun(t, "exec", "CP-1", "plug")

	// Events and OCPP messages in both directions, of this charger only.
	all := mustRun(t, "logs", "CP-1")
	for _, want := range []string{" CP-1 | accepted by the CSMS", ` CP-1 > [2,"`, ` CP-1 < [3,"`, "BootNotification", " CP-1 | connector 1: EV plugged in"} {
		if !strings.Contains(all, want) {
			t.Errorf("logs CP-1 is missing %q:\n%s", want, all)
		}
	}
	if strings.Contains(all, "CP-2") {
		t.Errorf("logs CP-1 shows lines of CP-2:\n%s", all)
	}
	// The name works as well as the ID, and a pattern takes in both.
	if byName := mustRun(t, "logs", "cp-1"); byName != all {
		t.Errorf("logs by name differs from logs by ID")
	}
	if both := mustRun(t, "logs", "CP-*"); !strings.Contains(both, " CP-1 | ") || !strings.Contains(both, " CP-2 | ") {
		t.Errorf("logs 'CP-*' does not show both chargers:\n%s", both)
	}
	// The log of the whole site is the same thing for a charger that is its
	// own site.
	if site := mustRun(t, "site", "logs", "-s", "CP-1"); site != all {
		t.Errorf("site logs -s CP-1 differs from logs CP-1")
	}

	events := mustRun(t, "logs", "CP-1", "--events")
	if strings.Contains(events, " > ") || strings.Contains(events, " < ") || !strings.Contains(events, " | ") {
		t.Errorf("logs --events shows messages, or no events:\n%s", events)
	}

	if tail := lines(mustRun(t, "logs", "CP-1", "--tail", "2")); len(tail) != 2 || tail[1] != lines(all)[len(lines(all))-1] {
		t.Errorf("logs --tail 2 = %q, want the last two lines", tail)
	}
	if none := mustRun(t, "logs", "CP-1", "--tail", "0"); none != "" {
		t.Errorf("logs --tail 0 printed %q", none)
	}
	if recent := mustRun(t, "logs", "CP-1", "--since", "1h"); recent != all {
		t.Errorf("logs --since 1h differs from the whole log of a charger that just started")
	}
	if future := mustRun(t, "logs", "CP-1", "--since", "2999-01-01"); future != "" {
		t.Errorf("logs --since 2999-01-01 printed %q", future)
	}
}

func TestLogsAsJSON(t *testing.T) {
	useTempDataDir(t)
	csms := csmstest.Start(t)
	startRun(t, csms, "CP-1")

	type line struct {
		Time          string `json:"time"`
		ChargePointID string `json:"chargePointId"`
		Kind          string `json:"kind"`
		Text          string `json:"text"`
		Message       []any  `json:"message"`
	}
	kinds := map[string]int{}
	sawBoot := false
	for _, raw := range lines(mustRun(t, "logs", "CP-1", "--format", "json")) {
		var l line
		if err := json.Unmarshal([]byte(raw), &l); err != nil {
			t.Fatalf("line %q is not a JSON object: %v", raw, err)
		}
		if _, err := time.Parse(time.RFC3339Nano, l.Time); err != nil || l.ChargePointID != "CP-1" || l.Text == "" {
			t.Errorf("line = %+v", l)
		}
		kinds[l.Kind]++
		switch l.Kind {
		case "event":
			if l.Message != nil {
				t.Errorf("an event carries a message: %+v", l)
			}
		case "sent", "received":
			// The OCPP message is there as JSON, not only as text.
			if len(l.Message) < 3 {
				t.Errorf("a %s line has no OCPP message: %+v", l.Kind, l)
			}
			if l.Kind == "sent" && len(l.Message) == 4 && l.Message[2] == v16.ActionBootNotification {
				sawBoot = true
			}
		default:
			t.Errorf("unknown kind in %+v", l)
		}
	}
	if kinds["event"] == 0 || kinds["sent"] == 0 || kinds["received"] == 0 || !sawBoot {
		t.Errorf("kinds = %v, BootNotification seen: %v; want events, sent and received lines", kinds, sawBoot)
	}
}

func TestLogsFollow(t *testing.T) {
	useTempDataDir(t)
	csms := csmstest.Start(t)
	startRun(t, csms, "CP-1")

	followed, stop := background(t, "logs", "CP-1", "-f", "--events")
	eventually(t, "the followed log shows what is there already", func() bool {
		return strings.Contains(followed.String(), "accepted by the CSMS")
	})
	mustRun(t, "exec", "CP-1", "plug")
	eventually(t, "the followed log shows the new event", func() bool {
		return strings.Contains(followed.String(), "CP-1 | connector 1: EV plugged in")
	})
	if code := stop(); code != ExitOK {
		t.Errorf("logs -f exited with %d after Ctrl+C, want 0", code)
	}
}

func TestLogsOutliveTheirSite(t *testing.T) {
	dataDir := useTempDataDir(t)
	csms := csmstest.Start(t)
	upDetached(t, csms)

	// Following ends by itself when the site stops.
	var followed, stderr syncBuffer
	exit := make(chan int, 1)
	go func() {
		exit <- RunContext(context.Background(), []string{"site", "logs", "-s", "depot", "-f", "--events"}, &followed, &stderr)
	}()
	eventually(t, "the followed log shows the site", func() bool {
		return strings.Contains(followed.String(), "LOBBY-1 | accepted by the CSMS")
	})
	mustRun(t, "site", "stop", "-s", "depot")
	select {
	case code := <-exit:
		if code != ExitOK {
			t.Errorf("site logs -f exited with %d, stderr %q", code, stderr.String())
		}
	case <-time.After(10 * time.Second):
		t.Fatal("site logs -f did not end when the site stopped")
	}

	// A stopped charger still has its log.
	log := mustRun(t, "logs", "FAST-01")
	if !strings.Contains(log, "FAST-01 | accepted by the CSMS") || strings.Contains(log, "FAST-02") {
		t.Errorf("logs FAST-01 after the stop:\n%s", log)
	}
	// With -f it prints what there is and ends.
	if again := mustRun(t, "logs", "FAST-01", "-f"); again != log {
		t.Errorf("logs -f on a stopped charger differs from logs")
	}

	// Wiping the site wipes its log.
	mustRun(t, "site", "down", "-s", "depot", "--reset")
	if left, _ := filepath.Glob(filepath.Join(dataDir, "*")); len(left) != 0 {
		t.Errorf("site down --reset left %v", left)
	}
}

func TestTheLogStaysWithinTheCapOfTheSiteFile(t *testing.T) {
	dataDir := useTempDataDir(t)
	csms := csmstest.Start(t)
	path := writeSite(t, csms.URL(), fiveChargers+"log: { maxSize: 2kB, files: 2 }\n")
	startSiteUp(t, csms, path, fiveIDs)

	// Far more than 4 kB of log.
	for i := 0; i < 6; i++ {
		mustRun(t, "exec", "FAST-*", "plug")
		mustRun(t, "exec", "FAST-*", "unplug")
	}

	// The site is still writing, and for an instant during a rotation one of
	// the two files is on its way to its new name.
	var files []string
	eventually(t, "the log is in two files", func() bool {
		files, _ = filepath.Glob(filepath.Join(dataDir, "*.log*"))
		return len(files) == 2
	})
	if more, _ := filepath.Glob(filepath.Join(dataDir, "*.log.2")); len(more) != 0 {
		t.Errorf("a third log file was kept: %v", more)
	}
	for _, file := range files {
		if info, err := os.Stat(file); err == nil && info.Size() > 2000 {
			t.Errorf("%s holds %d bytes, want at most 2000", filepath.Base(file), info.Size())
		}
	}
	// What is left is the newest part, read across both files.
	log := lines(mustRun(t, "site", "logs", "-s", "depot"))
	if len(log) < 5 || !strings.Contains(strings.Join(log, "\n"), "EV unplugged") {
		t.Errorf("site logs shows %d lines, none of them of the last unplugs:\n%s", len(log), strings.Join(log, "\n"))
	}
	if strings.Contains(strings.Join(log, "\n"), "accepted by the CSMS") {
		t.Error("the log still holds the boot, which the cap should have dropped")
	}

	// A cap that makes no sense is a validation error.
	for body, want := range map[string]string{
		"log: { maxSize: 12 }\n":         "log.maxSize: invalid size",
		"log: { maxSize: 10B }\n":        "log.maxSize must be at least 1kB",
		"log: { files: 0 }\n":            "log.files must be from 1 to 100",
		"log: { maxSize: 1MB, kept: 2 }": "kept",
	} {
		bad := writeSite(t, csms.URL(), fiveChargers+body)
		if code, _, stderr := run("site", "validate", "-f", bad); code != ExitUsage || !strings.Contains(stderr, want) {
			t.Errorf("%q: exit %d, stderr %q; want exit %d and %q", body, code, stderr, ExitUsage, want)
		}
	}
}

func TestRunRmKeepsNoLog(t *testing.T) {
	dataDir := useTempDataDir(t)
	csms := csmstest.Start(t)
	startRun(t, csms, "CP-1", "--rm")

	code, _, stderr := run("logs", "CP-1")
	if code != ExitFailure || !strings.Contains(stderr, "there is no log") {
		t.Errorf("logs of a --rm charger: exit %d, stderr %q", code, stderr)
	}
	if files, _ := filepath.Glob(filepath.Join(dataDir, "*.log*")); len(files) != 0 {
		t.Errorf("a --rm charger left log files: %v", files)
	}
	// It can still be inspected while it runs.
	if out := mustRun(t, "inspect", "CP-1"); !strings.Contains(out, "Acme Wallbox, firmware 1.0") {
		t.Errorf("inspect of a --rm charger:\n%s", out)
	}
}

func TestStats(t *testing.T) {
	useTempDataDir(t)

	if out := mustRun(t, "stats"); !strings.HasPrefix(out, "CHARGE POINT ID") || !strings.Contains(out, "TOTAL            0 chargers") {
		t.Errorf("stats with nothing running:\n%s", out)
	}
	if out := mustRun(t, "stats", "--format", "json"); out != "" {
		t.Errorf("stats --format json with nothing running printed %q", out)
	}

	csms := csmstest.Start(t)
	startRun(t, csms, "CP-1", "--speed", "60")
	startRun(t, csms, "CP-2")
	startTransaction(t, csms, "CP-1")

	type row struct {
		Site             string  `json:"site"`
		Name             string  `json:"name"`
		ChargePointID    string  `json:"chargePointId"`
		Connection       string  `json:"connection"`
		PowerW           float64 `json:"powerW"`
		EnergyWh         float64 `json:"energyWh"`
		Transactions     int     `json:"transactions"`
		MessagesSent     int64   `json:"messagesSent"`
		MessagesReceived int64   `json:"messagesReceived"`
	}
	var rows []row
	eventually(t, "stats shows energy flowing", func() bool {
		rows = nil
		for _, raw := range lines(mustRun(t, "stats", "--format", "json")) {
			var r row
			if err := json.Unmarshal([]byte(raw), &r); err != nil {
				t.Fatalf("line %q is not a JSON object: %v", raw, err)
			}
			rows = append(rows, r)
		}
		return len(rows) == 2 && rows[0].EnergyWh > 0
	})
	busy, idle := rows[0], rows[1]
	if busy.ChargePointID != "CP-1" || busy.Site != "CP-1" || busy.Name != "cp-1" || busy.Connection != "online" ||
		busy.PowerW != 7400 || busy.Transactions != 1 || busy.MessagesSent < 4 || busy.MessagesReceived < 4 {
		t.Errorf("stats of the charging charger = %+v", busy)
	}
	if idle.ChargePointID != "CP-2" || idle.PowerW != 0 || idle.Transactions != 0 || idle.MessagesSent < 2 {
		t.Errorf("stats of the idle charger = %+v", idle)
	}

	// Output that is not a terminal is printed once, total first.
	table := lines(mustRun(t, "stats"))
	if len(table) != 4 || !strings.HasPrefix(table[1], "TOTAL            2 chargers") || !strings.Contains(table[1], "7.4 kW") ||
		!strings.HasPrefix(table[2], "CP-1 ") || !strings.Contains(table[2], "7.4 kW") || !strings.HasPrefix(table[3], "CP-2 ") {
		t.Errorf("stats table:\n%s", strings.Join(table, "\n"))
	}
	if only := lines(mustRun(t, "stats", "--no-stream", "-s", "CP-2")); len(only) != 3 || !strings.HasPrefix(only[2], "CP-2 ") {
		t.Errorf("stats -s CP-2:\n%s", strings.Join(only, "\n"))
	}
}

func TestInspect(t *testing.T) {
	useTempDataDir(t)
	csms := csmstest.Start(t)
	startRun(t, csms, "CP-1")
	startTransaction(t, csms, "CP-1")

	// The CSMS installs a charging profile and changes a key.
	sixteen := v16.SetChargingProfileRequest{ConnectorID: 0, CsChargingProfiles: v16.ChargingProfile{
		ChargingProfileID: 5, ChargingProfilePurpose: v16.PurposeTxDefault, ChargingProfileKind: v16.KindAbsolute,
		ChargingSchedule: v16.ChargingSchedule{ChargingRateUnit: v16.UnitAmps, ChargingSchedulePeriod: []v16.ChargingSchedulePeriod{{Limit: 16}}},
	}}
	var answer v16.StatusAnswer
	if err := csms.Call(context.Background(), "CP-1", v16.ActionSetChargingProfile, sixteen, &answer); err != nil || answer.Status != v16.StatusAccepted {
		t.Fatalf("SetChargingProfile: %v, %+v", err, answer)
	}
	if err := csms.Call(context.Background(), "CP-1", v16.ActionChangeConfiguration, v16.ChangeConfigurationRequest{Key: "HeartbeatInterval", Value: "77"}, &answer); err != nil {
		t.Fatal(err)
	}

	type view struct {
		Site          string `json:"site"`
		Name          string `json:"name"`
		ChargePointID string `json:"chargePointId"`
		Connection    string `json:"connection"`
		CSMSURL       string `json:"csmsUrl"`
		Model         struct {
			Vendor  string  `json:"vendor"`
			Model   string  `json:"model"`
			OCPP    string  `json:"ocpp"`
			Voltage float64 `json:"voltage"`
			Phases  int     `json:"phases"`
		} `json:"model"`
		Connectors []struct {
			ID        int     `json:"id"`
			Type      string  `json:"type"`
			MaxPowerW float64 `json:"maxPowerW"`
			Status    string  `json:"status"`
			EV        *struct {
				BatteryWh float64 `json:"batteryWh"`
			} `json:"ev"`
			Transaction *struct {
				ID        int    `json:"id"`
				IDTag     string `json:"idTag"`
				StartedAt string `json:"startedAt"`
			} `json:"transaction"`
		} `json:"connectors"`
		ChargingProfiles []struct {
			ConnectorID int                 `json:"connectorId"`
			Profile     v16.ChargingProfile `json:"profile"`
		} `json:"chargingProfiles"`
		Configuration []struct {
			Key      string `json:"key"`
			Value    string `json:"value"`
			Readonly bool   `json:"readonly"`
		} `json:"configuration"`
		QueuedMessages []string `json:"queuedMessages"`
		MessagesSent   int64    `json:"messagesSent"`
	}
	read := func() (view, string) {
		t.Helper()
		raw := mustRun(t, "inspect", "CP-1", "--format", "json")
		var v view
		if err := json.Unmarshal([]byte(raw), &v); err != nil || len(lines(raw)) != 1 {
			t.Fatalf("inspect --format json is not one JSON object on one line (%v):\n%s", err, raw)
		}
		return v, raw
	}

	v, raw := read()
	if v.ChargePointID != "CP-1" || v.Site != "CP-1" || v.Name != "cp-1" || v.Connection != "online" || v.CSMSURL != csms.URL() ||
		v.Model.Vendor != "Acme" || v.Model.Model != "Wallbox" || v.Model.OCPP != "1.6" || v.Model.Voltage != 230 || v.Model.Phases != 3 || v.MessagesSent == 0 {
		t.Errorf("inspect = %+v", v)
	}
	k := v.Connectors[0]
	if len(v.Connectors) != 1 || k.ID != 1 || k.Type != "Type2" || k.MaxPowerW != 7400 || k.Status != v16.StatusCharging ||
		k.EV == nil || k.EV.BatteryWh != 60000 || k.Transaction == nil || k.Transaction.ID != 1 || k.Transaction.IDTag != "TAG-1" || k.Transaction.StartedAt == "" {
		t.Errorf("connector = %+v, EV %+v, transaction %+v", k, k.EV, k.Transaction)
	}
	if len(v.ChargingProfiles) != 1 || v.ChargingProfiles[0].ConnectorID != 0 || v.ChargingProfiles[0].Profile.ChargingProfileID != 5 {
		t.Errorf("charging profiles = %+v", v.ChargingProfiles)
	}
	keys := map[string]string{}
	for _, key := range v.Configuration {
		keys[key.Key] = key.Value
		if key.Key == "NumberOfConnectors" && !key.Readonly {
			t.Error("NumberOfConnectors is not marked read-only")
		}
	}
	if keys["HeartbeatInterval"] != "77" || keys["NumberOfConnectors"] != "1" || keys["SupportedFeatureProfiles"] == "" {
		t.Errorf("configuration = %v", keys)
	}
	if !strings.Contains(raw, `"queuedMessages":[]`) || strings.Contains(strings.ToLower(raw), "password") {
		t.Errorf("inspect JSON:\n%s", raw)
	}

	text := mustRun(t, "inspect", "cp-1")
	for _, want := range []string{
		"CP-1\n", "Name        cp-1", "Connection  online", "Model       Acme Wallbox, firmware 1.0", "OCPP        1.6", "Supply      230 V, 3 phases",
		"Connectors\n  1  Type2, 7.4kW  Charging", "EV: 20% of 60 kWh, charging to 100%", "transaction: 1, tag TAG-1, started ",
		"Charging profiles\n  5  whole charger, TxDefaultProfile, stack level 0, Absolute: 16 A from 0s",
		"Queued for the CSMS\n  nothing", "HeartbeatInterval", "77", "NumberOfConnectors", "read-only",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("inspect is missing %q:\n%s", want, text)
		}
	}

	// A stopped charger is inspected from its saved state. Its transaction
	// ended with the stop; the profile and the changed key are kept.
	mustRun(t, "stop", "CP-1")
	eventually(t, "the charger shows as stopped", func() bool { return strings.Contains(psRow(t, "CP-1"), "stopped") })
	v, _ = read()
	if v.Connection != "stopped" || v.Connectors[0].Transaction != nil || v.Connectors[0].Status != v16.StatusFinishing ||
		v.Connectors[0].EV == nil || len(v.ChargingProfiles) != 1 {
		t.Errorf("inspect of the stopped charger = %+v", v)
	}
	if stopped := mustRun(t, "inspect", "CP-1"); !strings.Contains(stopped, "Connection  stopped") || !strings.Contains(stopped, "77") {
		t.Errorf("inspect of the stopped charger:\n%s", stopped)
	}
}

func TestInfo(t *testing.T) {
	dataDir := useTempDataDir(t)

	out := mustRun(t, "info")
	if want := "Version:         dev\nOCPP:            1.6J\nBuilt with:      " + runtime.Version() + ", for " + runtime.GOOS + "/" + runtime.GOARCH + "\n"; !strings.HasPrefix(out, want) {
		t.Errorf("info starts with:\n%s\nwant:\n%s", out, want)
	}
	if !strings.Contains(out, "Data directory:  "+dataDir+"\n") || !strings.Contains(out, "Sites:           0 running, 0 stopped, 0 down\n") || strings.Contains(out, "SITE") {
		t.Errorf("info with nothing there:\n%s", out)
	}

	csms := csmstest.Start(t)
	startRun(t, csms, "CP-1")
	out = mustRun(t, "info")
	if !strings.Contains(out, "1 running, 0 stopped, 0 down") || !strings.Contains(out, "SITE  STATUS   CHARGERS  POWERED ON  PROCESS") || !strings.Contains(out, csms.URL()) {
		t.Errorf("info with a charger running:\n%s", out)
	}

	var v struct {
		Version       string   `json:"version"`
		OCPP          []string `json:"ocpp"`
		Go            string   `json:"go"`
		OS            string   `json:"os"`
		Arch          string   `json:"arch"`
		DataDirectory string   `json:"dataDirectory"`
		Sites         []struct {
			Site     string   `json:"site"`
			Status   string   `json:"status"`
			Chargers int      `json:"chargers"`
			Powered  int      `json:"powered"`
			PID      int      `json:"pid"`
			CSMS     []string `json:"csms"`
		} `json:"sites"`
	}
	raw := mustRun(t, "info", "--format", "json")
	if err := json.Unmarshal([]byte(raw), &v); err != nil || len(lines(raw)) != 1 {
		t.Fatalf("info --format json is not one JSON object on one line (%v):\n%s", err, raw)
	}
	if v.Version == "" || len(v.OCPP) == 0 || v.Go != runtime.Version() || v.OS != runtime.GOOS || v.Arch != runtime.GOARCH ||
		v.DataDirectory != dataDir || len(v.Sites) != 1 {
		t.Fatalf("info = %+v", v)
	}
	if s := v.Sites[0]; s.Site != "CP-1" || s.Status != "running" || s.Chargers != 1 || s.Powered != 1 || s.PID != os.Getpid() || len(s.CSMS) != 1 {
		t.Errorf("site = %+v", s)
	}
}

func TestObservingCommandsExitCodes(t *testing.T) {
	useTempDataDir(t)

	for _, tc := range []struct {
		args []string
		code int
		want string
	}{
		// Usage errors exit with 2.
		{[]string{"logs"}, ExitUsage, "accepts 1 arg(s), received 0"},
		{[]string{"logs", "CP-1", "--tail", "-5"}, ExitUsage, "--tail must be 0 or more"},
		{[]string{"logs", "CP-1", "--since", "yesterday"}, ExitUsage, `invalid --since "yesterday"`},
		{[]string{"logs", "CP-1", "--format", "xml"}, ExitUsage, `unknown format "xml"`},
		{[]string{"logs", "CP-[", "--format", "json"}, ExitUsage, "invalid charger pattern"},
		{[]string{"site", "logs", "extra"}, ExitUsage, "unknown command"},
		{[]string{"stats", "extra"}, ExitUsage, "unknown command"},
		{[]string{"stats", "--format", "yaml"}, ExitUsage, `unknown format "yaml"`},
		{[]string{"inspect"}, ExitUsage, "accepts 1 arg(s), received 0"},
		{[]string{"inspect", "CP-*"}, ExitUsage, "inspect shows one charger"},
		{[]string{"info", "extra"}, ExitUsage, "unknown command"},
		{[]string{"info", "--format", "xml"}, ExitUsage, `unknown format "xml"`},
		// What cannot be done exits with 1.
		{[]string{"logs", "CP-1"}, ExitFailure, `no charger matches "CP-1"`},
		{[]string{"site", "logs", "-s", "nowhere"}, ExitFailure, `no site is named "nowhere"`},
		{[]string{"stats", "-s", "nowhere"}, ExitFailure, `no site is named "nowhere"`},
		{[]string{"inspect", "CP-1"}, ExitFailure, `no charger matches "CP-1"`},
	} {
		code, stdout, stderr := run(tc.args...)
		if code != tc.code || !strings.Contains(stderr, tc.want) || stdout != "" {
			t.Errorf("%v: exit %d, stdout %q, stderr %q\nwant exit %d and %q", tc.args, code, stdout, stderr, tc.code, tc.want)
		}
	}
}

func TestParseLogLine(t *testing.T) {
	e, ok := parseLogLine(`2026-01-02T15:04:05.123+01:00 CP 1 > [2,"7","Heartbeat",{}]`)
	if !ok || e.ChargePointID != "CP 1" || e.Text != `[2,"7","Heartbeat",{}]` || e.Time.Nanosecond() != 123e6 {
		t.Errorf("parseLogLine = %+v, %v", e, ok)
	}
	// The first marker decides, whatever the text holds.
	e, ok = parseLogLine("2026-01-02T15:04:05.000Z CP-1 | the CSMS set X to \"a > b\"")
	if !ok || e.ChargePointID != "CP-1" || e.Text != `the CSMS set X to "a > b"` {
		t.Errorf("parseLogLine = %+v, %v", e, ok)
	}
	for _, bad := range []string{"", "panic: something", "2026-01-02T15:04:05.000Z no marker here"} {
		if _, ok := parseLogLine(bad); ok {
			t.Errorf("parseLogLine(%q) succeeded", bad)
		}
	}
}
