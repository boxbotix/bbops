// SPDX-License-Identifier: MIT
// Copyright (c) 2026 boxbotix

import { isFiniteNumber, isRecord } from "../uxs";

export const DEFAULT_GRAPH_SEARCH_PATH = "/graph-gateway/search";

export const GRAPH_SEARCH_VERBS = [
  "globalSearch",
  "localSearch",
  "semantic",
  "similar",
] as const;
export type GraphSearchVerb = (typeof GRAPH_SEARCH_VERBS)[number];

export interface CortexSearchHit {
  readonly entityId: string;
  readonly label: string | null;
  readonly score: number;
  readonly verb: GraphSearchVerb;
  readonly wordRank: number | null;
  readonly vectorRank: number | null;
  readonly indexedRevision?: number | null;
}

export interface CortexSearchRefusal {
  readonly class: string;
  readonly code: string;
  readonly message: string;
  readonly notEnabled: boolean;
}

export interface CortexSearchOutcome {
  readonly ok: boolean;
  readonly verb: GraphSearchVerb;
  readonly hits: readonly CortexSearchHit[];
  readonly indexedRevision: number;
  readonly damagedEntities: number;
  readonly producer: string | null;
  readonly answer: string | null;
  readonly refusal: CortexSearchRefusal | null;
}

export interface GraphSearchClientOptions {
  endpoint?: string;
  fetchImpl?: typeof fetch;
}

export interface GlobalSearchParams {
  query: string;
  level?: number;
  limit?: number;
  minRevision?: number;
}

export interface LocalSearchParams {
  entityId: string;
  query: string;
  level?: number;
  limit?: number;
  minRevision?: number;
}

export interface SemanticSearchParams {
  query: string;
  limit?: number;
  scope?: readonly string[];
}

export interface SimilarSearchParams {
  entityId: string;
  limit?: number;
}

export interface GraphSearchClient {
  readonly endpoint: string;
  globalSearch(
    params: GlobalSearchParams,
    signal?: AbortSignal,
  ): Promise<CortexSearchOutcome>;
  localSearch(
    params: LocalSearchParams,
    signal?: AbortSignal,
  ): Promise<CortexSearchOutcome>;
  semantic(
    params: SemanticSearchParams,
    signal?: AbortSignal,
  ): Promise<CortexSearchOutcome>;
  similar(
    params: SimilarSearchParams,
    signal?: AbortSignal,
  ): Promise<CortexSearchOutcome>;
}

function parseTextMatches(
  rawMatches: unknown,
  verb: GraphSearchVerb,
  hitRevision: number | null,
): CortexSearchHit[] {
  if (!Array.isArray(rawMatches)) {
    return [];
  }
  const hits: CortexSearchHit[] = [];
  for (const item of rawMatches) {
    if (
      !isRecord(item) ||
      typeof item.id !== "string" ||
      item.id.length === 0
    ) {
      continue;
    }
    hits.push({
      entityId: item.id,
      label:
        typeof item.label === "string" && item.label.trim().length > 0
          ? item.label.trim()
          : null,
      score: isFiniteNumber(item.score) ? item.score : 0,
      verb,
      wordRank: isFiniteNumber(item.word_rank) ? item.word_rank : null,
      vectorRank: isFiniteNumber(item.vector_rank) ? item.vector_rank : null,
      indexedRevision: hitRevision,
    });
  }
  return hits;
}

function parseSimilarityHits(
  rawResults: unknown,
  verb: GraphSearchVerb,
  hitRevision: number | null,
): CortexSearchHit[] {
  if (!Array.isArray(rawResults)) {
    return [];
  }
  const hits: CortexSearchHit[] = [];
  for (const item of rawResults) {
    if (
      !isRecord(item) ||
      typeof item.entity_id !== "string" ||
      item.entity_id.length === 0
    ) {
      continue;
    }
    hits.push({
      entityId: item.entity_id,
      label: null,
      score: isFiniteNumber(item.similarity) ? item.similarity : 0,
      verb,
      wordRank: null,
      vectorRank: null,
      indexedRevision: hitRevision,
    });
  }
  return hits;
}

function parseSearchEnvelope(
  verb: GraphSearchVerb,
  rawEnvelope: unknown,
): CortexSearchOutcome {
  if (!isRecord(rawEnvelope)) {
    throw new Error("invalid /graph-gateway/search JSON response");
  }

  if (rawEnvelope.ok === false) {
    const rawErr = isRecord(rawEnvelope.error) ? rawEnvelope.error : {};
    const code = typeof rawErr.code === "string" ? rawErr.code : "unknown";
    const message =
      typeof rawErr.message === "string"
        ? rawErr.message
        : "graph query refused";
    const errClass =
      typeof rawErr.class === "string" ? rawErr.class : "invalid";
    return {
      ok: false,
      verb,
      hits: [],
      indexedRevision: 0,
      damagedEntities: 0,
      producer: null,
      answer: null,
      refusal: {
        class: errClass,
        code,
        message,
        notEnabled: code === "not_enabled",
      },
    };
  }

  const qr = isRecord(rawEnvelope.result) ? rawEnvelope.result : {};
  const indexedRevision = isFiniteNumber(qr.indexed_revision)
    ? qr.indexed_revision
    : 0;
  const hitRevision = indexedRevision > 0 ? indexedRevision : null;
  const damagedEntities = isFiniteNumber(qr.damaged_entities)
    ? qr.damaged_entities
    : 0;
  const producer = typeof qr.producer === "string" ? qr.producer : null;
  const data = isRecord(qr.data) ? qr.data : {};

  let hits: CortexSearchHit[] = [];
  let answer: string | null = null;

  if (verb === "globalSearch") {
    const text = isRecord(data.text) ? data.text : null;
    if (text) {
      hits = parseTextMatches(text.matches, verb, hitRevision);
      answer = typeof text.answer === "string" ? text.answer : null;
    }
  } else if (verb === "localSearch") {
    hits = parseTextMatches(data.matches, verb, hitRevision);
    answer = typeof data.answer === "string" ? data.answer : null;
  } else {
    hits = parseSimilarityHits(data.results, verb, hitRevision);
  }

  return {
    ok: true,
    verb,
    hits,
    indexedRevision,
    damagedEntities,
    producer,
    answer,
    refusal: null,
  };
}

export function createGraphSearchClient(
  options: GraphSearchClientOptions = {},
): GraphSearchClient {
  const endpoint = options.endpoint ?? DEFAULT_GRAPH_SEARCH_PATH;
  const fetchFn =
    options.fetchImpl ??
    ((input: RequestInfo | URL, init?: RequestInit) => fetch(input, init));

  async function execute(
    verb: GraphSearchVerb,
    params: Record<string, unknown>,
    signal?: AbortSignal,
  ): Promise<CortexSearchOutcome> {
    const response = await fetchFn(endpoint, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ verb, params }),
      signal,
    });

    const text = await response.text();
    let parsed: unknown = null;
    if (text.trim().length > 0) {
      try {
        parsed = JSON.parse(text);
      } catch {
        parsed = null;
      }
    }

    if (!response.ok) {
      if (isRecord(parsed) && parsed.ok === false && isRecord(parsed.error)) {
        return parseSearchEnvelope(verb, parsed);
      }
      throw new Error(
        `graph search HTTP ${response.status}: ${text.slice(0, 160) || response.statusText}`,
      );
    }

    return parseSearchEnvelope(verb, parsed);
  }

  return {
    endpoint,

    globalSearch(params, signal) {
      const wire: Record<string, unknown> = { query: params.query };
      if (params.level !== undefined) wire.level = params.level;
      if (params.limit !== undefined) wire.limit = params.limit;
      if (params.minRevision !== undefined) {
        wire.min_revision = params.minRevision;
      }
      return execute("globalSearch", wire, signal);
    },

    localSearch(params, signal) {
      const wire: Record<string, unknown> = {
        entity_id: params.entityId,
        query: params.query,
      };
      if (params.level !== undefined) wire.level = params.level;
      if (params.limit !== undefined) wire.limit = params.limit;
      if (params.minRevision !== undefined) {
        wire.min_revision = params.minRevision;
      }
      return execute("localSearch", wire, signal);
    },

    semantic(params, signal) {
      const wire: Record<string, unknown> = {
        query: params.query,
        limit: params.limit ?? 10,
      };
      if (params.scope && params.scope.length > 0) {
        wire.scope = [...params.scope];
      }
      return execute("semantic", wire, signal);
    },

    similar(params, signal) {
      return execute(
        "similar",
        {
          entity_id: params.entityId,
          limit: params.limit ?? 10,
        },
        signal,
      );
    },
  };
}
