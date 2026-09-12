package gateway

import (
	"encoding/base64"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"google.golang.org/protobuf/proto"

	queryv1 "github.com/smhunt/sumpnet/gen/go/sumpnet/query/v1"
	"github.com/smhunt/sumpnet/internal/store/sqlcgen"
)

func f8(v float64) pgtype.Float8 { return pgtype.Float8{Float64: v, Valid: true} }
func hid(n byte) uuid.UUID       { return uuid.UUID{15: n} }

func TestAggregateStorm(t *testing.T) {
	rows := []sqlcgen.ListStormHomeMetricsRow{
		{SegmentID: "seg-a", HomeID: hid(1), VolumeL: 100, LagMin: f8(30)},
		{SegmentID: "seg-a", HomeID: hid(2), VolumeL: 200, LagMin: f8(50)},
		{SegmentID: "seg-b", HomeID: hid(3), VolumeL: 300, LagMin: f8(10), RecessionMin: f8(100)},
		{SegmentID: "seg-b", HomeID: hid(4), VolumeL: 600, LagMin: f8(20), RecessionMin: f8(200)},
		{SegmentID: "seg-b", HomeID: hid(5), VolumeL: 900},
		{SegmentID: "seg-x", HomeID: hid(6), VolumeL: 1},
	}
	got := aggregateStorm("s1", []string{"seg-a", "seg-b", "seg-c"}, rows)
	want := []*queryv1.SegmentStormMetrics{
		{StormId: "s1", SegmentId: "seg-a", HomesReporting: 2, Suppressed: true},
		{StormId: "s1", SegmentId: "seg-b", HomesReporting: 3, LoadLPerHome: 600, MedianLagMin: 15, MedianRecessionMin: 150},
		{StormId: "s1", SegmentId: "seg-c", Suppressed: true},
		{StormId: "s1", SegmentId: "seg-x", HomesReporting: 1, Suppressed: true},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d segments, want %d: %v", len(got), len(want), got)
	}
	for i := range want {
		if !proto.Equal(got[i], want[i]) {
			t.Errorf("segment %d = %v, want %v", i, got[i], want[i])
		}
	}
}

func TestBuildStatuses(t *testing.T) {
	act := []sqlcgen.SegmentActivityRow{
		{SegmentID: "seg-a", HomesReporting: 2, Cycles: 10},
		{SegmentID: "seg-b", HomesReporting: 3, Cycles: 12},
	}
	alerts := []sqlcgen.SegmentOpenAlertCountsRow{{SegmentID: "seg-a", ActiveAlerts: 1}, {SegmentID: "seg-b", ActiveAlerts: 2}}
	tests := []struct {
		name   string
		window time.Duration
		want   map[string]*queryv1.SegmentStatus
	}{
		{name: "1h", window: time.Hour, want: map[string]*queryv1.SegmentStatus{
			"seg-a": {SegmentId: "seg-a", HomesReporting: 2, Suppressed: true},
			"seg-b": {SegmentId: "seg-b", HomesReporting: 3, CyclesPerHour: 4, ActiveAlerts: 2},
			"seg-c": {SegmentId: "seg-c", Suppressed: true},
		}},
		{name: "30m", window: 30 * time.Minute, want: map[string]*queryv1.SegmentStatus{
			"seg-a": {SegmentId: "seg-a", HomesReporting: 2, Suppressed: true},
			"seg-b": {SegmentId: "seg-b", HomesReporting: 3, CyclesPerHour: 8, ActiveAlerts: 2},
			"seg-c": {SegmentId: "seg-c", Suppressed: true},
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, order := buildStatuses([]string{"seg-c", "seg-a", "seg-b"}, act, alerts, tc.window)
			if len(order) != 3 || order[0] != "seg-a" || order[2] != "seg-c" {
				t.Fatalf("order = %v", order)
			}
			for id, w := range tc.want {
				if !proto.Equal(got[id], w) {
					t.Errorf("%s = %v, want %v", id, got[id], w)
				}
			}
		})
	}
}

func TestCursor(t *testing.T) {
	ts := time.Date(2026, 4, 15, 12, 30, 0, 123456000, time.UTC)
	id := uuid.MustParse("0b8f3a36-8d2e-4c1a-9d8f-0c7b0e1f2a3b")
	gotTS, gotID, err := decodeCursor(encodeCursor(ts, id))
	if err != nil || !gotTS.Equal(ts) || gotID != id {
		t.Fatalf("round trip = %v %v %v", gotTS, gotID, err)
	}
	enc := func(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }
	for _, bad := range []string{"!!!", enc("no-dot"), enc("x." + id.String()), enc("123.not-a-uuid")} {
		if _, _, err := decodeCursor(bad); err == nil {
			t.Errorf("decodeCursor(%q) accepted", bad)
		}
	}
}

func TestConversions(t *testing.T) {
	if u32(-1) != 0 || u32(7) != 7 || u32(1<<40) != 1<<32-1 {
		t.Error("u32 clamping")
	}
	m := homeStormToProto(sqlcgen.ListHomeStormMetricsRow{LagMin: f8(12), Cycles: 3})
	if !m.GetLagReached() || m.GetRecessionReached() || m.GetLagMin() != 12 || m.GetCycles() != 3 {
		t.Errorf("homeStormToProto = %v", m)
	}
	s := segmentToProto(sqlcgen.GatewayListSegmentsRow{ID: "seg-01", Kind: "near_pond", HomeCount: 4, Geometry: []byte(`{"type":"Polygon"}`)})
	if s.GetKind() != queryv1.SegmentKind_SEGMENT_KIND_NEAR_POND || s.GetHomeCount() != 4 || s.GetGeometryGeojson() == "" {
		t.Errorf("segmentToProto = %v", s)
	}
}
