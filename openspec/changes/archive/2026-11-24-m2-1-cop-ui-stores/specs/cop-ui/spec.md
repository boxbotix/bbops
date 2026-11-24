## ADDED Requirements

### Requirement: Latest-wins kinematic store and bounded transition event ring buffer
The `ui/src/lib/stores/telemetry.svelte.ts` store SHALL maintain a resilient WebSocket #1 connection (`bbops.telemetry.>`) that overwrites the latest per-`sysid` (`1..=254`) kinematic snapshot (`quat_wxyz`, `body_rates`, `ned_xyz`, `ned_vel`, `lat_lon_alt`, `has_global_pos`, `hdg_deg`, `timestamp`) in place without unbounded array growth or `20 Hz` per-frame DOM `$state` invalidation, ignores out-of-order kinematic frames with older timestamps for the same `sysid`, records discrete `bbops.telemetry.events.<sysid>` `TransitionEvent`s (`heartbeat_edge`, `failsafe_increment`, `statustext`) in a bounded ring buffer (default `100` entries), transmits periodic `< 30 s` (`25 s` default) application keepalive frames while open, and reconnects automatically using `500 ms -> 30 s` exponential backoff.

#### Scenario: Per-sysid kinematic frames overwrite in place and drop older out-of-order timestamps
- **WHEN** WebSocket #1 delivers rapid `20 Hz` `data` frames on `bbops.telemetry.<sysid>` across multiple `sysid`s, including a delayed packet carrying a `timestamp` strictly less than that `sysid`'s current snapshot
- **THEN** the store updates each `sysid`'s latest kinematic snapshot in place without growing history arrays or firing per-frame DOM reactivity, and discards the stale out-of-order packet without regressing `quat_wxyz`, `ned_xyz`, `ned_vel`, or `hdg_deg`

#### Scenario: Discrete Lane 3 transition events append to a bounded ring buffer
- **WHEN** WebSocket #1 delivers more than `100` discrete `bbops.telemetry.events.<sysid>` frames (`DirectCrsfOverride`, `McuFailsafe`, `teleop_deadman_clamped`, failsafe increments, and `STATUSTEXT`)
- **THEN** the store retains the newest `100` transition events in arrival order and evicts the oldest entries deterministically

#### Scenario: Telemetry WebSocket sends sub-30s keepalive frames and reconnects with exponential backoff
- **WHEN** the telemetry WebSocket remains open without outbound traffic across `25 s` intervals and later closes unexpectedly (`1006`) across consecutive retry failures
- **THEN** the client transmits a keepalive frame every `25 s` (`< 30 s`) so `output/websocket`'s `60 s` `ReadMessage` deadline never expires on idle links, schedules reconnect attempts along `500 ms -> 30 s` capped exponential backoff, and resets backoff to `500 ms` after a clean connection is established

### Requirement: Gapless ENTITY_STATES SKG store, external contact separation, and GraphQL client
The `ui/src/lib/stores/skg.svelte.ts` and `ui/src/lib/graphql.ts` modules SHALL maintain a WebSocket #2 `kv-watch` connection on `ENTITY_STATES` (with `< 30 s` keepalive and `500 ms -> 30 s` exponential reconnect backoff) that reconciles `put` snapshot frames, the `synced` barrier, and live `put`, `delete`, and `poisoned` frames into a reactive `SvelteMap<string, CopEntityState>` keyed by 6-part entity ID, narrowing `<org>.<platform>.robotics.uxs.vehicle.<sysid>` (`1..=254`) entities with valid `uxs.*` triples into `VehicleEntityState` while keeping non-UXS/external COP contacts distinct with unknown-preserving provenance/staleness metadata (`source`, `observedAt`, `receivedAt`, `expiresAt`, `uncertaintyMeters`, `feedStatus`) excluded from vehicle command selectors, paired with a typed single-root-field `POST /graph-gateway/graphql` client (`entity`, `entitiesByPrefix`, `entitiesByPredicate`, `pathSearch`).

#### Scenario: Initial snapshot and reconnect stage updates until the synced barrier without blanking live state
- **WHEN** WebSocket #2 connects initially or reconnects after a disconnect, delivering a burst of `put` snapshot frames followed by `{"type":"synced","bucket":"ENTITY_STATES"}` where one previously cached entity was deleted while offline
- **THEN** prior live `CopEntityState` entries remain readable without blanking during reconnect until `synced` arrives, at which point the store atomically swaps in the reconciled snapshot and prunes the vanished entity key

#### Scenario: Live put, delete, and poisoned frames reconcile UXS vehicle selectors accurately
- **WHEN** post-`synced` `put`, `delete`, and `poisoned` frames arrive for `<org>.<platform>.robotics.uxs.vehicle.<sysid>` entities carrying `uxs.*` and `geo.location.*` triples
- **THEN** `put` parses strongly typed `domain`, `authority`, `companionMode`, `linkTier`, `armed`, `healthFlags`, `sysStatus`, kinematics, and coordinates into `VehicleEntityState`, `delete` removes the entity from both `entities` and vehicle selectors, and `poisoned` flags the entity's `poisoned` error metadata and withholds it from healthy commandable vehicle selection

#### Scenario: External COP contacts coexist with unknown-preserving metadata and never enter vehicle command selectors
- **WHEN** `ENTITY_STATES` delivers non-UXS / external contact entities (such as `cop.*` tracks with partial or absent `source`, `observedAt`, `receivedAt`, `expiresAt`, `uncertaintyMeters`, or `feedStatus` triples) and the operator selects one of those external contacts
- **THEN** the store retains the external contact in `entities` with absent provenance/staleness fields left `null` or `"unknown"` (never fabricated defaults), and commandable `VehicleEntityState` selectors return `null` so external contacts never enter vehicle actuation paths

#### Scenario: GraphQL client executes single-root-field inspector queries and surfaces damaged entity extensions
- **WHEN** the UI invokes `entity`, `entitiesByPrefix`, `entitiesByPredicate`, or `pathSearch` against `POST /graph-gateway/graphql`
- **THEN** `graphql.ts` sends a single-root-field GraphQL document per request, parses typed entity/triple/edge payloads alongside optional `extensions.damagedEntities`, and surfaces HTTP or GraphQL `errors` deterministically

### Requirement: Deterministic COP viewport controller verbs and URL hash serialization
The `ui/src/lib/stores/viewport.svelte.ts` module SHALL expose pure TypeScript controller verbs (`selectEntity`, `tetherEntity`, `setChaseLock`, `setDomainFilter`, `toggleLayer`, `setDeclutter`) decoupled from DOM event handlers and serialize/hydrate viewport state deterministically via URL hash fragments (`#...`).

#### Scenario: Controller verbs mutate selection, tether, chase lock, domain filters, layers, and declutter deterministically
- **WHEN** callers invoke `selectEntity`, `tetherEntity`, `setChaseLock`, `setDomainFilter`, `toggleLayer`, and `setDeclutter` programmatically without DOM events (as shared by keyboard hotkeys and MCP/copilot tools)
- **THEN** reactive viewport state transitions deterministically (including clearing chase lock when camera tether is cleared and preserving domain/layer set invariants)

#### Scenario: Viewport state round-trips through URL hash serialization and sanitizes invalid fragments
- **WHEN** non-default viewport state is serialized to a URL hash string and applied to a fresh viewport store, or a malformed URL hash containing unknown domains, bogus booleans, or unrecognized keys is parsed
- **THEN** valid viewport state round-trips losslessly while unknown or malformed hash parameters fall back safely to canonical defaults without throwing
