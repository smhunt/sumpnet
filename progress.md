# progress

## Status
Phase: **0 — Scaffold** — DONE 2026-09-11 (all acceptance criteria met, CI run 34634272184 green)

Phase: **3 — Cycle detection + alerts** — DONE 2026-09-11 (acceptance below; branch `phase-3/alerts`)

Phases **4 — Weather + storm analytics** and **5 — API gateway + dashboard** run in parallel (owner's call, 2026-09-12) on branches `phase-4/weather` and `phase-5/gateway`, both forked from `contracts/phase-4-5`.

Phase: **4 — Weather + storm analytics** — work items done 2026-09-12 on `phase-4/weather`; acceptance: recession ±10 % met for every home, lag ±10 % not reachable for fast homes (data resolution) — the e2e accepts ±10 % or 15 min, pending the owner's decision (prompt_plan §14).

## Session log (newest first)

### 2026-09-12 (Phase 4, branch `phase-4/weather`)
- Merged `contracts/phase-4-5` fix 224bad8 (empty segment kind) before the first DB commit.
- **Codec** fPort 5 rain gauge (11 B) with three golden vectors; fuzz/round-trip.
- **Simulator** rain gauges: 2 nodes (kind `rain`, DevEUI `70b3d57ed1…`), 0.2 mm tips from the true
  minute series, 5-min wake / 15-min dry cadence, counter_reset on first uplink, own PCG streams.
  `sim.Config.RainGauges` defaults to 0 — with 0 gauges the Phase 1 hash is still
  `4d88f34a…4a24` (storm50 × 60) and failing-pump seed 5 × 12 is still `bb4578a2…7d7e`; the CLI and
  `testpipeline.NewSim` use 2 (storm50 seed 42 × 60 → `e530369a…21df`, 14,076 events). New scenarios
  `storm25-long` / `storm50-long` (3-day dry lead, 3-day tail).
- **Truth bug fixed:** the last minute of a run has no rate sample (read 0), so `recession_min` was
  "end of scenario − rain end" (961 / 721) for every home that had not receded; now −1.
- **Finding:** `lag_min_discrete` saturates — one cycle in its 30-min window is 48 cycles/day, above
  2× baseflow for any baseflow < 24 — so it measures time-to-next-cycle (0 min for several homes).
  Acceptance uses `lag_min` / `recession_min`.
- **Ingest**: `SubmitRainGaugeReadings`, migration 0005 (`rain_gauge_uplinks` partitioned monthly,
  `rainfall(updated_at)` index), bulk path, bridges; auto-registered senders get the table's device kind.
- **weather**: gauge consumer + ECCC GeoMet poller. Verified: LONDON CS 6144478 has populated hourly
  precipitation (LONDON A 6144473 is null throughout; nothing closer reports hourly); `PRECIP_AMOUNT` at
  `UTC_DATE` is the hour ending then (hourly sums over (06Z, 06Z] match climate-daily totals on every
  boundary-rain day; hour-beginning does not). Recorded GeoMet pages for tests.
- **hydrology**: storms (station intervals for onset/end, 5-min bins for totals), baseflow (median of
  per-interval dry-weather rates), water-balance inflow rate, lag, recession, AnalyseHomeStorm,
  SegmentLoad. **storm-analytics** (ADR 0007): recompute-from-source consumer, NOTIFY `storm_events`.
  **alerts**: OUTAGE_RISK rain term (missing rainfall data count as rain).
- **Estimator evidence** (in memory, sim events → hydrology, 60 homes, 1-min grid, 20-min / 2-h windows):
  storm25-long seed 42: lag within ±10 % 38/60, recession 59/60; storm50-long seed 42: 38/60, 60/60;
  seed 7: 40/60, 60/60 and 43/60, 58/60 (recession misses at 10.4–11.3 %). |lag error| median 3–4 min,
  max ~13 min (one 21.7); the crossing error with the *true* onset is just as large, i.e. the
  15-min heartbeat localisation dominates, with gauge onset (0.2 mm tips) adding up to +5 min on
  slow-ramping storms. Hypothetical 5-min heartbeats + true onset: 44–47/60. Tried and rejected:
  isotonic smoothing (no gain), interpolated cumulative inflow (4–13 of 24 homes), interpolating crossings
  across rate holes (e2e 7/16, median 7.1 min).
- **Finding:** `volume_l` = Σ §9 est volumes understates storm inflow up to ~2.6× where inflow nears
  pump capacity (inflow during runs is not a level drop); cycle counts match truth exactly (§14).

#### Phase 4 acceptance (`TestPhase4StormAcceptance`, storm50-long seed 42 × 16 homes, ECCC off, ~11 s)
- Storm: onset +4m44s, rain end −5m30s, total 50.00 mm (truth 50.11), peak 15.2 mm/h (15.8), source
  gauge, closed. Baseflow within 3 % for every home (≤ 1.1 %).
- Per home (truth → estimate): lag 94→94.0, 91→84.0, 52→55.0, 23→24.0, 73→78.0, 90→83.0, 42→49.0,
  30→21.0, 17→12.0, 16→8.0, 104→103.0, 55→44.0, 66→62.0, 87→89.0, 74→70.6, 96→89.0 (11/16 within
  ±10 %, all within 15 min, median |error| 5.0 min); recession errors +0.8, −0.0, +0.3, +7.1, +4.7,
  +2.8, +2.2, −0.8, −1.8, +6.7, −1.4, +4.2, +3.7, +3.0, −2.4, −0.4 % (16/16 within ±10 %).
- `make lint`, `make test`, `-tags integration ./...` green (one testcontainers reaper start-up flake
  in `TestPipelineStorm` under the parallel run, green on rerun). Live stack not exercised (`make up`
  shares the running stack's ports; not run in this session).

### 2026-09-12 (Resend, PR #4 merge, Phase 4/5 contracts)
- Resend plugin (skills) installed. Alert email goes through Resend SMTP
  (`smtp.resend.com:465`, user `resend`, API key as password, verified-domain
  sender) — configuration only in `.env`; `alerts testmail` /
  `make alerts-testmail` sends one delivery check. No Resend idempotency
  header (a retry's fresh Date header would be a conflicting payload).
- PR #4 (Phase 3) merged.
- §14 decisions with owner: rain gauge = fPort 5, 11 B, cumulative tip
  counter; owner auth = Clerk.
- Shared contract for the parallel phases: migration 0004 (`segments.kind`,
  `rainfall`, `storm_events`, `home_storm_metrics`, `home_owners`),
  `internal/privacy` (k ≥ 3) + ADR 0005, §5 fPort 5 spec. Ownership: Phase 4
  owns codec/sim/ingest/weather/storm-analytics and may add migration 0005;
  Phase 5 owns api-gateway/web/query protos and may add migration 0006.

### 2026-09-11 (Phase 3, branch `phase-3/alerts`)
- Decisions with owner: real SMTP via a transactional provider (creds in
  `.env` only; empty `SMTP_HOST` = log only); every WARNING+ alert to one
  operator address `ALERTS_TO`; per-owner routing deferred (§14).
- Migration 0003: `alerts` (one open row per device+code, event-time
  timestamps, notify state machines), `detections`, `consumer_watermarks`,
  `inserted_at` indexes. sqlc queries for polling, alerts, homes.
- `internal/watermark`: ADR 0003 consumer (LISTEN hint, lag-bounded
  group-complete polls, transactional watermark, Reset on rollback).
- `internal/hydrology`: §10 verbatim, pure, 100% covered.
- `internal/detector`: est_volume (NULL for unlinked devices; backfill query),
  dry-run / continuous-run / short-cycling detections; **finding:** a hard
  short-cycler trips the node's storm mode so its cycles arrive only as
  fPort 4 summaries — the detector now applies the rule to summaries too
  (mean interval ≤ 60 s ⇔ count ≥ 15 per 900 s).
- `internal/alerts`: engine + rules (node alarms, heartbeats incl. the
  Phase 3 outage-risk form, detections, OFFLINE sweep), stdlib SMTP notifier
  (STARTTLS/implicit TLS/PLAIN, deterministic Message-ID, resolve mails for
  CRITICAL), delivery loop with retries, AlertService gRPC (+ reflection).
- **Finding:** sources are consumed at different watermark positions, so the
  same episode can be seen out of event-time order across tables; `raised_at`
  is now the earliest trigger and a stale row cannot resolve a newer episode
  (ADR 0003 addendum).
- Compose: cycle-detector/alerts env, alerts gRPC on host **3135** (registered).

#### Phase 3 acceptance
- [x] `internal/e2e` (testcontainers, all services in-process): failing-pump
  (12 homes, 4 d) → DRY_RUN home 9 (still open), SHORT_CYCLING home 11,
  CONTINUOUS_RUN home 7; alert sets equal an oracle computed from the emitted
  events; first raises within 2 simulated minutes of their trigger; volumes
  within 6% of the simulator's truth; gRPC list/ack/NotFound; one notification
  per WARNING+ row. outage (24 homes) → MAINS_LOST for exactly the 6 affected
  homes within 2 min, resolved on the first mains-ok heartbeat; FLOAT_HIGH and
  OUTAGE_RISK as expected. ~25 s.
- [x] Live stack: `make up` (migrate to 0003, cycle-detector + alerts healthy),
  failing-pump replay (7,949 events) → consumers caught up (5,568 readings,
  97 alarms, 5 detections); alerts: FLOAT_HIGH ×2, DRY_RUN, CONTINUOUS_RUN
  (resolved), SHORT_CYCLING (detector, resolved), plus 12 OFFLINE from the
  wall-clock sweep — expected when replaying April data live. Volumes are NULL
  and `ListActiveAlerts{segment}` is empty because live devices are unlinked
  (documented). SMTP unset locally → log-only notifier, 17 raise + 1 resolve
  notifications recorded in metrics.

### 2026-09-11 (Phase 2, branch `phase-2/ingest`)
- Decisions with owner: nodes are ESP32-S3 + SX1262 (LoRaWAN deployed path,
  Wi-Fi → MQTT bench path); there are **no Particle Photons** — the Photon
  line in §3 was obsolete. `mqtt-bridge` is now the node-MQTT bridge for the
  `sumpnet/v1/{dev_eui}/up` envelope (`docs/node-mqtt.md`).
- Schema (`migrations/`): segments, homes, devices (dev_eui natural key),
  readings + cycle_events partitioned monthly by pg_partman (2026-01 → now+2,
  infinite), storm_summaries, alarm_events; PK `(device_id, event time, f_cnt)`.
  One-shot `migrate` compose job; Postgres runs in UTC.
- `internal/store`: pgx pool with per-connection temp staging tables; batch =
  COPY → devices upsert → `INSERT … ON CONFLICT DO NOTHING` → `pg_notify`;
  on-demand partition creation; sqlc queries (`make sqlc`, CI `sqlc diff`).
- `internal/ingest`: client-streaming handlers with a bounded batch queue;
  a full queue stops Recv (HTTP/2 flow control = backpressure); validation
  with per-reason reject counters.
- `internal/bridge` (shared): manual-ack MQTT consumer, bounded batchers that
  ack only after ingest confirms, stream-per-batch client with retry;
  `lorabridge` (ChirpStack events) and `nodebridge` (Wi-Fi envelope) decoders.
- Broker tuning for QoS 1 bursts: mosquitto `max_inflight_messages 1000`,
  `max_queued_messages 100000`; ChirpStack `[integration.mqtt] qos=1`.
- Compose: the simulator (a CLI since Phase 1) moved behind `COMPOSE_PROFILES=sim`;
  it had been restart-looping as a "service".
- ADR 0003 (LISTEN/NOTIFY) and ADR 0004 (native partitioning + pg_partman).

#### Phase 2 acceptance
- [x] `TestPipelineStorm` (testcontainers Postgres + Mosquitto, in-process
  ingest + both bridges): storm25 × 10 homes = 1,996 events → 960 readings,
  1,014 cycle events, 22 storm summaries (175 cycles), 0 alarms; per-device
  rows == max f_cnt + 1 (no gaps); 1,014 + 175 == 1,189 true cycles; replaying
  the same stream changes nothing and reports 1,996 duplicates; the Wi-Fi
  transport leg doubles every count with the same bytes. ~6 s.
- [x] Live stack: `make up`, `simulator -scenario storm25 -homes 10 -sink mqtt`
  → identical DB counts, `sumpnet_bridge_drops_total` 0.

### 2026-09-11 (Phase 1, branch `phase-1/contracts`, PR #2)
- Protos: `telemetry/v1` (IngestService: SubmitReadings, SubmitCycleEvents
  incl. storm summaries, SubmitAlarms), `query/v1` (QueryService with
  grpc-gateway GETs, `WatchNeighbourhoodResponse` wrapper), `alerts/v1`.
  Generated code committed under `gen/go` with a CI drift check (ADR 0002).
- `internal/codec`: strict little-endian encode/decode for fPorts 1–4 with
  hand-derived golden vectors, round-trip property test and fuzzing; fPort 4
  layout fixed at 9 bytes (count, window_s, total_run_s, max_peak_current_da,
  min_level_mm). All ports ≤ 11 B (US915 DR0 limit).
- `internal/chirpstack`: topic helpers and `UplinkEvent` JSON via the vendor's
  own proto types + protojson (compacted for byte-stable streams).
- `internal/sim`: deterministic engine (PCG per home, integer hydrology,
  virtual clock), emulated node firmware (heartbeats, cycles, alarms with
  hold-off, storm mode), six scenarios, ground-truth export with §10
  lag/recession applied literally, hash/JSONL/MQTT sinks.
- `cmd/simulator` CLI; `make sim`, `make test-integration`; README.
- Model calibration notes: response 60–160 L/mm and reservoir 30–300 min
  were needed for storm mode to be exercised by a representative share of
  homes (9/24 in tests); short-cycling is modelled as a float fault (20 mm
  hysteresis) plus 85% check-valve backflow; a failing pump decays to zero
  output by 90% of the scenario so continuous-run/float-high are reachable.
- Test-time note: `internal/sim` takes ~40 s under `-race` (was 151 s before
  removing a per-step map write and memoising runs); 0.7 s without.
- Gotcha: testcontainers hangs from Claude's sandboxed shell because the
  `osxkeychain` credential helper never returns; `DOCKER_CONFIG` pointing at
  an empty config.json works. Documented in CLAUDE.md.

#### Phase 1 acceptance
- [x] Two unpaced runs `-scenario storm50 -seed 42 -homes 60 -sink stdout`:
  byte-identical JSONL (13,795 events, 15 MB) and identical truth JSON;
  stream hash `4d88f34ae1956fcccc2bcdfcf187241a4997f015319de48a4aaba3b57c0b4a24`.
- [x] `make sim SCENARIO=storm50 SEED=42 SPEED=60` into the compose Mosquitto:
  13,795 events, completed in 23 min 59 s real (24 h virtual), hash
  `4d88f34ae1956fcccc2bcdfcf187241a4997f015319de48a4aaba3b57c0b4a24` — identical
  to the stdout runs, so the hash is sink- and pacing-independent.
- [x] Integration test: 979 events from 5 devices received in order.

### 2026-09-11 (Phase 0)
- Decided with owner: dev on the Mac (lab VM later), own Mosquitto in compose,
  private GitHub repo `smhunt/sumpnet`, module path `github.com/smhunt/sumpnet`.
- Registered host ports in `~/.claude/PORTS.md`: ChirpStack 3131, MQTT 3133,
  api-gateway 3134, web 3034, Postgres 5444.
- Done: go.mod, Makefile + `.versions.env` tool pins, `internal/platform`
  (config, slog JSON, /healthz /readyz /metrics, graceful shutdown,
  `healthcheck` subcommand), ten placeholder services, golangci v2 config,
  buf config + placeholder proto, Dockerfile (distroless, `ARG SERVICE`),
  compose stack (Postgres 16 + pg_partman, Redis, Mosquitto, ChirpStack v4
  US915), CI workflow, ADR 0001.
- Fixed during `make up`: initdb script needed `#!/bin/sh` + exec bit (Alpine image,
  no bash) and the pg_partman BGW needs `pg_partman_bgw.role=sumpnet` since the
  superuser is not `postgres`. Image is `postgres 16.14` + `pg_partman 5.2.4`.
- Deferred: HTTPS on `dev.ecoworks.ca:<port>` for the ChirpStack UI and ops
  endpoints — Phase 0 acceptance is plain TCP; wire Traefik/TLS in Phase 5.
- Spec amendments agreed for Phase 1 (see plan): `WatchNeighbourhoodResponse`
  wrapper, `SubmitAlarms` RPC, ADR renumbering (0002 generated code,
  0003 eventing), explicit `go_package` (no buf managed mode), rain-gauge fPort
  is a new §14 open question.

## Phase 0 acceptance checklist
- [x] `make lint && make test` clean
- [x] `make up` exits 0; `make ps` shows postgres, redis, mosquitto, chirpstack and all 10 Go services healthy
- [x] `curl -s localhost:3134/readyz` → 200; `/metrics` contains `sumpnet_build_info`
- [x] ChirpStack UI on http://localhost:3131 (admin/admin); `pg_partman` in sumpnet DB; `pg_trgm`/`hstore` in chirpstack DB
- [x] `make down && make up` idempotent
- [x] First push to `smhunt/sumpnet` green on all CI jobs (15/15, https://github.com/smhunt/sumpnet/actions/runs/34634272184)
