package bridge

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

type recorder struct {
	mu      sync.Mutex
	batches [][]int
	acked   []int
	gate    chan struct{}
	err     error
}

func (r *recorder) flush(ctx context.Context, msgs []int) error {
	if r.gate != nil {
		select {
		case <-r.gate:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.err != nil {
		return r.err
	}
	r.batches = append(r.batches, append([]int(nil), msgs...))
	return nil
}

func (r *recorder) ack(i int) func() error {
	return func() error {
		r.mu.Lock()
		defer r.mu.Unlock()
		r.acked = append(r.acked, i)
		return nil
	}
}

func newBatcher(cfg BatchConfig, r *recorder) *Batcher[int] {
	return NewBatcher("test", cfg, NewMetrics(prometheus.NewRegistry()), slog.New(slog.DiscardHandler), r.flush)
}

func TestFlushBySize(t *testing.T) {
	r := &recorder{}
	b := newBatcher(BatchConfig{MaxItems: 3, MaxDelay: time.Hour, QueueDepth: 10}, r)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- b.Run(ctx) }()
	for i := 1; i <= 7; i++ {
		if err := b.Add(ctx, Envelope[int]{Msg: i, Ack: r.ack(i)}); err != nil {
			t.Fatal(err)
		}
	}
	waitFor(t, func() bool { r.mu.Lock(); defer r.mu.Unlock(); return len(r.batches) == 2 })
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	// Drain on cancel flushes the last partial batch and acks everything in order.
	if len(r.batches) != 3 || len(r.batches[2]) != 1 {
		t.Fatalf("batches = %v", r.batches)
	}
	if len(r.acked) != 7 || r.acked[0] != 1 || r.acked[6] != 7 {
		t.Fatalf("acked = %v", r.acked)
	}
}

func TestFlushByDelay(t *testing.T) {
	r := &recorder{}
	b := newBatcher(BatchConfig{MaxItems: 100, MaxDelay: 30 * time.Millisecond, QueueDepth: 10}, r)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = b.Run(ctx) }()
	start := time.Now()
	_ = b.Add(ctx, Envelope[int]{Msg: 1})
	waitFor(t, func() bool { r.mu.Lock(); defer r.mu.Unlock(); return len(r.batches) == 1 })
	if time.Since(start) > 500*time.Millisecond {
		t.Fatalf("delay flush took %v", time.Since(start))
	}
}

func TestAddBlocksWhenFull(t *testing.T) {
	r := &recorder{gate: make(chan struct{})}
	b := newBatcher(BatchConfig{MaxItems: 1, MaxDelay: time.Hour, QueueDepth: 2}, r)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = b.Run(ctx) }()
	// One message in flight in flush (blocked), two in the queue, then Add must block.
	for i := 0; i < 3; i++ {
		if err := b.Add(ctx, Envelope[int]{Msg: i}); err != nil {
			t.Fatal(err)
		}
	}
	actx, acancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer acancel()
	if err := b.Add(actx, Envelope[int]{Msg: 99}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Add on a full queue returned %v, want DeadlineExceeded (blocked)", err)
	}
	close(r.gate)
	waitFor(t, func() bool { r.mu.Lock(); defer r.mu.Unlock(); return len(r.batches) == 3 })
}

func TestFlushErrorIsFatalAndUnacked(t *testing.T) {
	r := &recorder{err: errors.New("ingest down")}
	b := newBatcher(BatchConfig{MaxItems: 1, MaxDelay: time.Hour, QueueDepth: 10}, r)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- b.Run(ctx) }()
	_ = b.Add(ctx, Envelope[int]{Msg: 1, Ack: r.ack(1)})
	select {
	case err := <-done:
		if err == nil || len(r.acked) != 0 {
			t.Fatalf("err = %v, acked = %v", err, r.acked)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return on flush error")
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met in time")
		}
		time.Sleep(5 * time.Millisecond)
	}
}
