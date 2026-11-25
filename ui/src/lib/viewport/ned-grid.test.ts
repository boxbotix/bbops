// SPDX-License-Identifier: MIT
// Copyright (c) 2026 boxbotix

import { flushSync, mount, unmount } from "svelte";
import { describe, expect, it, vi } from "vitest";
import TacticalCanvas from "../components/TacticalCanvas.svelte";
import {
  type ExternalContactEntityState,
  createSkgStore,
} from "../stores/skg.svelte";
import { createTelemetryStore } from "../stores/telemetry.svelte";
import { createViewportStore } from "../stores/viewport.svelte";
import { handleViewportHotkey, isEditableHotkeyTarget } from "./hotkeys";
import {
  MAX_ZOOM_PX_PER_M,
  MIN_ZOOM_PX_PER_M,
  NedGridAdapter,
  USV_CRAB_SPEED_DEADBAND_MPS,
  WakeTrailBook,
  anchorSharedFrameOrigin,
  computeCrabAngleVectors,
  deriveNedOriginLla,
  llaToLocalNed,
  localNedToLla,
  resetSharedFrameOrigin,
  resolveContactPalette,
  resolveDomainGlyphKind,
  resolveRangeRingLabelPosition,
  resolveVehicleSafetyTone,
  wrapAngleSignedRad,
} from "./ned-grid";

function createXorShift32(seed: number): () => number {
  let state = seed >>> 0 || 1;
  return () => {
    state ^= state << 13;
    state ^= state >>> 17;
    state ^= state << 5;
    return (state >>> 0) / 0x1_0000_0000;
  };
}

describe("NedGridAdapter, tactical kinematics, and viewport hotkeys", () => {
  // spec: cop-ui / Renderer-agnostic ViewportAdapter, surface-vs-safety-HUD layer split, and multi-domain tactical canvas
  it("round-trips project/unproject and geometric screenCourseRad across seeded PRNG pan/zoom/rotation and WGS-84 tangent origins", () => {
    // Generated check (240 PRNG cases + explicit boundaries) verified against an
    // independent analytical 2D rotation + finite-difference forward-bearing oracle.
    const rng = createXorShift32(0xb1_0220);
    const explicitBearings = [0, Math.PI / 2, -Math.PI / 2, Math.PI, -Math.PI];
    let boundaryHits = 0;
    let assertionsActivated = 0;

    for (let i = 0; i < 240; i += 1) {
      const widthPx = 400 + Math.floor(rng() * 1200);
      const heightPx = 300 + Math.floor(rng() * 900);
      const zoomPxPerM =
        i === 0
          ? MIN_ZOOM_PX_PER_M
          : i === 1
            ? MAX_ZOOM_PX_PER_M
            : MIN_ZOOM_PX_PER_M + rng() * 35;
      if (
        zoomPxPerM === MIN_ZOOM_PX_PER_M ||
        zoomPxPerM === MAX_ZOOM_PX_PER_M
      ) {
        boundaryHits += 1;
      }
      const bearingRad =
        i < explicitBearings.length
          ? explicitBearings[i]!
          : (rng() * 4 - 2) * Math.PI;
      if (Math.abs(Math.abs(bearingRad) - Math.PI) < 1e-9 || bearingRad === 0) {
        boundaryHits += 1;
      }
      const centerN = (rng() - 0.5) * 2000;
      const centerE = (rng() - 0.5) * 2000;
      const originLat = (rng() - 0.5) * 120;
      const originLon = (rng() - 0.5) * 320;
      const originAlt = rng() * 500;

      const adapter = new NedGridAdapter({
        widthPx,
        heightPx,
        zoomPxPerM,
        centerNed: [centerN, centerE],
        bearingRad,
        originLla: [originLat, originLon, originAlt],
      });

      const targetN = centerN + (rng() - 0.5) * 400;
      const targetE = centerE + (rng() - 0.5) * 400;
      const proj = adapter.project({ ned: [targetN, targetE, -25] });
      expect(proj.rangeM).toBeCloseTo(
        Math.hypot(targetN - centerN, targetE - centerE),
        9,
      );

      const unproj = adapter.unproject(proj.x, proj.y);
      expect(unproj.ned[0]).toBeCloseTo(targetN, 7);
      expect(unproj.ned[1]).toBeCloseTo(targetE, 7);
      expect(unproj.ned[2]).toBe(0);
      expect(unproj.lla).not.toBeNull();

      // Project unproj.lla back via lla-only WorldPoint and confirm round-trip.
      const reProjFromLla = adapter.project({ lla: unproj.lla! });
      expect(reProjFromLla.x).toBeCloseTo(proj.x, 5);
      expect(reProjFromLla.y).toBeCloseTo(proj.y, 5);

      // Independent geometric oracle for screenCourseRad: step 10m along headingRad
      // in world NED and measure the screen-space vector angle from -Y toward +X.
      const headingRad = (rng() * 2 - 1) * Math.PI;
      const aheadProj = adapter.project({
        ned: [
          targetN + Math.cos(headingRad) * 10,
          targetE + Math.sin(headingRad) * 10,
          0,
        ],
      });
      const geometricScreenBearing = wrapAngleSignedRad(
        Math.atan2(aheadProj.x - proj.x, proj.y - aheadProj.y),
      );
      expect(adapter.screenCourseRad(headingRad)).toBeCloseTo(
        geometricScreenBearing,
        9,
      );
      assertionsActivated += 1;
    }

    expect(boundaryHits).toBeGreaterThanOrEqual(4);
    expect(assertionsActivated).toBe(240);

    // Unanchored adapter must mark lla-only contacts invisible rather than plotting at [0,0].
    const unanchored = new NedGridAdapter({ originLla: null });
    const unanchoredContact = unanchored.project({
      lla: [30.2672, -97.7431, 150],
    });
    expect(unanchoredContact.visible).toBe(false);
    expect(unanchored.flyTo({ lla: [30.2672, -97.7431, 150] })).toBe(false);

    // Direct tangent-plane helper round-trip check.
    const origin: [number, number, number] = [30.2672, -97.7431, 150];
    const ned = llaToLocalNed([30.2682, -97.7421, 175], origin);
    expect(ned).not.toBeNull();
    expect(ned![0]).toBeGreaterThan(100);
    expect(ned![1]).toBeGreaterThan(80);
    expect(ned![2]).toBeCloseTo(-25, 9);
    const roundTripLla = localNedToLla(ned!, origin);
    expect(roundTripLla![0]).toBeCloseTo(30.2682, 9);
    expect(roundTripLla![1]).toBeCloseTo(-97.7421, 9);
    expect(roundTripLla![2]).toBeCloseTo(175, 9);
  });

  // spec: cop-ui / Renderer-agnostic ViewportAdapter, surface-vs-safety-HUD layer split, and multi-domain tactical canvas
  it("computes SurfaceBoat crab-angle split above 0.15 m/s deadband and suppresses stationary course jitter", () => {
    const bowEastRad = Math.PI / 2;

    // Below deadband (0.14 m/s): suppress split even with orthogonal velocity noise.
    const below = computeCrabAngleVectors(bowEastRad, 0.14, 0);
    expect(below.hasCourseSplit).toBe(false);
    expect(below.crabAngleRad).toBe(0);
    expect(below.courseOverGroundRad).toBeCloseTo(bowEastRad, 9);

    // Exactly at deadband (0.15 m/s due North while bow points East -> -90 deg crab).
    const atDeadband = computeCrabAngleVectors(
      bowEastRad,
      USV_CRAB_SPEED_DEADBAND_MPS,
      0,
    );
    expect(atDeadband.hasCourseSplit).toBe(true);
    expect(atDeadband.courseOverGroundRad).toBeCloseTo(0, 9);
    expect(atDeadband.crabAngleRad).toBeCloseTo(-Math.PI / 2, 9);

    // Branch-cut wrap across +/-180 deg: bow +170 deg, course -170 deg -> +20 deg crab.
    const bow170 = (170 * Math.PI) / 180;
    const cogMinus170 = (-170 * Math.PI) / 180;
    const acrossCut = computeCrabAngleVectors(
      bow170,
      Math.cos(cogMinus170) * 3,
      Math.sin(cogMinus170) * 3,
    );
    expect(acrossCut.hasCourseSplit).toBe(true);
    expect(acrossCut.crabAngleRad).toBeCloseTo((20 * Math.PI) / 180, 9);
  });

  // spec: cop-ui / Renderer-agnostic ViewportAdapter, surface-vs-safety-HUD layer split, and multi-domain tactical canvas
  it("maps domain glyphs, reserved safety tones, WakeTrailBook history, and stacked surface/HUD canvas layers", () => {
    expect(resolveDomainGlyphKind("quadcopter")).toBe("quadcopter");
    expect(resolveDomainGlyphKind("rover")).toBe("rover");
    expect(resolveDomainGlyphKind("surface-boat")).toBe("surface-boat");

    const healthyFlags = {
      clock_sync_locked: true,
      teleop_deadman_clamped: false,
      crsf_link_healthy: true,
      companion_trunk_healthy: true,
    };
    const healthySys = {
      failsafes: 0,
      crcErrors: 0,
      loopDtUs: 1000,
      maxLoopDtUs: 1200,
      rttUs: 400,
    };

    expect(
      resolveVehicleSafetyTone({
        poisoned: false,
        authority: "companion-autonomous",
        armed: false,
        healthFlags: healthyFlags,
        sysStatus: healthySys,
      }).tone,
    ).toBe("nominal");

    expect(
      resolveVehicleSafetyTone({
        poisoned: false,
        authority: "companion-teleop",
        armed: true,
        healthFlags: healthyFlags,
        sysStatus: healthySys,
      }).tone,
    ).toBe("success");

    expect(
      resolveVehicleSafetyTone({
        poisoned: false,
        authority: "direct-crsf-override",
        armed: true,
        healthFlags: healthyFlags,
        sysStatus: healthySys,
      }).tone,
    ).toBe("warning");

    expect(
      resolveVehicleSafetyTone({
        poisoned: false,
        authority: "mcu-failsafe",
        armed: true,
        healthFlags: healthyFlags,
        sysStatus: healthySys,
      }).tone,
    ).toBe("error");

    expect(resolveContactPalette("live").tone).toBe("contact-live");
    expect(resolveContactPalette("unknown").tone).toBe("contact-muted");

    // Order-dependent WakeTrailBook history vs. reference model.
    const book = new WakeTrailBook(3, 1.0);
    const id = "boxbotix.local.robotics.uxs.vehicle.1";
    book.record(id, [0, 0, 0]);
    book.record(id, [0.4, 0.2, 0]); // < 1.0m step: ignored
    book.record(id, [2, 0, 0]);
    book.record(id, [4, 0, 0]);
    book.record(id, [6, 0, 0]); // evicts [0,0,0]
    expect(book.getTrail(id).map((s) => s.northM)).toEqual([2, 4, 6]);
    book.pruneExcept(new Set());
    expect(book.getTrail(id)).toEqual([]);

    // Mount TacticalCanvas in jsdom to verify .viewport-surface is layered beneath safety-hud-overlay.
    const host = document.createElement("div");
    document.body.appendChild(host);
    const telemetryStore = createTelemetryStore();
    const skgStore = createSkgStore();
    const viewportStore = createViewportStore({ skgStore });
    const app = mount(TacticalCanvas, {
      target: host,
      props: { telemetryStore, skgStore, viewportStore },
    });
    flushSync();

    const viewportEl = host.querySelector('[data-testid="tactical-viewport"]');
    const surfaceEl = host.querySelector('[data-layer="viewport-surface"]');
    const overlayEl = host.querySelector('[data-layer="safety-hud-overlay"]');
    expect(viewportEl).not.toBeNull();
    expect(surfaceEl).not.toBeNull();
    expect(overlayEl).not.toBeNull();
    // Surface precedes upper safety-HUD canvas in DOM paint order and carries distinct layer attributes.
    expect(
      Boolean(
        surfaceEl!.compareDocumentPosition(overlayEl!) &
        Node.DOCUMENT_POSITION_FOLLOWING,
      ),
    ).toBe(true);

    unmount(app);
    host.remove();
  });

  // spec: cop-ui / Renderer-agnostic ViewportAdapter, surface-vs-safety-HUD layer split, and multi-domain tactical canvas
  it("drives T, C, H, and Escape viewport hotkeys while ignoring editable targets and modifier chords", () => {
    const vehicleId = "boxbotix.local.robotics.uxs.vehicle.7";
    const viewport = createViewportStore();
    viewport.selectEntity(vehicleId);
    const syncedHashes: string[] = [];
    const recordHash = (hash: string) => syncedHashes.push(hash);

    // Editable targets must never trigger hotkeys.
    expect(isEditableHotkeyTarget({ tagName: "INPUT" })).toBe(true);
    expect(isEditableHotkeyTarget({ tagName: "textarea" })).toBe(true);
    expect(isEditableHotkeyTarget({ tagName: "SELECT" })).toBe(true);
    expect(isEditableHotkeyTarget({ isContentEditable: true })).toBe(true);
    expect(
      isEditableHotkeyTarget({
        tagName: "DIV",
        getAttribute: (name) => (name === "contenteditable" ? "true" : null),
      }),
    ).toBe(true);
    expect(isEditableHotkeyTarget({ tagName: "CANVAS" })).toBe(false);

    for (const ignoredEvent of [
      { key: "t", target: { tagName: "INPUT" } },
      { key: "h", target: { tagName: "TEXTAREA" } },
      { key: "c", target: { isContentEditable: true } },
      { key: "t", ctrlKey: true, target: { tagName: "DIV" } },
      { key: "h", metaKey: true, target: { tagName: "DIV" } },
      { key: "c", altKey: true, target: { tagName: "DIV" } },
      { key: "h", shiftKey: true, target: { tagName: "DIV" } },
      { key: "t", repeat: true, target: { tagName: "DIV" } },
    ]) {
      expect(handleViewportHotkey(ignoredEvent, viewport, recordHash)).toBe(
        false,
      );
    }
    expect(viewport.tetheredEntityId).toBeNull();
    expect(viewport.chaseLock).toBe(false);
    expect(viewport.declutter).toBe("none");
    expect(syncedHashes).toHaveLength(0);

    // T tethers selected entity; second T untethers.
    const preventDefault = vi.fn();
    expect(
      handleViewportHotkey(
        { key: "t", target: { tagName: "DIV" }, preventDefault },
        viewport,
        recordHash,
      ),
    ).toBe(true);
    expect(preventDefault).toHaveBeenCalledTimes(1);
    expect(viewport.tetheredEntityId).toBe(vehicleId);
    expect(syncedHashes.at(-1)).toContain("tether=");

    // C enables chase lock; Escape clears both chaseLock and tether.
    expect(
      handleViewportHotkey(
        { key: "c", target: { tagName: "DIV" } },
        viewport,
        recordHash,
      ),
    ).toBe(true);
    expect(viewport.chaseLock).toBe(true);
    expect(syncedHashes.at(-1)).toContain("chase=1");

    expect(
      handleViewportHotkey(
        { key: "Escape", target: { tagName: "DIV" } },
        viewport,
        recordHash,
      ),
    ).toBe(true);
    expect(viewport.tetheredEntityId).toBeNull();
    expect(viewport.chaseLock).toBe(false);

    // H cycles declutter none -> minimal -> tactical -> none.
    handleViewportHotkey({ key: "h" }, viewport, recordHash);
    expect(viewport.declutter).toBe("minimal");
    handleViewportHotkey({ key: "h" }, viewport, recordHash);
    expect(viewport.declutter).toBe("tactical");
    handleViewportHotkey({ key: "h" }, viewport, recordHash);
    expect(viewport.declutter).toBe("none");
  });

  // spec: cop-ui / Renderer-agnostic ViewportAdapter, surface-vs-safety-HUD layer split, and multi-domain tactical canvas
  it("derives NED=[0,0,0] WGS-84 origin from nonzero initial vehicle offsets and withholds external contacts until a verified (ned, lla) vehicle anchors", () => {
    const trueOriginLla: [number, number, number] = [30.2672, -97.7431, 150];
    const vehicleOffsetNed: [number, number, number] = [100, -45, -20];
    const vehicleLla = localNedToLla(vehicleOffsetNed, trueOriginLla)!;
    expect(vehicleLla).not.toBeNull();

    // Direct inverse check: deriveNedOriginLla recovers NED=[0,0,0] WGS-84 origin, not vehicleLla.
    const solvedOrigin = deriveNedOriginLla(vehicleOffsetNed, vehicleLla)!;
    expect(solvedOrigin).not.toBeNull();
    expect(solvedOrigin[0]).toBeCloseTo(trueOriginLla[0], 9);
    expect(solvedOrigin[1]).toBeCloseTo(trueOriginLla[1], 9);
    expect(solvedOrigin[2]).toBeCloseTo(trueOriginLla[2], 6);

    const coLocatedContactNed = llaToLocalNed(vehicleLla, solvedOrigin)!;
    expect(coLocatedContactNed[0]).toBeCloseTo(vehicleOffsetNed[0], 6);
    expect(coLocatedContactNed[1]).toBeCloseTo(vehicleOffsetNed[1], 6);
    expect(coLocatedContactNed[2]).toBeCloseTo(vehicleOffsetNed[2], 6);

    // Unverified candidate must not anchor adapter origin.
    const helperAdapter = new NedGridAdapter({ widthPx: 800, heightPx: 600 });
    expect(
      anchorSharedFrameOrigin(helperAdapter, [
        {
          ned: [0, 0, -20],
          lla: vehicleLla,
          hasVerifiedFrameReference: false,
        },
      ]),
    ).toBe(false);
    expect(helperAdapter.getCamera().originLla).toBeNull();

    const nowMs = Date.parse("2026-11-24T12:10:00.000Z");
    const vehicleId = "boxbotix.local.robotics.uxs.vehicle.1";
    const contactId = "boxbotix.local.tak.cot.contact.colocated";

    const makeVehicleEntity = (
      ned: [number, number, number],
      lla: [number, number, number],
    ) => ({
      id: vehicleId,
      kind: "uxs-vehicle" as const,
      title: "Quad-1",
      org: "boxbotix",
      platform: "local",
      sysid: 1,
      domain: "quadcopter" as const,
      authority: "companion-teleop" as const,
      companionMode: "teleop-velocity" as const,
      linkTier: "mesh-high-rate" as const,
      armed: true,
      healthFlags: {
        clock_sync_locked: true,
        teleop_deadman_clamped: false,
        crsf_link_healthy: true,
        companion_trunk_healthy: true,
      },
      sysStatus: {
        failsafes: 0,
        crcErrors: 0,
        loopDtUs: 1000,
        maxLoopDtUs: 1200,
        rttUs: 400,
      },
      attitudeQuaternion: [1, 0, 0, 0] as [number, number, number, number],
      attitudeRates: [0, 0, 0] as [number, number, number],
      ned,
      velocityNed: [0, 0, 0] as [number, number, number],
      headingDeg: 0,
      location: { latitude: lla[0], longitude: lla[1], altitude: lla[2] },
      kvRevision: 1,
      updatedAt: new Date(nowMs).toISOString(),
      poisoned: false,
      poisonError: null,
      indexingProfile: "full",
      source: "mavlink-ingest",
      observedAt: new Date(nowMs).toISOString(),
      receivedAt: new Date(nowMs).toISOString(),
      expiresAt: null,
      uncertaintyMeters: null,
      feedStatus: "live" as const,
      ingestedAtMs: nowMs,
      triples: [],
    });

    const coLocatedContact = {
      id: contactId,
      kind: "external-contact" as const,
      title: "CoLocated-Contact",
      contactType: "a-f-G-U-C",
      location: {
        latitude: vehicleLla[0],
        longitude: vehicleLla[1],
        altitude: vehicleLla[2],
      },
      kvRevision: 1,
      updatedAt: null,
      poisoned: false,
      poisonError: null,
      indexingProfile: null,
      source: "tak-bridge",
      observedAt: null,
      receivedAt: null,
      expiresAt: null,
      uncertaintyMeters: 5,
      feedStatus: "live" as const,
      ingestedAtMs: nowMs,
      triples: [],
    };

    // Arrival Order 1: Vehicle with nonzero initial NED offset arrives first, co-located contact second.
    {
      const host = document.createElement("div");
      document.body.appendChild(host);
      const telemetryStore = createTelemetryStore({ now: () => nowMs });
      let ws2Socket: {
        readyState: number;
        onopen: ((event: never) => unknown) | null;
        onmessage: ((event: MessageEvent<unknown>) => unknown) | null;
        onerror: ((event: never) => unknown) | null;
        onclose: ((event: CloseEvent) => unknown) | null;
        send(data: string): void;
        close(code?: number, reason?: string): void;
      } | null = null;
      const skgStore = createSkgStore({
        now: () => nowMs,
        createSocket: () => {
          ws2Socket = {
            readyState: 1,
            onopen: null,
            onmessage: null,
            onerror: null,
            onclose: null,
            send: () => undefined,
            close: () => undefined,
          };
          return ws2Socket;
        },
      });
      skgStore.connect("ws://127.0.0.1:8080/ws/graph");
      ws2Socket!.onopen?.(new Event("open") as never);
      const viewportStore = createViewportStore({ skgStore });
      const adapter = new NedGridAdapter({
        widthPx: 640,
        heightPx: 420,
        zoomPxPerM: 1.5,
      });

      skgStore.entities.set(
        vehicleId,
        makeVehicleEntity(vehicleOffsetNed, vehicleLla),
      );
      const app = mount(TacticalCanvas, {
        target: host,
        props: {
          telemetryStore,
          skgStore,
          viewportStore,
          adapter,
          now: () => nowMs,
        },
      });
      flushSync();

      expect(
        host
          .querySelector('[data-testid="tactical-viewport"]')
          ?.getAttribute("data-origin-anchored"),
      ).toBe("true");
      expect(adapter.getCamera().originLla?.[0]).toBeCloseTo(
        trueOriginLla[0],
        8,
      );
      expect(adapter.getCamera().originLla?.[1]).toBeCloseTo(
        trueOriginLla[1],
        8,
      );

      skgStore.entities.set(contactId, coLocatedContact);
      flushSync();

      const vehScreen = adapter.project({ ned: vehicleOffsetNed });
      const contactScreen = adapter.project({ lla: vehicleLla });
      expect(vehScreen.visible).toBe(true);
      expect(contactScreen.visible).toBe(true);
      expect(contactScreen.x).toBeCloseTo(vehScreen.x, 4);
      expect(contactScreen.y).toBeCloseTo(vehScreen.y, 4);

      unmount(app);
      host.remove();
    }

    // Arrival Order 2:
    //  (a) External contact arrives first -> withheld;
    //  (b) Snapshot-first SKG vehicle startup with global fix + default zero NED and zero WS1 samples -> STILL withheld (never locks [0,0,0] permanently!);
    //  (c) Legacy WS1 frame omitting has_local_pos flag -> STILL withheld;
    //  (d) Explicit #33-before-#32 WS1 frame (has_local_pos: false) -> STILL withheld;
    //  (e) First confirmed #32 LOCAL_POSITION_NED -> anchors true NED=[0,0,0] origin and co-locates contact!
    {
      let simNowMs = nowMs;
      let ws1Socket: {
        readyState: number;
        onopen: ((event: never) => unknown) | null;
        onmessage: ((event: MessageEvent<unknown>) => unknown) | null;
        onerror: ((event: never) => unknown) | null;
        onclose: ((event: CloseEvent) => unknown) | null;
        send(data: string): void;
        close(code?: number, reason?: string): void;
      } | null = null;
      const host = document.createElement("div");
      document.body.appendChild(host);
      const telemetryStore = createTelemetryStore({
        now: () => simNowMs,
        createSocket: () => {
          ws1Socket = {
            readyState: 1,
            onopen: null,
            onmessage: null,
            onerror: null,
            onclose: null,
            send: () => undefined,
            close: () => undefined,
          };
          return ws1Socket;
        },
      });
      telemetryStore.connect("ws://127.0.0.1:8080/ws/telemetry");
      ws1Socket!.onopen?.(new Event("open") as never);

      const skgStore = createSkgStore({ now: () => simNowMs });
      const viewportStore = createViewportStore({ skgStore });
      const adapter = new NedGridAdapter({
        widthPx: 640,
        heightPx: 420,
        zoomPxPerM: 1.5,
      });

      skgStore.entities.set(contactId, coLocatedContact);
      const app = mount(TacticalCanvas, {
        target: host,
        props: {
          telemetryStore,
          skgStore,
          viewportStore,
          adapter,
          now: () => simNowMs,
        },
      });
      flushSync();

      // (a) Contact alone MUST NOT establish tactical origin.
      expect(adapter.getCamera().originLla).toBeNull();
      expect(adapter.project({ lla: vehicleLla }).visible).toBe(false);
      expect(
        host
          .querySelector('[data-testid="tactical-viewport"]')
          ?.getAttribute("data-origin-anchored"),
      ).toBe("false");
      expect(
        host.querySelector('[data-testid="mixed-frame-withheld"]'),
      ).not.toBeNull();

      // (b) Normal snapshot-first SKG startup: global fix present, default zero NED [0,0,0], no WS1 sample yet.
      const startupIso = new Date(simNowMs).toISOString();
      skgStore.ingestRawFrame(
        JSON.stringify({
          type: "put",
          bucket: "ENTITY_STATES",
          key: vehicleId,
          revision: 1,
          payload: {
            id: vehicleId,
            updated_at: startupIso,
            triples: [
              {
                subject: vehicleId,
                predicate: "uxs.domain.kind",
                object: "quadcopter",
                source: "bbops-mavlink-ingest",
                timestamp: startupIso,
                confidence: 1,
              },
              {
                subject: vehicleId,
                predicate: "uxs.authority.state",
                object: "companion-teleop",
                source: "bbops-mavlink-ingest",
                timestamp: startupIso,
                confidence: 1,
              },
              {
                subject: vehicleId,
                predicate: "uxs.mode.companion",
                object: "teleop-velocity",
                source: "bbops-mavlink-ingest",
                timestamp: startupIso,
                confidence: 1,
              },
              {
                subject: vehicleId,
                predicate: "uxs.link.tier",
                object: "mesh-high-rate",
                source: "bbops-mavlink-ingest",
                timestamp: startupIso,
                confidence: 1,
              },
              {
                subject: vehicleId,
                predicate: "uxs.health.armed",
                object: true,
                source: "bbops-mavlink-ingest",
                timestamp: startupIso,
                confidence: 1,
              },
              {
                subject: vehicleId,
                predicate: "uxs.health.flags",
                object: {
                  clock_sync_locked: true,
                  teleop_deadman_clamped: false,
                  crsf_link_healthy: true,
                  companion_trunk_healthy: true,
                },
                source: "bbops-mavlink-ingest",
                timestamp: startupIso,
                confidence: 1,
              },
              {
                subject: vehicleId,
                predicate: "uxs.health.failsafes",
                object: 0,
                source: "bbops-mavlink-ingest",
                timestamp: startupIso,
                confidence: 1,
              },
              {
                subject: vehicleId,
                predicate: "uxs.health.crcerrors",
                object: 0,
                source: "bbops-mavlink-ingest",
                timestamp: startupIso,
                confidence: 1,
              },
              {
                subject: vehicleId,
                predicate: "uxs.health.loopdtus",
                object: 1000,
                source: "bbops-mavlink-ingest",
                timestamp: startupIso,
                confidence: 1,
              },
              {
                subject: vehicleId,
                predicate: "uxs.health.maxloopdtus",
                object: 1200,
                source: "bbops-mavlink-ingest",
                timestamp: startupIso,
                confidence: 1,
              },
              {
                subject: vehicleId,
                predicate: "uxs.health.rttus",
                object: 400,
                source: "bbops-mavlink-ingest",
                timestamp: startupIso,
                confidence: 1,
              },
              {
                subject: vehicleId,
                predicate: "uxs.attitude.quaternion",
                object: [0, 0, 0, 0],
                source: "bbops-mavlink-ingest",
                timestamp: "1970-01-01T00:00:00Z",
                confidence: 0,
              },
              {
                subject: vehicleId,
                predicate: "uxs.attitude.rates",
                object: [0, 0, 0],
                source: "bbops-mavlink-ingest",
                timestamp: "1970-01-01T00:00:00Z",
                confidence: 0,
              },
              {
                subject: vehicleId,
                predicate: "uxs.kinematics.ned",
                object: [0, 0, 0],
                source: "bbops-mavlink-ingest",
                timestamp: startupIso,
                confidence: 1,
              },
              {
                subject: vehicleId,
                predicate: "uxs.kinematics.vx",
                object: 0,
                source: "bbops-mavlink-ingest",
                timestamp: startupIso,
                confidence: 1,
              },
              {
                subject: vehicleId,
                predicate: "uxs.kinematics.vy",
                object: 0,
                source: "bbops-mavlink-ingest",
                timestamp: startupIso,
                confidence: 1,
              },
              {
                subject: vehicleId,
                predicate: "uxs.kinematics.vz",
                object: 0,
                source: "bbops-mavlink-ingest",
                timestamp: startupIso,
                confidence: 1,
              },
              {
                subject: vehicleId,
                predicate: "uxs.kinematics.headingdeg",
                object: 0,
                source: "bbops-mavlink-ingest",
                timestamp: startupIso,
                confidence: 1,
                context: "measured",
              },
              {
                subject: vehicleId,
                predicate: "geo.location.latitude",
                object: vehicleLla[0],
                source: "bbops-mavlink-ingest",
                timestamp: startupIso,
                confidence: 1,
                context: "measured",
              },
              {
                subject: vehicleId,
                predicate: "geo.location.longitude",
                object: vehicleLla[1],
                source: "bbops-mavlink-ingest",
                timestamp: startupIso,
                confidence: 1,
                context: "measured",
              },
              {
                subject: vehicleId,
                predicate: "geo.location.altitude",
                object: vehicleLla[2],
                source: "bbops-mavlink-ingest",
                timestamp: startupIso,
                confidence: 1,
                context: "measured",
              },
            ],
          },
        }),
      );
      skgStore.ingestRawFrame(
        JSON.stringify({
          type: "synced",
          bucket: "ENTITY_STATES",
          revision: 1,
        }),
      );
      skgStore.entities.set(contactId, coLocatedContact);
      flushSync();

      expect(adapter.getCamera().originLla).toBeNull();
      expect(
        host.querySelector('[data-testid="mixed-frame-withheld"]'),
      ).not.toBeNull();

      // (c) Legacy WS1 frame omitting has_local_pos flag -> STILL withheld!
      simNowMs += 5;
      telemetryStore.ingestRawFrame(
        JSON.stringify({
          type: "data",
          subject: "bbops.telemetry.1",
          timestamp: simNowMs,
          payload: {
            sysid: 1,
            domain: "quadcopter",
            seq: 1,
            msg_id: 33,
            quat_wxyz: [1, 0, 0, 0],
            has_attitude: true,
            body_rates: [0, 0, 0],
            ned_xyz: [25, -10, -20],
            ned_vel: [0, 0, 0],
            lat_lon_alt: vehicleLla,
            has_global_pos: true,
            hdg_deg: 0,
            timestamp: simNowMs,
          },
        }),
      );
      // Force reactive pass so TacticalCanvas re-evaluates anchorSharedFrameOrigin.
      skgStore.entities.set(
        vehicleId,
        makeVehicleEntity([0, 0, 0], vehicleLla),
      );
      flushSync();

      expect(adapter.getCamera().originLla).toBeNull();
      expect(
        host.querySelector('[data-testid="mixed-frame-withheld"]'),
      ).not.toBeNull();

      // (d) Explicit #33-before-#32 WS1 frame (has_local_pos: false) -> STILL withheld.
      simNowMs += 5;
      telemetryStore.ingestRawFrame(
        JSON.stringify({
          type: "data",
          subject: "bbops.telemetry.1",
          timestamp: simNowMs,
          payload: {
            sysid: 1,
            domain: "quadcopter",
            seq: 2,
            msg_id: 33,
            quat_wxyz: [0, 0, 0, 0],
            has_attitude: false,
            body_rates: [0, 0, 0],
            ned_xyz: [0, 0, -20],
            has_local_pos: false,
            ned_vel: [0, 0, 0],
            lat_lon_alt: vehicleLla,
            has_global_pos: true,
            hdg_deg: 0,
            timestamp: simNowMs,
          },
        }),
      );
      skgStore.entities.set(
        vehicleId,
        makeVehicleEntity([0, 0, 0], vehicleLla),
      );
      flushSync();

      expect(adapter.getCamera().originLla).toBeNull();
      expect(
        host.querySelector('[data-testid="mixed-frame-withheld"]'),
      ).not.toBeNull();

      // (e) First confirmed #32 LOCAL_POSITION_NED arrives with true offset [100, -45, -20] and has_local_pos: true.
      simNowMs += 10;
      telemetryStore.ingestRawFrame(
        JSON.stringify({
          type: "data",
          subject: "bbops.telemetry.1",
          timestamp: simNowMs,
          payload: {
            sysid: 1,
            domain: "quadcopter",
            seq: 3,
            msg_id: 32,
            quat_wxyz: [1, 0, 0, 0],
            has_attitude: true,
            body_rates: [0, 0, 0],
            ned_xyz: vehicleOffsetNed,
            has_local_pos: true,
            ned_vel: [0, 0, 0],
            lat_lon_alt: vehicleLla,
            has_global_pos: true,
            hdg_deg: 0,
            timestamp: simNowMs,
          },
        }),
      );
      skgStore.entities.set(
        vehicleId,
        makeVehicleEntity(vehicleOffsetNed, vehicleLla),
      );
      flushSync();

      expect(adapter.getCamera().originLla?.[0]).toBeCloseTo(
        trueOriginLla[0],
        8,
      );
      expect(adapter.getCamera().originLla?.[1]).toBeCloseTo(
        trueOriginLla[1],
        8,
      );
      expect(
        host.querySelector('[data-testid="mixed-frame-withheld"]'),
      ).toBeNull();

      const vehScreen = adapter.project({ ned: vehicleOffsetNed });
      const contactScreen = adapter.project({ lla: vehicleLla });
      expect(contactScreen.visible).toBe(true);
      expect(contactScreen.x).toBeCloseTo(vehScreen.x, 4);
      expect(contactScreen.y).toBeCloseTo(vehScreen.y, 4);

      unmount(app);
      telemetryStore.disconnect();
      host.remove();
    }
  });

  // spec: cop-ui / Renderer-agnostic ViewportAdapter, surface-vs-safety-HUD layer split, and multi-domain tactical canvas
  it("withholds map frame origin on legacy v1 global-only zero-NED SKG replays while anchoring genuinely measured local-zero [0,0,0] vehicles", () => {
    const nowMs = Date.parse("2026-11-24T12:30:00.000Z");
    const tsIso = new Date(nowMs).toISOString();
    const vehicleId = "boxbotix.local.robotics.uxs.vehicle.1";
    const contactId = "ais.uscg.marine.vessel.mmsi.367999111";
    const originLla: [number, number, number] = [30.2672, -97.7431, 150];

    const telemetryStore = createTelemetryStore({ now: () => nowMs });
    let ws2Socket: {
      readyState: number;
      onopen: ((event: never) => unknown) | null;
      onmessage: ((event: MessageEvent<unknown>) => unknown) | null;
      onerror: ((event: never) => unknown) | null;
      onclose: ((event: CloseEvent) => unknown) | null;
      send(data: string): void;
      close(code?: number, reason?: string): void;
    } | null = null;
    const skgStore = createSkgStore({
      now: () => nowMs,
      createSocket: () => {
        ws2Socket = {
          readyState: 1,
          onopen: null,
          onmessage: null,
          onerror: null,
          onclose: null,
          send: () => undefined,
          close: () => undefined,
        };
        return ws2Socket;
      },
    });
    skgStore.connect("ws://127.0.0.1:8080/ws/graph");
    ws2Socket!.onopen?.(new Event("open") as never);
    const viewportStore = createViewportStore({ skgStore });
    const adapter = new NedGridAdapter({ widthPx: 800, heightPx: 600 });

    const makeTriples = (measuredZeroContext: boolean) => {
      const mk = (
        predicate: string,
        object: unknown,
        context?: string,
      ): {
        subject: string;
        predicate: string;
        object: unknown;
        source: string;
        timestamp: string;
        confidence: number;
        context?: string;
      } => ({
        subject: vehicleId,
        predicate,
        object,
        source: "mavlink_0xb1",
        timestamp: tsIso,
        confidence: 1,
        ...(context ? { context } : {}),
      });
      const ctx = measuredZeroContext ? "measured" : undefined;
      return [
        mk("uxs.domain.kind", "quadcopter"),
        mk("uxs.authority.state", "companion-teleop"),
        mk("uxs.mode.companion", "teleop-velocity"),
        mk("uxs.link.tier", "mesh-high-rate"),
        mk("uxs.health.armed", true),
        mk("uxs.health.flags", {
          clock_sync_locked: true,
          teleop_deadman_clamped: false,
          crsf_link_healthy: true,
          companion_trunk_healthy: true,
        }),
        mk("uxs.health.failsafes", 0),
        mk("uxs.health.crcerrors", 0),
        mk("uxs.health.loopdtus", 1000),
        mk("uxs.health.maxloopdtus", 1200),
        mk("uxs.health.rttus", 400),
        mk("uxs.attitude.quaternion", [1, 0, 0, 0], ctx),
        mk("uxs.attitude.rates", [0, 0, 0]),
        mk("uxs.kinematics.ned", [0, 0, 0], ctx),
        mk("uxs.kinematics.vx", 0),
        mk("uxs.kinematics.vy", 0),
        mk("uxs.kinematics.vz", 0),
        mk("uxs.kinematics.headingdeg", 0, ctx),
        mk("geo.location.latitude", originLla[0], ctx),
        mk("geo.location.longitude", originLla[1], ctx),
        mk("geo.location.altitude", originLla[2], ctx),
      ];
    };

    // 1. Ingest a retained legacy v1 global-only payload (confidence=1, no context on zero NED).
    skgStore.ingestRawFrame(
      JSON.stringify({
        type: "put",
        bucket: "ENTITY_STATES",
        key: vehicleId,
        revision: 1,
        payload: {
          id: vehicleId,
          updated_at: tsIso,
          triples: makeTriples(false),
        },
      }),
    );
    skgStore.ingestRawFrame(
      JSON.stringify({ type: "synced", bucket: "ENTITY_STATES", revision: 1 }),
    );
    skgStore.entities.set(contactId, {
      id: contactId,
      kind: "external-contact",
      kvRevision: 2,
      poisoned: false,
      poisonError: null,
      title: "USCG Buoy",
      contactType: "ais",
      indexingProfile: "knowledge",
      source: "ais-bridge",
      observedAt: tsIso,
      receivedAt: tsIso,
      updatedAt: tsIso,
      expiresAt: null,
      feedStatus: "live",
      uncertaintyMeters: null,
      location: {
        latitude: originLla[0],
        longitude: originLla[1],
        altitude: originLla[2],
      },
      triples: [],
    });

    const host = document.createElement("div");
    document.body.appendChild(host);
    const app = mount(TacticalCanvas, {
      target: host,
      props: {
        telemetryStore,
        skgStore,
        viewportStore,
        adapter,
        now: () => nowMs,
      },
    });
    flushSync();

    expect(adapter.getCamera().originLla).toBeNull();
    expect(
      host.querySelector('[data-testid="mixed-frame-withheld"]'),
    ).not.toBeNull();

    // 2. Ingest a modern vehicle genuinely measured at local origin [0, 0, 0] (context: "measured").
    skgStore.ingestRawFrame(
      JSON.stringify({
        type: "put",
        bucket: "ENTITY_STATES",
        key: vehicleId,
        revision: 3,
        payload: {
          id: vehicleId,
          updated_at: tsIso,
          triples: makeTriples(true),
        },
      }),
    );
    flushSync();

    expect(adapter.getCamera().originLla?.[0]).toBeCloseTo(originLla[0], 8);
    expect(adapter.getCamera().originLla?.[1]).toBeCloseTo(originLla[1], 8);
    expect(
      host.querySelector('[data-testid="mixed-frame-withheld"]'),
    ).toBeNull();

    // 3. Direct jump to a new geographic local frame ([37.7749, -122.4194, 25] at NED [20, -10, -5]) re-anchors originLla so local & geo projections agree.
    expect(
      anchorSharedFrameOrigin(adapter, [
        {
          ned: [20, -10, -5],
          lla: [37.7749, -122.4194, 25],
          hasLocalPos: true,
          hasVerifiedFrameReference: true,
          stale: false,
          localPosObservedAtMs: nowMs + 2_000,
          globalPosObservedAtMs: nowMs + 2_050,
        },
      ]),
    ).toBe(true);
    const relocatedNedProj = adapter.project({ ned: [20, -10, -5] });
    const relocatedGeoProj = adapter.project({ lla: [37.7749, -122.4194, 25] });
    expect(relocatedNedProj.visible).toBe(true);
    expect(relocatedGeoProj.visible).toBe(true);
    expect(relocatedGeoProj.x).toBeCloseTo(relocatedNedProj.x, 4);
    expect(relocatedGeoProj.y).toBeCloseTo(relocatedNedProj.y, 4);

    // 4. Two live vehicles with incompatible local frames (SF vs Austin) withhold mixed-frame placement rather than misplacing contacts.
    expect(
      anchorSharedFrameOrigin(adapter, [
        {
          ned: [0, 0, 0],
          lla: [37.7749, -122.4194, 25],
          hasLocalPos: true,
          hasVerifiedFrameReference: true,
          stale: false,
          localPosObservedAtMs: nowMs + 3_000,
          globalPosObservedAtMs: nowMs + 3_000,
        },
        {
          ned: [0, 0, 0],
          lla: [30.2672, -97.7431, 150],
          hasLocalPos: true,
          hasVerifiedFrameReference: true,
          stale: false,
          localPosObservedAtMs: nowMs + 3_000,
          globalPosObservedAtMs: nowMs + 3_000,
        },
      ]),
    ).toBe(false);
    expect(adapter.getCamera().originLla).toBeNull();

    unmount(app);
    skgStore.disconnect();
    host.remove();
  });

  // spec: cop-ui / Renderer-agnostic ViewportAdapter, surface-vs-safety-HUD layer split, and multi-domain tactical canvas
  it("keeps stationary geographic contacts motionless across asynchronous local updates with cached GPS fixes, withholds beyond drift tolerance, and re-anchors only on confirmed relocation", () => {
    let simNowMs = Date.parse("2026-11-24T14:00:00.000Z");
    let ws1Socket: {
      readyState: number;
      onopen: ((event: never) => unknown) | null;
      onmessage: ((event: MessageEvent<unknown>) => unknown) | null;
      onerror: ((event: never) => unknown) | null;
      onclose: ((event: CloseEvent) => unknown) | null;
      send(data: string): void;
      close(code?: number, reason?: string): void;
    } | null = null;
    let ws2Socket: {
      readyState: number;
      onopen: ((event: never) => unknown) | null;
      onmessage: ((event: MessageEvent<unknown>) => unknown) | null;
      onerror: ((event: never) => unknown) | null;
      onclose: ((event: CloseEvent) => unknown) | null;
      send(data: string): void;
      close(code?: number, reason?: string): void;
    } | null = null;

    const telemetryStore = createTelemetryStore({
      now: () => simNowMs,
      createSocket: () => {
        ws1Socket = {
          readyState: 1,
          onopen: null,
          onmessage: null,
          onerror: null,
          onclose: null,
          send: () => undefined,
          close: () => undefined,
        };
        return ws1Socket;
      },
    });
    const skgStore = createSkgStore({
      now: () => simNowMs,
      createSocket: () => {
        ws2Socket = {
          readyState: 1,
          onopen: null,
          onmessage: null,
          onerror: null,
          onclose: null,
          send: () => undefined,
          close: () => undefined,
        };
        return ws2Socket;
      },
    });
    telemetryStore.connect("ws://127.0.0.1:8080/ws/telemetry");
    ws1Socket!.onopen?.(new Event("open") as never);
    skgStore.connect("ws://127.0.0.1:8080/ws/graph");
    ws2Socket!.onopen?.(new Event("open") as never);

    const viewportStore = createViewportStore({ skgStore });
    const adapter = new NedGridAdapter({
      widthPx: 800,
      heightPx: 600,
      zoomPxPerM: 4,
    });
    const vehicleId = "boxbotix.local.robotics.uxs.vehicle.1";
    const beaconId = "ais.uscg.cop.contact.vessel.sf-beacon";
    const sfBeaconLla: [number, number, number] = [37.7749, -122.4194, 25];
    const sfTrueOriginLla = deriveNedOriginLla([0, 0, -2], sfBeaconLla)!;
    const austinBeaconLla: [number, number, number] = [30.2672, -97.7431, 150];

    const putVehicleEntity = (
      ned: [number, number, number],
      lla: [number, number, number],
      localIso: string,
      globalIso: string,
      revision: number,
    ) => {
      const latestIso = localIso >= globalIso ? localIso : globalIso;
      skgStore.ingestRawFrame(
        JSON.stringify({
          type: "put",
          bucket: "ENTITY_STATES",
          key: vehicleId,
          revision,
          payload: {
            id: vehicleId,
            updated_at: latestIso,
            triples: [
              {
                subject: vehicleId,
                predicate: "uxs.domain.kind",
                object: "rover",
                source: "mavlink_0xb1",
                timestamp: latestIso,
                confidence: 1,
              },
              {
                subject: vehicleId,
                predicate: "uxs.authority.state",
                object: "companion-teleop",
                source: "mavlink_0xb1",
                timestamp: latestIso,
                confidence: 1,
              },
              {
                subject: vehicleId,
                predicate: "uxs.mode.companion",
                object: "teleop-velocity",
                source: "mavlink_0xb1",
                timestamp: latestIso,
                confidence: 1,
              },
              {
                subject: vehicleId,
                predicate: "uxs.link.tier",
                object: "mesh-high-rate",
                source: "mavlink_0xb1",
                timestamp: latestIso,
                confidence: 1,
              },
              {
                subject: vehicleId,
                predicate: "uxs.health.armed",
                object: true,
                source: "mavlink_0xb1",
                timestamp: latestIso,
                confidence: 1,
              },
              {
                subject: vehicleId,
                predicate: "uxs.health.flags",
                object: {
                  clock_sync_locked: true,
                  teleop_deadman_clamped: false,
                  crsf_link_healthy: true,
                  companion_trunk_healthy: true,
                },
                source: "mavlink_0xb1",
                timestamp: latestIso,
                confidence: 1,
              },
              {
                subject: vehicleId,
                predicate: "uxs.health.failsafes",
                object: 0,
                source: "mavlink_0xb1",
                timestamp: latestIso,
                confidence: 1,
              },
              {
                subject: vehicleId,
                predicate: "uxs.health.crcerrors",
                object: 0,
                source: "mavlink_0xb1",
                timestamp: latestIso,
                confidence: 1,
              },
              {
                subject: vehicleId,
                predicate: "uxs.health.loopdtus",
                object: 1000,
                source: "mavlink_0xb1",
                timestamp: latestIso,
                confidence: 1,
              },
              {
                subject: vehicleId,
                predicate: "uxs.health.maxloopdtus",
                object: 1200,
                source: "mavlink_0xb1",
                timestamp: latestIso,
                confidence: 1,
              },
              {
                subject: vehicleId,
                predicate: "uxs.health.rttus",
                object: 400,
                source: "mavlink_0xb1",
                timestamp: latestIso,
                confidence: 1,
              },
              {
                subject: vehicleId,
                predicate: "uxs.attitude.quaternion",
                object: [1, 0, 0, 0],
                source: "mavlink_0xb1",
                timestamp: latestIso,
                confidence: 1,
                context: "measured",
              },
              {
                subject: vehicleId,
                predicate: "uxs.attitude.rates",
                object: [0, 0, 0],
                source: "mavlink_0xb1",
                timestamp: latestIso,
                confidence: 1,
              },
              {
                subject: vehicleId,
                predicate: "uxs.kinematics.ned",
                object: ned,
                source: "mavlink_0xb1",
                timestamp: localIso,
                confidence: 1,
                context: "measured",
              },
              {
                subject: vehicleId,
                predicate: "uxs.kinematics.vx",
                object: 0,
                source: "mavlink_0xb1",
                timestamp: localIso,
                confidence: 1,
              },
              {
                subject: vehicleId,
                predicate: "uxs.kinematics.vy",
                object: 0,
                source: "mavlink_0xb1",
                timestamp: localIso,
                confidence: 1,
              },
              {
                subject: vehicleId,
                predicate: "uxs.kinematics.vz",
                object: 0,
                source: "mavlink_0xb1",
                timestamp: localIso,
                confidence: 1,
              },
              {
                subject: vehicleId,
                predicate: "uxs.kinematics.headingdeg",
                object: 0,
                source: "mavlink_0xb1",
                timestamp: latestIso,
                confidence: 1,
                context: "measured",
              },
              {
                subject: vehicleId,
                predicate: "geo.location.latitude",
                object: lla[0],
                source: "mavlink_0xb1",
                timestamp: globalIso,
                confidence: 1,
                context: "measured",
              },
              {
                subject: vehicleId,
                predicate: "geo.location.longitude",
                object: lla[1],
                source: "mavlink_0xb1",
                timestamp: globalIso,
                confidence: 1,
                context: "measured",
              },
              {
                subject: vehicleId,
                predicate: "geo.location.altitude",
                object: lla[2],
                source: "mavlink_0xb1",
                timestamp: globalIso,
                confidence: 1,
                context: "measured",
              },
            ],
          },
        }),
      );
    };

    const initialIso = new Date(simNowMs).toISOString();
    putVehicleEntity([0, 0, -2], sfBeaconLla, initialIso, initialIso, 1);
    skgStore.ingestRawFrame(
      JSON.stringify({ type: "synced", bucket: "ENTITY_STATES", revision: 1 }),
    );
    skgStore.entities.set(beaconId, {
      id: beaconId,
      kind: "external-contact",
      kvRevision: 2,
      poisoned: false,
      poisonError: null,
      title: "SF Beacon",
      contactType: "a-f-G-U-C",
      indexingProfile: null,
      source: "tak-bridge",
      observedAt: initialIso,
      receivedAt: initialIso,
      updatedAt: initialIso,
      expiresAt: null,
      feedStatus: "live",
      uncertaintyMeters: 5,
      location: {
        latitude: sfBeaconLla[0],
        longitude: sfBeaconLla[1],
        altitude: sfBeaconLla[2],
      },
      triples: [],
    });

    telemetryStore.ingestRawFrame(
      JSON.stringify({
        type: "data",
        subject: "bbops.telemetry.1",
        timestamp: simNowMs,
        payload: {
          sysid: 1,
          domain: "rover",
          seq: 1,
          msg_id: 33,
          quat_wxyz: [1, 0, 0, 0],
          has_attitude: true,
          body_rates: [0, 0, 0],
          ned_xyz: [0, 0, -2],
          has_local_pos: false,
          ned_vel: [0, 0, 0],
          lat_lon_alt: sfBeaconLla,
          has_global_pos: true,
          hdg_deg: 0,
          timestamp: simNowMs,
        },
      }),
    );
    simNowMs += 20;
    telemetryStore.ingestRawFrame(
      JSON.stringify({
        type: "data",
        subject: "bbops.telemetry.1",
        timestamp: simNowMs,
        payload: {
          sysid: 1,
          domain: "rover",
          seq: 2,
          msg_id: 32,
          quat_wxyz: [1, 0, 0, 0],
          has_attitude: true,
          body_rates: [0, 0, 0],
          ned_xyz: [0, 0, -2],
          has_local_pos: true,
          ned_vel: [0, 0, 0],
          lat_lon_alt: sfBeaconLla,
          has_global_pos: true,
          hdg_deg: 0,
          timestamp: simNowMs,
        },
      }),
    );

    const host = document.createElement("div");
    document.body.appendChild(host);
    const app = mount(TacticalCanvas, {
      target: host,
      props: {
        telemetryStore,
        skgStore,
        viewportStore,
        adapter,
        now: () => simNowMs,
      },
    });
    flushSync();

    const beaconInitial = adapter.project({ lla: sfBeaconLla });
    expect(beaconInitial.visible).toBe(true);
    expect(beaconInitial.x).toBeCloseTo(320, 6);
    expect(beaconInitial.y).toBeCloseTo(210, 6);

    // 1. 4 seconds later, a fresh #32 local update [12.5, -4.25, -2] (52.81 px) arrives carrying the cached #33 fix from 4s ago.
    simNowMs += 4_000;
    telemetryStore.ingestRawFrame(
      JSON.stringify({
        type: "data",
        subject: "bbops.telemetry.1",
        timestamp: simNowMs,
        payload: {
          sysid: 1,
          domain: "rover",
          seq: 3,
          msg_id: 32,
          quat_wxyz: [1, 0, 0, 0],
          has_attitude: true,
          body_rates: [0, 0, 0],
          ned_xyz: [12.5, -4.25, -2],
          has_local_pos: true,
          ned_vel: [1, -0.3, 0],
          lat_lon_alt: sfBeaconLla,
          has_global_pos: true,
          hdg_deg: 0,
          timestamp: simNowMs,
        },
      }),
    );
    putVehicleEntity(
      [12.5, -4.25, -2],
      sfBeaconLla,
      new Date(simNowMs).toISOString(),
      initialIso,
      3,
    );
    flushSync();

    const vehMoved = adapter.project({ ned: [12.5, -4.25, -2] });
    const beaconAfterLocalMove = adapter.project({ lla: sfBeaconLla });
    expect(
      Math.hypot(vehMoved.x - beaconInitial.x, vehMoved.y - beaconInitial.y),
    ).toBeCloseTo(Math.hypot(12.5, -4.25) * 4, 2);
    expect(beaconAfterLocalMove.visible).toBe(true);
    expect(beaconAfterLocalMove.x).toBe(beaconInitial.x);
    expect(beaconAfterLocalMove.y).toBe(beaconInitial.y);

    // 2. Large local-only step [60, -20, -2] (> 25 m) still paired with the old cached GPS fix withholds mixed-frame placement across ticks rather than moving SF Beacon!
    simNowMs += 500;
    telemetryStore.ingestRawFrame(
      JSON.stringify({
        type: "data",
        subject: "bbops.telemetry.1",
        timestamp: simNowMs,
        payload: {
          sysid: 1,
          domain: "rover",
          seq: 4,
          msg_id: 32,
          quat_wxyz: [1, 0, 0, 0],
          has_attitude: true,
          body_rates: [0, 0, 0],
          ned_xyz: [60, -20, -2],
          has_local_pos: true,
          ned_vel: [4, -1, 0],
          lat_lon_alt: sfBeaconLla,
          has_global_pos: true,
          hdg_deg: 0,
          timestamp: simNowMs,
        },
      }),
    );
    const largeLocalIso = new Date(simNowMs).toISOString();
    putVehicleEntity([60, -20, -2], sfBeaconLla, largeLocalIso, initialIso, 4);
    flushSync();

    expect(adapter.getCamera().originLla).toBeNull();
    expect(adapter.project({ lla: sfBeaconLla }).visible).toBe(false);
    expect(
      host.querySelector('[data-testid="mixed-frame-withheld"]'),
    ).not.toBeNull();

    // Repeated tick with the same cached GPS fix remains withheld.
    putVehicleEntity([60, -20, -2], sfBeaconLla, largeLocalIso, initialIso, 5);
    flushSync();
    expect(adapter.getCamera().originLla).toBeNull();
    expect(adapter.project({ lla: sfBeaconLla }).visible).toBe(false);

    // 3. Matching fresh #33 fix in the same frame restores originLla with 0 px stationary beacon shift.
    simNowMs += 100;
    const sfAt60mLla = localNedToLla([60, -20, -2], sfTrueOriginLla)!;
    telemetryStore.ingestRawFrame(
      JSON.stringify({
        type: "data",
        subject: "bbops.telemetry.1",
        timestamp: simNowMs,
        payload: {
          sysid: 1,
          domain: "rover",
          seq: 5,
          msg_id: 33,
          quat_wxyz: [1, 0, 0, 0],
          has_attitude: true,
          body_rates: [0, 0, 0],
          ned_xyz: [60, -20, -2],
          has_local_pos: true,
          ned_vel: [4, -1, 0],
          lat_lon_alt: sfAt60mLla,
          has_global_pos: true,
          hdg_deg: 0,
          timestamp: simNowMs,
        },
      }),
    );
    putVehicleEntity(
      [60, -20, -2],
      sfAt60mLla,
      largeLocalIso,
      new Date(simNowMs).toISOString(),
      6,
    );
    flushSync();

    const beaconRestored = adapter.project({ lla: sfBeaconLla });
    expect(beaconRestored.visible).toBe(true);
    expect(beaconRestored.x).toBeCloseTo(beaconInitial.x, 4);
    expect(beaconRestored.y).toBeCloseTo(beaconInitial.y, 4);

    // 4. Confirmed relocation (both #33 and #32 freshly observed in Austin) updates originLla to Austin.
    simNowMs += 2_000;
    const austinGlobalMs = simNowMs;
    telemetryStore.ingestRawFrame(
      JSON.stringify({
        type: "data",
        subject: "bbops.telemetry.1",
        timestamp: austinGlobalMs,
        payload: {
          sysid: 1,
          domain: "rover",
          seq: 6,
          msg_id: 33,
          quat_wxyz: [1, 0, 0, 0],
          has_attitude: true,
          body_rates: [0, 0, 0],
          ned_xyz: [5, -3, -1],
          has_local_pos: true,
          ned_vel: [0, 0, 0],
          lat_lon_alt: austinBeaconLla,
          has_global_pos: true,
          hdg_deg: 0,
          timestamp: austinGlobalMs,
        },
      }),
    );
    simNowMs += 50;
    const austinLocalMs = simNowMs;
    telemetryStore.ingestRawFrame(
      JSON.stringify({
        type: "data",
        subject: "bbops.telemetry.1",
        timestamp: austinLocalMs,
        payload: {
          sysid: 1,
          domain: "rover",
          seq: 7,
          msg_id: 32,
          quat_wxyz: [1, 0, 0, 0],
          has_attitude: true,
          body_rates: [0, 0, 0],
          ned_xyz: [5, -3, -1],
          has_local_pos: true,
          ned_vel: [0, 0, 0],
          lat_lon_alt: austinBeaconLla,
          has_global_pos: true,
          hdg_deg: 0,
          timestamp: austinLocalMs,
        },
      }),
    );
    putVehicleEntity(
      [5, -3, -1],
      austinBeaconLla,
      new Date(austinLocalMs).toISOString(),
      new Date(austinGlobalMs).toISOString(),
      7,
    );
    flushSync();

    const austinNedProj = adapter.project({ ned: [5, -3, -1] });
    const austinGeoProj = adapter.project({ lla: austinBeaconLla });
    expect(austinGeoProj.visible).toBe(true);
    expect(austinGeoProj.x).toBeCloseTo(austinNedProj.x, 4);
    expect(austinGeoProj.y).toBeCloseTo(austinNedProj.y, 4);

    unmount(app);
    telemetryStore.disconnect();
    skgStore.disconnect();
    host.remove();
  });

  // spec: cop-ui / Renderer-agnostic ViewportAdapter, surface-vs-safety-HUD layer split, and multi-domain tactical canvas
  it("keeps a withheld reference cleared when live vehicles time out to stale and restores geographic overlays only on new live confirming evidence", () => {
    let simNowMs = Date.parse("2026-11-24T15:00:00.000Z");
    let ws2Socket: {
      readyState: number;
      onopen: ((event: never) => unknown) | null;
      onmessage: ((event: MessageEvent<unknown>) => unknown) | null;
      onerror: ((event: never) => unknown) | null;
      onclose: ((event: CloseEvent) => unknown) | null;
      send(data: string): void;
      close(code?: number, reason?: string): void;
    } | null = null;
    const telemetryStore = createTelemetryStore({ now: () => simNowMs });
    const skgStore = createSkgStore({
      now: () => simNowMs,
      createSocket: () => {
        ws2Socket = {
          readyState: 1,
          onopen: null,
          onmessage: null,
          onerror: null,
          onclose: null,
          send: () => undefined,
          close: () => undefined,
        };
        return ws2Socket;
      },
    });
    skgStore.connect("ws://127.0.0.1:8080/ws/graph");
    ws2Socket!.onopen?.(new Event("open") as never);

    const viewportStore = createViewportStore({ skgStore });
    const adapter = new NedGridAdapter({ widthPx: 800, heightPx: 600 });
    const vehAId = "boxbotix.local.robotics.uxs.vehicle.1";
    const vehBId = "boxbotix.local.robotics.uxs.vehicle.2";
    const contactId = "ais.uscg.cop.contact.vessel.timeout-check";
    const sfLla: [number, number, number] = [37.7749, -122.4194, 25];
    const austinLla: [number, number, number] = [30.2672, -97.7431, 150];

    const buildVehicleEntity = (opts: {
      id: string;
      sysid: number;
      ned: [number, number, number];
      lla: [number, number, number];
      hasLocalPos: boolean;
      observedMs: number;
      revision: number;
    }) => {
      const iso = new Date(opts.observedMs).toISOString();
      return {
        id: opts.id,
        kind: "uxs-vehicle" as const,
        title: `Veh-${opts.sysid}`,
        org: "boxbotix",
        platform: "local",
        sysid: opts.sysid,
        domain: "quadcopter" as const,
        authority: "companion-teleop" as const,
        companionMode: "teleop-velocity" as const,
        linkTier: "mesh-high-rate" as const,
        armed: true,
        healthFlags: {
          clock_sync_locked: true,
          teleop_deadman_clamped: false,
          crsf_link_healthy: true,
          companion_trunk_healthy: true,
        },
        sysStatus: {
          failsafes: 0,
          crcErrors: 0,
          loopDtUs: 1000,
          maxLoopDtUs: 1200,
          rttUs: 400,
        },
        attitudeQuaternion: [1, 0, 0, 0] as [number, number, number, number],
        attitudeRates: [0, 0, 0] as [number, number, number],
        ned: opts.ned,
        velocityNed: [0, 0, 0] as [number, number, number],
        headingDeg: 0,
        location: {
          latitude: opts.lla[0],
          longitude: opts.lla[1],
          altitude: opts.lla[2],
        },
        hasAttitude: true,
        hasLocalPos: opts.hasLocalPos,
        hasGlobalPos: true,
        hasHeading: true,
        poseObservedAt: iso,
        localPosObservedAt: opts.hasLocalPos ? iso : null,
        globalPosObservedAt: iso,
        poseIngestedAtMs: opts.observedMs,
        poseIngestedLive: true,
        kvRevision: opts.revision,
        updatedAt: iso,
        poisoned: false,
        poisonError: null,
        indexingProfile: "full",
        source: "mavlink_0xb1",
        observedAt: iso,
        receivedAt: iso,
        expiresAt: null,
        uncertaintyMeters: null,
        feedStatus: "live" as const,
        ingestedAtMs: opts.observedMs,
        triples: [],
      };
    };

    const tA = simNowMs;
    skgStore.entities.set(
      vehAId,
      buildVehicleEntity({
        id: vehAId,
        sysid: 1,
        ned: [10, -5, -2],
        lla: sfLla,
        hasLocalPos: true,
        observedMs: tA,
        revision: 1,
      }),
    );
    skgStore.entities.set(contactId, {
      id: contactId,
      kind: "external-contact",
      kvRevision: 2,
      poisoned: false,
      poisonError: null,
      title: "Harbor Marker",
      contactType: "ais",
      indexingProfile: null,
      source: "ais-bridge",
      observedAt: new Date(tA).toISOString(),
      receivedAt: new Date(tA).toISOString(),
      updatedAt: new Date(tA).toISOString(),
      expiresAt: null,
      feedStatus: "live",
      uncertaintyMeters: 5,
      location: {
        latitude: sfLla[0],
        longitude: sfLla[1],
        altitude: sfLla[2],
      },
      triples: [],
    });

    const host = document.createElement("div");
    document.body.appendChild(host);
    const app = mount(TacticalCanvas, {
      target: host,
      props: {
        telemetryStore,
        skgStore,
        viewportStore,
        adapter,
        now: () => simNowMs,
      },
    });
    flushSync();

    // Step 1: Vehicle A establishes a verified reference.
    expect(adapter.getCamera().originLla).not.toBeNull();
    expect(
      host
        .querySelector('[data-testid="tactical-viewport"]')
        ?.getAttribute("data-origin-anchored"),
    ).toBe("true");
    expect(adapter.project({ lla: sfLla }).visible).toBe(true);

    // Step 2: Advance time (+5s) so Vehicle A goes stale, and supply live Vehicle B without a verified pair -> clears originLla.
    simNowMs += 5_000;
    const tB = simNowMs;
    skgStore.entities.set(
      vehBId,
      buildVehicleEntity({
        id: vehBId,
        sysid: 2,
        ned: [0, 0, 0],
        lla: austinLla,
        hasLocalPos: false,
        observedMs: tB,
        revision: 3,
      }),
    );
    flushSync();

    expect(adapter.getCamera().originLla).toBeNull();
    expect(
      host
        .querySelector('[data-testid="tactical-viewport"]')
        ?.getAttribute("data-origin-anchored"),
    ).toBe("false");
    expect(
      host.querySelector('[data-testid="mixed-frame-withheld"]'),
    ).not.toBeNull();
    expect(adapter.project({ lla: sfLla }).visible).toBe(false);

    // Step 3: Advance time (+5s) so Vehicle B ALSO times out to stale: true (no new data, only time passing).
    // Stale Vehicle A's old reference MUST NOT be re-enabled by timeout!
    simNowMs += 5_000;
    // Trigger reactive re-render at the new timestamp without mutating vehicle A or B pose evidence.
    skgStore.entities.set(contactId, {
      ...(skgStore.entities.get(contactId)! as ExternalContactEntityState),
      kvRevision: 4,
    });
    flushSync();

    expect(adapter.getCamera().originLla).toBeNull();
    expect(
      host
        .querySelector('[data-testid="tactical-viewport"]')
        ?.getAttribute("data-origin-anchored"),
    ).toBe("false");
    expect(
      host.querySelector('[data-testid="mixed-frame-withheld"]'),
    ).not.toBeNull();
    expect(adapter.project({ lla: sfLla }).visible).toBe(false);

    // Even removing timed-out Vehicle B while Vehicle A remains stale MUST NOT restore Vehicle A's old reference!
    skgStore.entities.delete(vehBId);
    flushSync();
    expect(adapter.getCamera().originLla).toBeNull();
    expect(
      host
        .querySelector('[data-testid="tactical-viewport"]')
        ?.getAttribute("data-origin-anchored"),
    ).toBe("false");

    // Step 4: New live confirming evidence (Vehicle B or A reporting a fresh contemporaneous pair) restores geographic overlays.
    simNowMs += 500;
    skgStore.entities.set(
      vehAId,
      buildVehicleEntity({
        id: vehAId,
        sysid: 1,
        ned: [10, -5, -2],
        lla: sfLla,
        hasLocalPos: true,
        observedMs: simNowMs,
        revision: 5,
      }),
    );
    flushSync();

    expect(adapter.getCamera().originLla).not.toBeNull();
    expect(
      host
        .querySelector('[data-testid="tactical-viewport"]')
        ?.getAttribute("data-origin-anchored"),
    ).toBe("true");
    expect(
      host.querySelector('[data-testid="mixed-frame-withheld"]'),
    ).toBeNull();
    expect(adapter.project({ lla: sfLla }).visible).toBe(true);

    unmount(app);
    skgStore.disconnect();
    host.remove();
  });

  // spec: cop-ui / Renderer-agnostic ViewportAdapter, surface-vs-safety-HUD layer split, and multi-domain tactical canvas
  it("withholds rather than shifting a stationary beacon on t=0 -> +400ms GPS refresh -> +1500ms (or +450ms / coalesced) local-only report, while re-anchoring on confirmed relocation", () => {
    const t0 = Date.parse("2026-11-24T16:00:00.000Z");
    const sfBeaconLla: [number, number, number] = [37.7749, -122.4194, 25];
    const sfOriginLla = deriveNedOriginLla([0, 0, -2], sfBeaconLla)!;
    const austinBeaconLla: [number, number, number] = [30.2672, -97.7431, 150];

    // Exact reproduction on 800x600 fixed camera (zoom 4 px/m -> center x = 400):
    // t=0 anchor -> +400ms identical geographic refresh -> +1500ms local report (-30 m East = +120 px if origin shifted) carrying cached +400ms fix.
    const adapter = new NedGridAdapter({
      widthPx: 800,
      heightPx: 600,
      zoomPxPerM: 4,
    });
    expect(
      anchorSharedFrameOrigin(adapter, [
        {
          ned: [0, 0, -2],
          lla: sfBeaconLla,
          hasLocalPos: true,
          hasVerifiedFrameReference: true,
          stale: false,
          localPosObservedAtMs: t0,
          globalPosObservedAtMs: t0,
        },
      ]),
    ).toBe(true);
    const projAtT0 = adapter.project({ lla: sfBeaconLla });
    expect(projAtT0.visible).toBe(true);
    expect(projAtT0.x).toBeCloseTo(400, 6);

    // +400ms: refresh identical geographic fix while local NED remains at t0.
    expect(
      anchorSharedFrameOrigin(adapter, [
        {
          ned: [0, 0, -2],
          lla: sfBeaconLla,
          hasLocalPos: true,
          hasVerifiedFrameReference: true,
          stale: false,
          localPosObservedAtMs: t0,
          globalPosObservedAtMs: t0 + 400,
        },
      ]),
    ).toBe(true);
    const projAt400 = adapter.project({ lla: sfBeaconLla });
    expect(projAt400.visible).toBe(true);
    expect(projAt400.x).toBe(projAtT0.x);

    // +1500ms: local report moves 30 m West ([0, -30, -2]) carrying the cached +400ms GPS fix -> MUST withhold rather than shifting beacon to x=520!
    expect(
      anchorSharedFrameOrigin(adapter, [
        {
          ned: [0, -30, -2],
          lla: sfBeaconLla,
          hasLocalPos: true,
          hasVerifiedFrameReference: true,
          stale: false,
          localPosObservedAtMs: t0 + 1_500,
          globalPosObservedAtMs: t0 + 400,
        },
      ]),
    ).toBe(false);
    expect(adapter.getCamera().originLla).toBeNull();
    expect(adapter.project({ lla: sfBeaconLla }).visible).toBe(false);

    // Tight-skew variant (+400ms GPS refresh 5 m West within drift tolerance -> +450ms local-only jump carrying that cached +400ms fix) withholds because globalMs did not advance past +400ms.
    const tightAdapter = new NedGridAdapter({
      widthPx: 800,
      heightPx: 600,
      zoomPxPerM: 4,
    });
    const sfAt5mWestLla = localNedToLla([0, -5, -2], sfOriginLla)!;
    expect(
      anchorSharedFrameOrigin(tightAdapter, [
        {
          ned: [0, 0, -2],
          lla: sfBeaconLla,
          hasLocalPos: true,
          hasVerifiedFrameReference: true,
          stale: false,
          localPosObservedAtMs: t0,
          globalPosObservedAtMs: t0,
        },
      ]),
    ).toBe(true);
    expect(
      anchorSharedFrameOrigin(tightAdapter, [
        {
          ned: [0, 0, -2],
          lla: sfAt5mWestLla,
          hasLocalPos: true,
          hasVerifiedFrameReference: true,
          stale: false,
          localPosObservedAtMs: t0,
          globalPosObservedAtMs: t0 + 400,
        },
      ]),
    ).toBe(true);
    expect(
      anchorSharedFrameOrigin(tightAdapter, [
        {
          ned: [0, -50, -2],
          lla: sfAt5mWestLla,
          hasLocalPos: true,
          hasVerifiedFrameReference: true,
          stale: false,
          localPosObservedAtMs: t0 + 450,
          globalPosObservedAtMs: t0 + 400,
        },
      ]),
    ).toBe(false);
    expect(tightAdapter.getCamera().originLla).toBeNull();

    // Coalesced variant (skipping intermediate +400ms render: t=0 -> t=+600ms with globalMs=+400ms and 30 m async move) also withholds across repeated ticks.
    const coalescedAdapter = new NedGridAdapter({
      widthPx: 800,
      heightPx: 600,
      zoomPxPerM: 4,
    });
    expect(
      anchorSharedFrameOrigin(coalescedAdapter, [
        {
          ned: [0, 0, -2],
          lla: sfBeaconLla,
          hasLocalPos: true,
          hasVerifiedFrameReference: true,
          stale: false,
          localPosObservedAtMs: t0,
          globalPosObservedAtMs: t0,
        },
      ]),
    ).toBe(true);
    const slightlyShiftedLla = localNedToLla([0, -2, -2], sfOriginLla)!;
    for (let tick = 0; tick < 2; tick += 1) {
      expect(
        anchorSharedFrameOrigin(coalescedAdapter, [
          {
            ned: [0, -32, -2],
            lla: slightlyShiftedLla,
            hasLocalPos: true,
            hasVerifiedFrameReference: true,
            stale: false,
            localPosObservedAtMs: t0 + 600,
            globalPosObservedAtMs: t0 + 400,
          },
        ]),
      ).toBe(false);
      expect(coalescedAdapter.getCamera().originLla).toBeNull();
    }

    // Matching geographic fix at +1600ms restores SF reference with zero stationary shift (x === 400).
    const sfMoved30mWestLla = localNedToLla([0, -30, -2], sfOriginLla)!;
    expect(
      anchorSharedFrameOrigin(adapter, [
        {
          ned: [0, -30, -2],
          lla: sfMoved30mWestLla,
          hasLocalPos: true,
          hasVerifiedFrameReference: true,
          stale: false,
          localPosObservedAtMs: t0 + 1_500,
          globalPosObservedAtMs: t0 + 1_600,
        },
      ]),
    ).toBe(true);
    const projRestored = adapter.project({ lla: sfBeaconLla });
    expect(projRestored.visible).toBe(true);
    expect(projRestored.x).toBeCloseTo(400, 6);
    expect(projRestored.y).toBeCloseTo(300, 6);

    // Positive relocation check: fresh contemporaneous Austin pair updates originLla so NED and LLA agree in Austin.
    expect(
      anchorSharedFrameOrigin(adapter, [
        {
          ned: [10, -6, -3],
          lla: austinBeaconLla,
          hasLocalPos: true,
          hasVerifiedFrameReference: true,
          stale: false,
          localPosObservedAtMs: t0 + 3_050,
          globalPosObservedAtMs: t0 + 3_000,
        },
      ]),
    ).toBe(true);
    const austinNed = adapter.project({ ned: [10, -6, -3] });
    const austinGeo = adapter.project({ lla: austinBeaconLla });
    expect(austinGeo.visible).toBe(true);
    expect(austinGeo.x).toBeCloseTo(austinNed.x, 4);
    expect(austinGeo.y).toBeCloseTo(austinNed.y, 4);
  });

  // spec: cop-ui / Renderer-agnostic ViewportAdapter, surface-vs-safety-HUD layer split, and multi-domain tactical canvas
  it("preserves a conflict hold across staggered vehicle expirations (t=0 A, t=+500ms conflicting B, t=+2600ms A expired while B still fresh, t=+3200ms both expired) until new confirming observations arrive", () => {
    let simNowMs = Date.parse("2026-11-24T17:00:00.000Z");
    let ws2Socket: {
      readyState: number;
      onopen: ((event: never) => unknown) | null;
      onmessage: ((event: MessageEvent<unknown>) => unknown) | null;
      onerror: ((event: never) => unknown) | null;
      onclose: ((event: CloseEvent) => unknown) | null;
      send(data: string): void;
      close(code?: number, reason?: string): void;
    } | null = null;
    const telemetryStore = createTelemetryStore({ now: () => simNowMs });
    const skgStore = createSkgStore({
      now: () => simNowMs,
      createSocket: () => {
        ws2Socket = {
          readyState: 1,
          onopen: null,
          onmessage: null,
          onerror: null,
          onclose: null,
          send: () => undefined,
          close: () => undefined,
        };
        return ws2Socket;
      },
    });
    skgStore.connect("ws://127.0.0.1:8080/ws/graph");
    ws2Socket!.onopen?.(new Event("open") as never);

    const viewportStore = createViewportStore({ skgStore });
    const adapter = new NedGridAdapter({ widthPx: 800, heightPx: 600 });
    const vehAId = "boxbotix.local.robotics.uxs.vehicle.1";
    const vehBId = "boxbotix.local.robotics.uxs.vehicle.2";
    const contactId = "ais.uscg.cop.contact.vessel.staggered-conflict";
    const sfLla: [number, number, number] = [37.7749, -122.4194, 25];
    const austinLla: [number, number, number] = [30.2672, -97.7431, 150];

    const makeVehicle = (
      id: string,
      sysid: number,
      ned: [number, number, number],
      lla: [number, number, number],
      observedMs: number,
      revision: number,
    ) => {
      const iso = new Date(observedMs).toISOString();
      return {
        id,
        kind: "uxs-vehicle" as const,
        title: `Veh-${sysid}`,
        org: "boxbotix",
        platform: "local",
        sysid,
        domain: "quadcopter" as const,
        authority: "companion-teleop" as const,
        companionMode: "teleop-velocity" as const,
        linkTier: "mesh-high-rate" as const,
        armed: true,
        healthFlags: {
          clock_sync_locked: true,
          teleop_deadman_clamped: false,
          crsf_link_healthy: true,
          companion_trunk_healthy: true,
        },
        sysStatus: {
          failsafes: 0,
          crcErrors: 0,
          loopDtUs: 1000,
          maxLoopDtUs: 1200,
          rttUs: 400,
        },
        attitudeQuaternion: [1, 0, 0, 0] as [number, number, number, number],
        attitudeRates: [0, 0, 0] as [number, number, number],
        ned,
        velocityNed: [0, 0, 0] as [number, number, number],
        headingDeg: 0,
        location: { latitude: lla[0], longitude: lla[1], altitude: lla[2] },
        hasAttitude: true,
        hasLocalPos: true,
        hasGlobalPos: true,
        hasHeading: true,
        poseObservedAt: iso,
        localPosObservedAt: iso,
        globalPosObservedAt: iso,
        poseIngestedAtMs: observedMs,
        poseIngestedLive: true,
        kvRevision: revision,
        updatedAt: iso,
        poisoned: false,
        poisonError: null,
        indexingProfile: "full",
        source: "mavlink_0xb1",
        observedAt: iso,
        receivedAt: iso,
        expiresAt: null,
        uncertaintyMeters: null,
        feedStatus: "live" as const,
        ingestedAtMs: observedMs,
        triples: [],
      };
    };

    const t0 = simNowMs;
    skgStore.entities.set(
      vehAId,
      makeVehicle(vehAId, 1, [0, 0, -2], sfLla, t0, 1),
    );
    skgStore.entities.set(contactId, {
      id: contactId,
      kind: "external-contact",
      kvRevision: 2,
      poisoned: false,
      poisonError: null,
      title: "Austin Marker",
      contactType: "ais",
      indexingProfile: null,
      source: "ais-bridge",
      observedAt: new Date(t0).toISOString(),
      receivedAt: new Date(t0).toISOString(),
      updatedAt: new Date(t0).toISOString(),
      expiresAt: null,
      feedStatus: "live",
      uncertaintyMeters: 5,
      location: {
        latitude: austinLla[0],
        longitude: austinLla[1],
        altitude: austinLla[2],
      },
      triples: [],
    });

    const host = document.createElement("div");
    document.body.appendChild(host);
    const app = mount(TacticalCanvas, {
      target: host,
      props: {
        telemetryStore,
        skgStore,
        viewportStore,
        adapter,
        now: () => simNowMs,
      },
    });
    flushSync();

    // 1. t=0: Vehicle A anchors SF reference.
    expect(adapter.getCamera().originLla).not.toBeNull();
    expect(
      host
        .querySelector('[data-testid="tactical-viewport"]')
        ?.getAttribute("data-origin-anchored"),
    ).toBe("true");

    // 2. t=+500ms: Conflicting Vehicle B arrives in Austin while A is still fresh -> clears originLla.
    simNowMs = t0 + 500;
    skgStore.entities.set(
      vehBId,
      makeVehicle(vehBId, 2, [0, 0, -2], austinLla, simNowMs, 3),
    );
    flushSync();

    expect(adapter.getCamera().originLla).toBeNull();
    expect(
      host
        .querySelector('[data-testid="tactical-viewport"]')
        ?.getAttribute("data-origin-anchored"),
    ).toBe("false");
    expect(adapter.project({ lla: austinLla }).visible).toBe(false);

    // 3. t=+2600ms: Vehicle A has expired (2600ms > 2500ms) while Vehicle B is still fresh (2100ms <= 2500ms), with NO new observations -> MUST stay withheld!
    simNowMs = t0 + 2_600;
    skgStore.entities.set(contactId, {
      ...(skgStore.entities.get(contactId)! as ExternalContactEntityState),
      kvRevision: 4,
    });
    flushSync();

    expect(adapter.getCamera().originLla).toBeNull();
    expect(
      host
        .querySelector('[data-testid="tactical-viewport"]')
        ?.getAttribute("data-origin-anchored"),
    ).toBe("false");
    expect(adapter.project({ lla: austinLla }).visible).toBe(false);

    // 4. t=+3200ms: Vehicle B also expires -> STILL withheld.
    simNowMs = t0 + 3_200;
    skgStore.entities.set(contactId, {
      ...(skgStore.entities.get(contactId)! as ExternalContactEntityState),
      kvRevision: 5,
    });
    flushSync();

    expect(adapter.getCamera().originLla).toBeNull();
    expect(
      host
        .querySelector('[data-testid="tactical-viewport"]')
        ?.getAttribute("data-origin-anchored"),
    ).toBe("false");

    // 5. t=+3500ms: New confirming observations arrive for Vehicle B (while A remains expired) -> restores geographic overlay in Austin!
    simNowMs = t0 + 3_500;
    skgStore.entities.set(
      vehBId,
      makeVehicle(vehBId, 2, [4, -2, -2], austinLla, simNowMs, 6),
    );
    flushSync();

    expect(adapter.getCamera().originLla).not.toBeNull();
    expect(
      host
        .querySelector('[data-testid="tactical-viewport"]')
        ?.getAttribute("data-origin-anchored"),
    ).toBe("true");
    expect(adapter.project({ lla: austinLla }).visible).toBe(true);

    unmount(app);
    skgStore.disconnect();
    host.remove();

    // Direct timestamp-free adapter probe: staggered conflict expiration without timestamps also preserves the hold until new observations arrive.
    const directAdapter = new NedGridAdapter({ widthPx: 800, heightPx: 600 });
    expect(
      anchorSharedFrameOrigin(directAdapter, [
        { ned: [0, 0, 0], lla: sfLla, stale: false },
      ]),
    ).toBe(true);
    expect(
      anchorSharedFrameOrigin(directAdapter, [
        { ned: [0, 0, 0], lla: sfLla, stale: false },
        { ned: [0, 0, 0], lla: austinLla, stale: false },
      ]),
    ).toBe(false);
    expect(
      anchorSharedFrameOrigin(directAdapter, [
        { ned: [0, 0, 0], lla: sfLla, stale: true },
        { ned: [0, 0, 0], lla: austinLla, stale: false },
      ]),
    ).toBe(false);
    expect(directAdapter.getCamera().originLla).toBeNull();
    const movedAustinLla = localNedToLla([2, 0, 0], austinLla)!;
    expect(
      anchorSharedFrameOrigin(directAdapter, [
        { ned: [0, 0, 0], lla: sfLla, stale: true },
        { ned: [2, 0, 0], lla: movedAustinLla, stale: false },
      ]),
    ).toBe(true);
    expect(directAdapter.getCamera().originLla).not.toBeNull();
  });

  // spec: cop-ui / Renderer-agnostic ViewportAdapter, surface-vs-safety-HUD layer split, and multi-domain tactical canvas
  it("requires the candidate supplying a reference to qualify individually in both [A, B] and [B, A] orders across initial placement, relocation, and recovery", () => {
    const t0 = Date.parse("2026-11-24T18:00:00.000Z");
    const sfBeaconLla: [number, number, number] = [37.7749, -122.4194, 25];
    const sfOriginLla = deriveNedOriginLla([0, 0, -2], sfBeaconLla)!;
    const austinBeaconLla: [number, number, number] = [30.2672, -97.7431, 150];
    const austinOriginLla = deriveNedOriginLla([0, 0, -2], austinBeaconLla)!;

    // Fixed camera 800x600 at 4 px/m (stationary sfBeaconLla projects to x = 400 when anchored at sfOriginLla).
    // Candidate A: no global observation timestamp (fails confirmation alone), shifted +30 m East (120 px at 4 px/m).
    // Candidate B: confirmed pair at t0 + 1000, shifted +10 m East (40 px at 4 px/m; 10 m <= 25 m from sfOriginLla, 20 m <= 25 m from A).
    const llaShifted30mEast = localNedToLla([0, 30, -2], sfOriginLla)!;
    const llaShifted15mEast = localNedToLla([0, 15, -2], sfOriginLla)!;
    const llaShifted10mEast = localNedToLla([0, 10, -2], sfOriginLla)!;
    const austinShifted20mEast = localNedToLla([0, 20, -2], austinOriginLla)!;

    const makeCandidateA = (tsMs: number) => ({
      ned: [0, 0, -2] as [number, number, number],
      lla: llaShifted30mEast,
      hasLocalPos: true,
      hasVerifiedFrameReference: true,
      stale: false,
      localPosObservedAtMs: tsMs,
      globalPosObservedAtMs: null,
    });
    const makeCandidateAMild = (tsMs: number) => ({
      ned: [0, 0, -2] as [number, number, number],
      lla: llaShifted15mEast,
      hasLocalPos: true,
      hasVerifiedFrameReference: true,
      stale: false,
      localPosObservedAtMs: tsMs,
      globalPosObservedAtMs: null,
    });
    const makeCandidateB = (tsMs: number) => ({
      ned: [0, 0, -2] as [number, number, number],
      lla: llaShifted10mEast,
      hasLocalPos: true,
      hasVerifiedFrameReference: true,
      stale: false,
      localPosObservedAtMs: tsMs,
      globalPosObservedAtMs: tsMs,
    });
    const makeCandidateC = (tsMs: number) => ({
      ned: [0, 0, -2] as [number, number, number],
      lla: austinBeaconLla,
      hasLocalPos: true,
      hasVerifiedFrameReference: true,
      stale: false,
      localPosObservedAtMs: tsMs,
      globalPosObservedAtMs: tsMs,
    });
    const makeCandidateAAustin = (tsMs: number) => ({
      ned: [0, 0, -2] as [number, number, number],
      lla: austinShifted20mEast,
      hasLocalPos: true,
      hasVerifiedFrameReference: true,
      stale: false,
      localPosObservedAtMs: tsMs,
      globalPosObservedAtMs: null,
    });

    // 1. INITIAL PLACEMENT: [A] withholds; [B], [A, B], and [B, A] all install B's origin (x = 360, never A's x = 280).
    const initAOnly = new NedGridAdapter({
      widthPx: 800,
      heightPx: 600,
      zoomPxPerM: 4,
    });
    expect(anchorSharedFrameOrigin(initAOnly, [makeCandidateA(t0)])).toBe(
      false,
    );
    expect(initAOnly.getCamera().originLla).toBeNull();

    const initBOnly = new NedGridAdapter({
      widthPx: 800,
      heightPx: 600,
      zoomPxPerM: 4,
    });
    expect(anchorSharedFrameOrigin(initBOnly, [makeCandidateB(t0)])).toBe(true);
    const expectedBProjX = initBOnly.project({ lla: sfBeaconLla }).x;
    expect(expectedBProjX).toBeCloseTo(360, 2);

    for (const pair of [
      [makeCandidateA(t0), makeCandidateB(t0)],
      [makeCandidateB(t0), makeCandidateA(t0)],
    ]) {
      const initPair = new NedGridAdapter({
        widthPx: 800,
        heightPx: 600,
        zoomPxPerM: 4,
      });
      expect(anchorSharedFrameOrigin(initPair, pair)).toBe(true);
      expect(initPair.getCamera().originLla).toEqual(
        initBOnly.getCamera().originLla,
      );
      expect(initPair.project({ lla: sfBeaconLla }).x).toBeCloseTo(
        expectedBProjX,
        6,
      );
    }

    // 2. RELOCATION (established sfOriginLla -> x = 400):
    // [A] alone withholds; [B] alone causes 0 px movement; [A, B] and [B, A] both withhold (never shifting 120 px or 40 px).
    const makeEstablishedAdapter = () => {
      const a = new NedGridAdapter({
        widthPx: 800,
        heightPx: 600,
        zoomPxPerM: 4,
      });
      expect(
        anchorSharedFrameOrigin(a, [
          {
            ned: [0, 0, -2],
            lla: sfBeaconLla,
            hasLocalPos: true,
            hasVerifiedFrameReference: true,
            stale: false,
            localPosObservedAtMs: t0,
            globalPosObservedAtMs: t0,
          },
        ]),
      ).toBe(true);
      expect(a.project({ lla: sfBeaconLla }).x).toBeCloseTo(400, 6);
      return a;
    };

    const relocAOnly = makeEstablishedAdapter();
    expect(
      anchorSharedFrameOrigin(relocAOnly, [makeCandidateA(t0 + 1_000)]),
    ).toBe(false);
    expect(relocAOnly.getCamera().originLla).toBeNull();

    const relocBOnly = makeEstablishedAdapter();
    expect(
      anchorSharedFrameOrigin(relocBOnly, [makeCandidateB(t0 + 1_000)]),
    ).toBe(true);
    expect(relocBOnly.project({ lla: sfBeaconLla }).x).toBeCloseTo(400, 6);

    for (const pair of [
      [makeCandidateA(t0 + 1_000), makeCandidateB(t0 + 1_000)],
      [makeCandidateB(t0 + 1_000), makeCandidateA(t0 + 1_000)],
    ]) {
      const relocPair = makeEstablishedAdapter();
      expect(anchorSharedFrameOrigin(relocPair, pair)).toBe(false);
      expect(relocPair.getCamera().originLla).toBeNull();
      expect(relocPair.project({ lla: sfBeaconLla }).visible).toBe(false);
    }

    // When both agree with established origin ([A_mild, B] and [B, A_mild]), both retain sfOriginLla (0 px movement).
    for (const pair of [
      [makeCandidateAMild(t0 + 1_000), makeCandidateB(t0 + 1_000)],
      [makeCandidateB(t0 + 1_000), makeCandidateAMild(t0 + 1_000)],
    ]) {
      const retainPair = makeEstablishedAdapter();
      expect(anchorSharedFrameOrigin(retainPair, pair)).toBe(true);
      expect(retainPair.project({ lla: sfBeaconLla }).x).toBeCloseTo(400, 6);
    }

    // When C confirms relocation to Austin alongside unconfirmed A_austin, both [A_austin, C] and [C, A_austin] install C's origin.
    for (const pair of [
      [makeCandidateAAustin(t0 + 1_000), makeCandidateC(t0 + 1_000)],
      [makeCandidateC(t0 + 1_000), makeCandidateAAustin(t0 + 1_000)],
    ]) {
      const austinPair = makeEstablishedAdapter();
      expect(anchorSharedFrameOrigin(austinPair, pair)).toBe(true);
      expect(austinPair.project({ lla: austinBeaconLla }).x).toBeCloseTo(
        400,
        4,
      );
    }

    // 3. RECOVERY (both divergence-hold and conflict-hold in both [A, B] and [B, A] orders):
    const makeDivergenceHeldAdapter = () => {
      const a = makeEstablishedAdapter();
      expect(anchorSharedFrameOrigin(a, [makeCandidateA(t0 + 500)])).toBe(
        false,
      );
      expect(a.getCamera().originLla).toBeNull();
      return a;
    };

    for (const pair of [
      [makeCandidateA(t0 + 1_500), makeCandidateB(t0 + 1_500)],
      [makeCandidateB(t0 + 1_500), makeCandidateA(t0 + 1_500)],
    ]) {
      const heldPair = makeDivergenceHeldAdapter();
      expect(anchorSharedFrameOrigin(heldPair, pair)).toBe(false);
      expect(heldPair.getCamera().originLla).toBeNull();
    }

    for (const pair of [
      [makeCandidateAMild(t0 + 1_500), makeCandidateB(t0 + 1_500)],
      [makeCandidateB(t0 + 1_500), makeCandidateAMild(t0 + 1_500)],
    ]) {
      const restoredPair = makeDivergenceHeldAdapter();
      expect(anchorSharedFrameOrigin(restoredPair, pair)).toBe(true);
      expect(restoredPair.project({ lla: sfBeaconLla }).x).toBeCloseTo(400, 6);
    }

    for (const pair of [
      [makeCandidateAAustin(t0 + 1_500), makeCandidateC(t0 + 1_500)],
      [makeCandidateC(t0 + 1_500), makeCandidateAAustin(t0 + 1_500)],
    ]) {
      const relocatedHoldPair = makeDivergenceHeldAdapter();
      expect(anchorSharedFrameOrigin(relocatedHoldPair, pair)).toBe(true);
      expect(relocatedHoldPair.project({ lla: austinBeaconLla }).x).toBeCloseTo(
        400,
        4,
      );
    }

    const makeConflictHeldAdapter = () => {
      const a = new NedGridAdapter({
        widthPx: 800,
        heightPx: 600,
        zoomPxPerM: 4,
      });
      expect(
        anchorSharedFrameOrigin(a, [
          makeCandidateB(t0),
          makeCandidateC(t0 + 100),
        ]),
      ).toBe(false);
      expect(a.getCamera().originLla).toBeNull();
      return a;
    };

    for (const pair of [
      [makeCandidateA(t0 + 1_500), makeCandidateB(t0 + 1_500)],
      [makeCandidateB(t0 + 1_500), makeCandidateA(t0 + 1_500)],
    ]) {
      const recoveredConflict = makeConflictHeldAdapter();
      expect(anchorSharedFrameOrigin(recoveredConflict, pair)).toBe(true);
      expect(recoveredConflict.getCamera().originLla).toEqual(
        initBOnly.getCamera().originLla,
      );
      expect(recoveredConflict.project({ lla: sfBeaconLla }).x).toBeCloseTo(
        expectedBProjX,
        6,
      );
    }
  });

  // spec: cop-ui / Renderer-agnostic ViewportAdapter, surface-vs-safety-HUD layer split, and multi-domain tactical canvas
  it("enforces order-invariant multi-vehicle origin selection and individual candidate qualification across generated histories", () => {
    const rand = createXorShift32(0x6d32_32f1);
    const t0 = Date.parse("2026-11-24T19:00:00.000Z");
    const baseBeaconLla: [number, number, number] = [37.7749, -122.4194, 25];
    const baseOriginLla = deriveNedOriginLla([0, 0, -2], baseBeaconLla)!;
    const offsetsM = [0, 10, 24.5, 25.5, 30, 80, 250];

    let initialCount = 0;
    let retainedCount = 0;
    let relocatedCount = 0;
    let withheldCount = 0;
    let restoredPriorCount = 0;
    let recoveredConflictCount = 0;

    const genCandidate = (baseTsMs: number) => {
      const eastOffsetM = offsetsM[Math.floor(rand() * offsetsM.length)]!;
      const northOffsetM = (rand() - 0.5) * 8;
      const ned: [number, number, number] = [2, -3, -4];
      const targetOrigin = localNedToLla(
        [northOffsetM, eastOffsetM, 0],
        baseOriginLla,
      )!;
      const lla = localNedToLla(ned, targetOrigin)!;
      const tsKind = Math.floor(rand() * 5);
      let localPosObservedAtMs: number | null = baseTsMs;
      let globalPosObservedAtMs: number | null = baseTsMs;
      if (tsKind === 1) {
        globalPosObservedAtMs = null;
      } else if (tsKind === 2) {
        localPosObservedAtMs = null;
      } else if (tsKind === 3) {
        globalPosObservedAtMs = baseTsMs - 700; // > 500ms skew
      } else if (tsKind === 4) {
        localPosObservedAtMs = baseTsMs + Math.floor(rand() * 200);
      }
      return {
        ned,
        lla,
        hasLocalPos: true,
        hasVerifiedFrameReference: true,
        stale: false,
        localPosObservedAtMs,
        globalPosObservedAtMs,
      };
    };

    const prepareModeAdapter = (
      mode: "initial" | "established" | "divergence-hold" | "conflict-hold",
    ) => {
      const a = new NedGridAdapter({
        widthPx: 800,
        heightPx: 600,
        zoomPxPerM: 4,
      });
      if (mode === "initial") {
        return a;
      }
      anchorSharedFrameOrigin(a, [
        {
          ned: [0, 0, -2],
          lla: baseBeaconLla,
          hasLocalPos: true,
          hasVerifiedFrameReference: true,
          stale: false,
          localPosObservedAtMs: t0,
          globalPosObservedAtMs: t0,
        },
      ]);
      if (mode === "established") {
        return a;
      }
      if (mode === "divergence-hold") {
        anchorSharedFrameOrigin(a, [
          {
            ned: [0, 0, -2],
            lla: localNedToLla([0, 40, -2], baseOriginLla)!,
            hasLocalPos: true,
            hasVerifiedFrameReference: true,
            stale: false,
            localPosObservedAtMs: t0 + 200,
            globalPosObservedAtMs: null,
          },
        ]);
        return a;
      }
      anchorSharedFrameOrigin(a, [
        {
          ned: [0, 0, -2],
          lla: baseBeaconLla,
          hasLocalPos: true,
          hasVerifiedFrameReference: true,
          stale: false,
          localPosObservedAtMs: t0 + 200,
          globalPosObservedAtMs: t0 + 200,
        },
        {
          ned: [0, 0, -2],
          lla: localNedToLla([0, 200, -2], baseOriginLla)!,
          hasLocalPos: true,
          hasVerifiedFrameReference: true,
          stale: false,
          localPosObservedAtMs: t0 + 200,
          globalPosObservedAtMs: t0 + 200,
        },
      ]);
      return a;
    };

    const modes = [
      "initial",
      "established",
      "divergence-hold",
      "conflict-hold",
    ] as const;

    for (let trial = 0; trial < 240; trial += 1) {
      const mode = modes[trial % modes.length]!;
      const c1 = genCandidate(t0 + 1_000);
      const c2 = genCandidate(t0 + 1_000);

      const forwardAdapter = prepareModeAdapter(mode);
      const reverseAdapter = prepareModeAdapter(mode);
      const priorOrigin = forwardAdapter.getCamera().originLla;

      const anchoredForward = anchorSharedFrameOrigin(forwardAdapter, [c1, c2]);
      const anchoredReverse = anchorSharedFrameOrigin(reverseAdapter, [c2, c1]);

      expect(anchoredForward).toBe(anchoredReverse);
      expect(forwardAdapter.getCamera().originLla).toEqual(
        reverseAdapter.getCamera().originLla,
      );

      const nextOrigin = forwardAdapter.getCamera().originLla;
      if (nextOrigin === null) {
        withheldCount += 1;
        continue;
      }

      if (
        mode === "established" &&
        nextOrigin[0] === priorOrigin?.[0] &&
        nextOrigin[1] === priorOrigin?.[1]
      ) {
        retainedCount += 1;
        continue;
      }
      if (
        mode === "divergence-hold" &&
        Math.abs(nextOrigin[0] - baseOriginLla[0]) < 1e-9 &&
        Math.abs(nextOrigin[1] - baseOriginLla[1]) < 1e-9
      ) {
        restoredPriorCount += 1;
        continue;
      }

      // Any newly installed origin MUST come from a candidate that itself anchors that exact origin alone.
      const solo1 = prepareModeAdapter(mode);
      const solo2 = prepareModeAdapter(mode);
      const ok1 = anchorSharedFrameOrigin(solo1, [c1]);
      const ok2 = anchorSharedFrameOrigin(solo2, [c2]);
      const matchesSolo1 =
        ok1 &&
        JSON.stringify(solo1.getCamera().originLla) ===
          JSON.stringify(nextOrigin);
      const matchesSolo2 =
        ok2 &&
        JSON.stringify(solo2.getCamera().originLla) ===
          JSON.stringify(nextOrigin);
      expect(matchesSolo1 || matchesSolo2).toBe(true);

      if (mode === "initial") {
        initialCount += 1;
      } else if (mode === "established") {
        relocatedCount += 1;
      } else if (mode === "conflict-hold") {
        recoveredConflictCount += 1;
      }
    }

    expect(initialCount).toBeGreaterThan(0);
    expect(retainedCount).toBeGreaterThan(0);
    expect(relocatedCount).toBeGreaterThan(0);
    expect(withheldCount).toBeGreaterThan(0);
    expect(restoredPriorCount).toBeGreaterThan(0);
    expect(recoveredConflictCount).toBeGreaterThan(0);
  });

  // spec: cop-ui / Renderer-agnostic ViewportAdapter, surface-vs-safety-HUD layer split, and multi-domain tactical canvas
  it("clears conflict hold state when the entity set becomes empty or the WebSocket endpoint switches, accepting a replacement feed whose clock is one hour behind", () => {
    const t0 = Date.parse("2026-11-24T20:00:00.000Z");
    const oneHourBehindMs = t0 - 3_600_000;
    const sfLla: [number, number, number] = [37.7749, -122.4194, 25];
    const austinLla: [number, number, number] = [30.2672, -97.7431, 150];
    const seattleLla: [number, number, number] = [47.6062, -122.3321, 40];

    // 1. Direct adapter transition: conflict -> empty entity list -> replacement feed 1h behind matches a brand-new adapter.
    const reusedAdapter = new NedGridAdapter({
      widthPx: 800,
      heightPx: 600,
      zoomPxPerM: 4,
    });
    const freshAdapter = new NedGridAdapter({
      widthPx: 800,
      heightPx: 600,
      zoomPxPerM: 4,
    });

    expect(
      anchorSharedFrameOrigin(reusedAdapter, [
        {
          ned: [0, 0, -2],
          lla: sfLla,
          hasLocalPos: true,
          hasVerifiedFrameReference: true,
          stale: false,
          localPosObservedAtMs: t0,
          globalPosObservedAtMs: t0,
        },
        {
          ned: [0, 0, -2],
          lla: austinLla,
          hasLocalPos: true,
          hasVerifiedFrameReference: true,
          stale: false,
          localPosObservedAtMs: t0 + 500,
          globalPosObservedAtMs: t0 + 500,
        },
      ]),
    ).toBe(false);
    expect(reusedAdapter.getCamera().originLla).toBeNull();

    // Emptying the entity list clears session state even though originLla is already null.
    expect(anchorSharedFrameOrigin(reusedAdapter, [])).toBe(false);

    const replacementPose = {
      ned: [5, -3, -2] as [number, number, number],
      lla: seattleLla,
      hasLocalPos: true,
      hasVerifiedFrameReference: true,
      stale: false,
      localPosObservedAtMs: oneHourBehindMs,
      globalPosObservedAtMs: oneHourBehindMs,
    };
    expect(anchorSharedFrameOrigin(reusedAdapter, [replacementPose])).toBe(
      true,
    );
    expect(anchorSharedFrameOrigin(freshAdapter, [replacementPose])).toBe(true);
    expect(reusedAdapter.getCamera().originLla).toEqual(
      freshAdapter.getCamera().originLla,
    );

    // Explicit resetSharedFrameOrigin helper also clears an active conflict hold without requiring an intermediate empty tick.
    expect(
      anchorSharedFrameOrigin(reusedAdapter, [
        {
          ned: [0, 0, -2],
          lla: sfLla,
          hasLocalPos: true,
          hasVerifiedFrameReference: true,
          stale: false,
          localPosObservedAtMs: t0,
          globalPosObservedAtMs: t0,
        },
        {
          ned: [0, 0, -2],
          lla: austinLla,
          hasLocalPos: true,
          hasVerifiedFrameReference: true,
          stale: false,
          localPosObservedAtMs: t0,
          globalPosObservedAtMs: t0,
        },
      ]),
    ).toBe(false);
    resetSharedFrameOrigin(reusedAdapter);
    expect(anchorSharedFrameOrigin(reusedAdapter, [replacementPose])).toBe(
      true,
    );

    // 2. Mounted TacticalCanvas endpoint switch: conflict on host-a -> connect(host-b) with synchronous snapshot 1h behind before flushSync().
    let simNowMs = t0;
    const sockets: Array<{
      url: string;
      readyState: number;
      onopen: ((event: never) => unknown) | null;
      onmessage: ((event: MessageEvent<unknown>) => unknown) | null;
      onerror: ((event: never) => unknown) | null;
      onclose: ((event: CloseEvent) => unknown) | null;
      send(data: string): void;
      close(code?: number, reason?: string): void;
    }> = [];
    const telemetryStore = createTelemetryStore({ now: () => simNowMs });
    const skgStore = createSkgStore({
      url: "ws://host-a.local:8080/ws/graph",
      now: () => simNowMs,
      createSocket: (url) => {
        const sock = {
          url,
          readyState: 1,
          onopen: null,
          onmessage: null,
          onerror: null,
          onclose: null,
          send: () => undefined,
          close: () => undefined,
        };
        sockets.push(sock);
        return sock;
      },
    });
    skgStore.connect();
    sockets[0]!.onopen?.(new Event("open") as never);

    const buildWireVehicle = (
      id: string,
      lla: [number, number, number],
      observedMs: number,
    ) => {
      const iso = new Date(observedMs).toISOString();
      const triple = (
        predicate: string,
        object: unknown,
        context = "measured",
      ) => ({
        subject: id,
        predicate,
        object,
        source: "mavlink",
        timestamp: iso,
        confidence: 1,
        context,
      });
      return {
        id,
        updated_at: iso,
        triples: [
          triple("uxs.domain.kind", "quadcopter"),
          triple("uxs.authority.state", "companion-teleop"),
          triple("uxs.mode.companion", "teleop-velocity"),
          triple("uxs.link.tier", "mesh-high-rate"),
          triple("uxs.health.armed", true),
          triple("uxs.health.flags", {
            clock_sync_locked: true,
            teleop_deadman_clamped: false,
            crsf_link_healthy: true,
            companion_trunk_healthy: true,
          }),
          triple("uxs.health.failsafes", 0),
          triple("uxs.health.crcerrors", 0),
          triple("uxs.health.loopdtus", 1000),
          triple("uxs.health.maxloopdtus", 1200),
          triple("uxs.health.rttus", 400),
          triple("uxs.attitude.quaternion", [1, 0, 0, 0]),
          triple("uxs.attitude.rates", [0, 0, 0]),
          triple("uxs.kinematics.ned", [0, 0, -2]),
          triple("uxs.kinematics.vx", 0),
          triple("uxs.kinematics.vy", 0),
          triple("uxs.kinematics.vz", 0),
          triple("uxs.kinematics.headingdeg", 0),
          triple("geo.location.latitude", lla[0]),
          triple("geo.location.longitude", lla[1]),
          triple("geo.location.altitude", lla[2]),
        ],
      };
    };

    const viewportStore = createViewportStore({ skgStore });
    const mountedAdapter = new NedGridAdapter({ widthPx: 800, heightPx: 600 });
    const host = document.createElement("div");
    document.body.appendChild(host);
    const app = mount(TacticalCanvas, {
      target: host,
      props: {
        telemetryStore,
        skgStore,
        viewportStore,
        adapter: mountedAdapter,
        now: () => simNowMs,
      },
    });

    // Deliver conflict on host-a and sync.
    skgStore.ingestRawFrame(
      JSON.stringify({
        type: "put",
        bucket: "ENTITY_STATES",
        key: "boxbotix.local.robotics.uxs.vehicle.1",
        revision: 1,
        payload: buildWireVehicle(
          "boxbotix.local.robotics.uxs.vehicle.1",
          sfLla,
          t0,
        ),
      }),
    );
    skgStore.ingestRawFrame(
      JSON.stringify({
        type: "put",
        bucket: "ENTITY_STATES",
        key: "boxbotix.local.robotics.uxs.vehicle.2",
        revision: 2,
        payload: buildWireVehicle(
          "boxbotix.local.robotics.uxs.vehicle.2",
          austinLla,
          t0 + 500,
        ),
      }),
    );
    skgStore.ingestRawFrame(
      JSON.stringify({
        type: "synced",
        bucket: "ENTITY_STATES",
        revision: 2,
      }),
    );
    flushSync();

    expect(skgStore.vehicles.length).toBe(2);
    expect(mountedAdapter.getCamera().originLla).toBeNull();
    expect(
      host
        .querySelector('[data-testid="tactical-viewport"]')
        ?.getAttribute("data-origin-anchored"),
    ).toBe("false");

    // Synchronously switch to host-b AND deliver replacement synced + live update 1h behind BEFORE flushSync() runs.
    skgStore.connect("ws://host-b.local:8080/ws/graph");
    expect(sockets.length).toBe(2);
    sockets[1]!.onopen?.(new Event("open") as never);
    skgStore.ingestRawFrame(
      JSON.stringify({
        type: "synced",
        bucket: "ENTITY_STATES",
        revision: 1,
      }),
    );
    skgStore.ingestRawFrame(
      JSON.stringify({
        type: "put",
        bucket: "ENTITY_STATES",
        key: "boxbotix.remote.robotics.uxs.vehicle.7",
        revision: 2,
        payload: buildWireVehicle(
          "boxbotix.remote.robotics.uxs.vehicle.7",
          seattleLla,
          oneHourBehindMs,
        ),
      }),
    );
    flushSync();

    expect(skgStore.vehicles.length).toBe(1);
    expect(mountedAdapter.getCamera().originLla).not.toBeNull();
    expect(
      host
        .querySelector('[data-testid="tactical-viewport"]')
        ?.getAttribute("data-origin-anchored"),
    ).toBe("true");
    expect(mountedAdapter.project({ lla: seattleLla }).visible).toBe(true);

    unmount(app);
    skgStore.disconnect();
    host.remove();
  });

  // spec: cop-ui / Renderer-agnostic ViewportAdapter, surface-vs-safety-HUD layer split, and multi-domain tactical canvas
  it("preserves an active conflict hold when the same entities remain present in the store after timing out", () => {
    const t0 = Date.parse("2026-11-24T21:00:00.000Z");
    const oneHourBehindMs = t0 - 3_600_000;
    const sfLla: [number, number, number] = [37.7749, -122.4194, 25];
    const austinLla: [number, number, number] = [30.2672, -97.7431, 150];

    const adapter = new NedGridAdapter({
      widthPx: 800,
      heightPx: 600,
      zoomPxPerM: 4,
    });

    // 1. Supply conflict at t0 / t0 + 500.
    expect(
      anchorSharedFrameOrigin(adapter, [
        {
          ned: [0, 0, -2],
          lla: sfLla,
          hasLocalPos: true,
          hasVerifiedFrameReference: true,
          stale: false,
          localPosObservedAtMs: t0,
          globalPosObservedAtMs: t0,
        },
        {
          ned: [0, 0, -2],
          lla: austinLla,
          hasLocalPos: true,
          hasVerifiedFrameReference: true,
          stale: false,
          localPosObservedAtMs: t0 + 500,
          globalPosObservedAtMs: t0 + 500,
        },
      ]),
    ).toBe(false);
    expect(adapter.getCamera().originLla).toBeNull();

    // 2. Both entities time out (stale: true) while remaining present in the store -> hold MUST survive.
    expect(
      anchorSharedFrameOrigin(adapter, [
        {
          ned: [0, 0, -2],
          lla: sfLla,
          hasLocalPos: true,
          hasVerifiedFrameReference: true,
          stale: true,
          localPosObservedAtMs: t0,
          globalPosObservedAtMs: t0,
        },
        {
          ned: [0, 0, -2],
          lla: austinLla,
          hasLocalPos: true,
          hasVerifiedFrameReference: true,
          stale: true,
          localPosObservedAtMs: t0 + 500,
          globalPosObservedAtMs: t0 + 500,
        },
      ]),
    ).toBe(false);
    expect(adapter.getCamera().originLla).toBeNull();

    // 3. Replaying an older (1h behind) or unadvanced (t0 + 500) pair without clearing entities MUST remain withheld.
    expect(
      anchorSharedFrameOrigin(adapter, [
        {
          ned: [0, 0, -2],
          lla: sfLla,
          hasLocalPos: true,
          hasVerifiedFrameReference: true,
          stale: false,
          localPosObservedAtMs: oneHourBehindMs,
          globalPosObservedAtMs: oneHourBehindMs,
        },
      ]),
    ).toBe(false);
    expect(adapter.getCamera().originLla).toBeNull();

    expect(
      anchorSharedFrameOrigin(adapter, [
        {
          ned: [0, 0, -2],
          lla: austinLla,
          hasLocalPos: true,
          hasVerifiedFrameReference: true,
          stale: false,
          localPosObservedAtMs: t0 + 500,
          globalPosObservedAtMs: t0 + 500,
        },
      ]),
    ).toBe(false);
    expect(adapter.getCamera().originLla).toBeNull();

    // 4. Only a post-conflict confirming pair (> t0 + 500) lifts the preserved hold.
    expect(
      anchorSharedFrameOrigin(adapter, [
        {
          ned: [0, 0, -2],
          lla: austinLla,
          hasLocalPos: true,
          hasVerifiedFrameReference: true,
          stale: false,
          localPosObservedAtMs: t0 + 1_000,
          globalPosObservedAtMs: t0 + 1_000,
        },
      ]),
    ).toBe(true);
    expect(adapter.getCamera().originLla).not.toBeNull();
  });

  // spec: cop-ui / Full-stack headless browser COP end-to-end verification and visual legibility
  it("relocates concentric range-ring labels off northern arcs occluded by .viewport-toolbar across viewport geometries", () => {
    const toolbarBottomPx = 46;
    const minBaselineY = toolbarBottomPx + 16; // 62px

    // Exact boundary: northY = cy - rPx + 13 = 300 - 251 + 13 = 62 -> unobstructed north placement.
    const exactNorth = resolveRangeRingLabelPosition(
      400,
      300,
      251,
      800,
      600,
      toolbarBottomPx,
    );
    expect(exactNorth).toEqual({ x: 406, y: 62, placement: "north" });

    // 1px higher (northY = 61 < 62): northern arc enters toolbar band -> shifts to East axis.
    const shiftedEast = resolveRangeRingLabelPosition(
      400,
      300,
      252,
      800,
      600,
      toolbarBottomPx,
    );
    expect(shiftedEast).not.toBeNull();
    expect(shiftedEast!.placement).toBe("east");
    expect(shiftedEast!.y).toBeGreaterThanOrEqual(minBaselineY);

    // Tall portrait viewport where East overflows widthPx - 44 -> falls back to southeast or south.
    const portraitSe = resolveRangeRingLabelPosition(
      200,
      280,
      235,
      400,
      700,
      toolbarBottomPx,
    );
    expect(portraitSe).not.toBeNull();
    expect(["southeast", "south"]).toContain(portraitSe!.placement);
    expect(portraitSe!.y).toBeGreaterThanOrEqual(minBaselineY);

    // Corner-only outer ring exceeding north, east, southeast, and south bounds -> suppressed cleanly.
    const cornerOnly = resolveRangeRingLabelPosition(
      200,
      200,
      275,
      400,
      400,
      toolbarBottomPx,
    );
    expect(cornerOnly).toBeNull();
  });
});
