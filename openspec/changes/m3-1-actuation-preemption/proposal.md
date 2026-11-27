# Proposal: m3-1-actuation-preemption
Approved-by:
Architect: gemini (gemini-4-carbon, default)

## Track
`design` — introduces `openspec/specs/uxs-control/spec.md` and adds guarded actuation & preemption UX requirements to `openspec/specs/cop-ui/spec.md`, touching `internal/mavlink`, `internal/control`, `internal/host`, `internal/e2eharness`, and `ui/` (`#11`, parent `#10`, after `#39`/`#41`/`#43` readiness).

## Why
- `bbops` currently ingests 3-lane `0xB1` MAVLink v2 telemetry (`internal/mavlink/component.go:266-317`) and encodes `#0`, `#76`, `#77`, and `#84` wire frames (`internal/mavcodec/frame.go:22-50,156-208`), but drops inbound `#77 COMMAND_ACK` (`internal/mavlink/tracker.go:272-275`), has no outbound UDP command/teleop service, and leaves `PilotWorkspace.svelte:34-36,247-253` as a read-only placeholder.
- `vehicleSession.recordAcceptedPacket` (`internal/mavlink/tracker.go:132-142`) overwrites `sess.peerAddr` on every accepted packet (`last-sender-wins`), meaning an unpinned `Component.PeerAddr` (`internal/mavlink/component.go:181-184`) could redirect outbound `#0`/`#76`/`#84` traffic if another UDP sender claims an admitted `sysid` (`#11` design note).
- Operators need lease-bound `10–30 Hz` `#84` velocity teleop (`< 250 ms` server deadman), ADR-111 Decision 3 non-retrying `#76 COMMAND_LONG` actuation (`commit_unknown` without blind retry), `mesh-supervisory` LoRa rate gating, `sitl-local` fault/current lab controls (`MAV_CMD_USER_1`), and instant (`< 2 ms` edge) UI/server release on `direct-crsf-override` or `mcu-failsafe` while preserving `#39` freshness, `#41` keyboard safety, and `#43` offline parameter provenance.

## What Changes
- Pin per-`sysid` UDP command target endpoints against spoofed sender redirection (`OQ1`), ingest `#77 COMMAND_ACK` correlation frames in `internal/mavlink`, and emit `1 Hz` GCS `#0 HEARTBEAT` (`MAV_TYPE_GCS = 6`) while an operator control session is active.
- Add `internal/control` + same-origin host routes (`OQ2`) enforcing finite-float `#84 SET_POSITION_TARGET_LOCAL_NED` (`type_mask = 0x07C7`, `LOCAL_NED`/`BODY_NED`) at-most-once UDP forwarding with `< 250 ms` server lease expiry, `mesh-supervisory` and authority/link-loss lockouts, and ADR-111 Decision 3 non-retrying `#76 COMMAND_LONG` (`ARM_DISARM`, mode/hold, and `sitl-local`-only `MAV_CMD_USER_1`) with instant preemption on `direct-crsf-override` / `mcu-failsafe`.
- Implement `ControlPanel.svelte` in `PilotWorkspace.svelte` (`OQ3`) with supervisory commands, `mesh-supervisory` pad collapse, `sitl-local` Fault & Current Lab drawer, instant stick zeroing + preemption banner latching until explicit re-engage, and preserved `#43` non-actuating offline `Diagnostics` staging.

## Decisions you are making by approving
- **`OQ1` (UDP TX peer endpoint pinning for admitted `sysid`s):** Pin first-admitted `netip.AddrPort` on `0xB1` `#0 HEARTBEAT` until `link_lost` (ignoring mismatched-peer datagrams while live) plus optional `allowed_peers` CIDR filter (Option A) vs. require an explicit static `actuation_peers` `sysid -> host:port` config map before TX while keeping dynamic `PeerAddr` receive-only (Option B).
- **`OQ2` (Browser-to-host control transport & ADR-111 discrete command state):** Expose same-origin `POST /control/command` + `POST /control/teleop` HTTP routes and track discrete `#76` commands via `semengine/pkg/lifecycle.Manager` CAS entities in `ENTITY_STATES` while keeping `10–30 Hz` `#84` leases strictly in-memory/UDP (Option A) vs. add a third `/ws/control` WebSocket for `#84` teleop + `POST /control/command` backed by an in-memory revisioned command store broadcast on Lane 3 events without `ENTITY_STATES` command entities (Option B).
- **`OQ3` (Operator teleop input model, `#41` keyboard safety, & `#43` Diagnostics boundary):** Mount `ControlPanel.svelte` in `Vehicle` (`PilotWorkspace`) with an explicit `Engage Teleop` gate, spring-return pointer pad + focus-scoped pad-only hold keys, and keep `Diagnostics` tuning offline in M3.1 (Option A) vs. pointer-only virtual pad (zero keyboard velocity keys) mounted in both `Vehicle` and `Fleet` `Inspector` while keeping `Diagnostics` tuning offline (Option B).

## Scope & Non-Goals
- **In scope:** `openspec/specs/{uxs-control,cop-ui}/spec.md`, `internal/mavcodec`, `internal/mavlink`, `internal/control`, `internal/host`, `internal/e2eharness`, `ui/src/lib/{control,components,bench}`, Go/Vitest unit + reference-model + `ateam mutate` checks, and Playwright E2E.
- **Non-goals:** Blind command retries, JetStream/`ENTITY_STATES` persistence of `#84` velocity setpoints, `PARAM_SET` wire writes over `"domain-default"` samples (`#43`/`#47`), or M3.2 50-vehicle swarm soak (`#12`).

## Definition of Done
- Owner rules on `OQ1`–`OQ3`, fills `Approved-by: <name> (<YYYY-MM-DD>) sha256:<12 hex>`, and removes the `OPEN-Q` hold in `tasks.md`.
- Go `-race`, `task mutate` on actuation/peer/preemption gates, Vitest reference-model + `cp` SHA-256 UI mutation checks, and Chromium Playwright E2E tests pass.
- `openspec/changes/m3-1-actuation-preemption/` is archived, `GOTOOLCHAIN=auto task verify` passes on a clean tree, and cross-provider review approves before squash merge.
