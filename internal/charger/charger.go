// Package charger simulates one charger: a single charge point ID with its
// own connection to the CSMS.
package charger

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Amraa1/plugyard/internal/model"
	"github.com/Amraa1/plugyard/internal/ocpp/ocppj"
	"github.com/Amraa1/plugyard/internal/ocpp/v16"
)

// CSMS says how to reach the CSMS.
type CSMS struct {
	// URL is the CSMS endpoint, ws:// or wss://. The charge point ID is
	// appended to its path.
	URL string
	// Password, if set, is sent with HTTP Basic authentication, with the
	// charge point ID as the user name.
	Password string
	// InsecureSkipVerify disables TLS certificate verification.
	InsecureSkipVerify bool
}

// Timing holds the charger's own timeouts. Zero values select the defaults.
type Timing struct {
	// CallTimeout is how long to wait for the CSMS to answer a message.
	CallTimeout time.Duration
	// ReconnectMin and ReconnectMax bound the wait between connection
	// attempts, which doubles after each failure.
	ReconnectMin time.Duration
	ReconnectMax time.Duration
	// Tick is how often the EVs on the connectors are advanced.
	Tick time.Duration
}

// Config describes one charger.
type Config struct {
	// ID is the charge point ID.
	ID    string
	Model *model.Model
	CSMS  CSMS
	// Speed multiplies the rate of EV charging and energy accumulation.
	// Zero means 1. Protocol timers are not affected.
	Speed  float64
	Timing Timing
	// Log, if set, receives the charger's events and OCPP messages. It is
	// called from several goroutines and must not block.
	Log func(Entry)
}

// EntryKind tells the kinds of log entry apart.
type EntryKind int

const (
	// Event is something that happened to the charger, in words.
	Event EntryKind = iota
	// MessageSent is a raw OCPP message sent to the CSMS.
	MessageSent
	// MessageReceived is a raw OCPP message received from the CSMS.
	MessageReceived
)

// Entry is one line of a charger's log.
type Entry struct {
	Time          time.Time
	ChargePointID string
	Kind          EntryKind
	Text          string
}

const (
	defaultCallTimeout       = 30 * time.Second
	defaultReconnectMin      = time.Second
	defaultReconnectMax      = 30 * time.Second
	defaultHeartbeatInterval = 300 * time.Second
	defaultBootRetryInterval = 30 * time.Second
	defaultTick              = time.Second
)

// Connection states, as shown by "plugyard ps".
const (
	Connecting = "connecting"
	Online     = "online"
	Offline    = "offline"
)

// defaultConfig holds the OCPP configuration keys the charger acts on and
// their values when the Chargerfile does not set them.
var defaultConfig = map[string]string{
	"HeartbeatInterval":          "300",
	"ConnectionTimeOut":          "60",
	"MeterValueSampleInterval":   "60",
	"MeterValuesSampledData":     v16.MeasurandEnergy + "," + v16.MeasurandPower,
	"StopTxnSampledData":         v16.MeasurandEnergy,
	"StopTransactionOnInvalidId": "true",
}

// Charger is one simulated charger.
type Charger struct {
	cfg    Config
	url    string
	config map[string]string // OCPP configuration keys

	// wake tells the connection that the outbox has something to send.
	wake chan struct{}

	mu         sync.Mutex
	connection string      // Connecting, Online or Offline
	conn       *ocppj.Conn // set while the CSMS has accepted the charger
	connectors []*connector
	outbox     []*outMsg
}

// outMsg is a message waiting to be sent to the CSMS. Messages are sent one
// at a time, in order, and stay queued while the charger is not connected.
type outMsg struct {
	action string
	// build returns the payload. It runs just before sending, so that it can
	// use values learned from earlier answers, such as a transaction ID.
	build func() any
	// done, if set, is given the answer, or nil if the CSMS answered with an
	// error.
	done func(result json.RawMessage)
}

// New checks cfg and returns a charger that has not been powered on yet.
func New(cfg Config) (*Charger, error) {
	if strings.TrimSpace(cfg.ID) == "" {
		return nil, errors.New("the charge point ID is empty")
	}
	if cfg.Model == nil {
		return nil, errors.New("the charger has no model")
	}

	u, err := url.Parse(cfg.CSMS.URL)
	if err != nil {
		return nil, fmt.Errorf("invalid CSMS URL: %w", err)
	}
	if u.Scheme != "ws" && u.Scheme != "wss" {
		return nil, fmt.Errorf("invalid CSMS URL %q: it must start with ws:// or wss://", cfg.CSMS.URL)
	}
	if u.Host == "" {
		return nil, fmt.Errorf("invalid CSMS URL %q: it has no host", cfg.CSMS.URL)
	}

	if cfg.Speed < 0 {
		return nil, errors.New("the speed cannot be negative")
	}
	if cfg.Speed == 0 {
		cfg.Speed = 1
	}

	if cfg.Timing.CallTimeout <= 0 {
		cfg.Timing.CallTimeout = defaultCallTimeout
	}
	if cfg.Timing.ReconnectMin <= 0 {
		cfg.Timing.ReconnectMin = defaultReconnectMin
	}
	if cfg.Timing.ReconnectMax < cfg.Timing.ReconnectMin {
		cfg.Timing.ReconnectMax = max(defaultReconnectMax, cfg.Timing.ReconnectMin)
	}
	if cfg.Timing.Tick <= 0 {
		cfg.Timing.Tick = defaultTick
	}

	c := &Charger{
		cfg:        cfg,
		url:        u.JoinPath(url.PathEscape(cfg.ID)).String(),
		config:     map[string]string{},
		wake:       make(chan struct{}, 1),
		connection: Connecting,
	}
	for key, value := range defaultConfig {
		c.config[key] = value
	}
	for key, value := range cfg.Model.Config {
		c.config[key] = value
	}
	for _, mc := range cfg.Model.Connectors {
		c.connectors = append(c.connectors, &connector{
			id:       mc.ID,
			maxPower: float64(mc.MaxPower),
			status:   v16.StatusAvailable,
		})
	}
	return c, nil
}

// ID returns the charge point ID.
func (c *Charger) ID() string { return c.cfg.ID }

// URL returns the full address the charger connects to: the CSMS URL with
// the charge point ID appended.
func (c *Charger) URL() string { return c.url }

// Run powers the charger on and keeps it running until ctx is cancelled,
// which shuts it down in an orderly way. While running, the charger
// reconnects by itself whenever the connection is lost.
func (c *Charger) Run(ctx context.Context) error {
	c.event("powered on")
	wait := c.cfg.Timing.ReconnectMin

	// The EVs keep charging whether or not the CSMS is reachable.
	var simulation sync.WaitGroup
	simulation.Add(1)
	go func() {
		defer simulation.Done()
		c.simulate(ctx)
	}()
	defer simulation.Wait()

	for {
		connected, err := c.session(ctx)
		if ctx.Err() != nil {
			c.event("powered off")
			return nil
		}
		if connected {
			c.event("connection lost: %v", err)
			wait = c.cfg.Timing.ReconnectMin
		} else {
			c.event("could not connect: %v", err)
		}

		c.mu.Lock()
		c.connection = Offline
		c.mu.Unlock()

		// A little randomness keeps many chargers from reconnecting in step.
		pause := wait + time.Duration(rand.Int64N(int64(wait)/4+1))
		c.event("retrying in %s", pause.Round(time.Millisecond))
		if !sleep(ctx, pause) {
			c.event("powered off")
			return nil
		}
		wait = min(wait*2, c.cfg.Timing.ReconnectMax)
	}
}

// session runs one connection from dial to disconnect. It reports whether
// the connection was established, and why it ended.
func (c *Charger) session(ctx context.Context) (connected bool, err error) {
	dialCtx, cancel := context.WithTimeout(ctx, c.cfg.Timing.CallTimeout)
	conn, err := ocppj.Dial(dialCtx, c.url, ocppj.DialOptions{
		Options:            ocppj.Options{OnFrame: c.frame},
		Subprotocol:        v16.Subprotocol,
		Username:           c.cfg.ID,
		Password:           c.cfg.CSMS.Password,
		InsecureSkipVerify: c.cfg.CSMS.InsecureSkipVerify,
	})
	cancel()
	if err != nil {
		return false, err
	}

	c.event("connected to %s", c.url)
	if conn.Subprotocol() != v16.Subprotocol {
		c.event("warning: the CSMS did not select the %q WebSocket subprotocol", v16.Subprotocol)
	}

	defer func() {
		if ctx.Err() != nil {
			// Orderly shutdown: say goodbye properly.
			_ = conn.Close()
			c.event("disconnected")
			return
		}
		conn.Drop()
	}()

	heartbeat, err := c.boot(ctx, conn)
	if err != nil {
		return true, err
	}

	// From here on the charger is online: report where every connector
	// stands, after anything that was queued while it was away.
	c.mu.Lock()
	c.conn = conn
	c.connection = Online
	for _, k := range c.connectors {
		c.enqueueStatus(k)
	}
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		c.conn = nil
		c.connection = Offline
		c.mu.Unlock()
	}()

	ticker := time.NewTicker(heartbeat)
	defer ticker.Stop()
	for {
		if err := c.drain(conn); err != nil {
			return true, err
		}
		select {
		case <-ctx.Done():
			// Orderly shutdown: end the transactions and tell the CSMS
			// before leaving.
			c.mu.Lock()
			for _, k := range c.connectors {
				if k.tx != nil {
					c.stopTx(k, v16.ReasonOther, k.tx.idTag)
					c.reportStatus(k)
				}
			}
			c.mu.Unlock()
			_ = c.drain(conn)
			return true, ctx.Err()
		case <-conn.Done():
			return true, conn.Err()
		case <-c.wake:
		case <-ticker.C:
			if err := c.call(context.Background(), conn, v16.ActionHeartbeat, v16.HeartbeatRequest{}, nil); err != nil {
				return true, err
			}
		}
	}
}

// drain sends the queued messages in order. A message leaves the queue only
// once the CSMS has answered it, so nothing is lost if the connection drops.
func (c *Charger) drain(conn *ocppj.Conn) error {
	for {
		c.mu.Lock()
		if len(c.outbox) == 0 {
			c.mu.Unlock()
			return nil
		}
		m := c.outbox[0]
		c.mu.Unlock()

		var result json.RawMessage
		err := c.call(context.Background(), conn, m.action, m.build(), &result)
		var callErr *ocppj.CallError
		switch {
		case errors.As(err, &callErr):
			c.event("the CSMS answered %s with an error: %v", m.action, callErr)
			result = nil
		case err != nil:
			return err
		}

		c.mu.Lock()
		c.outbox = c.outbox[1:]
		c.mu.Unlock()
		if m.done != nil {
			m.done(result)
		}
	}
}

// enqueue adds a message to the outbox. The caller holds c.mu.
func (c *Charger) enqueue(m *outMsg) {
	c.outbox = append(c.outbox, m)
	select {
	case c.wake <- struct{}{}:
	default:
	}
}

// boot sends BootNotification until the CSMS accepts the charger, and
// returns the heartbeat interval to use.
func (c *Charger) boot(ctx context.Context, conn *ocppj.Conn) (time.Duration, error) {
	m := c.cfg.Model
	for {
		var resp v16.BootNotificationResponse
		err := c.call(ctx, conn, v16.ActionBootNotification, v16.BootNotificationRequest{
			ChargePointVendor: m.Vendor,
			ChargePointModel:  m.Name,
			FirmwareVersion:   m.Firmware,
		}, &resp)
		if err != nil {
			return 0, err
		}

		interval := time.Duration(resp.Interval) * time.Second

		if resp.Status == v16.RegistrationAccepted {
			c.event("accepted by the CSMS")
			if interval <= 0 {
				interval = c.configuredHeartbeatInterval()
			}
			return interval, nil
		}

		// Pending or Rejected: OCPP 1.6 has the charger stay quiet and ask
		// again after the interval the CSMS gave.
		if interval <= 0 {
			interval = defaultBootRetryInterval
		}
		c.event("the CSMS answered BootNotification with %q; asking again in %s", resp.Status, interval)

		select {
		case <-time.After(interval):
		case <-ctx.Done():
			return 0, ctx.Err()
		case <-conn.Done():
			return 0, conn.Err()
		}
	}
}

func (c *Charger) configuredHeartbeatInterval() time.Duration {
	if interval := c.configSeconds("HeartbeatInterval"); interval > 0 {
		return interval
	}
	return defaultHeartbeatInterval
}

// configSeconds reads a configuration key holding a number of seconds. It
// returns zero when the key is missing, not a number, or not positive.
func (c *Charger) configSeconds(key string) time.Duration {
	seconds, err := strconv.Atoi(strings.TrimSpace(c.config[key]))
	if err != nil || seconds <= 0 {
		return 0
	}
	return time.Duration(seconds) * time.Second
}

// configMeasurands reads a configuration key holding a comma-separated list
// of measurands, keeping the ones the charger can report.
func (c *Charger) configMeasurands(key string) []string {
	var out []string
	for _, name := range strings.Split(c.config[key], ",") {
		switch name = strings.TrimSpace(name); name {
		case v16.MeasurandEnergy, v16.MeasurandPower, v16.MeasurandSoC:
			out = append(out, name)
		}
	}
	return out
}

func (c *Charger) call(ctx context.Context, conn *ocppj.Conn, action string, req, resp any) error {
	ctx, cancel := context.WithTimeout(ctx, c.cfg.Timing.CallTimeout)
	defer cancel()
	return conn.Call(ctx, action, req, resp)
}

func (c *Charger) event(format string, args ...any) {
	c.log(Event, fmt.Sprintf(format, args...))
}

func (c *Charger) frame(dir ocppj.Direction, frame []byte) {
	kind := MessageSent
	if dir == ocppj.Received {
		kind = MessageReceived
	}
	c.log(kind, string(frame))
}

func (c *Charger) log(kind EntryKind, text string) {
	if c.cfg.Log == nil {
		return
	}
	c.cfg.Log(Entry{Time: time.Now(), ChargePointID: c.cfg.ID, Kind: kind, Text: text})
}

// sleep waits for d and reports whether it finished without ctx ending.
func sleep(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-ctx.Done():
		return false
	}
}
