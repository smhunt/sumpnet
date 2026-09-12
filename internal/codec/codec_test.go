package codec

import (
	"bytes"
	"encoding/hex"
	"errors"
	"math/rand/v2"
	"reflect"
	"strings"
	"testing"
)

// golden holds hand-computed byte vectors. Each comment shows the derivation
// so Phase 7 firmware tests can copy the same table into C++.
var golden = []struct {
	name string
	port uint8
	hex  string
	want Uplink
}{
	{
		name: "heartbeat typical",
		port: PortHeartbeat,
		// level 1234 = 0x04D2 → d2 04 | temp 2157 (21.57 °C) = 0x086D → 6d 08 |
		// rh 55 = 0x37 | batt 3987 = 0x0F93 → 93 0f | cycles 3 |
		// flags 0x05 = mains_ok|backup_ran | reserved 0
		hex:  "d2 04 6d 08 37 93 0f 03 05 00",
		want: &Heartbeat{LevelMM: 1234, TempCentiC: 2157, RHPct: 55, BattMV: 3987, CyclesSinceLast: 3, Flags: FlagMainsOK | FlagBackupRan},
	},
	{
		name: "heartbeat negative temperature",
		port: PortHeartbeat,
		// level 300 = 0x012C → 2c 01 | temp −500 (−5.00 °C) = int16 0xFE0C → 0c fe |
		// rh 70 = 0x46 | batt 3600 = 0x0E10 → 10 0e | cycles 0 | flags 0x02 float_high | reserved 0
		hex:  "2c 01 0c fe 46 10 0e 00 02 00",
		want: &Heartbeat{LevelMM: 300, TempCentiC: -500, RHPct: 70, BattMV: 3600, Flags: FlagFloatHigh},
	},
	{
		name: "cycle event primary",
		port: PortCycleEvent,
		// offset 45 → 2d 00 | run 18 → 12 00 | peak 63 (6.3 A) → 3f 00 |
		// start 420 = 0x01A4 → a4 01 | end 610 = 0x0262 → 62 02 | pump 0
		hex:  "2d 00 12 00 3f 00 a4 01 62 02 00",
		want: &CycleEvent{StartOffsetS: 45, RunS: 18, PeakCurrentDA: 63, LevelStartMM: 420, LevelEndMM: 610, PumpID: PumpPrimary},
	},
	{
		name: "cycle event boundary values backup",
		port: PortCycleEvent,
		// offset 65535 → ff ff | run 10000 = 0x2710 → 10 27 | peak 0 | start 0 | end 65535 | pump 1
		hex:  "ff ff 10 27 00 00 00 00 ff ff 01",
		want: &CycleEvent{StartOffsetS: 65535, RunS: 10000, LevelEndMM: 65535, PumpID: PumpBackup},
	},
	{
		name: "alarm mains lost",
		port: PortAlarm,
		// code 2 | value 500 = 0x01F4 → f4 01
		hex:  "02 f4 01",
		want: &Alarm{Code: AlarmMainsLost, Value: 500},
	},
	{
		name: "alarm float high",
		port: PortAlarm,
		// code 1 | value 350 = 0x015E → 5e 01
		hex:  "01 5e 01",
		want: &Alarm{Code: AlarmFloatHigh, Value: 350},
	},
	{
		name: "storm summary",
		port: PortStormSummary,
		// count 12 = 0x0c | window 900 = 0x0384 → 84 03 | total_run 216 = 0xD8 → d8 00 |
		// max_peak 71 = 0x47 → 47 00 | min_level 380 = 0x017C → 7c 01
		hex:  "0c 84 03 d8 00 47 00 7c 01",
		want: &StormSummary{Count: 12, WindowS: 900, TotalRunS: 216, MaxPeakCurrentDA: 71, MinLevelMM: 380},
	},
	{
		name: "rain gauge while tipping",
		port: PortRainGauge,
		// tips 1234 = 0x000004D2 → d2 04 00 00 | 200 µm/tip = 0x00C8 → c8 00 |
		// interval 300 s = 0x012C → 2c 01 | batt 3600 = 0x0E10 → 10 0e | flags 0
		hex:  "d2 04 00 00 c8 00 2c 01 10 0e 00",
		want: &RainGauge{TipCount: 1234, MMPerTipUM: 200, IntervalS: 300, BattMV: 3600},
	},
	{
		name: "rain gauge after reboot",
		port: PortRainGauge,
		// tips 3 → 03 00 00 00 | 254 µm/tip (0.01 in) = 0x00FE → fe 00 |
		// interval 900 = 0x0384 → 84 03 | batt 3312 = 0x0CF0 → f0 0c | flags 0x01 counter_reset
		hex:  "03 00 00 00 fe 00 84 03 f0 0c 01",
		want: &RainGauge{TipCount: 3, MMPerTipUM: 254, IntervalS: 900, BattMV: 3312, Flags: RainCounterReset},
	},
	{
		name: "rain gauge boundary values sensor fault",
		port: PortRainGauge,
		// tips 4294967295 → ff ff ff ff | 200 → c8 00 | interval 65535 → ff ff |
		// batt 0 → 00 00 | flags 0x03 counter_reset|sensor_fault
		hex:  "ff ff ff ff c8 00 ff ff 00 00 03",
		want: &RainGauge{TipCount: 4294967295, MMPerTipUM: 200, IntervalS: 65535, Flags: RainCounterReset | RainSensorFault},
	},
}

func mustHex(t testing.TB, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(strings.ReplaceAll(s, " ", ""))
	if err != nil {
		t.Fatalf("bad hex %q: %v", s, err)
	}
	return b
}

func TestGoldenVectors(t *testing.T) {
	for _, tc := range golden {
		t.Run(tc.name, func(t *testing.T) {
			b := mustHex(t, tc.hex)

			got, err := Decode(tc.port, b)
			if err != nil {
				t.Fatalf("Decode: %v", err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("Decode = %+v, want %+v", got, tc.want)
			}

			port, enc, err := Encode(tc.want)
			if err != nil {
				t.Fatalf("Encode: %v", err)
			}
			if port != tc.port {
				t.Errorf("Encode port = %d, want %d", port, tc.port)
			}
			if !bytes.Equal(enc, b) {
				t.Errorf("Encode = % x, want % x", enc, b)
			}
		})
	}
}

func TestLengthsWithinAirtimeBudget(t *testing.T) {
	for _, l := range []int{LenHeartbeat, LenCycleEvent, LenAlarm, LenStormSummary, LenRainGauge} {
		if l > MaxLen {
			t.Errorf("payload length %d exceeds the %d-byte US915 DR0 limit", l, MaxLen)
		}
	}
}

func TestDecodeErrors(t *testing.T) {
	lens := map[uint8]int{PortHeartbeat: LenHeartbeat, PortCycleEvent: LenCycleEvent, PortAlarm: LenAlarm, PortStormSummary: LenStormSummary, PortRainGauge: LenRainGauge}
	for port, want := range lens {
		for _, n := range []int{0, want - 1, want + 1} {
			if _, err := Decode(port, make([]byte, n)); !errors.Is(err, ErrBadLength) {
				t.Errorf("Decode(port %d, %d bytes) err = %v, want ErrBadLength", port, n, err)
			}
		}
	}
	for _, port := range []uint8{0, 6, 200, 255} {
		if _, err := Decode(port, []byte{1, 2, 3}); !errors.Is(err, ErrUnknownPort) {
			t.Errorf("Decode(port %d) err = %v, want ErrUnknownPort", port, err)
		}
	}

	bad := []struct {
		name string
		port uint8
		hex  string
	}{
		{"heartbeat undefined flag bit", PortHeartbeat, "00 00 00 00 00 00 00 00 10 00"},
		{"heartbeat reserved nonzero", PortHeartbeat, "00 00 00 00 00 00 00 00 00 01"},
		{"cycle pump_id 2", PortCycleEvent, "00 00 00 00 00 00 00 00 00 00 02"},
		{"alarm code 0", PortAlarm, "00 00 00"},
		{"alarm code 6", PortAlarm, "06 00 00"},
		{"rain undefined flag bit", PortRainGauge, "00 00 00 00 c8 00 2c 01 10 0e 04"},
		{"rain zero mm per tip", PortRainGauge, "01 00 00 00 00 00 2c 01 10 0e 00"},
	}
	for _, tc := range bad {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Decode(tc.port, mustHex(t, tc.hex)); !errors.Is(err, ErrBadValue) {
				t.Errorf("err = %v, want ErrBadValue", err)
			}
		})
	}
}

func TestEncodeRejectsBadValues(t *testing.T) {
	for _, u := range []Uplink{
		&Heartbeat{Flags: 1 << 7},
		&Heartbeat{Reserved: 1},
		&CycleEvent{PumpID: 2},
		&Alarm{Code: 0},
		&Alarm{Code: 6},
		&RainGauge{MMPerTipUM: 0},
		&RainGauge{MMPerTipUM: 200, Flags: 1 << 2},
	} {
		if _, _, err := Encode(u); !errors.Is(err, ErrBadValue) {
			t.Errorf("Encode(%+v) err = %v, want ErrBadValue", u, err)
		}
	}
}

// randomUplink builds a valid uplink of each kind from a seeded generator.
func randomUplink(r *rand.Rand, kind int) Uplink {
	u16 := func() uint16 { return uint16(r.Uint32()) }
	u8 := func() uint8 { return uint8(r.Uint32()) }
	switch kind % 5 {
	case 0:
		return &Heartbeat{LevelMM: u16(), TempCentiC: int16(u16()), RHPct: u8(), BattMV: u16(), CyclesSinceLast: u8(), Flags: Flags(u8()) & flagsMask}
	case 1:
		return &CycleEvent{StartOffsetS: u16(), RunS: u16(), PeakCurrentDA: u16(), LevelStartMM: u16(), LevelEndMM: u16(), PumpID: PumpID(u8() % 2)}
	case 2:
		return &Alarm{Code: AlarmCode(1 + u8()%5), Value: u16()}
	case 3:
		return &StormSummary{Count: u8(), WindowS: u16(), TotalRunS: u16(), MaxPeakCurrentDA: u16(), MinLevelMM: u16()}
	default:
		return &RainGauge{TipCount: r.Uint32(), MMPerTipUM: max(1, u16()), IntervalS: u16(), BattMV: u16(), Flags: RainFlags(u8()) & rainFlagsMask}
	}
}

func TestRoundTrip(t *testing.T) {
	r := rand.New(rand.NewPCG(42, 0))
	for i := 0; i < 5000; i++ {
		in := randomUplink(r, i)
		port, b, err := Encode(in)
		if err != nil {
			t.Fatalf("Encode(%+v): %v", in, err)
		}
		out, err := Decode(port, b)
		if err != nil {
			t.Fatalf("Decode(% x): %v", b, err)
		}
		if !reflect.DeepEqual(in, out) {
			t.Fatalf("round trip: in %+v, out %+v", in, out)
		}
	}
}

func FuzzDecode(f *testing.F) {
	for _, tc := range golden {
		f.Add(tc.port, mustHex(f, tc.hex))
	}
	f.Add(uint8(0), []byte{})
	f.Add(PortHeartbeat, []byte{1})
	f.Fuzz(func(t *testing.T, port uint8, b []byte) {
		u, err := Decode(port, b)
		if err != nil {
			return
		}
		gotPort, out, err := Encode(u)
		if err != nil {
			t.Fatalf("Decode accepted % x on port %d but Encode failed: %v", b, port, err)
		}
		if gotPort != port || !bytes.Equal(out, b) {
			t.Fatalf("not byte-exact: in port %d % x, out port %d % x", port, b, gotPort, out)
		}
	})
}
