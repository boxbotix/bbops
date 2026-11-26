## ADDED Requirements

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

## MODIFIED Requirements

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
