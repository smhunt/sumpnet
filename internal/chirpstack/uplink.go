// Package chirpstack holds the MQTT topic conventions and JSON encoding of
// ChirpStack v4 application integration events. It is shared by the simulator
// (which emits ChirpStack-shaped uplinks) and lora-bridge (which consumes real
// ones), so both sides agree on the vendor's schema by construction: events are
// the vendor's own protobuf types serialised with protojson.
package chirpstack

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/chirpstack/chirpstack/api/go/v4/integration"
	"google.golang.org/protobuf/encoding/protojson"
)

// UplinkTopicFilter subscribes to uplink events from every application and
// device (ChirpStack's default event_topic template with event = "up").
const UplinkTopicFilter = "application/+/device/+/event/up"

// UplinkTopic returns the topic ChirpStack publishes uplinks on for a device.
// DevEUIs are 16 lowercase hex characters on the wire.
func UplinkTopic(applicationID, devEUI string) string {
	return "application/" + applicationID + "/device/" + strings.ToLower(devEUI) + "/event/up"
}

// ParseUplinkTopic extracts the application ID and DevEUI from an uplink topic.
func ParseUplinkTopic(topic string) (applicationID, devEUI string, err error) {
	p := strings.Split(topic, "/")
	if len(p) != 6 || p[0] != "application" || p[2] != "device" || p[4] != "event" || p[5] != "up" || p[1] == "" || p[3] == "" {
		return "", "", fmt.Errorf("chirpstack: %q is not an uplink event topic", topic)
	}
	return p[1], p[3], nil
}

// MarshalEvent encodes an UplinkEvent as ChirpStack does with json=true:
// proto3 JSON (lowerCamelCase names, bytes as base64, defaults omitted).
// protojson deliberately randomises whitespace between builds, so the output
// is compacted to keep the simulator's stream byte-stable.
func MarshalEvent(ev *integration.UplinkEvent) ([]byte, error) {
	raw, err := protojson.Marshal(ev)
	if err != nil {
		return nil, fmt.Errorf("chirpstack: marshal uplink event: %w", err)
	}
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		return nil, fmt.Errorf("chirpstack: compact uplink event: %w", err)
	}
	return buf.Bytes(), nil
}

// UnmarshalEvent decodes a ChirpStack uplink event. Unknown fields are
// ignored so newer ChirpStack releases do not break the bridge.
func UnmarshalEvent(b []byte) (*integration.UplinkEvent, error) {
	ev := &integration.UplinkEvent{}
	if err := (protojson.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(b, ev); err != nil {
		return nil, fmt.Errorf("chirpstack: unmarshal uplink event: %w", err)
	}
	return ev, nil
}
