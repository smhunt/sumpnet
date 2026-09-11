package alerts

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	alertsv1 "github.com/smhunt/sumpnet/gen/go/sumpnet/alerts/v1"
	"github.com/smhunt/sumpnet/internal/store/sqlcgen"
)

// Server implements alerts.v1.AlertService.
//
// There is no authentication in Phase 3: the port is compose-internal and the
// api-gateway (Phase 5) will enforce owner scoping and the k >= 3 rule before
// forwarding here.
type Server struct {
	alertsv1.UnimplementedAlertServiceServer
	q   *sqlcgen.Queries
	now func() time.Time
}

// NewServer builds a Server on pool-bound queries.
func NewServer(q *sqlcgen.Queries) *Server { return &Server{q: q, now: time.Now} }

// ListActiveAlerts implements AlertServiceServer.
func (s *Server) ListActiveAlerts(ctx context.Context, req *alertsv1.ListActiveAlertsRequest) (*alertsv1.ListActiveAlertsResponse, error) {
	if req.GetHomeId() != "" && req.GetSegmentId() != "" {
		return nil, status.Error(codes.InvalidArgument, "filter by home_id or segment_id, not both")
	}
	var p sqlcgen.ListActiveAlertsParams
	if req.GetHomeId() != "" {
		id, err := uuid.Parse(req.GetHomeId())
		if err != nil {
			return nil, status.Error(codes.InvalidArgument, "home_id must be a UUID")
		}
		p.HomeID = uuid.NullUUID{UUID: id, Valid: true}
	}
	if req.GetSegmentId() != "" {
		p.SegmentID = pgtype.Text{String: req.GetSegmentId(), Valid: true}
	}
	rows, err := s.q.ListActiveAlerts(ctx, p)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "list alerts: %v", err)
	}
	out := &alertsv1.ListActiveAlertsResponse{Alerts: make([]*alertsv1.Alert, 0, len(rows))}
	for _, r := range rows {
		out.Alerts = append(out.Alerts, toProto(r.Alert, r.SegmentID))
	}
	return out, nil
}

// Acknowledge implements AlertServiceServer. It is idempotent.
func (s *Server) Acknowledge(ctx context.Context, req *alertsv1.AcknowledgeRequest) (*alertsv1.AcknowledgeResponse, error) {
	id, err := uuid.Parse(req.GetAlertId())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, "alert_id must be a UUID")
	}
	_, err = s.q.AcknowledgeAlert(ctx, sqlcgen.AcknowledgeAlertParams{
		ID: id, AckedAt: sql.NullTime{Time: s.now().UTC(), Valid: true}, Note: pgtype.Text{String: req.GetNote(), Valid: req.GetNote() != ""},
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, status.Error(codes.NotFound, "alert not found")
	}
	if err != nil {
		return nil, status.Errorf(codes.Internal, "acknowledge: %v", err)
	}
	row, err := s.q.GetAlert(ctx, id)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "get alert: %v", err)
	}
	return &alertsv1.AcknowledgeResponse{Alert: toProto(row.Alert, row.SegmentID)}, nil
}

func toProto(a sqlcgen.Alert, segment pgtype.Text) *alertsv1.Alert {
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
