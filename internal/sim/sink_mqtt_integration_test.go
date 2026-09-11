//go:build integration

package sim

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/eclipse/paho.golang/autopaho"
	"github.com/eclipse/paho.golang/paho"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/smhunt/sumpnet/internal/chirpstack"
)

const mosquittoConf = "listener 1883\nallow_anonymous true\n"

func startMosquitto(t *testing.T, ctx context.Context) string {
	t.Helper()
	c, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image:        "eclipse-mosquitto:2",
			ExposedPorts: []string{"1883/tcp"},
			Files: []testcontainers.ContainerFile{{
				Reader:            strings.NewReader(mosquittoConf),
				ContainerFilePath: "/mosquitto/config/mosquitto.conf",
				FileMode:          0o644,
			}},
			WaitingFor: wait.ForListeningPort("1883/tcp").WithStartupTimeout(30 * time.Second),
		},
		Started: true,
	})
	if err != nil {
		t.Fatalf("start mosquitto: %v", err)
	}
	t.Cleanup(func() { _ = c.Terminate(context.Background()) })
	host, err := c.Host(ctx)
	if err != nil {
		t.Fatal(err)
	}
	port, err := c.MappedPort(ctx, "1883/tcp")
	if err != nil {
		t.Fatal(err)
	}
	return fmt.Sprintf("mqtt://%s:%s", host, port.Port())
}

type received struct {
	topic   string
	payload []byte
}

func subscribe(t *testing.T, ctx context.Context, broker string) (<-chan received, func()) {
	t.Helper()
	u, _ := url.Parse(broker)
	ch := make(chan received, 10000)
	cm, err := autopaho.NewConnection(ctx, autopaho.ClientConfig{
		ServerUrls:                    []*url.URL{u},
		KeepAlive:                     30,
		CleanStartOnInitialConnection: true,
		OnConnectionUp: func(cm *autopaho.ConnectionManager, _ *paho.Connack) {
			_, err := cm.Subscribe(ctx, &paho.Subscribe{Subscriptions: []paho.SubscribeOptions{{Topic: chirpstack.UplinkTopicFilter, QoS: 1}}})
			if err != nil {
				t.Errorf("subscribe: %v", err)
			}
		},
		ClientConfig: paho.ClientConfig{
			ClientID: "sumpnet-sim-test-sub",
			OnPublishReceived: []func(paho.PublishReceived) (bool, error){
				func(pr paho.PublishReceived) (bool, error) {
					ch <- received{topic: pr.Packet.Topic, payload: pr.Packet.Payload}
					return true, nil
				},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := cm.AwaitConnection(ctx); err != nil {
		t.Fatal(err)
	}
	// Give the subscription a moment to be acknowledged before publishing.
	time.Sleep(300 * time.Millisecond)
	return ch, func() { _ = cm.Disconnect(context.Background()) }
}

func TestMQTTSinkIntegration(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	broker := startMosquitto(t, ctx)
	rx, stop := subscribe(t, ctx, broker)
	defer stop()

	scn := Scenarios()["storm25"]
	e, err := New(Config{Seed: 3, Start: testStart, Homes: 5, Segments: 2, Scenario: scn})
	if err != nil {
		t.Fatal(err)
	}
	sink, err := NewMQTTSink(ctx, MQTTConfig{URL: broker, ClientID: "sumpnet-sim-test", Publishers: 3, Identity: DefaultIdentity(), SegmentOf: e.SegmentOf})
	if err != nil {
		t.Fatal(err)
	}
	hs := NewHashSink()
	truth, err := e.Run(ctx, MultiSink{sink, hs})
	if err != nil {
		t.Fatal(err)
	}
	if err := sink.Close(ctx); err != nil {
		t.Fatalf("close: %v", err)
	}
	want := int(truth.Events)
	if want == 0 {
		t.Fatal("no events emitted")
	}

	var got []received
	deadline := time.After(60 * time.Second)
	for len(got) < want {
		select {
		case r := <-rx:
			got = append(got, r)
		case <-deadline:
			t.Fatalf("received %d of %d events before timeout", len(got), want)
		}
	}
	select {
	case extra := <-rx:
		t.Fatalf("received more events than emitted: extra on %s", extra.topic)
	case <-time.After(500 * time.Millisecond):
	}

	// Every message decodes, topic matches the event, and fCnt is strictly
	// increasing per device.
	last := map[string]int64{}
	var mu sync.Mutex
	for _, r := range got {
		ev, err := chirpstack.UnmarshalEvent(r.payload)
		if err != nil {
			t.Fatalf("unmarshal: %v\n%s", err, r.payload)
		}
		app, dev, err := chirpstack.ParseUplinkTopic(r.topic)
		if err != nil || app != DefaultIdentity().ApplicationID || dev != ev.DeviceInfo.DevEui {
			t.Fatalf("topic %q does not match event (app %s, dev %s): %v", r.topic, app, ev.DeviceInfo.DevEui, err)
		}
		mu.Lock()
		prev, seen := last[dev]
		if seen && int64(ev.FCnt) <= prev {
			t.Fatalf("device %s: fCnt %d after %d", dev, ev.FCnt, prev)
		}
		last[dev] = int64(ev.FCnt)
		mu.Unlock()
	}
	if len(last) != 5 {
		t.Fatalf("saw %d devices, want 5", len(last))
	}
	t.Logf("received %d events from %d devices; stream hash %s", len(got), len(last), hs.Sum())
}
