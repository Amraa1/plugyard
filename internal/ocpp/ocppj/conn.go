package ocppj

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"sync"
	"time"

	"github.com/coder/websocket"
)

// Direction says which way a frame travelled, seen from this end.
type Direction int

const (
	Sent Direction = iota
	Received
)

// Handler answers a call from the other side. It returns the result payload,
// or a CallError to answer with an error. A nil payload is sent as {}.
type Handler func(ctx context.Context, action string, payload json.RawMessage) (any, *CallError)

// Options configure a Conn.
type Options struct {
	// Handler answers incoming calls. Without one, every call is answered
	// with NotImplemented.
	Handler Handler
	// OnFrame, if set, is given every frame sent and received, for logging.
	// It must not block.
	OnFrame func(dir Direction, frame []byte)
}

// DialOptions configure Dial.
type DialOptions struct {
	Options
	// Subprotocol is the WebSocket subprotocol to request, such as "ocpp1.6".
	Subprotocol string
	// Username and Password, when Password is set, are sent as HTTP Basic
	// authentication.
	Username string
	Password string
	// InsecureSkipVerify disables TLS certificate verification.
	InsecureSkipVerify bool
}

// ErrClosed is returned by Call when the connection is closed before the
// answer arrives.
var ErrClosed = errors.New("ocppj: connection closed")

// Conn is one OCPP-J connection. Both ends of a connection use the same type:
// each may call the other and must answer the other's calls.
type Conn struct {
	ws          *websocket.Conn
	opts        Options
	subprotocol string
	// remote is the address a dialed connection goes to, or the zero
	// value when it is not known.
	remote netip.Addr

	callMu sync.Mutex // OCPP allows one outstanding call per direction

	mu      sync.Mutex
	pending map[string]chan Message

	cancel  context.CancelFunc
	done    chan struct{}
	err     error
	handler sync.WaitGroup
}

// Dial opens a connection to url.
func Dial(ctx context.Context, url string, opts DialOptions) (*Conn, error) {
	header := http.Header{}
	if opts.Password != "" {
		credentials := opts.Username + ":" + opts.Password
		header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(credentials)))
	}

	transport := http.DefaultTransport.(*http.Transport).Clone()
	// Note where the connection goes: the address of the CSMS, or of the
	// proxy when one is set.
	var mu sync.Mutex
	var remote netip.Addr
	dial := transport.DialContext
	transport.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		conn, err := dial(ctx, network, addr)
		if err == nil {
			if tcp, ok := conn.RemoteAddr().(*net.TCPAddr); ok {
				mu.Lock()
				remote = tcp.AddrPort().Addr().Unmap()
				mu.Unlock()
			}
		}
		return conn, err
	}
	if opts.InsecureSkipVerify {
		transport.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
	}

	dialOpts := &websocket.DialOptions{
		HTTPClient: &http.Client{Transport: transport},
		HTTPHeader: header,
	}
	if opts.Subprotocol != "" {
		dialOpts.Subprotocols = []string{opts.Subprotocol}
	}

	ws, resp, err := websocket.Dial(ctx, url, dialOpts)
	if err != nil {
		if resp != nil && resp.StatusCode != http.StatusSwitchingProtocols {
			return nil, fmt.Errorf("the CSMS answered HTTP %s", resp.Status)
		}
		return nil, err
	}
	conn := NewConn(ws, opts.Options)
	mu.Lock()
	conn.remote = remote
	mu.Unlock()
	return conn, nil
}

// RemoteAddr returns the address a dialed connection goes to. It is the
// zero value for a connection that was accepted, or when it is not known.
func (c *Conn) RemoteAddr() netip.Addr { return c.remote }

// NewConn wraps an established WebSocket connection and starts reading from
// it.
func NewConn(ws *websocket.Conn, opts Options) *Conn {
	ws.SetReadLimit(1 << 20)

	ctx, cancel := context.WithCancel(context.Background())
	c := &Conn{
		ws:          ws,
		opts:        opts,
		subprotocol: ws.Subprotocol(),
		pending:     map[string]chan Message{},
		cancel:      cancel,
		done:        make(chan struct{}),
	}
	go c.readLoop(ctx)
	return c
}

// Subprotocol returns the WebSocket subprotocol both sides agreed on, or ""
// if none was agreed.
func (c *Conn) Subprotocol() string { return c.subprotocol }

// Done is closed when the connection has ended, for any reason.
func (c *Conn) Done() <-chan struct{} { return c.done }

// Err reports why the connection ended. It is only meaningful after Done is
// closed.
func (c *Conn) Err() error { return c.err }

// Call sends a call and waits for its answer, which is decoded into result
// unless result is nil. If the other side answers with an error, Call returns
// a *CallError.
func (c *Conn) Call(ctx context.Context, action string, payload, result any) error {
	body, err := marshalPayload(payload)
	if err != nil {
		return fmt.Errorf("ocppj: encoding %s: %w", action, err)
	}

	c.callMu.Lock()
	defer c.callMu.Unlock()

	id := newMessageID()
	answer := make(chan Message, 1)
	c.mu.Lock()
	c.pending[id] = answer
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
	}()

	if err := c.write(ctx, Message{Type: TypeCall, ID: id, Action: action, Payload: body}); err != nil {
		return err
	}

	select {
	case m := <-answer:
		if m.Type == TypeCallError {
			return &CallError{Code: m.ErrorCode, Description: m.ErrorDescription, Details: m.ErrorDetails}
		}
		if result == nil {
			return nil
		}
		if err := json.Unmarshal(m.Payload, result); err != nil {
			return fmt.Errorf("ocppj: decoding the answer to %s: %w", action, err)
		}
		return nil
	case <-c.done:
		return ErrClosed
	case <-ctx.Done():
		return fmt.Errorf("ocppj: no answer to %s: %w", action, ctx.Err())
	}
}

// Close ends the connection with a normal WebSocket close handshake.
func (c *Conn) Close() error {
	err := c.ws.Close(websocket.StatusNormalClosure, "")
	c.cancel()
	<-c.done
	return err
}

// Drop ends the connection abruptly, without a close handshake, as a power
// cut or a network failure would.
func (c *Conn) Drop() {
	c.ws.CloseNow()
	c.cancel()
	<-c.done
}

func (c *Conn) readLoop(ctx context.Context) {
	defer close(c.done)
	defer c.cancel()

	for {
		typ, data, err := c.ws.Read(ctx)
		if err != nil {
			c.err = err
			c.ws.CloseNow()
			return
		}
		if typ != websocket.MessageText {
			continue
		}
		c.frame(Received, data)

		m, err := Decode(data)
		if err != nil {
			// A frame that cannot be read is answered with an error when it
			// carries an ID to answer to, and ignored otherwise.
			if m.Type == TypeCall && m.ID != "" {
				c.answerError(ctx, m.ID, &CallError{Code: ErrFormationViolation, Description: err.Error()})
			}
			continue
		}

		switch m.Type {
		case TypeCall:
			c.handler.Add(1)
			go func() {
				defer c.handler.Done()
				c.handle(ctx, m)
			}()
		case TypeCallResult, TypeCallError:
			c.mu.Lock()
			answer := c.pending[m.ID]
			c.mu.Unlock()
			if answer != nil {
				select {
				case answer <- m:
				default:
				}
			}
		}
	}
}

func (c *Conn) handle(ctx context.Context, call Message) {
	if c.opts.Handler == nil {
		c.answerError(ctx, call.ID, &CallError{Code: ErrNotImplemented, Description: call.Action + " is not implemented"})
		return
	}

	result, callErr := c.opts.Handler(ctx, call.Action, call.Payload)
	if callErr != nil {
		c.answerError(ctx, call.ID, callErr)
		return
	}
	body, err := marshalPayload(result)
	if err != nil {
		c.answerError(ctx, call.ID, &CallError{Code: ErrInternalError, Description: err.Error()})
		return
	}
	_ = c.write(ctx, Message{Type: TypeCallResult, ID: call.ID, Payload: body})
}

func (c *Conn) answerError(ctx context.Context, id string, e *CallError) {
	_ = c.write(ctx, Message{
		Type:             TypeCallError,
		ID:               id,
		ErrorCode:        e.Code,
		ErrorDescription: e.Description,
		ErrorDetails:     e.Details,
	})
}

func (c *Conn) write(ctx context.Context, m Message) error {
	data, err := m.Encode()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	if err := c.ws.Write(ctx, websocket.MessageText, data); err != nil {
		select {
		case <-c.done:
			return ErrClosed
		default:
			return err
		}
	}
	c.frame(Sent, data)
	return nil
}

func (c *Conn) frame(dir Direction, data []byte) {
	if c.opts.OnFrame != nil {
		c.opts.OnFrame(dir, data)
	}
}

func marshalPayload(payload any) (json.RawMessage, error) {
	if payload == nil {
		return emptyObject, nil
	}
	if raw, ok := payload.(json.RawMessage); ok {
		if len(raw) == 0 {
			return emptyObject, nil
		}
		return raw, nil
	}
	return json.Marshal(payload)
}

func newMessageID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err) // the system's random source is broken
	}
	return hex.EncodeToString(b[:])
}
