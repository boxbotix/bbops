# host-runtime Specification

## Purpose
Defines the `bbops` standalone Go host binary CLI contract, embedded NATS/JetStream and `semengine` (`v0.1.0-alpha.7`) SKG runtime lifecycle, ADR-115 GraphQL and dual `output/websocket` streaming planes, and Svelte 5 COP/GCS shell bootstrap invariants.

## Requirements
### Requirement: Host CLI version and flag entrypoint
The `bbops` host binary SHALL parse CLI flags cleanly without panicking, print a deterministic `bbops <version> (<commit>)` line to stdout and return exit code `0` when `-version` is passed, validate the resolved `semengine` configuration and component composition offline and return exit code `0` without binding network listeners when `-validate` is passed, boot the host runtime until context cancellation or termination signal when invoked for execution, and return exit code `2` with diagnostic output on stderr when unknown flags or unexpected positional arguments are supplied.

#### Scenario: Version flag prints release identity and exits zero
- **WHEN** `run` is invoked with `["-version"]`
- **THEN** it writes `bbops <version> (<commit>)\n` to stdout, writes nothing to stderr, and returns exit code `0`

#### Scenario: Validate flag checks SemEngine configuration and composition offline
- **WHEN** `run` is invoked with `["-validate"]`
- **THEN** it validates the default or `-config` flow document and `semengine/composition` graph without starting NATS or HTTP listeners and returns exit code `0`

#### Scenario: Unknown CLI flag or positional argument returns exit code two
- **WHEN** `run` is invoked with an unknown flag such as `["-unknown-flag"]` or an unexpected positional argument such as `["extra"]`
- **THEN** it writes diagnostic output to stderr, writes nothing to stdout, and returns exit code `2`

### Requirement: Svelte 5 COP shell bootstrap metadata
The `ui/` SvelteKit 2 + Svelte 5 Runes shell SHALL expose typed multi-domain UXS vehicle descriptors (`Quadcopter`, `Rover`, `SurfaceBoat`) and deterministic banner formatting without runtime dependencies on legacy `semlink` or `semops` packages.

#### Scenario: Supported UXS domains enumerate Quadcopter, Rover, and SurfaceBoat
- **WHEN** the UI bootstrap module enumerates supported UXS domains and formats the shell banner for a release string
- **THEN** it returns the ordered tuple `["Quadcopter", "Rover", "SurfaceBoat"]` and formats `bbops COP/GCS (<version>) — Quadcopter · Rover · SurfaceBoat`

### Requirement: Embedded NATS JetStream and SemEngine SKG pipeline lifecycle
The `bbops` host runtime SHALL start an embedded NATS server with JetStream enabled (unless an external `-nats-url` is configured), ensure the `ENTITY` stream on `entity.>` with explicit finite `max_age`, `max_bytes`, and `discard: "old"` retention, enforce `ENTITY_STATES` KV single-revision compaction (`History: 1`, `RetentionNoLifecycle`), compose `processor/graph-ingest` (`WriteOwnerOnly`), `processor/graph-index` (`coalesce_ms: 500`), `processor/graph-query`, and `processor/graph-lexical`, gate readiness on startup completion so `graph-ingest` and query responders are active before ready probes succeed, and shut down all services, WebSocket listeners, and embedded NATS cleanly on context cancellation.

#### Scenario: Readiness succeeds only after embedded NATS and graph-ingest pipeline boot
- **WHEN** `bbops` boots in-process on ephemeral loopback ports (`1024..65535`) with a temporary JetStream data directory
- **THEN** readiness and health probes report ready (`200`), `ENTITY` stream carries finite `max_age`/`max_bytes` with `discard: "old"`, `ENTITY_STATES` KV is configured with `History: 1`, and canceling the host context stops `ServiceManager` and embedded NATS cleanly

### Requirement: ADR-115 GraphQL front door and dual output/websocket streaming planes
The `bbops` host runtime SHALL mount `gateway/graph-gateway` at `POST /graph-gateway/graphql` on the `ServiceManager` HTTP front door (serving `entity`, `entitiesByPrefix`, `entitiesByPredicate`, and `pathSearch`), expose `output/websocket` instance #1 (`kind: "nats"`, `bbops.telemetry.>`) delivering uncoalesced frames with 256-frame drop-oldest per-client buffering, expose `output/websocket` instance #2 (`kind: "kv-watch"`, `ENTITY_STATES`) sharing a single `pkg/graphview.View` (`250 ms` = 4 Hz coalesced `put` snapshot followed by `synced` and live deltas) without using `/message-logger/kv/{bucket}/watch` debug SSE, and serve static `ui/build` assets (or dev proxy configuration) without shadowing reserved SemEngine routes.

#### Scenario: Entity ingest flows to SKG WebSocket snapshot/sync and GraphQL query while telemetry streams uncoalesced
- **WHEN** a running `bbops` instance ingests a test entity on `entity.>` and receives a JSON frame on `bbops.telemetry.test`
- **THEN** WebSocket #2 (`ENTITY_STATES`) delivers `put` and `synced` frames for the entity, `POST /graph-gateway/graphql` returns the entity state, and WebSocket #1 (`bbops.telemetry.>`) delivers the uncoalesced `data` frame for `bbops.telemetry.test`

