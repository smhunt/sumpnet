package sim

import (
	"context"
	"fmt"
	"math"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/smhunt/sumpnet/internal/codec"
)

var testStart = time.Date(2026, 4, 15, 0, 0, 0, 0, time.UTC)

// collectSink keeps every event in memory.
type collectSink struct{ events []Event }

func (c *collectSink) Publish(_ context.Context, ev Event) error {
	c.events = append(c.events, ev)
	return nil
}
func (c *collectSink) Close(context.Context) error { return nil }

type run struct {
	truth  *Truth
	events []Event
	hash   string
	engine *Engine
}

// Runs are deterministic, so identical requests are memoised across tests
// (the race detector makes each 60-home day cost several seconds).
var (
	runsMu sync.Mutex
	runs   = map[string]run{}
)

func runScenario(t testing.TB, name string, seed uint64, homes, segments int, dur time.Duration) run {
	t.Helper()
	key := fmt.Sprintf("%s/%d/%d/%d/%s", name, seed, homes, segments, dur)
	runsMu.Lock()
	defer runsMu.Unlock()
	if r, ok := runs[key]; ok {
		return r
	}
	r := runScenarioUncached(t, name, seed, homes, segments, dur)
	runs[key] = r
	return r
}

func runScenarioUncached(t testing.TB, name string, seed uint64, homes, segments int, dur time.Duration) run {
	t.Helper()
	scn, ok := Scenarios()[name]
	if !ok {
		t.Fatalf("no scenario %q", name)
	}
	e, err := New(Config{Seed: seed, Start: testStart, Homes: homes, Segments: segments, Scenario: scn, Duration: dur})
	if err != nil {
		t.Fatal(err)
	}
	col := &collectSink{}
	hs := NewHashSink()
	truth, err := e.Run(context.Background(), MultiSink{col, hs})
	if err != nil {
		t.Fatal(err)
	}
	if !truth.Completed {
		t.Fatal("run did not complete")
	}
	return run{truth: truth, events: col.events, hash: hs.Sum(), engine: e}
}

func decode(t testing.TB, ev Event) codec.Uplink {
	t.Helper()
	u, err := codec.Decode(ev.FPort, ev.Payload)
	if err != nil {
		t.Fatalf("event seq %d: %v", ev.Seq, err)
	}
	return u
}

func byHome(events []Event) map[int][]Event {
	m := map[int][]Event{}
	for _, ev := range events {
		m[ev.HomeIndex] = append(m[ev.HomeIndex], ev)
	}
	return m
}

func TestDeterminism(t *testing.T) {
	a := runScenario(t, "storm50", 42, 24, 8, 0)
	b := runScenarioUncached(t, "storm50", 42, 24, 8, 0)
	if a.hash != b.hash || len(a.events) != len(b.events) {
		t.Fatalf("same seed differs: %s (%d events) vs %s (%d events)", a.hash, len(a.events), b.hash, len(b.events))
	}
	if a.truth.Events == 0 || a.truth.Events != uint64(len(a.events)) {
		t.Fatalf("truth.Events = %d, events = %d", a.truth.Events, len(a.events))
	}
	c := runScenario(t, "storm50", 43, 24, 8, 0)
	if c.hash == a.hash {
		t.Fatal("different seed produced the same hash")
	}
	t.Logf("storm50 seed 42: %d events, hash %s", len(a.events), a.hash)
}

func TestAddingHomesDoesNotPerturb(t *testing.T) {
	a := runScenario(t, "storm25", 7, 12, 4, 0)
	b := runScenario(t, "storm25", 7, 13, 4, 0)
	am, bm := byHome(a.events), byHome(b.events)
	for i := 0; i < 12; i++ {
		x, y := am[i], bm[i]
		if len(x) != len(y) {
			t.Fatalf("home %d: %d events with 12 homes, %d with 13", i, len(x), len(y))
		}
		for k := range x {
			x[k].Seq, y[k].Seq = 0, 0
			if x[k].DedupID != y[k].DedupID || x[k].FCnt != y[k].FCnt || string(x[k].Payload) != string(y[k].Payload) || !x[k].Time.Equal(y[k].Time) {
				t.Fatalf("home %d event %d differs: %+v vs %+v", i, k, x[k], y[k])
			}
		}
	}
	if len(bm[12]) == 0 {
		t.Fatal("home 12 produced no events")
	}
}

func TestDryWeekBaseflow(t *testing.T) {
	const days = 3
	r := runScenario(t, "dry-week", 3, 8, 4, days*24*time.Hour)
	hm := byHome(r.events)
	for _, p := range r.truth.Homes {
		var cycles, summaries, alarms int
		for _, ev := range hm[p.Index] {
			switch ev.FPort {
			case codec.PortCycleEvent:
				cycles++
			case codec.PortStormSummary:
				summaries++
			case codec.PortAlarm:
				alarms++
			}
		}
		perDay := float64(cycles) / days
		// ±6% plus one cycle of slack for the integer count.
		if math.Abs(perDay-p.BaseflowCPD) > 0.06*p.BaseflowCPD+1.0/days {
			t.Errorf("home %d: %.2f cycles/day observed, baseflow %.2f", p.Index, perDay, p.BaseflowCPD)
		}
		if summaries != 0 || alarms != 0 {
			t.Errorf("home %d: %d storm summaries, %d alarms in dry weather", p.Index, summaries, alarms)
		}
	}
	if len(r.truth.Storms) != 0 {
		t.Errorf("dry week detected %d storms", len(r.truth.Storms))
	}
}

func TestStorm50(t *testing.T) {
	r := runScenario(t, "storm50", 42, 24, 8, 0)
	if len(r.truth.Storms) != 1 {
		t.Fatalf("found %d storms, want 1", len(r.truth.Storms))
	}
	st := r.truth.Storms[0]
	if math.Abs(st.TotalMm-50) > 3 {
		t.Errorf("storm total %.1f mm, want ≈50", st.TotalMm)
	}
	if got := st.Onset.Sub(testStart); got < 6*time.Hour || got > 6*time.Hour+15*time.Minute {
		t.Errorf("onset at %v, want ≈6h", got)
	}

	// Inflow starts one delay after the FIRST rain, which precedes §10's onset
	// (>= 1 mm/h) while the storm ramps up; lag is measured from onset.
	var firstRain time.Time
	for _, s := range r.truth.Rainfall {
		if s.Mm > 0 {
			firstRain = s.TS
			break
		}
	}
	rampMin := st.Onset.Sub(firstRain).Minutes()

	var total, stormMode int
	hm := byHome(r.events)
	for i, p := range r.truth.Homes {
		ht := st.Homes[i]
		if ht.EnteredStormMode {
			stormMode++
		}
		lo, hi := p.DelayMin-rampMin-1, p.DelayMin+p.ReservoirMin+60
		if ht.LagMin < 0 || ht.LagMin < lo || ht.LagMin > hi {
			t.Errorf("home %d: lag %.0f min not in [%.0f, %.0f]", i, ht.LagMin, lo, hi)
		}
		if ht.RecessionMin == 0 {
			t.Errorf("home %d: recession 0 min", i)
		}
		if ht.VolumeL <= 0 || ht.PeakRateCPD <= 2*p.BaseflowCPD {
			t.Errorf("home %d: volume %.0f L, peak rate %.1f cpd (baseflow %.1f)", i, ht.VolumeL, ht.PeakRateCPD, p.BaseflowCPD)
		}
		var dryHours6, stormCycles int
		for _, ev := range hm[i] {
			if ev.FPort == codec.PortCycleEvent && ev.Time.Before(st.Onset) {
				dryHours6++
			}
		}
		for _, c := range r.truth.TrueCycles {
			if c.HomeIndex == i && !c.StartedAt.Before(st.Onset) {
				stormCycles++
			}
		}
		if stormCycles <= 2*dryHours6*3 { // 6 dry hours → 18 h post-onset at the dry rate, ×2
			t.Errorf("home %d: %d cycles after onset vs %d in the 6 dry hours", i, stormCycles, dryHours6)
		}
	}
	total = len(r.truth.TrueCycles)
	if total < 24*40 {
		t.Errorf("only %d true cycles for 24 homes, want ≥ 960", total)
	}
	if stormMode < 4 {
		t.Errorf("only %d homes entered storm mode, want ≥ 4", stormMode)
	}
	t.Logf("storm50: %d true cycles, %d homes in storm mode, %d events", total, stormMode, len(r.events))
}

func TestStormModeInvariant(t *testing.T) {
	r := runScenario(t, "storm50", 42, 24, 8, 0)
	end := testStart.Add(r.engine.Duration())
	hm := byHome(r.events)
	for i := range r.truth.Homes {
		var times []time.Time
		var sent, summarised int
		for _, ev := range hm[i] {
			switch ev.FPort {
			case codec.PortCycleEvent:
				sent++
				// The firmware rule counts cycle ends; the event is sent 0-5 s later.
				c := decode(t, ev).(*codec.CycleEvent)
				times = append(times, ev.Time.Add(-time.Duration(c.StartOffsetS-c.RunS)*time.Second))
			case codec.PortStormSummary:
				summarised += int(decode(t, ev).(*codec.StormSummary).Count)
			}
		}
		sort.Slice(times, func(a, b int) bool { return times[a].Before(times[b]) })
		for a := range times {
			n := 0
			for b := a; b < len(times) && times[b].Sub(times[a]) < stormWindow*time.Second; b++ {
				n++
			}
			if n > stormThreshold {
				t.Errorf("home %d: %d fPort 2 events within 900 s starting %v", i, n, times[a])
				break
			}
		}
		var truth, pending int
		for _, c := range r.truth.TrueCycles {
			if c.HomeIndex != i {
				continue
			}
			truth++
			if end.Sub(c.StartedAt.Add(time.Duration(c.RunS)*time.Second)) < stormWindow*time.Second {
				pending++
			}
		}
		if sent+summarised > truth || truth-(sent+summarised) > pending {
			t.Errorf("home %d: %d sent + %d summarised vs %d true cycles (%d may be pending)", i, sent, summarised, truth, pending)
		}
	}
}

func TestOutage(t *testing.T) {
	r := runScenario(t, "outage", 11, 24, 8, 0)
	out := r.engine.cfg.Scenario.Outages[0]
	from := testStart.Add(out.At)
	to := from.Add(out.Duration)
	affected := map[int]bool{}
	for _, s := range r.truth.Segments {
		for _, si := range out.SegmentIndexes {
			if s.Index == si {
				for _, h := range s.HomeIndexes {
					affected[h] = true
				}
			}
		}
	}
	if len(affected) < 6 {
		t.Fatalf("only %d homes affected", len(affected))
	}
	hm := byHome(r.events)
	floatHigh := 0
	for i, p := range r.truth.Homes {
		var mainsLost bool
		for _, ev := range hm[i] {
			u := decode(t, ev)
			switch v := u.(type) {
			case *codec.Alarm:
				if v.Code == codec.AlarmMainsLost {
					if !affected[i] {
						t.Errorf("home %d outside the outage raised mains_lost", i)
					}
					if d := ev.Time.Sub(from); d < 0 || d > time.Minute {
						t.Errorf("home %d: mains_lost at %v after outage start", i, d)
					}
					mainsLost = true
				}
				if v.Code == codec.AlarmFloatHigh && affected[i] && !p.HasBackup && ev.Time.After(from) && ev.Time.Before(to) {
					floatHigh++
				}
			case *codec.Heartbeat:
				inOutage := affected[i] && ev.Time.After(from) && ev.Time.Before(to)
				if inOutage && v.Flags.Has(codec.FlagMainsOK) {
					t.Errorf("home %d: heartbeat at %v reports mains_ok during outage", i, ev.Time)
				}
				if !inOutage && !v.Flags.Has(codec.FlagMainsOK) {
					t.Errorf("home %d: heartbeat at %v reports mains lost outside outage", i, ev.Time)
				}
			}
		}
		if affected[i] && !mainsLost {
			t.Errorf("home %d in outage never raised mains_lost", i)
		}
	}
	if floatHigh == 0 {
		t.Error("no non-backup home raised float_high during the outage")
	}
}

func TestFailingPump(t *testing.T) {
	r := runScenario(t, "failing-pump", 5, 12, 4, 4*24*time.Hour)
	hm := byHome(r.events)

	// Home 9 dry-runs: dry_run alarms and cycle events with run >= 30 s and < 5 mm drop.
	var dryAlarms, dryCycles int
	for _, ev := range hm[9] {
		switch v := decode(t, ev).(type) {
		case *codec.Alarm:
			if v.Code == codec.AlarmDryRun {
				dryAlarms++
			}
		case *codec.CycleEvent:
			if v.RunS >= dryRunMinS && int(v.LevelEndMM)-int(v.LevelStartMM) < dryRunMaxDropMM {
				dryCycles++
			}
		}
	}
	if dryAlarms == 0 || dryCycles == 0 {
		t.Errorf("home 9: %d dry_run alarms, %d dry cycle events", dryAlarms, dryCycles)
	}

	// Home 11 short-cycles: at least 5 consecutive true cycles < 60 s apart.
	var ends []time.Time
	for _, c := range r.truth.TrueCycles {
		if c.HomeIndex == 11 && c.PumpID == 0 {
			ends = append(ends, c.StartedAt.Add(time.Duration(c.RunS)*time.Second))
		}
	}
	runLen, best := 1, 1
	for i := 1; i < len(ends); i++ {
		if ends[i].Sub(ends[i-1]) < 60*time.Second {
			runLen++
		} else {
			runLen = 1
		}
		best = max(best, runLen)
	}
	if best < 5 {
		t.Errorf("home 11: longest run of cycles < 60 s apart is %d, want ≥ 5", best)
	}

	// Home 7 fails: continuous_run raised at some point.
	var continuous bool
	for _, ev := range hm[7] {
		if a, ok := decode(t, ev).(*codec.Alarm); ok && a.Code == codec.AlarmContinuousRun {
			continuous = true
		}
	}
	if !continuous {
		t.Error("home 7 never raised continuous_run")
	}
}

func TestVolumeIdentity(t *testing.T) {
	r := runScenario(t, "dry-week", 9, 6, 3, 3*24*time.Hour)
	hm := byHome(r.events)
	for _, p := range r.truth.Homes {
		want := p.CycleVolumeL()
		n := 0
		for _, ev := range hm[p.Index] {
			c, ok := decode(t, ev).(*codec.CycleEvent)
			if !ok || c.PumpID != codec.PumpPrimary {
				continue
			}
			got := p.PitAreaM2 * float64(int(c.LevelEndMM)-int(c.LevelStartMM))
			if math.Abs(got-want) > 0.06*want {
				t.Errorf("home %d: §9 volume %.1f L from levels, true cycle volume %.1f L", p.Index, got, want)
				break
			}
			n++
		}
		if n == 0 {
			t.Errorf("home %d: no cycle events", p.Index)
		}
	}
}

func TestPacer(t *testing.T) {
	t.Run("unpaced never touches the clock", func(t *testing.T) {
		p := NewPacer(testStart, 0)
		p.now = func() time.Time { panic("now called") }
		p.after = func(time.Duration) <-chan time.Time { panic("after called") }
		if err := p.Wait(context.Background(), testStart.Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("60x maps one virtual minute to one real second", func(t *testing.T) {
		p := NewPacer(testStart, 60)
		wall := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
		p.now = func() time.Time { return wall }
		var slept time.Duration
		p.after = func(d time.Duration) <-chan time.Time {
			slept = d
			ch := make(chan time.Time, 1)
			ch <- wall.Add(d)
			return ch
		}
		if err := p.Wait(context.Background(), testStart.Add(time.Minute)); err != nil {
			t.Fatal(err)
		}
		if slept != time.Second {
			t.Errorf("slept %v, want 1s", slept)
		}
	})
	t.Run("cancel interrupts", func(t *testing.T) {
		p := NewPacer(testStart, 1)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if err := p.Wait(ctx, testStart.Add(time.Hour)); err == nil {
			t.Error("expected context error")
		}
	})
}

func TestScenarioShapes(t *testing.T) {
	want := map[string]float64{"storm25": 25, "storm50": 50, "thaw50": 50, "outage": 50, "failing-pump": 25, "dry-week": 0}
	for name, mm := range want {
		s := Scenarios()[name]
		if err := s.validate(); err != nil {
			t.Errorf("%s: %v", name, err)
		}
		if got := s.Rain.Total(); math.Abs(got-mm) > 0.01 {
			t.Errorf("%s: rain total %.2f mm, want %.0f", name, got, mm)
		}
	}
	segs := buildSegments(60, 8)
	for _, s := range segs {
		if len(s.HomeIndexes) < 3 {
			t.Errorf("segment %s has %d homes; k-anonymity needs ≥ 3", s.ID, len(s.HomeIndexes))
		}
	}
}

func TestRunCancel(t *testing.T) {
	scn := Scenarios()["storm50"]
	e, err := New(Config{Seed: 1, Start: testStart, Homes: 4, Segments: 2, Scenario: scn})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	truth, err := e.Run(ctx, NewHashSink())
	if err == nil || truth == nil || truth.Completed {
		t.Fatalf("Run with cancelled ctx: err=%v truth=%+v", err, truth)
	}
}

func BenchmarkStorm50(b *testing.B) {
	for i := 0; i < b.N; i++ {
		runScenario(b, "storm50", 42, 60, 8, 0)
	}
}
