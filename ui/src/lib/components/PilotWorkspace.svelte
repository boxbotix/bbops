<!--
  SPDX-License-Identifier: MIT
  Copyright (c) 2026 boxbotix
-->
<script lang="ts">
  import type { Snippet } from "svelte";
  import TacticalCanvas from "$lib/components/TacticalCanvas.svelte";
  import VehicleHud from "$lib/components/VehicleHud.svelte";
  import {
    buildExternalContactSummary,
    buildHealthStrip,
    extractAbnormalHealthChips,
  } from "$lib/hud/view-model";
  import {
    type SkgStore,
    type VehicleEntityState,
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
    onSyncHash?: (hash: string) => void;
    // shortcut: Pilot guarded action slot renders read-only placeholder when unpopulated in M2.4, wire ADR-111 arm/disarm/mode/e-stop actions in M3.1 (#11)
    guardedActions?: Snippet<[VehicleEntityState]>;
  }

  let {
    telemetryStore,
    skgStore,
    viewportStore,
    onSyncHash,
    guardedActions,
  }: Props = $props();

  const selectedEntity = $derived(viewportStore.selectedEntity);
  const selectedVehicle = $derived(
    isVehicleEntity(selectedEntity) ? selectedEntity : null,
  );
  const selectedContact = $derived(
    isExternalContactEntity(selectedEntity) ? selectedEntity : null,
  );
  const commandableVehicle = $derived(viewportStore.selectedCommandableVehicle);
  const isPoisonedOwnshipSelected = $derived(
    Boolean(
      selectedVehicle?.poisoned ||
      (selectedEntity?.poisoned &&
        parseVehicleEntityId(selectedEntity.id) !== null),
    ),
  );

  const contactSummary = $derived(
    selectedContact ? buildExternalContactSummary(selectedContact) : null,
  );

  const abnormalChips = $derived(
    selectedVehicle
      ? extractAbnormalHealthChips(
          buildHealthStrip(
            selectedVehicle.healthFlags,
            selectedVehicle.sysStatus,
          ),
        )
      : [],
  );

  function toggleLayout(): void {
    viewportStore.togglePilotLayout();
    onSyncHash?.(viewportStore.toUrlHash());
  }
</script>

<div class="pilot-stage" data-testid="pilot-workspace">
  {#if contactSummary}
    <div
      class="pilot-contact-isolation"
      data-testid="pilot-contact-isolation"
      aria-label="External COP contact read-only view"
    >
      <div class="contact-card">
        <div class="contact-card-head">
          <span class="contact-badge">EXTERNAL CONTACT · READ-ONLY</span>
          <strong>{contactSummary.title}</strong>
        </div>
        <p class="isolation-copy">
          External COP tracks carry no BoxBotix 0xB1 authority or actuation
          channel. Vehicle PFD and guarded command surfaces are withheld.
        </p>
        <div class="contact-facts">
          <span>type: <strong>{contactSummary.contactType}</strong></span>
          <span>source: <strong>{contactSummary.source}</strong></span>
          <span>feed: <strong>{contactSummary.feedStatus}</strong></span>
          <span>observed: <strong>{contactSummary.observedAt}</strong></span>
          <span>received: <strong>{contactSummary.receivedAt}</strong></span>
          <span>expires: <strong>{contactSummary.expiresAt}</strong></span>
          <span
            >uncertainty: <strong>{contactSummary.uncertaintyLabel}</strong
            ></span
          >
        </div>
      </div>

      <div class="pilot-canvas-fill">
        <TacticalCanvas
          {telemetryStore}
          {skgStore}
          {viewportStore}
          {onSyncHash}
        />
      </div>
    </div>
  {:else}
    <div class="pilot-toolbar">
      <div class="pilot-toolbar-left">
        <span class="pilot-mode-label">
          PILOT STAGE ·
          {viewportStore.pilotLayout === "map"
            ? "MAP-PRIMARY + ATTITUDE PIP"
            : "50/50 PFD + MAP SPLIT"}
        </span>

        {#if abnormalChips.length > 0}
          <div
            class="abnormal-strip"
            data-testid="pilot-abnormal-strip"
            aria-label="Active vehicle health exceptions"
          >
            {#each abnormalChips as chip (chip.key)}
              <span
                class="exception-chip severity-{chip.severity}"
                data-chip-key={chip.key}
              >
                {chip.label}: <strong>{chip.value}</strong>
              </span>
            {/each}
          </div>
        {/if}
      </div>

      <button
        type="button"
        class="swap-btn"
        data-testid="pilot-layout-toggle"
        onclick={toggleLayout}
      >
        Swap Layout [M] →
        {viewportStore.pilotLayout === "map" ? "50/50 Split" : "Map-Primary"}
      </button>
    </div>

    {#if viewportStore.pilotLayout === "map"}
      <div class="pilot-map-primary" data-testid="pilot-map-primary">
        <TacticalCanvas
          {telemetryStore}
          {skgStore}
          {viewportStore}
          {onSyncHash}
        />

        {#if !viewportStore.lightsOut}
          <div class="pilot-pip-hud" data-testid="pilot-hud-pip">
            <VehicleHud
              {telemetryStore}
              vehicle={selectedVehicle}
              skgStatus={skgStore.status}
            />
          </div>

          {#if commandableVehicle}
            <div
              class="pilot-action-seam floating"
              data-testid="pilot-guarded-actions"
            >
              {#if guardedActions}
                {@render guardedActions(commandableVehicle)}
              {:else}
                <div class="seam-placeholder">
                  <span class="seam-title">GUARDED ACTUATION SEAM</span>
                  <span>
                    Actuation offline (M3.1) · UXS #{commandableVehicle.sysid} ({commandableVehicle.domain})
                  </span>
                </div>
              {/if}
            </div>
          {:else if isPoisonedOwnshipSelected}
            <div
              class="pilot-poisoned-guard"
              data-testid="pilot-poisoned-guard"
            >
              POISONED ENTITY — commandable actuation slot withheld.
            </div>
          {/if}
        {/if}
      </div>
    {:else}
      <div class="pilot-split-view" data-testid="pilot-split-view">
        <div class="pilot-pfd-column">
          <VehicleHud
            {telemetryStore}
            vehicle={selectedVehicle}
            skgStatus={skgStore.status}
          />

          {#if commandableVehicle}
            <div class="pilot-action-seam" data-testid="pilot-guarded-actions">
              {#if guardedActions}
                {@render guardedActions(commandableVehicle)}
              {:else}
                <div class="seam-placeholder">
                  <span class="seam-title">GUARDED ACTUATION SEAM</span>
                  <span>
                    Actuation offline (M3.1) · UXS #{commandableVehicle.sysid} ({commandableVehicle.domain})
                  </span>
                </div>
              {/if}
            </div>
          {:else if isPoisonedOwnshipSelected}
            <div
              class="pilot-poisoned-guard"
              data-testid="pilot-poisoned-guard"
            >
              POISONED ENTITY — commandable actuation slot withheld.
            </div>
          {/if}
        </div>

        <div class="pilot-canvas-fill">
          <TacticalCanvas
            {telemetryStore}
            {skgStore}
            {viewportStore}
            {onSyncHash}
          />
        </div>
      </div>
    {/if}
  {/if}
</div>

<style>
  .pilot-stage {
    flex: 1;
    display: flex;
    flex-direction: column;
    gap: 8px;
    min-height: 480px;
    font-family: ui-monospace, SFMono-Regular, Menlo, monospace;
  }

  .pilot-toolbar {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    justify-content: space-between;
    gap: 8px;
    padding: 6px 10px;
    background: var(--ui-surface-secondary, #101726);
    border: 1px solid var(--ui-border-subtle, #1e293b);
    border-radius: 6px;
    font-size: 11px;
  }

  .pilot-toolbar-left {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    gap: 10px;
  }

  .pilot-mode-label {
    font-size: 11px;
    font-weight: 700;
    color: var(--ui-text-secondary, #94a3b8);
  }

  .abnormal-strip {
    display: inline-flex;
    flex-wrap: wrap;
    gap: 6px;
  }

  .exception-chip {
    padding: 2px 7px;
    border-radius: 4px;
    font-size: 11px;
    border: 1px solid transparent;
  }

  .exception-chip.severity-error {
    color: var(--status-error, #ef4444);
    background: var(--status-error-bg, rgba(239, 68, 68, 0.16));
    border-color: var(--status-error, #ef4444);
  }

  .exception-chip.severity-warning {
    color: var(--status-warning, #f59e0b);
    background: var(--status-warning-bg, rgba(245, 158, 11, 0.16));
    border-color: var(--status-warning, #f59e0b);
  }

  .swap-btn {
    padding: 4px 10px;
    border-radius: 4px;
    border: 1px solid var(--ui-border-interactive, #3b82f6);
    background: rgba(59, 130, 246, 0.16);
    color: var(--ui-text-primary, #e2e8f0);
    font: inherit;
    font-size: 11px;
    font-weight: 600;
    cursor: pointer;
  }

  .pilot-map-primary {
    position: relative;
    flex: 1;
    display: flex;
    flex-direction: column;
    min-height: 460px;
  }

  .pilot-pip-hud {
    position: absolute;
    left: 12px;
    bottom: 12px;
    width: min(320px, calc(100% - 24px));
    z-index: 4;
    box-shadow: 0 10px 26px rgba(0, 0, 0, 0.55);
  }

  .pilot-action-seam {
    padding: 8px 10px;
    border-radius: 6px;
    border: 1px dashed var(--ui-border-strong, #334155);
    background: rgba(16, 23, 38, 0.92);
    font-size: 11px;
    color: var(--ui-text-secondary, #94a3b8);
  }

  .pilot-action-seam.floating {
    position: absolute;
    right: 12px;
    bottom: 12px;
    max-width: 320px;
    z-index: 4;
  }

  .seam-placeholder {
    display: flex;
    flex-direction: column;
    gap: 3px;
  }

  .seam-title {
    font-size: 10px;
    font-weight: 700;
    color: var(--ui-text-muted, #64748b);
  }

  .pilot-poisoned-guard {
    padding: 8px 10px;
    border-radius: 6px;
    border: 1px solid var(--status-error, #ef4444);
    background: rgba(239, 68, 68, 0.14);
    color: var(--status-error, #ef4444);
    font-size: 11px;
    font-weight: 700;
  }

  .pilot-split-view {
    flex: 1;
    display: grid;
    grid-template-columns: minmax(320px, 1fr) minmax(0, 1fr);
    gap: 8px;
    min-height: 460px;
  }

  .pilot-pfd-column,
  .pilot-canvas-fill {
    display: flex;
    flex-direction: column;
    gap: 8px;
    min-width: 0;
    min-height: 460px;
  }

  .pilot-contact-isolation {
    flex: 1;
    display: grid;
    grid-template-columns: 320px minmax(0, 1fr);
    gap: 8px;
    min-height: 460px;
  }

  .contact-card {
    display: flex;
    flex-direction: column;
    gap: 10px;
    padding: 12px;
    border-radius: 6px;
    background: var(--ui-surface-secondary, #101726);
    border: 1px solid rgba(34, 211, 238, 0.35);
    font-size: 11px;
  }

  .contact-card-head {
    display: flex;
    flex-direction: column;
    gap: 4px;
  }

  .contact-badge {
    align-self: flex-start;
    padding: 2px 6px;
    border-radius: 4px;
    background: rgba(34, 211, 238, 0.14);
    color: var(--skg-node-contact, #22d3ee);
    border: 1px solid rgba(34, 211, 238, 0.4);
    font-size: 10px;
    font-weight: 700;
  }

  .isolation-copy {
    margin: 0;
    color: var(--ui-text-secondary, #94a3b8);
    line-height: 1.4;
  }

  .contact-facts {
    display: flex;
    flex-direction: column;
    gap: 5px;
    color: var(--ui-text-secondary, #94a3b8);
  }

  @media (max-width: 960px) {
    .pilot-split-view,
    .pilot-contact-isolation {
      grid-template-columns: 1fr;
    }

    .pilot-pip-hud,
    .pilot-action-seam.floating {
      position: static;
      width: 100%;
      max-width: none;
    }
  }
</style>
