// SPDX-License-Identifier: MIT
// Copyright (c) 2026 boxbotix

import { describe, expect, it } from "vitest";
import { UXS_DOMAIN_KINDS, type UxsDomainKind } from "../uxs";
import { createSkgStore } from "./skg.svelte";
import { buildVehicleStoredPayload, createSeededRng } from "./test-helpers";
import {
  COP_LAYERS,
  type CopLayer,
  createViewportStore,
  DECLUTTER_LEVELS,
  type DeclutterLevel,
  defaultViewportSnapshot,
  parseViewportHash,
  serializeViewportHash,
  type ViewportSnapshot,
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
          source: "",
          timestamp: "",
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
    // - chase=1 without tether -> chaseLock false
    // - control char in sel -> null
    // - unknown domain/layer tokens -> defaults
    // - unknown declutter -> "none"
    const sanitized = parseViewportHash(
      "#sel=bad%00id&chase=1&domains=submarine,spaceship&layers=radar&declutter=ultra&extra=ignored",
    );
    expect(sanitized).toEqual(defaultViewportSnapshot());

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
});
