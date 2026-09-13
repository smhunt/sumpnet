// Package sim is a deterministic simulator of a neighbourhood of sump pumps.
// Given a seed and a scenario it produces the exact LoRaWAN uplinks a fleet of
// house nodes would transmit, in ChirpStack's event shape, plus the ground
// truth (per-home parameters, rainfall, true cycles, realised lag/recession)
// that later phases are validated against.
//
// Determinism rules: virtual time only (no time.Now in this package outside
// the Pacer), one engine goroutine, one PCG stream per home seeded by
// (seed, index) so adding homes never perturbs existing ones, integer-exact
// hydrology, and UUIDs derived from those streams.
package sim

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"time"
)

// Config describes a run.
type Config struct {
	Seed     uint64
	Start    time.Time // virtual t0 (UTC)
	Speed    float64   // wall-clock pacing; 0 = as fast as possible
	Homes    int
	Segments int
	Scenario *Scenario
	Identity Identity
	// Duration overrides the scenario's duration when non-zero (shorter runs).
	Duration time.Duration
	// RainGauges is the number of tipping-bucket gauge nodes (fPort 5) at the
	// ends of the neighbourhood; the pilot has 2 (§4). 0 = none.
	RainGauges int
	// Site replaces the synthetic neighbourhood with real streets and address
	// points (Homes and Segments must then be 0). Scenarios with pump health
	// overrides are refused on a site, so no default run pins a failure on a
	// real address.
	Site *Site
}

// Engine runs one scenario for one set of homes.
type Engine struct {
	cfg      Config
	duration time.Duration
	segments []SegmentParams
	homes    []*home
	gauges   []*gauge
	rain     []float64 // mm/h per virtual minute
	pacer    *Pacer
	seq      uint64
	events   uint64
}

// New validates cfg and builds the neighbourhood.
func New(cfg Config) (*Engine, error) {
	if cfg.Scenario == nil {
		return nil, errors.New("sim: no scenario")
	}
	if err := cfg.Scenario.validate(); err != nil {
		return nil, err
	}
	if cfg.Site != nil {
		if err := cfg.Site.validate(); err != nil {
			return nil, err
		}
		if cfg.Homes != 0 || cfg.Segments != 0 {
			return nil, fmt.Errorf("sim: homes (%d) and segments (%d) come from site %q; leave them 0", cfg.Homes, cfg.Segments, cfg.Site.Name)
		}
		if len(cfg.Scenario.HealthOverrides) > 0 {
			return nil, fmt.Errorf("sim: scenario %q pins pump health on specific homes, which a real-address site does not allow", cfg.Scenario.Name)
		}
		for _, o := range cfg.Scenario.Outages {
			for _, si := range o.SegmentIndexes {
				if si < 0 || si >= len(cfg.Site.Segments) {
					return nil, fmt.Errorf("sim: scenario %q outage segment %d is outside site %q (%d segments)", cfg.Scenario.Name, si, cfg.Site.Name, len(cfg.Site.Segments))
				}
			}
		}
	} else if cfg.Homes <= 0 || cfg.Segments <= 0 || cfg.Segments > cfg.Homes {
		return nil, fmt.Errorf("sim: need 1 <= segments (%d) <= homes (%d)", cfg.Segments, cfg.Homes)
	}
	if cfg.RainGauges < 0 {
		return nil, fmt.Errorf("sim: rain gauges must be >= 0, got %d", cfg.RainGauges)
	}
	if cfg.Start.IsZero() {
		return nil, errors.New("sim: start time is required")
	}
	if cfg.Identity == (Identity{}) {
		cfg.Identity = DefaultIdentity()
	}
	e := &Engine{cfg: cfg, duration: cfg.Scenario.Duration}
	if cfg.Duration > 0 {
		e.duration = cfg.Duration
	}
	nSteps := int(e.duration / time.Second)

	scenarioRNG := rand.New(rand.NewPCG(cfg.Seed, 0)) //nolint:gosec // deterministic simulation
	if len(cfg.Scenario.RainMinutes) > 0 {
		e.rain = minuteSeries(cfg.Scenario.RainMinutes, e.duration, cfg.Scenario.RainJitter, scenarioRNG)
	} else {
		e.rain = rainSeries(cfg.Scenario.Rain, e.duration, cfg.Scenario.RainJitter, scenarioRNG)
	}

	nHomes := cfg.Homes
	if cfg.Site != nil {
		e.segments = siteSegments(cfg.Site)
		nHomes = len(cfg.Site.Homes)
	} else {
		e.segments = buildSegments(cfg.Homes, cfg.Segments)
	}
	e.homes = make([]*home, nHomes)
	for _, seg := range e.segments {
		seg := seg
		for _, idx := range seg.HomeIndexes {
			id := syntheticIdentity(idx, cfg.Seed)
			if cfg.Site != nil {
				id = siteIdentity(cfg.Site.Homes[idx])
			}
			e.homes[idx] = newHome(idx, id, &seg, cfg.Scenario, cfg.Seed, nSteps)
		}
	}
	for i := 0; i < cfg.RainGauges; i++ {
		g := newGauge(i, cfg.Seed)
		if cfg.Site != nil {
			g.p.Location, g.p.Lon, g.p.Lat = siteGaugePlacement(cfg.Site, i)
		}
		e.gauges = append(e.gauges, g)
	}
	e.pacer = NewPacer(cfg.Start.UTC(), cfg.Speed)
	return e, nil
}

// Segments returns the segment parameters.
func (e *Engine) Segments() []SegmentParams { return e.segments }

// Homes returns the per-home ground-truth parameters.
func (e *Engine) Homes() []HomeParams {
	out := make([]HomeParams, len(e.homes))
	for i, h := range e.homes {
		out[i] = h.p
	}
	return out
}

// RainGauges returns the rain gauge node parameters.
func (e *Engine) RainGauges() []RainGaugeParams {
	out := make([]RainGaugeParams, len(e.gauges))
	for i, g := range e.gauges {
		out[i] = g.p
	}
	return out
}

// SegmentOf returns the segment ID of a home (for sinks); "" for rain gauges.
func (e *Engine) SegmentOf(homeIndex int) string {
	if homeIndex < 0 || homeIndex >= len(e.homes) {
		return ""
	}
	return e.homes[homeIndex].p.SegmentID
}

// Duration is the effective run length.
func (e *Engine) Duration() time.Duration { return e.duration }

func (e *Engine) mainsOK(segIdx, step int) bool {
	t := time.Duration(step) * time.Second
	for _, o := range e.cfg.Scenario.Outages {
		if t < o.At || t >= o.At+o.Duration {
			continue
		}
		for _, s := range o.SegmentIndexes {
			if s == segIdx {
				return false
			}
		}
	}
	return true
}

// Run steps virtual time one second at a time until the scenario ends or ctx
// is cancelled, publishing every uplink to sink in emission order. The
// returned Truth is complete either way (Completed reports which).
func (e *Engine) Run(ctx context.Context, sink Sink) (*Truth, error) {
	start := e.cfg.Start.UTC()
	nSteps := int(e.duration / time.Second)
	rainAt := func(minute int) float64 {
		if minute < 0 || minute >= len(e.rain) {
			return 0
		}
		return e.rain[minute]
	}
	buf := make([]Event, 0, 16)
	var runErr error
	step := 0
	for ; step < nSteps; step++ {
		now := start.Add(time.Duration(step) * time.Second)
		if step%60 == 0 {
			if err := ctx.Err(); err != nil {
				runErr = err
				break
			}
			if err := e.pacer.Wait(ctx, now); err != nil {
				runErr = err
				break
			}
		}
		for _, h := range e.homes {
			buf = h.step(step, now, rainAt, e.mainsOK(h.seg.Index, step), buf[:0])
			if err := e.publish(ctx, sink, buf); err != nil {
				return e.truth(start, step, false), err
			}
		}
		for _, g := range e.gauges {
			buf = g.step(step, now, rainAt, buf[:0])
			if err := e.publish(ctx, sink, buf); err != nil {
				return e.truth(start, step, false), err
			}
		}
	}
	return e.truth(start, min(step, nSteps), runErr == nil), runErr
}

func (e *Engine) publish(ctx context.Context, sink Sink, evs []Event) error {
	for _, ev := range evs {
		ev.Seq = e.seq
		e.seq++
		if err := sink.Publish(ctx, ev); err != nil {
			return fmt.Errorf("sim: publish seq %d: %w", ev.Seq, err)
		}
		e.events++
	}
	return nil
}

func (e *Engine) truth(start time.Time, step int, completed bool) *Truth {
	t := &Truth{
		Version:   1,
		Seed:      e.cfg.Seed,
		Scenario:  e.cfg.Scenario.Name,
		Start:     start,
		Completed: completed,
		EndedAt:   start.Add(time.Duration(step) * time.Second),
		Events:    e.events,
		Segments:  e.segments,
		Homes:     e.Homes(),
		// Non-nil so JSON consumers see [] rather than null.
		Storms:         []StormTruth{},
		TrueCycles:     []CycleTruth{},
		ExpectedAlarms: []AlarmTruth{},
	}
	endMin := step / 60
	if endMin >= len(e.rain) {
		endMin = len(e.rain) - 1
	}
	// Per-minute home state (rate, storm inflow) is recorded at the first
	// second of each minute the run actually stepped through.
	lastMin := -1
	if step > 0 {
		lastMin = min((step-1)/60, endMin)
	}
	t.Rainfall = make([]RainSample, endMin+1)
	for m := 0; m <= endMin; m++ {
		t.Rainfall[m] = RainSample{TS: start.Add(time.Duration(m) * time.Minute), Mm: e.rain[m] / 60}
	}
	for i, w := range findStorms(e.rain[:endMin+1]) {
		st := StormTruth{
			ID:         fmt.Sprintf("storm-%d", i+1),
			Onset:      start.Add(time.Duration(w.onset) * time.Minute),
			RainEnd:    start.Add(time.Duration(w.rainEnd) * time.Minute),
			TotalMm:    w.total,
			PeakMmPerH: w.peak,
		}
		for _, h := range e.homes {
			st.Homes = append(st.Homes, homeStormTruth(h, w, start, lastMin))
		}
		t.Storms = append(t.Storms, st)
	}
	for _, g := range e.gauges {
		t.RainGauges = append(t.RainGauges, g.truth())
	}
	for _, h := range e.homes {
		t.TrueCycles = append(t.TrueCycles, h.cycles...)
		t.ExpectedAlarms = append(t.ExpectedAlarms, h.alarms...)
	}
	return t
}
