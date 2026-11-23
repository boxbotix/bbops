# Proposal: m1-1-uxs-vocab-codec
Approved-by: Coby Leuschke (2026-11-23) sha256:c89f9e748652
Architect: gemini (gemini-4-carbon, medium)

## Track

`design`: introduces the new `openspec/specs/mavlink-ingest/spec.md` capability, defines exported domain contracts in `internal/uxsvocab` and `internal/mavcodec` consumed by `#6` (`M1.2`), `#8` (`M2.1`), and `#11` (`M3.1`), and wires `bbops` vocabulary/payload registration into the `internal/host` boot/validation path (#5 / Epic #4).

## Why

`bbops` boots an embedded NATS/JetStream + `semengine v0.1.0-alpha.7` SKG runtime (`internal/host/runtime.go:75-235`), but registers zero domain predicates or `Graphable` payloads (`internal/host/runtime.go:196-201`, `cmd/bbops/main.go:81-89`) and has no clean-room MAVLink v2 (`0xFD`) codec (`go.mod:1-38`). Before `#6` (`M1.2`) can ingest live UDP telemetry into the 3-lane SKG pipeline without `PREDICATE_INDEX` write amplification or vendor dialect contamination, `bbops` needs its canonical `uxs.*` vocabulary pack, ADR-102 vehicle entity identity + constant-predicate `Graphable` snapshot, and deterministic `bbstack` `0xB1` `custom_mode` + `common.xml` MAVLink v2 frame codec.

## What Changes

1. **UXS Consumer Vocabulary Pack & `Graphable` Snapshot (`internal/uxsvocab`):**
   - Register the 18 lowercase 3-part underscore-free `uxs.*` predicates (`uxs.domain.kind`, `uxs.authority.state`, `uxs.mode.companion`, `uxs.link.tier`, `uxs.health.{armed,flags,failsafes,crcerrors,loopdtus,maxloopdtus,rttus}`, `uxs.attitude.{quaternion,rates}`, `uxs.kinematics.{ned,vx,vy,vz,headingdeg}`) and `uxs.vehicle` entity type via `semengine/vocabulary`, reusing built-ins `geo.location.{latitude,longitude,altitude}`, `dc.terms.title`, and `entity.indexing.profile` (`"signal"`).
   - Provide the ADR-102 6-part entity ID builder `<org>.<platform>.robotics.uxs.vehicle.<sysid>` (`sysid` in `1..=254`, `<= 256B`), `EntityDomainDelegation` (`Producer: "bbops"`, `Domain: "uxs"`, `Type: "vehicle"`), and a constant-predicate `graph.Graphable` snapshot payload (`uxs.vehicle.v1`, `IndexingProfileSignal`) wired into `internal/host`.
2. **`bbstack` `0xB1` `custom_mode` & Clean-Room `common.xml` MAVLink v2 Codec (`internal/mavcodec`):**
   - Implement pure-Go pack/unpack for the 32-bit `0xB1` `HEARTBEAT.custom_mode` word (`Magic=0xB1`, `LinkTier`, `HealthFlags`, `VehicleDomain`, `CompanionMode`, `ControlAuthority`), returning typed errors on non-`0xB1` magic or invalid enum nibbles without panicking.
   - Implement MAVLink v2 (`0xFD`) `minimal.xml` + `common.xml` frame encode/decode covering `#0 HEARTBEAT`, `#1 SYS_STATUS`, `#31 ATTITUDE_QUATERNION`, `#32 LOCAL_POSITION_NED`, `#33 GLOBAL_POSITION_INT`, `#76 COMMAND_LONG`, `#77 COMMAND_ACK`, `#84 SET_POSITION_TARGET_LOCAL_NED`, and `#253 STATUSTEXT`, guarded by an automated zero-GPLv3 / zero-`ardupilotmega` dependency check.
3. **Capability Spec:** Add `openspec/specs/mavlink-ingest/spec.md` covering vocabulary invariants, constant-predicate `Graphable` snapshots, `0xB1` bitfield laws, and clean-room `common.xml` framing.

## Scope & Non-Goals

- **In-bounds:** `internal/uxsvocab/`, `internal/mavcodec/`, `internal/host/` vocabulary/payload hook wiring, `cmd/bbops/`, `go.mod`/`go.sum` (if `D1` selects `gomavlib/v3`), `THIRD_PARTY_NOTICES`, `openspec/specs/mavlink-ingest/spec.md`.
- **Non-goals:** Live UDP socket I/O, NATS `bbops.telemetry.<sysid>` publishing, or 1 Hz / edge-burst JetStream batching (`#6` `M1.2`); outbound actuation leases (`#11` `M3.1`); Svelte 5 COP stores (`#8` `M2.1`).

## Definition of Done

- Owner approves `proposal.md` + `design.md` with `OQ1`–`OQ4` rulings and change is archived before ready-for-review.
- Golden-byte round-trip tests, `FuzzDecodeCustomMode` + frame fuzzing (`testing.F`), host `ENTITY_STATES`/GraphQL round-trip test, zero-`ardupilotmega`/zero-GPLv3 dependency guard test, and survivor-free `ateam mutate` runs pass on `internal/uxsvocab` and `internal/mavcodec`.
- `task verify` and `ateam doctor --check` pass green on a clean working tree.
