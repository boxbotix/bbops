<!--
  SPDX-License-Identifier: MIT
  Copyright (c) 2026 Complete 360 Studio
  Copyright (c) 2026 boxbotix

  Provenance: Adapted from Sigma.js v3 WebGL graph canvas, neighborhood reducer,
  and ForceAtlas2 lifecycle in C360Studio/semstreams-ui
  src/lib/components/runtime/SigmaCanvas.svelte
  @ 3814b3d59dab7136011429cad99a8148aeceb0cc (MIT).
-->
<script lang="ts">
  import { MultiDirectedGraph } from "graphology";
  import { onMount } from "svelte";
  import type { GraphQLClient, PathSearchResult } from "$lib/graphql";
  import {
    SKG_EDGE_PALETTE,
    buildSkgTopologyModel,
    drawSkgNodeHover,
    drawSkgNodeLabel,
    formatEntityTriples,
    formatTransitionEvent,
    syncTopologyToGraph,
  } from "$lib/skg/graphology-adapter";
  import { LayoutController } from "$lib/skg/sigma-layout";
  import type { SkgStore } from "$lib/stores/skg.svelte";
  import type { TelemetryStore } from "$lib/stores/telemetry.svelte";
  import type { ViewportStore } from "$lib/stores/viewport.svelte";

  interface Props {
    skgStore: SkgStore;
    telemetryStore: TelemetryStore;
    viewportStore: ViewportStore;
    graphqlClient: GraphQLClient;
    onSyncHash?: (hash: string) => void;
  }

  let {
    skgStore,
    telemetryStore,
    viewportStore,
    graphqlClient,
    onSyncHash,
  }: Props = $props();

  let sigmaContainerEl: HTMLDivElement | undefined = $state();
  let webglAvailable = $state(false);
  let sigmaReady = $state(false);
  let layoutRunning = $state(false);
  let hoveredNodeId = $state<string | null>(null);
  let expanding = $state(false);
  let expandError = $state<string | null>(null);
  let expandedSearches = $state.raw<readonly PathSearchResult[]>([]);

  const graph = new MultiDirectedGraph();
  const layoutController = new LayoutController((running) => {
    layoutRunning = running;
  });

  interface SigmaCameraLike {
    animatedZoom(opts?: { duration?: number }): void;
    animatedUnzoom(opts?: { duration?: number }): void;
    animatedReset(opts?: { duration?: number }): void;
  }

  interface SigmaInstanceLike {
    refresh(): void;
    kill(): void;
    getCamera(): SigmaCameraLike;
  }

  let sigmaInstance: SigmaInstanceLike | null = null;

  const topologyModel = $derived(
    buildSkgTopologyModel(skgStore.entities.values(), expandedSearches),
  );
  const tripleRows = $derived(
    formatEntityTriples(viewportStore.selectedEntity),
  );
  const transitionRows = $derived(
    [...telemetryStore.events].reverse().map(formatTransitionEvent),
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

  async function expandNeighborhood(entityId: string): Promise<void> {
    if (!entityId || expanding) {
      return;
    }
    expanding = true;
    expandError = null;
    try {
      const res = await graphqlClient.pathSearch({
        startEntity: entityId,
        direction: "both",
        maxDepth: 2,
        maxNodes: 50,
      });
      expandedSearches = [...expandedSearches, res.data];
    } catch (err) {
      expandError =
        err instanceof Error ? err.message : "pathSearch expansion failed";
    } finally {
      expanding = false;
    }
  }

  function selectNode(entityId: string): void {
    viewportStore.selectEntity(entityId);
    onSyncHash?.(viewportStore.toUrlHash());
  }

  $effect(() => {
    const model = topologyModel;
    const { structureChanged } = syncTopologyToGraph(graph, model);
    if (sigmaInstance) {
      sigmaInstance.refresh();
      if (structureChanged) {
        layoutController.start(graph);
      }
    }
  });

  $effect(() => {
    void viewportStore.selectedEntityId;
    void hoveredNodeId;
    sigmaInstance?.refresh();
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
            stagePadding: 32,
            defaultNodeColor: "#60a5fa",
            defaultEdgeColor: SKG_EDGE_PALETTE.default,
            labelColor: { color: "#e2e8f0" },
            labelSize: 11,
            defaultDrawNodeLabel: drawSkgNodeLabel,
            defaultDrawNodeHover: drawSkgNodeHover,
            nodeReducer: (node, data) => {
              const res = { ...data };
              const focusId = hoveredNodeId ?? viewportStore.selectedEntityId;
              if (node === viewportStore.selectedEntityId) {
                res.highlighted = true;
              }
              if (focusId && graph.hasNode(focusId) && node !== focusId) {
                const isNeighbor =
                  graph.hasNode(node) && graph.areNeighbors(focusId, node);
                if (!isNeighbor) {
                  res.color = "#334155";
                  res.label = "";
                }
              }
              return res;
            },
            edgeReducer: (edge, data) => {
              const res = { ...data };
              const focusId = hoveredNodeId ?? viewportStore.selectedEntityId;
              if (focusId && graph.hasNode(focusId) && graph.hasEdge(edge)) {
                const [src, dst] = graph.extremities(edge);
                if (src !== focusId && dst !== focusId) {
                  res.color = SKG_EDGE_PALETTE.dimmed;
                }
              }
              return res;
            },
          });

          renderer.on("clickNode", ({ node }) => {
            selectNode(node);
          });
          renderer.on("doubleClickNode", (payload) => {
            payload.preventSigmaDefault();
            selectNode(payload.node);
            void expandNeighborhood(payload.node);
          });
          renderer.on("enterNode", ({ node }) => {
            hoveredNodeId = node;
            sigmaContainerEl?.setAttribute("data-hovered-node", node);
          });
          renderer.on("leaveNode", () => {
            hoveredNodeId = null;
            sigmaContainerEl?.setAttribute("data-hovered-node", "");
          });

          sigmaInstance = renderer;
          sigmaReady = true;
          if (graph.order >= 2 && graph.size > 0) {
            layoutController.start(graph);
          }
        })
        .catch(() => {
          sigmaReady = false;
          webglAvailable = false;
        });
    }

    return () => {
      disposed = true;
      sigmaReady = false;
      layoutController.stop();
      sigmaInstance?.kill();
      sigmaInstance = null;
    };
  });
</script>

<aside
  class="skg-pane"
  data-testid="skg-pane"
  aria-label="SKG topology and inspector"
>
  <section class="skg-topology-card">
    <header class="pane-header">
      <div class="header-title">
        <strong>SKG Topology</strong>
        <span class="meta-count">
          {topologyModel.nodes.length}N · {topologyModel.edges.length}E
          {#if layoutRunning}· layout{/if}
        </span>
      </div>
      <div class="graph-controls">
        {#if webglAvailable}
          <button
            type="button"
            class="ctrl-btn"
            aria-label="Zoom in topology"
            onclick={() =>
              sigmaInstance?.getCamera().animatedZoom({ duration: 150 })}
          >
            +
          </button>
          <button
            type="button"
            class="ctrl-btn"
            aria-label="Zoom out topology"
            onclick={() =>
              sigmaInstance?.getCamera().animatedUnzoom({ duration: 150 })}
          >
            -
          </button>
          <button
            type="button"
            class="ctrl-btn"
            aria-label="Fit topology"
            onclick={() =>
              sigmaInstance?.getCamera().animatedReset({ duration: 180 })}
          >
            Fit
          </button>
        {/if}
        <button
          type="button"
          class="ctrl-btn"
          disabled={!viewportStore.selectedEntityId || expanding}
          onclick={() => {
            if (viewportStore.selectedEntityId) {
              void expandNeighborhood(viewportStore.selectedEntityId);
            }
          }}
        >
          {expanding ? "Expanding…" : "2-Hop"}
        </button>
      </div>
    </header>

    {#if expandError}
      <p class="expand-error" role="status">{expandError}</p>
    {/if}

    <div
      class="sigma-stage"
      data-testid="skg-sigma-stage"
      data-sigma-ready={sigmaReady ? "true" : "false"}
      data-highlighted-node={viewportStore.selectedEntityId ?? ""}
      data-hovered-node={hoveredNodeId ?? ""}
      bind:this={sigmaContainerEl}
      class:hidden={!webglAvailable}
    ></div>

    {#if !webglAvailable}
      <div class="topology-fallback" data-testid="skg-topology-fallback">
        {#if topologyModel.nodes.length === 0}
          <p class="empty-note">Awaiting ENTITY_STATES snapshot…</p>
        {:else}
          <ul class="node-list">
            {#each topologyModel.nodes as node (node.id)}
              <li>
                <button
                  type="button"
                  class="node-item"
                  aria-pressed={viewportStore.selectedEntityId === node.id}
                  onclick={() => selectNode(node.id)}
                  ondblclick={() => void expandNeighborhood(node.id)}
                >
                  <span class="node-dot" style:background-color={node.color}
                  ></span>
                  <span>{node.label}</span>
                </button>
              </li>
            {/each}
          </ul>
        {/if}
      </div>
    {/if}
  </section>

  <section class="inspector-card" data-testid="skg-triple-inspector">
    <header class="pane-header">
      <strong>Triple Inspector</strong>
      <span class="meta-count">{tripleRows.length} triples</span>
    </header>
    {#if tripleRows.length === 0}
      <p class="empty-note">
        {viewportStore.selectedEntityId
          ? "Selected entity has zero cached triples."
          : "Select a graph node or tactical track to inspect uxs.* / cop.* triples."}
      </p>
    {:else}
      <div class="table-scroll">
        <table>
          <thead>
            <tr>
              <th>Predicate</th>
              <th>Value</th>
              <th>Source</th>
              <th>Timestamp</th>
            </tr>
          </thead>
          <tbody>
            {#each tripleRows as row, idx (`${idx}:${row.predicate}:${row.source}:${row.timestamp}`)}
              <tr>
                <td class="pred-cell">{row.predicate}</td>
                <td class="val-cell">{row.objectText}</td>
                <td class="src-cell">{row.source}</td>
                <td class="ts-cell">{row.timestamp}</td>
              </tr>
            {/each}
          </tbody>
        </table>
      </div>
    {/if}
  </section>

  <section class="timeline-card" data-testid="transition-event-timeline">
    <header class="pane-header">
      <strong>Transition Events</strong>
      <span class="meta-count">{transitionRows.length}</span>
    </header>
    {#if transitionRows.length === 0}
      <p class="empty-note">No Lane 3 STATUSTEXT or authority transitions.</p>
    {:else}
      <ul class="event-list">
        {#each transitionRows as ev (ev.key)}
          <li class="event-item severity-{ev.severity}">
            <button
              type="button"
              class="event-btn"
              onclick={() => selectNode(ev.entityId)}
            >
              <span class="event-headline">{ev.headline}</span>
              <span class="event-detail">{ev.detail}</span>
            </button>
          </li>
        {/each}
      </ul>
    {/if}
  </section>
</aside>

<style>
  .skg-pane {
    flex: 1 1 auto;
    display: flex;
    flex-direction: column;
    gap: 8px;
    min-height: 0;
  }

  .skg-topology-card,
  .inspector-card,
  .timeline-card {
    display: flex;
    flex-direction: column;
    background: var(--ui-surface-secondary, #101726);
    border: 1px solid var(--ui-border-subtle, #1e293b);
    border-radius: 6px;
    padding: 8px;
    font-family: ui-monospace, SFMono-Regular, Menlo, monospace;
    font-size: 11px;
  }

  .inspector-card {
    flex: 1 1 auto;
    min-height: 190px;
  }

  .pane-header {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: 6px;
    margin-bottom: 6px;
    color: var(--ui-text-primary, #e2e8f0);
  }

  .header-title,
  .graph-controls {
    display: inline-flex;
    align-items: center;
    gap: 4px;
  }

  .meta-count {
    color: var(--ui-text-muted, #64748b);
    font-size: 10px;
  }

  .ctrl-btn {
    background: var(--ui-surface-elevated, #172033);
    color: var(--ui-text-primary, #e2e8f0);
    border: 1px solid var(--ui-border-strong, #334155);
    border-radius: 4px;
    padding: 1px 6px;
    font-size: 10px;
    font-family: inherit;
    cursor: pointer;
  }

  .ctrl-btn:disabled {
    opacity: 0.45;
    cursor: not-allowed;
  }

  .sigma-stage {
    width: 100%;
    height: 190px;
    border-radius: 4px;
    background: var(--ui-surface-primary, #090d16);
  }

  .sigma-stage.hidden {
    display: none;
  }

  .topology-fallback {
    max-height: 180px;
    overflow-y: auto;
    background: var(--ui-surface-primary, #090d16);
    border-radius: 4px;
    padding: 4px;
  }

  .node-list,
  .event-list {
    list-style: none;
    margin: 0;
    padding: 0;
    display: flex;
    flex-direction: column;
    gap: 4px;
  }

  .node-item {
    width: 100%;
    display: flex;
    align-items: center;
    gap: 6px;
    background: transparent;
    color: var(--ui-text-secondary, #94a3b8);
    border: 1px solid transparent;
    border-radius: 4px;
    padding: 3px 6px;
    font-family: inherit;
    font-size: 11px;
    text-align: left;
    cursor: pointer;
  }

  .node-item[aria-pressed="true"] {
    color: var(--ui-text-primary, #e2e8f0);
    border-color: var(--ui-border-interactive, #3b82f6);
    background: rgba(59, 130, 246, 0.16);
  }

  .node-dot {
    width: 8px;
    height: 8px;
    border-radius: 50%;
    flex-shrink: 0;
  }

  .table-scroll {
    flex: 1 1 auto;
    min-height: 150px;
    max-height: 320px;
    overflow: auto;
  }

  table {
    width: 100%;
    border-collapse: collapse;
    font-size: 10px;
  }

  th,
  td {
    padding: 3px 4px;
    text-align: left;
    border-bottom: 1px solid var(--ui-border-subtle, #1e293b);
    vertical-align: top;
  }

  th {
    color: var(--ui-text-muted, #64748b);
  }

  .pred-cell {
    color: var(--skg-node-vehicle, #60a5fa);
    white-space: nowrap;
  }

  .val-cell {
    color: var(--ui-text-primary, #e2e8f0);
    word-break: break-word;
  }

  .src-cell,
  .ts-cell {
    color: var(--ui-text-muted, #64748b);
    white-space: nowrap;
  }

  .event-list {
    max-height: 160px;
    overflow-y: auto;
  }

  .event-item {
    border-left: 3px solid var(--ui-border-strong, #334155);
    border-radius: 3px;
    background: rgba(15, 23, 42, 0.6);
  }

  .event-item.severity-error {
    border-left-color: var(--status-error, #ef4444);
  }

  .event-item.severity-warning {
    border-left-color: var(--status-warning, #f59e0b);
  }

  .event-item.severity-success {
    border-left-color: var(--status-success, #22c55e);
  }

  .event-btn {
    width: 100%;
    display: flex;
    flex-direction: column;
    gap: 2px;
    background: transparent;
    border: none;
    color: inherit;
    padding: 4px 6px;
    text-align: left;
    font-family: inherit;
    font-size: 10px;
    cursor: pointer;
  }

  .event-headline {
    font-weight: 700;
    color: var(--ui-text-primary, #e2e8f0);
  }

  .event-detail,
  .empty-note {
    color: var(--ui-text-secondary, #94a3b8);
    margin: 0;
  }

  .expand-error {
    margin: 0 0 6px;
    color: var(--skg-node-contact, #22d3ee);
    font-size: 10px;
  }
</style>
