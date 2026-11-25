# Proposal: m2-5-keyboard-navigation
Approved-by: coby (2026-11-25) sha256:a64a644f3a8c
Architect: gemini (gemini-4-carbon, low)

## Track
`design` — adds `Requirement: Conventional keyboard traversal, WAI-ARIA tablist navigation, focus management, and character-key shortcut gating` and modifies `openspec/specs/cop-ui/spec.md` across `ui/src/lib/` stores, hotkey controller, workspace components, styles, and Playwright E2E gates (`#41`, `#44` Position 3, prerequisite for `#42` and `#11`).

## Why
- Clicking or dragging on `TacticalCanvas.svelte:766,865-866`, `BenchWorkspace.svelte:470-472`, or `CortexWorkspace.svelte:749-755` focuses a `tabindex="-1"` `[data-viewport-hotkey-scope="true"]` surface, where `hotkeys.ts:161-167,227` intercepts `Tab`, toggles `lightsOut`, and calls `preventDefault()`—trapping forward keyboard traversal while `Shift+Tab` traverses backward.
- In `CopWorkspace.svelte:292-330,333-561`, the `Fleet` Solo-Mode `role="tablist"` puts all three `role="tab"` buttons in the `Tab` order, ignores `ArrowLeft`/`ArrowRight`/`Home`/`End`, omits `aria-controls` / `role="tabpanel"` / `aria-labelledby`, and drops focus to `document.body` if focused panel content unmounts.
- Global character-key shortcuts (`G`, `D`, `B`, `K`, `M`, `1`–`9`, `T`, `C`, `H` in `hotkeys.ts:133-214`) have no operator disable control (WCAG 2.2 SC 2.1.4), `WorkspaceShell.svelte:218-229,244-280` uses `aria-pressed` instead of `aria-current="page"` and strands focus on `document.body` when cross-workspace jump buttons unmount, and `--ui-focus-ring` (`colors.css:32`) has zero `:focus-visible` rules.

## What Changes
- Remove `Tab` as a shortcut (`hotkeys.ts:28,64-80,161-167`, `WorkspaceShell.svelte:239`) so `Tab` and `Shift+Tab` always traverse controls in logical DOM order, and bind `Lights-Out` to a non-conflicting shortcut and labeled topbar button (`OQ1`).
- Add a global character-key shortcut disable mechanism (`OQ2`) with synchronized `<kbd>` / `[X]` shortcut hints, ARIA editable-widget exclusion, and preserved pointer controls.
- Implement the WAI-ARIA `tablist` pattern in `CopWorkspace.svelte` (`OQ3`: single roving tab stop, arrow navigation, `role="tabpanel"` associations, focus recovery), `aria-current="page"` + predictable focus across workspace switches (`WorkspaceShell.svelte`), and unobstructed `:focus-visible` styling (`colors.css`).

## Decisions you are making by approving
- **`OQ1` (`Lights-Out` shortcut):** Whether `Lights-Out` uses single-key `L` (gated when character shortcuts are disabled) or `Shift+L` (active even when character shortcuts are disabled), replacing `Tab`.
- **`OQ2` (Disabling character-key shortcuts):** Whether the global character-shortcut toggle persists per browser (`localStorage`), keeps `Escape` camera untether active, and hides inactive `[X]` / `<kbd>` shortcut hints, or lives in the URL hash (`#keys=off`) and disables `Escape` too.
- **`OQ3` (`Fleet` inspector tabs & post-switch focus):** Whether `Left`/`Right` Arrow switches `Fleet` inspector tabs immediately (automatic activation) or requires `Enter`/`Space` (manual activation), and whether hotkey/jump workspace switches focus the newly opened workspace stage or the topbar navigation button.

## Scope & Non-Goals
- **In scope:** `openspec/specs/cop-ui/spec.md`, `ui/src/lib/stores/viewport.svelte.ts`, `ui/src/lib/viewport/hotkeys.ts`, `ui/src/routes/+page.svelte`, `ui/src/styles/colors.css`, `ui/src/lib/components/{WorkspaceShell,CopWorkspace,PilotWorkspace,BenchWorkspace,CortexWorkspace,TacticalCanvas,FleetFilmstrip}.svelte`, Vitest suites, and Playwright E2E (`ui/e2e/cop-workspace.e2e.ts`).
- **Non-goals:** Full WCAG certification, custom per-key remapping UI, compact desktop drawer layout (`#42`), session draft persistence (`#40`), or `M3.1` actuation (`#11`).

## Definition of Done
- Owner rules on `OQ1`–`OQ3`, fills `Approved-by: <name> (<YYYY-MM-DD>) sha256:<12 hex>`, and clears the `OPEN-Q` approval task gate.
- Vitest unit/component + reference-model hotkey/tablist tests, non-destructive `cp` + SHA-256 mutation checks, Playwright browser keyboard traversal/focus E2E checks, and manual assistive-technology spot-check notes pass with `// spec:` attribution.
- `openspec/changes/m2-5-keyboard-navigation/` is archived, `GOTOOLCHAIN=auto task verify` passes on a clean tree, and cross-provider review approves before squash merge.
