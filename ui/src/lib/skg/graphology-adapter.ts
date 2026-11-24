// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Complete 360 Studio
// Copyright (c) 2026 boxbotix
//
// Provenance: Adapted from Graphology store synchronization and neighbor-seeded
// placement patterns in C360Studio/semstreams-ui src/lib/utils/graphology-adapter.ts
// @ 3814b3d59dab7136011429cad99a8148aeceb0cc (MIT), replacing status hues with
// bbops non-safety topology tokens (#7 reserved-safety-color guardrail).

import type { MultiDirectedGraph } from "graphology";
import type { PathSearchResult } from "../graphql";
import type { SafetyBadgeSeverity } from "../hud/view-model";
import type { CopEntityState, SkgTriple } from "../stores/skg.svelte";
import type { TransitionEvent } from "../stores/telemetry.svelte";
import {
  isFiniteNumber,
  isValidEntityIdString,
  parseSixPartEntityId,
} from "../uxs";

export type SkgNodeKind =
  "uxs-vehicle" | "external-contact" | "generic" | "path-neighbor";

/**
 * Non-safety SKG topology palette (#7 guardrail: red, amber, and green are
 * strictly reserved for UXS vehicle arming/authority/failsafe safety state).
 */
export const SKG_NODE_PALETTE: Readonly<Record<SkgNodeKind, string>> = {
  "uxs-vehicle": "#60a5fa",
  "external-contact": "#22d3ee",
  generic: "#a78bfa",
  "path-neighbor": "#818cf8",
};

export const SKG_EDGE_PALETTE = {
  uxs: "#38bdf8",
  cop: "#2dd4bf",
  default: "#64748b",
  dimmed: "#1e293b",
} as const;

export interface SkgTopologyNode {
  readonly id: string;
  readonly label: string;
  readonly nodeKind: SkgNodeKind;
  readonly color: string;
  readonly size: number;
  readonly nedHint: readonly [number, number] | null;
}

export interface SkgTopologyEdge {
  readonly key: string;
  readonly source: string;
  readonly target: string;
  readonly predicate: string;
  readonly color: string;
}

export interface SkgTopologyModel {
  readonly nodes: readonly SkgTopologyNode[];
  readonly edges: readonly SkgTopologyEdge[];
}

export interface FormattedTripleRow {
  readonly predicate: string;
  readonly objectText: string;
  readonly source: string;
  readonly timestamp: string;
  readonly confidencePct: string;
}

export interface FormattedTransitionRow {
  readonly key: string;
  readonly sysid: number;
  readonly entityId: string;
  readonly kind: TransitionEvent["kind"];
  readonly severity: SafetyBadgeSeverity;
  readonly headline: string;
  readonly detail: string;
  readonly timestamp: number;
}

const GOLDEN_ANGLE_RAD = Math.PI * (3 - Math.sqrt(5));

export function shortEntityLabel(
  entityId: string,
  title?: string | null,
): string {
  if (typeof title === "string" && title.trim().length > 0) {
    return title.trim();
  }
  const parts = parseSixPartEntityId(entityId);
  if (!parts) {
    return entityId;
  }
  return `${parts[4]}:${parts[5]}`;
}

export function resolvePredicateEdgeColor(predicate: string): string {
  if (predicate.startsWith("uxs.")) {
    return SKG_EDGE_PALETTE.uxs;
  }
  if (predicate.startsWith("cop.")) {
    return SKG_EDGE_PALETTE.cop;
  }
  return SKG_EDGE_PALETTE.default;
}

function resolveEntityNode(entity: CopEntityState): SkgTopologyNode {
  const nodeKind: SkgNodeKind = entity.kind;
  const label =
    entity.kind === "uxs-vehicle"
      ? `${entity.domain}#${entity.sysid}`
      : shortEntityLabel(entity.id, entity.title);
  const size =
    entity.kind === "uxs-vehicle"
      ? 11
      : entity.kind === "external-contact"
        ? 9
        : 8;
  const nedHint: [number, number] | null =
    entity.kind === "uxs-vehicle" &&
    entity.hasLocalPos !== false &&
    isFiniteNumber(entity.ned[0]) &&
    isFiniteNumber(entity.ned[1])
      ? [entity.ned[0], entity.ned[1]]
      : null;

  return {
    id: entity.id,
    label,
    nodeKind,
    color: SKG_NODE_PALETTE[nodeKind],
    size,
    nedHint,
  };
}

/**
 * Projects live skgStore entities and optional pathSearch neighborhood results
 * into a deduplicated, deterministically sorted topology model.
 */
export function buildSkgTopologyModel(
  entities: Iterable<CopEntityState>,
  expandedSearches: readonly PathSearchResult[] = [],
): SkgTopologyModel {
  const nodeMap = new Map<string, SkgTopologyNode>();
  const edgeMap = new Map<string, SkgTopologyEdge>();

  const ensureNeighborNode = (id: string): void => {
    if (!id || nodeMap.has(id)) {
      return;
    }
    nodeMap.set(id, {
      id,
      label: shortEntityLabel(id),
      nodeKind: "path-neighbor",
      color: SKG_NODE_PALETTE["path-neighbor"],
      size: 7,
      nedHint: null,
    });
  };

  const addDirectedEdge = (
    source: string,
    predicate: string,
    target: string,
  ): void => {
    if (!source || !target || !predicate || source === target) {
      return;
    }
    const key = `${source}|${predicate}|${target}`;
    if (edgeMap.has(key)) {
      return;
    }
    edgeMap.set(key, {
      key,
      source,
      target,
      predicate,
      color: resolvePredicateEdgeColor(predicate),
    });
  };

  const entityList = Array.from(entities).sort((a, b) =>
    a.id.localeCompare(b.id),
  );

  for (const entity of entityList) {
    nodeMap.set(entity.id, resolveEntityNode(entity));
  }

  for (const entity of entityList) {
    for (const triple of entity.triples) {
      if (
        typeof triple.object === "string" &&
        isValidEntityIdString(triple.object) &&
        triple.object !== entity.id
      ) {
        ensureNeighborNode(triple.object);
        addDirectedEdge(entity.id, triple.predicate, triple.object);
      }
    }
  }

  for (const result of expandedSearches) {
    for (const node of result.entities) {
      ensureNeighborNode(node.id);
    }
    for (const path of result.paths) {
      for (const edge of path) {
        ensureNeighborNode(edge.from);
        ensureNeighborNode(edge.to);
        addDirectedEdge(edge.from, edge.predicate, edge.to);
      }
    }
  }

  const nodes = Array.from(nodeMap.values()).sort((a, b) =>
    a.id.localeCompare(b.id),
  );
  const edges = Array.from(edgeMap.values()).sort((a, b) =>
    a.key.localeCompare(b.key),
  );
  return { nodes, edges };
}

function deterministicSeedPosition(
  index: number,
  node: SkgTopologyNode,
): { x: number; y: number } {
  if (node.nedHint) {
    // Map local East -> +x, North -> +y scaled gently around origin.
    return {
      x: node.nedHint[1] * 0.2 + Math.cos(index * GOLDEN_ANGLE_RAD) * 0.5,
      y: node.nedHint[0] * 0.2 + Math.sin(index * GOLDEN_ANGLE_RAD) * 0.5,
    };
  }
  const radius = 6 + Math.sqrt(index + 1) * 5;
  const angle = index * GOLDEN_ANGLE_RAD;
  return {
    x: Math.cos(angle) * radius,
    y: Math.sin(angle) * radius,
  };
}

function neighborSeededPosition(
  graph: MultiDirectedGraph,
  nodeId: string,
  edges: readonly SkgTopologyEdge[],
  fallbackIndex: number,
  node: SkgTopologyNode,
): { x: number; y: number } {
  let sumX = 0;
  let sumY = 0;
  let count = 0;
  for (const edge of edges) {
    const otherId =
      edge.source === nodeId
        ? edge.target
        : edge.target === nodeId
          ? edge.source
          : null;
    if (otherId && graph.hasNode(otherId)) {
      const attrs = graph.getNodeAttributes(otherId);
      if (isFiniteNumber(attrs.x) && isFiniteNumber(attrs.y)) {
        sumX += attrs.x;
        sumY += attrs.y;
        count += 1;
      }
    }
  }
  if (count > 0) {
    const angle = (fallbackIndex + 1) * GOLDEN_ANGLE_RAD;
    return {
      x: sumX / count + Math.cos(angle) * 4,
      y: sumY / count + Math.sin(angle) * 4,
    };
  }
  return deterministicSeedPosition(fallbackIndex, node);
}

/**
 * Incrementally synchronizes a SkgTopologyModel into a Graphology instance,
 * preserving existing node (x, y) positions across 4 Hz SKG refreshes and
 * seeding newly expanded nodes near already-placed neighbors.
 */
export function syncTopologyToGraph(
  graph: MultiDirectedGraph,
  model: SkgTopologyModel,
): { structureChanged: boolean } {
  let structureChanged = false;
  const nextNodeIds = new Set(model.nodes.map((n) => n.id));
  const nextEdgeKeys = new Set(model.edges.map((e) => e.key));

  for (const existingNode of graph.nodes()) {
    if (!nextNodeIds.has(existingNode)) {
      graph.dropNode(existingNode);
      structureChanged = true;
    }
  }

  model.nodes.forEach((node, index) => {
    if (graph.hasNode(node.id)) {
      graph.mergeNodeAttributes(node.id, {
        label: node.label,
        color: node.color,
        size: node.size,
        nodeKind: node.nodeKind,
      });
    } else {
      const pos = neighborSeededPosition(
        graph,
        node.id,
        model.edges,
        index,
        node,
      );
      graph.addNode(node.id, {
        x: pos.x,
        y: pos.y,
        label: node.label,
        color: node.color,
        size: node.size,
        nodeKind: node.nodeKind,
      });
      structureChanged = true;
    }
  });

  for (const existingEdge of graph.edges()) {
    if (!nextEdgeKeys.has(existingEdge)) {
      graph.dropEdge(existingEdge);
      structureChanged = true;
    }
  }

  for (const edge of model.edges) {
    if (!graph.hasNode(edge.source) || !graph.hasNode(edge.target)) {
      continue;
    }
    if (graph.hasEdge(edge.key)) {
      graph.mergeEdgeAttributes(edge.key, {
        label: edge.predicate,
        color: edge.color,
        predicate: edge.predicate,
      });
    } else {
      graph.addDirectedEdgeWithKey(edge.key, edge.source, edge.target, {
        label: edge.predicate,
        color: edge.color,
        size: 1.8,
        predicate: edge.predicate,
      });
      structureChanged = true;
    }
  }

  return { structureChanged };
}

function stringifyTripleObject(value: unknown): string {
  if (value === null || value === undefined) {
    return "null";
  }
  if (typeof value === "string") {
    return value;
  }
  if (typeof value === "number" || typeof value === "boolean") {
    return String(value);
  }
  try {
    return JSON.stringify(value);
  } catch {
    return String(value);
  }
}

export function formatEntityTriples(
  entity: CopEntityState | null | undefined,
): readonly FormattedTripleRow[] {
  if (!entity || entity.triples.length === 0) {
    return [];
  }
  const sorted: SkgTriple[] = [...entity.triples].sort((a, b) => {
    const byPred = a.predicate.localeCompare(b.predicate);
    if (byPred !== 0) {
      return byPred;
    }
    const byTs = a.timestamp.localeCompare(b.timestamp);
    if (byTs !== 0) {
      return byTs;
    }
    return a.source.localeCompare(b.source);
  });

  return sorted.map((triple) => ({
    predicate: triple.predicate,
    objectText: stringifyTripleObject(triple.object),
    source: triple.source || "unknown",
    timestamp: triple.timestamp || "unknown",
    confidencePct: `${Math.round(
      Math.max(0, Math.min(1, triple.confidence)) * 100,
    )}%`,
  }));
}

export function formatTransitionEvent(
  ev: TransitionEvent,
): FormattedTransitionRow {
  const key = `${ev.sysid}:${ev.kind}:${ev.seq}:${ev.timestamp}`;
  const reasonSuffix =
    ev.reasons.length > 0 ? ` (${ev.reasons.join(", ")})` : "";

  switch (ev.kind) {
    case "statustext": {
      const st = ev.status_text;
      const mavSeverity = st?.severity ?? 6;
      const severity: SafetyBadgeSeverity =
        mavSeverity <= 3 ? "error" : mavSeverity === 4 ? "warning" : "nominal";
      return {
        key,
        sysid: ev.sysid,
        entityId: ev.entity_id,
        kind: ev.kind,
        severity,
        headline: `SYS#${ev.sysid} STATUSTEXT`,
        detail: st?.text || "(empty statustext)",
        timestamp: ev.timestamp,
      };
    }
    case "failsafe_increment":
      return {
        key,
        sysid: ev.sysid,
        entityId: ev.entity_id,
        kind: ev.kind,
        severity: "error",
        headline: `SYS#${ev.sysid} FAILSAFE #${ev.failsafes}`,
        detail: `${ev.authority}${reasonSuffix}`,
        timestamp: ev.timestamp,
      };
    case "link_lost":
      return {
        key,
        sysid: ev.sysid,
        entityId: ev.entity_id,
        kind: ev.kind,
        severity: "error",
        headline: `SYS#${ev.sysid} LINK LOST`,
        detail: `${ev.link_tier}${reasonSuffix}`,
        timestamp: ev.timestamp,
      };
    case "link_restored":
      return {
        key,
        sysid: ev.sysid,
        entityId: ev.entity_id,
        kind: ev.kind,
        severity: "success",
        headline: `SYS#${ev.sysid} LINK RESTORED`,
        detail: `${ev.link_tier} · ${ev.authority}`,
        timestamp: ev.timestamp,
      };
    case "heartbeat_edge": {
      const severity: SafetyBadgeSeverity =
        ev.authority === "mcu-failsafe"
          ? "error"
          : ev.authority === "direct-crsf-override" ||
              ev.flags.teleop_deadman_clamped ||
              !ev.flags.clock_sync_locked
            ? "warning"
            : ev.armed
              ? "success"
              : "nominal";
      const transitionText = ev.prev_authority
        ? `${ev.prev_authority} -> ${ev.authority}`
        : ev.authority;
      return {
        key,
        sysid: ev.sysid,
        entityId: ev.entity_id,
        kind: ev.kind,
        severity,
        headline: `SYS#${ev.sysid} ${ev.armed ? "ARMED" : "DISARMED"}`,
        detail: `${transitionText}${reasonSuffix}`,
        timestamp: ev.timestamp,
      };
    }
  }
}
