// Package sitefile reads site files. A site file declares a site: a named
// group of chargers that are started and stopped together.
package sitefile

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/Amraa1/plugyard/internal/charger"
	"github.com/Amraa1/plugyard/internal/ev"
	"github.com/Amraa1/plugyard/internal/model"
)

// DefaultName is the site file looked for when none is named.
const DefaultName = "site.yaml"

// Site is a site as declared by one site file.
type Site struct {
	Name string
	// Path is the absolute path of the site file.
	Path string
	// Speed multiplies the rate of EV charging. It is 1 unless the file
	// says otherwise.
	Speed float64
	// ConnectRate is the most new connections to open per second. Zero
	// means no limit: every charger connects at once.
	ConnectRate float64
	// EVs holds the EV presets by name.
	EVs map[string]ev.Overrides
	// Entries are the charger entries, in the order the file lists them.
	Entries []Entry
}

// Entry is one entry under "chargers": a model instantiated one or more
// times.
type Entry struct {
	// Name is the entry's name. It is used only inside the file.
	Name     string
	Chargers []Charger
}

// Charger is one charger the site will run.
type Charger struct {
	// ID is the charge point ID.
	ID string
	// Model is the charger's model, with the entry's configuration
	// overrides applied.
	Model *model.Model
	CSMS  charger.CSMS
	// EV names the EV preset used when a plug does not name one, or is
	// empty.
	EV string
}

// Chargers returns every charger of the site, in file order.
func (s *Site) Chargers() []Charger {
	var out []Charger
	for _, e := range s.Entries {
		out = append(out, e.Chargers...)
	}
	return out
}

type file struct {
	Name        string            `yaml:"name"`
	CSMS        *fileCSMS         `yaml:"csms"`
	Speed       *float64          `yaml:"speed"`
	ConnectRate *float64          `yaml:"connectRate"`
	EVs         map[string]fileEV `yaml:"evs"`
	Chargers    yaml.Node         `yaml:"chargers"`
}

type fileCSMS struct {
	URL                string `yaml:"url"`
	Password           string `yaml:"password"`
	InsecureSkipVerify *bool  `yaml:"insecureSkipVerify"`
}

type fileEV struct {
	Battery   string    `yaml:"battery"`
	SoC       string    `yaml:"soc"`
	MaxPower  string    `yaml:"maxPower"`
	TargetSoC string    `yaml:"targetSoc"`
	Curve     yaml.Node `yaml:"curve"`
}

type fileEntry struct {
	Model  string         `yaml:"model"`
	Count  *int           `yaml:"count"`
	ID     string         `yaml:"id"`
	Start  *int           `yaml:"start"`
	IDs    []string       `yaml:"ids"`
	EV     string         `yaml:"ev"`
	Config map[string]any `yaml:"config"`
	CSMS   *fileCSMS      `yaml:"csms"`
}

// Load reads and validates the site file at path, together with the
// Chargerfiles it refers to.
func Load(path string) (*Site, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(abs)
	if err != nil {
		return nil, err
	}
	site, err := Parse(data, filepath.Dir(abs), os.LookupEnv)
	if err != nil {
		return nil, fmt.Errorf("%s:\n%w", path, err)
	}
	site.Path = abs
	return site, nil
}

// Name returns the name of the site that the file at path declares, without
// checking the rest of the file. Shutting a site down needs only its name,
// and must still work when the file has since been edited into something
// invalid.
func Name(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	var f struct {
		Name string `yaml:"name"`
	}
	if err := yaml.Unmarshal(data, &f); err != nil {
		return "", fmt.Errorf("%s: %w", path, err)
	}
	name := strings.TrimSpace(f.Name)
	if name == "" {
		return "", fmt.Errorf("%s: name is required", path)
	}
	if strings.Contains(name, "$") {
		name = envRef.ReplaceAllStringFunc(name, func(ref string) string {
			if ref == "$$" {
				return "$"
			}
			parts := envRef.FindStringSubmatch(ref)
			if value, set := os.LookupEnv(parts[1]); set && value != "" {
				return value
			}
			return parts[3]
		})
	}
	return name, nil
}

// Parse reads and validates the contents of a site file. Chargerfile paths
// are taken relative to baseDir, and ${VAR} references are resolved with
// lookupEnv. When the file has several problems, the returned error lists
// all of them.
func Parse(data []byte, baseDir string, lookupEnv func(string) (string, bool)) (*Site, error) {
	var root yaml.Node
	if err := yaml.Unmarshal(data, &root); err != nil {
		return nil, err
	}
	if len(root.Content) == 0 {
		return nil, errors.New("the file is empty")
	}

	var problems []error
	problem := func(format string, args ...any) {
		problems = append(problems, fmt.Errorf(format, args...))
	}

	substitute(&root, lookupEnv, problem)
	if len(problems) > 0 {
		return nil, errors.Join(problems...)
	}

	var f file
	if err := strictDecode(root.Content[0], &f); err != nil {
		return nil, err
	}

	site := &Site{
		Name:  strings.TrimSpace(f.Name),
		Speed: 1,
		EVs:   map[string]ev.Overrides{},
	}
	if site.Name == "" {
		problem("name is required")
	}
	if f.Speed != nil {
		site.Speed = *f.Speed
		if site.Speed <= 0 {
			problem("speed must be above 0")
		}
	}
	if f.ConnectRate != nil {
		site.ConnectRate = *f.ConnectRate
		if site.ConnectRate < 0 {
			problem("connectRate cannot be negative; use 0 for no limit")
		}
	}

	for name, fe := range f.EVs {
		preset, err := parseEV(fe)
		if err != nil {
			problem("evs.%s: %v", name, err)
			continue
		}
		site.EVs[name] = preset
	}

	if f.Chargers.Kind != yaml.MappingNode || len(f.Chargers.Content) == 0 {
		problem("chargers: at least one entry is required")
	}

	models := map[string]*model.Model{}
	seen := map[string]string{} // charge point ID -> entry that made it
	for i := 0; i+1 < len(f.Chargers.Content); i += 2 {
		entryName := f.Chargers.Content[i].Value
		at := "chargers." + entryName

		var fe fileEntry
		if err := strictDecode(f.Chargers.Content[i+1], &fe); err != nil {
			problem("%s: %v", at, err)
			continue
		}
		entry := Entry{Name: entryName}

		ids, err := expandIDs(fe)
		if err != nil {
			problem("%s: %v", at, err)
		}

		var m *model.Model
		switch {
		case fe.Model == "":
			problem("%s: model is required", at)
		default:
			modelPath := fe.Model
			if !filepath.IsAbs(modelPath) {
				modelPath = filepath.Join(baseDir, modelPath)
			}
			var ok bool
			if m, ok = models[modelPath]; !ok {
				loaded, err := model.Load(modelPath)
				if err != nil {
					problem("%s: model: %v", at, err)
				}
				m = loaded
				models[modelPath] = loaded
			}
		}
		if m != nil && len(fe.Config) > 0 {
			merged, err := withConfig(m, fe.Config)
			if err != nil {
				problem("%s: %v", at, err)
			}
			m = merged
		}

		if fe.EV != "" {
			if _, ok := f.EVs[fe.EV]; !ok {
				problem("%s: ev: there is no EV preset named %q", at, fe.EV)
			}
		}

		csms, err := resolveCSMS(f.CSMS, fe.CSMS)
		if err != nil {
			problem("%s: %v", at, err)
		}

		for _, id := range ids {
			if other, dup := seen[id]; dup {
				problem("%s: charge point ID %q is already used by chargers.%s", at, id, other)
				continue
			}
			seen[id] = entryName
			entry.Chargers = append(entry.Chargers, Charger{ID: id, Model: m, CSMS: csms, EV: fe.EV})
		}
		site.Entries = append(site.Entries, entry)
	}

	if len(problems) > 0 {
		return nil, errors.Join(problems...)
	}
	return site, nil
}

// strictDecode decodes a YAML node into v and rejects fields v does not
// have, so that a misspelled setting is reported instead of ignored.
func strictDecode(node *yaml.Node, v any) error {
	data, err := yaml.Marshal(node)
	if err != nil {
		return err
	}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	return dec.Decode(v)
}

// envRef matches $$, ${VAR} and ${VAR:-default}.
var envRef = regexp.MustCompile(`\$\$|\$\{([A-Za-z_][A-Za-z0-9_]*)(:-([^}]*))?\}`)

// substitute replaces environment references in every value of the file.
// A variable that is not set and has no default is a problem: an empty
// password or URL would only fail later, and less clearly.
func substitute(node *yaml.Node, lookupEnv func(string) (string, bool), problem func(string, ...any)) {
	if node.Kind == yaml.ScalarNode && strings.Contains(node.Value, "$") {
		replaced := envRef.ReplaceAllStringFunc(node.Value, func(ref string) string {
			if ref == "$$" {
				return "$"
			}
			parts := envRef.FindStringSubmatch(ref)
			name, hasDefault, fallback := parts[1], parts[2] != "", parts[3]
			value, set := lookupEnv(name)
			switch {
			case set && (value != "" || !hasDefault):
				return value
			case hasDefault:
				return fallback
			default:
				problem("line %d: the environment variable %s is not set; set it, or give a fallback with ${%s:-value}", node.Line, name, name)
				return ""
			}
		})
		if replaced != node.Value {
			node.Value = replaced
			// Let a substituted plain value be read as a number or a
			// boolean where the file expects one.
			if node.Style == 0 {
				node.Tag = ""
			}
		}
	}
	for _, child := range node.Content {
		substitute(child, lookupEnv, problem)
	}
}

func parseEV(fe fileEV) (ev.Overrides, error) {
	var o ev.Overrides
	if fe.Battery != "" {
		v, err := ev.ParseEnergy(fe.Battery)
		if err != nil {
			return o, fmt.Errorf("battery: %w", err)
		}
		o.BatteryWh = &v
	}
	if fe.SoC != "" {
		v, err := ev.ParsePercent(fe.SoC)
		if err != nil {
			return o, fmt.Errorf("soc: %w", err)
		}
		o.SoC = &v
	}
	if fe.MaxPower != "" {
		p, err := model.ParsePower(fe.MaxPower)
		if err != nil {
			return o, fmt.Errorf("maxPower: %w", err)
		}
		v := float64(p)
		o.MaxPowerW = &v
	}
	if fe.TargetSoC != "" {
		v, err := ev.ParsePercent(fe.TargetSoC)
		if err != nil {
			return o, fmt.Errorf("targetSoc: %w", err)
		}
		o.TargetSoC = &v
	}
	if fe.Curve.Kind != 0 {
		curve, err := parseCurve(&fe.Curve)
		if err != nil {
			return o, fmt.Errorf("curve: %w", err)
		}
		o.Curve = &curve
	}

	// Check the preset on top of the built-in EV, so that a mistake such as
	// a target of 0% is caught here and not at the first plug-in.
	if err := ev.Defaults().With(o).Validate(); err != nil {
		return o, err
	}
	return o, nil
}

// parseCurve reads a charge curve: the name of a built-in one, or a list of
// [state of charge, power] pairs in percent.
func parseCurve(node *yaml.Node) (ev.Curve, error) {
	switch node.Kind {
	case yaml.ScalarNode:
		return ev.BuiltinCurve(node.Value)
	case yaml.SequenceNode:
		var points []ev.Point
		for i, item := range node.Content {
			if item.Kind != yaml.SequenceNode || len(item.Content) != 2 {
				return ev.Curve{}, fmt.Errorf("point %d: want a pair such as [80%%, 28%%]", i+1)
			}
			soc, err := ev.ParsePercent(item.Content[0].Value)
			if err != nil {
				return ev.Curve{}, fmt.Errorf("point %d: %w", i+1, err)
			}
			fraction, err := ev.ParsePercent(item.Content[1].Value)
			if err != nil {
				return ev.Curve{}, fmt.Errorf("point %d: %w", i+1, err)
			}
			points = append(points, ev.Point{SoC: soc, Fraction: fraction})
		}
		return ev.NewCurve(points)
	default:
		return ev.Curve{}, errors.New("want the name of a built-in curve or a list of points")
	}
}

// placeholder matches {n} and {n:03} in an ID template, and anything else in
// braces so that it can be reported.
var placeholder = regexp.MustCompile(`\{[^{}]*\}`)
var counter = regexp.MustCompile(`^\{n(?::0(\d+))?\}$`)

// expandIDs works out the charge point IDs of an entry from its ID template
// and count, or from its explicit list.
func expandIDs(fe fileEntry) ([]string, error) {
	if len(fe.IDs) > 0 {
		if fe.ID != "" || fe.Start != nil {
			return nil, errors.New("give either ids, or id with count, not both")
		}
		if fe.Count != nil && *fe.Count != len(fe.IDs) {
			return nil, fmt.Errorf("count is %d but ids lists %d charge point IDs; leave count out", *fe.Count, len(fe.IDs))
		}
		var ids []string
		for _, id := range fe.IDs {
			id = strings.TrimSpace(id)
			if id == "" {
				return nil, errors.New("ids contains an empty charge point ID")
			}
			ids = append(ids, id)
		}
		return ids, nil
	}

	template := strings.TrimSpace(fe.ID)
	if template == "" {
		return nil, errors.New(`id is required, for example id: "FAST-{n}"`)
	}
	count := 1
	if fe.Count != nil {
		count = *fe.Count
	}
	if count < 1 {
		return nil, errors.New("count must be 1 or more")
	}
	start := 1
	if fe.Start != nil {
		start = *fe.Start
	}
	if start < 0 {
		return nil, errors.New("start cannot be negative")
	}

	hasCounter := false
	for _, found := range placeholder.FindAllString(template, -1) {
		if !counter.MatchString(found) {
			return nil, fmt.Errorf("id: %s is not understood; use {n} for the number, or {n:03} to pad it to three digits", found)
		}
		hasCounter = true
	}
	if count > 1 && !hasCounter {
		return nil, fmt.Errorf("%d chargers need different IDs; add {n} to the template, for example %q", count, template+"-{n}")
	}

	ids := make([]string, 0, count)
	for n := start; n < start+count; n++ {
		ids = append(ids, placeholder.ReplaceAllStringFunc(template, func(found string) string {
			width := counter.FindStringSubmatch(found)[1]
			if width == "" {
				return strconv.Itoa(n)
			}
			w, _ := strconv.Atoi(width)
			return fmt.Sprintf("%0*d", w, n)
		}))
	}
	return ids, nil
}

// withConfig returns a copy of m with configuration keys overridden.
func withConfig(m *model.Model, overrides map[string]any) (*model.Model, error) {
	copied := *m
	copied.Config = make(map[string]string, len(m.Config)+len(overrides))
	for key, value := range m.Config {
		copied.Config[key] = value
	}
	for key, value := range overrides {
		switch v := value.(type) {
		case string:
			copied.Config[key] = v
		case int, int64, uint64, float64, bool:
			copied.Config[key] = fmt.Sprint(v)
		default:
			return m, fmt.Errorf("config.%s: want a single value such as a number, true/false or text", key)
		}
	}
	return &copied, nil
}

// resolveCSMS combines the site's CSMS settings with an entry's overrides.
func resolveCSMS(site, entry *fileCSMS) (charger.CSMS, error) {
	var out charger.CSMS
	for _, c := range []*fileCSMS{site, entry} {
		if c == nil {
			continue
		}
		if c.URL != "" {
			out.URL = c.URL
		}
		if c.Password != "" {
			out.Password = c.Password
		}
		if c.InsecureSkipVerify != nil {
			out.InsecureSkipVerify = *c.InsecureSkipVerify
		}
	}
	if out.URL == "" {
		return out, errors.New("no CSMS URL: set csms.url for the site or for this entry")
	}
	return out, nil
}
