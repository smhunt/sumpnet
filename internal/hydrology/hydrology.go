// Package hydrology holds the pure analytics definitions of prompt_plan.md
// §9–§10, applied to node-reported pump cycles and heartbeats. Everything
// here is a function of its arguments; no I/O, no clocks.
//
// Levels are ultrasonic *distances* from the sensor to the water surface, so
// a positive drop (end − start) means the water level fell.
package hydrology

import "time"

// §10 thresholds.
const (
	CycleMinRunS       int32 = 3                // a cycle: current on for at least this long ...
	CycleMinDropMM     int32 = 20               // ... with at least this much level drop
	DryRunMinRunS      int32 = 30               // dry run: current on at least this long ...
	DryRunMaxDropMM    int32 = 5                // ... with less than this much level drop
	ShortCycleGap            = 60 * time.Second // consecutive cycles closer than this ...
	ShortCycleMinCount       = 5                // ... for at least this many cycles
	ContinuousRun            = 10 * time.Minute // current on longer than this
)

// Cycle is one pump run as the node reported it.
type Cycle struct {
	StartedAt    time.Time
	RunS         int32
	LevelStartMM int32
	LevelEndMM   int32
	Pump         string // "primary" | "backup"
}

// EndedAt is when the pump stopped.
func (c Cycle) EndedAt() time.Time { return c.StartedAt.Add(time.Duration(c.RunS) * time.Second) }

// DropMM is how far the water fell during the run (negative if it rose).
func (c Cycle) DropMM() int32 { return c.LevelEndMM - c.LevelStartMM }

// IsCycle reports whether the run meets the §10 definition of a pump cycle.
func IsCycle(c Cycle) bool { return c.RunS >= CycleMinRunS && c.DropMM() >= CycleMinDropMM }

// IsDryRun reports a failed pump or stuck check valve: a long run that moved
// no water.
func IsDryRun(c Cycle) bool { return c.RunS >= DryRunMinRunS && c.DropMM() < DryRunMaxDropMM }

// IsContinuousRun reports a run longer than §10's continuous-run threshold.
func IsContinuousRun(c Cycle) bool {
	return time.Duration(c.RunS)*time.Second > ContinuousRun
}

// ShortCyclingRun returns the length of the trailing run of cycles in which
// each cycle started less than ShortCycleGap after the previous one ended.
// "Apart" in §10 is read as the idle gap between consecutive runs. The input
// must be sorted by StartedAt; the result is 0 for no cycles and 1 for a
// cycle with no close predecessor. Short cycling is present when the result
// reaches ShortCycleMinCount.
func ShortCyclingRun(sorted []Cycle) int {
	n := len(sorted)
	if n == 0 {
		return 0
	}
	run := 1
	for i := n - 1; i > 0; i-- {
		gap := sorted[i].StartedAt.Sub(sorted[i-1].EndedAt())
		if gap >= ShortCycleGap {
			break
		}
		run++
	}
	return run
}

// EstVolumeL is §9's estimated volume per cycle: pit area × level drop
// (m² × mm = litres). Negative drops yield negative volumes; callers decide
// whether to store them.
func EstVolumeL(pitAreaM2 float64, c Cycle) float64 {
	return pitAreaM2 * float64(c.DropMM())
}

// LevelRising reports whether the water rose monotonically across the given
// sensor distances (oldest first): every step decreases the distance and the
// total decrease is at least minTotalMM. Fewer than two samples is never rising.
func LevelRising(distancesMM []int32, minTotalMM int32) bool {
	if len(distancesMM) < 2 {
		return false
	}
	for i := 1; i < len(distancesMM); i++ {
		if distancesMM[i] >= distancesMM[i-1] {
			return false
		}
	}
	return distancesMM[0]-distancesMM[len(distancesMM)-1] >= minTotalMM
}
