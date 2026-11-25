// SPDX-License-Identifier: MIT
// Copyright (c) 2026 boxbotix

import { type UxsDomainKind, isFiniteNumber } from "../uxs";

// shortcut: tuning rack stages parameter diffs locally read-only in M2.4, wire ADR-111 non-retrying PARAM_SET commit in M3.1 (#11)
export type TuningControlWidget = "dial" | "slider";

export interface TuningParamSpec {
  readonly key: string;
  readonly label: string;
  readonly unit: string;
  readonly widget: TuningControlWidget;
  readonly min: number;
  readonly max: number;
  readonly step: number;
  readonly decimals: number;
}

export interface TuningDiffEntry {
  readonly key: string;
  readonly label: string;
  readonly unit: string;
  readonly baseline: number;
  readonly staged: number;
  readonly delta: number;
}

export const BENCH_TUNING_SPECS: readonly TuningParamSpec[] = [
  {
    key: "uxs.control.heading_kp",
    label: "Heading P Gain",
    unit: "1/s",
    widget: "dial",
    min: 0.1,
    max: 8.0,
    step: 0.05,
    decimals: 2,
  },
  {
    key: "uxs.control.heading_kd",
    label: "Heading D Damping",
    unit: "s",
    widget: "dial",
    min: 0.0,
    max: 2.5,
    step: 0.02,
    decimals: 2,
  },
  {
    key: "uxs.control.rate_ff",
    label: "Rate Feedforward",
    unit: "gain",
    widget: "dial",
    min: 0.0,
    max: 1.5,
    step: 0.01,
    decimals: 2,
  },
  {
    key: "uxs.limits.max_groundspeed_mps",
    label: "Max Groundspeed",
    unit: "m/s",
    widget: "slider",
    min: 0.5,
    max: 20.0,
    step: 0.5,
    decimals: 1,
  },
  {
    key: "uxs.limits.max_yaw_rate_degs",
    label: "Max Yaw Rate",
    unit: "deg/s",
    widget: "slider",
    min: 10,
    max: 180,
    step: 5,
    decimals: 0,
  },
  {
    key: "uxs.mux.teleop_deadman_ms",
    label: "Teleop Deadman Timeout",
    unit: "ms",
    widget: "slider",
    min: 100,
    max: 1000,
    step: 25,
    decimals: 0,
  },
] as const;

export function defaultTuningBaseline(
  domain: UxsDomainKind,
): Record<string, number> {
  switch (domain) {
    case "quadcopter":
      return {
        "uxs.control.heading_kp": 3.2,
        "uxs.control.heading_kd": 0.36,
        "uxs.control.rate_ff": 0.45,
        "uxs.limits.max_groundspeed_mps": 12.0,
        "uxs.limits.max_yaw_rate_degs": 120,
        "uxs.mux.teleop_deadman_ms": 250,
      };
    case "rover":
      return {
        "uxs.control.heading_kp": 2.1,
        "uxs.control.heading_kd": 0.24,
        "uxs.control.rate_ff": 0.3,
        "uxs.limits.max_groundspeed_mps": 6.0,
        "uxs.limits.max_yaw_rate_degs": 90,
        "uxs.mux.teleop_deadman_ms": 300,
      };
    case "surface-boat":
      return {
        "uxs.control.heading_kp": 1.6,
        "uxs.control.heading_kd": 0.48,
        "uxs.control.rate_ff": 0.25,
        "uxs.limits.max_groundspeed_mps": 8.5,
        "uxs.limits.max_yaw_rate_degs": 60,
        "uxs.mux.teleop_deadman_ms": 400,
      };
  }
}

export function clampTuningValue(spec: TuningParamSpec, raw: number): number {
  if (!isFiniteNumber(raw)) {
    return spec.min;
  }
  const clamped = Math.max(spec.min, Math.min(spec.max, raw));
  const quantized =
    spec.step > 0
      ? Math.round((clamped - spec.min) / spec.step) * spec.step + spec.min
      : clamped;
  const factor = 10 ** spec.decimals;
  const rounded = Math.round(quantized * factor) / factor;
  return Math.max(
    spec.min,
    Math.min(spec.max, Object.is(rounded, -0) ? 0 : rounded),
  );
}

export function stageTuningValue(
  specs: readonly TuningParamSpec[],
  draft: Readonly<Record<string, number>>,
  key: string,
  nextRawValue: number,
): Record<string, number> {
  const spec = specs.find((item) => item.key === key);
  if (!spec) {
    return { ...draft };
  }
  return {
    ...draft,
    [key]: clampTuningValue(spec, nextRawValue),
  };
}

export function revertTuningDraft(
  baseline: Readonly<Record<string, number>>,
): Record<string, number> {
  return { ...baseline };
}

export function computeTuningDiff(
  specs: readonly TuningParamSpec[],
  baseline: Readonly<Record<string, number>>,
  draft: Readonly<Record<string, number>>,
): readonly TuningDiffEntry[] {
  const diffs: TuningDiffEntry[] = [];
  for (const spec of specs) {
    const baseVal = baseline[spec.key];
    const draftVal = draft[spec.key];
    if (!isFiniteNumber(baseVal) || !isFiniteNumber(draftVal)) {
      continue;
    }
    const baseClean = clampTuningValue(spec, baseVal);
    const draftClean = clampTuningValue(spec, draftVal);
    const delta = Number((draftClean - baseClean).toFixed(spec.decimals));
    if (Math.abs(delta) > 1e-9) {
      diffs.push({
        key: spec.key,
        label: spec.label,
        unit: spec.unit,
        baseline: baseClean,
        staged: draftClean,
        delta,
      });
    }
  }
  return diffs;
}

/**
 * Computes normalized [0, 1] dial sweep progress for SVG rotary arc rendering.
 */
export function computeDialProgress(
  spec: TuningParamSpec,
  value: number,
): number {
  const span = spec.max - spec.min;
  if (span <= 0 || !isFiniteNumber(value)) {
    return 0;
  }
  return Math.max(0, Math.min(1, (value - spec.min) / span));
}
