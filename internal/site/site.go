// Package site holds a running site: a named group of chargers owned by one
// process.
package site

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Amraa1/plugyard/internal/charger"
	"github.com/Amraa1/plugyard/internal/ev"
	"github.com/Amraa1/plugyard/internal/model"
)

// Member is a charger of a site, whether powered on or not.
type Member struct {
	// ID is the charge point ID.
	ID string
	// Name is the charger's name, unique on the machine.
	Name  string
	Model *model.Model
	CSMS  charger.CSMS
	// EV names the EV preset used when a plug does not name one.
	EV string
}

// Store is where a site keeps the state of its chargers.
type Store interface {
	// LoadState returns a charger's saved state, or nil if it has none.
	LoadState(chargePointID string) (*charger.State, error)
	SaveState(chargePointID string, state charger.State) error
	// Remove deletes a charger and its state.
	Remove(chargePointID string) error
}

// Config describes a site.
type Config struct {
	Name string
	// Speed multiplies the rate of EV charging. Zero means 1.
	Speed float64
	// ConnectRate is the most new connections to open per second. Zero
	// means no limit.
	ConnectRate float64
	// Presets holds the EV presets by name.
	Presets map[string]ev.Overrides
	// Log, if set, receives the log of every charger.
	Log func(charger.Entry)
	// Diagnostics, if set, returns the log of one charger between two
	// moments, for GetDiagnostics.
	Diagnostics func(chargePointID string, from, to time.Time) ([]byte, error)
	// AllowUploads lists the hosts diagnostics may be uploaded to besides
	// the machine the CSMS is on.
	AllowUploads []string
	// UploadsInsecureSkipVerify turns off the check of the certificate of
	// an https or ftps server diagnostics are uploaded to.
	UploadsInsecureSkipVerify bool
	// Store, if set, keeps the chargers' state.
	Store  Store
	Timing charger.Timing
}

// MemberState is a member and where it stands.
type MemberState struct {
	Name string
	// Powered says whether the charger is powered on.
	Powered bool
	charger.Snapshot
}

// Site is a running site.
type Site struct {
	cfg   Config
	pacer func(ctx context.Context) error
	csms  []string

	mu      sync.Mutex
	members map[string]*member
	// ctx is the context of Run; chargers are powered under it.
	ctx context.Context
	// powered counts the chargers that are powered on, and holds the
	// operations that are about to power one on.
	powered int
	holds   int
	closing bool
	// idle is closed when the last powered charger has gone off.
	idle     chan struct{}
	idleOnce sync.Once
	wg       sync.WaitGroup
}

type member struct {
	Member
	// charger is nil while the member is powered off.
	charger *charger.Charger
	cancel  func()
	// done is closed when the charger has finished powering off.
	done chan struct{}
}

// New checks the members and returns a site that is not running yet.
func New(cfg Config, members []Member) (*Site, error) {
	s := &Site{
		cfg:     cfg,
		pacer:   NewPacer(cfg.ConnectRate),
		members: map[string]*member{},
		idle:    make(chan struct{}),
	}
	seen := map[string]bool{}
	for _, m := range members {
		if _, err := charger.New(s.chargerConfig(m, nil)); err != nil {
			return nil, fmt.Errorf("charger %s: %w", m.ID, err)
		}
		s.members[m.ID] = &member{Member: m}
		if !seen[m.CSMS.URL] {
			seen[m.CSMS.URL] = true
			s.csms = append(s.csms, m.CSMS.URL)
		}
	}
	sort.Strings(s.csms)
	return s, nil
}

func (s *Site) chargerConfig(m Member, saved *charger.State) charger.Config {
	cfg := charger.Config{
		ID:         m.ID,
		Model:      m.Model,
		CSMS:       m.CSMS,
		Speed:      s.cfg.Speed,
		Timing:     s.cfg.Timing,
		BeforeDial: s.pacer,
		Log:        s.cfg.Log,
		Restore:    saved,

		AllowUploads:              s.cfg.AllowUploads,
		UploadsInsecureSkipVerify: s.cfg.UploadsInsecureSkipVerify,
	}
	if s.cfg.Diagnostics != nil {
		id := m.ID
		cfg.Diagnostics = func(from, to time.Time) ([]byte, error) { return s.cfg.Diagnostics(id, from, to) }
	}
	if s.cfg.Store != nil {
		id := m.ID
		cfg.Save = func(state charger.State) error { return s.cfg.Store.SaveState(id, state) }
	}
	return cfg
}

// Name returns the site's name.
func (s *Site) Name() string { return s.cfg.Name }

// Speed returns how many times faster than real time the site's EVs charge.
func (s *Site) Speed() float64 {
	if s.cfg.Speed <= 0 {
		return 1
	}
	return s.cfg.Speed
}

// CSMS returns the distinct CSMS URLs of the site, sorted.
func (s *Site) CSMS() []string { return s.csms }

// Len returns the number of chargers in the site, powered on or not.
func (s *Site) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.members)
}

// Powered returns the number of chargers that are powered on.
func (s *Site) Powered() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.powered
}

// Charger returns the charger with the given charge point ID if it is
// powered on, and nil otherwise.
func (s *Site) Charger(id string) *charger.Charger {
	s.mu.Lock()
	defer s.mu.Unlock()
	if m := s.members[id]; m != nil {
		return m.charger
	}
	return nil
}

// Has reports whether the site has a charger with the given charge point ID.
func (s *Site) Has(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.members[id] != nil
}

// States returns where every charger of the site stands, sorted by charge
// point ID.
func (s *Site) States() []MemberState {
	type entry struct {
		m Member
		c *charger.Charger
	}
	s.mu.Lock()
	entries := make([]entry, 0, len(s.members))
	for _, m := range s.members {
		entries = append(entries, entry{m.Member, m.charger})
	}
	s.mu.Unlock()
	sort.Slice(entries, func(i, j int) bool { return entries[i].m.ID < entries[j].m.ID })

	out := make([]MemberState, 0, len(entries))
	for _, e := range entries {
		state := MemberState{Name: e.m.Name}
		if e.c != nil {
			state.Powered = true
			state.Snapshot = e.c.Snapshot()
		} else {
			var saved *charger.State
			if s.cfg.Store != nil {
				saved, _ = s.cfg.Store.LoadState(e.m.ID)
			}
			state.Snapshot = saved.Snapshot(e.m.ID, e.m.Model)
		}
		out = append(out, state)
	}
	return out
}

// Inspect returns a charger of the site with its state: as it is now when
// the charger is powered on, and as it was saved otherwise. The state is nil
// for a charger that has never run.
func (s *Site) Inspect(id string) (Member, *charger.State, bool) {
	s.mu.Lock()
	m := s.members[id]
	if m == nil {
		s.mu.Unlock()
		return Member{}, nil, false
	}
	member, c := m.Member, m.charger
	s.mu.Unlock()

	if c != nil {
		state := c.State()
		return member, &state, true
	}
	var saved *charger.State
	if s.cfg.Store != nil {
		saved, _ = s.cfg.Store.LoadState(id)
	}
	return member, saved, true
}

// EVParams works out the EV to plug into a charger. From weakest to
// strongest: the built-in EV, the charger's default preset, the preset named
// with the plug, and the parameters given with the plug.
func (s *Site) EVParams(chargePointID, preset string, given ev.Overrides) (ev.Params, error) {
	params := ev.Defaults()
	s.mu.Lock()
	m := s.members[chargePointID]
	s.mu.Unlock()
	if m != nil && m.EV != "" {
		params = params.With(s.cfg.Presets[m.EV])
	}
	if preset != "" {
		o, ok := s.cfg.Presets[preset]
		if !ok {
			return params, fmt.Errorf("the site has no EV preset named %q%s", preset, s.presetHint())
		}
		params = params.With(o)
	}
	return params.With(given), nil
}

func (s *Site) presetHint() string {
	if len(s.cfg.Presets) == 0 {
		return "; it defines none"
	}
	names := make([]string, 0, len(s.cfg.Presets))
	for name := range s.cfg.Presets {
		names = append(names, name)
	}
	sort.Strings(names)
	return "; it has " + strings.Join(names, ", ")
}

// Run powers on the chargers for which powerOn reports true (all of them
// when it is nil) and keeps the site running until ctx is cancelled, which
// shuts every charger down in an orderly way, or until the last powered
// charger has been stopped.
func (s *Site) Run(ctx context.Context, powerOn func(chargePointID string) bool) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	s.mu.Lock()
	s.ctx = ctx
	var failed error
	for _, m := range s.members {
		if powerOn != nil && !powerOn(m.ID) {
			continue
		}
		if err := s.power(m); err != nil {
			failed = fmt.Errorf("charger %s: %w", m.ID, err)
			break
		}
	}
	nothing := s.powered == 0
	s.mu.Unlock()

	if failed == nil && !nothing {
		select {
		case <-ctx.Done():
		case <-s.idle:
		}
	}
	cancel()
	// From here on nothing may be powered on, so that the wait below sees
	// every charger.
	s.mu.Lock()
	s.closing = true
	s.mu.Unlock()
	s.wg.Wait()
	return failed
}

// power powers a member on. The caller holds s.mu.
func (s *Site) power(m *member) error {
	if s.ctx == nil || s.closing || s.ctx.Err() != nil {
		return errors.New("the site is shutting down")
	}
	var saved *charger.State
	if s.cfg.Store != nil {
		var err error
		if saved, err = s.cfg.Store.LoadState(m.ID); err != nil {
			return fmt.Errorf("cannot read its saved state: %w", err)
		}
	}
	c, err := charger.New(s.chargerConfig(m.Member, saved))
	if err != nil {
		return err
	}

	ctx, cancel := context.WithCancel(s.ctx)
	done := make(chan struct{})
	m.charger, m.cancel, m.done = c, cancel, done
	s.powered++
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		_ = c.Run(ctx)
		cancel()

		s.mu.Lock()
		if m.charger == c {
			m.charger = nil
		}
		s.powered--
		s.checkIdle()
		s.mu.Unlock()
		close(done)
	}()
	return nil
}

// checkIdle notes when nothing is powered any more. The caller holds s.mu.
func (s *Site) checkIdle() {
	if s.powered == 0 && s.holds == 0 {
		s.idleOnce.Do(func() { close(s.idle) })
	}
}

// Start powers a charger on.
func (s *Site) Start(id string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m := s.members[id]
	switch {
	case m == nil:
		return "", errNoCharger
	case m.charger != nil:
		return "already running", nil
	}
	if err := s.power(m); err != nil {
		return "", err
	}
	return "started", nil
}

// Stop shuts a charger down in an orderly way and waits until it is off.
func (s *Site) Stop(id string) (string, error) {
	s.mu.Lock()
	m := s.members[id]
	if m == nil {
		s.mu.Unlock()
		return "", errNoCharger
	}
	if m.charger == nil {
		s.mu.Unlock()
		return "already stopped", nil
	}
	cancel, done := m.cancel, m.done
	s.mu.Unlock()

	cancel()
	<-done
	return "stopped", nil
}

// Kill cuts a charger's power.
func (s *Site) Kill(id string) (string, error) {
	s.mu.Lock()
	m := s.members[id]
	if m == nil {
		s.mu.Unlock()
		return "", errNoCharger
	}
	if m.charger == nil {
		s.mu.Unlock()
		return "", errors.New("the charger is not running")
	}
	c, done := m.charger, m.done
	s.mu.Unlock()

	c.Kill()
	<-done
	return "killed", nil
}

// Restart stops a charger and starts it again.
func (s *Site) Restart(id string) (string, error) {
	// Keep the site alive while its only charger is off for a moment.
	s.mu.Lock()
	s.holds++
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.holds--
		s.checkIdle()
		s.mu.Unlock()
	}()

	if _, err := s.Stop(id); err != nil {
		return "", err
	}
	if _, err := s.Start(id); err != nil {
		return "", err
	}
	return "restarted", nil
}

// Remove takes a charger out of the site and wipes its saved state. A
// charger that is powered on is refused unless force is set, which stops it
// first.
func (s *Site) Remove(id string, force bool) (string, error) {
	s.mu.Lock()
	m := s.members[id]
	if m == nil {
		s.mu.Unlock()
		return "", errNoCharger
	}
	if m.charger != nil && !force {
		s.mu.Unlock()
		return "", errors.New("the charger is running; stop it first, or use -f")
	}
	s.mu.Unlock()

	if _, err := s.Stop(id); err != nil {
		return "", err
	}

	s.mu.Lock()
	delete(s.members, id)
	s.mu.Unlock()
	if s.cfg.Store != nil {
		if err := s.cfg.Store.Remove(id); err != nil {
			return "", err
		}
	}
	return "removed", nil
}

var errNoCharger = errors.New("no such charger in this site")

// NewPacer returns a function that spaces out connection attempts to at most
// perSecond a second across every charger that shares it. It returns nil
// when perSecond is zero, meaning no limit.
func NewPacer(perSecond float64) func(ctx context.Context) error {
	if perSecond <= 0 {
		return nil
	}
	interval := time.Duration(float64(time.Second) / perSecond)
	var mu sync.Mutex
	var next time.Time

	return func(ctx context.Context) error {
		mu.Lock()
		now := time.Now()
		if next.Before(now) {
			next = now
		}
		wait := next.Sub(now)
		next = next.Add(interval)
		mu.Unlock()

		if wait <= 0 {
			return nil
		}
		timer := time.NewTimer(wait)
		defer timer.Stop()
		select {
		case <-timer.C:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}
