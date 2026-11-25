// SPDX-License-Identifier: MIT
// Copyright (c) 2026 boxbotix

import { MultiDirectedGraph } from "graphology";
import { flushSync, mount, unmount } from "svelte";
import { describe, expect, it, vi } from "vitest";
import SkgPane from "../components/SkgPane.svelte";
import type { GraphQLClient, PathSearchResult } from "../graphql";
import {
  type CopEntityState,
  type VehicleEntityState,
  createSkgStore,
} from "../stores/skg.svelte";
import {
  type TransitionEvent,
  createTelemetryStore,
} from "../stores/telemetry.svelte";
import { createViewportStore } from "../stores/viewport.svelte";
import {
  SKG_EDGE_PALETTE,
  SKG_HOVER_PILL_FILL,
  SKG_HOVER_TEXT_HEX,
  SKG_NODE_PALETTE,
  buildSkgTopologyModel,
  computeClampedLabelPlacement,
  drawSkgNodeHover,
  drawSkgNodeLabel,
  formatEntityTriples,
  formatTransitionEvent,
  syncTopologyToGraph,
} from "./graphology-adapter";

function parseHexRgb(hex: string): [number, number, number] {
  const clean = hex.replace(/^#/, "");
  expect(clean).toHaveLength(6);
  return [
    Number.parseInt(clean.slice(0, 2), 16),
    Number.parseInt(clean.slice(2, 4), 16),
    Number.parseInt(clean.slice(4, 6), 16),
  ];
}

function isReservedSafetyHue(hex: string): boolean {
  const [r, g, b] = parseHexRgb(hex);
  const isRed = r > g + 40 && r > b + 40;
  const isAmber = r > 200 && g > 110 && b < 70;
  const isGreen = g > b + 25 && g > r + 40;
  return isRed || isAmber || isGreen;
}

function makeVehicle(sysid: number, peerEntityId?: string): VehicleEntityState {
  const id = `boxbotix.local.robotics.uxs.vehicle.${sysid}`;
  return {
    id,
    kind: "uxs-vehicle",
    title: `USV-${sysid}`,
    org: "boxbotix",
    platform: "local",
    sysid,
    domain: "surface-boat",
    authority: "companion-autonomous",
    companionMode: "autonomous-velocity",
    linkTier: "mesh-high-rate",
    armed: true,
    healthFlags: {
      clock_sync_locked: true,
      teleop_deadman_clamped: false,
      crsf_link_healthy: true,
      companion_trunk_healthy: true,
    },
    sysStatus: {
      failsafes: 0,
      crcErrors: 0,
      loopDtUs: 1000,
      maxLoopDtUs: 1200,
      rttUs: 350,
    },
    attitudeQuaternion: [1, 0, 0, 0],
    attitudeRates: [0, 0, 0],
    ned: [sysid * 10, sysid * 5, 0],
    velocityNed: [1.2, 0.4, 0],
    headingDeg: 45,
    location: { latitude: 30.2672, longitude: -97.7431, altitude: 150 },
    kvRevision: 1,
    updatedAt: "2026-11-24T12:00:00Z",
    poisoned: false,
    poisonError: null,
    indexingProfile: "full",
    source: "mavlink-ingest",
    observedAt: "2026-11-24T12:00:00Z",
    receivedAt: "2026-11-24T12:00:00Z",
    expiresAt: null,
    uncertaintyMeters: null,
    feedStatus: "live",
    triples: [
      {
        subject: id,
        predicate: "uxs.authority.state",
        object: "companion-autonomous",
        source: "mavlink-ingest",
        timestamp: "2026-11-24T12:00:01Z",
        confidence: 1,
      },
      ...(peerEntityId
        ? [
            {
              subject: id,
              predicate: "cop.mission.assigned_sector",
              object: peerEntityId,
              source: "mission-planner",
              timestamp: "2026-11-24T12:00:00Z",
              confidence: 0.95,
            },
          ]
        : []),
    ],
  };
}

describe("SKG topology Graphology adapter, triple inspector, and transition timeline", () => {
  // spec: cop-ui / Live SKG topology graph, triple inspector, and transition event timeline
  it("projects SKG entities and pathSearch edges with non-safety palette tokens and position-preserving sync", () => {
    for (const hex of [
      ...Object.values(SKG_NODE_PALETTE),
      ...Object.values(SKG_EDGE_PALETTE),
    ]) {
      expect(isReservedSafetyHue(hex)).toBe(false);
    }

    const sectorId = "boxbotix.local.cop.geo.sector.bravo";
    const contactEntity: CopEntityState = {
      id: "boxbotix.local.tak.cot.contact.c1",
      kind: "external-contact",
      title: "Track-C1",
      contactType: "a-u-S",
      location: { latitude: 30.27, longitude: -97.74, altitude: 0 },
      kvRevision: 1,
      updatedAt: null,
      poisoned: false,
      poisonError: null,
      indexingProfile: null,
      source: "tak-bridge",
      observedAt: null,
      receivedAt: null,
      expiresAt: null,
      uncertaintyMeters: 15,
      feedStatus: "live",
      triples: [],
    };
    const v1 = makeVehicle(1, sectorId);

    const pathExpansion: PathSearchResult = {
      entities: [
        { id: v1.id, depth: 0 },
        { id: sectorId, depth: 1 },
        { id: contactEntity.id, depth: 2 },
      ],
      paths: [
        [
          {
            from: v1.id,
            predicate: "cop.mission.assigned_sector",
            to: sectorId,
          },
          {
            from: sectorId,
            predicate: "cop.sector.contains_contact",
            to: contactEntity.id,
          },
        ],
      ],
      truncated: false,
    };

    const model = buildSkgTopologyModel([v1, contactEntity], [pathExpansion]);
    expect(model.nodes.map((n) => n.id)).toEqual([
      sectorId,
      v1.id,
      contactEntity.id,
    ]);
    expect(model.edges).toHaveLength(2);
    for (const node of model.nodes) {
      expect(isReservedSafetyHue(node.color)).toBe(false);
    }
    for (const edge of model.edges) {
      expect(isReservedSafetyHue(edge.color)).toBe(false);
    }

    // Sync into Graphology and verify existing (x, y) coordinates survive 4 Hz attribute refreshes.
    const graph = new MultiDirectedGraph();
    const firstSync = syncTopologyToGraph(graph, model);
    expect(firstSync.structureChanged).toBe(true);
    expect(graph.order).toBe(3);
    expect(graph.size).toBe(2);

    graph.setNodeAttribute(v1.id, "x", 42.5);
    graph.setNodeAttribute(v1.id, "y", -19.25);

    const secondSync = syncTopologyToGraph(graph, model);
    expect(secondSync.structureChanged).toBe(false);
    expect(graph.getNodeAttribute(v1.id, "x")).toBe(42.5);
    expect(graph.getNodeAttribute(v1.id, "y")).toBe(-19.25);

    // Expanding a brand-new neighbor seeds near v1's existing (42.5, -19.25) position.
    const relayId = "boxbotix.local.mesh.halow.relay.node9";
    const expandedModel = buildSkgTopologyModel(
      [v1, contactEntity],
      [
        pathExpansion,
        {
          entities: [{ id: relayId, depth: 1 }],
          paths: [
            [{ from: v1.id, predicate: "uxs.link.via_relay", to: relayId }],
          ],
          truncated: false,
        },
      ],
    );
    const thirdSync = syncTopologyToGraph(graph, expandedModel);
    expect(thirdSync.structureChanged).toBe(true);
    const relayX = graph.getNodeAttribute(relayId, "x") as number;
    const relayY = graph.getNodeAttribute(relayId, "y") as number;
    expect(Math.hypot(relayX - 42.5, relayY - -19.25)).toBeCloseTo(4, 5);
  });

  // spec: cop-ui / Live SKG topology graph, triple inspector, and transition event timeline
  it("formats sorted entity triples, Lane 3 transition events, and SkgPane jsdom node selection/2-hop expansion", async () => {
    const sectorId = "boxbotix.local.cop.geo.sector.bravo";
    const v1 = makeVehicle(1, sectorId);
    const rows = formatEntityTriples(v1);
    expect(rows.map((r) => r.predicate)).toEqual([
      "cop.mission.assigned_sector",
      "uxs.authority.state",
    ]);
    expect(rows[0]?.confidencePct).toBe("95%");
    expect(rows[1]?.objectText).toBe("companion-autonomous");

    const baseEv: TransitionEvent = {
      sysid: 1,
      entity_id: v1.id,
      domain: "surface-boat",
      seq: 12,
      msg_id: 253,
      kind: "statustext",
      reasons: ["statustext"],
      armed: true,
      authority: "direct-crsf-override",
      prev_authority: "companion-teleop",
      companion_mode: "teleop-velocity",
      link_tier: "mesh-high-rate",
      flags: {
        clock_sync_locked: true,
        teleop_deadman_clamped: true,
        crsf_link_healthy: true,
        companion_trunk_healthy: true,
      },
      failsafes: 1,
      status_text: {
        severity: 2,
        text: "EKF yaw reset",
        id: 0,
        chunk_seq: 0,
      },
      timestamp: 1700000000000,
    };

    const statusFmt = formatTransitionEvent(baseEv);
    expect(statusFmt.severity).toBe("error");
    expect(statusFmt.detail).toBe("EKF yaw reset");

    const edgeFmt = formatTransitionEvent({
      ...baseEv,
      kind: "heartbeat_edge",
      reasons: ["crsf_preempt"],
    });
    expect(edgeFmt.severity).toBe("warning");
    expect(edgeFmt.detail).toContain(
      "companion-teleop -> direct-crsf-override",
    );

    const failsafeFmt = formatTransitionEvent({
      ...baseEv,
      kind: "failsafe_increment",
    });
    expect(failsafeFmt.severity).toBe("error");
    expect(failsafeFmt.headline).toContain("FAILSAFE #1");

    // Mount SkgPane in jsdom and verify fallback node click + 2-hop GraphQL expansion.
    const skgStore = createSkgStore();
    skgStore.entities.set(v1.id, v1);
    const telemetryStore = createTelemetryStore();
    const viewportStore = createViewportStore({ skgStore });

    const neighborId = "boxbotix.local.mesh.halow.relay.node3";
    const pathSearchMock = vi.fn().mockResolvedValue({
      data: {
        entities: [
          { id: v1.id, depth: 0 },
          { id: neighborId, depth: 1 },
        ],
        paths: [
          [{ from: v1.id, predicate: "uxs.link.via_relay", to: neighborId }],
        ],
        truncated: false,
      },
      damagedEntities: 0,
    });
    const graphqlClient: GraphQLClient = {
      endpoint: "/graph-gateway/graphql",
      entity: vi.fn(),
      entitiesByPrefix: vi.fn(),
      entitiesByPredicate: vi.fn(),
      pathSearch: pathSearchMock,
    };

    const host = document.createElement("div");
    document.body.appendChild(host);
    const app = mount(SkgPane, {
      target: host,
      props: { skgStore, telemetryStore, viewportStore, graphqlClient },
    });
    flushSync();

    const fallback = host.querySelector(
      '[data-testid="skg-topology-fallback"]',
    );
    expect(fallback).not.toBeNull();
    const nodeButtons = fallback!.querySelectorAll("button.node-item");
    expect(nodeButtons.length).toBeGreaterThanOrEqual(2);

    const v1Btn = Array.from(nodeButtons).find((btn) =>
      btn.textContent?.includes("surface-boat#1"),
    ) as HTMLButtonElement | undefined;
    expect(v1Btn).toBeDefined();
    v1Btn!.click();
    flushSync();
    expect(viewportStore.selectedEntityId).toBe(v1.id);
    expect(
      host.querySelector('[data-testid="skg-triple-inspector"]')?.textContent,
    ).toContain("uxs.authority.state");

    const expandBtn = Array.from(host.querySelectorAll("button.ctrl-btn")).find(
      (btn) => btn.textContent?.includes("2-Hop"),
    ) as HTMLButtonElement | undefined;
    expect(expandBtn).toBeDefined();
    expandBtn!.click();
    await vi.waitFor(() => {
      expect(pathSearchMock).toHaveBeenCalledWith({
        startEntity: v1.id,
        direction: "both",
        maxDepth: 2,
        maxNodes: 50,
      });
      flushSync();
      expect(fallback!.textContent).toContain("relay:node3");
    });

    unmount(app);
    host.remove();
  });

  // spec: cop-ui / Live SKG topology graph, triple inspector, and transition event timeline
  it("supports multiple predicates between the same ordered (source, target) pair across MultiDirectedGraph and SkgPane syncs", () => {
    const sectorId = "boxbotix.local.cop.geo.sector.bravo";
    const v1FirstSync = makeVehicle(1, sectorId);
    const v1SecondSync: VehicleEntityState = {
      ...v1FirstSync,
      kvRevision: 2,
      triples: [
        ...v1FirstSync.triples,
        {
          subject: v1FirstSync.id,
          predicate: "cop.comms.relay_target",
          object: sectorId,
          source: "mesh-router",
          timestamp: "2026-11-24T12:00:02Z",
          confidence: 0.9,
        },
      ],
    };

    const graph = new MultiDirectedGraph();
    expect(graph.multi).toBe(true);
    expect(graph.type).toBe("directed");

    const pass1 = syncTopologyToGraph(
      graph,
      buildSkgTopologyModel([v1FirstSync], []),
    );
    expect(pass1.structureChanged).toBe(true);
    expect(graph.size).toBe(1);

    const pass2 = syncTopologyToGraph(
      graph,
      buildSkgTopologyModel([v1SecondSync], []),
    );
    expect(pass2.structureChanged).toBe(true);
    expect(graph.size).toBe(2);
    expect(graph.areNeighbors(v1FirstSync.id, sectorId)).toBe(true);
    expect(
      graph
        .edges(v1FirstSync.id, sectorId)
        .map((edgeKey) => graph.getEdgeAttribute(edgeKey, "label"))
        .sort(),
    ).toEqual(["cop.comms.relay_target", "cop.mission.assigned_sector"]);

    // Also exercise mounted SkgPane across 2 live skgStore updates + node selection.
    const skgStore = createSkgStore();
    skgStore.entities.set(v1FirstSync.id, v1FirstSync);
    const telemetryStore = createTelemetryStore();
    const viewportStore = createViewportStore({ skgStore });
    const graphqlClient: GraphQLClient = {
      endpoint: "/graph-gateway/graphql",
      entity: vi.fn(),
      entitiesByPrefix: vi.fn(),
      entitiesByPredicate: vi.fn(),
      pathSearch: vi.fn(),
    };

    const host = document.createElement("div");
    document.body.appendChild(host);
    const app = mount(SkgPane, {
      target: host,
      props: { skgStore, telemetryStore, viewportStore, graphqlClient },
    });
    flushSync();
    expect(host.textContent).toContain("2N · 1E");

    skgStore.entities.set(v1SecondSync.id, v1SecondSync);
    viewportStore.selectEntity(v1SecondSync.id);
    flushSync();
    expect(host.textContent).toContain("2N · 2E");

    unmount(app);
    host.remove();
  });

  // spec: cop-ui / Live SKG topology graph, triple inspector, and transition event timeline
  it("renders collision-free Triple Inspector rows with Timestamp column when multiple assertions share predicate and timestamp", () => {
    const base = makeVehicle(2);
    const multiSourceVehicle: VehicleEntityState = {
      ...base,
      triples: [
        {
          subject: base.id,
          predicate: "cop.intel.observation",
          object: "wake-plume-bearing-085",
          source: "sensor-alpha",
          timestamp: "2026-11-24T12:00:00Z",
          confidence: 0.9,
        },
        {
          subject: base.id,
          predicate: "cop.intel.observation",
          object: "acoustic-contact-bearing-087",
          source: "sensor-bravo",
          timestamp: "2026-11-24T12:00:00Z",
          confidence: 0.85,
        },
        {
          subject: base.id,
          predicate: "cop.intel.note",
          object: "first unclocked operator tag",
          source: "operator-1",
          timestamp: "",
          confidence: 1,
        },
        {
          subject: base.id,
          predicate: "cop.intel.note",
          object: "second unclocked operator tag",
          source: "operator-2",
          timestamp: "",
          confidence: 1,
        },
      ],
    };

    const skgStore = createSkgStore();
    skgStore.entities.set(multiSourceVehicle.id, multiSourceVehicle);
    const telemetryStore = createTelemetryStore();
    const viewportStore = createViewportStore({ skgStore });
    viewportStore.selectEntity(multiSourceVehicle.id);
    const graphqlClient: GraphQLClient = {
      endpoint: "/graph-gateway/graphql",
      entity: vi.fn(),
      entitiesByPrefix: vi.fn(),
      entitiesByPredicate: vi.fn(),
      pathSearch: vi.fn(),
    };

    const host = document.createElement("div");
    document.body.appendChild(host);
    const app = mount(SkgPane, {
      target: host,
      props: { skgStore, telemetryStore, viewportStore, graphqlClient },
    });
    flushSync();

    const inspector = host.querySelector(
      '[data-testid="skg-triple-inspector"]',
    );
    expect(inspector).not.toBeNull();
    const headers = Array.from(inspector!.querySelectorAll("thead th")).map(
      (th) => th.textContent?.trim(),
    );
    expect(headers).toEqual(["Predicate", "Value", "Source", "Timestamp"]);

    const bodyRows = Array.from(inspector!.querySelectorAll("tbody tr"));
    expect(bodyRows).toHaveLength(4);
    const tableText = inspector!.querySelector("tbody")?.textContent ?? "";
    expect(tableText).toContain("sensor-alpha");
    expect(tableText).toContain("sensor-bravo");
    expect(tableText).toContain("2026-11-24T12:00:00Z");
    expect(tableText).toContain("operator-1");
    expect(tableText).toContain("operator-2");
    expect(tableText).toContain("unknown");

    unmount(app);
    host.remove();
  });

  // spec: cop-ui / Full-stack headless browser COP end-to-end verification and visual legibility
  it("renders high-contrast dark SKG hover pills without #ffffff fills and clamps or truncates wide node labels inside the stage", () => {
    expect(SKG_HOVER_PILL_FILL).toBe("#0f172a");
    expect(SKG_HOVER_TEXT_HEX).toBe("#f8fafc");
    expect(isReservedSafetyHue(SKG_HOVER_PILL_FILL)).toBe(false);

    // Interior node places label to the right of the disc.
    const interior = computeClampedLabelPlacement(120, 90, 9, 88, 11, 360);
    expect(interior.side).toBe("right");
    expect(interior.boxX + interior.boxWidth).toBeLessThanOrEqual(356);

    // Right-edge node flips label left of the disc so boxX + boxWidth stays inside stageWidthPx - 4.
    const rightEdge = computeClampedLabelPlacement(338, 90, 9, 112, 11, 360);
    expect(rightEdge.side).toBe("left");
    expect(rightEdge.boxX).toBeGreaterThanOrEqual(4);
    expect(rightEdge.boxX + rightEdge.boxWidth).toBeLessThanOrEqual(356);

    // Label wider than the entire 360px stage (440px) never overflows [4, 356].
    const widerPlacement = computeClampedLabelPlacement(
      180,
      90,
      9,
      440,
      11,
      360,
    );
    expect(widerPlacement.boxX).toBeGreaterThanOrEqual(4);
    expect(widerPlacement.boxX + widerPlacement.boxWidth).toBeLessThanOrEqual(
      356,
    );

    const fillStylesUsed: string[] = [];
    const drawnLabels: Array<{ text: string; x: number; y: number }> = [];
    const drawnRects: Array<{ x: number; y: number; w: number; h: number }> =
      [];
    let currentFill = "#000000";

    const mockCtx = {
      canvas: { clientWidth: 360, width: 720 },
      font: "",
      lineWidth: 1,
      strokeStyle: "",
      get fillStyle() {
        return currentFill;
      },
      set fillStyle(val: string) {
        currentFill = val;
        fillStylesUsed.push(val.toLowerCase());
      },
      save: vi.fn(),
      restore: vi.fn(),
      beginPath: vi.fn(),
      arc: vi.fn(),
      fill: vi.fn(),
      stroke: vi.fn(),
      measureText: (txt: string) => ({ width: Array.from(txt).length * 7 }),
      fillRect: (x: number, y: number, w: number, h: number) => {
        drawnRects.push({ x, y, w, h });
      },
      strokeRect: vi.fn(),
      fillText: (text: string, x: number, y: number) => {
        drawnLabels.push({ text, x, y });
      },
    } as unknown as CanvasRenderingContext2D;

    const settings = {
      labelSize: 11,
      labelFont: "monospace",
      labelWeight: "600",
      labelColor: { color: "#e2e8f0" },
    };

    drawSkgNodeLabel(
      mockCtx,
      {
        x: 342,
        y: 80,
        size: 9,
        label: "quadcopter#1 [companion-teleop]",
        color: "#06b6d4",
      },
      settings,
    );
    expect(drawnLabels).toHaveLength(1);
    expect(drawnLabels[0]!.x).toBeLessThan(342);

    drawSkgNodeHover(
      mockCtx,
      {
        x: 342,
        y: 80,
        size: 9,
        label: "quadcopter#1 [companion-teleop]",
        color: "#06b6d4",
      },
      settings,
    );
    expect(drawnRects).toHaveLength(1);
    expect(drawnRects[0]!.x).toBeGreaterThanOrEqual(4);
    expect(drawnRects[0]!.x + drawnRects[0]!.w).toBeLessThanOrEqual(356);
    expect(fillStylesUsed).toContain("#0f172a");
    expect(fillStylesUsed).toContain("#f8fafc");
    expect(fillStylesUsed).not.toContain("#fff");
    expect(fillStylesUsed).not.toContain("#ffffff");

    // External contact title wider than the 360px stage (66 chars * 7px = 462px) at left, center, and right coordinates.
    const wideContactTitle =
      "AIS Maritime Contact Vessel Harbor-77 Long-Range Patrol Track Alpha";
    expect(Array.from(wideContactTitle).length * 7).toBeGreaterThan(360);

    for (const nodeX of [18, 180, 342]) {
      drawnLabels.length = 0;
      drawnRects.length = 0;

      drawSkgNodeLabel(
        mockCtx,
        {
          x: nodeX,
          y: 110,
          size: 8,
          label: wideContactTitle,
          color: "#a855f7",
        },
        settings,
      );
      expect(drawnLabels).toHaveLength(1);
      const normalLabel = drawnLabels[0]!;
      expect(normalLabel.text.endsWith("…")).toBe(true);
      const normalWidth = Array.from(normalLabel.text).length * 7;
      expect(normalLabel.x).toBeGreaterThanOrEqual(4);
      expect(normalLabel.x + normalWidth).toBeLessThanOrEqual(356);

      drawSkgNodeHover(
        mockCtx,
        {
          x: nodeX,
          y: 110,
          size: 8,
          label: wideContactTitle,
          color: "#a855f7",
        },
        settings,
      );
      expect(drawnRects).toHaveLength(1);
      expect(drawnLabels).toHaveLength(2);
      const pill = drawnRects[0]!;
      const hoverLabel = drawnLabels[1]!;
      expect(pill.x).toBeGreaterThanOrEqual(4);
      expect(pill.x + pill.w).toBeLessThanOrEqual(356);
      expect(hoverLabel.text.endsWith("…")).toBe(true);
      const hoverWidth = Array.from(hoverLabel.text).length * 7;
      expect(hoverLabel.x).toBeGreaterThanOrEqual(pill.x);
      expect(hoverLabel.x + hoverWidth).toBeLessThanOrEqual(pill.x + pill.w);
    }
  });
});
