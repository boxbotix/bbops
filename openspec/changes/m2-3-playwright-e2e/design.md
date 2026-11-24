# Design: m2-3-playwright-e2e

Architect: `gemini (gemini-4-carbon, low)`

## Context

- `ui/package.json:6-45` & `ui/vite.config.ts:39-43`: Vitest `^3.2.4` runs `src/**/*.{test,spec}.{js,ts}` in `jsdom`; zero Playwright or headless browser dependencies exist (`git log -S 'playwright'` returns empty).
- `.agents/protocol.md:30-45` & `Taskfile.yml:42-47,72-76,82-99`: `ateam test-spec` scans `*.{test,spec}.{ts,tsx,js,jsx}` as Vitest ASTs and states *"other frameworks are outside this gate; adopting projects must provide appropriate attribution enforcement for other frameworks and languages"*.
- `cmd/bbops/main.go:36-71,83-108` & `internal/host/runtime.go:316-407`: `bbops` supports `-http-port`, `-telemetry-ws-port`, `-graph-ws-port`, `-metrics-port -1`, `-mavlink-udp-port`, `-bind-host 127.0.0.1`, `-data-dir`, and `-ui-dir`, while `host.StartEphemeral` allocates collision-free loopback TCP/UDP ports (`1024..65535`).
- `internal/host/runtime_integration_test.go:211-228,1038-1282`: Go integration tests already encode `0xB1` MAVLink v2 UDP frames via `internal/mavcodec` and publish `graph.Graphable` records onto `entity.>` via `rt.NATSClient().PublishToStream`, while testing only a synthetic one-line `index.html` (`:115-118`).
- `ui/src/routes/+page.svelte:42-119,172-191`: mounts `AuthorityBanner`, `TacticalCanvas`, `VehicleHud`, and `SkgPane` in a `minmax(0, 1fr) 360px` grid, auto-connecting `/ws/telemetry` and `/ws/graph` and binding `window` `keydown` / `hashchange`.
- `ui/src/lib/components/SkgPane.svelte:80-95,151-185,289-313,493-496,530-533`: `probeWebGLSupport()` disables `Sigma` in `jsdom`; in real WebGL, `Sigma` uses default `#ffffff` hover/highlight label pills against `labelColor: { color: "#e2e8f0" }` (white-on-white) without fit padding, and `.table-scroll` (`max-height: 170px`) cramps the 23-row triple table.
- `ui/src/lib/components/VehicleHud.svelte:160-177,194-203`, `ui/src/lib/hud/view-model.ts:32-36,156-160`, & `ui/src/lib/viewport/ned-grid.ts:377-412`: `drawHorizonAndTape` skips 2D context in `jsdom`, draws `GS/VS/ALT` (`x=10`) and `R/P/Y` (`widthPx - 10`) on the same `bottomY` baseline where 2–3 digit numbers collide at `342px` width, and omits `computeCrabAngleVectors` signed degrees on the helm HUD.
- `ui/src/lib/components/TacticalCanvas.svelte:204-215,596-610,788-792`: skips 2D context in `jsdom`; `.viewport-toolbar` (`top: 10px`) overlaps northern range-ring text drawn at `(cx + 6, cy - rPx + 13)`.

## Goals / Non-Goals

- **Goals:** Establish a deterministic ephemeral-port Playwright Chromium E2E harness exercising compiled `ui/build` + `bbops` + loopback `0xB1` MAVLink UDP + `ENTITY_STATES` external-contact fixtures, enforce `// spec:` citations on E2E tests without breaking `ateam test-spec`, wire `task test:e2e`, and fix the four real-browser rendering/layout bugs uncovered in live `M2.2` inspection.
- **Non-Goals:** Command/teleop TX (`#11`), multi-minute swarm soak or `bbstack` SITL qualification (`#12`), `internal/mavlink` link-loss/flush refactors (`#27`/`#29`), and 2.5D/3D MapLibre globe (`#16`).

## Decisions

### D1. Full-Stack E2E Host & Loopback `0xB1` Telemetry Fixture Seam (`ui/e2e/`, `internal/`)

| Option | Cost | Blast radius | Forecloses |
| --- | --- | --- | --- |
| **A. Small Go E2E harness helper (`internal/e2eharness` + CLI entrypoint invoked by Playwright worker fixture) reusing `host.StartEphemeral` (`runtime.go:316-351`), `mavcodec.EncodeFrame` (`0xB1` `#0/#1/#31/#32/#33/#253`), and `rt.NATSClient().PublishToStream` (external-contact + `pathSearch` edge seeding), printing resolved `{ baseURL, udpAddr, platform }` JSON on stdout and accepting scenario triggers (`pulseCrsfOverride`, `emitStatusText`, `upsertExternalContact`)** | ~160 Go LOC + ~75 TS LOC | `internal/e2eharness/`, `ui/e2e/` | Running Playwright E2E without a Go toolchain in `PATH` |
| **B. Pure TypeScript Playwright fixture spawning `./bin/bbops` (`cmd/bbops/main.go:36-71`) on Node-probed ephemeral ports and re-implementing `0xFD` MAVLink v2 framing/CRC + `0xB1` `custom_mode` packing in `ui/e2e/mavlink-fixture.ts` (`node:dgram`), with no direct NATS `ENTITY_STATES` external-contact injection** | ~190 TS LOC | `ui/e2e/` | Seeding non-MAVLink external COP contacts (`cop.*`) or multi-hop SKG edges into `ENTITY_STATES` during E2E before `M4.2` (`#17`/`#21`) HTTP/CoT ingest exists |

Adopter seam: Playwright tests request a typed `copStack` fixture (`{ page, baseURL, vehicles, triggerCrsfEdge(), seedExternalContact() }`). Under A, new `0xB1` or `uxsvocab` fields stay single-sourced in Go (`internal/mavcodec`, `internal/uxsvocab`) and `#11` (`M3.1`) can assert outbound `#76`/`#84` UDP packets received by the Go peer socket; under B, TypeScript duplicates binary MAVLink framing and cannot seed `cop.*` external contacts until `#17`.

### D2. Playwright File Naming, `test:spec` Attribution & `Taskfile.yml` Gate Placement (`Taskfile.yml`, `ui/e2e/`)

| Option | Cost | Blast radius | Forecloses |
| --- | --- | --- | --- |
| **A. Name Playwright suites `ui/e2e/*.e2e.ts` (disjoint from Vitest's `src/**/*.{test,spec}.{js,ts}` and `ateam test-spec`'s Vitest-only `*.{test,spec}.ts` scanner), validate `// spec:` citations on `ui/e2e/*.e2e.ts` via a ~65-line TypeScript AST script chained in `task test:spec`, and include `task test:e2e` inside `task verify`** | ~65 TS LOC + ~15 YAML LOC | `Taskfile.yml`, `ui/package.json`, `ui/e2e/` | Running `task verify` on a machine where `npx playwright install chromium` has not been run once |
| **B. Same `ui/e2e/*.e2e.ts` naming and `// spec:` checker in `task test:spec`, while keeping `task test:e2e` as a separate `Taskfile.yml` gate (required in `preflight` for `ui/` / `internal/host/` diffs and Phase-2 CI, excluded from `task verify`)** | ~65 TS LOC + ~12 YAML LOC | `Taskfile.yml`, `.agents/skills/preflight/SKILL.md`, `ui/e2e/` | Catching real-browser regressions automatically when a developer runs only `task verify` |

Adopter seam: Every `test(...)` in `ui/e2e/*.e2e.ts` carries an immediately preceding `// spec: <capability> / <exact title>` comment resolved against `openspec/specs/*/spec.md` (or active `openspec/changes/*/specs/*/spec.md`). If a developer writes an uncited or dangling `ui/e2e/*.e2e.ts` test, `task test:spec` fails closed under both A and B; A additionally runs Chromium inside `task verify`, whereas B keeps `task verify` fast/offline-friendly at the risk of skipping `task test:e2e` unless invoked explicitly.

### D3. Real-Browser HUD Readout Layout, `Sigma` Highlight Contrast & Viewport Clearance (`ui/src/lib/components/`)

| Option | Cost | Blast radius | Forecloses |
| --- | --- | --- | --- |
| **A. Keep `VehicleHud` telemetry inside the 2D `rAF` `<canvas>` (`VehicleHud.svelte:160-177`) split across two non-overlapping footer rows (`GS / VS / ALT` + optional USV `CRAB ±N°` on row 1; `R / P / Y` on row 2), custom dark-pill `defaultDrawNodeHover` + camera fit padding in `SkgPane.svelte:151-185`, toolbar-safe range-ring label offset in `TacticalCanvas.svelte:204-215`, and flex-grow `.inspector-card` in `SkgPane.svelte:385-496`** | ~85 Svelte/TS LOC | `VehicleHud.svelte`, `SkgPane.svelte`, `TacticalCanvas.svelte`, `view-model.ts` | Querying `GS`/`ALT`/`CRAB` text directly via DOM selectors in Playwright (requires view-model + canvas pixel/layout checks) |
| **B. Render `GS`, `VS`, `ALT`, `R/P/Y`, and USV `CRAB` in a `4 Hz`-throttled semantic HTML footer strip below the `60 fps` horizon `<canvas>` in `VehicleHud.svelte` (`0` `20 Hz` DOM thrash), paired with the same dark `Sigma` highlight pill/fit padding, toolbar-safe range-ring labels, and flex `.inspector-card` sizing** | ~105 Svelte/TS LOC | `VehicleHud.svelte`, `SkgPane.svelte`, `TacticalCanvas.svelte`, `view-model.ts` | Purely canvas-drawn numeric footer inside `VehicleHud` |

Adopter seam: `#11` (`M3.1`) mounts `ControlPanel.svelte` in the inspector column alongside `VehicleHud` and `SkgPane`. Under A, all `VehicleHud` metrics paint in `rAF` without DOM nodes; under B, the horizon/tape stays `60 fps` `rAF` while numeric readouts become CSS-wrapped DOM cells testable via Playwright `getByTestId` bounding boxes (`boundingBox()` overlap == `0`) without violating Epic `#7`'s zero-`20 Hz`-DOM-thrash guardrail.

## Risks / Trade-offs

- **Headless WebGL in CI/macOS/Linux:** Chromium supports WebGL via SwiftShader/ANGLE (`--use-gl=angle --use-angle=swiftshader` fallback when GPU is absent) so `Sigma` v3 mounts deterministically in headless runs without falling back to `jsdom` list mode.
- **Port & temp-dir hygiene:** Ephemeral ports (`127.0.0.1:0`) and per-worker `os.MkdirTemp` / `fs.mkdtemp` ensure parallel E2E runs never touch `.bbops/data`, `:8080`, or `:14550` or dirty `git status`.

## Migration Plan

Land after `#9` review follow-up (`PR #31`) merges to `main` so edits to `SkgPane.svelte`, `VehicleHud.svelte`, `TacticalCanvas.svelte`, and `internal/host/` apply cleanly without rebase conflicts. Rollback is reverting the merge commit.

## Open Questions

1. **OQ1 (`D1`) [Needs owner ruling]:** Prefer **Option A** (Go `internal/e2eharness` reusing `host.StartEphemeral` + `mavcodec` + `NATSClient` external-contact/edge seeding) or **Option B** (pure TypeScript `./bin/bbops` spawner + TS MAVLink UDP encoder)?
2. **OQ2 (`D2`) [Needs owner ruling]:** Prefer **Option A** (`task test:e2e` included inside `task verify`) or **Option B** (`task test:e2e` standalone + CI/preflight gate, keeping `task verify` browser-binary-free), with `ui/e2e/*.e2e.ts` `// spec:` attribution enforced in `task test:spec`?
3. **OQ3 (`D3`) [Needs owner ruling]:** Prefer **Option A** (two-row canvas-drawn `VehicleHud` footer) or **Option B** (`4 Hz`-throttled HTML `VehicleHud` readout strip beneath the `60 fps` horizon canvas for CSS wrapping + DOM bounding-box E2E assertions), alongside dark `Sigma` highlight pills, range-ring toolbar clearance, and flex inspector sizing?
