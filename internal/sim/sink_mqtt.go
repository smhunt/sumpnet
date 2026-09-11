package sim

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"log/slog"
	"net/url"
	"sync"
	"time"

	"github.com/eclipse/paho.golang/autopaho"
	"github.com/eclipse/paho.golang/paho"
	"golang.org/x/sync/errgroup"

	"github.com/smhunt/sumpnet/internal/chirpstack"
)

// MQTTConfig configures an MQTTSink.
type MQTTConfig struct {
	URL        string // e.g. mqtt://localhost:3133
	ClientID   string
	QoS        byte // 1 by default; ChirpStack itself publishes at 0
	Publishers int  // parallel publisher goroutines (events sharded by DevEUI)
	Identity   Identity
	SegmentOf  func(homeIndex int) string
	Log        *slog.Logger
	// ConnectTimeout bounds the initial connection attempt.
	ConnectTimeout time.Duration
}

// MQTTSink publishes events to a broker on ChirpStack's uplink topics.
// Events are sharded by DevEUI across N publishers so each device's fCnt
// order is preserved; a bounded queue per publisher provides backpressure
// to the engine.
type MQTTSink struct {
	cfg    MQTTConfig
	cm     *autopaho.ConnectionManager
	queues []chan Event
	g      *errgroup.Group
	gctx   context.Context
	cancel context.CancelFunc

	closeOnce sync.Once
	closeErr  error
}

// NewMQTTSink connects to the broker and starts the publishers. It fails fast
// if the broker cannot be reached within ConnectTimeout.
func NewMQTTSink(ctx context.Context, cfg MQTTConfig) (*MQTTSink, error) {
	if cfg.Publishers <= 0 {
		cfg.Publishers = 4
	}
	if cfg.QoS == 0 {
		cfg.QoS = 1
	}
	if cfg.ClientID == "" {
		cfg.ClientID = "sumpnet-sim"
	}
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	if cfg.ConnectTimeout <= 0 {
		cfg.ConnectTimeout = 10 * time.Second
	}
	if cfg.SegmentOf == nil {
		cfg.SegmentOf = func(int) string { return "" }
	}
	u, err := url.Parse(cfg.URL)
	if err != nil {
		return nil, fmt.Errorf("mqtt sink: url: %w", err)
	}

	sctx, cancel := context.WithCancel(ctx)
	log := cfg.Log.With("component", "mqtt-sink", "broker", u.Host)
	cm, err := autopaho.NewConnection(sctx, autopaho.ClientConfig{
		ServerUrls:                    []*url.URL{u},
		KeepAlive:                     30,
		CleanStartOnInitialConnection: true,
		SessionExpiryInterval:         0,
		OnConnectionUp: func(*autopaho.ConnectionManager, *paho.Connack) {
			log.Info("connected")
		},
		OnConnectError: func(err error) { log.Warn("connect error", "err", err) },
		ClientConfig: paho.ClientConfig{
			ClientID:      cfg.ClientID,
			OnClientError: func(err error) { log.Warn("client error", "err", err) },
			OnServerDisconnect: func(d *paho.Disconnect) {
				log.Warn("server disconnect", "reason", d.ReasonCode)
			},
		},
	})
	if err != nil {
		cancel()
		return nil, fmt.Errorf("mqtt sink: %w", err)
	}
	actx, acancel := context.WithTimeout(sctx, cfg.ConnectTimeout)
	defer acancel()
	if err := cm.AwaitConnection(actx); err != nil {
		cancel()
		return nil, fmt.Errorf("mqtt sink: connect to %s: %w", u.Host, err)
	}

	s := &MQTTSink{cfg: cfg, cm: cm, cancel: cancel}
	s.g, s.gctx = errgroup.WithContext(sctx)
	s.queues = make([]chan Event, cfg.Publishers)
	for i := range s.queues {
		q := make(chan Event, 1024)
		s.queues[i] = q
		s.g.Go(func() error { return s.publisher(q) })
	}
	return s, nil
}

func (s *MQTTSink) publisher(q <-chan Event) error {
	for ev := range q {
		body, err := chirpstack.MarshalEvent(ev.ChirpStackEvent(s.cfg.Identity, s.cfg.SegmentOf(ev.HomeIndex)))
		if err != nil {
			return err
		}
		_, err = s.cm.Publish(s.gctx, &paho.Publish{
			QoS:     s.cfg.QoS,
			Topic:   chirpstack.UplinkTopic(s.cfg.Identity.ApplicationID, ev.DevEUI),
			Payload: body,
		})
		if err != nil {
			return fmt.Errorf("mqtt sink: publish seq %d: %w", ev.Seq, err)
		}
	}
	return nil
}

// Publish implements Sink. It blocks when the device's publisher queue is
// full, which is the engine's backpressure.
func (s *MQTTSink) Publish(ctx context.Context, ev Event) error {
	h := fnv.New32a()
	_, _ = h.Write([]byte(ev.DevEUI))
	q := s.queues[int(h.Sum32()%uint32(len(s.queues)))] //nolint:gosec // len(queues) is small
	select {
	case q <- ev:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-s.gctx.Done():
		return fmt.Errorf("mqtt sink: publisher stopped: %w", s.g.Wait())
	}
}

// Close drains the queues, waits for every in-flight publish and disconnects.
func (s *MQTTSink) Close(ctx context.Context) error {
	s.closeOnce.Do(func() {
		for _, q := range s.queues {
			close(q)
		}
		err := s.g.Wait()
		if derr := s.cm.Disconnect(ctx); derr != nil && !errors.Is(derr, context.Canceled) && err == nil {
			err = fmt.Errorf("mqtt sink: disconnect: %w", derr)
		}
		s.cancel()
		s.closeErr = err
	})
	return s.closeErr
}
