package ingest

import (
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"

	telemetryv1 "github.com/smhunt/sumpnet/gen/go/sumpnet/telemetry/v1"
	"github.com/smhunt/sumpnet/internal/store"
)

var devEUIRe = regexp.MustCompile(`^[0-9a-f]{16}$`)

// reject is why a row was not stored; it becomes a metrics label.
type reject string

const (
	rejNoMeta   reject = "no_meta"
	rejDevEUI   reject = "bad_dev_eui"
	rejNoTime   reject = "no_time"
	rejFuture   reject = "future"
	rejBadPump  reject = "bad_pump_id"
	rejBadCode  reject = "bad_alarm_code"
	rejBadValue reject = "bad_value"
)

// metaOf validates UplinkMeta and converts it to the store's shape.
func (s *Server) metaOf(m *telemetryv1.UplinkMeta) (devEUI string, received time.Time, meta store.Meta, why reject) {
	if m == nil {
		return "", time.Time{}, meta, rejNoMeta
	}
	devEUI = strings.ToLower(m.GetDevEui())
	if !devEUIRe.MatchString(devEUI) {
		return "", time.Time{}, meta, rejDevEUI
	}
	if m.GetReceivedAt() == nil {
		return "", time.Time{}, meta, rejNoTime
	}
	received = m.GetReceivedAt().AsTime().UTC()
	if received.After(s.now().Add(s.cfg.MaxFuture)) {
		return "", time.Time{}, meta, rejFuture
	}
	if m.GetGatewayId() != "" {
		gw := m.GetGatewayId()
		rssi := int16(clamp(int64(m.GetRssiDbm()), -32768, 32767)) //nolint:gosec // clamped
		snr := m.GetSnrDb()
		meta.GatewayID, meta.RSSIDBm, meta.SNRDb = &gw, &rssi, &snr
	}
	if sf := m.GetSpreadingFactor(); sf != 0 {
		v := int16(clamp(int64(sf), 0, 12)) //nolint:gosec // clamped
		meta.SF = &v
	}
	if id, err := uuid.Parse(m.GetDeduplicationId()); err == nil {
		meta.DedupID = &id
	}
	return devEUI, received, meta, ""
}

func clamp(v, lo, hi int64) int64 { return max(lo, min(hi, v)) }

func i32(v uint32) int32 { return int32(min(uint32(1<<31-1), v)) } //nolint:gosec // clamped
func i16(v uint32) int16 { return int16(min(uint32(1<<15-1), v)) } //nolint:gosec // clamped

func (s *Server) readingRow(r *telemetryv1.Reading) (store.Reading, reject) {
	dev, received, meta, why := s.metaOf(r.GetMeta())
	if why != "" {
		return store.Reading{}, why
	}
	ts := received
	if r.GetTs() != nil {
		ts = r.GetTs().AsTime().UTC()
		if ts.After(s.now().Add(s.cfg.MaxFuture)) {
			return store.Reading{}, rejFuture
		}
	}
	f := r.GetFlags()
	return store.Reading{
		DeviceID: dev, TS: ts, FCnt: int64(r.GetMeta().GetFCnt()),
		LevelMM: i32(r.GetLevelMm()), TempC: float32(r.GetTempC()), RHPct: i16(r.GetRhPct()),
		BattMV: i32(r.GetBattMv()), CyclesSinceLast: i16(r.GetCyclesSinceLast()),
		MainsOK: f.GetMainsOk(), FloatHigh: f.GetFloatHigh(), BackupRan: f.GetBackupRan(), SensorFault: f.GetSensorFault(),
		Meta: meta,
	}, ""
}

func (s *Server) cycleRow(c *telemetryv1.CycleEvent) (store.CycleEvent, reject) {
	dev, received, meta, why := s.metaOf(c.GetMeta())
	if why != "" {
		return store.CycleEvent{}, why
	}
	if c.GetStartedAt() == nil {
		return store.CycleEvent{}, rejNoTime
	}
	var pump string
	switch c.GetPumpId() {
	case telemetryv1.PumpId_PUMP_ID_PRIMARY:
		pump = "primary"
	case telemetryv1.PumpId_PUMP_ID_BACKUP:
		pump = "backup"
	default:
		return store.CycleEvent{}, rejBadPump
	}
	if c.GetPeakCurrentA() < 0 {
		return store.CycleEvent{}, rejBadValue
	}
	return store.CycleEvent{
		DeviceID: dev, StartedAt: c.GetStartedAt().AsTime().UTC(), FCnt: int64(c.GetMeta().GetFCnt()),
		ReceivedAt: received, RunS: i32(c.GetRunS()), PeakCurrentA: float32(c.GetPeakCurrentA()),
		LevelStartMM: i32(c.GetLevelStartMm()), LevelEndMM: i32(c.GetLevelEndMm()), PumpID: pump,
		Meta: meta,
	}, ""
}

func (s *Server) summaryRow(x *telemetryv1.StormSummary) (store.StormSummary, reject) {
	dev, received, meta, why := s.metaOf(x.GetMeta())
	if why != "" {
		return store.StormSummary{}, why
	}
	end := received
	if x.GetWindowEnd() != nil {
		end = x.GetWindowEnd().AsTime().UTC()
	}
	return store.StormSummary{
		DeviceID: dev, WindowEnd: end, FCnt: int64(x.GetMeta().GetFCnt()),
		WindowS: i32(x.GetWindowS()), CycleCount: i32(x.GetCount()), TotalRunS: i32(x.GetTotalRunS()),
		MaxPeakCurrentA: float32(x.GetMaxPeakCurrentA()), MinLevelMM: i32(x.GetMinLevelMm()),
		Meta: meta,
	}, ""
}

func (s *Server) alarmRow(a *telemetryv1.Alarm) (store.AlarmEvent, reject) {
	dev, received, meta, why := s.metaOf(a.GetMeta())
	if why != "" {
		return store.AlarmEvent{}, why
	}
	code := int64(a.GetCode())
	if code < 1 || code > 5 {
		return store.AlarmEvent{}, rejBadCode
	}
	at := received
	if a.GetRaisedAt() != nil {
		at = a.GetRaisedAt().AsTime().UTC()
	}
	return store.AlarmEvent{
		DeviceID: dev, RaisedAt: at, FCnt: int64(a.GetMeta().GetFCnt()),
		Code: int16(code), Value: i32(a.GetValue()), Meta: meta,
	}, ""
}
