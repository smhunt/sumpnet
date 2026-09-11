package sim

import (
	"fmt"
	"math"
	"math/rand/v2"
	"time"

	"github.com/google/uuid"

	"github.com/smhunt/sumpnet/internal/codec"
)

// PumpHealth is the condition of a home's primary pump.
type PumpHealth uint8

// Pump conditions.
const (
	PumpHealthy PumpHealth = iota
	PumpDryRunning
	PumpShortCycling
	PumpFailing
)

func (h PumpHealth) String() string {
	switch h {
	case PumpHealthy:
		return "healthy"
	case PumpDryRunning:
		return "dry_running"
	case PumpShortCycling:
		return "short_cycling"
	case PumpFailing:
		return "failing"
	}
	return fmt.Sprintf("PumpHealth(%d)", uint8(h))
}

// MarshalText implements encoding.TextMarshaler.
func (h PumpHealth) MarshalText() ([]byte, error) { return []byte(h.String()), nil }

// UnmarshalText implements encoding.TextUnmarshaler (scenario files).
func (h *PumpHealth) UnmarshalText(b []byte) error {
	for _, c := range []PumpHealth{PumpHealthy, PumpDryRunning, PumpShortCycling, PumpFailing} {
		if c.String() == string(b) {
			*h = c
			return nil
		}
	}
	return fmt.Errorf("sim: unknown pump health %q", b)
}

// HomeParams are the ground-truth parameters of one simulated home. Storm
// analytics (Phase 4) must recover the realised lag/recession derived from
// these to within ±10%.
type HomeParams struct {
	Index     int    `json:"index"`
	HomeID    string `json:"home_id"`
	DevEUI    string `json:"dev_eui"`
	DevAddr   string `json:"dev_addr"`
	SegmentID string `json:"segment_id"`

	PitAreaM2        float64 `json:"pit_area_m2"`
	SensorHeightMM   int     `json:"sensor_height_mm"`    // sensor face to pit floor
	TriggerDepthMM   int     `json:"trigger_depth_mm"`    // primary float on
	StopDepthMM      int     `json:"stop_depth_mm"`       // primary float off
	FloatHighDepthMM int     `json:"float_high_depth_mm"` // independent high-water float

	BaseflowCPD      float64 `json:"baseflow_cycles_per_day"`
	ResponseLPerMM   float64 `json:"response_l_per_mm"` // tile inflow per mm of rain
	DelayMin         float64 `json:"delay_min"`         // pure delay rain → tile
	ReservoirMin     float64 `json:"reservoir_min"`     // linear-reservoir time constant
	PumpLPS          float64 `json:"pump_l_per_s"`
	NominalCurrentDA int     `json:"nominal_current_da"`

	Health         PumpHealth    `json:"health"`
	HasBackup      bool          `json:"has_backup"`
	SensorOffsetMM int           `json:"sensor_offset_mm"`
	RSSIBase       [2]int        `json:"rssi_base"`
	HeartbeatPhase time.Duration `json:"heartbeat_phase"`
}

// CycleVolumeL is the volume pumped per normal cycle (§9 formula identity).
func (p HomeParams) CycleVolumeL() float64 {
	return p.PitAreaM2 * float64(p.TriggerDepthMM-p.StopDepthMM)
}

const (
	heartbeatInterval = 15 * time.Minute
	stormWindow       = 900 // seconds
	stormThreshold    = 6   // more than this many cycles per window enters storm mode
	alarmHoldoff      = 3600
	continuousRunS    = 600
	dryRunMinS        = 30
	dryRunMaxDropMM   = 5
	thermalRunS       = 45
	thermalRestS      = 120
	backflowFraction  = 0.85
	backflowSeconds   = 20
	battFullMV        = 4150
	battOutageDrainMV = 550 // over 24 h
	usPerL            = 1_000_000
)

// home is the mutable simulation state of one house: hydrology, pump(s) and
// the emulated node firmware. All volumes are int64 microlitres and depths are
// derived from volume / area so the model is integer-exact.
type home struct {
	p   HomeParams
	seg *SegmentParams
	rng *rand.Rand

	// derived constants
	areaMM2       int64
	triggerVol    int64
	stopVol       int64
	floatHighVol  int64
	backupVol     int64
	cycleVol      int64
	qBase         int64 // µL/s
	respULsPerMMH float64
	delayMin      int
	reservoirS    int64
	pumpULps      int64
	baseflowCPD   float64 // after scenario/segment multipliers
	scenarioSecs  int

	// hydrology state
	vol      int64
	storage  int64 // linear reservoir
	noise    float64
	qStorm   int64
	backflow int64 // µL still to flow back (short cycling)
	backPerS int64 // µL returned per second while backflow > 0

	// pump state
	primaryOn        bool
	primaryOnStep    int
	primaryStartVol  int64
	primaryStartDist int
	primaryPeakDA    int
	backupOn         bool
	backupOnStep     int
	backupStartDist  int
	thermalOffUntil  int
	continuousRaised bool
	backupRanSinceHB bool
	mainsOK          bool
	outageStartStep  int

	// node firmware state
	fcnt          uint32
	nextHeartbeat int
	cyclesSinceHB int
	stormMode     bool
	windowStart   int
	recentEnds    []int
	accCount      int
	accRunS       int
	accPeakDA     int
	accMinLevel   int
	lastAlarmStep map[codec.AlarmCode]int
	floatHigh     bool // edge state for the float-high alarm

	// truth
	rateNF       []float32 // noise-free cycles/day per minute
	stormUL      []int64   // storm inflow µL per minute
	cycles       []CycleTruth
	alarms       []AlarmTruth
	enteredStorm bool
}

type rngReader struct{ r *rand.Rand }

func (r rngReader) Read(b []byte) (int, error) {
	for i := range b {
		b[i] = byte(r.r.Uint32())
	}
	return len(b), nil
}

var homeNamespace = uuid.NewSHA1(uuid.NameSpaceURL, []byte("https://github.com/smhunt/sumpnet/sim"))

func newHome(index int, seg *SegmentParams, scn *Scenario, seed uint64, nSteps int) *home {
	r := rand.New(rand.NewPCG(seed, uint64(index)+1)) //nolint:gosec // deterministic simulation, not security
	unif := func(lo, hi float64) float64 { return lo + (hi-lo)*r.Float64() }

	p := HomeParams{
		Index:     index,
		HomeID:    uuid.NewSHA1(homeNamespace, []byte(fmt.Sprintf("home:%d:%d", seed, index))).String(),
		DevEUI:    fmt.Sprintf("70b3d57ed0%06x", index),
		DevAddr:   fmt.Sprintf("01a3b%03x", index),
		SegmentID: seg.ID,
	}
	// About one in six pits is a 24" basin.
	if r.IntN(6) == 0 {
		p.PitAreaM2 = unif(0.27, 0.31)
	} else {
		p.PitAreaM2 = unif(0.148, 0.180)
	}
	p.SensorHeightMM = int(unif(650, 800))
	p.StopDepthMM = int(unif(220, 280))
	p.TriggerDepthMM = p.StopDepthMM + int(unif(130, 170))
	p.FloatHighDepthMM = p.TriggerDepthMM + int(unif(100, 140))
	p.BaseflowCPD = unif(1, 12) * seg.BaseflowMul * scn.mul(scn.BaseflowMul)
	p.ResponseLPerMM = unif(60, 160) * seg.ResponseMul * scn.mul(scn.ResponseMul)
	p.DelayMin = unif(5, 90)
	p.ReservoirMin = unif(30, 300) * seg.ReservoirMul * scn.mul(scn.ReservoirMul)
	p.PumpLPS = unif(0.5, 1.2)
	p.NominalCurrentDA = int(unif(45, 90))
	p.HasBackup = r.IntN(4) == 0
	p.SensorOffsetMM = int(unif(-15, 15))
	p.RSSIBase = [2]int{int(unif(-118, -70)), int(unif(-118, -70))}
	p.HeartbeatPhase = time.Duration(r.IntN(int(heartbeatInterval/time.Second))) * time.Second
	if h, ok := scn.HealthOverrides[index]; ok {
		p.Health = h
	}
	if p.Health == PumpShortCycling {
		// Float fault: the switch sticks near the trigger level, so every cycle
		// moves only 20 mm and the leaking check valve refills most of it.
		p.StopDepthMM = p.TriggerDepthMM - 20
	}
	if p.ReservoirMin < 30 {
		p.ReservoirMin = 30 // explicit Euler at 1 s stays stable
	}

	h := &home{p: p, seg: seg, rng: r, scenarioSecs: nSteps, mainsOK: true}
	h.areaMM2 = int64(math.Round(p.PitAreaM2 * 1e6))
	h.triggerVol = int64(p.TriggerDepthMM) * h.areaMM2
	h.stopVol = int64(p.StopDepthMM) * h.areaMM2
	h.floatHighVol = int64(p.FloatHighDepthMM) * h.areaMM2
	h.backupVol = (h.triggerVol + h.floatHighVol) / 2
	h.cycleVol = h.triggerVol - h.stopVol
	h.baseflowCPD = p.BaseflowCPD
	h.qBase = int64(math.Round(p.BaseflowCPD * float64(h.cycleVol) / 86400))
	h.respULsPerMMH = p.ResponseLPerMM * usPerL / 3600
	h.delayMin = int(math.Round(p.DelayMin))
	h.reservoirS = int64(math.Round(p.ReservoirMin * 60))
	h.pumpULps = int64(math.Round(p.PumpLPS * usPerL))
	h.vol = h.stopVol + h.cycleVol/2
	h.nextHeartbeat = int(p.HeartbeatPhase / time.Second)
	h.lastAlarmStep = map[codec.AlarmCode]int{}
	mins := nSteps/60 + 1
	h.rateNF = make([]float32, mins)
	h.stormUL = make([]int64, mins)
	return h
}

// depthMM is the current water depth above the pit floor.
func (h *home) depthMM() int { return int(h.vol / h.areaMM2) }

// distanceMM is what the ultrasonic sensor reports: sensor face to water
// surface, with calibration offset and ±2 mm noise.
func (h *home) distanceMM() int {
	d := h.p.SensorHeightMM - h.depthMM() + h.p.SensorOffsetMM + h.rng.IntN(5) - 2
	if d < 0 {
		d = 0
	}
	if d > 65535 {
		d = 65535
	}
	return d
}

// primaryRate is the effective primary pump output at this step.
func (h *home) primaryRate(step int) int64 {
	switch h.p.Health {
	case PumpDryRunning:
		return 0
	case PumpFailing:
		// Output decays linearly and is gone by 90% of the scenario, so the
		// last stretch exercises continuous-run and float-high on a dead pump.
		progress := float64(step) / float64(h.scenarioSecs)
		return int64(float64(h.pumpULps) * math.Max(0, 1-progress/0.9))
	default:
		return h.pumpULps
	}
}

func (h *home) peakCurrentDA() int {
	factor := 1.0
	switch h.p.Health {
	case PumpDryRunning:
		factor = 0.6
	case PumpFailing:
		factor = 1.3
	}
	jitter := 1 + (h.rng.Float64()*0.06 - 0.03)
	return int(math.Round(float64(h.p.NominalCurrentDA) * factor * jitter))
}

// step advances the home by one virtual second and returns any uplinks.
// rainMMH is the neighbourhood rainfall intensity at (now - delay) is looked
// up by the caller via rainAt.
func (h *home) step(step int, now time.Time, rainAt func(minute int) float64, mains bool, out []Event) []Event {
	minute := step / 60

	// --- hydrology -------------------------------------------------------
	delayedMin := minute - h.delayMin
	var excess int64
	if delayedMin >= 0 {
		excess = int64(h.respULsPerMMH * rainAt(delayedMin))
	}
	h.storage += excess - h.storage/h.reservoirS
	if h.storage < 0 {
		h.storage = 0
	}
	h.qStorm = h.storage / h.reservoirS

	// AR(1) noise on baseflow so dry-weather cycles are not metronomic.
	h.noise = 0.9*h.noise + 0.05*h.rng.NormFloat64()
	qBaseNoisy := int64(float64(h.qBase) * (1 + h.noise))
	h.vol += qBaseNoisy + h.qStorm

	if h.backflow > 0 {
		back := min(h.backPerS, h.backflow)
		h.vol += back
		h.backflow -= back
	}

	if step%60 == 0 {
		h.rateNF[minute] = float32(float64(h.qBase+h.qStorm) * 86400 / float64(h.cycleVol))
		h.stormUL[minute] = h.qStorm * 60
	}

	// --- mains -------------------------------------------------------------
	if mains != h.mainsOK {
		h.mainsOK = mains
		if !mains {
			h.outageStartStep = step
			out = h.raiseAlarm(step, now, codec.AlarmMainsLost, uint16(h.battMV(step)), out)
		}
	}

	// --- primary pump ------------------------------------------------------
	if !h.primaryOn && h.mainsOK && step >= h.thermalOffUntil && h.vol >= h.triggerVol {
		h.primaryOn = true
		h.primaryOnStep = step
		h.primaryStartVol = h.vol
		h.primaryStartDist = h.distanceMM()
		h.primaryPeakDA = h.peakCurrentDA()
		h.continuousRaised = false
	}
	if h.primaryOn {
		h.vol -= h.primaryRate(step)
		if h.vol < 0 {
			h.vol = 0
		}
		run := step - h.primaryOnStep
		floatOff := h.vol <= h.stopVol
		if floatOff {
			h.vol = h.stopVol // the float switch cuts the pump exactly at its level
		}
		stop := floatOff || !h.mainsOK
		if h.p.Health == PumpDryRunning && run >= thermalRunS {
			stop = true
			h.thermalOffUntil = step + thermalRestS
		}
		if !h.continuousRaised && run > continuousRunS {
			h.continuousRaised = true
			out = h.raiseAlarm(step, now, codec.AlarmContinuousRun, uint16(min(run, 65535)), out)
		}
		if stop {
			h.primaryOn = false
			out = h.endCycle(step, now, codec.PumpPrimary, run, h.primaryStartDist, h.primaryPeakDA, out)
			pumped := h.primaryStartVol - h.vol
			if h.p.Health == PumpShortCycling && pumped > 0 {
				h.backflow = int64(float64(pumped) * backflowFraction)
				h.backPerS = max(1, h.backflow/backflowSeconds)
			}
		}
	}

	// --- backup pump (battery powered) ------------------------------------
	if h.p.HasBackup {
		if !h.backupOn && h.vol >= h.backupVol {
			h.backupOn = true
			h.backupOnStep = step
			h.backupStartDist = h.distanceMM()
		}
		if h.backupOn {
			h.vol -= h.pumpULps
			if h.vol < 0 {
				h.vol = 0
			}
			if h.vol <= h.stopVol {
				h.vol = h.stopVol
				h.backupOn = false
				h.backupRanSinceHB = true
				out = h.endCycle(step, now, codec.PumpBackup, step-h.backupOnStep, h.backupStartDist, h.p.NominalCurrentDA, out)
			}
		}
	}

	// --- float-high alarm ----------------------------------------------------
	high := h.vol >= h.floatHighVol
	if high && !h.floatHigh {
		out = h.raiseAlarm(step, now, codec.AlarmFloatHigh, uint16(h.distanceMM()), out)
	}
	h.floatHigh = high

	// --- node: storm window, heartbeat -----------------------------------------
	if h.stormMode && step-h.windowStart >= stormWindow {
		out = h.emitStormSummary(step, now, out)
	}
	if step >= h.nextHeartbeat {
		out = h.emitHeartbeat(step, now, high, out)
		h.nextHeartbeat += int(heartbeatInterval / time.Second)
	}
	return out
}

func (h *home) battMV(step int) int {
	if h.mainsOK {
		return battFullMV + h.rng.IntN(21) - 10
	}
	drain := int64(step-h.outageStartStep) * battOutageDrainMV / 86400
	return battFullMV - int(drain)
}

// endCycle records a completed pump run and either sends it as an fPort 2
// event or folds it into the current storm-mode window.
func (h *home) endCycle(step int, now time.Time, pump codec.PumpID, runS, startDist, peakDA int, out []Event) []Event {
	endDist := h.distanceMM()
	drop := endDist - startDist
	dry := runS >= dryRunMinS && drop < dryRunMaxDropMM
	h.cycles = append(h.cycles, CycleTruth{
		HomeIndex: h.p.Index, StartedAt: now.Add(-time.Duration(runS) * time.Second), RunS: runS,
		VolumeL: float64(h.cycleVol) / usPerL, PumpID: uint8(pump), DryRun: dry,
	})
	h.cyclesSinceHB++
	if dry {
		out = h.raiseAlarm(step, now, codec.AlarmDryRun, uint16(min(runS, 65535)), out)
	}

	// Storm-mode bookkeeping (firmware parity: see prompt_plan.md §5).
	keep := h.recentEnds[:0]
	for _, t := range h.recentEnds {
		if step-t < stormWindow {
			keep = append(keep, t)
		}
	}
	h.recentEnds = append(keep, step)
	if !h.stormMode && len(h.recentEnds) > stormThreshold {
		h.stormMode = true
		h.enteredStorm = true
		h.windowStart = step
		h.resetAccumulator()
	}
	if h.stormMode {
		h.accCount++
		h.accRunS += runS
		h.accPeakDA = max(h.accPeakDA, peakDA)
		h.accMinLevel = min(h.accMinLevel, startDist)
		return out
	}

	txDelay := h.rng.IntN(6)
	ev := codec.CycleEvent{
		StartOffsetS:  uint16(min(runS+txDelay, 65535)),
		RunS:          uint16(min(runS, 65535)),
		PeakCurrentDA: uint16(min(peakDA, 65535)),
		LevelStartMM:  uint16(startDist),
		LevelEndMM:    uint16(endDist),
		PumpID:        pump,
	}
	return append(out, h.uplink(now.Add(time.Duration(txDelay)*time.Second), &ev, false))
}

func (h *home) resetAccumulator() {
	h.accCount, h.accRunS, h.accPeakDA, h.accMinLevel = 0, 0, 0, 65535
}

func (h *home) emitStormSummary(step int, now time.Time, out []Event) []Event {
	s := codec.StormSummary{
		Count:            uint8(min(h.accCount, 255)),
		WindowS:          stormWindow,
		TotalRunS:        uint16(min(h.accRunS, 65535)),
		MaxPeakCurrentDA: uint16(min(h.accPeakDA, 65535)),
		MinLevelMM:       uint16(h.accMinLevel),
	}
	out = append(out, h.uplink(now, &s, false))
	if h.accCount <= stormThreshold {
		h.stormMode = false
	}
	h.windowStart = step
	h.resetAccumulator()
	return out
}

func (h *home) emitHeartbeat(step int, now time.Time, high bool, out []Event) []Event {
	hour := now.Sub(now.Truncate(24*time.Hour)).Hours() + float64(h.p.Index%7) // per-home basement offset
	temp := 18 + 2*math.Sin(2*math.Pi*(hour-6)/24) + h.rng.NormFloat64()*0.1
	var flags codec.Flags
	if h.mainsOK {
		flags |= codec.FlagMainsOK
	}
	if high {
		flags |= codec.FlagFloatHigh
	}
	if h.backupRanSinceHB {
		flags |= codec.FlagBackupRan
	}
	hb := codec.Heartbeat{
		LevelMM:         uint16(h.distanceMM()),
		TempCentiC:      int16(math.Round(temp * 100)),
		RHPct:           uint8(45 + h.rng.IntN(21)),
		BattMV:          uint16(h.battMV(step)),
		CyclesSinceLast: uint8(min(h.cyclesSinceHB, 255)),
		Flags:           flags,
	}
	h.cyclesSinceHB = 0
	h.backupRanSinceHB = false
	return append(out, h.uplink(now, &hb, false))
}

func (h *home) raiseAlarm(step int, now time.Time, code codec.AlarmCode, value uint16, out []Event) []Event {
	if last, ok := h.lastAlarmStep[code]; ok && step-last < alarmHoldoff {
		return out
	}
	h.lastAlarmStep[code] = step
	h.alarms = append(h.alarms, AlarmTruth{HomeIndex: h.p.Index, Code: uint8(code), At: now})
	a := codec.Alarm{Code: code, Value: value}
	return append(out, h.uplink(now, &a, true))
}

// uplink encodes a payload as the node would transmit it, with RF metadata.
func (h *home) uplink(at time.Time, u codec.Uplink, confirmed bool) Event {
	port, payload, err := codec.Encode(u)
	if err != nil {
		panic(fmt.Sprintf("sim: home %d produced an unencodable %T: %v", h.p.Index, u, err))
	}
	fcnt := h.fcnt
	h.fcnt++

	var rssi [2]int32
	var snr [2]float32
	best := -200
	for i := range rssi {
		v := h.p.RSSIBase[i] + h.rng.IntN(7) - 3
		rssi[i] = int32(v)
		s := float64(v+115) / 2
		s = math.Max(-15, math.Min(10, s)) + (h.rng.Float64()*2 - 1)
		snr[i] = float32(math.Round(s*4) / 4)
		best = max(best, v)
	}
	sf, dr := uint8(10), uint8(0)
	switch {
	case best > -95:
		sf, dr = 7, 3
	case best > -105:
		sf, dr = 8, 2
	case best > -112:
		sf, dr = 9, 1
	}
	ch := (int(fcnt) + h.p.Index) % 8
	id, err := uuid.NewRandomFromReader(rngReader{h.rng})
	if err != nil {
		panic("sim: uuid from rng: " + err.Error())
	}
	return Event{
		Time:      at,
		HomeIndex: h.p.Index,
		DevEUI:    h.p.DevEUI,
		DevAddr:   h.p.DevAddr,
		FCnt:      fcnt,
		FPort:     port,
		Confirmed: confirmed,
		Payload:   payload,
		SF:        sf,
		DR:        dr,
		FreqHz:    uint32(902_300_000 + 200_000*ch),
		RSSI:      rssi,
		SNR:       snr,
		DedupID:   id.String(),
	}
}
