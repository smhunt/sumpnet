// Package ingest implements telemetry.v1.IngestService: client-streaming
// RPCs that batch rows and store them idempotently. Backpressure is a bounded
// queue between the receive loop and the store — when the store falls behind,
// the handler stops calling Recv, HTTP/2 flow control fills, and the bridge's
// Send blocks.
package ingest

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"time"

	"golang.org/x/sync/errgroup"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	telemetryv1 "github.com/smhunt/sumpnet/gen/go/sumpnet/telemetry/v1"
	"github.com/smhunt/sumpnet/internal/store"
)

// Store is the subset of *store.Store the handlers need (faked in tests).
type Store interface {
	InsertReadings(ctx context.Context, rows []store.Reading) (store.Result, error)
	InsertCycleEvents(ctx context.Context, rows []store.CycleEvent) (store.Result, error)
	InsertStormSummaries(ctx context.Context, rows []store.StormSummary) (store.Result, error)
	InsertAlarmEvents(ctx context.Context, rows []store.AlarmEvent) (store.Result, error)
	InsertRainGaugeUplinks(ctx context.Context, rows []store.RainGaugeUplink) (store.Result, error)
}

// Server implements IngestServiceServer.
type Server struct {
	telemetryv1.UnimplementedIngestServiceServer
	store Store
	cfg   Config
	m     *Metrics
	log   *slog.Logger
	now   func() time.Time
}

// New builds a Server.
func New(st Store, m *Metrics, cfg Config, log *slog.Logger) *Server {
	return &Server{store: st, cfg: cfg, m: m, log: log, now: time.Now}
}

type counts struct{ accepted, duplicates, rejected uint32 }

// SubmitReadings implements IngestServiceServer.
func (s *Server) SubmitReadings(stream telemetryv1.IngestService_SubmitReadingsServer) error {
	c, err := runStream(stream.Context(), s, "readings", stream.Recv,
		func(req *telemetryv1.SubmitReadingsRequest) []store.Reading {
			rows := make([]store.Reading, 0, len(req.GetReadings()))
			for _, r := range req.GetReadings() {
				row, why := s.readingRow(r)
				if why != "" {
					s.reject("readings", why)
					continue
				}
				rows = append(rows, row)
			}
			return rows
		},
		s.store.InsertReadings)
	if err != nil {
		return err
	}
	return stream.SendAndClose(&telemetryv1.SubmitReadingsResponse{Accepted: c.accepted, Duplicates: c.duplicates})
}

// cycleItem carries either a cycle event or a storm summary through one stream.
type cycleItem struct {
	cycle   *store.CycleEvent
	summary *store.StormSummary
}

// SubmitCycleEvents implements IngestServiceServer.
func (s *Server) SubmitCycleEvents(stream telemetryv1.IngestService_SubmitCycleEventsServer) error {
	c, err := runStream(stream.Context(), s, "cycle_events", stream.Recv,
		func(req *telemetryv1.SubmitCycleEventsRequest) []cycleItem {
			items := make([]cycleItem, 0, len(req.GetCycleEvents())+len(req.GetStormSummaries()))
			for _, ce := range req.GetCycleEvents() {
				row, why := s.cycleRow(ce)
				if why != "" {
					s.reject("cycle_events", why)
					continue
				}
				items = append(items, cycleItem{cycle: &row})
			}
			for _, ss := range req.GetStormSummaries() {
				row, why := s.summaryRow(ss)
				if why != "" {
					s.reject("storm_summaries", why)
					continue
				}
				items = append(items, cycleItem{summary: &row})
			}
			return items
		},
		func(ctx context.Context, items []cycleItem) (store.Result, error) {
			var cycles []store.CycleEvent
			var summaries []store.StormSummary
			for _, it := range items {
				if it.cycle != nil {
					cycles = append(cycles, *it.cycle)
				} else {
					summaries = append(summaries, *it.summary)
				}
			}
			var total store.Result
			if len(cycles) > 0 {
				r, err := s.store.InsertCycleEvents(ctx, cycles)
				if err != nil {
					return total, err
				}
				total.Accepted += r.Accepted
				total.Duplicates += r.Duplicates
				total.Rejected += r.Rejected
			}
			if len(summaries) > 0 {
				r, err := s.store.InsertStormSummaries(ctx, summaries)
				if err != nil {
					return total, err
				}
				total.Accepted += r.Accepted
				total.Duplicates += r.Duplicates
				total.Rejected += r.Rejected
			}
			return total, nil
		})
	if err != nil {
		return err
	}
	return stream.SendAndClose(&telemetryv1.SubmitCycleEventsResponse{Accepted: c.accepted, Duplicates: c.duplicates})
}

// SubmitAlarms implements IngestServiceServer.
func (s *Server) SubmitAlarms(stream telemetryv1.IngestService_SubmitAlarmsServer) error {
	c, err := runStream(stream.Context(), s, "alarm_events", stream.Recv,
		func(req *telemetryv1.SubmitAlarmsRequest) []store.AlarmEvent {
			rows := make([]store.AlarmEvent, 0, len(req.GetAlarms()))
			for _, a := range req.GetAlarms() {
				row, why := s.alarmRow(a)
				if why != "" {
					s.reject("alarm_events", why)
					continue
				}
				rows = append(rows, row)
			}
			return rows
		},
		s.store.InsertAlarmEvents)
	if err != nil {
		return err
	}
	return stream.SendAndClose(&telemetryv1.SubmitAlarmsResponse{Accepted: c.accepted, Duplicates: c.duplicates})
}

// SubmitRainGaugeReadings implements IngestServiceServer.
func (s *Server) SubmitRainGaugeReadings(stream telemetryv1.IngestService_SubmitRainGaugeReadingsServer) error {
	c, err := runStream(stream.Context(), s, "rain_gauge_uplinks", stream.Recv,
		func(req *telemetryv1.SubmitRainGaugeReadingsRequest) []store.RainGaugeUplink {
			rows := make([]store.RainGaugeUplink, 0, len(req.GetRainGaugeReadings()))
			for _, r := range req.GetRainGaugeReadings() {
				row, why := s.rainGaugeRow(r)
				if why != "" {
					s.reject("rain_gauge_uplinks", why)
					continue
				}
				rows = append(rows, row)
			}
			return rows
		},
		s.store.InsertRainGaugeUplinks)
	if err != nil {
		return err
	}
	return stream.SendAndClose(&telemetryv1.SubmitRainGaugeReadingsResponse{Accepted: c.accepted, Duplicates: c.duplicates})
}

func (s *Server) reject(kind string, why reject) {
	s.m.Rejects.WithLabelValues(kind, string(why)).Inc()
	s.m.Rows.WithLabelValues(kind, "rejected").Inc()
}

// runStream drives one client stream: a receive goroutine feeds a batching
// goroutine, which pushes full or aged batches onto a bounded channel that a
// store goroutine drains. The bounded channel is the backpressure point.
func runStream[Req any, Row any](
	ctx context.Context,
	s *Server,
	kind string,
	recv func() (*Req, error),
	extract func(*Req) []Row,
	flush func(context.Context, []Row) (store.Result, error),
) (counts, error) {
	s.m.StreamsActive.WithLabelValues(kind).Inc()
	defer s.m.StreamsActive.WithLabelValues(kind).Dec()

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	g, gctx := errgroup.WithContext(ctx)

	reqs := make(chan *Req)
	batches := make(chan []Row, s.cfg.QueueBatches)
	var total counts

	// R: receive until EOF.
	g.Go(func() error {
		defer close(reqs)
		for {
			req, err := recv()
			if errors.Is(err, io.EOF) {
				return nil
			}
			if err != nil {
				return fmt.Errorf("recv: %w", err)
			}
			select {
			case reqs <- req:
			case <-gctx.Done():
				return gctx.Err()
			}
		}
	})

	// A: batch by size and age; blocks on `batches` when the store is behind.
	g.Go(func() error {
		defer close(batches)
		var cur []Row
		var timer *time.Timer
		var due <-chan time.Time
		push := func() error {
			if len(cur) == 0 {
				return nil
			}
			b := cur
			cur = nil
			if timer != nil {
				timer.Stop()
				timer, due = nil, nil
			}
			select {
			case batches <- b:
				s.m.QueueBatches.WithLabelValues(kind).Set(float64(len(batches)))
				return nil
			case <-gctx.Done():
				return gctx.Err()
			}
		}
		for {
			select {
			case req, ok := <-reqs:
				if !ok {
					return push()
				}
				rows := extract(req)
				if len(rows) == 0 {
					continue
				}
				if cur == nil {
					timer = time.NewTimer(s.cfg.BatchDelay)
					due = timer.C
				}
				cur = append(cur, rows...)
				if len(cur) >= s.cfg.BatchMax {
					if err := push(); err != nil {
						return err
					}
				}
			case <-due:
				if err := push(); err != nil {
					return err
				}
			case <-gctx.Done():
				return gctx.Err()
			}
		}
	})

	// B: store batches in order.
	g.Go(func() error {
		for b := range batches {
			s.m.QueueBatches.WithLabelValues(kind).Set(float64(len(batches)))
			start := time.Now()
			res, err := flush(gctx, b)
			s.m.BatchSeconds.WithLabelValues(kind).Observe(time.Since(start).Seconds())
			s.m.BatchRows.WithLabelValues(kind).Observe(float64(len(b)))
			if err != nil {
				return fmt.Errorf("store: %w", err)
			}
			s.m.Rows.WithLabelValues(kind, "accepted").Add(float64(res.Accepted))
			s.m.Rows.WithLabelValues(kind, "duplicate").Add(float64(res.Duplicates))
			s.m.Rows.WithLabelValues(kind, "rejected").Add(float64(res.Rejected))
			total.accepted += uint32(res.Accepted)     //nolint:gosec // batch sizes are small
			total.duplicates += uint32(res.Duplicates) //nolint:gosec // batch sizes are small
			total.rejected += uint32(res.Rejected)     //nolint:gosec // batch sizes are small
		}
		return nil
	})

	if err := g.Wait(); err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return total, status.Error(codes.Canceled, err.Error())
		}
		s.log.Warn("stream failed", "kind", kind, "err", err)
		return total, status.Error(codes.Unavailable, err.Error())
	}
	return total, nil
}
