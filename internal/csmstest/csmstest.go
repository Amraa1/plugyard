// Package csmstest provides a fake CSMS for tests: an in-process WebSocket
// server that records every message chargers send, answers them, and can
// send requests of its own.
package csmstest

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/Amraa1/plugyard/internal/ocpp/ocppj"
	"github.com/Amraa1/plugyard/internal/ocpp/v16"
)

// waitTimeout bounds every Wait method.
const waitTimeout = 5 * time.Second

// Call is one request a charger sent to the CSMS.
type Call struct {
	ChargePointID string
	Action        string
	Payload       json.RawMessage
	Time          time.Time
}

// Decode reads the call's payload into v.
func (c Call) Decode(t testing.TB, v any) {
	t.Helper()
	if err := json.Unmarshal(c.Payload, v); err != nil {
		t.Fatalf("decoding %s payload %s: %v", c.Action, c.Payload, err)
	}
}

// HandlerFunc answers one action. It returns the result payload, or a
// CallError to answer with an error.
type HandlerFunc func(chargePointID string, payload json.RawMessage) (any, *ocppj.CallError)

// Option configures a fake CSMS.
type Option func(*CSMS)

// RequirePassword makes the CSMS demand HTTP Basic authentication with the
// charge point ID as user name and the given password.
func RequirePassword(password string) Option {
	return func(s *CSMS) { s.password = password }
}

// HeartbeatInterval sets the interval, in seconds, that the CSMS gives in
// its BootNotification answer. The default is 300.
func HeartbeatInterval(seconds int) Option {
	return func(s *CSMS) { s.interval = seconds }
}

// WithoutSubprotocol makes the CSMS accept connections without selecting the
// OCPP WebSocket subprotocol.
func WithoutSubprotocol() Option {
	return func(s *CSMS) { s.noSubprotocol = true }
}

// CSMS is a running fake CSMS.
type CSMS struct {
	srv           *httptest.Server
	password      string
	interval      int
	noSubprotocol bool

	mu       sync.Mutex
	handlers map[string]HandlerFunc
	conns    map[string]*ocppj.Conn
	// calls holds the calls received, by charge point ID, so that looking
	// at one charger stays cheap when thousands are connected.
	calls       map[string][]Call
	connects    map[string]int
	disconnects map[string][]Disconnect

	lastTransactionID int
}

// Disconnect describes how one connection from a charger ended.
type Disconnect struct {
	// Clean is true when the charger closed the WebSocket with a normal
	// close handshake, and false when the connection simply went away.
	Clean bool
}

// Start runs a fake CSMS until the test ends. By default it accepts every
// charger and every tag, numbers transactions from 1, and answers
// BootNotification, Heartbeat, StatusNotification, Authorize,
// StartTransaction, StopTransaction and MeterValues.
func Start(t testing.TB, opts ...Option) *CSMS {
	s := &CSMS{
		interval:    300,
		handlers:    map[string]HandlerFunc{},
		calls:       map[string][]Call{},
		conns:       map[string]*ocppj.Conn{},
		connects:    map[string]int{},
		disconnects: map[string][]Disconnect{},
	}
	for _, opt := range opts {
		opt(s)
	}

	s.Handle(v16.ActionBootNotification, func(string, json.RawMessage) (any, *ocppj.CallError) {
		return v16.BootNotificationResponse{
			Status:      v16.RegistrationAccepted,
			CurrentTime: v16.Time{Time: time.Now()},
			Interval:    s.interval,
		}, nil
	})
	s.Handle(v16.ActionHeartbeat, func(string, json.RawMessage) (any, *ocppj.CallError) {
		return v16.HeartbeatResponse{CurrentTime: v16.Time{Time: time.Now()}}, nil
	})
	s.Handle(v16.ActionStatusNotification, func(string, json.RawMessage) (any, *ocppj.CallError) {
		return v16.StatusNotificationResponse{}, nil
	})

	s.Handle(v16.ActionAuthorize, func(string, json.RawMessage) (any, *ocppj.CallError) {
		return v16.AuthorizeResponse{IDTagInfo: v16.IDTagInfo{Status: v16.AuthorizationAccepted}}, nil
	})
	s.Handle(v16.ActionStartTransaction, func(string, json.RawMessage) (any, *ocppj.CallError) {
		s.mu.Lock()
		s.lastTransactionID++
		id := s.lastTransactionID
		s.mu.Unlock()
		return v16.StartTransactionResponse{
			IDTagInfo:     v16.IDTagInfo{Status: v16.AuthorizationAccepted},
			TransactionID: id,
		}, nil
	})
	s.Handle(v16.ActionStopTransaction, func(string, json.RawMessage) (any, *ocppj.CallError) {
		return v16.StopTransactionResponse{}, nil
	})
	s.Handle(v16.ActionMeterValues, func(string, json.RawMessage) (any, *ocppj.CallError) {
		return v16.MeterValuesResponse{}, nil
	})
	for _, action := range []string{v16.ActionFirmwareStatusNotification, v16.ActionDiagnosticsStatusNotification} {
		s.Handle(action, func(string, json.RawMessage) (any, *ocppj.CallError) {
			return struct{}{}, nil
		})
	}

	s.srv = httptest.NewServer(http.HandlerFunc(s.serve))
	t.Cleanup(func() {
		s.mu.Lock()
		conns := make([]*ocppj.Conn, 0, len(s.conns))
		for _, conn := range s.conns {
			conns = append(conns, conn)
		}
		s.mu.Unlock()
		for _, conn := range conns {
			conn.Drop()
		}
		s.srv.Close()
	})
	return s
}

// URL returns the CSMS URL to configure chargers with. Chargers append
// their charge point ID to it.
func (s *CSMS) URL() string {
	return "ws" + strings.TrimPrefix(s.srv.URL, "http") + "/ocpp"
}

// Handle sets how the CSMS answers an action, replacing any earlier handler.
func (s *CSMS) Handle(action string, fn HandlerFunc) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.handlers[action] = fn
}

func (s *CSMS) serve(w http.ResponseWriter, r *http.Request) {
	id, err := url.PathUnescape(strings.TrimPrefix(r.URL.EscapedPath(), "/ocpp/"))
	if err != nil || id == "" || strings.Contains(id, "/") {
		http.NotFound(w, r)
		return
	}

	if s.password != "" {
		user, pass, ok := r.BasicAuth()
		if !ok || user != id || pass != s.password {
			w.Header().Set("WWW-Authenticate", `Basic realm="csms"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
	}

	accept := &websocket.AcceptOptions{Subprotocols: []string{v16.Subprotocol}}
	if s.noSubprotocol {
		accept.Subprotocols = nil
	}
	ws, err := websocket.Accept(w, r, accept)
	if err != nil {
		return
	}

	conn := ocppj.NewConn(ws, ocppj.Options{
		Handler: func(_ context.Context, action string, payload json.RawMessage) (any, *ocppj.CallError) {
			return s.answer(id, action, payload)
		},
	})

	s.mu.Lock()
	s.conns[id] = conn
	s.connects[id]++
	s.mu.Unlock()

	<-conn.Done()

	s.mu.Lock()
	if s.conns[id] == conn {
		delete(s.conns, id)
	}
	clean := websocket.CloseStatus(conn.Err()) == websocket.StatusNormalClosure
	s.disconnects[id] = append(s.disconnects[id], Disconnect{Clean: clean})
	s.mu.Unlock()
}

func (s *CSMS) answer(id, action string, payload json.RawMessage) (any, *ocppj.CallError) {
	s.mu.Lock()
	s.calls[id] = append(s.calls[id], Call{ChargePointID: id, Action: action, Payload: payload, Time: time.Now()})
	handler := s.handlers[action]
	s.mu.Unlock()

	if handler == nil {
		return nil, &ocppj.CallError{Code: ocppj.ErrNotImplemented, Description: action + " is not implemented by the fake CSMS"}
	}
	return handler(id, payload)
}

// Calls returns every call received so far from one charger, oldest first.
func (s *CSMS) Calls(chargePointID string) []Call {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Call(nil), s.calls[chargePointID]...)
}

// Actions returns the actions of every call received so far from one
// charger, oldest first.
func (s *CSMS) Actions(chargePointID string) []string {
	var out []string
	for _, call := range s.Calls(chargePointID) {
		out = append(out, call.Action)
	}
	return out
}

// WaitForCalls waits until the charger has sent at least n calls of the
// given action and returns those calls. It fails the test if that does not
// happen in time.
func (s *CSMS) WaitForCalls(t testing.TB, chargePointID, action string, n int) []Call {
	t.Helper()
	var matching []Call
	ok := poll(func() bool {
		matching = matching[:0]
		for _, call := range s.Calls(chargePointID) {
			if call.Action == action {
				matching = append(matching, call)
			}
		}
		return len(matching) >= n
	})
	if !ok {
		t.Fatalf("waited for %d %s from %s, got %d; all calls received: %v",
			n, action, chargePointID, len(matching), s.Actions(chargePointID))
	}
	return matching
}

// Connects returns how many times the charger has connected.
func (s *CSMS) Connects(chargePointID string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.connects[chargePointID]
}

// WaitDisconnects waits until the charger's connections have ended at least
// n times and returns how each ended.
func (s *CSMS) WaitDisconnects(t testing.TB, chargePointID string, n int) []Disconnect {
	t.Helper()
	var got []Disconnect
	ok := poll(func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		got = append(got[:0], s.disconnects[chargePointID]...)
		return len(got) >= n
	})
	if !ok {
		t.Fatalf("waited for %d disconnects from %s, got %d", n, chargePointID, len(got))
	}
	return got
}

// Call sends a request to a connected charger and decodes its answer into
// result, unless result is nil.
func (s *CSMS) Call(ctx context.Context, chargePointID, action string, payload, result any) error {
	s.mu.Lock()
	conn := s.conns[chargePointID]
	s.mu.Unlock()
	if conn == nil {
		return ocppj.ErrClosed
	}
	return conn.Call(ctx, action, payload, result)
}

// Drop cuts the charger's connection without a close handshake, as a network
// failure would.
func (s *CSMS) Drop(chargePointID string) {
	s.mu.Lock()
	conn := s.conns[chargePointID]
	s.mu.Unlock()
	if conn != nil {
		conn.Drop()
	}
}

func poll(condition func() bool) bool {
	deadline := time.Now().Add(waitTimeout)
	for {
		if condition() {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(5 * time.Millisecond)
	}
}
