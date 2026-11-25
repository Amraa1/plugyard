package cli

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Amraa1/plugyard/internal/api"
	"github.com/Amraa1/plugyard/internal/csmstest"
	"github.com/Amraa1/plugyard/internal/ocpp/v16"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// apiCall calls the running API and returns the status and the body.
func apiCall(t *testing.T, base, method, path, token, body string) (int, string) {
	t.Helper()
	var reader io.Reader
	if method == http.MethodPost {
		reader = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, base+path, reader)
	if err != nil {
		t.Fatal(err)
	}
	if method == http.MethodPost {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	client := &http.Client{Timeout: 10 * time.Second}
	defer client.CloseIdleConnections()
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, strings.TrimSpace(string(data))
}

// apiUp starts the API in the background and returns its address.
func apiUp(t *testing.T) string {
	t.Helper()
	t.Setenv(asCLIEnv, "1")
	out := mustRun(t, "api", "up", "-d", "--listen", "127.0.0.1:0")
	t.Cleanup(func() { run("api", "down") })
	if !strings.HasPrefix(out, "The API is running in the background at http://127.0.0.1:") ||
		!strings.Contains(out, "See its calls with: plugyard api logs\nStop it with:       plugyard api down\n") {
		t.Fatalf("api up -d printed:\n%s", out)
	}
	var status apiStatus
	if err := json.Unmarshal([]byte(mustRun(t, "api", "status", "--format", "json")), &status); err != nil || !status.Running {
		t.Fatalf("api status: %+v, %v", status, err)
	}
	return status.URL
}

func TestAPIActsOnChargersLikeTheCommands(t *testing.T) {
	useTempDataDir(t)
	csms := csmstest.Start(t)
	startRun(t, csms, "CP-1")
	startRun(t, csms, "CP-2")

	// Nothing runs yet.
	if out := mustRun(t, "api", "status"); out != "The API is not running. Start it with: plugyard api up -d\n" {
		t.Errorf("api status = %q", out)
	}
	if code, _, stderr := run("api", "down"); code != ExitFailure || !strings.Contains(stderr, "the API is not running") {
		t.Errorf("api down with nothing running: exit %d, stderr %q", code, stderr)
	}

	base := apiUp(t)
	out := mustRun(t, "api", "status")
	for _, want := range []string{"Address  " + base, "Tokens   calls from this machine need no token", "Traffic  not encrypted (HTTP)"} {
		if !strings.Contains(out, want) {
			t.Errorf("api status is missing %q:\n%s", want, out)
		}
	}
	if code, _, stderr := run("api", "up", "-d"); code != ExitFailure || !strings.Contains(stderr, "the API is already running, at "+base) {
		t.Errorf("a second api up: exit %d, stderr %q", code, stderr)
	}

	// The list is what ps -a prints as JSON.
	status, body := apiCall(t, base, "GET", "/v1/chargers", "", "")
	var list struct {
		Chargers []json.RawMessage `json:"chargers"`
	}
	if status != 200 || json.Unmarshal([]byte(body), &list) != nil || len(list.Chargers) != 2 {
		t.Fatalf("list = %d %s", status, body)
	}
	lines := strings.Split(strings.TrimSpace(mustRun(t, "ps", "-a", "--format", "json")), "\n")
	for i, line := range lines {
		var a, b map[string]any
		json.Unmarshal([]byte(line), &a)
		json.Unmarshal(list.Chargers[i], &b)
		// The counts of messages move on between the two.
		for _, m := range []map[string]any{a, b} {
			delete(m, "messagesSent")
			delete(m, "messagesReceived")
		}
		if x, _ := json.Marshal(a); string(x) != func() string { y, _ := json.Marshal(b); return string(y) }() {
			t.Errorf("charger %d: ps says %s, the API says %s", i, line, list.Chargers[i])
		}
	}

	// An action does what the command does, and answers what it prints.
	status, body = apiCall(t, base, "POST", "/v1/chargers/CP-1/plug", "", `{"soc":"30%","battery":"82kWh"}`)
	if status != 200 || body != `{"results":[{"chargePointId":"CP-1","connector":1,"ok":true,"message":"EV plugged in (82 kWh, 30%)"}]}` {
		t.Errorf("plug = %d %s", status, body)
	}
	if status, body = apiCall(t, base, "POST", "/v1/chargers/CP-1/tap", "", `{"tag":"04A1B2C3"}`); status != 200 || !strings.Contains(body, `"ok":true`) {
		t.Errorf("tap = %d %s", status, body)
	}
	csms.WaitForCalls(t, "CP-1", v16.ActionStartTransaction, 1)
	mustRun(t, "wait", "CP-1", "Charging", "--timeout", "10s")

	// One charger in full, as inspect prints it.
	status, body = apiCall(t, base, "GET", "/v1/chargers/cp-1", "", "")
	var view struct {
		ChargePointID string `json:"chargePointId"`
		Connectors    []struct {
			Status string `json:"status"`
		} `json:"connectors"`
	}
	if status != 200 || json.Unmarshal([]byte(body), &view) != nil || view.ChargePointID != "CP-1" || view.Connectors[0].Status != "Charging" {
		t.Errorf("inspect by name = %d %s", status, body)
	}

	// A pattern acts on every charger it matches, and a failure on one of
	// them is a conflict that still carries every result.
	status, body = apiCall(t, base, "POST", "/v1/chargers/CP-*/plug", "", "{}")
	if status != 409 || !strings.Contains(body, `{"chargePointId":"CP-1","connector":1,"ok":false,`) || !strings.Contains(body, `{"chargePointId":"CP-2","connector":1,"ok":true,`) {
		t.Errorf("plug on every charger = %d %s", status, body)
	}
	if status, body = apiCall(t, base, "POST", "/v1/chargers/CP-2/fault", "", `{"code":"GroundFailure","info":"leak"}`); status != 200 || !strings.Contains(body, "fault GroundFailure reported") {
		t.Errorf("fault = %d %s", status, body)
	}
	if row := psRow(t, "CP-2"); row != "CP-2 cp-2 CP-2 online 1:Faulted(GroundFailure)" {
		t.Errorf("ps row = %q", row)
	}
	if status, _ = apiCall(t, base, "POST", "/v1/chargers/CP-2/offline", "", `{"for":"200ms"}`); status != 200 {
		t.Errorf("offline = %d", status)
	}

	// What is wrong with a call is answered as the command would say it.
	mustRun(t, "stop", "CP-2")
	for _, tc := range []struct {
		method, path, body string
		status             int
		want               string
	}{
		{"POST", "/v1/chargers/CP-9/plug", "{}", 404, `no running charger matches \"CP-9\"`},
		{"GET", "/v1/chargers/CP-9", "", 404, `no charger matches \"CP-9\"`},
		{"GET", "/v1/chargers/CP-*", "", 400, "cannot be a pattern"},
		{"GET", "/v1/chargers?site=nowhere", "", 404, `no site is named \"nowhere\"`},
		{"POST", "/v1/chargers/CP-2/plug", "{}", 409, `charger \"CP-2\" is stopped`},
		{"POST", "/v1/chargers/CP-1/tap", "{}", 400, `tap needs \"tag\"`},
		{"POST", "/v1/chargers/CP-1/fault", `{"code":"Broken"}`, 400, "one of the OCPP 1.6 error codes: ConnectorLockFailure"},
		{"POST", "/v1/chargers/CP-1/fail", `{"step":"lunch"}`, 400, `fail needs \"step\", one of download, install, upload`},
		{"POST", "/v1/chargers/CP-1/reboot", `{"connector":1}`, 400, `reboot takes no options, and \"connector\" was given`},
		{"POST", "/v1/chargers/CP-1/unplug", `{"tag":"x"}`, 400, `unplug does not take \"tag\": it takes connector`},
		{"POST", "/v1/chargers/CP-1/plug", `{"connector":0}`, 400, "the connector number must be 1 or more"},
		{"POST", "/v1/chargers/CP-1/plug", `{"battery":"big"}`, 400, "battery: "},
		{"POST", "/v1/chargers/CP-1/offline", `{"for":"soon"}`, 400, "for must be a time above zero"},
	} {
		status, body := apiCall(t, base, tc.method, tc.path, "", tc.body)
		if status != tc.status || !strings.Contains(body, tc.want) {
			t.Errorf("%s %s %s = %d %s, want %d and %q", tc.method, tc.path, tc.body, status, body, tc.status, tc.want)
		}
	}
	// A stopped charger is listed, as stopped.
	if _, body = apiCall(t, base, "GET", "/v1/chargers", "", ""); !strings.Contains(body, `"connection":"stopped"`) {
		t.Errorf("the list has no stopped charger:\n%s", body)
	}

	// The log says who did what to which charger.
	logs := mustRun(t, "api", "logs", "--charger", "CP-2")
	for _, want := range []string{" local 127.0.0.1 200 fault CP-2: fault GroundFailure reported\n", " local 127.0.0.1 409 plug CP-1,CP-2: failed on 1 of 2 chargers\n"} {
		if !strings.Contains(logs, want) {
			t.Errorf("api logs --charger CP-2 is missing %q:\n%s", want, logs)
		}
	}
	if strings.Contains(logs, "tap") {
		t.Errorf("api logs --charger CP-2 shows calls about another charger:\n%s", logs)
	}
	if last := mustRun(t, "api", "logs", "--tail", "1", "--format", "json"); strings.Count(last, "\n") != 1 || !strings.Contains(last, `"token":"local"`) {
		t.Errorf("api logs --tail 1 --format json = %q", last)
	}

	if out := mustRun(t, "api", "down"); out != "The API at "+base+" is stopped\n" {
		t.Errorf("api down printed %q", out)
	}
	if out := mustRun(t, "api", "status"); !strings.HasPrefix(out, "The API is not running.") {
		t.Errorf("api status after down = %q", out)
	}
	// The log outlives the API, and uninstall counts it as plugyard's.
	if out := mustRun(t, "api", "logs", "--tail", "1"); out == "" {
		t.Error("the log is gone with the API")
	}
}

func TestAPITokens(t *testing.T) {
	useTempDataDir(t)
	csms := csmstest.Start(t)
	startRun(t, csms, "CP-1")
	startRun(t, csms, "CP-2")

	if out := mustRun(t, "api", "token", "ls"); out != "NAME  SITE  CAN  CREATED\n" {
		t.Errorf("token ls with no tokens = %q", out)
	}
	// The token alone goes to standard output.
	code, token, stderr := run("api", "token", "create", "mobile-team", "--site", "CP-1")
	token = strings.TrimSpace(token)
	if code != ExitOK || !strings.HasPrefix(token, "pyd_") || strings.ContainsAny(token, " \n") ||
		!strings.Contains(stderr, "Token mobile-team is for site CP-1, and can act on chargers and look at them.") ||
		!strings.Contains(stderr, "It is shown this once") {
		t.Fatalf("token create: exit %d, stdout %q, stderr %q", code, token, stderr)
	}
	_, looker, stderr := run("api", "token", "create", "dashboard", "--read-only")
	looker = strings.TrimSpace(looker)
	if !strings.Contains(stderr, "Token dashboard is for every site, and can look at chargers and not act on them.") {
		t.Errorf("token create --read-only said %q", stderr)
	}
	if code, _, stderr := run("api", "token", "create", "dashboard"); code != ExitUsage || !strings.Contains(stderr, "exists already") {
		t.Errorf("a second token of the same name: exit %d, stderr %q", code, stderr)
	}
	out := mustRun(t, "api", "token", "ls")
	if strings.Contains(out, token) || strings.Contains(out, looker) ||
		!strings.Contains(out, "dashboard    every site  look          ") || !strings.Contains(out, "mobile-team  CP-1        act and look  ") {
		t.Errorf("token ls =\n%s", out)
	}
	if rows := mustRun(t, "api", "token", "ls", "--format", "json"); strings.Contains(rows, "hash") || !strings.Contains(rows, `"name":"mobile-team","site":"CP-1","readOnly":false`) {
		t.Errorf("token ls --format json =\n%s", rows)
	}

	base := apiUp(t)
	if out := mustRun(t, "api", "status"); !strings.Contains(out, "Tokens   every call needs a token (2 tokens)") {
		t.Errorf("api status with tokens:\n%s", out)
	}
	// Every call needs a token now, also from this machine.
	if status, _ := apiCall(t, base, "GET", "/v1/chargers", "", ""); status != 401 {
		t.Errorf("a call without a token = %d", status)
	}
	// A token for one site sees and reaches only that site. A charger
	// started with run is a site of its own, named after it.
	status, body := apiCall(t, base, "GET", "/v1/chargers", token, "")
	if status != 200 || !strings.Contains(body, `"CP-1"`) || strings.Contains(body, `"CP-2"`) {
		t.Errorf("list with a token of site CP-1 = %d %s", status, body)
	}
	if status, _ := apiCall(t, base, "POST", "/v1/chargers/CP-1/plug", token, "{}"); status != 200 {
		t.Errorf("plug within the token's site = %d", status)
	}
	// A charger of another site and one that is nowhere get the same
	// answer.
	for _, id := range []string{"CP-2", "CP-9"} {
		status, body := apiCall(t, base, "POST", "/v1/chargers/"+id+"/plug", token, "{}")
		if status != 403 || !strings.Contains(body, `token mobile-team is limited to site CP-1: no running charger matches \"`+id+`\"`) {
			t.Errorf("plug on %s, outside the token's site = %d %s", id, status, body)
		}
	}
	if row := psRow(t, "CP-2"); row != "CP-2 cp-2 CP-2 online 1:Available" {
		t.Errorf("a token of another site acted on CP-2: %q", row)
	}
	// A read-only token looks everywhere and acts nowhere.
	if status, body := apiCall(t, base, "GET", "/v1/chargers", looker, ""); status != 200 || !strings.Contains(body, `"CP-2"`) {
		t.Errorf("list with a read-only token = %d %s", status, body)
	}
	if status, _ := apiCall(t, base, "POST", "/v1/chargers/CP-2/plug", looker, "{}"); status != 403 {
		t.Errorf("plug with a read-only token = %d", status)
	}
	if logs := mustRun(t, "api", "logs"); !strings.Contains(logs, " mobile-team 127.0.0.1 200 plug CP-1: ") || !strings.Contains(logs, " dashboard 127.0.0.1 403 plug CP-2: token dashboard is read-only") ||
		strings.Contains(logs, token) {
		t.Errorf("api logs =\n%s", logs)
	}

	// Removing takes several names, and works at once.
	code, out, stderr = run("api", "token", "rm", "mobile-team", "nobody", "dashboard")
	if code != ExitFailure || out != "Token mobile-team is removed\nToken dashboard is removed\n" || !strings.Contains(stderr, `there is no token named "nobody"`) {
		t.Errorf("token rm: exit %d, stdout %q, stderr %q", code, out, stderr)
	}
	if status, _ := apiCall(t, base, "GET", "/v1/chargers", token, ""); status != 200 {
		// With no token left, this machine needs none again.
		t.Errorf("a call after every token was removed = %d", status)
	}
}

func TestAPIBeyondThisMachineAndItsDescription(t *testing.T) {
	useTempDataDir(t)

	// Without a token, it does not listen where others can reach it.
	code, _, stderr := run("api", "up", "--listen", "0.0.0.0:0")
	if code != ExitFailure || !strings.Contains(stderr, "no token exists") || !strings.Contains(stderr, `"plugyard api token create <name>"`) {
		t.Errorf("api up beyond this machine without a token: exit %d, stderr %q", code, stderr)
	}
	if code, _, stderr := run("api", "up", "--tls-cert", "cert.pem"); code != ExitUsage || !strings.Contains(stderr, "--tls-cert and --tls-key go together") {
		t.Errorf("api up with a certificate and no key: exit %d, stderr %q", code, stderr)
	}

	// The description is the one the server serves.
	if out := mustRun(t, "api", "spec"); out != string(api.Spec) {
		t.Error("api spec prints something other than the description of the server")
	}
	// The suite alone shows its help.
	if out := mustRun(t, "api"); !strings.Contains(out, "plugyard api [command]") {
		t.Errorf("plugyard api printed:\n%s", out)
	}
	for _, args := range [][]string{{"api", "serve"}, {"api", "token", "show"}, {"api", "logs", "extra"}, {"api", "token", "rm"}} {
		if code, _, stderr := run(args...); code != ExitUsage || stderr == "" {
			t.Errorf("%v: exit %d", args, code)
		}
	}
}

func TestAPIWaitsAndReadsTheLogLikeTheCommands(t *testing.T) {
	useTempDataDir(t)
	csms := csmstest.Start(t)
	startRun(t, csms, "CP-1")
	startRun(t, csms, "CP-2")
	base := apiUp(t)

	type waited struct {
		Reached  bool         `json:"reached"`
		Chargers []api.Waited `json:"chargers"`
	}
	wait := func(path string) waited {
		t.Helper()
		status, body := apiCall(t, base, "GET", path, "", "")
		var got waited
		if err := json.Unmarshal([]byte(body), &got); status != 200 || err != nil {
			t.Fatalf("GET %s = %d %s", path, status, body)
		}
		return got
	}

	// A state the charger is in already is reached at once, whatever the
	// case it is written in.
	got := wait("/v1/chargers/cp-1/wait?state=available")
	if !got.Reached || len(got.Chargers) != 1 || got.Chargers[0].ChargePointID != "CP-1" || got.Chargers[0].Connector != 1 || got.Chargers[0].State != v16.StatusAvailable {
		t.Errorf("a wait for Available = %+v", got)
	}
	if got := wait("/v1/chargers/CP-*/wait?state=online"); !got.Reached || len(got.Chargers) != 2 || got.Chargers[1].Connector != 0 || got.Chargers[1].State != "online" {
		t.Errorf("a wait for online = %+v", got)
	}

	// A state that comes later: the call is answered when it does.
	done := make(chan waited, 1)
	go func() { done <- wait("/v1/chargers/CP-*/wait?state=Charging&timeout=8s") }()
	apiCall(t, base, "POST", "/v1/chargers/CP-*/plug", "", "{}")
	apiCall(t, base, "POST", "/v1/chargers/CP-1/tap", "", `{"tag":"TAG-1"}`)
	select {
	case got := <-done:
		t.Fatalf("the wait was answered before CP-2 was charging: %+v", got)
	case <-time.After(300 * time.Millisecond):
	}
	apiCall(t, base, "POST", "/v1/chargers/CP-2/tap", "", `{"tag":"TAG-2"}`)
	select {
	case got := <-done:
		if !got.Reached || len(got.Chargers) != 2 || got.Chargers[1].State != v16.StatusCharging {
			t.Errorf("a wait for Charging = %+v", got)
		}
	case <-time.After(8 * time.Second):
		t.Fatal("the wait was not answered once both chargers were charging")
	}

	// A state that does not come is an answer too, and says where each
	// charger is.
	apiCall(t, base, "POST", "/v1/chargers/CP-2/tap", "", `{"tag":"TAG-2"}`)
	wait("/v1/chargers/CP-2/wait?state=Finishing")
	start := time.Now()
	got = wait("/v1/chargers/CP-*/wait?state=Charging&timeout=300ms")
	if got.Reached || len(got.Chargers) != 2 || !got.Chargers[0].Reached || got.Chargers[1].Reached || got.Chargers[1].State != v16.StatusFinishing {
		t.Errorf("a wait that runs out = %+v", got)
	}
	if took := time.Since(start); took < 300*time.Millisecond || took > 5*time.Second {
		t.Errorf("a wait with a timeout of 300ms took %s", took)
	}

	// A stopped charger is "stopped", and has no connector status.
	mustRun(t, "stop", "CP-2")
	if got := wait("/v1/chargers/CP-2/wait?state=stopped&timeout=8s"); !got.Reached || got.Chargers[0].Connector != 0 {
		t.Errorf("a wait for stopped = %+v", got)
	}
	if got := wait("/v1/chargers/CP-2/wait?state=Available&timeout=200ms"); got.Reached || got.Chargers[0].State != "stopped" || got.Chargers[0].Connector != 0 {
		t.Errorf("a wait for a connector of a stopped charger = %+v", got)
	}

	// The log is the one the command prints, also of a stopped charger.
	lines := func(path string) []string {
		t.Helper()
		status, body := apiCall(t, base, "GET", path, "", "")
		var got struct {
			Lines []json.RawMessage `json:"lines"`
		}
		if err := json.Unmarshal([]byte(body), &got); status != 200 || err != nil || got.Lines == nil {
			t.Fatalf("GET %s = %d %s", path, status, body)
		}
		var out []string
		for _, l := range got.Lines {
			out = append(out, string(l))
		}
		return out
	}
	want := strings.Split(strings.TrimSpace(mustRun(t, "logs", "CP-2", "--events", "--format", "json")), "\n")
	if got := lines("/v1/chargers/CP-2/logs?events=true"); strings.Join(got, "\n") != strings.Join(want, "\n") || len(got) < 3 {
		t.Errorf("the events of CP-2 through the API:\n%s\nand from the command:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if got := lines("/v1/chargers/CP-2/logs?events=true&tail=2&since=1h"); strings.Join(got, "\n") != strings.Join(want[len(want)-2:], "\n") {
		t.Errorf("the last two events of CP-2 = %v", got)
	}
	if got := lines("/v1/chargers/CP-2/logs?since=2099-01-01"); len(got) != 0 {
		t.Errorf("the log from a time yet to come = %v", got)
	}
	all := strings.Join(lines("/v1/chargers/CP-*/logs"), "\n")
	for _, want := range []string{`"chargePointId":"CP-1"`, `"chargePointId":"CP-2"`, `"kind":"sent"`, `"kind":"received"`, `"message":[2,`} {
		if !strings.Contains(all, want) {
			t.Errorf("the log of both chargers lacks %s", want)
		}
	}

	for path, want := range map[string]string{
		"/v1/chargers/CP-1/wait?state=Sleeping":              `400 {"error":"unknown state \"Sleeping\": want a connector status (Available, `,
		"/v1/chargers/CP-1/wait?state=Reserved":              `400 {"error":"unknown state`,
		"/v1/chargers/CP-9/wait?state=Available":             `404 {"error":"no charger matches \"CP-9\""}`,
		"/v1/chargers/CP-%5B/wait?state=Available":           `400 {"error":"invalid charger pattern`,
		"/v1/chargers/CP-1/wait?state=Available&connector=2": `400 {"error":"charger CP-1 has no connector 2"}`,
		"/v1/chargers/CP-1/wait?state=online&connector=1":    `400 {"error":"online is a state of the whole charger, so it takes no connector"}`,
		"/v1/chargers/CP-1/wait?state=online&site=nowhere":   `404 {"error":"no site is named \"nowhere\""}`,
		"/v1/chargers/CP-9/logs":                             `404 {"error":"no charger matches \"CP-9\""}`,
		"/v1/chargers/CP-1/logs?since=yesterday":             `400 {"error":"invalid since \"yesterday\": want how long ago`,
		"/v1/chargers/CP-1/logs?since=-5m":                   `400 {"error":"since cannot be negative"}`,
	} {
		status, body := apiCall(t, base, "GET", path, "", "")
		if got := strconv.Itoa(status) + " " + body; !strings.HasPrefix(got, want) {
			t.Errorf("GET %s = %s\nwant %s", path, got, want)
		}
	}

	// The calls are in the log of the API, with how each wait ended.
	log := mustRun(t, "api", "logs")
	for _, want := range []string{" 200 wait CP-1,CP-2: Charging\n", " 200 wait CP-1,CP-2: not Charging after 300ms\n", " 200 logs CP-2\n", " 404 wait CP-9: no charger matches"} {
		if !strings.Contains(log, want) {
			t.Errorf("api logs lacks %q:\n%s", want, log)
		}
	}
}

// An AI agent drives the chargers through the tools of the API, with a
// client of the Model Context Protocol.
func TestAnAgentDrivesChargersThroughTheTools(t *testing.T) {
	useTempDataDir(t)
	csms := csmstest.Start(t)
	startRun(t, csms, "CP-1")
	base := apiUp(t)

	out := mustRun(t, "api", "status")
	if !strings.Contains(out, "Agents   "+base+"/mcp (MCP)\n") {
		t.Errorf("api status does not say where agents connect:\n%s", out)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil)
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: base + api.MCPPath, DisableStandaloneSSE: true}, nil)
	if err != nil {
		t.Fatalf("connecting to the tools: %v", err)
	}
	defer session.Close()
	use := func(tool string, args map[string]any) string {
		t.Helper()
		result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: tool, Arguments: args})
		if err != nil {
			t.Fatalf("%s: %v", tool, err)
		}
		text := result.Content[0].(*mcp.TextContent).Text
		if result.IsError {
			t.Fatalf("%s failed: %s", tool, text)
		}
		return text
	}

	if got := use("list_chargers", nil); !strings.Contains(got, `"chargePointId":"CP-1"`) || !strings.Contains(got, `"connection":"online"`) {
		t.Errorf("list_chargers = %s", got)
	}
	// A charging session, the way the instructions of the server describe it.
	if got := use("plug", map[string]any{"charger": "CP-1", "soc": "30%"}); !strings.Contains(got, `"ok":true`) || !strings.Contains(got, "EV plugged in") {
		t.Errorf("plug = %s", got)
	}
	if got := use("tap", map[string]any{"charger": "CP-1", "tag": "TAG-1"}); !strings.Contains(got, `"ok":true`) {
		t.Errorf("tap = %s", got)
	}
	if got := use("wait_for_state", map[string]any{"charger": "CP-1", "state": "Charging", "timeout": "10s"}); !strings.Contains(got, `"state":"Charging","reached":true}],"reached":true`) {
		t.Errorf("wait_for_state = %s", got)
	}
	if got := use("inspect_charger", map[string]any{"charger": "CP-1"}); !strings.Contains(got, `"idTag":"TAG-1"`) {
		t.Errorf("inspect_charger does not show the transaction: %s", got)
	}
	if got := use("read_logs", map[string]any{"charger": "CP-1", "events": true}); !strings.Contains(got, "EV plugged in") || !strings.Contains(got, `"kind":"event"`) {
		t.Errorf("read_logs = %s", got)
	}
	if got := use("read_logs", map[string]any{"charger": "CP-1", "tail": 3}); !strings.Contains(got, `"message":[`) {
		t.Errorf("read_logs with messages = %s", got)
	}
	// What the command line would refuse, the tool refuses with the same
	// words.
	result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "fault", Arguments: map[string]any{"charger": "CP-1", "code": "Broken"}})
	if err != nil || !result.IsError || !strings.Contains(result.Content[0].(*mcp.TextContent).Text, "one of the OCPP 1.6 error codes") {
		t.Errorf("fault with a code OCPP does not have: %+v, %v", result, err)
	}
	use("tap", map[string]any{"charger": "CP-1", "tag": "TAG-1"})
	use("unplug", map[string]any{"charger": "CP-1"})
	if got := use("wait_for_state", map[string]any{"charger": "CP-1", "state": "Available", "timeout": "10s"}); !strings.Contains(got, `"reached":true}`) {
		t.Errorf("wait_for_state after the session = %s", got)
	}

	// The log of calls says what the agent did.
	log := mustRun(t, "api", "logs", "--charger", "CP-1")
	for _, want := range []string{" local 127.0.0.1 200 plug CP-1: EV plugged in", " 200 wait CP-1: Charging\n", " 200 logs CP-1\n", " 400 fault CP-1: fault needs \"code\"", " 200 unplug CP-1: "} {
		if !strings.Contains(log, want) {
			t.Errorf("api logs lacks %q:\n%s", want, log)
		}
	}
}
