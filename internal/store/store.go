// Package store is the Postgres access layer: connection pool, migrations,
// sqlc-generated queries, and the bulk idempotent inserts used by ingest.
package store

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"regexp"
	"strconv"
	"strings"
	"sync"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/pgx/v5" // registers the pgx5:// scheme
	"github.com/golang-migrate/migrate/v4/source/iofs"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/smhunt/sumpnet/internal/store/sqlcgen"
	"github.com/smhunt/sumpnet/migrations"
)

// Store wraps a pgx pool. Every pooled connection carries the temp staging
// tables the bulk inserts COPY into (see bulk.go).
type Store struct {
	pool *pgxpool.Pool
	q    *sqlcgen.Queries

	autoRegister bool
	// OnPartitionCreated is called after the safety net creates a partition.
	OnPartitionCreated func(table string)

	mu         sync.Mutex
	seenMonths map[string]struct{} // "table:2026-04"
}

// Option configures New.
type Option func(*Store)

// WithAutoRegister controls whether unknown DevEUIs are inserted into devices
// (home_id NULL) or their rows rejected. Default true.
func WithAutoRegister(on bool) Option { return func(s *Store) { s.autoRegister = on } }

// New connects to dsn, verifies the connection and prepares the pool.
func New(ctx context.Context, dsn string, opts ...Option) (*Store, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("store: parse dsn: %w", err)
	}
	cfg.AfterConnect = createStagingTables
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("store: pool: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("store: ping: %w", err)
	}
	s := &Store{pool: pool, q: sqlcgen.New(pool), autoRegister: true, seenMonths: map[string]struct{}{}}
	for _, o := range opts {
		o(s)
	}
	return s, nil
}

// Close releases the pool.
func (s *Store) Close() { s.pool.Close() }

// Pool exposes the underlying pool for callers that need raw access (tests, LISTEN).
func (s *Store) Pool() *pgxpool.Pool { return s.pool }

// Queries returns the sqlc-generated query set.
func (s *Store) Queries() *sqlcgen.Queries { return s.q }

// --- migrations --------------------------------------------------------------

func migrator(dsn string) (*migrate.Migrate, error) {
	src, err := iofs.New(migrations.FS, ".")
	if err != nil {
		return nil, fmt.Errorf("store: migrations source: %w", err)
	}
	url := "pgx5://" + strings.TrimPrefix(strings.TrimPrefix(dsn, "postgres://"), "postgresql://")
	m, err := migrate.NewWithSourceInstance("iofs", src, url)
	if err != nil {
		return nil, fmt.Errorf("store: migrate: %w", err)
	}
	return m, nil
}

// Migrate applies every pending migration.
func Migrate(_ context.Context, dsn string) error {
	m, err := migrator(dsn)
	if err != nil {
		return err
	}
	defer func() { _, _ = m.Close() }()
	if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return fmt.Errorf("store: migrate up: %w", err)
	}
	return nil
}

// MigrateDown reverts every migration (tests).
func MigrateDown(_ context.Context, dsn string) error {
	m, err := migrator(dsn)
	if err != nil {
		return err
	}
	defer func() { _, _ = m.Close() }()
	if err := m.Down(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return fmt.Errorf("store: migrate down: %w", err)
	}
	return nil
}

var migrationName = regexp.MustCompile(`^(\d+)_.*\.up\.sql$`)

// LatestVersion is the highest embedded migration number.
func LatestVersion() (int64, error) {
	entries, err := fs.ReadDir(migrations.FS, ".")
	if err != nil {
		return 0, err
	}
	var latest int64
	for _, e := range entries {
		m := migrationName.FindStringSubmatch(e.Name())
		if m == nil {
			continue
		}
		v, err := strconv.ParseInt(m[1], 10, 32)
		if err != nil {
			return 0, err
		}
		latest = max(latest, v)
	}
	return latest, nil
}

// ErrSchemaMismatch is returned by CheckSchema when the database is behind or dirty.
var ErrSchemaMismatch = errors.New("store: schema version mismatch")

// CheckSchema verifies the database is at the embedded latest migration.
func (s *Store) CheckSchema(ctx context.Context) error {
	want, err := LatestVersion()
	if err != nil {
		return err
	}
	var got int64
	var dirty bool
	err = s.pool.QueryRow(ctx, `SELECT version, dirty FROM schema_migrations`).Scan(&got, &dirty)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("%w: no migrations applied (want %d)", ErrSchemaMismatch, want)
	}
	if err != nil {
		return fmt.Errorf("store: read schema_migrations: %w", err)
	}
	if dirty || got != want {
		return fmt.Errorf("%w: database at %d (dirty=%t), binary wants %d", ErrSchemaMismatch, got, dirty, want)
	}
	return nil
}
