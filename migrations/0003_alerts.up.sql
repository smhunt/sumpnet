-- Phase 3: platform-raised alerts, detector → alerts hand-off, and the
-- watermarks every ADR 0003 consumer persists.

-- One OPEN alert per (device, code); a resolved alert never reopens — a new
-- episode is a new row. code/severity mirror alerts.v1 enum values.
CREATE TABLE alerts (
  id                   uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
  device_id            text        NOT NULL REFERENCES devices(dev_eui),
  home_id              uuid        REFERENCES homes(id),      -- linkage snapshot at raise time; NULL = unlinked device
  code                 smallint    NOT NULL CHECK (code BETWEEN 1 AND 9),
  severity             smallint    NOT NULL CHECK (severity BETWEEN 1 AND 3),
  raised_at            timestamptz NOT NULL,                   -- EVENT time of the trigger, never wall clock
  last_seen_at         timestamptz NOT NULL,                   -- event time the condition was last re-observed
  occurrences          integer     NOT NULL DEFAULT 1,
  acked_at             timestamptz,
  ack_note             text,
  resolved_at          timestamptz,                            -- event time
  resolve_reason       text,
  message              text        NOT NULL,
  source               text        NOT NULL CHECK (source IN ('node', 'heartbeat', 'detector', 'sweep')),
  trigger_key          text        NOT NULL,                   -- '<table>:<device>:<event time>:<f_cnt>' of the first trigger
  notify_state         text        NOT NULL DEFAULT 'pending'
                                   CHECK (notify_state IN ('pending', 'sent', 'suppressed', 'failed')),
  notify_attempts      integer     NOT NULL DEFAULT 0,
  notified_at          timestamptz,
  resolve_notify_state text        NOT NULL DEFAULT 'none'
                                   CHECK (resolve_notify_state IN ('none', 'pending', 'sent', 'failed')),
  resolve_notify_attempts integer  NOT NULL DEFAULT 0,
  updated_at           timestamptz NOT NULL DEFAULT now(),
  inserted_at          timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX alerts_open_uniq      ON alerts (device_id, code) WHERE resolved_at IS NULL;
CREATE INDEX alerts_active_by_home        ON alerts (home_id, raised_at DESC) WHERE resolved_at IS NULL;
CREATE INDEX alerts_notify_pending        ON alerts (raised_at)
  WHERE notify_state IN ('pending', 'failed') OR resolve_notify_state IN ('pending', 'failed');
CREATE INDEX alerts_last_sent             ON alerts (device_id, code, notified_at) WHERE notify_state = 'sent';

-- cycle-detector → alerts: durable, idempotent, auditable (ADR 0003).
CREATE TABLE detections (
  device_id   text        NOT NULL REFERENCES devices(dev_eui),
  code        smallint    NOT NULL CHECK (code BETWEEN 1 AND 9),
  action      smallint    NOT NULL CHECK (action IN (1, 2)),  -- 1 raise, 2 clear
  observed_at timestamptz NOT NULL,                            -- event time
  f_cnt       bigint      NOT NULL,                            -- of the triggering cycle
  detail      jsonb,
  inserted_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (device_id, code, action, observed_at, f_cnt)
);
CREATE INDEX detections_inserted_at ON detections (inserted_at);

-- One row per (consumer, source table).
CREATE TABLE consumer_watermarks (
  consumer    text        NOT NULL,
  source      text        NOT NULL,
  inserted_at timestamptz NOT NULL,
  updated_at  timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (consumer, source)
);

-- Consumers poll by inserted_at. Declared on the partitioned parents so every
-- existing and future child (pg_partman) carries the index.
CREATE INDEX readings_inserted_at     ON readings     (inserted_at);
CREATE INDEX cycle_events_inserted_at ON cycle_events (inserted_at);
CREATE INDEX alarm_events_inserted_at ON alarm_events (inserted_at);
CREATE INDEX storm_summaries_inserted_at ON storm_summaries (inserted_at);
