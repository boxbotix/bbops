# Rules Enforcement Index

Mirror of the rule tables in `AGENTS.md`; `ateam doctor --check` fails when the two disagree.

## 1. Ponytail decision ladder, clean-room provenance, and code discipline

| Gate | Rule | Enforcement & Test |
| --- | --- | --- |
| `review` | `PROVENANCE.md` and `.agents/rules/ip-cleanroom.md` are authoritative (`MIT`, Zero-GPLv3, `common.xml`-only MAVLink v2, zero `semlink`/`semops` imports); the agent harness adds nothing to them and overrides nothing in them | `PROVENANCE.md`; `.agents/rules/ip-cleanroom.md` |
| `verify+review` | Follow the 6-step Ponytail decision ladder; no unrequested abstractions, wrappers, or dependencies | `ateam hook` |
| `verify+review` | Every intentional deferral uses `// shortcut: <the limit>, <when to upgrade>`, scanned by `ateam state debt` | `ateam state debt` |
| `verify+review` | Non-trivial logic ships with a test using an independent oracle and proven able to fail via non-destructive mutation check (`task mutate` / `ateam mutate` or `cp` + SHA-256; never `git checkout`/`restore`/`stash`/`reset`), reporting `detection`, `survivor`, `invalid`, or `inconclusive` | `ateam mutate` |
| `verify+review` | Changes with interacting inputs, stated laws (round-trip, idempotence), or order-dependent histories record whether generated checks (Go `testing.F` or PBT) are used or why named examples suffice, prove boundary reach and assertion activation, and check histories against a reference model | `task test`; review |
| `verify+review` | Every Go `Test`/`Fuzz`/executable `Example` and Vitest `it`/`test` declaration carries attached spec references (`// spec:` or `//aip:verifies`) resolved by `task test:spec`; syntactic attribution does not prove semantic coverage | `ateam test-spec`; `.agents/protocol.md` |
| `verify` | Source passes formatting, vet, typecheck, and linting with zero warnings (`gofmt`, `go vet`, `golangci-lint`, `svelte-check`, `eslint`) | `Taskfile.yml` |
| `review` | Task conclusions and handoffs end with 1–2 lines stating what was skipped/not checked and any residual risk; acknowledgements and progress updates need no footer but still name blockers, failed or unavailable checks, and changed risks | review |

## 2. Hooks, MCP budget, and multi-agent parity

| Gate | Rule | Enforcement & Test |
| --- | --- | --- |
| `verify` | `AGENTS.md` stays under 24,000 bytes, `CLAUDE.md` only imports `@AGENTS.md`, and rule row counts match `.agents/rules-index.md` | `ateam doctor` |
| `verify` | Canonical skills in `.agents/skills/*/SKILL.md` have matching thin adapters in `.claude/skills/` and `.gemini/commands/` | `ateam doctor` |
| `verify` | Hook configs in `.claude/settings.json`, `.codex/hooks.json`, `.gemini/settings.json`, and `.agents/hooks.json` map native events to `ateam hook`; with `ATEAM_HOOK_SCOPE=user`, `ateam doctor` also accepts one complete local or user-scope file (`.claude/settings.local.json`, `~/.claude/settings.json`, `~/.codex/hooks.json`, `~/.gemini/settings.json`) and names the file under `hook_sources` | `ateam doctor` |
| `verify` | `ateam mcp` exposes only the 2 multiplexed tools (`ateam_state`, `ateam_task`) with `<300` total schema tokens | `ateam doctor` |
| `verify` | `ateam hook` executes normalized lifecycle events in `<10ms` target budget (`doctor` warns `>10ms`, fails `--check` `>100ms`) with zero external runtime dependencies | `ateam doctor` |

## 3. State, OpenSpec, Forge (GitHub/Gitea), and Definition of Done

| Gate | Rule | Enforcement & Test |
| --- | --- | --- |
| `review` | Clarify Deliverable, Scope/non-goals, Definition of Done, and Stop boundary before mutating state | `.agents/protocol.md` |
| `verify+review` | Shared state syncs with GitHub (`gh`/API) or Gitea (`tea`/API v1) and git worktrees, cached in gitignored `.ateam/state.json` | `ateam state pickup` |
| `verify` | OpenSpec changes/specs validate via `spec:check`, and `Hold:` / caveats (`HALT`, `RED`, `BLOCKED`, `WONTDO`, `OPEN-Q`) are parsed by `spec:queue` | `ateam state spec-check` |
| `review` | Non-trivial work's OpenSpec `proposal.md` stays one page (≤ 60 lines; `spec:check` warns past it) and carries `Approved-by:`, empty until the owner fills `<name> (<YYYY-MM-DD>) sha256:<12 hex>`, the digest `spec:queue` prints, which `spec:check` fails once `proposal.md` or `design.md` changes; the change's first task holds `Hold: OPEN-Q proposal awaits owner approval` until the owner removes it in the same edit, and an agent whose Stop boundary is proposal approval stops there and hands off | `ateam state spec-check`; review |
| `review` | A non-trivial change declares the `quick` or `design` track at Clarify; `design` is required when it adds a spec Requirement, touches more than one capability under `openspec/specs/`, adds exported surface reachable from an adopting repository, or touches a boot path, durability/ack behavior, or a gate file, and then carries a `design.md` (≤ 120 lines; `spec:check` warns past it) drafted by the read-only `design` skill and approved with the proposal under the one `OPEN-Q` hold | `ateam state spec-check`; review |
| `verify` | `task verify` runs formatting (`fmt:check`), lint (`lint`), Go + Vitest tests (`test`), OpenSpec validation (`spec:check`), test-to-spec attribution (`test:spec`), and multi-agent parity (`doctor`) on a clean tree | `task verify` |
| `verify+review` | Review requires an independent reviewer not weaker than every implementer by model tier, then effort band, cross-provider separation per `.agents/review-policy.json`, and current trusted-base evidence; the dormant configured path adds scoped owner ceiling exceptions | `ateam review`; `.agents/contracts/reviewer.md` |
| `merge:check+review` | Non-draft code PRs (any file outside `.md` and `openspec/**/*.{md,yml,yaml,json}`, plus `.claude/agents/`, `.agents/contracts/` and `.agents/skills/review-pr/`) carry `implemented-by:` and `reviewed-by:` each naming an agent (`claude`, `codex`, `gemini`) with its model and native effort in parentheses (`codex (gpt-6.2-astra, high)`), the reviewer not weaker by tier then band (`ateam review compare`) and a different agent when `.agents/review-policy.json` sets `cross_provider`, plus a PR comment review record (`reviewed-by: <agent>` or `### Independent Cross-Agent Review`) naming a PR commit with only clean `main` merges or docs-only commits after it | `ateam review gate` |
| `review` | A PR archives its completed OpenSpec change before it is marked ready; once its required check is green on an up-to-date branch and the squash auto-merge (GitHub or Gitea) has landed, the claiming agent removes its worktree and remote branch (the `main` run is informational), and a review checkout is removed when its record is posted | `.agents/protocol.md` § Close; `ateam state pickup` worktree labels |
