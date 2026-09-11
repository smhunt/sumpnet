package sim

import (
	"fmt"
	"time"

	"github.com/chirpstack/chirpstack/api/go/v4/gw"
	"github.com/chirpstack/chirpstack/api/go/v4/integration"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// Event is one simulated LoRaWAN uplink, independent of any sink. The hash of
// the run is computed over Events in Seq order, so it is the same whether the
// events went to MQTT, stdout or nowhere.
type Event struct {
	Seq       uint64     `json:"seq"`
	Time      time.Time  `json:"time"`
	HomeIndex int        `json:"home_index"`
	DevEUI    string     `json:"dev_eui"`
	DevAddr   string     `json:"dev_addr"`
	FCnt      uint32     `json:"f_cnt"`
	FPort     uint8      `json:"f_port"`
	Confirmed bool       `json:"confirmed"`
	Payload   []byte     `json:"payload"`
	SF        uint8      `json:"sf"`
	DR        uint8      `json:"dr"`
	FreqHz    uint32     `json:"freq_hz"`
	RSSI      [2]int32   `json:"rssi"`
	SNR       [2]float32 `json:"snr"`
	DedupID   string     `json:"dedup_id"`
}

// Identity is the ChirpStack tenant/application the simulated devices belong to.
type Identity struct {
	TenantID        string
	TenantName      string
	ApplicationID   string
	ApplicationName string
}

// DefaultIdentity is a fixed tenant/application pair for local runs.
func DefaultIdentity() Identity {
	return Identity{
		TenantID:        "52f14cd4-c6f1-4fbd-8f87-4025e1d49242",
		TenantName:      "sumpnet",
		ApplicationID:   "17c82e96-be03-4f38-aef3-f83d48582d97",
		ApplicationName: "sumpnet-sim",
	}
}

// Gateway IDs reported in rxInfo; two gateways always hear every uplink.
var gatewayIDs = [2]string{"a84041ffff1e0001", "a84041ffff1e0002"}

// ChirpStackEvent builds the vendor-shaped integration event for this uplink.
func (e Event) ChirpStackEvent(id Identity, segmentID string) *integration.UplinkEvent {
	rx := make([]*gw.UplinkRxInfo, 2)
	for i := range rx {
		rx[i] = &gw.UplinkRxInfo{
			GatewayId: gatewayIDs[i],
			UplinkId:  uint32(e.Seq % (1 << 24)),
			Rssi:      e.RSSI[i],
			Snr:       e.SNR[i],
			Channel:   uint32((e.FreqHz - 902_300_000) / 200_000),
			Metadata:  map[string]string{"region_common_name": "US915", "region_config_id": "us915_0"},
		}
	}
	return &integration.UplinkEvent{
		DeduplicationId: e.DedupID,
		Time:            timestamppb.New(e.Time),
		DeviceInfo: &integration.DeviceInfo{
			TenantId:          id.TenantID,
			TenantName:        id.TenantName,
			ApplicationId:     id.ApplicationID,
			ApplicationName:   id.ApplicationName,
			DeviceProfileId:   "6f5b0f1e-0000-4000-8000-000000000001",
			DeviceProfileName: "rak4631-house-node-v1",
			DeviceName:        fmt.Sprintf("sim-home-%02d", e.HomeIndex),
			DevEui:            e.DevEUI,
			Tags:              map[string]string{"sim": "true", "segment": segmentID},
		},
		DevAddr:   e.DevAddr,
		Adr:       true,
		Dr:        uint32(e.DR),
		FCnt:      e.FCnt,
		FPort:     uint32(e.FPort),
		Confirmed: e.Confirmed,
		Data:      e.Payload,
		RxInfo:    rx,
		TxInfo: &gw.UplinkTxInfo{
			Frequency: e.FreqHz,
			Modulation: &gw.Modulation{Parameters: &gw.Modulation_Lora{Lora: &gw.LoraModulationInfo{
				Bandwidth:       125_000,
				SpreadingFactor: uint32(e.SF),
				CodeRate:        gw.CodeRate_CR_4_5,
			}}},
		},
		RegionConfigId: "us915_0",
	}
}
