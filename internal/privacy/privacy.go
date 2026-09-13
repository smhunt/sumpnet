// Package privacy enforces the public-view rules of prompt_plan.md §2 and
// ADR 0005: per-home values never leave an owner-scoped path, and a street
// segment's aggregate is published only when at least MinHomes homes report.
// The api-gateway and (through QueryService) the MCP server must build every
// public segment view with this package.
package privacy

import (
	"math"
	"sort"
)

// MinHomes is the k-anonymity threshold for segment aggregates.
const MinHomes = 3

// Visible reports whether an aggregate over homesReporting homes may be shown.
func Visible(homesReporting int) bool { return homesReporting >= MinHomes }

// HomeStorm is one home's response to a storm: the input to a segment aggregate.
type HomeStorm struct {
	HomeID       string
	VolumeL      float64
	LagMin       *float64 // nil: the lag threshold was never reached
	RecessionMin *float64 // nil: the recession threshold was not reached (yet)
}

// SegmentStorm is the public storm aggregate for one segment. When
// Suppressed is true every numeric field except HomesReporting is zero.
type SegmentStorm struct {
	SegmentID          string
	HomesReporting     int
	Suppressed         bool
	LoadLPerHome       float64
	MedianLagMin       float64 // 0 when no home reached the threshold
	MedianRecessionMin float64 // 0 when no home reached the threshold
}

// AggregateStorm builds a segment's storm aggregate. Homes are counted once
// by HomeID (the first row wins).
func AggregateStorm(segmentID string, homes []HomeStorm) SegmentStorm {
	seen := make(map[string]bool, len(homes))
	var total float64
	var lags, recessions []float64
	for _, h := range homes {
		if seen[h.HomeID] {
			continue
		}
		seen[h.HomeID] = true
		total += h.VolumeL
		if h.LagMin != nil {
			lags = append(lags, *h.LagMin)
		}
		if h.RecessionMin != nil {
			recessions = append(recessions, *h.RecessionMin)
		}
	}
	out := SegmentStorm{SegmentID: segmentID, HomesReporting: len(seen)}
	if !Visible(out.HomesReporting) {
		out.Suppressed = true
		return out
	}
	out.LoadLPerHome = total / float64(out.HomesReporting)
	out.MedianLagMin = median(lags)
	out.MedianRecessionMin = median(recessions)
	return out
}

// SegmentStatus is the live public status of one segment.
type SegmentStatus struct {
	SegmentID      string
	HomesReporting int
	Suppressed     bool
	CyclesPerHour  float64 // mean per reporting home
	ActiveAlerts   int
}

// AggregateStatus builds a segment's live status from segment totals. A
// suppressed segment hides its cycle rate and alert count too: either could
// identify a single home.
func AggregateStatus(segmentID string, homesReporting int, totalCyclesPerHour float64, activeAlerts int) SegmentStatus {
	out := SegmentStatus{SegmentID: segmentID, HomesReporting: homesReporting}
	if !Visible(homesReporting) {
		out.Suppressed = true
		return out
	}
	out.CyclesPerHour = totalCyclesPerHour / float64(homesReporting)
	out.ActiveAlerts = activeAlerts
	return out
}

func median(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	s := append([]float64(nil), xs...)
	sort.Float64s(s)
	n := len(s)
	if n%2 == 1 {
		return s[n/2]
	}
	return math.Round((s[n/2-1]+s[n/2])/2*1e9) / 1e9
}
