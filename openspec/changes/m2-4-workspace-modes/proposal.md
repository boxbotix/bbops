# Proposal: m2-4-workspace-modes
Approved-by:
Architect: gemini (gemini-4-carbon, low)

## Track

`design`: adds a capability `Requirement` in `openspec/specs/cop-ui/spec.md`, extends exported viewport state/URL-hash verbs (`ui/src/lib/stores/viewport.svelte.ts`) shared with MCP/director callers (`#19`), defines the operator/engineering/analyst workspace seams consumed by `#11` (`M3.1`), `#16`/`#18` (`M4.1`/`M4.3`), and `#19` (`M4.4`), and evaluates an optional read-only `semengine` `graph.query.*` host search bridge in `internal/host/` (#36 / Epic #7).

## Why

`ui/src/routes/+page.svelte:81-119` currently renders every persona simultaneously inside a single `1fr + 360px` grid: `AuthorityBanner.svelte:24-68` mixes field-critical `P1`–`P4` `ControlAuthority` and `ARMED` badges with bench-level `0xB1` MCU timing counters (`loop_dt_us`, `max_loop_dt_us`, `rtt_us`, `crc_errors`), while the `360px` right rail stacks `VehicleHud.svelte:216-237` above `SkgPane.svelte:220-375` (`Sigma` graph, `Triple Inspector`, and `Transition Events` in competing scroll boxes) and leaves unanchored or off-screen vehicles with no persistent fleet roster. Before `#11` (`M3.1`) adds guarded actuation/tuning and `#19` (`M4.4`) adds SKG analyst queries, `bbops` needs a Lightroom-inspired **modal workspace stage over non-modal safety chrome**.

## What Changes

1. **Invariant Non-Modal Safety Topbar, Persistent `FleetFilmstrip` & Workspace Controller Verbs (`viewport.svelte.ts`, `hotkeys.ts`, `+page.svelte`):**
   - Keep stream health (`WS1`, `WS2 · synced`), a fleet-wide fault counter across all `skgStore.vehicles`, selected-entity core authority/arming/link (or isolated external-contact provenance), and a persistent bottom `FleetFilmstrip` (`1`–`9` entity select) invariant across modes, while switching the main stage across four workspaces (`COP [G]`, `Pilot [D]`, `Bench [B]`, `Cortex [K]`) + `Lights-Out [Tab]` synchronized to `#...` URL hash state.
2. **`COP` (`[G]`) & `Pilot` (`[D]`) Operational Workspaces (`CopWorkspace.svelte`, `PilotWorkspace.svelte`):**
   - `COP` pairs an ownship/external-contact roster, full-bleed `TacticalCanvas`, and a Lightroom-style Solo-Mode right rail (one expanded section at a time). `Pilot` swaps (`[M]`) between **Map-Primary** (chase-capable `TacticalCanvas` + floating corner `VehicleHud` PiP) and **50/50 Split** (`VehicleHud` PFD + chase radar), promoting abnormal ISA-101 health faults while withholding all vehicle authority/actuation surfaces on external contacts and exposing the `#11` (`M3.1`) guarded action seam.
3. **`Bench` (`[B]`) 20 Hz Telemetry Oscilloscope & Staged Tuning Rack (`ui/src/lib/bench/`, `BenchWorkspace.svelte`):**
   - Relocate `0xB1` MCU/trunk timing counters (`loop_dt_us`, `max_loop_dt_us`, `rtt_us`, `crc_errors`, `HealthFlags`) into `Bench`, pair a bounded zero-DOM-thrash `rAF` 20 Hz kinematic stripchart scope with a staged parameter dials/sliders rack seam (read-only diff preview until `#11` wires ADR-111 TX), and dock the Lane 3 `STATUSTEXT`/transition console underneath.
4. **`Cortex` (`[K]`) Lexical, Semantic & Neural SKG Surface (`ui/src/lib/cortex/`, `CortexWorkspace.svelte`):**
   - Re-center SKG visualization around retrieval and provenance: a unified query bar (`Lexical` · `Semantic` · `Neural`), ranked entity/predicate hits, a focused 2-hop `pathSearch` **Causal Evidence Subgraph** (`Sigma` with non-safety palette tokens), and `kvRevision`-cited triple provenance ready for `#19` (`M4.4`).

## Scope & Non-Goals

- **In-bounds:** `ui/src/lib/{stores,viewport,hud,skg,bench,cortex,components}/`, `ui/src/routes/+page.svelte`, `openspec/specs/cop-ui/spec.md`, and (if `OQ4` selects Option A) read-only `internal/host/` `graph.query.*` HTTP search bridging + `openspec/specs/host-runtime/spec.md`.
- **Non-goals:** Outbound `COMMAND_LONG` / teleop / `PARAM_SET` UDP actuation (`#11` `M3.1`), `internal/mavlink` ingest refactors (`#27`/`#29`), 2.5D/3D MapLibre globe (`#16` `M4.1`), sensor shaders (`#18` `M4.3`), and live LLM/MCP voice session brokering (`#19` `M4.4`).

## Definition of Done

- Owner approves `proposal.md` + `design.md` (`OQ1`–`OQ4` ruled) and change is archived before ready-for-review.
- Attributed Vitest (and Go, if `OQ4` Option A) tests (`// spec:` verified by `task test:spec`), URL-hash/ring-buffer property checks, and non-destructive mutation checks (`cp` + SHA-256 / `task mutate`) pass with zero survivors.
- `npm --prefix ui run build` and `task verify` pass on a clean working tree.
