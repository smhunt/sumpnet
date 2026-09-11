// Command lora-bridge subscribes to ChirpStack's MQTT uplink events, decodes
// the binary payloads with internal/codec and streams them to ingest.
package main

import (
	"context"
	"os"

	"github.com/smhunt/sumpnet/internal/bridge"
	"github.com/smhunt/sumpnet/internal/chirpstack"
	"github.com/smhunt/sumpnet/internal/lorabridge"
	"github.com/smhunt/sumpnet/internal/platform"
)

func main() { os.Exit(platform.Run("lora-bridge", run)) }

func run(ctx context.Context, app *platform.App) error {
	cfg, err := bridge.ConfigFromEnv(chirpstack.UplinkTopicFilter, "lora-bridge")
	if err != nil {
		return err
	}
	return bridge.Run(ctx, app, cfg, lorabridge.Decode)
}
