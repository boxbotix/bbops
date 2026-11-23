# Design: m1-2-mavlink-3lane-ingest

Architect: `gemini (gemini-4-carbon, medium)`

## Context

- `configs/bbops.json:18-29,56-100,215-277`: provisions `ENTITY` (`entity.>`, `256 MiB`, `24h`, `discard: "old"`), `graph-ingest` (`entity_stream` on `entity.>`, `enable_hierarchy: false`), `telemetry-websocket` (`kind: "nats"`, `subject: "bbops.telemetry.>"`, `:8081`), and `graph-websocket` (`kind: "kv-watch"`, `bucket: "ENTITY_STATES"`, `:8082`), with 7 enabled components and no `input` origin node.
- `internal/host/config.go:48-106,173-229,258-305,343-420` & `internal/host/runtime.go:192-205,245-301,359-394`: `RegisterComponents` registers 6 SemEngine factories; `ValidateOffline` enforces `requiredM02Components` + `composition.Validate`; `StartEphemeral` allocates 4 loopback TCP ports (`HTTP`, `TelemetryWS`, `GraphWS`, `Metrics`) without overriding UDP `:14550`; `checkLiveReadiness` gates `/readyz` on `ComponentManager.GetComponentHealth()`.
- `cmd/bbops/main.go:36-78` & `cmd/bbops/main_test.go:243-268,424-444`: CLI flags override HTTP/WS/metrics TCP ports; parallel `go test ./...` runs `cmd/bbops` and `internal/host` concurrently.
- `internal/uxsvocab/entity_id.go:23-52` & `internal/uxsvocab/snapshot.go:53-109,134-200,264-332`: `VehicleEntityID` mints `<org>.<platform>.robotics.uxs.vehicle.<sysid>` (`1..=254`); `NewVehicleSnapshot` initializes finite zero/identity defaults (`Quaternion=[1,0,0,0]`); `VehicleSnapshot` (`uxs.vehicle.v1`, `IndexingProfileSignal`) emits the constant 22-predicate set and rejects `NaN`/`±Inf` or out-of-range lat/lon/heading in `Validate()`.
- `internal/mavcodec/custom_mode.go:278-328` & `internal/mavcodec/frame.go:22-31,67-220,274-343`: `DecodeFrame` decodes single-datagram MAVLink v2 (`0xFD`) frames (`#0,#1,#31,#32,#33,#76,#77,#84,#253`) with 1B payload floor and `sysid`/`compid != 0`; `DecodeHeartbeat` enforces strict `0xB1` vehicle envelopes (`ErrNotBBStackHeartbeat` on GCS/non-`0xB1` peers).
- `PROVENANCE.md:10-16,30-32`: permits Tier-1 `C360Studio/semengine` (`v0.1.0-alpha.7`), `C360Studio/semboids` (`ADR-001` 3-lane split), `boxbotix/bbstack` (`0xB1` `#1 SYS_STATUS`: `load=loop_dt_us`, `errors_comm=crc_errors`, `errors_count1=failsafes`, `errors_count2=max_loop_dt_us`, `errors_count3=rtt_us`), and open literature (Fossen yaw/crab kinematics).
- `semengine v0.1.0-alpha.7` (`component`, `composition`, `natsclient`, `output/websocket` via `go doc`): `component.PortDefinition.External` suppresses orphan-input findings (ADR-100); `component.Lifecycle` implements ADR-108/113 single-start/stop/health rules; `composition.Analyze` treats explicit `streams.ENTITY` as satisfying `graph-ingest` when publishers declare core `NATSPort` outputs on `entity.uxs.upsert`; `natsclient.Client.PublishBatchToStream` drains batch-local JetStream acks asynchronously.

## Goals / Non-Goals

- **Goals:** Ingest multi-`sysid` (`1..=254`) `0xB1` MAVLink v2 UDP packets (`127.0.0.1:14550` default) via `internal/mavlink` across 3 lanes: `20 Hz` uncoalesced kinematics (`bbops.telemetry.<sysid>` -> `WS #1`), `1–5 Hz` (default `1 Hz`) non-blocking `PublishBatchToStream` SKG snapshots (`entity.uxs.upsert` -> `ENTITY_STATES` -> `WS #2`/GraphQL), and instant edge-burst transitions (`bbops.telemetry.events.<sysid>` + out-of-band SKG flush), with ephemeral UDP port isolation in `host`/`cmd/bbops`.
- **Non-Goals:** Outbound `#0`/`#76`/`#84` UDP transmission or ADR-111 `COMMAND_ACK` correlation (`#11` `M3.1`); Svelte 5 stores (`#8` `M2.1`).

## Decisions

### D1. SemEngine Component Composition & Ephemeral UDP Port Isolation

| Option | Cost | Blast radius | Forecloses |
| --- | --- | --- | --- |
| **A. Registered `input` `LifecycleComponent` (`"mavlink-ingest"`, factory `"mavlink-udp"`) enabled in `configs/bbops.json`**: declares `"external": true` UDP `NetworkPort` (`127.0.0.1:14550`) + `NATSPort` outputs (`bbops.telemetry.>`, `entity.uxs.upsert`), adds `PortOverrides.MAVLinkUDPPort` (`<0` disables in isolated unit tests, `>0` overrides) + `-mavlink-udp-port` CLI flag + `ResolvedEndpoints.MAVLinkUDPAddr`, and allocates a free UDP port in `StartEphemeral` | ~90 LOC port/config plumbing across `internal/mavlink`, `internal/host`, `cmd/bbops` | `configs/bbops.json`, `internal/mavlink`, `internal/host`, `cmd/bbops` (8 admitted graph nodes) | Running `host.Start` with default config when `:14550` is already bound unless `-mavlink-udp-port` or `PortOverrides` is supplied |
| **B. Host-managed background worker outside `cfg.Components`**: `host.Start` constructs `mavlink.Adapter` directly after `ServiceManager.StartAll` without adding a factory to `component.Registry` or `configs/bbops.json` | ~45 LOC fewer config helpers | `internal/mavlink`, `internal/host`, `cmd/bbops` (`composition.Graph` stays 7 nodes) | `composition.FlowPaths`/`Mermaid` origin tracing, `ComponentManager` `/health` + `DataFlow` visibility, and declarative JSON `skg_hz`/port config |

Adopter seam: `#8` (`M2.1`) and `#11` (`M3.1`) consume `rt.Endpoints()`, `WS #1/#2`, and a shared peer-socket handle. Under A, callers of `host.StartEphemeral` automatically receive a collision-free `rt.Endpoints().MAVLinkUDPAddr` and `bbops -validate` checks the full 8-node graph; callers doing nothing get default `127.0.0.1:14550` in production and isolated ephemeral UDP in tests. Under B, `composition` still reports no origin node for `bbops.telemetry.>` or `entity.>`.

### D2. Lane 1 (`bbops.telemetry.<sysid>`) Session Admission & Publish Cadence

| Option | Cost | Blast radius | Forecloses |
| --- | --- | --- | --- |
| **A. Gate `sysid` session birth on first valid `0xB1` `#0 HEARTBEAT` (`compid == 1`) and publish fused latest-wins JSON immediately on each `#31`/`#32`/`#33` arrival**: drops pre-`HEARTBEAT` or non-`0xB1` packets cleanly; emits `{"sysid","domain","seq","msg_id","quat_wxyz","body_rates","ned_xyz","ned_vel","lat_lon_alt","has_global_pos","hdg_deg","timestamp"}` (`at-most-once` core NATS) with `<1 ms` latency | ~35 LOC session gate + fused struct serializer | `internal/mavlink`, `#8` `telemetry.svelte.ts` | Streaming raw `#31/#32` kinematics on `WS #1` from a peer that never transmits a valid `0xB1` `HEARTBEAT` |
| **B. Fixed `50 ms` (`20 Hz`) server-side ticker coalescing `#31/#32/#33` per `sysid` without requiring prior `#0 HEARTBEAT`**: buffers incoming kinematics and flushes once per `50 ms` tick even before domain identity is known | ~40 LOC per-`sysid` ticker loop | `internal/mavlink`, `#8` `telemetry.svelte.ts` (`domain` nullable on `WS #1`) | Sub-millisecond attitude/velocity forwarding (adds `0–50 ms` server phase jitter) and strict `0xB1`-only peer admission |

Adopter seam: `#8` (`telemetry.svelte.ts`) overwrites per-`sysid` kinematic state in place from `WS #1` (`bbops.telemetry.<sysid>`). Under A, `domain` is always a valid UXS domain and test/SITL senders must send at least one `#0 HEARTBEAT` (`1 Hz` + boot burst in `bbnav`) to admit a `sysid`; if a test sends `#31` alone without `#0`, nothing is published. Under B, `telemetry.svelte.ts` must handle unknown-domain ghost `sysid`s from stray UDP peers.

### D3. Lane 2 Coalesced SKG Batching & Lane 3 Out-of-Band Edge-Burst Queue

| Option | Cost | Blast radius | Forecloses |
| --- | --- | --- | --- |
| **A. Cap-2 drop-oldest periodic batch channel (`1–5 Hz`, default `1 Hz`) + coalesced urgent-`sysid` edge trigger**: UDP loop updates in-memory vehicle state, immediately publishes `bbops.telemetry.events.<sysid>` via core NATS on any edge (`armed`, `ControlAuthority`, `CompanionMode`, `LinkTier`, `HealthFlags`, `failsafes` delta, `#253 STATUSTEXT`), and marks the `sysid` in an urgent dirty set drained ahead of periodic batches via `PublishBatchToStream("entity.uxs.upsert", msgs)` | ~75 LOC publisher worker + urgent dirty-set merge | `internal/mavlink` (`publisher.go`) | Losing an edge-triggered SKG state update behind a queued periodic batch during transient JetStream backpressure |
| **B. Single shared cap-2 `chan [][]byte` (`drop-newest` on full) for both periodic ticks and edge-burst snapshots**: both `1 Hz` ticks and Lane 3 edges push pre-marshaled `[][]byte` slices onto one channel | ~35 LOC simpler worker | `internal/mavlink` (`publisher.go`) | Guaranteeing immediate post-edge SKG convergence when `batchCh` already holds 2 unacked periodic batches (waits up to `1000 ms` for next tick) |

Adopter seam: `#8` (`skg.svelte.ts`) and `#11` (`M3.1` preemption banner) rely on `WS #1` (`bbops.telemetry.events.<sysid>`) for `<2 ms` alert timelines and `WS #2` (`ENTITY_STATES`) for authoritative state convergence. Under A, consumers doing nothing get both instant `WS #1` events and guaranteed latest-state `ENTITY_STATES` reconciliation even under burst load; under B, transient JetStream stall can drop the out-of-band SKG flush until the next periodic tick.

### D4. Heading (`hdg_deg` / `uxs.kinematics.headingdeg`), Packet Sanitization & Peer Socket Reuse

| Option | Cost | Blast radius | Forecloses |
| --- | --- | --- | --- |
| **A. `20 Hz` ZYX Euler yaw $\psi \in [0,360)$ from `#31 ATTITUDE_QUATERNION` with `#33.Hdg` (`<36000` cdeg) fallback + per-packet finite/bounds guard + tracked peer `netip.AddrPort` per `sysid`**: computes nose/bow heading from unit quaternion (`#33.Hdg` used only before first valid `#31`; `65535` ignored), drops `NaN`/`±Inf`/out-of-range packets (`CountError()`) without mutating last-good state, and records peer `AddrPort` on the bound `*net.UDPConn` for `#11` | ~45 LOC Fossen yaw + bounds check + peer table | `internal/mavlink`, `#9` USV crab-angle HUD, `#11` `M3.1` TX | Treating `#33.Hdg` (`5 Hz` GPS course/heading) as overriding `20 Hz` IMU attitude quaternion yaw once `#31` is active |
| **B. Populate `HeadingDeg` solely from `#33 GLOBAL_POSITION_INT.Hdg` (`5 Hz`, `0` until GPS fix) without tracking peer `AddrPort` in `M1.2`**: copies `float64(Hdg)/100` when `Hdg <= 36000` and leaves outbound socket design to `#11` | ~30 LOC less in `M1.2` | `internal/mavlink`, `#9` USV HUD, `#11` `M3.1` | Non-zero `20 Hz` bow heading in indoor/local-NED bench runs without `#33` and `20 Hz` phase-aligned USV crab-angle ($\chi - \psi$) rendering in `#9` |

Adopter seam: `#9` (`M2.2` USV crab-angle HUD) compares NED velocity course $\chi = \operatorname{atan2}(v_E, v_N)$ against `hdg_deg` ($\psi$), and `#11` (`M3.1`) transmits `#0`/`#76`/`#84` back to `bbnav` peers. Under A, `#9` receives phase-aligned `20 Hz` `ned_vel` + `hdg_deg` even prior to GPS lock and `#11` reuses the symmetric `:14550` UDP socket; doing nothing in `#9`/`#11` works out of the box.

## Risks / Trade-offs

- **UDP read-loop head-of-line stall:** Calling `PublishBatchToStream` inline on the UDP goroutine would drop kernel socket datagrams during JetStream fsync spikes. Mitigation: UDP goroutine performs only `DecodeFrame`, state mutation, core NATS `Publish`, and non-blocking channel/dirty-set signaling.
- **Parallel test UDP `:14550` collisions:** Enabling `mavlink-ingest` in `configs/bbops.json` collides across concurrent tests unless overridden. Mitigation: `StartEphemeral` reserves an ephemeral loopback UDP port alongside its 4 TCP ports (`D1`).

## Migration Plan

Additive `internal/mavlink` package composed into `configs/bbops.json`, `internal/host`, and `cmd/bbops`. Existing HTTP, GraphQL, and dual-WebSocket endpoints remain unchanged; rollback is reverting the merge commit.

## Open Questions

1. **OQ1 (`D1`) [Owner ruling required]:** Compose `internal/mavlink` as a registered SemEngine `input` `LifecycleComponent` (`"mavlink-ingest"`, `"external": true` UDP port + `-mavlink-udp-port` + `StartEphemeral` UDP allocation, Option A) or as a host-managed worker outside `cfg.Components` (Option B)?
2. **OQ2 (`D2`) [Owner ruling required]:** Gate `sysid` session admission on a valid `0xB1` `#0 HEARTBEAT` (`compid == 1`) and publish fused kinematics immediately per `#31/#32/#33` packet (Option A), or run a fixed `50 ms` (`20 Hz`) server-side coalescing ticker without `HEARTBEAT` admission gating (Option B)?
3. **OQ3 (`D3`) [Owner ruling required]:** Use a cap-2 drop-oldest periodic batch channel (`1–5 Hz`, default `1 Hz`) paired with an urgent dirty-`sysid` edge flush so Lane 3 transitions never drop behind periodic batches (Option A), or a single shared cap-2 drop-newest batch channel (Option B)?
4. **OQ4 (`D4`) [Owner ruling required]:** Derive `HeadingDeg` ($\psi \in [0,360)$) at `20 Hz` from `#31 ATTITUDE_QUATERNION` with `#33.Hdg` fallback + per-packet finite/bounds sanitization + peer `netip.AddrPort` tracking for `#11` (Option A), or populate `HeadingDeg` only from `5 Hz` `#33.Hdg` and defer peer socket tracking to `#11` (Option B)?
