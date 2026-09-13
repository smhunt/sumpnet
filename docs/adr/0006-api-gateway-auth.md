# 0006 — api-gateway: Clerk owner auth, one gRPC surface, private neighbourhood stream
Status: accepted · Date: 2026-09-12

## Context
Phase 5 puts sumpnet in front of people: a public map of how streets handle
water, and each owner's own home and alerts. The owner chose Clerk for sign-in
(§14). ADR 0005 fixes the privacy rule (per-home data owner-scoped; public
segment aggregates only with at least 3 homes). Phase 3 left `AlertService`
unauthenticated on a compose-internal port for the gateway to front. The same
QueryService must serve the dashboard (REST, a browser) and the Phase 6 MCP
server (gRPC).

## Decision
**Authentication.** The gateway verifies Clerk session tokens itself:
RS256 only, signed by a key in the issuer's JWKS, `iss` = `CLERK_ISSUER`,
`exp` required, `nbf`/`iat` checked with a 5 s leeway, `sub` present and, when
`CLERK_AUTHORIZED_PARTIES` is set, `azp` one of them (a token minted for
another site is refused). Libraries: `golang-jwt/jwt/v5` (the de-facto Go JWT
parser) with `MicahParks/keyfunc/v3` (JWKS cache, hourly refresh, a
rate-limited refresh when an unknown `kid` appears after key rotation). Both
are small and single-purpose; Clerk's Go SDK would add its Backend API client
and session-management surface the gateway does not use. Tests serve a JWKS
from an in-test RSA key (`internal/auth/authtest`) and never call Clerk.

No `authorization` header means an anonymous caller; a header that is not a
valid bearer token is `UNAUTHENTICATED` on every RPC, public ones included, so
a misconfigured client fails loudly instead of silently downgrading. The
subject maps to homes through `home_owners`. A home or alert the caller does
not own is `NOT_FOUND`, indistinguishable from one that does not exist. An
empty `CLERK_ISSUER` disables owner RPCs and keeps public views working.

**One surface for both transports.** REST is grpc-gateway proxying to the
gateway's own gRPC listener over loopback, not the in-process handler: the
in-process transport cannot stream, and the loopback hop means interceptors,
ownership checks and privacy code run once, identically, for REST and gRPC.
grpc-gateway forwards `Authorization` as `authorization` metadata.

**Writes stay with their owners.** The gateway's Postgres pool sets
`default_transaction_read_only=on`; it cannot write. Acknowledging an alert is
an ownership check in the gateway followed by `AlertService.Acknowledge`, the
single writer of `alerts` (new additive RPCs on QueryService; alerts.proto is
unchanged).

**Transport.** The REST listener (`REST_ADDR`) optionally serves TLS
(`TLS_CERT_FILE`/`TLS_KEY_FILE`, the shared mkcert certificate in compose, so
the dashboard can call `https://dev.ecoworks.ca:3134`); the platform ops
listener moves to `HTTP_ADDR=:8081` and stays plain HTTP for the container
healthcheck. `TLS_FALLBACK_HTTP` (set in compose) serves plain HTTP when the
certificate is absent. CORS is an exact-origin allowlist
(`CORS_ALLOWED_ORIGINS`); the Vite dev server proxies `/v1` same-origin.

**WatchNeighbourhood.** One hub per gateway, the ADR 0003 pattern: `LISTEN
sumpnet_ingest` (and storm-analytics' `storm_events` notifications) is a hint,
debounced; a ticker is the safety net; each round recomputes the whole
neighbourhood in one read-only repeatable-read transaction.
- Segment status covers a window (`STATUS_WINDOW`, 1 h) ending at the
  **neighbourhood clock**: the newest event time from a linked house device,
  clamped to the wall clock. Live data behaves as "the last hour"; a replay of
  a past storm animates the map as it is ingested. Status numbers come only
  from `privacy.AggregateStatus` (reporting homes = distinct linked homes with
  a heartbeat in the window; cycles = the heartbeats' `cycles_since_last`).
  Only changed segments are sent.
- Storm events and alerts are sent when their row version changes
  (`updated_at`, re-read with a 1-minute overlap and de-duplicated by version).
- Routing is the privacy rule: statuses and storm events go to everyone;
  an alert goes only to subscribers whose Clerk subject owns the alert's home
  in `home_owners` at send time. Anonymous subscribers never receive one.
- A subscriber whose bounded queue fills is disconnected
  (`RESOURCE_EXHAUSTED`) and reconnects with a snapshot, so one slow browser
  cannot stall the hub.

## Consequences
- Revoking an owner link stops alert delivery on the next change; an open
  stream was authorised at connect time, so a sign-out takes effect when the
  client reconnects (the dashboard reopens the stream on identity change).
- Clerk configuration is three values in `.env` plus the publishable key in
  `web/.env.local`; nothing secret is needed by the gateway (JWKS is public).
- The neighbourhood clock means a street whose sensors all go silent keeps
  its last window while any other linked device still reports; per-home
  offline alerts (Phase 3) cover the silent homes.
- `SegmentActivity` scans the readings in the window per round; fine for the
  pilot, and a `readings (ts)` index or a rollup is the Phase 8 lever.
- Per-owner email, magic links and roles (operator vs owner) remain open.

## Alternatives considered
- **Clerk Go SDK** — official, but a larger dependency for the one thing needed
  (JWT verification against JWKS).
- **Hand-rolled JWKS cache** — ~150 lines of key parsing, caching and rotation
  handling that keyfunc already does and tests.
- **Cookie sessions / magic links** — the owner chose Clerk; bearer tokens also
  suit the MCP client.
- **Enforcing ownership in SQL row-level security** — hides the rule from Go
  tests and still needs the subject in every session.
- **Per-subscriber database polling** — N subscribers would mean N polls per
  tick; one hub computes once and fans out.
