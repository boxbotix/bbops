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
  SkgStore,
  VehicleEntityState,
  VehicleSysStatus,
} from "../stores/skg.svelte";
import type { VehicleTelemetryLinkState } from "../stores/telemetry.svelte";
import type { WsConnectionStatus } from "../stores/ws-client";

const RAD_TO_DEG = 180 / Math.PI;

export type SafetyBadgeSeverity = "error" | "warning" | "success" | "nominal";

export type VehicleStaleReason =
  | "link_lost"
  | "fleet_disconnected"
  | "observation_timeout"
  | "feed_stale"
  | "poisoned_unverified";

export interface VehicleFreshnessView {
  readonly fresh: boolean;
  readonly staleReason: VehicleStaleReason | null;
  readonly ageMs: number | null;
  readonly ageLabel: string;
  readonly statusLabel: string;
  readonly lastObservedAt: string | null;
  readonly thresholdMs: number;
  readonly decodeErrorAgeMs?: number | null;
  readonly decodeErrorAgeLabel?: string | null;
}

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
  readonly freshness: VehicleFreshnessView | null;
}

export interface PoisonedVehicleBannerView {
  readonly kind: "poisoned-uxs-vehicle";
  readonly entityId: string;
  readonly sysid: number;
  readonly poisonError: string | null;
  readonly freshness: VehicleFreshnessView | null;
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
  const hasTiming = sysStatus.hasSysStatus !== false;
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
      value: hasTiming ? `${sysStatus.loopDtUs} us` : "--",
      severity: "nominal",
    },
    {
      key: "max_loop_dt_us",
      label: "Max dt",
      value: hasTiming ? `${sysStatus.maxLoopDtUs} us` : "--",
      severity: "nominal",
    },
    {
      key: "rtt_us",
      label: "RTT",
      value: hasTiming ? `${sysStatus.rttUs} us` : "--",
      severity: "nominal",
    },
  ];
}

export const WS1_KINEMATICS_FRESH_MS = 1_000;
export const SKG_VEHICLE_FRESH_MS = 2_500;
// Aligns with internal/mavlink/config.go DefaultLinkTimeoutMs (3_000ms) + 1 Hz SKG flush margin.
export const VEHICLE_FRESH_DEFAULT_MS = 4_000;
// Aligns with internal/mavlink/config.go DefaultSupervisoryTimeoutMs (10_000ms) + 1 Hz SKG flush margin.
export const VEHICLE_FRESH_SUPERVISORY_MS = 11_000;
export const FLEET_FRESHNESS_TICK_MS = 1_000;

export function resolveVehicleFreshnessThresholdMs(linkTier: LinkTier): number {
  return linkTier === "mesh-supervisory"
    ? VEHICLE_FRESH_SUPERVISORY_MS
    : VEHICLE_FRESH_DEFAULT_MS;
}

export function formatObservationAge(ageMs: number | null | undefined): string {
  if (ageMs === null || ageMs === undefined || !isFiniteNumber(ageMs)) {
    return "age unknown";
  }
  const clampedMs = Math.max(0, ageMs);
  if (clampedMs < 1_000) {
    return "<1s ago";
  }
  const totalSec = Math.floor(clampedMs / 1_000);
  if (totalSec < 60) {
    return `${totalSec}s ago`;
  }
  const totalMin = Math.floor(totalSec / 60);
  const remSec = totalSec % 60;
  if (totalMin < 60) {
    return `${totalMin}m ${remSec}s ago`;
  }
  const totalHr = Math.floor(totalMin / 60);
  const remMin = totalMin % 60;
  if (totalHr < 24) {
    return `${totalHr}h ${remMin}m ago`;
  }
  const days = Math.floor(totalHr / 24);
  const remHr = totalHr % 24;
  return `${days}d ${remHr}h ago`;
}

const DEG_TO_RAD = Math.PI / 180;

export interface VehicleFreshnessOptions {
  readonly telemetryLink?: VehicleTelemetryLinkState;
  readonly telemetryStatus?: WsConnectionStatus | string;
  readonly skgStatus?: WsConnectionStatus | string;
  readonly skgSynced?: boolean;
  readonly nowMs?: number;
}

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
  readonly telemetryLink?: VehicleTelemetryLinkState;
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

function didSkgObservationSupersedeLinkLoss(
  skgObsMs: number | null,
  skgReceiptMs: number | null,
  skgIngestedLive: boolean,
  link: VehicleTelemetryLinkState | undefined,
): boolean {
  if (!link || !link.linkLost) {
    return true;
  }
  if (
    skgObsMs !== null &&
    skgObsMs >= MIN_COMPARABLE_EPOCH_MS &&
    link.linkLostTimestamp !== null &&
    link.linkLostTimestamp >= MIN_COMPARABLE_EPOCH_MS
  ) {
    return skgObsMs > link.linkLostTimestamp;
  }
  return (
    skgIngestedLive &&
    skgReceiptMs !== null &&
    isFiniteNumber(skgReceiptMs) &&
    link.linkLostAtMs !== null &&
    skgReceiptMs > link.linkLostAtMs
  );
}

/**
 * Evaluates per-vehicle health/observation freshness across WS #1 link-loss
 * signals (`#27`) and LinkTier-scaled WS #2 measured `uxs.*` health timestamps
 * (`#29`), preserving stale observation age across socket reconnects, fast
 * snapshot replays, and pose-only updates.
 */
export function evaluateVehicleFreshness(
  vehicle: Pick<
    VehicleEntityState,
    | "linkTier"
    | "feedStatus"
    | "observedAt"
    | "hasMeasuredSafetyState"
    | "stateObservedAt"
    | "stateIngestedAtMs"
    | "stateIngestedLive"
    | "stateReplayedUnchanged"
    | "ingestedAtMs"
  > &
    Partial<
      Pick<
        VehicleEntityState,
        "poisoned" | "poisonedAtMs" | "stateProducerAgeMs"
      >
    >,
  options: VehicleFreshnessOptions = {},
): VehicleFreshnessView {
  const nowMs = options.nowMs ?? Date.now();
  const thresholdMs = resolveVehicleFreshnessThresholdMs(vehicle.linkTier);
  const skgConnected =
    options.skgStatus === undefined || options.skgStatus === "connected";
  const skgSynced = options.skgSynced ?? true;
  const fleetDataLive = skgConnected && skgSynced;

  const safetyMeasured = vehicle.hasMeasuredSafetyState !== false;
  const producerAgeMs =
    safetyMeasured &&
    vehicle.stateProducerAgeMs !== undefined &&
    isFiniteNumber(vehicle.stateProducerAgeMs) &&
    vehicle.stateProducerAgeMs > 0
      ? vehicle.stateProducerAgeMs
      : 0;
  const rawObs = !safetyMeasured
    ? null
    : vehicle.stateObservedAt !== undefined
      ? vehicle.stateObservedAt
      : (vehicle.observedAt ?? null);
  const skgObsMs = parseIsoEpochMs(rawObs);
  const skgReceiptMs =
    rawObs === null
      ? null
      : vehicle.stateIngestedAtMs !== undefined
        ? vehicle.stateIngestedAtMs
        : (vehicle.ingestedAtMs ?? null);
  const skgIngestedLive = rawObs !== null && vehicle.stateIngestedLive === true;
  const link = options.telemetryLink;

  const effectiveLinkLost =
    Boolean(link?.linkLost) &&
    !didSkgObservationSupersedeLinkLoss(
      skgObsMs,
      skgReceiptMs,
      skgIngestedLive,
      link,
    );

  let skgElapsedMs: number | null = null;
  if (
    skgIngestedLive &&
    skgReceiptMs !== null &&
    isFiniteNumber(skgReceiptMs) &&
    skgReceiptMs > 0
  ) {
    skgElapsedMs = Math.max(0, nowMs - skgReceiptMs, producerAgeMs);
  } else if (skgObsMs !== null && skgObsMs > 0) {
    skgElapsedMs =
      skgReceiptMs !== null && isFiniteNumber(skgReceiptMs) && skgReceiptMs > 0
        ? Math.max(0, nowMs - skgObsMs, nowMs - skgReceiptMs, producerAgeMs)
        : Math.max(0, nowMs - skgObsMs, producerAgeMs);
  }

  const ageMs = skgElapsedMs;
  const ageLabel = formatObservationAge(ageMs);
  const lastObservedAt =
    rawObs !== null && skgObsMs !== null && skgObsMs > 0
      ? new Date(skgObsMs).toISOString()
      : null;

  const decodeErrorAgeMs =
    vehicle.poisoned &&
    vehicle.poisonedAtMs !== null &&
    vehicle.poisonedAtMs !== undefined &&
    isFiniteNumber(vehicle.poisonedAtMs) &&
    vehicle.poisonedAtMs > 0
      ? Math.max(0, nowMs - vehicle.poisonedAtMs)
      : null;
  const decodeErrorAgeLabel =
    decodeErrorAgeMs !== null ? formatObservationAge(decodeErrorAgeMs) : null;
  const staleStatusLabel = decodeErrorAgeLabel
    ? `LAST KNOWN · ${ageLabel} (decode error ${decodeErrorAgeLabel})`
    : `LAST KNOWN · ${ageLabel}`;
  const poisonMeta =
    decodeErrorAgeMs !== null ? { decodeErrorAgeMs, decodeErrorAgeLabel } : {};

  if (vehicle.feedStatus === "stale" || vehicle.feedStatus === "disconnected") {
    return {
      fresh: false,
      staleReason: "feed_stale",
      ageMs,
      ageLabel,
      statusLabel: staleStatusLabel,
      lastObservedAt,
      thresholdMs,
      ...poisonMeta,
    };
  }

  if (effectiveLinkLost) {
    return {
      fresh: false,
      staleReason: "link_lost",
      ageMs,
      ageLabel,
      statusLabel: staleStatusLabel,
      lastObservedAt,
      thresholdMs,
      ...poisonMeta,
    };
  }

  if (!fleetDataLive) {
    return {
      fresh: false,
      staleReason: "fleet_disconnected",
      ageMs,
      ageLabel,
      statusLabel: staleStatusLabel,
      lastObservedAt,
      thresholdMs,
      ...poisonMeta,
    };
  }

  if (vehicle.poisoned) {
    return {
      fresh: false,
      staleReason: "poisoned_unverified",
      ageMs,
      ageLabel,
      statusLabel: staleStatusLabel,
      lastObservedAt,
      thresholdMs,
      ...poisonMeta,
    };
  }

  const skgObsWindowOk =
    skgObsMs !== null &&
    skgObsMs > 0 &&
    nowMs - skgObsMs >= -thresholdMs &&
    nowMs - skgObsMs <= thresholdMs;
  const skgReceiptWindowOk =
    skgReceiptMs !== null &&
    isFiniteNumber(skgReceiptMs) &&
    nowMs - skgReceiptMs >= -thresholdMs &&
    nowMs - skgReceiptMs <= thresholdMs;
  const skgStateFresh =
    safetyMeasured &&
    producerAgeMs <= thresholdMs &&
    vehicle.stateReplayedUnchanged !== true &&
    (skgIngestedLive
      ? skgReceiptWindowOk
      : skgObsWindowOk && (skgReceiptMs === null || skgReceiptWindowOk));

  const fresh = skgStateFresh;
  return {
    fresh,
    staleReason: fresh ? null : "observation_timeout",
    ageMs,
    ageLabel,
    statusLabel: fresh ? "LIVE" : staleStatusLabel,
    lastObservedAt,
    thresholdMs,
  };
}

/**
 * Evaluates conservative freshness for a poisoned-first ownship vehicle key
 * whose KV payload never produced a decoded `uxs.*` record. Because observation
 * timestamps and LinkTier are unverified, `fresh` is always `false` and unknown
 * observation age (`ageMs: null`, `"age unknown"`) is kept strictly distinct
 * from elapsed time since the decode error (`decodeErrorAgeMs`).
 */
export function evaluatePoisonedVehicleFreshness(
  entity: Pick<CopEntityState, "observedAt" | "poisonedAtMs">,
  options: VehicleFreshnessOptions = {},
): VehicleFreshnessView {
  const nowMs = options.nowMs ?? Date.now();
  const skgConnected =
    options.skgStatus === undefined || options.skgStatus === "connected";
  const skgSynced = options.skgSynced ?? true;
  const fleetDataLive = skgConnected && skgSynced;
  const link = options.telemetryLink;

  const obsMs = parseIsoEpochMs(entity.observedAt);
  const ageMs = obsMs !== null && obsMs > 0 ? Math.max(0, nowMs - obsMs) : null;
  const ageLabel = formatObservationAge(ageMs);

  const decodeErrorAgeMs =
    entity.poisonedAtMs !== null &&
    entity.poisonedAtMs !== undefined &&
    isFiniteNumber(entity.poisonedAtMs) &&
    entity.poisonedAtMs > 0
      ? Math.max(0, nowMs - entity.poisonedAtMs)
      : null;
  const decodeErrorAgeLabel =
    decodeErrorAgeMs !== null ? formatObservationAge(decodeErrorAgeMs) : null;

  const staleReason: VehicleStaleReason = link?.linkLost
    ? "link_lost"
    : !fleetDataLive
      ? "fleet_disconnected"
      : "poisoned_unverified";

  const statusLabel = decodeErrorAgeLabel
    ? `LAST KNOWN · ${ageLabel} (decode error ${decodeErrorAgeLabel})`
    : `LAST KNOWN · ${ageLabel}`;

  return {
    fresh: false,
    staleReason,
    ageMs,
    ageLabel,
    statusLabel,
    lastObservedAt:
      obsMs !== null && obsMs > 0 ? new Date(obsMs).toISOString() : null,
    thresholdMs: VEHICLE_FRESH_DEFAULT_MS,
    decodeErrorAgeMs,
    decodeErrorAgeLabel,
  };
}

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
  const link = options.telemetryLink;

  const ws1Connected = options.telemetryStatus === "connected";
  const ws1LinkLost =
    Boolean(link?.linkLost) &&
    (fast === undefined ||
      link?.linkLostTimestamp === null ||
      link?.linkLostTimestamp === undefined ||
      fast.timestamp <= link.linkLostTimestamp);
  const ws1ElapsedMs =
    fast !== undefined && isFiniteNumber(fast.receivedAtMs)
      ? nowMs - fast.receivedAtMs
      : Number.POSITIVE_INFINITY;
  const ws1Fresh =
    fast !== undefined &&
    ws1Connected &&
    !ws1LinkLost &&
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
  const skgPoseSupersededLoss = didSkgObservationSupersedeLinkLoss(
    skgPoseMs,
    skgPoseReceiptMs,
    vehicle.poseIngestedLive === true,
    link,
  );
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
    skgPoseSupersededLoss &&
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
  options?: { readonly isStale?: boolean },
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
    return options?.isStale ? "warning" : "success";
  }
  return "nominal";
}

export interface FleetFreshnessContext {
  readonly telemetryStatus?: WsConnectionStatus | string;
  readonly telemetryLink?: VehicleTelemetryLinkState;
  readonly getTelemetryLink?: (
    sysid: number,
  ) => VehicleTelemetryLinkState | undefined;
  readonly skgStatus?: WsConnectionStatus | string;
  readonly skgSynced?: boolean;
  readonly skgLastSyncedRevision?: number | null;
  readonly nowMs?: number;
}

function resolveVehicleFreshnessFromContext(
  vehicle: VehicleEntityState,
  context: FleetFreshnessContext,
): VehicleFreshnessView {
  const telemetryLink =
    context.telemetryLink ?? context.getTelemetryLink?.(vehicle.sysid);
  return evaluateVehicleFreshness(vehicle, {
    telemetryLink,
    telemetryStatus: context.telemetryStatus,
    skgStatus: context.skgStatus,
    skgSynced: context.skgSynced,
    nowMs: context.nowMs,
  });
}

function resolvePoisonedVehicleFreshnessFromContext(
  entity: Pick<CopEntityState, "observedAt" | "poisonedAtMs">,
  sysid: number,
  context: FleetFreshnessContext,
): VehicleFreshnessView {
  const telemetryLink =
    context.telemetryLink ?? context.getTelemetryLink?.(sysid);
  return evaluatePoisonedVehicleFreshness(entity, {
    telemetryLink,
    telemetryStatus: context.telemetryStatus,
    skgStatus: context.skgStatus,
    skgSynced: context.skgSynced,
    nowMs: context.nowMs,
  });
}

export function buildVehicleBannerView(
  vehicle: VehicleEntityState,
  freshness: VehicleFreshnessView | null = null,
): VehicleBannerView {
  const isStale = freshness !== null && !freshness.fresh;
  const overallSafety = evaluateVehicleSafetySeverity(vehicle, { isStale });
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
    freshness,
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
  freshnessContext?: FleetFreshnessContext,
):
  | VehicleBannerView
  | PoisonedVehicleBannerView
  | ExternalContactSummaryView
  | null {
  if (!entity) {
    return null;
  }
  if (entity.kind === "uxs-vehicle") {
    const freshness = freshnessContext
      ? resolveVehicleFreshnessFromContext(entity, freshnessContext)
      : null;
    return buildVehicleBannerView(entity, freshness);
  }
  if (entity.kind === "external-contact") {
    return buildExternalContactSummary(entity);
  }
  if (entity.poisoned) {
    const parsed = parseVehicleEntityId(entity.id);
    if (parsed !== null) {
      const freshness = freshnessContext
        ? resolvePoisonedVehicleFreshnessFromContext(
            entity,
            parsed.sysid,
            freshnessContext,
          )
        : null;
      return {
        kind: "poisoned-uxs-vehicle",
        entityId: entity.id,
        sysid: parsed.sysid,
        poisonError: entity.poisonError,
        freshness,
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

export type StreamPillTone = "connected" | "degraded" | "offline";

export interface StreamReadinessPill {
  readonly stream: "ws1" | "ws2";
  readonly label: "Telemetry" | "Fleet data";
  readonly status: WsConnectionStatus;
  readonly tone: StreamPillTone;
  readonly stateLabel: string;
  readonly detailLabel: string;
}

export interface StreamReadinessSummary {
  readonly telemetry: StreamReadinessPill;
  readonly fleetData: StreamReadinessPill;
}

export interface StreamReadinessOptions {
  readonly telemetryStatus: WsConnectionStatus;
  readonly telemetryVehicleCount: number;
  readonly skgStatus: WsConnectionStatus;
  readonly skgSynced: boolean;
  readonly skgLastSyncedRevision?: number | null;
  readonly skgEntityCount?: number;
}

function normalizeWsStatus(
  status: WsConnectionStatus | string | undefined,
): WsConnectionStatus {
  if (
    status === "connecting" ||
    status === "connected" ||
    status === "reconnecting" ||
    status === "disconnected"
  ) {
    return status;
  }
  return "disconnected";
}

/**
 * Formats operator-facing stream readiness pills (`Telemetry` and `Fleet data`)
 * while keeping technical WebSocket labels (`WS1`, `WS2`) in secondary details.
 */
export function summarizeStreamReadiness(
  options: StreamReadinessOptions,
): StreamReadinessSummary {
  const ws1Status = normalizeWsStatus(options.telemetryStatus);
  const ws2Status = normalizeWsStatus(options.skgStatus);
  const hasPriorSync =
    options.skgSynced ||
    (options.skgLastSyncedRevision !== undefined &&
      options.skgLastSyncedRevision !== null);
  const entityCount = options.skgEntityCount ?? 0;

  let telemetry: StreamReadinessPill;
  switch (ws1Status) {
    case "connected":
      telemetry = {
        stream: "ws1",
        label: "Telemetry",
        status: ws1Status,
        tone: "connected",
        stateLabel: `Live (${options.telemetryVehicleCount})`,
        detailLabel: `WS1 (/ws/telemetry) · connected · ${options.telemetryVehicleCount} kinematic streams`,
      };
      break;
    case "connecting":
      telemetry = {
        stream: "ws1",
        label: "Telemetry",
        status: ws1Status,
        tone: "degraded",
        stateLabel: "Connecting…",
        detailLabel: "WS1 (/ws/telemetry) · connecting",
      };
      break;
    case "reconnecting":
      telemetry = {
        stream: "ws1",
        label: "Telemetry",
        status: ws1Status,
        tone: "degraded",
        stateLabel: "Reconnecting…",
        detailLabel: "WS1 (/ws/telemetry) · reconnecting",
      };
      break;
    case "disconnected":
      telemetry = {
        stream: "ws1",
        label: "Telemetry",
        status: ws1Status,
        tone: "offline",
        stateLabel: "Offline",
        detailLabel: "WS1 (/ws/telemetry) · disconnected",
      };
      break;
  }

  let fleetData: StreamReadinessPill;
  switch (ws2Status) {
    case "connected":
      fleetData = options.skgSynced
        ? {
            stream: "ws2",
            label: "Fleet data",
            status: ws2Status,
            tone: "connected",
            stateLabel: `Live · synced (${entityCount})`,
            detailLabel: "WS2 (/ws/graph · ENTITY_STATES) · connected · synced",
          }
        : {
            stream: "ws2",
            label: "Fleet data",
            status: ws2Status,
            tone: "degraded",
            stateLabel: "Syncing…",
            detailLabel:
              "WS2 (/ws/graph · ENTITY_STATES) · connected · awaiting synced barrier",
          };
      break;
    case "connecting":
      fleetData = {
        stream: "ws2",
        label: "Fleet data",
        status: ws2Status,
        tone: "degraded",
        stateLabel: "Connecting…",
        detailLabel: "WS2 (/ws/graph · ENTITY_STATES) · connecting",
      };
      break;
    case "reconnecting":
      fleetData = {
        stream: "ws2",
        label: "Fleet data",
        status: ws2Status,
        tone: "degraded",
        stateLabel: hasPriorSync
          ? "Reconnecting · last known"
          : "Reconnecting…",
        detailLabel: "WS2 (/ws/graph · ENTITY_STATES) · reconnecting",
      };
      break;
    case "disconnected":
      fleetData = {
        stream: "ws2",
        label: "Fleet data",
        status: ws2Status,
        tone: "offline",
        stateLabel:
          hasPriorSync && entityCount > 0 ? "Offline · last known" : "Offline",
        detailLabel: "WS2 (/ws/graph · ENTITY_STATES) · disconnected",
      };
      break;
  }

  return { telemetry, fleetData };
}

export type FleetHealthState =
  "connecting" | "unavailable" | "empty" | "fault" | "stale" | "nominal";

export interface FleetHealthSummary extends FleetFaultSummary {
  readonly state: FleetHealthState;
  readonly badgeLabel: string;
  readonly exceptionsEmptyCopy: string;
  readonly freshVehicleCount: number;
  readonly staleVehicleCount: number;
  readonly hasStaleFaultContext: boolean;
  readonly oldestStaleAgeLabel: string | null;
}

/**
 * Evaluates fleet-wide readiness, observation freshness, and safety faults
 * without ever claiming `FLEET NOMINAL` before `ENTITY_STATES` sync, when zero
 * ownship UXS vehicles exist, or when any vehicle/fleet-data stream is stale.
 */
export function summarizeFleetHealth(
  entities: Iterable<CopEntityState>,
  context: FleetFreshnessContext = {},
): FleetHealthSummary {
  const entityList = Array.from(entities);
  const faultSummary = summarizeFleetFaults(entityList);
  const skgStatus = normalizeWsStatus(context.skgStatus ?? "connected");
  const skgSynced = context.skgSynced ?? true;
  const hasEverSynced =
    skgSynced ||
    (context.skgLastSyncedRevision !== undefined &&
      context.skgLastSyncedRevision !== null);
  const fleetDataLive = skgStatus === "connected" && skgSynced;

  const ownshipEntries = new Map<
    string,
    {
      readonly sysid: number;
      readonly id: string;
      readonly fresh: boolean;
      readonly ageMs: number | null;
      readonly hasFault: boolean;
    }
  >();

  for (const entity of entityList) {
    if (entity.kind === "uxs-vehicle") {
      const freshness = resolveVehicleFreshnessFromContext(entity, context);
      const fault = evaluateActiveVehicleFault(entity);
      ownshipEntries.set(entity.id, {
        sysid: entity.sysid,
        id: entity.id,
        fresh: freshness.fresh,
        ageMs: freshness.ageMs,
        hasFault: fault.severity !== null,
      });
    } else if (entity.poisoned) {
      const parsed = parseVehicleEntityId(entity.id);
      if (parsed !== null) {
        const freshness = resolvePoisonedVehicleFreshnessFromContext(
          entity,
          parsed.sysid,
          context,
        );
        ownshipEntries.set(entity.id, {
          sysid: parsed.sysid,
          id: entity.id,
          fresh: freshness.fresh,
          ageMs: freshness.ageMs,
          hasFault: true,
        });
      }
    }
  }

  let freshVehicleCount = 0;
  let staleVehicleCount = 0;
  let oldestStaleAgeMs: number | null = null;
  let oldestFaultStaleAgeMs: number | null = null;
  let anyFaultVehicleStale = false;

  for (const item of ownshipEntries.values()) {
    if (item.fresh) {
      freshVehicleCount += 1;
    } else {
      staleVehicleCount += 1;
      if (
        item.ageMs !== null &&
        (oldestStaleAgeMs === null || item.ageMs > oldestStaleAgeMs)
      ) {
        oldestStaleAgeMs = item.ageMs;
      }
      if (item.hasFault) {
        anyFaultVehicleStale = true;
        if (
          item.ageMs !== null &&
          (oldestFaultStaleAgeMs === null || item.ageMs > oldestFaultStaleAgeMs)
        ) {
          oldestFaultStaleAgeMs = item.ageMs;
        }
      }
    }
  }

  const oldestStaleAgeLabel =
    oldestStaleAgeMs !== null ? formatObservationAge(oldestStaleAgeMs) : null;
  const oldestFaultAgeLabel =
    oldestFaultStaleAgeMs !== null
      ? formatObservationAge(oldestFaultStaleAgeMs)
      : oldestStaleAgeLabel;
  const hasStaleFaultContext = !fleetDataLive || anyFaultVehicleStale;

  if (faultSummary.faultedCount > 0) {
    const countText =
      faultSummary.faultedCount === 1
        ? "1 FLEET FAULT"
        : `${faultSummary.faultedCount} FLEET FAULTS`;
    const detailText = faultSummary.firstFaultLabel
      ? ` (${faultSummary.firstFaultLabel})`
      : "";
    const staleQualifier = hasStaleFaultContext
      ? ` · LAST KNOWN${oldestFaultAgeLabel ? ` ${oldestFaultAgeLabel}` : ""}`
      : "";
    return {
      ...faultSummary,
      state: "fault",
      badgeLabel: `! ${countText}${detailText}${staleQualifier}`,
      exceptionsEmptyCopy: "Active fleet exceptions listed above.",
      freshVehicleCount,
      staleVehicleCount,
      hasStaleFaultContext,
      oldestStaleAgeLabel,
    };
  }

  if (!hasEverSynced) {
    if (skgStatus === "disconnected") {
      return {
        ...faultSummary,
        state: "unavailable",
        badgeLabel: "FLEET DATA UNAVAILABLE",
        exceptionsEmptyCopy:
          "Fleet health unavailable — Fleet data stream is offline.",
        freshVehicleCount,
        staleVehicleCount,
        hasStaleFaultContext: false,
        oldestStaleAgeLabel,
      };
    }
    return {
      ...faultSummary,
      state: "connecting",
      badgeLabel:
        skgStatus === "reconnecting"
          ? "RECONNECTING FLEET DATA…"
          : "AWAITING FLEET DATA…",
      exceptionsEmptyCopy: "Waiting for initial Fleet data sync…",
      freshVehicleCount,
      staleVehicleCount,
      hasStaleFaultContext: false,
      oldestStaleAgeLabel,
    };
  }

  if (faultSummary.totalVehicles === 0) {
    if (fleetDataLive) {
      return {
        ...faultSummary,
        state: "empty",
        badgeLabel: "No vehicles detected",
        exceptionsEmptyCopy: "No ownship UXS vehicles detected.",
        freshVehicleCount: 0,
        staleVehicleCount: 0,
        hasStaleFaultContext: false,
        oldestStaleAgeLabel: null,
      };
    }
    if (skgStatus === "disconnected") {
      return {
        ...faultSummary,
        state: "unavailable",
        badgeLabel: "FLEET DATA UNAVAILABLE",
        exceptionsEmptyCopy:
          "Fleet health unavailable — Fleet data stream is offline.",
        freshVehicleCount: 0,
        staleVehicleCount: 0,
        hasStaleFaultContext: false,
        oldestStaleAgeLabel: null,
      };
    }
    return {
      ...faultSummary,
      state: "connecting",
      badgeLabel:
        skgStatus === "reconnecting"
          ? "RECONNECTING FLEET DATA…"
          : "AWAITING FLEET DATA…",
      exceptionsEmptyCopy: "Reconnecting to Fleet data stream…",
      freshVehicleCount: 0,
      staleVehicleCount: 0,
      hasStaleFaultContext: false,
      oldestStaleAgeLabel: null,
    };
  }

  if (!fleetDataLive || staleVehicleCount > 0) {
    const agePart = oldestStaleAgeLabel ? ` · ${oldestStaleAgeLabel}` : "";
    const badgeLabel =
      !fleetDataLive || staleVehicleCount === faultSummary.totalVehicles
        ? `FLEET STALE · LAST KNOWN (${faultSummary.totalVehicles}${agePart})`
        : `FLEET STALE (${staleVehicleCount}/${faultSummary.totalVehicles} LAST KNOWN${agePart})`;
    const exceptionsEmptyCopy = !fleetDataLive
      ? "Fleet data stream offline — showing last known vehicle state."
      : "One or more vehicles are stale — live nominal health unconfirmed.";
    return {
      ...faultSummary,
      state: "stale",
      badgeLabel,
      exceptionsEmptyCopy,
      freshVehicleCount,
      staleVehicleCount,
      hasStaleFaultContext: false,
      oldestStaleAgeLabel,
    };
  }

  return {
    ...faultSummary,
    state: "nominal",
    badgeLabel: `FLEET NOMINAL (${faultSummary.totalVehicles})`,
    exceptionsEmptyCopy: `All ${faultSummary.totalVehicles} ownship UXS vehicles live and nominal.`,
    freshVehicleCount,
    staleVehicleCount: 0,
    hasStaleFaultContext: false,
    oldestStaleAgeLabel: null,
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

export interface FaultedOwnshipEntryView {
  readonly id: string;
  readonly sysid: number;
  readonly statusLabel: string;
  readonly staleLabel: string | null;
  readonly freshness: VehicleFreshnessView | null;
  readonly poisoned: boolean;
  readonly chips: readonly HealthChipView[];
}

/**
 * Builds deterministic COP Exceptions rows across decoded and poisoned-first
 * ownship UXS vehicles, applying the same conservative freshness rules as
 * `summarizeFleetHealth`, `selectBannerPresentation`, and `buildFilmstripItems`.
 */
export function buildFaultedOwnshipEntries(
  entities: Iterable<CopEntityState>,
  freshnessContext?: FleetFreshnessContext,
): readonly FaultedOwnshipEntryView[] {
  const rows: FaultedOwnshipEntryView[] = [];
  for (const entity of entities) {
    if (entity.kind === "uxs-vehicle") {
      const fault = evaluateActiveVehicleFault(entity);
      if (fault.severity !== null) {
        const freshView = freshnessContext
          ? resolveVehicleFreshnessFromContext(entity, freshnessContext)
          : null;
        rows.push({
          id: entity.id,
          sysid: entity.sysid,
          statusLabel: entity.authority,
          staleLabel:
            freshView && !freshView.fresh ? freshView.statusLabel : null,
          freshness: freshView,
          poisoned: entity.poisoned,
          chips: extractAbnormalHealthChips(
            buildHealthStrip(entity.healthFlags, entity.sysStatus),
          ),
        });
      }
    } else if (entity.poisoned) {
      const parsed = parseVehicleEntityId(entity.id);
      if (parsed !== null) {
        const freshView = freshnessContext
          ? resolvePoisonedVehicleFreshnessFromContext(
              entity,
              parsed.sysid,
              freshnessContext,
            )
          : null;
        rows.push({
          id: entity.id,
          sysid: parsed.sysid,
          statusLabel: "decode-error",
          staleLabel:
            freshView && !freshView.fresh ? freshView.statusLabel : null,
          freshness: freshView,
          poisoned: true,
          chips: [],
        });
      }
    }
  }
  rows.sort((a, b) => a.sysid - b.sysid || a.id.localeCompare(b.id));
  return rows;
}

export interface FilmstripItemView {
  readonly entityId: string;
  readonly shortcutKey: string | null;
  readonly kind: "uxs-vehicle" | "external-contact";
  readonly primaryLabel: string;
  readonly secondaryLabel: string;
  readonly safetySeverity: SafetyBadgeSeverity | null;
  readonly contactFeedStatus: CopFeedStatus | null;
  readonly stale?: boolean;
  readonly ageLabel?: string | null;
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
  freshnessContext?: FleetFreshnessContext,
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
      const freshness = freshnessContext
        ? resolveVehicleFreshnessFromContext(vehicle, freshnessContext)
        : null;
      const stale = freshness !== null && !freshness.fresh;
      const armLabel = vehicle.armed ? "ARMED" : "DISARMED";
      const baseSecondary = `#${vehicle.sysid} · ${authCode} · ${armLabel}`;
      const staleDetail =
        stale && freshness.decodeErrorAgeLabel
          ? `${freshness.ageLabel} · decode error ${freshness.decodeErrorAgeLabel}`
          : stale
            ? freshness.ageLabel
            : null;
      items.push({
        entityId: vehicle.id,
        shortcutKey,
        kind: "uxs-vehicle",
        primaryLabel: vehicle.title ?? `UXS #${vehicle.sysid}`,
        secondaryLabel: staleDetail
          ? `${baseSecondary} · LAST KNOWN (${staleDetail})`
          : baseSecondary,
        safetySeverity:
          activeFault.severity ??
          (vehicle.armed ? (stale ? "warning" : "success") : "nominal"),
        contactFeedStatus: null,
        stale,
        ageLabel: stale ? freshness.ageLabel : null,
      });
    } else {
      const pFresh = freshnessContext
        ? resolvePoisonedVehicleFreshnessFromContext(
            entry.poisonedEntity,
            entry.sysid,
            freshnessContext,
          )
        : null;
      const stale = pFresh !== null && !pFresh.fresh;
      const staleDetail =
        stale && pFresh.decodeErrorAgeLabel
          ? `${pFresh.ageLabel} · decode error ${pFresh.decodeErrorAgeLabel}`
          : stale
            ? pFresh.ageLabel
            : null;
      items.push({
        entityId: entry.id,
        shortcutKey,
        kind: "uxs-vehicle",
        primaryLabel: entry.poisonedEntity.title ?? `UXS #${entry.sysid}`,
        secondaryLabel: staleDetail
          ? `#${entry.sysid} · POISONED · LAST KNOWN (${staleDetail})`
          : `#${entry.sysid} · POISONED`,
        safetySeverity: "error",
        contactFeedStatus: null,
        stale,
        ageLabel: stale ? pFresh.ageLabel : null,
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
      stale:
        contact.feedStatus === "stale" || contact.feedStatus === "disconnected",
      ageLabel: null,
    });
  }

  return items;
}

/**
 * Canonical SKG store selector for FleetFilmstrip cards and 1..9 keyboard
 * shortcuts, ensuring poisoned-first ownship vehicle keys are ordered
 * identically in both the top-level hotkey listener and rendered filmstrip.
 */
export function selectFilmstripItems(
  skgStore: Pick<SkgStore, "vehicles" | "externalContacts" | "entities"> &
    Partial<Pick<SkgStore, "status" | "synced" | "lastSyncedRevision">>,
  freshnessContext?: FleetFreshnessContext,
): readonly FilmstripItemView[] {
  const mergedContext: FleetFreshnessContext | undefined = freshnessContext
    ? {
        ...freshnessContext,
        skgStatus: freshnessContext.skgStatus ?? skgStore.status,
        skgSynced: freshnessContext.skgSynced ?? skgStore.synced,
        skgLastSyncedRevision:
          freshnessContext.skgLastSyncedRevision !== undefined
            ? freshnessContext.skgLastSyncedRevision
            : skgStore.lastSyncedRevision,
      }
    : undefined;
  return buildFilmstripItems(
    skgStore.vehicles,
    skgStore.externalContacts,
    skgStore.entities.values(),
    mergedContext,
  );
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
