# Proposal: m1-link-loss-freshness
Approved-by:
Architect: gemini (gemini-4-carbon, medium)

## Track
`design` — modifies `openspec/specs/mavlink-ingest/spec.md` and changes Lane 2/3 SKG batching and link-freshness behavior in `internal/mavlink` (`#27`, `#29`), unblocking `#39` (`#44` Position 2).

## Why
- `Tracker` (`internal/mavlink/tracker.go:109-128,691-724`) never tracks per-`sysid` silence or emits `link_lost` / `link_restored` (`ui/src/lib/stores/telemetry.svelte.ts:39-46` already consumes both), and `tickLoop` (`internal/mavlink/component.go:294-310`) re-marshals every session on every `1–5 Hz` tick so silent vehicles keep churning `ENTITY_STATES` revisions (`ui/src/lib/hud/view-model.ts:453-474`).
- `Publisher` (`internal/mavlink/publisher.go:28-80,175-215`) marshals periodic batches at tick time into a cap-2 `periodicCh`, requiring `maxAttemptedRev` and `skipEqualSysIDs` bookkeeping, while `applyStatusText` (`internal/mavlink/tracker.go:569-618`) triggers an urgent 22-triple SKG rewrite even though `#253 STATUSTEXT` is absent from `VehicleSnapshot` (`internal/uxsvocab/snapshot.go:53-97`).

## What Changes
- Track per-`sysid` last-accepted-packet time in `internal/mavlink`, evaluate silence on the existing SKG tick (zero new timers/goroutines), and emit `TransitionEvent` (`link_lost` once after silence threshold; `link_restored` on next accepted packet) on `bbops.telemetry.events.<sysid>` (`#27`, pending `OQ1`–`OQ2`).
- Replace tick-time periodic marshaling (`PeriodicQueueCap`, `periodicCh`, `EnqueuePeriodic`, `DroppedPeriodic`, `maxAttemptedRev`) with build-at-flush periodic SKG batching that drains urgent `sysid`s strictly ahead of periodic flushes while keeping UDP reads non-blocking (`#29`, pending `OQ3`).
- Drop `UrgentSKGSync` on `#253 STATUSTEXT` while preserving its immediate Lane 3 `bbops.telemetry.events.<sysid>` `statustext` event (`#29`).

## Scope & Non-Goals
- **In scope:** `openspec/specs/mavlink-ingest/spec.md`, `internal/mavlink/{config.go,component.go,publisher.go,tracker.go,mavlink_test.go}`, and any `internal/uxsvocab` / `internal/host` / `ui/src/lib/stores/skg.svelte.ts` touch required if `OQ2` selects a 23rd predicate.
- **Non-goals:** COP/HUD unavailable-vs-nominal fleet health UI (`#39`), `M3` outbound `#76`/`#84` actuation (`#11`), session eviction from `ENTITY_STATES` (stale vehicles remain visible with last-known fault state), or `M4+` Reticulum arbitration.

## Definition of Done
- Owner rules on `OQ1`–`OQ3`, fills `Approved-by: <name> (<YYYY-MM-DD>) sha256:<12 hex>`, and clears `Hold: OPEN-Q proposal awaits owner approval`.
- Deterministic injected-clock unit tests, reference-model property tests (`mavlink_test.go`), host integration tests, and non-destructive `task mutate` checks pass with `// spec:` attribution.
- `openspec/changes/m1-link-loss-freshness/` is archived, `GOTOOLCHAIN=auto task verify` passes on a clean tree, and cross-provider review approves before squash merge.
