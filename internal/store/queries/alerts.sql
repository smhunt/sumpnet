-- The alerts service is the single writer of this table.

-- name: GetOpenAlert :one
SELECT * FROM alerts WHERE device_id = $1 AND code = $2 AND resolved_at IS NULL;

-- name: InsertAlert :one
INSERT INTO alerts (device_id, home_id, code, severity, raised_at, last_seen_at, message, source, trigger_key, notify_state)
VALUES (@device_id, @home_id, @code, @severity, @raised_at, @raised_at, @message, @source, @trigger_key, @notify_state)
RETURNING *;

-- name: TouchAlert :exec
UPDATE alerts
SET last_seen_at = GREATEST(last_seen_at, $2), occurrences = occurrences + 1, updated_at = now()
WHERE id = $1;

-- name: ResolveOpenAlert :one
UPDATE alerts
SET resolved_at = @resolved_at, resolve_reason = @resolve_reason,
    resolve_notify_state = CASE WHEN @notify_resolve::boolean AND notify_state = 'sent' THEN 'pending' ELSE 'none' END,
    updated_at = now()
WHERE device_id = @device_id AND code = @code AND resolved_at IS NULL
RETURNING *;

-- name: LastNotifiedAt :one
SELECT max(notified_at)::timestamptz AS notified_at
FROM alerts WHERE device_id = $1 AND code = $2 AND notify_state = 'sent';

-- name: ListPendingRaiseNotifications :many
SELECT sqlc.embed(a), h.segment_id
FROM alerts a LEFT JOIN homes h ON h.id = a.home_id
WHERE (a.notify_state = 'pending')
   OR (a.notify_state = 'failed' AND a.notify_attempts < @max_attempts
       AND a.updated_at < now() - make_interval(secs => @retry_seconds::float8))
ORDER BY a.raised_at
LIMIT @lim;

-- name: ListPendingResolveNotifications :many
SELECT sqlc.embed(a), h.segment_id
FROM alerts a LEFT JOIN homes h ON h.id = a.home_id
WHERE (a.resolve_notify_state = 'pending')
   OR (a.resolve_notify_state = 'failed' AND a.resolve_notify_attempts < @max_attempts
       AND a.updated_at < now() - make_interval(secs => @retry_seconds::float8))
ORDER BY a.resolved_at
LIMIT @lim;

-- name: MarkRaiseNotification :exec
UPDATE alerts
SET notify_state = @state,
    notified_at = CASE WHEN @state::text = 'sent' THEN now() ELSE notified_at END,
    notify_attempts = notify_attempts + 1,
    updated_at = now()
WHERE id = @id;

-- name: MarkResolveNotification :exec
UPDATE alerts
SET resolve_notify_state = @state,
    resolve_notify_attempts = resolve_notify_attempts + 1,
    updated_at = now()
WHERE id = @id;

-- name: ListActiveAlerts :many
SELECT sqlc.embed(a), h.segment_id
FROM alerts a LEFT JOIN homes h ON h.id = a.home_id
WHERE a.resolved_at IS NULL
  AND (sqlc.narg('home_id')::uuid IS NULL OR a.home_id = sqlc.narg('home_id'))
  AND (sqlc.narg('segment_id')::text IS NULL OR h.segment_id = sqlc.narg('segment_id'))
ORDER BY a.raised_at DESC;

-- name: GetAlert :one
SELECT sqlc.embed(a), h.segment_id
FROM alerts a LEFT JOIN homes h ON h.id = a.home_id
WHERE a.id = $1;

-- name: AcknowledgeAlert :one
UPDATE alerts
SET acked_at = coalesce(acked_at, @acked_at), ack_note = coalesce(ack_note, @note), updated_at = now()
WHERE id = @id
RETURNING *;

-- name: CountOpenAlertsByCode :many
SELECT code, count(*)::bigint AS n FROM alerts WHERE resolved_at IS NULL GROUP BY code ORDER BY code;

-- name: ListAlertsForDevice :many
SELECT * FROM alerts WHERE device_id = $1 ORDER BY raised_at, code;

-- name: ListStaleDevices :many
-- Devices silent longer than the given number of seconds (wall clock).
SELECT dev_eui, home_id, last_seen_at
FROM devices
WHERE kind = 'house' AND last_seen_at IS NOT NULL
  AND last_seen_at < now() - make_interval(secs => @after_seconds::float8);
