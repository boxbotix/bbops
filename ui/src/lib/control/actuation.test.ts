// SPDX-License-Identifier: MIT
// Copyright (c) 2026 boxbotix

import { flushSync, mount, unmount } from "svelte";
import { describe, expect, it, vi } from "vitest";
import { createDiagnosticsDraftStore } from "../bench/tuning-rack";
import WorkspaceShell from "../components/WorkspaceShell.svelte";
import { createSkgStore } from "../stores/skg.svelte";
import { createTelemetryStore } from "../stores/telemetry.svelte";
import {
  MockWebSocketRig,
  buildVehicleStoredPayload,
  createSeededRng,
  makeTriple,
} from "../stores/test-helpers";
import { createViewportStore } from "../stores/viewport.svelte";
import type { ControlAuthority, LinkTier } from "../uxs";
import { handleViewportHotkey } from "../viewport/hotkeys";
import {
  COMMIT_UNKNOWN_BANNER_TEXT,
  type ControlClient,
  type ControlCommandRequest,
  type ControlTeleopRequest,
  type DiscreteCommandPhase,
  createActuationController,
  createControlClient,
  extractLatestVehicleCommand,
} from "./actuation.svelte";

function buildTransitionFrame(
  sysid: number,
  opts: {
    seq?: number;
    kind?:
      "heartbeat_edge" | "failsafe_increment" | "link_lost" | "link_restored";
    armed?: boolean;
    authority?: ControlAuthority;
    companionMode?:
      "disarmed" | "hold-position" | "teleop-velocity" | "autonomous-velocity";
    linkTier?: LinkTier;
    timestamp?: number;
  } = {},
): string {
  return JSON.stringify({
    type: "data",
    subject: `bbops.telemetry.events.${sysid}`,
    payload: {
      sysid,
      entity_id: `boxbotix.local.robotics.uxs.vehicle.${sysid}`,
      domain: "quadcopter",
      seq: opts.seq ?? 1,
      msg_id: 0,
      kind: opts.kind ?? "heartbeat_edge",
      reasons: ["authority"],
      armed: opts.armed ?? true,
      authority: opts.authority ?? "companion-teleop",
      companion_mode: opts.companionMode ?? "teleop-velocity",
      link_tier: opts.linkTier ?? "mesh-high-rate",
      flags: {
        clock_sync_locked: true,
        teleop_deadman_clamped: false,
        crsf_link_healthy: true,
        companion_trunk_healthy: true,
      },
      failsafes: 0,
      timestamp: opts.timestamp ?? Date.parse("2026-11-24T11:00:05.000Z"),
    },
  });
}

function buildCommandEntityFrame(opts: {
  id: string;
  revision: number;
  sysid: number;
  cmdId?: number;
  action: string;
  phase: DiscreteCommandPhase;
  ackResult?: number;
  detail?: string;
  issuedMs: number;
  updatedMs: number;
}): string {
  const ts = new Date(opts.updatedMs).toISOString();
  return JSON.stringify({
    type: "put",
    bucket: "ENTITY_STATES",
    key: opts.id,
    revision: opts.revision,
    payload: {
      id: opts.id,
      updated_at: ts,
      triples: [
        makeTriple(opts.id, "uxs.command.sysid", opts.sysid, {
          source: "control_0xb1",
          timestamp: ts,
        }),
        makeTriple(opts.id, "uxs.command.cmdid", opts.cmdId ?? 400, {
          source: "control_0xb1",
          timestamp: ts,
        }),
        makeTriple(opts.id, "uxs.command.action", opts.action, {
          source: "control_0xb1",
          timestamp: ts,
        }),
        makeTriple(opts.id, "uxs.command.phase", opts.phase, {
          source: "control_0xb1",
          timestamp: ts,
        }),
        makeTriple(opts.id, "uxs.command.ackresult", opts.ackResult ?? -1, {
          source: "control_0xb1",
          timestamp: ts,
        }),
        makeTriple(
          opts.id,
          "uxs.command.detail",
          opts.detail ??
            (opts.phase === "commit_unknown"
              ? COMMIT_UNKNOWN_BANNER_TEXT
              : "Awaiting COMMAND_ACK"),
          {
            source: "control_0xb1",
            timestamp: ts,
          },
        ),
        makeTriple(opts.id, "uxs.command.issuedms", opts.issuedMs, {
          source: "control_0xb1",
          timestamp: ts,
        }),
        makeTriple(opts.id, "uxs.command.updatedms", opts.updatedMs, {
          source: "control_0xb1",
          timestamp: ts,
        }),
      ],
    },
  });
}

describe("M3.1 Actuation Client, Controller & ControlPanel (OQ2 + OQ3 Option A)", () => {
  // spec: cop-ui / Guarded Vehicle ControlPanel, LinkTier supervisory collapse, sub-2ms hardware preemption release, and SITL fault lab
  it("posts non-retrying /control/command and /control/teleop envelopes and extracts latest SKG command entity per sysid", async () => {
    const fetchCalls: Array<{ url: string; body: unknown }> = [];
    let failNext = false;
    const client = createControlClient({
      baseUrl: "http://127.0.0.1:8080/",
      fetchImpl: (async (input, init) => {
        fetchCalls.push({
          url: String(input),
          body: JSON.parse(String(init?.body ?? "{}")),
        });
        if (failNext) {
          throw new Error("simulated socket reset");
        }
        return new Response(
          JSON.stringify({
            ok: true,
            result: {
              command_id: "boxbotix.local.robotics.uxs.command.s1c1",
              sysid: 1,
              cmd_id: 17,
              action: "hold-position",
              phase: "pending",
              detail: "Awaiting COMMAND_ACK",
              issued_ms: 1700000000100,
            },
          }),
          { status: 200, headers: { "Content-Type": "application/json" } },
        );
      }) as typeof fetch,
    });

    const okRes = await client.submitCommand({
      sysid: 1,
      action: "hold-position",
    });
    expect(okRes.ok).toBe(true);
    expect(fetchCalls).toHaveLength(1);
    expect(fetchCalls[0]).toEqual({
      url: "http://127.0.0.1:8080/control/command",
      body: { sysid: 1, action: "hold-position" },
    });

    // Network failure never auto-retries (strictly 1 fetch attempt).
    failNext = true;
    const netErr = await client.submitTeleop({
      sysid: 1,
      frame: "local-ned",
      vx: 1.5,
      vy: 0,
      vz: 0,
      yaw_rate: 0,
      engage: true,
    });
    expect(netErr).toEqual({
      ok: false,
      error: {
        code: "network_error",
        message: "simulated socket reset",
        status: 0,
      },
    });
    expect(fetchCalls).toHaveLength(2);

    const skgStore = createSkgStore();
    skgStore.ingestRawFrame(
      JSON.stringify({
        type: "synced",
        bucket: "ENTITY_STATES",
        revision: 1,
      }),
    );
    skgStore.ingestRawFrame(
      buildCommandEntityFrame({
        id: "boxbotix.local.robotics.uxs.command.s1c1",
        revision: 2,
        sysid: 1,
        cmdId: 17,
        action: "hold-position",
        phase: "commit_unknown",
        issuedMs: 1000,
        updatedMs: 2500,
      }),
    );
    skgStore.ingestRawFrame(
      buildCommandEntityFrame({
        id: "boxbotix.local.robotics.uxs.command.s1c2",
        revision: 3,
        sysid: 1,
        cmdId: 400,
        action: "disarm",
        phase: "confirmed",
        ackResult: 0,
        detail: "Accepted by vehicle",
        issuedMs: 3000,
        updatedMs: 3050,
      }),
    );
    skgStore.ingestRawFrame(
      buildCommandEntityFrame({
        id: "boxbotix.local.robotics.uxs.command.s2c1",
        revision: 4,
        sysid: 2,
        cmdId: 400,
        action: "arm",
        phase: "pending",
        issuedMs: 9000,
        updatedMs: 9000,
      }),
    );

    const latestSys1 = extractLatestVehicleCommand(
      skgStore.entities.values(),
      1,
    );
    expect(latestSys1?.id).toBe("boxbotix.local.robotics.uxs.command.s1c2");
    expect(latestSys1?.phase).toBe("confirmed");
    expect(
      extractLatestVehicleCommand(skgStore.entities.values(), 2)?.action,
    ).toBe("arm");
  });

  // spec: cop-ui / Guarded Vehicle ControlPanel, LinkTier supervisory collapse, sub-2ms hardware preemption release, and SITL fault lab
  it("enforces Engage Teleop gate, <2ms CRSF preemption latch, mesh-supervisory collapse, commit_unknown zero-retry UX, sitl-local lab gating, and #41/#43 non-actuating boundaries", async () => {
    vi.useFakeTimers();
    try {
      const nowMs = Date.parse("2026-11-24T11:00:01.000Z");
      const ws1Rig = new MockWebSocketRig();
      const ws2Rig = new MockWebSocketRig();
      const skgStore = createSkgStore({
        url: "ws://127.0.0.1:8082/ws",
        createSocket: ws2Rig.createSocket,
        now: () => nowMs,
      });
      const telemetryStore = createTelemetryStore({
        url: "ws://127.0.0.1:8081/ws",
        createSocket: ws1Rig.createSocket,
        now: () => nowMs,
      });
      telemetryStore.connect();
      skgStore.connect();
      ws1Rig.latest.serverOpen();
      ws2Rig.latest.serverOpen();
      const viewportStore = createViewportStore({ skgStore });
      const diagnosticsDraftStore = createDiagnosticsDraftStore();

      const commandCalls: ControlCommandRequest[] = [];
      const teleopCalls: ControlTeleopRequest[] = [];
      let nextCmdSeq = 0;

      const controlClient: ControlClient = {
        async submitCommand(req) {
          commandCalls.push(req);
          nextCmdSeq += 1;
          return {
            ok: true,
            result: {
              command_id: `boxbotix.local.robotics.uxs.command.s${req.sysid}c${nextCmdSeq}`,
              sysid: req.sysid,
              cmd_id: req.action === "hold-position" ? 17 : 400,
              action: req.action,
              phase: "pending",
              detail: "Awaiting COMMAND_ACK",
              issued_ms: nowMs,
            },
          };
        },
        async submitTeleop(req) {
          teleopCalls.push(req);
          return {
            ok: true,
            result: {
              sysid: req.sysid,
              lease_active: !req.stop,
              expires_ms: nowMs + 240,
            },
          };
        },
      };

      const quadSitl = buildVehicleStoredPayload(
        1,
        {
          "uxs.domain.kind": "quadcopter",
          "uxs.authority.state": "companion-teleop",
          "uxs.mode.companion": "teleop-velocity",
          "uxs.link.tier": "sitl-local",
          "uxs.health.armed": true,
        },
        { observedAt: "2026-11-24T11:00:00.900Z" },
      );
      const roverMesh = buildVehicleStoredPayload(
        2,
        {
          "uxs.domain.kind": "rover",
          "uxs.authority.state": "companion-teleop",
          "uxs.mode.companion": "teleop-velocity",
          "uxs.link.tier": "mesh-high-rate",
          "uxs.health.armed": true,
        },
        { observedAt: "2026-11-24T11:00:00.900Z" },
      );

      skgStore.ingestRawFrame(
        JSON.stringify({
          type: "put",
          bucket: "ENTITY_STATES",
          key: quadSitl.id,
          revision: 1,
          payload: quadSitl,
        }),
      );
      skgStore.ingestRawFrame(
        JSON.stringify({
          type: "put",
          bucket: "ENTITY_STATES",
          key: roverMesh.id,
          revision: 2,
          payload: roverMesh,
        }),
      );
      skgStore.ingestRawFrame(
        JSON.stringify({
          type: "synced",
          bucket: "ENTITY_STATES",
          revision: 2,
        }),
      );

      viewportStore.selectEntity(quadSitl.id);
      viewportStore.setWorkspace("pilot");

      const hostEl = document.createElement("div");
      document.body.appendChild(hostEl);
      const app = mount(WorkspaceShell, {
        target: hostEl,
        props: {
          telemetryStore,
          skgStore,
          viewportStore,
          controlClient,
          diagnosticsDraftStore,
          now: () => nowMs,
        },
      });
      flushSync();

      // 1. SITL Fault & Current Lab is visible on sitl-local Quadcopter (#1).
      expect(
        hostEl.querySelector('[data-testid="control-panel"]'),
      ).not.toBeNull();
      expect(
        hostEl.querySelector('[data-testid="sitl-lab-toggle"]'),
      ).not.toBeNull();

      // Pad keys do nothing BEFORE Engage Teleop is clicked.
      const padEl = hostEl.querySelector(
        '[data-testid="teleop-velocity-pad"]',
      ) as HTMLDivElement;
      padEl.dispatchEvent(
        new KeyboardEvent("keydown", { key: "w", bubbles: true }),
      );
      flushSync();
      expect(teleopCalls).toHaveLength(0);

      // 2. Click Engage Teleop -> sends initial #84 with engage=true, then hold 'd' on focused pad.
      const engageBtn = hostEl.querySelector(
        '[data-testid="teleop-engage-btn"]',
      ) as HTMLButtonElement;
      engageBtn.click();
      await Promise.resolve();
      flushSync();

      expect(teleopCalls).toHaveLength(1);
      expect(teleopCalls[0]).toMatchObject({
        sysid: 1,
        vx: 0,
        vy: 0,
        engage: true,
      });

      const keyDEvent = new KeyboardEvent("keydown", {
        key: "d",
        bubbles: true,
        cancelable: true,
      });
      padEl.dispatchEvent(keyDEvent);
      await Promise.resolve();
      flushSync();
      expect(keyDEvent.defaultPrevented).toBe(true);
      expect(
        hostEl.querySelector('[data-testid="teleop-axes-readout"]')
          ?.textContent,
      ).toContain("vy: 4.0 m/s");

      // Advance 100ms -> 20 Hz timer emits two more #84 nudges.
      await vi.advanceTimersByTimeAsync(100);
      flushSync();
      const countBeforePreempt = teleopCalls.length;
      expect(countBeforePreempt).toBeGreaterThanOrEqual(4);

      // 3. Inject P1 direct-crsf-override on WS1 -> sticks zero synchronously (<2ms), preemption banner latches,
      // and restoring P2 companion-teleop does NOT resume #84 nudges until explicit re-engage!
      const t0 = performance.now();
      telemetryStore.ingestRawFrame(
        buildTransitionFrame(1, {
          seq: 2,
          kind: "heartbeat_edge",
          authority: "direct-crsf-override",
          companionMode: "hold-position",
          linkTier: "sitl-local",
          timestamp: nowMs + 100,
        }),
      );
      const elapsedMs = performance.now() - t0;
      flushSync();
      expect(elapsedMs).toBeLessThan(2);

      expect(
        hostEl.querySelector('[data-testid="control-preemption-banner"]')
          ?.textContent,
      ).toContain("PREEMPTED");
      expect(
        hostEl.querySelector('[data-testid="teleop-axes-readout"]')
          ?.textContent,
      ).toContain("vy: 0.0 m/s");

      await vi.advanceTimersByTimeAsync(200);
      expect(teleopCalls).toHaveLength(countBeforePreempt);

      // Restore P2 companion-teleop -> preemption banner remains latched and zero #84 packets fire!
      telemetryStore.ingestRawFrame(
        buildTransitionFrame(1, {
          seq: 3,
          kind: "heartbeat_edge",
          authority: "companion-teleop",
          companionMode: "teleop-velocity",
          linkTier: "sitl-local",
          timestamp: nowMs + 300,
        }),
      );
      flushSync();
      await vi.advanceTimersByTimeAsync(200);
      expect(teleopCalls).toHaveLength(countBeforePreempt);
      expect(
        hostEl.querySelector('[data-testid="control-preemption-banner"]'),
      ).not.toBeNull();

      // Explicit re-engage clears preemption banner, and subsequent real-browser pointerdown -> pad blur -> click on Disengage Teleop disengages cleanly!
      engageBtn.click();
      await Promise.resolve();
      flushSync();
      expect(
        (hostEl.querySelector('[data-testid="control-panel"]') as HTMLElement)
          .dataset.engaged,
      ).toBe("true");
      expect(
        hostEl.querySelector('[data-testid="control-preemption-banner"]'),
      ).toBeNull();

      engageBtn.dispatchEvent(new Event("pointerdown", { bubbles: true }));
      padEl.dispatchEvent(new FocusEvent("blur", { relatedTarget: engageBtn }));
      engageBtn.click();
      await Promise.resolve();
      flushSync();
      expect(
        (hostEl.querySelector('[data-testid="control-panel"]') as HTMLElement)
          .dataset.engaged,
      ).toBe("false");
      expect(teleopCalls.at(-1)).toMatchObject({
        sysid: 1,
        vx: 0,
        stop: true,
      });

      // 4. Discrete Hold Position command -> commit_unknown -> zero UI retries!
      const holdBtn = hostEl.querySelector(
        '[data-testid="cmd-hold-position"]',
      ) as HTMLButtonElement;
      holdBtn.click();
      await Promise.resolve();
      flushSync();
      expect(commandCalls).toHaveLength(1);
      expect(commandCalls[0]).toEqual({ sysid: 1, action: "hold-position" });

      skgStore.ingestRawFrame(
        buildCommandEntityFrame({
          id: "boxbotix.local.robotics.uxs.command.s1c1",
          revision: 3,
          sysid: 1,
          cmdId: 17,
          action: "hold-position",
          phase: "commit_unknown",
          issuedMs: nowMs,
          updatedMs: nowMs + 1600,
        }),
      );
      flushSync();

      const statusBadge = hostEl.querySelector(
        '[data-testid="control-command-status"]',
      ) as HTMLElement;
      expect(statusBadge.dataset.phase).toBe("commit_unknown");
      expect(statusBadge.textContent).toContain(COMMIT_UNKNOWN_BANNER_TEXT);
      await vi.advanceTimersByTimeAsync(3000);
      expect(commandCalls).toHaveLength(1);

      // 5. Select non-SITL Rover (#2): SITL lab is absent; transition Rover (#2) to mesh-supervisory -> pad collapses!
      viewportStore.selectEntity(roverMesh.id);
      flushSync();
      expect(
        hostEl.querySelector('[data-testid="sitl-lab-toggle"]'),
      ).toBeNull();

      const roverEngage = hostEl.querySelector(
        '[data-testid="teleop-engage-btn"]',
      ) as HTMLButtonElement;
      roverEngage.click();
      await Promise.resolve();
      flushSync();
      const roverTeleopBeforeCollapse = teleopCalls.length;

      telemetryStore.ingestRawFrame(
        buildTransitionFrame(2, {
          seq: 4,
          kind: "heartbeat_edge",
          authority: "companion-teleop",
          companionMode: "teleop-velocity",
          linkTier: "mesh-supervisory",
          timestamp: nowMs + 500,
        }),
      );
      flushSync();

      expect(
        hostEl.querySelector('[data-testid="control-supervisory-only"]'),
      ).not.toBeNull();
      expect(
        hostEl.querySelector('[data-testid="teleop-velocity-pad"]'),
      ).toBeNull();
      await vi.advanceTimersByTimeAsync(250);
      expect(teleopCalls).toHaveLength(roverTeleopBeforeCollapse);

      // 6. Global #41 workspace hotkeys and #43 Diagnostics draft edits fire ZERO control requests!
      const cmdBeforeHotkeys = commandCalls.length;
      const teleopBeforeHotkeys = teleopCalls.length;
      for (const key of ["g", "d", "b", "k", "l", "h", "Escape"]) {
        handleViewportHotkey({ key }, viewportStore);
      }
      viewportStore.setWorkspace("bench");
      flushSync();

      const stagedView = diagnosticsDraftStore.stageParam(
        skgStore.getCommandableVehicle(roverMesh.id),
        "uxs.limits.max_groundspeed_mps",
        8.5,
      );
      expect(stagedView).not.toBeNull();
      flushSync();

      expect(commandCalls).toHaveLength(cmdBeforeHotkeys);
      expect(teleopCalls).toHaveLength(teleopBeforeHotkeys);

      unmount(app);
      hostEl.remove();
      telemetryStore.disconnect();
      skgStore.disconnect();
    } finally {
      vi.useRealTimers();
    }
  });

  // spec: cop-ui / Guarded Vehicle ControlPanel, LinkTier supervisory collapse, sub-2ms hardware preemption release, and SITL fault lab
  it("matches reference model across 40 seeded interaction histories (engage, hold keys, supervisory collapse, P1/P0 preemption, P2 recovery latch, commit_unknown)", async () => {
    let engagedBoundaries = 0;
    let supervisoryCollapses = 0;
    let crsfPreemptions = 0;
    let failsafePreemptions = 0;
    let p2RecoveryStayedDisengaged = 0;
    let commitUnknownTransitions = 0;
    let focusLossReleases = 0;

    for (let seed = 1; seed <= 40; seed += 1) {
      const rng = createSeededRng(seed * 7919);
      let clockMs = 1_700_000_000_000;
      let perfClockMs = 100;

      const ws1Rig = new MockWebSocketRig();
      const ws2Rig = new MockWebSocketRig();
      const skgStore = createSkgStore({
        url: "ws://127.0.0.1:8082/ws",
        createSocket: ws2Rig.createSocket,
        now: () => clockMs,
      });
      const telemetryStore = createTelemetryStore({
        url: "ws://127.0.0.1:8081/ws",
        createSocket: ws1Rig.createSocket,
        now: () => clockMs,
      });
      telemetryStore.connect();
      skgStore.connect();
      ws1Rig.latest.serverOpen();
      ws2Rig.latest.serverOpen();

      let cmdCounter = 0;
      let kvRev = 1;
      const sentCommands: ControlCommandRequest[] = [];
      const sentTeleops: ControlTeleopRequest[] = [];

      // Manual deterministic timer harness for controller.
      const timerState: { cb: (() => void) | null } = { cb: null };
      const controller = createActuationController({
        telemetryStore,
        skgStore,
        now: () => clockMs,
        perfNow: () => {
          perfClockMs += 0.05;
          return perfClockMs;
        },
        setIntervalFn: (cb) => {
          timerState.cb = cb;
          return 1 as unknown as ReturnType<typeof setInterval>;
        },
        clearIntervalFn: () => {
          timerState.cb = null;
        },
        client: {
          async submitCommand(req) {
            sentCommands.push(req);
            cmdCounter += 1;
            return {
              ok: true,
              result: {
                command_id: `boxbotix.local.robotics.uxs.command.s1c${cmdCounter}`,
                sysid: req.sysid,
                cmd_id: 17,
                action: req.action,
                phase: "pending",
                detail: "Awaiting COMMAND_ACK",
                issued_ms: clockMs,
              },
            };
          },
          async submitTeleop(req) {
            sentTeleops.push(req);
            return {
              ok: true,
              result: {
                sysid: req.sysid,
                lease_active: !req.stop,
                expires_ms: clockMs + 240,
              },
            };
          },
        },
      });

      const initialVehicle = buildVehicleStoredPayload(
        1,
        {
          "uxs.domain.kind": "quadcopter",
          "uxs.authority.state": "companion-teleop",
          "uxs.mode.companion": "teleop-velocity",
          "uxs.link.tier": "sitl-local",
          "uxs.health.armed": true,
        },
        { observedAt: new Date(clockMs).toISOString() },
      );
      skgStore.ingestRawFrame(
        JSON.stringify({
          type: "put",
          bucket: "ENTITY_STATES",
          key: initialVehicle.id,
          revision: kvRev++,
          payload: initialVehicle,
        }),
      );
      skgStore.ingestRawFrame(
        JSON.stringify({
          type: "synced",
          bucket: "ENTITY_STATES",
          revision: kvRev++,
        }),
      );

      // Independent reference model state for vehicle #1:
      let refArmed = true;
      let refAuthority: ControlAuthority = "companion-teleop";
      let refLinkTier: LinkTier = "sitl-local";
      let refEngaged = false;
      let refLatched = false;
      const cmdModel: {
        phase: DiscreteCommandPhase | null;
        id: string | null;
      } = { phase: null, id: null };
      let seq = 1;

      controller.reconcileVehicle(skgStore.getVehicle(initialVehicle.id));

      for (let step = 0; step < 55; step += 1) {
        clockMs += 50;
        const vehicle = skgStore.getVehicle(initialVehicle.id)!;
        const op = Math.floor(rng() * 8);

        if (op === 0) {
          // Attempt Engage Teleop
          const canEngage =
            refArmed &&
            refAuthority === "companion-teleop" &&
            refLinkTier !== "mesh-supervisory";
          const ok = await controller.engageTeleop(vehicle);
          expect(ok).toBe(canEngage);
          if (canEngage) {
            refEngaged = true;
            refLatched = false;
            engagedBoundaries += 1;
          }
        } else if (op === 1) {
          // Hold key W / release W on pad
          const handled = controller.handlePadKeyDown({ key: "w" });
          expect(handled).toBe(refEngaged);
          if (refEngaged) {
            expect(controller.axes.vx).toBeGreaterThan(0);
            controller.handlePadKeyUp({ key: "w" });
            expect(controller.axes.vx).toBe(0);
          } else {
            expect(controller.axes.vx).toBe(0);
          }
        } else if (op === 2) {
          // Timer tick
          const before = sentTeleops.length;
          timerState.cb?.();
          await Promise.resolve();
          if (refEngaged) {
            expect(sentTeleops.length).toBe(before + 1);
          } else {
            expect(sentTeleops.length).toBe(before);
          }
        } else if (op === 3) {
          // P1 CRSF Override or P0 MCU Failsafe transition
          const nextAuth: ControlAuthority =
            rng() < 0.5 ? "direct-crsf-override" : "mcu-failsafe";
          refAuthority = nextAuth;
          refEngaged = false;
          refLatched = true;
          if (cmdModel.phase === "pending") {
            cmdModel.phase = "preempted";
          }
          seq = (seq + 1) % 256;
          telemetryStore.ingestRawFrame(
            buildTransitionFrame(1, {
              seq,
              kind: "heartbeat_edge",
              armed: refArmed,
              authority: refAuthority,
              companionMode: "hold-position",
              linkTier: refLinkTier,
              timestamp: clockMs,
            }),
          );
          if (nextAuth === "direct-crsf-override") {
            crsfPreemptions += 1;
          } else {
            failsafePreemptions += 1;
          }
          expect(controller.lastPreemptLatencyMs).not.toBeNull();
          expect(controller.lastPreemptLatencyMs!).toBeLessThan(2);
        } else if (op === 4) {
          // Restore P2 companion-teleop (must NOT auto-re-engage!)
          const wasLatched = refLatched;
          refAuthority = "companion-teleop";
          refArmed = true;
          seq = (seq + 1) % 256;
          telemetryStore.ingestRawFrame(
            buildTransitionFrame(1, {
              seq,
              kind: "heartbeat_edge",
              armed: true,
              authority: "companion-teleop",
              companionMode: "teleop-velocity",
              linkTier: refLinkTier,
              timestamp: clockMs,
            }),
          );
          if (wasLatched) {
            p2RecoveryStayedDisengaged += 1;
            expect(controller.engaged).toBe(false);
            expect(controller.preemptionLatch).not.toBeNull();
          }
        } else if (op === 5) {
          // Switch LinkTier between sitl-local and mesh-supervisory
          refLinkTier =
            refLinkTier === "mesh-supervisory"
              ? "sitl-local"
              : "mesh-supervisory";
          if (refLinkTier === "mesh-supervisory" && refEngaged) {
            refEngaged = false;
            supervisoryCollapses += 1;
          }
          seq = (seq + 1) % 256;
          telemetryStore.ingestRawFrame(
            buildTransitionFrame(1, {
              seq,
              kind: "heartbeat_edge",
              armed: refArmed,
              authority: refAuthority,
              companionMode: "teleop-velocity",
              linkTier: refLinkTier,
              timestamp: clockMs,
            }),
          );
        } else if (op === 6) {
          // Focus loss / blur
          if (refEngaged) {
            focusLossReleases += 1;
          }
          refEngaged = false;
          controller.handleFocusLoss();
          await Promise.resolve();
        } else {
          // Discrete Hold Position command -> commit_unknown -> confirmed on SKG
          if (refAuthority === "companion-teleop") {
            const cmdCountBefore = sentCommands.length;
            const res = await controller.sendDiscreteCommand(
              vehicle,
              "hold-position",
            );
            expect(res.ok).toBe(true);
            expect(sentCommands.length).toBe(cmdCountBefore + 1);
            refEngaged = false;
            if (res.ok) {
              cmdModel.id = res.result.command_id;
              cmdModel.phase = "commit_unknown";
              skgStore.ingestRawFrame(
                buildCommandEntityFrame({
                  id: cmdModel.id,
                  revision: kvRev++,
                  sysid: 1,
                  cmdId: 17,
                  action: "hold-position",
                  phase: "commit_unknown",
                  issuedMs: clockMs,
                  updatedMs: clockMs + 10,
                }),
              );
              commitUnknownTransitions += 1;
              expect(controller.latestCommand?.phase).toBe("commit_unknown");
              expect(sentCommands.length).toBe(cmdCountBefore + 1);
            }
          }
        }

        // Assert controller invariants match reference model after every step!
        expect(controller.engaged).toBe(refEngaged);
        expect(controller.preemptionLatch !== null).toBe(refLatched);
        expect(controller.axes).toEqual({
          vx: 0,
          vy: 0,
          vz: 0,
          yawRate: 0,
        });
        if (!refEngaged) {
          expect(timerState.cb).toBeNull();
        }
        if (cmdModel.phase !== null) {
          expect(controller.latestCommand?.phase).toBe(cmdModel.phase);
        }
      }

      controller.dispose();
    }

    expect(engagedBoundaries).toBeGreaterThan(20);
    expect(supervisoryCollapses).toBeGreaterThan(5);
    expect(crsfPreemptions).toBeGreaterThan(15);
    expect(failsafePreemptions).toBeGreaterThan(15);
    expect(p2RecoveryStayedDisengaged).toBeGreaterThan(15);
    expect(commitUnknownTransitions).toBeGreaterThan(15);
    expect(focusLossReleases).toBeGreaterThan(5);
  });
});
