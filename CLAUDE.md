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
make proto   # buf generate → gen/go (committed; CI fails if stale — see ADR 0002)
make up      # docker compose up -d --build --wait (blocks until every healthcheck passes)
make down / make ps / make logs S=<service>
make env     # copies deploy/compose/.env.example → .env if missing (make up does this)
make test-integration                       # -tags integration; testcontainers, needs Docker
make sim SCENARIO=storm50 SEED=42 SPEED=60  # replay into the stack's Mosquitto; truth → loadtest/results/
make migrate-up / migrate-down / migrate-new NAME=x   # golang-migrate against the compose DB (from .env)
make sqlc                                    # regenerate internal/store/sqlcgen (committed; CI runs sqlc diff)
make db-shell                                # psql into the compose Postgres
make alerts-testmail                         # one test email through the SMTP provider in .env (Resend)
```

Alerts gRPC: `docker run --rm --network host fullstorydev/grpcurl:v1.9.3 -plaintext -d '{"segment_id":"seg-01"}' localhost:3135 sumpnet.alerts.v1.AlertService/ListActiveAlerts` (reflection is on).

The simulator is a CLI: in compose it lives behind `COMPOSE_PROFILES=sim` and exits after one replay; `make up` does not start it.

Single test: `go test -race -run TestName ./internal/sim/...`. `internal/sim` takes ~40 s under `-race` (integer-heavy loop); iterate with plain `go test ./internal/sim/` (<1 s) and let `make test` do the race run.

Simulator CLI: `go run ./cmd/simulator -list-scenarios`; `-sink stdout -hash` writes JSONL to stdout and the SHA-256 of the event stream to stderr (logs also go to stderr, so stdout stays pure JSONL). Same seed ⇒ byte-identical output; never introduce `time.Now()`, `uuid.New()` or map iteration into `internal/sim`'s emission path.

**Integration tests from Claude's Bash tool:** testcontainers calls the Docker credential helper (`osxkeychain`), which hangs in the sandboxed shell waiting on Keychain. Run them as `DOCKER_CONFIG=<dir containing an empty config.json> go test -tags integration ./...` (public images need no auth). From a normal terminal `make test-integration` works as-is.

Breaking-change check locally: `./bin/buf breaking --against '.git#branch=main'`.

CI (`.github/workflows/ci.yml`) runs buf lint/format (+breaking on PRs), golangci-lint, `go test -race`, `docker compose config`, and a per-service Docker build matrix (build only, no push). Tool versions come from `.versions.env`. When you add a Makefile target, update this section.

Compose lives in `deploy/compose/`; `docker compose` commands need `-f deploy/compose/docker-compose.yml` (the Makefile adds it). `docker compose down -v` wipes both the sumpnet and chirpstack databases — they share one Postgres. A one-shot `migrate` service applies `migrations/` before `ingest` starts; `ingest` also refuses to start on a stale schema.

## Ingest path invariants

- Every telemetry table's PRIMARY KEY is `(device_id, <event time>, f_cnt)`; that key is the idempotency contract and includes the partition column. Bridges store the **event** time (ChirpStack `time` / envelope `t`), never receive time (a counted fallback only).
- `internal/store` bulk inserts go COPY → temp staging table → `INSERT … ON CONFLICT DO NOTHING`; the Go column lists in `rows.go` must match the schema (an integration test checks). New months are created on demand for replays.
- A bridge acknowledges an MQTT message only after ingest confirms the batch; a flush failure is fatal so the broker redelivers. Poison messages (undecodable) are acknowledged and counted in `sumpnet_bridge_drops_total{reason}`.
- Unknown DevEUIs are auto-registered with `home_id NULL` (invisible to owner views/aggregates) unless `INGEST_AUTO_REGISTER=false`.

## Consumer invariants (cycle-detector, alerts, weather, storm-analytics — ADR 0003)

- Consumers are `internal/watermark` stages: `LISTEN sumpnet_ingest` is only a hint; the poll by `inserted_at` watermark is the truth, and poll + writes + watermark commit together. `WATERMARK_LAG` must exceed the longest ingest transaction.
- Every alert timestamp (`raised_at`, `resolved_at`, detection `observed_at`) is **event time**, never wall clock; the only wall-clock rule is the OFFLINE sweep (`ALERTS_OFFLINE_AFTER=0` disables it — required for replays and tests).
- The alerts service is the single writer of `alerts`. One open row per (device, code) (partial unique index); a new episode is a new row; `raised_at` is the earliest trigger across sources; a row from a lagging source cannot resolve a newer episode.
- `cycle-detector` → `alerts` goes through the `detections` table (idempotent PK), never in-process.
- §10 rules live in `internal/hydrology` (pure); storm-mode summaries carry the short-cycling test in aggregate (`SummaryShortCycling`).
- Email: real provider SMTP from `.env` (`SMTP_HOST/PORT/USER/PASSWORD/FROM`, `ALERTS_TO`); empty `SMTP_HOST` = log only. The provider is **Resend** over SMTP (`smtp.resend.com`, port 465, user `resend`, password = Resend API key, `SMTP_FROM` on a Resend-verified domain). `SMTP_PASSWORD` is an API key — never commit `.env`, never paste it into chat. `make alerts-testmail` sends one delivery check. Tests use Mailpit via testcontainers, never real mail.

## Weather and storm analytics invariants (ADR 0007)

- fPort 5 uplinks are stored raw in `rain_gauge_uplinks`. `weather` is the single writer of `rainfall`: gauge rows from consecutive tip-count deltas (re-derived for the stretch an uplink touches; tips after an interval longer than the 5-min wake are placed in its last 5 min), ECCC rows from GeoMet (`rainfall.ts` = `UTC_DATE` − 1 h: the amount is for the hour ending at `UTC_DATE`). Writers bump `updated_at` only on a real change; `storm-analytics` polls `rainfall` by `updated_at`.
- `storm-analytics` is the single writer of `storm_events` and `home_storm_metrics`. It keeps no state between polls: every stage re-segments the rain around the change, matches existing storms by onset-inside-storm (ids stay stable), recomputes affected homes, writes only changed rows and notifies `sumpnet_ingest` with table `storm_events`.
- Own gauges are primary; an ECCC hour is used only where no gauge interval overlaps it. `WEATHER_ECCC_ENABLED=false` for replays and tests; tests never call api.weather.gc.ca (recorded pages in `internal/weather/testdata`).
- §10 storm/baseflow/lag/recession live in `internal/hydrology` (pure). Acceptance compares `HomeStormTruth.lag_min` / `recession_min` (continuous truth); `lag_min_discrete` saturates below 24 cycles/day and is not a usable truth.
- `sim.Config.RainGauges` defaults to 0 so existing stream hashes hold; the CLI (`-rain-gauges`) and `testpipeline.NewSim` use 2. Gauge events have `HomeIndex = -1` and `Kind = "rain"`.
- OUTAGE_RISK rain term: rain recorded in the 6 h before the reading; rainfall data that do not reach within 30 min of it count as rain (a dead feed never hides the alert).

## Privacy invariants (ADR 0005)

- Per-home data is served only on owner-scoped paths: the caller's Clerk subject must be linked in `home_owners`. Public views aggregate to segments through `internal/privacy` only (`MinHomes = 3`; suppressed aggregates zero every number, including live cycle rate and alert count). Unlinked devices never count as reporting homes.

## Architecture

```
LoRa house/rain nodes -> LoRaWAN gateways -> ChirpStack v4 --MQTT--> lora-bridge --┐
ESP32 Wi-Fi nodes (dev) -----------------------> Mosquitto --> mqtt-bridge ---------┼--gRPC stream--> ingest -> Postgres 16
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

- **Payload decoding happens only in Go (`internal/codec`)**, never in ChirpStack JS codecs. Uplinks are binary and little-endian: fPort 1 heartbeat (10 B), 2 pump cycle (11 B), 3 alarm (3 B, confirmed), 4 storm-mode summary, 5 rain gauge (11 B, cumulative tip counter). The exact layouts are in §5 of the plan. Firmware (`firmware/`, PlatformIO on ESP32-S3 + SX1262) must encode byte-for-byte what the codec decodes, over LoRaWAN or the Wi-Fi/MQTT envelope in `docs/node-mqtt.md`. Keep golden byte-vector table tests as the shared contract, and change them together with firmware.
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
| alerts gRPC (no auth until Phase 5) | 3135 | `ALERTS_GRPC_PORT` |

Container-internal ports stay at defaults (8080, 1883, 5432). Host 1883 belongs to the unrelated home-assistant Mosquitto (pool equipment; nothing sumpnet-related publishes there); 3132 belongs to another project. 8xxx is blocked on the host, so never publish ChirpStack on 8080. HTTPS via `dev.ecoworks.ca:<port>` is deferred to Phase 5; Phase 0/1 endpoints are plain HTTP on localhost.
