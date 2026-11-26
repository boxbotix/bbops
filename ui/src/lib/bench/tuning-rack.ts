// SPDX-License-Identifier: MIT
// Copyright (c) 2026 boxbotix

import { SvelteMap } from "svelte/reactivity";
import {
  type UxsDomainKind,
  isFiniteNumber,
  isUxsDomainKind,
  parseVehicleEntityId,
} from "../uxs";

// shortcut: tuning rack stages parameter diffs locally read-only in M2.4, wire ADR-111 non-retrying PARAM_SET commit in M3.1 (#11)
export type TuningControlWidget = "dial" | "slider";

export type TuningBaselineSource = "domain-default" | "vehicle-reported";

export type TuningNumericValidationErrorReason =
  "empty" | "nan" | "below-min" | "above-max" | "off-step";

export type TuningNumericValidationResult =
  | {
      readonly ok: true;
      readonly value: number;
    }
  | {
      readonly ok: false;
      readonly reason: TuningNumericValidationErrorReason;
      readonly message: string;
    };

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
  readonly decimals: number;
  readonly min: number;
  readonly max: number;
  readonly step: number;
  readonly baseline: number;
  readonly staged: number;
  readonly delta: number;
}

export interface TuningBaselineSeed {
  readonly baseline: Readonly<Record<string, number>>;
  readonly source?: TuningBaselineSource;
}

export interface VehicleTuningDraftRecord {
  readonly entityId: string;
  readonly sysid: number;
  readonly domain: UxsDomainKind;
  readonly baselineSource: TuningBaselineSource;
  readonly baseline: Readonly<Record<string, number>>;
  readonly staged: Readonly<Record<string, number>>;
}

export interface VehicleTuningView {
  readonly entityId: string;
  readonly sysid: number;
  readonly domain: UxsDomainKind;
  readonly baselineSource: TuningBaselineSource;
  readonly baseline: Readonly<Record<string, number>>;
  readonly staged: Readonly<Record<string, number>>;
  readonly diffs: readonly TuningDiffEntry[];
}

export interface DormantVehicleDraftView {
  readonly entityId: string;
  readonly sysid: number;
  readonly domain: UxsDomainKind;
  readonly baselineSource: TuningBaselineSource;
  readonly diffs: readonly TuningDiffEntry[];
}

export interface CommandableVehicleRef {
  readonly id: string;
  readonly sysid: number;
  readonly domain: UxsDomainKind;
  readonly poisoned?: boolean;
}

export interface DiagnosticsDraftStore {
  readonly records: SvelteMap<string, VehicleTuningDraftRecord>;
  readVehicle(
    vehicle: CommandableVehicleRef | null | undefined,
  ): VehicleTuningView | null;
  getDormantDraft(
    entityId: string | null | undefined,
    knownDomain?: UxsDomainKind | null,
  ): DormantVehicleDraftView | null;
  seedVehicleBaseline(
    vehicle: CommandableVehicleRef | null | undefined,
    seed: TuningBaselineSeed,
  ): VehicleTuningView | null;
  stageParam(
    vehicle: CommandableVehicleRef | null | undefined,
    key: string,
    rawValue: number,
    baselineOverride?: Readonly<Record<string, number>> | TuningBaselineSeed,
  ): VehicleTuningView | null;
  revertVehicle(entityId: string | null | undefined): boolean;
  reconcileVehicleDomain(
    entityId: string | null | undefined,
    domain: UxsDomainKind,
  ): void;
  clearAll(): void;
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

export function formatTuningNumber(
  spec: Pick<TuningParamSpec, "decimals">,
  value: number,
): string {
  const normalized = Object.is(value, -0) ? 0 : value;
  return normalized.toFixed(spec.decimals);
}

export function formatTuningBounds(spec: TuningParamSpec): string {
  const minText = formatTuningNumber(spec, spec.min);
  const maxText = formatTuningNumber(spec, spec.max);
  const stepText = formatTuningNumber(spec, spec.step);
  return `Bounds ${minText}–${maxText} ${spec.unit} · step ${stepText}`;
}

export function formatSignedTuningDelta(
  spec: Pick<TuningParamSpec, "decimals">,
  delta: number,
): string {
  const normalized = Object.is(delta, -0) ? 0 : delta;
  const prefix = normalized > 0 ? "+" : "";
  return `${prefix}${normalized.toFixed(spec.decimals)}`;
}

export interface TuningProvenanceCopy {
  readonly badge: string;
  readonly headline: string;
  readonly localStagingNotice: string;
  readonly unavailableApplyNotice: string;
}

export function describeTuningProvenance(
  sysid: number,
  domain: UxsDomainKind,
  baselineSource: TuningBaselineSource,
): TuningProvenanceCopy {
  const localStagingNotice =
    "Edits create a local draft in this browser only and cannot change the vehicle in this version.";
  const unavailableApplyNotice =
    "Applying parameters to the vehicle is unavailable in this version. No values have been sent, applied, or confirmed on the vehicle.";

  if (baselineSource === "vehicle-reported") {
    return {
      badge: "VEHICLE-REPORTED BASELINE",
      headline: `Baseline values shown for UXS #${sysid} (${domain}) come from vehicle-reported parameter data.`,
      localStagingNotice,
      unavailableApplyNotice,
    };
  }

  return {
    badge: "SAMPLE DOMAIN DEFAULTS",
    headline: `Values shown for UXS #${sysid} are sample ${domain} domain defaults, not parameters read from the vehicle.`,
    localStagingNotice,
    unavailableApplyNotice,
  };
}

const STRICT_NUMERIC_LITERAL_RE = /^[+-]?(?:\d+(?:\.\d*)?|\.\d+)$/;

/**
 * Validates exact numeric text entry against parameter bounds and increment step
 * (`OQ1` Option A: rejects empty, non-numeric, out-of-bounds, and off-step values
 * without mutating staged state).
 */
export function validateTuningNumericInput(
  spec: TuningParamSpec,
  rawText: string,
): TuningNumericValidationResult {
  const trimmed = rawText.trim();
  const minText = formatTuningNumber(spec, spec.min);
  const maxText = formatTuningNumber(spec, spec.max);
  const stepText = formatTuningNumber(spec, spec.step);

  if (trimmed.length === 0) {
    return {
      ok: false,
      reason: "empty",
      message: `Enter a number between ${minText} and ${maxText} ${spec.unit} (step ${stepText}).`,
    };
  }

  if (!STRICT_NUMERIC_LITERAL_RE.test(trimmed)) {
    return {
      ok: false,
      reason: "nan",
      message: `Enter a valid decimal number between ${minText} and ${maxText} ${spec.unit}.`,
    };
  }

  const parsed = Number(trimmed);
  if (!isFiniteNumber(parsed)) {
    return {
      ok: false,
      reason: "nan",
      message: `Enter a valid decimal number between ${minText} and ${maxText} ${spec.unit}.`,
    };
  }

  if (parsed < spec.min - 1e-9) {
    return {
      ok: false,
      reason: "below-min",
      message: `Minimum is ${minText} ${spec.unit} (allowed ${minText}–${maxText} ${spec.unit}, step ${stepText}).`,
    };
  }

  if (parsed > spec.max + 1e-9) {
    return {
      ok: false,
      reason: "above-max",
      message: `Maximum is ${maxText} ${spec.unit} (allowed ${minText}–${maxText} ${spec.unit}, step ${stepText}).`,
    };
  }

  if (spec.step > 0) {
    const stepsFromMin = (parsed - spec.min) / spec.step;
    const nearestSteps = Math.round(stepsFromMin);
    if (Math.abs(stepsFromMin - nearestSteps) > 1e-6) {
      return {
        ok: false,
        reason: "off-step",
        message: `Must align to step ${stepText} ${spec.unit} from ${minText} (allowed ${minText}–${maxText} ${spec.unit}).`,
      };
    }
  }

  return {
    ok: true,
    value: clampTuningValue(spec, parsed),
  };
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
    const rawDelta = Number((draftClean - baseClean).toFixed(spec.decimals));
    const delta = Object.is(rawDelta, -0) ? 0 : rawDelta;
    if (Math.abs(delta) > 1e-9) {
      diffs.push({
        key: spec.key,
        label: spec.label,
        unit: spec.unit,
        decimals: spec.decimals,
        min: spec.min,
        max: spec.max,
        step: spec.step,
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

function normalizeTuningMap(
  specs: readonly TuningParamSpec[],
  source: Readonly<Record<string, number>>,
): Record<string, number> {
  const out: Record<string, number> = {};
  for (const spec of specs) {
    const raw = source[spec.key];
    out[spec.key] = clampTuningValue(
      spec,
      isFiniteNumber(raw) ? raw : spec.min,
    );
  }
  return out;
}

function isTuningBaselineSource(value: unknown): value is TuningBaselineSource {
  return value === "domain-default" || value === "vehicle-reported";
}

function isTuningBaselineSeed(
  value: Readonly<Record<string, number>> | TuningBaselineSeed | undefined,
): value is TuningBaselineSeed {
  return (
    typeof value === "object" &&
    value !== null &&
    "baseline" in value &&
    typeof (value as TuningBaselineSeed).baseline === "object" &&
    (value as TuningBaselineSeed).baseline !== null
  );
}

function resolveBaselineInput(
  specs: readonly TuningParamSpec[],
  domain: UxsDomainKind,
  override?: Readonly<Record<string, number>> | TuningBaselineSeed,
): {
  readonly baseline: Record<string, number>;
  readonly baselineSource: TuningBaselineSource;
} {
  if (isTuningBaselineSeed(override)) {
    const source: TuningBaselineSource = isTuningBaselineSource(override.source)
      ? override.source
      : "domain-default";
    return {
      baseline: normalizeTuningMap(specs, override.baseline),
      baselineSource: source,
    };
  }
  if (override !== undefined) {
    return {
      baseline: normalizeTuningMap(specs, override),
      baselineSource: "domain-default",
    };
  }
  return {
    baseline: normalizeTuningMap(specs, defaultTuningBaseline(domain)),
    baselineSource: "domain-default",
  };
}

function validateCommandableVehicleRef(
  vehicle: CommandableVehicleRef | null | undefined,
): { entityId: string; sysid: number; domain: UxsDomainKind } | null {
  if (
    !vehicle ||
    vehicle.poisoned === true ||
    !isUxsDomainKind(vehicle.domain) ||
    typeof vehicle.id !== "string"
  ) {
    return null;
  }
  const parsed = parseVehicleEntityId(vehicle.id);
  if (!parsed || parsed.sysid !== vehicle.sysid) {
    return null;
  }
  return {
    entityId: vehicle.id,
    sysid: parsed.sysid,
    domain: vehicle.domain,
  };
}

/**
 * Session-scoped Diagnostics parameter draft store keyed strictly by canonical
 * 6-part UXS vehicle entity ID (`<org>.<platform>.robotics.uxs.vehicle.<sysid>`).
 * Tracks baseline provenance (`"domain-default"` vs `"vehicle-reported"`),
 * retains dormant drafts across transient `poisoned`/`delete` transitions, and
 * refuses invalid, non-vehicle, poisoned, or domain-mismatched reads/writes (#40, #43).
 */
export function createDiagnosticsDraftStore(
  specs: readonly TuningParamSpec[] = BENCH_TUNING_SPECS,
): DiagnosticsDraftStore {
  const records = new SvelteMap<string, VehicleTuningDraftRecord>();

  return {
    records,

    readVehicle(vehicle) {
      const valid = validateCommandableVehicleRef(vehicle);
      if (!valid) {
        return null;
      }
      const existing = records.get(valid.entityId);
      if (existing && existing.domain === valid.domain) {
        return {
          entityId: existing.entityId,
          sysid: existing.sysid,
          domain: existing.domain,
          baselineSource: existing.baselineSource,
          baseline: existing.baseline,
          staged: existing.staged,
          diffs: computeTuningDiff(specs, existing.baseline, existing.staged),
        };
      }
      const resolved = resolveBaselineInput(specs, valid.domain);
      return {
        entityId: valid.entityId,
        sysid: valid.sysid,
        domain: valid.domain,
        baselineSource: resolved.baselineSource,
        baseline: resolved.baseline,
        staged: { ...resolved.baseline },
        diffs: [],
      };
    },

    getDormantDraft(entityId, knownDomain) {
      if (typeof entityId !== "string") {
        return null;
      }
      const parsed = parseVehicleEntityId(entityId);
      if (!parsed) {
        return null;
      }
      const existing = records.get(entityId);
      if (!existing || existing.sysid !== parsed.sysid) {
        return null;
      }
      if (
        knownDomain !== undefined &&
        knownDomain !== null &&
        existing.domain !== knownDomain
      ) {
        return null;
      }
      const diffs = computeTuningDiff(
        specs,
        existing.baseline,
        existing.staged,
      );
      if (diffs.length === 0) {
        return null;
      }
      return {
        entityId: existing.entityId,
        sysid: existing.sysid,
        domain: existing.domain,
        baselineSource: existing.baselineSource,
        diffs,
      };
    },

    seedVehicleBaseline(vehicle, seed) {
      const valid = validateCommandableVehicleRef(vehicle);
      if (!valid) {
        return null;
      }
      const resolved = resolveBaselineInput(specs, valid.domain, seed);
      const nextRecord: VehicleTuningDraftRecord = {
        entityId: valid.entityId,
        sysid: valid.sysid,
        domain: valid.domain,
        baselineSource: resolved.baselineSource,
        baseline: resolved.baseline,
        staged: { ...resolved.baseline },
      };
      records.set(valid.entityId, nextRecord);
      return {
        ...nextRecord,
        diffs: [],
      };
    },

    stageParam(vehicle, key, rawValue, baselineOverride) {
      const valid = validateCommandableVehicleRef(vehicle);
      if (!valid) {
        return null;
      }
      const existing = records.get(valid.entityId);
      const hasMatchingRecord =
        existing !== undefined && existing.domain === valid.domain;
      const resolved = hasMatchingRecord
        ? {
            baseline: existing.baseline,
            baselineSource: existing.baselineSource,
          }
        : resolveBaselineInput(specs, valid.domain, baselineOverride);
      const priorStaged = hasMatchingRecord
        ? existing.staged
        : resolved.baseline;
      const nextStaged = stageTuningValue(specs, priorStaged, key, rawValue);
      const nextRecord: VehicleTuningDraftRecord = {
        entityId: valid.entityId,
        sysid: valid.sysid,
        domain: valid.domain,
        baselineSource: resolved.baselineSource,
        baseline: resolved.baseline,
        staged: nextStaged,
      };
      records.set(valid.entityId, nextRecord);
      return {
        ...nextRecord,
        diffs: computeTuningDiff(specs, resolved.baseline, nextStaged),
      };
    },

    revertVehicle(entityId) {
      if (typeof entityId !== "string" || !parseVehicleEntityId(entityId)) {
        return false;
      }
      const existing = records.get(entityId);
      if (!existing) {
        return false;
      }
      records.set(entityId, {
        ...existing,
        staged: revertTuningDraft(existing.baseline),
      });
      return true;
    },

    reconcileVehicleDomain(entityId, domain) {
      if (
        typeof entityId !== "string" ||
        !parseVehicleEntityId(entityId) ||
        !isUxsDomainKind(domain)
      ) {
        return;
      }
      const existing = records.get(entityId);
      if (existing && existing.domain !== domain) {
        records.delete(entityId);
      }
    },

    clearAll() {
      records.clear();
    },
  };
}

export interface WithheldTuningExplanationInput {
  readonly selectedEntityId: string | null;
  readonly selectedEntityKind:
    "uxs-vehicle" | "external-contact" | "generic" | null;
  readonly selectedEntityLabel: string | null;
  readonly selectedEntityPoisoned: boolean;
  readonly selectedVehicleSysId: number | null;
  readonly skgSynced: boolean;
  readonly skgLastSyncedRevision: number | null;
}

export function describeWithheldTuningState(
  input: WithheldTuningExplanationInput,
): string {
  if (input.selectedEntityKind === "external-contact") {
    return "External COP contacts expose no parameter tuning rack.";
  }
  const poisonedSysId =
    input.selectedVehicleSysId ??
    (input.selectedEntityPoisoned && input.selectedEntityId
      ? (parseVehicleEntityId(input.selectedEntityId)?.sysid ?? null)
      : null);
  if (input.selectedEntityPoisoned && poisonedSysId !== null) {
    return `UXS #${poisonedSysId} is POISONED — parameter staging is withheld.`;
  }
  if (input.selectedEntityKind === "generic" && input.selectedEntityId) {
    const label = input.selectedEntityLabel ?? input.selectedEntityId;
    return `Selected entity (${label}) is not a commandable UXS vehicle — parameter staging is unavailable.`;
  }
  if (input.selectedEntityId !== null && input.selectedEntityKind === null) {
    if (!input.skgSynced && input.skgLastSyncedRevision === null) {
      return `Awaiting Fleet data sync for ${input.selectedEntityId} — parameter staging is withheld until vehicle state is verified.`;
    }
    const parsedVehicle = parseVehicleEntityId(input.selectedEntityId);
    if (parsedVehicle) {
      return `UXS #${parsedVehicle.sysid} (${input.selectedEntityId}) is unavailable in live Fleet data — parameter staging is withheld.`;
    }
    return `Selected entity (${input.selectedEntityId}) is unavailable in live Fleet data — parameter staging applies only to active commandable UXS vehicles.`;
  }
  return "Select a commandable UXS vehicle to stage sample control and deadman parameters locally.";
}
