package hydrology

import (
	"math"
	"testing"
	"time"
)

func at(d time.Duration) time.Time { return t0.Add(d) }

func ri(start, dur time.Duration, mm float64) RainInterval {
	return RainInterval{Start: at(start), Dur: dur, MM: mm}
}

// steady returns n consecutive intervals of width w with mm each, from start.
func steady(start, w time.Duration, n int, mm float64) []RainInterval {
	out := make([]RainInterval, n)
	for i := range out {
		out[i] = ri(start+time.Duration(i)*w, w, mm)
	}
	return out
}

func near(a, b, tol float64) bool { return math.Abs(a-b) <= tol }

func TestRainInterval(t *testing.T) {
	r := ri(0, 15*time.Minute, 0.5)
	if !r.End().Equal(at(15*time.Minute)) || !near(r.IntensityMMH(), 2, 1e-12) || !r.Wet() {
		t.Errorf("interval %+v: end %v intensity %v", r, r.End(), r.IntensityMMH())
	}
	if (RainInterval{}).IntensityMMH() != 0 || (RainInterval{Dur: time.Minute}).Wet() {
		t.Error("zero interval")
	}
}

func TestMergeRain(t *testing.T) {
	a := []RainInterval{ri(0, 5*time.Minute, 0.2), ri(5*time.Minute, 15*time.Minute, 0)}
	b := []RainInterval{ri(150*time.Second, 5*time.Minute, 0.2), ri(0, 0, 5)} // zero-length ignored
	got := MergeRain([][]RainInterval{a, b}, 5*time.Minute)
	want := []RainInterval{
		ri(0, 5*time.Minute, (0.2+0.1)/2),
		ri(5*time.Minute, 5*time.Minute, (0+0.1)/2),
		ri(10*time.Minute, 5*time.Minute, 0), // only a covers it: no dilution by b
		ri(15*time.Minute, 5*time.Minute, 0),
	}
	if len(got) != len(want) {
		t.Fatalf("got %d bins: %+v", len(got), got)
	}
	for i := range want {
		if !got[i].Start.Equal(want[i].Start) || got[i].Dur != want[i].Dur || !near(got[i].MM, want[i].MM, 1e-12) {
			t.Errorf("bin %d = %+v, want %+v", i, got[i], want[i])
		}
	}
	if len(MergeRain(nil, 0)) != 0 {
		t.Error("empty input")
	}
	// Default bin, pre-epoch-aligned start still lands on 5-minute bins.
	g := MergeRain([][]RainInterval{{ri(-7*time.Minute, time.Minute, 1)}}, 0)
	if len(g) != 1 || g[0].Start.Minute()%5 != 0 || !near(g[0].MM, 1, 1e-12) {
		t.Errorf("default bin: %+v", g)
	}
}

func TestSegmentStorms(t *testing.T) {
	h := time.Hour
	cases := []struct {
		name    string
		rain    []RainInterval
		horizon time.Duration
		want    []Storm
	}{
		{
			name:    "below 5 mm is not a storm",
			rain:    steady(0, h, 4, 1.2),
			horizon: 48 * h,
		},
		{
			name: "gap shorter than 6 h joins",
			// 3 mm over [0,1h), dry until 6h55m, 3 mm over [6h55m, 7h55m): gap 5h55m
			rain:    []RainInterval{ri(0, h, 3), ri(h, 5*h+55*time.Minute, 0), ri(6*h+55*time.Minute, h, 3)},
			horizon: 20 * h,
			want:    []Storm{{FirstRain: at(0), Onset: at(0), RainEnd: at(7*h + 55*time.Minute), TotalMM: 6, PeakMMH: 3, Closed: true}},
		},
		{
			name:    "gap of 6 h splits",
			rain:    []RainInterval{ri(0, h, 6), ri(7*h, h, 6)},
			horizon: 20 * h,
			want: []Storm{
				{FirstRain: at(0), Onset: at(0), RainEnd: at(h), TotalMM: 6, PeakMMH: 6, Closed: true},
				{FirstRain: at(7 * h), Onset: at(7 * h), RainEnd: at(8 * h), TotalMM: 6, PeakMMH: 6, Closed: true},
			},
		},
		{
			name:    "onset skips drizzle below 1 mm/h",
			rain:    append(steady(0, h, 2, 0.5), steady(2*h, h, 2, 4)...),
			horizon: 30 * h,
			want:    []Storm{{FirstRain: at(0), Onset: at(2 * h), RainEnd: at(4 * h), TotalMM: 9, PeakMMH: 4, Closed: true}},
		},
		{
			name:    "no interval reaches 1 mm/h: onset is the first rain",
			rain:    steady(3*h, h, 10, 0.9),
			horizon: 30 * h,
			want:    []Storm{{FirstRain: at(3 * h), Onset: at(3 * h), RainEnd: at(13 * h), TotalMM: 9, PeakMMH: 0.9, Closed: true}},
		},
		{
			name:    "open until the data reach 6 h past the rain",
			rain:    steady(0, 30*time.Minute, 4, 2),
			horizon: 7*h + 59*time.Minute,
			want:    []Storm{{FirstRain: at(0), Onset: at(0), RainEnd: at(2 * h), TotalMM: 8, PeakMMH: 4, Closed: false}},
		},
		{
			name:    "exactly 6 h of data after the rain closes it",
			rain:    steady(0, 30*time.Minute, 4, 2),
			horizon: 8 * h,
			want:    []Storm{{FirstRain: at(0), Onset: at(0), RainEnd: at(2 * h), TotalMM: 8, PeakMMH: 4, Closed: true}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := SegmentStorms(tc.rain, at(tc.horizon))
			if len(got) != len(tc.want) {
				t.Fatalf("got %d storms %+v, want %d", len(got), got, len(tc.want))
			}
			for i, w := range tc.want {
				g := got[i]
				if !g.FirstRain.Equal(w.FirstRain) || !g.Onset.Equal(w.Onset) || !g.RainEnd.Equal(w.RainEnd) ||
					!near(g.TotalMM, w.TotalMM, 1e-9) || !near(g.PeakMMH, w.PeakMMH, 1e-9) || g.Closed != w.Closed {
					t.Errorf("storm %d = %+v, want %+v", i, g, w)
				}
			}
		})
	}
}

func TestFindStormsTakesOnsetAndEndFromStations(t *testing.T) {
	// Two gauges tipping every 5 minutes, offset by 2.5 minutes, 30 tips each.
	a := steady(0, 5*time.Minute, 30, 0.2)
	b := steady(150*time.Second, 5*time.Minute, 30, 0.2)
	// A 15-minute interval with one tip before the storm: 0.8 mm/h, below the
	// onset threshold, but still the first rain.
	a = append([]RainInterval{ri(-15*time.Minute, 15*time.Minute, 0.2)}, a...)
	got := FindStorms([][]RainInterval{a, b}, at(24*time.Hour))
	if len(got) != 1 {
		t.Fatalf("storms = %+v", got)
	}
	s := got[0]
	if !s.Onset.Equal(at(0)) || !s.FirstRain.Equal(at(-15*time.Minute)) {
		t.Errorf("onset %v first rain %v", s.Onset, s.FirstRain)
	}
	if want := at(150*time.Minute + 150*time.Second); !s.RainEnd.Equal(want) {
		t.Errorf("rain end %v, want %v (latest station interval)", s.RainEnd, want)
	}
	// Station totals are 6.2 and 6.0 mm; bins at the edges that only one
	// station covers carry that station's depth undiluted: 0.2 + 0.15 + 29 ×
	// 0.2 + 0.1.
	if !near(s.TotalMM, 6.25, 1e-9) || !s.Closed {
		t.Errorf("total %.3f closed %v", s.TotalMM, s.Closed)
	}
}

func TestRainBetweenAndSegmentLoad(t *testing.T) {
	rain := []RainInterval{ri(0, time.Hour, 6), ri(2*time.Hour, time.Hour, 3)}
	for _, c := range []struct {
		from, to time.Duration
		want     float64
	}{
		{0, time.Hour, 6},
		{30 * time.Minute, 150 * time.Minute, 3 + 1.5},
		{time.Hour, 2 * time.Hour, 0},
		{-time.Hour, 5 * time.Hour, 9},
	} {
		if got := RainBetween(rain, at(c.from), at(c.to)); !near(got, c.want, 1e-9) {
			t.Errorf("RainBetween(%v, %v) = %v, want %v", c.from, c.to, got, c.want)
		}
	}
	if SegmentLoad(nil) != 0 || SegmentLoad([]float64{100, 200, 600}) != 300 {
		t.Error("SegmentLoad")
	}
}
