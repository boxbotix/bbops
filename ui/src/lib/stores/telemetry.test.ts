// SPDX-License-Identifier: MIT
// Copyright (c) 2026 boxbotix

import { flushSync } from "svelte";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { createReactiveProbe } from "./rune-probe.svelte";
import { createSeededRng, MockWebSocketRig } from "./test-helpers";
import {
  createTelemetryStore,
  type TransitionEvent,
  type VehicleKinematics,
} from "./telemetry.svelte";

function buildKinematicEnvelope(
  sysid: number,
  overrides: Record<string, unknown> = {},
  envelopeTimestamp = 1_732_400_000_000,
): string {
  return JSON.stringify({
    type: "data",
    subject: `bbops.telemetry.${sysid}`,
    timestamp: envelopeTimestamp,
    payload: {
      sysid,
      domain: "quadcopter",
      seq: 1,
      msg_id: 31,
      quat_wxyz: [1, 0, 0, 0],
      body_rates: [0.01, -0.02, 0.03],
      ned_xyz: [10, 20, -15],
      ned_vel: [1.5, -0.5, 0.1],
      lat_lon_alt: [37.7749, -122.4194, 42.5],
      has_global_pos: true,
      hdg_deg: 90,
      timestamp: 1_732_400_000_000,
      ...overrides,
    },
  });
}

function buildTransitionEnvelope(
  sysid: number,
  overrides: Record<string, unknown> = {},
): string {
  return JSON.stringify({
    type: "data",
    subject: `bbops.telemetry.events.${sysid}`,
    timestamp: 1_732_400_000_500,
    payload: {
      sysid,
      entity_id: `boxbotix.local.robotics.uxs.vehicle.${sysid}`,
      domain: "surface-boat",
      seq: 7,
      msg_id: 0,
      kind: "heartbeat_edge",
      reasons: ["authority"],
      armed: true,
      authority: "direct-crsf-override",
      prev_authority: "companion-teleop",
      companion_mode: "teleop-velocity",
      link_tier: "mesh-high-rate",
      flags: {
        clock_sync_locked: true,
        teleop_deadman_clamped: false,
        crsf_link_healthy: true,
        companion_trunk_healthy: true,
      },
      failsafes: 0,
      timestamp: 1_732_400_000_500,
      ...overrides,
    },
  });
}

describe("telemetry.svelte.ts — WS #1 kinematic & transition event store", () => {
  beforeEach(() => {
    vi.useFakeTimers();
  });

  afterEach(() => {
    vi.useRealTimers();
  });

  // spec: cop-ui / Latest-wins kinematic store and bounded transition event ring buffer
  it("overwrites per-sysid kinematics in place without triggering per-frame $state reactivity and drops older timestamps", () => {
    const rig = new MockWebSocketRig();
    const store = createTelemetryStore({
      url: "ws://127.0.0.1:8081/ws",
      createSocket: rig.createSocket,
    });

    const probe = createReactiveProbe(() => {
      void store.status;
      void store.vehicleCount;
      void store.events;
      void store.getKinematics(1)?.ned_xyz[0];
      void store.getKinematics(1)?.hdg_deg;
    });

    try {
      flushSync();
      expect(probe.runs).toBe(1);

      store.connect();
      rig.latest.serverOpen();
      flushSync();
      const runsAfterOpen = probe.runs;

      // Birth of sysid 1 increments vehicleCount once.
      rig.latest.serverMessage(
        buildKinematicEnvelope(1, {
          seq: 1,
          ned_xyz: [1, 2, -3],
          hdg_deg: 45,
          timestamp: 1000,
        }),
      );
      flushSync();
      const runsAfterSysIdBirth = probe.runs;
      expect(runsAfterSysIdBirth).toBe(runsAfterOpen + 1);
      expect(store.vehicleCount).toBe(1);

      const firstRef = store.getKinematics(1) as VehicleKinematics;
      const firstQuatRef = firstRef.quat_wxyz;
      const firstNedRef = firstRef.ned_xyz;
      expect(firstRef).toBeDefined();

      // Stream 50 rapid 20 Hz updates (including an equal-millisecond fused #32 frame).
      for (let i = 1; i <= 50; i += 1) {
        const ts = i === 1 ? 1000 : 1000 + i * 50;
        rig.latest.serverMessage(
          buildKinematicEnvelope(1, {
            seq: (i + 1) % 256,
            msg_id: i === 1 ? 32 : 31,
            quat_wxyz: [0.7071, 0, 0, 0.7071],
            ned_xyz: [10 + i, 20 + i, -5],
            ned_vel: [2.5, -1.25, 0],
            hdg_deg: (90 + i) % 360,
            timestamp: ts,
          }),
        );
      }
      flushSync();

      // Zero additional reactive invalidations across all 50 in-place 20 Hz frames!
      expect(probe.runs).toBe(runsAfterSysIdBirth);
      const afterBurst = store.getKinematics(1) as VehicleKinematics;
      expect(afterBurst).toBe(firstRef);
      expect(afterBurst.quat_wxyz).toBe(firstQuatRef);
      expect(afterBurst.ned_xyz).toBe(firstNedRef);
      expect(afterBurst.ned_xyz).toEqual([60, 70, -5]);
      expect(afterBurst.timestamp).toBe(3500);
      expect(store.droppedOutOfOrderCount).toBe(0);

      // Delayed out-of-order packet (timestamp 3499 < 3500) must be dropped without mutating state.
      rig.latest.serverMessage(
        buildKinematicEnvelope(1, {
          seq: 99,
          quat_wxyz: [0, 1, 0, 0],
          ned_xyz: [-999, -999, -999],
          ned_vel: [-9, -9, -9],
          hdg_deg: 359,
          timestamp: 3499,
        }),
      );
      expect(store.droppedOutOfOrderCount).toBe(1);
      expect(afterBurst.ned_xyz).toEqual([60, 70, -5]);
      expect(afterBurst.hdg_deg).toBe(140);
      expect(afterBurst.timestamp).toBe(3500);
    } finally {
      probe.dispose();
      store.disconnect();
    }
  });

  // spec: cop-ui / Latest-wins kinematic store and bounded transition event ring buffer
  it("rejects non-finite, out-of-range, and mismatched-sysid telemetry frames without corrupting valid sessions", () => {
    const store = createTelemetryStore();
    store.ingestRawFrame(
      buildKinematicEnvelope(1, {
        ned_xyz: [5, 6, -7],
        hdg_deg: 180,
        timestamp: 2000,
      }),
    );
    expect(store.vehicleCount).toBe(1);

    const invalidFrames = [
      "{not-json",
      JSON.stringify({
        type: "put",
        subject: "bbops.telemetry.1",
        payload: {},
      }),
      // sysid 0 and 255 are outside controllable vehicle range 1..=254
      buildKinematicEnvelope(0, { sysid: 0 }),
      buildKinematicEnvelope(255, { sysid: 255 }),
      // subject/payload sysid mismatch
      JSON.stringify({
        type: "data",
        subject: "bbops.telemetry.1",
        timestamp: 2100,
        payload: JSON.parse(buildKinematicEnvelope(2)).payload,
      }),
      // out-of-range heading and lat/lon
      buildKinematicEnvelope(1, { hdg_deg: 360.1, timestamp: 2200 }),
      buildKinematicEnvelope(1, { hdg_deg: -0.1, timestamp: 2200 }),
      buildKinematicEnvelope(1, {
        lat_lon_alt: [91, 0, 10],
        timestamp: 2200,
      }),
      buildKinematicEnvelope(1, {
        lat_lon_alt: [0, -181, 10],
        timestamp: 2200,
      }),
      // non-canonical leading-zero sysid segment
      JSON.stringify({
        type: "data",
        subject: "bbops.telemetry.01",
        timestamp: 2200,
        payload: JSON.parse(buildKinematicEnvelope(1)).payload,
      }),
    ];

    for (const bad of invalidFrames) {
      store.ingestRawFrame(bad);
    }

    expect(store.malformedFrameCount).toBe(invalidFrames.length);
    expect(store.vehicleCount).toBe(1);
    expect(store.getKinematics(1)?.ned_xyz).toEqual([5, 6, -7]);
    expect(store.getKinematics(1)?.hdg_deg).toBe(180);
  });

  // spec: cop-ui / Latest-wins kinematic store and bounded transition event ring buffer
  it("retains the newest 100 discrete TransitionEvents in arrival order and preserves them across reconnects", () => {
    let clockMs = 1_732_400_000_000;
    const rig = new MockWebSocketRig();
    const store = createTelemetryStore({
      url: "ws://127.0.0.1:8081/ws",
      now: () => clockMs,
      createSocket: rig.createSocket,
    });

    store.connect();
    rig.latest.serverOpen();

    for (let i = 1; i <= 135; i += 1) {
      const sysid = ((i - 1) % 3) + 1;
      const isStatusText = i % 10 === 0;
      const isLinkEdge = i === 135;
      rig.latest.serverMessage(
        buildTransitionEnvelope(sysid, {
          seq: i % 256,
          kind: isLinkEdge
            ? "link_lost"
            : isStatusText
              ? "statustext"
              : "heartbeat_edge",
          failsafes: i,
          status_text: isStatusText
            ? {
                severity: 4,
                text: `WARN-${i}`,
                id: i,
                chunk_seq: 0,
              }
            : undefined,
          timestamp: 10_000 + i,
        }),
      );
    }

    expect(store.events).toHaveLength(100);
    expect(store.events[0]?.failsafes).toBe(36);
    expect(store.events[99]?.failsafes).toBe(135);
    expect(store.events[99]?.kind).toBe("link_lost");
    const statusEvent = store.events.find((e) => e.failsafes === 130);
    expect(statusEvent?.status_text?.text).toBe("WARN-130");

    // Simulate network drop and reconnect (#28 Option A: live ring buffer preserved + gap metadata tracked).
    clockMs += 5_000;
    rig.latest.serverDrop(1006);
    expect(store.lastDisconnectedAt).toBe(clockMs);
    expect(store.events).toHaveLength(100);

    vi.advanceTimersByTime(500);
    rig.latest.serverOpen();
    expect(store.reconnectCount).toBe(1);
    expect(store.events).toHaveLength(100);
    expect(store.events[0]?.failsafes).toBe(36);

    store.disconnect();
  });

  // spec: cop-ui / Latest-wins kinematic store and bounded transition event ring buffer
  it("transmits 25s (< 30s) keepalive frames while open and backs off 500ms -> 30s with clean reset", () => {
    let clockMs = 1_732_400_100_000;
    const rig = new MockWebSocketRig();
    const store = createTelemetryStore({
      url: "ws://127.0.0.1:8081/ws",
      now: () => clockMs,
      createSocket: rig.createSocket,
    });

    expect(() => createTelemetryStore({ keepaliveIntervalMs: 30_000 })).toThrow(
      RangeError,
    );

    store.connect();
    expect(store.status).toBe("connecting");
    expect(rig.sockets).toHaveLength(1);

    rig.latest.serverOpen();
    expect(store.status).toBe("connected");

    // Advance 24,999 ms -> no keepalive yet; +1 ms -> 25s keepalive sent!
    clockMs += 24_999;
    vi.advanceTimersByTime(24_999);
    expect(rig.latest.sentFrames).toHaveLength(0);

    clockMs += 1;
    vi.advanceTimersByTime(1);
    expect(rig.latest.sentFrames).toHaveLength(1);
    expect(JSON.parse(rig.latest.sentFrames[0] as string)).toEqual({
      type: "keepalive",
      timestamp: clockMs,
    });

    clockMs += 25_000;
    vi.advanceTimersByTime(25_000);
    expect(rig.latest.sentFrames).toHaveLength(2);

    // Drop connection and verify exact exponential backoff schedule:
    // 500, 1000, 2000, 4000, 8000, 16000, 30000, 30000 ms.
    const expectedDelays = [
      500, 1_000, 2_000, 4_000, 8_000, 16_000, 30_000, 30_000,
    ];
    rig.latest.serverDrop(1006);
    expect(store.status).toBe("reconnecting");

    for (let idx = 0; idx < expectedDelays.length; idx += 1) {
      const delay = expectedDelays[idx] as number;
      const beforeCount = rig.sockets.length;
      vi.advanceTimersByTime(delay - 1);
      expect(rig.sockets).toHaveLength(beforeCount);

      vi.advanceTimersByTime(1);
      expect(rig.sockets).toHaveLength(beforeCount + 1);

      if (idx < expectedDelays.length - 1) {
        // Handshake fails before open -> backoff continues growing.
        rig.latest.serverDrop(1006);
      }
    }

    // Establish clean connection -> backoff resets to 500 ms on subsequent drop.
    rig.latest.serverOpen();
    expect(store.status).toBe("connected");

    const countBeforeResetDrop = rig.sockets.length;
    rig.latest.serverDrop(1006);
    vi.advanceTimersByTime(499);
    expect(rig.sockets).toHaveLength(countBeforeResetDrop);
    vi.advanceTimersByTime(1);
    expect(rig.sockets).toHaveLength(countBeforeResetDrop + 1);

    store.disconnect();
    expect(store.status).toBe("disconnected");
  });

  // spec: cop-ui / Latest-wins kinematic store and bounded transition event ring buffer
  it("matches an independent reference model across seeded multi-sysid out-of-order and ring-buffer histories", () => {
    const rng = createSeededRng(0xbb095201);
    const maxEvents = 12;
    const store = createTelemetryStore({ maxEvents });

    const refKinematics = new Map<
      number,
      { timestamp: number; hdg_deg: number; nedX: number }
    >();
    const refEvents: TransitionEvent[] = [];
    let refDropped = 0;

    let sawEqualTimestampAccept = false;
    let sawOlderTimestampDrop = false;
    let sawRingEviction = false;
    let sawBoundarySysIds = false;

    const candidateSysIds = [1, 2, 17, 254];
    for (let step = 0; step < 250; step += 1) {
      const sysid = candidateSysIds[
        Math.floor(rng() * candidateSysIds.length)
      ] as number;
      if (sysid === 1 || sysid === 254) {
        sawBoundarySysIds = true;
      }

      if (rng() < 0.7) {
        const current = refKinematics.get(sysid);
        const roll = rng();
        let ts: number;
        if (!current) {
          ts = 10_000 + step * 10;
        } else if (roll < 0.25) {
          // Exact equal timestamp boundary
          ts = current.timestamp;
        } else if (roll < 0.55) {
          // Out-of-order older timestamp
          ts = current.timestamp - (1 + Math.floor(rng() * 50));
        } else {
          ts = current.timestamp + 1 + Math.floor(rng() * 50);
        }
        const hdg = Math.floor(rng() * 360);
        const nedX = step * 2.5;

        store.ingestRawFrame(
          buildKinematicEnvelope(sysid, {
            domain: sysid === 254 ? "surface-boat" : "rover",
            ned_xyz: [nedX, 0, 0],
            hdg_deg: hdg,
            timestamp: ts,
          }),
        );

        if (current && ts < current.timestamp) {
          refDropped += 1;
          sawOlderTimestampDrop = true;
        } else {
          if (current && ts === current.timestamp) {
            sawEqualTimestampAccept = true;
          }
          refKinematics.set(sysid, { timestamp: ts, hdg_deg: hdg, nedX });
        }
      } else {
        const ts = 50_000 + step;
        store.ingestRawFrame(
          buildTransitionEnvelope(sysid, {
            failsafes: step,
            timestamp: ts,
          }),
        );
        refEvents.push({
          sysid,
          entity_id: `boxbotix.local.robotics.uxs.vehicle.${sysid}`,
          domain: "surface-boat",
          seq: 7,
          msg_id: 0,
          kind: "heartbeat_edge",
          reasons: ["authority"],
          armed: true,
          authority: "direct-crsf-override",
          prev_authority: "companion-teleop",
          companion_mode: "teleop-velocity",
          link_tier: "mesh-high-rate",
          flags: {
            clock_sync_locked: true,
            teleop_deadman_clamped: false,
            crsf_link_healthy: true,
            companion_trunk_healthy: true,
          },
          failsafes: step,
          timestamp: ts,
        });
        if (refEvents.length > maxEvents) {
          refEvents.shift();
          sawRingEviction = true;
        }
      }

      expect(store.vehicleCount).toBe(refKinematics.size);
      expect(store.droppedOutOfOrderCount).toBe(refDropped);
      for (const [id, expected] of refKinematics.entries()) {
        const actual = store.getKinematics(id);
        expect(actual?.timestamp).toBe(expected.timestamp);
        expect(actual?.hdg_deg).toBe(expected.hdg_deg);
        expect(actual?.ned_xyz[0]).toBe(expected.nedX);
      }
      expect(store.events.map((e) => e.failsafes)).toEqual(
        refEvents.map((e) => e.failsafes),
      );
    }

    expect(sawEqualTimestampAccept).toBe(true);
    expect(sawOlderTimestampDrop).toBe(true);
    expect(sawRingEviction).toBe(true);
    expect(sawBoundarySysIds).toBe(true);
  });

  // spec: cop-ui / Latest-wins kinematic store and bounded transition event ring buffer
  it("keeps connect() idempotent on the active URL and resets feed state, timestamp guards, and event history on connect(overrideUrl)", () => {
    const rig = new MockWebSocketRig();
    const urlA = "ws://127.0.0.1:8081/ws-a";
    const urlB = "ws://127.0.0.1:8091/ws-b";
    const store = createTelemetryStore({
      url: urlA,
      createSocket: rig.createSocket,
    });

    store.connect();
    const socketA = rig.latest;
    socketA.serverOpen();
    expect(store.url).toBe(urlA);
    expect(store.status).toBe("connected");
    expect(rig.sockets).toHaveLength(1);

    // Populate endpoint A with sysids 1 (ts 5000) and 2 (ts 5050) plus a transition event.
    socketA.serverMessage(
      buildKinematicEnvelope(1, {
        ned_xyz: [50, 60, -10],
        hdg_deg: 90,
        timestamp: 5_000,
      }),
    );
    socketA.serverMessage(
      buildKinematicEnvelope(2, {
        ned_xyz: [70, 80, -20],
        hdg_deg: 180,
        timestamp: 5_050,
      }),
    );
    socketA.serverMessage(
      buildTransitionEnvelope(1, {
        failsafes: 3,
        timestamp: 5_060,
      }),
    );
    expect(store.vehicleCount).toBe(2);
    expect(store.events).toHaveLength(1);

    // Calling connect() or connect(urlA) while active on urlA is idempotent and preserves A's state.
    store.connect();
    store.connect(`  ${urlA}  `);
    expect(rig.sockets).toHaveLength(1);
    expect(store.url).toBe(urlA);
    expect(store.vehicleCount).toBe(2);
    expect(store.events).toHaveLength(1);

    // Switching to urlB while connected to urlA closes socketA, clears A's vehicles/events/timestamp watermarks, and opens socketB.
    store.connect(urlB);
    expect(store.url).toBe(urlB);
    expect(socketA.closeCalls).toEqual([
      {
        code: 1000,
        reason: "endpoint override",
      },
    ]);
    expect(store.vehicleCount).toBe(0);
    expect(store.getKinematics(1)).toBeUndefined();
    expect(store.getKinematics(2)).toBeUndefined();
    expect(store.events).toEqual([]);
    expect(store.lastDisconnectedAt).toBeNull();
    expect(rig.sockets).toHaveLength(2);
    const socketB = rig.latest;
    expect(socketB.url).toBe(urlB);

    // Any late message from detached socketA is ignored.
    socketA.serverMessage(
      buildKinematicEnvelope(2, {
        ned_xyz: [99, 99, 99],
        timestamp: 6_000,
      }),
    );
    expect(store.getKinematics(2)).toBeUndefined();

    // Endpoint B delivers sysid 1 at an earlier timestamp (4900 < A's 5000): accepted without out-of-order rejection, and A-only sysid 2 stays absent!
    socketB.serverOpen();
    expect(store.reconnectCount).toBe(0);
    socketB.serverMessage(
      buildKinematicEnvelope(1, {
        ned_xyz: [12, 34, -5],
        hdg_deg: 270,
        timestamp: 4_900,
      }),
    );
    expect(store.droppedOutOfOrderCount).toBe(0);
    expect(store.vehicleCount).toBe(1);
    expect(store.getKinematics(1)?.ned_xyz).toEqual([12, 34, -5]);
    expect(store.getKinematics(1)?.timestamp).toBe(4_900);
    expect(store.getKinematics(2)).toBeUndefined();

    // When socketB drops, same-endpoint reconnect preserves B's state and targets urlB (never urlA).
    socketB.serverDrop(1006);
    vi.advanceTimersByTime(500);
    expect(rig.sockets).toHaveLength(3);
    expect(rig.latest.url).toBe(urlB);
    expect(store.url).toBe(urlB);
    expect(store.getKinematics(1)?.ned_xyz).toEqual([12, 34, -5]);

    store.disconnect();
  });
});
