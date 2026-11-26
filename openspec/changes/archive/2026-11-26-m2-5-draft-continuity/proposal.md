# Proposal: m2-5-draft-continuity
Approved-by: coby (2026-11-26) sha256:172cc4d056c1
Architect: gemini (gemini-4-carbon, low)

## Track
`design` — adds `Requirement: Session-scoped Diagnostics parameter draft and Analysis investigation continuity across workspace switches` and modifies `openspec/specs/cop-ui/spec.md` across `ui/src/lib/bench/`, `ui/src/lib/cortex/`, `ui/src/routes/+page.svelte`, workspace components, Vitest suites, and Playwright E2E gates (`#40`, `#44` Position 4, prerequisite for `#43`).

## Why
- `WorkspaceShell.svelte:349-383` unmounts inactive stages on workspace switches (`{#if viewportStore.workspace === ...}`); while `traceBook` (`+page.svelte:39,103`, `WorkspaceShell.svelte:56,372`) survives at shell scope, `BenchWorkspace.svelte:57,80-98` holds `draftsByEntityId` locally (without recording per-entity baseline/domain) and resets staged edits (`Heading P Gain` `3.30 -> 3.20`, `0 staged`) on every round trip.
- When `viewportStore.selectedEntityId` (`viewport.svelte.ts:357,364-372`) references a deleted/unknown or `generic` entity, `BenchWorkspace.svelte:606-617` shows a generic selection prompt instead of explaining why that entity is unavailable/non-commandable.
- `CortexWorkspace.svelte:93-104,119-123,589-592` holds `queryText`, `activeTier`, `rawHits`, `pathResults`, banners, and request controllers locally, wiping entered queries, completed results, and 2-hop inspection state on workspace switch and exposing no provenance distinction when `viewportStore.selectedEntityId` changes in another workspace.

## What Changes
- Introduce a shell-owned `DiagnosticsDraftStore` (`ui/src/lib/bench/tuning-rack.ts`, `+page.svelte`, `WorkspaceShell.svelte`) keyed strictly by canonical 6-part UXS vehicle ID (`parseVehicleEntityId`, `uxs.ts:115-129`) that preserves per-vehicle `{ domain, baseline, staged }` across workspace/entity switches, isolates per-vehicle `Revert Draft`, never triggers `onApplyTuning` on navigation, and explains removed/poisoned/non-commandable states (`OQ1`).
- Introduce a shell-owned `AnalysisSessionStore` (`ui/src/lib/cortex/session.svelte.ts`, `+page.svelte`, `WorkspaceShell.svelte`) preserving `queryText`, `activeTier`, `status` (`idle | running | completed | cancelled | error`), ranked hits, 2-hop `pathResults`, result provenance (`submittedQuery`, `submittedTier`, `mode`, `scopeEntityId`), and inspection context (`OQ2`, `OQ3`) with monotonic request-sequence stale-result refusal.
- Add Vitest unit/component + seeded reference-model history checks, non-destructive `cp` + SHA-256 mutation checks, and Chromium Playwright E2E round-trip regressions (`#40`).

## Decisions you are making by approving
- **`OQ1` (`Diagnostics` drafts across `poisoned` / `delete` transitions):** Retain dormant per-vehicle local drafts across transient `poisoned`/`delete` states (restoring only on matching canonical `entityId` + `domain`, with a withheld dormant-draft notice and discard button) vs. purge a vehicle's draft immediately on `poisoned` or `delete`.
- **`OQ2` (In-flight `Analysis` searches on workspace switch):** Keep in-flight searches running in `AnalysisSessionStore` so returning shows `running` or `completed` without mutating `Fleet`/`Vehicle` selection while away (plus explicit `Cancel`) vs. abort in-flight requests on leaving `Analysis` and show a defined `cancelled` state on return.
- **`OQ3` (`Analysis` inspection context vs. global selection changes):** Pin `Analysis`'s `inspectedEntityId` (citations + 2-hop subgraph) across external selection changes with a provenance mismatch badge + one-click sync to current selection vs. always bind citations to `viewportStore.selectedEntity` while flagging earlier-selection result provenance.

## Scope & Non-Goals
- **In scope:** `openspec/specs/cop-ui/spec.md`, `ui/src/lib/bench/{tuning-rack.ts,bench.test.ts}`, `ui/src/lib/cortex/{session.svelte.ts,view-model.ts,cortex.test.ts}`, `ui/src/routes/+page.svelte`, `ui/src/lib/components/{WorkspaceShell,BenchWorkspace,CortexWorkspace}.svelte`, `ui/src/lib/components/workspaces.test.ts`, and `ui/e2e/cop-workspace.e2e.ts`.
- **Non-goals:** Persistence across browser reloads (`localStorage`/IndexedDB), cross-device draft sync, multi-step undo/redo stacks, parameter provenance copy overhaul (`#43`), or `M3.1` parameter write actuation (`#11`).

## Definition of Done
- Owner rules on `OQ1`–`OQ3`, fills `Approved-by: <name> (<YYYY-MM-DD>) sha256:<12 hex>`, and removes the `OPEN-Q` hold in `tasks.md`.
- Seeded model-based history checks (multi-vehicle edit/switch/revert/poison/delete + out-of-order search completions), non-destructive `cp` + SHA-256 mutation checks, and Playwright browser regressions pass with `// spec:` citations.
- `openspec/changes/m2-5-draft-continuity/` is archived, `GOTOOLCHAIN=auto task verify` passes on a clean tree, and cross-provider review approves before squash merge.
