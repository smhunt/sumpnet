//go:build integration

package alerts

import (
	"context"
	"database/sql"
	"log/slog"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/prometheus/client_golang/prometheus"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"

	alertsv1 "github.com/smhunt/sumpnet/gen/go/sumpnet/alerts/v1"
	"github.com/smhunt/sumpnet/internal/detector"
	"github.com/smhunt/sumpnet/internal/store"
	"github.com/smhunt/sumpnet/internal/store/sqlcgen"
	"github.com/smhunt/sumpnet/internal/testinfra"
	"github.com/smhunt/sumpnet/internal/watermark"
)

var (
	base   = time.Date(2026, 4, 15, 6, 0, 0, 0, time.UTC)
	devA   = "70b3d57ed0000020"
	devB   = "70b3d57ed0000021"
	homeA  = uuid.MustParse("aaaaaaaa-0000-4000-8000-000000000001")
	rssi16 = int16(-80)
)

type harness struct {
	st  *store.Store
	e   *Engine
	c   *watermark.Consumer
	cfg Config
}

func setup(t *testing.T, mutate func(*Config)) *harness {
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
	if _, err := q.UpsertHome(ctx, sqlcgen.UpsertHomeParams{ID: homeA, SegmentID: pgtype.Text{String: "seg-01", Valid: true}, PitAreaM2: sql.NullFloat64{Float64: 0.164, Valid: true}}); err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{devA, devB} {
		if _, err := q.UpsertDevice(ctx, sqlcgen.UpsertDeviceParams{DevEui: d, Kind: "house"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := q.LinkDevice(ctx, sqlcgen.LinkDeviceParams{DevEui: devA, HomeID: uuid.NullUUID{UUID: homeA, Valid: true}}); err != nil {
		t.Fatal(err)
	}
	cfg := DefaultConfig()
	cfg.Watermark = watermark.Config{Consumer: "alerts", Lag: 0, MaxGroups: 50, PollInterval: time.Hour}
	cfg.OfflineAfter = 0
	if mutate != nil {
		mutate(&cfg)
	}
	reg := prometheus.NewRegistry()
	e := NewEngine(cfg, NewMetrics(reg), slog.New(slog.DiscardHandler))
	c := watermark.New(st.Pool(), dsn, cfg.Watermark, watermark.NewMetrics(reg), slog.New(slog.DiscardHandler))
	AddStages(c, e)
	return &harness{st: st, e: e, c: c, cfg: cfg}
}

func (h *harness) round(t *testing.T) {
	t.Helper()
	for {
		n, err := h.c.RunOnce(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if n == 0 {
			return
		}
	}
}

func (h *harness) alerts(t *testing.T, dev string) []sqlcgen.Alert {
	t.Helper()
	rows, err := h.st.Queries().ListAlertsForDevice(context.Background(), dev)
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

func reading(dev string, at time.Duration, fcnt int64, level int32, mains, floatHigh bool, batt int32) store.Reading {
	return store.Reading{DeviceID: dev, TS: base.Add(at), FCnt: fcnt, LevelMM: level, TempC: 18, RHPct: 50, BattMV: batt, MainsOK: mains, FloatHigh: floatHigh, Meta: store.Meta{RSSIDBm: &rssi16}}
}

func TestNodeAlarmsAndLifecycle(t *testing.T) {
	h := setup(t, nil)
	ctx := context.Background()

	// Two float-high alarms 10 min apart: one open alert, touched once.
	if _, err := h.st.InsertAlarmEvents(ctx, []store.AlarmEvent{
		{DeviceID: devA, RaisedAt: base, FCnt: 1, Code: 1, Value: 350},
		{DeviceID: devA, RaisedAt: base.Add(10 * time.Minute), FCnt: 2, Code: 1, Value: 340},
	}); err != nil {
		t.Fatal(err)
	}
	h.round(t)
	al := h.alerts(t, devA)
	if len(al) != 1 || al[0].Code != 1 || al[0].Occurrences != 2 || !al[0].RaisedAt.Equal(base) || al[0].Severity != 3 || al[0].NotifyState != StatePending || !al[0].HomeID.Valid {
		t.Fatalf("alerts = %+v", al)
	}

	// A heartbeat with the float low resolves it (event time); a later float-high raises a NEW row.
	if _, err := h.st.InsertReadings(ctx, []store.Reading{reading(devA, 20*time.Minute, 3, 500, true, false, 4100)}); err != nil {
		t.Fatal(err)
	}
	h.round(t)
	al = h.alerts(t, devA)
	if len(al) != 1 || !al[0].ResolvedAt.Valid || !al[0].ResolvedAt.Time.Equal(base.Add(20*time.Minute)) || al[0].ResolveReason.String != "float low" {
		t.Fatalf("after float low: %+v", al)
	}
	if _, err := h.st.InsertReadings(ctx, []store.Reading{reading(devA, 40*time.Minute, 4, 300, true, true, 4100)}); err != nil {
		t.Fatal(err)
	}
	h.round(t)
	al = h.alerts(t, devA)
	if len(al) != 2 || al[1].Source != "heartbeat" || al[1].ResolvedAt.Valid {
		t.Fatalf("re-raise: %+v", al)
	}
	// Cooldown: the second raise within an hour of a *sent* first one is suppressed;
	// here nothing was sent yet, so it is pending.
	if al[1].NotifyState != StatePending {
		t.Fatalf("second raise notify_state = %s", al[1].NotifyState)
	}

	// gRPC: list by home, acknowledge (idempotent), not found.
	srv := grpc.NewServer()
	alertsv1.RegisterAlertServiceServer(srv, NewServer(h.st.Queries()))
	lis, _ := net.Listen("tcp", "127.0.0.1:0")
	go func() { _ = srv.Serve(lis) }()
	defer srv.Stop()
	conn, err := grpc.NewClient(lis.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	client := alertsv1.NewAlertServiceClient(conn)
	list, err := client.ListActiveAlerts(ctx, &alertsv1.ListActiveAlertsRequest{HomeId: homeA.String()})
	if err != nil || len(list.GetAlerts()) != 1 || list.GetAlerts()[0].GetCode() != alertsv1.AlertCode_ALERT_CODE_FLOAT_HIGH || list.GetAlerts()[0].GetDeviceId() != devA || list.GetAlerts()[0].GetSegmentId() != "seg-01" {
		t.Fatalf("ListActiveAlerts(home) = %v, %v", list, err)
	}
	if _, berr := client.ListActiveAlerts(ctx, &alertsv1.ListActiveAlertsRequest{HomeId: homeA.String(), SegmentId: "seg-01"}); status.Code(berr) != codes.InvalidArgument {
		t.Fatalf("both filters: %v", berr)
	}
	id := list.GetAlerts()[0].GetId()
	ack1, err := client.Acknowledge(ctx, &alertsv1.AcknowledgeRequest{AlertId: id, Note: "on it"})
	if err != nil || ack1.GetAlert().GetAckedAt() == nil {
		t.Fatalf("ack: %v %v", ack1, err)
	}
	ack2, err := client.Acknowledge(ctx, &alertsv1.AcknowledgeRequest{AlertId: id, Note: "again"})
	if err != nil || !ack2.GetAlert().GetAckedAt().AsTime().Equal(ack1.GetAlert().GetAckedAt().AsTime()) {
		t.Fatalf("ack must be idempotent: %v %v", ack2, err)
	}
	if _, nerr := client.Acknowledge(ctx, &alertsv1.AcknowledgeRequest{AlertId: uuid.New().String()}); status.Code(nerr) != codes.NotFound {
		t.Fatalf("unknown id: %v", nerr)
	}
	seg, err := client.ListActiveAlerts(ctx, &alertsv1.ListActiveAlertsRequest{SegmentId: "seg-01"})
	if err != nil || len(seg.GetAlerts()) != 1 {
		t.Fatalf("ListActiveAlerts(segment) = %v, %v", seg, err)
	}
}

func TestOutageRiskAndBattery(t *testing.T) {
	h := setup(t, nil)
	ctx := context.Background()
	rows := []store.Reading{
		reading(devB, 0, 1, 500, true, false, 4100),
		reading(devB, 15*time.Minute, 2, 500, false, false, 4000), // mains lost
		reading(devB, 30*time.Minute, 3, 490, false, false, 3900),
		reading(devB, 45*time.Minute, 4, 470, false, false, 3400), // rising 30 mm over 3 readings + low battery
		reading(devB, 60*time.Minute, 5, 480, false, false, 3400), // level falling
		reading(devB, 75*time.Minute, 6, 495, false, false, 3400), // falling again → outage risk resolves
		reading(devB, 90*time.Minute, 7, 500, true, false, 3800),  // mains back, battery recovered
	}
	if _, err := h.st.InsertReadings(ctx, rows); err != nil {
		t.Fatal(err)
	}
	h.round(t)
	byCode := map[int16]sqlcgen.Alert{}
	for _, a := range h.alerts(t, devB) {
		byCode[a.Code] = a
	}
	ml, ok := byCode[int16(alertsv1.AlertCode_ALERT_CODE_MAINS_LOST)]
	if !ok || !ml.RaisedAt.Equal(base.Add(15*time.Minute)) || !ml.ResolvedAt.Valid || !ml.ResolvedAt.Time.Equal(base.Add(90*time.Minute)) || ml.ResolveReason.String != "mains_ok" || ml.HomeID.Valid {
		t.Fatalf("MAINS_LOST = %+v (unlinked device → home_id NULL)", ml)
	}
	or, ok := byCode[int16(alertsv1.AlertCode_ALERT_CODE_OUTAGE_RISK)]
	if !ok || !or.RaisedAt.Equal(base.Add(45*time.Minute)) || or.Severity != 3 || !or.ResolvedAt.Valid || !or.ResolvedAt.Time.Equal(base.Add(75*time.Minute)) || or.ResolveReason.String != "level falling" {
		t.Fatalf("OUTAGE_RISK = %+v", or)
	}
	if !strings.Contains(or.Message, "Phase 4") {
		t.Errorf("message should mention the deferred rain check: %q", or.Message)
	}
	lb, ok := byCode[int16(alertsv1.AlertCode_ALERT_CODE_LOW_BATTERY)]
	if !ok || !lb.RaisedAt.Equal(base.Add(45*time.Minute)) || !lb.ResolvedAt.Valid || lb.ResolveReason.String != "battery recovered" {
		t.Fatalf("LOW_BATTERY = %+v", lb)
	}
	if len(byCode) != 3 {
		t.Fatalf("unexpected alerts: %+v", byCode)
	}
}

func TestDetectionsAndNotifierLoop(t *testing.T) {
	h := setup(t, nil)
	ctx := context.Background()
	q := h.st.Queries()
	for _, d := range []sqlcgen.InsertDetectionParams{
		{DeviceID: devA, Code: int16(alertsv1.AlertCode_ALERT_CODE_DRY_RUN), Action: detector.ActionRaise, ObservedAt: base, FCnt: 1},
		{DeviceID: devA, Code: int16(alertsv1.AlertCode_ALERT_CODE_SHORT_CYCLING), Action: detector.ActionRaise, ObservedAt: base.Add(time.Minute), FCnt: 2},
		{DeviceID: devA, Code: int16(alertsv1.AlertCode_ALERT_CODE_DRY_RUN), Action: detector.ActionClear, ObservedAt: base.Add(time.Hour), FCnt: 9},
	} {
		if _, err := q.InsertDetection(ctx, d); err != nil {
			t.Fatal(err)
		}
	}
	h.round(t)
	al := h.alerts(t, devA)
	if len(al) != 2 || al[0].Code != int16(alertsv1.AlertCode_ALERT_CODE_DRY_RUN) || !al[0].ResolvedAt.Valid || al[0].ResolveReason.String != "detector clear" || al[1].ResolvedAt.Valid {
		t.Fatalf("alerts = %+v", al)
	}

	// Notifier: the CRITICAL dry-run raise and its resolve, the WARNING short-cycling raise (resolve not notified).
	rec := &RecordingNotifier{}
	loop := &NotifierLoop{Queries: q, Notifier: rec, Metrics: h.e.m, Log: slog.New(slog.DiscardHandler), Interval: time.Hour, Retry: time.Minute, MaxAttempts: 5}
	if err := loop.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	got := rec.Messages()
	if len(got) != 3 {
		t.Fatalf("notifications = %+v", got)
	}
	events := map[string]int{}
	for _, m := range got {
		events[CodeName(m.Code)+"/"+m.Event]++
		if m.HomeID == nil || m.SegmentID != "seg-01" {
			t.Errorf("message lacks home/segment: %+v", m)
		}
	}
	if events["DRY_RUN/raised"] != 1 || events["DRY_RUN/resolved"] != 1 || events["SHORT_CYCLING/raised"] != 1 {
		t.Fatalf("events = %v", events)
	}
	// A second run sends nothing: states are persisted.
	if err := loop.RunOnce(ctx); err != nil || len(rec.Messages()) != 3 {
		t.Fatalf("second run resent: %d, %v", len(rec.Messages()), err)
	}
	al = h.alerts(t, devA)
	if al[0].NotifyState != StateSent || al[0].ResolveNotifyState != StateSent || al[1].NotifyState != StateSent || al[1].ResolveNotifyState != StateNone {
		t.Fatalf("states = %+v", al)
	}

	// Cooldown: raising DRY_RUN again within an hour of the sent email is suppressed.
	if _, err := q.InsertDetection(ctx, sqlcgen.InsertDetectionParams{DeviceID: devA, Code: int16(alertsv1.AlertCode_ALERT_CODE_DRY_RUN), Action: detector.ActionRaise, ObservedAt: base.Add(2 * time.Hour), FCnt: 20}); err != nil {
		t.Fatal(err)
	}
	h.round(t)
	al = h.alerts(t, devA)
	if len(al) != 3 || al[2].NotifyState != StateSuppressed {
		t.Fatalf("cooldown: %+v", al)
	}

	// Failed delivery is retried later, not immediately.
	rec2 := &RecordingNotifier{Err: context.DeadlineExceeded}
	loop.Notifier = rec2
	if _, err := q.InsertDetection(ctx, sqlcgen.InsertDetectionParams{DeviceID: devB, Code: int16(alertsv1.AlertCode_ALERT_CODE_CONTINUOUS_RUN), Action: detector.ActionRaise, ObservedAt: base.Add(3 * time.Hour), FCnt: 30}); err != nil {
		t.Fatal(err)
	}
	h.round(t)
	_ = loop.RunOnce(ctx)
	if b := h.alerts(t, devB); len(b) != 1 || b[0].NotifyState != StateFailed || b[0].NotifyAttempts != 1 {
		t.Fatalf("failed delivery: %+v", b)
	}
	_ = loop.RunOnce(ctx)
	if b := h.alerts(t, devB); b[0].NotifyAttempts != 1 {
		t.Fatalf("retried before the retry interval: %+v", b)
	}
}

func TestMailpitDelivery(t *testing.T) {
	mp := testinfra.StartMailpit(t)
	n := &SMTPNotifier{cfg: SMTPConfig{Host: mp.SMTPHost, Port: mp.SMTPPort, From: "alerts@sumpnet.test", To: []string{"ops@sumpnet.test"}, TLS: "none", Timeout: 10 * time.Second}}
	m := Message{AlertID: uuid.New(), Event: "raised", Code: alertsv1.AlertCode_ALERT_CODE_MAINS_LOST, Severity: alertsv1.AlertSeverity_ALERT_SEVERITY_WARNING, DeviceID: devA, At: base, Body: "Heartbeat reports mains power lost"}
	if err := n.Notify(context.Background(), m); err != nil {
		t.Fatalf("Notify: %v", err)
	}
	m.Event = "resolved"
	if err := n.Notify(context.Background(), m); err != nil {
		t.Fatalf("Notify resolved: %v", err)
	}
	msgs := mp.Messages(t)
	if len(msgs) != 2 {
		t.Fatalf("mailpit has %d messages, want 2", len(msgs))
	}
	seen := map[string]bool{}
	for _, x := range msgs {
		if len(x.To) != 1 || x.To[0].Address != "ops@sumpnet.test" || !strings.Contains(x.Subject, "MAINS_LOST") {
			t.Errorf("message = %+v", x)
		}
		seen[x.MessageID] = true
	}
	if len(seen) != 2 {
		t.Errorf("message ids not distinct per event: %v", seen)
	}
	// STARTTLS mode must refuse a server that does not offer it.
	strict := &SMTPNotifier{cfg: SMTPConfig{Host: mp.SMTPHost, Port: mp.SMTPPort, From: "a@sumpnet.test", To: []string{"b@sumpnet.test"}, TLS: "starttls", Timeout: 10 * time.Second}}
	if err := strict.Notify(context.Background(), m); err == nil || !strings.Contains(err.Error(), "STARTTLS") {
		t.Fatalf("starttls against a plaintext server: %v", err)
	}
}
