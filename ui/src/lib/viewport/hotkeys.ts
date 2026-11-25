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
  closest?(selector: string): unknown;
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

const EDITABLE_TAGS = new Set(["INPUT", "TEXTAREA", "SELECT"]);
const EDITABLE_ARIA_ROLES = new Set([
  "textbox",
  "searchbox",
  "combobox",
  "spinbutton",
  "slider",
]);
const EDITABLE_CLOSEST_SELECTOR = [
  "input",
  "textarea",
  "select",
  '[contenteditable=""]',
  '[contenteditable="true"]',
  '[contenteditable="plaintext-only"]',
  '[role="textbox"]',
  '[role="searchbox"]',
  '[role="combobox"]',
  '[role="spinbutton"]',
  '[role="slider"]',
].join(", ");

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
      contentEditableAttr.trim().toLowerCase() !== "false"
    ) {
      return true;
    }
    const roleAttr = target.getAttribute("role");
    if (
      roleAttr !== null &&
      EDITABLE_ARIA_ROLES.has(roleAttr.trim().toLowerCase())
    ) {
      return true;
    }
  }
  if (
    typeof target.tagName === "string" &&
    EDITABLE_TAGS.has(target.tagName.toUpperCase())
  ) {
    return true;
  }
  if (typeof target.closest === "function") {
    return target.closest(EDITABLE_CLOSEST_SELECTOR) !== null;
  }
  return false;
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
 * - L: toggle Lights-Out chrome mode
 * - 1..9: select nth entity in FleetFilmstrip
 * - T: toggle tether on selected entity (or untether when already tethered)
 * - C: toggle chase heading lock (auto-tethers selected entity if untethered)
 * - H: cycle declutter level (none -> minimal -> tactical -> none)
 * - Escape: untether camera and clear chase lock (remains active when
 *   character-only shortcuts are disabled)
 *
 * Never intercepts Tab or Shift+Tab focus traversal, ignores editable form,
 * contenteditable, and ARIA widget targets, ignores repeated keydowns and
 * Ctrl/Meta/Alt/Shift modifier chords, honors `viewport.characterShortcutsEnabled`,
 * and never triggers vehicle actuation.
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

  if (event.key === "Escape") {
    if (viewport.tetheredEntityId !== null || viewport.chaseLock) {
      viewport.tetherEntity(null);
      event.preventDefault?.();
      onSyncHash?.(viewport.toUrlHash());
      return true;
    }
    return false;
  }

  if (!viewport.characterShortcutsEnabled) {
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
    case "l": {
      viewport.setLightsOut(!viewport.lightsOut);
      mutated = true;
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
    default:
      return false;
  }

  if (mutated) {
    event.preventDefault?.();
    onSyncHash?.(viewport.toUrlHash());
  }
  return mutated;
}
