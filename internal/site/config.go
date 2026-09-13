// Package site builds a real-geography simulation site from the County of
// Middlesex open data portal: address points and road centrelines for a list
// of streets (a committed site config), cached raw and normalised into a
// gitignored snapshot, then turned into street segments with polygon outlines
// and homes with salted, address-free identifiers for the simulator and seed.
//
// Privacy (ADR 0005, ADR 0008): address text and per-house coordinates exist
// only in the gitignored cache and snapshot and in memory while seeding or
// simulating. Nothing built here that leaves the process (sim.Site, segment
// GeoJSON, identifiers) carries an address.
package site

import (
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"regexp"
	"sort"
	"strings"

	"github.com/smhunt/sumpnet/internal/sim"
)

//go:embed sites/*.json
var siteFS embed.FS

// DefaultMaxHomesPerSegment splits a street with more homes than this into
// contiguous blocks along its centreline.
const DefaultMaxHomesPerSegment = 40

// StreetConfig is one street of a site.
type StreetConfig struct {
	// Name is the address points' FULLSTREET, e.g. "TIMBERWALK TRAIL".
	Name string `json:"name"`
	// RoadName is the road network's FULLNAME when it differs from Name
	// (the County spells ARROWWOOD PATH as ARROWWOODPATH there).
	RoadName string `json:"road_name,omitempty"`
	// Label is the display name, e.g. "Timberwalk Trail".
	Label string `json:"label"`
	// Kind is the segment kind (standard, wooded, near_pond, high_ground);
	// empty takes the config's DefaultKind.
	Kind string `json:"kind,omitempty"`
}

// Road returns the road network name of the street.
func (s StreetConfig) Road() string {
	if s.RoadName != "" {
		return s.RoadName
	}
	return s.Name
}

// Config is a committed site definition (internal/site/sites/<name>.json).
type Config struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	// Extends names a parent config whose streets come first.
	Extends string `json:"extends,omitempty"`
	// MunCode is the County municipality code (MIDC = Middlesex Centre).
	MunCode string `json:"muncode,omitempty"`
	// Plans are the subdivision plans the streets belong to (informational).
	Plans              []string       `json:"plans,omitempty"`
	DefaultKind        string         `json:"default_kind,omitempty"`
	KindNote           string         `json:"kind_note,omitempty"`
	MaxHomesPerSegment int            `json:"max_homes_per_segment,omitempty"`
	Streets            []StreetConfig `json:"streets"`
}

var (
	siteNameRE   = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)
	streetNameRE = regexp.MustCompile(`^[A-Z0-9]+( [A-Z0-9]+)*$`)
)

// ConfigNames lists the embedded site configs, sorted.
func ConfigNames() []string {
	entries, err := fs.ReadDir(siteFS, "sites")
	if err != nil {
		return nil
	}
	var names []string
	for _, e := range entries {
		if n, ok := strings.CutSuffix(e.Name(), ".json"); ok {
			names = append(names, n)
		}
	}
	sort.Strings(names)
	return names
}

// LoadConfig returns the embedded site config name with its Extends chain
// resolved: parent streets first, the child's defaults filling what the
// parent left empty, and every street's Kind set.
func LoadConfig(name string) (Config, error) {
	return loadConfig(name, func(n string) ([]byte, error) { return siteFS.ReadFile(path.Join("sites", n+".json")) }, nil)
}

func loadConfig(name string, read func(string) ([]byte, error), seen []string) (Config, error) {
	for _, s := range seen {
		if s == name {
			return Config{}, fmt.Errorf("site: config %q extends itself (%s)", name, strings.Join(append(seen, name), " -> "))
		}
	}
	if !siteNameRE.MatchString(name) {
		return Config{}, fmt.Errorf("site: bad site name %q", name)
	}
	b, err := read(name)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return Config{}, fmt.Errorf("site: no site config %q (have %s)", name, strings.Join(ConfigNames(), ", "))
		}
		return Config{}, fmt.Errorf("site: config %q: %w", name, err)
	}
	var c Config
	if err := json.Unmarshal(b, &c); err != nil {
		return Config{}, fmt.Errorf("site: config %q: %w", name, err)
	}
	if c.Name != name {
		return Config{}, fmt.Errorf("site: config file %q names itself %q", name, c.Name)
	}
	own := c.Streets
	if c.Extends != "" {
		parent, err := loadConfig(c.Extends, read, append(seen, name))
		if err != nil {
			return Config{}, err
		}
		if c.MunCode == "" {
			c.MunCode = parent.MunCode
		}
		if c.MaxHomesPerSegment == 0 {
			c.MaxHomesPerSegment = parent.MaxHomesPerSegment
		}
		if c.DefaultKind == "" {
			c.DefaultKind = parent.DefaultKind
		}
		if c.KindNote == "" {
			c.KindNote = parent.KindNote
		}
		c.Plans = mergePlans(parent.Plans, c.Plans)
		c.Streets = append(append([]StreetConfig{}, parent.Streets...), own...)
	}
	if c.MaxHomesPerSegment == 0 {
		c.MaxHomesPerSegment = DefaultMaxHomesPerSegment
	}
	if c.DefaultKind == "" {
		c.DefaultKind = sim.SegmentStandard.String()
	}
	for i := len(c.Streets) - len(own); i < len(c.Streets); i++ {
		if c.Streets[i].Kind == "" {
			c.Streets[i].Kind = c.DefaultKind
		}
	}
	return c, c.validate()
}

func mergePlans(a, b []string) []string {
	out := append([]string{}, a...)
	for _, p := range b {
		dup := false
		for _, q := range out {
			dup = dup || p == q
		}
		if !dup {
			out = append(out, p)
		}
	}
	return out
}

func (c Config) validate() error {
	if c.MunCode == "" {
		return fmt.Errorf("site: config %q has no muncode", c.Name)
	}
	if c.MaxHomesPerSegment < 1 {
		return fmt.Errorf("site: config %q: max_homes_per_segment must be >= 1", c.Name)
	}
	if _, err := ParseKind(c.DefaultKind); err != nil {
		return fmt.Errorf("site: config %q default_kind: %w", c.Name, err)
	}
	if len(c.Streets) == 0 {
		return fmt.Errorf("site: config %q has no streets", c.Name)
	}
	names := map[string]bool{}
	slugs := map[string]bool{}
	for _, s := range c.Streets {
		if !streetNameRE.MatchString(s.Name) || !streetNameRE.MatchString(s.Road()) {
			return fmt.Errorf("site: config %q: street names must be upper-case words, got %q / %q", c.Name, s.Name, s.Road())
		}
		if names[s.Name] || slugs[slug(s.Name)] {
			return fmt.Errorf("site: config %q lists %q twice", c.Name, s.Name)
		}
		if strings.TrimSpace(s.Label) == "" {
			return fmt.Errorf("site: config %q street %q has no label", c.Name, s.Name)
		}
		if _, err := ParseKind(s.Kind); err != nil {
			return fmt.Errorf("site: config %q street %q: %w", c.Name, s.Name, err)
		}
		names[s.Name], slugs[slug(s.Name)] = true, true
	}
	return nil
}

// ParseKind maps a segment kind name to the simulator's kind.
func ParseKind(s string) (sim.SegmentKind, error) {
	for _, k := range []sim.SegmentKind{sim.SegmentStandard, sim.SegmentWooded, sim.SegmentNearPond, sim.SegmentHighGround} {
		if k.String() == s {
			return k, nil
		}
	}
	return 0, fmt.Errorf("unknown segment kind %q", s)
}

// slug turns "TIMBERWALK TRAIL" into "timberwalk-trail".
func slug(street string) string {
	return strings.ToLower(strings.Join(strings.Fields(street), "-"))
}
