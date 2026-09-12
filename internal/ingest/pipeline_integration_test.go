//go:build integration

package ingest_test

import (
	"context"
	"encoding/json"
	"math"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/eclipse/paho.golang/autopaho"
	"github.com/eclipse/paho.golang/paho"

	"github.com/smhunt/sumpnet/internal/codec"
	"github.com/smhunt/sumpnet/internal/nodebridge"
	"github.com/smhunt/sumpnet/internal/store"
	"github.com/smhunt/sumpnet/internal/store/sqlcgen"
	"github.com/smhunt/sumpnet/internal/testinfra"
	"github.com/smhunt/sumpnet/internal/testpipeline"
)

// TestPipelineStorm is the Phase 2 acceptance test: a simulated storm reaches
// the database with zero loss, a replay adds nothing, and the Wi-Fi transport
// carries the same bytes.
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

	ingestAddr := testpipeline.StartIngest(t, st)
	lora := testpipeline.StartLoraBridge(t, broker, ingestAddr)

	// --- 1. A full simulated storm arrives with zero loss ------------------
	engine := testpipeline.NewSim(t, "storm25", 7, 10, 3, 0)
	truth, events := testpipeline.Replay(t, broker, engine, 7)
	want := testpipeline.Expected(events)
	t.Logf("simulator emitted %d events: %+v; true cycles %d", len(events), want, len(truth.TrueCycles))
	got := testpipeline.WaitForCounts(t, q, want, 90*time.Second)

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
	if got.Cycles+got.SummarisedCycles != int64(len(truth.TrueCycles)) {
		t.Errorf("cycle_events %d + summarised %d != %d true cycles", got.Cycles, got.SummarisedCycles, len(truth.TrueCycles))
	}

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
	engine2 := testpipeline.NewSim(t, "storm25", 7, 10, 3, 0)
	_, events2 := testpipeline.Replay(t, broker, engine2, 7)
	if len(events2) != len(events) {
		t.Fatalf("replay emitted %d events, want %d", len(events2), len(events))
	}
	deadline := time.Now().Add(90 * time.Second)
	for testpipeline.Counter(t, lora, "sumpnet_bridge_duplicates_total")+testpipeline.Counter(t, lora, "sumpnet_bridge_accepted_total") < 2*float64(len(events)) {
		if time.Now().After(deadline) {
			t.Fatalf("bridge processed %v accepted + %v duplicates, want %d total", testpipeline.Counter(t, lora, "sumpnet_bridge_accepted_total"), testpipeline.Counter(t, lora, "sumpnet_bridge_duplicates_total"), 2*len(events))
		}
		time.Sleep(500 * time.Millisecond)
	}
	if again := testpipeline.DBCounts(t, q); again != got {
		t.Fatalf("replay changed the tables: %+v -> %+v", got, again)
	}
	if d := testpipeline.Counter(t, lora, "sumpnet_bridge_duplicates_total"); d != float64(len(events)) {
		t.Errorf("duplicates reported = %v, want %d", d, len(events))
	}
	if drops := testpipeline.Counter(t, lora, "sumpnet_bridge_drops_total"); drops != 0 {
		t.Errorf("bridge dropped %v messages", drops)
	}

	// --- 3. The Wi-Fi transport carries the same bytes through mqtt-bridge --
	testpipeline.StartBridge(t, "mqtt-bridge-test", broker, nodebridge.TopicFilter, ingestAddr, nodebridge.Decode)
	pub := publisher(t, broker)
	for _, ev := range events {
		dev := strings.Replace(ev.DevEUI, "70b3d57ed0", "70b3d57ee0", 1)
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
	doubled := testpipeline.Counts{Readings: 2 * want.Readings, Cycles: 2 * want.Cycles, Summaries: 2 * want.Summaries, Alarms: 2 * want.Alarms, SummarisedCycles: 2 * want.SummarisedCycles}
	testpipeline.WaitForCounts(t, q, doubled, 90*time.Second)
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
