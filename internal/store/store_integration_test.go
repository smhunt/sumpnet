//go:build integration

package store

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/smhunt/sumpnet/internal/store/sqlcgen"
	"github.com/smhunt/sumpnet/internal/testinfra"
)

func newStore(t *testing.T) (*Store, string) {
	t.Helper()
	ctx := context.Background()
	dsn := testinfra.StartPostgres(t)
	if err := Migrate(ctx, dsn); err != nil {
		t.Fatal(err)
	}
	s, err := New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	return s, dsn
}

func sampleReadings(dev string, base time.Time, n int) []Reading {
	rssi, snr, sf, gw := int16(-97), float32(6.5), int16(8), "a84041ffff1e0001"
	out := make([]Reading, n)
	for i := range out {
		id := uuid.NewSHA1(uuid.NameSpaceOID, []byte(fmt.Sprintf("%s-%d", dev, i)))
		out[i] = Reading{
			DeviceID: dev, TS: base.Add(time.Duration(i) * 15 * time.Minute), FCnt: int64(i),
			LevelMM: 400 + int32(i), TempC: 18.5, RHPct: 55, BattMV: 4100, MainsOK: true,
			Meta: Meta{RSSIDBm: &rssi, SNRDb: &snr, SF: &sf, GatewayID: &gw, DedupID: &id},
		}
	}
	return out
}

func TestMigrateCreatesPartitions(t *testing.T) {
	s, dsn := newStore(t)
	ctx := context.Background()
	if err := s.CheckSchema(ctx); err != nil {
		t.Fatalf("CheckSchema: %v", err)
	}
	var n int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM partman.part_config WHERE parent_table IN ('public.readings','public.cycle_events')`).Scan(&n); err != nil || n != 2 {
		t.Fatalf("part_config rows = %d (%v), want 2", n, err)
	}
	var exists bool
	if err := s.pool.QueryRow(ctx, `SELECT to_regclass('public.readings_p20260401') IS NOT NULL`).Scan(&exists); err != nil || !exists {
		t.Fatalf("readings_p20260401 missing (%v)", err)
	}
	if err := s.pool.QueryRow(ctx, `SELECT to_regclass('public.readings_default') IS NOT NULL`).Scan(&exists); err != nil || !exists {
		t.Fatalf("readings_default missing (%v)", err)
	}

	// Down and back up must be clean.
	s.Close()
	if err := MigrateDown(ctx, dsn); err != nil {
		t.Fatalf("down: %v", err)
	}
	if err := Migrate(ctx, dsn); err != nil {
		t.Fatalf("up again: %v", err)
	}
	s2, err := New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	if err := s2.CheckSchema(ctx); err != nil {
		t.Fatalf("CheckSchema after round trip: %v", err)
	}
}

func TestColumnListsMatchSchema(t *testing.T) {
	s, _ := newStore(t)
	ctx := context.Background()
	for _, tb := range allTables {
		rows, err := s.pool.Query(ctx, `SELECT column_name FROM information_schema.columns WHERE table_schema='public' AND table_name=$1 AND column_name <> 'inserted_at' AND column_name <> 'est_volume_l' ORDER BY ordinal_position`, tb.name)
		if err != nil {
			t.Fatal(err)
		}
		var db []string
		for rows.Next() {
			var c string
			_ = rows.Scan(&c)
			db = append(db, c)
		}
		rows.Close()
		if strings.Join(db, ",") != strings.Join(tb.cols, ",") {
			t.Errorf("%s: Go cols\n  %s\nschema cols\n  %s", tb.name, strings.Join(tb.cols, ","), strings.Join(db, ","))
		}
	}
}

func TestInsertIdempotent(t *testing.T) {
	s, _ := newStore(t)
	ctx := context.Background()
	base := time.Date(2026, 4, 15, 0, 0, 0, 0, time.UTC)
	rows := sampleReadings("70b3d57ed0000001", base, 10)

	res, err := s.InsertReadings(ctx, rows)
	if err != nil {
		t.Fatal(err)
	}
	if res != (Result{Accepted: 10}) {
		t.Fatalf("first insert = %+v, want 10 accepted", res)
	}
	res, err = s.InsertReadings(ctx, rows)
	if err != nil {
		t.Fatal(err)
	}
	if res != (Result{Duplicates: 10}) {
		t.Fatalf("replay = %+v, want 10 duplicates", res)
	}
	// Intra-batch duplicate + one new row.
	mixed := append(rows[:2:2], sampleReadings("70b3d57ed0000001", base.Add(48*time.Hour), 1)...)
	res, err = s.InsertReadings(ctx, mixed)
	if err != nil {
		t.Fatal(err)
	}
	if res != (Result{Accepted: 1, Duplicates: 2}) {
		t.Fatalf("mixed = %+v, want 1 accepted 2 dup", res)
	}

	// Auto-registered device with home_id NULL and last_seen_at = newest ts.
	d, err := s.Queries().GetDevice(ctx, "70b3d57ed0000001")
	if err != nil {
		t.Fatal(err)
	}
	if d.HomeID.Valid || !d.LastSeenAt.Valid || !d.LastSeenAt.Time.Equal(base.Add(48*time.Hour)) {
		t.Errorf("device = %+v", d)
	}
	n, _ := s.Queries().CountReadings(ctx)
	if n != 11 {
		t.Errorf("readings = %d, want 11", n)
	}
}

func TestOtherTables(t *testing.T) {
	s, _ := newStore(t)
	ctx := context.Background()
	base := time.Date(2026, 4, 15, 6, 0, 0, 0, time.UTC)
	dev := "70b3d57ed0000002"

	ce := []CycleEvent{{DeviceID: dev, StartedAt: base, FCnt: 5, ReceivedAt: base.Add(20 * time.Second), RunS: 18, PeakCurrentA: 6.3, LevelStartMM: 420, LevelEndMM: 610, PumpID: "primary"}}
	if res, err := s.InsertCycleEvents(ctx, ce); err != nil || res.Accepted != 1 {
		t.Fatalf("cycle: %+v %v", res, err)
	}
	if res, err := s.InsertCycleEvents(ctx, ce); err != nil || res.Duplicates != 1 {
		t.Fatalf("cycle replay: %+v %v", res, err)
	}
	ss := []StormSummary{{DeviceID: dev, WindowEnd: base, FCnt: 6, WindowS: 900, CycleCount: 12, TotalRunS: 216, MaxPeakCurrentA: 7.1, MinLevelMM: 380}}
	if res, err := s.InsertStormSummaries(ctx, ss); err != nil || res.Accepted != 1 {
		t.Fatalf("summary: %+v %v", res, err)
	}
	ae := []AlarmEvent{{DeviceID: dev, RaisedAt: base, FCnt: 7, Code: 2, Value: 500}}
	if res, err := s.InsertAlarmEvents(ctx, ae); err != nil || res.Accepted != 1 {
		t.Fatalf("alarm: %+v %v", res, err)
	}
	got, err := s.Queries().GetCycleEvent(ctx, sqlcgen.GetCycleEventParams{DeviceID: dev, FCnt: 5})
	if err != nil || got.PumpID != "primary" || got.RunS != 18 {
		t.Fatalf("GetCycleEvent = %+v %v", got, err)
	}
	stats, err := s.Queries().FCntStatsByDevice(ctx)
	if err != nil || len(stats) != 1 || stats[0].Rows != 3 || stats[0].MaxFCnt != 7 {
		t.Fatalf("FCntStatsByDevice = %+v %v", stats, err)
	}
}

func TestPartitionSafetyNet(t *testing.T) {
	s, _ := newStore(t)
	ctx := context.Background()
	var created []string
	s.OnPartitionCreated = func(tb string) { created = append(created, tb) }

	old := sampleReadings("70b3d57ed0000003", time.Date(2025, 3, 10, 0, 0, 0, 0, time.UTC), 3)
	if _, err := s.InsertReadings(ctx, old); err != nil {
		t.Fatal(err)
	}
	var exists bool
	if err := s.pool.QueryRow(ctx, `SELECT to_regclass('public.readings_p20250301') IS NOT NULL`).Scan(&exists); err != nil || !exists {
		t.Fatalf("partition for 2025-03 not created (%v)", err)
	}
	var inDefault int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM readings_default`).Scan(&inDefault); err != nil || inDefault != 0 {
		t.Fatalf("rows in default partition = %d (%v)", inDefault, err)
	}
	if len(created) != 1 {
		t.Errorf("OnPartitionCreated calls = %v, want 1", created)
	}
	// Second batch for the same month: no DDL, no callback.
	if _, err := s.InsertReadings(ctx, sampleReadings("70b3d57ed0000004", time.Date(2025, 3, 20, 0, 0, 0, 0, time.UTC), 2)); err != nil {
		t.Fatal(err)
	}
	if len(created) != 1 {
		t.Errorf("OnPartitionCreated calls after second batch = %v", created)
	}
}

func TestAutoRegisterOff(t *testing.T) {
	ctx := context.Background()
	dsn := testinfra.StartPostgres(t)
	if err := Migrate(ctx, dsn); err != nil {
		t.Fatal(err)
	}
	s, err := New(ctx, dsn, WithAutoRegister(false))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	base := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	res, err := s.InsertReadings(ctx, sampleReadings("70b3d57ed0000005", base, 4))
	if err != nil {
		t.Fatal(err)
	}
	if res != (Result{Rejected: 4}) {
		t.Fatalf("unknown device = %+v, want 4 rejected", res)
	}
	if _, uerr := s.Queries().UpsertDevice(ctx, sqlcgen.UpsertDeviceParams{DevEui: "70b3d57ed0000005", Kind: "house"}); uerr != nil {
		t.Fatal(uerr)
	}
	res, err = s.InsertReadings(ctx, sampleReadings("70b3d57ed0000005", base, 4))
	if err != nil {
		t.Fatal(err)
	}
	if res != (Result{Accepted: 4}) {
		t.Fatalf("known device = %+v, want 4 accepted", res)
	}
}

func TestNotify(t *testing.T) {
	s, dsn := newStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	if _, lerr := conn.Exec(ctx, "LISTEN "+NotifyChannel); lerr != nil {
		t.Fatal(lerr)
	}
	_ = dsn
	if _, ierr := s.InsertAlarmEvents(ctx, []AlarmEvent{{DeviceID: "70b3d57ed0000006", RaisedAt: time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC), FCnt: 1, Code: 1, Value: 350}}); ierr != nil {
		t.Fatal(ierr)
	}
	n, err := conn.Conn().WaitForNotification(ctx)
	if err != nil {
		t.Fatalf("no notification: %v", err)
	}
	if n.Channel != NotifyChannel || !strings.Contains(n.Payload, `"table":"alarm_events"`) || !strings.Contains(n.Payload, `"n":1`) {
		t.Errorf("notification = %+v", n)
	}
}

func TestRainGaugeUplinks(t *testing.T) {
	s, _ := newStore(t)
	ctx := context.Background()
	dev := "70b3d57ed1000000"
	base := time.Date(2026, 4, 15, 6, 0, 0, 0, time.UTC)
	rssi := int16(-80)
	rows := []RainGaugeUplink{
		{DeviceID: dev, TS: base, FCnt: 0, TipCount: 0, MMPerTip: 0.2, IntervalS: 137, BattMV: 3600, CounterReset: true, Meta: Meta{RSSIDBm: &rssi}},
		{DeviceID: dev, TS: base.Add(5 * time.Minute), FCnt: 1, TipCount: 7, MMPerTip: 0.254, IntervalS: 300, BattMV: 3598},
		{DeviceID: dev, TS: time.Date(2025, 11, 2, 0, 0, 0, 0, time.UTC), FCnt: 9, TipCount: 1, MMPerTip: 0.2, IntervalS: 900, SensorFault: true},
	}
	if res, err := s.InsertRainGaugeUplinks(ctx, rows); err != nil || res != (Result{Accepted: 3}) {
		t.Fatalf("insert: %+v %v", res, err)
	}
	if res, err := s.InsertRainGaugeUplinks(ctx, rows); err != nil || res != (Result{Duplicates: 3}) {
		t.Fatalf("replay: %+v %v", res, err)
	}
	// Auto-registered as a rain gauge, never as a house node.
	d, err := s.Queries().GetDevice(ctx, dev)
	if err != nil || d.Kind != "rain" || d.HomeID.Valid {
		t.Fatalf("device = %+v %v", d, err)
	}
	got, err := s.Queries().ListRainGaugeUplinks(ctx, sqlcgen.ListRainGaugeUplinksParams{DeviceID: dev, FromTs: base, ToTs: base.Add(time.Hour)})
	if err != nil || len(got) != 2 {
		t.Fatalf("list = %+v %v", got, err)
	}
	if got[0].MmPerTip != 0.2 || got[1].MmPerTip != 0.254 || got[1].TipCount != 7 || !got[0].CounterReset || got[0].RssiDbm.Int16 != -80 || got[1].IntervalS != 300 {
		t.Errorf("rows = %+v", got)
	}
	// The 2025 replay landed in its own partition, not the default one.
	var inDefault int
	if qerr := s.pool.QueryRow(ctx, `SELECT count(*) FROM rain_gauge_uplinks_default`).Scan(&inDefault); qerr != nil || inDefault != 0 {
		t.Fatalf("rows in default partition = %d (%v)", inDefault, qerr)
	}
	prev, err := s.Queries().RainGaugeUplinkBefore(ctx, sqlcgen.RainGaugeUplinkBeforeParams{DeviceID: dev, Ts: base.Add(5 * time.Minute)})
	if err != nil || prev.FCnt != 0 {
		t.Errorf("before = %+v %v", prev, err)
	}
	next, err := s.Queries().RainGaugeUplinkAfter(ctx, sqlcgen.RainGaugeUplinkAfterParams{DeviceID: dev, Ts: base})
	if err != nil || next.FCnt != 1 {
		t.Errorf("after = %+v %v", next, err)
	}
	stats, err := s.Queries().FCntStatsByDevice(ctx)
	if err != nil || len(stats) != 1 || stats[0].Rows != 3 {
		t.Errorf("fcnt stats = %+v %v", stats, err)
	}
}
