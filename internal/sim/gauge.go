package sim

import (
	"fmt"
	"math"
	"math/rand/v2"
	"time"

	"github.com/google/uuid"

	"github.com/smhunt/sumpnet/internal/codec"
)

// DeviceKindRain marks rain-gauge events (Event.Kind) and devices.kind rows.
const DeviceKindRain = "rain"

// Rain gauge node firmware (prompt_plan.md §5, fPort 5): wake every 5 min,
// transmit when the bucket tipped since the previous uplink, otherwise at
// least every 15 min.
const (
	gaugeWakeS        = 300
	gaugeDryIntervalS = 900
	gaugeTipUM        = 200
	// PCG stream offset for gauges; homes use streams 1..N and the scenario 0.
	gaugeStream = 1 << 40
)

// RainGaugeParams describes one simulated tipping-bucket gauge node.
type RainGaugeParams struct {
	Index      int           `json:"index"`
	DevEUI     string        `json:"dev_eui"`
	DevAddr    string        `json:"dev_addr"`
	Name       string        `json:"name"`
	Location   string        `json:"location"`
	MMPerTipUM int           `json:"mm_per_tip_um"`
	WakePhase  time.Duration `json:"wake_phase"`
	RSSIBase   [2]int        `json:"rssi_base"`
}

// RainGaugeTruth is what one gauge counted over the run.
type RainGaugeTruth struct {
	RainGaugeParams
	Tips    uint32  `json:"tips"`
	TotalMm float64 `json:"total_mm"`
	Uplinks int     `json:"uplinks"`
}

// gaugeLocations places the gauges at opposite ends of the neighbourhood (§4).
var gaugeLocations = []string{"north end", "south end"}

// gauge is the state of one rain gauge node. Every gauge sees the
// neighbourhood's true rain series; only the tip quantisation and the uplink
// phase differ between them.
type gauge struct {
	p   RainGaugeParams
	rng *rand.Rand

	tipNM      int64  // rain per tip, nanometres
	accNM      int64  // rain in the bucket that has not tipped yet
	tips       uint32 // cumulative since boot
	lastTxStep int    // boot counts as step 0
	lastTxTips uint32
	nextWake   int
	booted     bool // false until the first uplink, which carries counter_reset
	fcnt       uint32
	uplinks    int
}

func newGauge(index int, seed uint64) *gauge {
	r := rand.New(rand.NewPCG(seed, gaugeStream+uint64(index))) //nolint:gosec // deterministic simulation, not security
	loc := fmt.Sprintf("gauge %d", index+1)
	if index < len(gaugeLocations) {
		loc = gaugeLocations[index]
	}
	p := RainGaugeParams{
		Index:      index,
		DevEUI:     fmt.Sprintf("70b3d57ed1%06x", index),
		DevAddr:    fmt.Sprintf("01a3c%03x", index),
		Name:       fmt.Sprintf("sim-rain-%02d", index+1),
		Location:   loc,
		MMPerTipUM: gaugeTipUM,
		WakePhase:  time.Duration(30+r.IntN(gaugeWakeS-30)) * time.Second,
		RSSIBase:   [2]int{-100 + r.IntN(25), -100 + r.IntN(25)},
	}
	return &gauge{p: p, rng: r, tipNM: int64(p.MMPerTipUM) * 1000, nextWake: int(p.WakePhase / time.Second)}
}

// step advances the gauge by one virtual second and returns any uplink.
func (g *gauge) step(step int, now time.Time, rainAt func(minute int) float64, out []Event) []Event {
	if step%60 == 59 {
		// Minute m's rain has fallen into the bucket by the end of minute m.
		g.accNM += int64(math.Round(rainAt(step/60) * 1e6 / 60))
		if n := g.accNM / g.tipNM; n > 0 {
			g.tips += uint32(n) //nolint:gosec // a few tips per minute
			g.accNM -= n * g.tipNM
		}
	}
	if step < g.nextWake {
		return out
	}
	g.nextWake += gaugeWakeS
	if g.booted && g.tips == g.lastTxTips && step-g.lastTxStep < gaugeDryIntervalS {
		return out
	}
	var flags codec.RainFlags
	if !g.booted {
		flags |= codec.RainCounterReset
	}
	rg := codec.RainGauge{
		TipCount:   g.tips,
		MMPerTipUM: uint16(g.p.MMPerTipUM), //nolint:gosec // 200
		IntervalS:  uint16(min(step-g.lastTxStep, 65535)),
		BattMV:     uint16(3600 + g.rng.IntN(21) - 10),
		Flags:      flags,
	}
	g.booted = true
	g.lastTxStep, g.lastTxTips = step, g.tips
	return append(out, g.uplink(now, &rg))
}

func (g *gauge) uplink(at time.Time, u codec.Uplink) Event {
	port, payload, err := codec.Encode(u)
	if err != nil {
		panic(fmt.Sprintf("sim: gauge %d produced an unencodable %T: %v", g.p.Index, u, err))
	}
	fcnt := g.fcnt
	g.fcnt++
	g.uplinks++
	rssi, snr, sf, dr := radio(g.rng, g.p.RSSIBase)
	ch := (int(fcnt) + g.p.Index + 3) % 8
	id, err := uuid.NewRandomFromReader(rngReader{g.rng})
	if err != nil {
		panic("sim: uuid from rng: " + err.Error())
	}
	return Event{
		Time:       at,
		HomeIndex:  -1,
		DevEUI:     g.p.DevEUI,
		DevAddr:    g.p.DevAddr,
		FCnt:       fcnt,
		FPort:      port,
		Payload:    payload,
		SF:         sf,
		DR:         dr,
		FreqHz:     uint32(902_300_000 + 200_000*ch), //nolint:gosec // ch < 8
		RSSI:       rssi,
		SNR:        snr,
		DedupID:    id.String(),
		Kind:       DeviceKindRain,
		DeviceName: g.p.Name,
	}
}

func (g *gauge) truth() RainGaugeTruth {
	return RainGaugeTruth{RainGaugeParams: g.p, Tips: g.tips, TotalMm: float64(g.tips) * float64(g.p.MMPerTipUM) / 1000, Uplinks: g.uplinks}
}

// radio draws per-uplink RF metadata for two gateways. The order of RNG calls
// is part of the determinism contract: changing it changes every stream hash.
func radio(r *rand.Rand, base [2]int) (rssi [2]int32, snr [2]float32, sf, dr uint8) {
	best := -200
	for i := range rssi {
		v := base[i] + r.IntN(7) - 3
		rssi[i] = int32(v) //nolint:gosec // dBm
		s := float64(v+115) / 2
		s = math.Max(-15, math.Min(10, s)) + (r.Float64()*2 - 1)
		snr[i] = float32(math.Round(s*4) / 4)
		best = max(best, v)
	}
	sf, dr = 10, 0
	switch {
	case best > -95:
		sf, dr = 7, 3
	case best > -105:
		sf, dr = 8, 2
	case best > -112:
		sf, dr = 9, 1
	}
	return rssi, snr, sf, dr
}
