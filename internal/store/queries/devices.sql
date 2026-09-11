-- name: GetDevice :one
SELECT * FROM devices WHERE dev_eui = $1;

-- name: ListDevices :many
SELECT * FROM devices ORDER BY dev_eui;

-- name: UpsertDevice :one
INSERT INTO devices (dev_eui, home_id, kind, name, installed_at)
VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (dev_eui) DO UPDATE
  SET home_id = EXCLUDED.home_id,
      kind = EXCLUDED.kind,
      name = EXCLUDED.name,
      installed_at = EXCLUDED.installed_at
RETURNING *;
