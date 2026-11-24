## ADDED Requirements

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
