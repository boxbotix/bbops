// SPDX-License-Identifier: MIT
// Copyright (c) 2026 boxbotix

import { flushSync, mount, unmount } from "svelte";
import { describe, expect, it, vi } from "vitest";
import { createDiagnosticsDraftStore } from "../bench/tuning-rack";
import ControlPanel from "../components/ControlPanel.svelte";
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
  resolveLiveActuationSafety,
} from "./actuation.svelte";

const realPerfNow = performance.now.bind(performance);

function buildTransitionFrame(
  sysid: number,
  opts: {
    seq?: number;
    kind?:
      "heartbeat_edge" | "failsafe_increment" | "link_lost" | "link_restored";
    armed?: boolean;
    authority?: ControlAuthority;
    authorityIncarnation?: string;
    authorityEpoch?: number;
    observationRev?: number;
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
      ...(opts.authorityIncarnation !== undefined
        ? { authority_incarnation: opts.authorityIncarnation }
        : {}),
      ...(opts.authorityEpoch !== undefined
        ? { authority_epoch: opts.authorityEpoch }
        : {}),
      ...(opts.observationRev !== undefined
        ? { observation_rev: opts.observationRev }
        : {}),
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
      const t0 = realPerfNow();
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
      const elapsedMs = realPerfNow() - t0;
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

  // spec: cop-ui / Guarded Vehicle ControlPanel, LinkTier supervisory collapse, sub-2ms hardware preemption release, and SITL fault lab
  it("measures <2ms preemption with unfaked performance.now() and trips budget check when callback exceeds 2ms", async () => {
    const nowMs = Date.parse("2026-11-24T11:05:00.000Z");
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
    const veh = buildVehicleStoredPayload(
      1,
      {
        "uxs.domain.kind": "quadcopter",
        "uxs.authority.state": "companion-teleop",
        "uxs.mode.companion": "teleop-velocity",
        "uxs.link.tier": "sitl-local",
        "uxs.health.armed": true,
      },
      { observedAt: new Date(nowMs - 100).toISOString() },
    );
    skgStore.ingestRawFrame(
      JSON.stringify({
        type: "put",
        bucket: "ENTITY_STATES",
        key: veh.id,
        revision: 1,
        payload: veh,
      }),
    );
    skgStore.ingestRawFrame(
      JSON.stringify({ type: "synced", bucket: "ENTITY_STATES", revision: 1 }),
    );

    const assertWithinTwoMs = (latencyMs: number | null): void => {
      if (latencyMs === null || latencyMs >= 2) {
        throw new Error(
          `preemption release exceeded 2ms budget: ${String(latencyMs)}ms`,
        );
      }
    };

    const realController = createActuationController({
      telemetryStore,
      skgStore,
      now: () => nowMs,
      perfNow: realPerfNow,
      client: {
        async submitCommand() {
          throw new Error("unused");
        },
        async submitTeleop(req) {
          return {
            ok: true,
            result: {
              sysid: req.sysid,
              lease_active: !req.stop,
              expires_ms: nowMs + 240,
            },
          };
        },
      },
    });

    const engaged = await realController.engageTeleop(
      skgStore.getVehicle(veh.id),
    );
    expect(engaged).toBe(true);
    realController.handlePadKeyDown({ key: "w" });
    expect(realController.axes.vx).toBeGreaterThan(0);

    // Warm one non-preempting ingest so V8 JIT compilation does not skew single-frame wall measurement, then re-verify live CRSF preemption on an unfaked clock.
    const t0 = realPerfNow();
    telemetryStore.ingestRawFrame(
      buildTransitionFrame(1, {
        seq: 10,
        kind: "heartbeat_edge",
        authority: "direct-crsf-override",
        companionMode: "hold-position",
        linkTier: "sitl-local",
        timestamp: nowMs + 50,
      }),
    );
    const wallElapsedMs = realPerfNow() - t0;
    expect(realController.engaged).toBe(false);
    expect(realController.axes.vx).toBe(0);
    expect(wallElapsedMs).toBeLessThan(2);
    assertWithinTwoMs(realController.lastPreemptLatencyMs);
    realController.dispose();

    // Prove the budget check fails when an over-budget stall (>= 2.5 ms) occurs during preemption release.
    let simulatedPerfMs = 50;
    let firstRead = true;
    const stalledController = createActuationController({
      telemetryStore,
      skgStore,
      now: () => nowMs,
      perfNow: () => {
        if (firstRead) {
          firstRead = false;
          return simulatedPerfMs;
        }
        simulatedPerfMs += 2.75;
        return simulatedPerfMs;
      },
    });
    telemetryStore.ingestRawFrame(
      buildTransitionFrame(1, {
        seq: 11,
        kind: "heartbeat_edge",
        authority: "mcu-failsafe",
        companionMode: "hold-position",
        linkTier: "sitl-local",
        timestamp: nowMs + 100,
      }),
    );
    expect(stalledController.lastPreemptLatencyMs).toBeGreaterThanOrEqual(2.75);
    expect(() =>
      assertWithinTwoMs(stalledController.lastPreemptLatencyMs),
    ).toThrow(/exceeded 2ms budget/);
    stalledController.dispose();
    telemetryStore.disconnect();
    skgStore.disconnect();
  });

  // spec: cop-ui / Guarded Vehicle ControlPanel, LinkTier supervisory collapse, sub-2ms hardware preemption release, and SITL fault lab
  it("blocks Engage Teleop and disengages active #84 teleop on stale SKG (feed_stale, observation_timeout, and unchanged reconnect replay), then re-enables without auto-engaging on genuine live recovery", async () => {
    vi.useFakeTimers();
    try {
      let clockMs = Date.parse("2026-11-24T11:10:01.000Z");
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

      const teleopCalls: ControlTeleopRequest[] = [];
      const controlClient: ControlClient = {
        async submitCommand() {
          throw new Error("unused");
        },
        async submitTeleop(req) {
          teleopCalls.push(req);
          return {
            ok: true,
            result: {
              sysid: req.sysid,
              lease_active: !req.stop,
              expires_ms: clockMs + 240,
            },
          };
        },
      };

      const viewportStore = createViewportStore({ skgStore });
      const freshVeh = buildVehicleStoredPayload(
        1,
        {
          "uxs.domain.kind": "quadcopter",
          "uxs.authority.state": "companion-teleop",
          "uxs.mode.companion": "teleop-velocity",
          "uxs.link.tier": "sitl-local",
          "uxs.health.armed": true,
        },
        { observedAt: new Date(clockMs - 100).toISOString() },
      );
      skgStore.ingestRawFrame(
        JSON.stringify({
          type: "put",
          bucket: "ENTITY_STATES",
          key: freshVeh.id,
          revision: 1,
          payload: freshVeh,
        }),
      );
      skgStore.ingestRawFrame(
        JSON.stringify({
          type: "synced",
          bucket: "ENTITY_STATES",
          revision: 1,
        }),
      );
      viewportStore.selectEntity(freshVeh.id);
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
          now: () => clockMs,
        },
      });
      flushSync();

      const engageBtn = () =>
        hostEl.querySelector(
          '[data-testid="teleop-engage-btn"]',
        ) as HTMLButtonElement;
      expect(engageBtn().disabled).toBe(false);

      // Engage #84 teleop while fresh.
      engageBtn().click();
      await Promise.resolve();
      flushSync();
      expect(
        (hostEl.querySelector('[data-testid="control-panel"]') as HTMLElement)
          .dataset.engaged,
      ).toBe("true");
      expect(teleopCalls).toHaveLength(1);

      // 1) Advance clock past observation_timeout (> 5000ms without SKG update): 20 Hz timer disengages and sends stop=true, and HUD shows LAST KNOWN while Engage Teleop is disabled!
      clockMs += 6_000;
      await vi.advanceTimersByTimeAsync(1_050);
      flushSync();
      expect(
        (hostEl.querySelector('[data-testid="control-panel"]') as HTMLElement)
          .dataset.engaged,
      ).toBe("false");
      expect(engageBtn().disabled).toBe(true);
      expect(
        hostEl.querySelector('[data-testid="pilot-freshness-chip"]')
          ?.textContent,
      ).toContain("LAST KNOWN");
      expect(teleopCalls.at(-1)).toMatchObject({ sysid: 1, stop: true });
      const countAfterTimeout = teleopCalls.length;
      await vi.advanceTimersByTimeAsync(200);
      expect(teleopCalls).toHaveLength(countAfterTimeout);

      // 2) Simulate WS #2 disconnect + reconnect replaying the SAME unchanged pre-disconnect snapshot (`stateReplayedUnchanged === true`):
      // Engage Teleop MUST remain disabled despite skgStore.synced === true!
      ws2Rig.latest.serverDrop(1006, "dropped");
      await vi.advanceTimersByTimeAsync(600);
      ws2Rig.latest.serverOpen();
      ws2Rig.latest.serverMessage(
        JSON.stringify({
          type: "put",
          bucket: "ENTITY_STATES",
          key: freshVeh.id,
          revision: 1,
          payload: freshVeh,
        }),
      );
      ws2Rig.latest.serverMessage(
        JSON.stringify({
          type: "synced",
          bucket: "ENTITY_STATES",
          revision: 1,
        }),
      );
      flushSync();

      const replayedVeh = skgStore.getVehicle(freshVeh.id)!;
      expect(replayedVeh.stateReplayedUnchanged).toBe(true);
      const replaySafety = resolveLiveActuationSafety(
        replayedVeh,
        telemetryStore,
        skgStore,
        undefined,
        clockMs,
      );
      expect(replaySafety.fresh).toBe(false);
      expect(replaySafety.canEngageTeleop).toBe(false);
      expect(engageBtn().disabled).toBe(true);

      // 3) Explicit feed_stale triple also keeps Engage Teleop disabled even with fresh timestamp.
      const feedStaleVeh = buildVehicleStoredPayload(
        1,
        {
          "uxs.domain.kind": "quadcopter",
          "uxs.authority.state": "companion-teleop",
          "uxs.mode.companion": "teleop-velocity",
          "uxs.link.tier": "sitl-local",
          "uxs.health.armed": true,
          "cop.feed.status": "stale",
        },
        { observedAt: new Date(clockMs).toISOString() },
      );
      ws2Rig.latest.serverMessage(
        JSON.stringify({
          type: "put",
          bucket: "ENTITY_STATES",
          key: feedStaleVeh.id,
          revision: 2,
          payload: feedStaleVeh,
        }),
      );
      flushSync();
      expect(engageBtn().disabled).toBe(true);

      // 4) Genuine live SKG recovery (`cop.feed.status: live`, new observedAt) re-enables Engage Teleop WITHOUT auto-engaging!
      clockMs += 100;
      const recoveredVeh = buildVehicleStoredPayload(
        1,
        {
          "uxs.domain.kind": "quadcopter",
          "uxs.authority.state": "companion-teleop",
          "uxs.mode.companion": "teleop-velocity",
          "uxs.link.tier": "sitl-local",
          "uxs.health.armed": true,
          "cop.feed.status": "live",
        },
        { observedAt: new Date(clockMs).toISOString() },
      );
      ws2Rig.latest.serverMessage(
        JSON.stringify({
          type: "put",
          bucket: "ENTITY_STATES",
          key: recoveredVeh.id,
          revision: 3,
          payload: recoveredVeh,
        }),
      );
      flushSync();

      expect(engageBtn().disabled).toBe(false);
      expect(
        (hostEl.querySelector('[data-testid="control-panel"]') as HTMLElement)
          .dataset.engaged,
      ).toBe("false");
      await vi.advanceTimersByTimeAsync(200);
      expect(teleopCalls).toHaveLength(countAfterTimeout);

      unmount(app);
      hostEl.remove();
      telemetryStore.disconnect();
      skgStore.disconnect();
    } finally {
      vi.useRealTimers();
    }
  });

  // spec: cop-ui / Guarded Vehicle ControlPanel, LinkTier supervisory collapse, sub-2ms hardware preemption release, and SITL fault lab
  it("coalesces rapid pointer moves to 20 Hz with monotonic session/seq fencing and stops #84 traffic when selected vehicle is poisoned or removed without blur", async () => {
    vi.useFakeTimers();
    try {
      const clockMs = Date.parse("2026-11-24T11:20:01.000Z");
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

      const teleopCalls: ControlTeleopRequest[] = [];
      let holdNextNudgeResolver: (() => void) | null = null;
      let holdNextNudge = false;

      const controlClient: ControlClient = {
        async submitCommand() {
          throw new Error("unused");
        },
        async submitTeleop(req) {
          teleopCalls.push(req);
          if (holdNextNudge && !req.stop && !req.engage) {
            holdNextNudge = false;
            await new Promise<void>((resolve) => {
              holdNextNudgeResolver = resolve;
            });
            return {
              ok: false,
              error: {
                code: "teleop_session_stopped",
                message: "teleop session has already been stopped",
                status: 409,
              },
            };
          }
          return {
            ok: true,
            result: {
              sysid: req.sysid,
              lease_active: !req.stop,
              expires_ms: clockMs + 240,
            },
          };
        },
      };

      const viewportStore = createViewportStore({ skgStore });
      const veh = buildVehicleStoredPayload(
        1,
        {
          "uxs.domain.kind": "quadcopter",
          "uxs.authority.state": "companion-teleop",
          "uxs.mode.companion": "teleop-velocity",
          "uxs.link.tier": "sitl-local",
          "uxs.health.armed": true,
        },
        { observedAt: new Date(clockMs - 100).toISOString() },
      );
      skgStore.ingestRawFrame(
        JSON.stringify({
          type: "put",
          bucket: "ENTITY_STATES",
          key: veh.id,
          revision: 1,
          payload: veh,
        }),
      );
      skgStore.ingestRawFrame(
        JSON.stringify({
          type: "synced",
          bucket: "ENTITY_STATES",
          revision: 1,
        }),
      );
      viewportStore.selectEntity(veh.id);
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
          now: () => clockMs,
        },
      });
      flushSync();

      // 1) Engage teleop -> session S1 minted with seq=1.
      const engageBtn = () =>
        hostEl.querySelector(
          '[data-testid="teleop-engage-btn"]',
        ) as HTMLButtonElement;
      engageBtn().click();
      await Promise.resolve();
      flushSync();
      expect(teleopCalls).toHaveLength(1);
      const session1 = teleopCalls[0]!.session;
      expect(session1).toBeTruthy();
      expect(teleopCalls[0]!.seq).toBe(1);

      // 2) 25 rapid pointerdown/pointermove updates within <50ms coalesce onto a single 20 Hz timer POST!
      const padEl = () =>
        hostEl.querySelector(
          '[data-testid="teleop-velocity-pad"]',
        ) as HTMLDivElement;
      vi.spyOn(padEl(), "getBoundingClientRect").mockReturnValue({
        left: 0,
        top: 0,
        width: 200,
        height: 200,
        right: 200,
        bottom: 200,
        x: 0,
        y: 0,
        toJSON: () => ({}),
      });
      padEl().dispatchEvent(
        new PointerEvent("pointerdown", {
          button: 0,
          pointerId: 7,
          clientX: 110,
          clientY: 90,
          bubbles: true,
        }),
      );
      for (let i = 1; i <= 25; i += 1) {
        padEl().dispatchEvent(
          new PointerEvent("pointermove", {
            pointerId: 7,
            clientX: 100 + i * 3,
            clientY: 100 - i * 2,
            bubbles: true,
          }),
        );
      }
      flushSync();
      // Zero unthrottled pointermove POSTs before the 50ms 20 Hz tick!
      expect(teleopCalls).toHaveLength(1);

      await vi.advanceTimersByTimeAsync(50);
      flushSync();
      expect(teleopCalls).toHaveLength(2);
      expect(teleopCalls[1]).toMatchObject({
        sysid: 1,
        session: session1,
        seq: 2,
      });
      expect(teleopCalls[1]!.vx).toBeGreaterThan(0);
      expect(teleopCalls[1]!.vy).toBeGreaterThan(0);

      // 3) Delay an in-flight nudge (seq=3), disengage (sending stop=true with seq=4), re-engage (new session S2),
      // then resolve the delayed S1 nudge -> S2 remains engaged and uncorrupted!
      holdNextNudge = true;
      await vi.advanceTimersByTimeAsync(50);
      expect(teleopCalls).toHaveLength(3);
      expect(teleopCalls[2]).toMatchObject({ session: session1, seq: 3 });

      engageBtn().dispatchEvent(new Event("pointerdown", { bubbles: true }));
      engageBtn().click();
      await Promise.resolve();
      flushSync();
      expect(teleopCalls[3]).toMatchObject({
        session: session1,
        seq: 4,
        stop: true,
      });

      engageBtn().click();
      await Promise.resolve();
      flushSync();
      const session2 = teleopCalls[4]!.session;
      expect(session2).toBeTruthy();
      expect(session2).not.toBe(session1);
      expect(teleopCalls[4]!.seq).toBe(1);

      holdNextNudgeResolver!();
      await Promise.resolve();
      flushSync();
      expect(
        (hostEl.querySelector('[data-testid="control-panel"]') as HTMLElement)
          .dataset.engaged,
      ).toBe("true");

      // 4) Hold key 'd' on pad, then POISON vehicle #1 in SKG WITHOUT firing any blur event ->
      // ControlPanel unmounts, PilotWorkspace reconciles null, sends stop=true, and stops all 20 Hz #84 POSTs!
      padEl().dispatchEvent(
        new KeyboardEvent("keydown", { key: "d", bubbles: true }),
      );
      await Promise.resolve();
      flushSync();

      skgStore.ingestRawFrame(
        JSON.stringify({
          type: "poisoned",
          bucket: "ENTITY_STATES",
          key: veh.id,
          revision: 2,
          error: "corrupted uxs state",
        }),
      );
      flushSync();
      await Promise.resolve();

      expect(hostEl.querySelector('[data-testid="control-panel"]')).toBeNull();
      expect(
        hostEl.querySelector('[data-testid="pilot-poisoned-guard"]'),
      ).not.toBeNull();
      expect(teleopCalls.at(-1)).toMatchObject({
        sysid: 1,
        session: session2,
        stop: true,
      });
      const countAfterPoison = teleopCalls.length;
      await vi.advanceTimersByTimeAsync(250);
      expect(teleopCalls).toHaveLength(countAfterPoison);

      // 5) Restore valid vehicle #1, engage + hold 'w', then DELETE vehicle #1 from SKG WITHOUT blur ->
      // immediately sends stop=true and emits zero further #84 POSTs!
      skgStore.ingestRawFrame(
        JSON.stringify({
          type: "put",
          bucket: "ENTITY_STATES",
          key: veh.id,
          revision: 3,
          payload: veh,
        }),
      );
      flushSync();
      engageBtn().click();
      await Promise.resolve();
      flushSync();
      padEl().dispatchEvent(
        new KeyboardEvent("keydown", { key: "w", bubbles: true }),
      );
      await Promise.resolve();
      flushSync();

      skgStore.ingestRawFrame(
        JSON.stringify({
          type: "delete",
          bucket: "ENTITY_STATES",
          key: veh.id,
          revision: 4,
        }),
      );
      flushSync();
      await Promise.resolve();

      expect(hostEl.querySelector('[data-testid="control-panel"]')).toBeNull();
      expect(teleopCalls.at(-1)).toMatchObject({
        sysid: 1,
        stop: true,
      });
      const countAfterDelete = teleopCalls.length;
      await vi.advanceTimersByTimeAsync(250);
      expect(teleopCalls).toHaveLength(countAfterDelete);

      unmount(app);
      hostEl.remove();
      telemetryStore.disconnect();
      skgStore.disconnect();
    } finally {
      vi.useRealTimers();
    }
  });

  // spec: cop-ui / Guarded Vehicle ControlPanel, LinkTier supervisory collapse, sub-2ms hardware preemption release, and SITL fault lab
  it("ignores obsolete teleop_seq_superseded HTTP responses in the active session while still disengaging on genuine revocations", async () => {
    vi.useFakeTimers();
    try {
      const nowMs = Date.parse("2026-11-24T11:20:00.000Z");
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

      const veh = buildVehicleStoredPayload(
        1,
        {
          "uxs.domain.kind": "quadcopter",
          "uxs.authority.state": "companion-teleop",
          "uxs.mode.companion": "teleop-velocity",
          "uxs.link.tier": "sitl-local",
          "uxs.health.armed": true,
        },
        { observedAt: new Date(nowMs - 50).toISOString() },
      );
      skgStore.ingestRawFrame(
        JSON.stringify({
          type: "put",
          bucket: "ENTITY_STATES",
          key: veh.id,
          revision: 1,
          payload: veh,
        }),
      );
      skgStore.ingestRawFrame(
        JSON.stringify({
          type: "synced",
          bucket: "ENTITY_STATES",
          revision: 1,
        }),
      );

      const sentTeleops: ControlTeleopRequest[] = [];
      let resolveDelayedSeq2:
        | ((value: Awaited<ReturnType<ControlClient["submitTeleop"]>>) => void)
        | null = null;
      let failNextWithPreemption = false;

      const controller = createActuationController({
        telemetryStore,
        skgStore,
        now: () => nowMs,
        client: {
          async submitCommand() {
            throw new Error("unused");
          },
          async submitTeleop(req) {
            sentTeleops.push(req);
            if (req.seq === 2) {
              return await new Promise((resolve) => {
                resolveDelayedSeq2 = resolve;
              });
            }
            if (failNextWithPreemption && !req.stop) {
              return {
                ok: false,
                error: {
                  code: "preemption_latched",
                  message: "vehicle sysid 1 teleop was preempted",
                  status: 409,
                },
              };
            }
            return {
              ok: true,
              result: {
                sysid: req.sysid,
                lease_active: !req.stop,
                expires_ms: nowMs + 200,
              },
            };
          },
        },
      });

      const engaged = await controller.engageTeleop(
        skgStore.getVehicle(veh.id),
      );
      expect(engaged).toBe(true);
      expect(sentTeleops).toHaveLength(1);
      expect(sentTeleops[0]?.seq).toBe(1);
      const activeSession = sentTeleops[0]?.session;

      // Hold 'w' -> fires immediate seq=2 nudge (delayed in flight).
      controller.handlePadKeyDown({ key: "w" });
      await Promise.resolve();
      expect(sentTeleops.at(-1)?.seq).toBe(2);
      expect(resolveDelayedSeq2).not.toBeNull();

      // Next 50ms timer tick sends seq=3 and succeeds ahead of seq=2!
      await vi.advanceTimersByTimeAsync(50);
      expect(sentTeleops.at(-1)).toMatchObject({
        sysid: 1,
        session: activeSession,
        seq: 3,
      });
      expect(controller.engaged).toBe(true);

      // Delayed seq=2 response arrives out of order with 409 teleop_seq_superseded:
      // MUST NOT disengage, clear axes, or record lastError!
      resolveDelayedSeq2!({
        ok: false,
        error: {
          code: "teleop_seq_superseded",
          message:
            "teleop sequence 2 is older than latest 3 on vehicle sysid 1",
          status: 409,
        },
      });
      await Promise.resolve();
      expect(controller.engaged).toBe(true);
      expect(controller.lastError).toBeNull();
      expect(controller.axes.vx).toBeGreaterThan(0);

      // Next 50ms tick continues streaming seq=4 normally in the same session!
      await vi.advanceTimersByTimeAsync(50);
      expect(sentTeleops.at(-1)).toMatchObject({
        sysid: 1,
        session: activeSession,
        seq: 4,
      });

      // Genuine preemption_latched rejection still disengages and latches immediately!
      failNextWithPreemption = true;
      await vi.advanceTimersByTimeAsync(50);
      expect(controller.engaged).toBe(false);
      expect(controller.axes.vx).toBe(0);
      expect(controller.preemptionLatch).not.toBeNull();
      expect(controller.lastError).toContain("preempted");

      controller.dispose();
      telemetryStore.disconnect();
      skgStore.disconnect();
    } finally {
      vi.useRealTimers();
    }
  });

  // spec: cop-ui / Guarded Vehicle ControlPanel, LinkTier supervisory collapse, sub-2ms hardware preemption release, and SITL fault lab
  it("never emits a second engage=true on slow (>50ms) Engage Teleop responses and stays disengaged when P1 override intervenes before WS1 delivery", async () => {
    vi.useFakeTimers();
    try {
      let nowMs = Date.parse("2026-11-24T11:30:00.000Z");
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

      const veh = buildVehicleStoredPayload(
        1,
        {
          "uxs.domain.kind": "quadcopter",
          "uxs.authority.state": "companion-teleop",
          "uxs.mode.companion": "teleop-velocity",
          "uxs.link.tier": "sitl-local",
          "uxs.health.armed": true,
        },
        { observedAt: new Date(nowMs - 20).toISOString() },
      );
      skgStore.ingestRawFrame(
        JSON.stringify({
          type: "put",
          bucket: "ENTITY_STATES",
          key: veh.id,
          revision: 1,
          payload: veh,
        }),
      );
      skgStore.ingestRawFrame(
        JSON.stringify({
          type: "synced",
          bucket: "ENTITY_STATES",
          revision: 1,
        }),
      );

      // Server-side state mirroring control.Service:
      // 1) Initial engage (seq=1, engage=true) commits lease on server at t+10ms,
      // 2) Brief P1 CRSF override + P2 recovery happens at t+40ms BEFORE the slow HTTP response
      //    returns at t+75ms and BEFORE delayed WS1 transition delivery reaches the browser!
      let serverLeaseSession: string | null = null;
      let serverPreemptLatched = false;
      const serverRevokedSessions = new Set<string>();
      const sentTeleops: ControlTeleopRequest[] = [];

      const controller = createActuationController({
        telemetryStore,
        skgStore,
        now: () => nowMs,
        client: {
          async submitCommand() {
            throw new Error("unused");
          },
          async submitTeleop(req) {
            sentTeleops.push(req);
            if (req.stop) {
              if (req.session) {
                serverRevokedSessions.add(req.session);
              }
              serverLeaseSession = null;
              return {
                ok: true,
                result: { sysid: req.sysid, lease_active: false },
              };
            }
            if (sentTeleops.length === 1 && req.engage) {
              serverLeaseSession = req.session ?? null;
              serverPreemptLatched = false;
              // Simulate P1 override + P2 return on backend at t+30ms while HTTP response returns at t+75ms.
              setTimeout(() => {
                if (serverLeaseSession) {
                  serverRevokedSessions.add(serverLeaseSession);
                }
                serverLeaseSession = null;
                serverPreemptLatched = true;
              }, 30);
              await new Promise<void>((resolve) => {
                setTimeout(resolve, 75);
              });
              return {
                ok: true,
                result: {
                  sysid: req.sysid,
                  lease_active: true,
                  expires_ms: nowMs + 240,
                },
              };
            }
            if (
              (req.session && serverRevokedSessions.has(req.session)) ||
              (serverPreemptLatched && !req.engage)
            ) {
              return {
                ok: false,
                error: {
                  code: "preemption_latched",
                  message:
                    "vehicle sysid 1 teleop was preempted — explicit re-engage required",
                  status: 409,
                },
              };
            }
            if (req.engage) {
              serverPreemptLatched = false;
              serverLeaseSession = req.session ?? null;
            }
            return {
              ok: true,
              result: {
                sysid: req.sysid,
                lease_active: true,
                expires_ms: nowMs + 240,
              },
            };
          },
        },
      });

      const engagePromise = controller.engageTeleop(
        skgStore.getVehicle(veh.id),
      );
      nowMs += 30;
      await vi.advanceTimersByTimeAsync(30);
      nowMs += 20;
      await vi.advanceTimersByTimeAsync(20);
      // At t=50ms while initial engage=true (75ms) is still in-flight, no renewal tick may fire yet!
      expect(sentTeleops).toHaveLength(1);
      nowMs += 25;
      await vi.advanceTimersByTimeAsync(25);
      const firstEngageResult = await engagePromise;
      expect(firstEngageResult).toBe(true);

      // Next 50ms post-acceptance renewal tick (t=125ms) fires WITHOUT engage=true, hits preemption_latched,
      // and immediately disengages + latches PREEMPTED without emitting a second engage=true!
      nowMs += 50;
      await vi.advanceTimersByTimeAsync(50);

      expect(sentTeleops.filter((r) => r.engage === true)).toHaveLength(1);
      expect(sentTeleops).toHaveLength(2);
      expect(sentTeleops[0]?.engage).toBe(true);
      expect(sentTeleops[1]?.engage).toBeUndefined();

      expect(controller.engaged).toBe(false);
      expect(controller.preemptionLatch).not.toBeNull();
      expect(serverLeaseSession).toBeNull();

      // Subsequent timer ticks emit zero teleop packets until the operator explicitly clicks Engage Teleop again.
      const countAfterPreempt = sentTeleops.length;
      nowMs += 200;
      await vi.advanceTimersByTimeAsync(200);
      expect(sentTeleops).toHaveLength(countAfterPreempt);

      // Fresh explicit operator Engage Teleop click mints a brand-new session and succeeds cleanly.
      const secondSessionEngage = await controller.engageTeleop(
        skgStore.getVehicle(veh.id),
      );
      expect(secondSessionEngage).toBe(true);
      expect(serverLeaseSession).not.toBeNull();
      expect(serverLeaseSession).not.toBe(sentTeleops[0]?.session);

      controller.dispose();
      telemetryStore.disconnect();
      skgStore.disconnect();
    } finally {
      vi.useRealTimers();
    }
  });

  // spec: cop-ui / Guarded Vehicle ControlPanel, LinkTier supervisory collapse, sub-2ms hardware preemption release, and SITL fault lab
  it("waits for initial engage=true server acceptance before starting 50ms renewals so >50ms RTT handshake never leaves UI and server out of sync", async () => {
    vi.useFakeTimers();
    try {
      let nowMs = Date.parse("2026-11-27T16:50:00.000Z");
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

      const veh = buildVehicleStoredPayload(
        1,
        {
          "uxs.domain.kind": "quadcopter",
          "uxs.authority.state": "companion-teleop",
          "uxs.mode.companion": "teleop-velocity",
          "uxs.link.tier": "sitl-local",
          "uxs.health.armed": true,
        },
        { observedAt: new Date(nowMs - 20).toISOString() },
      );
      skgStore.ingestRawFrame(
        JSON.stringify({
          type: "put",
          bucket: "ENTITY_STATES",
          key: veh.id,
          revision: 1,
          payload: veh,
        }),
      );
      skgStore.ingestRawFrame(
        JSON.stringify({
          type: "synced",
          bucket: "ENTITY_STATES",
          revision: 1,
        }),
      );

      // Stateful mock server where initial engage=true takes 75ms in flight BEFORE reaching the server!
      let serverLeaseSession: string | null = null;
      const serverStoppedSessions = new Set<string>();
      const sentTeleops: ControlTeleopRequest[] = [];

      const controller = createActuationController({
        telemetryStore,
        skgStore,
        now: () => nowMs,
        client: {
          async submitCommand() {
            throw new Error("unused");
          },
          async submitTeleop(req) {
            sentTeleops.push(req);
            if (req.stop) {
              if (req.session) {
                serverStoppedSessions.add(req.session);
              }
              if (
                serverLeaseSession !== null &&
                (!req.session || serverLeaseSession === req.session)
              ) {
                serverLeaseSession = null;
              }
              return {
                ok: true,
                result: { sysid: req.sysid, lease_active: false },
              };
            }
            if (req.engage) {
              await new Promise<void>((resolve) => {
                setTimeout(resolve, 75);
              });
              if (req.session && serverStoppedSessions.has(req.session)) {
                return {
                  ok: false,
                  error: {
                    code: "teleop_session_stopped",
                    message: `teleop session "${req.session}" has already disengaged`,
                    status: 409,
                  },
                };
              }
              serverLeaseSession = req.session ?? null;
              return {
                ok: true,
                result: {
                  sysid: req.sysid,
                  lease_active: true,
                  expires_ms: nowMs + 250,
                },
              };
            }
            if (serverLeaseSession === null) {
              return {
                ok: false,
                error: {
                  code: "teleop_not_engaged",
                  message: "vehicle sysid 1 has no active teleop lease",
                  status: 409,
                },
              };
            }
            return {
              ok: true,
              result: {
                sysid: req.sysid,
                lease_active: true,
                expires_ms: nowMs + 250,
              },
            };
          },
        },
      });

      // 1) Normal >50ms RTT engage=true: no premature 50ms renewal fires before server acceptance at t=75ms!
      const engagePromise = controller.engageTeleop(
        skgStore.getVehicle(veh.id),
      );
      nowMs += 55;
      await vi.advanceTimersByTimeAsync(55);
      expect(sentTeleops).toHaveLength(1);
      expect(serverLeaseSession).toBeNull();

      nowMs += 20;
      await vi.advanceTimersByTimeAsync(20);
      expect(await engagePromise).toBe(true);
      expect(controller.engaged).toBe(true);
      expect(serverLeaseSession).toBe(sentTeleops[0]?.session);
      expect(controller.lastError).toBeNull();

      // Renewal ticks begin strictly AFTER acceptance (at t=125ms -> seq=2 succeeds cleanly).
      nowMs += 50;
      await vi.advanceTimersByTimeAsync(50);
      expect(sentTeleops).toHaveLength(2);
      expect(sentTeleops[1]).toMatchObject({
        sysid: 1,
        session: sentTeleops[0]?.session,
        seq: 2,
      });
      expect(controller.engaged).toBe(true);
      expect(serverLeaseSession).toBe(sentTeleops[0]?.session);

      // Disengage cleanly before testing mid-flight abort.
      await controller.disengageTeleop({ sendZero: true });
      expect(controller.engaged).toBe(false);
      expect(serverLeaseSession).toBeNull();

      // 2) Mid-flight abort: operator disengages at t+30ms while 75ms engage=true is still in transit!
      const abortedEngage = controller.engageTeleop(
        skgStore.getVehicle(veh.id),
      );
      nowMs += 30;
      await vi.advanceTimersByTimeAsync(30);
      await controller.disengageTeleop({ sendZero: true });

      nowMs += 50;
      await vi.advanceTimersByTimeAsync(50);
      expect(await abortedEngage).toBe(false);
      expect(controller.engaged).toBe(false);
      expect(serverLeaseSession).toBeNull();

      controller.dispose();
      telemetryStore.disconnect();
      skgStore.disconnect();
    } finally {
      vi.useRealTimers();
    }
  });

  // spec: cop-ui / Guarded Vehicle ControlPanel, LinkTier supervisory collapse, sub-2ms hardware preemption release, and SITL fault lab
  it("attaches observed authority_epoch and observed_ms on Engage Teleop so a delayed pre-override request cannot clear preemption after P2 recovery", async () => {
    vi.useFakeTimers();
    try {
      let nowMs = Date.parse("2026-11-27T17:00:00.000Z");
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

      const veh = buildVehicleStoredPayload(
        1,
        {
          "uxs.domain.kind": "quadcopter",
          "uxs.authority.state": "companion-teleop",
          "uxs.mode.companion": "teleop-velocity",
          "uxs.link.tier": "sitl-local",
          "uxs.health.armed": true,
        },
        { observedAt: new Date(nowMs - 20).toISOString() },
      );
      skgStore.ingestRawFrame(
        JSON.stringify({
          type: "put",
          bucket: "ENTITY_STATES",
          key: veh.id,
          revision: 1,
          payload: veh,
        }),
      );
      skgStore.ingestRawFrame(
        JSON.stringify({
          type: "synced",
          bucket: "ENTITY_STATES",
          revision: 1,
        }),
      );
      telemetryStore.ingestRawFrame(
        buildTransitionFrame(1, {
          seq: 1,
          kind: "heartbeat_edge",
          authority: "companion-teleop",
          authorityEpoch: 1,
          linkTier: "sitl-local",
          timestamp: nowMs - 10,
        }),
      );

      let serverPreemptLatched = false;
      let serverRecoveryEpoch = 0;
      let serverLeaseActive = false;
      const sentTeleops: ControlTeleopRequest[] = [];

      const controller = createActuationController({
        telemetryStore,
        skgStore,
        now: () => nowMs,
        client: {
          async submitCommand() {
            throw new Error("unused");
          },
          async submitTeleop(req) {
            sentTeleops.push(req);
            if (req.stop) {
              serverLeaseActive = false;
              return {
                ok: true,
                result: { sysid: req.sysid, lease_active: false },
              };
            }
            if (sentTeleops.length === 1 && req.engage) {
              // Delayed 80ms in network transit BEFORE reaching the server!
              await new Promise<void>((resolve) => {
                setTimeout(resolve, 80);
              });
            }
            if (
              serverPreemptLatched &&
              (!req.engage ||
                !req.authority_epoch ||
                req.authority_epoch < serverRecoveryEpoch)
            ) {
              return {
                ok: false,
                error: {
                  code: "preemption_latched",
                  message:
                    "vehicle sysid 1 teleop was preempted — re-engage requires current post-recovery authority observation",
                  status: 409,
                },
              };
            }
            serverPreemptLatched = false;
            serverLeaseActive = true;
            return {
              ok: true,
              result: {
                sysid: req.sysid,
                lease_active: true,
                expires_ms: nowMs + 250,
              },
            };
          },
        },
      });

      // Operator clicks Engage Teleop at t=0 observing authority_epoch=1; request takes 80ms to reach server.
      const preOverrideEngage = controller.engageTeleop(
        skgStore.getVehicle(veh.id),
      );
      expect(sentTeleops[0]).toMatchObject({
        sysid: 1,
        engage: true,
        authority_epoch: 1,
      });

      // At t=20ms: P1 direct-crsf-override (epoch=2) arrives -> server latches preemption & UI latches PREEMPTED.
      nowMs += 20;
      await vi.advanceTimersByTimeAsync(20);
      serverPreemptLatched = true;
      telemetryStore.ingestRawFrame(
        buildTransitionFrame(1, {
          seq: 2,
          kind: "heartbeat_edge",
          authority: "direct-crsf-override",
          authorityEpoch: 2,
          linkTier: "sitl-local",
          timestamp: nowMs,
        }),
      );

      // At t=40ms: P2 companion-teleop (epoch=3) recovers while pre-override request is STILL in flight!
      nowMs += 20;
      await vi.advanceTimersByTimeAsync(20);
      serverRecoveryEpoch = 3;
      telemetryStore.ingestRawFrame(
        buildTransitionFrame(1, {
          seq: 3,
          kind: "heartbeat_edge",
          authority: "companion-teleop",
          authorityEpoch: 3,
          linkTier: "sitl-local",
          timestamp: nowMs,
        }),
      );

      // At t=80ms: delayed pre-override request (authority_epoch=1 < 3) reaches server -> rejected with preemption_latched!
      nowMs += 40;
      await vi.advanceTimersByTimeAsync(40);
      expect(await preOverrideEngage).toBe(false);
      expect(serverLeaseActive).toBe(false);
      expect(controller.engaged).toBe(false);
      expect(controller.preemptionLatch).not.toBeNull();

      // Operator clicks Engage Teleop AFTER recovery -> carries authority_epoch=3 and succeeds!
      const postRecoveryEngage = await controller.engageTeleop(
        skgStore.getVehicle(veh.id),
      );
      expect(postRecoveryEngage).toBe(true);
      expect(sentTeleops.at(-1)).toMatchObject({
        sysid: 1,
        engage: true,
        authority_epoch: 3,
      });
      expect(serverLeaseActive).toBe(true);
      expect(controller.engaged).toBe(true);
      expect(controller.preemptionLatch).toBeNull();

      controller.dispose();
      telemetryStore.disconnect();
      skgStore.disconnect();
    } finally {
      vi.useRealTimers();
    }
  });

  // spec: cop-ui / Guarded Vehicle ControlPanel, LinkTier supervisory collapse, sub-2ms hardware preemption release, and SITL fault lab
  it("forwards authority_incarnation on Engage Teleop and protects an active post-recovery session when a delayed pre-override request rejects second", async () => {
    vi.useFakeTimers();
    try {
      let nowMs = Date.parse("2026-11-24T11:45:00.000Z");
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

      const veh = buildVehicleStoredPayload(
        1,
        {
          "uxs.domain.kind": "quadcopter",
          "uxs.authority.state": "companion-teleop",
          "uxs.mode.companion": "teleop-velocity",
          "uxs.link.tier": "sitl-local",
          "uxs.health.armed": true,
        },
        { observedAt: new Date(nowMs - 20).toISOString() },
      );
      skgStore.ingestRawFrame(
        JSON.stringify({
          type: "put",
          bucket: "ENTITY_STATES",
          key: veh.id,
          revision: 1,
          payload: veh,
        }),
      );
      skgStore.ingestRawFrame(
        JSON.stringify({
          type: "synced",
          bucket: "ENTITY_STATES",
          revision: 1,
        }),
      );

      // Initial transition event under incarnation "inc-A" at epoch=9.
      telemetryStore.ingestRawFrame(
        buildTransitionFrame(1, {
          seq: 1,
          kind: "heartbeat_edge",
          authority: "companion-teleop",
          authorityIncarnation: "inc-A",
          authorityEpoch: 9,
          linkTier: "sitl-local",
          timestamp: nowMs,
        }),
      );

      const sentTeleops: ControlTeleopRequest[] = [];
      let resolveDelayedStaleEngage:
        | ((value: Awaited<ReturnType<ControlClient["submitTeleop"]>>) => void)
        | null = null;

      const controller = createActuationController({
        telemetryStore,
        skgStore,
        now: () => nowMs,
        client: {
          async submitCommand() {
            throw new Error("unused");
          },
          async submitTeleop(req) {
            sentTeleops.push(req);
            if (req.engage && req.authority_incarnation === "inc-A") {
              return await new Promise((resolve) => {
                resolveDelayedStaleEngage = resolve;
              });
            }
            return {
              ok: true,
              result: {
                sysid: req.sysid,
                lease_active: !req.stop,
                expires_ms: nowMs + 250,
              },
            };
          },
        },
      });

      // 1) Pre-override Engage under inc-A (epoch=9) is delayed in network transit.
      const staleEngagePromise = controller.engageTeleop(
        skgStore.getVehicle(veh.id),
      );
      expect(sentTeleops[0]).toMatchObject({
        sysid: 1,
        engage: true,
        authority_incarnation: "inc-A",
        authority_epoch: 9,
        observed_ms: nowMs,
      });

      // 2) Host restarts onto inc-B: P1 override at epoch=2, then P2 recovery at epoch=3.
      nowMs += 20;
      telemetryStore.ingestRawFrame(
        buildTransitionFrame(1, {
          seq: 2,
          kind: "heartbeat_edge",
          authority: "direct-crsf-override",
          authorityIncarnation: "inc-B",
          authorityEpoch: 2,
          linkTier: "sitl-local",
          timestamp: nowMs,
        }),
      );
      nowMs += 20;
      telemetryStore.ingestRawFrame(
        buildTransitionFrame(1, {
          seq: 3,
          kind: "heartbeat_edge",
          authority: "companion-teleop",
          authorityIncarnation: "inc-B",
          authorityEpoch: 3,
          linkTier: "sitl-local",
          timestamp: nowMs,
        }),
      );

      // 3) Operator clicks Engage Teleop AFTER recovery -> carries inc-B + epoch=3 and succeeds FIRST!
      const freshEngaged = await controller.engageTeleop(
        skgStore.getVehicle(veh.id),
      );
      expect(freshEngaged).toBe(true);
      const freshSession = sentTeleops[1]?.session;
      expect(sentTeleops[1]).toMatchObject({
        sysid: 1,
        engage: true,
        authority_incarnation: "inc-B",
        authority_epoch: 3,
        observed_ms: nowMs,
      });
      expect(controller.engaged).toBe(true);

      // 4) Delayed pre-override inc-A Engage resolves SECOND with 409 preemption_latched:
      // MUST NOT disengage or latch preemption over the active inc-B session!
      resolveDelayedStaleEngage!({
        ok: false,
        error: {
          code: "preemption_latched",
          message:
            'vehicle sysid 1 teleop authority incarnation mismatch ("inc-A" != "inc-B")',
          status: 409,
        },
      });
      expect(await staleEngagePromise).toBe(false);
      expect(controller.engaged).toBe(true);
      expect(controller.preemptionLatch).toBeNull();
      expect(controller.lastError).toBeNull();

      // 5) Active inc-B session continues sending 20 Hz renewal nudges normally!
      await vi.advanceTimersByTimeAsync(50);
      expect(sentTeleops.at(-1)).toMatchObject({
        sysid: 1,
        session: freshSession,
        seq: 2,
      });

      controller.dispose();
      telemetryStore.disconnect();
      skgStore.disconnect();
    } finally {
      vi.useRealTimers();
    }
  });

  // spec: cop-ui / Guarded Vehicle ControlPanel, LinkTier supervisory collapse, sub-2ms hardware preemption release, and SITL fault lab
  it("forwards observation_rev from TransitionEvent onto Engage and avoids bogus RC-override latch on teleop_not_engaged", async () => {
    const nowMs = Date.parse("2026-11-27T18:30:00.000Z");
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

    const veh = buildVehicleStoredPayload(
      1,
      {
        "uxs.domain.kind": "quadcopter",
        "uxs.authority.state": "companion-teleop",
        "uxs.mode.companion": "teleop-velocity",
        "uxs.link.tier": "sitl-local",
        "uxs.health.armed": true,
      },
      { observedAt: new Date(nowMs - 50).toISOString() },
    );
    skgStore.ingestRawFrame(
      JSON.stringify({
        type: "put",
        bucket: "ENTITY_STATES",
        key: veh.id,
        revision: 1,
        payload: veh,
      }),
    );
    skgStore.ingestRawFrame(
      JSON.stringify({ type: "synced", bucket: "ENTITY_STATES", revision: 1 }),
    );

    // Malformed negative observation_rev is rejected without poisoning valid events.
    telemetryStore.ingestRawFrame(
      JSON.stringify({
        type: "data",
        subject: "bbops.telemetry.events.1",
        payload: {
          sysid: 1,
          entity_id: veh.id,
          domain: "quadcopter",
          seq: 1,
          msg_id: 0,
          kind: "heartbeat_edge",
          reasons: ["authority"],
          armed: true,
          authority: "companion-teleop",
          observation_rev: -3,
          companion_mode: "teleop-velocity",
          link_tier: "sitl-local",
          flags: {
            clock_sync_locked: true,
            teleop_deadman_clamped: false,
            crsf_link_healthy: true,
            companion_trunk_healthy: true,
          },
          failsafes: 0,
          timestamp: nowMs,
        },
      }),
    );
    expect(telemetryStore.events).toHaveLength(0);
    expect(telemetryStore.malformedFrameCount).toBe(1);

    telemetryStore.ingestRawFrame(
      buildTransitionFrame(1, {
        seq: 2,
        kind: "heartbeat_edge",
        authority: "companion-teleop",
        authorityIncarnation: "inc-mono",
        authorityEpoch: 5,
        observationRev: 17,
        linkTier: "sitl-local",
        timestamp: nowMs,
      }),
    );
    expect(telemetryStore.events[0]?.observation_rev).toBe(17);

    const sentTeleops: ControlTeleopRequest[] = [];
    let failFirstWithStaleEpoch = true;
    const controller = createActuationController({
      telemetryStore,
      skgStore,
      now: () => nowMs,
      client: {
        async submitCommand() {
          throw new Error("unused");
        },
        async submitTeleop(req) {
          sentTeleops.push(req);
          if (failFirstWithStaleEpoch) {
            failFirstWithStaleEpoch = false;
            return {
              ok: false,
              error: {
                code: "teleop_not_engaged",
                message:
                  "vehicle sysid 1 teleop authority epoch 3 superseded by current epoch 5",
                status: 409,
              },
            };
          }
          return {
            ok: true,
            result: {
              sysid: req.sysid,
              lease_active: !req.stop,
              expires_ms: nowMs + 240,
            },
          };
        },
      },
    });

    // 1) Stale disarm/rearm epoch refusal (teleop_not_engaged) rejects cleanly without setting bogus RC-override banner.
    expect(await controller.engageTeleop(skgStore.getVehicle(veh.id))).toBe(
      false,
    );
    expect(sentTeleops[0]).toMatchObject({
      sysid: 1,
      engage: true,
      observed_ms: nowMs,
      observation_rev: 17,
      authority_incarnation: "inc-mono",
      authority_epoch: 5,
    });
    expect(controller.preemptionLatch).toBeNull();
    expect(controller.lastError).toContain("superseded by current epoch 5");

    // 2) Subsequent Engage succeeds and forwards observation_rev ONLY on the initial Engage request.
    expect(await controller.engageTeleop(skgStore.getVehicle(veh.id))).toBe(
      true,
    );
    expect(sentTeleops[1]?.observation_rev).toBe(17);
    controller.handlePadKeyDown({ key: "w" });
    await Promise.resolve();
    expect(sentTeleops[2]?.observation_rev).toBeUndefined();

    controller.dispose();
    telemetryStore.disconnect();
    skgStore.disconnect();
  });

  // spec: cop-ui / Guarded Vehicle ControlPanel, LinkTier supervisory collapse, sub-2ms hardware preemption release, and SITL fault lab
  it("caps overlapping periodic teleop timer renewals at 2 in flight while keeping interactive pad nudges responsive", async () => {
    vi.useFakeTimers();
    try {
      let nowMs = Date.parse("2026-11-27T20:50:00.000Z");
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

      const veh = buildVehicleStoredPayload(
        1,
        {
          "uxs.domain.kind": "quadcopter",
          "uxs.authority.state": "companion-teleop",
          "uxs.mode.companion": "teleop-velocity",
          "uxs.link.tier": "sitl-local",
          "uxs.health.armed": true,
        },
        { observedAt: new Date(nowMs - 20).toISOString() },
      );
      skgStore.ingestRawFrame(
        JSON.stringify({
          type: "put",
          bucket: "ENTITY_STATES",
          key: veh.id,
          revision: 1,
          payload: veh,
        }),
      );
      skgStore.ingestRawFrame(
        JSON.stringify({
          type: "synced",
          bucket: "ENTITY_STATES",
          revision: 1,
        }),
      );

      const sentTeleops: ControlTeleopRequest[] = [];
      const pendingRenewals: Array<
        (res: Awaited<ReturnType<ControlClient["submitTeleop"]>>) => void
      > = [];

      const controller = createActuationController({
        telemetryStore,
        skgStore,
        now: () => nowMs,
        client: {
          async submitCommand() {
            throw new Error("unused");
          },
          async submitTeleop(req) {
            sentTeleops.push(req);
            if (req.engage) {
              return {
                ok: true,
                result: {
                  sysid: req.sysid,
                  lease_active: true,
                  expires_ms: nowMs + 240,
                },
              };
            }
            return await new Promise((resolve) => {
              pendingRenewals.push(resolve);
            });
          },
        },
      });

      expect(await controller.engageTeleop(skgStore.getVehicle(veh.id))).toBe(
        true,
      );
      expect(sentTeleops).toHaveLength(1);

      // Advance 4 timer ticks (200ms < 240ms lease TTL) while transport responses stall:
      // periodic timer renewals must cap at 2 in-flight requests (not 4).
      for (let i = 0; i < 4; i++) {
        nowMs += 50;
        telemetryStore.ingestRawFrame(
          buildTransitionFrame(1, {
            seq: i + 1,
            kind: "heartbeat_edge",
            authority: "companion-teleop",
            linkTier: "sitl-local",
            timestamp: nowMs,
          }),
        );
        await vi.advanceTimersByTimeAsync(50);
      }
      expect(sentTeleops).toHaveLength(3); // 1 engage + 2 capped periodic renewals
      expect(controller.engaged).toBe(true);

      // Interactive pad keydown still dispatches immediately without waiting on timer slots!
      controller.handlePadKeyDown({ key: "w" });
      await Promise.resolve();
      expect(sentTeleops).toHaveLength(4);
      expect(sentTeleops[3]?.vx).toBeGreaterThan(0);

      // Once 1 periodic renewal resolves (extending conservative deadline to 50 + 240 = 290ms),
      // the next timer tick at 250ms is allowed to send again.
      pendingRenewals.shift()?.({
        ok: true,
        result: { sysid: 1, lease_active: true, expires_ms: nowMs + 240 },
      });
      await Promise.resolve();
      nowMs += 50;
      await vi.advanceTimersByTimeAsync(50);
      expect(sentTeleops).toHaveLength(5);

      controller.dispose();
      telemetryStore.disconnect();
      skgStore.disconnect();
    } finally {
      vi.useRealTimers();
    }
  });

  // spec: cop-ui / Guarded Vehicle ControlPanel, LinkTier supervisory collapse, sub-2ms hardware preemption release, and SITL fault lab
  // spec: uxs-control / Lease-bound at-most-once #84 velocity teleop with LinkTier and authority safety gates
  it("expires displayed engagement on conservative monotonic lease deadline when periodic renewals stall, ignores late responses, and requires fresh Engage", async () => {
    vi.useFakeTimers();
    try {
      let nowMs = Date.parse("2026-11-27T20:55:00.000Z");
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

      const veh = buildVehicleStoredPayload(
        1,
        {
          "uxs.domain.kind": "quadcopter",
          "uxs.authority.state": "companion-teleop",
          "uxs.mode.companion": "teleop-velocity",
          "uxs.link.tier": "sitl-local",
          "uxs.health.armed": true,
        },
        { observedAt: new Date(nowMs - 20).toISOString() },
      );
      skgStore.ingestRawFrame(
        JSON.stringify({
          type: "put",
          bucket: "ENTITY_STATES",
          key: veh.id,
          revision: 1,
          payload: veh,
        }),
      );
      skgStore.ingestRawFrame(
        JSON.stringify({
          type: "synced",
          bucket: "ENTITY_STATES",
          revision: 1,
        }),
      );

      const sentTeleops: ControlTeleopRequest[] = [];
      let stallRenewals = true;
      const stalledResolvers: Array<
        (res: Awaited<ReturnType<ControlClient["submitTeleop"]>>) => void
      > = [];

      const controller = createActuationController({
        telemetryStore,
        skgStore,
        now: () => nowMs,
        client: {
          async submitCommand() {
            throw new Error("unused");
          },
          async submitTeleop(req) {
            sentTeleops.push(req);
            if (req.engage || req.stop || !stallRenewals) {
              return {
                ok: true,
                result: {
                  sysid: req.sysid,
                  lease_active: !req.stop,
                  expires_ms: nowMs + 240,
                },
              };
            }
            return await new Promise((resolve) => {
              stalledResolvers.push(resolve);
            });
          },
        },
      });

      const host = document.createElement("div");
      document.body.appendChild(host);
      const panel = mount(ControlPanel, {
        target: host,
        props: {
          vehicle: skgStore.getVehicle(veh.id)!,
          telemetryStore,
          skgStore,
          controller,
          now: () => nowMs,
        },
      });
      flushSync();
      const getPanelEl = (): HTMLElement =>
        host.querySelector('[data-testid="control-panel"]')!;
      const getEngageBtn = (): HTMLButtonElement =>
        host.querySelector('[data-testid="teleop-engage-btn"]')!;
      const getPadEl = (): HTMLElement =>
        host.querySelector('[data-testid="teleop-velocity-pad"]')!;

      // Case A: Engage confirmed at 0ms; periodic renewals at 50ms & 100ms stall under the 2-request cap.
      expect(await controller.engageTeleop(skgStore.getVehicle(veh.id))).toBe(
        true,
      );
      flushSync();
      expect(controller.engaged).toBe(true);
      expect(controller.engagedSysId).toBe(1);
      expect(getPanelEl().getAttribute("data-engaged")).toBe("true");
      expect(getEngageBtn().textContent?.trim()).toBe("Disengage Teleop");
      expect(getPadEl().classList.contains("engaged")).toBe(true);

      let hbSeq = 1;
      const stepHealthyMs = async (deltaMs: number): Promise<void> => {
        nowMs += deltaMs;
        hbSeq += 1;
        telemetryStore.ingestRawFrame(
          buildTransitionFrame(1, {
            seq: hbSeq,
            kind: "heartbeat_edge",
            authority: "companion-teleop",
            linkTier: "sitl-local",
            timestamp: nowMs,
          }),
        );
        await vi.advanceTimersByTimeAsync(deltaMs);
        flushSync();
      };

      // Advance to 200ms (4 ticks: renewals sent at 50ms & 100ms, capped at 150ms & 200ms).
      for (let i = 0; i < 4; i++) {
        await stepHealthyMs(50);
      }
      expect(sentTeleops).toHaveLength(3);
      expect(controller.engaged).toBe(true);
      expect(controller.leaseRenewalPending).toBe(false);

      await stepHealthyMs(39); // 239ms -> still within 240ms confirmed engage lease
      expect(controller.engaged).toBe(true);
      expect(controller.engagedSysId).toBe(1);
      expect(controller.leaseRenewalPending).toBe(false);
      expect(getPanelEl().getAttribute("data-engaged")).toBe("true");

      // At 240ms (confirmed deadline from 0ms engage when 50ms/100ms renewals are lost/unacknowledged,
      // with zero intervening 1 Hz vehicle heartbeats), controller.engaged and ControlPanel DOM MUST drop
      // to disengaged immediately via reactive leaseConfirmed while leaseRenewalPending tracks 340ms!
      nowMs += 1;
      await vi.advanceTimersByTimeAsync(1); // 240ms
      flushSync();
      expect(controller.engaged).toBe(false);
      expect(controller.engagedSysId).toBeNull();
      expect(controller.leaseRenewalPending).toBe(true);
      expect(getPanelEl().getAttribute("data-engaged")).toBe("false");
      expect(getEngageBtn().textContent?.trim()).toBe("Engage Teleop");
      expect(getPadEl().classList.contains("engaged")).toBe(false);
      expect(getPadEl().textContent).toContain(
        "Awaiting teleop lease renewal confirmation",
      );
      expect(controller.handlePadKeyDown({ key: "w" })).toBe(false);
      expect(sentTeleops).toHaveLength(3);

      await stepHealthyMs(10); // 250ms
      expect(controller.engaged).toBe(false);
      expect(controller.engagedSysId).toBeNull();
      expect(controller.leaseRenewalPending).toBe(true);
      expect(getPanelEl().getAttribute("data-engaged")).toBe("false");
      expect(sentTeleops).toHaveLength(3);

      await stepHealthyMs(89); // 339ms -> still unconfirmed, pending renewal grace still open
      expect(controller.engaged).toBe(false);
      expect(controller.leaseRenewalPending).toBe(true);

      await stepHealthyMs(1); // 340ms (100ms + 240ms server bound) -> pending session expires!
      expect(controller.engaged).toBe(false);
      expect(controller.engagedSysId).toBeNull();
      expect(controller.leaseRenewalPending).toBe(false);
      expect(getPanelEl().getAttribute("data-engaged")).toBe("false");
      expect(controller.lastError).toMatch(/teleop lease expired/i);
      expect(controller.handlePadKeyDown({ key: "w" })).toBe(false);

      // Remain disengaged through 800ms even while vehicle telemetry stays fresh and healthy.
      for (let i = 0; i < 9; i++) {
        await stepHealthyMs(50); // +450ms => 790ms
      }
      await stepHealthyMs(10); // 800ms
      expect(controller.engaged).toBe(false);
      expect(sentTeleops).toHaveLength(3);

      // Late responses from the stalled 50ms/100ms renewals arriving after lease loss MUST NOT revive engagement!
      while (stalledResolvers.length > 0) {
        stalledResolvers.shift()?.({
          ok: true,
          result: { sysid: 1, lease_active: true, expires_ms: nowMs + 240 },
        });
      }
      await vi.advanceTimersByTimeAsync(0);
      flushSync();
      expect(controller.engaged).toBe(false);
      await stepHealthyMs(100);
      expect(controller.engaged).toBe(false);
      expect(sentTeleops).toHaveLength(3);

      // Case B: Fresh explicit Engage Teleop resumes cleanly; confirm renewals at +50ms and +100ms
      // (confirmed deadline +340ms), then stall renewals at +150ms and +200ms (grace bound +440ms).
      stallRenewals = false;
      expect(await controller.engageTeleop(skgStore.getVehicle(veh.id))).toBe(
        true,
      );
      flushSync();
      expect(controller.engaged).toBe(true);
      await stepHealthyMs(50); // +50ms renewal confirmed -> deadline +290ms
      await stepHealthyMs(50); // +100ms renewal confirmed -> deadline +340ms
      expect(controller.engaged).toBe(true);

      stallRenewals = true;
      await stepHealthyMs(50); // +150ms renewal stalls (#1)
      await stepHealthyMs(50); // +200ms renewal stalls (#2, capped)
      await stepHealthyMs(139); // +339ms -> within +340ms confirmed lease
      expect(controller.engaged).toBe(true);
      expect(controller.leaseRenewalPending).toBe(false);
      await stepHealthyMs(1); // +340ms -> confirmed lease expires immediately; grace runs until +440ms!
      expect(controller.engaged).toBe(false);
      expect(controller.engagedSysId).toBeNull();
      expect(controller.leaseRenewalPending).toBe(true);
      expect(getPanelEl().getAttribute("data-engaged")).toBe("false");
      await stepHealthyMs(100); // +440ms -> grace window from +200ms expires!
      expect(controller.engaged).toBe(false);
      expect(controller.leaseRenewalPending).toBe(false);
      await stepHealthyMs(360); // +800ms -> still disengaged!
      expect(controller.engaged).toBe(false);

      // Late responses from +150ms/+200ms arriving at +800ms still cannot revive engagement without fresh Engage.
      while (stalledResolvers.length > 0) {
        stalledResolvers.shift()?.({
          ok: true,
          result: { sysid: 1, lease_active: true, expires_ms: nowMs + 240 },
        });
      }
      await vi.advanceTimersByTimeAsync(0);
      flushSync();
      expect(controller.engaged).toBe(false);

      // Case C: If a renewal sent at +100ms was accepted by the server and its HTTP response arrives
      // delayed at +260ms (> +240ms initial deadline, < +340ms renewal deadline), engaged and exposed axes
      // drop to false / 0 during [+240ms, +260ms) and restore cleanly with held keys intact at +260ms!
      stallRenewals = false;
      expect(await controller.engageTeleop(skgStore.getVehicle(veh.id))).toBe(
        true,
      );
      flushSync();
      stallRenewals = true;
      expect(controller.handlePadKeyDown({ key: "w" })).toBe(true);
      expect(controller.axes.vx).toBe(4);
      await stepHealthyMs(50); // +50ms stalled (#1)
      await stepHealthyMs(50); // +100ms stalled (#2)
      await stepHealthyMs(150); // +250ms (> +240ms initial, < +340ms in-flight bound)
      expect(controller.engaged).toBe(false);
      expect(controller.engagedSysId).toBeNull();
      expect(controller.leaseRenewalPending).toBe(true);
      expect(controller.axes.vx).toBe(0);
      expect(getPanelEl().getAttribute("data-engaged")).toBe("false");
      expect(getEngageBtn().textContent?.trim()).toBe("Engage Teleop");
      expect(getPadEl().classList.contains("engaged")).toBe(false);

      await stepHealthyMs(10); // +260ms: server-accepted +100ms renewal response arrives!
      stalledResolvers.pop()?.({
        ok: true,
        result: { sysid: 1, lease_active: true, expires_ms: nowMs + 80 },
      });
      stalledResolvers.length = 0;
      await vi.advanceTimersByTimeAsync(0);
      flushSync();
      expect(controller.engaged).toBe(true);
      expect(controller.engagedSysId).toBe(1);
      expect(controller.leaseRenewalPending).toBe(false);
      expect(controller.axes.vx).toBe(4);
      expect(getPanelEl().getAttribute("data-engaged")).toBe("true");
      expect(getEngageBtn().textContent?.trim()).toBe("Disengage Teleop");
      expect(getPadEl().classList.contains("engaged")).toBe(true);

      await stepHealthyMs(79); // +339ms -> still confirmed under +100ms + 240ms = +340ms
      expect(controller.engaged).toBe(true);
      expect(controller.axes.vx).toBe(4);
      expect(getPanelEl().getAttribute("data-engaged")).toBe("true");
      await stepHealthyMs(1); // +340ms (new renewal stalled -> drops engaged & axes to false / 0 at +340ms sharp!)
      expect(controller.engaged).toBe(false);
      expect(controller.axes.vx).toBe(0);
      expect(getPanelEl().getAttribute("data-engaged")).toBe("false");

      unmount(panel);
      host.remove();
      controller.dispose();
      telemetryStore.disconnect();
      skgStore.disconnect();
    } finally {
      vi.useRealTimers();
    }
  });

  // spec: cop-ui / Guarded Vehicle ControlPanel, LinkTier supervisory collapse, sub-2ms hardware preemption release, and SITL fault lab
  // spec: uxs-control / Lease-bound at-most-once #84 velocity teleop with LinkTier and authority safety gates
  it("rejects over-TTL delayed initial engage response and preserves monotonic max deadline across out-of-order renewals", async () => {
    vi.useFakeTimers();
    try {
      let nowMs = Date.parse("2026-11-27T21:00:00.000Z");
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

      const veh = buildVehicleStoredPayload(
        1,
        {
          "uxs.domain.kind": "quadcopter",
          "uxs.authority.state": "companion-teleop",
          "uxs.mode.companion": "teleop-velocity",
          "uxs.link.tier": "sitl-local",
          "uxs.health.armed": true,
        },
        { observedAt: new Date(nowMs - 20).toISOString() },
      );
      skgStore.ingestRawFrame(
        JSON.stringify({
          type: "put",
          bucket: "ENTITY_STATES",
          key: veh.id,
          revision: 1,
          payload: veh,
        }),
      );
      skgStore.ingestRawFrame(
        JSON.stringify({
          type: "synced",
          bucket: "ENTITY_STATES",
          revision: 1,
        }),
      );

      let mode: "slow-engage" | "reorder-renewals" | "stall-all" =
        "slow-engage";
      const seq2Resolvers: Array<
        (res: Awaited<ReturnType<ControlClient["submitTeleop"]>>) => void
      > = [];

      const controller = createActuationController({
        telemetryStore,
        skgStore,
        now: () => nowMs,
        client: {
          async submitCommand() {
            throw new Error("unused");
          },
          async submitTeleop(req) {
            if (mode === "stall-all" && !req.stop) {
              return await new Promise(() => {});
            }
            if (mode === "slow-engage" && req.engage) {
              return await new Promise((resolve) => {
                setTimeout(() => {
                  resolve({
                    ok: true,
                    result: {
                      sysid: req.sysid,
                      lease_active: true,
                      expires_ms: nowMs + 240,
                    },
                  });
                }, 250);
              });
            }
            if (mode === "reorder-renewals" && req.seq === 2) {
              return await new Promise((resolve) => {
                seq2Resolvers.push(resolve);
              });
            }
            return {
              ok: true,
              result: {
                sysid: req.sysid,
                lease_active: !req.stop,
                expires_ms: nowMs + 240,
              },
            };
          },
        },
      });

      // 1) Initial engage=true response delayed 250ms (>= 240ms lease TTL): conservatively rejected on arrival.
      const slowEngage = controller.engageTeleop(skgStore.getVehicle(veh.id));
      expect(controller.engaged).toBe(false);
      nowMs += 250;
      telemetryStore.ingestRawFrame(
        buildTransitionFrame(1, {
          seq: 1,
          kind: "heartbeat_edge",
          authority: "companion-teleop",
          linkTier: "sitl-local",
          timestamp: nowMs,
        }),
      );
      await vi.advanceTimersByTimeAsync(250);
      expect(await slowEngage).toBe(false);
      expect(controller.engaged).toBe(false);

      // 2) Out-of-order renewal: seq=2 sent at +50ms stalls while seq=3 sent at +100ms resolves immediately
      // (deadline = +340ms). Resolving seq=2 late at +160ms (candidate +290ms) must NOT shorten +340ms!
      mode = "reorder-renewals";
      expect(await controller.engageTeleop(skgStore.getVehicle(veh.id))).toBe(
        true,
      );
      nowMs += 50;
      await vi.advanceTimersByTimeAsync(50); // seq=2 sent at +50ms (stalled)
      nowMs += 50;
      await vi.advanceTimersByTimeAsync(50); // seq=3 sent at +100ms (resolved -> deadline +340ms)
      expect(seq2Resolvers).toHaveLength(1);

      // Now stall any subsequent renewals to isolate the +340ms deadline vs +290ms candidate!
      mode = "stall-all";
      nowMs += 60;
      telemetryStore.ingestRawFrame(
        buildTransitionFrame(1, {
          seq: 2,
          kind: "heartbeat_edge",
          authority: "companion-teleop",
          linkTier: "sitl-local",
          timestamp: nowMs,
        }),
      );
      await vi.advanceTimersByTimeAsync(60); // +160ms
      seq2Resolvers[0]!({
        ok: true,
        result: { sysid: 1, lease_active: true, expires_ms: nowMs + 240 },
      });
      await Promise.resolve();

      // At +310ms (past seq=2's +290ms candidate, before seq=3's +340ms deadline), still engaged!
      nowMs += 150;
      telemetryStore.ingestRawFrame(
        buildTransitionFrame(1, {
          seq: 3,
          kind: "heartbeat_edge",
          authority: "companion-teleop",
          linkTier: "sitl-local",
          timestamp: nowMs,
        }),
      );
      await vi.advanceTimersByTimeAsync(150); // +310ms
      expect(controller.engaged).toBe(true);

      controller.dispose();
      telemetryStore.disconnect();
      skgStore.disconnect();
    } finally {
      vi.useRealTimers();
    }
  });

  // spec: cop-ui / Guarded Vehicle ControlPanel, LinkTier supervisory collapse, sub-2ms hardware preemption release, and SITL fault lab
  // spec: uxs-control / Lease-bound at-most-once #84 velocity teleop with LinkTier and authority safety gates
  it("clears held movement keys when focus leaves the pad onto the toggle button and keyup occurs off-pad during renewal grace", async () => {
    vi.useFakeTimers();
    try {
      let nowMs = Date.parse("2026-11-27T21:15:00.000Z");
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

      const veh = buildVehicleStoredPayload(
        1,
        {
          "uxs.domain.kind": "quadcopter",
          "uxs.authority.state": "companion-teleop",
          "uxs.mode.companion": "teleop-velocity",
          "uxs.link.tier": "sitl-local",
          "uxs.health.armed": true,
        },
        { observedAt: new Date(nowMs - 20).toISOString() },
      );
      skgStore.ingestRawFrame(
        JSON.stringify({
          type: "put",
          bucket: "ENTITY_STATES",
          key: veh.id,
          revision: 1,
          payload: veh,
        }),
      );
      skgStore.ingestRawFrame(
        JSON.stringify({
          type: "synced",
          bucket: "ENTITY_STATES",
          revision: 1,
        }),
      );

      let stallRenewals = false;
      const sentTeleops: ControlTeleopRequest[] = [];
      const stalledResolvers: Array<
        (res: Awaited<ReturnType<ControlClient["submitTeleop"]>>) => void
      > = [];

      const client: ControlClient = {
        async submitCommand() {
          throw new Error("unused");
        },
        async submitTeleop(req) {
          sentTeleops.push(req);
          if (stallRenewals && !req.engage && !req.stop) {
            return await new Promise((resolve) => {
              stalledResolvers.push(resolve);
            });
          }
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

      const controller = createActuationController({
        telemetryStore,
        skgStore,
        client,
        now: () => nowMs,
      });

      const host = document.createElement("div");
      document.body.appendChild(host);
      const panel = mount(ControlPanel, {
        target: host,
        props: {
          get vehicle() {
            return skgStore.getVehicle(veh.id)!;
          },
          telemetryStore,
          skgStore,
          client,
          controller,
          now: () => nowMs,
        },
      });
      flushSync();

      const getPadEl = (): HTMLDivElement =>
        host.querySelector('[data-testid="teleop-velocity-pad"]')!;
      const getEngageBtn = (): HTMLButtonElement =>
        host.querySelector('[data-testid="teleop-engage-btn"]')!;

      let hbSeq = 0;
      const stepHealthyMs = async (deltaMs: number): Promise<void> => {
        nowMs += deltaMs;
        hbSeq += 1;
        telemetryStore.ingestRawFrame(
          buildTransitionFrame(1, {
            seq: hbSeq,
            kind: "heartbeat_edge",
            authority: "companion-teleop",
            linkTier: "sitl-local",
            timestamp: nowMs,
          }),
        );
        await vi.advanceTimersByTimeAsync(deltaMs);
        flushSync();
      };

      expect(await controller.engageTeleop(skgStore.getVehicle(veh.id))).toBe(
        true,
      );
      flushSync();
      getPadEl().focus();
      stallRenewals = true;

      // Hold W on the velocity pad -> immediate forward velocity request (vx: 4).
      getPadEl().dispatchEvent(
        new KeyboardEvent("keydown", { key: "w", bubbles: true }),
      );
      flushSync();
      expect(controller.axes.vx).toBe(4);
      expect(sentTeleops.at(-1)?.vx).toBe(4);

      // Advance into unconfirmed renewal grace (+250ms > +240ms initial deadline, < +340ms +100ms renewal bound).
      await stepHealthyMs(50);
      await stepHealthyMs(50);
      await stepHealthyMs(150);
      expect(controller.engaged).toBe(false);
      expect(controller.leaseRenewalPending).toBe(true);
      expect(controller.axes.vx).toBe(0);

      // Move focus from the velocity pad onto the toggle button and release W on the toggle button.
      getPadEl().dispatchEvent(
        new FocusEvent("blur", { relatedTarget: getEngageBtn() }),
      );
      getEngageBtn().focus();
      getEngageBtn().dispatchEvent(
        new KeyboardEvent("keyup", { key: "w", bubbles: true }),
      );
      flushSync();

      // Delayed +100ms renewal succeeds at +260ms; next teleop request MUST send vx: 0 (never stale vx: 4).
      stallRenewals = false;
      const countBeforeRecovery = sentTeleops.length;
      await stepHealthyMs(10);
      stalledResolvers.pop()?.({
        ok: true,
        result: { sysid: 1, lease_active: true, expires_ms: nowMs + 80 },
      });
      stalledResolvers.length = 0;
      await vi.advanceTimersByTimeAsync(0);
      flushSync();

      expect(controller.engaged).toBe(true);
      expect(controller.axes.vx).toBe(0);
      expect(sentTeleops.length).toBeGreaterThan(countBeforeRecovery);
      for (const pkt of sentTeleops.slice(countBeforeRecovery)) {
        expect(pkt.vx).toBe(0);
      }

      unmount(panel);
      host.remove();
      controller.dispose();
      telemetryStore.disconnect();
      skgStore.disconnect();
    } finally {
      vi.useRealTimers();
    }
  });

  // spec: cop-ui / Guarded Vehicle ControlPanel, LinkTier supervisory collapse, sub-2ms hardware preemption release, and SITL fault lab
  // spec: uxs-control / Lease-bound at-most-once #84 velocity teleop with LinkTier and authority safety gates
  it("never opens a new teleop session when a click intended to disengage crosses lease expiry before release", async () => {
    vi.useFakeTimers();
    try {
      let nowMs = Date.parse("2026-11-27T21:30:00.000Z");
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

      const veh = buildVehicleStoredPayload(
        1,
        {
          "uxs.domain.kind": "quadcopter",
          "uxs.authority.state": "companion-teleop",
          "uxs.mode.companion": "teleop-velocity",
          "uxs.link.tier": "sitl-local",
          "uxs.health.armed": true,
        },
        { observedAt: new Date(nowMs - 20).toISOString() },
      );
      skgStore.ingestRawFrame(
        JSON.stringify({
          type: "put",
          bucket: "ENTITY_STATES",
          key: veh.id,
          revision: 1,
          payload: veh,
        }),
      );
      skgStore.ingestRawFrame(
        JSON.stringify({
          type: "synced",
          bucket: "ENTITY_STATES",
          revision: 1,
        }),
      );

      let mode: "drop-renewals" | "stall-renewals" = "drop-renewals";
      const sentTeleops: ControlTeleopRequest[] = [];
      const stalledResolvers: Array<
        (res: Awaited<ReturnType<ControlClient["submitTeleop"]>>) => void
      > = [];

      const client: ControlClient = {
        async submitCommand() {
          throw new Error("unused");
        },
        async submitTeleop(req) {
          sentTeleops.push(req);
          if (!req.engage && !req.stop) {
            if (mode === "drop-renewals") {
              return {
                ok: false,
                error: {
                  code: "teleop_seq_superseded",
                  message: "simulated dropped renewal",
                  status: 409,
                },
              };
            }
            return await new Promise((resolve) => {
              stalledResolvers.push(resolve);
            });
          }
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

      const controller = createActuationController({
        telemetryStore,
        skgStore,
        client,
        now: () => nowMs,
      });

      const host = document.createElement("div");
      document.body.appendChild(host);
      const panel = mount(ControlPanel, {
        target: host,
        props: {
          get vehicle() {
            return skgStore.getVehicle(veh.id)!;
          },
          telemetryStore,
          skgStore,
          client,
          controller,
          now: () => nowMs,
        },
      });
      flushSync();

      const getEngageBtn = (): HTMLButtonElement =>
        host.querySelector('[data-testid="teleop-engage-btn"]')!;

      let hbSeq = 0;
      const stepHealthyMs = async (deltaMs: number): Promise<void> => {
        nowMs += deltaMs;
        hbSeq += 1;
        telemetryStore.ingestRawFrame(
          buildTransitionFrame(1, {
            seq: hbSeq,
            kind: "heartbeat_edge",
            authority: "companion-teleop",
            linkTier: "sitl-local",
            timestamp: nowMs,
          }),
        );
        await vi.advanceTimersByTimeAsync(deltaMs);
        flushSync();
      };

      // Case A: Full lease expiry between pointerdown (239ms) and click release (241ms).
      expect(await controller.engageTeleop(skgStore.getVehicle(veh.id))).toBe(
        true,
      );
      flushSync();
      expect(sentTeleops.filter((r) => r.engage)).toHaveLength(1);

      await stepHealthyMs(239); // 239ms -> still engaged ("Disengage Teleop")
      expect(controller.engaged).toBe(true);
      expect(getEngageBtn().textContent?.trim()).toBe("Disengage Teleop");

      getEngageBtn().dispatchEvent(
        new PointerEvent("pointerdown", { button: 0, bubbles: true }),
      );
      await stepHealthyMs(2); // 241ms -> confirmed lease expired at 240ms
      expect(controller.engaged).toBe(false);

      getEngageBtn().dispatchEvent(
        new PointerEvent("pointerup", { button: 0, bubbles: true }),
      );
      getEngageBtn().click();
      await vi.advanceTimersByTimeAsync(0);
      flushSync();

      expect(controller.engaged).toBe(false);
      expect(sentTeleops.filter((r) => r.engage)).toHaveLength(1);

      // Case B: Confirmed lease crosses into pending renewal grace between pointerdown (239ms) and click (241ms):
      // disengages the pending session (stop: true) and still sends zero new engage requests!
      mode = "stall-renewals";
      expect(await controller.engageTeleop(skgStore.getVehicle(veh.id))).toBe(
        true,
      );
      flushSync();
      expect(sentTeleops.filter((r) => r.engage)).toHaveLength(2);

      await stepHealthyMs(50);
      await stepHealthyMs(50);
      await stepHealthyMs(139); // +239ms -> still engaged ("Disengage Teleop")
      expect(controller.engaged).toBe(true);

      getEngageBtn().dispatchEvent(
        new PointerEvent("pointerdown", { button: 0, bubbles: true }),
      );
      await stepHealthyMs(2); // +241ms -> crossed into leaseRenewalPending
      expect(controller.engaged).toBe(false);
      expect(controller.leaseRenewalPending).toBe(true);

      getEngageBtn().dispatchEvent(
        new PointerEvent("pointerup", { button: 0, bubbles: true }),
      );
      getEngageBtn().click();
      await vi.advanceTimersByTimeAsync(0);
      flushSync();

      expect(controller.engaged).toBe(false);
      expect(controller.leaseRenewalPending).toBe(false);
      expect(sentTeleops.filter((r) => r.engage)).toHaveLength(2);
      expect(sentTeleops.at(-1)?.stop).toBe(true);
      stalledResolvers.length = 0;

      unmount(panel);
      host.remove();
      controller.dispose();
      telemetryStore.disconnect();
      skgStore.disconnect();
    } finally {
      vi.useRealTimers();
    }
  });

  // spec: cop-ui / Guarded Vehicle ControlPanel, LinkTier supervisory collapse, sub-2ms hardware preemption release, and SITL fault lab
  it("sends session-bound Stop when Disengage completes at 484ms after local expiry while server lease is still alive (preserving any replacement lease) and suppresses held Enter/Space repeat re-engagement", async () => {
    vi.useFakeTimers();
    try {
      const baseMs = Date.parse("2026-11-28T01:40:00.000Z");
      vi.setSystemTime(baseMs);
      const ws1Rig = new MockWebSocketRig();
      const ws2Rig = new MockWebSocketRig();
      const skgStore = createSkgStore({
        url: "ws://127.0.0.1:8082/ws",
        createSocket: ws2Rig.createSocket,
        now: () => Date.now(),
      });
      const telemetryStore = createTelemetryStore({
        url: "ws://127.0.0.1:8081/ws",
        createSocket: ws1Rig.createSocket,
        now: () => Date.now(),
      });
      telemetryStore.connect();
      skgStore.connect();
      ws1Rig.latest.serverOpen();
      ws2Rig.latest.serverOpen();

      const veh = buildVehicleStoredPayload(
        1,
        {
          "uxs.domain.kind": "quadcopter",
          "uxs.authority.state": "companion-teleop",
          "uxs.mode.companion": "teleop-velocity",
          "uxs.link.tier": "sitl-local",
          "uxs.health.armed": true,
        },
        { observedAt: new Date(Date.now() - 20).toISOString() },
      );
      skgStore.ingestRawFrame(
        JSON.stringify({
          type: "put",
          bucket: "ENTITY_STATES",
          key: veh.id,
          revision: 1,
          payload: veh,
        }),
      );
      skgStore.ingestRawFrame(
        JSON.stringify({
          type: "synced",
          bucket: "ENTITY_STATES",
          revision: 1,
        }),
      );

      let serverLease = null as {
        session: string;
        expiresMs: number;
        vx: number;
      } | null;
      let delayRenewals = true;
      const sentTeleops: ControlTeleopRequest[] = [];
      const pendingRenewalResolvers: Array<() => void> = [];

      const client: ControlClient = {
        async submitCommand() {
          throw new Error("unused");
        },
        async submitTeleop(req) {
          sentTeleops.push(req);
          const reqSentOffsetMs = Date.now() - baseMs;
          if (req.stop) {
            if (
              serverLease !== null &&
              (!req.session || serverLease.session === req.session)
            ) {
              serverLease = null;
            }
            return {
              ok: true,
              result: { sysid: req.sysid, lease_active: false },
            };
          }
          if (req.engage) {
            serverLease = {
              session: req.session ?? "",
              expiresMs: Date.now() + 240,
              vx: req.vx,
            };
            return {
              ok: true,
              result: {
                sysid: req.sysid,
                lease_active: true,
                expires_ms: Date.now() + 240,
              },
            };
          }
          if (delayRenewals) {
            // Exact timeline:
            // - 0ms nonzero nudge lands on server at 230ms (extending serverLease to 470ms)
            // - 50ms nonzero timer renewal lands on server at 455ms (extending serverLease to 695ms)
            // while both HTTP responses stay pending past 484ms so client deadlines expire first!
            const serverArrivalDelayMs =
              reqSentOffsetMs === 0 ? 230 : reqSentOffsetMs === 50 ? 405 : 230;
            setTimeout(() => {
              const nowAtServer = Date.now();
              if (
                serverLease !== null &&
                serverLease.session === req.session &&
                nowAtServer < serverLease.expiresMs
              ) {
                serverLease = {
                  session: serverLease.session,
                  expiresMs: nowAtServer + 240,
                  vx: req.vx,
                };
              }
            }, serverArrivalDelayMs);
            await new Promise<void>((resolve) => {
              pendingRenewalResolvers.push(resolve);
            });
            return {
              ok: true,
              result: {
                sysid: req.sysid,
                lease_active: serverLease?.session === req.session,
                expires_ms: serverLease?.expiresMs ?? Date.now(),
              },
            };
          }
          if (serverLease !== null && serverLease.session === req.session) {
            serverLease = {
              session: serverLease.session,
              expiresMs: Date.now() + 240,
              vx: req.vx,
            };
          }
          return {
            ok: true,
            result: {
              sysid: req.sysid,
              lease_active: true,
              expires_ms: Date.now() + 240,
            },
          };
        },
      };

      const controller = createActuationController({
        telemetryStore,
        skgStore,
        client,
        now: () => Date.now(),
      });

      const host = document.createElement("div");
      document.body.appendChild(host);
      const panel = mount(ControlPanel, {
        target: host,
        props: {
          get vehicle() {
            return skgStore.getVehicle(veh.id)!;
          },
          telemetryStore,
          skgStore,
          client,
          controller,
          now: () => Date.now(),
        },
      });
      flushSync();

      const getEngageBtn = (): HTMLButtonElement =>
        host.querySelector('[data-testid="teleop-engage-btn"]')!;

      let hbSeq = 0;
      const stepMs = async (deltaMs: number): Promise<void> => {
        await vi.advanceTimersByTimeAsync(deltaMs);
        hbSeq += 1;
        telemetryStore.ingestRawFrame(
          buildTransitionFrame(1, {
            seq: hbSeq,
            kind: "heartbeat_edge",
            authority: "companion-teleop",
            linkTier: "sitl-local",
            timestamp: Date.now(),
          }),
        );
        flushSync();
      };

      // 1) Finding 7: Engage at 0ms, hold forward ('w'), nonzero renewals land on server at 230ms and 455ms
      // (extending serverLease to 695ms), press Disengage at 239ms, advance to 484ms (both local deadlines
      // expired -> controller.engaged === false), release Disengage at 484ms -> sends Stop for session1!
      expect(await controller.engageTeleop(skgStore.getVehicle(veh.id))).toBe(
        true,
      );
      const session1 = controller.engagedSession!;
      expect(session1).toBeTruthy();
      controller.handlePadKeyDown({ key: "w" });
      await Promise.resolve();

      await stepMs(239); // 230ms server renewal already landed; at 239ms client is still engaged!
      expect(controller.engaged).toBe(true);
      expect(getEngageBtn().textContent?.trim()).toBe("Disengage Teleop");

      getEngageBtn().dispatchEvent(
        new PointerEvent("pointerdown", { button: 0, bubbles: true }),
      );

      // Advance to 484ms: 455ms server renewal extended serverLease to 695ms, while local client deadlines
      // expired at 240ms/340ms so controller.engaged & engagedSession are now false/null!
      await stepMs(245); // 239 + 245 = 484ms
      expect(controller.engaged).toBe(false);
      expect(controller.engagedSession).toBeNull();
      expect(serverLease?.session).toBe(session1);
      expect(serverLease!.expiresMs).toBe(baseMs + 695);
      expect(serverLease!.expiresMs).toBeGreaterThan(Date.now());

      getEngageBtn().dispatchEvent(
        new PointerEvent("pointerup", { button: 0, bubbles: true }),
      );
      getEngageBtn().click();
      await vi.advanceTimersByTimeAsync(0);
      flushSync();

      expect(sentTeleops.at(-1)).toMatchObject({
        sysid: 1,
        session: session1,
        stop: true,
        vx: 0,
      });
      expect(serverLease).toBeNull();
      expect(sentTeleops.filter((r) => r.engage)).toHaveLength(1);
      for (const r of pendingRenewalResolvers.splice(0)) {
        r();
      }
      await Promise.resolve();

      // Also verify replacement-session safety: stopping old session1 while session2 is engaged leaves session2 active!
      delayRenewals = false;
      expect(await controller.engageTeleop(skgStore.getVehicle(veh.id))).toBe(
        true,
      );
      const session2 = controller.engagedSession!;
      expect(session2).not.toBe(session1);
      await controller.disengageTeleop({
        sendZero: true,
        sysid: 1,
        session: session1,
      });
      expect(controller.engaged).toBe(true);
      expect(controller.engagedSession).toBe(session2);
      expect(serverLease?.session).toBe(session2);

      // 2) Finding 6A: Holding Enter on Disengage Teleop disengages once and suppresses auto-repeat re-engagement!
      const engagesBeforeHoldEnter = sentTeleops.filter((r) => r.engage).length;
      const stopsBeforeHoldEnter = sentTeleops.filter((r) => r.stop).length;
      const btn = getEngageBtn();
      btn.focus();
      expect(btn.textContent?.trim()).toBe("Disengage Teleop");

      const enterDown1 = new KeyboardEvent("keydown", {
        key: "Enter",
        code: "Enter",
        repeat: false,
        bubbles: true,
        cancelable: true,
      });
      btn.dispatchEvent(enterDown1);
      btn.click(); // Native <button> click on initial Enter keydown
      await vi.advanceTimersByTimeAsync(0);
      flushSync();

      expect(controller.engaged).toBe(false);
      expect(btn.textContent?.trim()).toBe("Engage Teleop");
      expect(sentTeleops.filter((r) => r.stop)).toHaveLength(
        stopsBeforeHoldEnter + 1,
      );

      // Simulate held Enter auto-repeats while button now reads "Engage Teleop":
      for (let i = 0; i < 3; i += 1) {
        const repeatEv = new KeyboardEvent("keydown", {
          key: "Enter",
          code: "Enter",
          repeat: true,
          bubbles: true,
          cancelable: true,
        });
        btn.dispatchEvent(repeatEv);
        expect(repeatEv.defaultPrevented).toBe(true);
        // Even if an environment dispatches a click during held Enter before keyup, buttonKeyConsumed blocks it:
        btn.click();
      }
      await vi.advanceTimersByTimeAsync(0);
      flushSync();

      expect(controller.engaged).toBe(false);
      expect(sentTeleops.filter((r) => r.engage)).toHaveLength(
        engagesBeforeHoldEnter,
      );

      btn.dispatchEvent(
        new KeyboardEvent("keyup", {
          key: "Enter",
          code: "Enter",
          bubbles: true,
          cancelable: true,
        }),
      );

      // 3) Finding 6B: Fresh Enter press engages, then holding Space across repeat keydowns disengages once on keyup without re-engaging!
      const spaceSessionOk = await controller.engageTeleop(
        skgStore.getVehicle(veh.id),
      );
      expect(spaceSessionOk).toBe(true);
      const spaceSession = controller.engagedSession!;
      flushSync();
      const engagesBeforeHoldSpace = sentTeleops.filter((r) => r.engage).length;
      const stopsBeforeHoldSpace = sentTeleops.filter((r) => r.stop).length;

      btn.focus();
      btn.dispatchEvent(
        new KeyboardEvent("keydown", {
          key: " ",
          code: "Space",
          repeat: false,
          bubbles: true,
          cancelable: true,
        }),
      );
      for (let i = 0; i < 3; i += 1) {
        btn.dispatchEvent(
          new KeyboardEvent("keydown", {
            key: " ",
            code: "Space",
            repeat: true,
            bubbles: true,
            cancelable: true,
          }),
        );
      }
      btn.dispatchEvent(
        new KeyboardEvent("keyup", {
          key: " ",
          code: "Space",
          bubbles: true,
          cancelable: true,
        }),
      );
      btn.click(); // Native <button> click on Space keyup
      await vi.advanceTimersByTimeAsync(0);
      flushSync();

      expect(controller.engaged).toBe(false);
      expect(sentTeleops.filter((r) => r.stop)).toHaveLength(
        stopsBeforeHoldSpace + 1,
      );
      expect(sentTeleops.at(-1)).toMatchObject({
        sysid: 1,
        session: spaceSession,
        stop: true,
      });
      expect(sentTeleops.filter((r) => r.engage)).toHaveLength(
        engagesBeforeHoldSpace,
      );

      // 4) Finding 5: Holding Space on Disengage Teleop until the confirmed lease expires locally
      // and then releasing Space (including post-keyup microtask drain before any native click)
      // MUST send Stop for the captured session and zero new Engage requests!
      delayRenewals = true;
      const spaceExpOk = await controller.engageTeleop(
        skgStore.getVehicle(veh.id),
      );
      expect(spaceExpOk).toBe(true);
      const spaceExpSession = controller.engagedSession!;
      flushSync();
      const engagesBeforeExpiredSpace = sentTeleops.filter(
        (r) => r.engage,
      ).length;
      const stopsBeforeExpiredSpace = sentTeleops.filter((r) => r.stop).length;

      btn.focus();
      expect(btn.textContent?.trim()).toBe("Disengage Teleop");
      btn.dispatchEvent(
        new KeyboardEvent("keydown", {
          key: " ",
          code: "Space",
          repeat: false,
          bubbles: true,
          cancelable: true,
        }),
      );

      // Advance past confirmed lease TTL + pending renewal grace with stalled renewals so local engaged becomes false while Space stays held.
      await stepMs(250);
      await stepMs(250);
      expect(controller.engaged).toBe(false);
      expect(btn.textContent?.trim()).toBe("Engage Teleop");

      const spaceUpAfterExpiry = new KeyboardEvent("keyup", {
        key: " ",
        code: "Space",
        bubbles: true,
        cancelable: true,
      });
      btn.dispatchEvent(spaceUpAfterExpiry);
      expect(spaceUpAfterExpiry.defaultPrevented).toBe(true);
      // Drain microtasks first (matching Chromium's post-keyup microtask checkpoint) before advancing macrotasks:
      await Promise.resolve();
      await vi.advanceTimersByTimeAsync(0);
      flushSync();

      expect(controller.engaged).toBe(false);
      expect(sentTeleops.filter((r) => r.engage)).toHaveLength(
        engagesBeforeExpiredSpace,
      );
      expect(sentTeleops.filter((r) => r.stop)).toHaveLength(
        stopsBeforeExpiredSpace + 1,
      );
      expect(sentTeleops.at(-1)).toMatchObject({
        sysid: 1,
        session: spaceExpSession,
        stop: true,
      });
      for (const r of pendingRenewalResolvers.splice(0)) {
        r();
      }
      await Promise.resolve();

      unmount(panel);
      host.remove();
      controller.dispose();
      telemetryStore.disconnect();
      skgStore.disconnect();
    } finally {
      vi.useRealTimers();
    }
  });
});
