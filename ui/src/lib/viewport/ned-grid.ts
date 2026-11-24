// SPDX-License-Identifier: MIT
// Copyright (c) 2026 boxbotix

import { type UxsDomainKind, isFiniteNumber, isValidLatLon } from "../uxs";
import {
  type ResolvedVehiclePose,
  evaluateVehicleSafetySeverity,
} from "../hud/view-model";
import type { CopFeedStatus, VehicleEntityState } from "../stores/skg.svelte";
import type {
  ScreenProjection,
  UnprojectedWorldPoint,
  ViewportAdapter,
  ViewportCameraState,
  ViewportSize,
  WorldPoint,
} from "./adapter";

// Standard WGS-84 ellipsoid constants (open geodesy / Fossen Ch. 2).
export const WGS84_SEMI_MAJOR_M = 6_378_137.0;
export const WGS84_FLATTENING = 1 / 298.257223563;
const WGS84_ECCENTRICITY_SQ = WGS84_FLATTENING * (2 - WGS84_FLATTENING);

export const MIN_ZOOM_PX_PER_M = 0.05;
export const MAX_ZOOM_PX_PER_M = 80;
export const DEFAULT_ZOOM_PX_PER_M = 4;

// Stationary speed deadband suppressing noisy atan2(ve, vn) course jitter (D2 Option A).
export const USV_CRAB_SPEED_DEADBAND_MPS = 0.15;

export const DEFAULT_WAKE_RING_CAPACITY = 64;
export const DEFAULT_WAKE_MIN_STEP_M = 0.25;

const DEG_TO_RAD = Math.PI / 180;
const RAD_TO_DEG = 180 / Math.PI;
const TWO_PI = 2 * Math.PI;

export type TacticalGlyphKind =
  "quadcopter" | "rover" | "surface-boat" | "external-contact";

export type TacticalSafetyTone =
  | "error"
  | "warning"
  | "success"
  | "nominal"
  | "contact-live"
  | "contact-muted";

export interface TacticalPaletteColors {
  readonly tone: TacticalSafetyTone;
  readonly strokeHex: string;
  readonly fillHex: string;
}

export const TACTICAL_TONE_PALETTE: Readonly<
  Record<TacticalSafetyTone, TacticalPaletteColors>
> = {
  error: {
    tone: "error",
    strokeHex: "#ef4444",
    fillHex: "rgba(239, 68, 68, 0.24)",
  },
  warning: {
    tone: "warning",
    strokeHex: "#f59e0b",
    fillHex: "rgba(245, 158, 11, 0.24)",
  },
  success: {
    tone: "success",
    strokeHex: "#22c55e",
    fillHex: "rgba(34, 197, 94, 0.22)",
  },
  nominal: {
    tone: "nominal",
    strokeHex: "#60a5fa",
    fillHex: "rgba(96, 165, 250, 0.20)",
  },
  "contact-live": {
    tone: "contact-live",
    strokeHex: "#22d3ee",
    fillHex: "rgba(34, 211, 238, 0.18)",
  },
  "contact-muted": {
    tone: "contact-muted",
    strokeHex: "#94a3b8",
    fillHex: "rgba(148, 163, 184, 0.16)",
  },
};

export interface CrabAngleVectors {
  readonly bowRad: number;
  readonly courseOverGroundRad: number;
  readonly crabAngleRad: number;
  readonly groundspeedMps: number;
  readonly hasCourseSplit: boolean;
}

export interface NedSample {
  readonly northM: number;
  readonly eastM: number;
  readonly downM: number;
}

/**
 * Wraps any finite angle in radians into (-pi, pi], mapping -pi to +pi.
 */
export function wrapAngleSignedRad(rad: number): number {
  if (!isFiniteNumber(rad)) {
    return 0;
  }
  let wrapped = ((((rad + Math.PI) % TWO_PI) + TWO_PI) % TWO_PI) - Math.PI;
  if (wrapped <= -Math.PI) {
    wrapped += TWO_PI;
  }
  return Object.is(wrapped, -0) ? 0 : wrapped;
}

function wgs84RadiiAtLatRad(lat0Rad: number): {
  meridionalM: number;
  primeVerticalM: number;
} {
  const sinLat = Math.sin(lat0Rad);
  const denomSq = 1 - WGS84_ECCENTRICITY_SQ * sinLat * sinLat;
  const denom = Math.sqrt(denomSq);
  return {
    primeVerticalM: WGS84_SEMI_MAJOR_M / denom,
    meridionalM:
      (WGS84_SEMI_MAJOR_M * (1 - WGS84_ECCENTRICITY_SQ)) / (denomSq * denom),
  };
}

/**
 * Projects a WGS-84 [latDeg, lonDeg, altM] coordinate onto the local tangent
 * NED plane anchored at originLla ([lat0Deg, lon0Deg, alt0M]).
 */
export function llaToLocalNed(
  lla: readonly [number, number, number | null],
  originLla: readonly [number, number, number],
): [number, number, number] | null {
  const [latDeg, lonDeg, altM] = lla;
  const [lat0Deg, lon0Deg, alt0M] = originLla;
  if (
    !isValidLatLon(latDeg, lonDeg) ||
    !isValidLatLon(lat0Deg, lon0Deg) ||
    !isFiniteNumber(alt0M)
  ) {
    return null;
  }
  const resolvedAlt = altM !== null && isFiniteNumber(altM) ? altM : alt0M;
  const lat0Rad = lat0Deg * DEG_TO_RAD;
  const { meridionalM, primeVerticalM } = wgs84RadiiAtLatRad(lat0Rad);
  const cosLat0 = Math.max(Math.cos(lat0Rad), 1e-9);

  const dLatRad = (latDeg - lat0Deg) * DEG_TO_RAD;
  const dLonRad = wrapAngleSignedRad((lonDeg - lon0Deg) * DEG_TO_RAD);

  const northM = dLatRad * (meridionalM + alt0M);
  const eastM = dLonRad * (primeVerticalM + alt0M) * cosLat0;
  const downM = -(resolvedAlt - alt0M);
  return [northM, eastM, downM];
}

/**
 * Inverse of llaToLocalNed on the local tangent plane anchored at originLla.
 */
export function localNedToLla(
  ned: readonly [number, number, number],
  originLla: readonly [number, number, number],
): [number, number, number] | null {
  const [northM, eastM, downM] = ned;
  const [lat0Deg, lon0Deg, alt0M] = originLla;
  if (
    !isFiniteNumber(northM) ||
    !isFiniteNumber(eastM) ||
    !isFiniteNumber(downM) ||
    !isValidLatLon(lat0Deg, lon0Deg) ||
    !isFiniteNumber(alt0M)
  ) {
    return null;
  }
  const lat0Rad = lat0Deg * DEG_TO_RAD;
  const { meridionalM, primeVerticalM } = wgs84RadiiAtLatRad(lat0Rad);
  const cosLat0 = Math.max(Math.cos(lat0Rad), 1e-9);

  const dLatDeg = (northM / (meridionalM + alt0M)) * RAD_TO_DEG;
  const dLonDeg = (eastM / ((primeVerticalM + alt0M) * cosLat0)) * RAD_TO_DEG;
  const latDeg = Math.max(-90, Math.min(90, lat0Deg + dLatDeg));
  const lonDeg =
    wrapAngleSignedRad((lon0Deg + dLonDeg) * DEG_TO_RAD) * RAD_TO_DEG;
  const altM = alt0M - downM;
  return [latDeg, lonDeg, altM];
}

/**
 * Solves for the WGS-84 tangent-plane origin [lat0Deg, lon0Deg, alt0M] at
 * local NED = [0, 0, 0] from a verified co-registered (ned, lla) vehicle pair
 * so llaToLocalNed(lla, originLla) == ned even when initial vehicle NED is
 * nonzero (e.g. [100, -45, -20]).
 */
export function deriveNedOriginLla(
  ned: readonly [number, number, number],
  lla: readonly [number, number, number],
): [number, number, number] | null {
  const [northM, eastM, downM] = ned;
  const [latDeg, lonDeg, altM] = lla;
  if (
    !isFiniteNumber(northM) ||
    !isFiniteNumber(eastM) ||
    !isFiniteNumber(downM) ||
    !isValidLatLon(latDeg, lonDeg) ||
    !isFiniteNumber(altM) ||
    (latDeg === 0 && lonDeg === 0 && altM === 0)
  ) {
    return null;
  }

  const alt0M = altM + downM;
  if (!isFiniteNumber(alt0M) || alt0M <= -WGS84_SEMI_MAJOR_M * 0.5) {
    return null;
  }

  let lat0Deg = latDeg;
  for (let i = 0; i < 4; i += 1) {
    const { meridionalM } = wgs84RadiiAtLatRad(lat0Deg * DEG_TO_RAD);
    const nextLat0Deg = latDeg - (northM / (meridionalM + alt0M)) * RAD_TO_DEG;
    if (!isFiniteNumber(nextLat0Deg) || nextLat0Deg < -90 || nextLat0Deg > 90) {
      return null;
    }
    lat0Deg = nextLat0Deg;
  }

  const lat0Rad = lat0Deg * DEG_TO_RAD;
  const { primeVerticalM } = wgs84RadiiAtLatRad(lat0Rad);
  const cosLat0 = Math.max(Math.cos(lat0Rad), 1e-9);
  const dLonRad = eastM / ((primeVerticalM + alt0M) * cosLat0);
  if (!isFiniteNumber(dLonRad) || Math.abs(dLonRad) >= Math.PI) {
    return null;
  }
  const lon0Deg =
    wrapAngleSignedRad(lonDeg * DEG_TO_RAD - dLonRad) * RAD_TO_DEG;
  if (!isValidLatLon(lat0Deg, lon0Deg)) {
    return null;
  }

  return [lat0Deg, lon0Deg, alt0M];
}

export const SHARED_ORIGIN_MAX_DRIFT_M = 25;

function poseAgreesWithOrigin(
  ned: readonly [number, number, number],
  lla: readonly [number, number, number],
  originLla: readonly [number, number, number],
  maxDriftM = SHARED_ORIGIN_MAX_DRIFT_M,
): boolean {
  const projectedNed = llaToLocalNed(lla, originLla);
  if (!projectedNed) {
    return false;
  }
  return (
    Math.hypot(
      projectedNed[0] - ned[0],
      projectedNed[1] - ned[1],
      projectedNed[2] - ned[2],
    ) <= maxDriftM
  );
}

/**
 * Reconciles the viewport's shared NED<->LLA reference against the active
 * vehicle poses that carry both local NED and a verified geographic fix.
 * Clears any prior-session origin while no confirmed (ned, lla) reference is
 * active or when active confirmed vehicles report conflicting local frames,
 * and updates originLla whenever the active confirmed pair shifts local frame.
 */
export function anchorSharedFrameOrigin(
  adapter: ViewportAdapter,
  vehiclePoses: Iterable<
    Pick<ResolvedVehiclePose, "ned" | "lla"> & {
      readonly hasLocalPos?: boolean;
      readonly hasVerifiedFrameReference?: boolean;
      readonly stale?: boolean;
    }
  >,
): boolean {
  const allPoses = Array.from(vehiclePoses);
  const hasLivePose = allPoses.some((pose) => pose.stale === false);
  const activePool = hasLivePose
    ? allPoses.filter((pose) => pose.stale === false)
    : allPoses;

  const verified: Array<{
    ned: readonly [number, number, number];
    lla: readonly [number, number, number];
    originLla: [number, number, number];
  }> = [];

  for (const pose of activePool) {
    if (
      pose.hasVerifiedFrameReference === false ||
      pose.hasLocalPos === false ||
      pose.lla === null
    ) {
      continue;
    }
    const originLla = deriveNedOriginLla(pose.ned, pose.lla);
    if (originLla !== null) {
      verified.push({ ned: pose.ned, lla: pose.lla, originLla });
    }
  }

  if (verified.length === 0) {
    if (adapter.getCamera().originLla !== null) {
      adapter.setCamera({ originLla: null });
    }
    return false;
  }

  const primary = verified[0]!;
  for (let i = 1; i < verified.length; i += 1) {
    const peer = verified[i]!;
    if (!poseAgreesWithOrigin(peer.ned, peer.lla, primary.originLla)) {
      if (adapter.getCamera().originLla !== null) {
        adapter.setCamera({ originLla: null });
      }
      return false;
    }
  }

  const currentOrigin = adapter.getCamera().originLla;
  const currentOriginValid =
    currentOrigin !== null &&
    poseAgreesWithOrigin(primary.ned, primary.lla, currentOrigin, 1e-2) &&
    verified.every((item) =>
      poseAgreesWithOrigin(item.ned, item.lla, currentOrigin),
    );
  if (!currentOriginValid) {
    adapter.setCamera({ originLla: primary.originLla });
  }
  return true;
}

function clampZoom(zoomPxPerM: number): number {
  if (!isFiniteNumber(zoomPxPerM)) {
    return DEFAULT_ZOOM_PX_PER_M;
  }
  return Math.max(MIN_ZOOM_PX_PER_M, Math.min(MAX_ZOOM_PX_PER_M, zoomPxPerM));
}

export interface NedGridAdapterOptions {
  widthPx?: number;
  heightPx?: number;
  zoomPxPerM?: number;
  centerNed?: [number, number];
  bearingRad?: number;
  originLla?: [number, number, number] | null;
}

export class NedGridAdapter implements ViewportAdapter {
  private widthPx: number;
  private heightPx: number;
  private centerNorthM: number;
  private centerEastM: number;
  private zoomPxPerM: number;
  private bearingRad: number;
  private originLla: [number, number, number] | null;

  constructor(options: NedGridAdapterOptions = {}) {
    this.widthPx = Math.max(1, options.widthPx ?? 800);
    this.heightPx = Math.max(1, options.heightPx ?? 600);
    this.centerNorthM = options.centerNed?.[0] ?? 0;
    this.centerEastM = options.centerNed?.[1] ?? 0;
    this.zoomPxPerM = clampZoom(options.zoomPxPerM ?? DEFAULT_ZOOM_PX_PER_M);
    this.bearingRad = wrapAngleSignedRad(options.bearingRad ?? 0);
    this.originLla = options.originLla ? [...options.originLla] : null;
  }

  private resolveWorldPointNed(
    point: WorldPoint,
  ): [number, number, number] | null {
    if (
      point.ned &&
      isFiniteNumber(point.ned[0]) &&
      isFiniteNumber(point.ned[1]) &&
      isFiniteNumber(point.ned[2])
    ) {
      return [point.ned[0], point.ned[1], point.ned[2]];
    }
    if (point.lla && this.originLla) {
      return llaToLocalNed(point.lla, this.originLla);
    }
    return null;
  }

  project(point: WorldPoint): ScreenProjection {
    const ned = this.resolveWorldPointNed(point);
    if (!ned) {
      return { x: 0, y: 0, visible: false, rangeM: Number.NaN };
    }
    const dNorth = ned[0] - this.centerNorthM;
    const dEast = ned[1] - this.centerEastM;
    const rangeM = Math.hypot(dNorth, dEast);

    const cosB = Math.cos(this.bearingRad);
    const sinB = Math.sin(this.bearingRad);
    const upM = dNorth * cosB + dEast * sinB;
    const rightM = -dNorth * sinB + dEast * cosB;

    const x = this.widthPx * 0.5 + rightM * this.zoomPxPerM;
    const y = this.heightPx * 0.5 - upM * this.zoomPxPerM;
    const marginPx = 64;
    const visible =
      x >= -marginPx &&
      x <= this.widthPx + marginPx &&
      y >= -marginPx &&
      y <= this.heightPx + marginPx;

    return { x, y, visible, rangeM };
  }

  unproject(xPx: number, yPx: number): UnprojectedWorldPoint {
    const safeX = isFiniteNumber(xPx) ? xPx : this.widthPx * 0.5;
    const safeY = isFiniteNumber(yPx) ? yPx : this.heightPx * 0.5;
    const rightM = (safeX - this.widthPx * 0.5) / this.zoomPxPerM;
    const upM = (this.heightPx * 0.5 - safeY) / this.zoomPxPerM;

    const cosB = Math.cos(this.bearingRad);
    const sinB = Math.sin(this.bearingRad);
    const dNorth = upM * cosB - rightM * sinB;
    const dEast = upM * sinB + rightM * cosB;

    const ned: [number, number, number] = [
      this.centerNorthM + dNorth,
      this.centerEastM + dEast,
      0,
    ];
    const lla = this.originLla ? localNedToLla(ned, this.originLla) : null;
    return { ned, lla };
  }

  screenCourseRad(worldHeadingRad: number): number {
    if (!isFiniteNumber(worldHeadingRad)) {
      return 0;
    }
    return wrapAngleSignedRad(worldHeadingRad - this.bearingRad);
  }

  flyTo(point: WorldPoint, zoomPxPerM?: number): boolean {
    const ned = this.resolveWorldPointNed(point);
    if (!ned) {
      return false;
    }
    this.centerNorthM = ned[0];
    this.centerEastM = ned[1];
    if (zoomPxPerM !== undefined) {
      this.zoomPxPerM = clampZoom(zoomPxPerM);
    }
    return true;
  }

  setCamera(patch: Partial<ViewportCameraState>): void {
    if (
      patch.centerNed &&
      isFiniteNumber(patch.centerNed[0]) &&
      isFiniteNumber(patch.centerNed[1])
    ) {
      this.centerNorthM = patch.centerNed[0];
      this.centerEastM = patch.centerNed[1];
    }
    if (patch.zoomPxPerM !== undefined) {
      this.zoomPxPerM = clampZoom(patch.zoomPxPerM);
    }
    if (patch.bearingRad !== undefined) {
      this.bearingRad = wrapAngleSignedRad(patch.bearingRad);
    }
    if (patch.originLla !== undefined) {
      if (
        patch.originLla !== null &&
        isValidLatLon(patch.originLla[0], patch.originLla[1]) &&
        isFiniteNumber(patch.originLla[2])
      ) {
        this.originLla = [
          patch.originLla[0],
          patch.originLla[1],
          patch.originLla[2],
        ];
      } else if (patch.originLla === null) {
        this.originLla = null;
      }
    }
  }

  setViewportSize(widthPx: number, heightPx: number): void {
    if (isFiniteNumber(widthPx) && widthPx > 0) {
      this.widthPx = widthPx;
    }
    if (isFiniteNumber(heightPx) && heightPx > 0) {
      this.heightPx = heightPx;
    }
  }

  getCamera(): ViewportCameraState {
    return {
      centerNed: [this.centerNorthM, this.centerEastM],
      zoomPxPerM: this.zoomPxPerM,
      bearingRad: this.bearingRad,
      originLla: this.originLla ? [...this.originLla] : null,
    };
  }

  getViewportSize(): ViewportSize {
    return { widthPx: this.widthPx, heightPx: this.heightPx };
  }
}

/**
 * Computes USV / UXS bow heading versus velocity-over-ground course vectors
 * (Thor I. Fossen Ch. 2 crab-angle beta = chi - psi), suppressing course-split
 * jitter below USV_CRAB_SPEED_DEADBAND_MPS (0.15 m/s).
 */
export function computeCrabAngleVectors(
  headingRad: number,
  vnMps: number,
  veMps: number,
  deadbandMps = USV_CRAB_SPEED_DEADBAND_MPS,
): CrabAngleVectors {
  const bowRad = wrapAngleSignedRad(headingRad);
  const rawSpeed =
    isFiniteNumber(vnMps) && isFiniteNumber(veMps)
      ? Math.hypot(vnMps, veMps)
      : 0;
  const safeDeadband =
    isFiniteNumber(deadbandMps) && deadbandMps >= 0
      ? deadbandMps
      : USV_CRAB_SPEED_DEADBAND_MPS;

  if (rawSpeed < safeDeadband) {
    return {
      bowRad,
      courseOverGroundRad: bowRad,
      crabAngleRad: 0,
      groundspeedMps: rawSpeed,
      hasCourseSplit: false,
    };
  }

  const courseOverGroundRad = Math.atan2(veMps, vnMps);
  const crabAngleRad = wrapAngleSignedRad(courseOverGroundRad - bowRad);
  return {
    bowRad,
    courseOverGroundRad,
    crabAngleRad,
    groundspeedMps: rawSpeed,
    hasCourseSplit: true,
  };
}

export function resolveDomainGlyphKind(
  domain: UxsDomainKind,
): TacticalGlyphKind {
  return domain;
}

export function resolveVehicleSafetyTone(
  vehicle: Pick<
    VehicleEntityState,
    "poisoned" | "authority" | "armed" | "healthFlags" | "sysStatus"
  >,
): TacticalPaletteColors {
  return TACTICAL_TONE_PALETTE[evaluateVehicleSafetySeverity(vehicle)];
}

export function resolveContactPalette(
  feedStatus: CopFeedStatus,
): TacticalPaletteColors {
  return feedStatus === "live"
    ? TACTICAL_TONE_PALETTE["contact-live"]
    : TACTICAL_TONE_PALETTE["contact-muted"];
}

/**
 * Bounded per-entity breadcrumb wake buffer sampled on significant horizontal
 * motion so stationary hover/idle frames do not evict historical wake trails.
 */
export class WakeTrailBook {
  private readonly capacity: number;
  private readonly minStepM: number;
  private readonly trails = new Map<string, NedSample[]>();

  constructor(
    capacity = DEFAULT_WAKE_RING_CAPACITY,
    minStepM = DEFAULT_WAKE_MIN_STEP_M,
  ) {
    this.capacity = Math.max(2, Math.trunc(capacity));
    this.minStepM = Math.max(0, minStepM);
  }

  record(entityId: string, ned: readonly [number, number, number]): void {
    if (
      !isFiniteNumber(ned[0]) ||
      !isFiniteNumber(ned[1]) ||
      !isFiniteNumber(ned[2])
    ) {
      return;
    }
    let samples = this.trails.get(entityId);
    if (!samples) {
      samples = [];
      this.trails.set(entityId, samples);
    }
    const last = samples[samples.length - 1];
    if (last) {
      const dist = Math.hypot(ned[0] - last.northM, ned[1] - last.eastM);
      if (dist < this.minStepM) {
        return;
      }
    }
    if (samples.length >= this.capacity) {
      samples.shift();
    }
    samples.push({ northM: ned[0], eastM: ned[1], downM: ned[2] });
  }

  getTrail(entityId: string): readonly NedSample[] {
    return this.trails.get(entityId) ?? [];
  }

  pruneExcept(activeIds: ReadonlySet<string>): void {
    for (const key of this.trails.keys()) {
      if (!activeIds.has(key)) {
        this.trails.delete(key);
      }
    }
  }

  clear(): void {
    this.trails.clear();
  }
}
