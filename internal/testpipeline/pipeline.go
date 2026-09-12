//go:build integration

// Package testpipeline runs the real ingest path in-process for integration
// tests: simulator → Mosquitto → lora-bridge → ingest → Postgres. It lives
// apart from testinfra so packages that testinfra's containers are used by
// (store, …) do not import-cycle through it.
package testpipeline

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"net"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"google.golang.org/grpc"

	telemetryv1 "github.com/smhunt/sumpnet/gen/go/sumpnet/telemetry/v1"
	"github.com/smhunt/sumpnet/internal/bridge"
	"github.com/smhunt/sumpnet/internal/chirpstack"
	"github.com/smhunt/sumpnet/internal/codec"
	"github.com/smhunt/sumpnet/internal/ingest"
	"github.com/smhunt/sumpnet/internal/lorabridge"
	"github.com/smhunt/sumpnet/internal/platform"
	"github.com/smhunt/sumpnet/internal/sim"
	"github.com/smhunt/sumpnet/internal/store"
	"github.com/smhunt/sumpnet/internal/store/sqlcgen"
)

// TestStart is the virtual start time every harness uses.
var TestStart = time.Date(2026, 4, 15, 0, 0, 0, 0, time.UTC)

// Recorder keeps every simulator event in memory.
type Recorder struct{ Events []sim.Event }

// Publish implements sim.Sink.
func (r *Recorder) Publish(_ context.Context, ev sim.Event) error {
	r.Events = append(r.Events, ev)
	return nil
}

// Close implements sim.Sink.
func (r *Recorder) Close(context.Context) error { return nil }

// Counts are the telemetry table counts plus the summarised cycles.
type Counts struct{ Readings, Cycles, Summaries, Alarms, SummarisedCycles, RainGauges int64 }

// DBCounts reads the counts.
func DBCounts(t *testing.T, q *sqlcgen.Queries) Counts {
	t.Helper()
	ctx := context.Background()
	var c Counts
	var err error
	if c.Readings, err = q.CountReadings(ctx); err != nil {
		t.Fatal(err)
	}
	if c.Cycles, err = q.CountCycleEvents(ctx); err != nil {
		t.Fatal(err)
	}
	if c.Summaries, err = q.CountStormSummaries(ctx); err != nil {
		t.Fatal(err)
	}
	if c.Alarms, err = q.CountAlarmEvents(ctx); err != nil {
		t.Fatal(err)
	}
	if c.SummarisedCycles, err = q.SumStormSummaryCycles(ctx); err != nil {
		t.Fatal(err)
	}
	if c.RainGauges, err = q.CountRainGaugeUplinks(ctx); err != nil {
		t.Fatal(err)
	}
	return c
}

// Expected derives the counts a set of simulator events must produce.
func Expected(events []sim.Event) Counts {
	var c Counts
	for _, ev := range events {
		switch ev.FPort {
		case codec.PortHeartbeat:
			c.Readings++
		case codec.PortCycleEvent:
			c.Cycles++
		case codec.PortStormSummary:
			c.Summaries++
			u, _ := codec.Decode(ev.FPort, ev.Payload)
			c.SummarisedCycles += int64(u.(*codec.StormSummary).Count)
		case codec.PortAlarm:
			c.Alarms++
		case codec.PortRainGauge:
			c.RainGauges++
		}
	}
	return c
}

// WaitForCounts polls until the counts equal want.
func WaitForCounts(t *testing.T, q *sqlcgen.Queries, want Counts, timeout time.Duration) Counts {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		got := DBCounts(t, q)
		if got == want {
			return got
		}
		if time.Now().After(deadline) {
			t.Fatalf("counts did not converge: got %+v want %+v", got, want)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// StartIngest serves a real IngestService on a loopback port.
func StartIngest(t *testing.T, st *store.Store) string {
	t.Helper()
	reg := platform.NewApp(platform.Config{Service: "ingest-test"}, slog.New(slog.DiscardHandler)).Metrics
	srv := grpc.NewServer(grpc.InitialWindowSize(256<<10), grpc.InitialConnWindowSize(1<<20))
	cfg := ingest.DefaultConfig()
	cfg.BatchDelay = 50 * time.Millisecond
	telemetryv1.RegisterIngestServiceServer(srv, ingest.New(st, ingest.NewMetrics(reg), cfg, slog.New(slog.DiscardHandler)))
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	return lis.Addr().String()
}

// StartBridge runs bridge.Run with the given decoder until the test ends.
func StartBridge(t *testing.T, name, broker, topic, ingestAddr string, decode bridge.Decoder) *platform.App {
	t.Helper()
	app := platform.NewApp(platform.Config{Service: name}, slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn})))
	cfg := bridge.RunConfig{
		MQTT:   bridge.MQTTConfig{URL: broker, ClientID: name, Topic: topic, ReceiveMax: 1000, SessionExpiry: time.Minute, Log: app.Log},
		Ingest: ingestAddr,
		Client: bridge.ClientConfig{SubmitTimeout: 10 * time.Second, DialTimeout: 5 * time.Second},
		Batch:  bridge.BatchConfig{MaxItems: 200, MaxDelay: 50 * time.Millisecond, QueueDepth: 1000, DrainTimeout: 10 * time.Second},
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- bridge.Run(ctx, app, cfg, decode) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("%s: Run returned %v", name, err)
			}
		case <-time.After(20 * time.Second):
			t.Errorf("%s: Run did not stop", name)
		}
	})
	WaitReady(t, app, done, name)
	return app
}

// StartLoraBridge is StartBridge for ChirpStack events.
func StartLoraBridge(t *testing.T, broker, ingestAddr string) *platform.App {
	return StartBridge(t, "lora-bridge-test", broker, chirpstack.UplinkTopicFilter, ingestAddr, lorabridge.Decode)
}

// WaitReady blocks until app is ready or done reports an early exit.
func WaitReady(t *testing.T, app *platform.App, done <-chan error, name string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for !app.Ready.Ready() {
		select {
		case err := <-done:
			t.Fatalf("%s: exited early: %v", name, err)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s: not ready in time", name)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// Counter sums a counter family across labels from an app's registry.
func Counter(t *testing.T, app *platform.App, name string) float64 {
	t.Helper()
	fams, err := app.Metrics.Gather()
	if err != nil {
		t.Fatal(err)
	}
	var total float64
	for _, f := range fams {
		if f.GetName() != name {
			continue
		}
		for _, m := range f.GetMetric() {
			total += m.GetCounter().GetValue()
		}
	}
	return total
}

// SimRainGauges is how many rain gauge nodes NewSim adds: the pilot's two (§4).
const SimRainGauges = 2

// NewSim builds a simulator engine for a built-in scenario, with the pilot's
// rain gauges.
func NewSim(t *testing.T, scenario string, seed uint64, homes, segments int, duration time.Duration) *sim.Engine {
	t.Helper()
	e, err := sim.New(sim.Config{Seed: seed, Start: TestStart, Homes: homes, Segments: segments, Scenario: sim.Scenarios()[scenario], Duration: duration, RainGauges: SimRainGauges})
	if err != nil {
		t.Fatal(err)
	}
	return e
}

// SeedHomes registers the engine's segments, homes and devices so every
// simulated house node is linked to a home with a pit area, and the rain
// gauges are registered as kind rain (they belong to no home).
func SeedHomes(t *testing.T, st *store.Store, e *sim.Engine) {
	t.Helper()
	ctx := context.Background()
	q := st.Queries()
	for _, s := range e.Segments() {
		if err := q.UpsertSegment(ctx, sqlcgen.UpsertSegmentParams{ID: s.ID, Name: s.Name, Kind: s.Kind.String()}); err != nil {
			t.Fatal(err)
		}
	}
	for _, h := range e.Homes() {
		id := uuid.MustParse(h.HomeID)
		if _, err := q.UpsertHome(ctx, sqlcgen.UpsertHomeParams{ID: id, SegmentID: pgtype.Text{String: h.SegmentID, Valid: true}, PitAreaM2: nullFloat(h.PitAreaM2)}); err != nil {
			t.Fatal(err)
		}
		if _, err := q.UpsertDevice(ctx, sqlcgen.UpsertDeviceParams{DevEui: h.DevEUI, Kind: "house", HomeID: uuid.NullUUID{UUID: id, Valid: true}}); err != nil {
			t.Fatal(err)
		}
	}
	for _, g := range e.RainGauges() {
		if _, err := q.UpsertDevice(ctx, sqlcgen.UpsertDeviceParams{DevEui: g.DevEUI, Kind: sim.DeviceKindRain, Name: pgtype.Text{String: g.Name + " (" + g.Location + ")", Valid: true}}); err != nil {
			t.Fatal(err)
		}
	}
}

// Replay runs the engine unpaced into the broker and returns the truth and
// every emitted event.
func Replay(t *testing.T, broker string, e *sim.Engine, seed uint64) (*sim.Truth, []sim.Event) {
	t.Helper()
	ctx := context.Background()
	mq, err := sim.NewMQTTSink(ctx, sim.MQTTConfig{URL: broker, ClientID: fmt.Sprintf("sim-%d-%d", seed, time.Now().UnixNano()), Publishers: 3, Identity: sim.DefaultIdentity(), SegmentOf: e.SegmentOf})
	if err != nil {
		t.Fatal(err)
	}
	rec := &Recorder{}
	truth, err := e.Run(ctx, sim.MultiSink{mq, rec})
	if err != nil {
		t.Fatal(err)
	}
	if err := mq.Close(ctx); err != nil {
		t.Fatal(err)
	}
	return truth, rec.Events
}

func nullFloat(f float64) sql.NullFloat64 { return sql.NullFloat64{Float64: f, Valid: true} }
