## ADDED Requirements

### Requirement: Pin-on-admission MAVLink v2 UDP command egress and 1 Hz GCS liveness
`internal/mavlink` and `internal/control` SHALL pin each `0xB1` vehicle peer's `netip.AddrPort` (`sysid` `1..=254`, `compid = 1`, gated by `allowed_peers`) on initial `#0 HEARTBEAT` admission until `link_lost`, send `#0`, `#76`, and `#84` frames strictly to that pinned peer, emit `1 Hz` GCS `#0 HEARTBEAT` (`MAV_TYPE_GCS = 6`, `sysid = 255`) while active, and decode `#77 COMMAND_ACK` from the pinned peer.

#### Scenario: Spoofed UDP sender claiming an admitted sysid cannot redirect outbound actuation
- **WHEN** `sysid = 1` is admitted from peer endpoint `A` (`0xB1` `#0 HEARTBEAT`) and while `sysid = 1` remains live (`!linkLost`), a second UDP socket `B != A` sends a valid `0xB1` packet claiming `sysid = 1`, followed by an operator `#76` or `#84` transmission
- **THEN** `bbops` rejects socket `B`'s datagram without mutating `VehicleSnapshot`, refreshing freshness, or redirecting `sysid = 1`'s TX target, and transmits outbound MAVLink v2 frames strictly to `A` (allowing peer re-binding only on a valid `0xB1` `#0 HEARTBEAT` after `link_lost`)

#### Scenario: Active operator control emits 1 Hz GCS HEARTBEAT and stops when idle
- **WHEN** an operator engages teleop or dispatches a discrete `#76` command for an admitted `sysid` across multiple `1 s` ticks and subsequently disengages/completes with zero active sessions
- **THEN** `bbops` transmits `1 Hz` MAVLink v2 `#0 HEARTBEAT` frames (`MAV_TYPE_GCS = 6`, `custom_mode = 0`, `sysid = 255`) to active peer(s) while engaged and halts outbound GCS heartbeats once idle

### Requirement: Lease-bound at-most-once #84 velocity teleop with LinkTier and authority safety gates
`internal/control` and same-origin `POST /control/teleop` SHALL forward `10–30 Hz` `#84 SET_POSITION_TARGET_LOCAL_NED` (`LOCAL_NED` or `BODY_NED`, `type_mask = 0x07C7`) at-most-once over UDP with zero JetStream/`ENTITY_STATES` writes, rejecting non-finite floats (`NaN`/`±Inf`), disarmed or `linkLost` targets, `mesh-supervisory` links, and `ControlAuthority != companion-teleop`, and expiring silent leases within `< 250 ms` with a zero-velocity frame.

#### Scenario: Non-finite velocity inputs and disallowed LinkTier or ControlAuthority states fail closed with zero UDP packets
- **WHEN** a client submits `#84` teleop nudges containing `NaN` or `±Inf` in `vx`, `vy`, `vz`, or `yaw_rate`, or targets a vehicle with `linkTier == "mesh-supervisory"`, `authority` in `{"direct-crsf-override", "mcu-failsafe", "companion-autonomous"}`, `!armed`, or `linkLost == true`
- **THEN** `internal/control` refuses the request with a structured error, transmits zero `#84` UDP datagrams, and writes zero records to JetStream or `ENTITY_STATES`

#### Scenario: Stalled browser teleop nudges expire server lease within 250ms ahead of bbnav's 300ms deadman
- **WHEN** an operator streams valid `20 Hz` `#84` velocity nudges to an armed `companion-teleop` vehicle on `sitl-local`, `direct-bench`, or `mesh-high-rate` and then stops sending nudges without an explicit stop call
- **THEN** `internal/control` expires the active teleop lease within `< 250 ms`, transmits a single zeroed `#84` (`vx=0, vy=0, vz=0, yaw_rate=0`) release datagram, and ceases further `#84` transmissions

### Requirement: ADR-111 non-retrying #76 COMMAND_LONG execution, #77 COMMAND_ACK correlation, and preemption
`internal/control` and same-origin `POST /control/command` SHALL execute `#76 COMMAND_LONG` (`ARM_DISARM = 400`, mode/loiter `176`/`17`, and `sitl-local`-only `MAV_CMD_USER_1 = 31010`) with `Confirmation = 0` and zero retries, projecting `pending -> confirmed | rejected | commit_unknown | preempted` via `lifecycle.Manager` CAS in `ENTITY_STATES`, timing out dropped ACKs to `commit_unknown`, and revoking teleop + preempting `pending` commands on `direct-crsf-override`/`mcu-failsafe`.

#### Scenario: Dropped COMMAND_ACK transitions to commit_unknown with zero blind retries and reconciles on next HEARTBEAT
- **WHEN** an operator dispatches a `#76 COMMAND_LONG` (`ARM_DISARM` or `HoldPosition`), the vehicle's `#77 COMMAND_ACK` is dropped in transit past the ACK timeout, and a subsequent `#0 HEARTBEAT` reports the updated vehicle state
- **THEN** `bbops` transmits exactly `1` `#76 COMMAND_LONG` UDP datagram (`Confirmation = 0`, zero retries), transitions the command entity in `ENTITY_STATES` from `pending` to `commit_unknown` upon timeout, and surfaces reconciled vehicle state once the next `#0 HEARTBEAT` arrives

#### Scenario: SITL fault injection command MAV_CMD_USER_1 is refused outside sitl-local LinkTier
- **WHEN** a client requests `MAV_CMD_USER_1 (31010)` (companion blackout, synthetic CRSF override, or cross-current injection) against a vehicle whose `linkTier` is `"direct-bench"`, `"mesh-high-rate"`, or `"mesh-supervisory"`
- **THEN** `internal/control` rejects the command before UDP serialization and transmits zero `#76` packets

#### Scenario: Hardware CRSF override or MCU failsafe edge immediately preempts active teleop and pending commands
- **WHEN** a vehicle has an active `#84` teleop lease and an in-flight `pending` `#76` command when `internal/mavlink` ingests a `#0 HEARTBEAT` edge transitioning `ControlAuthority` to `direct-crsf-override` or `mcu-failsafe`
- **THEN** `internal/control` synchronously revokes the `#84` teleop lease on the edge callback, transitions the in-flight command entity in `ENTITY_STATES` to `preempted` via revision-fenced CAS, and blocks new teleop nudges until authority returns to `companion-teleop` and the operator explicitly re-engages
