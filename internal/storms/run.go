package storms

import (
	"context"
	"fmt"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/smhunt/sumpnet/internal/hydrology"
	"github.com/smhunt/sumpnet/internal/platform"
	"github.com/smhunt/sumpnet/internal/store"
	"github.com/smhunt/sumpnet/internal/watermark"
)

// Config is the storm-analytics configuration.
type Config struct {
	Watermark watermark.Config
	Params    hydrology.ResponseParams
}

// ConfigFromEnv reads the watermark variables; the analytics parameters are
// the §10 defaults (see hydrology.DefaultResponseParams).
func ConfigFromEnv() (Config, error) {
	wm, err := watermark.ConfigFromEnv("storm-analytics")
	if err != nil {
		return Config{}, err
	}
	return Config{Watermark: wm, Params: hydrology.DefaultResponseParams()}, nil
}

// Metrics are the service's collectors.
type Metrics struct {
	StormEvents    *prometheus.CounterVec // action=insert|update|delete
	HomeMetrics    *prometheus.CounterVec // result=written|unchanged|deleted
	ProcessSeconds prometheus.Histogram
}

// NewMetrics registers the collectors on reg.
func NewMetrics(reg prometheus.Registerer) *Metrics {
	m := &Metrics{
		StormEvents: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "sumpnet_storms_events_total", Help: "storm_events rows written, by action."}, []string{"action"}),
		HomeMetrics: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "sumpnet_storms_home_metrics_total", Help: "home_storm_metrics recomputations, by outcome."}, []string{"result"}),
		ProcessSeconds: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name: "sumpnet_storms_process_seconds", Help: "Time to recompute storms and home metrics for one poll.",
			Buckets: prometheus.ExponentialBuckets(0.001, 2, 14)}),
	}
	reg.MustRegister(m.StormEvents, m.HomeMetrics, m.ProcessSeconds)
	return m
}

// Run is the service body used by cmd/storm-analytics.
func Run(ctx context.Context, app *platform.App, dsn string, cfg Config) error {
	st, err := store.New(ctx, dsn)
	if err != nil {
		return err
	}
	defer st.Close()
	if err := st.CheckSchema(ctx); err != nil {
		return fmt.Errorf("schema check (run migrations first): %w", err)
	}
	c := watermark.New(st.Pool(), dsn, cfg.Watermark, watermark.NewMetrics(app.Metrics), app.Log)
	NewAnalyzer(cfg.Params, NewMetrics(app.Metrics), app.Log).AddStages(c)
	c.OnRound(func(int) { app.Ready.Set(true) })
	app.Log.Info("storm-analytics running", "poll_interval", cfg.Watermark.PollInterval, "lag", cfg.Watermark.Lag)
	return c.Run(ctx)
}
