# 0004 — Time-series storage: native partitioning + pg_partman
Status: accepted · Date: 2026-09-11

## Context
`readings` (~96 rows/day/device) and `cycle_events` are append-only and
queried by time range. The Phase 8 load test targets 50k devices; the AWS
target is RDS Postgres; privacy requires explicit, per-device deletion rather
than blanket retention.

## Decision
PostgreSQL 16 declarative range partitioning by month on both tables, managed
by pg_partman 5 (`migrations/0002_partman.up.sql`):

- `partman.create_parent(... p_interval := '1 month', p_premake := 2,
  p_start_partition := '2026-01-01')`, `infinite_time_partitions`, no retention.
- The background worker (compose) or a scheduled `run_maintenance()` (RDS)
  keeps two months of partitions ahead.
- Every telemetry primary key includes the partition column, so idempotency is
  enforced per partition and inherited by each child.
- `internal/store` creates any missing month on demand
  (`partman.create_partition_time`) so replays of old storms never land in
  the default partition.

## Consequences
- Time-range queries prune to a few partitions; bulk deletion (if ever) is a
  detach/drop, not a VACUUM problem.
- No compression or continuous aggregates; Phase 4 uses materialised views.
- Operators must know `partman.part_config`, the default partition and
  `partman.check_default()`.
- The chosen image (`postgresql-partman`) and RDS both ship pg_partman, so the
  local and cloud schemas are identical.

## Alternatives considered
- **TimescaleDB** — compression, continuous aggregates and `time_bucket` are
  attractive, but it is unavailable on RDS and would make the AWS deployment a
  different database. Rejected while RDS is the target.
- **Citus** — distribution is not the bottleneck.
- **No partitioning** — fine to ~10⁸ rows, but retention and the load test
  would hit it.
