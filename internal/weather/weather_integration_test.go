//go:build integration

package weather

import (
	"context"
	"log/slog"
	"math"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"

	"github.com/smhunt/sumpnet/internal/store"
	"github.com/smhunt/sumpnet/internal/store/sqlcgen"
	"github.com/smhunt/sumpnet/internal/testinfra"
	"github.com/smhunt/sumpnet/internal/watermark"
)

func setup(t *testing.T) (*store.Store, *watermark.Consumer, *Metrics) {
	t.Helper()
	ctx := context.Background()
	dsn := testinfra.StartPostgres(t)
	if err := store.Migrate(ctx, dsn); err != nil {
		t.Fatal(err)
	}
	st, err := store.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	reg := prometheus.NewRegistry()
	m := NewMetrics(reg)
	c := watermark.New(st.Pool(), dsn, watermark.Config{Consumer: "weather", Lag: 0, MaxGroups: 50, PollInterval: time.Hour}, watermark.NewMetrics(reg), slog.New(slog.DiscardHandler))
	watermark.Add(c, GaugeSource, NewGaugeHandler(m, slog.New(slog.DiscardHandler)))
	return st, c, m
}

type rainRow struct {
	ts        time.Time
	intervalS int32
	mm        float64
	updated   time.Time
}

func rainfall(t *testing.T, st *store.Store, source, station string) []rainRow {
	t.Helper()
	rows, err := st.Pool().Query(context.Background(), `SELECT ts, interval_s, mm::float8, updated_at FROM rainfall WHERE source = $1 AND station_id = $2 ORDER BY ts`, source, station)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []rainRow
	for rows.Next() {
		var r rainRow
		if err := rows.Scan(&r.ts, &r.intervalS, &r.mm, &r.updated); err != nil {
			t.Fatal(err)
		}
		out = append(out, r)
	}
	return out
}

func TestGaugeConsumerDerivesAndReconcilesRainfall(t *testing.T) {
	st, c, m := setup(t)
	ctx := context.Background()
	dev := "70b3d57ed1000000"
	up := func(at time.Duration, fcnt, tips int64, intervalS int32, reset bool) store.RainGaugeUplink {
		return store.RainGaugeUplink{DeviceID: dev, TS: base.Add(at), FCnt: fcnt, TipCount: tips, MMPerTip: 0.2, IntervalS: intervalS, BattMV: 3600, CounterReset: reset}
	}
	m5 := time.Minute * 5
	// Boot, a dry 15 minutes, then tips every 5 minutes; fcnt 4 (at 30 min) is late.
	first := []store.RainGaugeUplink{
		up(2*time.Minute, 0, 0, 120, true),
		up(17*time.Minute, 1, 0, 900, false),
		up(22*time.Minute, 2, 3, 300, false),
		up(27*time.Minute, 3, 5, 300, false),
		up(37*time.Minute, 5, 9, 300, false),
	}
	if _, err := st.InsertRainGaugeUplinks(ctx, first); err != nil {
		t.Fatal(err)
	}
	if n, err := c.RunOnce(ctx); err != nil || n != len(first) {
		t.Fatalf("RunOnce = %d, %v", n, err)
	}
	got := rainfall(t, st, SourceGauge, dev)
	want := []rainRow{
		{base, 120, 0, time.Time{}},
		{base.Add(2 * time.Minute), 900, 0, time.Time{}},
		{base.Add(17 * time.Minute), 300, 0.6, time.Time{}},
		{base.Add(22 * time.Minute), 300, 0.4, time.Time{}},
		{base.Add(27 * time.Minute), 600, 0.8, time.Time{}}, // widened over the missing uplink
	}
	assertRain(t, got, want)

	// Nothing new: nothing rewritten.
	if n, err := c.RunOnce(ctx); err != nil || n != 0 {
		t.Fatalf("idle RunOnce = %d, %v", n, err)
	}
	if again := rainfall(t, st, SourceGauge, dev); again[4].updated != got[4].updated {
		t.Error("an idle round rewrote a row")
	}

	// The late uplink splits the widened interval; the rain total is unchanged.
	if _, err := st.InsertRainGaugeUplinks(ctx, []store.RainGaugeUplink{up(32*time.Minute, 4, 7, 300, false)}); err != nil {
		t.Fatal(err)
	}
	if n, err := c.RunOnce(ctx); err != nil || n != 1 {
		t.Fatalf("late RunOnce = %d, %v", n, err)
	}
	after := rainfall(t, st, SourceGauge, dev)
	want[4] = rainRow{base.Add(27 * time.Minute), 300, 0.4, time.Time{}}
	want = append(want, rainRow{base.Add(27*time.Minute + m5), 300, 0.4, time.Time{}})
	assertRain(t, after, want)
	if !after[4].updated.After(got[4].updated) || after[3].updated != got[3].updated {
		t.Error("only the re-split interval may bump updated_at")
	}

	// A reboot mid-rain: counter restarts, tips since boot count.
	if _, err := st.InsertRainGaugeUplinks(ctx, []store.RainGaugeUplink{up(47*time.Minute, 0, 2, 240, true)}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	final := rainfall(t, st, SourceGauge, dev)
	last := final[len(final)-1]
	if !last.ts.Equal(base.Add(43*time.Minute)) || last.intervalS != 240 || last.mm != 0.4 {
		t.Errorf("after reset: %+v", last)
	}
	var total float64
	for _, r := range final {
		total += r.mm
	}
	// The 3 minutes between the uplink at 37 and the reboot at 43 are not
	// covered (tips there were lost with the reboot).
	if math.Abs(total-(9+2)*0.2) > 1e-9 {
		t.Errorf("total %.2f mm", total)
	}
	d, err := st.Queries().GetDevice(ctx, dev)
	if err != nil || d.Kind != "rain" {
		t.Errorf("device %+v %v", d, err)
	}
	var resets dto.Metric
	if err := m.GaugeResets.Write(&resets); err != nil || resets.GetCounter().GetValue() != 2 {
		t.Errorf("reset counter = %v (%v)", resets.GetCounter().GetValue(), err)
	}
}

func assertRain(t *testing.T, got, want []rainRow) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("rainfall rows = %+v, want %+v", got, want)
	}
	for i := range want {
		if !got[i].ts.Equal(want[i].ts) || got[i].intervalS != want[i].intervalS || math.Abs(got[i].mm-want[i].mm) > 1e-9 {
			t.Errorf("row %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestECCCPollerUpsertsIdempotently(t *testing.T) {
	st, _, m := setup(t)
	ctx := context.Background()
	srv, _ := geomet(t)
	client, err := NewECCCClient(srv.URL, srv.Client(), 6)
	if err != nil {
		t.Fatal(err)
	}
	cfg := DefaultECCCConfig()
	p := NewECCCPoller(client, cfg, st.Pool(), m, slog.New(slog.DiscardHandler))
	from, to := utc("2026-07-18T13:00:00Z"), utc("2026-07-18T21:00:00Z")
	if n, perr := p.PollOnce(ctx, from, to); perr != nil || n != 8 {
		t.Fatalf("first poll wrote %d, %v", n, perr)
	}
	rows := rainfall(t, st, SourceECCC, DefaultECCCStation)
	if len(rows) != 8 || !rows[4].ts.Equal(utc("2026-07-18T17:00:00Z")) || rows[4].mm != 9.3 || rows[4].intervalS != 3600 {
		t.Fatalf("rows = %+v", rows)
	}
	if n, perr := p.PollOnce(ctx, from, to); perr != nil || n != 0 {
		t.Fatalf("second poll wrote %d, %v", n, perr)
	}
	// ECCC revises an hour: the next poll restores the published value and bumps updated_at.
	if _, xerr := st.Pool().Exec(ctx, `UPDATE rainfall SET mm = 9.0 WHERE source = 'eccc' AND ts = $1`, utc("2026-07-18T17:00:00Z")); xerr != nil {
		t.Fatal(xerr)
	}
	if n, perr := p.PollOnce(ctx, from, to); perr != nil || n != 1 {
		t.Fatalf("revision poll wrote %d, %v", n, perr)
	}
	if again := rainfall(t, st, SourceECCC, DefaultECCCStation); again[4].mm != 9.3 || !again[4].updated.After(rows[4].updated) {
		t.Errorf("revised row = %+v", again[4])
	}
	near, err := st.Queries().RainNear(ctx, sqlcgen.RainNearParams{ScanFrom: from.Add(-24 * time.Hour), FromTs: utc("2026-07-18T16:30:00Z"), ToTs: utc("2026-07-18T18:30:00Z")})
	if err != nil || near.Mm != 9.3+1.1 || !near.CoveredUntil.Equal(utc("2026-07-18T19:00:00Z")) {
		t.Errorf("RainNear = %+v, %v", near, err)
	}
}
