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
import type { CortexSearchHit } from "./search-client";

export const CORTEX_QUERY_TIERS = [
  "hybrid",
  "lexical",
  "semantic",
  "neural",
] as const;
export type CortexQueryTier = (typeof CORTEX_QUERY_TIERS)[number];

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
  readonly verbBadge: string;
  readonly kvRevision: number | null;
}

export function isCortexQueryTier(value: unknown): value is CortexQueryTier {
  return (
    typeof value === "string" &&
    (CORTEX_QUERY_TIERS as readonly string[]).includes(value)
  );
}

/**
 * Merges search hits from one or more Cortex query tiers with live skgStore
 * entity metadata in deterministic score-descending, ID-ascending order.
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
    return {
      entityId: hit.entityId,
      displayLabel,
      entityKind: entity?.kind ?? "path-neighbor",
      scoreLabel: hit.score > 0 ? hit.score.toFixed(3) : "match",
      verbBadge: hit.verb,
      kvRevision: entity?.kvRevision ?? null,
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
