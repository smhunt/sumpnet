// Command simulator replays a neighbourhood of sump pumps through a rainfall
// scenario and emits the LoRaWAN uplinks the house nodes would send, shaped
// exactly like ChirpStack's MQTT integration events. It is deterministic for
// a given seed and writes the ground truth later phases are validated against.
//
// Usage:
//
//	simulator -scenario storm50 -seed 42 -speed 60 -sink mqtt -mqtt-url mqtt://localhost:3133 -truth-out truth.json
//	simulator -scenario storm50 -seed 42 -sink stdout -hash > events.jsonl
//	simulator -scenario eccc -from 2026-08-01 -to 2026-09-12 -site-file data/sites/timberwalk.json -sink none -hash
//
// Events go to stdout (JSONL) or MQTT; logs and the stream hash go to stderr.
// -site-file replaces the synthetic neighbourhood with a real-geography site
// snapshot (make site-import); -scenario eccc replays observed ECCC hourly rain
// from the local cache (make eccc-import) with the virtual start at -from.
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
	"github.com/smhunt/sumpnet/internal/raincache"
	"github.com/smhunt/sumpnet/internal/sim"
	"github.com/smhunt/sumpnet/internal/site"
	"github.com/smhunt/sumpnet/internal/weather"
)

// observedScenario is the -scenario name for observed ECCC rain.
const observedScenario = "eccc"

func main() { os.Exit(run(os.Args[1:])) }

func run(args []string) int {
	fs := flag.NewFlagSet("simulator", flag.ContinueOnError)
	var (
		scenario     = fs.String("scenario", "storm50", "built-in scenario name, or eccc for observed rain (see -list-scenarios)")
		siteFile     = fs.String("site-file", "", "real-geography site snapshot (make site-import); replaces -homes and -segments")
		rainFile     = fs.String("rain-file", "", "observed hourly rain cache for -scenario eccc (default data/rain/eccc-hourly-<station>.json)")
		rainStation  = fs.String("rain-station", weather.DefaultECCCStation, "ECCC station of the default -rain-file")
		fromS        = fs.String("from", "", "-scenario eccc: window start and virtual start (YYYY-MM-DD = UTC midnight, or RFC 3339)")
		toS          = fs.String("to", "", "-scenario eccc: window end")
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
			fmt.Printf("%-13s %6s  %5.1f mm  %s\n", name, s.Duration, s.RainTotal(), s.Description)
		}
		fmt.Printf("%-13s %6s  %8s  %s\n", observedScenario, "FROM-TO", "observed", "ECCC LONDON CS hourly rain from the local cache (make eccc-import); needs -from and -to, starts at -from")
		return 0
	}
	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })

	startAt, err := time.Parse(time.RFC3339, *start)
	if err != nil {
		log.Error("-start must be RFC 3339", "err", err)
		return 2
	}
	var scn *sim.Scenario
	if *scenario == observedScenario && *scenarioFile == "" {
		if set["start"] {
			log.Error("-scenario eccc starts at -from; drop -start")
			return 2
		}
		scn, startAt, err = observedRain(*rainFile, *rainStation, *fromS, *toS)
	} else {
		if set["from"] || set["to"] {
			log.Error("-from and -to only apply to -scenario eccc")
			return 2
		}
		scn, err = loadScenario(*scenario, *scenarioFile)
	}
	if err != nil {
		log.Error("scenario", "err", err)
		return 2
	}
	var simSite *sim.Site
	if *siteFile != "" {
		if set["homes"] || set["segments"] {
			log.Error("-homes and -segments come from -site-file; drop them")
			return 2
		}
		snap, serr := site.LoadSnapshot(*siteFile)
		if serr != nil {
			log.Error("site", "err", serr)
			return 2
		}
		built, serr := site.Build(snap)
		if serr != nil {
			log.Error("site", "err", serr)
			return 2
		}
		simSite = built.SimSite()
		*homes, *segments = 0, 0
	}
	identity := sim.DefaultIdentity()
	identity.ApplicationID, identity.TenantID = *appID, *tenantID

	engine, err := sim.New(sim.Config{
		Seed: *seed, Start: startAt, Speed: *speed, Homes: *homes, Segments: *segments,
		Scenario: scn, Identity: identity, Duration: *duration, RainGauges: *rainGauges, Site: simSite,
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

	siteName := ""
	if simSite != nil {
		siteName = simSite.Name
	}
	log.Info("starting", "scenario", scn.Name, "seed", *seed, "site", siteName, "homes", len(engine.Homes()), "segments", len(engine.Segments()),
		"rain_gauges", *rainGauges, "start", startAt, "duration", engine.Duration(), "rain_mm", scn.RainTotal(), "speed", *speed, "sink", *sinkName)
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

// observedRain builds the eccc scenario from the local rain cache; the
// simulator never fetches.
func observedRain(file, station, fromS, toS string) (*sim.Scenario, time.Time, error) {
	if fromS == "" || toS == "" {
		return nil, time.Time{}, errors.New("-scenario eccc needs -from and -to")
	}
	from, err := raincache.ParseTime(fromS)
	if err != nil {
		return nil, time.Time{}, fmt.Errorf("-from: %w", err)
	}
	to, err := raincache.ParseTime(toS)
	if err != nil {
		return nil, time.Time{}, fmt.Errorf("-to: %w", err)
	}
	if file == "" {
		file = raincache.DefaultPath(station)
	}
	rc, err := raincache.Load(file)
	if err != nil {
		return nil, time.Time{}, err
	}
	if err = rc.Covered(from, to); err != nil {
		return nil, time.Time{}, err
	}
	desc := fmt.Sprintf("Observed ECCC hourly rain, station %s, hours ending %s to %s (%s)", rc.Station,
		from.Add(time.Hour).Format(time.RFC3339), to.Format(time.RFC3339), raincache.Attribution)
	scn, err := sim.ObservedRainScenario(observedScenario, desc, from, to, rc.Window(from, to))
	return scn, from, err
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
