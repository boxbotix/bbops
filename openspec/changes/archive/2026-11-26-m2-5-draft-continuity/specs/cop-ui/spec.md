## ADDED Requirements

### Requirement: Session-scoped Diagnostics parameter draft and Analysis investigation continuity across workspace switches
The `+page.svelte` shell, `WorkspaceShell.svelte`, `tuning-rack.ts`, `BenchWorkspace.svelte`, `ui/src/lib/cortex/`, and `CortexWorkspace.svelte` modules SHALL preserve per-vehicle Diagnostics parameter drafts (`{ entityId, domain, baseline, staged }`, `1..=254` canonical 6-part IDs, `OQ1`) and Analysis query, tier, results, provenance, and inspection context (`OQ2`, `OQ3`) across workspace switches with stale-result refusal, unavailable-entity explanations, and zero navigation actuation.

#### Scenario: Diagnostics staged parameter values, baselines, and per-vehicle reverts survive workspace and vehicle switches without actuation
- **WHEN** an operator in `Diagnostics` adjusts `Heading P Gain` (`3.20 -> 3.30`, `1 staged`) on `UXS #1` and a distinct parameter on `UXS #2`, switches through `Fleet` and `Vehicle`, returns to `Diagnostics` across `UXS #1` and `UXS #2`, clicks `Revert Draft` on `UXS #1`, and switches workspaces again
- **THEN** each vehicle's staged value, baseline, and staged count survive in strict isolation, reverting `UXS #1` clears only `UXS #1` (`0 staged`) while preserving `UXS #2`'s draft across subsequent switches, and zero `onApplyTuning` or actuation calls fire during workspace or entity navigation

#### Scenario: Removed, invalid, poisoned, and non-commandable entities never inherit drafts and explain unavailable state
- **WHEN** `UXS #1` holds a staged `Diagnostics` draft and `viewportStore.selectedEntityId` transitions to an `ExternalContactEntityState`, a `generic` SKG node, a removed/unindexed canonical entity ID (`selectedEntity === null`), or `UXS #1` after an `ENTITY_STATES` `poisoned` / `delete` frame or domain change
- **THEN** non-vehicle, invalid, removed, and `poisoned` selections never inherit another entity's draft or expose editable tuning/apply controls, `Diagnostics` renders an explicit explanation for the unavailable or non-commandable entity (`OQ1`), and existing commandability guards remain intact

#### Scenario: Analysis query, tier, results, provenance, and inspection context survive workspace round trips and selection changes
- **WHEN** an operator in `Analysis` selects a retrieval tier, enters query text, views completed ranked hits, 2-hop `pathSearch` causal subgraph, and grounded triple citations scoped to `UXS #1`, switches to `Fleet`, selects `UXS #2` (or edits query text without submitting), and returns to `Analysis`
- **THEN** `Analysis` restores the entered query text, active tier, completed hits, causal subgraph, and inspection context (`OQ3`) while displaying submission provenance (`submittedQuery`, `submittedTier`, `scopeEntityId`) that distinguishes results captured for `UXS #1` from the newly selected `UXS #2`

#### Scenario: Overlapping and navigation-spanning Analysis searches enforce stale-result refusal and defined return status
- **WHEN** an operator starts an `Analysis` search scoped to `UXS #1` and switches workspaces before or while a newer search or explicit cancel occurs across deterministic out-of-order network completions
- **THEN** older completions never overwrite a newer search, attach hits or subgraph edges to the wrong query or vehicle, or mutate `Fleet`/`Vehicle` selection while away, and returning to `Analysis` renders a defined `running`, `completed`, `cancelled`, or `error` state (`OQ2`)

## MODIFIED Requirements

### Requirement: Full-stack headless browser COP end-to-end verification and visual legibility
The `ui/e2e/` Playwright harness and COP workspace SHALL verify `ui/build` assets on a live `bbops` loopback runtime against synthetic `0xB1` UDP streams, external contacts, and disconnected startups in headless Chromium with zero `pageerror`/console faults, `// spec:` citations, unclipped tabs (`1024×768`, `1440×900`), accessible keyboard/focus controls, persistent `Diagnostics`/`Analysis` session state, non-overlapping HUD/range labels, and dark `Sigma` hover pills.

#### Scenario: Live browser COP connects dual WebSockets, renders Canvas2D and WebGL Sigma, and syncs selection across canvas, graph, and URL hash
- **WHEN** headless Chromium opens `bbops` while `Quadcopter`, `Rover`, and crabbing `SurfaceBoat` `0xB1` peers stream UDP telemetry and emit Lane 3 `DirectCrsfOverride` or `STATUSTEXT` transitions
- **THEN** `Telemetry` and `Fleet data` reach live/synced with `0` console/`pageerror` faults, `Fleet`/`Vehicle`/`Diagnostics`/`Analysis` render unclipped with `aria-current="page"`, `TacticalCanvas`/`VehicleHud`/`Sigma` render DPR-scaled frames without white-on-white pills, and selection, `2-Hop` `pathSearch`, and enabled hotkeys (`G`, `D`, `B`, `K`, `M`, `L`, `T`, `C`, `H`, `1`–`9`, `Escape`) sync deterministically

#### Scenario: Browser keyboard navigation preserves Tab order, WAI-ARIA inspector tabs, shortcut disablement, and workspace focus
- **WHEN** headless Chromium traverses controls with `Tab`/`Shift+Tab` from page entry and pointer-focused canvases, uses `ArrowLeft`/`ArrowRight` in the `Fleet` inspector `tablist`, types in search/range inputs, toggles character shortcuts off, and switches workspaces
- **THEN** `Tab`/`Shift+Tab` never toggle `Lights-Out`, `Fleet` inspector tabs expose one `Tab` stop and `tabpanel` links, editable controls and disabled character shortcuts suppress single-key hotkeys while preserving pointer controls and `Escape`, workspace switches leave focus on a mounted element, and `:focus-visible` outlines remain unobstructed

#### Scenario: Browser workspace round trips preserve per-vehicle Diagnostics drafts and Analysis query/result context
- **WHEN** headless Chromium stages `Heading P Gain` (`3.20 -> 3.30`, `1 staged`) on `UXS #1` and a distinct draft on `UXS #2` in `Diagnostics`, runs an `Analysis` query with a non-default tier and inspects citations, switches through `Fleet` and `Vehicle` (changing selected vehicle), returns to `Diagnostics` and `Analysis`, and reverts `UXS #1`
- **THEN** `Diagnostics` restores each vehicle's staged value, baseline, and staged count in isolation and keeps `UXS #1` reverted (`0 staged`) without affecting `UXS #2`, and `Analysis` restores its query text, tier, completed hits/subgraph, inspection context, and earlier-selection provenance without console/`pageerror` faults or outbound actuation

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
