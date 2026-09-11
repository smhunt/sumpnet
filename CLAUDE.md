# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project status

sumpnet is a neighbourhood sump-pump and drainage monitoring platform (pilot: Timberwalk, Ilderton, ON). It has two goals: a portfolio showcase for senior Go backend work (gRPC, microservices, AWS, CI/CD, published load-test numbers) and a real civic tool.

`prompt_plan.md` is the source of truth for architecture, payload formats, data model, analytics definitions, and the phased roadmap. Read the relevant section of it before starting work, and keep it in sync if a decision changes. `progress.md` records what each session did and the current phase; update it before ending a session. Module path is `github.com/smhunt/sumpnet` (GitHub account `smhunt`, private repo).

## Workflow rules from the plan

- Work proceeds in phases (§12 of `prompt_plan.md`). **Don't start a phase until the previous phase's acceptance criteria pass.** Tick the checkboxes in `prompt_plan.md` as items land.
- Update `progress.md` at the end of every session.
- Record significant decisions as ADRs in `docs/adr/` (planned: 0001 monorepo, 0002 eventing, partitioning vs Timescale, privacy thresholds, generated-code-committed-or-not).
- Resolve open questions in §14 with the user; don't pick an answer silently (owner auth: magic link vs Clerk, licence, gateway sites, simulator calibration).

## Commands

Go is installed via Homebrew (`/opt/homebrew/bin/go`). Dev tools are pinned in `.versions.env` and installed into the gitignored `./bin` by `make tools`; the Makefile and CI both read those pins, so never call a globally installed `buf`/`golangci-lint`.

```bash
make tools   # one-time: buf, golangci-lint, sqlc, migrate into ./bin at pinned versions
make lint    # buf lint + golangci-lint run + golangci-lint fmt --diff (fails on unformatted code)
make fmt     # apply gofmt/goimports
make test    # go test -race -count=1 -cover ./...
make proto   # buf generate → gen/go (gitignored until the Phase 1 ADR)
make up      # docker compose up -d --build --wait (blocks until every healthcheck passes)
make down / make ps / make logs S=<service>
make env     # copies deploy/compose/.env.example → .env if missing (make up does this)
```

Single test: `go test -race -run TestName ./internal/platform/...`. Integration tests (Phase 1+) are behind `-tags integration` and use `testcontainers-go`, so Docker must be running.

Breaking-change check locally: `./bin/buf breaking --against '.git#branch=main'`.

CI (`.github/workflows/ci.yml`) runs buf lint/format (+breaking on PRs), golangci-lint, `go test -race`, `docker compose config`, and a per-service Docker build matrix (build only, no push). Tool versions come from `.versions.env`. When you add a Makefile target, update this section.

Compose lives in `deploy/compose/`; `docker compose` commands need `-f deploy/compose/docker-compose.yml` (the Makefile adds it). `docker compose down -v` wipes both the sumpnet and chirpstack databases — they share one Postgres.

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

Registered in `~/.claude/PORTS.md` under `### sumpnet` and set in `deploy/compose/.env` (`.env.example` has the same values):

| Service | Host port | Env var |
|---|---|---|
| ChirpStack web UI + gRPC API | 3131 | `CHIRPSTACK_PORT` |
| Mosquitto (sumpnet's own broker) | 3133 | `MQTT_PORT` |
| api-gateway | 3134 | `API_PORT` |
| Web dashboard (Phase 5) | 3034 | `WEB_PORT` |
| Postgres 16 + pg_partman | 5444 | `POSTGRES_PORT` |

Container-internal ports stay at defaults (8080, 1883, 5432). Host 1883 belongs to the unrelated home-assistant Mosquitto (where the existing Photon node publishes — `mqtt-bridge` subscribes to it in Phase 2); 3132 belongs to another project. 8xxx is blocked on the host, so never publish ChirpStack on 8080. HTTPS via `dev.ecoworks.ca:<port>` is deferred to Phase 5; Phase 0/1 endpoints are plain HTTP on localhost.
