// Command alerts raises, acknowledges and resolves alerts from node alarms,
// heartbeats and cycle-detector output, emails an operator, and serves
// alerts.v1.AlertService.
//
// `alerts testmail` sends one delivery check through the configured SMTP
// provider and exits; it needs only the SMTP_* and ALERTS_TO variables.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/smhunt/sumpnet/internal/alerts"
	"github.com/smhunt/sumpnet/internal/platform"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "testmail" {
		os.Exit(testmail())
	}
	os.Exit(platform.Run("alerts", run))
}

func testmail() int {
	cfg, err := alerts.SMTPConfigFromEnv()
	if err != nil {
		fmt.Fprintln(os.Stderr, "testmail:", err)
		return 1
	}
	if cfg.Host == "" {
		fmt.Fprintln(os.Stderr, "testmail: SMTP_HOST is empty; set SMTP_* and ALERTS_TO in deploy/compose/.env")
		return 1
	}
	ctx, cancel := context.WithTimeout(context.Background(), cfg.Timeout+5*time.Second)
	defer cancel()
	n := alerts.NewNotifier(cfg, slog.New(slog.NewJSONHandler(os.Stderr, nil)))
	if err := n.Notify(ctx, alerts.TestMessage(time.Now())); err != nil {
		fmt.Fprintln(os.Stderr, "testmail:", err)
		return 1
	}
	fmt.Printf("testmail: sent via %s:%d to %s\n", cfg.Host, cfg.Port, strings.Join(cfg.To, ", "))
	return 0
}

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
