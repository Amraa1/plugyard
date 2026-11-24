package charger

import (
	"fmt"
	"math"
	"slices"
	"time"

	"github.com/Amraa1/plugyard/internal/model"
	"github.com/Amraa1/plugyard/internal/ocpp/v16"
)

// InstalledProfile is a charging profile the CSMS installed on a connector.
type InstalledProfile struct {
	// ConnectorID is the connector the profile is on, where 0 stands for the
	// charger as a whole.
	ConnectorID int `json:"connectorId"`
	// Installed is when the profile arrived. A schedule that names no start
	// of its own starts then.
	Installed time.Time           `json:"installed"`
	Profile   v16.ChargingProfile `json:"profile"`
}

// What the charger accepts in charging profiles. The CSMS can read these as
// configuration keys.
const (
	maxStackLevel      = 10
	maxSchedulePeriods = 24
	maxProfiles        = 20
	// maxCompositeDuration is the longest composite schedule the charger
	// works out, in seconds.
	maxCompositeDuration = 7 * 24 * 3600
)

// noLimit is the limit of a connector no charging profile limits.
const noLimit = -1

// periodAt returns the schedule period of the profile that applies at t.
// relStart is the moment a Relative schedule starts from; the zero time
// stands for the moment the profile was installed.
func (p *InstalledProfile) periodAt(t, relStart time.Time) (v16.ChargingSchedulePeriod, bool) {
	profile := &p.Profile
	none := v16.ChargingSchedulePeriod{}
	if profile.ValidFrom != nil && t.Before(profile.ValidFrom.Time) {
		return none, false
	}
	if profile.ValidTo != nil && !t.Before(profile.ValidTo.Time) {
		return none, false
	}

	schedule := &profile.ChargingSchedule
	start := p.Installed
	switch {
	case profile.ChargingProfileKind == v16.KindRelative:
		if !relStart.IsZero() {
			start = relStart
		}
	case schedule.StartSchedule != nil:
		start = schedule.StartSchedule.Time
	}
	elapsed := t.Sub(start)
	if elapsed < 0 {
		return none, false
	}
	if profile.ChargingProfileKind == v16.KindRecurring {
		cycle := 24 * time.Hour
		if profile.RecurrencyKind == v16.RecurrencyWeekly {
			cycle *= 7
		}
		elapsed %= cycle
	}
	if schedule.Duration != nil && elapsed >= time.Duration(*schedule.Duration)*time.Second {
		return none, false
	}

	found := -1
	for i, period := range schedule.ChargingSchedulePeriod {
		if time.Duration(period.StartPeriod)*time.Second <= elapsed {
			found = i
		}
	}
	if found < 0 {
		return none, false
	}
	return schedule.ChargingSchedulePeriod[found], true
}

// watts turns the limit of a schedule period into power.
func (c *Charger) watts(schedule *v16.ChargingSchedule, period v16.ChargingSchedulePeriod) float64 {
	if schedule.ChargingRateUnit == v16.UnitWatts {
		return period.Limit
	}
	voltage, phases := c.cfg.Model.Supply()
	if period.NumberPhases != nil {
		phases = *period.NumberPhases
	}
	return period.Limit * voltage * float64(phases)
}

// strongest returns the limit in W set at t by the profiles of one purpose
// on one connector: among those with something to say at t, the one with the
// highest stack level decides. The caller holds c.mu.
func (c *Charger) strongest(purpose string, connectorID int, t, relStart time.Time) (float64, bool) {
	level, limit := -1, 0.0
	for i := range c.profiles {
		p := &c.profiles[i]
		if p.ConnectorID != connectorID || p.Profile.ChargingProfilePurpose != purpose || p.Profile.StackLevel <= level {
			continue
		}
		if period, ok := p.periodAt(t, relStart); ok {
			level, limit = p.Profile.StackLevel, c.watts(&p.Profile.ChargingSchedule, period)
		}
	}
	return limit, level >= 0
}

// sharers counts the connectors a limit on the whole charger is divided
// between: connector k and every other connector that is charging. The
// caller holds c.mu.
func (c *Charger) sharers(k *connector) int {
	n := 1
	for _, other := range c.connectors {
		if other != k && other.tx != nil && other.ev != nil && !other.tx.evFull {
			n++
		}
	}
	return n
}

// limitAt returns the limit in W that charging profiles put on a connector
// at t, and whether there is one.
//
// A TxProfile overrides a TxDefaultProfile, and a TxDefaultProfile of the
// connector overrides one of the whole charger. A ChargePointMaxProfile caps
// the result: its limit is shared equally by the connectors that are
// charging. The caller holds c.mu.
func (c *Charger) limitAt(k *connector, t time.Time) (float64, bool) {
	// A Relative schedule counts from the start of the transaction, or from
	// t when the question is what a transaction starting then would get.
	relStart := t
	if k.tx != nil {
		relStart = k.tx.started
	}

	limit, limited := c.strongest(v16.PurposeTx, k.id, t, relStart)
	if !limited {
		limit, limited = c.strongest(v16.PurposeTxDefault, k.id, t, relStart)
	}
	if !limited {
		limit, limited = c.strongest(v16.PurposeTxDefault, 0, t, relStart)
	}
	if whole, ok := c.strongest(v16.PurposeChargePointMax, 0, t, time.Time{}); ok {
		if share := whole / float64(c.sharers(k)); !limited || share < limit {
			limit, limited = share, true
		}
	}
	return limit, limited
}

// offeredAt returns the power in W a connector offers an EV at t. The caller
// holds c.mu.
func (c *Charger) offeredAt(k *connector, t time.Time) float64 {
	if limit, limited := c.limitAt(k, t); limited {
		return min(k.maxPower, limit)
	}
	return k.maxPower
}

// applyLimit brings the limit of a connector's transaction up to date. The
// caller holds c.mu.
func (c *Charger) applyLimit(k *connector, now time.Time) {
	if k.tx == nil {
		k.limitW, k.offeredW, k.suspended = noLimit, k.maxPower, false
		return
	}
	limit, limited := c.limitAt(k, now)
	if !limited {
		limit = noLimit
	}
	k.offeredW = k.maxPower
	if limited {
		k.offeredW = min(k.maxPower, limit)
	}
	k.suspended = k.offeredW <= 0

	if limit == k.limitW {
		return
	}
	k.limitW = limit
	if limited {
		c.event("connector %d: a charging profile limits charging to %s", k.id, powerText(limit))
	} else {
		c.event("connector %d: no charging profile limits charging any more", k.id)
	}
}

func powerText(w float64) string {
	return model.Power(math.Round(w)).String()
}

// settle accounts for the charging of every connector up to now, under the
// limits as they are. The caller holds c.mu.
func (c *Charger) settle(now time.Time) {
	for _, k := range c.connectors {
		c.advance(k, now)
	}
}

// refreshLimits has every connector follow the charging profiles as they are
// now, and report a status that changed with them.
func (c *Charger) refreshLimits() {
	c.mu.Lock()
	defer c.unlock()
	now := time.Now()
	for _, k := range c.connectors {
		c.advance(k, now)
		c.reportStatus(k)
	}
}

// checkProfile says what is wrong with a charging profile, or returns "".
func checkProfile(p *v16.ChargingProfile) string {
	switch p.ChargingProfilePurpose {
	case v16.PurposeChargePointMax, v16.PurposeTxDefault, v16.PurposeTx:
	default:
		return fmt.Sprintf("unknown purpose %q", p.ChargingProfilePurpose)
	}
	switch p.ChargingProfileKind {
	case v16.KindAbsolute, v16.KindRelative:
	case v16.KindRecurring:
		if p.RecurrencyKind != v16.RecurrencyDaily && p.RecurrencyKind != v16.RecurrencyWeekly {
			return "a Recurring profile needs a recurrencyKind of Daily or Weekly"
		}
	default:
		return fmt.Sprintf("unknown kind %q", p.ChargingProfileKind)
	}
	if p.StackLevel < 0 || p.StackLevel > maxStackLevel {
		return fmt.Sprintf("stack level %d is outside 0 to %d", p.StackLevel, maxStackLevel)
	}
	if p.ValidFrom != nil && p.ValidTo != nil && !p.ValidTo.After(p.ValidFrom.Time) {
		return "validTo is not after validFrom"
	}

	schedule := &p.ChargingSchedule
	if schedule.ChargingRateUnit != v16.UnitAmps && schedule.ChargingRateUnit != v16.UnitWatts {
		return fmt.Sprintf("unknown charging rate unit %q", schedule.ChargingRateUnit)
	}
	if schedule.Duration != nil && *schedule.Duration < 0 {
		return "the duration is negative"
	}
	periods := schedule.ChargingSchedulePeriod
	switch {
	case len(periods) == 0:
		return "the schedule has no periods"
	case len(periods) > maxSchedulePeriods:
		return fmt.Sprintf("the schedule has more than %d periods", maxSchedulePeriods)
	case periods[0].StartPeriod != 0:
		return "the first period does not start at 0"
	}
	for i, period := range periods {
		switch {
		case i > 0 && period.StartPeriod <= periods[i-1].StartPeriod:
			return "the periods are not in order"
		case period.Limit < 0 || math.IsNaN(period.Limit) || math.IsInf(period.Limit, 0):
			return "a limit is not a number of zero or more"
		case period.NumberPhases != nil && (*period.NumberPhases < 1 || *period.NumberPhases > 3):
			return "numberPhases is outside 1 to 3"
		}
	}
	return ""
}

// install puts a charging profile on a connector, in place of one with the
// same ID, or with the same purpose and stack level on that connector. It
// reports whether there was room. The caller holds c.mu.
func (c *Charger) install(connectorID int, p v16.ChargingProfile, now time.Time) bool {
	kept := slices.DeleteFunc(slices.Clone(c.profiles), func(old InstalledProfile) bool {
		return old.Profile.ChargingProfileID == p.ChargingProfileID ||
			(old.ConnectorID == connectorID &&
				old.Profile.ChargingProfilePurpose == p.ChargingProfilePurpose &&
				old.Profile.StackLevel == p.StackLevel)
	})
	if len(kept) >= maxProfiles {
		return false
	}
	c.profiles = append(kept, InstalledProfile{ConnectorID: connectorID, Installed: now, Profile: p})
	c.dirty = true
	return true
}

// dropTxProfiles removes the TxProfiles of a connector: they end with their
// transaction. The caller holds c.mu.
func (c *Charger) dropTxProfiles(connectorID int) {
	c.profiles = slices.DeleteFunc(c.profiles, func(p InstalledProfile) bool {
		return p.ConnectorID == connectorID && p.Profile.ChargingProfilePurpose == v16.PurposeTx
	})
}

// installTx gives the transaction that just started on a connector the
// TxProfile that came with its RemoteStartTransaction. The caller holds c.mu.
func (c *Charger) installTx(k *connector, p *v16.ChargingProfile) {
	if p == nil || k.tx == nil {
		return
	}
	now := time.Now()
	if !c.install(k.id, *p, now) {
		c.event("connector %d: the charging profile of the remote start was dropped, %d profiles are installed already", k.id, maxProfiles)
		return
	}
	c.event("connector %d: charging profile %d of the remote start installed", k.id, p.ChargingProfileID)
	c.applyLimit(k, now)
}

func (c *Charger) setChargingProfile(req v16.SetChargingProfileRequest) string {
	c.mu.Lock()
	defer c.unlock()

	p := req.CsChargingProfiles
	reject := func(format string, args ...any) string {
		c.event("the CSMS sent charging profile %d, which was rejected: %s", p.ChargingProfileID, fmt.Sprintf(format, args...))
		return v16.StatusRejected
	}

	var k *connector
	if req.ConnectorID != 0 {
		found, err := c.connector(req.ConnectorID)
		if err != nil {
			return reject("%v", err)
		}
		k = found
	}
	if problem := checkProfile(&p); problem != "" {
		return reject("%s", problem)
	}
	switch p.ChargingProfilePurpose {
	case v16.PurposeChargePointMax:
		if k != nil {
			return reject("a ChargePointMaxProfile belongs on connector 0")
		}
	case v16.PurposeTx:
		switch {
		case k == nil:
			return reject("a TxProfile belongs on the connector of its transaction, not on connector 0")
		case k.tx == nil:
			return reject("connector %d has no transaction for a TxProfile", k.id)
		case p.TransactionID != nil && *p.TransactionID != k.tx.id:
			return reject("transaction %d is not the one on connector %d", *p.TransactionID, k.id)
		}
	}

	// What was delivered until now is counted under the old limits.
	now := time.Now()
	c.settle(now)
	if !c.install(req.ConnectorID, p, now) {
		return reject("%d profiles are installed already", maxProfiles)
	}
	c.event("the CSMS installed charging profile %d (%s, stack level %d) on connector %d",
		p.ChargingProfileID, p.ChargingProfilePurpose, p.StackLevel, req.ConnectorID)
	c.after(c.refreshLimits)
	return v16.StatusAccepted
}

func (c *Charger) clearChargingProfile(req v16.ClearChargingProfileRequest) string {
	c.mu.Lock()
	defer c.unlock()

	now := time.Now()
	c.settle(now)
	before := len(c.profiles)
	c.profiles = slices.DeleteFunc(c.profiles, func(p InstalledProfile) bool {
		switch {
		case req.ID != nil && *req.ID != p.Profile.ChargingProfileID,
			req.ConnectorID != nil && *req.ConnectorID != p.ConnectorID,
			req.ChargingProfilePurpose != "" && req.ChargingProfilePurpose != p.Profile.ChargingProfilePurpose,
			req.StackLevel != nil && *req.StackLevel != p.Profile.StackLevel:
			return false
		}
		return true
	})
	removed := before - len(c.profiles)
	if removed == 0 {
		c.event("the CSMS asked to clear charging profiles, and none matched")
		return v16.StatusUnknown
	}
	c.dirty = true
	c.event("the CSMS cleared %d charging profile(s)", removed)
	c.after(c.refreshLimits)
	return v16.StatusAccepted
}

// compositeSchedule answers GetCompositeSchedule: the limit that results
// from every installed profile, second by second from now.
func (c *Charger) compositeSchedule(req v16.GetCompositeScheduleRequest) v16.GetCompositeScheduleResponse {
	rejected := v16.GetCompositeScheduleResponse{Status: v16.StatusRejected}
	unit := req.ChargingRateUnit
	if unit == "" {
		unit = v16.UnitWatts
	}
	if (unit != v16.UnitAmps && unit != v16.UnitWatts) || req.Duration <= 0 {
		return rejected
	}
	duration := min(req.Duration, maxCompositeDuration)

	c.mu.Lock()
	defer c.mu.Unlock()
	targets := c.connectors
	if req.ConnectorID != 0 {
		k, err := c.connector(req.ConnectorID)
		if err != nil {
			return rejected
		}
		targets = []*connector{k}
	}

	voltage, phases := c.cfg.Model.Supply()
	now := time.Now()
	start := v16.Time{Time: now}
	schedule := &v16.ChargingSchedule{Duration: &duration, StartSchedule: &start, ChargingRateUnit: unit}
	last := 0.0
	for second := 0; second < duration; second++ {
		t := now.Add(time.Duration(second) * time.Second)
		watts := 0.0
		for _, k := range targets {
			watts += c.offeredAt(k, t)
		}
		if req.ConnectorID == 0 {
			// The connectors together never get more than the whole charger.
			if whole, ok := c.strongest(v16.PurposeChargePointMax, 0, t, time.Time{}); ok {
				watts = min(watts, whole)
			}
		}

		period := v16.ChargingSchedulePeriod{StartPeriod: second, Limit: math.Round(watts*10) / 10}
		if unit == v16.UnitAmps {
			period.Limit = math.Round(watts/(voltage*float64(phases))*10) / 10
			n := phases
			period.NumberPhases = &n
		}
		if second == 0 || period.Limit != last {
			schedule.ChargingSchedulePeriod = append(schedule.ChargingSchedulePeriod, period)
			last = period.Limit
		}
	}

	id := req.ConnectorID
	return v16.GetCompositeScheduleResponse{
		Status:           v16.StatusAccepted,
		ConnectorID:      &id,
		ScheduleStart:    &start,
		ChargingSchedule: schedule,
	}
}
