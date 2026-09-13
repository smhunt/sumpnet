# 0005 — Privacy thresholds: segment aggregation with k ≥ 3
Status: accepted · Date: 2026-09-12

## Context
sumpnet publishes neighbourhood drainage behaviour. Pump cycles, storm
response and outages at one address reveal occupancy, basement condition and
likely insurance exposure. Participation is opt-in and device IDs are
pseudonymous, but a map of a few homes on one street de-anonymises them.
Phases 4 (storm analytics) and 5 (api-gateway, dashboard) are built in
parallel and both need one rule they cannot drift on.

## Decision
- Per-home data (`home_storm_metrics`, readings, cycles, alerts with a
  `home_id`) is only served on an owner-scoped path: the caller's Clerk
  subject must be linked to the home in `home_owners`.
- Every public view aggregates to a street segment and is published only when
  **at least 3 homes report** (`privacy.MinHomes`). Below that the aggregate is
  returned with `suppressed = true` and every numeric field zeroed, including
  the live cycle rate and active-alert count, since either can single out one
  home. The number of reporting homes itself stays visible.
- "Reporting" means distinct linked homes with data for that storm or window;
  unlinked devices (`home_id NULL`) never count.
- The rule lives in `internal/privacy` (pure, 100 % covered). The api-gateway
  builds every public segment view with it; the MCP server reads through
  QueryService and so inherits it. No other package may compute a public
  segment aggregate.

## Consequences
- Small segments on a pilot street stay dark until a third neighbour joins;
  the dashboard must explain the suppression rather than show zeros as data.
- Medians skip homes that never reached a threshold, so a visible segment can
  report a zero median lag when no home responded; clients read that with
  `homes_reporting`.
- The threshold is a constant, not configuration: lowering it is a policy
  change that needs a new ADR.

## Alternatives considered
- **Differential privacy noise:** meaningful only at scales far beyond a
  60-home pilot, and noisy numbers undermine the civic use.
- **k = 5:** safer, but most pilot segments would never publish.
- **Enforce in SQL views:** hides the rule from unit tests and lets a new
  query bypass it; a Go package with tests is harder to skip.
