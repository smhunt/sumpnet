package sim

import (
	"encoding/json"
	"io"
	"time"
)

// Truth is the ground truth exported alongside a run. Later phases assert
// against it: Phase 2 (no loss: TrueCycles vs ingested), Phase 3 (alarms),
// Phase 4 (lag/recession within ±10% of the realised values here).
type Truth struct {
	Version    int       `json:"version"`
	Seed       uint64    `json:"seed"`
	Scenario   string    `json:"scenario"`
	Start      time.Time `json:"start"`
	Completed  bool      `json:"completed"`
	EndedAt    time.Time `json:"ended_at"`
	Events     uint64    `json:"events"`
	StreamHash string    `json:"stream_hash"`

	Segments []SegmentParams `json:"segments"`
	Homes    []HomeParams    `json:"homes"`
	// Rainfall is one sample per virtual minute (what the rain gauges see).
	Rainfall []RainSample `json:"rainfall"`
	// RainGauges is what each gauge node counted (empty without gauges).
	RainGauges []RainGaugeTruth `json:"rain_gauges,omitempty"`
	Storms     []StormTruth     `json:"storms"`
	// TrueCycles is every pump run, including those folded into storm summaries.
	TrueCycles     []CycleTruth `json:"true_cycles"`
	ExpectedAlarms []AlarmTruth `json:"expected_alarms"`
}

// RainSample is rainfall in one virtual minute.
type RainSample struct {
	TS time.Time `json:"ts"`
	Mm float64   `json:"mm"`
}

// StormTruth is one storm event per prompt_plan.md §10 (rain >= 5 mm, gaps < 6 h).
type StormTruth struct {
	ID         string           `json:"id"`
	Onset      time.Time        `json:"onset"`    // first minute with >= 1 mm/h
	RainEnd    time.Time        `json:"rain_end"` // last minute with rain
	TotalMm    float64          `json:"total_mm"`
	PeakMmPerH float64          `json:"peak_mm_per_h"`
	Homes      []HomeStormTruth `json:"homes"`
}

// HomeStormTruth is the realised response of one home to one storm.
type HomeStormTruth struct {
	HomeIndex int `json:"home_index"`
	// From the noise-free continuous cycle rate, §10 definitions applied literally.
	// -1 means the threshold was never reached before the scenario ended.
	LagMin       float64 `json:"lag_min"`
	RecessionMin float64 `json:"recession_min"`
	// From the true cycle timestamps with a 30-minute rolling window.
	LagMinDiscrete       float64 `json:"lag_min_discrete"`
	RecessionMinDiscrete float64 `json:"recession_min_discrete"`
	VolumeL              float64 `json:"volume_l"` // storm inflow from onset to end of recession
	PeakRateCPD          float64 `json:"peak_rate_cpd"`
	Cycles               int     `json:"cycles"`
	EnteredStormMode     bool    `json:"entered_storm_mode"`
}

// CycleTruth is one true pump run.
type CycleTruth struct {
	HomeIndex int       `json:"home_index"`
	StartedAt time.Time `json:"started_at"`
	RunS      int       `json:"run_s"`
	VolumeL   float64   `json:"volume_l"`
	PumpID    uint8     `json:"pump_id"`
	DryRun    bool      `json:"dry_run"`
}

// AlarmTruth is one alarm the node raised.
type AlarmTruth struct {
	HomeIndex int       `json:"home_index"`
	Code      uint8     `json:"code"`
	At        time.Time `json:"at"`
}

// Write encodes the truth as indented JSON.
func (t *Truth) Write(w io.Writer) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(t)
}

// storm windows in minutes: [onsetMin, rainEndMin]
type stormWindowMin struct {
	firstRain, onset, rainEnd int
	total, peak               float64
}

// findStorms applies §10: contiguous rain with gaps < 6 h, total >= 5 mm.
func findStorms(rain []float64) []stormWindowMin {
	const gapMin = 6 * 60
	var out []stormWindowMin
	cur := stormWindowMin{firstRain: -1, onset: -1}
	lastRain := -1
	flush := func() {
		if cur.firstRain >= 0 && cur.total >= 5 {
			out = append(out, cur)
		}
		cur = stormWindowMin{firstRain: -1, onset: -1}
	}
	for m, r := range rain {
		if r <= 0 {
			if lastRain >= 0 && m-lastRain >= gapMin {
				flush()
				lastRain = -1
			}
			continue
		}
		if cur.firstRain < 0 {
			cur.firstRain = m
		}
		if cur.onset < 0 && r >= 1 {
			cur.onset = m
		}
		cur.rainEnd = m
		cur.total += r / 60
		cur.peak = max(cur.peak, r)
		lastRain = m
	}
	flush()
	// A storm that never reached 1 mm/h has no onset by definition; use first rain.
	for i := range out {
		if out[i].onset < 0 {
			out[i].onset = out[i].firstRain
		}
	}
	return out
}

// homeStormTruth applies the §10 lag/recession definitions to one home.
// lastMin is the last minute whose rate was recorded; a threshold not
// reached by then is -1 (never reached before the scenario ended).
func homeStormTruth(h *home, w stormWindowMin, start time.Time, lastMin int) HomeStormTruth {
	endMin := lastMin
	base := h.baseflowCPD
	t := HomeStormTruth{HomeIndex: h.p.Index, LagMin: -1, RecessionMin: -1, LagMinDiscrete: -1, RecessionMinDiscrete: -1, EnteredStormMode: h.enteredStorm}

	// Continuous, noise-free definitions.
	for m := w.onset; m <= endMin; m++ {
		if float64(h.rateNF[m]) > 2*base {
			t.LagMin = float64(m - w.onset)
			break
		}
	}
	recEnd := endMin
	for m := w.rainEnd; m <= endMin; m++ {
		if float64(h.rateNF[m]) <= 1.2*base {
			t.RecessionMin = float64(m - w.rainEnd)
			recEnd = m
			break
		}
	}
	for m := w.onset; m <= recEnd; m++ {
		t.VolumeL += float64(h.stormUL[m]) / usPerL
		t.PeakRateCPD = max(t.PeakRateCPD, float64(h.rateNF[m]))
	}

	// Discrete definitions from true cycle timestamps: 30-minute rolling count.
	const win = 30
	counts := make([]int, endMin+1)
	for _, c := range h.cycles {
		m := int(c.StartedAt.Add(time.Duration(c.RunS)*time.Second).Sub(start) / time.Minute)
		if m >= 0 && m <= endMin {
			counts[m]++
		}
	}
	rate := func(m int) float64 {
		n := 0
		for k := max(0, m-win+1); k <= m; k++ {
			n += counts[k]
		}
		return float64(n) * (1440 / win)
	}
	for m := w.onset; m <= endMin; m++ {
		if rate(m) > 2*base {
			t.LagMinDiscrete = float64(m - w.onset)
			break
		}
	}
	for m := w.rainEnd + win; m <= endMin; m++ {
		if rate(m) <= 1.2*base {
			t.RecessionMinDiscrete = float64(m - w.rainEnd)
			break
		}
	}
	for m := w.onset; m <= recEnd; m++ {
		t.Cycles += counts[m]
	}
	return t
}
