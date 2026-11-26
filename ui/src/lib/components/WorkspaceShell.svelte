<!--
  SPDX-License-Identifier: MIT
  Copyright (c) 2026 boxbotix
-->
<script lang="ts">
  import { onMount } from "svelte";
  import {
    TelemetryTraceBook,
    lookupMeasuredSysStatus,
  } from "$lib/bench/ring-buffer";
  import {
    type DiagnosticsDraftStore,
    type TuningDiffEntry,
    createDiagnosticsDraftStore,
  } from "$lib/bench/tuning-rack";
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
  import {
    type AnalysisSessionStore,
    createAnalysisSessionStore,
  } from "$lib/cortex/session.svelte";
  import { type GraphQLClient, createGraphQLClient } from "$lib/graphql";
  import {
    FLEET_FRESHNESS_TICK_MS,
    type FilmstripItemView,
    type FleetFreshnessContext,
    selectFilmstripItems,
    summarizeFleetHealth,
    summarizeStreamReadiness,
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
    diagnosticsDraftStore?: DiagnosticsDraftStore;
    analysisSession?: AnalysisSessionStore;
    filmstripItems?: readonly FilmstripItemView[];
    now?: () => number;
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
    graphqlClient = createGraphQLClient(),
    searchClient = createGraphSearchClient(),
    traceBook = new TelemetryTraceBook(),
    diagnosticsDraftStore = createDiagnosticsDraftStore(),
    analysisSession,
    filmstripItems,
    now = () => Date.now(),
    onSyncHash,
    onApplyTuning,
  }: Props = $props();

  let ownedAnalysisSession: AnalysisSessionStore | null = null;
  function getShellAnalysisSession(): AnalysisSessionStore {
    if (!ownedAnalysisSession) {
      ownedAnalysisSession = createAnalysisSessionStore({
        skgStore,
        viewportStore,
        graphqlClient,
        searchClient,
      });
    }
    return ownedAnalysisSession;
  }

  const resolvedAnalysisSession = $derived.by(
    () => analysisSession ?? getShellAnalysisSession(),
  );

  let freshnessTick = $state(0);
  const clockNowMs = $derived.by(() => {
    void freshnessTick;
    return now();
  });

  onMount(() => {
    const intervalId = setInterval(() => {
      freshnessTick += 1;
    }, FLEET_FRESHNESS_TICK_MS);
    return () => {
      clearInterval(intervalId);
      if (!analysisSession && ownedAnalysisSession) {
        ownedAnalysisSession.dispose();
        ownedAnalysisSession = null;
      }
    };
  });

  $effect(() => {
    for (const vehicle of skgStore.vehicles) {
      if (!vehicle.poisoned) {
        diagnosticsDraftStore.reconcileVehicleDomain(
          vehicle.id,
          vehicle.domain,
        );
      }
    }
  });

  const banner = $derived(formatShellBanner("v0.1.0-dev"));
  const freshnessContext = $derived<FleetFreshnessContext>({
    telemetryStatus: telemetryStore.status,
    getTelemetryLink: (sysid) => telemetryStore.getLinkState(sysid),
    skgStatus: skgStore.status,
    skgSynced: skgStore.synced,
    skgLastSyncedRevision: skgStore.lastSyncedRevision,
    nowMs: clockNowMs,
  });
  const streamPills = $derived(
    summarizeStreamReadiness({
      telemetryStatus: telemetryStore.status,
      telemetryVehicleCount: telemetryStore.vehicleCount,
      skgStatus: skgStore.status,
      skgSynced: skgStore.synced,
      skgLastSyncedRevision: skgStore.lastSyncedRevision,
      skgEntityCount: skgStore.entities.size,
    }),
  );
  const fleetHealth = $derived(
    summarizeFleetHealth(skgStore.entities.values(), freshnessContext),
  );
  const resolvedFilmstripItems = $derived(
    filmstripItems ?? selectFilmstripItems(skgStore, freshnessContext),
  );

  $effect(() => {
    return traceBook.attach(telemetryStore, (sysid) =>
      lookupMeasuredSysStatus(skgStore, sysid),
    );
  });

  let shellEl: HTMLElement | undefined = $state();
  let stageHostEl: HTMLDivElement | undefined = $state();
  let lightsOutBtnEl: HTMLButtonElement | undefined = $state();
  let lastWorkspace: WorkspaceMode | null = null;
  let lastLightsOut: boolean | null = null;
  let directNavSelection = false;
  let hadShellFocusBeforeUpdate = false;

  const WORKSPACE_TABS: ReadonlyArray<{
    mode: WorkspaceMode;
    label: string;
    keycap: string;
  }> = [
    { mode: "cop", label: "Fleet", keycap: "G" },
    { mode: "pilot", label: "Vehicle", keycap: "D" },
    { mode: "bench", label: "Diagnostics", keycap: "B" },
    { mode: "cortex", label: "Analysis", keycap: "K" },
  ];

  function selectWorkspace(mode: WorkspaceMode): void {
    if (mode !== viewportStore.workspace) {
      directNavSelection = true;
    }
    viewportStore.setWorkspace(mode);
    onSyncHash?.(viewportStore.toUrlHash());
  }

  function toggleLightsOut(): void {
    viewportStore.setLightsOut(!viewportStore.lightsOut);
    onSyncHash?.(viewportStore.toUrlHash());
  }

  function toggleCharacterShortcuts(): void {
    viewportStore.setCharacterShortcutsEnabled(
      !viewportStore.characterShortcutsEnabled,
    );
  }

  $effect.pre(() => {
    void viewportStore.workspace;
    void viewportStore.lightsOut;
    const activeEl =
      typeof document !== "undefined" ? document.activeElement : null;
    hadShellFocusBeforeUpdate = Boolean(
      activeEl &&
      activeEl !== document.body &&
      activeEl !== document.documentElement &&
      shellEl?.contains(activeEl),
    );
  });

  $effect(() => {
    const currentWorkspace = viewportStore.workspace;
    const currentLightsOut = viewportStore.lightsOut;
    if (lastWorkspace === null || lastLightsOut === null) {
      lastWorkspace = currentWorkspace;
      lastLightsOut = currentLightsOut;
      return;
    }

    const workspaceChanged = currentWorkspace !== lastWorkspace;
    const lightsOutChanged = currentLightsOut !== lastLightsOut;
    lastWorkspace = currentWorkspace;
    lastLightsOut = currentLightsOut;

    if (workspaceChanged) {
      const preserveNavButton = directNavSelection;
      directNavSelection = false;
      if (!preserveNavButton) {
        const stageEl = stageHostEl?.querySelector<HTMLElement>(
          "[data-workspace-stage]",
        );
        stageEl?.focus({ preventScroll: true });
        return;
      }
    }

    if (lightsOutChanged && hadShellFocusBeforeUpdate) {
      const activeEl =
        typeof document !== "undefined" ? document.activeElement : null;
      if (
        !activeEl ||
        activeEl === document.body ||
        activeEl === document.documentElement ||
        !activeEl.isConnected
      ) {
        lightsOutBtnEl?.focus({ preventScroll: true });
      }
    }
  });

  function jumpToFirstFault(): void {
    if (!fleetHealth.firstFaultEntityId) {
      return;
    }
    viewportStore.selectEntity(fleetHealth.firstFaultEntityId);
    onSyncHash?.(viewportStore.toUrlHash());
  }
</script>

<main
  bind:this={shellEl}
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
        <span
          class="stream-pill"
          data-testid="stream-pill-telemetry"
          data-stream="ws1"
          data-status={streamPills.telemetry.status}
          data-tone={streamPills.telemetry.tone}
          title={streamPills.telemetry.detailLabel}
        >
          {streamPills.telemetry.label}:
          <strong>{streamPills.telemetry.stateLabel}</strong>
        </span>
        <span
          class="stream-pill"
          data-testid="stream-pill-fleet"
          data-stream="ws2"
          data-status={streamPills.fleetData.status}
          data-tone={streamPills.fleetData.tone}
          title={streamPills.fleetData.detailLabel}
        >
          {streamPills.fleetData.label}:
          <strong>{streamPills.fleetData.stateLabel}</strong>
        </span>
      </div>

      {#if fleetHealth.state === "fault"}
        <button
          type="button"
          class="fleet-fault-btn {fleetHealth.errorCount > 0
            ? 'severity-error'
            : 'severity-warning'}"
          data-testid="fleet-fault-badge"
          data-fleet-state="fault"
          data-stale-fault={fleetHealth.hasStaleFaultContext ? "true" : "false"}
          onclick={jumpToFirstFault}
        >
          {fleetHealth.badgeLabel}
        </button>
      {:else if fleetHealth.state === "nominal"}
        <span
          class="fleet-nominal-pill"
          data-testid="fleet-nominal-badge"
          data-fleet-state="nominal"
        >
          {fleetHealth.badgeLabel}
        </span>
      {:else}
        <span
          class="fleet-status-pill state-{fleetHealth.state}"
          data-testid="fleet-health-badge"
          data-fleet-state={fleetHealth.state}
        >
          {fleetHealth.badgeLabel}
        </span>
      {/if}
    </div>

    <div class="topbar-authority-slot">
      <AuthorityBanner
        entity={viewportStore.selectedEntity}
        {freshnessContext}
        compact={true}
      />
    </div>

    <nav class="workspace-switcher" aria-label="Task workspaces">
      {#each WORKSPACE_TABS as tab (tab.mode)}
        {@const active = viewportStore.workspace === tab.mode}
        <button
          type="button"
          class="mode-btn"
          class:active
          data-testid="workspace-tab-{tab.mode}"
          aria-current={active ? "page" : undefined}
          aria-keyshortcuts={viewportStore.characterShortcutsEnabled
            ? tab.keycap
            : undefined}
          onclick={() => selectWorkspace(tab.mode)}
        >
          <span>{tab.label}</span>
          {#if viewportStore.characterShortcutsEnabled}
            <kbd>{tab.keycap}</kbd>
          {/if}
        </button>
      {/each}

      <button
        bind:this={lightsOutBtnEl}
        type="button"
        class="chrome-btn"
        class:active={viewportStore.lightsOut}
        data-testid="lights-out-toggle"
        aria-pressed={viewportStore.lightsOut}
        aria-keyshortcuts={viewportStore.characterShortcutsEnabled
          ? "L"
          : undefined}
        title={viewportStore.characterShortcutsEnabled
          ? "Toggle Lights-Out display mode (L)"
          : "Toggle Lights-Out display mode"}
        onclick={toggleLightsOut}
      >
        <span>Lights-Out</span>
        {#if viewportStore.characterShortcutsEnabled}
          <kbd>L</kbd>
        {/if}
      </button>

      <button
        type="button"
        class="chrome-btn"
        class:active={viewportStore.characterShortcutsEnabled}
        data-testid="character-shortcuts-toggle"
        aria-pressed={viewportStore.characterShortcutsEnabled}
        title={viewportStore.characterShortcutsEnabled
          ? "Single-character keyboard shortcuts active (click to disable)"
          : "Single-character keyboard shortcuts disabled (Escape camera untether stays active)"}
        onclick={toggleCharacterShortcuts}
      >
        <span>
          {viewportStore.characterShortcutsEnabled
            ? "Shortcuts: On"
            : "Shortcuts: Off"}
        </span>
      </button>
    </nav>
  </header>

  <div class="workspace-stage-host" bind:this={stageHostEl}>
    {#if viewportStore.workspace === "cop"}
      <CopWorkspace
        {telemetryStore}
        {skgStore}
        {viewportStore}
        {freshnessContext}
        {now}
        {onSyncHash}
      />
    {:else if viewportStore.workspace === "pilot"}
      <PilotWorkspace
        {telemetryStore}
        {skgStore}
        {viewportStore}
        {freshnessContext}
        {now}
        {onSyncHash}
      />
    {:else if viewportStore.workspace === "bench"}
      <BenchWorkspace
        {telemetryStore}
        {skgStore}
        {viewportStore}
        {traceBook}
        draftStore={diagnosticsDraftStore}
        {onSyncHash}
        {onApplyTuning}
      />
    {:else}
      <CortexWorkspace
        {skgStore}
        {viewportStore}
        {graphqlClient}
        {searchClient}
        analysisSession={resolvedAnalysisSession}
        {onSyncHash}
      />
    {/if}
  </div>

  {#if !viewportStore.lightsOut}
    <FleetFilmstrip
      items={resolvedFilmstripItems}
      {viewportStore}
      {onSyncHash}
    />
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
  .fleet-nominal-pill,
  .fleet-status-pill {
    padding: 2px 8px;
    border-radius: 999px;
    background: var(--ui-surface-elevated, #172033);
    border: 1px solid var(--ui-border-strong, #334155);
    color: var(--ui-text-secondary, #94a3b8);
    font-family: ui-monospace, SFMono-Regular, Menlo, monospace;
    font-size: 11px;
  }

  .stream-pill[data-tone="connected"] {
    border-color: var(--ui-border-interactive, #3b82f6);
    color: var(--ui-text-primary, #e2e8f0);
  }

  .stream-pill[data-tone="degraded"] {
    border-color: var(--status-warning, #f59e0b);
    color: var(--status-warning, #f59e0b);
  }

  .fleet-nominal-pill {
    border-color: rgba(34, 197, 94, 0.45);
    color: var(--ui-text-primary, #e2e8f0);
  }

  .fleet-status-pill.state-stale,
  .fleet-status-pill.state-unavailable {
    color: var(--status-warning, #f59e0b);
    background: var(--status-warning-bg, rgba(245, 158, 11, 0.14));
    border-color: var(--status-warning, #f59e0b);
    font-weight: 600;
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
