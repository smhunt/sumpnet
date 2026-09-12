//go:build integration

package testinfra

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
)

// Mailpit is a running Mailpit SMTP sink.
type Mailpit struct {
	SMTPHost string
	SMTPPort int
	APIURL   string
}

// StartMailpit runs axllent/mailpit and returns its SMTP endpoint and HTTP API.
func StartMailpit(t *testing.T) Mailpit {
	t.Helper()
	ctx := context.Background()
	c, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image:        "axllent/mailpit:v1.24",
			ExposedPorts: []string{"1025/tcp", "8025/tcp"},
			WaitingFor:   wait.ForHTTP("/readyz").WithPort("8025/tcp").WithStartupTimeout(30 * time.Second),
		},
		Started: true,
	})
	if err != nil {
		t.Fatalf("start mailpit: %v", err)
	}
	t.Cleanup(func() { _ = c.Terminate(context.Background()) })
	host, err := c.Host(ctx)
	if err != nil {
		t.Fatal(err)
	}
	smtpPort, err := c.MappedPort(ctx, "1025/tcp")
	if err != nil {
		t.Fatal(err)
	}
	apiPort, err := c.MappedPort(ctx, "8025/tcp")
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(smtpPort.Port())
	if err != nil {
		t.Fatal(err)
	}
	return Mailpit{SMTPHost: host, SMTPPort: port, APIURL: fmt.Sprintf("http://%s:%s", host, apiPort.Port())}
}

// MailpitMessage is the subset of Mailpit's message summary tests use.
type MailpitMessage struct {
	ID        string `json:"ID"`
	MessageID string `json:"MessageID"`
	Subject   string `json:"Subject"`
	To        []struct {
		Address string `json:"Address"`
	} `json:"To"`
}

// Messages lists everything Mailpit has received.
func (m Mailpit) Messages(t *testing.T) []MailpitMessage {
	t.Helper()
	resp, err := http.Get(m.APIURL + "/api/v1/messages?limit=1000") //nolint:gosec // test container URL
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var body struct {
		Messages []MailpitMessage `json:"messages"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	return body.Messages
}
