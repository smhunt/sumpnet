package weather

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/smhunt/sumpnet/internal/platform"
	"github.com/smhunt/sumpnet/internal/store"
	"github.com/smhunt/sumpnet/internal/watermark"
)

// Config is the weather service configuration.
type Config struct {
	Watermark watermark.Config
	ECCC      ECCCConfig
}

// ConfigFromEnv reads the watermark variables and WEATHER_ECCC_*.
func ConfigFromEnv() (Config, error) {
	wm, err := watermark.ConfigFromEnv("weather")
	if err != nil {
		return Config{}, err
	}
	c := Config{Watermark: wm, ECCC: DefaultECCCConfig()}
	e := &c.ECCC
	if v, ok := os.LookupEnv("WEATHER_ECCC_ENABLED"); ok {
		if e.Enabled, err = strconv.ParseBool(v); err != nil {
			return c, fmt.Errorf("WEATHER_ECCC_ENABLED: %w", err)
		}
	}
	if v, ok := os.LookupEnv("WEATHER_ECCC_URL"); ok && v != "" {
		e.BaseURL = v
	}
	if v, ok := os.LookupEnv("WEATHER_ECCC_STATION"); ok {
		e.Station = v
	}
	for name, dst := range map[string]*time.Duration{
		"WEATHER_ECCC_POLL_INTERVAL": &e.PollInterval,
		"WEATHER_ECCC_BACKFILL":      &e.Backfill,
		"WEATHER_ECCC_LOOKBACK":      &e.Lookback,
		"WEATHER_ECCC_TIMEOUT":       &e.Timeout,
	} {
		if v, ok := os.LookupEnv(name); ok {
			d, perr := time.ParseDuration(v)
			if perr != nil {
				return c, fmt.Errorf("%s: %w", name, perr)
			}
			*dst = d
		}
	}
	if e.Enabled {
		switch {
		case e.Station == "":
			return c, errors.New("WEATHER_ECCC_STATION is required when the ECCC poller is enabled")
		case e.PollInterval <= 0 || e.Timeout <= 0:
			return c, errors.New("WEATHER_ECCC_POLL_INTERVAL and WEATHER_ECCC_TIMEOUT must be positive")
		case e.Lookback < 2*time.Hour || e.Backfill < e.Lookback:
			return c, errors.New("WEATHER_ECCC_LOOKBACK must be at least 2h and WEATHER_ECCC_BACKFILL at least the lookback")
		}
	}
	return c, nil
}

// Run is the service body used by cmd/weather: the rain gauge consumer and,
// unless disabled, the ECCC poller, until ctx ends.
func Run(ctx context.Context, app *platform.App, dsn string, cfg Config) error {
	st, err := store.New(ctx, dsn)
	if err != nil {
		return err
	}
	defer st.Close()
	if err := st.CheckSchema(ctx); err != nil {
		return fmt.Errorf("schema check (run migrations first): %w", err)
	}
	m := NewMetrics(app.Metrics)
	c := watermark.New(st.Pool(), dsn, cfg.Watermark, watermark.NewMetrics(app.Metrics), app.Log)
	watermark.Add(c, GaugeSource, NewGaugeHandler(m, app.Log))
	c.OnRound(func(int) { app.Ready.Set(true) })

	g, gctx := errgroup.WithContext(ctx)
	g.Go(func() error { return c.Run(gctx) })
	if cfg.ECCC.Enabled {
		client, err := NewECCCClient(cfg.ECCC.BaseURL, &http.Client{Timeout: cfg.ECCC.Timeout}, cfg.ECCC.PageSize)
		if err != nil {
			return err
		}
		poller := NewECCCPoller(client, cfg.ECCC, st.Pool(), m, app.Log)
		g.Go(func() error { return poller.Run(gctx) })
	}
	app.Log.Info("weather running", "poll_interval", cfg.Watermark.PollInterval, "lag", cfg.Watermark.Lag,
		"eccc_enabled", cfg.ECCC.Enabled, "eccc_station", cfg.ECCC.Station, "eccc_poll_interval", cfg.ECCC.PollInterval)
	return g.Wait()
}
