package gateway

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/smhunt/sumpnet/internal/auth"
)

// Config is the api-gateway configuration (environment only).
type Config struct {
	DatabaseURL string // DATABASE_URL (the pool is opened read-only)
	GRPCAddr    string // GRPC_ADDR, default ":9092" (container-internal; Phase 6 MCP dials it)
	// RESTAddr is REST_ADDR. Empty mounts the REST API on the platform ops
	// listener (HTTP_ADDR). Set, it is a separate listener serving REST plus
	// the ops endpoints, with TLS when TLS_CERT_FILE/TLS_KEY_FILE are set; the
	// ops listener then stays plain HTTP for the container healthcheck.
	RESTAddr    string
	AlertsAddr  string // ALERTS_ADDR (alerts:9091); empty disables acknowledge
	TLSCertFile string // TLS_CERT_FILE
	TLSKeyFile  string // TLS_KEY_FILE
	// TLSFallbackHTTP is TLS_FALLBACK_HTTP: when the certificate cannot be
	// loaded, serve REST over plain HTTP with a warning instead of failing
	// (compose sets it so a machine without the mkcert certs still starts).
	TLSFallbackHTTP bool
	CORSOrigins     []string // CORS_ALLOWED_ORIGINS, comma-separated
	Auth            auth.Config
	Watch           WatchConfig

	// JWKSClient fetches the Clerk JWKS; nil uses a default client. Tests
	// pass the httptest TLS client.
	JWKSClient *http.Client
}

// WatchConfig tunes the WatchNeighbourhood hub.
type WatchConfig struct {
	PollInterval time.Duration // WATCH_POLL_INTERVAL (10s): safety-net recompute when NOTIFY is missed
	Debounce     time.Duration // WATCH_DEBOUNCE (1s): coalesces a burst of ingest notifications
	StatusWindow time.Duration // STATUS_WINDOW (1h): live window ending at the neighbourhood clock
	Buffer       int           // WATCH_BUFFER (256): per-subscriber queue; overflowing it ends that stream
	RecentStorms int32         // snapshot: open storms plus this many most recent ones
}

// DefaultWatchConfig returns the production defaults.
func DefaultWatchConfig() WatchConfig {
	return WatchConfig{PollInterval: 10 * time.Second, Debounce: time.Second, StatusWindow: time.Hour, Buffer: 256, RecentStorms: 5}
}

// ConfigFromEnv reads the gateway configuration.
func ConfigFromEnv() (Config, error) {
	c := Config{
		DatabaseURL: os.Getenv("DATABASE_URL"),
		GRPCAddr:    ":9092",
		RESTAddr:    strings.TrimSpace(os.Getenv("REST_ADDR")),
		AlertsAddr:  strings.TrimSpace(os.Getenv("ALERTS_ADDR")),
		TLSCertFile: strings.TrimSpace(os.Getenv("TLS_CERT_FILE")),
		TLSKeyFile:  strings.TrimSpace(os.Getenv("TLS_KEY_FILE")),
		CORSOrigins: splitList(os.Getenv("CORS_ALLOWED_ORIGINS")),
		Watch:       DefaultWatchConfig(),
	}
	if v := strings.TrimSpace(os.Getenv("GRPC_ADDR")); v != "" {
		c.GRPCAddr = v
	}
	for name, dst := range map[string]*time.Duration{
		"WATCH_POLL_INTERVAL": &c.Watch.PollInterval,
		"WATCH_DEBOUNCE":      &c.Watch.Debounce,
		"STATUS_WINDOW":       &c.Watch.StatusWindow,
	} {
		if v := os.Getenv(name); v != "" {
			d, err := time.ParseDuration(v)
			if err != nil {
				return Config{}, fmt.Errorf("%s: %w", name, err)
			}
			*dst = d
		}
	}
	if v := os.Getenv("TLS_FALLBACK_HTTP"); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return Config{}, fmt.Errorf("TLS_FALLBACK_HTTP: %w", err)
		}
		c.TLSFallbackHTTP = b
	}
	if v := os.Getenv("WATCH_BUFFER"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return Config{}, fmt.Errorf("WATCH_BUFFER: %w", err)
		}
		c.Watch.Buffer = n
	}
	var err error
	if c.Auth, err = auth.ConfigFromEnv(); err != nil {
		return Config{}, err
	}
	return c, c.Validate()
}

// Validate checks the configuration for contradictions.
func (c Config) Validate() error {
	switch {
	case c.DatabaseURL == "":
		return errors.New("DATABASE_URL is required")
	case c.GRPCAddr == "":
		return errors.New("GRPC_ADDR is required")
	case (c.TLSCertFile == "") != (c.TLSKeyFile == ""):
		return errors.New("TLS_CERT_FILE and TLS_KEY_FILE must be set together")
	case c.TLSCertFile != "" && c.RESTAddr == "":
		return errors.New("TLS needs REST_ADDR: the ops listener (HTTP_ADDR) stays plain HTTP for the container healthcheck")
	case c.Watch.PollInterval <= 0 || c.Watch.Debounce < 0 || c.Watch.StatusWindow <= 0:
		return errors.New("WATCH_POLL_INTERVAL and STATUS_WINDOW must be positive, WATCH_DEBOUNCE non-negative")
	case c.Watch.Buffer < 16:
		return errors.New("WATCH_BUFFER must be at least 16")
	}
	return nil
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimRight(strings.TrimSpace(p), "/"); p != "" {
			out = append(out, p)
		}
	}
	return out
}
