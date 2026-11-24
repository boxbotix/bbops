// SPDX-License-Identifier: MIT
// Copyright (c) 2026 boxbotix

import {
  UXS_DOMAIN_KINDS,
  type UxsDomainKind,
  isUxsDomainKind,
  isValidEntityIdString,
} from "../uxs";
import type {
  CopEntityState,
  SkgStore,
  VehicleEntityState,
} from "./skg.svelte";

export const WORKSPACE_MODES = ["cop", "pilot", "bench", "cortex"] as const;
export type WorkspaceMode = (typeof WORKSPACE_MODES)[number];

export const PILOT_LAYOUTS = ["map", "split"] as const;
export type PilotLayout = (typeof PILOT_LAYOUTS)[number];

export const COP_LAYERS = [
  "vehicles",
  "contacts",
  "trails",
  "grid",
  "alerts",
] as const;
export type CopLayer = (typeof COP_LAYERS)[number];

export const DECLUTTER_LEVELS = ["none", "minimal", "tactical"] as const;
export type DeclutterLevel = (typeof DECLUTTER_LEVELS)[number];

export interface ViewportSnapshot {
  workspace: WorkspaceMode;
  pilotLayout: PilotLayout;
  lightsOut: boolean;
  selectedEntityId: string | null;
  tetheredEntityId: string | null;
  chaseLock: boolean;
  domainFilter: readonly UxsDomainKind[];
  visibleLayers: readonly CopLayer[];
  declutter: DeclutterLevel;
}

export interface ViewportStoreOptions {
  skgStore?: Pick<SkgStore, "getEntity" | "getCommandableVehicle">;
  initialHash?: string;
}

export interface ViewportStore {
  readonly workspace: WorkspaceMode;
  readonly pilotLayout: PilotLayout;
  readonly lightsOut: boolean;
  readonly selectedEntityId: string | null;
  readonly tetheredEntityId: string | null;
  readonly chaseLock: boolean;
  readonly domainFilter: readonly UxsDomainKind[];
  readonly visibleLayers: readonly CopLayer[];
  readonly declutter: DeclutterLevel;
  readonly selectedEntity: CopEntityState | null;
  readonly selectedCommandableVehicle: VehicleEntityState | null;
  setWorkspace(mode: WorkspaceMode): void;
  setPilotLayout(layout: PilotLayout): void;
  togglePilotLayout(): void;
  setLightsOut(enabled?: boolean): void;
  selectEntity(entityId: string | null): void;
  tetherEntity(entityId: string | null): void;
  setChaseLock(enabled: boolean): void;
  setDomainFilter(domains: Iterable<UxsDomainKind>): void;
  toggleLayer(layer: CopLayer, enabled?: boolean): void;
  isDomainVisible(domain: UxsDomainKind): boolean;
  isLayerVisible(layer: CopLayer): boolean;
  setDeclutter(level: DeclutterLevel): void;
  snapshot(): ViewportSnapshot;
  toUrlHash(): string;
  applyUrlHash(hash: string): void;
  reset(): void;
}

const EMPTY_SET_SENTINEL = "none";
const LIGHTS_OUT_CHROME_TOKEN = "lights-out";

export function isWorkspaceMode(value: unknown): value is WorkspaceMode {
  return (
    typeof value === "string" &&
    (WORKSPACE_MODES as readonly string[]).includes(value)
  );
}

export function isPilotLayout(value: unknown): value is PilotLayout {
  return (
    typeof value === "string" &&
    (PILOT_LAYOUTS as readonly string[]).includes(value)
  );
}

export function isCopLayer(value: unknown): value is CopLayer {
  return (
    typeof value === "string" &&
    (COP_LAYERS as readonly string[]).includes(value)
  );
}

export function isDeclutterLevel(value: unknown): value is DeclutterLevel {
  return (
    typeof value === "string" &&
    (DECLUTTER_LEVELS as readonly string[]).includes(value)
  );
}

function sanitizeEntityIdInput(
  value: string | null | undefined,
): string | null {
  if (typeof value !== "string") {
    return null;
  }
  const trimmed = value.trim();
  return isValidEntityIdString(trimmed) ? trimmed : null;
}

function canonicalizeDomains(
  domains: Iterable<UxsDomainKind>,
): UxsDomainKind[] {
  const input = Array.from(domains);
  return UXS_DOMAIN_KINDS.filter((domain) => input.includes(domain));
}

function canonicalizeLayers(layers: Iterable<CopLayer>): CopLayer[] {
  const input = Array.from(layers);
  return COP_LAYERS.filter((layer) => input.includes(layer));
}

function sameOrderedList<T>(a: readonly T[], b: readonly T[]): boolean {
  if (a.length !== b.length) {
    return false;
  }
  for (let i = 0; i < a.length; i += 1) {
    if (a[i] !== b[i]) {
      return false;
    }
  }
  return true;
}

export function defaultViewportSnapshot(): ViewportSnapshot {
  return {
    workspace: "cop",
    pilotLayout: "map",
    lightsOut: false,
    selectedEntityId: null,
    tetheredEntityId: null,
    chaseLock: false,
    domainFilter: [...UXS_DOMAIN_KINDS],
    visibleLayers: [...COP_LAYERS],
    declutter: "none",
  };
}

export function serializeViewportHash(state: ViewportSnapshot): string {
  // eslint-disable-next-line svelte/prefer-svelte-reactivity
  const params = new URLSearchParams();

  if (isWorkspaceMode(state.workspace) && state.workspace !== "cop") {
    params.set("mode", state.workspace);
  }
  if (isPilotLayout(state.pilotLayout) && state.pilotLayout !== "map") {
    params.set("pilot", state.pilotLayout);
  }
  if (state.lightsOut === true) {
    params.set("chrome", LIGHTS_OUT_CHROME_TOKEN);
  }

  const sel = sanitizeEntityIdInput(state.selectedEntityId);
  const tether = sanitizeEntityIdInput(state.tetheredEntityId);

  if (sel !== null) {
    params.set("sel", sel);
  }
  if (tether !== null) {
    params.set("tether", tether);
    if (state.chaseLock) {
      params.set("chase", "1");
    }
  }

  const domains = canonicalizeDomains(state.domainFilter);
  if (!sameOrderedList(domains, UXS_DOMAIN_KINDS)) {
    params.set(
      "domains",
      domains.length === 0 ? EMPTY_SET_SENTINEL : domains.join(","),
    );
  }

  const layers = canonicalizeLayers(state.visibleLayers);
  if (!sameOrderedList(layers, COP_LAYERS)) {
    params.set(
      "layers",
      layers.length === 0 ? EMPTY_SET_SENTINEL : layers.join(","),
    );
  }

  if (isDeclutterLevel(state.declutter) && state.declutter !== "none") {
    params.set("declutter", state.declutter);
  }

  const encoded = params.toString();
  return encoded.length > 0 ? `#${encoded}` : "";
}

export function parseViewportHash(hash: string): ViewportSnapshot {
  const defaults = defaultViewportSnapshot();
  if (typeof hash !== "string") {
    return defaults;
  }
  const stripped = hash.trim().replace(/^#/, "");
  if (stripped.length === 0) {
    return defaults;
  }

  // eslint-disable-next-line svelte/prefer-svelte-reactivity
  const params = new URLSearchParams(stripped);

  const rawMode = params.get("mode")?.trim();
  const workspace: WorkspaceMode = isWorkspaceMode(rawMode) ? rawMode : "cop";

  const rawPilot = params.get("pilot")?.trim();
  const pilotLayout: PilotLayout = isPilotLayout(rawPilot) ? rawPilot : "map";

  const rawChrome = params.get("chrome")?.trim();
  const lightsOut = rawChrome === LIGHTS_OUT_CHROME_TOKEN || rawChrome === "1";

  const selectedEntityId = sanitizeEntityIdInput(params.get("sel"));
  const tetheredEntityId = sanitizeEntityIdInput(params.get("tether"));

  const rawChase = params.get("chase");
  const chaseLock =
    tetheredEntityId !== null && (rawChase === "1" || rawChase === "true");

  let domainFilter: UxsDomainKind[] = [...UXS_DOMAIN_KINDS];
  const rawDomains = params.get("domains");
  if (rawDomains !== null) {
    const trimmed = rawDomains.trim();
    if (trimmed === EMPTY_SET_SENTINEL) {
      domainFilter = [];
    } else if (trimmed.length > 0) {
      const validTokens = trimmed
        .split(",")
        .map((token) => token.trim())
        .filter(isUxsDomainKind);
      if (validTokens.length > 0) {
        domainFilter = canonicalizeDomains(validTokens);
      }
    }
  }

  let visibleLayers: CopLayer[] = [...COP_LAYERS];
  const rawLayers = params.get("layers");
  if (rawLayers !== null) {
    const trimmed = rawLayers.trim();
    if (trimmed === EMPTY_SET_SENTINEL) {
      visibleLayers = [];
    } else if (trimmed.length > 0) {
      const validTokens = trimmed
        .split(",")
        .map((token) => token.trim())
        .filter(isCopLayer);
      if (validTokens.length > 0) {
        visibleLayers = canonicalizeLayers(validTokens);
      }
    }
  }

  const rawDeclutter = params.get("declutter")?.trim();
  const declutter: DeclutterLevel = isDeclutterLevel(rawDeclutter)
    ? rawDeclutter
    : "none";

  return {
    workspace,
    pilotLayout,
    lightsOut,
    selectedEntityId,
    tetheredEntityId,
    chaseLock,
    domainFilter,
    visibleLayers,
    declutter,
  };
}

export function createViewportStore(
  options: ViewportStoreOptions = {},
): ViewportStore {
  const initial =
    options.initialHash !== undefined
      ? parseViewportHash(options.initialHash)
      : defaultViewportSnapshot();

  let workspace = $state<WorkspaceMode>(initial.workspace);
  let pilotLayout = $state<PilotLayout>(initial.pilotLayout);
  let lightsOut = $state<boolean>(initial.lightsOut);
  let selectedEntityId = $state<string | null>(initial.selectedEntityId);
  let tetheredEntityId = $state<string | null>(initial.tetheredEntityId);
  let chaseLock = $state(initial.chaseLock);
  let domainFilter = $state.raw<readonly UxsDomainKind[]>(initial.domainFilter);
  let visibleLayers = $state.raw<readonly CopLayer[]>(initial.visibleLayers);
  let declutter = $state<DeclutterLevel>(initial.declutter);

  const selectedEntity = $derived.by(() =>
    options.skgStore ? options.skgStore.getEntity(selectedEntityId) : null,
  );

  const selectedCommandableVehicle = $derived.by(() =>
    options.skgStore
      ? options.skgStore.getCommandableVehicle(selectedEntityId)
      : null,
  );

  function applySnapshot(next: ViewportSnapshot): void {
    workspace = isWorkspaceMode(next.workspace) ? next.workspace : "cop";
    pilotLayout = isPilotLayout(next.pilotLayout) ? next.pilotLayout : "map";
    lightsOut = Boolean(next.lightsOut);
    selectedEntityId = next.selectedEntityId;
    tetheredEntityId = next.tetheredEntityId;
    chaseLock = next.tetheredEntityId !== null ? next.chaseLock : false;
    domainFilter = [...next.domainFilter];
    visibleLayers = [...next.visibleLayers];
    declutter = next.declutter;
  }

  function snapshot(): ViewportSnapshot {
    return {
      workspace,
      pilotLayout,
      lightsOut,
      selectedEntityId,
      tetheredEntityId,
      chaseLock,
      domainFilter: [...domainFilter],
      visibleLayers: [...visibleLayers],
      declutter,
    };
  }

  return {
    get workspace() {
      return workspace;
    },
    get pilotLayout() {
      return pilotLayout;
    },
    get lightsOut() {
      return lightsOut;
    },
    get selectedEntityId() {
      return selectedEntityId;
    },
    get tetheredEntityId() {
      return tetheredEntityId;
    },
    get chaseLock() {
      return chaseLock;
    },
    get domainFilter() {
      return domainFilter;
    },
    get visibleLayers() {
      return visibleLayers;
    },
    get declutter() {
      return declutter;
    },
    get selectedEntity() {
      return selectedEntity;
    },
    get selectedCommandableVehicle() {
      return selectedCommandableVehicle;
    },

    setWorkspace(mode: WorkspaceMode) {
      if (isWorkspaceMode(mode)) {
        workspace = mode;
      }
    },

    setPilotLayout(layout: PilotLayout) {
      if (isPilotLayout(layout)) {
        pilotLayout = layout;
      }
    },

    togglePilotLayout() {
      pilotLayout = pilotLayout === "map" ? "split" : "map";
    },

    setLightsOut(enabled?: boolean) {
      lightsOut = enabled !== undefined ? Boolean(enabled) : !lightsOut;
    },

    selectEntity(entityId: string | null) {
      selectedEntityId = sanitizeEntityIdInput(entityId);
    },

    tetherEntity(entityId: string | null) {
      const sanitized = sanitizeEntityIdInput(entityId);
      tetheredEntityId = sanitized;
      if (sanitized === null) {
        chaseLock = false;
      }
    },

    setChaseLock(enabled: boolean) {
      if (!enabled) {
        chaseLock = false;
        return;
      }
      if (tetheredEntityId !== null) {
        chaseLock = true;
        return;
      }
      if (selectedEntityId !== null) {
        tetheredEntityId = selectedEntityId;
        chaseLock = true;
        return;
      }
      chaseLock = false;
    },

    setDomainFilter(domains: Iterable<UxsDomainKind>) {
      domainFilter = canonicalizeDomains(domains);
    },

    toggleLayer(layer: CopLayer, enabled?: boolean) {
      if (!isCopLayer(layer)) {
        return;
      }
      const currentlyEnabled = visibleLayers.includes(layer);
      const shouldEnable =
        enabled !== undefined ? Boolean(enabled) : !currentlyEnabled;
      const next = shouldEnable
        ? [...visibleLayers, layer]
        : visibleLayers.filter((item) => item !== layer);
      visibleLayers = canonicalizeLayers(next);
    },

    isDomainVisible(domain: UxsDomainKind) {
      return domainFilter.includes(domain);
    },

    isLayerVisible(layer: CopLayer) {
      return visibleLayers.includes(layer);
    },

    setDeclutter(level: DeclutterLevel) {
      if (isDeclutterLevel(level)) {
        declutter = level;
      }
    },

    snapshot,

    toUrlHash() {
      return serializeViewportHash(snapshot());
    },

    applyUrlHash(hash: string) {
      applySnapshot(parseViewportHash(hash));
    },

    reset() {
      applySnapshot(defaultViewportSnapshot());
    },
  };
}
