# sumpnet architecture

sumpnet turns sump-pit sensors in volunteer homes into street-level storm
response and owner alerts. This page is the map of the running system;
`prompt_plan.md` is the specification and `docs/adr/` records the decisions.

## System overview

```
House nodes (LoRa) ─> gateways ─> ChirpStack ─MQTT─> lora-bridge ─┐
ESP32 Wi-Fi nodes ──────────────> Mosquitto ─────> mqtt-bridge ───┼─gRPC─> ingest ─> Postgres 16
                                                                  │                    │ LISTEN/NOTIFY
                        cycle-detector ── detections ──> alerts <─┘                    │ (hints, ADR 0003)
                        weather, storm-analytics (Phase 4) ─> storm_events, home_storm_metrics
                                                                                       │ read-only
  browser ─ https://dev.ecoworks.ca:3034 (web/, Vite) ─/v1 proxy─┐                     v
                                                                  └─ REST :3134 ─> api-gateway ─gRPC :9092─ (MCP server, Phase 6)
                                                                                     │  └─ AlertService.Acknowledge ─> alerts
                                                                               Clerk JWKS (owner tokens)
```

## Tech stack

| Layer | Choice |
|---|---|
| Services | Go 1.26, one module, `internal/platform` (slog JSON, `/healthz` `/readyz` `/metrics`, graceful shutdown) |
| Contracts | Protobuf + buf; gRPC; grpc-gateway REST (generated code committed, ADR 0002) |
| Storage | Postgres 16, native partitioning + pg_partman (ADR 0004), sqlc + pgx/v5, golang-migrate |
| Eventing | Postgres LISTEN/NOTIFY hints + watermark polls (ADR 0003) |
| Auth | Clerk session JWTs verified in the gateway: golang-jwt/jwt/v5 + MicahParks/keyfunc/v3 (ADR 0006) |
| Privacy | `internal/privacy`, k ≥ 3 homes per public segment aggregate (ADR 0005) |
| Dashboard | Vite 8, React 19, TypeScript 6, MapLibre GL 6, `@clerk/react` 6, Vitest |
| Local stack | Docker Compose (`deploy/compose`); Vite dev server on the host |

## File structure (Phase 5 parts)

```
cmd/api-gateway/            main: gateway.ConfigFromEnv + gateway.Run
cmd/seed/                   demo neighbourhood seed (make seed)
internal/auth/              Clerk token verifier, gRPC interceptors
internal/auth/authtest/     in-test JWKS issuer (tests only)
internal/gateway/           QueryService, WatchNeighbourhood hub, REST/CORS/TLS, read-only pool
internal/seed/              segments.geojson (illustrative) + seeding logic
internal/store/queries/     query.sql (gateway reads), seed.sql
internal/e2e/phase5_*       Phase 5 acceptance test
web/src/lib/                pure logic: API client, NDJSON stream decoder, privacy display state, heat scale
web/src/components/         MapView, StormGauge, Legend, SegmentPanel, OwnerPanel, AboutModal
```

## API

REST is JSON (protojson field names, enums as strings, unset messages as
`null`). Send `Authorization: Bearer <Clerk session token>` for owner calls.

| Method | Path | RPC | Access |
|---|---|---|---|
| GET | `/v1/segments` | ListSegments | public |
| GET | `/v1/storm-events?since=&until=&page_size=&page_token=` | ListStormEvents | public, newest first |
| GET | `/v1/storm-events/{storm_id}` | GetStormEvent | public, segment aggregates only |
| GET | `/v1/neighbourhood:watch?send_snapshot=true&segment_ids=` | WatchNeighbourhood | public; alerts only to their owner |
| GET | `/v1/homes/{home_id}` | GetHome | owner of that home |
| GET | `/v1/me/homes` | ListMyHomes | owner |
| GET | `/v1/me/alerts?home_id=` | ListMyAlerts | owner |
| POST | `/v1/me/alerts/{alert_id}:acknowledge` | AcknowledgeMyAlert | owner of the alert's home |

Status codes: 401 for a missing (owner RPCs) or invalid token (all RPCs),
404 for anything the caller does not own, 400 for malformed ids or page tokens.

`/v1/neighbourhood:watch` is newline-delimited JSON, one
`{"result":{"update":{…}}}` per line; a line `{"error":{…}}` ends the stream.
Each update carries `ts` and one of `segmentStatus`, `stormEvent` or `alert`.
With `send_snapshot=true` the stream starts with every segment's status, open
and recent storms and (for an owner) their active alerts. `segmentStatus.ts`
is the end of the status window: the newest heartbeat event time.

The same RPCs are on gRPC `api-gateway:9092` (reflection enabled) with the
token in `authorization` metadata.

## Privacy in the gateway

- Per-home rows are only read by owner-scoped queries keyed on the caller's
  Clerk subject, or as input to `privacy.AggregateStorm` / `AggregateStatus`.
- A segment with fewer than 3 reporting linked homes is published with
  `suppressed: true` and zeros; the dashboard hides it again and draws it
  hatched with an explanation. Unlinked devices never count.
- `WatchNeighbourhood` routes alerts by `home_owners` at send time; anonymous
  streams carry segment statuses and storm events only. The acceptance test
  asserts no home id or DevEUI ever appears on an anonymous stream.

## Running locally

```bash
make up                                   # stack incl. api-gateway on https://dev.ecoworks.ca:3134
make seed DEMO_OWNER_SUBJECT=user_...     # demo streets, homes, devices, optional owner link
make sim SCENARIO=storm50 SEED=42 SPEED=60
cp web/.env.example web/.env.local        # add VITE_CLERK_PUBLISHABLE_KEY for sign-in
make web-install && make web-dev          # https://dev.ecoworks.ca:3034
```

Owner-supplied configuration: `CLERK_ISSUER` and `CLERK_AUTHORIZED_PARTIES`
in `deploy/compose/.env`, `VITE_CLERK_PUBLISHABLE_KEY` in `web/.env.local`,
and `https://dev.ecoworks.ca:3034` allowed as an origin in the Clerk
dashboard.

## Known gaps

- `storm_events` and `home_storm_metrics` are filled by Phase 4; until then the
  storm replay shows its empty state and `baseflow_cycles_per_day` is 0.
- Street outlines are illustrative, not surveyed.
- OpenStreetMap's public tiles suit light development use; a pilot needs its
  own tile provider (`VITE_TILE_URL`).
