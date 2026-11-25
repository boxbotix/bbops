// SPDX-License-Identifier: MIT
// Copyright (c) 2026 boxbotix

import { describe, expect, it } from "vitest";
import { createSeededRng } from "../stores/test-helpers";
import {
  createViewportStore,
  DECLUTTER_LEVELS,
  defaultViewportSnapshot,
  serializeViewportHash,
  type ViewportSnapshot,
} from "../stores/viewport.svelte";
import { handleViewportHotkey, type HotkeyTargetLike } from "./hotkeys";

describe("hotkeys.ts — view-only workspace, pilot swap, lights-out, camera, and filmstrip hotkeys", () => {
  // spec: cop-ui / Conventional keyboard traversal, WAI-ARIA tablist navigation, focus management, and character-key shortcut gating
  // spec: cop-ui / Task-modal 4-workspace shell, invariant safety topbar, and Fleet Filmstrip
  it("switches workspaces via G/D/B/K, swaps Pilot layout via M only in Pilot, toggles Lights-Out via L, never intercepts Tab/Shift+Tab, and selects 1-9 filmstrip entities", () => {
    const viewport = createViewportStore();
    const hashes: string[] = [];
    const filmstrip = [
      "boxbotix.local.robotics.uxs.vehicle.1",
      "boxbotix.local.robotics.uxs.vehicle.2",
      "boxbotix.local.tak.cop.contact.harbor7",
    ];

    let prevented = 0;
    const press = (key: string, target?: HotkeyTargetLike, shiftKey = false) =>
      handleViewportHotkey(
        {
          key,
          shiftKey,
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

    // Tab and Shift+Tab NEVER intercept focus traversal anywhere (including focused canvases and role="application").
    expect(press("Tab")).toBe(false);
    expect(press("Tab", { tagName: "BODY" })).toBe(false);
    expect(press("Tab", { tagName: "BUTTON" })).toBe(false);
    expect(press("Tab", { tagName: "CANVAS" })).toBe(false);
    expect(press("Tab", { tagName: "CANVAS" }, true)).toBe(false);
    expect(
      press("Tab", {
        tagName: "DIV",
        getAttribute: (name) => (name === "role" ? "application" : null),
      }),
    ).toBe(false);
    expect(viewport.lightsOut).toBe(false);
    expect(prevented).toBe(6);

    // Dedicated L shortcut toggles Lights-Out chrome deterministically.
    expect(press("l", { tagName: "CANVAS" })).toBe(true);
    expect(viewport.lightsOut).toBe(true);
    expect(press("L")).toBe(true);
    expect(viewport.lightsOut).toBe(false);

    // 1..3 select filmstrip items; out-of-range 9 is ignored.
    expect(press("2")).toBe(true);
    expect(viewport.selectedEntityId).toBe(filmstrip[1]);
    expect(press("3")).toBe(true);
    expect(viewport.selectedEntityId).toBe(filmstrip[2]);
    expect(press("9")).toBe(false);
    expect(viewport.selectedEntityId).toBe(filmstrip[2]);

    // T, C, H, and Escape drive camera tether, chase lock, and HUD declutter.
    expect(press("t")).toBe(true);
    expect(viewport.tetheredEntityId).toBe(filmstrip[2]);
    expect(press("c")).toBe(true);
    expect(viewport.chaseLock).toBe(true);
    expect(press("h")).toBe(true);
    expect(viewport.declutter).toBe("minimal");
    expect(press("Escape")).toBe(true);
    expect(viewport.tetheredEntityId).toBeNull();
    expect(viewport.chaseLock).toBe(false);

    expect(prevented).toBe(14);
    expect(hashes.at(-1)).toBe(
      "#sel=boxbotix.local.tak.cop.contact.harbor7&declutter=minimal",
    );
  });

  // spec: cop-ui / Conventional keyboard traversal, WAI-ARIA tablist navigation, focus management, and character-key shortcut gating
  // spec: cop-ui / Renderer-agnostic ViewportAdapter, surface-vs-safety-HUD layer split, and multi-domain tactical canvas
  it("suppresses hotkeys on native/ARIA editable controls and gates single-character keys while keeping Escape active when characterShortcutsEnabled is false", () => {
    const viewport = createViewportStore();
    const filmstrip = ["boxbotix.local.robotics.uxs.vehicle.1"];

    const ariaRoleTarget = (role: string): HotkeyTargetLike => ({
      tagName: "DIV",
      getAttribute: (name) => (name === "role" ? role : null),
    });

    const nestedComboboxChild: HotkeyTargetLike = {
      tagName: "SPAN",
      getAttribute: () => null,
      closest: (selector) =>
        selector.includes('[role="combobox"]') ? { tagName: "DIV" } : null,
    };

    const blockedEvents = [
      { key: "d", target: { tagName: "INPUT" } },
      { key: "b", target: { tagName: "TEXTAREA" } },
      { key: "k", target: { tagName: "SELECT" } },
      { key: "l", target: { tagName: "input" } },
      { key: "1", target: { isContentEditable: true } },
      {
        key: "d",
        target: {
          tagName: "DIV",
          getAttribute: (name: string) =>
            name === "contenteditable" ? "plaintext-only" : null,
        },
      },
      { key: "g", target: ariaRoleTarget("textbox") },
      { key: "d", target: ariaRoleTarget("searchbox") },
      { key: "b", target: ariaRoleTarget("combobox") },
      { key: "k", target: ariaRoleTarget("spinbutton") },
      { key: "h", target: ariaRoleTarget("slider") },
      { key: "t", target: nestedComboboxChild },
      { key: "Escape", target: { tagName: "INPUT" } },
      { key: "Escape", target: ariaRoleTarget("searchbox") },
      { key: "d", ctrlKey: true },
      { key: "b", metaKey: true },
      { key: "k", altKey: true },
      { key: "l", shiftKey: true },
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

    // Select & tether + chase-lock an entity, then disable character-only shortcuts.
    viewport.selectEntity(filmstrip[0]!);
    viewport.setChaseLock(true);
    expect(viewport.tetheredEntityId).toBe(filmstrip[0]);
    expect(viewport.chaseLock).toBe(true);

    viewport.setCharacterShortcutsEnabled(false);
    let preventedWhenOff = 0;
    const syncedWhenOff: string[] = [];
    for (const key of ["g", "d", "b", "k", "m", "l", "L", "1", "t", "c", "h"]) {
      expect(
        handleViewportHotkey(
          {
            key,
            target: { tagName: "CANVAS" },
            preventDefault: () => {
              preventedWhenOff += 1;
            },
          },
          viewport,
          (hash) => syncedWhenOff.push(hash),
          filmstrip,
        ),
      ).toBe(false);
    }
    expect(preventedWhenOff).toBe(0);
    expect(syncedWhenOff).toHaveLength(0);
    expect(viewport.workspace).toBe("cop");
    expect(viewport.lightsOut).toBe(false);
    expect(viewport.tetheredEntityId).toBe(filmstrip[0]);
    expect(viewport.chaseLock).toBe(true);

    // Non-character Escape untether remains active even while character shortcuts are disabled.
    expect(
      handleViewportHotkey(
        {
          key: "Escape",
          target: { tagName: "CANVAS" },
          preventDefault: () => {
            preventedWhenOff += 1;
          },
        },
        viewport,
        (hash) => syncedWhenOff.push(hash),
        filmstrip,
      ),
    ).toBe(true);
    expect(preventedWhenOff).toBe(1);
    expect(viewport.tetheredEntityId).toBeNull();
    expect(viewport.chaseLock).toBe(false);
    expect(syncedWhenOff).toEqual([
      "#sel=boxbotix.local.robotics.uxs.vehicle.1",
    ]);
  });

  // spec: cop-ui / Conventional keyboard traversal, WAI-ARIA tablist navigation, focus management, and character-key shortcut gating
  // spec: cop-ui / Renderer-agnostic ViewportAdapter, surface-vs-safety-HUD layer split, and multi-domain tactical canvas
  it("matches an independent reference model across seeded hotkey, editable-target, and shortcut-disable histories", () => {
    const rng = createSeededRng(0x41a11925);
    const viewport = createViewportStore();
    const filmstrip = [
      "boxbotix.local.robotics.uxs.vehicle.1",
      "boxbotix.local.robotics.uxs.vehicle.2",
      "boxbotix.local.tak.cop.contact.alpha",
    ];

    let refSnap: ViewportSnapshot = defaultViewportSnapshot();
    let refShortcutsEnabled = true;

    let sawDisabledCharacterBlocked = 0;
    let sawEscapeWhileDisabledUntether = 0;
    let sawAriaWidgetSuppressed = 0;
    let sawTabNeverHandled = 0;
    let sawLightsOutToggled = 0;
    let sawDeclutterWrap = 0;
    let sawChaseAutoTether = 0;

    const candidateKeys = [
      "Tab",
      "g",
      "d",
      "b",
      "k",
      "m",
      "l",
      "1",
      "2",
      "9",
      "t",
      "c",
      "h",
      "Escape",
      "ArrowRight",
    ] as const;

    const targets: ReadonlyArray<{
      editable: boolean;
      ariaWidget: boolean;
      target: HotkeyTargetLike | undefined;
    }> = [
      { editable: false, ariaWidget: false, target: undefined },
      {
        editable: false,
        ariaWidget: false,
        target: { tagName: "CANVAS" },
      },
      {
        editable: false,
        ariaWidget: false,
        target: { tagName: "BUTTON" },
      },
      {
        editable: true,
        ariaWidget: false,
        target: { tagName: "INPUT" },
      },
      {
        editable: true,
        ariaWidget: true,
        target: {
          tagName: "DIV",
          getAttribute: (name) => (name === "role" ? "slider" : null),
        },
      },
      {
        editable: true,
        ariaWidget: true,
        target: {
          tagName: "SPAN",
          getAttribute: () => null,
          closest: (sel) =>
            sel.includes('[role="searchbox"]') ? { tagName: "DIV" } : null,
        },
      },
    ];

    for (let step = 0; step < 800; step += 1) {
      if (rng() < 0.18) {
        const nextEnabled: boolean =
          rng() < 0.5 ? !refShortcutsEnabled : rng() < 0.55;
        viewport.setCharacterShortcutsEnabled(nextEnabled);
        refShortcutsEnabled = nextEnabled;
        expect(viewport.characterShortcutsEnabled).toBe(refShortcutsEnabled);
        expect(viewport.snapshot()).toEqual(refSnap);
        continue;
      }

      const rawKey =
        candidateKeys[Math.floor(rng() * candidateKeys.length)] ?? "Tab";
      const key =
        rawKey.length === 1 && rng() < 0.35 ? rawKey.toUpperCase() : rawKey;
      const targetEntry = targets[Math.floor(rng() * targets.length)]!;
      const hasModifier = rng() < 0.12;
      const isRepeat = rng() < 0.06;

      let prevented = 0;
      let syncedHash: string | null = null;
      const handled = handleViewportHotkey(
        {
          key,
          ctrlKey: hasModifier,
          repeat: isRepeat,
          target: targetEntry.target,
          preventDefault: () => {
            prevented += 1;
          },
        },
        viewport,
        (hash) => {
          syncedHash = hash;
        },
        filmstrip,
      );

      // Compute independent reference transition.
      let expectedHandled = false;
      const lower = key.toLowerCase();
      if (rawKey === "Tab") {
        sawTabNeverHandled += 1;
      }

      if (!hasModifier && !isRepeat && !targetEntry.editable) {
        if (key === "Escape") {
          if (refSnap.tetheredEntityId !== null || refSnap.chaseLock) {
            if (!refShortcutsEnabled) {
              sawEscapeWhileDisabledUntether += 1;
            }
            refSnap = {
              ...refSnap,
              tetheredEntityId: null,
              chaseLock: false,
            };
            expectedHandled = true;
          }
        } else if (!refShortcutsEnabled) {
          if (rawKey !== "Tab" && rawKey !== "ArrowRight") {
            sawDisabledCharacterBlocked += 1;
          }
        } else if (lower === "g") {
          refSnap = { ...refSnap, workspace: "cop" };
          expectedHandled = true;
        } else if (lower === "d") {
          refSnap = { ...refSnap, workspace: "pilot" };
          expectedHandled = true;
        } else if (lower === "b") {
          refSnap = { ...refSnap, workspace: "bench" };
          expectedHandled = true;
        } else if (lower === "k") {
          refSnap = { ...refSnap, workspace: "cortex" };
          expectedHandled = true;
        } else if (lower === "m") {
          if (refSnap.workspace === "pilot") {
            refSnap = {
              ...refSnap,
              pilotLayout: refSnap.pilotLayout === "map" ? "split" : "map",
            };
            expectedHandled = true;
          }
        } else if (lower === "l") {
          refSnap = { ...refSnap, lightsOut: !refSnap.lightsOut };
          sawLightsOutToggled += 1;
          expectedHandled = true;
        } else if (lower >= "1" && lower <= "9") {
          const idx = Number.parseInt(lower, 10) - 1;
          const entityId = filmstrip[idx];
          if (entityId) {
            refSnap = { ...refSnap, selectedEntityId: entityId };
            expectedHandled = true;
          }
        } else if (lower === "t") {
          if (refSnap.tetheredEntityId !== null) {
            refSnap = {
              ...refSnap,
              tetheredEntityId: null,
              chaseLock: false,
            };
            expectedHandled = true;
          } else if (refSnap.selectedEntityId !== null) {
            refSnap = {
              ...refSnap,
              tetheredEntityId: refSnap.selectedEntityId,
            };
            expectedHandled = true;
          }
        } else if (lower === "c") {
          if (refSnap.chaseLock) {
            refSnap = { ...refSnap, chaseLock: false };
            expectedHandled = true;
          } else if (refSnap.tetheredEntityId !== null) {
            refSnap = { ...refSnap, chaseLock: true };
            expectedHandled = true;
          } else if (refSnap.selectedEntityId !== null) {
            sawChaseAutoTether += 1;
            refSnap = {
              ...refSnap,
              tetheredEntityId: refSnap.selectedEntityId,
              chaseLock: true,
            };
            expectedHandled = true;
          }
        } else if (lower === "h") {
          const curIdx = DECLUTTER_LEVELS.indexOf(refSnap.declutter);
          const nextIdx = (curIdx + 1) % DECLUTTER_LEVELS.length;
          if (nextIdx === 0) {
            sawDeclutterWrap += 1;
          }
          refSnap = {
            ...refSnap,
            declutter: DECLUTTER_LEVELS[nextIdx] ?? "none",
          };
          expectedHandled = true;
        }
      } else if (
        !hasModifier &&
        !isRepeat &&
        targetEntry.ariaWidget &&
        rawKey !== "Tab" &&
        rawKey !== "ArrowRight"
      ) {
        sawAriaWidgetSuppressed += 1;
      }

      expect(handled).toBe(expectedHandled);
      expect(prevented).toBe(expectedHandled ? 1 : 0);
      expect(viewport.snapshot()).toEqual(refSnap);
      expect(syncedHash).toBe(
        expectedHandled ? serializeViewportHash(refSnap) : null,
      );
    }

    expect(sawTabNeverHandled).toBeGreaterThan(0);
    expect(sawDisabledCharacterBlocked).toBeGreaterThan(0);
    expect(sawEscapeWhileDisabledUntether).toBeGreaterThan(0);
    expect(sawAriaWidgetSuppressed).toBeGreaterThan(0);
    expect(sawLightsOutToggled).toBeGreaterThan(0);
    expect(sawDeclutterWrap).toBeGreaterThan(0);
    expect(sawChaseAutoTether).toBeGreaterThan(0);
  });
});
