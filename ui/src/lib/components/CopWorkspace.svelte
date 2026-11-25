<!--
  SPDX-License-Identifier: MIT
  Copyright (c) 2026 boxbotix
-->
<script lang="ts">
  import TacticalCanvas from "$lib/components/TacticalCanvas.svelte";
  import {
    type CopSoloSection,
    type HealthChipView,
    buildExternalContactSummary,
    buildHealthStrip,
    buildVehicleBannerView,
    computeFlightMetrics,
    evaluateActiveVehicleFault,
    evaluateVehicleSafetySeverity,
    extractAbnormalHealthChips,
    resolveLiveVehiclePose,
    selectCopSoloSection,
    summarizeFleetFaults,
  } from "$lib/hud/view-model";
  import {
    formatEntityTriples,
    formatTransitionEvent,
  } from "$lib/skg/graphology-adapter";
  import {
    type SkgStore,
    isExternalContactEntity,
    isVehicleEntity,
  } from "$lib/stores/skg.svelte";
  import type { TelemetryStore } from "$lib/stores/telemetry.svelte";
  import type {
    ViewportStore,
    WorkspaceMode,
  } from "$lib/stores/viewport.svelte";
  import { parseVehicleEntityId } from "$lib/uxs";

  interface Props {
    telemetryStore: TelemetryStore;
    skgStore: SkgStore;
    viewportStore: ViewportStore;
    onSyncHash?: (hash: string) => void;
  }

  let { telemetryStore, skgStore, viewportStore, onSyncHash }: Props = $props();

  let activeSection = $state<CopSoloSection>("summary");

  const selectedEntity = $derived(viewportStore.selectedEntity);
  const selectedVehicle = $derived(
    isVehicleEntity(selectedEntity) ? selectedEntity : null,
  );
  const selectedContact = $derived(
    isExternalContactEntity(selectedEntity) ? selectedEntity : null,
  );

  const vehicleSummary = $derived(
    selectedVehicle ? buildVehicleBannerView(selectedVehicle) : null,
  );
  const contactSummary = $derived(
    selectedContact ? buildExternalContactSummary(selectedContact) : null,
  );

  const livePose = $derived.by(() => {
    if (!selectedVehicle) {
      return null;
    }
    const fast = telemetryStore.getKinematics(selectedVehicle.sysid);
    return resolveLiveVehiclePose(selectedVehicle, {
      fast,
      telemetryStatus: telemetryStore.status,
      skgStatus: skgStore.status,
    });
  });

  const liveMetrics = $derived(
    livePose && livePose.hasLocalPos
      ? computeFlightMetrics(livePose.ned, livePose.vel)
      : null,
  );

  const fleetFaults = $derived(
    summarizeFleetFaults(skgStore.entities.values()),
  );
  const faultedOwnshipEntries = $derived.by(() => {
    const rows: Array<{
      id: string;
      sysid: number;
      statusLabel: string;
      poisoned: boolean;
      chips: readonly HealthChipView[];
    }> = [];
    for (const entity of skgStore.entities.values()) {
      if (entity.kind === "uxs-vehicle") {
        const fault = evaluateActiveVehicleFault(entity);
        if (fault.severity !== null) {
          rows.push({
            id: entity.id,
            sysid: entity.sysid,
            statusLabel: entity.authority,
            poisoned: entity.poisoned,
            chips: extractAbnormalHealthChips(
              buildHealthStrip(entity.healthFlags, entity.sysStatus),
            ),
          });
        }
      } else if (entity.poisoned) {
        const parsed = parseVehicleEntityId(entity.id);
        if (parsed !== null) {
          rows.push({
            id: entity.id,
            sysid: parsed.sysid,
            statusLabel: "decode-error",
            poisoned: true,
            chips: [],
          });
        }
      }
    }
    rows.sort((a, b) => a.sysid - b.sysid || a.id.localeCompare(b.id));
    return rows;
  });

  const recentEvents = $derived(
    [...telemetryStore.events]
      .reverse()
      .slice(0, 12)
      .map(formatTransitionEvent),
  );
  const tripleRows = $derived(formatEntityTriples(selectedEntity).slice(0, 16));

  function handleSelectEntity(entityId: string): void {
    viewportStore.selectEntity(entityId);
    onSyncHash?.(viewportStore.toUrlHash());
  }

  function switchWorkspace(mode: WorkspaceMode): void {
    viewportStore.setWorkspace(mode);
    onSyncHash?.(viewportStore.toUrlHash());
  }

  function setSoloSection(section: CopSoloSection): void {
    activeSection = selectCopSoloSection(activeSection, section);
  }
</script>

<div
  class="cop-stage"
  class:lights-out={viewportStore.lightsOut}
  data-testid="cop-workspace"
>
  {#if !viewportStore.lightsOut}
    <aside
      class="cop-roster"
      data-testid="cop-roster"
      aria-label="Fleet and external contact roster"
    >
      <div class="roster-group">
        <h2 class="rail-heading">OWNSHIP UXS ({skgStore.vehicles.length})</h2>
        {#if skgStore.vehicles.length === 0}
          <p class="rail-empty">No UXS vehicles ingested.</p>
        {:else}
          <ul class="roster-list">
            {#each skgStore.vehicles as vehicle (vehicle.id)}
              {@const sev = evaluateVehicleSafetySeverity(vehicle)}
              <li>
                <button
                  type="button"
                  class="roster-btn"
                  class:selected={viewportStore.selectedEntityId === vehicle.id}
                  data-testid="cop-roster-vehicle"
                  data-entity-id={vehicle.id}
                  onclick={() => handleSelectEntity(vehicle.id)}
                >
                  <span class="dot vehicle-dot severity-{sev}"></span>
                  <span class="roster-title">
                    #{vehicle.sysid}
                    {vehicle.title ?? vehicle.domain}
                  </span>
                  <span class="roster-sub">{vehicle.domain}</span>
                </button>
              </li>
            {/each}
          </ul>
        {/if}
      </div>

      <div class="roster-group">
        <h2 class="rail-heading">
          EXTERNAL CONTACTS ({skgStore.externalContacts.length})
        </h2>
        {#if skgStore.externalContacts.length === 0}
          <p class="rail-empty">No external contacts ingested.</p>
        {:else}
          <ul class="roster-list">
            {#each skgStore.externalContacts as contact (contact.id)}
              <li>
                <button
                  type="button"
                  class="roster-btn"
                  class:selected={viewportStore.selectedEntityId === contact.id}
                  data-testid="cop-roster-contact"
                  data-entity-id={contact.id}
                  onclick={() => handleSelectEntity(contact.id)}
                >
                  <span class="dot contact-dot"></span>
                  <span class="roster-title">{contact.title ?? contact.id}</span
                  >
                  <span class="roster-sub">{contact.feedStatus}</span>
                </button>
              </li>
            {/each}
          </ul>
        {/if}
      </div>
    </aside>
  {/if}

  <div class="cop-canvas-wrap">
    <TacticalCanvas {telemetryStore} {skgStore} {viewportStore} {onSyncHash} />
  </div>

  {#if !viewportStore.lightsOut}
    <aside
      class="cop-solo-rail"
      data-testid="cop-solo-rail"
      aria-label="COP Solo-Mode inspector"
    >
      <div class="solo-tabs" role="tablist" aria-label="COP inspector sections">
        <button
          type="button"
          role="tab"
          class="solo-tab"
          class:active={activeSection === "summary"}
          aria-selected={activeSection === "summary"}
          data-testid="cop-solo-tab-summary"
          onclick={() => setSoloSection("summary")}
        >
          Summary
        </button>
        <button
          type="button"
          role="tab"
          class="solo-tab"
          class:active={activeSection === "alerts"}
          aria-selected={activeSection === "alerts"}
          data-testid="cop-solo-tab-alerts"
          onclick={() => setSoloSection("alerts")}
        >
          Exceptions ({fleetFaults.faultedCount})
        </button>
        <button
          type="button"
          role="tab"
          class="solo-tab"
          class:active={activeSection === "skg"}
          aria-selected={activeSection === "skg"}
          data-testid="cop-solo-tab-skg"
          onclick={() => setSoloSection("skg")}
        >
          SKG ({tripleRows.length})
        </button>
      </div>

      {#if activeSection === "summary"}
        <section
          class="solo-panel"
          data-testid="cop-solo-panel-summary"
          aria-label="Selected track summary"
        >
          {#if vehicleSummary && selectedVehicle}
            <div class="summary-header">
              <strong>UXS #{vehicleSummary.sysid}</strong>
              <span class="muted">{vehicleSummary.domain}</span>
            </div>

            <dl class="kv-grid">
              <div>
                <dt>Authority</dt>
                <dd>{vehicleSummary.authorityBadge.label}</dd>
              </div>
              <div>
                <dt>Propulsion</dt>
                <dd>{vehicleSummary.armedBadge.label}</dd>
              </div>
              <div>
                <dt>Link Tier</dt>
                <dd>{vehicleSummary.linkBadge.label}</dd>
              </div>
              <div>
                <dt>Kinematics</dt>
                <dd>
                  {#if liveMetrics}
                    GS {liveMetrics.groundspeedMps.toFixed(1)} m/s · ALT {liveMetrics.altitudeRelM.toFixed(
                      1,
                    )} m
                  {:else}
                    NO POS
                  {/if}
                </dd>
              </div>
            </dl>

            <div class="jump-actions">
              <button
                type="button"
                class="jump-btn"
                onclick={() => switchWorkspace("pilot")}
              >
                Pilot [D]
              </button>
              <button
                type="button"
                class="jump-btn"
                onclick={() => switchWorkspace("bench")}
              >
                Bench [B]
              </button>
              <button
                type="button"
                class="jump-btn"
                onclick={() => switchWorkspace("cortex")}
              >
                Cortex [K]
              </button>
            </div>
          {:else if contactSummary}
            <div class="contact-summary-card" data-testid="cop-contact-summary">
              <div class="summary-header">
                <span class="contact-badge">EXTERNAL CONTACT</span>
                <strong>{contactSummary.title}</strong>
              </div>
              <dl class="kv-grid">
                <div>
                  <dt>Type</dt>
                  <dd>{contactSummary.contactType}</dd>
                </div>
                <div>
                  <dt>Source</dt>
                  <dd>{contactSummary.source}</dd>
                </div>
                <div>
                  <dt>Feed</dt>
                  <dd>{contactSummary.feedStatus}</dd>
                </div>
                <div>
                  <dt>Uncertainty</dt>
                  <dd>{contactSummary.uncertaintyLabel}</dd>
                </div>
                <div>
                  <dt>Observed</dt>
                  <dd>{contactSummary.observedAt}</dd>
                </div>
                <div>
                  <dt>Received</dt>
                  <dd>{contactSummary.receivedAt}</dd>
                </div>
                <div>
                  <dt>Expires</dt>
                  <dd>{contactSummary.expiresAt}</dd>
                </div>
              </dl>
              <div class="jump-actions">
                <button
                  type="button"
                  class="jump-btn"
                  onclick={() => switchWorkspace("cortex")}
                >
                  Inspect Provenance in Cortex [K]
                </button>
              </div>
            </div>
          {:else}
            <p class="rail-empty">
              Select a UXS vehicle or external contact on the canvas, roster, or
              filmstrip.
            </p>
          {/if}
        </section>
      {:else if activeSection === "alerts"}
        <section
          class="solo-panel"
          data-testid="cop-solo-panel-alerts"
          aria-label="Active fleet exceptions"
        >
          {#if faultedOwnshipEntries.length === 0}
            <p class="rail-empty">All ingested UXS vehicles nominal.</p>
          {:else}
            <ul class="exception-list">
              {#each faultedOwnshipEntries as entry (entry.id)}
                <li class="exception-card">
                  <button
                    type="button"
                    class="exception-select"
                    onclick={() => handleSelectEntity(entry.id)}
                  >
                    <strong>UXS #{entry.sysid}</strong>
                    <span>{entry.statusLabel}</span>
                  </button>
                  {#if entry.poisoned}
                    <span class="chip-error">POISONED</span>
                  {/if}
                  <div class="chip-wrap">
                    {#each entry.chips as chip (chip.key)}
                      <span class="chip-{chip.severity}">
                        {chip.label}: {chip.value}
                      </span>
                    {/each}
                  </div>
                </li>
              {/each}
            </ul>
          {/if}

          <h3 class="sub-heading">Recent Lane 3 Transitions</h3>
          {#if recentEvents.length === 0}
            <p class="rail-empty">No transition events recorded.</p>
          {:else}
            <ul class="event-list">
              {#each recentEvents as row (row.key)}
                <li class="event-item">
                  <span>{row.headline}</span>
                  <span class="muted">{row.detail}</span>
                </li>
              {/each}
            </ul>
          {/if}
        </section>
      {:else}
        <section
          class="solo-panel"
          data-testid="cop-solo-panel-skg"
          aria-label="Quick SKG assertions"
        >
          {#if !selectedEntity}
            <p class="rail-empty">Select an entity to inspect SKG triples.</p>
          {:else}
            <div class="skg-meta">
              <span>rev #{selectedEntity.kvRevision}</span>
              <button
                type="button"
                class="jump-btn"
                onclick={() => switchWorkspace("cortex")}
              >
                Open Cortex [K]
              </button>
            </div>
            <ul class="triple-list">
              {#each tripleRows as row, idx (`${row.predicate}:${idx}`)}
                <li class="triple-row">
                  <span class="triple-pred">{row.predicate}</span>
                  <strong class="triple-obj">{row.objectText}</strong>
                </li>
              {/each}
            </ul>
          {/if}
        </section>
      {/if}
    </aside>
  {/if}
</div>

<style>
  .cop-stage {
    flex: 1;
    display: grid;
    grid-template-columns: 220px minmax(0, 1fr) 310px;
    gap: 8px;
    min-height: 480px;
  }

  .cop-stage.lights-out {
    grid-template-columns: minmax(0, 1fr);
  }

  .cop-roster,
  .cop-solo-rail {
    display: flex;
    flex-direction: column;
    gap: 10px;
    padding: 10px;
    background: var(--ui-surface-secondary, #101726);
    border: 1px solid var(--ui-border-subtle, #1e293b);
    border-radius: 6px;
    font-family: ui-monospace, SFMono-Regular, Menlo, monospace;
    font-size: 11px;
    min-width: 0;
  }

  .cop-canvas-wrap {
    display: flex;
    flex-direction: column;
    min-height: 460px;
    min-width: 0;
  }

  .rail-heading,
  .sub-heading {
    margin: 0 0 6px;
    font-size: 10px;
    font-weight: 700;
    letter-spacing: 0.05em;
    color: var(--ui-text-secondary, #94a3b8);
  }

  .rail-empty,
  .muted {
    margin: 0;
    color: var(--ui-text-muted, #64748b);
  }

  .roster-list,
  .exception-list,
  .event-list,
  .triple-list {
    list-style: none;
    margin: 0;
    padding: 0;
    display: flex;
    flex-direction: column;
    gap: 4px;
  }

  .roster-btn {
    width: 100%;
    display: flex;
    align-items: center;
    gap: 6px;
    padding: 5px 7px;
    border-radius: 4px;
    border: 1px solid var(--ui-border-subtle, #1e293b);
    background: var(--ui-surface-elevated, #172033);
    color: var(--ui-text-primary, #e2e8f0);
    font: inherit;
    cursor: pointer;
    text-align: left;
  }

  .roster-btn.selected {
    border-color: var(--ui-border-interactive, #3b82f6);
  }

  .roster-title {
    flex: 1;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
    font-weight: 600;
  }

  .roster-sub {
    font-size: 10px;
    color: var(--ui-text-muted, #64748b);
  }

  .dot {
    width: 7px;
    height: 7px;
    border-radius: 999px;
    flex-shrink: 0;
  }

  .vehicle-dot.severity-error {
    background: var(--status-error, #ef4444);
  }

  .vehicle-dot.severity-warning {
    background: var(--status-warning, #f59e0b);
  }

  .vehicle-dot.severity-success {
    background: var(--status-success, #22c55e);
  }

  .vehicle-dot.severity-nominal {
    background: var(--ui-text-muted, #64748b);
  }

  .contact-dot {
    background: var(--skg-node-contact, #22d3ee);
  }

  .solo-tabs {
    display: grid;
    grid-template-columns: repeat(3, 1fr);
    gap: 4px;
  }

  .solo-tab {
    padding: 5px 6px;
    border-radius: 4px;
    border: 1px solid var(--ui-border-strong, #334155);
    background: var(--ui-surface-elevated, #172033);
    color: var(--ui-text-secondary, #94a3b8);
    font: inherit;
    font-size: 10px;
    font-weight: 700;
    cursor: pointer;
  }

  .solo-tab.active {
    border-color: var(--ui-border-interactive, #3b82f6);
    color: var(--ui-text-primary, #e2e8f0);
  }

  .solo-panel {
    display: flex;
    flex-direction: column;
    gap: 10px;
    min-width: 0;
  }

  .summary-header {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: 6px;
  }

  .contact-badge {
    padding: 2px 6px;
    border-radius: 4px;
    background: rgba(34, 211, 238, 0.14);
    color: var(--skg-node-contact, #22d3ee);
    border: 1px solid rgba(34, 211, 238, 0.4);
    font-size: 10px;
    font-weight: 700;
  }

  .kv-grid {
    margin: 0;
    display: grid;
    grid-template-columns: 1fr;
    gap: 5px;
  }

  .kv-grid div {
    display: flex;
    justify-content: space-between;
    gap: 8px;
    padding: 4px 6px;
    border-radius: 4px;
    background: var(--ui-surface-elevated, #172033);
  }

  .kv-grid dt {
    color: var(--ui-text-muted, #64748b);
  }

  .kv-grid dd {
    margin: 0;
    font-weight: 600;
    text-align: right;
  }

  .jump-actions {
    display: flex;
    flex-wrap: wrap;
    gap: 6px;
  }

  .jump-btn {
    padding: 4px 8px;
    border-radius: 4px;
    border: 1px solid var(--ui-border-interactive, #3b82f6);
    background: rgba(59, 130, 246, 0.14);
    color: var(--ui-text-primary, #e2e8f0);
    font: inherit;
    font-size: 10px;
    cursor: pointer;
  }

  .exception-card,
  .event-item,
  .triple-row {
    display: flex;
    flex-direction: column;
    gap: 4px;
    padding: 6px;
    border-radius: 4px;
    background: var(--ui-surface-elevated, #172033);
  }

  .exception-select {
    display: flex;
    justify-content: space-between;
    border: none;
    padding: 0;
    background: transparent;
    color: var(--ui-text-primary, #e2e8f0);
    font: inherit;
    cursor: pointer;
  }

  .chip-wrap {
    display: flex;
    flex-wrap: wrap;
    gap: 4px;
  }

  .chip-error {
    padding: 1px 5px;
    border-radius: 3px;
    font-size: 10px;
    color: var(--status-error, #ef4444);
    border: 1px solid var(--status-error, #ef4444);
  }

  .chip-warning {
    padding: 1px 5px;
    border-radius: 3px;
    font-size: 10px;
    color: var(--status-warning, #f59e0b);
    border: 1px solid var(--status-warning, #f59e0b);
  }

  .skg-meta {
    display: flex;
    align-items: center;
    justify-content: space-between;
  }

  .triple-pred {
    color: var(--ui-text-muted, #64748b);
    font-size: 10px;
  }

  .triple-obj {
    word-break: break-all;
  }

  @media (max-width: 1100px) {
    .cop-stage {
      grid-template-columns: 1fr;
    }
  }
</style>
