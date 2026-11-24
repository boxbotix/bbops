# Proposal: m2-4-workspace-modes
Approved-by:
Architect: gemini (gemini-4-carbon, low)

## Track

`design`: touches two capability specs (`openspec/specs/cop-ui/spec.md` and `openspec/specs/host-runtime/spec.md`), adds capability `Requirement`s across UI workspaces and the read-only `POST /graph-gateway/search` host bridge, extends exported viewport state/URL-hash verbs (`ui/src/lib/stores/viewport.svelte.ts`) shared with MCP/director callers (`#19`), and defines the operator/engineering/analyst seams consumed by `#11` (`M3.1`), `#16`/`#18` (`M4.1`/`M4.3`), and `#19` (`M4.4`) (#36 / Epic #7).

## Why

`ui/src/routes/+page.svelte:81-119` currently renders every persona simultaneously inside a single `1fr + 360px` grid: `AuthorityBanner.svelte:24-68` mixes field-critical `P1`–`P4` `ControlAuthority` and `ARMED` badges with bench-level `0xB1` MCU timing counters (`loop_dt_us`, `max_loop_dt_us`, `rtt_us`, `crc_errors`), while the `360px` right rail stacks `VehicleHud.svelte:216-237` above `SkgPane.svelte:220-375` (`Sigma` graph, `Triple Inspector`, and `Transition Events` in competing scroll boxes) and leaves unanchored or off-screen vehicles with no persistent fleet roster. Before `#11` (`M3.1`) adds guarded actuation/tuning and `#19` (`M4.4`) adds SKG analyst queries, `bbops` needs a Lightroom-inspired **modal workspace stage over non-modal safety chrome**.

## What Changes

1. **Invariant Safety Topbar, Persistent `FleetFilmstrip` & Single-Route Workspace Verbs (`viewport.svelte.ts`, `hotkeys.ts`, `+page.svelte`):**
   - Extend `ViewportSnapshot` (`workspace`, `pilotLayout`, `lightsOut`) serialized in `#mode=&pilot=&chrome=` (defaults omitted so existing `#sel=` links stay identical), keeping stream health (`WS1`, `WS2 · synced`), a fleet-wide fault counter across all `skgStore.vehicles`, selected-entity core authority/arming/link (or isolated external-contact provenance), and `FleetFilmstrip` (`1`–`9` select) non-modal across `COP [G]`, `Pilot [D]`, `Bench [B]`, and `Cortex [K]` + `Lights-Out [Tab]`.
2. **`COP` (`[G]`) Solo-Mode Rail & `Pilot` (`[D]`) Map-vs-Split Swap (`CopWorkspace.svelte`, `PilotWorkspace.svelte`):**
   - `COP` pairs an ownship/external-contact roster, full-bleed `TacticalCanvas`, and a Lightroom-style Solo-Mode right accordion (one section open at a time). `Pilot` defaults to **Map-Primary** (chase-capable `TacticalCanvas` + floating corner `VehicleHud` PiP) with `[M]` swap to **50/50 Split**, auto-promoting abnormal ISA-101 health chips while withholding all vehicle authority/actuation surfaces on external contacts and exposing the `#11` (`M3.1`) guarded action slot seam.
3. **`Bench` (`[B]`) 20 Hz Telemetry Oscilloscope & Staged Tuning Rack (`ui/src/lib/bench/`, `BenchWorkspace.svelte`):**
   - Relocate `0xB1` MCU/trunk timing counters (`loop_dt_us`, `max_loop_dt_us`, `rtt_us`, `crc_errors`, `HealthFlags`) into `Bench`, pair a `300`-sample (`15 s @ 20 Hz`) typed-array `rAF` 2D stripchart oscilloscope with a staged parameter dials/sliders `TuningRack` seam (read-only diff/revert until `#11` wires ADR-111 TX), and dock the Lane 3 `STATUSTEXT`/transition console underneath.
4. **`Cortex` (`[K]`) SKG Surface & Read-Only Host `POST /graph-gateway/search` Bridge (`internal/host/`, `ui/src/lib/cortex/`, `CortexWorkspace.svelte`):**
   - Expose read-only `POST /graph-gateway/search` on the Go HTTP front door forwarding bounded `semengine` `graph.query.{globalSearch,localSearch,semantic,similar}` requests (gated on runtime readiness and surfacing `not_enabled` cleanly while `embedding_enabled: false`), paired in `Cortex` with ranked retrieval hits, a focused 2-hop `pathSearch` **Causal Evidence Subgraph** (`Sigma` with non-safety palette tokens), and `kvRevision`-cited `SkgTriple` provenance.

## Scope & Non-Goals

- **In-bounds:** `internal/host/{ui.go,search.go,runtime.go,runtime_integration_test.go}`, `ui/src/lib/{stores,viewport,hud,skg,bench,cortex,components}/`, `ui/src/routes/+page.svelte`, and `openspec/specs/{cop-ui,host-runtime}/spec.md`.
- **Non-goals:** Outbound `COMMAND_LONG` / teleop / `PARAM_SET` UDP actuation (`#11` `M3.1`), `internal/mavlink` ingest refactors (`#27`/`#29`), 2.5D/3D MapLibre globe (`#16` `M4.1`), sensor shaders (`#18` `M4.3`), and live LLM/MCP voice session brokering (`#19` `M4.4`).

## Definition of Done

- Owner approves `proposal.md` + `design.md` (`OQ1`–`OQ4` ruled Option A) and change is archived before ready-for-review.
- Attributed Go + Vitest tests (`// spec:` verified by `task test:spec`), URL-hash/ring-buffer property checks, and non-destructive mutation checks (`task mutate` + `cp`/SHA-256) pass with zero survivors.
- `npm --prefix ui run build` and `task verify` pass on a clean working tree.
