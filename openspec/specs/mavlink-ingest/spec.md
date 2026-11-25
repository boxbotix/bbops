# mavlink-ingest Specification

## Purpose
Defines the `bbops` UXS consumer vocabulary pack (`internal/uxsvocab`), SemEngine ADR-102 6-part vehicle entity identity, constant-predicate `Graphable` vehicle SKG snapshot (`uxs.vehicle.v1`), `bbstack` `0xB1` `HEARTBEAT.custom_mode` bitfield codec, clean-room `common.xml` MAVLink v2 (`0xFD`) frame codec (`internal/mavcodec`), and external 3-lane hybrid MAVLink v2 UDP telemetry & SKG ingest component (`internal/mavlink`).

## Requirements
### Requirement: UXS consumer vocabulary pack and ADR-102 vehicle entity identity
The `internal/uxsvocab` package SHALL register `bbops`'s consumer vocabulary pack with `semengine/vocabulary`, declaring the `uxs.vehicle` entity type, `EntityDomainDelegation` (`Producer: "bbops"`, `Domain: "uxs"`, `Type: "vehicle"`), and all 18 lowercase 3-part underscore-free `uxs.*` predicates (`uxs.domain.kind`, `uxs.authority.state`, `uxs.mode.companion`, `uxs.link.tier`, `uxs.health.armed`, `uxs.health.flags`, `uxs.health.failsafes`, `uxs.health.crcerrors`, `uxs.health.loopdtus`, `uxs.health.maxloopdtus`, `uxs.health.rttus`, `uxs.attitude.quaternion`, `uxs.attitude.rates`, `uxs.kinematics.ned`, `uxs.kinematics.vx`, `uxs.kinematics.vy`, `uxs.kinematics.vz`, `uxs.kinematics.headingdeg`) within `semengine`'s closed 7-datatype set (`string`, `entity_id`, `int`, `float`, `bool`, `datetime`, `json`), while reusing `semengine/vocabulary` built-ins (`geo.location.latitude`, `geo.location.longitude`, `geo.location.altitude`, `dc.terms.title`, `entity.indexing.profile`), and SHALL construct canonical 6-part ADR-102 vehicle entity IDs `<org>.<platform>.robotics.uxs.vehicle.<sysid>` (`<= 256` bytes) for MAVLink `sysid` in `1..=254`.

#### Scenario: Registered UXS predicates satisfy SemEngine 3-part grammar and closed datatype set
- **WHEN** the `uxsvocab` pack is validated and registered with `semengine/vocabulary`
- **THEN** all 18 `uxs.*` predicates and `uxs.vehicle` resolve without error, zero predicate segments contain uppercase characters or underscores, and every declared datatype belongs to `{string, entity_id, int, float, bool, datetime, json}`

#### Scenario: Vehicle entity ID builder formats valid sysids and rejects invalid inputs
- **WHEN** `VehicleEntityID(org, platform, sysid)` is called with valid `org`/`platform` tokens and `sysid` in `1..=254`, and called again with `sysid` `0`, `255`, empty/invalid segments, or inputs exceeding `256` total bytes
- **THEN** valid inputs return `<org>.<platform>.robotics.uxs.vehicle.<sysid>` passing `semengine/pkg/types.ValidateEntityID`, and invalid inputs return a non-nil validation error

### Requirement: Constant-predicate Graphable vehicle snapshot payload
The `internal/uxsvocab` package SHALL provide a `graph.Graphable` vehicle snapshot payload (`uxs.vehicle.v1`) registered in `payloadregistry.Registry` with `IndexingProfile: vocabulary.IndexingProfileSignal` (wired into `internal/host` startup) that emits the exact same constant 22-predicate producer set (18 `uxs.*` predicates plus `geo.location.latitude`, `geo.location.longitude`, `geo.location.altitude`, and `dc.terms.title`) on every snapshot—using deterministic finite zero/sentinel defaults before optional telemetry frames arrive and rejecting `NaN`/`±Inf` values during `Validate()`—so `graph-ingest` stamps `entity.indexing.profile = "signal"` and `graph-index` skips `PREDICATE_INDEX` rewrites after initial entity creation.

#### Scenario: Uninitialized and populated vehicle snapshots emit identical predicate sets
- **WHEN** `Triples()` is evaluated on a newly initialized `VehicleSnapshot` (prior to `#1 SYS_STATUS` or `#33 GLOBAL_POSITION_INT` arrival) and on a fully populated `VehicleSnapshot` for `Quadcopter`, `Rover`, and `SurfaceBoat`
- **THEN** both snapshots emit the exact same 22 canonical predicate names in deterministic order, omit producer-stamped `graph.SourceIndexingProfile`, and `Validate()` rejects non-finite (`NaN`/`±Inf`) floats or unknown enum strings

#### Scenario: Host runtime ingests VehicleSnapshot into ENTITY_STATES with signal indexing profile
- **WHEN** a running `bbops` host publishes a `uxs.vehicle.v1` `VehicleSnapshot` onto `entity.uxs.upsert`
- **THEN** `ENTITY_STATES` and `POST /graph-gateway/graphql` materialize `<org>.<platform>.robotics.uxs.vehicle.<sysid>` carrying all 22 producer triples plus `entity.indexing.profile = "signal"`

### Requirement: bbstack 0xB1 HEARTBEAT custom_mode bitfield codec
The `internal/mavcodec` package SHALL deterministically pack and unpack the 32-bit `bbstack` `0xB1` `HEARTBEAT.custom_mode` bitfield (`[31:24] = 0xB1`, `[23:20] = LinkTier` `{0..3}`, `[19:16] = HealthFlags` `{clock_sync_locked, teleop_deadman_clamped, crsf_link_healthy, companion_trunk_healthy}`, `[15:12] = VehicleDomain` `{0..2}`, `[11:8] = CompanionMode` `{0..3}`, `[7:0] = ControlAuthority` `{0..3}`), returning typed errors rather than panicking on non-`0xB1` magic bytes, out-of-range enum nibbles, or invalid `HEARTBEAT` envelopes.

#### Scenario: 0xB1 custom_mode packs and unpacks losslessly across all valid domains, modes, tiers, authorities, and flags
- **WHEN** every valid combination of `LinkTier`, `HealthFlags` (`4`-bit mask), `VehicleDomain`, `CompanionMode`, and `ControlAuthority` is encoded with `EncodeCustomMode` and decoded with `DecodeCustomMode`
- **THEN** `DecodeCustomMode(EncodeCustomMode(s))` equals `s` with `[31:24] == 0xB1` and maps cleanly onto `uxsvocab` kebab-case domain, mode, link-tier, and authority strings

#### Scenario: Malformed or non-0xB1 custom_mode words and fuzz inputs return typed errors without panicking
- **WHEN** `DecodeCustomMode` or `DecodeHeartbeat` receives arbitrary `uint32` words (`FuzzDecodeCustomMode`), non-`0xB1` magic bytes (including GCS `custom_mode = 0`), or out-of-range enum nibbles (`LinkTier > 3`, `VehicleDomain > 2`, `CompanionMode > 3`, `ControlAuthority > 3`)
- **THEN** it never panics and returns a typed error distinguishable via `errors.Is` / `errors.As`

### Requirement: Clean-room common.xml MAVLink v2 frame codec and zero-GPLv3 dependency guard
The `internal/mavcodec` package SHALL encode and decode MAVLink v2 (`0xFD`) frames restricted strictly to MIT `minimal.xml` and `common.xml` definitions for message IDs `#0 HEARTBEAT`, `#1 SYS_STATUS`, `#31 ATTITUDE_QUATERNION`, `#32 LOCAL_POSITION_NED`, `#33 GLOBAL_POSITION_INT`, `#76 COMMAND_LONG`, `#77 COMMAND_ACK`, `#84 SET_POSITION_TARGET_LOCAL_NED`, and `#253 STATUSTEXT`, returning typed errors on malformed frames, bad CRCs, or unsupported message IDs without panicking, and automated tests SHALL verify that `go.mod`, `go.sum`, and the `bbops` Go package dependency graph contain zero `ardupilotmega`, GPLv3 flight-stack (`ardupilot`, `betaflight`, `inav`, `cleanflight`), or retired `semlink`/`semops` imports.

#### Scenario: All nine contract messages round-trip through MAVLink v2 0xFD wire bytes
- **WHEN** representative frames for `#0`, `#1`, `#31`, `#32`, `#33`, `#76`, `#77`, `#84`, and `#253` (`sysid` `1..=254`, `compid = 1`) are encoded to bytes and decoded back
- **THEN** every frame starts with MAVLink v2 magic `0xFD`, preserves `sysid`, `compid`, sequence number, and all contract payload fields, and round-trips losslessly

#### Scenario: Corrupted CRC, non-v2 magic, truncated bytes, or unsupported message IDs fail cleanly
- **WHEN** `DecodeFrame` is given truncated byte slices, MAVLink v1 `0xFE` frames, corrupted X.25 CRC bytes, or message IDs outside `{0, 1, 31, 32, 33, 76, 77, 84, 253}`
- **THEN** it returns a non-nil typed error and never panics

#### Scenario: Dependency graph and module manifest exclude ardupilotmega and GPLv3 dialects
- **WHEN** the repository dependency guard inspects `go.mod`, `go.sum`, and `go list -deps ./...`
- **THEN** zero packages or modules match `ardupilotmega`, `ardupilot`, `betaflight`, `inav`, `cleanflight`, `semlink`, or `semops`

### Requirement: Three-lane hybrid MAVLink v2 UDP telemetry and SKG ingest component
The `internal/mavlink` package SHALL operate as `bbops`'s clean-room MAVLink v2 (`0xFD`) UDP edge ingest adapter (`127.0.0.1:14550` default, configurable host/port, `1–5 Hz` SKG snapshot rate defaulting to `1 Hz`, and configurable per-`sysid` silence timeout), maintaining per-`sysid` (`1..=254`, `compid = 1`) vehicle state and last-accepted-packet freshness across `#0 HEARTBEAT`, `#1 SYS_STATUS`, `#31 ATTITUDE_QUATERNION`, `#32 LOCAL_POSITION_NED`, `#33 GLOBAL_POSITION_INT`, and `#253 STATUSTEXT` and splitting inbound telemetry across three lanes without blocking the UDP socket loop on JetStream acknowledgements:
1. **Lane 1 (Fast Kinematics):** Publish `at-most-once` JSON kinematic frames onto `bbops.telemetry.<sysid>` for uncoalesced streaming over `output/websocket` instance #1.
2. **Lane 2 (Coalesced Live SKG):** Coalesce periodic `1–5 Hz` SKG ticks non-blockingly and build serialized constant 22-predicate `uxs.vehicle.v1` `VehicleSnapshot` batches (`IndexingProfileSignal`) at flush time for sessions with unconfirmed state updates, publishing via `natsclient.PublishBatchToStream` onto `entity.uxs.upsert` (`ENTITY` stream -> `processor/graph-ingest` -> `ENTITY_STATES`) so silent/unchanged sessions stop churning periodic SKG re-upserts while retaining their last-known SKG state.
3. **Lane 3 (Instant Edge-Burst Reconcile & Events):** Detect per-`sysid` state edges (`armed`, `ControlAuthority`, `CompanionMode`, `LinkTier`, `HealthFlags`, and `#1 SYS_STATUS` failsafe increments), immediately publishing discrete transition events onto `bbops.telemetry.events.<sysid>` AND triggering an out-of-band `VehicleSnapshot` SKG flush drained ahead of periodic SKG flushes so sub-`250 ms` `DirectCrsfOverride`, `McuFailsafe`, or deadman transitions are never masked; publish `#253 STATUSTEXT` immediately as a `statustext` event onto `bbops.telemetry.events.<sysid>` without triggering an out-of-band `VehicleSnapshot` SKG flush; and evaluate per-`sysid` packet silence on the existing SKG tick (without extra goroutines or per-`sysid` timers), emitting a single `link_lost` transition event onto `bbops.telemetry.events.<sysid>` once a session's elapsed time since its last accepted packet crosses the configured silence threshold and emitting `link_restored` (plus updating SKG freshness) upon the next accepted packet for that `sysid`.

#### Scenario: Lane 1 streams multi-sysid kinematics uncoalesced to WebSocket #1
- **WHEN** `Quadcopter`, `Rover`, and `SurfaceBoat` peers (`sysid` `1..=254`, `compid = 1`) transmit `0xB1` `#0 HEARTBEAT` followed by `#31 ATTITUDE_QUATERNION`, `#32 LOCAL_POSITION_NED`, and `#33 GLOBAL_POSITION_INT` UDP datagrams to `bbops`
- **THEN** `output/websocket` instance #1 delivers uncoalesced `data` frames on `bbops.telemetry.<sysid>` carrying each vehicle's fused quaternion, body rates, local NED position/velocity, global lat/lon/alt, and heading in `[0, 360]`

#### Scenario: Lane 2 builds constant-predicate VehicleSnapshot batches at flush time and halts silent re-upserts
- **WHEN** active vehicle sessions accumulate `#0 HEARTBEAT`, `#1 SYS_STATUS`, `#31`, `#32`, and optional `#33` telemetry across periodic `1–5 Hz` snapshot ticks (including prior to first `#33` GPS fix) and subsequently go silent with zero new packets
- **THEN** `internal/mavlink` builds `VehicleSnapshot` batches at flush time without blocking UDP reception under JetStream backpressure, materializes `<org>.<platform>.robotics.uxs.vehicle.<sysid>` in `ENTITY_STATES`, `output/websocket` instance #2, and `POST /graph-gateway/graphql` with `entity.indexing.profile = "signal"` and the constant 22-predicate triple set, and stops re-upserting unchanged snapshots once their latest revision is confirmed

#### Scenario: Lane 3 flushes instant authority, failsafe, and deadman transitions while keeping STATUSTEXT event-only
- **WHEN** a vehicle's consecutive `#0 HEARTBEAT` flips `ControlAuthority` (such as `CompanionTeleop` -> `DirectCrsfOverride` or `McuFailsafe`), `armed`, `CompanionMode`, `LinkTier`, or `HealthFlags` (`teleop_deadman_clamped`) or `#1 SYS_STATUS` increments `failsafes`, and subsequently `#253 STATUSTEXT` arrives without changing `VehicleSnapshot` predicates
- **THEN** `internal/mavlink` immediately publishes transition events for all edges (including `statustext`) on `bbops.telemetry.events.<sysid>` (received on WebSocket #1), triggers an out-of-band `VehicleSnapshot` flush to `entity.uxs.upsert` ahead of periodic flushes for the `#0`/`#1` state edges, and does not trigger an out-of-band `VehicleSnapshot` SKG flush for `#253 STATUSTEXT`

#### Scenario: Per-sysid silence emits a single link_lost edge and next accepted packet emits link_restored
- **WHEN** an admitted `sysid` stops sending valid MAVLink v2 packets across multiple SKG ticks past the configured silence threshold and later transmits a valid packet
- **THEN** `internal/mavlink` emits exactly one `link_lost` transition event on `bbops.telemetry.events.<sysid>` upon crossing the silence threshold while preserving the vehicle's last-known state in `ENTITY_STATES`, emits zero duplicate `link_lost` events on subsequent silent ticks, and emits `link_restored` on `bbops.telemetry.events.<sysid>` (alongside refreshing SKG state) when valid packets resume

#### Scenario: Non-0xB1 heartbeats, invalid sender IDs, and non-finite kinematic packets are rejected without poisoning vehicle state
- **WHEN** the UDP listener receives a peer GCS `#0 HEARTBEAT` (`MAV_TYPE_GCS = 6`, `custom_mode = 0`), a packet with `sysid = 255` or `compid != 1`, a corrupted/non-v2 datagram, or a `#31/#32/#33` packet with `NaN`/`±Inf` or out-of-bounds coordinates
- **THEN** the adapter drops the offending datagram without panicking, creating a bogus entity, refreshing link-loss timers, or corrupting an existing `sysid`'s last valid `VehicleSnapshot`
