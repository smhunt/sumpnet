-- Base schema (prompt_plan.md §9). Telemetry tables are keyed on
-- (device_id, <event time>, f_cnt): that key is the ingest idempotency contract
-- and, because the time column is also the partition key, it is legal on the
-- partitioned parents and inherited by every child partition.

CREATE SCHEMA IF NOT EXISTS partman;
CREATE EXTENSION IF NOT EXISTS pg_partman SCHEMA partman;

CREATE TABLE segments (
  id          text PRIMARY KEY,
  name        text NOT NULL,
  geometry    jsonb,
  created_at  timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE homes (
  id                      uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  segment_id              text REFERENCES segments(id),
  pit_area_m2             numeric(6,4),
  consent_at              timestamptz,
  owner_contact_encrypted bytea,
  created_at              timestamptz NOT NULL DEFAULT now()
);

-- home_id stays NULL until an operator links the device; unlinked devices are
-- invisible to owner views and excluded from segment aggregates.
CREATE TABLE devices (
  dev_eui        text PRIMARY KEY CHECK (dev_eui ~ '^[0-9a-f]{16}$'),
  home_id        uuid REFERENCES homes(id),
  kind           text NOT NULL DEFAULT 'house' CHECK (kind IN ('house', 'rain')),
  name           text,
  installed_at   timestamptz,
  first_seen_at  timestamptz NOT NULL DEFAULT now(),
  last_seen_at   timestamptz
);

CREATE TYPE pump_kind AS ENUM ('primary', 'backup');

-- fPort 1 heartbeats. ts is the event time reported by the node/network,
-- never the time the platform received it.
CREATE TABLE readings (
  device_id          text        NOT NULL REFERENCES devices(dev_eui),
  ts                 timestamptz NOT NULL,
  f_cnt              bigint      NOT NULL,
  level_mm           integer     NOT NULL,
  temp_c             real,
  rh_pct             smallint,
  batt_mv            integer,
  cycles_since_last  smallint    NOT NULL DEFAULT 0,
  mains_ok           boolean     NOT NULL,
  float_high         boolean     NOT NULL,
  backup_ran         boolean     NOT NULL,
  sensor_fault       boolean     NOT NULL,
  rssi_dbm           smallint,
  snr_db             real,
  sf                 smallint,
  gateway_id         text,
  dedup_id           uuid,
  inserted_at        timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (device_id, ts, f_cnt)
) PARTITION BY RANGE (ts);

-- fPort 2 pump cycles. started_at = received_at - start_offset_s, derived from
-- the frame, so a redelivered frame yields the same key.
CREATE TABLE cycle_events (
  device_id        text        NOT NULL REFERENCES devices(dev_eui),
  started_at       timestamptz NOT NULL,
  f_cnt            bigint      NOT NULL,
  received_at      timestamptz NOT NULL,
  run_s            integer     NOT NULL,
  peak_current_a   real        NOT NULL,
  level_start_mm   integer     NOT NULL,
  level_end_mm     integer     NOT NULL,
  pump_id          pump_kind   NOT NULL,
  est_volume_l     real,
  rssi_dbm         smallint,
  snr_db           real,
  sf               smallint,
  gateway_id       text,
  dedup_id         uuid,
  inserted_at      timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (device_id, started_at, f_cnt)
) PARTITION BY RANGE (started_at);

-- fPort 4 storm-mode roll-ups.
CREATE TABLE storm_summaries (
  device_id          text        NOT NULL REFERENCES devices(dev_eui),
  window_end         timestamptz NOT NULL,
  f_cnt              bigint      NOT NULL,
  window_s           integer     NOT NULL,
  cycle_count        integer     NOT NULL,
  total_run_s        integer     NOT NULL,
  max_peak_current_a real        NOT NULL,
  min_level_mm       integer     NOT NULL,
  rssi_dbm           smallint,
  snr_db             real,
  sf                 smallint,
  gateway_id         text,
  dedup_id           uuid,
  inserted_at        timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (device_id, window_end, f_cnt)
);

-- fPort 3 alarms as raised by the node. Platform-raised alerts with an
-- ack/resolve lifecycle are a separate table (Phase 3).
CREATE TABLE alarm_events (
  device_id   text        NOT NULL REFERENCES devices(dev_eui),
  raised_at   timestamptz NOT NULL,
  f_cnt       bigint      NOT NULL,
  code        smallint    NOT NULL CHECK (code BETWEEN 1 AND 5),
  value       integer     NOT NULL,
  rssi_dbm    smallint,
  snr_db      real,
  sf          smallint,
  gateway_id  text,
  dedup_id    uuid,
  inserted_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (device_id, raised_at, f_cnt)
);
