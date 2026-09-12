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

## Addendum (Phase 3, 2026-09-11): how consumers actually poll

`internal/watermark` implements this ADR. Three details matter:

- `inserted_at` is `DEFAULT now()` = transaction start, so all rows of one ingest
  batch share a value and become visible at commit, *later* than that value.
  A poll therefore excludes rows younger than `WATERMARK_LAG` (5 s by default;
  it must exceed the longest ingest transaction — watch
  `sumpnet_ingest_batch_seconds`) and never splits an `inserted_at` group.
- Poll, handler writes and the watermark update commit in one transaction, so
  side effects are effectively exactly-once; a rollback discards in-memory
  handler state (`Reset`).
- Sources are consumed at independent watermark positions, so one episode can
  be observed out of event-time order across tables (a node alarm before the
  heartbeat that confirms it, or after). The alerts engine keeps `raised_at`
  as the earliest trigger and refuses to resolve an alert with a row older than
  its raise.
