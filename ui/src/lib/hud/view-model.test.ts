// SPDX-License-Identifier: MIT
// Copyright (c) 2026 boxbotix

import { flushSync, mount, unmount } from "svelte";
import { describe, expect, it, vi } from "vitest";
import AuthorityBanner from "../components/AuthorityBanner.svelte";
import TacticalCanvas from "../components/TacticalCanvas.svelte";
import VehicleHud from "../components/VehicleHud.svelte";
import {
  type ExternalContactEntityState,
  type SkgTriple,
  type VehicleEntityState,
  createSkgStore,
} from "../stores/skg.svelte";
import { createTelemetryStore } from "../stores/telemetry.svelte";
import { MockWebSocketRig } from "../stores/test-helpers";
import { createViewportStore } from "../stores/viewport.svelte";
import type { WebSocketLike } from "../stores/ws-client";
import { NedGridAdapter, resolveVehicleSafetyTone } from "../viewport/ned-grid";
import {
  SKG_VEHICLE_FRESH_MS,
  buildExternalContactSummary,
  buildHealthStrip,
  buildVehicleBannerView,
  computeFlightMetrics,
  evaluateVehicleSafetySeverity,
  formatArmedBadge,
  formatAuthorityBadge,
  formatLinkTierBadge,
  quaternionToEulerDeg,
  resolveLiveVehiclePose,
} from "./view-model";

function eulerRadToQuaternionWxyz(
  rollRad: number,
  pitchRad: number,
  yawRad: number,
): [number, number, number, number] {
  const cr = Math.cos(rollRad * 0.5);
  const sr = Math.sin(rollRad * 0.5);
  const cp = Math.cos(pitchRad * 0.5);
  const sp = Math.sin(pitchRad * 0.5);
  const cy = Math.cos(yawRad * 0.5);
  const sy = Math.sin(yawRad * 0.5);
  return [
    cr * cp * cy + sr * sp * sy,
    sr * cp * cy - cr * sp * sy,
    cr * sp * cy + sr * cp * sy,
    cr * cp * sy - sr * sp * cy,
  ];
}

function makeSampleVehicle(
  overrides: Partial<VehicleEntityState> = {},
): VehicleEntityState {
  return {
    id: "boxbotix.local.robotics.uxs.vehicle.1",
    kind: "uxs-vehicle",
    title: "Quad-1",
    org: "boxbotix",
    platform: "local",
    sysid: 1,
    domain: "quadcopter",
    authority: "companion-teleop",
    companionMode: "teleop-velocity",
    linkTier: "mesh-high-rate",
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
      maxLoopDtUs: 1350,
      rttUs: 420,
    },
    attitudeQuaternion: [1, 0, 0, 0],
    attitudeRates: [0, 0, 0],
    ned: [10, -4, -18],
    velocityNed: [3, 4, -1.5],
    headingDeg: 90,
    location: { latitude: 30.2672, longitude: -97.7431, altitude: 168 },
    kvRevision: 4,
    updatedAt: "2026-11-24T12:00:00Z",
    poisoned: false,
    poisonError: null,
    indexingProfile: "full",
    source: "mavlink-ingest",
    observedAt: "2026-11-24T12:00:00Z",
    receivedAt: "2026-11-24T12:00:00Z",
    expiresAt: null,
    uncertaintyMeters: null,
    feedStatus: "live",
    triples: [],
    ...overrides,
  };
}

function makeSampleExternalContact(
  overrides: Partial<ExternalContactEntityState> = {},
): ExternalContactEntityState {
  return {
    id: "boxbotix.local.tak.cot.contact.alpha1",
    kind: "external-contact",
    title: null,
    contactType: "a-f-G-U-C",
    location: { latitude: 30.268, longitude: -97.742, altitude: null },
    kvRevision: 2,
    updatedAt: null,
    poisoned: false,
    poisonError: null,
    indexingProfile: null,
    source: null,
    observedAt: null,
    receivedAt: null,
    expiresAt: null,
    uncertaintyMeters: null,
    feedStatus: "unknown",
    triples: [],
    ...overrides,
  };
}

describe("HUD attitude extraction, P1-P4 badges, and external-contact isolation", () => {
  // spec: cop-ui / Primary attitude HUD, 4-state ControlAuthority banner, and external contact UI isolation
  it("extracts ZYX roll/pitch/yaw without gimbal-lock NaN and maps P1-P4, LinkTier, armed, and MCU health severities", () => {
    // Generated non-singular ZYX Euler grid checked against independent synthesis oracle.
    let checked = 0;
    for (const rollDeg of [-65, -20, 0, 35, 75]) {
      for (const pitchDeg of [-70, -25, 0, 30, 70]) {
        for (const yawDeg of [0, 45, 135, 225, 359]) {
          const q = eulerRadToQuaternionWxyz(
            (rollDeg * Math.PI) / 180,
            (pitchDeg * Math.PI) / 180,
            (yawDeg * Math.PI) / 180,
          );
          const out = quaternionToEulerDeg(q);
          expect(out.rollDeg).toBeCloseTo(rollDeg, 6);
          expect(out.pitchDeg).toBeCloseTo(pitchDeg, 6);
          expect(out.yawDeg).toBeCloseTo(yawDeg, 6);
          checked += 1;
        }
      }
    }
    expect(checked).toBe(125);

    // Exact IEEE-754 ULP boundary quaternion where even after Math.hypot
    // normalization, 2*(w*y - z*x) === 1.0000000000000002 > 1.0 (would NaN without clamp).
    const plus90 = quaternionToEulerDeg([
      0.707106774550704, 0.00009687362871952016, 0.707106774550704,
      -0.00009687362871952016,
    ]);
    expect(Number.isNaN(plus90.pitchDeg)).toBe(false);
    expect(plus90.pitchDeg).toBeCloseTo(90, 5);

    const minus90 = quaternionToEulerDeg([
      0.707106774550704, -0.00009687362871952016, -0.707106774550704,
      -0.00009687362871952016,
    ]);
    expect(Number.isNaN(minus90.pitchDeg)).toBe(false);
    expect(minus90.pitchDeg).toBeCloseTo(-90, 5);

    // Flight metrics: groundspeed hypot(3,4)=5, climb = -vz, relAlt = -nedZ.
    const metrics = computeFlightMetrics([10, -4, -18], [3, 4, -1.5]);
    expect(metrics).toEqual({
      groundspeedMps: 5,
      climbRateMps: 1.5,
      altitudeRelM: 18,
    });

    // All 4 ControlAuthority states and 4 LinkTiers.
    expect(formatAuthorityBadge("direct-crsf-override")).toEqual({
      code: "P1",
      authority: "direct-crsf-override",
      label: "P1 DirectCrsfOverride",
      severity: "warning",
    });
    expect(formatAuthorityBadge("companion-teleop")).toEqual({
      code: "P2",
      authority: "companion-teleop",
      label: "P2 CompanionTeleop",
      severity: "nominal",
    });
    expect(formatAuthorityBadge("companion-autonomous")).toEqual({
      code: "P3",
      authority: "companion-autonomous",
      label: "P3 CompanionAutonomous",
      severity: "nominal",
    });
    expect(formatAuthorityBadge("mcu-failsafe")).toEqual({
      code: "P4",
      authority: "mcu-failsafe",
      label: "P4 McuFailsafe",
      severity: "error",
    });

    expect(formatLinkTierBadge("sitl-local").label).toBe("SITL");
    expect(formatLinkTierBadge("direct-bench").label).toBe("Bench");
    expect(formatLinkTierBadge("mesh-high-rate").label).toBe("HaLow 30Hz");
    expect(formatLinkTierBadge("mesh-supervisory").label).toBe("LoRa 1Hz");

    expect(formatArmedBadge(false)).toEqual({
      armed: false,
      label: "DISARMED",
      severity: "nominal",
    });
    expect(formatArmedBadge(true, false)).toEqual({
      armed: true,
      label: "ARMED",
      severity: "success",
    });
    expect(formatArmedBadge(true, true)).toEqual({
      armed: true,
      label: "ARMED",
      severity: "warning",
    });

    const faultStrip = buildHealthStrip(
      {
        clock_sync_locked: false,
        teleop_deadman_clamped: true,
        crsf_link_healthy: false,
        companion_trunk_healthy: false,
      },
      {
        failsafes: 2,
        crcErrors: 3,
        loopDtUs: 1050,
        maxLoopDtUs: 2400,
        rttUs: 950,
      },
    );
    const byKey = Object.fromEntries(faultStrip.map((c) => [c.key, c]));
    expect(byKey.crsf_link_healthy?.severity).toBe("error");
    expect(byKey.companion_trunk_healthy?.severity).toBe("error");
    expect(byKey.clock_sync_locked?.severity).toBe("warning");
    expect(byKey.teleop_deadman_clamped?.severity).toBe("warning");
    expect(byKey.failsafes?.severity).toBe("error");
    expect(byKey.crc_errors?.severity).toBe("warning");
    expect(byKey.loop_dt_us?.value).toBe("1050 us");
    expect(byKey.max_loop_dt_us?.value).toBe("2400 us");
    expect(byKey.rtt_us?.value).toBe("950 us");

    const bannerView = buildVehicleBannerView(makeSampleVehicle());
    expect(bannerView.authorityBadge.code).toBe("P2");
    expect(bannerView.armedBadge.severity).toBe("success");
    expect(bannerView.linkBadge.label).toBe("HaLow 30Hz");
  });

  // spec: cop-ui / Primary attitude HUD, 4-state ControlAuthority banner, and external contact UI isolation
  it("renders unknown-preserving external-contact provenance and omits all vehicle authority/arming affordances", () => {
    const contact = makeSampleExternalContact();
    const summary = buildExternalContactSummary(contact);
    expect(summary).toEqual({
      kind: "external-contact",
      entityId: "boxbotix.local.tak.cot.contact.alpha1",
      title: "boxbotix.local.tak.cot.contact.alpha1",
      contactType: "a-f-G-U-C",
      source: "unknown",
      observedAt: "unknown",
      receivedAt: "unknown",
      expiresAt: "unknown",
      uncertaintyMeters: null,
      uncertaintyLabel: "unknown",
      feedStatus: "unknown",
      location: { latitude: 30.268, longitude: -97.742, altitude: null },
    });
    expect("authorityBadge" in summary).toBe(false);
    expect("armedBadge" in summary).toBe(false);

    // Mount AuthorityBanner with a UXS vehicle first, then mount with an external contact
    // to verify zero authority/arming/link vehicle elements exist in the contact DOM.
    const vehicleHost = document.createElement("div");
    document.body.appendChild(vehicleHost);
    const vehicleApp = mount(AuthorityBanner, {
      target: vehicleHost,
      props: { entity: makeSampleVehicle() },
    });
    flushSync();
    expect(
      vehicleHost.querySelector('[data-testid="vehicle-authority-banner"]'),
    ).not.toBeNull();
    expect(
      vehicleHost.querySelector('[data-testid="authority-badge"]')?.textContent,
    ).toContain("P2 CompanionTeleop");
    unmount(vehicleApp);
    vehicleHost.remove();

    const contactHost = document.createElement("div");
    document.body.appendChild(contactHost);
    const contactApp = mount(AuthorityBanner, {
      target: contactHost,
      props: { entity: contact },
    });
    flushSync();
    expect(
      contactHost.querySelector('[data-testid="external-contact-banner"]'),
    ).not.toBeNull();
    expect(contactHost.textContent).toContain("EXTERNAL CONTACT");
    expect(contactHost.textContent).toContain("unknown");
    expect(
      contactHost.querySelector('[data-testid="vehicle-authority-banner"]'),
    ).toBeNull();
    expect(
      contactHost.querySelector('[data-testid="authority-badge"]'),
    ).toBeNull();
    expect(contactHost.querySelector('[data-testid="armed-badge"]')).toBeNull();
    expect(
      contactHost.querySelector('[data-testid="vehicle-health-strip"]'),
    ).toBeNull();
    unmount(contactApp);
    contactHost.remove();
  });

  // spec: cop-ui / Primary attitude HUD, 4-state ControlAuthority banner, and external contact UI isolation
  // spec: cop-ui / Renderer-agnostic ViewportAdapter, surface-vs-safety-HUD layer split, and multi-domain tactical canvas
  it("falls back from disconnected WS1 kinematics to newer WS2 SKG poses on HUD and canvas, then marks STALE when neither feed is current", () => {
    interface ControllableSocket extends WebSocketLike {
      open(): void;
      receive(raw: string): void;
      dropConnection(): void;
    }

    function makeControllableSocket(): ControllableSocket {
      let state = 0;
      const sock: ControllableSocket = {
        get readyState() {
          return state;
        },
        onopen: null,
        onmessage: null,
        onerror: null,
        onclose: null,
        send: () => undefined,
        close: () => {
          state = 3;
        },
        open: () => {
          state = 1;
          sock.onopen?.(new Event("open") as never);
        },
        receive: (raw: string) => {
          sock.onmessage?.({ data: raw } as MessageEvent<unknown>);
        },
        dropConnection: () => {
          state = 3;
          sock.onclose?.({ code: 1006 } as CloseEvent);
        },
      };
      return sock;
    }

    function buildVehicleSkgPutFrame(params: {
      entityId: string;
      revision: number;
      ned: [number, number, number];
      vel: [number, number, number];
      headingDeg: number;
      quat: [number, number, number, number];
      timestampIso: string;
    }): string {
      const mk = (predicate: string, object: unknown): SkgTriple => ({
        subject: params.entityId,
        predicate,
        object,
        source: "mavlink-ingest",
        timestamp: params.timestampIso,
        confidence: 1,
      });
      const triples: SkgTriple[] = [
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
        mk("uxs.attitude.quaternion", params.quat),
        mk("uxs.attitude.rates", [0, 0, 0]),
        mk("uxs.kinematics.ned", params.ned),
        mk("uxs.kinematics.vx", params.vel[0]),
        mk("uxs.kinematics.vy", params.vel[1]),
        mk("uxs.kinematics.vz", params.vel[2]),
        mk("uxs.kinematics.headingdeg", params.headingDeg),
        mk("geo.location.latitude", 30.2672),
        mk("geo.location.longitude", -97.7431),
        mk("geo.location.altitude", 150 - params.ned[2]),
      ];
      return JSON.stringify({
        type: "put",
        bucket: "ENTITY_STATES",
        key: params.entityId,
        revision: params.revision,
        payload: {
          id: params.entityId,
          updated_at: params.timestampIso,
          triples,
        },
      });
    }

    let nowMs = Date.parse("2026-11-24T12:00:00.000Z");
    const ws1Sockets: ControllableSocket[] = [];
    const ws2Sockets: ControllableSocket[] = [];

    const telemetryStore = createTelemetryStore({
      now: () => nowMs,
      initialBackoffMs: 60_000,
      maxBackoffMs: 60_000,
      createSocket: () => {
        const s = makeControllableSocket();
        ws1Sockets.push(s);
        return s;
      },
    });
    const skgStore = createSkgStore({
      now: () => nowMs,
      initialBackoffMs: 60_000,
      maxBackoffMs: 60_000,
      createSocket: () => {
        const s = makeControllableSocket();
        ws2Sockets.push(s);
        return s;
      },
    });

    const drawnTexts: string[] = [];
    const mockCtx = {
      clearRect: vi.fn(),
      save: vi.fn(),
      restore: vi.fn(),
      beginPath: vi.fn(),
      closePath: vi.fn(),
      rect: vi.fn(),
      clip: vi.fn(),
      translate: vi.fn(),
      rotate: vi.fn(),
      setTransform: vi.fn(),
      fillRect: vi.fn(),
      strokeRect: vi.fn(),
      moveTo: vi.fn(),
      lineTo: vi.fn(),
      arc: vi.fn(),
      fill: vi.fn(),
      stroke: vi.fn(),
      setLineDash: vi.fn(),
      fillText: vi.fn((text: string) => {
        drawnTexts.push(text);
      }),
      fillStyle: "",
      strokeStyle: "",
      lineWidth: 1,
      globalAlpha: 1,
      font: "",
      textAlign: "left",
    } as unknown as CanvasRenderingContext2D;

    const rafCallbacks: FrameRequestCallback[] = [];
    const getCtxSpy = vi
      .spyOn(HTMLCanvasElement.prototype, "getContext")
      .mockImplementation(((contextId: string) =>
        contextId === "2d"
          ? mockCtx
          : null) as typeof HTMLCanvasElement.prototype.getContext);
    const rafSpy = vi
      .spyOn(window, "requestAnimationFrame")
      .mockImplementation((cb: FrameRequestCallback) => {
        rafCallbacks.push(cb);
        return rafCallbacks.length;
      });
    const cafSpy = vi
      .spyOn(window, "cancelAnimationFrame")
      .mockImplementation(() => undefined);

    const flushAnimationTicks = (): void => {
      const pending = rafCallbacks.splice(0, rafCallbacks.length);
      for (const cb of pending) {
        cb(nowMs);
      }
      flushSync();
    };

    const entityId = "boxbotix.local.robotics.uxs.vehicle.1";
    const hudHost = document.createElement("div");
    const canvasHost = document.createElement("div");
    document.body.append(hudHost, canvasHost);

    telemetryStore.connect("ws://127.0.0.1:8080/ws/telemetry");
    skgStore.connect("ws://127.0.0.1:8080/ws/graph");
    ws1Sockets[0]!.open();
    ws2Sockets[0]!.open();

    // Seed initial SKG snapshot at t=0.
    ws2Sockets[0]!.receive(
      buildVehicleSkgPutFrame({
        entityId,
        revision: 1,
        ned: [0, 0, -5],
        vel: [0, 0, 0],
        headingDeg: 10,
        quat: eulerRadToQuaternionWxyz(0, 0, (10 * Math.PI) / 180),
        timestampIso: new Date(nowMs).toISOString(),
      }),
    );
    ws2Sockets[0]!.receive(
      JSON.stringify({ type: "synced", bucket: "ENTITY_STATES", revision: 1 }),
    );
    flushSync();

    const viewportStore = createViewportStore({ skgStore });
    viewportStore.selectEntity(entityId);
    viewportStore.tetherEntity(entityId);
    const adapter = new NedGridAdapter({ widthPx: 800, heightPx: 600 });

    let hudApp: Record<string, unknown> | null = null;
    let canvasApp: Record<string, unknown> | null = null;

    try {
      hudApp = mount(VehicleHud, {
        target: hudHost,
        props: {
          telemetryStore,
          get vehicle() {
            return skgStore.getCommandableVehicle(entityId);
          },
          get skgStatus() {
            return skgStore.status;
          },
          now: () => nowMs,
        },
      });
      canvasApp = mount(TacticalCanvas, {
        target: canvasHost,
        props: {
          telemetryStore,
          skgStore,
          viewportStore,
          adapter,
          now: () => nowMs,
        },
      });
      flushSync();
      flushAnimationTicks();

      const hudPanel = hudHost.querySelector('[data-testid="vehicle-hud"]');
      expect(hudHost.querySelector('[data-testid="hud-readout"]')).toBeNull();
      expect(hudPanel?.getAttribute("data-pose-source")).toBe("skg");
      expect(hudPanel?.getAttribute("data-stale")).toBe("false");

      // Step 1A: 1st WS1-only frame arrives at +100ms while WS2 is quiet and HUD stays mounted -> WS1 wins on both HUD and canvas.
      nowMs += 100;
      ws1Sockets[0]!.receive(
        JSON.stringify({
          type: "data",
          subject: "bbops.telemetry.1",
          timestamp: nowMs,
          payload: {
            sysid: 1,
            domain: "quadcopter",
            seq: 1,
            msg_id: 32,
            quat_wxyz: eulerRadToQuaternionWxyz(0, 0, (45 * Math.PI) / 180),
            has_attitude: true,
            body_rates: [0, 0, 0],
            ned_xyz: [12, -6, -10],
            has_local_pos: true,
            ned_vel: [4, 0, 0],
            lat_lon_alt: [30.2673, -97.7431, 160],
            has_global_pos: true,
            hdg_deg: 45,
            timestamp: nowMs,
          },
        }),
      );
      drawnTexts.length = 0;
      flushAnimationTicks();

      expect(hudPanel?.getAttribute("data-pose-source")).toBe("ws1");
      expect(hudPanel?.getAttribute("data-stale")).toBe("false");
      expect(drawnTexts).toContain("045");
      expect(drawnTexts).toContain("R 0° P 0° Y 45°");
      expect(drawnTexts).toContain("GS 4.0m/s  VS +0.0m/s  ALT 10.0m");
      expect(adapter.getCamera().centerNed).toEqual([12, -6]);

      // Step 1B: 2nd WS1-only frame arrives at +150ms while WS2 remains quiet and HUD stays mounted.
      nowMs += 50;
      ws1Sockets[0]!.receive(
        JSON.stringify({
          type: "data",
          subject: "bbops.telemetry.1",
          timestamp: nowMs,
          payload: {
            sysid: 1,
            domain: "quadcopter",
            seq: 2,
            msg_id: 32,
            quat_wxyz: eulerRadToQuaternionWxyz(0, 0, (90 * Math.PI) / 180),
            has_attitude: true,
            body_rates: [0, 0, 0],
            ned_xyz: [18, -9, -14],
            has_local_pos: true,
            ned_vel: [7.5, 0, 0],
            lat_lon_alt: [30.2674, -97.7432, 164],
            has_global_pos: true,
            hdg_deg: 90,
            timestamp: nowMs,
          },
        }),
      );
      drawnTexts.length = 0;
      flushAnimationTicks();

      expect(hudPanel?.getAttribute("data-pose-source")).toBe("ws1");
      expect(drawnTexts).toContain("R 0° P 0° Y 90°");
      expect(drawnTexts).not.toContain("R 0° P 0° Y 45°");
      expect(drawnTexts).toContain("GS 7.5m/s  VS +0.0m/s  ALT 14.0m");
      expect(adapter.getCamera().centerNed).toEqual([18, -9]);

      // Step 2: WS1 disconnects while WS2 stays open and delivers 2 distinct newer poses (HUD still never remounted).
      nowMs += 250;
      ws1Sockets[0]!.dropConnection();
      expect(telemetryStore.status).toBe("reconnecting");

      nowMs += 250;
      ws2Sockets[0]!.receive(
        buildVehicleSkgPutFrame({
          entityId,
          revision: 2,
          ned: [30, 15, -22],
          vel: [5, 0, 0],
          headingDeg: 120,
          quat: eulerRadToQuaternionWxyz(0, 0, (120 * Math.PI) / 180),
          timestampIso: new Date(nowMs).toISOString(),
        }),
      );
      flushSync();
      drawnTexts.length = 0;
      flushAnimationTicks();

      expect(hudPanel?.getAttribute("data-pose-source")).toBe("skg");
      expect(hudPanel?.getAttribute("data-stale")).toBe("false");
      expect(drawnTexts).toContain("R 0° P 0° Y 120°");
      expect(drawnTexts).toContain("GS 5.0m/s  VS +0.0m/s  ALT 22.0m");
      expect(adapter.getCamera().centerNed).toEqual([30, 15]);

      nowMs += 250;
      ws2Sockets[0]!.receive(
        buildVehicleSkgPutFrame({
          entityId,
          revision: 3,
          ned: [55, -20, -34],
          vel: [6, 0, 0],
          headingDeg: 215,
          quat: eulerRadToQuaternionWxyz(0, 0, (215 * Math.PI) / 180),
          timestampIso: new Date(nowMs).toISOString(),
        }),
      );
      flushSync();
      drawnTexts.length = 0;
      flushAnimationTicks();

      expect(hudPanel?.getAttribute("data-pose-source")).toBe("skg");
      expect(hudPanel?.getAttribute("data-stale")).toBe("false");
      expect(drawnTexts).toContain("R 0° P 0° Y 215°");
      expect(drawnTexts).toContain("GS 6.0m/s  VS +0.0m/s  ALT 34.0m");
      expect(adapter.getCamera().centerNed).toEqual([55, -20]);

      // Step 3: Neither feed updates past SKG_VEHICLE_FRESH_MS (and WS2 drops) -> STALE indicated on both HUD and canvas.
      nowMs += SKG_VEHICLE_FRESH_MS + 500;
      ws2Sockets[0]!.dropConnection();
      flushSync();
      drawnTexts.length = 0;
      flushAnimationTicks();

      const latestVehicle = skgStore.getCommandableVehicle(entityId)!;
      const stalePose = resolveLiveVehiclePose(latestVehicle, {
        fast: telemetryStore.getKinematics(1),
        telemetryStatus: telemetryStore.status,
        skgStatus: skgStore.status,
        nowMs,
      });
      expect(stalePose.stale).toBe(true);
      expect(stalePose.ned).toEqual([55, -20, -34]);
      expect(
        hudHost.querySelector('[data-testid="hud-stale-badge"]')?.textContent,
      ).toBe("STALE");
      expect(hudPanel?.getAttribute("data-stale")).toBe("true");
      expect(drawnTexts).toContain("STALE TELEMETRY");
      expect(drawnTexts).toContain("#1 quadcopter · STALE");
      expect(
        canvasHost
          .querySelector('[data-testid="tactical-viewport"]')
          ?.getAttribute("data-stale-vehicles"),
      ).toContain(entityId);
    } finally {
      if (hudApp) {
        unmount(hudApp);
      }
      if (canvasApp) {
        unmount(canvasApp);
      }
      telemetryStore.disconnect();
      skgStore.disconnect();
      hudHost.remove();
      canvasHost.remove();
      getCtxSpy.mockRestore();
      rafSpy.mockRestore();
      cafSpy.mockRestore();
    }
  });

  // spec: cop-ui / Primary attitude HUD, 4-state ControlAuthority banner, and external contact UI isolation
  // spec: cop-ui / Renderer-agnostic ViewportAdapter, surface-vs-safety-HUD layer split, and multi-domain tactical canvas
  it("preserves #33 GLOBAL_POSITION_INT heading before #31 ATTITUDE_QUATERNION arrives and clears NO ATT while keeping HUD mounted", () => {
    let nowMs = Date.parse("2026-11-24T12:05:00.000Z");
    let ws1Socket: WebSocketLike | null = null;
    const telemetryStore = createTelemetryStore({
      now: () => nowMs,
      createSocket: () => {
        const s: WebSocketLike = {
          readyState: 1,
          onopen: null,
          onmessage: null,
          onerror: null,
          onclose: null,
          send: () => undefined,
          close: () => undefined,
        };
        ws1Socket = s;
        return s;
      },
    });
    telemetryStore.connect("ws://127.0.0.1:8080/ws/telemetry");
    ws1Socket!.onopen?.(new Event("open") as never);

    const skgStore = createSkgStore({ now: () => nowMs });

    const preAttitudeVehicle = makeSampleVehicle({
      attitudeQuaternion: [0, 0, 0, 0],
      hasAttitude: false,
      headingDeg: 90,
      ned: [0, 0, -15],
      velocityNed: [0, 5, 0],
      observedAt: new Date(nowMs).toISOString(),
      poseObservedAt: new Date(nowMs).toISOString(),
      ingestedAtMs: nowMs,
      poseIngestedAtMs: nowMs,
    });
    skgStore.entities.set(preAttitudeVehicle.id, preAttitudeVehicle);

    // Even a legacy [1, 0, 0, 0] SKG snapshot whose heading is 90 deg must be flagged unmeasured.
    const legacyIdentityPose = resolveLiveVehiclePose(
      makeSampleVehicle({
        attitudeQuaternion: [1, 0, 0, 0],
        headingDeg: 90,
        observedAt: new Date(nowMs).toISOString(),
        ingestedAtMs: nowMs,
      }),
      { nowMs },
    );
    expect(legacyIdentityPose.hasAttitude).toBe(false);
    expect(legacyIdentityPose.headingDeg).toBe(90);

    // Ingest WS1 #33 GLOBAL_POSITION_INT frame before any #31 ATTITUDE_QUATERNION.
    telemetryStore.ingestRawFrame(
      JSON.stringify({
        type: "data",
        subject: "bbops.telemetry.1",
        timestamp: nowMs,
        payload: {
          sysid: 1,
          domain: "quadcopter",
          seq: 1,
          msg_id: 33,
          quat_wxyz: [0, 0, 0, 0],
          has_attitude: false,
          body_rates: [0, 0, 0],
          ned_xyz: [0, 0, -15],
          has_local_pos: false,
          ned_vel: [0, 5, 0],
          lat_lon_alt: [30.2672, -97.7431, 165],
          has_global_pos: true,
          hdg_deg: 90,
          timestamp: nowMs,
        },
      }),
    );

    const prePose = resolveLiveVehiclePose(preAttitudeVehicle, {
      fast: telemetryStore.getKinematics(1),
      telemetryStatus: telemetryStore.status,
      skgStatus: "connected",
      nowMs,
    });
    expect(prePose.hasAttitude).toBe(false);
    expect(prePose.headingDeg).toBe(90);
    expect(prePose.headingRad).toBeCloseTo(Math.PI / 2, 9);

    const drawnTexts: Array<{ text: string; x: number; y: number }> = [];
    const lineSegments: Array<{
      x0: number;
      y0: number;
      x1: number;
      y1: number;
    }> = [];
    let lastMoveX = 0;
    let lastMoveY = 0;
    const mockCtx = {
      clearRect: vi.fn(),
      save: vi.fn(),
      restore: vi.fn(),
      beginPath: vi.fn(),
      closePath: vi.fn(),
      rect: vi.fn(),
      clip: vi.fn(),
      translate: vi.fn(),
      rotate: vi.fn(),
      setTransform: vi.fn(),
      fillRect: vi.fn(),
      strokeRect: vi.fn(),
      moveTo: vi.fn((x: number, y: number) => {
        lastMoveX = x;
        lastMoveY = y;
      }),
      lineTo: vi.fn((x: number, y: number) => {
        lineSegments.push({ x0: lastMoveX, y0: lastMoveY, x1: x, y1: y });
        lastMoveX = x;
        lastMoveY = y;
      }),
      arc: vi.fn(),
      fill: vi.fn(),
      stroke: vi.fn(),
      setLineDash: vi.fn(),
      fillText: vi.fn((text: string, x: number, y: number) => {
        drawnTexts.push({ text, x, y });
      }),
      fillStyle: "",
      strokeStyle: "",
      lineWidth: 1,
      globalAlpha: 1,
      font: "",
      textAlign: "left",
    } as unknown as CanvasRenderingContext2D;

    const rafCallbacks: FrameRequestCallback[] = [];
    const getCtxSpy = vi
      .spyOn(HTMLCanvasElement.prototype, "getContext")
      .mockImplementation(((contextId: string) =>
        contextId === "2d"
          ? mockCtx
          : null) as typeof HTMLCanvasElement.prototype.getContext);
    const rafSpy = vi
      .spyOn(window, "requestAnimationFrame")
      .mockImplementation((cb: FrameRequestCallback) => {
        rafCallbacks.push(cb);
        return rafCallbacks.length;
      });
    const cafSpy = vi
      .spyOn(window, "cancelAnimationFrame")
      .mockImplementation(() => undefined);

    const hudHost = document.createElement("div");
    const canvasHost = document.createElement("div");
    document.body.append(hudHost, canvasHost);
    const adapter = new NedGridAdapter({ widthPx: 800, heightPx: 600 });
    const viewportStore = createViewportStore({ skgStore });

    let hudApp: Record<string, unknown> | null = null;
    let canvasApp: Record<string, unknown> | null = null;

    try {
      hudApp = mount(VehicleHud, {
        target: hudHost,
        props: {
          telemetryStore,
          get vehicle() {
            return skgStore.getCommandableVehicle(preAttitudeVehicle.id);
          },
          skgStatus: "connected",
          now: () => nowMs,
        },
      });
      canvasApp = mount(TacticalCanvas, {
        target: canvasHost,
        props: {
          telemetryStore,
          skgStore,
          viewportStore,
          adapter,
          now: () => nowMs,
        },
      });
      flushSync();
      for (const cb of rafCallbacks.splice(0, rafCallbacks.length)) {
        cb(nowMs);
      }
      flushSync();

      const hudPanel = hudHost.querySelector('[data-testid="vehicle-hud"]');
      // Pre-#31 assertions: 90 deg East heading honored; attitude marked unavailable.
      expect(
        hudHost.querySelector('[data-testid="hud-attitude-unavailable"]')
          ?.textContent,
      ).toContain("NO ATT");
      expect(hudPanel?.getAttribute("data-attitude")).toBe("unavailable");
      expect(drawnTexts.some((t) => t.text === "ATTITUDE UNAVAILABLE")).toBe(
        true,
      );
      expect(drawnTexts.some((t) => t.text === "R -- P -- HDG 090°")).toBe(
        true,
      );
      expect(drawnTexts.some((t) => t.text.includes("Y 0°"))).toBe(false);
      // East ("E") tick sits at center lubber bug (cx = 150 on default 300px HUD canvas).
      const eastTick = drawnTexts.find((t) => t.text === "E");
      expect(eastTick?.x).toBeCloseTo(150, 5);
      // TacticalCanvas bow vector from center (320, 210) on 640x420 fallback points +28px East (+X), not North (-Y).
      expect(
        lineSegments.some(
          (seg) =>
            Math.abs(seg.x0 - 320) < 1e-3 &&
            Math.abs(seg.y0 - 210) < 1e-3 &&
            Math.abs(seg.x1 - 348) < 1e-3 &&
            Math.abs(seg.y1 - 210) < 1e-3,
        ),
      ).toBe(true);

      // Deliver genuine #31 ATTITUDE_QUATERNION on WS1 ONLY while WS2 stays untouched and VehicleHud stays mounted!
      nowMs += 50;
      telemetryStore.ingestRawFrame(
        JSON.stringify({
          type: "data",
          subject: "bbops.telemetry.1",
          timestamp: nowMs,
          payload: {
            sysid: 1,
            domain: "quadcopter",
            seq: 2,
            msg_id: 31,
            quat_wxyz: [1, 0, 0, 0],
            has_attitude: true,
            body_rates: [0, 0, 0],
            ned_xyz: [0, 0, -15],
            has_local_pos: true,
            ned_vel: [5, 0, 0],
            lat_lon_alt: [30.2672, -97.7431, 165],
            has_global_pos: true,
            hdg_deg: 0,
            timestamp: nowMs,
          },
        }),
      );
      drawnTexts.length = 0;
      for (const cb of rafCallbacks.splice(0, rafCallbacks.length)) {
        cb(nowMs);
      }
      flushSync();

      expect(
        hudHost.querySelector('[data-testid="hud-attitude-unavailable"]'),
      ).toBeNull();
      expect(hudPanel?.getAttribute("data-attitude")).toBe("measured");
      expect(hudPanel?.getAttribute("data-pose-source")).toBe("ws1");
      expect(drawnTexts.some((t) => t.text === "R 0° P 0° Y 0°")).toBe(true);
      const northTick = drawnTexts.find((t) => t.text === "N");
      expect(northTick?.x).toBeCloseTo(150, 5);
    } finally {
      if (hudApp) {
        unmount(hudApp);
      }
      if (canvasApp) {
        unmount(canvasApp);
      }
      telemetryStore.disconnect();
      hudHost.remove();
      canvasHost.remove();
      getCtxSpy.mockRestore();
      rafSpy.mockRestore();
      cafSpy.mockRestore();
    }
  });

  // spec: cop-ui / Primary attitude HUD, 4-state ControlAuthority banner, and external contact UI isolation
  it("flags replayed old SKG snapshots stale, preserves newer cached WS1 pose across reconnects, accepts genuine new SKG pose, and ignores health-only updates for pose freshness", () => {
    const entityId = "boxbotix.local.robotics.uxs.vehicle.1";
    let nowMs = Date.parse("2026-11-24T12:00:00.000Z");
    const skgStore = createSkgStore({ now: () => nowMs });

    const makePut = (
      revision: number,
      poseIso: string,
      healthIso: string,
      ned: [number, number, number],
      headingDeg: number,
      crcErrors = 0,
    ): string => {
      const mk = (
        predicate: string,
        object: unknown,
        timestamp: string,
      ): SkgTriple => ({
        subject: entityId,
        predicate,
        object,
        source: "bbops-mavlink-ingest",
        timestamp,
        confidence: 1,
        ...(predicate.startsWith("uxs.kinematics.") ||
        predicate.startsWith("uxs.attitude.") ||
        predicate.startsWith("geo.location.")
          ? { context: "measured" }
          : {}),
      });
      return JSON.stringify({
        type: "put",
        bucket: "ENTITY_STATES",
        key: entityId,
        revision,
        payload: {
          id: entityId,
          updated_at: healthIso,
          triples: [
            mk("uxs.domain.kind", "quadcopter", healthIso),
            mk("uxs.authority.state", "companion-teleop", healthIso),
            mk("uxs.mode.companion", "teleop-velocity", healthIso),
            mk("uxs.link.tier", "mesh-high-rate", healthIso),
            mk("uxs.health.armed", true, healthIso),
            mk(
              "uxs.health.flags",
              {
                clock_sync_locked: true,
                teleop_deadman_clamped: false,
                crsf_link_healthy: true,
                companion_trunk_healthy: true,
              },
              healthIso,
            ),
            mk("uxs.health.failsafes", 0, healthIso),
            mk("uxs.health.crcerrors", crcErrors, healthIso),
            mk("uxs.health.loopdtus", 1000, healthIso),
            mk("uxs.health.maxloopdtus", 1200, healthIso),
            mk("uxs.health.rttus", 400, healthIso),
            mk(
              "uxs.attitude.quaternion",
              eulerRadToQuaternionWxyz(0, 0, (headingDeg * Math.PI) / 180),
              poseIso,
            ),
            mk("uxs.attitude.rates", [0, 0, 0], poseIso),
            mk("uxs.kinematics.ned", ned, poseIso),
            mk("uxs.kinematics.vx", 0, poseIso),
            mk("uxs.kinematics.vy", 0, poseIso),
            mk("uxs.kinematics.vz", 0, poseIso),
            mk("uxs.kinematics.headingdeg", headingDeg, poseIso),
            mk("geo.location.latitude", 30.2672, poseIso),
            mk("geo.location.longitude", -97.7431, poseIso),
            mk("geo.location.altitude", 160, poseIso),
          ],
        },
      });
    };

    // 1. Hour-old observation delivered right now on WS2 must evaluate stale: true.
    const hourOldIso = new Date(nowMs - 3_600_000).toISOString();
    skgStore.ingestRawFrame(
      makePut(1, hourOldIso, new Date(nowMs).toISOString(), [5, 5, -3], 45),
    );
    skgStore.ingestRawFrame(
      JSON.stringify({ type: "synced", bucket: "ENTITY_STATES", revision: 1 }),
    );
    const replayedOld = skgStore.getCommandableVehicle(entityId)!;
    const oldOnlyPose = resolveLiveVehiclePose(replayedOld, {
      skgStatus: "connected",
      nowMs,
    });
    expect(oldOnlyPose.source).toBe("skg");
    expect(oldOnlyPose.stale).toBe(true);

    // 2. With a newer cached WS1 observation and WS1 reconnecting, an older SKG replay MUST NOT displace WS1.
    const telemetryStore = createTelemetryStore({ now: () => nowMs });
    nowMs += 5_000;
    telemetryStore.ingestRawFrame(
      JSON.stringify({
        type: "data",
        subject: "bbops.telemetry.1",
        timestamp: nowMs,
        payload: {
          sysid: 1,
          domain: "quadcopter",
          seq: 8,
          msg_id: 32,
          quat_wxyz: eulerRadToQuaternionWxyz(0, 0, (270 * Math.PI) / 180),
          has_attitude: true,
          body_rates: [0, 0, 0],
          ned_xyz: [42, -18, -12],
          has_local_pos: true,
          ned_vel: [0, -3, 0],
          lat_lon_alt: [30.2675, -97.7435, 162],
          has_global_pos: true,
          hdg_deg: 270,
          timestamp: nowMs,
        },
      }),
    );

    // WS1 drops to reconnecting and WS2 replays an older t0+1000ms snapshot at t0+6200ms.
    const olderSkgIso = new Date(nowMs - 4_000).toISOString();
    nowMs += 1_200;
    skgStore.ingestRawFrame(
      makePut(2, olderSkgIso, olderSkgIso, [8, 8, -4], 60),
    );
    const afterOlderReplay = resolveLiveVehiclePose(
      skgStore.getCommandableVehicle(entityId)!,
      {
        fast: telemetryStore.getKinematics(1),
        telemetryStatus: "reconnecting",
        skgStatus: "connected",
        nowMs,
      },
    );
    expect(afterOlderReplay.source).toBe("ws1");
    expect(afterOlderReplay.ned).toEqual([42, -18, -12]);
    expect(afterOlderReplay.headingDeg).toBe(270);
    expect(afterOlderReplay.stale).toBe(true);

    // 3. Genuine new SKG pose arrives at t0+6500ms -> displaces older cached WS1 and is live!
    nowMs += 300;
    const genuineNewPoseIso = new Date(nowMs).toISOString();
    skgStore.ingestRawFrame(
      makePut(3, genuineNewPoseIso, genuineNewPoseIso, [60, -22, -15], 300),
    );
    const afterGenuinePose = resolveLiveVehiclePose(
      skgStore.getCommandableVehicle(entityId)!,
      {
        fast: telemetryStore.getKinematics(1),
        telemetryStatus: "reconnecting",
        skgStatus: "connected",
        nowMs: nowMs + 100,
      },
    );
    expect(afterGenuinePose.source).toBe("skg");
    expect(afterGenuinePose.stale).toBe(false);
    expect(afterGenuinePose.ned).toEqual([60, -22, -15]);
    expect(afterGenuinePose.headingDeg).toBe(300);

    // 4. Health-only update 3 seconds later (past SKG_VEHICLE_FRESH_MS) must NOT refresh pose age!
    nowMs += SKG_VEHICLE_FRESH_MS + 500;
    const healthOnlyIso = new Date(nowMs).toISOString();
    skgStore.ingestRawFrame(
      makePut(4, genuineNewPoseIso, healthOnlyIso, [60, -22, -15], 300, 4),
    );
    const vehAfterHealth = skgStore.getCommandableVehicle(entityId)!;
    expect(vehAfterHealth.sysStatus.crcErrors).toBe(4);
    const afterHealthUpdate = resolveLiveVehiclePose(vehAfterHealth, {
      fast: telemetryStore.getKinematics(1),
      telemetryStatus: "reconnecting",
      skgStatus: "connected",
      nowMs,
    });
    expect(afterHealthUpdate.source).toBe("skg");
    expect(afterHealthUpdate.stale).toBe(true);
  });

  // spec: cop-ui / Primary attitude HUD, 4-state ControlAuthority banner, and external contact UI isolation
  it("evaluates WS1 timeout on the local browser clock across both +60s and -60s server clock offsets while WS1 stays open and silent", () => {
    for (const skewMs of [60_000, -60_000]) {
      let browserNowMs = Date.parse("2026-11-24T12:20:00.000Z");
      let ws1Socket: WebSocketLike | null = null;
      const telemetryStore = createTelemetryStore({
        now: () => browserNowMs,
        createSocket: () => {
          const s: WebSocketLike = {
            readyState: 1,
            onopen: null,
            onmessage: null,
            onerror: null,
            onclose: null,
            send: () => undefined,
            close: () => undefined,
          };
          ws1Socket = s;
          return s;
        },
      });
      telemetryStore.connect("ws://127.0.0.1:8080/ws/telemetry");
      ws1Socket!.onopen?.(new Event("open") as never);
      expect(telemetryStore.status).toBe("connected");

      const initialServerMs = browserNowMs + skewMs - 5_000;
      const vehicle = makeSampleVehicle({
        observedAt: new Date(initialServerMs).toISOString(),
        poseObservedAt: new Date(initialServerMs).toISOString(),
      });

      const serverFrameMs = browserNowMs + skewMs;
      ws1Socket!.onmessage?.({
        data: JSON.stringify({
          type: "data",
          subject: "bbops.telemetry.1",
          timestamp: serverFrameMs,
          payload: {
            sysid: 1,
            domain: "quadcopter",
            seq: 1,
            msg_id: 32,
            quat_wxyz: [1, 0, 0, 0],
            has_attitude: true,
            body_rates: [0, 0, 0],
            ned_xyz: [20, -10, -8],
            has_local_pos: true,
            ned_vel: [2, 0, 0],
            lat_lon_alt: [30.2672, -97.7431, 158],
            has_global_pos: true,
            hdg_deg: 0,
            timestamp: serverFrameMs,
          },
        }),
      } as MessageEvent<unknown>);

      // At +200ms on the browser clock with WS2 disconnected, both +60s and -60s server skews are live.
      browserNowMs += 200;
      const liveCheck = resolveLiveVehiclePose(vehicle, {
        fast: telemetryStore.getKinematics(1),
        telemetryStatus: telemetryStore.status,
        skgStatus: "reconnecting",
        nowMs: browserNowMs,
      });
      expect(liveCheck.source, `skew=${skewMs}`).toBe("ws1");
      expect(liveCheck.stale, `skew=${skewMs}`).toBe(false);

      // Keep WS1 open ("connected") and silent for 10 seconds -> MUST time out to stale: true!
      browserNowMs += 9_800;
      expect(telemetryStore.status).toBe("connected");
      const silentCheck = resolveLiveVehiclePose(vehicle, {
        fast: telemetryStore.getKinematics(1),
        telemetryStatus: telemetryStore.status,
        skgStatus: "reconnecting",
        nowMs: browserNowMs,
      });
      expect(silentCheck.source, `skew=${skewMs}`).toBe("ws1");
      expect(silentCheck.stale, `skew=${skewMs}`).toBe(true);

      telemetryStore.disconnect();
    }
  });

  // spec: cop-ui / Primary attitude HUD, 4-state ControlAuthority banner, and external contact UI isolation
  it("downgrades AuthorityBanner ARMED badge from green success to warning across all degraded safety conditions", () => {
    const warningCases: Array<{
      name: string;
      overrides: Partial<VehicleEntityState>;
    }> = [
      {
        name: "P1 DirectCrsfOverride preemption",
        overrides: { authority: "direct-crsf-override" },
      },
      {
        name: "teleop deadman clamped",
        overrides: {
          healthFlags: {
            clock_sync_locked: true,
            teleop_deadman_clamped: true,
            crsf_link_healthy: true,
            companion_trunk_healthy: true,
          },
        },
      },
      {
        name: "clock sync unlocked",
        overrides: {
          healthFlags: {
            clock_sync_locked: false,
            teleop_deadman_clamped: false,
            crsf_link_healthy: true,
            companion_trunk_healthy: true,
          },
        },
      },
      {
        name: "MCU CRC errors > 0",
        overrides: {
          sysStatus: {
            failsafes: 0,
            crcErrors: 1,
            loopDtUs: 1000,
            maxLoopDtUs: 1350,
            rttUs: 420,
          },
        },
      },
    ];

    for (const testCase of warningCases) {
      const armedDegraded = makeSampleVehicle({
        armed: true,
        ...testCase.overrides,
      });
      expect(evaluateVehicleSafetySeverity(armedDegraded), testCase.name).toBe(
        "warning",
      );
      expect(resolveVehicleSafetyTone(armedDegraded).tone, testCase.name).toBe(
        "warning",
      );

      const banner = buildVehicleBannerView(armedDegraded);
      expect(banner.armedBadge.severity, testCase.name).toBe("warning");

      const host = document.createElement("div");
      document.body.appendChild(host);
      const app = mount(AuthorityBanner, {
        target: host,
        props: { entity: armedDegraded },
      });
      flushSync();
      const armedBadgeEl = host.querySelector('[data-testid="armed-badge"]');
      expect(armedBadgeEl?.classList.contains("severity-warning")).toBe(true);
      expect(armedBadgeEl?.classList.contains("severity-success")).toBe(false);
      unmount(app);
      host.remove();
    }
  });

  // spec: cop-ui / Primary attitude HUD, 4-state ControlAuthority banner, and external contact UI isolation
  it("keeps live WS2 fallback fresh across +60s and -60s host/browser clock skew when WS1 is unavailable, times out on WS2 silence, and preserves reconnect snapshot boundaries", () => {
    for (const skewMs of [60_000, -60_000]) {
      let browserNowMs = Date.parse("2026-11-24T12:40:00.000Z");
      let ws2Socket: WebSocketLike | null = null;
      const skgStore = createSkgStore({
        now: () => browserNowMs,
        initialBackoffMs: 60_000,
        maxBackoffMs: 60_000,
        createSocket: () => {
          const s: WebSocketLike = {
            readyState: 1,
            onopen: null,
            onmessage: null,
            onerror: null,
            onclose: null,
            send: () => undefined,
            close: () => undefined,
          };
          ws2Socket = s;
          return s;
        },
      });
      const entityId = "boxbotix.local.robotics.uxs.vehicle.1";
      const makeVehiclePut = (
        revision: number,
        poseIso: string,
        updatedIso: string,
        ned: [number, number, number],
        headingDeg: number,
        crcErrors = 0,
      ): string => {
        const mk = (
          predicate: string,
          object: unknown,
          ts = poseIso,
          context?: string,
        ): SkgTriple => ({
          subject: entityId,
          predicate,
          object,
          source: "mavlink_0xb1",
          timestamp: ts,
          confidence: 1,
          ...(context ? { context } : {}),
        });
        return JSON.stringify({
          type: "put",
          bucket: "ENTITY_STATES",
          key: entityId,
          revision,
          payload: {
            id: entityId,
            updated_at: updatedIso,
            triples: [
              mk("uxs.domain.kind", "quadcopter"),
              mk("uxs.authority.state", "companion-teleop", updatedIso),
              mk("uxs.mode.companion", "teleop-velocity", updatedIso),
              mk("uxs.link.tier", "mesh-high-rate", updatedIso),
              mk("uxs.health.armed", true, updatedIso),
              mk(
                "uxs.health.flags",
                {
                  clock_sync_locked: true,
                  teleop_deadman_clamped: false,
                  crsf_link_healthy: true,
                  companion_trunk_healthy: true,
                },
                updatedIso,
              ),
              mk("uxs.health.failsafes", 0, updatedIso),
              mk("uxs.health.crcerrors", crcErrors, updatedIso),
              mk("uxs.health.loopdtus", 1000, updatedIso),
              mk("uxs.health.maxloopdtus", 1200, updatedIso),
              mk("uxs.health.rttus", 400, updatedIso),
              mk("uxs.attitude.quaternion", [1, 0, 0, 0], poseIso, "measured"),
              mk("uxs.attitude.rates", [0, 0, 0]),
              mk("uxs.kinematics.ned", ned, poseIso, "measured"),
              mk("uxs.kinematics.vx", 1),
              mk("uxs.kinematics.vy", 0),
              mk("uxs.kinematics.vz", 0),
              mk("uxs.kinematics.headingdeg", headingDeg, poseIso, "measured"),
              mk("geo.location.latitude", 30.2672, poseIso, "measured"),
              mk("geo.location.longitude", -97.7431, poseIso, "measured"),
              mk("geo.location.altitude", 158, poseIso, "measured"),
            ],
          },
        });
      };

      skgStore.connect("ws://127.0.0.1:8080/ws/graph");
      ws2Socket!.onopen?.(new Event("open") as never);

      // 1. Pre-synced snapshot with +/-60s skewed producer timestamp is NOT blindly treated as live before a post-synced advance.
      const snapProducerIso = new Date(
        browserNowMs + skewMs - 5_000,
      ).toISOString();
      ws2Socket!.onmessage?.({
        data: makeVehiclePut(
          1,
          snapProducerIso,
          snapProducerIso,
          [0, 0, -5],
          15,
        ),
      } as MessageEvent<unknown>);
      ws2Socket!.onmessage?.({
        data: JSON.stringify({
          type: "synced",
          bucket: "ENTITY_STATES",
          revision: 1,
        }),
      } as MessageEvent<unknown>);

      const preLiveCheck = resolveLiveVehiclePose(
        skgStore.getCommandableVehicle(entityId)!,
        {
          telemetryStatus: "reconnecting",
          skgStatus: skgStore.status,
          nowMs: browserNowMs + 100,
        },
      );
      expect(preLiveCheck.stale, `pre-live skew=${skewMs}`).toBe(true);

      // 2. Post-synced live 4 Hz WS2 pose advance arrives under +/-60s clock skew -> MUST be live 100ms after receipt!
      browserNowMs += 250;
      const live1Iso = new Date(browserNowMs + skewMs).toISOString();
      ws2Socket!.onmessage?.({
        data: makeVehiclePut(2, live1Iso, live1Iso, [10, -4, -8], 45),
      } as MessageEvent<unknown>);

      browserNowMs += 100;
      const live1Check = resolveLiveVehiclePose(
        skgStore.getCommandableVehicle(entityId)!,
        {
          telemetryStatus: "reconnecting",
          skgStatus: skgStore.status,
          nowMs: browserNowMs,
        },
      );
      expect(live1Check.source, `live1 skew=${skewMs}`).toBe("skg");
      expect(live1Check.stale, `live1 skew=${skewMs}`).toBe(false);
      expect(live1Check.ned).toEqual([10, -4, -8]);

      // 3. Subsequent 4 Hz WS2 pose advance keeps fallback live.
      browserNowMs += 250;
      const live2Iso = new Date(browserNowMs + skewMs).toISOString();
      ws2Socket!.onmessage?.({
        data: makeVehiclePut(3, live2Iso, live2Iso, [14, -6, -9], 60),
      } as MessageEvent<unknown>);
      browserNowMs += 100;
      expect(
        resolveLiveVehiclePose(skgStore.getCommandableVehicle(entityId)!, {
          telemetryStatus: "reconnecting",
          skgStatus: skgStore.status,
          nowMs: browserNowMs,
        }).stale,
        `live2 skew=${skewMs}`,
      ).toBe(false);

      // 4. WS2 stays connected while pose goes silent past SKG_VEHICLE_FRESH_MS (with a health-only put in between) -> MUST time out!
      browserNowMs += SKG_VEHICLE_FRESH_MS + 500;
      const healthIso = new Date(browserNowMs + skewMs).toISOString();
      ws2Socket!.onmessage?.({
        data: makeVehiclePut(4, live2Iso, healthIso, [14, -6, -9], 60, 2),
      } as MessageEvent<unknown>);
      const afterSilence = resolveLiveVehiclePose(
        skgStore.getCommandableVehicle(entityId)!,
        {
          telemetryStatus: "reconnecting",
          skgStatus: skgStore.status,
          nowMs: browserNowMs,
        },
      );
      expect(afterSilence.source, `silent skew=${skewMs}`).toBe("skg");
      expect(afterSilence.stale, `silent skew=${skewMs}`).toBe(true);

      skgStore.disconnect();
    }

    // 5. Reconnect snapshot freshness check (0ms clock skew): newer current snapshot is live 100ms after synced, identical and older stay stale.
    let reconnectNowMs = Date.parse("2026-11-24T12:50:00.000Z");
    const rig = new MockWebSocketRig();
    const skgReconnect = createSkgStore({
      now: () => reconnectNowMs,
      createSocket: rig.createSocket,
    });
    skgReconnect.connect("ws://127.0.0.1:8080/ws/graph");
    rig.latest.serverOpen();

    const baseIso = new Date(reconnectNowMs).toISOString();
    const mkReconnectPut = (
      sysid: number,
      rev: number,
      poseIso: string,
      hdg: number,
    ): string => {
      const id = `boxbotix.local.robotics.uxs.vehicle.${sysid}`;
      const mk = (predicate: string, object: unknown): SkgTriple => ({
        subject: id,
        predicate,
        object,
        source: "mavlink_0xb1",
        timestamp: poseIso,
        confidence: 1,
        context: "measured",
      });
      return JSON.stringify({
        type: "put",
        bucket: "ENTITY_STATES",
        key: id,
        revision: rev,
        payload: {
          id,
          updated_at: new Date(reconnectNowMs).toISOString(),
          triples: [
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
            mk("uxs.attitude.quaternion", [1, 0, 0, 0]),
            mk("uxs.attitude.rates", [0, 0, 0]),
            mk("uxs.kinematics.ned", [5, 0, -5]),
            mk("uxs.kinematics.vx", 0),
            mk("uxs.kinematics.vy", 0),
            mk("uxs.kinematics.vz", 0),
            mk("uxs.kinematics.headingdeg", hdg),
            mk("geo.location.latitude", 30.2672),
            mk("geo.location.longitude", -97.7431),
            mk("geo.location.altitude", 155),
          ],
        },
      });
    };

    rig.latest.serverMessage(mkReconnectPut(1, 1, baseIso, 10));
    rig.latest.serverMessage(mkReconnectPut(2, 2, baseIso, 20));
    rig.latest.serverMessage(mkReconnectPut(3, 3, baseIso, 30));
    rig.latest.serverMessage(
      JSON.stringify({ type: "synced", bucket: "ENTITY_STATES", revision: 3 }),
    );

    reconnectNowMs += 500;
    const liveBeforeDropIso = new Date(reconnectNowMs).toISOString();
    rig.latest.serverMessage(mkReconnectPut(1, 4, liveBeforeDropIso, 15));
    rig.latest.serverMessage(mkReconnectPut(2, 5, liveBeforeDropIso, 25));
    rig.latest.serverMessage(mkReconnectPut(3, 6, liveBeforeDropIso, 35));

    // Drop WS2 for 10 seconds, then reconnect.
    reconnectNowMs += 10_000;
    vi.useFakeTimers();
    try {
      rig.latest.serverDrop(1006);
      vi.advanceTimersByTime(500);
    } finally {
      vi.useRealTimers();
    }
    reconnectNowMs += 500;
    rig.latest.serverOpen();

    const newerReconnectIso = new Date(reconnectNowMs - 100).toISOString();
    const olderReconnectIso = new Date(reconnectNowMs - 12_000).toISOString();
    rig.latest.serverMessage(mkReconnectPut(1, 10, newerReconnectIso, 75));
    rig.latest.serverMessage(mkReconnectPut(2, 11, liveBeforeDropIso, 25));
    rig.latest.serverMessage(mkReconnectPut(3, 12, olderReconnectIso, 5));
    rig.latest.serverMessage(
      JSON.stringify({ type: "synced", bucket: "ENTITY_STATES", revision: 12 }),
    );

    reconnectNowMs += 100;
    const v1Pose = resolveLiveVehiclePose(
      skgReconnect.getCommandableVehicle(
        "boxbotix.local.robotics.uxs.vehicle.1",
      )!,
      {
        telemetryStatus: "reconnecting",
        skgStatus: skgReconnect.status,
        nowMs: reconnectNowMs,
      },
    );
    const v2Pose = resolveLiveVehiclePose(
      skgReconnect.getCommandableVehicle(
        "boxbotix.local.robotics.uxs.vehicle.2",
      )!,
      {
        telemetryStatus: "reconnecting",
        skgStatus: skgReconnect.status,
        nowMs: reconnectNowMs,
      },
    );
    const v3Pose = resolveLiveVehiclePose(
      skgReconnect.getCommandableVehicle(
        "boxbotix.local.robotics.uxs.vehicle.3",
      )!,
      {
        telemetryStatus: "reconnecting",
        skgStatus: skgReconnect.status,
        nowMs: reconnectNowMs,
      },
    );

    expect(v1Pose.stale).toBe(false);
    expect(v1Pose.headingDeg).toBe(75);
    expect(v2Pose.stale).toBe(true);
    expect(v3Pose.stale).toBe(true);

    skgReconnect.disconnect();
  });

  // spec: cop-ui / Primary attitude HUD, 4-state ControlAuthority banner, and external contact UI isolation
  it("renders NO HDG and HDG UNAVAILABLE when #33 heading is unknown (UINT16_MAX) and renders N / HDG 000° when 0° North is genuinely measured across WS1 and WS2", () => {
    let nowMs = Date.parse("2026-11-24T13:00:00.000Z");
    let ws1Socket: WebSocketLike | null = null;
    const telemetryStore = createTelemetryStore({
      now: () => nowMs,
      createSocket: () => {
        const s: WebSocketLike = {
          readyState: 1,
          onopen: null,
          onmessage: null,
          onerror: null,
          onclose: null,
          send: () => undefined,
          close: () => undefined,
        };
        ws1Socket = s;
        return s;
      },
    });
    const skgStore = createSkgStore({ now: () => nowMs });
    telemetryStore.connect("ws://127.0.0.1:8080/ws/telemetry");
    ws1Socket!.onopen?.(new Event("open") as never);

    const entityId = "boxbotix.local.robotics.uxs.vehicle.1";
    const makeHeadingSkgPut = (
      revision: number,
      hasMeasuredHeading: boolean,
      headingDeg: number,
      tsIso: string,
    ): string => {
      const mk = (
        predicate: string,
        object: unknown,
        opts: { confidence?: number; context?: string; ts?: string } = {},
      ): SkgTriple => ({
        subject: entityId,
        predicate,
        object,
        source: "mavlink_0xb1",
        timestamp: opts.ts ?? tsIso,
        confidence: opts.confidence ?? 1,
        ...(opts.context ? { context: opts.context } : {}),
      });
      return JSON.stringify({
        type: "put",
        bucket: "ENTITY_STATES",
        key: entityId,
        revision,
        payload: {
          id: entityId,
          updated_at: tsIso,
          triples: [
            mk("uxs.domain.kind", "rover"),
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
            mk("uxs.attitude.quaternion", [0, 0, 0, 0], {
              confidence: 0,
              ts: "1970-01-01T00:00:00Z",
            }),
            mk("uxs.attitude.rates", [0, 0, 0]),
            mk("uxs.kinematics.ned", [0, 0, 0], { context: "measured" }),
            mk("uxs.kinematics.vx", 0),
            mk("uxs.kinematics.vy", 3),
            mk("uxs.kinematics.vz", 0),
            hasMeasuredHeading
              ? mk("uxs.kinematics.headingdeg", headingDeg, {
                  confidence: 1,
                  context: "measured",
                })
              : mk("uxs.kinematics.headingdeg", 0, {
                  confidence: 0,
                  ts: "1970-01-01T00:00:00Z",
                }),
            mk("geo.location.latitude", 30.2672, { context: "measured" }),
            mk("geo.location.longitude", -97.7431, { context: "measured" }),
            mk("geo.location.altitude", 150, { context: "measured" }),
          ],
        },
      });
    };

    const drawnTexts: string[] = [];
    const mockCtx = {
      clearRect: vi.fn(),
      save: vi.fn(),
      restore: vi.fn(),
      beginPath: vi.fn(),
      closePath: vi.fn(),
      rect: vi.fn(),
      clip: vi.fn(),
      translate: vi.fn(),
      rotate: vi.fn(),
      setTransform: vi.fn(),
      fillRect: vi.fn(),
      strokeRect: vi.fn(),
      moveTo: vi.fn(),
      lineTo: vi.fn(),
      arc: vi.fn(),
      fill: vi.fn(),
      stroke: vi.fn(),
      setLineDash: vi.fn(),
      fillText: vi.fn((text: string) => {
        drawnTexts.push(text);
      }),
      fillStyle: "",
      strokeStyle: "",
      lineWidth: 1,
      globalAlpha: 1,
      font: "",
      textAlign: "left",
    } as unknown as CanvasRenderingContext2D;

    const rafCallbacks: FrameRequestCallback[] = [];
    const getCtxSpy = vi
      .spyOn(HTMLCanvasElement.prototype, "getContext")
      .mockImplementation(((contextId: string) =>
        contextId === "2d"
          ? mockCtx
          : null) as typeof HTMLCanvasElement.prototype.getContext);
    const rafSpy = vi
      .spyOn(window, "requestAnimationFrame")
      .mockImplementation((cb: FrameRequestCallback) => {
        rafCallbacks.push(cb);
        return rafCallbacks.length;
      });
    const cafSpy = vi
      .spyOn(window, "cancelAnimationFrame")
      .mockImplementation(() => undefined);

    const flushTicks = (): void => {
      const pending = rafCallbacks.splice(0, rafCallbacks.length);
      for (const cb of pending) {
        cb(nowMs);
      }
      flushSync();
    };

    // 1. Seed WS2 with unknown heading (#33 Hdg=65535 -> confidence: 0, hdg_deg: 0).
    const t0Iso = new Date(nowMs).toISOString();
    skgStore.ingestRawFrame(makeHeadingSkgPut(1, false, 0, t0Iso));
    skgStore.ingestRawFrame(
      JSON.stringify({ type: "synced", bucket: "ENTITY_STATES", revision: 1 }),
    );

    const hudHost = document.createElement("div");
    document.body.appendChild(hudHost);
    let hudApp: Record<string, unknown> | null = null;

    try {
      hudApp = mount(VehicleHud, {
        target: hudHost,
        props: {
          telemetryStore,
          get vehicle() {
            return skgStore.getCommandableVehicle(entityId);
          },
          skgStatus: "connected",
          now: () => nowMs,
        },
      });
      flushSync();
      drawnTexts.length = 0;
      flushTicks();

      const hudPanel = hudHost.querySelector('[data-testid="vehicle-hud"]');
      expect(hudPanel?.getAttribute("data-pose-source")).toBe("skg");
      expect(hudPanel?.getAttribute("data-heading")).toBe("unavailable");
      expect(
        hudHost
          .querySelector('[data-testid="hud-heading-unavailable"]')
          ?.textContent?.trim(),
      ).toBe("NO HDG");
      expect(drawnTexts).toContain("HDG UNAVAILABLE");
      expect(drawnTexts).toContain("R -- P -- HDG ---");
      expect(drawnTexts).not.toContain("N");
      expect(drawnTexts).not.toContain("R -- P -- HDG 000°");

      // 2. WS1 frame arrives with unknown heading (has_heading: false, hdg_deg: 0) -> STILL unavailable!
      nowMs += 50;
      ws1Socket!.onmessage?.({
        data: JSON.stringify({
          type: "data",
          subject: "bbops.telemetry.1",
          timestamp: nowMs,
          payload: {
            sysid: 1,
            domain: "rover",
            seq: 1,
            msg_id: 33,
            quat_wxyz: [0, 0, 0, 0],
            has_attitude: false,
            body_rates: [0, 0, 0],
            ned_xyz: [0, 0, 0],
            has_local_pos: true,
            ned_vel: [0, 3, 0],
            lat_lon_alt: [30.2672, -97.7431, 150],
            has_global_pos: true,
            hdg_deg: 0,
            has_heading: false,
            timestamp: nowMs,
          },
        }),
      } as MessageEvent<unknown>);
      drawnTexts.length = 0;
      flushTicks();

      expect(hudPanel?.getAttribute("data-pose-source")).toBe("ws1");
      expect(hudPanel?.getAttribute("data-heading")).toBe("unavailable");
      expect(drawnTexts).toContain("HDG UNAVAILABLE");
      expect(drawnTexts).toContain("R -- P -- HDG ---");
      expect(drawnTexts).not.toContain("N");

      // 3. WS1 frame arrives with genuinely measured 0° North (has_heading: true, hdg_deg: 0) -> renders N and HDG 000°!
      nowMs += 50;
      ws1Socket!.onmessage?.({
        data: JSON.stringify({
          type: "data",
          subject: "bbops.telemetry.1",
          timestamp: nowMs,
          payload: {
            sysid: 1,
            domain: "rover",
            seq: 2,
            msg_id: 33,
            quat_wxyz: [0, 0, 0, 0],
            has_attitude: false,
            body_rates: [0, 0, 0],
            ned_xyz: [0, 0, 0],
            has_local_pos: true,
            ned_vel: [0, 3, 0],
            lat_lon_alt: [30.2672, -97.7431, 150],
            has_global_pos: true,
            hdg_deg: 0,
            has_heading: true,
            timestamp: nowMs,
          },
        }),
      } as MessageEvent<unknown>);
      drawnTexts.length = 0;
      flushTicks();

      expect(hudPanel?.getAttribute("data-pose-source")).toBe("ws1");
      expect(hudPanel?.getAttribute("data-heading")).toBe("measured");
      expect(
        hudHost.querySelector('[data-testid="hud-heading-unavailable"]'),
      ).toBeNull();
      expect(drawnTexts).toContain("N");
      expect(drawnTexts).toContain("R -- P -- HDG 000°");

      // 4. Disconnect WS1 and deliver a measured 0° North WS2 put -> SKG fallback ALSO renders N and HDG 000°!
      telemetryStore.disconnect();
      nowMs += 100;
      const tMeasuredSkgIso = new Date(nowMs).toISOString();
      skgStore.ingestRawFrame(makeHeadingSkgPut(2, true, 0, tMeasuredSkgIso));
      drawnTexts.length = 0;
      flushTicks();

      expect(hudPanel?.getAttribute("data-pose-source")).toBe("skg");
      expect(hudPanel?.getAttribute("data-heading")).toBe("measured");
      expect(drawnTexts).toContain("N");
      expect(drawnTexts).toContain("R -- P -- HDG 000°");

      // 5. Post-synced higher KV revision carrying an hour-old 30° pose when WS1 is disconnected MUST mark stale=true!
      nowMs += 100;
      const hourOldPoseIso = new Date(nowMs - 3_600_000).toISOString();
      skgStore.ingestRawFrame(makeHeadingSkgPut(3, true, 30, hourOldPoseIso));
      drawnTexts.length = 0;
      flushTicks();

      const regressedPose = resolveLiveVehiclePose(
        skgStore.getCommandableVehicle(entityId)!,
        {
          telemetryStatus: telemetryStore.status,
          skgStatus: "connected",
          nowMs,
        },
      );
      expect(regressedPose.source).toBe("skg");
      expect(regressedPose.headingDeg).toBe(30);
      expect(regressedPose.stale).toBe(true);
      expect(hudPanel?.getAttribute("data-stale")).toBe("true");

      // 6. Reconnect WS1 and deliver legacy startup wire shape ([1,0,0,0], [0,0,0], msg_id=33, hdg_deg=0, omitted flags) -> NO ATT + NO HDG!
      telemetryStore.connect("ws://127.0.0.1:8080/ws/telemetry");
      ws1Socket!.onopen?.(new Event("open") as never);
      nowMs += 50;
      ws1Socket!.onmessage?.({
        data: JSON.stringify({
          type: "data",
          subject: "bbops.telemetry.1",
          timestamp: nowMs,
          payload: {
            sysid: 1,
            domain: "rover",
            seq: 10,
            msg_id: 33,
            quat_wxyz: [1, 0, 0, 0],
            body_rates: [0, 0, 0],
            ned_xyz: [0, 0, 0],
            ned_vel: [0, 0, 0],
            lat_lon_alt: [30.2672, -97.7431, 150],
            has_global_pos: true,
            hdg_deg: 0,
            timestamp: nowMs,
          },
        }),
      } as MessageEvent<unknown>);
      drawnTexts.length = 0;
      flushTicks();

      expect(hudPanel?.getAttribute("data-pose-source")).toBe("ws1");
      expect(hudPanel?.getAttribute("data-stale")).toBe("false");
      expect(hudPanel?.getAttribute("data-attitude")).toBe("unavailable");
      expect(hudPanel?.getAttribute("data-heading")).toBe("unavailable");
      expect(
        hudHost
          .querySelector('[data-testid="hud-attitude-unavailable"]')
          ?.textContent?.trim(),
      ).toBe("NO ATT");
      expect(
        hudHost
          .querySelector('[data-testid="hud-heading-unavailable"]')
          ?.textContent?.trim(),
      ).toBe("NO HDG");
      expect(drawnTexts).toContain("ATTITUDE UNAVAILABLE");
      expect(drawnTexts).toContain("HDG UNAVAILABLE");
      expect(drawnTexts).toContain("R -- P -- HDG ---");
      expect(drawnTexts).not.toContain("N");

      // 7. Modern positive control on WS1: genuine #31 level-North attitude clears both NO ATT and NO HDG!
      nowMs += 50;
      ws1Socket!.onmessage?.({
        data: JSON.stringify({
          type: "data",
          subject: "bbops.telemetry.1",
          timestamp: nowMs,
          payload: {
            sysid: 1,
            domain: "rover",
            seq: 11,
            msg_id: 31,
            quat_wxyz: [1, 0, 0, 0],
            has_attitude: true,
            body_rates: [0, 0, 0],
            ned_xyz: [0, 0, 0],
            has_local_pos: true,
            ned_vel: [0, 0, 0],
            lat_lon_alt: [30.2672, -97.7431, 150],
            has_global_pos: true,
            hdg_deg: 0,
            has_heading: true,
            timestamp: nowMs,
          },
        }),
      } as MessageEvent<unknown>);
      drawnTexts.length = 0;
      flushTicks();

      expect(hudPanel?.getAttribute("data-attitude")).toBe("measured");
      expect(hudPanel?.getAttribute("data-heading")).toBe("measured");
      expect(
        hudHost.querySelector('[data-testid="hud-attitude-unavailable"]'),
      ).toBeNull();
      expect(
        hudHost.querySelector('[data-testid="hud-heading-unavailable"]'),
      ).toBeNull();
      expect(drawnTexts).toContain("N");
      expect(drawnTexts).toContain("R 0° P 0° Y 0°");
    } finally {
      if (hudApp) unmount(hudApp);
      telemetryStore.disconnect();
      hudHost.remove();
      getCtxSpy.mockRestore();
      rafSpy.mockRestore();
      cafSpy.mockRestore();
    }
  });
});
