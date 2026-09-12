-- Phase 4: raw rain-gauge uplinks (fPort 5) and the rainfall poll index.

-- One row per fPort 5 uplink, stored as reported. weather derives rainfall
-- rows from consecutive tip_count deltas, so a lost uplink loses no rain.
-- ts is the uplink's event time, i.e. the END of the interval it reports.
CREATE TABLE rain_gauge_uplinks (
  device_id     text         NOT NULL REFERENCES devices(dev_eui),
  ts            timestamptz  NOT NULL,
  f_cnt         bigint       NOT NULL,
  tip_count     bigint       NOT NULL CHECK (tip_count >= 0),   -- tips since boot
  mm_per_tip    numeric(6,3) NOT NULL CHECK (mm_per_tip > 0),
  interval_s    integer      NOT NULL CHECK (interval_s >= 0),  -- since the node's previous uplink (since boot after a reset)
  batt_mv       integer,
  counter_reset boolean      NOT NULL,
  sensor_fault  boolean      NOT NULL,
  rssi_dbm      smallint,
  snr_db        real,
  sf            smallint,
  gateway_id    text,
  dedup_id      uuid,
  inserted_at   timestamptz  NOT NULL DEFAULT now(),
  PRIMARY KEY (device_id, ts, f_cnt)
) PARTITION BY RANGE (ts);
CREATE INDEX rain_gauge_uplinks_inserted_at ON rain_gauge_uplinks (inserted_at);

-- Monthly partitions like readings (ADR 0004); a DO block so sqlc skips it.
DO $$
BEGIN
  PERFORM partman.create_parent(
    p_parent_table    := 'public.rain_gauge_uplinks',
    p_control         := 'ts',
    p_interval        := '1 month',
    p_type            := 'range',
    p_premake         := 2,
    p_start_partition := '2026-01-01 00:00:00',
    p_default_table   := true,
    p_jobmon          := false
  );
  UPDATE partman.part_config
     SET infinite_time_partitions = true,
         retention                = NULL,
         retention_keep_table     = true
   WHERE parent_table = 'public.rain_gauge_uplinks';
END
$$;

-- storm-analytics polls rainfall by updated_at, not inserted_at: weather
-- rewrites a row when ECCC revises an hour or a late gauge uplink re-splits
-- an interval, and those rewrites must reach the consumer too. Writers only
-- bump updated_at when a value actually changes.
CREATE INDEX rainfall_updated_at ON rainfall (updated_at);
