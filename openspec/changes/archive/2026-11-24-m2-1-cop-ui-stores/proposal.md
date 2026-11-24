# Proposal: m2-1-cop-ui-stores
Approved-by: Coby Leuschke (2026-11-24) sha256:b6270bef9f98
Architect: gemini (gemini-4-carbon, low)

## Track

`design`: introduces new capability specification `openspec/specs/cop-ui/spec.md` (3 `Requirement`s), defines the client-side durability/reconciliation contract across both SemEngine `output/websocket` planes (`WS #1` `bbops.telemetry.>` and `WS #2` `ENTITY_STATES` `kv-watch`) plus ADR-115 `POST /graph-gateway/graphql`, and locks the ownship `VehicleEntityState` vs. external COP contact (`#17`/`#21`) type boundary consumed by `#9` (`M2.2`), `#11`/`#12` (`M3`), and `#19` (`M4`) (#8 / Epic #7).

## Why

`M1.2` (`#6`) landed `internal/mavlink` (`internal/mavlink/tracker.go:41-84`) streaming `20 Hz` `TelemetryFrame`s on `bbops.telemetry.<sysid>`, discrete `TransitionEvent`s on `bbops.telemetry.events.<sysid>`, and constant 22-predicate `uxs.vehicle.v1` `VehicleSnapshot`s (`internal/uxsvocab/snapshot.go:41-85`) into `ENTITY_STATES` (`configs/bbops.json:254-315`), while `ui/src/lib/` still contains only `ui/src/lib/bootstrap.ts:1-8` and local placeholder state in `ui/src/routes/+page.svelte:8-10`. `#8` (`M2.1`) builds the reactive Svelte 5 Runes store layer, ADR-115 GraphQL client, and deterministic COP controller verbs before `#9` (`M2.2`) mounts the canvas and HUD.

## What Changes

1. **Latest-Wins Kinematic & Transition Event Store (`ui/src/lib/stores/telemetry.svelte.ts`, `WS #1`):**
   - Connect to `output/websocket` #1 (`bbops.telemetry.>`), overwrite per-`sysid` kinematic snapshots (`quat_wxyz`, `body_rates`, `ned_xyz`, `ned_vel`, `lat_lon_alt`, `has_global_pos`, `hdg_deg`, `timestamp`) in place without growing arrays or triggering `20 Hz` per-frame DOM `$state` thrash, append `bbops.telemetry.events.<sysid>` `TransitionEvent`s into a bounded `100`-entry ring buffer, and run `500 ms -> 30 s` exponential-backoff reconnect + `25 s` (`< 30 s`) application keepalive frames so `output/websocket`'s `60 s` `ReadMessage` deadline never drops receive-only clients (`1006`).
2. **Gapless SKG Applied-State Store & GraphQL Client (`ui/src/lib/stores/skg.svelte.ts`, `ui/src/lib/graphql.ts`, `WS #2`):**
   - Reconcile `ENTITY_STATES` `kv-watch` frames (`put` snapshot -> `synced` barrier -> live `put`/`delete`/`poisoned`) into `SvelteMap<string, CopEntityState>` keyed by 6-part entity ID, narrowing `.robotics.uxs.vehicle.<sysid>` (`1..=254`) into `VehicleEntityState` while keeping external contacts distinct with unknown-preserving provenance/staleness selectors (`source`, `observedAt`, `receivedAt`, `expiresAt`, `uncertaintyMeters`, `feedStatus`) and excluding non-vehicles from command selectors; pair with a `25 s` WS keepalive + reconnect loop and a single-root-field `POST /graph-gateway/graphql` client (`entity`, `entitiesByPrefix`, `entitiesByPredicate`, `pathSearch`).
3. **Deterministic COP Viewport Controller Verbs (`ui/src/lib/stores/viewport.svelte.ts`):**
   - Expose pure TypeScript verbs (`selectEntity`, `tetherEntity`, `setChaseLock`, `setDomainFilter`, `toggleLayer`, `setDeclutter`) and shareable URL hash serialization decoupled from DOM handlers.
4. **Capability Spec:** Add `openspec/specs/cop-ui/spec.md` (and `openspec/specs/host-runtime/spec.md` if `D4` selects host `/ws/*` proxying in `#8`).

## Scope & Non-Goals

- **In-bounds:** `ui/src/lib/` (`stores/{telemetry,skg,viewport}.svelte.ts`, `graphql.ts`, `*.test.ts`), `openspec/specs/cop-ui/spec.md` (plus `internal/host/` only if `D4` rules Option B).
- **Non-goals:** Imperative `requestAnimationFrame` canvas, HUD panels, or keyboard bindings (`#9` `M2.2`); command dispatch (`#11`/`#12` `M3`); TAK network ingest (`#17`/`#21` `M4`).

## Definition of Done

- Owner approves `proposal.md` + `design.md` with `OQ1`–`OQ4` rulings and change is archived before ready-for-review.
- Attributed Vitest suite (`// spec:` verified by `ateam test-spec`) covers rapid/out-of-order frames, `put`->`synced`->`delete`/`poisoned` state machine, external contact vs. commandable `VehicleEntityState` separation, GraphQL client responses/errors, controller verb + URL hash round-trips, `500 ms -> 30 s` reconnect backoff, and `25 s` keepalive timer firing.
- Zero runtime npm dependencies added (`ui/package.json:19-39`), `svelte-check`/`eslint`/`prettier` clean, and `task verify` green on a clean tree.
