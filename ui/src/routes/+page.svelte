<!--
  SPDX-License-Identifier: MIT
  Copyright (c) 2026 boxbotix
-->
<script lang="ts">
  import { onMount } from "svelte";
  import { TelemetryTraceBook } from "$lib/bench/ring-buffer";
  import WorkspaceShell from "$lib/components/WorkspaceShell.svelte";
  import { createGraphSearchClient } from "$lib/cortex/search-client";
  import { createGraphQLClient } from "$lib/graphql";
  import { selectFilmstripItems } from "$lib/hud/view-model";
  import { createSkgStore } from "$lib/stores/skg.svelte";
  import { createTelemetryStore } from "$lib/stores/telemetry.svelte";
  import { createViewportStore } from "$lib/stores/viewport.svelte";
  import { handleViewportHotkey } from "$lib/viewport/hotkeys";

  const telemetryStore = createTelemetryStore();
  const skgStore = createSkgStore();
  const viewportStore = createViewportStore({ skgStore });
  const graphqlClient = createGraphQLClient();
  const searchClient = createGraphSearchClient();
  const traceBook = new TelemetryTraceBook();

  const filmstripEntityIds = $derived(
    selectFilmstripItems(skgStore).map((item) => item.entityId),
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
        filmstripEntityIds,
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

<WorkspaceShell
  {telemetryStore}
  {skgStore}
  {viewportStore}
  {graphqlClient}
  {searchClient}
  {traceBook}
  onSyncHash={syncWindowHash}
/>
