// SPDX-License-Identifier: MIT
// Copyright (c) 2026 boxbotix

import { describe, expect, it } from "vitest";
import { UXS_DOMAIN_KINDS, type UxsDomainKind } from "../uxs";
import { createSkgStore } from "./skg.svelte";
import { buildVehicleStoredPayload, createSeededRng } from "./test-helpers";
import {
  CHARACTER_SHORTCUTS_STORAGE_KEY,
  COP_LAYERS,
  type CopLayer,
  createViewportStore,
  DECLUTTER_LEVELS,
  type DeclutterLevel,
  defaultViewportSnapshot,
  parseViewportHash,
  PILOT_LAYOUTS,
  type PilotLayout,
  serializeViewportHash,
  type ShortcutStorageLike,
  type ViewportSnapshot,
  WORKSPACE_MODES,
  type WorkspaceMode,
} from "./viewport.svelte";

describe("viewport.svelte.ts — deterministic COP controller verbs & URL hash codec", () => {
  // spec: cop-ui / Deterministic COP viewport controller verbs and URL hash serialization
  it("mutates selection, tether, chase lock, domain filters, layers, and declutter deterministically and gates selectedCommandableVehicle", () => {
    const skgStore = createSkgStore();
    const quadPayload = buildVehicleStoredPayload(1, {
      "uxs.domain.kind": "quadcopter",
    });
    const contactId = "boxbotix.local.tak.cop.responder.team1";
    const contactPayload = {
      id: contactId,
      triples: [
        {
          subject: contactId,
          predicate: "dc.terms.title",
          object: "Team 1",
          source: "cop-ingest",
          timestamp: "2026-11-24T11:00:00Z",
          confidence: 1,
        },
      ],
    };

    skgStore.ingestRawFrame(
      JSON.stringify({
        type: "put",
        bucket: "ENTITY_STATES",
        key: quadPayload.id,
        revision: 1,
        payload: quadPayload,
      }),
    );
    skgStore.ingestRawFrame(
      JSON.stringify({
        type: "put",
        bucket: "ENTITY_STATES",
        key: contactId,
        revision: 2,
        payload: contactPayload,
      }),
    );
    skgStore.ingestRawFrame(
      JSON.stringify({
        type: "synced",
        bucket: "ENTITY_STATES",
        revision: 2,
      }),
    );

    const viewport = createViewportStore({ skgStore });
    expect(viewport.toUrlHash()).toBe("");
    expect(viewport.selectedEntity).toBeNull();
    expect(viewport.selectedCommandableVehicle).toBeNull();

    // Chase lock with neither tether nor selection stays false.
    viewport.setChaseLock(true);
    expect(viewport.chaseLock).toBe(false);
    expect(viewport.tetheredEntityId).toBeNull();

    // Selecting ownship UXS vehicle populates selectedCommandableVehicle; enabling chaseLock auto-tethers selected entity.
    viewport.selectEntity(`  ${quadPayload.id}  `);
    expect(viewport.selectedEntityId).toBe(quadPayload.id);
    expect(viewport.selectedCommandableVehicle?.sysid).toBe(1);

    viewport.setChaseLock(true);
    expect(viewport.tetheredEntityId).toBe(quadPayload.id);
    expect(viewport.chaseLock).toBe(true);

    // Clearing camera tether MUST clear chaseLock automatically.
    viewport.tetherEntity(null);
    expect(viewport.tetheredEntityId).toBeNull();
    expect(viewport.chaseLock).toBe(false);

    // Selecting an external contact populates selectedEntity while selectedCommandableVehicle MUST be null!
    viewport.selectEntity(contactId);
    expect(viewport.selectedEntity?.kind).toBe("external-contact");
    expect(viewport.selectedCommandableVehicle).toBeNull();

    // Domain filters deduplicate and preserve canonical ordering.
    viewport.setDomainFilter(["surface-boat", "quadcopter", "surface-boat"]);
    expect(viewport.domainFilter).toEqual(["quadcopter", "surface-boat"]);
    expect(viewport.isDomainVisible("quadcopter")).toBe(true);
    expect(viewport.isDomainVisible("rover")).toBe(false);

    // Layer toggles support implicit flip and explicit boolean.
    viewport.toggleLayer("grid");
    expect(viewport.isLayerVisible("grid")).toBe(false);
    viewport.toggleLayer("grid", false);
    expect(viewport.isLayerVisible("grid")).toBe(false);
    viewport.toggleLayer("grid", true);
    expect(viewport.isLayerVisible("grid")).toBe(true);

    viewport.setDeclutter("tactical");
    expect(viewport.declutter).toBe("tactical");
  });

  // spec: cop-ui / Deterministic COP viewport controller verbs and URL hash serialization
  it("round-trips non-default and empty-filter viewport state through URL hash and sanitizes malformed fragments", () => {
    const source = createViewportStore();
    source.selectEntity("boxbotix.local.robotics.uxs.vehicle.7");
    source.tetherEntity("boxbotix.local.robotics.uxs.vehicle.7");
    source.setChaseLock(true);
    source.setDomainFilter(["rover", "surface-boat"]);
    source.toggleLayer("grid", false);
    source.toggleLayer("trails", false);
    source.setDeclutter("minimal");

    const hash = source.toUrlHash();
    expect(hash).toBe(
      "#sel=boxbotix.local.robotics.uxs.vehicle.7&tether=boxbotix.local.robotics.uxs.vehicle.7&chase=1&domains=rover%2Csurface-boat&layers=vehicles%2Ccontacts%2Calerts&declutter=minimal",
    );

    const hydrated = createViewportStore({ initialHash: hash });
    expect(hydrated.snapshot()).toEqual(source.snapshot());

    // Empty domain and layer filters round-trip losslessly via `none` sentinel.
    source.setDomainFilter([]);
    source.toggleLayer("vehicles", false);
    source.toggleLayer("contacts", false);
    source.toggleLayer("alerts", false);
    const emptySetHash = source.toUrlHash();
    hydrated.applyUrlHash(emptySetHash);
    expect(hydrated.domainFilter).toEqual([]);
    expect(hydrated.visibleLayers).toEqual([]);
    expect(hydrated.snapshot()).toEqual(source.snapshot());

    // Malformed fragments sanitize safely to canonical defaults without throwing:
    // - chase=1 without tether or with malformed tether -> chaseLock false
    // - incomplete / non-6-part / forbidden-character / >256-byte IDs -> null
    // - unknown domain/layer tokens -> defaults
    // - unknown declutter -> "none"
    const malformedHashes = [
      "#sel=bad%00id&chase=1&domains=submarine,spaceship&layers=radar&declutter=ultra&extra=ignored",
      "#sel=incomplete&tether=incomplete&chase=1",
      "#sel=a.b.c.d.e._leading&tether=a.b.c.d.e.-dash&chase=1",
      "#sel=a.b.c.d.e.bad$char&tether=a.b.c.d.e.bad%2Fslash&chase=1",
      "#sel=a.b.c.d.e&tether=a.b.c.d.e.f.g&chase=1",
      `#sel=${"a".repeat(250)}.b.c.d.e.f&tether=${"x".repeat(250)}.b.c.d.e.f&chase=1`,
    ];
    for (const badHash of malformedHashes) {
      expect(parseViewportHash(badHash)).toEqual(defaultViewportSnapshot());
    }

    // Programmatic verbs likewise reject malformed ADR-102 IDs and keep chaseLock false.
    const guarded = createViewportStore();
    guarded.selectEntity("incomplete");
    guarded.tetherEntity("a.b.c.d.e._bad");
    guarded.setChaseLock(true);
    expect(guarded.selectedEntityId).toBeNull();
    expect(guarded.tetheredEntityId).toBeNull();
    expect(guarded.chaseLock).toBe(false);

    // Valid 6-part external contact IDs with underscores and hyphens round-trip cleanly.
    const validContactId =
      "boxbotix.local_1.tak-bridge.cop.responder_track.bravo-2";
    const contactHydrated = parseViewportHash(
      `#sel=${validContactId}&tether=${validContactId}&chase=1`,
    );
    expect(contactHydrated.selectedEntityId).toBe(validContactId);
    expect(contactHydrated.tetheredEntityId).toBe(validContactId);
    expect(contactHydrated.chaseLock).toBe(true);

    // Mixed valid + unknown tokens retain only the valid subset in canonical order.
    const mixed = parseViewportHash(
      "domains=surface-boat,unknown-domain,quadcopter&layers=alerts,bogus,vehicles",
    );
    expect(mixed.domainFilter).toEqual(["quadcopter", "surface-boat"]);
    expect(mixed.visibleLayers).toEqual(["vehicles", "alerts"]);
  });

  // spec: cop-ui / Deterministic COP viewport controller verbs and URL hash serialization
  it("satisfies round-trip, idempotence, and reference-model equivalence across seeded controller verb histories", () => {
    const rng = createSeededRng(0x71e39047);
    const store = createViewportStore();
    let ref: ViewportSnapshot = defaultViewportSnapshot();

    let sawEmptyDomains = false;
    let sawChaseAutoTether = false;
    let sawUntetherClearsChase = false;

    const sampleIds: Array<string | null> = [
      null,
      "boxbotix.local.robotics.uxs.vehicle.1",
      "boxbotix.local.robotics.uxs.vehicle.254",
      "boxbotix.local.tak.cop.contact.echo",
    ];

    for (let step = 0; step < 250; step += 1) {
      const op = rng();
      if (op < 0.2) {
        const id = sampleIds[Math.floor(rng() * sampleIds.length)] ?? null;
        store.selectEntity(id);
        ref = { ...ref, selectedEntityId: id };
      } else if (op < 0.4) {
        const id = sampleIds[Math.floor(rng() * sampleIds.length)] ?? null;
        if (id === null && ref.chaseLock) {
          sawUntetherClearsChase = true;
        }
        store.tetherEntity(id);
        ref = {
          ...ref,
          tetheredEntityId: id,
          chaseLock: id === null ? false : ref.chaseLock,
        };
      } else if (op < 0.58) {
        const enable = rng() < 0.6;
        if (
          enable &&
          ref.tetheredEntityId === null &&
          ref.selectedEntityId !== null
        ) {
          sawChaseAutoTether = true;
        }
        store.setChaseLock(enable);
        if (!enable) {
          ref = { ...ref, chaseLock: false };
        } else if (ref.tetheredEntityId !== null) {
          ref = { ...ref, chaseLock: true };
        } else if (ref.selectedEntityId !== null) {
          ref = {
            ...ref,
            tetheredEntityId: ref.selectedEntityId,
            chaseLock: true,
          };
        }
      } else if (op < 0.74) {
        const chosen: UxsDomainKind[] = UXS_DOMAIN_KINDS.filter(
          () => rng() < 0.5,
        );
        if (chosen.length === 0) {
          sawEmptyDomains = true;
        }
        store.setDomainFilter([...chosen].reverse());
        ref = { ...ref, domainFilter: chosen };
      } else if (op < 0.9) {
        const layer = COP_LAYERS[
          Math.floor(rng() * COP_LAYERS.length)
        ] as CopLayer;
        const explicit = rng() < 0.5 ? undefined : rng() < 0.5;
        store.toggleLayer(layer, explicit);
        const shouldEnable =
          explicit !== undefined
            ? explicit
            : !ref.visibleLayers.includes(layer);
        const nextLayers = COP_LAYERS.filter((l) =>
          l === layer ? shouldEnable : ref.visibleLayers.includes(l),
        );
        ref = { ...ref, visibleLayers: nextLayers };
      } else {
        const level = DECLUTTER_LEVELS[
          Math.floor(rng() * DECLUTTER_LEVELS.length)
        ] as DeclutterLevel;
        store.setDeclutter(level);
        ref = { ...ref, declutter: level };
      }

      // 1. Reference-model state equivalence
      const snap = store.snapshot();
      expect(snap).toEqual(ref);

      // 2. Round-trip law: parse(serialize(snap)) === snap
      const hash = store.toUrlHash();
      const reparsed = parseViewportHash(hash);
      expect(reparsed).toEqual(ref);

      // 3. Idempotence law: serialize(parse(hash)) === hash
      expect(serializeViewportHash(reparsed)).toBe(hash);
    }

    expect(sawEmptyDomains).toBe(true);
    expect(sawChaseAutoTether).toBe(true);
    expect(sawUntetherClearsChase).toBe(true);
  });

  // spec: cop-ui / Task-modal 4-workspace shell, invariant safety topbar, and Fleet Filmstrip
  it("preserves default #sel=... URL hashes while round-tripping workspace, pilotLayout, and lightsOut across seeded histories", () => {
    const store = createViewportStore();
    store.selectEntity("boxbotix.local.robotics.uxs.vehicle.2");
    expect(store.workspace).toBe("cop");
    expect(store.pilotLayout).toBe("map");
    expect(store.lightsOut).toBe(false);
    expect(store.toUrlHash()).toBe(
      "#sel=boxbotix.local.robotics.uxs.vehicle.2",
    );

    store.setWorkspace("pilot");
    store.togglePilotLayout();
    store.setLightsOut(true);
    expect(store.toUrlHash()).toBe(
      "#mode=pilot&pilot=split&chrome=lights-out&sel=boxbotix.local.robotics.uxs.vehicle.2",
    );

    const hydrated = createViewportStore({ initialHash: store.toUrlHash() });
    expect(hydrated.snapshot()).toEqual(store.snapshot());

    // Malformed mode, pilot, and chrome tokens fall back safely to cop / map / false.
    const sanitized = parseViewportHash(
      "#mode=spacedock&pilot=vr&chrome=neon&sel=boxbotix.local.robotics.uxs.vehicle.2",
    );
    expect(sanitized.workspace).toBe("cop");
    expect(sanitized.pilotLayout).toBe("map");
    expect(sanitized.lightsOut).toBe(false);
    expect(sanitized.selectedEntityId).toBe(
      "boxbotix.local.robotics.uxs.vehicle.2",
    );

    // Seeded property check across workspace, pilotLayout, and lightsOut transitions.
    const rng = createSeededRng(0x524d3234);
    let ref: ViewportSnapshot = defaultViewportSnapshot();
    store.reset();
    let sawAllModes = new Set<WorkspaceMode>();
    let sawSplit = false;
    let sawLightsOut = false;

    for (let step = 0; step < 160; step += 1) {
      const pick = rng();
      if (pick < 0.45) {
        const mode = WORKSPACE_MODES[
          Math.floor(rng() * WORKSPACE_MODES.length)
        ] as WorkspaceMode;
        sawAllModes.add(mode);
        store.setWorkspace(mode);
        ref = { ...ref, workspace: mode };
      } else if (pick < 0.75) {
        if (rng() < 0.5) {
          store.togglePilotLayout();
          const nextLayout: PilotLayout =
            ref.pilotLayout === "map" ? "split" : "map";
          if (nextLayout === "split") {
            sawSplit = true;
          }
          ref = { ...ref, pilotLayout: nextLayout };
        } else {
          const layout = PILOT_LAYOUTS[
            Math.floor(rng() * PILOT_LAYOUTS.length)
          ] as PilotLayout;
          if (layout === "split") {
            sawSplit = true;
          }
          store.setPilotLayout(layout);
          ref = { ...ref, pilotLayout: layout };
        }
      } else {
        const explicit = rng() < 0.5 ? undefined : rng() < 0.5;
        store.setLightsOut(explicit);
        const nextLights = explicit !== undefined ? explicit : !ref.lightsOut;
        if (nextLights) {
          sawLightsOut = true;
        }
        ref = { ...ref, lightsOut: nextLights };
      }

      const snap = store.snapshot();
      expect(snap).toEqual(ref);
      const encoded = store.toUrlHash();
      const decoded = parseViewportHash(encoded);
      expect(decoded).toEqual(ref);
      expect(serializeViewportHash(decoded)).toBe(encoded);
    }

    expect(sawAllModes.size).toBe(WORKSPACE_MODES.length);
    expect(sawSplit).toBe(true);
    expect(sawLightsOut).toBe(true);
  });

  // spec: cop-ui / Conventional keyboard traversal, WAI-ARIA tablist navigation, focus management, and character-key shortcut gating
  it("persists characterShortcutsEnabled in storage without leaking into ViewportSnapshot or shared URL hashes", () => {
    const backing = new Map<string, string>();
    const storage: ShortcutStorageLike = {
      getItem: (key) => backing.get(key) ?? null,
      setItem: (key, value) => {
        backing.set(key, value);
      },
    };

    const store = createViewportStore({ storage });
    expect(store.characterShortcutsEnabled).toBe(true);

    store.selectEntity("boxbotix.local.robotics.uxs.vehicle.4");
    const hashBeforeToggle = store.toUrlHash();
    const snapBeforeToggle = store.snapshot();

    // Implicit toggle -> false; persisted in storage, omitted from snapshot & URL hash.
    store.setCharacterShortcutsEnabled();
    expect(store.characterShortcutsEnabled).toBe(false);
    expect(backing.get(CHARACTER_SHORTCUTS_STORAGE_KEY)).toBe("false");
    expect(store.toUrlHash()).toBe(hashBeforeToggle);
    expect(store.snapshot()).toEqual(snapBeforeToggle);

    // Applying a shared URL hash or resetting tactical viewport state preserves operator shortcut preference.
    store.applyUrlHash("#mode=bench&sel=boxbotix.local.robotics.uxs.vehicle.9");
    expect(store.characterShortcutsEnabled).toBe(false);
    store.reset();
    expect(store.characterShortcutsEnabled).toBe(false);

    // Fresh store hydrates false from storage; explicit true writes "true".
    const rehydrated = createViewportStore({ storage });
    expect(rehydrated.characterShortcutsEnabled).toBe(false);
    rehydrated.setCharacterShortcutsEnabled(true);
    expect(rehydrated.characterShortcutsEnabled).toBe(true);
    expect(backing.get(CHARACTER_SHORTCUTS_STORAGE_KEY)).toBe("true");

    // Malformed storage token falls back to initialCharacterShortcutsEnabled (default true).
    backing.set(CHARACTER_SHORTCUTS_STORAGE_KEY, "corrupted");
    expect(createViewportStore({ storage }).characterShortcutsEnabled).toBe(
      true,
    );
    expect(
      createViewportStore({
        storage,
        initialCharacterShortcutsEnabled: false,
      }).characterShortcutsEnabled,
    ).toBe(false);

    // Throwing storage (e.g. sandboxed iframe) degrades gracefully in memory without throwing.
    const brokenStorage: ShortcutStorageLike = {
      getItem: () => {
        throw new Error("SecurityError");
      },
      setItem: () => {
        throw new Error("QuotaExceededError");
      },
    };
    const sandboxed = createViewportStore({ storage: brokenStorage });
    expect(sandboxed.characterShortcutsEnabled).toBe(true);
    sandboxed.setCharacterShortcutsEnabled(false);
    expect(sandboxed.characterShortcutsEnabled).toBe(false);
  });
});
