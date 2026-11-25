# Proposal: m2-5-health-freshness
Approved-by:
Architect: gemini (gemini-4-carbon, medium)

## Track
`design` — adds `Requirement: Fleet health readiness, per-vehicle observation freshness, and last-known fault retention` and modifies `openspec/specs/cop-ui/spec.md` across `ui/src/lib/` stores, HUD/shell view-models, and E2E gates (`#39`, `#44` Position 2, operator-command readiness gate for `#11`).

## Why
- On disconnected startup or empty `ENTITY_STATES` sync, `WorkspaceShell.svelte:124-145` and `summarizeFleetFaults` (`ui/src/lib/hud/view-model.ts:991-1050`) render `FLEET NOMINAL (0)`, while `CopWorkspace.svelte:389-391` renders `All ingested UXS vehicles nominal.`—conflating missing data with confirmed vehicle health.
- After `#27`/`#29` (`internal/mavlink/config.go:34-39`), silent vehicles freeze SKG `Triple.Timestamp`, stop periodic `ENTITY_STATES` re-upserts, and emit `link_lost`/`link_restored` on `WS #1`. However, `skg.svelte.ts:802-864` only high-waters pose (`poseObservedAt`), `telemetry.svelte.ts:581-594` only buffers `link_lost` in an evictable 100-item ring, and `AuthorityBanner.svelte:19-72` / `FleetFilmstrip.svelte:52-68` keep silent armed vehicles bright safety green (`ARMED` / `SAFE`) without last-known age or stream-degradation context.
- Stream pills in `WorkspaceShell.svelte:113-122` expose raw technical channel IDs (`WS1:`, `WS2:`) instead of operator-facing information domains (`Telemetry`, `Fleet data`).

## What Changes
- Distinguish fleet readiness and health states (`connecting`, `unavailable`, `empty` / `No vehicles detected`, `nominal`, `stale`, and active vs. last-known `fault`) in `ui/src/lib/hud/view-model.ts` and `WorkspaceShell.svelte` (`#39`, pending `OQ1`–`OQ3`).
- Track per-`sysid` link-loss/observation freshness across `telemetry.svelte.ts` (`link_lost`/`link_restored` + arrival timestamps) and `skg.svelte.ts` (`observedAt` high-water receipt tracking so socket reconnects or replayed snapshots never disguise old observations), surfacing explicit `ARMED` / `DISARMED` and `LAST KNOWN` age across `AuthorityBanner`, `CopWorkspace`, `PilotWorkspace`, `VehicleHud`, and `FleetFilmstrip`.
- Relabel topbar stream indicators to operator-facing `Telemetry` and `Fleet data` summaries (keeping `WS1`/`WS2` in secondary details/`title`) and represent partial stream loss accurately without hiding usable fallbacks or clearing retained faults.

## Scope & Non-Goals
- **In scope:** `openspec/specs/cop-ui/spec.md`, `ui/src/lib/stores/{telemetry.svelte.ts,skg.svelte.ts}`, `ui/src/lib/hud/view-model.ts`, `ui/src/lib/components/{WorkspaceShell,AuthorityBanner,CopWorkspace,PilotWorkspace,VehicleHud,FleetFilmstrip}.svelte`, Vitest unit/property suites, and Playwright E2E (`ui/e2e/cop-workspace.e2e.ts`).
- **Non-goals:** Changing `0xB1` vehicle fault semantics (`evaluateActiveVehicleFault`), adding WebSocket/UDP retry logic, redesigning alert acknowledgement, or enabling `M3.1` actuation (`#11`).

## Definition of Done
- Owner rules on `OQ1`–`OQ3`, fills `Approved-by: <name> (<YYYY-MM-DD>) sha256:<12 hex>`, and clears `Hold: OPEN-Q proposal awaits owner approval`.
- Controlled-clock Vitest unit + reference-model history tests, non-destructive mutation checks on `view-model.ts` / store freshness gates, and Playwright disconnected-startup + live COP browser checks pass with `// spec:` attribution.
- `openspec/changes/m2-5-health-freshness/` is archived, `GOTOOLCHAIN=auto task verify` passes on a clean tree, and cross-provider review approves before squash merge.
