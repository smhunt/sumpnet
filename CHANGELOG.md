# Changelog

All notable changes to sumpnet. The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/).
Versions follow the phases in `prompt_plan.md` §12: 0.N.0 is Phase N accepted, except 0.5.0, which
marks Phase 5 built with one acceptance check still open. The dashboard shows the same history under
About (`web/src/about.ts`).

## [Unreleased]
### Fixed
- api-gateway: after a Clerk signing-key rotation, tokens signed with the new key were rejected until the hourly JWKS refresh. The refresh on an unknown key ID now has a 10 s budget instead of 1 ms (PR #8).

### Changed
- Documentation: root README, architecture maps in `docs/README.md` (system, data flow, data model, repository layout, API), CLAUDE.md, progress, this changelog and the in-app About data brought in line with the code.

## [0.5.0] - 2026-09-12
Phase 5 built. Open acceptance check: a live storm replay visible on the map against the running stack.

### Added
- `api-gateway`: `query.v1.QueryService` over gRPC (`:9092`) and REST via grpc-gateway, with TLS on the published REST port (`https://dev.ecoworks.ca:3134`), a CORS allowlist and a read-only database pool.
- Clerk owner authentication (RS256 JWT against the issuer JWKS; issuer, expiry, not-before and authorized-party checks) mapped to `home_owners`; another owner's home is `NOT_FOUND`.
- Owner RPCs `ListMyHomes`, `ListMyAlerts`, `AcknowledgeMyAlert` (proxied to `AlertService`) and public `ListSegments`; `HomeStormMetrics` gains `lag_reached`, `recession_reached`, `storm_started_at`.
- `WatchNeighbourhood` hub: live segment status through `internal/privacy`, storm event updates, alerts only to the owning subscriber.
- `cmd/seed` / `make seed`: illustrative Timberwalk segment polygons, the simulator's homes and devices, optional demo owner link.
- `web/`: Vite + React + MapLibre dashboard with the live street heatmap, privacy-hidden streets, storm replay gauge, owner home and alert views, About dialog; `make web-install`, `web-dev`, `web-test`, `web-build`; CI `web` job.
- ADR 0006 (gateway authentication and stream privacy); Phase 5 acceptance test.

## [0.4.0] - 2026-09-12
Phase 4 accepted with the owner-approved lag tolerance.

### Added
- Shared Phase 4/5 contract: migration 0004 (`segments.kind`, `rainfall`, `storm_events`, `home_storm_metrics`, `home_owners`), `internal/privacy` (k ≥ 3) and ADR 0005.
- fPort 5 rain gauge payload (11 B, cumulative tip counter) in `internal/codec`, `IngestService.SubmitRainGaugeReadings`, and migration 0005 (`rain_gauge_uplinks` with monthly partitions, `rainfall(updated_at)` index).
- Simulator: two rain gauge nodes (`-rain-gauges`, default 2 in the CLI) and the `storm25-long` and `storm50-long` scenarios.
- `weather`: rainfall from consecutive tip-count deltas, and an ECCC MSC GeoMet hourly poller (LONDON CS, 6144478) as fallback and cross-check.
- `storm-analytics`: storm events and per-home lag, recession, volume, cycles and baseflow, recomputed from source whenever rain or telemetry change (ADR 0007).
- `internal/hydrology`: §10 storms, baseflow, water-balance inflow rate, lag, recession and segment load.
- Phase 4 acceptance test (`storm50-long` × 16 homes): recession within ±10 % for every home, lag within ±10 % or 15 min.

### Changed
- alerts: `OUTAGE_RISK` also requires rain in the 6 h before the reading; rainfall data that do not reach within 30 min of it count as rain.

### Fixed
- Simulator truth: `recession_min` is −1 for a home that had not receded by the end of the scenario (it was "end of scenario − rain end").

## [0.3.0] - 2026-09-11
### Added
- `cycle-detector`: estimated volume per cycle, dry run, short cycling (also from storm-mode summaries) and continuous run, handed to alerts through `detections`.
- `alerts`: raise, acknowledge and resolve with event-time timestamps, SMTP notification to an operator address, and `AlertService` gRPC on host port 3135.
- `internal/watermark` (the ADR 0003 consumer) and migration 0003 (`alerts`, `detections`, `consumer_watermarks`); Phase 3 acceptance test.
- Resend as the default SMTP provider and `make alerts-testmail` (added before the merge on 2026-09-12).

## [0.2.0] - 2026-09-11
### Added
- `lora-bridge`, `mqtt-bridge` (Wi-Fi envelope, `docs/node-mqtt.md`) and `ingest`: idempotent, lossless ingest into Postgres with LISTEN/NOTIFY hints.
- Migrations 0001–0002 with pg_partman monthly partitions and a one-shot compose `migrate` job; ADR 0003 (eventing) and ADR 0004 (partitioning).

## [0.1.0] - 2026-09-11
### Added
- Protobuf contracts (telemetry, query and alerts v1) with committed generated code (ADR 0002).
- `internal/codec` golden vectors for fPorts 1–4 and the deterministic neighbourhood simulator with six scenarios.

## [0.0.1] - 2026-09-11
### Added
- Scaffold: Go module, Makefile with pinned tools, `internal/platform`, ten placeholder services, Docker Compose stack, CI, ADR 0001.
