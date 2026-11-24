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
      // Mount reactive wrapper driving VehicleHud from live skgStore vehicle lookup.
      const mountHudForCurrentVehicle = (): void => {
        if (hudApp) {
          unmount(hudApp);
        }
        hudHost.innerHTML = "";
        hudApp = mount(VehicleHud, {
          target: hudHost,
          props: {
            telemetryStore,
            vehicle: skgStore.getCommandableVehicle(entityId),
            skgStatus: skgStore.status,
            now: () => nowMs,
          },
        });
        flushSync();
      };

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
      mountHudForCurrentVehicle();

      // Step 1: 1 WS1 frame arrives at +100ms -> WS1 wins on both HUD and canvas.
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
      mountHudForCurrentVehicle();
      drawnTexts.length = 0;
      flushAnimationTicks();

      const readout1 = hudHost.querySelector('[data-testid="hud-readout"]');
      expect(readout1?.getAttribute("data-pose-source")).toBe("ws1");
      expect(readout1?.getAttribute("data-stale")).toBe("false");
      expect(readout1?.textContent).toContain("HDG 045°");
      expect(readout1?.textContent).toContain("ALT 10.0m");
      expect(adapter.getCamera().centerNed).toEqual([12, -6]);

      // Step 2: WS1 disconnects while WS2 stays open and delivers 2 distinct newer poses.
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
      mountHudForCurrentVehicle();
      drawnTexts.length = 0;
      flushAnimationTicks();

      const readout2 = hudHost.querySelector('[data-testid="hud-readout"]');
      expect(readout2?.getAttribute("data-pose-source")).toBe("skg");
      expect(readout2?.getAttribute("data-stale")).toBe("false");
      expect(readout2?.textContent).toContain("HDG 120°");
      expect(readout2?.textContent).toContain("ALT 22.0m");
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
      mountHudForCurrentVehicle();
      drawnTexts.length = 0;
      flushAnimationTicks();

      const readout3 = hudHost.querySelector('[data-testid="hud-readout"]');
      expect(readout3?.getAttribute("data-pose-source")).toBe("skg");
      expect(readout3?.getAttribute("data-stale")).toBe("false");
      expect(readout3?.textContent).toContain("HDG 215°");
      expect(readout3?.textContent).toContain("ALT 34.0m");
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
      expect(
        hudHost
          .querySelector('[data-testid="hud-readout"]')
          ?.getAttribute("data-stale"),
      ).toBe("true");
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
  it("preserves #33 GLOBAL_POSITION_INT heading before #31 ATTITUDE_QUATERNION arrives and shows attitude unavailable without false level flight", () => {
    const nowMs = Date.parse("2026-11-24T12:05:00.000Z");
    const telemetryStore = createTelemetryStore({ now: () => nowMs });
    const skgStore = createSkgStore({ now: () => nowMs });

    const preAttitudeVehicle = makeSampleVehicle({
      attitudeQuaternion: [0, 0, 0, 0],
      headingDeg: 90,
      ned: [0, 0, -15],
      velocityNed: [0, 5, 0],
      observedAt: new Date(nowMs).toISOString(),
      ingestedAtMs: nowMs,
    });
    skgStore.entities.set(preAttitudeVehicle.id, preAttitudeVehicle);

    // Even a legacy [1, 0, 0, 0] SKG snapshot whose heading is 90 deg must be flagged unmeasured.
    const legacyIdentityPose = resolveLiveVehiclePose(
      makeSampleVehicle({
        attitudeQuaternion: [1, 0, 0, 0],
        headingDeg: 90,
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
      telemetryStatus: "connected",
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
          vehicle: preAttitudeVehicle,
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

      // Pre-#31 assertions: 90 deg East heading honored; attitude marked unavailable.
      expect(
        hudHost.querySelector('[data-testid="hud-attitude-unavailable"]')
          ?.textContent,
      ).toContain("NO ATT");
      const readout = hudHost.querySelector('[data-testid="hud-readout"]');
      expect(readout?.getAttribute("data-attitude")).toBe("unavailable");
      expect(readout?.textContent).toContain("HDG 090°");
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

      // Now deliver genuine #31 ATTITUDE_QUATERNION level North ([1, 0, 0, 0], has_attitude: true).
      telemetryStore.ingestRawFrame(
        JSON.stringify({
          type: "data",
          subject: "bbops.telemetry.1",
          timestamp: nowMs + 50,
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
            timestamp: nowMs + 50,
          },
        }),
      );
      unmount(hudApp);
      hudHost.innerHTML = "";
      drawnTexts.length = 0;
      hudApp = mount(VehicleHud, {
        target: hudHost,
        props: {
          telemetryStore,
          vehicle: makeSampleVehicle({
            attitudeQuaternion: [1, 0, 0, 0],
            headingDeg: 0,
            ingestedAtMs: nowMs + 50,
          }),
          skgStatus: "connected",
          now: () => nowMs + 50,
        },
      });
      flushSync();
      for (const cb of rafCallbacks.splice(0, rafCallbacks.length)) {
        cb(nowMs + 50);
      }
      flushSync();

      expect(
        hudHost.querySelector('[data-testid="hud-attitude-unavailable"]'),
      ).toBeNull();
      expect(
        hudHost
          .querySelector('[data-testid="hud-readout"]')
          ?.getAttribute("data-attitude"),
      ).toBe("measured");
      expect(drawnTexts.some((t) => t.text === "R 0° P 0° Y 0°")).toBe(true);
    } finally {
      if (hudApp) {
        unmount(hudApp);
      }
      if (canvasApp) {
        unmount(canvasApp);
      }
      hudHost.remove();
      canvasHost.remove();
      getCtxSpy.mockRestore();
      rafSpy.mockRestore();
      cafSpy.mockRestore();
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
});
