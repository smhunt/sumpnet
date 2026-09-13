-- Phase 4/5 shared contract: rainfall, storm analytics outputs, owner links
-- and segment kind. Written once so the weather/storm-analytics (Phase 4) and
-- api-gateway (Phase 5) work can proceed in parallel against one schema.

-- Segment kind mirrors internal/sim SegmentKind.String() and query.v1.SegmentKind.
ALTER TABLE segments
  ADD COLUMN kind text NOT NULL DEFAULT 'standard'
  CHECK (kind IN ('standard', 'wooded', 'near_pond', 'high_ground'));

-- Rainfall per station interval, from own gauges (fPort 5 tip deltas) and ECCC.
-- ts is the interval START (UTC); interval_s is its length.
CREATE TABLE rainfall (
  source      text        NOT NULL CHECK (source IN ('gauge', 'eccc')),
  station_id  text        NOT NULL,   -- gauge dev_eui, or ECCC CLIMATE_IDENTIFIER
  ts          timestamptz NOT NULL,
  interval_s  integer     NOT NULL CHECK (interval_s > 0),
  mm          numeric(7,2) NOT NULL CHECK (mm >= 0),
  inserted_at timestamptz NOT NULL DEFAULT now(),
  updated_at  timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (source, station_id, ts)
);
CREATE INDEX rainfall_ts          ON rainfall (ts);
CREATE INDEX rainfall_inserted_at ON rainfall (inserted_at);

-- §10 storm events. ended_at is the rain end; NULL while the storm is open
-- (less than 6 h since the last rain).
CREATE TABLE storm_events (
  id                  uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
  started_at          timestamptz NOT NULL,   -- rain onset (first interval ≥ 1 mm/h)
  ended_at            timestamptz,
  total_rain_mm       numeric(7,2) NOT NULL CHECK (total_rain_mm >= 0),
  peak_intensity_mm_h numeric(7,2) NOT NULL CHECK (peak_intensity_mm_h >= 0),
  rain_source         text        NOT NULL CHECK (rain_source IN ('gauge', 'eccc')),
  status              text        NOT NULL DEFAULT 'open' CHECK (status IN ('open', 'closed')),
  inserted_at         timestamptz NOT NULL DEFAULT now(),
  updated_at          timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX storm_events_started_at ON storm_events (started_at DESC);

-- Per-home response to one storm. Owner-scoped: only the api-gateway's
-- owner path and internal/privacy aggregates may read it.
CREATE TABLE home_storm_metrics (
  storm_id      uuid        NOT NULL REFERENCES storm_events(id) ON DELETE CASCADE,
  home_id       uuid        NOT NULL REFERENCES homes(id),
  lag_min       double precision,   -- NULL: cycle rate never exceeded 2× baseflow
  recession_min double precision,   -- NULL: not yet back within 1.2× baseflow
  volume_l      double precision NOT NULL DEFAULT 0,
  cycles        integer     NOT NULL DEFAULT 0,
  baseflow_cpd  double precision,   -- baseflow the lag/recession thresholds used
  computed_at   timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (storm_id, home_id)
);
CREATE INDEX home_storm_metrics_home ON home_storm_metrics (home_id, storm_id);

-- Owner ↔ home links. auth_subject is the Clerk user id (JWT sub claim).
CREATE TABLE home_owners (
  auth_subject text        NOT NULL,
  home_id      uuid        NOT NULL REFERENCES homes(id) ON DELETE CASCADE,
  created_at   timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (auth_subject, home_id)
);
CREATE INDEX home_owners_home ON home_owners (home_id);
