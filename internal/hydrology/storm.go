package hydrology

import (
	"math"
	"sort"
	"time"
)

// §10 storm thresholds.
const (
	StormMinTotalMM    = 5.0           // a storm has at least this much rain ...
	StormMaxGap        = 6 * time.Hour // ... with dry gaps shorter than this
	OnsetMinIntensity  = 1.0           // mm/h: onset is the first interval at or above this
	RainBin            = 5 * time.Minute
	rainEpsilonMM      = 1e-9
	intensityTolerance = 1e-9
)

// RainInterval is rainfall of MM over [Start, Start+Dur).
type RainInterval struct {
	Start time.Time
	Dur   time.Duration
	MM    float64
}

// End is the exclusive end of the interval.
func (r RainInterval) End() time.Time { return r.Start.Add(r.Dur) }

// IntensityMMH is the mean intensity over the interval.
func (r RainInterval) IntensityMMH() float64 {
	if r.Dur <= 0 {
		return 0
	}
	return r.MM / r.Dur.Hours()
}

// Wet reports whether any rain fell in the interval.
func (r RainInterval) Wet() bool { return r.MM > rainEpsilonMM }

// MergeRain combines rainfall from several stations onto a grid of bin-wide
// intervals aligned to the Unix epoch. Each station's interval depth is
// spread uniformly over its duration; a bin's depth is the mean over the
// stations whose intervals overlap it, so a station that is silent for a bin
// neither adds rain nor dilutes the others. Bins no station covers are
// omitted. The result is sorted and non-overlapping.
func MergeRain(stations [][]RainInterval, bin time.Duration) []RainInterval {
	if bin <= 0 {
		bin = RainBin
	}
	type acc struct {
		mm       float64
		stations int
	}
	bins := map[int64]*acc{}
	for _, st := range stations {
		seen := map[int64]float64{}
		for _, r := range st {
			if r.Dur <= 0 {
				continue
			}
			first := floorDiv(r.Start.UnixNano(), int64(bin))
			last := floorDiv(r.End().UnixNano()-1, int64(bin))
			for b := first; b <= last; b++ {
				bs := time.Unix(0, b*int64(bin))
				overlap := minTime(bs.Add(bin), r.End()).Sub(maxTime(bs, r.Start))
				if overlap <= 0 {
					continue
				}
				seen[b] += r.MM * float64(overlap) / float64(r.Dur)
			}
		}
		for b, mm := range seen {
			a := bins[b]
			if a == nil {
				a = &acc{}
				bins[b] = a
			}
			a.mm += mm
			a.stations++
		}
	}
	keys := make([]int64, 0, len(bins))
	for k := range bins {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
	out := make([]RainInterval, len(keys))
	for i, k := range keys {
		a := bins[k]
		out[i] = RainInterval{Start: time.Unix(0, k*int64(bin)).UTC(), Dur: bin, MM: a.mm / float64(a.stations)}
	}
	return out
}

// Storm is a §10 storm event found in a rain series.
type Storm struct {
	FirstRain time.Time // start of the first wet interval
	Onset     time.Time // start of the first interval at or above OnsetMinIntensity (FirstRain if none)
	RainEnd   time.Time // end of the last wet interval
	TotalMM   float64
	PeakMMH   float64 // highest interval intensity
	// Closed is true once the data reach StormMaxGap past RainEnd, so no
	// later rain can extend the storm.
	Closed bool
}

// SegmentStorms groups a sorted, non-overlapping rain series into storms:
// wet intervals separated by dry gaps shorter than StormMaxGap, with at least
// StormMinTotalMM in total. horizon is the time up to which rain is known
// (usually the end of the newest interval); it decides whether the last
// storm is closed. A trailing group below StormMinTotalMM is not reported
// even if it is still open.
func SegmentStorms(rain []RainInterval, horizon time.Time) []Storm {
	var out []Storm
	var cur *Storm
	flush := func() {
		if cur != nil && cur.TotalMM+rainEpsilonMM >= StormMinTotalMM {
			out = append(out, *cur)
		}
		cur = nil
	}
	onsetFound := false
	for _, r := range rain {
		if !r.Wet() {
			continue
		}
		if cur != nil && r.Start.Sub(cur.RainEnd) >= StormMaxGap {
			cur.Closed = true
			flush()
		}
		if cur == nil {
			cur = &Storm{FirstRain: r.Start, Onset: r.Start}
			onsetFound = false
		}
		intensity := r.IntensityMMH()
		if !onsetFound && intensity+intensityTolerance >= OnsetMinIntensity {
			cur.Onset = r.Start
			onsetFound = true
		}
		cur.RainEnd = maxTime(cur.RainEnd, r.End())
		cur.TotalMM += r.MM
		cur.PeakMMH = math.Max(cur.PeakMMH, intensity)
	}
	if cur != nil {
		cur.Closed = !horizon.Before(cur.RainEnd.Add(StormMaxGap))
		flush()
	}
	return out
}

// FindStorms is the storm pipeline for several stations: merge them onto
// RainBin bins, segment storms on the merged series (grouping, totals, peak),
// then take each storm's onset and rain end from the stations' own
// intervals, which are finer than a bin: onset is the earliest start of a
// station interval at or above OnsetMinIntensity inside the storm, rain end
// the latest end of a wet station interval inside it. Bins dilute a single
// gauge's first tip, and every station is late to see the first rain, so
// the earliest station is the best estimate of the true onset.
func FindStorms(stations [][]RainInterval, horizon time.Time) []Storm {
	storms := SegmentStorms(MergeRain(stations, RainBin), horizon)
	for i := range storms {
		s := &storms[i]
		from, to := s.FirstRain.Add(-RainBin), s.RainEnd.Add(RainBin)
		onset, end := time.Time{}, time.Time{}
		for _, st := range stations {
			for _, r := range st {
				if !r.Wet() || !r.End().After(from) || !r.Start.Before(to) {
					continue
				}
				if r.IntensityMMH()+intensityTolerance >= OnsetMinIntensity && (onset.IsZero() || r.Start.Before(onset)) {
					onset = r.Start
				}
				end = maxTime(end, r.End())
			}
		}
		if !onset.IsZero() {
			s.Onset = onset
			s.FirstRain = minTime(s.FirstRain, onset)
		}
		if !end.IsZero() {
			s.RainEnd = end
		}
	}
	return storms
}

// RainBetween sums rain that fell in [from, to), prorating partial overlaps.
func RainBetween(rain []RainInterval, from, to time.Time) float64 {
	var mm float64
	for _, r := range rain {
		overlap := minTime(to, r.End()).Sub(maxTime(from, r.Start))
		if overlap <= 0 || r.Dur <= 0 {
			continue
		}
		mm += r.MM * float64(overlap) / float64(r.Dur)
	}
	return mm
}

// SegmentLoad is §10's segment load: total storm volume of the reporting
// homes divided by their number. Whether it may be published is decided by
// internal/privacy (k ≥ 3); this is only the arithmetic.
func SegmentLoad(volumesL []float64) float64 {
	if len(volumesL) == 0 {
		return 0
	}
	var total float64
	for _, v := range volumesL {
		total += v
	}
	return total / float64(len(volumesL))
}

func floorDiv(a, b int64) int64 {
	q := a / b
	if (a%b != 0) && ((a < 0) != (b < 0)) {
		q--
	}
	return q
}

func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

func maxTime(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}
