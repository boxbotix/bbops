<!--
  SPDX-License-Identifier: MIT
  Copyright (c) 2026 boxbotix
-->
<script lang="ts">
  import { onMount } from "svelte";
  import {
    computeFlightMetrics,
    formatLinkTierBadge,
    quaternionToEulerDeg,
  } from "$lib/hud/view-model";
  import type { VehicleEntityState } from "$lib/stores/skg.svelte";
  import type { TelemetryStore } from "$lib/stores/telemetry.svelte";

  interface Props {
    telemetryStore: TelemetryStore;
    vehicle: VehicleEntityState | null;
  }

  let { telemetryStore, vehicle }: Props = $props();

  let canvasEl: HTMLCanvasElement | undefined = $state();

  const linkBadge = $derived(
    vehicle ? formatLinkTierBadge(vehicle.linkTier) : null,
  );

  function drawHorizonAndTape(
    ctx: CanvasRenderingContext2D,
    widthPx: number,
    heightPx: number,
    activeVehicle: VehicleEntityState,
  ): void {
    const fast = telemetryStore.getKinematics(activeVehicle.sysid);
    const quat = fast ? fast.quat_wxyz : activeVehicle.attitudeQuaternion;
    const ned = fast ? fast.ned_xyz : activeVehicle.ned;
    const vel = fast ? fast.ned_vel : activeVehicle.velocityNed;

    const euler = quaternionToEulerDeg(quat);
    const flight = computeFlightMetrics(ned, vel);

    ctx.clearRect(0, 0, widthPx, heightPx);

    const tapeHeight = 28;
    const horizonTop = tapeHeight + 4;
    const horizonHeight = heightPx - horizonTop - 32;
    const cx = widthPx * 0.5;
    const cy = horizonTop + horizonHeight * 0.5;
    const pxPerPitchDeg = 2.4;

    // 1. Clipped artificial pitch/roll horizon box.
    ctx.save();
    ctx.beginPath();
    ctx.rect(8, horizonTop, widthPx - 16, Math.max(40, horizonHeight));
    ctx.clip();

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
    ctx.restore();

    // Fixed reference wings at center of horizon box.
    ctx.save();
    ctx.strokeStyle = "#38bdf8";
    ctx.lineWidth = 2.2;
    ctx.beginPath();
    ctx.moveTo(cx - 34, cy);
    ctx.lineTo(cx - 12, cy);
    ctx.lineTo(cx - 12, cy + 5);
    ctx.moveTo(cx + 34, cy);
    ctx.lineTo(cx + 12, cy);
    ctx.lineTo(cx + 12, cy + 5);
    ctx.stroke();
    ctx.restore();

    // 2. Top scrolling heading tape (0..360 deg).
    ctx.save();
    ctx.fillStyle = "#101726";
    ctx.fillRect(8, 4, widthPx - 16, tapeHeight - 4);
    ctx.strokeStyle = "#1e293b";
    ctx.strokeRect(8, 4, widthPx - 16, tapeHeight - 4);

    ctx.beginPath();
    ctx.rect(8, 4, widthPx - 16, tapeHeight - 4);
    ctx.clip();

    const pxPerYawDeg = 2.2;
    const baseTick = Math.floor(euler.yawDeg / 15) * 15;
    ctx.fillStyle = "#cbd5e1";
    ctx.strokeStyle = "#64748b";
    ctx.font = "10px ui-monospace, SFMono-Regular, Menlo, monospace";
    ctx.textAlign = "center";

    for (let offset = -90; offset <= 90; offset += 15) {
      const rawDeg = baseTick + offset;
      const wrappedDeg = ((rawDeg % 360) + 360) % 360;
      let delta = rawDeg - euler.yawDeg;
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

    // 3. Bottom telemetry readout bar (GS, VS, ALT, Roll/Pitch/Yaw).
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
    ctx.fillText(
      `R ${euler.rollDeg.toFixed(0)}° P ${euler.pitchDeg.toFixed(0)}° Y ${euler.yawDeg.toFixed(0)}°`,
      widthPx - 10,
      bottomY,
    );
    ctx.restore();
  }

  onMount(() => {
    let rafId = 0;
    const renderTick = (): void => {
      if (canvasEl && vehicle) {
        const widthPx = Math.max(220, canvasEl.clientWidth || 300);
        const heightPx = Math.max(150, canvasEl.clientHeight || 190);
        const dpr = Math.max(1, window.devicePixelRatio || 1);
        const targetW = Math.floor(widthPx * dpr);
        const targetH = Math.floor(heightPx * dpr);
        if (canvasEl.width !== targetW || canvasEl.height !== targetH) {
          canvasEl.width = targetW;
          canvasEl.height = targetH;
        }
        let ctx: CanvasRenderingContext2D | null = null;
        if (
          typeof navigator === "undefined" ||
          !/jsdom/i.test(navigator.userAgent)
        ) {
          try {
            ctx = canvasEl.getContext("2d");
          } catch {
            ctx = null;
          }
        }
        if (ctx) {
          ctx.setTransform(dpr, 0, 0, dpr, 0, 0);
          drawHorizonAndTape(ctx, widthPx, heightPx, vehicle);
        }
      }
      rafId = window.requestAnimationFrame(renderTick);
    };
    rafId = window.requestAnimationFrame(renderTick);
    return () => window.cancelAnimationFrame(rafId);
  });
</script>

<section class="hud-panel" data-testid="vehicle-hud">
  {#if vehicle && linkBadge}
    <header class="hud-header">
      <span class="hud-title">
        SYS#{vehicle.sysid} · {vehicle.domain}
      </span>
      <span class="link-badge" data-testid="hud-link-tier">
        {linkBadge.label}
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
    margin-bottom: 6px;
    font-family: ui-monospace, SFMono-Regular, Menlo, monospace;
    font-size: 12px;
  }

  .hud-title {
    font-weight: 600;
    color: var(--ui-text-primary, #e2e8f0);
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
