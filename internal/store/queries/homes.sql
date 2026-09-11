-- name: UpsertSegment :exec
INSERT INTO segments (id, name) VALUES ($1, $2)
ON CONFLICT (id) DO UPDATE SET name = EXCLUDED.name;

-- name: UpsertHome :one
INSERT INTO homes (id, segment_id, pit_area_m2) VALUES ($1, $2, $3)
ON CONFLICT (id) DO UPDATE SET segment_id = EXCLUDED.segment_id, pit_area_m2 = EXCLUDED.pit_area_m2
RETURNING *;

-- name: LinkDevice :exec
UPDATE devices SET home_id = $2 WHERE dev_eui = $1;
