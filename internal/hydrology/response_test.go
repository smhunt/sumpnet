package hydrology

import (
	"math"
	"testing"
	"time"
)

// pit is an ideal pit for synthetic tests: distances are exact, a pump run is
// instantaneous (reported as 20 s) and moves the level from 400 mm to 250 mm
// of depth, i.e. a drop of 150 mm of sensor distance.
type pit struct {
	depth float64
	obs   HomeObs
}

const (
	pitSensor  = 800.0
	pitTrigger = 400.0
	pitStop    = 250.0
	pitDrop    = pitTrigger - pitStop
)

// run advances the pit from start for dur, in 10 s steps, with inflow rate(t)
// in cycles/day, heartbeats every hb (0 = none).
func (p *pit) run(start time.Time, dur, hb time.Duration, rate func(time.Time) float64) {
	const step = 10 * time.Second
	nextHB := start
	for t := start; t.Before(start.Add(dur)); t = t.Add(step) {
		p.depth += rate(t) * pitDrop / 86400 * step.Seconds()
		if p.depth >= pitTrigger {
			p.obs.Cycles = append(p.obs.Cycles, Cycle{StartedAt: t, RunS: 20,
				LevelStartMM: int32(math.Round(pitSensor - p.depth)), LevelEndMM: int32(pitSensor - pitStop), Pump: "primary"})
			p.depth = pitStop
		}
		if hb > 0 && !t.Before(nextHB) {
			// A heartbeat right after a run would fall inside it; nudge it.
			at := t
			if n := len(p.obs.Cycles); n > 0 && !at.After(p.obs.Cycles[n-1].EndedAt()) {
				at = p.obs.Cycles[n-1].EndedAt().Add(time.Second)
			}
			p.obs.Levels = append(p.obs.Levels, LevelSample{At: at, DistanceMM: int32(math.Round(pitSensor - p.depth))})
			nextHB = nextHB.Add(hb)
		}
	}
}

func TestEstimateBaseflow(t *testing.T) {
	var cycles []Cycle
	for i := 0; i < 20; i++ { // one primary cycle every 4 h = 6 cycles/day
		cycles = append(cycles, Cycle{StartedAt: at(time.Duration(i) * 4 * time.Hour), RunS: 20, LevelStartMM: 400, LevelEndMM: 550, Pump: "primary"})
	}
	// A backup run and a sub-threshold blip are ignored (the blip breaks the chain).
	cycles = append(cycles, Cycle{StartedAt: at(10*time.Hour + time.Minute), RunS: 20, LevelStartMM: 400, LevelEndMM: 550, Pump: "backup"})
	blip := Cycle{StartedAt: at(30 * time.Hour), RunS: 1, LevelStartMM: 400, LevelEndMM: 405, Pump: "primary"}
	cycles = append(cycles, blip)
	sorted := HomeObs{Cycles: cycles}
	sorted.Sort()

	bf, ok := EstimateBaseflow(sorted.Cycles, nil, at(0), at(80*time.Hour))
	if !ok || !near(bf.CPD, 6, 1e-9) || bf.DropMM != 150 {
		t.Fatalf("baseflow = %+v %v", bf, ok)
	}
	// 20 cycles → 19 intervals; the blip resets the chain, so 28 h → 32 h is
	// lost; the backup run is skipped without breaking it.
	if bf.Samples != 18 {
		t.Errorf("samples = %d", bf.Samples)
	}

	// Rain at 50 h excludes every interval that ends after it and starts
	// within 72 h of it.
	rain := []RainInterval{ri(50*time.Hour, time.Hour, 2)}
	bf, ok = EstimateBaseflow(sorted.Cycles, rain, at(0), at(80*time.Hour))
	if !ok || bf.Samples != 11 {
		t.Errorf("with rain: %+v %v", bf, ok)
	}
	if _, enough := EstimateBaseflow(sorted.Cycles, nil, at(0), at(5*time.Hour)); enough {
		t.Error("one interval must not be enough")
	}
}

func TestInflowRatesConstantInflow(t *testing.T) {
	p := &pit{depth: 300}
	p.run(t0, 48*time.Hour, 15*time.Minute, func(time.Time) float64 { return 20 })
	rates := InflowRates(p.obs, pitDrop, at(2*time.Hour), at(46*time.Hour), time.Minute, 20*time.Minute)
	// 15-minute heartbeats leave grid points whose 20-minute window holds a
	// single sample; those are holes, not guesses.
	if len(rates) < 44*60/4 {
		t.Fatalf("only %d rates", len(rates))
	}
	// Distances are whole millimetres, so a short window can be a few percent
	// off; the mean over the day is exact.
	var sum float64
	for _, r := range rates {
		sum += r.CPD
		if !near(r.CPD, 20, 1.6) {
			t.Fatalf("rate at %v = %.2f cycles/day, want 20", r.At.Sub(t0), r.CPD)
		}
	}
	if mean := sum / float64(len(rates)); !near(mean, 20, 0.2) {
		t.Errorf("mean rate %.3f cycles/day, want 20", mean)
	}
	if InflowRates(p.obs, 0, t0, t0, time.Minute, time.Minute) != nil {
		t.Error("zero drop must yield nothing")
	}
}

func TestInflowRatesStormMode(t *testing.T) {
	o := HomeObs{
		Levels: []LevelSample{{At: at(0), DistanceMM: 500}, {At: at(time.Hour), DistanceMM: 480}},
		// Two back-to-back roll-ups: 12 then 8 cycles in 900 s windows.
		Summaries: []Summary{{WindowEnd: at(75 * time.Minute), WindowS: 900, Count: 12}, {WindowEnd: at(90 * time.Minute), WindowS: 900, Count: 8}},
	}
	rates := InflowRates(o, 150, at(62*time.Minute), at(88*time.Minute), time.Minute, 20*time.Minute)
	got := map[int]float64{}
	for _, r := range rates {
		got[int(r.At.Sub(t0).Minutes())] = r.CPD
	}
	if got[80] != 8*86400/900.0 || got[65] < 12*86400/900.0-1 {
		t.Errorf("roll-up rates = %v", got)
	}
}

func TestResponseLagAndRecession(t *testing.T) {
	grid := func(f func(m float64) float64, from, to int) []Rate {
		var out []Rate
		for m := from; m <= to; m++ {
			out = append(out, Rate{At: at(time.Duration(m) * time.Minute), CPD: f(float64(m))})
		}
		return out
	}
	// Base 5: flat, then a ramp of 1 cycle/day per minute from minute 10.
	ramp := grid(func(m float64) float64 { return 5 + math.Max(0, m-10) }, 0, 120)
	lag, ok := ResponseLag(ramp, 5, at(0), 30*time.Minute)
	// 2 × 5 = 10 is first exceeded at minute 15; interpolated between 15 and 16.
	if !ok || lag < 15*time.Minute || lag > 16*time.Minute {
		t.Errorf("lag = %v %v", lag, ok)
	}
	if _, confirmed := ResponseLag(ramp, 5, at(0), 200*time.Minute); confirmed {
		t.Error("hold longer than the data must not confirm")
	}
	// A blip above the threshold that does not hold is ignored.
	blip := grid(func(m float64) float64 {
		if m >= 3 && m <= 5 {
			return 50
		}
		return 5 + math.Max(0, m-30)
	}, 0, 120)
	if bl, blOK := ResponseLag(blip, 5, at(0), 20*time.Minute); !blOK || bl < 35*time.Minute || bl > 36*time.Minute {
		t.Errorf("lag with blip = %v %v", bl, blOK)
	}
	// Recession: 105 → 5 exponentially from minute 0 with a 60-minute
	// constant; within 1.2 × 5 = 6 when 100 e^{-m/60} ≤ 1, m = 60 ln 100.
	decay := grid(func(m float64) float64 { return 5 + 100*math.Exp(-m/60) }, 0, 600)
	rec, ok := Recession(decay, 5, at(0), time.Hour)
	if want := 60 * math.Log(100); !ok || !near(rec.Minutes(), want, 1) {
		t.Errorf("recession = %v %v, want %.1f min", rec, ok, want)
	}
	if _, receded := Recession(decay[:200], 5, at(0), time.Hour); receded {
		t.Error("not receded within the data")
	}
}

func TestAnalyseHomeStorm(t *testing.T) {
	// Three dry days at 4 cycles/day, then a storm response ramping 4 → 104
	// over 50 min from T0, holding until T1 and decaying with a 60-min
	// constant. Heartbeats every 5 minutes.
	T0 := at(72*time.Hour + 30*time.Minute)
	T1 := T0.Add(4 * time.Hour)
	rate := func(t time.Time) float64 {
		switch {
		case t.Before(T0):
			return 4
		case t.Before(T0.Add(50 * time.Minute)):
			return 4 + 100*t.Sub(T0).Minutes()/50
		case t.Before(T1):
			return 104
		default:
			return 4 + 100*math.Exp(-t.Sub(T1).Minutes()/60)
		}
	}
	p := &pit{depth: 260}
	p.run(t0, 5*24*time.Hour, 5*time.Minute, rate)
	s := Storm{FirstRain: T0.Add(-15 * time.Minute), Onset: T0.Add(-10 * time.Minute), RainEnd: T1.Add(-30 * time.Minute), TotalMM: 20, Closed: true}
	rain := []RainInterval{{Start: s.FirstRain, Dur: s.RainEnd.Sub(s.FirstRain), MM: 20}}

	h := AnalyseHomeStorm(p.obs, 0.16, s, rain, time.Time{}, at(5*24*time.Hour), DefaultResponseParams())
	if !h.HasBaseflow || !near(h.Baseflow.CPD, 4, 0.05) || !near(h.Baseflow.DropMM, pitDrop, 2) {
		t.Fatalf("baseflow = %+v", h.Baseflow)
	}
	// Rate > 8 at T0 + 2 min → lag 12 min from onset.
	if !h.HasLag || !near(h.Lag.Minutes(), 12, 3) {
		t.Errorf("lag = %v (%v), want ≈12 min", h.Lag, h.HasLag)
	}
	// Rate ≤ 4.8 at T1 + 60 ln 125 → recession 30 + 289.7 min from rain end.
	wantRec := 30 + 60*math.Log(125)
	if !h.HasRecession || math.Abs(h.Recession.Minutes()-wantRec) > 0.03*wantRec {
		t.Errorf("recession = %v (%v), want ≈%.0f min", h.Recession, h.HasRecession, wantRec)
	}
	var cycles int
	for _, c := range p.obs.Cycles {
		if !c.StartedAt.Before(s.Onset) && c.StartedAt.Before(h.WindowEnd) {
			cycles++
		}
	}
	if h.Cycles != cycles || cycles < 20 || !near(h.VolumeL, float64(cycles)*0.16*pitDrop, float64(cycles)*0.16*2) || !h.HasSamples {
		t.Errorf("cycles %d (want %d), volume %.0f L", h.Cycles, cycles, h.VolumeL)
	}

	// Data that stop at the peak: lag known, recession not yet; volume up to the data.
	h2 := AnalyseHomeStorm(p.obs, 0.16, s, rain, time.Time{}, T1, DefaultResponseParams())
	if !h2.HasLag || h2.HasRecession || !h2.WindowEnd.Equal(T1) {
		t.Errorf("truncated: %+v", h2)
	}
	// The next storm bounds the analysis too.
	h3 := AnalyseHomeStorm(p.obs, 0.16, s, rain, T1.Add(time.Hour), at(5*24*time.Hour), DefaultResponseParams())
	if h3.HasRecession || !h3.WindowEnd.Equal(T1.Add(time.Hour)) {
		t.Errorf("bounded by next storm: %+v", h3)
	}
	// No dry-weather history: no baseflow, so no lag or recession, but cycles and volume still count.
	h4 := AnalyseHomeStorm(p.obs, 0, s, nil, time.Time{}, at(5*24*time.Hour), ResponseParams{Step: time.Minute, LagWindow: 20 * time.Minute, RecessionWindow: 2 * time.Hour, BaseflowLookback: time.Minute, MaxRecession: 24 * time.Hour})
	if h4.HasBaseflow || h4.HasLag || h4.HasRecession || h4.Cycles == 0 || h4.VolumeL != 0 {
		t.Errorf("no history: %+v", h4)
	}
}
