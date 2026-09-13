package gateway

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	alertsv1 "github.com/smhunt/sumpnet/gen/go/sumpnet/alerts/v1"
	queryv1 "github.com/smhunt/sumpnet/gen/go/sumpnet/query/v1"
	"github.com/smhunt/sumpnet/internal/auth"
	"github.com/smhunt/sumpnet/internal/store/sqlcgen"
)

const (
	defaultPageSize  = 50
	maxPageSize      = 200
	recentHomeStorms = 10
	maxNoteLen       = 500
	maxWatchSegments = 100
	alertsTimeout    = 10 * time.Second
)

// Server implements query.v1.QueryService. The auth interceptor has already
// resolved the caller (or rejected a bad token); owner RPCs require one.
type Server struct {
	queryv1.UnimplementedQueryServiceServer
	q      *sqlcgen.Queries
	alerts alertsv1.AlertServiceClient // nil: acknowledge unavailable
	hub    *Hub
	log    *slog.Logger
}

// NewServer builds a Server on read-only queries.
func NewServer(q *sqlcgen.Queries, alerts alertsv1.AlertServiceClient, hub *Hub, log *slog.Logger) *Server {
	return &Server{q: q, alerts: alerts, hub: hub, log: log}
}

// internal logs err and returns an opaque INTERNAL status.
func (s *Server) internal(ctx context.Context, op string, err error) error {
	if ctx.Err() != nil {
		return status.FromContextError(ctx.Err()).Err()
	}
	s.log.ErrorContext(ctx, "query failed", "op", op, "err", err)
	return status.Error(codes.Internal, "internal error")
}

// GetHome implements QueryServiceServer: owner-scoped, and a home the caller
// does not own is indistinguishable from one that does not exist.
func (s *Server) GetHome(ctx context.Context, req *queryv1.GetHomeRequest) (*queryv1.GetHomeResponse, error) {
	p, err := auth.RequireOwner(ctx)
	if err != nil {
		return nil, err
	}
	id, err := uuid.Parse(req.GetHomeId())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, "home_id must be a UUID")
	}
	h, err := s.q.GetOwnedHome(ctx, sqlcgen.GetOwnedHomeParams{AuthSubject: p.Subject, HomeID: id})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, status.Error(codes.NotFound, "home not found")
	}
	if err != nil {
		return nil, s.internal(ctx, "get home", err)
	}
	home, err := s.home(ctx, h)
	if err != nil {
		return nil, s.internal(ctx, "home detail", err)
	}
	rows, err := s.q.ListHomeStormMetrics(ctx, sqlcgen.ListHomeStormMetricsParams{HomeID: id, Lim: recentHomeStorms})
	if err != nil {
		return nil, s.internal(ctx, "home storms", err)
	}
	out := &queryv1.GetHomeResponse{Home: home, RecentStorms: make([]*queryv1.HomeStormMetrics, 0, len(rows))}
	for _, r := range rows {
		out.RecentStorms = append(out.RecentStorms, homeStormToProto(r))
	}
	return out, nil
}

// home assembles a Home the caller is already known to own.
func (s *Server) home(ctx context.Context, h sqlcgen.Home) (*queryv1.Home, error) {
	nid := uuid.NullUUID{UUID: h.ID, Valid: true}
	out := &queryv1.Home{Id: h.ID.String(), SegmentId: h.SegmentID.String, PitAreaM2: h.PitAreaM2.Float64, Health: &queryv1.HomeHealth{}}
	if h.ConsentAt.Valid {
		out.ConsentAt = timestamppb.New(h.ConsentAt.Time)
	}
	devs, err := s.q.ListHomeDevices(ctx, nid)
	if err != nil {
		return nil, err
	}
	for _, d := range devs {
		pd := &queryv1.Device{DevEui: d.DevEui, Kind: d.Kind}
		if d.InstalledAt.Valid {
			pd.InstalledAt = timestamppb.New(d.InstalledAt.Time)
		}
		out.Devices = append(out.Devices, pd)
	}
	r, err := s.q.GetHomeLatestReading(ctx, nid)
	switch {
	case err == nil:
		out.Health.LastSeen = timestamppb.New(r.Ts)
		out.Health.BattMv = u32(int64(r.BattMv.Int32))
		out.Health.MainsOk = r.MainsOk
	case !errors.Is(err, pgx.ErrNoRows):
		return nil, err
	}
	n, err := s.q.CountOpenAlertsForHome(ctx, nid)
	if err != nil {
		return nil, err
	}
	out.Health.ActiveAlerts = u32(n)
	bf, err := s.q.GetHomeLatestBaseflow(ctx, h.ID)
	switch {
	case err == nil:
		out.Health.BaseflowCyclesPerDay = bf
	case !errors.Is(err, pgx.ErrNoRows):
		return nil, err
	}
	return out, nil
}

// ListStormEvents implements QueryServiceServer (public, newest first).
func (s *Server) ListStormEvents(ctx context.Context, req *queryv1.ListStormEventsRequest) (*queryv1.ListStormEventsResponse, error) {
	size := req.GetPageSize()
	switch {
	case size < 0:
		return nil, status.Error(codes.InvalidArgument, "page_size must not be negative")
	case size == 0:
		size = defaultPageSize
	case size > maxPageSize:
		size = maxPageSize
	}
	p := sqlcgen.ListStormEventsPageParams{Lim: size + 1}
	if req.GetSince() != nil {
		if err := req.GetSince().CheckValid(); err != nil {
			return nil, status.Error(codes.InvalidArgument, "invalid since")
		}
		p.Since = sql.NullTime{Time: req.GetSince().AsTime(), Valid: true}
	}
	if req.GetUntil() != nil {
		if err := req.GetUntil().CheckValid(); err != nil {
			return nil, status.Error(codes.InvalidArgument, "invalid until")
		}
		p.Until = sql.NullTime{Time: req.GetUntil().AsTime(), Valid: true}
	}
	if tok := req.GetPageToken(); tok != "" {
		ts, id, err := decodeCursor(tok)
		if err != nil {
			return nil, status.Error(codes.InvalidArgument, err.Error())
		}
		p.CursorStartedAt = sql.NullTime{Time: ts, Valid: true}
		p.CursorID = uuid.NullUUID{UUID: id, Valid: true}
	}
	rows, err := s.q.ListStormEventsPage(ctx, p)
	if err != nil {
		return nil, s.internal(ctx, "list storms", err)
	}
	out := &queryv1.ListStormEventsResponse{}
	if len(rows) > int(size) {
		rows = rows[:size]
		last := rows[len(rows)-1]
		out.NextPageToken = encodeCursor(last.StartedAt, last.ID)
	}
	out.StormEvents = make([]*queryv1.StormEvent, 0, len(rows))
	for _, r := range rows {
		out.StormEvents = append(out.StormEvents, stormToProto(r))
	}
	return out, nil
}

// GetStormEvent implements QueryServiceServer: segment aggregates only.
func (s *Server) GetStormEvent(ctx context.Context, req *queryv1.GetStormEventRequest) (*queryv1.GetStormEventResponse, error) {
	id, err := uuid.Parse(req.GetStormId())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, "storm_id must be a UUID")
	}
	ev, err := s.q.GetStormEventByID(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, status.Error(codes.NotFound, "storm event not found")
	}
	if err != nil {
		return nil, s.internal(ctx, "get storm", err)
	}
	segs, err := s.q.GatewayListSegments(ctx)
	if err != nil {
		return nil, s.internal(ctx, "list segments", err)
	}
	rows, err := s.q.ListStormHomeMetrics(ctx, id)
	if err != nil {
		return nil, s.internal(ctx, "storm metrics", err)
	}
	ids := make([]string, len(segs))
	for i, sg := range segs {
		ids[i] = sg.ID
	}
	return &queryv1.GetStormEventResponse{StormEvent: stormToProto(ev), Segments: aggregateStorm(ev.ID.String(), ids, rows)}, nil
}

// ListSegments implements QueryServiceServer (public).
func (s *Server) ListSegments(ctx context.Context, _ *queryv1.ListSegmentsRequest) (*queryv1.ListSegmentsResponse, error) {
	segs, err := s.q.GatewayListSegments(ctx)
	if err != nil {
		return nil, s.internal(ctx, "list segments", err)
	}
	out := &queryv1.ListSegmentsResponse{Segments: make([]*queryv1.Segment, 0, len(segs))}
	for _, sg := range segs {
		out.Segments = append(out.Segments, segmentToProto(sg))
	}
	return out, nil
}

// ListMyHomes implements QueryServiceServer (owner).
func (s *Server) ListMyHomes(ctx context.Context, _ *queryv1.ListMyHomesRequest) (*queryv1.ListMyHomesResponse, error) {
	p, err := auth.RequireOwner(ctx)
	if err != nil {
		return nil, err
	}
	homes, err := s.q.ListOwnedHomes(ctx, p.Subject)
	if err != nil {
		return nil, s.internal(ctx, "owned homes", err)
	}
	out := &queryv1.ListMyHomesResponse{Homes: make([]*queryv1.Home, 0, len(homes))}
	for _, h := range homes {
		ph, err := s.home(ctx, h)
		if err != nil {
			return nil, s.internal(ctx, "home detail", err)
		}
		out.Homes = append(out.Homes, ph)
	}
	return out, nil
}

// ListMyAlerts implements QueryServiceServer (owner).
func (s *Server) ListMyAlerts(ctx context.Context, req *queryv1.ListMyAlertsRequest) (*queryv1.ListMyAlertsResponse, error) {
	p, err := auth.RequireOwner(ctx)
	if err != nil {
		return nil, err
	}
	params := sqlcgen.ListOwnerActiveAlertsParams{AuthSubject: p.Subject}
	if req.GetHomeId() != "" {
		id, perr := uuid.Parse(req.GetHomeId())
		if perr != nil {
			return nil, status.Error(codes.InvalidArgument, "home_id must be a UUID")
		}
		owned, oerr := s.q.OwnsHome(ctx, sqlcgen.OwnsHomeParams{AuthSubject: p.Subject, HomeID: id})
		if oerr != nil {
			return nil, s.internal(ctx, "owns home", oerr)
		}
		if !owned {
			return nil, status.Error(codes.NotFound, "home not found")
		}
		params.HomeID = uuid.NullUUID{UUID: id, Valid: true}
	}
	rows, err := s.q.ListOwnerActiveAlerts(ctx, params)
	if err != nil {
		return nil, s.internal(ctx, "owner alerts", err)
	}
	out := &queryv1.ListMyAlertsResponse{Alerts: make([]*alertsv1.Alert, 0, len(rows))}
	for _, r := range rows {
		out.Alerts = append(out.Alerts, alertToProto(r.Alert, r.SegmentID))
	}
	return out, nil
}

// AcknowledgeMyAlert implements QueryServiceServer: ownership check here,
// the write in the alerts service (the single writer of alerts).
func (s *Server) AcknowledgeMyAlert(ctx context.Context, req *queryv1.AcknowledgeMyAlertRequest) (*queryv1.AcknowledgeMyAlertResponse, error) {
	p, err := auth.RequireOwner(ctx)
	if err != nil {
		return nil, err
	}
	id, err := uuid.Parse(req.GetAlertId())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, "alert_id must be a UUID")
	}
	if utf8.RuneCountInString(req.GetNote()) > maxNoteLen {
		return nil, status.Errorf(codes.InvalidArgument, "note is longer than %d characters", maxNoteLen)
	}
	owned, err := s.q.OwnsAlert(ctx, sqlcgen.OwnsAlertParams{AuthSubject: p.Subject, AlertID: id})
	if err != nil {
		return nil, s.internal(ctx, "owns alert", err)
	}
	if !owned {
		return nil, status.Error(codes.NotFound, "alert not found")
	}
	if s.alerts == nil {
		return nil, status.Error(codes.Unavailable, "alert acknowledgement is not configured (ALERTS_ADDR)")
	}
	actx, cancel := context.WithTimeout(ctx, alertsTimeout)
	defer cancel()
	resp, err := s.alerts.Acknowledge(actx, &alertsv1.AcknowledgeRequest{AlertId: id.String(), Note: req.GetNote()})
	if err != nil {
		switch st := status.Convert(err); st.Code() {
		case codes.NotFound, codes.InvalidArgument:
			return nil, st.Err()
		default:
			if ctx.Err() != nil {
				return nil, status.FromContextError(ctx.Err()).Err()
			}
			s.log.WarnContext(ctx, "alerts acknowledge failed", "alert_id", id, "err", err)
			return nil, status.Error(codes.Unavailable, "alerts service unavailable")
		}
	}
	return &queryv1.AcknowledgeMyAlertResponse{Alert: resp.GetAlert()}, nil
}

// WatchNeighbourhood implements QueryServiceServer.
func (s *Server) WatchNeighbourhood(req *queryv1.WatchNeighbourhoodRequest, stream grpc.ServerStreamingServer[queryv1.WatchNeighbourhoodResponse]) error {
	if len(req.GetSegmentIds()) > maxWatchSegments {
		return status.Errorf(codes.InvalidArgument, "at most %d segment_ids", maxWatchSegments)
	}
	p, _ := auth.FromContext(stream.Context())
	return s.hub.Serve(stream.Context(), req, p.Subject, stream.Send)
}
