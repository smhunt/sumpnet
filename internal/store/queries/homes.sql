-- name: UpsertSegment :exec
-- An empty kind means 'standard' so callers that predate segments.kind keep working.
INSERT INTO segments (id, name, kind) VALUES (@id, @name, coalesce(nullif(@kind::text, ''), 'standard'))
ON CONFLICT (id) DO UPDATE SET name = EXCLUDED.name, kind = EXCLUDED.kind;

-- name: UpsertHome :one
INSERT INTO homes (id, segment_id, pit_area_m2) VALUES ($1, $2, $3)
ON CONFLICT (id) DO UPDATE SET segment_id = EXCLUDED.segment_id, pit_area_m2 = EXCLUDED.pit_area_m2
RETURNING *;

-- name: LinkDevice :exec
UPDATE devices SET home_id = $2 WHERE dev_eui = $1;
