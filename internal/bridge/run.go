package bridge

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"time"

	"golang.org/x/sync/errgroup"

	telemetryv1 "github.com/smhunt/sumpnet/gen/go/sumpnet/telemetry/v1"
	"github.com/smhunt/sumpnet/internal/platform"
)

// Decoded is the output of a transport-specific decoder: exactly one field set.
type Decoded struct {
	Reading *telemetryv1.Reading
	Cycle   *telemetryv1.CycleEvent
	Summary *telemetryv1.StormSummary
	Alarm   *telemetryv1.Alarm
	// TimeFallback is true when the decoder had to use now() as the event time.
	TimeFallback bool
}

// Decoder turns one MQTT message into a proto row. Returning a *DropError
// drops (and acknowledges) the message; any other error is treated the same
// way but logged at warn level.
type Decoder func(topic string, payload []byte, now time.Time) (Decoded, error)

// DropError is an expected, counted reason to discard a message.
type DropError struct{ Reason string }

func (e *DropError) Error() string { return "drop: " + e.Reason }

// RunConfig is everything a bridge process needs.
type RunConfig struct {
	MQTT   MQTTConfig
	Ingest string // host:port
	Client ClientConfig
	Batch  BatchConfig
}

// ConfigFromEnv reads the shared bridge variables. Defaults are for compose.
func ConfigFromEnv(defaultTopic, defaultClientID string) (RunConfig, error) {
	c := RunConfig{
		MQTT:   MQTTConfig{URL: env("MQTT_URL", "mqtt://mosquitto:1883"), ClientID: env("MQTT_CLIENT_ID", defaultClientID), Topic: env("MQTT_TOPIC", defaultTopic), SharedGroup: os.Getenv("MQTT_SHARED_GROUP")},
		Ingest: env("INGEST_ADDR", "ingest:9090"),
	}
	var err error
	if c.Batch.MaxItems, err = envInt("BATCH_MAX", 500); err != nil {
		return c, err
	}
	if c.Batch.QueueDepth, err = envInt("QUEUE_DEPTH", 2000); err != nil {
		return c, err
	}
	if c.Batch.MaxDelay, err = envDur("BATCH_DELAY", 100*time.Millisecond); err != nil {
		return c, err
	}
	if c.Client.SubmitTimeout, err = envDur("SUBMIT_TIMEOUT", 10*time.Second); err != nil {
		return c, err
	}
	recvMax, err := envInt("MQTT_RECEIVE_MAX", 1000)
	if err != nil {
		return c, err
	}
	c.MQTT.ReceiveMax = uint16(max(1, min(recvMax, 65535))) //nolint:gosec // clamped
	if c.MQTT.SessionExpiry, err = envDur("MQTT_SESSION_EXPIRY", time.Hour); err != nil {
		return c, err
	}
	c.Batch.DrainTimeout = c.Client.SubmitTimeout + 2*time.Second
	return c, nil
}

func env(name, def string) string {
	if v, ok := os.LookupEnv(name); ok {
		return v
	}
	return def
}

func envInt(name string, def int) (int, error) {
	v, ok := os.LookupEnv(name)
	if !ok {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", name, err)
	}
	return n, nil
}

func envDur(name string, def time.Duration) (time.Duration, error) {
	v, ok := os.LookupEnv(name)
	if !ok {
		return def, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", name, err)
	}
	return d, nil
}

// cycleMsg lets cycle events and storm summaries share one batcher/stream.
type cycleMsg struct {
	cycle   *telemetryv1.CycleEvent
	summary *telemetryv1.StormSummary
}

// Run wires consumer → decoder → batchers → ingest and blocks until ctx ends.
// Shutdown order: stop intake, drain and acknowledge, disconnect.
func Run(ctx context.Context, app *platform.App, cfg RunConfig, decode Decoder) error {
	log := app.Log
	if cfg.MQTT.Log == nil {
		cfg.MQTT.Log = log
	}
	m := NewMetrics(app.Metrics)

	client, err := Dial(ctx, cfg.Ingest, cfg.Client, m)
	if err != nil {
		return err
	}
	defer func() { _ = client.Close() }()

	readings := NewBatcher("readings", cfg.Batch, m, log, func(c context.Context, rows []*telemetryv1.Reading) error {
		_, serr := client.SubmitReadings(c, rows)
		return serr
	})
	cycles := NewBatcher("cycle_events", cfg.Batch, m, log, func(c context.Context, msgs []cycleMsg) error {
		var ce []*telemetryv1.CycleEvent
		var ss []*telemetryv1.StormSummary
		for _, x := range msgs {
			if x.cycle != nil {
				ce = append(ce, x.cycle)
			} else {
				ss = append(ss, x.summary)
			}
		}
		_, serr := client.SubmitCycleEvents(c, ce, ss)
		return serr
	})
	alarms := NewBatcher("alarm_events", cfg.Batch, m, log, func(c context.Context, rows []*telemetryv1.Alarm) error {
		_, serr := client.SubmitAlarms(c, rows)
		return serr
	})

	// Batchers outlive the consumer so they can drain after intake stops.
	bctx, stopBatchers := context.WithCancel(context.Background())
	defer stopBatchers()
	g, gctx := errgroup.WithContext(bctx)
	g.Go(func() error { return readings.Run(gctx) })
	g.Go(func() error { return cycles.Run(gctx) })
	g.Go(func() error { return alarms.Run(gctx) })

	handler := func(hctx context.Context, topic string, payload []byte, ack func() error) {
		d, derr := decode(topic, payload, time.Now())
		if derr != nil {
			var drop *DropError
			reason := "decode_error"
			if errors.As(derr, &drop) {
				reason = drop.Reason
				log.Debug("dropped message", "topic", topic, "reason", reason)
			} else {
				log.Warn("dropped message", "topic", topic, "err", derr)
			}
			m.Messages.WithLabelValues("dropped").Inc()
			m.Drops.WithLabelValues(reason).Inc()
			_ = ack() // a poison message must not be redelivered forever
			return
		}
		if d.TimeFallback {
			m.TimeFallback.Inc()
		}
		m.Messages.WithLabelValues("decoded").Inc()
		var aerr error
		switch {
		case d.Reading != nil:
			aerr = readings.Add(hctx, Envelope[*telemetryv1.Reading]{Msg: d.Reading, Ack: ack})
		case d.Cycle != nil:
			aerr = cycles.Add(hctx, Envelope[cycleMsg]{Msg: cycleMsg{cycle: d.Cycle}, Ack: ack})
		case d.Summary != nil:
			aerr = cycles.Add(hctx, Envelope[cycleMsg]{Msg: cycleMsg{summary: d.Summary}, Ack: ack})
		case d.Alarm != nil:
			aerr = alarms.Add(hctx, Envelope[*telemetryv1.Alarm]{Msg: d.Alarm, Ack: ack})
		default:
			m.Drops.WithLabelValues("empty_decode").Inc()
			_ = ack()
		}
		if aerr != nil {
			log.Debug("message not queued (shutting down); broker will redeliver", "err", aerr)
		}
	}

	consumer, err := Connect(ctx, cfg.MQTT, m, handler)
	if err != nil {
		stopBatchers()
		_ = g.Wait()
		return err
	}
	app.Ready.Set(true)
	log.Info("bridge running", "mqtt", cfg.MQTT.URL, "topic", cfg.MQTT.Topic, "ingest", cfg.Ingest)

	select {
	case <-ctx.Done():
	case <-gctx.Done(): // a batcher failed
	}
	app.Ready.Set(false)
	closeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if cerr := consumer.Close(closeCtx); cerr != nil && !errors.Is(cerr, context.Canceled) {
		log.Warn("mqtt disconnect", "err", cerr)
	}
	stopBatchers()
	if werr := g.Wait(); werr != nil && !errors.Is(werr, context.Canceled) {
		return werr
	}
	return nil
}
