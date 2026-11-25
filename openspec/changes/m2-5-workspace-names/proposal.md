# Proposal: m2-5-workspace-names
Approved-by:
Architect: gemini (gemini-4-carbon, low)

## Track

`quick`: updates visible workspace destination labels (`Fleet`, `Vehicle`, `Diagnostics`, `Analysis`) within a single capability (`openspec/specs/cop-ui/spec.md` `MODIFIED` requirements only), adds zero new `Requirement`s, preserves all internal `WorkspaceMode` identifiers and URL hash tokens (`cop`, `pilot`, `bench`, `cortex`), and touches no exported adopter surface, boot path, durability/ack behavior, or gate file (#38 / Epic #44 / #7).

## Why

`WorkspaceShell.svelte:70-79` currently labels the four workspace tabs `COP` (`G`), `Pilot` (`D`), `Bench` (`B`), and `Cortex` (`K`), mixing an acronym, an operator role, a physical bench metaphor, and a brain metaphor across navigation, headings (`PilotWorkspace.svelte:120-125`), cross-workspace jump actions (`CopWorkspace.svelte:303-325,363-370,439-446`), and accessible control/error strings (`CopWorkspace.svelte:226-228`, `CortexWorkspace.svelte:378,606`). During the 2026-11-25 UX review (`#38`, Position 1 in `#44` `M2.5`), the owner approved `Fleet`, `Vehicle`, `Diagnostics`, and `Analysis` as consistent destination names while keeping vehicle operating modes, domain `COP` / `Bench` (`direct-bench`) terms, and shareable URL values intact.

## What Changes

1. **Workspace Navigation & Stage Headings (`WorkspaceShell.svelte`, `PilotWorkspace.svelte`):**
   - Rename visible tab labels in `WORKSPACE_TABS` (`WorkspaceShell.svelte:75-78`) to `Fleet`, `Vehicle`, `Diagnostics`, and `Analysis` in existing order while preserving `mode` (`cop`, `pilot`, `bench`, `cortex`), `data-testid="workspace-tab-{mode}"`, shortcuts (`G`, `D`, `B`, `K`), and `#mode=` URL hashes; update `PILOT STAGE` (`PilotWorkspace.svelte:121`) to `VEHICLE WORKSPACE` so destination views stay distinct from vehicle operating modes.
2. **Cross-Workspace Links, Accessible Labels & Error Copy (`CopWorkspace.svelte`, `CortexWorkspace.svelte`):**
   - Update cross-workspace jump buttons (`CopWorkspace.svelte:309,316,323,368,444`) to `Vehicle [D]`, `Diagnostics [B]`, `Analysis [K]`, `Inspect Provenance in Analysis [K]`, and `Open Analysis [K]`, and align workspace-specific `aria-label` / fallback error strings (`CopWorkspace.svelte:226,228`, `CortexWorkspace.svelte:378,606`) without altering legitimate domain `COP` (Common Operating Picture / external COP contacts) or `0xB1` `Bench` (`direct-bench` `LinkTier`) terminology.
3. **Tests, Spec Wording & Baseline Viewport Legibility (`workspaces.test.ts`, `cortex.test.ts`, `cop-workspace.e2e.ts`, `openspec/specs/cop-ui/spec.md`):**
   - Update label-dependent Vitest and Playwright assertions, verify all four workspace tabs render without horizontal clipping or overlap at `1024×768` and `1440×900` in Chromium, and sync `openspec/specs/cop-ui/spec.md` wording without touching `openspec/changes/archive/`.

## Scope & Non-Goals

- **In-bounds:** `ui/src/lib/components/{WorkspaceShell,CopWorkspace,PilotWorkspace,CortexWorkspace}.svelte`, `ui/src/lib/{components/workspaces.test.ts,cortex/cortex.test.ts,viewport/hotkeys.ts}`, `ui/e2e/cop-workspace.e2e.ts`, and `openspec/specs/cop-ui/spec.md`.
- **Non-goals:** Layout redesign (`#42`), fleet health/freshness logic (`#39`), `Tab`/ARIA tablist keyboard rework (`#41`), session draft persistence (`#40`), parameter provenance (`#43`), URL token migration, component file renames, or rewriting archived OpenSpec records.

## Definition of Done

- Owner approves `proposal.md` (`Approved-by:` digest populated and `Hold: OPEN-Q` cleared) and change is archived before ready-for-review.
- Attributed Vitest + Playwright Chromium tests (`// spec:`) verify `Fleet`, `Vehicle`, `Diagnostics`, and `Analysis` navigation, cross-workspace links, accessible names, preserved `#mode=cop|pilot|bench|cortex` URLs/shortcuts, and unclipped tab rendering at `1024×768` and `1440×900`, backed by non-destructive `cp` + SHA-256 mutation checks.
- `npm --prefix ui run build` and `GOTOOLCHAIN=auto task verify` pass on a clean working tree.
