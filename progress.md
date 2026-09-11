# progress

## Status
Phase: **0 — Scaffold** (local acceptance passed 2026-09-11; awaiting first green CI run)

Next phase: 1 — Contracts + simulator (do not start until Phase 0 acceptance below passes).

## Session log (newest first)

### 2026-09-11
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
- [ ] First push to `smhunt/sumpnet` green on all CI jobs
