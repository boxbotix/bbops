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
    type GraphSearchClient,
    createGraphSearchClient,
  } from "$lib/cortex/search-client";
  import {
    type AnalysisSessionStore,
    createAnalysisSessionStore,
  } from "$lib/cortex/session.svelte";
  import {
    CORTEX_QUERY_TIERS,
    buildCausalSubgraphModel,
    buildGroundedTripleCitations,
    buildRankedCortexHits,
    formatAnalysisEntityRef,
    summarizeAnalysisProvenance,
  } from "$lib/cortex/view-model";
  import type { GraphQLClient } from "$lib/graphql";
  import {
    SKG_EDGE_PALETTE,
    drawSkgNodeHover,
    drawSkgNodeLabel,
    syncTopologyToGraph,
  } from "$lib/skg/graphology-adapter";
  import { LayoutController } from "$lib/skg/sigma-layout";
  import type { SkgStore } from "$lib/stores/skg.svelte";
  import type { ViewportStore } from "$lib/stores/viewport.svelte";

  export interface CortexSigmaRenderer {
    refresh(): void;
    kill(): void;
    on(
      event: "clickNode" | "enterNode" | "leaveNode",
      handler: (payload: { node: string }) => void,
    ): void;
  }

  export type CortexSigmaConstructor = new (
    graph: MultiDirectedGraph,
    container: HTMLElement,
    settings: Record<string, unknown>,
  ) => CortexSigmaRenderer;

  interface Props {
    skgStore: SkgStore;
    viewportStore: ViewportStore;
    graphqlClient: GraphQLClient;
    searchClient?: GraphSearchClient;
    analysisSession?: AnalysisSessionStore;
    probeWebGL?: () => boolean;
    loadSigma?: () => Promise<CortexSigmaConstructor>;
    onSyncHash?: (hash: string) => void;
  }

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

  const defaultLoadSigma = (): Promise<CortexSigmaConstructor> =>
    import("sigma").then(
      (mod) => mod.default as unknown as CortexSigmaConstructor,
    );

  let {
    skgStore,
    viewportStore,
    graphqlClient,
    searchClient = createGraphSearchClient(),
    analysisSession,
    probeWebGL = probeWebGLSupport,
    loadSigma = defaultLoadSigma,
    onSyncHash,
  }: Props = $props();

  let ownedFallbackSession: AnalysisSessionStore | null = null;
  function getFallbackSession(): AnalysisSessionStore {
    if (!ownedFallbackSession) {
      ownedFallbackSession = createAnalysisSessionStore({
        skgStore,
        viewportStore,
        graphqlClient,
        searchClient,
      });
    }
    return ownedFallbackSession;
  }

  const session = $derived.by(() => analysisSession ?? getFallbackSession());

  let sigmaContainerEl: HTMLDivElement | undefined = $state();
  let mounted = $state(false);
  let webglAvailable = $state(false);
  let sigmaReady = $state(false);
  let hoveredNodeId = $state<string | null>(null);
  let layoutRunning = $state(false);

  const graph = new MultiDirectedGraph();
  const layoutController = new LayoutController((active) => {
    layoutRunning = active;
  });

  let sigmaInstance: CortexSigmaRenderer | null = null;
  let isUnmounted = false;

  const lookupEntity = (id: string) => skgStore.getEntity(id);

  const rankedHits = $derived(
    buildRankedCortexHits(session.rawHits, lookupEntity),
  );

  const causalSubgraph = $derived(
    buildCausalSubgraphModel(
      skgStore.entities.values(),
      rankedHits.map((h) => h.entityId),
      session.pathResults,
      session.inspectedEntityId,
    ),
  );

  const inspectedEntity = $derived(
    session.inspectedEntityId
      ? skgStore.getEntity(session.inspectedEntityId)
      : null,
  );

  const selectedHitRevision = $derived.by(() => {
    const selId = session.inspectedEntityId;
    if (!selId) {
      return null;
    }
    const matchedHit = rankedHits.find((h) => h.entityId === selId);
    return matchedHit?.indexedRevision ?? null;
  });

  const citations = $derived(
    buildGroundedTripleCitations(inspectedEntity, selectedHitRevision),
  );

  const provenanceSummary = $derived(
    summarizeAnalysisProvenance(
      session.provenance,
      session.queryText,
      session.activeTier,
      viewportStore.selectedEntityId,
      lookupEntity,
    ),
  );

  const inspectedEntityLabel = $derived(
    session.inspectedEntityId
      ? formatAnalysisEntityRef(session.inspectedEntityId, lookupEntity)
      : "none",
  );

  const currentSelectionLabel = $derived(
    viewportStore.selectedEntityId
      ? formatAnalysisEntityRef(viewportStore.selectedEntityId, lookupEntity)
      : "none",
  );

  const inspectionDiverged = $derived(
    session.inspectedEntityId !== viewportStore.selectedEntityId &&
      (session.inspectedEntityId !== null ||
        viewportStore.selectedEntityId !== null),
  );

  $effect(() => {
    const activeSession = session;
    return activeSession.attachView((hash) => onSyncHash?.(hash));
  });

  $effect(() => {
    session.observeMountedSelection(viewportStore.selectedEntityId);
  });

  $effect(() => {
    const model = causalSubgraph;
    const { structureChanged } = syncTopologyToGraph(graph, model);
    if (sigmaInstance) {
      sigmaInstance.refresh();
      if (structureChanged && graph.order > 1 && graph.size > 0) {
        layoutController.start(graph);
      }
    }
  });

  $effect(() => {
    void session.inspectedEntityId;
    void hoveredNodeId;
    sigmaInstance?.refresh();
  });

  $effect(() => {
    if (!mounted || !webglAvailable || !sigmaContainerEl) {
      return;
    }
    const container = sigmaContainerEl;
    let cancelled = false;
    let createdRenderer: CortexSigmaRenderer | null = null;

    void loadSigma()
      .then((Sigma) => {
        if (cancelled || isUnmounted || !webglAvailable) {
          return;
        }
        syncTopologyToGraph(graph, causalSubgraph);
        const renderer = new Sigma(graph, container, {
          renderEdgeLabels: false,
          stagePadding: 32,
          defaultNodeColor: "#60a5fa",
          defaultEdgeColor: SKG_EDGE_PALETTE.default,
          labelColor: { color: "#e2e8f0" },
          labelSize: 11,
          defaultDrawNodeLabel: drawSkgNodeLabel,
          defaultDrawNodeHover: drawSkgNodeHover,
          nodeReducer: (node: string, data: Record<string, unknown>) => {
            const res = { ...data };
            const focusId = hoveredNodeId ?? session.inspectedEntityId;
            if (focusId) {
              if (node === focusId) {
                res.highlighted = true;
              } else if (
                graph.hasNode(focusId) &&
                graph.hasNode(node) &&
                !graph.areNeighbors(focusId, node) &&
                graph.size > 0
              ) {
                res.color = "#1e293b";
                res.label = "";
              }
            }
            return res;
          },
        });
        renderer.on("clickNode", ({ node }) => {
          session.selectHit(node);
        });
        renderer.on("enterNode", ({ node }) => {
          hoveredNodeId = node;
          container.setAttribute("data-hovered-node", node);
        });
        renderer.on("leaveNode", () => {
          hoveredNodeId = null;
          container.setAttribute("data-hovered-node", "");
        });
        createdRenderer = renderer;
        sigmaInstance = renderer;
        sigmaReady = true;
        renderer.refresh();
        if (graph.order > 1 && graph.size > 0) {
          layoutController.start(graph);
        }
      })
      .catch(() => {
        if (!cancelled && !isUnmounted) {
          sigmaReady = false;
          webglAvailable = false;
        }
      });

    return () => {
      cancelled = true;
      sigmaReady = false;
      layoutController.stop();
      if (createdRenderer) {
        createdRenderer.kill();
        if (sigmaInstance === createdRenderer) {
          sigmaInstance = null;
        }
      }
    };
  });

  onMount(() => {
    isUnmounted = false;
    webglAvailable = probeWebGL();
    mounted = true;

    return () => {
      isUnmounted = true;
      mounted = false;
      sigmaReady = false;
      if (!analysisSession && ownedFallbackSession) {
        ownedFallbackSession.dispose();
        ownedFallbackSession = null;
      }
      layoutController.stop();
      sigmaInstance?.kill();
      sigmaInstance = null;
    };
  });
</script>

<div
  class="cortex-stage"
  data-testid="cortex-workspace"
  data-workspace-stage="cortex"
  role="region"
  aria-label="Analysis workspace"
  tabindex="-1"
>
  <form
    class="cortex-query-bar"
    data-testid="cortex-query-bar"
    onsubmit={(ev) => session.executeSearch(ev)}
  >
    <div class="tier-pills" role="group" aria-label="Analysis SKG query tier">
      {#each CORTEX_QUERY_TIERS as tier (tier)}
        <button
          type="button"
          class="tier-btn"
          class:active={session.activeTier === tier}
          aria-pressed={session.activeTier === tier}
          data-testid="cortex-tier-{tier}"
          onclick={() => {
            session.setActiveTier(tier);
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
      placeholder={session.activeTier === "semantic"
        ? "Predicate pattern (e.g. uxs.authority.state or cop.contact.source)…"
        : "Search SKG entities, callsigns, descriptions, or causal links…"}
      bind:value={session.queryText}
    />

    <button
      type="submit"
      class="run-btn"
      data-testid="cortex-query-submit"
      disabled={session.queryText.trim().length === 0}
    >
      {session.running ? "Querying…" : "Search SKG"}
    </button>

    {#if session.running}
      <button
        type="button"
        class="cancel-btn"
        data-testid="cortex-query-cancel"
        onclick={() => session.cancelSearch()}
      >
        Cancel
      </button>
    {/if}

    <button
      type="button"
      class="similar-btn"
      data-testid="cortex-similar-btn"
      disabled={!viewportStore.selectedEntityId && !session.inspectedEntityId}
      onclick={() => session.runFindSimilar()}
    >
      2-Hop &amp; Similar
    </button>

    {#if session.status !== "idle"}
      <span
        class="status-pill"
        data-testid="cortex-session-status"
        data-status={session.status}
      >
        {session.status}
      </span>
    {/if}

    {#if session.indexedRevision !== null && session.indexedRevision > 0}
      <span class="rev-pill" data-testid="cortex-indexed-rev">
        idx rev #{session.indexedRevision}
        {#if session.damagedEntities > 0}
          · damaged:{session.damagedEntities}
        {/if}
      </span>
    {:else if session.damagedEntities > 0}
      <span class="rev-pill" data-testid="cortex-damaged-pill">
        damaged:{session.damagedEntities}
      </span>
    {/if}
  </form>

  {#if provenanceSummary}
    <div
      class="provenance-banner"
      class:diverged={provenanceSummary.earlierSelectionMismatch ||
        provenanceSummary.draftQueryDiverged}
      data-testid="cortex-result-provenance"
      data-earlier-selection={provenanceSummary.earlierSelectionMismatch
        ? "true"
        : "false"}
      role="status"
    >
      <span>{provenanceSummary.headline}</span>
    </div>
  {/if}

  {#if inspectionDiverged}
    <div
      class="divergence-banner"
      data-testid="cortex-inspection-divergence"
      role="status"
    >
      <span>
        Pinned inspection: {inspectedEntityLabel} · Current selection: {currentSelectionLabel}
      </span>
      <button
        type="button"
        class="sync-btn"
        data-testid="cortex-sync-selection-btn"
        onclick={() => session.syncInspectionToSelection()}
      >
        {viewportStore.selectedEntityId
          ? `Inspect ${currentSelectionLabel}`
          : "Clear Pinned Inspection"}
      </button>
    </div>
  {/if}

  {#if session.status === "cancelled" && session.cancelledSummary}
    <div
      class="cancelled-banner"
      data-testid="cortex-cancelled-banner"
      role="status"
    >
      {session.cancelledSummary}
    </div>
  {/if}

  {#if session.neuralStatusBanner}
    <div class="neural-banner" data-testid="cortex-neural-status" role="status">
      {session.neuralStatusBanner}
    </div>
  {/if}

  {#if session.errorBanner}
    <div class="error-banner" data-testid="cortex-error-banner" role="alert">
      {session.errorBanner}
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
                class:selected={session.inspectedEntityId === hit.entityId}
                data-testid="cortex-hit-item"
                data-entity-id={hit.entityId}
                onclick={() => session.selectHit(hit.entityId)}
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

      <div
        bind:this={sigmaContainerEl}
        class="sigma-host"
        class:hidden={!webglAvailable}
        data-testid="cortex-sigma-host"
        data-sigma-ready={sigmaReady ? "true" : "false"}
        data-highlighted-node={session.inspectedEntityId ?? ""}
        data-hovered-node={hoveredNodeId ?? ""}
        role="application"
        aria-label="Causal SKG interactive WebGL canvas"
        tabindex="-1"
        onpointerdown={() => {
          sigmaContainerEl?.focus?.({ preventScroll: true });
        }}
      ></div>

      {#if !webglAvailable}
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
                    onclick={() => session.selectHit(node.id)}
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

      {#if session.answerSummary}
        <div class="answer-box" data-testid="cortex-answer-summary">
          {session.answerSummary}
        </div>
      {/if}

      {#if session.inspectedEntityId !== null && inspectedEntity === null}
        <p class="unavailable-note" data-testid="cortex-entity-unavailable">
          Entity {inspectedEntityLabel} ({session.inspectedEntityId}) is
          unavailable in live Fleet data — grounded SKG triple citations cannot
          be resolved.
        </p>
      {:else if citations.length === 0}
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
  .cancel-btn,
  .similar-btn,
  .sync-btn {
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

  .cancel-btn,
  .sync-btn {
    border: 1px solid var(--ui-border-strong, #334155);
    background: var(--ui-surface-elevated, #172033);
    color: var(--ui-text-primary, #e2e8f0);
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

  .status-pill,
  .rev-pill {
    padding: 2px 7px;
    border-radius: 999px;
    font-size: 10px;
  }

  .status-pill {
    border: 1px solid var(--ui-border-strong, #334155);
    background: var(--ui-surface-elevated, #172033);
    color: var(--ui-text-secondary, #94a3b8);
    text-transform: uppercase;
  }

  .rev-pill {
    border: 1px solid #818cf8;
    color: #c7d2fe;
  }

  .provenance-banner,
  .divergence-banner,
  .cancelled-banner {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    justify-content: space-between;
    gap: 8px;
    padding: 6px 10px;
    border-radius: 5px;
    border: 1px solid var(--ui-border-strong, #334155);
    background: var(--ui-surface-secondary, #101726);
    color: var(--ui-text-secondary, #94a3b8);
  }

  .provenance-banner.diverged,
  .divergence-banner {
    border-color: rgba(56, 189, 248, 0.45);
    background: rgba(56, 189, 248, 0.08);
    color: var(--ui-text-primary, #e2e8f0);
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

  .unavailable-note {
    margin: 0;
    padding: 8px;
    border-radius: 4px;
    border: 1px solid rgba(129, 140, 248, 0.4);
    background: var(--ui-surface-elevated, #172033);
    color: var(--ui-text-secondary, #94a3b8);
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

  .sigma-host {
    position: relative;
    overflow: hidden;
    padding: 0;
  }

  .sigma-host.hidden {
    display: none;
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
