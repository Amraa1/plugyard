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
	// Reboot is how long the charger is away when it reboots.
	Reboot time.Duration
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
	// BeforeDial, if set, is called before every connection attempt and
	// may wait. A site uses it to pace the connections of its chargers.
	BeforeDial func(ctx context.Context) error
	// Log, if set, receives the charger's events and OCPP messages. It is
	// called from several goroutines and must not block.
	Log func(Entry)
	// Restore, if set, is the state saved when the charger last had power.
	Restore *State
	// Save, if set, is given the charger's state whenever something worth
	// keeping has changed: at once for transactions and queued messages, and
	// every MeterSaveInterval seconds for meter readings.
	Save func(State) error
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
	defaultMeterSaveInterval = 5 * time.Second
	defaultReboot            = time.Second
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
	// How failures are handled.
	"TransactionMessageAttempts":      "3",
	"TransactionMessageRetryInterval": "60",
	"AllowOfflineTxForUnknownId":      "false",
	"AuthorizationCacheEnabled":       "false",
	"LocalAuthorizeOffline":           "true",
	"AuthorizeRemoteTxRequests":       "false",
	// What the charger is.
	"SupportedFeatureProfiles": "Core,RemoteTrigger",
	"GetConfigurationMaxKeys":  "500",
	// MeterSaveInterval is plugyard's own key: how often, in seconds, meter
	// readings are saved to disk during a transaction. It bounds the energy a
	// power cut loses from the reported meterStop.
	"MeterSaveInterval": "5",
}

// Charger is one simulated charger. It is powered on once, with Run; a
// charger that has been powered off is replaced by a new one made from its
// saved state.
type Charger struct {
	cfg    Config
	url    string
	config map[string]string // OCPP configuration keys
	// overrides holds the configuration keys the CSMS changed.
	overrides map[string]string
	// recovered describes what restoring the saved state brought to light,
	// to be logged when the charger is powered on.
	recovered []string

	// wake tells the connection that the outbox has something to send.
	wake chan struct{}

	mu         sync.Mutex
	connection string      // Connecting, Online or Offline
	conn       *ocppj.Conn // set while the CSMS has accepted the charger
	connectors []*connector
	// zero stands for the charger as a whole, which OCPP addresses as
	// connector 0. It is reported only when it is not Available.
	zero   *connector
	outbox []*outMsg
	// retryAt is when the message at the head of the outbox may be sent
	// again after the CSMS answered it with an error.
	retryAt time.Time
	// authCache holds what the CSMS said about tags, when the authorization
	// cache is on.
	authCache map[string]v16.IDTagInfo
	// netDown says the network connection was taken down on purpose.
	netDown  bool
	netTimer *time.Timer
	// network, reboots and heartbeatChanged wake the connection.
	network          chan struct{}
	reboots          chan reboot
	heartbeatChanged chan struct{}
	// unconfirmed holds, by reference, the transactions whose
	// StartTransaction the CSMS has not answered yet.
	unconfirmed map[int]*transaction
	nextTxRef   int

	// cut ends Run. killed says the end is a power cut, and off that the
	// charger has no power any more.
	cut    func()
	killed bool
	off    bool
	// dirty says the state has changed since it was last saved.
	dirty    bool
	nextSave time.Time
}

// outMsg is a message waiting to be sent to the CSMS. Messages are sent one
// at a time, in order, and stay queued while the charger is not connected.
type outMsg struct {
	action string
	// req points to the request.
	req any
	// txRef, when not zero, names a transaction whose ID the request needs
	// and the CSMS has not given yet. The ID is filled in when it arrives.
	txRef int
	// volatile messages are not saved: they describe a moment, and are sent
	// afresh after the next boot.
	volatile bool
	// attempts counts how often the CSMS has answered the message with an
	// error.
	attempts int
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
	if cfg.Timing.Reboot <= 0 {
		cfg.Timing.Reboot = defaultReboot
	}

	c := &Charger{
		cfg:         cfg,
		url:         u.JoinPath(url.PathEscape(cfg.ID)).String(),
		config:      map[string]string{},
		overrides:   map[string]string{},
		unconfirmed: map[int]*transaction{},
		wake:        make(chan struct{}, 1),
		connection:  Connecting,
		zero:        &connector{status: v16.StatusAvailable, code: v16.ErrorCodeNoError},
		authCache:   map[string]v16.IDTagInfo{},

		network:          make(chan struct{}, 1),
		reboots:          make(chan reboot, 1),
		heartbeatChanged: make(chan struct{}, 1),
	}
	for key, value := range defaultConfig {
		c.config[key] = value
	}
	for key, value := range cfg.Model.Config {
		c.config[key] = value
	}
	c.config["NumberOfConnectors"] = strconv.Itoa(len(cfg.Model.Connectors))
	for _, mc := range cfg.Model.Connectors {
		c.connectors = append(c.connectors, &connector{
			id:       mc.ID,
			maxPower: float64(mc.MaxPower),
			status:   v16.StatusAvailable,
			code:     v16.ErrorCodeNoError,
		})
	}
	if cfg.Restore != nil {
		c.restore(cfg.Restore)
	}
	return c, nil
}

// ID returns the charge point ID.
func (c *Charger) ID() string { return c.cfg.ID }

// URL returns the full address the charger connects to: the CSMS URL with
// the charge point ID appended.
func (c *Charger) URL() string { return c.url }

// Run powers the charger on and keeps it running until ctx is cancelled,
// which shuts it down in an orderly way, or until Kill cuts its power. While
// running, the charger reconnects by itself whenever the connection is lost.
// A charger can be powered on only once.
func (c *Charger) Run(ctx context.Context) error {
	ctx, cut := context.WithCancel(ctx)
	defer cut()

	c.mu.Lock()
	if c.cut != nil || c.off {
		c.mu.Unlock()
		return errors.New("the charger has already been powered on once")
	}
	c.cut = cut
	if c.killed {
		cut()
	}
	// Save what restoring brought: a transaction ended by a power loss must
	// not be found open again.
	c.dirty = true
	c.unlock()

	c.event("powered on")
	for _, line := range c.recovered {
		c.event("%s", line)
	}
	wait := c.cfg.Timing.ReconnectMin

	// The EVs keep charging whether or not the CSMS is reachable.
	var simulation sync.WaitGroup
	simulation.Add(1)
	go func() {
		defer simulation.Done()
		c.simulate(ctx)
	}()
	defer simulation.Wait()
	defer c.powerOff()

	for {
		// A network that was taken down stays down until it is restored.
		if !c.waitForNetwork(ctx) {
			return nil
		}

		connected, err := c.session(ctx)
		if ctx.Err() != nil {
			return nil
		}
		switch {
		case errors.Is(err, errReboot):
			c.mu.Lock()
			c.connection = Offline
			c.mu.Unlock()
			c.event("rebooting")
			if !sleep(ctx, c.cfg.Timing.Reboot) {
				return nil
			}
			c.event("powered on again")
			wait = c.cfg.Timing.ReconnectMin
			continue
		case errors.Is(err, errNetwork):
			wait = c.cfg.Timing.ReconnectMin
			continue
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
			return nil
		}
		wait = min(wait*2, c.cfg.Timing.ReconnectMax)
	}
}

// Kill cuts the charger's power: Run returns at once, the connection drops
// with no message, and nothing that happened since the last save is kept.
func (c *Charger) Kill() {
	c.mu.Lock()
	c.killed = true
	cut, conn := c.cut, c.conn
	c.mu.Unlock()
	if cut != nil {
		cut()
	}
	if conn != nil {
		// Do not wait for an answer that is on its way.
		conn.Drop()
	}
}

// errOff is returned by actions on a charger that has no power.
var errOff = errors.New("the charger is stopped")

func (c *Charger) isKilled() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.killed
}

// powerOff is the last thing Run does.
func (c *Charger) powerOff() {
	c.mu.Lock()
	if c.killed {
		c.off = true
		c.mu.Unlock()
		c.event("power cut")
		return
	}
	// An orderly shutdown ends the transactions even when the CSMS cannot be
	// told now: the messages stay queued and are sent after the next boot.
	for _, k := range c.connectors {
		if k.tx != nil {
			c.stopTx(k, v16.ReasonOther, k.tx.idTag)
			c.reportStatus(k)
		}
	}
	c.dirty = true
	c.flush()
	c.off = true
	c.mu.Unlock()
	c.event("powered off")
}

// session runs one connection from dial to disconnect. It reports whether
// the connection was established, and why it ended.
func (c *Charger) session(ctx context.Context) (connected bool, err error) {
	if c.cfg.BeforeDial != nil {
		if err := c.cfg.BeforeDial(ctx); err != nil {
			return false, err
		}
	}
	dialCtx, cancel := context.WithTimeout(ctx, c.cfg.Timing.CallTimeout)
	conn, err := ocppj.Dial(dialCtx, c.url, ocppj.DialOptions{
		Options:            ocppj.Options{OnFrame: c.frame, Handler: c.handle},
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

	// goodbye says the charger is leaving on purpose and closes the
	// connection properly. Otherwise the connection is dropped.
	goodbye := false
	defer func() {
		if goodbye && !c.isKilled() {
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
	if c.zero.status != v16.StatusAvailable {
		c.enqueueStatus(c.zero)
	}
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

		// retry fires when a message the CSMS answered with an error is due
		// to be sent again.
		var retry <-chan time.Time
		c.mu.Lock()
		if d := time.Until(c.retryAt); d > 0 && len(c.outbox) > 0 {
			retry = time.After(d)
		}
		c.mu.Unlock()

		select {
		case <-retry:
		case <-c.heartbeatChanged:
			c.mu.Lock()
			interval := c.configuredHeartbeatInterval()
			c.mu.Unlock()
			ticker.Reset(interval)
		case <-c.network:
			c.mu.Lock()
			down := c.netDown
			c.mu.Unlock()
			if down {
				return true, errNetwork
			}
		case r := <-c.reboots:
			// The transactions end and the CSMS is told before the charger
			// goes away.
			c.mu.Lock()
			c.endForReboot(r.reason)
			c.unlock()
			_ = c.drain(conn)
			goodbye = !r.hard
			return true, errReboot
		case <-ctx.Done():
			if c.isKilled() {
				return true, ctx.Err()
			}
			goodbye = true
			// Orderly shutdown: end the transactions and tell the CSMS
			// before leaving.
			c.mu.Lock()
			for _, k := range c.connectors {
				if k.tx != nil {
					c.stopTx(k, v16.ReasonOther, k.tx.idTag)
					c.reportStatus(k)
				}
			}
			c.unlock()
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
		if len(c.outbox) == 0 || time.Now().Before(c.retryAt) {
			c.mu.Unlock()
			return nil
		}
		m := c.outbox[0]
		c.mu.Unlock()

		// The request is only changed by this goroutine, after an answer, so
		// it can be read here without the lock.
		var result json.RawMessage
		err := c.call(context.Background(), conn, m.action, m.req, &result)
		var callErr *ocppj.CallError
		switch {
		case errors.As(err, &callErr):
			if c.retryLater(m, callErr) {
				return nil
			}
			result = nil
		case err != nil:
			return err
		case m.action == v16.ActionStartTransaction && !readableStart(result):
			// An answer that cannot be read is no better than an error.
			unreadable := &ocppj.CallError{Code: ocppj.ErrFormationViolation, Description: "its answer could not be read: " + string(result)}
			if c.retryLater(m, unreadable) {
				return nil
			}
			result = nil
		}

		c.mu.Lock()
		// A power cut may have come while the CSMS was answering.
		if len(c.outbox) > 0 && c.outbox[0] == m {
			c.outbox = c.outbox[1:]
		}
		if m.action == v16.ActionStartTransaction {
			c.startAnswered(m, result)
		}
		if !m.volatile {
			c.dirty = true
		}
		c.unlock()
	}
}

// readableStart reports whether an answer to StartTransaction carries what
// the charger needs from it.
func readableStart(result json.RawMessage) bool {
	var resp v16.StartTransactionResponse
	return json.Unmarshal(result, &resp) == nil && resp.IDTagInfo.Status != ""
}

// retryLater decides what happens to a message the CSMS answered with an
// error. Transaction messages are sent again, after a wait that grows with
// each attempt, until TransactionMessageAttempts is reached; it then reports
// true and the message keeps its place at the head of the queue, so that
// order is kept. Anything else, and a message out of attempts, is dropped.
func (c *Charger) retryLater(m *outMsg, callErr *ocppj.CallError) bool {
	c.mu.Lock()
	defer c.mu.Unlock()

	transactional := m.action == v16.ActionStartTransaction || m.action == v16.ActionStopTransaction || m.action == v16.ActionMeterValues
	if !transactional {
		c.event("the CSMS answered %s with an error: %v", m.action, callErr)
		return false
	}
	m.attempts++
	limit := max(c.configInt("TransactionMessageAttempts", 3), 1)
	if m.attempts >= limit {
		c.event("the CSMS answered %s with an error %d times: %v; the message is dropped", m.action, m.attempts, callErr)
		return false
	}
	wait := time.Duration(m.attempts) * c.configSeconds("TransactionMessageRetryInterval")
	c.retryAt = time.Now().Add(wait)
	c.event("the CSMS answered %s with an error: %v; sending it again in %s (attempt %d of %d)", m.action, callErr, wait, m.attempts+1, limit)
	return true
}

// waitForNetwork waits while the network connection is taken down. It
// reports false when ctx ends first.
func (c *Charger) waitForNetwork(ctx context.Context) bool {
	for {
		c.mu.Lock()
		down := c.netDown
		if down {
			c.connection = Offline
		}
		c.mu.Unlock()
		if !down {
			return true
		}
		select {
		case <-c.network:
		case <-ctx.Done():
			return false
		}
	}
}

// bootRequest returns the charger's BootNotification.
func (c *Charger) bootRequest() v16.BootNotificationRequest {
	m := c.cfg.Model
	return v16.BootNotificationRequest{
		ChargePointVendor: m.Vendor,
		ChargePointModel:  m.Name,
		FirmwareVersion:   m.Firmware,
	}
}

// enqueue adds a message to the outbox. The caller holds c.mu.
func (c *Charger) enqueue(m *outMsg) {
	c.outbox = append(c.outbox, m)
	if !m.volatile {
		c.dirty = true
	}
	select {
	case c.wake <- struct{}{}:
	default:
	}
}

// boot sends BootNotification until the CSMS accepts the charger, and
// returns the heartbeat interval to use.
func (c *Charger) boot(ctx context.Context, conn *ocppj.Conn) (time.Duration, error) {
	for {
		var resp v16.BootNotificationResponse
		err := c.call(ctx, conn, v16.ActionBootNotification, c.bootRequest(), &resp)
		if err != nil {
			return 0, err
		}

		interval := time.Duration(resp.Interval) * time.Second

		if resp.Status == v16.RegistrationAccepted {
			c.event("accepted by the CSMS")
			if interval <= 0 {
				c.mu.Lock()
				interval = c.configuredHeartbeatInterval()
				c.mu.Unlock()
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
