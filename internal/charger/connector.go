package charger

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/Amraa1/plugyard/internal/ev"
	"github.com/Amraa1/plugyard/internal/model"
	"github.com/Amraa1/plugyard/internal/ocpp/v16"
)

// connector is the state of one connector. It is guarded by Charger.mu.
type connector struct {
	id       int
	maxPower float64 // W

	// status and code are the status and error code last reported to the
	// CSMS.
	status string
	code   string
	// fault is the fault the connector has, or nil.
	fault *Fault
	// inoperative says the CSMS has made the connector unavailable. scheduled
	// says it is to become so when its transaction ends.
	inoperative bool
	scheduled   bool
	// meterWh is the meter register. It only counts up and is not reset
	// between transactions.
	meterWh float64
	// powerW is the power the EV drew the last time it was advanced.
	powerW float64
	// advanced is the moment up to which the EV's charging has been
	// accounted for.
	advanced time.Time

	ev *ev.EV // nil when nothing is plugged in
	tx *transaction
	// pending is a tag that was accepted before an EV was plugged in.
	pending *pendingTag
	// finishing is set when a transaction has ended and the EV is still
	// plugged in.
	finishing bool
}

type transaction struct {
	// ref identifies the transaction inside the charger. Messages queued
	// before the CSMS has given the transaction an ID carry it instead.
	ref int
	// id is the transaction ID given by the CSMS. It is zero until the CSMS
	// has answered StartTransaction; confirmed is closed at that point.
	id        int
	confirmed chan struct{}

	idTag       string
	parentIDTag string

	// begin is the reading taken when the transaction started, for the
	// transactionData of StopTransaction.
	begin *v16.MeterValue
	// evFull is set when the EV has reached its target and the transaction
	// was left open.
	evFull     bool
	nextSample time.Time
}

type pendingTag struct {
	tag    string
	parent string
	timer  *time.Timer
}

// Snapshot is the state of a charger at one moment.
type Snapshot struct {
	ChargePointID string              `json:"chargePointId"`
	Connection    string              `json:"connection"`
	Connectors    []ConnectorSnapshot `json:"connectors"`
}

// ConnectorSnapshot is the state of a connector at one moment.
type ConnectorSnapshot struct {
	ID     int    `json:"id"`
	Status string `json:"status"`
	// TransactionID is zero when there is no transaction, or when the CSMS
	// has not confirmed it yet.
	TransactionID int `json:"transactionId,omitempty"`
	// ErrorCode is the fault the connector reports, or empty.
	ErrorCode string  `json:"errorCode,omitempty"`
	PowerW    float64 `json:"powerW"`
	MeterWh   float64 `json:"meterWh"`
	// SoC is the EV's state of charge from 0 to 1, or nil with no EV.
	SoC *float64 `json:"soc,omitempty"`
}

// Snapshot returns the charger's current state.
func (c *Charger) Snapshot() Snapshot {
	c.mu.Lock()
	defer c.mu.Unlock()

	s := Snapshot{ChargePointID: c.cfg.ID, Connection: c.connection}
	now := time.Now()
	// The charger as a whole is listed, as connector 0, only when something
	// is the matter with it.
	if c.zero.status != v16.StatusAvailable {
		zero := ConnectorSnapshot{Status: c.zero.status}
		if c.zero.fault != nil {
			zero.ErrorCode = c.zero.fault.Code
		}
		s.Connectors = append(s.Connectors, zero)
	}
	for _, k := range c.connectors {
		// Bring the reading up to this moment, not the last tick.
		c.advance(k, now)
		ks := ConnectorSnapshot{
			ID:      k.id,
			Status:  k.status,
			PowerW:  math.Round(k.powerW),
			MeterWh: math.Floor(k.meterWh),
		}
		if k.fault != nil {
			ks.ErrorCode = k.fault.Code
		}
		if k.tx != nil {
			ks.TransactionID = k.tx.id
		}
		if k.ev != nil {
			soc := math.Round(k.ev.SoC()*1000) / 1000
			ks.SoC = &soc
		}
		s.Connectors = append(s.Connectors, ks)
	}
	return s
}

func (c *Charger) connector(id int) (*connector, error) {
	for _, k := range c.connectors {
		if k.id == id {
			return k, nil
		}
	}
	return nil, fmt.Errorf("the charger has no connector %d", id)
}

// currentStatus works out the OCPP status of a connector from its state.
func (k *connector) currentStatus() string {
	switch {
	case k.fault != nil:
		return v16.StatusFaulted
	case k.tx != nil && k.tx.evFull:
		return v16.StatusSuspendedEV
	case k.tx != nil:
		return v16.StatusCharging
	case k.inoperative:
		return v16.StatusUnavailable
	case k.ev != nil && k.finishing:
		return v16.StatusFinishing
	case k.ev != nil || k.pending != nil:
		return v16.StatusPreparing
	default:
		return v16.StatusAvailable
	}
}

// reportStatus tells the CSMS when a connector's status has changed. The
// caller holds c.mu.
func (c *Charger) reportStatus(k *connector) {
	status, code := k.currentStatus(), k.errorCode()
	if status == k.status && code == k.code {
		return
	}
	k.status, k.code = status, code
	switch {
	case k.id == 0:
		c.event("the charger is %s%s", status, faultSuffix(code))
	default:
		c.event("connector %d is %s%s", k.id, status, faultSuffix(code))
	}
	// While offline nothing is queued: every connector's status is reported
	// afresh when the charger is back online.
	if c.conn != nil {
		c.enqueueStatus(k)
	}
}

// enqueueStatus queues a StatusNotification with the connector's status as
// it is now. The caller holds c.mu.
func (c *Charger) enqueueStatus(k *connector) {
	now := v16.Time{Time: time.Now()}
	req := &v16.StatusNotificationRequest{
		ConnectorID: k.id,
		ErrorCode:   k.code,
		Status:      k.status,
		Timestamp:   &now,
	}
	if k.fault != nil {
		req.Info = k.fault.Info
		req.VendorErrorCode = k.fault.VendorCode
	}
	c.enqueue(&outMsg{action: v16.ActionStatusNotification, req: req, volatile: true})
}

// errorCode returns the error code the connector reports.
func (k *connector) errorCode() string {
	if k.fault != nil {
		return k.fault.Code
	}
	return v16.ErrorCodeNoError
}

func faultSuffix(code string) string {
	if code == v16.ErrorCodeNoError {
		return ""
	}
	return " (" + code + ")"
}

// blocked says why no transaction can start on a connector, or returns "".
// The caller holds c.mu.
func (c *Charger) blocked(k *connector) string {
	switch {
	case c.zero.fault != nil:
		return "the charger has a fault (" + c.zero.fault.Code + "); clear it first"
	case k.fault != nil:
		return "the connector has a fault (" + k.fault.Code + "); clear it first"
	case k.inoperative:
		return "the CSMS has made the connector unavailable"
	}
	return ""
}

// Plug plugs an EV into a connector. It returns a description of what
// happened, or an error if the action could not be carried out.
func (c *Charger) Plug(ctx context.Context, connectorID int, params ev.Params) (string, error) {
	if err := params.Validate(); err != nil {
		return "", err
	}

	c.mu.Lock()
	if c.off {
		c.mu.Unlock()
		return "", errOff
	}
	k, err := c.connector(connectorID)
	if err != nil {
		c.mu.Unlock()
		return "", err
	}
	if k.ev != nil {
		c.mu.Unlock()
		return "", errors.New("the connector already has an EV")
	}

	k.ev = ev.New(params)
	k.finishing = false
	c.dirty = true
	outcome := fmt.Sprintf("EV plugged in (%s kWh, %.0f%%)",
		strconv.FormatFloat(params.BatteryWh/1000, 'f', -1, 64), params.SoC*100)
	c.event("connector %d: %s", k.id, outcome)

	var tx *transaction
	switch {
	case k.pending != nil:
		// A tag was accepted before the EV arrived.
		p := k.pending
		p.timer.Stop()
		k.pending = nil
		tx = c.startTx(k, p.tag, p.parent)
	case c.cfg.Model.StartOn == model.StartOnPlug && c.blocked(k) == "":
		tx = c.startTx(k, c.cfg.Model.StartTag, "")
	}
	c.reportStatus(k)
	c.unlock()

	if tx != nil {
		outcome += ", " + c.awaitStart(ctx, k, tx)
	}
	return outcome, nil
}

// Unplug removes the EV from a connector, ending its transaction if there
// is one.
func (c *Charger) Unplug(connectorID int) (string, error) {
	c.mu.Lock()
	defer c.unlock()

	if c.off {
		return "", errOff
	}
	k, err := c.connector(connectorID)
	if err != nil {
		return "", err
	}
	if k.ev == nil {
		return "", errors.New("the connector has no EV")
	}
	c.dirty = true

	outcome := "EV unplugged"
	if k.tx != nil {
		outcome += ", " + c.stopTx(k, v16.ReasonEVDisconnected, k.tx.idTag)
	}
	k.ev = nil
	k.finishing = false
	c.event("connector %d: EV unplugged", k.id)
	c.reportStatus(k)
	return outcome, nil
}

// Tap presents a tag at a connector: it stops the connector's transaction if
// the tag is entitled to, and otherwise asks the CSMS to authorize the tag
// and starts a transaction. A tag the CSMS rejects is an outcome, not an
// error.
func (c *Charger) Tap(ctx context.Context, connectorID int, tag string) (string, error) {
	tag = strings.TrimSpace(tag)
	switch {
	case tag == "":
		return "", errors.New("the tag is empty")
	case len(tag) > 20:
		return "", errors.New("the tag is longer than the 20 characters OCPP 1.6 allows")
	}

	c.mu.Lock()
	if c.off {
		c.mu.Unlock()
		return "", errOff
	}
	k, err := c.connector(connectorID)
	if err != nil {
		c.mu.Unlock()
		return "", err
	}
	running := k.tx
	switch {
	case running != nil && running.idTag == tag:
		// The tag that started the transaction stops it, with no need to
		// ask the CSMS.
		outcome := c.stopTx(k, v16.ReasonLocal, tag)
		c.reportStatus(k)
		c.unlock()
		return outcome, nil
	case running == nil && c.blocked(k) != "":
		reason := c.blocked(k)
		c.mu.Unlock()
		return "", errors.New(reason)
	case running == nil && k.finishing:
		c.mu.Unlock()
		return "", errors.New("the last transaction has ended; unplug the EV first")
	case running == nil && k.pending != nil:
		c.mu.Unlock()
		return "", errors.New("a tag has already been accepted and the connector is waiting for an EV")
	}
	c.mu.Unlock()

	info, err := c.authorize(ctx, tag)
	if err != nil {
		return "", err
	}

	c.mu.Lock()
	// The connector may have changed while the CSMS was answering.
	if k.tx != running || (running == nil && (k.finishing || k.pending != nil)) {
		c.mu.Unlock()
		return "", errors.New("the connector changed state while the tag was being authorized")
	}

	if running != nil {
		defer c.unlock()
		if info.Status != v16.AuthorizationAccepted {
			return fmt.Sprintf("tag rejected by the CSMS (%s), transaction continues", info.Status), nil
		}
		if info.ParentIDTag == "" || info.ParentIDTag != running.parentIDTag {
			c.event("connector %d: tag %s is not entitled to stop the transaction", k.id, tag)
			return "tag is not the one that started the transaction and does not share its parent, transaction continues", nil
		}
		outcome := c.stopTx(k, v16.ReasonLocal, tag)
		c.reportStatus(k)
		return outcome, nil
	}

	if info.Status != v16.AuthorizationAccepted {
		c.mu.Unlock()
		c.event("connector %d: tag %s rejected by the CSMS (%s)", k.id, tag, info.Status)
		return fmt.Sprintf("tag rejected by the CSMS (%s)", info.Status), nil
	}

	outcome, tx := c.admit(k, tag, info.ParentIDTag)
	c.reportStatus(k)
	c.unlock()
	if tx != nil {
		outcome = c.awaitStart(ctx, k, tx)
	}
	return "tag accepted, " + outcome, nil
}

// admit acts on a tag that may charge on a connector: it starts a
// transaction if an EV is plugged in, and otherwise has the connector wait
// for one. The caller holds c.mu and reports the connector's status.
func (c *Charger) admit(k *connector, tag, parent string) (string, *transaction) {
	if k.ev != nil {
		return "", c.startTx(k, tag, parent)
	}

	timeout := c.configSeconds("ConnectionTimeOut")
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	p := &pendingTag{tag: tag, parent: parent}
	p.timer = time.AfterFunc(timeout, func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		if k.pending == p {
			k.pending = nil
			c.event("connector %d: no EV was plugged in within %s, tag forgotten", k.id, timeout)
			c.reportStatus(k)
		}
	})
	k.pending = p
	c.event("connector %d: tag %s accepted, waiting for an EV", k.id, tag)
	return fmt.Sprintf("waiting %s for an EV", timeout), nil
}

// authorize finds out whether a tag may charge: from the CSMS when the
// charger is online, and otherwise from what the configuration allows a
// charger to decide by itself.
func (c *Charger) authorize(ctx context.Context, tag string) (v16.IDTagInfo, error) {
	c.mu.Lock()
	conn := c.conn
	if conn == nil {
		defer c.mu.Unlock()
		if info, ok := c.authCache[tag]; ok && c.configBool("AuthorizationCacheEnabled") && c.configBool("LocalAuthorizeOffline") {
			expired := info.ExpiryDate != nil && info.ExpiryDate.Before(time.Now())
			if info.Status == v16.AuthorizationAccepted && !expired {
				c.event("tag %s accepted from the authorization cache while offline", tag)
				return info, nil
			}
		}
		if c.configBool("AllowOfflineTxForUnknownId") {
			c.event("tag %s accepted while offline, as AllowOfflineTxForUnknownId allows", tag)
			return v16.IDTagInfo{Status: v16.AuthorizationAccepted}, nil
		}
		return v16.IDTagInfo{}, errors.New("the charger is offline, so the tag cannot be authorized")
	}
	c.mu.Unlock()

	var resp v16.AuthorizeResponse
	if err := c.call(ctx, conn, v16.ActionAuthorize, v16.AuthorizeRequest{IDTag: tag}, &resp); err != nil {
		return v16.IDTagInfo{}, fmt.Errorf("the CSMS did not answer Authorize: %w", err)
	}
	c.mu.Lock()
	c.remember(tag, resp.IDTagInfo)
	c.unlock()
	return resp.IDTagInfo, nil
}

// remember keeps what the CSMS said about a tag, if the authorization cache
// is on. The caller holds c.mu.
func (c *Charger) remember(tag string, info v16.IDTagInfo) {
	if tag == "" || info.Status == "" || !c.configBool("AuthorizationCacheEnabled") {
		return
	}
	c.authCache[tag] = info
	c.dirty = true
}

// startTx starts a transaction on a connector. The caller holds c.mu.
func (c *Charger) startTx(k *connector, tag, parent string) *transaction {
	now := time.Now()
	c.nextTxRef++
	tx := &transaction{
		ref:         c.nextTxRef,
		confirmed:   make(chan struct{}),
		idTag:       tag,
		parentIDTag: parent,
		begin:       c.sample(k, c.configMeasurands("StopTxnSampledData"), v16.ContextTransactionBegin, now),
	}
	if interval := c.configSeconds("MeterValueSampleInterval"); interval > 0 {
		tx.nextSample = now.Add(interval)
	}
	k.tx = tx
	k.finishing = false
	k.advanced = now

	c.unconfirmed[tx.ref] = tx
	c.enqueue(&outMsg{
		action: v16.ActionStartTransaction,
		req: &v16.StartTransactionRequest{
			ConnectorID: k.id,
			IDTag:       tag,
			MeterStart:  int(k.meterWh),
			Timestamp:   v16.Time{Time: now},
		},
		txRef: tx.ref,
	})
	c.event("connector %d: transaction started with tag %s", k.id, tag)
	return tx
}

// startAnswered takes in the CSMS's answer to a StartTransaction, or nil if
// it answered with an error. The caller holds c.mu.
func (c *Charger) startAnswered(m *outMsg, result json.RawMessage) {
	tx := c.unconfirmed[m.txRef]
	delete(c.unconfirmed, m.txRef)
	if tx != nil {
		defer close(tx.confirmed)
	}

	var resp v16.StartTransactionResponse
	if result == nil || json.Unmarshal(result, &resp) != nil {
		return
	}

	// Messages queued while the ID was unknown get it now.
	for _, queued := range c.outbox {
		if queued.txRef != m.txRef {
			continue
		}
		queued.txRef = 0
		switch req := queued.req.(type) {
		case *v16.StopTransactionRequest:
			req.TransactionID = resp.TransactionID
		case *v16.MeterValuesRequest:
			id := resp.TransactionID
			req.TransactionID = &id
		}
	}

	if tx == nil {
		// The transaction is from before a power loss and is already over.
		return
	}
	tx.id = resp.TransactionID
	c.remember(tx.idTag, resp.IDTagInfo)
	if resp.IDTagInfo.ParentIDTag != "" {
		tx.parentIDTag = resp.IDTagInfo.ParentIDTag
	}

	var k *connector
	for _, candidate := range c.connectors {
		if candidate.tx == tx {
			k = candidate
		}
	}
	if k == nil {
		return
	}
	c.event("connector %d: the CSMS confirmed transaction %d", k.id, tx.id)

	rejected := resp.IDTagInfo.Status != v16.AuthorizationAccepted
	if rejected && c.config["StopTransactionOnInvalidId"] != "false" {
		c.event("connector %d: the CSMS rejected tag %s at StartTransaction (%s)", k.id, tx.idTag, resp.IDTagInfo.Status)
		c.stopTx(k, v16.ReasonDeAuthorized, tx.idTag)
		c.reportStatus(k)
	}
}

// stopTx ends the connector's transaction and returns a description of what
// happened. The caller holds c.mu.
func (c *Charger) stopTx(k *connector, reason, tag string) string {
	tx := k.tx
	now := time.Now()
	// Count the energy delivered since the last tick, so that the final
	// reading is exact.
	c.advance(k, now)

	var data []v16.MeterValue
	if tx.begin != nil {
		data = append(data, *tx.begin)
		if end := c.sample(k, c.configMeasurands("StopTxnSampledData"), v16.ContextTransactionEnd, now); end != nil {
			data = append(data, *end)
		}
	}

	k.tx = nil
	k.powerW = 0
	k.finishing = k.ev != nil
	if k.scheduled {
		// The CSMS made the connector unavailable during the transaction.
		k.scheduled = false
		k.inoperative = true
	}

	m := &outMsg{
		action: v16.ActionStopTransaction,
		req: &v16.StopTransactionRequest{
			IDTag:           tag,
			MeterStop:       int(k.meterWh),
			Timestamp:       v16.Time{Time: now},
			TransactionID:   tx.id,
			Reason:          reason,
			TransactionData: data,
		},
	}
	// The CSMS may not have answered StartTransaction yet: the ID is then
	// filled in when it does.
	if tx.id == 0 && c.startPending(tx.ref) {
		m.txRef = tx.ref
	}
	c.enqueue(m)

	if tx.id != 0 {
		c.event("connector %d: transaction %d stopped (%s)", k.id, tx.id, reason)
		return fmt.Sprintf("transaction %d stopped", tx.id)
	}
	c.event("connector %d: transaction stopped (%s)", k.id, reason)
	return "transaction stopped"
}

// awaitStart waits a short while for the CSMS to confirm a transaction that
// was just started, so that the outcome can name it.
func (c *Charger) awaitStart(ctx context.Context, k *connector, tx *transaction) string {
	timer := time.NewTimer(c.cfg.Timing.CallTimeout)
	defer timer.Stop()
	select {
	case <-tx.confirmed:
	case <-ctx.Done():
	case <-timer.C:
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	switch {
	case tx.id == 0:
		return "transaction started, not yet confirmed by the CSMS"
	case k.tx != tx:
		return fmt.Sprintf("transaction %d started, then stopped", tx.id)
	default:
		return fmt.Sprintf("transaction %d started", tx.id)
	}
}

// simulate advances the EVs until ctx ends.
func (c *Charger) simulate(ctx context.Context) {
	ticker := time.NewTicker(c.cfg.Timing.Tick)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			c.tick(now)
		}
	}
}

// advance accounts for the EV's charging on a connector up to now. The
// caller holds c.mu.
func (c *Charger) advance(k *connector, now time.Time) {
	elapsed := now.Sub(k.advanced)
	k.advanced = now
	if k.tx == nil || k.ev == nil || k.tx.evFull {
		return
	}

	// Only charging runs faster with --speed; protocol timers such as the
	// sample interval stay in real time.
	simulated := time.Duration(float64(elapsed) * c.cfg.Speed)
	if simulated <= 0 {
		return
	}
	energy := k.ev.Charge(k.maxPower, simulated)
	k.meterWh += energy
	k.powerW = energy / simulated.Hours()
}

// tick advances every connector to now, and acts on what that brings: an EV
// reaching its target, or a meter sample falling due.
func (c *Charger) tick(now time.Time) {
	c.mu.Lock()
	defer c.unlock()
	if c.off || c.killed {
		return
	}

	for _, k := range c.connectors {
		tx := k.tx
		if tx == nil || k.ev == nil {
			continue
		}
		c.advance(k, now)
		// Meter readings are saved every MeterSaveInterval seconds.
		if !now.Before(c.nextSave) {
			c.dirty = true
		}

		if !tx.evFull && k.ev.Full() {
			k.powerW = 0
			c.event("connector %d: the EV reached its target", k.id)
			if c.cfg.Model.OnEvFull == model.OnEvFullSuspend {
				tx.evFull = true
				c.dirty = true
			} else {
				c.stopTx(k, v16.ReasonOther, tx.idTag)
			}
		}

		if k.tx == tx && !tx.nextSample.IsZero() && !now.Before(tx.nextSample) {
			tx.nextSample = time.Time{}
			if interval := c.configSeconds("MeterValueSampleInterval"); interval > 0 {
				tx.nextSample = now.Add(interval)
			}
			c.enqueueMeterValues(k, tx, now, v16.ContextSamplePeriodic)
		}
		c.reportStatus(k)
	}
}

// enqueueMeterValues queues a MeterValues message. tx is nil when the
// connector has no transaction. The caller holds
// c.mu.
func (c *Charger) enqueueMeterValues(k *connector, tx *transaction, now time.Time, readingContext string) {
	value := c.sample(k, c.configMeasurands("MeterValuesSampledData"), readingContext, now)
	if value == nil {
		return
	}
	req := &v16.MeterValuesRequest{ConnectorID: k.id, MeterValue: []v16.MeterValue{*value}}
	m := &outMsg{action: v16.ActionMeterValues, req: req}
	switch {
	case tx == nil:
	case tx.id != 0:
		id := tx.id
		req.TransactionID = &id
	case c.startPending(tx.ref):
		m.txRef = tx.ref
	}
	c.enqueue(m)
}

// sample reads the given measurands from a connector. It returns nil when
// there is nothing to report. The caller holds c.mu.
func (c *Charger) sample(k *connector, measurands []string, readingContext string, now time.Time) *v16.MeterValue {
	var values []v16.SampledValue
	for _, measurand := range measurands {
		switch measurand {
		case v16.MeasurandEnergy:
			values = append(values, v16.SampledValue{
				Value:     strconv.Itoa(int(k.meterWh)),
				Context:   readingContext,
				Measurand: measurand,
				Location:  "Outlet",
				Unit:      "Wh",
			})
		case v16.MeasurandPower:
			values = append(values, v16.SampledValue{
				Value:     strconv.Itoa(int(math.Round(k.powerW))),
				Context:   readingContext,
				Measurand: measurand,
				Location:  "Outlet",
				Unit:      "W",
			})
		case v16.MeasurandSoC:
			if k.ev == nil {
				continue
			}
			values = append(values, v16.SampledValue{
				Value:     strconv.Itoa(int(math.Round(k.ev.SoC() * 100))),
				Context:   readingContext,
				Measurand: measurand,
				Location:  "EV",
				Unit:      "Percent",
			})
		}
	}
	if len(values) == 0 {
		return nil
	}
	return &v16.MeterValue{Timestamp: v16.Time{Time: now}, SampledValue: values}
}
