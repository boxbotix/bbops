# Proposal: m2-2-cop-ui-canvas
Approved-by:
Architect: gemini (gemini-4-carbon, low)

## Track

`design`: touches two capability specs (`openspec/specs/cop-ui/spec.md` and `openspec/specs/host-runtime/spec.md`), adds 3 `cop-ui` `Requirement`s, exports the renderer-agnostic `ViewportAdapter` seam (`ui/src/lib/viewport/adapter.ts`) consumed by `#16` (`M4.1`), `#18` (`M4.3`), and `#19` (`M4.4`), registers `C360Studio/semstreams-ui` (`MIT`) in Tier-1 provenance for the `Sigma` + `ForceAtlas2` SKG graph canvas, and wires standalone HTTP front-door `/ws/telemetry` and `/ws/graph` routing in `internal/host/` (#9 / Epic #7).

## Why

`M2.1` (`#8`, PR #26) landed zero-thrash Svelte 5 Runes stores (`ui/src/lib/stores/{telemetry,skg,viewport}.svelte.ts`) and the ADR-115 GraphQL client (`ui/src/lib/graphql.ts`), while `ui/src/routes/+page.svelte:1-28` remains the `M0.1` domain-button bootstrap placeholder and `internal/host/ui.go:66-70,91-112` still falls back to `index.html` on same-origin `/ws/telemetry` and `/ws/graph`. `#9` (`M2.2`) builds the operator-facing 60 fps multi-domain COP canvas, quaternion attitude HUD, 4-state `ControlAuthority` banner, `semstreams-ui`-adapted SKG `Sigma` topology/triple inspector pane, external-contact UI isolation, and standalone host `/ws/*` routing so `./bin/bbops` (and the optional `ui/Caddyfile` stack) serves a complete read-only multi-domain GCS/COP workspace before `M3` actuation (`#11`).

## What Changes

1. **Renderer-Agnostic `ViewportAdapter` & Imperative `rAF` Tactical Canvas (`ui/src/lib/viewport/`, `TacticalCanvas.svelte`):**
   - Define `ViewportAdapter` (`project`, `unproject`, `screenCourseRad`, `flyTo`, `setCamera`) with dual-frame `WorldPoint` (`{ ned?, lla? }`) backed in `M2.2` by zero-dependency `NedGridAdapter` (tangent-plane `lla` contact projection), render tactical glyphs (`Quadcopter`, `Rover`, `SurfaceBoat`, plus external contacts), selection/tether reticles, bounded wake trails, USV crab-angle bow-vs-velocity vectors, and reserved safety hues (`red`/`amber`/`green`) on an upper DPR-aware 2D `rAF` canvas above `.viewport-surface`, and bind keyboard hotkeys (`T`, `C`, `H`, `Esc`) + URL hash sync to `viewport.svelte.ts`.
2. **Primary Flight/Helm HUD, Authority Banner & External-Contact Isolation (`ui/src/lib/hud/`, `VehicleHud.svelte`, `AuthorityBanner.svelte`):**
   - Render ZYX quaternion pitch/roll horizon + heading tape (`rAF` 2D canvas), groundspeed/climb, `LinkTier` badge (`SITL`, `Bench`, `HaLow 30Hz`, `LoRa 1Hz`), 4-state `ControlAuthority` (`P1`–`P4`) + `armed` banner, and MCU/trunk health strip (`loop_dt_us`, `max_loop_dt_us`, `rtt_us`, `failsafes`, `crc_errors`, `HealthFlags`), while isolating `ExternalContactEntityState` selection to an unknown-preserving provenance/freshness card with zero vehicle authority/control affordances.
3. **Live SKG `Sigma` Topology, Triple Inspector & Workspace Shell (`ui/src/lib/skg/`, `SkgPane.svelte`, `ui/src/routes/+page.svelte`):**
   - Adapt the proven `C360Studio/semstreams-ui` (`MIT`) `SigmaCanvas.svelte` + `graphology-adapter.ts` + `sigma-layout.ts` (`sigma` v3, `graphology`, `graphology-layout-forceatlas2` worker auto-stopping at `3 s`, position-preserving sync, `doubleClickNode` -> `graphql.ts` `pathSearch` expansion, zoom/fit controls, and non-safety palette tokens), paired with a selected-entity `uxs.*`/`cop.*` triple inspector and scrolling `TransitionEvent` timeline.
4. **Standalone Go Host Same-Origin `/ws/*` Reverse-Proxy (`internal/host/ui.go`, `internal/host/runtime.go`):**
   - Proxy `/ws/telemetry` -> `TelemetryWS` and `/ws/graph` -> `GraphWS` via Go stdlib `httputil.NewSingleHostReverseProxy` on `:8080` once `isReady()` is true (`503` pre-ready), preserving direct `:8081`/`:8082` listeners for `ui/Caddyfile` (`:3000`) and Vite (`:5173`).

## Scope & Non-Goals

- **In-bounds:** `ui/src/lib/{viewport,hud,skg,components}/`, `ui/src/routes/+page.svelte`, `ui/package.json` (`sigma`, `graphology`, `graphology-types`, `graphology-layout-forceatlas2`), `internal/host/{ui.go,runtime.go,runtime_integration_test.go}`, `PROVENANCE.md`, `.agents/rules/ip-cleanroom.md`, `AGENTS.md`, `THIRD_PARTY_NOTICES`, and `openspec/specs/{cop-ui,host-runtime}/spec.md`.
- **Non-goals:** Vehicle command/teleop TX (`#11` `M3.1`), `internal/mavlink` link-loss/flush refactors (`#27`/`#29`), 2.5D/3D MapLibre/PMTiles globe (`#16` `M4.1`), post-process shaders (`#18` `M4.3`), and live TAK/CoT ingest (`#17`/`#21` `M4.2`).

## Definition of Done

- Owner approves `proposal.md` + `design.md` (`OQ1`–`OQ4` ruled) and change is archived before ready-for-review.
- Attributed Go + Vitest tests (`// spec:` verified by `ateam test-spec`) and non-destructive mutation checks (`task mutate` + `cp`/SHA-256) cover `NedGridAdapter` projection/course math, USV crab-angle vectors, quaternion Euler/HUD badge mappers, external-contact UI separation, hotkey guards, `semstreams-ui`-adapted SKG `graphology` sync/palette rules, and standalone host `/ws/*` readiness/upgrade routing.
- `npm --prefix ui run build` serves cleanly through `./bin/bbops`, Tier-1 provenance notices (`semboids`, `semstreams-ui`, `sigma`/`graphology`) are recorded, and `task verify` passes on a clean tree.
