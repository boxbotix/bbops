<script lang="ts">
  import {
    UXS_DOMAINS,
    formatShellBanner,
    type UxsDomain,
  } from "$lib/bootstrap";

  let selectedDomain = $state<UxsDomain>("Quadcopter");
  const banner = $derived(formatShellBanner("v0.1.0-dev"));
</script>

<main>
  <h1>{banner}</h1>
  <p>Active domain filter: <strong>{selectedDomain}</strong></p>
  <div role="group" aria-label="UXS vehicle domain filter">
    {#each UXS_DOMAINS as domain (domain)}
      <button
        type="button"
        aria-pressed={selectedDomain === domain}
        onclick={() => {
          selectedDomain = domain;
        }}
      >
        {domain}
      </button>
    {/each}
  </div>
</main>
