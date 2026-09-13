// Package weather is the weather service: it turns stored rain-gauge uplinks
// (fPort 5) into rainfall rows and polls ECCC's hourly climate observations
// as the fallback and cross-check (prompt_plan.md §11).
package weather

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/smhunt/sumpnet/internal/store"
	"github.com/smhunt/sumpnet/internal/store/sqlcgen"
	"github.com/smhunt/sumpnet/internal/watermark"
)

// Rainfall sources (rainfall.source).
const (
	SourceGauge = "gauge"
	SourceECCC  = "eccc"
)

const (
	// TipWindow is the gauge firmware's wake period (§5: transmit every 5 min
	// while tips are counted, every 15 min otherwise). A node that reports
	// tips after a longer interval would have transmitted at an earlier wake
	// had they fallen before it, so they fell in the interval's last TipWindow.
	TipWindow = 5 * time.Minute
	// lossSlack tolerates clock jitter between the node's interval_s and the
	// gap between stored uplinks; a larger mismatch means an uplink was lost.
	lossSlack = 30 * time.Second
)

// GaugeUplink is the part of a stored fPort 5 uplink that rainfall depends on.
type GaugeUplink struct {
	At           time.Time // event time: end of the reported interval
	FCnt         int64
	TipCount     int64
	MMPerTip     float64
	IntervalS    int32
	CounterReset bool
	SensorFault  bool
}

// RainRow is one derived rainfall row: MM over [Start, Start+IntervalS).
type RainRow struct {
	Start     time.Time
	IntervalS int32
	MM        float64
}

// DeriveGaugeRainfall turns consecutive uplinks of one gauge (sorted by
// time) into rainfall rows. prev is the stored uplink just before ups[0], or
// nil if there is none; it only provides the counter baseline. Rules:
//
//   - The depth of an uplink is (tip_count − previous tip_count) × mm_per_tip,
//     over [previous uplink, this uplink): a lost uplink loses no rain, it
//     only widens the interval.
//   - After counter_reset, or a counter that went backwards, the depth is the
//     new tip_count over the node's own interval_s (since boot), never
//     starting before the previous stored uplink.
//   - A gauge's first uplink ever is a baseline unless it carries
//     counter_reset (the origin of its counter is unknown).
//   - Tips reported after an interval longer than TipWindow, with no uplink
//     lost, are placed in the last TipWindow and the rest of the interval
//     is a dry row.
//   - An uplink with sensor_fault yields no rows (its counter still serves
//     as the next baseline).
func DeriveGaugeRainfall(prev *GaugeUplink, ups []GaugeUplink) []RainRow {
	var out []RainRow
	p := prev
	for i := range ups {
		out = append(out, deriveOne(p, ups[i])...)
		p = &ups[i]
	}
	return out
}

func deriveOne(p *GaugeUplink, u GaugeUplink) []RainRow {
	nodeStart := u.At.Add(-time.Duration(u.IntervalS) * time.Second)
	var tips int64
	var start time.Time
	lost := false
	switch {
	case p == nil && !u.CounterReset:
		return nil
	case p == nil:
		tips, start = u.TipCount, nodeStart
	case u.CounterReset || u.TipCount < p.TipCount:
		tips, start = u.TipCount, nodeStart
		if start.Before(p.At) {
			start = p.At
		}
	default:
		tips, start = u.TipCount-p.TipCount, p.At
		lost = nodeStart.Sub(p.At) > lossSlack
	}
	total := u.At.Sub(start)
	if total < time.Second || u.SensorFault {
		return nil
	}
	mm := float64(tips) * u.MMPerTip
	if tips > 0 && !lost && total > TipWindow {
		wet := u.At.Add(-TipWindow)
		return []RainRow{
			{Start: start, IntervalS: seconds(wet.Sub(start)), MM: 0},
			{Start: wet, IntervalS: seconds(TipWindow), MM: mm},
		}
	}
	return []RainRow{{Start: start, IntervalS: seconds(total), MM: mm}}
}

func seconds(d time.Duration) int32 { return int32(min(d/time.Second, 1<<31-1)) } //nolint:gosec // clamped

func toGaugeUplink(r sqlcgen.RainGaugeUplink) GaugeUplink {
	return GaugeUplink{At: r.Ts, FCnt: r.FCnt, TipCount: r.TipCount, MMPerTip: r.MmPerTip, IntervalS: r.IntervalS, CounterReset: r.CounterReset, SensorFault: r.SensorFault}
}

// GaugeHandler is the watermark handler for rain_gauge_uplinks: for every
// gauge in a poll it re-derives the rainfall of the affected stretch — the
// new uplinks plus the stored one after them, whose delta a late arrival
// changes — and reconciles the gauge's rainfall rows in it.
type GaugeHandler struct {
	m   *Metrics
	log *slog.Logger
}

// NewGaugeHandler builds a GaugeHandler.
func NewGaugeHandler(m *Metrics, log *slog.Logger) *GaugeHandler {
	return &GaugeHandler{m: m, log: log}
}

// Reset implements watermark.Handler (no state).
func (h *GaugeHandler) Reset() {}

// Handle implements watermark.Handler.
func (h *GaugeHandler) Handle(ctx context.Context, tx pgx.Tx, q *sqlcgen.Queries, rows []sqlcgen.RainGaugeUplink) error {
	type span struct{ from, to time.Time }
	spans := map[string]*span{}
	for _, r := range rows {
		s := spans[r.DeviceID]
		if s == nil {
			spans[r.DeviceID] = &span{r.Ts, r.Ts}
			continue
		}
		if r.Ts.Before(s.from) {
			s.from = r.Ts
		}
		if r.Ts.After(s.to) {
			s.to = r.Ts
		}
	}
	devs := make([]string, 0, len(spans))
	for d := range spans {
		devs = append(devs, d)
	}
	sort.Strings(devs)

	for _, r := range rows {
		if r.CounterReset {
			h.m.GaugeResets.Inc()
		}
		if r.SensorFault {
			h.m.GaugeFaults.Inc()
		}
	}

	changed := 0
	var minTS, maxTS time.Time
	for _, dev := range devs {
		s := spans[dev]
		n, from, to, err := h.reconcile(ctx, q, dev, s.from, s.to)
		if err != nil {
			return err
		}
		if n > 0 {
			changed += n
			if minTS.IsZero() || from.Before(minTS) {
				minTS = from
			}
			if to.After(maxTS) {
				maxTS = to
			}
		}
	}
	if changed > 0 {
		return store.Notify(ctx, tx, store.Notification{Table: "rainfall", N: changed, MinTS: minTS, MaxTS: maxTS})
	}
	return nil
}

func (h *GaugeHandler) reconcile(ctx context.Context, q *sqlcgen.Queries, dev string, from, to time.Time) (int, time.Time, time.Time, error) {
	var prev *GaugeUplink
	pr, err := q.RainGaugeUplinkBefore(ctx, sqlcgen.RainGaugeUplinkBeforeParams{DeviceID: dev, Ts: from})
	switch {
	case err == nil:
		g := toGaugeUplink(pr)
		prev = &g
	case !errors.Is(err, pgx.ErrNoRows):
		return 0, from, to, fmt.Errorf("gauge %s: previous uplink: %w", dev, err)
	}
	stored, err := q.ListRainGaugeUplinks(ctx, sqlcgen.ListRainGaugeUplinksParams{DeviceID: dev, FromTs: from, ToTs: to})
	if err != nil {
		return 0, from, to, fmt.Errorf("gauge %s: uplinks: %w", dev, err)
	}
	next, err := q.RainGaugeUplinkAfter(ctx, sqlcgen.RainGaugeUplinkAfterParams{DeviceID: dev, Ts: to})
	switch {
	case err == nil:
		stored = append(stored, next)
	case !errors.Is(err, pgx.ErrNoRows):
		return 0, from, to, fmt.Errorf("gauge %s: next uplink: %w", dev, err)
	}
	ups := make([]GaugeUplink, len(stored))
	for i, r := range stored {
		ups[i] = toGaugeUplink(r)
	}
	derived := DeriveGaugeRainfall(prev, ups)

	// The stretch this derivation owns: from the baseline (or the first row)
	// to the newest uplink considered.
	lo, hi := ups[0].At, ups[len(ups)-1].At
	if prev != nil {
		lo = prev.At
	}
	keep := make([]time.Time, len(derived))
	for i, r := range derived {
		keep[i] = r.Start
		if r.Start.Before(lo) {
			lo = r.Start
		}
	}
	deleted, err := q.DeleteRainfallExcept(ctx, sqlcgen.DeleteRainfallExceptParams{Source: SourceGauge, StationID: dev, FromTs: lo, ToTs: hi, Keep: keep})
	if err != nil {
		return 0, lo, hi, fmt.Errorf("gauge %s: delete stale rainfall: %w", dev, err)
	}
	h.m.RainfallRows.WithLabelValues(SourceGauge, "deleted").Add(float64(deleted))
	written := 0
	for _, r := range derived {
		n, err := q.UpsertRainfall(ctx, sqlcgen.UpsertRainfallParams{Source: SourceGauge, StationID: dev, Ts: r.Start, IntervalS: r.IntervalS, Mm: r.MM})
		if err != nil {
			return 0, lo, hi, fmt.Errorf("gauge %s: upsert rainfall at %s: %w", dev, r.Start.Format(time.RFC3339), err)
		}
		written += int(n)
	}
	h.m.RainfallRows.WithLabelValues(SourceGauge, "written").Add(float64(written))
	h.m.RainfallRows.WithLabelValues(SourceGauge, "unchanged").Add(float64(len(derived) - written))
	// Deletions alone do not bump updated_at, so they wake storm-analytics
	// only through the notification; they always accompany rewritten rows
	// in practice (a late uplink splits an interval the next one re-covers).
	return written + int(deleted), lo, hi, nil
}

// GaugeSource is the watermark source for rain_gauge_uplinks.
var GaugeSource = watermark.Source[sqlcgen.RainGaugeUplink]{
	Table: "rain_gauge_uplinks",
	Poll: func(ctx context.Context, q *sqlcgen.Queries, after time.Time, lag float64, maxGroups int32) ([]sqlcgen.RainGaugeUplink, error) {
		return q.PollRainGaugeUplinks(ctx, sqlcgen.PollRainGaugeUplinksParams{After: after, LagSeconds: lag, MaxGroups: maxGroups})
	},
	InsertedAt: func(r sqlcgen.RainGaugeUplink) time.Time { return r.InsertedAt },
}
