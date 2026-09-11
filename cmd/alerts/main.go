// Command alerts raises, acknowledges and resolves alerts from node alarms,
// heartbeats and cycle-detector output, emails an operator, and serves
// alerts.v1.AlertService.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/smhunt/sumpnet/internal/alerts"
	"github.com/smhunt/sumpnet/internal/platform"
)

func main() { os.Exit(platform.Run("alerts", run)) }

func run(ctx context.Context, app *platform.App) error {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		return errors.New("DATABASE_URL is required")
	}
	addr := os.Getenv("GRPC_ADDR")
	if addr == "" {
		addr = ":9091"
	}
	cfg, err := alerts.ConfigFromEnv()
	if err != nil {
		return err
	}
	smtpCfg, err := alerts.SMTPConfigFromEnv()
	if err != nil {
		return err
	}
	opts := alerts.RunOptions{DSN: dsn, GRPCAddr: addr, Notifier: alerts.NewNotifier(smtpCfg, app.Log)}
	if v := os.Getenv("ALERTS_NOTIFY_RETRY"); v != "" {
		if opts.Retry, err = time.ParseDuration(v); err != nil {
			return fmt.Errorf("ALERTS_NOTIFY_RETRY: %w", err)
		}
	}
	if v := os.Getenv("ALERTS_NOTIFY_MAX_ATTEMPTS"); v != "" {
		n, err := strconv.ParseInt(v, 10, 32)
		if err != nil {
			return fmt.Errorf("ALERTS_NOTIFY_MAX_ATTEMPTS: %w", err)
		}
		opts.MaxAttempt = int32(n)
	}
	if smtpCfg.Host == "" {
		app.Log.Warn("SMTP_HOST is empty; alert notifications will only be logged")
	}
	return alerts.Run(ctx, app, cfg, opts)
}
