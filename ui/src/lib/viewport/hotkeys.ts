// SPDX-License-Identifier: MIT
// Copyright (c) 2026 boxbotix

import {
  DECLUTTER_LEVELS,
  type DeclutterLevel,
  type ViewportStore,
} from "../stores/viewport.svelte";

export interface HotkeyTargetLike {
  readonly tagName?: string;
  readonly isContentEditable?: boolean;
  getAttribute?(name: string): string | null;
}

export interface HotkeyEventLike {
  readonly key: string;
  readonly ctrlKey?: boolean;
  readonly metaKey?: boolean;
  readonly altKey?: boolean;
  readonly shiftKey?: boolean;
  readonly repeat?: boolean;
  readonly defaultPrevented?: boolean;
  readonly target?: HotkeyTargetLike | null;
  preventDefault?(): void;
}

export const VIEWPORT_HOTKEY_SCOPE_ATTR = "data-viewport-hotkey-scope";

const EDITABLE_TAGS = new Set(["INPUT", "TEXTAREA", "SELECT"]);
const INTERACTIVE_CONTROL_TAGS = new Set([
  "BUTTON",
  "A",
  "INPUT",
  "TEXTAREA",
  "SELECT",
  "SUMMARY",
]);

export function isEditableHotkeyTarget(
  target: HotkeyTargetLike | null | undefined,
): boolean {
  if (!target) {
    return false;
  }
  if (target.isContentEditable === true) {
    return true;
  }
  if (typeof target.getAttribute === "function") {
    const contentEditableAttr = target.getAttribute("contenteditable");
    if (
      contentEditableAttr !== null &&
      contentEditableAttr.toLowerCase() !== "false"
    ) {
      return true;
    }
  }
  if (typeof target.tagName === "string") {
    return EDITABLE_TAGS.has(target.tagName.toUpperCase());
  }
  return false;
}

export function isViewportScopeTarget(
  target: HotkeyTargetLike | null | undefined,
): boolean {
  if (!target || isEditableHotkeyTarget(target)) {
    return false;
  }
  if (
    typeof target.tagName === "string" &&
    INTERACTIVE_CONTROL_TAGS.has(target.tagName.toUpperCase())
  ) {
    return false;
  }
  return (
    typeof target.getAttribute === "function" &&
    target.getAttribute(VIEWPORT_HOTKEY_SCOPE_ATTR) === "true"
  );
}

export function nextDeclutterLevel(current: DeclutterLevel): DeclutterLevel {
  const idx = DECLUTTER_LEVELS.indexOf(current);
  if (idx < 0) {
    return "none";
  }
  return DECLUTTER_LEVELS[(idx + 1) % DECLUTTER_LEVELS.length] ?? "none";
}

/**
 * Handles view-only workspace and COP tactical viewport hotkeys:
 * - G: switch to Fleet workspace ("cop")
 * - D: switch to Vehicle workspace ("pilot")
 * - B: switch to Diagnostics workspace ("bench")
 * - K: switch to Analysis workspace ("cortex")
 * - M: toggle Vehicle layout ("map" <-> "split") when in Vehicle workspace
 * - Tab: toggle Lights-Out chrome mode ONLY when an interactive viewport surface
 *   (`[data-viewport-hotkey-scope="true"]`) is explicitly focused, preserving
 *   normal Tab traversal from page entry and between toolbar/form controls
 * - 1..9: select nth entity in FleetFilmstrip
 * - T: toggle tether on selected entity (or untether when already tethered)
 * - C: toggle chase heading lock (auto-tethers selected entity if untethered)
 * - H: cycle declutter level (none -> minimal -> tactical -> none)
 * - Escape: untether camera and clear chase lock
 *
 * Ignores editable form/contenteditable targets, repeated keydowns, and
 * Ctrl/Meta/Alt/Shift modifier chords. Never triggers vehicle actuation.
 */
export function handleViewportHotkey(
  event: HotkeyEventLike,
  viewport: ViewportStore,
  onSyncHash?: (hash: string) => void,
  filmstripEntityIds?: readonly string[],
): boolean {
  if (
    event.defaultPrevented ||
    event.repeat ||
    event.ctrlKey ||
    event.metaKey ||
    event.altKey ||
    event.shiftKey
  ) {
    return false;
  }
  if (isEditableHotkeyTarget(event.target)) {
    return false;
  }

  const normalizedKey =
    event.key.length === 1 ? event.key.toLowerCase() : event.key;
  let mutated = false;

  switch (normalizedKey) {
    case "g": {
      viewport.setWorkspace("cop");
      mutated = true;
      break;
    }
    case "d": {
      viewport.setWorkspace("pilot");
      mutated = true;
      break;
    }
    case "b": {
      viewport.setWorkspace("bench");
      mutated = true;
      break;
    }
    case "k": {
      viewport.setWorkspace("cortex");
      mutated = true;
      break;
    }
    case "m": {
      if (viewport.workspace === "pilot") {
        viewport.togglePilotLayout();
        mutated = true;
      }
      break;
    }
    case "Tab": {
      if (isViewportScopeTarget(event.target)) {
        viewport.setLightsOut(!viewport.lightsOut);
        mutated = true;
      }
      break;
    }
    case "1":
    case "2":
    case "3":
    case "4":
    case "5":
    case "6":
    case "7":
    case "8":
    case "9": {
      if (filmstripEntityIds && filmstripEntityIds.length > 0) {
        const index = Number(normalizedKey) - 1;
        const entityId = filmstripEntityIds[index];
        if (typeof entityId === "string" && entityId.length > 0) {
          viewport.selectEntity(entityId);
          mutated = true;
        }
      }
      break;
    }
    case "t": {
      if (viewport.tetheredEntityId !== null) {
        viewport.tetherEntity(null);
        mutated = true;
      } else if (viewport.selectedEntityId !== null) {
        viewport.tetherEntity(viewport.selectedEntityId);
        mutated = true;
      }
      break;
    }
    case "c": {
      if (viewport.chaseLock) {
        viewport.setChaseLock(false);
        mutated = true;
      } else if (
        viewport.tetheredEntityId !== null ||
        viewport.selectedEntityId !== null
      ) {
        viewport.setChaseLock(true);
        mutated = true;
      }
      break;
    }
    case "h": {
      viewport.setDeclutter(nextDeclutterLevel(viewport.declutter));
      mutated = true;
      break;
    }
    case "Escape": {
      if (viewport.tetheredEntityId !== null || viewport.chaseLock) {
        viewport.tetherEntity(null);
        mutated = true;
      }
      break;
    }
    default:
      return false;
  }

  if (mutated) {
    event.preventDefault?.();
    onSyncHash?.(viewport.toUrlHash());
  }
  return mutated;
}
