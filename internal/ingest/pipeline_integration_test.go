//go:build integration

package ingest_test

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"net"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/eclipse/paho.golang/autopaho"
	"github.com/eclipse/paho.golang/paho"
	dto "github.com/prometheus/client_model/go"
	"google.golang.org/grpc"

	telemetryv1 "github.com/smhunt/sumpnet/gen/go/sumpnet/telemetry/v1"
	"github.com/smhunt/sumpnet/internal/bridge"
	"github.com/smhunt/sumpnet/internal/chirpstack"
	"github.com/smhunt/sumpnet/internal/codec"
	"github.com/smhunt/sumpnet/internal/ingest"
	"github.com/smhunt/sumpnet/internal/lorabridge"
	"github.com/smhunt/sumpnet/internal/nodebridge"
	"github.com/smhunt/sumpnet/internal/platform"
	"github.com/smhunt/sumpnet/internal/sim"
	"github.com/smhunt/sumpnet/internal/store"
	"github.com/smhunt/sumpnet/internal/store/sqlcgen"
	"github.com/smhunt/sumpnet/internal/testinfra"
)

var testStart = time.Date(2026, 4, 15, 0, 0, 0, 0, time.UTC)

type recorder struct{ events []sim.Event }

func (r *recorder) Publish(_ context.Context, ev sim.Event) error {
	r.events = append(r.events, ev)
	return nil
}
func (r *recorder) Close(context.Context) error { return nil }

type counts struct{ readings, cycles, summaries, alarms, summarisedCycles int64 }

func dbCounts(t *testing.T, q *sqlcgen.Queries) counts {
	t.Helper()
	ctx := context.Background()
	var c counts
	var err error
	if c.readings, err = q.CountReadings(ctx); err != nil {
		t.Fatal(err)
	}
	if c.cycles, err = q.CountCycleEvents(ctx); err != nil {
		t.Fatal(err)
	}
	if c.summaries, err = q.CountStormSummaries(ctx); err != nil {
		t.Fatal(err)
	}
	if c.alarms, err = q.CountAlarmEvents(ctx); err != nil {
		t.Fatal(err)
	}
	if c.summarisedCycles, err = q.SumStormSummaryCycles(ctx); err != nil {
		t.Fatal(err)
	}
	return c
}

func expected(events []sim.Event) counts {
	var c counts
	for _, ev := range events {
		switch ev.FPort {
		case codec.PortHeartbeat:
			c.readings++
		case codec.PortCycleEvent:
			c.cycles++
		case codec.PortStormSummary:
			c.summaries++
			u, _ := codec.Decode(ev.FPort, ev.Payload)
			c.summarisedCycles += int64(u.(*codec.StormSummary).Count)
		case codec.PortAlarm:
			c.alarms++
		}
	}
	return c
}

// waitForCounts polls until the four table counts match want (or the timeout).
func waitForCounts(t *testing.T, q *sqlcgen.Queries, want counts, timeout time.Duration) counts {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var got counts
	for {
		got = dbCounts(t, q)
		if got == want {
			return got
		}
		if time.Now().After(deadline) {
			t.Fatalf("counts did not converge: got %+v want %+v", got, want)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

func startIngest(t *testing.T, st *store.Store) string {
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

func startBridge(t *testing.T, name, broker, topic, ingestAddr string, decode bridge.Decoder) *platform.App {
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
	deadline := time.Now().Add(15 * time.Second)
	for !app.Ready.Ready() {
		select {
		case err := <-done:
			t.Fatalf("%s: Run exited early: %v", name, err)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s: not ready in time", name)
		}
		time.Sleep(50 * time.Millisecond)
	}
	return app
}

func counter(t *testing.T, app *platform.App, name string) float64 {
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
	_ = dto.MetricType_COUNTER
	return total
}

func runSim(t *testing.T, broker string, seed uint64) (*sim.Truth, []sim.Event, *sim.Engine) {
	t.Helper()
	ctx := context.Background()
	e, err := sim.New(sim.Config{Seed: seed, Start: testStart, Homes: 10, Segments: 3, Scenario: sim.Scenarios()["storm25"]})
	if err != nil {
		t.Fatal(err)
	}
	mq, err := sim.NewMQTTSink(ctx, sim.MQTTConfig{URL: broker, ClientID: fmt.Sprintf("sim-%d-%d", seed, time.Now().UnixNano()), Publishers: 3, Identity: sim.DefaultIdentity(), SegmentOf: e.SegmentOf})
	if err != nil {
		t.Fatal(err)
	}
	rec := &recorder{}
	truth, err := e.Run(ctx, sim.MultiSink{mq, rec})
	if err != nil {
		t.Fatal(err)
	}
	if err := mq.Close(ctx); err != nil {
		t.Fatal(err)
	}
	return truth, rec.events, e
}

func TestPipelineStorm(t *testing.T) {
	ctx := context.Background()
	dsn := testinfra.StartPostgres(t)
	broker := testinfra.StartMosquitto(t)
	if err := store.Migrate(ctx, dsn); err != nil {
		t.Fatal(err)
	}
	st, err := store.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	q := st.Queries()

	ingestAddr := startIngest(t, st)
	lora := startBridge(t, "lora-bridge-test", broker, chirpstack.UplinkTopicFilter, ingestAddr, lorabridge.Decode)

	// --- 1. A full simulated storm arrives with zero loss ------------------
	truth, events, _ := runSim(t, broker, 7)
	want := expected(events)
	t.Logf("simulator emitted %d events: %+v; true cycles %d", len(events), want, len(truth.TrueCycles))
	got := waitForCounts(t, q, want, 90*time.Second)

	// No gaps: rows per device across every table == max f_cnt + 1.
	stats, err := q.FCntStatsByDevice(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(stats) != 10 {
		t.Fatalf("%d devices stored, want 10", len(stats))
	}
	for _, s := range stats {
		if s.Rows != s.MaxFCnt+1 {
			t.Errorf("device %s: %d rows but max f_cnt %d (gap)", s.DeviceID, s.Rows, s.MaxFCnt)
		}
	}
	// Truth: individually reported cycles + summarised cycles == every real pump run.
	if got.cycles+got.summarisedCycles != int64(len(truth.TrueCycles)) {
		t.Errorf("cycle_events %d + summarised %d != %d true cycles", got.cycles, got.summarisedCycles, len(truth.TrueCycles))
	}

	// Spot-check one cycle event's arithmetic and units.
	for _, ev := range events {
		if ev.FPort != codec.PortCycleEvent {
			continue
		}
		u, _ := codec.Decode(ev.FPort, ev.Payload)
		ce := u.(*codec.CycleEvent)
		row, rerr := q.GetCycleEvent(ctx, sqlcgen.GetCycleEventParams{DeviceID: ev.DevEUI, FCnt: int64(ev.FCnt)})
		if rerr != nil {
			t.Fatalf("cycle row for %s/%d: %v", ev.DevEUI, ev.FCnt, rerr)
		}
		if !row.StartedAt.Equal(ev.Time.Add(-time.Duration(ce.StartOffsetS) * time.Second)) {
			t.Errorf("started_at = %v, want %v − %ds", row.StartedAt, ev.Time, ce.StartOffsetS)
		}
		if math.Abs(float64(row.PeakCurrentA)-float64(ce.PeakCurrentDA)/10) > 0.001 || !row.ReceivedAt.Equal(ev.Time) {
			t.Errorf("row = %+v, event %+v", row, ce)
		}
		if !row.DedupID.Valid || row.DedupID.UUID.String() != ev.DedupID {
			t.Errorf("dedup id %v, want %s", row.DedupID, ev.DedupID)
		}
		break
	}

	// --- 2. Replaying the same stream produces no duplicates ---------------
	_, events2, _ := runSim(t, broker, 7)
	if len(events2) != len(events) {
		t.Fatalf("replay emitted %d events, want %d", len(events2), len(events))
	}
	deadline := time.Now().Add(90 * time.Second)
	for counter(t, lora, "sumpnet_bridge_duplicates_total")+counter(t, lora, "sumpnet_bridge_accepted_total") < 2*float64(len(events)) {
		if time.Now().After(deadline) {
			t.Fatalf("bridge processed %v accepted + %v duplicates, want %d total", counter(t, lora, "sumpnet_bridge_accepted_total"), counter(t, lora, "sumpnet_bridge_duplicates_total"), 2*len(events))
		}
		time.Sleep(500 * time.Millisecond)
	}
	if again := dbCounts(t, q); again != got {
		t.Fatalf("replay changed the tables: %+v -> %+v", got, again)
	}
	if d := counter(t, lora, "sumpnet_bridge_duplicates_total"); d != float64(len(events)) {
		t.Errorf("duplicates reported = %v, want %d", d, len(events))
	}
	if drops := counter(t, lora, "sumpnet_bridge_drops_total"); drops != 0 {
		t.Errorf("bridge dropped %v messages", drops)
	}

	// --- 3. The Wi-Fi transport carries the same bytes through mqtt-bridge --
	startBridge(t, "mqtt-bridge-test", broker, nodebridge.TopicFilter, ingestAddr, nodebridge.Decode)
	pub := publisher(t, broker)
	for _, ev := range events {
		dev := strings.Replace(ev.DevEUI, "70b3d57ed0", "70b3d57ee0", 1) // distinct Wi-Fi devices
		ts := ev.Time.Unix()
		rssi := int32(-60)
		body, merr := json.Marshal(nodebridge.Envelope{FCnt: ev.FCnt, FPort: ev.FPort, Data: ev.Payload, T: &ts, RSSI: &rssi})
		if merr != nil {
			t.Fatal(merr)
		}
		if _, perr := pub.Publish(ctx, &paho.Publish{QoS: 1, Topic: nodebridge.Topic(dev), Payload: body}); perr != nil {
			t.Fatal(perr)
		}
	}
	doubled := counts{2 * want.readings, 2 * want.cycles, 2 * want.summaries, 2 * want.alarms, 2 * want.summarisedCycles}
	waitForCounts(t, q, doubled, 90*time.Second)
	stats, err = q.FCntStatsByDevice(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(stats) != 20 {
		t.Fatalf("%d devices after the Wi-Fi leg, want 20", len(stats))
	}
	for _, s := range stats {
		if s.Rows != s.MaxFCnt+1 {
			t.Errorf("device %s: %d rows but max f_cnt %d (gap)", s.DeviceID, s.Rows, s.MaxFCnt)
		}
	}
	dev, err := q.GetDevice(ctx, "70b3d57ee0000000")
	if err != nil || dev.HomeID.Valid {
		t.Fatalf("wifi device = %+v, %v (must be auto-registered with home_id NULL)", dev, err)
	}
}

func publisher(t *testing.T, broker string) *autopaho.ConnectionManager {
	t.Helper()
	u, _ := url.Parse(broker)
	ctx := context.Background()
	cm, err := autopaho.NewConnection(ctx, autopaho.ClientConfig{
		ServerUrls: []*url.URL{u}, KeepAlive: 30, CleanStartOnInitialConnection: true,
		ClientConfig: paho.ClientConfig{ClientID: "pipeline-test-pub"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := cm.AwaitConnection(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cm.Disconnect(context.Background()) })
	return cm
}
