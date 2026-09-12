// Command simulator replays a neighbourhood of sump pumps through a rainfall
// scenario and emits the LoRaWAN uplinks the house nodes would send, shaped
// exactly like ChirpStack's MQTT integration events. It is deterministic for
// a given seed and writes the ground truth later phases are validated against.
//
// Usage:
//
//	simulator -scenario storm50 -seed 42 -speed 60 -sink mqtt -mqtt-url mqtt://localhost:3133 -truth-out truth.json
//	simulator -scenario storm50 -seed 42 -sink stdout -hash > events.jsonl
//
// Events go to stdout (JSONL) or MQTT; logs and the stream hash go to stderr.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/smhunt/sumpnet/internal/platform"
	"github.com/smhunt/sumpnet/internal/sim"
)

func main() { os.Exit(run(os.Args[1:])) }

func run(args []string) int {
	fs := flag.NewFlagSet("simulator", flag.ContinueOnError)
	var (
		scenario     = fs.String("scenario", "storm50", "built-in scenario name (see -list-scenarios)")
		scenarioFile = fs.String("scenario-file", "", "JSON scenario file (overrides -scenario)")
		seed         = fs.Uint64("seed", 42, "random seed; same seed + scenario = identical events")
		start        = fs.String("start", "2026-04-15T00:00:00Z", "virtual start time (RFC 3339)")
		speed        = fs.Float64("speed", 0, "wall-clock pacing factor (60 = one virtual hour per real minute; 0 = unpaced)")
		homes        = fs.Int("homes", 60, "number of homes")
		segments     = fs.Int("segments", 8, "number of street segments")
		duration     = fs.Duration("duration", 0, "override the scenario duration")
		rainGauges   = fs.Int("rain-gauges", 2, "tipping-bucket rain gauge nodes (fPort 5) at the ends of the neighbourhood")
		sinkName     = fs.String("sink", "stdout", "where events go: stdout, mqtt or none")
		mqttURL      = fs.String("mqtt-url", "mqtt://localhost:3133", "broker URL for -sink mqtt")
		qos          = fs.Uint("qos", 1, "MQTT QoS (ChirpStack itself uses 0)")
		publishers   = fs.Int("publishers", 4, "parallel MQTT publishers (events sharded by DevEUI)")
		clientID     = fs.String("client-id", "", "MQTT client id (default sumpnet-sim-<seed>-<pid>)")
		appID        = fs.String("app-id", sim.DefaultIdentity().ApplicationID, "ChirpStack application id in topics and events")
		tenantID     = fs.String("tenant-id", sim.DefaultIdentity().TenantID, "ChirpStack tenant id in events")
		truthOut     = fs.String("truth-out", "", "write ground truth JSON to this file")
		printHash    = fs.Bool("hash", false, "print the SHA-256 of the event stream to stderr when done")
		list         = fs.Bool("list-scenarios", false, "list built-in scenarios and exit")
		logLevel     = fs.String("log-level", "info", "debug, info, warn or error")
	)
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}

	var level slog.Level
	if err := level.UnmarshalText([]byte(*logLevel)); err != nil {
		fmt.Fprintln(os.Stderr, "simulator: -log-level:", err)
		return 2
	}
	log := platform.NewLogger(os.Stderr, platform.Config{Service: "simulator", LogLevel: level})

	if *list {
		all := sim.Scenarios()
		for _, name := range sim.ScenarioNames() {
			s := all[name]
			fmt.Printf("%-13s %6s  %5.1f mm  %s\n", name, s.Duration, s.Rain.Total(), s.Description)
		}
		return 0
	}

	scn, err := loadScenario(*scenario, *scenarioFile)
	if err != nil {
		log.Error("scenario", "err", err)
		return 2
	}
	startAt, err := time.Parse(time.RFC3339, *start)
	if err != nil {
		log.Error("-start must be RFC 3339", "err", err)
		return 2
	}
	identity := sim.DefaultIdentity()
	identity.ApplicationID, identity.TenantID = *appID, *tenantID

	engine, err := sim.New(sim.Config{
		Seed: *seed, Start: startAt, Speed: *speed, Homes: *homes, Segments: *segments,
		Scenario: scn, Identity: identity, Duration: *duration, RainGauges: *rainGauges,
	})
	if err != nil {
		log.Error("configure", "err", err)
		return 2
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	hash := sim.NewHashSink()
	sinks := sim.MultiSink{hash}
	switch *sinkName {
	case "stdout":
		sinks = append(sinks, sim.NewWriterSink(os.Stdout, identity, engine.SegmentOf))
	case "mqtt":
		id := *clientID
		if id == "" {
			id = fmt.Sprintf("sumpnet-sim-%d-%d", *seed, os.Getpid())
		}
		mq, err := sim.NewMQTTSink(ctx, sim.MQTTConfig{
			URL: *mqttURL, ClientID: id, QoS: byte(*qos), Publishers: *publishers, //nolint:gosec // 0..2
			Identity: identity, SegmentOf: engine.SegmentOf, Log: log,
		})
		if err != nil {
			log.Error("mqtt", "err", err)
			return 1
		}
		sinks = append(sinks, mq)
	case "none":
	default:
		log.Error("unknown -sink", "sink", *sinkName)
		return 2
	}

	log.Info("starting", "scenario", scn.Name, "seed", *seed, "homes", *homes, "segments", *segments, "rain_gauges", *rainGauges,
		"duration", engine.Duration(), "speed", *speed, "sink", *sinkName)
	began := time.Now()
	truth, runErr := engine.Run(ctx, sinks)
	closeCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := sinks.Close(closeCtx); err != nil {
		log.Error("close sinks", "err", err)
		if runErr == nil {
			runErr = err
		}
	}
	truth.StreamHash = hash.Sum()

	if *truthOut != "" {
		if err := writeTruth(*truthOut, truth); err != nil {
			log.Error("write truth", "err", err)
			if runErr == nil {
				runErr = err
			}
		}
	}
	if *printHash {
		fmt.Fprintln(os.Stderr, truth.StreamHash)
	}
	log.Info("finished", "completed", truth.Completed, "events", truth.Events, "virtual_end", truth.EndedAt,
		"elapsed", time.Since(began).Round(time.Millisecond), "hash", truth.StreamHash)
	if runErr != nil {
		log.Error("run", "err", runErr)
		return 1
	}
	return 0
}

func loadScenario(name, file string) (*sim.Scenario, error) {
	if file == "" {
		s, ok := sim.Scenarios()[name]
		if !ok {
			return nil, fmt.Errorf("unknown scenario %q (try -list-scenarios)", name)
		}
		return s, nil
	}
	b, err := os.ReadFile(file) //nolint:gosec // user-supplied path is the point
	if err != nil {
		return nil, err
	}
	var s sim.Scenario
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, fmt.Errorf("%s: %w", file, err)
	}
	return &s, nil
}

func writeTruth(path string, t *sim.Truth) error {
	f, err := os.Create(path) //nolint:gosec // user-supplied path is the point
	if err != nil {
		return err
	}
	if err := t.Write(f); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}
