# Design: m1-1-uxs-vocab-codec

Architect: `gemini (gemini-4-carbon, medium)`

## Context

- `go.mod:1-38`: `module github.com/boxbotix/bbops` (`go 1.26.11`), pins `github.com/c360studio/semengine v0.1.0-alpha.7` with zero MAVLink packages.
- `PROVENANCE.md:10-26,30-32` & `THIRD_PARTY_NOTICES:1-48`: permits Tier-1 MIT `mavlink/mavlink` (`minimal.xml` + `common.xml`), `boxbotix/bbstack`, `C360Studio/semengine`, `C360Studio/semboids`, and MIT `bluenviron/gomavlib` restricted to `minimal`/`common` (`:15`); strictly bans GPLv3 repos and `ardupilotmega.xml` (`:20-26`).
- `internal/host/runtime.go:45-46,86,196-201`, `internal/host/config.go:142-169`, & `cmd/bbops/main.go:71-89`: `host.Options.RegisterPayloads` wires payloads into `payloadregistry.Registry`, while `cmd/bbops/main.go:81-89` passes `nil` and neither `PrepareConfig` nor `Start` registers a consumer `vocabulary.PackSpec`.
- `configs/bbops.json:18-29,56-100`: `ENTITY` (`entity.>`) feeds `graph-ingest` (`enable_hierarchy: false`) -> `ENTITY_STATES` -> `graph-index` (`coalesce_ms: 500`).
- `C360Studio/semengine` @ `3d89a31775219e7f7aaa44ab10892bbc804ae08e` (`v0.1.0-alpha.7`):
  - `vocabulary/predicate_contract.go:9-13,65-118`: 3-part lower-kebab ASCII grammar `[a-z][a-z0-9]*(-[a-z0-9]+)*` (`<= 64B`/segment, `<= 194B` total; underscores fail with `PredicateReasonSegmentCharacter`).
  - `vocabulary/predicates.go:30-37,113-143,156-215` & `vocabulary/pack.go:81-116,134-155,186-266`: built-ins `GeoLocationLatitude`, `GeoLocationLongitude`, `GeoLocationAltitude`, `EntityIndexingProfile`, `IndexingProfileSignal` (`"signal"`), `DCTermsTitle`; closed 7-datatype set (`string`, `entity_id`, `int`, `float`, `bool`, `datetime`, `json`); strict `LoadPackJSON` + atomic `RegisterPack`.
  - `pkg/types/entity_id.go:11-14,48-55,76-156` & `pkg/types/entity_domain_authority.go:7-13,34-50`: ADR-102 6-part `<org>.<platform>.<system>.<domain>.<type>.<instance>` (`<= 256B`, `[A-Za-z0-9][A-Za-z0-9_-]*`, accepting numeric `sysid` `"1".."254"` and reserving `"group"|"container"|"level"`), plus `EntityDomainDelegation{Producer, Domain, Type}`.
  - `payloadregistry/registry.go:53-72` & `processor/graph-ingest/ingest.go:298-365`: `Registration.IndexingProfile = vocabulary.IndexingProfileSignal` stamps `entity.indexing.profile = "signal"` under reserved `graph.SourceIndexingProfile` (`graph/sources.go:22`), whereas producer-emitted `SourceIndexingProfile` triples are rejected.
  - `processor/graph-index/owner_reconcile.go:104-118` & `processor/graph-index/index.go:364-366,451`: `reconcilePredicateIndex` reconciles `plan.predicatesUsed` with `overwriteExisting: false`, skipping `PREDICATE_INDEX` KV writes when every snapshot emits a constant predicate set.
  - `gateway/graph-gateway/schema.graphql:16` & `vocabulary/export/object.go:189-201,304-309`: GraphQL `Triple.object: JSON` and `ENTITY_STATES` KV JSON serialize Go slices/structs natively, whereas `vocabulary/export` (`:197-199`) emits `^^rdf:JSON` only when `Triple.Object` is a JSON-valid `string` (otherwise `fmt.Sprintf("%v", v)`).
- `C360Studio/semboids` @ `37dbdb088438a2601c8556d012ab2c33bec85da2`: `internal/boidgraph/vocabulary.go:20-32` & `internal/boidgraph/payload.go:42-140` (`Graphable` `Entity` + `RegisterPayloads`).
- `boxbotix/bbstack` @ `8547b08b1be49e94d47aac210fd974d240a061c6`: `crates/bbwire/src/lib.rs:24-77` & `crates/bbcore/src/mux.rs:5-15,53-74` (`VehicleDomain`, `CompanionMode`, `ControlAuthority`, `McuTelemetry`, `LinkStatus`).
- `bluenviron/gomavlib/v3` @ `33ff3fce8d7f2e832e08e08cf02b946de821208e` (`v3.3.5`, MIT): `go.mod:1-20` requires `kong`, `pion/transport`, `testify`, `go.bug.st/serial` at module root (pruned from compiled `bbops` packages when importing only `pkg/frame` + `pkg/dialects/common`).

## Goals / Non-Goals

- **Goals:** Register the 18 `uxs.*` predicates + `uxs.vehicle` entity type, enforce ADR-102 `<org>.<platform>.robotics.uxs.vehicle.<sysid>` identity and constant-predicate `Graphable` snapshots (`IndexingProfileSignal`), implement deterministic `0xB1` `custom_mode` + clean-room MAVLink v2 (`0xFD`) `common.xml` encode/decode for the 9 contract messages (`#0,#1,#31,#32,#33,#76,#77,#84,#253`), and guard `go.mod`/build deps against `ardupilotmega` and GPLv3.
- **Non-Goals:** UDP socket loops, NATS `bbops.telemetry.<sysid>` fan-out, or 1 Hz/edge-burst publishing (`#6`); outbound command leases (`#11`); UI stores (`#8`).

## Decisions

### D1. MAVLink v2 (`0xFD`) `common.xml` Wire Codec Implementation

| Option | Cost | Blast radius | Forecloses |
| --- | --- | --- | --- |
| **A. Wrap `bluenviron/gomavlib/v3` (`v3.3.5`) `pkg/frame` + `pkg/dialects/common`**: domain types in `internal/mavcodec` translate to/from the 9 `common` structs using `frame.ReadWriter` (`OutVersion: V2`); test guards `go list -deps ./...` + AST against `ardupilotmega`/GPLv3 | 1 MIT module pin (`THIRD_PARTY_NOTICES`) + ~180 LOC adapter | `go.mod`, `go.sum`, `internal/mavcodec`, `THIRD_PARTY_NOTICES` | Zero-new-module `go.mod` footprint (`gomavlib` root `go.mod` lists `serial`/`pion` though Go package pruning excludes them from `bbops` builds) |
| **B. Zero-dependency clean-room Go MAVLink v2 wire codec**: pure-Go `0xFD` parser/writer, X.25 CRC + `CRC_EXTRA` table, trailing-zero truncation, and little-endian wire codecs for the exact 9 `minimal.xml`/`common.xml` messages | ~280 LOC wire serializer + golden byte vectors against `common.xml` | `internal/mavcodec` only (`go.mod` unchanged) | Instant access to additional `common.xml` messages without adding a struct codec entry |

Adopter seam: Callers in `#6` (`M1.2`) and `#11` (`M3.1`) import only `internal/mavcodec` domain structs/functions (`EncodeFrame`, `DecodeFrame`, `Message`); neither option leaks `gomavlib` types across `internal/mavcodec`. Doing nothing in `#6` works identically under A or B, except under A any contributor importing `gomavlib/v3/pkg/dialects/ardupilotmega` immediately fails the clean-room dependency test. **Ruled Option A (owner, 2026-11-23).**

### D2. `Triple.Object` Representation for `DataTypeJSON` (`"json"`) Predicates

| Option | Cost | Blast radius | Forecloses |
| --- | --- | --- | --- |
| **A. Native Go JSON-marshalable values (`HealthFlags` struct, `[4]float64`, `[3]float64`)**: stored directly in `Triple.Object` so `ENTITY_STATES` KV JSON (`WS #2`) and GraphQL `scalar JSON` (`schema.graphql:16`) emit structured JSON arrays/objects | Zero wrapper parsing in Go or TypeScript | `internal/uxsvocab`, `#8` `skg.svelte.ts` | `semengine/vocabulary/export` (`object.go:197-199`) emitting `^^rdf:JSON` Turtle without a string conversion pass |
| **B. Pre-encoded JSON `string` (`"[1,0,0,0]"`, `'{"clock_sync_locked":true,...}'`)**: `Triple.Object` holds a `string` satisfying `json.Valid` for `vocabulary/export` `^^rdf:JSON` | `json.Marshal` per snapshot + `JSON.parse` in UI | `internal/uxsvocab`, `#8` `skg.svelte.ts` (`WS #2` & GraphQL receive stringified JSON) | Direct typed JSON array/object destructuring on `WS #2` and GraphQL responses without `JSON.parse` |

Adopter seam: `#8` (`skg.svelte.ts`) reads `uxs.health.flags`, `uxs.attitude.quaternion`, `uxs.attitude.rates`, and `uxs.kinematics.ned` from `WS #2` (`ENTITY_STATES`) and `POST /graph-gateway/graphql`. Under A, consumers read native JSON arrays/objects directly (and forgetting `JSON.parse` cannot fail); under B, consumers that omit `JSON.parse` get a string instead of a tuple/object. **Ruled Option A (owner, 2026-11-23).**

### D3. Vocabulary Pack Authoring & Host Registration Lifecycle

| Option | Cost | Blast radius | Forecloses |
| --- | --- | --- | --- |
| **A. Embedded `uxs_pack.json` (`LoadPackJSON`) + explicit idempotent `uxsvocab.Register()` / `RegisterPayloads` in `host.PrepareConfig` & `host.Start`**: validates JSON pack during `-validate` and startup via `sync.Once` | 1 embedded JSON file + ~40 LOC loader/wiring | `internal/uxsvocab`, `internal/host`, `cmd/bbops` | Implicit registration when a test uses `vocabulary.Lookup` without calling `uxsvocab.Register()` or `host.PrepareConfig` |
| **B. Go `vocabulary.PackSpec` literal in `package uxsvocab` `init()` + default `uxsvocab.RegisterPayloads` in `host.Start`**: auto-registers on package import (matching `semboids/internal/boidgraph/vocabulary.go:20`) | ~70 LOC Go struct table | `internal/uxsvocab`, `internal/host` | Inspecting the `uxs.*` ontology as a standalone JSON artifact or exercising `vocabulary.LoadPackJSON` in production |

Adopter seam: Under both options, `host.Start` registers `uxsvocab.RegisterPayloads` by default (chaining any caller-supplied `opts.RegisterPayloads`) so `bbops` and ephemeral host tests decode `uxs.vehicle.v1` automatically with zero caller boilerplate. Under A, standalone unit tests outside `host` call `uxsvocab.Register()` (idempotent); under B, importing `internal/uxsvocab` mutates the process-global `semengine/vocabulary` registry at init time. **Ruled Option A (owner, 2026-11-23).**

### D4. `HEARTBEAT` (`#0`) `0xB1` Envelope Validation Strictness

| Option | Cost | Blast radius | Forecloses |
| --- | --- | --- | --- |
| **A. Strict `0xB1` vehicle envelope**: require `custom_mode[31:24] == 0xB1`, `base_mode & MAV_MODE_FLAG_CUSTOM_MODE_ENABLED != 0`, `autopilot == MAV_AUTOPILOT_GENERIC (0)`, exact `HEARTBEAT.type` <-> `VehicleDomain` match (`2<->Quadcopter`, `10<->Rover`, `11<->SurfaceBoat`), and valid enum nibbles, returning typed `*DecodeError` | ~25 LOC cross-field checks + error kinds | `internal/mavcodec`, `#6` `internal/mavlink` | Accepting synthetic `0xB1` heartbeats that leave `HEARTBEAT.type = MAV_TYPE_GENERIC (0)` or omit `CUSTOM_MODE_ENABLED` |
| **B. Lenient `HEARTBEAT` envelope**: validate only `custom_mode[31:24] == 0xB1` and enum nibble bounds (`LinkTier <= 3`, `VehicleDomain <= 2`, `CompanionMode <= 3`, `ControlAuthority <= 3`), ignoring `HEARTBEAT.type`, `autopilot`, and `CUSTOM_MODE_ENABLED` | ~10 LOC fewer checks | `internal/mavcodec`, `#6` `internal/mavlink` | Catching inconsistent `HEARTBEAT.type` vs `VehicleDomain` bugs from misconfigured SITL/bench peers at the codec boundary |

Adopter seam: On UDP links with peer GCS heartbeats (`MAV_TYPE_GCS = 6`, `custom_mode = 0`), both options return a typed non-`0xB1` error (`errors.Is(err, ErrNotBBStackHeartbeat)`) so `#6` ignores non-vehicle peers without panicking. Under A, `#6` SITL fixtures and `bbnav` peers must populate matching `HEARTBEAT.type`, `MAV_AUTOPILOT_GENERIC`, and `MAV_MODE_FLAG_CUSTOM_MODE_ENABLED` or frame validation rejects them cleanly. **Ruled Option A (owner, 2026-11-23).**

## Risks / Trade-offs

- **Global `semengine/vocabulary` singleton (`vocabulary/pack.go:186-266`):** Repeated `RegisterPack` calls with identical pack names fail unless guarded. Mitigation: guard `uxsvocab.Register()` with `sync.Once` while exposing pure `PackSpec()`/`Validate()` for repeatable unit tests and mutation checks.
- **Float edge values (`NaN`/`±Inf`) in MAVLink payloads:** Encoding `NaN`/`±Inf` into JSON triples breaks `encoding/json.Marshal` inside `graph-ingest`. Mitigation: reject or sanitize non-finite floats at `VehicleSnapshot.Validate()` before `Triples()` ingestion.

## Migration Plan

Additive packages (`internal/uxsvocab`, `internal/mavcodec`) and default vocabulary/payload registration in `internal/host`. Existing `M0.2` CLI/HTTP/WS contracts remain backwards-compatible; rollback is reverting the merge commit.

## Open Questions

1. **Resolved (`OQ1`–`OQ4`, owner 2026-11-23):** D1 Option A (`bluenviron/gomavlib/v3` `v3.3.5` `pkg/frame` + `pkg/dialects/common` behind `internal/mavcodec` + automated clean-room dependency test), D2 Option A (native Go structs/arrays in `Triple.Object` for `DataTypeJSON` predicates), D3 Option A (embedded `uxs_pack.json` via `LoadPackJSON` + idempotent `uxsvocab.Register()` and default registration in `host.PrepareConfig` & `host.Start` without requiring `semvocab`), and D4 Option A (strict `0xB1` vehicle `HEARTBEAT` envelope checks with typed `*DecodeError`).
