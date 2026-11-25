<!--
  SPDX-License-Identifier: MIT
  Copyright (c) 2026 boxbotix
-->
<script lang="ts">
  import type { FilmstripItemView } from "$lib/hud/view-model";
  import type { ViewportStore } from "$lib/stores/viewport.svelte";

  interface Props {
    items: readonly FilmstripItemView[];
    viewportStore: ViewportStore;
    onSyncHash?: (hash: string) => void;
  }

  let { items, viewportStore, onSyncHash }: Props = $props();

  function handleSelect(entityId: string): void {
    viewportStore.selectEntity(entityId);
    onSyncHash?.(viewportStore.toUrlHash());
  }
</script>

<footer
  class="fleet-filmstrip"
  data-testid="fleet-filmstrip"
  aria-label="Fleet and COP contact filmstrip"
>
  <span class="filmstrip-label">FLEET &amp; CONTACTS ({items.length})</span>

  {#if items.length === 0}
    <span class="filmstrip-empty" data-testid="filmstrip-empty">
      Awaiting UXS vehicles or external COP contacts on ENTITY_STATES…
    </span>
  {:else}
    <div class="filmstrip-cards">
      {#each items as item (item.entityId)}
        {@const selected = viewportStore.selectedEntityId === item.entityId}
        <button
          type="button"
          class="filmstrip-card"
          class:selected
          class:stale={Boolean(item.stale)}
          data-testid="filmstrip-item"
          data-entity-id={item.entityId}
          data-entity-kind={item.kind}
          data-stale={item.stale ? "true" : "false"}
          aria-pressed={selected}
          aria-keyshortcuts={item.shortcutKey &&
          viewportStore.characterShortcutsEnabled
            ? item.shortcutKey
            : undefined}
          onclick={() => handleSelect(item.entityId)}
        >
          {#if item.shortcutKey && viewportStore.characterShortcutsEnabled}
            <kbd class="shortcut-key">{item.shortcutKey}</kbd>
          {/if}

          {#if item.kind === "uxs-vehicle"}
            <span
              class="status-dot vehicle-dot severity-{item.safetySeverity ??
                'nominal'}"
              data-testid="filmstrip-vehicle-dot"
            ></span>
          {:else}
            <span
              class="status-dot contact-dot"
              data-testid="filmstrip-contact-dot"
            ></span>
          {/if}

          <span class="card-copy">
            <strong class="card-primary">{item.primaryLabel}</strong>
            <span class="card-secondary">{item.secondaryLabel}</span>
          </span>
        </button>
      {/each}
    </div>
  {/if}
</footer>

<style>
  .fleet-filmstrip {
    display: flex;
    align-items: center;
    gap: 10px;
    padding: 6px 10px;
    background: var(--ui-surface-secondary, #101726);
    border: 1px solid var(--ui-border-subtle, #1e293b);
    border-radius: 6px;
    font-family: ui-monospace, SFMono-Regular, Menlo, monospace;
    font-size: 11px;
    overflow-x: auto;
  }

  .filmstrip-label {
    flex-shrink: 0;
    font-size: 10px;
    font-weight: 700;
    letter-spacing: 0.05em;
    color: var(--ui-text-muted, #64748b);
  }

  .filmstrip-empty {
    color: var(--ui-text-muted, #64748b);
  }

  .filmstrip-cards {
    display: flex;
    align-items: center;
    gap: 6px;
    min-width: 0;
  }

  .filmstrip-card {
    display: inline-flex;
    align-items: center;
    gap: 6px;
    padding: 4px 8px;
    border-radius: 5px;
    border: 1px solid var(--ui-border-strong, #334155);
    background: var(--ui-surface-elevated, #172033);
    color: var(--ui-text-primary, #e2e8f0);
    font: inherit;
    text-align: left;
    cursor: pointer;
    flex-shrink: 0;
  }

  .filmstrip-card.stale {
    border-style: dashed;
  }

  .filmstrip-card.selected {
    border-color: var(--ui-border-interactive, #3b82f6);
    box-shadow: 0 0 0 1px rgba(59, 130, 246, 0.45);
  }

  .shortcut-key {
    padding: 1px 4px;
    border-radius: 3px;
    border: 1px solid var(--ui-border-strong, #334155);
    background: rgba(15, 23, 42, 0.7);
    color: var(--ui-text-secondary, #94a3b8);
    font-size: 10px;
  }

  .status-dot {
    width: 8px;
    height: 8px;
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

  .card-copy {
    display: flex;
    flex-direction: column;
    line-height: 1.2;
  }

  .card-primary {
    font-size: 11px;
    color: var(--ui-text-primary, #e2e8f0);
  }

  .card-secondary {
    font-size: 10px;
    color: var(--ui-text-secondary, #94a3b8);
  }
</style>
