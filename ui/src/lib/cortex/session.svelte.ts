// SPDX-License-Identifier: MIT
// Copyright (c) 2026 boxbotix

import { untrack } from "svelte";
import type { GraphQLClient, PathSearchResult } from "../graphql";
import type { SkgStore } from "../stores/skg.svelte";
import type { ViewportStore } from "../stores/viewport.svelte";
import { isValidEntityIdString } from "../uxs";
import {
  type CortexSearchHit,
  type GraphSearchClient,
  createGraphSearchClient,
} from "./search-client";
import {
  type AnalysisQueryStatus,
  type AnalysisResultProvenance,
  type AnalysisRetrievalScope,
  type AnalysisSubmissionMode,
  type CortexQueryTier,
  buildRankedCortexHits,
  isCortexQueryTier,
  resolveAnalysisRetrievalScope,
} from "./view-model";

function sanitizeEntityId(value: string | null | undefined): string | null {
  return isValidEntityIdString(value) ? value : null;
}

export interface AnalysisInFlightRequest {
  readonly reqSeq: number;
  readonly mode: AnalysisSubmissionMode;
  readonly submittedQuery: string;
  readonly submittedTier: CortexQueryTier;
  readonly retrievalScope: AnalysisRetrievalScope;
  readonly scopeEntityId: string | null;
  readonly submittedSelectionId: string | null;
}

export interface CreateAnalysisSessionOptions {
  readonly skgStore: SkgStore;
  readonly viewportStore: ViewportStore;
  readonly graphqlClient: GraphQLClient;
  readonly searchClient?: GraphSearchClient;
}

export interface AnalysisSessionStore {
  queryText: string;
  activeTier: CortexQueryTier;
  readonly status: AnalysisQueryStatus;
  readonly running: boolean;
  readonly errorBanner: string | null;
  readonly neuralStatusBanner: string | null;
  readonly cancelledSummary: string | null;
  readonly answerSummary: string | null;
  readonly indexedRevision: number | null;
  readonly damagedEntities: number;
  readonly rawHits: readonly CortexSearchHit[];
  readonly pathResults: readonly PathSearchResult[];
  readonly provenance: AnalysisResultProvenance | null;
  readonly inFlight: AnalysisInFlightRequest | null;
  readonly inspectedEntityId: string | null;
  readonly viewMounted: boolean;
  setActiveTier(tier: CortexQueryTier): void;
  attachView(onSyncHash?: (hash: string) => void): () => void;
  observeMountedSelection(selectedEntityId: string | null): void;
  syncInspectionToSelection(): void;
  selectHit(entityId: string): void;
  executeSearch(ev?: Event): Promise<void>;
  runFindSimilar(): Promise<void>;
  cancelSearch(): boolean;
  dispose(): void;
}

function isAbortError(err: unknown, signal: AbortSignal): boolean {
  if (signal.aborted) {
    return true;
  }
  return (
    typeof err === "object" &&
    err !== null &&
    "name" in err &&
    (err as { name?: string }).name === "AbortError"
  );
}

/**
 * Session-scoped Analysis (Cortex) investigation store (#40).
 *
 * Preserves query text, tier, ranked hits, 2-hop causal `pathSearch` results,
 * immutable submission provenance, and pinned `inspectedEntityId` across
 * workspace switches while enforcing monotonic `reqSeq` stale-result refusal
 * and refusing cross-workspace selection/hash mutations when unmounted.
 */
export function createAnalysisSessionStore(
  options: CreateAnalysisSessionOptions,
): AnalysisSessionStore {
  const {
    skgStore,
    viewportStore,
    graphqlClient,
    searchClient = createGraphSearchClient(),
  } = options;

  let queryText = $state("");
  let activeTier = $state<CortexQueryTier>("hybrid");
  let status = $state<AnalysisQueryStatus>("idle");
  let errorBanner = $state<string | null>(null);
  let neuralStatusBanner = $state<string | null>(null);
  let cancelledSummary = $state<string | null>(null);
  let answerSummary = $state<string | null>(null);
  let indexedRevision = $state<number | null>(null);
  let damagedEntities = $state<number>(0);

  const initialEntityId = sanitizeEntityId(viewportStore.selectedEntityId);
  let rawHits = $state.raw<readonly CortexSearchHit[]>([]);
  let pathResults = $state.raw<readonly PathSearchResult[]>([]);
  let provenance = $state<AnalysisResultProvenance | null>(null);
  let inFlight = $state<AnalysisInFlightRequest | null>(null);
  let inspectedEntityId = $state<string | null>(initialEntityId);
  let viewMounted = $state(false);

  let hasInvestigationContext = initialEntityId !== null;
  let expandedEntityId: string | null = null;
  let lastKnownViewportSelection: string | null = initialEntityId;
  let activeSyncHash: ((hash: string) => void) | undefined;
  let isDisposed = false;
  let activeSearchController: AbortController | null = null;
  let activeExpandController: AbortController | null = null;
  let searchReqSeq = 0;
  let expandReqSeq = 0;
  let selectionEpoch = 0;
  let inFlightPriorInspectedId: string | null = initialEntityId;
  let inFlightEpochAtStart = 0;

  async function expand2Hop(
    entityId: string,
    parentSearchSeq?: number,
  ): Promise<void> {
    const sanitized = sanitizeEntityId(entityId);
    if (!sanitized || isDisposed) {
      return;
    }
    activeExpandController?.abort();
    const controller = new AbortController();
    activeExpandController = controller;
    const reqSeq = ++expandReqSeq;
    const epochAtStart = selectionEpoch;

    try {
      const res = await graphqlClient.pathSearch(
        {
          startEntity: sanitized,
          direction: "both",
          maxDepth: 2,
          maxNodes: 40,
        },
        { signal: controller.signal },
      );
      if (
        isDisposed ||
        controller.signal.aborted ||
        reqSeq !== expandReqSeq ||
        (parentSearchSeq !== undefined && parentSearchSeq !== searchReqSeq) ||
        selectionEpoch !== epochAtStart ||
        (inspectedEntityId !== null && inspectedEntityId !== sanitized)
      ) {
        return;
      }
      pathResults = [res.data];
      expandedEntityId = sanitized;
    } catch {
      // Keep existing subgraph if optional GraphQL expansion fails or is aborted.
    } finally {
      if (activeExpandController === controller) {
        activeExpandController = null;
      }
    }
  }

  function setActiveTier(tier: CortexQueryTier): void {
    if (isCortexQueryTier(tier)) {
      activeTier = tier;
    }
  }

  function attachView(onSyncHash?: (hash: string) => void): () => void {
    if (isDisposed) {
      return () => {};
    }
    untrack(() => {
      viewMounted = true;
      activeSyncHash = onSyncHash;
      const currentSelection = sanitizeEntityId(viewportStore.selectedEntityId);
      lastKnownViewportSelection = currentSelection;

      if (!hasInvestigationContext && currentSelection !== null) {
        inspectedEntityId = currentSelection;
        hasInvestigationContext = true;
      }
    });

    return () => {
      untrack(() => {
        viewMounted = false;
        if (activeSyncHash === onSyncHash) {
          activeSyncHash = undefined;
        }
      });
    };
  }

  function observeMountedSelection(selectedEntityId: string | null): void {
    untrack(() => {
      if (!viewMounted || isDisposed) {
        return;
      }
      const normalized = sanitizeEntityId(selectedEntityId);
      if (normalized === lastKnownViewportSelection) {
        return;
      }
      lastKnownViewportSelection = normalized;
      selectionEpoch += 1;
      activeExpandController?.abort();
      activeExpandController = null;
      ++expandReqSeq;
      inspectedEntityId = normalized;
      if (normalized !== null) {
        hasInvestigationContext = true;
      }
      if (expandedEntityId !== normalized) {
        pathResults = [];
        expandedEntityId = null;
      }
    });
  }

  function syncInspectionToSelection(): void {
    if (isDisposed) {
      return;
    }
    const currentSelection = sanitizeEntityId(viewportStore.selectedEntityId);
    lastKnownViewportSelection = currentSelection;
    selectionEpoch += 1;
    activeExpandController?.abort();
    activeExpandController = null;
    ++expandReqSeq;
    inspectedEntityId = currentSelection;
    hasInvestigationContext =
      currentSelection !== null || hasInvestigationContext;
    pathResults = [];
    expandedEntityId = null;
    if (currentSelection) {
      void expand2Hop(currentSelection);
    }
  }

  function selectHit(entityId: string): void {
    const sanitized = sanitizeEntityId(entityId);
    if (!sanitized || isDisposed) {
      return;
    }
    selectionEpoch += 1;
    hasInvestigationContext = true;
    inspectedEntityId = sanitized;
    pathResults = [];
    expandedEntityId = null;
    if (viewMounted) {
      viewportStore.selectEntity(sanitized);
      lastKnownViewportSelection = sanitizeEntityId(
        viewportStore.selectedEntityId,
      );
      activeSyncHash?.(viewportStore.toUrlHash());
    }
    void expand2Hop(sanitized);
  }

  function cancelSearch(): boolean {
    if (status !== "running" || isDisposed) {
      return false;
    }
    const cancelledReq = inFlight;
    const restoreInspected = selectionEpoch === inFlightEpochAtStart;
    ++searchReqSeq;
    ++expandReqSeq;
    activeSearchController?.abort();
    activeExpandController?.abort();
    activeSearchController = null;
    activeExpandController = null;
    inFlight = null;
    rawHits = [];
    pathResults = [];
    expandedEntityId = null;
    provenance = null;
    indexedRevision = null;
    damagedEntities = 0;
    answerSummary = null;
    errorBanner = null;
    neuralStatusBanner = null;
    if (restoreInspected) {
      inspectedEntityId = inFlightPriorInspectedId;
    }
    status = "cancelled";
    cancelledSummary = cancelledReq
      ? cancelledReq.mode === "query"
        ? `Cancelled ${cancelledReq.submittedTier.toUpperCase()} query "${cancelledReq.submittedQuery}".`
        : "Cancelled 2-Hop & Similar lookup."
      : "Cancelled Analysis query.";
    return true;
  }

  async function executeSearch(ev?: Event): Promise<void> {
    ev?.preventDefault();
    const trimmed = queryText.trim();
    if (!trimmed || isDisposed) {
      return;
    }

    activeSearchController?.abort();
    activeExpandController?.abort();
    const controller = new AbortController();
    activeSearchController = controller;
    const reqSeq = ++searchReqSeq;
    ++expandReqSeq;
    const tierAtStart = activeTier;
    const selectionAtStart = sanitizeEntityId(viewportStore.selectedEntityId);
    const retrievalScope = resolveAnalysisRetrievalScope(
      "query",
      tierAtStart,
      selectionAtStart,
    );
    const epochAtStart = selectionEpoch;

    hasInvestigationContext = true;
    inFlightPriorInspectedId = inspectedEntityId;
    inFlightEpochAtStart = epochAtStart;
    if (inspectedEntityId === null && selectionAtStart !== null) {
      inspectedEntityId = selectionAtStart;
    }

    status = "running";
    inFlight = {
      reqSeq,
      mode: "query",
      submittedQuery: trimmed,
      submittedTier: tierAtStart,
      retrievalScope,
      scopeEntityId: retrievalScope.entityId,
      submittedSelectionId: selectionAtStart,
    };
    cancelledSummary = null;
    rawHits = [];
    pathResults = [];
    expandedEntityId = null;
    provenance = null;
    errorBanner = null;
    neuralStatusBanner = null;
    answerSummary = null;
    indexedRevision = null;
    damagedEntities = 0;

    try {
      const collected: CortexSearchHit[] = [];
      let nextIndexedRevision: number | null = null;
      let nextDamagedEntities = 0;
      let nextAnswerSummary: string | null = null;
      let nextNeuralBanner: string | null = null;
      let nextErrorBanner: string | null = null;

      if (tierAtStart === "lexical" || tierAtStart === "hybrid") {
        const lex = selectionAtStart
          ? await searchClient.localSearch(
              {
                entityId: selectionAtStart,
                query: trimmed,
                limit: 15,
              },
              controller.signal,
            )
          : await searchClient.globalSearch(
              {
                query: trimmed,
                limit: 15,
              },
              controller.signal,
            );
        if (lex.ok) {
          collected.push(...lex.hits);
          if (lex.indexedRevision > 0) {
            nextIndexedRevision = Math.max(
              nextIndexedRevision ?? 0,
              lex.indexedRevision,
            );
          }
          nextDamagedEntities = Math.max(
            nextDamagedEntities,
            lex.damagedEntities,
          );
          nextAnswerSummary = lex.answer;
        } else if (lex.refusal) {
          nextErrorBanner = `${lex.refusal.code}: ${lex.refusal.message}`;
        }
      }

      if (tierAtStart === "semantic") {
        const predRes = await graphqlClient.entitiesByPredicate(
          {
            predicate: trimmed,
          },
          { signal: controller.signal },
        );
        for (const id of predRes.data.entities) {
          collected.push({
            entityId: id,
            label: null,
            score: 1,
            verb: "entitiesByPredicate",
            wordRank: null,
            vectorRank: null,
            indexedRevision: null,
          });
        }
        nextDamagedEntities = predRes.damagedEntities;
      }

      if (tierAtStart === "neural" || tierAtStart === "hybrid") {
        const sem = await searchClient.semantic(
          {
            query: trimmed,
            limit: 10,
          },
          controller.signal,
        );
        if (sem.ok) {
          collected.push(...sem.hits);
          if (sem.indexedRevision > 0) {
            nextIndexedRevision = Math.max(
              nextIndexedRevision ?? 0,
              sem.indexedRevision,
            );
          }
          nextDamagedEntities = Math.max(
            nextDamagedEntities,
            sem.damagedEntities,
          );
        } else if (sem.refusal?.notEnabled) {
          nextNeuralBanner = `Neural embeddings not enabled on host (${sem.refusal.code})`;
        } else if (sem.refusal && tierAtStart === "neural") {
          nextErrorBanner = `${sem.refusal.code}: ${sem.refusal.message}`;
        }
      }

      if (isDisposed || controller.signal.aborted || reqSeq !== searchReqSeq) {
        return;
      }

      const isTerminalQueryError =
        nextErrorBanner !== null && collected.length === 0;
      rawHits = collected;
      indexedRevision = nextIndexedRevision;
      damagedEntities = nextDamagedEntities;
      answerSummary = nextAnswerSummary;
      neuralStatusBanner = nextNeuralBanner;
      errorBanner = nextErrorBanner;
      provenance = isTerminalQueryError
        ? null
        : {
            reqSeq,
            mode: "query",
            submittedQuery: trimmed,
            submittedTier: tierAtStart,
            retrievalScope,
            scopeEntityId: retrievalScope.entityId,
            submittedSelectionId: selectionAtStart,
            settledSelectionId: selectionAtStart,
          };

      let expandTargetId: string | null = null;
      if (!isTerminalQueryError && selectionEpoch === epochAtStart) {
        const topRanked = buildRankedCortexHits(collected, (id) =>
          skgStore.getEntity(id),
        )[0];
        const topId = sanitizeEntityId(topRanked?.entityId ?? selectionAtStart);
        if (
          topId &&
          (!viewMounted ||
            sanitizeEntityId(viewportStore.selectedEntityId) ===
              selectionAtStart)
        ) {
          expandTargetId = topId;
          inspectedEntityId = topId;
          await expand2Hop(topId, reqSeq);
        }
      }

      if (isDisposed || controller.signal.aborted || reqSeq !== searchReqSeq) {
        return;
      }

      let settledSelectionId = selectionAtStart;
      if (
        expandTargetId !== null &&
        selectionEpoch === epochAtStart &&
        viewMounted &&
        sanitizeEntityId(viewportStore.selectedEntityId) === selectionAtStart
      ) {
        inspectedEntityId = expandTargetId;
        if (expandTargetId !== viewportStore.selectedEntityId) {
          viewportStore.selectEntity(expandTargetId);
          lastKnownViewportSelection = sanitizeEntityId(
            viewportStore.selectedEntityId,
          );
          activeSyncHash?.(viewportStore.toUrlHash());
        }
        settledSelectionId = sanitizeEntityId(viewportStore.selectedEntityId);
      }

      provenance = isTerminalQueryError
        ? null
        : {
            reqSeq,
            mode: "query",
            submittedQuery: trimmed,
            submittedTier: tierAtStart,
            retrievalScope,
            scopeEntityId: retrievalScope.entityId,
            submittedSelectionId: selectionAtStart,
            settledSelectionId,
          };
      inFlight = null;
      status = isTerminalQueryError ? "error" : "completed";
    } catch (err) {
      if (
        !isDisposed &&
        !isAbortError(err, controller.signal) &&
        reqSeq === searchReqSeq
      ) {
        rawHits = [];
        pathResults = [];
        expandedEntityId = null;
        provenance = null;
        inFlight = null;
        indexedRevision = null;
        damagedEntities = 0;
        answerSummary = null;
        status = "error";
        errorBanner =
          err instanceof Error ? err.message : "Analysis query failed";
      }
    } finally {
      if (activeSearchController === controller) {
        activeSearchController = null;
      }
      if (!isDisposed && reqSeq === searchReqSeq && status === "running") {
        inFlight = null;
        status = "idle";
      }
    }
  }

  async function runFindSimilar(): Promise<void> {
    const submittedSelectionId = sanitizeEntityId(
      viewportStore.selectedEntityId,
    );
    const entityId = sanitizeEntityId(
      submittedSelectionId ?? inspectedEntityId,
    );
    if (!entityId || isDisposed) {
      return;
    }

    activeSearchController?.abort();
    activeExpandController?.abort();
    const controller = new AbortController();
    activeSearchController = controller;
    const reqSeq = ++searchReqSeq;
    ++expandReqSeq;
    const tierAtStart = activeTier;
    const epochAtStart = selectionEpoch;
    const retrievalScope = resolveAnalysisRetrievalScope(
      "similar",
      tierAtStart,
      entityId,
    );

    hasInvestigationContext = true;
    inFlightPriorInspectedId = inspectedEntityId;
    inFlightEpochAtStart = epochAtStart;
    inspectedEntityId = entityId;
    status = "running";
    inFlight = {
      reqSeq,
      mode: "similar",
      submittedQuery: queryText.trim(),
      submittedTier: tierAtStart,
      retrievalScope,
      scopeEntityId: retrievalScope.entityId,
      submittedSelectionId,
    };
    cancelledSummary = null;
    rawHits = [];
    pathResults = [];
    expandedEntityId = null;
    provenance = null;
    errorBanner = null;
    neuralStatusBanner = null;
    answerSummary = null;
    indexedRevision = null;
    damagedEntities = 0;

    try {
      const sim = await searchClient.similar(
        { entityId, limit: 10 },
        controller.signal,
      );
      if (isDisposed || controller.signal.aborted || reqSeq !== searchReqSeq) {
        return;
      }
      let nextErrorBanner: string | null = null;
      const simHits = sim.ok ? sim.hits : [];
      const isTerminalSimilarError =
        !sim.ok &&
        sim.refusal !== null &&
        !sim.refusal.notEnabled &&
        simHits.length === 0;
      rawHits = simHits;
      if (sim.ok) {
        indexedRevision = sim.indexedRevision > 0 ? sim.indexedRevision : null;
        damagedEntities = sim.damagedEntities;
      } else if (sim.refusal?.notEnabled) {
        neuralStatusBanner = `Similarity embeddings not enabled on host (${sim.refusal.code})`;
      } else if (sim.refusal) {
        nextErrorBanner = `${sim.refusal.code}: ${sim.refusal.message}`;
        errorBanner = nextErrorBanner;
      }
      provenance = isTerminalSimilarError
        ? null
        : {
            reqSeq,
            mode: "similar",
            submittedQuery: "",
            submittedTier: tierAtStart,
            retrievalScope,
            scopeEntityId: retrievalScope.entityId,
            submittedSelectionId,
            settledSelectionId: entityId,
          };

      if (
        !isTerminalSimilarError &&
        selectionEpoch === epochAtStart &&
        (!viewMounted ||
          sanitizeEntityId(viewportStore.selectedEntityId) === null ||
          sanitizeEntityId(viewportStore.selectedEntityId) === entityId)
      ) {
        await expand2Hop(entityId, reqSeq);
      }

      if (isDisposed || controller.signal.aborted || reqSeq !== searchReqSeq) {
        return;
      }

      provenance = isTerminalSimilarError
        ? null
        : {
            reqSeq,
            mode: "similar",
            submittedQuery: "",
            submittedTier: tierAtStart,
            retrievalScope,
            scopeEntityId: retrievalScope.entityId,
            submittedSelectionId,
            settledSelectionId: entityId,
          };
      inFlight = null;
      status = isTerminalSimilarError ? "error" : "completed";
    } catch (err) {
      if (
        !isDisposed &&
        !isAbortError(err, controller.signal) &&
        reqSeq === searchReqSeq
      ) {
        rawHits = [];
        pathResults = [];
        expandedEntityId = null;
        provenance = null;
        inFlight = null;
        indexedRevision = null;
        damagedEntities = 0;
        answerSummary = null;
        status = "error";
        errorBanner =
          err instanceof Error ? err.message : "Similarity lookup failed";
      }
    } finally {
      if (activeSearchController === controller) {
        activeSearchController = null;
      }
      if (!isDisposed && reqSeq === searchReqSeq && status === "running") {
        inFlight = null;
        status = "idle";
      }
    }
  }

  function dispose(): void {
    isDisposed = true;
    viewMounted = false;
    activeSyncHash = undefined;
    ++searchReqSeq;
    ++expandReqSeq;
    activeSearchController?.abort();
    activeExpandController?.abort();
    activeSearchController = null;
    activeExpandController = null;
    inFlight = null;
  }

  return {
    get queryText() {
      return queryText;
    },
    set queryText(next: string) {
      queryText = next;
    },
    get activeTier() {
      return activeTier;
    },
    set activeTier(next: CortexQueryTier) {
      setActiveTier(next);
    },
    get status() {
      return status;
    },
    get running() {
      return status === "running";
    },
    get errorBanner() {
      return errorBanner;
    },
    get neuralStatusBanner() {
      return neuralStatusBanner;
    },
    get cancelledSummary() {
      return cancelledSummary;
    },
    get answerSummary() {
      return answerSummary;
    },
    get indexedRevision() {
      return indexedRevision;
    },
    get damagedEntities() {
      return damagedEntities;
    },
    get rawHits() {
      return rawHits;
    },
    get pathResults() {
      return pathResults;
    },
    get provenance() {
      return provenance;
    },
    get inFlight() {
      return inFlight;
    },
    get inspectedEntityId() {
      return inspectedEntityId;
    },
    get viewMounted() {
      return viewMounted;
    },
    setActiveTier,
    attachView,
    observeMountedSelection,
    syncInspectionToSelection,
    selectHit,
    executeSearch,
    runFindSimilar,
    cancelSearch,
    dispose,
  };
}
