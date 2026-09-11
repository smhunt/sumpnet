package chirpstack

import (
	"encoding/base64"
	"encoding/json"
	"testing"
	"time"

	"github.com/chirpstack/chirpstack/api/go/v4/gw"
	"github.com/chirpstack/chirpstack/api/go/v4/integration"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func sampleEvent() *integration.UplinkEvent {
	return &integration.UplinkEvent{
		DeduplicationId: "8c0f3b2e-5b7a-4c1d-9e2f-0123456789ab",
		Time:            timestamppb.New(time.Date(2026, 4, 15, 3, 12, 7, 0, time.UTC)),
		DeviceInfo: &integration.DeviceInfo{
			TenantId:        "52f14cd4-c6f1-4fbd-8f87-4025e1d49242",
			TenantName:      "sumpnet",
			ApplicationId:   "17c82e96-be03-4f38-aef3-f83d48582d97",
			ApplicationName: "sumpnet-sim",
			DeviceName:      "sim-home-07",
			DevEui:          "70b3d57ed0000007",
			Tags:            map[string]string{"sim": "true"},
		},
		DevAddr:   "01a3b007",
		Adr:       true,
		Dr:        2,
		FCnt:      1842,
		FPort:     2,
		Confirmed: false,
		Data:      []byte{0x2d, 0x00, 0x12, 0x00, 0x3f, 0x00, 0xa4, 0x01, 0x62, 0x02, 0x00},
		RxInfo: []*gw.UplinkRxInfo{
			{GatewayId: "a84041ffff1e0001", UplinkId: 391, Rssi: -97, Snr: 6.5, Channel: 3},
			{GatewayId: "a84041ffff1e0002", UplinkId: 391, Rssi: -108, Snr: -2.25, Channel: 3},
		},
		TxInfo: &gw.UplinkTxInfo{
			Frequency: 904500000,
			Modulation: &gw.Modulation{Parameters: &gw.Modulation_Lora{Lora: &gw.LoraModulationInfo{
				Bandwidth:       125000,
				SpreadingFactor: 8,
				CodeRate:        gw.CodeRate_CR_4_5,
			}}},
		},
		RegionConfigId: "us915_0",
	}
}

func TestMarshalEventShape(t *testing.T) {
	ev := sampleEvent()
	b, err := MarshalEvent(ev)
	if err != nil {
		t.Fatal(err)
	}

	var m map[string]any
	if jerr := json.Unmarshal(b, &m); jerr != nil {
		t.Fatalf("output is not JSON: %v\n%s", jerr, b)
	}

	// Fields the bridge relies on, in ChirpStack's lowerCamelCase JSON names.
	if got := m["fCnt"]; got != float64(1842) {
		t.Errorf("fCnt = %v, want 1842", got)
	}
	if got := m["fPort"]; got != float64(2) {
		t.Errorf("fPort = %v, want 2", got)
	}
	if _, present := m["confirmed"]; present {
		t.Errorf("confirmed=false must be omitted (proto3 JSON default), got %v", m["confirmed"])
	}
	data, _ := m["data"].(string)
	raw, err := base64.StdEncoding.DecodeString(data)
	if err != nil || string(raw) != string(ev.Data) {
		t.Errorf("data = %q, want standard base64 of payload (err=%v)", data, err)
	}
	di, _ := m["deviceInfo"].(map[string]any)
	if di["devEui"] != "70b3d57ed0000007" || di["applicationId"] != ev.DeviceInfo.ApplicationId {
		t.Errorf("deviceInfo = %v", di)
	}
	if m["time"] != "2026-04-15T03:12:07Z" {
		t.Errorf("time = %v, want RFC 3339 UTC", m["time"])
	}
	tx, _ := m["txInfo"].(map[string]any)
	lora, _ := tx["modulation"].(map[string]any)["lora"].(map[string]any)
	if lora["spreadingFactor"] != float64(8) || lora["bandwidth"] != float64(125000) || lora["codeRate"] != "CR_4_5" {
		t.Errorf("txInfo.modulation.lora = %v", lora)
	}
	rx, _ := m["rxInfo"].([]any)
	if len(rx) != 2 {
		t.Errorf("rxInfo has %d entries, want 2", len(rx))
	}

	// Compact: no whitespace outside strings.
	if json.Valid(b) && len(b) != len(mustCompact(t, b)) {
		t.Errorf("output is not compact")
	}
}

func TestMarshalEventStable(t *testing.T) {
	a, err := MarshalEvent(sampleEvent())
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 50; i++ {
		b, err := MarshalEvent(sampleEvent())
		if err != nil {
			t.Fatal(err)
		}
		if string(a) != string(b) {
			t.Fatalf("MarshalEvent is not byte-stable:\n%s\n%s", a, b)
		}
	}
}

func TestRoundTrip(t *testing.T) {
	in := sampleEvent()
	in.Confirmed = true
	b, err := MarshalEvent(in)
	if err != nil {
		t.Fatal(err)
	}
	out, err := UnmarshalEvent(b)
	if err != nil {
		t.Fatal(err)
	}
	if !proto.Equal(in, out) {
		t.Fatalf("round trip mismatch:\n in: %v\nout: %v", in, out)
	}
}

func TestUnmarshalTolerates(t *testing.T) {
	// Unknown fields, "+00:00" offsets and omitted defaults must all decode.
	src := `{"deduplicationId":"x","time":"2026-04-15T03:12:07.123456789+00:00","deviceInfo":{"devEui":"70b3d57ed0000007","applicationId":"app"},"fCnt":7,"fPort":3,"confirmed":true,"data":"AvQB","futureField":{"a":1}}`
	ev, err := UnmarshalEvent([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	if ev.FCnt != 7 || ev.FPort != 3 || !ev.Confirmed || string(ev.Data) != "\x02\xf4\x01" {
		t.Errorf("decoded %v", ev)
	}
	if !ev.Time.AsTime().Equal(time.Date(2026, 4, 15, 3, 12, 7, 123456789, time.UTC)) {
		t.Errorf("time = %v", ev.Time.AsTime())
	}
	if _, err := UnmarshalEvent([]byte(`{"fCnt":"not a number"}`)); err == nil {
		t.Error("expected error for malformed event")
	}
}

func TestTopics(t *testing.T) {
	topic := UplinkTopic("17c82e96-be03-4f38-aef3-f83d48582d97", "70B3D57ED0000007")
	want := "application/17c82e96-be03-4f38-aef3-f83d48582d97/device/70b3d57ed0000007/event/up"
	if topic != want {
		t.Errorf("UplinkTopic = %q, want %q", topic, want)
	}
	app, dev, err := ParseUplinkTopic(topic)
	if err != nil || app != "17c82e96-be03-4f38-aef3-f83d48582d97" || dev != "70b3d57ed0000007" {
		t.Errorf("ParseUplinkTopic = %q, %q, %v", app, dev, err)
	}
	for _, bad := range []string{
		"",
		"application/a/device/b/event/join",
		"application/a/device/b/up",
		"application//device/b/event/up",
		"us915_0/gateway/abc/event/up",
	} {
		if _, _, err := ParseUplinkTopic(bad); err == nil {
			t.Errorf("ParseUplinkTopic(%q) should fail", bad)
		}
	}
}

func mustCompact(t *testing.T, b []byte) []byte {
	t.Helper()
	var m any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	out, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return out
}
