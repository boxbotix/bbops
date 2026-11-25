# Tasks: m2-5-workspace-names

## 1. Approval

- [x] 1.1 Owner approved `proposal.md` (`Approved-by: Coby Leuschke (2026-11-25) sha256:67a10bb66acc` for `#38`).

## 2. Implementation

- [x] 2.1 Update visible workspace destination labels to `Fleet`, `Vehicle`, `Diagnostics`, and `Analysis` across navigation tabs (`WorkspaceShell.svelte:70-79`), `Vehicle` workspace stage heading (`PilotWorkspace.svelte:120-125`), cross-workspace jump links and inspector `aria-label`s (`CopWorkspace.svelte:226-228,303-325,363-370,439-446`), `Analysis` query-tier `aria-label` and fallback error copy (`CortexWorkspace.svelte:378,606`), and hotkey doc comments (`ui/src/lib/viewport/hotkeys.ts:91-97`), while preserving internal `WorkspaceMode` values (`cop`, `pilot`, `bench`, `cortex`), component filenames, keyboard shortcuts (`G`, `D`, `B`, `K`, `M`, `1`–`9`), `#mode=` URL hashes, and legitimate domain uses of `COP` and `0xB1` `Bench` `LinkTier` (`hud/view-model.ts:218`, `docs/architecture/comms-topology.md`). Check: `svelte-check`, `eslint`, and attributed Vitest assertions in `ui/src/lib/components/workspaces.test.ts` and `ui/src/lib/cortex/cortex.test.ts` verify visible/accessible names, cross-workspace jump actions, URL hash compatibility, and domain `Bench` / `COP` preservation, proven able to fail via non-destructive `cp` + SHA-256 mutation check.
- [x] 2.2 Update label-dependent Playwright E2E assertions in `ui/e2e/cop-workspace.e2e.ts` and verify in headless Chromium that `Fleet`, `Vehicle`, `Diagnostics`, and `Analysis` navigation controls expose their accessible names, preserve existing `#mode=` URLs and shortcuts, and render without horizontal clipping or overlap at both `1024×768` and `1440×900` (`#38` acceptance criteria). Check: `GOTOOLCHAIN=auto task test:e2e` and `task test:spec` exit `0`.

## 3. Verification & Archive

- [x] 3.1 Sync `openspec/changes/m2-5-workspace-names/specs/cop-ui/spec.md` into `openspec/specs/cop-ui/spec.md` (leaving `openspec/changes/archive/` untouched), archive `openspec/changes/m2-5-workspace-names/` under `openspec/changes/archive/<YYYY-MM-DD>-m2-5-workspace-names/`, and pass `GOTOOLCHAIN=auto task verify` + `GOTOOLCHAIN=auto task build` + `ateam doctor --check` before marking the PR ready for cross-provider review. Check: all gates exit `0` on a clean working tree.
