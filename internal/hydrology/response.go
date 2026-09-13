package hydrology

import (
	"math"
	"sort"
	"time"
)

// §10 response thresholds.
const (
	LagRateFactor       = 2.0            // response lag: cycle rate above this × baseflow
	RecessionRateFactor = 1.2            // recession: cycle rate back within this × baseflow
	BaseflowDryFor      = 72 * time.Hour // baseflow uses cycles with no rain this long before
	BaseflowMinSamples  = 2              // intervals needed for a baseflow estimate
)

// ResponseParams tunes the inflow-rate estimator behind lag and recession.
// The defaults were chosen against the simulator (prompt_plan.md §10):
// narrower lag windows leave holes between 15-minute heartbeats, wider ones
// smear the rising limb; the falling limb is slow, so a long window buys
// noise immunity for low-baseflow homes.
type ResponseParams struct {
	Step             time.Duration // rate grid spacing
	LagWindow        time.Duration // least-squares window on the rising limb
	LagHold          time.Duration // the rate must stay above 2× baseflow this long
	RecessionWindow  time.Duration // least-squares window on the falling limb
	RecessionHold    time.Duration // the rate must stay within 1.2× baseflow this long
	BaseflowLookback time.Duration // dry-weather cycles considered before the first rain
	MaxRecession     time.Duration // analysis stops this long after the rain ends
}

// DefaultResponseParams returns the tuned defaults.
func DefaultResponseParams() ResponseParams {
	return ResponseParams{
		Step:             time.Minute,
		LagWindow:        20 * time.Minute,
		LagHold:          30 * time.Minute,
		RecessionWindow:  2 * time.Hour,
		RecessionHold:    time.Hour,
		BaseflowLookback: 14 * 24 * time.Hour,
		MaxRecession:     7 * 24 * time.Hour,
	}
}

// LevelSample is one heartbeat's pit reading.
type LevelSample struct {
	At         time.Time
	DistanceMM int32 // sensor → water; smaller = higher water
}

// Summary is one storm-mode roll-up: Count cycles ended in the WindowS
// seconds up to WindowEnd.
type Summary struct {
	WindowEnd time.Time
	WindowS   int32
	Count     int32
}

// WindowStart is the start of the roll-up window.
func (s Summary) WindowStart() time.Time {
	return s.WindowEnd.Add(-time.Duration(s.WindowS) * time.Second)
}

// HomeObs is everything a node reported that bears on its inflow, each slice
// sorted by time.
type HomeObs struct {
	Cycles    []Cycle
	Summaries []Summary
	Levels    []LevelSample
}

// Sort orders every slice by time (stable for equal times).
func (o *HomeObs) Sort() {
	sort.SliceStable(o.Cycles, func(i, j int) bool { return o.Cycles[i].StartedAt.Before(o.Cycles[j].StartedAt) })
	sort.SliceStable(o.Summaries, func(i, j int) bool { return o.Summaries[i].WindowEnd.Before(o.Summaries[j].WindowEnd) })
	sort.SliceStable(o.Levels, func(i, j int) bool { return o.Levels[i].At.Before(o.Levels[j].At) })
}

// Baseflow is a §10 baseflow estimate.
type Baseflow struct {
	CPD     float64 // median dry-weather cycles/day
	DropMM  float64 // median level drop of the same cycles: the size of one cycle
	Samples int     // dry-weather intervals the median was taken over
}

// EstimateBaseflow applies §10 baseflow to the primary-pump cycles started
// in [from, to): the median of the per-interval cycle rates 86 400 s ÷ (gap
// between consecutive cycle starts), over intervals with no rain recorded
// from BaseflowDryFor before the earlier cycle until the later one. The
// median of per-interval rates equals one day over the median interval, so
// a home with one cycle a day still gets a precise estimate from a few days
// of data. ok is false with fewer than BaseflowMinSamples intervals.
func EstimateBaseflow(cycles []Cycle, rain []RainInterval, from, to time.Time) (Baseflow, bool) {
	var prev *Cycle
	var rates, drops []float64
	for i := range cycles {
		c := cycles[i]
		if c.Pump != "" && c.Pump != "primary" {
			continue
		}
		if c.StartedAt.Before(from) || !c.StartedAt.Before(to) || !IsCycle(c) {
			prev = nil
			continue
		}
		if prev != nil {
			gap := c.StartedAt.Sub(prev.StartedAt)
			if gap > 0 && RainBetween(rain, prev.StartedAt.Add(-BaseflowDryFor), c.StartedAt) <= rainEpsilonMM {
				rates = append(rates, 86400/gap.Seconds())
				drops = append(drops, float64(prev.DropMM()))
			}
		}
		prev = &cycles[i]
	}
	if len(rates) < BaseflowMinSamples {
		return Baseflow{Samples: len(rates)}, false
	}
	return Baseflow{CPD: median(rates), DropMM: median(drops), Samples: len(rates)}, true
}

// Rate is the inflow around At expressed in cycles per day of the home's
// typical cycle (inflow ÷ one cycle's volume, the unit baseflow is in).
type Rate struct {
	At  time.Time
	CPD float64
}

type volSample struct {
	at time.Time
	mm float64 // cumulative inflow in millimetres of pit depth (+ constant)
}

// cumulativeInflow turns the node's reports into samples of cumulative
// inflow, in millimetres of pit depth so the pit area cancels: at any
// instant, inflow so far = level rise + everything pumped out so far. A
// sensor distance d contributes −d; each cycle adds its drop once it ends;
// a storm-mode roll-up adds Count × dropMM at its window end. Level readings
// taken inside a pump run or a roll-up window are skipped because the pumped
// volume up to that instant is unknown.
func cumulativeInflow(o HomeObs, dropMM float64) []volSample {
	type event struct {
		at   time.Time
		kind int // 0 cycle start, 1 level, 2 summary end (pumped only)
		i    int
	}
	evs := make([]event, 0, len(o.Cycles)+len(o.Levels)+len(o.Summaries))
	for i, c := range o.Cycles {
		evs = append(evs, event{c.StartedAt, 0, i})
	}
	for i, l := range o.Levels {
		evs = append(evs, event{l.At, 1, i})
	}
	for i, s := range o.Summaries {
		evs = append(evs, event{s.WindowEnd, 2, i})
	}
	sort.SliceStable(evs, func(i, j int) bool { return evs[i].at.Before(evs[j].at) })

	var out []volSample
	var pumped float64
	type pending struct {
		end time.Time
		mm  float64
	}
	var running []pending
	settle := func(t time.Time) bool { // apply runs that ended by t; report whether a run is in progress
		keep := running[:0]
		busy := false
		for _, p := range running {
			if !p.end.After(t) {
				pumped += p.mm
				continue
			}
			busy = true
			keep = append(keep, p)
		}
		running = keep
		return busy
	}
	inWindow := func(t time.Time) bool {
		j := sort.Search(len(o.Summaries), func(k int) bool { return !o.Summaries[k].WindowEnd.Before(t) })
		return j < len(o.Summaries) && !t.Before(o.Summaries[j].WindowStart())
	}
	for _, e := range evs {
		busy := settle(e.at)
		switch e.kind {
		case 0:
			c := o.Cycles[e.i]
			if !busy && !inWindow(e.at) {
				out = append(out, volSample{e.at, pumped - float64(c.LevelStartMM)})
			}
			running = append(running, pending{c.EndedAt(), float64(c.DropMM())})
		case 1:
			l := o.Levels[e.i]
			if !busy && !inWindow(e.at) {
				out = append(out, volSample{e.at, pumped - float64(l.DistanceMM)})
			}
		case 2:
			pumped += float64(o.Summaries[e.i].Count) * dropMM
		}
	}
	return out
}

// InflowRates estimates the inflow rate every step from from to to: the
// least-squares slope of cumulative inflow over the samples within ±window/2
// (at least two samples spanning a third of the window), converted to cycles
// per day with dropMM. Grid points without enough samples — typically deep
// inside storm mode, where no level can be trusted — take the roll-up rate
// Count × 86 400 ÷ WindowS of the window that contains them.
func InflowRates(o HomeObs, dropMM float64, from, to time.Time, step, window time.Duration) []Rate {
	if dropMM <= 0 || step <= 0 || window <= 0 {
		return nil
	}
	samples := cumulativeInflow(o, dropMM)
	var out []Rate
	lo := 0
	for t := from; !t.After(to); t = t.Add(step) {
		a, b := t.Add(-window/2), t.Add(window/2)
		for lo < len(samples) && samples[lo].at.Before(a) {
			lo++
		}
		hi := lo
		for hi < len(samples) && !samples[hi].at.After(b) {
			hi++
		}
		win := samples[lo:hi]
		if len(win) >= 2 && win[len(win)-1].at.Sub(win[0].at) >= window/3 {
			out = append(out, Rate{At: t, CPD: slopePerSecond(win) * 86400 / dropMM})
			continue
		}
		if s, ok := summaryAt(o.Summaries, t); ok && s.WindowS > 0 {
			out = append(out, Rate{At: t, CPD: float64(s.Count) * 86400 / float64(s.WindowS)})
		}
	}
	return out
}

func summaryAt(ss []Summary, t time.Time) (Summary, bool) {
	j := sort.Search(len(ss), func(k int) bool { return !ss[k].WindowEnd.Before(t) })
	if j < len(ss) && !t.Before(ss[j].WindowStart()) {
		return ss[j], true
	}
	return Summary{}, false
}

func slopePerSecond(s []volSample) float64 {
	t0 := s[0].at
	var sx, sy, sxx, sxy float64
	n := float64(len(s))
	for _, p := range s {
		x := p.at.Sub(t0).Seconds()
		sx += x
		sy += p.mm
		sxx += x * x
		sxy += x * p.mm
	}
	den := n*sxx - sx*sx
	if den == 0 {
		return 0
	}
	return (n*sxy - sx*sy) / den
}

// ResponseLag is §10 response lag: the time from onset to the first rate
// above LagRateFactor × baseflow that stays above it for hold. The crossing
// is interpolated linearly between the grid points either side of it. ok is
// false if that has not happened in the data.
func ResponseLag(rates []Rate, baseCPD float64, onset time.Time, hold time.Duration) (time.Duration, bool) {
	t, ok := firstSustained(rates, onset, hold, LagRateFactor*baseCPD, true)
	return max(0, t.Sub(onset)), ok
}

// Recession is §10 recession: the time from rain end to the first rate
// within RecessionRateFactor × baseflow that stays within it for hold.
func Recession(rates []Rate, baseCPD float64, rainEnd time.Time, hold time.Duration) (time.Duration, bool) {
	t, ok := firstSustained(rates, rainEnd, hold, RecessionRateFactor*baseCPD, false)
	return max(0, t.Sub(rainEnd)), ok
}

// firstSustained finds the first rate at or after from on the wanted side of
// threshold (strictly above when above, at or below otherwise) that stays
// there for hold, and interpolates the crossing from the previous rate.
func firstSustained(rates []Rate, from time.Time, hold time.Duration, threshold float64, above bool) (time.Time, bool) {
	cond := func(r float64) bool {
		if above {
			return r > threshold
		}
		return r <= threshold
	}
	start := -1
	for i, r := range rates {
		if r.At.Before(from) {
			continue
		}
		if !cond(r.CPD) {
			start = -1
			continue
		}
		if start < 0 {
			start = i
		}
		if r.At.Sub(rates[start].At) < hold {
			continue
		}
		at := rates[start].At
		if start > 0 {
			p, c := rates[start-1], rates[start]
			if gap := c.At.Sub(p.At); gap > 0 && gap <= 2*maxStep(rates) && c.CPD != p.CPD {
				f := (threshold - p.CPD) / (c.CPD - p.CPD)
				f = math.Max(0, math.Min(1, f))
				at = p.At.Add(time.Duration(f * float64(gap)))
			}
		}
		if at.Before(from) {
			at = from
		}
		return at, true
	}
	return time.Time{}, false
}

// maxStep is the typical grid spacing (the first gap), used to refuse
// interpolating across holes in the series.
func maxStep(rates []Rate) time.Duration {
	if len(rates) < 2 {
		return 0
	}
	return rates[1].At.Sub(rates[0].At)
}

// HomeStorm is one home's §10 response to one storm.
type HomeStorm struct {
	Baseflow     Baseflow
	HasBaseflow  bool
	Lag          time.Duration // onset → rate above 2× baseflow
	HasLag       bool
	Recession    time.Duration // rain end → rate back within 1.2× baseflow
	HasRecession bool
	// Cycles and VolumeL cover [storm onset, rain end + recession), or up to
	// the end of the analysis while the home has not receded: individually
	// reported cycles plus storm-mode roll-ups.
	Cycles     int
	VolumeL    float64
	WindowEnd  time.Time
	HasSamples bool // the home reported anything in the window
}

// AnalyseHomeStorm computes one home's response to storm s. rain is the
// neighbourhood rainfall (for the dry-weather test of baseflow), until is the
// next storm's first rain (zero if none) and dataEnd the newest time the
// home's data are known up to; the analysis never looks past either, nor
// past MaxRecession after the rain ends. pitAreaM2 ≤ 0 leaves VolumeL at 0.
//
// The recession search starts at the rain end, or at the lag crossing if the
// home only responded after the rain had stopped (otherwise a home that had
// not yet risen would count as receded at once).
func AnalyseHomeStorm(o HomeObs, pitAreaM2 float64, s Storm, rain []RainInterval, until, dataEnd time.Time, p ResponseParams) HomeStorm {
	end := s.RainEnd.Add(p.MaxRecession)
	if !until.IsZero() && until.Before(end) {
		end = until
	}
	if dataEnd.Before(end) {
		end = dataEnd
	}
	var h HomeStorm
	h.Baseflow, h.HasBaseflow = EstimateBaseflow(o.Cycles, rain, s.FirstRain.Add(-p.BaseflowLookback), s.FirstRain)
	if h.HasBaseflow {
		lagRates := InflowRates(o, h.Baseflow.DropMM, s.Onset.Add(-p.LagWindow), end, p.Step, p.LagWindow)
		h.Lag, h.HasLag = ResponseLag(lagRates, h.Baseflow.CPD, s.Onset, p.LagHold)
		recFrom := s.RainEnd
		if h.HasLag && s.Onset.Add(h.Lag).After(recFrom) {
			recFrom = s.Onset.Add(h.Lag)
		}
		recRates := InflowRates(o, h.Baseflow.DropMM, recFrom, end, p.Step, p.RecessionWindow)
		if at, ok := firstSustained(recRates, recFrom, p.RecessionHold, RecessionRateFactor*h.Baseflow.CPD, false); ok {
			h.Recession, h.HasRecession = at.Sub(s.RainEnd), true
		}
	}

	h.WindowEnd = end
	if h.HasRecession {
		h.WindowEnd = minTime(end, s.RainEnd.Add(h.Recession))
	}
	in := func(t time.Time) bool { return !t.Before(s.Onset) && t.Before(h.WindowEnd) }
	dropMM := h.Baseflow.DropMM
	var drops []float64
	for _, c := range o.Cycles {
		if !in(c.StartedAt) {
			continue
		}
		h.Cycles++
		h.HasSamples = true
		if d := c.DropMM(); d > 0 {
			h.VolumeL += pitAreaM2 * float64(d)
			drops = append(drops, float64(d))
		}
	}
	if !h.HasBaseflow && len(drops) > 0 {
		dropMM = median(drops)
	}
	for _, sm := range o.Summaries {
		if !in(sm.WindowEnd) {
			continue
		}
		h.Cycles += int(sm.Count)
		h.HasSamples = true
		h.VolumeL += pitAreaM2 * float64(sm.Count) * dropMM
	}
	for _, l := range o.Levels {
		if in(l.At) {
			h.HasSamples = true
			break
		}
	}
	h.VolumeL = math.Max(0, h.VolumeL)
	return h
}

func median(xs []float64) float64 {
	if len(xs) == 0 {
		return math.NaN()
	}
	s := append([]float64(nil), xs...)
	sort.Float64s(s)
	n := len(s)
	if n%2 == 1 {
		return s[n/2]
	}
	return (s[n/2-1] + s[n/2]) / 2
}
