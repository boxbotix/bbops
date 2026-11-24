// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Complete 360 Studio
// Copyright (c) 2026 boxbotix
//
// Provenance: Adapted from ENTITY_STATES kv-watch store pattern in
// C360Studio/semboids ui/src/lib/stores/graph.svelte.ts
// @ 37dbdb088438a2601c8556d012ab2c33bec85da2 (MIT).

import { SvelteMap } from "svelte/reactivity";
import {
  type CompanionMode,
  type ControlAuthority,
  type HealthFlags,
  type LinkTier,
  type UxsDomainKind,
  isCompanionMode,
  isControlAuthority,
  isFiniteNumber,
  isLinkTier,
  isNonNegativeInt,
  isRecord,
  isUxsDomainKind,
  isValidEntityIdString,
  isValidHeadingDeg,
  isValidLatLon,
  parseFiniteTuple3,
  parseFiniteTuple4,
  parseHealthFlags,
  parseSixPartEntityId,
  parseVehicleEntityId,
} from "../uxs";
import {
  ManagedWebSocketClient,
  type WebSocketLike,
  type WsConnectionStatus,
} from "./ws-client";

export const DEFAULT_GRAPH_WS_PATH = "/ws/graph";
export const ENTITY_STATES_BUCKET = "ENTITY_STATES";

export const COP_FEED_STATUSES = [
  "live",
  "stale",
  "degraded",
  "disconnected",
  "unknown",
] as const;
export type CopFeedStatus = (typeof COP_FEED_STATUSES)[number];

export interface SkgTriple {
  subject: string;
  predicate: string;
  object: unknown;
  source: string;
  timestamp: string;
  confidence: number;
  context?: string;
  datatype?: string;
  expires_at?: string;
}

export interface GeoLocation {
  latitude: number;
  longitude: number;
  altitude: number | null;
}

export interface VehicleSysStatus {
  failsafes: number;
  crcErrors: number;
  loopDtUs: number;
  maxLoopDtUs: number;
  rttUs: number;
}

export interface CopEntityBase {
  id: string;
  kind: "uxs-vehicle" | "external-contact" | "generic";
  title: string | null;
  location: GeoLocation | null;
  kvRevision: number;
  updatedAt: string | null;
  poisoned: boolean;
  poisonError: string | null;
  indexingProfile: string | null;
  source: string | null;
  observedAt: string | null;
  receivedAt: string | null;
  expiresAt: string | null;
  uncertaintyMeters: number | null;
  feedStatus: CopFeedStatus;
  ingestedAtMs?: number;
  triples: readonly SkgTriple[];
}

export interface VehicleEntityState extends CopEntityBase {
  kind: "uxs-vehicle";
  org: string;
  platform: string;
  sysid: number;
  domain: UxsDomainKind;
  authority: ControlAuthority;
  companionMode: CompanionMode;
  linkTier: LinkTier;
  armed: boolean;
  healthFlags: HealthFlags;
  sysStatus: VehicleSysStatus;
  attitudeQuaternion: [number, number, number, number];
  attitudeRates: [number, number, number];
  ned: [number, number, number];
  velocityNed: [number, number, number];
  headingDeg: number;
  location: {
    latitude: number;
    longitude: number;
    altitude: number;
  };
  hasAttitude?: boolean;
  hasLocalPos?: boolean;
  hasGlobalPos?: boolean;
  hasHeading?: boolean;
  poseObservedAt?: string | null;
  localPosObservedAt?: string | null;
  globalPosObservedAt?: string | null;
  poseIngestedAtMs?: number | null;
  poseIngestedLive?: boolean;
}

export interface ExternalContactEntityState extends CopEntityBase {
  kind: "external-contact";
  contactType: string | null;
}

export interface GenericEntityState extends CopEntityBase {
  kind: "generic";
}

export type CopEntityState =
  VehicleEntityState | ExternalContactEntityState | GenericEntityState;

export interface StoredEntityStateWire {
  id: string;
  triples: unknown[];
  updated_at?: string;
}

export interface SkgStoreOptions {
  url?: string;
  keepaliveIntervalMs?: number;
  initialBackoffMs?: number;
  maxBackoffMs?: number;
  now?: () => number;
  createSocket?: (url: string) => WebSocketLike;
}

export interface SkgStore {
  readonly status: WsConnectionStatus;
  readonly url: string;
  readonly synced: boolean;
  readonly lastSyncedRevision: number | null;
  readonly entities: SvelteMap<string, CopEntityState>;
  readonly vehicles: readonly VehicleEntityState[];
  readonly commandableVehicles: readonly VehicleEntityState[];
  readonly externalContacts: readonly ExternalContactEntityState[];
  readonly malformedFrameCount: number;
  connect(overrideUrl?: string): void;
  disconnect(): void;
  getEntity(entityId: string | null | undefined): CopEntityState | null;
  getVehicle(entityId: string | null | undefined): VehicleEntityState | null;
  getCommandableVehicle(
    entityId: string | null | undefined,
  ): VehicleEntityState | null;
  ingestRawFrame(rawText: string): void;
  reset(): void;
}

export function isVehicleEntity(
  entity: CopEntityState | null | undefined,
): entity is VehicleEntityState {
  return (
    entity !== null && entity !== undefined && entity.kind === "uxs-vehicle"
  );
}

export function isCommandableVehicle(
  entity: CopEntityState | null | undefined,
): entity is VehicleEntityState & { poisoned: false } {
  return isVehicleEntity(entity) && !entity.poisoned;
}

export function isExternalContactEntity(
  entity: CopEntityState | null | undefined,
): entity is ExternalContactEntityState {
  return (
    entity !== null &&
    entity !== undefined &&
    entity.kind === "external-contact"
  );
}

function normalizeKnownTimestamp(value: unknown): string | null {
  if (typeof value !== "string") {
    return null;
  }
  const trimmed = value.trim();
  if (trimmed.length === 0) {
    return null;
  }
  const epochMs = Date.parse(trimmed);
  if (Number.isNaN(epochMs) || epochMs <= 0) {
    return null;
  }
  return new Date(epochMs).toISOString();
}

function normalizeNonEmptyString(value: unknown): string | null {
  if (typeof value !== "string") {
    return null;
  }
  const trimmed = value.trim();
  return trimmed.length > 0 ? trimmed : null;
}

function parseFeedStatus(value: unknown): CopFeedStatus {
  if (
    typeof value === "string" &&
    (COP_FEED_STATUSES as readonly string[]).includes(value)
  ) {
    return value as CopFeedStatus;
  }
  return "unknown";
}

const GO_ZERO_TIME_NS = -62_135_596_800_000_000_000n; // 0001-01-01T00:00:00Z
const RFC3339_NANO_RE =
  /^(\d{4})-(\d{2})-(\d{2})[T ](\d{2}):(\d{2}):(\d{2})(?:\.(\d{1,9}))?(Z|[+-]\d{2}:\d{2})$/i;
const utf8Encoder = new TextEncoder();

function parseTripleInstantNs(raw: string): bigint {
  const trimmed = raw.trim();
  if (trimmed.length === 0) {
    return GO_ZERO_TIME_NS;
  }
  const match = RFC3339_NANO_RE.exec(trimmed);
  if (match) {
    const frac = (match[7] ?? "").padEnd(9, "0");
    const tz = match[8]!.toUpperCase();
    const wholeSecIso = `${match[1]}-${match[2]}-${match[3]}T${match[4]}:${match[5]}:${match[6]}${tz}`;
    const utcMs = Date.parse(wholeSecIso);
    if (Number.isNaN(utcMs)) {
      return GO_ZERO_TIME_NS;
    }
    return BigInt(utcMs) * 1_000_000n + BigInt(frac);
  }
  const fallbackMs = Date.parse(trimmed);
  if (Number.isNaN(fallbackMs)) {
    return GO_ZERO_TIME_NS;
  }
  return BigInt(fallbackMs) * 1_000_000n;
}

function compareUtf8Lexicographic(a: string, b: string): number {
  if (a === b) {
    return 0;
  }
  const bytesA = utf8Encoder.encode(a);
  const bytesB = utf8Encoder.encode(b);
  const limit = Math.min(bytesA.length, bytesB.length);
  for (let i = 0; i < limit; i += 1) {
    const diff = bytesA[i]! - bytesB[i]!;
    if (diff !== 0) {
      return diff;
    }
  }
  return bytesA.length - bytesB.length;
}

/**
 * Mirrors SemEngine alpha.7 `graph.EntityState.GetTriple` (`pickLatest`):
 * latest Timestamp wins; on equal Timestamp, lexicographically smaller Source
 * wins; on equal Timestamp and Source, the first stored assertion wins.
 */
function selectWinningTriple(
  incumbent: SkgTriple | undefined,
  candidate: SkgTriple,
): SkgTriple {
  if (incumbent === undefined) {
    return candidate;
  }
  const candTs = parseTripleInstantNs(candidate.timestamp);
  const bestTs = parseTripleInstantNs(incumbent.timestamp);
  if (candTs > bestTs) {
    return candidate;
  }
  if (
    candTs === bestTs &&
    compareUtf8Lexicographic(candidate.source, incumbent.source) < 0
  ) {
    return candidate;
  }
  return incumbent;
}

function parseSkgTriple(raw: unknown): SkgTriple | null {
  if (!isRecord(raw)) {
    return null;
  }
  if (typeof raw.subject !== "string" || typeof raw.predicate !== "string") {
    return null;
  }
  const triple: SkgTriple = {
    subject: raw.subject,
    predicate: raw.predicate,
    object: raw.object,
    source: typeof raw.source === "string" ? raw.source : "",
    timestamp: typeof raw.timestamp === "string" ? raw.timestamp : "",
    confidence: isFiniteNumber(raw.confidence) ? raw.confidence : 1.0,
  };
  if (typeof raw.context === "string" && raw.context.length > 0) {
    triple.context = raw.context;
  }
  if (typeof raw.datatype === "string" && raw.datatype.length > 0) {
    triple.datatype = raw.datatype;
  }
  if (typeof raw.expires_at === "string" && raw.expires_at.length > 0) {
    triple.expires_at = raw.expires_at;
  }
  return triple;
}

function extractCommonMetadata(
  id: string,
  triples: readonly SkgTriple[],
  byPredicate: Record<string, SkgTriple | undefined>,
  updatedAtRaw: unknown,
  kvRevision: number,
): Omit<CopEntityBase, "kind"> {
  const title = normalizeNonEmptyString(byPredicate["dc.terms.title"]?.object);
  const indexingProfile = normalizeNonEmptyString(
    byPredicate["entity.indexing.profile"]?.object,
  );
  const updatedAt = normalizeKnownTimestamp(updatedAtRaw);

  const latObj = byPredicate["geo.location.latitude"]?.object;
  const lonObj = byPredicate["geo.location.longitude"]?.object;
  const altObj = byPredicate["geo.location.altitude"]?.object;
  let location: GeoLocation | null = null;
  if (
    isFiniteNumber(latObj) &&
    isFiniteNumber(lonObj) &&
    isValidLatLon(latObj, lonObj)
  ) {
    location = {
      latitude: latObj,
      longitude: lonObj,
      altitude: isFiniteNumber(altObj) ? altObj : null,
    };
  }

  const explicitSource =
    normalizeNonEmptyString(byPredicate["cop.contact.source"]?.object) ??
    normalizeNonEmptyString(byPredicate["cop.provenance.source"]?.object) ??
    normalizeNonEmptyString(byPredicate["prov.wasAttributedTo"]?.object);

  const explicitObserved =
    normalizeKnownTimestamp(byPredicate["cop.time.observed"]?.object) ??
    normalizeKnownTimestamp(byPredicate["cop.contact.observed_at"]?.object);

  const receivedAt =
    normalizeKnownTimestamp(byPredicate["cop.time.received"]?.object) ??
    normalizeKnownTimestamp(byPredicate["cop.contact.received_at"]?.object);

  const expiresAt =
    normalizeKnownTimestamp(byPredicate["cop.time.expires"]?.object) ??
    normalizeKnownTimestamp(byPredicate["cop.contact.expires_at"]?.object);

  const rawUncertainty =
    byPredicate["cop.location.uncertainty_m"]?.object ??
    byPredicate["geo.location.uncertainty_m"]?.object ??
    byPredicate["cop.contact.ce_m"]?.object;
  const uncertaintyMeters =
    isFiniteNumber(rawUncertainty) && rawUncertainty >= 0
      ? rawUncertainty
      : null;

  const rawFeedStatus =
    byPredicate["cop.feed.status"]?.object ??
    byPredicate["cop.contact.feed_status"]?.object;

  return {
    id,
    title,
    location,
    kvRevision,
    updatedAt,
    poisoned: false,
    poisonError: null,
    indexingProfile,
    source: explicitSource,
    observedAt: explicitObserved,
    receivedAt,
    expiresAt,
    uncertaintyMeters,
    feedStatus: parseFeedStatus(rawFeedStatus),
    triples,
  };
}

export function parseCopEntityState(
  stored: unknown,
  kvRevision: number,
  expectedKey?: string,
  ingestedAtMs?: number,
): CopEntityState | null {
  if (!isRecord(stored) || !isValidEntityIdString(stored.id)) {
    return null;
  }
  if (expectedKey !== undefined && stored.id !== expectedKey) {
    return null;
  }
  if (!Array.isArray(stored.triples)) {
    return null;
  }

  const triples: SkgTriple[] = [];
  const byPredicate: Record<string, SkgTriple | undefined> = Object.create(
    null,
  ) as Record<string, SkgTriple | undefined>;
  for (const rawTriple of stored.triples) {
    const parsed = parseSkgTriple(rawTriple);
    if (!parsed) {
      return null;
    }
    triples.push(parsed);
    byPredicate[parsed.predicate] = selectWinningTriple(
      byPredicate[parsed.predicate],
      parsed,
    );
  }

  const base = extractCommonMetadata(
    stored.id,
    triples,
    byPredicate,
    stored.updated_at,
    kvRevision,
  );
  if (ingestedAtMs !== undefined && isFiniteNumber(ingestedAtMs)) {
    base.ingestedAtMs = ingestedAtMs;
  }

  const vehicleIdentity = parseVehicleEntityId(stored.id);
  if (vehicleIdentity !== null) {
    const domain = byPredicate["uxs.domain.kind"]?.object;
    const authority = byPredicate["uxs.authority.state"]?.object;
    const companionMode = byPredicate["uxs.mode.companion"]?.object;
    const linkTier = byPredicate["uxs.link.tier"]?.object;
    const armed = byPredicate["uxs.health.armed"]?.object;
    const healthFlags = parseHealthFlags(
      byPredicate["uxs.health.flags"]?.object,
    );
    const failsafes = byPredicate["uxs.health.failsafes"]?.object;
    const crcErrors = byPredicate["uxs.health.crcerrors"]?.object;
    const loopDtUs = byPredicate["uxs.health.loopdtus"]?.object;
    const maxLoopDtUs = byPredicate["uxs.health.maxloopdtus"]?.object;
    const rttUs = byPredicate["uxs.health.rttus"]?.object;
    const quat = parseFiniteTuple4(
      byPredicate["uxs.attitude.quaternion"]?.object,
    );
    const rates = parseFiniteTuple3(byPredicate["uxs.attitude.rates"]?.object);
    const ned = parseFiniteTuple3(byPredicate["uxs.kinematics.ned"]?.object);
    const vx = byPredicate["uxs.kinematics.vx"]?.object;
    const vy = byPredicate["uxs.kinematics.vy"]?.object;
    const vz = byPredicate["uxs.kinematics.vz"]?.object;
    const headingDeg = byPredicate["uxs.kinematics.headingdeg"]?.object;
    const lat = byPredicate["geo.location.latitude"]?.object;
    const lon = byPredicate["geo.location.longitude"]?.object;
    const alt = byPredicate["geo.location.altitude"]?.object;

    if (
      isUxsDomainKind(domain) &&
      isControlAuthority(authority) &&
      isCompanionMode(companionMode) &&
      isLinkTier(linkTier) &&
      typeof armed === "boolean" &&
      healthFlags !== null &&
      isNonNegativeInt(failsafes) &&
      isNonNegativeInt(crcErrors) &&
      isNonNegativeInt(loopDtUs) &&
      isNonNegativeInt(maxLoopDtUs) &&
      isNonNegativeInt(rttUs) &&
      quat !== null &&
      rates !== null &&
      ned !== null &&
      isFiniteNumber(vx) &&
      isFiniteNumber(vy) &&
      isFiniteNumber(vz) &&
      isValidHeadingDeg(headingDeg) &&
      isFiniteNumber(lat) &&
      isFiniteNumber(lon) &&
      isValidLatLon(lat, lon) &&
      isFiniteNumber(alt)
    ) {
      let latestUxsTriple: SkgTriple | undefined;
      for (const triple of triples) {
        if (triple.predicate.startsWith("uxs.")) {
          latestUxsTriple = selectWinningTriple(latestUxsTriple, triple);
        }
      }
      const posePredicates = [
        "uxs.attitude.quaternion",
        "uxs.attitude.rates",
        "uxs.kinematics.ned",
        "uxs.kinematics.vx",
        "uxs.kinematics.vy",
        "uxs.kinematics.vz",
        "uxs.kinematics.headingdeg",
        "geo.location.latitude",
        "geo.location.longitude",
        "geo.location.altitude",
      ] as const;
      const isMeasuredAssertion = (
        triple: SkgTriple | undefined,
      ): triple is SkgTriple =>
        triple !== undefined &&
        triple.confidence > 0 &&
        normalizeKnownTimestamp(triple.timestamp) !== null;

      let latestPoseTriple: SkgTriple | undefined;
      for (const pred of posePredicates) {
        const candidate = byPredicate[pred];
        if (isMeasuredAssertion(candidate)) {
          latestPoseTriple = selectWinningTriple(latestPoseTriple, candidate);
        }
      }

      const quatTriple = byPredicate["uxs.attitude.quaternion"];
      const nedTriple = byPredicate["uxs.kinematics.ned"];
      const hdgTriple = byPredicate["uxs.kinematics.headingdeg"];
      const latTriple = byPredicate["geo.location.latitude"];
      const lonTriple = byPredicate["geo.location.longitude"];
      const quatNormSq =
        quat[0] * quat[0] +
        quat[1] * quat[1] +
        quat[2] * quat[2] +
        quat[3] * quat[3];
      const wrappedHeading = ((headingDeg % 360) + 360) % 360;
      const headingDistFromNorth = Math.min(
        wrappedHeading,
        360 - wrappedHeading,
      );
      const hasNonZeroRates =
        rates[0] !== 0 || rates[1] !== 0 || rates[2] !== 0;
      const isUnmeasuredIdentityQuat =
        quatTriple?.context !== "measured" &&
        Math.abs(quat[0] - 1) < 1e-9 &&
        Math.abs(quat[1]) < 1e-9 &&
        Math.abs(quat[2]) < 1e-9 &&
        Math.abs(quat[3]) < 1e-9 &&
        (headingDistFromNorth > 0.5 || !hasNonZeroRates);
      const hasAttitude =
        isMeasuredAssertion(quatTriple) &&
        quatNormSq >= 1e-6 &&
        !isUnmeasuredIdentityQuat;
      const hasNonZeroLocalKinematics =
        ned[0] !== 0 ||
        ned[1] !== 0 ||
        ned[2] !== 0 ||
        vx !== 0 ||
        vy !== 0 ||
        vz !== 0;
      const hasLocalPos =
        isMeasuredAssertion(nedTriple) &&
        (nedTriple.context === "measured" || hasNonZeroLocalKinematics);
      const hasGlobalPos =
        isMeasuredAssertion(latTriple) &&
        isMeasuredAssertion(lonTriple) &&
        (latTriple.context === "measured" ||
          lat !== 0 ||
          lon !== 0 ||
          alt !== 0);
      const hasHeading =
        isMeasuredAssertion(hdgTriple) &&
        (hdgTriple.context === "measured" || headingDeg !== 0 || hasAttitude);

      const poseObservedAt = normalizeKnownTimestamp(
        latestPoseTriple?.timestamp,
      );
      const localPosObservedAt = hasLocalPos
        ? normalizeKnownTimestamp(nedTriple?.timestamp)
        : null;
      const latestGlobalPosTriple =
        latTriple !== undefined && lonTriple !== undefined
          ? selectWinningTriple(latTriple, lonTriple)
          : (latTriple ?? lonTriple);
      const globalPosObservedAt = hasGlobalPos
        ? normalizeKnownTimestamp(latestGlobalPosTriple?.timestamp)
        : null;
      const vehicleObservedAt =
        base.observedAt ?? normalizeKnownTimestamp(latestUxsTriple?.timestamp);
      const vehicleSource =
        base.source ?? normalizeNonEmptyString(latestUxsTriple?.source);
      return {
        ...base,
        source: vehicleSource,
        observedAt: vehicleObservedAt,
        poseObservedAt,
        localPosObservedAt,
        globalPosObservedAt,
        hasAttitude,
        hasLocalPos,
        hasGlobalPos,
        hasHeading,
        kind: "uxs-vehicle",
        org: vehicleIdentity.org,
        platform: vehicleIdentity.platform,
        sysid: vehicleIdentity.sysid,
        domain,
        authority,
        companionMode,
        linkTier,
        armed,
        healthFlags,
        sysStatus: {
          failsafes,
          crcErrors,
          loopDtUs,
          maxLoopDtUs,
          rttUs,
        },
        attitudeQuaternion: quat,
        attitudeRates: rates,
        ned,
        velocityNed: [vx, vy, vz],
        headingDeg,
        location: {
          latitude: lat,
          longitude: lon,
          altitude: alt,
        },
      };
    }
  }

  const sixParts = parseSixPartEntityId(stored.id);
  const hasCopDomain = sixParts !== null && sixParts[3] === "cop";
  const hasCopPredicate = triples.some((t) => t.predicate.startsWith("cop."));
  if (hasCopDomain || hasCopPredicate) {
    const contactType =
      normalizeNonEmptyString(byPredicate["cop.contact.type"]?.object) ??
      normalizeNonEmptyString(byPredicate["cop.marker.type"]?.object) ??
      (hasCopDomain && sixParts ? sixParts[4] : null);
    return {
      ...base,
      kind: "external-contact",
      contactType,
    };
  }

  return {
    ...base,
    kind: "generic",
  };
}

function buildPoisonedEntity(
  key: string,
  prior: CopEntityState | undefined,
  revision: number,
  errorMessage: string,
  ingestedAtMs?: number,
): CopEntityState {
  if (prior !== undefined) {
    return {
      ...prior,
      kvRevision: revision,
      poisoned: true,
      poisonError: errorMessage,
      ...(ingestedAtMs !== undefined ? { ingestedAtMs } : {}),
    };
  }
  const sixParts = parseSixPartEntityId(key);
  if (sixParts !== null && sixParts[3] === "cop") {
    return {
      id: key,
      kind: "external-contact",
      contactType: sixParts[4],
      title: null,
      location: null,
      kvRevision: revision,
      updatedAt: null,
      poisoned: true,
      poisonError: errorMessage,
      indexingProfile: null,
      source: null,
      observedAt: null,
      receivedAt: null,
      expiresAt: null,
      uncertaintyMeters: null,
      feedStatus: "unknown",
      ...(ingestedAtMs !== undefined ? { ingestedAtMs } : {}),
      triples: [],
    };
  }
  return {
    id: key,
    kind: "generic",
    title: null,
    location: null,
    kvRevision: revision,
    updatedAt: null,
    poisoned: true,
    poisonError: errorMessage,
    indexingProfile: null,
    source: null,
    observedAt: null,
    receivedAt: null,
    expiresAt: null,
    uncertaintyMeters: null,
    feedStatus: "unknown",
    ...(ingestedAtMs !== undefined ? { ingestedAtMs } : {}),
    triples: [],
  };
}

function tuplesEqual(a: readonly number[], b: readonly number[]): boolean {
  if (a.length !== b.length) {
    return false;
  }
  for (let i = 0; i < a.length; i += 1) {
    if (a[i] !== b[i]) {
      return false;
    }
  }
  return true;
}

const EXPIRED_POSE_RECEIPT_MS = -1e15;

function didVehiclePoseAdvance(
  prior: CopEntityState | undefined,
  next: VehicleEntityState,
): boolean {
  if (!next.poseObservedAt) {
    return false;
  }
  if (
    prior === undefined ||
    prior.kind !== "uxs-vehicle" ||
    !prior.poseObservedAt
  ) {
    return true;
  }
  const nextNs = parseTripleInstantNs(next.poseObservedAt);
  const priorNs = parseTripleInstantNs(prior.poseObservedAt);
  if (nextNs > priorNs) {
    return true;
  }
  if (nextNs < priorNs) {
    return false;
  }
  return (
    next.hasAttitude !== prior.hasAttitude ||
    next.hasLocalPos !== prior.hasLocalPos ||
    next.hasGlobalPos !== prior.hasGlobalPos ||
    next.hasHeading !== prior.hasHeading ||
    next.headingDeg !== prior.headingDeg ||
    next.location.latitude !== prior.location.latitude ||
    next.location.longitude !== prior.location.longitude ||
    next.location.altitude !== prior.location.altitude ||
    !tuplesEqual(next.attitudeQuaternion, prior.attitudeQuaternion) ||
    !tuplesEqual(next.attitudeRates, prior.attitudeRates) ||
    !tuplesEqual(next.ned, prior.ned) ||
    !tuplesEqual(next.velocityNed, prior.velocityNed)
  );
}

interface VehiclePoseHighWater {
  observedNs: bigint;
  pose: VehicleEntityState;
  ingestedAtMs: number | null;
  ingestedLive: boolean;
}

function applyVehiclePoseReceipt(
  highWater: VehiclePoseHighWater | undefined,
  next: VehicleEntityState,
  receiptMs: number,
  isLivePostSync: boolean,
): VehiclePoseHighWater | undefined {
  const nextPoseObs = next.poseObservedAt;
  if (!nextPoseObs) {
    next.poseIngestedAtMs =
      highWater !== undefined ? EXPIRED_POSE_RECEIPT_MS : null;
    next.poseIngestedLive = false;
    return highWater;
  }

  const nextNs = parseTripleInstantNs(nextPoseObs);
  if (highWater === undefined || nextNs > highWater.observedNs) {
    next.poseIngestedAtMs = receiptMs;
    next.poseIngestedLive = isLivePostSync;
    return {
      observedNs: nextNs,
      pose: next,
      ingestedAtMs: next.poseIngestedAtMs,
      ingestedLive: next.poseIngestedLive,
    };
  }

  if (nextNs < highWater.observedNs) {
    next.poseIngestedAtMs = EXPIRED_POSE_RECEIPT_MS;
    next.poseIngestedLive = false;
    return highWater;
  }

  if (didVehiclePoseAdvance(highWater.pose, next)) {
    next.poseIngestedAtMs = receiptMs;
    next.poseIngestedLive = isLivePostSync;
    return {
      observedNs: nextNs,
      pose: next,
      ingestedAtMs: next.poseIngestedAtMs,
      ingestedLive: next.poseIngestedLive,
    };
  }

  next.poseIngestedAtMs =
    highWater.ingestedAtMs ?? (isLivePostSync ? null : EXPIRED_POSE_RECEIPT_MS);
  next.poseIngestedLive = isLivePostSync ? highWater.ingestedLive : false;
  if (!isLivePostSync) {
    return {
      observedNs: highWater.observedNs,
      pose: next,
      ingestedAtMs: next.poseIngestedAtMs,
      ingestedLive: false,
    };
  }
  return highWater;
}

export function createSkgStore(options: SkgStoreOptions = {}): SkgStore {
  const now = options.now ?? (() => Date.now());
  const envUrl =
    typeof import.meta !== "undefined" && import.meta.env
      ? (import.meta.env.VITE_GRAPH_WS_URL as string | undefined)
      : undefined;

  const entities = new SvelteMap<string, CopEntityState>();
  // D2 Option A: pre-synced shadow buffer intentionally stays off Svelte reactive tracking.
  // eslint-disable-next-line svelte/prefer-svelte-reactivity
  const stagedSnapshot = new Map<string, CopEntityState>();
  // eslint-disable-next-line svelte/prefer-svelte-reactivity
  const maxPoseByKey = new Map<string, VehiclePoseHighWater>();
  // eslint-disable-next-line svelte/prefer-svelte-reactivity
  const stagedMaxPoseByKey = new Map<string, VehiclePoseHighWater>();
  let malformedFrames = 0;

  let status = $state<WsConnectionStatus>("disconnected");
  let synced = $state(false);
  let lastSyncedRevision = $state<number | null>(null);

  const vehicles = $derived.by(() => {
    const list: VehicleEntityState[] = [];
    for (const entity of entities.values()) {
      if (entity.kind === "uxs-vehicle") {
        list.push(entity);
      }
    }
    list.sort((a, b) => a.sysid - b.sysid);
    return list;
  });

  const commandableVehicles = $derived.by(() =>
    vehicles.filter((v) => !v.poisoned),
  );

  const externalContacts = $derived.by(() => {
    const list: ExternalContactEntityState[] = [];
    for (const entity of entities.values()) {
      if (entity.kind === "external-contact") {
        list.push(entity);
      }
    }
    list.sort((a, b) => a.id.localeCompare(b.id));
    return list;
  });

  function clearFeedState(): void {
    entities.clear();
    stagedSnapshot.clear();
    maxPoseByKey.clear();
    stagedMaxPoseByKey.clear();
    synced = false;
    lastSyncedRevision = null;
    malformedFrames = 0;
  }

  function commitStagedSnapshot(syncRevision: number): void {
    for (const existingKey of Array.from(entities.keys())) {
      if (!stagedSnapshot.has(existingKey)) {
        entities.delete(existingKey);
        maxPoseByKey.delete(existingKey);
      }
    }
    for (const trackedKey of Array.from(maxPoseByKey.keys())) {
      const stagedEntity = stagedSnapshot.get(trackedKey);
      if (!stagedEntity || stagedEntity.kind !== "uxs-vehicle") {
        maxPoseByKey.delete(trackedKey);
      }
    }
    for (const [key, entity] of stagedSnapshot.entries()) {
      entities.set(key, entity);
      if (entity.kind === "uxs-vehicle") {
        const nextHighWater = stagedMaxPoseByKey.get(key);
        if (nextHighWater !== undefined) {
          maxPoseByKey.set(key, nextHighWater);
        }
      } else {
        maxPoseByKey.delete(key);
      }
    }
    stagedSnapshot.clear();
    stagedMaxPoseByKey.clear();
    synced = true;
    lastSyncedRevision = syncRevision;
  }

  function ingestRawFrame(rawText: string): void {
    let parsed: unknown;
    try {
      parsed = JSON.parse(rawText);
    } catch {
      malformedFrames += 1;
      return;
    }
    if (!isRecord(parsed) || typeof parsed.type !== "string") {
      malformedFrames += 1;
      return;
    }
    if (parsed.bucket !== undefined && parsed.bucket !== ENTITY_STATES_BUCKET) {
      return;
    }

    const revision = isNonNegativeInt(parsed.revision) ? parsed.revision : 0;

    if (parsed.type === "synced") {
      commitStagedSnapshot(revision);
      return;
    }

    if (!isValidEntityIdString(parsed.key)) {
      malformedFrames += 1;
      return;
    }
    const key = parsed.key;

    if (parsed.type === "put") {
      const receiptMs = now();
      const entity = parseCopEntityState(
        parsed.payload,
        revision,
        key,
        receiptMs,
      );
      if (!entity) {
        malformedFrames += 1;
        return;
      }
      if (!synced) {
        const prevStaged = stagedSnapshot.get(key);
        if (prevStaged && revision > 0 && revision < prevStaged.kvRevision) {
          return;
        }
        if (entity.kind === "uxs-vehicle") {
          const priorHighWater =
            stagedMaxPoseByKey.get(key) ?? maxPoseByKey.get(key);
          const nextHighWater = applyVehiclePoseReceipt(
            priorHighWater,
            entity,
            receiptMs,
            false,
          );
          if (nextHighWater !== undefined) {
            stagedMaxPoseByKey.set(key, nextHighWater);
          }
        } else {
          stagedMaxPoseByKey.delete(key);
        }
        stagedSnapshot.set(key, entity);
        return;
      }
      const prevLive = entities.get(key);
      if (prevLive && revision > 0 && revision < prevLive.kvRevision) {
        return;
      }
      if (entity.kind === "uxs-vehicle") {
        const nextHighWater = applyVehiclePoseReceipt(
          maxPoseByKey.get(key),
          entity,
          receiptMs,
          true,
        );
        if (nextHighWater !== undefined) {
          maxPoseByKey.set(key, nextHighWater);
        }
      } else {
        maxPoseByKey.delete(key);
      }
      entities.set(key, entity);
      return;
    }

    if (parsed.type === "delete") {
      if (!synced) {
        const prevStaged = stagedSnapshot.get(key);
        if (prevStaged && revision > 0 && revision < prevStaged.kvRevision) {
          return;
        }
        stagedSnapshot.delete(key);
        stagedMaxPoseByKey.delete(key);
        return;
      }
      const prevLive = entities.get(key);
      if (prevLive && revision > 0 && revision < prevLive.kvRevision) {
        return;
      }
      entities.delete(key);
      maxPoseByKey.delete(key);
      return;
    }

    if (parsed.type === "poisoned") {
      const errorMsg =
        typeof parsed.error === "string" && parsed.error.length > 0
          ? parsed.error
          : "entity state decode error";
      const receiptMs = now();
      if (!synced) {
        const prevStaged = stagedSnapshot.get(key);
        if (prevStaged && revision > 0 && revision < prevStaged.kvRevision) {
          return;
        }
        const diagnosticPrior = prevStaged ?? entities.get(key);
        stagedSnapshot.set(
          key,
          buildPoisonedEntity(
            key,
            diagnosticPrior,
            revision,
            errorMsg,
            receiptMs,
          ),
        );
        return;
      }
      const priorLive = entities.get(key);
      if (priorLive && revision > 0 && revision < priorLive.kvRevision) {
        return;
      }
      entities.set(
        key,
        buildPoisonedEntity(key, priorLive, revision, errorMsg, receiptMs),
      );
      return;
    }

    malformedFrames += 1;
  }

  const wsClient = new ManagedWebSocketClient({
    url: options.url,
    envUrl,
    defaultPath: DEFAULT_GRAPH_WS_PATH,
    keepaliveIntervalMs: options.keepaliveIntervalMs,
    initialBackoffMs: options.initialBackoffMs,
    maxBackoffMs: options.maxBackoffMs,
    now,
    createSocket: options.createSocket,
    onStatusChange(nextStatus) {
      status = nextStatus;
    },
    onOpen() {
      // Begin staging pre-synced snapshot without clearing live `entities` (gapless reconnect).
      synced = false;
      stagedSnapshot.clear();
      stagedMaxPoseByKey.clear();
    },
    onMessage: ingestRawFrame,
    onDisconnect() {
      synced = false;
      stagedSnapshot.clear();
      stagedMaxPoseByKey.clear();
    },
    onEndpointChange(nextUrl) {
      currentUrl = nextUrl;
      clearFeedState();
    },
  });
  let currentUrl = $state(wsClient.url);

  return {
    get status() {
      return status;
    },
    get url() {
      return currentUrl;
    },
    get synced() {
      return synced;
    },
    get lastSyncedRevision() {
      return lastSyncedRevision;
    },
    get entities() {
      return entities;
    },
    get vehicles() {
      return vehicles;
    },
    get commandableVehicles() {
      return commandableVehicles;
    },
    get externalContacts() {
      return externalContacts;
    },
    get malformedFrameCount() {
      return malformedFrames;
    },
    connect(overrideUrl?: string) {
      wsClient.connect(overrideUrl);
    },
    disconnect() {
      wsClient.disconnect();
      synced = false;
      stagedSnapshot.clear();
      stagedMaxPoseByKey.clear();
    },
    getEntity(entityId: string | null | undefined) {
      if (!entityId) {
        return null;
      }
      return entities.get(entityId) ?? null;
    },
    getVehicle(entityId: string | null | undefined) {
      const entity = entityId ? entities.get(entityId) : undefined;
      return isVehicleEntity(entity) ? entity : null;
    },
    getCommandableVehicle(entityId: string | null | undefined) {
      const entity = entityId ? entities.get(entityId) : undefined;
      return isCommandableVehicle(entity) ? entity : null;
    },
    ingestRawFrame,
    reset() {
      wsClient.disconnect();
      clearFeedState();
    },
  };
}
