-- Phase 4 storm-analytics: storm_events and home_storm_metrics maintenance.
-- storm-analytics is the single writer of both tables.

-- name: PollRainfall :many
-- Polled by updated_at (see migration 0005): rewrites must reach the consumer.
WITH bound AS (
  SELECT max(updated_at) AS hi FROM (
    SELECT DISTINCT updated_at FROM rainfall
    WHERE updated_at > @after AND updated_at <= now() - make_interval(secs => @lag_seconds::float8)
    ORDER BY updated_at LIMIT @max_groups
  ) g
)
SELECT r.* FROM rainfall r, bound
WHERE r.updated_at > @after AND r.updated_at <= bound.hi
ORDER BY r.updated_at, r.source, r.station_id, r.ts;

-- name: ListStormEventsOverlapping :many
-- Storms whose [started_at, ended_at] meets [from_ts, to_ts); open storms reach forward indefinitely.
SELECT * FROM storm_events
WHERE started_at < @to_ts::timestamptz AND coalesce(ended_at, 'infinity'::timestamptz) > @from_ts::timestamptz
ORDER BY started_at, id;

-- name: InsertStormEvent :one
INSERT INTO storm_events (started_at, ended_at, total_rain_mm, peak_intensity_mm_h, rain_source, status)
VALUES (@started_at, @ended_at, @total_rain_mm, @peak_intensity_mm_h, @rain_source, @status)
RETURNING id;

-- name: UpdateStormEvent :exec
UPDATE storm_events
SET started_at = @started_at, ended_at = @ended_at, total_rain_mm = @total_rain_mm,
    peak_intensity_mm_h = @peak_intensity_mm_h, rain_source = @rain_source, status = @status,
    updated_at = now()
WHERE id = @id;

-- name: DeleteStormEvent :exec
DELETE FROM storm_events WHERE id = @id;

-- name: ListHomeMetricsForStorm :many
SELECT * FROM home_storm_metrics WHERE storm_id = @storm_id ORDER BY home_id;

-- name: UpsertHomeStormMetrics :exec
INSERT INTO home_storm_metrics (storm_id, home_id, lag_min, recession_min, volume_l, cycles, baseflow_cpd, inflow_est_l, pump_rate_lps, pump_rate_source, computed_at)
VALUES (@storm_id, @home_id, @lag_min, @recession_min, @volume_l, @cycles, @baseflow_cpd, @inflow_est_l, @pump_rate_lps, @pump_rate_source, now())
ON CONFLICT (storm_id, home_id) DO UPDATE
  SET lag_min = EXCLUDED.lag_min, recession_min = EXCLUDED.recession_min, volume_l = EXCLUDED.volume_l,
      cycles = EXCLUDED.cycles, baseflow_cpd = EXCLUDED.baseflow_cpd,
      inflow_est_l = EXCLUDED.inflow_est_l, pump_rate_lps = EXCLUDED.pump_rate_lps,
      pump_rate_source = EXCLUDED.pump_rate_source, computed_at = now();

-- name: DeleteHomeStormMetrics :exec
DELETE FROM home_storm_metrics WHERE storm_id = @storm_id AND home_id = @home_id;

-- name: ListLinkedHouseDevices :many
-- House nodes linked to a home; last_seen_at is the newest event time ingest saw.
SELECT d.dev_eui, h.id AS home_id, coalesce(h.pit_area_m2, 0)::float8 AS pit_area_m2, d.last_seen_at
FROM devices d
JOIN homes h ON h.id = d.home_id
WHERE d.kind = 'house'
ORDER BY h.id, d.dev_eui;

-- name: GetLinkedHouseDevice :one
SELECT d.dev_eui, h.id AS home_id, coalesce(h.pit_area_m2, 0)::float8 AS pit_area_m2, d.last_seen_at
FROM devices d
JOIN homes h ON h.id = d.home_id
WHERE d.kind = 'house' AND d.dev_eui = @dev_eui;

-- name: ListCycleEventsForDevice :many
SELECT started_at, run_s, level_start_mm, level_end_mm, pump_id FROM cycle_events
WHERE device_id = @device_id AND started_at >= @from_ts AND started_at < @to_ts
ORDER BY started_at, f_cnt;

-- name: ListStormSummariesForDevice :many
SELECT window_end, window_s, cycle_count, total_run_s FROM storm_summaries
WHERE device_id = @device_id AND window_end >= @from_ts AND window_end < @to_ts
ORDER BY window_end, f_cnt;

-- name: ListLevelsForDevice :many
SELECT ts, level_mm FROM readings
WHERE device_id = @device_id AND ts >= @from_ts AND ts < @to_ts AND NOT sensor_fault
ORDER BY ts, f_cnt;
