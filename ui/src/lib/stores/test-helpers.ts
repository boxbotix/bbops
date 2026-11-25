// SPDX-License-Identifier: MIT
// Copyright (c) 2026 boxbotix

import type { SkgTriple } from "./skg.svelte";
import {
  WS_CLOSED,
  WS_CONNECTING,
  WS_OPEN,
  type WebSocketLike,
} from "./ws-client";

export class MockWebSocket implements WebSocketLike {
  readonly url: string;
  readyState: number = WS_CONNECTING;
  onopen: ((event: never) => unknown) | null = null;
  onmessage: ((event: MessageEvent<unknown>) => unknown) | null = null;
  onerror: ((event: never) => unknown) | null = null;
  onclose: ((event: CloseEvent) => unknown) | null = null;
  readonly sentFrames: string[] = [];
  closeCalls: Array<{ code?: number; reason?: string }> = [];

  constructor(url: string) {
    this.url = url;
  }

  send(data: string): void {
    if (this.readyState !== WS_OPEN) {
      throw new Error("MockWebSocket: send called while socket is not OPEN");
    }
    this.sentFrames.push(data);
  }

  close(code = 1000, reason = ""): void {
    this.closeCalls.push({ code, reason });
    this.readyState = WS_CLOSED;
    const handler = this.onclose;
    if (handler) {
      handler({ code, reason } as CloseEvent);
    }
  }

  serverOpen(): void {
    this.readyState = WS_OPEN;
    const handler = this.onopen;
    if (handler) {
      handler(undefined as never);
    }
  }

  serverMessage(rawText: string): void {
    const handler = this.onmessage;
    if (handler) {
      handler({ data: rawText } as MessageEvent<unknown>);
    }
  }

  serverDrop(code = 1006, reason = "abnormal closure"): void {
    this.readyState = WS_CLOSED;
    const errHandler = this.onerror;
    if (errHandler) {
      errHandler(undefined as never);
    }
    const closeHandler = this.onclose;
    if (closeHandler) {
      closeHandler({ code, reason } as CloseEvent);
    }
  }
}

export class MockWebSocketRig {
  readonly sockets: MockWebSocket[] = [];

  readonly createSocket = (url: string): WebSocketLike => {
    const sock = new MockWebSocket(url);
    this.sockets.push(sock);
    return sock;
  };

  get latest(): MockWebSocket {
    const last = this.sockets[this.sockets.length - 1];
    if (!last) {
      throw new Error("MockWebSocketRig: no socket created yet");
    }
    return last;
  }
}

export function createSeededRng(seed: number): () => number {
  let state = seed >>> 0;
  return () => {
    state = (state + 0x6d2b79f5) >>> 0;
    let t = Math.imul(state ^ (state >>> 15), 1 | state);
    t = (t + Math.imul(t ^ (t >>> 7), 61 | t)) ^ t;
    return ((t ^ (t >>> 14)) >>> 0) / 4294967296;
  };
}

export function makeTriple(
  subject: string,
  predicate: string,
  object: unknown,
  opts: {
    source?: string;
    timestamp?: string;
    expires_at?: string;
    confidence?: number;
    context?: string;
  } = {},
): SkgTriple {
  const triple: SkgTriple = {
    subject,
    predicate,
    object,
    source: opts.source ?? "mavlink_0xb1",
    timestamp: opts.timestamp ?? "2026-11-24T11:00:00.250Z",
    confidence: opts.confidence ?? 1.0,
  };
  if (opts.context) {
    triple.context = opts.context;
  }
  if (opts.expires_at) {
    triple.expires_at = opts.expires_at;
  }
  return triple;
}

const SYS_STATUS_TIMING_PREDICATES = new Set([
  "uxs.health.loopdtus",
  "uxs.health.maxloopdtus",
  "uxs.health.rttus",
]);

export function buildVehicleStoredPayload(
  sysid: number,
  overrides: Partial<Record<string, unknown>> = {},
  opts: {
    org?: string;
    platform?: string;
    observedAt?: string;
    updatedAt?: string;
    hasSysStatus?: boolean;
  } = {},
): { id: string; triples: SkgTriple[]; updated_at: string } {
  const org = opts.org ?? "boxbotix";
  const platform = opts.platform ?? "local";
  const id = `${org}.${platform}.robotics.uxs.vehicle.${sysid}`;
  const observedAt = opts.observedAt ?? "2026-11-24T11:00:00.250Z";
  const defaults: Record<string, unknown> = {
    "uxs.domain.kind": "quadcopter",
    "uxs.authority.state": "companion-teleop",
    "uxs.mode.companion": "teleop-velocity",
    "uxs.link.tier": "mesh-high-rate",
    "uxs.health.armed": true,
    "uxs.health.flags": {
      clock_sync_locked: true,
      teleop_deadman_clamped: false,
      crsf_link_healthy: true,
      companion_trunk_healthy: true,
    },
    "uxs.health.failsafes": 0,
    "uxs.health.crcerrors": 1,
    "uxs.health.loopdtus": opts.hasSysStatus === false ? 0 : 2500,
    "uxs.health.maxloopdtus": opts.hasSysStatus === false ? 0 : 3100,
    "uxs.health.rttus": opts.hasSysStatus === false ? 0 : 420,
    "uxs.attitude.quaternion": [1, 0, 0, 0],
    "uxs.attitude.rates": [0.01, -0.02, 0.03],
    "uxs.kinematics.ned": [12.5, -4.0, -25.0],
    "uxs.kinematics.vx": 2.0,
    "uxs.kinematics.vy": -0.5,
    "uxs.kinematics.vz": 0.0,
    "uxs.kinematics.headingdeg": 135.5,
    "geo.location.latitude": 37.7749,
    "geo.location.longitude": -122.4194,
    "geo.location.altitude": 48.2,
    "dc.terms.title": `UXS-${sysid}`,
    "entity.indexing.profile": "signal",
    ...overrides,
  };

  const triples: SkgTriple[] = Object.entries(defaults).map(([pred, val]) => {
    const isTimingPred = SYS_STATUS_TIMING_PREDICATES.has(pred);
    const confidence = isTimingPred && opts.hasSysStatus === false ? 0.0 : 1.0;
    const context =
      isTimingPred && opts.hasSysStatus === true ? "measured" : undefined;
    return makeTriple(id, pred, val, {
      source:
        pred === "entity.indexing.profile" ? "graph-ingest" : "mavlink_0xb1",
      timestamp: observedAt,
      confidence,
      context,
    });
  });

  return {
    id,
    triples,
    updated_at: opts.updatedAt ?? "2026-11-24T11:00:01.000Z",
  };
}
