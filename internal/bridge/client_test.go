package bridge

import (
	"context"
	"io"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	telemetryv1 "github.com/smhunt/sumpnet/gen/go/sumpnet/telemetry/v1"
)

// flakyIngest fails the first N streams with Unavailable, then counts rows.
type flakyIngest struct {
	telemetryv1.UnimplementedIngestServiceServer
	failFirst int32
	calls     atomic.Int32
	rows      atomic.Int32
}

func (f *flakyIngest) SubmitReadings(stream telemetryv1.IngestService_SubmitReadingsServer) error {
	n := f.calls.Add(1)
	if n <= f.failFirst {
		return status.Error(codes.Unavailable, "warming up")
	}
	var total uint32
	for {
		req, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		total += uint32(len(req.GetReadings())) //nolint:gosec // test sizes
	}
	f.rows.Add(int32(total)) //nolint:gosec // test sizes
	return stream.SendAndClose(&telemetryv1.SubmitReadingsResponse{Accepted: total - 1, Duplicates: 1})
}

func startServer(t *testing.T, impl telemetryv1.IngestServiceServer) string {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := grpc.NewServer()
	telemetryv1.RegisterIngestServiceServer(srv, impl)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	return lis.Addr().String()
}

func TestClientRetriesAndChunks(t *testing.T) {
	impl := &flakyIngest{failFirst: 2}
	addr := startServer(t, impl)
	m := NewMetrics(prometheus.NewRegistry())
	c, err := Dial(context.Background(), addr, ClientConfig{Chunk: 100, SubmitTimeout: 10 * time.Second, DialTimeout: 5 * time.Second}, m)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()

	rows := make([]*telemetryv1.Reading, 250)
	for i := range rows {
		rows[i] = &telemetryv1.Reading{LevelMm: uint32(i)} //nolint:gosec // test
	}
	n, err := c.SubmitReadings(context.Background(), rows)
	if err != nil {
		t.Fatal(err)
	}
	if n.Accepted != 249 || n.Duplicates != 1 {
		t.Fatalf("counts = %+v", n)
	}
	if impl.calls.Load() != 3 || impl.rows.Load() != 250 {
		t.Fatalf("calls = %d rows = %d, want 3 calls (2 retried) and 250 rows", impl.calls.Load(), impl.rows.Load())
	}
}

func TestClientGivesUpAfterTimeout(t *testing.T) {
	impl := &flakyIngest{failFirst: 1 << 30}
	addr := startServer(t, impl)
	c, err := Dial(context.Background(), addr, ClientConfig{SubmitTimeout: 400 * time.Millisecond, DialTimeout: 5 * time.Second}, NewMetrics(prometheus.NewRegistry()))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	start := time.Now()
	if _, err := c.SubmitReadings(context.Background(), []*telemetryv1.Reading{{}}); err == nil {
		t.Fatal("expected error after retries exhausted")
	}
	if d := time.Since(start); d < 300*time.Millisecond || d > 3*time.Second {
		t.Fatalf("gave up after %v, want ≈ the 400 ms submit timeout", d)
	}
}

func TestDialFailsFast(t *testing.T) {
	start := time.Now()
	_, err := Dial(context.Background(), "127.0.0.1:1", ClientConfig{DialTimeout: 500 * time.Millisecond}, NewMetrics(prometheus.NewRegistry()))
	if err == nil {
		t.Fatal("expected dial error")
	}
	if time.Since(start) > 3*time.Second {
		t.Fatalf("Dial took %v", time.Since(start))
	}
}
