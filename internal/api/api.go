// Package api serves plugyard's HTTP API: the charger actions of the
// command line, and reading chargers, for callers that have no shell on the
// machine (see docs/adr/0006).
//
// What the API offers is kept narrow on purpose. It acts on chargers and
// reads them. It never takes a file path, starts or removes a site or a
// charger, changes where a charger connects to, updates or removes
// plugyard, or manages its own tokens: those stay on the command line.
package api

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/Amraa1/plugyard/internal/control"
	"github.com/Amraa1/plugyard/internal/datadir"
)

// DefaultListen is where the API listens when nothing else is said: on
// this machine only.
const DefaultListen = "127.0.0.1:7070"

// Actions lists what the API can do to a charger. They are the actions of
// "plugyard exec".
var Actions = []string{
	control.ActionPlug, control.ActionUnplug, control.ActionTap, control.ActionFault, control.ActionClear,
	control.ActionOffline, control.ActionOnline, control.ActionReboot, control.ActionFail,
}

// Spec is the description of the API as an OpenAPI document.
//
//go:embed openapi.json
var Spec []byte

// Action is an action on chargers with what goes with it, as the body of a
// request carries it. Which fields apply depends on the action.
type Action struct {
	// Name is the action, taken from the address of the request.
	Name string `json:"-"`
	// Connector is the connector the action is about. Nil means the
	// default: connector 1.
	Connector *int `json:"connector,omitempty"`

	Tag        string `json:"tag,omitempty"`        // tap
	Code       string `json:"code,omitempty"`       // fault
	Info       string `json:"info,omitempty"`       // fault
	VendorCode string `json:"vendorCode,omitempty"` // fault
	For        string `json:"for,omitempty"`        // offline
	Step       string `json:"step,omitempty"`       // fail

	EV        string `json:"ev,omitempty"` // plug: an EV preset of the site
	Battery   string `json:"battery,omitempty"`
	SoC       string `json:"soc,omitempty"`
	MaxPower  string `json:"maxPower,omitempty"`
	TargetSoC string `json:"targetSoc,omitempty"`
	Curve     string `json:"curve,omitempty"`
}

// Wait says what to wait for: a state of chargers, as "plugyard wait"
// takes it.
type Wait struct {
	// State is a connector status or a connection state, in any case.
	State string
	// Connector is the connector a status is that of. Nil means the
	// default: connector 1.
	Connector *int
	// Timeout is how long to wait at most.
	Timeout time.Duration
}

// Waited says where one charger was when a wait ended.
type Waited struct {
	Site          string `json:"site"`
	ChargePointID string `json:"chargePointId"`
	// Connector is the connector the state is that of. It is absent for
	// a state of the whole charger.
	Connector int `json:"connector,omitempty"`
	// State is the state the charger or its connector was in.
	State string `json:"state"`
	// Reached says whether that is the state waited for.
	Reached bool `json:"reached"`
}

// The limits of a wait and of reading the log, so that one call neither
// holds a connection for ever nor answers with a whole log.
const (
	DefaultWaitTimeout = 30 * time.Second
	MaxWaitTimeout     = 5 * time.Minute
	DefaultLogTail     = 200
	MaxLogTail         = 5000
)

// LogQuery says which lines of the log to read, as the flags of "plugyard
// logs" do.
type LogQuery struct {
	// Since is how long ago, such as 10m, or a time. Empty means from
	// the start of the log.
	Since string
	// Tail is how many lines to answer with at most: the newest.
	Tail int
	// Events leaves the OCPP messages out.
	Events bool
}

// LogLine is one line of the log of a charger, as "plugyard logs --format
// json" prints it.
type LogLine struct {
	Time          string `json:"time"`
	ChargePointID string `json:"chargePointId"`
	// Kind is "event", "sent" or "received".
	Kind string `json:"kind"`
	Text string `json:"text"`
	// Message is the OCPP message of a line that is one.
	Message json.RawMessage `json:"message,omitempty"`
}

// Backend is what the API needs done. The command line implements it, so
// that a call to the API does what the command of the same name does.
type Backend interface {
	// Chargers lists the chargers of a site, or of every site when site
	// is empty.
	Chargers(ctx context.Context, site string) ([]control.ChargerState, error)
	// Inspect returns everything about one charger, as "plugyard inspect
	// --format json" prints it.
	Inspect(ctx context.Context, site, charger string) (any, error)
	// Exec runs an action on the chargers a charger argument names.
	Exec(ctx context.Context, site, charger string, action Action) ([]control.ExecResult, error)
	// Wait waits until the chargers a charger argument names are in a
	// state, and says where each was when that was so or the time was
	// up.
	Wait(ctx context.Context, site, charger string, wait Wait) ([]Waited, error)
	// Logs returns lines of the log of the chargers a charger argument
	// names, oldest first.
	Logs(ctx context.Context, site, charger string, query LogQuery) ([]LogLine, error)
}

// Error is a failure the caller is told about with an HTTP status of its
// own. Any other error of a Backend is answered with 500.
type Error struct {
	Status  int
	Message string
}

func (e *Error) Error() string { return e.Message }

// Errorf makes an Error.
func Errorf(status int, format string, args ...any) *Error {
	return &Error{Status: status, Message: fmt.Sprintf(format, args...)}
}

// Config says how to serve the API.
type Config struct {
	// DataDir is plugyard's data directory, where the tokens, the log of
	// calls and the socket of the API are kept.
	DataDir string
	// Listen is the address to listen on, such as 127.0.0.1:7070.
	Listen string
	// TLSCert and TLSKey, when both are set, are the files of a
	// certificate and its key, and the API is served over HTTPS.
	TLSCert, TLSKey string
	Backend         Backend
	// Version is the version of plugyard, shown by Info.
	Version string
	// Out, if set, is shown every call as it comes in.
	Out io.Writer
}

// Info says what a running API is.
type Info struct {
	PID    int    `json:"pid"`
	Listen string `json:"listen"`
	TLS    bool   `json:"tls"`
	// TLSCert and TLSKey are the files HTTPS is served with, as full
	// paths. They are told over the socket of this machine only.
	TLSCert string    `json:"tlsCert,omitempty"`
	TLSKey  string    `json:"tlsKey,omitempty"`
	Since   time.Time `json:"since"`
	Version string    `json:"version"`
}

// URL returns the address of the API as a caller writes it.
func (i Info) URL() string {
	scheme := "http"
	if i.TLS {
		scheme = "https"
	}
	return scheme + "://" + i.Listen
}

// ErrAlreadyRunning is returned by Serve when an API already runs on this
// machine for this user.
var ErrAlreadyRunning = errors.New("the API is already running")

// loopback reports whether a listen address is reachable from this machine
// only.
func loopback(listen string) bool {
	host, _, err := net.SplitHostPort(listen)
	if err != nil {
		return false
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	addr, err := netip.ParseAddr(host)
	return err == nil && addr.IsLoopback()
}

// Server is a running API.
type Server struct {
	cfg      Config
	info     Info
	local    bool
	audit    *audit
	http     *http.Server
	admin    *http.Server
	sockPath string
	done     chan struct{}
	stop     chan struct{}
	// calls is done, and closing makes it so, when the API stops: it
	// ends the calls that are still waiting.
	calls   context.Context
	closing context.CancelFunc
}

func socketPath(dataDir string) string { return filepath.Join(dataDir, "api.sock") }

// maxSocketPath is the longest socket path that works on every supported
// system.
const maxSocketPath = 103

// Serve starts the API. It refuses to listen beyond this machine while no
// token exists, since anyone who can reach it could then act on chargers.
func Serve(cfg Config) (*Server, error) {
	if cfg.Listen == "" {
		cfg.Listen = DefaultListen
	}
	if (cfg.TLSCert == "") != (cfg.TLSKey == "") {
		return nil, errors.New("a certificate and its key go together: give both or neither")
	}
	local := loopback(cfg.Listen)
	if !local {
		tokens, err := LoadTokens(cfg.DataDir)
		if err != nil {
			return nil, err
		}
		if len(tokens) == 0 {
			return nil, fmt.Errorf("%s can be reached from other machines, and no token exists, so anyone who reaches it could act on the chargers. "+
				"Make a token first with \"plugyard api token create <name>\", or listen on %s", cfg.Listen, DefaultListen)
		}
	}

	if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
		return nil, err
	}
	path := socketPath(cfg.DataDir)
	if running, err := Probe(context.Background(), cfg.DataDir); err == nil {
		return nil, fmt.Errorf("%w, at %s (process %d)", ErrAlreadyRunning, running.URL(), running.PID)
	}
	_ = os.Remove(path)
	if len(path) > maxSocketPath {
		return nil, fmt.Errorf("the data directory path %s is too long for a socket (%d characters, limit %d); set %s to a shorter path",
			cfg.DataDir, len(path), maxSocketPath, datadir.Env)
	}

	listener, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		return nil, fmt.Errorf("cannot listen on %s: %w", cfg.Listen, err)
	}
	// The address as it came out, for a port left to the system to pick.
	cfg.Listen = listener.Addr().String()
	socket, err := net.Listen("unix", path)
	if err != nil {
		listener.Close()
		return nil, fmt.Errorf("cannot open the socket %s: %w", path, err)
	}
	_ = os.Chmod(path, 0o600)

	log, err := openAudit(cfg.DataDir, cfg.Out)
	if err != nil {
		listener.Close()
		socket.Close()
		return nil, err
	}

	calls, closing := context.WithCancel(context.Background())
	s := &Server{
		calls:    calls,
		closing:  closing,
		cfg:      cfg,
		local:    local,
		audit:    log,
		sockPath: path,
		done:     make(chan struct{}),
		stop:     make(chan struct{}, 1),
		info: Info{
			PID:     os.Getpid(),
			Listen:  cfg.Listen,
			TLS:     cfg.TLSCert != "",
			TLSCert: absolute(cfg.TLSCert),
			TLSKey:  absolute(cfg.TLSKey),
			Since:   time.Now().UTC().Truncate(time.Second),
			Version: cfg.Version,
		},
	}

	routes := http.NewServeMux()
	routes.HandleFunc("GET /v1/openapi.json", s.handleSpec)
	routes.HandleFunc("GET /v1/chargers", s.handleList)
	routes.HandleFunc("GET /v1/chargers/{charger}", s.handleInspect)
	routes.HandleFunc("GET /v1/chargers/{charger}/wait", s.handleWait)
	routes.HandleFunc("GET /v1/chargers/{charger}/logs", s.handleLogs)
	routes.HandleFunc("POST /v1/chargers/{charger}/{action}", s.handleAction)
	routes.Handle(MCPPath, s.mcpHandler())
	s.http = &http.Server{
		Handler:           s.guard(routes),
		BaseContext:       func(net.Listener) context.Context { return calls },
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      2 * time.Minute,
		IdleTimeout:       2 * time.Minute,
		MaxHeaderBytes:    64 * 1024,
	}

	// The socket is how the commands of this machine reach the API: to
	// ask what it is, and to stop it. It is never offered over the
	// network.
	admin := http.NewServeMux()
	admin.HandleFunc("GET /info", func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, http.StatusOK, s.info) })
	admin.HandleFunc("POST /shutdown", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, struct{}{})
		select {
		case s.stop <- struct{}{}:
		default:
		}
	})
	s.admin = &http.Server{Handler: admin, ReadHeaderTimeout: 10 * time.Second}

	go func() { _ = s.admin.Serve(socket) }()
	go func() {
		defer close(s.done)
		var served error
		if cfg.TLSCert != "" {
			served = s.http.ServeTLS(listener, cfg.TLSCert, cfg.TLSKey)
		} else {
			served = s.http.Serve(listener)
		}
		if served != nil && !errors.Is(served, http.ErrServerClosed) && cfg.Out != nil {
			fmt.Fprintf(cfg.Out, "the API stopped: %v\n", served)
		}
	}()
	return s, nil
}

// absolute returns a path that holds from any folder.
func absolute(path string) string {
	if path == "" {
		return ""
	}
	if abs, err := filepath.Abs(path); err == nil {
		return abs
	}
	return path
}

// Info says what this API is.
func (s *Server) Info() Info { return s.info }

// Local reports whether the API can be reached from this machine only.
func (s *Server) Local() bool { return s.local }

// Stopped is closed when "plugyard api down" has asked the API to stop.
func (s *Server) Stopped() <-chan struct{} { return s.stop }

// Done is closed when the API no longer serves, whatever the reason.
func (s *Server) Done() <-chan struct{} { return s.done }

// Close stops the API.
func (s *Server) Close() {
	s.closing()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.http.Shutdown(ctx); err != nil {
		_ = s.http.Close()
	}
	_ = s.admin.Close()
	_ = os.Remove(s.sockPath)
	s.audit.close()
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// caller is what the guard found out about who is calling.
type caller struct {
	// name is the token's name, or "local".
	name     string
	site     string
	readOnly bool
	// remote is the address the call came from.
	remote string
}

type callerKey struct{}

// recorder notes how a call was answered, for the log.
type recorder struct {
	http.ResponseWriter
	entry *Entry
	// refused says that the call was turned away here, with fail.
	refused bool
}

func (r *recorder) WriteHeader(status int) {
	r.entry.Status = status
	r.ResponseWriter.WriteHeader(status)
}

// Unwrap lets a route that answers late move the time limits of its
// connection.
func (r *recorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

func (r *recorder) fail(status int, format string, args ...any) {
	r.refused = true
	r.entry.Message = fmt.Sprintf(format, args...)
	writeJSON(r, status, map[string]string{"error": r.entry.Message})
}

// guard decides whether a call may go on to the routes, and records every
// call, also the ones it turns away.
func (s *Server) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		remote, _, _ := net.SplitHostPort(r.RemoteAddr)
		entry := &Entry{Time: time.Now().UTC(), Token: "-", Remote: remote, Method: r.Method, Path: r.URL.Path, Status: http.StatusOK}
		rec := &recorder{ResponseWriter: w, entry: entry}
		// An agent's requests to the tools are mostly the protocol
		// talking to itself. The log records each call of a tool
		// instead, which says who did what to which charger, and the
		// requests that were turned away.
		tools := r.URL.Path == MCPPath
		defer func() {
			if !tools || rec.refused {
				s.audit.record(*entry)
			}
		}()
		// What the call is about is noted first, so that the log says it
		// also for a call that is turned away.
		switch parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/"); {
		case len(parts) < 2 || parts[0] != "v1" || parts[1] != "chargers":
		case len(parts) == 2 && r.Method == http.MethodGet:
			entry.Action = "list"
		case len(parts) == 3 && r.Method == http.MethodGet:
			entry.Action, entry.Chargers = "inspect", []string{parts[2]}
		case len(parts) == 4 && (r.Method == http.MethodPost || parts[3] == "wait" || parts[3] == "logs"):
			entry.Action, entry.Chargers = parts[3], []string{parts[2]}
		}

		// A web page open in a browser can send requests to this
		// machine. Three things keep it from acting on chargers: a call
		// must name this address as its host, which a page that reached
		// it through another name does not; it must not come from a
		// page of another origin; and an action must say its body is
		// JSON, which a page may not do across origins without asking
		// first, and that question is never answered with yes.
		if s.local && !loopbackHost(r.Host) {
			rec.fail(http.StatusForbidden, "the API listens on this machine only, and this call names %q as its host", r.Host)
			return
		}
		if origin := r.Header.Get("Origin"); origin != "" && !sameOrigin(origin, r.Host) {
			rec.fail(http.StatusForbidden, "calls from web pages of other sites are not allowed")
			return
		}

		// The description of the API is the same for everyone.
		if r.Method == http.MethodGet && r.URL.Path == "/v1/openapi.json" {
			next.ServeHTTP(rec, r)
			return
		}

		tokens, err := LoadTokens(s.cfg.DataDir)
		if err != nil {
			rec.fail(http.StatusInternalServerError, "the tokens cannot be read: %v", err)
			return
		}
		who := caller{name: "local"}
		// Without any token, calls from this machine need none. Once a
		// token exists, every call needs one.
		if !s.local || len(tokens) > 0 {
			secret, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
			token, found := findToken(tokens, strings.TrimSpace(secret))
			if !ok || !found {
				rec.Header().Set("WWW-Authenticate", `Bearer realm="plugyard"`)
				rec.fail(http.StatusUnauthorized, "a token is needed: send it as \"Authorization: Bearer <token>\"")
				return
			}
			who = caller{name: token.Name, site: token.Site, readOnly: token.ReadOnly}
		}
		who.remote = remote
		entry.Token = who.name

		if tools {
			// A tool may wait as long as a wait under /v1 does.
			limits := http.NewResponseController(rec)
			late := time.Now().Add(MaxWaitTimeout + 30*time.Second)
			_ = limits.SetReadDeadline(late)
			_ = limits.SetWriteDeadline(late)
		}
		if r.Method == http.MethodPost {
			// Every request to the tools is a POST, also the ones that
			// read: there a read-only token is offered no tool that
			// acts.
			if who.readOnly && !tools {
				rec.fail(http.StatusForbidden, "token %s is read-only: it can look at chargers and not act on them", who.name)
				return
			}
			if kind, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type")); kind != "application/json" {
				rec.fail(http.StatusUnsupportedMediaType, "send the body as JSON, with \"Content-Type: application/json\"; an action without options takes {}")
				return
			}
			r.Body = http.MaxBytesReader(rec, r.Body, 64*1024)
		}
		next.ServeHTTP(rec, r.WithContext(context.WithValue(r.Context(), callerKey{}, who)))
	})
}

// loopbackHost reports whether the host a call names is this machine.
func loopbackHost(host string) bool {
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.Trim(host, "[]")
	if strings.EqualFold(host, "localhost") {
		return true
	}
	addr, err := netip.ParseAddr(host)
	return err == nil && addr.IsLoopback()
}

// sameOrigin reports whether a web page's origin is the API itself.
func sameOrigin(origin, host string) bool {
	_, rest, ok := strings.Cut(origin, "://")
	return ok && strings.EqualFold(rest, host)
}

// site works out which site a call is about: the one its token is limited
// to, or the one it names.
func (s *Server) site(rec *recorder, r *http.Request) (string, bool) {
	who := r.Context().Value(callerKey{}).(caller)
	named := r.URL.Query().Get("site")
	if who.site != "" && named != "" && named != who.site {
		rec.fail(http.StatusForbidden, "token %s is limited to site %s", who.name, who.site)
		return "", false
	}
	if who.site != "" {
		return who.site, true
	}
	return named, true
}

// answer writes what a Backend failed with.
func answer(rec *recorder, r *http.Request, err error) {
	status, message := explain(r.Context().Value(callerKey{}).(caller), err)
	rec.fail(status, "%s", message)
}

// explain says with which status and which words a caller is told what a
// Backend failed with.
func explain(who caller, err error) (int, string) {
	var known *Error
	if !errors.As(err, &known) {
		return http.StatusInternalServerError, err.Error()
	}
	// A token that is limited to a site is told so when it asks for a
	// charger it cannot reach, whether that charger is in another site
	// or nowhere at all. The answer is the same for both, so it says
	// nothing about what other sites hold.
	if who.site != "" && known.Status == http.StatusNotFound {
		return http.StatusForbidden, fmt.Sprintf("token %s is limited to site %s: %s", who.name, who.site, known.Message)
	}
	return known.Status, known.Message
}

func (s *Server) handleSpec(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(Spec)
}

func (s *Server) handleList(w http.ResponseWriter, r *http.Request) {
	rec := w.(*recorder)
	rec.entry.Action = "list"
	site, ok := s.site(rec, r)
	if !ok {
		return
	}
	chargers, err := s.cfg.Backend.Chargers(r.Context(), site)
	if err != nil {
		answer(rec, r, err)
		return
	}
	if chargers == nil {
		chargers = []control.ChargerState{}
	}
	writeJSON(rec, http.StatusOK, map[string]any{"chargers": chargers})
}

func (s *Server) handleInspect(w http.ResponseWriter, r *http.Request) {
	rec := w.(*recorder)
	rec.entry.Action = "inspect"
	rec.entry.Chargers = []string{r.PathValue("charger")}
	site, ok := s.site(rec, r)
	if !ok {
		return
	}
	view, err := s.cfg.Backend.Inspect(r.Context(), site, r.PathValue("charger"))
	if err != nil {
		answer(rec, r, err)
		return
	}
	writeJSON(rec, http.StatusOK, view)
}

func (s *Server) handleAction(w http.ResponseWriter, r *http.Request) {
	rec := w.(*recorder)
	name := r.PathValue("action")
	rec.entry.Action = name
	rec.entry.Chargers = []string{r.PathValue("charger")}

	known := false
	for _, a := range Actions {
		known = known || a == name
	}
	if !known {
		rec.fail(http.StatusNotFound, "unknown action %q: want one of %s", name, strings.Join(Actions, ", "))
		return
	}
	site, ok := s.site(rec, r)
	if !ok {
		return
	}

	action := Action{}
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&action); err != nil && !errors.Is(err, io.EOF) {
		rec.fail(http.StatusBadRequest, "the body is not what %s takes: %v", name, err)
		return
	}
	action.Name = name

	results, err := s.cfg.Backend.Exec(r.Context(), site, r.PathValue("charger"), action)
	if err != nil {
		answer(rec, r, err)
		return
	}
	writeJSON(rec, executed(rec.entry, results), map[string]any{"results": results})
}

// executed notes in the record of a call how an action ended, and returns
// the status that says so.
func executed(entry *Entry, results []control.ExecResult) int {
	// The log names the chargers the action ran on, which a pattern
	// does not.
	entry.Chargers = nil
	status, failed := http.StatusOK, 0
	for _, result := range results {
		entry.Chargers = append(entry.Chargers, result.ChargePointID)
		if !result.OK {
			failed++
			entry.Message = result.Message
		}
	}
	if failed > 0 {
		// The action could not be carried out on every charger.
		status = http.StatusConflict
		if len(results) > 1 {
			entry.Message = fmt.Sprintf("failed on %d of %d chargers", failed, len(results))
		}
	} else if len(results) == 1 {
		entry.Message = results[0].Message
	}
	return status
}

// What waiting and reading the log take in the address of a call, beside
// the site.
var (
	waitTakes = []string{"state", "connector", "timeout"}
	logsTakes = []string{"since", "tail", "events"}
)

// query reads the parameters of a call's address. One the call does not
// take is a mistake, as an option an action does not take is: a misspelt
// "timeout" would otherwise be left out without a word.
func query(rec *recorder, r *http.Request, takes ...string) (map[string]string, bool) {
	given := map[string]string{}
	for name, values := range r.URL.Query() {
		known := name == "site"
		for _, t := range takes {
			known = known || name == t
		}
		if !known {
			rec.fail(http.StatusBadRequest, "%s does not take %q: it takes %s", rec.entry.Action, name, strings.Join(slices.Concat(takes, []string{"site"}), ", "))
			return nil, false
		}
		given[name] = values[0]
	}
	return given, true
}

func (s *Server) handleWait(w http.ResponseWriter, r *http.Request) {
	rec := w.(*recorder)
	rec.entry.Action = "wait"
	rec.entry.Chargers = []string{r.PathValue("charger")}
	given, ok := query(rec, r, waitTakes...)
	if !ok {
		return
	}
	site, ok := s.site(rec, r)
	if !ok {
		return
	}

	var connector *int
	if text, ok := given["connector"]; ok {
		n, err := strconv.Atoi(text)
		if err != nil {
			n = -1
		}
		connector = &n
	}
	wait, err := newWait(given["state"], connector, given["timeout"])
	if err != nil {
		answer(rec, r, err)
		return
	}
	// The answer comes when the wait is over, which is later than the
	// server gives a call otherwise.
	limits := http.NewResponseController(rec)
	late := time.Now().Add(wait.Timeout + 30*time.Second)
	_ = limits.SetReadDeadline(late)
	_ = limits.SetWriteDeadline(late)

	chargers, err := s.cfg.Backend.Wait(r.Context(), site, r.PathValue("charger"), wait)
	if err != nil && r.Context().Err() != nil {
		err = errWaitEnded
	}
	if err != nil {
		answer(rec, r, err)
		return
	}
	// A state that did not come in time is an answer and not a failure
	// of the call: the caller asked whether it would.
	writeJSON(rec, http.StatusOK, waited(rec.entry, wait, chargers))
}

var errWaitEnded = Errorf(http.StatusServiceUnavailable, "the wait ended early: the API is stopping, or the caller went away")

// newWait checks what a wait was asked with. An empty timeout stands for
// the default.
func newWait(state string, connector *int, timeout string) (Wait, error) {
	wait := Wait{State: state, Connector: connector, Timeout: DefaultWaitTimeout}
	if state == "" {
		return wait, Errorf(http.StatusBadRequest, "wait needs \"state\", the state to wait for, such as Charging")
	}
	if connector != nil && *connector < 1 {
		return wait, Errorf(http.StatusBadRequest, "the connector number must be 1 or more")
	}
	if timeout != "" {
		d, err := time.ParseDuration(timeout)
		if err != nil || d <= 0 || d > MaxWaitTimeout {
			return wait, Errorf(http.StatusBadRequest, "timeout must be a time above zero and at most %s, such as 30s; to wait longer, call again", MaxWaitTimeout)
		}
		wait.Timeout = d
	}
	return wait, nil
}

// waited notes in the record of a call how a wait ended, and returns what
// the caller is answered with.
func waited(entry *Entry, wait Wait, chargers []Waited) map[string]any {
	entry.Chargers = nil
	reached := true
	for _, c := range chargers {
		entry.Chargers = append(entry.Chargers, c.ChargePointID)
		reached = reached && c.Reached
	}
	if reached {
		entry.Message = wait.State
	} else {
		entry.Message = fmt.Sprintf("not %s after %s", wait.State, wait.Timeout)
	}
	if chargers == nil {
		chargers = []Waited{}
	}
	return map[string]any{"reached": reached, "chargers": chargers}
}

// logTail checks how many lines of the log were asked for.
func logTail(n int) (int, error) {
	if n < 0 || n > MaxLogTail {
		return 0, Errorf(http.StatusBadRequest, "tail must be a number from 0 to %d; for older lines, narrow the call with \"since\" or \"events\"", MaxLogTail)
	}
	return n, nil
}

func (s *Server) handleLogs(w http.ResponseWriter, r *http.Request) {
	rec := w.(*recorder)
	rec.entry.Action = "logs"
	rec.entry.Chargers = []string{r.PathValue("charger")}
	given, ok := query(rec, r, logsTakes...)
	if !ok {
		return
	}
	site, ok := s.site(rec, r)
	if !ok {
		return
	}

	q := LogQuery{Since: given["since"], Tail: DefaultLogTail}
	if text, ok := given["tail"]; ok {
		n, err := strconv.Atoi(text)
		if err != nil {
			n = -1
		}
		if q.Tail, err = logTail(n); err != nil {
			answer(rec, r, err)
			return
		}
	}
	if text, ok := given["events"]; ok {
		only, err := strconv.ParseBool(text)
		if err != nil {
			rec.fail(http.StatusBadRequest, "events must be true or false")
			return
		}
		q.Events = only
	}

	lines, err := s.cfg.Backend.Logs(r.Context(), site, r.PathValue("charger"), q)
	if err != nil {
		answer(rec, r, err)
		return
	}
	if lines == nil {
		lines = []LogLine{}
	}
	writeJSON(rec, http.StatusOK, map[string]any{"lines": lines})
}

// adminClient reaches a running API over its socket.
func adminClient(dataDir string) *http.Client {
	return &http.Client{
		Timeout: 3 * time.Second,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", socketPath(dataDir))
			},
		},
	}
}

// Probe asks the API running for this user what it is. It fails when none
// is running.
func Probe(ctx context.Context, dataDir string) (Info, error) {
	client := adminClient(dataDir)
	defer client.CloseIdleConnections()
	var info Info
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://plugyard/info", nil)
	if err != nil {
		return info, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return info, err
	}
	defer resp.Body.Close()
	return info, json.NewDecoder(resp.Body).Decode(&info)
}

// Shutdown asks the API running for this user to stop, and waits until it
// has.
func Shutdown(ctx context.Context, dataDir string) error {
	client := adminClient(dataDir)
	defer client.CloseIdleConnections()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://plugyard/shutdown", nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := Probe(ctx, dataDir); err != nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
	return errors.New("the API did not stop")
}
