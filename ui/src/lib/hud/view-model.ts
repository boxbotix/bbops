// SPDX-License-Identifier: MIT
// Copyright (c) 2026 boxbotix

import {
  type ControlAuthority,
  type HealthFlags,
  type LinkTier,
  isFiniteNumber,
  parseVehicleEntityId,
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

export interface PoisonedVehicleBannerView {
  readonly kind: "poisoned-uxs-vehicle";
  readonly entityId: string;
  readonly sysid: number;
  readonly poisonError: string | null;
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
    readonly msg_id?: number;
    readonly quat_wxyz: readonly [number, number, number, number];
    readonly has_attitude: boolean;
    readonly ned_xyz: readonly [number, number, number];
    readonly has_local_pos?: boolean;
    readonly has_explicit_local_pos?: boolean;
    readonly local_pos_confirmed_at_ms?: number | null;
    readonly local_pos_observed_at_ms?: number | null;
    readonly ned_vel: readonly [number, number, number];
    readonly lat_lon_alt: readonly [number, number, number];
    readonly has_global_pos: boolean;
    readonly global_pos_observed_at_ms?: number | null;
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
  readonly hasLocalPos: boolean;
  readonly hasVerifiedFrameReference: boolean;
  readonly quat: [number, number, number, number];
  readonly ned: [number, number, number];
  readonly vel: [number, number, number];
  readonly headingDeg: number;
  readonly headingRad: number;
  readonly lla: [number, number, number] | null;
  readonly localPosObservedAtMs?: number | null;
  readonly globalPosObservedAtMs?: number | null;
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

  const skgLla: [number, number, number] | null =
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
  const skgLocalPosMs =
    vehicle.localPosObservedAt !== undefined
      ? parseIsoEpochMs(vehicle.localPosObservedAt)
      : skgHasConfirmedLocalPos
        ? skgPoseMs
        : null;
  const skgGlobalPosMs =
    vehicle.globalPosObservedAt !== undefined
      ? parseIsoEpochMs(vehicle.globalPosObservedAt)
      : skgLla !== null
        ? skgPoseMs
        : null;

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
    let skgNewSessionInvalidatesLegacyLatch = false;
    if (
      fast.has_explicit_local_pos === false &&
      fast.msg_id !== 32 &&
      fast.local_pos_confirmed_at_ms !== null &&
      fast.local_pos_confirmed_at_ms !== undefined &&
      fast.local_pos_confirmed_at_ms >= MIN_COMPARABLE_EPOCH_MS &&
      vehicle.hasLocalPos === false
    ) {
      for (const triple of vehicle.triples) {
        if (
          triple.predicate === "uxs.kinematics.ned" &&
          triple.confidence === 0
        ) {
          const nedUnmeasuredMs = parseIsoEpochMs(triple.timestamp);
          if (
            nedUnmeasuredMs !== null &&
            nedUnmeasuredMs >= MIN_COMPARABLE_EPOCH_MS &&
            nedUnmeasuredMs > fast.local_pos_confirmed_at_ms
          ) {
            skgNewSessionInvalidatesLegacyLatch = true;
            break;
          }
        }
      }
    }
    const fastHasLocalPos =
      typeof fast.has_local_pos === "boolean"
        ? fast.has_local_pos
        : fast.msg_id !== undefined
          ? fast.msg_id === 32
          : fast.ned_xyz[0] !== 0 ||
            fast.ned_xyz[1] !== 0 ||
            fast.ned_xyz[2] !== 0 ||
            fast.ned_vel[0] !== 0 ||
            fast.ned_vel[1] !== 0 ||
            fast.ned_vel[2] !== 0;
    const hasLocalPos = fastHasLocalPos && !skgNewSessionInvalidatesLegacyLatch;
    const nedTuple: [number, number, number] = hasLocalPos
      ? [fast.ned_xyz[0], fast.ned_xyz[1], fast.ned_xyz[2]]
      : [0, 0, 0];
    const fastEpochComparable =
      isFiniteNumber(fast.timestamp) &&
      fast.timestamp >= MIN_COMPARABLE_EPOCH_MS;
    const skgLocalFallbackMs =
      fastEpochComparable &&
      skgHasConfirmedLocalPos &&
      skgLocalPosMs !== null &&
      skgLocalPosMs >= MIN_COMPARABLE_EPOCH_MS &&
      Math.abs(nedTuple[0] - vehicle.ned[0]) <= 1e-4 &&
      Math.abs(nedTuple[1] - vehicle.ned[1]) <= 1e-4 &&
      Math.abs(nedTuple[2] - vehicle.ned[2]) <= 1e-4
        ? skgLocalPosMs
        : null;
    const skgGlobalFallbackMs =
      fastEpochComparable &&
      lla !== null &&
      skgLla !== null &&
      skgGlobalPosMs !== null &&
      skgGlobalPosMs >= MIN_COMPARABLE_EPOCH_MS &&
      Math.abs(lla[0] - skgLla[0]) <= 1e-7 &&
      Math.abs(lla[1] - skgLla[1]) <= 1e-7 &&
      Math.abs(lla[2] - skgLla[2]) <= 1e-3
        ? skgGlobalPosMs
        : null;
    const localPosObservedAtMs = !hasLocalPos
      ? null
      : fast.local_pos_observed_at_ms !== undefined
        ? (fast.local_pos_observed_at_ms ?? skgLocalFallbackMs)
        : fast.msg_id === undefined || fast.msg_id === 32
          ? isFiniteNumber(fast.timestamp)
            ? fast.timestamp
            : null
          : skgLocalFallbackMs;
    const globalPosObservedAtMs =
      lla === null
        ? null
        : fast.global_pos_observed_at_ms !== undefined
          ? (fast.global_pos_observed_at_ms ?? skgGlobalFallbackMs)
          : fast.msg_id === undefined || fast.msg_id === 33
            ? isFiniteNumber(fast.timestamp)
              ? fast.timestamp
              : null
            : skgGlobalFallbackMs;
    return {
      source: "ws1",
      stale,
      hasAttitude,
      hasHeading,
      hasLocalPos,
      hasVerifiedFrameReference: hasLocalPos && lla !== null,
      quat,
      ned: nedTuple,
      vel: hasLocalPos
        ? [fast.ned_vel[0], fast.ned_vel[1], fast.ned_vel[2]]
        : [0, 0, 0],
      headingDeg,
      headingRad: headingDeg * DEG_TO_RAD,
      lla,
      localPosObservedAtMs,
      globalPosObservedAtMs,
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
  const ws1ContradictsLocalPos =
    fast !== undefined &&
    fast.has_explicit_local_pos !== false &&
    fast.has_local_pos === false &&
    (skgPoseMs === null || fast.timestamp >= skgPoseMs);
  const hasLocalPos = skgHasConfirmedLocalPos && !ws1ContradictsLocalPos;

  return {
    source: "skg",
    stale,
    hasAttitude,
    hasHeading,
    hasLocalPos,
    hasVerifiedFrameReference: hasLocalPos && skgLla !== null,
    quat,
    ned: hasLocalPos
      ? [vehicle.ned[0], vehicle.ned[1], vehicle.ned[2]]
      : [0, 0, 0],
    vel: hasLocalPos
      ? [vehicle.velocityNed[0], vehicle.velocityNed[1], vehicle.velocityNed[2]]
      : [0, 0, 0],
    headingDeg,
    headingRad: headingDeg * DEG_TO_RAD,
    lla: skgLla,
    localPosObservedAtMs: hasLocalPos ? skgLocalPosMs : null,
    globalPosObservedAtMs: skgLla !== null ? skgGlobalPosMs : null,
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
):
  | VehicleBannerView
  | PoisonedVehicleBannerView
  | ExternalContactSummaryView
  | null {
  if (!entity) {
    return null;
  }
  if (entity.kind === "uxs-vehicle") {
    return buildVehicleBannerView(entity);
  }
  if (entity.kind === "external-contact") {
    return buildExternalContactSummary(entity);
  }
  if (entity.poisoned) {
    const parsed = parseVehicleEntityId(entity.id);
    if (parsed !== null) {
      return {
        kind: "poisoned-uxs-vehicle",
        entityId: entity.id,
        sysid: parsed.sysid,
        poisonError: entity.poisonError,
      };
    }
  }
  return null;
}

export const HUD_READOUT_THROTTLE_MS = 250;
export const USV_HUD_CRAB_DEADBAND_MPS = 0.15;

export interface VehicleHudReadoutView {
  readonly gsText: string;
  readonly vsText: string;
  readonly altText: string;
  readonly attitudeLabel: "R / P / Y" | "R / P / HDG";
  readonly attitudeText: string;
  readonly crabAngleDeg: number | null;
  readonly crabText: string | null;
}

function wrapSignedAngleRad(rad: number): number {
  if (!isFiniteNumber(rad)) {
    return 0;
  }
  const twoPi = 2 * Math.PI;
  let wrapped = ((((rad + Math.PI) % twoPi) + twoPi) % twoPi) - Math.PI;
  if (wrapped <= -Math.PI) {
    wrapped += twoPi;
  }
  return cleanZero(wrapped);
}

export function shouldRefreshHudReadout(
  lastRefreshMs: number | null,
  nowMs: number,
  forceImmediate: boolean,
  throttleMs = HUD_READOUT_THROTTLE_MS,
): boolean {
  if (
    forceImmediate ||
    lastRefreshMs === null ||
    !isFiniteNumber(lastRefreshMs) ||
    !isFiniteNumber(nowMs) ||
    nowMs < lastRefreshMs
  ) {
    return true;
  }
  return nowMs - lastRefreshMs >= throttleMs;
}

/**
 * Formats non-overlapping semantic HTML HUD telemetry cells (GS, VS, ALT,
 * R/P/Y or R/P/HDG, plus signed Fossen Ch. 2 crab angle beta = chi - psi for
 * active surface-boat vehicles above USV_HUD_CRAB_DEADBAND_MPS).
 */
export function formatVehicleHudReadouts(
  pose: ResolvedVehiclePose,
  domain: VehicleEntityState["domain"],
): VehicleHudReadoutView {
  const flight = pose.hasLocalPos
    ? computeFlightMetrics(pose.ned, pose.vel)
    : null;
  const gsText =
    flight !== null ? `${flight.groundspeedMps.toFixed(1)} m/s` : "--";
  const vsText =
    flight !== null
      ? `${flight.climbRateMps >= 0 ? "+" : ""}${flight.climbRateMps.toFixed(1)} m/s`
      : "--";
  const altText =
    flight !== null ? `${flight.altitudeRelM.toFixed(1)} m` : "--";

  let attitudeLabel: "R / P / Y" | "R / P / HDG" = "R / P / HDG";
  let attitudeText = "-- / -- / ---";
  if (pose.hasAttitude) {
    const euler = quaternionToEulerDeg(pose.quat);
    const roll = cleanZero(Math.round(euler.rollDeg));
    const pitch = cleanZero(Math.round(euler.pitchDeg));
    const yaw = ((Math.round(euler.yawDeg) % 360) + 360) % 360;
    attitudeLabel = "R / P / Y";
    attitudeText = `${roll}° / ${pitch}° / ${yaw}°`;
  } else if (pose.hasHeading) {
    const hdg = ((Math.round(pose.headingDeg) % 360) + 360) % 360;
    attitudeText = `-- / -- / ${String(hdg).padStart(3, "0")}°`;
  }

  let crabAngleDeg: number | null = null;
  let crabText: string | null = null;
  if (
    domain === "surface-boat" &&
    pose.hasHeading &&
    pose.hasLocalPos &&
    flight !== null &&
    flight.groundspeedMps >= USV_HUD_CRAB_DEADBAND_MPS
  ) {
    const bowRad = wrapSignedAngleRad(pose.headingRad);
    const cogRad = Math.atan2(pose.vel[1], pose.vel[0]);
    const crabRad = wrapSignedAngleRad(cogRad - bowRad);
    crabAngleDeg = cleanZero(crabRad * RAD_TO_DEG);
    crabText = `${crabAngleDeg >= 0 ? "+" : ""}${crabAngleDeg.toFixed(1)}°`;
  }

  return {
    gsText,
    vsText,
    altText,
    attitudeLabel,
    attitudeText,
    crabAngleDeg,
    crabText,
  };
}

export interface ActiveVehicleFault {
  readonly severity: "error" | "warning" | null;
  readonly reason: string | null;
}

/**
 * Evaluates strictly current/active fault conditions on an ownship UXS vehicle
 * without treating cumulative lifetime counters (`failsafes`, `crcErrors`,
 * retained in Bench diagnostics) as unrecoverable active exceptions.
 */
export function evaluateActiveVehicleFault(
  vehicle: Pick<VehicleEntityState, "poisoned" | "authority" | "healthFlags">,
): ActiveVehicleFault {
  if (vehicle.poisoned) {
    return { severity: "error", reason: "POISONED" };
  }
  if (vehicle.authority === "mcu-failsafe") {
    return { severity: "error", reason: "P4" };
  }
  if (!vehicle.healthFlags.crsf_link_healthy) {
    return { severity: "error", reason: "CRSF FAULT" };
  }
  if (!vehicle.healthFlags.companion_trunk_healthy) {
    return { severity: "error", reason: "TRUNK FAULT" };
  }
  if (vehicle.authority === "direct-crsf-override") {
    return { severity: "warning", reason: "P1" };
  }
  if (vehicle.healthFlags.teleop_deadman_clamped) {
    return { severity: "warning", reason: "DEADMAN" };
  }
  if (!vehicle.healthFlags.clock_sync_locked) {
    return { severity: "warning", reason: "CLOCK UNLOCKED" };
  }
  return { severity: null, reason: null };
}

export interface FleetFaultSummary {
  readonly totalVehicles: number;
  readonly faultedCount: number;
  readonly errorCount: number;
  readonly warningCount: number;
  readonly firstFaultEntityId: string | null;
  readonly firstFaultLabel: string | null;
}

interface FleetFaultCandidate {
  readonly id: string;
  readonly sysid: number;
  readonly fault: ActiveVehicleFault;
}

/**
 * Aggregates active safety faults across all ownship UXS vehicles (including
 * poisoned-first ownship vehicle keys whose triples never decoded) in deterministic
 * ascending sysid order so off-screen or unselected vehicle faults remain visible
 * in the invariant topbar across COP, Pilot, Bench, and Cortex.
 */
export function summarizeFleetFaults(
  entities: Iterable<CopEntityState>,
): FleetFaultSummary {
  const byId = new Map<string, FleetFaultCandidate>();
  for (const entity of entities) {
    if (entity.kind === "uxs-vehicle") {
      byId.set(entity.id, {
        id: entity.id,
        sysid: entity.sysid,
        fault: evaluateActiveVehicleFault(entity),
      });
    } else if (entity.poisoned) {
      const parsed = parseVehicleEntityId(entity.id);
      if (parsed !== null) {
        byId.set(entity.id, {
          id: entity.id,
          sysid: parsed.sysid,
          fault: { severity: "error", reason: "POISONED" },
        });
      }
    }
  }

  const ordered = Array.from(byId.values()).sort(
    (a, b) => a.sysid - b.sysid || a.id.localeCompare(b.id),
  );
  let errorCount = 0;
  let warningCount = 0;
  let firstError: FleetFaultCandidate | null = null;
  let firstWarning: FleetFaultCandidate | null = null;

  for (const candidate of ordered) {
    if (candidate.fault.severity === "error") {
      errorCount += 1;
      if (firstError === null) {
        firstError = candidate;
      }
    } else if (candidate.fault.severity === "warning") {
      warningCount += 1;
      if (firstWarning === null) {
        firstWarning = candidate;
      }
    }
  }

  const primary = firstError ?? firstWarning;
  const firstFaultLabel =
    primary !== null && primary.fault.reason !== null
      ? `UXS #${primary.sysid} · ${primary.fault.reason}`
      : null;

  return {
    totalVehicles: ordered.length,
    faultedCount: errorCount + warningCount,
    errorCount,
    warningCount,
    firstFaultEntityId: primary?.id ?? null,
    firstFaultLabel,
  };
}

/**
 * Filters a vehicle's 0xB1 health strip down to ISA-101 abnormal exceptions
 * (error first, then warning) for Pilot cockpit promotion.
 */
export function extractAbnormalHealthChips(
  strip: readonly HealthChipView[],
): readonly HealthChipView[] {
  const errors = strip.filter((chip) => chip.severity === "error");
  const warnings = strip.filter((chip) => chip.severity === "warning");
  return [...errors, ...warnings];
}

export interface FilmstripItemView {
  readonly entityId: string;
  readonly shortcutKey: string | null;
  readonly kind: "uxs-vehicle" | "external-contact";
  readonly primaryLabel: string;
  readonly secondaryLabel: string;
  readonly safetySeverity: SafetyBadgeSeverity | null;
  readonly contactFeedStatus: CopFeedStatus | null;
}

/**
 * Builds deterministic FleetFilmstrip items (ownship vehicles sorted by sysid,
 * including poisoned-first ownship vehicle keys when `allEntities` is supplied,
 * followed by external contacts sorted by entity ID), assigning 1..9 hotkeys
 * and keeping reserved safety severities strictly null on external contacts.
 */
export function buildFilmstripItems(
  vehicles: Iterable<VehicleEntityState>,
  contacts: Iterable<ExternalContactEntityState>,
  allEntities?: Iterable<CopEntityState>,
): readonly FilmstripItemView[] {
  type OwnshipEntry =
    | {
        readonly sysid: number;
        readonly id: string;
        readonly decoded: VehicleEntityState;
      }
    | {
        readonly sysid: number;
        readonly id: string;
        readonly poisonedEntity: CopEntityState;
      };

  const ownshipById = new Map<string, OwnshipEntry>();
  for (const vehicle of vehicles) {
    ownshipById.set(vehicle.id, {
      sysid: vehicle.sysid,
      id: vehicle.id,
      decoded: vehicle,
    });
  }
  if (allEntities) {
    for (const entity of allEntities) {
      if (
        entity.kind !== "uxs-vehicle" &&
        entity.poisoned &&
        !ownshipById.has(entity.id)
      ) {
        const parsed = parseVehicleEntityId(entity.id);
        if (parsed !== null) {
          ownshipById.set(entity.id, {
            sysid: parsed.sysid,
            id: entity.id,
            poisonedEntity: entity,
          });
        }
      }
    }
  }

  const orderedOwnship = Array.from(ownshipById.values()).sort(
    (a, b) => a.sysid - b.sysid || a.id.localeCompare(b.id),
  );
  const orderedContacts = Array.from(contacts).sort((a, b) =>
    a.id.localeCompare(b.id),
  );

  const items: FilmstripItemView[] = [];
  for (const entry of orderedOwnship) {
    const shortcutKey = items.length < 9 ? String(items.length + 1) : null;
    if ("decoded" in entry) {
      const vehicle = entry.decoded;
      const authCode = formatAuthorityBadge(vehicle.authority).code;
      const activeFault = evaluateActiveVehicleFault(vehicle);
      items.push({
        entityId: vehicle.id,
        shortcutKey,
        kind: "uxs-vehicle",
        primaryLabel: vehicle.title ?? `UXS #${vehicle.sysid}`,
        secondaryLabel: `#${vehicle.sysid} · ${authCode} · ${vehicle.armed ? "ARMED" : "SAFE"}`,
        safetySeverity:
          activeFault.severity ?? (vehicle.armed ? "success" : "nominal"),
        contactFeedStatus: null,
      });
    } else {
      items.push({
        entityId: entry.id,
        shortcutKey,
        kind: "uxs-vehicle",
        primaryLabel: entry.poisonedEntity.title ?? `UXS #${entry.sysid}`,
        secondaryLabel: `#${entry.sysid} · POISONED`,
        safetySeverity: "error",
        contactFeedStatus: null,
      });
    }
  }

  for (const contact of orderedContacts) {
    const shortcutKey = items.length < 9 ? String(items.length + 1) : null;
    items.push({
      entityId: contact.id,
      shortcutKey,
      kind: "external-contact",
      primaryLabel: contact.title ?? contact.id,
      secondaryLabel: `EXT · ${contact.source ?? "unknown"} · ${contact.feedStatus}`,
      safetySeverity: null,
      contactFeedStatus: contact.feedStatus,
    });
  }

  return items;
}

export const COP_SOLO_SECTIONS = ["summary", "alerts", "skg"] as const;
export type CopSoloSection = (typeof COP_SOLO_SECTIONS)[number];

export function selectCopSoloSection(
  current: CopSoloSection,
  requested: string,
): CopSoloSection {
  return (COP_SOLO_SECTIONS as readonly string[]).includes(requested)
    ? (requested as CopSoloSection)
    : current;
}
