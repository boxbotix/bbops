// SPDX-License-Identifier: MIT
// Copyright (c) 2026 boxbotix

import { describe, expect, it } from "vitest";
import { createGraphQLClient, GraphQLClientError } from "./graphql";

interface CapturedCall {
  url: string;
  method: string;
  headers: Record<string, string>;
  body: {
    query: string;
    variables: Record<string, unknown>;
  };
  signal?: AbortSignal | null;
}

function countRootSelections(query: string): string[] {
  const firstBrace = query.indexOf("{");
  const lastBrace = query.lastIndexOf("}");
  const inner = query.slice(firstBrace + 1, lastBrace).trim();
  const rootMatch = inner.match(/^([a-zA-Z_]\w*)\s*\(/);
  if (!rootMatch) {
    return [];
  }
  // Ensure there is no sibling root field after the balanced parentheses/braces of the single root field.
  let depthParen = 0;
  let depthBrace = 0;
  let enteredBody = false;
  const roots: string[] = [rootMatch[1] as string];
  for (let i = rootMatch[0].length - 1; i < inner.length; i += 1) {
    const ch = inner[i];
    if (ch === "(" && depthBrace === 0) depthParen += 1;
    else if (ch === ")" && depthBrace === 0) depthParen -= 1;
    else if (ch === "{") {
      depthBrace += 1;
      enteredBody = true;
    } else if (ch === "}") {
      depthBrace -= 1;
      if (enteredBody && depthBrace === 0 && depthParen === 0) {
        const trailing = inner.slice(i + 1).trim();
        if (trailing.length > 0) {
          roots.push(trailing);
        }
        break;
      }
    }
  }
  return roots;
}

describe("graphql.ts — ADR-115 single-root-field GraphQL client", () => {
  // spec: cop-ui / Gapless ENTITY_STATES SKG store, external contact separation, and GraphQL client
  it("executes single-root-field entity, entitiesByPrefix, entitiesByPredicate, and pathSearch queries and surfaces damagedEntities", async () => {
    const calls: CapturedCall[] = [];
    const responses: Array<{ status: number; body: string }> = [
      {
        status: 200,
        body: JSON.stringify({
          data: {
            entity: {
              kvRevision: 19,
              entity: {
                id: "boxbotix.local.robotics.uxs.vehicle.1",
                triples: [
                  {
                    subject: "boxbotix.local.robotics.uxs.vehicle.1",
                    predicate: "uxs.domain.kind",
                    object: "quadcopter",
                    source: "mavlink_0xb1",
                    timestamp: "2026-11-24T11:00:00Z",
                    confidence: 1.0,
                  },
                ],
              },
            },
          },
        }),
      },
      {
        status: 200,
        body: JSON.stringify({
          data: {
            entitiesByPrefix: {
              entities: [
                {
                  id: "boxbotix.local.robotics.uxs.vehicle.1",
                  triples: [],
                },
              ],
              total: 3,
              next_cursor: "cursor-page-2",
            },
          },
        }),
      },
      {
        status: 200,
        body: JSON.stringify({
          data: {
            entitiesByPredicate: {
              entities: [
                "boxbotix.local.robotics.uxs.vehicle.1",
                "boxbotix.local.robotics.uxs.vehicle.2",
              ],
            },
          },
          extensions: {
            damagedEntities: 2,
          },
        }),
      },
      {
        status: 200,
        body: JSON.stringify({
          data: {
            pathSearch: {
              entities: [
                { id: "boxbotix.local.robotics.uxs.vehicle.1", depth: 0 },
                { id: "boxbotix.local.mission.plan.route.alpha", depth: 1 },
              ],
              paths: [
                [],
                [
                  {
                    from: "boxbotix.local.robotics.uxs.vehicle.1",
                    predicate: "uxs.mission.assigned",
                    to: "boxbotix.local.mission.plan.route.alpha",
                  },
                ],
              ],
              truncated: false,
            },
          },
          extensions: {
            damagedEntities: 1,
          },
        }),
      },
    ];

    const client = createGraphQLClient({
      endpoint: "/graph-gateway/graphql",
      fetchImpl: async (input, init) => {
        calls.push({
          url: String(input),
          method: init?.method ?? "GET",
          headers: (init?.headers as Record<string, string>) ?? {},
          body: JSON.parse(String(init?.body)),
          signal: init?.signal,
        });
        const next = responses.shift()!;
        return new Response(next.body, { status: next.status });
      },
    });

    const controller = new AbortController();
    const entityRes = await client.entity(
      "boxbotix.local.robotics.uxs.vehicle.1",
      { signal: controller.signal },
    );
    expect(entityRes.damagedEntities).toBe(0);
    expect(entityRes.data.kvRevision).toBe(19);
    expect(entityRes.data.entity.triples[0]?.object).toBe("quadcopter");
    expect(calls[0]?.signal).toBe(controller.signal);
    expect(countRootSelections(calls[0]!.body.query)).toEqual(["entity"]);

    const prefixRes = await client.entitiesByPrefix({
      prefix: "boxbotix.local.robotics.uxs.vehicle.",
      limit: 10,
      cursor: "cursor-page-1",
    });
    expect(prefixRes.data.total).toBe(3);
    expect(prefixRes.data.nextCursor).toBe("cursor-page-2");
    expect(countRootSelections(calls[1]!.body.query)).toEqual([
      "entitiesByPrefix",
    ]);

    const predRes = await client.entitiesByPredicate({
      predicate: "uxs.domain.kind",
      value: "quadcopter",
      limit: 25,
    });
    expect(predRes.data.entities).toEqual([
      "boxbotix.local.robotics.uxs.vehicle.1",
      "boxbotix.local.robotics.uxs.vehicle.2",
    ]);
    expect(predRes.damagedEntities).toBe(2);
    expect(countRootSelections(calls[2]!.body.query)).toEqual([
      "entitiesByPredicate",
    ]);

    const pathRes = await client.pathSearch({
      startEntity: "boxbotix.local.robotics.uxs.vehicle.1",
      direction: "outgoing",
      maxDepth: 2,
      maxNodes: 50,
      predicates: ["uxs.mission.assigned"],
    });
    expect(pathRes.damagedEntities).toBe(1);
    expect(pathRes.data.truncated).toBe(false);
    expect(pathRes.data.paths[1]?.[0]?.to).toBe(
      "boxbotix.local.mission.plan.route.alpha",
    );
    expect(countRootSelections(calls[3]!.body.query)).toEqual(["pathSearch"]);
    for (const c of calls) {
      expect(c.method).toBe("POST");
      expect(c.body.query).not.toContain("__schema");
      expect(c.body.query).not.toContain("__type");
    }
  });

  // spec: cop-ui / Gapless ENTITY_STATES SKG store, external contact separation, and GraphQL client
  it("surfaces structured GraphQLClientError on verb refusals, plain-text 503 shutdowns, HTTP 504 timeouts, and invalid arguments", async () => {
    const refusalClient = createGraphQLClient({
      fetchImpl: async () =>
        new Response(
          JSON.stringify({
            data: { entity: null },
            errors: [
              {
                message: "entity not found",
                path: ["entity"],
                extensions: { class: "client", code: "NOT_FOUND" },
              },
            ],
          }),
          { status: 200 },
        ),
    });

    await expect(
      refusalClient.entity("boxbotix.local.robotics.uxs.vehicle.99"),
    ).rejects.toMatchObject({
      name: "GraphQLClientError",
      status: 200,
      message: "entity not found",
      errors: [
        {
          message: "entity not found",
          extensions: { class: "client", code: "NOT_FOUND" },
        },
      ],
    });

    // SemEngine graph-gateway returns plain-text 503 while stopping.
    const stoppingClient = createGraphQLClient({
      fetchImpl: async () =>
        new Response("graph-gateway is stopping", { status: 503 }),
    });
    await expect(
      stoppingClient.entitiesByPrefix({ prefix: "" }),
    ).rejects.toEqual(
      expect.objectContaining({
        name: "GraphQLClientError",
        status: 503,
        message: "graph-gateway is stopping",
      }),
    );

    const timeoutClient = createGraphQLClient({
      fetchImpl: async () =>
        new Response(
          JSON.stringify({
            data: { pathSearch: null },
            errors: [{ message: "request timeout" }],
          }),
          { status: 504 },
        ),
    });
    try {
      await timeoutClient.pathSearch({
        startEntity: "boxbotix.local.robotics.uxs.vehicle.1",
        direction: "both",
        maxDepth: 3,
      });
      expect.unreachable("expected GraphQLClientError on 504");
    } catch (err) {
      expect(err).toBeInstanceOf(GraphQLClientError);
      expect((err as GraphQLClientError).status).toBe(504);
    }

    // Client-side bounds validation matching graph-gateway contract.
    await expect(refusalClient.entity("   ")).rejects.toThrow(RangeError);
    await expect(
      refusalClient.pathSearch({
        startEntity: "boxbotix.local.robotics.uxs.vehicle.1",
        direction: "outgoing",
        maxDepth: 0,
      }),
    ).rejects.toThrow(RangeError);
    await expect(
      refusalClient.pathSearch({
        startEntity: "boxbotix.local.robotics.uxs.vehicle.1",
        direction: "outgoing",
        maxDepth: 11,
      }),
    ).rejects.toThrow(RangeError);
    await expect(
      refusalClient.pathSearch({
        startEntity: "boxbotix.local.robotics.uxs.vehicle.1",
        direction: "outgoing",
        maxDepth: 2,
        maxNodes: 501,
      }),
    ).rejects.toThrow(RangeError);
  });
});
