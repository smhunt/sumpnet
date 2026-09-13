package storms

import (
	"sort"
	"time"

	"github.com/smhunt/sumpnet/internal/hydrology"
	"github.com/smhunt/sumpnet/internal/store/sqlcgen"
	"github.com/smhunt/sumpnet/internal/weather"
)

// rainSet is the rainfall storm-analytics works on: one series per station.
type rainSet struct {
	stations [][]hydrology.RainInterval
	gauge    []bool // parallel to stations: own gauge (true) or ECCC
}

// selectRain builds the station series from rainfall rows (§11: own gauges
// are primary, ECCC is the fallback). An ECCC interval is used only where no
// gauge interval overlaps it, so a window covered by any gauge — including
// its dry rows — ignores ECCC entirely, and a gap in gauge coverage (gauges
// not yet installed, offline, or a replay without gauges) falls back to it.
func selectRain(rows []sqlcgen.ListRainfallRow) rainSet {
	type key struct{ source, station string }
	byKey := map[key][]hydrology.RainInterval{}
	var keys []key
	for _, r := range rows {
		k := key{r.Source, r.StationID}
		if _, ok := byKey[k]; !ok {
			keys = append(keys, k)
		}
		byKey[k] = append(byKey[k], hydrology.RainInterval{Start: r.Ts, Dur: time.Duration(r.IntervalS) * time.Second, MM: r.Mm})
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].source != keys[j].source {
			return keys[i].source == weather.SourceGauge
		}
		return keys[i].station < keys[j].station
	})

	var cover []hydrology.RainInterval
	for _, k := range keys {
		if k.source == weather.SourceGauge {
			cover = append(cover, byKey[k]...)
		}
	}
	spans := unionSpans(cover)

	var set rainSet
	for _, k := range keys {
		iv := byKey[k]
		sort.Slice(iv, func(i, j int) bool { return iv[i].Start.Before(iv[j].Start) })
		isGauge := k.source == weather.SourceGauge
		if !isGauge {
			iv = uncovered(iv, spans)
		}
		if len(iv) == 0 {
			continue
		}
		set.stations = append(set.stations, iv)
		set.gauge = append(set.gauge, isGauge)
	}
	return set
}

type timeSpan struct{ from, to time.Time }

// unionSpans merges intervals into sorted, disjoint spans.
func unionSpans(iv []hydrology.RainInterval) []timeSpan {
	s := append([]hydrology.RainInterval(nil), iv...)
	sort.Slice(s, func(i, j int) bool { return s[i].Start.Before(s[j].Start) })
	var out []timeSpan
	for _, r := range s {
		if r.Dur <= 0 {
			continue
		}
		if n := len(out); n > 0 && !r.Start.After(out[n-1].to) {
			if r.End().After(out[n-1].to) {
				out[n-1].to = r.End()
			}
			continue
		}
		out = append(out, timeSpan{r.Start, r.End()})
	}
	return out
}

// uncovered keeps the intervals that overlap no span.
func uncovered(iv []hydrology.RainInterval, spans []timeSpan) []hydrology.RainInterval {
	var out []hydrology.RainInterval
	for _, r := range iv {
		j := sort.Search(len(spans), func(k int) bool { return spans[k].to.After(r.Start) })
		if j < len(spans) && spans[j].from.Before(r.End()) {
			continue
		}
		out = append(out, r)
	}
	return out
}

// wetExtent is the start of the first and the end of the last wet interval.
func (s rainSet) wetExtent() (first, last time.Time, ok bool) {
	for _, st := range s.stations {
		for _, r := range st {
			if !r.Wet() {
				continue
			}
			if !ok || r.Start.Before(first) {
				first = r.Start
			}
			if !ok || r.End().After(last) {
				last = r.End()
			}
			ok = true
		}
	}
	return first, last, ok
}

// flatten returns every selected interval in one slice (for "was there rain").
func (s rainSet) flatten() []hydrology.RainInterval {
	var out []hydrology.RainInterval
	for _, st := range s.stations {
		out = append(out, st...)
	}
	return out
}

// sourceFor names the storm's rain source: gauge when any own gauge saw rain
// during it, otherwise ECCC.
func (s rainSet) sourceFor(storm hydrology.Storm) string {
	for i, st := range s.stations {
		if !s.gauge[i] {
			continue
		}
		for _, r := range st {
			if r.Wet() && r.End().After(storm.FirstRain) && r.Start.Before(storm.RainEnd) {
				return weather.SourceGauge
			}
		}
	}
	return weather.SourceECCC
}
