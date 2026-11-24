// SPDX-License-Identifier: MIT
// Copyright (c) 2026 boxbotix

export const MIN_VEHICLE_SYSID = 1;
export const MAX_VEHICLE_SYSID = 254;
export const MAX_ENTITY_ID_BYTES = 256;

export const UXS_DOMAIN_KINDS = [
  "quadcopter",
  "rover",
  "surface-boat",
] as const;
export type UxsDomainKind = (typeof UXS_DOMAIN_KINDS)[number];

export const CONTROL_AUTHORITIES = [
  "direct-crsf-override",
  "companion-teleop",
  "companion-autonomous",
  "mcu-failsafe",
] as const;
export type ControlAuthority = (typeof CONTROL_AUTHORITIES)[number];

export const COMPANION_MODES = [
  "disarmed",
  "teleop-velocity",
  "autonomous-velocity",
  "hold-position",
] as const;
export type CompanionMode = (typeof COMPANION_MODES)[number];

export const LINK_TIERS = [
  "sitl-local",
  "direct-bench",
  "mesh-high-rate",
  "mesh-supervisory",
] as const;
export type LinkTier = (typeof LINK_TIERS)[number];

export interface HealthFlags {
  clock_sync_locked: boolean;
  teleop_deadman_clamped: boolean;
  crsf_link_healthy: boolean;
  companion_trunk_healthy: boolean;
}

export interface ParsedVehicleEntityId {
  org: string;
  platform: string;
  sysid: number;
}

const CANONICAL_DECIMAL_RE = /^(?:[1-9]\d*)$/;
const ENTITY_ID_SEGMENT_RE = /^[A-Za-z0-9][A-Za-z0-9_-]*$/;
const CANONICAL_ENTITY_ID_PARTS = 6;

export function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === "object" && value !== null && !Array.isArray(value);
}

export function isFiniteNumber(value: unknown): value is number {
  return typeof value === "number" && Number.isFinite(value);
}

export function isNonNegativeInt(value: unknown): value is number {
  return (
    typeof value === "number" &&
    Number.isInteger(value) &&
    Number.isFinite(value) &&
    value >= 0
  );
}

export function isValidVehicleSysId(value: unknown): value is number {
  return (
    typeof value === "number" &&
    Number.isInteger(value) &&
    value >= MIN_VEHICLE_SYSID &&
    value <= MAX_VEHICLE_SYSID
  );
}

export function parseCanonicalSysIdSegment(segment: string): number | null {
  if (!CANONICAL_DECIMAL_RE.test(segment)) {
    return null;
  }
  const parsed = Number(segment);
  return isValidVehicleSysId(parsed) ? parsed : null;
}

export function parseSixPartEntityId(
  id: unknown,
): [string, string, string, string, string, string] | null {
  if (typeof id !== "string") {
    return null;
  }
  if (id.length === 0 || id.length > MAX_ENTITY_ID_BYTES) {
    return null;
  }
  const parts = id.split(".");
  if (parts.length !== CANONICAL_ENTITY_ID_PARTS) {
    return null;
  }
  for (const part of parts) {
    if (!ENTITY_ID_SEGMENT_RE.test(part)) {
      return null;
    }
  }
  return parts as [string, string, string, string, string, string];
}

export function isValidEntityIdString(value: unknown): value is string {
  return parseSixPartEntityId(value) !== null;
}

export function parseVehicleEntityId(id: string): ParsedVehicleEntityId | null {
  const parts = parseSixPartEntityId(id);
  if (!parts) {
    return null;
  }
  const [org, platform, system, domain, entityType, instance] = parts;
  if (system !== "robotics" || domain !== "uxs" || entityType !== "vehicle") {
    return null;
  }
  const sysid = parseCanonicalSysIdSegment(instance);
  if (sysid === null) {
    return null;
  }
  return { org, platform, sysid };
}

export function isUxsDomainKind(value: unknown): value is UxsDomainKind {
  return (
    typeof value === "string" &&
    (UXS_DOMAIN_KINDS as readonly string[]).includes(value)
  );
}

export function isControlAuthority(value: unknown): value is ControlAuthority {
  return (
    typeof value === "string" &&
    (CONTROL_AUTHORITIES as readonly string[]).includes(value)
  );
}

export function isCompanionMode(value: unknown): value is CompanionMode {
  return (
    typeof value === "string" &&
    (COMPANION_MODES as readonly string[]).includes(value)
  );
}

export function isLinkTier(value: unknown): value is LinkTier {
  return (
    typeof value === "string" &&
    (LINK_TIERS as readonly string[]).includes(value)
  );
}

export function parseHealthFlags(value: unknown): HealthFlags | null {
  if (!isRecord(value)) {
    return null;
  }
  const {
    clock_sync_locked,
    teleop_deadman_clamped,
    crsf_link_healthy,
    companion_trunk_healthy,
  } = value;
  if (
    typeof clock_sync_locked !== "boolean" ||
    typeof teleop_deadman_clamped !== "boolean" ||
    typeof crsf_link_healthy !== "boolean" ||
    typeof companion_trunk_healthy !== "boolean"
  ) {
    return null;
  }
  return {
    clock_sync_locked,
    teleop_deadman_clamped,
    crsf_link_healthy,
    companion_trunk_healthy,
  };
}

export function parseFiniteTuple3(
  value: unknown,
): [number, number, number] | null {
  if (
    !Array.isArray(value) ||
    value.length !== 3 ||
    !isFiniteNumber(value[0]) ||
    !isFiniteNumber(value[1]) ||
    !isFiniteNumber(value[2])
  ) {
    return null;
  }
  return [value[0], value[1], value[2]];
}

export function parseFiniteTuple4(
  value: unknown,
): [number, number, number, number] | null {
  if (
    !Array.isArray(value) ||
    value.length !== 4 ||
    !isFiniteNumber(value[0]) ||
    !isFiniteNumber(value[1]) ||
    !isFiniteNumber(value[2]) ||
    !isFiniteNumber(value[3])
  ) {
    return null;
  }
  return [value[0], value[1], value[2], value[3]];
}

export function isValidHeadingDeg(value: unknown): value is number {
  return isFiniteNumber(value) && value >= 0 && value <= 360;
}

export function isValidLatLon(lat: unknown, lon: unknown): boolean {
  return (
    isFiniteNumber(lat) &&
    isFiniteNumber(lon) &&
    lat >= -90 &&
    lat <= 90 &&
    lon >= -180 &&
    lon <= 180
  );
}
