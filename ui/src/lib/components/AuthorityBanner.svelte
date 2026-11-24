<!--
  SPDX-License-Identifier: MIT
  Copyright (c) 2026 boxbotix
-->
<script lang="ts">
  import { selectBannerPresentation } from "$lib/hud/view-model";
  import type { CopEntityState } from "$lib/stores/skg.svelte";

  interface Props {
    entity: CopEntityState | null;
  }

  let { entity }: Props = $props();

  const presentation = $derived(selectBannerPresentation(entity));
</script>

{#if presentation?.kind === "uxs-vehicle"}
  <section
    class="banner vehicle-banner"
    data-testid="vehicle-authority-banner"
    aria-label="UXS vehicle authority and safety health"
  >
    <div class="banner-primary">
      <span class="entity-tag">
        UXS #{presentation.sysid} · {presentation.domain}
      </span>

      <span
        class="safety-badge severity-{presentation.authorityBadge.severity}"
        data-testid="authority-badge"
        data-authority={presentation.authorityBadge.authority}
      >
        {presentation.authorityBadge.label}
      </span>

      <span
        class="safety-badge severity-{presentation.armedBadge.severity}"
        data-testid="armed-badge"
      >
        {presentation.armedBadge.label}
      </span>

      <span class="link-pill" data-testid="banner-link-tier">
        {presentation.linkBadge.label}
      </span>

      {#if presentation.poisoned}
        <span class="safety-badge severity-error" data-testid="poisoned-badge">
          POISONED{presentation.poisonError
            ? `: ${presentation.poisonError}`
            : ""}
        </span>
      {/if}
    </div>

    <div class="health-strip" data-testid="vehicle-health-strip">
      {#each presentation.healthStrip as chip (chip.key)}
        <span
          class="health-chip severity-{chip.severity}"
          data-chip-key={chip.key}
        >
          <span class="chip-label">{chip.label}:</span>
          <strong>{chip.value}</strong>
        </span>
      {/each}
    </div>
  </section>
{:else if presentation?.kind === "external-contact"}
  <section
    class="banner contact-banner"
    data-testid="external-contact-banner"
    aria-label="External COP contact provenance"
  >
    <div class="banner-primary">
      <span class="contact-tag">EXTERNAL CONTACT</span>
      <strong class="contact-title">{presentation.title}</strong>
      <span class="contact-pill">type: {presentation.contactType}</span>
      <span class="contact-pill">feed: {presentation.feedStatus}</span>
    </div>

    <div class="contact-meta">
      <span>source: <strong>{presentation.source}</strong></span>
      <span>observed: <strong>{presentation.observedAt}</strong></span>
      <span>received: <strong>{presentation.receivedAt}</strong></span>
      <span>expires: <strong>{presentation.expiresAt}</strong></span>
      <span>uncertainty: <strong>{presentation.uncertaintyLabel}</strong></span>
    </div>
  </section>
{:else}
  <section
    class="banner standby-banner"
    data-testid="authority-banner-standby"
    aria-label="Entity status banner"
  >
    <span>No UXS vehicle or external COP contact selected.</span>
  </section>
{/if}

<style>
  .banner {
    display: flex;
    flex-direction: column;
    gap: 6px;
    padding: 8px 12px;
    background: var(--ui-surface-secondary, #101726);
    border: 1px solid var(--ui-border-subtle, #1e293b);
    border-radius: 6px;
    font-family: ui-monospace, SFMono-Regular, Menlo, monospace;
    font-size: 12px;
  }

  .banner-primary,
  .health-strip,
  .contact-meta {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    gap: 8px;
  }

  .entity-tag {
    font-weight: 700;
    color: var(--ui-text-primary, #e2e8f0);
  }

  .safety-badge {
    padding: 2px 8px;
    border-radius: 4px;
    font-weight: 700;
    border: 1px solid var(--ui-border-strong, #334155);
  }

  .severity-error {
    color: var(--status-error, #ef4444);
    background: var(--status-error-bg, rgba(239, 68, 68, 0.16));
    border-color: var(--status-error, #ef4444);
  }

  .severity-warning {
    color: var(--status-warning, #f59e0b);
    background: var(--status-warning-bg, rgba(245, 158, 11, 0.16));
    border-color: var(--status-warning, #f59e0b);
  }

  .severity-success {
    color: var(--status-success, #22c55e);
    background: var(--status-success-bg, rgba(34, 197, 94, 0.16));
    border-color: var(--status-success, #22c55e);
  }

  .severity-nominal {
    color: var(--ui-text-secondary, #94a3b8);
    background: rgba(51, 65, 85, 0.25);
    border-color: var(--ui-border-strong, #334155);
  }

  .link-pill,
  .contact-pill {
    padding: 2px 8px;
    border-radius: 999px;
    color: var(--skg-node-contact, #22d3ee);
    border: 1px solid rgba(34, 211, 238, 0.35);
    font-size: 11px;
  }

  .health-chip {
    display: inline-flex;
    gap: 4px;
    padding: 1px 6px;
    border-radius: 4px;
    font-size: 11px;
    border: 1px solid transparent;
  }

  .chip-label {
    opacity: 0.8;
  }

  .contact-tag {
    padding: 2px 7px;
    border-radius: 4px;
    background: rgba(34, 211, 238, 0.14);
    color: var(--skg-node-contact, #22d3ee);
    border: 1px solid rgba(34, 211, 238, 0.4);
    font-weight: 700;
  }

  .contact-title {
    color: var(--ui-text-primary, #e2e8f0);
  }

  .contact-meta {
    color: var(--ui-text-secondary, #94a3b8);
    font-size: 11px;
  }

  .standby-banner {
    color: var(--ui-text-muted, #64748b);
  }
</style>
