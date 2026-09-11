// Package lorabridge decodes ChirpStack v4 uplink integration events into
// telemetry.v1 rows. Everything transport-agnostic lives in internal/bridge.
package lorabridge

import (
	"errors"
	"strings"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	telemetryv1 "github.com/smhunt/sumpnet/gen/go/sumpnet/telemetry/v1"
	"github.com/smhunt/sumpnet/internal/bridge"
	"github.com/smhunt/sumpnet/internal/chirpstack"
	"github.com/smhunt/sumpnet/internal/codec"
)

// Decode is the bridge.Decoder for ChirpStack events.
func Decode(topic string, payload []byte, now time.Time) (bridge.Decoded, error) {
	_, topicDev, err := chirpstack.ParseUplinkTopic(topic)
	if err != nil {
		return bridge.Decoded{}, &bridge.DropError{Reason: "bad_topic"}
	}
	ev, err := chirpstack.UnmarshalEvent(payload)
	if err != nil {
		return bridge.Decoded{}, &bridge.DropError{Reason: "bad_json"}
	}
	dev := strings.ToLower(ev.GetDeviceInfo().GetDevEui())
	if dev != topicDev {
		return bridge.Decoded{}, &bridge.DropError{Reason: "bad_topic"}
	}

	// Event time: the network's timestamp, else a gateway's, else receive time.
	var t time.Time
	fallback := false
	switch {
	case ev.GetTime() != nil:
		t = ev.GetTime().AsTime().UTC()
	default:
		for _, rx := range ev.GetRxInfo() {
			if rx.GetGwTime() != nil {
				t = rx.GetGwTime().AsTime().UTC()
				break
			}
		}
		if t.IsZero() {
			t = now.UTC()
			fallback = true
		}
	}

	if ev.GetFPort() > 255 {
		return bridge.Decoded{}, &bridge.DropError{Reason: "unknown_port"}
	}
	u, err := codec.Decode(uint8(ev.GetFPort()), ev.GetData()) //nolint:gosec // range-checked above
	if err != nil {
		if errors.Is(err, codec.ErrUnknownPort) {
			return bridge.Decoded{}, &bridge.DropError{Reason: "unknown_port"}
		}
		return bridge.Decoded{}, &bridge.DropError{Reason: "bad_payload"}
	}

	meta := &telemetryv1.UplinkMeta{
		DevEui:          dev,
		FCnt:            ev.GetFCnt(),
		ReceivedAt:      timestamppb.New(t),
		DeduplicationId: ev.GetDeduplicationId(),
		SpreadingFactor: ev.GetTxInfo().GetModulation().GetLora().GetSpreadingFactor(),
	}
	// Best gateway = strongest RSSI.
	for i, rx := range ev.GetRxInfo() {
		if i == 0 || rx.GetRssi() > meta.RssiDbm {
			meta.GatewayId, meta.RssiDbm, meta.SnrDb = rx.GetGatewayId(), rx.GetRssi(), rx.GetSnr()
		}
	}
	d := bridge.FromUplink(u, meta, t)
	d.TimeFallback = fallback
	return d, nil
}
