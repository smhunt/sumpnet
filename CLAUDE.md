# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project status

sumpnet is a neighbourhood sump-pump and drainage monitoring platform (pilot: Timberwalk, Ilderton, ON). It has two goals: a portfolio showcase for senior Go backend work (gRPC, microservices, AWS, CI/CD, published load-test numbers) and a real civic tool.

`prompt_plan.md` is the source of truth for architecture, payload formats, data model, analytics definitions, and the phased roadmap. Read the relevant section of it before starting work, and keep it in sync if a decision changes. `progress.md` records what each session did; its Status table is the current state of every phase. Update it before ending a session. Module path is `github.com/smhunt/sumpnet` (GitHub account `smhunt`, private repo).

## Workflow rules from the plan

- Work proceeds in phases (§12 of `prompt_plan.md`). **Don't start a phase until the previous phase's acceptance criteria pass.** Tick the checkboxes in `prompt_plan.md` as items land.
- Update `progress.md` at the end of every session: the Status table plus a session entry, newest first. Add user-visible changes to `CHANGELOG.md` and to the in-app copy in `web/src/about.ts` (changelog and roadmap).
- Record significant decisions as ADRs in `docs/adr/` (0001–0008 so far) and add each one to `docs/adr/README.md`.
- Resolve open questions in §14 with the user; don't pick an answer silently (still open: gateway sites, simulator calibration before public numbers, licence, the County of Middlesex data licence, whether real-street segment kinds need a measured basis, per-owner alert email, and the bucket-test proposals in `docs/research/pump-flow-bucket-test.md`, chiefly whether a bucket test calibrates pit area instead of overriding the pump rate).
- When services, tables, ports or routes change, update the maps together. The Mermaid system diagram is identical in `README.md` and `docs/README.md`. The ASCII diagram below matches `prompt_plan.md` §3. The data-flow and ER diagrams live in `docs/README.md`. The repository layout is in `docs/README.md` and `prompt_plan.md` §6. The ports tables are in `README.md`, `docs/README.md` and the Ports section below.

## Commands

Go is installed via Homebrew (`/opt/homebrew/bin/go`). Dev tools are pinned in `.versions.env` and installed into the gitignored `./bin` by `make tools`; the Makefile and CI both read those pins, so never call a globally installed `buf`/`golangci-lint`.

```bash
make tools                  # one-time: buf, golangci-lint, sqlc, migrate into ./bin at pinned versions
make lint                   # buf lint + buf format --diff + golangci-lint run (plain and -tags integration) + golangci-lint fmt --diff
make fmt                    # buf format -w + golangci-lint fmt
make test                   # go test -race -count=1 -cover ./...
make test-integration       # go test -race -count=1 -tags integration ./...; testcontainers, needs Docker
make all                    # lint + test
make proto                  # buf generate → gen/go (committed; CI fails if stale — see ADR 0002)
make sqlc                   # regenerate internal/store/sqlcgen (committed; CI runs sqlc diff)
make build                  # docker compose build: every service image
make up                     # docker compose up -d --build --wait (blocks until every healthcheck passes)
make down / make ps / make logs S=<service>
make env                    # copies deploy/compose/.env.example → .env if missing (up, build, sim, seed, db and testmail targets do this)
make clean                  # rm -rf ./bin gen — gen/ is committed, so restore it with make proto or git checkout
make seed [SEED=42 HOMES=60 DEMO_OWNER_SUBJECT=user_...]   # illustrative segments + the simulator's homes/devices; optional Clerk owner link to home 0
make sim SCENARIO=storm50 SEED=42 SPEED=60 HOMES=60        # replay into the stack's Mosquitto; truth → loadtest/results/
make site-import SITE=timberwalk [REFRESH=1 | OFFLINE=1] [LOCATE="<number> <STREET>"]   # County address points + road centrelines → gitignored data/cache/middlesex/ and data/sites/<site>.json
make eccc-import FROM=2026-08-01 TO=2026-09-12             # ECCC LONDON CS hourly rain → gitignored data/rain/ (dates are UTC midnight)
make seed SITE=timberwalk [OWNER_ADDRESS="<number> <STREET>" DEMO_OWNER_SUBJECT=user_...]   # the site's real segments + one home/device per address
make sim SCENARIO=eccc SITE=timberwalk FROM=2026-08-01 TO=2026-09-12 SPEED=0   # observed rain over the site; virtual start = FROM
make migrate-up / migrate-down / migrate-new NAME=x        # golang-migrate against the compose DB (from .env); migrate-down steps back one
make db-shell               # psql into the compose Postgres
make alerts-testmail        # one test email through the SMTP provider in .env (Resend)
make web-install / web-dev / web-test / web-build   # web/: npm ci, Vite on https://dev.ecoworks.ca:3034, eslint + tsc + vitest, production build
```

Run `make seed` before `make sim`: devices the seed has not linked stay unlinked and never count in owner views or public aggregates. Set `WEATHER_ECCC_ENABLED=false` in `.env` for replays.

Dashboard: `cp web/.env.example web/.env.local` (Clerk publishable key, `API_PROXY_TARGET`); the Vite dev server proxies `/v1` to the gateway. `web/go.mod` exists only so `go ./...` never walks `node_modules`.

api-gateway env (compose sets them): `REST_ADDR=:8080` (REST + ops, TLS via `TLS_CERT_FILE`/`TLS_KEY_FILE`, `TLS_FALLBACK_HTTP=true` serves HTTP when the mkcert cert is absent), `HTTP_ADDR=:8081` (ops only, container healthcheck), `GRPC_ADDR=:9092`, `ALERTS_ADDR`, `CORS_ALLOWED_ORIGINS`, `CLERK_ISSUER`, `CLERK_JWKS_URL` (default `<issuer>/.well-known/jwks.json`), `CLERK_AUTHORIZED_PARTIES`, `CLERK_CLOCK_SKEW` (5s), `STATUS_WINDOW` (1h), `WATCH_POLL_INTERVAL`, `WATCH_DEBOUNCE`. Empty `CLERK_ISSUER` = public RPCs only. REST reference: `docs/README.md`.

Alerts gRPC: `docker run --rm --network host fullstorydev/grpcurl:v1.9.3 -plaintext -d '{"segment_id":"seg-01"}' localhost:3135 sumpnet.alerts.v1.AlertService/ListActiveAlerts` (reflection is on).

The simulator is a CLI: in compose it lives behind `COMPOSE_PROFILES=sim` and exits after one replay; `make up` does not start it.

Single test: `go test -race -run TestName ./internal/sim/...`. `internal/sim` takes ~40 s under `-race` (integer-heavy loop); iterate with plain `go test ./internal/sim/` (<1 s) and let `make test` do the race run.

Site and rain imports: `go run ./cmd/dataimport sites` lists the committed site configs; `-site-file` (simulator, seed) and `-scenario eccc -from -to` (simulator) read only the gitignored caches those imports write; the simulator and seed never fetch.

Simulator CLI: `go run ./cmd/simulator -list-scenarios`; `-sink stdout -hash` writes JSONL to stdout and the SHA-256 of the event stream to stderr (logs also go to stderr, so stdout stays pure JSONL). Same seed ⇒ byte-identical output; never introduce `time.Now()`, `uuid.New()` or map iteration into `internal/sim`'s emission path.

**Integration tests from Claude's Bash tool:** testcontainers calls the Docker credential helper (`osxkeychain`), which hangs in the sandboxed shell waiting on Keychain. Run them as `DOCKER_CONFIG=<dir containing an empty config.json> go test -tags integration ./...` (public images need no auth). From a normal terminal `make test-integration` works as-is.

Breaking-change check locally: `./bin/buf breaking --against '.git#branch=main'`.

CI (`.github/workflows/ci.yml`) jobs: `buf` (lint, format, breaking on PRs, `gen/` drift), `lint` (golangci-lint, formatters, `sqlc diff`), `test` (`go mod verify`, build, `go test -race`), `test-integration`, `web` (`npm ci`, lint, test, build on Node 24), `compose-config`, and `docker` (build matrix over the ten services, no push). Tool versions come from `.versions.env`. When you add a Makefile target or CI job, update this section.

Compose lives in `deploy/compose/`; `docker compose` commands need `-f deploy/compose/docker-compose.yml` (the Makefile adds it). `docker compose down -v` wipes both the sumpnet and chirpstack databases — they share one Postgres. A one-shot `migrate` service applies `migrations/` first; every service that opens the database checks the schema version at startup and refuses to run on a stale one.

## Operations notes (verified on the live stack, 2026-09-12)

- Demo: `make up`; `make seed SEED=42 HOMES=60`; `make sim SCENARIO=storm50-long SEED=42 HOMES=60 SPEED=0`; `make web-install && make web-dev`. Seed and sim need the same `SEED` and `HOMES`, or the simulated devices stay unlinked. Expect the simulated 50 mm storm plus any real ECCC storm the poller fetched.
- Replays raise an OFFLINE alert per device: the sweep is wall clock and compose hard-codes `ALERTS_OFFLINE_AFTER: 1h`. Expected; don't change it without asking.
- storm-analytics does not backfill columns a migration adds. Stop it, run `update consumer_watermarks set inserted_at='2000-01-01' where consumer='storm-analytics' and source='cycle_events'`, then start it. Rewinding only the `rainfall` watermark is not enough. Runbook: `docs/README.md`.
- Sim home 9 (~500 dry-weather cycles/day) never gets a baseflow; its NULL lag, recession and pump rate are by design.
- No Clerk values: the gateway logs `auth:false` and the dashboard shows public views only.
- Real streets (not yet run on the live stack): `make site-import SITE=timberwalk`; `make eccc-import FROM=2026-08-01 TO=2026-09-12`; `make seed SITE=timberwalk OWNER_ADDRESS="<number> <STREET>" DEMO_OWNER_SUBJECT=user_...`; `make sim SCENARIO=eccc SITE=timberwalk FROM=2026-08-01 TO=2026-09-12 SPEED=0`. A database that already holds `seg-01`…`seg-08` keeps them, and the map then fits both sets.

## Ingest path invariants

- Every telemetry table's PRIMARY KEY is `(device_id, <event time>, f_cnt)`; that key is the idempotency contract and includes the partition column. Bridges store the **event** time (ChirpStack `time` / envelope `t`), never receive time (a counted fallback only).
- `internal/store` bulk inserts go COPY → temp staging table → `INSERT … ON CONFLICT DO NOTHING`; the Go column lists in `rows.go` must match the schema (an integration test checks). New months are created on demand for replays.
- A bridge acknowledges an MQTT message only after ingest confirms the batch; a flush failure is fatal so the broker redelivers. Poison messages (undecodable) are acknowledged and counted in `sumpnet_bridge_drops_total{reason}`.
- Unknown DevEUIs are auto-registered with `home_id NULL` (invisible to owner views/aggregates) unless `INGEST_AUTO_REGISTER=false`.

## Consumer invariants (cycle-detector, alerts, weather, storm-analytics — ADR 0003)

- Consumers are `internal/watermark` stages: `LISTEN sumpnet_ingest` is only a hint; the poll by `inserted_at` watermark is the truth, and poll + writes + watermark commit together. `WATERMARK_LAG` must exceed the longest ingest transaction.
- Sources: cycle-detector ← `cycle_events`, `storm_summaries`; alerts ← `alarm_events`, `readings`, `detections`; weather ← `rain_gauge_uplinks`; storm-analytics ← `rainfall` (by `updated_at`), `readings`, `cycle_events`, `storm_summaries`. A writer whose rows another consumer polls calls `store.Notify` in the same transaction (detector → `detections`, weather → `rainfall`, storm-analytics → `storm_events`).
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
- `home_storm_metrics.volume_l` is the §9 pit-drop floor; `inflow_est_l` = calibrated pump rate (`pump_rate_lps`, pit area × median dry-weather drop ÷ run time) × run time; `pump_rate_source` is `dry_weather` (learned) or, later, `bucket_test` (measured, takes precedence); all three NULL without a pit area or calibration (migration 0006). storm-analytics never edits `query.sql` / `query.proto`.

## Privacy invariants (ADR 0005)

- Per-home data is served only on owner-scoped paths: the caller's Clerk subject must be linked in `home_owners`. Public views aggregate to segments through `internal/privacy` only (`MinHomes = 3`; suppressed aggregates zero every number, including live cycle rate and alert count). Unlinked devices never count as reporting homes.
- `MinHomes` is a constant; changing it is a policy change that needs a new ADR. The MCP server (Phase 6) reads through QueryService, never SQL, so it inherits these rules.

## Real-geography site invariants (ADR 0008)

- County of Middlesex data (address points, road centrelines) and everything derived from it live only under the gitignored `data/`: the raw query cache per street, site snapshots, segment GeoJSON and the identity salt. The licence is unconfirmed (§14), so never commit any of it; tests use synthetic fixtures only.
- The street lists are the committed configs `internal/site/sites/*.json` (`timberwalk` is the default; `extends` nests the larger cached sets). Change a site there, then `make site-import SITE=…`; cached streets need no network (`OFFLINE=1` proves it, `REFRESH=1` re-queries).
- Never put an address, a house position or the salt into the database, protos, REST, the dashboard, tests, fixtures, configs, docs, commit messages or PR text. The owner's address is passed only at seed time (`OWNER_ADDRESS`); examples use `"<number> <STREET>"`. A site owner link is never made by index.
- Site home ids, DevEUIs (`5e…`), DevAddrs and PCG streams are HMAC-SHA256 of the address keyed by `data/cache/middlesex/identity-salt.hex` (copied into each snapshot); a new salt changes every id, so re-seed. Homes are ordered by id, never by position. `internal/sim` gets ids, segment membership and coordinates for gauge placement only, and emits no coordinates.
- A scenario with `HealthOverrides` is refused on a site (no pump failure pinned on a real address). Without `Config.Site`, every stream hash is unchanged.
- `-scenario eccc` reads `data/rain/eccc-hourly-<station>.json`, refuses a window its fetches do not cover, spreads each hour evenly over its 60 minutes and starts the virtual clock at FROM.

## Gateway invariants (api-gateway, ADR 0006)

- The gateway's pool is read-only (`default_transaction_read_only`); alert acknowledgement is an ownership check plus `AlertService.Acknowledge`. Gateway SQL lives in `internal/store/queries/query.sql`.
- Owner RPCs take the Clerk subject from the auth interceptor only; a home or alert the caller does not own is `NOT_FOUND`. An `authorization` header that is not a valid token is `UNAUTHENTICATED` even on public RPCs. Tests use `internal/auth/authtest`, never real Clerk.
- JWKS (keyfunc/jwkset): hourly refresh plus at most one refresh a minute on an unknown `kid`. jwkset uses `RateLimitWaitMax` as the refresh request's own deadline, so keep it at the HTTP timeout (10 s, PR #8); a tiny value silently breaks key rotation.
- REST is grpc-gateway proxying to the gateway's own gRPC listener over loopback, so every rule runs once for both transports. Add RPCs to `query.proto` additively (`./bin/buf breaking --against '.git#branch=main'`).
- `WatchNeighbourhood`: statuses and storm events are public; an alert is routed only to subscribers owning its home (checked at send time). Segment status windows end at the neighbourhood clock (newest linked-device event time, clamped to now), so replays animate.
- The REST port (`REST_ADDR`, host 3134) serves TLS; the ops listener (`HTTP_ADDR=:8081`) stays plain HTTP for the container healthcheck. AlertService on host 3135 is unauthenticated; owners reach alerts only through the gateway.

## Architecture

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

- **Single Go module** (Go 1.26, `go.mod`). Binaries live in `cmd/<service>` (plus the `simulator` and `seed` CLIs); shared code lives in `internal/`. The package map is in `docs/README.md` (Repository layout).
- **Protos** live in `proto/sumpnet/{telemetry,query,alerts}/v1/` and are managed by `buf`. The three services are `IngestService` (client-streaming submits, fPorts 1–5), `QueryService` (public segment and storm reads, owner-scoped homes and alerts, the neighbourhood stream) and `AlertService`.
- **Eventing between services** is Postgres `LISTEN/NOTIFY` hints plus watermark polls, not NATS/SQS (ADR 0003).
- **The simulator is a first-class input.** `cmd/simulator` publishes *ChirpStack-shaped* MQTT uplink events, so the real bridge → ingest path gets exercised. Real nodes must flow through the identical pipeline. The simulator must be deterministic given a seed, and its per-home parameters are the ground truth that storm analytics is validated against (recession ±10 %; lag ±10 % or 15 min, whichever is larger — owner-approved 2026-09-12).
- **Local-first:** Docker Compose on the lab VM is the primary target (development currently runs on the Mac). AWS (Terraform, ECS Fargate, RDS) is a second deployment target, not a dependency.

## Invariants that span multiple components

- **Payload decoding happens only in Go (`internal/codec`)**, never in ChirpStack JS codecs. Uplinks are binary and little-endian: fPort 1 heartbeat (10 B), 2 pump cycle (11 B), 3 alarm (3 B, confirmed), 4 storm-mode summary (9 B), 5 rain gauge (11 B, cumulative tip counter). The exact layouts are in §5 of the plan. Firmware (`firmware/`, planned for Phase 7: PlatformIO on ESP32-S3 + SX1262) must encode byte-for-byte what the codec decodes, over LoRaWAN or the Wi-Fi/MQTT envelope in `docs/node-mqtt.md`. Keep golden byte-vector table tests as the shared contract, and change them together with firmware.
- **Ingest is idempotent** on `(device_id, ts, fcnt)`. It uses bounded channels for backpressure and batches inserts with `pgx.CopyFrom`.
- **Privacy:** data is opt-in, device IDs are pseudonymous, per-house data is visible only to its owner, and public views aggregate to street segments only when **≥ 3 homes** report. `internal/privacy` enforces this. The API gateway and MCP server must both go through it (MCP reads via QueryService).
- **Analytics are pure functions** in `internal/hydrology`, with thresholds defined in §10 of the plan (cycle, dry run, short cycling, continuous run, baseflow, storm event, lag, recession, segment load, outage risk). Use those definitions exactly. If you change one, change the plan too.
- `readings`, `cycle_events` and `rain_gauge_uplinks` are partitioned monthly (native partitioning + pg_partman).

## Go conventions

- Use only the stdlib plus small, well-known libraries. No heavy frameworks.
- `log/slog` JSON logging. Pass `context.Context` everywhere, and give every goroutine a cancellation path.
- Wrap errors with `fmt.Errorf("...: %w", err)`. Sentinel errors live in `internal/domain`.
- Libraries: `google.golang.org/grpc` + `grpc-gateway` v2, `github.com/eclipse/paho.golang` (MQTT v5), `pgx/v5` + `sqlc`, `golang-migrate` (SQL in `migrations/`), `golang-jwt/jwt/v5` + `MicahParks/keyfunc/v3` (Clerk tokens), `prometheus/client_golang`, `testcontainers-go`. Phase 6 will add `github.com/modelcontextprotocol/go-sdk` (not yet in `go.mod`).
- Every service exposes `/healthz`, `/readyz` and Prometheus `/metrics`, and shuts down gracefully on SIGTERM. The shared setup lives in `internal/platform`, which also provides the `healthcheck` subcommand used by the distroless containers.
- Tests are table-driven. Dockerfiles are multi-stage and produce distroless images.

## Hard constraints

- Canada uses the **US915** band plan with a 400 ms dwell time, so keep SF7–SF10 at 125 kHz. Storm mode (> 6 cycles in 15 min) rolls cycle events into fPort 4 summaries to save airtime.
- Don't propose sensors on municipal assets (pond, outlet, pump station) without Middlesex Centre approval.
- Current sensing must be non-invasive only (CT clamp on a plug-through splitter). Never modify pump wiring.
- Source hardware from Canadian suppliers (DigiKey Canada, BC Robotics).

## Ports

Registered in `~/.claude/PORTS.md` under `### sumpnet` and set in `deploy/compose/.env` (`.env.example` has the same values):

| Service | Host port | Container port | Env var | Protocol |
|---|---|---|---|---|
| Web dashboard (Vite dev server on the host) | 3034 | none | `WEB_PORT` (informational; Vite's port is set in `web/vite.config.ts`) | HTTPS, `https://dev.ecoworks.ca:3034` |
| api-gateway REST + NDJSON stream + ops | 3134 | 8080 | `API_PORT` | HTTPS, `https://dev.ecoworks.ca:3134` (HTTP without the cert) |
| alerts AlertService gRPC (no auth) | 3135 | 9091 | `ALERTS_GRPC_PORT` | gRPC plaintext |
| ChirpStack web UI + gRPC API | 3131 | 8080 | `CHIRPSTACK_PORT` | HTTP |
| Mosquitto (sumpnet's own broker) | 3133 | 1883 | `MQTT_PORT` | MQTT |
| Postgres 16 + pg_partman | 5444 | 5432 | `POSTGRES_PORT` | PostgreSQL |
| chirpstack-gateway-bridge (profile `gateways`, Phase 7) | 1700/udp | 1700/udp | `GW_UDP_PORT` | UDP |

Compose-internal only: ingest gRPC `:9090`, api-gateway gRPC `:9092`, every Go service's ops listener `:8080` (the gateway's is `:8081`), Redis. Host 1883 belongs to the unrelated home-assistant Mosquitto (pool equipment; nothing sumpnet-related publishes there); 3132 belongs to another project. 8xxx is blocked on the host, so never publish ChirpStack on 8080. The api-gateway REST port is HTTPS with the shared mkcert cert mounted from `~/Code/.traefik/certs`, and the Vite dashboard serves `https://dev.ecoworks.ca:3034` with the same cert; the other host ports stay plain HTTP/TCP. `GW_UDP_PORT` is not in PORTS.md; register a port there before publishing it.
