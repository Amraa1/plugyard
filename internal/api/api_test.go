package api

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Amraa1/plugyard/internal/control"
)

// fakeBackend stands in for the chargers, and notes what it was asked.
type fakeBackend struct {
	mu    sync.Mutex
	sites []string
	execs []Action
	fail  bool
	err   error
}

func (b *fakeBackend) note(site string) {
	b.mu.Lock()
	b.sites = append(b.sites, site)
	b.mu.Unlock()
}

func (b *fakeBackend) Chargers(_ context.Context, site string) ([]control.ChargerState, error) {
	b.note(site)
	state := control.ChargerState{Site: "yard", Name: "cp-1"}
	state.ChargePointID, state.Connection = "CP-1", "online"
	return []control.ChargerState{state}, b.err
}

func (b *fakeBackend) Inspect(_ context.Context, site, charger string) (any, error) {
	b.note(site)
	if charger != "CP-1" {
		return nil, Errorf(http.StatusNotFound, "no charger matches %q", charger)
	}
	return map[string]string{"chargePointId": charger}, nil
}

func (b *fakeBackend) Exec(_ context.Context, site, charger string, action Action) ([]control.ExecResult, error) {
	b.note(site)
	b.mu.Lock()
	b.execs = append(b.execs, action)
	b.mu.Unlock()
	if b.err != nil {
		return nil, b.err
	}
	return []control.ExecResult{{ChargePointID: charger, Connector: 1, OK: !b.fail, Message: "done"}}, nil
}

// dataDir returns a data directory with a short path, as sockets need.
func dataDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "py")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

func serve(t *testing.T, cfg Config) *Server {
	t.Helper()
	if cfg.Listen == "" {
		cfg.Listen = "127.0.0.1:0"
	}
	if cfg.Backend == nil {
		cfg.Backend = &fakeBackend{}
	}
	s, err := Serve(cfg)
	if err != nil {
		t.Fatalf("Serve: %v", err)
	}
	t.Cleanup(s.Close)
	return s
}

// call makes one call and returns the status and the body.
func call(t *testing.T, s *Server, method, path, token, body string, edit ...func(*http.Request)) (int, string) {
	t.Helper()
	var reader io.Reader
	if method == http.MethodPost {
		reader = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, s.Info().URL()+path, reader)
	if err != nil {
		t.Fatal(err)
	}
	if method == http.MethodPost {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for _, e := range edit {
		e(req)
	}
	client := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}}
	defer client.CloseIdleConnections()
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, strings.TrimSpace(string(data))
}

func TestCallsFromThisMachineWithoutTokens(t *testing.T) {
	dir := dataDir(t)
	backend := &fakeBackend{}
	s := serve(t, Config{DataDir: dir, Backend: backend})

	if status, body := call(t, s, "GET", "/v1/chargers", "", ""); status != 200 || !strings.Contains(body, `"chargePointId":"CP-1"`) || !strings.HasPrefix(body, `{"chargers":[`) {
		t.Errorf("list = %d %s", status, body)
	}
	if status, body := call(t, s, "GET", "/v1/chargers/CP-1", "", ""); status != 200 || body != `{"chargePointId":"CP-1"}` {
		t.Errorf("inspect = %d %s", status, body)
	}
	if status, body := call(t, s, "GET", "/v1/chargers/CP-9", "", ""); status != 404 || body != `{"error":"no charger matches \"CP-9\""}` {
		t.Errorf("inspect of a charger that is not there = %d %s", status, body)
	}
	status, body := call(t, s, "POST", "/v1/chargers/CP-1/tap", "", `{"tag":"04A1B2C3","connector":2}`)
	if status != 200 || body != `{"results":[{"chargePointId":"CP-1","connector":1,"ok":true,"message":"done"}]}` {
		t.Errorf("tap = %d %s", status, body)
	}
	if got := backend.execs[0]; got.Name != "tap" || got.Tag != "04A1B2C3" || got.Connector == nil || *got.Connector != 2 {
		t.Errorf("the backend was asked for %+v", got)
	}
	// An action without options takes {} or nothing at all.
	for _, body := range []string{"{}", ""} {
		if status, _ := call(t, s, "POST", "/v1/chargers/CP-1/reboot", "", body); status != 200 {
			t.Errorf("reboot with body %q = %d", body, status)
		}
	}

	// What is wrong with a call is said, with a status of its own.
	for _, tc := range []struct {
		method, path, body string
		status             int
		want               string
	}{
		{"POST", "/v1/chargers/CP-1/explode", "{}", 404, `unknown action \"explode\": want one of plug, unplug, tap, fault, clear, offline, online, reboot, fail`},
		{"POST", "/v1/chargers/CP-1/tap", "{tag}", 400, "the body is not what tap takes"},
		{"POST", "/v1/chargers/CP-1/tap", `{"tagg":"x"}`, 400, "unknown field"},
		{"GET", "/v1/sites", "", 404, ""},
		{"DELETE", "/v1/chargers/CP-1", "", 405, ""},
		{"POST", "/v1/chargers", "{}", 405, ""},
	} {
		status, body := call(t, s, tc.method, tc.path, "", tc.body)
		if status != tc.status || !strings.Contains(body, tc.want) {
			t.Errorf("%s %s = %d %s, want %d and %q", tc.method, tc.path, status, body, tc.status, tc.want)
		}
	}

	// An action that fails on a charger is a conflict, with the results.
	backend.fail = true
	if status, body := call(t, s, "POST", "/v1/chargers/CP-1/plug", "", "{}"); status != 409 || !strings.Contains(body, `"ok":false`) {
		t.Errorf("an action that failed = %d %s", status, body)
	}
	backend.fail, backend.err = false, Errorf(http.StatusConflict, "charger \"CP-1\" is stopped")
	if status, body := call(t, s, "POST", "/v1/chargers/CP-1/plug", "", "{}"); status != 409 || body != `{"error":"charger \"CP-1\" is stopped"}` {
		t.Errorf("an action on a stopped charger = %d %s", status, body)
	}
	backend.err = errors.New("the disk is full")
	if status, body := call(t, s, "GET", "/v1/chargers", "", ""); status != 500 || !strings.Contains(body, "the disk is full") {
		t.Errorf("a backend that fails = %d %s", status, body)
	}
}

func TestBrowsersAreShutOut(t *testing.T) {
	s := serve(t, Config{DataDir: dataDir(t)})
	host := func(h string) func(*http.Request) { return func(r *http.Request) { r.Host = h } }
	header := func(k, v string) func(*http.Request) { return func(r *http.Request) { r.Header.Set(k, v) } }
	_, port, _ := net.SplitHostPort(s.Info().Listen)

	// A page that reached this machine under another name.
	status, body := call(t, s, "POST", "/v1/chargers/CP-1/unplug", "", "{}", host("attacker.example:"+port))
	if status != 403 || !strings.Contains(body, "listens on this machine only") {
		t.Errorf("a call naming another host = %d %s", status, body)
	}
	// A page of another site.
	status, body = call(t, s, "POST", "/v1/chargers/CP-1/unplug", "", "{}", header("Origin", "https://attacker.example"))
	if status != 403 || !strings.Contains(body, "web pages of other sites") {
		t.Errorf("a call from another origin = %d %s", status, body)
	}
	// The kinds of body a page may send across origins without asking.
	for _, kind := range []string{"text/plain", "application/x-www-form-urlencoded", ""} {
		status, _ := call(t, s, "POST", "/v1/chargers/CP-1/unplug", "", "{}", header("Content-Type", kind))
		if status != 415 {
			t.Errorf("an action with Content-Type %q = %d, want 415", kind, status)
		}
	}
	// The question a browser asks first is never answered with yes.
	status, _ = call(t, s, "OPTIONS", "/v1/chargers/CP-1/unplug", "", "", header("Origin", "http://127.0.0.1:"+port), header("Access-Control-Request-Method", "POST"))
	if status == 200 || status == 204 {
		t.Errorf("a preflight was answered with %d", status)
	}
	// Other names of this machine are fine, and so is the API's own origin.
	for _, name := range []string{"localhost:" + port, "[::1]:" + port} {
		if status, _ := call(t, s, "GET", "/v1/chargers", "", "", host(name)); status != 200 {
			t.Errorf("a call naming %s = %d", name, status)
		}
	}
	if status, _ := call(t, s, "POST", "/v1/chargers/CP-1/unplug", "", "{}", header("Origin", "http://"+s.Info().Listen)); status != 200 {
		t.Errorf("a call from the API's own origin = %d", status)
	}
}

func TestTokens(t *testing.T) {
	dir := dataDir(t)
	backend := &fakeBackend{}
	s := serve(t, Config{DataDir: dir, Backend: backend})

	full, err := CreateToken(dir, "ops", "", false)
	if err != nil {
		t.Fatal(err)
	}
	staging, _ := CreateToken(dir, "mobile-team", "staging", false)
	looker, _ := CreateToken(dir, "dashboard", "staging", true)
	if !strings.HasPrefix(full, "pyd_") || len(full) < 40 || full == staging {
		t.Errorf("tokens %q and %q", full, staging)
	}
	if _, err := CreateToken(dir, "ops", "", false); err == nil || !strings.Contains(err.Error(), "exists already") {
		t.Errorf("a second token of the same name: %v", err)
	}
	if _, err := CreateToken(dir, "no spaces", "", false); err == nil {
		t.Error("a token with a space in its name was made")
	}
	// What is kept cannot be used as a token.
	saved, _ := os.ReadFile(filepath.Join(dir, "api.tokens.json"))
	for _, secret := range []string{full, staging, looker} {
		if bytes.Contains(saved, []byte(secret)) {
			t.Error("the tokens file holds a token as it is")
		}
	}

	// Once a token exists, every call needs one, also from this machine.
	for _, token := range []string{"", "pyd_wrong", strings.ToUpper(full)} {
		status, body := call(t, s, "GET", "/v1/chargers", token, "")
		if status != 401 || !strings.Contains(body, "a token is needed") {
			t.Errorf("a call with token %q = %d %s", token, status, body)
		}
	}
	if status, _ := call(t, s, "GET", "/v1/chargers", full, "", func(r *http.Request) { r.Header.Set("Authorization", "Basic "+full) }); status != 401 {
		t.Errorf("a token sent as Basic = %d", status)
	}
	// The description needs none.
	if status, body := call(t, s, "GET", "/v1/openapi.json", "", ""); status != 200 || !strings.Contains(body, `"openapi"`) {
		t.Errorf("the description without a token = %d", status)
	}

	site := func() string {
		backend.mu.Lock()
		defer backend.mu.Unlock()
		return backend.sites[len(backend.sites)-1]
	}
	// A token for every site sees every site, or the one it names.
	if status, _ := call(t, s, "GET", "/v1/chargers", full, ""); status != 200 || site() != "" {
		t.Errorf("list with a token for every site = %d, site %q", status, site())
	}
	if status, _ := call(t, s, "GET", "/v1/chargers?site=yard", full, ""); status != 200 || site() != "yard" {
		t.Errorf("list of a named site = %d, site %q", status, site())
	}
	// A token for one site is held to it.
	if status, _ := call(t, s, "POST", "/v1/chargers/CP-1/plug", staging, "{}"); status != 200 || site() != "staging" {
		t.Errorf("an action with a token of site staging = %d, site %q", status, site())
	}
	if status, body := call(t, s, "GET", "/v1/chargers?site=yard", staging, ""); status != 403 || !strings.Contains(body, "token mobile-team is limited to site staging") {
		t.Errorf("a token of site staging asking for site yard = %d %s", status, body)
	}
	// What it cannot reach is forbidden to it, whether it is in another
	// site or nowhere: the answer does not tell the two apart. A token
	// for every site is told that there is no such charger.
	if status, body := call(t, s, "GET", "/v1/chargers/CP-9", staging, ""); status != 403 || body != `{"error":"token mobile-team is limited to site staging: no charger matches \"CP-9\""}` {
		t.Errorf("a token of site staging asking for a charger it cannot reach = %d %s", status, body)
	}
	if status, _ := call(t, s, "GET", "/v1/chargers/CP-9", full, ""); status != 404 {
		t.Errorf("a token for every site asking for a charger that is not there = %d", status)
	}
	// A read-only token looks and does not act.
	if status, _ := call(t, s, "GET", "/v1/chargers/CP-1", looker, ""); status != 200 {
		t.Errorf("inspect with a read-only token = %d", status)
	}
	before := len(backend.execs)
	if status, body := call(t, s, "POST", "/v1/chargers/CP-1/unplug", looker, "{}"); status != 403 || !strings.Contains(body, "token dashboard is read-only") {
		t.Errorf("an action with a read-only token = %d %s", status, body)
	}
	if len(backend.execs) != before {
		t.Error("an action with a read-only token reached the chargers")
	}

	// A token that is removed stops working at once.
	if found, err := RemoveToken(dir, "ops"); !found || err != nil {
		t.Fatalf("RemoveToken = %v, %v", found, err)
	}
	if found, _ := RemoveToken(dir, "ops"); found {
		t.Error("a token was removed twice")
	}
	if status, _ := call(t, s, "GET", "/v1/chargers", full, ""); status != 401 {
		t.Errorf("a call with a removed token = %d", status)
	}

	// The log names who called, and never holds a token.
	entries, err := ReadLog(dir)
	if err != nil {
		t.Fatal(err)
	}
	var plug *Entry
	names := map[string]bool{}
	for i, e := range entries {
		names[e.Token] = true
		if e.Action == "plug" {
			plug = &entries[i]
		}
	}
	if !names["ops"] || !names["mobile-team"] || !names["dashboard"] || !names["-"] {
		t.Errorf("the log names the callers %v", names)
	}
	if plug == nil || plug.Token != "mobile-team" || plug.Status != 200 || strings.Join(plug.Chargers, ",") != "CP-1" || plug.Remote != "127.0.0.1" {
		t.Errorf("the entry of the plug = %+v", plug)
	}
	if line := plug.Line(); !strings.Contains(line, " mobile-team 127.0.0.1 200 plug CP-1: done") {
		t.Errorf("the line of the plug = %q", line)
	}
	logged, _ := os.ReadFile(filepath.Join(dir, "api.log"))
	for _, secret := range []string{full, staging, looker} {
		if bytes.Contains(logged, []byte(secret)) {
			t.Error("the log holds a token")
		}
	}
	if !bytes.Contains(logged, []byte(`"status":401`)) || !bytes.Contains(logged, []byte(`"status":403`)) {
		t.Error("the log leaves out calls that were turned away")
	}
}

func TestListeningBeyondThisMachineNeedsAToken(t *testing.T) {
	for listen, local := range map[string]bool{
		"127.0.0.1:7070": true, "localhost:7070": true, "[::1]:7070": true,
		"0.0.0.0:7070": false, ":7070": false, "192.0.2.4:7070": false, "[::]:7070": false, "nonsense": false,
	} {
		if got := loopback(listen); got != local {
			t.Errorf("loopback(%q) = %v, want %v", listen, got, local)
		}
	}

	dir := dataDir(t)
	_, err := Serve(Config{DataDir: dir, Listen: "0.0.0.0:0", Backend: &fakeBackend{}})
	if err == nil || !strings.Contains(err.Error(), "no token exists") || !strings.Contains(err.Error(), "plugyard api token create") {
		t.Fatalf("Serve beyond this machine without a token = %v", err)
	}
	if _, err := Probe(context.Background(), dir); err == nil {
		t.Fatal("an API that was refused is running")
	}

	if runtime.GOOS == "windows" {
		t.Skip("listening on every address brings up a firewall question on Windows")
	}
	token, _ := CreateToken(dir, "ops", "", false)
	s := serve(t, Config{DataDir: dir, Listen: "0.0.0.0:0"})
	if s.Local() {
		t.Fatal("an API on every address counts as local")
	}
	_, port, _ := net.SplitHostPort(s.Info().Listen)
	s.info.Listen = "127.0.0.1:" + port
	// Any host name is fine here: a reverse proxy passes its own on.
	if status, _ := call(t, s, "GET", "/v1/chargers", token, "", func(r *http.Request) { r.Host = "api.example.com" }); status != 200 {
		t.Errorf("a call with a token = %d", status)
	}
	// When the last token goes, nobody gets in.
	RemoveToken(dir, "ops")
	if status, _ := call(t, s, "GET", "/v1/chargers", token, ""); status != 401 {
		t.Errorf("a call after the last token was removed = %d", status)
	}
	if status, _ := call(t, s, "GET", "/v1/chargers", "", ""); status != 401 {
		t.Errorf("a call without a token = %d", status)
	}
}

func TestOnlyOneAPIAndStoppingIt(t *testing.T) {
	dir := dataDir(t)
	if _, err := Probe(context.Background(), dir); err == nil {
		t.Fatal("an API answers before one was started")
	}
	s := serve(t, Config{DataDir: dir, Version: "v9.9.9"})

	info, err := Probe(context.Background(), dir)
	if err != nil || info.Listen != s.Info().Listen || info.PID != os.Getpid() || info.Version != "v9.9.9" || info.TLS {
		t.Fatalf("Probe = %+v, %v", info, err)
	}
	_, err = Serve(Config{DataDir: dir, Listen: "127.0.0.1:0", Backend: &fakeBackend{}})
	if !errors.Is(err, ErrAlreadyRunning) || !strings.Contains(err.Error(), info.URL()) {
		t.Fatalf("a second API = %v", err)
	}

	// The socket is how it is stopped; the network offers no such thing.
	if status, _ := call(t, s, "POST", "/shutdown", "", "{}"); status != 404 {
		t.Errorf("POST /shutdown over the network = %d", status)
	}
	stopped := make(chan error, 1)
	go func() { stopped <- Shutdown(context.Background(), dir) }()
	select {
	case <-s.Stopped():
		s.Close()
	case <-time.After(5 * time.Second):
		t.Fatal("the API was not asked to stop")
	}
	if err := <-stopped; err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if _, err := Probe(context.Background(), dir); err == nil {
		t.Error("the API still answers after it was stopped")
	}
}

func TestServingHTTPS(t *testing.T) {
	dir := dataDir(t)
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "plugyard test"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, _ := x509.MarshalECPrivateKey(key)
	certFile, keyFile := filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
	os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600)
	os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600)

	if _, err := Serve(Config{DataDir: dir, Listen: "127.0.0.1:0", TLSCert: certFile, Backend: &fakeBackend{}}); err == nil {
		t.Fatal("a certificate without its key was taken")
	}
	s := serve(t, Config{DataDir: dir, TLSCert: certFile, TLSKey: keyFile})
	if !s.Info().TLS || !strings.HasPrefix(s.Info().URL(), "https://") {
		t.Fatalf("Info = %+v", s.Info())
	}
	if status, _ := call(t, s, "GET", "/v1/chargers", "", ""); status != 200 {
		t.Errorf("a call over HTTPS = %d", status)
	}
}

// The description says what the server does, and no more.
func TestTheDescriptionMatchesTheServer(t *testing.T) {
	var doc struct {
		Paths map[string]map[string]struct {
			Parameters []struct {
				Name   string `json:"name"`
				Schema struct {
					Enum []string `json:"enum"`
				} `json:"schema"`
			} `json:"parameters"`
		} `json:"paths"`
		Components struct {
			Schemas map[string]struct {
				Properties map[string]json.RawMessage `json:"properties"`
			} `json:"schemas"`
		} `json:"components"`
	}
	if err := json.Unmarshal(Spec, &doc); err != nil {
		t.Fatalf("the description is not JSON: %v", err)
	}

	var described []string
	for path, methods := range doc.Paths {
		for method := range methods {
			described = append(described, strings.ToUpper(method)+" "+path)
		}
	}
	sort.Strings(described)
	want := []string{"GET /v1/chargers", "GET /v1/chargers/{charger}", "GET /v1/openapi.json", "POST /v1/chargers/{charger}/{action}"}
	if strings.Join(described, "; ") != strings.Join(want, "; ") {
		t.Errorf("the description has %v, want %v", described, want)
	}
	// Each of them is answered by the server, not with "no such address".
	s := serve(t, Config{DataDir: dataDir(t)})
	for _, route := range want {
		method, path, _ := strings.Cut(route, " ")
		path = strings.NewReplacer("{charger}", "CP-1", "{action}", "reboot").Replace(path)
		if status, _ := call(t, s, method, path, "", "{}"); status != 200 {
			t.Errorf("%s %s = %d", method, path, status)
		}
	}

	// The actions and their options are those of the server.
	for _, p := range doc.Paths["/v1/chargers/{charger}/{action}"]["post"].Parameters {
		if p.Name == "action" && strings.Join(p.Schema.Enum, ",") != strings.Join(Actions, ",") {
			t.Errorf("the description lists the actions %v, the server has %v", p.Schema.Enum, Actions)
		}
	}
	var options, fields []string
	for name := range doc.Components.Schemas["Options"].Properties {
		options = append(options, name)
	}
	kind := reflect.TypeOf(Action{})
	for i := 0; i < kind.NumField(); i++ {
		if name, _, _ := strings.Cut(kind.Field(i).Tag.Get("json"), ","); name != "-" {
			fields = append(fields, name)
		}
	}
	sort.Strings(options)
	sort.Strings(fields)
	if strings.Join(options, ",") != strings.Join(fields, ",") {
		t.Errorf("the description has the options %v, the server takes %v", options, fields)
	}
}
