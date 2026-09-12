package ingest

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	telemetryv1 "github.com/smhunt/sumpnet/gen/go/sumpnet/telemetry/v1"
	"github.com/smhunt/sumpnet/internal/store"
)

// fakeStore records batches; optional gate blocks every insert until released.
type fakeStore struct {
	mu      sync.Mutex
	batches [][]store.Reading
	times   []time.Time
	err     error
	gate    chan struct{}
}

func (f *fakeStore) InsertReadings(ctx context.Context, rows []store.Reading) (store.Result, error) {
	if f.gate != nil {
		select {
		case <-f.gate:
		case <-ctx.Done():
			return store.Result{}, ctx.Err()
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return store.Result{}, f.err
	}
	f.batches = append(f.batches, rows)
	f.times = append(f.times, time.Now())
	dup := 0
	for _, r := range rows {
		if r.FCnt%10 == 9 { // every tenth row is a "duplicate"
			dup++
		}
	}
	return store.Result{Accepted: len(rows) - dup, Duplicates: dup}, nil
}

func (f *fakeStore) InsertCycleEvents(context.Context, []store.CycleEvent) (store.Result, error) {
	return store.Result{}, nil
}
func (f *fakeStore) InsertStormSummaries(context.Context, []store.StormSummary) (store.Result, error) {
	return store.Result{}, nil
}
func (f *fakeStore) InsertAlarmEvents(context.Context, []store.AlarmEvent) (store.Result, error) {
	return store.Result{}, nil
}
func (f *fakeStore) InsertRainGaugeUplinks(context.Context, []store.RainGaugeUplink) (store.Result, error) {
	return store.Result{}, nil
}

// fakeStream feeds requests from a channel and captures the response.
type fakeStream struct {
	grpc.ServerStream
	ctx  context.Context
	in   chan *telemetryv1.SubmitReadingsRequest
	resp *telemetryv1.SubmitReadingsResponse
}

func (f *fakeStream) Context() context.Context { return f.ctx }
func (f *fakeStream) Recv() (*telemetryv1.SubmitReadingsRequest, error) {
	req, ok := <-f.in
	if !ok {
		return nil, io.EOF
	}
	return req, nil
}
func (f *fakeStream) SendAndClose(r *telemetryv1.SubmitReadingsResponse) error {
	f.resp = r
	return nil
}

func newServer(st Store, cfg Config) *Server {
	s := New(st, NewMetrics(prometheus.NewRegistry()), cfg, slog.New(slog.DiscardHandler))
	s.now = func() time.Time { return time.Date(2026, 4, 15, 6, 0, 0, 0, time.UTC) }
	return s
}

func reading(dev string, fcnt uint32) *telemetryv1.Reading {
	// One second per frame: 5000 frames is > MaxFuture (1 h) past the fake now.
	ts := timestamppb.New(time.Date(2026, 4, 15, 6, 0, 0, 0, time.UTC).Add(time.Duration(fcnt) * time.Second))
	return &telemetryv1.Reading{
		Meta:    &telemetryv1.UplinkMeta{DevEui: dev, FCnt: fcnt, ReceivedAt: ts, GatewayId: "gw", RssiDbm: -90, SnrDb: 5, SpreadingFactor: 7},
		Ts:      ts,
		LevelMm: 400, TempC: 18.2, RhPct: 55, BattMv: 4100,
		Flags: &telemetryv1.ReadingFlags{MainsOk: true},
	}
}

func run(t *testing.T, s *Server, feed func(chan<- *telemetryv1.SubmitReadingsRequest)) (*telemetryv1.SubmitReadingsResponse, error) {
	t.Helper()
	st := &fakeStream{ctx: context.Background(), in: make(chan *telemetryv1.SubmitReadingsRequest)}
	done := make(chan error, 1)
	go func() { done <- s.SubmitReadings(st) }()
	feed(st.in)
	close(st.in)
	select {
	case err := <-done:
		return st.resp, err
	case <-time.After(10 * time.Second):
		t.Fatal("handler did not return")
		return nil, nil
	}
}

func TestBatchesBySize(t *testing.T) {
	fs := &fakeStore{}
	s := newServer(fs, Config{BatchMax: 500, BatchDelay: time.Hour, QueueBatches: 2, MaxFuture: time.Hour})
	resp, err := run(t, s, func(in chan<- *telemetryv1.SubmitReadingsRequest) {
		for i := 0; i < 12; i++ { // 12 × 100 = 1200 rows
			req := &telemetryv1.SubmitReadingsRequest{}
			for j := 0; j < 100; j++ {
				req.Readings = append(req.Readings, reading("70b3d57ed0000001", uint32(i*100+j)))
			}
			in <- req
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	sizes := []int{}
	for _, b := range fs.batches {
		sizes = append(sizes, len(b))
	}
	if len(sizes) != 3 || sizes[0] != 500 || sizes[1] != 500 || sizes[2] != 200 {
		t.Fatalf("batch sizes = %v, want [500 500 200]", sizes)
	}
	if resp.Accepted+resp.Duplicates != 1200 || resp.Duplicates != 120 {
		t.Fatalf("response = %+v", resp)
	}
}

func TestBatchesByDelay(t *testing.T) {
	fs := &fakeStore{}
	s := newServer(fs, Config{BatchMax: 500, BatchDelay: 50 * time.Millisecond, QueueBatches: 2, MaxFuture: time.Hour})
	var sentAt time.Time
	_, err := run(t, s, func(in chan<- *telemetryv1.SubmitReadingsRequest) {
		in <- &telemetryv1.SubmitReadingsRequest{Readings: []*telemetryv1.Reading{reading("70b3d57ed0000001", 1), reading("70b3d57ed0000001", 2)}}
		sentAt = time.Now()
		time.Sleep(300 * time.Millisecond) // stream stays open; the age flush must fire
		in <- &telemetryv1.SubmitReadingsRequest{Readings: []*telemetryv1.Reading{reading("70b3d57ed0000001", 3)}}
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(fs.batches) != 2 || len(fs.batches[0]) != 2 {
		t.Fatalf("batches = %d (first %d rows), want 2 with the first holding 2", len(fs.batches), len(fs.batches[0]))
	}
	if d := fs.times[0].Sub(sentAt); d > 250*time.Millisecond {
		t.Fatalf("first batch stored %v after send, want ≈50 ms", d)
	}
}

func TestRejectsAndUnits(t *testing.T) {
	fs := &fakeStore{}
	s := newServer(fs, Config{BatchMax: 10, BatchDelay: time.Hour, QueueBatches: 1, MaxFuture: time.Hour})
	bad := []*telemetryv1.Reading{
		{Ts: timestamppb.Now()}, // no meta
		reading("XYZ", 1),       // bad dev_eui
		{Meta: &telemetryv1.UplinkMeta{DevEui: "70b3d57ed0000001"}}, // no time
		reading("70b3d57ed0000001", 5000),                           // 5000 s ahead → future
	}
	good := reading("70B3D57ED0000002", 7) // uppercase is normalised
	resp, err := run(t, s, func(in chan<- *telemetryv1.SubmitReadingsRequest) {
		in <- &telemetryv1.SubmitReadingsRequest{Readings: append(bad, good)}
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Accepted != 1 || len(fs.batches) != 1 || len(fs.batches[0]) != 1 {
		t.Fatalf("resp %+v, batches %v", resp, fs.batches)
	}
	row := fs.batches[0][0]
	if row.DeviceID != "70b3d57ed0000002" || row.FCnt != 7 || *row.RSSIDBm != -90 || *row.SF != 7 || !row.MainsOK || row.LevelMM != 400 {
		t.Errorf("row = %+v", row)
	}
	if got := counterValue(t, s.m.Rejects, "readings", "bad_dev_eui"); got != 1 {
		t.Errorf("bad_dev_eui rejects = %v", got)
	}
	if got := counterValue(t, s.m.Rejects, "readings", "future"); got != 1 {
		t.Errorf("future rejects = %v", got)
	}
}

func TestStoreErrorIsUnavailable(t *testing.T) {
	fs := &fakeStore{err: errors.New("db down")}
	s := newServer(fs, Config{BatchMax: 10, BatchDelay: time.Hour, QueueBatches: 1, MaxFuture: time.Hour})
	_, err := run(t, s, func(in chan<- *telemetryv1.SubmitReadingsRequest) {
		in <- &telemetryv1.SubmitReadingsRequest{Readings: []*telemetryv1.Reading{reading("70b3d57ed0000001", 1)}}
	})
	if status.Code(err) != codes.Unavailable {
		t.Fatalf("err = %v, want Unavailable", err)
	}
}

func TestBackpressureStopsRecv(t *testing.T) {
	fs := &fakeStore{gate: make(chan struct{})}
	s := newServer(fs, Config{BatchMax: 1, BatchDelay: time.Hour, QueueBatches: 1, MaxFuture: time.Hour})
	st := &fakeStream{ctx: context.Background(), in: make(chan *telemetryv1.SubmitReadingsRequest)}
	done := make(chan error, 1)
	go func() { done <- s.SubmitReadings(st) }()

	// With the store blocked, only a handful of requests can be in flight:
	// one in the store, QueueBatches queued, one held by the batcher, one held
	// by the receiver. The next send must block — that is Recv no longer being called.
	accepted := 0
	for i := 0; i < 20; i++ {
		select {
		case st.in <- &telemetryv1.SubmitReadingsRequest{Readings: []*telemetryv1.Reading{reading("70b3d57ed0000001", uint32(i))}}:
			accepted++
		case <-time.After(200 * time.Millisecond):
			i = 20
		}
	}
	if accepted > 5 {
		t.Fatalf("%d requests accepted while the store was blocked; backpressure is not engaging", accepted)
	}
	close(fs.gate)
	close(st.in)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if st.resp.Accepted != uint32(accepted) {
		t.Fatalf("accepted %d, sent %d", st.resp.Accepted, accepted)
	}
}

func counterValue(t *testing.T, vec *prometheus.CounterVec, labels ...string) float64 {
	t.Helper()
	c, err := vec.GetMetricWithLabelValues(labels...)
	if err != nil {
		t.Fatal(err)
	}
	var m dtoMetric
	if err := c.Write(&m.Metric); err != nil {
		t.Fatal(err)
	}
	return m.GetCounter().GetValue()
}

func TestRainGaugeRow(t *testing.T) {
	s := newServer(&fakeStore{}, Config{BatchMax: 10, BatchDelay: time.Hour, QueueBatches: 1, MaxFuture: time.Hour})
	ts := timestamppb.New(time.Date(2026, 4, 15, 5, 55, 0, 0, time.UTC))
	good := &telemetryv1.RainGaugeReading{
		Meta: &telemetryv1.UplinkMeta{DevEui: "70B3D57ED1000001", FCnt: 12, ReceivedAt: ts, GatewayId: "gw", RssiDbm: -80},
		Ts:   ts, TipCount: 250, MmPerTip: 0.2, IntervalS: 300, BattMv: 3600, CounterReset: true,
	}
	row, why := s.rainGaugeRow(good)
	if why != "" || row.DeviceID != "70b3d57ed1000001" || row.FCnt != 12 || row.TipCount != 250 || row.MMPerTip != 0.2 ||
		row.IntervalS != 300 || row.BattMV != 3600 || !row.CounterReset || row.SensorFault || !row.TS.Equal(ts.AsTime()) || *row.RSSIDBm != -80 {
		t.Fatalf("row = %+v, %q", row, why)
	}
	for name, mutate := range map[string]func(*telemetryv1.RainGaugeReading){
		"zero mm per tip": func(r *telemetryv1.RainGaugeReading) { r.MmPerTip = 0 },
		"huge mm per tip": func(r *telemetryv1.RainGaugeReading) { r.MmPerTip = 1000 },
		"future": func(r *telemetryv1.RainGaugeReading) {
			r.Ts = timestamppb.New(time.Date(2026, 4, 15, 8, 0, 0, 0, time.UTC))
		},
	} {
		bad := proto.Clone(good).(*telemetryv1.RainGaugeReading)
		mutate(bad)
		if _, why := s.rainGaugeRow(bad); why == "" {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, why := s.rainGaugeRow(&telemetryv1.RainGaugeReading{MmPerTip: 0.2}); why != rejNoMeta {
		t.Errorf("no meta: %q", why)
	}
}
