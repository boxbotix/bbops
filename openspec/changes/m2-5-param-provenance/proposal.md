# Proposal: m2-5-param-provenance
Approved-by:
Architect: gemini (gemini-4-carbon, low)

## Track
`design` — adds `Requirement: Diagnostics sample baseline provenance, exact numeric staging, and non-actuating offline readiness` and updates `openspec/specs/cop-ui/spec.md` across `ui/src/lib/bench/`, `BenchWorkspace.svelte`, `WorkspaceShell.svelte`, Vitest suites, and Playwright E2E (`#43`, `#44` Position 5, command-readiness gate for `#11`).

## Why
- Before editing (`0 staged`), `BenchWorkspace.svelte:504-597` and `defaultTuningBaseline` (`ui/src/lib/bench/tuning-rack.ts:152-184,335-346`) display hardcoded domain constants beside a live `UXS #<sysid>` without identifying them as sample domain defaults rather than vehicle-reported parameters, and `DiagnosticsDraftStore` (`tuning-rack.ts:35-57,311-414`) carries no provenance discriminator (`"domain-default"` vs. `"vehicle-reported"`).
- `BenchWorkspace.svelte:609-626` relies on a disabled `"Apply Offline (M3.1)"` button and internal engineering jargon (`"until #11 (M3.1) wires ADR-111 non-retrying actuation"`) instead of plain operator copy explaining that edits create a local draft only and cannot change the vehicle, while `BenchWorkspace.svelte:41-44,134-139` and `WorkspaceShell.svelte:59-62,427` still expose an unshipped `onApplyTuning` callback.
- Dial/slider controls (`BenchWorkspace.svelte:514-597`) offer no exact numeric entry, hide `[min, max]`/`step` bounds and unchanged baselines, lack a compact target-vehicle diff summary, and silently coerce invalid/out-of-bounds values via `clampTuningValue` (`tuning-rack.ts:186-201`).

## What Changes
- Track explicit `baselineSource: "domain-default" | "vehicle-reported"` on `DiagnosticsDraftStore` views/records (`ui/src/lib/bench/tuning-rack.ts`) while preserving `#40` per-vehicle `{ entityId, domain, baseline, staged }` isolation, dormant-draft handling, and withheld non-vehicle/poisoned/external-contact guards (`OQ2`).
- Render a first-view provenance & local-staging banner in `BenchWorkspace.svelte` (`0 staged` and dirty states) identifying sample domain defaults vs. vehicle-reported fixtures, stating in plain language that edits stay local and do not modify the vehicle, exposing per-parameter baseline, draft, units, and `[min, max]`/`step` bounds, and adding a target-vehicle (`UXS #<sysid> · <domain>`) change summary plus nearby unavailable-apply text (`OQ2`).
- Pair every dial and slider with an accessible exact numeric input sharing bounds/step, keyboard shortcut suppression (`hotkeys.ts:29-86`), and explicit validation feedback (`OQ1`), verified by Vitest reference-model histories, non-destructive `cp` + SHA-256 mutation checks, and Chromium Playwright E2E checks asserting zero outbound command traffic (`#43`).

## Decisions you are making by approving
- **`OQ1` (Exact numeric input validation for out-of-bounds, off-step, or non-numeric entry):** Reject invalid/out-of-bounds/off-step numeric text with `aria-invalid="true"` and inline bounds/step error copy **without** mutating `staged` until corrected (Option A) vs. clamp/quantize on commit (`change`/`blur`/`Enter`) via `clampTuningValue` and display an inline `"Adjusted <raw> -> <committed>"` notice (Option B).
- **`OQ2` (Offline actuation boundary & `onApplyTuning` prop before `#11`):** Remove `onApplyTuning` from `BenchWorkspace.svelte` and `WorkspaceShell.svelte` now so `Diagnostics` has zero actuation callback path until `#11` defines `reported -> draft -> pending -> confirmed/failed` (Option A) vs. retain optional `onApplyTuning` strictly gated on `baselineSource === "vehicle-reported"` test fixtures while unconditionally blocking `"domain-default"` and unconfigured production mounts (Option B).

## Scope & Non-Goals
- **In scope:** `openspec/specs/cop-ui/spec.md`, `ui/src/lib/bench/{tuning-rack.ts,bench.test.ts}`, `ui/src/lib/components/{BenchWorkspace,WorkspaceShell}.svelte`, `ui/src/lib/components/workspaces.test.ts`, and `ui/e2e/cop-workspace.e2e.ts`.
- **Non-goals:** Reading/writing live vehicle parameters (`PARAM_REQUEST_LIST`/`PARAM_SET`), changing `0xB1` control gains, speculative `pending -> confirmed/failed` command state (`#11`), or compact viewport layout (`#42`).

## Definition of Done
- Owner rules on `OQ1`–`OQ2`, fills `Approved-by: <name> (<YYYY-MM-DD>) sha256:<12 hex>`, and removes the `OPEN-Q` hold in `tasks.md`.
- Spec-cited Vitest unit/component + seeded reference-model history tests (covering `"domain-default"` vs. `"vehicle-reported"` fixtures, exact numeric validation, and vehicle switching), non-destructive `cp` + SHA-256 mutation checks, and Playwright browser checks (including zero outbound command frames/requests) pass.
- `openspec/changes/m2-5-param-provenance/` is archived, `GOTOOLCHAIN=auto task verify` passes on a clean tree, and cross-provider review approves before squash merge.
