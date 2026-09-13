package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Result reports what happened to one batch.
type Result struct {
	Accepted   int // new rows
	Duplicates int // rows whose (device_id, time, f_cnt) already existed
	Rejected   int // rows for unknown devices (only when auto-registration is off)
}

// NotifyChannel is the LISTEN/NOTIFY channel ingest signals after each batch (ADR 0003).
const NotifyChannel = "sumpnet_ingest"

// Notification is the JSON payload sent on NotifyChannel: a wake-up hint, no device ids.
type Notification struct {
	Table string    `json:"table"`
	N     int       `json:"n"`
	MinTS time.Time `json:"min_ts"`
	MaxTS time.Time `json:"max_ts"`
}

// Notify sends a wake-up hint on NotifyChannel inside tx (delivered at commit).
// Any ADR 0003 producer — ingest for telemetry tables, cycle-detector for
// detections — calls this once per committed batch.
func Notify(ctx context.Context, tx pgx.Tx, n Notification) error {
	payload, err := json.Marshal(n)
	if err != nil {
		return fmt.Errorf("store: notify payload: %w", err)
	}
	if _, err := tx.Exec(ctx, `SELECT pg_notify($1, $2)`, NotifyChannel, string(payload)); err != nil {
		return fmt.Errorf("store: notify: %w", err)
	}
	return nil
}

// table describes one telemetry table for the bulk path.
type table struct {
	name        string
	stage       string
	cols        []string
	timeCol     string
	partitioned bool
	kind        string // devices.kind given to auto-registered senders
}

var (
	tReadings         = table{"readings", "readings_stage", readingCols, "ts", true, "house"}
	tCycleEvents      = table{"cycle_events", "cycle_events_stage", cycleEventCols, "started_at", true, "house"}
	tStormSummaries   = table{"storm_summaries", "storm_summaries_stage", stormSummaryCols, "window_end", false, "house"}
	tAlarmEvents      = table{"alarm_events", "alarm_events_stage", alarmEventCols, "raised_at", false, "house"}
	tRainGaugeUplinks = table{"rain_gauge_uplinks", "rain_gauge_uplinks_stage", rainGaugeUplinkCols, "ts", true, "rain"}
	allTables         = []table{tReadings, tCycleEvents, tStormSummaries, tAlarmEvents, tRainGaugeUplinks}
)

// createStagingTables runs once per pooled connection: session-scoped temp
// tables that COPY lands in before the idempotent merge. ON COMMIT DELETE ROWS
// leaves them empty for the next batch without catalog churn.
func createStagingTables(ctx context.Context, conn *pgx.Conn) error {
	for _, t := range allTables {
		sql := fmt.Sprintf(`CREATE TEMP TABLE IF NOT EXISTS %s (LIKE %s INCLUDING DEFAULTS) ON COMMIT DELETE ROWS`, t.stage, t.name)
		if _, err := conn.Exec(ctx, sql); err != nil {
			return fmt.Errorf("store: create %s: %w", t.stage, err)
		}
	}
	return nil
}

// InsertReadings stores a batch of heartbeats.
func (s *Store) InsertReadings(ctx context.Context, rows []Reading) (Result, error) {
	vals := make([][]any, len(rows))
	times := make([]time.Time, len(rows))
	for i, r := range rows {
		vals[i], times[i] = r.values(), r.TS
	}
	return s.insert(ctx, tReadings, vals, times)
}

// InsertCycleEvents stores a batch of pump cycles.
func (s *Store) InsertCycleEvents(ctx context.Context, rows []CycleEvent) (Result, error) {
	vals := make([][]any, len(rows))
	times := make([]time.Time, len(rows))
	for i, r := range rows {
		vals[i], times[i] = r.values(), r.StartedAt
	}
	return s.insert(ctx, tCycleEvents, vals, times)
}

// InsertStormSummaries stores a batch of storm-mode roll-ups.
func (s *Store) InsertStormSummaries(ctx context.Context, rows []StormSummary) (Result, error) {
	vals := make([][]any, len(rows))
	times := make([]time.Time, len(rows))
	for i, r := range rows {
		vals[i], times[i] = r.values(), r.WindowEnd
	}
	return s.insert(ctx, tStormSummaries, vals, times)
}

// InsertAlarmEvents stores a batch of node alarms.
func (s *Store) InsertAlarmEvents(ctx context.Context, rows []AlarmEvent) (Result, error) {
	vals := make([][]any, len(rows))
	times := make([]time.Time, len(rows))
	for i, r := range rows {
		vals[i], times[i] = r.values(), r.RaisedAt
	}
	return s.insert(ctx, tAlarmEvents, vals, times)
}

// InsertRainGaugeUplinks stores a batch of fPort 5 rain gauge reports.
func (s *Store) InsertRainGaugeUplinks(ctx context.Context, rows []RainGaugeUplink) (Result, error) {
	vals := make([][]any, len(rows))
	times := make([]time.Time, len(rows))
	for i, r := range rows {
		vals[i], times[i] = r.values(), r.TS
	}
	return s.insert(ctx, tRainGaugeUplinks, vals, times)
}

// insert is the shared bulk path: ensure partitions → COPY into the staging
// table → register devices → INSERT … ON CONFLICT DO NOTHING → NOTIFY.
func (s *Store) insert(ctx context.Context, t table, vals [][]any, times []time.Time) (Result, error) {
	if len(vals) == 0 {
		return Result{}, nil
	}
	minTS, maxTS := times[0], times[0]
	for _, ts := range times[1:] {
		if ts.Before(minTS) {
			minTS = ts
		}
		if ts.After(maxTS) {
			maxTS = ts
		}
	}
	if t.partitioned {
		if err := s.ensurePartitions(ctx, t, times); err != nil {
			return Result{}, err
		}
	}

	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		return Result{}, fmt.Errorf("store: acquire: %w", err)
	}
	defer conn.Release()

	tx, err := conn.Begin(ctx)
	if err != nil {
		return Result{}, fmt.Errorf("store: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	copied, err := tx.CopyFrom(ctx, pgx.Identifier{t.stage}, t.cols, pgx.CopyFromRows(vals))
	if err != nil {
		return Result{}, fmt.Errorf("store: copy into %s: %w", t.stage, err)
	}

	var res Result
	if s.autoRegister {
		// A new sender is registered with the table's device kind; an existing
		// device keeps whatever kind an operator gave it.
		_, err = tx.Exec(ctx, fmt.Sprintf(`
			INSERT INTO devices (dev_eui, kind, last_seen_at)
			SELECT device_id, $1, max(%s) FROM %s GROUP BY device_id
			ON CONFLICT (dev_eui) DO UPDATE
			  SET last_seen_at = GREATEST(devices.last_seen_at, EXCLUDED.last_seen_at)`, t.timeCol, t.stage), t.kind)
		if err != nil {
			return Result{}, fmt.Errorf("store: register devices: %w", err)
		}
	} else {
		del, delErr := tx.Exec(ctx, fmt.Sprintf(`DELETE FROM %s WHERE device_id NOT IN (SELECT dev_eui FROM devices)`, t.stage))
		if delErr != nil {
			return Result{}, fmt.Errorf("store: reject unknown devices: %w", delErr)
		}
		res.Rejected = int(del.RowsAffected())
		_, err = tx.Exec(ctx, fmt.Sprintf(`
			UPDATE devices d SET last_seen_at = GREATEST(d.last_seen_at, m.max_ts)
			FROM (SELECT device_id, max(%s) AS max_ts FROM %s GROUP BY device_id) m
			WHERE d.dev_eui = m.device_id`, t.timeCol, t.stage))
		if err != nil {
			return Result{}, fmt.Errorf("store: touch devices: %w", err)
		}
	}

	cols := strings.Join(t.cols, ", ")
	tag, err := tx.Exec(ctx, fmt.Sprintf(`INSERT INTO %s (%s) SELECT %s FROM %s ON CONFLICT DO NOTHING`, t.name, cols, cols, t.stage))
	if err != nil {
		return Result{}, fmt.Errorf("store: merge into %s: %w", t.name, err)
	}
	res.Accepted = int(tag.RowsAffected())
	res.Duplicates = int(copied) - res.Accepted - res.Rejected

	if res.Accepted > 0 {
		if err := Notify(ctx, tx, Notification{Table: t.name, N: res.Accepted, MinTS: minTS.UTC(), MaxTS: maxTS.UTC()}); err != nil {
			return Result{}, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return Result{}, fmt.Errorf("store: commit: %w", err)
	}
	return res, nil
}

// ensurePartitions creates any monthly partition the batch needs that this
// process has not seen yet (replays of past storms). Idempotent; a race with
// another replica surfaces as duplicate_table and is retried once.
func (s *Store) ensurePartitions(ctx context.Context, t table, times []time.Time) error {
	months := map[string]time.Time{}
	for _, ts := range times {
		u := ts.UTC()
		m := time.Date(u.Year(), u.Month(), 1, 0, 0, 0, 0, time.UTC)
		months[t.name+":"+m.Format("2006-01")] = m
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for key, m := range months {
		if _, ok := s.seenMonths[key]; ok {
			continue
		}
		created, err := s.createPartition(ctx, t.name, m)
		if err != nil {
			return err
		}
		if created && s.OnPartitionCreated != nil {
			s.OnPartitionCreated(t.name)
		}
		s.seenMonths[key] = struct{}{}
	}
	return nil
}

func (s *Store) createPartition(ctx context.Context, tableName string, month time.Time) (bool, error) {
	const q = `SELECT partman.create_partition_time($1, ARRAY[$2::timestamptz])`
	var created bool
	for attempt := 0; ; attempt++ {
		err := s.pool.QueryRow(ctx, q, "public."+tableName, month).Scan(&created)
		if err == nil {
			return created, nil
		}
		var pgErr *pgconn.PgError
		if attempt == 0 && errors.As(err, &pgErr) && pgErr.Code == "42P07" { // duplicate_table: another replica won
			continue
		}
		return false, fmt.Errorf("store: create partition %s %s: %w", tableName, month.Format("2006-01"), err)
	}
}
