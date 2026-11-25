// SPDX-License-Identifier: MIT
// Copyright (c) 2026 boxbotix

import { flushSync, mount, unmount } from "svelte";
import { describe, expect, it, vi } from "vitest";
import { createGraphSearchClient } from "../cortex/search-client";
import type { GraphQLClient } from "../graphql";
import {
  buildFilmstripItems,
  buildHealthStrip,
  extractAbnormalHealthChips,
  selectCopSoloSection,
  summarizeFleetFaults,
} from "../hud/view-model";
import { createSkgStore } from "../stores/skg.svelte";
import { createTelemetryStore } from "../stores/telemetry.svelte";
import {
  MockWebSocket,
  MockWebSocketRig,
  buildVehicleStoredPayload,
} from "../stores/test-helpers";
import { createViewportStore } from "../stores/viewport.svelte";
import { handleViewportHotkey } from "../viewport/hotkeys";
import Page from "../../routes/+page.svelte";
import WorkspaceShell from "./WorkspaceShell.svelte";

describe("WorkspaceShell, CopWorkspace, PilotWorkspace & FleetFilmstrip (M2.4)", () => {
  // spec: cop-ui / Task-modal 4-workspace shell, invariant safety topbar, and Fleet Filmstrip
  it("preserves continuous WS1/WS2 connections and non-modal unselected fleet fault visibility across COP, Pilot, Bench, Cortex, and Lights-Out", () => {
    const ws1Rig = new MockWebSocketRig();
    const ws2Rig = new MockWebSocketRig();
    const telemetryStore = createTelemetryStore({
      url: "ws://127.0.0.1:8081/ws",
      createSocket: ws1Rig.createSocket,
    });
    const skgStore = createSkgStore({
      url: "ws://127.0.0.1:8082/ws",
      createSocket: ws2Rig.createSocket,
    });
    const viewportStore = createViewportStore({ skgStore });

    telemetryStore.connect();
    skgStore.connect();
    ws1Rig.latest.serverOpen();
    ws2Rig.latest.serverOpen();

    // Vehicle #1 is nominal and selected; Vehicle #2 is unselected and in P4 McuFailsafe!
    const quad1 = buildVehicleStoredPayload(1, {
      "uxs.domain.kind": "quadcopter",
      "uxs.authority.state": "companion-teleop",
      "uxs.health.armed": true,
      "uxs.health.crcerrors": 0,
    });
    const rover2Fault = buildVehicleStoredPayload(2, {
      "uxs.domain.kind": "rover",
      "uxs.authority.state": "mcu-failsafe",
      "uxs.health.failsafes": 2,
    });
    const contactId = "boxbotix.local.tak.cop.vessel.alpha9";

    ws2Rig.latest.serverMessage(
      JSON.stringify({
        type: "put",
        bucket: "ENTITY_STATES",
        key: quad1.id,
        revision: 1,
        payload: quad1,
      }),
    );
    ws2Rig.latest.serverMessage(
      JSON.stringify({
        type: "put",
        bucket: "ENTITY_STATES",
        key: rover2Fault.id,
        revision: 2,
        payload: rover2Fault,
      }),
    );
    ws2Rig.latest.serverMessage(
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
              object: "Alpha 9",
              source: "ais",
              timestamp: "2026-11-24T12:00:00Z",
              confidence: 1,
            },
          ],
        },
      }),
    );
    ws2Rig.latest.serverMessage(
      JSON.stringify({
        type: "synced",
        bucket: "ENTITY_STATES",
        revision: 3,
      }),
    );

    const faultSummary = summarizeFleetFaults(skgStore.vehicles);
    expect(faultSummary).toEqual({
      totalVehicles: 2,
      faultedCount: 1,
      errorCount: 1,
      warningCount: 0,
      firstFaultEntityId: rover2Fault.id,
      firstFaultLabel: "UXS #2 · P4",
    });

    const stripItems = buildFilmstripItems(
      skgStore.vehicles,
      skgStore.externalContacts,
    );
    expect(stripItems.map((i) => i.shortcutKey)).toEqual(["1", "2", "3"]);
    expect(stripItems[2]?.kind).toBe("external-contact");
    expect(stripItems[2]?.safetySeverity).toBeNull();

    viewportStore.selectEntity(quad1.id);

    const hostEl = document.createElement("div");
    document.body.appendChild(hostEl);
    const app = mount(WorkspaceShell, {
      target: hostEl,
      props: {
        telemetryStore,
        skgStore,
        viewportStore,
      },
    });
    flushSync();

    // Switch through all 4 workspaces and verify unselected UXS #2 fault stays visible in topbar with zero WS reconnects.
    for (const mode of ["cop", "pilot", "bench", "cortex"] as const) {
      const tab = hostEl.querySelector(
        `[data-testid="workspace-tab-${mode}"]`,
      ) as HTMLButtonElement;
      tab.click();
      flushSync();

      expect(viewportStore.workspace).toBe(mode);
      expect(
        hostEl.querySelector(`[data-testid="${mode}-workspace"]`),
      ).not.toBeNull();
      expect(
        hostEl.querySelector('[data-testid="fleet-fault-badge"]')?.textContent,
      ).toContain("1 FLEET FAULT");
      expect(
        hostEl.querySelector('[data-testid="fleet-fault-badge"]')?.textContent,
      ).toContain("UXS #2 · P4");
    }

    expect(ws1Rig.sockets).toHaveLength(1);
    expect(ws2Rig.sockets).toHaveLength(1);
    expect(telemetryStore.reconnectCount).toBe(0);

    // Clicking the non-modal fleet fault badge selects the faulted unselected UXS #2!
    (
      hostEl.querySelector(
        '[data-testid="fleet-fault-badge"]',
      ) as HTMLButtonElement
    ).click();
    flushSync();
    expect(viewportStore.selectedEntityId).toBe(rover2Fault.id);

    // Lights-Out hides the filmstrip while keeping the invariant safety topbar visible.
    (
      hostEl.querySelector(
        '[data-testid="lights-out-toggle"]',
      ) as HTMLButtonElement
    ).click();
    flushSync();
    expect(hostEl.querySelector('[data-testid="fleet-filmstrip"]')).toBeNull();
    expect(
      hostEl.querySelector('[data-testid="invariant-safety-topbar"]'),
    ).not.toBeNull();

    unmount(app);
    hostEl.remove();
    telemetryStore.disconnect();
    skgStore.disconnect();
  });

  // spec: cop-ui / Task-modal 4-workspace shell, invariant safety topbar, and Fleet Filmstrip
  it("enforces COP Solo-Mode single-open accordion, Pilot Map-Primary <-> Split swap, ISA-101 exception promotion, and external-contact isolation", () => {
    expect(selectCopSoloSection("summary", "alerts")).toBe("alerts");
    expect(selectCopSoloSection("alerts", "invalid-tab")).toBe("alerts");

    const skgStore = createSkgStore();
    const telemetryStore = createTelemetryStore();

    const boatWithWarning = buildVehicleStoredPayload(3, {
      "uxs.domain.kind": "surface-boat",
      "uxs.authority.state": "companion-teleop",
      "uxs.health.crcerrors": 4,
    });
    const contactId = "boxbotix.local.tak.cop.vessel.delta4";

    skgStore.ingestRawFrame(
      JSON.stringify({
        type: "put",
        bucket: "ENTITY_STATES",
        key: boatWithWarning.id,
        revision: 1,
        payload: boatWithWarning,
      }),
    );
    skgStore.ingestRawFrame(
      JSON.stringify({
        type: "put",
        bucket: "ENTITY_STATES",
        key: contactId,
        revision: 2,
        payload: {
          id: contactId,
          triples: [
            {
              subject: contactId,
              predicate: "dc.terms.title",
              object: "Delta 4",
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
        revision: 2,
      }),
    );

    const boatEntity = skgStore.getVehicle(boatWithWarning.id)!;
    const abnormal = extractAbnormalHealthChips(
      buildHealthStrip(boatEntity.healthFlags, boatEntity.sysStatus),
    );
    expect(abnormal.map((c) => c.key)).toEqual(["crc_errors"]);

    const viewportStore = createViewportStore({ skgStore });
    viewportStore.selectEntity(boatWithWarning.id);

    const hostEl = document.createElement("div");
    document.body.appendChild(hostEl);
    const app = mount(WorkspaceShell, {
      target: hostEl,
      props: {
        telemetryStore,
        skgStore,
        viewportStore,
      },
    });
    flushSync();

    // 1. COP Solo-Mode accordion mounts strictly ONE panel at a time.
    const countSoloPanels = () =>
      hostEl.querySelectorAll('[data-testid^="cop-solo-panel-"]').length;
    expect(countSoloPanels()).toBe(1);
    expect(
      hostEl.querySelector('[data-testid="cop-solo-panel-summary"]'),
    ).not.toBeNull();

    (
      hostEl.querySelector(
        '[data-testid="cop-solo-tab-alerts"]',
      ) as HTMLButtonElement
    ).click();
    flushSync();
    expect(countSoloPanels()).toBe(1);
    expect(
      hostEl.querySelector('[data-testid="cop-solo-panel-alerts"]'),
    ).not.toBeNull();

    (
      hostEl.querySelector(
        '[data-testid="cop-solo-tab-skg"]',
      ) as HTMLButtonElement
    ).click();
    flushSync();
    expect(countSoloPanels()).toBe(1);
    expect(
      hostEl.querySelector('[data-testid="cop-solo-panel-skg"]'),
    ).not.toBeNull();

    // 2. Switch to Pilot: defaults to Map-Primary with floating HUD PiP, ISA-101 abnormal chip, and guarded action seam.
    viewportStore.setWorkspace("pilot");
    flushSync();
    expect(
      hostEl.querySelector('[data-testid="pilot-map-primary"]'),
    ).not.toBeNull();
    expect(
      hostEl.querySelector('[data-testid="pilot-hud-pip"]'),
    ).not.toBeNull();
    expect(
      hostEl.querySelector('[data-testid="pilot-abnormal-strip"]')?.textContent,
    ).toContain("CRC");
    expect(
      hostEl.querySelector('[data-testid="pilot-guarded-actions"]'),
    ).not.toBeNull();

    // Swap Pilot layout to 50/50 Split.
    (
      hostEl.querySelector(
        '[data-testid="pilot-layout-toggle"]',
      ) as HTMLButtonElement
    ).click();
    flushSync();
    expect(
      hostEl.querySelector('[data-testid="pilot-split-view"]'),
    ).not.toBeNull();
    expect(
      hostEl.querySelector('[data-testid="pilot-map-primary"]'),
    ).toBeNull();

    // 3. Selecting ExternalContactEntityState in Pilot & Bench surfaces both observed and received provenance while withholding UXS authority/HUD/actuation!
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
              object: "Delta 4",
              source: "ais",
              timestamp: "2030-01-15T12:00:00Z",
              confidence: 1,
            },
            {
              subject: contactId,
              predicate: "cop.contact.source",
              object: "ais-coastal",
              source: "ais",
              timestamp: "2030-01-15T12:00:00Z",
              confidence: 1,
            },
            {
              subject: contactId,
              predicate: "cop.time.observed",
              object: "2030-01-15T12:00:00.000Z",
              source: "ais",
              timestamp: "2030-01-15T12:00:14Z",
              confidence: 1,
            },
            {
              subject: contactId,
              predicate: "cop.time.received",
              object: "2030-01-15T12:00:14.000Z",
              source: "ais",
              timestamp: "2030-01-15T12:00:14Z",
              confidence: 1,
            },
          ],
        },
      }),
    );
    viewportStore.selectEntity(contactId);
    flushSync();
    expect(
      hostEl.querySelector('[data-testid="external-contact-banner"]')
        ?.textContent,
    ).toContain("observed: 2030-01-15T12:00:00.000Z");
    expect(
      hostEl.querySelector('[data-testid="external-contact-banner"]')
        ?.textContent,
    ).toContain("received: 2030-01-15T12:00:14.000Z");
    expect(
      hostEl.querySelector('[data-testid="pilot-contact-isolation"]')
        ?.textContent,
    ).toContain("observed: 2030-01-15T12:00:00.000Z");
    expect(
      hostEl.querySelector('[data-testid="pilot-contact-isolation"]')
        ?.textContent,
    ).toContain("received: 2030-01-15T12:00:14.000Z");
    expect(hostEl.querySelector('[data-testid="vehicle-hud"]')).toBeNull();
    expect(
      hostEl.querySelector('[data-testid="vehicle-hud-standby"]'),
    ).toBeNull();
    expect(hostEl.querySelector('[data-testid="authority-badge"]')).toBeNull();
    expect(hostEl.querySelector('[data-testid="armed-badge"]')).toBeNull();
    expect(
      hostEl.querySelector('[data-testid="pilot-guarded-actions"]'),
    ).toBeNull();

    viewportStore.setWorkspace("bench");
    flushSync();
    expect(
      hostEl.querySelector('[data-testid="bench-contact-notice"]')?.textContent,
    ).toContain("observed: 2030-01-15T12:00:00.000Z");
    expect(
      hostEl.querySelector('[data-testid="bench-contact-notice"]')?.textContent,
    ).toContain("received: 2030-01-15T12:00:14.000Z");

    unmount(app);
    hostEl.remove();
  });

  // spec: cop-ui / Task-modal 4-workspace shell, invariant safety topbar, and Fleet Filmstrip
  it("cancels deferred Cortex searches across workspace switches without mutating COP selection/hash, and preserves Tab traversal between DOM toolbar buttons while scoping Lights-Out to focused viewports", async () => {
    const skgStore = createSkgStore();
    const telemetryStore = createTelemetryStore();
    const quad1 = buildVehicleStoredPayload(1, {
      "uxs.domain.kind": "quadcopter",
      "dc.terms.title": "Falcon 1",
    });
    const rover2 = buildVehicleStoredPayload(2, {
      "uxs.domain.kind": "rover",
      "dc.terms.title": "Vanguard 2",
    });

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
        type: "synced",
        bucket: "ENTITY_STATES",
        revision: 2,
      }),
    );

    let resolveDeferredSearch!: (res: Response) => void;
    let searchAbortSignal: AbortSignal | undefined;
    const deferredSearchPromise = new Promise<Response>((res) => {
      resolveDeferredSearch = res;
    });

    const mockSearch = createGraphSearchClient({
      fetchImpl: (async (_input, init) => {
        searchAbortSignal = init?.signal ?? undefined;
        return deferredSearchPromise;
      }) as typeof fetch,
    });
    const mockGraphql: GraphQLClient = {
      endpoint: "/graph-gateway/graphql",
      entity: vi.fn(),
      entitiesByPrefix: vi.fn(),
      entitiesByPredicate: vi.fn(),
      pathSearch: vi.fn().mockResolvedValue({
        data: { entities: [], paths: [], truncated: false },
        damagedEntities: 0,
      }),
    };

    const viewportStore = createViewportStore({ skgStore });
    viewportStore.selectEntity(quad1.id);

    const syncedHashes: string[] = [];
    const hostEl = document.createElement("div");
    document.body.appendChild(hostEl);

    const onKeyDown = (ev: KeyboardEvent): void => {
      handleViewportHotkey(
        {
          key: ev.key,
          ctrlKey: ev.ctrlKey,
          metaKey: ev.metaKey,
          altKey: ev.altKey,
          shiftKey: ev.shiftKey,
          repeat: ev.repeat,
          defaultPrevented: ev.defaultPrevented,
          target: ev.target as HTMLElement | null,
          preventDefault: () => ev.preventDefault(),
        },
        viewportStore,
        (hash) => syncedHashes.push(hash),
        [quad1.id, rover2.id],
      );
    };
    window.addEventListener("keydown", onKeyDown);

    const app = mount(WorkspaceShell, {
      target: hostEl,
      props: {
        telemetryStore,
        skgStore,
        viewportStore,
        graphqlClient: mockGraphql,
        searchClient: mockSearch,
        onSyncHash: (hash) => syncedHashes.push(hash),
      },
    });
    flushSync();

    try {
      // 1. Tab from page entry (document.body) and between toolbar buttons is NOT prevented.
      const bodyTab = new KeyboardEvent("keydown", {
        key: "Tab",
        bubbles: true,
        cancelable: true,
      });
      document.body.dispatchEvent(bodyTab);
      expect(bodyTab.defaultPrevented).toBe(false);
      expect(viewportStore.lightsOut).toBe(false);

      const copTabBtn = hostEl.querySelector(
        '[data-testid="workspace-tab-cop"]',
      ) as HTMLButtonElement;
      const pilotTabBtn = hostEl.querySelector(
        '[data-testid="workspace-tab-pilot"]',
      ) as HTMLButtonElement;
      copTabBtn.focus();
      expect(document.activeElement).toBe(copTabBtn);
      const toolbarTab = new KeyboardEvent("keydown", {
        key: "Tab",
        bubbles: true,
        cancelable: true,
      });
      copTabBtn.dispatchEvent(toolbarTab);
      expect(toolbarTab.defaultPrevented).toBe(false);
      expect(viewportStore.lightsOut).toBe(false);
      pilotTabBtn.focus();
      expect(document.activeElement).toBe(pilotTabBtn);

      // Focusing the interactive tactical canvas surface scopes Tab to Lights-Out toggle.
      const scopedCanvas = hostEl.querySelector(
        '[data-viewport-hotkey-scope="true"]',
      ) as HTMLCanvasElement;
      expect(scopedCanvas).not.toBeNull();
      scopedCanvas.focus();
      expect(document.activeElement).toBe(scopedCanvas);
      const canvasTab = new KeyboardEvent("keydown", {
        key: "Tab",
        bubbles: true,
        cancelable: true,
      });
      scopedCanvas.dispatchEvent(canvasTab);
      flushSync();
      expect(canvasTab.defaultPrevented).toBe(true);
      expect(viewportStore.lightsOut).toBe(true);

      // Toggle Lights-Out back off via the same scoped viewport shortcut.
      const canvasTabOff = new KeyboardEvent("keydown", {
        key: "Tab",
        bubbles: true,
        cancelable: true,
      });
      scopedCanvas.dispatchEvent(canvasTabOff);
      flushSync();
      expect(viewportStore.lightsOut).toBe(false);

      // 2. Switch to Cortex, launch deferred lexical search, then switch back to COP before it resolves.
      (
        hostEl.querySelector(
          '[data-testid="workspace-tab-cortex"]',
        ) as HTMLButtonElement
      ).click();
      flushSync();

      (
        hostEl.querySelector(
          '[data-testid="cortex-tier-lexical"]',
        ) as HTMLButtonElement
      ).click();
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
      flushSync();

      // Switch back to COP before deferred search settles.
      copTabBtn.click();
      flushSync();
      expect(viewportStore.workspace).toBe("cop");
      expect(searchAbortSignal?.aborted).toBe(true);
      const hashCountAtCopSwitch = syncedHashes.length;
      const hashAtCopSwitch = syncedHashes.at(-1);

      // Deferred search resolves with rover2.id after Cortex was torn down.
      resolveDeferredSearch(
        new Response(
          JSON.stringify({
            ok: true,
            verb: "globalSearch",
            result: {
              indexed_revision: 42,
              damaged_entities: 0,
              producer: "graph-query",
              data: {
                text: {
                  matches: [
                    {
                      id: rover2.id,
                      label: "Vanguard 2",
                      score: 5.0,
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
      await Promise.resolve();
      await Promise.resolve();
      flushSync();

      expect(viewportStore.workspace).toBe("cop");
      expect(viewportStore.selectedEntityId).toBe(quad1.id);
      expect(syncedHashes).toHaveLength(hashCountAtCopSwitch);
      expect(syncedHashes.at(-1)).toBe(hashAtCopSwitch);
      expect(mockGraphql.pathSearch).not.toHaveBeenCalled();
    } finally {
      window.removeEventListener("keydown", onKeyDown);
      unmount(app);
      hostEl.remove();
    }
  });

  // spec: cop-ui / Task-modal 4-workspace shell, invariant safety topbar, and Fleet Filmstrip
  // spec: cop-ui / Primary attitude HUD, 4-state ControlAuthority banner, and external contact UI isolation
  it("counts poisoned-first ownship vehicle snapshots as non-commandable fleet faults until replaced by a healthy put, and prioritizes current P4 faults over historical failsafe/CRC counters while retaining cumulative counts in Bench", () => {
    const skgStore = createSkgStore();
    const telemetryStore = createTelemetryStore();
    const viewportStore = createViewportStore({ skgStore });
    const poisonedKey = "boxbotix.local.robotics.uxs.vehicle.2";

    // 1. Poisoned-first initial snapshot (no prior decodable put -> entity.kind === "generic").
    skgStore.ingestRawFrame(
      JSON.stringify({
        type: "poisoned",
        bucket: "ENTITY_STATES",
        key: poisonedKey,
        revision: 1,
        error: "corrupt msgpack triple",
      }),
    );
    skgStore.ingestRawFrame(
      JSON.stringify({
        type: "synced",
        bucket: "ENTITY_STATES",
        revision: 1,
      }),
    );

    expect(skgStore.getEntity(poisonedKey)?.kind).toBe("generic");
    expect(skgStore.vehicles).toHaveLength(0);
    expect(skgStore.commandableVehicles).toHaveLength(0);
    expect(skgStore.getCommandableVehicle(poisonedKey)).toBeNull();

    const poisonedSummary = summarizeFleetFaults(skgStore.entities.values());
    expect(poisonedSummary).toEqual({
      totalVehicles: 1,
      faultedCount: 1,
      errorCount: 1,
      warningCount: 0,
      firstFaultEntityId: poisonedKey,
      firstFaultLabel: "UXS #2 · POISONED",
    });

    const hostEl = document.createElement("div");
    document.body.appendChild(hostEl);
    const app = mount(WorkspaceShell, {
      target: hostEl,
      props: { telemetryStore, skgStore, viewportStore },
    });
    flushSync();

    try {
      expect(
        hostEl.querySelector('[data-testid="fleet-nominal-badge"]'),
      ).toBeNull();
      const faultBtn = hostEl.querySelector(
        '[data-testid="fleet-fault-badge"]',
      ) as HTMLButtonElement;
      expect(faultBtn?.textContent).toContain("1 FLEET FAULT");
      expect(faultBtn?.textContent).toContain("UXS #2 · POISONED");

      // Clicking the badge selects the poisoned-first record while keeping Pilot & Bench non-commandable.
      faultBtn.click();
      flushSync();
      expect(viewportStore.selectedEntityId).toBe(poisonedKey);
      expect(viewportStore.selectedCommandableVehicle).toBeNull();
      expect(
        hostEl.querySelector('[data-testid="poisoned-badge"]')?.textContent,
      ).toContain("POISONED: corrupt msgpack triple");
      expect(
        hostEl.querySelector('[data-testid="authority-badge"]'),
      ).toBeNull();

      viewportStore.setWorkspace("pilot");
      flushSync();
      expect(
        hostEl.querySelector('[data-testid="pilot-guarded-actions"]'),
      ).toBeNull();
      expect(
        hostEl.querySelector('[data-testid="pilot-poisoned-guard"]')
          ?.textContent,
      ).toContain("POISONED ENTITY");

      viewportStore.setWorkspace("bench");
      flushSync();
      expect(
        hostEl.querySelector('[data-testid="bench-tuning-withheld"]')
          ?.textContent,
      ).toContain("UXS #2 is POISONED");
      expect(hostEl.querySelectorAll('input[type="range"]')).toHaveLength(0);

      // 2. Replacement by a healthy put upgrades UXS #2 and clears the fleet fault to FLEET NOMINAL (1).
      const healthyRover2 = buildVehicleStoredPayload(2, {
        "uxs.domain.kind": "rover",
        "uxs.authority.state": "companion-autonomous",
        "uxs.health.armed": true,
      });
      skgStore.ingestRawFrame(
        JSON.stringify({
          type: "put",
          bucket: "ENTITY_STATES",
          key: poisonedKey,
          revision: 2,
          payload: healthyRover2,
        }),
      );
      flushSync();

      expect(skgStore.commandableVehicles).toHaveLength(1);
      expect(
        hostEl.querySelector('[data-testid="fleet-fault-badge"]'),
      ).toBeNull();
      expect(
        hostEl.querySelector('[data-testid="fleet-nominal-badge"]')
          ?.textContent,
      ).toContain("FLEET NOMINAL (1)");

      // 3. UXS #1 recovers to P2 with nonzero historical failsafes=1 & crcErrors=2 while UXS #2 enters P4.
      const recoveredQuad1 = buildVehicleStoredPayload(1, {
        "uxs.domain.kind": "quadcopter",
        "uxs.authority.state": "companion-teleop",
        "uxs.health.armed": true,
        "uxs.health.failsafes": 1,
        "uxs.health.crcerrors": 2,
      });
      const failsafeRover2 = buildVehicleStoredPayload(2, {
        "uxs.domain.kind": "rover",
        "uxs.authority.state": "mcu-failsafe",
        "uxs.health.failsafes": 1,
      });
      skgStore.ingestRawFrame(
        JSON.stringify({
          type: "put",
          bucket: "ENTITY_STATES",
          key: recoveredQuad1.id,
          revision: 3,
          payload: recoveredQuad1,
        }),
      );
      skgStore.ingestRawFrame(
        JSON.stringify({
          type: "put",
          bucket: "ENTITY_STATES",
          key: failsafeRover2.id,
          revision: 4,
          payload: failsafeRover2,
        }),
      );
      flushSync();

      const postRecoverySummary = summarizeFleetFaults(
        skgStore.entities.values(),
      );
      expect(postRecoverySummary).toEqual({
        totalVehicles: 2,
        faultedCount: 1,
        errorCount: 1,
        warningCount: 0,
        firstFaultEntityId: failsafeRover2.id,
        firstFaultLabel: "UXS #2 · P4",
      });

      // Selecting UXS #1 in Bench still retains cumulative failsafes=1 and crcErrors=2 in bench-health-strip,
      // while the topbar badge jumps to UXS #2 · P4 rather than UXS #1 · P2!
      viewportStore.selectEntity(recoveredQuad1.id);
      flushSync();
      const benchStrip = hostEl.querySelector(
        '[data-testid="bench-health-strip"]',
      );
      expect(
        benchStrip?.querySelector('[data-chip-key="failsafes"]')?.textContent,
      ).toContain("1");
      expect(
        benchStrip?.querySelector('[data-chip-key="crc_errors"]')?.textContent,
      ).toContain("2");

      const activeBadge = hostEl.querySelector(
        '[data-testid="fleet-fault-badge"]',
      ) as HTMLButtonElement;
      expect(activeBadge.textContent).toContain("1 FLEET FAULT");
      expect(activeBadge.textContent).toContain("UXS #2 · P4");
      activeBadge.click();
      flushSync();
      expect(viewportStore.selectedEntityId).toBe(failsafeRover2.id);
    } finally {
      unmount(app);
      hostEl.remove();
    }
  });

  // spec: cop-ui / Task-modal 4-workspace shell, invariant safety topbar, and Fleet Filmstrip
  it("mounts +page.svelte and maps every displayed filmstrip shortcut key to the same entity ID and URL hash when poisoned-first ownship vehicles are present", () => {
    const sockets: MockWebSocket[] = [];
    class StubWebSocket extends MockWebSocket {}
    const FakeWebSocketCtor = function (url: string | URL) {
      const sock = new StubWebSocket(String(url));
      sockets.push(sock);
      return sock;
    } as unknown as typeof WebSocket;
    vi.stubGlobal("WebSocket", FakeWebSocketCtor);

    const originalHash = window.location.hash;
    window.history.replaceState(
      null,
      "",
      `${window.location.pathname}${window.location.search}`,
    );

    const hostEl = document.createElement("div");
    document.body.appendChild(hostEl);
    let app: ReturnType<typeof mount> | undefined;

    try {
      app = mount(Page, { target: hostEl });
      flushSync();

      const skgSocket = sockets.find((sock) => sock.url.includes("/ws/graph"));
      expect(skgSocket).toBeDefined();
      skgSocket!.serverOpen();

      const poisonedFirstId = "boxbotix.local.robotics.uxs.vehicle.1";
      const healthySecond = buildVehicleStoredPayload(2, {
        "uxs.domain.kind": "rover",
        "dc.terms.title": "Rover Two",
      });
      const contactThirdId = "boxbotix.local.tak.cop.contact.alpha";

      skgSocket!.serverMessage(
        JSON.stringify({
          type: "poisoned",
          bucket: "ENTITY_STATES",
          key: poisonedFirstId,
          revision: 1,
          error: "corrupt first ownship payload",
        }),
      );
      skgSocket!.serverMessage(
        JSON.stringify({
          type: "put",
          bucket: "ENTITY_STATES",
          key: healthySecond.id,
          revision: 2,
          payload: healthySecond,
        }),
      );
      skgSocket!.serverMessage(
        JSON.stringify({
          type: "put",
          bucket: "ENTITY_STATES",
          key: contactThirdId,
          revision: 3,
          payload: {
            id: contactThirdId,
            triples: [
              {
                subject: contactThirdId,
                predicate: "dc.terms.title",
                object: "Alpha Buoy",
                source: "ais",
                timestamp: "2030-01-15T12:00:00Z",
                confidence: 1,
              },
              {
                subject: contactThirdId,
                predicate: "cop.contact.source",
                object: "ais-coastal",
                source: "ais",
                timestamp: "2030-01-15T12:00:00Z",
                confidence: 1,
              },
              {
                subject: contactThirdId,
                predicate: "geo.location.latitude",
                object: 37.801,
                source: "ais",
                timestamp: "2030-01-15T12:00:00Z",
                confidence: 1,
              },
              {
                subject: contactThirdId,
                predicate: "geo.location.longitude",
                object: -122.405,
                source: "ais",
                timestamp: "2030-01-15T12:00:00Z",
                confidence: 1,
              },
            ],
          },
        }),
      );
      skgSocket!.serverMessage(
        JSON.stringify({
          type: "synced",
          bucket: "ENTITY_STATES",
          revision: 3,
        }),
      );
      flushSync();

      const cards = Array.from(
        hostEl.querySelectorAll<HTMLButtonElement>(
          '[data-testid="filmstrip-item"]',
        ),
      );
      expect(
        cards.map((card) => ({
          key: card.querySelector(".shortcut-key")?.textContent?.trim(),
          entityId: card.getAttribute("data-entity-id"),
        })),
      ).toEqual([
        { key: "1", entityId: poisonedFirstId },
        { key: "2", entityId: healthySecond.id },
        { key: "3", entityId: contactThirdId },
      ]);

      // Press each card's displayed shortcut key (3 -> 1 -> 2) on window and verify
      // both the active selected card and window.location.hash match that card's entity ID.
      for (const card of [cards[2]!, cards[0]!, cards[1]!]) {
        const displayedKey = card
          .querySelector(".shortcut-key")
          ?.textContent?.trim();
        const cardEntityId = card.getAttribute("data-entity-id");
        expect(displayedKey).toBeTruthy();
        expect(cardEntityId).toBeTruthy();

        const keyEv = new KeyboardEvent("keydown", {
          key: displayedKey!,
          bubbles: true,
          cancelable: true,
        });
        window.dispatchEvent(keyEv);
        flushSync();
        expect(keyEv.defaultPrevented).toBe(true);

        const activeCards = hostEl.querySelectorAll<HTMLButtonElement>(
          '[data-testid="filmstrip-item"][aria-pressed="true"]',
        );
        expect(activeCards).toHaveLength(1);
        expect(activeCards[0]?.getAttribute("data-entity-id")).toBe(
          cardEntityId,
        );

        const hashParams = new URLSearchParams(
          window.location.hash.replace(/^#/, ""),
        );
        expect(hashParams.get("sel")).toBe(cardEntityId);
      }
    } finally {
      if (app) {
        unmount(app);
      }
      hostEl.remove();
      window.history.replaceState(
        null,
        "",
        `${window.location.pathname}${window.location.search}${originalHash}`,
      );
      vi.unstubAllGlobals();
    }
  });
});
