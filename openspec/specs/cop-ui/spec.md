# cop-ui Specification

## Purpose
Defines the `bbops` Svelte 5 Runes Common Operating Picture (COP) client state stores (`telemetry.svelte.ts`, `skg.svelte.ts`, `viewport.svelte.ts`) and ADR-115 single-root-field GraphQL client (`graphql.ts`), covering 20 Hz latest-wins kinematic streaming, bounded Lane 3 transition event buffering, gapless `ENTITY_STATES` `kv-watch` reconciliation with strict ownship UXS vehicle vs. external contact separation, sub-30s WebSocket keepalive and capped exponential reconnect backoff, and deterministic controller verbs with URL hash serialization.
## Requirements
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

### Requirement: Renderer-agnostic ViewportAdapter, surface-vs-safety-HUD layer split, and multi-domain tactical canvas
The `ui/src/lib/viewport/` and `TacticalCanvas.svelte` modules SHALL define a renderer-agnostic `ViewportAdapter` contract (`project`, `unproject`, `screenCourseRad`, `flyTo`, `setCamera`) backed in `M2.2` by a local `NedGridAdapter`, render multi-domain UXS tactical glyphs (`Quadcopter`, `Rover`, `SurfaceBoat`), non-vehicle external contact markers, selection/tether reticles, bounded breadcrumb wake trails, USV crab-angle split vectors (true bow heading from `quat_wxyz` / `hdg_deg` alongside velocity-over-ground course from `vx, vy`), and reserved safety hues (`red`, `amber`, `green`) on an upper DPR-aware `requestAnimationFrame` 2D overlay canvas above the `.viewport-surface` layer, and bind keyboard shortcuts (`T` tether, `C` chase lock, `H` declutter, `Escape` untether) and URL view-state hash synchronization to `viewport.svelte.ts` without triggering `20 Hz` per-frame Svelte DOM re-renders.

#### Scenario: NedGridAdapter projects local coordinates and preserves world-course angles across camera rotation
- **WHEN** `NedGridAdapter` projects and unprojects world positions across pan, zoom, and camera rotation states (including chase heading rotation and tangent-plane `WGS-84` contact offsets when anchored)
- **THEN** `project` and `unproject` round-trip within floating-point tolerance, `rangeM` reflects horizontal distance from camera center, and `screenCourseRad` maps North-referenced world course radians to the rotated screen-space bearing deterministically

#### Scenario: SurfaceBoat crab-angle split distinguishes bow heading from cross-current velocity over ground
- **WHEN** a `SurfaceBoat` entity reports bow heading (`quat_wxyz` / `hdg_deg`) diverging from horizontal velocity-over-ground (`vx, vy`) above the stationary speed deadband versus below the deadband
- **THEN** the tactical view-model emits distinct bow-heading and course-over-ground vectors with signed crab angle wrapped to `(-pi, pi]` when moving above the deadband, and suppresses phantom velocity-vector divergence when stationary below the deadband

#### Scenario: Tactical canvas hotkeys drive viewport controller verbs while ignoring editable inputs and modifier chords
- **WHEN** keydown events fire for `T`, `C`, `H`, and `Escape` on the workspace window versus inside `<input>`/`<textarea>`/`<select>`/`[contenteditable]` elements or with `Ctrl`/`Meta`/`Alt` modifiers held
- **THEN** unmodified workspace hotkeys invoke deterministic `viewport.svelte.ts` tether, chase-lock, declutter-cycle, and untether transitions synchronized to the URL hash, while editable-target and modifier-chord keystrokes are ignored without mutating viewport state

### Requirement: Primary attitude HUD, 4-state ControlAuthority banner, and external contact UI isolation
The `ui/src/lib/hud/`, `VehicleHud.svelte`, and `AuthorityBanner.svelte` modules SHALL extract ZYX (`3-2-1`, NED-to-FRD) roll, pitch, and yaw from `quat_wxyz` for the artificial horizon and heading tape alongside groundspeed, vertical speed, and active `LinkTier` badge (`SITL`, `Bench`, `HaLow 30Hz`, `LoRa 1Hz`), render high-contrast 4-state `ControlAuthority` (`P1 DirectCrsfOverride`, `P2 CompanionTeleop`, `P3 CompanionAutonomous`, `P4 McuFailsafe`), `armed` status, and MCU/trunk health strip (`loop_dt_us`, `max_loop_dt_us`, `rtt_us`, `failsafes`, `crc_errors`, `clock_sync_locked`, `teleop_deadman_clamped`, `crsf_link_healthy`, `companion_trunk_healthy`) using reserved safety colors exclusively for controllable UXS vehicles, and present an isolated external-contact view (`source`, freshness timestamps, `uncertaintyMeters`, `feedStatus`) with zero vehicle authority or control affordances whenever a non-UXS external contact is selected.

#### Scenario: Quaternion attitude extraction and authority/health badges map 0xB1 vehicle state deterministically
- **WHEN** the HUD view-model evaluates unit quaternions (including level, banked/pitched, and near-`+/-90 deg` pitch gimbal-lock inputs) and maps all 4 `ControlAuthority` states, 4 `LinkTier` tiers, `armed` states, and `HealthFlags` / `sysStatus` counters
- **THEN** roll, pitch, and `[0, 360)` yaw match the ZYX NED-to-FRD reference equations without `NaN` at pitch singularities, and `P1`–`P4` authority, `LinkTier` (`SITL`, `Bench`, `HaLow 30Hz`, `LoRa 1Hz`), arming, deadman-clamp, and failsafe indicators map exclusively to their reserved safety severities

#### Scenario: Selecting an external COP contact displays unknown-preserving provenance without vehicle authority affordances
- **WHEN** the operator selects an `ExternalContactEntityState` (with partial or unknown `source`, `observedAt`, `receivedAt`, `expiresAt`, `uncertaintyMeters`, and `feedStatus`) instead of a `VehicleEntityState`
- **THEN** the workspace renders the external-contact provenance/freshness summary preserving unknown fields as `"unknown"`/`null` and omits all UXS `ControlAuthority`, `armed`, and vehicle actuation indicators

### Requirement: Live SKG topology graph, triple inspector, and transition event timeline
The `ui/src/lib/skg/` and `SkgPane.svelte` modules SHALL project live `skgStore.entities` and on-demand `POST /graph-gateway/graphql` `pathSearch` neighborhoods into an interactive topology graph whose node/edge styling preserves reserved safety colors (`red`, `amber`, `green` never used for decorative community coloring), display a selected-entity `uxs.*` / `cop.*` triple inspector table, and render a bounded scrolling timeline of Lane 3 `TransitionEvent`s (`STATUSTEXT`, authority edges, failsafe increments, link loss/recovery).

#### Scenario: SKG topology model projects entities and pathSearch edges without consuming reserved safety colors
- **WHEN** `skgStore.entities` contains UXS vehicles, external COP contacts, and generic SKG entities alongside `pathSearch` edge lists
- **THEN** the topology builder produces deterministic deduplicated node and directed-edge records using non-safety palette tokens and selects the corresponding COP entity when a graph node is activated

#### Scenario: Triple inspector and transition timeline reflect selected entity triples and Lane 3 alerts
- **WHEN** an entity is selected in `viewportStore` and `telemetryStore.events` receives `STATUSTEXT`, `DirectCrsfOverride` / `McuFailsafe` authority edges, and failsafe transitions
- **THEN** the inspector lists the selected entity's `SkgTriple` assertions sorted deterministically with source/timestamp metadata and formats each transition event with severity, `sysid`, and reason summary

### Requirement: Full-stack headless browser COP end-to-end verification and visual legibility
The `ui/e2e/` Playwright harness and `ui/src/lib/components/` COP workspace SHALL verify compiled `ui/build` assets served by a live `bbops` runtime on ephemeral loopback TCP/UDP ports against synthetic `0xB1` multi-domain MAVLink v2 UDP streams (`Quadcopter`, `Rover`, `SurfaceBoat` with cross-current crab-angle divergence, and Lane 3 transitions) plus external COP contact `ENTITY_STATES` fixtures in headless Chromium with zero uncaught `pageerror` or console error diagnostics, enforcing `// spec:` test-to-spec attribution on E2E declarations and maintaining unobstructed, high-contrast legibility across `VehicleHud` readouts (including signed USV crab-angle degrees when active without `GS/VS/ALT` vs. `R/P/Y` overlap), `Sigma` WebGL highlighted node labels without white-on-white pills or viewport clipping, and `TacticalCanvas` northern range-ring labels beneath `.viewport-toolbar`.

#### Scenario: Live browser COP connects dual WebSockets, renders Canvas2D and WebGL Sigma, and syncs selection across canvas, graph, and URL hash
- **WHEN** headless Chromium opens the `bbops` front door while `Quadcopter`, `Rover`, and crabbing `SurfaceBoat` `0xB1` peers stream UDP telemetry and emit a Lane 3 `DirectCrsfOverride` or `STATUSTEXT` transition
- **THEN** `pageerror` and console error counts remain `0`, `WS1` (`telemetry`) and `WS2` (`SKG · synced`) transition to `connected` with all `3` vehicles, `TacticalCanvas` and `VehicleHud` render non-blank DPR-scaled Canvas2D pixels, `SkgPane` mounts WebGL `Sigma` (`[data-testid="skg-topology-fallback"]` absent) with high-contrast highlighted node labels, selecting a vehicle via canvas hit-testing, `Sigma` node click, or `#sel=` URL hash updates `AuthorityBanner`, `23`-triple `Triple Inspector`, `2-Hop` `POST /graph-gateway/graphql` `pathSearch`, and keyboard hotkeys (`T`, `C`, `H`, `Escape`) deterministically

#### Scenario: VehicleHud readouts, USV crab-angle indicator, and TacticalCanvas range labels remain non-overlapping at inspector column widths
- **WHEN** a `Quadcopter` or crabbing `SurfaceBoat` reports multi-digit altitude, groundspeed, and signed roll/pitch/yaw (`+/-180 deg`) inside the `360px` desktop inspector column alongside `.viewport-toolbar` on `TacticalCanvas`
- **THEN** `VehicleHud` renders `GS`, `VS`, `ALT`, `R/P/Y`, and active `SurfaceBoat` signed crab-angle readouts in a `4 Hz`-throttled semantic HTML strip beneath the `60 fps` horizon `<canvas>` with zero horizontal `boundingBox()` overlap or clipping, and northern range-ring labels remain unobstructed below `.viewport-toolbar`

#### Scenario: Selecting an external COP contact in the live browser preserves strict vehicle authority isolation
- **WHEN** `ENTITY_STATES` delivers a non-UXS external contact record and the operator selects it in the live browser workspace
- **THEN** `AuthorityBanner` renders only the unknown-preserving external-contact provenance card, `VehicleHud` withholds vehicle attitude/link-tier controls, and zero UXS `ControlAuthority` or `armed` badges appear in the DOM
