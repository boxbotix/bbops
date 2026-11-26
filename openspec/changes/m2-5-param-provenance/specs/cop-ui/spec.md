## ADDED Requirements

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

## MODIFIED Requirements

### Requirement: Full-stack headless browser COP end-to-end verification and visual legibility
The `ui/e2e/` harness and COP workspace SHALL verify `ui/build` assets on a live `bbops` loopback runtime against synthetic `0xB1` UDP streams, external contacts, and disconnected startups in headless Chromium with zero `pageerror`/console faults, `// spec:` citations, unclipped tabs (`1024×768`, `1440×900`), accessible keyboard/focus controls, persistent `Diagnostics`/`Analysis` state, sample-parameter provenance, exact numeric staging, zero outbound commands, and legible HUD/Sigma labels.

#### Scenario: Live browser COP connects dual WebSockets, renders Canvas2D and WebGL Sigma, and syncs selection across canvas, graph, and URL hash
- **WHEN** headless Chromium opens `bbops` while `Quadcopter`, `Rover`, and crabbing `SurfaceBoat` `0xB1` peers stream UDP telemetry and emit Lane 3 `DirectCrsfOverride` or `STATUSTEXT` transitions
- **THEN** `Telemetry` and `Fleet data` reach live/synced with `0` console/`pageerror` faults, `Fleet`/`Vehicle`/`Diagnostics`/`Analysis` render unclipped with `aria-current="page"`, `TacticalCanvas`/`VehicleHud`/`Sigma` render DPR-scaled frames without white-on-white pills, and selection, `2-Hop` `pathSearch`, and enabled hotkeys (`G`, `D`, `B`, `K`, `M`, `L`, `T`, `C`, `H`, `1`–`9`, `Escape`) sync deterministically

#### Scenario: Browser keyboard navigation preserves Tab order, WAI-ARIA inspector tabs, shortcut disablement, and workspace focus
- **WHEN** headless Chromium traverses controls with `Tab`/`Shift+Tab` from page entry and pointer-focused canvases, uses `ArrowLeft`/`ArrowRight` in the `Fleet` inspector `tablist`, types in search/range/numeric inputs, toggles character shortcuts off, and switches workspaces
- **THEN** `Tab`/`Shift+Tab` never toggle `Lights-Out`, `Fleet` inspector tabs expose one `Tab` stop and `tabpanel` links, editable controls and disabled character shortcuts suppress single-key hotkeys while preserving pointer controls and `Escape`, workspace switches leave focus on a mounted element, and `:focus-visible` outlines remain unobstructed

#### Scenario: Browser workspace round trips preserve per-vehicle Diagnostics provenance, exact numeric drafts, and Analysis query/result context
- **WHEN** headless Chromium opens `Diagnostics` on `UXS #1` (checking first-view sample-default provenance and unavailable-apply copy), enters exact numeric and slider values (including invalid/bounds checks) on `UXS #1` and `UXS #2`, runs a non-default `Analysis` query, switches through `Fleet` (including an external contact) and `Vehicle`, returns, and reverts `UXS #1`
- **THEN** `Diagnostics` preserves sample-default provenance, bounds/validation feedback, target-vehicle diff summary, and isolated drafts (`UXS #1` reverted to `0 staged`; `UXS #2` intact) while withholding external contacts, `Analysis` restores query, tier, hits/subgraph, inspection context, and provenance, and zero console/`pageerror` faults or outbound command frames occur

#### Scenario: Disconnected browser startup withholds nominal fleet health claims
- **WHEN** headless Chromium loads the compiled `bbops` UI while WebSocket connections are blocked or pending initial `ENTITY_STATES` sync (or synced with zero UXS vehicles)
- **THEN** the topbar renders waiting/unavailable or `No vehicles detected` status and never renders `FLEET NOMINAL`

#### Scenario: VehicleHud readouts, USV crab-angle indicator, and TacticalCanvas range labels remain non-overlapping at inspector column widths
- **WHEN** a `Quadcopter` or crabbing `SurfaceBoat` reports multi-digit altitude, groundspeed, and signed roll/pitch/yaw (`+/-180 deg`) inside the `360px` desktop inspector column alongside `.viewport-toolbar` on `TacticalCanvas`
- **THEN** `VehicleHud` renders `GS`, `VS`, `ALT`, `R/P/Y`, and active `SurfaceBoat` signed crab-angle readouts in a `4 Hz`-throttled semantic HTML strip beneath the `60 fps` horizon `<canvas>` with zero horizontal `boundingBox()` overlap or clipping, and northern range-ring labels remain unobstructed below `.viewport-toolbar`

#### Scenario: Selecting an external COP contact in the live browser preserves strict vehicle authority isolation
- **WHEN** `ENTITY_STATES` delivers a non-UXS external contact record and the operator selects it in the live browser workspace
- **THEN** `AuthorityBanner` renders only the unknown-preserving external-contact provenance card, the `Vehicle` workspace (`PilotWorkspace.svelte` / `VehicleHud`) withholds vehicle attitude/link-tier controls, and zero UXS `ControlAuthority` or `armed` badges appear in the DOM

### Requirement: Session-scoped Diagnostics parameter draft and Analysis investigation continuity across workspace switches
The `+page.svelte` shell, `WorkspaceShell.svelte`, `tuning-rack.ts`, `BenchWorkspace.svelte`, `ui/src/lib/cortex/`, and `CortexWorkspace.svelte` modules SHALL preserve per-vehicle Diagnostics parameter drafts (`{ entityId, domain, baselineSource, baseline, staged }`, `1..=254` canonical 6-part IDs) and Analysis query, tier, results, provenance, and inspection context across workspace switches with stale-result refusal, unavailable-entity explanations, and zero navigation actuation.

#### Scenario: Diagnostics staged parameter values, baselines, provenance, and per-vehicle reverts survive workspace and vehicle switches without actuation
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
