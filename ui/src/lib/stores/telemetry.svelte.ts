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
  body_rates: [number, number, number];
  ned_xyz: [number, number, number];
  ned_vel: [number, number, number];
  lat_lon_alt: [number, number, number];
  has_global_pos: boolean;
  hdg_deg: number;
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
  companion_mode: CompanionMode;
  link_tier: LinkTier;
  flags: HealthFlags;
  failsafes: number;
  status_text?: StatusTextPayload;
  timestamp: number;
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
  kinematicsEntries(): IterableIterator<[number, VehicleKinematics]>;
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
  return {
    sysid: expectedSysId,
    domain: payload.domain,
    seq: payload.seq,
    msg_id: payload.msg_id,
    quat_wxyz: quat,
    body_rates: rates,
    ned_xyz: ned,
    ned_vel: vel,
    lat_lon_alt: lla,
    has_global_pos: payload.has_global_pos,
    hdg_deg: payload.hdg_deg,
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
  target.body_rates[0] = next.body_rates[0];
  target.body_rates[1] = next.body_rates[1];
  target.body_rates[2] = next.body_rates[2];
  target.ned_xyz[0] = next.ned_xyz[0];
  target.ned_xyz[1] = next.ned_xyz[1];
  target.ned_xyz[2] = next.ned_xyz[2];
  target.ned_vel[0] = next.ned_vel[0];
  target.ned_vel[1] = next.ned_vel[1];
  target.ned_vel[2] = next.ned_vel[2];
  target.lat_lon_alt[0] = next.lat_lon_alt[0];
  target.lat_lon_alt[1] = next.lat_lon_alt[1];
  target.lat_lon_alt[2] = next.lat_lon_alt[2];
  target.has_global_pos = next.has_global_pos;
  target.hdg_deg = next.hdg_deg;
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

  // D1 Option A: non-reactive Map mutated in place so 20 Hz frames never trigger DOM $state thrash.
  // eslint-disable-next-line svelte/prefer-svelte-reactivity
  const kinematicsBySysId = new Map<number, VehicleKinematics>();
  const eventRing = new Array<TransitionEvent | undefined>(maxEvents);
  let ringWriteIdx = 0;
  let ringSize = 0;
  let droppedOutOfOrder = 0;
  let malformedFrames = 0;
  let hasEstablishedOnce = false;

  let status = $state<WsConnectionStatus>("disconnected");
  let vehicleCount = $state(0);
  let events = $state.raw<readonly TransitionEvent[]>([]);
  let reconnectCount = $state(0);
  let lastDisconnectedAt = $state<number | null>(null);

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

    const receivedAtMs = isFiniteNumber(parsed.timestamp)
      ? parsed.timestamp
      : now();
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
      const frame = parseTelemetryPayload(parsed.payload, sysid, receivedAtMs);
      if (!frame) {
        malformedFrames += 1;
        return;
      }
      const existing = kinematicsBySysId.get(sysid);
      if (existing !== undefined) {
        if (frame.timestamp < existing.timestamp) {
          droppedOutOfOrder += 1;
          return;
        }
        overwriteKinematicsInPlace(existing, frame);
        return;
      }
      kinematicsBySysId.set(sysid, frame);
      vehicleCount = kinematicsBySysId.size;
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
      pushTransitionEvent(transition);
      return;
    }

    malformedFrames += 1;
  }

  function clearFeedState(): void {
    kinematicsBySysId.clear();
    eventRing.fill(undefined);
    ringWriteIdx = 0;
    ringSize = 0;
    droppedOutOfOrder = 0;
    malformedFrames = 0;
    hasEstablishedOnce = false;
    vehicleCount = 0;
    events = [];
    reconnectCount = 0;
    lastDisconnectedAt = null;
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
      if (hasEstablishedOnce) {
        reconnectCount += 1;
      }
      hasEstablishedOnce = true;
    },
    onMessage: ingestRawFrame,
    onDisconnect(info) {
      if (info.wasConnected) {
        lastDisconnectedAt = info.timestamp;
      }
    },
    onEndpointChange() {
      clearFeedState();
    },
  });

  return {
    get status() {
      return status;
    },
    get url() {
      return wsClient.url;
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
    },
    getKinematics(sysid: number) {
      return kinematicsBySysId.get(sysid);
    },
    kinematicsEntries() {
      return kinematicsBySysId.entries();
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
