// SPDX-License-Identifier: MIT
// Copyright (c) 2026 boxbotix

import { flushSync, mount, unmount } from "svelte";
import { describe, expect, it } from "vitest";
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
  MockWebSocketRig,
  buildVehicleStoredPayload,
} from "../stores/test-helpers";
import { createViewportStore } from "../stores/viewport.svelte";
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

    // 3. Selecting ExternalContactEntityState in Pilot withholds VehicleHud, authority/armed badges, and guarded action slot!
    viewportStore.selectEntity(contactId);
    flushSync();
    expect(
      hostEl.querySelector('[data-testid="pilot-contact-isolation"]'),
    ).not.toBeNull();
    expect(hostEl.querySelector('[data-testid="vehicle-hud"]')).toBeNull();
    expect(
      hostEl.querySelector('[data-testid="vehicle-hud-standby"]'),
    ).toBeNull();
    expect(hostEl.querySelector('[data-testid="authority-badge"]')).toBeNull();
    expect(hostEl.querySelector('[data-testid="armed-badge"]')).toBeNull();
    expect(
      hostEl.querySelector('[data-testid="pilot-guarded-actions"]'),
    ).toBeNull();

    unmount(app);
    hostEl.remove();
  });
});
