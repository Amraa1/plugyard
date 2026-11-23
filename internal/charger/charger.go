// Package charger simulates one charger: a single charge point ID with its
// own connection to the CSMS.
package charger

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"net/url"
	"strconv"
	"strings"
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
}

// Config describes one charger.
type Config struct {
	// ID is the charge point ID.
	ID     string
	Model  *model.Model
	CSMS   CSMS
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
)

// Charger is one simulated charger.
type Charger struct {
	cfg Config
	url string
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

	if cfg.Timing.CallTimeout <= 0 {
		cfg.Timing.CallTimeout = defaultCallTimeout
	}
	if cfg.Timing.ReconnectMin <= 0 {
		cfg.Timing.ReconnectMin = defaultReconnectMin
	}
	if cfg.Timing.ReconnectMax < cfg.Timing.ReconnectMin {
		cfg.Timing.ReconnectMax = max(defaultReconnectMax, cfg.Timing.ReconnectMin)
	}

	return &Charger{
		cfg: cfg,
		url: u.JoinPath(url.PathEscape(cfg.ID)).String(),
	}, nil
}

// URL returns the full address the charger connects to: the CSMS URL with
// the charge point ID appended.
func (c *Charger) URL() string { return c.url }

// Run powers the charger on and keeps it running until ctx is cancelled,
// which shuts it down in an orderly way. While running, the charger
// reconnects by itself whenever the connection is lost.
func (c *Charger) Run(ctx context.Context) error {
	c.event("powered on")
	wait := c.cfg.Timing.ReconnectMin

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

	for _, connector := range c.cfg.Model.Connectors {
		now := v16.Time{Time: time.Now()}
		err := c.call(ctx, conn, v16.ActionStatusNotification, v16.StatusNotificationRequest{
			ConnectorID: connector.ID,
			ErrorCode:   v16.ErrorCodeNoError,
			Status:      v16.StatusAvailable,
			Timestamp:   &now,
		}, nil)
		if err != nil {
			return true, err
		}
	}

	ticker := time.NewTicker(heartbeat)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return true, ctx.Err()
		case <-conn.Done():
			return true, conn.Err()
		case <-ticker.C:
			if err := c.call(ctx, conn, v16.ActionHeartbeat, v16.HeartbeatRequest{}, nil); err != nil {
				return true, err
			}
		}
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
	seconds, err := strconv.Atoi(c.cfg.Model.Config["HeartbeatInterval"])
	if err != nil || seconds <= 0 {
		return defaultHeartbeatInterval
	}
	return time.Duration(seconds) * time.Second
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
