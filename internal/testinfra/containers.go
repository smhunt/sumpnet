//go:build integration

package testinfra

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
)

// PostgresImage matches deploy/compose (pg_partman 5, PostgreSQL 16).
const PostgresImage = "ghcr.io/dbsystel/postgresql-partman:16-5"

// StartPostgres runs a fresh Postgres with pg_partman and returns a DSN for
// the superuser. The container is terminated when the test ends.
func StartPostgres(t *testing.T) string {
	t.Helper()
	ctx := context.Background()
	c, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image:        PostgresImage,
			ExposedPorts: []string{"5432/tcp"},
			Env:          map[string]string{"POSTGRES_USER": "sumpnet", "POSTGRES_PASSWORD": "sumpnet", "POSTGRES_DB": "sumpnet"},
			Cmd:          []string{"postgres", "-c", "timezone=UTC", "-c", "shared_preload_libraries=pg_partman_bgw", "-c", "pg_partman_bgw.dbname=sumpnet", "-c", "pg_partman_bgw.role=sumpnet"},
			// The entrypoint starts the server twice (init, then real); wait for the second.
			WaitingFor: wait.ForLog("database system is ready to accept connections").WithOccurrence(2).WithStartupTimeout(60 * time.Second),
		},
		Started: true,
	})
	if err != nil {
		t.Fatalf("start postgres: %v", err)
	}
	t.Cleanup(func() { _ = c.Terminate(context.Background()) })
	host, err := c.Host(ctx)
	if err != nil {
		t.Fatal(err)
	}
	port, err := c.MappedPort(ctx, "5432/tcp")
	if err != nil {
		t.Fatal(err)
	}
	return fmt.Sprintf("postgres://sumpnet:sumpnet@%s:%s/sumpnet?sslmode=disable", host, port.Port())
}

const mosquittoConf = "listener 1883\nallow_anonymous true\nmax_inflight_messages 1000\nmax_queued_messages 100000\n"

// StartMosquitto runs an anonymous Mosquitto 2 broker tuned like the compose
// one and returns its mqtt:// URL.
func StartMosquitto(t *testing.T) string {
	t.Helper()
	ctx := context.Background()
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
