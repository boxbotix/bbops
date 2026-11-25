<!--
  SPDX-License-Identifier: MIT
  Copyright (c) 2026 boxbotix
-->
<script lang="ts">
  import { TelemetryTraceBook } from "$lib/bench/ring-buffer";
  import { formatShellBanner } from "$lib/bootstrap";
  import AuthorityBanner from "$lib/components/AuthorityBanner.svelte";
  import BenchWorkspace from "$lib/components/BenchWorkspace.svelte";
  import CopWorkspace from "$lib/components/CopWorkspace.svelte";
  import CortexWorkspace from "$lib/components/CortexWorkspace.svelte";
  import FleetFilmstrip from "$lib/components/FleetFilmstrip.svelte";
  import PilotWorkspace from "$lib/components/PilotWorkspace.svelte";
  import {
    type GraphSearchClient,
    createGraphSearchClient,
  } from "$lib/cortex/search-client";
  import { type GraphQLClient, createGraphQLClient } from "$lib/graphql";
  import {
    buildFilmstripItems,
    summarizeFleetFaults,
  } from "$lib/hud/view-model";
  import type { SkgStore } from "$lib/stores/skg.svelte";
  import type { TelemetryStore } from "$lib/stores/telemetry.svelte";
  import type {
    ViewportStore,
    WorkspaceMode,
  } from "$lib/stores/viewport.svelte";

  interface Props {
    telemetryStore: TelemetryStore;
    skgStore: SkgStore;
    viewportStore: ViewportStore;
    graphqlClient?: GraphQLClient;
    searchClient?: GraphSearchClient;
    traceBook?: TelemetryTraceBook;
    onSyncHash?: (hash: string) => void;
  }

  let {
    telemetryStore,
    skgStore,
    viewportStore,
    graphqlClient = createGraphQLClient(),
    searchClient = createGraphSearchClient(),
    traceBook = new TelemetryTraceBook(),
    onSyncHash,
  }: Props = $props();

  const banner = $derived(formatShellBanner("v0.1.0-dev"));
  const fleetFaults = $derived(
    summarizeFleetFaults(skgStore.entities.values()),
  );
  const filmstripItems = $derived(
    buildFilmstripItems(
      skgStore.vehicles,
      skgStore.externalContacts,
      skgStore.entities.values(),
    ),
  );

  $effect(() => {
    return traceBook.attach(
      telemetryStore,
      (sysid) =>
        skgStore.vehicles.find((v) => v.sysid === sysid)?.sysStatus ?? null,
    );
  });

  const WORKSPACE_TABS: ReadonlyArray<{
    mode: WorkspaceMode;
    label: string;
    keycap: string;
  }> = [
    { mode: "cop", label: "COP", keycap: "G" },
    { mode: "pilot", label: "Pilot", keycap: "D" },
    { mode: "bench", label: "Bench", keycap: "B" },
    { mode: "cortex", label: "Cortex", keycap: "K" },
  ];

  function selectWorkspace(mode: WorkspaceMode): void {
    viewportStore.setWorkspace(mode);
    onSyncHash?.(viewportStore.toUrlHash());
  }

  function toggleLightsOut(): void {
    viewportStore.setLightsOut(!viewportStore.lightsOut);
    onSyncHash?.(viewportStore.toUrlHash());
  }

  function jumpToFirstFault(): void {
    if (!fleetFaults.firstFaultEntityId) {
      return;
    }
    viewportStore.selectEntity(fleetFaults.firstFaultEntityId);
    onSyncHash?.(viewportStore.toUrlHash());
  }
</script>

<main
  class="workspace-shell"
  data-testid="workspace-shell"
  data-workspace={viewportStore.workspace}
>
  <header
    class="invariant-topbar"
    data-testid="invariant-safety-topbar"
    aria-label="Invariant safety and workspace header"
  >
    <div class="topbar-brand-cluster">
      <h1 class="workspace-title">{banner}</h1>

      <div class="stream-pills" aria-label="Stream connection status">
        <span class="stream-pill" data-status={telemetryStore.status}>
          WS1: <strong>{telemetryStore.status}</strong>
          ({telemetryStore.vehicleCount})
        </span>
        <span class="stream-pill" data-status={skgStore.status}>
          WS2: <strong>{skgStore.status}</strong> ({skgStore.entities
            .size}{skgStore.synced ? " · synced" : ""})
        </span>
      </div>

      {#if fleetFaults.faultedCount > 0}
        <button
          type="button"
          class="fleet-fault-btn {fleetFaults.errorCount > 0
            ? 'severity-error'
            : 'severity-warning'}"
          data-testid="fleet-fault-badge"
          onclick={jumpToFirstFault}
        >
          ! {fleetFaults.faultedCount} FLEET FAULT{fleetFaults.faultedCount ===
          1
            ? ""
            : "S"}
          {#if fleetFaults.firstFaultLabel}
            ({fleetFaults.firstFaultLabel})
          {/if}
        </button>
      {:else}
        <span class="fleet-nominal-pill" data-testid="fleet-nominal-badge">
          FLEET NOMINAL ({fleetFaults.totalVehicles})
        </span>
      {/if}
    </div>

    <div class="topbar-authority-slot">
      <AuthorityBanner entity={viewportStore.selectedEntity} compact={true} />
    </div>

    <nav class="workspace-switcher" aria-label="Task workspaces">
      {#each WORKSPACE_TABS as tab (tab.mode)}
        {@const active = viewportStore.workspace === tab.mode}
        <button
          type="button"
          class="mode-btn"
          class:active
          data-testid="workspace-tab-{tab.mode}"
          aria-pressed={active}
          onclick={() => selectWorkspace(tab.mode)}
        >
          <span>{tab.label}</span>
          <kbd>{tab.keycap}</kbd>
        </button>
      {/each}

      <button
        type="button"
        class="chrome-btn"
        class:active={viewportStore.lightsOut}
        data-testid="lights-out-toggle"
        aria-pressed={viewportStore.lightsOut}
        onclick={toggleLightsOut}
      >
        Lights-Out <kbd>Tab</kbd>
      </button>
    </nav>
  </header>

  <div class="workspace-stage-host">
    {#if viewportStore.workspace === "cop"}
      <CopWorkspace {telemetryStore} {skgStore} {viewportStore} {onSyncHash} />
    {:else if viewportStore.workspace === "pilot"}
      <PilotWorkspace
        {telemetryStore}
        {skgStore}
        {viewportStore}
        {onSyncHash}
      />
    {:else if viewportStore.workspace === "bench"}
      <BenchWorkspace
        {telemetryStore}
        {skgStore}
        {viewportStore}
        {traceBook}
        {onSyncHash}
      />
    {:else}
      <CortexWorkspace
        {skgStore}
        {viewportStore}
        {graphqlClient}
        {searchClient}
        {onSyncHash}
      />
    {/if}
  </div>

  {#if !viewportStore.lightsOut}
    <FleetFilmstrip items={filmstripItems} {viewportStore} {onSyncHash} />
  {/if}
</main>

<style>
  .workspace-shell {
    display: flex;
    flex-direction: column;
    gap: 8px;
    padding: 10px;
    min-height: 100vh;
    background: var(--ui-surface-primary, #090d16);
    color: var(--ui-text-primary, #e2e8f0);
  }

  .invariant-topbar {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    justify-content: space-between;
    gap: 8px;
    padding: 6px 10px;
    background: var(--ui-surface-secondary, #101726);
    border: 1px solid var(--ui-border-subtle, #1e293b);
    border-radius: 6px;
  }

  .topbar-brand-cluster {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    gap: 8px;
  }

  .workspace-title {
    margin: 0;
    font-size: 13px;
    font-weight: 700;
    font-family: ui-monospace, SFMono-Regular, Menlo, monospace;
  }

  .stream-pills {
    display: inline-flex;
    flex-wrap: wrap;
    gap: 6px;
    font-family: ui-monospace, SFMono-Regular, Menlo, monospace;
    font-size: 11px;
  }

  .stream-pill,
  .fleet-nominal-pill {
    padding: 2px 8px;
    border-radius: 999px;
    background: var(--ui-surface-elevated, #172033);
    border: 1px solid var(--ui-border-strong, #334155);
    color: var(--ui-text-secondary, #94a3b8);
    font-family: ui-monospace, SFMono-Regular, Menlo, monospace;
    font-size: 11px;
  }

  .stream-pill[data-status="connected"] {
    border-color: var(--ui-border-interactive, #3b82f6);
    color: var(--ui-text-primary, #e2e8f0);
  }

  .fleet-fault-btn {
    padding: 3px 9px;
    border-radius: 999px;
    font-family: ui-monospace, SFMono-Regular, Menlo, monospace;
    font-size: 11px;
    font-weight: 700;
    cursor: pointer;
  }

  .fleet-fault-btn.severity-error {
    color: var(--status-error, #ef4444);
    background: var(--status-error-bg, rgba(239, 68, 68, 0.18));
    border: 1px solid var(--status-error, #ef4444);
  }

  .fleet-fault-btn.severity-warning {
    color: var(--status-warning, #f59e0b);
    background: var(--status-warning-bg, rgba(245, 158, 11, 0.18));
    border: 1px solid var(--status-warning, #f59e0b);
  }

  .topbar-authority-slot {
    flex: 1 1 280px;
    display: flex;
    justify-content: center;
    min-width: 0;
  }

  .workspace-switcher {
    display: inline-flex;
    flex-wrap: wrap;
    align-items: center;
    gap: 4px;
    font-family: ui-monospace, SFMono-Regular, Menlo, monospace;
    font-size: 11px;
  }

  .mode-btn,
  .chrome-btn {
    display: inline-flex;
    align-items: center;
    gap: 5px;
    padding: 4px 8px;
    border-radius: 5px;
    border: 1px solid var(--ui-border-strong, #334155);
    background: var(--ui-surface-elevated, #172033);
    color: var(--ui-text-secondary, #94a3b8);
    font: inherit;
    font-weight: 600;
    cursor: pointer;
  }

  .mode-btn.active,
  .chrome-btn.active {
    border-color: var(--ui-border-interactive, #3b82f6);
    background: rgba(59, 130, 246, 0.2);
    color: var(--ui-text-primary, #e2e8f0);
  }

  .mode-btn kbd,
  .chrome-btn kbd {
    padding: 0 4px;
    border-radius: 3px;
    background: rgba(9, 13, 22, 0.7);
    border: 1px solid var(--ui-border-subtle, #1e293b);
    font-size: 10px;
    color: var(--ui-text-muted, #64748b);
  }

  .workspace-stage-host {
    flex: 1;
    display: flex;
    flex-direction: column;
    min-height: 0;
  }
</style>
