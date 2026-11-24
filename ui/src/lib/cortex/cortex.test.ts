// SPDX-License-Identifier: MIT
// Copyright (c) 2026 boxbotix

import { flushSync, mount, unmount } from "svelte";
import { describe, expect, it, vi } from "vitest";
import CortexWorkspace from "../components/CortexWorkspace.svelte";
import type { GraphQLClient } from "../graphql";
import { createSkgStore } from "../stores/skg.svelte";
import { buildVehicleStoredPayload } from "../stores/test-helpers";
import { createViewportStore } from "../stores/viewport.svelte";
import { createGraphSearchClient } from "./search-client";
import {
  buildCausalSubgraphModel,
  buildGroundedTripleCitations,
  hasOnlyNonSafetyTopologyColors,
} from "./view-model";

describe("cortex/search-client.ts, view-model.ts & CortexWorkspace.svelte", () => {
  // spec: cop-ui / Task-modal 4-workspace shell, invariant safety topbar, and Fleet Filmstrip
  it("parses lexical globalSearch/localSearch matches and structured not_enabled neural refusals without throwing", async () => {
    const calls: Array<{ url: string; body: unknown }> = [];
    const client = createGraphSearchClient({
      fetchImpl: (async (input, init) => {
        const body = JSON.parse(String(init?.body ?? "{}"));
        calls.push({ url: String(input), body });

        if (body.verb === "globalSearch") {
          return new Response(
            JSON.stringify({
              ok: true,
              verb: "globalSearch",
              result: {
                indexed_revision: 17,
                damaged_entities: 0,
                producer: "graph-query",
                data: {
                  text: {
                    matches: [
                      {
                        id: "boxbotix.local.robotics.uxs.vehicle.2",
                        label: "Vanguard Rover",
                        score: 2.75,
                        word_rank: 1,
                        entity: {},
                      },
                    ],
                    answer: "Matched 1 entity: Vanguard Rover",
                  },
                },
              },
            }),
            { status: 200 },
          );
        }

        return new Response(
          JSON.stringify({
            ok: false,
            verb: body.verb,
            error: {
              class: "invalid",
              code: "not_enabled",
              message: "embedding search is disabled",
            },
          }),
          { status: 200 },
        );
      }) as typeof fetch,
    });

    const globalRes = await client.globalSearch({
      query: "Vanguard",
      limit: 10,
    });
    expect(globalRes.ok).toBe(true);
    expect(globalRes.indexedRevision).toBe(17);
    expect(globalRes.hits).toHaveLength(1);
    expect(globalRes.hits[0]?.entityId).toBe(
      "boxbotix.local.robotics.uxs.vehicle.2",
    );
    expect(globalRes.answer).toContain("Vanguard Rover");

    const semRes = await client.semantic({ query: "Vanguard", limit: 5 });
    expect(semRes.ok).toBe(false);
    expect(semRes.refusal?.notEnabled).toBe(true);
    expect(semRes.refusal?.code).toBe("not_enabled");
    expect(calls).toHaveLength(2);
  });

  // spec: cop-ui / Task-modal 4-workspace shell, invariant safety topbar, and Fleet Filmstrip
  it("projects matched entities + 2-hop pathSearch into a non-safety causal subgraph with kvRevision/indexedRevision citations and zero actuation controls", async () => {
    const skgStore = createSkgStore();
    const rover = buildVehicleStoredPayload(2, {
      "uxs.domain.kind": "rover",
      "dc.terms.title": "Vanguard Rover",
    });
    const uninvolved = buildVehicleStoredPayload(9, {
      "uxs.domain.kind": "quadcopter",
      "dc.terms.title": "Unrelated Quad",
    });
    const contactId = "boxbotix.local.tak.cop.vessel.harbor7";

    skgStore.ingestRawFrame(
      JSON.stringify({
        type: "put",
        bucket: "ENTITY_STATES",
        key: rover.id,
        revision: 11,
        payload: rover,
      }),
    );
    skgStore.ingestRawFrame(
      JSON.stringify({
        type: "put",
        bucket: "ENTITY_STATES",
        key: uninvolved.id,
        revision: 12,
        payload: uninvolved,
      }),
    );
    skgStore.ingestRawFrame(
      JSON.stringify({
        type: "put",
        bucket: "ENTITY_STATES",
        key: contactId,
        revision: 13,
        payload: {
          id: contactId,
          triples: [
            {
              subject: contactId,
              predicate: "dc.terms.title",
              object: "Harbor Escort",
              source: "ais",
              timestamp: "2026-11-24T12:00:00Z",
              confidence: 1,
            },
          ],
        },
      }),
    );
    skgStore.ingestRawFrame(
      JSON.stringify({
        type: "synced",
        bucket: "ENTITY_STATES",
        revision: 13,
      }),
    );

    const pathResult = {
      entities: [
        { id: rover.id, depth: 0 },
        { id: contactId, depth: 1 },
      ],
      paths: [
        [
          {
            from: rover.id,
            predicate: "cop.proximity.near",
            to: contactId,
          },
        ],
      ],
      truncated: false,
    };

    const subgraph = buildCausalSubgraphModel(
      skgStore.entities.values(),
      [rover.id],
      [pathResult],
    );

    // Unrelated entity #9 is excluded from the scoped causal subgraph.
    expect(subgraph.nodes.map((n) => n.id)).toEqual([rover.id, contactId]);
    expect(subgraph.edges).toHaveLength(1);
    expect(hasOnlyNonSafetyTopologyColors(subgraph)).toBe(true);

    const cites = buildGroundedTripleCitations(
      skgStore.getEntity(rover.id),
      21,
    );
    expect(cites.length).toBeGreaterThan(0);
    expect(cites[0]?.kvRevision).toBe(11);
    expect(cites[0]?.indexedRevision).toBe(21);

    // Mount CortexWorkspace and execute Hybrid query end-to-end.
    const viewportStore = createViewportStore({ skgStore });
    const mockGraphql: GraphQLClient = {
      endpoint: "/graph-gateway/graphql",
      entity: vi.fn(),
      entitiesByPrefix: vi.fn(),
      entitiesByPredicate: vi.fn(),
      pathSearch: vi.fn().mockResolvedValue({
        data: pathResult,
        damagedEntities: 0,
      }),
    };
    const mockSearch = createGraphSearchClient({
      fetchImpl: (async (_input, init) => {
        const body = JSON.parse(String(init?.body ?? "{}"));
        if (body.verb === "globalSearch") {
          return new Response(
            JSON.stringify({
              ok: true,
              verb: "globalSearch",
              result: {
                indexed_revision: 21,
                damaged_entities: 0,
                producer: "graph-query",
                data: {
                  text: {
                    matches: [
                      {
                        id: rover.id,
                        label: "Vanguard Rover",
                        score: 3.1,
                        entity: {},
                      },
                    ],
                    answer: "Grounded match: Vanguard Rover",
                  },
                },
              },
            }),
            { status: 200 },
          );
        }
        return new Response(
          JSON.stringify({
            ok: false,
            verb: body.verb,
            error: {
              class: "invalid",
              code: "not_enabled",
              message: "embeddings disabled",
            },
          }),
          { status: 200 },
        );
      }) as typeof fetch,
    });

    const hostEl = document.createElement("div");
    document.body.appendChild(hostEl);
    const app = mount(CortexWorkspace, {
      target: hostEl,
      props: {
        skgStore,
        viewportStore,
        graphqlClient: mockGraphql,
        searchClient: mockSearch,
      },
    });
    flushSync();

    const input = hostEl.querySelector(
      '[data-testid="cortex-query-input"]',
    ) as HTMLInputElement;
    input.value = "Vanguard";
    input.dispatchEvent(new Event("input", { bubbles: true }));
    flushSync();

    const form = hostEl.querySelector(
      '[data-testid="cortex-query-bar"]',
    ) as HTMLFormElement;
    form.dispatchEvent(
      new Event("submit", { bubbles: true, cancelable: true }),
    );
    await vi.waitFor(() => {
      flushSync();
      expect(
        hostEl.querySelectorAll('[data-testid="cortex-hit-item"]'),
      ).toHaveLength(1);
    });

    expect(
      hostEl.querySelector('[data-testid="cortex-neural-status"]')?.textContent,
    ).toContain("not_enabled");
    expect(
      hostEl.querySelector('[data-testid="cortex-indexed-rev"]')?.textContent,
    ).toContain("idx rev #21");
    expect(
      hostEl.querySelector('[data-testid="cortex-citation-list"]')?.textContent,
    ).toContain("kv#11 · idx#21");

    // Zero vehicle actuation or tuning affordances exist inside Cortex.
    expect(
      hostEl.querySelector('[data-testid="pilot-guarded-actions"]'),
    ).toBeNull();
    expect(
      hostEl.querySelector('[data-testid="bench-tuning-rack"]'),
    ).toBeNull();

    unmount(app);
    hostEl.remove();
  });
});
