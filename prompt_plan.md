# sumpnet — Neighbourhood Sump & Drainage Platform

Working name: **sumpnet**. Pilot neighbourhood: Timberwalk, Ilderton (Middlesex Centre, ON).

## 1. Purpose

Two goals, in priority order:

1. **Portfolio showcase** for senior Go backend roles: idiomatic Go, gRPC, microservices, AWS, Docker, CI/CD — with real data, real hardware, and published load-test numbers.
2. **Real civic tool**: understand how a modern subdivision's drainage behaves during storms, house by house, and give homeowners early warning of pump failure.

Questions the platform must answer:

- How quickly does each basement respond to rainfall (lag), and how long does it take to return to normal (recession)?
- Which streets/lots carry the highest storm load? Does it correlate with wooded lots, grading, or distance to the stormwater pond?
- Which pumps are failing (dry-running, short-cycling, drawing abnormal current)?
- Is anyone at risk right now (power outage + rising pit level during rain)?

## 2. Constraints & principles

- **Local-first**: everything runs in Docker Compose on the Ubuntu lab VM. AWS is a second deployment target, not a dependency.
- **Explicit over magic**: no heavy frameworks. stdlib + small, well-known libraries.
- **Plan-first, small reviewable commits.** Update `progress.md` at the end of every session.
- **Privacy by design**: opt-in only; pseudonymous device IDs; per-house data visible only to the owner; public views aggregate to street segments with a minimum of 3 homes.
- **No sensors on municipal assets** (stormwater pond, outlet, pump station) without Middlesex Centre approval.
- **Electrical safety**: non-invasive current sensing only (clamp on a single conductor via a plug-through adapter). No modification of pump wiring. Anything permanent goes through a licensed electrician (ESA).
- **Canadian suppliers** for all hardware (DigiKey Canada, BC Robotics — confirm stock before ordering).

## 3. Architecture

```
House nodes (ESP32-S3 + SX1262, fPorts 1-4) ─┐
Rain gauge nodes x2 (fPort 5) ───────────────┴─> LoRaWAN gateways ─> ChirpStack v4 ─┐
ESP32 Wi-Fi nodes (bench): sumpnet/v1/{dev_eui}/up ─────────────────────────────────┤ MQTT
simulator CLI (ChirpStack-shaped events, replays) ──────────────────────────────────┤
                                                                                    v
lora-bridge (ChirpStack events) + mqtt-bridge (Wi-Fi envelope) <─────────────── Mosquitto
  │ gRPC IngestService (client streams)
  v
ingest ─ COPY, ON CONFLICT DO NOTHING, NOTIFY sumpnet_ingest ─> Postgres 16 + pg_partman
                                                                  │
  LISTEN hint + watermark poll (ADR 0003) <───────────────────────┤
  ├─ cycle-detector    -> est_volume_l, detections
  ├─ alerts            -> alerts; email via Resend SMTP; AlertService gRPC (host 3135)
  ├─ weather           -> rainfall from gauges + ECCC MSC GeoMet hourly poller
  └─ storm-analytics   -> storm_events, home_storm_metrics
                                                                  │ read-only pool + LISTEN
api-gateway <─────────────────────────────────────────────────────┘
  gRPC :9092, REST + NDJSON stream https :3134, Clerk JWKS; Acknowledge -> alerts
  ^ web dashboard (Vite + React + MapLibre, https :3034)
  ^ mcp-server (placeholder until Phase 6; will read QueryService, never SQL)
```

Canada uses the US915 LoRaWAN band plan (902–928 MHz). No duty-cycle limit, but 400 ms dwell time — keep SF7–SF10 at 125 kHz.

The same system as Mermaid diagrams (architecture, one uplink's data flow, data model), plus the services and ports tables: `docs/README.md`.

## 4. Hardware (per pilot)

| Item | Qty | Notes |
|---|---|---|
| Node MCU + radio: ESP32-S3 + SX1262 board (Heltec WiFi LoRa 32 V3 or LILYGO T3-S3) | 1 per house | One platform for both paths: LoRaWAN via RadioLib through ChirpStack for deployed nodes; Wi-Fi → MQTT (`sumpnet/v1/{dev_eui}/up`, §5) for dev and bench nodes. No Particle Photons (owner decision). RAK WisBlock RAK4631 (nRF52840) was considered and not chosen |
| JSN-SR04T waterproof ultrasonic | 1 per house | Pit level. Ultrasonic, not optical: time-of-flight sensors are unreliable on water surfaces (`docs/research/pump-flow-bucket-test.md`) |
| SCT-013 CT clamp + plug-through line splitter | 1–2 per house | Primary + backup pump current |
| Float switch | 1 per house | Independent high-water alarm |
| BME280 | 1 per house | Basement temp / RH |
| 18650 cell + mains-present detect | 1 per house | Node must survive outages — that's when it matters most |
| External 915 MHz antenna + short pigtail | 1 per house | Mount high on rim joist or near basement window |
| Outdoor LoRaWAN gateway (e.g., RAK7289 or indoor RAK7268 near a high window) | 2 | Redundancy + basement penetration |
| Tipping-bucket rain gauge on a LoRa node | 2 | Opposite ends of the neighbourhood |

**RF risk:** below-grade concrete attenuates heavily. Phase 7 includes a single-house RF survey (RSSI/SNR by SF) before buying more nodes.

## 5. Uplink payloads (binary, little-endian)

Decoder lives in Go (`internal/codec`), not in ChirpStack JS codecs. Table-driven tests with golden byte vectors.

**fPort 1 — heartbeat (every 15 min, unconfirmed), 10 bytes**
| Field | Type | Unit |
|---|---|---|
| level_mm | uint16 | mm from sensor to water |
| temp_c | int16 | 0.01 °C |
| rh | uint8 | % |
| batt_mv | uint16 | mV |
| cycles_since_last | uint8 | count |
| flags | uint8 | bit0 mains_ok, bit1 float_high, bit2 backup_ran, bit3 sensor_fault |
| reserved | uint8 | |

**fPort 2 — pump cycle event (unconfirmed), 11 bytes**
| Field | Type | Unit |
|---|---|---|
| start_offset_s | uint16 | seconds before transmit |
| run_s | uint16 | seconds |
| peak_current_da | uint16 | 0.1 A |
| level_start_mm | uint16 | |
| level_end_mm | uint16 | |
| pump_id | uint8 | 0 primary, 1 backup |

**fPort 3 — alarm (confirmed, immediate), 3 bytes:** code uint8, value uint16.
Codes: 1 float_high, 2 mains_lost, 3 dry_run, 4 continuous_run, 5 sensor_fault.

**Storm mode:** if more than 6 cycles occur within 15 min, stop sending individual fPort 2 events and roll cycles into a summary (fPort 4: count, total_run_s, max_peak_current_da, min_level_mm) to protect airtime.

**fPort 5 — rain gauge (unconfirmed), 11 bytes** (device kind `rain`; decided 2026-09-12)
| Field | Type | Unit |
|---|---|---|
| tip_count | uint32 | tips since boot (cumulative) |
| mm_per_tip_um | uint16 | µm of rain per tip (200 = 0.2 mm) |
| interval_s | uint16 | seconds since the previous uplink |
| batt_mv | uint16 | mV |
| flags | uint8 | bit0 counter_reset (rebooted since last uplink), bit1 sensor_fault |

Transmit every 5 min while tips are being counted, every 15 min otherwise. The server stores the raw uplink and derives rainfall from consecutive `tip_count` deltas, so a lost uplink loses no rain; after `counter_reset` the delta is the new `tip_count`.

Derivation as implemented in Phase 4 (`internal/weather`, ADR 0007): raw uplinks go to `rain_gauge_uplinks`; each rainfall row covers [previous stored uplink, this uplink) (a lost uplink widens the interval); after `counter_reset` (or a counter that went backwards) the interval is the node's `interval_s`, never starting before the previous stored uplink; a gauge's first uplink without `counter_reset` is only a baseline; tips reported after an interval longer than the 5-min wake fell in its last 5 min (the node would otherwise have transmitted at an earlier wake), so that interval becomes a dry row plus a 5-min wet row; `sensor_fault` uplinks yield no rainfall.

**Wi-Fi transport (bench/dev nodes):** the same payload bytes, published over MQTT to `sumpnet/v1/{dev_eui}/up` (QoS 1) as `{"fcnt":N,"fport":P,"data":"<base64>","t":<unix s, optional>,"rssi":<dBm, optional>}`. `mqtt-bridge` decodes it with the same `internal/codec`. Spec: `docs/node-mqtt.md`.

## 6. Repo layout

```
sumpnet/
├── cmd/                      one main package per binary
│   ├── simulator/            CLI: deterministic neighbourhood replay (compose profile sim)
│   ├── seed/                 CLI: demo segments, homes, devices, owner link (make seed)
│   ├── dataimport/           CLI: County site import, ECCC hourly rain cache (make site-import, eccc-import)
│   ├── lora-bridge/          ChirpStack uplink events -> ingest
│   ├── mqtt-bridge/          Wi-Fi envelope uplinks -> ingest
│   ├── ingest/               IngestService: idempotent bulk writes + NOTIFY
│   ├── cycle-detector/       estimated volume, dry run, short cycling, continuous run
│   ├── alerts/               alert engine, email, AlertService; `alerts testmail`
│   ├── weather/              rainfall from gauges + ECCC GeoMet poller
│   ├── storm-analytics/      storm events + per-home storm metrics
│   ├── api-gateway/          QueryService over gRPC + REST, WatchNeighbourhood
│   └── mcp-server/           placeholder (ops endpoints only) until Phase 6
├── internal/
│   ├── platform/             config, slog, health/metrics endpoints, shutdown, healthcheck
│   ├── codec/                fPort 1-5 binary payloads with golden vectors
│   ├── chirpstack/           uplink topics and UplinkEvent JSON
│   ├── sim/                  simulator engine, scenarios, rain gauges, sinks, truth, sites, observed rain
│   ├── site/                 site configs (sites/*.json), County ArcGIS import, segments and outlines, salted ids
│   ├── raincache/            observed hourly rain cache for -scenario eccc
│   ├── bridge/               shared MQTT consumer, bounded batchers, ingest client
│   ├── lorabridge/           ChirpStack event decoder
│   ├── nodebridge/           Wi-Fi envelope decoder
│   ├── ingest/               IngestService server, bounded batch queue
│   ├── store/                pgx pool, COPY bulk path, NOTIFY; queries/*.sql -> sqlcgen/
│   ├── watermark/            ADR 0003 consumer: LISTEN hint + watermark poll
│   ├── hydrology/            §10 analytics as pure functions
│   ├── detector/             cycle-detector body
│   ├── alerts/               rules, engine, SMTP notifier, AlertService server
│   ├── weather/              gauge consumer, ECCC client; testdata/ recorded pages
│   ├── storms/               storm-analytics body
│   ├── privacy/              k >= 3 segment aggregates (ADR 0005)
│   ├── auth/                 Clerk JWT verifier, gRPC interceptors; authtest/ test JWKS
│   ├── gateway/              QueryService, WatchNeighbourhood hub, REST, CORS, TLS
│   ├── seed/                 segments.geojson (illustrative outlines) + seeding, synthetic or from a site
│   ├── domain/               sentinel errors
│   ├── testinfra/            testcontainers helpers: Postgres, Mosquitto, Mailpit
│   ├── testpipeline/         in-process pipeline helpers for tests
│   └── e2e/                  Phase 3, 4 and 5 acceptance tests (integration tag)
├── proto/sumpnet/            telemetry/v1, query/v1, alerts/v1
├── gen/go/                   generated protobuf, gRPC and grpc-gateway code (ADR 0002)
├── migrations/               0001-0006 SQL for golang-migrate, embedded by embed.go
├── deploy/compose/           docker-compose.yml, .env.example, chirpstack/, mosquitto/,
│                             chirpstack-gateway-bridge/, postgres/initdb/
├── web/                      dashboard: src/components, src/lib, src/auth, src/about.ts
├── docs/                     README.md (this map), node-mqtt.md, adr/, research/
├── loadtest/results/         make sim truth exports (gitignored JSON)
├── data/                     gitignored: County query cache, site snapshots, ECCC rain (never committed)
├── .github/                  workflows/ci.yml, dependabot.yml
├── Dockerfile                one distroless image, --build-arg SERVICE=<name>
├── Makefile, .versions.env   make targets; pinned tool versions shared with CI
├── buf.yaml, buf.gen.yaml, sqlc.yaml, .golangci.yml
└── README.md, CHANGELOG.md, CLAUDE.md, prompt_plan.md, progress.md
```

Planned, not in the tree yet: `firmware/` and `docs/rf-survey.md` (Phase 7), `deploy/terraform/`,
load-test scripts and `loadtest/results/README.md` (Phase 8).

## 7. Go conventions

- Go 1.26 (`go.mod`; `GO_VERSION` in `.versions.env`). `log/slog` JSON logging. `context.Context` everywhere; no goroutine without a cancellation path.
- Errors wrapped with `fmt.Errorf("...: %w", err)`; sentinel errors in `domain`.
- gRPC: `google.golang.org/grpc`, protos managed with `buf` (lint + breaking-change check in CI). REST via `grpc-gateway`.
- MQTT: `github.com/eclipse/paho.golang` (v5). Postgres: `pgx/v5` + `sqlc`. Migrations: `golang-migrate`.
- MCP: official Go SDK `github.com/modelcontextprotocol/go-sdk`.
- Tests: table-driven unit tests; `testcontainers-go` for Postgres/Mosquitto integration tests.
- Every service: `/healthz`, `/readyz`, Prometheus `/metrics`, graceful shutdown on SIGTERM.
- Lint: `golangci-lint` v2 (errcheck, govet, staticcheck, revive, gosec, ineffassign, unused) plus its formatters; `buf lint` and `buf format`.
- Multi-stage Dockerfiles producing distroless images.

## 8. Proto sketch

```proto
// telemetry/v1
service IngestService {
  rpc SubmitReadings(stream SubmitReadingsRequest) returns (SubmitReadingsResponse);
  rpc SubmitCycleEvents(stream SubmitCycleEventsRequest) returns (SubmitCycleEventsResponse); // also carries fPort 4 storm summaries
  rpc SubmitAlarms(stream SubmitAlarmsRequest) returns (SubmitAlarmsResponse);                // fPort 3
  rpc SubmitRainGaugeReadings(stream SubmitRainGaugeReadingsRequest) returns (SubmitRainGaugeReadingsResponse); // fPort 5, stored raw (Phase 4)
}

// query/v1
service QueryService {
  rpc GetHome(GetHomeRequest) returns (GetHomeResponse);                // owner-scoped
  rpc ListStormEvents(ListStormEventsRequest) returns (ListStormEventsResponse);
  rpc GetStormEvent(GetStormEventRequest) returns (GetStormEventResponse); // segment aggregates
  rpc WatchNeighbourhood(WatchNeighbourhoodRequest) returns (stream WatchNeighbourhoodResponse); // wraps NeighbourhoodUpdate (buf RPC_RESPONSE_STANDARD_NAME); alerts only to their owner
  // Phase 5 additions (additive): public segment catalogue; owner-scoped homes/alerts; ack proxied to AlertService
  rpc ListSegments(ListSegmentsRequest) returns (ListSegmentsResponse);
  rpc ListMyHomes(ListMyHomesRequest) returns (ListMyHomesResponse);
  rpc ListMyAlerts(ListMyAlertsRequest) returns (ListMyAlertsResponse);
  rpc AcknowledgeMyAlert(AcknowledgeMyAlertRequest) returns (AcknowledgeMyAlertResponse);
}

// alerts/v1
service AlertService {
  rpc ListActiveAlerts(ListActiveAlertsRequest) returns (ListActiveAlertsResponse);
  rpc Acknowledge(AcknowledgeRequest) returns (AcknowledgeResponse);
}
```

Internal service-to-service events: start with Postgres `LISTEN/NOTIFY` for simplicity; document the trade-off vs NATS/SQS in `docs/adr/0003-eventing.md` (0002 is the generated-code decision, made first in Phase 1).

## 9. Data model (initial)

- `homes` (id uuid, segment_id, pit_area_m2, consent_at, owner_contact_encrypted)
- `devices` (dev_eui, home_id, kind, installed_at)
- `segments` (id, name, kind standard|wooded|near_pond|high_ground, geometry — street segment polygon)
- `readings` (device_id, ts, level_mm, temp_c, rh, batt_mv, flags) — partitioned monthly
- `cycle_events` (device_id, started_at, run_s, peak_current_a, level_start_mm, level_end_mm, pump_id, est_volume_l) — partitioned monthly
- `rain_gauge_uplinks` (device_id, ts, f_cnt, tip_count, mm_per_tip, interval_s, batt_mv, counter_reset, sensor_fault) — raw fPort 5, partitioned monthly (migration 0005)
- `rainfall` (source gauge|eccc, station_id, ts interval start, interval_s, mm) — rain gauges + ECCC; polled by `updated_at`, which writers bump only on a real change
- `storm_events` (id, started_at, ended_at NULL while open, total_rain_mm, peak_intensity_mm_h, rain_source, status open|closed)
- `home_storm_metrics` (storm_id, home_id, lag_min NULL = never reached, recession_min, volume_l, cycles, baseflow_cpd, inflow_est_l, pump_rate_lps, pump_rate_source dry_weather|bucket_test) — the last three added in migration 0006: volume_l is the §9 pit-drop floor, inflow_est_l the calibrated pump rate × run time, all three NULL when no rate is available (§10)
- `home_owners` (auth_subject = Clerk user id, home_id) — owner scoping for the api-gateway
- `alerts` (id, device_id, home_id (snapshot, NULL until linked), code, severity, raised_at, last_seen_at, acked_at, resolved_at, resolve_reason, message, notify state) — one open row per (device, code)
- `detections` (device_id, code, action raise|clear, observed_at, f_cnt) — cycle-detector → alerts hand-off
- `consumer_watermarks` (consumer, source, inserted_at) — ADR 0003 pollers

Estimated volume per cycle = `pit_area_m2 × (level_end_mm − level_start_mm) / 1000 × 1000 L`. An 18" basin is about 0.164 m².

## 10. Analytics definitions (`internal/hydrology`, pure + tested)

- **Cycle**: current above threshold for ≥ 3 s with level drop ≥ 20 mm.
- **Dry run**: current on ≥ 30 s with level drop < 5 mm → failed pump or stuck check valve.
- **Short cycling**: consecutive cycles < 60 s apart (idle gap between runs) for ≥ 5 cycles → check valve or float issue. In storm mode the node only reports fPort 4 summaries, so the equivalent test on a summary is mean interval ≤ 60 s (count ≥ 15 per 900 s window).
- **Continuous run**: current on > 10 min.
- **Baseflow**: median dry-weather cycles/day (no rain for 72 h) → water-table indicator. Phase 4 clarification: the median of per-interval rates 86 400 s ÷ (gap between consecutive primary-pump cycle starts) — equal to one day over the median gap — over intervals with no rain recorded from 72 h before the earlier cycle to the later one, among cycles in the 14 days before the storm's first rain; at least 2 intervals, else no baseflow (and no lag/recession). No recorded rain counts as dry. The median drop of the same cycles is the home's "one cycle" for rates.
- **Storm event**: rainfall ≥ 5 mm total with gaps < 6 h. Phase 4 clarification: stations are merged on 5-min bins (mean over stations covering a bin) for grouping, total and peak; onset is the start of the earliest station interval ≥ 1 mm/h and rain end the end of the latest wet station interval (bins would dilute a single gauge's first tip); a storm is open until rainfall data reach 6 h past its rain end. Own gauges are primary; an ECCC hour is used only where no gauge interval overlaps it.
- **Response lag**: time from rain onset (≥ 1 mm/h) to cycle rate > 2× baseflow. Phase 4 clarification: "cycle rate" is the pit inflow in cycles/day of the home's cycle, by water balance (level rise from heartbeats and cycle starts + pumped drops; storm-mode roll-ups add count × cycle drop), as the least-squares slope over 20 min on a 1-min grid; the rate must stay above the threshold for 30 min and the crossing is interpolated from the previous grid point.
- **Recession**: time from rain end to cycle rate back within 1.2× baseflow. Phase 4 clarification: same rate over a 2-h window, held for 1 h; the search starts at the rain end, or at the lag crossing when the home only responded after the rain stopped; analysis stops 7 days after the rain end or at the next storm.
- **Segment load**: sum of storm volume per segment ÷ homes reporting (only if ≥ 3 homes). Storm volume per home = Σ §9 estimated volumes (plus roll-up count × cycle drop × pit area) over [onset, rain end + recession), or to the end of the data while not receded: `volume_l`, a conservative floor, since a level drop leaves out inflow during the run. Owner decision 2026-09-12: also `inflow_est_l` = the home's pump rate × run time over the same window (roll-ups: × their total run time; never below a cycle's floor; backup-pump runs, never calibrated in dry weather, count at their floor), with `pump_rate_lps` = pit area × median(level drop ÷ run time) over the dry-weather cycles baseflow uses. Both are NULL without a pit area or a baseflow calibration. `pump_rate_source` records where the rate came from: `dry_weather` (learned, the only source today) or `bucket_test` (a homeowner pours a known volume into the pit and times the pump), which will take precedence over the learned rate once recorded. On the Phase 4 e2e the pump rate is within 5 % of the simulator's and `inflow_est_l` within 2.2 % of the true storm + baseflow inflow, where `volume_l` was 8–62 % low.
- **Outage risk**: mains_lost AND level rising AND rain in last 6 h → alert owner, then opted-in neighbours. Level rising = the sensor distance strictly decreasing over 3 heartbeats within 1 h by ≥ 10 mm. Rain term (Phase 4): rain recorded by any source (gauge or ECCC) in intervals overlapping the 6 h before the reading; if rainfall data do not reach within 30 min of the reading (feed lagging or absent), rain is assumed — a missing feed never hides the alert.
- **Severities**: CRITICAL = float_high, dry_run, continuous_run, outage_risk; WARNING = mains_lost, sensor_fault, short_cycling, low_battery, offline.

## 11. External data

- ECCC hourly climate data via the MSC GeoMet OGC API (`api.weather.gc.ca`) for the nearest London station. Verified 2026-09-12:
  - Collection `climate-hourly` (`/collections/climate-hourly/items?f=json&CLIMATE_IDENTIFIER=…&datetime=…&sortby=LOCAL_DATE&limit=…`, paged by `rel=next` links, which omit `f`). The `datetime` filter applies to `LOCAL_DATE` (local standard time, UTC−5 all year); the service pads a day and selects on `UTC_DATE`.
  - Station: **LONDON CS, CLIMATE_IDENTIFIER 6144478** (STN_ID 10999, 43.03 N 81.15 W, ≈23 km from Timberwalk). Hourly `PRECIP_AMOUNT` is populated (no nulls 2026-07-01 → 09-12; its 1990s rows are null). LONDON A (6144473, same airport) has hourly rows but `PRECIP_AMOUNT` is always null. No closer station reports hourly data; the next hourly stations are 74+ km away.
  - Interval semantics: `PRECIP_AMOUNT` at `UTC_DATE` is the precipitation in the hour **ending** at `UTC_DATE`, so `rainfall.ts = UTC_DATE − 1 h`, `interval_s = 3600`. Evidence: hourly sums over (06Z, 06Z] reproduce `climate-daily` `TOTAL_PRECIPITATION` on every June–September 2026 day with rain in the 06Z boundary hour (e.g. 2026-06-05: daily 8.1 mm, hour-ending sum 8.1, hour-beginning sum 2.8); 88/100 days match exactly vs 81 for hour-beginning, the rest differ by 0.1 mm rounding.
  - Data are published hours late and revised: the poller re-reads 48 h each hour (7 days on start), skips null or `M` hours, and only real changes are written. Env: `WEATHER_ECCC_ENABLED` (off switch), `WEATHER_ECCC_URL`, `WEATHER_ECCC_STATION`, `WEATHER_ECCC_POLL_INTERVAL`, `WEATHER_ECCC_BACKFILL`, `WEATHER_ECCC_LOOKBACK`, `WEATHER_ECCC_TIMEOUT`.
  - Licence: MSC open data (GeoMet included) is under the **Environment and Climate Change Canada Data Services End-use Licence** (v2.1.1, `https://eccc-msc.github.io/open-data/licence/readme_en/`): copying, redistribution and adaptation are allowed with the attribution "Data Source: Environment and Climate Change Canada". Verified 2026-09-13; it is ECCC's own licence, not the Open Government Licence – Canada.
  - Hourly LONDON CS amounts for 2026-08-01 → 09-12 are complete (1008 of 1008 hours, 236.3 mm). `make eccc-import FROM=… TO=…` caches them with their fetch windows in the gitignored `data/rain/` for `-scenario eccc` (ADR 0008).
- Own rain gauges are primary; ECCC is fallback (hours no gauge covers) and cross-check.
- **County of Middlesex open data** (real-geography sites, ADR 0008). Verified 2026-09-13:
  - Address points: `https://utility.arcgis.com/usrsvcs/servers/f4dd79bdd35a456c87c0c20668138c05/rest/services/MiddlesexCounty/Base_Layers/MapServer/1` (ArcGIS MapServer point layer "Address", native EPSG:26917, maxRecordCount 2000, pagination and orderBy supported). Fields used: `OBJECTID_1`, `GlobalID`, `MUNNUMBER`, `STREET_UNI`, `FULLADDRES`, `FULLSTREET`, `MUNCODE` (`MIDC` = Middlesex Centre). Query per street: `where=FULLSTREET='…' AND MUNCODE='MIDC'`, `outSR=4326`, `orderByFields=OBJECTID_1`, paged with `resultOffset`/`resultRecordCount` while `exceededTransferLimit` is true. Townhouse unit points carry a blank `MUNNUMBER`, the unit in `STREET_UNI` and the civic number only in `FULLADDRES`; the civic point of a numbered block with units is dropped (Maplewood Lane: 28 points, 27 homes).
  - Road centrelines (Single Line Road Network): `https://utility.arcgis.com/usrsvcs/servers/7d7b31fb8cf144939edc0a4436706dfe/rest/services/MiddlesexCounty/SLRN/MapServer/0` (polyline, EPSG:26917). Query: `where=FULLNAME='…' AND (MUNL='MIDC' OR MUNR='MIDC')`; fields `FULLNAME`, `LFADD`/`LTADD`/`RFADD`/`RTADD`, `CLASS`, `SUBDIVISIO`, `PROPOSED` (non-zero pieces dropped), `MUNL`/`MUNR`. The road network spells Arrowwood Path `ARROWWOODPATH`. Every street probed chains into a single centreline.
  - Timberwalk is subdivision plan `39T-MC0401` (Arrowwood Path 31, Mayapple Crescent 48, Mossy Wood Walk 8, Songbird Lane 3, Timberwalk Trail 73, Violet Court 22 addresses) plus `39T-MC1901` (Timberwalk Close 5, and the southern part of Timberwalk Trail). Nearby, cached: Bowman Drive 102, Basil Crescent 57, Stone Field Lane 43 (39T-MC1401); Woodlily Lane 45, Meadowsweet Crescent 34, Periwinkle Drive 26, Red Clover Court 21; Ashwood Crescent 37, Maplewood Lane 28, Stone Field Gate 23, Havenwood Lane 11, Havenwood Street 11 (39T-MC0601/0602).
  - Query errors come back as HTTP 200 with `{"error":{"code":…}}`; the importer retries 429, 5xx, transport and decode failures, and fails on the rest.
  - Licence unconfirmed (§14); attribution "Contains information from the County of Middlesex Open Data portal". Raw pages, snapshots and outlines stay in the gitignored `data/`.
- **Alert email: Resend** over SMTP (decided 2026-09-12): `smtp.resend.com`, port 465 (implicit TLS), user `resend`, the Resend API key as `SMTP_PASSWORD`, `SMTP_FROM` on a Resend-verified domain (`onboarding@resend.dev` only delivers to the account owner). Any SMTP provider works through the same `SMTP_*` variables; an empty `SMTP_HOST` logs instead of sending. Configuration lives only in the gitignored `deploy/compose/.env`; `make alerts-testmail` checks delivery. No Resend idempotency header (a retry's fresh Date header would be a conflicting payload).
- **Owner sign-in: Clerk** (decided 2026-09-12, ADR 0006): the dashboard uses `@clerk/react` with the publishable key in `web/.env.local`; the api-gateway verifies Clerk session JWTs against the issuer's public JWKS (`CLERK_ISSUER`, `CLERK_JWKS_URL`, `CLERK_AUTHORIZED_PARTIES`) and needs no Clerk secret. `https://dev.ecoworks.ca:3034` must be an allowed origin in the Clerk dashboard.

## 12. Phases

Each phase is sized for one to three Claude Code sessions. Do not start a phase until the previous acceptance criteria pass.

### Phase 0 — Scaffold
- [x] Go module, Makefile (`make proto lint test up down`), buf config
- [x] docker-compose: Postgres 16, Mosquitto, ChirpStack v4 (+ Redis), placeholder services
- [x] GitHub Actions: buf lint/breaking, golangci-lint, `go test -race`, Docker build
- [x] `progress.md`, `docs/adr/0001-monorepo.md`
- **Accept:** `make up` brings stack up on the lab VM; CI green on empty services.

### Phase 1 — Contracts + simulator
- [x] Protos in §8; generated code committed or generated in CI (decide + ADR)
- [x] `internal/codec` with golden-vector tests for all fPorts
- [x] `cmd/simulator`: 60 homes across ~8 segments; per-home parameters (pit area, baseflow, lag, recession, pump health); rainfall scenarios (dry week, 25 mm summer storm, 50 mm spring thaw + rain, power outage mid-storm, failing pump)
- [x] Simulator publishes ChirpStack-shaped MQTT uplink events so the real bridge path is exercised
- **Accept:** simulator replays a 50 mm storm for 60 homes at 60× speed, deterministic with a seed.

### Phase 2 — Ingest path
- [x] `lora-bridge` (ChirpStack MQTT integration → decode → gRPC stream to ingest)
- [x] `mqtt-bridge` for ESP32 Wi-Fi nodes publishing the codec bytes in the `sumpnet/v1/{dev_eui}/up` envelope (§5)
- [x] `ingest` service + migrations + sqlc queries, idempotent on (device_id, ts, fcnt)
- [x] Backpressure: bounded channels, batch inserts via `pgx.CopyFrom`
- **Accept:** integration test (testcontainers) ingests a full simulated storm with zero loss and no duplicates.

### Phase 3 — Cycle detection + alerts
- [x] `cycle-detector` computes est_volume, dry-run, short-cycling, continuous-run
- [x] `alerts` service: raise/ack/resolve, dedupe window, email via SMTP (Resend; SMS later)
- **Accept:** failing-pump and outage scenarios raise the correct alerts within 2 simulated minutes.

### Phase 4 — Weather + storm analytics
- [x] `weather` service polls ECCC, stores rainfall; ingests rain gauge nodes (fPort 5 codec, simulator gauges, `rain_gauge_uplinks`, gauge consumer, GeoMet poller)
- [x] `storm-analytics` segments storm events and computes `home_storm_metrics` (ADR 0007)
- [x] `internal/privacy` enforces segment k ≥ 3 (package + ADR 0005; the api-gateway builds every public view with it)
- **Accept:** simulated storm produces lag/recession within ±10% of the simulator's ground-truth parameters. Status 2026-09-12 (`internal/e2e/phase4_integration_test.go`, storm50-long × 16 homes): recession within ±10 % for 16/16 homes; lag within ±10 % for 11/16, all within one heartbeat interval (15 min), median error 5 min — ±10 % is below the data resolution for fast homes; the owner approved "±10 % or ±15 min, median ≤ 5 min" for lag on 2026-09-12, with recession strict ±10 % (§14). Storm inflow (`inflow_est_l`) within 2.2 % of truth.

### Phase 5 — API gateway + dashboard
- [x] `api-gateway`: gRPC + REST (grpc-gateway), `WatchNeighbourhood` server streaming, auth (Clerk JWTs, ADR 0006)
- [ ] `web/`: MapLibre segment heatmap, storm replay slider, owner home view, alert list — built (lint, Vitest, production build, HTTPS dev-server smoke); tick after the live map check
- **Accept:** live storm replay visible on the map; owner sees own home only; public view shows aggregates only.
  - Status 2026-09-12: "owner sees own home only" and "public view shows aggregates only" pass in `internal/e2e` over gRPC and REST. Phase 4 is merged, so `storm_events`/`home_storm_metrics` are populated; "live storm replay visible on the map" still needs a browser check against `make up`. The live segment heatmap already animates during a simulator replay (neighbourhood clock).

### Phase 6 — MCP server
- [ ] Tools: `list_storm_events`, `get_storm_summary`, `get_segment_load`, `get_my_home_health`, `list_active_alerts`
- [ ] Same privacy rules as the API — MCP reads through QueryService, never direct SQL
- **Accept:** Claude can answer "Which segments pumped the most in the last storm, and how fast did they recover?"

### Phase 7 — Firmware + first real nodes
- [ ] ESP32-S3 firmware (PlatformIO; RadioLib LoRaWAN OTAA, Wi-Fi/MQTT envelope as fallback): sensor read, local cycle detection, payload encode matching `internal/codec`, storm mode
- [ ] Single-house RF survey: RSSI/SNR at SF7–SF10 from the sump location; document in `docs/rf-survey.md`
- [ ] Bring one LoRa node and one Wi-Fi node live alongside simulated homes
- **Accept:** real node data passes through the identical pipeline as the simulator; RF margin documented.

### Phase 8 — AWS + load test
- [ ] Terraform: VPC, ECS Fargate per service, RDS Postgres (pg_partman), ALB (gRPC + HTTP), Secrets Manager, CloudWatch
- [ ] Load test: simulator at 1k, 10k, 50k devices in storm mode; record p50/p99 ingest latency, throughput, DB write rate, cost per month
- [ ] Publish `loadtest/results/README.md` with graphs and the exact commands to reproduce
- **Accept:** `terraform apply` → working stack; `terraform destroy` → clean; results published.

### Phase 9 — Pilot
- [ ] Consent form + data policy page (plain language, deletion on request)
- [ ] 5–10 volunteer homes, two gateways, two rain gauges
- [ ] First real storm report
- **Accept:** one storm season of real data; first segment-level findings written up.

## 13. README must show (for reviewers)

- The problem in two sentences plus the architecture diagram
- `make up` quickstart that works on a clean machine
- Load-test results table
- ADR index (monorepo, eventing, partitioning vs Timescale, privacy thresholds)
- A short "what I'd do differently at 1M devices" section

## 14. Open questions

- [ ] Gateway sites: which volunteer homes have the best line of sight?
- [x] Owner auth: **Clerk** (gateway verifies Clerk JWTs via JWKS; `home_owners.auth_subject`). Decided 2026-09-12.
- [ ] Should the simulator's hydrology model be calibrated against the first real storm before any public numbers are shared?
- [ ] Licence: MIT vs AGPL for the platform; firmware separate?
- [x] Rain-gauge node uplink format: **fPort 5, 11 B, cumulative tip counter** (§5). Decided 2026-09-12.
- [ ] Per-owner alert email needs a decryption scheme for `homes.owner_contact_encrypted` (and auth); Phase 3 emails a single operator address.
- [x] Phase 4 acceptance tolerance for response lag: ±10 % is below what 15-min heartbeats, 0.2 mm tips and a 5-min gauge wake can resolve for homes that respond within ~60 min (truth lags 11–30 min need 1–3 min accuracy; measured median error 3–5 min, worst ~13 min, from the crossing localisation and the onset alike). The e2e currently accepts ±10 % or ±15 min, whichever is larger, plus a 5-min median. Accept that, or change the firmware cadence (e.g. heartbeats every 5 min in storm mode: in-memory evidence 44–47/60 homes within ±10 % with the true onset), or restate the criterion? (Recession meets ±10 % for every home.) **Decided 2026-09-12 by owner: approved as built** — recession strict ±10 % for every home; lag ±10 % or ±15 min (one heartbeat interval), whichever is larger, with median error ≤ 5 min.
- [x] Storm `volume_l` is Σ §9 estimated volumes (pit area × level drop), which leaves out inflow during a pump run; for homes whose inflow nears pump capacity it understates the storm inflow up to ~2.6× (cycle counts match truth exactly). Keep §9, or estimate pumped volume as pump rate (learned from dry-weather runs) × run time? **Decided 2026-09-12 by owner: add a pump-rate estimate** — `volume_l` stays the §9 floor; `inflow_est_l` (calibrated pump rate × run time) and `pump_rate_lps` are stored beside it (migration 0006, §10), with `pump_rate_source` (`dry_weather` now, `bucket_test` when measured). Exposure through query.proto is the main session's to add.
- [x] Real-geography site street set: simulate **the 7 Timberwalk streets** (plan 39T-MC0401 plus Timberwalk Close: 190 addresses) by default; cache the northern streets now as the larger sites `timberwalk-plus-basil-bowman`, `timberwalk-plus-plant-streets` and `timberwalk-nearby` (`internal/site/sites/`). **Decided 2026-09-13 by the owner.**
- [ ] County of Middlesex data licence: the portal item has no licence text and the hub's "Terms of Use" label is unlinked. Confirm redistribution, derived maps and attribution wording with the County before committing any snapshot or segment outline, or publishing a map built from them (ADR 0008).
- [ ] Segment kinds on real streets are illustrative (Timberwalk streets `wooded`, the rest `standard`). Should they come from something measured (tree cover, grading, distance to the stormwater pond) before any public comparison between streets?
- [ ] Bucket test (research report `docs/research/pump-flow-bucket-test.md`, PR #10): pouring a bucket and timing the pump does not by itself measure flow rate; a measured pour mainly calibrates the pit's effective area. Should a bucket test calibrate `homes.pit_area_m2` (keeping the learned dry-weather drawdown for `pump_rate_lps`) instead of recording an overriding `pump_rate_source = bucket_test` rate as migration 0006 reserves? The report's other proposals (P2–P9: §4 ultrasonic sensor notes, §5 payload definitions and calibration records, §9 `bucket_tests` table, §10 area/drain-back/degradation rules, firmware calibration mode, dashboard wizard, simulator pump curves, further §14 questions) also await decisions.
