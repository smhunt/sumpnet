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
House nodes (LoRa 915 MHz) ─┐
Rain / groundwater nodes  ──┼─> LoRaWAN gateways ×2 ─> ChirpStack v4 ─┐
                            │                                         │ MQTT integration events
Existing Photon (WiFi) ─────┴────────────────────────> Mosquitto ─────┤
                                                                      v
                                   ┌──────────── Go services (gRPC) ────────────┐
                                   │ lora-bridge  mqtt-bridge  ->  ingest       │
                                   │ cycle-detector   storm-analytics  weather  │
                                   │ alerts   api-gateway (gRPC + REST + stream)│
                                   │ mcp-server                                 │
                                   └────────────────────────────────────────────┘
                                                    │
                          Postgres 16 (native partitioning + pg_partman)
                                                    │
                           React/TS dashboard (MapLibre) · Claude via MCP
```

Canada uses the US915 LoRaWAN band plan (902–928 MHz). No duty-cycle limit, but 400 ms dwell time — keep SF7–SF10 at 125 kHz.

## 4. Hardware (per pilot)

| Item | Qty | Notes |
|---|---|---|
| LoRa node MCU: RAK WisBlock RAK4631 + RAK19007 base | 1 per house | nRF52840 + SX1262, low power. Alt: Adafruit Feather RP2040 RFM95 (BC Robotics) |
| JSN-SR04T waterproof ultrasonic | 1 per house | Pit level (already proven on Photon node) |
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

## 6. Repo layout

```
sumpnet/
├── go.mod                      # single module to start; split later only if needed
├── buf.yaml / buf.gen.yaml
├── proto/sumpnet/
│   ├── telemetry/v1/telemetry.proto
│   ├── query/v1/query.proto
│   └── alerts/v1/alerts.proto
├── cmd/
│   ├── simulator/  lora-bridge/  mqtt-bridge/  ingest/
│   ├── cycle-detector/  storm-analytics/  weather/
│   ├── alerts/  api-gateway/  mcp-server/
├── internal/
│   ├── codec/        # payload encode/decode
│   ├── domain/       # Reading, CycleEvent, StormEvent, Home, Segment
│   ├── store/        # pgx/v5 + sqlc-generated queries
│   ├── hydrology/    # lag, recession, baseflow, volume maths (pure functions)
│   ├── platform/     # config, slog setup, graceful shutdown, health checks
│   └── privacy/      # aggregation + k-threshold enforcement
├── migrations/       # golang-migrate SQL
├── firmware/         # Arduino/PlatformIO for RAK4631
├── web/              # React/TS + MapLibre dashboard
├── deploy/
│   ├── compose/      # docker-compose.yml, chirpstack + mosquitto config
│   └── terraform/    # AWS: VPC, ECS Fargate, RDS, ALB, Secrets Manager
├── loadtest/         # simulator scenarios + k6 or ghz scripts, results/*.md
├── .github/workflows/ci.yml
├── Makefile
├── prompt_plan.md
└── progress.md
```

## 7. Go conventions

- Go 1.23+. `log/slog` JSON logging. `context.Context` everywhere; no goroutine without a cancellation path.
- Errors wrapped with `fmt.Errorf("...: %w", err)`; sentinel errors in `domain`.
- gRPC: `google.golang.org/grpc`, protos managed with `buf` (lint + breaking-change check in CI). REST via `grpc-gateway`.
- MQTT: `github.com/eclipse/paho.golang` (v5). Postgres: `pgx/v5` + `sqlc`. Migrations: `golang-migrate`.
- MCP: official Go SDK `github.com/modelcontextprotocol/go-sdk`.
- Tests: table-driven unit tests; `testcontainers-go` for Postgres/Mosquitto integration tests.
- Every service: `/healthz`, `/readyz`, Prometheus `/metrics`, graceful shutdown on SIGTERM.
- Lint: `golangci-lint` (errcheck, govet, staticcheck, revive, gosec).
- Multi-stage Dockerfiles producing distroless images.

## 8. Proto sketch

```proto
// telemetry/v1
service IngestService {
  rpc SubmitReadings(stream SubmitReadingsRequest) returns (SubmitReadingsResponse);
  rpc SubmitCycleEvents(stream SubmitCycleEventsRequest) returns (SubmitCycleEventsResponse); // also carries fPort 4 storm summaries
  rpc SubmitAlarms(stream SubmitAlarmsRequest) returns (SubmitAlarmsResponse);                // fPort 3
}

// query/v1
service QueryService {
  rpc GetHome(GetHomeRequest) returns (GetHomeResponse);                // owner-scoped
  rpc ListStormEvents(ListStormEventsRequest) returns (ListStormEventsResponse);
  rpc GetStormEvent(GetStormEventRequest) returns (GetStormEventResponse); // segment aggregates
  rpc WatchNeighbourhood(WatchNeighbourhoodRequest) returns (stream WatchNeighbourhoodResponse); // wraps NeighbourhoodUpdate (buf RPC_RESPONSE_STANDARD_NAME)
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
- `segments` (id, name, geometry — street segment polygon)
- `readings` (device_id, ts, level_mm, temp_c, rh, batt_mv, flags) — partitioned monthly
- `cycle_events` (device_id, started_at, run_s, peak_current_a, level_start_mm, level_end_mm, pump_id, est_volume_l) — partitioned monthly
- `rainfall` (source, station_id, ts, mm) — rain gauges + ECCC
- `storm_events` (id, started_at, ended_at, total_rain_mm, peak_intensity_mm_h)
- `home_storm_metrics` (storm_id, home_id, lag_min, recession_min, volume_l, cycles)
- `alerts` (id, home_id, code, raised_at, acked_at, resolved_at)

Estimated volume per cycle = `pit_area_m2 × (level_end_mm − level_start_mm) / 1000 × 1000 L`. An 18" basin is about 0.164 m².

## 10. Analytics definitions (`internal/hydrology`, pure + tested)

- **Cycle**: current above threshold for ≥ 3 s with level drop ≥ 20 mm.
- **Dry run**: current on ≥ 30 s with level drop < 5 mm → failed pump or stuck check valve.
- **Short cycling**: consecutive cycles < 60 s apart for ≥ 5 cycles → check valve or float issue.
- **Continuous run**: current on > 10 min.
- **Baseflow**: median dry-weather cycles/day (no rain for 72 h) → water-table indicator.
- **Storm event**: rainfall ≥ 5 mm total with gaps < 6 h.
- **Response lag**: time from rain onset (≥ 1 mm/h) to cycle rate > 2× baseflow.
- **Recession**: time from rain end to cycle rate back within 1.2× baseflow.
- **Segment load**: sum of storm volume per segment ÷ homes reporting (only if ≥ 3 homes).
- **Outage risk**: mains_lost AND level rising AND rain in last 6 h → alert owner, then opted-in neighbours.

## 11. External data

- ECCC hourly climate data via the MSC GeoMet OGC API (`api.weather.gc.ca`) for the nearest London station. Verify collection names and station IDs in phase 4.
- Own rain gauges are primary; ECCC is fallback and cross-check.

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
- [ ] `lora-bridge` (ChirpStack MQTT integration → decode → gRPC stream to ingest)
- [ ] `mqtt-bridge` for the existing Photon node's topics
- [ ] `ingest` service + migrations + sqlc queries, idempotent on (device_id, ts, fcnt)
- [ ] Backpressure: bounded channels, batch inserts via `pgx.CopyFrom`
- **Accept:** integration test (testcontainers) ingests a full simulated storm with zero loss and no duplicates.

### Phase 3 — Cycle detection + alerts
- [ ] `cycle-detector` computes est_volume, dry-run, short-cycling, continuous-run
- [ ] `alerts` service: raise/ack/resolve, dedupe window, email via SMTP (SMS later)
- **Accept:** failing-pump and outage scenarios raise the correct alerts within 2 simulated minutes.

### Phase 4 — Weather + storm analytics
- [ ] `weather` service polls ECCC, stores rainfall; ingests rain gauge nodes
- [ ] `storm-analytics` segments storm events and computes `home_storm_metrics`
- [ ] `internal/privacy` enforces segment k ≥ 3
- **Accept:** simulated storm produces lag/recession within ±10% of the simulator's ground-truth parameters.

### Phase 5 — API gateway + dashboard
- [ ] `api-gateway`: gRPC + REST (grpc-gateway), `WatchNeighbourhood` server streaming, auth (owner magic-link or Clerk)
- [ ] `web/`: MapLibre segment heatmap, storm replay slider, owner home view, alert list
- **Accept:** live storm replay visible on the map; owner sees own home only; public view shows aggregates only.

### Phase 6 — MCP server
- [ ] Tools: `list_storm_events`, `get_storm_summary`, `get_segment_load`, `get_my_home_health`, `list_active_alerts`
- [ ] Same privacy rules as the API — MCP reads through QueryService, never direct SQL
- **Accept:** Claude can answer "Which segments pumped the most in the last storm, and how fast did they recover?"

### Phase 7 — Firmware + first real nodes
- [ ] RAK4631 firmware (PlatformIO): sensor read, local cycle detection, payload encode matching `internal/codec`, storm mode, OTAA join
- [ ] Single-house RF survey: RSSI/SNR at SF7–SF10 from the sump location; document in `docs/rf-survey.md`
- [ ] Bring the existing Photon node and one LoRa node live alongside simulated homes
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
- [ ] Owner auth: magic link vs Clerk?
- [ ] Should the simulator's hydrology model be calibrated against the first real storm before any public numbers are shared?
- [ ] Licence: MIT vs AGPL for the platform; firmware separate?
- [ ] Rain-gauge node uplink format: §5 defines no fPort for tipping-bucket gauges. Define (fPort 5?) before Phase 4.
