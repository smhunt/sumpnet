// Package platform is the shared process skeleton for every sumpnet service:
// configuration from the environment, slog JSON logging, the operational HTTP
// endpoints (/healthz, /readyz, /metrics) and graceful shutdown on
// SIGINT/SIGTERM. Services supply a RunFunc and call Run from main.
package platform

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/prometheus/client_golang/prometheus"
	"golang.org/x/sync/errgroup"
)

// Build metadata, injected with -ldflags "-X github.com/smhunt/sumpnet/internal/platform.Version=...".
var (
	Version = "dev"
	Commit  = "unknown"
)

// App is what a service's RunFunc receives. Services may register additional
// routes on Mux and collectors on Metrics before flipping Ready.
type App struct {
	Config  Config
	Log     *slog.Logger
	Mux     *http.ServeMux
	Ready   *ReadyGate
	Metrics *prometheus.Registry
}

// RunFunc is a service's main loop. It must return when ctx is cancelled.
// Returning nil ends the process cleanly; returning an error ends it with exit
// code 1.
type RunFunc func(ctx context.Context, app *App) error

// Run is the process entrypoint used by every cmd/<service>/main.go. It parses
// configuration, handles the "healthcheck" subcommand (used by the container
// HEALTHCHECK, since distroless images have no curl), installs signal handling
// and returns the process exit code.
func Run(service string, fn RunFunc) int {
	cfg, err := FromEnv(service)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s: config: %v\n", service, err)
		return 1
	}
	if len(os.Args) > 1 && os.Args[1] == "healthcheck" {
		return Healthcheck(cfg.HTTPAddr)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := RunContext(ctx, cfg, fn); err != nil {
		slog.Error("service exited with error", "err", err)
		return 1
	}
	return 0
}

// RunContext is Run without signal handling or process exit, so it can be
// driven from tests. It starts the ops HTTP server, calls fn, and shuts the
// server down when ctx is cancelled or fn returns.
func RunContext(ctx context.Context, cfg Config, fn RunFunc) error {
	log := NewLogger(cfg.logOutput(), cfg)
	slog.SetDefault(log)

	app := NewApp(cfg, log)
	srv := newOpsServer(cfg.HTTPAddr, app.Mux)

	// fn finishing (with or without error) must tear everything down too, so
	// wrap ctx in an explicit cancel that fn's goroutine triggers on return.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	g, gctx := errgroup.WithContext(ctx)

	log.Info("starting", "addr", cfg.HTTPAddr, "commit", Commit)

	g.Go(func() error {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("ops http server: %w", err)
		}
		return nil
	})
	g.Go(func() error {
		defer cancel()
		if err := fn(gctx, app); err != nil {
			return fmt.Errorf("run: %w", err)
		}
		return nil
	})
	g.Go(func() error {
		<-gctx.Done()
		app.Ready.Set(false)
		log.Info("shutting down", "timeout", cfg.ShutdownTimeout)
		shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
		defer cancelShutdown()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			return fmt.Errorf("ops http shutdown: %w", err)
		}
		return nil
	})

	err := g.Wait()
	log.Info("stopped")
	return err
}
