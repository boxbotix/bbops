# Proposal: m2-5-compact-layout
Approved-by: coby (2026-11-26) sha256:a049e40d5a05
Architect: gemini (gemini-4-carbon, low)

## Track
`design` — adds `Requirement: Compact desktop viewport layout, persistent safety header, and non-modal Fleet panel controls` and updates `openspec/specs/cop-ui/spec.md` across `WorkspaceShell.svelte`, `CopWorkspace.svelte`, `FleetFilmstrip.svelte`, `AuthorityBanner.svelte`, shared layout styles, Vitest suites, and Playwright E2E (`#42`, `#44` Position 6, building on `#40` continuity and `#41` keyboard/focus conventions).

## Why
- At `1024×768`, `@media (max-width: 1100px)` (`CopWorkspace.svelte:731,1010-1014`) collapses `.cop-stage` (`220px minmax(0, 1fr) 310px`) to a single stacked column above `.cop-canvas-wrap` (`min-height: 460px`, `754-759`), pushing `.cop-solo-rail` to `y ≈ 739` and `FleetFilmstrip` (`WorkspaceShell.svelte:434-440`) to `y ≈ 955` so operators must scroll the whole page to connect vehicle selection with its identity, authority, arming, and freshness details.
- `.workspace-shell` (`WorkspaceShell.svelte:444-452`) uses unbounded `min-height: 100vh` with static `.invariant-topbar` (`454-464`) and unbounded `.cop-roster` / `.cop-solo-rail` heights (`CopWorkspace.svelte:740-752,870-881`), so long rosters, active fault chips, or SKG triples scroll global safety status off-screen while both `.cop-roster` and `FleetFilmstrip` permanently consume compact space.
- Long entity/contact titles, source URIs, and multi-fault chips lack width/truncation guards across `AuthorityBanner.svelte:35-79,130-144`, `CopWorkspace.svelte:455-493,556-589,633-660`, and `FleetFilmstrip.svelte:71-74,114-127`, risking horizontal page overflow or clipped controls at `1024×768`, `1440×900`, and `200%` zoom.

## What Changes
- Establish a compact desktop `Fleet` layout at `1024×768` and `1440×900` (`100%` zoom) using non-modal collapsible/bounded `Roster` and `Inspector` panels alongside `TacticalCanvas` and a reachable vehicle switcher so selecting a vehicle reveals identity, `ControlAuthority`, `ARMED`/`DISARMED`, and freshness without whole-page scrolling (`OQ1`).
- Keep global safety status (`invariant-safety-topbar`) visible during long roster/detail scrolling without obscuring `:focus-visible` targets, bound internal panel scrolling at desktop sizes, and provide a deliberate responsive scroll fallback at `200%` zoom without control clipping or horizontal page overflow (`OQ2`).
- Add keyboard-accessible non-modal panel open/close toggles (`aria-expanded`, `aria-controls`), deterministic focus return + route back to `TacticalCanvas`, `#40`/`#41` selection and session continuity across panel toggles/resizes (`OQ3`), and truncation/wrapping guards for long names, multi-fault lists, and empty states.

## Decisions you are making by approving
- **`OQ1` (`Fleet` compact panel topology & switcher deduplication at `1024×768` vs. `1440×900`):** Keep side-by-side `TacticalCanvas` + `Inspector` at `1024×768` with `Roster` collapsed by default behind a non-modal disclosure toggle while `FleetFilmstrip` serves as the compact switcher (`1440×900` defaults both rails open) (Option A) vs. keep `Roster` docked left at `1024×768`, hide `FleetFilmstrip` in `Fleet` while `Roster` is open, and float `Inspector` as a non-modal right overlay drawer over `TacticalCanvas` (Option B).
- **`OQ2` (Bounded desktop shell vs. sticky chrome & `200%` zoom responsive fallback):** Bound `.workspace-shell` to `100dvh` with internal panel `overflow-y: auto` at `>= 900px × 700px` and fall back to responsive page scrolling (`height: auto; min-height: 100dvh`) with sticky topbar + `ResizeObserver`-synced `--shell-topbar-offset` `scroll-padding-top` below `900px × 700px` / `200%` zoom (Option A) vs. keep document-level scrolling everywhere with `position: sticky` topbar + bottom `FleetFilmstrip` and `max-height`-capped panels (Option B).
- **`OQ3` (Non-modal panel keyboard/focus-return contract & state persistence):** Use non-modal disclosure toggles (`aria-expanded`), return focus to the panel toggle when closing a focused panel (plus scoped `Escape` close when focus is inside a collapsible drawer before camera untether and an explicit `Focus Map` control), and keep manual panel overrides in session state (Option A) vs. persist panel open/close flags in `ViewportStore` / `#...` URL hashes and omit `Escape` panel close (Option B).

## Scope & Non-Goals
- **In scope:** `openspec/specs/cop-ui/spec.md`, `ui/src/styles/colors.css`, `ui/src/lib/hud/{view-model.ts,view-model.test.ts}`, `ui/src/lib/components/{WorkspaceShell,CopWorkspace,FleetFilmstrip,AuthorityBanner,PilotWorkspace,BenchWorkspace,CortexWorkspace}.svelte`, `ui/src/lib/components/workspaces.test.ts`, and `ui/e2e/cop-workspace.e2e.ts`.
- **Non-goals:** Phone-specific operating UI, map-engine replacement (`#16`), wholesale visual redesign, or `M3.1` operator command actuation (`#11`).

## Definition of Done
- Owner rules on `OQ1`–`OQ3`, fills `Approved-by: <name> (<YYYY-MM-DD>) sha256:<12 hex>`, and removes the approval hold in `tasks.md`.
- Spec-cited Vitest unit/component + reference-model layout/panel history tests, non-destructive `cp` + SHA-256 mutation checks, and Chromium Playwright E2E checks (`1024×768`, `1440×900`, `200%` zoom, populated/faulted/empty/long-name fixtures, panel toggles, focus return, and `#40` continuity) pass.
- `openspec/changes/m2-5-compact-layout/` is archived, `GOTOOLCHAIN=auto task verify` passes on a clean tree, and independent cross-provider review approves before squash merge.
