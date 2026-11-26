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
import {
  CHARACTER_SHORTCUTS_STORAGE_KEY,
  createViewportStore,
} from "../stores/viewport.svelte";
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

    const syncedHashes: string[] = [];
    const hostEl = document.createElement("div");
    document.body.appendChild(hostEl);
    const app = mount(WorkspaceShell, {
      target: hostEl,
      props: {
        telemetryStore,
        skgStore,
        viewportStore,
        onSyncHash: (hash) => syncedHashes.push(hash),
      },
    });
    flushSync();

    // Navigation displays Fleet, Vehicle, Diagnostics, and Analysis in order while preserving mode IDs and keycaps.
    const navButtons = Array.from(
      hostEl.querySelectorAll<HTMLButtonElement>(
        '.workspace-switcher [data-testid^="workspace-tab-"]',
      ),
    );
    expect(
      navButtons.map((btn) => ({
        testId: btn.getAttribute("data-testid"),
        label: btn.querySelector("span")?.textContent?.trim(),
        keycap: btn.querySelector("kbd")?.textContent?.trim(),
      })),
    ).toEqual([
      { testId: "workspace-tab-cop", label: "Fleet", keycap: "G" },
      { testId: "workspace-tab-pilot", label: "Vehicle", keycap: "D" },
      { testId: "workspace-tab-bench", label: "Diagnostics", keycap: "B" },
      { testId: "workspace-tab-cortex", label: "Analysis", keycap: "K" },
    ]);

    // Fleet Solo-Mode inspector exposes updated accessible labels and cross-workspace jump actions.
    expect(
      hostEl
        .querySelector('[data-testid="cop-solo-rail"]')
        ?.getAttribute("aria-label"),
    ).toBe("Fleet Solo-Mode inspector");
    expect(
      hostEl.querySelector('[role="tablist"]')?.getAttribute("aria-label"),
    ).toBe("Fleet inspector sections");

    const summaryJumpBtns = Array.from(
      hostEl.querySelectorAll<HTMLButtonElement>(
        '[data-testid="cop-solo-panel-summary"] .jump-btn',
      ),
    );
    expect(summaryJumpBtns.map((btn) => btn.textContent?.trim())).toEqual([
      "Vehicle [D]",
      "Diagnostics [B]",
      "Analysis [K]",
    ]);

    // Cross-workspace jump buttons navigate to the intended workspace and preserve #mode= URL tokens.
    summaryJumpBtns[0]!.click();
    flushSync();
    expect(viewportStore.workspace).toBe("pilot");
    expect(syncedHashes.at(-1)).toContain("mode=pilot");
    expect(
      hostEl.querySelector(".pilot-mode-label")?.textContent?.trim(),
    ).toContain("VEHICLE WORKSPACE");

    viewportStore.setWorkspace("cop");
    flushSync();
    (
      hostEl.querySelector(
        '[data-testid="cop-solo-panel-summary"] .jump-btn:nth-child(2)',
      ) as HTMLButtonElement
    ).click();
    flushSync();
    expect(viewportStore.workspace).toBe("bench");
    expect(syncedHashes.at(-1)).toContain("mode=bench");

    viewportStore.setWorkspace("cop");
    flushSync();
    (
      hostEl.querySelector(
        '[data-testid="cop-solo-tab-skg"]',
      ) as HTMLButtonElement
    ).click();
    flushSync();
    const openAnalysisBtn = hostEl.querySelector(
      '[data-testid="cop-solo-panel-skg"] .jump-btn',
    ) as HTMLButtonElement;
    expect(openAnalysisBtn.textContent?.trim()).toBe("Open Analysis [K]");
    openAnalysisBtn.click();
    flushSync();
    expect(viewportStore.workspace).toBe("cortex");
    expect(syncedHashes.at(-1)).toContain("mode=cortex");

    // External contact summary in Fleet links to "Inspect Provenance in Analysis [K]" while keeping domain COP label intact.
    viewportStore.setWorkspace("cop");
    viewportStore.selectEntity(contactId);
    flushSync();
    (
      hostEl.querySelector(
        '[data-testid="cop-solo-tab-summary"]',
      ) as HTMLButtonElement
    ).click();
    flushSync();
    const contactJumpBtn = hostEl.querySelector(
      '[data-testid="cop-contact-summary"] .jump-btn',
    ) as HTMLButtonElement;
    expect(contactJumpBtn.textContent?.trim()).toBe(
      "Inspect Provenance in Analysis [K]",
    );
    expect(
      hostEl
        .querySelector('[data-testid="external-contact-banner"]')
        ?.getAttribute("aria-label"),
    ).toBe("External COP contact provenance");
    contactJumpBtn.click();
    flushSync();
    expect(viewportStore.workspace).toBe("cortex");

    viewportStore.selectEntity(quad1.id);
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

    // 1. COP Solo-Mode accordion keeps all 3 role="tabpanel" targets mounted for aria-controls while expanding strictly ONE unhidden panel lazily.
    const countMountedPanels = () =>
      hostEl.querySelectorAll('[data-testid^="cop-solo-panel-"]').length;
    const getVisiblePanels = () =>
      Array.from(
        hostEl.querySelectorAll<HTMLElement>(
          '[data-testid^="cop-solo-panel-"]:not([hidden])',
        ),
      );
    const getHiddenPanels = () =>
      Array.from(
        hostEl.querySelectorAll<HTMLElement>(
          '[data-testid^="cop-solo-panel-"][hidden]',
        ),
      );
    expect(countMountedPanels()).toBe(3);
    expect(getVisiblePanels().map((el) => el.dataset.testid)).toEqual([
      "cop-solo-panel-summary",
    ]);
    expect(getHiddenPanels().every((el) => el.children.length === 0)).toBe(
      true,
    );

    (
      hostEl.querySelector(
        '[data-testid="cop-solo-tab-alerts"]',
      ) as HTMLButtonElement
    ).click();
    flushSync();
    expect(countMountedPanels()).toBe(3);
    expect(getVisiblePanels().map((el) => el.dataset.testid)).toEqual([
      "cop-solo-panel-alerts",
    ]);
    expect(getHiddenPanels().every((el) => el.children.length === 0)).toBe(
      true,
    );

    (
      hostEl.querySelector(
        '[data-testid="cop-solo-tab-skg"]',
      ) as HTMLButtonElement
    ).click();
    flushSync();
    expect(countMountedPanels()).toBe(3);
    expect(getVisiblePanels().map((el) => el.dataset.testid)).toEqual([
      "cop-solo-panel-skg",
    ]);
    expect(getHiddenPanels().every((el) => el.children.length === 0)).toBe(
      true,
    );

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
  // spec: cop-ui / Conventional keyboard traversal, WAI-ARIA tablist navigation, focus management, and character-key shortcut gating
  // spec: cop-ui / Session-scoped Diagnostics parameter draft and Analysis investigation continuity across workspace switches
  it("completes deferred Cortex searches in AnalysisSessionStore across workspace switches without mutating COP selection/hash, aborts on shell teardown, preserves Tab/Shift+Tab traversal on focused canvases, and toggles Lights-Out via L", async () => {
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

    const searchQueue: Array<{
      signal: AbortSignal | undefined;
      resolve: (res: Response) => void;
    }> = [];

    const mockSearch = createGraphSearchClient({
      fetchImpl: (async (_input, init) => {
        return new Promise<Response>((resolve) => {
          searchQueue.push({
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

      // Focusing the interactive tactical canvas surface preserves native Tab and Shift+Tab focus traversal; L toggles Lights-Out.
      expect(
        hostEl.querySelector('[data-viewport-hotkey-scope="true"]'),
      ).toBeNull();
      const tacticalCanvas = hostEl.querySelector(
        ".safety-hud-canvas",
      ) as HTMLCanvasElement;
      expect(tacticalCanvas).not.toBeNull();
      tacticalCanvas.focus();
      expect(document.activeElement).toBe(tacticalCanvas);
      const canvasTab = new KeyboardEvent("keydown", {
        key: "Tab",
        bubbles: true,
        cancelable: true,
      });
      tacticalCanvas.dispatchEvent(canvasTab);
      const canvasShiftTab = new KeyboardEvent("keydown", {
        key: "Tab",
        shiftKey: true,
        bubbles: true,
        cancelable: true,
      });
      tacticalCanvas.dispatchEvent(canvasShiftTab);
      flushSync();
      expect(canvasTab.defaultPrevented).toBe(false);
      expect(canvasShiftTab.defaultPrevented).toBe(false);
      expect(viewportStore.lightsOut).toBe(false);

      const lightsOutOn = new KeyboardEvent("keydown", {
        key: "l",
        bubbles: true,
        cancelable: true,
      });
      tacticalCanvas.dispatchEvent(lightsOutOn);
      flushSync();
      expect(lightsOutOn.defaultPrevented).toBe(true);
      expect(viewportStore.lightsOut).toBe(true);

      const lightsOutOff = new KeyboardEvent("keydown", {
        key: "L",
        bubbles: true,
        cancelable: true,
      });
      window.dispatchEvent(lightsOutOff);
      flushSync();
      expect(lightsOutOff.defaultPrevented).toBe(true);
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

      // Switch back to COP before deferred search settles: OQ2 Option A keeps search running in AnalysisSessionStore.
      copTabBtn.click();
      flushSync();
      expect(viewportStore.workspace).toBe("cop");
      expect(searchQueue).toHaveLength(1);
      expect(searchQueue[0]?.signal?.aborted).toBe(false);

      // Returning early while still pending surfaces defined "running" status + Cancel button.
      const cortexTabBtn = hostEl.querySelector(
        '[data-testid="workspace-tab-cortex"]',
      ) as HTMLButtonElement;
      cortexTabBtn.click();
      flushSync();
      expect(
        hostEl
          .querySelector('[data-testid="cortex-session-status"]')
          ?.getAttribute("data-status"),
      ).toBe("running");
      expect(
        hostEl.querySelector('[data-testid="cortex-query-cancel"]'),
      ).not.toBeNull();

      // Switch back to COP again and let the search settle in the background without mutating COP selection or hash.
      copTabBtn.click();
      flushSync();
      const hashCountAtCopSwitch = syncedHashes.length;
      const hashAtCopSwitch = syncedHashes.at(-1);

      searchQueue[0]!.resolve(
        new Response(
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
                    id: rover2.id,
                    label: "Vanguard 2",
                    score: 5.0,
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
        expect(mockGraphql.pathSearch).toHaveBeenCalledWith(
          expect.objectContaining({ startEntity: rover2.id }),
          expect.anything(),
        );
      });
      flushSync();

      expect(viewportStore.workspace).toBe("cop");
      expect(viewportStore.selectedEntityId).toBe(quad1.id);
      expect(syncedHashes).toHaveLength(hashCountAtCopSwitch);
      expect(syncedHashes.at(-1)).toBe(hashAtCopSwitch);

      // Returning to Analysis shows completed hit + captured UXS #1 provenance; launching a new search and unmounting WorkspaceShell aborts it.
      cortexTabBtn.click();
      flushSync();
      expect(
        hostEl
          .querySelector('[data-testid="cortex-session-status"]')
          ?.getAttribute("data-status"),
      ).toBe("completed");
      expect(
        hostEl.querySelectorAll('[data-testid="cortex-hit-item"]'),
      ).toHaveLength(1);
      expect(
        hostEl.querySelector('[data-testid="cortex-result-provenance"]')
          ?.textContent,
      ).toContain("UXS #1");

      const remountedForm = hostEl.querySelector(
        '[data-testid="cortex-query-bar"]',
      ) as HTMLFormElement;
      remountedForm.dispatchEvent(
        new Event("submit", { bubbles: true, cancelable: true }),
      );
      flushSync();
      expect(searchQueue).toHaveLength(2);
      expect(searchQueue[1]?.signal?.aborted).toBe(false);
    } finally {
      window.removeEventListener("keydown", onKeyDown);
      unmount(app);
      expect(searchQueue[1]?.signal?.aborted).toBe(true);
      hostEl.remove();
    }
  });

  // spec: cop-ui / Task-modal 4-workspace shell, invariant safety topbar, and Fleet Filmstrip
  // spec: cop-ui / Primary attitude HUD, 4-state ControlAuthority banner, and external contact UI isolation
  it("counts poisoned-first ownship vehicle snapshots as non-commandable fleet faults until replaced by a healthy put, and prioritizes current P4 faults over historical failsafe/CRC counters while retaining cumulative counts in Bench", () => {
    const ws2Rig = new MockWebSocketRig();
    const skgStore = createSkgStore({
      url: "ws://127.0.0.1:8082/ws",
      createSocket: ws2Rig.createSocket,
    });
    const telemetryStore = createTelemetryStore();
    const viewportStore = createViewportStore({ skgStore });
    const poisonedKey = "boxbotix.local.robotics.uxs.vehicle.2";

    skgStore.connect();
    ws2Rig.latest.serverOpen();

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
      skgStore.disconnect();
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

  // spec: cop-ui / Fleet health readiness, per-vehicle observation freshness, and last-known fault retention
  // spec: cop-ui / Task-modal 4-workspace shell, invariant safety topbar, and Fleet Filmstrip
  // spec: cop-ui / Primary attitude HUD, 4-state ControlAuthority banner, and external contact UI isolation
  it("gates FLEET NOMINAL on connected+synced Fleet data and fresh non-empty vehicles, ticks 1 Hz LAST KNOWN staleness without WebSocket messages, standardizes ARMED/DISARMED, and retains stale faults across link loss and snapshot replay (#39)", () => {
    vi.useFakeTimers();
    const startMs = Date.parse("2026-11-25T12:00:00.000Z");
    vi.setSystemTime(startMs);

    const ws1Rig = new MockWebSocketRig();
    const ws2Rig = new MockWebSocketRig();
    const telemetryStore = createTelemetryStore({
      url: "ws://127.0.0.1:8081/ws",
      createSocket: ws1Rig.createSocket,
      now: () => Date.now(),
    });
    const skgStore = createSkgStore({
      url: "ws://127.0.0.1:8082/ws",
      createSocket: ws2Rig.createSocket,
      now: () => Date.now(),
    });
    const viewportStore = createViewportStore({ skgStore });

    const hostEl = document.createElement("div");
    document.body.appendChild(hostEl);
    const app = mount(WorkspaceShell, {
      target: hostEl,
      props: {
        telemetryStore,
        skgStore,
        viewportStore,
        now: () => Date.now(),
      },
    });
    flushSync();

    try {
      // 1a. Initial disconnected state (before connect()): never claims FLEET NOMINAL.
      expect(
        hostEl.querySelector('[data-testid="stream-pill-telemetry"]')
          ?.textContent,
      ).toContain("Telemetry:");
      expect(
        hostEl.querySelector('[data-testid="stream-pill-fleet"]')?.textContent,
      ).toContain("Fleet data:");
      expect(
        hostEl.querySelector('[data-testid="fleet-nominal-badge"]'),
      ).toBeNull();
      expect(
        hostEl.querySelector('[data-testid="fleet-health-badge"]')?.textContent,
      ).toContain("FLEET DATA UNAVAILABLE");

      // 1b. Connecting pre-sync -> AWAITING FLEET DATA…
      telemetryStore.connect();
      skgStore.connect();
      flushSync();
      expect(
        hostEl.querySelector('[data-testid="fleet-nominal-badge"]'),
      ).toBeNull();
      expect(
        hostEl.querySelector('[data-testid="fleet-health-badge"]')?.textContent,
      ).toContain("AWAITING FLEET DATA");

      // 1c. Handshake drop before ENTITY_STATES sync -> RECONNECTING FLEET DATA…
      ws1Rig.latest.serverDrop(1006);
      ws2Rig.latest.serverDrop(1006);
      flushSync();
      expect(
        hostEl.querySelector('[data-testid="fleet-nominal-badge"]'),
      ).toBeNull();
      expect(
        hostEl.querySelector('[data-testid="fleet-health-badge"]')?.textContent,
      ).toContain("RECONNECTING FLEET DATA");
      expect(hostEl.textContent).not.toContain("FLEET NOMINAL");

      // Open COP Exceptions solo tab to verify readiness-aware empty copy.
      (
        hostEl.querySelector(
          '[data-testid="cop-solo-tab-alerts"]',
        ) as HTMLButtonElement
      ).click();
      flushSync();
      expect(
        hostEl.querySelector('[data-testid="cop-exceptions-empty"]')
          ?.textContent,
      ).toContain("Waiting for initial Fleet data sync");

      // 2. Reconnect both streams & sync with 0 ownship vehicles + 1 external contact -> No vehicles detected.
      telemetryStore.connect();
      skgStore.connect();
      ws1Rig.latest.serverOpen();
      ws2Rig.latest.serverOpen();
      const contactId = "boxbotix.local.tak.cop.vessel.bravo";
      ws2Rig.latest.serverMessage(
        JSON.stringify({
          type: "put",
          bucket: "ENTITY_STATES",
          key: contactId,
          revision: 1,
          payload: {
            id: contactId,
            triples: [
              {
                subject: contactId,
                predicate: "dc.terms.title",
                object: "Bravo Buoy",
                source: "ais",
                timestamp: new Date(Date.now()).toISOString(),
                confidence: 1,
              },
              {
                subject: contactId,
                predicate: "cop.contact.source",
                object: "ais",
                source: "ais",
                timestamp: new Date(Date.now()).toISOString(),
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
          revision: 1,
        }),
      );
      flushSync();

      expect(
        hostEl.querySelector('[data-testid="fleet-nominal-badge"]'),
      ).toBeNull();
      expect(
        hostEl
          .querySelector('[data-testid="fleet-health-badge"]')
          ?.getAttribute("data-fleet-state"),
      ).toBe("empty");
      expect(
        hostEl.querySelector('[data-testid="fleet-health-badge"]')?.textContent,
      ).toContain("No vehicles detected");
      expect(
        hostEl.querySelector('[data-testid="cop-exceptions-empty"]')
          ?.textContent,
      ).toContain("No ownship UXS vehicles detected.");

      // 3. Ingest two live fault-free vehicles: Quad #1 (sitl-local, ARMED) + Boat #2 (mesh-supervisory, DISARMED).
      const quad1 = buildVehicleStoredPayload(
        1,
        {
          "uxs.domain.kind": "quadcopter",
          "uxs.authority.state": "companion-teleop",
          "uxs.link.tier": "sitl-local",
          "uxs.health.armed": true,
          "uxs.health.crcerrors": 0,
        },
        { observedAt: new Date(Date.now()).toISOString() },
      );
      const boat2 = buildVehicleStoredPayload(
        2,
        {
          "uxs.domain.kind": "surface-boat",
          "uxs.authority.state": "companion-autonomous",
          "uxs.link.tier": "mesh-supervisory",
          "uxs.health.armed": false,
          "uxs.health.crcerrors": 0,
        },
        { observedAt: new Date(Date.now()).toISOString() },
      );
      ws2Rig.latest.serverMessage(
        JSON.stringify({
          type: "put",
          bucket: "ENTITY_STATES",
          key: quad1.id,
          revision: 2,
          payload: quad1,
        }),
      );
      ws2Rig.latest.serverMessage(
        JSON.stringify({
          type: "put",
          bucket: "ENTITY_STATES",
          key: boat2.id,
          revision: 3,
          payload: boat2,
        }),
      );
      flushSync();

      expect(
        hostEl.querySelector('[data-testid="fleet-nominal-badge"]')
          ?.textContent,
      ).toContain("FLEET NOMINAL (2)");
      viewportStore.selectEntity(quad1.id);
      flushSync();
      const armedBadge = hostEl.querySelector('[data-testid="armed-badge"]');
      expect(armedBadge?.textContent?.trim()).toBe("ARMED");
      expect(armedBadge?.className).toContain("severity-success");

      // Explicit ARMED / DISARMED across filmstrip & roster (never "SAFE").
      const filmstripText =
        hostEl.querySelector('[data-testid="fleet-filmstrip"]')?.textContent ??
        "";
      expect(filmstripText).toContain("ARMED");
      expect(filmstripText).toContain("DISARMED");
      expect(filmstripText).not.toContain("SAFE");
      const rosterText =
        hostEl.querySelector('[data-testid="cop-roster"]')?.textContent ?? "";
      expect(rosterText).toContain("ARMED");
      expect(rosterText).toContain("DISARMED");
      expect(rosterText).not.toContain("SAFE");

      // 4. Partial stream degradation: Telemetry (WS #1) drops while Fleet data (WS #2) remains connected+synced & fresh.
      ws1Rig.latest.serverDrop(1006);
      flushSync();
      expect(
        hostEl
          .querySelector('[data-testid="stream-pill-telemetry"]')
          ?.getAttribute("data-tone"),
      ).toBe("degraded");
      expect(
        hostEl
          .querySelector('[data-testid="stream-pill-fleet"]')
          ?.getAttribute("data-tone"),
      ).toBe("connected");
      expect(
        hostEl.querySelector('[data-testid="fleet-nominal-badge"]')
          ?.textContent,
      ).toContain("FLEET NOMINAL (2)");

      // Reconnect WS #1 before advancing timers so backoff doesn't interfere.
      telemetryStore.connect();
      ws1Rig.latest.serverOpen();
      flushSync();

      // 5. Silent vehicle + 1 Hz shell clock (+5_000 ms with ZERO new WebSocket messages!):
      // Quad #1 (4_000 ms sitl-local threshold) turns stale while Boat #2 (11_000 ms mesh-supervisory) stays fresh.
      vi.advanceTimersByTime(5_000);
      flushSync();

      expect(
        hostEl.querySelector('[data-testid="fleet-nominal-badge"]'),
      ).toBeNull();
      const stalePill = hostEl.querySelector(
        '[data-testid="fleet-health-badge"]',
      );
      expect(stalePill?.getAttribute("data-fleet-state")).toBe("stale");
      expect(stalePill?.textContent).toContain("FLEET STALE (1/2 LAST KNOWN");
      expect(stalePill?.textContent).toContain("5s ago");

      // Stale fault-free ARMED vehicle withholds safety green (severity-warning) and surfaces LAST KNOWN badge.
      const staleArmedBadge = hostEl.querySelector(
        '[data-testid="armed-badge"]',
      );
      expect(staleArmedBadge?.className).toContain("severity-warning");
      expect(staleArmedBadge?.className).not.toContain("severity-success");
      expect(
        hostEl.querySelector('[data-testid="vehicle-freshness-badge"]')
          ?.textContent,
      ).toContain("LAST KNOWN · 5s ago");

      // Switch COP solo tab back to Summary & Pilot workspace to verify LAST KNOWN propagation + 1 Hz age tick.
      (
        hostEl.querySelector(
          '[data-testid="cop-solo-tab-summary"]',
        ) as HTMLButtonElement
      ).click();
      flushSync();
      expect(
        hostEl.querySelector('[data-testid="cop-summary-freshness"]')
          ?.textContent,
      ).toContain("LAST KNOWN · 5s ago");

      vi.advanceTimersByTime(1_000);
      flushSync();
      expect(
        hostEl.querySelector('[data-testid="vehicle-freshness-badge"]')
          ?.textContent,
      ).toContain("LAST KNOWN · 6s ago");

      viewportStore.setWorkspace("pilot");
      flushSync();
      expect(
        hostEl.querySelector('[data-testid="pilot-freshness-chip"]')
          ?.textContent,
      ).toContain("LAST KNOWN · 6s ago");
      viewportStore.setWorkspace("cop");
      flushSync();

      // 6. Active P4 fault retained across WS #1 link_lost + WS #2 disconnect + unchanged snapshot replay (#29).
      const faultObsIso = new Date(Date.now()).toISOString();
      const quad1P4 = buildVehicleStoredPayload(
        1,
        {
          "uxs.domain.kind": "quadcopter",
          "uxs.authority.state": "mcu-failsafe",
          "uxs.link.tier": "sitl-local",
          "uxs.health.armed": true,
          "uxs.health.failsafes": 1,
        },
        { observedAt: faultObsIso },
      );
      ws2Rig.latest.serverMessage(
        JSON.stringify({
          type: "put",
          bucket: "ENTITY_STATES",
          key: quad1.id,
          revision: 4,
          payload: quad1P4,
        }),
      );
      flushSync();

      const liveFaultBadge = hostEl.querySelector(
        '[data-testid="fleet-fault-badge"]',
      );
      expect(liveFaultBadge?.textContent).toContain("1 FLEET FAULT");
      expect(liveFaultBadge?.getAttribute("data-stale-fault")).toBe("false");

      // Advance +3_000 ms, emit WS #1 link_lost for sysid 1, and drop WS #2.
      vi.advanceTimersByTime(3_000);
      ws1Rig.latest.serverMessage(
        JSON.stringify({
          type: "data",
          subject: "bbops.telemetry.events.1",
          timestamp: Date.now(),
          payload: {
            sysid: 1,
            entity_id: quad1.id,
            domain: "quadcopter",
            seq: 9,
            msg_id: 1,
            kind: "link_lost",
            reasons: ["link_timeout"],
            armed: true,
            authority: "mcu-failsafe",
            companion_mode: "teleop-velocity",
            link_tier: "sitl-local",
            flags: {
              clock_sync_locked: true,
              teleop_deadman_clamped: false,
              crsf_link_healthy: false,
              companion_trunk_healthy: false,
            },
            failsafes: 1,
            timestamp: Date.now(),
          },
        }),
      );
      ws2Rig.latest.serverDrop(1006);
      flushSync();
      expect(telemetryStore.malformedFrameCount).toBe(0);

      const staleFaultBadge = hostEl.querySelector(
        '[data-testid="fleet-fault-badge"]',
      );
      expect(staleFaultBadge?.textContent).toContain("1 FLEET FAULT");
      expect(staleFaultBadge?.textContent).toContain("LAST KNOWN");
      expect(staleFaultBadge?.getAttribute("data-stale-fault")).toBe("true");

      (
        hostEl.querySelector(
          '[data-testid="cop-solo-tab-alerts"]',
        ) as HTMLButtonElement
      ).click();
      flushSync();
      expect(
        hostEl.querySelector('[data-testid="cop-exception-stale"]')
          ?.textContent,
      ).toContain("LAST KNOWN");

      // Reconnect WS #2 + replay unchanged frozen faultObsIso snapshot (#29) + synced -> still LAST KNOWN!
      vi.advanceTimersByTime(5_000);
      skgStore.connect();
      ws2Rig.latest.serverOpen();
      ws2Rig.latest.serverMessage(
        JSON.stringify({
          type: "put",
          bucket: "ENTITY_STATES",
          key: quad1.id,
          revision: 4,
          payload: quad1P4,
        }),
      );
      ws2Rig.latest.serverMessage(
        JSON.stringify({
          type: "put",
          bucket: "ENTITY_STATES",
          key: boat2.id,
          revision: 3,
          payload: boat2,
        }),
      );
      ws2Rig.latest.serverMessage(
        JSON.stringify({
          type: "synced",
          bucket: "ENTITY_STATES",
          revision: 4,
        }),
      );
      flushSync();

      expect(
        hostEl
          .querySelector('[data-testid="fleet-fault-badge"]')
          ?.getAttribute("data-stale-fault"),
      ).toBe("true");

      // Fresh recovery put with advancing observedAt restores live FLEET NOMINAL (2)!
      const freshIso = new Date(Date.now()).toISOString();
      const recoveredQuad1 = buildVehicleStoredPayload(
        1,
        {
          "uxs.domain.kind": "quadcopter",
          "uxs.authority.state": "companion-teleop",
          "uxs.link.tier": "sitl-local",
          "uxs.health.armed": true,
          "uxs.health.failsafes": 1,
        },
        { observedAt: freshIso },
      );
      const refreshedBoat2 = buildVehicleStoredPayload(
        2,
        {
          "uxs.domain.kind": "surface-boat",
          "uxs.authority.state": "companion-autonomous",
          "uxs.link.tier": "mesh-supervisory",
          "uxs.health.armed": false,
        },
        { observedAt: freshIso },
      );
      ws2Rig.latest.serverMessage(
        JSON.stringify({
          type: "put",
          bucket: "ENTITY_STATES",
          key: quad1.id,
          revision: 5,
          payload: recoveredQuad1,
        }),
      );
      ws2Rig.latest.serverMessage(
        JSON.stringify({
          type: "put",
          bucket: "ENTITY_STATES",
          key: boat2.id,
          revision: 6,
          payload: refreshedBoat2,
        }),
      );
      flushSync();

      expect(
        hostEl.querySelector('[data-testid="fleet-fault-badge"]'),
      ).toBeNull();
      expect(
        hostEl.querySelector('[data-testid="fleet-nominal-badge"]')
          ?.textContent,
      ).toContain("FLEET NOMINAL (2)");
      expect(
        hostEl.querySelector('[data-testid="vehicle-freshness-badge"]'),
      ).toBeNull();

      // 8. Poisoned-first ownship vehicle with WS #2 open across 60s silence:
      // fault remains visible with LAST KNOWN qualifier across topbar summary, banner,
      // COP Summary & Exceptions, Pilot, and filmstrip, keeping unknown observation age
      // strictly distinct from elapsed decode-error time.
      ws2Rig.latest.serverMessage(
        JSON.stringify({
          type: "delete",
          bucket: "ENTITY_STATES",
          key: quad1.id,
          revision: 7,
        }),
      );
      ws2Rig.latest.serverMessage(
        JSON.stringify({
          type: "delete",
          bucket: "ENTITY_STATES",
          key: boat2.id,
          revision: 8,
        }),
      );
      const poisonedOnlyId = "boxbotix.local.robotics.uxs.vehicle.3";
      ws2Rig.latest.serverMessage(
        JSON.stringify({
          type: "poisoned",
          bucket: "ENTITY_STATES",
          key: poisonedOnlyId,
          revision: 9,
          error: "corrupt uxs triple",
        }),
      );
      flushSync();

      vi.advanceTimersByTime(60_000);
      flushSync();

      const poisonedFaultBadge = hostEl.querySelector(
        '[data-testid="fleet-fault-badge"]',
      ) as HTMLButtonElement;
      expect(poisonedFaultBadge?.getAttribute("data-stale-fault")).toBe("true");
      expect(poisonedFaultBadge?.textContent?.trim()).toBe(
        "! 1 FLEET FAULT (UXS #3 · POISONED) · LAST KNOWN",
      );

      poisonedFaultBadge.click();
      flushSync();

      expect(
        hostEl.querySelector('[data-testid="poisoned-badge"]')?.textContent,
      ).toContain("POISONED: corrupt uxs triple");
      expect(
        hostEl.querySelector('[data-testid="vehicle-freshness-badge"]')
          ?.textContent,
      ).toContain("LAST KNOWN · age unknown (decode error 1m 0s ago)");

      viewportStore.setWorkspace("cop");
      flushSync();
      (
        hostEl.querySelector(
          '[data-testid="cop-solo-tab-summary"]',
        ) as HTMLButtonElement
      ).click();
      flushSync();
      expect(
        hostEl.querySelector('[data-testid="cop-summary-freshness"]')
          ?.textContent,
      ).toContain("LAST KNOWN · age unknown (decode error 1m 0s ago)");

      (
        hostEl.querySelector(
          '[data-testid="cop-solo-tab-alerts"]',
        ) as HTMLButtonElement
      ).click();
      flushSync();
      expect(
        hostEl.querySelector('[data-testid="cop-exception-stale"]')
          ?.textContent,
      ).toContain("LAST KNOWN · age unknown (decode error 1m 0s ago)");

      viewportStore.setWorkspace("pilot");
      flushSync();
      expect(
        hostEl.querySelector('[data-testid="pilot-freshness-chip"]')
          ?.textContent,
      ).toContain("LAST KNOWN · age unknown (decode error 1m 0s ago)");

      const poisonedCard = hostEl.querySelector(
        `[data-testid="filmstrip-item"][data-entity-id="${poisonedOnlyId}"]`,
      );
      expect(poisonedCard?.getAttribute("data-stale")).toBe("true");
      expect(poisonedCard?.textContent).toContain(
        "#3 · POISONED · LAST KNOWN (age unknown · decode error 1m 0s ago)",
      );
    } finally {
      unmount(app);
      hostEl.remove();
      telemetryStore.disconnect();
      skgStore.disconnect();
      vi.useRealTimers();
    }
  });

  // spec: cop-ui / Conventional keyboard traversal, WAI-ARIA tablist navigation, focus management, and character-key shortcut gating
  it("enforces WAI-ARIA roving tablist/tabpanel links, unmounted-child focus recovery, aria-current workspace focus targets, and synchronized character-shortcut disablement (#41)", () => {
    const skgStore = createSkgStore();
    const telemetryStore = createTelemetryStore();
    const quad1 = buildVehicleStoredPayload(1, {
      "uxs.domain.kind": "quadcopter",
      "dc.terms.title": "Hawk 1",
    });
    const faultedRover = buildVehicleStoredPayload(2, {
      "uxs.domain.kind": "rover",
      "dc.terms.title": "Badger 2",
      "uxs.authority.state": "mcu-failsafe",
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
        key: faultedRover.id,
        revision: 2,
        payload: faultedRover,
      }),
    );
    skgStore.ingestRawFrame(
      JSON.stringify({
        type: "synced",
        bucket: "ENTITY_STATES",
        revision: 2,
      }),
    );

    const storageMap = new Map<string, string>();
    const viewportStore = createViewportStore({
      skgStore,
      storage: {
        getItem: (key) => storageMap.get(key) ?? null,
        setItem: (key, val) => {
          storageMap.set(key, val);
        },
      },
    });
    viewportStore.selectEntity(quad1.id);

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
    const mockSearch = createGraphSearchClient({
      fetchImpl: (async () =>
        new Response(
          JSON.stringify({
            entities: [],
            communitySummaries: [],
            relationships: [],
            count: 0,
            durationMs: 1,
          }),
          { status: 200, headers: { "Content-Type": "application/json" } },
        )) as typeof fetch,
    });

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
        undefined,
        [quad1.id, faultedRover.id],
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
      },
    });
    flushSync();

    try {
      // 1. Active workspace nav button exposes aria-current="page" exclusively.
      const copNavBtn = hostEl.querySelector(
        '[data-testid="workspace-tab-cop"]',
      ) as HTMLButtonElement;
      const pilotNavBtn = hostEl.querySelector(
        '[data-testid="workspace-tab-pilot"]',
      ) as HTMLButtonElement;
      const benchNavBtn = hostEl.querySelector(
        '[data-testid="workspace-tab-bench"]',
      ) as HTMLButtonElement;
      const cortexNavBtn = hostEl.querySelector(
        '[data-testid="workspace-tab-cortex"]',
      ) as HTMLButtonElement;
      expect(copNavBtn.getAttribute("aria-current")).toBe("page");
      expect(pilotNavBtn.getAttribute("aria-current")).toBeNull();
      expect(benchNavBtn.getAttribute("aria-current")).toBeNull();
      expect(cortexNavBtn.getAttribute("aria-current")).toBeNull();

      // 2. Fleet inspector tablist enforces single Tab stop (roving tabindex 0/-1), panel links, and arrow/Home/End navigation.
      const summaryTab = hostEl.querySelector(
        '[data-testid="cop-solo-tab-summary"]',
      ) as HTMLButtonElement;
      const alertsTab = hostEl.querySelector(
        '[data-testid="cop-solo-tab-alerts"]',
      ) as HTMLButtonElement;
      const skgTab = hostEl.querySelector(
        '[data-testid="cop-solo-tab-skg"]',
      ) as HTMLButtonElement;

      const tabs = [summaryTab, alertsTab, skgTab];
      const assertPanelLinkageAndSingleExposed = (expectedPanelId: string) => {
        const resolved = tabs.map((tab) => {
          const controlsId = tab.getAttribute("aria-controls");
          expect(controlsId).toBeTruthy();
          const panel = document.getElementById(controlsId!);
          expect(panel).not.toBeNull();
          expect(panel?.getAttribute("role")).toBe("tabpanel");
          expect(panel?.getAttribute("aria-labelledby")).toBe(tab.id);
          return panel as HTMLElement;
        });
        const exposed = resolved.filter((panel) => !panel.hidden);
        expect(exposed.map((panel) => panel.id)).toEqual([expectedPanelId]);
        for (const hiddenPanel of resolved.filter((panel) => panel.hidden)) {
          expect(hiddenPanel.children.length).toBe(0);
          expect(hiddenPanel.getAttribute("tabindex")).toBeNull();
        }
      };

      expect(summaryTab.getAttribute("tabindex")).toBe("0");
      expect(alertsTab.getAttribute("tabindex")).toBe("-1");
      expect(skgTab.getAttribute("tabindex")).toBe("-1");
      assertPanelLinkageAndSingleExposed("cop-solo-panel-summary");
      const summaryPanel = document.getElementById("cop-solo-panel-summary")!;
      const alertsPanel = document.getElementById("cop-solo-panel-alerts")!;
      const skgPanel = document.getElementById("cop-solo-panel-skg")!;
      // Selected quad1 supplies .jump-btn controls inside Summary -> panel itself omits tabindex.
      expect(summaryPanel.getAttribute("tabindex")).toBeNull();

      summaryTab.focus();
      summaryTab.dispatchEvent(
        new KeyboardEvent("keydown", {
          key: "ArrowRight",
          bubbles: true,
          cancelable: true,
        }),
      );
      flushSync();
      expect(document.activeElement).toBe(alertsTab);
      expect(alertsTab.getAttribute("aria-selected")).toBe("true");
      expect(alertsTab.getAttribute("tabindex")).toBe("0");
      expect(summaryTab.getAttribute("tabindex")).toBe("-1");
      assertPanelLinkageAndSingleExposed("cop-solo-panel-alerts");
      expect(alertsPanel.getAttribute("tabindex")).toBeNull();

      // ArrowRight -> skg; ArrowRight -> wraps to summary; ArrowLeft -> wraps to skg; Home -> summary; End -> skg.
      alertsTab.dispatchEvent(
        new KeyboardEvent("keydown", {
          key: "ArrowRight",
          bubbles: true,
          cancelable: true,
        }),
      );
      flushSync();
      expect(document.activeElement).toBe(skgTab);
      expect(skgTab.getAttribute("aria-selected")).toBe("true");
      assertPanelLinkageAndSingleExposed("cop-solo-panel-skg");
      expect(skgPanel.getAttribute("tabindex")).toBeNull();

      skgTab.dispatchEvent(
        new KeyboardEvent("keydown", {
          key: "ArrowRight",
          bubbles: true,
          cancelable: true,
        }),
      );
      flushSync();
      expect(document.activeElement).toBe(summaryTab);
      assertPanelLinkageAndSingleExposed("cop-solo-panel-summary");

      summaryTab.dispatchEvent(
        new KeyboardEvent("keydown", {
          key: "ArrowLeft",
          bubbles: true,
          cancelable: true,
        }),
      );
      flushSync();
      expect(document.activeElement).toBe(skgTab);
      assertPanelLinkageAndSingleExposed("cop-solo-panel-skg");

      skgTab.dispatchEvent(
        new KeyboardEvent("keydown", {
          key: "Home",
          bubbles: true,
          cancelable: true,
        }),
      );
      flushSync();
      expect(document.activeElement).toBe(summaryTab);
      assertPanelLinkageAndSingleExposed("cop-solo-panel-summary");

      summaryTab.dispatchEvent(
        new KeyboardEvent("keydown", {
          key: "End",
          bubbles: true,
          cancelable: true,
        }),
      );
      flushSync();
      expect(document.activeElement).toBe(skgTab);
      assertPanelLinkageAndSingleExposed("cop-solo-panel-skg");

      // 3. Panel focus recovery and read-only tabpanel tab stop (tabindex="0") when no focusable children exist.
      summaryTab.click();
      flushSync();
      const jumpPilotBtn = Array.from(
        hostEl.querySelectorAll<HTMLButtonElement>(".jump-btn"),
      ).find((btn) => btn.textContent?.includes("Vehicle"));
      expect(jumpPilotBtn).toBeDefined();
      jumpPilotBtn!.focus();
      expect(document.activeElement).toBe(jumpPilotBtn);

      // Deselecting entity unmounts jump buttons -> focus recovers to active summary tab, and read-only summary panel becomes tabindex="0".
      viewportStore.selectEntity(null);
      flushSync();
      expect(document.activeElement).toBe(summaryTab);
      expect(summaryPanel.getAttribute("tabindex")).toBe("0");

      // Switch to alerts tab, focus faultedRover's exception button, then resolve fault -> focus recovers to alerts tab and alerts panel becomes tabindex="0".
      alertsTab.click();
      flushSync();
      expect(alertsPanel.getAttribute("tabindex")).toBeNull();
      const exceptionBtn = hostEl.querySelector(
        ".exception-select",
      ) as HTMLButtonElement;
      expect(exceptionBtn).not.toBeNull();
      exceptionBtn.focus();
      expect(document.activeElement).toBe(exceptionBtn);

      const clearedRover = buildVehicleStoredPayload(2, {
        "uxs.domain.kind": "rover",
        "dc.terms.title": "Badger 2",
        "uxs.authority.state": "companion-teleop",
      });
      skgStore.ingestRawFrame(
        JSON.stringify({
          type: "put",
          bucket: "ENTITY_STATES",
          key: clearedRover.id,
          revision: 3,
          payload: clearedRover,
        }),
      );
      flushSync();
      expect(hostEl.querySelector(".exception-select")).toBeNull();
      expect(document.activeElement).toBe(alertsTab);
      expect(alertsPanel.getAttribute("tabindex")).toBe("0");

      // SKG panel with no selected entity also exposes tabindex="0" as a read-only tab stop.
      skgTab.click();
      flushSync();
      expect(skgPanel.getAttribute("tabindex")).toBe("0");

      // Focusing read-only summary panel and then selecting quad1 (mounting focusable buttons and removing panel tabindex) recovers focus cleanly to summaryTab.
      summaryTab.click();
      flushSync();
      summaryPanel.focus();
      expect(document.activeElement).toBe(summaryPanel);
      viewportStore.selectEntity(quad1.id);
      flushSync();
      expect(summaryPanel.getAttribute("tabindex")).toBeNull();
      expect(document.activeElement).toBe(summaryTab);

      pilotNavBtn.focus();
      pilotNavBtn.click();
      flushSync();
      expect(viewportStore.workspace).toBe("pilot");
      expect(pilotNavBtn.getAttribute("aria-current")).toBe("page");
      expect(copNavBtn.getAttribute("aria-current")).toBeNull();
      expect(document.activeElement).toBe(pilotNavBtn);

      // Hotkey G back to Fleet focuses [data-workspace-stage="cop"].
      window.dispatchEvent(
        new KeyboardEvent("keydown", {
          key: "g",
          bubbles: true,
          cancelable: true,
        }),
      );
      flushSync();
      expect(viewportStore.workspace).toBe("cop");
      expect(document.activeElement).toBe(
        hostEl.querySelector('[data-workspace-stage="cop"]'),
      );

      // Unmounting cross-workspace .jump-btn click ("Diagnostics [B]") focuses [data-workspace-stage="bench"].
      const jumpBenchBtn = Array.from(
        hostEl.querySelectorAll<HTMLButtonElement>(".jump-btn"),
      ).find((btn) => btn.textContent?.includes("Diagnostics"));
      expect(jumpBenchBtn).toBeDefined();
      jumpBenchBtn!.focus();
      jumpBenchBtn!.click();
      flushSync();
      expect(viewportStore.workspace).toBe("bench");
      const benchStage = hostEl.querySelector('[data-workspace-stage="bench"]');
      expect(document.activeElement).toBe(benchStage);
      expect(
        hostEl
          .querySelector('[data-testid="bench-oscilloscope"]')
          ?.getAttribute("data-viewport-hotkey-scope"),
      ).toBeNull();

      // Hotkey K -> Analysis focuses [data-workspace-stage="cortex"] and exposes aria-pressed on tier buttons.
      window.dispatchEvent(
        new KeyboardEvent("keydown", {
          key: "k",
          bubbles: true,
          cancelable: true,
        }),
      );
      flushSync();
      expect(viewportStore.workspace).toBe("cortex");
      expect(document.activeElement).toBe(
        hostEl.querySelector('[data-workspace-stage="cortex"]'),
      );
      expect(
        hostEl
          .querySelector('[data-testid="cortex-tier-hybrid"]')
          ?.getAttribute("aria-pressed"),
      ).toBe("true");
      expect(
        hostEl
          .querySelector('[data-testid="cortex-tier-lexical"]')
          ?.getAttribute("aria-pressed"),
      ).toBe("false");

      // 5. Disabling character-only shortcuts hides <kbd>/[X] hints, blocks single-key hotkeys, and keeps Escape + pointer controls active.
      window.dispatchEvent(
        new KeyboardEvent("keydown", {
          key: "g",
          bubbles: true,
          cancelable: true,
        }),
      );
      flushSync();
      viewportStore.tetherEntity(quad1.id);
      flushSync();

      const shortcutsToggle = hostEl.querySelector(
        '[data-testid="character-shortcuts-toggle"]',
      ) as HTMLButtonElement;
      expect(shortcutsToggle.textContent).toContain("Shortcuts: On");
      expect(shortcutsToggle.getAttribute("aria-pressed")).toBe("true");
      expect(hostEl.querySelectorAll("kbd").length).toBeGreaterThan(0);

      const declutterGroup = hostEl.querySelector(
        '[data-testid="declutter-group"]',
      ) as HTMLElement;
      expect(declutterGroup).not.toBeNull();
      const declutterDescIdOn = declutterGroup.getAttribute("aria-describedby");
      expect(declutterDescIdOn).toBeTruthy();
      expect(
        document.getElementById(declutterDescIdOn!)?.textContent?.trim(),
      ).toBe("Press H to cycle HUD declutter levels");
      const declutterButtons = Array.from(
        declutterGroup.querySelectorAll<HTMLButtonElement>("button.chip"),
      );
      expect(declutterButtons).toHaveLength(3);
      for (const btn of declutterButtons) {
        expect(btn.getAttribute("aria-keyshortcuts")).toBeNull();
        expect(btn.getAttribute("title")).toBe(
          `HUD declutter: ${btn.textContent?.trim()}`,
        );
      }

      shortcutsToggle.click();
      flushSync();
      expect(shortcutsToggle.textContent).toContain("Shortcuts: Off");
      expect(shortcutsToggle.getAttribute("aria-pressed")).toBe("false");
      expect(storageMap.get(CHARACTER_SHORTCUTS_STORAGE_KEY)).toBe("false");
      expect(declutterGroup.getAttribute("aria-describedby")).toBeNull();
      expect(
        hostEl.querySelector('[data-testid="declutter-shortcut-hint"]'),
      ).toBeNull();

      // Zero <kbd> hints remain in topbar or filmstrip, and [D]/[B]/[K]/[T]/[C] suffixes & aria-keyshortcuts are removed.
      expect(hostEl.querySelectorAll("kbd").length).toBe(0);
      expect(copNavBtn.getAttribute("aria-keyshortcuts")).toBeNull();
      expect(
        hostEl
          .querySelector('[data-testid="lights-out-toggle"]')
          ?.getAttribute("aria-keyshortcuts"),
      ).toBeNull();
      const jumpTextsWhenOff = Array.from(
        hostEl.querySelectorAll(".jump-btn"),
      ).map((el) => el.textContent?.trim());
      expect(jumpTextsWhenOff).toEqual(["Vehicle", "Diagnostics", "Analysis"]);
      const chipTextsWhenOff = Array.from(
        hostEl.querySelectorAll('[data-testid="viewport-toolbar"] .chip'),
      ).map((el) => el.textContent?.trim());
      expect(chipTextsWhenOff).toContain("Tether");
      expect(chipTextsWhenOff).toContain("Chase");
      expect(chipTextsWhenOff).not.toContain("Tether [T]");
      expect(chipTextsWhenOff).not.toContain("Chase [C]");

      // Single-character hotkeys are ignored while disabled, whereas Escape untethers and pointer clicks still switch workspaces.
      for (const blockedKey of ["d", "b", "k", "l", "2", "t", "c", "h"]) {
        window.dispatchEvent(
          new KeyboardEvent("keydown", {
            key: blockedKey,
            bubbles: true,
            cancelable: true,
          }),
        );
      }
      flushSync();
      expect(viewportStore.workspace).toBe("cop");
      expect(viewportStore.lightsOut).toBe(false);
      expect(viewportStore.selectedEntityId).toBe(quad1.id);
      expect(viewportStore.tetheredEntityId).toBe(quad1.id);

      window.dispatchEvent(
        new KeyboardEvent("keydown", {
          key: "Escape",
          bubbles: true,
          cancelable: true,
        }),
      );
      flushSync();
      expect(viewportStore.tetheredEntityId).toBeNull();

      pilotNavBtn.click();
      flushSync();
      expect(viewportStore.workspace).toBe("pilot");
      expect(
        hostEl.querySelector('[data-testid="pilot-layout-toggle"]')
          ?.textContent,
      ).not.toContain("[M]");

      // Re-enabling restores Shortcuts: On and <kbd>/[M] hints.
      shortcutsToggle.click();
      flushSync();
      expect(shortcutsToggle.textContent).toContain("Shortcuts: On");
      expect(
        hostEl.querySelector('[data-testid="pilot-layout-toggle"]')
          ?.textContent,
      ).toContain("[M]");
      expect(hostEl.querySelectorAll("kbd").length).toBeGreaterThan(0);
    } finally {
      window.removeEventListener("keydown", onKeyDown);
      unmount(app);
      hostEl.remove();
      telemetryStore.disconnect();
      skgStore.disconnect();
    }
  });

  // spec: cop-ui / Session-scoped Diagnostics parameter draft and Analysis investigation continuity across workspace switches
  it("preserves per-vehicle Diagnostics drafts (Heading P Gain 3.20 -> 3.30, baseline, single-vehicle Revert Draft) and Analysis investigation context across Fleet, Vehicle, Diagnostics, and Analysis switches without navigation actuation (#40)", async () => {
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
    const escortId = "boxbotix.local.tak.cop.vessel.escort4";

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
        key: escortId,
        revision: 3,
        payload: {
          id: escortId,
          triples: [
            {
              subject: escortId,
              predicate: "dc.terms.title",
              object: "Escort Vessel 4",
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

    const mockSearch = createGraphSearchClient({
      fetchImpl: (async () =>
        new Response(
          JSON.stringify({
            ok: true,
            verb: "localSearch",
            result: {
              indexed_revision: 71,
              damaged_entities: 0,
              producer: "graph-query",
              data: {
                matches: [
                  {
                    id: quad1.id,
                    label: "Falcon 1",
                    score: 4.1,
                    entity: {},
                  },
                ],
                answer: "Falcon 1 active in sector",
              },
            },
          }),
          { status: 200 },
        )) as typeof fetch,
    });

    const mockGraphql: GraphQLClient = {
      endpoint: "/graph-gateway/graphql",
      entity: vi.fn(),
      entitiesByPrefix: vi.fn(),
      entitiesByPredicate: vi.fn(),
      pathSearch: vi.fn().mockResolvedValue({
        data: {
          entities: [
            { id: quad1.id, depth: 0 },
            { id: escortId, depth: 1 },
          ],
          paths: [
            [
              {
                from: quad1.id,
                predicate: "cop.proximity.near",
                to: escortId,
              },
            ],
          ],
          truncated: false,
        },
        damagedEntities: 0,
      }),
    };

    const viewportStore = createViewportStore({ skgStore });
    viewportStore.selectEntity(quad1.id);
    const onApplyTuning = vi.fn();

    const hostEl = document.createElement("div");
    document.body.appendChild(hostEl);
    const app = mount(WorkspaceShell, {
      target: hostEl,
      props: {
        telemetryStore,
        skgStore,
        viewportStore,
        graphqlClient: mockGraphql,
        searchClient: mockSearch,
        onApplyTuning,
      },
    });
    flushSync();

    try {
      const tab = (mode: "cop" | "pilot" | "bench" | "cortex") =>
        hostEl.querySelector(
          `[data-testid="workspace-tab-${mode}"]`,
        ) as HTMLButtonElement;

      // 1. Open Diagnostics on UXS #1, stage Heading P Gain 3.20 -> 3.30 (1 staged), and stage Max Groundspeed 6.0 -> 9.5 on UXS #2.
      tab("bench").click();
      flushSync();
      expect(hostEl.querySelectorAll("[data-workspace-stage]")).toHaveLength(1);

      const uxs1KpSlider = hostEl.querySelector(
        '[data-param-key="uxs.control.heading_kp"] input[type="range"]',
      ) as HTMLInputElement;
      expect(uxs1KpSlider.value).toBe("3.2");
      uxs1KpSlider.value = "3.3";
      uxs1KpSlider.dispatchEvent(new Event("input", { bubbles: true }));
      flushSync();

      expect(
        hostEl.querySelector('[data-testid="bench-staged-count"]')?.textContent,
      ).toContain("1 staged");
      expect(
        hostEl.querySelector('[data-param-key="uxs.control.heading_kp"]')
          ?.textContent,
      ).toContain("3.30");
      expect(
        hostEl.querySelector('[data-param-key="uxs.control.heading_kp"]')
          ?.textContent,
      ).toContain("(base 3.20)");

      // Switch to UXS #2 via FleetFilmstrip and stage Max Groundspeed 6.0 -> 9.5.
      const filmstripItems = hostEl.querySelectorAll<HTMLButtonElement>(
        '[data-testid="filmstrip-item"]',
      );
      filmstripItems[1]!.click();
      flushSync();
      expect(viewportStore.selectedEntityId).toBe(rover2.id);
      expect(
        hostEl.querySelector('[data-testid="bench-staged-count"]')?.textContent,
      ).toContain("0 staged");

      const uxs2GsSlider = hostEl.querySelector(
        '[data-param-key="uxs.limits.max_groundspeed_mps"] input[type="range"]',
      ) as HTMLInputElement;
      uxs2GsSlider.value = "9.5";
      uxs2GsSlider.dispatchEvent(new Event("input", { bubbles: true }));
      flushSync();
      expect(
        hostEl.querySelector('[data-testid="bench-staged-count"]')?.textContent,
      ).toContain("1 staged");

      // 2. Switch to Analysis, select UXS #1, run Lexical search "Falcon", then round-trip through Fleet -> Vehicle -> Analysis -> Diagnostics.
      filmstripItems[0]!.click();
      flushSync();
      tab("cortex").click();
      flushSync();

      (
        hostEl.querySelector(
          '[data-testid="cortex-tier-lexical"]',
        ) as HTMLButtonElement
      ).click();
      flushSync();
      const cortexInput = hostEl.querySelector(
        '[data-testid="cortex-query-input"]',
      ) as HTMLInputElement;
      cortexInput.value = "Falcon";
      cortexInput.dispatchEvent(new Event("input", { bubbles: true }));
      flushSync();
      (
        hostEl.querySelector(
          '[data-testid="cortex-query-bar"]',
        ) as HTMLFormElement
      ).dispatchEvent(new Event("submit", { bubbles: true, cancelable: true }));

      await vi.waitFor(() => {
        flushSync();
        expect(
          hostEl
            .querySelector('[data-testid="cortex-session-status"]')
            ?.getAttribute("data-status"),
        ).toBe("completed");
      });

      // Switch to Fleet, change selected vehicle to UXS #2, visit Vehicle, then return to Analysis.
      tab("cop").click();
      flushSync();
      filmstripItems[1]!.click();
      flushSync();
      expect(viewportStore.selectedEntityId).toBe(rover2.id);

      tab("pilot").click();
      flushSync();
      tab("cortex").click();
      flushSync();

      // Analysis query text, lexical tier, hit, 2-hop Escort Vessel 4, pinned UXS #1 citations, and earlier-selection provenance all survive!
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
      ).toContain("Escort Vessel 4");
      expect(
        hostEl
          .querySelector('[data-testid="cortex-result-provenance"]')
          ?.getAttribute("data-earlier-selection"),
      ).toBe("true");
      expect(
        hostEl.querySelector('[data-testid="cortex-citation-list"]')
          ?.textContent,
      ).toContain("Falcon 1");

      // While Analysis IS mounted, direct operator selection (e.g., filmstrip / [1]-[9]) updates inspectedEntityId immediately!
      filmstripItems[0]!.click();
      flushSync();
      filmstripItems[1]!.click();
      flushSync();
      expect(
        hostEl.querySelector('[data-testid="cortex-citation-list"]')
          ?.textContent,
      ).toContain("Vanguard 2");

      // 3. Return to Diagnostics: both UXS #2 (9.5 m/s, 1 staged) and UXS #1 (3.30 1/s, base 3.20, 1 staged) survived!
      tab("bench").click();
      flushSync();
      expect(
        hostEl.querySelector('[data-testid="bench-staged-count"]')?.textContent,
      ).toContain("1 staged");
      expect(
        hostEl.querySelector(
          '[data-param-key="uxs.limits.max_groundspeed_mps"]',
        )?.textContent,
      ).toContain("9.5");
      expect(
        hostEl.querySelector(
          '[data-param-key="uxs.limits.max_groundspeed_mps"]',
        )?.textContent,
      ).toContain("(base 6.0)");

      filmstripItems[0]!.click();
      flushSync();
      expect(
        hostEl.querySelector('[data-testid="bench-staged-count"]')?.textContent,
      ).toContain("1 staged");
      expect(
        hostEl.querySelector('[data-param-key="uxs.control.heading_kp"]')
          ?.textContent,
      ).toContain("3.30");
      expect(
        hostEl.querySelector('[data-param-key="uxs.control.heading_kp"]')
          ?.textContent,
      ).toContain("(base 3.20)");

      // Revert UXS #1 ONLY, switch through Fleet and back to Diagnostics, and verify UXS #1 stays 0 staged while UXS #2 stays 1 staged!
      (
        hostEl.querySelector(
          '[data-testid="bench-tuning-revert"]',
        ) as HTMLButtonElement
      ).click();
      flushSync();
      expect(
        hostEl.querySelector('[data-testid="bench-staged-count"]')?.textContent,
      ).toContain("0 staged");

      tab("cop").click();
      flushSync();
      tab("bench").click();
      flushSync();
      expect(
        hostEl.querySelector('[data-testid="bench-staged-count"]')?.textContent,
      ).toContain("0 staged");
      expect(
        hostEl.querySelector('[data-param-key="uxs.control.heading_kp"]')
          ?.textContent,
      ).toContain("3.20");
      expect(
        hostEl.querySelector('[data-param-key="uxs.control.heading_kp"]')
          ?.textContent,
      ).not.toContain("(base 3.20)");

      filmstripItems[1]!.click();
      flushSync();
      expect(
        hostEl.querySelector('[data-testid="bench-staged-count"]')?.textContent,
      ).toContain("1 staged");
      expect(
        hostEl.querySelector(
          '[data-param-key="uxs.limits.max_groundspeed_mps"]',
        )?.textContent,
      ).toContain("9.5");

      expect(onApplyTuning).not.toHaveBeenCalled();
    } finally {
      unmount(app);
      hostEl.remove();
    }
  });
});
