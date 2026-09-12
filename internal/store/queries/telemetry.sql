-- name: CountReadings :one
SELECT count(*) FROM readings;

-- name: CountCycleEvents :one
SELECT count(*) FROM cycle_events;

-- name: CountStormSummaries :one
SELECT count(*) FROM storm_summaries;

-- name: CountAlarmEvents :one
SELECT count(*) FROM alarm_events;

-- name: CountRainGaugeUplinks :one
SELECT count(*) FROM rain_gauge_uplinks;

-- name: SumStormSummaryCycles :one
SELECT coalesce(sum(cycle_count), 0)::bigint AS cycles FROM storm_summaries;

-- name: FCntStatsByDevice :many
-- Rows and highest frame counter per device across every telemetry table;
-- with contiguous counters rows == max_f_cnt + 1.
SELECT device_id, max(f_cnt)::bigint AS max_f_cnt, count(*)::bigint AS rows
FROM (
  SELECT device_id, f_cnt FROM readings
  UNION ALL SELECT device_id, f_cnt FROM cycle_events
  UNION ALL SELECT device_id, f_cnt FROM storm_summaries
  UNION ALL SELECT device_id, f_cnt FROM alarm_events
  UNION ALL SELECT device_id, f_cnt FROM rain_gauge_uplinks
) u
GROUP BY device_id
ORDER BY device_id;

-- name: GetCycleEvent :one
SELECT * FROM cycle_events WHERE device_id = $1 AND f_cnt = $2;

-- name: ListReadingsForDevice :many
SELECT * FROM readings WHERE device_id = $1 AND ts >= $2 AND ts < $3 ORDER BY ts;
