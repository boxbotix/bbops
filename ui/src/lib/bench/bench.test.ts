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
import type { UxsDomainKind } from "../uxs";
import {
  BENCH_TUNING_SPECS,
  type TuningBaselineSource,
  computeTuningDiff,
  createDiagnosticsDraftStore,
  defaultTuningBaseline,
  describeTuningProvenance,
  formatTuningBounds,
  revertTuningDraft,
  stageTuningValue,
  validateTuningNumericInput,
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

    const fetchSpy = vi.fn();
    vi.stubGlobal("fetch", fetchSpy);
    const hostEl = document.createElement("div");
    document.body.appendChild(hostEl);
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

    try {
      // 1. 0xB1 MCU diagnostics bar renders loop_dt_us and crc_errors in Bench.
      const healthBar = hostEl.querySelector(
        '[data-testid="bench-health-strip"]',
      );
      expect(healthBar?.textContent).toContain("1120 us");
      expect(healthBar?.textContent).toContain("3");

      // 2. First view (0 staged) visibly identifies sample domain defaults & plain-language local-draft notice.
      expect(
        hostEl.querySelector('[data-testid="bench-baseline-source-badge"]')
          ?.textContent,
      ).toContain("SAMPLE DOMAIN DEFAULTS");
      expect(
        hostEl.querySelector('[data-testid="bench-provenance-headline"]')
          ?.textContent,
      ).toContain("sample rover domain defaults");
      expect(
        hostEl.querySelector('[data-testid="bench-local-draft-notice"]')
          ?.textContent,
      ).toContain("Edits create a local draft");
      expect(hostEl.textContent).not.toContain("ADR-111");

      // 3. Dragging slider stages locally, updates target-vehicle summary, and keeps Apply disabled with nearby explanation.
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
      expect(
        hostEl.querySelector('[data-testid="bench-change-summary"]')
          ?.textContent,
      ).toContain("TARGET UXS #2 (rover)");
      expect(
        hostEl.querySelector('[data-testid="bench-change-summary-list"]')
          ?.textContent,
      ).toContain("2.10 → 4.20 1/s");

      const applyBtn = hostEl.querySelector(
        '[data-testid="bench-tuning-apply"]',
      ) as HTMLButtonElement;
      expect(applyBtn.disabled).toBe(true);
      applyBtn.click();
      flushSync();
      expect(
        hostEl.querySelector('[data-testid="bench-apply-unavailable-note"]')
          ?.textContent,
      ).toContain("Applying parameters to the vehicle is unavailable");
      expect(fetchSpy).not.toHaveBeenCalled();

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
      expect(hostEl.querySelectorAll('input[type="number"]')).toHaveLength(0);
    } finally {
      vi.unstubAllGlobals();
      unmount(app);
      hostEl.remove();
    }
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

    // Step 1 (HEARTBEAT before #1 SYS_STATUS): Go NewVehicleSnapshot emits loopdtus/maxloopdtus/rttus = 0 with confidence=0 -> hasSysStatus=false.
    const birthBeforeSysStatus = buildVehicleStoredPayload(
      1,
      { "uxs.domain.kind": "quadcopter" },
      { hasSysStatus: false },
    );
    skgStore.ingestRawFrame(
      JSON.stringify({
        type: "put",
        bucket: "ENTITY_STATES",
        key: vehicleId,
        revision: 1,
        payload: birthBeforeSysStatus,
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
    viewportStore.selectEntity(vehicleId);
    const hostEl = document.createElement("div");
    document.body.appendChild(hostEl);

    try {
      const app = mount(BenchWorkspace, {
        target: hostEl,
        props: { telemetryStore, skgStore, viewportStore, traceBook },
      });
      flushSync();

      // Diagnostic chips show "--" before #1 SYS_STATUS has been observed (NOT "0 us").
      const chipText = (key: string): string | undefined =>
        hostEl.querySelector(
          `[data-testid="bench-health-strip"] [data-chip-key="${key}"] strong`,
        )?.textContent ?? undefined;
      expect(chipText("loop_dt_us")).toBe("--");
      expect(chipText("max_loop_dt_us")).toBe("--");
      expect(chipText("rtt_us")).toBe("--");

      // WS1 arrival while has_attitude=false, has_local_pos=false, and hasSysStatus=false -> all 7 channels record null!
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

      // Step 2 (Genuinely measured stationary zero): #1 SYS_STATUS arrives with measured 0/0/0 (confidence=1, context="measured").
      const zeroTimingQuad = buildVehicleStoredPayload(
        1,
        {
          "uxs.domain.kind": "quadcopter",
          "uxs.health.loopdtus": 0,
          "uxs.health.maxloopdtus": 0,
          "uxs.health.rttus": 0,
        },
        { hasSysStatus: true },
      );
      skgStore.ingestRawFrame(
        JSON.stringify({
          type: "put",
          bucket: "ENTITY_STATES",
          key: vehicleId,
          revision: 2,
          payload: zeroTimingQuad,
        }),
      );
      flushSync();
      expect(chipText("loop_dt_us")).toBe("0 us");
      expect(chipText("max_loop_dt_us")).toBe("0 us");
      expect(chipText("rtt_us")).toBe("0 us");

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

      // Step 3 (Poisoned timing source + unmeasured WS1 motion): SKG marks vehicle #1 poisoned (retaining prior vehicle fields in skgStore.vehicles),
      // and WS1 delivers seq=3 with has_attitude=false, has_local_pos=false -> ingress excludes poisoned timing and records null across all 7 channels!
      skgStore.ingestRawFrame(
        JSON.stringify({
          type: "poisoned",
          bucket: "ENTITY_STATES",
          key: vehicleId,
          revision: 3,
          error: "corrupt entity payload",
        }),
      );
      flushSync();
      expect(
        hostEl.querySelector('[data-testid="bench-poisoned-notice"]'),
      ).not.toBeNull();

      telemetryStore.ingestRawFrame(
        JSON.stringify({
          type: "data",
          subject: "bbops.telemetry.1",
          payload: {
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
          },
        }),
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

  // spec: cop-ui / Task-modal 4-workspace shell, invariant safety topbar, and Fleet Filmstrip
  it("excludes poisoned timing sources across healthy -> poisoned -> recovered store transitions instead of recording stale nonzero MCU timing", () => {
    const skgStore = createSkgStore();
    const telemetryStore = createTelemetryStore({ now: () => 5_000 });
    const traceBook = new TelemetryTraceBook();
    const vehicleId = "boxbotix.local.robotics.uxs.vehicle.1";

    const pushWs1 = (seq: number, timestamp: number): void => {
      telemetryStore.ingestRawFrame(
        JSON.stringify({
          type: "data",
          subject: "bbops.telemetry.1",
          payload: {
            sysid: 1,
            domain: "quadcopter",
            seq,
            msg_id: 30,
            quat_wxyz: [1, 0, 0, 0],
            has_attitude: true,
            body_rates: [0.1, 0, 0],
            ned_xyz: [1, 2, -5],
            has_local_pos: true,
            ned_vel: [1, 0, 0],
            lat_lon_alt: [37.7749, -122.4194, 15],
            has_global_pos: true,
            hdg_deg: 90,
            has_heading: true,
            timestamp,
          },
        }),
      );
    };

    // 1. Healthy vehicle (2500 us / 420 us).
    const healthy = buildVehicleStoredPayload(
      1,
      {
        "uxs.health.loopdtus": 2500,
        "uxs.health.maxloopdtus": 3100,
        "uxs.health.rttus": 420,
      },
      { hasSysStatus: true },
    );
    skgStore.ingestRawFrame(
      JSON.stringify({
        type: "put",
        bucket: "ENTITY_STATES",
        key: vehicleId,
        revision: 1,
        payload: healthy,
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
    viewportStore.selectEntity(vehicleId);
    const hostEl = document.createElement("div");
    document.body.appendChild(hostEl);

    try {
      const app = mount(BenchWorkspace, {
        target: hostEl,
        props: { telemetryStore, skgStore, viewportStore, traceBook },
      });
      flushSync();

      pushWs1(1, 2_000);
      expect(
        hostEl.querySelector(
          '[data-testid="bench-health-strip"] [data-chip-key="loop_dt_us"] strong',
        )?.textContent,
      ).toBe("2500 us");

      // 2. Vehicle becomes poisoned: skgStore.vehicles still holds stale 2500/420 on the poisoned entry,
      //    but Bench health strip switches to poisoned notice and WS1 ingress records null timing!
      skgStore.ingestRawFrame(
        JSON.stringify({
          type: "poisoned",
          bucket: "ENTITY_STATES",
          key: vehicleId,
          revision: 2,
          error: "corrupt kv payload",
        }),
      );
      flushSync();
      expect(skgStore.vehicles[0]?.poisoned).toBe(true);
      expect(skgStore.vehicles[0]?.sysStatus.loopDtUs).toBe(2500);
      expect(
        hostEl.querySelector('[data-testid="bench-poisoned-notice"]'),
      ).not.toBeNull();
      expect(
        hostEl.querySelector(
          '[data-testid="bench-health-strip"] [data-chip-key="loop_dt_us"]',
        ),
      ).toBeNull();

      pushWs1(2, 2_050);

      // 3. Vehicle recovers with fresh timing (1800 us / 310 us): diagnostics & trace recording resume!
      const recovered = buildVehicleStoredPayload(
        1,
        {
          "uxs.health.loopdtus": 1800,
          "uxs.health.maxloopdtus": 2200,
          "uxs.health.rttus": 310,
        },
        { hasSysStatus: true },
      );
      skgStore.ingestRawFrame(
        JSON.stringify({
          type: "put",
          bucket: "ENTITY_STATES",
          key: vehicleId,
          revision: 3,
          payload: recovered,
        }),
      );
      flushSync();
      expect(
        hostEl.querySelector('[data-testid="bench-poisoned-notice"]'),
      ).toBeNull();
      expect(
        hostEl.querySelector(
          '[data-testid="bench-health-strip"] [data-chip-key="loop_dt_us"] strong',
        )?.textContent,
      ).toBe("1800 us");
      expect(
        hostEl.querySelector(
          '[data-testid="bench-health-strip"] [data-chip-key="rtt_us"] strong',
        )?.textContent,
      ).toBe("310 us");

      pushWs1(3, 2_100);

      const timingHistory = (
        traceBook.getBuffer(1)?.toChronologicalSamples() ?? []
      ).map((s) => ({
        timestamp: s.timestamp,
        loopDtUs: s.loopDtUs,
        rttUs: s.rttUs,
      }));
      expect(timingHistory).toEqual([
        { timestamp: 2_000, loopDtUs: 2500, rttUs: 420 },
        { timestamp: 2_050, loopDtUs: null, rttUs: null },
        { timestamp: 2_100, loopDtUs: 1800, rttUs: 310 },
      ]);

      unmount(app);
    } finally {
      hostEl.remove();
    }
  });

  // spec: cop-ui / Session-scoped Diagnostics parameter draft and Analysis investigation continuity across workspace switches
  it("matches an independent per-entity reference model across remounts, vehicle switches, single-vehicle reverts, dormant poison/delete retention, domain resets, and non-commandable selections with zero navigation actuation (#40)", () => {
    const skgStore = createSkgStore();
    const telemetryStore = createTelemetryStore();
    const traceBook = new TelemetryTraceBook();
    const draftStore = createDiagnosticsDraftStore();

    const domains: readonly UxsDomainKind[] = [
      "quadcopter",
      "rover",
      "surface-boat",
    ];
    const vehicleIds = [
      "boxbotix.local.robotics.uxs.vehicle.1",
      "boxbotix.local.robotics.uxs.vehicle.2",
      "boxbotix.local.robotics.uxs.vehicle.3",
    ] as const;
    const contactId = "boxbotix.local.tak.cop.vessel.harbor7";
    const genericId = "boxbotix.local.ops.corridor.sector.north1";
    const missingVehicleId = "boxbotix.local.robotics.uxs.vehicle.99";

    let rev = 1;
    const pushPut = (key: string, payload: unknown): void => {
      skgStore.ingestRawFrame(
        JSON.stringify({
          type: "put",
          bucket: "ENTITY_STATES",
          key,
          revision: rev++,
          payload,
        }),
      );
    };
    const pushPoison = (key: string): void => {
      skgStore.ingestRawFrame(
        JSON.stringify({
          type: "poisoned",
          bucket: "ENTITY_STATES",
          key,
          revision: rev++,
          error: "corrupt kv frame",
        }),
      );
    };
    const pushDelete = (key: string): void => {
      skgStore.ingestRawFrame(
        JSON.stringify({
          type: "delete",
          bucket: "ENTITY_STATES",
          key,
          revision: rev++,
        }),
      );
    };

    // Canonical sysid boundary & non-vehicle entity rejection at store seam.
    expect(
      draftStore.stageParam(
        {
          id: "boxbotix.local.robotics.uxs.vehicle.0",
          sysid: 0,
          domain: "quadcopter",
        },
        "uxs.control.heading_kp",
        4.0,
      ),
    ).toBeNull();
    expect(
      draftStore.stageParam(
        {
          id: "boxbotix.local.robotics.uxs.vehicle.255",
          sysid: 255,
          domain: "quadcopter",
        },
        "uxs.control.heading_kp",
        4.0,
      ),
    ).toBeNull();
    expect(
      draftStore.stageParam(
        {
          id: "boxbotix.local.robotics.uxs.vehicle.01",
          sysid: 1,
          domain: "quadcopter",
        },
        "uxs.control.heading_kp",
        4.0,
      ),
    ).toBeNull();
    expect(
      draftStore.stageParam(
        {
          id: contactId,
          sysid: 1,
          domain: "quadcopter",
        },
        "uxs.control.heading_kp",
        4.0,
      ),
    ).toBeNull();
    expect(
      draftStore.stageParam(
        {
          id: vehicleIds[0],
          sysid: 2, // sysid mismatch vs canonical ID
          domain: "quadcopter",
        },
        "uxs.control.heading_kp",
        4.0,
      ),
    ).toBeNull();
    expect(
      draftStore.stageParam(
        {
          id: vehicleIds[0],
          sysid: 1,
          domain: "quadcopter",
          poisoned: true,
        },
        "uxs.control.heading_kp",
        4.0,
      ),
    ).toBeNull();
    expect(draftStore.records.size).toBe(0);

    // Direct store-seam domain mismatch check (before reconcileVehicleDomain runs):
    draftStore.stageParam(
      { id: vehicleIds[0], sysid: 1, domain: "quadcopter" },
      "uxs.control.heading_kp",
      3.5,
    );
    const mismatchRead = draftStore.readVehicle({
      id: vehicleIds[0],
      sysid: 1,
      domain: "rover",
    });
    expect(mismatchRead?.domain).toBe("rover");
    expect(mismatchRead?.diffs).toEqual([]);
    expect(mismatchRead?.staged["uxs.control.heading_kp"]).toBe(
      defaultTuningBaseline("rover")["uxs.control.heading_kp"],
    );
    expect(draftStore.getDormantDraft(vehicleIds[0], "rover")).toBeNull();
    const mismatchStage = draftStore.stageParam(
      { id: vehicleIds[0], sysid: 1, domain: "rover" },
      "uxs.limits.max_groundspeed_mps",
      8.0,
    );
    expect(mismatchStage?.domain).toBe("rover");
    expect(mismatchStage?.baseline["uxs.control.heading_kp"]).toBe(
      defaultTuningBaseline("rover")["uxs.control.heading_kp"],
    );
    draftStore.clearAll();

    // Seed initial fleet + external contact + generic entity.
    const liveVehicleState = new Map<
      string,
      {
        sysid: number;
        domain: UxsDomainKind;
        status: "healthy" | "poisoned" | "deleted";
      }
    >();
    for (let i = 0; i < vehicleIds.length; i += 1) {
      const sysid = i + 1;
      const domain = domains[i]!;
      const id = vehicleIds[i]!;
      liveVehicleState.set(id, { sysid, domain, status: "healthy" });
      pushPut(
        id,
        buildVehicleStoredPayload(sysid, { "uxs.domain.kind": domain }),
      );
    }
    pushPut(contactId, {
      id: contactId,
      triples: [
        {
          subject: contactId,
          predicate: "dc.terms.title",
          object: "Harbor 7",
          source: "ais",
          timestamp: "2026-11-26T12:00:00Z",
          confidence: 1,
        },
      ],
    });
    pushPut(genericId, {
      id: genericId,
      triples: [
        {
          subject: genericId,
          predicate: "dc.terms.title",
          object: "North Corridor",
          source: "ops",
          timestamp: "2026-11-26T12:00:00Z",
          confidence: 1,
        },
      ],
    });
    skgStore.ingestRawFrame(
      JSON.stringify({
        type: "synced",
        bucket: "ENTITY_STATES",
        revision: rev - 1,
      }),
    );

    const viewportStore = createViewportStore({ skgStore });
    viewportStore.selectEntity(vehicleIds[0]);

    const hostEl = document.createElement("div");
    document.body.appendChild(hostEl);

    let mountedApp: ReturnType<typeof mount> | null = mount(BenchWorkspace, {
      target: hostEl,
      props: {
        telemetryStore,
        skgStore,
        viewportStore,
        traceBook,
        draftStore,
      },
    });
    flushSync();

    interface RefDraft {
      domain: UxsDomainKind;
      baselineSource: TuningBaselineSource;
      baseline: Record<string, number>;
      staged: Record<string, number>;
    }
    const refModel = new Map<string, RefDraft>();

    let sawRemountWithActiveDraft = false;
    let sawMultiVehicleIsolation = false;
    let sawSingleVehicleRevert = false;
    let sawPoisonDormantAndRecovery = false;
    let sawDeleteDormantRevert = false;
    let sawDomainMismatchReset = false;
    let sawGenericAndMissingExplanation = false;

    const mountIfNeeded = (): void => {
      if (!mountedApp) {
        mountedApp = mount(BenchWorkspace, {
          target: hostEl,
          props: {
            telemetryStore,
            skgStore,
            viewportStore,
            traceBook,
            draftStore,
          },
        });
        flushSync();
      }
    };

    try {
      const rng = createSeededRng(0x40d1a650);

      for (let step = 0; step < 260; step += 1) {
        const roll = rng();

        if (roll < 0.14) {
          // Simulate workspace switch away from and back to BenchWorkspace.
          if (mountedApp) {
            const hadAnyDiff = Array.from(refModel.values()).some(
              (r) =>
                computeTuningDiff(BENCH_TUNING_SPECS, r.baseline, r.staged)
                  .length > 0,
            );
            unmount(mountedApp);
            mountedApp = null;
            if (hadAnyDiff) {
              sawRemountWithActiveDraft = true;
            }
          } else {
            mountIfNeeded();
          }
        } else if (roll < 0.34) {
          // Select among UXS vehicles, external contact, generic node, missing vehicle ID, or null.
          const choices = [
            ...vehicleIds,
            contactId,
            genericId,
            missingVehicleId,
            null,
          ] as const;
          const pick = choices[Math.floor(rng() * choices.length)] ?? null;
          viewportStore.selectEntity(pick);
          flushSync();
          if (mountedApp && (pick === genericId || pick === missingVehicleId)) {
            sawGenericAndMissingExplanation = true;
          }
        } else if (roll < 0.62) {
          // Stage a parameter via DOM slider when mounted & commandable, or store seam.
          mountIfNeeded();
          const selId = viewportStore.selectedEntityId;
          const vState = selId ? liveVehicleState.get(selId) : undefined;
          if (vState && vState.status === "healthy" && selId) {
            const spec =
              BENCH_TUNING_SPECS[
                Math.floor(rng() * BENCH_TUNING_SPECS.length)
              ]!;
            const rawCandidates = [
              spec.min - 10,
              spec.max + 10,
              spec.min + rng() * (spec.max - spec.min),
              spec.key === "uxs.control.heading_kp" ? 3.3 : spec.max,
            ];
            const rawVal =
              rawCandidates[Math.floor(rng() * rawCandidates.length)]!;
            const input = hostEl.querySelector(
              `[data-param-key="${spec.key}"] input[type="range"]`,
            ) as HTMLInputElement | null;
            expect(input).not.toBeNull();
            input!.value = String(rawVal);
            input!.dispatchEvent(new Event("input", { bubbles: true }));
            flushSync();

            const existingRef = refModel.get(selId);
            const baseline =
              existingRef && existingRef.domain === vState.domain
                ? existingRef.baseline
                : defaultTuningBaseline(vState.domain);
            const baselineSource =
              existingRef && existingRef.domain === vState.domain
                ? existingRef.baselineSource
                : "domain-default";
            const priorStaged =
              existingRef && existingRef.domain === vState.domain
                ? existingRef.staged
                : baseline;
            refModel.set(selId, {
              domain: vState.domain,
              baselineSource,
              baseline,
              staged: stageTuningValue(
                BENCH_TUNING_SPECS,
                priorStaged,
                spec.key,
                rawVal,
              ),
            });

            const distinctDraftVehicles = Array.from(refModel.entries()).filter(
              ([, entry]) =>
                computeTuningDiff(
                  BENCH_TUNING_SPECS,
                  entry.baseline,
                  entry.staged,
                ).length > 0,
            );
            if (distinctDraftVehicles.length >= 2) {
              sawMultiVehicleIsolation = true;
            }
          }
        } else if (roll < 0.74) {
          // Revert active or dormant draft on currently selected vehicle.
          mountIfNeeded();
          const selId = viewportStore.selectedEntityId;
          const vState = selId ? liveVehicleState.get(selId) : undefined;
          const refEntry = selId ? refModel.get(selId) : undefined;
          if (selId && vState && refEntry) {
            const activeDiffs = computeTuningDiff(
              BENCH_TUNING_SPECS,
              refEntry.baseline,
              refEntry.staged,
            );
            if (activeDiffs.length > 0) {
              if (vState.status === "healthy") {
                const revertBtn = hostEl.querySelector(
                  '[data-testid="bench-tuning-revert"]',
                ) as HTMLButtonElement;
                revertBtn.click();
                flushSync();
                refModel.set(selId, {
                  ...refEntry,
                  staged: revertTuningDraft(refEntry.baseline),
                });
                const otherStillDirty = Array.from(refModel.entries()).some(
                  ([otherId, entry]) =>
                    otherId !== selId &&
                    computeTuningDiff(
                      BENCH_TUNING_SPECS,
                      entry.baseline,
                      entry.staged,
                    ).length > 0,
                );
                if (otherStillDirty) {
                  sawSingleVehicleRevert = true;
                }
              } else {
                const dormantRevertBtn = hostEl.querySelector(
                  '[data-testid="bench-dormant-revert"]',
                ) as HTMLButtonElement | null;
                expect(dormantRevertBtn).not.toBeNull();
                dormantRevertBtn!.click();
                flushSync();
                refModel.set(selId, {
                  ...refEntry,
                  staged: revertTuningDraft(refEntry.baseline),
                });
                if (vState.status === "deleted") {
                  sawDeleteDormantRevert = true;
                }
              }
            }
          }
        } else if (roll < 0.88) {
          // Poison / delete / recover a vehicle with same domain (OQ1 Option A).
          const targetId = vehicleIds[Math.floor(rng() * vehicleIds.length)]!;
          const vState = liveVehicleState.get(targetId)!;
          if (vState.status === "healthy") {
            if (rng() < 0.55) {
              vState.status = "poisoned";
              pushPoison(targetId);
            } else {
              vState.status = "deleted";
              pushDelete(targetId);
            }
            flushSync();
          } else {
            const wasPoisonedWithDraft =
              vState.status === "poisoned" &&
              Boolean(
                refModel.get(targetId) &&
                computeTuningDiff(
                  BENCH_TUNING_SPECS,
                  refModel.get(targetId)!.baseline,
                  refModel.get(targetId)!.staged,
                ).length > 0,
              );
            vState.status = "healthy";
            pushPut(
              targetId,
              buildVehicleStoredPayload(vState.sysid, {
                "uxs.domain.kind": vState.domain,
              }),
            );
            flushSync();
            if (wasPoisonedWithDraft) {
              sawPoisonDormantAndRecovery = true;
            }
          }
        } else {
          // Healthy domain change on a vehicle resets any stale domain draft.
          const targetId = vehicleIds[Math.floor(rng() * vehicleIds.length)]!;
          const vState = liveVehicleState.get(targetId)!;
          const nextDomain =
            domains[(domains.indexOf(vState.domain) + 1) % domains.length]!;
          const hadDirtyPriorDomain = Boolean(
            refModel.get(targetId) &&
            refModel.get(targetId)!.domain !== nextDomain &&
            computeTuningDiff(
              BENCH_TUNING_SPECS,
              refModel.get(targetId)!.baseline,
              refModel.get(targetId)!.staged,
            ).length > 0,
          );
          vState.domain = nextDomain;
          vState.status = "healthy";
          pushPut(
            targetId,
            buildVehicleStoredPayload(vState.sysid, {
              "uxs.domain.kind": nextDomain,
            }),
          );
          mountIfNeeded();
          flushSync();
          refModel.delete(targetId);
          if (hadDirtyPriorDomain) {
            sawDomainMismatchReset = true;
          }
        }

        // Assert store + DOM match reference model at every step.
        for (const id of vehicleIds) {
          const vState = liveVehicleState.get(id)!;
          const refEntry = refModel.get(id);
          if (vState.status === "healthy") {
            const expectedBaseline =
              refEntry && refEntry.domain === vState.domain
                ? refEntry.baseline
                : defaultTuningBaseline(vState.domain);
            const expectedStaged =
              refEntry && refEntry.domain === vState.domain
                ? refEntry.staged
                : expectedBaseline;
            const expectedDiffs = computeTuningDiff(
              BENCH_TUNING_SPECS,
              expectedBaseline,
              expectedStaged,
            );
            const view = draftStore.readVehicle(
              skgStore.getCommandableVehicle(id),
            );
            expect(view).not.toBeNull();
            expect(view!.baselineSource).toBe(
              refEntry?.baselineSource ?? "domain-default",
            );
            expect(view!.baseline).toEqual(expectedBaseline);
            expect(view!.staged).toEqual(expectedStaged);
            expect(view!.diffs).toEqual(expectedDiffs);
          } else {
            expect(skgStore.getCommandableVehicle(id)).toBeNull();
          }
        }

        if (mountedApp) {
          const selId = viewportStore.selectedEntityId;
          const vState = selId ? liveVehicleState.get(selId) : undefined;
          if (selId && vState && vState.status === "healthy") {
            const refEntry = refModel.get(selId);
            const expectedBaseline =
              refEntry?.baseline ?? defaultTuningBaseline(vState.domain);
            const expectedStaged = refEntry?.staged ?? expectedBaseline;
            const expectedDiffs = computeTuningDiff(
              BENCH_TUNING_SPECS,
              expectedBaseline,
              expectedStaged,
            );
            expect(
              hostEl.querySelector('[data-testid="bench-staged-count"]')
                ?.textContent,
            ).toContain(`${expectedDiffs.length} staged`);
            expect(
              (
                hostEl.querySelector(
                  '[data-testid="bench-tuning-apply"]',
                ) as HTMLButtonElement | null
              )?.disabled,
            ).toBe(true);
            const kpSpec = BENCH_TUNING_SPECS[0]!;
            const kpCard = hostEl.querySelector(
              `[data-param-key="${kpSpec.key}"]`,
            );
            expect(kpCard?.textContent).toContain(
              expectedStaged[kpSpec.key]!.toFixed(kpSpec.decimals),
            );
            if (
              Math.abs(
                expectedStaged[kpSpec.key]! - expectedBaseline[kpSpec.key]!,
              ) > 1e-9
            ) {
              expect(kpCard?.textContent).toContain(
                `(base ${expectedBaseline[kpSpec.key]!.toFixed(kpSpec.decimals)})`,
              );
            }
          } else {
            // Non-commandable / unavailable selection: zero range/number inputs and zero Apply button.
            expect(hostEl.querySelectorAll('input[type="range"]')).toHaveLength(
              0,
            );
            expect(
              hostEl.querySelectorAll('input[type="number"]'),
            ).toHaveLength(0);
            expect(
              hostEl.querySelector('[data-testid="bench-tuning-apply"]'),
            ).toBeNull();
            const withheldText =
              hostEl.querySelector('[data-testid="bench-tuning-withheld"]')
                ?.textContent ?? "";
            expect(withheldText.length).toBeGreaterThan(10);
            if (selId === contactId) {
              expect(withheldText).toContain("External COP contacts");
            } else if (selId === genericId) {
              expect(withheldText).toContain("North Corridor");
              expect(withheldText).toContain("not a commandable UXS vehicle");
            } else if (selId === missingVehicleId) {
              expect(withheldText).toContain("UXS #99");
              expect(withheldText).toContain("unavailable in live Fleet data");
            }
          }
        }
      }

      // Deterministic check of dormant delete -> revert path if RNG did not hit that exact branch.
      mountIfNeeded();
      if (!sawDeleteDormantRevert) {
        const targetId = vehicleIds[1];
        const vState = liveVehicleState.get(targetId)!;
        vState.status = "healthy";
        pushPut(
          targetId,
          buildVehicleStoredPayload(vState.sysid, {
            "uxs.domain.kind": vState.domain,
          }),
        );
        viewportStore.selectEntity(targetId);
        flushSync();
        const input = hostEl.querySelector(
          '[data-param-key="uxs.control.heading_kp"] input[type="range"]',
        ) as HTMLInputElement;
        input.value = "3.75";
        input.dispatchEvent(new Event("input", { bubbles: true }));
        flushSync();
        pushDelete(targetId);
        vState.status = "deleted";
        flushSync();
        expect(
          hostEl.querySelector('[data-testid="bench-tuning-withheld"]')
            ?.textContent,
        ).toContain("unavailable in live Fleet data");
        expect(
          hostEl.querySelector('[data-testid="bench-dormant-draft"]'),
        ).not.toBeNull();
        (
          hostEl.querySelector(
            '[data-testid="bench-dormant-revert"]',
          ) as HTMLButtonElement
        ).click();
        flushSync();
        expect(
          hostEl.querySelector('[data-testid="bench-dormant-draft"]'),
        ).toBeNull();
        sawDeleteDormantRevert = true;
      }

      expect(sawRemountWithActiveDraft).toBe(true);
      expect(sawMultiVehicleIsolation).toBe(true);
      expect(sawSingleVehicleRevert).toBe(true);
      expect(sawPoisonDormantAndRecovery).toBe(true);
      expect(sawDeleteDormantRevert).toBe(true);
      expect(sawDomainMismatchReset).toBe(true);
      expect(sawGenericAndMissingExplanation).toBe(true);
    } finally {
      if (mountedApp) {
        unmount(mountedApp);
      }
      hostEl.remove();
    }
  });

  // spec: cop-ui / Diagnostics sample baseline provenance, exact numeric staging, and non-actuating offline readiness
  it("distinguishes sample domain defaults from vehicle-reported fixtures before editing, enforces OQ1 strict exact numeric validation, renders target-vehicle diff summaries, and preserves provenance across vehicle switches with zero outbound commands (#43)", () => {
    const kpSpec = BENCH_TUNING_SPECS[0]!; // 0.10..8.00, step 0.05
    const gsSpec = BENCH_TUNING_SPECS[3]!; // 0.5..20.0, step 0.5
    const deadmanSpec = BENCH_TUNING_SPECS[5]!; // 100..1000, step 25

    // 1. Unit oracle checks for strict exact numeric validation (OQ1 Option A).
    expect(validateTuningNumericInput(kpSpec, "")).toMatchObject({
      ok: false,
      reason: "empty",
    });
    expect(validateTuningNumericInput(kpSpec, "   ")).toMatchObject({
      ok: false,
      reason: "empty",
    });
    expect(validateTuningNumericInput(kpSpec, "3.2abc")).toMatchObject({
      ok: false,
      reason: "nan",
    });
    expect(validateTuningNumericInput(kpSpec, "1e2")).toMatchObject({
      ok: false,
      reason: "nan",
    });
    expect(validateTuningNumericInput(kpSpec, "0.05")).toMatchObject({
      ok: false,
      reason: "below-min",
    });
    expect(validateTuningNumericInput(kpSpec, "8.05")).toMatchObject({
      ok: false,
      reason: "above-max",
    });
    expect(validateTuningNumericInput(kpSpec, "3.23")).toMatchObject({
      ok: false,
      reason: "off-step",
    });
    expect(validateTuningNumericInput(kpSpec, "0.10")).toEqual({
      ok: true,
      value: 0.1,
    });
    expect(validateTuningNumericInput(kpSpec, "8.00")).toEqual({
      ok: true,
      value: 8.0,
    });
    expect(validateTuningNumericInput(kpSpec, "3.35")).toEqual({
      ok: true,
      value: 3.35,
    });
    expect(validateTuningNumericInput(gsSpec, "12.3")).toMatchObject({
      ok: false,
      reason: "off-step",
    });
    expect(validateTuningNumericInput(gsSpec, "12.5")).toEqual({
      ok: true,
      value: 12.5,
    });
    expect(validateTuningNumericInput(deadmanSpec, "260")).toMatchObject({
      ok: false,
      reason: "off-step",
    });
    expect(validateTuningNumericInput(deadmanSpec, "275")).toEqual({
      ok: true,
      value: 275,
    });

    expect(formatTuningBounds(kpSpec)).toBe("Bounds 0.10–8.00 1/s · step 0.05");
    expect(
      describeTuningProvenance(1, "quadcopter", "domain-default").badge,
    ).toBe("SAMPLE DOMAIN DEFAULTS");
    expect(describeTuningProvenance(2, "rover", "vehicle-reported").badge).toBe(
      "VEHICLE-REPORTED BASELINE",
    );

    // 2. Component & multi-vehicle provenance check: UXS #1 uses sample domain defaults, UXS #2 uses seeded vehicle-reported baseline fixture.
    const skgStore = createSkgStore();
    const telemetryStore = createTelemetryStore();
    const traceBook = new TelemetryTraceBook();
    const draftStore = createDiagnosticsDraftStore();

    const quad1 = buildVehicleStoredPayload(1, {
      "uxs.domain.kind": "quadcopter",
    });
    const rover2 = buildVehicleStoredPayload(2, {
      "uxs.domain.kind": "rover",
    });
    const contactId = "boxbotix.local.tak.cop.vessel.harbor9";

    skgStore.ingestRawFrame(
      JSON.stringify({
        type: "put",
        bucket: "ENTITY_STATES",
        key: quad1.id,
        revision: 1,
        payload: quad1,
      }),
    );
    skgStore.ingestRawFrame(
      JSON.stringify({
        type: "put",
        bucket: "ENTITY_STATES",
        key: rover2.id,
        revision: 2,
        payload: rover2,
      }),
    );
    skgStore.ingestRawFrame(
      JSON.stringify({
        type: "put",
        bucket: "ENTITY_STATES",
        key: contactId,
        revision: 3,
        payload: {
          id: contactId,
          triples: [
            {
              subject: contactId,
              predicate: "dc.terms.title",
              object: "Harbor 9",
              source: "ais",
              timestamp: "2026-11-26T12:00:00Z",
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
        revision: 3,
      }),
    );

    // Seed UXS #2 with a vehicle-reported baseline distinct from rover sample defaults (kp 2.45 vs 2.10).
    const seededRoverView = draftStore.seedVehicleBaseline(
      skgStore.getCommandableVehicle(rover2.id),
      {
        source: "vehicle-reported",
        baseline: {
          "uxs.control.heading_kp": 2.45,
          "uxs.control.heading_kd": 0.24,
          "uxs.control.rate_ff": 0.3,
          "uxs.limits.max_groundspeed_mps": 6.0,
          "uxs.limits.max_yaw_rate_degs": 90,
          "uxs.mux.teleop_deadman_ms": 300,
        },
      },
    );
    expect(seededRoverView?.baselineSource).toBe("vehicle-reported");
    expect(seededRoverView?.diffs).toEqual([]);

    const viewportStore = createViewportStore({ skgStore });
    viewportStore.selectEntity(quad1.id);

    const fetchSpy = vi.fn();
    vi.stubGlobal("fetch", fetchSpy);
    const hostEl = document.createElement("div");
    document.body.appendChild(hostEl);
    const app = mount(BenchWorkspace, {
      target: hostEl,
      props: {
        telemetryStore,
        skgStore,
        viewportStore,
        traceBook,
        draftStore,
      },
    });
    flushSync();

    try {
      // 3. Before editing UXS #1 (0 staged), verify sample-default provenance, plain-language local-draft copy, per-param baseline/bounds, and nearby unavailable-apply text.
      expect(
        hostEl.querySelector('[data-testid="bench-staged-count"]')?.textContent,
      ).toBe("0 staged");
      const badgeEl = hostEl.querySelector(
        '[data-testid="bench-baseline-source-badge"]',
      );
      expect(badgeEl?.getAttribute("data-baseline-source")).toBe(
        "domain-default",
      );
      expect(badgeEl?.textContent).toContain("SAMPLE DOMAIN DEFAULTS");
      expect(
        hostEl.querySelector('[data-testid="bench-provenance-headline"]')
          ?.textContent,
      ).toContain(
        "Values shown for UXS #1 are sample quadcopter domain defaults, not parameters read from the vehicle.",
      );
      expect(
        hostEl.querySelector('[data-testid="bench-local-draft-notice"]')
          ?.textContent,
      ).toContain(
        "Edits create a local draft in this browser only and cannot change the vehicle in this version.",
      );
      expect(
        hostEl.querySelectorAll('[data-testid="bench-param-meta"]'),
      ).toHaveLength(BENCH_TUNING_SPECS.length);
      expect(
        hostEl.querySelector(
          '[data-param-key="uxs.control.heading_kp"] [data-testid="bench-param-meta"]',
        )?.textContent,
      ).toContain("Baseline 3.20 1/s · Bounds 0.10–8.00 1/s · step 0.05");
      expect(
        hostEl.querySelector('[data-testid="bench-change-summary-empty"]')
          ?.textContent,
      ).toContain("No local parameter changes staged for UXS #1.");
      const applyNote = hostEl.querySelector(
        '[data-testid="bench-apply-unavailable-note"]',
      )?.textContent;
      expect(applyNote).toContain(
        "Applying parameters to the vehicle is unavailable in this version.",
      );
      expect(hostEl.textContent).not.toMatch(/\b(?:Applied|Confirmed)\b/);

      // 4. Enter valid exact numeric value (3.45) on UXS #1 -> stages 3.45, syncs slider, and updates target-vehicle summary (+0.25 1/s).
      const kpNumberInput = hostEl.querySelector(
        '[data-param-key="uxs.control.heading_kp"] input[type="number"]',
      ) as HTMLInputElement;
      const kpRangeInput = hostEl.querySelector(
        '[data-param-key="uxs.control.heading_kp"] input[type="range"]',
      ) as HTMLInputElement;
      kpNumberInput.value = "3.45";
      kpNumberInput.dispatchEvent(new Event("input", { bubbles: true }));
      flushSync();

      expect(kpRangeInput.value).toBe("3.45");
      expect(
        hostEl.querySelector('[data-testid="bench-staged-count"]')?.textContent,
      ).toBe("1 staged");
      const summaryText =
        hostEl.querySelector('[data-testid="bench-change-summary"]')
          ?.textContent ?? "";
      expect(summaryText).toContain("TARGET UXS #1 (quadcopter)");
      expect(summaryText).toContain("3.20 → 3.45 1/s");
      expect(summaryText).toContain("(+0.25 1/s)");
      expect(summaryText).toContain("[0.10–8.00 1/s, step 0.05]");

      // 5. Enter out-of-bounds ("9.50") and off-step ("3.42") text -> rejected with aria-invalid + alert without mutating staged 3.45!
      kpNumberInput.value = "9.50";
      kpNumberInput.dispatchEvent(new Event("input", { bubbles: true }));
      flushSync();

      expect(kpNumberInput.getAttribute("aria-invalid")).toBe("true");
      expect(
        hostEl.querySelector(
          '[data-param-key="uxs.control.heading_kp"] [data-testid="bench-param-error"]',
        )?.textContent,
      ).toContain("Maximum is 8.00 1/s");
      expect(
        draftStore.readVehicle(skgStore.getCommandableVehicle(quad1.id))
          ?.staged["uxs.control.heading_kp"],
      ).toBe(3.45);
      expect(kpRangeInput.value).toBe("3.45");

      kpNumberInput.value = "3.42";
      kpNumberInput.dispatchEvent(new Event("input", { bubbles: true }));
      flushSync();
      expect(
        hostEl.querySelector(
          '[data-param-key="uxs.control.heading_kp"] [data-testid="bench-param-error"]',
        )?.textContent,
      ).toContain("Must align to step 0.05 1/s");
      expect(
        draftStore.readVehicle(skgStore.getCommandableVehicle(quad1.id))
          ?.staged["uxs.control.heading_kp"],
      ).toBe(3.45);

      // Pressing Escape inside the invalid numeric input clears the invalid draft back to 3.45 without mutating staged state.
      kpNumberInput.dispatchEvent(
        new KeyboardEvent("keydown", { key: "Escape", bubbles: true }),
      );
      flushSync();
      expect(kpNumberInput.getAttribute("aria-invalid")).toBeNull();
      expect(kpNumberInput.value).toBe("3.45");
      expect(
        hostEl.querySelector('[data-testid="bench-param-error"]'),
      ).toBeNull();

      // 6. Switch to UXS #2 (vehicle-reported fixture): provenance switches cleanly to VEHICLE-REPORTED BASELINE while keeping UXS #1's sample draft isolated.
      viewportStore.selectEntity(rover2.id);
      flushSync();

      const roverBadge = hostEl.querySelector(
        '[data-testid="bench-baseline-source-badge"]',
      );
      expect(roverBadge?.getAttribute("data-baseline-source")).toBe(
        "vehicle-reported",
      );
      expect(roverBadge?.textContent).toContain("VEHICLE-REPORTED BASELINE");
      expect(
        hostEl.querySelector('[data-testid="bench-provenance-headline"]')
          ?.textContent,
      ).toContain("vehicle-reported parameter data");
      expect(
        hostEl.querySelector(
          '[data-param-key="uxs.control.heading_kp"] [data-testid="bench-param-meta"]',
        )?.textContent,
      ).toContain("Baseline 2.45 1/s");

      const roverGsInput = hostEl.querySelector(
        '[data-param-key="uxs.limits.max_groundspeed_mps"] input[type="number"]',
      ) as HTMLInputElement;
      roverGsInput.value = "8.5";
      roverGsInput.dispatchEvent(new Event("input", { bubbles: true }));
      flushSync();
      expect(
        hostEl.querySelector('[data-testid="bench-change-summary"]')
          ?.textContent,
      ).toContain("vs. vehicle-reported baseline");

      // Switch to external contact -> withheld, then back to UXS #1 -> sample-default provenance & 3.45 draft intact.
      viewportStore.selectEntity(contactId);
      flushSync();
      expect(
        hostEl.querySelector('[data-testid="bench-tuning-withheld"]'),
      ).not.toBeNull();

      viewportStore.selectEntity(quad1.id);
      flushSync();
      expect(
        hostEl
          .querySelector('[data-testid="bench-baseline-source-badge"]')
          ?.getAttribute("data-baseline-source"),
      ).toBe("domain-default");
      expect(
        hostEl.querySelector('[data-testid="bench-staged-count"]')?.textContent,
      ).toBe("1 staged");

      // Revert UXS #1 even after typing an invalid draft ("abc"): clears both staged diff and transient validation error!
      const quadKpInputAfter = hostEl.querySelector(
        '[data-param-key="uxs.control.heading_kp"] input[type="number"]',
      ) as HTMLInputElement;
      quadKpInputAfter.value = "abc";
      quadKpInputAfter.dispatchEvent(new Event("input", { bubbles: true }));
      flushSync();
      expect(quadKpInputAfter.getAttribute("aria-invalid")).toBe("true");

      (
        hostEl.querySelector(
          '[data-testid="bench-tuning-revert"]',
        ) as HTMLButtonElement
      ).click();
      flushSync();
      expect(
        hostEl.querySelector('[data-testid="bench-staged-count"]')?.textContent,
      ).toBe("0 staged");
      expect(quadKpInputAfter.getAttribute("aria-invalid")).toBeNull();
      expect(quadKpInputAfter.value).toBe("3.20");

      // UXS #2 retains its vehicle-reported baseline and 8.5 m/s staged diff.
      const roverViewAfter = draftStore.readVehicle(
        skgStore.getCommandableVehicle(rover2.id),
      );
      expect(roverViewAfter?.baselineSource).toBe("vehicle-reported");
      expect(roverViewAfter?.diffs).toHaveLength(1);
      expect(roverViewAfter?.staged["uxs.limits.max_groundspeed_mps"]).toBe(
        8.5,
      );
      expect(fetchSpy).not.toHaveBeenCalled();
    } finally {
      vi.unstubAllGlobals();
      unmount(app);
      hostEl.remove();
    }
  });

  // spec: cop-ui / Diagnostics sample baseline provenance, exact numeric staging, and non-actuating offline readiness
  it("matches an independent exact-numeric & slider reference model across seeded valid, out-of-bounds, off-step, NaN, switch, and revert histories (#43)", () => {
    const skgStore = createSkgStore();
    const telemetryStore = createTelemetryStore();
    const traceBook = new TelemetryTraceBook();
    const draftStore = createDiagnosticsDraftStore();

    const v1 = buildVehicleStoredPayload(1, {
      "uxs.domain.kind": "quadcopter",
    });
    const v2 = buildVehicleStoredPayload(2, {
      "uxs.domain.kind": "surface-boat",
    });
    skgStore.ingestRawFrame(
      JSON.stringify({
        type: "put",
        bucket: "ENTITY_STATES",
        key: v1.id,
        revision: 1,
        payload: v1,
      }),
    );
    skgStore.ingestRawFrame(
      JSON.stringify({
        type: "put",
        bucket: "ENTITY_STATES",
        key: v2.id,
        revision: 2,
        payload: v2,
      }),
    );
    skgStore.ingestRawFrame(
      JSON.stringify({
        type: "synced",
        bucket: "ENTITY_STATES",
        revision: 2,
      }),
    );

    const v2Baseline = {
      ...defaultTuningBaseline("surface-boat"),
      "uxs.control.heading_kp": 1.85,
    };
    draftStore.seedVehicleBaseline(skgStore.getCommandableVehicle(v2.id), {
      source: "vehicle-reported",
      baseline: v2Baseline,
    });

    const viewportStore = createViewportStore({ skgStore });
    viewportStore.selectEntity(v1.id);

    const hostEl = document.createElement("div");
    document.body.appendChild(hostEl);
    const app = mount(BenchWorkspace, {
      target: hostEl,
      props: {
        telemetryStore,
        skgStore,
        viewportStore,
        traceBook,
        draftStore,
      },
    });
    flushSync();

    const refStaged = new Map<string, Record<string, number>>([
      [v1.id, { ...defaultTuningBaseline("quadcopter") }],
      [v2.id, { ...v2Baseline }],
    ]);
    const refBaseline = new Map<string, Record<string, number>>([
      [v1.id, { ...defaultTuningBaseline("quadcopter") }],
      [v2.id, { ...v2Baseline }],
    ]);
    const refSource = new Map<string, TuningBaselineSource>([
      [v1.id, "domain-default"],
      [v2.id, "vehicle-reported"],
    ]);

    let sawValidExactNumeric = false;
    let sawBelowMinReject = false;
    let sawAboveMaxReject = false;
    let sawOffStepReject = false;
    let sawNonNumericReject = false;

    try {
      const rng = createSeededRng(0x430ff1ce);
      const ids = [v1.id, v2.id] as const;

      for (let step = 0; step < 180; step += 1) {
        const action = rng();
        const selId = viewportStore.selectedEntityId!;
        const spec =
          BENCH_TUNING_SPECS[Math.floor(rng() * BENCH_TUNING_SPECS.length)]!;

        if (action < 0.18) {
          const nextId = ids[Math.floor(rng() * ids.length)]!;
          viewportStore.selectEntity(nextId);
          flushSync();
        } else if (action < 0.72) {
          const numInput = hostEl.querySelector(
            `[data-param-key="${spec.key}"] input[type="number"]`,
          ) as HTMLInputElement;
          const kindRoll = rng();
          let candidateText: string;
          if (kindRoll < 0.42) {
            const totalSteps = Math.round((spec.max - spec.min) / spec.step);
            const k = Math.floor(rng() * (totalSteps + 1));
            const validNum = spec.min + k * spec.step;
            candidateText = validNum.toFixed(spec.decimals);
          } else if (kindRoll < 0.57) {
            candidateText = (spec.min - spec.step).toFixed(spec.decimals);
          } else if (kindRoll < 0.72) {
            candidateText = (spec.max + spec.step).toFixed(spec.decimals);
          } else if (kindRoll < 0.87) {
            candidateText = (spec.min + spec.step * 0.37).toFixed(
              spec.decimals + 2,
            );
          } else {
            candidateText = rng() < 0.5 ? "" : "bad-num";
          }

          numInput.value = candidateText;
          numInput.dispatchEvent(new Event("input", { bubbles: true }));
          flushSync();

          // Independent oracle check without calling validateTuningNumericInput:
          const trimmed = candidateText.trim();
          const literalValid = /^[+-]?(?:\d+(?:\.\d*)?|\.\d+)$/.test(trimmed);
          const parsed = literalValid ? Number(trimmed) : Number.NaN;
          const inRange =
            Number.isFinite(parsed) &&
            parsed >= spec.min - 1e-9 &&
            parsed <= spec.max + 1e-9;
          const stepAligned =
            inRange &&
            Math.abs(
              (parsed - spec.min) / spec.step -
                Math.round((parsed - spec.min) / spec.step),
            ) <= 1e-6;

          if (stepAligned) {
            const committed = Number(parsed.toFixed(spec.decimals));
            refStaged.set(selId, {
              ...refStaged.get(selId)!,
              [spec.key]: Object.is(committed, -0) ? 0 : committed,
            });
            expect(numInput.getAttribute("aria-invalid")).toBeNull();
            sawValidExactNumeric = true;
          } else {
            expect(numInput.getAttribute("aria-invalid")).toBe("true");
            if (!literalValid || !Number.isFinite(parsed)) {
              sawNonNumericReject = true;
            } else if (parsed < spec.min - 1e-9) {
              sawBelowMinReject = true;
            } else if (parsed > spec.max + 1e-9) {
              sawAboveMaxReject = true;
            } else {
              sawOffStepReject = true;
            }
          }
        } else if (action < 0.88) {
          const rangeInput = hostEl.querySelector(
            `[data-param-key="${spec.key}"] input[type="range"]`,
          ) as HTMLInputElement;
          const totalSteps = Math.round((spec.max - spec.min) / spec.step);
          const k = Math.floor(rng() * (totalSteps + 1));
          const nextVal = Number(
            (spec.min + k * spec.step).toFixed(spec.decimals),
          );
          rangeInput.value = String(nextVal);
          rangeInput.dispatchEvent(new Event("input", { bubbles: true }));
          flushSync();
          refStaged.set(selId, {
            ...refStaged.get(selId)!,
            [spec.key]: nextVal,
          });
        } else {
          const revertBtn = hostEl.querySelector(
            '[data-testid="bench-tuning-revert"]',
          ) as HTMLButtonElement;
          if (!revertBtn.disabled) {
            revertBtn.click();
            flushSync();
            refStaged.set(selId, { ...refBaseline.get(selId)! });
          }
        }

        for (const id of ids) {
          const view = draftStore.readVehicle(
            skgStore.getCommandableVehicle(id),
          );
          expect(view).not.toBeNull();
          expect(view!.baselineSource).toBe(refSource.get(id));
          expect(view!.baseline).toEqual(refBaseline.get(id));
          expect(view!.staged).toEqual(refStaged.get(id));
        }
      }

      expect(sawValidExactNumeric).toBe(true);
      expect(sawBelowMinReject).toBe(true);
      expect(sawAboveMaxReject).toBe(true);
      expect(sawOffStepReject).toBe(true);
      expect(sawNonNumericReject).toBe(true);
    } finally {
      unmount(app);
      hostEl.remove();
    }
  });
});
