# host-runtime Specification

## Purpose
Defines the `bbops` standalone Go host binary CLI contract and Svelte 5 COP/GCS shell bootstrap invariants prior to `semengine` runtime composition in `M0.2`.

## Requirements
### Requirement: Host CLI version and flag entrypoint
The `bbops` host binary SHALL parse CLI flags cleanly without panicking, print a deterministic `bbops <version> (<commit>)` line to stdout and return exit code `0` when `-version` is passed, return exit code `0` with no error when invoked with zero arguments in `M0.1` bootstrap mode, and return exit code `2` with diagnostic output on stderr when unknown flags or unexpected positional arguments are supplied.

#### Scenario: Version flag prints release identity and exits zero
- **WHEN** `run` is invoked with `["-version"]`
- **THEN** it writes `bbops <version> (<commit>)\n` to stdout, writes nothing to stderr, and returns exit code `0`

#### Scenario: Unknown CLI flag or positional argument returns exit code two
- **WHEN** `run` is invoked with an unknown flag such as `["-unknown-flag"]` or an unexpected positional argument such as `["extra"]`
- **THEN** it writes diagnostic output to stderr, writes nothing to stdout, and returns exit code `2`

### Requirement: Svelte 5 COP shell bootstrap metadata
The `ui/` SvelteKit 2 + Svelte 5 Runes shell SHALL expose typed multi-domain UXS vehicle descriptors (`Quadcopter`, `Rover`, `SurfaceBoat`) and deterministic banner formatting without runtime dependencies on legacy `semlink` or `semops` packages.

#### Scenario: Supported UXS domains enumerate Quadcopter, Rover, and SurfaceBoat
- **WHEN** the UI bootstrap module enumerates supported UXS domains and formats the shell banner for a release string
- **THEN** it returns the ordered tuple `["Quadcopter", "Rover", "SurfaceBoat"]` and formats `bbops COP/GCS (<version>) — Quadcopter · Rover · SurfaceBoat`
