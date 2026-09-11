# sumpnet

Sump pumps are the last line of defence for basements in a modern subdivision, and nobody
knows how they behave during a storm until one fails. sumpnet instruments a neighbourhood of
them — pit level, pump current, float switch, mains power — over LoRaWAN, and turns the data
into street-by-street storm response, early warning of failing pumps, and outage risk alerts.

Pilot neighbourhood: Timberwalk, Ilderton (Middlesex Centre, ON). Working plan and spec:
[`prompt_plan.md`](prompt_plan.md). Session log: [`progress.md`](progress.md).

## Architecture

```
House nodes (LoRa 915 MHz) ─┐
Rain / groundwater nodes  ──┼─> LoRaWAN gateways ×2 ─> ChirpStack v4 ─┐
                            │                                         │ MQTT integration events
ESP32 Wi-Fi nodes (dev) ────┴────────────────────────> Mosquitto ─────┤
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

Single Go module, one binary per service under `cmd/`, shared packages under `internal/`.
Contracts are protobuf (`proto/`, generated code committed under `gen/go`). Everything runs
locally with Docker Compose; AWS is a second deployment target (Phase 8).

## Quickstart

Requires Go 1.26+, Docker, and `make`.

```bash
make tools        # buf, golangci-lint, sqlc, migrate into ./bin (versions in .versions.env)
make up           # Postgres 16 + pg_partman, Redis, Mosquitto, ChirpStack v4, all Go services
make ps           # everything should be "healthy"

# Replay a 50 mm storm for 60 homes at 60x speed into the stack's MQTT broker,
# shaped exactly like ChirpStack uplink events. Writes ground truth to loadtest/results/.
make sim SCENARIO=storm50 SEED=42 SPEED=60

# Or dump the event stream as JSONL, unpaced, with the stream hash on stderr:
go run ./cmd/simulator -scenario storm50 -seed 42 -sink stdout -hash > events.jsonl
go run ./cmd/simulator -list-scenarios
```

Ports are read from `deploy/compose/.env` (created from `.env.example`). ChirpStack's UI is on
the `CHIRPSTACK_PORT` (default 3131, admin/admin).

Development loop: `make lint`, `make test`, `make test-integration` (Docker), `make proto` after
editing anything under `proto/`.

## The simulator

`cmd/simulator` is a deterministic model of a neighbourhood: 60 homes across 8 street segments
(standard, wooded, near the pond, high ground), each with its own pit geometry, baseflow,
rain-response lag, recession time and pump health. Rainfall scenarios (`dry-week`, `storm25`,
`storm50`, `thaw50`, `outage`, `failing-pump`) drive a linear-reservoir hydrology model; an
emulated node firmware turns the resulting pump cycles into the same binary payloads the real
RAK4631 nodes will send, including storm-mode roll-ups and alarms.

The same seed always produces the same byte stream, and the exported truth file contains the
realised lag, recession and volume per home per storm, so the analytics services can be tested
against known answers.

## Status

| Phase | State |
|---|---|
| 0 — Scaffold (module, compose stack, CI) | done |
| 1 — Contracts + simulator | in progress |
| 2 — Ingest path | planned |
| 3 — Cycle detection + alerts | planned |
| 4 — Weather + storm analytics | planned |
| 5 — API gateway + dashboard | planned |
| 6 — MCP server | planned |
| 7 — Firmware + first real nodes | planned |
| 8 — AWS + load test | planned |
| 9 — Pilot | planned |

Decisions are recorded in [`docs/adr/`](docs/adr/README.md).
