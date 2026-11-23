// Package site holds a running site: a named group of chargers owned by one
// process.
package site

import (
	"sort"

	"github.com/Amraa1/plugyard/internal/charger"
)

// Site is a running site.
type Site struct {
	name     string
	chargers map[string]*charger.Charger
}

// New returns a site with the given chargers.
func New(name string, chargers ...*charger.Charger) *Site {
	s := &Site{name: name, chargers: map[string]*charger.Charger{}}
	for _, c := range chargers {
		s.chargers[c.ID()] = c
	}
	return s
}

// Name returns the site's name.
func (s *Site) Name() string { return s.name }

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
