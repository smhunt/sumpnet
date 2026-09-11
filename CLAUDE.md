# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project status

sumpnet is a neighbourhood sump-pump and drainage monitoring platform (pilot: Timberwalk, Ilderton, ON). It has two goals: a portfolio showcase for senior Go backend work (gRPC, microservices, AWS, CI/CD, published load-test numbers) and a real civic tool.

**The repo is currently pre-code.** `prompt_plan.md` is the source of truth for architecture, payload formats, data model, analytics definitions, and the phased roadmap. Read the relevant section of it before starting work, and keep it in sync if a decision changes. The directory is not yet a git repository (Phase 0 creates it).

## Workflow rules from the plan

- Work proceeds in phases (§12 of `prompt_plan.md`). **Don't start a phase until the previous phase's acceptance criteria pass.** Tick the checkboxes in `prompt_plan.md` as items land.
- Update `progress.md` at the end of every session.
- Record significant decisions as ADRs in `docs/adr/` (planned: 0001 monorepo, 0002 eventing, partitioning vs Timescale, privacy thresholds, generated-code-committed-or-not).
- Resolve open questions in §14 with the user; don't pick an answer silently (owner auth: magic link vs Clerk, licence, gateway sites, simulator calibration).

## Commands (planned, Phase 0)

The Makefile doesn't exist yet. The plan defines these targets:

```bash
make proto   # buf generate from proto/
make lint    # golangci-lint (errcheck, govet, staticcheck, revive, gosec) + buf lint
make test    # go test -race ./...
make up      # docker compose stack: Postgres 16, Mosquitto, ChirpStack v4 + Redis, services
make down
```

Single test: `go test -race -run TestName ./internal/codec/...`. Integration tests use `testcontainers-go` (Docker must be running).

CI (`.github/workflows/ci.yml`) runs buf lint + breaking-change check, golangci-lint, `go test -race`, and Docker builds. When you add a Makefile target, update this section.

## Architecture

```
LoRa house/rain nodes -> LoRaWAN gateways -> ChirpStack v4 --MQTT--> lora-bridge --┐
Existing Photon (WiFi) ------------------------> Mosquitto --> mqtt-bridge ---------┼--gRPC stream--> ingest -> Postgres 16
                                                                                    │
cycle-detector, storm-analytics, weather, alerts  <-- Postgres LISTEN/NOTIFY --------┘
api-gateway (gRPC + grpc-gateway REST + WatchNeighbourhood stream) -> React/TS + MapLibre dashboard
mcp-server -> QueryService (never direct SQL)
```

- **Single Go module** (Go 1.23+). Binaries live in `cmd/<service>`; shared code lives in `internal/` (`codec`, `domain`, `store`, `hydrology`, `platform`, `privacy`).
- **Protos** live in `proto/sumpnet/{telemetry,query,alerts}/v1/` and are managed by `buf`. The three services are `IngestService` (client-streaming submits), `QueryService` (owner-scoped reads plus the neighbourhood stream) and `AlertService`.
- **Eventing between services** starts with Postgres `LISTEN/NOTIFY` rather than NATS/SQS.
- **The simulator is a first-class input.** `cmd/simulator` publishes *ChirpStack-shaped* MQTT uplink events, so the real bridge → ingest path gets exercised. Real nodes must flow through the identical pipeline. The simulator must be deterministic given a seed, and its per-home parameters are the ground truth that storm analytics is validated against (±10%).
- **Local-first:** Docker Compose on the lab VM is the primary target. AWS (Terraform, ECS Fargate, RDS) is a second deployment target, not a dependency.

## Invariants that span multiple components

- **Payload decoding happens only in Go (`internal/codec`)**, never in ChirpStack JS codecs. Uplinks are binary and little-endian: fPort 1 heartbeat (10 B), 2 pump cycle (11 B), 3 alarm (3 B, confirmed), 4 storm-mode summary. The exact layouts are in §5 of the plan. Firmware (`firmware/`, PlatformIO on RAK4631) must encode byte-for-byte what the codec decodes. Keep golden byte-vector table tests as the shared contract, and change them together with firmware.
- **Ingest is idempotent** on `(device_id, ts, fcnt)`. It uses bounded channels for backpressure and batches inserts with `pgx.CopyFrom`.
- **Privacy:** data is opt-in, device IDs are pseudonymous, per-house data is visible only to its owner, and public views aggregate to street segments only when **≥ 3 homes** report. `internal/privacy` enforces this. The API gateway and MCP server must both go through it (MCP reads via QueryService).
- **Analytics are pure functions** in `internal/hydrology`, with thresholds defined in §10 of the plan (cycle, dry run, short cycling, continuous run, baseflow, storm event, lag, recession, segment load, outage risk). Use those definitions exactly. If you change one, change the plan too.
- `readings` and `cycle_events` are partitioned monthly (native partitioning + pg_partman).

## Go conventions

- Use only the stdlib plus small, well-known libraries. No heavy frameworks.
- `log/slog` JSON logging. Pass `context.Context` everywhere, and give every goroutine a cancellation path.
- Wrap errors with `fmt.Errorf("...: %w", err)`. Sentinel errors live in `internal/domain`.
- Libraries: `google.golang.org/grpc` + `grpc-gateway`, `github.com/eclipse/paho.golang` (MQTT v5), `pgx/v5` + `sqlc`, `golang-migrate` (SQL in `migrations/`), `github.com/modelcontextprotocol/go-sdk`.
- Every service exposes `/healthz`, `/readyz` and Prometheus `/metrics`, and shuts down gracefully on SIGTERM. The shared setup lives in `internal/platform`.
- Tests are table-driven. Dockerfiles are multi-stage and produce distroless images.

## Hard constraints

- Canada uses the **US915** band plan with a 400 ms dwell time, so keep SF7–SF10 at 125 kHz. Storm mode (> 6 cycles in 15 min) rolls cycle events into fPort 4 summaries to save airtime.
- Don't propose sensors on municipal assets (pond, outlet, pump station) without Middlesex Centre approval.
- Current sensing must be non-invasive only (CT clamp on a plug-through splitter). Never modify pump wiring.
- Source hardware from Canadian suppliers (DigiKey Canada, BC Robotics).

## Ports

No sumpnet ports are registered in `~/.claude/PORTS.md` yet. Before exposing Postgres, Mosquitto, ChirpStack, api-gateway or the web dashboard from the compose stack, follow the port protocol in the global CLAUDE.md. Remember that 8xxx is blocked, which rules out ChirpStack's default 8080 and MQTT-over-TLS 8883 (Mosquitto's plain 1883 is fine).
