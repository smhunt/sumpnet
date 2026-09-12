-- Phase 4 weather service: rain-gauge uplinks → rainfall, ECCC rainfall, and
-- rainfall reads for storm-analytics and the alerts outage-risk rule.

-- name: PollRainGaugeUplinks :many
WITH bound AS (
  SELECT max(inserted_at) AS hi FROM (
    SELECT DISTINCT inserted_at FROM rain_gauge_uplinks
    WHERE inserted_at > @after AND inserted_at <= now() - make_interval(secs => @lag_seconds::float8)
    ORDER BY inserted_at LIMIT @max_groups
  ) g
)
SELECT u.* FROM rain_gauge_uplinks u, bound
WHERE u.inserted_at > @after AND u.inserted_at <= bound.hi
ORDER BY u.inserted_at, u.device_id, u.ts, u.f_cnt;

-- name: RainGaugeUplinkBefore :one
SELECT * FROM rain_gauge_uplinks
WHERE device_id = @device_id AND ts < @ts
ORDER BY ts DESC, f_cnt DESC
LIMIT 1;

-- name: RainGaugeUplinkAfter :one
SELECT * FROM rain_gauge_uplinks
WHERE device_id = @device_id AND ts > @ts
ORDER BY ts, f_cnt
LIMIT 1;

-- name: ListRainGaugeUplinks :many
SELECT * FROM rain_gauge_uplinks
WHERE device_id = @device_id AND ts >= @from_ts AND ts <= @to_ts
ORDER BY ts, f_cnt;

-- name: UpsertRainfall :execrows
-- Only an insert or a real change bumps updated_at, which storm-analytics polls.
INSERT INTO rainfall (source, station_id, ts, interval_s, mm)
VALUES (@source, @station_id, @ts, @interval_s, @mm)
ON CONFLICT (source, station_id, ts) DO UPDATE
  SET interval_s = EXCLUDED.interval_s, mm = EXCLUDED.mm, updated_at = now()
  WHERE rainfall.interval_s IS DISTINCT FROM EXCLUDED.interval_s
     OR rainfall.mm IS DISTINCT FROM EXCLUDED.mm;

-- name: DeleteRainfallExcept :execrows
-- Removes a station's rows in [from_ts, to_ts) that a re-derivation no longer produces.
DELETE FROM rainfall
WHERE source = @source AND station_id = @station_id
  AND ts >= @from_ts AND ts < @to_ts
  AND NOT (ts = ANY(@keep::timestamptz[]));

-- name: ListRainfall :many
-- Intervals overlapping [from_ts, to_ts); scan_from bounds the index scan
-- (no interval is longer than from_ts - scan_from).
SELECT source, station_id, ts, interval_s, mm FROM rainfall
WHERE ts >= @scan_from AND ts < @to_ts
  AND ts + make_interval(secs => interval_s) > @from_ts
ORDER BY source, station_id, ts;

-- name: RainfallCoveredUntil :one
-- End of the newest interval of a source: how far its data reach.
SELECT coalesce(max(ts + make_interval(secs => interval_s)), 'epoch'::timestamptz)::timestamptz AS covered_until
FROM rainfall
WHERE source = @source AND ts >= @scan_from;

-- name: RainNear :one
-- Rain recorded by any source in intervals overlapping [from_ts, to_ts), and
-- how far the data reach before to_ts (the outage-risk rain term).
SELECT
  coalesce(sum(mm) FILTER (WHERE ts + make_interval(secs => interval_s) > @from_ts), 0)::float8 AS mm,
  coalesce(max(ts + make_interval(secs => interval_s)), 'epoch'::timestamptz)::timestamptz AS covered_until
FROM rainfall
WHERE ts >= @scan_from AND ts < @to_ts;
