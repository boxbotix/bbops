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

const EDITABLE_TAGS = new Set(["INPUT", "TEXTAREA", "SELECT"]);

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

export function nextDeclutterLevel(current: DeclutterLevel): DeclutterLevel {
  const idx = DECLUTTER_LEVELS.indexOf(current);
  if (idx < 0) {
    return "none";
  }
  return DECLUTTER_LEVELS[(idx + 1) % DECLUTTER_LEVELS.length] ?? "none";
}

/**
 * Handles COP tactical viewport hotkeys:
 * - T: toggle tether on selected entity (or untether when already tethered)
 * - C: toggle chase heading lock (auto-tethers selected entity if untethered)
 * - H: cycle declutter level (none -> minimal -> tactical -> none)
 * - Escape: untether camera and clear chase lock
 *
 * Ignores editable form/contenteditable targets, repeated keydowns, and
 * Ctrl/Meta/Alt/Shift modifier chords. Returns true when handled.
 */
export function handleViewportHotkey(
  event: HotkeyEventLike,
  viewport: ViewportStore,
  onSyncHash?: (hash: string) => void,
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
