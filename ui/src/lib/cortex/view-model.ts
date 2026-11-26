// SPDX-License-Identifier: MIT
// Copyright (c) 2026 boxbotix

import type { PathSearchResult } from "../graphql";
import {
  type SkgTopologyModel,
  buildSkgTopologyModel,
  formatEntityTriples,
  shortEntityLabel,
} from "../skg/graphology-adapter";
import type { CopEntityState } from "../stores/skg.svelte";
import { parseVehicleEntityId } from "../uxs";
import type { CortexHitSource, CortexSearchHit } from "./search-client";

export const CORTEX_QUERY_TIERS = [
  "hybrid",
  "lexical",
  "semantic",
  "neural",
] as const;
export type CortexQueryTier = (typeof CORTEX_QUERY_TIERS)[number];

export type AnalysisQueryStatus =
  "idle" | "running" | "completed" | "cancelled" | "error";

export type AnalysisSubmissionMode = "query" | "similar";

export type AnalysisRetrievalScope =
  | { readonly kind: "fleet"; readonly entityId: null }
  | { readonly kind: "entity"; readonly entityId: string }
  | { readonly kind: "hybrid-local-and-fleet"; readonly entityId: string };

export interface AnalysisResultProvenance {
  readonly reqSeq: number;
  readonly mode: AnalysisSubmissionMode;
  readonly submittedQuery: string;
  readonly submittedTier: CortexQueryTier;
  readonly retrievalScope: AnalysisRetrievalScope;
  readonly scopeEntityId: string | null;
  readonly submittedSelectionId: string | null;
  readonly settledSelectionId: string | null;
}

export interface AnalysisProvenanceSummary {
  readonly headline: string;
  readonly scopeLabel: string;
  readonly submittedSelectionLabel: string;
  readonly earlierSelectionMismatch: boolean;
  readonly draftQueryDiverged: boolean;
}

export function resolveAnalysisRetrievalScope(
  mode: AnalysisSubmissionMode,
  tier: CortexQueryTier,
  entityId: string | null,
): AnalysisRetrievalScope {
  if (mode === "similar") {
    return entityId
      ? { kind: "entity", entityId }
      : { kind: "fleet", entityId: null };
  }
  if (tier === "lexical") {
    return entityId
      ? { kind: "entity", entityId }
      : { kind: "fleet", entityId: null };
  }
  if (tier === "hybrid") {
    return entityId
      ? { kind: "hybrid-local-and-fleet", entityId }
      : { kind: "fleet", entityId: null };
  }
  // `semantic` (entitiesByPredicate) and `neural` (semantic vector search) run fleet-wide.
  return { kind: "fleet", entityId: null };
}

/** Reserved vehicle safety hex colors strictly prohibited on SKG topology (#7). */
export const RESERVED_SAFETY_HEX_COLORS = new Set([
  "#ef4444",
  "#f59e0b",
  "#22c55e",
]);

export interface GroundedTripleCitation {
  readonly subject: string;
  readonly predicate: string;
  readonly objectText: string;
  readonly source: string;
  readonly timestamp: string;
  readonly confidencePct: string;
  readonly kvRevision: number;
  readonly indexedRevision: number | null;
}

export interface RankedCortexEntityView {
  readonly entityId: string;
  readonly displayLabel: string;
  readonly entityKind: CopEntityState["kind"] | "path-neighbor";
  readonly scoreLabel: string;
  readonly verbBadge: CortexHitSource;
  readonly kvRevision: number | null;
  readonly indexedRevision: number | null;
}

export function isCortexQueryTier(value: unknown): value is CortexQueryTier {
  return (
    typeof value === "string" &&
    (CORTEX_QUERY_TIERS as readonly string[]).includes(value)
  );
}

export function formatAnalysisEntityRef(
  entityId: string | null | undefined,
  entityLookup: (id: string) => CopEntityState | null,
): string {
  if (!entityId) {
    return "Fleet-wide";
  }
  const parsed = parseVehicleEntityId(entityId);
  const entity = entityLookup(entityId);
  if (parsed) {
    return entity?.title && entity.title !== entityId
      ? `UXS #${parsed.sysid} (${entity.title})`
      : `UXS #${parsed.sysid}`;
  }
  return entity
    ? shortEntityLabel(entity.id, entity.title)
    : shortEntityLabel(entityId);
}

function formatRetrievalScopeLabel(
  scope: AnalysisRetrievalScope,
  entityLookup: (id: string) => CopEntityState | null,
): string {
  switch (scope.kind) {
    case "fleet":
      return "Fleet-wide";
    case "entity":
      return formatAnalysisEntityRef(scope.entityId, entityLookup);
    case "hybrid-local-and-fleet": {
      const localLabel = formatAnalysisEntityRef(scope.entityId, entityLookup);
      return `local lexical (${localLabel}) + fleet-wide neural`;
    }
  }
}

export function summarizeAnalysisProvenance(
  provenance: AnalysisResultProvenance | null,
  liveQueryText: string,
  liveTier: CortexQueryTier,
  currentSelectedEntityId: string | null,
  entityLookup: (id: string) => CopEntityState | null,
): AnalysisProvenanceSummary | null {
  if (!provenance) {
    return null;
  }
  const scopeLabel = formatRetrievalScopeLabel(
    provenance.retrievalScope,
    entityLookup,
  );
  const submittedSelectionLabel = provenance.submittedSelectionId
    ? formatAnalysisEntityRef(provenance.submittedSelectionId, entityLookup)
    : "none";
  const selectionNote = provenance.submittedSelectionId
    ? parseVehicleEntityId(provenance.submittedSelectionId)
      ? `vehicle selected at submission: ${submittedSelectionLabel}`
      : `entity selected at submission: ${submittedSelectionLabel}`
    : "selected at submission: none";

  const referenceSelectionId =
    provenance.submittedSelectionId ?? provenance.settledSelectionId;
  const earlierSelectionMismatch =
    provenance.submittedSelectionId !== null
      ? currentSelectedEntityId !== provenance.submittedSelectionId
      : currentSelectedEntityId !== referenceSelectionId;
  const draftQueryDiverged =
    provenance.mode === "query" &&
    (liveQueryText.trim() !== provenance.submittedQuery ||
      liveTier !== provenance.submittedTier);

  const base =
    provenance.mode === "query"
      ? `Results for "${provenance.submittedQuery}" (${provenance.submittedTier.toUpperCase()}) · retrieval scope: ${scopeLabel} · ${selectionNote}`
      : `2-Hop & Similar results · retrieval scope: ${scopeLabel} · ${selectionNote}`;

  const notes: string[] = [base];
  if (earlierSelectionMismatch) {
    const currentLabel = currentSelectedEntityId
      ? formatAnalysisEntityRef(currentSelectedEntityId, entityLookup)
      : "none";
    notes.push(`earlier selection (current: ${currentLabel})`);
  }
  if (draftQueryDiverged) {
    notes.push("query edited since run");
  }

  return {
    headline: notes.join(" · "),
    scopeLabel,
    submittedSelectionLabel,
    earlierSelectionMismatch,
    draftQueryDiverged,
  };
}

/**
 * Merges search hits from one or more Cortex query tiers with live skgStore
 * entity metadata in deterministic score-descending, ID-ascending order,
 * preserving the winning hit's source badge and indexedRevision together.
 */
export function buildRankedCortexHits(
  hits: readonly CortexSearchHit[],
  entityLookup: (id: string) => CopEntityState | null,
): readonly RankedCortexEntityView[] {
  const dedup = new Map<string, CortexSearchHit>();
  for (const hit of hits) {
    const existing = dedup.get(hit.entityId);
    if (!existing || hit.score > existing.score) {
      dedup.set(hit.entityId, hit);
    }
  }

  const ordered = Array.from(dedup.values()).sort((a, b) => {
    if (b.score !== a.score) {
      return b.score - a.score;
    }
    return a.entityId.localeCompare(b.entityId);
  });

  return ordered.map((hit) => {
    const entity = entityLookup(hit.entityId);
    const displayLabel =
      hit.label ??
      (entity
        ? shortEntityLabel(entity.id, entity.title)
        : shortEntityLabel(hit.entityId));
    const hitRevision =
      typeof hit.indexedRevision === "number" && hit.indexedRevision > 0
        ? hit.indexedRevision
        : null;
    return {
      entityId: hit.entityId,
      displayLabel,
      entityKind: entity?.kind ?? "path-neighbor",
      scoreLabel: hit.score > 0 ? hit.score.toFixed(3) : "match",
      verbBadge: hit.verb,
      kvRevision: entity?.kvRevision ?? null,
      indexedRevision: hitRevision,
    };
  });
}

/**
 * Projects matched SKG entities and their 2-hop pathSearch expansions into a
 * focused Causal Evidence Subgraph using strictly non-safety topology colors.
 * When activeMatchIds is empty and no pathSearch has run, falls back to the
 * selected entity (or ingested entities up to 24 nodes) so Cortex boots cleanly.
 */
export function buildCausalSubgraphModel(
  allEntities: Iterable<CopEntityState>,
  activeMatchIds: Iterable<string>,
  pathResults: readonly PathSearchResult[],
  selectedEntityId: string | null = null,
): SkgTopologyModel {
  const entityList = Array.from(allEntities);
  const scopeIds = new Set<string>(activeMatchIds);

  for (const result of pathResults) {
    for (const node of result.entities) {
      scopeIds.add(node.id);
    }
    for (const path of result.paths) {
      for (const edge of path) {
        scopeIds.add(edge.from);
        scopeIds.add(edge.to);
      }
    }
  }

  if (selectedEntityId) {
    scopeIds.add(selectedEntityId);
  }

  const scopedEntities =
    scopeIds.size > 0
      ? entityList.filter((entity) => scopeIds.has(entity.id))
      : entityList.slice(0, 24);

  return buildSkgTopologyModel(scopedEntities, pathResults);
}

/**
 * Formats grounding SkgTriple assertions with verifiable ENTITY_STATES
 * kvRevision and query indexedRevision provenance.
 */
export function buildGroundedTripleCitations(
  entity: CopEntityState | null | undefined,
  indexedRevision: number | null = null,
): readonly GroundedTripleCitation[] {
  if (!entity) {
    return [];
  }
  const rows = formatEntityTriples(entity);
  return rows.map((row) => ({
    subject: entity.id,
    predicate: row.predicate,
    objectText: row.objectText,
    source: row.source,
    timestamp: row.timestamp,
    confidencePct: row.confidencePct,
    kvRevision: entity.kvRevision,
    indexedRevision:
      indexedRevision !== null && indexedRevision > 0 ? indexedRevision : null,
  }));
}

/**
 * Verifies that every node and edge color in a topology model avoids reserved
 * UXS vehicle safety colors (#ef4444, #f59e0b, #22c55e).
 */
export function hasOnlyNonSafetyTopologyColors(
  model: SkgTopologyModel,
): boolean {
  for (const node of model.nodes) {
    if (RESERVED_SAFETY_HEX_COLORS.has(node.color.toLowerCase())) {
      return false;
    }
  }
  for (const edge of model.edges) {
    if (RESERVED_SAFETY_HEX_COLORS.has(edge.color.toLowerCase())) {
      return false;
    }
  }
  return true;
}
