DROP INDEX IF EXISTS rainfall_updated_at;
DELETE FROM partman.part_config WHERE parent_table = 'public.rain_gauge_uplinks';
DROP TABLE IF EXISTS partman.template_public_rain_gauge_uplinks;
DROP TABLE IF EXISTS rain_gauge_uplinks;
