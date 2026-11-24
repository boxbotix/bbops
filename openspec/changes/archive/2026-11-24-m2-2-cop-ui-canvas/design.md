# Design: m2-2-cop-ui-canvas

Architect: `gemini (gemini-4-carbon, low)`

## Context

- `ui/package.json:11-39` & `ui/vite.config.ts:17,27-43`: SvelteKit `^2.43.2`, Svelte `^5.39.5`, Vitest `^3.2.4` (`jsdom` without WebGL/Worker), zero runtime `dependencies` (`git log -S` confirms zero prior `ViewportAdapter`, `TacticalCanvas`, `SkgPane`, or `ReverseProxy` symbols).
- `ui/src/routes/+page.svelte:1-28` & `ui/src/lib/bootstrap.ts:1-8`: shell still renders `M0.1` placeholder buttons; `ui/src/lib/bootstrap.test.ts:5-14` cites `bootstrap.ts` directly.
- `ui/src/lib/stores/telemetry.svelte.ts:48-116,324-338`: `WS #1` (`/ws/telemetry`) mutates non-reactive `Map<number, VehicleKinematics>` (`quat_wxyz`, `body_rates`, `ned_xyz`, `ned_vel`, `lat_lon_alt`, `has_global_pos`, `hdg_deg`, `timestamp`) in place and exposes `$state` only for `status`, `vehicleCount`, and `100`-entry `TransitionEvent` ring (`heartbeat_edge`, `failsafe_increment`, `statustext`, `link_lost`, `link_restored`).
- `ui/src/lib/stores/skg.svelte.ts:76-189`: `WS #2` (`/ws/graph`) exposes reactive `SvelteMap<string, CopEntityState>` (`"uxs-vehicle" | "external-contact" | "generic"`), where `ExternalContactEntityState` carries nullable WGS-84 `location` + unknown-preserving `source`/`observedAt`/`receivedAt`/`expiresAt`/`uncertaintyMeters`/`feedStatus` without `ned`, and `getCommandableVehicle` excludes non-vehicles and `poisoned` records.
- `ui/src/lib/stores/viewport.svelte.ts:16-63,126-232` & `ui/src/lib/graphql.ts:91-115`: pure verbs (`selectEntity`, `tetherEntity`, `setChaseLock`, `setDomainFilter`, `toggleLayer`, `setDeclutter`), `#sel=...` URL hash codecs, and single-root-field `POST /graph-gateway/graphql` (`entity`, `entitiesByPrefix`, `entitiesByPredicate`, `pathSearch`).
- `internal/mavlink/tracker.go:594-608`: Hamilton `[w, x, y, z]` ZYX (`3-2-1`, Fossen §2.2.2) NED-to-FRD yaw (`QuaternionYawDeg`).
- `internal/host/ui.go:17-74,91-112`, `internal/host/config.go:81-89,476-506,641-676`, `internal/host/runtime.go:210-215,277-279`, `ui/Caddyfile:16-25`: standalone `:8080` passes `/graph-gateway/*` through while `/ws/telemetry` and `/ws/graph` currently hit SPA `index.html` fallback because `output/websocket` listens on separate `TelemetryWS` (`:8081/ws`) and `GraphWS` (`:8082/ws`) ports (`#8` `D4 Option A` deferred host `/ws/*` routing to `#9`).
- `PROVENANCE.md:8-15,30-32`: authorizes Tier-1 MIT `C360Studio/semboids` (`FlockCanvas.svelte`, `colors.css` @ `37dbdb088438a2601c8556d012ab2c33bec85da2`); `C360Studio/semstreams-ui` (`MIT` @ `3814b3d59dab7136011429cad99a8148aeceb0cc`, `src/lib/components/runtime/SigmaCanvas.svelte`, `src/lib/utils/graphology-adapter.ts`, `src/lib/utils/sigma-layout.ts`) provides proven `sigma` v3 + `graphology` + `graphology-layout-forceatlas2` SKG rendering once added to Tier-1 provenance (`PROVENANCE.md`, `.agents/rules/ip-cleanroom.md`, `AGENTS.md`, `THIRD_PARTY_NOTICES`).

## Goals / Non-Goals

- **Goals:** Deliver 60 fps `TacticalCanvas.svelte` + `ViewportAdapter`/`NedGridAdapter` with surface/safety-HUD layer split and USV crab-angle vectors, `VehicleHud.svelte` + `AuthorityBanner.svelte` with strict external-contact isolation, `semstreams-ui`-adapted `Sigma` SKG topology/triple/event inspector (`SkgPane.svelte`), COP workspace shell (`+page.svelte`) with hotkeys/URL hash sync, and standalone `:8080` `/ws/{telemetry,graph}` reverse-proxying compatible with `ui/Caddyfile`.
- **Non-Goals:** Command/teleop actuation (`#11`), `internal/mavlink` refactors (`#27`/`#29`), 2.5D/3D MapLibre/PMTiles globe (`#16`), post-process shaders (`#18`), and TAK network ingest (`#17`/`#21`).

## Decisions

### D1. `ViewportAdapter` Coordinate Contract & Surface-vs-Safety-HUD Layer Split (`ui/src/lib/viewport/`)

| Option | Cost | Blast radius | Forecloses |
| --- | --- | --- | --- |
| **A. Dual-frame `WorldPoint` (`{ ned?: [n,e,d]; lla?: [lat,lon,alt] }`) in `ViewportAdapter` (`project`, `unproject`, `screenCourseRad`, `flyTo`, `setCamera`) + `NedGridAdapter` tangent-plane `lla` projection (anchored to explicit or first valid `has_global_pos`/`location` origin; `visible: false` when unanchored) + stacked `.viewport-surface` slot beneath an upper DPR-aware 2D `rAF` safety-HUD `<canvas>` (`telemetryStore` -> `skgStore` 4 Hz fallback + bounded wake ring)** | ~165 LOC TS + ~210 LOC Svelte | `ui/src/lib/viewport/`, `TacticalCanvas.svelte`, `#16`, `#18` | Coupling tactical glyph/reticle draw calls directly to `NedGridAdapter` grid lines on a single canvas |
| **B. `NED`-only `ViewportAdapter` (`project(northM, eastM)`) drawing grid and glyphs on one shared `<canvas>` and omitting `lla`-only external contacts until `#16`** | ~110 LOC TS/Svelte | `ui/src/lib/viewport/`, `#16`, `#18`, `#21` | Projecting `lla`-only external contacts in `M2.2` and shading `.viewport-surface` in `#18` (`M4.3`) without tinting red/amber/green safety indicators |

Adopter seam: `#16` (`MapLibreAdapter`) mounts inside `.viewport-surface` implementing `ViewportAdapter`, `#18` applies NVG/FLIR shaders strictly to `.viewport-surface`, and `#21` (`M4` CoT contacts) populates `location` on `ExternalContactEntityState`. Under A, callers doing nothing keep safety colors unshifted above any basemap/shader and project both UXS `ned` tracks and `lla`-only contacts; under B, `#18` must refactor `TacticalCanvas` and external contacts cannot plot on the local grid. **Ruled Option A (owner, 2026-11-24).**

### D2. Attitude Horizon, USV Crab-Angle Kinematics & External-Contact HUD Boundary (`ui/src/lib/hud/`)

| Option | Cost | Blast radius | Forecloses |
| --- | --- | --- | --- |
| **A. Pure TypeScript `ui/src/lib/hud/view-model.ts` (Fossen §2.2.2 ZYX Euler roll/pitch/yaw with `asin` clamp, crab-angle $\beta = \operatorname{wrap}_{(-\pi,\pi]}(\operatorname{atan2}(v_e,v_n)-\psi)$ gated by `0.15 m/s` groundspeed deadband, `P1`–`P4` authority/link/health badge mappers, unknown-preserving contact presenter) + 2D `rAF` `<canvas>` horizon/heading tape in `VehicleHud.svelte` + `4 Hz`/edge-reactive `AuthorityBanner.svelte` (`VehicleEntityState` only; external contacts render dedicated provenance card)** | ~175 LOC TS + ~195 LOC Svelte | `ui/src/lib/hud/`, `VehicleHud.svelte`, `AuthorityBanner.svelte`, `#11` | Styling individual pitch-ladder ticks with CSS DOM transitions |
| **B. `20–60 Hz` Svelte `$state` ticker driving SVG horizon DOM transforms, unthresholded `atan2(vy, vx)` course arrows, and one shared header component with inline `entity.kind` branches** | ~150 LOC Svelte | `VehicleHud.svelte`, `+page.svelte`, `#11` | Zero-DOM-thrash `20 Hz` streaming (`#7` guardrail), jitter-free stationary USV bow rendering, and structural exclusion of external contacts from vehicle authority UI |

Adopter seam: `#11` (`M3.1` `ControlPanel.svelte`) mounts beside `AuthorityBanner` bound to `viewportStore.selectedCommandableVehicle`. Under A, `20 Hz` horizon motion stays inside `rAF`, stationary boats suppress noisy `0 m/s` drift vectors, and selecting an external contact or `poisoned` vehicle yields zero actuation/authority affordances by construction; under B, every `20 Hz` tick invalidates Svelte DOM and `#11` must duplicate contact guards. **Ruled Option A (owner, 2026-11-24).**

### D3. SKG Topology Graph Engine (`semstreams-ui` `SigmaCanvas` + `ForceAtlas2` vs. Zero-Dep 2D Canvas) (`SkgPane.svelte`, `ui/package.json`)

| Option | Cost | Blast radius | Forecloses |
| --- | --- | --- | --- |
| **A. Adapt `C360Studio/semstreams-ui` (`MIT` @ `3814b3d59dab7136011429cad99a8148aeceb0cc`) `SigmaCanvas.svelte` + `graphology-adapter.ts` + `sigma-layout.ts` with MIT `sigma` (`^3.0.3`), `graphology` (`^0.26.0`), `graphology-types` (`^0.24.8`), and `graphology-layout-forceatlas2` (`^0.10.1`) (`3 s` auto-stop worker layout, position-preserving sync, neighbor-seeded `pathSearch` expansion, `nodeReducer`/`edgeReducer` focus, zoom/fit controls, `jsdom`/non-WebGL guard, and non-safety blue/cyan/indigo/violet/teal/slate palette; register `semstreams-ui` in `PROVENANCE.md` Tier 1 + `THIRD_PARTY_NOTICES`)** | ~180 LOC TS/Svelte + 4 MIT packages + provenance entries | `ui/package.json`, `PROVENANCE.md`, `.agents/rules/ip-cleanroom.md`, `AGENTS.md`, `THIRD_PARTY_NOTICES`, `ui/src/lib/skg/`, `SkgPane.svelte` | Keeping `ui/package.json` at zero runtime npm dependencies |
| **B. Zero-dependency HTML5 2D `<canvas>` radial/force topology view inside `SkgPane.svelte` (`0` new npm packages)** | ~195 LOC custom 2D layout/pan/hit-test TS/Svelte | `ui/src/lib/skg/`, `SkgPane.svelte` | Reusing proven `semstreams-ui` WebGL `Sigma` + `ForceAtlas2` layout/focus UX when `#17`/`#21`/`#23` scale SKG topology |

Adopter seam: `SkgPane.svelte` syncs `skgStore.entities` + `graphqlClient.pathSearch` into `graphology`, `clickNode` calls `viewportStore.selectEntity`, `doubleClickNode` expands `pathSearch`, and node/edge colors replace `semstreams-ui`'s green/amber/red tokens with non-safety hues (`#7`). Under A, `bbops` reuses `semstreams-ui`'s battle-tested `Sigma` + `ForceAtlas2` canvas with `jsdom`-safe guards; under B, `ui/` avoids npm dependencies at the cost of bespoke 2D graph layout/zoom code. **Ruled Option A with `C360Studio/semstreams-ui` `SigmaCanvas` + `ForceAtlas2` adaptation (owner, 2026-11-24).**

### D4. Standalone Go Host `/ws/telemetry` & `/ws/graph` Front-Door Routing (`internal/host/`)

| Option | Cost | Blast radius | Forecloses |
| --- | --- | --- | --- |
| **A. Stdlib `httputil.NewSingleHostReverseProxy` in `internal/host/ui.go` wired from `rt.endpoints.{TelemetryWS,GraphWS}` (`internal/host/runtime.go:277-279`, rewriting `/ws/{telemetry,graph}` -> configured `/ws` target path), upgrading on `:8080` once `isReady()` is true (`503 NOT READY` pre-ready) while keeping `:8081`/`:8082` open for `ui/Caddyfile` (`:3000`) and Vite (`:5173`)** | ~75 Go LOC + integration & `ateam mutate` tests (`0` deps) | `internal/host/{ui.go,runtime.go,runtime_integration_test.go}`, `openspec/specs/host-runtime/spec.md` | Using `/ws/telemetry` or `/ws/graph` as client-side SPA page paths |
| **B. Serve `GET /bbops-endpoints.json` from `:8080` (or require Caddy in front of standalone `./bin/bbops`) so browser stores discover `:8081`/`:8082` URLs** | ~70 Go+TS LOC | `internal/host/ui.go`, `ui/src/lib/stores/`, `+page.svelte` | Single-port `./bin/bbops` (`:8080`) working standalone without Caddy or 3 exposed TCP ports |

Adopter seam: `+page.svelte` calls `telemetryStore.connect()` and `skgStore.connect()` with zero arguments across Vite (`:5173`), `ui/Caddyfile` (`:3000`), and standalone `./bin/bbops` (`:8080`). Under A, both standalone `:8080` and optional Caddy multi-service deployments work identically out of the box; under B, standalone `./bin/bbops` requires extra ports or Caddy to stream telemetry. **Ruled Option A, preserving `ui/Caddyfile` compatibility (owner, 2026-11-24).**

## Risks / Trade-offs

- **Headless `jsdom` Canvas/WebGL/Worker absence:** Pure projection, kinematics, badge, and `graphology-adapter` logic lives in `.ts` modules tested directly against independent oracles; `<canvas>`, `Sigma`, and `FA2Layout` worker mounts guard missing browser WebGL/Worker APIs cleanly.
- **Global hotkey collisions:** Window `keydown` handlers ignore `ctrlKey`/`metaKey`/`altKey` chords and editable targets (`INPUT`, `TEXTAREA`, `SELECT`, `isContentEditable`).

## Migration Plan

Register `C360Studio/semstreams-ui` (`MIT`) in Tier-1 provenance docs, add `ui/src/lib/{viewport,hud,skg,components}/` + `Sigma` npm deps, mount the COP workspace in `ui/src/routes/+page.svelte` (`bootstrap.ts` preserved), and wire `/ws/{telemetry,graph}` proxying in `internal/host/`. Rollback is reverting the merge commit.

## Open Questions

1. **Resolved (`OQ1`–`OQ4`, owner 2026-11-24):** `D1` Option A (dual-frame `WorldPoint` + `NedGridAdapter` tangent-plane `lla` projection + `.viewport-surface` vs. upper 2D `rAF` safety-HUD split), `D2` Option A (pure `view-model.ts` + 2D `rAF` horizon/heading canvas + `0.15 m/s` crab-angle deadband + vehicle-only `AuthorityBanner` vs. contact provenance card), `D3` Option A adapted from `C360Studio/semstreams-ui` (`SigmaCanvas.svelte` + `graphology-adapter.ts` + `sigma-layout.ts` `@ 3814b3d59dab7136011429cad99a8148aeceb0cc`, MIT, with non-safety palette and Tier-1 `PROVENANCE.md` registration), and `D4` Option A (Go stdlib `httputil.NewSingleHostReverseProxy` on `:8080` for `/ws/telemetry` & `/ws/graph` gated on `isReady()`, preserving `:8081`/`:8082` for `ui/Caddyfile` and Vite).
