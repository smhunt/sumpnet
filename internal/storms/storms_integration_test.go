//go:build integration

package storms

import (
	"context"
	"log/slog"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/smhunt/sumpnet/internal/codec"
	"github.com/smhunt/sumpnet/internal/hydrology"
	"github.com/smhunt/sumpnet/internal/sim"
	"github.com/smhunt/sumpnet/internal/store"
	"github.com/smhunt/sumpnet/internal/store/sqlcgen"
	"github.com/smhunt/sumpnet/internal/testinfra"
	"github.com/smhunt/sumpnet/internal/testpipeline"
	"github.com/smhunt/sumpnet/internal/watermark"
	"github.com/smhunt/sumpnet/internal/weather"
)

// insertEvents stores simulator events directly (no MQTT, no gRPC): this
// package's tests are about storm analytics, not transport.
func insertEvents(t *testing.T, st *store.Store, events []sim.Event) {
	t.Helper()
	ctx := context.Background()
	var readings []store.Reading
	var cycles []store.CycleEvent
	var summaries []store.StormSummary
	var gauges []store.RainGaugeUplink
	for _, ev := range events {
		u, err := codec.Decode(ev.FPort, ev.Payload)
		if err != nil {
			t.Fatal(err)
		}
		switch v := u.(type) {
		case *codec.Heartbeat:
			readings = append(readings, store.Reading{DeviceID: ev.DevEUI, TS: ev.Time, FCnt: int64(ev.FCnt), LevelMM: int32(v.LevelMM), BattMV: int32(v.BattMV), MainsOK: v.Flags.Has(codec.FlagMainsOK)})
		case *codec.CycleEvent:
			pump := "primary"
			if v.PumpID == codec.PumpBackup {
				pump = "backup"
			}
			cycles = append(cycles, store.CycleEvent{DeviceID: ev.DevEUI, StartedAt: ev.Time.Add(-time.Duration(v.StartOffsetS) * time.Second), FCnt: int64(ev.FCnt), ReceivedAt: ev.Time,
				RunS: int32(v.RunS), PeakCurrentA: float32(v.PeakCurrentDA) / 10, LevelStartMM: int32(v.LevelStartMM), LevelEndMM: int32(v.LevelEndMM), PumpID: pump})
		case *codec.StormSummary:
			summaries = append(summaries, store.StormSummary{DeviceID: ev.DevEUI, WindowEnd: ev.Time, FCnt: int64(ev.FCnt), WindowS: int32(v.WindowS), CycleCount: int32(v.Count), TotalRunS: int32(v.TotalRunS), MinLevelMM: int32(v.MinLevelMM)})
		case *codec.RainGauge:
			gauges = append(gauges, store.RainGaugeUplink{DeviceID: ev.DevEUI, TS: ev.Time, FCnt: int64(ev.FCnt), TipCount: int64(v.TipCount), MMPerTip: float64(v.MMPerTipUM) / 1000,
				IntervalS: int32(v.IntervalS), BattMV: int32(v.BattMV), CounterReset: v.Flags.Has(codec.RainCounterReset), SensorFault: v.Flags.Has(codec.RainSensorFault)})
		}
	}
	for i := 0; i < len(readings); i += 2000 {
		if _, err := st.InsertReadings(ctx, readings[i:min(i+2000, len(readings))]); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := st.InsertCycleEvents(ctx, cycles); err != nil {
		t.Fatal(err)
	}
	if len(summaries) > 0 {
		if _, err := st.InsertStormSummaries(ctx, summaries); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := st.InsertRainGaugeUplinks(ctx, gauges); err != nil {
		t.Fatal(err)
	}
}

func drain(t *testing.T, c *watermark.Consumer) {
	t.Helper()
	for i := 0; i < 1000; i++ {
		n, err := c.RunOnce(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if n == 0 {
			return
		}
	}
	t.Fatal("consumer did not drain")
}

func TestStormAnalyticsFromSimulatedStorm(t *testing.T) {
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
	engine := testpipeline.NewSim(t, "storm25-long", 42, 4, 2, 0)
	testpipeline.SeedHomes(t, st, engine)
	rec := &testpipeline.Recorder{}
	truth, err := engine.Run(ctx, rec)
	if err != nil {
		t.Fatal(err)
	}
	insertEvents(t, st, rec.Events)

	reg := prometheus.NewRegistry()
	log := slog.New(slog.DiscardHandler)
	wm := watermark.NewMetrics(reg)
	wcfg := watermark.Config{Lag: 0, MaxGroups: 100, PollInterval: time.Hour}
	wcfg.Consumer = "weather"
	wc := watermark.New(st.Pool(), dsn, wcfg, wm, log)
	watermark.Add(wc, weather.GaugeSource, weather.NewGaugeHandler(weather.NewMetrics(reg), log))
	drain(t, wc)

	// Listen for storm notifications before the analyzer runs.
	conn, err := st.Pool().Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	if _, lerr := conn.Exec(ctx, "LISTEN "+store.NotifyChannel); lerr != nil {
		t.Fatal(lerr)
	}

	scfg := wcfg
	scfg.Consumer = "storm-analytics"
	sc := watermark.New(st.Pool(), dsn, scfg, wm, log)
	m := NewMetrics(reg)
	NewAnalyzer(hydrology.DefaultResponseParams(), m, log).AddStages(sc)
	drain(t, sc)

	q := st.Queries()
	storms, err := q.ListStormEventsOverlapping(ctx, sqlcgen.ListStormEventsOverlappingParams{FromTs: testpipeline.TestStart, ToTs: testpipeline.TestStart.Add(7 * 24 * time.Hour)})
	if err != nil || len(storms) != 1 {
		t.Fatalf("storms = %+v, %v", storms, err)
	}
	s, ts := storms[0], truth.Storms[0]
	if s.Status != "closed" || s.RainSource != "gauge" || !s.EndedAt.Valid {
		t.Errorf("storm = %+v", s)
	}
	if d := s.StartedAt.Sub(ts.Onset); d < -5*time.Minute || d > 5*time.Minute {
		t.Errorf("onset %v, truth %v", s.StartedAt, ts.Onset)
	}
	if d := s.EndedAt.Time.Sub(ts.RainEnd); d < -10*time.Minute || d > 10*time.Minute {
		t.Errorf("rain end %v, truth %v", s.EndedAt.Time, ts.RainEnd)
	}
	if math.Abs(s.TotalRainMm-ts.TotalMm) > 0.3 {
		t.Errorf("total %.2f mm, truth %.2f", s.TotalRainMm, ts.TotalMm)
	}
	metrics, err := q.ListHomeMetricsForStorm(ctx, s.ID)
	if err != nil || len(metrics) != 4 {
		t.Fatalf("metrics = %+v, %v", metrics, err)
	}
	homeIndex := map[string]int{}
	for _, h := range truth.Homes {
		homeIndex[h.HomeID] = h.Index
	}
	for _, hm := range metrics {
		ht := ts.Homes[homeIndex[hm.HomeID.String()]]
		if !hm.LagMin.Valid || !hm.RecessionMin.Valid || !hm.BaseflowCpd.Valid || hm.Cycles == 0 || hm.VolumeL <= 0 {
			t.Errorf("home %d metrics = %+v", ht.HomeIndex, hm)
			continue
		}
		if math.Abs(hm.LagMin.Float64-ht.LagMin) > 15 || math.Abs(hm.RecessionMin.Float64-ht.RecessionMin) > 0.1*ht.RecessionMin {
			t.Errorf("home %d lag %.1f (truth %.0f) recession %.1f (truth %.0f)", ht.HomeIndex, hm.LagMin.Float64, ht.LagMin, hm.RecessionMin.Float64, ht.RecessionMin)
		}
		if p := truth.Homes[ht.HomeIndex]; math.Abs(hm.BaseflowCpd.Float64-p.BaseflowCPD) > 0.03*p.BaseflowCPD {
			t.Errorf("home %d baseflow %.3f, truth %.3f", ht.HomeIndex, hm.BaseflowCpd.Float64, p.BaseflowCPD)
		}
	}
	wctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	n, err := conn.Conn().WaitForNotification(wctx)
	cancel()
	if err != nil || !strings.Contains(n.Payload, `"table":"storm_events"`) {
		t.Errorf("notification = %+v, %v", n, err)
	}

	// Recomputation is idempotent: replaying every source changes nothing.
	before := metrics
	if _, err := st.Pool().Exec(ctx, `DELETE FROM consumer_watermarks WHERE consumer = 'storm-analytics'`); err != nil {
		t.Fatal(err)
	}
	updates := func() float64 {
		var total float64
		fams, _ := reg.Gather()
		for _, f := range fams {
			if f.GetName() == "sumpnet_storms_events_total" || f.GetName() == "sumpnet_storms_home_metrics_total" {
				for _, mm := range f.GetMetric() {
					for _, l := range mm.GetLabel() {
						if l.GetValue() != "unchanged" {
							total += mm.GetCounter().GetValue()
						}
					}
				}
			}
		}
		return total
	}
	was := updates()
	drain(t, sc)
	if now := updates(); now != was {
		t.Errorf("replay wrote %v more rows", now-was)
	}
	after, _ := q.ListHomeMetricsForStorm(ctx, s.ID)
	for i := range after {
		if !after[i].ComputedAt.Equal(before[i].ComputedAt) {
			t.Errorf("home %s recomputed on replay", after[i].HomeID)
		}
	}

	// Without gauge data the ECCC record is the fallback, and the storm keeps its id.
	if _, err := st.Pool().Exec(ctx, `DELETE FROM rainfall WHERE source = 'gauge'`); err != nil {
		t.Fatal(err)
	}
	var hourly []sqlcgen.UpsertRainfallParams
	for _, smp := range truth.Rainfall {
		hour := smp.TS.Truncate(time.Hour)
		if n := len(hourly); n == 0 || !hourly[n-1].Ts.Equal(hour) {
			hourly = append(hourly, sqlcgen.UpsertRainfallParams{Source: "eccc", StationID: "6144478", Ts: hour, IntervalS: 3600})
		}
		hourly[len(hourly)-1].Mm += smp.Mm
	}
	for _, h := range hourly {
		h.Mm = math.Round(h.Mm*10) / 10
		if _, err := q.UpsertRainfall(ctx, h); err != nil {
			t.Fatal(err)
		}
	}
	drain(t, sc)
	storms2, _ := q.ListStormEventsOverlapping(ctx, sqlcgen.ListStormEventsOverlappingParams{FromTs: testpipeline.TestStart, ToTs: testpipeline.TestStart.Add(7 * 24 * time.Hour)})
	if len(storms2) != 1 || storms2[0].ID != s.ID || storms2[0].RainSource != "eccc" || math.Abs(storms2[0].TotalRainMm-ts.TotalMm) > 0.5 {
		t.Errorf("ECCC fallback storms = %+v (was %+v)", storms2, s)
	}

	// Rain that turns out to be nothing removes the storm and its metrics.
	if _, err := st.Pool().Exec(ctx, `UPDATE rainfall SET mm = 0, updated_at = now()`); err != nil {
		t.Fatal(err)
	}
	drain(t, sc)
	var left int
	if err := st.Pool().QueryRow(ctx, `SELECT (SELECT count(*) FROM storm_events) + (SELECT count(*) FROM home_storm_metrics)`).Scan(&left); err != nil || left != 0 {
		t.Errorf("rows left after the rain vanished: %d (%v)", left, err)
	}
}
