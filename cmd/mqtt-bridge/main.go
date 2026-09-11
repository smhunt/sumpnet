// Command mqtt-bridge consumes uplinks from ESP32 nodes on the Wi-Fi
// transport (sumpnet/v1/{dev_eui}/up JSON envelopes, see docs/node-mqtt.md)
// and streams them to ingest.
package main

import (
	"context"
	"os"

	"github.com/smhunt/sumpnet/internal/bridge"
	"github.com/smhunt/sumpnet/internal/nodebridge"
	"github.com/smhunt/sumpnet/internal/platform"
)

func main() { os.Exit(platform.Run("mqtt-bridge", run)) }

func run(ctx context.Context, app *platform.App) error {
	cfg, err := bridge.ConfigFromEnv(nodebridge.TopicFilter, "mqtt-bridge")
	if err != nil {
		return err
	}
	return bridge.Run(ctx, app, cfg, nodebridge.Decode)
}
