// Package raincache stores observed hourly precipitation for one station in a
// local, gitignored file (data/rain/), so observed-rain simulator runs stay
// offline and deterministic. `make eccc-import` fills it from ECCC MSC GeoMet.
package raincache

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/smhunt/sumpnet/internal/sim"
)

// Format and source facts written into every file.
const (
	Version     = 1
	Source      = "ECCC MSC GeoMet climate-hourly (api.weather.gc.ca)"
	Licence     = "Environment and Climate Change Canada Data Services End-use Licence (https://eccc-msc.github.io/open-data/licence/readme_en/)"
	Attribution = "Data Source: Environment and Climate Change Canada"
	Semantics   = "mm is PRECIP_AMOUNT for the hour ending at hour_end (UTC_DATE); hours inside a fetch window without an amount were null or flagged missing and count as dry"
)

// Hour is the precipitation in the hour ending at End.
type Hour struct {
	End time.Time `json:"hour_end"`
	MM  float64   `json:"mm"`
}

// Fetch records one import: hours ending in (From, To].
type Fetch struct {
	From      time.Time `json:"from"`
	To        time.Time `json:"to"`
	FetchedAt time.Time `json:"fetched_at"`
	URL       string    `json:"url"`
	Hours     int       `json:"hours"`
	Skipped   int       `json:"skipped"`
}

// File is one station's cached hourly precipitation.
type File struct {
	Version     int     `json:"version"`
	Source      string  `json:"source"`
	Station     string  `json:"station"`
	Licence     string  `json:"licence"`
	Attribution string  `json:"attribution"`
	Semantics   string  `json:"semantics"`
	Fetches     []Fetch `json:"fetches"`
	Hours       []Hour  `json:"hours"`
}

// New returns an empty file for station.
func New(station string) *File {
	return &File{Version: Version, Source: Source, Station: station, Licence: Licence, Attribution: Attribution, Semantics: Semantics}
}

// DefaultPath is where `make eccc-import` keeps a station's file.
func DefaultPath(station string) string {
	return filepath.Join("data", "rain", "eccc-hourly-"+station+".json")
}

// Load reads a file written by Write.
func Load(path string) (*File, error) {
	b, err := os.ReadFile(path) //nolint:gosec // operator-supplied path
	if err != nil {
		return nil, fmt.Errorf("raincache: %w (run make eccc-import FROM=… TO=…)", err)
	}
	var f File
	if err := json.Unmarshal(b, &f); err != nil {
		return nil, fmt.Errorf("raincache: %s: %w", path, err)
	}
	if f.Version != Version {
		return nil, fmt.Errorf("raincache: %s is version %d, want %d", path, f.Version, Version)
	}
	return &f, nil
}

// Merge replaces the hours ending in (from, to] with hours and records the fetch.
func (f *File) Merge(fetch Fetch, hours []Hour) error {
	if !fetch.To.After(fetch.From) {
		return errors.New("raincache: empty fetch window")
	}
	kept := f.Hours[:0:0]
	for _, h := range f.Hours {
		if !h.End.After(fetch.From) || h.End.After(fetch.To) {
			kept = append(kept, h)
		}
	}
	for _, h := range hours {
		if !h.End.After(fetch.From) || h.End.After(fetch.To) {
			return fmt.Errorf("raincache: hour ending %s is outside the fetch window", h.End.Format(time.RFC3339))
		}
		if h.MM < 0 || math.IsNaN(h.MM) {
			return fmt.Errorf("raincache: hour ending %s has %v mm", h.End.Format(time.RFC3339), h.MM)
		}
		kept = append(kept, Hour{End: h.End.UTC(), MM: h.MM})
	}
	sort.SliceStable(kept, func(i, j int) bool { return kept[i].End.Before(kept[j].End) })
	for i := 1; i < len(kept); i++ {
		if kept[i].End.Equal(kept[i-1].End) {
			return fmt.Errorf("raincache: hour ending %s appears twice", kept[i].End.Format(time.RFC3339))
		}
	}
	fetch.From, fetch.To, fetch.FetchedAt = fetch.From.UTC(), fetch.To.UTC(), fetch.FetchedAt.UTC()
	fetch.Hours = len(hours)
	f.Hours = kept
	f.Fetches = append(f.Fetches, fetch)
	return nil
}

// Covered returns an error unless the recorded fetches cover hours ending in (from, to].
func (f *File) Covered(from, to time.Time) error {
	iv := make([]Fetch, len(f.Fetches))
	copy(iv, f.Fetches)
	sort.SliceStable(iv, func(i, j int) bool { return iv[i].From.Before(iv[j].From) })
	reach := from
	for _, w := range iv {
		if w.From.After(reach) {
			break
		}
		if w.To.After(reach) {
			reach = w.To
		}
		if !reach.Before(to) {
			return nil
		}
	}
	return fmt.Errorf("raincache: station %s has hours to %s only; run make eccc-import FROM=%s TO=%s",
		f.Station, reach.Format(time.RFC3339), from.Format("2006-01-02"), to.Format("2006-01-02"))
}

// Window returns the hours ending in (from, to].
func (f *File) Window(from, to time.Time) []sim.HourlyRain {
	var out []sim.HourlyRain
	for _, h := range f.Hours {
		if h.End.After(from) && !h.End.After(to) {
			out = append(out, sim.HourlyRain{End: h.End, MM: h.MM})
		}
	}
	return out
}

// Write saves the file as indented JSON, creating its directory.
func (f *File) Write(path string) error {
	b, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return fmt.Errorf("raincache: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return fmt.Errorf("raincache: %w", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o600); err != nil {
		return fmt.Errorf("raincache: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("raincache: %w", err)
	}
	return nil
}

// ParseTime accepts a UTC date (2026-08-01, midnight UTC) or RFC 3339.
func ParseTime(s string) (time.Time, error) {
	if t, err := time.Parse("2006-01-02", s); err == nil {
		return t.UTC(), nil
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}, fmt.Errorf("want YYYY-MM-DD (UTC midnight) or RFC 3339, got %q", s)
	}
	return t.UTC(), nil
}

// DayTotal is the precipitation of one climate day.
type DayTotal struct {
	Day time.Time // the local date the climate day is named after (00:00 UTC of that date)
	MM  float64
}

// ClimateDays sums hours into ECCC climate days, which run (06Z, 06Z] and
// reproduce climate-daily TOTAL_PRECIPITATION (prompt_plan.md §11). Days
// without rain are left out; totals are rounded to 0.1 mm.
func ClimateDays(hours []sim.HourlyRain) []DayTotal {
	var out []DayTotal
	for _, h := range hours {
		day := h.End.Add(-6*time.Hour - time.Nanosecond).Truncate(24 * time.Hour)
		if n := len(out); n > 0 && out[n-1].Day.Equal(day) {
			out[n-1].MM += h.MM
		} else if h.MM > 0 {
			out = append(out, DayTotal{Day: day, MM: h.MM})
		} else {
			out = append(out, DayTotal{Day: day})
		}
	}
	kept := out[:0]
	for _, d := range out {
		if d.MM = math.Round(d.MM*10) / 10; d.MM > 0 {
			kept = append(kept, d)
		}
	}
	return kept
}
