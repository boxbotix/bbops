## ADDED Requirements

### Requirement: Full-stack headless browser COP end-to-end verification and visual legibility
The `ui/e2e/` Playwright harness and `ui/src/lib/components/` COP workspace SHALL verify compiled `ui/build` assets served by a live `bbops` runtime on ephemeral loopback TCP/UDP ports against synthetic `0xB1` multi-domain MAVLink v2 UDP streams (`Quadcopter`, `Rover`, `SurfaceBoat` with cross-current crab-angle divergence, and Lane 3 transitions) plus external COP contact `ENTITY_STATES` fixtures in headless Chromium with zero uncaught `pageerror` or console error diagnostics, enforcing `// spec:` test-to-spec attribution on E2E declarations and maintaining unobstructed, high-contrast legibility across `VehicleHud` readouts (including signed USV crab-angle degrees when active without `GS/VS/ALT` vs. `R/P/Y` overlap), `Sigma` WebGL highlighted node labels without white-on-white pills or viewport clipping, and `TacticalCanvas` northern range-ring labels beneath `.viewport-toolbar`.

#### Scenario: Live browser COP connects dual WebSockets, renders Canvas2D and WebGL Sigma, and syncs selection across canvas, graph, and URL hash
- **WHEN** headless Chromium opens the `bbops` front door while `Quadcopter`, `Rover`, and crabbing `SurfaceBoat` `0xB1` peers stream UDP telemetry and emit a Lane 3 `DirectCrsfOverride` or `STATUSTEXT` transition
- **THEN** `pageerror` and console error counts remain `0`, `WS1` (`telemetry`) and `WS2` (`SKG · synced`) transition to `connected` with all `3` vehicles, `TacticalCanvas` and `VehicleHud` render non-blank DPR-scaled Canvas2D pixels, `SkgPane` mounts WebGL `Sigma` (`[data-testid="skg-topology-fallback"]` absent) with high-contrast highlighted node labels, selecting a vehicle via canvas hit-testing, `Sigma` node click, or `#sel=` URL hash updates `AuthorityBanner`, `23`-triple `Triple Inspector`, `2-Hop` `POST /graph-gateway/graphql` `pathSearch`, and keyboard hotkeys (`T`, `C`, `H`, `Escape`) deterministically

#### Scenario: VehicleHud readouts, USV crab-angle indicator, and TacticalCanvas range labels remain non-overlapping at inspector column widths
- **WHEN** a `Quadcopter` or crabbing `SurfaceBoat` reports multi-digit altitude, groundspeed, and signed roll/pitch/yaw (`+/-180 deg`) inside the `360px` desktop inspector column alongside `.viewport-toolbar` on `TacticalCanvas`
- **THEN** `GS/VS/ALT`, `R/P/Y`, and active `SurfaceBoat` signed crab-angle readouts render without horizontal text collision or clipping, and northern range-ring labels remain unobstructed below `.viewport-toolbar`

#### Scenario: Selecting an external COP contact in the live browser preserves strict vehicle authority isolation
- **WHEN** `ENTITY_STATES` delivers a non-UXS external contact record and the operator selects it in the live browser workspace
- **THEN** `AuthorityBanner` renders only the unknown-preserving external-contact provenance card, `VehicleHud` withholds vehicle attitude/link-tier controls, and zero UXS `ControlAuthority` or `armed` badges appear in the DOM
