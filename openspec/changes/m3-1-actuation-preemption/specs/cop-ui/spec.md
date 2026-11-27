## ADDED Requirements

### Requirement: Guarded Vehicle ControlPanel, LinkTier supervisory collapse, sub-2ms hardware preemption release, and SITL fault lab
`ControlPanel.svelte` and `PilotWorkspace.svelte` (`OQ3`) SHALL expose actuation only for commandable `VehicleEntityState`s, honor `#39` freshness and `#41` keyboard safety, collapse `10–30 Hz` teleop into `1 Hz` supervisory actions on `mesh-supervisory`, gate the `SITL Fault & Current Lab` to `sitl-local`, zero sticks and latch preemption on `direct-crsf-override`/`mcu-failsafe` until re-engaged, show non-retrying command states, and keep `#43` `Diagnostics` drafts offline.

#### Scenario: MeshSupervisory LinkTier collapses continuous velocity pad into discrete supervisory buttons
- **WHEN** a selected commandable UXS vehicle reports `linkTier === "mesh-supervisory"` (or transitions from `"mesh-high-rate"` to `"mesh-supervisory"` mid-session)
- **THEN** `ControlPanel.svelte` hides/disables the `10–30 Hz` velocity pad (`#84`), ceases any active velocity nudge timer immediately, and presents only discrete `1 Hz` supervisory buttons (`Hold Position`, `Disarm`, and mode selection)

#### Scenario: DirectCrsfOverride or McuFailsafe transition immediately zeroes teleop sticks and requires explicit re-engagement
- **WHEN** an operator is actively streaming velocity teleop nudges in `ControlPanel.svelte` and WebSocket #1 / #2 delivers a `direct-crsf-override` (`P1`) or `mcu-failsafe` (`P0`) authority transition, followed later by authority returning to `companion-teleop` (`P2`)
- **THEN** active UI velocity sticks zero and disengage on the transition frame, any in-flight discrete command badge updates to `PREEMPTED`, the preemption banner highlights ownership loss, and zero `#84` nudges resume after `P2` returns until the operator explicitly activates `Engage Teleop` again

#### Scenario: Indeterminate COMMAND_ACK timeout surfaces commit_unknown status without UI auto-retry while SITL lab stays gated to sitl-local
- **WHEN** a discrete command (`Arm`, `Disarm`, or `Hold Position`) times out without `#77 COMMAND_ACK` (`commit_unknown`), and the operator inspects `ControlPanel.svelte` across `sitl-local` versus non-SITL (`direct-bench`, `mesh-high-rate`, `mesh-supervisory`) vehicles and switches to `Diagnostics`
- **THEN** `ControlPanel.svelte` renders an explicit indeterminate `Status unconfirmed — awaiting vehicle heartbeat` state without re-sending the command, renders the `SITL Fault & Current Lab` drawer only on `sitl-local` vehicles, never fires actuation from global `#41` workspace hotkeys, and keeps `"domain-default"` `Diagnostics` drafts strictly local and non-actuating
