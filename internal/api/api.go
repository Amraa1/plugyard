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
	PID     int       `json:"pid"`
	Listen  string    `json:"listen"`
	TLS     bool      `json:"tls"`
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

	s := &Server{
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
			Since:   time.Now().UTC().Truncate(time.Second),
			Version: cfg.Version,
		},
	}

	routes := http.NewServeMux()
	routes.HandleFunc("GET /v1/openapi.json", s.handleSpec)
	routes.HandleFunc("GET /v1/chargers", s.handleList)
	routes.HandleFunc("GET /v1/chargers/{charger}", s.handleInspect)
	routes.HandleFunc("POST /v1/chargers/{charger}/{action}", s.handleAction)
	s.http = &http.Server{
		Handler:           s.guard(routes),
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
}

type callerKey struct{}

// recorder notes how a call was answered, for the log.
type recorder struct {
	http.ResponseWriter
	entry *Entry
}

func (r *recorder) WriteHeader(status int) {
	r.entry.Status = status
	r.ResponseWriter.WriteHeader(status)
}

func (r *recorder) fail(status int, format string, args ...any) {
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
		defer func() { s.audit.record(*entry) }()
		// What the call is about is noted first, so that the log says it
		// also for a call that is turned away.
		switch parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/"); {
		case len(parts) < 2 || parts[0] != "v1" || parts[1] != "chargers":
		case len(parts) == 2 && r.Method == http.MethodGet:
			entry.Action = "list"
		case len(parts) == 3 && r.Method == http.MethodGet:
			entry.Action, entry.Chargers = "inspect", []string{parts[2]}
		case len(parts) == 4 && r.Method == http.MethodPost:
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
		entry.Token = who.name

		if r.Method == http.MethodPost {
			if who.readOnly {
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
	var known *Error
	if !errors.As(err, &known) {
		rec.fail(http.StatusInternalServerError, "%v", err)
		return
	}
	// A token that is limited to a site is told so when it asks for a
	// charger it cannot reach, whether that charger is in another site
	// or nowhere at all. The answer is the same for both, so it says
	// nothing about what other sites hold.
	if who := r.Context().Value(callerKey{}).(caller); who.site != "" && known.Status == http.StatusNotFound {
		rec.fail(http.StatusForbidden, "token %s is limited to site %s: %s", who.name, who.site, known.Message)
		return
	}
	rec.fail(known.Status, "%s", known.Message)
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
	// The log names the chargers the action ran on, which a pattern
	// does not.
	rec.entry.Chargers = nil
	status, failed := http.StatusOK, 0
	for _, result := range results {
		rec.entry.Chargers = append(rec.entry.Chargers, result.ChargePointID)
		if !result.OK {
			failed++
			rec.entry.Message = result.Message
		}
	}
	if failed > 0 {
		// The action could not be carried out on every charger.
		status = http.StatusConflict
		if len(results) > 1 {
			rec.entry.Message = fmt.Sprintf("failed on %d of %d chargers", failed, len(results))
		}
	} else if len(results) == 1 {
		rec.entry.Message = results[0].Message
	}
	writeJSON(rec, status, map[string]any{"results": results})
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
