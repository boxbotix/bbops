// SPDX-License-Identifier: MIT
// Copyright (c) 2026 boxbotix

import { flushSync, mount, unmount } from "svelte";
import { describe, expect, it, vi } from "vitest";
import CortexWorkspace from "../components/CortexWorkspace.svelte";
import type { GraphQLClient, PathSearchResult } from "../graphql";
import { createSkgStore } from "../stores/skg.svelte";
import { buildVehicleStoredPayload } from "../stores/test-helpers";
import { createViewportStore } from "../stores/viewport.svelte";
import { createGraphSearchClient } from "./search-client";
import { createAnalysisSessionStore } from "./session.svelte";
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

    let fetchMode:
      "held-lexical" | "throw-error" | "throw-string" | "duplicate-hybrid" =
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
        if (fetchMode === "throw-string") {
          throw "non-error rejection";
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

    expect(
      hostEl.querySelector(".tier-pills")?.getAttribute("aria-label"),
    ).toBe("Analysis SKG query tier");

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

    // Non-Error rejection uses updated "Analysis query failed" fallback copy.
    fetchMode = "throw-string";
    form.dispatchEvent(
      new Event("submit", { bubbles: true, cancelable: true }),
    );
    await vi.waitFor(() => {
      flushSync();
      expect(
        hostEl.querySelector('[data-testid="cortex-error-banner"]')
          ?.textContent,
      ).toBe("Analysis query failed");
    });

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

  // spec: cop-ui / Session-scoped Diagnostics parameter draft and Analysis investigation continuity across workspace switches
  it("enforces monotonic reqSeq stale-result refusal across out-of-order search/2-hop completions (even when transport ignores AbortSignal) and explicit Cancel (#40)", async () => {
    const skgStore = createSkgStore();
    const quad1 = buildVehicleStoredPayload(1, {
      "uxs.domain.kind": "quadcopter",
      "dc.terms.title": "Falcon 1",
    });
    const rover2 = buildVehicleStoredPayload(2, {
      "uxs.domain.kind": "rover",
      "dc.terms.title": "Vanguard 2",
    });
    const neighbor1Id = "boxbotix.local.tak.cop.vessel.alpha1";
    const neighbor2Id = "boxbotix.local.tak.cop.vessel.bravo2";

    for (const [rev, id, payload] of [
      [1, quad1.id, quad1],
      [2, rover2.id, rover2],
      [
        3,
        neighbor1Id,
        {
          id: neighbor1Id,
          triples: [
            {
              subject: neighbor1Id,
              predicate: "dc.terms.title",
              object: "Alpha Escort",
              source: "ais",
              timestamp: "2026-11-26T12:00:00Z",
              confidence: 1,
            },
          ],
        },
      ],
      [
        4,
        neighbor2Id,
        {
          id: neighbor2Id,
          triples: [
            {
              subject: neighbor2Id,
              predicate: "dc.terms.title",
              object: "Bravo Escort",
              source: "ais",
              timestamp: "2026-11-26T12:00:00Z",
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

    interface PendingSearch {
      query: string;
      entityId: string | undefined;
      signal: AbortSignal | undefined;
      resolve: (res: Response) => void;
    }
    const pendingSearches: PendingSearch[] = [];

    interface PendingExpand {
      startEntity: string;
      signal: AbortSignal | undefined;
      resolve: (value: {
        data: {
          entities: Array<{ id: string; depth: number }>;
          paths: Array<Array<{ from: string; predicate: string; to: string }>>;
          truncated: boolean;
        };
        damagedEntities: number;
      }) => void;
    }
    const pendingExpands: PendingExpand[] = [];

    // Transport deliberately ignores AbortSignal so only monotonic reqSeq prevents stale corruption!
    const mockSearch = createGraphSearchClient({
      fetchImpl: (async (_input, init) => {
        const body = JSON.parse(String(init?.body ?? "{}")) as {
          args?: { query?: string; entity_id?: string };
        };
        return new Promise<Response>((resolve) => {
          pendingSearches.push({
            query: body.args?.query ?? "",
            entityId: body.args?.entity_id,
            signal: init?.signal ?? undefined,
            resolve,
          });
        });
      }) as typeof fetch,
    });

    const mockGraphql: GraphQLClient = {
      endpoint: "/graph-gateway/graphql",
      entity: vi.fn(),
      entitiesByPrefix: vi.fn(),
      entitiesByPredicate: vi.fn(),
      pathSearch: vi.fn().mockImplementation(
        (vars, opts) =>
          new Promise((resolve) => {
            pendingExpands.push({
              startEntity: vars.startEntity,
              signal: opts?.signal,
              resolve,
            });
          }),
      ),
    };

    const viewportStore = createViewportStore({ skgStore });
    viewportStore.selectEntity(quad1.id);

    const analysisSession = createAnalysisSessionStore({
      skgStore,
      viewportStore,
      graphqlClient: mockGraphql,
      searchClient: mockSearch,
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
        analysisSession,
      },
    });
    flushSync();

    try {
      (
        hostEl.querySelector(
          '[data-testid="cortex-tier-lexical"]',
        ) as HTMLButtonElement
      ).click();
      flushSync();

      // 1. Launch Search #1 ("Falcon" scoped to UXS #1), then supersede with Search #2 ("Vanguard" scoped to UXS #2)
      //    and resolve Search #2 FIRST, Search #1 SECOND.
      const input = hostEl.querySelector(
        '[data-testid="cortex-query-input"]',
      ) as HTMLInputElement;
      const form = hostEl.querySelector(
        '[data-testid="cortex-query-bar"]',
      ) as HTMLFormElement;

      input.value = "Falcon";
      input.dispatchEvent(new Event("input", { bubbles: true }));
      flushSync();
      form.dispatchEvent(
        new Event("submit", { bubbles: true, cancelable: true }),
      );
      flushSync();

      expect(analysisSession.status).toBe("running");
      expect(pendingSearches).toHaveLength(1);
      expect(
        hostEl.querySelector('[data-testid="cortex-query-cancel"]'),
      ).not.toBeNull();

      viewportStore.selectEntity(rover2.id);
      flushSync();
      input.value = "Vanguard";
      input.dispatchEvent(new Event("input", { bubbles: true }));
      flushSync();
      form.dispatchEvent(
        new Event("submit", { bubbles: true, cancelable: true }),
      );
      flushSync();

      expect(pendingSearches).toHaveLength(2);
      expect(pendingSearches[0]?.signal?.aborted).toBe(true);
      expect(pendingSearches[1]?.signal?.aborted).toBe(false);

      // Resolve newer Search #2 first.
      pendingSearches[1]!.resolve(
        new Response(
          JSON.stringify({
            ok: true,
            verb: "localSearch",
            result: {
              indexed_revision: 52,
              damaged_entities: 0,
              producer: "graph-query",
              data: {
                matches: [
                  {
                    id: rover2.id,
                    label: "Vanguard 2",
                    score: 4.5,
                    entity: {},
                  },
                ],
              },
            },
          }),
          { status: 200 },
        ),
      );
      await vi.waitFor(() => {
        flushSync();
        expect(pendingExpands).toHaveLength(1);
      });
      // While the associated 2-hop expansion is pending, Search #2 stays running and Cancel stays available!
      expect(analysisSession.status).toBe("running");
      expect(analysisSession.inFlight).not.toBeNull();
      expect(
        hostEl.querySelector('[data-testid="cortex-query-cancel"]'),
      ).not.toBeNull();
      expect(pendingExpands[0]?.startEntity).toBe(rover2.id);

      // Now resolve stale Search #1 AFTER Search #2 hits arrived (even ignoring AbortSignal): MUST be refused!
      pendingSearches[0]!.resolve(
        new Response(
          JSON.stringify({
            ok: true,
            verb: "localSearch",
            result: {
              indexed_revision: 19,
              damaged_entities: 2,
              producer: "graph-query",
              data: {
                matches: [
                  {
                    id: quad1.id,
                    label: "Falcon 1",
                    score: 9.9,
                    entity: {},
                  },
                ],
              },
            },
          }),
          { status: 200 },
        ),
      );
      await new Promise((resolve) => setTimeout(resolve, 20));
      flushSync();

      expect(analysisSession.status).toBe("running");
      expect(analysisSession.indexedRevision).toBe(52);
      expect(analysisSession.damagedEntities).toBe(0);
      expect(analysisSession.rawHits.map((h) => h.entityId)).toEqual([
        rover2.id,
      ]);

      // Resolve rover2's 2-hop expansion and verify Search #2 now completes with provenance and Bravo Escort.
      pendingExpands[0]!.resolve({
        data: {
          entities: [
            { id: rover2.id, depth: 0 },
            { id: neighbor2Id, depth: 1 },
          ],
          paths: [
            [
              {
                from: rover2.id,
                predicate: "cop.proximity.near",
                to: neighbor2Id,
              },
            ],
          ],
          truncated: false,
        },
        damagedEntities: 0,
      });
      await vi.waitFor(() => {
        flushSync();
        expect(analysisSession.status).toBe("completed");
        expect(
          hostEl.querySelector('[data-testid="cortex-subgraph-fallback"]')
            ?.textContent,
        ).toContain("Bravo Escort");
      });
      expect(analysisSession.provenance?.submittedQuery).toBe("Vanguard");
      expect(analysisSession.provenance?.scopeEntityId).toBe(rover2.id);
      expect(analysisSession.provenance?.submittedSelectionId).toBe(rover2.id);

      // 2. Launch Search #3 ("SlowQuery") and click explicit Cancel before completion;
      //    then resolve Search #3 anyway and verify cancelled state refuses late payload.
      input.value = "SlowQuery";
      input.dispatchEvent(new Event("input", { bubbles: true }));
      flushSync();
      form.dispatchEvent(
        new Event("submit", { bubbles: true, cancelable: true }),
      );
      flushSync();

      expect(analysisSession.status).toBe("running");
      expect(pendingSearches).toHaveLength(3);

      const cancelBtn = hostEl.querySelector(
        '[data-testid="cortex-query-cancel"]',
      ) as HTMLButtonElement;
      expect(cancelBtn).not.toBeNull();
      cancelBtn.click();
      flushSync();

      expect(analysisSession.status).toBe("cancelled");
      expect(pendingSearches[2]?.signal?.aborted).toBe(true);
      expect(
        hostEl
          .querySelector('[data-testid="cortex-session-status"]')
          ?.getAttribute("data-status"),
      ).toBe("cancelled");
      expect(
        hostEl.querySelector('[data-testid="cortex-cancelled-banner"]')
          ?.textContent,
      ).toContain('Cancelled LEXICAL query "SlowQuery"');
      expect(analysisSession.queryText).toBe("SlowQuery");
      expect(analysisSession.activeTier).toBe("lexical");

      // Late resolution of cancelled Search #3 is refused!
      pendingSearches[2]!.resolve(
        new Response(
          JSON.stringify({
            ok: true,
            verb: "localSearch",
            result: {
              indexed_revision: 88,
              damaged_entities: 0,
              producer: "graph-query",
              data: {
                matches: [
                  {
                    id: quad1.id,
                    label: "Ghost Late Hit",
                    score: 7.0,
                    entity: {},
                  },
                ],
              },
            },
          }),
          { status: 200 },
        ),
      );
      await new Promise((resolve) => setTimeout(resolve, 20));
      flushSync();

      expect(analysisSession.status).toBe("cancelled");
      expect(analysisSession.rawHits).toHaveLength(0);
      expect(analysisSession.indexedRevision).toBeNull();
    } finally {
      unmount(app);
      analysisSession.dispose();
      hostEl.remove();
    }
  });

  // spec: cop-ui / Session-scoped Diagnostics parameter draft and Analysis investigation continuity across workspace switches
  it("preserves Analysis query, tier, results, pinned inspection, earlier-selection provenance, one-click selection sync, and removed-entity explanations across remounts (#40)", async () => {
    const skgStore = createSkgStore();
    const quad1 = buildVehicleStoredPayload(1, {
      "uxs.domain.kind": "quadcopter",
      "dc.terms.title": "Falcon 1",
    });
    const rover2 = buildVehicleStoredPayload(2, {
      "uxs.domain.kind": "rover",
      "dc.terms.title": "Vanguard 2",
    });
    const contactId = "boxbotix.local.tak.cop.vessel.harbor7";

    skgStore.ingestRawFrame(
      JSON.stringify({
        type: "put",
        bucket: "ENTITY_STATES",
        key: quad1.id,
        revision: 1,
        payload: quad1,
      }),
    );
    skgStore.ingestRawFrame(
      JSON.stringify({
        type: "put",
        bucket: "ENTITY_STATES",
        key: rover2.id,
        revision: 2,
        payload: rover2,
      }),
    );
    skgStore.ingestRawFrame(
      JSON.stringify({
        type: "put",
        bucket: "ENTITY_STATES",
        key: contactId,
        revision: 3,
        payload: {
          id: contactId,
          triples: [
            {
              subject: contactId,
              predicate: "dc.terms.title",
              object: "Harbor Escort",
              source: "ais",
              timestamp: "2026-11-26T12:00:00Z",
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
        revision: 3,
      }),
    );

    const mockGraphql: GraphQLClient = {
      endpoint: "/graph-gateway/graphql",
      entity: vi.fn(),
      entitiesByPrefix: vi.fn(),
      entitiesByPredicate: vi.fn(),
      pathSearch: vi.fn().mockImplementation(async (vars) => ({
        data: {
          entities: [
            { id: vars.startEntity, depth: 0 },
            ...(vars.startEntity === quad1.id
              ? [{ id: contactId, depth: 1 }]
              : []),
          ],
          paths:
            vars.startEntity === quad1.id
              ? [
                  [
                    {
                      from: quad1.id,
                      predicate: "cop.proximity.near",
                      to: contactId,
                    },
                  ],
                ]
              : [],
          truncated: false,
        },
        damagedEntities: 0,
      })),
    };

    const mockSearch = createGraphSearchClient({
      fetchImpl: (async (_input, init) => {
        const body = JSON.parse(String(init?.body ?? "{}")) as {
          verb: string;
        };
        if (body.verb === "localSearch") {
          return new Response(
            JSON.stringify({
              ok: true,
              verb: "localSearch",
              result: {
                indexed_revision: 64,
                damaged_entities: 0,
                producer: "graph-query",
                data: {
                  matches: [
                    {
                      id: quad1.id,
                      label: "Falcon 1",
                      score: 3.4,
                      entity: {},
                    },
                  ],
                  answer: "Matched Falcon 1 corridor context",
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
              message: "disabled",
            },
          }),
          { status: 200 },
        );
      }) as typeof fetch,
    });

    const viewportStore = createViewportStore({ skgStore });
    viewportStore.selectEntity(quad1.id);

    const analysisSession = createAnalysisSessionStore({
      skgStore,
      viewportStore,
      graphqlClient: mockGraphql,
      searchClient: mockSearch,
    });

    const hostEl = document.createElement("div");
    document.body.appendChild(hostEl);
    let app: ReturnType<typeof mount> | null = mount(CortexWorkspace, {
      target: hostEl,
      props: {
        skgStore,
        viewportStore,
        graphqlClient: mockGraphql,
        searchClient: mockSearch,
        analysisSession,
      },
    });
    flushSync();

    try {
      // 1. Select Lexical tier, submit "Falcon" scoped to UXS #1, and verify hits + 2-hop subgraph + citations.
      (
        hostEl.querySelector(
          '[data-testid="cortex-tier-lexical"]',
        ) as HTMLButtonElement
      ).click();
      flushSync();

      const input = hostEl.querySelector(
        '[data-testid="cortex-query-input"]',
      ) as HTMLInputElement;
      input.value = "Falcon";
      input.dispatchEvent(new Event("input", { bubbles: true }));
      flushSync();

      (
        hostEl.querySelector(
          '[data-testid="cortex-query-bar"]',
        ) as HTMLFormElement
      ).dispatchEvent(new Event("submit", { bubbles: true, cancelable: true }));

      await vi.waitFor(() => {
        flushSync();
        expect(analysisSession.status).toBe("completed");
        expect(analysisSession.pathResults).toHaveLength(1);
      });

      expect(
        hostEl
          .querySelector('[data-testid="cortex-result-provenance"]')
          ?.getAttribute("data-earlier-selection"),
      ).toBe("false");
      expect(
        hostEl.querySelector('[data-testid="cortex-inspection-divergence"]'),
      ).toBeNull();

      // 2. Unmount CortexWorkspace (simulate switching to Fleet), select UXS #2 externally, and remount CortexWorkspace.
      unmount(app);
      app = null;

      viewportStore.selectEntity(rover2.id);
      flushSync();

      app = mount(CortexWorkspace, {
        target: hostEl,
        props: {
          skgStore,
          viewportStore,
          graphqlClient: mockGraphql,
          searchClient: mockSearch,
          analysisSession,
        },
      });
      flushSync();

      // Query text, tier, ranked hits, 2-hop Harbor Escort subgraph, and pinned UXS #1 citations ALL survive!
      expect(
        (
          hostEl.querySelector(
            '[data-testid="cortex-query-input"]',
          ) as HTMLInputElement
        ).value,
      ).toBe("Falcon");
      expect(
        hostEl
          .querySelector('[data-testid="cortex-tier-lexical"]')
          ?.getAttribute("aria-pressed"),
      ).toBe("true");
      expect(
        hostEl.querySelectorAll('[data-testid="cortex-hit-item"]'),
      ).toHaveLength(1);
      expect(
        hostEl.querySelector('[data-testid="cortex-subgraph-fallback"]')
          ?.textContent,
      ).toContain("Harbor Escort");
      expect(analysisSession.inspectedEntityId).toBe(quad1.id);
      expect(
        hostEl.querySelector('[data-testid="cortex-citation-list"]')
          ?.textContent,
      ).toContain("Falcon 1");

      // Provenance banner & divergence strip distinguish UXS #1 results from current UXS #2 selection (OQ3 Option A).
      const provBanner = hostEl.querySelector(
        '[data-testid="cortex-result-provenance"]',
      );
      expect(provBanner?.getAttribute("data-earlier-selection")).toBe("true");
      expect(provBanner?.textContent).toContain("UXS #1");
      expect(provBanner?.textContent).toContain("earlier selection");
      expect(provBanner?.textContent).toContain("UXS #2");

      const divBanner = hostEl.querySelector(
        '[data-testid="cortex-inspection-divergence"]',
      );
      expect(divBanner?.textContent).toContain("Pinned inspection: UXS #1");
      expect(divBanner?.textContent).toContain("Current selection: UXS #2");

      // 3. Delete pinned UXS #1 from skgStore -> explicit unavailable notice renders instead of silent blank or wrong entity!
      skgStore.ingestRawFrame(
        JSON.stringify({
          type: "delete",
          bucket: "ENTITY_STATES",
          key: quad1.id,
          revision: 4,
        }),
      );
      flushSync();
      expect(
        hostEl.querySelector('[data-testid="cortex-entity-unavailable"]')
          ?.textContent,
      ).toContain(quad1.id);

      // 4. One-click sync button adopts current global selection (UXS #2) and resolves UXS #2 citations.
      (
        hostEl.querySelector(
          '[data-testid="cortex-sync-selection-btn"]',
        ) as HTMLButtonElement
      ).click();
      await vi.waitFor(() => {
        flushSync();
        expect(analysisSession.inspectedEntityId).toBe(rover2.id);
      });
      expect(
        hostEl.querySelector('[data-testid="cortex-inspection-divergence"]'),
      ).toBeNull();
      expect(
        hostEl.querySelector('[data-testid="cortex-entity-unavailable"]'),
      ).toBeNull();
      expect(
        hostEl.querySelector('[data-testid="cortex-citation-list"]')
          ?.textContent,
      ).toContain("Vanguard 2");
    } finally {
      if (app) {
        unmount(app);
      }
      analysisSession.dispose();
      hostEl.remove();
    }
  });

  // spec: cop-ui / Session-scoped Diagnostics parameter draft and Analysis investigation continuity across workspace switches
  it("keeps Cancel active while 2-hop pathSearch is pending after hits arrive (discarding late edges on Search and 2-Hop & Similar) and distinguishes fleet-wide vs vehicle-scoped retrieval provenance (#40)", async () => {
    const skgStore = createSkgStore();
    const quad1 = buildVehicleStoredPayload(1, {
      "uxs.domain.kind": "quadcopter",
      "dc.terms.title": "Falcon 1",
    });
    const rover2 = buildVehicleStoredPayload(2, {
      "uxs.domain.kind": "rover",
      "dc.terms.title": "Vanguard 2",
    });
    const lateContactId = "boxbotix.local.tak.cop.vessel.late9";
    const validContactId = "boxbotix.local.tak.cop.vessel.escort3";

    for (const [rev, id, payload] of [
      [1, quad1.id, quad1],
      [2, rover2.id, rover2],
      [
        3,
        lateContactId,
        {
          id: lateContactId,
          triples: [
            {
              subject: lateContactId,
              predicate: "dc.terms.title",
              object: "Discarded Late Vessel",
              source: "ais",
              timestamp: "2026-11-26T12:00:00Z",
              confidence: 1,
            },
          ],
        },
      ],
      [
        4,
        validContactId,
        {
          id: validContactId,
          triples: [
            {
              subject: validContactId,
              predicate: "dc.terms.title",
              object: "Verified Escort",
              source: "ais",
              timestamp: "2026-11-26T12:00:00Z",
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

    interface PendingExpand {
      startEntity: string;
      signal?: AbortSignal;
      resolve: (res: {
        data: PathSearchResult;
        damagedEntities: number;
      }) => void;
    }
    const heldExpands: PendingExpand[] = [];
    let holdPathSearch = true;
    const predicateCalls: Array<Record<string, unknown>> = [];
    const searchCalls: Array<Record<string, unknown>> = [];

    const mockGraphql: GraphQLClient = {
      endpoint: "/graph-gateway/graphql",
      entity: vi.fn(),
      entitiesByPrefix: vi.fn(),
      entitiesByPredicate: vi.fn().mockImplementation(async (vars) => {
        predicateCalls.push(vars as Record<string, unknown>);
        // Fleet-wide predicate query returns UXS #2 even when UXS #1 was selected!
        return {
          data: { entities: [rover2.id] },
          damagedEntities: 0,
        };
      }),
      pathSearch: vi.fn().mockImplementation((vars, opts) => {
        if (holdPathSearch) {
          return new Promise((resolve) => {
            heldExpands.push({
              startEntity: vars.startEntity,
              signal: opts?.signal,
              resolve,
            });
          });
        }
        return Promise.resolve({
          data: {
            entities: [
              { id: vars.startEntity, depth: 0 },
              { id: validContactId, depth: 1 },
            ],
            paths: [
              [
                {
                  from: vars.startEntity,
                  predicate: "cop.proximity.near",
                  to: validContactId,
                },
              ],
            ],
            truncated: false,
          },
          damagedEntities: 0,
        });
      }),
    };

    const mockSearch = createGraphSearchClient({
      fetchImpl: (async (_input, init) => {
        const body = JSON.parse(String(init?.body ?? "{}")) as Record<
          string,
          unknown
        >;
        searchCalls.push(body);
        if (body.verb === "similar") {
          return new Response(
            JSON.stringify({
              ok: true,
              verb: "similar",
              result: {
                indexed_revision: 41,
                damaged_entities: 0,
                producer: "graph-query",
                data: {
                  results: [{ entity_id: rover2.id, similarity: 0.91 }],
                },
              },
            }),
            { status: 200 },
          );
        }
        if (body.verb === "localSearch") {
          return new Response(
            JSON.stringify({
              ok: true,
              verb: "localSearch",
              result: {
                indexed_revision: 42,
                damaged_entities: 0,
                producer: "graph-query",
                data: {
                  matches: [
                    {
                      id: quad1.id,
                      label: "Falcon 1",
                      score: 3.8,
                      entity: {},
                    },
                  ],
                },
              },
            }),
            { status: 200 },
          );
        }
        if (body.verb === "semantic") {
          return new Response(
            JSON.stringify({
              ok: true,
              verb: "semantic",
              result: {
                indexed_revision: 43,
                damaged_entities: 0,
                producer: "graph-query",
                data: {
                  results: [{ entity_id: rover2.id, similarity: 2.2 }],
                },
              },
            }),
            { status: 200 },
          );
        }
        return new Response(
          JSON.stringify({
            ok: false,
            verb: String(body.verb ?? ""),
            error: {
              class: "invalid",
              code: "not_enabled",
              message: "disabled",
            },
          }),
          { status: 200 },
        );
      }) as typeof fetch,
    });

    const viewportStore = createViewportStore({ skgStore });
    viewportStore.selectEntity(quad1.id);

    const analysisSession = createAnalysisSessionStore({
      skgStore,
      viewportStore,
      graphqlClient: mockGraphql,
      searchClient: mockSearch,
    });

    const hostEl = document.createElement("div");
    document.body.appendChild(hostEl);
    let app = mount(CortexWorkspace, {
      target: hostEl,
      props: {
        skgStore,
        viewportStore,
        graphqlClient: mockGraphql,
        searchClient: mockSearch,
        analysisSession,
      },
    });
    flushSync();

    const remountAnalysis = () => {
      app = mount(CortexWorkspace, {
        target: hostEl,
        props: {
          skgStore,
          viewportStore,
          graphqlClient: mockGraphql,
          searchClient: mockSearch,
          analysisSession,
        },
      });
      flushSync();
    };

    try {
      let input = hostEl.querySelector(
        '[data-testid="cortex-query-input"]',
      ) as HTMLInputElement;
      let form = hostEl.querySelector(
        '[data-testid="cortex-query-bar"]',
      ) as HTMLFormElement;

      // 1A. Search SKG (SEMANTIC): hits arrive immediately while pathSearch is held open ->
      //     renders hit list + submission provenance banner immediately while keeping Cancel active!
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
        expect(heldExpands).toHaveLength(1);
      });
      expect(analysisSession.rawHits).toHaveLength(1);
      expect(analysisSession.status).toBe("running");
      expect(analysisSession.running).toBe(true);
      expect(
        hostEl.querySelectorAll('[data-testid="cortex-hit-item"]'),
      ).toHaveLength(1);
      expect(
        hostEl.querySelector('[data-testid="cortex-result-provenance"]')
          ?.textContent ?? "",
      ).toContain(
        'Results for "uxs.domain.kind" (SEMANTIC) · retrieval scope: Fleet-wide · vehicle selected at submission: UXS #1 (Falcon 1)',
      );
      expect(
        hostEl.querySelector('[data-testid="cortex-query-cancel"]'),
      ).not.toBeNull();

      // Switch away to Fleet, select UXS #2, return to Analysis, and edit draft to "other" BEFORE pathSearch resolves!
      unmount(app);
      viewportStore.selectEntity(rover2.id);
      remountAnalysis();
      input = hostEl.querySelector(
        '[data-testid="cortex-query-input"]',
      ) as HTMLInputElement;
      input.value = "other";
      input.dispatchEvent(new Event("input", { bubbles: true }));
      flushSync();

      // Provisional hits AND provenance banner (with earlier-selection + query-edited notices) render together while Cancel stays active!
      expect(analysisSession.status).toBe("running");
      expect(
        hostEl.querySelectorAll('[data-testid="cortex-hit-item"]'),
      ).toHaveLength(1);
      const pendingProvBanner = hostEl.querySelector(
        '[data-testid="cortex-result-provenance"]',
      );
      expect(pendingProvBanner).not.toBeNull();
      expect(pendingProvBanner?.getAttribute("data-earlier-selection")).toBe(
        "true",
      );
      const pendingProvText = pendingProvBanner?.textContent ?? "";
      expect(pendingProvText).toContain(
        'Results for "uxs.domain.kind" (SEMANTIC)',
      );
      expect(pendingProvText).toContain("retrieval scope: Fleet-wide");
      expect(pendingProvText).toContain(
        "vehicle selected at submission: UXS #1 (Falcon 1)",
      );
      expect(pendingProvText).toContain(
        "earlier selection (current: UXS #2 (Vanguard 2))",
      );
      expect(pendingProvText).toContain("query edited since run");
      expect(
        hostEl.querySelector('[data-testid="cortex-query-cancel"]'),
      ).not.toBeNull();

      // Cancel after hits arrived while 2-hop pathSearch is pending!
      expect(analysisSession.cancelSearch()).toBe(true);
      flushSync();
      expect(analysisSession.status).toBe("cancelled");
      expect(analysisSession.provenance).toBeNull();
      expect(
        hostEl.querySelector('[data-testid="cortex-result-provenance"]'),
      ).toBeNull();
      expect(heldExpands[0]?.signal?.aborted).toBe(true);
      expect(
        hostEl.querySelector('[data-testid="cortex-query-cancel"]'),
      ).toBeNull();

      // Resolve the held Search pathSearch anyway -> session stays cancelled and late edges are discarded!
      heldExpands[0]!.resolve({
        data: {
          entities: [
            { id: rover2.id, depth: 0 },
            { id: lateContactId, depth: 1 },
          ],
          paths: [
            [
              {
                from: rover2.id,
                predicate: "cop.proximity.near",
                to: lateContactId,
              },
            ],
          ],
          truncated: false,
        },
        damagedEntities: 0,
      });
      await new Promise((resolve) => setTimeout(resolve, 20));
      flushSync();

      expect(analysisSession.status).toBe("cancelled");
      expect(analysisSession.pathResults).toHaveLength(0);
      expect(
        hostEl.querySelector('[data-testid="cortex-subgraph-fallback"]')
          ?.textContent ?? "",
      ).not.toContain("Discarded Late Vessel");

      // 1B. Divergent-pin round trip on 2-Hop & Similar:
      //     Pin UXS #1 in Analysis, select UXS #2 while away, return, start Similar (targets #2),
      //     verify provisional provenance + hit list + Cancel while pathSearch is held open,
      //     then Cancel during path lookup -> pin restores to UXS #1 while global selection stays UXS #2!
      holdPathSearch = false;
      analysisSession.selectHit(quad1.id);
      await vi.waitFor(() => {
        flushSync();
        expect(analysisSession.inspectedEntityId).toBe(quad1.id);
        expect(analysisSession.pathResults).toHaveLength(1);
      });
      unmount(app);
      viewportStore.selectEntity(rover2.id);
      remountAnalysis();
      expect(analysisSession.inspectedEntityId).toBe(quad1.id);
      expect(viewportStore.selectedEntityId).toBe(rover2.id);

      holdPathSearch = true;
      (
        hostEl.querySelector(
          '[data-testid="cortex-similar-btn"]',
        ) as HTMLButtonElement
      ).click();
      await vi.waitFor(() => {
        flushSync();
        expect(heldExpands).toHaveLength(2);
      });
      expect(heldExpands[1]?.startEntity).toBe(rover2.id);
      expect(analysisSession.rawHits).toHaveLength(1);
      expect(analysisSession.status).toBe("running");
      expect(
        hostEl.querySelectorAll('[data-testid="cortex-hit-item"]'),
      ).toHaveLength(1);
      expect(
        hostEl.querySelector('[data-testid="cortex-result-provenance"]')
          ?.textContent ?? "",
      ).toContain(
        "2-Hop & Similar results · retrieval scope: UXS #2 (Vanguard 2) · vehicle selected at submission: UXS #2 (Vanguard 2)",
      );
      expect(
        hostEl.querySelector('[data-testid="cortex-query-cancel"]'),
      ).not.toBeNull();

      (
        hostEl.querySelector(
          '[data-testid="cortex-query-cancel"]',
        ) as HTMLButtonElement
      ).click();
      flushSync();
      expect(analysisSession.status).toBe("cancelled");
      expect(analysisSession.provenance).toBeNull();
      expect(analysisSession.inspectedEntityId).toBe(quad1.id);
      expect(viewportStore.selectedEntityId).toBe(rover2.id);
      expect(heldExpands[1]?.signal?.aborted).toBe(true);

      heldExpands[1]!.resolve({
        data: {
          entities: [
            { id: rover2.id, depth: 0 },
            { id: lateContactId, depth: 1 },
          ],
          paths: [
            [
              {
                from: rover2.id,
                predicate: "cop.proximity.near",
                to: lateContactId,
              },
            ],
          ],
          truncated: false,
        },
        damagedEntities: 0,
      });
      await new Promise((resolve) => setTimeout(resolve, 20));
      flushSync();

      expect(analysisSession.status).toBe("cancelled");
      expect(analysisSession.pathResults).toHaveLength(0);
      expect(analysisSession.inspectedEntityId).toBe(quad1.id);
      expect(viewportStore.selectedEntityId).toBe(rover2.id);
      expect(
        hostEl.querySelector('[data-testid="cortex-inspection-divergence"]')
          ?.textContent ?? "",
      ).toContain(
        "Pinned inspection: UXS #1 (Falcon 1) · Current selection: UXS #2 (Vanguard 2)",
      );
      expect(
        hostEl.querySelector('[data-testid="cortex-subgraph-fallback"]')
          ?.textContent ?? "",
      ).not.toContain("Discarded Late Vessel");

      // 2A. Fleet-wide SEMANTIC query submitted while UXS #1 is selected returning UXS #2:
      //     labels retrieval scope Fleet-wide (never UXS #1!) and preserves UXS #1 as vehicle selected at submission.
      holdPathSearch = false;
      viewportStore.selectEntity(quad1.id);
      flushSync();
      input = hostEl.querySelector(
        '[data-testid="cortex-query-input"]',
      ) as HTMLInputElement;
      form = hostEl.querySelector(
        '[data-testid="cortex-query-bar"]',
      ) as HTMLFormElement;
      input.value = "uxs.domain.kind";
      input.dispatchEvent(new Event("input", { bubbles: true }));
      flushSync();
      form.dispatchEvent(
        new Event("submit", { bubbles: true, cancelable: true }),
      );
      await vi.waitFor(() => {
        flushSync();
        expect(analysisSession.status).toBe("completed");
      });

      expect(predicateCalls.at(-1)).toEqual({ predicate: "uxs.domain.kind" });
      expect(analysisSession.provenance?.retrievalScope).toEqual({
        kind: "fleet",
        entityId: null,
      });
      expect(analysisSession.provenance?.scopeEntityId).toBeNull();
      expect(analysisSession.provenance?.submittedSelectionId).toBe(quad1.id);
      expect(analysisSession.provenance?.settledSelectionId).toBe(rover2.id);

      const semanticProvText =
        hostEl.querySelector('[data-testid="cortex-result-provenance"]')
          ?.textContent ?? "";
      expect(semanticProvText).toContain("retrieval scope: Fleet-wide");
      expect(semanticProvText).not.toContain("captured scope: UXS #1");
      expect(semanticProvText).not.toContain("retrieval scope: UXS #1");
      expect(semanticProvText).toContain(
        "vehicle selected at submission: UXS #1 (Falcon 1)",
      );
      expect(semanticProvText).toContain(
        "earlier selection (current: UXS #2 (Vanguard 2))",
      );

      // 2B. Vehicle-scoped LEXICAL search with UXS #1 selected sends entityId=quad1.id and labels retrieval scope UXS #1.
      viewportStore.selectEntity(quad1.id);
      flushSync();
      (
        hostEl.querySelector(
          '[data-testid="cortex-tier-lexical"]',
        ) as HTMLButtonElement
      ).click();
      flushSync();
      input.value = "Falcon";
      input.dispatchEvent(new Event("input", { bubbles: true }));
      flushSync();
      form.dispatchEvent(
        new Event("submit", { bubbles: true, cancelable: true }),
      );
      await vi.waitFor(() => {
        flushSync();
        expect(analysisSession.status).toBe("completed");
      });

      const lastLocalCall = searchCalls
        .filter((c) => c.verb === "localSearch")
        .at(-1);
      expect(
        (lastLocalCall?.params as Record<string, unknown> | undefined)
          ?.entity_id,
      ).toBe(quad1.id);
      expect(analysisSession.provenance?.retrievalScope).toEqual({
        kind: "entity",
        entityId: quad1.id,
      });
      expect(analysisSession.provenance?.scopeEntityId).toBe(quad1.id);
      const lexicalProvText =
        hostEl.querySelector('[data-testid="cortex-result-provenance"]')
          ?.textContent ?? "";
      expect(lexicalProvText).toContain("retrieval scope: UXS #1 (Falcon 1)");
      expect(lexicalProvText).toContain(
        "vehicle selected at submission: UXS #1 (Falcon 1)",
      );
      expect(
        hostEl
          .querySelector('[data-testid="cortex-result-provenance"]')
          ?.getAttribute("data-earlier-selection"),
      ).toBe("false");

      // 2C. HYBRID with UXS #1 selected combines local lexical on UXS #1 + fleet-wide neural.
      (
        hostEl.querySelector(
          '[data-testid="cortex-tier-hybrid"]',
        ) as HTMLButtonElement
      ).click();
      flushSync();
      form.dispatchEvent(
        new Event("submit", { bubbles: true, cancelable: true }),
      );
      await vi.waitFor(() => {
        flushSync();
        expect(analysisSession.status).toBe("completed");
      });
      expect(analysisSession.provenance?.retrievalScope).toEqual({
        kind: "hybrid-local-and-fleet",
        entityId: quad1.id,
      });
      const hybridProvText =
        hostEl.querySelector('[data-testid="cortex-result-provenance"]')
          ?.textContent ?? "";
      expect(hybridProvText).toContain(
        "retrieval scope: local lexical (UXS #1 (Falcon 1)) + fleet-wide neural",
      );
      expect(hybridProvText).toContain(
        "vehicle selected at submission: UXS #1 (Falcon 1)",
      );

      // 2D. NEURAL with UXS #1 selected returning UXS #2 is also fleet-wide retrieval scope.
      viewportStore.selectEntity(quad1.id);
      flushSync();
      (
        hostEl.querySelector(
          '[data-testid="cortex-tier-neural"]',
        ) as HTMLButtonElement
      ).click();
      flushSync();
      form.dispatchEvent(
        new Event("submit", { bubbles: true, cancelable: true }),
      );
      await vi.waitFor(() => {
        flushSync();
        expect(analysisSession.status).toBe("completed");
      });
      const lastSemanticVerbCall = searchCalls
        .filter((c) => c.verb === "semantic")
        .at(-1);
      expect(
        (lastSemanticVerbCall?.params as Record<string, unknown> | undefined)
          ?.scope,
      ).toBeUndefined();
      expect(analysisSession.provenance?.retrievalScope).toEqual({
        kind: "fleet",
        entityId: null,
      });
      expect(analysisSession.provenance?.scopeEntityId).toBeNull();
      const neuralProvText =
        hostEl.querySelector('[data-testid="cortex-result-provenance"]')
          ?.textContent ?? "";
      expect(neuralProvText).toContain("retrieval scope: Fleet-wide");
      expect(neuralProvText).toContain(
        "vehicle selected at submission: UXS #1 (Falcon 1)",
      );
      expect(neuralProvText).toContain(
        "earlier selection (current: UXS #2 (Vanguard 2))",
      );
    } finally {
      unmount(app);
      analysisSession.dispose();
      hostEl.remove();
    }
  });
});
