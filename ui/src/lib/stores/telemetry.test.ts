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

  // spec: cop-ui / Latest-wins kinematic store and bounded transition event ring buffer
  it("stamps receivedAtMs from the local browser clock across +60s and -60s server skew while retaining producer timestamps and defaulting omitted has_local_pos to false", () => {
    let browserNowMs = 1_732_400_100_000;
    const store = createTelemetryStore({
      now: () => browserNowMs,
    });

    // +60s ahead server envelope & producer timestamp, with legacy omitted has_local_pos.
    store.ingestRawFrame(
      buildKinematicEnvelope(
        1,
        {
          seq: 1,
          timestamp: browserNowMs + 60_000,
        },
        browserNowMs + 60_000,
      ),
    );
    const ahead = store.getKinematics(1);
    expect(ahead?.receivedAtMs).toBe(browserNowMs);
    expect(ahead?.timestamp).toBe(browserNowMs + 60_000);
    expect(ahead?.has_local_pos).toBe(false);

    // -60s behind server envelope & producer timestamp on sysid 2, with explicit has_local_pos: true.
    browserNowMs += 250;
    store.ingestRawFrame(
      buildKinematicEnvelope(
        2,
        {
          seq: 1,
          has_local_pos: true,
          timestamp: browserNowMs - 60_000,
        },
        browserNowMs - 60_000,
      ),
    );
    const behind = store.getKinematics(2);
    expect(behind?.receivedAtMs).toBe(browserNowMs);
    expect(behind?.timestamp).toBe(browserNowMs - 60_000);
    expect(behind?.has_local_pos).toBe(true);
  });

  // spec: cop-ui / Latest-wins kinematic store and bounded transition event ring buffer
  it("preserves explicit has_heading availability across unknown #33 heading and measured 0° North in-place updates", () => {
    const store = createTelemetryStore();

    // #33 GLOBAL_POSITION_INT with Hdg=65535 before #31 -> hdg_deg: 0, has_heading: false.
    store.ingestRawFrame(
      buildKinematicEnvelope(1, {
        seq: 1,
        msg_id: 33,
        quat_wxyz: [0, 0, 0, 0],
        has_attitude: false,
        hdg_deg: 0,
        has_heading: false,
        timestamp: 10_000,
      }),
    );
    const first = store.getKinematics(1);
    expect(first?.hdg_deg).toBe(0);
    expect(first?.has_attitude).toBe(false);
    expect(first?.has_heading).toBe(false);

    // Measured 0° North #33 update overwrites in place with has_heading: true.
    store.ingestRawFrame(
      buildKinematicEnvelope(1, {
        seq: 2,
        msg_id: 33,
        quat_wxyz: [0, 0, 0, 0],
        has_attitude: false,
        hdg_deg: 0,
        has_heading: true,
        timestamp: 10_050,
      }),
    );
    expect(store.getKinematics(1)).toBe(first);
    expect(first?.hdg_deg).toBe(0);
    expect(first?.has_heading).toBe(true);

    // Subsequent unknown-heading #33 update clears has_heading back to false in place.
    store.ingestRawFrame(
      buildKinematicEnvelope(1, {
        seq: 3,
        msg_id: 33,
        quat_wxyz: [0, 0, 0, 0],
        has_attitude: false,
        hdg_deg: 0,
        has_heading: false,
        timestamp: 10_100,
      }),
    );
    expect(first?.has_heading).toBe(false);

    // Legacy WS1 session on one connection:
    // 1) unannotated #33 position -> [false, false]
    // 2) genuine #31 level-North attitude ([1,0,0,0], [0,0,0], hdg_deg=0) -> [true, true]
    // 3) #32 local-position 50 ms later with fused [1,0,0,0] -> stays [true, true]
    // 4) #33 global-position 50 ms later with fused [1,0,0,0] -> stays [true, true]
    // 5) session disconnect + new session -> resets back to [false, false] on unannotated #33!
    const rig = new MockWebSocketRig();
    const legacyStore = createTelemetryStore({
      url: "ws://127.0.0.1:8081/ws",
      createSocket: rig.createSocket,
    });
    legacyStore.connect();
    rig.latest.serverOpen();

    const makeLegacyFrame = (
      seq: number,
      msgId: number,
      timestamp: number,
      extra: Record<string, unknown> = {},
    ) =>
      JSON.stringify({
        type: "data",
        subject: "bbops.telemetry.2",
        timestamp,
        payload: {
          sysid: 2,
          domain: "rover",
          seq,
          msg_id: msgId,
          quat_wxyz: [1, 0, 0, 0],
          body_rates: [0, 0, 0],
          ned_xyz: [0, 0, 0],
          ned_vel: [0, 0, 0],
          lat_lon_alt: [30.2672, -97.7431, 150],
          has_global_pos: true,
          hdg_deg: 0,
          timestamp,
          ...extra,
        },
      });

    rig.latest.serverMessage(makeLegacyFrame(1, 33, 10_200));
    const legacyParsed = legacyStore.getKinematics(2);
    expect(legacyParsed?.has_attitude).toBe(false);
    expect(legacyParsed?.has_heading).toBe(false);
    expect(legacyParsed?.has_local_pos).toBe(false);

    // Out-of-order #31 and #32 frames (timestamp < 10_200) are dropped and MUST NOT latch attitude or local pos!
    rig.latest.serverMessage(makeLegacyFrame(2, 31, 10_150));
    rig.latest.serverMessage(makeLegacyFrame(2, 32, 10_160));
    expect(legacyStore.droppedOutOfOrderCount).toBe(2);
    expect(legacyParsed?.has_local_pos).toBe(false);

    // First in-order legacy #32 LOCAL_POSITION_NED (including measured zero [0,0,0]) latches local pos while attitude stays unmeasured!
    rig.latest.serverMessage(makeLegacyFrame(3, 32, 10_225));
    expect(legacyParsed?.has_attitude).toBe(false);
    expect(legacyParsed?.has_heading).toBe(false);
    expect(legacyParsed?.has_local_pos).toBe(true);

    // Genuine in-order #31 ATTITUDE_QUATERNION at exact level North ([1,0,0,0], [0,0,0], msg_id=31) latches attitude + heading and preserves local pos!
    rig.latest.serverMessage(makeLegacyFrame(4, 31, 10_250));
    expect(legacyParsed?.has_attitude).toBe(true);
    expect(legacyParsed?.has_heading).toBe(true);
    expect(legacyParsed?.has_local_pos).toBe(true);

    // #32 LOCAL_POSITION_NED 50 ms later carrying fused [1,0,0,0] retains measured attitude + heading + local pos!
    rig.latest.serverMessage(
      makeLegacyFrame(5, 32, 10_300, { ned_xyz: [4, -2, 0] }),
    );
    expect(legacyParsed?.ned_xyz).toEqual([4, -2, 0]);
    expect(legacyParsed?.has_attitude).toBe(true);
    expect(legacyParsed?.has_heading).toBe(true);
    expect(legacyParsed?.has_local_pos).toBe(true);

    // #33 GLOBAL_POSITION_INT 50 ms later carrying fused [1,0,0,0] also retains measured attitude + heading + local pos!
    rig.latest.serverMessage(
      makeLegacyFrame(6, 33, 10_350, {
        ned_xyz: [4, -2, 0],
        lat_lon_alt: [30.2673, -97.7432, 150],
      }),
    );
    expect(legacyParsed?.has_attitude).toBe(true);
    expect(legacyParsed?.has_heading).toBe(true);
    expect(legacyParsed?.has_local_pos).toBe(true);

    // Explicit modern availability flags override the legacy latch immediately (even on msg_id=32)!
    rig.latest.serverMessage(
      makeLegacyFrame(7, 32, 10_400, {
        quat_wxyz: [0, 0, 0, 0],
        has_attitude: false,
        has_local_pos: false,
        has_heading: true,
        hdg_deg: 0,
      }),
    );
    expect(legacyParsed?.has_attitude).toBe(false);
    expect(legacyParsed?.has_local_pos).toBe(false);
    expect(legacyParsed?.has_heading).toBe(true);

    // Re-latch via #32 + #31, verify link_lost clears latches on next legacy #33, then re-latch and verify socket reconnect resets inference!
    rig.latest.serverMessage(
      makeLegacyFrame(8, 32, 10_425, { ned_xyz: [12, -5, -4] }),
    );
    rig.latest.serverMessage(
      makeLegacyFrame(9, 31, 10_450, { ned_xyz: [12, -5, -4] }),
    );
    expect(legacyParsed?.has_attitude).toBe(true);
    expect(legacyParsed?.has_heading).toBe(true);
    expect(legacyParsed?.has_local_pos).toBe(true);

    rig.latest.serverMessage(
      buildTransitionEnvelope(2, {
        kind: "link_lost",
        timestamp: 10_460,
      }),
    );
    rig.latest.serverMessage(makeLegacyFrame(10, 33, 10_475));
    expect(legacyParsed?.has_attitude).toBe(false);
    expect(legacyParsed?.has_heading).toBe(false);
    expect(legacyParsed?.has_local_pos).toBe(false);

    rig.latest.serverMessage(makeLegacyFrame(11, 32, 10_480));
    rig.latest.serverMessage(makeLegacyFrame(12, 31, 10_490));
    expect(legacyParsed?.has_attitude).toBe(true);
    expect(legacyParsed?.has_local_pos).toBe(true);

    rig.latest.serverDrop(1006);
    vi.advanceTimersByTime(500);
    rig.latest.serverOpen();

    rig.latest.serverMessage(makeLegacyFrame(13, 33, 10_500));
    expect(legacyParsed?.has_attitude).toBe(false);
    expect(legacyParsed?.has_heading).toBe(false);
    expect(legacyParsed?.has_local_pos).toBe(false);

    legacyStore.disconnect();
  });

  // spec: cop-ui / Latest-wins kinematic store and bounded transition event ring buffer
  // spec: cop-ui / Fleet health readiness, per-vehicle observation freshness, and last-known fault retention
  it("preserves per-sysid linkLost state across 100-event ring eviction, clearEvents, socket reconnects, and pre-loss packets until link_restored or post-loss telemetry arrives", () => {
    const rig = new MockWebSocketRig();
    let nowMs = 1_732_500_000_000;
    const store = createTelemetryStore({
      url: "ws://127.0.0.1:8081/ws",
      createSocket: rig.createSocket,
      now: () => nowMs,
    });

    store.connect();
    rig.latest.serverOpen();

    // 1. Admit sysid=1 with live kinematics at t0.
    rig.latest.serverMessage(
      buildKinematicEnvelope(1, {
        seq: 1,
        msg_id: 32,
        has_local_pos: true,
        ned_xyz: [10, -4, -8],
        timestamp: nowMs,
      }),
    );
    expect(store.getLinkState(1)).toEqual({
      sysid: 1,
      linkLost: false,
      linkLostAtMs: null,
      linkLostTimestamp: null,
      lastPacketTimestamp: nowMs,
      lastPacketReceivedAtMs: nowMs,
    });

    // 2. Steady-state 20 Hz updates advance lastPacketTimestamp/lastPacketReceivedAtMs in place.
    const initialPacketMs = nowMs;
    nowMs += 50;
    rig.latest.serverMessage(
      buildKinematicEnvelope(1, {
        seq: 2,
        msg_id: 32,
        has_local_pos: true,
        ned_xyz: [11, -4, -8],
        timestamp: nowMs,
      }),
    );
    expect(store.getLinkState(1)?.linkLost).toBe(false);
    expect(store.getLinkState(1)?.lastPacketTimestamp).toBe(
      initialPacketMs + 50,
    );
    expect(store.getLinkState(1)?.lastPacketReceivedAtMs).toBe(
      initialPacketMs + 50,
    );

    // 3. sysid=1 crosses silence threshold at +3_050ms -> link_lost latches without advancing lastPacketTimestamp.
    const lastLivePacketMs = nowMs;
    nowMs += 3_000;
    const lossMs = nowMs;
    rig.latest.serverMessage(
      buildTransitionEnvelope(1, {
        kind: "link_lost",
        reasons: ["silence_timeout"],
        timestamp: lossMs,
      }),
    );
    expect(store.getLinkState(1)).toEqual({
      sysid: 1,
      linkLost: true,
      linkLostAtMs: lossMs,
      linkLostTimestamp: lossMs,
      lastPacketTimestamp: lastLivePacketMs,
      lastPacketReceivedAtMs: lastLivePacketMs,
    });

    // 4. Push 125 events for sysid=2 so sysid=1's link_lost event is completely evicted from the 100-entry ring.
    for (let i = 0; i < 125; i += 1) {
      nowMs += 10;
      rig.latest.serverMessage(
        buildTransitionEnvelope(2, {
          seq: i % 256,
          kind: "statustext",
          timestamp: nowMs,
          status_text: {
            severity: 6,
            text: `sys2-event-${i}`,
            id: 0,
            chunk_seq: 0,
          },
        }),
      );
    }
    expect(store.events).toHaveLength(100);
    expect(store.events.some((ev) => ev.sysid === 1)).toBe(false);
    expect(store.getLinkState(1)?.linkLost).toBe(true);
    expect(store.getLinkState(2)?.linkLost).toBe(false);

    // Clearing the UI event log also preserves per-sysid link state.
    store.clearEvents();
    expect(store.events).toHaveLength(0);
    expect(store.getLinkState(1)?.linkLost).toBe(true);

    // 5. Socket drop + reconnect alone NEVER clears linkLost.
    rig.latest.serverDrop(1006);
    vi.advanceTimersByTime(500);
    rig.latest.serverOpen();
    expect(store.status).toBe("connected");
    expect(store.getLinkState(1)?.linkLost).toBe(true);

    // 6. A delayed pre-loss kinematic frame (timestamp between lastLivePacketMs and lossMs) does NOT clear linkLost.
    nowMs += 100;
    rig.latest.serverMessage(
      buildKinematicEnvelope(1, {
        seq: 3,
        msg_id: 32,
        has_local_pos: true,
        ned_xyz: [11.5, -4, -8],
        timestamp: lossMs - 500,
      }),
    );
    expect(store.getLinkState(1)?.linkLost).toBe(true);

    // 7. Explicit link_restored clears linkLost, and endpoint override resets link state cleanly.
    nowMs += 200;
    const restoreMs = nowMs;
    rig.latest.serverMessage(
      buildTransitionEnvelope(1, {
        seq: 4,
        kind: "link_restored",
        reasons: ["link_restored"],
        timestamp: restoreMs,
      }),
    );
    expect(store.getLinkState(1)).toEqual({
      sysid: 1,
      linkLost: false,
      linkLostAtMs: null,
      linkLostTimestamp: null,
      lastPacketTimestamp: restoreMs,
      lastPacketReceivedAtMs: restoreMs,
    });

    // Re-trigger link_lost and verify a post-loss kinematic packet (>= lossTimestamp) also clears linkLost.
    nowMs += 3_100;
    const secondLossMs = nowMs;
    rig.latest.serverMessage(
      buildTransitionEnvelope(1, {
        seq: 5,
        kind: "link_lost",
        reasons: ["silence_timeout"],
        timestamp: secondLossMs,
      }),
    );
    expect(store.getLinkState(1)?.linkLost).toBe(true);

    nowMs += 50;
    const resumeKinMs = nowMs;
    rig.latest.serverMessage(
      buildKinematicEnvelope(1, {
        seq: 6,
        msg_id: 32,
        has_local_pos: true,
        ned_xyz: [15, -2, -8],
        timestamp: resumeKinMs,
      }),
    );
    expect(store.getLinkState(1)?.linkLost).toBe(false);
    expect(store.getLinkState(1)?.lastPacketTimestamp).toBe(resumeKinMs);

    store.connect("ws://127.0.0.1:8099/ws-next");
    expect(store.getLinkState(1)).toBeUndefined();
    store.disconnect();
  });

  // spec: cop-ui / Latest-wins kinematic store and bounded transition event ring buffer
  // spec: cop-ui / Fleet health readiness, per-vehicle observation freshness, and last-known fault retention
  it("ignores delayed older link_restored and link_lost transitions until a genuinely newer transition arrives", () => {
    const rig = new MockWebSocketRig();
    let nowMs = 1_732_510_000_000;
    const store = createTelemetryStore({
      url: "ws://127.0.0.1:8081/ws",
      createSocket: rig.createSocket,
      now: () => nowMs,
    });

    store.connect();
    rig.latest.serverOpen();

    // 1. Live kinematics at T.
    const t0 = nowMs;
    rig.latest.serverMessage(
      buildKinematicEnvelope(1, {
        seq: 1,
        msg_id: 32,
        has_local_pos: true,
        ned_xyz: [10, -4, -8],
        timestamp: t0,
      }),
    );
    expect(store.getLinkState(1)?.linkLost).toBe(false);

    // 2. link_lost at T + 3000 ms latches linkLost = true.
    nowMs = t0 + 3_000;
    rig.latest.serverMessage(
      buildTransitionEnvelope(1, {
        seq: 2,
        kind: "link_lost",
        reasons: ["silence_timeout"],
        timestamp: t0 + 3_000,
      }),
    );
    expect(store.getLinkState(1)).toEqual({
      sysid: 1,
      linkLost: true,
      linkLostAtMs: t0 + 3_000,
      linkLostTimestamp: t0 + 3_000,
      lastPacketTimestamp: t0,
      lastPacketReceivedAtMs: t0,
    });

    // 3. Delayed out-of-order link_restored stamped T + 1000 ms (>= T, < T + 3000 ms) MUST NOT clear linkLost.
    nowMs = t0 + 3_200;
    rig.latest.serverMessage(
      buildTransitionEnvelope(1, {
        seq: 3,
        kind: "link_restored",
        reasons: ["link_restored"],
        timestamp: t0 + 1_000,
      }),
    );
    expect(store.getLinkState(1)).toEqual({
      sysid: 1,
      linkLost: true,
      linkLostAtMs: t0 + 3_000,
      linkLostTimestamp: t0 + 3_000,
      lastPacketTimestamp: t0,
      lastPacketReceivedAtMs: t0,
    });

    // 4. Genuinely newer link_restored stamped T + 4000 ms clears linkLost.
    nowMs = t0 + 4_000;
    rig.latest.serverMessage(
      buildTransitionEnvelope(1, {
        seq: 4,
        kind: "link_restored",
        reasons: ["link_restored"],
        timestamp: t0 + 4_000,
      }),
    );
    expect(store.getLinkState(1)).toEqual({
      sysid: 1,
      linkLost: false,
      linkLostAtMs: null,
      linkLostTimestamp: null,
      lastPacketTimestamp: t0 + 4_000,
      lastPacketReceivedAtMs: t0 + 4_000,
    });

    // 5. Delayed out-of-order link_lost stamped T + 3500 ms (< T + 4000 ms) MUST NOT re-latch linkLost.
    nowMs = t0 + 4_100;
    rig.latest.serverMessage(
      buildTransitionEnvelope(1, {
        seq: 5,
        kind: "link_lost",
        reasons: ["silence_timeout"],
        timestamp: t0 + 3_500,
      }),
    );
    expect(store.getLinkState(1)?.linkLost).toBe(false);

    store.disconnect();
  });
});
