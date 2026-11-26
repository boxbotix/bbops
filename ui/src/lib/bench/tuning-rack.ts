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

export interface VehicleTuningDraftRecord {
  readonly entityId: string;
  readonly sysid: number;
  readonly domain: UxsDomainKind;
  readonly baseline: Readonly<Record<string, number>>;
  readonly staged: Readonly<Record<string, number>>;
}

export interface VehicleTuningView {
  readonly entityId: string;
  readonly sysid: number;
  readonly domain: UxsDomainKind;
  readonly baseline: Readonly<Record<string, number>>;
  readonly staged: Readonly<Record<string, number>>;
  readonly diffs: readonly TuningDiffEntry[];
}

export interface DormantVehicleDraftView {
  readonly entityId: string;
  readonly sysid: number;
  readonly domain: UxsDomainKind;
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
  stageParam(
    vehicle: CommandableVehicleRef | null | undefined,
    key: string,
    rawValue: number,
    baselineOverride?: Readonly<Record<string, number>>,
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
 * Retains dormant drafts across transient `poisoned`/`delete` transitions while
 * refusing invalid, non-vehicle, poisoned, or domain-mismatched reads/writes (#40).
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
          baseline: existing.baseline,
          staged: existing.staged,
          diffs: computeTuningDiff(specs, existing.baseline, existing.staged),
        };
      }
      const baseline = normalizeTuningMap(
        specs,
        defaultTuningBaseline(valid.domain),
      );
      return {
        entityId: valid.entityId,
        sysid: valid.sysid,
        domain: valid.domain,
        baseline,
        staged: { ...baseline },
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
        diffs,
      };
    },

    stageParam(vehicle, key, rawValue, baselineOverride) {
      const valid = validateCommandableVehicleRef(vehicle);
      if (!valid) {
        return null;
      }
      const existing = records.get(valid.entityId);
      const baseline =
        existing && existing.domain === valid.domain
          ? existing.baseline
          : normalizeTuningMap(
              specs,
              baselineOverride ?? defaultTuningBaseline(valid.domain),
            );
      const priorStaged =
        existing && existing.domain === valid.domain
          ? existing.staged
          : baseline;
      const nextStaged = stageTuningValue(specs, priorStaged, key, rawValue);
      const nextRecord: VehicleTuningDraftRecord = {
        entityId: valid.entityId,
        sysid: valid.sysid,
        domain: valid.domain,
        baseline,
        staged: nextStaged,
      };
      records.set(valid.entityId, nextRecord);
      return {
        ...nextRecord,
        diffs: computeTuningDiff(specs, baseline, nextStaged),
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
  return "Select a commandable UXS vehicle to stage control and deadman parameters.";
}
