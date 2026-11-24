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

export const WS1_KINEMATICS_FRESH_MS = 1_000;
export const SKG_VEHICLE_FRESH_MS = 2_500;

const DEG_TO_RAD = Math.PI / 180;

export interface LiveVehiclePoseOptions {
  readonly fast?: {
    readonly quat_wxyz: readonly [number, number, number, number];
    readonly has_attitude: boolean;
    readonly ned_xyz: readonly [number, number, number];
    readonly has_local_pos?: boolean;
    readonly ned_vel: readonly [number, number, number];
    readonly lat_lon_alt: readonly [number, number, number];
    readonly has_global_pos: boolean;
    readonly hdg_deg: number;
    readonly has_heading?: boolean;
    readonly timestamp: number;
    readonly receivedAtMs: number;
  };
  readonly telemetryStatus?: string;
  readonly skgStatus?: string;
  readonly nowMs?: number;
}

export interface ResolvedVehiclePose {
  readonly source: "ws1" | "skg";
  readonly stale: boolean;
  readonly hasAttitude: boolean;
  readonly hasHeading: boolean;
  readonly hasVerifiedFrameReference: boolean;
  readonly quat: [number, number, number, number];
  readonly ned: [number, number, number];
  readonly vel: [number, number, number];
  readonly headingDeg: number;
  readonly headingRad: number;
  readonly lla: [number, number, number] | null;
}

function normalizeHeadingDeg360(deg: number): number {
  if (!isFiniteNumber(deg)) {
    return 0;
  }
  const wrapped = ((deg % 360) + 360) % 360;
  return wrapped >= 360 || Object.is(wrapped, -0) ? 0 : wrapped;
}

function parseIsoEpochMs(iso: string | null | undefined): number | null {
  if (!iso) {
    return null;
  }
  const parsed = Date.parse(iso);
  return Number.isFinite(parsed) ? parsed : null;
}

function isValidNonZeroLla(lat: number, lon: number, alt: number): boolean {
  return (
    isFiniteNumber(lat) &&
    isFiniteNumber(lon) &&
    isFiniteNumber(alt) &&
    lat >= -90 &&
    lat <= 90 &&
    lon >= -180 &&
    lon <= 180 &&
    (lat !== 0 || lon !== 0 || alt !== 0)
  );
}

/**
 * Returns true only when quatWxyz carries a measured unit/non-zero attitude
 * quaternion rather than the pre-#31 zero-norm [0, 0, 0, 0] sentinel (or an
 * unmeasured [1, 0, 0, 0] placeholder paired with a divergent #33 heading).
 */
export function isMeasuredAttitudeQuaternion(
  quatWxyz: readonly [number, number, number, number],
  resolvedHeadingDeg?: number,
): boolean {
  const [w, x, y, z] = quatWxyz;
  if (
    !isFiniteNumber(w) ||
    !isFiniteNumber(x) ||
    !isFiniteNumber(y) ||
    !isFiniteNumber(z)
  ) {
    return false;
  }
  const normSq = w * w + x * x + y * y + z * z;
  if (normSq < 1e-6) {
    return false;
  }
  if (
    resolvedHeadingDeg !== undefined &&
    isFiniteNumber(resolvedHeadingDeg) &&
    Math.abs(w - 1) < 1e-9 &&
    Math.abs(x) < 1e-9 &&
    Math.abs(y) < 1e-9 &&
    Math.abs(z) < 1e-9
  ) {
    const wrappedHdg = normalizeHeadingDeg360(resolvedHeadingDeg);
    const distFromNorth = Math.min(wrappedHdg, 360 - wrappedHdg);
    if (distFromNorth > 0.5) {
      return false;
    }
  }
  return true;
}

const MIN_COMPARABLE_EPOCH_MS = 946_684_800_000; // 2000-01-01T00:00:00Z

/**
 * Selects the freshest per-vehicle pose across WS #1 (20 Hz kinematics) and
 * WS #2 (SKG ENTITY_STATES), honoring the tracker's resolved heading before
 * the first #31 ATTITUDE_QUATERNION report and flagging stale state when
 * neither feed is current.
 */
export function resolveLiveVehiclePose(
  vehicle: VehicleEntityState,
  options: LiveVehiclePoseOptions = {},
): ResolvedVehiclePose {
  const nowMs = options.nowMs ?? Date.now();
  const fast = options.fast;

  const ws1Connected = options.telemetryStatus === "connected";
  const ws1ElapsedMs =
    fast !== undefined && isFiniteNumber(fast.receivedAtMs)
      ? nowMs - fast.receivedAtMs
      : Number.POSITIVE_INFINITY;
  const ws1Fresh =
    fast !== undefined &&
    ws1Connected &&
    ws1ElapsedMs >= -WS1_KINEMATICS_FRESH_MS &&
    ws1ElapsedMs <= WS1_KINEMATICS_FRESH_MS;

  const skgPoseMs =
    vehicle.poseObservedAt !== undefined
      ? parseIsoEpochMs(vehicle.poseObservedAt)
      : parseIsoEpochMs(vehicle.observedAt);
  const skgConnected =
    options.skgStatus === undefined || options.skgStatus === "connected";
  const skgObsElapsedMs =
    skgPoseMs !== null && skgPoseMs > 0
      ? nowMs - skgPoseMs
      : Number.POSITIVE_INFINITY;
  const skgPoseReceiptMs =
    vehicle.poseIngestedAtMs !== undefined
      ? vehicle.poseIngestedAtMs
      : (vehicle.ingestedAtMs ?? null);
  const skgReceiptElapsedMs =
    skgPoseReceiptMs !== null && isFiniteNumber(skgPoseReceiptMs)
      ? nowMs - skgPoseReceiptMs
      : null;
  const skgReceiptFresh =
    skgReceiptElapsedMs !== null &&
    skgReceiptElapsedMs >= -SKG_VEHICLE_FRESH_MS &&
    skgReceiptElapsedMs <= SKG_VEHICLE_FRESH_MS;
  const skgSnapshotAgeFresh =
    skgObsElapsedMs >= -SKG_VEHICLE_FRESH_MS &&
    skgObsElapsedMs <= SKG_VEHICLE_FRESH_MS &&
    (skgReceiptElapsedMs === null || skgReceiptFresh);
  const skgFresh =
    skgConnected &&
    vehicle.feedStatus !== "stale" &&
    vehicle.feedStatus !== "disconnected" &&
    (vehicle.poseIngestedLive === true ? skgReceiptFresh : skgSnapshotAgeFresh);

  let useFast = false;
  if (fast !== undefined) {
    if (skgPoseMs === null || skgPoseMs <= 0) {
      useFast = true;
    } else if (
      skgPoseMs >= MIN_COMPARABLE_EPOCH_MS &&
      isFiniteNumber(fast.timestamp) &&
      fast.timestamp >= MIN_COMPARABLE_EPOCH_MS
    ) {
      if (fast.timestamp > skgPoseMs) {
        useFast = true;
      } else if (skgPoseMs > fast.timestamp) {
        useFast = false;
      } else {
        useFast = ws1Fresh || !skgFresh;
      }
    } else if (ws1Fresh && !skgFresh) {
      useFast = true;
    } else if (skgFresh && !ws1Fresh) {
      useFast = false;
    } else if (
      (vehicle.poseIngestedLive === true || skgSnapshotAgeFresh) &&
      skgPoseReceiptMs !== null &&
      isFiniteNumber(skgPoseReceiptMs)
    ) {
      useFast = fast.receivedAtMs >= skgPoseReceiptMs;
    } else {
      useFast = true;
    }
  }

  const stale = !(useFast ? ws1Fresh : skgFresh);

  if (useFast && fast !== undefined) {
    const quat: [number, number, number, number] = [
      fast.quat_wxyz[0],
      fast.quat_wxyz[1],
      fast.quat_wxyz[2],
      fast.quat_wxyz[3],
    ];
    const headingDeg = normalizeHeadingDeg360(fast.hdg_deg);
    const hasAttitude =
      fast.has_attitude && isMeasuredAttitudeQuaternion(quat, fast.hdg_deg);
    const hasHeading = hasAttitude || (fast.has_heading ?? headingDeg !== 0);
    const lla: [number, number, number] | null =
      fast.has_global_pos &&
      isFiniteNumber(fast.lat_lon_alt[0]) &&
      isFiniteNumber(fast.lat_lon_alt[1]) &&
      isFiniteNumber(fast.lat_lon_alt[2]) &&
      fast.lat_lon_alt[0] >= -90 &&
      fast.lat_lon_alt[0] <= 90 &&
      fast.lat_lon_alt[1] >= -180 &&
      fast.lat_lon_alt[1] <= 180
        ? [fast.lat_lon_alt[0], fast.lat_lon_alt[1], fast.lat_lon_alt[2]]
        : null;
    const hasLocalPos = fast.has_local_pos === true;
    return {
      source: "ws1",
      stale,
      hasAttitude,
      hasHeading,
      hasVerifiedFrameReference: hasLocalPos && lla !== null,
      quat,
      ned: [fast.ned_xyz[0], fast.ned_xyz[1], fast.ned_xyz[2]],
      vel: [fast.ned_vel[0], fast.ned_vel[1], fast.ned_vel[2]],
      headingDeg,
      headingRad: headingDeg * DEG_TO_RAD,
      lla,
    };
  }

  const quat: [number, number, number, number] = [
    vehicle.attitudeQuaternion[0],
    vehicle.attitudeQuaternion[1],
    vehicle.attitudeQuaternion[2],
    vehicle.attitudeQuaternion[3],
  ];
  const headingDeg = normalizeHeadingDeg360(vehicle.headingDeg);
  const hasAttitude =
    (vehicle.hasAttitude ?? true) &&
    isMeasuredAttitudeQuaternion(quat, vehicle.headingDeg);
  const hasHeading = hasAttitude || (vehicle.hasHeading ?? headingDeg !== 0);
  const lla: [number, number, number] | null =
    (vehicle.hasGlobalPos ?? true) &&
    isValidNonZeroLla(
      vehicle.location.latitude,
      vehicle.location.longitude,
      vehicle.location.altitude,
    )
      ? [
          vehicle.location.latitude,
          vehicle.location.longitude,
          vehicle.location.altitude,
        ]
      : null;
  const hasNonZeroVehicleLocalKinematics =
    vehicle.ned[0] !== 0 ||
    vehicle.ned[1] !== 0 ||
    vehicle.ned[2] !== 0 ||
    vehicle.velocityNed[0] !== 0 ||
    vehicle.velocityNed[1] !== 0 ||
    vehicle.velocityNed[2] !== 0;
  const skgHasConfirmedLocalPos =
    vehicle.hasLocalPos === true ||
    (vehicle.hasLocalPos === undefined && hasNonZeroVehicleLocalKinematics);
  const ws1ContradictsLocalPos =
    fast !== undefined &&
    fast.has_local_pos === false &&
    (skgPoseMs === null || fast.timestamp >= skgPoseMs);

  return {
    source: "skg",
    stale,
    hasAttitude,
    hasHeading,
    hasVerifiedFrameReference:
      skgHasConfirmedLocalPos && !ws1ContradictsLocalPos && lla !== null,
    quat,
    ned: [vehicle.ned[0], vehicle.ned[1], vehicle.ned[2]],
    vel: [
      vehicle.velocityNed[0],
      vehicle.velocityNed[1],
      vehicle.velocityNed[2],
    ],
    headingDeg,
    headingRad: headingDeg * DEG_TO_RAD,
    lla,
  };
}

/**
 * Canonical UXS vehicle safety severity shared by both TacticalCanvas glyphs
 * and AuthorityBanner arming indicators so active warnings never show green.
 */
export function evaluateVehicleSafetySeverity(
  vehicle: Pick<
    VehicleEntityState,
    "poisoned" | "authority" | "armed" | "healthFlags" | "sysStatus"
  >,
): SafetyBadgeSeverity {
  const flags: HealthFlags = vehicle.healthFlags;
  const sys: VehicleSysStatus = vehicle.sysStatus;
  if (
    vehicle.poisoned ||
    vehicle.authority === "mcu-failsafe" ||
    sys.failsafes > 0 ||
    !flags.crsf_link_healthy ||
    !flags.companion_trunk_healthy
  ) {
    return "error";
  }
  if (
    vehicle.authority === "direct-crsf-override" ||
    flags.teleop_deadman_clamped ||
    !flags.clock_sync_locked ||
    sys.crcErrors > 0
  ) {
    return "warning";
  }
  if (vehicle.armed) {
    return "success";
  }
  return "nominal";
}

export function buildVehicleBannerView(
  vehicle: VehicleEntityState,
): VehicleBannerView {
  const overallSafety = evaluateVehicleSafetySeverity(vehicle);
  const hasSafetyIssue =
    overallSafety === "error" || overallSafety === "warning";
  return {
    kind: "uxs-vehicle",
    entityId: vehicle.id,
    sysid: vehicle.sysid,
    domain: vehicle.domain,
    poisoned: vehicle.poisoned,
    poisonError: vehicle.poisonError,
    authorityBadge: formatAuthorityBadge(vehicle.authority),
    armedBadge: formatArmedBadge(vehicle.armed, hasSafetyIssue),
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
