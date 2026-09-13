# 0007 — Storm analytics: recompute from source, gauge-first rainfall
Status: accepted (acceptance tolerance pending, see §14) · Date: 2026-09-12

## Context
Phase 4 turns rain-gauge uplinks and ECCC observations into `rainfall`, and
rainfall plus pump telemetry into `storm_events` and `home_storm_metrics`,
which Phase 5 serves and streams. Every input can arrive late or change:
MQTT redelivers, a gauge uplink is lost and another covers its interval,
ECCC publishes hours late and revises recent values, simulator replays load
months-old storms. A storm's onset, end and total move while it rains, and a
home's lag and recession are only known hours or days later. The §10
definitions must stay pure Go functions (`internal/hydrology`), and the
measurements are coarse: 0.2 mm tips, a 5-minute gauge wake, 15-minute
heartbeats, individual cycles hours apart at baseflow.

## Decision
- **Raw first.** fPort 5 uplinks are stored as reported
  (`rain_gauge_uplinks`, partitioned like `readings`). `weather` re-derives
  the rainfall of every stretch an uplink touches (its predecessor to its
  successor) and reconciles the gauge's rows: deltas over the previous stored
  uplink, so a lost uplink widens an interval without losing rain;
  `counter_reset` counts from boot; tips reported after an interval longer
  than the 5-minute wake fell in its last 5 minutes (§5 cadence).
- **Changes are visible.** Writers bump `rainfall.updated_at` only when a
  value changes, and `storm-analytics` polls `rainfall` by `updated_at`
  (migration 0005 index); telemetry is polled by `inserted_at` as in ADR 0003.
- **Recompute, don't accumulate.** Every stage of `storm-analytics`
  re-segments the rain around what changed from the database (the window is
  widened a day at a time until no wet interval is within 7 h of either end),
  reconciles `storm_events` — an existing row belongs to the computed storm
  whose rain span contains its onset, so ids survive moving onsets; a storm
  that absorbs another keeps the earlier row; a vanished storm is deleted
  with its metrics — and recomputes home metrics for every home of a changed
  storm, otherwise only for homes whose own data changed. Rows are written
  only when a value differs, so a replay writes nothing and sends no
  notification. A change notifies `sumpnet_ingest` with table `storm_events`.
- **Gauges first.** An ECCC hour is used only where no own-gauge interval
  (dry rows included) overlaps it. A storm's `rain_source` is `gauge` if a
  gauge saw rain during it.
- **Rate by water balance.** §10's "cycle rate" is the pit inflow expressed
  in cycles/day of the home's median cycle drop, estimated from level rise
  plus pumped volume (least-squares slope over 20 min on the rising limb,
  2 h on the falling limb); baseflow is the median of per-interval dry-weather
  rates. Units are millimetres of pit depth, so homes without a pit area
  still get lag and recession.

## Consequences
- Replays, redeliveries, late uplinks and ECCC revisions converge to the same
  tables; there is no consumer state to lose on restart.
- Cost is proportional to the data in the recompute window: fine for the
  pilot (a 16-home, 6-day storm replays and settles in ~10 s in the e2e), to
  be revisited with materialised inputs at the Phase 8 load test.
- A rainfall change that only deletes rows (no value rewritten) is carried
  by the notification, not by `updated_at`; in practice deletions accompany
  a rewritten neighbour.
- The acceptance tolerance for lag cannot be ±10 % for fast homes: the
  15-minute heartbeat and the gauges' resolution put ~5 minutes of noise on
  the crossing and the onset (evidence in `progress.md`). The e2e asserts
  ±10 % or 15 min, whichever is larger, plus a 5-minute median; recession
  holds ±10 % for every home. The owner decides whether that stands (§14).
- `volume_l` is the §9 estimate (pit area × level drop per cycle); it leaves
  out inflow during a pump run and so understates storm inflow for homes
  whose inflow approaches pump capacity (§14).

## Alternatives considered
- **Incremental state machine per storm/home** — cheaper per event, but late
  data, restarts and ECCC revisions each need their own repair path.
- **SQL materialised views** (hinted in ADR 0004) — would move §10 logic out
  of the tested Go package.
- **Averaging ECCC with the gauges** — LONDON CS is 23 km away and hourly;
  mixing it in would blur onset and peak for the neighbourhood.
- **Onset from interpolated tip times or a fitted hyetograph** — better
  numbers on the simulator's storm shapes, but a model of the storm, not a
  measurement.
