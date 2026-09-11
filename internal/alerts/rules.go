package alerts

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	alertsv1 "github.com/smhunt/sumpnet/gen/go/sumpnet/alerts/v1"
	"github.com/smhunt/sumpnet/internal/detector"
	"github.com/smhunt/sumpnet/internal/hydrology"
	"github.com/smhunt/sumpnet/internal/store/sqlcgen"
	"github.com/smhunt/sumpnet/internal/watermark"
)

// --- node alarms (alarm_events) ---------------------------------------------

type alarmHandler struct{ e *Engine }

var alarmMessages = map[alertsv1.AlertCode]string{
	alertsv1.AlertCode_ALERT_CODE_FLOAT_HIGH:     "Node float switch reports high water (level %d mm)",
	alertsv1.AlertCode_ALERT_CODE_MAINS_LOST:     "Node reports mains power lost (battery %d mV)",
	alertsv1.AlertCode_ALERT_CODE_DRY_RUN:        "Node reports the pump ran %d s without lowering the level",
	alertsv1.AlertCode_ALERT_CODE_CONTINUOUS_RUN: "Node reports the pump running continuously (%d s so far)",
	alertsv1.AlertCode_ALERT_CODE_SENSOR_FAULT:   "Node reports a sensor fault (code %d)",
}

// Handle raises one alert per node alarm.
func (h *alarmHandler) Handle(ctx context.Context, _ pgx.Tx, q *sqlcgen.Queries, rows []sqlcgen.AlarmEvent) error {
	for _, a := range rows {
		code := alertsv1.AlertCode(a.Code)
		tmpl, ok := alarmMessages[code]
		if !ok {
			continue
		}
		msg := fmt.Sprintf(tmpl, a.Value)
		if _, err := h.e.Raise(ctx, q, a.DeviceID, code, a.RaisedAt, "node", triggerKey("alarm_events", a.DeviceID, a.RaisedAt, a.FCnt), msg); err != nil {
			return err
		}
	}
	h.e.refreshOpenGauge(ctx, q)
	return nil
}

// Reset implements watermark.Handler (no state).
func (h *alarmHandler) Reset() {}

// --- heartbeats (readings) -----------------------------------------------------

type levelSample struct {
	ts   time.Time
	dist int32
}

type readingHandler struct {
	e      *Engine
	levels map[string][]levelSample // last three samples per device, oldest first
}

func newReadingHandler(e *Engine) *readingHandler {
	return &readingHandler{e: e, levels: map[string][]levelSample{}}
}

// Reset drops the per-device level samples; they are re-seeded from the DB.
func (h *readingHandler) Reset() { h.levels = map[string][]levelSample{} }

// Handle applies the heartbeat rules (float, mains, sensor, battery, outage risk).
func (h *readingHandler) Handle(ctx context.Context, _ pgx.Tx, q *sqlcgen.Queries, rows []sqlcgen.Reading) error {
	e := h.e
	for _, r := range rows {
		dev := r.DeviceID
		key := triggerKey("readings", dev, r.Ts, r.FCnt)

		// A reporting device is not offline.
		if _, err := e.Resolve(ctx, q, dev, alertsv1.AlertCode_ALERT_CODE_OFFLINE, r.Ts, "device reporting"); err != nil {
			return err
		}

		switch {
		case r.FloatHigh:
			if _, err := e.Raise(ctx, q, dev, alertsv1.AlertCode_ALERT_CODE_FLOAT_HIGH, r.Ts, "heartbeat", key, fmt.Sprintf("Heartbeat reports the high-water float active (level %d mm)", r.LevelMm)); err != nil {
				return err
			}
		default:
			if _, err := e.Resolve(ctx, q, dev, alertsv1.AlertCode_ALERT_CODE_FLOAT_HIGH, r.Ts, "float low"); err != nil {
				return err
			}
		}

		switch {
		case r.SensorFault:
			if _, err := e.Raise(ctx, q, dev, alertsv1.AlertCode_ALERT_CODE_SENSOR_FAULT, r.Ts, "heartbeat", key, "Heartbeat reports a sensor fault"); err != nil {
				return err
			}
		default:
			if _, err := e.Resolve(ctx, q, dev, alertsv1.AlertCode_ALERT_CODE_SENSOR_FAULT, r.Ts, "sensor ok"); err != nil {
				return err
			}
		}

		if r.BattMv.Valid {
			switch {
			case r.BattMv.Int32 < e.cfg.LowBatteryMV:
				if _, err := e.Raise(ctx, q, dev, alertsv1.AlertCode_ALERT_CODE_LOW_BATTERY, r.Ts, "heartbeat", key, fmt.Sprintf("Battery at %d mV", r.BattMv.Int32)); err != nil {
					return err
				}
			case r.BattMv.Int32 >= e.cfg.LowBatteryClearMV:
				if _, err := e.Resolve(ctx, q, dev, alertsv1.AlertCode_ALERT_CODE_LOW_BATTERY, r.Ts, "battery recovered"); err != nil {
					return err
				}
			}
		}

		if r.MainsOk {
			if _, err := e.Resolve(ctx, q, dev, alertsv1.AlertCode_ALERT_CODE_MAINS_LOST, r.Ts, "mains_ok"); err != nil {
				return err
			}
			if _, err := e.Resolve(ctx, q, dev, alertsv1.AlertCode_ALERT_CODE_OUTAGE_RISK, r.Ts, "mains_ok"); err != nil {
				return err
			}
			delete(h.levels, dev)
			continue
		}

		// Mains lost: raise, then watch the level for outage risk.
		if _, err := e.Raise(ctx, q, dev, alertsv1.AlertCode_ALERT_CODE_MAINS_LOST, r.Ts, "heartbeat", key, "Heartbeat reports mains power lost"); err != nil {
			return err
		}
		samples, err := h.samples(ctx, q, dev, r)
		if err != nil {
			return err
		}
		if len(samples) >= 3 && samples[len(samples)-1].ts.Sub(samples[0].ts) <= e.cfg.RiseWindow {
			dists := make([]int32, len(samples))
			for i, s := range samples {
				dists[i] = s.dist
			}
			switch {
			case hydrology.LevelRising(dists, e.cfg.RiseMinMM):
				msg := fmt.Sprintf("Mains lost and the pit level rose %d mm over the last %d readings (rainfall check arrives in Phase 4)", dists[0]-dists[len(dists)-1], len(dists))
				if _, err := e.Raise(ctx, q, dev, alertsv1.AlertCode_ALERT_CODE_OUTAGE_RISK, r.Ts, "heartbeat", key, msg); err != nil {
					return err
				}
			case dists[len(dists)-1] > dists[len(dists)-2] && dists[len(dists)-2] > dists[len(dists)-3]:
				if _, err := e.Resolve(ctx, q, dev, alertsv1.AlertCode_ALERT_CODE_OUTAGE_RISK, r.Ts, "level falling"); err != nil {
					return err
				}
			}
		}
	}
	e.refreshOpenGauge(ctx, q)
	return nil
}

// samples appends the reading to the device's recent level samples, seeding
// from the database on first sight, and returns the last three.
func (h *readingHandler) samples(ctx context.Context, q *sqlcgen.Queries, dev string, r sqlcgen.Reading) ([]levelSample, error) {
	s, ok := h.levels[dev]
	if !ok {
		prev, err := q.ListReadingsBefore(ctx, sqlcgen.ListReadingsBeforeParams{DeviceID: dev, Ts: r.Ts, Limit: 2})
		if err != nil {
			return nil, fmt.Errorf("seed levels %s: %w", dev, err)
		}
		for i := len(prev) - 1; i >= 0; i-- { // newest-first → oldest-first
			if !prev[i].MainsOk {
				s = append(s, levelSample{prev[i].Ts, prev[i].LevelMm})
			}
		}
	}
	s = append(s, levelSample{r.Ts, r.LevelMm})
	if len(s) > 3 {
		s = s[len(s)-3:]
	}
	h.levels[dev] = s
	return s, nil
}

// --- detector output (detections) -----------------------------------------------

type detectionHandler struct{ e *Engine }

var detectionMessages = map[alertsv1.AlertCode]string{
	alertsv1.AlertCode_ALERT_CODE_DRY_RUN:        "Pump ran without lowering the level (failed pump or stuck check valve)",
	alertsv1.AlertCode_ALERT_CODE_SHORT_CYCLING:  "Pump is short-cycling: five or more runs less than 60 s apart (check valve or float)",
	alertsv1.AlertCode_ALERT_CODE_CONTINUOUS_RUN: "Pump ran continuously for more than 10 minutes",
}

// Handle raises or resolves alerts from detector output.
func (h *detectionHandler) Handle(ctx context.Context, _ pgx.Tx, q *sqlcgen.Queries, rows []sqlcgen.Detection) error {
	for _, d := range rows {
		code := alertsv1.AlertCode(d.Code)
		switch d.Action {
		case detector.ActionRaise:
			msg, ok := detectionMessages[code]
			if !ok {
				msg = CodeName(code) + " detected"
			}
			if _, err := h.e.Raise(ctx, q, d.DeviceID, code, d.ObservedAt, "detector", triggerKey("detections", d.DeviceID, d.ObservedAt, d.FCnt), msg); err != nil {
				return err
			}
		case detector.ActionClear:
			if _, err := h.e.Resolve(ctx, q, d.DeviceID, code, d.ObservedAt, "detector clear"); err != nil {
				return err
			}
		}
	}
	h.e.refreshOpenGauge(ctx, q)
	return nil
}

// Reset implements watermark.Handler (no state).
func (h *detectionHandler) Reset() {}

// --- sources -----------------------------------------------------------------------

var (
	alarmSource = watermark.Source[sqlcgen.AlarmEvent]{
		Table: "alarm_events",
		Poll: func(ctx context.Context, q *sqlcgen.Queries, after time.Time, lag float64, maxGroups int32) ([]sqlcgen.AlarmEvent, error) {
			return q.PollAlarmEvents(ctx, sqlcgen.PollAlarmEventsParams{After: after, LagSeconds: lag, MaxGroups: maxGroups})
		},
		InsertedAt: func(a sqlcgen.AlarmEvent) time.Time { return a.InsertedAt },
	}
	readingSource = watermark.Source[sqlcgen.Reading]{
		Table: "readings",
		Poll: func(ctx context.Context, q *sqlcgen.Queries, after time.Time, lag float64, maxGroups int32) ([]sqlcgen.Reading, error) {
			return q.PollReadings(ctx, sqlcgen.PollReadingsParams{After: after, LagSeconds: lag, MaxGroups: maxGroups})
		},
		InsertedAt: func(r sqlcgen.Reading) time.Time { return r.InsertedAt },
	}
	detectionSource = watermark.Source[sqlcgen.Detection]{
		Table: "detections",
		Poll: func(ctx context.Context, q *sqlcgen.Queries, after time.Time, lag float64, maxGroups int32) ([]sqlcgen.Detection, error) {
			return q.PollDetections(ctx, sqlcgen.PollDetectionsParams{After: after, LagSeconds: lag, MaxGroups: maxGroups})
		},
		InsertedAt: func(d sqlcgen.Detection) time.Time { return d.InsertedAt },
	}
)

// AddStages registers the three rule stages on a consumer, in order.
func AddStages(c *watermark.Consumer, e *Engine) {
	watermark.Add(c, alarmSource, &alarmHandler{e: e})
	watermark.Add(c, readingSource, newReadingHandler(e))
	watermark.Add(c, detectionSource, &detectionHandler{e: e})
}

// SweepOffline raises OFFLINE for devices silent longer than OfflineAfter
// (wall clock). Disabled when OfflineAfter is 0.
func (e *Engine) SweepOffline(ctx context.Context, q *sqlcgen.Queries) error {
	if e.cfg.OfflineAfter <= 0 {
		return nil
	}
	stale, err := q.ListStaleDevices(ctx, e.cfg.OfflineAfter.Seconds())
	if err != nil {
		return fmt.Errorf("stale devices: %w", err)
	}
	now := e.now().UTC()
	for _, d := range stale {
		msg := fmt.Sprintf("No uplink since %s", d.LastSeenAt.Time.UTC().Format(time.RFC3339))
		if _, err := e.Raise(ctx, q, d.DevEui, alertsv1.AlertCode_ALERT_CODE_OFFLINE, now, "sweep", "sweep:"+d.DevEui+":"+now.Format(time.RFC3339), msg); err != nil {
			return err
		}
	}
	return nil
}
