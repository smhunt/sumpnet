ALTER TABLE home_storm_metrics
  DROP CONSTRAINT IF EXISTS home_storm_metrics_pump_rate_complete,
  DROP COLUMN IF EXISTS pump_rate_source,
  DROP COLUMN IF EXISTS pump_rate_lps,
  DROP COLUMN IF EXISTS inflow_est_l;
