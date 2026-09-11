package ingest

import (
	"fmt"
	"os"
	"strconv"
	"time"
)

// Config tunes the stream handlers.
type Config struct {
	BatchMax     int           // INGEST_BATCH_MAX: rows per store call
	BatchDelay   time.Duration // INGEST_BATCH_DELAY: max time a row waits in a partial batch
	QueueBatches int           // INGEST_QUEUE_BATCHES: batches buffered between Recv and the store
	MaxFuture    time.Duration // INGEST_MAX_FUTURE: reject event times further ahead of now than this
}

// DefaultConfig is used when env vars are absent.
func DefaultConfig() Config {
	return Config{BatchMax: 500, BatchDelay: 250 * time.Millisecond, QueueBatches: 2, MaxFuture: time.Hour}
}

// ConfigFromEnv overlays INGEST_* variables on DefaultConfig.
func ConfigFromEnv() (Config, error) {
	c := DefaultConfig()
	var err error
	if c.BatchMax, err = envInt("INGEST_BATCH_MAX", c.BatchMax); err != nil {
		return c, err
	}
	if c.QueueBatches, err = envInt("INGEST_QUEUE_BATCHES", c.QueueBatches); err != nil {
		return c, err
	}
	if c.BatchDelay, err = envDuration("INGEST_BATCH_DELAY", c.BatchDelay); err != nil {
		return c, err
	}
	if c.MaxFuture, err = envDuration("INGEST_MAX_FUTURE", c.MaxFuture); err != nil {
		return c, err
	}
	if c.BatchMax <= 0 || c.QueueBatches <= 0 || c.BatchDelay <= 0 {
		return c, fmt.Errorf("ingest: batch max, queue and delay must be positive")
	}
	return c, nil
}

func envInt(name string, def int) (int, error) {
	v, ok := os.LookupEnv(name)
	if !ok {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", name, err)
	}
	return n, nil
}

func envDuration(name string, def time.Duration) (time.Duration, error) {
	v, ok := os.LookupEnv(name)
	if !ok {
		return def, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", name, err)
	}
	return d, nil
}

// EnvBool reads a boolean env var with a default (exported for cmd/ingest).
func EnvBool(name string, def bool) (bool, error) {
	v, ok := os.LookupEnv(name)
	if !ok {
		return def, nil
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return false, fmt.Errorf("%s: %w", name, err)
	}
	return b, nil
}
