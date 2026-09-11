// Package watermark is the ADR 0003 consumer skeleton shared by every service
// that reacts to new rows: LISTEN on the ingest channel as a wake-up hint, poll
// each source table by a persisted inserted_at watermark, and run the handler,
// its writes and the watermark update in one transaction.
//
// Two facts about inserted_at (DEFAULT now() = transaction start) shape the
// polling: all rows of one ingest batch share a value and become visible only
// at commit, later than that value. So a poll never splits an inserted_at
// group and never reads rows younger than a lag that covers in-flight ingest
// transactions (see the Poll* queries in internal/store/queries/watermarks.sql).
package watermark

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/smhunt/sumpnet/internal/store"
	"github.com/smhunt/sumpnet/internal/store/sqlcgen"
)

// Config tunes a Consumer.
type Config struct {
	Consumer     string        // watermark owner, e.g. "cycle-detector"
	PollInterval time.Duration // POLL_INTERVAL: the safety-net poll (default 30s)
	Lag          time.Duration // WATERMARK_LAG: must exceed the longest ingest transaction (default 5s)
	MaxGroups    int32         // MAX_GROUPS: distinct inserted_at values (≈ ingest batches) per poll (default 20)
}

// ConfigFromEnv overlays POLL_INTERVAL, WATERMARK_LAG and MAX_GROUPS on the defaults.
func ConfigFromEnv(consumer string) (Config, error) {
	c := Config{Consumer: consumer, PollInterval: 30 * time.Second, Lag: 5 * time.Second, MaxGroups: 20}
	if v, ok := os.LookupEnv("POLL_INTERVAL"); ok {
		d, err := time.ParseDuration(v)
		if err != nil {
			return c, fmt.Errorf("POLL_INTERVAL: %w", err)
		}
		c.PollInterval = d
	}
	if v, ok := os.LookupEnv("WATERMARK_LAG"); ok {
		d, err := time.ParseDuration(v)
		if err != nil {
			return c, fmt.Errorf("WATERMARK_LAG: %w", err)
		}
		c.Lag = d
	}
	if v, ok := os.LookupEnv("MAX_GROUPS"); ok {
		n, err := strconv.ParseInt(v, 10, 32)
		if err != nil || n <= 0 {
			return c, fmt.Errorf("MAX_GROUPS: %q", v)
		}
		c.MaxGroups = int32(n)
	}
	if c.PollInterval <= 0 || c.Lag < 0 {
		return c, errors.New("watermark: poll interval must be positive and lag non-negative")
	}
	return c, nil
}

// Source reads rows of one table newer than a watermark.
type Source[T any] struct {
	Table      string
	Poll       func(ctx context.Context, q *sqlcgen.Queries, after time.Time, lagSeconds float64, maxGroups int32) ([]T, error)
	InsertedAt func(T) time.Time
}

// Handler processes one poll's rows inside the poll transaction (q and tx are
// bound to it). Reset is called after a rolled-back transaction so any
// in-memory state derived from the failed batch is discarded.
type Handler[T any] interface {
	Handle(ctx context.Context, tx pgx.Tx, q *sqlcgen.Queries, rows []T) error
	Reset()
}

// Metrics are the consumer's collectors.
type Metrics struct {
	Polls         *prometheus.CounterVec // consumer, source, result
	Rows          *prometheus.CounterVec // consumer, source
	WatermarkLag  *prometheus.GaugeVec   // consumer, source
	Notifications *prometheus.CounterVec // consumer
}

// NewMetrics registers the collectors on reg.
func NewMetrics(reg prometheus.Registerer) *Metrics {
	m := &Metrics{
		Polls: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "sumpnet_consumer_polls_total", Help: "Poll transactions by result."}, []string{"consumer", "source", "result"}),
		Rows: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "sumpnet_consumer_rows_total", Help: "Rows handled."}, []string{"consumer", "source"}),
		WatermarkLag: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "sumpnet_consumer_watermark_lag_seconds", Help: "now() minus the persisted watermark."}, []string{"consumer", "source"}),
		Notifications: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "sumpnet_consumer_notifications_total", Help: "LISTEN wake-ups received for a watched table."}, []string{"consumer"}),
	}
	reg.MustRegister(m.Polls, m.Rows, m.WatermarkLag, m.Notifications)
	return m
}

type stage interface {
	table() string
	run(ctx context.Context, c *Consumer) (int, error)
}

// Consumer runs one or more stages in order, each in its own transaction.
type Consumer struct {
	pool    *pgxpool.Pool
	dsn     string
	cfg     Config
	m       *Metrics
	log     *slog.Logger
	stages  []stage
	wake    chan struct{}
	onRound []func(rows int)
	ready   atomic.Bool
}

// New builds a Consumer; dsn is used for the dedicated LISTEN connection.
func New(pool *pgxpool.Pool, dsn string, cfg Config, m *Metrics, log *slog.Logger) *Consumer {
	return &Consumer{pool: pool, dsn: dsn, cfg: cfg, m: m, log: log.With("consumer", cfg.Consumer), wake: make(chan struct{}, 1)}
}

// Add appends a stage (a free function because methods cannot be generic).
func Add[T any](c *Consumer, src Source[T], h Handler[T]) {
	c.stages = append(c.stages, &typedStage[T]{src: src, h: h})
}

// OnRound registers a callback invoked after every successful round with the
// number of rows handled (the alerts service uses it to wake its notifier).
func (c *Consumer) OnRound(fn func(rows int)) { c.onRound = append(c.onRound, fn) }

// Ready reports whether at least one round has completed successfully.
func (c *Consumer) Ready() bool { return c.ready.Load() }

// Poke requests a round soon (coalesced).
func (c *Consumer) Poke() {
	select {
	case c.wake <- struct{}{}:
	default:
	}
}

// RunOnce performs one round over every stage and returns the rows handled.
func (c *Consumer) RunOnce(ctx context.Context) (int, error) {
	total := 0
	for _, s := range c.stages {
		n, err := s.run(ctx, c)
		if err != nil {
			c.m.Polls.WithLabelValues(c.cfg.Consumer, s.table(), "error").Inc()
			return total, fmt.Errorf("%s/%s: %w", c.cfg.Consumer, s.table(), err)
		}
		c.m.Polls.WithLabelValues(c.cfg.Consumer, s.table(), "ok").Inc()
		c.m.Rows.WithLabelValues(c.cfg.Consumer, s.table()).Add(float64(n))
		total += n
	}
	c.ready.Store(true)
	for _, fn := range c.onRound {
		fn(total)
	}
	return total, nil
}

// Run listens, polls on wake-ups and on the ticker, and drains until a round
// returns no rows. It returns when ctx ends.
func (c *Consumer) Run(ctx context.Context) error {
	go c.listen(ctx) //nolint:gosec // G118: the Background() inside is only for closing the conn after ctx is cancelled
	ticker := time.NewTicker(c.cfg.PollInterval)
	defer ticker.Stop()
	c.Poke()
	backoff := time.Second
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-c.wake:
		case <-ticker.C:
		}
		for {
			n, err := c.RunOnce(ctx)
			if err != nil {
				if ctx.Err() != nil {
					return nil
				}
				c.log.Warn("round failed; backing off", "err", err, "backoff", backoff)
				select {
				case <-time.After(backoff):
				case <-ctx.Done():
					return nil
				}
				backoff = min(backoff*2, 30*time.Second)
				continue
			}
			backoff = time.Second
			if n == 0 {
				break
			}
		}
	}
}

func (c *Consumer) watches(table string) bool {
	for _, s := range c.stages {
		if s.table() == table {
			return true
		}
	}
	return false
}

// listen keeps a dedicated connection subscribed to the ingest channel and
// pokes the loop for notifications about watched tables. It reconnects with
// backoff and pokes on every (re)connect so nothing missed while disconnected
// waits for the ticker.
func (c *Consumer) listen(ctx context.Context) {
	backoff := time.Second
	for ctx.Err() == nil {
		conn, err := pgx.Connect(ctx, c.dsn)
		if err != nil {
			c.log.Warn("listen connect failed", "err", err)
			select {
			case <-time.After(backoff):
			case <-ctx.Done():
				return
			}
			backoff = min(backoff*2, 30*time.Second)
			continue
		}
		backoff = time.Second
		if _, err := conn.Exec(ctx, "LISTEN "+store.NotifyChannel); err != nil {
			c.log.Warn("LISTEN failed", "err", err)
			_ = conn.Close(ctx)
			continue
		}
		c.Poke()
		for ctx.Err() == nil {
			wctx, cancel := context.WithTimeout(ctx, 60*time.Second)
			n, err := conn.WaitForNotification(wctx)
			cancel()
			if err != nil {
				if errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil {
					continue // idle; keep the connection
				}
				break // reconnect
			}
			var note store.Notification
			if json.Unmarshal([]byte(n.Payload), &note) == nil && c.watches(note.Table) {
				c.m.Notifications.WithLabelValues(c.cfg.Consumer).Inc()
				c.Poke()
			}
		}
		_ = conn.Close(context.Background())
	}
}

// typedStage runs one source: poll → handle → watermark, in one transaction.
type typedStage[T any] struct {
	src Source[T]
	h   Handler[T]
}

func (s *typedStage[T]) table() string { return s.src.Table }

func (s *typedStage[T]) run(ctx context.Context, c *Consumer) (n int, err error) {
	tx, err := c.pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("begin: %w", err)
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback(ctx)
			s.h.Reset()
		}
	}()
	q := sqlcgen.New(tx)

	after, err := q.GetWatermark(ctx, sqlcgen.GetWatermarkParams{Consumer: c.cfg.Consumer, Source: s.src.Table})
	if errors.Is(err, pgx.ErrNoRows) {
		after = time.Unix(0, 0).UTC()
	} else if err != nil {
		return 0, fmt.Errorf("get watermark: %w", err)
	}

	rows, err := s.src.Poll(ctx, q, after, c.cfg.Lag.Seconds(), c.cfg.MaxGroups)
	if err != nil {
		return 0, fmt.Errorf("poll: %w", err)
	}

	newMark := after
	if len(rows) == 0 {
		// Nothing in (after, now-lag]: it is safe to advance to the lag boundary.
		boundary, err := q.LagBoundary(ctx, c.cfg.Lag.Seconds())
		if err != nil {
			return 0, fmt.Errorf("lag boundary: %w", err)
		}
		if boundary.After(newMark) {
			newMark = boundary
		}
	} else {
		if err := s.h.Handle(ctx, tx, q, rows); err != nil {
			return 0, fmt.Errorf("handle %d rows: %w", len(rows), err)
		}
		for _, r := range rows {
			if t := s.src.InsertedAt(r); t.After(newMark) {
				newMark = t
			}
		}
	}
	if newMark.After(after) {
		if err := q.SetWatermark(ctx, sqlcgen.SetWatermarkParams{Consumer: c.cfg.Consumer, Source: s.src.Table, InsertedAt: newMark}); err != nil {
			return 0, fmt.Errorf("set watermark: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("commit: %w", err)
	}
	c.m.WatermarkLag.WithLabelValues(c.cfg.Consumer, s.src.Table).Set(time.Since(newMark).Seconds())
	return len(rows), nil
}
