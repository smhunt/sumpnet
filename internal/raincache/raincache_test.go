package raincache

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/smhunt/sumpnet/internal/sim"
)

func TestMergeCoverWindow(t *testing.T) {
	d := func(day, hour int) time.Time { return time.Date(2026, 8, day, hour, 0, 0, 0, time.UTC) }
	f := New("0000000")
	if err := f.Merge(Fetch{From: d(1, 0), To: d(2, 0)}, []Hour{{d(1, 3), 1.5}, {d(1, 4), 0.2}}); err != nil {
		t.Fatal(err)
	}
	// A later fetch overlapping the first replaces its hours in the overlap.
	if err := f.Merge(Fetch{From: d(1, 3), To: d(3, 0)}, []Hour{{d(1, 4), 0.4}, {d(2, 7), 2}}); err != nil {
		t.Fatal(err)
	}
	if len(f.Hours) != 3 || f.Hours[1].MM != 0.4 {
		t.Fatalf("hours %+v", f.Hours)
	}
	if err := f.Covered(d(1, 0), d(3, 0)); err != nil {
		t.Errorf("covered: %v", err)
	}
	if err := f.Covered(d(1, 0), d(3, 1)); err == nil || !strings.Contains(err.Error(), "make eccc-import") {
		t.Errorf("uncovered end: %v", err)
	}
	if err := f.Merge(Fetch{From: d(5, 0), To: d(6, 0)}, nil); err != nil {
		t.Fatal(err)
	}
	if err := f.Covered(d(2, 0), d(5, 12)); err == nil {
		t.Error("gap between fetches reported as covered")
	}
	w := f.Window(d(1, 3), d(2, 7))
	if len(w) != 2 || w[0].MM != 0.4 || w[1].MM != 2 {
		t.Errorf("window %+v", w)
	}
	if err := f.Merge(Fetch{From: d(1, 0), To: d(2, 0)}, []Hour{{d(4, 0), 1}}); err == nil {
		t.Error("hour outside the fetch accepted")
	}
	p := filepath.Join(t.TempDir(), "rain", "x.json")
	if err := f.Write(p); err != nil {
		t.Fatal(err)
	}
	g, err := Load(p)
	if err != nil || len(g.Hours) != 3 || len(g.Fetches) != 3 || g.Licence != Licence {
		t.Fatalf("load: %v %+v", err, g)
	}
}

func TestClimateDaysAndParse(t *testing.T) {
	h := func(day, hour int, mm float64) sim.HourlyRain {
		return sim.HourlyRain{End: time.Date(2026, 6, day, hour, 0, 0, 0, time.UTC), MM: mm}
	}
	days := ClimateDays([]sim.HourlyRain{h(5, 6, 1), h(5, 7, 2.3), h(6, 6, 0.5), h(6, 7, 0), h(7, 5, 0.04)})
	// (04 06Z, 05 06Z] is climate day 4; hours ending 07Z on the 5th … 06Z on the 6th are day 5.
	if len(days) != 2 || days[0].MM != 1 || !days[0].Day.Equal(time.Date(2026, 6, 4, 0, 0, 0, 0, time.UTC)) || days[1].MM != 2.8 {
		t.Errorf("days %+v", days)
	}
	if got, err := ParseTime("2026-08-01"); err != nil || !got.Equal(time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("date: %v %v", got, err)
	}
	if got, err := ParseTime("2026-08-01T05:00:00-05:00"); err != nil || got.Hour() != 10 {
		t.Errorf("rfc3339: %v %v", got, err)
	}
	if _, err := ParseTime("Aug 1"); err == nil {
		t.Error("bad time accepted")
	}
}
