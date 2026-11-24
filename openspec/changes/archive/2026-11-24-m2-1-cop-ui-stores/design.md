# Design: m2-1-cop-ui-stores

Architect: `gemini (gemini-4-carbon, low)`

## Context

- `ui/package.json:11-39` & `ui/eslint.config.js:41-73`: SvelteKit `^2.43.2`, Svelte `^5.39.5` (Runes `$state`/`$derived`/`$effect` configured for `**/*.svelte.ts`), Vitest `^3.2.4` (`jsdom` `^27.0.0`), zero runtime `dependencies`.
- `ui/src/lib/bootstrap.ts:1-8` & `ui/src/routes/+page.svelte:1-28`: only `UXS_DOMAINS` + `formatShellBanner` exist (`git log -S` confirms zero prior `CopEntityState` or `telemetry.svelte` code).
- `ui/vite.config.ts:17,27-36` & `ui/Caddyfile:16-30`: Vite (`:5173`) and Caddy (`:3000`) proxy `/graph-gateway` -> `:8080`, `/ws/telemetry` -> `:8081/ws`, and `/ws/graph` -> `:8082/ws`.
- `internal/host/ui.go:66-70,91-112` & `internal/host/config.go:81-89,476-506`: standalone `:8080` passes `/graph-gateway/*` through, while `/ws/telemetry` and `/ws/graph` currently hit SPA `index.html` fallback because `output/websocket` listens on separate `TelemetryWS` (`:8081/ws`) and `GraphWS` (`:8082/ws`) ports (`configs/bbops.json:254-315`, `ping_interval: "30s"`).
- `internal/mavlink/tracker.go:34-84`: `WS #1` (`bbops.telemetry.>`) wraps core NATS `TelemetryFrame` (`bbops.telemetry.<sysid>`) and `TransitionEvent` (`bbops.telemetry.events.<sysid>`, kinds `heartbeat_edge`, `failsafe_increment`, `statustext`) inside `{"type":"data","subject","timestamp","payload"}`.
- `internal/uxsvocab/entity_id.go:11-52`, `internal/uxsvocab/pack.go:23-101`, `internal/uxsvocab/snapshot.go:41-85`: vehicle IDs follow `<org>.<platform>.robotics.uxs.vehicle.<sysid>` (`1..=254`) with 18 `uxs.*` predicates, 3 `geo.location.*` predicates, `dc.terms.title`, `entity.indexing.profile = "signal"`, closed kebab-case enums, and structured `HealthFlags`.
- `PROVENANCE.md:10-12`: permits Tier-1 `C360Studio/semboids` (`flock.svelte.ts`, `graph.svelte.ts` @ `37dbdb088438a2601c8556d012ab2c33bec85da2`, MIT) and `C360Studio/semengine` (`v0.1.0-alpha.7` `output/websocket` `60 s` `ReadMessage` deadline discarding client frames; `gateway/graph-gateway` enforcing 1 root field per `POST /graph-gateway/graphql` and surfacing `extensions.damagedEntities`).

## Goals / Non-Goals

- **Goals:** Ship zero-dependency Svelte 5 Runes stores (`telemetry.svelte.ts`, `skg.svelte.ts`, `viewport.svelte.ts`) and typed `graphql.ts` client with `< 30 s` (`25 s`) WS keepalives, `500 ms -> 30 s` backoff reconnects, gapless `synced` reconciliation, strict `VehicleEntityState` vs. external COP contact separation, and pure controller/URL-hash verbs.
- **Non-Goals:** Canvas/HUD rendering or hotkeys (`#9` `M2.2`); actuation mutations (`#11`/`#12` `M3`); TAK feed ingest (`#17`/`#21` `M4`).

## Decisions

### D1. Kinematic Store Reactivity Boundary & Out-of-Order Frame Guard (`telemetry.svelte.ts`)

| Option | Cost | Blast radius | Forecloses |
| --- | --- | --- | --- |
| **A. Plain non-reactive `Map<number, VehicleKinematics>` mutated in place + monotonic `timestamp` guard (`payload.timestamp < prev.timestamp` dropped) + `$state` only for `status`, `vehicleCount`, and `100`-entry `TransitionEvent` ring buffer** | ~95 LOC store + ring buffer | `ui/src/lib/stores/telemetry.svelte.ts`, `#9` `rAF` canvas | Subscribing Svelte `$derived` DOM templates directly to `20 Hz` attitude/position fields without polling or `skg.svelte.ts` (`1–5 Hz`) |
| **B. Reactive `SvelteMap<number, VehicleKinematics>` (or per-`sysid` `$state` proxy) overwritten on every arrival without timestamp comparison** | ~75 LOC simpler store | `telemetry.svelte.ts`, `#9` HUD/canvas | Zero-DOM-invalidation `20 Hz` multi-vehicle (`500+ msg/s`) streaming (`#7` invariant) and protection against out-of-order UDP/NATS frame regression |

Adopter seam: `#9` (`M2.2`) reads `telemetryStore.getKinematics(sysid)` inside `requestAnimationFrame` for `60 fps` canvas interpolation and binds HUD alert banners to reactive `telemetryStore.events` / `telemetryStore.status`. Under A, developers binding DOM text to vehicle state use `skgStore` (`1–5 Hz` + instant edges) and never accidentally trigger `500 Hz` Svelte effect storms; doing nothing in `#9` keeps `60 fps` budgets intact. Under B, any `$derived` reading `telemetryStore` re-runs on every `20 Hz` packet. **Ruled Option A (owner, 2026-11-24).**

### D2. `ENTITY_STATES` `synced` Barrier Reconciliation & `poisoned` Retention (`skg.svelte.ts`)

| Option | Cost | Blast radius | Forecloses |
| --- | --- | --- | --- |
| **A. Shadow-stage pre-`synced` snapshot frames on `(re)connect` and swap/prune `SvelteMap<string, CopEntityState>` atomically at `synced`; retain last-good entity marked `poisoned: true` (`poisonError`, `revision`) on `poisoned` frames (or stub if unseen)** | ~110 LOC state machine | `ui/src/lib/stores/skg.svelte.ts`, `#9` roster/canvas | Reflecting partial pre-`synced` keys incrementally before the initial `ENTITY_STATES` watch burst finishes |
| **B. Clear `SvelteMap` immediately on socket `(re)open`, apply `put`/`delete` live, and evict keys on `poisoned`** | ~65 LOC direct map updates | `skg.svelte.ts`, `#9` roster/HUD, `#12` command bar | Blank-free reconnects (briefly empties COP tracks/selection on transient network blips) and surfacing corrupt-record diagnostics on an active vehicle |

Adopter seam: `#9` and `#12` read `skgStore.entities` and `skgStore.synced`. Under A, reconnects preserve visible tracks until `synced` atomically prunes server-deleted keys and updates survivors, while `poisoned` surfaces a degraded badge instead of vanishing an in-flight vehicle; callers doing nothing get flicker-free reconnects. Under B, every reconnect flashes an empty roster and clears active selection if checked naively. **Ruled Option A (owner, 2026-11-24).**

### D3. Ownship `VehicleEntityState` vs. External COP Contact (`CopEntityState`) Boundary (`skg.svelte.ts`)

| Option | Cost | Blast radius | Forecloses |
| --- | --- | --- | --- |
| **A. Discriminated union `CopEntityState` (`kind: "uxs-vehicle" \| "external-contact" \| "generic"`): narrow to `"uxs-vehicle"` iff ID matches `<org>.<platform>.robotics.uxs.vehicle.<sysid>` (`1..=254`) AND core `uxs.*` triples validate; reserve nullable/unknown contact provenance (`source`, `observedAt`, `receivedAt`, `expiresAt`, `uncertaintyMeters`, `feedStatus: ... \| "unknown"`); `selectedCommandableVehicle` returns `null` unless `kind === "uxs-vehicle" && !poisoned`** | ~130 LOC triple parser + union guards | `skg.svelte.ts`, `viewport.svelte.ts`, `#9`, `#12`, `#17`/`#21` | Treating partial/non-vehicle entities carrying stray `uxs.*` predicates as controllable ownship vehicles |
| **B. Single flat `CopEntityState` record with optional `uxs?: Partial<VehicleFields>` populated opportunistically from whatever `uxs.*` triples exist** | ~80 LOC looser mapper | `skg.svelte.ts`, every `#9`/`#12`/`#21` consumer | Compile-time guarantee that selecting an external TAK/CoT contact never leaks into `#12` (`M3.2`) vehicle actuation selectors |

Adopter seam: `#12` (`M3.2` command bar) binds exclusively to `selectedCommandableVehicle` (`VehicleEntityState | null`), while `#9` COP canvas/inspector renders `skgStore.entities.values()` across all `kind`s and `#21` (`M4` CoT ingest) populates `cop.*` contacts without refactoring M2 stores. Under A, unknown contact metadata stays `null`/`"unknown"` and non-vehicle selection returns `null` in command selectors automatically; under B, every actuation caller must repeat manual ID/predicate guards. **Ruled Option A (owner, 2026-11-24).**

### D4. WebSocket Endpoint Resolution & Viewport URL Hash Encoding (`viewport.svelte.ts`, `internal/host/ui.go`)

| Option | Cost | Blast radius | Forecloses |
| --- | --- | --- | --- |
| **A. Pure `ui/` scope in `M2.1`: default browser URLs to same-origin `/ws/telemetry`, `/ws/graph`, and `/graph-gateway/graphql` (`ui/vite.config.ts:17,27-36`, `ui/Caddyfile:16-30`) with injectable URL/socket overrides (`VITE_*` / factory options; standalone `:8080` `/ws/*` proxy deferred to `#9` `M2.2`), plus human-readable `URLSearchParams` hash `#sel=<id>&tether=<id>&chase=1&domains=...&layers=...&declutter=...` omitting defaults** | ~95 LOC in `ui/src/lib/` (0 Go LOC) | `ui/src/lib/stores/`, `openspec/specs/cop-ui/spec.md` | Opening compiled UI directly on standalone `:8080` without Caddy/Vite or explicit WS port overrides until `#9` wires standalone `/ws/*` |
| **B. Cross-layer `M2.1` scope: also add `/ws/telemetry` & `/ws/graph` HTTP reverse-proxying in `internal/host/ui.go` (`ResolvedEndpoints.{TelemetryWS,GraphWS}`) + `openspec/specs/host-runtime/spec.md` now, and encode viewport hash as `#state=<uri-encoded-json>`** | ~170 LOC across Go host + UI + `task mutate` | `internal/host/`, `ui/src/lib/`, 2 capability specs | Keeping `#8` isolated to `ui/` stores and hand-editing/diffing readable `#sel=...` deep links |

Adopter seam: `#9` (`M2.2` hotkeys/shell) and `#19` (`M4` MCP/copilot tools) invoke `viewportStore.{selectEntity,tetherEntity,setChaseLock,setDomainFilter,toggleLayer,setDeclutter}` and `toUrlHash()`/`applyUrlHash()`. Under A, `#8` stays strictly inside `ui/` with transparent `#sel=...` URLs and `#9` wires standalone host `/ws/*` alongside the mounted SPA; under B, standalone `:8080` proxies `/ws/*` immediately at wider PR blast radius. **Ruled Option A (owner, 2026-11-24).**

## Risks / Trade-offs

- **Receive-only WebSocket `1006` drops (`60 s` server `ReadMessage` deadline):** Mitigated on both `WS #1` and `WS #2` by sending `{"type":"keepalive","timestamp":...}` every `25 s` while `readyState === OPEN`, paired with `500 ms -> 30 s` exponential reconnect backoff (resetting on open/sync).
- **Multi-root GraphQL `400` rejections:** `semengine/gateway/graph-gateway` rejects multi-field documents and introspection; `graphql.ts` issues strictly single-root-field POST helpers (`entity`, `entitiesByPrefix`, `entitiesByPredicate`, `pathSearch`) with `AbortSignal` support and surfaces `extensions.damagedEntities`.

## Migration Plan

Additive TypeScript/Svelte 5 modules under `ui/src/lib/` (`stores/` + `graphql.ts`) and `openspec/specs/cop-ui/spec.md`. Existing `ui/src/routes/+page.svelte` bootstrap tests remain intact until `#9` (`M2.2`) mounts the COP workspace; rollback is reverting the merge commit.

## Open Questions

1. **Resolved (`OQ1`–`OQ4` & `#28`, owner 2026-11-24):** D1 Option A (plain non-reactive `Map` for `20 Hz` kinematics + monotonic `timestamp` guard + `$state` strictly for status/disconnect-gap metadata and `100`-entry `TransitionEvent` ring buffer preserved across live-only `#28` Option A reconnects), D2 Option A (shadow-stage pre-`synced` frames for atomic gapless swap/prune + retain `poisoned`-flagged tracks), D3 Option A (discriminated union `"uxs-vehicle" | "external-contact" | "generic"` gating `selectedCommandableVehicle` on `.robotics.uxs.vehicle.<sysid>` + valid `uxs.*` triples with unknown-preserving contact metadata and `Triple.Timestamp`-derived vehicle `observedAt`), and D4 Option A (pure `ui/` scope with same-origin `/ws/*` defaults + injectable overrides and `URLSearchParams` hash).
