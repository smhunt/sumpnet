package gateway

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	queryv1 "github.com/smhunt/sumpnet/gen/go/sumpnet/query/v1"
	"github.com/smhunt/sumpnet/internal/store"
	"github.com/smhunt/sumpnet/internal/store/sqlcgen"
)

// changeOverlap re-reads rows stamped this long before the newest change
// seen, so a transaction that committed after a later-stamped one is not
// missed (updated_at = now() is the transaction start).
const changeOverlap = time.Minute

var errShuttingDown = status.Error(codes.Unavailable, "gateway shutting down; reconnect")

// Hub fans live neighbourhood updates out to WatchNeighbourhood streams.
//
// One goroutine (Run) recomputes the neighbourhood on every ingest NOTIFY
// (debounced) and on a ticker, the ADR 0003 pattern: the notification is a
// hint, the recompute from the database is the truth. Segment status is
// recomputed in full through internal/privacy and only changed segments are
// sent; storm events and alerts are sent when their row changes. Alerts are
// routed only to subscribers whose Clerk subject owns the alert's home;
// anonymous subscribers never receive one.
type Hub struct {
	pool *pgxpool.Pool
	dsn  string
	cfg  WatchConfig
	m    *Metrics
	log  *slog.Logger

	kick      chan struct{}
	ready     chan struct{} // closed after the first successful round
	readyOnce sync.Once
	done      chan struct{} // closed when Run returns

	mu       sync.Mutex
	subs     map[*subscriber]struct{}
	statuses map[string]*queryv1.SegmentStatus
	order    []string
	clock    time.Time

	// Owned by the Run goroutine.
	storms changeTracker
	alerts changeTracker
}

// NewHub builds a Hub; Run must be started for Serve to make progress.
func NewHub(pool *pgxpool.Pool, dsn string, cfg WatchConfig, m *Metrics, log *slog.Logger) *Hub {
	return &Hub{
		pool: pool, dsn: dsn, cfg: cfg, m: m, log: log.With("component", "watch"),
		kick: make(chan struct{}, 1), ready: make(chan struct{}), done: make(chan struct{}),
		subs: map[*subscriber]struct{}{}, statuses: map[string]*queryv1.SegmentStatus{},
		storms: newChangeTracker(time.Time{}), alerts: newChangeTracker(time.Time{}),
	}
}

// Ready is closed once the first neighbourhood state has been computed.
func (h *Hub) Ready() <-chan struct{} { return h.ready }

// poke requests a recompute soon (coalesced).
func (h *Hub) poke() {
	select {
	case h.kick <- struct{}{}:
	default:
	}
}

// Run recomputes and publishes until ctx ends.
func (h *Hub) Run(ctx context.Context) error {
	defer close(h.done)
	var now time.Time
	if err := h.pool.QueryRow(ctx, `SELECT now()`).Scan(&now); err != nil {
		if ctx.Err() != nil {
			return nil
		}
		return fmt.Errorf("watch: database clock: %w", err)
	}
	// Changes from before start are covered by snapshots, not replayed.
	h.storms, h.alerts = newChangeTracker(now), newChangeTracker(now)

	go h.listen(ctx)
	ticker := time.NewTicker(h.cfg.PollInterval)
	defer ticker.Stop()
	for {
		if err := h.round(ctx); err != nil {
			if ctx.Err() != nil {
				return nil
			}
			h.m.Rounds.WithLabelValues("error").Inc()
			h.log.Warn("neighbourhood round failed", "err", err)
		} else {
			h.m.Rounds.WithLabelValues("ok").Inc()
			h.readyOnce.Do(func() { close(h.ready) })
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		case <-h.kick:
			if h.cfg.Debounce > 0 {
				t := time.NewTimer(h.cfg.Debounce)
				select {
				case <-t.C:
				case <-ctx.Done():
					t.Stop()
					return nil
				}
			}
			select {
			case <-h.kick:
			default:
			}
		}
	}
}

// listen keeps a dedicated connection subscribed to the ingest channel and
// pokes on every notification and (re)connect. Storm-analytics also notifies
// this channel (table storm_events); alert changes are caught by the ticker.
func (h *Hub) listen(ctx context.Context) {
	backoff := time.Second
	sleep := func() bool {
		select {
		case <-time.After(backoff):
			backoff = min(backoff*2, 30*time.Second)
			return true
		case <-ctx.Done():
			return false
		}
	}
	for ctx.Err() == nil {
		conn, err := connectListener(ctx, h.dsn)
		if err != nil {
			h.log.Warn("listen connect failed", "err", err)
			if !sleep() {
				return
			}
			continue
		}
		if _, err := conn.Exec(ctx, "LISTEN "+store.NotifyChannel); err != nil {
			h.log.Warn("LISTEN failed", "err", err)
			_ = conn.Close(context.WithoutCancel(ctx))
			if !sleep() {
				return
			}
			continue
		}
		backoff = time.Second
		h.poke()
		for ctx.Err() == nil {
			if _, err := conn.WaitForNotification(ctx); err != nil {
				break
			}
			h.m.Notifications.Inc()
			h.poke()
		}
		_ = conn.Close(context.WithoutCancel(ctx))
	}
}

// round reads the neighbourhood in one read-only snapshot and publishes the
// differences.
func (h *Hub) round(ctx context.Context) error {
	tx, err := h.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	q := sqlcgen.New(tx)

	clock, statuses, order, err := computeStatuses(ctx, q, h.cfg.StatusWindow)
	if err != nil {
		return err
	}
	storms, err := q.ListStormEventsChangedSince(ctx, h.storms.since())
	if err != nil {
		return fmt.Errorf("changed storms: %w", err)
	}
	alertRows, err := q.ListAlertsChangedSince(ctx, h.alerts.since())
	if err != nil {
		return fmt.Errorf("changed alerts: %w", err)
	}
	var extra []routed
	var stormSeen, alertSeen []version
	for _, e := range storms {
		if h.storms.isNew(e.ID, e.UpdatedAt) {
			extra = append(extra, stormUpdate(e))
			stormSeen = append(stormSeen, version{e.ID, e.UpdatedAt})
		}
	}
	var changed []sqlcgen.ListAlertsChangedSinceRow
	var homeIDs []uuid.UUID
	for _, r := range alertRows {
		if h.alerts.isNew(r.Alert.ID, r.Alert.UpdatedAt) {
			changed = append(changed, r)
			alertSeen = append(alertSeen, version{r.Alert.ID, r.Alert.UpdatedAt})
			homeIDs = append(homeIDs, r.Alert.HomeID.UUID)
		}
	}
	owners := map[uuid.UUID][]string{}
	if len(changed) > 0 {
		rows, err := q.ListOwnersOfHomes(ctx, homeIDs)
		if err != nil {
			return fmt.Errorf("alert owners: %w", err)
		}
		for _, o := range rows {
			owners[o.HomeID] = append(owners[o.HomeID], o.AuthSubject)
		}
	}
	for _, r := range changed {
		extra = append(extra, alertUpdate(r.Alert, r.SegmentID, owners[r.Alert.HomeID.UUID]))
	}
	// Everything read: only now record what was seen.
	h.storms.record(stormSeen)
	h.alerts.record(alertSeen)
	h.publish(clock, statuses, order, extra)
	return nil
}

func computeStatuses(ctx context.Context, q *sqlcgen.Queries, window time.Duration) (time.Time, map[string]*queryv1.SegmentStatus, []string, error) {
	clock, err := q.NeighbourhoodClock(ctx)
	if err != nil {
		return time.Time{}, nil, nil, fmt.Errorf("clock: %w", err)
	}
	segs, err := q.GatewayListSegments(ctx)
	if err != nil {
		return time.Time{}, nil, nil, fmt.Errorf("segments: %w", err)
	}
	act, err := q.SegmentActivity(ctx, sqlcgen.SegmentActivityParams{WindowStart: clock.Add(-window), WindowEnd: clock})
	if err != nil {
		return time.Time{}, nil, nil, fmt.Errorf("activity: %w", err)
	}
	al, err := q.SegmentOpenAlertCounts(ctx)
	if err != nil {
		return time.Time{}, nil, nil, fmt.Errorf("alert counts: %w", err)
	}
	ids := make([]string, len(segs))
	for i, s := range segs {
		ids[i] = s.ID
	}
	statuses, order := buildStatuses(ids, act, al, window)
	return clock, statuses, order, nil
}

// publish records the new state and delivers what changed.
func (h *Hub) publish(clock time.Time, statuses map[string]*queryv1.SegmentStatus, order []string, extra []routed) {
	h.mu.Lock()
	defer h.mu.Unlock()
	ts := timestamppb.New(clock)
	var updates []routed
	for _, id := range order {
		st := statuses[id]
		if old, ok := h.statuses[id]; ok && proto.Equal(old, st) {
			continue
		}
		updates = append(updates, routed{kind: kindStatus, segment: id, u: statusUpdate(ts, st)})
	}
	h.statuses, h.order, h.clock = statuses, order, clock
	updates = append(updates, extra...)
	for _, r := range updates {
		h.m.Updates.WithLabelValues(r.kind.String()).Inc()
	}
	for sub := range h.subs {
		for _, r := range updates {
			if sub.wants(r) && !h.deliver(sub, r.u) {
				break
			}
		}
	}
}

// deliver queues u for s, or ends s's stream when its queue is full.
// Callers hold h.mu.
func (h *Hub) deliver(s *subscriber, u *queryv1.NeighbourhoodUpdate) bool {
	select {
	case s.ch <- u:
		return true
	default:
		delete(h.subs, s)
		s.close()
		h.m.Dropped.Inc()
		h.m.Subscribers.Set(float64(len(h.subs)))
		return false
	}
}

func (h *Hub) remove(s *subscriber) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.subs, s)
	h.m.Subscribers.Set(float64(len(h.subs)))
}

// Serve streams updates to one subscriber until ctx ends. subject is the
// authenticated Clerk subject, or "" for an anonymous caller.
func (h *Hub) Serve(ctx context.Context, req *queryv1.WatchNeighbourhoodRequest, subject string, send func(*queryv1.WatchNeighbourhoodResponse) error) error {
	select {
	case <-h.ready:
	case <-h.done:
		return errShuttingDown
	case <-ctx.Done():
		return nil
	}
	sub := newSubscriber(subject, req.GetSegmentIds(), h.cfg.Buffer)

	// Register and capture the status snapshot under one lock, so every later
	// broadcast is a change relative to what this subscriber was sent.
	var snapshot []*queryv1.NeighbourhoodUpdate
	h.mu.Lock()
	if req.GetSendSnapshot() {
		ts := timestamppb.New(h.clock)
		for _, id := range h.order {
			if sub.wantsSegment(id) {
				snapshot = append(snapshot, statusUpdate(ts, h.statuses[id]))
			}
		}
	}
	h.subs[sub] = struct{}{}
	h.m.Subscribers.Set(float64(len(h.subs)))
	h.mu.Unlock()
	defer h.remove(sub)

	if req.GetSendSnapshot() {
		more, err := h.snapshot(ctx, sub)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			h.log.Error("watch snapshot failed", "err", err)
			return status.Error(codes.Internal, "snapshot failed")
		}
		for _, u := range append(snapshot, more...) {
			if err := send(&queryv1.WatchNeighbourhoodResponse{Update: u}); err != nil {
				return err
			}
		}
	}
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-h.done:
			return errShuttingDown
		case <-sub.gone:
			return status.Error(codes.ResourceExhausted, "subscriber fell behind; reconnect with send_snapshot")
		case u := <-sub.ch:
			if err := send(&queryv1.WatchNeighbourhoodResponse{Update: u}); err != nil {
				return err
			}
		}
	}
}

// snapshot reads open and recent storms and, for an owner, their active alerts.
func (h *Hub) snapshot(ctx context.Context, sub *subscriber) ([]*queryv1.NeighbourhoodUpdate, error) {
	q := sqlcgen.New(h.pool)
	storms, err := q.ListSnapshotStormEvents(ctx, h.cfg.RecentStorms)
	if err != nil {
		return nil, fmt.Errorf("storms: %w", err)
	}
	out := make([]*queryv1.NeighbourhoodUpdate, 0, len(storms))
	for _, e := range storms {
		out = append(out, stormUpdate(e).u)
	}
	if sub.subject == "" {
		return out, nil
	}
	rows, err := q.ListOwnerActiveAlerts(ctx, sqlcgen.ListOwnerActiveAlertsParams{AuthSubject: sub.subject})
	if err != nil {
		return nil, fmt.Errorf("owner alerts: %w", err)
	}
	for _, r := range rows {
		if sub.wantsSegment(r.SegmentID.String) {
			out = append(out, alertUpdate(r.Alert, r.SegmentID, nil).u)
		}
	}
	return out, nil
}

// --- routing -----------------------------------------------------------------

type updateKind uint8

const (
	kindStatus updateKind = iota
	kindStorm
	kindAlert
)

func (k updateKind) String() string {
	switch k {
	case kindStatus:
		return "segment_status"
	case kindStorm:
		return "storm_event"
	default:
		return "alert"
	}
}

// routed is an update plus what decides who may receive it.
type routed struct {
	kind    updateKind
	segment string
	owners  []string // alert: Clerk subjects owning the alert's home
	u       *queryv1.NeighbourhoodUpdate
}

type subscriber struct {
	subject  string          // "" = anonymous
	segments map[string]bool // nil = every segment
	ch       chan *queryv1.NeighbourhoodUpdate
	gone     chan struct{}
	goneOnce sync.Once
}

func newSubscriber(subject string, segmentIDs []string, buffer int) *subscriber {
	s := &subscriber{subject: subject, ch: make(chan *queryv1.NeighbourhoodUpdate, buffer), gone: make(chan struct{})}
	if len(segmentIDs) > 0 {
		s.segments = make(map[string]bool, len(segmentIDs))
		for _, id := range segmentIDs {
			s.segments[id] = true
		}
	}
	return s
}

func (s *subscriber) close() { s.goneOnce.Do(func() { close(s.gone) }) }

func (s *subscriber) wantsSegment(id string) bool { return s.segments == nil || s.segments[id] }

// wants is the stream privacy rule: statuses and storms are public; an alert
// goes only to an authenticated owner of the alert's home.
func (s *subscriber) wants(r routed) bool {
	switch r.kind {
	case kindStatus:
		return s.wantsSegment(r.segment)
	case kindStorm:
		return true
	case kindAlert:
		return s.subject != "" && slices.Contains(r.owners, s.subject) && s.wantsSegment(r.segment)
	}
	return false
}

func statusUpdate(ts *timestamppb.Timestamp, st *queryv1.SegmentStatus) *queryv1.NeighbourhoodUpdate {
	return &queryv1.NeighbourhoodUpdate{Ts: ts, Update: &queryv1.NeighbourhoodUpdate_SegmentStatus{SegmentStatus: st}}
}

func stormUpdate(e sqlcgen.StormEvent) routed {
	return routed{kind: kindStorm, u: &queryv1.NeighbourhoodUpdate{
		Ts: timestamppb.New(e.UpdatedAt), Update: &queryv1.NeighbourhoodUpdate_StormEvent{StormEvent: stormToProto(e)},
	}}
}

func alertUpdate(a sqlcgen.Alert, segment pgtype.Text, owners []string) routed {
	return routed{kind: kindAlert, segment: segment.String, owners: owners, u: &queryv1.NeighbourhoodUpdate{
		Ts: timestamppb.New(a.UpdatedAt), Update: &queryv1.NeighbourhoodUpdate_Alert{Alert: alertToProto(a, segment)},
	}}
}

// --- change tracking ---------------------------------------------------------

type version struct {
	id        uuid.UUID
	updatedAt time.Time
}

// changeTracker turns "rows with updated_at > mark - overlap" into one
// delivery per row version.
type changeTracker struct {
	mark time.Time
	seen map[uuid.UUID]time.Time
}

func newChangeTracker(mark time.Time) changeTracker {
	return changeTracker{mark: mark, seen: map[uuid.UUID]time.Time{}}
}

func (c *changeTracker) since() time.Time { return c.mark.Add(-changeOverlap) }

func (c *changeTracker) isNew(id uuid.UUID, updatedAt time.Time) bool {
	prev, ok := c.seen[id]
	return !ok || !prev.Equal(updatedAt)
}

func (c *changeTracker) record(vs []version) {
	for _, v := range vs {
		c.seen[v.id] = v.updatedAt
		if v.updatedAt.After(c.mark) {
			c.mark = v.updatedAt
		}
	}
	cut := c.since()
	for id, t := range c.seen {
		if t.Before(cut) {
			delete(c.seen, id)
		}
	}
}
