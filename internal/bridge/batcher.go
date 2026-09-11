// Package bridge is what lora-bridge and mqtt-bridge share: an MQTT consumer
// with manual acknowledgements, batchers with a bounded queue, and a retrying
// gRPC client for IngestService. A message is acknowledged to the broker only
// after ingest has confirmed its batch, so a crash means redelivery, not loss.
package bridge

import (
	"context"
	"fmt"
	"log/slog"
	"time"
)

// Envelope is one message plus the function that acknowledges it upstream.
type Envelope[T any] struct {
	Msg T
	Ack func() error
}

// BatchConfig tunes a Batcher.
type BatchConfig struct {
	MaxItems     int           // flush when this many messages are pending
	MaxDelay     time.Duration // or when the oldest pending message is this old
	QueueDepth   int           // Add blocks when this many messages are queued
	DrainTimeout time.Duration // time allowed to flush pending messages on shutdown
}

// Batcher groups envelopes into batches and hands them to flush in order.
type Batcher[T any] struct {
	kind  string
	cfg   BatchConfig
	m     *Metrics
	log   *slog.Logger
	in    chan Envelope[T]
	flush func(context.Context, []T) error
}

// NewBatcher builds a Batcher; Run must be started for Add to make progress.
func NewBatcher[T any](kind string, cfg BatchConfig, m *Metrics, log *slog.Logger, flush func(context.Context, []T) error) *Batcher[T] {
	if cfg.MaxItems <= 0 {
		cfg.MaxItems = 500
	}
	if cfg.MaxDelay <= 0 {
		cfg.MaxDelay = 100 * time.Millisecond
	}
	if cfg.QueueDepth <= 0 {
		cfg.QueueDepth = 2000
	}
	if cfg.DrainTimeout <= 0 {
		cfg.DrainTimeout = 10 * time.Second
	}
	return &Batcher[T]{kind: kind, cfg: cfg, m: m, log: log.With("batcher", kind), in: make(chan Envelope[T], cfg.QueueDepth), flush: flush}
}

// Add queues one envelope. It blocks while the queue is full — that is the
// backpressure that eventually stalls the MQTT client — or until ctx ends.
func (b *Batcher[T]) Add(ctx context.Context, e Envelope[T]) error {
	select {
	case b.in <- e:
		b.m.QueueDepth.WithLabelValues(b.kind).Set(float64(len(b.in)))
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Run flushes batches until ctx is cancelled, then drains what is queued and
// returns. A flush error is fatal: the batch stays unacknowledged and the
// process restarts to let the broker redeliver.
func (b *Batcher[T]) Run(ctx context.Context) error {
	var cur []Envelope[T]
	var timer *time.Timer
	var due <-chan time.Time
	stopTimer := func() {
		if timer != nil {
			timer.Stop()
			timer, due = nil, nil
		}
	}
	doFlush := func(fctx context.Context) error {
		if len(cur) == 0 {
			return nil
		}
		batch := cur
		cur = nil
		stopTimer()
		return b.submit(fctx, batch)
	}
	for {
		select {
		case e := <-b.in:
			b.m.QueueDepth.WithLabelValues(b.kind).Set(float64(len(b.in)))
			if cur == nil {
				timer = time.NewTimer(b.cfg.MaxDelay)
				due = timer.C
			}
			cur = append(cur, e)
			if len(cur) >= b.cfg.MaxItems {
				if err := doFlush(ctx); err != nil {
					return err
				}
			}
		case <-due:
			if err := doFlush(ctx); err != nil {
				return err
			}
		case <-ctx.Done():
			// Drain: take whatever is queued and flush with a bounded context.
			for done := false; !done; {
				select {
				case e := <-b.in:
					cur = append(cur, e)
				default:
					done = true
				}
			}
			dctx, cancel := context.WithTimeout(context.Background(), b.cfg.DrainTimeout)
			defer cancel()
			for len(cur) > 0 {
				n := min(len(cur), b.cfg.MaxItems)
				batch := cur[:n]
				cur = cur[n:]
				if err := b.submit(dctx, batch); err != nil {
					return fmt.Errorf("drain: %w", err)
				}
			}
			return nil
		}
	}
}

func (b *Batcher[T]) submit(ctx context.Context, batch []Envelope[T]) error {
	msgs := make([]T, len(batch))
	for i, e := range batch {
		msgs[i] = e.Msg
	}
	start := time.Now()
	err := b.flush(ctx, msgs)
	b.m.SubmitSeconds.WithLabelValues(b.kind).Observe(time.Since(start).Seconds())
	b.m.BatchRows.WithLabelValues(b.kind).Observe(float64(len(batch)))
	if err != nil {
		return fmt.Errorf("batcher %s: flush %d messages: %w", b.kind, len(batch), err)
	}
	for _, e := range batch {
		if e.Ack != nil {
			if aerr := e.Ack(); aerr != nil {
				b.log.Debug("ack failed (message will be redelivered)", "err", aerr)
			}
		}
	}
	return nil
}
