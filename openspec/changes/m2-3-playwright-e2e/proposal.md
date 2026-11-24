# Proposal: m2-3-playwright-e2e
Approved-by:
Architect: gemini (gemini-4-carbon, low)

## Track

`design`: adds a new capability `Requirement` in `openspec/specs/cop-ui/spec.md`, touches verification gate configuration in `Taskfile.yml` (`test:spec`, `test:e2e`, `verify`), introduces `@playwright/test` (`Apache-2.0`) in `ui/package.json` + `THIRD_PARTY_NOTICES`, and defines the full-stack loopback `0xB1` browser E2E seam ahead of `M3.1` actuation (#34 / Epic #7).

## Why

`M2.1` (#8) and `M2.2` (#9) test pure TypeScript store, projection, attitude, and `graphology` modules under Vitest `jsdom` (`ui/vite.config.ts:39-43`), while `TacticalCanvas.svelte:596-610`, `VehicleHud.svelte:194-203`, and `SkgPane.svelte:80-95` explicitly bypass Canvas2D rasterization, WebGL `Sigma` v3, and `ForceAtlas2` Web Workers under `jsdom`, and `ui/src/routes/+page.svelte:18-120` has zero real-browser mount or Go-to-Chromium E2E coverage.

Booting `./bin/bbops` with a live `20 Hz` synthetic `0xB1` swarm in a real browser immediately uncovered four defects invisible to `jsdom`: (1) white-on-white highlighted node pills and right-edge label clipping in `Sigma` v3 (`SkgPane.svelte:151-185`), (2) colliding `GS/VS/ALT` and `R/P/Y` footer strings at `342px` inspector width plus missing HUD USV crab-angle readout (`VehicleHud.svelte:160-177`), (3) `.viewport-toolbar` occluding the northernmost range-ring label (`TacticalCanvas.svelte:204-215,788-792`), and (4) fixed `max-height` cramping the 23-row `Triple Inspector` (`SkgPane.svelte:493-496`). Before `M3.1` (#11) adds `COMMAND_LONG` and teleop actuation, `bbops` needs deterministic headless Chromium E2E coverage and visual fixes.

## What Changes

1. **Playwright Full-Stack Chromium Harness & Go `internal/e2eharness` Fixture (`internal/e2eharness/`, `ui/e2e/`, `ui/playwright.config.ts`):**
   - Boot compiled `ui/build` + `host.StartEphemeral` on ephemeral loopback TCP/UDP ports with isolated JetStream temp directories via `internal/e2eharness`, streaming deterministic `0xB1` `Quadcopter`, `Rover`, and crabbing `SurfaceBoat` UDP telemetry (`internal/mavcodec`), Lane 3 `DirectCrsfOverride` / `STATUSTEXT` edges, and `NATSClient().PublishToStream`-seeded external COP contacts (`cop.*`) + multi-hop `pathSearch` edges.
2. **Test-to-Spec Attribution & `task verify` Gate Integration (`Taskfile.yml`, `ui/package.json`):**
   - Add `@playwright/test` (`Apache-2.0`, recorded in `THIRD_PARTY_NOTICES`), enforce `// spec:` AST attribution validation on `ui/e2e/*.e2e.ts` inside `task test:spec` (`.agents/protocol.md:43-45`), and run `task test:e2e` inside `task verify`.
3. **End-to-End COP Browser Suite & Real-Browser Layout Fixes (`ui/src/lib/components/`, `ui/e2e/`):**
   - Replace the canvas-drawn `VehicleHud` footer with a `4 Hz`-throttled semantic HTML readout strip (`GS`, `VS`, `ALT`, `R/P/Y`, and active USV signed `CRAB` degrees) beneath the `60 fps` horizon `<canvas>`, fix `Sigma` highlighted label dark-pill contrast/fit padding (`SkgPane.svelte`), clear `.viewport-toolbar` northern range-ring occlusion (`TacticalCanvas.svelte`), and flex-size `.inspector-card` (`SkgPane.svelte`), guarded by headless Chromium E2E assertions covering WS1/WS2 sync, Canvas2D pixel output, DOM `boundingBox()` non-overlap, pointer/hotkey `#sel=` sync, WebGL `Sigma` + `2-Hop` `pathSearch`, and external-contact UI isolation.

## Scope & Non-Goals

- **In-bounds:** `internal/e2eharness/`, `ui/package.json`, `ui/playwright.config.ts`, `ui/e2e/`, `ui/src/lib/{components,hud}/`, `Taskfile.yml`, `THIRD_PARTY_NOTICES`, and `openspec/specs/cop-ui/spec.md`.
- **Non-goals:** Vehicle command/teleop TX (`#11` `M3.1`), multi-minute swarm soak & `bbstack` SITL qualification (`#12` `M3.2`), `internal/mavlink` link-loss/flush refactors (`#27`/`#29`), and 2.5D/3D MapLibre globe (`#16` `M4.1`).

## Definition of Done

- Owner approves `proposal.md` + `design.md` (`OQ1`–`OQ3` ruled) and change is archived before ready-for-review.
- Playwright Chromium E2E suite passes deterministically on ephemeral loopback ports with zero `pageerror`s, verified `// spec:` citations, and non-destructive mutation checks (`task mutate` + `cp`/SHA-256) on `internal/e2eharness`, attribution validation, and HUD/canvas layout helpers.
- `task verify` (including `task test:e2e`) passes on a clean working tree (`THIRD_PARTY_NOTICES` updated for `@playwright/test`).
