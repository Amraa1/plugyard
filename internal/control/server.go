package control

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/Amraa1/plugyard/internal/charger"
	"github.com/Amraa1/plugyard/internal/datadir"
	"github.com/Amraa1/plugyard/internal/site"
)

// ErrAlreadyRunning is returned by Serve when a live process already serves
// a site of the same name.
var ErrAlreadyRunning = errors.New("the site is already running")

// maxSocketPath is the longest socket path that works on every supported
// system: macOS allows 103 characters, Linux and Windows 107.
const maxSocketPath = 103

// Server answers commands for one running site.
type Server struct {
	site     *site.Site
	shutdown func()
	path     string
	listener net.Listener
	http     *http.Server
}

// Serve starts answering commands for s on its socket in dataDir. shutdown
// is called when a command asks the site to shut down.
func Serve(dataDir string, s *site.Site, shutdown func()) (*Server, error) {
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return nil, err
	}
	path := SocketPath(dataDir, s.Name())

	// A socket file may be left over from a process that died. Only a
	// process that answers counts as running.
	if _, err := probe(context.Background(), path); err == nil {
		return nil, fmt.Errorf("site %q: %w", s.Name(), ErrAlreadyRunning)
	}
	_ = os.Remove(path)

	// Operating systems cap socket paths at about 100 characters and report
	// a longer one as a bare "invalid argument".
	if len(path) > maxSocketPath {
		return nil, fmt.Errorf("the data directory path %s is too long for a socket (%d characters, limit %d); set %s to a shorter path",
			dataDir, len(path), maxSocketPath, datadir.Env)
	}

	listener, err := net.Listen("unix", path)
	if err != nil {
		return nil, fmt.Errorf("cannot open the control socket %s: %w", path, err)
	}
	_ = os.Chmod(path, 0o600)

	srv := &Server{site: s, shutdown: shutdown, path: path, listener: listener}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/info", srv.handleInfo)
	mux.HandleFunc("GET /v1/chargers", srv.handleChargers)
	mux.HandleFunc("GET /v1/inspect", srv.handleInspect)
	mux.HandleFunc("POST /v1/exec", srv.handleExec)
	mux.HandleFunc("POST /v1/power", srv.handlePower)
	mux.HandleFunc("POST /v1/shutdown", srv.handleShutdown)
	srv.http = &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}

	go func() { _ = srv.http.Serve(listener) }()
	return srv, nil
}

// Close stops answering commands and removes the socket.
func (srv *Server) Close() {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := srv.http.Shutdown(ctx); err != nil {
		_ = srv.http.Close()
	}
	_ = os.Remove(srv.path)
}

func (srv *Server) handleInfo(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, Info{
		Protocol: ProtocolVersion,
		Site:     srv.site.Name(),
		PID:      os.Getpid(),
		Chargers: srv.site.Len(),
		Powered:  srv.site.Powered(),
		CSMS:     srv.site.CSMS(),
		Speed:    srv.site.Speed(),
	})
}

func (srv *Server) handleShutdown(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, struct{}{})
	if srv.shutdown != nil {
		// Answer first: shutting down closes this very connection.
		go srv.shutdown()
	}
}

func (srv *Server) handleChargers(w http.ResponseWriter, _ *http.Request) {
	states := []wireState{}
	for _, m := range srv.site.States() {
		states = append(states, wireState{
			ChargerState: ChargerState{Site: srv.site.Name(), Name: m.Name, Snapshot: m.Snapshot},
			Powered:      m.Powered,
		})
	}
	writeJSON(w, states)
}

func (srv *Server) handleInspect(w http.ResponseWriter, r *http.Request) {
	member, state, ok := srv.site.Inspect(r.URL.Query().Get("id"))
	if !ok {
		http.Error(w, "the site has no such charger", http.StatusNotFound)
		return
	}
	writeJSON(w, Inspection{Model: member.Model, CSMSURL: member.CSMS.URL, State: state})
}

func (srv *Server) handleExec(w http.ResponseWriter, r *http.Request) {
	var req ExecRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "the request is not valid JSON", http.StatusBadRequest)
		return
	}

	// Chargers are independent, so the action runs on all of them at once.
	results := make([]ExecResult, len(req.ChargePointIDs))
	var wg sync.WaitGroup
	for i, id := range req.ChargePointIDs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			result := ExecResult{ChargePointID: id, Connector: req.Connector}
			if WholeCharger(req.Action) {
				result.Connector = 0
			}
			message, err := srv.exec(r.Context(), id, req)
			if err != nil {
				result.Message = err.Error()
			} else {
				result.OK = true
				result.Message = message
			}
			results[i] = result
		}()
	}
	wg.Wait()
	writeJSON(w, results)
}

func (srv *Server) exec(ctx context.Context, id string, req ExecRequest) (string, error) {
	c := srv.site.Charger(id)
	switch {
	case c != nil:
		return srv.runAction(ctx, c, req)
	case srv.site.Has(id):
		return "", errors.New("the charger is stopped")
	default:
		return "", errors.New("no such charger in this site")
	}
}

func (srv *Server) handlePower(w http.ResponseWriter, r *http.Request) {
	var req PowerRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "the request is not valid JSON", http.StatusBadRequest)
		return
	}

	var op func(id string) (string, error)
	switch req.Op {
	case OpStart:
		op = srv.site.Start
	case OpStop:
		op = srv.site.Stop
	case OpRestart:
		op = srv.site.Restart
	case OpKill:
		op = srv.site.Kill
	case OpRemove:
		op = func(id string) (string, error) { return srv.site.Remove(id, req.Force) }
	default:
		http.Error(w, fmt.Sprintf("unknown operation %q", req.Op), http.StatusBadRequest)
		return
	}

	results := make([]ExecResult, len(req.ChargePointIDs))
	var wg sync.WaitGroup
	for i, id := range req.ChargePointIDs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			result := ExecResult{ChargePointID: id}
			message, err := op(id)
			if err != nil {
				result.Message = err.Error()
			} else {
				result.OK = true
				result.Message = message
			}
			results[i] = result
		}()
	}
	wg.Wait()
	writeJSON(w, results)
}

func (srv *Server) runAction(ctx context.Context, c *charger.Charger, req ExecRequest) (string, error) {
	switch req.Action {
	case ActionPlug:
		params, err := srv.site.EVParams(c.ID(), req.EVPreset, req.EV)
		if err != nil {
			return "", err
		}
		return c.Plug(ctx, req.Connector, params)
	case ActionUnplug:
		return c.Unplug(req.Connector)
	case ActionTap:
		return c.Tap(ctx, req.Connector, req.Tag)
	case ActionFault:
		return c.Fault(req.Connector, req.Fault)
	case ActionClear:
		return c.ClearFault(req.Connector)
	case ActionOffline:
		return c.Offline(req.For)
	case ActionOnline:
		return c.Online()
	case ActionReboot:
		return c.Reboot()
	default:
		return "", fmt.Errorf("unknown action %q", req.Action)
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
