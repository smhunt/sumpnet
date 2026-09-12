-- name: GetWatermark :one
SELECT inserted_at FROM consumer_watermarks WHERE consumer = $1 AND source = $2;

-- name: SetWatermark :exec
INSERT INTO consumer_watermarks (consumer, source, inserted_at)
VALUES ($1, $2, $3)
ON CONFLICT (consumer, source) DO UPDATE
  SET inserted_at = EXCLUDED.inserted_at, updated_at = now();

-- name: LagBoundary :one
-- The newest inserted_at a poller may read: now() minus the lag that covers
-- in-flight ingest transactions. Computed in SQL so only the DB clock matters.
SELECT (now() - make_interval(secs => @lag_seconds::float8))::timestamptz AS boundary;

-- Poll queries: rows newer than the watermark, never splitting an inserted_at
-- group (all rows of one ingest batch share it), bounded to max_groups groups
-- and to the lag boundary.

-- name: PollCycleEvents :many
WITH bound AS (
  SELECT max(inserted_at) AS hi FROM (
    SELECT DISTINCT inserted_at FROM cycle_events
    WHERE inserted_at > @after AND inserted_at <= now() - make_interval(secs => @lag_seconds::float8)
    ORDER BY inserted_at LIMIT @max_groups
  ) g
)
SELECT c.* FROM cycle_events c, bound
WHERE c.inserted_at > @after AND c.inserted_at <= bound.hi
ORDER BY c.inserted_at, c.device_id, c.started_at, c.f_cnt;

-- name: PollReadings :many
WITH bound AS (
  SELECT max(inserted_at) AS hi FROM (
    SELECT DISTINCT inserted_at FROM readings
    WHERE inserted_at > @after AND inserted_at <= now() - make_interval(secs => @lag_seconds::float8)
    ORDER BY inserted_at LIMIT @max_groups
  ) g
)
SELECT r.* FROM readings r, bound
WHERE r.inserted_at > @after AND r.inserted_at <= bound.hi
ORDER BY r.inserted_at, r.device_id, r.ts, r.f_cnt;

-- name: PollAlarmEvents :many
WITH bound AS (
  SELECT max(inserted_at) AS hi FROM (
    SELECT DISTINCT inserted_at FROM alarm_events
    WHERE inserted_at > @after AND inserted_at <= now() - make_interval(secs => @lag_seconds::float8)
    ORDER BY inserted_at LIMIT @max_groups
  ) g
)
SELECT a.* FROM alarm_events a, bound
WHERE a.inserted_at > @after AND a.inserted_at <= bound.hi
ORDER BY a.inserted_at, a.device_id, a.raised_at, a.f_cnt;

-- name: PollDetections :many
WITH bound AS (
  SELECT max(inserted_at) AS hi FROM (
    SELECT DISTINCT inserted_at FROM detections
    WHERE inserted_at > @after AND inserted_at <= now() - make_interval(secs => @lag_seconds::float8)
    ORDER BY inserted_at LIMIT @max_groups
  ) g
)
SELECT d.* FROM detections d, bound
WHERE d.inserted_at > @after AND d.inserted_at <= bound.hi
ORDER BY d.inserted_at, d.device_id, d.observed_at, d.f_cnt;

-- name: PollStormSummaries :many
WITH bound AS (
  SELECT max(inserted_at) AS hi FROM (
    SELECT DISTINCT inserted_at FROM storm_summaries
    WHERE inserted_at > @after AND inserted_at <= now() - make_interval(secs => @lag_seconds::float8)
    ORDER BY inserted_at LIMIT @max_groups
  ) g
)
SELECT s.* FROM storm_summaries s, bound
WHERE s.inserted_at > @after AND s.inserted_at <= bound.hi
ORDER BY s.inserted_at, s.device_id, s.window_end, s.f_cnt;
