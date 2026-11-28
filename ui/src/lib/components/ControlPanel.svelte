<!--
  SPDX-License-Identifier: MIT
  Copyright (c) 2026 boxbotix
-->
<script lang="ts">
  import { onMount, tick } from "svelte";
  import {
    type ActuationController,
    COMMIT_UNKNOWN_BANNER_TEXT,
    type ControlClient,
    type DiscreteCommandAction,
    type DiscreteCommandPhase,
    type TeleopFrame,
    createActuationController,
    resolveLiveActuationSafety,
  } from "$lib/control/actuation.svelte";
  import type { FleetFreshnessContext } from "$lib/hud/view-model";
  import type { SkgStore, VehicleEntityState } from "$lib/stores/skg.svelte";
  import type { TelemetryStore } from "$lib/stores/telemetry.svelte";

  interface Props {
    vehicle: VehicleEntityState;
    telemetryStore: TelemetryStore;
    skgStore: SkgStore;
    freshnessContext?: FleetFreshnessContext;
    client?: ControlClient;
    controller?: ActuationController;
    now?: () => number;
  }

  let {
    vehicle,
    telemetryStore,
    skgStore,
    freshnessContext,
    client,
    controller: externalController,
    now,
  }: Props = $props();

  let ownedController: ActuationController | null = null;
  function getPanelController(): ActuationController {
    if (!ownedController) {
      ownedController = createActuationController({
        telemetryStore,
        skgStore,
        client,
        now,
      });
    }
    return ownedController;
  }

  const controller = $derived(externalController ?? getPanelController());

  const safety = $derived(
    resolveLiveActuationSafety(
      vehicle,
      telemetryStore,
      skgStore,
      freshnessContext,
      now?.(),
    ),
  );

  let trackedVehicleSysId: number | null = null;
  $effect(() => {
    const nextSysId = vehicle.sysid;
    if (trackedVehicleSysId !== null && trackedVehicleSysId !== nextSysId) {
      if (pendingToggleIntent?.kind === "engage") {
        pendingToggleIntent = {
          kind: "cancelled",
          sysid: pendingToggleIntent.sysid,
          session: null,
        };
        suppressPadBlur = false;
      }
    }
    trackedVehicleSysId = nextSysId;
    controller.reconcileVehicle(vehicle, freshnessContext);
  });

  let padEl: HTMLDivElement | undefined = $state();
  let engageBtnEl: HTMLButtonElement | undefined = $state();
  let sitlToggleEl: HTMLButtonElement | undefined = $state();
  let sitlLabOpen = $state(false);
  let currentNorthMps = $state("1.5");
  let currentEastMps = $state("1.2");
  let activePointerId: number | null = null;
  let suppressPadBlur = false;
  let activeButtonKey: string | null = null;
  let buttonKeyConsumed = false;
  let buttonKeyReleased = false;
  let pendingToggleIntent: {
    readonly kind: "disengage" | "engage" | "cancelled";
    readonly sysid: number;
    readonly session: string | null;
  } | null = null;

  $effect(() => {
    if (!safety.isSitlLocal && sitlLabOpen) {
      sitlLabOpen = false;
    }
  });

  onMount(() => {
    const onWindowBlur = (): void => {
      controller.handleFocusLoss();
    };
    const onWindowKeyUp = (ev: KeyboardEvent): void => {
      if (
        activeButtonKey !== null &&
        !buttonKeyReleased &&
        !buttonKeyConsumed &&
        isSameEngageButtonKey(ev.key, activeButtonKey)
      ) {
        onEngageButtonKeyUp(ev);
      }
      controller.handlePadKeyUp(ev);
    };
    const clearReleasedPointerIntent = (): void => {
      setTimeout(() => {
        if (activeButtonKey === null) {
          pendingToggleIntent = null;
          suppressPadBlur = false;
        }
      }, 0);
    };
    const onVisibilityChange = (): void => {
      if (
        typeof document !== "undefined" &&
        document.visibilityState === "hidden"
      ) {
        controller.handleFocusLoss();
      }
    };
    window.addEventListener("blur", onWindowBlur);
    window.addEventListener("keyup", onWindowKeyUp);
    window.addEventListener("pointerup", clearReleasedPointerIntent);
    window.addEventListener("mouseup", clearReleasedPointerIntent);
    document.addEventListener("visibilitychange", onVisibilityChange);
    return () => {
      window.removeEventListener("blur", onWindowBlur);
      window.removeEventListener("keyup", onWindowKeyUp);
      window.removeEventListener("pointerup", clearReleasedPointerIntent);
      window.removeEventListener("mouseup", clearReleasedPointerIntent);
      document.removeEventListener("visibilitychange", onVisibilityChange);
      if (!externalController && ownedController) {
        ownedController.dispose();
        ownedController = null;
      }
    };
  });

  const isCommandPending = $derived(
    controller.submittingAction !== null ||
      controller.latestCommand?.phase === "pending",
  );

  function formatPhaseLabel(phase: DiscreteCommandPhase): string {
    switch (phase) {
      case "pending":
        return "PENDING";
      case "confirmed":
        return "CONFIRMED";
      case "rejected":
        return "REJECTED";
      case "commit_unknown":
        return `COMMIT UNKNOWN — ${COMMIT_UNKNOWN_BANNER_TEXT}`;
      case "preempted":
        return "PREEMPTED";
    }
  }

  function isEngageButtonKey(key: string): boolean {
    return key === " " || key === "Spacebar" || key === "Enter";
  }

  function isSameEngageButtonKey(a: string, b: string): boolean {
    if (a === b) {
      return true;
    }
    return (a === " " || a === "Spacebar") && (b === " " || b === "Spacebar");
  }

  function captureEngageToggleIntent(): void {
    if (controller.engaged || controller.leaseRenewalPending) {
      suppressPadBlur = true;
    }
    pendingToggleIntent = {
      kind: controller.engaged ? "disengage" : "engage",
      sysid: controller.engagedSysId ?? vehicle.sysid,
      session: controller.engagedSession,
    };
  }

  function onEngageButtonKeyDown(ev: KeyboardEvent): void {
    if (!isEngageButtonKey(ev.key)) {
      return;
    }
    if (!ev.repeat && buttonKeyReleased) {
      activeButtonKey = null;
      buttonKeyConsumed = false;
      buttonKeyReleased = false;
      pendingToggleIntent = null;
    }
    if (ev.repeat || activeButtonKey !== null) {
      ev.preventDefault();
      return;
    }
    activeButtonKey = ev.key;
    buttonKeyConsumed = false;
    buttonKeyReleased = false;
    captureEngageToggleIntent();
  }

  function onEngageButtonKeyUp(ev: KeyboardEvent): void {
    if (
      activeButtonKey === null ||
      !isSameEngageButtonKey(ev.key, activeButtonKey)
    ) {
      return;
    }
    if (buttonKeyConsumed || ev.key === "Enter") {
      activeButtonKey = null;
      buttonKeyConsumed = false;
      buttonKeyReleased = false;
      pendingToggleIntent = null;
      suppressPadBlur = false;
      return;
    }
    ev.preventDefault();
    void toggleTeleopEngagement();
    buttonKeyReleased = true;
    setTimeout(() => {
      if (buttonKeyReleased) {
        activeButtonKey = null;
        buttonKeyConsumed = false;
        buttonKeyReleased = false;
        pendingToggleIntent = null;
        suppressPadBlur = false;
      }
    }, 0);
  }

  function onEngageButtonBlur(): void {
    if (activeButtonKey !== null) {
      activeButtonKey = null;
      buttonKeyConsumed = false;
      buttonKeyReleased = false;
      pendingToggleIntent = null;
      suppressPadBlur = false;
    }
  }

  async function toggleTeleopEngagement(): Promise<void> {
    if (activeButtonKey !== null) {
      if (buttonKeyConsumed) {
        return;
      }
      if (buttonKeyReleased) {
        activeButtonKey = null;
        buttonKeyConsumed = false;
        buttonKeyReleased = false;
      } else {
        buttonKeyConsumed = true;
      }
    }
    suppressPadBlur = false;
    const intent = pendingToggleIntent;
    pendingToggleIntent = null;
    if (
      intent?.kind === "cancelled" ||
      (intent?.kind === "engage" && intent.sysid !== vehicle.sysid)
    ) {
      return;
    }
    if (intent?.kind === "disengage") {
      if (intent.session !== null) {
        await controller.disengageTeleop({
          sendZero: true,
          sysid: intent.sysid,
          session: intent.session,
        });
      }
      return;
    }
    if (intent?.kind === "engage") {
      if (controller.engaged) {
        await tick();
        padEl?.focus({ preventScroll: true });
        return;
      }
      const ok = await controller.engageTeleop(vehicle, freshnessContext);
      if (ok) {
        await tick();
        padEl?.focus({ preventScroll: true });
      }
      return;
    }
    if (controller.engaged) {
      await controller.disengageTeleop({ sendZero: true });
      return;
    }
    const ok = await controller.engageTeleop(vehicle, freshnessContext);
    if (ok) {
      await tick();
      padEl?.focus({ preventScroll: true });
    }
  }

  function sendAction(
    action: DiscreteCommandAction,
    params?: { param1?: number; param2?: number; param3?: number },
  ): void {
    void controller.sendDiscreteCommand(
      vehicle,
      action,
      params,
      freshnessContext,
    );
  }

  function updatePadFromPointer(ev: PointerEvent, dragging: boolean): void {
    if (!padEl || !controller.engaged) {
      return;
    }
    const rect = padEl.getBoundingClientRect();
    const halfW = Math.max(rect.width / 2, 1);
    const halfH = Math.max(rect.height / 2, 1);
    const centerX = rect.left + rect.width / 2;
    const centerY = rect.top + rect.height / 2;
    const normX = (ev.clientX - centerX) / halfW;
    const normY = -(ev.clientY - centerY) / halfH;
    controller.setPadVector(normX, normY, dragging);
  }

  function onPadPointerDown(ev: PointerEvent): void {
    if (!controller.engaged || ev.button !== 0) {
      return;
    }
    ev.preventDefault();
    activePointerId = ev.pointerId;
    padEl?.focus({ preventScroll: true });
    try {
      padEl?.setPointerCapture(ev.pointerId);
    } catch {
      // Synthetic pointer events in jsdom may not register active pointer IDs.
    }
    updatePadFromPointer(ev, true);
  }

  function onPadPointerMove(ev: PointerEvent): void {
    if (!controller.engaged || activePointerId !== ev.pointerId) {
      return;
    }
    ev.preventDefault();
    updatePadFromPointer(ev, true);
  }

  function finishPadPointer(ev: PointerEvent): void {
    if (activePointerId !== null && ev.pointerId !== activePointerId) {
      return;
    }
    if (activePointerId !== null) {
      try {
        padEl?.releasePointerCapture(activePointerId);
      } catch {
        // Ignore synthetic jsdom pointer release errors.
      }
    }
    activePointerId = null;
    controller.releasePadPointer();
  }

  function onPadKeyDown(ev: KeyboardEvent): void {
    controller.handlePadKeyDown(ev);
  }

  function onPadKeyUp(ev: KeyboardEvent): void {
    controller.handlePadKeyUp(ev);
  }

  function onPadBlur(ev: FocusEvent): void {
    activePointerId = null;
    if (
      suppressPadBlur ||
      (engageBtnEl &&
        ev.relatedTarget instanceof Node &&
        engageBtnEl.contains(ev.relatedTarget))
    ) {
      suppressPadBlur = false;
      controller.clearHeldMovement();
      return;
    }
    controller.handleFocusLoss();
  }

  function handleFrameChange(ev: Event): void {
    const val = (ev.currentTarget as HTMLSelectElement).value as TeleopFrame;
    controller.setFrame(val);
  }

  function toggleSitlLab(): void {
    sitlLabOpen = !sitlLabOpen;
  }

  function handleSitlDrawerKeydown(ev: KeyboardEvent): void {
    if (ev.key === "Escape" && sitlLabOpen) {
      ev.preventDefault();
      ev.stopPropagation();
      sitlLabOpen = false;
      sitlToggleEl?.focus({ preventScroll: true });
    }
  }

  function applySitlCurrent(): void {
    const north = Number.parseFloat(currentNorthMps);
    const east = Number.parseFloat(currentEastMps);
    if (!Number.isFinite(north) || !Number.isFinite(east)) {
      return;
    }
    sendAction("sitl-cross-current", { param1: north, param2: east });
  }
</script>

<section
  class="control-panel"
  data-testid="control-panel"
  data-sysid={vehicle.sysid}
  data-link-tier={safety.linkTier}
  data-authority={safety.authority}
  data-engaged={controller.engaged ? "true" : "false"}
  aria-label="Vehicle guarded actuation panel"
>
  <header class="panel-header">
    <div class="header-title">
      <span class="eyebrow">GUARDED ACTUATION</span>
      <strong>UXS #{vehicle.sysid} · {vehicle.domain}</strong>
    </div>
    <span
      class="tier-pill"
      class:supervisory={safety.isSupervisoryOnly}
      class:sitl={safety.isSitlLocal}
      data-testid="control-link-tier"
    >
      {safety.linkTier}
    </span>
  </header>

  {#if controller.preemptionLatch || safety.isHardwarePreempted}
    <div
      class="preemption-banner"
      data-testid="control-preemption-banner"
      role="alert"
    >
      <div class="preempt-head">
        <strong>PREEMPTED</strong>
        {#if controller.lastPreemptLatencyMs !== null}
          <span class="latency-tag">
            release {controller.lastPreemptLatencyMs.toFixed(2)} ms
          </span>
        {/if}
      </div>
      <span>
        {controller.preemptionLatch?.message ??
          (safety.authority === "direct-crsf-override"
            ? "P1 RC PILOT OVERRIDE holds actuator authority"
            : "P0 MCU FAILSAFE holds actuator authority")}
      </span>
    </div>
  {/if}

  {#if controller.lastError}
    <div class="error-banner" data-testid="control-error-banner" role="alert">
      <span>{controller.lastError}</span>
      <button
        type="button"
        class="dismiss-btn"
        onclick={() => controller.clearError()}
      >
        Dismiss
      </button>
    </div>
  {/if}

  <div
    class="discrete-strip"
    data-testid="control-discrete-strip"
    role="group"
    aria-label="Supervisory discrete commands"
  >
    <button
      type="button"
      class="cmd-btn"
      data-testid="cmd-arm"
      disabled={!safety.canIssueStandardCommand ||
        safety.armed ||
        isCommandPending}
      onclick={() => sendAction("arm")}
    >
      Arm
    </button>
    <button
      type="button"
      class="cmd-btn"
      data-testid="cmd-hold-position"
      disabled={!safety.canIssueStandardCommand || isCommandPending}
      onclick={() => sendAction("hold-position")}
    >
      Hold Position
    </button>
    <button
      type="button"
      class="cmd-btn"
      data-testid="cmd-mode-teleop"
      disabled={!safety.canIssueStandardCommand || isCommandPending}
      onclick={() => sendAction("mode-teleop")}
    >
      Teleop Mode
    </button>
    <button
      type="button"
      class="cmd-btn"
      data-testid="cmd-mode-autonomous"
      disabled={!safety.canIssueStandardCommand || isCommandPending}
      onclick={() => sendAction("mode-autonomous")}
    >
      Auto Mode
    </button>
    <button
      type="button"
      class="cmd-btn danger"
      data-testid="cmd-disarm"
      disabled={!safety.canEmergencyDisarm ||
        controller.submittingAction === "disarm"}
      onclick={() => sendAction("disarm")}
    >
      Disarm
    </button>
  </div>

  {#if controller.latestCommand}
    <div
      class="command-status phase-{controller.latestCommand.phase}"
      data-testid="control-command-status"
      data-phase={controller.latestCommand.phase}
      data-command-id={controller.latestCommand.id}
      role="status"
      aria-live="polite"
    >
      <div class="cmd-status-head">
        <span class="cmd-action">{controller.latestCommand.action}</span>
        <strong>{formatPhaseLabel(controller.latestCommand.phase)}</strong>
      </div>
      {#if controller.latestCommand.detail && controller.latestCommand.phase !== "commit_unknown"}
        <span class="cmd-detail">{controller.latestCommand.detail}</span>
      {/if}
    </div>
  {/if}

  {#if safety.isSupervisoryOnly}
    <div
      class="supervisory-notice"
      data-testid="control-supervisory-only"
      role="status"
    >
      <strong>SUPERVISORY LINK TIER (1 Hz)</strong>
      <span>
        Continuous 10–30 Hz #84 velocity pad collapsed on mesh-supervisory;
        issue discrete Hold Position, Mode, or Disarm commands above.
      </span>
    </div>
  {:else}
    <div class="teleop-section" data-testid="control-teleop-section">
      <div class="teleop-bar">
        <label class="frame-label">
          <span>Frame</span>
          <select
            class="frame-select"
            data-testid="teleop-frame-select"
            value={controller.frame}
            onchange={handleFrameChange}
          >
            <option value="local-ned">Local NED</option>
            <option value="body-ned">Body NED</option>
          </select>
        </label>

        <button
          bind:this={engageBtnEl}
          type="button"
          class="engage-btn"
          class:active={controller.engaged}
          data-testid="teleop-engage-btn"
          aria-pressed={controller.engaged}
          disabled={!controller.engaged && !safety.canEngageTeleop}
          onpointerdown={(ev) => {
            if (ev.button === 0) {
              activeButtonKey = null;
              buttonKeyConsumed = false;
              buttonKeyReleased = false;
              captureEngageToggleIntent();
            }
          }}
          onmousedown={(ev) => {
            if (ev.button === 0 && pendingToggleIntent === null) {
              activeButtonKey = null;
              buttonKeyConsumed = false;
              buttonKeyReleased = false;
              captureEngageToggleIntent();
            }
          }}
          onkeydown={onEngageButtonKeyDown}
          onkeyup={onEngageButtonKeyUp}
          onblur={onEngageButtonBlur}
          onpointercancel={() => {
            pendingToggleIntent = null;
            suppressPadBlur = false;
          }}
          onclick={() => void toggleTeleopEngagement()}
        >
          {controller.engaged ? "Disengage Teleop" : "Engage Teleop"}
        </button>
      </div>

      <!-- svelte-ignore a11y_no_noninteractive_tabindex, a11y_no_noninteractive_element_interactions -->
      <div
        bind:this={padEl}
        class="velocity-pad"
        class:engaged={controller.engaged}
        data-testid="teleop-velocity-pad"
        data-actuation-pad="true"
        tabindex={controller.engaged || controller.leaseRenewalPending ? 0 : -1}
        role="application"
        aria-label="Spring-return 2D velocity teleop pad. Drag pointer or hold WASD/Arrows while engaged; Space or Escape disengages."
        onpointerdown={onPadPointerDown}
        onpointermove={onPadPointerMove}
        onpointerup={finishPadPointer}
        onpointercancel={finishPadPointer}
        onkeydown={onPadKeyDown}
        onkeyup={onPadKeyUp}
        onblur={onPadBlur}
      >
        <div class="pad-crosshair-h" aria-hidden="true"></div>
        <div class="pad-crosshair-v" aria-hidden="true"></div>
        <div
          class="pad-puck"
          style:transform="translate(calc(-50% + {controller.padNormX * 42}px),
          calc(-50% + {-controller.padNormY * 42}px))"
          aria-hidden="true"
        ></div>
        {#if !controller.engaged}
          <span class="pad-overlay-hint">
            {controller.leaseRenewalPending
              ? "Awaiting teleop lease renewal confirmation…"
              : safety.canEngageTeleop
                ? "Click Engage Teleop to unlock velocity pad"
                : !safety.armed
                  ? "Arm vehicle to enable velocity teleop"
                  : `Teleop locked (${safety.authority})`}
          </span>
        {/if}
      </div>

      <div class="axes-readout" data-testid="teleop-axes-readout">
        <span>vx: <strong>{controller.axes.vx.toFixed(1)}</strong> m/s</span>
        <span>vy: <strong>{controller.axes.vy.toFixed(1)}</strong> m/s</span>
        <span>vz: <strong>{controller.axes.vz.toFixed(1)}</strong> m/s</span>
        <span
          >yaw: <strong>{controller.axes.yawRate.toFixed(2)}</strong> rad/s</span
        >
      </div>
    </div>
  {/if}

  {#if safety.isSitlLocal}
    <div class="sitl-section" data-testid="sitl-lab-section">
      <button
        bind:this={sitlToggleEl}
        type="button"
        class="sitl-toggle-btn"
        data-testid="sitl-lab-toggle"
        aria-expanded={sitlLabOpen}
        aria-controls="sitl-lab-drawer"
        onclick={toggleSitlLab}
      >
        <span>SITL Fault &amp; Current Lab</span>
        <span>{sitlLabOpen ? "▾" : "▸"}</span>
      </button>

      {#if sitlLabOpen}
        <!-- svelte-ignore a11y_no_noninteractive_element_interactions -->
        <div
          id="sitl-lab-drawer"
          class="sitl-drawer"
          data-testid="sitl-lab-drawer"
          role="region"
          aria-label="SITL Fault and Current Lab controls"
          onkeydown={handleSitlDrawerKeydown}
        >
          <div class="sitl-actions">
            <button
              type="button"
              class="cmd-btn warning"
              data-testid="sitl-blackout-btn"
              onclick={() =>
                sendAction("sitl-companion-blackout", { param1: 350 })}
            >
              350 ms Blackout
            </button>
            <button
              type="button"
              class="cmd-btn warning"
              data-testid="sitl-crsf-btn"
              onclick={() =>
                sendAction("sitl-crsf-override", {
                  param1: safety.authority === "direct-crsf-override" ? -1 : 1,
                })}
            >
              {safety.authority === "direct-crsf-override"
                ? "Restore P2 Teleop"
                : "Inject P1 CRSF Override"}
            </button>
          </div>

          <div class="sitl-current-row">
            <label>
              <span>Curr N (m/s)</span>
              <input
                type="number"
                step="0.1"
                data-testid="sitl-current-north"
                bind:value={currentNorthMps}
              />
            </label>
            <label>
              <span>Curr E (m/s)</span>
              <input
                type="number"
                step="0.1"
                data-testid="sitl-current-east"
                bind:value={currentEastMps}
              />
            </label>
            <button
              type="button"
              class="cmd-btn"
              data-testid="sitl-current-apply-btn"
              onclick={applySitlCurrent}
            >
              Apply Current
            </button>
          </div>
        </div>
      {/if}
    </div>
  {/if}
</section>

<style>
  .control-panel {
    display: flex;
    flex-direction: column;
    gap: 8px;
    color: #f8fafc;
    font-size: 0.76rem;
  }

  .panel-header {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: 8px;
  }

  .header-title {
    display: flex;
    flex-direction: column;
    gap: 1px;
  }

  .eyebrow {
    font-size: 0.64rem;
    font-weight: 800;
    letter-spacing: 0.07em;
    color: #94a3b8;
  }

  .tier-pill {
    padding: 2px 7px;
    border-radius: 999px;
    border: 1px solid #38bdf8;
    background: rgba(14, 116, 144, 0.35);
    color: #e0f2fe;
    font-size: 0.68rem;
    font-weight: 700;
    font-family: ui-monospace, SFMono-Regular, Menlo, monospace;
  }

  .tier-pill.supervisory {
    border-color: #f59e0b;
    background: rgba(120, 53, 15, 0.55);
    color: #fef3c7;
  }

  .tier-pill.sitl {
    border-color: #a78bfa;
    background: rgba(76, 29, 149, 0.45);
    color: #ede9fe;
  }

  .preemption-banner {
    display: flex;
    flex-direction: column;
    gap: 3px;
    padding: 6px 8px;
    border-radius: 5px;
    border: 1px solid #f87171;
    background: rgba(127, 29, 29, 0.85);
    color: #fee2e2;
    font-size: 0.72rem;
  }

  .preempt-head {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: 6px;
  }

  .latency-tag {
    font-family: ui-monospace, SFMono-Regular, Menlo, monospace;
    font-size: 0.66rem;
    color: #fecaca;
  }

  .error-banner {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: 8px;
    padding: 5px 8px;
    border-radius: 5px;
    border: 1px solid #fbbf24;
    background: rgba(120, 53, 15, 0.78);
    color: #fef3c7;
    font-size: 0.71rem;
  }

  .dismiss-btn {
    padding: 2px 6px;
    border-radius: 4px;
    border: 1px solid rgba(254, 243, 199, 0.5);
    background: transparent;
    color: #fef3c7;
    font-size: 0.66rem;
    cursor: pointer;
  }

  .discrete-strip {
    display: flex;
    flex-wrap: wrap;
    gap: 5px;
  }

  .cmd-btn {
    padding: 4px 8px;
    border-radius: 4px;
    border: 1px solid #475569;
    background: #1e293b;
    color: #f8fafc;
    font-size: 0.72rem;
    font-weight: 700;
    cursor: pointer;
  }

  .cmd-btn:hover:not(:disabled) {
    border-color: #38bdf8;
    background: #0f172a;
  }

  .cmd-btn:disabled {
    opacity: 0.45;
    cursor: not-allowed;
  }

  .cmd-btn.danger {
    border-color: #ef4444;
    background: rgba(127, 29, 29, 0.72);
    color: #fee2e2;
  }

  .cmd-btn.warning {
    border-color: #f59e0b;
    background: rgba(120, 53, 15, 0.65);
    color: #fef3c7;
  }

  .command-status {
    display: flex;
    flex-direction: column;
    gap: 2px;
    padding: 5px 8px;
    border-radius: 5px;
    border: 1px solid #475569;
    background: rgba(15, 23, 42, 0.9);
    font-size: 0.7rem;
  }

  .command-status.phase-confirmed {
    border-color: #10b981;
    color: #d1fae5;
  }

  .command-status.phase-commit_unknown {
    border-color: #f59e0b;
    background: rgba(120, 53, 15, 0.72);
    color: #fef3c7;
  }

  .command-status.phase-rejected,
  .command-status.phase-preempted {
    border-color: #ef4444;
    background: rgba(127, 29, 29, 0.72);
    color: #fee2e2;
  }

  .cmd-status-head {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: 8px;
  }

  .cmd-action {
    font-family: ui-monospace, SFMono-Regular, Menlo, monospace;
    text-transform: uppercase;
    color: #cbd5e1;
  }

  .cmd-detail {
    color: #cbd5e1;
    font-size: 0.67rem;
  }

  .supervisory-notice {
    display: flex;
    flex-direction: column;
    gap: 3px;
    padding: 7px 9px;
    border-radius: 5px;
    border: 1px solid #f59e0b;
    background: rgba(120, 53, 15, 0.65);
    color: #fef3c7;
    font-size: 0.72rem;
  }

  .teleop-section {
    display: flex;
    flex-direction: column;
    gap: 6px;
  }

  .teleop-bar {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: 8px;
  }

  .frame-label {
    display: inline-flex;
    align-items: center;
    gap: 5px;
    color: #cbd5e1;
    font-size: 0.7rem;
  }

  .frame-select {
    padding: 3px 6px;
    border-radius: 4px;
    border: 1px solid #475569;
    background: #0f172a;
    color: #f8fafc;
    font-size: 0.7rem;
  }

  .engage-btn {
    padding: 4px 10px;
    border-radius: 4px;
    border: 1px solid #38bdf8;
    background: #0c4a6e;
    color: #e0f2fe;
    font-size: 0.72rem;
    font-weight: 800;
    cursor: pointer;
  }

  .engage-btn.active {
    border-color: #10b981;
    background: #065f46;
    color: #ecfdf5;
  }

  .engage-btn:disabled {
    opacity: 0.45;
    cursor: not-allowed;
  }

  .velocity-pad {
    position: relative;
    height: 104px;
    border-radius: 6px;
    border: 1px dashed #475569;
    background: rgba(15, 23, 42, 0.85);
    touch-action: none;
    user-select: none;
    overflow: hidden;
  }

  .velocity-pad.engaged {
    border-style: solid;
    border-color: #10b981;
    cursor: crosshair;
  }

  .velocity-pad:focus-visible {
    outline: 2px solid #38bdf8;
    outline-offset: 2px;
  }

  .pad-crosshair-h {
    position: absolute;
    left: 0;
    right: 0;
    top: 50%;
    height: 1px;
    background: rgba(148, 163, 184, 0.25);
  }

  .pad-crosshair-v {
    position: absolute;
    top: 0;
    bottom: 0;
    left: 50%;
    width: 1px;
    background: rgba(148, 163, 184, 0.25);
  }

  .pad-puck {
    position: absolute;
    left: 50%;
    top: 50%;
    width: 16px;
    height: 16px;
    border-radius: 999px;
    border: 2px solid #38bdf8;
    background: rgba(56, 189, 248, 0.35);
    pointer-events: none;
  }

  .velocity-pad.engaged .pad-puck {
    border-color: #34d399;
    background: rgba(16, 185, 129, 0.45);
  }

  .pad-overlay-hint {
    position: absolute;
    inset: 0;
    display: flex;
    align-items: center;
    justify-content: center;
    padding: 6px;
    text-align: center;
    color: #cbd5e1;
    background: rgba(2, 6, 23, 0.55);
    font-size: 0.69rem;
    pointer-events: none;
  }

  .axes-readout {
    display: flex;
    flex-wrap: wrap;
    justify-content: space-between;
    gap: 6px;
    color: #cbd5e1;
    font-family: ui-monospace, SFMono-Regular, Menlo, monospace;
    font-size: 0.68rem;
  }

  .sitl-section {
    display: flex;
    flex-direction: column;
    gap: 6px;
    border-top: 1px solid #334155;
    padding-top: 6px;
  }

  .sitl-toggle-btn {
    display: flex;
    align-items: center;
    justify-content: space-between;
    padding: 4px 8px;
    border-radius: 4px;
    border: 1px solid #6d28d9;
    background: rgba(76, 29, 149, 0.35);
    color: #ede9fe;
    font-size: 0.71rem;
    font-weight: 700;
    cursor: pointer;
  }

  .sitl-drawer {
    display: flex;
    flex-direction: column;
    gap: 6px;
    padding: 6px;
    border-radius: 5px;
    border: 1px solid #4c1d95;
    background: rgba(15, 23, 42, 0.92);
  }

  .sitl-actions {
    display: flex;
    flex-wrap: wrap;
    gap: 5px;
  }

  .sitl-current-row {
    display: flex;
    flex-wrap: wrap;
    align-items: flex-end;
    gap: 6px;
  }

  .sitl-current-row label {
    display: flex;
    flex-direction: column;
    gap: 2px;
    color: #cbd5e1;
    font-size: 0.66rem;
  }

  .sitl-current-row input {
    width: 68px;
    padding: 3px 5px;
    border-radius: 4px;
    border: 1px solid #475569;
    background: #0f172a;
    color: #f8fafc;
    font-size: 0.7rem;
  }
</style>
