//go:build integration

package watermark

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/smhunt/sumpnet/internal/store"
	"github.com/smhunt/sumpnet/internal/store/sqlcgen"
	"github.com/smhunt/sumpnet/internal/testinfra"
)

type recorder struct {
	mu      sync.Mutex
	batches [][]sqlcgen.AlarmEvent
	failN   int // fail the first N Handle calls
	resets  int
}

func (r *recorder) Handle(_ context.Context, _ pgx.Tx, _ *sqlcgen.Queries, rows []sqlcgen.AlarmEvent) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.failN > 0 {
		r.failN--
		return errors.New("handler boom")
	}
	r.batches = append(r.batches, rows)
	return nil
}

func (r *recorder) Reset() { r.mu.Lock(); r.resets++; r.mu.Unlock() }

func (r *recorder) count() (batches, rows int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, b := range r.batches {
		rows += len(b)
	}
	return len(r.batches), rows
}

var alarmSource = Source[sqlcgen.AlarmEvent]{
	Table: "alarm_events",
	Poll: func(ctx context.Context, q *sqlcgen.Queries, after time.Time, lag float64, maxGroups int32) ([]sqlcgen.AlarmEvent, error) {
		return q.PollAlarmEvents(ctx, sqlcgen.PollAlarmEventsParams{After: after, LagSeconds: lag, MaxGroups: maxGroups})
	},
	InsertedAt: func(a sqlcgen.AlarmEvent) time.Time { return a.InsertedAt },
}

func setup(t *testing.T) (*store.Store, string) {
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
	return st, dsn
}

// insertBatch stores n alarms in one transaction (one inserted_at group).
func insertBatch(t *testing.T, st *store.Store, dev string, base time.Time, fcntFrom, n int) {
	t.Helper()
	rows := make([]store.AlarmEvent, n)
	for i := range rows {
		rows[i] = store.AlarmEvent{DeviceID: dev, RaisedAt: base.Add(time.Duration(fcntFrom+i) * time.Minute), FCnt: int64(fcntFrom + i), Code: 1, Value: 1}
	}
	if _, err := st.InsertAlarmEvents(context.Background(), rows); err != nil {
		t.Fatal(err)
	}
}

func newConsumer(st *store.Store, dsn string, cfg Config) *Consumer {
	return New(st.Pool(), dsn, cfg, NewMetrics(prometheus.NewRegistry()), slog.New(slog.DiscardHandler))
}

func TestGroupCompletePolling(t *testing.T) {
	st, dsn := setup(t)
	ctx := context.Background()
	base := time.Date(2026, 4, 15, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 3; i++ { // three transactions → three inserted_at groups
		insertBatch(t, st, "70b3d57ed0000001", base, i*10, 4)
		time.Sleep(20 * time.Millisecond)
	}
	rec := &recorder{}
	c := newConsumer(st, dsn, Config{Consumer: "test", Lag: 0, MaxGroups: 2, PollInterval: time.Hour})
	Add(c, alarmSource, rec)

	n, err := c.RunOnce(ctx)
	if err != nil || n != 8 {
		t.Fatalf("first round: %d rows, %v; want 8 (two groups of 4)", n, err)
	}
	n, err = c.RunOnce(ctx)
	if err != nil || n != 4 {
		t.Fatalf("second round: %d rows, %v; want the third group", n, err)
	}
	n, err = c.RunOnce(ctx)
	if err != nil || n != 0 {
		t.Fatalf("third round: %d rows, %v; want 0", n, err)
	}
	if b, r := rec.count(); b != 2 || r != 12 {
		t.Fatalf("handler saw %d batches / %d rows, want 2 / 12", b, r)
	}
	if !c.Ready() {
		t.Fatal("consumer not ready after a successful round")
	}
}

func TestLagExcludesFreshRows(t *testing.T) {
	st, dsn := setup(t)
	ctx := context.Background()
	insertBatch(t, st, "70b3d57ed0000002", time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC), 0, 3)
	rec := &recorder{}
	c := newConsumer(st, dsn, Config{Consumer: "test", Lag: 30 * time.Second, MaxGroups: 5, PollInterval: time.Hour})
	Add(c, alarmSource, rec)
	if n, err := c.RunOnce(ctx); err != nil || n != 0 {
		t.Fatalf("rows inside the lag were polled: %d, %v", n, err)
	}
	c.cfg.Lag = 0
	if n, err := c.RunOnce(ctx); err != nil || n != 3 {
		t.Fatalf("after the lag: %d rows, %v; want 3", n, err)
	}
}

func TestHandlerErrorRollsBack(t *testing.T) {
	st, dsn := setup(t)
	ctx := context.Background()
	insertBatch(t, st, "70b3d57ed0000003", time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC), 0, 2)
	rec := &recorder{failN: 1}
	c := newConsumer(st, dsn, Config{Consumer: "test", Lag: 0, MaxGroups: 5, PollInterval: time.Hour})
	Add(c, alarmSource, rec)

	if _, err := c.RunOnce(ctx); err == nil {
		t.Fatal("expected the handler error")
	}
	if _, err := st.Queries().GetWatermark(ctx, sqlcgen.GetWatermarkParams{Consumer: "test", Source: "alarm_events"}); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("watermark must not advance on error; got err=%v", err)
	}
	if rec.resets != 1 {
		t.Fatalf("Reset calls = %d, want 1", rec.resets)
	}
	if n, err := c.RunOnce(ctx); err != nil || n != 2 {
		t.Fatalf("retry: %d rows, %v", n, err)
	}
	// A new consumer resumes from the stored watermark: nothing to redo.
	c2 := newConsumer(st, dsn, Config{Consumer: "test", Lag: 0, MaxGroups: 5, PollInterval: time.Hour})
	Add(c2, alarmSource, &recorder{})
	if n, err := c2.RunOnce(ctx); err != nil || n != 0 {
		t.Fatalf("resume: %d rows, %v; want 0", n, err)
	}
}

func TestNotifyWakes(t *testing.T) {
	st, dsn := setup(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rec := &recorder{}
	c := newConsumer(st, dsn, Config{Consumer: "test", Lag: 0, MaxGroups: 5, PollInterval: time.Hour})
	Add(c, alarmSource, rec)
	done := make(chan error, 1)
	go func() { done <- c.Run(ctx) }()
	deadline := time.Now().Add(10 * time.Second)
	for !c.Ready() {
		if time.Now().After(deadline) {
			t.Fatal("consumer never became ready")
		}
		time.Sleep(20 * time.Millisecond)
	}
	// Give the LISTEN connection a moment, then insert (store.Notify fires on commit).
	time.Sleep(300 * time.Millisecond)
	insertBatch(t, st, "70b3d57ed0000004", time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC), 0, 1)
	deadline = time.Now().Add(5 * time.Second)
	for {
		if _, rows := rec.count(); rows == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("NOTIFY did not wake the consumer within 5 s (poll interval is 1 h)")
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
