package platform

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"time"
)

// Config is the process-level configuration shared by all services.
// Service-specific settings are read by the service itself.
type Config struct {
	Service         string
	HTTPAddr        string        // HTTP_ADDR, default ":8080"
	LogLevel        slog.Level    // LOG_LEVEL, default "info"
	ShutdownTimeout time.Duration // SHUTDOWN_TIMEOUT, default 15s

	// out overrides the log destination (tests); nil means os.Stdout.
	out io.Writer
}

// FromEnv builds a Config for service from the environment, applying defaults.
func FromEnv(service string) (Config, error) {
	cfg := Config{
		Service:         service,
		HTTPAddr:        ":8080",
		LogLevel:        slog.LevelInfo,
		ShutdownTimeout: 15 * time.Second,
	}
	if v, ok := os.LookupEnv("HTTP_ADDR"); ok {
		cfg.HTTPAddr = v
	}
	if v, ok := os.LookupEnv("LOG_LEVEL"); ok {
		if err := cfg.LogLevel.UnmarshalText([]byte(v)); err != nil {
			return Config{}, fmt.Errorf("LOG_LEVEL: %w", err)
		}
	}
	if v, ok := os.LookupEnv("SHUTDOWN_TIMEOUT"); ok {
		d, err := time.ParseDuration(v)
		if err != nil {
			return Config{}, fmt.Errorf("SHUTDOWN_TIMEOUT: %w", err)
		}
		cfg.ShutdownTimeout = d
	}
	return cfg, nil
}

func (c Config) logOutput() io.Writer {
	if c.out != nil {
		return c.out
	}
	return os.Stdout
}
