// SPDX-License-Identifier: MIT
// Copyright (c) 2026 boxbotix

import { flushSync, mount, unmount } from "svelte";
import { describe, expect, it, vi } from "vitest";
import BenchWorkspace from "../components/BenchWorkspace.svelte";
import { createSkgStore } from "../stores/skg.svelte";
import { createTelemetryStore } from "../stores/telemetry.svelte";
import {
  buildVehicleStoredPayload,
  createSeededRng,
} from "../stores/test-helpers";
import { createViewportStore } from "../stores/viewport.svelte";
import {
  DEFAULT_TRACE_CAPACITY,
  type TelemetrySample,
  TelemetryTraceBook,
  VehicleRingBuffer,
} from "./ring-buffer";
import {
  BENCH_TUNING_SPECS,
  computeTuningDiff,
  defaultTuningBaseline,
  revertTuningDraft,
  stageTuningValue,
} from "./tuning-rack";

describe("bench/ring-buffer.ts & tuning-rack.ts — 20 Hz trace ring & staged tuning seam", () => {
  // spec: cop-ui / Task-modal 4-workspace shell, invariant safety topbar, and Fleet Filmstrip
  it("matches a chronological reference model across wraparound, out-of-order rejection, and resets over seeded 20 Hz histories", () => {
    const capacity = 16;
    const ring = new VehicleRingBuffer(capacity);
    let ref: TelemetrySample[] = [];
    let refDropped = 0;

    let sawWraparound = false;
    let sawOutOfOrderDrop = false;
    let sawClear = false;
    let clock = 1_000;

    const rng = createSeededRng(0xb34c4200);
    for (let step = 0; step < 320; step += 1) {
      const action = rng();
      if (action < 0.06 && ref.length > 0) {
        ring.clear();
        ref = [];
        sawClear = true;
      } else {
        let ts: number;
        if (action < 0.24 && ref.length > 0) {
          // Out-of-order or duplicate timestamp
          ts = ref[ref.length - 1]!.timestamp - Math.floor(rng() * 50);
        } else if (action < 0.27) {
          ts = Number.NaN;
        } else {
          clock += 50;
          ts = clock;
        }

        const opt = (val: number) => (rng() < 0.2 ? null : val);
        const sample: TelemetrySample = {
          timestamp: ts,
          rollRateDegS: opt(Math.round((rng() - 0.5) * 200) + 0),
          pitchRateDegS: opt(Math.round((rng() - 0.5) * 180) + 0),
          yawRateDegS: opt(Math.round((rng() - 0.5) * 120) + 0),
          groundspeedMps: opt(Math.max(0, Math.round(rng() * 15)) + 0),
          climbRateMps: opt(Math.round((rng() - 0.5) * 8) + 0),
          loopDtUs: opt(1000 + Math.floor(rng() * 500)),
          rttUs: opt(800 + Math.floor(rng() * 600)),
        };

        const accepted = ring.push(sample);
        const lastRefTs =
          ref.length > 0 ? ref[ref.length - 1]!.timestamp : -Infinity;
        if (!Number.isFinite(ts) || (ref.length > 0 && ts <= lastRefTs)) {
          expect(accepted).toBe(false);
          refDropped += 1;
          sawOutOfOrderDrop = true;
        } else {
          expect(accepted).toBe(true);
          ref.push(sample);
          if (ref.length > capacity) {
            ref = ref.slice(ref.length - capacity);
            sawWraparound = true;
          }
        }
      }

      expect(ring.size).toBe(ref.length);
      expect(ring.size).toBeLessThanOrEqual(capacity);
      expect(ring.droppedOutOfOrderCount).toBe(refDropped);
      expect(ring.toChronologicalSamples()).toEqual(ref);
    }

    expect(DEFAULT_TRACE_CAPACITY).toBe(300);
    expect(sawWraparound).toBe(true);
    expect(sawOutOfOrderDrop).toBe(true);
    expect(sawClear).toBe(true);
  });

  // spec: cop-ui / Task-modal 4-workspace shell, invariant safety topbar, and Fleet Filmstrip
  it("stages parameter diffs locally without unconfirmed TX on slider drag and withholds tuning on poisoned or external contacts", () => {
    const baseline = defaultTuningBaseline("rover");
    let draft = stageTuningValue(
      BENCH_TUNING_SPECS,
      baseline,
      "uxs.control.heading_kp",
      4.52, // quantized by 0.05 step to 4.5
    );
    draft = stageTuningValue(
      BENCH_TUNING_SPECS,
      draft,
      "uxs.mux.teleop_deadman_ms",
      5000, // clamped to 1000
    );

    const diffs = computeTuningDiff(BENCH_TUNING_SPECS, baseline, draft);
    expect(diffs).toHaveLength(2);
    expect(diffs[0]?.key).toBe("uxs.control.heading_kp");
    expect(diffs[0]?.staged).toBe(4.5);
    expect(diffs[1]?.key).toBe("uxs.mux.teleop_deadman_ms");
    expect(diffs[1]?.staged).toBe(1000);
    expect(
      computeTuningDiff(
        BENCH_TUNING_SPECS,
        baseline,
        revertTuningDraft(baseline),
      ),
    ).toEqual([]);

    const skgStore = createSkgStore();
    const telemetryStore = createTelemetryStore();
    const traceBook = new TelemetryTraceBook();
    const rover = buildVehicleStoredPayload(2, {
      "uxs.domain.kind": "rover",
      "uxs.health.loopdtus": 1120,
      "uxs.health.crcerrors": 3,
    });
    const contactId = "boxbotix.local.tak.cop.vessel.harbor7";
    skgStore.ingestRawFrame(
      JSON.stringify({
        type: "put",
        bucket: "ENTITY_STATES",
        key: rover.id,
        revision: 1,
        payload: rover,
      }),
    );
    skgStore.ingestRawFrame(
      JSON.stringify({
        type: "put",
        bucket: "ENTITY_STATES",
        key: contactId,
        revision: 2,
        payload: {
          id: contactId,
          triples: [
            {
              subject: contactId,
              predicate: "dc.terms.title",
              object: "Harbor 7",
              source: "ais",
              timestamp: "2026-11-24T12:00:00Z",
              confidence: 1,
            },
          ],
        },
      }),
    );
    skgStore.ingestRawFrame(
      JSON.stringify({
        type: "synced",
        bucket: "ENTITY_STATES",
        revision: 2,
      }),
    );

    const viewportStore = createViewportStore({ skgStore });
    viewportStore.selectEntity(rover.id);

    const onApply = vi.fn();
    const hostEl = document.createElement("div");
    document.body.appendChild(hostEl);
    const app = mount(BenchWorkspace, {
      target: hostEl,
      props: {
        telemetryStore,
        skgStore,
        viewportStore,
        traceBook,
        onApplyTuning: onApply,
      },
    });
    flushSync();

    // 1. 0xB1 MCU diagnostics bar renders loop_dt_us and crc_errors in Bench.
    const healthBar = hostEl.querySelector(
      '[data-testid="bench-health-strip"]',
    );
    expect(healthBar?.textContent).toContain("1120 us");
    expect(healthBar?.textContent).toContain("3");

    // 2. Dragging slider stages locally WITHOUT invoking onApplyTuning.
    const slider = hostEl.querySelector(
      '[data-param-key="uxs.control.heading_kp"] input[type="range"]',
    ) as HTMLInputElement;
    expect(slider).not.toBeNull();
    slider.value = "4.2";
    slider.dispatchEvent(new Event("input", { bubbles: true }));
    flushSync();

    expect(
      hostEl.querySelector('[data-testid="bench-staged-count"]')?.textContent,
    ).toContain("1 staged");
    expect(onApply).not.toHaveBeenCalled();

    // 3. Clicking Apply explicitly invokes onApplyTuning once; Revert clears staged diffs.
    const applyBtn = hostEl.querySelector(
      '[data-testid="bench-tuning-apply"]',
    ) as HTMLButtonElement;
    applyBtn.click();
    expect(onApply).toHaveBeenCalledTimes(1);
    expect(onApply.mock.calls[0]?.[0]).toBe(2);

    const revertBtn = hostEl.querySelector(
      '[data-testid="bench-tuning-revert"]',
    ) as HTMLButtonElement;
    revertBtn.click();
    flushSync();
    expect(
      hostEl.querySelector('[data-testid="bench-staged-count"]')?.textContent,
    ).toContain("0 staged");

    // 4. Selecting an external contact withholds the tuning rack completely.
    viewportStore.selectEntity(contactId);
    flushSync();
    expect(
      hostEl.querySelector('[data-testid="bench-tuning-withheld"]'),
    ).not.toBeNull();
    expect(hostEl.querySelectorAll('input[type="range"]')).toHaveLength(0);

    unmount(app);
    hostEl.remove();
  });

  // spec: cop-ui / Task-modal 4-workspace shell, invariant safety topbar, and Fleet Filmstrip
  it("records accepted WS1 kinematics frames at ingress rather than inside rAF, retaining burst samples and recording zero false out-of-order drops across idle paints", () => {
    const rafCallbacks = new Map<number, FrameRequestCallback>();
    let nextRafId = 1;
    const rafSpy = vi
      .spyOn(window, "requestAnimationFrame")
      .mockImplementation((cb: FrameRequestCallback) => {
        const id = nextRafId++;
        rafCallbacks.set(id, cb);
        return id;
      });
    const cafSpy = vi
      .spyOn(window, "cancelAnimationFrame")
      .mockImplementation((id: number) => {
        rafCallbacks.delete(id);
      });

    const strokeSpy = vi.fn();
    const fake2dCtx = {
      save: vi.fn(),
      restore: vi.fn(),
      scale: vi.fn(),
      setTransform: vi.fn(),
      fillRect: vi.fn(),
      strokeRect: vi.fn(),
      beginPath: vi.fn(),
      moveTo: vi.fn(),
      lineTo: vi.fn(),
      stroke: strokeSpy,
      fillText: vi.fn(),
      measureText: () => ({ width: 32 }),
      fillStyle: "",
      strokeStyle: "",
      lineWidth: 1,
      font: "",
      textAlign: "left",
    } as unknown as CanvasRenderingContext2D;
    const getContextSpy = vi
      .spyOn(HTMLCanvasElement.prototype, "getContext")
      .mockImplementation(function (
        this: HTMLCanvasElement,
        contextId: string,
      ) {
        if (
          contextId === "2d" &&
          this.getAttribute("data-testid") === "bench-oscilloscope"
        ) {
          return fake2dCtx;
        }
        return null;
      } as typeof HTMLCanvasElement.prototype.getContext);

    const stepNextPaint = (frameTimeMs: number): void => {
      const pending = [...rafCallbacks.entries()];
      rafCallbacks.clear();
      for (const [, cb] of pending) {
        cb(frameTimeMs);
      }
    };

    const makeWs1Frame = (
      seq: number,
      timestamp: number,
      rollRateRadS: number,
    ) =>
      JSON.stringify({
        type: "data",
        subject: "bbops.telemetry.2",
        payload: {
          sysid: 2,
          domain: "rover",
          seq,
          msg_id: 30,
          quat_wxyz: [1, 0, 0, 0],
          has_attitude: true,
          body_rates: [rollRateRadS, 0.05, -0.02],
          ned_xyz: [4, -2, 0],
          has_local_pos: true,
          ned_vel: [1.5, 0.2, 0],
          lat_lon_alt: [37.7749, -122.4194, 12],
          has_global_pos: true,
          hdg_deg: 90,
          has_heading: true,
          timestamp,
        },
      });

    const skgStore = createSkgStore();
    const telemetryStore = createTelemetryStore({ now: () => 5_000 });
    const traceBook = new TelemetryTraceBook();
    const rover = buildVehicleStoredPayload(2, {
      "uxs.domain.kind": "rover",
      "uxs.health.loopdtus": 1050,
      "uxs.health.rttus": 390,
    });
    skgStore.ingestRawFrame(
      JSON.stringify({
        type: "put",
        bucket: "ENTITY_STATES",
        key: rover.id,
        revision: 1,
        payload: rover,
      }),
    );
    skgStore.ingestRawFrame(
      JSON.stringify({
        type: "synced",
        bucket: "ENTITY_STATES",
        revision: 1,
      }),
    );

    const viewportStore = createViewportStore({ skgStore });
    viewportStore.selectEntity(rover.id);

    const hostEl = document.createElement("div");
    document.body.appendChild(hostEl);

    try {
      const app = mount(BenchWorkspace, {
        target: hostEl,
        props: {
          telemetryStore,
          skgStore,
          viewportStore,
          traceBook,
        },
      });
      flushSync();

      // Deliver TWO accepted WS1 frames before a single rAF paint executes.
      telemetryStore.ingestRawFrame(makeWs1Frame(1, 1_000, 0.25));
      telemetryStore.ingestRawFrame(makeWs1Frame(2, 1_050, 0.5));
      expect(telemetryStore.malformedFrameCount).toBe(0);

      // Run 1 paint frame: both samples are already recorded in order and rendered.
      stepNextPaint(16);
      const bufAfterBurst = traceBook.getBuffer(2);
      expect(bufAfterBurst?.size).toBe(2);
      expect(
        bufAfterBurst?.toChronologicalSamples().map((s) => s.timestamp),
      ).toEqual([1_000, 1_050]);
      expect(bufAfterBurst?.droppedOutOfOrderCount).toBe(0);
      expect(strokeSpy).toHaveBeenCalled();

      // Run 3 additional rAF paints with NO new WS1 arrival: zero duplicate pushes or false out-of-order drops!
      stepNextPaint(32);
      stepNextPaint(48);
      stepNextPaint(64);
      expect(bufAfterBurst?.size).toBe(2);
      expect(bufAfterBurst?.droppedOutOfOrderCount).toBe(0);
      expect(
        bufAfterBurst?.toChronologicalSamples().map((s) => s.timestamp),
      ).toEqual([1_000, 1_050]);

      unmount(app);
    } finally {
      hostEl.remove();
      getContextSpy.mockRestore();
      cafSpy.mockRestore();
      rafSpy.mockRestore();
    }
  });

  // spec: cop-ui / Task-modal 4-workspace shell, invariant safety topbar, and Fleet Filmstrip
  it("distinguishes missing motion and timing measurements from measured stationary zeros across missing -> measured zero -> missing sequences", () => {
    const rafCallbacks = new Map<number, FrameRequestCallback>();
    let nextRafId = 1;
    const rafSpy = vi
      .spyOn(window, "requestAnimationFrame")
      .mockImplementation((cb: FrameRequestCallback) => {
        const id = nextRafId++;
        rafCallbacks.set(id, cb);
        return id;
      });
    const cafSpy = vi
      .spyOn(window, "cancelAnimationFrame")
      .mockImplementation((id: number) => {
        rafCallbacks.delete(id);
      });

    const drawnTexts: string[] = [];
    const lineToCalls: Array<[number, number]> = [];
    const markerRects: Array<[number, number, number, number]> = [];
    const fake2dCtx = {
      save: vi.fn(),
      restore: vi.fn(),
      scale: vi.fn(),
      setTransform: vi.fn(),
      fillRect: vi.fn((x: number, y: number, w: number, h: number) => {
        if (w === 3 && h === 3) {
          markerRects.push([x, y, w, h]);
        }
      }),
      strokeRect: vi.fn(),
      beginPath: vi.fn(),
      moveTo: vi.fn(),
      lineTo: vi.fn((x: number, y: number) => {
        lineToCalls.push([x, y]);
      }),
      stroke: vi.fn(),
      fillText: vi.fn((text: string) => {
        drawnTexts.push(text);
      }),
      measureText: () => ({ width: 32 }),
      fillStyle: "",
      strokeStyle: "",
      lineWidth: 1,
      font: "",
      textAlign: "left",
    } as unknown as CanvasRenderingContext2D;
    const getContextSpy = vi
      .spyOn(HTMLCanvasElement.prototype, "getContext")
      .mockImplementation(function (
        this: HTMLCanvasElement,
        contextId: string,
      ) {
        if (
          contextId === "2d" &&
          this.getAttribute("data-testid") === "bench-oscilloscope"
        ) {
          return fake2dCtx;
        }
        return null;
      } as typeof HTMLCanvasElement.prototype.getContext);

    const stepNextPaint = (frameTimeMs: number): void => {
      drawnTexts.length = 0;
      lineToCalls.length = 0;
      markerRects.length = 0;
      const pending = [...rafCallbacks.entries()];
      rafCallbacks.clear();
      for (const [, cb] of pending) {
        cb(frameTimeMs);
      }
    };

    const skgStore = createSkgStore();
    const telemetryStore = createTelemetryStore({ now: () => 5_000 });
    const traceBook = new TelemetryTraceBook();
    const vehicleId = "boxbotix.local.robotics.uxs.vehicle.1";

    // Seed vehicle #1 initially poisoned (no SKG timing available -> lookupSysStatus returns null),
    // then select it by ID once healthy, then poison again for the trailing missing timing sample.
    skgStore.ingestRawFrame(
      JSON.stringify({
        type: "poisoned",
        bucket: "ENTITY_STATES",
        key: vehicleId,
        revision: 1,
        error: "no timing yet",
      }),
    );
    skgStore.ingestRawFrame(
      JSON.stringify({
        type: "synced",
        bucket: "ENTITY_STATES",
        revision: 1,
      }),
    );

    const viewportStore = createViewportStore({ skgStore });
    const hostEl = document.createElement("div");
    document.body.appendChild(hostEl);

    try {
      const app = mount(BenchWorkspace, {
        target: hostEl,
        props: { telemetryStore, skgStore, viewportStore, traceBook },
      });
      flushSync();

      // Step 1 (Missing): has_attitude=false, has_local_pos=false, and no SKG sysStatus.
      telemetryStore.ingestRawFrame(
        JSON.stringify({
          type: "data",
          subject: "bbops.telemetry.1",
          payload: {
            sysid: 1,
            domain: "quadcopter",
            seq: 1,
            msg_id: 33,
            quat_wxyz: [0, 0, 0, 0],
            has_attitude: false,
            body_rates: [0, 0, 0],
            ned_xyz: [0, 0, 0],
            has_local_pos: false,
            ned_vel: [0, 0, 0],
            lat_lon_alt: [37.7749, -122.4194, 10],
            has_global_pos: true,
            hdg_deg: 0,
            has_heading: false,
            timestamp: 1_000,
          },
        }),
      );

      // Step 2 (Measured stationary zero): SKG arrives with loopDtUs=0 & rttUs=0, followed by WS1 with has_attitude=true & has_local_pos=true at 0.
      const zeroTimingQuad = buildVehicleStoredPayload(1, {
        "uxs.domain.kind": "quadcopter",
        "uxs.health.loopdtus": 0,
        "uxs.health.rttus": 0,
      });
      skgStore.ingestRawFrame(
        JSON.stringify({
          type: "put",
          bucket: "ENTITY_STATES",
          key: vehicleId,
          revision: 2,
          payload: zeroTimingQuad,
        }),
      );
      viewportStore.selectEntity(vehicleId);
      flushSync();

      telemetryStore.ingestRawFrame(
        JSON.stringify({
          type: "data",
          subject: "bbops.telemetry.1",
          payload: {
            sysid: 1,
            domain: "quadcopter",
            seq: 2,
            msg_id: 32,
            quat_wxyz: [1, 0, 0, 0],
            has_attitude: true,
            body_rates: [0, 0, 0],
            ned_xyz: [0, 0, 0],
            has_local_pos: true,
            ned_vel: [0, 0, 0],
            lat_lon_alt: [37.7749, -122.4194, 10],
            has_global_pos: true,
            hdg_deg: 0,
            has_heading: true,
            timestamp: 1_050,
          },
        }),
      );

      stepNextPaint(16);
      // At Step 2, latest sample is genuinely measured 0 across all 7 channels -> legend shows :0.0!
      expect(drawnTexts).toContain("p:0.0");
      expect(drawnTexts).toContain("GS:0.0");
      expect(drawnTexts).toContain("loop_dt:0.0");
      expect(drawnTexts).toContain("rtt:0.0");

      // Step 3 (Missing again): clear SKG vehicle via delete (re-selecting after put or inspecting buffer),
      // or record directly on traceBook with sysStatus=null and both flags false, then restore selection to render!
      traceBook.recordKinematics(
        {
          sysid: 1,
          domain: "quadcopter",
          seq: 3,
          msg_id: 33,
          quat_wxyz: [0, 0, 0, 0],
          has_attitude: false,
          body_rates: [0, 0, 0],
          ned_xyz: [0, 0, 0],
          has_local_pos: false,
          ned_vel: [0, 0, 0],
          lat_lon_alt: [37.7749, -122.4194, 10],
          has_global_pos: true,
          hdg_deg: 0,
          has_heading: false,
          timestamp: 1_100,
          receivedAtMs: 5_100,
        },
        null,
      );

      const samples = traceBook.getBuffer(1)?.toChronologicalSamples() ?? [];
      expect(samples).toEqual([
        {
          timestamp: 1_000,
          rollRateDegS: null,
          pitchRateDegS: null,
          yawRateDegS: null,
          groundspeedMps: null,
          climbRateMps: null,
          loopDtUs: null,
          rttUs: null,
        },
        {
          timestamp: 1_050,
          rollRateDegS: 0,
          pitchRateDegS: 0,
          yawRateDegS: 0,
          groundspeedMps: 0,
          climbRateMps: 0,
          loopDtUs: 0,
          rttUs: 0,
        },
        {
          timestamp: 1_100,
          rollRateDegS: null,
          pitchRateDegS: null,
          yawRateDegS: null,
          groundspeedMps: null,
          climbRateMps: null,
          loopDtUs: null,
          rttUs: null,
        },
      ]);

      stepNextPaint(32);
      // Latest sample (1_100) is missing across all 7 channels -> legend transitions back to :-- (NOT :0.0),
      // only the 2 zero-reference axis lines call lineTo (zero data-bridging lineTo across null gaps),
      // and all 7 channels render isolated 3x3 markers at the measured middle sample!
      expect(drawnTexts).toContain("p:--");
      expect(drawnTexts).toContain("q:--");
      expect(drawnTexts).toContain("r:--");
      expect(drawnTexts).toContain("GS:--");
      expect(drawnTexts).toContain("VS:--");
      expect(drawnTexts).toContain("loop_dt:--");
      expect(drawnTexts).toContain("rtt:--");
      expect(drawnTexts).not.toContain("p:0.0");
      expect(drawnTexts).not.toContain("GS:0.0");
      expect(drawnTexts).not.toContain("loop_dt:0.0");
      expect(lineToCalls).toHaveLength(2); // strictly the CH1 & CH2 symmetricZero axis lines
      expect(markerRects).toHaveLength(7); // 1 isolated middle sample marker per channel

      unmount(app);
    } finally {
      hostEl.remove();
      getContextSpy.mockRestore();
      cafSpy.mockRestore();
      rafSpy.mockRestore();
    }
  });
});
