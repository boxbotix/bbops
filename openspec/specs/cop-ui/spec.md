# cop-ui Specification

## Purpose
Defines the `bbops` Svelte 5 Runes Common Operating Picture (COP) client state stores (`telemetry.svelte.ts`, `skg.svelte.ts`, `viewport.svelte.ts`) and ADR-115 single-root-field GraphQL client (`graphql.ts`), covering 20 Hz latest-wins kinematic streaming, bounded Lane 3 transition event buffering, gapless `ENTITY_STATES` `kv-watch` reconciliation with strict ownship UXS vehicle vs. external contact separation, sub-30s WebSocket keepalive and capped exponential reconnect backoff, and deterministic controller verbs with URL hash serialization.
## Requirements
### Requirement: Latest-wins kinematic store and bounded transition event ring buffer
The `ui/src/lib/stores/telemetry.svelte.ts` store SHALL maintain a resilient WebSocket #1 connection (`bbops.telemetry.>`) that overwrites the latest per-`sysid` (`1..=254`) kinematic snapshot (`quat_wxyz`, `body_rates`, `ned_xyz`, `ned_vel`, `lat_lon_alt`, `has_global_pos`, `hdg_deg`, `timestamp`) in place without unbounded array growth or `20 Hz` per-frame DOM `$state` invalidation, ignores out-of-order kinematic frames with older timestamps for the same `sysid`, records discrete `bbops.telemetry.events.<sysid>` `TransitionEvent`s (`heartbeat_edge`, `failsafe_increment`, `statustext`, `link_lost`, `link_restored`) in a bounded ring buffer (default `100` entries) while maintaining per-`sysid` link-loss/event liveness state immune to ring eviction, transmits periodic `< 30 s` (`25 s` default) application keepalive frames while open, and reconnects automatically using `500 ms -> 30 s` exponential backoff.

#### Scenario: Per-sysid kinematic frames overwrite in place and drop older out-of-order timestamps
- **WHEN** WebSocket #1 delivers rapid `20 Hz` `data` frames on `bbops.telemetry.<sysid>` across multiple `sysid`s, including a delayed packet carrying a `timestamp` strictly less than that `sysid`'s current snapshot
- **THEN** the store updates each `sysid`'s latest kinematic snapshot in place without growing history arrays or firing per-frame DOM reactivity, and discards the stale out-of-order packet without regressing `quat_wxyz`, `ned_xyz`, `ned_vel`, or `hdg_deg`

#### Scenario: Discrete Lane 3 transition events append to a bounded ring buffer and maintain per-sysid link-loss state
- **WHEN** WebSocket #1 delivers `link_lost` for `sysid = 1` followed by more than `100` discrete `bbops.telemetry.events.2` frames (`statustext`, `DirectCrsfOverride`, `McuFailsafe`, failsafe increments) and later `link_restored` (or a newer kinematic/event packet) for `sysid = 1`
- **THEN** the store retains the newest `100` transition events in arrival order while preserving `sysid = 1`'s per-`sysid` `linkLost` state across ring eviction until `sysid = 1` receives `link_restored` or newer valid telemetry

#### Scenario: Telemetry WebSocket sends sub-30s keepalive frames and reconnects with exponential backoff
- **WHEN** the telemetry WebSocket remains open without outbound traffic across `25 s` intervals and later closes unexpectedly (`1006`) across consecutive retry failures
- **THEN** the client transmits a keepalive frame every `25 s` (`< 30 s`) so `output/websocket`'s `60 s` `ReadMessage` deadline never expires on idle links, schedules reconnect attempts along `500 ms -> 30 s` capped exponential backoff, and resets backoff to `500 ms` after a clean connection is established

### Requirement: Gapless ENTITY_STATES SKG store, external contact separation, and GraphQL client
The `ui/src/lib/stores/skg.svelte.ts` and `ui/src/lib/graphql.ts` modules SHALL maintain a WebSocket #2 `kv-watch` connection on `ENTITY_STATES` (with `< 30 s` keepalive and `500 ms -> 30 s` exponential reconnect backoff) that reconciles `put` snapshot frames, the `synced` barrier, and live `put`, `delete`, and `poisoned` frames into a reactive `SvelteMap<string, CopEntityState>` keyed by 6-part entity ID, narrowing `<org>.<platform>.robotics.uxs.vehicle.<sysid>` (`1..=254`) entities with valid `uxs.*` triples into `VehicleEntityState` (tracking high-water receipt freshness for both `poseObservedAt` and `observedAt` so replayed snapshots never disguise stale vehicle observations) while keeping non-UXS/external COP contacts distinct with unknown-preserving provenance/staleness metadata (`source`, `observedAt`, `receivedAt`, `expiresAt`, `uncertaintyMeters`, `feedStatus`) excluded from vehicle command selectors, paired with a typed single-root-field `POST /graph-gateway/graphql` client (`entity`, `entitiesByPrefix`, `entitiesByPredicate`, `pathSearch`).

#### Scenario: Initial snapshot and reconnect stage updates until the synced barrier without blanking live state or refreshing stale observations
- **WHEN** WebSocket #2 connects initially or reconnects after a disconnect, delivering a burst of `put` snapshot frames (including an unchanged snapshot for an already-silent UXS vehicle) followed by `{"type":"synced","bucket":"ENTITY_STATES"}` where one previously cached entity was deleted while offline
- **THEN** prior live `CopEntityState` entries remain readable without blanking during reconnect until `synced` arrives, at which point the store atomically swaps in the reconciled snapshot, prunes the vanished entity key, and preserves the silent vehicle's original stale observation age rather than treating snapshot receipt as a fresh vehicle update

#### Scenario: Live put, delete, and poisoned frames reconcile UXS vehicle selectors accurately
- **WHEN** post-`synced` `put`, `delete`, and `poisoned` frames arrive for `<org>.<platform>.robotics.uxs.vehicle.<sysid>` entities carrying `uxs.*` and `geo.location.*` triples
- **THEN** `put` parses strongly typed `domain`, `authority`, `companionMode`, `linkTier`, `armed`, `healthFlags`, `sysStatus`, kinematics, coordinates, and observation high-water metadata into `VehicleEntityState`, `delete` removes the entity from both `entities` and vehicle selectors, and `poisoned` flags the entity's `poisoned` error metadata and withholds it from healthy commandable vehicle selection

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
The `ui/src/lib/viewport/` and `TacticalCanvas.svelte` modules SHALL implement `ViewportAdapter` (`NedGridAdapter`), render `Quadcopter`/`Rover`/`SurfaceBoat` glyphs, external contacts, reticles, wake trails, USV crab-angle split vectors, and reserved safety hues (`red`, `amber`, `green`) on a DPR-aware `rAF` 2D canvas above `.viewport-surface`, and bind `T`, `C`, `H` (when character shortcuts are enabled) and `Escape` to `viewport.svelte.ts` without `20 Hz` DOM thrash or `Tab` capture.

#### Scenario: NedGridAdapter projects local coordinates and preserves world-course angles across camera rotation
- **WHEN** `NedGridAdapter` projects and unprojects world positions across pan, zoom, and camera rotation states (including chase heading rotation and tangent-plane `WGS-84` contact offsets when anchored)
- **THEN** `project` and `unproject` round-trip within floating-point tolerance, `rangeM` reflects horizontal distance from camera center, and `screenCourseRad` maps North-referenced world course radians to the rotated screen-space bearing deterministically

#### Scenario: SurfaceBoat crab-angle split distinguishes bow heading from cross-current velocity over ground
- **WHEN** a `SurfaceBoat` entity reports bow heading (`quat_wxyz` / `hdg_deg`) diverging from horizontal velocity-over-ground (`vx, vy`) above the stationary speed deadband versus below the deadband
- **THEN** the tactical view-model emits distinct bow-heading and course-over-ground vectors with signed crab angle wrapped to `(-pi, pi]` when moving above the deadband, and suppresses phantom velocity-vector divergence when stationary below the deadband

#### Scenario: Tactical canvas hotkeys drive viewport controller verbs while respecting editable targets, modifier chords, and character-shortcut gating
- **WHEN** keydown events fire for `T`, `C`, `H`, and `Escape` on the workspace window (with `characterShortcutsEnabled` `true` vs. `false`) versus inside editable/ARIA-widget elements or with `Ctrl`/`Meta`/`Alt`/`Shift` modifiers held
- **THEN** unmodified `T`, `C`, and `H` invoke `viewport.svelte.ts` tether, chase-lock, and declutter transitions synced to the URL hash strictly when `characterShortcutsEnabled` is `true`, `Escape` clears tether/chase lock even when character shortcuts are off, and editable or modified keys are ignored

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
The `ui/e2e/` harness and COP workspace SHALL verify `ui/build` assets on a live `bbops` loopback runtime against synthetic `0xB1` UDP streams, external contacts, and disconnected startups in headless Chromium with zero `pageerror`/console faults, `// spec:` citations, compact desktop (`1024×768`, `1440×900`) and `200%` zoom layout accessibility, keyboard/focus controls, persistent `Diagnostics`/`Analysis` state, sample-parameter provenance, zero outbound commands, and legible labels.

#### Scenario: Live browser COP connects dual WebSockets, renders Canvas2D and WebGL Sigma, and syncs selection across canvas, graph, and URL hash
- **WHEN** headless Chromium opens `bbops` while `Quadcopter`, `Rover`, and crabbing `SurfaceBoat` `0xB1` peers stream UDP telemetry and emit Lane 3 `DirectCrsfOverride` or `STATUSTEXT` transitions
- **THEN** `Telemetry` and `Fleet data` reach live/synced with `0` console/`pageerror` faults, `Fleet`/`Vehicle`/`Diagnostics`/`Analysis` render unclipped with `aria-current="page"`, `TacticalCanvas`/`VehicleHud`/`Sigma` render DPR-scaled frames without white-on-white pills, and selection, `2-Hop` `pathSearch`, and enabled hotkeys (`G`, `D`, `B`, `K`, `M`, `L`, `T`, `C`, `H`, `1`–`9`, `Escape`) sync deterministically

#### Scenario: Browser keyboard navigation preserves Tab order, WAI-ARIA inspector tabs, shortcut disablement, and workspace focus
- **WHEN** headless Chromium traverses controls with `Tab`/`Shift+Tab` from page entry and pointer-focused canvases, uses `ArrowLeft`/`ArrowRight` in the `Fleet` inspector `tablist`, types in search/range/numeric inputs, toggles character shortcuts off, and switches workspaces
- **THEN** `Tab`/`Shift+Tab` never toggle `Lights-Out`, `Fleet` inspector tabs expose one `Tab` stop and `tabpanel` links, editable controls and disabled character shortcuts suppress single-key hotkeys while preserving pointer controls and `Escape`, workspace switches leave focus on a mounted element, and `:focus-visible` outlines remain unobstructed

#### Scenario: Browser workspace round trips preserve per-vehicle Diagnostics drafts and Analysis query/result context
- **WHEN** headless Chromium opens `Diagnostics` on `UXS #1` (checking first-view sample-default provenance and unavailable-apply copy), enters exact numeric and slider values (including invalid/bounds checks) on `UXS #1` and `UXS #2`, runs a non-default `Analysis` query, switches through `Fleet` (including an external contact) and `Vehicle`, returns, and reverts `UXS #1`
- **THEN** `Diagnostics` preserves sample-default provenance, bounds/validation feedback, target-vehicle diff summary, and isolated drafts (`UXS #1` reverted to `0 staged`; `UXS #2` intact) while withholding external contacts, `Analysis` restores query, tier, hits/subgraph, inspection context, and provenance, and zero console/`pageerror` faults or outbound command frames occur

#### Scenario: Browser compact desktop viewports (1024x768, 1440x900) and 200% zoom preserve visible safety status, reachable fleet switching, and non-modal panel focus
- **WHEN** headless Chromium exercises `bbops` at `1024×768` and `1440×900` (`100%` zoom) and `200%` zoom equivalents (`512×384`, `720×450`) across populated, multi-fault, empty, growing-roster, and long-name fixtures while opening, closing, and resizing `Fleet` panels via keyboard and pointer
- **THEN** baseline selection reveals `UXS #<sysid>`, `ControlAuthority`, `ARMED`/`DISARMED`, and freshness without page scrolling alongside `TacticalCanvas` and a reachable switcher, `invariant-safety-topbar` stays visible without `:focus-visible` occlusion, baseline sizes incur zero horizontal overflow or overlap, `200%` zoom keeps controls unclipped via scroll, and non-modal panel toggles preserve context and return focus

#### Scenario: Disconnected browser startup withholds nominal fleet health claims
- **WHEN** headless Chromium loads the compiled `bbops` UI while WebSocket connections are blocked or pending initial `ENTITY_STATES` sync (or synced with zero UXS vehicles)
- **THEN** the topbar renders waiting/unavailable or `No vehicles detected` status and never renders `FLEET NOMINAL`

#### Scenario: VehicleHud readouts, USV crab-angle indicator, and TacticalCanvas range labels remain non-overlapping at inspector column widths
- **WHEN** a `Quadcopter` or crabbing `SurfaceBoat` reports multi-digit altitude, groundspeed, and signed roll/pitch/yaw (`+/-180 deg`) inside the `360px` desktop inspector column alongside `.viewport-toolbar` on `TacticalCanvas`
- **THEN** `VehicleHud` renders `GS`, `VS`, `ALT`, `R/P/Y`, and active `SurfaceBoat` signed crab-angle readouts in a `4 Hz`-throttled semantic HTML strip beneath the `60 fps` horizon `<canvas>` with zero horizontal `boundingBox()` overlap or clipping, and northern range-ring labels remain unobstructed below `.viewport-toolbar`

#### Scenario: Selecting an external COP contact in the live browser preserves strict vehicle authority isolation
- **WHEN** `ENTITY_STATES` delivers a non-UXS external contact record and the operator selects it in the live browser workspace
- **THEN** `AuthorityBanner` renders only the unknown-preserving external-contact provenance card, the `Vehicle` workspace (`PilotWorkspace.svelte` / `VehicleHud`) withholds vehicle attitude/link-tier controls, and zero UXS `ControlAuthority` or `armed` badges appear in the DOM

### Requirement: Task-modal 4-workspace shell, invariant safety topbar, and Fleet Filmstrip
The `+page.svelte` shell, `viewport.svelte.ts`, session stores, and workspace components SHALL present `Fleet`, `Vehicle`, `Diagnostics`, and `Analysis` in order (`aria-current="page"` on active) while preserving internal stage tokens (`cop`, `pilot`, `bench`, `cortex`), `Vehicle` `map`/`split` layouts, live dual-WebSocket connections, session-scoped drafts/investigation state, global fleet fault counts, safety/contact banners, `FleetFilmstrip` (`1`–`9`), and lossless `#...` URL hash sync.

#### Scenario: Switching across Fleet, Vehicle, Diagnostics, and Analysis preserves live streams, selection, session drafts, URL compatibility, and non-modal fleet fault visibility
- **WHEN** `skgStore` holds ownship UXS vehicles (including an unselected `P4 McuFailsafe` or `poisoned` vehicle) and an external contact while the operator switches across `Fleet`, `Vehicle`, `Diagnostics`, `Analysis`, `pilotLayout`, and `lightsOut` via buttons, links, verbs, enabled hotkeys (`G`, `D`, `B`, `K`, `M`, `L`, `1`–`9`), or `#...` URL hashes
- **THEN** controls expose `Fleet`, `Vehicle`, `Diagnostics`, and `Analysis` (`aria-current="page"` on active) while preserving domain `COP`/`Bench` `LinkTier` labels, both WebSockets stay connected without churn, per-vehicle `Diagnostics` drafts and `Analysis` session context persist across switches, unselected fleet fault counts and `FleetFilmstrip` selection persist, and `ViewportSnapshot` round-trips `#...` URL hashes losslessly

#### Scenario: Vehicle Map-Primary swap and Fleet Solo-Mode rail enforce external-contact isolation and scroll-free focus
- **WHEN** the `Fleet` workspace switches right-rail sections (`summary`, `alerts`, `skg`) or the `Vehicle` workspace toggles `[M]` between `Map-Primary` (chase-capable `TacticalCanvas` with floating corner `VehicleHud` PiP) and `50/50 Split` across both a `VehicleEntityState` with abnormal `HealthFlags` and an `ExternalContactEntityState`
- **THEN** `Fleet` expands strictly one Solo-Mode `role="tabpanel"` at a time and labels cross-workspace jump actions for `Vehicle`, `Diagnostics`, and `Analysis`, `Vehicle` promotes abnormal vehicle health chips (`severity !== "nominal"`) while rendering guarded action slots exclusively for commandable UXS vehicles, and selecting an `ExternalContactEntityState` withholds all `P1`–`P4` `ControlAuthority`, `armed`, and guarded actuation surfaces

#### Scenario: Diagnostics 20 Hz telemetry ring buffer and staged tuning rack operate without per-frame Svelte reactivity thrash
- **WHEN** the `Diagnostics` workspace (`BenchWorkspace.svelte`) receives rapid `20 Hz` `WS #1` `VehicleKinematics` frames across `sysid`s (including out-of-order timestamps) and an engineer adjusts staged tuning dial/slider values across workspace switches before reverting or applying
- **THEN** the bounded `300`-sample per-`sysid` ring buffer records monotonically newer kinematic samples for `rAF` 2D stripchart rendering without unbounded memory growth or `20 Hz` DOM `$state` invalidation, renders `0xB1` MCU/trunk timing counters and Lane 3 events, and preserves per-vehicle staged parameter diffs locally across switches without unconfirmed outbound packets

#### Scenario: Analysis executes lexical, semantic, and neural SKG queries with a non-safety causal evidence subgraph and triple citations
- **WHEN** an analyst queries the `Analysis` workspace (`CortexWorkspace.svelte`) via `POST /graph-gateway/search` (`globalSearch`, `localSearch`, `semantic`, `similar`) and `POST /graph-gateway/graphql` (`pathSearch`) across UXS vehicles, external COP contacts, and multi-hop relationships (including when `embedding_enabled` is `false`)
- **THEN** `Analysis` renders ranked lexical/predicate matches (and surfaces `not_enabled` neural status cleanly without throwing when embeddings are disabled), projects the matched 2-hop `pathSearch` neighborhood into a focused causal evidence subgraph using strictly non-safety palette tokens (`red`, `amber`, `green` reserved for vehicle safety badges), cites grounding `SkgTriple` assertions with `indexedRevision`/`kvRevision` and submission provenance metadata, and exposes zero vehicle actuation affordances

### Requirement: Fleet health readiness, per-vehicle observation freshness, and last-known fault retention
The `ui/src/lib/hud/view-model.ts`, `ui/src/lib/stores/telemetry.svelte.ts`, `ui/src/lib/stores/skg.svelte.ts`, and workspace UI components (`WorkspaceShell.svelte`, `AuthorityBanner.svelte`, `CopWorkspace.svelte`, `PilotWorkspace.svelte`, `VehicleHud.svelte`, `FleetFilmstrip.svelte`) SHALL distinguish stream connection readiness, per-vehicle observation freshness, and observed vehicle safety health without ever equating missing, empty, stale, or disconnected state with confirmed nominal health:
1. **Pre-sync & empty-fleet gating:** Before the first valid `ENTITY_STATES` `synced` snapshot arrives, the fleet summary SHALL display a waiting/connecting state while connecting/reconnecting and an unavailable state when disconnected—never `Nominal`. When `ENTITY_STATES` syncs with zero ownship UXS vehicles, the fleet summary SHALL display `No vehicles detected` rather than a nominal vehicle health claim.
2. **Per-vehicle observation freshness & reconnection discipline:** Vehicle observation freshness SHALL combine per-`sysid` Lane 3 `link_lost`/`link_restored` state on WebSocket #1 with `LinkTier`-scaled `uxs.*` observation timestamps (`observedAt` high-water receipt tracking on WebSocket #2), ensuring neither socket connection/reconnection alone nor replay of unchanged `ENTITY_STATES` snapshots restores live status until fresh vehicle observations arrive.
3. **Partial stream degradation & usable fallbacks:** Operator-facing stream status controls SHALL name the affected information (`Telemetry` and `Fleet data`, with technical identifiers `WS1`/`WS2` confined to secondary details/tooltips). Degrading one stream SHALL accurately reflect its freshness impact without hiding missing required health data or declaring a usable live pose/health fallback unavailable.
4. **Retained last-known faults & explicit arming status:** Disconnected or silent vehicles SHALL remain visible with `LAST KNOWN` / `STALE` status and observation age (or last-update timestamp), retaining all last-known safety faults (`P4 McuFailsafe`, `P1 DirectCrsfOverride`, `HealthFlags` faults, `POISONED`) without clearing them on link loss, withholding live safety-green (`success`) styling on stale `ARMED` vehicles, and displaying explicit `ARMED` / `DISARMED` labels consistently across banner, roster, and filmstrip surfaces.

#### Scenario: Disconnected startup and empty fleet snapshot never claim nominal fleet health
- **WHEN** `WorkspaceShell` mounts before WebSocket #2 completes its first `ENTITY_STATES` `synced` barrier (across `connecting`, `reconnecting`, and `disconnected` states), and subsequently receives `{"type":"synced","bucket":"ENTITY_STATES"}` with zero ownship UXS vehicles (with or without external COP contacts)
- **THEN** pre-sync states render waiting/connecting or fleet-data-unavailable status with zero `FLEET NOMINAL` text or badge, and the synced empty fleet renders `No vehicles detected` without implying healthy vehicles

#### Scenario: Confirmed nominal health requires fresh fleet observations and zero active faults
- **WHEN** WebSocket #2 is `connected` and `synced` with one or more ownship UXS vehicles whose observations are within their `LinkTier` freshness threshold (`4 s` bench/SITL/high-rate; `11 s` `mesh-supervisory`) and carry zero active safety faults, versus when any vehicle reports an active fault or ages past its freshness threshold (or emits `link_lost`)
- **THEN** `FLEET NOMINAL (N)` renders strictly while all `N > 0` ownship vehicles are fresh and fault-free, transitions immediately to fault status on any active fault, and downgrades to stale/last-known status once any vehicle's observation goes silent or `link_lost`

#### Scenario: Partial stream loss preserves usable fallbacks while representing degraded freshness accurately
- **WHEN** WebSocket #1 (`Telemetry`) disconnects while WebSocket #2 (`Fleet data`) stays `connected · synced` with fresh `HEARTBEAT` snapshots, and conversely when WebSocket #2 disconnects after initial sync while WebSocket #1 continues streaming fresh `20 Hz` `VehicleKinematics`
- **THEN** operator-facing pills identify `Telemetry` vs. `Fleet data` degradation explicitly, `WS #1` loss falls back to fresh `WS #2` fleet health and pose without falsely declaring vehicles unavailable, and `WS #2` loss preserves live `WS #1` HUD/canvas pose while downgrading fleet health from `FLEET NOMINAL` to stale/last-known

#### Scenario: Retained faults survive link loss with age metadata and reconnect waits for fresh data
- **WHEN** an armed or faulted (`P4 McuFailsafe` / `CRSF FAULT`) UXS vehicle goes silent (`link_lost` or WebSocket disconnect) across controlled clock advances, followed first by bare WebSocket `onopen` + replay of the unchanged stale `ENTITY_STATES` snapshot and only later by a newly observed vehicle packet (`link_restored` / advancing `observedAt`)
- **THEN** active faults remain visible throughout silence and disconnect with `LAST KNOWN` qualification and advancing observation age, stale fault-free `ARMED` vehicles drop live safety-green styling while keeping explicit `ARMED` / `DISARMED` labels, bare socket open or unchanged snapshot replay keeps the vehicle stale, and live status resumes only after fresh vehicle data arrives

### Requirement: Conventional keyboard traversal, WAI-ARIA tablist navigation, focus management, and character-key shortcut gating
The `ui/src/lib/` stores, hotkey controller, styles, and workspace components SHALL preserve native `Tab`/`Shift+Tab` focus order (never toggling `Lights-Out`), expose a labeled `Lights-Out` button and non-conflicting shortcut (`OQ1`), implement a single-stop WAI-ARIA `tablist` with `tabpanel` links and focus recovery in `Fleet` (`OQ3`), allow disabling global character-only shortcuts with synced hints (`OQ2`), and maintain `aria-current="page"` and unobstructed `:focus-visible` outlines.

#### Scenario: Tab and Shift+Tab traverse controls in logical order without toggling Lights-Out
- **WHEN** an operator presses `Tab` and `Shift+Tab` from page entry, across toolbar controls, and immediately after pointer focus on `TacticalCanvas`, `BenchWorkspace` oscilloscope, or `CortexWorkspace` Sigma host, and then triggers `Lights-Out` via its button or dedicated shortcut (`L`)
- **THEN** `Tab` and `Shift+Tab` move focus forward and backward through mounted controls without mutating `viewportStore.lightsOut`, while the `Lights-Out` button and dedicated shortcut toggle `lightsOut` deterministically with synchronized shortcut labels

#### Scenario: Fleet inspector tablist enforces single tab stop, arrow navigation, panel associations, and focus recovery
- **WHEN** keyboard focus enters the `Fleet` inspector `role="tablist"`, presses `ArrowRight`, `ArrowLeft`, `Home`, `End`, and `Tab`, and a focused control inside the active `role="tabpanel"` later unmounts on entity or fault changes
- **THEN** strictly the active `role="tab"` has `tabindex="0"` (`-1` on inactive tabs), arrow/home/end keys cycle tabs with matching `aria-selected` and `aria-controls`/`aria-labelledby` panel links, `Tab` advances directly into panel content, and unmounting a focused panel child restores focus to the active tab

#### Scenario: Global character-key shortcuts can be disabled and never fire inside editable controls
- **WHEN** keys fire inside `<input>`, `<textarea>`, `<select>`, `[contenteditable]`, or ARIA editable/slider widgets, or the operator disables character-only shortcuts via the topbar toggle and presses character keys (`G`, `D`, `B`, `K`, `M`, `L`, `1`–`9`, `T`, `C`, `H`) versus `Escape` or pointer buttons
- **THEN** editable and widget targets never trigger global hotkeys, disabling character shortcuts suppresses all single-character hotkeys and hides inactive `<kbd>` / `[X]` hints from visible and accessible labels while keeping pointer controls and `Escape` untether active, and re-enabling restores them

#### Scenario: Workspace switches preserve predictable focus, programmatic active state, and unobstructed focus visibility
- **WHEN** the operator switches across `Fleet`, `Vehicle`, `Diagnostics`, and `Analysis` via topbar navigation buttons, cross-workspace jump buttons, or enabled hotkeys (`G`, `D`, `B`, `K`) and traverses controls with `Tab`
- **THEN** strictly the active workspace button exposes `aria-current="page"`, focus lands on a predictable mounted element rather than `document.body` when outgoing controls unmount, and focused elements display a high-contrast `:focus-visible` ring unobstructed by headers, toolbars, or HUD overlays

### Requirement: Session-scoped Diagnostics parameter draft and Analysis investigation continuity across workspace switches
The `+page.svelte` shell, `WorkspaceShell.svelte`, `tuning-rack.ts`, `BenchWorkspace.svelte`, `ui/src/lib/cortex/`, and `CortexWorkspace.svelte` modules SHALL preserve per-vehicle Diagnostics parameter drafts (`{ entityId, domain, baselineSource, baseline, staged }`, `1..=254` canonical 6-part IDs) and Analysis query, tier, results, provenance, and inspection context across workspace switches with stale-result refusal, unavailable-entity explanations, and zero navigation actuation.

#### Scenario: Diagnostics staged parameter values, baselines, and per-vehicle reverts survive workspace and vehicle switches without actuation
- **WHEN** an operator in `Diagnostics` adjusts `Heading P Gain` (`3.20 -> 3.30`, `1 staged`) on `UXS #1` and a distinct parameter on `UXS #2`, switches through `Fleet` and `Vehicle`, returns to `Diagnostics` across `UXS #1` and `UXS #2`, clicks `Revert Draft` on `UXS #1`, and switches workspaces again
- **THEN** each vehicle's staged value, baseline, `baselineSource`, and staged count survive in strict isolation, reverting `UXS #1` clears only `UXS #1` (`0 staged`) while preserving `UXS #2`'s draft across subsequent switches, and zero parameter-write or actuation calls fire during workspace or entity navigation

#### Scenario: Removed, invalid, poisoned, and non-commandable entities never inherit drafts and explain unavailable state
- **WHEN** `UXS #1` holds a staged `Diagnostics` draft and `viewportStore.selectedEntityId` transitions to an `ExternalContactEntityState`, a `generic` SKG node, a removed/unindexed canonical entity ID (`selectedEntity === null`), or `UXS #1` after an `ENTITY_STATES` `poisoned` / `delete` frame or domain change
- **THEN** non-vehicle, invalid, removed, and `poisoned` selections never inherit another entity's draft or expose editable tuning/apply controls, `Diagnostics` renders an explicit explanation for the unavailable or non-commandable entity, and existing commandability guards remain intact

#### Scenario: Analysis query, tier, results, provenance, and inspection context survive workspace round trips and selection changes
- **WHEN** an operator in `Analysis` selects a retrieval tier, enters query text, views completed ranked hits, 2-hop `pathSearch` causal subgraph, and grounded triple citations scoped to `UXS #1`, switches to `Fleet`, selects `UXS #2` (or edits query text without submitting), and returns to `Analysis`
- **THEN** `Analysis` restores the entered query text, active tier, completed hits, causal subgraph, and inspection context while displaying submission provenance (`submittedQuery`, `submittedTier`, `scopeEntityId`) that distinguishes results captured for `UXS #1` from the newly selected `UXS #2`

#### Scenario: Overlapping and navigation-spanning Analysis searches enforce stale-result refusal and defined return status
- **WHEN** an operator starts an `Analysis` search scoped to `UXS #1` and switches workspaces before or while a newer search or explicit cancel occurs across deterministic out-of-order network completions
- **THEN** older completions never overwrite a newer search, attach hits or subgraph edges to the wrong query or vehicle, or mutate `Fleet`/`Vehicle` selection while away, and returning to `Analysis` renders a defined `running`, `completed`, `cancelled`, or `error` state

### Requirement: Diagnostics sample baseline provenance, exact numeric staging, and non-actuating offline readiness
`tuning-rack.ts` and `BenchWorkspace.svelte` (plus `WorkspaceShell.svelte` per `OQ2`) SHALL distinguish `"domain-default"` baselines from `"vehicle-reported"` fixtures, state in plain language that edits stage a local draft only, expose per-parameter baseline, draft, units, and bounds plus a target-vehicle diff summary, pair exact numeric inputs with sliders (`OQ1`), explain unavailable apply nearby without `"Applied"`/`"Confirmed"` claims, and emit zero outbound commands (`OQ2`).

#### Scenario: First-view Diagnostics rack identifies sample domain defaults and plain-language local-only staging before editing
- **WHEN** an operator selects a commandable `VehicleEntityState` (`UXS #<sysid>`) and opens `Diagnostics` at `0 staged`, versus when a test fixture seeds `baselineSource: "vehicle-reported"`
- **THEN** the rack identifies `"domain-default"` values as sample domain defaults rather than vehicle-reported parameters (distinct from `"vehicle-reported"` fixtures), states in plain language without ADR/milestone/UDP jargon that editing creates a local draft only and cannot change the vehicle, and shows each parameter's baseline, value, units, and `[min, max]`/`step` bounds

#### Scenario: Exact numeric entry and range sliders enforce shared bounds, increments, and validation feedback without hotkey capture
- **WHEN** an operator enters exact numeric values (in-range step-aligned, out-of-bounds, off-step, and non-numeric/empty) alongside dial/slider `<input type="range">` controls while character shortcuts are enabled
- **THEN** valid numeric entries and slider drags stage identical step-aligned values and deltas without firing single-character hotkeys, invalid/out-of-bounds/off-step entries follow the approved `OQ1` validation contract with visible bounds/step feedback, and `Revert Draft` clears both staged diffs and transient input state for that vehicle

#### Scenario: Edited parameters surface a target-vehicle baseline-to-draft summary, nearby unavailable-apply explanation, and zero outbound commands
- **WHEN** an operator stages edits on `UXS #1`, inspects the change summary and apply footer, switches across `UXS #2`, an external contact, and a `poisoned`/removed entity, and interacts with every tuning control
- **THEN** a compact summary names `UXS #1` (`domain`) and each edited parameter's baseline, draft, delta, and units, nearby text explains that vehicle application is unavailable with zero `"Applied"`/`"Confirmed"` labels, switching preserves per-vehicle `baselineSource` and isolated drafts while withholding non-commandable entities, and zero outbound commands fire (`OQ2`)

### Requirement: Compact desktop viewport layout, persistent safety header, and non-modal Fleet panel controls
`WorkspaceShell.svelte`, `CopWorkspace.svelte`, `FleetFilmstrip.svelte`, `AuthorityBanner.svelte`, `view-model.ts`, and layout styles SHALL reveal selected-vehicle identity, `ControlAuthority`, `ARMED`/`DISARMED`, and freshness without page scroll at `1024×768` and `1440×900` (`OQ1`), keep `invariant-safety-topbar` visible without focus occlusion (`OQ2`), remain reachable at `200%` zoom without clipping or horizontal overflow, and preserve context and focus across non-modal panels (`OQ3`).

#### Scenario: Selecting a vehicle at 1024x768 and 1440x900 reveals identity, authority, arming, and freshness without page scroll
- **WHEN** `WorkspaceShell` renders in `Fleet` at `1024×768` and `1440×900` (`100%` zoom) with a populated fleet and the operator selects a UXS vehicle via `TacticalCanvas`, `FleetFilmstrip` (`1`–`9` or click), or `Roster`
- **THEN** `TacticalCanvas`, `invariant-safety-topbar`, a reachable fleet switcher (`OQ1` deduplicating permanent `Roster` + `FleetFilmstrip` space at compact widths), and the selected vehicle's `UXS #<sysid>`, `ControlAuthority`, `ARMED`/`DISARMED`, and `LIVE`/`LAST KNOWN` freshness appear in-viewport (`window.scrollY === 0`) without document scrolling

#### Scenario: Global safety status stays visible across long content scrolling without obscuring focused controls
- **WHEN** `Fleet` (or another workspace) contains a growing vehicle/contact roster, multiple active fault chips, or long SKG triple lists and the operator scrolls panel/page content and traverses controls with `Tab` / `Shift+Tab`
- **THEN** `invariant-safety-topbar` remains visible in the viewport (`OQ2`) and focused controls maintain unobstructed `:focus-visible` visibility clear of sticky chrome

#### Scenario: Non-modal panel open, close, and resize preserve selection and workspace context with keyboard focus return
- **WHEN** an operator opens, closes, or resizes `Fleet` `Roster` / `Inspector` panels via keyboard (`Enter`/`Space` and `OQ3` `Escape` rules) or pointer across `1024×768` <-> `1440×900` while holding a selected entity, staged `Diagnostics` draft, and `Analysis` session
- **THEN** panels expose non-modal `aria-expanded` and `aria-controls` state (never `aria-modal` unless genuinely modal), closing a panel containing focus returns focus to its toggle control (never `document.body`), a clear control returns focus/view to `TacticalCanvas`, and `selectedEntityId` plus `#40` session state remain intact

#### Scenario: Long names, multiple active faults, empty data, growing rosters, and 200% zoom avoid horizontal overflow and clipped controls
- **WHEN** `WorkspaceShell` renders empty-fleet, multi-fault, growing-roster, and overlong vehicle/contact name fixtures across `1024×768`, `1440×900`, and `200%` zoom (`512×384` and `720×450` effective CSS px)
- **THEN** baseline desktop sizes exhibit zero horizontal document overflow (`documentElement.scrollWidth <= innerWidth`) or control overlap, and `200%` zoom allows vertical scrolling to reach topbar navigation, safety status, workspace stage controls, inspector details, and fleet selection without clipping interactive controls

