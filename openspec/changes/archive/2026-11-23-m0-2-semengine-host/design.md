# Design: m0-2-semengine-host

Architect: `gemini (gemini-4-carbon, medium)`

## Context

- `cmd/bbops/main.go:19-37`: `run` parses `-version` only and exits `0` on zero args (`:35-36` `M0.1` shortcut); `cmd/bbops/main_test.go:25-34` and `openspec/specs/host-runtime/spec.md:8` codify that `M0.1` zero-arg no-op.
- `go.mod:1-3`: `module github.com/boxbotix/bbops` (`go 1.26.11`), zero external dependencies.
- `Taskfile.yml:18,25`: `gofmt` runs only on `./cmd` (avoiding `ui/node_modules`); adding `internal/host` requires adding `./internal`.
- `ui/vite.config.ts:13-16`: dev server listens on `:5173` with no backend proxy; `PROVENANCE.md:32` and `THIRD_PARTY_NOTICES:17-20` require 40-char SHA + SPDX headers on adapted Tier-1 files.
- `C360Studio/semengine` @ `3d89a31775219e7f7aaa44ab10892bbc804ae08e` (`v0.1.0-alpha.7`):
  - `go.mod:1`: module path is lowercase `github.com/c360studio/semengine` (pinned `nats-server/v2 v2.15.2`, `nats.go v1.55.0`).
  - `config/streams.go:18-47`: ordinary JetStream streams require explicit finite `max_age`, `max_bytes > 0`, and `discard` (`"old"`/`"new"`).
  - `service/graph_runtime.go:16-35`: `WireGraphRuntime` enforces `kvcatalog.EnsureCatalogRetentionClean` (`ENTITY_STATES` `History: 1`, `RetentionNoLifecycle`) and builds `projection.MutationClient`.
  - `service/service_manager.go:165-167,359-388` & `service/service_manager_http.go:44-100,278-286,447-460`: `HTTPPort: 0` defaults to `8080` (`:165`); `startMainListener` binds before `Start(ctx)` runs on `component-manager`/`graph-ingest` (`:359-376`), so `/healthz` (`handleLiveness`, `:447`) answers `200 OK` immediately while `/readyz` (`handleReadiness`, `:454`, ADR-109 `:45-48`) answers `503 NOT READY` until all components start and `fullMux` (`/graph-gateway/graphql`, `:283`) commits (`:387`).
  - `service/service_manager_http.go:88-93,211-225` & `docs/adr/115-http-surfaces.md:38-63`: `ServiceManager` wraps routes in `WriteTimeout` and `httpadmission.Requests` (blocking `StopAll` on open streams); ADR-115 isolates streaming on `output/websocket` listeners.
  - `output/websocket/config.go:28-33,138-199`: refuses removed keys (`delivery_mode`, `ack_timeout`, `passthrough`), accepts one `kv-watch` (`ENTITY_STATES`, no keys/history) or core `nats` subject (no queue), and requires `network.Port` in `1024..65535` (`port: 0` refused at `:194`).
  - `processor/graph-query/config.go:23-29` & `processor/graph-lexical/config.go:86-92,117-126`: tier-0 defaults (`embedding_enabled: false`, `ENTITY_STATES` `kv-watch`) need no `objectstore`.
- `C360Studio/semboids` @ `37dbdb088438a2601c8556d012ab2c33bec85da2`: `cmd/semboids/main.go:130-219` (NATS -> `EnsureStreams` -> `ConfigManager.Start` -> `ServiceManager`) and `ui/Caddyfile:9-65` (reverse-proxy split, `encode @notSSE gzip`).

## Goals / Non-Goals

- **Goals:** Single-binary embedded NATS/JetStream + `semengine v0.1.0-alpha.7` SKG pipeline (`ENTITY` -> `graph-ingest` -> `ENTITY_STATES` -> `graph-index`/`graph-lexical`/`graph-query` -> `graph-gateway`), dual `output/websocket` planes (`bbops.telemetry.>` and `ENTITY_STATES`), deterministic readiness gating, UI static/proxy serving, and in-process ephemeral-port integration tests.
- **Non-Goals:** MAVLink UDP listener or `uxs.*` ontology (#5/#6), `processor/rule` or spatial/temporal indexes, TLS/ACME, or GitHub Actions CI.

## Decisions

### D1. CLI Invocation Contract & Embedded NATS Mode

| Option | Cost | Blast radius | Forecloses |
| --- | --- | --- | --- |
| **A. Daemon default + `-validate` flag**: zero-arg `bbops` boots embedded NATS (`JetStream: true`, `-data-dir`, `-nats-url` override) + SemEngine until signal/ctx cancel; `-validate` runs offline `config` + `composition.Validate` and exits `0` | Update 1 `M0.1` test/scenario (`main_test.go:25-34`, `spec.md:8`) | `cmd/bbops`, `openspec/specs/host-runtime/spec.md` | Treating bare `bbops` as a side-effect-free no-op in scripts |
| **B. Explicit `bbops serve` subcommand**: bare `bbops` keeps `M0.1` no-op exit `0`; `bbops serve` boots runtime | Subcommand routing in `cmd/bbops` | Every operator, container, and `Taskfile` run command | Single-command appliance UX matching `semboids/cmd/semboids/main.go:89` |

Adopter seam: With A, running `./bin/bbops` immediately starts the GCS on default ports and `-validate` checks config offline; doing nothing in tests that call `run(nil, ...)` without a canceled `ctx` or `-validate` blocks waiting for shutdown. With B, operators running `./bin/bbops` get silent exit `0` instead of a running server. **Ruled Option A (owner, 2026-11-23).**

### D2. Readiness Probe Contract (`/readyz` vs `/healthz`) & Ephemeral Test Ports

| Option | Cost | Blast radius | Forecloses |
| --- | --- | --- | --- |
| **A. Native SemEngine ADR-109 probes**: `/readyz` (`200 READY` only after `graph-ingest` + all components finish `Start` and `fullMux` commits) is the readiness gate; `/healthz` (`200 OK`) stays liveness; `/health` reports JSON component+NATS health | Zero custom probe middleware | Aligns `#3`'s readiness wording with SemEngine `service_manager_http.go:447-460` | Using `/healthz` alone as a cold-start readiness gate |
| **B. Gate `/healthz` via `UseHTTPMiddleware`**: intercept `/healthz` (`service_manager_http.go:44`) to return `503 NOT READY` until `currentStartupSnapshot` / `readyz` is `200` | ~20 LOC middleware + test | Main listener `/healthz` diverges from dedicated health listener (`:156`) | Container liveness probes on `/healthz` surviving slow JetStream startup replay |

Adopter seam: Callers and integration tests waiting for `graph-ingest` responders and `/graph-gateway/graphql` must poll `/readyz` under A (or either `/readyz`/`/healthz` under B); polling `/healthz` under A without checking `/readyz` hits `503 NOT READY` on `/graph-gateway/graphql` during cold start. Because `HTTPPort: 0` defaults to `8080` and `output/websocket` refuses `port < 1024`, test harnesses must allocate free loopback ports in `1024..65535`. **Ruled Option A (owner, 2026-11-23).**

### D3. UI Serving (`ui/build`) & Dual `output/websocket` Topology

| Option | Cost | Blast radius | Forecloses |
| --- | --- | --- | --- |
| **A. Go static SPA fallback on `:8080` + native WS ports (`:8081`, `:8082`) + dev proxy**: `UseHTTPMiddleware` serves `-ui-dir` (`ui/build` when present) on unmatched `GET`/`HEAD` paths after readiness; WS #1 (`:8081`) and WS #2 (`:8082`) stay on native `output/websocket` listeners; `ui/Caddyfile` + `ui/vite.config.ts` proxy all three in dev/container | ~55 LOC static/SPA middleware + `ui/Caddyfile` + Vite proxy | `internal/host`, `ui/Caddyfile`, `ui/vite.config.ts` | Mounting arbitrary catch-all API routes outside reserved prefixes (`/health*`, `/readyz`, `/services`, `/components/`, `/graph/`, `/graph-gateway/`, `/openapi.json`, `/docs`) |
| **B. Caddy/Vite reverse-proxy only**: Go serves only SemEngine `:8080` + WS `:8081`/`:8082`; Caddy/Vite serves `ui/build` and proxies API/WS | `ui/Caddyfile` + Vite proxy only (0 Go static code) | Requires `caddy` or Node alongside `bbops` to view UI | Standalone single-binary `./bin/bbops` serving compiled `ui/build` in the field |

Adopter seam: Browser UI code connects WS #1/WS #2 via configurable URLs (defaulting to same-host `:8081/ws` & `:8082/ws` when unproxied, or `/ws/telemetry` & `/ws/graph` behind Caddy/Vite); reverse-proxying WS inside `ServiceManager` middleware is avoided because `WriteTimeout` and `httpadmission.Requests` (`service_manager_http.go:92,211-225`) cut or block shutdown. If `-ui-dir` is absent under A, unmatched routes return `404` cleanly. **Ruled Option A (owner, 2026-11-23).**

### D4. Flow Configuration Source & `ENTITY` Stream Retention Bounds

| Option | Cost | Blast radius | Forecloses |
| --- | --- | --- | --- |
| **A. Versioned `configs/bbops.json` + `go:embed` fallback**: canonical JSON flow (`ENTITY`: `entity.>`, `file`, `max_age: "24h"`, `max_bytes: 268435456` [256 MiB], `discard: "old"`, `replicas: 1`) embedded as default when `-config` is unset, with CLI port overrides | 1 JSON file + `embed` loader + `THIRD_PARTY_NOTICES` entry | `configs/bbops.json`, `internal/host` | Drift only if Go overrides mutate component names |
| **B. Pure Go struct builder + optional `-config` file**: construct default `*config.Config` in Go code with no `configs/bbops.json` in repo | Go builder only | `internal/host` | Inspecting/diffing default port wiring directly in a static JSON file like `semboids/configs/flock.json` |

Adopter seam: Any custom `-config` file must specify finite `max_age`, `max_bytes > 0`, and `discard` on every stream (`semengine/config/streams.go:18-47`) and omit removed WS keys; doing nothing uses the embedded default seamlessly. **Ruled Option A (owner, 2026-11-23).**

## Risks / Trade-offs

- **Ephemeral port TOCTOU in tests:** Probing a free TCP port (`1024..65535`) and closing before `StartAll`/`websocket.Start` leaves a tiny bind-race window. Mitigation: bind `127.0.0.1` in tests and retry host boot up to 3 times on `EADDRINUSE`.
- **Embedded NATS disk growth:** Without finite bounds JetStream fills field disks. Mitigation: temporary `t.TempDir()` in tests, `-data-dir` flag in CLI, and mandatory `max_bytes: 268435456` + `History: 1` `ENTITY_STATES`.

## Migration Plan

Replaces the `M0.1` bootstrap stub in `cmd/bbops/main.go:35-36` and updates `openspec/specs/host-runtime/spec.md`. No persisted production state exists yet (`v0.0.0`); rollback is reverting the merge commit.

## Open Questions

1. **Resolved (`OQ1`–`OQ4`, owner 2026-11-23):** D1 Option A (`bbops` daemon default + `-validate`), D2 Option A (native ADR-109 `/readyz` readiness + `/healthz` liveness), D3 Option A (`-ui-dir` SPA fallback on `:8080` + native WS `:8081`/`:8082` + `ui/Caddyfile` & Vite proxy), and D4 Option A (versioned `configs/bbops.json` with `go:embed` fallback and `256 MiB / 24h / old` `ENTITY` stream bounds).
