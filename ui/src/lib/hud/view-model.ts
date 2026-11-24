// SPDX-License-Identifier: MIT
// Copyright (c) 2026 boxbotix

import {
  type ControlAuthority,
  type HealthFlags,
  type LinkTier,
  isFiniteNumber,
} from "../uxs";
import type {
  CopEntityState,
  CopFeedStatus,
  ExternalContactEntityState,
  GeoLocation,
  VehicleEntityState,
  VehicleSysStatus,
} from "../stores/skg.svelte";

const RAD_TO_DEG = 180 / Math.PI;

export type SafetyBadgeSeverity = "error" | "warning" | "success" | "nominal";

export interface EulerAttitudeDeg {
  readonly rollDeg: number;
  readonly pitchDeg: number;
  readonly yawDeg: number;
  readonly rollRad: number;
  readonly pitchRad: number;
  readonly yawRad: number;
}

export interface FlightMetricsView {
  readonly groundspeedMps: number;
  readonly climbRateMps: number;
  readonly altitudeRelM: number;
}

export interface AuthorityBadgeView {
  readonly code: "P1" | "P2" | "P3" | "P4";
  readonly authority: ControlAuthority;
  readonly label:
    | "P1 DirectCrsfOverride"
    | "P2 CompanionTeleop"
    | "P3 CompanionAutonomous"
    | "P4 McuFailsafe";
  readonly severity: SafetyBadgeSeverity;
}

export interface LinkTierBadgeView {
  readonly tier: LinkTier;
  readonly label: "SITL" | "Bench" | "HaLow 30Hz" | "LoRa 1Hz";
}

export interface ArmedBadgeView {
  readonly armed: boolean;
  readonly label: "ARMED" | "DISARMED";
  readonly severity: SafetyBadgeSeverity;
}

export interface HealthChipView {
  readonly key: string;
  readonly label: string;
  readonly value: string;
  readonly severity: SafetyBadgeSeverity;
}

export interface VehicleBannerView {
  readonly kind: "uxs-vehicle";
  readonly entityId: string;
  readonly sysid: number;
  readonly domain: VehicleEntityState["domain"];
  readonly poisoned: boolean;
  readonly poisonError: string | null;
  readonly authorityBadge: AuthorityBadgeView;
  readonly armedBadge: ArmedBadgeView;
  readonly linkBadge: LinkTierBadgeView;
  readonly healthStrip: readonly HealthChipView[];
}

export interface ExternalContactSummaryView {
  readonly kind: "external-contact";
  readonly entityId: string;
  readonly title: string;
  readonly contactType: string;
  readonly source: string;
  readonly observedAt: string;
  readonly receivedAt: string;
  readonly expiresAt: string;
  readonly uncertaintyMeters: number | null;
  readonly uncertaintyLabel: string;
  readonly feedStatus: CopFeedStatus;
  readonly location: GeoLocation | null;
}

function cleanZero(value: number): number {
  return Object.is(value, -0) ? 0 : value;
}

/**
 * Extracts ZYX (3-2-1, NED-to-FRD, Thor I. Fossen §2.2.2) Euler roll, pitch,
 * and [0, 360) yaw from a Hamilton unit quaternion [w, x, y, z].
 * Clamps the pitch sine argument into [-1, 1] to prevent NaN at +/-90 deg pitch.
 */
export function quaternionToEulerDeg(
  quatWxyz: readonly [number, number, number, number],
): EulerAttitudeDeg {
  const [rawW, rawX, rawY, rawZ] = quatWxyz;
  if (
    !isFiniteNumber(rawW) ||
    !isFiniteNumber(rawX) ||
    !isFiniteNumber(rawY) ||
    !isFiniteNumber(rawZ)
  ) {
    return {
      rollDeg: 0,
      pitchDeg: 0,
      yawDeg: 0,
      rollRad: 0,
      pitchRad: 0,
      yawRad: 0,
    };
  }
  const norm = Math.hypot(rawW, rawX, rawY, rawZ);
  const [w, x, y, z] =
    norm > 1e-9
      ? [rawW / norm, rawX / norm, rawY / norm, rawZ / norm]
      : [1, 0, 0, 0];

  const sinRollCosPitch = 2 * (w * x + y * z);
  const cosRollCosPitch = 1 - 2 * (x * x + y * y);
  const rollRad = cleanZero(Math.atan2(sinRollCosPitch, cosRollCosPitch));

  const sinPitchRaw = 2 * (w * y - z * x);
  const sinPitchClamped = Math.max(-1, Math.min(1, sinPitchRaw));
  const pitchRad = cleanZero(Math.asin(sinPitchClamped));

  const sinYawCosPitch = 2 * (w * z + x * y);
  const cosYawCosPitch = 1 - 2 * (y * y + z * z);
  const yawRad = cleanZero(Math.atan2(sinYawCosPitch, cosYawCosPitch));

  let yawDeg = (((yawRad * RAD_TO_DEG) % 360) + 360) % 360;
  if (yawDeg >= 360 || Object.is(yawDeg, -0)) {
    yawDeg = 0;
  }

  return {
    rollDeg: cleanZero(rollRad * RAD_TO_DEG),
    pitchDeg: cleanZero(pitchRad * RAD_TO_DEG),
    yawDeg,
    rollRad,
    pitchRad,
    yawRad,
  };
}

export function computeFlightMetrics(
  nedXyz: readonly [number, number, number],
  nedVel: readonly [number, number, number],
): FlightMetricsView {
  const groundspeedMps =
    isFiniteNumber(nedVel[0]) && isFiniteNumber(nedVel[1])
      ? Math.hypot(nedVel[0], nedVel[1])
      : 0;
  const climbRateMps = isFiniteNumber(nedVel[2]) ? cleanZero(-nedVel[2]) : 0;
  const altitudeRelM = isFiniteNumber(nedXyz[2]) ? cleanZero(-nedXyz[2]) : 0;
  return { groundspeedMps, climbRateMps, altitudeRelM };
}

export function formatAuthorityBadge(
  authority: ControlAuthority,
): AuthorityBadgeView {
  switch (authority) {
    case "direct-crsf-override":
      return {
        code: "P1",
        authority,
        label: "P1 DirectCrsfOverride",
        severity: "warning",
      };
    case "companion-teleop":
      return {
        code: "P2",
        authority,
        label: "P2 CompanionTeleop",
        severity: "nominal",
      };
    case "companion-autonomous":
      return {
        code: "P3",
        authority,
        label: "P3 CompanionAutonomous",
        severity: "nominal",
      };
    case "mcu-failsafe":
      return {
        code: "P4",
        authority,
        label: "P4 McuFailsafe",
        severity: "error",
      };
  }
}

export function formatLinkTierBadge(tier: LinkTier): LinkTierBadgeView {
  switch (tier) {
    case "sitl-local":
      return { tier, label: "SITL" };
    case "direct-bench":
      return { tier, label: "Bench" };
    case "mesh-high-rate":
      return { tier, label: "HaLow 30Hz" };
    case "mesh-supervisory":
      return { tier, label: "LoRa 1Hz" };
  }
}

export function formatArmedBadge(
  armed: boolean,
  hasSafetyFault = false,
): ArmedBadgeView {
  if (!armed) {
    return { armed: false, label: "DISARMED", severity: "nominal" };
  }
  return {
    armed: true,
    label: "ARMED",
    severity: hasSafetyFault ? "warning" : "success",
  };
}

export function buildHealthStrip(
  flags: HealthFlags,
  sysStatus: VehicleSysStatus,
): readonly HealthChipView[] {
  return [
    {
      key: "crsf_link_healthy",
      label: "CRSF",
      value: flags.crsf_link_healthy ? "OK" : "FAULT",
      severity: flags.crsf_link_healthy ? "nominal" : "error",
    },
    {
      key: "companion_trunk_healthy",
      label: "Trunk",
      value: flags.companion_trunk_healthy ? "OK" : "FAULT",
      severity: flags.companion_trunk_healthy ? "nominal" : "error",
    },
    {
      key: "clock_sync_locked",
      label: "Clock",
      value: flags.clock_sync_locked ? "LOCKED" : "UNLOCKED",
      severity: flags.clock_sync_locked ? "nominal" : "warning",
    },
    {
      key: "teleop_deadman_clamped",
      label: "Deadman",
      value: flags.teleop_deadman_clamped ? "CLAMPED" : "CLEAR",
      severity: flags.teleop_deadman_clamped ? "warning" : "nominal",
    },
    {
      key: "failsafes",
      label: "Failsafes",
      value: String(sysStatus.failsafes),
      severity: sysStatus.failsafes > 0 ? "error" : "nominal",
    },
    {
      key: "crc_errors",
      label: "CRC Err",
      value: String(sysStatus.crcErrors),
      severity: sysStatus.crcErrors > 0 ? "warning" : "nominal",
    },
    {
      key: "loop_dt_us",
      label: "Loop dt",
      value: `${sysStatus.loopDtUs} us`,
      severity: "nominal",
    },
    {
      key: "max_loop_dt_us",
      label: "Max dt",
      value: `${sysStatus.maxLoopDtUs} us`,
      severity: "nominal",
    },
    {
      key: "rtt_us",
      label: "RTT",
      value: `${sysStatus.rttUs} us`,
      severity: "nominal",
    },
  ];
}

export function buildVehicleBannerView(
  vehicle: VehicleEntityState,
): VehicleBannerView {
  const hasFault =
    vehicle.poisoned ||
    vehicle.authority === "mcu-failsafe" ||
    vehicle.sysStatus.failsafes > 0 ||
    !vehicle.healthFlags.crsf_link_healthy ||
    !vehicle.healthFlags.companion_trunk_healthy;
  return {
    kind: "uxs-vehicle",
    entityId: vehicle.id,
    sysid: vehicle.sysid,
    domain: vehicle.domain,
    poisoned: vehicle.poisoned,
    poisonError: vehicle.poisonError,
    authorityBadge: formatAuthorityBadge(vehicle.authority),
    armedBadge: formatArmedBadge(vehicle.armed, hasFault),
    linkBadge: formatLinkTierBadge(vehicle.linkTier),
    healthStrip: buildHealthStrip(vehicle.healthFlags, vehicle.sysStatus),
  };
}

export function buildExternalContactSummary(
  contact: ExternalContactEntityState,
): ExternalContactSummaryView {
  const uncertaintyMeters =
    contact.uncertaintyMeters !== null &&
    isFiniteNumber(contact.uncertaintyMeters)
      ? contact.uncertaintyMeters
      : null;
  return {
    kind: "external-contact",
    entityId: contact.id,
    title: contact.title ?? contact.id,
    contactType: contact.contactType ?? "unknown",
    source: contact.source ?? "unknown",
    observedAt: contact.observedAt ?? "unknown",
    receivedAt: contact.receivedAt ?? "unknown",
    expiresAt: contact.expiresAt ?? "unknown",
    uncertaintyMeters,
    uncertaintyLabel:
      uncertaintyMeters !== null
        ? `±${uncertaintyMeters.toFixed(1)} m`
        : "unknown",
    feedStatus: contact.feedStatus,
    location: contact.location
      ? {
          latitude: contact.location.latitude,
          longitude: contact.location.longitude,
          altitude: contact.location.altitude,
        }
      : null,
  };
}

export function selectBannerPresentation(
  entity: CopEntityState | null | undefined,
): VehicleBannerView | ExternalContactSummaryView | null {
  if (!entity) {
    return null;
  }
  if (entity.kind === "uxs-vehicle") {
    return buildVehicleBannerView(entity);
  }
  if (entity.kind === "external-contact") {
    return buildExternalContactSummary(entity);
  }
  return null;
}
