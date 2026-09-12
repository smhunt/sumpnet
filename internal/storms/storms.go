// Package storms is the storm-analytics service: it segments rainfall into
// §10 storm events and computes each linked home's response to every storm
// (home_storm_metrics). It is the single writer of both tables and consumes
// rainfall, cycle_events, storm_summaries and readings through ADR 0003
// watermark stages; every stage recomputes the affected storms and homes
// from the database, so replays and redeliveries converge to one answer.
package storms

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"math"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/smhunt/sumpnet/internal/hydrology"
	"github.com/smhunt/sumpnet/internal/store"
	"github.com/smhunt/sumpnet/internal/store/sqlcgen"
	"github.com/smhunt/sumpnet/internal/watermark"
)

const (
	// edgePad reaches past a changed stretch far enough to catch the rain of
	// any storm it could join (a gap shorter than StormMaxGap) plus an hour.
	edgePad       = hydrology.StormMaxGap + time.Hour
	extendStep    = 24 * time.Hour
	maxExtensions = 90
	// rainScanBefore bounds the index scan for intervals that start before a
	// window: no rainfall interval is longer than this.
	rainScanBefore = 7 * 24 * time.Hour
	// nextStormHorizon is how far ahead a following storm is looked up.
	nextStormHorizon = 60 * 24 * time.Hour
	statusOpen       = "open"
	statusClosed     = "closed"
)

// Analyzer maintains storm_events and home_storm_metrics.
type Analyzer struct {
	p   hydrology.ResponseParams
	m   *Metrics
	log *slog.Logger
}

// NewAnalyzer builds an Analyzer.
func NewAnalyzer(p hydrology.ResponseParams, m *Metrics, log *slog.Logger) *Analyzer {
	return &Analyzer{p: p, m: m, log: log}
}

type span struct {
	from, to time.Time
	set      bool
}

func (s *span) add(from, to time.Time) {
	if !s.set {
		s.from, s.to, s.set = from, to, true
		return
	}
	if from.Before(s.from) {
		s.from = from
	}
	if to.After(s.to) {
		s.to = to
	}
}

type stormInfo struct {
	id     uuid.UUID
	storm  hydrology.Storm
	source string
	until  time.Time // the next storm's onset; zero if none is known
}

type device struct {
	dev      string
	home     uuid.UUID
	pitArea  float64
	lastSeen time.Time
}

// process recomputes the storms touched by changed rainfall in rain and the
// storms whose analysis window covers the changed telemetry in devs, then the
// home metrics that can have changed: every home for a storm whose own row
// changed, otherwise only the homes whose data changed.
func (a *Analyzer) process(ctx context.Context, tx pgx.Tx, q *sqlcgen.Queries, rain span, devs map[string]span) error {
	began := time.Now()
	defer func() { a.m.ProcessSeconds.Observe(time.Since(began).Seconds()) }()

	var window span
	if rain.set {
		window.add(rain.from.Add(-edgePad), rain.to.Add(edgePad))
	}
	if len(devs) > 0 {
		var all span
		for _, s := range devs {
			all.add(s.from, s.to)
		}
		cands, err := q.ListStormEventsOverlapping(ctx, sqlcgen.ListStormEventsOverlappingParams{
			FromTs: all.from.Add(-a.p.MaxRecession - 24*time.Hour), ToTs: all.to.Add(a.p.BaseflowLookback + 24*time.Hour),
		})
		if err != nil {
			return fmt.Errorf("storms: candidates: %w", err)
		}
		for _, c := range cands {
			end := c.StartedAt
			if c.EndedAt.Valid {
				end = c.EndedAt.Time
			}
			window.add(c.StartedAt.Add(-edgePad), end.Add(edgePad))
		}
	}
	if !window.set {
		return nil
	}

	infos, changed, stormChanges, err := a.refreshStorms(ctx, q, window.from, window.to)
	if err != nil {
		return err
	}
	metricChanges := 0
	if len(infos) > 0 {
		devices, err := a.devices(ctx, q)
		if err != nil {
			return err
		}
		for _, si := range infos {
			targets := a.targets(si, changed[si.id], devices, devs)
			if len(targets) == 0 {
				continue
			}
			existing, err := q.ListHomeStormMetrics(ctx, si.id)
			if err != nil {
				return fmt.Errorf("storms: metrics of %s: %w", si.id, err)
			}
			byHome := make(map[uuid.UUID]sqlcgen.HomeStormMetric, len(existing))
			for _, e := range existing {
				byHome[e.HomeID] = e
			}
			rain, err := a.baseflowRain(ctx, q, si.storm)
			if err != nil {
				return err
			}
			for _, d := range targets {
				n, err := a.recomputeHome(ctx, q, si, d, rain, byHome)
				if err != nil {
					return err
				}
				metricChanges += n
			}
		}
	}
	if total := stormChanges + metricChanges; total > 0 {
		return store.Notify(ctx, tx, store.Notification{Table: "storm_events", N: total, MinTS: window.from.UTC(), MaxTS: window.to.UTC()})
	}
	return nil
}

// targets are the devices whose home metrics for si must be recomputed.
func (a *Analyzer) targets(si stormInfo, stormChanged bool, devices []device, devs map[string]span) []device {
	if stormChanged {
		return devices
	}
	dependsFrom := si.storm.FirstRain.Add(-a.p.BaseflowLookback)
	dependsTo := si.storm.RainEnd.Add(a.p.MaxRecession)
	var out []device
	for _, d := range devices {
		if s, ok := devs[d.dev]; ok && !s.to.Before(dependsFrom) && !s.from.After(dependsTo) {
			out = append(out, d)
		}
	}
	return out
}

// devices returns one linked house node per home (the first by DevEUI);
// storm metrics are per home and a second node in the same pit is ignored.
func (a *Analyzer) devices(ctx context.Context, q *sqlcgen.Queries) ([]device, error) {
	rows, err := q.ListLinkedHouseDevices(ctx)
	if err != nil {
		return nil, fmt.Errorf("storms: linked devices: %w", err)
	}
	var out []device
	seen := map[uuid.UUID]bool{}
	for _, r := range rows {
		if seen[r.HomeID] || !r.LastSeenAt.Valid {
			continue
		}
		seen[r.HomeID] = true
		out = append(out, device{dev: r.DevEui, home: r.HomeID, pitArea: r.PitAreaM2, lastSeen: r.LastSeenAt.Time})
	}
	return out, nil
}

// horizon is how far rainfall data reach, over both sources.
func (a *Analyzer) horizon(ctx context.Context, q *sqlcgen.Queries, from time.Time) (time.Time, error) {
	var h time.Time
	for _, src := range []string{"gauge", "eccc"} {
		t, err := q.RainfallCoveredUntil(ctx, sqlcgen.RainfallCoveredUntilParams{Source: src, ScanFrom: from.Add(-rainScanBefore)})
		if err != nil {
			return h, fmt.Errorf("storms: rainfall coverage: %w", err)
		}
		if t.After(h) {
			h = t
		}
	}
	return h, nil
}

// refreshStorms re-segments the rain around [lo, hi] and reconciles
// storm_events with the result. The window is widened a day at a time until
// no wet interval lies within edgePad of either end (or the right end reaches
// the data horizon), so every storm it holds is complete. Existing rows are
// matched to computed storms by their onset falling inside the storm's rain,
// which keeps a storm's id stable while its onset and end move with new
// data; a storm that absorbed another keeps the earlier row, and rows whose
// storm no longer exists are deleted (their metrics cascade).
func (a *Analyzer) refreshStorms(ctx context.Context, q *sqlcgen.Queries, lo, hi time.Time) ([]stormInfo, map[uuid.UUID]bool, int, error) {
	horizon, err := a.horizon(ctx, q, lo)
	if err != nil {
		return nil, nil, 0, err
	}
	var set rainSet
	for i := 0; ; i++ {
		rows, lerr := q.ListRainfall(ctx, sqlcgen.ListRainfallParams{ScanFrom: lo.Add(-rainScanBefore), FromTs: lo, ToTs: hi})
		if lerr != nil {
			return nil, nil, 0, fmt.Errorf("storms: rainfall: %w", lerr)
		}
		set = selectRain(rows)
		first, last, ok := set.wetExtent()
		if !ok || i >= maxExtensions {
			break
		}
		extended := false
		if first.Sub(lo) < edgePad {
			lo, extended = lo.Add(-extendStep), true
		}
		if hi.Sub(last) < edgePad && hi.Before(horizon) {
			hi, extended = hi.Add(extendStep), true
		}
		if !extended {
			break
		}
	}

	computed := hydrology.FindStorms(set.stations, horizon)
	existing, err := q.ListStormEventsOverlapping(ctx, sqlcgen.ListStormEventsOverlappingParams{FromTs: lo, ToTs: hi})
	if err != nil {
		return nil, nil, 0, fmt.Errorf("storms: existing: %w", err)
	}
	var owned []sqlcgen.StormEvent
	for _, e := range existing {
		if !e.StartedAt.Before(lo) {
			owned = append(owned, e)
		}
	}
	used := make([]bool, len(owned))
	changed := map[uuid.UUID]bool{}
	changes := 0
	var infos []stormInfo
	for ci, c := range computed {
		if !c.RainEnd.After(lo) || !c.FirstRain.Before(hi) {
			continue
		}
		want := stormRowFor(c, set.sourceFor(c))
		match := -1
		for j, e := range owned {
			if used[j] || e.StartedAt.Before(c.FirstRain) || e.StartedAt.After(c.RainEnd) {
				continue
			}
			used[j] = true
			if match < 0 {
				match = j
				continue
			}
			if derr := q.DeleteStormEvent(ctx, e.ID); derr != nil {
				return nil, nil, 0, fmt.Errorf("storms: delete merged %s: %w", e.ID, derr)
			}
			a.m.StormEvents.WithLabelValues("delete").Inc()
			changes++
		}
		var id uuid.UUID
		if match < 0 {
			id, err = q.InsertStormEvent(ctx, sqlcgen.InsertStormEventParams{
				StartedAt: want.startedAt, EndedAt: want.endedAt, TotalRainMm: want.total, PeakIntensityMmH: want.peak, RainSource: want.source, Status: want.status,
			})
			if err != nil {
				return nil, nil, 0, fmt.Errorf("storms: insert: %w", err)
			}
			a.m.StormEvents.WithLabelValues("insert").Inc()
			a.log.Info("storm opened", "id", id, "onset", want.startedAt, "total_mm", want.total, "source", want.source, "status", want.status)
			changed[id] = true
			changes++
		} else {
			e := owned[match]
			id = e.ID
			if !want.sameAs(e) {
				if uerr := q.UpdateStormEvent(ctx, sqlcgen.UpdateStormEventParams{
					ID: id, StartedAt: want.startedAt, EndedAt: want.endedAt, TotalRainMm: want.total, PeakIntensityMmH: want.peak, RainSource: want.source, Status: want.status,
				}); uerr != nil {
					return nil, nil, 0, fmt.Errorf("storms: update %s: %w", id, uerr)
				}
				a.m.StormEvents.WithLabelValues("update").Inc()
				if e.Status != want.status {
					a.log.Info("storm "+want.status, "id", id, "onset", want.startedAt, "ended_at", want.endedAt.Time, "total_mm", want.total)
				}
				changed[id] = true
				changes++
			}
		}
		info := stormInfo{id: id, storm: c, source: want.source}
		if ci+1 < len(computed) {
			info.until = computed[ci+1].Onset
		} else if info.until, err = a.nextStormOnset(ctx, q, c.RainEnd); err != nil {
			return nil, nil, 0, err
		}
		infos = append(infos, info)
	}
	for j, e := range owned {
		if used[j] {
			continue
		}
		if derr := q.DeleteStormEvent(ctx, e.ID); derr != nil {
			return nil, nil, 0, fmt.Errorf("storms: delete %s: %w", e.ID, derr)
		}
		a.m.StormEvents.WithLabelValues("delete").Inc()
		a.log.Info("storm removed", "id", e.ID, "onset", e.StartedAt)
		changes++
	}
	return infos, changed, changes, nil
}

func (a *Analyzer) nextStormOnset(ctx context.Context, q *sqlcgen.Queries, after time.Time) (time.Time, error) {
	rows, err := q.ListStormEventsOverlapping(ctx, sqlcgen.ListStormEventsOverlappingParams{FromTs: after, ToTs: after.Add(nextStormHorizon)})
	if err != nil {
		return time.Time{}, fmt.Errorf("storms: next storm: %w", err)
	}
	for _, r := range rows { // ordered by started_at
		if r.StartedAt.After(after) {
			return r.StartedAt, nil
		}
	}
	return time.Time{}, nil
}

type stormRow struct {
	startedAt   time.Time
	endedAt     sql.NullTime
	total, peak float64
	source      string
	status      string
}

func stormRowFor(s hydrology.Storm, source string) stormRow {
	r := stormRow{startedAt: dbTime(s.Onset), total: round(s.TotalMM, 2), peak: round(s.PeakMMH, 2), source: source, status: statusOpen}
	if s.Closed {
		r.endedAt = sql.NullTime{Time: dbTime(s.RainEnd), Valid: true}
		r.status = statusClosed
	}
	return r
}

func (r stormRow) sameAs(e sqlcgen.StormEvent) bool {
	return e.StartedAt.Equal(r.startedAt) && e.EndedAt.Valid == r.endedAt.Valid && (!r.endedAt.Valid || e.EndedAt.Time.Equal(r.endedAt.Time)) &&
		e.TotalRainMm == r.total && e.PeakIntensityMmH == r.peak && e.RainSource == r.source && e.Status == r.status
}

// baseflowRain is the rain the dry-weather test of the storm's baseflow looks at.
func (a *Analyzer) baseflowRain(ctx context.Context, q *sqlcgen.Queries, s hydrology.Storm) ([]hydrology.RainInterval, error) {
	from := s.FirstRain.Add(-a.p.BaseflowLookback - hydrology.BaseflowDryFor)
	rows, err := q.ListRainfall(ctx, sqlcgen.ListRainfallParams{ScanFrom: from.Add(-rainScanBefore), FromTs: from, ToTs: s.FirstRain})
	if err != nil {
		return nil, fmt.Errorf("storms: baseflow rainfall: %w", err)
	}
	return selectRain(rows).flatten(), nil
}

// recomputeHome writes one home's metrics for one storm if they changed and
// returns how many rows it wrote or deleted.
func (a *Analyzer) recomputeHome(ctx context.Context, q *sqlcgen.Queries, si stormInfo, d device, rain []hydrology.RainInterval, existing map[uuid.UUID]sqlcgen.HomeStormMetric) (int, error) {
	s := si.storm
	end := s.RainEnd.Add(a.p.MaxRecession)
	if !si.until.IsZero() && si.until.Before(end) {
		end = si.until
	}
	if limit := d.lastSeen.Add(time.Second); limit.Before(end) {
		end = limit
	}
	obs, err := a.loadObs(ctx, q, d.dev, s, end)
	if err != nil {
		return 0, err
	}
	h := hydrology.AnalyseHomeStorm(obs, d.pitArea, s, rain, si.until, d.lastSeen, a.p)
	old, had := existing[d.home]
	if !h.HasSamples {
		if !had {
			return 0, nil
		}
		if err := q.DeleteHomeStormMetrics(ctx, sqlcgen.DeleteHomeStormMetricsParams{StormID: si.id, HomeID: d.home}); err != nil {
			return 0, fmt.Errorf("storms: delete metrics %s/%s: %w", si.id, d.home, err)
		}
		a.m.HomeMetrics.WithLabelValues("deleted").Inc()
		return 1, nil
	}
	row := sqlcgen.UpsertHomeStormMetricsParams{
		StormID: si.id, HomeID: d.home,
		LagMin:       optFloat(h.Lag.Minutes(), h.HasLag, 1),
		RecessionMin: optFloat(h.Recession.Minutes(), h.HasRecession, 1),
		VolumeL:      round(h.VolumeL, 1),
		Cycles:       int32(min(h.Cycles, math.MaxInt32)), //nolint:gosec // clamped
		BaseflowCpd:  optFloat(h.Baseflow.CPD, h.HasBaseflow, 3),
	}
	if had && old.LagMin == row.LagMin && old.RecessionMin == row.RecessionMin && old.VolumeL == row.VolumeL &&
		old.Cycles == row.Cycles && old.BaseflowCpd == row.BaseflowCpd {
		a.m.HomeMetrics.WithLabelValues("unchanged").Inc()
		return 0, nil
	}
	if err := q.UpsertHomeStormMetrics(ctx, row); err != nil {
		return 0, fmt.Errorf("storms: upsert metrics %s/%s: %w", si.id, d.home, err)
	}
	a.m.HomeMetrics.WithLabelValues("written").Inc()
	return 1, nil
}

// loadObs reads what one node reported from the start of the baseflow
// lookback (cycles) or shortly before the storm (roll-ups, levels) to end.
func (a *Analyzer) loadObs(ctx context.Context, q *sqlcgen.Queries, dev string, s hydrology.Storm, end time.Time) (hydrology.HomeObs, error) {
	var o hydrology.HomeObs
	from := s.FirstRain.Add(-a.p.BaseflowLookback)
	near := minTime(s.FirstRain, s.Onset).Add(-a.p.LagWindow - time.Hour)
	cycles, err := q.ListCycleEventsForDevice(ctx, sqlcgen.ListCycleEventsForDeviceParams{DeviceID: dev, FromTs: from, ToTs: end})
	if err != nil {
		return o, fmt.Errorf("storms: cycles of %s: %w", dev, err)
	}
	for _, c := range cycles {
		o.Cycles = append(o.Cycles, hydrology.Cycle{StartedAt: c.StartedAt, RunS: c.RunS, LevelStartMM: c.LevelStartMm, LevelEndMM: c.LevelEndMm, Pump: string(c.PumpID)})
	}
	summaries, err := q.ListStormSummariesForDevice(ctx, sqlcgen.ListStormSummariesForDeviceParams{DeviceID: dev, FromTs: near, ToTs: end})
	if err != nil {
		return o, fmt.Errorf("storms: roll-ups of %s: %w", dev, err)
	}
	for _, sm := range summaries {
		o.Summaries = append(o.Summaries, hydrology.Summary{WindowEnd: sm.WindowEnd, WindowS: sm.WindowS, Count: sm.CycleCount})
	}
	levels, err := q.ListLevelsForDevice(ctx, sqlcgen.ListLevelsForDeviceParams{DeviceID: dev, FromTs: near, ToTs: end})
	if err != nil {
		return o, fmt.Errorf("storms: levels of %s: %w", dev, err)
	}
	for _, l := range levels {
		o.Levels = append(o.Levels, hydrology.LevelSample{At: l.Ts, DistanceMM: l.LevelMm})
	}
	o.Sort()
	return o, nil
}

func optFloat(v float64, ok bool, places int) pgtype.Float8 {
	if !ok {
		return pgtype.Float8{}
	}
	return pgtype.Float8{Float64: round(v, places), Valid: true}
}

func round(v float64, places int) float64 {
	p := math.Pow(10, float64(places))
	return math.Round(v*p) / p
}

// dbTime matches Postgres' microsecond timestamptz so comparisons are exact.
func dbTime(t time.Time) time.Time { return t.UTC().Truncate(time.Microsecond) }

func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

// --- stages ------------------------------------------------------------------

type rainfallHandler struct{ a *Analyzer }

// Handle implements watermark.Handler.
func (h *rainfallHandler) Handle(ctx context.Context, tx pgx.Tx, q *sqlcgen.Queries, rows []sqlcgen.Rainfall) error {
	var s span
	for _, r := range rows {
		s.add(r.Ts, r.Ts.Add(time.Duration(r.IntervalS)*time.Second))
	}
	return h.a.process(ctx, tx, q, s, nil)
}

// Reset implements watermark.Handler (no state).
func (h *rainfallHandler) Reset() {}

type telemetryHandler[T any] struct {
	a  *Analyzer
	at func(T) (string, time.Time)
}

// Handle implements watermark.Handler.
func (h *telemetryHandler[T]) Handle(ctx context.Context, tx pgx.Tx, q *sqlcgen.Queries, rows []T) error {
	devs := map[string]span{}
	for _, r := range rows {
		dev, t := h.at(r)
		s := devs[dev]
		s.add(t, t)
		devs[dev] = s
	}
	return h.a.process(ctx, tx, q, span{}, devs)
}

// Reset implements watermark.Handler (no state).
func (h *telemetryHandler[T]) Reset() {}

// AddStages registers the four stages, rainfall first.
func (a *Analyzer) AddStages(c *watermark.Consumer) {
	watermark.Add(c, watermark.Source[sqlcgen.Rainfall]{
		Table: "rainfall",
		Poll: func(ctx context.Context, q *sqlcgen.Queries, after time.Time, lag float64, maxGroups int32) ([]sqlcgen.Rainfall, error) {
			return q.PollRainfall(ctx, sqlcgen.PollRainfallParams{After: after, LagSeconds: lag, MaxGroups: maxGroups})
		},
		// Polled by updated_at: see migration 0005.
		InsertedAt: func(r sqlcgen.Rainfall) time.Time { return r.UpdatedAt },
	}, &rainfallHandler{a})
	watermark.Add(c, watermark.Source[sqlcgen.CycleEvent]{
		Table: "cycle_events",
		Poll: func(ctx context.Context, q *sqlcgen.Queries, after time.Time, lag float64, maxGroups int32) ([]sqlcgen.CycleEvent, error) {
			return q.PollCycleEvents(ctx, sqlcgen.PollCycleEventsParams{After: after, LagSeconds: lag, MaxGroups: maxGroups})
		},
		InsertedAt: func(r sqlcgen.CycleEvent) time.Time { return r.InsertedAt },
	}, &telemetryHandler[sqlcgen.CycleEvent]{a, func(r sqlcgen.CycleEvent) (string, time.Time) { return r.DeviceID, r.StartedAt }})
	watermark.Add(c, watermark.Source[sqlcgen.StormSummary]{
		Table: "storm_summaries",
		Poll: func(ctx context.Context, q *sqlcgen.Queries, after time.Time, lag float64, maxGroups int32) ([]sqlcgen.StormSummary, error) {
			return q.PollStormSummaries(ctx, sqlcgen.PollStormSummariesParams{After: after, LagSeconds: lag, MaxGroups: maxGroups})
		},
		InsertedAt: func(r sqlcgen.StormSummary) time.Time { return r.InsertedAt },
	}, &telemetryHandler[sqlcgen.StormSummary]{a, func(r sqlcgen.StormSummary) (string, time.Time) { return r.DeviceID, r.WindowEnd }})
	watermark.Add(c, watermark.Source[sqlcgen.Reading]{
		Table: "readings",
		Poll: func(ctx context.Context, q *sqlcgen.Queries, after time.Time, lag float64, maxGroups int32) ([]sqlcgen.Reading, error) {
			return q.PollReadings(ctx, sqlcgen.PollReadingsParams{After: after, LagSeconds: lag, MaxGroups: maxGroups})
		},
		InsertedAt: func(r sqlcgen.Reading) time.Time { return r.InsertedAt },
	}, &telemetryHandler[sqlcgen.Reading]{a, func(r sqlcgen.Reading) (string, time.Time) { return r.DeviceID, r.Ts }})
}
