// Package site holds a running site: a named group of chargers owned by one
// process.
package site

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Amraa1/plugyard/internal/charger"
	"github.com/Amraa1/plugyard/internal/ev"
)

// Site is a running site.
type Site struct {
	name     string
	chargers map[string]*charger.Charger

	// csms lists the distinct CSMS URLs the site's chargers connect to.
	csms []string
	// presets holds the EV presets by name.
	presets map[string]ev.Overrides
	// defaultEV names, per charge point ID, the preset used when a plug
	// does not name one.
	defaultEV map[string]string
}

// New returns a site with the given chargers.
func New(name string, chargers ...*charger.Charger) *Site {
	s := &Site{
		name:      name,
		chargers:  map[string]*charger.Charger{},
		presets:   map[string]ev.Overrides{},
		defaultEV: map[string]string{},
	}
	for _, c := range chargers {
		s.chargers[c.ID()] = c
	}
	return s
}

// SetCSMS records the CSMS URLs the site's chargers connect to, for display.
func (s *Site) SetCSMS(urls []string) {
	seen := map[string]bool{}
	s.csms = nil
	for _, u := range urls {
		if !seen[u] {
			seen[u] = true
			s.csms = append(s.csms, u)
		}
	}
	sort.Strings(s.csms)
}

// SetPresets gives the site its EV presets.
func (s *Site) SetPresets(presets map[string]ev.Overrides) {
	s.presets = presets
}

// SetDefaultEV names the EV preset a charger uses when a plug does not name
// one.
func (s *Site) SetDefaultEV(chargePointID, preset string) {
	if preset != "" {
		s.defaultEV[chargePointID] = preset
	}
}

// Name returns the site's name.
func (s *Site) Name() string { return s.name }

// CSMS returns the distinct CSMS URLs of the site, sorted.
func (s *Site) CSMS() []string { return s.csms }

// Chargers returns the site's chargers, sorted by charge point ID.
func (s *Site) Chargers() []*charger.Charger {
	out := make([]*charger.Charger, 0, len(s.chargers))
	for _, c := range s.chargers {
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID() < out[j].ID() })
	return out
}

// Charger returns the charger with the given charge point ID, or nil.
func (s *Site) Charger(id string) *charger.Charger {
	return s.chargers[id]
}

// EVParams works out the EV to plug into a charger. From weakest to
// strongest: the built-in EV, the charger's default preset, the preset named
// with the plug, and the parameters given with the plug.
func (s *Site) EVParams(chargePointID, preset string, given ev.Overrides) (ev.Params, error) {
	params := ev.Defaults()
	if name := s.defaultEV[chargePointID]; name != "" {
		params = params.With(s.presets[name])
	}
	if preset != "" {
		o, ok := s.presets[preset]
		if !ok {
			return params, fmt.Errorf("the site has no EV preset named %q%s", preset, s.presetHint())
		}
		params = params.With(o)
	}
	return params.With(given), nil
}

func (s *Site) presetHint() string {
	if len(s.presets) == 0 {
		return "; it defines none"
	}
	names := make([]string, 0, len(s.presets))
	for name := range s.presets {
		names = append(names, name)
	}
	sort.Strings(names)
	return "; it has " + strings.Join(names, ", ")
}

// Run powers on every charger and keeps the site running until ctx is
// cancelled, which shuts every charger down in an orderly way.
func (s *Site) Run(ctx context.Context) error {
	var wg sync.WaitGroup
	for _, c := range s.chargers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = c.Run(ctx)
		}()
	}
	wg.Wait()
	return nil
}

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
