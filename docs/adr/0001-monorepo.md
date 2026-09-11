# 0001 — Single Go module monorepo
Status: accepted · Date: 2026-09-11

## Context
sumpnet is ten small Go services (`prompt_plan.md` §3) that share a payload
codec, domain types, storage layer and process skeleton. The first goal of the
project is reviewability as a portfolio piece; the second is a working civic
tool for one neighbourhood. Neither needs organisation-scale independence
between services.

## Decision
One repository, one `go.mod` (`github.com/smhunt/sumpnet`):

- `cmd/<service>/main.go` per binary, each a thin wrapper around `internal/platform.Run`.
- Shared code under `internal/` so Go's visibility rules enforce that only this
  module can import it.
- One parameterised `Dockerfile` (`--build-arg SERVICE=<name>`), one compose
  stack (`deploy/compose/`), one CI workflow.

## Consequences
- Cross-service changes (a proto field, a codec fix) land atomically in one commit.
- One `go test -race ./...`, one `go.sum`, one Go version for everything.
- Every CI run builds every image. Mitigated by per-service GHA layer caches;
  can collapse to a single `docker bake` job if minutes become a problem.
- A Go version bump applies to all services at once. Acceptable at this size.

## Alternatives considered
- **Multi-module workspace (`go.work`)** — deferred. Worth it only when a
  package needs its own release cadence (e.g. publishing the codec for firmware
  tooling). Splitting later is mechanical.
- **Polyrepo** — rejected. Ten repos for a solo project multiplies CI, dependency
  updates and review overhead without a consumer that needs the isolation.
