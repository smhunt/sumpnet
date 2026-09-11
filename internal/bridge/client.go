package bridge

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/status"

	telemetryv1 "github.com/smhunt/sumpnet/gen/go/sumpnet/telemetry/v1"
)

// Counts is what ingest reported for one submission.
type Counts struct{ Accepted, Duplicates uint32 }

// Client submits batches to IngestService, one stream per batch, retrying
// transient failures. The response is what authorises acknowledging the
// underlying MQTT messages.
type Client struct {
	conn    *grpc.ClientConn
	c       telemetryv1.IngestServiceClient
	m       *Metrics
	chunk   int
	timeout time.Duration
}

// ClientConfig tunes Dial.
type ClientConfig struct {
	Chunk         int           // messages per Send (ingest's MaxRecvMsgSize is 16 MiB; 500 is far below)
	SubmitTimeout time.Duration // overall budget per batch including retries
	DialTimeout   time.Duration // fail fast if the server is unreachable at startup
}

// Dial connects to ingest inside the compose/cluster network (no TLS) and
// waits for the connection to become ready.
func Dial(ctx context.Context, addr string, cfg ClientConfig, m *Metrics) (*Client, error) {
	if cfg.Chunk <= 0 {
		cfg.Chunk = 500
	}
	if cfg.SubmitTimeout <= 0 {
		cfg.SubmitTimeout = 10 * time.Second
	}
	if cfg.DialTimeout <= 0 {
		cfg.DialTimeout = 10 * time.Second
	}
	conn, err := grpc.NewClient(addr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithDefaultServiceConfig(`{"loadBalancingConfig":[{"round_robin":{}}]}`),
		grpc.WithKeepaliveParams(keepalive.ClientParameters{Time: 30 * time.Second, Timeout: 10 * time.Second, PermitWithoutStream: true}),
	)
	if err != nil {
		return nil, fmt.Errorf("bridge: dial %s: %w", addr, err)
	}
	conn.Connect()
	dctx, cancel := context.WithTimeout(ctx, cfg.DialTimeout)
	defer cancel()
	for {
		st := conn.GetState()
		if st.String() == "READY" {
			break
		}
		if !conn.WaitForStateChange(dctx, st) {
			_ = conn.Close()
			return nil, fmt.Errorf("bridge: ingest at %s not reachable within %s (state %s)", addr, cfg.DialTimeout, st)
		}
	}
	return &Client{conn: conn, c: telemetryv1.NewIngestServiceClient(conn), m: m, chunk: cfg.Chunk, timeout: cfg.SubmitTimeout}, nil
}

// Close releases the connection.
func (c *Client) Close() error { return c.conn.Close() }

// SubmitReadings sends heartbeats.
func (c *Client) SubmitReadings(ctx context.Context, rows []*telemetryv1.Reading) (Counts, error) {
	reqs := chunked(rows, c.chunk, func(part []*telemetryv1.Reading) *telemetryv1.SubmitReadingsRequest {
		return &telemetryv1.SubmitReadingsRequest{Readings: part}
	})
	return submit(ctx, c, "readings", c.c.SubmitReadings, reqs, func(r *telemetryv1.SubmitReadingsResponse) Counts {
		return Counts{r.GetAccepted(), r.GetDuplicates()}
	})
}

// SubmitCycleEvents sends pump cycles and storm summaries together.
func (c *Client) SubmitCycleEvents(ctx context.Context, cycles []*telemetryv1.CycleEvent, summaries []*telemetryv1.StormSummary) (Counts, error) {
	var reqs []*telemetryv1.SubmitCycleEventsRequest
	reqs = append(reqs, chunked(cycles, c.chunk, func(part []*telemetryv1.CycleEvent) *telemetryv1.SubmitCycleEventsRequest {
		return &telemetryv1.SubmitCycleEventsRequest{CycleEvents: part}
	})...)
	reqs = append(reqs, chunked(summaries, c.chunk, func(part []*telemetryv1.StormSummary) *telemetryv1.SubmitCycleEventsRequest {
		return &telemetryv1.SubmitCycleEventsRequest{StormSummaries: part}
	})...)
	return submit(ctx, c, "cycle_events", c.c.SubmitCycleEvents, reqs, func(r *telemetryv1.SubmitCycleEventsResponse) Counts {
		return Counts{r.GetAccepted(), r.GetDuplicates()}
	})
}

// SubmitAlarms sends node alarms.
func (c *Client) SubmitAlarms(ctx context.Context, rows []*telemetryv1.Alarm) (Counts, error) {
	reqs := chunked(rows, c.chunk, func(part []*telemetryv1.Alarm) *telemetryv1.SubmitAlarmsRequest {
		return &telemetryv1.SubmitAlarmsRequest{Alarms: part}
	})
	return submit(ctx, c, "alarm_events", c.c.SubmitAlarms, reqs, func(r *telemetryv1.SubmitAlarmsResponse) Counts {
		return Counts{r.GetAccepted(), r.GetDuplicates()}
	})
}

func chunked[T any, Req any](rows []T, n int, wrap func([]T) Req) []Req {
	var out []Req
	for i := 0; i < len(rows); i += n {
		out = append(out, wrap(rows[i:min(i+n, len(rows))]))
	}
	return out
}

// submit opens one client stream per batch and retries transient failures
// with jittered exponential backoff inside the submit timeout.
func submit[Req any, Res any](
	ctx context.Context,
	c *Client,
	kind string,
	open func(context.Context, ...grpc.CallOption) (grpc.ClientStreamingClient[Req, Res], error),
	reqs []*Req,
	counts func(*Res) Counts,
) (Counts, error) {
	if len(reqs) == 0 {
		return Counts{}, nil
	}
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	backoff := 100 * time.Millisecond
	for attempt := 0; ; attempt++ {
		res, err := attemptSubmit(ctx, open, reqs)
		if err == nil {
			n := counts(res)
			c.m.Accepted.WithLabelValues(kind).Add(float64(n.Accepted))
			c.m.Duplicates.WithLabelValues(kind).Add(float64(n.Duplicates))
			return n, nil
		}
		if !retryable(err) || ctx.Err() != nil {
			return Counts{}, fmt.Errorf("bridge: submit %s: %w", kind, err)
		}
		c.m.SubmitRetries.WithLabelValues(kind).Inc()
		wait := backoff + time.Duration(rand.Int64N(int64(backoff))) //nolint:gosec // jitter, not security
		select {
		case <-time.After(wait):
		case <-ctx.Done():
			return Counts{}, fmt.Errorf("bridge: submit %s: %w (last error: %v)", kind, ctx.Err(), err)
		}
		backoff = min(backoff*2, 5*time.Second)
	}
}

func attemptSubmit[Req any, Res any](ctx context.Context, open func(context.Context, ...grpc.CallOption) (grpc.ClientStreamingClient[Req, Res], error), reqs []*Req) (*Res, error) {
	stream, err := open(ctx)
	if err != nil {
		return nil, err
	}
	for _, r := range reqs {
		if err := stream.Send(r); err != nil {
			// The server may have closed the stream with a status; CloseAndRecv surfaces it.
			if res, rerr := stream.CloseAndRecv(); rerr != nil {
				return nil, rerr
			} else {
				return res, nil
			}
		}
	}
	return stream.CloseAndRecv()
}

func retryable(err error) bool {
	switch status.Code(err) {
	case codes.Unavailable, codes.DeadlineExceeded, codes.Aborted, codes.ResourceExhausted:
		return true
	}
	return errors.Is(err, context.DeadlineExceeded)
}
