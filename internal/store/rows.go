package store

import (
	"time"

	"github.com/google/uuid"
)

// Meta is the radio/network metadata attached to every stored uplink.
// Pointer fields are NULL when unknown (e.g. the Wi-Fi transport has no SNR).
type Meta struct {
	RSSIDBm   *int16
	SNRDb     *float32
	SF        *int16
	GatewayID *string
	DedupID   *uuid.UUID
}

func (m Meta) values() []any {
	return []any{nilable(m.RSSIDBm), nilable(m.SNRDb), nilable(m.SF), nilable(m.GatewayID), nilable(m.DedupID)}
}

// nilable turns a nil pointer into an untyped nil (SQL NULL) for CopyFrom.
func nilable[T any](p *T) any {
	if p == nil {
		return nil
	}
	return *p
}

var metaCols = []string{"rssi_dbm", "snr_db", "sf", "gateway_id", "dedup_id"}

// Reading is one fPort 1 heartbeat row.
type Reading struct {
	DeviceID        string
	TS              time.Time
	FCnt            int64
	LevelMM         int32
	TempC           float32
	RHPct           int16
	BattMV          int32
	CyclesSinceLast int16
	MainsOK         bool
	FloatHigh       bool
	BackupRan       bool
	SensorFault     bool
	Meta
}

// CycleEvent is one fPort 2 pump-cycle row.
type CycleEvent struct {
	DeviceID     string
	StartedAt    time.Time
	FCnt         int64
	ReceivedAt   time.Time
	RunS         int32
	PeakCurrentA float32
	LevelStartMM int32
	LevelEndMM   int32
	PumpID       string // "primary" | "backup"
	Meta
}

// StormSummary is one fPort 4 roll-up row.
type StormSummary struct {
	DeviceID        string
	WindowEnd       time.Time
	FCnt            int64
	WindowS         int32
	CycleCount      int32
	TotalRunS       int32
	MaxPeakCurrentA float32
	MinLevelMM      int32
	Meta
}

// AlarmEvent is one fPort 3 alarm row.
type AlarmEvent struct {
	DeviceID string
	RaisedAt time.Time
	FCnt     int64
	Code     int16
	Value    int32
	Meta
}

// RainGaugeUplink is one fPort 5 rain gauge row, stored as reported.
type RainGaugeUplink struct {
	DeviceID     string
	TS           time.Time // event time = end of the reported interval
	FCnt         int64
	TipCount     int64
	MMPerTip     float64
	IntervalS    int32
	BattMV       int32
	CounterReset bool
	SensorFault  bool
	Meta
}

var (
	readingCols = append([]string{
		"device_id", "ts", "f_cnt", "level_mm", "temp_c", "rh_pct", "batt_mv", "cycles_since_last",
		"mains_ok", "float_high", "backup_ran", "sensor_fault",
	}, metaCols...)
	cycleEventCols = append([]string{
		"device_id", "started_at", "f_cnt", "received_at", "run_s", "peak_current_a",
		"level_start_mm", "level_end_mm", "pump_id",
	}, metaCols...)
	stormSummaryCols = append([]string{
		"device_id", "window_end", "f_cnt", "window_s", "cycle_count", "total_run_s",
		"max_peak_current_a", "min_level_mm",
	}, metaCols...)
	alarmEventCols      = append([]string{"device_id", "raised_at", "f_cnt", "code", "value"}, metaCols...)
	rainGaugeUplinkCols = append([]string{
		"device_id", "ts", "f_cnt", "tip_count", "mm_per_tip", "interval_s", "batt_mv", "counter_reset", "sensor_fault",
	}, metaCols...)
)

func (r Reading) values() []any {
	return append([]any{
		r.DeviceID, r.TS, r.FCnt, r.LevelMM, r.TempC, r.RHPct, r.BattMV, r.CyclesSinceLast,
		r.MainsOK, r.FloatHigh, r.BackupRan, r.SensorFault,
	}, r.Meta.values()...)
}

func (c CycleEvent) values() []any {
	return append([]any{
		c.DeviceID, c.StartedAt, c.FCnt, c.ReceivedAt, c.RunS, c.PeakCurrentA,
		c.LevelStartMM, c.LevelEndMM, c.PumpID,
	}, c.Meta.values()...)
}

func (s StormSummary) values() []any {
	return append([]any{
		s.DeviceID, s.WindowEnd, s.FCnt, s.WindowS, s.CycleCount, s.TotalRunS,
		s.MaxPeakCurrentA, s.MinLevelMM,
	}, s.Meta.values()...)
}

func (a AlarmEvent) values() []any {
	return append([]any{a.DeviceID, a.RaisedAt, a.FCnt, a.Code, a.Value}, a.Meta.values()...)
}

func (r RainGaugeUplink) values() []any {
	return append([]any{
		r.DeviceID, r.TS, r.FCnt, r.TipCount, r.MMPerTip, r.IntervalS, r.BattMV, r.CounterReset, r.SensorFault,
	}, r.Meta.values()...)
}
