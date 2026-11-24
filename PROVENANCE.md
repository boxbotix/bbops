# Code Provenance & IP Clean-Room Policy (`bbops`)

`bbops` is licensed under the permissive **MIT License** (`LICENSE`). Every human contributor and automated coding agent operating in this repository MUST adhere to these clean-room boundaries.

## 1. Reference Repository Classification

### Tier 1: Permitted Upstream Sources (Green Light)
| Upstream Project | Repository | License | Target `bbops` Scope |
| :--- | :--- | :--- | :--- |
| **SemEngine** | `C360Studio/semengine` | MIT | Embedded NATS/JetStream runtime, SKG graph pipeline (`ENTITY_STATES`, `graph-ingest`, `graph-index`, `graph-query`, `graph-lexical`), ADR-100/110/111/115 HTTP & dual-WebSocket surfaces |
| **SemBoids** | `C360Studio/semboids` | MIT | 3-lane hybrid SKG telemetry split (`ADR-001`), SvelteKit 2 + Svelte 5 Runes stores, imperative `requestAnimationFrame` canvas, shared `pkg/graphview.View` patterns |
| **SemStreams UI** | `C360Studio/semstreams-ui` | MIT | Svelte 5 + `sigma` v3 + `graphology` + `ForceAtlas2` SKG topology canvas (`SigmaCanvas.svelte`, `graphology-adapter.ts`, `sigma-layout.ts`) |
| **ATeam** | `C360Studio/ateam` | MIT | Agentic harness, OpenSpec gates, `test-spec` attribution, Go `-overlay` mutation checks, cross-provider review workflow |
| **BoxBotix Stack** | `boxbotix/bbstack` | Apache-2.0 OR MIT | `bbnav` `0xB1` Edge MAVLink v2 contract, 4-state `CommandMux` authority model, SITL physics harness (`bbsim`) |
| **MAVLink Standard Dialects** | `mavlink/mavlink` | MIT | `minimal.xml` and `common.xml` standard dialect definitions ONLY (local UDP `127.0.0.1:14550` edge boundary) |
| **Permissive Go Framing** | `bluenviron/gomavlib` (or clean-room Go) | MIT | Optional MAVLink v2 packet framing/CRC restricted strictly to `minimal` / `common` dialects (zero vendor dialect imports) |

### Tier 2: Retired Internal / Pre-Reticulum Lineage (Do Not Import)
* **`C360Studio/semlink` & `C360Studio/semops`:** `bbops` is a clean-slate replacement. Do NOT import `semlink` pre-Reticulum meshing packages or migrate legacy `semops` ArduPilot/vendor-coupled code into `bbops`.

### Tier 3: Strictly Prohibited Sources (Zero-Read / Zero-Context Rule)
Reading, searching, fetching, translating, refactoring, or pasting code or dialect schemas from **GPLv3** repositories or ArduPilot vendor dialects into `bbops` is strictly forbidden:
* `ArduPilot/ardupilot` (GPLv3)
* `betaflight/betaflight` (GPLv3)
* `iNavFlight/inav` (GPLv3)
* `cleanflight/cleanflight` (GPLv3)
* `ardupilotmega.xml` (ArduPilot vendor MAVLink dialect — prohibited across all codecs, vocabularies, and UI stores)

## 2. Mandatory Rules for Human & AI Contributors
1. **Workspace & Agent Context Isolation:** NEVER open Tier 3 repositories in the same workspace, fetch their files, or pass snippets from them into an LLM/agent session working on `bbops`.
2. **Pure `common.xml` (`0xB1` Edge Contract) Boundary:** All vehicle telemetry and actuation at the `bbops` edge adapter MUST use standard MIT `minimal.xml` + `common.xml` messages under the `bbstack` `0xB1` contract. Vendor-specific `ardupilotmega.xml` messages and enums are banned.
3. **First-Principles & Open Literature Rule:** Implement domain kinematics, geodesy, and HUD visualizations absent from Tier 1 using standard mathematics and open academic literature (for example, Thor I. Fossen for USV hydrodynamics and crab-angle drift visualization; WGS-84 / NED standard transformations).
4. **File-Level Provenance Headers & Notices:** Any non-trivial file adapted from a Tier 1 repository MUST carry an SPDX header citing the upstream repository, file path, 40-character commit SHA, and original MIT / Apache-2.0 / BSD copyright notice, mirrored in `THIRD_PARTY_NOTICES` (comment-free formats such as JSON record their exact provenance entry in `THIRD_PARTY_NOTICES`).

