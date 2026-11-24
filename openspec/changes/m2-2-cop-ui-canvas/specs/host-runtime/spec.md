## MODIFIED Requirements

### Requirement: ADR-115 GraphQL front door and dual output/websocket streaming planes
The `bbops` host runtime SHALL mount `gateway/graph-gateway` at `POST /graph-gateway/graphql` on the `ServiceManager` HTTP front door (serving `entity`, `entitiesByPrefix`, `entitiesByPredicate`, and `pathSearch`), expose `output/websocket` instance #1 (`kind: "nats"`, `bbops.telemetry.>`) delivering uncoalesced frames with 256-frame drop-oldest per-client buffering, expose `output/websocket` instance #2 (`kind: "kv-watch"`, `ENTITY_STATES`) sharing a single `pkg/graphview.View` (`250 ms` = 4 Hz coalesced `put` snapshot followed by `synced` and live deltas) without using `/message-logger/kv/{bucket}/watch` debug SSE, route same-origin WebSocket upgrades on `/ws/telemetry` and `/ws/graph` at the HTTP front door once runtime readiness is admitted (`503` prior to readiness) so compiled `ui/build` assets connect without external proxy processes, and serve static `ui/build` assets without shadowing reserved SemEngine or `/ws/*` routes.

#### Scenario: Entity ingest flows to SKG WebSocket snapshot/sync and GraphQL query while telemetry streams uncoalesced
- **WHEN** a running `bbops` instance ingests a test entity on `entity.>` and receives a JSON frame on `bbops.telemetry.test`
- **THEN** WebSocket #2 (`ENTITY_STATES`) delivers `put` and `synced` frames for the entity, `POST /graph-gateway/graphql` returns the entity state, and WebSocket #1 (`bbops.telemetry.>`) delivers the uncoalesced `data` frame for `bbops.telemetry.test`

#### Scenario: Standalone HTTP front door serves compiled COP UI and upgrades same-origin WebSocket routes when ready
- **WHEN** `bbops` boots with a compiled `ui/build` directory and clients request `/ws/telemetry` and `/ws/graph` on the `ServiceManager` HTTP front door before and after readiness
- **THEN** pre-ready requests to `/`, `/ws/telemetry`, and `/ws/graph` return `503 Service Unavailable`, and post-ready WebSocket upgrades on `ws://<http-host>/ws/telemetry` and `ws://<http-host>/ws/graph` stream `bbops.telemetry.>` frames and `ENTITY_STATES` `put`/`synced` frames identically to the underlying `output/websocket` listeners while `/` serves `ui/build/index.html`
