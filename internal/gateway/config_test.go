package gateway

import (
	"testing"
	"time"
)

func TestConfigFromEnv(t *testing.T) {
	keys := []string{"DATABASE_URL", "GRPC_ADDR", "REST_ADDR", "ALERTS_ADDR", "TLS_CERT_FILE", "TLS_KEY_FILE",
		"CORS_ALLOWED_ORIGINS", "TLS_FALLBACK_HTTP", "WATCH_POLL_INTERVAL", "WATCH_DEBOUNCE", "STATUS_WINDOW", "WATCH_BUFFER",
		"CLERK_ISSUER", "CLERK_JWKS_URL", "CLERK_AUTHORIZED_PARTIES", "CLERK_CLOCK_SKEW"}
	const dsn = "postgres://u:p@db:5432/sumpnet"
	tests := []struct {
		name    string
		env     map[string]string
		wantErr bool
		check   func(t *testing.T, c Config)
	}{
		{name: "defaults", env: map[string]string{"DATABASE_URL": dsn}, check: func(t *testing.T, c Config) {
			if c.GRPCAddr != ":9092" || c.RESTAddr != "" || c.Watch != DefaultWatchConfig() || c.Auth.Enabled() {
				t.Errorf("defaults = %+v", c)
			}
		}},
		{name: "compose", env: map[string]string{
			"DATABASE_URL": dsn, "REST_ADDR": ":8080", "ALERTS_ADDR": "alerts:9091",
			"TLS_CERT_FILE": "/certs/cert.pem", "TLS_KEY_FILE": "/certs/key.pem",
			"CORS_ALLOWED_ORIGINS": "https://dev.ecoworks.ca:3034", "STATUS_WINDOW": "30m", "WATCH_BUFFER": "64",
			"CLERK_ISSUER": "https://clerk.example.com", "CLERK_AUTHORIZED_PARTIES": "https://dev.ecoworks.ca:3034",
		}, check: func(t *testing.T, c Config) {
			if c.RESTAddr != ":8080" || c.Watch.StatusWindow != 30*time.Minute || c.Watch.Buffer != 64 ||
				len(c.CORSOrigins) != 1 || !c.Auth.Enabled() || c.AlertsAddr != "alerts:9091" {
				t.Errorf("compose = %+v", c)
			}
		}},
		{name: "tls fallback", env: map[string]string{"DATABASE_URL": dsn, "REST_ADDR": ":8080", "TLS_CERT_FILE": "c", "TLS_KEY_FILE": "k", "TLS_FALLBACK_HTTP": "true"}, check: func(t *testing.T, c Config) {
			if !c.TLSFallbackHTTP {
				t.Error("TLS_FALLBACK_HTTP not parsed")
			}
		}},
		{name: "bad tls fallback", env: map[string]string{"DATABASE_URL": dsn, "TLS_FALLBACK_HTTP": "maybe"}, wantErr: true},
		{name: "no database", env: map[string]string{}, wantErr: true},
		{name: "cert without key", env: map[string]string{"DATABASE_URL": dsn, "REST_ADDR": ":8080", "TLS_CERT_FILE": "c"}, wantErr: true},
		{name: "tls on the ops listener", env: map[string]string{"DATABASE_URL": dsn, "TLS_CERT_FILE": "c", "TLS_KEY_FILE": "k"}, wantErr: true},
		{name: "bad window", env: map[string]string{"DATABASE_URL": dsn, "STATUS_WINDOW": "0s"}, wantErr: true},
		{name: "bad duration", env: map[string]string{"DATABASE_URL": dsn, "WATCH_DEBOUNCE": "soon"}, wantErr: true},
		{name: "tiny buffer", env: map[string]string{"DATABASE_URL": dsn, "WATCH_BUFFER": "2"}, wantErr: true},
		{name: "bad clerk issuer", env: map[string]string{"DATABASE_URL": dsn, "CLERK_ISSUER": "http://clerk.example.com"}, wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			for _, k := range keys {
				t.Setenv(k, tc.env[k])
			}
			c, err := ConfigFromEnv()
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %t", err, tc.wantErr)
			}
			if err == nil && tc.check != nil {
				tc.check(t, c)
			}
		})
	}
}
