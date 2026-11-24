<!--
  SPDX-License-Identifier: MIT
  Copyright (c) 2026 Complete 360 Studio
  Copyright (c) 2026 boxbotix

  Provenance: Sigma.js v3 WebGL mount and non-WebGL fallback lifecycle adapted from
  C360Studio/semstreams-ui src/lib/components/runtime/SigmaCanvas.svelte
  @ 3814b3d59dab7136011429cad99a8148aeceb0cc (MIT), using bbops non-safety SKG tokens.
-->
<script lang="ts">
  import { MultiDirectedGraph } from "graphology";
  import { onMount } from "svelte";
  import {
    type CortexSearchHit,
    type GraphSearchClient,
    createGraphSearchClient,
  } from "$lib/cortex/search-client";
  import {
    CORTEX_QUERY_TIERS,
    type CortexQueryTier,
    buildCausalSubgraphModel,
    buildGroundedTripleCitations,
    buildRankedCortexHits,
  } from "$lib/cortex/view-model";
  import type { GraphQLClient, PathSearchResult } from "$lib/graphql";
  import {
    SKG_EDGE_PALETTE,
    syncTopologyToGraph,
  } from "$lib/skg/graphology-adapter";
  import { LayoutController } from "$lib/skg/sigma-layout";
  import type { SkgStore } from "$lib/stores/skg.svelte";
  import type { ViewportStore } from "$lib/stores/viewport.svelte";

  interface Props {
    skgStore: SkgStore;
    viewportStore: ViewportStore;
    graphqlClient: GraphQLClient;
    searchClient?: GraphSearchClient;
    onSyncHash?: (hash: string) => void;
  }

  let {
    skgStore,
    viewportStore,
    graphqlClient,
    searchClient = createGraphSearchClient(),
    onSyncHash,
  }: Props = $props();

  let queryText = $state("");
  let activeTier = $state<CortexQueryTier>("hybrid");
  let running = $state(false);
  let errorBanner = $state<string | null>(null);
  let neuralStatusBanner = $state<string | null>(null);
  let answerSummary = $state<string | null>(null);
  let indexedRevision = $state<number | null>(null);
  let damagedEntities = $state<number>(0);

  let rawHits = $state.raw<readonly CortexSearchHit[]>([]);
  let pathResults = $state.raw<readonly PathSearchResult[]>([]);

  let sigmaContainerEl: HTMLDivElement | undefined = $state();
  let webglAvailable = $state(false);
  let layoutRunning = $state(false);

  const graph = new MultiDirectedGraph();
  const layoutController = new LayoutController((active) => {
    layoutRunning = active;
  });

  interface SigmaLike {
    refresh(): void;
    kill(): void;
  }

  let sigmaInstance: SigmaLike | null = null;

  const rankedHits = $derived(
    buildRankedCortexHits(rawHits, (id) => skgStore.getEntity(id)),
  );

  const causalSubgraph = $derived(
    buildCausalSubgraphModel(
      skgStore.entities.values(),
      rankedHits.map((h) => h.entityId),
      pathResults,
      viewportStore.selectedEntityId,
    ),
  );

  const citations = $derived(
    buildGroundedTripleCitations(viewportStore.selectedEntity, indexedRevision),
  );

  function probeWebGLSupport(): boolean {
    if (
      typeof document === "undefined" ||
      typeof WebGLRenderingContext === "undefined" ||
      (typeof navigator !== "undefined" && /jsdom/i.test(navigator.userAgent))
    ) {
      return false;
    }
    try {
      const probe = document.createElement("canvas");
      const gl = probe.getContext("webgl2") ?? probe.getContext("webgl");
      return gl !== null;
    } catch {
      return false;
    }
  }

  async function expand2Hop(entityId: string): Promise<void> {
    if (!entityId) {
      return;
    }
    try {
      const res = await graphqlClient.pathSearch({
        startEntity: entityId,
        direction: "both",
        maxDepth: 2,
        maxNodes: 40,
      });
      pathResults = [res.data];
    } catch {
      // Keep existing subgraph if optional GraphQL expansion fails.
    }
  }

  async function executeSearch(ev?: Event): Promise<void> {
    ev?.preventDefault();
    const trimmed = queryText.trim();
    if (!trimmed || running) {
      return;
    }

    running = true;
    errorBanner = null;
    neuralStatusBanner = null;
    answerSummary = null;

    try {
      const collected: CortexSearchHit[] = [];

      if (activeTier === "lexical" || activeTier === "hybrid") {
        const lex = viewportStore.selectedEntityId
          ? await searchClient.localSearch({
              entityId: viewportStore.selectedEntityId,
              query: trimmed,
              limit: 15,
            })
          : await searchClient.globalSearch({
              query: trimmed,
              limit: 15,
            });
        if (lex.ok) {
          collected.push(...lex.hits);
          indexedRevision = lex.indexedRevision;
          damagedEntities = lex.damagedEntities;
          answerSummary = lex.answer;
        } else if (lex.refusal) {
          errorBanner = `${lex.refusal.code}: ${lex.refusal.message}`;
        }
      }

      if (activeTier === "semantic") {
        const predRes = await graphqlClient.entitiesByPredicate({
          predicate: trimmed,
        });
        for (const id of predRes.data.entities) {
          collected.push({
            entityId: id,
            label: null,
            score: 1,
            verb: "globalSearch",
            wordRank: null,
            vectorRank: null,
          });
        }
        damagedEntities = predRes.damagedEntities;
      }

      if (activeTier === "neural" || activeTier === "hybrid") {
        const sem = await searchClient.semantic({
          query: trimmed,
          limit: 10,
        });
        if (sem.ok) {
          collected.push(...sem.hits);
          if (sem.indexedRevision > 0) {
            indexedRevision = sem.indexedRevision;
          }
        } else if (sem.refusal?.notEnabled) {
          neuralStatusBanner = `Neural embeddings not enabled on host (${sem.refusal.code})`;
        } else if (sem.refusal && activeTier === "neural") {
          errorBanner = `${sem.refusal.code}: ${sem.refusal.message}`;
        }
      }

      rawHits = collected;
      const topId = collected[0]?.entityId ?? viewportStore.selectedEntityId;
      if (topId) {
        viewportStore.selectEntity(topId);
        onSyncHash?.(viewportStore.toUrlHash());
        await expand2Hop(topId);
      }
    } catch (err) {
      errorBanner = err instanceof Error ? err.message : "Cortex query failed";
    } finally {
      running = false;
    }
  }

  async function runFindSimilar(): Promise<void> {
    const entityId = viewportStore.selectedEntityId;
    if (!entityId || running) {
      return;
    }
    running = true;
    errorBanner = null;
    neuralStatusBanner = null;

    try {
      const sim = await searchClient.similar({ entityId, limit: 10 });
      if (sim.ok) {
        rawHits = sim.hits;
      } else if (sim.refusal?.notEnabled) {
        neuralStatusBanner = `Similarity embeddings not enabled on host (${sim.refusal.code})`;
      } else if (sim.refusal) {
        errorBanner = `${sim.refusal.code}: ${sim.refusal.message}`;
      }
      await expand2Hop(entityId);
    } catch (err) {
      errorBanner =
        err instanceof Error ? err.message : "Similarity lookup failed";
    } finally {
      running = false;
    }
  }

  function selectHit(entityId: string): void {
    viewportStore.selectEntity(entityId);
    onSyncHash?.(viewportStore.toUrlHash());
    void expand2Hop(entityId);
  }

  $effect(() => {
    const model = causalSubgraph;
    const { structureChanged } = syncTopologyToGraph(graph, model);
    if (sigmaInstance) {
      sigmaInstance.refresh();
      if (structureChanged) {
        layoutController.start(graph);
      }
    }
  });

  onMount(() => {
    webglAvailable = probeWebGLSupport();
    let disposed = false;

    if (webglAvailable && sigmaContainerEl) {
      void import("sigma")
        .then(({ default: Sigma }) => {
          if (disposed || !sigmaContainerEl) {
            return;
          }
          const renderer = new Sigma(graph, sigmaContainerEl, {
            renderEdgeLabels: false,
            defaultNodeColor: "#60a5fa",
            defaultEdgeColor: SKG_EDGE_PALETTE.default,
            labelColor: { color: "#e2e8f0" },
            labelSize: 11,
          });
          renderer.on("clickNode", ({ node }) => {
            selectHit(node);
          });
          sigmaInstance = renderer;
          if (graph.order > 1) {
            layoutController.start(graph);
          }
        })
        .catch(() => {
          webglAvailable = false;
        });
    }

    return () => {
      disposed = true;
      layoutController.stop();
      sigmaInstance?.kill();
      sigmaInstance = null;
    };
  });
</script>

<div class="cortex-stage" data-testid="cortex-workspace">
  <form
    class="cortex-query-bar"
    data-testid="cortex-query-bar"
    onsubmit={executeSearch}
  >
    <div class="tier-pills" role="group" aria-label="Cortex SKG query tier">
      {#each CORTEX_QUERY_TIERS as tier (tier)}
        <button
          type="button"
          class="tier-btn"
          class:active={activeTier === tier}
          data-testid="cortex-tier-{tier}"
          onclick={() => {
            activeTier = tier;
          }}
        >
          {tier.toUpperCase()}
        </button>
      {/each}
    </div>

    <input
      type="search"
      class="query-input"
      data-testid="cortex-query-input"
      placeholder={activeTier === "semantic"
        ? "Predicate pattern (e.g. uxs.authority.state or cop.contact.source)…"
        : "Search SKG entities, callsigns, descriptions, or causal links…"}
      bind:value={queryText}
    />

    <button
      type="submit"
      class="run-btn"
      data-testid="cortex-query-submit"
      disabled={running || queryText.trim().length === 0}
    >
      {running ? "Querying…" : "Search SKG"}
    </button>

    <button
      type="button"
      class="similar-btn"
      data-testid="cortex-similar-btn"
      disabled={running || !viewportStore.selectedEntityId}
      onclick={runFindSimilar}
    >
      2-Hop &amp; Similar
    </button>

    {#if indexedRevision !== null && indexedRevision > 0}
      <span class="rev-pill" data-testid="cortex-indexed-rev">
        idx rev #{indexedRevision}
        {#if damagedEntities > 0}
          · damaged:{damagedEntities}
        {/if}
      </span>
    {/if}
  </form>

  {#if neuralStatusBanner}
    <div class="neural-banner" data-testid="cortex-neural-status" role="status">
      {neuralStatusBanner}
    </div>
  {/if}

  {#if errorBanner}
    <div class="error-banner" data-testid="cortex-error-banner" role="alert">
      {errorBanner}
    </div>
  {/if}

  <div class="cortex-grid">
    <section
      class="cortex-col"
      data-testid="cortex-hits-column"
      aria-label="Ranked SKG retrieval hits"
    >
      <div class="col-head">
        <strong>RANKED RETRIEVAL HITS ({rankedHits.length})</strong>
      </div>
      {#if rankedHits.length === 0}
        <p class="muted">
          Run a Hybrid, Lexical, Semantic, or Neural query to rank matching UXS
          and external COP entities.
        </p>
      {:else}
        <ul class="hit-list">
          {#each rankedHits as hit (hit.entityId)}
            <li>
              <button
                type="button"
                class="hit-btn"
                class:selected={viewportStore.selectedEntityId === hit.entityId}
                data-testid="cortex-hit-item"
                data-entity-id={hit.entityId}
                onclick={() => selectHit(hit.entityId)}
              >
                <span class="hit-top">
                  <strong>{hit.displayLabel}</strong>
                  <span class="verb-badge">{hit.verbBadge}</span>
                </span>
                <span class="hit-sub">
                  score {hit.scoreLabel}
                  {#if hit.kvRevision !== null}
                    · kv rev #{hit.kvRevision}
                  {/if}
                </span>
              </button>
            </li>
          {/each}
        </ul>
      {/if}
    </section>

    <section
      class="cortex-col subgraph-col"
      data-testid="cortex-causal-subgraph"
      aria-label="2-hop causal evidence subgraph"
    >
      <div class="col-head">
        <strong>
          CAUSAL EVIDENCE SUBGRAPH ({causalSubgraph.nodes.length} nodes ·
          {causalSubgraph.edges.length} edges)
        </strong>
        {#if layoutRunning}
          <span class="muted">ForceAtlas2 settling…</span>
        {/if}
      </div>

      <div class="legend-row">
        <span class="legend-chip uxs">UXS Vehicle</span>
        <span class="legend-chip contact">External Contact</span>
        <span class="legend-chip neighbor">2-Hop Neighbor</span>
      </div>

      {#if webglAvailable}
        <div bind:this={sigmaContainerEl} class="sigma-host"></div>
      {:else}
        <div class="subgraph-fallback" data-testid="cortex-subgraph-fallback">
          {#if causalSubgraph.nodes.length === 0}
            <p class="muted">No SKG nodes in causal scope.</p>
          {:else}
            <ul class="fallback-nodes">
              {#each causalSubgraph.nodes as node (node.id)}
                <li>
                  <button
                    type="button"
                    class="fallback-node-btn"
                    data-node-color={node.color}
                    onclick={() => selectHit(node.id)}
                  >
                    <span class="node-swatch" style:background={node.color}
                    ></span>
                    <span>{node.label}</span>
                    <span class="muted">({node.nodeKind})</span>
                  </button>
                </li>
              {/each}
            </ul>
          {/if}
        </div>
      {/if}
    </section>

    <section
      class="cortex-col"
      data-testid="cortex-provenance-column"
      aria-label="Grounded SKG triple citations"
    >
      <div class="col-head">
        <strong>GROUNDED PROVENANCE ({citations.length})</strong>
      </div>

      {#if answerSummary}
        <div class="answer-box" data-testid="cortex-answer-summary">
          {answerSummary}
        </div>
      {/if}

      {#if citations.length === 0}
        <p class="muted">
          Select an entity hit or subgraph node to inspect kvRevision-grounded
          SKG triple assertions.
        </p>
      {:else}
        <ul class="citation-list" data-testid="cortex-citation-list">
          {#each citations as cite, idx (`${cite.predicate}:${idx}`)}
            <li class="citation-card">
              <div class="cite-head">
                <span class="cite-pred">{cite.predicate}</span>
                <span class="cite-rev">
                  kv#{cite.kvRevision}{cite.indexedRevision
                    ? ` · idx#${cite.indexedRevision}`
                    : ""}
                </span>
              </div>
              <strong class="cite-obj">{cite.objectText}</strong>
              <span class="cite-meta">
                {cite.source} · {cite.confidencePct}
              </span>
            </li>
          {/each}
        </ul>
      {/if}
    </section>
  </div>
</div>

<style>
  .cortex-stage {
    flex: 1;
    display: flex;
    flex-direction: column;
    gap: 8px;
    min-height: 480px;
    font-family: ui-monospace, SFMono-Regular, Menlo, monospace;
    font-size: 11px;
  }

  .cortex-query-bar {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    gap: 8px;
    padding: 8px 10px;
    border-radius: 6px;
    background: var(--ui-surface-secondary, #101726);
    border: 1px solid var(--ui-border-subtle, #1e293b);
  }

  .tier-pills {
    display: inline-flex;
    gap: 4px;
  }

  .tier-btn {
    padding: 4px 8px;
    border-radius: 4px;
    border: 1px solid var(--ui-border-strong, #334155);
    background: var(--ui-surface-elevated, #172033);
    color: var(--ui-text-secondary, #94a3b8);
    font: inherit;
    font-size: 10px;
    font-weight: 700;
    cursor: pointer;
  }

  .tier-btn.active {
    border-color: #818cf8;
    color: var(--ui-text-primary, #e2e8f0);
    background: rgba(129, 140, 248, 0.18);
  }

  .query-input {
    flex: 1;
    min-width: 220px;
    padding: 5px 9px;
    border-radius: 4px;
    border: 1px solid var(--ui-border-strong, #334155);
    background: #090d16;
    color: var(--ui-text-primary, #e2e8f0);
    font: inherit;
  }

  .run-btn,
  .similar-btn {
    padding: 5px 10px;
    border-radius: 4px;
    font: inherit;
    cursor: pointer;
  }

  .run-btn {
    border: 1px solid var(--ui-border-interactive, #3b82f6);
    background: rgba(59, 130, 246, 0.2);
    color: var(--ui-text-primary, #e2e8f0);
    font-weight: 700;
  }

  .similar-btn {
    border: 1px solid #818cf8;
    background: rgba(129, 140, 248, 0.14);
    color: var(--ui-text-primary, #e2e8f0);
  }

  .run-btn:disabled,
  .similar-btn:disabled {
    opacity: 0.45;
    cursor: not-allowed;
  }

  .rev-pill {
    padding: 2px 7px;
    border-radius: 999px;
    border: 1px solid #818cf8;
    color: #c7d2fe;
    font-size: 10px;
  }

  .neural-banner {
    padding: 6px 10px;
    border-radius: 5px;
    border: 1px solid rgba(129, 140, 248, 0.45);
    background: rgba(129, 140, 248, 0.12);
    color: #c7d2fe;
  }

  .error-banner {
    padding: 6px 10px;
    border-radius: 5px;
    border: 1px solid #64748b;
    background: rgba(51, 65, 85, 0.35);
    color: var(--ui-text-primary, #e2e8f0);
  }

  .cortex-grid {
    flex: 1;
    display: grid;
    grid-template-columns: 280px minmax(0, 1fr) 320px;
    gap: 8px;
    min-height: 400px;
  }

  .cortex-col {
    display: flex;
    flex-direction: column;
    gap: 8px;
    padding: 10px;
    border-radius: 6px;
    background: var(--ui-surface-secondary, #101726);
    border: 1px solid var(--ui-border-subtle, #1e293b);
    min-width: 0;
  }

  .col-head {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: 6px;
  }

  .muted {
    margin: 0;
    color: var(--ui-text-muted, #64748b);
  }

  .hit-list,
  .citation-list,
  .fallback-nodes {
    list-style: none;
    margin: 0;
    padding: 0;
    display: flex;
    flex-direction: column;
    gap: 5px;
    overflow-y: auto;
  }

  .hit-btn,
  .fallback-node-btn {
    width: 100%;
    display: flex;
    flex-direction: column;
    gap: 3px;
    padding: 6px 8px;
    border-radius: 4px;
    border: 1px solid var(--ui-border-strong, #334155);
    background: var(--ui-surface-elevated, #172033);
    color: var(--ui-text-primary, #e2e8f0);
    font: inherit;
    text-align: left;
    cursor: pointer;
  }

  .fallback-node-btn {
    flex-direction: row;
    align-items: center;
    gap: 6px;
  }

  .hit-btn.selected {
    border-color: #818cf8;
  }

  .hit-top {
    display: flex;
    justify-content: space-between;
    gap: 6px;
  }

  .verb-badge {
    font-size: 10px;
    color: #38bdf8;
  }

  .hit-sub {
    font-size: 10px;
    color: var(--ui-text-secondary, #94a3b8);
  }

  .legend-row {
    display: flex;
    flex-wrap: wrap;
    gap: 6px;
    font-size: 10px;
  }

  .legend-chip {
    padding: 2px 6px;
    border-radius: 999px;
    border: 1px solid transparent;
  }

  .legend-chip.uxs {
    color: #60a5fa;
    border-color: rgba(96, 165, 250, 0.45);
  }

  .legend-chip.contact {
    color: #22d3ee;
    border-color: rgba(34, 211, 238, 0.45);
  }

  .legend-chip.neighbor {
    color: #818cf8;
    border-color: rgba(129, 140, 248, 0.45);
  }

  .sigma-host,
  .subgraph-fallback {
    flex: 1;
    min-height: 300px;
    border-radius: 4px;
    background: #090d16;
    border: 1px solid var(--ui-border-subtle, #1e293b);
    padding: 6px;
  }

  .node-swatch {
    width: 8px;
    height: 8px;
    border-radius: 999px;
    flex-shrink: 0;
  }

  .answer-box {
    padding: 7px 8px;
    border-radius: 4px;
    background: rgba(56, 189, 248, 0.1);
    border: 1px solid rgba(56, 189, 248, 0.35);
    color: var(--ui-text-primary, #e2e8f0);
  }

  .citation-card {
    display: flex;
    flex-direction: column;
    gap: 3px;
    padding: 6px 8px;
    border-radius: 4px;
    background: var(--ui-surface-elevated, #172033);
  }

  .cite-head {
    display: flex;
    justify-content: space-between;
    gap: 6px;
    font-size: 10px;
  }

  .cite-pred {
    color: #38bdf8;
  }

  .cite-rev,
  .cite-meta {
    color: var(--ui-text-muted, #64748b);
    font-size: 10px;
  }

  .cite-obj {
    word-break: break-all;
  }

  @media (max-width: 1100px) {
    .cortex-grid {
      grid-template-columns: 1fr;
    }
  }
</style>
