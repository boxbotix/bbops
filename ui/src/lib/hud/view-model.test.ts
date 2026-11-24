// SPDX-License-Identifier: MIT
// Copyright (c) 2026 boxbotix

import { flushSync, mount, unmount } from "svelte";
import { describe, expect, it } from "vitest";
import AuthorityBanner from "../components/AuthorityBanner.svelte";
import type {
  ExternalContactEntityState,
  VehicleEntityState,
} from "../stores/skg.svelte";
import {
  buildExternalContactSummary,
  buildHealthStrip,
  buildVehicleBannerView,
  computeFlightMetrics,
  formatArmedBadge,
  formatAuthorityBadge,
  formatLinkTierBadge,
  quaternionToEulerDeg,
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
});
