# Changelog

Versions follow the phases in `prompt_plan.md` §12 (0.N.0 = Phase N accepted).
The dashboard shows the same history under About.

## [0.5.0] - 2026-09-12
### Added
- `api-gateway`: `query.v1.QueryService` over gRPC (`:9092`) and REST via grpc-gateway, with optional TLS on the published REST port, CORS allowlist and a read-only database pool.
- Clerk owner authentication (RS256 JWT against the issuer JWKS; issuer, expiry, not-before and authorized-party checks) mapped to `home_owners`; another owner's home is `NOT_FOUND`.
- Owner RPCs `ListMyHomes`, `ListMyAlerts`, `AcknowledgeMyAlert` (proxied to `AlertService`) and public `ListSegments`; `HomeStormMetrics` gains `lag_reached`, `recession_reached`, `storm_started_at`.
- `WatchNeighbourhood` hub: live segment status through `internal/privacy`, storm event updates, alerts only to the owning subscriber.
- `cmd/seed` / `make seed`: illustrative Timberwalk segment polygons, the simulator's homes and devices, optional demo owner link.
- `web/`: Vite + React + MapLibre dashboard with the live street heatmap, privacy-hidden streets, storm replay gauge, owner home and alert views, About dialog.
- ADR 0006 (gateway authentication and stream privacy); Phase 5 acceptance test.

## [0.3.0] - 2026-09-11
### Added
- `cycle-detector` (estimated volume, dry run, short cycling, continuous run) and `alerts` (raise, acknowledge, resolve, SMTP notification, `AlertService`).

## [0.2.0] - 2026-09-11
### Added
- `lora-bridge`, `mqtt-bridge` and `ingest`: idempotent, lossless ingest into partitioned Postgres with LISTEN/NOTIFY hints.

## [0.1.0] - 2026-09-11
### Added
- Protobuf contracts, `internal/codec` golden vectors and the deterministic neighbourhood simulator.

## [0.0.1] - 2026-09-11
### Added
- Scaffold: Go module, Makefile, Docker Compose stack, CI, platform package.
