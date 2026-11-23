## ADDED Requirements

### Requirement: Three-lane hybrid MAVLink v2 UDP telemetry and SKG ingest component
The `internal/mavlink` package SHALL operate as `bbops`'s clean-room MAVLink v2 (`0xFD`) UDP edge ingest adapter (`127.0.0.1:14550` default, configurable host/port and `1–5 Hz` SKG snapshot rate defaulting to `1 Hz`), maintaining per-`sysid` (`1..=254`, `compid = 1`) vehicle state across `#0 HEARTBEAT`, `#1 SYS_STATUS`, `#31 ATTITUDE_QUATERNION`, `#32 LOCAL_POSITION_NED`, `#33 GLOBAL_POSITION_INT`, and `#253 STATUSTEXT` and splitting inbound telemetry across three lanes without blocking the UDP socket loop on JetStream acknowledgements:
1. **Lane 1 (Fast Kinematics):** Publish `at-most-once` JSON kinematic frames onto `bbops.telemetry.<sysid>` for uncoalesced streaming over `output/websocket` instance #1.
2. **Lane 2 (Coalesced Live SKG):** Buffer snapshots non-blockingly and publish constant 22-predicate `uxs.vehicle.v1` `VehicleSnapshot` batches (`IndexingProfileSignal`) via `natsclient.PublishBatchToStream` onto `entity.uxs.upsert` (`ENTITY` stream -> `processor/graph-ingest` -> `ENTITY_STATES`).
3. **Lane 3 (Instant Edge-Burst Reconcile & Events):** Detect per-`sysid` state edges (`armed`, `ControlAuthority`, `CompanionMode`, `LinkTier`, `HealthFlags`, `#1 SYS_STATUS` failsafe increments, and `#253 STATUSTEXT`), immediately publishing discrete transition events onto `bbops.telemetry.events.<sysid>` AND triggering an out-of-band `VehicleSnapshot` SKG flush so sub-`250 ms` `DirectCrsfOverride`, `McuFailsafe`, or deadman transitions are never masked.

#### Scenario: Lane 1 streams multi-sysid kinematics uncoalesced to WebSocket #1
- **WHEN** `Quadcopter`, `Rover`, and `SurfaceBoat` peers (`sysid` `1..=254`, `compid = 1`) transmit `0xB1` `#0 HEARTBEAT` followed by `#31 ATTITUDE_QUATERNION`, `#32 LOCAL_POSITION_NED`, and `#33 GLOBAL_POSITION_INT` UDP datagrams to `bbops`
- **THEN** `output/websocket` instance #1 delivers uncoalesced `data` frames on `bbops.telemetry.<sysid>` carrying each vehicle's fused quaternion, body rates, local NED position/velocity, global lat/lon/alt, and heading in `[0, 360]`

#### Scenario: Lane 2 batches constant-predicate VehicleSnapshots via PublishBatchToStream to ENTITY_STATES
- **WHEN** active vehicle sessions accumulate `#0 HEARTBEAT`, `#1 SYS_STATUS`, `#31`, `#32`, and optional `#33` telemetry across periodic `1–5 Hz` snapshot ticks (including prior to first `#33` GPS fix)
- **THEN** `ENTITY_STATES`, `output/websocket` instance #2, and `POST /graph-gateway/graphql` materialize `<org>.<platform>.robotics.uxs.vehicle.<sysid>` with `entity.indexing.profile = "signal"` and the constant 22-predicate triple set without blocking UDP reception under JetStream backpressure

#### Scenario: Lane 3 flushes instant authority, failsafe, deadman, and STATUSTEXT transitions without 250 ms masking
- **WHEN** a vehicle's consecutive `#0 HEARTBEAT` flips `ControlAuthority` (such as `CompanionTeleop` -> `DirectCrsfOverride` or `McuFailsafe`), `armed`, `CompanionMode`, `LinkTier`, or `HealthFlags` (`teleop_deadman_clamped`), `#1 SYS_STATUS` increments `failsafes`, or `#253 STATUSTEXT` arrives
- **THEN** `internal/mavlink` immediately publishes a transition event on `bbops.telemetry.events.<sysid>` (received on WebSocket #1) and triggers an out-of-band `VehicleSnapshot` flush to `entity.uxs.upsert` ahead of the next periodic SKG tick

#### Scenario: Non-0xB1 heartbeats, invalid sender IDs, and non-finite kinematic packets are rejected without poisoning vehicle state
- **WHEN** the UDP listener receives a peer GCS `#0 HEARTBEAT` (`MAV_TYPE_GCS = 6`, `custom_mode = 0`), a packet with `sysid = 255` or `compid != 1`, a corrupted/non-v2 datagram, or a `#31/#32/#33` packet with `NaN`/`±Inf` or out-of-bounds coordinates
- **THEN** the adapter drops the offending datagram without panicking, creating a bogus entity, or corrupting an existing `sysid`'s last valid `VehicleSnapshot`
