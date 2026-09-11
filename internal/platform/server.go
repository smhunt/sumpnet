package platform

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// NewApp assembles an App with the ops endpoints pre-registered on Mux.
func NewApp(cfg Config, log *slog.Logger) *App {
	mux := http.NewServeMux()
	ready := &ReadyGate{}
	reg := newRegistry(cfg)

	mux.HandleFunc("GET /healthz", healthz)
	mux.Handle("GET /readyz", ready)
	mux.Handle("GET /metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{}))

	return &App{Config: cfg, Log: log, Mux: mux, Ready: ready, Metrics: reg}
}

func newOpsServer(addr string, h http.Handler) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           h,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
}
