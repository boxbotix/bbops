// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Complete 360 Studio
// Copyright (c) 2026 boxbotix
//
// Provenance: Adapted from latest-wins WebSocket telemetry store pattern in
// C360Studio/semboids ui/src/lib/stores/flock.svelte.ts
// @ 37dbdb088438a2601c8556d012ab2c33bec85da2 (MIT).

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
  parseCanonicalSysIdSegment,
  parseFiniteTuple3,
  parseFiniteTuple4,
  parseHealthFlags,
} from "../uxs";
import {
  ManagedWebSocketClient,
  type WebSocketLike,
  type WsConnectionStatus,
} from "./ws-client";

export const DEFAULT_TELEMETRY_WS_PATH = "/ws/telemetry";
export const DEFAULT_EVENT_RING_CAPACITY = 100;

export const TRANSITION_EVENT_KINDS = [
  "heartbeat_edge",
  "failsafe_increment",
  "statustext",
  "link_lost",
  "link_restored",
] as const;
export type TransitionEventKind = (typeof TRANSITION_EVENT_KINDS)[number];

export interface VehicleKinematics {
  sysid: number;
  domain: UxsDomainKind;
  seq: number;
  msg_id: number;
  quat_wxyz: [number, number, number, number];
  has_attitude: boolean;
  body_rates: [number, number, number];
  ned_xyz: [number, number, number];
  has_local_pos: boolean;
  has_explicit_local_pos?: boolean;
  local_pos_confirmed_at_ms?: number | null;
  local_pos_observed_at_ms?: number | null;
  ned_vel: [number, number, number];
  lat_lon_alt: [number, number, number];
  has_global_pos: boolean;
  global_pos_observed_at_ms?: number | null;
  hdg_deg: number;
  has_heading: boolean;
  timestamp: number;
  receivedAtMs: number;
}

export interface StatusTextPayload {
  severity: number;
  text: string;
  id: number;
  chunk_seq: number;
}

export interface TransitionEvent {
  sysid: number;
  entity_id: string;
  domain: UxsDomainKind;
  seq: number;
  msg_id: number;
  kind: TransitionEventKind;
  reasons: readonly string[];
  armed: boolean;
  authority: ControlAuthority;
  prev_authority?: ControlAuthority;
  authority_incarnation?: string;
  authority_epoch?: number;
  observation_rev?: number;
  companion_mode: CompanionMode;
  link_tier: LinkTier;
  flags: HealthFlags;
  failsafes: number;
  status_text?: StatusTextPayload;
  timestamp: number;
}

export interface VehicleTelemetryLinkState {
  readonly sysid: number;
  readonly linkLost: boolean;
  readonly linkLostAtMs: number | null;
  readonly linkLostTimestamp: number | null;
  readonly lastPacketTimestamp: number | null;
  readonly lastPacketReceivedAtMs: number | null;
}

export interface TelemetryStoreOptions {
  url?: string;
  maxEvents?: number;
  keepaliveIntervalMs?: number;
  initialBackoffMs?: number;
  maxBackoffMs?: number;
  now?: () => number;
  createSocket?: (url: string) => WebSocketLike;
}

export interface TelemetryStore {
  readonly status: WsConnectionStatus;
  readonly url: string;
  readonly vehicleCount: number;
  readonly events: readonly TransitionEvent[];
  readonly reconnectCount: number;
  readonly lastDisconnectedAt: number | null;
  readonly droppedOutOfOrderCount: number;
  readonly malformedFrameCount: number;
  connect(overrideUrl?: string): void;
  disconnect(): void;
  getKinematics(sysid: number): VehicleKinematics | undefined;
  getLinkState(sysid: number): VehicleTelemetryLinkState | undefined;
  kinematicsEntries(): IterableIterator<[number, VehicleKinematics]>;
  onKinematicsAccepted(
    listener: (frame: VehicleKinematics) => void,
  ): () => void;
  onTransitionEvent?(listener: (event: TransitionEvent) => void): () => void;
  onFeedReset?(listener: () => void): () => void;
  ingestRawFrame(rawText: string): void;
  clearEvents(): void;
  reset(): void;
}

function isTransitionEventKind(value: unknown): value is TransitionEventKind {
  return (
    typeof value === "string" &&
    (TRANSITION_EVENT_KINDS as readonly string[]).includes(value)
  );
}

function parseStatusText(value: unknown): StatusTextPayload | null {
  if (!isRecord(value)) {
    return null;
  }
  if (
    !isNonNegativeInt(value.severity) ||
    value.severity > 255 ||
    typeof value.text !== "string" ||
    !isNonNegativeInt(value.id) ||
    value.id > 65535 ||
    !isNonNegativeInt(value.chunk_seq) ||
    value.chunk_seq > 255
  ) {
    return null;
  }
  return {
    severity: value.severity,
    text: value.text,
    id: value.id,
    chunk_seq: value.chunk_seq,
  };
}

function parseTelemetryPayload(
  payload: unknown,
  expectedSysId: number,
  receivedAtMs: number,
  priorLegacyAttitudeLatched: boolean,
  priorLegacyLocalPosConfirmedAtMs: number | null,
  priorLocalPosObservedAtMs: number | null,
  priorGlobalPosObservedAtMs: number | null,
): VehicleKinematics | null {
  if (!isRecord(payload)) {
    return null;
  }
  if (
    payload.sysid !== expectedSysId ||
    !isUxsDomainKind(payload.domain) ||
    !isNonNegativeInt(payload.seq) ||
    payload.seq > 255 ||
    !isNonNegativeInt(payload.msg_id) ||
    (payload.has_attitude !== undefined &&
      typeof payload.has_attitude !== "boolean") ||
    (payload.has_local_pos !== undefined &&
      typeof payload.has_local_pos !== "boolean") ||
    (payload.has_heading !== undefined &&
      typeof payload.has_heading !== "boolean") ||
    typeof payload.has_global_pos !== "boolean" ||
    !isValidHeadingDeg(payload.hdg_deg) ||
    !isFiniteNumber(payload.timestamp)
  ) {
    return null;
  }
  const quat = parseFiniteTuple4(payload.quat_wxyz);
  const rates = parseFiniteTuple3(payload.body_rates);
  const ned = parseFiniteTuple3(payload.ned_xyz);
  const vel = parseFiniteTuple3(payload.ned_vel);
  const lla = parseFiniteTuple3(payload.lat_lon_alt);
  if (!quat || !rates || !ned || !vel || !lla) {
    return null;
  }
  if (!isValidLatLon(lla[0], lla[1])) {
    return null;
  }
  const quatNormSq =
    quat[0] * quat[0] +
    quat[1] * quat[1] +
    quat[2] * quat[2] +
    quat[3] * quat[3];
  const wrappedHdg = ((payload.hdg_deg % 360) + 360) % 360;
  const hdgDistFromNorth = Math.min(wrappedHdg, 360 - wrappedHdg);
  const hasNonZeroRates = rates[0] !== 0 || rates[1] !== 0 || rates[2] !== 0;
  const isLegacyUnmeasuredBirthQuat =
    payload.msg_id !== 31 &&
    Math.abs(quat[0] - 1) < 1e-9 &&
    Math.abs(quat[1]) < 1e-9 &&
    Math.abs(quat[2]) < 1e-9 &&
    Math.abs(quat[3]) < 1e-9 &&
    (hdgDistFromNorth > 0.5 ||
      (!hasNonZeroRates && !priorLegacyAttitudeLatched));
  const hasAttitude =
    typeof payload.has_attitude === "boolean"
      ? payload.has_attitude && quatNormSq >= 1e-6
      : quatNormSq >= 1e-6 && !isLegacyUnmeasuredBirthQuat;
  const hasExplicitLocalPos = typeof payload.has_local_pos === "boolean";
  const hasLocalPos = hasExplicitLocalPos
    ? (payload.has_local_pos as boolean)
    : payload.msg_id === 32 || priorLegacyLocalPosConfirmedAtMs !== null;
  const localPosConfirmedAtMs = hasExplicitLocalPos
    ? payload.has_local_pos
      ? payload.timestamp
      : null
    : payload.msg_id === 32
      ? payload.timestamp
      : priorLegacyLocalPosConfirmedAtMs;
  const localPosObservedAtMs = !hasLocalPos
    ? null
    : payload.msg_id === 32
      ? payload.timestamp
      : priorLocalPosObservedAtMs;
  const globalPosObservedAtMs = !payload.has_global_pos
    ? null
    : payload.msg_id === 33
      ? payload.timestamp
      : priorGlobalPosObservedAtMs;
  const hasHeading =
    typeof payload.has_heading === "boolean"
      ? payload.has_heading || hasAttitude
      : hasAttitude || payload.hdg_deg !== 0;
  return {
    sysid: expectedSysId,
    domain: payload.domain,
    seq: payload.seq,
    msg_id: payload.msg_id,
    quat_wxyz: quat,
    has_attitude: hasAttitude,
    body_rates: rates,
    ned_xyz: ned,
    has_local_pos: hasLocalPos,
    has_explicit_local_pos: hasExplicitLocalPos,
    local_pos_confirmed_at_ms: localPosConfirmedAtMs,
    local_pos_observed_at_ms: localPosObservedAtMs,
    ned_vel: vel,
    lat_lon_alt: lla,
    has_global_pos: payload.has_global_pos,
    global_pos_observed_at_ms: globalPosObservedAtMs,
    hdg_deg: payload.hdg_deg,
    has_heading: hasHeading,
    timestamp: payload.timestamp,
    receivedAtMs,
  };
}

function overwriteKinematicsInPlace(
  target: VehicleKinematics,
  next: VehicleKinematics,
): void {
  target.domain = next.domain;
  target.seq = next.seq;
  target.msg_id = next.msg_id;
  target.quat_wxyz[0] = next.quat_wxyz[0];
  target.quat_wxyz[1] = next.quat_wxyz[1];
  target.quat_wxyz[2] = next.quat_wxyz[2];
  target.quat_wxyz[3] = next.quat_wxyz[3];
  target.has_attitude = next.has_attitude;
  target.body_rates[0] = next.body_rates[0];
  target.body_rates[1] = next.body_rates[1];
  target.body_rates[2] = next.body_rates[2];
  target.ned_xyz[0] = next.ned_xyz[0];
  target.ned_xyz[1] = next.ned_xyz[1];
  target.ned_xyz[2] = next.ned_xyz[2];
  target.has_local_pos = next.has_local_pos;
  target.has_explicit_local_pos = next.has_explicit_local_pos;
  target.local_pos_confirmed_at_ms = next.local_pos_confirmed_at_ms;
  target.local_pos_observed_at_ms = next.local_pos_observed_at_ms;
  target.ned_vel[0] = next.ned_vel[0];
  target.ned_vel[1] = next.ned_vel[1];
  target.ned_vel[2] = next.ned_vel[2];
  target.lat_lon_alt[0] = next.lat_lon_alt[0];
  target.lat_lon_alt[1] = next.lat_lon_alt[1];
  target.lat_lon_alt[2] = next.lat_lon_alt[2];
  target.has_global_pos = next.has_global_pos;
  target.global_pos_observed_at_ms = next.global_pos_observed_at_ms;
  target.hdg_deg = next.hdg_deg;
  target.has_heading = next.has_heading;
  target.timestamp = next.timestamp;
  target.receivedAtMs = next.receivedAtMs;
}

function parseTransitionEventPayload(
  payload: unknown,
  expectedSysId: number,
): TransitionEvent | null {
  if (!isRecord(payload)) {
    return null;
  }
  if (
    payload.sysid !== expectedSysId ||
    !isValidEntityIdString(payload.entity_id) ||
    !isUxsDomainKind(payload.domain) ||
    !isNonNegativeInt(payload.seq) ||
    payload.seq > 255 ||
    !isNonNegativeInt(payload.msg_id) ||
    !isTransitionEventKind(payload.kind) ||
    typeof payload.armed !== "boolean" ||
    !isControlAuthority(payload.authority) ||
    !isCompanionMode(payload.companion_mode) ||
    !isLinkTier(payload.link_tier) ||
    !isNonNegativeInt(payload.failsafes) ||
    !isFiniteNumber(payload.timestamp)
  ) {
    return null;
  }
  const flags = parseHealthFlags(payload.flags);
  if (!flags) {
    return null;
  }

  let reasons: string[] = [];
  if (payload.reasons !== undefined && payload.reasons !== null) {
    if (
      !Array.isArray(payload.reasons) ||
      !payload.reasons.every((item) => typeof item === "string")
    ) {
      return null;
    }
    reasons = [...payload.reasons];
  }

  let prevAuthority: ControlAuthority | undefined;
  if (
    payload.prev_authority !== undefined &&
    payload.prev_authority !== null &&
    payload.prev_authority !== ""
  ) {
    if (!isControlAuthority(payload.prev_authority)) {
      return null;
    }
    prevAuthority = payload.prev_authority;
  }

  let authorityIncarnation: string | undefined;
  if (
    payload.authority_incarnation !== undefined &&
    payload.authority_incarnation !== null &&
    payload.authority_incarnation !== ""
  ) {
    if (typeof payload.authority_incarnation !== "string") {
      return null;
    }
    authorityIncarnation = payload.authority_incarnation;
  }

  let authorityEpoch: number | undefined;
  if (
    payload.authority_epoch !== undefined &&
    payload.authority_epoch !== null
  ) {
    if (!isNonNegativeInt(payload.authority_epoch)) {
      return null;
    }
    if (payload.authority_epoch > 0) {
      authorityEpoch = payload.authority_epoch;
    }
  }

  let observationRev: number | undefined;
  if (
    payload.observation_rev !== undefined &&
    payload.observation_rev !== null
  ) {
    if (!isNonNegativeInt(payload.observation_rev)) {
      return null;
    }
    if (payload.observation_rev > 0) {
      observationRev = payload.observation_rev;
    }
  }

  let statusText: StatusTextPayload | undefined;
  if (payload.status_text !== undefined && payload.status_text !== null) {
    const parsedStatus = parseStatusText(payload.status_text);
    if (!parsedStatus) {
      return null;
    }
    statusText = parsedStatus;
  }

  const event: TransitionEvent = {
    sysid: expectedSysId,
    entity_id: payload.entity_id,
    domain: payload.domain,
    seq: payload.seq,
    msg_id: payload.msg_id,
    kind: payload.kind,
    reasons,
    armed: payload.armed,
    authority: payload.authority,
    companion_mode: payload.companion_mode,
    link_tier: payload.link_tier,
    flags,
    failsafes: payload.failsafes,
    timestamp: payload.timestamp,
  };
  if (prevAuthority !== undefined) {
    event.prev_authority = prevAuthority;
  }
  if (authorityIncarnation !== undefined) {
    event.authority_incarnation = authorityIncarnation;
  }
  if (authorityEpoch !== undefined) {
    event.authority_epoch = authorityEpoch;
  }
  if (observationRev !== undefined) {
    event.observation_rev = observationRev;
  }
  if (statusText !== undefined) {
    event.status_text = statusText;
  }
  return event;
}

export function createTelemetryStore(
  options: TelemetryStoreOptions = {},
): TelemetryStore {
  const maxEvents = options.maxEvents ?? DEFAULT_EVENT_RING_CAPACITY;
  if (!Number.isInteger(maxEvents) || maxEvents < 1) {
    throw new RangeError(`maxEvents (${maxEvents}) must be an integer >= 1`);
  }
  const now = options.now ?? (() => Date.now());
  const envUrl =
    typeof import.meta !== "undefined" && import.meta.env
      ? (import.meta.env.VITE_TELEMETRY_WS_URL as string | undefined)
      : undefined;

  interface MutableTelemetryLinkState {
    sysid: number;
    linkLost: boolean;
    linkLostAtMs: number | null;
    linkLostTimestamp: number | null;
    lastPacketTimestamp: number | null;
    lastPacketReceivedAtMs: number | null;
  }

  // D1 Option A: non-reactive Map mutated in place so 20 Hz frames never trigger DOM $state thrash.
  // eslint-disable-next-line svelte/prefer-svelte-reactivity
  const kinematicsBySysId = new Map<number, VehicleKinematics>();
  // eslint-disable-next-line svelte/prefer-svelte-reactivity
  const linkStateBySysId = new Map<number, MutableTelemetryLinkState>();
  // eslint-disable-next-line svelte/prefer-svelte-reactivity
  const lastLinkTransitionTsBySysId = new Map<number, number>();
  // eslint-disable-next-line svelte/prefer-svelte-reactivity
  const legacyAttitudeLatchedBySysId = new Set<number>();
  // eslint-disable-next-line svelte/prefer-svelte-reactivity
  const legacyLocalPosLatchedAtBySysId = new Map<number, number>();
  // eslint-disable-next-line svelte/prefer-svelte-reactivity
  const localPosObservedAtBySysId = new Map<number, number>();
  // eslint-disable-next-line svelte/prefer-svelte-reactivity
  const globalPosObservedAtBySysId = new Map<number, number>();
  // eslint-disable-next-line svelte/prefer-svelte-reactivity
  const kinematicsAcceptedListeners = new Set<
    (frame: VehicleKinematics) => void
  >();
  // eslint-disable-next-line svelte/prefer-svelte-reactivity
  const transitionEventListeners = new Set<(event: TransitionEvent) => void>();
  // eslint-disable-next-line svelte/prefer-svelte-reactivity
  const feedResetListeners = new Set<() => void>();
  const eventRing = new Array<TransitionEvent | undefined>(maxEvents);
  let ringWriteIdx = 0;
  let ringSize = 0;
  let droppedOutOfOrder = 0;
  let malformedFrames = 0;
  let hasEstablishedOnce = false;

  let status = $state<WsConnectionStatus>("disconnected");
  let vehicleCount = $state(0);
  let linkStateVersion = $state(0);
  let events = $state.raw<readonly TransitionEvent[]>([]);
  let reconnectCount = $state(0);
  let lastDisconnectedAt = $state<number | null>(null);

  function latestLinkWatermark(sysid: number): number | null {
    let maxTs: number | null = null;
    const kin = kinematicsBySysId.get(sysid);
    if (kin !== undefined) {
      maxTs = kin.timestamp;
    }
    const entry = linkStateBySysId.get(sysid);
    if (entry !== undefined) {
      if (
        entry.lastPacketTimestamp !== null &&
        (maxTs === null || entry.lastPacketTimestamp > maxTs)
      ) {
        maxTs = entry.lastPacketTimestamp;
      }
      if (
        entry.linkLostTimestamp !== null &&
        (maxTs === null || entry.linkLostTimestamp > maxTs)
      ) {
        maxTs = entry.linkLostTimestamp;
      }
    }
    const lastTransitionTs = lastLinkTransitionTsBySysId.get(sysid);
    if (
      lastTransitionTs !== undefined &&
      (maxTs === null || lastTransitionTs > maxTs)
    ) {
      maxTs = lastTransitionTs;
    }
    return maxTs;
  }

  function recordPacketLinkReceipt(
    sysid: number,
    packetTimestamp: number,
    receivedAtMs: number,
  ): void {
    const entry = linkStateBySysId.get(sysid);
    if (entry === undefined) {
      linkStateBySysId.set(sysid, {
        sysid,
        linkLost: false,
        linkLostAtMs: null,
        linkLostTimestamp: null,
        lastPacketTimestamp: packetTimestamp,
        lastPacketReceivedAtMs: receivedAtMs,
      });
      linkStateVersion += 1;
      return;
    }
    const predatesActiveLoss =
      entry.linkLost &&
      entry.linkLostTimestamp !== null &&
      packetTimestamp < entry.linkLostTimestamp;
    if (
      !predatesActiveLoss &&
      (entry.lastPacketTimestamp === null ||
        packetTimestamp >= entry.lastPacketTimestamp)
    ) {
      entry.lastPacketTimestamp = packetTimestamp;
      entry.lastPacketReceivedAtMs = receivedAtMs;
    }
    if (
      entry.linkLost &&
      (entry.linkLostTimestamp === null ||
        packetTimestamp >= entry.linkLostTimestamp)
    ) {
      entry.linkLost = false;
      entry.linkLostAtMs = null;
      entry.linkLostTimestamp = null;
      linkStateVersion += 1;
    }
  }

  function recordLinkLostTransition(
    sysid: number,
    lossTimestamp: number,
    receivedAtMs: number,
  ): void {
    const entry = linkStateBySysId.get(sysid);
    if (
      entry !== undefined &&
      entry.lastPacketTimestamp !== null &&
      lossTimestamp < entry.lastPacketTimestamp
    ) {
      return;
    }
    if (
      entry !== undefined &&
      entry.linkLostTimestamp !== null &&
      lossTimestamp < entry.linkLostTimestamp
    ) {
      return;
    }
    if (entry === undefined) {
      linkStateBySysId.set(sysid, {
        sysid,
        linkLost: true,
        linkLostAtMs: receivedAtMs,
        linkLostTimestamp: lossTimestamp,
        lastPacketTimestamp: null,
        lastPacketReceivedAtMs: null,
      });
      linkStateVersion += 1;
      return;
    }
    const wasLost = entry.linkLost;
    entry.linkLost = true;
    entry.linkLostAtMs = receivedAtMs;
    entry.linkLostTimestamp = lossTimestamp;
    if (!wasLost) {
      linkStateVersion += 1;
    }
  }

  function clearAllSessionLatches(): void {
    legacyAttitudeLatchedBySysId.clear();
    legacyLocalPosLatchedAtBySysId.clear();
    localPosObservedAtBySysId.clear();
    globalPosObservedAtBySysId.clear();
  }

  function clearSysIdSessionLatches(sysid: number): void {
    legacyAttitudeLatchedBySysId.delete(sysid);
    legacyLocalPosLatchedAtBySysId.delete(sysid);
    localPosObservedAtBySysId.delete(sysid);
    globalPosObservedAtBySysId.delete(sysid);
  }

  function snapshotEventRing(): readonly TransitionEvent[] {
    if (ringSize === 0) {
      return [];
    }
    const ordered = new Array<TransitionEvent>(ringSize);
    const startIdx = ringSize < maxEvents ? 0 : ringWriteIdx;
    for (let i = 0; i < ringSize; i += 1) {
      ordered[i] = eventRing[(startIdx + i) % maxEvents] as TransitionEvent;
    }
    return ordered;
  }

  function pushTransitionEvent(ev: TransitionEvent): void {
    eventRing[ringWriteIdx] = ev;
    ringWriteIdx = (ringWriteIdx + 1) % maxEvents;
    if (ringSize < maxEvents) {
      ringSize += 1;
    }
    events = snapshotEventRing();
  }

  function ingestRawFrame(rawText: string): void {
    let parsed: unknown;
    try {
      parsed = JSON.parse(rawText);
    } catch {
      malformedFrames += 1;
      return;
    }
    if (
      !isRecord(parsed) ||
      parsed.type !== "data" ||
      typeof parsed.subject !== "string"
    ) {
      malformedFrames += 1;
      return;
    }

    // Stamp local arrival time from the browser clock so WS1 staleness timeout
    // never depends on server and browser clocks agreeing; producer timestamps
    // on payload.timestamp remain authoritative for cross-lane ordering.
    const receivedAtMs = now();
    const segments = parsed.subject.split(".");
    if (segments[0] !== "bbops" || segments[1] !== "telemetry") {
      malformedFrames += 1;
      return;
    }

    if (segments.length === 3) {
      const sysid = parseCanonicalSysIdSegment(segments[2]);
      if (sysid === null) {
        malformedFrames += 1;
        return;
      }
      const frame = parseTelemetryPayload(
        parsed.payload,
        sysid,
        receivedAtMs,
        legacyAttitudeLatchedBySysId.has(sysid),
        legacyLocalPosLatchedAtBySysId.get(sysid) ?? null,
        localPosObservedAtBySysId.get(sysid) ?? null,
        globalPosObservedAtBySysId.get(sysid) ?? null,
      );
      if (!frame) {
        malformedFrames += 1;
        return;
      }
      const existing = kinematicsBySysId.get(sysid);
      if (existing !== undefined && frame.timestamp < existing.timestamp) {
        droppedOutOfOrder += 1;
        return;
      }
      recordPacketLinkReceipt(sysid, frame.timestamp, receivedAtMs);
      if (frame.has_attitude) {
        legacyAttitudeLatchedBySysId.add(sysid);
      } else {
        legacyAttitudeLatchedBySysId.delete(sysid);
      }
      if (
        frame.has_local_pos &&
        frame.local_pos_confirmed_at_ms !== null &&
        frame.local_pos_confirmed_at_ms !== undefined
      ) {
        legacyLocalPosLatchedAtBySysId.set(
          sysid,
          frame.local_pos_confirmed_at_ms,
        );
      } else {
        legacyLocalPosLatchedAtBySysId.delete(sysid);
      }
      if (
        frame.has_local_pos &&
        frame.local_pos_observed_at_ms !== null &&
        frame.local_pos_observed_at_ms !== undefined
      ) {
        localPosObservedAtBySysId.set(sysid, frame.local_pos_observed_at_ms);
      } else {
        localPosObservedAtBySysId.delete(sysid);
      }
      if (
        frame.has_global_pos &&
        frame.global_pos_observed_at_ms !== null &&
        frame.global_pos_observed_at_ms !== undefined
      ) {
        globalPosObservedAtBySysId.set(sysid, frame.global_pos_observed_at_ms);
      } else {
        globalPosObservedAtBySysId.delete(sysid);
      }
      if (existing !== undefined) {
        overwriteKinematicsInPlace(existing, frame);
        for (const listener of kinematicsAcceptedListeners) {
          listener(existing);
        }
        return;
      }
      kinematicsBySysId.set(sysid, frame);
      vehicleCount = kinematicsBySysId.size;
      for (const listener of kinematicsAcceptedListeners) {
        listener(frame);
      }
      return;
    }

    if (segments.length === 4 && segments[2] === "events") {
      const sysid = parseCanonicalSysIdSegment(segments[3]);
      if (sysid === null) {
        malformedFrames += 1;
        return;
      }
      const transition = parseTransitionEventPayload(parsed.payload, sysid);
      if (!transition) {
        malformedFrames += 1;
        return;
      }
      const watermark = latestLinkWatermark(sysid);
      const notOlderThanLinkState =
        watermark === null || transition.timestamp >= watermark;
      if (
        (transition.kind === "link_lost" ||
          transition.kind === "link_restored") &&
        notOlderThanLinkState
      ) {
        lastLinkTransitionTsBySysId.set(sysid, transition.timestamp);
        clearSysIdSessionLatches(sysid);
      }
      if (transition.kind === "link_lost") {
        if (notOlderThanLinkState) {
          recordLinkLostTransition(sysid, transition.timestamp, receivedAtMs);
        }
      } else if (transition.kind === "link_restored") {
        if (notOlderThanLinkState) {
          recordPacketLinkReceipt(sysid, transition.timestamp, receivedAtMs);
        }
      } else {
        recordPacketLinkReceipt(sysid, transition.timestamp, receivedAtMs);
      }
      pushTransitionEvent(transition);
      for (const listener of transitionEventListeners) {
        listener(transition);
      }
      return;
    }

    malformedFrames += 1;
  }

  function clearFeedState(): void {
    const hadLinkState = linkStateBySysId.size > 0;
    kinematicsBySysId.clear();
    linkStateBySysId.clear();
    lastLinkTransitionTsBySysId.clear();
    clearAllSessionLatches();
    eventRing.fill(undefined);
    ringWriteIdx = 0;
    ringSize = 0;
    droppedOutOfOrder = 0;
    malformedFrames = 0;
    hasEstablishedOnce = false;
    vehicleCount = 0;
    if (hadLinkState) {
      linkStateVersion += 1;
    }
    events = [];
    reconnectCount = 0;
    lastDisconnectedAt = null;
    for (const listener of feedResetListeners) {
      listener();
    }
  }

  const wsClient = new ManagedWebSocketClient({
    url: options.url,
    envUrl,
    defaultPath: DEFAULT_TELEMETRY_WS_PATH,
    keepaliveIntervalMs: options.keepaliveIntervalMs,
    initialBackoffMs: options.initialBackoffMs,
    maxBackoffMs: options.maxBackoffMs,
    now,
    createSocket: options.createSocket,
    onStatusChange(nextStatus) {
      status = nextStatus;
    },
    onOpen() {
      clearAllSessionLatches();
      if (hasEstablishedOnce) {
        reconnectCount += 1;
      }
      hasEstablishedOnce = true;
    },
    onMessage: ingestRawFrame,
    onDisconnect(info) {
      clearAllSessionLatches();
      if (info.wasConnected) {
        lastDisconnectedAt = info.timestamp;
      }
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
    get vehicleCount() {
      return vehicleCount;
    },
    get events() {
      return events;
    },
    get reconnectCount() {
      return reconnectCount;
    },
    get lastDisconnectedAt() {
      return lastDisconnectedAt;
    },
    get droppedOutOfOrderCount() {
      return droppedOutOfOrder;
    },
    get malformedFrameCount() {
      return malformedFrames;
    },
    connect(overrideUrl?: string) {
      wsClient.connect(overrideUrl);
    },
    disconnect() {
      wsClient.disconnect();
      clearAllSessionLatches();
    },
    getKinematics(sysid: number) {
      return kinematicsBySysId.get(sysid);
    },
    getLinkState(sysid: number) {
      void linkStateVersion;
      return linkStateBySysId.get(sysid);
    },
    kinematicsEntries() {
      return kinematicsBySysId.entries();
    },
    onKinematicsAccepted(listener: (frame: VehicleKinematics) => void) {
      kinematicsAcceptedListeners.add(listener);
      return () => {
        kinematicsAcceptedListeners.delete(listener);
      };
    },
    onTransitionEvent(listener: (event: TransitionEvent) => void) {
      transitionEventListeners.add(listener);
      return () => {
        transitionEventListeners.delete(listener);
      };
    },
    onFeedReset(listener: () => void) {
      feedResetListeners.add(listener);
      return () => {
        feedResetListeners.delete(listener);
      };
    },
    ingestRawFrame,
    clearEvents() {
      eventRing.fill(undefined);
      ringWriteIdx = 0;
      ringSize = 0;
      events = [];
    },
    reset() {
      wsClient.disconnect();
      clearFeedState();
    },
  };
}
