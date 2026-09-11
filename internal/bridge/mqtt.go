package bridge

import (
	"context"
	"fmt"
	"log/slog"
	"net/url"
	"time"

	"github.com/eclipse/paho.golang/autopaho"
	"github.com/eclipse/paho.golang/paho"
)

// MQTTConfig configures the consumer.
type MQTTConfig struct {
	URL           string        // mqtt://host:port
	ClientID      string        // stable, so the broker keeps the session across restarts
	Topic         string        // subscription filter
	SharedGroup   string        // if set, subscribe as $share/<group>/<topic>
	ReceiveMax    uint16        // in-flight QoS 1 messages the broker may send unacked
	SessionExpiry time.Duration // how long the broker queues for us while disconnected
	Log           *slog.Logger
}

// Handler receives each message; ack must be called once the message is safe
// to forget (it is not called by the consumer itself).
type Handler func(ctx context.Context, topic string, payload []byte, ack func() error)

// Consumer is a subscribed MQTT session with manual acknowledgements.
type Consumer struct {
	cm  *autopaho.ConnectionManager
	log *slog.Logger
}

// Connect subscribes to cfg.Topic and dispatches messages to handle. It
// returns once the first connection is up (or ctx ends). Because paho
// delivers messages from one goroutine and the handler blocks on the
// batcher queue, a full queue stalls delivery and the broker stops at
// ReceiveMax unacknowledged messages: end-to-end backpressure.
func Connect(ctx context.Context, cfg MQTTConfig, m *Metrics, handle Handler) (*Consumer, error) {
	if cfg.ReceiveMax == 0 {
		cfg.ReceiveMax = 1000
	}
	if cfg.SessionExpiry <= 0 {
		cfg.SessionExpiry = time.Hour
	}
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	u, err := url.Parse(cfg.URL)
	if err != nil {
		return nil, fmt.Errorf("bridge: mqtt url: %w", err)
	}
	topic := cfg.Topic
	if cfg.SharedGroup != "" {
		topic = "$share/" + cfg.SharedGroup + "/" + cfg.Topic
	}
	log := cfg.Log.With("component", "mqtt", "broker", u.Host, "topic", topic)
	recvMax := cfg.ReceiveMax
	sessionExpiry := uint32(cfg.SessionExpiry.Seconds()) //nolint:gosec // seconds, small

	cm, err := autopaho.NewConnection(ctx, autopaho.ClientConfig{
		ServerUrls:                    []*url.URL{u},
		KeepAlive:                     30,
		CleanStartOnInitialConnection: false,
		SessionExpiryInterval:         sessionExpiry,
		ConnectPacketBuilder: func(c *paho.Connect, _ *url.URL) (*paho.Connect, error) {
			if c.Properties == nil {
				c.Properties = &paho.ConnectProperties{}
			}
			c.Properties.ReceiveMaximum = &recvMax
			return c, nil
		},
		OnConnectionUp: func(cm *autopaho.ConnectionManager, ack *paho.Connack) {
			m.MQTTConnected.Set(1)
			log.Info("connected", "session_present", ack.SessionPresent)
			if _, serr := cm.Subscribe(ctx, &paho.Subscribe{Subscriptions: []paho.SubscribeOptions{{Topic: topic, QoS: 1}}}); serr != nil {
				log.Error("subscribe failed", "err", serr)
			}
		},
		OnConnectError: func(err error) {
			m.MQTTConnected.Set(0)
			log.Warn("connect error", "err", err)
		},
		ClientConfig: paho.ClientConfig{
			ClientID:                   cfg.ClientID,
			EnableManualAcknowledgment: true,
			OnPublishReceived: []func(paho.PublishReceived) (bool, error){
				func(pr paho.PublishReceived) (bool, error) {
					pkt := pr.Packet
					handle(ctx, pkt.Topic, pkt.Payload, func() error { return pr.Client.Ack(pkt) })
					return true, nil
				},
			},
			OnClientError: func(err error) {
				m.MQTTConnected.Set(0)
				log.Warn("client error", "err", err)
			},
			OnServerDisconnect: func(d *paho.Disconnect) {
				m.MQTTConnected.Set(0)
				log.Warn("server disconnect", "reason", d.ReasonCode)
			},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("bridge: mqtt: %w", err)
	}
	if err := cm.AwaitConnection(ctx); err != nil {
		return nil, fmt.Errorf("bridge: mqtt connect to %s: %w", u.Host, err)
	}
	return &Consumer{cm: cm, log: log}, nil
}

// Close disconnects; the broker keeps the session (and queues) for SessionExpiry.
func (c *Consumer) Close(ctx context.Context) error {
	return c.cm.Disconnect(ctx)
}
