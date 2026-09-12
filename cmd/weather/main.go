// Command weather derives rainfall from the rain gauge nodes (fPort 5) and
// polls ECCC hourly climate observations as the fallback and cross-check
// (prompt_plan.md §11).
package main

import (
	"context"
	"errors"
	"os"

	"github.com/smhunt/sumpnet/internal/platform"
	"github.com/smhunt/sumpnet/internal/weather"
)

func main() { os.Exit(platform.Run("weather", run)) }

func run(ctx context.Context, app *platform.App) error {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		return errors.New("DATABASE_URL is required")
	}
	cfg, err := weather.ConfigFromEnv()
	if err != nil {
		return err
	}
	return weather.Run(ctx, app, dsn, cfg)
}
