# 0008 — Real-geography sites from County of Middlesex open data
Status: accepted · Date: 2026-09-13

## Context
Until now the simulator placed 60 invented homes on 8 illustrative polygons about 3 km from
Timberwalk. The owner wants the pilot's actual streets and houses, and the storms that actually fell
this summer, so replays line up with the `rainfall` rows the live weather service stores. The County
of Middlesex open data portal publishes address points and a single-line road network, but its
licence is unconfirmed: the portal item has no licence text and the hub's "Terms of Use" label is
unlinked. ADR 0005 requires that per-home data is never publicly attributable, and a simulation of
real addresses must not become a fake record of a real house.

## Decision
- **Committed site configs, uncommitted data.** `internal/site/sites/<name>.json` lists a site's
  streets (address `FULLSTREET`, road `FULLNAME` when spelled differently, label, illustrative segment
  kind), municipality and subdivision plans; `extends` nests larger sets. The default `timberwalk`
  site is the owner's choice (plan 39T-MC0401 plus Timberwalk Close: 7 streets, 190 addresses).
  `make site-import` caches each street's raw ArcGIS pages under `data/cache/middlesex/` and writes
  a normalised snapshot to `data/sites/<site>.json`, with source URLs, queries, fetch times, the
  attribution line and a licence note. `data/` is gitignored; tests use synthetic fixtures.
- **Segments.** One per street; a street with more than 40 homes is split into contiguous blocks
  by position along its chained centreline, with each boundary halfway between neighbouring homes.
  IDs are `<street-slug>-<block>`. Each outline is built on a 2 m raster:
  - A cell counts if it is within 16 m of the block's centreline or of a connector from one of its
    homes to the centreline.
  - Each cell goes to the nearest segment, so outlines never overlap.
  - The largest 4-connected region, with holes filled, is traced and simplified (1.5 m) into a
    simple, counter-clockwise GeoJSON Polygon.

  All of this is pure Go.
- **Identity.** A home's id (UUIDv5 of the digest), DevEUI (`5e` + 14 hex, locally administered),
  DevAddr and PCG stream come from HMAC-SHA256 of its address. The HMAC key is a random 32-byte
  salt kept with the cache (`identity-salt.hex`) and copied into the snapshot.
  - They are stable for an address across sites and re-imports that share the salt.
  - They cannot be recomputed from the public address list.
  - The stream makes a home's simulated parameters independent of which other streets are in the site.
  - Homes are ordered by id, so an index or device name says nothing about position.
- **Simulator.** `sim.Config.Site` replaces the synthetic neighbourhood. House coordinates are used
  only to put the two rain gauges at the ends of the site's longer axis and are never emitted. A
  scenario with pump health overrides is refused on a site. Without a site, every stream hash is
  unchanged.
- **Observed rain.** `make eccc-import` stores ECCC LONDON CS hourly amounts and their fetch windows
  in `data/rain/`.
  - `-scenario eccc -from -to` spreads each hour's amount evenly over its 60 minutes, which is how
    `rainfall` stores an ECCC hour.
  - The virtual clock starts at `-from`.
  - An uncovered window is refused, and the simulator never fetches.
- **Seed.** `make seed SITE=…` writes each segment (outline, kind) and one home and linked device per
  address, with the simulator's ids. The owner link needs `OWNER_ADDRESS`, which is matched in memory
  and never written; there is no index fallback onto a real house. Nothing written to the database,
  protos, REST or the dashboard carries an address or a house position.

## Consequences
- A new cache (new salt) changes every site home id and DevEUI, so re-seed after re-importing from
  scratch; keep the salt with the cache.
- The outlines are derived from County data. Committing snapshots or publishing maps waits on the
  licence question in `prompt_plan.md` §14 and needs the attribution line.
- Short streets matter for privacy: Songbird Lane has 3 homes and Timberwalk Close 5, so they go dark
  under k ≥ 3 as soon as fewer homes report.
- Real hourly rain has drizzle before the first ≥ 1 mm/h hour, so the truth lag is often 0 min on
  observed storms. Heavy hours push some simulated homes past pump capacity, and healthy simulated
  pumps then raise `continuous_run` and `float_high`. These are synthetic, owner-scoped alerts, not
  statements about the real house.
- Seeding a site into a database that already holds the illustrative `seg-01`…`seg-08` keeps both, and
  the map then fits both sets.
- The compose `sim` profile has no `data/` mount; site and observed-rain replays run with `make sim`
  from the host.

## Alternatives considered
- **Commit the snapshot or outlines:** blocked on the licence.
- **Unsalted address hash for ids:** anyone with the public address list could map ids back to houses.
- **Convex hulls or offset curves:** hulls of crescents overlap their neighbours, and offset curves
  self-intersect on tight bends and ignore where the houses are.
- **Parcel polygons:** not in the layers probed, and finer than the segment-level rule needs.
- **Hourly steps as hyetograph breakpoints:** per-minute sampling puts each boundary minute in the
  wrong hour; an explicit per-minute series is exact.
- **Shaped within-hour profiles:** they would invent structure the hourly data do not have.
