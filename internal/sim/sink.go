package sim

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash"
	"io"

	"github.com/smhunt/sumpnet/internal/chirpstack"
)

// Sink receives events in emission order. Publish is called from the engine
// goroutine, so implementations that talk to the network should buffer.
type Sink interface {
	Publish(ctx context.Context, ev Event) error
	Close(ctx context.Context) error
}

// HashSink folds every event into a SHA-256 over its canonical JSON. Two runs
// with the same seed produce the same Sum regardless of the other sinks.
type HashSink struct {
	h hash.Hash
	n uint64
}

// NewHashSink returns an empty HashSink.
func NewHashSink() *HashSink { return &HashSink{h: sha256.New()} }

// Publish implements Sink.
func (s *HashSink) Publish(_ context.Context, ev Event) error {
	b, err := json.Marshal(ev)
	if err != nil {
		return fmt.Errorf("hash sink: %w", err)
	}
	s.h.Write(b)
	s.h.Write([]byte{'\n'})
	s.n++
	return nil
}

// Close implements Sink.
func (s *HashSink) Close(context.Context) error { return nil }

// Sum returns the hex digest of everything published so far.
func (s *HashSink) Sum() string { return hex.EncodeToString(s.h.Sum(nil)) }

// Count returns the number of events published.
func (s *HashSink) Count() uint64 { return s.n }

// WriterSink writes one JSON line per event: {"topic": ..., "event": <ChirpStack UplinkEvent>}.
type WriterSink struct {
	w        *bufio.Writer
	id       Identity
	segments func(homeIndex int) string
}

// NewWriterSink writes JSONL to w. segmentOf maps a home index to its segment
// ID for the device tags.
func NewWriterSink(w io.Writer, id Identity, segmentOf func(homeIndex int) string) *WriterSink {
	return &WriterSink{w: bufio.NewWriterSize(w, 1<<16), id: id, segments: segmentOf}
}

type line struct {
	Topic string          `json:"topic"`
	Event json.RawMessage `json:"event"`
}

// Publish implements Sink.
func (s *WriterSink) Publish(_ context.Context, ev Event) error {
	body, err := chirpstack.MarshalEvent(ev.ChirpStackEvent(s.id, s.segments(ev.HomeIndex)))
	if err != nil {
		return err
	}
	b, err := json.Marshal(line{Topic: chirpstack.UplinkTopic(s.id.ApplicationID, ev.DevEUI), Event: body})
	if err != nil {
		return fmt.Errorf("writer sink: %w", err)
	}
	if _, err := s.w.Write(b); err != nil {
		return fmt.Errorf("writer sink: %w", err)
	}
	return s.w.WriteByte('\n')
}

// Close implements Sink.
func (s *WriterSink) Close(context.Context) error { return s.w.Flush() }

// MultiSink fans out to several sinks in order and stops at the first error.
type MultiSink []Sink

// Publish implements Sink.
func (m MultiSink) Publish(ctx context.Context, ev Event) error {
	for _, s := range m {
		if err := s.Publish(ctx, ev); err != nil {
			return err
		}
	}
	return nil
}

// Close implements Sink; every sink is closed even if one fails.
func (m MultiSink) Close(ctx context.Context) error {
	var first error
	for _, s := range m {
		if err := s.Close(ctx); err != nil && first == nil {
			first = err
		}
	}
	return first
}
