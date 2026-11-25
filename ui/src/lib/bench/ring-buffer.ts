// SPDX-License-Identifier: MIT
// Copyright (c) 2026 boxbotix

import { isFiniteNumber } from "../uxs";
import type { VehicleSysStatus } from "../stores/skg.svelte";
import type { VehicleKinematics } from "../stores/telemetry.svelte";

const RAD_TO_DEG = 180 / Math.PI;

/** 15 seconds at 20 Hz fast-lane WS #1 telemetry. */
export const DEFAULT_TRACE_CAPACITY = 300;

export interface TelemetrySample {
  readonly timestamp: number;
  readonly rollRateDegS: number | null;
  readonly pitchRateDegS: number | null;
  readonly yawRateDegS: number | null;
  readonly groundspeedMps: number | null;
  readonly climbRateMps: number | null;
  readonly loopDtUs: number | null;
  readonly rttUs: number | null;
}

function encodeOptionalFinite(
  value: number | null | undefined,
  clampNonNegative = false,
): number {
  if (value === null || value === undefined || !isFiniteNumber(value)) {
    return Number.NaN;
  }
  const cleaned = Object.is(value, -0) ? 0 : value;
  return clampNonNegative ? Math.max(0, cleaned) : cleaned;
}

function decodeOptionalFinite(stored: number | undefined): number | null {
  return stored !== undefined && Number.isFinite(stored) ? stored : null;
}

/**
 * Fixed-capacity typed-array circular buffer for a single UXS vehicle's 20 Hz
 * telemetry traces. Mutates pre-allocated Float64Arrays outside Svelte $state
 * so 20 Hz WS #1 frames never trigger reactive DOM diffing. Unavailable
 * channels are stored in-place as IEEE-754 NaN and decoded as `null`.
 */
export class VehicleRingBuffer {
  readonly capacity: number;
  private readonly timestamps: Float64Array;
  private readonly rollRates: Float64Array;
  private readonly pitchRates: Float64Array;
  private readonly yawRates: Float64Array;
  private readonly groundspeeds: Float64Array;
  private readonly climbRates: Float64Array;
  private readonly loopDts: Float64Array;
  private readonly rtts: Float64Array;

  private head = 0;
  private count = 0;
  private latestTimestamp = -Infinity;
  private droppedCount = 0;

  constructor(capacity: number = DEFAULT_TRACE_CAPACITY) {
    if (!Number.isInteger(capacity) || capacity < 1) {
      throw new RangeError(`invalid ring buffer capacity ${capacity}`);
    }
    this.capacity = capacity;
    this.timestamps = new Float64Array(capacity);
    this.rollRates = new Float64Array(capacity);
    this.pitchRates = new Float64Array(capacity);
    this.yawRates = new Float64Array(capacity);
    this.groundspeeds = new Float64Array(capacity);
    this.climbRates = new Float64Array(capacity);
    this.loopDts = new Float64Array(capacity);
    this.rtts = new Float64Array(capacity);
  }

  get size(): number {
    return this.count;
  }

  get lastTimestamp(): number | null {
    return this.count > 0 ? this.latestTimestamp : null;
  }

  get droppedOutOfOrderCount(): number {
    return this.droppedCount;
  }

  push(sample: TelemetrySample): boolean {
    if (!isFiniteNumber(sample.timestamp)) {
      this.droppedCount += 1;
      return false;
    }
    if (this.count > 0 && sample.timestamp <= this.latestTimestamp) {
      this.droppedCount += 1;
      return false;
    }

    const slot = this.head;
    this.timestamps[slot] = sample.timestamp;
    this.rollRates[slot] = encodeOptionalFinite(sample.rollRateDegS);
    this.pitchRates[slot] = encodeOptionalFinite(sample.pitchRateDegS);
    this.yawRates[slot] = encodeOptionalFinite(sample.yawRateDegS);
    this.groundspeeds[slot] = encodeOptionalFinite(sample.groundspeedMps, true);
    this.climbRates[slot] = encodeOptionalFinite(sample.climbRateMps);
    this.loopDts[slot] = encodeOptionalFinite(sample.loopDtUs, true);
    this.rtts[slot] = encodeOptionalFinite(sample.rttUs, true);

    this.head = (slot + 1) % this.capacity;
    if (this.count < this.capacity) {
      this.count += 1;
    }
    this.latestTimestamp = sample.timestamp;
    return true;
  }

  forEachSample(
    visitor: (sample: TelemetrySample, index: number, total: number) => void,
  ): void {
    const total = this.count;
    if (total === 0) {
      return;
    }
    const start = total < this.capacity ? 0 : this.head;
    for (let i = 0; i < total; i += 1) {
      const slot = (start + i) % this.capacity;
      visitor(
        {
          timestamp: this.timestamps[slot] ?? 0,
          rollRateDegS: decodeOptionalFinite(this.rollRates[slot]),
          pitchRateDegS: decodeOptionalFinite(this.pitchRates[slot]),
          yawRateDegS: decodeOptionalFinite(this.yawRates[slot]),
          groundspeedMps: decodeOptionalFinite(this.groundspeeds[slot]),
          climbRateMps: decodeOptionalFinite(this.climbRates[slot]),
          loopDtUs: decodeOptionalFinite(this.loopDts[slot]),
          rttUs: decodeOptionalFinite(this.rtts[slot]),
        },
        i,
        total,
      );
    }
  }

  toChronologicalSamples(): TelemetrySample[] {
    const out: TelemetrySample[] = [];
    this.forEachSample((sample) => {
      out.push(sample);
    });
    return out;
  }

  clear(): void {
    this.head = 0;
    this.count = 0;
    this.latestTimestamp = -Infinity;
  }
}

export interface TelemetryIngressSource {
  kinematicsEntries(): IterableIterator<[number, VehicleKinematics]>;
  onKinematicsAccepted(
    listener: (frame: VehicleKinematics) => void,
  ): () => void;
  onFeedReset?(listener: () => void): () => void;
}

type SysStatusLookup = (
  sysid: number,
) => Pick<VehicleSysStatus, "loopDtUs" | "rttUs"> | null;

export class TelemetryTraceBook {
  readonly capacityPerVehicle: number;
  private readonly buffers = new Map<number, VehicleRingBuffer>();
  private readonly attachments = new Map<
    TelemetryIngressSource,
    {
      refCount: number;
      lookupSysStatus?: SysStatusLookup;
      unsubscribe: () => void;
    }
  >();

  constructor(capacityPerVehicle: number = DEFAULT_TRACE_CAPACITY) {
    this.capacityPerVehicle = capacityPerVehicle;
  }

  getOrCreateBuffer(sysid: number): VehicleRingBuffer {
    let buf = this.buffers.get(sysid);
    if (!buf) {
      buf = new VehicleRingBuffer(this.capacityPerVehicle);
      this.buffers.set(sysid, buf);
    }
    return buf;
  }

  getBuffer(sysid: number): VehicleRingBuffer | undefined {
    return this.buffers.get(sysid);
  }

  recordKinematics(
    kinematics: VehicleKinematics,
    sysStatus?: Pick<VehicleSysStatus, "loopDtUs" | "rttUs"> | null,
  ): boolean {
    const buf = this.getOrCreateBuffer(kinematics.sysid);
    const hasLocal = kinematics.has_local_pos === true;
    const hasAtt = kinematics.has_attitude === true;
    const hasTiming =
      sysStatus !== null &&
      sysStatus !== undefined &&
      isFiniteNumber(sysStatus.loopDtUs) &&
      isFiniteNumber(sysStatus.rttUs);

    return buf.push({
      timestamp: kinematics.timestamp,
      rollRateDegS:
        hasAtt && isFiniteNumber(kinematics.body_rates[0])
          ? kinematics.body_rates[0] * RAD_TO_DEG
          : null,
      pitchRateDegS:
        hasAtt && isFiniteNumber(kinematics.body_rates[1])
          ? kinematics.body_rates[1] * RAD_TO_DEG
          : null,
      yawRateDegS:
        hasAtt && isFiniteNumber(kinematics.body_rates[2])
          ? kinematics.body_rates[2] * RAD_TO_DEG
          : null,
      groundspeedMps:
        hasLocal &&
        isFiniteNumber(kinematics.ned_vel[0]) &&
        isFiniteNumber(kinematics.ned_vel[1])
          ? Math.hypot(kinematics.ned_vel[0], kinematics.ned_vel[1])
          : null,
      climbRateMps:
        hasLocal && isFiniteNumber(kinematics.ned_vel[2])
          ? -kinematics.ned_vel[2]
          : null,
      loopDtUs: hasTiming ? sysStatus.loopDtUs : null,
      rttUs: hasTiming ? sysStatus.rttUs : null,
    });
  }

  /**
   * Subscribes idempotently (reference-counted per TelemetryStore) to accepted
   * WS #1 kinematics frames at ingress so animation frames never sample or drop
   * intermediate arrivals.
   */
  attach(
    source: TelemetryIngressSource,
    lookupSysStatus?: SysStatusLookup,
  ): () => void {
    const existing = this.attachments.get(source);
    if (existing) {
      existing.refCount += 1;
      if (lookupSysStatus) {
        existing.lookupSysStatus = lookupSysStatus;
      }
      let detached = false;
      return () => {
        if (detached) return;
        detached = true;
        existing.refCount -= 1;
        if (existing.refCount <= 0) {
          this.attachments.delete(source);
          existing.unsubscribe();
        }
      };
    }

    const entry: {
      refCount: number;
      lookupSysStatus?: SysStatusLookup;
      unsubscribe: () => void;
    } = {
      refCount: 1,
      lookupSysStatus,
      unsubscribe: () => {},
    };

    for (const [sysid, kin] of source.kinematicsEntries()) {
      if ((this.buffers.get(sysid)?.size ?? 0) === 0) {
        this.recordKinematics(kin, entry.lookupSysStatus?.(sysid) ?? null);
      }
    }

    const offAccepted = source.onKinematicsAccepted((frame) => {
      this.recordKinematics(
        frame,
        entry.lookupSysStatus?.(frame.sysid) ?? null,
      );
    });
    const offReset = source.onFeedReset?.(() => {
      this.clear();
    });
    entry.unsubscribe = () => {
      offAccepted();
      offReset?.();
    };
    this.attachments.set(source, entry);

    let detached = false;
    return () => {
      if (detached) return;
      detached = true;
      entry.refCount -= 1;
      if (entry.refCount <= 0) {
        this.attachments.delete(source);
        entry.unsubscribe();
      }
    };
  }

  clear(): void {
    this.buffers.clear();
  }
}
