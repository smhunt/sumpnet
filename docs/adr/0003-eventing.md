# 0003 — Service-to-service eventing: Postgres LISTEN/NOTIFY
Status: accepted · Date: 2026-09-11

## Context
cycle-detector, storm-analytics and alerts (Phases 3–4) must react to new
telemetry soon after ingest stores it. The stack already runs Postgres; Redis
is present but belongs to ChirpStack. The project is local-first with AWS as a
second target, operated by one person.

## Decision
`ingest` ends every committed batch with
`pg_notify('sumpnet_ingest', '{"table":…,"n":…,"min_ts":…,"max_ts":…}')`
(see `internal/store/bulk.go`). The payload is a wake-up hint — no device ids,
well under the 8000-byte limit.

Consumers `LISTEN` on a dedicated connection and, on each notification *and*
on a periodic timer (~30 s), read new rows by a persisted `inserted_at`
watermark. The poll is the source of truth; a missed notification costs
latency, never data.

## Consequences
- No new infrastructure, credentials or client libraries.
- At-least-once by construction; consumers must be idempotent (they are keyed
  on the same `(device_id, time, f_cnt)` rows).
- Notifications are not durable across consumer restarts; the watermark
  covers that.
- Couples eventing to the database (RDS supports NOTIFY). No fan-out to
  systems outside Postgres — if that is ever needed, a consumer republishes.

## Alternatives considered
- **NATS JetStream** — the right choice if consumers multiply or need replay;
  adds a stateful service to run and secure. Revisit at Phase 8 scale.
- **SQS/SNS** — AWS-only; breaks the local-first rule.
- **Redis Streams** — the instance is ChirpStack's, with its persistence settings.
- **Kafka** — far more than one neighbourhood needs.
