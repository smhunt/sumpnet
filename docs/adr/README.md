# Architecture Decision Records

Short MADR-style records, numbered in the order the decisions were made.

| ADR | Title | Status |
|---|---|---|
| [0001](0001-monorepo.md) | Single Go module monorepo | accepted |
| [0002](0002-generated-code.md) | Commit generated protobuf code under `gen/go` | accepted |
| [0003](0003-eventing.md) | Service-to-service eventing: Postgres LISTEN/NOTIFY | accepted |
| [0004](0004-partitioning.md) | Time-series storage: native partitioning + pg_partman | accepted |
| 0005 | Privacy thresholds: segment aggregation with k ≥ 3 | planned (Phase 4) |

## Template

```markdown
# NNNN — Title
Status: proposed | accepted | superseded by NNNN · Date: YYYY-MM-DD

## Context
## Decision
## Consequences
## Alternatives considered
```
