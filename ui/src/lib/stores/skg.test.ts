// SPDX-License-Identifier: MIT
// Copyright (c) 2026 boxbotix

import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import {
  buildVehicleStoredPayload,
  createSeededRng,
  makeTriple,
  MockWebSocketRig,
} from "./test-helpers";
import {
  createSkgStore,
  ENTITY_STATES_BUCKET,
  isCommandableVehicle,
  isExternalContactEntity,
  isVehicleEntity,
} from "./skg.svelte";

function putFrame(
  key: string,
  revision: number,
  payload: unknown,
  bucket = ENTITY_STATES_BUCKET,
): string {
  return JSON.stringify({
    type: "put",
    bucket,
    key,
    revision,
    timestamp: 1_732_446_001_000,
    payload,
  });
}

function deleteFrame(key: string, revision: number): string {
  return JSON.stringify({
    type: "delete",
    bucket: ENTITY_STATES_BUCKET,
    key,
    revision,
    timestamp: 1_732_446_002_000,
  });
}

function poisonedFrame(key: string, revision: number, error: string): string {
  return JSON.stringify({
    type: "poisoned",
    bucket: ENTITY_STATES_BUCKET,
    key,
    revision,
    error,
  });
}

function syncedFrame(revision: number): string {
  return JSON.stringify({
    type: "synced",
    bucket: ENTITY_STATES_BUCKET,
    revision,
  });
}

describe("skg.svelte.ts — WS #2 ENTITY_STATES kv-watch store", () => {
  beforeEach(() => {
    vi.useFakeTimers();
  });

  afterEach(() => {
    vi.useRealTimers();
  });

  // spec: cop-ui / Gapless ENTITY_STATES SKG store, external contact separation, and GraphQL client
  it("stages initial and reconnect snapshots until synced without blanking live state and prunes deleted keys atomically", () => {
    const rig = new MockWebSocketRig();
    const store = createSkgStore({
      url: "ws://127.0.0.1:8082/ws",
      createSocket: rig.createSocket,
    });

    store.connect();
    rig.latest.serverOpen();
    expect(store.synced).toBe(false);

    const v1 = buildVehicleStoredPayload(1, {
      "uxs.domain.kind": "quadcopter",
    });
    const v2 = buildVehicleStoredPayload(2, { "uxs.domain.kind": "rover" });

    // Pre-synced puts are staged off the live SvelteMap until `synced` arrives.
    rig.latest.serverMessage(putFrame(v1.id, 1, v1));
    rig.latest.serverMessage(putFrame(v2.id, 2, v2));
    expect(store.entities.size).toBe(0);
    expect(store.synced).toBe(false);

    rig.latest.serverMessage(syncedFrame(2));
    expect(store.synced).toBe(true);
    expect(store.lastSyncedRevision).toBe(2);
    expect(store.entities.size).toBe(2);
    expect(store.vehicles.map((v) => v.sysid)).toEqual([1, 2]);

    // Simulate transient WS disconnect while vehicle 2 is deleted offline and vehicle 3 joins.
    rig.latest.serverDrop(1013);
    expect(store.status).toBe("reconnecting");
    expect(store.synced).toBe(false);
    // Gapless invariant: prior tracks remain visible without blanking during reconnect!
    expect(store.entities.size).toBe(2);
    expect(store.getCommandableVehicle(v1.id)?.sysid).toBe(1);
    expect(store.getCommandableVehicle(v2.id)?.sysid).toBe(2);

    vi.advanceTimersByTime(500);
    rig.latest.serverOpen();
    // Still gapless while pre-synced burst streams in!
    expect(store.entities.size).toBe(2);

    const v1Updated = buildVehicleStoredPayload(1, {
      "uxs.authority.state": "direct-crsf-override",
    });
    const v3 = buildVehicleStoredPayload(3, {
      "uxs.domain.kind": "surface-boat",
    });
    rig.latest.serverMessage(putFrame(v1Updated.id, 5, v1Updated));
    rig.latest.serverMessage(putFrame(v3.id, 6, v3));
    // Before synced arrives, v2 is still visible and v3 is staged.
    expect(store.entities.has(v2.id)).toBe(true);
    expect(store.entities.has(v3.id)).toBe(false);

    // At synced, v2 is pruned atomically and v1 + v3 go live!
    rig.latest.serverMessage(syncedFrame(6));
    expect(store.synced).toBe(true);
    expect(store.lastSyncedRevision).toBe(6);
    expect(store.entities.has(v2.id)).toBe(false);
    expect(store.vehicles.map((v) => v.sysid)).toEqual([1, 3]);
    expect(store.getVehicle(v1.id)?.authority).toBe("direct-crsf-override");

    store.disconnect();
  });

  // spec: cop-ui / Gapless ENTITY_STATES SKG store, external contact separation, and GraphQL client
  it("reconciles live put, delete, and poisoned frames and withholds poisoned vehicles from commandable selectors until healed", () => {
    const store = createSkgStore();
    const v1 = buildVehicleStoredPayload(
      1,
      {
        "uxs.domain.kind": "surface-boat",
        "uxs.authority.state": "companion-autonomous",
        "uxs.mode.companion": "autonomous-velocity",
        "uxs.link.tier": "mesh-supervisory",
        "uxs.health.failsafes": 2,
        "uxs.kinematics.headingdeg": 270,
      },
      {
        observedAt: "2026-11-24T11:10:00.000Z",
        updatedAt: "2026-11-24T11:10:05.000Z",
      },
    );

    store.ingestRawFrame(putFrame(v1.id, 10, v1));
    store.ingestRawFrame(syncedFrame(10));

    const parsed = store.getCommandableVehicle(v1.id);
    expect(parsed).not.toBeNull();
    expect(isVehicleEntity(parsed)).toBe(true);
    expect(isCommandableVehicle(parsed)).toBe(true);
    expect(parsed?.domain).toBe("surface-boat");
    expect(parsed?.authority).toBe("companion-autonomous");
    expect(parsed?.companionMode).toBe("autonomous-velocity");
    expect(parsed?.linkTier).toBe("mesh-supervisory");
    expect(parsed?.sysStatus).toEqual({
      failsafes: 2,
      crcErrors: 1,
      loopDtUs: 2500,
      maxLoopDtUs: 3100,
      rttUs: 420,
      hasSysStatus: true,
    });
    // Vehicle observedAt derives from uxs.* Triple.Timestamp, distinct from EntityState.updated_at!
    expect(parsed?.observedAt).toBe("2026-11-24T11:10:00.000Z");
    expect(parsed?.updatedAt).toBe("2026-11-24T11:10:05.000Z");

    // Stale lower-revision put is ignored.
    const staleV1 = buildVehicleStoredPayload(1, {
      "uxs.authority.state": "mcu-failsafe",
    });
    store.ingestRawFrame(putFrame(v1.id, 9, staleV1));
    expect(store.getVehicle(v1.id)?.authority).toBe("companion-autonomous");

    // Poisoned frame retains last-good vehicle state for roster/map diagnostics while withholding commandable status.
    store.ingestRawFrame(
      poisonedFrame(v1.id, 11, "corrupt JSON in ENTITY_STATES"),
    );
    const poisonedVehicle = store.getVehicle(v1.id);
    expect(poisonedVehicle?.poisoned).toBe(true);
    expect(poisonedVehicle?.poisonError).toBe("corrupt JSON in ENTITY_STATES");
    expect(poisonedVehicle?.location.latitude).toBeCloseTo(37.7749, 4);
    expect(store.getCommandableVehicle(v1.id)).toBeNull();
    expect(store.commandableVehicles).toHaveLength(0);

    // Later valid write heals the poisoned entity.
    const healedV1 = buildVehicleStoredPayload(1, {
      "uxs.authority.state": "companion-teleop",
    });
    store.ingestRawFrame(putFrame(v1.id, 12, healedV1));
    expect(store.getCommandableVehicle(v1.id)?.poisoned).toBe(false);
    expect(store.getCommandableVehicle(v1.id)?.authority).toBe(
      "companion-teleop",
    );
    expect(store.commandableVehicles).toHaveLength(1);

    // Delete removes the entity from both entities and vehicle selectors.
    store.ingestRawFrame(deleteFrame(v1.id, 13));
    expect(store.getEntity(v1.id)).toBeNull();
    expect(store.getVehicle(v1.id)).toBeNull();
    expect(store.getCommandableVehicle(v1.id)).toBeNull();
  });

  // spec: cop-ui / Gapless ENTITY_STATES SKG store, external contact separation, and GraphQL client
  it("keeps external COP contacts distinct with unknown-preserving metadata and excludes non-vehicles from command selectors", () => {
    const store = createSkgStore();

    // 1. Minimal external contact with valid assertion-level metadata (e.g. a title edit by `label-editor` with its own triple expiry)
    //    but zero contact-level provenance/staleness assertions -> contact-level source/observedAt/receivedAt/expiresAt MUST stay null!
    const sparseContactId = "boxbotix.local.tak.cop.responder.bravo2";
    const sparseContactPayload = {
      id: sparseContactId,
      updated_at: "2026-11-24T11:20:00.000Z",
      triples: [
        makeTriple(sparseContactId, "dc.terms.title", "Responder Bravo-2", {
          source: "label-editor",
          timestamp: "2026-11-24T11:20:00.000Z",
          expires_at: "2026-11-24T11:22:00.000Z",
        }),
        makeTriple(sparseContactId, "geo.location.latitude", 37.801, {
          source: "geo-importer",
          timestamp: "2026-11-24T11:20:00.000Z",
        }),
        makeTriple(sparseContactId, "geo.location.longitude", -122.405, {
          source: "geo-importer",
          timestamp: "2026-11-24T11:20:00.000Z",
        }),
      ],
    };

    // 2. Rich external contact carrying full provenance, timestamps, uncertainty, and feed status.
    const richContactId = "boxbotix.local.ogc.cop.marker.lz1";
    const richContactPayload = {
      id: richContactId,
      updated_at: "2026-11-24T11:21:00.000Z",
      triples: [
        makeTriple(richContactId, "dc.terms.title", "LZ Alpha", {
          source: "atak-bridge",
        }),
        makeTriple(richContactId, "cop.contact.type", "landing-zone", {
          source: "atak-bridge",
        }),
        makeTriple(richContactId, "cop.contact.source", "atak-team-red", {
          source: "atak-bridge",
        }),
        makeTriple(
          richContactId,
          "cop.time.observed",
          "2026-11-24T11:20:55.000Z",
          { source: "atak-bridge" },
        ),
        makeTriple(
          richContactId,
          "cop.time.received",
          "2026-11-24T11:20:57.000Z",
          { source: "atak-bridge" },
        ),
        makeTriple(
          richContactId,
          "cop.time.expires",
          "2026-11-24T11:25:55.000Z",
          { source: "atak-bridge" },
        ),
        makeTriple(richContactId, "cop.location.uncertainty-m", 8.5, {
          source: "atak-bridge",
        }),
        makeTriple(richContactId, "cop.feed.status", "live", {
          source: "atak-bridge",
        }),
        makeTriple(richContactId, "geo.location.latitude", 37.79, {
          source: "atak-bridge",
        }),
        makeTriple(richContactId, "geo.location.longitude", -122.41, {
          source: "atak-bridge",
        }),
        makeTriple(richContactId, "geo.location.altitude", 15.0, {
          source: "atak-bridge",
        }),
      ],
    };

    // 3. Adversarial non-vehicle entity carrying stray uxs.* predicates AND a vehicle-ID entity missing core uxs.* triples.
    const spoofedContactId = "boxbotix.local.tak.cop.contact.spoof1";
    const spoofedPayload = {
      ...buildVehicleStoredPayload(99),
      id: spoofedContactId,
    };
    const incompleteVehicleId = "boxbotix.local.robotics.uxs.vehicle.4";
    const incompleteVehiclePayload = {
      id: incompleteVehicleId,
      triples: [
        makeTriple(incompleteVehicleId, "uxs.domain.kind", "rover"),
        makeTriple(incompleteVehicleId, "dc.terms.title", "Partial Rover"),
      ],
    };

    store.ingestRawFrame(putFrame(sparseContactId, 1, sparseContactPayload));
    store.ingestRawFrame(putFrame(richContactId, 2, richContactPayload));
    store.ingestRawFrame(putFrame(spoofedContactId, 3, spoofedPayload));
    store.ingestRawFrame(
      putFrame(incompleteVehicleId, 4, incompleteVehiclePayload),
    );
    store.ingestRawFrame(syncedFrame(4));

    expect(store.entities.size).toBe(4);
    expect(store.vehicles).toHaveLength(0);
    expect(store.commandableVehicles).toHaveLength(0);
    expect(store.externalContacts).toHaveLength(3);

    const sparse = store.getEntity(sparseContactId);
    expect(isExternalContactEntity(sparse)).toBe(true);
    expect(sparse?.source).toBeNull();
    expect(sparse?.observedAt).toBeNull();
    expect(sparse?.receivedAt).toBeNull();
    expect(sparse?.expiresAt).toBeNull();
    expect(sparse?.uncertaintyMeters).toBeNull();
    expect(sparse?.feedStatus).toBe("unknown");
    // Assertion-level source and expires_at remain intact on the stored triple itself.
    const titleTriple = sparse?.triples.find(
      (t) => t.predicate === "dc.terms.title",
    );
    expect(titleTriple?.source).toBe("label-editor");
    expect(titleTriple?.expires_at).toBe("2026-11-24T11:22:00.000Z");
    expect(sparse?.location).toEqual({
      latitude: 37.801,
      longitude: -122.405,
      altitude: null,
    });
    expect(store.getCommandableVehicle(sparseContactId)).toBeNull();

    const rich = store.getEntity(richContactId);
    expect(isExternalContactEntity(rich)).toBe(true);
    if (isExternalContactEntity(rich)) {
      expect(rich.contactType).toBe("landing-zone");
    }
    expect(rich?.source).toBe("atak-team-red");
    expect(rich?.observedAt).toBe("2026-11-24T11:20:55.000Z");
    expect(rich?.receivedAt).toBe("2026-11-24T11:20:57.000Z");
    expect(rich?.expiresAt).toBe("2026-11-24T11:25:55.000Z");
    expect(rich?.uncertaintyMeters).toBe(8.5);
    expect(rich?.feedStatus).toBe("live");
    expect(store.getCommandableVehicle(richContactId)).toBeNull();

    // Neither spoofed contact with uxs.* predicates nor incomplete vehicle ID may enter command selectors!
    expect(store.getEntity(spoofedContactId)?.kind).toBe("external-contact");
    expect(store.getCommandableVehicle(spoofedContactId)).toBeNull();
    expect(store.getEntity(incompleteVehicleId)?.kind).toBe("generic");
    expect(store.getCommandableVehicle(incompleteVehicleId)).toBeNull();
  });

  // spec: cop-ui / Gapless ENTITY_STATES SKG store, external contact separation, and GraphQL client
  it("sends 25s (< 30s) keepalive frames on WS #2 and reconnects with 500ms -> 30s backoff", () => {
    let nowMs = 1_732_450_000_000;
    const rig = new MockWebSocketRig();
    const store = createSkgStore({
      url: "ws://127.0.0.1:8082/ws",
      now: () => nowMs,
      createSocket: rig.createSocket,
    });

    store.connect();
    rig.latest.serverOpen();
    expect(store.status).toBe("connected");

    nowMs += 25_000;
    vi.advanceTimersByTime(25_000);
    expect(rig.latest.sentFrames).toHaveLength(1);
    expect(JSON.parse(rig.latest.sentFrames[0] as string)).toEqual({
      type: "keepalive",
      timestamp: nowMs,
    });

    rig.latest.serverDrop(1006);
    expect(store.status).toBe("reconnecting");
    vi.advanceTimersByTime(500);
    expect(rig.sockets).toHaveLength(2);
    rig.latest.serverDrop(1006);
    vi.advanceTimersByTime(1_000);
    expect(rig.sockets).toHaveLength(3);

    store.disconnect();
  });

  // spec: cop-ui / Gapless ENTITY_STATES SKG store, external contact separation, and GraphQL client
  it("matches an independent shadow-staging reference model across seeded put/delete/poisoned/synced/reconnect histories", () => {
    const rng = createSeededRng(0x51c60002);
    const rig = new MockWebSocketRig();
    const store = createSkgStore({
      url: "ws://127.0.0.1:8082/ws",
      createSocket: rig.createSocket,
    });

    type RefRow = {
      id: string;
      isVehicle: boolean;
      poisoned: boolean;
      authority?: string;
      rev: number;
    };

    const refLive = new Map<string, RefRow>();
    let refStaged = new Map<string, RefRow>();
    let refSynced = false;
    let nextRev = 1;

    let sawReconnectPrune = false;
    let sawPoisonAndHeal = false;

    store.connect();
    rig.latest.serverOpen();

    const keys = [
      {
        id: "boxbotix.local.robotics.uxs.vehicle.1",
        sysid: 1,
        isVehicle: true,
      },
      {
        id: "boxbotix.local.robotics.uxs.vehicle.2",
        sysid: 2,
        isVehicle: true,
      },
      {
        id: "boxbotix.local.tak.cop.contact.alpha",
        sysid: 0,
        isVehicle: false,
      },
      { id: "boxbotix.local.tak.cop.marker.bravo", sysid: 0, isVehicle: false },
    ];

    for (let step = 0; step < 180; step += 1) {
      const action = rng();
      const target = keys[Math.floor(rng() * keys.length)]!;

      if (action < 0.45) {
        // put
        const rev = nextRev++;
        const auth = rng() < 0.5 ? "companion-teleop" : "direct-crsf-override";
        const payload = target.isVehicle
          ? buildVehicleStoredPayload(target.sysid, {
              "uxs.authority.state": auth,
            })
          : {
              id: target.id,
              triples: [
                makeTriple(target.id, "dc.terms.title", target.id, {
                  source: "cop-ingest",
                }),
                makeTriple(target.id, "geo.location.latitude", 37.7, {
                  source: "cop-ingest",
                }),
                makeTriple(target.id, "geo.location.longitude", -122.4, {
                  source: "cop-ingest",
                }),
              ],
            };
        rig.latest.serverMessage(putFrame(target.id, rev, payload));

        const prior = refSynced
          ? refLive.get(target.id)
          : refStaged.get(target.id);
        if (prior?.poisoned) {
          sawPoisonAndHeal = true;
        }
        const row: RefRow = {
          id: target.id,
          isVehicle: target.isVehicle,
          poisoned: false,
          authority: target.isVehicle ? auth : undefined,
          rev,
        };
        if (refSynced) {
          refLive.set(target.id, row);
        } else {
          refStaged.set(target.id, row);
        }
      } else if (action < 0.62) {
        // delete
        const rev = nextRev++;
        rig.latest.serverMessage(deleteFrame(target.id, rev));
        if (refSynced) {
          refLive.delete(target.id);
        } else {
          refStaged.delete(target.id);
        }
      } else if (action < 0.77) {
        // poisoned
        const rev = nextRev++;
        rig.latest.serverMessage(poisonedFrame(target.id, rev, "decode err"));
        const prior = refSynced
          ? refLive.get(target.id)
          : (refStaged.get(target.id) ?? refLive.get(target.id));
        const row: RefRow = {
          id: target.id,
          isVehicle: prior ? prior.isVehicle : false,
          poisoned: true,
          authority: prior?.authority,
          rev,
        };
        if (refSynced) {
          refLive.set(target.id, row);
        } else {
          refStaged.set(target.id, row);
        }
      } else if (action < 0.9) {
        // synced
        if (!refSynced) {
          const rev = nextRev++;
          const hadKeyMissingInStaged = Array.from(refLive.keys()).some(
            (k) => !refStaged.has(k),
          );
          if (hadKeyMissingInStaged) {
            sawReconnectPrune = true;
          }
          rig.latest.serverMessage(syncedFrame(rev));
          refLive.clear();
          for (const [k, v] of refStaged.entries()) {
            refLive.set(k, v);
          }
          refStaged.clear();
          refSynced = true;
        }
      } else {
        // drop + reconnect (half the time simulating bucket recreation resetting KV revisions to 1)
        rig.latest.serverDrop(1006);
        vi.advanceTimersByTime(500);
        rig.latest.serverOpen();
        refSynced = false;
        refStaged = new Map();
        if (rng() < 0.5) {
          nextRev = 1;
        }
      }

      expect(store.synced).toBe(refSynced);
      expect(Array.from(store.entities.keys()).sort()).toEqual(
        Array.from(refLive.keys()).sort(),
      );
      for (const [k, expected] of refLive.entries()) {
        const actual = store.getEntity(k);
        expect(actual?.poisoned).toBe(expected.poisoned);
        const cmdVehicle = store.getCommandableVehicle(k);
        if (expected.isVehicle && !expected.poisoned) {
          expect(cmdVehicle).not.toBeNull();
          expect(cmdVehicle?.authority).toBe(expected.authority);
        } else {
          expect(cmdVehicle).toBeNull();
        }
      }
    }

    expect(sawReconnectPrune).toBe(true);
    expect(sawPoisonAndHeal).toBe(true);
    store.disconnect();
  });

  // spec: cop-ui / Gapless ENTITY_STATES SKG store, external contact separation, and GraphQL client
  it("projects multi-source scalar triples using SemEngine GetTriple rules across array orders and equal timestamps", () => {
    const store = createSkgStore();
    const base = buildVehicleStoredPayload(7);
    const nonArmedTriples = base.triples.filter(
      (t) => t.predicate !== "uxs.health.armed",
    );

    const newerArmedTrue = makeTriple(base.id, "uxs.health.armed", true, {
      source: "source_z",
      timestamp: "2026-11-24T11:30:02.000000200Z",
    });
    const olderArmedFalse = makeTriple(base.id, "uxs.health.armed", false, {
      source: "source_a",
      timestamp: "2026-11-24T11:30:02.000000100Z",
    });

    // Order 1: newer armed=true followed by older armed=false from an alphabetically earlier source.
    store.ingestRawFrame(
      putFrame(base.id, 1, {
        ...base,
        triples: [...nonArmedTriples, newerArmedTrue, olderArmedFalse],
      }),
    );
    store.ingestRawFrame(syncedFrame(1));
    expect(store.getCommandableVehicle(base.id)?.armed).toBe(true);

    // Order 2: older armed=false followed by newer armed=true (expressed with +01:00 offset = 11:30:02.000000200Z).
    const newerWithOffset = makeTriple(base.id, "uxs.health.armed", true, {
      source: "source_z",
      timestamp: "2026-11-24T12:30:02.000000200+01:00",
    });
    store.ingestRawFrame(
      putFrame(base.id, 2, {
        ...base,
        triples: [...nonArmedTriples, olderArmedFalse, newerWithOffset],
      }),
    );
    expect(store.getCommandableVehicle(base.id)?.armed).toBe(true);

    // Equal timestamps, different sources -> ascending UTF-8 source ("alpha_src" < "zeta_src") wins in both array orders!
    const tieAlphaTrue = makeTriple(base.id, "uxs.health.armed", true, {
      source: "alpha_src",
      timestamp: "2026-11-24T11:35:00.000Z",
    });
    const tieZetaFalse = makeTriple(base.id, "uxs.health.armed", false, {
      source: "zeta_src",
      timestamp: "2026-11-24T11:35:00.000Z",
    });

    store.ingestRawFrame(
      putFrame(base.id, 3, {
        ...base,
        triples: [...nonArmedTriples, tieAlphaTrue, tieZetaFalse],
      }),
    );
    expect(store.getCommandableVehicle(base.id)?.armed).toBe(true);

    store.ingestRawFrame(
      putFrame(base.id, 4, {
        ...base,
        triples: [...nonArmedTriples, tieZetaFalse, tieAlphaTrue],
      }),
    );
    expect(store.getCommandableVehicle(base.id)?.armed).toBe(true);

    // Equal timestamps AND equal sources -> first stored assertion wins.
    const sameSourceFirstFalse = makeTriple(
      base.id,
      "uxs.health.armed",
      false,
      {
        source: "same_src",
        timestamp: "2026-11-24T11:40:00.000Z",
      },
    );
    const sameSourceSecondTrue = makeTriple(base.id, "uxs.health.armed", true, {
      source: "same_src",
      timestamp: "2026-11-24T11:40:00.000Z",
    });
    store.ingestRawFrame(
      putFrame(base.id, 5, {
        ...base,
        triples: [
          ...nonArmedTriples,
          sameSourceFirstFalse,
          sameSourceSecondTrue,
        ],
      }),
    );
    expect(store.getCommandableVehicle(base.id)?.armed).toBe(false);
  });

  // spec: cop-ui / Gapless ENTITY_STATES SKG store, external contact separation, and GraphQL client
  it("retains a visible poisoned entity excluded from command selectors when reconnect snapshot resets revisions to 1", () => {
    const rig = new MockWebSocketRig();
    const store = createSkgStore({
      url: "ws://127.0.0.1:8082/ws",
      createSocket: rig.createSocket,
    });

    store.connect();
    rig.latest.serverOpen();

    const v1 = buildVehicleStoredPayload(1, {
      "uxs.domain.kind": "quadcopter",
      "uxs.authority.state": "companion-teleop",
    });
    rig.latest.serverMessage(putFrame(v1.id, 100, v1));
    rig.latest.serverMessage(syncedFrame(100));
    expect(store.getCommandableVehicle(v1.id)?.kvRevision).toBe(100);
    expect(store.commandableVehicles).toHaveLength(1);

    // Disconnect and simulate ENTITY_STATES bucket recreation where the key is poisoned at revision 1.
    rig.latest.serverDrop(1006);
    vi.advanceTimersByTime(500);
    rig.latest.serverOpen();

    rig.latest.serverMessage(
      poisonedFrame(v1.id, 1, "recreated bucket corrupt entry"),
    );
    rig.latest.serverMessage(syncedFrame(1));

    const retained = store.getVehicle(v1.id);
    expect(retained).not.toBeNull();
    expect(retained?.poisoned).toBe(true);
    expect(retained?.kvRevision).toBe(1);
    expect(retained?.poisonError).toBe("recreated bucket corrupt entry");
    expect(retained?.domain).toBe("quadcopter");
    expect(store.entities.has(v1.id)).toBe(true);
    expect(store.getCommandableVehicle(v1.id)).toBeNull();
    expect(store.commandableVehicles).toHaveLength(0);

    store.disconnect();
  });

  // spec: cop-ui / Gapless ENTITY_STATES SKG store, external contact separation, and GraphQL client
  it("starts fresh SKG state on explicit connect(overrideUrl) without leaking prior endpoint vehicles or poison diagnostics", () => {
    const rig = new MockWebSocketRig();
    const urlA = "ws://127.0.0.1:8082/ws-a";
    const urlB = "ws://127.0.0.1:8092/ws-b";
    const store = createSkgStore({
      url: urlA,
      createSocket: rig.createSocket,
    });

    store.connect();
    rig.latest.serverOpen();
    const v1 = buildVehicleStoredPayload(1, {
      "uxs.domain.kind": "quadcopter",
      "dc.terms.title": "Alpha-A-Vehicle",
    });
    const v2 = buildVehicleStoredPayload(2, {
      "uxs.domain.kind": "rover",
    });
    rig.latest.serverMessage(putFrame(v1.id, 10, v1));
    rig.latest.serverMessage(putFrame(v2.id, 11, v2));
    rig.latest.serverMessage(syncedFrame(11));
    expect(store.getCommandableVehicle(v1.id)?.sysid).toBe(1);
    expect(store.commandableVehicles).toHaveLength(2);

    // Explicitly switch to endpoint B: A's vehicles MUST NOT remain readable before B's barrier or leak into B's poison diagnostics!
    store.connect(urlB);
    expect(store.url).toBe(urlB);
    expect(store.synced).toBe(false);
    expect(store.lastSyncedRevision).toBeNull();
    expect(store.entities.size).toBe(0);
    expect(store.getCommandableVehicle(v1.id)).toBeNull();
    expect(store.commandableVehicles).toEqual([]);

    rig.latest.serverOpen();
    const v3 = buildVehicleStoredPayload(3, {
      "uxs.domain.kind": "surface-boat",
    });
    // Key v1.id arrives poisoned on endpoint B with zero prior B record -> must NOT inherit endpoint A's vehicle kind/title/coordinates!
    rig.latest.serverMessage(
      poisonedFrame(v1.id, 1, "corrupt entry on endpoint B"),
    );
    rig.latest.serverMessage(putFrame(v3.id, 2, v3));
    rig.latest.serverMessage(syncedFrame(2));

    expect(store.entities.size).toBe(2);
    expect(store.entities.has(v2.id)).toBe(false);
    expect(store.commandableVehicles.map((v) => v.sysid)).toEqual([3]);

    const poisonedB = store.getEntity(v1.id);
    expect(poisonedB?.poisoned).toBe(true);
    expect(poisonedB?.kind).toBe("generic");
    expect(poisonedB?.title).toBeNull();
    expect(poisonedB?.location).toBeNull();
    expect(store.getVehicle(v1.id)).toBeNull();
    expect(store.getCommandableVehicle(v1.id)).toBeNull();

    store.disconnect();
  });

  // spec: cop-ui / Gapless ENTITY_STATES SKG store, external contact separation, and GraphQL client
  it("keeps poseObservedAt and poseIngestedAtMs unrefreshed across pre-synced snapshot replays and post-synced health-only updates", () => {
    const initialReceiptMs = Date.parse("2026-11-24T12:00:00.000Z");
    let browserNowMs = initialReceiptMs;
    const store = createSkgStore({ now: () => browserNowMs });

    const oldPoseIso = "2026-11-24T11:00:00.000Z";
    const replayPayload = buildVehicleStoredPayload(
      1,
      {
        "uxs.kinematics.ned": [0, 0, 0],
        "uxs.kinematics.vx": 0,
        "uxs.kinematics.vy": 0,
        "uxs.kinematics.vz": 0,
      },
      {
        observedAt: oldPoseIso,
        updatedAt: new Date(browserNowMs).toISOString(),
      },
    );

    // Pre-synced snapshot replay of an hour-old observation with default zero NED.
    store.ingestRawFrame(putFrame(replayPayload.id, 1, replayPayload));
    store.ingestRawFrame(syncedFrame(1));

    const replayed = store.getCommandableVehicle(replayPayload.id);
    expect(replayed?.poseObservedAt).toBe(oldPoseIso);
    expect(replayed?.poseIngestedAtMs).toBe(initialReceiptMs);
    expect(replayed?.poseIngestedLive).toBe(false);
    expect(replayed?.hasLocalPos).toBe(false);

    // Live post-synced genuine pose update at +500ms with measured local NED.
    browserNowMs += 500;
    const livePoseIso = new Date(browserNowMs).toISOString();
    const livePosePayload = buildVehicleStoredPayload(
      1,
      {
        "uxs.kinematics.ned": [15, -8, -12],
      },
      {
        observedAt: livePoseIso,
        updatedAt: livePoseIso,
      },
    );
    store.ingestRawFrame(putFrame(livePosePayload.id, 2, livePosePayload));
    const afterLivePose = store.getCommandableVehicle(livePosePayload.id);
    expect(afterLivePose?.poseObservedAt).toBe(livePoseIso);
    expect(afterLivePose?.poseIngestedAtMs).toBe(browserNowMs);
    expect(afterLivePose?.poseIngestedLive).toBe(true);
    expect(afterLivePose?.hasLocalPos).toBe(true);

    // Health-only post-synced update 4 seconds later (pose triples retain livePoseIso).
    const poseReceiptAtLive = browserNowMs;
    browserNowMs += 4_000;
    const healthIso = new Date(browserNowMs).toISOString();
    const healthOnlyTriples = livePosePayload.triples.map((t) =>
      t.predicate.startsWith("uxs.health.") ||
      t.predicate === "uxs.authority.state"
        ? {
            ...t,
            object:
              t.predicate === "uxs.health.crcerrors"
                ? 9
                : t.predicate === "uxs.authority.state"
                  ? "direct-crsf-override"
                  : t.object,
            timestamp: healthIso,
          }
        : t,
    );
    store.ingestRawFrame(
      putFrame(livePosePayload.id, 3, {
        id: livePosePayload.id,
        updated_at: healthIso,
        triples: healthOnlyTriples,
      }),
    );

    const afterHealthOnly = store.getCommandableVehicle(livePosePayload.id);
    expect(afterHealthOnly?.authority).toBe("direct-crsf-override");
    expect(afterHealthOnly?.sysStatus.crcErrors).toBe(9);
    expect(afterHealthOnly?.observedAt).toBe(healthIso);
    expect(afterHealthOnly?.poseObservedAt).toBe(livePoseIso);
    expect(afterHealthOnly?.poseIngestedAtMs).toBe(poseReceiptAtLive);
    expect(afterHealthOnly?.poseIngestedLive).toBe(true);
  });

  // spec: cop-ui / Gapless ENTITY_STATES SKG store, external contact separation, and GraphQL client
  it("associates reconnect snapshot poseIngestedAtMs with each observation across newer, identical, older, and historical-replay snapshots", () => {
    let browserNowMs = Date.parse("2026-11-24T12:10:00.000Z");
    const rig = new MockWebSocketRig();
    const store = createSkgStore({
      url: "ws://127.0.0.1:8082/ws",
      now: () => browserNowMs,
      createSocket: rig.createSocket,
    });

    store.connect();
    rig.latest.serverOpen();

    const t0Iso = new Date(browserNowMs).toISOString();
    const oldReplayT0Iso = "2026-11-24T10:00:00.000Z";
    const vNewer = buildVehicleStoredPayload(
      1,
      { "uxs.kinematics.headingdeg": 45 },
      { observedAt: t0Iso, updatedAt: t0Iso },
    );
    const vIdentical = buildVehicleStoredPayload(
      2,
      { "uxs.kinematics.headingdeg": 90 },
      { observedAt: t0Iso, updatedAt: t0Iso },
    );
    const vOlder = buildVehicleStoredPayload(
      3,
      { "uxs.kinematics.headingdeg": 180 },
      { observedAt: t0Iso, updatedAt: t0Iso },
    );
    const vReplay = buildVehicleStoredPayload(
      4,
      { "uxs.kinematics.headingdeg": 225 },
      { observedAt: oldReplayT0Iso, updatedAt: t0Iso },
    );

    rig.latest.serverMessage(putFrame(vNewer.id, 1, vNewer));
    rig.latest.serverMessage(putFrame(vIdentical.id, 2, vIdentical));
    rig.latest.serverMessage(putFrame(vOlder.id, 3, vOlder));
    rig.latest.serverMessage(putFrame(vReplay.id, 4, vReplay));
    rig.latest.serverMessage(syncedFrame(4));

    // Deliver a live post-synced advance at +500ms for v1, v2, v3.
    browserNowMs += 500;
    const liveReceiptMs = browserNowMs;
    const liveIso = new Date(browserNowMs).toISOString();
    for (const [sysid, hdg] of [
      [1, 50],
      [2, 95],
      [3, 185],
    ] as const) {
      const livePayload = buildVehicleStoredPayload(
        sysid,
        { "uxs.kinematics.headingdeg": hdg },
        { observedAt: liveIso, updatedAt: liveIso },
      );
      rig.latest.serverMessage(
        putFrame(livePayload.id, 4 + sysid, livePayload),
      );
    }

    // Disconnect WS #2 for 10 seconds, then reconnect and replay pre-synced snapshots.
    browserNowMs += 10_000;
    rig.latest.serverDrop(1006);
    vi.advanceTimersByTime(500);
    browserNowMs += 500;
    const reconnectReceiptMs = browserNowMs;
    rig.latest.serverOpen();

    // (a) v1: genuinely newer current pose observed 100ms before reconnect.
    const newerIso = new Date(reconnectReceiptMs - 100).toISOString();
    const vNewerSnap = buildVehicleStoredPayload(
      1,
      { "uxs.kinematics.headingdeg": 120, "uxs.kinematics.ned": [30, -10, -8] },
      { observedAt: newerIso, updatedAt: newerIso },
    );
    // (b) v2: identical pose from 10.5s ago.
    const vIdenticalSnap = buildVehicleStoredPayload(
      2,
      { "uxs.kinematics.headingdeg": 95 },
      { observedAt: liveIso, updatedAt: newerIso },
    );
    // (c) v3: older pose than prior live state.
    const olderIso = new Date(liveReceiptMs - 2_000).toISOString();
    const vOlderSnap = buildVehicleStoredPayload(
      3,
      { "uxs.kinematics.headingdeg": 170 },
      { observedAt: olderIso, updatedAt: newerIso },
    );
    // (d) v4: newer than 2-hour-old prior, but still 1 hour old (replay).
    const replayNewerIso = "2026-11-24T11:10:00.000Z";
    const vReplaySnap = buildVehicleStoredPayload(
      4,
      { "uxs.kinematics.headingdeg": 240 },
      { observedAt: replayNewerIso, updatedAt: newerIso },
    );

    rig.latest.serverMessage(putFrame(vNewerSnap.id, 10, vNewerSnap));
    rig.latest.serverMessage(putFrame(vIdenticalSnap.id, 11, vIdenticalSnap));
    rig.latest.serverMessage(putFrame(vOlderSnap.id, 12, vOlderSnap));
    rig.latest.serverMessage(putFrame(vReplaySnap.id, 13, vReplaySnap));
    rig.latest.serverMessage(syncedFrame(13));

    const gotNewer = store.getCommandableVehicle(vNewer.id)!;
    const gotIdentical = store.getCommandableVehicle(vIdentical.id)!;
    const gotOlder = store.getCommandableVehicle(vOlder.id)!;
    const gotReplay = store.getCommandableVehicle(vReplay.id)!;

    expect(gotNewer.poseObservedAt).toBe(newerIso);
    expect(gotNewer.poseIngestedAtMs).toBe(reconnectReceiptMs);
    expect(gotNewer.poseIngestedLive).toBe(false);

    expect(gotIdentical.poseObservedAt).toBe(liveIso);
    expect(gotIdentical.poseIngestedAtMs).toBe(liveReceiptMs);
    expect(gotIdentical.poseIngestedLive).toBe(false);

    expect(gotOlder.poseObservedAt).toBe(olderIso);
    expect(gotOlder.poseIngestedAtMs).toBeLessThan(0);
    expect(gotOlder.poseIngestedLive).toBe(false);

    expect(gotReplay.poseObservedAt).toBe(replayNewerIso);
    expect(gotReplay.poseIngestedAtMs).toBe(reconnectReceiptMs);
    expect(gotReplay.poseIngestedLive).toBe(false);

    store.disconnect();
  });

  // spec: cop-ui / Gapless ENTITY_STATES SKG store, external contact separation, and GraphQL client
  it("distinguishes legacy v1 unannotated zero-pose triples from measured local-zero NED and measured 0° North triples", () => {
    const store = createSkgStore();
    const tsIso = "2026-11-24T12:15:00.000Z";

    // Legacy v1 global-only payload: confidence=1 with omitted context on zero-valued local NED, [1,0,0,0] quat, [0,0,0] rates, and 0° heading.
    const legacyV1 = buildVehicleStoredPayload(
      1,
      {
        "uxs.attitude.quaternion": [1, 0, 0, 0],
        "uxs.attitude.rates": [0, 0, 0],
        "uxs.kinematics.ned": [0, 0, 0],
        "uxs.kinematics.vx": 0,
        "uxs.kinematics.vy": 0,
        "uxs.kinematics.vz": 0,
        "uxs.kinematics.headingdeg": 0,
      },
      { observedAt: tsIso, updatedAt: tsIso },
    );

    // Modern payload at local origin [0, 0, 0] facing 0° North (#32 + #33 received, #31 still unmeasured).
    const modernBase = buildVehicleStoredPayload(
      2,
      {
        "uxs.attitude.quaternion": [0, 0, 0, 0],
        "uxs.kinematics.ned": [0, 0, 0],
        "uxs.kinematics.vx": 0,
        "uxs.kinematics.vy": 0,
        "uxs.kinematics.vz": 0,
        "uxs.kinematics.headingdeg": 0,
      },
      { observedAt: tsIso, updatedAt: tsIso },
    );
    const modernMeasuredZero = {
      ...modernBase,
      triples: modernBase.triples.map((t) => {
        if (t.predicate === "uxs.attitude.quaternion") {
          return {
            ...t,
            confidence: 0,
            timestamp: "1970-01-01T00:00:00Z",
          };
        }
        if (
          t.predicate === "uxs.kinematics.ned" ||
          t.predicate === "uxs.kinematics.headingdeg" ||
          t.predicate.startsWith("geo.location.")
        ) {
          return {
            ...t,
            confidence: 1,
            context: "measured",
          };
        }
        return t;
      }),
    };

    store.ingestRawFrame(putFrame(legacyV1.id, 1, legacyV1));
    store.ingestRawFrame(
      putFrame(modernMeasuredZero.id, 2, modernMeasuredZero),
    );
    store.ingestRawFrame(syncedFrame(2));

    const v1Parsed = store.getCommandableVehicle(legacyV1.id)!;
    expect(v1Parsed.hasLocalPos).toBe(false);
    expect(v1Parsed.hasAttitude).toBe(false);
    expect(v1Parsed.hasHeading).toBe(false);
    expect(v1Parsed.hasGlobalPos).toBe(true);

    const v2Parsed = store.getCommandableVehicle(modernMeasuredZero.id)!;
    expect(v2Parsed.hasLocalPos).toBe(true);
    expect(v2Parsed.hasAttitude).toBe(false);
    expect(v2Parsed.hasHeading).toBe(true);
    expect(v2Parsed.hasGlobalPos).toBe(true);
  });

  // spec: cop-ui / Gapless ENTITY_STATES SKG store, external contact separation, and GraphQL client
  it("expires poseIngestedLive and poseIngestedAtMs when a post-synced put carries a regressed pose timestamp while preserving identical and health-only puts", () => {
    let browserNowMs = Date.parse("2026-11-24T13:10:00.000Z");
    const store = createSkgStore({ now: () => browserNowMs });

    const t0Iso = new Date(browserNowMs).toISOString();
    const initPayload = buildVehicleStoredPayload(
      1,
      { "uxs.kinematics.headingdeg": 10 },
      { observedAt: t0Iso, updatedAt: t0Iso },
    );
    store.ingestRawFrame(putFrame(initPayload.id, 1, initPayload));
    store.ingestRawFrame(syncedFrame(1));

    // Deliver a live post-synced 20° pose at +250ms.
    browserNowMs += 250;
    const liveReceiptMs = browserNowMs;
    const live20Iso = new Date(browserNowMs).toISOString();
    const live20Payload = buildVehicleStoredPayload(
      1,
      { "uxs.kinematics.headingdeg": 20 },
      { observedAt: live20Iso, updatedAt: live20Iso },
    );
    store.ingestRawFrame(putFrame(live20Payload.id, 2, live20Payload));
    const afterLive20 = store.getCommandableVehicle(live20Payload.id)!;
    expect(afterLive20.poseIngestedLive).toBe(true);
    expect(afterLive20.poseIngestedAtMs).toBe(liveReceiptMs);

    // Identical pose observation at rev 3 preserves liveReceiptMs and poseIngestedLive=true.
    browserNowMs += 100;
    store.ingestRawFrame(putFrame(live20Payload.id, 3, live20Payload));
    const afterIdentical = store.getCommandableVehicle(live20Payload.id)!;
    expect(afterIdentical.poseIngestedLive).toBe(true);
    expect(afterIdentical.poseIngestedAtMs).toBe(liveReceiptMs);

    // Higher KV revision (rev 4) carrying an hour-old 30° pose MUST expire poseIngestedLive and poseIngestedAtMs!
    browserNowMs += 100;
    const hourOldBaseMs = browserNowMs - 3_600_000;
    const hourOld100Iso = new Date(hourOldBaseMs + 100).toISOString();
    const regressed30Payload = buildVehicleStoredPayload(
      1,
      { "uxs.kinematics.headingdeg": 30 },
      {
        observedAt: hourOld100Iso,
        updatedAt: new Date(browserNowMs).toISOString(),
      },
    );
    store.ingestRawFrame(
      putFrame(regressed30Payload.id, 4, regressed30Payload),
    );
    const afterRegressed1 = store.getCommandableVehicle(regressed30Payload.id)!;
    expect(afterRegressed1.headingDeg).toBe(30);
    expect(afterRegressed1.poseObservedAt).toBe(hourOld100Iso);
    expect(afterRegressed1.poseIngestedLive).toBe(false);
    expect(afterRegressed1.poseIngestedAtMs).toBeLessThan(0);

    // Second and third replay updates advancing within the 11:00 replay window (+100ms, +200ms)
    // MUST stay stale because they remain older than the entity's 13:10:00.250Z high-water mark!
    browserNowMs += 100;
    const hourOld200Iso = new Date(hourOldBaseMs + 200).toISOString();
    const regressed31Payload = buildVehicleStoredPayload(
      1,
      { "uxs.kinematics.headingdeg": 31 },
      {
        observedAt: hourOld200Iso,
        updatedAt: new Date(browserNowMs).toISOString(),
      },
    );
    store.ingestRawFrame(
      putFrame(regressed31Payload.id, 5, regressed31Payload),
    );
    const afterRegressed2 = store.getCommandableVehicle(regressed31Payload.id)!;
    expect(afterRegressed2.headingDeg).toBe(31);
    expect(afterRegressed2.poseObservedAt).toBe(hourOld200Iso);
    expect(afterRegressed2.poseIngestedLive).toBe(false);
    expect(afterRegressed2.poseIngestedAtMs).toBeLessThan(0);

    browserNowMs += 100;
    const hourOld300Iso = new Date(hourOldBaseMs + 300).toISOString();
    const regressed32Payload = buildVehicleStoredPayload(
      1,
      { "uxs.kinematics.headingdeg": 32 },
      {
        observedAt: hourOld300Iso,
        updatedAt: new Date(browserNowMs).toISOString(),
      },
    );
    store.ingestRawFrame(
      putFrame(regressed32Payload.id, 6, regressed32Payload),
    );
    const afterRegressed3 = store.getCommandableVehicle(regressed32Payload.id)!;
    expect(afterRegressed3.poseIngestedLive).toBe(false);
    expect(afterRegressed3.poseIngestedAtMs).toBeLessThan(0);

    // Duplicate replay put (rev 7) and health-only update while holding replay pose (rev 8) MUST also remain stale!
    browserNowMs += 100;
    store.ingestRawFrame(
      putFrame(regressed32Payload.id, 7, regressed32Payload),
    );
    expect(
      store.getCommandableVehicle(regressed32Payload.id)?.poseIngestedLive,
    ).toBe(false);
    expect(
      store.getCommandableVehicle(regressed32Payload.id)?.poseIngestedAtMs,
    ).toBeLessThan(0);

    browserNowMs += 100;
    const healthAtReplayIso = new Date(browserNowMs).toISOString();
    const healthDuringReplayTriples = regressed32Payload.triples.map((t) =>
      t.predicate === "uxs.health.armed"
        ? { ...t, object: false, timestamp: healthAtReplayIso }
        : t,
    );
    store.ingestRawFrame(
      putFrame(regressed32Payload.id, 8, {
        id: regressed32Payload.id,
        updated_at: healthAtReplayIso,
        triples: healthDuringReplayTriples,
      }),
    );
    const afterHealthDuringReplay = store.getCommandableVehicle(
      regressed32Payload.id,
    )!;
    expect(afterHealthDuringReplay.armed).toBe(false);
    expect(afterHealthDuringReplay.poseObservedAt).toBe(hourOld300Iso);
    expect(afterHealthDuringReplay.poseIngestedLive).toBe(false);
    expect(afterHealthDuringReplay.poseIngestedAtMs).toBeLessThan(0);

    // Genuinely new observation advancing past the high-water mark (rev 9) restores live freshness!
    browserNowMs += 100;
    const newLiveReceiptMs = browserNowMs;
    const newLiveIso = new Date(browserNowMs).toISOString();
    const live35Payload = buildVehicleStoredPayload(
      1,
      { "uxs.kinematics.headingdeg": 35 },
      { observedAt: newLiveIso, updatedAt: newLiveIso },
    );
    store.ingestRawFrame(putFrame(live35Payload.id, 9, live35Payload));
    const afterRecoveredLive = store.getCommandableVehicle(live35Payload.id)!;
    expect(afterRecoveredLive.headingDeg).toBe(35);
    expect(afterRecoveredLive.poseObservedAt).toBe(newLiveIso);
    expect(afterRecoveredLive.poseIngestedLive).toBe(true);
    expect(afterRecoveredLive.poseIngestedAtMs).toBe(newLiveReceiptMs);
  });

  // spec: cop-ui / Gapless ENTITY_STATES SKG store, external contact separation, and GraphQL client
  it("tracks measured heading -> invalidated unknown heading (confidence=0 at obsTS) -> health-only update -> measured North (0°) without resurrecting stale heading", () => {
    let browserNowMs = Date.parse("2026-11-24T13:20:00.000Z");
    const store = createSkgStore({ now: () => browserNowMs });
    const entityId = "boxbotix.local.robotics.uxs.vehicle.1";

    const buildStepPayload = (opts: {
      hdgObj: number;
      hdgConf: number;
      hdgCtx?: string;
      hdgTs: string;
      geoTs: string;
      healthTs: string;
      armed: boolean;
    }) => {
      const base = buildVehicleStoredPayload(
        1,
        {
          "uxs.attitude.quaternion": [0, 0, 0, 0],
          "uxs.attitude.rates": [0, 0, 0],
          "uxs.kinematics.ned": [0, 0, 0],
          "uxs.kinematics.vx": 0,
          "uxs.kinematics.vy": 0,
          "uxs.kinematics.vz": 0,
          "uxs.kinematics.headingdeg": opts.hdgObj,
          "uxs.health.armed": opts.armed,
        },
        { observedAt: opts.healthTs, updatedAt: opts.healthTs },
      );
      return {
        ...base,
        triples: base.triples.map((tr) => {
          if (
            tr.predicate.startsWith("uxs.attitude.") ||
            (tr.predicate.startsWith("uxs.kinematics.") &&
              tr.predicate !== "uxs.kinematics.headingdeg")
          ) {
            return {
              ...tr,
              confidence: 0,
              timestamp: "1970-01-01T00:00:00Z",
            };
          }
          if (tr.predicate === "uxs.kinematics.headingdeg") {
            return {
              subject: entityId,
              predicate: tr.predicate,
              object: opts.hdgObj,
              source: tr.source,
              timestamp: opts.hdgTs,
              confidence: opts.hdgConf,
              ...(opts.hdgCtx ? { context: opts.hdgCtx } : {}),
            };
          }
          if (tr.predicate.startsWith("geo.location.")) {
            return {
              ...tr,
              confidence: 1,
              context: "measured",
              timestamp: opts.geoTs,
            };
          }
          return tr;
        }),
      };
    };

    // 1. Measured fallback heading 274.5° at t1.
    const t1Iso = "2026-11-24T13:20:00.000Z";
    store.ingestRawFrame(
      putFrame(
        entityId,
        1,
        buildStepPayload({
          hdgObj: 274.5,
          hdgConf: 1,
          hdgCtx: "measured",
          hdgTs: t1Iso,
          geoTs: t1Iso,
          healthTs: t1Iso,
          armed: false,
        }),
      ),
    );
    store.ingestRawFrame(syncedFrame(1));
    expect(store.getCommandableVehicle(entityId)?.hasHeading).toBe(true);
    expect(store.getCommandableVehicle(entityId)?.headingDeg).toBe(274.5);

    // 2. Direct telemetry reports unknown heading (UINT16_MAX) at t2 -> ReplaceBySource stores confidence=0 at t2.
    browserNowMs += 250;
    const t2ReceiptMs = browserNowMs;
    const t2Iso = "2026-11-24T13:20:00.250Z";
    store.ingestRawFrame(
      putFrame(
        entityId,
        2,
        buildStepPayload({
          hdgObj: 0,
          hdgConf: 0,
          hdgTs: t2Iso,
          geoTs: t2Iso,
          healthTs: t2Iso,
          armed: false,
        }),
      ),
    );
    const afterUnknown = store.getCommandableVehicle(entityId)!;
    expect(afterUnknown.hasHeading).toBe(false);
    expect(afterUnknown.headingDeg).toBe(0);
    expect(afterUnknown.poseObservedAt).toBe(t2Iso);
    expect(afterUnknown.poseIngestedAtMs).toBe(t2ReceiptMs);

    // 3. Health-only update at t3 while heading remains unknown -> heading stays unavailable and poseObservedAt stays at t2.
    browserNowMs += 250;
    const t3Iso = "2026-11-24T13:20:00.500Z";
    store.ingestRawFrame(
      putFrame(
        entityId,
        3,
        buildStepPayload({
          hdgObj: 0,
          hdgConf: 0,
          hdgTs: t2Iso,
          geoTs: t2Iso,
          healthTs: t3Iso,
          armed: true,
        }),
      ),
    );
    const afterHealthWhileUnknown = store.getCommandableVehicle(entityId)!;
    expect(afterHealthWhileUnknown.armed).toBe(true);
    expect(afterHealthWhileUnknown.hasHeading).toBe(false);
    expect(afterHealthWhileUnknown.poseObservedAt).toBe(t2Iso);
    expect(afterHealthWhileUnknown.poseIngestedAtMs).toBe(t2ReceiptMs);

    // 4. Measured North (0°) at t4 restores hasHeading=true, headingDeg=0.
    browserNowMs += 250;
    const t4Iso = "2026-11-24T13:20:00.750Z";
    store.ingestRawFrame(
      putFrame(
        entityId,
        4,
        buildStepPayload({
          hdgObj: 0,
          hdgConf: 1,
          hdgCtx: "measured",
          hdgTs: t4Iso,
          geoTs: t4Iso,
          healthTs: t4Iso,
          armed: true,
        }),
      ),
    );
    const afterNorth = store.getCommandableVehicle(entityId)!;
    expect(afterNorth.hasHeading).toBe(true);
    expect(afterNorth.headingDeg).toBe(0);
    expect(afterNorth.poseObservedAt).toBe(t4Iso);
  });

  // spec: cop-ui / Gapless ENTITY_STATES SKG store, external contact separation, and GraphQL client
  it("preserves whether #1 SYS_STATUS has been observed across heartbeat-before-SYS_STATUS, measured zero, and healthy -> poisoned -> recovered updates", () => {
    const store = createSkgStore();
    const entityId = "boxbotix.local.robotics.uxs.vehicle.7";

    // 1. HEARTBEAT birth before #1 SYS_STATUS (confidence=0, 0/0/0) -> hasSysStatus=false.
    const birthPayload = buildVehicleStoredPayload(
      7,
      {},
      { hasSysStatus: false },
    );
    store.ingestRawFrame(putFrame(entityId, 1, birthPayload));
    store.ingestRawFrame(syncedFrame(1));
    const afterBirth = store.getCommandableVehicle(entityId);
    expect(afterBirth?.sysStatus).toEqual({
      failsafes: 0,
      crcErrors: 1,
      loopDtUs: 0,
      maxLoopDtUs: 0,
      rttUs: 0,
      hasSysStatus: false,
    });

    // 2. Genuinely measured zero #1 SYS_STATUS (confidence=1, context="measured", 0/0/0) -> hasSysStatus=true.
    const measuredZeroPayload = buildVehicleStoredPayload(
      7,
      {
        "uxs.health.loopdtus": 0,
        "uxs.health.maxloopdtus": 0,
        "uxs.health.rttus": 0,
      },
      { hasSysStatus: true },
    );
    store.ingestRawFrame(putFrame(entityId, 2, measuredZeroPayload));
    expect(store.getCommandableVehicle(entityId)?.sysStatus).toEqual({
      failsafes: 0,
      crcErrors: 1,
      loopDtUs: 0,
      maxLoopDtUs: 0,
      rttUs: 0,
      hasSysStatus: true,
    });

    // 3. Healthy nonzero -> poisoned -> recovered: commandableVehicles excludes poisoned entry until recovered.
    const healthyPayload = buildVehicleStoredPayload(
      7,
      {
        "uxs.health.loopdtus": 2500,
        "uxs.health.maxloopdtus": 3100,
        "uxs.health.rttus": 420,
      },
      { hasSysStatus: true },
    );
    store.ingestRawFrame(putFrame(entityId, 3, healthyPayload));
    expect(store.commandableVehicles).toHaveLength(1);
    expect(store.commandableVehicles[0]?.sysStatus.loopDtUs).toBe(2500);

    store.ingestRawFrame(
      JSON.stringify({
        type: "poisoned",
        bucket: "ENTITY_STATES",
        key: entityId,
        revision: 4,
        error: "corrupt kv entry",
      }),
    );
    expect(store.vehicles[0]?.poisoned).toBe(true);
    expect(store.commandableVehicles).toHaveLength(0);
    expect(store.getCommandableVehicle(entityId)).toBeNull();

    const recoveredPayload = buildVehicleStoredPayload(
      7,
      {
        "uxs.health.loopdtus": 1800,
        "uxs.health.maxloopdtus": 2200,
        "uxs.health.rttus": 310,
      },
      { hasSysStatus: true },
    );
    store.ingestRawFrame(putFrame(entityId, 5, recoveredPayload));
    expect(store.commandableVehicles).toHaveLength(1);
    expect(store.getCommandableVehicle(entityId)?.sysStatus).toEqual({
      failsafes: 0,
      crcErrors: 1,
      loopDtUs: 1800,
      maxLoopDtUs: 2200,
      rttUs: 310,
      hasSysStatus: true,
    });
  });

  // spec: cop-ui / Gapless ENTITY_STATES SKG store, external contact separation, and GraphQL client
  // spec: cop-ui / Fleet health readiness, per-vehicle observation freshness, and last-known fault retention
  it("tracks stateIngestedAtMs and stateIngestedLive across stationary heartbeats while preserving stale age across reconnect snapshot replays", () => {
    const rig = new MockWebSocketRig();
    let browserNowMs = Date.parse("2026-11-25T12:00:00.000Z");
    const store = createSkgStore({
      url: "ws://127.0.0.1:8082/ws",
      createSocket: rig.createSocket,
      now: () => browserNowMs,
    });

    store.connect();
    rig.latest.serverOpen();

    // 1. Initial pre-synced snapshot from 20s ago sets stateIngestedLive=false.
    const initialObsIso = new Date(browserNowMs - 20_000).toISOString();
    const initialSnapshot = buildVehicleStoredPayload(
      1,
      {
        "uxs.domain.kind": "quadcopter",
        "uxs.authority.state": "companion-teleop",
        "uxs.health.armed": true,
      },
      {
        observedAt: initialObsIso,
        updatedAt: new Date(browserNowMs).toISOString(),
      },
    );
    rig.latest.serverMessage(putFrame(initialSnapshot.id, 1, initialSnapshot));
    rig.latest.serverMessage(syncedFrame(1));

    const v0 = store.getCommandableVehicle(initialSnapshot.id)!;
    expect(v0.stateObservedAt).toBe(initialObsIso);
    expect(v0.stateIngestedAtMs).toBe(browserNowMs);
    expect(v0.stateIngestedLive).toBe(false);

    // 2. Stationary 1 Hz #0 HEARTBEAT update advances health/authority timestamps while poseObservedAt stays frozen!
    const liveHbReceiptMs = browserNowMs + 1_000;
    browserNowMs = liveHbReceiptMs;
    const liveHbIso = "2026-11-25T12:00:01.000000500Z";
    const stationaryHeartbeatPayload = {
      ...initialSnapshot,
      updated_at: liveHbIso,
      triples: initialSnapshot.triples.map((triple) =>
        triple.predicate.startsWith("uxs.health.") ||
        triple.predicate === "uxs.authority.state" ||
        triple.predicate === "uxs.mode.companion" ||
        triple.predicate === "uxs.link.tier" ||
        triple.predicate === "uxs.domain.kind"
          ? { ...triple, timestamp: liveHbIso }
          : triple,
      ),
    };
    rig.latest.serverMessage(
      putFrame(initialSnapshot.id, 2, stationaryHeartbeatPayload),
    );

    const vLiveHb = store.getCommandableVehicle(initialSnapshot.id)!;
    expect(vLiveHb.stateObservedAt).toBe(liveHbIso);
    expect(vLiveHb.stateIngestedAtMs).toBe(liveHbReceiptMs);
    expect(vLiveHb.stateIngestedLive).toBe(true);
    // Pose did NOT advance on stationary heartbeat-only update:
    expect(vLiveHb.poseObservedAt).toBe(initialObsIso);
    expect(vLiveHb.poseIngestedLive).toBe(false);

    // 3. Vehicle goes silent for 10s; WS2 drops, reconnects, and replays the unchanged rev=2 snapshot.
    browserNowMs += 10_000;
    rig.latest.serverDrop(1006);
    vi.advanceTimersByTime(500);
    browserNowMs += 500;
    rig.latest.serverOpen();
    expect(store.synced).toBe(false);
    expect(store.lastSyncedRevision).toBe(1);

    rig.latest.serverMessage(
      putFrame(initialSnapshot.id, 2, stationaryHeartbeatPayload),
    );
    rig.latest.serverMessage(syncedFrame(2));

    const vReplayed = store.getCommandableVehicle(initialSnapshot.id)!;
    expect(store.synced).toBe(true);
    expect(store.lastSyncedRevision).toBe(2);
    // Reconnect snapshot replay MUST preserve liveHbReceiptMs rather than resetting to browserNowMs!
    expect(vReplayed.stateIngestedAtMs).toBe(liveHbReceiptMs);
    expect(vReplayed.stateObservedAt).toBe(liveHbIso);

    // 4. Fresh post-reconnect observation advances sub-millisecond nanosecond timestamp and refreshes stateIngestedAtMs.
    browserNowMs += 1_000;
    const recoveredReceiptMs = browserNowMs;
    const recoveredIso = "2026-11-25T12:00:12.500000900Z";
    const recoveredPayload = buildVehicleStoredPayload(
      1,
      {
        "uxs.domain.kind": "quadcopter",
        "uxs.authority.state": "companion-teleop",
        "uxs.health.armed": true,
      },
      {
        observedAt: recoveredIso,
        updatedAt: recoveredIso,
      },
    );
    rig.latest.serverMessage(putFrame(initialSnapshot.id, 3, recoveredPayload));

    const vRecovered = store.getCommandableVehicle(initialSnapshot.id)!;
    expect(vRecovered.stateObservedAt).toBe(recoveredIso);
    expect(vRecovered.stateIngestedAtMs).toBe(recoveredReceiptMs);
    expect(vRecovered.stateIngestedLive).toBe(true);
    expect(vRecovered.stateReplayedUnchanged).toBe(false);

    store.disconnect();
  });

  // spec: cop-ui / Fleet health readiness, per-vehicle observation freshness, and last-known fault retention
  it("excludes zero-confidence placeholders and pose-only updates from stateObservedAt, flags fast-reconnect unchanged snapshots, and preserves poisonedAtMs", () => {
    const rig = new MockWebSocketRig();
    let browserNowMs = Date.parse("2026-11-25T13:00:00.000Z");
    const store = createSkgStore({
      url: "ws://127.0.0.1:8082/ws",
      createSocket: rig.createSocket,
      now: () => browserNowMs,
    });

    store.connect();
    rig.latest.serverOpen();

    // 1. All-zero-confidence triples decode stateObservedAt = null and stateIngestedAtMs = null.
    const currentIso = new Date(browserNowMs).toISOString();
    const baseZero = buildVehicleStoredPayload(
      1,
      { "uxs.domain.kind": "quadcopter" },
      { observedAt: currentIso, updatedAt: currentIso },
    );
    const zeroConfidencePayload = {
      ...baseZero,
      triples: baseZero.triples.map((t) => ({ ...t, confidence: 0 })),
    };
    const poisonedKey = "boxbotix.local.robotics.uxs.vehicle.2";
    rig.latest.serverMessage(putFrame(baseZero.id, 1, zeroConfidencePayload));
    rig.latest.serverMessage(
      JSON.stringify({
        type: "poisoned",
        bucket: "ENTITY_STATES",
        key: poisonedKey,
        revision: 2,
        error: "corrupt msgpack triple",
      }),
    );
    rig.latest.serverMessage(syncedFrame(2));

    const vZero = store.getCommandableVehicle(baseZero.id)!;
    expect(vZero.stateObservedAt).toBeNull();
    expect(vZero.stateIngestedAtMs).toBeNull();
    expect(vZero.stateIngestedLive).toBe(false);
    expect(store.getEntity(poisonedKey)?.poisonedAtMs).toBe(browserNowMs);
    const initialPoisonedAtMs = browserNowMs;

    // 2. Measured health update advances stateObservedAt and sets stateIngestedLive = true.
    browserNowMs += 1_000;
    const measuredHealthMs = browserNowMs;
    const measuredHealthIso = new Date(measuredHealthMs).toISOString();
    const measuredPayload = buildVehicleStoredPayload(
      1,
      { "uxs.domain.kind": "quadcopter", "uxs.health.armed": true },
      { observedAt: measuredHealthIso, updatedAt: measuredHealthIso },
    );
    rig.latest.serverMessage(putFrame(baseZero.id, 3, measuredPayload));
    const vMeasured = store.getCommandableVehicle(baseZero.id)!;
    expect(vMeasured.stateObservedAt).toBe(measuredHealthIso);
    expect(vMeasured.stateIngestedAtMs).toBe(measuredHealthMs);
    expect(vMeasured.stateIngestedLive).toBe(true);
    expect(vMeasured.stateReplayedUnchanged).toBe(false);

    // 3. Pose-only SKG update advances poseObservedAt without advancing stateObservedAt or stateIngestedAtMs.
    browserNowMs += 1_500;
    const poseOnlyIso = new Date(browserNowMs).toISOString();
    const poseOnlyPayload = {
      ...measuredPayload,
      updated_at: poseOnlyIso,
      triples: measuredPayload.triples.map((t) =>
        t.predicate === "uxs.kinematics.ned" ||
        t.predicate === "uxs.kinematics.vx"
          ? {
              ...t,
              object:
                t.predicate === "uxs.kinematics.ned" ? [25, -4, -25] : 4.5,
              timestamp: poseOnlyIso,
            }
          : t,
      ),
    };
    rig.latest.serverMessage(putFrame(baseZero.id, 4, poseOnlyPayload));
    const vPoseOnly = store.getCommandableVehicle(baseZero.id)!;
    expect(vPoseOnly.poseObservedAt).toBe(poseOnlyIso);
    expect(vPoseOnly.poseIngestedAtMs).toBe(browserNowMs);
    expect(vPoseOnly.stateObservedAt).toBe(measuredHealthIso);
    expect(vPoseOnly.stateIngestedAtMs).toBe(measuredHealthMs);

    // 4. Fast reconnect (+100 ms drop, +500 ms reconnect — inside 4_000 ms threshold)
    // replaying identical snapshot sets stateReplayedUnchanged = true and preserves poisonedAtMs.
    browserNowMs = measuredHealthMs + 100;
    rig.latest.serverDrop(1006);
    browserNowMs = measuredHealthMs + 600;
    store.connect();
    rig.latest.serverOpen();
    rig.latest.serverMessage(putFrame(baseZero.id, 4, poseOnlyPayload));
    rig.latest.serverMessage(
      JSON.stringify({
        type: "poisoned",
        bucket: "ENTITY_STATES",
        key: poisonedKey,
        revision: 2,
        error: "corrupt msgpack triple",
      }),
    );
    rig.latest.serverMessage(syncedFrame(4));

    const vFastReplay = store.getCommandableVehicle(baseZero.id)!;
    expect(vFastReplay.stateIngestedAtMs).toBe(measuredHealthMs);
    expect(vFastReplay.stateIngestedLive).toBe(false);
    expect(vFastReplay.stateReplayedUnchanged).toBe(true);
    expect(store.getEntity(poisonedKey)?.poisonedAtMs).toBe(
      initialPoisonedAtMs,
    );

    // 5. Advancing health observation clears stateReplayedUnchanged = false.
    browserNowMs += 200;
    const nextHealthIso = new Date(browserNowMs).toISOString();
    const advancingHealthPayload = buildVehicleStoredPayload(
      1,
      { "uxs.domain.kind": "quadcopter", "uxs.health.armed": false },
      { observedAt: nextHealthIso, updatedAt: nextHealthIso },
    );
    rig.latest.serverMessage(putFrame(baseZero.id, 5, advancingHealthPayload));
    const vAdvanced = store.getCommandableVehicle(baseZero.id)!;
    expect(vAdvanced.stateObservedAt).toBe(nextHealthIso);
    expect(vAdvanced.stateIngestedAtMs).toBe(browserNowMs);
    expect(vAdvanced.stateIngestedLive).toBe(true);
    expect(vAdvanced.stateReplayedUnchanged).toBe(false);

    store.disconnect();
  });

  // spec: cop-ui / Fleet health readiness, per-vehicle observation freshness, and last-known fault retention
  it("requires all #0 HEARTBEAT safety predicates to be measured, selects the oldest safety timestamp, and ignores #1 SYS_STATUS timing-only updates", () => {
    const rig = new MockWebSocketRig();
    let browserNowMs = Date.parse("2026-11-25T14:00:00.000Z");
    const store = createSkgStore({
      url: "ws://127.0.0.1:8082/ws",
      createSocket: rig.createSocket,
      now: () => browserNowMs,
    });

    store.connect();
    rig.latest.serverOpen();

    const t0Iso = new Date(browserNowMs).toISOString();
    const base = buildVehicleStoredPayload(
      1,
      { "uxs.domain.kind": "quadcopter" },
      { observedAt: t0Iso, updatedAt: t0Iso },
    );

    // 1. Only uxs.health.rttus has confidence=1 while safety predicates have confidence=0 -> unmeasured!
    const timingOnlyMeasured = {
      ...base,
      triples: base.triples.map((t) => ({
        ...t,
        confidence: t.predicate === "uxs.health.rttus" ? 1 : 0,
      })),
    };
    rig.latest.serverMessage(putFrame(base.id, 1, timingOnlyMeasured));
    rig.latest.serverMessage(syncedFrame(1));

    const vTimingOnly = store.getCommandableVehicle(base.id)!;
    expect(vTimingOnly.hasMeasuredSafetyState).toBe(false);
    expect(vTimingOnly.stateObservedAt).toBeNull();
    expect(vTimingOnly.stateIngestedAtMs).toBeNull();
    expect(vTimingOnly.stateIngestedLive).toBe(false);

    // 2. Five of six safety predicates have confidence=1, but uxs.health.flags has confidence=0 -> still unmeasured!
    browserNowMs += 500;
    const partialSafety = {
      ...base,
      triples: base.triples.map((t) => ({
        ...t,
        confidence: t.predicate === "uxs.health.flags" ? 0 : 1,
      })),
    };
    rig.latest.serverMessage(putFrame(base.id, 2, partialSafety));
    const vPartial = store.getCommandableVehicle(base.id)!;
    expect(vPartial.hasMeasuredSafetyState).toBe(false);
    expect(vPartial.stateObservedAt).toBeNull();
    expect(vPartial.stateIngestedAtMs).toBeNull();

    // 3. All 6 safety predicates measured with skewed timestamps -> stateObservedAt equals the OLDEST safety timestamp.
    browserNowMs += 500;
    const tHbReceiptMs = browserNowMs;
    const olderFlagsIso = "2026-11-25T14:00:00.250Z";
    const newerAuthIso = "2026-11-25T14:00:01.000Z";
    const skewedMeasured = {
      ...base,
      updated_at: newerAuthIso,
      triples: base.triples.map((t) => ({
        ...t,
        confidence: 1,
        timestamp:
          t.predicate === "uxs.health.flags" ? olderFlagsIso : newerAuthIso,
      })),
    };
    rig.latest.serverMessage(putFrame(base.id, 3, skewedMeasured));
    const vSkewed = store.getCommandableVehicle(base.id)!;
    expect(vSkewed.hasMeasuredSafetyState).toBe(true);
    expect(vSkewed.stateObservedAt).toBe(olderFlagsIso);
    expect(vSkewed.stateProducerAgeMs).toBe(750);
    expect(vSkewed.stateIngestedAtMs).toBe(tHbReceiptMs - 750);

    // 4. #1 SYS_STATUS-only update (rttUs/loopDtUs/failsafes) advances timing timestamps without advancing stateObservedAt or stateIngestedAtMs!
    browserNowMs += 2_000;
    const sysStatusOnlyIso = new Date(browserNowMs).toISOString();
    const sysStatusOnly = {
      ...skewedMeasured,
      updated_at: sysStatusOnlyIso,
      triples: skewedMeasured.triples.map((t) =>
        t.predicate === "uxs.health.rttus" ||
        t.predicate === "uxs.health.loopdtus" ||
        t.predicate === "uxs.health.failsafes"
          ? {
              ...t,
              object: t.predicate === "uxs.health.rttus" ? 275 : 1400,
              timestamp: sysStatusOnlyIso,
            }
          : t,
      ),
    };
    rig.latest.serverMessage(putFrame(base.id, 4, sysStatusOnly));
    const vAfterSysStatus = store.getCommandableVehicle(base.id)!;
    expect(vAfterSysStatus.sysStatus.rttUs).toBe(275);
    expect(vAfterSysStatus.stateObservedAt).toBe(olderFlagsIso);
    expect(vAfterSysStatus.stateIngestedAtMs).toBe(tHbReceiptMs - 750);

    store.disconnect();
  });

  // spec: cop-ui / Fleet health readiness, per-vehicle observation freshness, and last-known fault retention
  it("projects same-producer safety lag on first post-sync sight and retains unadvanced safety predicate receipts across partial updates", () => {
    const rig = new MockWebSocketRig();
    const producerT0Ms = Date.parse("2026-11-25T15:00:00.000Z");
    // Browser clock skewed +1 hour ahead of producer clock:
    let browserNowMs = producerT0Ms + 3_600_000;
    const store = createSkgStore({
      url: "ws://127.0.0.1:8082/ws",
      createSocket: rig.createSocket,
      now: () => browserNowMs,
    });

    store.connect();
    rig.latest.serverOpen();
    rig.latest.serverMessage(syncedFrame(0));

    // 1. First sight after sync: producer snapshot at T+6s with safety triples at T and NED at T+6s.
    browserNowMs += 6_000;
    const hbT0Iso = new Date(producerT0Ms).toISOString();
    const nedT6Iso = new Date(producerT0Ms + 6_000).toISOString();
    const base = buildVehicleStoredPayload(
      1,
      { "uxs.domain.kind": "quadcopter", "uxs.health.armed": true },
      { observedAt: hbT0Iso, updatedAt: nedT6Iso },
    );
    const firstSightDelayedSafety = {
      ...base,
      triples: base.triples.map((t) =>
        t.predicate === "uxs.kinematics.ned" ||
        t.predicate === "uxs.kinematics.vx"
          ? {
              ...t,
              object:
                t.predicate === "uxs.kinematics.ned" ? [12, -2, -15] : 3.0,
              timestamp: nedT6Iso,
            }
          : t,
      ),
    };
    rig.latest.serverMessage(putFrame(base.id, 1, firstSightDelayedSafety));

    const vFirstSight = store.getCommandableVehicle(base.id)!;
    expect(vFirstSight.stateObservedAt).toBe(hbT0Iso);
    expect(vFirstSight.stateProducerAgeMs).toBe(6_000);
    expect(vFirstSight.stateIngestedLive).toBe(true);
    expect(vFirstSight.stateIngestedAtMs).toBe(browserNowMs - 6_000);

    // 2. Complete fresh #0 HEARTBEAT at T+7s clears lag and anchors all 6 safety predicates at receiptMs.
    browserNowMs += 1_000;
    const t7ReceiptMs = browserNowMs;
    const hbT7Iso = new Date(producerT0Ms + 7_000).toISOString();
    const completeT7 = buildVehicleStoredPayload(
      1,
      { "uxs.domain.kind": "quadcopter", "uxs.health.armed": true },
      { observedAt: hbT7Iso, updatedAt: hbT7Iso },
    );
    rig.latest.serverMessage(putFrame(base.id, 2, completeT7));

    const vCompleteT7 = store.getCommandableVehicle(base.id)!;
    expect(vCompleteT7.stateObservedAt).toBe(hbT7Iso);
    expect(vCompleteT7.stateProducerAgeMs).toBe(0);
    expect(vCompleteT7.stateIngestedAtMs).toBe(t7ReceiptMs);
    expect(vCompleteT7.stateIngestedLive).toBe(true);

    // 3. Partial safety update 6s later (T+13s): 5 safety predicates advance to T+13s (changing armed=false),
    // while measured uxs.health.flags stays at T+7s -> receipt remains anchored at t7ReceiptMs!
    browserNowMs += 6_000;
    const hbT13Iso = new Date(producerT0Ms + 13_000).toISOString();
    const partialSafetyT13 = {
      ...completeT7,
      updated_at: hbT13Iso,
      triples: completeT7.triples.map((t) => {
        if (t.predicate === "uxs.health.flags") {
          return t; // stays at hbT7Iso
        }
        if (t.predicate === "uxs.health.armed") {
          return { ...t, object: false, timestamp: hbT13Iso };
        }
        if (
          t.predicate === "uxs.domain.kind" ||
          t.predicate === "uxs.authority.state" ||
          t.predicate === "uxs.mode.companion" ||
          t.predicate === "uxs.link.tier"
        ) {
          return { ...t, timestamp: hbT13Iso };
        }
        return t;
      }),
    };
    rig.latest.serverMessage(putFrame(base.id, 3, partialSafetyT13));

    const vPartialT13 = store.getCommandableVehicle(base.id)!;
    expect(vPartialT13.armed).toBe(false);
    expect(vPartialT13.stateObservedAt).toBe(hbT7Iso);
    expect(vPartialT13.stateProducerAgeMs).toBe(6_000);
    expect(vPartialT13.stateIngestedAtMs).toBe(t7ReceiptMs);

    // 4. Complete #0 HEARTBEAT at T+14s updates all 6 safety predicates including uxs.health.flags -> refreshes receipt!
    browserNowMs += 1_000;
    const t14ReceiptMs = browserNowMs;
    const hbT14Iso = new Date(producerT0Ms + 14_000).toISOString();
    const completeT14 = buildVehicleStoredPayload(
      1,
      { "uxs.domain.kind": "quadcopter", "uxs.health.armed": false },
      { observedAt: hbT14Iso, updatedAt: hbT14Iso },
    );
    rig.latest.serverMessage(putFrame(base.id, 4, completeT14));

    const vCompleteT14 = store.getCommandableVehicle(base.id)!;
    expect(vCompleteT14.stateObservedAt).toBe(hbT14Iso);
    expect(vCompleteT14.stateProducerAgeMs).toBe(0);
    expect(vCompleteT14.stateIngestedAtMs).toBe(t14ReceiptMs);
    expect(vCompleteT14.stateIngestedLive).toBe(true);

    store.disconnect();
  });
});
