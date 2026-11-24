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

        const sample: TelemetrySample = {
          timestamp: ts,
          rollRateDegS: Math.round((rng() - 0.5) * 200) + 0,
          pitchRateDegS: Math.round((rng() - 0.5) * 180) + 0,
          yawRateDegS: Math.round((rng() - 0.5) * 120) + 0,
          groundspeedMps: Math.max(0, Math.round(rng() * 15)) + 0,
          climbRateMps: Math.round((rng() - 0.5) * 8) + 0,
          loopDtUs: 1000 + Math.floor(rng() * 500),
          rttUs: 800 + Math.floor(rng() * 600),
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
});
