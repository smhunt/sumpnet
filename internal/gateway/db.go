package gateway

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/smhunt/sumpnet/internal/store"
)

// readOnlyParams make every session default to read-only transactions, so no
// code path in the gateway can write telemetry or alerts even by mistake.
var readOnlyParams = map[string]string{
	"default_transaction_read_only": "on",
	"application_name":              "sumpnet-api-gateway",
}

// OpenReadOnly opens the gateway's connection pool.
func OpenReadOnly(ctx context.Context, dsn string) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("gateway: parse dsn: %w", err)
	}
	for k, v := range readOnlyParams {
		cfg.ConnConfig.RuntimeParams[k] = v
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("gateway: pool: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("gateway: ping: %w", err)
	}
	return pool, nil
}

// connectListener opens the dedicated LISTEN connection (read-only too).
func connectListener(ctx context.Context, dsn string) (*pgx.Conn, error) {
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		return nil, err
	}
	for k, v := range readOnlyParams {
		cfg.RuntimeParams[k] = v
	}
	return pgx.ConnectConfig(ctx, cfg)
}

// CheckSchema verifies the database is at this binary's latest migration
// (store.CheckSchema needs a writable store; the gateway's pool is read-only).
func CheckSchema(ctx context.Context, pool *pgxpool.Pool) error {
	want, err := store.LatestVersion()
	if err != nil {
		return err
	}
	var got int64
	var dirty bool
	err = pool.QueryRow(ctx, `SELECT version, dirty FROM schema_migrations`).Scan(&got, &dirty)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("%w: no migrations applied (want %d)", store.ErrSchemaMismatch, want)
	}
	if err != nil {
		return fmt.Errorf("gateway: read schema_migrations: %w", err)
	}
	if dirty || got != want {
		return fmt.Errorf("%w: database at %d (dirty=%t), binary wants %d", store.ErrSchemaMismatch, got, dirty, want)
	}
	return nil
}
