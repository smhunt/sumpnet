// Package nodebridge decodes the Wi-Fi transport: ESP32 nodes publishing the
// codec bytes in a JSON envelope on sumpnet/v1/{dev_eui}/up (docs/node-mqtt.md).
package nodebridge

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
	"google.golang.org/protobuf/types/known/timestamppb"

	telemetryv1 "github.com/smhunt/sumpnet/gen/go/sumpnet/telemetry/v1"
	"github.com/smhunt/sumpnet/internal/bridge"
	"github.com/smhunt/sumpnet/internal/codec"
)

// TopicFilter subscribes to every node.
const TopicFilter = "sumpnet/v1/+/up"

// GatewayID is what the Wi-Fi transport reports as its "gateway".
const GatewayID = "wifi"

var devEUIRe = regexp.MustCompile(`^[0-9a-f]{16}$`)

var dedupNamespace = uuid.NewSHA1(uuid.NameSpaceURL, []byte("https://github.com/smhunt/sumpnet/node-mqtt"))

// Envelope is the JSON message format.
type Envelope struct {
	FCnt  uint32 `json:"fcnt"`
	FPort uint8  `json:"fport"`
	Data  []byte `json:"data"` // base64 in JSON
	T     *int64 `json:"t,omitempty"`
	RSSI  *int32 `json:"rssi,omitempty"`
}

// Topic returns the publish topic for a device.
func Topic(devEUI string) string { return "sumpnet/v1/" + strings.ToLower(devEUI) + "/up" }

// ParseTopic extracts the DevEUI from a node topic.
func ParseTopic(topic string) (string, error) {
	p := strings.Split(topic, "/")
	if len(p) != 4 || p[0] != "sumpnet" || p[1] != "v1" || p[3] != "up" || !devEUIRe.MatchString(p[2]) {
		return "", fmt.Errorf("nodebridge: %q is not a node uplink topic", topic)
	}
	return p[2], nil
}

// DedupID derives a stable id for a node uplink (no network-side dedup id exists).
func DedupID(devEUI string, fcnt uint32, fport uint8) string {
	return uuid.NewSHA1(dedupNamespace, []byte(fmt.Sprintf("%s:%d:%d", devEUI, fcnt, fport))).String()
}

// Decode is the bridge.Decoder for node envelopes.
func Decode(topic string, payload []byte, now time.Time) (bridge.Decoded, error) {
	dev, err := ParseTopic(topic)
	if err != nil {
		return bridge.Decoded{}, &bridge.DropError{Reason: "bad_topic"}
	}
	var env Envelope
	if jerr := json.Unmarshal(payload, &env); jerr != nil {
		return bridge.Decoded{}, &bridge.DropError{Reason: "bad_json"}
	}
	t := now.UTC()
	fallback := true
	if env.T != nil {
		t = time.Unix(*env.T, 0).UTC()
		fallback = false
	}
	u, err := codec.Decode(env.FPort, env.Data)
	if err != nil {
		if errors.Is(err, codec.ErrUnknownPort) {
			return bridge.Decoded{}, &bridge.DropError{Reason: "unknown_port"}
		}
		return bridge.Decoded{}, &bridge.DropError{Reason: "bad_payload"}
	}
	meta := &telemetryv1.UplinkMeta{
		DevEui:          dev,
		FCnt:            env.FCnt,
		ReceivedAt:      timestamppb.New(t),
		DeduplicationId: DedupID(dev, env.FCnt, env.FPort),
		GatewayId:       GatewayID,
	}
	if env.RSSI != nil {
		meta.RssiDbm = *env.RSSI
	}
	d := bridge.FromUplink(u, meta, t)
	d.TimeFallback = fallback
	return d, nil
}
