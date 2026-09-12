package nodebridge

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/smhunt/sumpnet/internal/bridge"
	"github.com/smhunt/sumpnet/internal/codec"
)

const dev = "70b3d57ed0000099"

func now() time.Time { return time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC) }

func envelope(t *testing.T, u codec.Uplink, fcnt uint32, ts *int64, rssi *int32) []byte {
	t.Helper()
	port, data, err := codec.Encode(u)
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(Envelope{FCnt: fcnt, FPort: port, Data: data, T: ts, RSSI: rssi})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestDecodeEnvelope(t *testing.T) {
	ts := int64(1776222727) // 2026-04-15T03:12:07Z
	rssi := int32(-61)
	body := envelope(t, &codec.CycleEvent{StartOffsetS: 45, RunS: 18, PeakCurrentDA: 63, LevelStartMM: 420, LevelEndMM: 610}, 1842, &ts, &rssi)
	// The documented example bytes must be what the codec produces.
	var env Envelope
	_ = json.Unmarshal(body, &env)
	if got := string(mustJSON(t, env.Data)); got != `"LQASAD8ApAFiAgA="` {
		t.Errorf("data base64 = %s, want the docs/node-mqtt.md example", got)
	}

	d, err := Decode(Topic("70B3D57ED0000099"), body, now())
	if err != nil {
		t.Fatal(err)
	}
	c := d.Cycle
	if c == nil || d.TimeFallback {
		t.Fatalf("decoded = %+v", d)
	}
	want := time.Unix(ts, 0).UTC()
	if !c.GetStartedAt().AsTime().Equal(want.Add(-45 * time.Second)) {
		t.Errorf("started_at = %v", c.GetStartedAt().AsTime())
	}
	m := c.GetMeta()
	if m.GetDevEui() != dev || m.GetFCnt() != 1842 || m.GetGatewayId() != GatewayID || m.GetRssiDbm() != -61 || m.GetSpreadingFactor() != 0 {
		t.Errorf("meta = %v", m)
	}
	if m.GetDeduplicationId() != DedupID(dev, 1842, codec.PortCycleEvent) || m.GetDeduplicationId() == "" {
		t.Errorf("dedup id = %q", m.GetDeduplicationId())
	}
	if !m.GetReceivedAt().AsTime().Equal(want) {
		t.Errorf("received_at = %v", m.GetReceivedAt().AsTime())
	}
}

func TestDecodeRainGaugeEnvelope(t *testing.T) {
	ts := int64(1776222727)
	body := envelope(t, &codec.RainGauge{TipCount: 3, MMPerTipUM: 254, IntervalS: 900, BattMV: 3312, Flags: codec.RainCounterReset}, 7, &ts, nil)
	d, err := Decode(Topic(dev), body, now())
	if err != nil || d.RainGauge == nil || d.TimeFallback {
		t.Fatalf("decoded = %+v, %v", d, err)
	}
	r := d.RainGauge
	if r.GetTipCount() != 3 || r.GetMmPerTip() != 0.254 || r.GetIntervalS() != 900 || !r.GetCounterReset() ||
		r.GetMeta().GetDeduplicationId() != DedupID(dev, 7, codec.PortRainGauge) || !r.GetTs().AsTime().Equal(time.Unix(ts, 0)) {
		t.Errorf("rain gauge = %v", r)
	}
}

func TestDecodeWithoutTimeUsesNow(t *testing.T) {
	body := envelope(t, &codec.Heartbeat{LevelMM: 1234, TempCentiC: 2157, RHPct: 55, BattMV: 3987, CyclesSinceLast: 3, Flags: codec.FlagMainsOK | codec.FlagBackupRan}, 1, nil, nil)
	d, err := Decode(Topic(dev), body, now())
	if err != nil || d.Reading == nil || !d.TimeFallback || !d.Reading.GetTs().AsTime().Equal(now()) {
		t.Fatalf("decoded = %+v, %v", d, err)
	}
	if d.Reading.GetTempC() != 21.57 || !d.Reading.GetFlags().GetBackupRan() {
		t.Errorf("reading = %v", d.Reading)
	}
}

func TestDrops(t *testing.T) {
	good := envelope(t, &codec.Alarm{Code: 1, Value: 1}, 1, nil, nil)
	cases := map[string]struct {
		topic string
		body  []byte
	}{
		"bad_topic":    {"sumpnet/v1/XYZ/up", good},
		"bad_topic_2":  {"application/a/device/70b3d57ed0000099/event/up", good},
		"bad_json":     {Topic(dev), []byte(`{"fcnt":1,"fport":3,"data":"not base64!!"}`)},
		"unknown_port": {Topic(dev), []byte(`{"fcnt":1,"fport":7,"data":"AQEA"}`)},
		"bad_payload":  {Topic(dev), []byte(`{"fcnt":1,"fport":3,"data":"AQE="}`)},
	}
	for name, c := range cases {
		_, err := Decode(c.topic, c.body, now())
		var drop *bridge.DropError
		want := name
		if name == "bad_topic_2" {
			want = "bad_topic"
		}
		if !errors.As(err, &drop) || drop.Reason != want {
			t.Errorf("%s: err = %v", name, err)
		}
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
