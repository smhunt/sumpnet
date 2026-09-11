DELETE FROM partman.part_config WHERE parent_table IN ('public.readings', 'public.cycle_events');
DROP TABLE IF EXISTS partman.template_public_readings;
DROP TABLE IF EXISTS partman.template_public_cycle_events;
