# Proposal: m0-2-semengine-host
Approved-by: Coby Leuschke (2026-11-23) sha256:45e8ca3a6a61
Architect: gemini (gemini-4-carbon, medium)

## Track

`design`: adds runtime, HTTP/GraphQL, and dual-WebSocket Requirements to `openspec/specs/host-runtime/spec.md`, replaces the `M0.1` CLI boot path (`cmd/bbops/main.go:35-36`), and configures JetStream stream retention bounds and readiness surfaces (#3 / Epic #1).

## Why

`cmd/bbops/main.go:35-36` currently exits `0` immediately after flag parsing with no runtime substrate (`// shortcut: M0.1 entrypoint exits 0 after flag validation, wire embedded NATS + SemEngine host runtime in M0.2 (#3)`). Before `M1` MAVLink edge ingest (#5) or `uxs.*` vocabulary registration (#6) can land, `bbops` needs an embedded NATS + JetStream lifecycle and `semengine v0.1.0-alpha.7` Semantic Knowledge Graph (SKG) host exposing ADR-115 HTTP/GraphQL and dual-WebSocket planes.

## What Changes

1. Pin `github.com/c360studio/semengine v0.1.0-alpha.7` (`3d89a31775219e7f7aaa44ab10892bbc804ae08e`) and embedded `github.com/nats-io/nats-server/v2` in `go.mod`.
2. Boot an in-process NATS + JetStream server (or attach to `-nats-url`), enforce `kvcatalog` `ENTITY_STATES` retention (`History: 1`, `RetentionNoLifecycle`), ensure the `ENTITY` stream (`entity.>` with explicit finite `max_age`, `max_bytes`, `discard: "old"`), and compose `processor/graph-ingest` (`WriteOwnerOnly`), `processor/graph-index` (`coalesce_ms: 500`), `processor/graph-query`, and `processor/graph-lexical`.
3. Expose ADR-115 surfaces:
   - Request/response front door on `ServiceManager` (`POST /graph-gateway/graphql` for `entity`, `entitiesByPrefix`, `entitiesByPredicate`, `pathSearch`, plus readiness/health probes).
   - `output/websocket` #1 (`telemetry-websocket`, `kind: "nats"`, `bbops.telemetry.>`): uncoalesced frames with 256-frame drop-oldest per-client buffer.
   - `output/websocket` #2 (`graph-websocket`, `kind: "kv-watch"`, `ENTITY_STATES`): shared `pkg/graphview.View` (`250 ms` = 4 Hz coalesced `put` snapshot -> `synced` -> live `put`/`delete`/`poisoned` deltas). Never use `/message-logger/kv/{bucket}/watch` debug SSE for UI clients.
   - Static/dev UI serving for `ui/build` and `ui/Caddyfile` / Vite proxy wiring.
4. Update `openspec/specs/host-runtime/spec.md` and `Taskfile.yml` formatting paths for `internal/`.

## Scope & Non-Goals

- **In-bounds:** `go.mod`, `go.sum`, `cmd/bbops/`, `internal/host/`, flow config + `ui/Caddyfile` / `ui/vite.config.ts` proxy, `Taskfile.yml`, `THIRD_PARTY_NOTICES`, `openspec/specs/host-runtime/spec.md`.
- **Non-goals:** MAVLink UDP listener or `uxs.*` vocabulary registration (#5, #6 `M1`); `processor/rule` or spatial/temporal indexes (`semengine` `alpha.9`/`alpha.10`); CI/ruleset setup (Phase 2).

## Definition of Done

- Owner approves `proposal.md` + `design.md` with `OQ1`–`OQ4` rulings and change is archived before ready-for-review.
- In-process integration test on ephemeral ports (`1024..65535`) boots `bbops`, verifies readiness/health, publishes a test entity via `entity.>` -> `ENTITY_STATES`, observes `put` + `synced` on WS #2, queries it via `POST /graph-gateway/graphql`, and observes an uncoalesced message on WS #1 (`bbops.telemetry.test`).
- `ateam mutate` detection recorded on host config/lifecycle wiring; `task verify` and `ateam doctor --check` pass green.
