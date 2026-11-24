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
  resolveContactPalette,
  resolveDomainGlyphKind,
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
});
