-- Monthly range partitions managed by pg_partman 5 (ADR 0004). Wrapped in a
-- DO block so sqlc's schema parser skips it and golang-migrate runs it as one
-- statement. Partitions exist from the project epoch (2026-01) through
-- now() + 2 months; the background worker keeps 2 months ahead and ingest
-- creates any missing month on demand for replays of older data.
DO $$
BEGIN
  PERFORM partman.create_parent(
    p_parent_table    := 'public.readings',
    p_control         := 'ts',
    p_interval        := '1 month',
    p_type            := 'range',
    p_premake         := 2,
    p_start_partition := '2026-01-01 00:00:00',
    p_default_table   := true,
    p_jobmon          := false
  );
  PERFORM partman.create_parent(
    p_parent_table    := 'public.cycle_events',
    p_control         := 'started_at',
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
   WHERE parent_table IN ('public.readings', 'public.cycle_events');
END
$$;
