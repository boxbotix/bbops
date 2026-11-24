// SPDX-License-Identifier: MIT
// Copyright (c) 2026 boxbotix

import { describe, expect, it } from "vitest";
import { createViewportStore } from "../stores/viewport.svelte";
import { handleViewportHotkey } from "./hotkeys";

describe("hotkeys.ts — view-only workspace, pilot swap, lights-out, and filmstrip hotkeys", () => {
  // spec: cop-ui / Task-modal 4-workspace shell, invariant safety topbar, and Fleet Filmstrip
  it("switches workspaces via G/D/B/K, swaps Pilot layout via M only in Pilot, toggles Lights-Out via scoped Tab, and selects 1-9 filmstrip entities", () => {
    const viewport = createViewportStore();
    const hashes: string[] = [];
    const filmstrip = [
      "boxbotix.local.robotics.uxs.vehicle.1",
      "boxbotix.local.robotics.uxs.vehicle.2",
      "boxbotix.local.tak.cop.contact.harbor7",
    ];
    const scopedViewportTarget = {
      tagName: "CANVAS",
      getAttribute: (name: string) =>
        name === "data-viewport-hotkey-scope" ? "true" : null,
    };

    let prevented = 0;
    const press = (
      key: string,
      target?: {
        tagName?: string;
        getAttribute?: (name: string) => string | null;
      },
    ) =>
      handleViewportHotkey(
        {
          key,
          target,
          preventDefault: () => {
            prevented += 1;
          },
        },
        viewport,
        (hash) => hashes.push(hash),
        filmstrip,
      );

    // M outside Pilot is ignored.
    expect(press("m")).toBe(false);
    expect(viewport.pilotLayout).toBe("map");
    expect(prevented).toBe(0);

    // D -> Pilot, M -> Split, M -> Map
    expect(press("d")).toBe(true);
    expect(viewport.workspace).toBe("pilot");
    expect(press("m")).toBe(true);
    expect(viewport.pilotLayout).toBe("split");
    expect(press("M")).toBe(true);
    expect(viewport.pilotLayout).toBe("map");

    // B -> Bench, K -> Cortex, G -> COP
    expect(press("b")).toBe(true);
    expect(viewport.workspace).toBe("bench");
    expect(press("k")).toBe(true);
    expect(viewport.workspace).toBe("cortex");
    expect(press("g")).toBe(true);
    expect(viewport.workspace).toBe("cop");

    // Unscoped Tab from page entry (BODY / undefined) and toolbar BUTTON does NOT intercept focus traversal.
    expect(press("Tab")).toBe(false);
    expect(press("Tab", { tagName: "BODY" })).toBe(false);
    expect(press("Tab", { tagName: "BUTTON" })).toBe(false);
    expect(viewport.lightsOut).toBe(false);
    expect(prevented).toBe(6);

    // Scoped Tab on an explicitly focused viewport surface toggles Lights-Out chrome.
    expect(press("Tab", scopedViewportTarget)).toBe(true);
    expect(viewport.lightsOut).toBe(true);
    expect(press("Tab", scopedViewportTarget)).toBe(true);
    expect(viewport.lightsOut).toBe(false);

    // 1..3 select filmstrip items; out-of-range 9 is ignored.
    expect(press("2")).toBe(true);
    expect(viewport.selectedEntityId).toBe(filmstrip[1]);
    expect(press("3")).toBe(true);
    expect(viewport.selectedEntityId).toBe(filmstrip[2]);
    expect(press("9")).toBe(false);
    expect(viewport.selectedEntityId).toBe(filmstrip[2]);

    expect(prevented).toBe(10);
    expect(hashes.at(-1)).toBe("#sel=boxbotix.local.tak.cop.contact.harbor7");
  });

  // spec: cop-ui / Task-modal 4-workspace shell, invariant safety topbar, and Fleet Filmstrip
  it("suppresses single-key and Tab hotkeys inside editable inputs, interactive buttons/links, contenteditable elements, repeats, and modifier chords", () => {
    const viewport = createViewportStore();
    const filmstrip = ["boxbotix.local.robotics.uxs.vehicle.1"];

    const blockedEvents = [
      { key: "d", target: { tagName: "INPUT" } },
      { key: "b", target: { tagName: "TEXTAREA" } },
      { key: "k", target: { tagName: "SELECT" } },
      { key: "Tab", target: { tagName: "input" } },
      {
        key: "Tab",
        target: {
          tagName: "BUTTON",
          getAttribute: (name: string) =>
            name === "data-viewport-hotkey-scope" ? "true" : null,
        },
      },
      {
        key: "Tab",
        target: {
          tagName: "A",
          getAttribute: (name: string) =>
            name === "data-viewport-hotkey-scope" ? "true" : null,
        },
      },
      { key: "1", target: { isContentEditable: true } },
      {
        key: "d",
        target: {
          tagName: "DIV",
          getAttribute: (name: string) =>
            name === "contenteditable" ? "true" : null,
        },
      },
      { key: "d", ctrlKey: true },
      { key: "b", metaKey: true },
      { key: "k", altKey: true },
      { key: "g", shiftKey: true },
      {
        key: "Tab",
        shiftKey: true,
        target: {
          tagName: "CANVAS",
          getAttribute: (name: string) =>
            name === "data-viewport-hotkey-scope" ? "true" : null,
        },
      },
      { key: "d", repeat: true },
      { key: "d", defaultPrevented: true },
    ];

    for (const ev of blockedEvents) {
      expect(handleViewportHotkey(ev, viewport, undefined, filmstrip)).toBe(
        false,
      );
    }

    expect(viewport.workspace).toBe("cop");
    expect(viewport.lightsOut).toBe(false);
    expect(viewport.selectedEntityId).toBeNull();
  });
});
