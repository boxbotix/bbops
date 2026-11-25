# Design: m2-4-workspace-modes

Architect: `gemini (gemini-4-carbon, low)`

## Context

- `ui/src/routes/+page.svelte:18-28,81-119,172-197`: mounts one static `1fr + 360px` `.workspace-grid` rendering `AuthorityBanner`, `TacticalCanvas`, `VehicleHud`, and `SkgPane` simultaneously (`git log -S` confirms zero `WorkspaceMode`, `FleetFilmstrip`, `BenchPane`, or `CortexPane` symbols).
- `ui/src/lib/components/AuthorityBanner.svelte:18-98` & `ui/src/lib/hud/view-model.ts:67-93,231-362`: `buildVehicleBannerView` couples operator `P1`–`P4` `ControlAuthority`/`armed`/`LinkTier` badges with `9` bench `0xB1` counters (`loop_dt_us`, `max_loop_dt_us`, `rtt_us`, `crc_errors`, `failsafes`, `HealthFlags`) while isolating `ExternalContactEntityState`.
- `ui/src/lib/components/SkgPane.svelte:220-375,435-533` & `ui/src/lib/skg/graphology-adapter.ts:28-40,142-160`: stacks three scroll containers (`190px` `Sigma`, `170px` `Triple Inspector`, `160px` `Transition Events`) inside `360px` using non-safety palette tokens (`#7`).
- `ui/src/lib/stores/viewport.svelte.ts:16-63,115-232` & `ui/src/lib/viewport/hotkeys.ts:28-52,72-140`: pure controller verbs, `#sel=...` URL hash codec, and `T`/`C`/`H`/`Escape` hotkeys with editable-target and modifier-chord guards.
- `ui/src/lib/stores/telemetry.svelte.ts:48-88,100-116` & `ui/src/lib/stores/skg.svelte.ts:76-189`: `WS #1` overwrites latest per-`sysid` `VehicleKinematics` in place (no time-series history buffer yet) + `100`-entry `TransitionEvent` ring; `WS #2` exposes `vehicles`, `commandableVehicles`, and `externalContacts`.
- `configs/bbops.json:189-249`, `internal/host/config.go:51-62`, `internal/host/ui.go:41-94,206-227`, `internal/host/runtime.go:279-299`, `ui/Caddyfile:28-30`, `ui/vite.config.ts:17`, `ui/src/lib/graphql.ts:91-115`: `semengine v0.1.0-alpha.7` runs `graph-lexical` and `graph-query` (`embedding_enabled: false`), while `graph-gateway` (`POST /graph-gateway/graphql`) exposes only `entity`, `entitiesByPrefix`, `entitiesByPredicate`, and `pathSearch`; `/graph-gateway/*` is already reserved and reverse-proxied in both `ui/Caddyfile` and `ui/vite.config.ts`.

## Goals / Non-Goals

- **Goals:** Separate `COP`, `Pilot`, `Bench`, and `Cortex` task stages over shared live `telemetryStore`/`skgStore`/`viewportStore` connections; keep fleet-wide fault counts, selected safety state, and `FleetFilmstrip` non-modal; default `Pilot` to `Map-Primary` with `[M]` `Split` swap; give `Bench` zero-thrash 20 Hz stripcharts + `0xB1` diagnostics + staged `TuningRack` seam; and wire `Cortex` over a read-only `POST /graph-gateway/search` (`graph.query.*`) bridge + 2-hop causal `pathSearch` subgraph.
- **Non-Goals:** Outbound `COMMAND_LONG` / teleop / `PARAM_SET` UDP actuation (`#11`), `internal/mavlink` ingest changes (`#27`/`#29`), 2.5D/3D MapLibre globe (`#16`), sensor shaders (`#18`), and live MCP/voice session brokering (`#19`).

## Decisions

### D1. Workspace Mode State, Non-Modal Safety Chrome & Navigation (`viewport.svelte.ts`, `+page.svelte`)

| Option | Cost | Blast radius | Forecloses |
| --- | --- | --- | --- |
| **A. Extend `ViewportSnapshot` (`workspace: "cop" \| "pilot" \| "bench" \| "cortex"`, `pilotLayout: "map" \| "split"`, `lightsOut: boolean`) serialized in `#mode=...&pilot=...&chrome=...` (omitting `"cop"`/`"map"`/`false` defaults so existing `#sel=...` hashes stay identical) inside single-route `+page.svelte`, with an invariant safety topbar (stream pills + global fleet fault counter across `skgStore.vehicles` + core authority/arming/link or contact badge + module picker) and persistent `FleetFilmstrip.svelte` (`G`/`D`/`B`/`K` modes, `M` pilot swap, `Tab` lights-out, `1`–`9` filmstrip select)** | ~140 LOC TS + ~190 LOC Svelte | `viewport.svelte.ts`, `hotkeys.ts`, `+page.svelte`, `#19`, `#34` | Separate browser pathname entries (`/pilot`, `/bench`) in `window.location.pathname` |
| **B. Multi-route SvelteKit pages (`/cop`, `/pilot`, `/bench`, `/cortex`) lifting `telemetryStore`/`skgStore` into `+layout.svelte` and storing mode in `pathname` instead of `ViewportSnapshot`** | ~170 LOC TS/Svelte across 5 route files | `ui/src/routes/**`, `internal/host/ui.go` SPA fallback tests, `#19`, `#34` | Programmatic single-snapshot `viewportStore.snapshot()` / `#...` URL sharing of workspace + selection + tether state |

Adopter seam: `#19` (`M4.4` MCP director) and `#34` (`M2.3` E2E) drive `viewportStore` verbs (`setWorkspace`, `setPilotLayout`, `setLightsOut`, `selectEntity`). Under A, callers doing nothing boot into `"cop"` with identical `#sel=...` hashes and continuous WebSocket connections; under B, `#19` and `#34` must coordinate SvelteKit router navigation alongside `viewportStore`. **Ruled Option A (owner, 2026-11-24).**

### D2. `COP` Context Rail & `Pilot` Map-vs-PFD Surface Composition (`CopWorkspace.svelte`, `PilotWorkspace.svelte`)

| Option | Cost | Blast radius | Forecloses |
| --- | --- | --- | --- |
| **A. `COP` pairs a collapsible left `Roster` + full-bleed `TacticalCanvas` + right Solo-Mode accordion rail (`Track Summary` \| `Active Exceptions` \| `Quick SKG Relations`, strictly 1 section open); `Pilot` defaults to `Map-Primary` (chase-capable `TacticalCanvas` + floating corner `VehicleHud` PiP + guarded action slot) and swaps via `[M]` to `Split` (`VehicleHud` PFD + telemetry cards + guarded action slot beside `TacticalCanvas`), auto-promoting abnormal `HealthChipView`s (`severity !== "nominal"`) and rendering a read-only contact card on `ExternalContactEntityState`** | ~240 LOC Svelte + ~65 LOC TS view-model | `ui/src/lib/hud/view-model.ts`, `CopWorkspace.svelte`, `PilotWorkspace.svelte`, `#11`, `#16`, `#18` | Viewing `COP` summary, alerts, and SKG relations simultaneously without switching accordion tabs |
| **B. Keep multi-open stacked cards in `COP`'s right rail and ship `Pilot` as a fixed 50/50 PFD + Map split without `Map-Primary` PiP swap** | ~150 LOC Svelte | `CopWorkspace.svelte`, `PilotWorkspace.svelte`, `#11`, `#16` | Full-screen spatial map awareness in `Pilot` on small field tablets and scroll-trap-free `COP` inspection |

Adopter seam: `#11` (`M3.1` `ControlPanel.svelte`) mounts into `PilotWorkspace`'s guarded action Snippet gated on `viewportStore.selectedCommandableVehicle`, while `#16`/`#18` mount inside `TacticalCanvas`'s `.viewport-surface`. If `#11` is absent (`M2.4`), the slot renders a read-only `"Actuation offline (M3.1)"` placeholder and remains completely absent when an external contact is selected. **Ruled Option A (owner, 2026-11-24).**

### D3. `Bench` 20 Hz Telemetry Oscilloscope & Parameter Tuning Rack Seam (`ui/src/lib/bench/`, `BenchWorkspace.svelte`)

| Option | Cost | Blast radius | Forecloses |
| --- | --- | --- | --- |
| **A. Fixed-capacity per-`sysid` typed-array ring buffer (`300` samples = `15 s @ 20 Hz`, mutated outside Svelte `$state`) rendered on a DPR-aware 2D `rAF` stripchart `<canvas>` (attitude rates, groundspeed/climb, `loop_dt_us`/`rtt_us`), paired with the `0xB1` MCU/trunk timing bar (moved out of `AuthorityBanner`), Lane 3 event console, and a staged `TuningRack` (`draft` vs `baseline` dials/sliders with optional `onApply` callback disabled as read-only preview until `#11`)** | ~180 LOC TS + ~210 LOC Svelte | `ui/src/lib/bench/`, `BenchWorkspace.svelte`, `AuthorityBanner.svelte`, `#11` | Inspecting >`15 s` historical telemetry traces without scrolling/export |
| **B. Reactive Svelte `$state` sample arrays driving SVG `<polyline>` charts in `BenchWorkspace`, omitting the `TuningRack` UI until `#11` lands outbound `0xB1` config TX** | ~120 LOC Svelte | `BenchWorkspace.svelte`, `#11` | Zero-DOM-thrash `20 Hz` rendering (`#7` guardrail) and validating the Lightroom `Develop`-style staged slider/dial UX in `M2.4` |

Adopter seam: `#11` (`M3.1`) supplies an ADR-111 non-retrying `onApply(sysid, stagedDiff)` handler to `BenchWorkspace`. Callers doing nothing get live `20 Hz` stripcharts, `0xB1` timing diagnostics, and local staged diff/revert preview with zero outbound UDP packets. **Ruled Option A (owner, 2026-11-24).**

### D4. `Cortex` Lexical, Semantic & Neural SKG Surface vs. Go Host Search Bridge (`ui/src/lib/cortex/`, `internal/host/`)

| Option | Cost | Blast radius | Forecloses |
| --- | --- | --- | --- |
| **A. Add read-only `POST /graph-gateway/search` in `internal/host/` forwarding strict-decoded `semengine/graph` (`GlobalSearchRequest`, `LocalSearchRequest`, `SemanticRequest`, `SimilarRequest`) to `graph.query.{globalSearch,localSearch,semantic,similar}` via `rt.NATSClient()` (`503` pre-ready; bounded body/limit/timeout; surfacing `not_enabled` cleanly while `embedding_enabled: false`), paired in `CortexWorkspace.svelte` with `graphql.ts` `pathSearch` 2-hop **Causal Evidence Subgraph** (`Sigma`) and `kvRevision`-cited triple provenance** | ~120 Go LOC + ~195 TS/Svelte LOC | `internal/host/{ui.go,search.go,runtime.go}`, `openspec/specs/{cop-ui,host-runtime}/spec.md`, `ui/src/lib/cortex/`, `#19` | Keeping `M2.4` strictly TypeScript/Svelte-only |
| **B. Pure UI `M2.4` (`0` Go LOC): `CortexWorkspace.svelte` executes client-side token + predicate search over `skgStore.entities` + `graphql.ts` `pathSearch`, deferring `graph.query.*` bridging to `#19`** | ~175 TS/Svelte LOC (`0` Go LOC) | `ui/src/lib/cortex/`, `CortexWorkspace.svelte`, `openspec/specs/cop-ui/spec.md`, `#19` | Querying `semengine` `graph-lexical` BM25 scores/hints/`indexedRevision` and neural `semantic`/`similar` verbs from the browser in `M2.4` |

Adopter seam: `/graph-gateway/*` is already proxied by `ui/Caddyfile:28-30` and `ui/vite.config.ts:17`, and `#19` (`M4.4` `internal/copmcp`) reuses the exact read-only `graph.query.*` validator/bridge and `CortexWorkspace` 3-column layout with zero access to `#11` actuation paths. **Ruled Option A (owner, 2026-11-24).**

## Risks / Trade-offs

- **PR `#31` (`M2.2`) & PR `#35` (`M2.3`) merge ordering:** `M2.4` restructures `+page.svelte` and `AuthorityBanner.svelte`; sequencing implementation after `#31` and `#35` merge avoids E2E selector churn (`data-testid` hooks preserved).
- **Single-key hotkey & search bridge safety:** `G`/`D`/`B`/`K`/`M`/`Tab`/`1–9` mutate view state only (suppressed on editable targets/modifiers), and `POST /graph-gateway/search` whitelists strictly read-only `graph.query.{globalSearch,localSearch,semantic,similar}` subjects with `64 KiB` body and `Limit` caps.

## Migration Plan

Land `M2.4` after PR `#31` and PR `#35`, wiring `POST /graph-gateway/search` in `internal/host/`, extending `viewport.svelte.ts` + `hotkeys.ts`, splitting `AuthorityBanner` (core safety in topbar; `0xB1` MCU timing in `Bench`), mounting `COP`, `Pilot`, `Bench`, and `Cortex` + `FleetFilmstrip` in `+page.svelte`, and reconciling E2E selectors. Rollback is reverting the merge commit.

## Open Questions

1. **Resolved (`OQ1`–`OQ4`, owner 2026-11-24):** `D1` Option A (single-route `ViewportSnapshot` `#mode=&pilot=&chrome=` + invariant safety topbar + `FleetFilmstrip` + `G`/`D`/`B`/`K`/`M`/`Tab`/`1–9` view hotkeys), `D2` Option A (`COP` Solo-Mode accordion + `Pilot` `[M]` swap defaulting to `Map-Primary` w/ corner attitude PiP + `#11` guarded action slot seam), `D3` Option A (`300`-sample typed-array `rAF` 20 Hz oscilloscope + `0xB1` MCU strip in `Bench` + staged read-only `TuningRack` seam for `#11`), and `D4` Option A (thin read-only Go `POST /graph-gateway/search` -> `graph.query.{globalSearch,localSearch,semantic,similar}` bridge alongside `Cortex`'s 2-hop `pathSearch` causal subgraph and `kvRevision` triple citations).
