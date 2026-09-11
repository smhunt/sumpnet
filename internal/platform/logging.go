package platform

import (
	"io"
	"log/slog"
)

// NewLogger returns a JSON slog.Logger tagged with the service name and build
// version.
func NewLogger(w io.Writer, cfg Config) *slog.Logger {
	h := slog.NewJSONHandler(w, &slog.HandlerOptions{Level: cfg.LogLevel})
	return slog.New(h).With("service", cfg.Service, "version", Version)
}
