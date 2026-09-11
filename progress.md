# progress

## Status
Phase: **0 — Scaffold** — DONE 2026-09-11 (all acceptance criteria met, CI run 34634272184 green)

Phase: **1 — Contracts + simulator** — DONE 2026-09-11 (acceptance below; PR #2 awaiting merge)

Next phase: **2 — Ingest path** (lora-bridge, mqtt-bridge, ingest, migrations, sqlc). Do not start until PR #2 is merged.

## Session log (newest first)

### 2026-09-11 (Phase 1, branch `phase-1/contracts`, PR #2)
- Protos: `telemetry/v1` (IngestService: SubmitReadings, SubmitCycleEvents
  incl. storm summaries, SubmitAlarms), `query/v1` (QueryService with
  grpc-gateway GETs, `WatchNeighbourhoodResponse` wrapper), `alerts/v1`.
  Generated code committed under `gen/go` with a CI drift check (ADR 0002).
- `internal/codec`: strict little-endian encode/decode for fPorts 1–4 with
  hand-derived golden vectors, round-trip property test and fuzzing; fPort 4
  layout fixed at 9 bytes (count, window_s, total_run_s, max_peak_current_da,
  min_level_mm). All ports ≤ 11 B (US915 DR0 limit).
- `internal/chirpstack`: topic helpers and `UplinkEvent` JSON via the vendor's
  own proto types + protojson (compacted for byte-stable streams).
- `internal/sim`: deterministic engine (PCG per home, integer hydrology,
  virtual clock), emulated node firmware (heartbeats, cycles, alarms with
  hold-off, storm mode), six scenarios, ground-truth export with §10
  lag/recession applied literally, hash/JSONL/MQTT sinks.
- `cmd/simulator` CLI; `make sim`, `make test-integration`; README.
- Model calibration notes: response 60–160 L/mm and reservoir 30–300 min
  were needed for storm mode to be exercised by a representative share of
  homes (9/24 in tests); short-cycling is modelled as a float fault (20 mm
  hysteresis) plus 85% check-valve backflow; a failing pump decays to zero
  output by 90% of the scenario so continuous-run/float-high are reachable.
- Test-time note: `internal/sim` takes ~40 s under `-race` (was 151 s before
  removing a per-step map write and memoising runs); 0.7 s without.
- Gotcha: testcontainers hangs from Claude's sandboxed shell because the
  `osxkeychain` credential helper never returns; `DOCKER_CONFIG` pointing at
  an empty config.json works. Documented in CLAUDE.md.

#### Phase 1 acceptance
- [x] Two unpaced runs `-scenario storm50 -seed 42 -homes 60 -sink stdout`:
  byte-identical JSONL (13,795 events, 15 MB) and identical truth JSON;
  stream hash `4d88f34ae1956fcccc2bcdfcf187241a4997f015319de48a4aaba3b57c0b4a24`.
- [x] `make sim SCENARIO=storm50 SEED=42 SPEED=60` into the compose Mosquitto:
  13,795 events, completed in 23 min 59 s real (24 h virtual), hash
  `4d88f34ae1956fcccc2bcdfcf187241a4997f015319de48a4aaba3b57c0b4a24` — identical
  to the stdout runs, so the hash is sink- and pacing-independent.
- [x] Integration test: 979 events from 5 devices received in order.

### 2026-09-11 (Phase 0)
- Decided with owner: dev on the Mac (lab VM later), own Mosquitto in compose,
  private GitHub repo `smhunt/sumpnet`, module path `github.com/smhunt/sumpnet`.
- Registered host ports in `~/.claude/PORTS.md`: ChirpStack 3131, MQTT 3133,
  api-gateway 3134, web 3034, Postgres 5444.
- Done: go.mod, Makefile + `.versions.env` tool pins, `internal/platform`
  (config, slog JSON, /healthz /readyz /metrics, graceful shutdown,
  `healthcheck` subcommand), ten placeholder services, golangci v2 config,
  buf config + placeholder proto, Dockerfile (distroless, `ARG SERVICE`),
  compose stack (Postgres 16 + pg_partman, Redis, Mosquitto, ChirpStack v4
  US915), CI workflow, ADR 0001.
- Fixed during `make up`: initdb script needed `#!/bin/sh` + exec bit (Alpine image,
  no bash) and the pg_partman BGW needs `pg_partman_bgw.role=sumpnet` since the
  superuser is not `postgres`. Image is `postgres 16.14` + `pg_partman 5.2.4`.
- Deferred: HTTPS on `dev.ecoworks.ca:<port>` for the ChirpStack UI and ops
  endpoints — Phase 0 acceptance is plain TCP; wire Traefik/TLS in Phase 5.
- Spec amendments agreed for Phase 1 (see plan): `WatchNeighbourhoodResponse`
  wrapper, `SubmitAlarms` RPC, ADR renumbering (0002 generated code,
  0003 eventing), explicit `go_package` (no buf managed mode), rain-gauge fPort
  is a new §14 open question.

## Phase 0 acceptance checklist
- [x] `make lint && make test` clean
- [x] `make up` exits 0; `make ps` shows postgres, redis, mosquitto, chirpstack and all 10 Go services healthy
- [x] `curl -s localhost:3134/readyz` → 200; `/metrics` contains `sumpnet_build_info`
- [x] ChirpStack UI on http://localhost:3131 (admin/admin); `pg_partman` in sumpnet DB; `pg_trgm`/`hstore` in chirpstack DB
- [x] `make down && make up` idempotent
- [x] First push to `smhunt/sumpnet` green on all CI jobs (15/15, https://github.com/smhunt/sumpnet/actions/runs/34634272184)
