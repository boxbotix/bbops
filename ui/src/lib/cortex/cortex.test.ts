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

    // Non-WebGL fallback mount hides .sigma-host and renders accessible fallback list.
    expect(
      hostEl
        .querySelector('[data-testid="cortex-sigma-host"]')
        ?.classList.contains("hidden"),
    ).toBe(true);
    expect(
      hostEl.querySelector('[data-testid="cortex-subgraph-fallback"]'),
    ).not.toBeNull();

    // Follow-up semantic (entitiesByPredicate) search with NO indexedRevision clears #21 from pill & citations!
    (
      mockGraphql.entitiesByPredicate as ReturnType<typeof vi.fn>
    ).mockResolvedValueOnce({
      data: { entities: [rover.id] },
      damagedEntities: 1,
    });
    (
      hostEl.querySelector(
        '[data-testid="cortex-tier-semantic"]',
      ) as HTMLButtonElement
    ).click();
    flushSync();

    input.value = "uxs.domain.kind";
    input.dispatchEvent(new Event("input", { bubbles: true }));
    flushSync();
    form.dispatchEvent(
      new Event("submit", { bubbles: true, cancelable: true }),
    );
    await vi.waitFor(() => {
      flushSync();
      expect(
        hostEl.querySelector('[data-testid="cortex-damaged-pill"]')
          ?.textContent,
      ).toContain("damaged:1");
    });

    expect(
      hostEl.querySelector('[data-testid="cortex-indexed-rev"]'),
    ).toBeNull();
    expect(
      hostEl.querySelector('[data-testid="cortex-hit-item"] .verb-badge')
        ?.textContent,
    ).toBe("entitiesByPredicate");
    const semanticCitationText =
      hostEl.querySelector('[data-testid="cortex-citation-list"]')
        ?.textContent ?? "";
    expect(semanticCitationText).toContain("kv#11");
    expect(semanticCitationText).not.toContain("idx#21");

    // 2-Hop & Similar refusal also leaves indexedRevision cleared rather than resurrecting stale #21.
    (
      hostEl.querySelector(
        '[data-testid="cortex-similar-btn"]',
      ) as HTMLButtonElement
    ).click();
    await vi.waitFor(() => {
      flushSync();
      expect(
        hostEl.querySelector('[data-testid="cortex-neural-status"]')
          ?.textContent,
      ).toContain("Similarity embeddings not enabled");
    });
    expect(
      hostEl.querySelector('[data-testid="cortex-indexed-rev"]'),
    ).toBeNull();
    expect(
      hostEl.querySelector('[data-testid="cortex-citation-list"]')?.textContent,
    ).not.toContain("idx#21");

    unmount(app);
    hostEl.remove();
  });

  // spec: cop-ui / Task-modal 4-workspace shell, invariant safety topbar, and Fleet Filmstrip
  it("initializes Sigma v3 against the bound host element on WebGL browsers, wires clickNode selection, and tears down on unmount", async () => {
    const skgStore = createSkgStore();
    const rover = buildVehicleStoredPayload(2, {
      "uxs.domain.kind": "rover",
      "dc.terms.title": "Vanguard Rover",
    });
    skgStore.ingestRawFrame(
      JSON.stringify({
        type: "put",
        bucket: "ENTITY_STATES",
        key: rover.id,
        revision: 4,
        payload: rover,
      }),
    );
    skgStore.ingestRawFrame(
      JSON.stringify({
        type: "synced",
        bucket: "ENTITY_STATES",
        revision: 4,
      }),
    );

    const viewportStore = createViewportStore({ skgStore });
    viewportStore.selectEntity(rover.id);

    const constructed: Array<{
      graphOrder: number;
      container: HTMLElement;
      refreshCount: number;
      killCount: number;
      clickHandlers: Array<(payload: { node: string }) => void>;
    }> = [];

    class FakeSigma {
      private readonly record: (typeof constructed)[number];

      constructor(
        graph: { order: number },
        container: HTMLElement,
        _settings: Record<string, unknown>,
      ) {
        this.record = {
          graphOrder: graph.order,
          container,
          refreshCount: 0,
          killCount: 0,
          clickHandlers: [],
        };
        constructed.push(this.record);
      }

      refresh(): void {
        this.record.refreshCount += 1;
      }

      kill(): void {
        this.record.killCount += 1;
      }

      on(
        event: "clickNode",
        handler: (payload: { node: string }) => void,
      ): void {
        if (event === "clickNode") {
          this.record.clickHandlers.push(handler);
        }
      }
    }

    const mockGraphql: GraphQLClient = {
      endpoint: "/graph-gateway/graphql",
      entity: vi.fn(),
      entitiesByPrefix: vi.fn(),
      entitiesByPredicate: vi.fn(),
      pathSearch: vi.fn().mockResolvedValue({
        data: {
          entities: [{ id: rover.id, depth: 0 }],
          paths: [],
          truncated: false,
        },
        damagedEntities: 0,
      }),
    };

    const hostEl = document.createElement("div");
    document.body.appendChild(hostEl);
    const app = mount(CortexWorkspace, {
      target: hostEl,
      props: {
        skgStore,
        viewportStore,
        graphqlClient: mockGraphql,
        probeWebGL: () => true,
        loadSigma: async () => FakeSigma as never,
      },
    });
    flushSync();

    await vi.waitFor(() => {
      flushSync();
      expect(constructed).toHaveLength(1);
    });

    const sigmaHost = hostEl.querySelector(
      '[data-testid="cortex-sigma-host"]',
    ) as HTMLDivElement;
    expect(sigmaHost).not.toBeNull();
    expect(sigmaHost.classList.contains("hidden")).toBe(false);
    expect(
      hostEl.querySelector('[data-testid="cortex-subgraph-fallback"]'),
    ).toBeNull();
    expect(constructed[0]?.container).toBe(sigmaHost);
    expect(constructed[0]?.graphOrder).toBe(1);
    expect(constructed[0]?.refreshCount).toBeGreaterThan(0);

    // Clicking a Sigma node selects the entity through CortexWorkspace.
    viewportStore.selectEntity(null);
    constructed[0]?.clickHandlers[0]?.({ node: rover.id });
    flushSync();
    expect(viewportStore.selectedEntityId).toBe(rover.id);

    unmount(app);
    hostEl.remove();
    expect(constructed[0]?.killCount).toBe(1);
  });

  // spec: cop-ui / Task-modal 4-workspace shell, invariant safety topbar, and Fleet Filmstrip
  it("guards deferred search replies and overlapping 2-hop expansions from overwriting newer selections", async () => {
    const skgStore = createSkgStore();
    const rover = buildVehicleStoredPayload(2, {
      "uxs.domain.kind": "rover",
      "dc.terms.title": "Vanguard Rover",
    });
    const quad = buildVehicleStoredPayload(5, {
      "uxs.domain.kind": "quadcopter",
      "dc.terms.title": "Falcon Quad",
    });
    const staleContactId = "boxbotix.local.tak.cop.vessel.stale1";
    const freshContactId = "boxbotix.local.tak.cop.vessel.fresh2";

    for (const [rev, id, payload] of [
      [1, rover.id, rover],
      [2, quad.id, quad],
      [
        3,
        staleContactId,
        {
          id: staleContactId,
          triples: [
            {
              subject: staleContactId,
              predicate: "dc.terms.title",
              object: "Stale Vessel",
              source: "ais",
              timestamp: "2026-11-24T12:00:00Z",
              confidence: 1,
            },
          ],
        },
      ],
      [
        4,
        freshContactId,
        {
          id: freshContactId,
          triples: [
            {
              subject: freshContactId,
              predicate: "dc.terms.title",
              object: "Fresh Vessel",
              source: "ais",
              timestamp: "2026-11-24T12:00:00Z",
              confidence: 1,
            },
          ],
        },
      ],
    ] as const) {
      skgStore.ingestRawFrame(
        JSON.stringify({
          type: "put",
          bucket: "ENTITY_STATES",
          key: id,
          revision: rev,
          payload,
        }),
      );
    }
    skgStore.ingestRawFrame(
      JSON.stringify({
        type: "synced",
        bucket: "ENTITY_STATES",
        revision: 4,
      }),
    );

    const createDeferred = <T>() => {
      let resolve!: (value: T) => void;
      const promise = new Promise<T>((res) => {
        resolve = res;
      });
      return { promise, resolve };
    };

    const deferredSearch = createDeferred<Response>();
    const deferredRoverExpand = createDeferred<{
      data: {
        entities: Array<{ id: string; depth: number }>;
        paths: Array<Array<{ from: string; predicate: string; to: string }>>;
        truncated: boolean;
      };
      damagedEntities: number;
    }>();
    const deferredQuadExpand = createDeferred<{
      data: {
        entities: Array<{ id: string; depth: number }>;
        paths: Array<Array<{ from: string; predicate: string; to: string }>>;
        truncated: boolean;
      };
      damagedEntities: number;
    }>();

    const expandSignals: Record<string, AbortSignal | undefined> = {};
    const mockGraphql: GraphQLClient = {
      endpoint: "/graph-gateway/graphql",
      entity: vi.fn(),
      entitiesByPrefix: vi.fn(),
      entitiesByPredicate: vi.fn(),
      pathSearch: vi.fn().mockImplementation((vars, opts) => {
        expandSignals[vars.startEntity] = opts?.signal;
        if (vars.startEntity === rover.id) {
          return deferredRoverExpand.promise;
        }
        return deferredQuadExpand.promise;
      }),
    };
    const mockSearch = createGraphSearchClient({
      fetchImpl: (async () => deferredSearch.promise) as typeof fetch,
    });

    const viewportStore = createViewportStore({ skgStore });
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

    // 1. Start a deferred lexical search while selection is null, then select quad BEFORE search resolves.
    (
      hostEl.querySelector(
        '[data-testid="cortex-tier-lexical"]',
      ) as HTMLButtonElement
    ).click();
    flushSync();

    const input = hostEl.querySelector(
      '[data-testid="cortex-query-input"]',
    ) as HTMLInputElement;
    input.value = "Fleet";
    input.dispatchEvent(new Event("input", { bubbles: true }));
    flushSync();

    const form = hostEl.querySelector(
      '[data-testid="cortex-query-bar"]',
    ) as HTMLFormElement;
    form.dispatchEvent(
      new Event("submit", { bubbles: true, cancelable: true }),
    );
    flushSync();

    // Operator selects Falcon Quad while lexical search is still in flight.
    viewportStore.selectEntity(quad.id);
    flushSync();

    deferredSearch.resolve(
      new Response(
        JSON.stringify({
          ok: true,
          verb: "globalSearch",
          result: {
            indexed_revision: 30,
            damaged_entities: 0,
            producer: "graph-query",
            data: {
              text: {
                matches: [
                  {
                    id: rover.id,
                    label: "Vanguard Rover",
                    score: 4.0,
                    entity: {},
                  },
                  {
                    id: quad.id,
                    label: "Falcon Quad",
                    score: 2.5,
                    entity: {},
                  },
                ],
              },
            },
          },
        }),
        { status: 200 },
      ),
    );

    await vi.waitFor(() => {
      flushSync();
      expect(
        hostEl.querySelectorAll('[data-testid="cortex-hit-item"]'),
      ).toHaveLength(2);
    });

    // Newer selection (Falcon Quad) was NOT overwritten by top hit (Vanguard Rover)!
    expect(viewportStore.selectedEntityId).toBe(quad.id);

    // 2. Click hit 1 (rover) then immediately click hit 2 (quad) with overlapping deferred 2-hop expansions.
    const hitButtons = hostEl.querySelectorAll(
      '[data-testid="cortex-hit-item"]',
    ) as NodeListOf<HTMLButtonElement>;
    hitButtons[0]!.click(); // selects rover, starts deferredRoverExpand
    flushSync();
    hitButtons[1]!.click(); // selects quad, aborts deferredRoverExpand, starts deferredQuadExpand
    flushSync();

    expect(viewportStore.selectedEntityId).toBe(quad.id);
    expect(expandSignals[rover.id]?.aborted).toBe(true);

    // Quad's expansion resolves first with freshContactId; stale rover expansion resolves second with staleContactId.
    deferredQuadExpand.resolve({
      data: {
        entities: [
          { id: quad.id, depth: 0 },
          { id: freshContactId, depth: 1 },
        ],
        paths: [
          [
            {
              from: quad.id,
              predicate: "cop.proximity.near",
              to: freshContactId,
            },
          ],
        ],
        truncated: false,
      },
      damagedEntities: 0,
    });
    await Promise.resolve();
    flushSync();

    deferredRoverExpand.resolve({
      data: {
        entities: [
          { id: rover.id, depth: 0 },
          { id: staleContactId, depth: 1 },
        ],
        paths: [
          [
            {
              from: rover.id,
              predicate: "cop.proximity.near",
              to: staleContactId,
            },
          ],
        ],
        truncated: false,
      },
      damagedEntities: 0,
    });
    await Promise.resolve();
    flushSync();

    const fallbackText =
      hostEl.querySelector('[data-testid="cortex-subgraph-fallback"]')
        ?.textContent ?? "";
    expect(fallbackText).toContain("Fresh Vessel");
    expect(fallbackText).not.toContain("Stale Vessel");

    unmount(app);
    hostEl.remove();
  });

  // spec: cop-ui / Task-modal 4-workspace shell, invariant safety topbar, and Fleet Filmstrip
  it("locks submitted tier across mid-flight tier clicks, clears prior hits/revisions on transport failures, and aligns duplicate-hit citation revision with the winning scored record", async () => {
    const skgStore = createSkgStore();
    const rover = buildVehicleStoredPayload(2, {
      "uxs.domain.kind": "rover",
      "dc.terms.title": "Vanguard Rover",
    });
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
        type: "synced",
        bucket: "ENTITY_STATES",
        revision: 11,
      }),
    );

    let resolveHeldLexical!: (res: Response) => void;
    const heldLexicalPromise = new Promise<Response>((res) => {
      resolveHeldLexical = res;
    });

    let fetchMode: "held-lexical" | "throw-error" | "duplicate-hybrid" =
      "held-lexical";

    const mockGraphql: GraphQLClient = {
      endpoint: "/graph-gateway/graphql",
      entity: vi.fn(),
      entitiesByPrefix: vi.fn(),
      entitiesByPredicate: vi.fn().mockResolvedValue({
        data: { entities: [rover.id] },
        damagedEntities: 0,
      }),
      pathSearch: vi.fn().mockResolvedValue({
        data: {
          entities: [{ id: rover.id, depth: 0 }],
          paths: [],
          truncated: false,
        },
        damagedEntities: 0,
      }),
    };

    const mockSearch = createGraphSearchClient({
      fetchImpl: (async (_input, init) => {
        if (fetchMode === "held-lexical") {
          return heldLexicalPromise;
        }
        if (fetchMode === "throw-error") {
          throw new TypeError("Failed to fetch");
        }
        const body = JSON.parse(String(init?.body ?? "{}"));
        if (body.verb === "globalSearch" || body.verb === "localSearch") {
          return new Response(
            JSON.stringify({
              ok: true,
              verb: body.verb,
              result: {
                indexed_revision: 21,
                damaged_entities: 0,
                producer: "graph-query",
                data:
                  body.verb === "globalSearch"
                    ? {
                        text: {
                          matches: [
                            {
                              id: rover.id,
                              label: "Vanguard Rover",
                              score: 1.4,
                              entity: {},
                            },
                          ],
                        },
                      }
                    : {
                        matches: [
                          {
                            id: rover.id,
                            label: "Vanguard Rover",
                            score: 1.4,
                            entity: {},
                          },
                        ],
                      },
              },
            }),
            { status: 200 },
          );
        }
        return new Response(
          JSON.stringify({
            ok: true,
            verb: "semantic",
            result: {
              indexed_revision: 34,
              damaged_entities: 0,
              producer: "graph-query",
              data: {
                results: [
                  {
                    entity_id: rover.id,
                    similarity: 3.85,
                  },
                ],
              },
            },
          }),
          { status: 200 },
        );
      }) as typeof fetch,
    });

    const viewportStore = createViewportStore({ skgStore });
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

    // 1. Submit Lexical, click Semantic while Lexical is held in flight, then release Lexical reply.
    // Single submission MUST NOT call entitiesByPredicate!
    (
      hostEl.querySelector(
        '[data-testid="cortex-tier-lexical"]',
      ) as HTMLButtonElement
    ).click();
    flushSync();

    const input = hostEl.querySelector(
      '[data-testid="cortex-query-input"]',
    ) as HTMLInputElement;
    input.value = "demo";
    input.dispatchEvent(new Event("input", { bubbles: true }));
    flushSync();

    const form = hostEl.querySelector(
      '[data-testid="cortex-query-bar"]',
    ) as HTMLFormElement;
    form.dispatchEvent(
      new Event("submit", { bubbles: true, cancelable: true }),
    );
    flushSync();

    // Click Semantic mid-flight BEFORE the lexical reply settles.
    (
      hostEl.querySelector(
        '[data-testid="cortex-tier-semantic"]',
      ) as HTMLButtonElement
    ).click();
    flushSync();

    resolveHeldLexical(
      new Response(
        JSON.stringify({
          ok: true,
          verb: "globalSearch",
          result: {
            indexed_revision: 25,
            damaged_entities: 0,
            producer: "graph-query",
            data: {
              text: {
                matches: [
                  {
                    id: rover.id,
                    label: "Vanguard Rover",
                    score: 2.2,
                    entity: {},
                  },
                ],
              },
            },
          },
        }),
        { status: 200 },
      ),
    );

    await vi.waitFor(() => {
      flushSync();
      expect(
        hostEl.querySelectorAll('[data-testid="cortex-hit-item"]'),
      ).toHaveLength(1);
    });
    expect(mockGraphql.entitiesByPredicate).not.toHaveBeenCalled();
    expect(
      hostEl.querySelector('[data-testid="cortex-hit-item"] .verb-badge')
        ?.textContent,
    ).toBe("globalSearch");
    expect(
      hostEl.querySelector('[data-testid="cortex-indexed-rev"]')?.textContent,
    ).toContain("idx rev #25");
    expect(
      hostEl.querySelector('[data-testid="cortex-citation-list"]')?.textContent,
    ).toContain("kv#11 · idx#25");

    // 2. Follow successful revision-bearing lookup with a thrown transport error on a new Lexical search:
    // old demo hit AND old idx#25 revision MUST be cleared!
    fetchMode = "throw-error";
    (
      hostEl.querySelector(
        '[data-testid="cortex-tier-lexical"]',
      ) as HTMLButtonElement
    ).click();
    flushSync();

    input.value = "unrelated search";
    input.dispatchEvent(new Event("input", { bubbles: true }));
    flushSync();
    form.dispatchEvent(
      new Event("submit", { bubbles: true, cancelable: true }),
    );

    await vi.waitFor(() => {
      flushSync();
      expect(
        hostEl.querySelector('[data-testid="cortex-error-banner"]')
          ?.textContent,
      ).toContain("Failed to fetch");
    });
    expect(
      hostEl.querySelectorAll('[data-testid="cortex-hit-item"]'),
    ).toHaveLength(0);
    expect(
      hostEl.querySelector('[data-testid="cortex-indexed-rev"]'),
    ).toBeNull();
    expect(
      hostEl.querySelector('[data-testid="cortex-citation-list"]')?.textContent,
    ).not.toContain("idx#25");

    // 3. Duplicate-ID Hybrid match (lexical score 1.4 @ idx#21 vs neural score 3.85 @ idx#34):
    // winning badge ("semantic"), score ("3.850"), AND citation revision ("idx#34") come from the same winning hit!
    fetchMode = "duplicate-hybrid";
    (
      hostEl.querySelector(
        '[data-testid="cortex-tier-hybrid"]',
      ) as HTMLButtonElement
    ).click();
    flushSync();

    input.value = "Vanguard";
    input.dispatchEvent(new Event("input", { bubbles: true }));
    flushSync();
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
      hostEl.querySelector('[data-testid="cortex-hit-item"] .verb-badge')
        ?.textContent,
    ).toBe("semantic");
    expect(
      hostEl.querySelector('[data-testid="cortex-hit-item"]')?.textContent,
    ).toContain("score 3.850");
    const dupCitationText =
      hostEl.querySelector('[data-testid="cortex-citation-list"]')
        ?.textContent ?? "";
    expect(dupCitationText).toContain("kv#11 · idx#34");
    expect(dupCitationText).not.toContain("idx#21");

    // 4. Transport error on 2-Hop & Similar ALSO clears prior hits and idx#34 revision!
    fetchMode = "throw-error";
    (
      hostEl.querySelector(
        '[data-testid="cortex-similar-btn"]',
      ) as HTMLButtonElement
    ).click();
    await vi.waitFor(() => {
      flushSync();
      expect(
        hostEl.querySelector('[data-testid="cortex-error-banner"]')
          ?.textContent,
      ).toContain("Failed to fetch");
    });
    expect(
      hostEl.querySelectorAll('[data-testid="cortex-hit-item"]'),
    ).toHaveLength(0);
    expect(
      hostEl.querySelector('[data-testid="cortex-indexed-rev"]'),
    ).toBeNull();
    expect(
      hostEl.querySelector('[data-testid="cortex-citation-list"]')?.textContent,
    ).not.toContain("idx#34");

    unmount(app);
    hostEl.remove();
  });
});
