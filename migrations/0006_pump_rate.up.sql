-- Phase 4 follow-up (owner decision 2026-09-12): storm inflow from the pump.
-- volume_l stays the §9 pit-drop floor. inflow_est_l is the home's pump rate
-- × run time over the same window, pump_rate_lps the rate it used, and
-- pump_rate_source where that rate came from: 'dry_weather' = learned by
-- storm-analytics (pit area × median drop ÷ run time of the dry-weather
-- cycles baseflow uses); 'bucket_test' = measured by the homeowner pouring a
-- known volume into the pit, which takes precedence once recorded. All three
-- are NULL when no rate is available.
ALTER TABLE home_storm_metrics
  ADD COLUMN inflow_est_l     double precision,
  ADD COLUMN pump_rate_lps    double precision,
  ADD COLUMN pump_rate_source text CHECK (pump_rate_source IN ('dry_weather', 'bucket_test')),
  ADD CONSTRAINT home_storm_metrics_pump_rate_complete
    CHECK ((pump_rate_lps IS NULL) = (pump_rate_source IS NULL) AND (inflow_est_l IS NULL) = (pump_rate_lps IS NULL));
