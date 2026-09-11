// Command cycle-detector annotates stored pump cycles with estimated volume
// and detects dry-run, short-cycling and continuous-run conditions
// (prompt_plan.md §10), handing them to the alerts service via the detections
// table.
package main

import (
	"context"
	"errors"
	"os"

	"github.com/smhunt/sumpnet/internal/detector"
	"github.com/smhunt/sumpnet/internal/platform"
)

func main() { os.Exit(platform.Run("cycle-detector", run)) }

func run(ctx context.Context, app *platform.App) error {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		return errors.New("DATABASE_URL is required")
	}
	cfg, err := detector.ConfigFromEnv()
	if err != nil {
		return err
	}
	return detector.Run(ctx, app, dsn, cfg)
}
