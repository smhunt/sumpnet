-- name: ListCyclesBefore :many
SELECT * FROM cycle_events
WHERE device_id = $1 AND started_at < $2
ORDER BY started_at DESC
LIMIT $3;

-- name: ListReadingsBefore :many
SELECT * FROM readings
WHERE device_id = $1 AND ts < $2
ORDER BY ts DESC
LIMIT $3;

-- name: SetCycleVolume :batchexec
UPDATE cycle_events SET est_volume_l = @est_volume_l
WHERE device_id = @device_id AND started_at = @started_at AND f_cnt = @f_cnt;

-- name: RecomputeCycleVolumes :execrows
-- Backfill after an operator links a device to a home with a pit area.
UPDATE cycle_events c
SET est_volume_l = (h.pit_area_m2 * (c.level_end_mm - c.level_start_mm))::real
FROM devices d
JOIN homes h ON h.id = d.home_id
WHERE c.device_id = d.dev_eui AND d.dev_eui = $1 AND h.pit_area_m2 IS NOT NULL;

-- name: GetDevicePitArea :one
SELECT d.dev_eui, d.home_id, h.pit_area_m2::float8 AS pit_area_m2, h.segment_id
FROM devices d
LEFT JOIN homes h ON h.id = d.home_id
WHERE d.dev_eui = $1;

-- name: InsertDetection :execrows
INSERT INTO detections (device_id, code, action, observed_at, f_cnt, detail)
VALUES ($1, $2, $3, $4, $5, $6)
ON CONFLICT DO NOTHING;

-- name: ListDetections :many
SELECT * FROM detections WHERE device_id = $1 ORDER BY observed_at, code, action;
