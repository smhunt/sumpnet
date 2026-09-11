package sim

import (
	"fmt"
	"sort"
	"time"
)

// Outage cuts mains power to whole segments for a period.
type Outage struct {
	SegmentIndexes []int         `json:"segment_indexes"`
	At             time.Duration `json:"at"`
	Duration       time.Duration `json:"duration"`
}

// Scenario is a complete description of one simulated period. Scenarios are
// Go data so they are versioned with the code that interprets them.
type Scenario struct {
	Name        string        `json:"name"`
	Description string        `json:"description"`
	Duration    time.Duration `json:"duration"`
	Rain        Hyetograph    `json:"rain"`
	// RainJitter is the ± fraction applied per minute from the scenario RNG.
	RainJitter float64 `json:"rain_jitter"`
	// Multipliers on every home's parameters (1.0 = unchanged).
	BaseflowMul  float64  `json:"baseflow_mul"`
	ReservoirMul float64  `json:"reservoir_mul"`
	ResponseMul  float64  `json:"response_mul"`
	Outages      []Outage `json:"outages,omitempty"`
	// HealthOverrides pins specific homes (by index) to a pump condition.
	HealthOverrides map[int]PumpHealth `json:"health_overrides,omitempty"`
}

func (s *Scenario) validate() error {
	if s.Name == "" {
		return fmt.Errorf("sim: scenario has no name")
	}
	if s.Duration <= 0 {
		return fmt.Errorf("sim: scenario %q: duration must be positive", s.Name)
	}
	for i := 1; i < len(s.Rain); i++ {
		if s.Rain[i].At < s.Rain[i-1].At {
			return fmt.Errorf("sim: scenario %q: rain breakpoints not sorted", s.Name)
		}
	}
	return nil
}

func (s *Scenario) mul(v float64) float64 {
	if v == 0 {
		return 1
	}
	return v
}

// Storm shapes (mm/h), later normalised to their total.
var (
	// 2 h front-loaded burst peaking 30 min in.
	burstShape = Hyetograph{{0, 0}, {10 * time.Minute, 10}, {30 * time.Minute, 26}, {60 * time.Minute, 17}, {90 * time.Minute, 6.5}, {120 * time.Minute, 0}}
	// 6 h storm: ramp, plateau, second peak, tail.
	longShape = Hyetograph{{0, 0}, {1 * time.Hour, 15}, {2 * time.Hour, 20}, {3 * time.Hour, 12}, {4 * time.Hour, 25}, {5 * time.Hour, 10}, {6 * time.Hour, 0}}
)

// Scenarios returns the built-in scenarios keyed by name.
func Scenarios() map[string]*Scenario {
	storm50 := &Scenario{
		Name:        "storm50",
		Description: "50 mm spring storm over 6 h after a 6 h dry lead; the Phase 1 acceptance scenario",
		Duration:    24 * time.Hour,
		Rain:        longShape.Normalised(50).Shifted(6 * time.Hour),
		RainJitter:  0.05,
	}
	list := []*Scenario{
		{
			Name:        "dry-week",
			Description: "Seven dry days: pure baseflow, no alarms expected on healthy homes",
			Duration:    7 * 24 * time.Hour,
		},
		{
			Name:        "storm25",
			Description: "25 mm summer burst (2 h) after a 6 h dry lead",
			Duration:    24 * time.Hour,
			Rain:        burstShape.Normalised(25).Shifted(6 * time.Hour),
			RainJitter:  0.05,
		},
		storm50,
		{
			Name:        "thaw50",
			Description: "Spring thaw: 72 h dry lead at a high water table, then 12 h of steady rain (48 mm) and a 2 mm shower",
			Duration:    108 * time.Hour,
			Rain: Hyetograph{{72 * time.Hour, 0}, {72*time.Hour + 30*time.Minute, 4}, {84 * time.Hour, 4}, {84*time.Hour + 30*time.Minute, 0}}.
				Append(Hyetograph{{90 * time.Hour, 0}, {90*time.Hour + 15*time.Minute, 8}, {90*time.Hour + 30*time.Minute, 0}}).
				Normalised(50),
			RainJitter:   0.05,
			BaseflowMul:  2.0,
			ReservoirMul: 1.5,
			ResponseMul:  1.4,
		},
		{
			Name:        "outage",
			Description: "storm50 with a 3 h power outage on segments 3-4 starting 90 min after rain onset",
			Duration:    storm50.Duration,
			Rain:        storm50.Rain,
			RainJitter:  storm50.RainJitter,
			Outages:     []Outage{{SegmentIndexes: []int{2, 3}, At: 7*time.Hour + 30*time.Minute, Duration: 3 * time.Hour}},
		},
		{
			Name:        "failing-pump",
			Description: "Seven days with a 25 mm storm on day 4; homes 7 (failing), 9 (dry-running) and 11 (short-cycling) are unhealthy",
			Duration:    7 * 24 * time.Hour,
			Rain:        burstShape.Normalised(25).Shifted(3*24*time.Hour + 6*time.Hour),
			RainJitter:  0.05,
			HealthOverrides: map[int]PumpHealth{
				7:  PumpFailing,
				9:  PumpDryRunning,
				11: PumpShortCycling,
			},
		},
	}
	out := make(map[string]*Scenario, len(list))
	for _, s := range list {
		out[s.Name] = s
	}
	return out
}

// ScenarioNames returns the built-in scenario names, sorted.
func ScenarioNames() []string {
	m := Scenarios()
	names := make([]string, 0, len(m))
	for n := range m {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}
