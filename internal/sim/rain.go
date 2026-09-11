package sim

import (
	"math/rand/v2"
	"sort"
	"time"
)

// RainBreakpoint is a point on a piecewise-linear hyetograph.
type RainBreakpoint struct {
	At     time.Duration `json:"at"`
	MmPerH float64       `json:"mm_per_h"`
}

// Hyetograph is rainfall intensity over the scenario as a piecewise-linear
// curve; intensity is zero outside the first and last breakpoint.
type Hyetograph []RainBreakpoint

// Intensity returns mm/h at offset t from the scenario start.
func (h Hyetograph) Intensity(t time.Duration) float64 {
	if len(h) == 0 || t < h[0].At || t > h[len(h)-1].At {
		return 0
	}
	i := sort.Search(len(h), func(i int) bool { return h[i].At >= t })
	if i == 0 {
		return h[0].MmPerH
	}
	a, b := h[i-1], h[i]
	if b.At == a.At {
		return b.MmPerH
	}
	f := float64(t-a.At) / float64(b.At-a.At)
	return a.MmPerH + f*(b.MmPerH-a.MmPerH)
}

// Total integrates the curve in millimetres (trapezoidal, exact for a
// piecewise-linear curve).
func (h Hyetograph) Total() float64 {
	var mm float64
	for i := 1; i < len(h); i++ {
		hours := (h[i].At - h[i-1].At).Hours()
		mm += hours * (h[i].MmPerH + h[i-1].MmPerH) / 2
	}
	return mm
}

// Normalised scales the curve so that Total() == totalMM.
func (h Hyetograph) Normalised(totalMM float64) Hyetograph {
	cur := h.Total()
	out := make(Hyetograph, len(h))
	copy(out, h)
	if cur <= 0 {
		return out
	}
	k := totalMM / cur
	for i := range out {
		out[i].MmPerH *= k
	}
	return out
}

// Shifted moves the whole curve later by d.
func (h Hyetograph) Shifted(d time.Duration) Hyetograph {
	out := make(Hyetograph, len(h))
	for i, p := range h {
		out[i] = RainBreakpoint{At: p.At + d, MmPerH: p.MmPerH}
	}
	return out
}

// Append concatenates two curves (the second must start after the first ends).
func (h Hyetograph) Append(o Hyetograph) Hyetograph {
	out := make(Hyetograph, 0, len(h)+len(o))
	out = append(out, h...)
	return append(out, o...)
}

// rainSeries samples the hyetograph once per virtual minute with optional
// multiplicative jitter from the scenario RNG. This is the neighbourhood's
// "true" rainfall, i.e. what the rain gauges observe.
func rainSeries(h Hyetograph, duration time.Duration, jitter float64, r *rand.Rand) []float64 {
	n := int(duration/time.Minute) + 1
	out := make([]float64, n)
	for i := range out {
		v := h.Intensity(time.Duration(i) * time.Minute)
		if jitter > 0 && v > 0 {
			v *= 1 + jitter*(2*r.Float64()-1)
		}
		out[i] = v
	}
	return out
}
