# Repository Agent Instructions (`bbops`)

`bbops` is the clean-slate **BoxBotix Multi-Domain Ground Control Station (GCS) & Common Operating Picture (COP)** (`MIT`), running as one standalone Go host binary (`module github.com/boxbotix/bbops`, embedding `github.com/c360studio/semengine v0.1.0-alpha.7` in `M0.2+`) paired with a **SvelteKit 2 + Svelte 5 Runes + TypeScript** UI (`ui/`).

Guidance for coding agents. Codex and Gemini load this file as `AGENTS.md`; `CLAUDE.md` imports it (`@AGENTS.md`), keeping a single copy strictly under 24,000 bytes.

## Workspace Boundaries

- `cmd/bbops/`: Standalone Go host binary entrypoint, CLI flags, and runtime composition (`M0.2+` embeds NATS/JetStream + `semengine` SKG + ADR-115 GraphQL & dual-WebSocket surfaces).
- `internal/`: Go domain packages (MAVLink v2 `0xB1` `common.xml` edge adapter, `uxs.*` vocabulary pack, 3-lane SKG ingest, ADR-111 non-retrying actuation).
- `ui/`: SvelteKit 2 + Svelte 5 Runes + TypeScript + Vitest frontend (`Quadcopter`, `Rover`, `SurfaceBoat` COP canvas, 20 Hz kinematic store, 4 Hz `ENTITY_STATES` SKG store).
- `openspec/`: Canonical `spec-driven` capability specifications (`openspec/specs/`) and in-flight changes (`openspec/changes/`).

## Mandatory Clean-Room Policy

`PROVENANCE.md` and `.agents/rules/ip-cleanroom.md` are strictly authoritative: zero reads, searches, imports, or translations from GPLv3 repositories (`ArduPilot`, `betaflight`, `iNavFlight`, `cleanflight`) or `ardupilotmega.xml`, and zero imports from retired `semlink` or legacy `semops`.

@[PROVENANCE](PROVENANCE.md)

## Ponytail decision ladder

You are a lazy senior developer. The best code is the code never written. Solve the whole problem with the least new code, and end each task conclusion and each handoff with 1–2 lines stating **what you skipped or did not check** and **any residual risk**. Acknowledgements and progress updates need no footer, but they still name a blocker, a failed or unavailable check, or a changed risk when one arises.

1. **Does it need to exist?** Skip unrequested features, options, and abstractions.
2. **Already in this codebase?** Reuse existing helpers, components, and conventions.
3. **Standard library or platform feature?** Prefer the project language’s standard library and native platform features.
4. **Installed dependency?** Use what is already in `go.mod` / `package.json`; never add a dependency for a few lines.
5. **Readable at a glance?** Prefer flat, obvious code over clever one-liners.
6. **Minimum complete change:** Finish every caller, test, and fixture your change touches. Mark any intentional deferral with `// shortcut: <the limit>, <when to upgrade>`.

## Rules and what enforces them

`.agents/rules-index.md` mirrors this table. Workflow protocol (Clarify, Claim, Merge, Close): `.agents/protocol.md`.

### 1. Ponytail decision ladder, clean-room provenance, and code discipline

| Gate | Rule | Where |
| --- | --- | --- |
| `review` | `PROVENANCE.md` and `.agents/rules/ip-cleanroom.md` are authoritative (`MIT`, Zero-GPLv3, `common.xml`-only MAVLink v2, zero `semlink`/`semops` imports); the agent harness adds nothing to them and overrides nothing in them | `PROVENANCE.md`; `.agents/rules/ip-cleanroom.md` |
| `verify+review` | Follow the 6-step Ponytail decision ladder; no unrequested abstractions, wrappers, or dependencies | `.agents/skills/ponytail/SKILL.md` |
| `verify+review` | Every intentional deferral uses `// shortcut: <the limit>, <when to upgrade>`, scanned by `ateam state debt` | `.agents/skills/ponytail/SKILL.md` |
| `verify+review` | Non-trivial logic ships with a test using an independent oracle and proven able to fail via non-destructive mutation check (`task mutate` / `ateam mutate` or `cp` + SHA-256; never `git checkout`/`restore`/`stash`/`reset`), reporting `detection`, `survivor`, `invalid`, or `inconclusive` | `.agents/protocol.md` |
| `verify+review` | Changes with interacting inputs, stated laws (round-trip, idempotence), or order-dependent histories record whether generated checks (Go `testing.F` or PBT) are used or why named examples suffice, prove boundary reach and assertion activation, and check histories against a reference model | `.agents/protocol.md` |
| `verify+review` | Every Go `Test`/`Fuzz`/executable `Example` and Vitest `it`/`test` declaration carries attached spec references (`// spec:` or `//aip:verifies`) resolved by `task test:spec`; syntactic attribution does not prove semantic coverage | `ateam test-spec`; `.agents/protocol.md` |
| `verify` | Source passes formatting, vet, typecheck, and linting with zero warnings (`gofmt`, `go vet`, `golangci-lint`, `svelte-check`, `eslint`) | `Taskfile.yml` |
| `review` | Task conclusions and handoffs end with 1–2 lines stating what was skipped/not checked and any residual risk; acknowledgements and progress updates need no footer but still name blockers, failed or unavailable checks, and changed risks | `.agents/skills/ponytail/SKILL.md` |

### 2. Hooks, MCP budget, and multi-agent parity

| Gate | Rule | Where |
| --- | --- | --- |
| `verify` | `AGENTS.md` stays under 24,000 bytes, `CLAUDE.md` only imports `@AGENTS.md`, and rule row counts match `.agents/rules-index.md` | `.agents/README.md` |
| `verify` | Canonical skills in `.agents/skills/*/SKILL.md` have matching thin adapters in `.claude/skills/` and `.gemini/commands/` | `.agents/README.md` |
| `verify` | Hook configs in `.claude/settings.json`, `.codex/hooks.json`, `.gemini/settings.json`, and `.agents/hooks.json` map native events to `ateam hook`; with `ATEAM_HOOK_SCOPE=user`, `ateam doctor` also accepts one complete local or user-scope file (`.claude/settings.local.json`, `~/.claude/settings.json`, `~/.codex/hooks.json`, `~/.gemini/settings.json`) and names the file under `hook_sources` | `.agents/README.md` |
| `verify` | `ateam mcp` exposes only the 2 multiplexed tools (`ateam_state`, `ateam_task`) with `<300` total schema tokens | `.agents/README.md` |
| `verify` | `ateam hook` executes normalized lifecycle events in `<10ms` target budget (`doctor` warns `>10ms`, fails `--check` `>100ms`) with zero external runtime dependencies | `.agents/README.md` |

### 3. State, OpenSpec, Forge (GitHub/Gitea), and Definition of Done

| Gate | Rule | Where |
| --- | --- | --- |
| `review` | Clarify Deliverable, Scope/non-goals, Definition of Done, and Stop boundary before mutating state | `.agents/protocol.md` |
| `verify+review` | Shared state syncs with GitHub (`gh`/API) or Gitea (`tea`/API v1) and git worktrees, cached in gitignored `.ateam/state.json` | `.agents/protocol.md` |
| `verify` | OpenSpec changes/specs validate via `spec:check`, and `Hold:` / caveats (`HALT`, `RED`, `BLOCKED`, `WONTDO`, `OPEN-Q`) are parsed by `spec:queue` | `openspec/` |
| `review` | Non-trivial work's OpenSpec `proposal.md` stays one page (≤ 60 lines; `spec:check` warns past it) and carries `Approved-by:`, empty until the owner fills `<name> (<YYYY-MM-DD>) sha256:<12 hex>`, the digest `spec:queue` prints, which `spec:check` fails once `proposal.md` or `design.md` changes; the change's first task holds `Hold: OPEN-Q proposal awaits owner approval` until the owner removes it in the same edit, and an agent whose Stop boundary is proposal approval stops there and hands off | `.agents/protocol.md` |
| `review` | A non-trivial change declares the `quick` or `design` track at Clarify; `design` is required when it adds a spec Requirement, touches more than one capability under `openspec/specs/`, adds exported surface reachable from an adopting repository, or touches a boot path, durability/ack behavior, or a gate file, and then carries a `design.md` (≤ 120 lines; `spec:check` warns past it) drafted by the read-only `design` skill and approved with the proposal under the one `OPEN-Q` hold | `.agents/protocol.md` § Clarify; `.agents/skills/design/SKILL.md` |
| `verify` | `task verify` runs formatting (`fmt:check`), lint (`lint`), Go + Vitest tests (`test`), OpenSpec validation (`spec:check`), test-to-spec attribution (`test:spec`), and multi-agent parity (`doctor`) on a clean tree | `Taskfile.yml` |
| `verify+review` | Review requires an independent reviewer not weaker than every implementer by model tier, then effort band, cross-provider separation per `.agents/review-policy.json`, and current trusted-base evidence; the dormant configured path adds scoped owner ceiling exceptions | `ateam review`; `.agents/contracts/reviewer.md` |
| `merge:check+review` | Non-draft code PRs (any file outside `.md` and `openspec/**/*.{md,yml,yaml,json}`, plus `.claude/agents/`, `.agents/contracts/` and `.agents/skills/review-pr/`) carry `implemented-by:` and `reviewed-by:` each naming an agent (`claude`, `codex`, `gemini`) with its model and native effort in parentheses (`codex (gpt-6.2-astra, high)`), the reviewer not weaker by tier then band (`ateam review compare`) and a different agent when `.agents/review-policy.json` sets `cross_provider`, plus a PR comment review record (`reviewed-by: <agent>` or `### Independent Cross-Agent Review`) naming a PR commit with only clean `main` merges or docs-only commits after it | `.agents/review-policy.json`; `.agents/protocol.md` |
| `review` | A PR archives its completed OpenSpec change before it is marked ready; once its required check is green on an up-to-date branch and the squash auto-merge (GitHub or Gitea) has landed, the claiming agent removes its worktree and remote branch (the `main` run is informational), and a review checkout is removed when its record is posted | `.agents/protocol.md` § Close; `ateam state pickup` worktree labels |
