//go:build integration

// Package e2e holds the phase acceptance tests that run every service
// in-process against testcontainers.
package e2e

import (
	"context"
	"database/sql"
	"log/slog"
	"net"
	"os"
	"sort"
	"strconv"
	"testing"
	"time"

	"github.com/google/uuid"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"

	alertsv1 "github.com/smhunt/sumpnet/gen/go/sumpnet/alerts/v1"
	"github.com/smhunt/sumpnet/internal/alerts"
	"github.com/smhunt/sumpnet/internal/codec"
	"github.com/smhunt/sumpnet/internal/detector"
	"github.com/smhunt/sumpnet/internal/hydrology"
	"github.com/smhunt/sumpnet/internal/platform"
	"github.com/smhunt/sumpnet/internal/sim"
	"github.com/smhunt/sumpnet/internal/store"
	"github.com/smhunt/sumpnet/internal/store/sqlcgen"
	"github.com/smhunt/sumpnet/internal/testinfra"
	"github.com/smhunt/sumpnet/internal/testpipeline"
	"github.com/smhunt/sumpnet/internal/watermark"
)

const (
	dryRun        = alertsv1.AlertCode_ALERT_CODE_DRY_RUN
	shortCycling  = alertsv1.AlertCode_ALERT_CODE_SHORT_CYCLING
	continuousRun = alertsv1.AlertCode_ALERT_CODE_CONTINUOUS_RUN
	floatHigh     = alertsv1.AlertCode_ALERT_CODE_FLOAT_HIGH
	mainsLost     = alertsv1.AlertCode_ALERT_CODE_MAINS_LOST
	outageRisk    = alertsv1.AlertCode_ALERT_CODE_OUTAGE_RISK
)

// stack is everything running for one scenario.
type stack struct {
	st       *store.Store
	dsn      string
	broker   string
	engine   *sim.Engine
	notifier *alerts.RecordingNotifier
	grpcAddr string
	homes    []sim.HomeParams
	byDev    map[string]sim.HomeParams
}

func newStack(t *testing.T, scenario string, seed uint64, homes, segments int, duration time.Duration) *stack {
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
	s.engine = testpipeline.NewSim(t, scenario, seed, homes, segments, duration)
	s.homes = s.engine.Homes()
	s.byDev = map[string]sim.HomeParams{}
	for _, h := range s.homes {
		s.byDev[h.DevEUI] = h
	}
	testpipeline.SeedHomes(t, st, s.engine)

	ingestAddr := testpipeline.StartIngest(t, st)
	testpipeline.StartLoraBridge(t, s.broker, ingestAddr)

	wm := watermark.Config{Lag: time.Second, PollInterval: time.Second, MaxGroups: 50}
	// cycle-detector
	dapp := platform.NewApp(platform.Config{Service: "cycle-detector-test"}, quietLog())
	dcfg := detector.Config{Watermark: wm, HealthyCyclesToClear: 3, Window: 10, PitAreaTTL: time.Minute}
	dcfg.Watermark.Consumer = "cycle-detector"
	s.runService(t, "cycle-detector", dapp, func(ctx context.Context) error { return detector.Run(ctx, dapp, s.dsn, dcfg) })
	// alerts
	aapp := platform.NewApp(platform.Config{Service: "alerts-test", ShutdownTimeout: 5 * time.Second}, quietLog())
	acfg := alerts.DefaultConfig()
	acfg.Watermark = wm
	acfg.Watermark.Consumer = "alerts"
	acfg.OfflineAfter = 0
	s.notifier = &alerts.RecordingNotifier{}
	s.grpcAddr = freePort(t)
	s.runService(t, "alerts", aapp, func(ctx context.Context) error {
		return alerts.Run(ctx, aapp, acfg, alerts.RunOptions{DSN: s.dsn, GRPCAddr: s.grpcAddr, Notifier: s.notifier})
	})
	return s
}

func quietLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
}

func freePort(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	_ = l.Close()
	return addr
}

func (s *stack) runService(t *testing.T, name string, app *platform.App, run func(context.Context) error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("%s: %v", name, err)
			}
		case <-time.After(30 * time.Second):
			t.Errorf("%s did not stop", name)
		}
	})
	testpipeline.WaitReady(t, app, done, name)
}

// waitCaughtUp blocks until every consumer watermark has passed the newest
// row of its source and the alerts table has been stable for two polls.
func (s *stack) waitCaughtUp(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	sources := map[string][]string{"cycle-detector": {"cycle_events"}, "alerts": {"alarm_events", "readings", "detections"}}
	deadline := time.Now().Add(3 * time.Minute)
	var lastCount int64 = -1
	stable := 0
	for {
		caught := true
		for consumer, tables := range sources {
			for _, tb := range tables {
				var maxIns sql.NullTime
				if err := s.st.Pool().QueryRow(ctx, "SELECT max(inserted_at) FROM "+tb).Scan(&maxIns); err != nil {
					t.Fatal(err)
				}
				wm, err := s.st.Queries().GetWatermark(ctx, sqlcgen.GetWatermarkParams{Consumer: consumer, Source: tb})
				if err != nil || (maxIns.Valid && wm.Before(maxIns.Time)) {
					caught = false
				}
			}
		}
		var n int64
		_ = s.st.Pool().QueryRow(ctx, "SELECT count(*) FROM alerts").Scan(&n)
		if caught && n == lastCount {
			stable++
		} else {
			stable = 0
		}
		lastCount = n
		if stable >= 2 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("consumers did not catch up in time")
		}
		time.Sleep(time.Second)
	}
}

func (s *stack) allAlerts(t *testing.T) []sqlcgen.Alert {
	t.Helper()
	var out []sqlcgen.Alert
	for _, h := range s.homes {
		rows, err := s.st.Queries().ListAlertsForDevice(context.Background(), h.DevEUI)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, rows...)
	}
	return out
}

// oracle derives, from the recorded events and the same hydrology rules, which
// homes must carry which alerts, and the earliest triggering event time.
type oracle struct {
	homes    map[alertsv1.AlertCode]map[int]bool
	earliest map[[2]int]time.Time   // (home, code) → first trigger
	triggers map[[2]int][]time.Time // (home, code) → every trigger, in event order
}

func buildOracle(events []sim.Event) *oracle {
	o := &oracle{homes: map[alertsv1.AlertCode]map[int]bool{}, earliest: map[[2]int]time.Time{}, triggers: map[[2]int][]time.Time{}}
	mark := func(code alertsv1.AlertCode, home int, at time.Time) {
		if o.homes[code] == nil {
			o.homes[code] = map[int]bool{}
		}
		o.homes[code][home] = true
		k := [2]int{home, int(code)}
		if e, ok := o.earliest[k]; !ok || at.Before(e) {
			o.earliest[k] = at
		}
		o.triggers[k] = append(o.triggers[k], at)
	}
	type pumpKey struct {
		home int
		pump codec.PumpID
	}
	windows := map[pumpKey][]hydrology.Cycle{}
	sorted := append([]sim.Event(nil), events...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].Time.Before(sorted[j].Time) })
	for _, ev := range sorted {
		u, err := codec.Decode(ev.FPort, ev.Payload)
		if err != nil {
			continue
		}
		home := ev.HomeIndex
		switch v := u.(type) {
		case *codec.Alarm:
			mark(alertsv1.AlertCode(v.Code), home, ev.Time)
		case *codec.Heartbeat:
			if v.Flags.Has(codec.FlagFloatHigh) {
				mark(floatHigh, home, ev.Time)
			}
			if !v.Flags.Has(codec.FlagMainsOK) {
				mark(mainsLost, home, ev.Time)
			}
		case *codec.StormSummary:
			if hydrology.SummaryShortCycling(int32(v.Count), int32(v.WindowS)) {
				mark(shortCycling, home, ev.Time)
			}
		case *codec.CycleEvent:
			c := hydrology.Cycle{StartedAt: ev.Time.Add(-time.Duration(v.StartOffsetS) * time.Second), RunS: int32(v.RunS), LevelStartMM: int32(v.LevelStartMM), LevelEndMM: int32(v.LevelEndMM)}
			if hydrology.IsDryRun(c) {
				mark(dryRun, home, ev.Time)
			}
			if hydrology.IsContinuousRun(c) {
				mark(continuousRun, home, c.StartedAt.Add(hydrology.ContinuousRun))
			}
			k := pumpKey{home, v.PumpID}
			w := append(windows[k], c)
			if len(w) > 10 {
				w = w[len(w)-10:]
			}
			windows[k] = w
			if hydrology.ShortCyclingRun(w) >= hydrology.ShortCycleMinCount {
				mark(shortCycling, home, c.EndedAt())
			}
		}
	}
	return o
}

func TestPhase3FailingPump(t *testing.T) {
	s := newStack(t, "failing-pump", 5, 12, 4, 4*24*time.Hour)
	truth, events := testpipeline.Replay(t, s.broker, s.engine, 5)
	testpipeline.WaitForCounts(t, s.st.Queries(), testpipeline.Expected(events), 2*time.Minute)
	s.waitCaughtUp(t)
	o := buildOracle(events)
	got := s.allAlerts(t)
	t.Logf("%d alerts from %d events (%d true cycles)", len(got), len(events), len(truth.TrueCycles))
	s.logDiagnostics(t, got)

	// Which homes carry which codes — must equal the oracle exactly for the
	// pump-health codes, and include the scenario's named homes.
	have := map[alertsv1.AlertCode]map[int]bool{}
	for _, a := range got {
		code := alertsv1.AlertCode(a.Code)
		if have[code] == nil {
			have[code] = map[int]bool{}
		}
		have[code][s.byDev[a.DeviceID].Index] = true
	}
	for _, code := range []alertsv1.AlertCode{dryRun, continuousRun, floatHigh, shortCycling} {
		if !sameSet(have[code], o.homes[code]) {
			t.Errorf("%s homes = %v, oracle %v", alerts.CodeName(code), keys(have[code]), keys(o.homes[code]))
		}
	}
	for code, home := range map[alertsv1.AlertCode]int{dryRun: 9, shortCycling: 11, continuousRun: 7} {
		if !have[code][home] {
			t.Errorf("home %d must have %s", home, alerts.CodeName(code))
		}
	}
	for _, h := range s.homes {
		if h.Health == sim.PumpHealthy {
			for _, code := range []alertsv1.AlertCode{dryRun, continuousRun, floatHigh} {
				if have[code][h.Index] {
					t.Errorf("healthy home %d has %s", h.Index, alerts.CodeName(code))
				}
			}
		}
	}

	// Timing on event time: the first alert of each (home, code) is raised within
	// 2 minutes of the first trigger; later episodes are raised within 2 minutes
	// of some trigger (a node hold-off can defer the re-alarm to the next heartbeat).
	sort.Slice(got, func(i, j int) bool { return got[i].RaisedAt.Before(got[j].RaisedAt) })
	seenEpisode := map[[2]int]bool{}
	for _, a := range got {
		home := s.byDev[a.DeviceID].Index
		k := [2]int{home, int(a.Code)}
		trig, ok := o.triggers[k]
		if !ok {
			continue
		}
		if !seenEpisode[k] {
			seenEpisode[k] = true
			if d := a.RaisedAt.Sub(o.earliest[k]); d < -10*time.Second || d > 2*time.Minute {
				t.Errorf("home %d %s first raised %v after its first trigger (%v vs %v)", home, alerts.CodeName(alertsv1.AlertCode(a.Code)), d, a.RaisedAt, o.earliest[k])
			}
			continue
		}
		near := false
		for _, tt := range trig {
			if d := a.RaisedAt.Sub(tt); d >= -10*time.Second && d <= 2*time.Minute {
				near = true
				break
			}
		}
		if !near {
			t.Errorf("home %d %s raised at %v with no trigger in the preceding 2 minutes", home, alerts.CodeName(alertsv1.AlertCode(a.Code)), a.RaisedAt)
		}
	}

	// Home 9 (dry-running throughout) is still open at the end.
	var open9 bool
	for _, a := range got {
		if s.byDev[a.DeviceID].Index == 9 && a.Code == int16(dryRun) && !a.ResolvedAt.Valid {
			open9 = true
		}
	}
	if !open9 {
		t.Error("home 9 DRY_RUN should still be open")
	}

	// Estimated volumes: present everywhere, and on healthy primary cycles
	// within 6 % of the simulator's true cycle volume.
	rows, err := s.st.Pool().Query(context.Background(), `SELECT device_id, avg(est_volume_l), count(*) FILTER (WHERE est_volume_l IS NULL) FROM cycle_events WHERE pump_id = 'primary' GROUP BY 1`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var dev string
		var avg float64
		var nulls int64
		if serr := rows.Scan(&dev, &avg, &nulls); serr != nil {
			t.Fatal(serr)
		}
		h := s.byDev[dev]
		if nulls != 0 {
			t.Errorf("home %d: %d cycles without a volume", h.Index, nulls)
		}
		if h.Health == sim.PumpHealthy {
			if want := h.CycleVolumeL(); avg < 0.94*want || avg > 1.06*want {
				t.Errorf("home %d: mean est volume %.1f L, true %.1f L", h.Index, avg, want)
			}
		}
	}
	rows.Close()

	// gRPC lifecycle.
	conn, err := grpc.NewClient(s.grpcAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	client := alertsv1.NewAlertServiceClient(conn)
	ctx := context.Background()
	home9 := s.homes[9].HomeID
	list, err := client.ListActiveAlerts(ctx, &alertsv1.ListActiveAlertsRequest{HomeId: home9})
	if err != nil || len(list.GetAlerts()) == 0 {
		t.Fatalf("ListActiveAlerts(home 9) = %v, %v", list, err)
	}
	id := list.GetAlerts()[0].GetId()
	a1, err := client.Acknowledge(ctx, &alertsv1.AcknowledgeRequest{AlertId: id, Note: "checked"})
	if err != nil || a1.GetAlert().GetAckedAt() == nil {
		t.Fatalf("Acknowledge: %v %v", a1, err)
	}
	a2, _ := client.Acknowledge(ctx, &alertsv1.AcknowledgeRequest{AlertId: id})
	if !a2.GetAlert().GetAckedAt().AsTime().Equal(a1.GetAlert().GetAckedAt().AsTime()) {
		t.Error("Acknowledge is not idempotent")
	}
	if _, nerr := client.Acknowledge(ctx, &alertsv1.AcknowledgeRequest{AlertId: uuid.New().String()}); status.Code(nerr) != codes.NotFound {
		t.Errorf("unknown id → %v", nerr)
	}
	seg, err := client.ListActiveAlerts(ctx, &alertsv1.ListActiveAlertsRequest{SegmentId: s.homes[9].SegmentID})
	if err != nil || len(seg.GetAlerts()) == 0 {
		t.Errorf("ListActiveAlerts(segment) = %v, %v", seg, err)
	}

	// Notifications: exactly one raise message per row at WARNING or above.
	var wantSent int
	for _, a := range got {
		if a.NotifyState == alerts.StateSent {
			wantSent++
		}
		if a.NotifyState == alerts.StatePending || a.NotifyState == alerts.StateFailed {
			t.Errorf("alert %s still %s", a.ID, a.NotifyState)
		}
	}
	raised := 0
	for _, m := range s.notifier.Messages() {
		if m.Event == "raised" {
			raised++
		}
	}
	if raised != wantSent || wantSent == 0 {
		t.Errorf("raise notifications = %d, sent rows = %d", raised, wantSent)
	}
}

func TestPhase3Outage(t *testing.T) {
	s := newStack(t, "outage", 11, 24, 8, 0)
	_, events := testpipeline.Replay(t, s.broker, s.engine, 11)
	testpipeline.WaitForCounts(t, s.st.Queries(), testpipeline.Expected(events), 2*time.Minute)
	s.waitCaughtUp(t)
	o := buildOracle(events)
	got := s.allAlerts(t)

	out := sim.Scenarios()["outage"].Outages[0]
	start := testpipeline.TestStart.Add(out.At)
	end := start.Add(out.Duration)
	affected := map[int]bool{}
	for _, seg := range s.engine.Segments() {
		for _, si := range out.SegmentIndexes {
			if seg.Index == si {
				for _, h := range seg.HomeIndexes {
					affected[h] = true
				}
			}
		}
	}
	seen := map[int]bool{}
	for _, a := range got {
		home := s.byDev[a.DeviceID].Index
		switch alertsv1.AlertCode(a.Code) {
		case mainsLost:
			seen[home] = true
			if !affected[home] {
				t.Errorf("home %d outside the outage has MAINS_LOST", home)
			}
			if a.RaisedAt.Before(start) || a.RaisedAt.After(start.Add(2*time.Minute)) {
				t.Errorf("home %d MAINS_LOST raised at %v, outage began %v", home, a.RaisedAt, start)
			}
			if !a.ResolvedAt.Valid || a.ResolveReason.String != "mains_ok" || !a.ResolvedAt.Time.After(end) || a.ResolvedAt.Time.After(end.Add(16*time.Minute)) {
				t.Errorf("home %d MAINS_LOST resolution = %v %q (outage ended %v)", home, a.ResolvedAt, a.ResolveReason.String, end)
			}
		case floatHigh:
			if !o.homes[floatHigh][home] {
				t.Errorf("home %d FLOAT_HIGH without a float-high event", home)
			}
		case outageRisk:
			if !affected[home] {
				t.Errorf("home %d OUTAGE_RISK outside the outage", home)
			}
			if a.RaisedAt.Before(start) || a.RaisedAt.After(end) {
				t.Errorf("home %d OUTAGE_RISK raised at %v outside [%v, %v]", home, a.RaisedAt, start, end)
			}
		}
	}
	for h := range affected {
		if !seen[h] {
			t.Errorf("affected home %d never got MAINS_LOST", h)
		}
	}
	// Every float-high home per the oracle has the alert.
	for h := range o.homes[floatHigh] {
		found := false
		for _, a := range got {
			if s.byDev[a.DeviceID].Index == h && a.Code == int16(floatHigh) {
				found = true
			}
		}
		if !found {
			t.Errorf("home %d emitted float-high but has no alert", h)
		}
	}
	// Outage risk must fire for at least one affected home without a backup pump.
	risk := 0
	for _, a := range got {
		if a.Code == int16(outageRisk) {
			risk++
		}
	}
	if risk == 0 {
		t.Error("no OUTAGE_RISK raised during a 3 h outage in a storm")
	}
	t.Logf("outage: %d affected homes, %d alerts, %d outage-risk", len(affected), len(got), risk)
}

func sameSet(a, b map[int]bool) bool {
	if len(a) != len(b) {
		return false
	}
	for k := range a {
		if !b[k] {
			return false
		}
	}
	return true
}

func keys(m map[int]bool) []string {
	var out []string
	for k := range m {
		out = append(out, strconv.Itoa(k))
	}
	sort.Strings(out)
	return out
}

// logDiagnostics prints what the detector and alerts produced, per home.
func (s *stack) logDiagnostics(t *testing.T, got []sqlcgen.Alert) {
	t.Helper()
	rows, err := s.st.Pool().Query(context.Background(), `SELECT device_id, code, action, count(*) FROM detections GROUP BY 1, 2, 3 ORDER BY 1, 2, 3`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var dev string
		var code, action int16
		var n int64
		_ = rows.Scan(&dev, &code, &action, &n)
		t.Logf("detection home %d %s action %d ×%d", s.byDev[dev].Index, alerts.CodeName(alertsv1.AlertCode(code)), action, n)
	}
	rows.Close()
	for _, a := range got {
		t.Logf("alert home %d %s source=%s raised=%v resolved=%v notify=%s", s.byDev[a.DeviceID].Index, alerts.CodeName(alertsv1.AlertCode(a.Code)), a.Source, a.RaisedAt.Format(time.RFC3339), a.ResolvedAt.Time.Format(time.RFC3339), a.NotifyState)
	}
}
