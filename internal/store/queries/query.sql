-- api-gateway (Phase 5) reads. The gateway is read-only: it never writes
-- telemetry or alerts. Per-home rows are reachable only through an
-- owner-scoped query (auth_subject = Clerk user id) or as input to an
-- internal/privacy aggregate (ADR 0005). "Linked" homes are homes with at
-- least one device whose home_id points at them; unlinked devices never count.

-- name: GatewayListSegments :many
SELECT s.id, s.name, s.kind, s.geometry,
       (SELECT count(DISTINCT d.home_id) FROM devices d JOIN homes h ON h.id = d.home_id
        WHERE h.segment_id = s.id AND d.kind = 'house')::bigint AS home_count
FROM segments s
ORDER BY s.id;

-- name: ListOwnedHomes :many
SELECT h.* FROM homes h
JOIN home_owners o ON o.home_id = h.id
WHERE o.auth_subject = @auth_subject
ORDER BY h.created_at, h.id;

-- name: GetOwnedHome :one
SELECT h.* FROM homes h
JOIN home_owners o ON o.home_id = h.id
WHERE o.auth_subject = @auth_subject AND h.id = @home_id;

-- name: ListHomeDevices :many
SELECT * FROM devices WHERE home_id = @home_id ORDER BY dev_eui;

-- name: GetHomeLatestReading :one
-- Newest heartbeat across the home's house devices.
SELECT r.ts, r.batt_mv, r.mains_ok
FROM readings r
WHERE r.device_id IN (SELECT d.dev_eui FROM devices d WHERE d.home_id = @home_id AND d.kind = 'house')
ORDER BY r.ts DESC
LIMIT 1;

-- name: CountOpenAlertsForHome :one
SELECT count(*)::bigint FROM alerts WHERE home_id = @home_id AND resolved_at IS NULL;

-- name: GetHomeLatestBaseflow :one
-- Baseflow storm-analytics used for the home's most recent storm.
SELECT m.baseflow_cpd::float8 AS baseflow_cpd
FROM home_storm_metrics m JOIN storm_events s ON s.id = m.storm_id
WHERE m.home_id = @home_id AND m.baseflow_cpd IS NOT NULL
ORDER BY s.started_at DESC, s.id DESC
LIMIT 1;

-- name: ListHomeStormMetrics :many
SELECT m.storm_id, m.home_id, m.lag_min, m.recession_min, m.volume_l, m.cycles, s.started_at
FROM home_storm_metrics m JOIN storm_events s ON s.id = m.storm_id
WHERE m.home_id = @home_id
ORDER BY s.started_at DESC, s.id DESC
LIMIT @lim;

-- name: ListStormEventsPage :many
-- Newest first, keyset-paged on (started_at, id).
SELECT * FROM storm_events
WHERE (sqlc.narg('since')::timestamptz IS NULL OR started_at >= sqlc.narg('since'))
  AND (sqlc.narg('until')::timestamptz IS NULL OR started_at < sqlc.narg('until'))
  AND (sqlc.narg('cursor_started_at')::timestamptz IS NULL
       OR (started_at, id) < (sqlc.narg('cursor_started_at')::timestamptz, sqlc.narg('cursor_id')::uuid))
ORDER BY started_at DESC, id DESC
LIMIT @lim;

-- name: GetStormEventByID :one
SELECT * FROM storm_events WHERE id = @id;

-- name: ListStormHomeMetrics :many
-- Input to privacy.AggregateStorm only; never returned to a caller as rows.
SELECT h.segment_id::text AS segment_id, m.home_id, m.volume_l, m.lag_min, m.recession_min
FROM home_storm_metrics m
JOIN homes h ON h.id = m.home_id
WHERE m.storm_id = @storm_id AND h.segment_id IS NOT NULL
  AND EXISTS (SELECT 1 FROM devices d WHERE d.home_id = h.id AND d.kind = 'house')
ORDER BY h.segment_id, m.home_id;

-- name: NeighbourhoodClock :one
-- The event-time "now" of the neighbourhood: the newest event from a linked
-- house device, never later than the wall clock. Anchoring the live status
-- window here makes a replay of a past storm animate like a live one.
SELECT coalesce(least(max(d.last_seen_at), now()), now())::timestamptz AS clock
FROM devices d
WHERE d.home_id IS NOT NULL AND d.kind = 'house';

-- name: SegmentActivity :many
-- Input to privacy.AggregateStatus: distinct linked homes with a heartbeat in
-- (window_start, window_end] and the cycles those heartbeats report
-- (cycles_since_last counts storm-mode cycles too).
SELECT h.segment_id::text AS segment_id,
       count(DISTINCT h.id)::bigint AS homes_reporting,
       coalesce(sum(r.cycles_since_last), 0)::bigint AS cycles
FROM readings r
JOIN devices d ON d.dev_eui = r.device_id
JOIN homes h ON h.id = d.home_id
WHERE r.ts > @window_start AND r.ts <= @window_end
  AND d.kind = 'house' AND h.segment_id IS NOT NULL
GROUP BY h.segment_id;

-- name: SegmentOpenAlertCounts :many
-- Input to privacy.AggregateStatus: open alerts on linked homes per segment.
SELECT h.segment_id::text AS segment_id, count(*)::bigint AS active_alerts
FROM alerts a JOIN homes h ON h.id = a.home_id
WHERE a.resolved_at IS NULL AND h.segment_id IS NOT NULL
GROUP BY h.segment_id;

-- name: ListStormEventsChangedSince :many
SELECT * FROM storm_events WHERE updated_at > @after ORDER BY updated_at, id;

-- name: ListSnapshotStormEvents :many
-- Open storms plus the most recent ones, for a WatchNeighbourhood snapshot.
SELECT * FROM storm_events
WHERE status = 'open'
   OR id IN (SELECT r.id FROM storm_events r ORDER BY r.started_at DESC, r.id DESC LIMIT @recent)
ORDER BY started_at, id;

-- name: ListAlertsChangedSince :many
-- Alert rows on homes that changed after @after; routed to owners only.
SELECT sqlc.embed(a), h.segment_id
FROM alerts a JOIN homes h ON h.id = a.home_id
WHERE a.updated_at > @after
ORDER BY a.updated_at, a.id;

-- name: ListOwnersOfHomes :many
SELECT auth_subject, home_id FROM home_owners WHERE home_id = ANY(@home_ids::uuid[]);

-- name: ListOwnerActiveAlerts :many
SELECT sqlc.embed(a), h.segment_id
FROM alerts a
JOIN homes h ON h.id = a.home_id
JOIN home_owners o ON o.home_id = a.home_id
WHERE o.auth_subject = @auth_subject AND a.resolved_at IS NULL
  AND (sqlc.narg('home_id')::uuid IS NULL OR a.home_id = sqlc.narg('home_id'))
ORDER BY a.raised_at DESC, a.id;

-- name: OwnsAlert :one
SELECT EXISTS (
  SELECT 1 FROM alerts a JOIN home_owners o ON o.home_id = a.home_id
  WHERE o.auth_subject = @auth_subject AND a.id = @alert_id
) AS owned;

-- name: OwnsHome :one
SELECT EXISTS (
  SELECT 1 FROM home_owners WHERE auth_subject = @auth_subject AND home_id = @home_id
) AS owned;
