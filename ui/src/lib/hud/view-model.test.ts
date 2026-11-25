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
import {
  NedGridAdapter,
  computeCrabAngleVectors,
  resolveVehicleSafetyTone,
} from "../viewport/ned-grid";
import {
  HUD_READOUT_THROTTLE_MS,
  SKG_VEHICLE_FRESH_MS,
  USV_HUD_CRAB_DEADBAND_MPS,
  VEHICLE_FRESH_DEFAULT_MS,
  VEHICLE_FRESH_SUPERVISORY_MS,
  buildExternalContactSummary,
  buildFilmstripItems,
  buildHealthStrip,
  buildVehicleBannerView,
  computeFlightMetrics,
  evaluateVehicleFreshness,
  evaluateVehicleSafetySeverity,
  formatArmedBadge,
  formatAuthorityBadge,
  formatLinkTierBadge,
  formatObservationAge,
  formatVehicleHudReadouts,
  quaternionToEulerDeg,
  resolveLiveVehiclePose,
  resolveVehicleFreshnessThresholdMs,
  selectBannerPresentation,
  selectFilmstripItems,
  shouldRefreshHudReadout,
  summarizeFleetHealth,
  summarizeStreamReadiness,
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
      const hudCellText = (testId: string): string | null =>
        hudHost.querySelector(`[data-testid="${testId}"] dd`)?.textContent ??
        null;
      expect(
        hudHost.querySelector('[data-testid="hud-readout-strip"]'),
      ).not.toBeNull();
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
      expect(hudCellText("hud-readout-attitude")).toBe("0° / 0° / 45°");
      expect(hudCellText("hud-readout-gs")).toBe("4.0 m/s");
      expect(hudCellText("hud-readout-vs")).toBe("+0.0 m/s");
      expect(hudCellText("hud-readout-alt")).toBe("10.0 m");
      expect(adapter.getCamera().centerNed).toEqual([12, -6]);

      // Step 1B: 2nd WS1-only frame arrives at +50ms (20 Hz cadence): canvas updates immediately while HTML readout throttles until +250ms (4 Hz).
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
      expect(drawnTexts).toContain("E");
      expect(adapter.getCamera().centerNed).toEqual([18, -9]);
      // Within 250ms throttle window: HTML readout holds previous 4 Hz snapshot (zero 20 Hz DOM thrash).
      expect(hudCellText("hud-readout-attitude")).toBe("0° / 0° / 45°");

      nowMs += 200;
      flushAnimationTicks();
      expect(hudCellText("hud-readout-attitude")).toBe("0° / 0° / 90°");
      expect(hudCellText("hud-readout-gs")).toBe("7.5 m/s");
      expect(hudCellText("hud-readout-alt")).toBe("14.0 m");

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
      expect(hudCellText("hud-readout-attitude")).toBe("0° / 0° / 120°");
      expect(hudCellText("hud-readout-gs")).toBe("5.0 m/s");
      expect(hudCellText("hud-readout-alt")).toBe("22.0 m");
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
      expect(hudCellText("hud-readout-attitude")).toBe("0° / 0° / 215°");
      expect(hudCellText("hud-readout-gs")).toBe("6.0 m/s");
      expect(hudCellText("hud-readout-alt")).toBe("34.0 m");
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
          has_local_pos: true,
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
      expect(
        hudHost.querySelector('[data-testid="hud-readout-attitude"] dt')
          ?.textContent,
      ).toBe("R / P / HDG");
      expect(
        hudHost.querySelector('[data-testid="hud-readout-attitude"] dd')
          ?.textContent,
      ).toBe("-- / -- / 090°");
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
      expect(
        hudHost.querySelector('[data-testid="hud-readout-attitude"] dt')
          ?.textContent,
      ).toBe("R / P / Y");
      expect(
        hudHost.querySelector('[data-testid="hud-readout-attitude"] dd')
          ?.textContent,
      ).toBe("0° / 0° / 0°");
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
      const hudAttitudeText = (): string | null =>
        hudHost.querySelector('[data-testid="hud-readout-attitude"] dd')
          ?.textContent ?? null;
      expect(hudPanel?.getAttribute("data-pose-source")).toBe("skg");
      expect(hudPanel?.getAttribute("data-heading")).toBe("unavailable");
      expect(
        hudHost
          .querySelector('[data-testid="hud-heading-unavailable"]')
          ?.textContent?.trim(),
      ).toBe("NO HDG");
      expect(drawnTexts).toContain("HDG UNAVAILABLE");
      expect(hudAttitudeText()).toBe("-- / -- / ---");
      expect(drawnTexts).not.toContain("N");

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
      expect(hudAttitudeText()).toBe("-- / -- / ---");
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
      expect(hudAttitudeText()).toBe("-- / -- / 000°");

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
      expect(hudAttitudeText()).toBe("-- / -- / 000°");

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
      expect(hudAttitudeText()).toBe("-- / -- / ---");
      expect(drawnTexts).not.toContain("N");

      // 7. Unannotated legacy #31 level-North attitude clears both NO ATT and NO HDG,
      // and subsequent unannotated #32 and #33 position frames 50 ms later MUST keep both measured!
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

      expect(hudPanel?.getAttribute("data-attitude")).toBe("measured");
      expect(hudPanel?.getAttribute("data-heading")).toBe("measured");
      expect(
        hudHost.querySelector('[data-testid="hud-attitude-unavailable"]'),
      ).toBeNull();
      expect(
        hudHost.querySelector('[data-testid="hud-heading-unavailable"]'),
      ).toBeNull();
      expect(drawnTexts).toContain("N");
      expect(hudAttitudeText()).toBe("0° / 0° / 0°");

      // 8. 50 ms later, unannotated #32 local-position carrying fused [1,0,0,0] keeps attitude + heading measured!
      nowMs += 50;
      ws1Socket!.onmessage?.({
        data: JSON.stringify({
          type: "data",
          subject: "bbops.telemetry.1",
          timestamp: nowMs,
          payload: {
            sysid: 1,
            domain: "rover",
            seq: 12,
            msg_id: 32,
            quat_wxyz: [1, 0, 0, 0],
            body_rates: [0, 0, 0],
            ned_xyz: [2, -1, 0],
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
      expect(hudPanel?.getAttribute("data-attitude")).toBe("measured");
      expect(hudPanel?.getAttribute("data-heading")).toBe("measured");
      expect(drawnTexts).toContain("N");
      expect(hudAttitudeText()).toBe("0° / 0° / 0°");

      // 9. Another 50 ms later, unannotated #33 global-position carrying fused [1,0,0,0] also stays measured!
      nowMs += 50;
      ws1Socket!.onmessage?.({
        data: JSON.stringify({
          type: "data",
          subject: "bbops.telemetry.1",
          timestamp: nowMs,
          payload: {
            sysid: 1,
            domain: "rover",
            seq: 13,
            msg_id: 33,
            quat_wxyz: [1, 0, 0, 0],
            body_rates: [0, 0, 0],
            ned_xyz: [2, -1, 0],
            ned_vel: [0, 0, 0],
            lat_lon_alt: [30.2673, -97.7432, 150],
            has_global_pos: true,
            hdg_deg: 0,
            timestamp: nowMs,
          },
        }),
      } as MessageEvent<unknown>);
      drawnTexts.length = 0;
      flushTicks();
      expect(hudPanel?.getAttribute("data-attitude")).toBe("measured");
      expect(hudPanel?.getAttribute("data-heading")).toBe("measured");

      // 10. End WS1 session and open a new session: unannotated #33 resets back to NO ATT + NO HDG!
      telemetryStore.disconnect();
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
            seq: 14,
            msg_id: 33,
            quat_wxyz: [1, 0, 0, 0],
            body_rates: [0, 0, 0],
            ned_xyz: [0, 0, 0],
            ned_vel: [0, 0, 0],
            lat_lon_alt: [30.2673, -97.7432, 150],
            has_global_pos: true,
            hdg_deg: 0,
            timestamp: nowMs,
          },
        }),
      } as MessageEvent<unknown>);
      drawnTexts.length = 0;
      flushTicks();
      expect(hudPanel?.getAttribute("data-attitude")).toBe("unavailable");
      expect(hudPanel?.getAttribute("data-heading")).toBe("unavailable");
    } finally {
      if (hudApp) unmount(hudApp);
      telemetryStore.disconnect();
      hudHost.remove();
      getCtxSpy.mockRestore();
      rafSpy.mockRestore();
      cafSpy.mockRestore();
    }
  });

  // spec: cop-ui / Primary attitude HUD, 4-state ControlAuthority banner, and external contact UI isolation
  it("keeps resolveLiveVehiclePose stale across multi-step historical SKG replay updates until an observation catches up or advances", () => {
    let browserNowMs = Date.parse("2026-11-24T12:00:00.000Z");
    const skgStore = createSkgStore({ now: () => browserNowMs });
    const entityId = "boxbotix.local.robotics.uxs.vehicle.1";

    const mkPut = (rev: number, poseIso: string, hdgDeg: number): string => {
      const mk = (predicate: string, object: unknown): SkgTriple => ({
        subject: entityId,
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
        key: entityId,
        revision: rev,
        payload: {
          id: entityId,
          updated_at: new Date(browserNowMs).toISOString(),
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
            mk("uxs.attitude.quaternion", [0, 0, 0, 0]),
            mk("uxs.attitude.rates", [0, 0, 0]),
            mk("uxs.kinematics.ned", [5, 0, 0]),
            mk("uxs.kinematics.vx", 0),
            mk("uxs.kinematics.vy", 0),
            mk("uxs.kinematics.vz", 0),
            mk("uxs.kinematics.headingdeg", hdgDeg),
            mk("geo.location.latitude", 30.2672),
            mk("geo.location.longitude", -97.7431),
            mk("geo.location.altitude", 150),
          ],
        },
      });
    };

    skgStore.ingestRawFrame(mkPut(1, "2026-11-24T11:59:59.500Z", 15));
    skgStore.ingestRawFrame(
      JSON.stringify({ type: "synced", bucket: "ENTITY_STATES", revision: 1 }),
    );

    // Live 12:00:00.000Z observation -> fresh.
    skgStore.ingestRawFrame(mkPut(2, "2026-11-24T12:00:00.000Z", 20));
    expect(
      resolveLiveVehiclePose(skgStore.getCommandableVehicle(entityId)!, {
        telemetryStatus: "reconnecting",
        skgStatus: "connected",
        nowMs: browserNowMs,
      }).stale,
    ).toBe(false);

    // First 11:00 replay update -> stale.
    browserNowMs += 100;
    skgStore.ingestRawFrame(mkPut(3, "2026-11-24T11:00:00.100Z", 30));
    expect(
      resolveLiveVehiclePose(skgStore.getCommandableVehicle(entityId)!, {
        telemetryStatus: "reconnecting",
        skgStatus: "connected",
        nowMs: browserNowMs,
      }).stale,
    ).toBe(true);

    // Second 11:00 replay update 100 ms newer than the first -> MUST remain stale!
    browserNowMs += 100;
    skgStore.ingestRawFrame(mkPut(4, "2026-11-24T11:00:00.200Z", 31));
    const afterSecondReplay = resolveLiveVehiclePose(
      skgStore.getCommandableVehicle(entityId)!,
      {
        telemetryStatus: "reconnecting",
        skgStatus: "connected",
        nowMs: browserNowMs,
      },
    );
    expect(afterSecondReplay.stale).toBe(true);
    expect(afterSecondReplay.headingDeg).toBe(31);

    // Genuinely new observation advancing past 12:00:00.000Z -> fresh again!
    browserNowMs += 300;
    skgStore.ingestRawFrame(mkPut(5, "2026-11-24T12:00:00.500Z", 35));
    const afterCatchUp = resolveLiveVehiclePose(
      skgStore.getCommandableVehicle(entityId)!,
      {
        telemetryStatus: "reconnecting",
        skgStatus: "connected",
        nowMs: browserNowMs,
      },
    );
    expect(afterCatchUp.stale).toBe(false);
    expect(afterCatchUp.headingDeg).toBe(35);
  });

  // spec: cop-ui / Primary attitude HUD, 4-state ControlAuthority banner, and external contact UI isolation
  // spec: cop-ui / Renderer-agnostic ViewportAdapter, surface-vs-safety-HUD layer split, and multi-domain tactical canvas
  it("clears retained previous-session attitude, heading, local position, and geographic frame origin across restarts while honoring legacy #32 local reports", () => {
    const entityId = "boxbotix.bbops.robotics.uxs.vehicle.1";
    let nowMs = Date.parse("2026-11-24T12:00:00.300Z");
    const ws2Rig = new MockWebSocketRig();
    const ws1Rig = new MockWebSocketRig();
    const skgStore = createSkgStore({
      now: () => nowMs,
      createSocket: ws2Rig.createSocket,
    });
    const telemetryStore = createTelemetryStore({
      now: () => nowMs,
      createSocket: ws1Rig.createSocket,
    });

    interface DomainAssertion<T> {
      value: T;
      measured: boolean;
      timestamp: string;
    }

    const buildMergedPut = (opts: {
      revision: number;
      healthIso: string;
      quat: DomainAssertion<[number, number, number, number]>;
      ned: DomainAssertion<[number, number, number]>;
      heading: DomainAssertion<number>;
      globalPos: DomainAssertion<[number, number, number]>;
      crcErrors?: number;
    }): string => {
      const healthTriple = (predicate: string, object: unknown): SkgTriple => ({
        subject: entityId,
        predicate,
        object,
        source: "mavlink_0xb1",
        timestamp: opts.healthIso,
        confidence: 1,
      });
      const poseTriple = (
        predicate: string,
        object: unknown,
        domain: { measured: boolean; timestamp: string },
      ): SkgTriple => ({
        subject: entityId,
        predicate,
        object,
        source: "mavlink_0xb1",
        timestamp: domain.timestamp,
        confidence: domain.measured ? 1 : 0,
        ...(domain.measured ? { context: "measured" } : {}),
      });

      return JSON.stringify({
        type: "put",
        bucket: "ENTITY_STATES",
        key: entityId,
        revision: opts.revision,
        payload: {
          id: entityId,
          updated_at: opts.healthIso,
          triples: [
            healthTriple("uxs.domain.kind", "rover"),
            healthTriple("uxs.authority.state", "companion-teleop"),
            healthTriple("uxs.mode.companion", "teleop-velocity"),
            healthTriple("uxs.link.tier", "mesh-high-rate"),
            healthTriple("uxs.health.armed", true),
            healthTriple("uxs.health.flags", {
              clock_sync_locked: true,
              teleop_deadman_clamped: false,
              crsf_link_healthy: true,
              companion_trunk_healthy: true,
            }),
            healthTriple("uxs.health.failsafes", 0),
            healthTriple("uxs.health.crcerrors", opts.crcErrors ?? 0),
            healthTriple("uxs.health.loopdtus", 950),
            healthTriple("uxs.health.maxloopdtus", 1100),
            healthTriple("uxs.health.rttus", 350),
            poseTriple("uxs.attitude.quaternion", opts.quat.value, opts.quat),
            poseTriple("uxs.attitude.rates", [0, 0, 0], opts.quat),
            poseTriple("uxs.kinematics.ned", opts.ned.value, opts.ned),
            poseTriple("uxs.kinematics.vx", 0, opts.ned),
            poseTriple("uxs.kinematics.vy", 0, opts.ned),
            poseTriple("uxs.kinematics.vz", 0, opts.ned),
            poseTriple(
              "uxs.kinematics.headingdeg",
              opts.heading.value,
              opts.heading,
            ),
            poseTriple(
              "geo.location.latitude",
              opts.globalPos.value[0],
              opts.globalPos,
            ),
            poseTriple(
              "geo.location.longitude",
              opts.globalPos.value[1],
              opts.globalPos,
            ),
            poseTriple(
              "geo.location.altitude",
              opts.globalPos.value[2],
              opts.globalPos,
            ),
            healthTriple("dc.terms.title", "UXS rover #1"),
          ],
        },
      });
    };

    const hudTexts: string[] = [];
    const overlayTexts: string[] = [];
    const overlayTrailSegments: Array<{
      x0: number;
      y0: number;
      x1: number;
      y1: number;
    }> = [];

    const makeMock2dCtx = (
      kind: "hud" | "overlay" | "surface",
    ): CanvasRenderingContext2D => {
      let lastMoveX = 0;
      let lastMoveY = 0;
      const ctx = {
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
          if (
            kind === "overlay" &&
            ctx.strokeStyle === "rgba(96, 165, 250, 0.36)"
          ) {
            overlayTrailSegments.push({
              x0: lastMoveX,
              y0: lastMoveY,
              x1: x,
              y1: y,
            });
          }
          lastMoveX = x;
          lastMoveY = y;
        }),
        arc: vi.fn(),
        fill: vi.fn(),
        stroke: vi.fn(),
        setLineDash: vi.fn(),
        fillText: vi.fn((text: string) => {
          if (kind === "hud") {
            hudTexts.push(text);
          } else if (kind === "overlay") {
            overlayTexts.push(text);
          }
        }),
        fillStyle: "",
        strokeStyle: "",
        lineWidth: 1,
        globalAlpha: 1,
        font: "",
        textAlign: "left",
      };
      return ctx as unknown as CanvasRenderingContext2D;
    };

    const hudCtx = makeMock2dCtx("hud");
    const overlayCtx = makeMock2dCtx("overlay");
    const surfaceCtx = makeMock2dCtx("surface");

    const rafCallbacks: FrameRequestCallback[] = [];
    const getCtxSpy = vi
      .spyOn(HTMLCanvasElement.prototype, "getContext")
      .mockImplementation(function (
        this: HTMLCanvasElement,
        contextId: string,
      ) {
        if (contextId !== "2d") {
          return null;
        }
        if (this.classList.contains("hud-canvas")) {
          return hudCtx;
        }
        if (this.classList.contains("safety-hud-canvas")) {
          return overlayCtx;
        }
        return surfaceCtx;
      } as typeof HTMLCanvasElement.prototype.getContext);
    const rafSpy = vi
      .spyOn(window, "requestAnimationFrame")
      .mockImplementation((cb: FrameRequestCallback) => {
        rafCallbacks.push(cb);
        return rafCallbacks.length;
      });
    const cafSpy = vi
      .spyOn(window, "cancelAnimationFrame")
      .mockImplementation(() => undefined);

    const resetDrawn = (): void => {
      hudTexts.length = 0;
      overlayTexts.length = 0;
      overlayTrailSegments.length = 0;
    };

    const flushTicks = (): void => {
      flushSync();
      for (const cb of rafCallbacks.splice(0, rafCallbacks.length)) {
        cb(nowMs);
      }
      flushSync();
    };

    const hudHost = document.createElement("div");
    const canvasHost = document.createElement("div");
    document.body.append(hudHost, canvasHost);
    const adapter = new NedGridAdapter({ widthPx: 800, heightPx: 600 });
    const viewportStore = createViewportStore({ skgStore });
    let hudApp: Record<string, unknown> | null = null;
    let canvasApp: Record<string, unknown> | null = null;

    try {
      // Session 1 step A (12:00:00.150Z): initial local NED [0, 0, -2] + 90 deg East yaw + global fix.
      nowMs = Date.parse("2026-11-24T12:00:00.150Z");
      skgStore.connect("ws://127.0.0.1:8080/ws/graph");
      ws2Rig.latest.serverOpen();
      skgStore.ingestRawFrame(
        buildMergedPut({
          revision: 1,
          healthIso: "2026-11-24T12:00:00.150Z",
          quat: {
            value: eulerRadToQuaternionWxyz(0, 0, Math.PI / 2),
            measured: true,
            timestamp: "2026-11-24T12:00:00.100Z",
          },
          ned: {
            value: [0, 0, -2],
            measured: true,
            timestamp: "2026-11-24T12:00:00.150Z",
          },
          heading: {
            value: 90,
            measured: true,
            timestamp: "2026-11-24T12:00:00.100Z",
          },
          globalPos: {
            value: [37.7749, -122.4194, 25],
            measured: true,
            timestamp: "2026-11-24T12:00:00.150Z",
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

      const makeExternalContact = (
        id: string,
        title: string,
        lla: [number, number, number],
      ) => ({
        id,
        kind: "external-contact" as const,
        title,
        contactType: "a-f-G-U-C",
        location: { latitude: lla[0], longitude: lla[1], altitude: lla[2] },
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
      });
      skgStore.entities.set(
        "ais.uscg.cop.contact.vessel.sf",
        makeExternalContact(
          "ais.uscg.cop.contact.vessel.sf",
          "SF Beacon",
          [37.7749, -122.4194, 25],
        ),
      );
      skgStore.entities.set(
        "ais.uscg.cop.contact.vessel.austin",
        makeExternalContact(
          "ais.uscg.cop.contact.vessel.austin",
          "Austin Beacon",
          [30.2672, -97.7431, 150],
        ),
      );

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
      flushTicks();
      const s1StepABeaconProj = adapter.project({
        lla: [37.7749, -122.4194, 25],
      });
      expect(s1StepABeaconProj.visible).toBe(true);

      // Session 1 step B (12:00:00.300Z): local NED advances to [12.5, -4.25, -2] while globalPos retains its cached 12:00:00.150Z fix; SF Beacon stays stationary.
      nowMs = Date.parse("2026-11-24T12:00:00.300Z");
      skgStore.ingestRawFrame(
        buildMergedPut({
          revision: 2,
          healthIso: "2026-11-24T12:00:00.300Z",
          quat: {
            value: eulerRadToQuaternionWxyz(0, 0, Math.PI / 2),
            measured: true,
            timestamp: "2026-11-24T12:00:00.100Z",
          },
          ned: {
            value: [12.5, -4.25, -2],
            measured: true,
            timestamp: "2026-11-24T12:00:00.200Z",
          },
          heading: {
            value: 90,
            measured: true,
            timestamp: "2026-11-24T12:00:00.100Z",
          },
          globalPos: {
            value: [37.7749, -122.4194, 25],
            measured: true,
            timestamp: "2026-11-24T12:00:00.150Z",
          },
        }),
      );
      resetDrawn();
      flushTicks();

      const hudPanel = hudHost.querySelector('[data-testid="vehicle-hud"]');
      const hudReadout = (testId: string): string | null =>
        hudHost.querySelector(`[data-testid="${testId}"] dd`)?.textContent ??
        null;
      const tacticalViewport = canvasHost.querySelector(
        '[data-testid="tactical-viewport"]',
      );
      expect(hudPanel?.getAttribute("data-attitude")).toBe("measured");
      expect(hudPanel?.getAttribute("data-heading")).toBe("measured");
      expect(hudPanel?.getAttribute("data-local-pos")).toBe("measured");
      expect(
        hudHost.querySelector('[data-testid="hud-local-pos-unavailable"]'),
      ).toBeNull();
      expect(hudReadout("hud-readout-gs")).toBe("0.0 m/s");
      expect(hudReadout("hud-readout-vs")).toBe("+0.0 m/s");
      expect(hudReadout("hud-readout-alt")).toBe("2.0 m");
      expect(tacticalViewport?.getAttribute("data-origin-anchored")).toBe(
        "true",
      );
      expect(
        canvasHost.querySelector('[data-testid="mixed-frame-withheld"]'),
      ).toBeNull();
      const s1NedProj = adapter.project({ ned: [12.5, -4.25, -2] });
      const s1GeoProj = adapter.project({ lla: [37.7749, -122.4194, 25] });
      expect(s1NedProj.visible).toBe(true);
      expect(s1GeoProj.visible).toBe(true);
      expect(
        Math.hypot(
          s1NedProj.x - s1StepABeaconProj.x,
          s1NedProj.y - s1StepABeaconProj.y,
        ),
      ).toBeCloseTo(Math.hypot(12.5, -4.25) * 4, 2);
      expect(s1GeoProj.x).toBe(s1StepABeaconProj.x);
      expect(s1GeoProj.y).toBe(s1StepABeaconProj.y);
      expect(overlayTexts).toContain("#1 rover");
      expect(overlayTexts).toContain("0.0m/s · 2.0m");
      expect(overlayTexts).toContain("SF Beacon");
      expect(overlayTexts).not.toContain("Austin Beacon");
      expect(overlayTrailSegments).toHaveLength(1);

      // Pan camera away from [0, 0] and tether to entityId before Session 2 restart to prove unmeasured [0, 0, 0] does not snap camera.
      adapter.setCamera({ centerNed: [40, -25] });
      viewportStore.tetherEntity(entityId);

      // Session 2 restarts 1 hour later (13:00:00.000Z admission) and receives #33 global fix with Hdg=65535 at 13:00:00.250Z.
      nowMs = Date.parse("2026-11-24T13:00:00.250Z");
      skgStore.ingestRawFrame(
        buildMergedPut({
          revision: 3,
          healthIso: "2026-11-24T13:00:00.250Z",
          quat: {
            value: [0, 0, 0, 0],
            measured: false,
            timestamp: "2026-11-24T13:00:00.000Z",
          },
          ned: {
            value: [0, 0, 0],
            measured: false,
            timestamp: "2026-11-24T13:00:00.000Z",
          },
          heading: {
            value: 0,
            measured: false,
            timestamp: "2026-11-24T13:00:00.250Z",
          },
          globalPos: {
            value: [30.2672, -97.7431, 150],
            measured: true,
            timestamp: "2026-11-24T13:00:00.250Z",
          },
        }),
      );
      resetDrawn();
      flushTicks();

      const afterRestartPose = resolveLiveVehiclePose(
        skgStore.getCommandableVehicle(entityId)!,
        {
          telemetryStatus: "reconnecting",
          skgStatus: "connected",
          nowMs,
        },
      );
      expect(afterRestartPose.source).toBe("skg");
      expect(afterRestartPose.stale).toBe(false);
      expect(afterRestartPose.lla).toEqual([30.2672, -97.7431, 150]);
      expect(afterRestartPose.hasAttitude).toBe(false);
      expect(afterRestartPose.hasHeading).toBe(false);
      expect(afterRestartPose.hasLocalPos).toBe(false);
      expect(afterRestartPose.hasVerifiedFrameReference).toBe(false);
      expect(afterRestartPose.quat).toEqual([0, 0, 0, 0]);
      expect(afterRestartPose.ned).toEqual([0, 0, 0]);
      expect(hudPanel?.getAttribute("data-stale")).toBe("false");
      expect(hudPanel?.getAttribute("data-attitude")).toBe("unavailable");
      expect(hudPanel?.getAttribute("data-heading")).toBe("unavailable");
      expect(hudPanel?.getAttribute("data-local-pos")).toBe("unavailable");
      expect(
        hudHost.querySelector('[data-testid="hud-heading-unavailable"]')
          ?.textContent,
      ).toContain("NO HDG");
      expect(
        hudHost.querySelector('[data-testid="hud-local-pos-unavailable"]')
          ?.textContent,
      ).toContain("NO POS");
      expect(hudTexts).toContain("ATTITUDE UNAVAILABLE");
      expect(hudTexts).toContain("HDG UNAVAILABLE");
      expect(hudReadout("hud-readout-gs")).toBe("--");
      expect(hudReadout("hud-readout-vs")).toBe("--");
      expect(hudReadout("hud-readout-alt")).toBe("--");
      expect(adapter.getCamera().originLla).toBeNull();
      expect(tacticalViewport?.getAttribute("data-origin-anchored")).toBe(
        "false",
      );
      expect(
        canvasHost.querySelector('[data-testid="mixed-frame-withheld"]'),
      ).not.toBeNull();
      expect(
        Number.isNaN(adapter.project({ lla: [37.7749, -122.4194, 25] }).rangeM),
      ).toBe(true);
      expect(
        Number.isNaN(adapter.project({ lla: [30.2672, -97.7431, 150] }).rangeM),
      ).toBe(true);
      expect(overlayTexts).toEqual([]);
      expect(overlayTrailSegments).toHaveLength(0);
      expect(adapter.getCamera().centerNed).toEqual([40, -25]);

      // Health-only update at 13:00:00.500Z preserves per-domain pose timestamps and unavailability.
      nowMs = Date.parse("2026-11-24T13:00:00.500Z");
      skgStore.ingestRawFrame(
        buildMergedPut({
          revision: 4,
          healthIso: "2026-11-24T13:00:00.500Z",
          crcErrors: 2,
          quat: {
            value: [0, 0, 0, 0],
            measured: false,
            timestamp: "2026-11-24T13:00:00.000Z",
          },
          ned: {
            value: [0, 0, 0],
            measured: false,
            timestamp: "2026-11-24T13:00:00.000Z",
          },
          heading: {
            value: 0,
            measured: false,
            timestamp: "2026-11-24T13:00:00.250Z",
          },
          globalPos: {
            value: [30.2672, -97.7431, 150],
            measured: true,
            timestamp: "2026-11-24T13:00:00.250Z",
          },
        }),
      );
      resetDrawn();
      flushTicks();
      const vehAfterHealth = skgStore.getCommandableVehicle(entityId)!;
      expect(vehAfterHealth.poseObservedAt).toBe("2026-11-24T13:00:00.250Z");
      expect(vehAfterHealth.hasAttitude).toBe(false);
      expect(vehAfterHealth.hasHeading).toBe(false);
      expect(vehAfterHealth.hasLocalPos).toBe(false);
      expect(vehAfterHealth.hasGlobalPos).toBe(true);
      expect(hudReadout("hud-readout-gs")).toBe("--");
      expect(hudReadout("hud-readout-alt")).toBe("--");
      expect(adapter.getCamera().originLla).toBeNull();
      expect(overlayTexts).toEqual([]);

      // Re-measure heading (#33 Hdg=180 deg) while attitude and local position remain unavailable.
      nowMs = Date.parse("2026-11-24T13:00:00.750Z");
      skgStore.ingestRawFrame(
        buildMergedPut({
          revision: 5,
          healthIso: "2026-11-24T13:00:00.750Z",
          quat: {
            value: [0, 0, 0, 0],
            measured: false,
            timestamp: "2026-11-24T13:00:00.000Z",
          },
          ned: {
            value: [0, 0, 0],
            measured: false,
            timestamp: "2026-11-24T13:00:00.000Z",
          },
          heading: {
            value: 180,
            measured: true,
            timestamp: "2026-11-24T13:00:00.750Z",
          },
          globalPos: {
            value: [30.2672, -97.7431, 150],
            measured: true,
            timestamp: "2026-11-24T13:00:00.750Z",
          },
        }),
      );
      resetDrawn();
      flushTicks();
      const afterHdgPose = resolveLiveVehiclePose(
        skgStore.getCommandableVehicle(entityId)!,
        { telemetryStatus: "reconnecting", skgStatus: "connected", nowMs },
      );
      expect(afterHdgPose.hasHeading).toBe(true);
      expect(afterHdgPose.headingDeg).toBe(180);
      expect(afterHdgPose.hasAttitude).toBe(false);
      expect(afterHdgPose.hasLocalPos).toBe(false);
      expect(afterHdgPose.hasVerifiedFrameReference).toBe(false);
      expect(hudPanel?.getAttribute("data-heading")).toBe("measured");
      expect(hudPanel?.getAttribute("data-attitude")).toBe("unavailable");
      expect(hudPanel?.getAttribute("data-local-pos")).toBe("unavailable");
      expect(hudReadout("hud-readout-gs")).toBe("--");
      expect(hudReadout("hud-readout-alt")).toBe("--");
      expect(adapter.getCamera().originLla).toBeNull();
      expect(overlayTexts).toEqual([]);
      expect(overlayTrailSegments).toHaveLength(0);

      // Re-measure local position (#32 [15, -8, -4]) while attitude remains unavailable -> re-anchors geographic origin to Austin!
      nowMs = Date.parse("2026-11-24T13:00:01.000Z");
      skgStore.ingestRawFrame(
        buildMergedPut({
          revision: 6,
          healthIso: "2026-11-24T13:00:01.000Z",
          quat: {
            value: [0, 0, 0, 0],
            measured: false,
            timestamp: "2026-11-24T13:00:00.000Z",
          },
          ned: {
            value: [15, -8, -4],
            measured: true,
            timestamp: "2026-11-24T13:00:01.000Z",
          },
          heading: {
            value: 180,
            measured: true,
            timestamp: "2026-11-24T13:00:00.750Z",
          },
          globalPos: {
            value: [30.2672, -97.7431, 150],
            measured: true,
            timestamp: "2026-11-24T13:00:00.750Z",
          },
        }),
      );
      resetDrawn();
      flushTicks();
      const afterNedPose = resolveLiveVehiclePose(
        skgStore.getCommandableVehicle(entityId)!,
        { telemetryStatus: "reconnecting", skgStatus: "connected", nowMs },
      );
      expect(afterNedPose.hasLocalPos).toBe(true);
      expect(afterNedPose.hasVerifiedFrameReference).toBe(true);
      expect(afterNedPose.ned).toEqual([15, -8, -4]);
      expect(afterNedPose.hasAttitude).toBe(false);
      expect(hudPanel?.getAttribute("data-local-pos")).toBe("measured");
      expect(
        hudHost.querySelector('[data-testid="hud-local-pos-unavailable"]'),
      ).toBeNull();
      expect(hudReadout("hud-readout-gs")).toBe("0.0 m/s");
      expect(hudReadout("hud-readout-vs")).toBe("+0.0 m/s");
      expect(hudReadout("hud-readout-alt")).toBe("4.0 m");
      expect(tacticalViewport?.getAttribute("data-origin-anchored")).toBe(
        "true",
      );
      expect(
        canvasHost.querySelector('[data-testid="mixed-frame-withheld"]'),
      ).toBeNull();
      const s2NedProj = adapter.project({ ned: [15, -8, -4] });
      const s2GeoProj = adapter.project({ lla: [30.2672, -97.7431, 150] });
      expect(s2NedProj.visible).toBe(true);
      expect(s2GeoProj.visible).toBe(true);
      expect(s2GeoProj.x).toBeCloseTo(s2NedProj.x, 4);
      expect(s2GeoProj.y).toBeCloseTo(s2NedProj.y, 4);
      expect(overlayTexts).toContain("#1 rover");
      expect(overlayTexts).toContain("0.0m/s · 4.0m");
      expect(overlayTexts).toContain("Austin Beacon");
      expect(overlayTexts).not.toContain("SF Beacon");
      // First post-restart local sample must NOT connect back to [0, 0, 0] or Session 1's trail.
      expect(overlayTrailSegments).toHaveLength(0);
      expect(adapter.getCamera().centerNed).toEqual([15, -8]);

      // Re-measure attitude (#31 45 deg yaw) + second post-restart NED [18, -6, -4] -> all domains measured & fresh Session 2 trail segment drawn.
      nowMs = Date.parse("2026-11-24T13:00:01.250Z");
      skgStore.ingestRawFrame(
        buildMergedPut({
          revision: 7,
          healthIso: "2026-11-24T13:00:01.250Z",
          quat: {
            value: eulerRadToQuaternionWxyz(0, 0, Math.PI / 4),
            measured: true,
            timestamp: "2026-11-24T13:00:01.250Z",
          },
          ned: {
            value: [18, -6, -4],
            measured: true,
            timestamp: "2026-11-24T13:00:01.250Z",
          },
          heading: {
            value: 45,
            measured: true,
            timestamp: "2026-11-24T13:00:01.250Z",
          },
          globalPos: {
            value: [30.2672, -97.7431, 150],
            measured: true,
            timestamp: "2026-11-24T13:00:00.750Z",
          },
        }),
      );
      resetDrawn();
      flushTicks();
      const afterAttPose = resolveLiveVehiclePose(
        skgStore.getCommandableVehicle(entityId)!,
        { telemetryStatus: "reconnecting", skgStatus: "connected", nowMs },
      );
      expect(afterAttPose.hasAttitude).toBe(true);
      expect(afterAttPose.hasHeading).toBe(true);
      expect(afterAttPose.hasLocalPos).toBe(true);
      expect(afterAttPose.headingDeg).toBeCloseTo(45, 5);
      expect(hudPanel?.getAttribute("data-attitude")).toBe("measured");
      expect(hudPanel?.getAttribute("data-heading")).toBe("measured");
      expect(hudPanel?.getAttribute("data-local-pos")).toBe("measured");
      expect(hudReadout("hud-readout-attitude")).toBe("0° / 0° / 45°");
      expect(overlayTrailSegments).toHaveLength(1);

      // Session 3 (14:00:00.200Z): genuine measured [0, 0, 0] local position & zero velocity WITHOUT any global fix (SKG + WS1).
      nowMs = Date.parse("2026-11-24T14:00:00.200Z");
      skgStore.ingestRawFrame(
        buildMergedPut({
          revision: 8,
          healthIso: "2026-11-24T14:00:00.200Z",
          quat: {
            value: eulerRadToQuaternionWxyz(0, 0, 0),
            measured: true,
            timestamp: "2026-11-24T14:00:00.200Z",
          },
          ned: {
            value: [0, 0, 0],
            measured: true,
            timestamp: "2026-11-24T14:00:00.200Z",
          },
          heading: {
            value: 0,
            measured: true,
            timestamp: "2026-11-24T14:00:00.200Z",
          },
          globalPos: {
            value: [0, 0, 0],
            measured: false,
            timestamp: "2026-11-24T14:00:00.000Z",
          },
        }),
      );
      resetDrawn();
      flushTicks();
      const zeroNoGpsSkgPose = resolveLiveVehiclePose(
        skgStore.getCommandableVehicle(entityId)!,
        { telemetryStatus: "reconnecting", skgStatus: "connected", nowMs },
      );
      expect(zeroNoGpsSkgPose.source).toBe("skg");
      expect(zeroNoGpsSkgPose.hasLocalPos).toBe(true);
      expect(zeroNoGpsSkgPose.lla).toBeNull();
      expect(zeroNoGpsSkgPose.hasVerifiedFrameReference).toBe(false);
      expect(adapter.getCamera().originLla).toBeNull();
      expect(tacticalViewport?.getAttribute("data-origin-anchored")).toBe(
        "false",
      );
      expect(
        canvasHost.querySelector('[data-testid="mixed-frame-withheld"]'),
      ).not.toBeNull();
      expect(hudPanel?.getAttribute("data-local-pos")).toBe("measured");
      expect(hudReadout("hud-readout-gs")).toBe("0.0 m/s");
      expect(hudReadout("hud-readout-alt")).toBe("0.0 m");
      expect(overlayTexts).toContain("#1 rover");
      expect(overlayTexts).toContain("0.0m/s · 0.0m");
      expect(overlayTexts).not.toContain("Austin Beacon");
      expect(overlayTexts).not.toContain("SF Beacon");

      nowMs = Date.parse("2026-11-24T14:00:00.250Z");
      telemetryStore.connect("ws://127.0.0.1:8080/ws/telemetry");
      ws1Rig.latest.serverOpen();
      telemetryStore.ingestRawFrame(
        JSON.stringify({
          type: "data",
          subject: "bbops.telemetry.1",
          timestamp: nowMs,
          payload: {
            sysid: 1,
            domain: "rover",
            seq: 1,
            msg_id: 32,
            quat_wxyz: [1, 0, 0, 0],
            has_attitude: true,
            body_rates: [0, 0, 0],
            ned_xyz: [0, 0, 0],
            has_local_pos: true,
            ned_vel: [0, 0, 0],
            lat_lon_alt: [0, 0, 0],
            has_global_pos: false,
            hdg_deg: 0,
            has_heading: true,
            timestamp: nowMs,
          },
        }),
      );
      resetDrawn();
      flushTicks();
      const zeroNoGpsWs1Pose = resolveLiveVehiclePose(
        skgStore.getCommandableVehicle(entityId)!,
        {
          fast: telemetryStore.getKinematics(1),
          telemetryStatus: "connected",
          skgStatus: "connected",
          nowMs,
        },
      );
      expect(zeroNoGpsWs1Pose.source).toBe("ws1");
      expect(zeroNoGpsWs1Pose.hasLocalPos).toBe(true);
      expect(zeroNoGpsWs1Pose.lla).toBeNull();
      expect(zeroNoGpsWs1Pose.hasVerifiedFrameReference).toBe(false);
      expect(hudPanel?.getAttribute("data-pose-source")).toBe("ws1");
      expect(hudReadout("hud-readout-gs")).toBe("0.0 m/s");
      expect(hudReadout("hud-readout-alt")).toBe("0.0 m");
      expect(overlayTexts).toContain("#1 rover");
      expect(overlayTexts).toContain("0.0m/s · 0.0m");

      // Session 4 (15:00:00Z): legacy WS1 local-position lifecycle (omitted optional availability flags).
      telemetryStore.disconnect();
      telemetryStore.connect("ws://127.0.0.1:8080/ws/telemetry");
      ws1Rig.latest.serverOpen();
      nowMs = Date.parse("2026-11-24T15:00:00.000Z");
      skgStore.ingestRawFrame(
        buildMergedPut({
          revision: 9,
          healthIso: "2026-11-24T15:00:00.000Z",
          quat: {
            value: [0, 0, 0, 0],
            measured: false,
            timestamp: "2026-11-24T15:00:00.000Z",
          },
          ned: {
            value: [0, 0, 0],
            measured: false,
            timestamp: "2026-11-24T15:00:00.000Z",
          },
          heading: {
            value: 0,
            measured: false,
            timestamp: "2026-11-24T15:00:00.000Z",
          },
          globalPos: {
            value: [0, 0, 0],
            measured: false,
            timestamp: "2026-11-24T15:00:00.000Z",
          },
        }),
      );

      // 4a. Startup legacy #33 without a local report -> local position unavailable.
      nowMs = Date.parse("2026-11-24T15:00:00.050Z");
      telemetryStore.ingestRawFrame(
        JSON.stringify({
          type: "data",
          subject: "bbops.telemetry.1",
          timestamp: nowMs,
          payload: {
            sysid: 1,
            domain: "rover",
            seq: 1,
            msg_id: 33,
            quat_wxyz: [1, 0, 0, 0],
            body_rates: [0, 0, 0],
            ned_xyz: [0, 0, 0],
            ned_vel: [0, 0, 0],
            lat_lon_alt: [0, 0, 0],
            has_global_pos: false,
            hdg_deg: 0,
            timestamp: nowMs,
          },
        }),
      );
      resetDrawn();
      flushTicks();
      expect(hudPanel?.getAttribute("data-pose-source")).toBe("ws1");
      expect(hudPanel?.getAttribute("data-local-pos")).toBe("unavailable");
      expect(hudReadout("hud-readout-gs")).toBe("--");
      expect(hudReadout("hud-readout-alt")).toBe("--");
      expect(overlayTexts).toEqual([]);

      // 4b. First legacy #32 LOCAL_POSITION_NED ([12, -5, -4], no GPS, omitted optional flags) -> rendered on HUD & map!
      nowMs = Date.parse("2026-11-24T15:00:00.100Z");
      telemetryStore.ingestRawFrame(
        JSON.stringify({
          type: "data",
          subject: "bbops.telemetry.1",
          timestamp: nowMs,
          payload: {
            sysid: 1,
            domain: "rover",
            seq: 2,
            msg_id: 32,
            quat_wxyz: [1, 0, 0, 0],
            body_rates: [0, 0, 0],
            ned_xyz: [12, -5, -4],
            ned_vel: [0, 0, 0],
            lat_lon_alt: [0, 0, 0],
            has_global_pos: false,
            hdg_deg: 0,
            timestamp: nowMs,
          },
        }),
      );
      resetDrawn();
      flushTicks();
      expect(hudPanel?.getAttribute("data-pose-source")).toBe("ws1");
      expect(hudPanel?.getAttribute("data-local-pos")).toBe("measured");
      expect(hudReadout("hud-readout-gs")).toBe("0.0 m/s");
      expect(hudReadout("hud-readout-alt")).toBe("4.0 m");
      expect(overlayTexts).toContain("#1 rover");
      expect(overlayTexts).toContain("0.0m/s · 4.0m");

      // 4c. Following legacy #33 and #31 fused frames preserve local-position availability in that vehicle session.
      for (const [offsetMs, seq, msgId] of [
        [50, 3, 33],
        [100, 4, 31],
      ] as const) {
        nowMs = Date.parse("2026-11-24T15:00:00.100Z") + offsetMs;
        telemetryStore.ingestRawFrame(
          JSON.stringify({
            type: "data",
            subject: "bbops.telemetry.1",
            timestamp: nowMs,
            payload: {
              sysid: 1,
              domain: "rover",
              seq,
              msg_id: msgId,
              quat_wxyz: [1, 0, 0, 0],
              body_rates: [0, 0, 0],
              ned_xyz: [12, -5, -4],
              ned_vel: [0, 0, 0],
              lat_lon_alt: [0, 0, 0],
              has_global_pos: false,
              hdg_deg: 0,
              timestamp: nowMs,
            },
          }),
        );
        resetDrawn();
        flushTicks();
        expect(hudPanel?.getAttribute("data-local-pos")).toBe("measured");
        expect(hudReadout("hud-readout-gs")).toBe("0.0 m/s");
        expect(hudReadout("hud-readout-alt")).toBe("4.0 m");
        expect(overlayTexts).toContain("#1 rover");
        expect(overlayTexts).toContain("0.0m/s · 4.0m");
      }

      // 4d. Legacy #32 at measured zero ([0, 0, 0], no GPS, omitted flags) stays available;
      // 150ms after 4c (.200Z -> .350Z) is gated by 4 Hz throttle, then refreshes at +250ms (.450Z).
      nowMs = Date.parse("2026-11-24T15:00:00.350Z");
      telemetryStore.ingestRawFrame(
        JSON.stringify({
          type: "data",
          subject: "bbops.telemetry.1",
          timestamp: nowMs,
          payload: {
            sysid: 1,
            domain: "rover",
            seq: 5,
            msg_id: 32,
            quat_wxyz: [1, 0, 0, 0],
            body_rates: [0, 0, 0],
            ned_xyz: [0, 0, 0],
            ned_vel: [0, 0, 0],
            lat_lon_alt: [0, 0, 0],
            has_global_pos: false,
            hdg_deg: 0,
            timestamp: nowMs,
          },
        }),
      );
      resetDrawn();
      flushTicks();
      expect(hudPanel?.getAttribute("data-local-pos")).toBe("measured");
      expect(hudReadout("hud-readout-alt")).toBe("4.0 m");
      expect(overlayTexts).toContain("#1 rover");
      expect(overlayTexts).toContain("0.0m/s · 0.0m");

      nowMs = Date.parse("2026-11-24T15:00:00.450Z");
      flushTicks();
      expect(hudReadout("hud-readout-gs")).toBe("0.0 m/s");
      expect(hudReadout("hud-readout-alt")).toBe("0.0 m");

      // 4e. Explicit modern has_local_pos: false remains authoritative even on msg_id=32.
      nowMs = Date.parse("2026-11-24T15:00:00.500Z");
      telemetryStore.ingestRawFrame(
        JSON.stringify({
          type: "data",
          subject: "bbops.telemetry.1",
          timestamp: nowMs,
          payload: {
            sysid: 1,
            domain: "rover",
            seq: 6,
            msg_id: 32,
            quat_wxyz: [1, 0, 0, 0],
            has_attitude: true,
            body_rates: [0, 0, 0],
            ned_xyz: [12, -5, -4],
            has_local_pos: false,
            ned_vel: [0, 0, 0],
            lat_lon_alt: [0, 0, 0],
            has_global_pos: false,
            hdg_deg: 0,
            has_heading: true,
            timestamp: nowMs,
          },
        }),
      );
      resetDrawn();
      flushTicks();
      expect(hudPanel?.getAttribute("data-local-pos")).toBe("unavailable");
      expect(hudReadout("hud-readout-gs")).toBe("--");
      expect(hudReadout("hud-readout-alt")).toBe("--");
      expect(overlayTexts).toEqual([]);

      // 4f. Re-latch via legacy #32, then start a new vehicle session -> local position resets until a new #32 arrives.
      nowMs = Date.parse("2026-11-24T15:00:00.550Z");
      telemetryStore.ingestRawFrame(
        JSON.stringify({
          type: "data",
          subject: "bbops.telemetry.1",
          timestamp: nowMs,
          payload: {
            sysid: 1,
            domain: "rover",
            seq: 7,
            msg_id: 32,
            quat_wxyz: [1, 0, 0, 0],
            body_rates: [0, 0, 0],
            ned_xyz: [6, -3, -2],
            ned_vel: [0, 0, 0],
            lat_lon_alt: [0, 0, 0],
            has_global_pos: false,
            hdg_deg: 0,
            timestamp: nowMs,
          },
        }),
      );
      flushTicks();
      expect(hudPanel?.getAttribute("data-local-pos")).toBe("measured");

      telemetryStore.disconnect();
      telemetryStore.connect("ws://127.0.0.1:8080/ws/telemetry");
      ws1Rig.latest.serverOpen();
      nowMs = Date.parse("2026-11-24T15:00:01.050Z");
      telemetryStore.ingestRawFrame(
        JSON.stringify({
          type: "data",
          subject: "bbops.telemetry.1",
          timestamp: nowMs,
          payload: {
            sysid: 1,
            domain: "rover",
            seq: 1,
            msg_id: 33,
            quat_wxyz: [1, 0, 0, 0],
            body_rates: [0, 0, 0],
            ned_xyz: [0, 0, 0],
            ned_vel: [0, 0, 0],
            lat_lon_alt: [0, 0, 0],
            has_global_pos: false,
            hdg_deg: 0,
            timestamp: nowMs,
          },
        }),
      );
      resetDrawn();
      flushTicks();
      expect(hudPanel?.getAttribute("data-local-pos")).toBe("unavailable");
      expect(hudReadout("hud-readout-gs")).toBe("--");
      expect(hudReadout("hud-readout-alt")).toBe("--");
      expect(overlayTexts).toEqual([]);
    } finally {
      if (canvasApp) unmount(canvasApp);
      if (hudApp) unmount(hudApp);
      telemetryStore.disconnect();
      skgStore.disconnect();
      canvasHost.remove();
      hudHost.remove();
      getCtxSpy.mockRestore();
      rafSpy.mockRestore();
      cafSpy.mockRestore();
    }
  });

  // spec: cop-ui / Full-stack headless browser COP end-to-end verification and visual legibility
  it("gates semantic HUD readout refreshes to 4 Hz (250ms) and matches Fossen Ch. 2 USV crab angle across wraparound and deadband boundaries", () => {
    expect(HUD_READOUT_THROTTLE_MS).toBe(250);
    expect(USV_HUD_CRAB_DEADBAND_MPS).toBe(0.15);

    // Boundary checks on 4 Hz (250ms) throttle gate.
    expect(shouldRefreshHudReadout(null, 1_000, false)).toBe(true);
    expect(shouldRefreshHudReadout(1_000, 1_249, false)).toBe(false);
    expect(shouldRefreshHudReadout(1_000, 1_250, false)).toBe(true);
    expect(shouldRefreshHudReadout(1_000, 1_050, true)).toBe(true);
    expect(shouldRefreshHudReadout(1_000, 999, false)).toBe(true);

    // Generated PRNG + explicit wraparound/deadband sweep checked against computeCrabAngleVectors oracle.
    let state = 0x51a7_2301;
    const nextRand = () => {
      state ^= state << 13;
      state ^= state >>> 17;
      state ^= state << 5;
      return (state >>> 0) / 0x1_0000_0000;
    };

    const explicitCases: Array<{ hdgDeg: number; vn: number; ve: number }> = [
      { hdgDeg: 175, vn: -2.0, ve: -0.35 }, // +/-180 deg wraparound positive crab
      { hdgDeg: -175, vn: -2.0, ve: 0.35 }, // +/-180 deg wraparound negative crab
      { hdgDeg: 100, vn: 0.9, ve: 2.3 }, // portside crab ~ -30.1 deg
      { hdgDeg: 45, vn: 0.1, ve: 0.1 }, // just below 0.15 m/s deadband (0.1414 m/s)
      { hdgDeg: 45, vn: 0.12, ve: 0.1 }, // just above 0.15 m/s deadband (0.1562 m/s)
    ];

    let belowDeadbandHits = 0;
    let activeCrabHits = 0;

    for (let i = 0; i < 120; i += 1) {
      const sample =
        i < explicitCases.length
          ? explicitCases[i]!
          : {
              hdgDeg: nextRand() * 720 - 360,
              vn: nextRand() * 8 - 4,
              ve: nextRand() * 8 - 4,
            };
      const headingRad = (sample.hdgDeg * Math.PI) / 180;
      const pose = {
        source: "ws1" as const,
        stale: false,
        hasAttitude: true,
        hasHeading: true,
        hasLocalPos: true,
        hasVerifiedFrameReference: true,
        quat: eulerRadToQuaternionWxyz(0.05, -0.03, headingRad),
        ned: [12, -8, -1.5] as [number, number, number],
        vel: [sample.vn, sample.ve, -0.2] as [number, number, number],
        headingDeg: sample.hdgDeg,
        headingRad,
        lla: [30.2672, -97.7431, 150] as [number, number, number],
      };

      const oracle = computeCrabAngleVectors(headingRad, sample.vn, sample.ve);
      const boatReadout = formatVehicleHudReadouts(pose, "surface-boat");
      const quadReadout = formatVehicleHudReadouts(pose, "quadcopter");
      const roverReadout = formatVehicleHudReadouts(pose, "rover");

      expect(quadReadout.crabAngleDeg).toBeNull();
      expect(quadReadout.crabText).toBeNull();
      expect(roverReadout.crabAngleDeg).toBeNull();
      expect(roverReadout.crabText).toBeNull();

      if (!oracle.hasCourseSplit) {
        belowDeadbandHits += 1;
        expect(boatReadout.crabAngleDeg).toBeNull();
        expect(boatReadout.crabText).toBeNull();
      } else {
        activeCrabHits += 1;
        const expectedDeg = (oracle.crabAngleRad * 180) / Math.PI;
        expect(boatReadout.crabAngleDeg).not.toBeNull();
        expect(boatReadout.crabAngleDeg!).toBeCloseTo(expectedDeg, 6);
        expect(boatReadout.crabText).toBe(
          `${expectedDeg >= 0 ? "+" : ""}${expectedDeg.toFixed(1)}°`,
        );
      }
    }

    expect(belowDeadbandHits).toBeGreaterThanOrEqual(1);
    expect(activeCrabHits).toBeGreaterThanOrEqual(50);
  });

  // spec: cop-ui / Fleet health readiness, per-vehicle observation freshness, and last-known fault retention
  it("distinguishes stream readiness, LinkTier freshness, partial stream fallbacks, retained stale faults, and explicit ARMED/DISARMED labels", () => {
    expect(VEHICLE_FRESH_DEFAULT_MS).toBe(4_000);
    expect(VEHICLE_FRESH_SUPERVISORY_MS).toBe(11_000);
    expect(resolveVehicleFreshnessThresholdMs("sitl-local")).toBe(4_000);
    expect(resolveVehicleFreshnessThresholdMs("direct-bench")).toBe(4_000);
    expect(resolveVehicleFreshnessThresholdMs("mesh-high-rate")).toBe(4_000);
    expect(resolveVehicleFreshnessThresholdMs("mesh-supervisory")).toBe(11_000);

    expect(formatObservationAge(null)).toBe("age unknown");
    expect(formatObservationAge(450)).toBe("<1s ago");
    expect(formatObservationAge(4_200)).toBe("4s ago");
    expect(formatObservationAge(72_000)).toBe("1m 12s ago");
    expect(formatObservationAge(7_500_000)).toBe("2h 5m ago");
    expect(formatObservationAge(97_200_000)).toBe("1d 3h ago");

    // 1. Pre-sync & empty-fleet states NEVER claim nominal health.
    const preSyncConnecting = summarizeFleetHealth([], {
      skgStatus: "connecting",
      skgSynced: false,
      skgLastSyncedRevision: null,
    });
    expect(preSyncConnecting.state).toBe("connecting");
    expect(preSyncConnecting.badgeLabel).toBe("AWAITING FLEET DATA…");
    expect(preSyncConnecting.badgeLabel).not.toContain("NOMINAL");

    const preSyncReconnecting = summarizeFleetHealth([], {
      skgStatus: "reconnecting",
      skgSynced: false,
      skgLastSyncedRevision: null,
    });
    expect(preSyncReconnecting.state).toBe("connecting");
    expect(preSyncReconnecting.badgeLabel).toBe("RECONNECTING FLEET DATA…");

    const preSyncOffline = summarizeFleetHealth([], {
      skgStatus: "disconnected",
      skgSynced: false,
      skgLastSyncedRevision: null,
    });
    expect(preSyncOffline.state).toBe("unavailable");
    expect(preSyncOffline.badgeLabel).toBe("FLEET DATA UNAVAILABLE");

    // Synced with 0 UXS vehicles (even with an external COP contact) -> "empty", never nominal.
    const syncedEmptyWithContact = summarizeFleetHealth(
      [makeSampleExternalContact()],
      {
        skgStatus: "connected",
        skgSynced: true,
        skgLastSyncedRevision: 0,
      },
    );
    expect(syncedEmptyWithContact.state).toBe("empty");
    expect(syncedEmptyWithContact.badgeLabel).toBe("No vehicles detected");

    // 2. Operator-facing stream pills name Telemetry and Fleet data; WS1/WS2 live in detailLabel.
    const pillsDegraded = summarizeStreamReadiness({
      telemetryStatus: "disconnected",
      telemetryVehicleCount: 0,
      skgStatus: "connected",
      skgSynced: true,
      skgLastSyncedRevision: 4,
      skgEntityCount: 2,
    });
    expect(pillsDegraded.telemetry.label).toBe("Telemetry");
    expect(pillsDegraded.telemetry.stateLabel).toBe("Offline");
    expect(pillsDegraded.telemetry.detailLabel).toContain("WS1");
    expect(pillsDegraded.fleetData.label).toBe("Fleet data");
    expect(pillsDegraded.fleetData.stateLabel).toBe("Live · synced (2)");
    expect(pillsDegraded.fleetData.detailLabel).toContain("WS2");

    // 3. LinkTier scaling + partial stream degradation + explicit ARMED / DISARMED labels.
    let nowMs = Date.parse("2026-11-25T15:00:00.000Z");
    const highRateVehicle = makeSampleVehicle({
      id: "boxbotix.local.robotics.uxs.vehicle.1",
      sysid: 1,
      armed: true,
      linkTier: "mesh-high-rate",
      observedAt: new Date(nowMs).toISOString(),
      stateObservedAt: new Date(nowMs).toISOString(),
      stateIngestedAtMs: nowMs,
      stateIngestedLive: true,
    });
    const supervisoryDisarmed = makeSampleVehicle({
      id: "boxbotix.local.robotics.uxs.vehicle.2",
      sysid: 2,
      title: "Boat-2",
      domain: "surface-boat",
      armed: false,
      linkTier: "mesh-supervisory",
      observedAt: new Date(nowMs).toISOString(),
      stateObservedAt: new Date(nowMs).toISOString(),
      stateIngestedAtMs: nowMs,
      stateIngestedLive: true,
    });

    // Telemetry (WS1) offline while Fleet data (WS2) is connected+synced & fresh -> FLEET NOMINAL (2)!
    const ws1OfflineFleetNominal = summarizeFleetHealth(
      [highRateVehicle, supervisoryDisarmed],
      {
        telemetryStatus: "disconnected",
        skgStatus: "connected",
        skgSynced: true,
        skgLastSyncedRevision: 2,
        nowMs: nowMs + 2_000,
      },
    );
    expect(ws1OfflineFleetNominal.state).toBe("nominal");
    expect(ws1OfflineFleetNominal.badgeLabel).toBe("FLEET NOMINAL (2)");

    // Explicit ARMED / DISARMED across banner and filmstrip (never "SAFE").
    const liveStrip = buildFilmstripItems(
      [highRateVehicle, supervisoryDisarmed],
      [],
      undefined,
      {
        skgStatus: "connected",
        skgSynced: true,
        nowMs: nowMs + 2_000,
      },
    );
    expect(liveStrip[0]?.secondaryLabel).toBe("#1 · P2 · ARMED");
    expect(liveStrip[0]?.safetySeverity).toBe("success");
    expect(liveStrip[1]?.secondaryLabel).toBe("#2 · P2 · DISARMED");
    expect(liveStrip[1]?.safetySeverity).toBe("nominal");

    // Advance +9s: supervisory (#2, 11s threshold) is still fresh while high-rate (#1, 4s threshold) is stale!
    const atNineSec = nowMs + 9_000;
    const highRateFreshAt9s = evaluateVehicleFreshness(highRateVehicle, {
      skgStatus: "connected",
      skgSynced: true,
      nowMs: atNineSec,
    });
    const supervisoryFreshAt9s = evaluateVehicleFreshness(supervisoryDisarmed, {
      skgStatus: "connected",
      skgSynced: true,
      nowMs: atNineSec,
    });
    expect(highRateFreshAt9s.fresh).toBe(false);
    expect(highRateFreshAt9s.ageLabel).toBe("9s ago");
    expect(supervisoryFreshAt9s.fresh).toBe(true);

    const partialStaleSummary = summarizeFleetHealth(
      [highRateVehicle, supervisoryDisarmed],
      {
        skgStatus: "connected",
        skgSynced: true,
        skgLastSyncedRevision: 2,
        nowMs: atNineSec,
      },
    );
    expect(partialStaleSummary.state).toBe("stale");
    expect(partialStaleSummary.badgeLabel).toBe(
      "FLEET STALE (1/2 LAST KNOWN · 9s ago)",
    );

    // Stale fault-free ARMED vehicle (#1) withholds live green ("success") -> "warning" + LAST KNOWN.
    const staleArmedBanner = selectBannerPresentation(highRateVehicle, {
      skgStatus: "connected",
      skgSynced: true,
      nowMs: atNineSec,
    });
    expect(staleArmedBanner?.kind).toBe("uxs-vehicle");
    if (staleArmedBanner?.kind === "uxs-vehicle") {
      expect(staleArmedBanner.armedBadge.label).toBe("ARMED");
      expect(staleArmedBanner.armedBadge.severity).toBe("warning");
      expect(staleArmedBanner.freshness?.statusLabel).toBe(
        "LAST KNOWN · 9s ago",
      );
    }
    const staleStrip = buildFilmstripItems(
      [highRateVehicle, supervisoryDisarmed],
      [],
      undefined,
      { skgStatus: "connected", skgSynced: true, nowMs: atNineSec },
    );
    expect(staleStrip[0]?.secondaryLabel).toBe(
      "#1 · P2 · ARMED · LAST KNOWN (9s ago)",
    );
    expect(staleStrip[0]?.safetySeverity).toBe("warning");
    expect(
      resolveVehicleSafetyTone(highRateVehicle, { isStale: true }).tone,
    ).toBe("warning");

    // 4. Fleet data (WS2) drops while Telemetry (WS1) streams fresh 20 Hz pose:
    // pose stays live ("ws1", stale: false), while fleet health downgrades to "stale".
    nowMs = atNineSec + 1_000;
    const telemetryStore = createTelemetryStore({ now: () => nowMs });
    telemetryStore.ingestRawFrame(
      JSON.stringify({
        type: "data",
        subject: "bbops.telemetry.1",
        timestamp: nowMs,
        payload: {
          sysid: 1,
          domain: "quadcopter",
          seq: 1,
          msg_id: 32,
          quat_wxyz: [1, 0, 0, 0],
          has_attitude: true,
          body_rates: [0, 0, 0],
          ned_xyz: [12, -5, -20],
          has_local_pos: true,
          ned_vel: [1, 0, 0],
          lat_lon_alt: [30.2672, -97.7431, 168],
          has_global_pos: true,
          hdg_deg: 45,
          timestamp: nowMs,
        },
      }),
    );
    const liveWs1PoseUnderWs2Drop = resolveLiveVehiclePose(highRateVehicle, {
      fast: telemetryStore.getKinematics(1),
      telemetryLink: telemetryStore.getLinkState(1),
      telemetryStatus: "connected",
      skgStatus: "reconnecting",
      nowMs: nowMs + 100,
    });
    expect(liveWs1PoseUnderWs2Drop.source).toBe("ws1");
    expect(liveWs1PoseUnderWs2Drop.stale).toBe(false);

    const ws2DropSummary = summarizeFleetHealth([highRateVehicle], {
      telemetryStatus: "connected",
      getTelemetryLink: (sysid) => telemetryStore.getLinkState(sysid),
      skgStatus: "reconnecting",
      skgSynced: false,
      skgLastSyncedRevision: 2,
      nowMs: nowMs + 100,
    });
    expect(ws2DropSummary.state).toBe("stale");
    expect(ws2DropSummary.badgeLabel).toContain("FLEET STALE · LAST KNOWN");

    // 5. Retained faults survive link_lost with LAST KNOWN age and do not clear on link loss.
    const faultedP4 = makeSampleVehicle({
      authority: "mcu-failsafe",
      observedAt: new Date(nowMs).toISOString(),
      stateObservedAt: new Date(nowMs).toISOString(),
      stateIngestedAtMs: nowMs,
      stateIngestedLive: true,
    });
    nowMs += 6_000;
    telemetryStore.ingestRawFrame(
      JSON.stringify({
        type: "data",
        subject: "bbops.telemetry.events.1",
        timestamp: nowMs,
        payload: {
          sysid: 1,
          entity_id: "boxbotix.local.robotics.uxs.vehicle.1",
          domain: "quadcopter",
          seq: 2,
          msg_id: 0,
          kind: "link_lost",
          reasons: ["link_timeout"],
          armed: true,
          authority: "mcu-failsafe",
          companion_mode: "teleop-velocity",
          link_tier: "mesh-high-rate",
          flags: {
            clock_sync_locked: true,
            teleop_deadman_clamped: false,
            crsf_link_healthy: true,
            companion_trunk_healthy: true,
          },
          failsafes: 0,
          timestamp: nowMs,
        },
      }),
    );
    expect(telemetryStore.malformedFrameCount).toBe(0);
    expect(telemetryStore.getLinkState(1)?.linkLost).toBe(true);
    const retainedFaultSummary = summarizeFleetHealth([faultedP4], {
      telemetryStatus: "connected",
      getTelemetryLink: (sysid) => telemetryStore.getLinkState(sysid),
      skgStatus: "connected",
      skgSynced: true,
      skgLastSyncedRevision: 3,
      nowMs,
    });
    expect(retainedFaultSummary.state).toBe("fault");
    expect(retainedFaultSummary.faultedCount).toBe(1);
    expect(retainedFaultSummary.hasStaleFaultContext).toBe(true);
    expect(retainedFaultSummary.badgeLabel).toBe(
      "! 1 FLEET FAULT (UXS #1 · P4) · LAST KNOWN 6s ago",
    );
  });

  // spec: cop-ui / Fleet health readiness, per-vehicle observation freshness, and last-known fault retention
  it("matches an independent reference model across 50 seeded multi-vehicle connection, link-loss, replay, and recovery histories", () => {
    interface RefSkgVehicle {
      sysid: number;
      linkTier: "mesh-high-rate" | "mesh-supervisory";
      armed: boolean;
      fault: "none" | "p4" | "p1";
      lastSkgObsMs: number;
    }

    interface RefWs1Vehicle {
      lastWs1PacketMs: number | null;
      linkLost: boolean;
      linkLostAtMs: number | null;
    }

    const reach = {
      connecting: 0,
      unavailable: 0,
      empty: 0,
      nominal: 0,
      stale: 0,
      faultLive: 0,
      faultLastKnown: 0,
      supervisoryFreshPast4s: 0,
      reconnectReplayStillStale: 0,
    };

    const mkEntityPut = (
      revision: number,
      v: RefSkgVehicle,
      obsMs: number,
    ): string => {
      const entityId = `boxbotix.local.robotics.uxs.vehicle.${v.sysid}`;
      const ts = new Date(obsMs).toISOString();
      const authority =
        v.fault === "p4"
          ? "mcu-failsafe"
          : v.fault === "p1"
            ? "direct-crsf-override"
            : "companion-teleop";
      const mk = (predicate: string, object: unknown): SkgTriple => ({
        subject: entityId,
        predicate,
        object,
        source: "mavlink_0xb1",
        timestamp: ts,
        confidence: 1,
      });
      return JSON.stringify({
        type: "put",
        bucket: "ENTITY_STATES",
        key: entityId,
        revision,
        payload: {
          id: entityId,
          updated_at: ts,
          triples: [
            mk(
              "uxs.domain.kind",
              v.sysid === 2 ? "surface-boat" : "quadcopter",
            ),
            mk("uxs.authority.state", authority),
            mk("uxs.mode.companion", "teleop-velocity"),
            mk("uxs.link.tier", v.linkTier),
            mk("uxs.health.armed", v.armed),
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
            mk("uxs.kinematics.ned", [v.sysid * 5, 0, -10]),
            mk("uxs.kinematics.vx", 1),
            mk("uxs.kinematics.vy", 0),
            mk("uxs.kinematics.vz", 0),
            mk("uxs.kinematics.headingdeg", 90),
            mk("geo.location.latitude", 30.2672),
            mk("geo.location.longitude", -97.7431),
            mk("geo.location.altitude", 160),
          ],
        },
      });
    };

    const mkLinkEventFrame = (
      sysid: number,
      kind: "link_lost" | "link_restored",
      seq: number,
      timestamp: number,
    ): string =>
      JSON.stringify({
        type: "data",
        subject: `bbops.telemetry.events.${sysid}`,
        timestamp,
        payload: {
          sysid,
          entity_id: `boxbotix.local.robotics.uxs.vehicle.${sysid}`,
          domain: sysid === 2 ? "surface-boat" : "quadcopter",
          seq: seq & 0xff,
          msg_id: 0,
          kind,
          reasons: [kind],
          armed: false,
          authority: "companion-teleop",
          companion_mode: "teleop-velocity",
          link_tier: sysid === 2 ? "mesh-supervisory" : "mesh-high-rate",
          flags: {
            clock_sync_locked: true,
            teleop_deadman_clamped: false,
            crsf_link_healthy: true,
            companion_trunk_healthy: true,
          },
          failsafes: 0,
          timestamp,
        },
      });

    for (let seed = 1; seed <= 50; seed += 1) {
      let rng = (seed * 0x9e37_79b9) >>> 0;
      const nextU32 = () => {
        rng ^= rng << 13;
        rng ^= rng >>> 17;
        rng ^= rng << 5;
        return rng >>> 0;
      };
      const pick = <T>(arr: readonly T[]): T =>
        arr[nextU32() % arr.length] as T;

      let nowMs = Date.parse("2026-11-25T16:00:00.000Z");
      const ws1Rig = new MockWebSocketRig();
      const ws2Rig = new MockWebSocketRig();
      const telemetryStore = createTelemetryStore({
        url: "ws://127.0.0.1:8080/ws/telemetry",
        now: () => nowMs,
        createSocket: ws1Rig.createSocket,
        initialBackoffMs: 60_000,
        maxBackoffMs: 60_000,
      });
      const skgStore = createSkgStore({
        url: "ws://127.0.0.1:8080/ws/graph",
        now: () => nowMs,
        createSocket: ws2Rig.createSocket,
        initialBackoffMs: 60_000,
        maxBackoffMs: 60_000,
      });

      let ws1Status:
        "disconnected" | "connecting" | "connected" | "reconnecting" =
        "disconnected";
      let ws2Status:
        "disconnected" | "connecting" | "connected" | "reconnecting" =
        "disconnected";
      let ws2Synced = false;
      let hasEverSynced = false;
      let kvRev = 0;

      const serverSkg = new Map<number, RefSkgVehicle>();
      let visibleSkg = new Map<number, RefSkgVehicle>();
      const browserWs1 = new Map<number, RefWs1Vehicle>();

      const cloneSkgMap = (src: Map<number, RefSkgVehicle>) => {
        const out = new Map<number, RefSkgVehicle>();
        for (const [k, v] of src) {
          out.set(k, { ...v });
        }
        return out;
      };

      for (let step = 0; step < 60; step += 1) {
        const action = nextU32() % 10;
        let wasReconnectReplay = false;

        if (action === 0) {
          nowMs += 1_000 + (nextU32() % 6_001);
        } else if (action === 1) {
          if (ws1Status === "disconnected") {
            telemetryStore.connect();
            ws1Status = "connecting";
          } else if (ws1Status === "connecting") {
            ws1Rig.latest.serverOpen();
            ws1Status = "connected";
          } else if (ws1Status === "reconnecting") {
            telemetryStore.connect();
            ws1Rig.latest.serverOpen();
            ws1Status = "connected";
          } else if (nextU32() % 2 === 0) {
            ws1Rig.latest.serverDrop(1006, "drop");
            ws1Status = "reconnecting";
          } else {
            telemetryStore.disconnect();
            ws1Status = "disconnected";
          }
        } else if (action === 2) {
          if (ws2Status === "disconnected") {
            skgStore.connect();
            ws2Status = "connecting";
            ws2Synced = false;
          } else if (ws2Status === "connecting") {
            ws2Rig.latest.serverOpen();
            ws2Status = "connected";
            ws2Synced = false;
          } else if (ws2Status === "reconnecting") {
            skgStore.connect();
            ws2Rig.latest.serverOpen();
            ws2Status = "connected";
            ws2Synced = false;
          } else if (nextU32() % 2 === 0) {
            ws2Rig.latest.serverDrop(1006, "drop");
            ws2Status = "reconnecting";
            ws2Synced = false;
          } else {
            skgStore.disconnect();
            ws2Status = "disconnected";
            ws2Synced = false;
          }
        } else if (action === 3) {
          if (ws2Status === "connected" && !ws2Synced) {
            const hadPriorSync = hasEverSynced;
            for (const v of serverSkg.values()) {
              kvRev += 1;
              ws2Rig.latest.serverMessage(
                mkEntityPut(kvRev, v, v.lastSkgObsMs),
              );
            }
            if (nextU32() % 3 === 0) {
              kvRev += 1;
              ws2Rig.latest.serverMessage(
                JSON.stringify({
                  type: "put",
                  bucket: "ENTITY_STATES",
                  key: "boxbotix.local.tak.cot.contact.bravo",
                  revision: kvRev,
                  payload: {
                    id: "boxbotix.local.tak.cot.contact.bravo",
                    triples: [
                      {
                        subject: "boxbotix.local.tak.cot.contact.bravo",
                        predicate: "cop.contact.type",
                        object: "a-f-G",
                        source: "cot",
                        timestamp: new Date(nowMs).toISOString(),
                        confidence: 1,
                      },
                    ],
                  },
                }),
              );
            }
            kvRev += 1;
            ws2Rig.latest.serverMessage(
              JSON.stringify({
                type: "synced",
                bucket: "ENTITY_STATES",
                revision: kvRev,
              }),
            );
            ws2Synced = true;
            hasEverSynced = true;
            visibleSkg = cloneSkgMap(serverSkg);
            wasReconnectReplay = hadPriorSync;
          }
        } else if (action === 4 || action === 5) {
          const sysid = pick([1, 2] as const);
          nowMs += 5;
          const updated: RefSkgVehicle = {
            sysid,
            linkTier:
              sysid === 2
                ? "mesh-supervisory"
                : pick(["mesh-high-rate", "mesh-supervisory"] as const),
            armed: (nextU32() & 1) === 1,
            fault: pick(["none", "none", "none", "p1", "p4"] as const),
            lastSkgObsMs: nowMs,
          };
          serverSkg.set(sysid, updated);
          if (ws2Status === "connected" && ws2Synced) {
            kvRev += 1;
            ws2Rig.latest.serverMessage(mkEntityPut(kvRev, updated, nowMs));
            visibleSkg.set(sysid, { ...updated });
          }
        } else if (action === 6) {
          const sysid = pick([1, 2] as const);
          if (ws1Status === "connected" && serverSkg.has(sysid)) {
            nowMs += 5;
            telemetryStore.ingestRawFrame(
              JSON.stringify({
                type: "data",
                subject: `bbops.telemetry.${sysid}`,
                timestamp: nowMs,
                payload: {
                  sysid,
                  domain: sysid === 2 ? "surface-boat" : "quadcopter",
                  seq: (step + 1) & 0xff,
                  msg_id: 32,
                  quat_wxyz: [1, 0, 0, 0],
                  has_attitude: true,
                  body_rates: [0, 0, 0],
                  ned_xyz: [10, 0, -5],
                  has_local_pos: true,
                  ned_vel: [1, 0, 0],
                  lat_lon_alt: [30.2672, -97.7431, 160],
                  has_global_pos: true,
                  hdg_deg: 0,
                  timestamp: nowMs,
                },
              }),
            );
            browserWs1.set(sysid, {
              lastWs1PacketMs: nowMs,
              linkLost: false,
              linkLostAtMs: null,
            });
          }
        } else if (action === 7) {
          const sysid = pick([1, 2] as const);
          if (ws1Status === "connected" && serverSkg.has(sysid)) {
            nowMs += 10;
            telemetryStore.ingestRawFrame(
              mkLinkEventFrame(sysid, "link_lost", step + 1, nowMs),
            );
            const prevW1 = browserWs1.get(sysid);
            browserWs1.set(sysid, {
              lastWs1PacketMs: prevW1?.lastWs1PacketMs ?? null,
              linkLost: true,
              linkLostAtMs: nowMs,
            });
          }
        } else if (action === 8) {
          const sysid = pick([1, 2] as const);
          const prevW1 = browserWs1.get(sysid);
          if (ws1Status === "connected" && prevW1?.linkLost) {
            nowMs += 10;
            telemetryStore.ingestRawFrame(
              mkLinkEventFrame(sysid, "link_restored", step + 1, nowMs),
            );
            browserWs1.set(sysid, {
              lastWs1PacketMs: nowMs,
              linkLost: false,
              linkLostAtMs: null,
            });
          }
        } else {
          const sysid = pick([1, 2] as const);
          if (ws2Status === "connected" && ws2Synced && serverSkg.has(sysid)) {
            serverSkg.delete(sysid);
            visibleSkg.delete(sysid);
            kvRev += 1;
            ws2Rig.latest.serverMessage(
              JSON.stringify({
                type: "delete",
                bucket: "ENTITY_STATES",
                key: `boxbotix.local.robotics.uxs.vehicle.${sysid}`,
                revision: kvRev,
              }),
            );
          }
        }

        expect(telemetryStore.malformedFrameCount).toBe(0);
        expect(skgStore.malformedFrameCount).toBe(0);

        const fleetLive = ws2Status === "connected" && ws2Synced;
        let refFaultCount = 0;
        let refStaleCount = 0;
        let refAnyFaultStale = false;

        for (const v of visibleSkg.values()) {
          const threshold = v.linkTier === "mesh-supervisory" ? 11_000 : 4_000;
          const w1 = browserWs1.get(v.sysid);
          const effectiveLinkLost =
            Boolean(w1?.linkLost) &&
            !(
              w1?.linkLostAtMs !== null &&
              w1?.linkLostAtMs !== undefined &&
              v.lastSkgObsMs > w1.linkLostAtMs
            );
          const skgFresh = fleetLive && nowMs - v.lastSkgObsMs <= threshold;
          const ws1Fresh =
            ws1Status === "connected" &&
            !effectiveLinkLost &&
            w1?.lastWs1PacketMs !== null &&
            w1?.lastWs1PacketMs !== undefined &&
            nowMs - w1.lastWs1PacketMs <= threshold;
          const isFresh =
            fleetLive && !effectiveLinkLost && (skgFresh || ws1Fresh);

          if (
            isFresh &&
            v.linkTier === "mesh-supervisory" &&
            nowMs - v.lastSkgObsMs > 4_000 &&
            (w1?.lastWs1PacketMs === null ||
              w1?.lastWs1PacketMs === undefined ||
              nowMs - w1.lastWs1PacketMs > 4_000)
          ) {
            reach.supervisoryFreshPast4s += 1;
          }

          if (!isFresh) {
            refStaleCount += 1;
          }
          if (v.fault !== "none") {
            refFaultCount += 1;
            if (!isFresh) {
              refAnyFaultStale = true;
            }
          }
        }

        const expectedState =
          refFaultCount > 0
            ? "fault"
            : !hasEverSynced
              ? ws2Status === "disconnected"
                ? "unavailable"
                : "connecting"
              : visibleSkg.size === 0
                ? fleetLive
                  ? "empty"
                  : ws2Status === "disconnected"
                    ? "unavailable"
                    : "connecting"
                : !fleetLive || refStaleCount > 0
                  ? "stale"
                  : "nominal";

        const context = {
          telemetryStatus: telemetryStore.status,
          getTelemetryLink: (s: number) => telemetryStore.getLinkState(s),
          skgStatus: skgStore.status,
          skgSynced: skgStore.synced,
          skgLastSyncedRevision: skgStore.lastSyncedRevision,
          nowMs,
        };
        const actual = summarizeFleetHealth(
          skgStore.entities.values(),
          context,
        );

        expect(actual.state, `seed=${seed} step=${step}`).toBe(expectedState);
        expect(
          actual.badgeLabel.startsWith("FLEET NOMINAL"),
          `seed=${seed} step=${step}`,
        ).toBe(expectedState === "nominal");
        expect(actual.faultedCount, `seed=${seed} step=${step}`).toBe(
          refFaultCount,
        );

        if (expectedState === "connecting") reach.connecting += 1;
        if (expectedState === "unavailable") reach.unavailable += 1;
        if (expectedState === "empty") reach.empty += 1;
        if (expectedState === "nominal") reach.nominal += 1;
        if (expectedState === "stale") {
          reach.stale += 1;
          if (wasReconnectReplay) {
            reach.reconnectReplayStillStale += 1;
          }
        }
        if (expectedState === "fault") {
          const expectedStaleFault = !fleetLive || refAnyFaultStale;
          expect(actual.hasStaleFaultContext, `seed=${seed} step=${step}`).toBe(
            expectedStaleFault,
          );
          if (expectedStaleFault) {
            reach.faultLastKnown += 1;
            expect(actual.badgeLabel).toContain("LAST KNOWN");
          } else {
            reach.faultLive += 1;
          }
        }

        const strip = selectFilmstripItems(skgStore, context);
        for (const item of strip) {
          expect(item.secondaryLabel).not.toContain("SAFE");
          if (item.kind === "uxs-vehicle" && item.stale) {
            expect(item.safetySeverity).not.toBe("success");
          }
        }
      }

      telemetryStore.disconnect();
      skgStore.disconnect();
    }

    expect(reach.connecting).toBeGreaterThanOrEqual(10);
    expect(reach.unavailable).toBeGreaterThanOrEqual(10);
    expect(reach.empty).toBeGreaterThanOrEqual(5);
    expect(reach.nominal).toBeGreaterThanOrEqual(10);
    expect(reach.stale).toBeGreaterThanOrEqual(10);
    expect(reach.faultLive).toBeGreaterThanOrEqual(10);
    expect(reach.faultLastKnown).toBeGreaterThanOrEqual(10);
    expect(reach.supervisoryFreshPast4s).toBeGreaterThanOrEqual(5);
    expect(reach.reconnectReplayStillStale).toBeGreaterThanOrEqual(5);
  });
});
