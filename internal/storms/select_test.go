package storms

import (
	"testing"
	"time"

	"github.com/smhunt/sumpnet/internal/hydrology"
	"github.com/smhunt/sumpnet/internal/store/sqlcgen"
)

var t0 = time.Date(2026, 4, 18, 0, 0, 0, 0, time.UTC)

func rf(source, station string, at time.Duration, intervalS int32, mm float64) sqlcgen.ListRainfallRow {
	return sqlcgen.ListRainfallRow{Source: source, StationID: station, Ts: t0.Add(at), IntervalS: intervalS, Mm: mm}
}

func TestSelectRainPrefersGauges(t *testing.T) {
	h := time.Hour
	rows := []sqlcgen.ListRainfallRow{
		// ECCC first in the input: order must not matter.
		rf("eccc", "6144478", 0, 3600, 4),
		rf("eccc", "6144478", h, 3600, 2),
		rf("eccc", "6144478", 3*h, 3600, 6), // no gauge covers 03:00–04:00
		rf("gauge", "70b3d57ed1000001", 30*time.Minute, 900, 0),
		rf("gauge", "70b3d57ed1000000", 0, 1800, 1.2),
		rf("gauge", "70b3d57ed1000000", 30*time.Minute, 5400, 0), // to 02:00
	}
	set := selectRain(rows)
	if len(set.stations) != 3 || !set.gauge[0] || !set.gauge[1] || set.gauge[2] {
		t.Fatalf("stations = %+v gauge = %v", set.stations, set.gauge)
	}
	if eccc := set.stations[2]; len(eccc) != 1 || !eccc[0].Start.Equal(t0.Add(3*h)) || eccc[0].MM != 6 {
		t.Errorf("ECCC fallback = %+v, want only the uncovered 03:00 hour", eccc)
	}
	first, last, ok := set.wetExtent()
	if !ok || !first.Equal(t0) || !last.Equal(t0.Add(4*h)) {
		t.Errorf("wet extent %v %v %v", first, last, ok)
	}
	if got := len(set.flatten()); got != 4 {
		t.Errorf("flatten = %d intervals", got)
	}
	if src := set.sourceFor(hydrology.Storm{FirstRain: t0, RainEnd: t0.Add(4 * h)}); src != "gauge" {
		t.Errorf("source = %s", src)
	}
	if src := set.sourceFor(hydrology.Storm{FirstRain: t0.Add(3 * h), RainEnd: t0.Add(4 * h)}); src != "eccc" {
		t.Errorf("source = %s", src)
	}
	if empty := selectRain(nil); len(empty.stations) != 0 {
		t.Error("empty")
	}
	if _, _, ok := (rainSet{}).wetExtent(); ok {
		t.Error("empty extent")
	}
}

func TestUnionAndUncovered(t *testing.T) {
	iv := []hydrology.RainInterval{
		{Start: t0.Add(2 * time.Hour), Dur: time.Hour},
		{Start: t0, Dur: time.Hour},
		{Start: t0.Add(30 * time.Minute), Dur: time.Hour},
		{Start: t0.Add(5 * time.Hour)}, // zero length ignored
	}
	spans := unionSpans(iv)
	if len(spans) != 2 || !spans[0].to.Equal(t0.Add(90*time.Minute)) || !spans[1].from.Equal(t0.Add(2*time.Hour)) {
		t.Fatalf("spans = %+v", spans)
	}
	keep := uncovered([]hydrology.RainInterval{
		{Start: t0.Add(90 * time.Minute), Dur: 30 * time.Minute}, // touches both ends, overlaps neither
		{Start: t0.Add(80 * time.Minute), Dur: 30 * time.Minute}, // overlaps the first span
		{Start: t0.Add(4 * time.Hour), Dur: time.Hour},
	}, spans)
	if len(keep) != 2 || !keep[0].Start.Equal(t0.Add(90*time.Minute)) {
		t.Errorf("uncovered = %+v", keep)
	}
}

func TestSpanAndRound(t *testing.T) {
	var s span
	s.add(t0.Add(time.Hour), t0.Add(2*time.Hour))
	s.add(t0, t0.Add(90*time.Minute))
	s.add(t0.Add(3*time.Hour), t0.Add(3*time.Hour))
	if !s.set || !s.from.Equal(t0) || !s.to.Equal(t0.Add(3*time.Hour)) {
		t.Errorf("span = %+v", s)
	}
	if round(25.004, 2) != 25 || round(12.345678, 3) != 12.346 || optFloat(1, false, 1).Valid || optFloat(1.26, true, 1).Float64 != 1.3 {
		t.Error("rounding")
	}
}
