//go:build integration

package e2e

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/smhunt/sumpnet/internal/detector"
	"github.com/smhunt/sumpnet/internal/hydrology"
	"github.com/smhunt/sumpnet/internal/platform"
	"github.com/smhunt/sumpnet/internal/sim"
	"github.com/smhunt/sumpnet/internal/store"
	"github.com/smhunt/sumpnet/internal/store/sqlcgen"
	"github.com/smhunt/sumpnet/internal/storms"
	"github.com/smhunt/sumpnet/internal/testinfra"
	"github.com/smhunt/sumpnet/internal/testpipeline"
	"github.com/smhunt/sumpnet/internal/watermark"
	"github.com/smhunt/sumpnet/internal/weather"
)

// Phase 4 acceptance (prompt_plan.md §12): a simulated storm, including the
// two rain gauge nodes, flows sim → Mosquitto → lora-bridge → ingest →
// weather (gauge rainfall) → cycle-detector (est volume) → storm-analytics,
// with ECCC disabled, and the storm and every home's response must match the
// simulator's ground truth.
//
// Truth fields compared: StormTruth onset / rain_end / total_mm, and per home
// HomeStormTruth.lag_min and recession_min — the §10 definitions applied to
// the simulator's noise-free, continuous inflow rate. lag_min_discrete is not
// used: its 30-minute rolling window turns a single cycle into 48 cycles/day,
// which already exceeds 2× baseflow for every home below 24 cycles/day, so it
// measures "time to the next cycle" rather than the hydrological response
// (it reads 0 min for several high-baseflow homes).
//
// Tolerances:
//   - recession: ±10 % of truth for every home whose truth is defined;
//   - lag: ±10 % of truth, or ±15 min where that is larger. 15 min is one
//     heartbeat interval: the pit level is observed every 15 min, so the
//     instant the inflow first exceeds 2× baseflow can only be placed within
//     the heartbeat interval that contains it, and the gauges (0.2 mm tips,
//     5-min wake) see the rain onset minutes late. With truth lags of 11–30
//     min for fast homes, ±10 % (1–3 min) is below that resolution. The
//     median absolute lag error must stay within 5 min, and every lag and
//     recession error is logged. See progress.md (Phase 4) for the evidence
//     and the owner decision this rests on.
const (
	phase4Scenario = "storm50-long"
	phase4Seed     = 42
	phase4Homes    = 16
	phase4Segments = 8 // the first 16 homes are the same homes as in a 60-home run
)

type phase4Stack struct {
	*stack
}

func newPhase4Stack(t *testing.T) *phase4Stack {
	t.Helper()
	ctx := context.Background()
	s := &stack{dsn: testinfra.StartPostgres(t), broker: testinfra.StartMosquitto(t)}
	if err := store.Migrate(ctx, s.dsn); err != nil {
		t.Fatal(err)
	}
	st, err := store.New(ctx, s.dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	s.st = st
	s.engine = testpipeline.NewSim(t, phase4Scenario, phase4Seed, phase4Homes, phase4Segments, 0)
	s.homes = s.engine.Homes()
	s.byDev = map[string]sim.HomeParams{}
	for _, h := range s.homes {
		s.byDev[h.DevEUI] = h
	}
	testpipeline.SeedHomes(t, st, s.engine)
	ingestAddr := testpipeline.StartIngest(t, st)
	testpipeline.StartLoraBridge(t, s.broker, ingestAddr)

	wm := watermark.Config{Lag: time.Second, PollInterval: time.Second, MaxGroups: 50}

	dapp := platform.NewApp(platform.Config{Service: "cycle-detector-test"}, quietLog())
	dcfg := detector.Config{Watermark: wm, HealthyCyclesToClear: 3, Window: 10, PitAreaTTL: time.Minute}
	dcfg.Watermark.Consumer = "cycle-detector"
	s.runService(t, "cycle-detector", dapp, func(ctx context.Context) error { return detector.Run(ctx, dapp, s.dsn, dcfg) })

	wapp := platform.NewApp(platform.Config{Service: "weather-test"}, quietLog())
	wcfg := weather.Config{Watermark: wm, ECCC: weather.DefaultECCCConfig()}
	wcfg.Watermark.Consumer = "weather"
	wcfg.ECCC.Enabled = false
	s.runService(t, "weather", wapp, func(ctx context.Context) error { return weather.Run(ctx, wapp, s.dsn, wcfg) })

	sapp := platform.NewApp(platform.Config{Service: "storm-analytics-test"}, quietLog())
	scfg := storms.Config{Watermark: wm, Params: hydrology.DefaultResponseParams()}
	scfg.Watermark.Consumer = "storm-analytics"
	s.runService(t, "storm-analytics", sapp, func(ctx context.Context) error { return storms.Run(ctx, sapp, s.dsn, scfg) })
	return &phase4Stack{s}
}

// waitSettled blocks until every consumer has passed the newest row of each
// of its sources and the storm tables have not changed for three polls.
func (s *phase4Stack) waitSettled(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	type src struct{ consumer, table, col string }
	sources := []src{
		{"weather", "rain_gauge_uplinks", "inserted_at"},
		{"cycle-detector", "cycle_events", "inserted_at"},
		{"cycle-detector", "storm_summaries", "inserted_at"},
		{"storm-analytics", "rainfall", "updated_at"},
		{"storm-analytics", "cycle_events", "inserted_at"},
		{"storm-analytics", "storm_summaries", "inserted_at"},
		{"storm-analytics", "readings", "inserted_at"},
	}
	deadline := time.Now().Add(5 * time.Minute)
	last, stable := "", 0
	for {
		caught := true
		for _, sc := range sources {
			var newest sql.NullTime
			if err := s.st.Pool().QueryRow(ctx, fmt.Sprintf("SELECT max(%s) FROM %s", sc.col, sc.table)).Scan(&newest); err != nil {
				t.Fatal(err)
			}
			wm, err := s.st.Queries().GetWatermark(ctx, sqlcgen.GetWatermarkParams{Consumer: sc.consumer, Source: sc.table})
			if newest.Valid && (err != nil || wm.Before(newest.Time)) {
				caught = false
			}
		}
		var sig string
		if err := s.st.Pool().QueryRow(ctx, `SELECT (SELECT count(*) FROM storm_events)::text || '/' || coalesce((SELECT max(updated_at) FROM storm_events)::text, '') || '/' ||
			(SELECT count(*) FROM home_storm_metrics)::text || '/' || coalesce((SELECT max(computed_at) FROM home_storm_metrics)::text, '')`).Scan(&sig); err != nil {
			t.Fatal(err)
		}
		if caught && sig == last {
			stable++
		} else {
			stable = 0
		}
		last = sig
		if stable >= 3 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("storm analytics did not settle (caught up %v, %s)", caught, sig)
		}
		time.Sleep(time.Second)
	}
}

func TestPhase4StormAcceptance(t *testing.T) {
	s := newPhase4Stack(t)
	began := time.Now()
	truth, events := testpipeline.Replay(t, s.broker, s.engine, phase4Seed)
	testpipeline.WaitForCounts(t, s.st.Queries(), testpipeline.Expected(events), 3*time.Minute)
	s.waitSettled(t)
	t.Logf("%s seed %d × %d homes: %d events replayed and analysed in %v", phase4Scenario, phase4Seed, phase4Homes, len(events), time.Since(began).Round(time.Second))
	ctx := context.Background()
	q := s.st.Queries()

	// --- the storm -------------------------------------------------------------
	if len(truth.Storms) != 1 {
		t.Fatalf("truth has %d storms", len(truth.Storms))
	}
	ts := truth.Storms[0]
	rows, err := q.ListStormEventsOverlapping(ctx, sqlcgen.ListStormEventsOverlappingParams{FromTs: testpipeline.TestStart, ToTs: truth.EndedAt})
	if err != nil || len(rows) != 1 {
		t.Fatalf("storm_events = %+v, %v", rows, err)
	}
	se := rows[0]
	onsetErr, endErr := se.StartedAt.Sub(ts.Onset), se.EndedAt.Time.Sub(ts.RainEnd)
	t.Logf("storm: onset %s (truth %s, %+v), rain end %s (truth %s, %+v), total %.2f mm (truth %.2f), peak %.1f mm/h (truth %.1f), source %s, status %s",
		se.StartedAt.UTC().Format(time.RFC3339), ts.Onset.UTC().Format(time.RFC3339), onsetErr, se.EndedAt.Time.UTC().Format(time.RFC3339), ts.RainEnd.UTC().Format(time.RFC3339), endErr,
		se.TotalRainMm, ts.TotalMm, se.PeakIntensityMmH, ts.PeakMmPerH, se.RainSource, se.Status)
	if se.Status != "closed" || !se.EndedAt.Valid || se.RainSource != "gauge" {
		t.Errorf("storm = %+v", se)
	}
	if onsetErr.Abs() > 10*time.Minute || endErr.Abs() > 10*time.Minute {
		t.Errorf("onset error %v, rain end error %v (tolerance 10 min)", onsetErr, endErr)
	}
	if math.Abs(se.TotalRainMm-ts.TotalMm) > 0.5 {
		t.Errorf("total %.2f mm, truth %.2f mm (tolerance 0.5 mm: 0.2 mm tips)", se.TotalRainMm, ts.TotalMm)
	}
	if math.Abs(se.PeakIntensityMmH-ts.PeakMmPerH) > 0.2*ts.PeakMmPerH {
		t.Errorf("peak %.1f mm/h, truth %.1f (tolerance 20 %%: 5-minute bins)", se.PeakIntensityMmH, ts.PeakMmPerH)
	}
	var eccc int
	if qerr := s.st.Pool().QueryRow(ctx, `SELECT count(*) FROM rainfall WHERE source = 'eccc'`).Scan(&eccc); qerr != nil || eccc != 0 {
		t.Errorf("ECCC rows = %d (%v); the poller is disabled", eccc, qerr)
	}

	// --- every home ------------------------------------------------------------
	metrics, err := q.ListHomeStormMetrics(ctx, se.ID)
	if err != nil {
		t.Fatal(err)
	}
	byHome := map[string]sqlcgen.HomeStormMetric{}
	for _, m := range metrics {
		byHome[m.HomeID.String()] = m
	}
	if len(metrics) != phase4Homes {
		t.Errorf("%d home_storm_metrics rows, want %d", len(metrics), phase4Homes)
	}
	var table strings.Builder
	fmt.Fprintf(&table, "\n%4s %6s %6s %9s | %6s %6s %7s %6s | %6s %6s %7s | %6s %5s %7s %7s\n", "home", "base", "est", "delayMin", "lag", "est", "err", "err%", "rec", "est", "err%", "cycles", "truth", "vol_L", "truthL")
	var lagAbs []float64
	lagStrict, lagN, recN := 0, 0, 0
	for i, p := range s.homes {
		ht := ts.Homes[i]
		m, ok := byHome[p.HomeID]
		if !ok {
			t.Errorf("home %d has no metrics", i)
			continue
		}
		if !m.BaseflowCpd.Valid || math.Abs(m.BaseflowCpd.Float64-p.BaseflowCPD) > 0.03*p.BaseflowCPD {
			t.Errorf("home %d baseflow %v, truth %.3f cycles/day (tolerance 3 %%)", i, m.BaseflowCpd, p.BaseflowCPD)
		}
		lagErr, lagPct, recPct := math.NaN(), math.NaN(), math.NaN()
		if ht.LagMin >= 0 {
			lagN++
			if !m.LagMin.Valid {
				t.Errorf("home %d: truth lag %.0f min, none detected", i, ht.LagMin)
			} else {
				lagErr = m.LagMin.Float64 - ht.LagMin
				lagPct = 100 * lagErr / ht.LagMin
				lagAbs = append(lagAbs, math.Abs(lagErr))
				if math.Abs(lagErr) <= 0.1*ht.LagMin {
					lagStrict++
				}
				if tol := math.Max(0.1*ht.LagMin, 15); math.Abs(lagErr) > tol {
					t.Errorf("home %d: lag %.1f min, truth %.0f (error %+.1f min > %.1f)", i, m.LagMin.Float64, ht.LagMin, lagErr, tol)
				}
			}
		}
		if ht.RecessionMin >= 0 {
			recN++
			if !m.RecessionMin.Valid {
				t.Errorf("home %d: truth recession %.0f min, none detected", i, ht.RecessionMin)
			} else {
				recPct = 100 * (m.RecessionMin.Float64 - ht.RecessionMin) / ht.RecessionMin
				if math.Abs(recPct) > 10 {
					t.Errorf("home %d: recession %.1f min, truth %.0f (error %+.1f %% > 10 %%)", i, m.RecessionMin.Float64, ht.RecessionMin, recPct)
				}
			}
		}
		fmt.Fprintf(&table, "%4d %6.2f %6.2f %9.1f | %6.0f %6.1f %+7.1f %+6.1f | %6.0f %6.1f %+7.1f | %6d %5d %7.0f %7.0f\n",
			i, p.BaseflowCPD, m.BaseflowCpd.Float64, p.DelayMin, ht.LagMin, m.LagMin.Float64, lagErr, lagPct, ht.RecessionMin, m.RecessionMin.Float64, recPct, m.Cycles, ht.Cycles, m.VolumeL, ht.VolumeL)
		if m.Cycles <= 0 || m.VolumeL <= 0 {
			t.Errorf("home %d: %d cycles, %.0f L", i, m.Cycles, m.VolumeL)
		}
	}
	sort.Float64s(lagAbs)
	med := math.NaN()
	if n := len(lagAbs); n > 0 {
		med = lagAbs[n/2]
		if n%2 == 0 {
			med = (lagAbs[n/2-1] + lagAbs[n/2]) / 2
		}
	}
	t.Logf("per-home results (truth = HomeStormTruth.lag_min / recession_min):%s", table.String())
	t.Logf("lag: %d/%d homes within ±10 %%, median |error| %.1f min; recession: %d homes with defined truth, all checked at ±10 %%", lagStrict, lagN, med, recN)
	if lagN == 0 || recN == 0 {
		t.Fatalf("no home with defined truth (lag %d, recession %d)", lagN, recN)
	}
	if med > 5 {
		t.Errorf("median absolute lag error %.1f min > 5 min", med)
	}
}
