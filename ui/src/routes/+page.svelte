<!--
  SPDX-License-Identifier: MIT
  Copyright (c) 2026 boxbotix
-->
<script lang="ts">
  import { onMount } from "svelte";
  import { formatShellBanner } from "$lib/bootstrap";
  import AuthorityBanner from "$lib/components/AuthorityBanner.svelte";
  import SkgPane from "$lib/components/SkgPane.svelte";
  import TacticalCanvas from "$lib/components/TacticalCanvas.svelte";
  import VehicleHud from "$lib/components/VehicleHud.svelte";
  import { createGraphQLClient } from "$lib/graphql";
  import { createSkgStore, isVehicleEntity } from "$lib/stores/skg.svelte";
  import { createTelemetryStore } from "$lib/stores/telemetry.svelte";
  import { createViewportStore } from "$lib/stores/viewport.svelte";
  import { handleViewportHotkey } from "$lib/viewport/hotkeys";

  const telemetryStore = createTelemetryStore();
  const skgStore = createSkgStore();
  const viewportStore = createViewportStore({ skgStore });
  const graphqlClient = createGraphQLClient();

  const banner = $derived(formatShellBanner("v0.1.0-dev"));
  const selectedVehicle = $derived(
    isVehicleEntity(viewportStore.selectedEntity)
      ? viewportStore.selectedEntity
      : null,
  );

  function syncWindowHash(nextHash: string): void {
    if (typeof window === "undefined") {
      return;
    }
    const currentHash = window.location.hash;
    if (currentHash === nextHash) {
      return;
    }
    const nextUrl = `${window.location.pathname}${window.location.search}${nextHash}`;
    window.history.replaceState(null, "", nextUrl);
  }

  onMount(() => {
    viewportStore.applyUrlHash(window.location.hash);
    telemetryStore.connect();
    skgStore.connect();

    const onKeyDown = (ev: KeyboardEvent): void => {
      handleViewportHotkey(
        {
          key: ev.key,
          ctrlKey: ev.ctrlKey,
          metaKey: ev.metaKey,
          altKey: ev.altKey,
          shiftKey: ev.shiftKey,
          repeat: ev.repeat,
          defaultPrevented: ev.defaultPrevented,
          target: ev.target as HTMLElement | null,
          preventDefault: () => ev.preventDefault(),
        },
        viewportStore,
        syncWindowHash,
      );
    };

    const onHashChange = (): void => {
      viewportStore.applyUrlHash(window.location.hash);
    };

    window.addEventListener("keydown", onKeyDown);
    window.addEventListener("hashchange", onHashChange);

    return () => {
      window.removeEventListener("keydown", onKeyDown);
      window.removeEventListener("hashchange", onHashChange);
      telemetryStore.disconnect();
      skgStore.disconnect();
    };
  });
</script>

<main class="cop-workspace">
  <header class="workspace-topbar">
    <h1 class="workspace-title">{banner}</h1>
    <div class="stream-pills" aria-label="Stream connection status">
      <span class="stream-pill" data-status={telemetryStore.status}>
        WS1 telemetry: <strong>{telemetryStore.status}</strong>
        ({telemetryStore.vehicleCount})
      </span>
      <span class="stream-pill" data-status={skgStore.status}>
        WS2 SKG: <strong>{skgStore.status}</strong>
        ({skgStore.entities.size}{skgStore.synced ? " · synced" : ""})
      </span>
    </div>
  </header>

  <AuthorityBanner entity={viewportStore.selectedEntity} />

  <div class="workspace-grid">
    <div class="canvas-column">
      <TacticalCanvas
        {telemetryStore}
        {skgStore}
        {viewportStore}
        onSyncHash={syncWindowHash}
      />
    </div>

    <div class="inspector-column">
      <VehicleHud {telemetryStore} vehicle={selectedVehicle} />
      <SkgPane
        {skgStore}
        {telemetryStore}
        {viewportStore}
        {graphqlClient}
        onSyncHash={syncWindowHash}
      />
    </div>
  </div>
</main>

<style>
  .cop-workspace {
    display: flex;
    flex-direction: column;
    gap: 8px;
    padding: 10px;
    min-height: 100vh;
    background: var(--ui-surface-primary, #090d16);
    color: var(--ui-text-primary, #e2e8f0);
  }

  .workspace-topbar {
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

  .workspace-title {
    margin: 0;
    font-size: 14px;
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

  .stream-pill {
    padding: 2px 8px;
    border-radius: 999px;
    background: var(--ui-surface-elevated, #172033);
    border: 1px solid var(--ui-border-strong, #334155);
    color: var(--ui-text-secondary, #94a3b8);
  }

  .stream-pill[data-status="connected"] {
    border-color: var(--ui-border-interactive, #3b82f6);
    color: var(--ui-text-primary, #e2e8f0);
  }

  .workspace-grid {
    flex: 1;
    display: grid;
    grid-template-columns: minmax(0, 1fr) 360px;
    gap: 8px;
    min-height: 540px;
  }

  .canvas-column {
    display: flex;
    flex-direction: column;
    min-height: 460px;
  }

  .inspector-column {
    display: flex;
    flex-direction: column;
    gap: 8px;
    min-width: 0;
  }

  @media (max-width: 960px) {
    .workspace-grid {
      grid-template-columns: 1fr;
    }
  }
</style>
