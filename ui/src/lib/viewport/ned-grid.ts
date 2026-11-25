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
export const MAX_FRAME_PAIR_SKEW_MS = 500;
export const MAX_ASYNC_MOTION_SPEED_MPS = 60;

interface VerifiedOriginBaseline {
  readonly ned: readonly [number, number, number];
  readonly lla: readonly [number, number, number];
  readonly localPosObservedAtMs: number | null | undefined;
  readonly globalPosObservedAtMs: number | null | undefined;
}

export interface SharedFrameOriginOptions {
  readonly sessionKey?: string;
}

interface VerifiedOriginCandidate extends VerifiedOriginBaseline {
  readonly originLla: [number, number, number];
}

interface AdapterOriginState {
  sessionKey: string | null;
  consistentMaxLocalMs: number | null;
  consistentMaxGlobalMs: number | null;
  consistentSnapshots: VerifiedOriginBaseline[];
  isWithheld: boolean;
  withheldFromOriginLla: [number, number, number] | null;
  holdMaxLocalMs: number | null;
  holdMaxGlobalMs: number | null;
  holdOriginSnapshots: VerifiedOriginBaseline[];
  holdSnapshots: VerifiedOriginBaseline[];
}

const adapterOriginStates = new WeakMap<ViewportAdapter, AdapterOriginState>();

function createEmptyOriginState(): AdapterOriginState {
  return {
    sessionKey: null,
    consistentMaxLocalMs: null,
    consistentMaxGlobalMs: null,
    consistentSnapshots: [],
    isWithheld: false,
    withheldFromOriginLla: null,
    holdMaxLocalMs: null,
    holdMaxGlobalMs: null,
    holdOriginSnapshots: [],
    holdSnapshots: [],
  };
}

function getOrCreateOriginState(adapter: ViewportAdapter): AdapterOriginState {
  let state = adapterOriginStates.get(adapter);
  if (!state) {
    state = createEmptyOriginState();
    adapterOriginStates.set(adapter, state);
  }
  return state;
}

function resetOriginState(state: AdapterOriginState): void {
  state.consistentMaxLocalMs = null;
  state.consistentMaxGlobalMs = null;
  state.consistentSnapshots = [];
  state.isWithheld = false;
  state.withheldFromOriginLla = null;
  state.holdMaxLocalMs = null;
  state.holdMaxGlobalMs = null;
  state.holdOriginSnapshots = [];
  state.holdSnapshots = [];
}

/**
 * Clears cached shared-frame origin watermarks, hold state, and camera origin
 * for an adapter when switching feeds or tearing down an active session.
 */
export function resetSharedFrameOrigin(adapter: ViewportAdapter): void {
  const state = adapterOriginStates.get(adapter);
  if (state) {
    resetOriginState(state);
    state.sessionKey = null;
  }
  if (adapter.getCamera().originLla !== null) {
    adapter.setCamera({ originLla: null });
  }
}

function tuple3Equals(
  a: readonly [number, number, number],
  b: readonly [number, number, number],
  eps = 1e-9,
): boolean {
  return (
    Math.abs(a[0] - b[0]) <= eps &&
    Math.abs(a[1] - b[1]) <= eps &&
    Math.abs(a[2] - b[2]) <= eps
  );
}

function maxObservedMs(
  current: number | null,
  candidate: number | null | undefined,
): number | null {
  if (!isFiniteNumber(candidate)) {
    return current;
  }
  return current === null ? candidate : Math.max(current, candidate);
}

function hasContemporaneousObservations(
  localPosObservedAtMs: number | null | undefined,
  globalPosObservedAtMs: number | null | undefined,
): boolean {
  if (
    localPosObservedAtMs === undefined &&
    globalPosObservedAtMs === undefined
  ) {
    return true;
  }
  if (
    !isFiniteNumber(localPosObservedAtMs) ||
    !isFiniteNumber(globalPosObservedAtMs)
  ) {
    return false;
  }
  return (
    Math.abs(localPosObservedAtMs - globalPosObservedAtMs) <=
    MAX_FRAME_PAIR_SKEW_MS
  );
}

function toOriginBaseline(
  candidate: VerifiedOriginBaseline,
): VerifiedOriginBaseline {
  return {
    ned: [candidate.ned[0], candidate.ned[1], candidate.ned[2]],
    lla: [candidate.lla[0], candidate.lla[1], candidate.lla[2]],
    localPosObservedAtMs: candidate.localPosObservedAtMs,
    globalPosObservedAtMs: candidate.globalPosObservedAtMs,
  };
}

function originDisagreementMeters(
  ned: readonly [number, number, number],
  lla: readonly [number, number, number],
  originLla: readonly [number, number, number],
): number {
  const projectedNed = llaToLocalNed(lla, originLla);
  if (!projectedNed) {
    return Number.POSITIVE_INFINITY;
  }
  return Math.hypot(
    projectedNed[0] - ned[0],
    projectedNed[1] - ned[1],
    projectedNed[2] - ned[2],
  );
}

function poseAgreesWithOrigin(
  ned: readonly [number, number, number],
  lla: readonly [number, number, number],
  originLla: readonly [number, number, number],
  maxDriftM = SHARED_ORIGIN_MAX_DRIFT_M,
): boolean {
  return originDisagreementMeters(ned, lla, originLla) <= maxDriftM;
}

function compareOriginCandidates(
  a: VerifiedOriginCandidate,
  b: VerifiedOriginCandidate,
): number {
  const aHasTs =
    isFiniteNumber(a.localPosObservedAtMs) &&
    isFiniteNumber(a.globalPosObservedAtMs);
  const bHasTs =
    isFiniteNumber(b.localPosObservedAtMs) &&
    isFiniteNumber(b.globalPosObservedAtMs);
  if (aHasTs !== bHasTs) {
    return aHasTs ? -1 : 1;
  }
  if (aHasTs && bHasTs) {
    const aPairMin = Math.min(
      a.localPosObservedAtMs!,
      a.globalPosObservedAtMs!,
    );
    const bPairMin = Math.min(
      b.localPosObservedAtMs!,
      b.globalPosObservedAtMs!,
    );
    if (aPairMin !== bPairMin) {
      return bPairMin - aPairMin;
    }
    const aSkew = Math.abs(a.localPosObservedAtMs! - a.globalPosObservedAtMs!);
    const bSkew = Math.abs(b.localPosObservedAtMs! - b.globalPosObservedAtMs!);
    if (aSkew !== bSkew) {
      return aSkew - bSkew;
    }
  }
  for (let i = 0; i < 3; i += 1) {
    if (a.originLla[i] !== b.originLla[i]) {
      return a.originLla[i]! - b.originLla[i]!;
    }
  }
  for (let i = 0; i < 3; i += 1) {
    if (a.ned[i] !== b.ned[i]) {
      return a.ned[i]! - b.ned[i]!;
    }
  }
  return 0;
}

function selectCanonicalOriginCandidate(
  candidates: readonly VerifiedOriginCandidate[],
): VerifiedOriginCandidate | null {
  if (candidates.length === 0) {
    return null;
  }
  let best = candidates[0]!;
  for (let i = 1; i < candidates.length; i += 1) {
    const candidate = candidates[i]!;
    if (compareOriginCandidates(candidate, best) < 0) {
      best = candidate;
    }
  }
  return best;
}

function hasAdvancedPastWatermarks(
  candidate: VerifiedOriginBaseline,
  maxLocalMs: number | null,
  maxGlobalMs: number | null,
  priorSnapshots: readonly VerifiedOriginBaseline[],
): boolean {
  const hasCandidateTimestamps =
    isFiniteNumber(candidate.localPosObservedAtMs) &&
    isFiniteNumber(candidate.globalPosObservedAtMs);

  if (hasCandidateTimestamps && (maxLocalMs !== null || maxGlobalMs !== null)) {
    const localNewer =
      maxLocalMs === null || candidate.localPosObservedAtMs > maxLocalMs;
    const globalNewer =
      maxGlobalMs === null || candidate.globalPosObservedAtMs > maxGlobalMs;
    return localNewer && globalNewer;
  }

  for (const snap of priorSnapshots) {
    if (
      tuple3Equals(candidate.ned, snap.ned) &&
      tuple3Equals(candidate.lla, snap.lla)
    ) {
      return false;
    }
  }
  return true;
}

function isConfirmedRelocation(
  candidate: VerifiedOriginBaseline,
  referenceOriginLla: readonly [number, number, number],
  maxLocalMs: number | null,
  maxGlobalMs: number | null,
  referenceSnapshots: readonly VerifiedOriginBaseline[],
): boolean {
  if (poseAgreesWithOrigin(candidate.ned, candidate.lla, referenceOriginLla)) {
    return false;
  }
  if (
    !hasContemporaneousObservations(
      candidate.localPosObservedAtMs,
      candidate.globalPosObservedAtMs,
    )
  ) {
    return false;
  }
  if (
    !hasAdvancedPastWatermarks(
      candidate,
      maxLocalMs,
      maxGlobalMs,
      referenceSnapshots,
    )
  ) {
    return false;
  }

  const driftM = originDisagreementMeters(
    candidate.ned,
    candidate.lla,
    referenceOriginLla,
  );
  if (
    isFiniteNumber(candidate.localPosObservedAtMs) &&
    isFiniteNumber(candidate.globalPosObservedAtMs)
  ) {
    const skewMs = Math.abs(
      candidate.localPosObservedAtMs - candidate.globalPosObservedAtMs,
    );
    if (skewMs > 0) {
      const maxAsyncExplainableM =
        SHARED_ORIGIN_MAX_DRIFT_M +
        (skewMs / 1_000) * MAX_ASYNC_MOTION_SPEED_MPS;
      if (driftM <= maxAsyncExplainableM) {
        return false;
      }
      for (const snap of referenceSnapshots) {
        if (
          candidate.localPosObservedAtMs > candidate.globalPosObservedAtMs &&
          tuple3Equals(candidate.lla, snap.lla)
        ) {
          return false;
        }
        if (
          candidate.globalPosObservedAtMs > candidate.localPosObservedAtMs &&
          tuple3Equals(candidate.ned, snap.ned)
        ) {
          return false;
        }
      }
    }
    return true;
  }

  for (const snap of referenceSnapshots) {
    if (tuple3Equals(candidate.lla, snap.lla)) {
      return false;
    }
    if (tuple3Equals(candidate.ned, snap.ned) && driftM <= 1_000) {
      return false;
    }
  }
  return true;
}

function recordConsistentPoses(
  state: AdapterOriginState,
  poses: readonly VerifiedOriginBaseline[],
): void {
  for (const pose of poses) {
    if (
      isFiniteNumber(pose.localPosObservedAtMs) &&
      isFiniteNumber(pose.globalPosObservedAtMs)
    ) {
      state.consistentMaxLocalMs = maxObservedMs(
        state.consistentMaxLocalMs,
        pose.localPosObservedAtMs,
      );
      state.consistentMaxGlobalMs = maxObservedMs(
        state.consistentMaxGlobalMs,
        pose.globalPosObservedAtMs,
      );
    }
  }
  state.consistentSnapshots = poses.map(toOriginBaseline);
  state.isWithheld = false;
  state.withheldFromOriginLla = null;
  state.holdMaxLocalMs = null;
  state.holdMaxGlobalMs = null;
  state.holdOriginSnapshots = [];
  state.holdSnapshots = [];
}

function enterHoldState(
  state: AdapterOriginState,
  blockedPoses: readonly VerifiedOriginBaseline[],
): void {
  let nextMaxLocalMs = maxObservedMs(
    state.holdMaxLocalMs,
    state.consistentMaxLocalMs,
  );
  let nextMaxGlobalMs = maxObservedMs(
    state.holdMaxGlobalMs,
    state.consistentMaxGlobalMs,
  );
  for (const pose of blockedPoses) {
    nextMaxLocalMs = maxObservedMs(nextMaxLocalMs, pose.localPosObservedAtMs);
    nextMaxGlobalMs = maxObservedMs(
      nextMaxGlobalMs,
      pose.globalPosObservedAtMs,
    );
  }
  const mergedSnapshots = [
    ...state.holdSnapshots,
    ...state.consistentSnapshots,
    ...blockedPoses.map(toOriginBaseline),
  ];
  state.isWithheld = true;
  state.withheldFromOriginLla = null;
  state.holdMaxLocalMs = nextMaxLocalMs;
  state.holdMaxGlobalMs = nextMaxGlobalMs;
  state.holdOriginSnapshots = [];
  state.holdSnapshots = mergedSnapshots.slice(-16);
  state.consistentMaxLocalMs = null;
  state.consistentMaxGlobalMs = null;
  state.consistentSnapshots = [];
}

function enterDivergenceHoldState(
  state: AdapterOriginState,
  candidatePoses: readonly VerifiedOriginBaseline[],
  priorOriginLla: readonly [number, number, number],
): void {
  const divergedOnly = candidatePoses.filter(
    (pose) => !poseAgreesWithOrigin(pose.ned, pose.lla, priorOriginLla),
  );
  state.isWithheld = true;
  state.withheldFromOriginLla = [
    priorOriginLla[0],
    priorOriginLla[1],
    priorOriginLla[2],
  ];
  state.holdMaxLocalMs = maxObservedMs(
    state.holdMaxLocalMs,
    state.consistentMaxLocalMs,
  );
  state.holdMaxGlobalMs = maxObservedMs(
    state.holdMaxGlobalMs,
    state.consistentMaxGlobalMs,
  );
  state.holdOriginSnapshots = [...state.consistentSnapshots];
  state.holdSnapshots = [
    ...state.consistentSnapshots,
    ...divergedOnly.map(toOriginBaseline),
  ].slice(-16);
  state.consistentMaxLocalMs = null;
  state.consistentMaxGlobalMs = null;
  state.consistentSnapshots = [];
}

function canRestorePriorOrigin(
  candidate: VerifiedOriginBaseline,
  priorOriginLla: readonly [number, number, number],
  state: AdapterOriginState,
): boolean {
  return (
    poseAgreesWithOrigin(candidate.ned, candidate.lla, priorOriginLla) &&
    hasContemporaneousObservations(
      candidate.localPosObservedAtMs,
      candidate.globalPosObservedAtMs,
    ) &&
    hasAdvancedPastWatermarks(
      candidate,
      state.holdMaxLocalMs,
      state.holdMaxGlobalMs,
      state.holdSnapshots,
    )
  );
}

function canRelocateFromPriorHold(
  candidate: VerifiedOriginBaseline,
  priorOriginLla: readonly [number, number, number],
  state: AdapterOriginState,
): boolean {
  if (
    !hasAdvancedPastWatermarks(
      candidate,
      state.holdMaxLocalMs,
      state.holdMaxGlobalMs,
      state.holdSnapshots,
    )
  ) {
    return false;
  }
  return isConfirmedRelocation(
    candidate,
    priorOriginLla,
    state.holdMaxLocalMs,
    state.holdMaxGlobalMs,
    state.holdOriginSnapshots,
  );
}

function canLiftConflictHold(
  candidate: VerifiedOriginBaseline,
  state: AdapterOriginState,
): boolean {
  return (
    hasContemporaneousObservations(
      candidate.localPosObservedAtMs,
      candidate.globalPosObservedAtMs,
    ) &&
    hasAdvancedPastWatermarks(
      candidate,
      state.holdMaxLocalMs,
      state.holdMaxGlobalMs,
      state.holdSnapshots,
    )
  );
}

function collectVerifiedBaselines(
  poses: ReadonlyArray<{
    readonly ned: readonly [number, number, number];
    readonly lla: readonly [number, number, number] | null;
    readonly hasLocalPos?: boolean;
    readonly hasVerifiedFrameReference?: boolean;
    readonly localPosObservedAtMs?: number | null;
    readonly globalPosObservedAtMs?: number | null;
  }>,
): VerifiedOriginCandidate[] {
  const out: VerifiedOriginCandidate[] = [];
  for (const pose of poses) {
    if (
      pose.hasVerifiedFrameReference === false ||
      pose.hasLocalPos === false ||
      pose.lla === null
    ) {
      continue;
    }
    const originLla = deriveNedOriginLla(pose.ned, pose.lla);
    if (originLla !== null) {
      out.push({
        ned: pose.ned,
        lla: pose.lla,
        localPosObservedAtMs: pose.localPosObservedAtMs,
        globalPosObservedAtMs: pose.globalPosObservedAtMs,
        originLla,
      });
    }
  }
  return out;
}

/**
 * Reconciles the viewport's shared NED<->LLA reference against live vehicle
 * poses that carry both local NED and a verified geographic fix.
 * Keeps an established reference steady across ordinary asynchronous local or
 * global updates, requires the candidate supplying any displayed reference to
 * qualify individually in deterministic order, clears session state when the
 * entity set is empty or the endpoint changes, and preserves holds across
 * timeouts when entities remain present until new confirming observations arrive.
 */
export function anchorSharedFrameOrigin(
  adapter: ViewportAdapter,
  vehiclePoses: Iterable<
    Pick<ResolvedVehiclePose, "ned" | "lla"> & {
      readonly hasLocalPos?: boolean;
      readonly hasVerifiedFrameReference?: boolean;
      readonly stale?: boolean;
      readonly localPosObservedAtMs?: number | null;
      readonly globalPosObservedAtMs?: number | null;
    }
  >,
  options?: SharedFrameOriginOptions,
): boolean {
  const state = getOrCreateOriginState(adapter);
  let currentOrigin = adapter.getCamera().originLla;
  if (options?.sessionKey !== undefined) {
    if (state.sessionKey !== null && state.sessionKey !== options.sessionKey) {
      if (currentOrigin !== null) {
        adapter.setCamera({ originLla: null });
        currentOrigin = null;
      }
      resetOriginState(state);
    }
    state.sessionKey = options.sessionKey;
  }

  if (
    currentOrigin === null &&
    (state.consistentMaxLocalMs !== null ||
      state.consistentMaxGlobalMs !== null ||
      state.consistentSnapshots.length > 0)
  ) {
    state.consistentMaxLocalMs = null;
    state.consistentMaxGlobalMs = null;
    state.consistentSnapshots = [];
  }
  if (currentOrigin !== null && state.isWithheld) {
    state.isWithheld = false;
    state.withheldFromOriginLla = null;
    state.holdMaxLocalMs = null;
    state.holdMaxGlobalMs = null;
    state.holdOriginSnapshots = [];
    state.holdSnapshots = [];
  }

  const allPoses = Array.from(vehiclePoses);
  if (allPoses.length === 0) {
    if (currentOrigin !== null) {
      adapter.setCamera({ originLla: null });
    }
    resetOriginState(state);
    return false;
  }

  const livePoses = allPoses.filter((pose) => pose.stale !== true);

  if (livePoses.length === 0) {
    if (currentOrigin === null) {
      return false;
    }
    const staleStillConsistent = allPoses.every(
      (pose) =>
        pose.hasVerifiedFrameReference !== false &&
        pose.hasLocalPos !== false &&
        pose.lla !== null &&
        poseAgreesWithOrigin(pose.ned, pose.lla, currentOrigin),
    );
    if (!staleStillConsistent) {
      adapter.setCamera({ originLla: null });
      enterHoldState(state, collectVerifiedBaselines(allPoses));
      return false;
    }
    return true;
  }

  const verified = collectVerifiedBaselines(livePoses);

  if (verified.length === 0) {
    const staleVerified = collectVerifiedBaselines(allPoses);
    if (currentOrigin !== null) {
      adapter.setCamera({ originLla: null });
      enterHoldState(state, staleVerified);
    } else if (state.isWithheld || staleVerified.length > 0) {
      enterHoldState(state, staleVerified);
    }
    return false;
  }

  for (let i = 0; i < verified.length; i += 1) {
    const base = verified[i]!;
    for (let j = i + 1; j < verified.length; j += 1) {
      const peer = verified[j]!;
      if (
        !poseAgreesWithOrigin(peer.ned, peer.lla, base.originLla) ||
        !poseAgreesWithOrigin(base.ned, base.lla, peer.originLla)
      ) {
        if (currentOrigin !== null) {
          adapter.setCamera({ originLla: null });
        }
        enterHoldState(state, verified);
        return false;
      }
    }
  }

  if (currentOrigin !== null) {
    const allAgreeWithCurrent = verified.every((item) =>
      poseAgreesWithOrigin(item.ned, item.lla, currentOrigin),
    );
    if (allAgreeWithCurrent) {
      recordConsistentPoses(state, verified);
      return true;
    }

    const relocatingCandidates = verified.filter((item) =>
      isConfirmedRelocation(
        item,
        currentOrigin,
        state.consistentMaxLocalMs,
        state.consistentMaxGlobalMs,
        state.consistentSnapshots,
      ),
    );
    const chosenRelocation =
      selectCanonicalOriginCandidate(relocatingCandidates);
    if (chosenRelocation === null) {
      adapter.setCamera({ originLla: null });
      enterDivergenceHoldState(state, verified, currentOrigin);
      return false;
    }

    adapter.setCamera({ originLla: chosenRelocation.originLla });
    recordConsistentPoses(state, verified);
    return true;
  }

  if (state.isWithheld) {
    if (state.withheldFromOriginLla !== null) {
      const priorOrigin = state.withheldFromOriginLla;
      const allAgreeWithPrior = verified.every((item) =>
        poseAgreesWithOrigin(item.ned, item.lla, priorOrigin),
      );
      if (allAgreeWithPrior) {
        const restoreConfirmed = verified.some((item) =>
          canRestorePriorOrigin(item, priorOrigin, state),
        );
        if (!restoreConfirmed) {
          return false;
        }
        adapter.setCamera({ originLla: priorOrigin });
        recordConsistentPoses(state, verified);
        return true;
      }

      const relocatingFromHold = verified.filter((item) =>
        canRelocateFromPriorHold(item, priorOrigin, state),
      );
      const chosenHoldRelocation =
        selectCanonicalOriginCandidate(relocatingFromHold);
      if (chosenHoldRelocation === null) {
        return false;
      }
      adapter.setCamera({ originLla: chosenHoldRelocation.originLla });
      recordConsistentPoses(state, verified);
      return true;
    }

    const conflictRecoveryCandidates = verified.filter((item) =>
      canLiftConflictHold(item, state),
    );
    const chosenRecovery = selectCanonicalOriginCandidate(
      conflictRecoveryCandidates,
    );
    if (chosenRecovery === null) {
      return false;
    }
    adapter.setCamera({ originLla: chosenRecovery.originLla });
    recordConsistentPoses(state, verified);
    return true;
  }

  const initialCandidates = verified.filter((item) =>
    hasContemporaneousObservations(
      item.localPosObservedAtMs,
      item.globalPosObservedAtMs,
    ),
  );
  const chosenInitial = selectCanonicalOriginCandidate(initialCandidates);
  if (chosenInitial === null) {
    return false;
  }

  adapter.setCamera({ originLla: chosenInitial.originLla });
  recordConsistentPoses(state, verified);
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
  options?: { isStale?: boolean },
): TacticalPaletteColors {
  return TACTICAL_TONE_PALETTE[evaluateVehicleSafetySeverity(vehicle, options)];
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

export interface RangeRingLabelPosition {
  readonly x: number;
  readonly y: number;
  readonly placement: "north" | "east" | "southeast" | "south";
}

/**
 * Resolves an unobstructed screen coordinate for a concentric range-ring label
 * so northern ring arcs never collide with the floating .viewport-toolbar band.
 */
export function resolveRangeRingLabelPosition(
  cx: number,
  cy: number,
  radiusPx: number,
  widthPx: number,
  heightPx: number,
  toolbarBottomPx: number,
): RangeRingLabelPosition | null {
  if (
    !isFiniteNumber(cx) ||
    !isFiniteNumber(cy) ||
    !isFiniteNumber(radiusPx) ||
    radiusPx <= 0
  ) {
    return null;
  }
  const safeToolbarBottom = isFiniteNumber(toolbarBottomPx)
    ? Math.max(0, toolbarBottomPx)
    : 0;
  // 11px monospace ascenders + 5px breathing room beneath toolbar bottom.
  const safeBaselineMinY = safeToolbarBottom + 16;
  const maxLabelX = widthPx - 44;
  const maxBaselineY = heightPx - 8;

  const northX = cx + 6;
  const northY = cy - radiusPx + 13;
  if (
    northY >= safeBaselineMinY &&
    northY <= maxBaselineY &&
    northX >= 6 &&
    northX <= maxLabelX
  ) {
    return { x: northX, y: northY, placement: "north" };
  }

  const eastX = cx + radiusPx + 6;
  const eastY = Math.max(safeBaselineMinY, cy - 5);
  if (eastX >= 6 && eastX <= maxLabelX && eastY <= maxBaselineY) {
    return { x: eastX, y: eastY, placement: "east" };
  }

  const diagOffset = radiusPx * Math.SQRT1_2;
  const seX = cx + diagOffset + 5;
  const seY = cy + diagOffset - 4;
  if (
    seX >= 6 &&
    seX <= maxLabelX &&
    seY >= safeBaselineMinY &&
    seY <= maxBaselineY
  ) {
    return { x: seX, y: seY, placement: "southeast" };
  }

  const southX = cx + 6;
  const southY = cy + radiusPx - 6;
  if (
    southY >= safeBaselineMinY &&
    southY <= maxBaselineY &&
    southX >= 6 &&
    southX <= maxLabelX
  ) {
    return { x: southX, y: southY, placement: "south" };
  }

  return null;
}
