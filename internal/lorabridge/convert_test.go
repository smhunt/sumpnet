package lorabridge

import (
	"errors"
	"testing"
	"time"

	"github.com/chirpstack/chirpstack/api/go/v4/gw"
	"github.com/chirpstack/chirpstack/api/go/v4/integration"
	"google.golang.org/protobuf/types/known/timestamppb"

	telemetryv1 "github.com/smhunt/sumpnet/gen/go/sumpnet/telemetry/v1"
	"github.com/smhunt/sumpnet/internal/bridge"
	"github.com/smhunt/sumpnet/internal/chirpstack"
	"github.com/smhunt/sumpnet/internal/codec"
)

const (
	app = "17c82e96-be03-4f38-aef3-f83d48582d97"
	dev = "70b3d57ed0000007"
)

var evTime = time.Date(2026, 4, 15, 3, 12, 7, 0, time.UTC)

func event(t *testing.T, u codec.Uplink, mutate func(*integration.UplinkEvent)) (string, []byte) {
	t.Helper()
	port, data, err := codec.Encode(u)
	if err != nil {
		t.Fatal(err)
	}
	ev := &integration.UplinkEvent{
		DeduplicationId: "8c0f3b2e-5b7a-4c1d-9e2f-0123456789ab",
		Time:            timestamppb.New(evTime),
		DeviceInfo:      &integration.DeviceInfo{ApplicationId: app, DevEui: dev},
		FCnt:            1842,
		FPort:           uint32(port),
		Data:            data,
		RxInfo: []*gw.UplinkRxInfo{
			{GatewayId: "a84041ffff1e0001", Rssi: -97, Snr: 6.5},
			{GatewayId: "a84041ffff1e0002", Rssi: -72, Snr: 10.25},
		},
		TxInfo: &gw.UplinkTxInfo{Modulation: &gw.Modulation{Parameters: &gw.Modulation_Lora{Lora: &gw.LoraModulationInfo{SpreadingFactor: 8}}}},
	}
	if mutate != nil {
		mutate(ev)
	}
	b, err := chirpstack.MarshalEvent(ev)
	if err != nil {
		t.Fatal(err)
	}
	return chirpstack.UplinkTopic(app, dev), b
}

func now() time.Time { return time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC) }

func TestDecodeHeartbeat(t *testing.T) {
	topic, body := event(t, &codec.Heartbeat{LevelMM: 1234, TempCentiC: -500, RHPct: 55, BattMV: 3987, CyclesSinceLast: 3, Flags: codec.FlagMainsOK | codec.FlagBackupRan}, nil)
	d, err := Decode(topic, body, now())
	if err != nil {
		t.Fatal(err)
	}
	r := d.Reading
	if r == nil || d.Cycle != nil || d.TimeFallback {
		t.Fatalf("decoded = %+v", d)
	}
	if r.GetLevelMm() != 1234 || r.GetTempC() != -5 || r.GetRhPct() != 55 || r.GetBattMv() != 3987 || r.GetCyclesSinceLast() != 3 {
		t.Errorf("reading = %v", r)
	}
	if !r.GetFlags().GetMainsOk() || !r.GetFlags().GetBackupRan() || r.GetFlags().GetFloatHigh() {
		t.Errorf("flags = %v", r.GetFlags())
	}
	if !r.GetTs().AsTime().Equal(evTime) {
		t.Errorf("ts = %v", r.GetTs().AsTime())
	}
	m := r.GetMeta()
	if m.GetDevEui() != dev || m.GetFCnt() != 1842 || m.GetGatewayId() != "a84041ffff1e0002" || m.GetRssiDbm() != -72 || m.GetSnrDb() != 10.25 || m.GetSpreadingFactor() != 8 {
		t.Errorf("meta = %v (best gateway must be the strongest RSSI)", m)
	}
	if !m.GetReceivedAt().AsTime().Equal(evTime) || m.GetDeduplicationId() == "" {
		t.Errorf("meta time/dedup = %v", m)
	}
}

func TestDecodeCycleEvent(t *testing.T) {
	topic, body := event(t, &codec.CycleEvent{StartOffsetS: 45, RunS: 18, PeakCurrentDA: 63, LevelStartMM: 420, LevelEndMM: 610, PumpID: codec.PumpBackup}, nil)
	d, err := Decode(topic, body, now())
	if err != nil {
		t.Fatal(err)
	}
	c := d.Cycle
	if c == nil {
		t.Fatalf("decoded = %+v", d)
	}
	if !c.GetStartedAt().AsTime().Equal(evTime.Add(-45 * time.Second)) {
		t.Errorf("started_at = %v, want event time − 45 s", c.GetStartedAt().AsTime())
	}
	if c.GetRunS() != 18 || c.GetPeakCurrentA() != 6.3 || c.GetLevelStartMm() != 420 || c.GetLevelEndMm() != 610 || c.GetPumpId() != telemetryv1.PumpId_PUMP_ID_BACKUP {
		t.Errorf("cycle = %v", c)
	}
}

func TestDecodeAlarmAndSummary(t *testing.T) {
	topic, body := event(t, &codec.Alarm{Code: codec.AlarmMainsLost, Value: 500}, nil)
	d, err := Decode(topic, body, now())
	if err != nil || d.Alarm == nil || d.Alarm.GetCode() != telemetryv1.AlarmCode_ALARM_CODE_MAINS_LOST || d.Alarm.GetValue() != 500 || !d.Alarm.GetRaisedAt().AsTime().Equal(evTime) {
		t.Fatalf("alarm = %+v, %v", d, err)
	}
	topic, body = event(t, &codec.StormSummary{Count: 12, WindowS: 900, TotalRunS: 216, MaxPeakCurrentDA: 71, MinLevelMM: 380}, nil)
	d, err = Decode(topic, body, now())
	if err != nil || d.Summary == nil || d.Summary.GetCount() != 12 || d.Summary.GetWindowS() != 900 || d.Summary.GetMaxPeakCurrentA() != 7.1 || !d.Summary.GetWindowEnd().AsTime().Equal(evTime) {
		t.Fatalf("summary = %+v, %v", d, err)
	}
}

func TestDecodeRainGauge(t *testing.T) {
	topic, body := event(t, &codec.RainGauge{TipCount: 1234, MMPerTipUM: 200, IntervalS: 300, BattMV: 3600, Flags: codec.RainCounterReset | codec.RainSensorFault}, nil)
	d, err := Decode(topic, body, now())
	if err != nil || d.RainGauge == nil || d.Reading != nil {
		t.Fatalf("decoded = %+v, %v", d, err)
	}
	r := d.RainGauge
	if r.GetTipCount() != 1234 || r.GetMmPerTip() != 0.2 || r.GetIntervalS() != 300 || r.GetBattMv() != 3600 ||
		!r.GetCounterReset() || !r.GetSensorFault() || !r.GetTs().AsTime().Equal(evTime) || r.GetMeta().GetFCnt() != 1842 {
		t.Errorf("rain gauge = %v", r)
	}
}

func TestTimeFallbacks(t *testing.T) {
	gwTime := evTime.Add(2 * time.Second)
	topic, body := event(t, &codec.Alarm{Code: 1, Value: 1}, func(ev *integration.UplinkEvent) {
		ev.Time = nil
		ev.RxInfo[1].GwTime = timestamppb.New(gwTime)
	})
	d, err := Decode(topic, body, now())
	if err != nil || d.TimeFallback || !d.Alarm.GetRaisedAt().AsTime().Equal(gwTime) {
		t.Fatalf("gateway time not used: %+v %v", d, err)
	}
	topic, body = event(t, &codec.Alarm{Code: 1, Value: 1}, func(ev *integration.UplinkEvent) { ev.Time = nil })
	d, err = Decode(topic, body, now())
	if err != nil || !d.TimeFallback || !d.Alarm.GetRaisedAt().AsTime().Equal(now()) {
		t.Fatalf("receive-time fallback not used: %+v %v", d, err)
	}
}

func TestDrops(t *testing.T) {
	cases := map[string]struct {
		topic string
		body  []byte
	}{}
	topic, body := event(t, &codec.Alarm{Code: 1, Value: 1}, nil)
	cases["bad_topic"] = struct {
		topic string
		body  []byte
	}{"application/x/device/70b3d57ed0000099/event/up", body}
	cases["bad_json"] = struct {
		topic string
		body  []byte
	}{topic, []byte("{not json")}
	_, badPort := event(t, &codec.Alarm{Code: 1, Value: 1}, func(ev *integration.UplinkEvent) { ev.FPort = 9 })
	cases["unknown_port"] = struct {
		topic string
		body  []byte
	}{topic, badPort}
	_, badLen := event(t, &codec.Alarm{Code: 1, Value: 1}, func(ev *integration.UplinkEvent) { ev.Data = []byte{1, 2} })
	cases["bad_payload"] = struct {
		topic string
		body  []byte
	}{topic, badLen}
	for reason, c := range cases {
		_, err := Decode(c.topic, c.body, now())
		var drop *bridge.DropError
		if !errors.As(err, &drop) || drop.Reason != reason {
			t.Errorf("%s: err = %v", reason, err)
		}
	}
}
