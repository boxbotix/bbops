// SPDX-License-Identifier: MIT
// Copyright (c) 2026 boxbotix

import { untrack } from "svelte";
import { SvelteMap } from "svelte/reactivity";
import type { FleetFreshnessContext } from "../hud/view-model";
import { evaluateVehicleFreshness } from "../hud/view-model";
import type {
  CopEntityState,
  SkgStore,
  SkgTriple,
  VehicleEntityState,
} from "../stores/skg.svelte";
import type {
  TelemetryStore,
  TransitionEvent,
} from "../stores/telemetry.svelte";
import {
  type CompanionMode,
  type ControlAuthority,
  type LinkTier,
  isFiniteNumber,
  isRecord,
  parseSixPartEntityId,
} from "../uxs";

export const CONTROL_COMMAND_PATH = "/control/command";
export const CONTROL_TELEOP_PATH = "/control/teleop";

export const DEFAULT_TELEOP_INTERVAL_MS = 50; // 20 Hz within [10..30 Hz] contract
export const DEFAULT_TELEOP_LEASE_TTL_MS = 240; // Conservative match for server 240 ms #84 lease TTL
export const DEFAULT_MAX_LINEAR_MPS = 4.0;
export const DEFAULT_MAX_VERTICAL_MPS = 2.0;
export const DEFAULT_MAX_YAW_RAD_S = 1.2;

export const COMMIT_UNKNOWN_BANNER_TEXT =
  "Status unconfirmed — awaiting vehicle heartbeat";

export const DISCRETE_COMMAND_PHASES = [
  "pending",
  "confirmed",
  "rejected",
  "commit_unknown",
  "preempted",
] as const;
export type DiscreteCommandPhase = (typeof DISCRETE_COMMAND_PHASES)[number];

export const DISCRETE_COMMAND_ACTIONS = [
  "arm",
  "disarm",
  "hold-position",
  "mode-teleop",
  "mode-autonomous",
  "sitl-companion-blackout",
  "sitl-crsf-override",
  "sitl-cross-current",
] as const;
export type DiscreteCommandAction = (typeof DISCRETE_COMMAND_ACTIONS)[number];

export const TELEOP_FRAMES = ["local-ned", "body-ned"] as const;
export type TeleopFrame = (typeof TELEOP_FRAMES)[number];

export interface TeleopAxes {
  readonly vx: number;
  readonly vy: number;
  readonly vz: number;
  readonly yawRate: number;
}

export const ZERO_TELEOP_AXES: TeleopAxes = Object.freeze({
  vx: 0,
  vy: 0,
  vz: 0,
  yawRate: 0,
});

export interface ControlCommandRequest {
  readonly sysid: number;
  readonly action: DiscreteCommandAction;
  readonly param1?: number;
  readonly param2?: number;
  readonly param3?: number;
}

export interface ControlCommandWireResult {
  readonly command_id: string;
  readonly sysid: number;
  readonly cmd_id: number;
  readonly action: string;
  readonly phase: DiscreteCommandPhase;
  readonly detail: string;
  readonly issued_ms: number;
}

export interface ControlTeleopRequest {
  readonly sysid: number;
  readonly session?: string;
  readonly seq?: number;
  readonly frame: TeleopFrame;
  readonly vx: number;
  readonly vy: number;
  readonly vz: number;
  readonly yaw_rate: number;
  readonly engage?: boolean;
  readonly stop?: boolean;
  readonly observed_ms?: number;
  readonly observation_rev?: number;
  readonly authority_incarnation?: string;
  readonly authority_epoch?: number;
}

export interface ControlTeleopWireResult {
  readonly sysid: number;
  readonly lease_active: boolean;
  readonly expires_ms?: number;
}

export interface ControlErrorDetail {
  readonly code: string;
  readonly message: string;
  readonly status: number;
}

export type ControlResponse<T> =
  | { readonly ok: true; readonly result: T }
  | { readonly ok: false; readonly error: ControlErrorDetail };

export interface ControlClient {
  submitCommand(
    req: ControlCommandRequest,
  ): Promise<ControlResponse<ControlCommandWireResult>>;
  submitTeleop(
    req: ControlTeleopRequest,
  ): Promise<ControlResponse<ControlTeleopWireResult>>;
}

export interface CreateControlClientOptions {
  readonly baseUrl?: string;
  readonly fetchImpl?: typeof fetch;
}

export interface VehicleCommandRecord {
  readonly id: string;
  readonly sysid: number;
  readonly cmdId: number;
  readonly action: string;
  readonly phase: DiscreteCommandPhase;
  readonly ackResult: number;
  readonly detail: string;
  readonly issuedMs: number;
  readonly updatedMs: number;
  readonly kvRevision: number;
}

export interface LiveActuationSafety {
  readonly sysid: number;
  readonly armed: boolean;
  readonly authority: ControlAuthority;
  readonly companionMode: CompanionMode;
  readonly linkTier: LinkTier;
  readonly linkLost: boolean;
  readonly fresh: boolean;
  readonly statusLabel: string;
  readonly stateObservedMs: number | null;
  readonly observationRev?: number;
  readonly authorityIncarnation?: string;
  readonly authorityEpoch?: number;
  readonly isSupervisoryOnly: boolean;
  readonly isSitlLocal: boolean;
  readonly isHardwarePreempted: boolean;
  readonly canEngageTeleop: boolean;
  readonly canIssueStandardCommand: boolean;
  readonly canEmergencyDisarm: boolean;
}

export interface PreemptionLatch {
  readonly sysid: number;
  readonly source: ControlAuthority | "link-lost";
  readonly message: string;
  readonly latchedAtMs: number;
}

export interface PadKeyEventLike {
  readonly key: string;
  readonly code?: string;
  readonly repeat?: boolean;
  readonly ctrlKey?: boolean;
  readonly metaKey?: boolean;
  readonly altKey?: boolean;
  preventDefault?(): void;
  stopPropagation?(): void;
}

export interface CreateActuationControllerOptions {
  readonly telemetryStore: TelemetryStore;
  readonly skgStore: SkgStore;
  readonly client?: ControlClient;
  readonly teleopIntervalMs?: number;
  readonly leaseTtlMs?: number;
  readonly maxLinearMps?: number;
  readonly maxVerticalMps?: number;
  readonly maxYawRadS?: number;
  readonly now?: () => number;
  readonly perfNow?: () => number;
  readonly setIntervalFn?: (
    cb: () => void,
    ms: number,
  ) => ReturnType<typeof setInterval>;
  readonly clearIntervalFn?: (handle: ReturnType<typeof setInterval>) => void;
}

export interface ActuationController {
  readonly activeSysId: number | null;
  readonly engaged: boolean;
  readonly engagedSysId: number | null;
  readonly leaseRenewalPending: boolean;
  readonly frame: TeleopFrame;
  readonly axes: TeleopAxes;
  readonly padNormX: number;
  readonly padNormY: number;
  readonly pointerDragging: boolean;
  readonly submittingAction: DiscreteCommandAction | null;
  readonly lastError: string | null;
  readonly lastPreemptLatencyMs: number | null;
  readonly teleopPacketsSent: number;
  readonly commandRequestsSent: number;
  readonly latestCommand: VehicleCommandRecord | null;
  readonly preemptionLatch: PreemptionLatch | null;
  setFrame(nextFrame: TeleopFrame): void;
  reconcileVehicle(
    vehicle: VehicleEntityState | null,
    freshnessContext?: FleetFreshnessContext,
  ): LiveActuationSafety | null;
  engageTeleop(
    vehicle: VehicleEntityState | null,
    freshnessContext?: FleetFreshnessContext,
  ): Promise<boolean>;
  disengageTeleop(options?: { sendZero?: boolean }): Promise<void>;
  setPadVector(normX: number, normY: number, dragging: boolean): void;
  releasePadPointer(): void;
  handlePadKeyDown(event: PadKeyEventLike): boolean;
  handlePadKeyUp(event: PadKeyEventLike): boolean;
  handleFocusLoss(): void;
  sendDiscreteCommand(
    vehicle: VehicleEntityState | null,
    action: DiscreteCommandAction,
    params?: { param1?: number; param2?: number; param3?: number },
    freshnessContext?: FleetFreshnessContext,
  ): Promise<ControlResponse<ControlCommandWireResult>>;
  clearError(): void;
  dispose(): void;
}

export function isDiscreteCommandPhase(
  value: unknown,
): value is DiscreteCommandPhase {
  return (
    typeof value === "string" &&
    (DISCRETE_COMMAND_PHASES as readonly string[]).includes(value)
  );
}

function clampUnit(v: number): number {
  if (!Number.isFinite(v)) {
    return 0;
  }
  if (v < -1) {
    return -1;
  }
  if (v > 1) {
    return 1;
  }
  return v;
}

function roundAxis(v: number): number {
  if (!Number.isFinite(v) || Math.abs(v) < 1e-6) {
    return 0;
  }
  return Math.round(v * 1000) / 1000;
}

async function postControlJson<T>(
  fetchImpl: typeof fetch,
  url: string,
  payload: unknown,
): Promise<ControlResponse<T>> {
  let resp: Response;
  try {
    resp = await fetchImpl(url, {
      method: "POST",
      headers: {
        "Content-Type": "application/json",
      },
      body: JSON.stringify(payload),
    });
  } catch (err) {
    return {
      ok: false,
      error: {
        code: "network_error",
        message:
          err instanceof Error ? err.message : "Control HTTP request failed",
        status: 0,
      },
    };
  }

  let parsed: unknown = null;
  try {
    parsed = await resp.json();
  } catch {
    parsed = null;
  }

  if (
    resp.ok &&
    isRecord(parsed) &&
    parsed.ok === true &&
    isRecord(parsed.result)
  ) {
    return {
      ok: true,
      result: parsed.result as unknown as T,
    };
  }

  if (isRecord(parsed) && isRecord(parsed.error)) {
    const code =
      typeof parsed.error.code === "string" && parsed.error.code.length > 0
        ? parsed.error.code
        : `http_${resp.status}`;
    const message =
      typeof parsed.error.message === "string" &&
      parsed.error.message.length > 0
        ? parsed.error.message
        : `Control request failed (${resp.status})`;
    return {
      ok: false,
      error: {
        code,
        message,
        status: resp.status,
      },
    };
  }

  return {
    ok: false,
    error: {
      code: `http_${resp.status}`,
      message: `Control request failed (${resp.status})`,
      status: resp.status,
    },
  };
}

export function createControlClient(
  options: CreateControlClientOptions = {},
): ControlClient {
  const baseUrl = (options.baseUrl ?? "").replace(/\/+$/, "");
  const fetchImpl: typeof fetch =
    options.fetchImpl ?? ((input, init) => fetch(input, init));

  return {
    submitCommand(req) {
      return postControlJson<ControlCommandWireResult>(
        fetchImpl,
        `${baseUrl}${CONTROL_COMMAND_PATH}`,
        req,
      );
    },
    submitTeleop(req) {
      return postControlJson<ControlTeleopWireResult>(
        fetchImpl,
        `${baseUrl}${CONTROL_TELEOP_PATH}`,
        req,
      );
    },
  };
}

function parseCommandTripleMap(
  triples: readonly SkgTriple[],
): Map<string, unknown> {
  // eslint-disable-next-line svelte/prefer-svelte-reactivity
  const out = new Map<string, unknown>();
  for (const triple of triples) {
    out.set(triple.predicate, triple.object);
  }
  return out;
}

export function parseVehicleCommandEntity(
  entity: CopEntityState,
): VehicleCommandRecord | null {
  if (entity.poisoned) {
    return null;
  }
  const parts = parseSixPartEntityId(entity.id);
  if (
    !parts ||
    parts[2] !== "robotics" ||
    parts[3] !== "uxs" ||
    parts[4] !== "command"
  ) {
    return null;
  }
  const map = parseCommandTripleMap(entity.triples);
  const sysid = map.get("uxs.command.sysid");
  const cmdId = map.get("uxs.command.cmdid");
  const action = map.get("uxs.command.action");
  const phase = map.get("uxs.command.phase");
  const ackResult = map.get("uxs.command.ackresult");
  const detail = map.get("uxs.command.detail");
  const issuedMs = map.get("uxs.command.issuedms");
  const updatedMs = map.get("uxs.command.updatedms");

  if (
    !isFiniteNumber(sysid) ||
    sysid < 1 ||
    sysid > 254 ||
    !isFiniteNumber(cmdId) ||
    typeof action !== "string" ||
    action.length === 0 ||
    !isDiscreteCommandPhase(phase)
  ) {
    return null;
  }

  return {
    id: entity.id,
    sysid: Math.trunc(sysid),
    cmdId: Math.trunc(cmdId),
    action,
    phase,
    ackResult: isFiniteNumber(ackResult) ? Math.trunc(ackResult) : -1,
    detail:
      typeof detail === "string" && detail.length > 0
        ? detail
        : phase === "commit_unknown"
          ? COMMIT_UNKNOWN_BANNER_TEXT
          : "",
    issuedMs: isFiniteNumber(issuedMs) ? Math.trunc(issuedMs) : 0,
    updatedMs: isFiniteNumber(updatedMs) ? Math.trunc(updatedMs) : 0,
    kvRevision: entity.kvRevision,
  };
}

export function extractLatestVehicleCommand(
  entities: Iterable<CopEntityState>,
  sysid: number | null | undefined,
): VehicleCommandRecord | null {
  if (!isFiniteNumber(sysid) || sysid < 1 || sysid > 254) {
    return null;
  }
  let best: VehicleCommandRecord | null = null;
  for (const entity of entities) {
    const parsed = parseVehicleCommandEntity(entity);
    if (!parsed || parsed.sysid !== sysid) {
      continue;
    }
    if (!best) {
      best = parsed;
      continue;
    }
    if (parsed.issuedMs !== best.issuedMs) {
      if (parsed.issuedMs > best.issuedMs) {
        best = parsed;
      }
      continue;
    }
    if (parsed.updatedMs !== best.updatedMs) {
      if (parsed.updatedMs > best.updatedMs) {
        best = parsed;
      }
      continue;
    }
    if (parsed.kvRevision !== best.kvRevision) {
      if (parsed.kvRevision > best.kvRevision) {
        best = parsed;
      }
      continue;
    }
    if (parsed.id > best.id) {
      best = parsed;
    }
  }
  return best;
}

function findLatestControlTransitionEvent(
  events: readonly TransitionEvent[],
  sysid: number,
): TransitionEvent | null {
  for (let i = events.length - 1; i >= 0; i -= 1) {
    const ev = events[i];
    if (!ev || ev.sysid !== sysid) {
      continue;
    }
    if (
      ev.kind === "heartbeat_edge" ||
      ev.kind === "failsafe_increment" ||
      ev.kind === "link_lost" ||
      ev.kind === "link_restored"
    ) {
      return ev;
    }
  }
  return null;
}

export function resolveLiveActuationSafety(
  vehicle: VehicleEntityState,
  telemetryStore: TelemetryStore,
  skgStore: SkgStore,
  freshnessContext?: FleetFreshnessContext,
  fallbackNowMs?: number,
): LiveActuationSafety {
  const linkState =
    freshnessContext?.telemetryLink ??
    freshnessContext?.getTelemetryLink?.(vehicle.sysid) ??
    telemetryStore.getLinkState(vehicle.sysid);
  const freshness = evaluateVehicleFreshness(vehicle, {
    telemetryLink: linkState,
    telemetryStatus: freshnessContext?.telemetryStatus ?? telemetryStore.status,
    skgStatus: freshnessContext?.skgStatus ?? skgStore.status,
    skgSynced: freshnessContext?.skgSynced ?? skgStore.synced,
    nowMs: freshnessContext?.nowMs ?? fallbackNowMs,
  });

  const latestEv = findLatestControlTransitionEvent(
    telemetryStore.events,
    vehicle.sysid,
  );
  const skgStateMs = vehicle.stateObservedAt
    ? Date.parse(vehicle.stateObservedAt)
    : Number.NaN;
  const fallbackObsIso = vehicle.stateObservedAt || vehicle.observedAt;
  const fallbackObsMs = fallbackObsIso
    ? Date.parse(fallbackObsIso)
    : Number.NaN;
  const useEventOverlay =
    latestEv !== null &&
    (Number.isNaN(skgStateMs) || latestEv.timestamp >= skgStateMs);
  const stateObservedMs = useEventOverlay
    ? latestEv.timestamp
    : Number.isFinite(fallbackObsMs)
      ? fallbackObsMs
      : null;
  const authorityIncarnation =
    useEventOverlay &&
    latestEv.authority_incarnation !== undefined &&
    latestEv.authority_incarnation.length > 0
      ? latestEv.authority_incarnation
      : undefined;
  const authorityEpoch =
    useEventOverlay &&
    latestEv.authority_epoch !== undefined &&
    latestEv.authority_epoch > 0
      ? latestEv.authority_epoch
      : undefined;
  const observationRev =
    useEventOverlay &&
    latestEv.observation_rev !== undefined &&
    latestEv.observation_rev > 0
      ? latestEv.observation_rev
      : undefined;

  const armed = useEventOverlay ? latestEv.armed : vehicle.armed;
  const authority = useEventOverlay ? latestEv.authority : vehicle.authority;
  const companionMode = useEventOverlay
    ? latestEv.companion_mode
    : vehicle.companionMode;
  const linkTier = useEventOverlay ? latestEv.link_tier : vehicle.linkTier;
  const linkLost =
    Boolean(linkState?.linkLost) ||
    (useEventOverlay && latestEv.kind === "link_lost") ||
    freshness.staleReason === "link_lost" ||
    freshness.staleReason === "fleet_disconnected";

  const isSupervisoryOnly = linkTier === "mesh-supervisory";
  const isSitlLocal =
    !vehicle.poisoned && !linkLost && linkTier === "sitl-local";
  const isHardwarePreempted =
    authority === "direct-crsf-override" || authority === "mcu-failsafe";

  const canEngageTeleop =
    !vehicle.poisoned &&
    !linkLost &&
    freshness.fresh &&
    !isSupervisoryOnly &&
    armed &&
    authority === "companion-teleop";

  const canIssueStandardCommand =
    !vehicle.poisoned &&
    !linkLost &&
    authority !== "direct-crsf-override" &&
    !(authority === "mcu-failsafe" && armed);

  const canEmergencyDisarm =
    !vehicle.poisoned &&
    !linkLost &&
    armed &&
    authority !== "direct-crsf-override";

  return {
    sysid: vehicle.sysid,
    armed,
    authority,
    companionMode,
    linkTier,
    linkLost,
    fresh: freshness.fresh,
    statusLabel: freshness.statusLabel,
    stateObservedMs,
    ...(observationRev !== undefined ? { observationRev } : {}),
    ...(authorityIncarnation !== undefined ? { authorityIncarnation } : {}),
    ...(authorityEpoch !== undefined ? { authorityEpoch } : {}),
    isSupervisoryOnly,
    isSitlLocal,
    isHardwarePreempted,
    canEngageTeleop,
    canIssueStandardCommand,
    canEmergencyDisarm,
  };
}

type PadHoldKey =
  | "forward"
  | "backward"
  | "left"
  | "right"
  | "yaw_left"
  | "yaw_right"
  | "climb"
  | "descend";

function classifyPadHoldKey(
  event: PadKeyEventLike,
): PadHoldKey | "stop" | null {
  const key = event.key;
  if (key === "Escape" || key === " ") {
    return "stop";
  }
  const lower = key.length === 1 ? key.toLowerCase() : key;
  switch (lower) {
    case "w":
    case "ArrowUp":
      return "forward";
    case "s":
    case "ArrowDown":
      return "backward";
    case "a":
      return "left";
    case "d":
      return "right";
    case "q":
    case "ArrowLeft":
      return "yaw_left";
    case "e":
    case "ArrowRight":
      return "yaw_right";
    case "r":
      return "climb";
    case "f":
      return "descend";
    default:
      return null;
  }
}

export function createActuationController(
  options: CreateActuationControllerOptions,
): ActuationController {
  const telemetryStore = options.telemetryStore;
  const skgStore = options.skgStore;
  const client = options.client ?? createControlClient();
  const teleopIntervalMs =
    options.teleopIntervalMs ?? DEFAULT_TELEOP_INTERVAL_MS;
  const leaseTtlMs = options.leaseTtlMs ?? DEFAULT_TELEOP_LEASE_TTL_MS;
  const maxLinearMps = options.maxLinearMps ?? DEFAULT_MAX_LINEAR_MPS;
  const maxVerticalMps = options.maxVerticalMps ?? DEFAULT_MAX_VERTICAL_MPS;
  const maxYawRadS = options.maxYawRadS ?? DEFAULT_MAX_YAW_RAD_S;
  const now = options.now ?? (() => Date.now());
  const perfNow =
    options.perfNow ??
    (() =>
      typeof performance !== "undefined" &&
      typeof performance.now === "function"
        ? performance.now()
        : Date.now());
  const setIntervalFn =
    options.setIntervalFn ?? ((cb, ms) => setInterval(cb, ms));
  const clearIntervalFn =
    options.clearIntervalFn ?? ((handle) => clearInterval(handle));

  let activeSysId = $state<number | null>(null);
  let engagedSysId = $state<number | null>(null);
  let leaseAccepted = $state(false);
  let leaseConfirmed = $state(false);
  let renewalGraceActive = $state(false);
  let frame = $state<TeleopFrame>("local-ned");
  let axes = $state<TeleopAxes>(ZERO_TELEOP_AXES);
  let padNormX = $state(0);
  let padNormY = $state(0);
  let pointerDragging = $state(false);
  let submittingAction = $state<DiscreteCommandAction | null>(null);
  let optimisticCommand = $state<VehicleCommandRecord | null>(null);
  let lastError = $state<string | null>(null);
  let lastPreemptLatencyMs = $state<number | null>(null);
  let teleopPacketsSent = $state(0);
  let commandRequestsSent = $state(0);

  const preemptBySysId = new SvelteMap<number, PreemptionLatch>();
  // eslint-disable-next-line svelte/prefer-svelte-reactivity
  const heldKeys = new Set<PadHoldKey>();
  let teleopTimer: ReturnType<typeof setInterval> | null = null;
  let leaseExpiryTimer: ReturnType<typeof setTimeout> | null = null;
  let confirmedLeaseDeadlinePerf: number | null = null;
  let inFlightRenewals: Array<{ seq: number; sentPerf: number }> = [];
  let timerRenewalsInFlight = 0;
  let timerGen = 0;
  let engageCounter = 0;
  let engagedEntityId: string | null = null;
  let engagedSession: string | null = null;
  let teleopSeq = 0;
  let lastFreshnessContext: FleetFreshnessContext | undefined;

  const skgLatestCommand = $derived(
    extractLatestVehicleCommand(skgStore.entities.values(), activeSysId),
  );

  const latestCommand = $derived.by<VehicleCommandRecord | null>(() => {
    const opt =
      optimisticCommand && optimisticCommand.sysid === activeSysId
        ? optimisticCommand
        : null;
    if (!opt) {
      return skgLatestCommand;
    }
    if (!skgLatestCommand) {
      return opt;
    }
    if (
      skgLatestCommand.id === opt.id ||
      skgLatestCommand.issuedMs > opt.issuedMs
    ) {
      return skgLatestCommand;
    }
    return opt;
  });

  const preemptionLatch = $derived(
    activeSysId !== null ? (preemptBySysId.get(activeSysId) ?? null) : null,
  );

  function clearLeaseExpiryTimer(): void {
    if (leaseExpiryTimer !== null) {
      clearTimeout(leaseExpiryTimer);
      leaseExpiryTimer = null;
    }
  }

  function stopTeleopTimer(): void {
    timerGen += 1;
    timerRenewalsInFlight = 0;
    clearLeaseExpiryTimer();
    if (teleopTimer !== null) {
      clearIntervalFn(teleopTimer);
      teleopTimer = null;
    }
  }

  function clearEngagedSessionLocal(): void {
    stopTeleopTimer();
    engagedSysId = null;
    engagedEntityId = null;
    engagedSession = null;
    leaseAccepted = false;
    leaseConfirmed = false;
    renewalGraceActive = false;
    confirmedLeaseDeadlinePerf = null;
    inFlightRenewals = [];
    teleopSeq = 0;
    resetStickState();
  }

  function hasConfirmedLeaseForDispatch(nowPerf = perfNow()): boolean {
    return (
      leaseAccepted &&
      confirmedLeaseDeadlinePerf !== null &&
      nowPerf < confirmedLeaseDeadlinePerf
    );
  }

  function getPendingRenewalDeadlinePerf(): number | null {
    if (!leaseAccepted || confirmedLeaseDeadlinePerf === null) {
      return null;
    }
    let best: number | null = null;
    for (const entry of inFlightRenewals) {
      const cand = entry.sentPerf + leaseTtlMs;
      if (best === null || cand > best) {
        best = cand;
      }
    }
    return best;
  }

  function hasPendingRenewalWindow(nowPerf = perfNow()): boolean {
    const deadline = getPendingRenewalDeadlinePerf();
    return deadline !== null && nowPerf < deadline;
  }

  function hasSessionOrRenewalWindow(nowPerf = perfNow()): boolean {
    return (
      hasConfirmedLeaseForDispatch(nowPerf) || hasPendingRenewalWindow(nowPerf)
    );
  }

  function expireConfirmedLeaseLocal(sysid: number): void {
    clearEngagedSessionLocal();
    lastError = `UXS #${sysid} teleop lease expired — click Engage Teleop to resume`;
  }

  function scheduleLeaseExpiryTimer(sysid: number, session: string): void {
    clearLeaseExpiryTimer();
    if (
      engagedSysId !== sysid ||
      engagedSession !== session ||
      !leaseAccepted ||
      confirmedLeaseDeadlinePerf === null
    ) {
      return;
    }
    const nowPerf = perfNow();
    if (nowPerf < confirmedLeaseDeadlinePerf) {
      leaseConfirmed = true;
      renewalGraceActive = false;
      recomputeKeyboardAxes();
      const delayMs = Math.max(0, confirmedLeaseDeadlinePerf - nowPerf);
      leaseExpiryTimer = setTimeout(() => {
        leaseExpiryTimer = null;
        scheduleLeaseExpiryTimer(sysid, session);
      }, delayMs);
      return;
    }
    leaseConfirmed = false;
    pointerDragging = false;
    const pendingDeadline = getPendingRenewalDeadlinePerf();
    if (pendingDeadline !== null && nowPerf < pendingDeadline) {
      renewalGraceActive = true;
      const delayMs = Math.max(0, pendingDeadline - nowPerf);
      leaseExpiryTimer = setTimeout(() => {
        leaseExpiryTimer = null;
        scheduleLeaseExpiryTimer(sysid, session);
      }, delayMs);
      return;
    }
    expireConfirmedLeaseLocal(sysid);
  }

  function resetStickState(): void {
    heldKeys.clear();
    if (pointerDragging) {
      pointerDragging = false;
    }
    if (padNormX !== 0) {
      padNormX = 0;
    }
    if (padNormY !== 0) {
      padNormY = 0;
    }
    if (axes.vx !== 0 || axes.vy !== 0 || axes.vz !== 0 || axes.yawRate !== 0) {
      axes = ZERO_TELEOP_AXES;
    }
  }

  function recomputeKeyboardAxes(): void {
    if (pointerDragging) {
      return;
    }
    const forward =
      (heldKeys.has("forward") ? 1 : 0) - (heldKeys.has("backward") ? 1 : 0);
    const strafe =
      (heldKeys.has("right") ? 1 : 0) - (heldKeys.has("left") ? 1 : 0);
    const yaw =
      (heldKeys.has("yaw_right") ? 1 : 0) - (heldKeys.has("yaw_left") ? 1 : 0);
    // In NED, negative Z is climb (up) and positive Z is descend (down).
    const vzSign =
      (heldKeys.has("descend") ? 1 : 0) - (heldKeys.has("climb") ? 1 : 0);

    padNormX = strafe;
    padNormY = forward;
    axes = {
      vx: roundAxis(forward * maxLinearMps),
      vy: roundAxis(strafe * maxLinearMps),
      vz: roundAxis(vzSign * maxVerticalMps),
      yawRate: roundAxis(yaw * maxYawRadS),
    };
  }

  function verifyEngagedVehicleLive(sysid: number): boolean {
    if (engagedSysId !== sysid) {
      return false;
    }
    if (leaseAccepted && !hasSessionOrRenewalWindow()) {
      expireConfirmedLeaseLocal(sysid);
      return false;
    }
    const liveVehicle =
      (engagedEntityId
        ? skgStore.getCommandableVehicle(engagedEntityId)
        : null) ??
      skgStore.commandableVehicles.find((v) => v.sysid === sysid) ??
      null;
    if (!liveVehicle) {
      void disengageTeleop({ sendZero: true });
      return false;
    }
    const currentNow = now();
    const liveSafety = resolveLiveActuationSafety(
      liveVehicle,
      telemetryStore,
      skgStore,
      lastFreshnessContext
        ? { ...lastFreshnessContext, nowMs: currentNow }
        : undefined,
      currentNow,
    );
    if (liveSafety.canEngageTeleop) {
      return true;
    }
    if (liveSafety.authority === "direct-crsf-override") {
      applyPreemptionEdge(
        sysid,
        "direct-crsf-override",
        "P1 RC PILOT OVERRIDE preempted teleop — click Engage Teleop after P2 returns",
      );
    } else if (liveSafety.authority === "mcu-failsafe") {
      applyPreemptionEdge(
        sysid,
        "mcu-failsafe",
        "P0 MCU FAILSAFE preempted teleop — click Engage Teleop after recovery",
      );
    } else if (liveSafety.linkLost) {
      applyPreemptionEdge(
        sysid,
        "link-lost",
        `UXS #${sysid} link lost — teleop disengaged`,
      );
    } else if (
      liveSafety.isSupervisoryOnly ||
      !liveSafety.armed ||
      liveSafety.authority !== "companion-teleop"
    ) {
      clearEngagedSessionLocal();
    } else {
      lastError = `UXS #${sysid} telemetry is stale (${liveSafety.statusLabel}) — teleop disengaged`;
      void disengageTeleop({ sendZero: true });
    }
    return false;
  }

  function removeInFlightRenewal(seq: number): void {
    inFlightRenewals = inFlightRenewals.filter((entry) => entry.seq !== seq);
  }

  function dispatchTeleopNudge(
    sysid: number,
    engageFlag: boolean,
    engageMeta?: {
      observedMs: number | null;
      observationRev?: number;
      authorityIncarnation?: string;
      authorityEpoch?: number;
    },
  ): Promise<boolean> {
    if (engagedSysId !== sysid || engagedSession === null) {
      return Promise.resolve(false);
    }
    const sentPerf = perfNow();
    if (!engageFlag) {
      if (!leaseAccepted || !verifyEngagedVehicleLive(sysid)) {
        return Promise.resolve(false);
      }
      if (!hasConfirmedLeaseForDispatch(sentPerf)) {
        return Promise.resolve(false);
      }
    }
    const session = engagedSession;
    teleopSeq += 1;
    const seq = teleopSeq;
    teleopPacketsSent += 1;
    const currentAxes = axes;
    if (!engageFlag) {
      inFlightRenewals = [...inFlightRenewals, { seq, sentPerf }];
      scheduleLeaseExpiryTimer(sysid, session);
    }
    return client
      .submitTeleop({
        sysid,
        session,
        seq,
        frame,
        vx: currentAxes.vx,
        vy: currentAxes.vy,
        vz: currentAxes.vz,
        yaw_rate: currentAxes.yawRate,
        engage: engageFlag || undefined,
        ...(engageFlag &&
        engageMeta?.observedMs !== null &&
        engageMeta?.observedMs !== undefined
          ? { observed_ms: Math.trunc(engageMeta.observedMs) }
          : {}),
        ...(engageFlag &&
        engageMeta?.observationRev !== undefined &&
        engageMeta.observationRev > 0
          ? { observation_rev: engageMeta.observationRev }
          : {}),
        ...(engageFlag &&
        engageMeta?.authorityIncarnation !== undefined &&
        engageMeta.authorityIncarnation.length > 0
          ? { authority_incarnation: engageMeta.authorityIncarnation }
          : {}),
        ...(engageFlag && engageMeta?.authorityEpoch !== undefined
          ? { authority_epoch: engageMeta.authorityEpoch }
          : {}),
      })
      .then(
        (res) => {
          if (engagedSession === session && !engageFlag) {
            removeInFlightRenewal(seq);
          }
          if (!res.ok) {
            if (
              engagedSession !== session ||
              engagedSysId !== sysid ||
              res.error.code === "teleop_seq_superseded"
            ) {
              if (engagedSession === session && engagedSysId === sysid) {
                if (!hasSessionOrRenewalWindow()) {
                  expireConfirmedLeaseLocal(sysid);
                } else {
                  scheduleLeaseExpiryTimer(sysid, session);
                }
              }
              return false;
            }
            clearEngagedSessionLocal();
            lastError = res.error.message;
            if (
              res.error.code === "preemption_latched" ||
              res.error.code === "authority_not_teleop"
            ) {
              preemptBySysId.set(sysid, {
                sysid,
                source: "direct-crsf-override",
                message: res.error.message,
                latchedAtMs: now(),
              });
            }
            return false;
          }
          if (engagedSession !== session || engagedSysId !== sysid) {
            return false;
          }
          if (!res.result.lease_active) {
            expireConfirmedLeaseLocal(sysid);
            return false;
          }
          if (!engageFlag && !leaseAccepted) {
            return false;
          }
          const candidateDeadline = sentPerf + leaseTtlMs;
          const nextDeadline =
            confirmedLeaseDeadlinePerf === null
              ? candidateDeadline
              : Math.max(confirmedLeaseDeadlinePerf, candidateDeadline);
          if (perfNow() >= nextDeadline && !hasPendingRenewalWindow()) {
            expireConfirmedLeaseLocal(sysid);
            return false;
          }
          if (perfNow() >= candidateDeadline) {
            scheduleLeaseExpiryTimer(sysid, session);
            return false;
          }
          if (engageFlag) {
            if (!verifyEngagedVehicleLive(sysid)) {
              return false;
            }
            confirmedLeaseDeadlinePerf = nextDeadline;
            leaseAccepted = true;
            startTeleopTimer(sysid);
            scheduleLeaseExpiryTimer(sysid, session);
            if (
              heldKeys.size > 0 ||
              nextDeadline - perfNow() <= teleopIntervalMs * 2
            ) {
              triggerConfirmedTimerRenewal(sysid);
            }
          } else {
            const wasUnconfirmed = !leaseConfirmed;
            confirmedLeaseDeadlinePerf = nextDeadline;
            scheduleLeaseExpiryTimer(sysid, session);
            if (
              wasUnconfirmed ||
              nextDeadline - perfNow() <= teleopIntervalMs * 2
            ) {
              triggerConfirmedTimerRenewal(sysid);
            }
          }
          return true;
        },
        (err: unknown) => {
          if (engagedSession === session && !engageFlag) {
            removeInFlightRenewal(seq);
          }
          throw err;
        },
      );
  }

  function triggerConfirmedTimerRenewal(sysid: number, gen = timerGen): void {
    if (engagedSysId !== sysid) {
      stopTeleopTimer();
      return;
    }
    if (!verifyEngagedVehicleLive(sysid)) {
      return;
    }
    if (!hasConfirmedLeaseForDispatch()) {
      return;
    }
    if (timerRenewalsInFlight >= 2) {
      return;
    }
    timerRenewalsInFlight += 1;
    void dispatchTeleopNudge(sysid, false).finally(() => {
      if (timerGen === gen && timerRenewalsInFlight > 0) {
        timerRenewalsInFlight -= 1;
      }
    });
  }

  function startTeleopTimer(sysid: number): void {
    stopTeleopTimer();
    const gen = timerGen;
    teleopTimer = setIntervalFn(() => {
      triggerConfirmedTimerRenewal(sysid, gen);
    }, teleopIntervalMs);
  }

  function applyPreemptionEdge(
    sysid: number,
    source: ControlAuthority | "link-lost",
    message: string,
  ): void {
    const startPerf = perfNow();
    const wasEngaged = engagedSysId === sysid;
    let mutated = false;
    if (wasEngaged) {
      clearEngagedSessionLocal();
      mutated = true;
    } else if (activeSysId === sysid) {
      resetStickState();
    }
    if (wasEngaged || source !== "link-lost") {
      const existing = preemptBySysId.get(sysid);
      if (!existing || existing.source !== source || wasEngaged) {
        preemptBySysId.set(sysid, {
          sysid,
          source,
          message,
          latchedAtMs: now(),
        });
        mutated = true;
      }
    }
    if (
      optimisticCommand &&
      optimisticCommand.sysid === sysid &&
      optimisticCommand.phase === "pending"
    ) {
      optimisticCommand = {
        ...optimisticCommand,
        phase: "preempted",
        detail: message,
        updatedMs: now(),
      };
      mutated = true;
    }
    if (mutated || lastPreemptLatencyMs === null) {
      lastPreemptLatencyMs = Math.max(0, perfNow() - startPerf);
    }
  }

  const unsubscribeTransitions =
    telemetryStore.onTransitionEvent?.((ev) => {
      if (ev.kind === "link_lost") {
        applyPreemptionEdge(
          ev.sysid,
          "link-lost",
          `UXS #${ev.sysid} link lost — teleop disengaged`,
        );
        return;
      }
      if (ev.authority === "direct-crsf-override") {
        applyPreemptionEdge(
          ev.sysid,
          "direct-crsf-override",
          "P1 RC PILOT OVERRIDE preempted teleop — click Engage Teleop after P2 returns",
        );
        return;
      }
      if (ev.authority === "mcu-failsafe") {
        applyPreemptionEdge(
          ev.sysid,
          "mcu-failsafe",
          "P0 MCU FAILSAFE preempted teleop — click Engage Teleop after recovery",
        );
        return;
      }
      if (
        engagedSysId === ev.sysid &&
        (ev.link_tier === "mesh-supervisory" ||
          !ev.armed ||
          ev.authority !== "companion-teleop")
      ) {
        clearEngagedSessionLocal();
      }
    }) ?? null;

  async function sendStopRelease(
    sysid: number,
    session?: string,
    seq?: number,
  ): Promise<void> {
    teleopPacketsSent += 1;
    await client.submitTeleop({
      sysid,
      ...(session !== undefined ? { session } : {}),
      ...(seq !== undefined ? { seq } : {}),
      frame,
      vx: 0,
      vy: 0,
      vz: 0,
      yaw_rate: 0,
      stop: true,
    });
  }

  async function disengageTeleop(
    opts: { sendZero?: boolean } = {},
  ): Promise<void> {
    const prevSysId = engagedSysId;
    const prevSession = engagedSession ?? undefined;
    const stopSeq = prevSession !== undefined ? teleopSeq + 1 : undefined;
    clearEngagedSessionLocal();
    if (prevSysId !== null && (opts.sendZero ?? true)) {
      await sendStopRelease(prevSysId, prevSession, stopSeq);
    }
  }

  function reconcileVehicle(
    vehicle: VehicleEntityState | null,
    freshnessContext?: FleetFreshnessContext,
  ): LiveActuationSafety | null {
    lastFreshnessContext = freshnessContext;
    if (!vehicle || vehicle.poisoned) {
      untrack(() => {
        const prevEngaged = engagedSysId;
        if (activeSysId !== null) {
          activeSysId = null;
        }
        if (prevEngaged !== null) {
          void disengageTeleop({ sendZero: true });
        } else {
          resetStickState();
        }
      });
      return null;
    }

    const safety = resolveLiveActuationSafety(
      vehicle,
      telemetryStore,
      skgStore,
      freshnessContext,
      now(),
    );

    untrack(() => {
      if (activeSysId !== vehicle.sysid) {
        const prevEngaged = engagedSysId;
        activeSysId = vehicle.sysid;
        lastError = null;
        if (prevEngaged !== null && prevEngaged !== vehicle.sysid) {
          void disengageTeleop({ sendZero: true });
        }
      }

      if (safety.authority === "direct-crsf-override") {
        applyPreemptionEdge(
          vehicle.sysid,
          "direct-crsf-override",
          "P1 RC PILOT OVERRIDE preempted teleop — click Engage Teleop after P2 returns",
        );
      } else if (safety.authority === "mcu-failsafe") {
        applyPreemptionEdge(
          vehicle.sysid,
          "mcu-failsafe",
          "P0 MCU FAILSAFE preempted teleop — click Engage Teleop after recovery",
        );
      } else if (safety.linkLost && engagedSysId === vehicle.sysid) {
        applyPreemptionEdge(
          vehicle.sysid,
          "link-lost",
          `UXS #${vehicle.sysid} link lost — teleop disengaged`,
        );
      } else if (
        engagedSysId === vehicle.sysid &&
        (safety.isSupervisoryOnly ||
          !safety.armed ||
          safety.authority !== "companion-teleop")
      ) {
        clearEngagedSessionLocal();
      } else if (engagedSysId === vehicle.sysid && !safety.fresh) {
        lastError = `UXS #${vehicle.sysid} telemetry is stale (${safety.statusLabel}) — teleop disengaged`;
        void disengageTeleop({ sendZero: true });
      } else if (
        engagedSysId === vehicle.sysid &&
        leaseAccepted &&
        !hasSessionOrRenewalWindow()
      ) {
        expireConfirmedLeaseLocal(vehicle.sysid);
      }
    });

    return safety;
  }

  async function engageTeleop(
    vehicle: VehicleEntityState | null,
    freshnessContext?: FleetFreshnessContext,
  ): Promise<boolean> {
    if (!vehicle || vehicle.poisoned) {
      lastError = "Selected entity is not a commandable UXS vehicle";
      return false;
    }
    const safety = reconcileVehicle(vehicle, freshnessContext);
    if (!safety || !safety.canEngageTeleop) {
      lastError = safety?.isSupervisoryOnly
        ? "Continuous #84 velocity teleop is disabled on mesh-supervisory links"
        : safety?.linkLost
          ? `UXS #${vehicle.sysid} link is silent (LINK LOST)`
          : safety && !safety.fresh
            ? `UXS #${vehicle.sysid} telemetry is stale (${safety.statusLabel})`
            : !safety?.armed
              ? `UXS #${vehicle.sysid} must be ARMED before engaging velocity teleop`
              : `UXS #${vehicle.sysid} authority is ${safety?.authority ?? vehicle.authority} (companion-teleop required)`;
      return false;
    }

    stopTeleopTimer();
    lastError = null;
    preemptBySysId.delete(vehicle.sysid);
    engageCounter += 1;
    const session = `s${vehicle.sysid}-e${engageCounter}-${now()}`;
    activeSysId = vehicle.sysid;
    engagedSysId = vehicle.sysid;
    engagedEntityId = vehicle.id;
    engagedSession = session;
    leaseAccepted = false;
    leaseConfirmed = false;
    renewalGraceActive = false;
    confirmedLeaseDeadlinePerf = null;
    inFlightRenewals = [];
    teleopSeq = 0;
    resetStickState();

    const ok = await dispatchTeleopNudge(vehicle.sysid, true, {
      observedMs: safety.stateObservedMs,
      observationRev: safety.observationRev,
      authorityIncarnation: safety.authorityIncarnation,
      authorityEpoch: safety.authorityEpoch,
    });
    if (!ok || engagedSysId !== vehicle.sysid || engagedSession !== session) {
      if (engagedSysId === vehicle.sysid && engagedSession === session) {
        clearEngagedSessionLocal();
      }
      return false;
    }
    return true;
  }

  function setPadVector(normX: number, normY: number, dragging: boolean): void {
    if (
      engagedSysId === null ||
      engagedSysId !== activeSysId ||
      !leaseAccepted
    ) {
      return;
    }
    if (!hasConfirmedLeaseForDispatch()) {
      if (!hasPendingRenewalWindow()) {
        expireConfirmedLeaseLocal(engagedSysId);
      }
      return;
    }
    const clampedX = clampUnit(normX);
    const clampedY = clampUnit(normY);
    pointerDragging = dragging;
    padNormX = clampedX;
    padNormY = clampedY;
    axes = {
      vx: roundAxis(clampedY * maxLinearMps),
      vy: roundAxis(clampedX * maxLinearMps),
      vz: axes.vz,
      yawRate: axes.yawRate,
    };
  }

  function releasePadPointer(): void {
    if (!pointerDragging && padNormX === 0 && padNormY === 0) {
      return;
    }
    pointerDragging = false;
    padNormX = 0;
    padNormY = 0;
    if (heldKeys.size > 0) {
      recomputeKeyboardAxes();
    } else {
      axes = ZERO_TELEOP_AXES;
    }
    if (engagedSysId !== null && leaseAccepted) {
      if (hasConfirmedLeaseForDispatch()) {
        void dispatchTeleopNudge(engagedSysId, false);
      } else if (!hasPendingRenewalWindow()) {
        expireConfirmedLeaseLocal(engagedSysId);
      }
    }
  }

  function handlePadKeyDown(event: PadKeyEventLike): boolean {
    if (
      engagedSysId === null ||
      engagedSysId !== activeSysId ||
      !leaseAccepted ||
      event.ctrlKey ||
      event.metaKey ||
      event.altKey
    ) {
      return false;
    }
    const classified = classifyPadHoldKey(event);
    if (classified === null) {
      return false;
    }
    if (classified === "stop") {
      if (!hasSessionOrRenewalWindow()) {
        expireConfirmedLeaseLocal(engagedSysId);
        return false;
      }
      event.preventDefault?.();
      event.stopPropagation?.();
      void disengageTeleop({ sendZero: true });
      return true;
    }
    if (!hasConfirmedLeaseForDispatch()) {
      if (!hasPendingRenewalWindow()) {
        expireConfirmedLeaseLocal(engagedSysId);
      }
      return false;
    }
    event.preventDefault?.();
    event.stopPropagation?.();

    if (event.repeat && heldKeys.has(classified)) {
      return true;
    }
    heldKeys.add(classified);
    recomputeKeyboardAxes();
    void dispatchTeleopNudge(engagedSysId, false);
    return true;
  }

  function handlePadKeyUp(event: PadKeyEventLike): boolean {
    const classified = classifyPadHoldKey(event);
    if (classified === null || classified === "stop") {
      return false;
    }
    if (!heldKeys.has(classified)) {
      return false;
    }
    event.preventDefault?.();
    event.stopPropagation?.();
    heldKeys.delete(classified);
    recomputeKeyboardAxes();
    if (engagedSysId !== null && leaseAccepted) {
      if (hasConfirmedLeaseForDispatch()) {
        void dispatchTeleopNudge(engagedSysId, false);
      } else if (!hasPendingRenewalWindow()) {
        expireConfirmedLeaseLocal(engagedSysId);
      }
    }
    return true;
  }

  function handleFocusLoss(): void {
    if (engagedSysId !== null) {
      void disengageTeleop({ sendZero: true });
      return;
    }
    resetStickState();
  }

  async function sendDiscreteCommand(
    vehicle: VehicleEntityState | null,
    action: DiscreteCommandAction,
    params: { param1?: number; param2?: number; param3?: number } = {},
    freshnessContext?: FleetFreshnessContext,
  ): Promise<ControlResponse<ControlCommandWireResult>> {
    if (!vehicle || vehicle.poisoned) {
      const errResp: ControlResponse<ControlCommandWireResult> = {
        ok: false,
        error: {
          code: "invalid_vehicle",
          message: "Selected entity is not a commandable UXS vehicle",
          status: 400,
        },
      };
      lastError = errResp.error.message;
      return errResp;
    }

    reconcileVehicle(vehicle, freshnessContext);

    if (
      engagedSysId === vehicle.sysid &&
      (action === "disarm" ||
        action === "hold-position" ||
        action === "mode-autonomous")
    ) {
      await disengageTeleop({ sendZero: false });
    }

    lastError = null;
    submittingAction = action;
    commandRequestsSent += 1;

    const payload: ControlCommandRequest = {
      sysid: vehicle.sysid,
      action,
      ...(params.param1 !== undefined ? { param1: params.param1 } : {}),
      ...(params.param2 !== undefined ? { param2: params.param2 } : {}),
      ...(params.param3 !== undefined ? { param3: params.param3 } : {}),
    };

    try {
      const resp = await client.submitCommand(payload);
      if (resp.ok) {
        const currentSk = extractLatestVehicleCommand(
          skgStore.entities.values(),
          vehicle.sysid,
        );
        if (!currentSk || currentSk.id !== resp.result.command_id) {
          optimisticCommand = {
            id: resp.result.command_id,
            sysid: resp.result.sysid,
            cmdId: resp.result.cmd_id,
            action: resp.result.action,
            phase: resp.result.phase,
            ackResult: -1,
            detail: resp.result.detail,
            issuedMs: resp.result.issued_ms,
            updatedMs: resp.result.issued_ms,
            kvRevision: 0,
          };
        }
      } else {
        lastError = resp.error.message;
      }
      return resp;
    } finally {
      submittingAction = null;
    }
  }

  return {
    get activeSysId() {
      return activeSysId;
    },
    get engaged() {
      return (
        leaseConfirmed &&
        engagedSysId !== null &&
        engagedSysId === activeSysId &&
        hasConfirmedLeaseForDispatch()
      );
    },
    get engagedSysId() {
      return leaseConfirmed && hasConfirmedLeaseForDispatch()
        ? engagedSysId
        : null;
    },
    get leaseRenewalPending() {
      return (
        engagedSysId !== null &&
        engagedSysId === activeSysId &&
        leaseAccepted &&
        !hasConfirmedLeaseForDispatch() &&
        (renewalGraceActive || hasPendingRenewalWindow())
      );
    },
    get frame() {
      return frame;
    },
    get axes() {
      return leaseConfirmed && hasConfirmedLeaseForDispatch()
        ? axes
        : ZERO_TELEOP_AXES;
    },
    get padNormX() {
      return leaseConfirmed && hasConfirmedLeaseForDispatch() ? padNormX : 0;
    },
    get padNormY() {
      return leaseConfirmed && hasConfirmedLeaseForDispatch() ? padNormY : 0;
    },
    get pointerDragging() {
      return (
        leaseConfirmed && hasConfirmedLeaseForDispatch() && pointerDragging
      );
    },
    get submittingAction() {
      return submittingAction;
    },
    get lastError() {
      return lastError;
    },
    get lastPreemptLatencyMs() {
      return lastPreemptLatencyMs;
    },
    get teleopPacketsSent() {
      return teleopPacketsSent;
    },
    get commandRequestsSent() {
      return commandRequestsSent;
    },
    get latestCommand() {
      return latestCommand;
    },
    get preemptionLatch() {
      return preemptionLatch;
    },
    setFrame(nextFrame: TeleopFrame) {
      if ((TELEOP_FRAMES as readonly string[]).includes(nextFrame)) {
        frame = nextFrame;
      }
    },
    reconcileVehicle,
    engageTeleop,
    disengageTeleop,
    setPadVector,
    releasePadPointer,
    handlePadKeyDown,
    handlePadKeyUp,
    handleFocusLoss,
    sendDiscreteCommand,
    clearError() {
      lastError = null;
    },
    dispose() {
      const prevSysId = engagedSysId;
      const prevSession = engagedSession ?? undefined;
      const stopSeq = prevSession !== undefined ? teleopSeq + 1 : undefined;
      clearEngagedSessionLocal();
      unsubscribeTransitions?.();
      if (prevSysId !== null) {
        void sendStopRelease(prevSysId, prevSession, stopSeq);
      }
    },
  };
}
