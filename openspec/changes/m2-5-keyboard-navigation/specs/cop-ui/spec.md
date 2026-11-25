## ADDED Requirements

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

## MODIFIED Requirements

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

### Requirement: Full-stack headless browser COP end-to-end verification and visual legibility
The `ui/e2e/` Playwright harness and COP workspace SHALL verify `ui/build` assets on a live `bbops` loopback runtime against synthetic `0xB1` UDP streams, external contact `ENTITY_STATES` fixtures, and disconnected startups in headless Chromium with zero `pageerror`/console errors, `// spec:` citations, unclipped `Fleet`/`Vehicle`/`Diagnostics`/`Analysis` tabs (`1024×768`, `1440×900`), accessible keyboard/focus controls, non-overlapping `VehicleHud`/range labels, and dark `Sigma` hover pills.

#### Scenario: Live browser COP connects dual WebSockets, renders Canvas2D and WebGL Sigma, and syncs selection across canvas, graph, and URL hash
- **WHEN** headless Chromium opens `bbops` while `Quadcopter`, `Rover`, and crabbing `SurfaceBoat` `0xB1` peers stream UDP telemetry and emit Lane 3 `DirectCrsfOverride` or `STATUSTEXT` transitions
- **THEN** `Telemetry` and `Fleet data` reach live/synced with `0` console/`pageerror` faults, `Fleet`/`Vehicle`/`Diagnostics`/`Analysis` render unclipped with `aria-current="page"`, `TacticalCanvas`/`VehicleHud`/`Sigma` render DPR-scaled frames without white-on-white pills, and selection, `2-Hop` `pathSearch`, and enabled hotkeys (`G`, `D`, `B`, `K`, `M`, `L`, `T`, `C`, `H`, `1`–`9`, `Escape`) sync deterministically

#### Scenario: Browser keyboard navigation preserves Tab order, WAI-ARIA inspector tabs, shortcut disablement, and workspace focus
- **WHEN** headless Chromium traverses controls with `Tab`/`Shift+Tab` from page entry and pointer-focused canvases, uses `ArrowLeft`/`ArrowRight` in the `Fleet` inspector `tablist`, types in search/range inputs, toggles character shortcuts off, and switches workspaces
- **THEN** `Tab`/`Shift+Tab` never toggle `Lights-Out`, `Fleet` inspector tabs expose one `Tab` stop and `tabpanel` links, editable controls and disabled character shortcuts suppress single-key hotkeys while preserving pointer controls and `Escape`, workspace switches leave focus on a mounted element, and `:focus-visible` outlines remain unobstructed

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
The `+page.svelte` shell, `viewport.svelte.ts`, and workspace components SHALL present `Fleet`, `Vehicle`, `Diagnostics`, and `Analysis` in order (`aria-current="page"` on active) while preserving internal stage tokens (`cop`, `pilot`, `bench`, `cortex`), `Vehicle` `map`/`split` layouts, live dual-WebSocket connections, global fleet fault counts, selected safety/external-contact banners, persistent `FleetFilmstrip` (`1`–`9`), and lossless `#...` URL hash sync without `20 Hz` DOM thrash.

#### Scenario: Switching across Fleet, Vehicle, Diagnostics, and Analysis preserves live streams, selection, URL compatibility, and non-modal fleet fault visibility
- **WHEN** `skgStore` holds ownship UXS vehicles (including an unselected `P4 McuFailsafe` or `poisoned` vehicle) and an external contact while the operator switches across `Fleet`, `Vehicle`, `Diagnostics`, `Analysis`, `pilotLayout`, and `lightsOut` via buttons, links, verbs, enabled hotkeys (`G`, `D`, `B`, `K`, `M`, `L`, `1`–`9`), or `#...` URL hashes
- **THEN** controls expose `Fleet`, `Vehicle`, `Diagnostics`, and `Analysis` (`aria-current="page"` on active) while preserving domain `COP`/`Bench` `LinkTier` labels, both WebSockets stay connected without churn, unselected fleet fault counts and `FleetFilmstrip` selection persist, and `ViewportSnapshot` round-trips `#...` URL hashes losslessly

#### Scenario: Vehicle Map-Primary swap and Fleet Solo-Mode rail enforce external-contact isolation and scroll-free focus
- **WHEN** the `Fleet` workspace switches right-rail sections (`summary`, `alerts`, `skg`) or the `Vehicle` workspace toggles `[M]` between `Map-Primary` (chase-capable `TacticalCanvas` with floating corner `VehicleHud` PiP) and `50/50 Split` across both a `VehicleEntityState` with abnormal `HealthFlags` and an `ExternalContactEntityState`
- **THEN** `Fleet` expands strictly one Solo-Mode `role="tabpanel"` at a time and labels cross-workspace jump actions for `Vehicle`, `Diagnostics`, and `Analysis`, `Vehicle` promotes abnormal vehicle health chips (`severity !== "nominal"`) while rendering guarded action slots exclusively for commandable UXS vehicles, and selecting an `ExternalContactEntityState` withholds all `P1`–`P4` `ControlAuthority`, `armed`, and guarded actuation surfaces

#### Scenario: Diagnostics 20 Hz telemetry ring buffer and staged tuning rack operate without per-frame Svelte reactivity thrash
- **WHEN** the `Diagnostics` workspace (`BenchWorkspace.svelte`) receives rapid `20 Hz` `WS #1` `VehicleKinematics` frames across `sysid`s (including out-of-order timestamps) and an engineer adjusts staged tuning dial/slider values before reverting or applying
- **THEN** the bounded `300`-sample per-`sysid` telemetry ring buffer records monotonically newer kinematic samples for `rAF` 2D stripchart rendering without unbounded memory growth or `20 Hz` DOM `$state` invalidation, renders `0xB1` MCU/trunk timing counters (`loop_dt_us`, `max_loop_dt_us`, `rtt_us`, `crc_errors`, `HealthFlags`) and Lane 3 transition events, and tracks staged parameter diffs locally without transmitting unconfirmed packets on slider drag

#### Scenario: Analysis executes lexical, semantic, and neural SKG queries with a non-safety causal evidence subgraph and triple citations
- **WHEN** an analyst queries the `Analysis` workspace (`CortexWorkspace.svelte`) via `POST /graph-gateway/search` (`globalSearch`, `localSearch`, `semantic`, `similar`) and `POST /graph-gateway/graphql` (`pathSearch`) across UXS vehicles, external COP contacts, and multi-hop relationships (including when `embedding_enabled` is `false`)
- **THEN** `Analysis` renders ranked lexical/predicate matches (and surfaces `not_enabled` neural status cleanly without throwing when embeddings are disabled), projects the matched 2-hop `pathSearch` neighborhood into a focused causal evidence subgraph using strictly non-safety palette tokens (`red`, `amber`, `green` reserved for vehicle safety badges), cites grounding `SkgTriple` assertions with `indexedRevision`/`kvRevision` metadata, and exposes zero vehicle actuation affordances
