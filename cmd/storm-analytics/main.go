// Command storm-analytics segments rainfall into storm events and computes
// each linked home's response to every storm (prompt_plan.md §10): lag,
// recession, volume, cycles and the baseflow they were measured against.
package main

import (
	"context"
	"errors"
	"os"

	"github.com/smhunt/sumpnet/internal/platform"
	"github.com/smhunt/sumpnet/internal/storms"
)

func main() { os.Exit(platform.Run("storm-analytics", run)) }

func run(ctx context.Context, app *platform.App) error {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		return errors.New("DATABASE_URL is required")
	}
	cfg, err := storms.ConfigFromEnv()
	if err != nil {
		return err
	}
	return storms.Run(ctx, app, dsn, cfg)
}
