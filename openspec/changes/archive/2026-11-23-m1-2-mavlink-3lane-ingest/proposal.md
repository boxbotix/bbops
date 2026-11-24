# Proposal: m1-2-mavlink-3lane-ingest
Approved-by: Coby Leuschke (2026-11-23) sha256:be4c1633d413
Architect: gemini (gemini-4-carbon, medium)

## Track

`design`: adds a `Requirement` to `openspec/specs/mavlink-ingest/spec.md`, touches a second capability (`openspec/specs/host-runtime/spec.md`), wires `internal/mavlink` into the `configs/bbops.json` + `internal/host` + `cmd/bbops` boot path, and defines JetStream/NATS backpressure and edge-burst durability semantics (#6 / Epic #4).

## Why

`M1.1` (`#5`) landed the `uxs.*` vocabulary pack (`internal/uxsvocab/pack.go:38-115`), constant 22-predicate `VehicleSnapshot` (`internal/uxsvocab/snapshot.go:53-315`), and `bbstack` `0xB1` + `common.xml` MAVLink v2 frame codec (`internal/mavcodec/frame.go:223-343`), while `configs/bbops.json:56-277` and `internal/host/config.go:48-106` still compose only the 7 SKG/GraphQL/WebSocket components with zero inbound UDP telemetry source. `#6` (`M1.2`) needs `internal/mavlink` as `bbops`'s external Go MAVLink v2 UDP adapter (`"external": true` per `semengine` ADR-100) implementing the `semboids` ADR-001 + `semengine` ADR-115 3-lane split so `20 Hz` kinematics stream uncoalesced to WebSocket #1, `1–5 Hz` coalesced `VehicleSnapshot` batches update `ENTITY_STATES` without `PREDICATE_INDEX` write amplification, and sub-`250 ms` `DirectCrsfOverride` / `McuFailsafe` / deadman edges surface immediately.

## What Changes

1. **External MAVLink v2 UDP Ingest Component (`internal/mavlink`):**
   - Bind a configurable local UDP listener (`127.0.0.1:14550` default), decode inbound `#0 HEARTBEAT`, `#1 SYS_STATUS`, `#31 ATTITUDE_QUATERNION`, `#32 LOCAL_POSITION_NED`, `#33 GLOBAL_POSITION_INT`, and `#253 STATUSTEXT` packets via `internal/mavcodec` (`sysid` `1..=254`, `compid == 1`), and maintain per-`sysid` vehicle state + peer UDP endpoint metadata.
   - **Lane 1 (`20 Hz` Fast Path):** Publish compact latest-wins kinematic JSON frames (`at-most-once` core NATS) on `bbops.telemetry.<sysid>` for uncoalesced delivery over `telemetry-websocket` (`output/websocket` #1).
   - **Lane 2 (`1–5 Hz` Coalesced SKG Snapshot):** Decouple the UDP read loop from JetStream via bounded non-blocking buffering and flush constant-predicate `uxs.vehicle.v1` `VehicleSnapshot` batches (`IndexingProfileSignal`) via `natsclient.PublishBatchToStream` onto `entity.uxs.upsert` (`ENTITY` -> `graph-ingest` -> `ENTITY_STATES`).
   - **Lane 3 (Instant Edge-Burst Reconcile & Events):** Diff consecutive `HEARTBEAT` (`armed`, `ControlAuthority`, `CompanionMode`, `LinkTier`, `HealthFlags`), `#1 SYS_STATUS` failsafe increments, and `#253 STATUSTEXT` per `sysid`, publishing immediate transition events on `bbops.telemetry.events.<sysid>` AND triggering an out-of-band SKG snapshot flush (tagged with `#6`'s required `// shortcut:` marker for `semengine` `alpha.9` `processor/rule` migration).
2. **Host Composition & Ephemeral Port Isolation (`configs/bbops.json`, `internal/host`, `cmd/bbops`):**
   - Compose `mavlink-ingest` into the default flow and add UDP port override/ephemeral loopback allocation so parallel `host.StartEphemeral` and CLI tests never collide on `:14550`.
3. **Capability Specs:** Add 3-lane ingest requirements to `openspec/specs/mavlink-ingest/spec.md` and update `openspec/specs/host-runtime/spec.md`.

## Scope & Non-Goals

- **In-bounds:** `internal/mavlink/`, `configs/bbops.json`, `internal/host/`, `cmd/bbops/`, `openspec/specs/{mavlink-ingest,host-runtime}/spec.md`.
- **Non-goals:** Outbound `#0` GCS heartbeat, `#84` velocity leases, or `#76`/`#77` ADR-111 command state machines (`#11` `M3.1`); Svelte 5 stores (`#8` `M2.1`).

## Definition of Done

- Owner approves `proposal.md` + `design.md` with `OQ1`–`OQ4` rulings and change is archived before ready-for-review.
- Loopback UDP multi-`sysid` (`Quadcopter`, `Rover`, `SurfaceBoat`) integration test verifies uncoalesced `WS #1` kinematics (`bbops.telemetry.<sysid>`), `ENTITY_STATES` + GraphQL + `WS #2` materialization, and instant `DirectCrsfOverride` / `McuFailsafe` / deadman edge bursts (`bbops.telemetry.events.<sysid>` + out-of-band SKG update).
- Survivor-free `task mutate` checks on edge-diff / coalescer / validation logic, `ateam state debt` tracking the `processor/rule` shortcut, and `task verify` green on a clean tree.
