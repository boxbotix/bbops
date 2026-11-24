<!--
  SPDX-License-Identifier: MIT
  Copyright (c) 2026 boxbotix
-->
<script lang="ts">
  import { onMount } from "svelte";
  import {
    type ResolvedVehiclePose,
    computeFlightMetrics,
    formatLinkTierBadge,
    quaternionToEulerDeg,
    resolveLiveVehiclePose,
  } from "$lib/hud/view-model";
  import type { VehicleEntityState } from "$lib/stores/skg.svelte";
  import type { TelemetryStore } from "$lib/stores/telemetry.svelte";
  import type { WsConnectionStatus } from "$lib/stores/ws-client";

  interface Props {
    telemetryStore: TelemetryStore;
    vehicle: VehicleEntityState | null;
    skgStatus?: WsConnectionStatus;
    now?: () => number;
  }

  let {
    telemetryStore,
    vehicle,
    skgStatus,
    now = () => Date.now(),
  }: Props = $props();

  let canvasEl: HTMLCanvasElement | undefined = $state();
  let poseSource = $state<"ws1" | "skg">("skg");
  let isStale = $state(false);
  let hasAttitude = $state(false);

  const linkBadge = $derived(
    vehicle ? formatLinkTierBadge(vehicle.linkTier) : null,
  );

  function syncDiscreteHudState(pose: ResolvedVehiclePose): void {
    if (poseSource !== pose.source) {
      poseSource = pose.source;
    }
    if (isStale !== pose.stale) {
      isStale = pose.stale;
    }
    if (hasAttitude !== pose.hasAttitude) {
      hasAttitude = pose.hasAttitude;
    }
  }

  $effect(() => {
    if (!vehicle) {
      isStale = false;
      hasAttitude = false;
      poseSource = "skg";
      return;
    }
    const pose = resolveLiveVehiclePose(vehicle, {
      fast: telemetryStore.getKinematics(vehicle.sysid),
      telemetryStatus: telemetryStore.status,
      skgStatus,
      nowMs: now(),
    });
    syncDiscreteHudState(pose);
  });

  function get2dContext(
    canvas: HTMLCanvasElement,
  ): CanvasRenderingContext2D | null {
    const isJsdom =
      typeof navigator !== "undefined" && /jsdom/i.test(navigator.userAgent);
    const isMockedContext =
      typeof canvas.getContext === "function" && "mock" in canvas.getContext;
    if (isJsdom && !isMockedContext) {
      return null;
    }
    try {
      return canvas.getContext("2d");
    } catch {
      return null;
    }
  }

  function drawHorizonAndTape(
    ctx: CanvasRenderingContext2D,
    widthPx: number,
    heightPx: number,
    pose: ResolvedVehiclePose,
  ): void {
    const euler = pose.hasAttitude ? quaternionToEulerDeg(pose.quat) : null;
    const flight = computeFlightMetrics(pose.ned, pose.vel);

    ctx.clearRect(0, 0, widthPx, heightPx);

    const tapeHeight = 28;
    const horizonTop = tapeHeight + 4;
    const horizonHeight = Math.max(40, heightPx - horizonTop - 32);
    const cx = widthPx * 0.5;
    const cy = horizonTop + horizonHeight * 0.5;
    const pxPerPitchDeg = 2.4;

    // 1. Clipped artificial pitch/roll horizon box (or neutral unavailable box pre-#31).
    ctx.save();
    ctx.beginPath();
    ctx.rect(8, horizonTop, widthPx - 16, horizonHeight);
    ctx.clip();

    if (euler !== null) {
      ctx.translate(cx, cy);
      ctx.rotate(-euler.rollRad);
      const pitchOffsetPx = euler.pitchDeg * pxPerPitchDeg;

      // Sky & earth halves.
      ctx.fillStyle = "#0f2744";
      ctx.fillRect(
        -widthPx,
        -heightPx * 2 + pitchOffsetPx,
        widthPx * 2,
        heightPx * 2,
      );
      ctx.fillStyle = "#2b1f16";
      ctx.fillRect(-widthPx, pitchOffsetPx, widthPx * 2, heightPx * 2);

      // Horizon line.
      ctx.strokeStyle = "#e2e8f0";
      ctx.lineWidth = 1.8;
      ctx.beginPath();
      ctx.moveTo(-widthPx, pitchOffsetPx);
      ctx.lineTo(widthPx, pitchOffsetPx);
      ctx.stroke();

      // Pitch ladder (+/-10, +/-20, +/-30 deg).
      ctx.strokeStyle = "rgba(226, 232, 240, 0.72)";
      ctx.fillStyle = "rgba(226, 232, 240, 0.82)";
      ctx.lineWidth = 1.2;
      ctx.font = "10px ui-monospace, SFMono-Regular, Menlo, monospace";
      for (const deg of [-30, -20, -10, 10, 20, 30]) {
        const y = pitchOffsetPx - deg * pxPerPitchDeg;
        const halfSpan = Math.abs(deg) === 10 ? 18 : 26;
        ctx.beginPath();
        ctx.moveTo(-halfSpan, y);
        ctx.lineTo(halfSpan, y);
        ctx.stroke();
        ctx.fillText(String(deg), halfSpan + 4, y + 3);
      }
    } else {
      ctx.fillStyle = "#131c2e";
      ctx.fillRect(8, horizonTop, widthPx - 16, horizonHeight);
      ctx.strokeStyle = "rgba(148, 163, 184, 0.4)";
      ctx.lineWidth = 1;
      ctx.setLineDash([4, 4]);
      ctx.beginPath();
      ctx.moveTo(16, cy);
      ctx.lineTo(widthPx - 16, cy);
      ctx.stroke();
      ctx.setLineDash([]);
      ctx.fillStyle = "#94a3b8";
      ctx.font = "bold 11px ui-monospace, SFMono-Regular, Menlo, monospace";
      ctx.textAlign = "center";
      ctx.fillText("ATTITUDE UNAVAILABLE", cx, cy - 10);
    }
    ctx.restore();

    // Fixed reference wings at center of horizon box.
    ctx.save();
    ctx.strokeStyle = euler !== null ? "#38bdf8" : "#64748b";
    ctx.lineWidth = 2.2;
    ctx.beginPath();
    ctx.moveTo(cx - 34, cy);
    ctx.lineTo(cx - 12, cy);
    ctx.lineTo(cx - 12, cy + 5);
    ctx.moveTo(cx + 34, cy);
    ctx.lineTo(cx + 12, cy);
    ctx.lineTo(cx + 12, cy + 5);
    ctx.stroke();
    if (pose.stale) {
      ctx.fillStyle = "#f59e0b";
      ctx.font = "bold 10px ui-monospace, SFMono-Regular, Menlo, monospace";
      ctx.textAlign = "center";
      ctx.fillText("STALE TELEMETRY", cx, horizonTop + 14);
    }
    ctx.restore();

    // 2. Top scrolling heading tape (0..360 deg) driven by tracker-resolved heading.
    ctx.save();
    ctx.fillStyle = "#101726";
    ctx.fillRect(8, 4, widthPx - 16, tapeHeight - 4);
    ctx.strokeStyle = "#1e293b";
    ctx.strokeRect(8, 4, widthPx - 16, tapeHeight - 4);

    ctx.beginPath();
    ctx.rect(8, 4, widthPx - 16, tapeHeight - 4);
    ctx.clip();

    const pxPerYawDeg = 2.2;
    const headingDeg = pose.headingDeg;
    const baseTick = Math.floor(headingDeg / 15) * 15;
    ctx.fillStyle = "#cbd5e1";
    ctx.strokeStyle = "#64748b";
    ctx.font = "10px ui-monospace, SFMono-Regular, Menlo, monospace";
    ctx.textAlign = "center";

    for (let offset = -90; offset <= 90; offset += 15) {
      const rawDeg = baseTick + offset;
      const wrappedDeg = ((rawDeg % 360) + 360) % 360;
      let delta = rawDeg - headingDeg;
      if (delta > 180) delta -= 360;
      if (delta < -180) delta += 360;
      const tx = cx + delta * pxPerYawDeg;
      ctx.beginPath();
      ctx.moveTo(tx, 20);
      ctx.lineTo(tx, 26);
      ctx.stroke();

      const label =
        wrappedDeg === 0
          ? "N"
          : wrappedDeg === 90
            ? "E"
            : wrappedDeg === 180
              ? "S"
              : wrappedDeg === 270
                ? "W"
                : String(wrappedDeg).padStart(3, "0");
      ctx.fillText(label, tx, 16);
    }
    // Center lubber bug.
    ctx.strokeStyle = "#38bdf8";
    ctx.beginPath();
    ctx.moveTo(cx, 4);
    ctx.lineTo(cx, 28);
    ctx.stroke();
    ctx.restore();

    // 3. Bottom telemetry readout bar (GS, VS, ALT, Roll/Pitch/Yaw or Heading).
    ctx.save();
    ctx.fillStyle = "#e2e8f0";
    ctx.font = "11px ui-monospace, SFMono-Regular, Menlo, monospace";
    const bottomY = heightPx - 10;
    ctx.fillText(
      `GS ${flight.groundspeedMps.toFixed(1)}m/s  VS ${flight.climbRateMps >= 0 ? "+" : ""}${flight.climbRateMps.toFixed(1)}m/s  ALT ${flight.altitudeRelM.toFixed(1)}m`,
      10,
      bottomY,
    );
    ctx.textAlign = "right";
    ctx.fillStyle = "#94a3b8";
    if (euler !== null) {
      ctx.fillText(
        `R ${euler.rollDeg.toFixed(0)}° P ${euler.pitchDeg.toFixed(0)}° Y ${euler.yawDeg.toFixed(0)}°`,
        widthPx - 10,
        bottomY,
      );
    } else {
      const hdgLabel = String(Math.round(headingDeg) % 360).padStart(3, "0");
      ctx.fillText(`R -- P -- HDG ${hdgLabel}°`, widthPx - 10, bottomY);
    }
    ctx.restore();
  }

  onMount(() => {
    let rafId = 0;
    const renderTick = (): void => {
      if (vehicle) {
        const nowMs = now();
        const pose = resolveLiveVehiclePose(vehicle, {
          fast: telemetryStore.getKinematics(vehicle.sysid),
          telemetryStatus: telemetryStore.status,
          skgStatus,
          nowMs,
        });
        syncDiscreteHudState(pose);
        if (canvasEl) {
          const widthPx = Math.max(220, canvasEl.clientWidth || 300);
          const heightPx = Math.max(150, canvasEl.clientHeight || 190);
          const dpr = Math.max(1, window.devicePixelRatio || 1);
          const targetW = Math.floor(widthPx * dpr);
          const targetH = Math.floor(heightPx * dpr);
          if (canvasEl.width !== targetW || canvasEl.height !== targetH) {
            canvasEl.width = targetW;
            canvasEl.height = targetH;
          }
          const ctx = get2dContext(canvasEl);
          if (ctx) {
            ctx.setTransform(dpr, 0, 0, dpr, 0, 0);
            drawHorizonAndTape(ctx, widthPx, heightPx, pose);
          }
        }
      }
      rafId = window.requestAnimationFrame(renderTick);
    };
    rafId = window.requestAnimationFrame(renderTick);
    return () => window.cancelAnimationFrame(rafId);
  });
</script>

<section
  class="hud-panel"
  data-testid="vehicle-hud"
  data-pose-source={vehicle ? poseSource : undefined}
  data-stale={vehicle ? (isStale ? "true" : "false") : undefined}
  data-attitude={vehicle
    ? hasAttitude
      ? "measured"
      : "unavailable"
    : undefined}
>
  {#if vehicle && linkBadge}
    <header class="hud-header">
      <span class="hud-title">
        SYS#{vehicle.sysid} · {vehicle.domain}
      </span>
      <span class="hud-badges">
        {#if isStale}
          <span class="stale-badge" data-testid="hud-stale-badge">STALE</span>
        {/if}
        {#if !hasAttitude}
          <span class="no-att-badge" data-testid="hud-attitude-unavailable">
            NO ATT
          </span>
        {/if}
        <span class="link-badge" data-testid="hud-link-tier">
          {linkBadge.label}
        </span>
      </span>
    </header>
    <canvas
      bind:this={canvasEl}
      class="hud-canvas"
      aria-label="Primary attitude horizon and heading tape"
    ></canvas>
  {:else}
    <div class="hud-empty" data-testid="vehicle-hud-standby">
      Select a controllable UXS vehicle to view live attitude horizon &amp; link
      tier.
    </div>
  {/if}
</section>

<style>
  .hud-panel {
    display: flex;
    flex-direction: column;
    background: var(--ui-surface-secondary, #101726);
    border: 1px solid var(--ui-border-subtle, #1e293b);
    border-radius: 6px;
    padding: 8px;
    min-height: 220px;
  }

  .hud-header {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: 6px;
    margin-bottom: 6px;
    font-family: ui-monospace, SFMono-Regular, Menlo, monospace;
    font-size: 12px;
  }

  .hud-title {
    font-weight: 600;
    color: var(--ui-text-primary, #e2e8f0);
  }

  .hud-badges {
    display: inline-flex;
    align-items: center;
    gap: 4px;
  }

  .stale-badge {
    padding: 2px 6px;
    border-radius: 999px;
    background: rgba(245, 158, 11, 0.18);
    color: var(--status-warning, #f59e0b);
    border: 1px solid rgba(245, 158, 11, 0.45);
    font-size: 10px;
    font-weight: 700;
  }

  .no-att-badge {
    padding: 2px 6px;
    border-radius: 999px;
    background: rgba(148, 163, 184, 0.16);
    color: var(--ui-text-secondary, #94a3b8);
    border: 1px solid rgba(148, 163, 184, 0.35);
    font-size: 10px;
  }

  .link-badge {
    padding: 2px 8px;
    border-radius: 999px;
    background: var(--status-info-bg, rgba(56, 189, 248, 0.16));
    color: var(--status-info, #38bdf8);
    border: 1px solid rgba(56, 189, 248, 0.4);
    font-size: 11px;
  }

  .hud-canvas {
    width: 100%;
    height: 184px;
    display: block;
    border-radius: 4px;
  }

  .hud-empty {
    flex: 1;
    display: flex;
    align-items: center;
    justify-content: center;
    text-align: center;
    color: var(--ui-text-muted, #64748b);
    font-size: 12px;
    padding: 16px;
  }
</style>
