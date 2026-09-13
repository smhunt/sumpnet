package sim

import (
	"context"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"
)

// testSite is a synthetic three-street site: no real addresses or coordinates.
func testSite(homesPerSegment ...int) *Site {
	s := &Site{Name: "testville"}
	kinds := []SegmentKind{SegmentWooded, SegmentStandard, SegmentNearPond}
	n := 0
	for si, count := range homesPerSegment {
		s.Segments = append(s.Segments, SiteSegment{ID: fmt.Sprintf("test-street-%d", si+1), Name: fmt.Sprintf("Test Street %d", si+1), Kind: kinds[si%len(kinds)]})
		for k := 0; k < count; k++ {
			s.Homes = append(s.Homes, SiteHome{
				HomeID:  fmt.Sprintf("00000000-0000-5000-8000-%012x", n+1),
				DevEUI:  fmt.Sprintf("5e0000000000%04x", n+1),
				DevAddr: fmt.Sprintf("0200%04x", n+1),
				Stream:  siteStreamBit | uint64(1000+n),
				Segment: si,
				Lon:     -80 + 0.001*float64(k),
				Lat:     45 + 0.002*float64(si),
			})
			n++
		}
	}
	return s
}

func runSite(t *testing.T, site *Site, scenario string, dur time.Duration, gauges int) run {
	t.Helper()
	e, err := New(Config{Seed: 42, Start: testStart, Scenario: Scenarios()[scenario], Duration: dur, Site: site, RainGauges: gauges})
	if err != nil {
		t.Fatal(err)
	}
	col := &collectSink{}
	hs := NewHashSink()
	truth, err := e.Run(context.Background(), MultiSink{col, hs})
	if err != nil {
		t.Fatal(err)
	}
	return run{truth: truth, events: col.events, hash: hs.Sum(), engine: e}
}

func TestSiteRun(t *testing.T) {
	site := testSite(4, 3, 5)
	a := runSite(t, site, "storm25", 12*time.Hour, 2)
	b := runSite(t, site, "storm25", 12*time.Hour, 2)
	if a.hash != b.hash || len(a.events) == 0 {
		t.Fatalf("site run not deterministic: %s vs %s (%d events)", a.hash, b.hash, len(a.events))
	}
	if got := len(a.truth.Homes); got != 12 {
		t.Fatalf("homes = %d, want 12", got)
	}
	for i, h := range a.truth.Homes {
		want := site.Homes[i]
		if h.HomeID != want.HomeID || h.DevEUI != want.DevEUI || h.DevAddr != want.DevAddr || h.SegmentID != site.Segments[want.Segment].ID {
			t.Errorf("home %d = %s/%s/%s/%s, want the site's identity", i, h.HomeID, h.DevEUI, h.DevAddr, h.SegmentID)
		}
	}
	for i, s := range a.truth.Segments {
		if s.ID != site.Segments[i].ID || s.Kind != site.Segments[i].Kind || len(s.HomeIndexes) != []int{4, 3, 5}[i] {
			t.Errorf("segment %d = %+v", i, s)
		}
		if s.ResponseMul != kindMultipliers[s.Kind].response {
			t.Errorf("segment %s response multiplier %.2f", s.ID, s.ResponseMul)
		}
	}
	g := a.engine.RainGauges()
	if len(g) != 2 || !strings.HasPrefix(g[0].Location, "north end of testville") || !strings.HasPrefix(g[1].Location, "south end") || g[0].Lat != 45.004 || g[1].Lat != 45 {
		t.Errorf("gauges = %+v", g)
	}
	devs := map[string]bool{}
	for _, ev := range a.events {
		devs[ev.DevEUI] = true
	}
	for _, h := range site.Homes {
		if !devs[h.DevEUI] {
			t.Errorf("no events from %s", h.DevEUI)
		}
	}
}

// A home's behaviour depends on its stream, not its index or the rest of the
// site, so removing a street leaves every other home's uplinks unchanged.
func TestSiteHomesIndependentOfOtherStreets(t *testing.T) {
	full := testSite(4, 3, 5)
	part := &Site{Name: full.Name, Segments: full.Segments[:2], Homes: full.Homes[:7]}
	a := runSite(t, full, "storm25", 10*time.Hour, 0)
	b := runSite(t, part, "storm25", 10*time.Hour, 0)
	am, bm := byHome(a.events), byHome(b.events)
	for i := 0; i < 7; i++ {
		x, y := am[i], bm[i]
		if len(x) != len(y) || len(x) == 0 {
			t.Fatalf("home %d: %d events in the full site, %d in the partial one", i, len(x), len(y))
		}
		for k := range x {
			if x[k].DedupID != y[k].DedupID || string(x[k].Payload) != string(y[k].Payload) || !x[k].Time.Equal(y[k].Time) {
				t.Fatalf("home %d event %d differs", i, k)
			}
		}
	}
}

func TestSiteConfigErrors(t *testing.T) {
	bad := func(mut func(s *Site)) *Site {
		s := testSite(3, 3)
		mut(s)
		return s
	}
	tests := []struct {
		name string
		cfg  Config
		want string
	}{
		{"homes set", Config{Homes: 6, Segments: 2, Site: testSite(3, 3)}, "leave them 0"},
		{"health overrides", Config{Site: testSite(3, 3), Scenario: Scenarios()["failing-pump"]}, "does not allow"},
		{"outage outside site", Config{Site: testSite(3, 3), Scenario: Scenarios()["outage"]}, "outside site"},
		{"no homes", Config{Site: &Site{Name: "x", Segments: []SiteSegment{{ID: "a"}}}}, "needs segments"},
		{"duplicate segment", Config{Site: bad(func(s *Site) { s.Segments[1].ID = s.Segments[0].ID })}, "duplicate id"},
		{"empty segment", Config{Site: bad(func(s *Site) {
			for i := range s.Homes {
				s.Homes[i].Segment = 0
			}
		})}, "has no homes"},
		{"segment range", Config{Site: bad(func(s *Site) { s.Homes[2].Segment = 7 })}, "out of range"},
		{"duplicate deveui", Config{Site: bad(func(s *Site) { s.Homes[1].DevEUI = s.Homes[0].DevEUI })}, "duplicate identity"},
		{"stream without top bit", Config{Site: bad(func(s *Site) { s.Homes[1].Stream = 5 })}, "top bit"},
		{"duplicate stream", Config{Site: bad(func(s *Site) { s.Homes[1].Stream = s.Homes[0].Stream })}, "top bit"},
		{"unknown kind", Config{Site: bad(func(s *Site) { s.Segments[0].Kind = 9 })}, "unknown kind"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := tt.cfg
			cfg.Seed, cfg.Start = 1, testStart
			if cfg.Scenario == nil {
				cfg.Scenario = Scenarios()["storm25"]
			}
			_, err := New(cfg)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("New: %v, want error containing %q", err, tt.want)
			}
		})
	}
}

func TestObservedRainScenario(t *testing.T) {
	from := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	to := from.Add(30 * time.Hour)
	hours := []HourlyRain{
		{End: from, MM: 9},                        // hour before the window: dropped
		{End: from.Add(3 * time.Hour), MM: 1.2},   // [2h, 3h)
		{End: from.Add(4 * time.Hour), MM: 6.0},   // [3h, 4h)
		{End: from.Add(5 * time.Hour), MM: 0.4},   // [4h, 5h)
		{End: from.Add(20 * time.Hour), MM: 2.5},  // 14 h later, 2.5 mm < 5 mm: not a storm
		{End: from.Add(31 * time.Hour), MM: 50.0}, // after the window: dropped
	}
	scn, err := ObservedRainScenario("eccc", "test", from, to, hours)
	if err != nil {
		t.Fatal(err)
	}
	if scn.Duration != 30*time.Hour || len(scn.RainMinutes) != 30*60 {
		t.Fatalf("duration %v, %d minutes", scn.Duration, len(scn.RainMinutes))
	}
	if got := scn.RainTotal(); math.Abs(got-10.1) > 1e-9 {
		t.Errorf("total %.3f mm, want 10.1", got)
	}
	for m, want := range map[int]float64{119: 0, 120: 1.2, 179: 1.2, 180: 6, 239: 6, 240: 0.4, 300: 0, 1140: 2.5, 1199: 2.5, 1200: 0} {
		if scn.RainMinutes[m] != want {
			t.Errorf("minute %d = %.2f mm/h, want %.2f", m, scn.RainMinutes[m], want)
		}
	}
	e, err := New(Config{Seed: 42, Start: from, Scenario: scn, Site: testSite(3), RainGauges: 1})
	if err != nil {
		t.Fatal(err)
	}
	truth, err := e.Run(context.Background(), NewHashSink())
	if err != nil {
		t.Fatal(err)
	}
	if len(truth.Storms) != 1 {
		t.Fatalf("storms = %d, want 1 (7.6 mm on the first morning)", len(truth.Storms))
	}
	st := truth.Storms[0]
	if math.Abs(st.TotalMm-7.6) > 1e-9 || !st.Onset.Equal(from.Add(2*time.Hour)) || !st.RainEnd.Equal(from.Add(5*time.Hour-time.Minute)) || st.PeakMmPerH != 6 {
		t.Errorf("storm = total %.3f onset %s end %s peak %.1f", st.TotalMm, st.Onset, st.RainEnd, st.PeakMmPerH)
	}
	if got := truth.RainGauges[0].TotalMm; math.Abs(got-10.0) > 0.21 {
		t.Errorf("gauge counted %.1f mm, want ≈10.1", got)
	}

	for _, tt := range []struct {
		name     string
		from, to time.Time
		hours    []HourlyRain
	}{
		{"empty window", from, from, nil},
		{"sub-minute from", from.Add(time.Second), to, nil},
		{"negative", from, to, []HourlyRain{{End: from.Add(time.Hour), MM: -1}}},
		{"duplicate hour", from, to, []HourlyRain{{End: from.Add(time.Hour), MM: 1}, {End: from.Add(time.Hour), MM: 2}}},
	} {
		if _, err := ObservedRainScenario("eccc", "", tt.from, tt.to, tt.hours); err == nil {
			t.Errorf("%s: accepted", tt.name)
		}
	}
	if err := (&Scenario{Name: "x", Duration: time.Hour, Rain: burstShape, RainMinutes: []float64{1}}).validate(); err == nil {
		t.Error("rain and rain_minutes together accepted")
	}
}
