//go:build integration

package detector

import (
	"context"
	"log/slog"
	"math"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"

	alertsv1 "github.com/smhunt/sumpnet/gen/go/sumpnet/alerts/v1"
	"github.com/smhunt/sumpnet/internal/store"
	"github.com/smhunt/sumpnet/internal/store/sqlcgen"
	"github.com/smhunt/sumpnet/internal/testinfra"
	"github.com/smhunt/sumpnet/internal/watermark"
)

var base = time.Date(2026, 4, 15, 6, 0, 0, 0, time.UTC)

const (
	linked   = "70b3d57ed0000010"
	unlinked = "70b3d57ed0000011"
)

func setup(t *testing.T) (*store.Store, *watermark.Consumer) {
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
	q := st.Queries()
	if err := q.UpsertSegment(ctx, sqlcgen.UpsertSegmentParams{ID: "seg-01", Name: "Segment 01"}); err != nil {
		t.Fatal(err)
	}
	homeID := uuid.New()
	if _, err := q.UpsertHome(ctx, sqlcgen.UpsertHomeParams{ID: homeID, SegmentID: pgText("seg-01"), PitAreaM2: nullFloat(0.164)}); err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{linked, unlinked} {
		if _, err := q.UpsertDevice(ctx, sqlcgen.UpsertDeviceParams{DevEui: d, Kind: "house"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := q.LinkDevice(ctx, sqlcgen.LinkDeviceParams{DevEui: linked, HomeID: uuid.NullUUID{UUID: homeID, Valid: true}}); err != nil {
		t.Fatal(err)
	}
	cfg := Config{Watermark: watermark.Config{Consumer: "cycle-detector", Lag: 0, MaxGroups: 50, PollInterval: time.Hour}, HealthyCyclesToClear: 3, Window: 10, PitAreaTTL: time.Minute}
	reg := prometheus.NewRegistry()
	c := watermark.New(st.Pool(), dsn, cfg.Watermark, watermark.NewMetrics(reg), slog.New(slog.DiscardHandler))
	AddStages(c, NewHandler(cfg, NewMetrics(reg), slog.New(slog.DiscardHandler)))
	return st, c
}

func cycle(dev string, at time.Duration, fcnt int64, runS, start, end int32) store.CycleEvent {
	started := base.Add(at)
	return store.CycleEvent{DeviceID: dev, StartedAt: started, FCnt: fcnt, ReceivedAt: started.Add(time.Duration(runS) * time.Second), RunS: runS, PeakCurrentA: 6, LevelStartMM: start, LevelEndMM: end, PumpID: "primary"}
}

type det struct {
	code   alertsv1.AlertCode
	action int16
	at     time.Time
}

func detections(t *testing.T, st *store.Store, dev string) []det {
	t.Helper()
	rows, err := st.Queries().ListDetections(context.Background(), dev)
	if err != nil {
		t.Fatal(err)
	}
	out := make([]det, len(rows))
	for i, r := range rows {
		out[i] = det{alertsv1.AlertCode(r.Code), r.Action, r.ObservedAt}
	}
	return out
}

func TestVolumesAndDetections(t *testing.T) {
	st, c := setup(t)
	ctx := context.Background()

	// Linked device: two healthy cycles, then a dry run (45 s, 2 mm), a 700 s
	// continuous run (which moves water, so it is the first healthy cycle after
	// the dry run), then healthy cycles; the dry run clears after the third
	// healthy one (cycle 6).
	rows := []store.CycleEvent{
		cycle(linked, 0, 1, 18, 420, 570),
		cycle(linked, 30*time.Minute, 2, 18, 420, 570),
		cycle(linked, 60*time.Minute, 3, 45, 420, 422),
		cycle(linked, 90*time.Minute, 4, 700, 380, 600),
		cycle(linked, 120*time.Minute, 5, 18, 420, 570),
		cycle(linked, 150*time.Minute, 6, 18, 420, 570),
		cycle(linked, 180*time.Minute, 7, 18, 420, 570),
	}
	if _, err := st.InsertCycleEvents(ctx, rows); err != nil {
		t.Fatal(err)
	}
	// Unlinked device: one cycle, no pit area → no volume.
	if _, err := st.InsertCycleEvents(ctx, []store.CycleEvent{cycle(unlinked, 0, 1, 18, 420, 570)}); err != nil {
		t.Fatal(err)
	}
	if n, err := c.RunOnce(ctx); err != nil || n != 8 {
		t.Fatalf("round: %d rows, %v", n, err)
	}

	got, err := st.Queries().GetCycleEvent(ctx, sqlcgen.GetCycleEventParams{DeviceID: linked, FCnt: 1})
	if err != nil || !got.EstVolumeL.Valid || math.Abs(float64(got.EstVolumeL.Float32)-0.164*150) > 0.01 {
		t.Fatalf("linked volume = %+v (%v), want 24.6 L", got.EstVolumeL, err)
	}
	if u, err := st.Queries().GetCycleEvent(ctx, sqlcgen.GetCycleEventParams{DeviceID: unlinked, FCnt: 1}); err != nil || u.EstVolumeL.Valid {
		t.Fatalf("unlinked volume = %+v (%v), want NULL", u.EstVolumeL, err)
	}

	want := []det{
		{alertsv1.AlertCode_ALERT_CODE_DRY_RUN, ActionRaise, base.Add(60*time.Minute + 45*time.Second)},
		{alertsv1.AlertCode_ALERT_CODE_CONTINUOUS_RUN, ActionRaise, base.Add(100 * time.Minute)},
		{alertsv1.AlertCode_ALERT_CODE_CONTINUOUS_RUN, ActionClear, base.Add(90*time.Minute + 700*time.Second)},
		{alertsv1.AlertCode_ALERT_CODE_DRY_RUN, ActionClear, base.Add(150*time.Minute + 18*time.Second)},
	}
	got2 := detections(t, st, linked)
	if len(got2) != len(want) {
		t.Fatalf("detections = %+v, want %+v", got2, want)
	}
	for i := range want {
		if got2[i].code != want[i].code || got2[i].action != want[i].action || !got2[i].at.Equal(want[i].at) {
			t.Errorf("detection %d = %+v, want %+v", i, got2[i], want[i])
		}
	}
	if d := detections(t, st, unlinked); len(d) != 0 {
		t.Errorf("unlinked healthy device produced detections: %+v", d)
	}

	// Backfill after linking: the unlinked device gets a home with a 24" basin.
	homeID := uuid.New()
	if _, err := st.Queries().UpsertHome(ctx, sqlcgen.UpsertHomeParams{ID: homeID, SegmentID: pgText("seg-01"), PitAreaM2: nullFloat(0.29)}); err != nil {
		t.Fatal(err)
	}
	if err := st.Queries().LinkDevice(ctx, sqlcgen.LinkDeviceParams{DevEui: unlinked, HomeID: uuid.NullUUID{UUID: homeID, Valid: true}}); err != nil {
		t.Fatal(err)
	}
	if n, err := st.Queries().RecomputeCycleVolumes(ctx, unlinked); err != nil || n != 1 {
		t.Fatalf("recompute = %d, %v", n, err)
	}
	if u, _ := st.Queries().GetCycleEvent(ctx, sqlcgen.GetCycleEventParams{DeviceID: unlinked, FCnt: 1}); !u.EstVolumeL.Valid || math.Abs(float64(u.EstVolumeL.Float32)-0.29*150) > 0.01 {
		t.Fatalf("backfilled volume = %+v, want 43.5 L", u.EstVolumeL)
	}
}

func TestShortCycling(t *testing.T) {
	st, c := setup(t)
	ctx := context.Background()
	// Six cycles 10 s long with 20 s gaps: short cycling from the 5th; then a
	// long pause and three well-spaced cycles clear it.
	var rows []store.CycleEvent
	at := time.Duration(0)
	for i := int64(1); i <= 6; i++ {
		rows = append(rows, cycle(linked, at, i, 10, 420, 445))
		at += 30 * time.Second
	}
	at += time.Hour
	for i := int64(7); i <= 9; i++ {
		rows = append(rows, cycle(linked, at, i, 10, 420, 445))
		at += 30 * time.Minute
	}
	if _, err := st.InsertCycleEvents(ctx, rows); err != nil {
		t.Fatal(err)
	}
	if _, err := c.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	got := detections(t, st, linked)
	fifthEnd := base.Add(4*30*time.Second + 10*time.Second)
	ninthEnd := base.Add(6*30*time.Second + time.Hour + 2*30*time.Minute + 10*time.Second)
	want := []det{
		{alertsv1.AlertCode_ALERT_CODE_SHORT_CYCLING, ActionRaise, fifthEnd},
		{alertsv1.AlertCode_ALERT_CODE_SHORT_CYCLING, ActionClear, ninthEnd},
	}
	if len(got) != 2 || !sameDet(got[0], want[0]) || !sameDet(got[1], want[1]) {
		t.Fatalf("detections = %+v, want %+v", got, want)
	}
	// A second round (new consumer, seeded from the DB) must not re-raise.
	if n, err := c.RunOnce(ctx); err != nil || n != 0 {
		t.Fatalf("second round: %d, %v", n, err)
	}
}

func sameDet(a, b det) bool { return a.code == b.code && a.action == b.action && a.at.Equal(b.at) }

func TestStormSummaryShortCycling(t *testing.T) {
	st, c := setup(t)
	ctx := context.Background()
	// Storm mode: 40 cycles in a 900 s window is a mean interval of 22 s.
	// Two windows raise once; three quiet windows clear.
	var rows []store.StormSummary
	counts := []int32{40, 37, 8, 6, 3}
	for i, n := range counts {
		rows = append(rows, store.StormSummary{DeviceID: linked, WindowEnd: base.Add(time.Duration(i+1) * 15 * time.Minute), FCnt: int64(100 + i), WindowS: 900, CycleCount: n, TotalRunS: n * 4, MaxPeakCurrentA: 6, MinLevelMM: 300})
	}
	if _, err := st.InsertStormSummaries(ctx, rows); err != nil {
		t.Fatal(err)
	}
	if _, err := c.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	got := detections(t, st, linked)
	want := []det{
		{alertsv1.AlertCode_ALERT_CODE_SHORT_CYCLING, ActionRaise, base.Add(15 * time.Minute)},
		{alertsv1.AlertCode_ALERT_CODE_SHORT_CYCLING, ActionClear, base.Add(75 * time.Minute)},
	}
	if len(got) != 2 || !sameDet(got[0], want[0]) || !sameDet(got[1], want[1]) {
		t.Fatalf("detections = %+v, want %+v", got, want)
	}
}
