<!--
  SPDX-License-Identifier: MIT
  Copyright (c) 2026 boxbotix
-->
<script lang="ts">
  import { onMount } from "svelte";
  import {
    type TelemetrySample,
    TelemetryTraceBook,
    lookupMeasuredSysStatus,
  } from "$lib/bench/ring-buffer";
  import {
    BENCH_TUNING_SPECS,
    type TuningDiffEntry,
    computeDialProgress,
    computeTuningDiff,
    defaultTuningBaseline,
    revertTuningDraft,
    stageTuningValue,
  } from "$lib/bench/tuning-rack";
  import {
    buildExternalContactSummary,
    buildHealthStrip,
  } from "$lib/hud/view-model";
  import { formatTransitionEvent } from "$lib/skg/graphology-adapter";
  import {
    type SkgStore,
    isExternalContactEntity,
    isVehicleEntity,
  } from "$lib/stores/skg.svelte";
  import type { TelemetryStore } from "$lib/stores/telemetry.svelte";
  import type { ViewportStore } from "$lib/stores/viewport.svelte";
  import { parseVehicleEntityId } from "$lib/uxs";

  interface Props {
    telemetryStore: TelemetryStore;
    skgStore: SkgStore;
    viewportStore: ViewportStore;
    traceBook?: TelemetryTraceBook;
    onSyncHash?: (hash: string) => void;
    onApplyTuning?: (
      sysid: number,
      diffs: readonly TuningDiffEntry[],
    ) => void | Promise<void>;
  }

  let {
    telemetryStore,
    skgStore,
    viewportStore,
    traceBook = new TelemetryTraceBook(),
    onSyncHash,
    onApplyTuning,
  }: Props = $props();

  let scopeCanvasEl: HTMLCanvasElement | undefined = $state();
  let draftsByEntityId = $state<Record<string, Record<string, number>>>({});

  const selectedEntity = $derived(viewportStore.selectedEntity);
  const selectedVehicle = $derived(
    isVehicleEntity(selectedEntity) ? selectedEntity : null,
  );
  const selectedContact = $derived(
    isExternalContactEntity(selectedEntity) ? selectedEntity : null,
  );
  const contactSummary = $derived(
    selectedContact ? buildExternalContactSummary(selectedContact) : null,
  );
  const poisonedOwnshipId = $derived(
    selectedEntity?.poisoned ? parseVehicleEntityId(selectedEntity.id) : null,
  );
  const commandableVehicle = $derived(viewportStore.selectedCommandableVehicle);

  const healthChips = $derived(
    selectedVehicle && !selectedVehicle.poisoned
      ? buildHealthStrip(selectedVehicle.healthFlags, selectedVehicle.sysStatus)
      : [],
  );

  const baseline = $derived(
    commandableVehicle
      ? defaultTuningBaseline(commandableVehicle.domain)
      : null,
  );

  const activeDraft = $derived.by(() => {
    if (!commandableVehicle || !baseline) {
      return null;
    }
    return draftsByEntityId[commandableVehicle.id] ?? baseline;
  });

  const stagedDiffs = $derived(
    baseline && activeDraft
      ? computeTuningDiff(BENCH_TUNING_SPECS, baseline, activeDraft)
      : [],
  );

  const dialSpecs = BENCH_TUNING_SPECS.filter((s) => s.widget === "dial");
  const sliderSpecs = BENCH_TUNING_SPECS.filter((s) => s.widget === "slider");

  const eventRows = $derived(
    [...telemetryStore.events].reverse().map(formatTransitionEvent),
  );

  function handleParamInput(key: string, rawValue: number): void {
    if (!commandableVehicle || !baseline) {
      return;
    }
    const current = draftsByEntityId[commandableVehicle.id] ?? baseline;
    const next = stageTuningValue(BENCH_TUNING_SPECS, current, key, rawValue);
    draftsByEntityId = {
      ...draftsByEntityId,
      [commandableVehicle.id]: next,
    };
  }

  function handleRevert(): void {
    if (!commandableVehicle || !baseline) {
      return;
    }
    draftsByEntityId = {
      ...draftsByEntityId,
      [commandableVehicle.id]: revertTuningDraft(baseline),
    };
  }

  function handleApply(): void {
    if (!commandableVehicle || !onApplyTuning || stagedDiffs.length === 0) {
      return;
    }
    void onApplyTuning(commandableVehicle.sysid, stagedDiffs);
  }

  function drawTraceBand(
    ctx: CanvasRenderingContext2D,
    x: number,
    y: number,
    width: number,
    height: number,
    title: string,
    samples: readonly TelemetrySample[],
    series: ReadonlyArray<{
      label: string;
      color: string;
      pick: (s: TelemetrySample) => number | null;
    }>,
    symmetricZero: boolean,
  ): void {
    ctx.fillStyle = "#0b111e";
    ctx.fillRect(x, y, width, height);
    ctx.strokeStyle = "#1e293b";
    ctx.lineWidth = 1;
    ctx.strokeRect(x + 0.5, y + 0.5, width - 1, height - 1);

    ctx.fillStyle = "#94a3b8";
    ctx.font = "10px ui-monospace, SFMono-Regular, Menlo, monospace";
    ctx.fillText(title, x + 8, y + 14);

    if (samples.length === 0) {
      ctx.fillStyle = "#64748b";
      ctx.fillText(
        "Awaiting 20 Hz WS #1 telemetry samples…",
        x + 8,
        y + height * 0.55,
      );
      return;
    }

    let legendX = x + width - 8;
    ctx.textAlign = "right";
    for (let idx = series.length - 1; idx >= 0; idx -= 1) {
      const line = series[idx];
      if (!line) continue;
      const latest = line.pick(samples[samples.length - 1]!);
      const tag =
        latest !== null
          ? `${line.label}:${latest.toFixed(1)}`
          : `${line.label}:--`;
      ctx.fillStyle = line.color;
      ctx.fillText(tag, legendX, y + 14);
      legendX -= ctx.measureText(tag).width + 10;
    }
    ctx.textAlign = "left";

    let measuredCount = 0;
    let minVal = symmetricZero ? -1 : 0;
    let maxVal = 1;
    for (const sample of samples) {
      for (const line of series) {
        const v = line.pick(sample);
        if (v === null) continue;
        if (measuredCount === 0 && !symmetricZero) {
          minVal = v;
          maxVal = v;
        } else {
          if (v < minVal) minVal = v;
          if (v > maxVal) maxVal = v;
        }
        measuredCount += 1;
      }
    }

    if (measuredCount === 0) {
      ctx.fillStyle = "#64748b";
      ctx.fillText("No measured samples in window", x + 8, y + height * 0.55);
      return;
    }

    if (symmetricZero) {
      const bound = Math.max(Math.abs(minVal), Math.abs(maxVal), 1);
      minVal = -bound;
      maxVal = bound;
    } else {
      minVal = Math.min(0, minVal);
      if (maxVal - minVal < 1e-3) {
        maxVal = minVal + 1;
      }
    }

    const plotTop = y + 22;
    const plotHeight = Math.max(16, height - 30);
    const span = maxVal - minVal;

    if (symmetricZero) {
      const zeroY = plotTop + plotHeight * 0.5;
      ctx.strokeStyle = "rgba(148, 163, 184, 0.2)";
      ctx.beginPath();
      ctx.moveTo(x + 4, zeroY);
      ctx.lineTo(x + width - 4, zeroY);
      ctx.stroke();
    }

    for (const line of series) {
      ctx.strokeStyle = line.color;
      ctx.fillStyle = line.color;
      ctx.lineWidth = 1.5;
      ctx.beginPath();
      let runLength = 0;
      let hasLineSegment = false;
      let lastPx = 0;
      let lastPy = 0;
      for (let i = 0; i < samples.length; i += 1) {
        const sample = samples[i]!;
        const v = line.pick(sample);
        if (v === null) {
          if (runLength === 1) {
            ctx.fillRect(lastPx - 1.5, lastPy - 1.5, 3, 3);
          }
          runLength = 0;
          continue;
        }
        const px =
          samples.length === 1
            ? x + width * 0.5
            : x + 6 + (i / (samples.length - 1)) * (width - 12);
        const norm = (v - minVal) / span;
        const py = plotTop + (1 - norm) * plotHeight;
        if (runLength === 0) {
          ctx.moveTo(px, py);
          runLength = 1;
        } else {
          ctx.lineTo(px, py);
          runLength += 1;
          hasLineSegment = true;
        }
        lastPx = px;
        lastPy = py;
      }
      if (runLength === 1) {
        ctx.fillRect(lastPx - 1.5, lastPy - 1.5, 3, 3);
      }
      if (hasLineSegment) {
        ctx.stroke();
      }
    }
  }

  $effect(() => {
    return traceBook.attach(telemetryStore, (sysid) =>
      lookupMeasuredSysStatus(skgStore, sysid),
    );
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

  onMount(() => {
    if (typeof requestAnimationFrame === "undefined") {
      return;
    }

    let rafId = 0;
    let disposed = false;

    const renderFrame = (): void => {
      if (disposed) {
        return;
      }

      if (scopeCanvasEl) {
        const ctx = get2dContext(scopeCanvasEl);
        if (ctx) {
          const dpr = Math.max(
            1,
            (typeof window !== "undefined" ? window.devicePixelRatio : 1) || 1,
          );
          const cssWidth = Math.max(280, scopeCanvasEl.clientWidth || 560);
          const cssHeight = Math.max(260, scopeCanvasEl.clientHeight || 330);
          const targetW = Math.round(cssWidth * dpr);
          const targetH = Math.round(cssHeight * dpr);
          if (
            scopeCanvasEl.width !== targetW ||
            scopeCanvasEl.height !== targetH
          ) {
            scopeCanvasEl.width = targetW;
            scopeCanvasEl.height = targetH;
          }

          ctx.save();
          ctx.scale(dpr, dpr);
          ctx.fillStyle = "#090d16";
          ctx.fillRect(0, 0, cssWidth, cssHeight);

          const sysid =
            selectedVehicle?.sysid ?? poisonedOwnshipId?.sysid ?? null;
          const samples =
            sysid !== null
              ? (traceBook.getBuffer(sysid)?.toChronologicalSamples() ?? [])
              : [];

          const gap = 6;
          const bandHeight = Math.floor((cssHeight - gap * 2) / 3);

          drawTraceBand(
            ctx,
            0,
            0,
            cssWidth,
            bandHeight,
            "CH1 · BODY ANGULAR RATES (deg/s)",
            samples,
            [
              { label: "p", color: "#38bdf8", pick: (s) => s.rollRateDegS },
              { label: "q", color: "#a78bfa", pick: (s) => s.pitchRateDegS },
              { label: "r", color: "#22d3ee", pick: (s) => s.yawRateDegS },
            ],
            true,
          );

          drawTraceBand(
            ctx,
            0,
            bandHeight + gap,
            cssWidth,
            bandHeight,
            "CH2 · GROUNDSPEED & CLIMB (m/s)",
            samples,
            [
              { label: "GS", color: "#60a5fa", pick: (s) => s.groundspeedMps },
              { label: "VS", color: "#c084fc", pick: (s) => s.climbRateMps },
            ],
            true,
          );

          drawTraceBand(
            ctx,
            0,
            (bandHeight + gap) * 2,
            cssWidth,
            cssHeight - (bandHeight + gap) * 2,
            "CH3 · 0xB1 MCU LOOP & TRUNK RTT (us)",
            samples,
            [
              { label: "loop_dt", color: "#38bdf8", pick: (s) => s.loopDtUs },
              { label: "rtt", color: "#818cf8", pick: (s) => s.rttUs },
            ],
            false,
          );

          ctx.restore();
        }
      }

      rafId = requestAnimationFrame(renderFrame);
    };

    rafId = requestAnimationFrame(renderFrame);
    return () => {
      disposed = true;
      cancelAnimationFrame(rafId);
    };
  });
</script>

<div class="bench-stage" data-testid="bench-workspace">
  <section
    class="bench-health-bar"
    data-testid="bench-health-strip"
    aria-label="0xB1 MCU and companion trunk timing diagnostics"
  >
    <span class="bar-title">0xB1 MCU &amp; TRUNK DIAGNOSTICS</span>
    {#if selectedVehicle && !selectedVehicle.poisoned}
      <div class="chip-row">
        {#each healthChips as chip (chip.key)}
          <span
            class="diag-chip severity-{chip.severity}"
            data-chip-key={chip.key}
          >
            <span class="chip-label">{chip.label}:</span>
            <strong>{chip.value}</strong>
          </span>
        {/each}
      </div>
    {:else if poisonedOwnshipId}
      <span class="bar-muted" data-testid="bench-poisoned-notice">
        UXS #{poisonedOwnshipId.sysid} is POISONED — 0xB1 MCU diagnostics failed to
        decode.
      </span>
    {:else if contactSummary}
      <span class="bar-muted" data-testid="bench-contact-notice">
        External COP contact selected ({contactSummary.title}) — source: {contactSummary.source}
        · observed: {contactSummary.observedAt} · received: {contactSummary.receivedAt}
        · expires: {contactSummary.expiresAt} · uncertainty: {contactSummary.uncertaintyLabel}
        — 0xB1 MCU diagnostics apply only to ownship UXS vehicles.
      </span>
    {:else}
      <span class="bar-muted">
        Select an ownship UXS vehicle to inspect 0xB1 MCU loop timing and trunk
        CRC counters.
      </span>
    {/if}
  </section>

  <div class="bench-main-grid">
    <section
      class="scope-panel"
      aria-label="20 Hz telemetry stripchart oscilloscope"
    >
      <div class="panel-head">
        <strong>20 HZ TELEMETRY OSCILLOSCOPE (15 S RING · 300 SAMPLES)</strong>
        <span class="muted">
          {#if selectedVehicle && !selectedVehicle.poisoned}
            UXS #{selectedVehicle.sysid} · {selectedVehicle.domain}
          {:else if poisonedOwnshipId}
            UXS #{poisonedOwnshipId.sysid} · POISONED
          {:else}
            STANDBY
          {/if}
        </span>
      </div>
      <canvas
        bind:this={scopeCanvasEl}
        class="scope-canvas"
        data-testid="bench-oscilloscope"
        data-viewport-hotkey-scope="true"
        tabindex="-1"
        onpointerdown={() => scopeCanvasEl?.focus?.({ preventScroll: true })}
      ></canvas>
    </section>

    <section
      class="tuning-panel"
      data-testid="bench-tuning-rack"
      aria-label="Staged parameter tuning rack"
    >
      <div class="panel-head">
        <strong>STAGED TUNING RACK</strong>
        {#if commandableVehicle}
          <span class="diff-pill" data-testid="bench-staged-count">
            {stagedDiffs.length} staged
          </span>
        {/if}
      </div>

      {#if commandableVehicle && activeDraft && baseline}
        <div class="dial-row">
          {#each dialSpecs as spec (spec.key)}
            {@const val = activeDraft[spec.key] ?? spec.min}
            {@const progress = computeDialProgress(spec, val)}
            <label class="dial-card" data-param-key={spec.key}>
              <svg viewBox="0 0 64 46" class="dial-svg" aria-hidden="true">
                <path
                  d="M 10 40 A 24 24 0 1 1 54 40"
                  fill="none"
                  stroke="#1e293b"
                  stroke-width="6"
                  stroke-linecap="round"
                />
                <path
                  d="M 10 40 A 24 24 0 1 1 54 40"
                  fill="none"
                  stroke="#38bdf8"
                  stroke-width="6"
                  stroke-linecap="round"
                  pathLength="100"
                  stroke-dasharray="{Math.round(progress * 100)} 100"
                />
              </svg>
              <span class="param-label">{spec.label}</span>
              <strong class="param-readout">
                {val.toFixed(spec.decimals)}
                {spec.unit}
              </strong>
              <input
                type="range"
                min={spec.min}
                max={spec.max}
                step={spec.step}
                value={val}
                aria-label={spec.label}
                oninput={(ev) =>
                  handleParamInput(
                    spec.key,
                    Number((ev.currentTarget as HTMLInputElement).value),
                  )}
              />
            </label>
          {/each}
        </div>

        <div class="slider-list">
          {#each sliderSpecs as spec (spec.key)}
            {@const val = activeDraft[spec.key] ?? spec.min}
            {@const baseVal = baseline[spec.key] ?? spec.min}
            <label class="slider-row" data-param-key={spec.key}>
              <span class="slider-head">
                <span>{spec.label}</span>
                <strong>
                  {val.toFixed(spec.decimals)}
                  {spec.unit}
                  {#if Math.abs(val - baseVal) > 1e-9}
                    <em class="delta-tag">
                      (base {baseVal.toFixed(spec.decimals)})
                    </em>
                  {/if}
                </strong>
              </span>
              <input
                type="range"
                min={spec.min}
                max={spec.max}
                step={spec.step}
                value={val}
                aria-label={spec.label}
                oninput={(ev) =>
                  handleParamInput(
                    spec.key,
                    Number((ev.currentTarget as HTMLInputElement).value),
                  )}
              />
            </label>
          {/each}
        </div>

        <div class="tuning-footer">
          <button
            type="button"
            class="secondary-btn"
            data-testid="bench-tuning-revert"
            disabled={stagedDiffs.length === 0}
            onclick={handleRevert}
          >
            Revert Draft
          </button>
          <button
            type="button"
            class="primary-btn"
            data-testid="bench-tuning-apply"
            disabled={!onApplyTuning || stagedDiffs.length === 0}
            onclick={handleApply}
          >
            {onApplyTuning
              ? `Apply ${stagedDiffs.length} Staged`
              : "Apply Offline (M3.1)"}
          </button>
        </div>
        {#if !onApplyTuning}
          <p class="seam-note">
            Changes stage locally without outbound UDP packets until #11 (M3.1)
            wires ADR-111 non-retrying actuation.
          </p>
        {/if}
      {:else}
        <p class="withheld-note" data-testid="bench-tuning-withheld">
          {#if selectedContact}
            External COP contacts expose no parameter tuning rack.
          {:else if selectedVehicle?.poisoned}
            UXS #{selectedVehicle.sysid} is POISONED — parameter staging is withheld.
          {:else if poisonedOwnshipId}
            UXS #{poisonedOwnshipId.sysid} is POISONED — parameter staging is withheld.
          {:else}
            Select a commandable UXS vehicle to stage control and deadman
            parameters.
          {/if}
        </p>
      {/if}
    </section>
  </div>

  <section
    class="event-console"
    data-testid="bench-event-console"
    aria-label="Lane 3 transition and STATUSTEXT console"
  >
    <div class="panel-head">
      <strong
        >LANE 3 TRANSITION &amp; STATUSTEXT CONSOLE ({eventRows.length})</strong
      >
      {#if eventRows.length > 0}
        <button
          type="button"
          class="secondary-btn"
          onclick={() => telemetryStore.clearEvents()}
        >
          Clear Log
        </button>
      {/if}
    </div>
    {#if eventRows.length === 0}
      <p class="muted">No Lane 3 transition or STATUSTEXT frames received.</p>
    {:else}
      <ul class="console-list" data-testid="transition-event-timeline">
        {#each eventRows as row (row.key)}
          <li class="console-line">
            {#if row.entityId}
              <button
                type="button"
                class="event-btn"
                onclick={() => {
                  viewportStore.selectEntity(row.entityId);
                  onSyncHash?.(viewportStore.toUrlHash());
                }}
              >
                <span class="console-tag">{row.headline}</span>
                <span>{row.detail}</span>
              </button>
            {:else}
              <span class="console-tag">{row.headline}</span>
              <span>{row.detail}</span>
            {/if}
          </li>
        {/each}
      </ul>
    {/if}
  </section>
</div>

<style>
  .bench-stage {
    flex: 1;
    display: flex;
    flex-direction: column;
    gap: 8px;
    min-height: 480px;
    font-family: ui-monospace, SFMono-Regular, Menlo, monospace;
    font-size: 11px;
  }

  .bench-health-bar,
  .scope-panel,
  .tuning-panel,
  .event-console {
    padding: 10px;
    background: var(--ui-surface-secondary, #101726);
    border: 1px solid var(--ui-border-subtle, #1e293b);
    border-radius: 6px;
  }

  .bench-health-bar {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    gap: 10px;
  }

  .bar-title {
    font-size: 10px;
    font-weight: 700;
    letter-spacing: 0.05em;
    color: var(--ui-text-secondary, #94a3b8);
  }

  .bar-muted,
  .muted,
  .seam-note {
    margin: 0;
    color: var(--ui-text-muted, #64748b);
  }

  .chip-row {
    display: flex;
    flex-wrap: wrap;
    gap: 6px;
  }

  .diag-chip {
    display: inline-flex;
    gap: 4px;
    padding: 2px 7px;
    border-radius: 4px;
    border: 1px solid var(--ui-border-strong, #334155);
  }

  .diag-chip.severity-error {
    color: var(--status-error, #ef4444);
    background: var(--status-error-bg, rgba(239, 68, 68, 0.16));
    border-color: var(--status-error, #ef4444);
  }

  .diag-chip.severity-warning {
    color: var(--status-warning, #f59e0b);
    background: var(--status-warning-bg, rgba(245, 158, 11, 0.16));
    border-color: var(--status-warning, #f59e0b);
  }

  .diag-chip.severity-nominal {
    color: var(--ui-text-secondary, #94a3b8);
  }

  .bench-main-grid {
    flex: 1;
    display: grid;
    grid-template-columns: minmax(0, 1.35fr) minmax(320px, 1fr);
    gap: 8px;
    min-height: 340px;
  }

  .scope-panel,
  .tuning-panel {
    display: flex;
    flex-direction: column;
    gap: 10px;
    min-width: 0;
  }

  .panel-head {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: 8px;
  }

  .scope-canvas {
    flex: 1;
    width: 100%;
    min-height: 300px;
    border-radius: 4px;
    background: #090d16;
  }

  .diff-pill {
    padding: 2px 7px;
    border-radius: 999px;
    border: 1px solid var(--ui-border-interactive, #3b82f6);
    color: var(--ui-text-primary, #e2e8f0);
    font-size: 10px;
  }

  .dial-row {
    display: grid;
    grid-template-columns: repeat(3, minmax(0, 1fr));
    gap: 8px;
  }

  .dial-card {
    display: flex;
    flex-direction: column;
    align-items: center;
    gap: 4px;
    padding: 8px 6px;
    border-radius: 5px;
    background: var(--ui-surface-elevated, #172033);
    text-align: center;
  }

  .dial-svg {
    width: 56px;
    height: 40px;
  }

  .param-label {
    font-size: 10px;
    color: var(--ui-text-secondary, #94a3b8);
  }

  .param-readout {
    font-size: 11px;
    color: var(--ui-text-primary, #e2e8f0);
  }

  .dial-card input[type="range"],
  .slider-row input[type="range"] {
    width: 100%;
    accent-color: #38bdf8;
  }

  .slider-list {
    display: flex;
    flex-direction: column;
    gap: 8px;
  }

  .slider-row {
    display: flex;
    flex-direction: column;
    gap: 4px;
    padding: 6px 8px;
    border-radius: 5px;
    background: var(--ui-surface-elevated, #172033);
  }

  .slider-head {
    display: flex;
    justify-content: space-between;
    gap: 6px;
  }

  .delta-tag {
    font-style: normal;
    color: #38bdf8;
    font-size: 10px;
  }

  .tuning-footer {
    display: flex;
    justify-content: flex-end;
    gap: 8px;
  }

  .secondary-btn,
  .primary-btn {
    padding: 5px 10px;
    border-radius: 4px;
    font: inherit;
    font-size: 11px;
    cursor: pointer;
  }

  .secondary-btn {
    border: 1px solid var(--ui-border-strong, #334155);
    background: var(--ui-surface-elevated, #172033);
    color: var(--ui-text-secondary, #94a3b8);
  }

  .primary-btn {
    border: 1px solid var(--ui-border-interactive, #3b82f6);
    background: rgba(59, 130, 246, 0.2);
    color: var(--ui-text-primary, #e2e8f0);
    font-weight: 700;
  }

  .secondary-btn:disabled,
  .primary-btn:disabled {
    opacity: 0.45;
    cursor: not-allowed;
  }

  .withheld-note {
    margin: 0;
    padding: 10px;
    border-radius: 5px;
    background: var(--ui-surface-elevated, #172033);
    color: var(--ui-text-secondary, #94a3b8);
  }

  .console-list {
    list-style: none;
    margin: 8px 0 0;
    padding: 0;
    max-height: 140px;
    overflow-y: auto;
    display: flex;
    flex-direction: column;
    gap: 4px;
  }

  .console-line {
    display: flex;
    flex-wrap: wrap;
    gap: 8px;
    padding: 4px 6px;
    border-radius: 4px;
    background: var(--ui-surface-elevated, #172033);
  }

  .event-btn {
    display: flex;
    flex-wrap: wrap;
    gap: 8px;
    width: 100%;
    padding: 0;
    border: none;
    background: transparent;
    color: inherit;
    font: inherit;
    text-align: left;
    cursor: pointer;
  }

  .console-tag {
    color: #38bdf8;
    font-weight: 600;
  }

  @media (max-width: 980px) {
    .bench-main-grid {
      grid-template-columns: 1fr;
    }
  }
</style>
