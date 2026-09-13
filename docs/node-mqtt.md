# Node MQTT envelope (Wi-Fi transport)

House nodes are ESP32-S3 boards with an SX1262 radio. Deployed nodes speak
LoRaWAN to the gateways and reach the platform through ChirpStack. On the bench,
or for a node that has Wi-Fi and mains power, the **same payload bytes** can be
published straight to the sumpnet Mosquitto broker. `mqtt-bridge` consumes this
topic and feeds `ingest` exactly as `lora-bridge` does for ChirpStack events.

Both paths share one encoder in the firmware: the binary payloads in
`prompt_plan.md` §5, implemented in Go by `internal/codec` (with golden vectors
the firmware tests must reproduce).

## Topic

```
sumpnet/v1/{dev_eui}/up
```

`dev_eui` is 16 lowercase hex characters. A Wi-Fi-only node uses its LoRaWAN
DevEUI if it has one, otherwise a locally administered EUI-64 derived from its
MAC. Publish with QoS 1.

## Payload

UTF-8 JSON, one uplink per message:

```json
{"fcnt": 1842, "fport": 2, "data": "LQASAD8ApAFiAgA=", "t": 1776222727, "rssi": -61}
```

| Field | Type | Required | Meaning |
|---|---|---|---|
| `fcnt` | integer ≥ 0 | yes | Frame counter, strictly increasing per device across all ports (same counter the LoRaWAN stack would use). Reset to 0 on reboot is tolerated: the platform's idempotency key is `(dev_eui, event time, fcnt)`. |
| `fport` | 1–5 | yes | Payload type per §5 (1 heartbeat, 2 cycle event, 3 alarm, 4 storm summary, 5 rain gauge). |
| `data` | base64 (standard, padded) | yes | The codec bytes. Length must match the port (10 / 11 / 3 / 9 / 11). |
| `t` | integer, Unix seconds UTC | no | Event time from the node's NTP-synced clock. If absent the bridge uses its receive time and increments `sumpnet_bridge_time_fallback_total`; send it whenever the clock is synced. |
| `rssi` | integer dBm | no | Wi-Fi RSSI, stored as `rssi_dbm` with `gateway_id = "wifi"`. |

Unknown fields are ignored. Anything else — bad base64, wrong length for the
port, unknown port, malformed JSON — is dropped with a `sumpnet_bridge_drops_total{reason}`
counter and a debug log line; the bridge never disconnects over a bad message.

## Semantics

- **Alarms (fport 3) are not confirmed** on this transport; QoS 1 gives
  at-least-once delivery instead. Duplicates are absorbed by the database key.
- The bridge does not acknowledge the MQTT message until `ingest` has committed
  the row, so a bridge crash means redelivery, not loss.
- Storm mode (§5) applies unchanged: the node decides when to roll cycles into
  fport 4 summaries, regardless of transport.

## Testing a node by hand

```bash
# heartbeat: level 1234 mm, 21.57 °C, 55 %, 3987 mV, 3 cycles, mains_ok|backup_ran
mosquitto_pub -h dev.ecoworks.ca -p 3133 -q 1 -t sumpnet/v1/70b3d57ed0000099/up \
  -m '{"fcnt":1,"fport":1,"data":"0gRtCDeTDwMFAA=="}'
```

The golden vectors in `internal/codec/codec_test.go` are the reference bytes for
every port.
