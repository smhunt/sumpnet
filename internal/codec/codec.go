// Package codec encodes and decodes the binary LoRaWAN uplink payloads defined
// in prompt_plan.md §5. It is the byte-exact contract shared with the node
// firmware: little-endian, one fixed length per fPort, no trailing bytes.
//
// The package depends on nothing but the standard library and keeps its
// sentinel errors here rather than in internal/domain, so firmware tooling can
// import it in isolation. Decoding is strict: a payload that would not
// round-trip through Encode is rejected, which keeps the firmware and the
// platform honest about the same set of values.
package codec

import (
	"encoding"
	"encoding/binary"
	"errors"
	"fmt"
)

// LoRaWAN fPorts.
const (
	PortHeartbeat    uint8 = 1
	PortCycleEvent   uint8 = 2
	PortAlarm        uint8 = 3
	PortStormSummary uint8 = 4
	PortRainGauge    uint8 = 5
)

// Payload lengths in bytes. US915 DR0 (SF10 @ 125 kHz) caps the application
// payload at 11 bytes, and the 400 ms dwell-time limit yields the same cap, so
// no port may exceed MaxLen.
const (
	LenHeartbeat    = 10
	LenCycleEvent   = 11
	LenAlarm        = 3
	LenStormSummary = 9
	LenRainGauge    = 11

	MaxLen = 11
)

// Sentinel errors. Wrapped errors carry the fPort and lengths involved.
var (
	ErrUnknownPort = errors.New("codec: unknown fPort")
	ErrBadLength   = errors.New("codec: bad payload length")
	ErrBadValue    = errors.New("codec: value out of range")
)

// Flags is the fPort 1 flags byte.
type Flags uint8

// Flag bits.
const (
	FlagMainsOK     Flags = 1 << 0
	FlagFloatHigh   Flags = 1 << 1
	FlagBackupRan   Flags = 1 << 2
	FlagSensorFault Flags = 1 << 3

	flagsMask = FlagMainsOK | FlagFloatHigh | FlagBackupRan | FlagSensorFault
)

// Has reports whether every bit in x is set.
func (f Flags) Has(x Flags) bool { return f&x == x }

// PumpID identifies which pump a cycle event refers to.
type PumpID uint8

// Pump identifiers.
const (
	PumpPrimary PumpID = 0
	PumpBackup  PumpID = 1
)

// AlarmCode is the fPort 3 alarm code.
type AlarmCode uint8

// Alarm codes (prompt_plan.md §5).
const (
	AlarmFloatHigh     AlarmCode = 1
	AlarmMainsLost     AlarmCode = 2
	AlarmDryRun        AlarmCode = 3
	AlarmContinuousRun AlarmCode = 4
	AlarmSensorFault   AlarmCode = 5
)

// RainFlags is the fPort 5 flags byte.
type RainFlags uint8

// Rain gauge flag bits.
const (
	RainCounterReset RainFlags = 1 << 0 // the node rebooted since its previous uplink
	RainSensorFault  RainFlags = 1 << 1

	rainFlagsMask = RainCounterReset | RainSensorFault
)

// Has reports whether every bit in x is set.
func (f RainFlags) Has(x RainFlags) bool { return f&x == x }

// Uplink is the sealed union of decoded payloads. Every implementation also
// implements encoding.BinaryUnmarshaler on its pointer type.
type Uplink interface {
	Port() uint8
	encoding.BinaryMarshaler
	sealed()
}

// Heartbeat is the fPort 1 periodic report (10 bytes).
type Heartbeat struct {
	LevelMM         uint16 // distance sensor → water; smaller means higher water
	TempCentiC      int16  // 0.01 °C
	RHPct           uint8  // %
	BattMV          uint16 // mV
	CyclesSinceLast uint8  // pump cycles since the previous heartbeat (saturates)
	Flags           Flags
	Reserved        uint8 // must be 0
}

// CycleEvent is the fPort 2 pump-cycle report (11 bytes).
type CycleEvent struct {
	StartOffsetS  uint16 // seconds before transmit that the cycle started
	RunS          uint16 // seconds the pump ran
	PeakCurrentDA uint16 // 0.1 A
	LevelStartMM  uint16 // sensor distance when the pump started (higher water = smaller)
	LevelEndMM    uint16 // sensor distance when the pump stopped
	PumpID        PumpID
}

// Alarm is the fPort 3 confirmed alarm (3 bytes).
type Alarm struct {
	Code  AlarmCode
	Value uint16 // code-dependent: level_mm, batt_mv, run_s or raw sensor code
}

// StormSummary is the fPort 4 storm-mode roll-up (9 bytes): once more than 6
// cycles occur within 15 minutes the node stops sending individual fPort 2
// events and reports one of these per window instead.
type StormSummary struct {
	Count            uint8  // cycles in the window (saturates at 255)
	WindowS          uint16 // window length in seconds (900 for firmware v1)
	TotalRunS        uint16
	MaxPeakCurrentDA uint16 // 0.1 A
	MinLevelMM       uint16 // minimum sensor distance = highest water reached
}

// RainGauge is the fPort 5 tipping-bucket report (11 bytes, device kind
// "rain"). The counter is cumulative since boot, so the platform derives
// rainfall from consecutive deltas and a lost uplink loses no rain.
type RainGauge struct {
	TipCount   uint32 // tips since boot
	MMPerTipUM uint16 // µm of rain per tip (200 = 0.2 mm); never 0
	IntervalS  uint16 // seconds since the previous uplink (since boot after a reset)
	BattMV     uint16 // mV
	Flags      RainFlags
}

// MM is the rainfall represented by n tips.
func (r *RainGauge) MM(tips uint32) float64 { return float64(tips) * float64(r.MMPerTipUM) / 1000 }

// Port implements Uplink.
func (*Heartbeat) Port() uint8 { return PortHeartbeat }

// Port implements Uplink.
func (*CycleEvent) Port() uint8 { return PortCycleEvent }

// Port implements Uplink.
func (*Alarm) Port() uint8 { return PortAlarm }

// Port implements Uplink.
func (*StormSummary) Port() uint8 { return PortStormSummary }

// Port implements Uplink.
func (*RainGauge) Port() uint8 { return PortRainGauge }

func (*Heartbeat) sealed()    {}
func (*CycleEvent) sealed()   {}
func (*Alarm) sealed()        {}
func (*StormSummary) sealed() {}
func (*RainGauge) sealed()    {}

// Decode parses a payload received on fPort. The result is one of *Heartbeat,
// *CycleEvent, *Alarm, *StormSummary or *RainGauge.
func Decode(fPort uint8, b []byte) (Uplink, error) {
	var u interface {
		Uplink
		encoding.BinaryUnmarshaler
	}
	switch fPort {
	case PortHeartbeat:
		u = &Heartbeat{}
	case PortCycleEvent:
		u = &CycleEvent{}
	case PortAlarm:
		u = &Alarm{}
	case PortStormSummary:
		u = &StormSummary{}
	case PortRainGauge:
		u = &RainGauge{}
	default:
		return nil, fmt.Errorf("%w: %d", ErrUnknownPort, fPort)
	}
	if err := u.UnmarshalBinary(b); err != nil {
		return nil, err
	}
	return u, nil
}

// Encode serialises u and returns the fPort it must be sent on.
func Encode(u Uplink) (fPort uint8, b []byte, err error) {
	b, err = u.MarshalBinary()
	if err != nil {
		return 0, nil, err
	}
	return u.Port(), b, nil
}

func checkLen(port uint8, want int, b []byte) error {
	if len(b) != want {
		return fmt.Errorf("%w: fPort %d: want %d bytes, got %d", ErrBadLength, port, want, len(b))
	}
	return nil
}

// MarshalBinary implements encoding.BinaryMarshaler.
func (h *Heartbeat) MarshalBinary() ([]byte, error) {
	if h.Flags&^flagsMask != 0 {
		return nil, fmt.Errorf("%w: fPort 1: flags 0x%02x has undefined bits", ErrBadValue, uint8(h.Flags))
	}
	if h.Reserved != 0 {
		return nil, fmt.Errorf("%w: fPort 1: reserved byte is %d, want 0", ErrBadValue, h.Reserved)
	}
	b := make([]byte, LenHeartbeat)
	binary.LittleEndian.PutUint16(b[0:2], h.LevelMM)
	binary.LittleEndian.PutUint16(b[2:4], uint16(h.TempCentiC)) //nolint:gosec // two's-complement wire encoding
	b[4] = h.RHPct
	binary.LittleEndian.PutUint16(b[5:7], h.BattMV)
	b[7] = h.CyclesSinceLast
	b[8] = uint8(h.Flags)
	b[9] = h.Reserved
	return b, nil
}

// UnmarshalBinary implements encoding.BinaryUnmarshaler.
func (h *Heartbeat) UnmarshalBinary(b []byte) error {
	if err := checkLen(PortHeartbeat, LenHeartbeat, b); err != nil {
		return err
	}
	if Flags(b[8])&^flagsMask != 0 {
		return fmt.Errorf("%w: fPort 1: flags 0x%02x has undefined bits", ErrBadValue, b[8])
	}
	if b[9] != 0 {
		return fmt.Errorf("%w: fPort 1: reserved byte is %d, want 0", ErrBadValue, b[9])
	}
	*h = Heartbeat{
		LevelMM:         binary.LittleEndian.Uint16(b[0:2]),
		TempCentiC:      int16(binary.LittleEndian.Uint16(b[2:4])), //nolint:gosec // two's-complement wire encoding
		RHPct:           b[4],
		BattMV:          binary.LittleEndian.Uint16(b[5:7]),
		CyclesSinceLast: b[7],
		Flags:           Flags(b[8]),
		Reserved:        b[9],
	}
	return nil
}

// MarshalBinary implements encoding.BinaryMarshaler.
func (c *CycleEvent) MarshalBinary() ([]byte, error) {
	if c.PumpID > PumpBackup {
		return nil, fmt.Errorf("%w: fPort 2: pump_id %d", ErrBadValue, c.PumpID)
	}
	b := make([]byte, LenCycleEvent)
	binary.LittleEndian.PutUint16(b[0:2], c.StartOffsetS)
	binary.LittleEndian.PutUint16(b[2:4], c.RunS)
	binary.LittleEndian.PutUint16(b[4:6], c.PeakCurrentDA)
	binary.LittleEndian.PutUint16(b[6:8], c.LevelStartMM)
	binary.LittleEndian.PutUint16(b[8:10], c.LevelEndMM)
	b[10] = uint8(c.PumpID)
	return b, nil
}

// UnmarshalBinary implements encoding.BinaryUnmarshaler.
func (c *CycleEvent) UnmarshalBinary(b []byte) error {
	if err := checkLen(PortCycleEvent, LenCycleEvent, b); err != nil {
		return err
	}
	if PumpID(b[10]) > PumpBackup {
		return fmt.Errorf("%w: fPort 2: pump_id %d", ErrBadValue, b[10])
	}
	*c = CycleEvent{
		StartOffsetS:  binary.LittleEndian.Uint16(b[0:2]),
		RunS:          binary.LittleEndian.Uint16(b[2:4]),
		PeakCurrentDA: binary.LittleEndian.Uint16(b[4:6]),
		LevelStartMM:  binary.LittleEndian.Uint16(b[6:8]),
		LevelEndMM:    binary.LittleEndian.Uint16(b[8:10]),
		PumpID:        PumpID(b[10]),
	}
	return nil
}

// MarshalBinary implements encoding.BinaryMarshaler.
func (a *Alarm) MarshalBinary() ([]byte, error) {
	if a.Code < AlarmFloatHigh || a.Code > AlarmSensorFault {
		return nil, fmt.Errorf("%w: fPort 3: alarm code %d", ErrBadValue, a.Code)
	}
	b := make([]byte, LenAlarm)
	b[0] = uint8(a.Code)
	binary.LittleEndian.PutUint16(b[1:3], a.Value)
	return b, nil
}

// UnmarshalBinary implements encoding.BinaryUnmarshaler.
func (a *Alarm) UnmarshalBinary(b []byte) error {
	if err := checkLen(PortAlarm, LenAlarm, b); err != nil {
		return err
	}
	if code := AlarmCode(b[0]); code < AlarmFloatHigh || code > AlarmSensorFault {
		return fmt.Errorf("%w: fPort 3: alarm code %d", ErrBadValue, b[0])
	}
	*a = Alarm{
		Code:  AlarmCode(b[0]),
		Value: binary.LittleEndian.Uint16(b[1:3]),
	}
	return nil
}

// MarshalBinary implements encoding.BinaryMarshaler.
func (s *StormSummary) MarshalBinary() ([]byte, error) {
	b := make([]byte, LenStormSummary)
	b[0] = s.Count
	binary.LittleEndian.PutUint16(b[1:3], s.WindowS)
	binary.LittleEndian.PutUint16(b[3:5], s.TotalRunS)
	binary.LittleEndian.PutUint16(b[5:7], s.MaxPeakCurrentDA)
	binary.LittleEndian.PutUint16(b[7:9], s.MinLevelMM)
	return b, nil
}

// UnmarshalBinary implements encoding.BinaryUnmarshaler.
func (s *StormSummary) UnmarshalBinary(b []byte) error {
	if err := checkLen(PortStormSummary, LenStormSummary, b); err != nil {
		return err
	}
	*s = StormSummary{
		Count:            b[0],
		WindowS:          binary.LittleEndian.Uint16(b[1:3]),
		TotalRunS:        binary.LittleEndian.Uint16(b[3:5]),
		MaxPeakCurrentDA: binary.LittleEndian.Uint16(b[5:7]),
		MinLevelMM:       binary.LittleEndian.Uint16(b[7:9]),
	}
	return nil
}

func checkRain(tipUM uint16, flags RainFlags) error {
	if tipUM == 0 {
		return fmt.Errorf("%w: fPort 5: mm_per_tip_um is 0", ErrBadValue)
	}
	if flags&^rainFlagsMask != 0 {
		return fmt.Errorf("%w: fPort 5: flags 0x%02x has undefined bits", ErrBadValue, uint8(flags))
	}
	return nil
}

// MarshalBinary implements encoding.BinaryMarshaler.
func (r *RainGauge) MarshalBinary() ([]byte, error) {
	if err := checkRain(r.MMPerTipUM, r.Flags); err != nil {
		return nil, err
	}
	b := make([]byte, LenRainGauge)
	binary.LittleEndian.PutUint32(b[0:4], r.TipCount)
	binary.LittleEndian.PutUint16(b[4:6], r.MMPerTipUM)
	binary.LittleEndian.PutUint16(b[6:8], r.IntervalS)
	binary.LittleEndian.PutUint16(b[8:10], r.BattMV)
	b[10] = uint8(r.Flags)
	return b, nil
}

// UnmarshalBinary implements encoding.BinaryUnmarshaler.
func (r *RainGauge) UnmarshalBinary(b []byte) error {
	if err := checkLen(PortRainGauge, LenRainGauge, b); err != nil {
		return err
	}
	v := RainGauge{
		TipCount:   binary.LittleEndian.Uint32(b[0:4]),
		MMPerTipUM: binary.LittleEndian.Uint16(b[4:6]),
		IntervalS:  binary.LittleEndian.Uint16(b[6:8]),
		BattMV:     binary.LittleEndian.Uint16(b[8:10]),
		Flags:      RainFlags(b[10]),
	}
	if err := checkRain(v.MMPerTipUM, v.Flags); err != nil {
		return err
	}
	*r = v
	return nil
}
