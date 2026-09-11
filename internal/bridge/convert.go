package bridge

import (
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	telemetryv1 "github.com/smhunt/sumpnet/gen/go/sumpnet/telemetry/v1"
	"github.com/smhunt/sumpnet/internal/codec"
)

// FromUplink converts a decoded wire payload into the SI-unit proto row,
// stamping it with meta and the event time. Shared by every transport.
func FromUplink(u codec.Uplink, meta *telemetryv1.UplinkMeta, eventTime time.Time) Decoded {
	ts := timestamppb.New(eventTime)
	switch v := u.(type) {
	case *codec.Heartbeat:
		return Decoded{Reading: &telemetryv1.Reading{
			Meta:            meta,
			Ts:              ts,
			LevelMm:         uint32(v.LevelMM),
			TempC:           float64(v.TempCentiC) / 100,
			RhPct:           uint32(v.RHPct),
			BattMv:          uint32(v.BattMV),
			CyclesSinceLast: uint32(v.CyclesSinceLast),
			Flags: &telemetryv1.ReadingFlags{
				MainsOk:     v.Flags.Has(codec.FlagMainsOK),
				FloatHigh:   v.Flags.Has(codec.FlagFloatHigh),
				BackupRan:   v.Flags.Has(codec.FlagBackupRan),
				SensorFault: v.Flags.Has(codec.FlagSensorFault),
			},
		}}
	case *codec.CycleEvent:
		pump := telemetryv1.PumpId_PUMP_ID_PRIMARY
		if v.PumpID == codec.PumpBackup {
			pump = telemetryv1.PumpId_PUMP_ID_BACKUP
		}
		return Decoded{Cycle: &telemetryv1.CycleEvent{
			Meta:         meta,
			StartedAt:    timestamppb.New(eventTime.Add(-time.Duration(v.StartOffsetS) * time.Second)),
			RunS:         uint32(v.RunS),
			PeakCurrentA: float64(v.PeakCurrentDA) / 10,
			LevelStartMm: uint32(v.LevelStartMM),
			LevelEndMm:   uint32(v.LevelEndMM),
			PumpId:       pump,
		}}
	case *codec.Alarm:
		return Decoded{Alarm: &telemetryv1.Alarm{
			Meta:     meta,
			RaisedAt: ts,
			Code:     telemetryv1.AlarmCode(v.Code),
			Value:    uint32(v.Value),
		}}
	case *codec.StormSummary:
		return Decoded{Summary: &telemetryv1.StormSummary{
			Meta:            meta,
			WindowEnd:       ts,
			WindowS:         uint32(v.WindowS),
			Count:           uint32(v.Count),
			TotalRunS:       uint32(v.TotalRunS),
			MaxPeakCurrentA: float64(v.MaxPeakCurrentDA) / 10,
			MinLevelMm:      uint32(v.MinLevelMM),
		}}
	}
	return Decoded{}
}
