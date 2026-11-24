// SPDX-License-Identifier: MIT
// Copyright (c) 2026 boxbotix

import type { SkgTriple } from "./stores/skg.svelte";
import { isFiniteNumber, isNonNegativeInt, isRecord } from "./uxs";

export const DEFAULT_GRAPHQL_ENDPOINT = "/graph-gateway/graphql";

export type PathSearchDirection = "outgoing" | "incoming" | "both";

export interface GraphQLErrorItem {
  message: string;
  path?: readonly (string | number)[];
  extensions?: {
    class?: string;
    code?: string;
    [key: string]: unknown;
  };
}

export interface GraphEntityRecord {
  id: string;
  triples: readonly SkgTriple[];
}

export interface EntityQueryResult {
  entity: GraphEntityRecord;
  kvRevision: number;
}

export interface EntitiesByPrefixResult {
  entities: readonly GraphEntityRecord[];
  total: number;
  nextCursor: string | null;
}

export interface EntitiesByPredicateResult {
  entities: readonly string[];
}

export interface PathSearchNode {
  id: string;
  depth: number;
}

export interface PathSearchEdge {
  from: string;
  predicate: string;
  to: string;
}

export interface PathSearchResult {
  entities: readonly PathSearchNode[];
  paths: readonly (readonly PathSearchEdge[])[];
  truncated: boolean;
}

export interface GraphQLResultEnvelope<T> {
  data: T;
  damagedEntities: number;
}

export interface GraphQLRequestOptions {
  signal?: AbortSignal;
}

export interface GraphQLClientOptions {
  endpoint?: string;
  fetchImpl?: typeof fetch;
}

export class GraphQLClientError extends Error {
  readonly status: number;
  readonly errors: readonly GraphQLErrorItem[];
  readonly damagedEntities: number;

  constructor(
    message: string,
    status: number,
    errors: readonly GraphQLErrorItem[] = [],
    damagedEntities = 0,
  ) {
    super(message);
    this.name = "GraphQLClientError";
    this.status = status;
    this.errors = errors;
    this.damagedEntities = damagedEntities;
  }
}

export interface GraphQLClient {
  readonly endpoint: string;
  entity(
    id: string,
    options?: GraphQLRequestOptions,
  ): Promise<GraphQLResultEnvelope<EntityQueryResult>>;
  entitiesByPrefix(
    params: { prefix: string; limit?: number; cursor?: string },
    options?: GraphQLRequestOptions,
  ): Promise<GraphQLResultEnvelope<EntitiesByPrefixResult>>;
  entitiesByPredicate(
    params: { predicate: string; value?: string; limit?: number },
    options?: GraphQLRequestOptions,
  ): Promise<GraphQLResultEnvelope<EntitiesByPredicateResult>>;
  pathSearch(
    params: {
      startEntity: string;
      direction: PathSearchDirection;
      maxDepth: number;
      maxNodes?: number;
      predicates?: readonly string[];
    },
    options?: GraphQLRequestOptions,
  ): Promise<GraphQLResultEnvelope<PathSearchResult>>;
}

const QUERY_ENTITY = `query GetEntity($id: String!) {
  entity(id: $id) {
    kvRevision
    entity {
      id
      triples {
        subject
        predicate
        object
        source
        timestamp
        confidence
      }
    }
  }
}`;

const QUERY_ENTITIES_BY_PREFIX = `query EntitiesByPrefix($prefix: String!, $limit: Int, $cursor: String) {
  entitiesByPrefix(prefix: $prefix, limit: $limit, cursor: $cursor) {
    entities {
      id
      triples {
        subject
        predicate
        object
        source
        timestamp
        confidence
      }
    }
    total
    next_cursor
  }
}`;

const QUERY_ENTITIES_BY_PREDICATE = `query EntitiesByPredicate($predicate: String!, $value: String, $limit: Int) {
  entitiesByPredicate(predicate: $predicate, value: $value, limit: $limit) {
    entities
  }
}`;

const QUERY_PATH_SEARCH = `query PathSearch($startEntity: String!, $direction: String!, $maxDepth: Int!, $maxNodes: Int, $predicates: [String]) {
  pathSearch(startEntity: $startEntity, direction: $direction, maxDepth: $maxDepth, maxNodes: $maxNodes, predicates: $predicates) {
    entities {
      id
      depth
    }
    paths {
      from
      predicate
      to
    }
    truncated
  }
}`;

function parseDamagedEntities(extensions: unknown): number {
  if (!isRecord(extensions)) {
    return 0;
  }
  const raw = extensions.damagedEntities;
  return isNonNegativeInt(raw) ? raw : 0;
}

function parseErrorItems(rawErrors: unknown): GraphQLErrorItem[] {
  if (!Array.isArray(rawErrors)) {
    return [];
  }
  const items: GraphQLErrorItem[] = [];
  for (const item of rawErrors) {
    if (!isRecord(item) || typeof item.message !== "string") {
      continue;
    }
    const parsed: GraphQLErrorItem = { message: item.message };
    if (
      Array.isArray(item.path) &&
      item.path.every((p) => typeof p === "string" || typeof p === "number")
    ) {
      parsed.path = item.path as (string | number)[];
    }
    if (isRecord(item.extensions)) {
      parsed.extensions = item.extensions as GraphQLErrorItem["extensions"];
    }
    items.push(parsed);
  }
  return items;
}

function parseTripleRecord(raw: unknown): SkgTriple | null {
  if (
    !isRecord(raw) ||
    typeof raw.subject !== "string" ||
    typeof raw.predicate !== "string"
  ) {
    return null;
  }
  return {
    subject: raw.subject,
    predicate: raw.predicate,
    object: raw.object,
    source: typeof raw.source === "string" ? raw.source : "",
    timestamp: typeof raw.timestamp === "string" ? raw.timestamp : "",
    confidence: isFiniteNumber(raw.confidence) ? raw.confidence : 1.0,
  };
}

function parseGraphEntityRecord(raw: unknown): GraphEntityRecord | null {
  if (
    !isRecord(raw) ||
    typeof raw.id !== "string" ||
    !Array.isArray(raw.triples)
  ) {
    return null;
  }
  const triples: SkgTriple[] = [];
  for (const t of raw.triples) {
    const parsed = parseTripleRecord(t);
    if (!parsed) {
      return null;
    }
    triples.push(parsed);
  }
  return { id: raw.id, triples };
}

export function createGraphQLClient(
  options: GraphQLClientOptions = {},
): GraphQLClient {
  const envEndpoint =
    typeof import.meta !== "undefined" && import.meta.env
      ? (import.meta.env.VITE_GRAPHQL_URL as string | undefined)
      : undefined;
  const endpoint =
    options.endpoint?.trim() || envEndpoint?.trim() || DEFAULT_GRAPHQL_ENDPOINT;
  const fetchImpl =
    options.fetchImpl ?? ((input, init) => globalThis.fetch(input, init));

  async function executeRootField(
    rootField: string,
    query: string,
    variables: Record<string, unknown>,
    reqOptions?: GraphQLRequestOptions,
  ): Promise<{ fieldValue: unknown; damagedEntities: number }> {
    const response = await fetchImpl(endpoint, {
      method: "POST",
      headers: {
        "Content-Type": "application/json",
        Accept: "application/json",
      },
      body: JSON.stringify({ query, variables }),
      signal: reqOptions?.signal,
    });

    const rawText = await response.text();
    let parsedBody: unknown;
    try {
      parsedBody = rawText.length > 0 ? JSON.parse(rawText) : null;
    } catch {
      throw new GraphQLClientError(
        rawText.trim() || `GraphQL HTTP ${response.status}`,
        response.status,
      );
    }

    if (!isRecord(parsedBody)) {
      throw new GraphQLClientError(
        `Invalid GraphQL response envelope (HTTP ${response.status})`,
        response.status,
      );
    }

    const damagedEntities = parseDamagedEntities(parsedBody.extensions);
    const errors = parseErrorItems(parsedBody.errors);

    if (!response.ok || errors.length > 0) {
      const primaryMsg =
        errors[0]?.message ?? `GraphQL HTTP ${response.status}`;
      throw new GraphQLClientError(
        primaryMsg,
        response.status,
        errors,
        damagedEntities,
      );
    }

    if (!isRecord(parsedBody.data) || !(rootField in parsedBody.data)) {
      throw new GraphQLClientError(
        `GraphQL response missing data.${rootField}`,
        response.status,
        errors,
        damagedEntities,
      );
    }

    const fieldValue = parsedBody.data[rootField];
    if (fieldValue === null || fieldValue === undefined) {
      throw new GraphQLClientError(
        `GraphQL data.${rootField} resolved to null`,
        response.status,
        errors,
        damagedEntities,
      );
    }

    return { fieldValue, damagedEntities };
  }

  return {
    get endpoint() {
      return endpoint;
    },

    async entity(id, reqOptions) {
      if (typeof id !== "string" || id.trim().length === 0) {
        throw new RangeError("entity id must be a non-empty string");
      }
      const { fieldValue, damagedEntities } = await executeRootField(
        "entity",
        QUERY_ENTITY,
        { id },
        reqOptions,
      );
      if (!isRecord(fieldValue) || !isNonNegativeInt(fieldValue.kvRevision)) {
        throw new GraphQLClientError("Invalid entity response payload", 200);
      }
      const record = parseGraphEntityRecord(fieldValue.entity);
      if (!record) {
        throw new GraphQLClientError("Invalid entity record payload", 200);
      }
      return {
        data: {
          entity: record,
          kvRevision: fieldValue.kvRevision,
        },
        damagedEntities,
      };
    },

    async entitiesByPrefix(params, reqOptions) {
      const variables: Record<string, unknown> = { prefix: params.prefix };
      if (params.limit !== undefined) {
        variables.limit = params.limit;
      }
      if (params.cursor !== undefined) {
        variables.cursor = params.cursor;
      }
      const { fieldValue, damagedEntities } = await executeRootField(
        "entitiesByPrefix",
        QUERY_ENTITIES_BY_PREFIX,
        variables,
        reqOptions,
      );
      if (
        !isRecord(fieldValue) ||
        !Array.isArray(fieldValue.entities) ||
        !isNonNegativeInt(fieldValue.total)
      ) {
        throw new GraphQLClientError(
          "Invalid entitiesByPrefix response payload",
          200,
        );
      }
      const entities: GraphEntityRecord[] = [];
      for (const item of fieldValue.entities) {
        const parsed = parseGraphEntityRecord(item);
        if (!parsed) {
          throw new GraphQLClientError(
            "Invalid entity in entitiesByPrefix response",
            200,
          );
        }
        entities.push(parsed);
      }
      const nextCursor =
        typeof fieldValue.next_cursor === "string" &&
        fieldValue.next_cursor.length > 0
          ? fieldValue.next_cursor
          : null;
      return {
        data: {
          entities,
          total: fieldValue.total,
          nextCursor,
        },
        damagedEntities,
      };
    },

    async entitiesByPredicate(params, reqOptions) {
      if (
        typeof params.predicate !== "string" ||
        params.predicate.trim().length === 0
      ) {
        throw new RangeError("predicate must be a non-empty string");
      }
      const variables: Record<string, unknown> = {
        predicate: params.predicate,
      };
      if (params.value !== undefined) {
        variables.value = params.value;
      }
      if (params.limit !== undefined) {
        variables.limit = params.limit;
      }
      const { fieldValue, damagedEntities } = await executeRootField(
        "entitiesByPredicate",
        QUERY_ENTITIES_BY_PREDICATE,
        variables,
        reqOptions,
      );
      if (
        !isRecord(fieldValue) ||
        !Array.isArray(fieldValue.entities) ||
        !fieldValue.entities.every((id) => typeof id === "string")
      ) {
        throw new GraphQLClientError(
          "Invalid entitiesByPredicate response payload",
          200,
        );
      }
      return {
        data: {
          entities: [...(fieldValue.entities as string[])],
        },
        damagedEntities,
      };
    },

    async pathSearch(params, reqOptions) {
      if (
        typeof params.startEntity !== "string" ||
        params.startEntity.trim().length === 0
      ) {
        throw new RangeError("startEntity must be a non-empty string");
      }
      if (
        !Number.isInteger(params.maxDepth) ||
        params.maxDepth < 1 ||
        params.maxDepth > 10
      ) {
        throw new RangeError("maxDepth must be an integer in [1, 10]");
      }
      if (
        params.maxNodes !== undefined &&
        (!Number.isInteger(params.maxNodes) ||
          params.maxNodes < 0 ||
          params.maxNodes > 500)
      ) {
        throw new RangeError("maxNodes must be an integer in [0, 500]");
      }
      if (params.predicates !== undefined) {
        for (const pred of params.predicates) {
          if (typeof pred !== "string" || pred.length === 0) {
            throw new RangeError(
              "predicates must contain only non-empty strings",
            );
          }
        }
      }

      const variables: Record<string, unknown> = {
        startEntity: params.startEntity,
        direction: params.direction,
        maxDepth: params.maxDepth,
      };
      if (params.maxNodes !== undefined) {
        variables.maxNodes = params.maxNodes;
      }
      if (params.predicates !== undefined) {
        variables.predicates = [...params.predicates];
      }

      const { fieldValue, damagedEntities } = await executeRootField(
        "pathSearch",
        QUERY_PATH_SEARCH,
        variables,
        reqOptions,
      );
      if (
        !isRecord(fieldValue) ||
        !Array.isArray(fieldValue.entities) ||
        !Array.isArray(fieldValue.paths) ||
        typeof fieldValue.truncated !== "boolean"
      ) {
        throw new GraphQLClientError(
          "Invalid pathSearch response payload",
          200,
        );
      }

      const nodes: PathSearchNode[] = [];
      for (const n of fieldValue.entities) {
        if (
          !isRecord(n) ||
          typeof n.id !== "string" ||
          !isNonNegativeInt(n.depth)
        ) {
          throw new GraphQLClientError("Invalid pathSearch node", 200);
        }
        nodes.push({ id: n.id, depth: n.depth });
      }

      const paths: PathSearchEdge[][] = [];
      for (const p of fieldValue.paths) {
        if (!Array.isArray(p)) {
          throw new GraphQLClientError("Invalid pathSearch edge list", 200);
        }
        const edges: PathSearchEdge[] = [];
        for (const edge of p) {
          if (
            !isRecord(edge) ||
            typeof edge.from !== "string" ||
            typeof edge.predicate !== "string" ||
            typeof edge.to !== "string"
          ) {
            throw new GraphQLClientError("Invalid pathSearch edge", 200);
          }
          edges.push({
            from: edge.from,
            predicate: edge.predicate,
            to: edge.to,
          });
        }
        paths.push(edges);
      }

      return {
        data: {
          entities: nodes,
          paths,
          truncated: fieldValue.truncated,
        },
        damagedEntities,
      };
    },
  };
}
