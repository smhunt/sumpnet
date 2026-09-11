// Package detector is the cycle-detector service: it annotates stored pump
// cycles with their estimated volume (§9) and turns the §10 conditions
// dry-run, short-cycling and continuous-run into rows of the detections
// table for the alerts service. It consumes cycle_events via the ADR 0003
// watermark poller, so every batch of annotations and detections commits
// atomically with the watermark.
package detector

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sort"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/prometheus/client_golang/prometheus"

	alertsv1 "github.com/smhunt/sumpnet/gen/go/sumpnet/alerts/v1"
	"github.com/smhunt/sumpnet/internal/hydrology"
	"github.com/smhunt/sumpnet/internal/platform"
	"github.com/smhunt/sumpnet/internal/store"
	"github.com/smhunt/sumpnet/internal/store/sqlcgen"
	"github.com/smhunt/sumpnet/internal/watermark"
)

// Detection actions (detections.action).
const (
	ActionRaise int16 = 1
	ActionClear int16 = 2
)

// Config tunes the detector.
type Config struct {
	Watermark            watermark.Config
	HealthyCyclesToClear int // DETECTOR_HEALTHY_CYCLES: consecutive healthy cycles before a condition clears
	Window               int // cycles remembered per (device, pump)
	PitAreaTTL           time.Duration
}

// ConfigFromEnv reads the detector's variables.
func ConfigFromEnv() (Config, error) {
	wm, err := watermark.ConfigFromEnv("cycle-detector")
	if err != nil {
		return Config{}, err
	}
	c := Config{Watermark: wm, HealthyCyclesToClear: 3, Window: 10, PitAreaTTL: 5 * time.Minute}
	if v, ok := os.LookupEnv("DETECTOR_HEALTHY_CYCLES"); ok {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			return c, fmt.Errorf("DETECTOR_HEALTHY_CYCLES: %q", v)
		}
		c.HealthyCyclesToClear = n
	}
	return c, nil
}

// Metrics are the detector's collectors.
type Metrics struct {
	Cycles        *prometheus.CounterVec // kind=cycle|dry_run|sub_threshold
	Detections    *prometheus.CounterVec // code, action
	VolumeMissing prometheus.Counter
}

// NewMetrics registers the collectors on reg.
func NewMetrics(reg prometheus.Registerer) *Metrics {
	m := &Metrics{
		Cycles: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "sumpnet_detector_cycles_total", Help: "Cycles classified per §10."}, []string{"kind"}),
		Detections: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "sumpnet_detector_detections_total", Help: "Detections written."}, []string{"code", "action"}),
		VolumeMissing: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "sumpnet_detector_volume_missing_total", Help: "Cycles left without a volume (device unlinked or no pit area)."}),
	}
	reg.MustRegister(m.Cycles, m.Detections, m.VolumeMissing)
	return m
}

type pitEntry struct {
	area  float64
	valid bool
	at    time.Time
}

type windowKey struct{ device, pump string }

type condKey struct {
	device string
	code   alertsv1.AlertCode
}

// Handler is the watermark handler for cycle_events.
type Handler struct {
	cfg     Config
	m       *Metrics
	log     *slog.Logger
	now     func() time.Time
	pitArea map[string]pitEntry
	windows map[windowKey][]hydrology.Cycle
	active  map[condKey]bool
	healthy map[condKey]int
}

// NewHandler builds a Handler.
func NewHandler(cfg Config, m *Metrics, log *slog.Logger) *Handler {
	h := &Handler{cfg: cfg, m: m, log: log, now: time.Now}
	h.Reset()
	h.pitArea = map[string]pitEntry{}
	return h
}

// Reset discards per-device state; it is rebuilt lazily from the database.
func (h *Handler) Reset() {
	h.windows = map[windowKey][]hydrology.Cycle{}
	h.active = map[condKey]bool{}
	h.healthy = map[condKey]int{}
}

type detection struct {
	device string
	code   alertsv1.AlertCode
	action int16
	at     time.Time
	fcnt   int64
	detail any
}

// Handle annotates and classifies one poll's cycles.
func (h *Handler) Handle(ctx context.Context, tx pgx.Tx, q *sqlcgen.Queries, rows []sqlcgen.CycleEvent) error {
	byDevice := map[string][]sqlcgen.CycleEvent{}
	var order []string
	for _, r := range rows {
		if _, ok := byDevice[r.DeviceID]; !ok {
			order = append(order, r.DeviceID)
		}
		byDevice[r.DeviceID] = append(byDevice[r.DeviceID], r)
	}

	var volumes []sqlcgen.SetCycleVolumeParams
	var dets []detection
	for _, dev := range order {
		cycles := byDevice[dev]
		sort.Slice(cycles, func(i, j int) bool { return cycles[i].StartedAt.Before(cycles[j].StartedAt) })
		area, ok, err := h.pit(ctx, q, dev)
		if err != nil {
			return err
		}
		for _, r := range cycles {
			c := toCycle(r)
			if ok {
				volumes = append(volumes, sqlcgen.SetCycleVolumeParams{
					EstVolumeL: pgtype.Float4{Float32: float32(hydrology.EstVolumeL(area, c)), Valid: true},
					DeviceID:   dev, StartedAt: r.StartedAt, FCnt: r.FCnt,
				})
			} else {
				h.m.VolumeMissing.Inc()
			}
			d, err := h.classify(ctx, q, dev, r, c)
			if err != nil {
				return err
			}
			dets = append(dets, d...)
		}
	}

	if len(volumes) > 0 {
		var batchErr error
		res := q.SetCycleVolume(ctx, volumes)
		res.Exec(func(i int, err error) {
			if err != nil && batchErr == nil {
				batchErr = fmt.Errorf("set volume %s/%d: %w", volumes[i].DeviceID, volumes[i].FCnt, err)
			}
		})
		if err := res.Close(); err != nil && batchErr == nil {
			batchErr = err
		}
		if batchErr != nil {
			return batchErr
		}
	}
	return h.writeDetections(ctx, tx, q, dets)
}

// writeDetections inserts detections idempotently and announces them.
func (h *Handler) writeDetections(ctx context.Context, tx pgx.Tx, q *sqlcgen.Queries, dets []detection) error {
	inserted := 0
	for _, d := range dets {
		detail, _ := json.Marshal(d.detail)
		n, err := q.InsertDetection(ctx, sqlcgen.InsertDetectionParams{
			DeviceID: d.device, Code: int16(d.code), Action: d.action, ObservedAt: d.at, FCnt: d.fcnt, Detail: detail, //nolint:gosec // enum ≤ 9
		})
		if err != nil {
			return fmt.Errorf("insert detection: %w", err)
		}
		if n > 0 {
			inserted++
			h.m.Detections.WithLabelValues(d.code.String(), strconv.Itoa(int(d.action))).Inc()
		}
	}
	if inserted > 0 {
		return store.Notify(ctx, tx, store.Notification{Table: "detections", N: inserted, MinTS: dets[0].at, MaxTS: dets[len(dets)-1].at})
	}
	return nil
}

func toCycle(r sqlcgen.CycleEvent) hydrology.Cycle {
	return hydrology.Cycle{StartedAt: r.StartedAt, RunS: r.RunS, LevelStartMM: r.LevelStartMm, LevelEndMM: r.LevelEndMm, Pump: string(r.PumpID)}
}

// pit returns the device's pit area, cached briefly; ok is false for
// unlinked devices or homes without a pit area.
func (h *Handler) pit(ctx context.Context, q *sqlcgen.Queries, dev string) (float64, bool, error) {
	if e, ok := h.pitArea[dev]; ok && h.now().Sub(e.at) < h.cfg.PitAreaTTL {
		return e.area, e.valid, nil
	}
	row, err := q.GetDevicePitArea(ctx, dev)
	if errors.Is(err, pgx.ErrNoRows) {
		h.pitArea[dev] = pitEntry{at: h.now()}
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("pit area %s: %w", dev, err)
	}
	valid := row.HomeID.Valid && row.PitAreaM2 > 0
	h.pitArea[dev] = pitEntry{area: row.PitAreaM2, valid: valid, at: h.now()}
	return row.PitAreaM2, valid, nil
}

// classify applies §10 to one cycle against the device's window and returns
// the detections it produces.
func (h *Handler) classify(ctx context.Context, q *sqlcgen.Queries, dev string, r sqlcgen.CycleEvent, c hydrology.Cycle) ([]detection, error) {
	wk := windowKey{dev, c.Pump}
	if _, ok := h.windows[wk]; !ok {
		if err := h.seed(ctx, q, dev, c.Pump, r.StartedAt); err != nil {
			return nil, err
		}
	}
	var out []detection
	switch {
	case hydrology.IsDryRun(c):
		h.m.Cycles.WithLabelValues("dry_run").Inc()
	case hydrology.IsCycle(c):
		h.m.Cycles.WithLabelValues("cycle").Inc()
	default:
		h.m.Cycles.WithLabelValues("sub_threshold").Inc()
	}

	// Dry run: raise on the first dry cycle, clear after N healthy ones.
	dry := condKey{dev, alertsv1.AlertCode_ALERT_CODE_DRY_RUN}
	if hydrology.IsDryRun(c) {
		h.healthy[dry] = 0
		if !h.active[dry] {
			h.active[dry] = true
			out = append(out, detection{dev, dry.code, ActionRaise, r.ReceivedAt, r.FCnt,
				map[string]any{"run_s": c.RunS, "drop_mm": c.DropMM(), "pump": c.Pump}})
		}
	} else if h.active[dry] {
		h.healthy[dry]++
		if h.healthy[dry] >= h.cfg.HealthyCyclesToClear {
			h.active[dry] = false
			out = append(out, detection{dev, dry.code, ActionClear, c.EndedAt(), r.FCnt,
				map[string]any{"healthy_cycles": h.healthy[dry]}})
		}
	}

	// Continuous run: the cycle row is the run's end, so raise and clear together.
	if hydrology.IsContinuousRun(c) {
		code := alertsv1.AlertCode_ALERT_CODE_CONTINUOUS_RUN
		out = append(out,
			detection{dev, code, ActionRaise, c.StartedAt.Add(hydrology.ContinuousRun), r.FCnt, map[string]any{"run_s": c.RunS, "pump": c.Pump}},
			detection{dev, code, ActionClear, c.EndedAt(), r.FCnt, map[string]any{"run_s": c.RunS, "pump": c.Pump}},
		)
	}

	// Short cycling over the per-pump window.
	w := append(h.windows[wk], c)
	if len(w) > h.cfg.Window {
		w = w[len(w)-h.cfg.Window:]
	}
	h.windows[wk] = w
	short := condKey{dev, alertsv1.AlertCode_ALERT_CODE_SHORT_CYCLING}
	run := hydrology.ShortCyclingRun(w)
	switch {
	case run >= hydrology.ShortCycleMinCount:
		h.healthy[short] = 0
		if !h.active[short] {
			h.active[short] = true
			out = append(out, detection{dev, short.code, ActionRaise, c.EndedAt(), r.FCnt, map[string]any{"streak": run, "pump": c.Pump}})
		}
	case run == 1 && h.active[short]:
		h.healthy[short]++
		if h.healthy[short] >= h.cfg.HealthyCyclesToClear {
			h.active[short] = false
			out = append(out, detection{dev, short.code, ActionClear, c.EndedAt(), r.FCnt, map[string]any{"healthy_cycles": h.healthy[short]}})
		}
	default:
		h.healthy[short] = 0
	}
	return out, nil
}

// seed loads the cycles before `before` for (device, pump) and derives the
// active conditions from them, so a restart does not re-raise or miss a clear.
func (h *Handler) seed(ctx context.Context, q *sqlcgen.Queries, dev, pump string, before time.Time) error {
	prev, err := q.ListCyclesBefore(ctx, sqlcgen.ListCyclesBeforeParams{DeviceID: dev, StartedAt: before, Limit: int32(h.cfg.Window)}) //nolint:gosec // small
	if err != nil {
		return fmt.Errorf("seed window %s: %w", dev, err)
	}
	var w []hydrology.Cycle
	for i := len(prev) - 1; i >= 0; i-- { // ListCyclesBefore is newest-first
		if string(prev[i].PumpID) == pump {
			w = append(w, toCycle(prev[i]))
		}
	}
	h.windows[windowKey{dev, pump}] = w
	if len(w) > 0 {
		last := w[len(w)-1]
		h.active[condKey{dev, alertsv1.AlertCode_ALERT_CODE_DRY_RUN}] = hydrology.IsDryRun(last)
		h.active[condKey{dev, alertsv1.AlertCode_ALERT_CODE_SHORT_CYCLING}] = hydrology.ShortCyclingRun(w) >= hydrology.ShortCycleMinCount
	}
	return nil
}

// summaryHandler is the storm_summaries stage; it shares the Handler's state.
type summaryHandler struct{ h *Handler }

// Handle applies the summary form of short cycling (hydrology.SummaryShortCycling).
func (s *summaryHandler) Handle(ctx context.Context, tx pgx.Tx, q *sqlcgen.Queries, rows []sqlcgen.StormSummary) error {
	h := s.h
	var dets []detection
	for _, r := range rows {
		key := condKey{r.DeviceID, alertsv1.AlertCode_ALERT_CODE_SHORT_CYCLING}
		short := hydrology.SummaryShortCycling(r.CycleCount, r.WindowS)
		detail := map[string]any{"cycle_count": r.CycleCount, "window_s": r.WindowS}
		switch {
		case short && !h.active[key]:
			h.active[key] = true
			h.healthy[key] = 0
			dets = append(dets, detection{r.DeviceID, key.code, ActionRaise, r.WindowEnd, r.FCnt, detail})
		case short:
			h.healthy[key] = 0
		case !short && h.active[key]:
			h.healthy[key]++
			if h.healthy[key] >= h.cfg.HealthyCyclesToClear {
				h.active[key] = false
				dets = append(dets, detection{r.DeviceID, key.code, ActionClear, r.WindowEnd, r.FCnt, detail})
			}
		}
		h.m.Cycles.WithLabelValues("summarised").Add(float64(r.CycleCount))
	}
	return h.writeDetections(ctx, tx, q, dets)
}

// Reset defers to the shared handler.
func (s *summaryHandler) Reset() { s.h.Reset() }

// SummarySource is the watermark source for storm_summaries.
var SummarySource = watermark.Source[sqlcgen.StormSummary]{
	Table: "storm_summaries",
	Poll: func(ctx context.Context, q *sqlcgen.Queries, after time.Time, lag float64, maxGroups int32) ([]sqlcgen.StormSummary, error) {
		return q.PollStormSummaries(ctx, sqlcgen.PollStormSummariesParams{After: after, LagSeconds: lag, MaxGroups: maxGroups})
	},
	InsertedAt: func(s sqlcgen.StormSummary) time.Time { return s.InsertedAt },
}

// Source is the watermark source for cycle_events.
var Source = watermark.Source[sqlcgen.CycleEvent]{
	Table: "cycle_events",
	Poll: func(ctx context.Context, q *sqlcgen.Queries, after time.Time, lag float64, maxGroups int32) ([]sqlcgen.CycleEvent, error) {
		return q.PollCycleEvents(ctx, sqlcgen.PollCycleEventsParams{After: after, LagSeconds: lag, MaxGroups: maxGroups})
	},
	InsertedAt: func(c sqlcgen.CycleEvent) time.Time { return c.InsertedAt },
}

// AddStages registers the cycle_events and storm_summaries stages on a consumer.
func AddStages(c *watermark.Consumer, h *Handler) {
	watermark.Add(c, Source, h)
	watermark.Add(c, SummarySource, &summaryHandler{h: h})
}

// Run is the service body used by cmd/cycle-detector: it connects, verifies
// the schema and runs the consumer until ctx ends.
func Run(ctx context.Context, app *platform.App, dsn string, cfg Config) error {
	st, err := store.New(ctx, dsn)
	if err != nil {
		return err
	}
	defer st.Close()
	if err := st.CheckSchema(ctx); err != nil {
		return fmt.Errorf("schema check (run migrations first): %w", err)
	}
	m := NewMetrics(app.Metrics)
	c := watermark.New(st.Pool(), dsn, cfg.Watermark, watermark.NewMetrics(app.Metrics), app.Log)
	AddStages(c, NewHandler(cfg, m, app.Log))
	c.OnRound(func(int) { app.Ready.Set(true) })
	app.Log.Info("cycle-detector running", "poll_interval", cfg.Watermark.PollInterval, "lag", cfg.Watermark.Lag)
	return c.Run(ctx)
}
