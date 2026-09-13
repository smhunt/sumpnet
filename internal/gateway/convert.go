package gateway

import (
	"encoding/base64"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"google.golang.org/protobuf/types/known/timestamppb"

	alertsv1 "github.com/smhunt/sumpnet/gen/go/sumpnet/alerts/v1"
	queryv1 "github.com/smhunt/sumpnet/gen/go/sumpnet/query/v1"
	"github.com/smhunt/sumpnet/internal/privacy"
	"github.com/smhunt/sumpnet/internal/store/sqlcgen"
)

var segmentKinds = map[string]queryv1.SegmentKind{
	"standard":    queryv1.SegmentKind_SEGMENT_KIND_STANDARD,
	"wooded":      queryv1.SegmentKind_SEGMENT_KIND_WOODED,
	"near_pond":   queryv1.SegmentKind_SEGMENT_KIND_NEAR_POND,
	"high_ground": queryv1.SegmentKind_SEGMENT_KIND_HIGH_GROUND,
}

func segmentToProto(s sqlcgen.GatewayListSegmentsRow) *queryv1.Segment {
	return &queryv1.Segment{
		Id:              s.ID,
		Name:            s.Name,
		GeometryGeojson: string(s.Geometry),
		HomeCount:       u32(s.HomeCount),
		Kind:            segmentKinds[s.Kind],
	}
}

func stormToProto(e sqlcgen.StormEvent) *queryv1.StormEvent {
	p := &queryv1.StormEvent{
		Id:               e.ID.String(),
		StartedAt:        timestamppb.New(e.StartedAt),
		TotalRainMm:      e.TotalRainMm,
		PeakIntensityMmH: e.PeakIntensityMmH,
	}
	if e.EndedAt.Valid {
		p.EndedAt = timestamppb.New(e.EndedAt.Time)
	}
	return p
}

// alertToProto mirrors internal/alerts' conversion (the gateway reads the
// same rows but must not depend on the alerts service package).
func alertToProto(a sqlcgen.Alert, segment pgtype.Text) *alertsv1.Alert {
	p := &alertsv1.Alert{
		Id:        a.ID.String(),
		SegmentId: segment.String,
		Code:      alertsv1.AlertCode(a.Code),
		Severity:  alertsv1.AlertSeverity(a.Severity),
		RaisedAt:  timestamppb.New(a.RaisedAt),
		Message:   a.Message,
		DeviceId:  a.DeviceID,
	}
	if a.HomeID.Valid {
		p.HomeId = a.HomeID.UUID.String()
	}
	if a.AckedAt.Valid {
		p.AckedAt = timestamppb.New(a.AckedAt.Time)
	}
	if a.ResolvedAt.Valid {
		p.ResolvedAt = timestamppb.New(a.ResolvedAt.Time)
	}
	return p
}

func homeStormToProto(r sqlcgen.ListHomeStormMetricsRow) *queryv1.HomeStormMetrics {
	return &queryv1.HomeStormMetrics{
		StormId:          r.StormID.String(),
		HomeId:           r.HomeID.String(),
		LagMin:           r.LagMin.Float64,
		LagReached:       r.LagMin.Valid,
		RecessionMin:     r.RecessionMin.Float64,
		RecessionReached: r.RecessionMin.Valid,
		VolumeL:          r.VolumeL,
		Cycles:           u32(int64(r.Cycles)),
		StormStartedAt:   timestamppb.New(r.StartedAt),
	}
}

func f8ptr(v pgtype.Float8) *float64 {
	if !v.Valid {
		return nil
	}
	f := v.Float64
	return &f
}

// aggregateStorm builds the public per-segment view of one storm. Every
// segment in the catalogue is reported (suppressed when fewer than
// privacy.MinHomes linked homes have metrics), so the map can explain why a
// street is dark. The numbers come only from privacy.AggregateStorm.
func aggregateStorm(stormID string, segmentIDs []string, rows []sqlcgen.ListStormHomeMetricsRow) []*queryv1.SegmentStormMetrics {
	by := make(map[string][]privacy.HomeStorm)
	for _, r := range rows {
		by[r.SegmentID] = append(by[r.SegmentID], privacy.HomeStorm{
			HomeID: r.HomeID.String(), VolumeL: r.VolumeL, LagMin: f8ptr(r.LagMin), RecessionMin: f8ptr(r.RecessionMin),
		})
	}
	ids := mergeIDs(segmentIDs, by)
	out := make([]*queryv1.SegmentStormMetrics, 0, len(ids))
	for _, id := range ids {
		a := privacy.AggregateStorm(id, by[id])
		out = append(out, &queryv1.SegmentStormMetrics{
			StormId:            stormID,
			SegmentId:          a.SegmentID,
			HomesReporting:     u32(int64(a.HomesReporting)),
			Suppressed:         a.Suppressed,
			LoadLPerHome:       a.LoadLPerHome,
			MedianLagMin:       a.MedianLagMin,
			MedianRecessionMin: a.MedianRecessionMin,
		})
	}
	return out
}

// buildStatuses builds every segment's live status from window totals; the
// numbers come only from privacy.AggregateStatus.
func buildStatuses(segmentIDs []string, activity []sqlcgen.SegmentActivityRow, alerts []sqlcgen.SegmentOpenAlertCountsRow, window time.Duration) (map[string]*queryv1.SegmentStatus, []string) {
	type totals struct{ homes, cycles, alerts int64 }
	by := make(map[string]*totals)
	get := func(id string) *totals {
		t, ok := by[id]
		if !ok {
			t = &totals{}
			by[id] = t
		}
		return t
	}
	for _, a := range activity {
		t := get(a.SegmentID)
		t.homes, t.cycles = a.HomesReporting, a.Cycles
	}
	for _, a := range alerts {
		get(a.SegmentID).alerts = a.ActiveAlerts
	}
	ids := mergeIDs(segmentIDs, by)
	out := make(map[string]*queryv1.SegmentStatus, len(ids))
	for _, id := range ids {
		t := by[id]
		if t == nil {
			t = &totals{}
		}
		s := privacy.AggregateStatus(id, int(t.homes), float64(t.cycles)/window.Hours(), int(t.alerts))
		out[id] = &queryv1.SegmentStatus{
			SegmentId:      s.SegmentID,
			HomesReporting: u32(int64(s.HomesReporting)),
			Suppressed:     s.Suppressed,
			CyclesPerHour:  s.CyclesPerHour,
			ActiveAlerts:   u32(int64(s.ActiveAlerts)),
		}
	}
	return out, ids
}

// mergeIDs returns the catalogue ids followed by any extra keys, sorted.
func mergeIDs[V any](catalogue []string, extra map[string]V) []string {
	seen := make(map[string]bool, len(catalogue))
	ids := make([]string, 0, len(catalogue)+len(extra))
	for _, id := range catalogue {
		if !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	for id := range extra {
		if !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	return ids
}

// u32 clamps a count into a uint32 proto field.
func u32(n int64) uint32 {
	if n <= 0 {
		return 0
	}
	if n > math.MaxUint32 {
		return math.MaxUint32
	}
	return uint32(n)
}

// Page tokens are opaque to clients: base64url("<started_at unix µs>.<id>").
var errBadPageToken = errors.New("invalid page_token")

func encodeCursor(startedAt time.Time, id uuid.UUID) string {
	return base64.RawURLEncoding.EncodeToString([]byte(strconv.FormatInt(startedAt.UnixMicro(), 10) + "." + id.String()))
}

func decodeCursor(tok string) (time.Time, uuid.UUID, error) {
	raw, err := base64.RawURLEncoding.DecodeString(tok)
	if err != nil {
		return time.Time{}, uuid.UUID{}, errBadPageToken
	}
	ts, id, ok := strings.Cut(string(raw), ".")
	if !ok {
		return time.Time{}, uuid.UUID{}, errBadPageToken
	}
	us, err := strconv.ParseInt(ts, 10, 64)
	if err != nil {
		return time.Time{}, uuid.UUID{}, errBadPageToken
	}
	u, err := uuid.Parse(id)
	if err != nil {
		return time.Time{}, uuid.UUID{}, fmt.Errorf("%w: id", errBadPageToken)
	}
	return time.UnixMicro(us).UTC(), u, nil
}
